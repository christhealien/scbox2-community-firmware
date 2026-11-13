package main

import (
	"math"
	"strings"
	"testing"
	"time"
)

// Round 7 auto-trim scenarios. Real boards (tf1 export): ~0.4 errors/chip/hour at stock, most chips 0 in 25 min.

func cwLogText() string {
	s := ""
	for _, l := range chipWatchView()["log"].([]map[string]any) {
		s += l["msg"].(string) + "\n"
	}
	return s
}

func cwSlowed(s *cwSim) int {
	n := 0
	for _, v := range s.trims() {
		if v < 0 {
			n++
		}
	}
	return n
}

// A chip missing from one icinfo reading (Perf 0 for a moment, or its board reported not running) comes back
// with its whole lifetime counter: that must not count as errors made in the window.
func TestR7ATGapDoesNotCountLifetime(t *testing.T) {
	s := newCwSim(t, func(chip int, trim float64) float64 { return 0.4 })
	for i := 0; i < 46; i++ {
		s.errs[ckey{2, i}] = 150 // a week of mining
	}
	on := true
	chipWatchSet(&on, false)
	s.run(30 * time.Minute)
	real := cwReadErrors
	cwReadErrors = func() (map[ckey]int, error) {
		m, err := real()
		delete(m, ckey{2, 19}) // hook chip 20 missing from one reading
		return m, err
	}
	s.run(time.Minute)
	cwReadErrors = real
	s.run(2 * time.Hour)
	if tr := s.trims(); len(tr) != 0 {
		t.Fatalf("a chip that missed one reading was trimmed for its lifetime counter: %v\n%s", tr, cwLogText())
	}
}

// After a trim on a board, a NEW weak chip appears. Trims cost the other chips nothing here, so the budget must
// not stop it being trimmed (the weak chip's own errors are not "the untrimmed chips' errors rising").
func TestR7ATBudgetNotTrippedByNewWeakChip(t *testing.T) {
	var s *cwSim
	s = newCwSim(t, func(chip int, trim float64) float64 {
		if trim < 0 {
			return 0.4
		}
		if chip == 5 {
			return 10
		}
		if chip == 17 && s.now.Sub(time.Unix(1_790_000_000, 0)) > 8*time.Hour {
			return 30
		}
		return 0.4
	})
	on := true
	chipWatchSet(&on, false)
	s.run(16 * time.Hour)
	tr := s.trims()
	lg := cwLogText()
	if strings.Contains(lg, "no new trims on this board") || tr[17] >= 0 {
		t.Fatalf("a new weak chip tripped the budget (trims %v):\n%s", tr, lg)
	}
}

// Every slowed chip costs the rest of the board 15 % more errors (the measured "costs every other chip a
// little"). Six weak chips on six pairs. The budget should stop before the untrimmed chips' errors have doubled.
func TestR7ATBudgetCatchesCumulativeCost(t *testing.T) {
	var s *cwSim
	weak := map[int]bool{5: true, 9: true, 13: true, 17: true, 21: true, 25: true}
	s = newCwSim(t, func(chip int, trim float64) float64 {
		if weak[chip] && trim == 0 {
			return 12
		}
		return 0.4 * math.Pow(1.15, float64(cwSlowed(s)))
	})
	on := true
	chipWatchSet(&on, false)
	s.run(40 * time.Hour)
	n := cwSlowed(s)
	lg := cwLogText()
	if !strings.Contains(lg, "no new trims on this board") || n >= 6 {
		t.Fatalf("budget never fired: %d trims, rest of the board at %.2fx its errors\n%s", n, math.Pow(1.15, float64(n)), lg)
	}
	t.Logf("stopped at %d trims (rest of board at %.2fx)\n%s", n, math.Pow(1.15, float64(n)), lg)
}

// The pair rule's note is written once, not every window (the log keeps 40 lines).
func TestR7ATPairNoteNotSpammed(t *testing.T) {
	s := newCwSim(t, func(chip int, trim float64) float64 {
		if (chip == 11 || chip == 12) && trim == 0 {
			return 15
		}
		return 0.4
	})
	on := true
	chipWatchSet(&on, false)
	s.run(24 * time.Hour)
	if n := strings.Count(cwLogText(), "one trim per pair"); n > 2 {
		t.Fatalf("pair note logged %d times in a day:\n%s", n, cwLogText())
	}
}

// Budget fired; then a tuning run (auto-trim paused) moves the board clock. The budget starts over.
func TestR7ATBudgetResetByTunerClockChange(t *testing.T) {
	s := newCwSim(t, nil)
	s.rate = func(chip int, trim float64) float64 {
		if trim < 0 {
			return 1
		}
		if chip == 5 || chip == 17 || chip == 29 || chip == 41 {
			return 30
		}
		return 1.0 + 2*float64(cwSlowed(s))
	}
	on := true
	chipWatchSet(&on, false)
	for i := 0; i < 20*60; i++ {
		s.run(time.Minute)
		cwMu.Lock()
		has := false
		for _, v := range cwSt.Budget {
			has = has || v > 0
		}
		cwMu.Unlock()
		if has {
			break
		}
	}
	cwMu.Lock()
	fired := len(cwSt.Budget) > 0
	cwMu.Unlock()
	if !fired {
		t.Fatal("setup: the budget never fired")
	}
	cwBlocked = func() string { return "paused while the tuner runs" }
	s.run(30 * time.Minute)
	s.base = 6000
	s.run(30 * time.Minute)
	cwBlocked = func() string { return "" }
	s.run(2 * time.Minute)
	cwMu.Lock()
	defer cwMu.Unlock()
	for k, v := range cwSt.Budget {
		if v > s.now.Unix() {
			t.Fatalf("board %s still on its budget block after the clock changed during a tuner run", k)
		}
	}
}

