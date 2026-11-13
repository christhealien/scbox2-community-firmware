package main

// r7-tuner: how the tuner uses chip trims (pair rule, retest, trim check, undo at the end, hand trims).

import (
	"strings"
	"testing"
)

func r7cfg() tunerConfig {
	c := small()
	c.TopDown = false // the default walk
	return c
}

func r7trimTotal(f *fakeMiner) string { return sortedTrims(f.trims) }

// Pair rule across clocks: b2.c7 is trimmed at 600; at 625 its partner b2.c8 fails. The tuner must refuse to
// slow c8 and raise PV instead, and the run must still find 625 clean at more voltage.
func TestR7PairRefusedRaisesPV(t *testing.T) {
	e, f := newFM(t, r7cfg())
	f.rules = []fmRule{
		needRule(ckey{2, 7}, always(600, 625), 0.6, -12.5),
		needRule(ckey{2, 8}, map[int]int{625: 9000}, 0.6, -12.5), // a trim would fix it, more voltage too
	}
	runFM(t, e, f)
	dump(t, e)
	mustContain(t, e, "one trim per pair")
	if strings.Contains(strings.Join(f.trimCalls, " "), "2.8=") {
		t.Fatalf("partner b2.c8 was trimmed: %v", f.trimCalls)
	}
	if c := curveStr(e.curve); !strings.Contains(c, "625/9300") {
		t.Fatalf("625 should pass at PV 9300 (mV 9000): curve %s\n%s", c, tailLog(e, 60))
	}
}

// Pair rule inside one retest loop: c7 fails, gets slowed, then c8 fails: refused ("was just slowed"); c7's
// trim made for this test comes off and the walk raises PV.
func TestR7PairJustSlowedUndone(t *testing.T) {
	e, f := newFM(t, r7cfg())
	f.rules = []fmRule{
		needRule(ckey{2, 7}, map[int]int{600: 8900}, 0.6, -12.5),
		// c8 only fails once c7 is slowed (the partner rule's premise is the opposite, but the log must stay true)
		func(f *fakeMiner, k ckey) float64 {
			if k == (ckey{2, 8}) && f.plan.MHz == 600 && f.plan.MV < 8900 && f.trimOf(ckey{2, 7}) < 0 {
				return 0.6
			}
			return 0
		},
	}
	runFM(t, e, f)
	dump(t, e)
	mustContain(t, e, "one trim per pair", "the trims made for it are undone")
	if e.confirmed && r7trimTotal(f) != "" {
		t.Fatalf("trims left: %s", r7trimTotal(f))
	}
}

// A Stop during the retest after a trim: nothing is left behind, and the log doesn't claim the setting failed.
func TestR7StopDuringTrimRetest(t *testing.T) {
	e, f := newFM(t, r7cfg())
	f.rules = []fmRule{needRule(ckey{2, 7}, always(600), 0.6, -12.5)}
	f.stopWhen = func(f *fakeMiner) bool { return f.trims[[2]int{2, 7}] < 0 && f.sinceApply() > 5 }
	runFM(t, e, f)
	dump(t, e)
	if len(f.trims) != 0 {
		t.Fatalf("trims left after a stop: %v", f.trims)
	}
	if strings.Contains(logStr(e), "isn't clean even with chip trims") {
		t.Errorf("a Stop is logged as a failed setting:\n%s", tailLog(e, 20))
	}
}

// An abort (too hot) during the retest after a trim.
func TestR7AbortDuringTrimRetest(t *testing.T) {
	e, f := newFM(t, r7cfg())
	f.rules = []fmRule{needRule(ckey{2, 7}, always(600), 0.6, -12.5)}
	f.temp = func(f *fakeMiner) (float64, bool) {
		if f.trims[[2]int{2, 7}] < 0 && f.sinceApply() > 5 {
			return 95, true
		}
		return 70, true
	}
	runFM(t, e, f)
	dump(t, e)
	if len(f.trims) != 0 {
		t.Fatalf("trims left after an abort: %v", f.trims)
	}
	if strings.Contains(logStr(e), "isn't clean even with chip trims") {
		t.Errorf("an abort is logged as a failed setting:\n%s", tailLog(e, 20))
	}
}

// A stop after a trim was kept (later clock): the kept trim comes off at the end.
func TestR7StopAfterKeptTrim(t *testing.T) {
	e, f := newFM(t, r7cfg())
	f.rules = []fmRule{needRule(ckey{2, 7}, always(600, 625), 0.6, -12.5)}
	f.stopWhen = func(f *fakeMiner) bool { return f.plan.MHz == 625 && f.sinceApply() > 5 }
	runFM(t, e, f)
	dump(t, e)
	if len(f.trims) != 0 || len(e.runTrims) != 0 {
		t.Fatalf("trims left after a stop: %v / %v", f.trims, e.runTrims)
	}
	mustContain(t, e, "chip trims this run made are taken off")
}

