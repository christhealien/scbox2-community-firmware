package main

// Round 3: edge cases of the bottom-up map walk.

import (
	"strings"
	"testing"
)

func bu(c tunerConfig) tunerConfig { c.TopDown = false; return c }

func walkOf(e *tunerEngine, lo, hi int) string {
	var w []string
	for _, r := range e.results {
		if r.MHz >= lo && r.MHz <= hi {
			w = append(w, r.Result[:1]+itoa(r.MHz)+"/"+itoa(r.PV))
		}
	}
	return strings.Join(w, " ")
}

// The plan shown before/while the run says how the walk goes: bottom-up must not describe the old walk.
func TestR3PlanDescribesBottomUp(t *testing.T) {
	e, _ := newFM(t, bu(scfg()))
	e.buildSteps(planMV{615, 9100, 9400}, "x")
	var desc string
	for _, s := range e.steps {
		if s.Key == "curve" {
			desc = s.Desc
		}
	}
	if strings.Contains(desc, "down until it fails") || strings.Contains(desc, "starts at the baseline voltage") {
		t.Fatalf("bottom-up plan describes the old walk:\n%s", desc)
	}
}

// A dry run's curve is made up and not saved; its RESULT line mustn't say no clock was clean.
func TestR3DryRunResultLine(t *testing.T) {
	c := bu(scfg())
	c.DryRun = true
	c.LowestMHz, c.HighestMHz = 550, 600
	e, f := newFM(t, c)
	runFM(t, e, f)
	if len(e.curve) == 0 {
		t.Fatalf("dry run made no curve")
	}
	if l := logStr(e); strings.Contains(l, "no clean clocks found") {
		t.Fatalf("dry run with a %d-clock curve logs \"no clean clocks found\":\n%s", len(e.curve), tailLog(e, 20))
	}
}

// Stopped after two clocks were mapped: the RESULT line mustn't say there's no curve.
func TestR3StopResultLine(t *testing.T) {
	c := bu(scfg())
	e, f := newFM(t, c)
	f.stopWhen = func(f *fakeMiner) bool { return f.plan.MHz == 600 && f.sinceApply() > 10 }
	runFM(t, e, f)
	if !f.stopFired || len(e.curve) != 2 {
		t.Fatalf("stop %v curve %v", f.stopFired, curveStr(e.curve))
	}
	if l := logStr(e); strings.Contains(l, "stopped before a clock was finished, so there's no curve") {
		t.Fatalf("2 clocks mapped, the log says there's no curve:\n%s", tailLog(e, 12))
	}
}

// MinMV above the baseline's mV: the first clock starts at MinMV, nothing below it is sent.
func TestR3MinAboveBaseline(t *testing.T) {
	c := bu(scfg())
	c.BaselineMV = 8800 // baseline PV 9100
	c.MinMV = 8900      // range PV 9200–9600
	c.LowestMHz, c.HighestMHz = 550, 600
	e, f := newFM(t, c)
	f.rules = []fmRule{boardRule(1, map[int]int{600: 9000}, 0.5)}
	runFM(t, e, f)
	if got := curveStr(e.curve); got != "550/9200 575/9200 600/9300" {
		t.Fatalf("curve %s walk %s", got, walkOf(e, 550, 600))
	}
	for _, p := range f.applied[1:] {
		if p.MV < 8900 && p != (planMV{615, 8800, 9100}) {
			t.Fatalf("below MinMV: %v", p)
		}
	}
	mustContain(t, e, "550 MHz: starting at PV 9200")
}

// A step that doesn't divide the range: the last step lands on MaxMV (PV 9600), which is retested; the
// next clock starts there.
func TestR3StepNotDividing(t *testing.T) {
	c := bu(scfg())
	c.MVStep = 75 // 8800 8875 8950 9025 9100 9175 9250 → 9300
	c.LowestMHz, c.HighestMHz = 550, 600
	c.ChipTrim = false
	e, f := newFM(t, c)
	f.rules = []fmRule{boardRule(1, map[int]int{575: 9260, 600: 9260}, 0.15)} // borderline: retest at the top
	runFM(t, e, f)
	dump(t, e)
	w := walkOf(e, 575, 600)
	if !strings.Contains(w, "f575/9550 c575/9600") || !strings.HasPrefix(curveStr(e.curve), "550/9100 575/9600 600/9600") {
		t.Fatalf("walk %s curve %s", w, curveStr(e.curve))
	}
	for _, p := range f.applied {
		if p.PV > 9600 || p.MV > 9300 {
			t.Fatalf("past the top: %v", p)
		}
	}
}

