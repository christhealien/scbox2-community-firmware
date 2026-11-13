package main

// Scenario tests of the tuner engine's decisions (map mode, chip trims, confirm, stops, aborts, saving),
// on the fake miner of logic_harness_test.go. Each test is a whole run on a virtual clock.

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// small: 575–625 MHz by 25, PV 9100–9600 by 100, baseline 615 / PV 9400, chip trims on.
func small() tunerConfig {
	c := scfg()
	c.LowestMHz, c.HighestMHz = 575, 625
	c.MVStep = 100
	return c
}

var basePlan = planMV{615, 9100, 9400}

type evt struct {
	mhz, mv int     // the setting (mv 0: any)
	nth     int     // which application of it (1 = the first)
	at      float64 // minutes after it was applied
	do      func(f *fakeMiner)
}

// events: one-shot actions at a point of a given test.
func events(f *fakeMiner, evs ...evt) {
	done := map[int]bool{}
	prev := f.onTick
	f.onTick = func(f *fakeMiner) {
		if prev != nil {
			prev(f)
		}
		for i, ev := range evs {
			if done[i] || f.plan.MHz != ev.mhz || (ev.mv != 0 && f.plan.MV != ev.mv) {
				continue
			}
			if f.seen[f.plan] == ev.nth && f.sinceApply() >= ev.at {
				done[i] = true
				ev.do(f)
			}
		}
	}
}

func bumpChip(k ckey, n float64) func(f *fakeMiner) { return func(f *fakeMiner) { f.cnt[k] += n } }

func always(mhzs ...int) map[int]int {
	m := map[int]int{}
	for _, x := range mhzs {
		m[x] = 99999
	}
	return m
}

// 1. One bad chip at 600+ that a 12.5 MHz trim fixes: the trim stays, the board keeps its low voltage, the
// trim check finds it needed, the confirm passes and the run ends on it.
func TestLogicTrimFixesOneChip(t *testing.T) {
	e, f := newFM(t, small())
	f.rules = []fmRule{needRule(ckey{2, 7}, always(600, 625), 0.6, -12.5)}
	runFM(t, e, f)
	dump(t, e)
	if c := curveStr(e.curve); c != "575/9100 600/9100 625/9100" {
		t.Fatalf("curve %s\n%s", c, tailLog(e, 40))
	}
	if strings.Join(f.trimCalls, " ") != "2.7=-12.5 2.7=0 2.7=-12.5" {
		t.Fatalf("trim calls %v", f.trimCalls)
	}
	if sortedTrims(f.trims) != "2.7=-12.5" || sortedTrims(e.runTrims) != "2.7=-12.5" {
		t.Fatalf("trims %v / %v", f.trims, e.runTrims)
	}
	if !e.confirmed || *e.best != (planMV{625, 8800, 9100}) || lastApplied(f) != *e.best {
		t.Fatalf("best %v confirmed %v last %v", e.best, e.confirmed, lastApplied(f))
	}
	mustContain(t, e, "slowing b2.c7 to -12.5 MHz", "the trim is needed, back to -12.5 MHz", "CHIP TRIMS kept by this run")
}

// 2. A chip no trim fixes (it needs voltage): the trims are tried to -50, all undone, and the clock gets more
// voltage; nothing is left trimmed.
func TestLogicTrimNotEnoughUndone(t *testing.T) {
	c := small()
	c.HighestMHz = 600
	e, f := newFM(t, c)
	f.rules = []fmRule{needRule(ckey{2, 7}, map[int]int{600: 8900}, 0.6, 0)}
	runFM(t, e, f)
	dump(t, e)
	// (the undo walks back every step it made: -37.5, -25, -12.5, 0 — four live re-sends for one undo)
	if strings.Join(f.trimCalls, " ") != "2.7=-12.5 2.7=-25 2.7=-37.5 2.7=-50 2.7=-37.5 2.7=-25 2.7=-12.5 2.7=0" {
		t.Fatalf("trim calls %v", f.trimCalls)
	}
	if len(f.trims) != 0 || len(e.runTrims) != 0 {
		t.Fatalf("trims left: %v %v", f.trims, e.runTrims)
	}
	if c := curveStr(e.curve); c != "575/9100 600/9200" {
		t.Fatalf("curve %s", c)
	}
	mustContain(t, e, "b2.c7 is already at -50 MHz", "isn't clean even with chip trims: the trims made for it are undone", "600 MHz: more voltage, PV 9200")
	if !e.confirmed || lastApplied(f) != (planMV{600, 8900, 9200}) {
		t.Fatalf("last %v", lastApplied(f))
	}
}

