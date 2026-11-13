package main

// Findings: each test asserts the intended behaviour and FAILS on v1.5.16 (run: go test -run TestLogicBug).

import (
	"path/filepath"
	"strings"
	"testing"
)

// F1. The real trimSet writes the trims file first and then applies it live; when the live apply fails
// (no fresh hook report, cgminer busy), trimOne treats the trim as not made — but the file keeps it, and the
// hook applies the file at the next clock change (every tuner step). The chip stays slowed, untracked, never
// undone, and from then on the tuner calls it "a trim set by hand".
func TestLogicBugTrimFileKeptOnApplyError(t *testing.T) {
	old := *authFile
	*authFile = filepath.Join(t.TempDir(), "auth.json")
	defer func() { *authFile = old }()
	c := small()
	c.HighestMHz = 625
	e, f := newFM(t, c)
	real := newTunerEngine(tunerConfig{})
	e.trimSet, e.trimGet = real.trimSet, real.trimGet // on this host reapplyTrims fails: no hook report
	f.rules = []fmRule{needRule(ckey{2, 7}, map[int]int{600: 8900, 625: 8900}, 0.6, 0)}
	runFM(t, e, f)
	dump(t, e)
	mustContain(t, e, "no chip trim (couldn't set the trim")
	if ts := readChipTrims(); len(ts) != 0 {
		t.Fatalf("the log says no trim was made, but the trims file holds %+v (applied by the hook at the next clock change)\n%s", ts, tailLog(e, 12))
	}
}

// F2. A miner restart early in the baseline ends the baseline watch after a minute, and the run carries on
// with those rates: a minute's worth of the Pseudo prior is ~25× a 25-minute one, so every limit opens up and
// a chip making 4 errors per test (limit 2 normally) passes. The baseline should be redone (or the run stop).
func TestLogicBugBaselineRestartInflatesLimits(t *testing.T) {
	run := func(restart bool) *tunerEngine {
		c := small()
		c.HighestMHz, c.ChipTrim, c.ConfirmWin = 600, false, false
		e, f := newFM(t, c)
		f.rules = []fmRule{needRule(ckey{1, 20}, always(600), 0.16, 0)} // ~4 errors per 25 min at 600, any voltage
		if restart {
			events(f, evt{615, 9100, 1, 6.2, func(f *fakeMiner) { f.restart() }})
		}
		runFM(t, e, f)
		dump(t, e)
		return e
	}
	if c := curveStr(run(false).curve); c != "575/9100" {
		t.Fatalf("control: curve %s", c)
	}
	e := run(true)
	if c := curveStr(e.curve); c != "575/9100" {
		st := e.testStats[[2]int{600, 8800}]
		t.Fatalf("after a restart 1 min into the baseline, 600 MHz passes with b1.c20 +%d (limit now %.1f, board limit loosened too): curve %s\n%s",
			st.Gained[ckey{1, 20}], st.Limits[ckey{1, 20}], c, tailLog(e, 8))
	}
}

// F3. The trim check lifts a trim at the best setting (where more voltage makes it unneeded), but a slower
// clock's pass relied on it. When the best fails its confirm, the confirm falls back to that slower setting —
// now without its trim — and rejects it (and saves it as rejected).
func TestLogicBugConfirmFallbackLostTrim(t *testing.T) {
	c := small()
	c.BaselineMV = 8800 // baseline 615 / PV 9100: 575 starts at the floor
	c.HighestMHz = 600
	e, f := newFM(t, c)
	need := map[int]int{575: 8900, 600: 8900}
	f.rules = []fmRule{
		needRule(ckey{2, 7}, need, 0.6, -12.5),     // b2.c7: needs PV 9200, or a 12.5 MHz trim
		boardRule(1, map[int]int{600: 8900}, 0.05), // board 1 at 600 needs PV 9200 (a board-wide fail: no trim)
		func(f *fakeMiner, k ckey) float64 { // 600 doesn't hold the longer confirm
			if k == (ckey{0, 3}) && f.plan.MHz == 600 && strings.HasPrefix(planState(f.e, "confirm"), "run") {
				return 0.6
			}
			return 0
		},
	}
	runFM(t, e, f)
	dump(t, e)
	if c := curveStr(e.curve); c != "575/9100 600/9200" {
		t.Fatalf("setup: curve %s\n%s", c, tailLog(e, 40))
	}
	mustContain(t, e, "575 MHz / PV 9100 (mV 8800) is clean with chip trims", "b2.c7 at +0 MHz: still clean, the trim is lifted")
	if !e.confirmed || *e.best != (planMV{575, 8800, 9100}) {
		t.Fatalf("575 / PV 9100 was clean with b2.c7 at -12.5, but its confirm ran with the trim lifted: confirmed %v best %v trims %v\n%s",
			e.confirmed, e.best, f.trims, tailLog(e, 14))
	}
}