// Hand trims: a hand-slowed chip and a hand "+" chip are never changed by the run; a hand "+" partner doesn't
// block a trim (it isn't slowed); a hand-slowed partner does.
func TestR7HandTrims(t *testing.T) {
	e, f := newFM(t, r7cfg())
	f.trims[[2]int{1, 5}] = -12.5 // hand
	f.trims[[2]int{1, 9}] = 25    // hand "+"
	f.trims[[2]int{3, 3}] = 12.5  // hand "+" partner of b3.c4
	f.trims[[2]int{0, 11}] = -25  // hand-slowed partner of b0.c12
	f.rules = []fmRule{
		needRule(ckey{1, 9}, map[int]int{600: 8900}, 0.6, -12.5),
		needRule(ckey{3, 4}, always(600, 625), 0.6, -12.5),
		needRule(ckey{0, 12}, map[int]int{625: 9000}, 0.6, -12.5),
	}
	runFM(t, e, f)
	dump(t, e)
	if f.trims[[2]int{1, 5}] != -12.5 || f.trims[[2]int{1, 9}] != 25 || f.trims[[2]int{3, 3}] != 12.5 || f.trims[[2]int{0, 11}] != -25 {
		t.Fatalf("hand trims changed: %v (calls %v)", f.trims, f.trimCalls)
	}
	for _, c := range f.trimCalls {
		if strings.HasPrefix(c, "1.9=") || strings.HasPrefix(c, "0.12=") || strings.HasPrefix(c, "1.5=") {
			t.Fatalf("hand-trimmed chip or slowed partner touched: %v", f.trimCalls)
		}
	}
	mustContain(t, e, "b1.c9 has a trim set by hand", "its partner b0.c11 is already slowed")
	if !strings.Contains(strings.Join(f.trimCalls, " "), "3.4=-12.5") {
		t.Fatalf("b3.c4 (hand + partner) should be trimmable: %v", f.trimCalls)
	}
}

// The trim check lifts a trim at the best setting (625, marginal: "clean with a chip trim"), so the confirm tries
// 600 first and puts the lifted trim back for it. 600 fails its confirm; 625 is then confirmed WITH the trim the
// trim check showed isn't needed there, and the run keeps it.
func TestR7LiftedTrimBackAtLiftedSetting(t *testing.T) {
	e, f := newFM(t, r7cfg())
	events(f,
		evt{mhz: 625, mv: 8800, nth: 1, at: 5, do: bumpChip(ckey{2, 7}, 3)},  // bad luck: c7 gets trimmed
		evt{mhz: 600, mv: 8800, nth: 2, at: 5, do: bumpChip(ckey{1, 20}, 6)}, // 600's confirm fails
	)
	runFM(t, e, f)
	dump(t, e)
	mustContain(t, e, "slowing b2.c7 to -12.5 MHz", "still clean, the trim is lifted to +0 MHz", "CONFIRMED: 625 MHz")
	if len(f.trims) != 0 {
		t.Fatalf("625 confirmed with a trim the trim check lifted there: %v\n%s", f.trims, tailLog(e, 30))
	}
}

// The watchdog ended the run (dead) while it sat at the end of its confirm, and the frozen goroutine woke up
// later: the watchdog already took every trim back, so the late end mustn't hand "kept" trims to auto-trim
// or log them as kept.
func TestR7DeadRunDoesntAdoptTrims(t *testing.T) {
	c := r7cfg()
	c.BuildPresets = true // stuck after the confirm, in the presets
	e, f := newFM(t, c)
	f.rules = []fmRule{needRule(ckey{2, 7}, always(600, 625), 0.6, -12.5)}
	adopted := 0
	e.adoptTrims = func(ts []chipTrim, _ string) { adopted += len(ts) }
	f.onTick = func(f *fakeMiner) {
		if f.e.confirmed && !f.e.dead.Load() { // (the engine goroutine: its own field)
			f.e.dead.Store(true) // tunerForceEnd: hooks dead, trims back to before the run
			f.trims = map[[2]int]float64{}
		}
	}
	runFM(t, e, f)
	dump(t, e)
	if !e.dead.Load() {
		t.Fatalf("scenario didn't reach the confirm:\n%s", tailLog(e, 30))
	}
	if adopted > 0 || strings.Contains(logStr(e), "CHIP TRIMS kept by this run") {
		t.Fatalf("a run the watchdog ended handed %d trim(s) to auto-trim:\n%s", adopted, tailLog(e, 12))
	}
}

// confirmLifted: a slower candidate that mapped clean with the trim gets it back; the lifted-at setting tried
// after it gets the lift again (the old code put the trims back once and left them on for good).
func TestR7ConfirmLiftedPerCandidate(t *testing.T) {
	e, f := newFM(t, r7cfg())
	at, slow := planMV{625, 8800, 9100}, planMV{600, 8800, 9100}
	k := [2]int{2, 7}
	f.trims[k] = 0
	delete(f.trims, k)
	e.runTrims = map[[2]int]float64{}
	e.lifted, e.liftedTo, e.liftedAt = map[[2]int]float64{k: -25}, map[[2]int]float64{k: -12.5}, at
	e.runTrims[k] = -12.5
	f.trims[k] = -12.5
	e.trimsAtPass = map[[2]int]map[[2]int]float64{{600, 8800}: {k: -25}}
	e.confirmLifted(slow)
	if f.trims[k] != -25 || e.runTrims[k] != -25 {
		t.Fatalf("slower candidate: %v %v", f.trims, e.runTrims)
	}
	e.confirmLifted(at)
	if f.trims[k] != -12.5 || e.runTrims[k] != -12.5 {
		t.Fatalf("lifted-at candidate: %v %v", f.trims, e.runTrims)
	}
	e.trimsAtPass = map[[2]int]map[[2]int]float64{{600, 8800}: {}} // 600 mapped clean before the trim was made
	e.confirmLifted(slow)
	if _, on := f.trims[k]; on || len(e.runTrims) != 0 {
		t.Fatalf("slower candidate that ran without it: %v %v", f.trims, e.runTrims)
	}
}