// 3. Two bad chips on one board: both trimmed in turn, both kept.
func TestLogicTwoBadChipsOneBoard(t *testing.T) {
	c := small()
	c.HighestMHz = 600
	e, f := newFM(t, c)
	f.rules = []fmRule{needRule(ckey{2, 7}, always(600), 0.6, -12.5), needRule(ckey{2, 30}, always(600), 0.3, -12.5)}
	runFM(t, e, f)
	dump(t, e)
	// both were trimmed for 600 MHz; the run confirms 575 MHz (600 passed only on its retest), which the map ran
	// clean before either trim: v1.5.19 takes them off there (they'd only cost hashrate)
	if !strings.Contains(strings.Join(f.trimCalls, " "), "2.7=-12.5") || !strings.Contains(strings.Join(f.trimCalls, " "), "2.30=-12.5") {
		t.Fatalf("not both trimmed: %v", f.trimCalls)
	}
	if e.confirmed && e.best.MHz == 575 {
		mustContain(t, e, "the trims made for faster clocks come off")
		if len(e.runTrims) != 0 || len(f.trims) != 0 {
			t.Fatalf("trims left at 575: %v / %v", e.runTrims, f.trims)
		}
	} else if sortedTrims(e.runTrims) != "2.30=-12.5,2.7=-12.5" || sortedTrims(f.trims) != "2.30=-12.5,2.7=-12.5" {
		t.Fatalf("trims %v / %v\n%s", e.runTrims, f.trims, tailLog(e, 30))
	}
	if c := curveStr(e.curve); c != "575/9100 600/9100" {
		t.Fatalf("curve %s", c)
	}
}

// 4. Two bad chips, but the tuner may trim only one per board: the second can't be trimmed, the first trim is
// undone, the board gets more voltage.
func TestLogicTrimMaxPerBoard(t *testing.T) {
	c := small()
	c.HighestMHz, c.ChipTrimMax = 600, 1
	e, f := newFM(t, c)
	need := map[int]int{600: 8900}
	f.rules = []fmRule{needRule(ckey{2, 7}, need, 0.6, -12.5), needRule(ckey{2, 30}, need, 0.3, -12.5)}
	runFM(t, e, f)
	dump(t, e)
	mustContain(t, e, "1 chips on board 2 are trimmed already, the most this run may", "the trims made for it are undone")
	if len(f.trims) != 0 || len(e.runTrims) != 0 {
		t.Fatalf("trims left %v %v", f.trims, e.runTrims)
	}
	if c := curveStr(e.curve); c != "575/9100 600/9200" {
		t.Fatalf("curve %s", c)
	}
}

// 5. The bad chip is on a board the chip hook doesn't report (a dead chain): no trim, more voltage.
func TestLogicBadChipOnDeadChain(t *testing.T) {
	c := small()
	c.HighestMHz = 600
	e, f := newFM(t, c)
	f.hook = []int{0, 1, 2}
	f.rules = []fmRule{needRule(ckey{3, 5}, map[int]int{600: 8900}, 0.6, -12.5)}
	runFM(t, e, f)
	dump(t, e)
	if len(f.trimCalls) != 0 {
		t.Fatalf("trimmed a chip the hook doesn't have: %v", f.trimCalls)
	}
	mustContain(t, e, "no chip trim (the chip isn't in the hook's chip list)")
	if c := curveStr(e.curve); c != "575/9100 600/9200" {
		t.Fatalf("curve %s", c)
	}
}

// 6. Board 1 absent ("No CPB Detected"): board 3's bad chip is trimmed on chain 3 (not shifted), hashrate is
// judged against the 3-board baseline.
func TestLogicAbsentBoard(t *testing.T) {
	c := small()
	c.HighestMHz = 600
	e, f := newFM(t, c)
	f.boards, f.hook = []int{0, 2, 3}, []int{0, 2, 3}
	f.rules = []fmRule{needRule(ckey{3, 5}, always(600), 0.6, -12.5)}
	runFM(t, e, f)
	dump(t, e)
	if !strings.Contains(strings.Join(f.trimCalls, " "), "3.5=-12.5") { // trimmed on chain 3, not shifted
		t.Fatalf("trims %v %v", e.runTrims, f.trimCalls)
	}
	if e.confirmed && e.best.MHz == 575 { // confirmed where the map ran clean before the trim: it comes off
		mustContain(t, e, "the trims made for faster clocks come off")
	} else if sortedTrims(e.runTrims) != "3.5=-12.5" {
		t.Fatalf("trims %v %v", e.runTrims, f.trimCalls)
	}
	if c := curveStr(e.curve); c != "575/9100 600/9100" {
		t.Fatalf("curve %s", c)
	}
	for _, r := range e.results {
		if strings.Contains(r.Reason, "hashrate") {
			t.Fatalf("3-board hashrate judged against 4 boards: %v", r)
		}
	}
}