// F4. Stopped during the presets, after a confirm: the miner is left on the confirmed setting, but the plan's
// Finish row says it went back to what it ran before.
func TestLogicBugStopDuringPresetsMessage(t *testing.T) {
	c := small()
	c.BuildPresets, c.WarmestC, c.PresetMin = true, 75, 20
	e, f := newFM(t, c)
	f.stopWhen = func(f *fakeMiner) bool {
		return strings.HasPrefix(planState(f.e, "presets"), "run") && f.sinceApply() > 8
	}
	runFM(t, e, f)
	dump(t, e)
	if !f.stopFired {
		t.Fatal("never reached the presets")
	}
	st := planState(e, "finish")
	back := strings.Contains(st, "back to what it ran before")
	if back != (lastApplied(f) == basePlan) {
		t.Fatalf("finish says %q, but the miner was left on %v", st, lastApplied(f))
	}
}

// F5. Presets built from a 12.5 MHz map step use int(math.Round(12.5)) = 13 MHz clock steps: Middle/Low/Lowest
// land on 612, 599, 586 MHz — each runs a lower 12.5 step than its label (600, 587.5, 575), off the grid.
func TestLogicBugPresetClocksOffGrid(t *testing.T) {
	c := small()
	c.Step, c.LowestMHz, c.HighestMHz = 12.5, 625, 638
	c.BuildPresets, c.WarmestC, c.PresetMin = true, 75, 20
	e, f := newFM(t, c)
	f.rules = []fmRule{needRule(ckey{1, 20}, always(638), 0.6, 0)}
	e.cfg.ChipTrim = false
	runFM(t, e, f) // the harness fails on a clock that isn't the request for its own 12.5 MHz step
	dump(t, e)
}

// F6. A voltage step that doesn't land on the maximum (8800 + 3×150 = 9250): PV 9600 is never tested, yet the
// clock is reported "not clean up to PV 9600" and the curve ends there.
func TestLogicBugMaxPVNeverTested(t *testing.T) {
	c := small()
	c.HighestMHz, c.MVStep, c.ChipTrim = 600, 150, false
	e, f := newFM(t, c)
	f.rules = []fmRule{needRule(ckey{1, 20}, map[int]int{600: 9300}, 0.6, 0)} // 600 needs PV 9600
	runFM(t, e, f)
	dump(t, e)
	top := 0
	for _, p := range f.applied {
		if p.MHz == 600 && p.PV > top {
			top = p.PV
		}
	}
	if top != 9600 {
		t.Fatalf("600 MHz tried up to PV %d only; log: %q; curve %s", top,
			"600 MHz: no clean voltage up to PV 9600", curveStr(e.curve))
	}
}

// F7. A chip already proven untrimmable at a clock (still failing at -50) is put through -12.5…-50 again at
// every voltage step of that clock: 4 extra tests (~35 min) per step. Here 600 needs PV 9400 (+300 mV).
func TestLogicBugRepeatsUselessTrims(t *testing.T) {
	c := small()
	c.HighestMHz, c.MVStep = 600, 50
	e, f := newFM(t, c)
	f.rules = []fmRule{needRule(ckey{2, 7}, map[int]int{600: 9100}, 0.6, 0)}
	runFM(t, e, f)
	dump(t, e)
	n := strings.Count(logStr(e), "slowing b2.c7 to -12.5 MHz")
	if n > 1 {
		t.Fatalf("b2.c7 trimmed from scratch at %d voltage steps of 600 MHz (%d trim tests, %.1f h); it already failed at -50 at the first",
			n, 4*n, e.now().Sub(e.startedAt).Hours())
	}
}

// F8 (cosmetic). The trim check's tests are attached to the run plan row of the last clock the curve tried
// (a failed one here), not to a trim-check row.
func TestLogicBugTrimCheckRowsUnderWrongClock(t *testing.T) {
	c := small()
	c.HighestMHz = 650
	e, f := newFM(t, c)
	f.rules = []fmRule{needRule(ckey{2, 7}, always(600, 625, 650), 0.6, -12.5), needRule(ckey{1, 20}, always(650), 0.6, 0)}
	runFM(t, e, f)
	dump(t, e)
	for _, k := range e.clockByMHz(650).Kids {
		if strings.HasPrefix(k.Label, "625 MHz") {
			t.Fatalf("650 MHz's row holds the trim check's 625 test: %+v", e.clockByMHz(650).Kids)
		}
	}
}
