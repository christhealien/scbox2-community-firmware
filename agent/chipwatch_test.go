package main

import (
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// a simulated board: chain 2, 46 chips (icinfo numbers them 0..45, the hook 1..46). Each chip makes
// errors at rate(chip, trim) per hour; the watchdog sees the counters every minute.
type cwSim struct {
	t    *testing.T
	now  time.Time
	rng  *rand.Rand
	errs map[ckey]int
	rate func(hookChip int, trim float64) float64
	base int
}

func newCwSim(t *testing.T, rate func(int, float64) float64) *cwSim {
	*authFile = filepath.Join(t.TempDir(), "auth.json")
	s := &cwSim{t: t, now: time.Unix(1_790_000_000, 0), rng: rand.New(rand.NewSource(5)), errs: map[ckey]int{}, rate: rate, base: 6250}
	for i := 0; i < 46; i++ {
		s.errs[ckey{2, i}] = 0
	}
	cwMu.Lock()
	cwLoaded, cwSt, cwStatus = false, cwState{}, "off"
	cwResetWindow()
	cwMu.Unlock()
	cwNow = func() time.Time { return s.now }
	cwBlocked = func() string { return "" }
	cwApply = func() error { return nil }
	cwReadStats = func() (chipStatsFile, bool) {
		var f chipStatsFile
		f.Chains = append(f.Chains, struct {
			Chain   int   `json:"chain"`
			Chips   int   `json:"chips"`
			Base10  int   `json:"base10"`
			Clock10 []int `json:"clock10"`
			Nonces  []int `json:"nonces"`
		}{Chain: 2, Chips: 46, Base10: s.base})
		return f, true
	}
	cwReadErrors = func() (map[ckey]int, error) {
		out := map[ckey]int{}
		for k, v := range s.errs {
			out[k] = v
		}
		return out, nil
	}
	t.Cleanup(func() {
		cwNow = time.Now
		cwReadErrors = readChipErrors
	})
	return s
}

func (s *cwSim) trims() map[int]float64 {
	m := map[int]float64{}
	for _, t := range readChipTrims() {
		m[t.Chip] = t.MHz
	}
	return m
}

// run the board for d, a reading a minute
func (s *cwSim) run(d time.Duration) {
	for el := time.Duration(0); el < d; el += time.Minute {
		tr := s.trims()
		for i := 0; i < 46; i++ {
			lam := s.rate(i+1, tr[i+1]) / 60
			// Poisson draw for one minute
			n, l, p := 0, mathExp(-lam), 1.0
			for {
				p *= s.rng.Float64()
				if p < l {
					break
				}
				n++
			}
			s.errs[ckey{2, i}] += n
		}
		s.now = s.now.Add(time.Minute)
		chipWatchTick()
	}
}

func mathExp(x float64) float64 { return math.Exp(x) }

func TestChipWatch(t *testing.T) {
	// chip 12 (icinfo 11) is bad and a trim fixes it; chip 21 (icinfo 20) is bad and trims don't help
	s := newCwSim(t, func(chip int, trim float64) float64 {
		switch {
		case chip == 12 && trim <= -12.5:
			return 1
		case chip == 12, chip == 21:
			return 15
		}
		return 1
	})
	on := true
	if _, err := chipWatchSet(&on, false); err != nil {
		t.Fatal(err)
	}
	s.run(2*time.Hour + time.Minute)
	tr := s.trims()
	if tr[12] != -12.5 && tr[21] != -12.5 {
		t.Fatalf("after the first window one bad chip should be at -12.5: %v", tr)
	}
	if len(tr) != 1 {
		t.Fatalf("one change per board per window: %v", tr)
	}
	s.run(10 * time.Hour)
	tr = s.trims()
	if tr[12] != -12.5 {
		t.Fatalf("chip 12's trim helped and should stay at -12.5: %v", tr)
	}
	if tr[21] != 0 {
		t.Fatalf("chip 21's trims didn't help and should be undone: %v", tr)
	}
	for c := range tr {
		if c != 12 {
			t.Fatalf("a good chip was trimmed: %v", tr)
		}
	}
	v := chipWatchView()
	log := ""
	for _, l := range v["log"].([]map[string]any) {
		log += l["msg"].(string) + "\n"
	}
	if !strings.Contains(log, "chip 12 at -12.5 MHz: its errors fell") || !strings.Contains(log, "chip 21: -12.5 MHz didn't lower") {
		t.Fatalf("log:\n%s", log)
	}
	// saved: a restart keeps it on with its trims
	b, _ := os.ReadFile(chipWatchPath())
	if !strings.Contains(string(b), `"on": true`) {
		t.Fatalf("state not saved: %s", b)
	}
	// undo removes only its own trims
	if err := cwSetTrim(2, 30, -25); err != nil { // one set by hand
		t.Fatal(err)
	}
	if _, err := chipWatchSet(nil, true); err != nil {
		t.Fatal(err)
	}
	tr = s.trims()
	if tr[12] != 0 || tr[30] != -25 {
		t.Fatalf("undo: %v", tr)
	}
}

func TestChipWatchQuietBoard(t *testing.T) {
	// all chips alike (noisy but even): nothing is trimmed, however long it runs
	s := newCwSim(t, func(chip int, trim float64) float64 { return 3 })
	on := true
	chipWatchSet(&on, false)
	s.run(24 * time.Hour)
	if tr := s.trims(); len(tr) != 0 {
		t.Fatalf("an even board got trims: %v", tr)
	}
}

func TestChipWatchManualAndClock(t *testing.T) {
	s := newCwSim(t, func(chip int, trim float64) float64 {
		if chip == 5 && trim > -25 {
			return 20
		}
		return 1
	})
	cwSetTrim(2, 5, -12.5) // set by hand: the watchdog must not touch it, even though the chip is bad
	on := true
	chipWatchSet(&on, false)
	s.run(6 * time.Hour)
	if tr := s.trims(); tr[5] != -12.5 || len(tr) != 1 {
		t.Fatalf("a chip trimmed by hand was changed: %v", tr)
	}
	// a board clock change restarts the window: nothing judged on a mixed window
	cwSetTrim(2, 5, 0)
	s.run(90 * time.Minute)
	s.base = 6000
	s.run(90 * time.Minute)
	if tr := s.trims(); len(tr) != 0 {
		for _, l := range chipWatchView()["log"].([]map[string]any) {
			t.Log(time.Unix(l["t"].(int64), 0).Sub(time.Unix(1_790_000_000, 0)), l["msg"])
		}
		t.Fatalf("judged across a clock change: %v", tr)
	}
	s.run(31 * time.Minute)
	if tr := s.trims(); tr[5] != -12.5 {
		t.Fatalf("after a full window at the new clock chip 5 should be trimmed: %v", tr)
	}
}

func TestErrToHook(t *testing.T) {
	var st chipStatsFile
	st.Chains = append(st.Chains, struct {
		Chain   int   `json:"chain"`
		Chips   int   `json:"chips"`
		Base10  int   `json:"base10"`
		Clock10 []int `json:"clock10"`
		Nonces  []int `json:"nonces"`
	}{Chain: 2, Chips: 46})
	errs := map[ckey]int{{2, 0}: 1, {2, 45}: 1}
	mi := minIndexes(errs)
	if c, ch, ok := errToHook(ckey{2, 0}, mi, st); !ok || c != 2 || ch != 1 {
		t.Fatal(c, ch, ok)
	}
	if _, ch, ok := errToHook(ckey{2, 45}, mi, st); !ok || ch != 46 {
		t.Fatal(ch, ok)
	}
	// 1-based icinfo, as on the real SC Lite: the same numbers, even when chip 1 isn't listed
	errs1 := map[ckey]int{{2, 3}: 1, {2, 46}: 1}
	if _, ch, ok := errToHook(ckey{2, 46}, minIndexes(errs1), st); !ok || ch != 46 {
		t.Fatal(ch, ok)
	}
	if _, ch, ok := errToHook(ckey{2, 3}, minIndexes(errs1), st); !ok || ch != 3 {
		t.Fatal(ch, ok)
	}
	// board slot 0 on the error side, chain 2 on the hook side, one board each
	if c, _, ok := errToHook(ckey{0, 3}, map[int]int{0: 0}, st); !ok || c != 2 {
		t.Fatal(c, ok)
	}
}

// The tuner's kept trims become auto-trim's: it may slow such a chip further, and Undo removes it.
func TestChipWatchAdoptsTunerTrims(t *testing.T) {
	s := newCwSim(t, func(chip int, trim float64) float64 {
		if chip == 9 && trim > -25 {
			return 20 // still bad at -12.5 (the tuner's trim), fine at -25
		}
		return 1
	})
	cwSetTrim(2, 9, -12.5)
	chipWatchAdopt([]chipTrim{{Chain: 2, Chip: 9, MHz: -12.5}}, "kept by the tuning run")
	if o := chipWatchView()["owned"].([]cwOwned); len(o) != 1 || o[0].Origin != "tuner" || !o[0].Judged {
		t.Fatalf("not adopted: %+v", o)
	}
	on := true
	chipWatchSet(&on, false)
	s.run(4*time.Hour + 2*time.Minute)
	if tr := s.trims(); tr[9] != -25 {
		t.Fatalf("an adopted chip that's still bad should go one step further: %v", tr)
	}
	if _, err := chipWatchSet(nil, true); err != nil {
		t.Fatal(err)
	}
	if tr := s.trims(); len(tr) != 0 {
		t.Fatalf("Undo should remove the adopted trim too: %v", tr)
	}
}

// The auto-trim's deepest trim: -50 MHz on the SC Lite (unchanged), -100 MHz on the SC Box II (its trim library
// takes -100 .. +50 MHz); the Chip trims card is told the same floor.
func TestChipWatchFloorPerModel(t *testing.T) {
	b10TempAuth(t)
	b10SetModel(t, profSCLite)
	if f := cwTrimFloor(); f != -50 {
		t.Fatalf("SC Lite auto-trim floor %v, want -50", f)
	}
	if f, _ := chipWatchView()["floor"].(float64); f != -50 {
		t.Fatalf("SC Lite Chip trims card floor %v, want -50", chipWatchView()["floor"])
	}
	b10SetModel(t, profSCBox2)
	if f := cwTrimFloor(); f != -100 || f < trimMin {
		t.Fatalf("SC Box II auto-trim floor %v, want -100 (and within the manual range %v)", f, trimMin)
	}
	if f, _ := chipWatchView()["floor"].(float64); f != -100 {
		t.Fatalf("SC Box II Chip trims card floor %v, want -100", chipWatchView()["floor"])
	}
}