// 7. The miner restarts 10 minutes into a test (its counters drop to 0): that test isn't clean; it's retested
// once and the retest passes (a marginal pass: clean only on its retest). No chip is blamed or trimmed.
func TestLogicRestartMidTest(t *testing.T) {
	c := small()
	c.HighestMHz = 600
	e, f := newFM(t, c)
	f.baseNoise = 0.004
	events(f, evt{600, 8800, 1, 13, func(f *fakeMiner) { f.restart() }})
	runFM(t, e, f)
	dump(t, e)
	got := strings.Join(resultsStr(e), ", ")
	if !strings.Contains(got, "600/9100 failed, 600/9100 clean") {
		t.Fatalf("results %s", got)
	}
	mustContain(t, e, "the miner restarted during the test): retesting it once")
	if len(f.trimCalls) != 0 {
		t.Fatalf("a restart got a chip trimmed: %v", f.trimCalls)
	}
	if e.marginal[[2]int{600, 8800}] != "clean only on its retest" {
		t.Fatalf("marginal %v", e.marginal)
	}
}

// 8. The counters drop to 0 without a restart (a counter reset): no negative counts, no false fail.
func TestLogicCounterResetNoRestart(t *testing.T) {
	c := small()
	c.HighestMHz = 600
	e, f := newFM(t, c)
	events(f, evt{600, 8800, 1, 13, func(f *fakeMiner) {
		for k := range f.cnt {
			f.cnt[k] = 0
		}
	}})
	runFM(t, e, f)
	for _, r := range e.results {
		if r.Result != "clean" || r.ErrAdd < 0 {
			t.Fatalf("%v", r)
		}
	}
}

// 9. Hashrate drops to 0 for 6 minutes of a test: it fails on hashrate, gets a retest (not decisive), passes.
func TestLogicHashrateDropout(t *testing.T) {
	c := small()
	c.HighestMHz = 600
	e, f := newFM(t, c)
	f.ghsMul = func(f *fakeMiner) float64 {
		if f.plan == (planMV{600, 8800, 9100}) && f.seen[f.plan] == 1 && f.sinceApply() > 8 && f.sinceApply() < 14 {
			return 0
		}
		return 1
	}
	runFM(t, e, f)
	dump(t, e)
	var fail *testResult
	for i, r := range e.results {
		if r.MHz == 600 && r.Result == "failed" {
			fail = &e.results[i]
		}
	}
	if fail == nil || !strings.Contains(fail.Reason, "hashrate") {
		t.Fatalf("no hashrate fail: %v", resultsStr(e))
	}
	if !strings.Contains(strings.Join(resultsStr(e), ","), "600/9100 failed,600/9100 clean") || len(f.trimCalls) != 0 {
		t.Fatalf("results %v trims %v", resultsStr(e), f.trimCalls)
	}
}

// 10. A temperature spike during a chip-trim retest: the run aborts, the trim made for that test is undone,
// the miner goes back to the baseline, nothing is confirmed and no curve is saved.
func TestLogicTempAbortDuringTrimRetest(t *testing.T) {
	e, f := newFM(t, small())
	f.rules = []fmRule{needRule(ckey{2, 7}, always(600, 625), 0.6, -12.5)}
	f.temp = func(f *fakeMiner) (float64, bool) {
		if f.plan.MHz == 600 && len(f.trims) > 0 && f.sinceApply() > 10 {
			return 89, true
		}
		return 70, true
	}
	runFM(t, e, f)
	dump(t, e)
	if len(f.trims) != 0 || len(e.runTrims) != 0 {
		t.Fatalf("the trim of an aborted test stayed: %v %v", f.trims, e.runTrims)
	}
	if lastApplied(f) != basePlan || e.confirmed {
		t.Fatalf("last %v confirmed %v", lastApplied(f), e.confirmed)
	}
	if sc := savedCurveNow(); len(sc.Curve) != 0 {
		t.Fatalf("an aborted map saved a curve: %+v", sc)
	}
	last := e.results[len(e.results)-1]
	if last.Result != "aborted" || !strings.Contains(last.Reason, "89.0°C") {
		t.Fatalf("last result %+v", last)
	}
	if !strings.HasPrefix(planState(e, "finish"), "cut") || !strings.Contains(e.abortWhy, "89.0°C") {
		t.Fatalf("finish %s / %q", planState(e, "finish"), e.abortWhy)
	}
}