// Stop at each point of a bottom-up run: back to the baseline, trims undone, nothing confirmed or saved
// for a cut map, every phase ends.
func TestR3StopAtEachPointBottomUp(t *testing.T) {
	for _, tc := range []struct {
		name  string
		when  func(f *fakeMiner) bool
		curve bool
	}{
		{"baseline", func(f *fakeMiner) bool { return f.phase() == "baseline" && f.sinceApply() > 10 }, false},
		{"first test at the bottom", func(f *fakeMiner) bool { return f.plan.MHz == 575 && f.plan.MV == 8800 && f.sinceApply() > 15 }, false},
		{"settle of the next clock", func(f *fakeMiner) bool { return f.plan.MHz == 600 && f.sinceApply() > 1 }, false},
		{"trim retest", func(f *fakeMiner) bool { return f.plan.MHz == 600 && len(f.trims) > 0 && f.sinceApply() > 6 }, false},
		{"step up", func(f *fakeMiner) bool { return f.plan.MHz == 625 && f.plan.MV == 8900 && f.sinceApply() > 2 }, false},
		{"trim check", func(f *fakeMiner) bool {
			return strings.HasPrefix(planState(f.e, "trimcheck"), "run") && f.sinceApply() > 2
		}, true},
		{"confirm", func(f *fakeMiner) bool {
			return strings.HasPrefix(planState(f.e, "confirm"), "run") && f.sinceApply() > 20
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := bu(small())
			e, f := newFM(t, c)
			// 2.7 is bad at 600/625 unless slowed 12.5; board 3 needs 8900 at 625
			f.rules = []fmRule{needRule(ckey{2, 7}, always(600, 625), 0.6, -12.5), boardRule(3, map[int]int{625: 8900}, 0.5)}
			f.stopWhen = tc.when
			runFM(t, e, f)
			dump(t, e)
			if !f.stopFired {
				t.Fatalf("stop condition never met: %s", walkOf(e, 575, 625))
			}
			if lastApplied(f) != basePlan || e.confirmed {
				t.Fatalf("after a stop: last %v confirmed %v", lastApplied(f), e.confirmed)
			}
			if len(f.trims) != 0 {
				t.Fatalf("trims left %v (calls %v)", f.trims, f.trimCalls)
			}
			sc := savedCurveNow()
			if (len(sc.Curve) > 0) != tc.curve || sc.Confirmed != nil {
				t.Fatalf("saved curve %+v", sc)
			}
			for _, s := range e.steps {
				if s.State == "run" || s.State == "todo" {
					t.Fatalf("plan phase %q left %s", s.Label, s.State)
				}
				for _, k := range s.Kids {
					if k.State == "run" {
						t.Fatalf("row %q left running", k.Label)
					}
				}
			}
		})
	}
}

// Too hot at the second voltage of a clock: the run aborts, goes back, the curve phase is cut.
func TestR3AbortMidWalk(t *testing.T) {
	c := bu(small())
	e, f := newFM(t, c)
	f.rules = []fmRule{boardRule(3, map[int]int{600: 8900}, 0.5)}
	f.temp = func(f *fakeMiner) (float64, bool) {
		if f.plan.MHz == 600 && f.plan.MV == 8900 && f.sinceApply() > 5 {
			return 95, true
		}
		return 70, true
	}
	runFM(t, e, f)
	if lastApplied(f) != basePlan || e.confirmed || len(savedCurveNow().Curve) != 0 {
		t.Fatalf("after an abort: last %v confirmed %v", lastApplied(f), e.confirmed)
	}
	if st := planState(e, "curve"); !strings.HasPrefix(st, "cut: safety stop") {
		t.Fatalf("curve phase %s", st)
	}
}

// A clock that's clean only with a chip trim, at the bottom of the range: the trim stays, the clock keeps
// the bottom voltage, the next clock starts there, the mark says why.
func TestR3OnlyWithTrimAtBottom(t *testing.T) {
	c := bu(scfg())
	c.LowestMHz, c.HighestMHz = 550, 600
	e, f := newFM(t, c)
	f.rules = []fmRule{needRule(ckey{2, 7}, always(575, 600), 0.6, -12.5)}
	runFM(t, e, f)
	dump(t, e)
	if got := curveStr(e.curve); got != "550/9100 575/9100 600/9100" {
		t.Fatalf("curve %s walk %s", got, walkOf(e, 550, 600))
	}
	if w := walkOf(e, 575, 600); !strings.HasPrefix(w, "f575/9100 c575/9100 c600/9100") {
		t.Fatalf("walk %s", w)
	}
	if e.marginal[[2]int{575, 8800}] != "clean with a chip trim" {
		t.Fatalf("marginal %v", e.marginal)
	}
	if sortedTrims(f.trims) != "2.7=-12.5" {
		t.Fatalf("trims %v", f.trims)
	}
}

// Baseline reuse still works bottom-up: the baseline's clock on the grid, the walk climbs to its voltage.
func TestR3BaselineReusedOnTheWayUp(t *testing.T) {
	c := bu(scfg())
	c.BaselineMHz, c.BaselineMV = 600, 8900
	c.LowestMHz, c.HighestMHz = 575, 600
	c.ChipTrim = false
	e, f := newFM(t, c)
	f.rules = []fmRule{boardRule(1, map[int]int{600: 8900}, 0.5)}
	runFM(t, e, f)
	if w := walkOf(e, 575, 600); !strings.HasPrefix(w, "c575/9100 f600/9100 f600/9150") {
		t.Fatalf("walk %s", w)
	}
	mustContain(t, e, "600 MHz / PV 9200: clean in the baseline, not tested again")
	if got := curveStr(e.curve); got != "575/9100 600/9200" {
		t.Fatalf("curve %s", got)
	}
}