// Undo auto-trims removes every trim: the budget (no new trims for a day) starts over too.
func TestR7ATUndoClearsBudget(t *testing.T) {
	s := newCwSim(t, nil)
	s.rate = func(chip int, trim float64) float64 {
		if trim < 0 {
			return 1
		}
		if chip == 5 || chip == 17 || chip == 29 || chip == 41 {
			return 30
		}
		return 1.0 + 2*float64(cwSlowed(s))
	}
	on := true
	chipWatchSet(&on, false)
	s.run(16 * time.Hour)
	if !strings.Contains(cwLogText(), "no new trims on this board") {
		t.Fatal("setup: the budget never fired")
	}
	chipWatchSet(nil, true)
	cwMu.Lock()
	defer cwMu.Unlock()
	for k, v := range cwSt.Budget {
		if v > s.now.Unix() {
			t.Fatalf("board %s still blocked after Undo auto-trims", k)
		}
	}
}

// A step is waiting to be judged when the owner raises the board clock. At the new clock the chip errs more
// whatever its trim, so comparing with the rate measured at the old clock undoes a trim that helps 4x and bars
// the chip for a day.
func TestR7ATClockChangeWhileJudging(t *testing.T) {
	var s *cwSim
	s = newCwSim(t, func(chip int, trim float64) float64 {
		if chip != 5 {
			return 0.4
		}
		r := 20.0
		if trim < 0 {
			r = 5
		}
		if s.base > 6250 {
			r *= 4
		}
		return r
	})
	on := true
	chipWatchSet(&on, false)
	s.run(2*time.Hour + time.Minute)
	if s.trims()[5] != -12.5 {
		t.Fatalf("setup: %v", s.trims())
	}
	s.run(30 * time.Minute)
	s.base = 6500
	s.run(2*time.Hour + 5*time.Minute)
	lg := cwLogText()
	if s.trims()[5] == 0 || strings.Contains(lg, "didn't lower") {
		t.Fatalf("a trim judged against the old clock's rate:\n%s", lg)
	}
}

// A chip set by hand to +25 becomes the board's worst: stepped back to 0 one window at a time, never below the
// board by that rule; its partner (weak) may still be slowed: a "+" partner isn't a slowed partner.
func TestR7ATPlusStepBackAndPartner(t *testing.T) {
	s := newCwSim(t, func(chip int, trim float64) float64 {
		switch {
		case chip == 7 && trim > 0:
			return 15
		case chip == 8 && trim == 0:
			return 15
		}
		return 0.4
	})
	cwSetTrim(2, 7, 25)
	on := true
	chipWatchSet(&on, false)
	s.run(12 * time.Hour)
	tr := s.trims()
	if tr[7] != 0 {
		t.Fatalf("+ chip not stepped back to the board clock: %v\n%s", tr, cwLogText())
	}
	if tr[8] != -12.5 {
		t.Fatalf("weak partner of a + chip not slowed: %v\n%s", tr, cwLogText())
	}
	if n := strings.Count(cwLogText(), "faster, is making"); n != 2 {
		t.Fatalf("want 2 step-backs, got %d:\n%s", n, cwLogText())
	}
}

// A + chip that goes bad on a board whose budget is spent is still stepped back (that gives the board voltage).
func TestR7ATPlusStepBackDuringBudget(t *testing.T) {
	s := newCwSim(t, nil)
	s.rate = func(chip int, trim float64) float64 {
		if chip == 7 {
			if trim > 0 && s.now.Sub(time.Unix(1_790_000_000, 0)) > 16*time.Hour+30*time.Minute {
				return 40
			}
			return 1
		}
		if trim < 0 {
			return 1
		}
		if chip == 5 || chip == 17 || chip == 29 || chip == 41 {
			return 30
		}
		return 1.0 + 2*float64(cwSlowed(s))
	}
	cwSetTrim(2, 7, 25)
	on := true
	chipWatchSet(&on, false)
	s.run(16 * time.Hour)
	if !strings.Contains(cwLogText(), "no new trims on this board") {
		t.Fatalf("setup: budget not fired\n%s", cwLogText())
	}
	s.run(6 * time.Hour)
	if tr := s.trims(); tr[7] >= 25 {
		t.Fatalf("+ chip not stepped back while the budget blocks new trims: %v\n%s", tr, cwLogText())
	}
}

// Every chip now and then makes a burst of errors (a glitch), otherwise all alike. Over a week, how many chips
// end up trimmed? (Each burst passes the "bad" test, the next window is clean, so the step is "kept".)
func TestR7ATBurstyBoard(t *testing.T) {
	s := newCwSim(t, func(chip int, trim float64) float64 { return 0.4 })
	on := true
	chipWatchSet(&on, false)
	for h := 0; h < 7*24; h++ {
		for i := 0; i < 46; i++ {
			if s.rng.Float64() < 0.01 { // 1 % per chip-hour: a burst of 15
				s.errs[ckey{2, i}] += 15
			}
		}
		s.run(time.Hour)
	}
	t.Logf("bursty but even board, after a week: %d chips trimmed %v", cwSlowed(s), s.trims())
}