// 11. 625 MHz isn't clean even at the PV ceiling: it's tested up to PV 9600 (never past) and the curve ends;
// the run ends on 600.
func TestLogicPVCeiling(t *testing.T) {
	c := small()
	c.HighestMHz = 650
	e, f := newFM(t, c)
	f.rules = []fmRule{needRule(ckey{1, 20}, map[int]int{600: 9200, 625: 9999, 650: 0}, 0.6, 0)}
	c.ChipTrim = false
	e.cfg.ChipTrim = false
	runFM(t, e, f)
	dump(t, e)
	top := 0
	for _, p := range f.applied {
		if p.MHz == 625 && p.PV > top {
			top = p.PV
		}
		if p.MHz == 650 {
			t.Fatalf("650 tried after 625 failed at the ceiling")
		}
	}
	if top != 9600 {
		t.Fatalf("625 was tested up to PV %d, not the ceiling 9600", top)
	}
	if c := curveStr(e.curve); c != "575/9100 600/9500" || !e.confirmed || lastApplied(f) != (planMV{600, 9200, 9500}) {
		t.Fatalf("curve %s last %v", c, lastApplied(f))
	}
	if !strings.HasPrefix(e.steps[1].Kids[2].State, "fail") || e.steps[1].Kids[3].State != "skip" {
		t.Fatalf("plan rows %+v", e.steps[1].Kids)
	}
}

// 12. Every clock clean at the lowest voltage: the curve runs to the end of the range and ends on the top.
func TestLogicClockRangeEnd(t *testing.T) {
	c := small()
	c.HighestMHz = 700
	e, f := newFM(t, c)
	runFM(t, e, f)
	if c := curveStr(e.curve); c != "575/9100 600/9100 625/9100 650/9100 675/9100 700/9100" {
		t.Fatalf("curve %s", c)
	}
	if !e.confirmed || lastApplied(f) != (planMV{700, 8800, 9100}) {
		t.Fatalf("last %v", lastApplied(f))
	}
	// each faster clock starts at the previous lowest (8800) and isn't walked below the minimum
	n := 0
	for _, r := range e.results {
		if r.MHz > 575 {
			n++
		}
	}
	if n != 5+1 { // one test per faster clock, plus the confirm
		t.Fatalf("%d tests above 575: %v", n, resultsStr(e))
	}
	sc := savedCurveNow()
	if len(sc.Curve) != 6 || sc.Confirmed == nil || *sc.Confirmed != (planMV{700, 8800, 9100}) || len(sc.Rejected) != 0 {
		t.Fatalf("saved %+v", sc)
	}
}