// The confirm's fallback: with the top clock failing its confirm and the clocks under it marked marginal
// (a chip used its whole limit), the run should fall back one clock, not to the slowest clean clock.
func TestR3ConfirmFallbackSkipsMarginalClocks(t *testing.T) {
	c := bu(scfg())
	c.LowestMHz, c.HighestMHz = 550, 650
	c.ChipTrim = false
	e, f := newFM(t, c)
	// 575, 600, 625: chip 1.20 makes 2 errors mid-test (100 % of a quiet chip's limit 2): marginal passes
	events(f, evt{575, 8800, 1, 15, bumpChip(ckey{1, 20}, 2)}, evt{600, 8800, 1, 15, bumpChip(ckey{1, 20}, 2)},
		evt{625, 8800, 1, 15, bumpChip(ckey{1, 20}, 2)})
	f.rules = []fmRule{func(f *fakeMiner, k ckey) float64 {
		if k == (ckey{0, 3}) && f.plan.MHz == 650 && f.sinceApply() > 30 {
			return 0.6 // clean for a 25-min test, not for the 50-min confirm
		}
		return 0
	}}
	runFM(t, e, f)
	dump(t, e)
	if got := curveStr(e.curve); got != "550/9100 575/9100 600/9100 625/9100 650/9100" {
		t.Fatalf("curve %s", got)
	}
	t.Logf("marginal %v; best %v confirmed %v", e.marginal, e.best, e.confirmed)
	if e.best == nil || e.best.MHz < 625 {
		t.Fatalf("650 failed its confirm and the run fell back to %v (625 passed, marginal): %d clock steps lost\n%s",
			e.best, (650-e.best.MHz)/25, tailLog(e, 14))
	}
}

// Confirm off: the run ends on the fastest non-marginal pass. Bottom-up only has each clock's lowest pass
// (often marginal), so it falls to a slower clock where the old walk had the same clock a step higher.
func TestR3ConfirmOffMarginalTop(t *testing.T) {
	for _, confirm := range []bool{false, true} {
		run := func(topDown bool) *tunerEngine {
			c := scfg()
			c.TopDown = topDown
			c.LowestMHz, c.HighestMHz = 550, 650
			c.ChipTrim, c.ConfirmWin = false, confirm
			e, f := newFM(t, c)
			// 650 needs 8850 (PV 9150); there, chip 1.20 uses its whole limit (2 errors): a marginal pass.
			// 625 needs 8850 too (board 3): the old walk then starts 650 a step higher, at 8900
			f.rules = []fmRule{needRule(ckey{1, 20}, map[int]int{650: 8850}, 0.6, 0), boardRule(3, map[int]int{625: 8850}, 0.5)}
			events(f, evt{650, 8850, 1, 15, bumpChip(ckey{1, 20}, 2)})
			runFM(t, e, f)
			dump(t, e)
			return e
		}
		td, bu := run(true), run(false)
		t.Logf("confirm %v: top-down best %v, bottom-up best %v", confirm, td.best, bu.best)
		if td.best == nil || bu.best == nil || bu.best.MHz < td.best.MHz {
			t.Errorf("confirm %v: bottom-up ends on %v, the old walk on %v", confirm, bu.best, td.best)
		}
	}
}

// A unit whose supply holds PV 9200 as its floor (PV 9100 and 9150 read the same as 9200): the bottom-up walk
// never detects it (detection only ran walking down). What it costs: the steps under the floor repeat the
// floor's test; the curve says PV 9100 where the boards ran PV 9200's voltage (applying it gives that again).
func TestR3RaisedFloorBottomUp(t *testing.T) {
	c := bu(scfg())
	c.LowestMHz, c.HighestMHz = 550, 600
	c.ChipTrim = false
	e, f := newFM(t, c)
	phys := func(f *fakeMiner) int { return max(f.plan.MV, 8900) } // the boards' real voltage (as mV)
	e.readVolt = func() (float64, bool) {
		f.mu.Lock()
		defer f.mu.Unlock()
		return float64(phys(f)+300) - 80, true
	}
	f.rules = []fmRule{func(f *fakeMiner, k ckey) float64 {
		if k.B == 1 && f.plan.MHz == 600 && phys(f) < 8950 {
			return 0.5
		}
		return 0
	}}
	runFM(t, e, f)
	t.Logf("walk %s; curve %s; floorPV %d", walkOf(e, 550, 600), curveStr(e.curve), e.floorPV)
	if e.floorPV != 0 {
		t.Logf("floor detected at %d", e.floorPV)
	}
	if got := curveStr(e.curve); got != "550/9100 575/9100 600/9250" {
		t.Fatalf("curve %s", got)
	}
}