// 13–18. Stop at each phase: the miner always goes back to the baseline, nothing is confirmed, a cut map is
// never saved, a trim being tried is undone, a trim under check is put back.
func TestLogicStopAtEachPhase(t *testing.T) {
	for _, tc := range []struct {
		name      string
		when      func(f *fakeMiner) bool
		wantTrims string
		curve     bool // the map finished before the stop: its curve is saved
	}{
		{"baseline", func(f *fakeMiner) bool { return f.phase() == "baseline" && f.sinceApply() > 10 }, "", false},
		{"settle", func(f *fakeMiner) bool { return f.plan.MHz == 600 && f.sinceApply() > 1 }, "", false},
		{"watch", func(f *fakeMiner) bool { return f.plan.MHz == 575 && f.plan.MV == 8900 && f.sinceApply() > 15 }, "", false},
		{"trim retest", func(f *fakeMiner) bool { return f.plan.MHz == 600 && len(f.trims) > 0 && f.sinceApply() > 6 }, "", false},
		{"trim check", func(f *fakeMiner) bool {
			return strings.HasPrefix(planState(f.e, "trimcheck"), "run") && f.sinceApply() > 2
		}, "", true},
		{"confirm", func(f *fakeMiner) bool {
			return strings.HasPrefix(planState(f.e, "confirm"), "run") && f.sinceApply() > 20
		}, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, f := newFM(t, small())
			f.rules = []fmRule{needRule(ckey{2, 7}, always(600, 625), 0.6, -12.5)}
			f.stopWhen = tc.when
			runFM(t, e, f)
			dump(t, e)
			if !f.stopFired {
				t.Fatal("stop condition never met")
			}
			if lastApplied(f) != basePlan || e.confirmed {
				t.Fatalf("after a stop: last %v confirmed %v", lastApplied(f), e.confirmed)
			}
			if sortedTrims(f.trims) != tc.wantTrims {
				t.Fatalf("trims %v want %q (calls %v)", f.trims, tc.wantTrims, f.trimCalls)
			}
			sc := savedCurveNow()
			if (len(sc.Curve) > 0) != tc.curve || sc.Confirmed != nil {
				t.Fatalf("saved curve %+v", sc)
			}
			if st := planState(e, "finish"); !strings.HasPrefix(st, "cut: stopped by you") {
				t.Fatalf("finish %s", st)
			}
			if n := len(e.results); n > 0 && tc.name != "baseline" && tc.name != "settle" && e.results[n-1].Result != "stopped" {
				t.Fatalf("last result %+v", e.results[n-1])
			}
			for _, s := range e.steps {
				if s.State == "run" || s.State == "todo" {
					t.Fatalf("plan phase %q left %s", s.Label, s.State)
				}
			}
		})
	}
}

// 19. The top clock fails its confirm on a chip: the other settings at that clock are skipped, the next clock
// is confirmed; the saved curve records both.
func TestLogicConfirmFallbackSaved(t *testing.T) {
	e, f := newFM(t, small())
	f.rules = []fmRule{func(f *fakeMiner, k ckey) float64 {
		if k == (ckey{0, 3}) && f.plan.MHz == 625 && strings.HasPrefix(planState(f.e, "confirm"), "run") {
			return 0.6
		}
		return 0
	}}
	runFM(t, e, f)
	dump(t, e)
	sc := savedCurveNow()
	if len(sc.Rejected) != 1 || sc.Rejected[0] != (planMV{625, 8800, 9100}) || sc.Confirmed == nil || *sc.Confirmed != (planMV{600, 8800, 9100}) {
		t.Fatalf("saved %+v", sc)
	}
	if lastApplied(f) != (planMV{600, 8800, 9100}) || !e.confirmed {
		t.Fatalf("last %v", lastApplied(f))
	}
	// 625's other passes (9000, 9100) aren't confirmed: its chip failed there
	for _, p := range f.applied[len(f.applied)-3:] {
		if p.MHz == 625 && p.MV != 8800 {
			t.Fatalf("tried another 625 setting after its chip failed the confirm: %v", f.applied)
		}
	}
}

// 20. Nothing holds the confirm (a hashrate shortfall in every confirm): at most confirmCap candidates, each
// with one retest, all rejected; the miner goes back to the baseline.
func TestLogicConfirmNothingHolds(t *testing.T) {
	e, f := newFM(t, small())
	f.ghsMul = func(f *fakeMiner) float64 {
		if strings.HasPrefix(planState(f.e, "confirm"), "run") {
			return 0.85
		}
		return 1
	}
	runFM(t, e, f)
	dump(t, e)
	n := strings.Count(logStr(e), "  CONFIRM ")
	if n != confirmCap {
		t.Fatalf("%d confirm candidates, want %d", n, confirmCap)
	}
	if strings.Count(logStr(e), "retest the confirm of") != confirmCap {
		t.Fatal("each hashrate fail gets one retest")
	}
	sc := savedCurveNow()
	if len(sc.Rejected) != confirmCap || sc.Confirmed != nil || len(sc.Curve) != 3 {
		t.Fatalf("saved %+v", sc)
	}
	if lastApplied(f) != basePlan || e.confirmed {
		t.Fatalf("last %v", lastApplied(f))
	}
	if st := planState(e, "finish"); !strings.Contains(st, "nothing confirmed") {
		t.Fatalf("finish %s", st)
	}
}

// 21. Dry run: nothing is applied, trimmed or saved.
func TestLogicDryRun(t *testing.T) {
	c := small()
	c.DryRun = true
	e, f := newFM(t, c)
	f.rules = []fmRule{needRule(ckey{2, 7}, always(600, 625), 0.6, -12.5)}
	runFM(t, e, f)
	if len(f.applied) != 0 || len(f.trimCalls) != 0 || len(f.fans) != 0 {
		t.Fatalf("dry run changed the miner: %v %v %v", f.applied, f.trimCalls, f.fans)
	}
	if _, err := os.Stat(tunerCurvePath()); err == nil {
		t.Fatal("dry run saved a curve")
	}
	if st := planState(e, "finish"); !strings.Contains(st, "dry run") {
		t.Fatalf("finish %s", st)
	}
}

// 22. Confirm off: the run ends on the fastest pass that wasn't marginal (625 passed with its worst chip at
// its limit), and saves that as the confirmed setting.
func TestLogicConfirmOffSkipsMarginal(t *testing.T) {
	c := small()
	c.ConfirmWin = false
	c.ChipTrim = false
	e, f := newFM(t, c)
	b := bumpChip(ckey{0, 3}, 1)
	events(f, evt{625, 8800, 1, 10, b}, evt{625, 8800, 1, 20, b})
	runFM(t, e, f)
	dump(t, e)
	if e.marginal[[2]int{625, 8800}] == "" {
		t.Fatalf("625/8800 not marginal: %v", e.marginal)
	}
	if lastApplied(f) != (planMV{625, 8900, 9200}) && lastApplied(f) != (planMV{600, 8800, 9100}) {
		t.Fatalf("last %v", lastApplied(f))
	}
	sc := savedCurveNow()
	if sc.Confirmed == nil || *sc.Confirmed != lastApplied(f) {
		t.Fatalf("saved %+v", sc)
	}
	mustContain(t, e, "confirm off: ending on")
}

// 23. A fail late in the test is retested; a clean retest that doesn't make both tests together pass still
// fails the setting (combinedOK), and the clock gets more voltage.
func TestLogicLateFailCombined(t *testing.T) {
	c := small()
	c.HighestMHz, c.ChipTrim = 600, false
	for _, tc := range []struct {
		retestErrs int
		want       string
	}{{2, "600/9100 failed, 600/9100 clean, 600/9200 clean"}, {0, "600/9100 failed, 600/9100 clean"}} {
		e, f := newFM(t, c)
		b := bumpChip(ckey{0, 3}, 1)
		evs := []evt{{600, 8800, 1, 19, b}, {600, 8800, 1, 21, b}, {600, 8800, 1, 23, b}}
		for i := 0; i < tc.retestErrs; i++ {
			evs = append(evs, evt{600, 8800, 2, 10 + float64(i)*5, b})
		}
		events(f, evs...)
		runFM(t, e, f)
		dump(t, e)
		got := strings.Join(resultsStr(e), ", ")
		if !strings.Contains(got, tc.want) {
			t.Fatalf("retest errs %d: %s\n%s", tc.retestErrs, got, tailLog(e, 30))
		}
	}
}

// 24. A chip far over its normal rate (but under its limit) makes a pass marginal; the confirm puts it last.
func TestLogicNoisyChipMarginal(t *testing.T) {
	c := small()
	c.ChipTrim = false
	e, f := newFM(t, c)
	// board 0 is noisy at baseline (one weak chip), so its quiet chips get a generous limit
	events(f, evt{615, 9100, 1, 10, bumpChip(ckey{0, 3}, 1)}) // one error at baseline: a limit of 6
	b := bumpChip(ckey{0, 3}, 1)
	events(f, evt{625, 8800, 1, 8, b}, evt{625, 8800, 1, 12, b}, evt{625, 8800, 1, 16, b}, evt{625, 8800, 1, 20, b})
	runFM(t, e, f)
	dump(t, e)
	why := e.marginal[[2]int{625, 8800}]
	if !strings.Contains(why, "chip b0.c3 made 4 errors") {
		t.Fatalf("marginal %v\n%s", e.marginal, tailLog(e, 30))
	}
	// it ranks as one clock slower: after 600's clean passes, not first
	mustContain(t, e, "confirm candidate 625 MHz / PV 9100 (mV 8800) is marginal")
	if c := strings.Index(logStr(e), "CONFIRM "); c < 0 || strings.HasPrefix(logStr(e)[c:], "CONFIRM 625 MHz") {
		t.Fatalf("the marginal 625 was confirmed first:\n%s", tailLog(e, 20))
	}
	if lastApplied(f) == (planMV{625, 8800, 9100}) {
		t.Fatal("the marginal pass was confirmed first")
	}
}

// 25. No temperature reading: the run waits out the backend wait + 2 min, then stops itself.
func TestLogicNoTemperatureAbort(t *testing.T) {
	e, f := newFM(t, small())
	f.temp = func(f *fakeMiner) (float64, bool) {
		if f.plan.MHz == 600 && f.sinceApply() > 5 {
			return 0, false
		}
		return 70, true
	}
	runFM(t, e, f)
	if !strings.Contains(e.abortWhy, "no board temperature") || lastApplied(f) != basePlan {
		t.Fatalf("abort %q last %v", e.abortWhy, lastApplied(f))
	}
	r := e.results[len(e.results)-1]
	if r.MinS < 6.5 || r.MinS > 8 { // 3 min backend wait + 2, from 2 min into the watch
		t.Fatalf("aborted after %.1f min", r.MinS)
	}
}

// 26. A chip trimmed by hand is never touched by the tuner.
func TestLogicHandTrimRefused(t *testing.T) {
	c := small()
	c.HighestMHz = 600
	e, f := newFM(t, c)
	f.trims[[2]int{2, 7}] = -12.5
	f.rules = []fmRule{needRule(ckey{2, 7}, map[int]int{600: 8900}, 0.6, -25)}
	runFM(t, e, f)
	mustContain(t, e, "b2.c7 has a trim set by hand")
	if len(f.trimCalls) != 0 || f.trims[[2]int{2, 7}] != -12.5 {
		t.Fatalf("hand trim changed: %v %v", f.trimCalls, f.trims)
	}
}

// 27. margin() scales with the test length; HashTol is at least 6 %.
func TestLogicMarginAndHashTol(t *testing.T) {
	e, _ := newFM(t, small())
	if e.margin(25.0/60) != 2 || e.margin(50.0/60) != 4 || e.margin(10.0/60) != 2 {
		t.Fatalf("margin %g %g %g", e.margin(25.0/60), e.margin(50.0/60), e.margin(10.0/60))
	}
	for _, tc := range []struct {
		tol, mul float64
		fail     bool
	}{{0.04, 0.95, false}, {0.04, 0.93, true}, {0.10, 0.92, false}, {0.10, 0.89, true}} {
		c := small()
		c.HighestMHz, c.HashTol, c.ConfirmWin = 575, tc.tol, false
		e, f := newFM(t, c)
		f.ghsMul = func(f *fakeMiner) float64 {
			if f.plan.MHz == 575 && f.plan.MV == 9000 {
				return tc.mul
			}
			return 1
		}
		runFM(t, e, f)
		failed := strings.Contains(strings.Join(resultsStr(e), ","), "575/9300 failed")
		if failed != tc.fail {
			t.Fatalf("HashTol %g at %.0f%%: failed=%v %v", tc.tol, tc.mul*100, failed, resultsStr(e))
		}
	}
}

// 28. Config validation: the hardware ranges.
func TestLogicConfigValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		mod  func(c *tunerConfig)
		ok   bool
	}{
		{"user's run", func(c *tunerConfig) {}, true},
		{"step 12.5", func(c *tunerConfig) { c.Step = 12.5 }, true},
		{"step 37.5", func(c *tunerConfig) { c.Step = 37.5 }, true},
		{"step 10", func(c *tunerConfig) { c.Step = 10 }, false},
		{"step 30", func(c *tunerConfig) { c.Step = 30 }, false},
		{"step 112.5", func(c *tunerConfig) { c.Step = 112.5 }, false},
		{"PV floor 9050", func(c *tunerConfig) { c.MinMV = 8750 }, false},
		{"PV ceiling 9650", func(c *tunerConfig) { c.MaxMV = 9350 }, false},
		{"gap 400 → PV 9700", func(c *tunerConfig) { c.PVGap = 400 }, false},
		{"gap 200 → PV 9000 floor", func(c *tunerConfig) { c.PVGap = 200 }, false},
		{"gap 200, 8900–9300", func(c *tunerConfig) { c.PVGap, c.MinMV = 200, 8900 }, true},
		{"min > max", func(c *tunerConfig) { c.MinMV, c.MaxMV = 9100, 9000 }, false},
		{"baseline PV 9650", func(c *tunerConfig) { c.BaselineMV = 9350 }, false},
		{"confirm mode PV 9700", func(c *tunerConfig) { c.Mode, c.ConfirmMV = "confirm", 9400 }, false},
		{"confirm mode mV 8700", func(c *tunerConfig) { c.Mode, c.ConfirmMV = "confirm", 8700 }, false},
		{"trim max 0", func(c *tunerConfig) { c.ChipTrimMax = 0 }, false},
		{"trim max 11", func(c *tunerConfig) { c.ChipTrimMax = 11 }, false},
		{"abort 91", func(c *tunerConfig) { c.AbortC = 91 }, false},
		{"fan target too close to abort", func(c *tunerConfig) { c.FanTargetC = 84 }, false},
		{"mv step 5", func(c *tunerConfig) { c.MVStep = 5 }, false},
		{"warmest > abort-12", func(c *tunerConfig) { c.BuildPresets, c.WarmestC = true, 80 }, false},
		{"clock 750 locked", func(c *tunerConfig) { c.HighestMHz = 750 }, false},
	} {
		c := scfg()
		tc.mod(&c)
		if err := validateTunerConfig(c); (err == nil) != tc.ok {
			t.Errorf("%s: err=%v, want ok=%v", tc.name, err, tc.ok)
		}
	}
}

// 29. Trims made by a run that is then stopped: they come off again with the clock and voltage (nothing goes to
// auto-trim), so the miner is exactly as it was before the run.
func TestLogicStoppedRunUndoesTrims(t *testing.T) {
	e, f := newFM(t, small())
	f.rules = []fmRule{needRule(ckey{2, 7}, always(600, 625), 0.6, -12.5)}
	var adopted []chipTrim
	e.adoptTrims = func(ts []chipTrim, _ string) { adopted = ts }
	f.stopWhen = func(f *fakeMiner) bool { return f.plan.MHz == 625 && f.sinceApply() > 10 }
	runFM(t, e, f)
	if lastApplied(f) != basePlan || sortedTrims(f.trims) != "" || len(adopted) != 0 { // a stop puts the trims back too
		t.Fatalf("last %v trims %v adopted %v", lastApplied(f), f.trims, adopted)
	}
}

// 30. Confirm mode: rounds, a failed round's retest, CONFIRMED stays / REJECTED goes back.
func TestLogicConfirmMode(t *testing.T) {
	for _, tc := range []struct {
		badRound int // 0: none
		bothBad  bool
		want     bool
	}{{0, false, true}, {2, false, true}, {2, true, false}} {
		c := scfg()
		c.Mode, c.ConfirmMHz, c.ConfirmMV, c.ConfirmHours = "confirm", 625, 8900, 1.25 // 3 rounds of 25
		e, f := newFM(t, c)
		p := planMV{625, 8900, 9200}
		if tc.badRound > 0 {
			// 3 errors late in the round (over its limit of 2, not a decisive fail)
			evs := []evt{{625, 8900, tc.badRound, 22, bumpChip(ckey{1, 1}, 3)}}
			if tc.bothBad {
				evs = append(evs, evt{625, 8900, tc.badRound + 1, 22, bumpChip(ckey{1, 1}, 3)})
			}
			events(f, evs...)
		}
		runFM(t, e, f)
		dump(t, e)
		if e.confirmed != tc.want {
			t.Fatalf("%+v: confirmed %v\n%s", tc, e.confirmed, tailLog(e, 20))
		}
		if tc.want && lastApplied(f) != p || !tc.want && lastApplied(f) != basePlan {
			t.Fatalf("%+v: last %v", tc, lastApplied(f))
		}
	}
}

// 31. The clocks a 12.5-step map sends are each the request for its own 12.5 step (588 runs 587.5).
func TestLogicClockGrid(t *testing.T) {
	c := small()
	c.Step, c.LowestMHz, c.HighestMHz = 12.5, 575, 625
	e, f := newFM(t, c)
	runFM(t, e, f) // the harness checks every clock sent
	seen := map[float64]bool{}
	for _, p := range e.curve {
		if seen[pll(float64(p.MHz))] {
			t.Fatalf("two curve points run the same clock: %v", e.curve)
		}
		seen[pll(float64(p.MHz))] = true
	}
	if fmt.Sprint(c.clockList()) != "[575 588 600 613 625]" {
		t.Fatal(c.clockList())
	}
}
