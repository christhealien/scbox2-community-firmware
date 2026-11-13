package main

import (
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// planDump: the run plan as text, "phase[state]: item[state] a1[state] a2[state] …".
func planDump(e *tunerEngine) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var b strings.Builder
	for _, ph := range e.steps {
		fmt.Fprintf(&b, "%s[%s]", ph.Key, ph.State)
		for _, it := range ph.Kids {
			fmt.Fprintf(&b, " | %s[%s]", it.Label, it.State)
			for _, a := range it.Kids {
				fmt.Fprintf(&b, " %s[%s]", a.Label, a.State)
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}

func mapCfg() tunerConfig {
	cfg := fastCfg()
	cfg.LowestMHz, cfg.HighestMHz, cfg.Step = 575, 650, 25
	cfg.MaxMV, cfg.MinMV, cfg.MVStep = 9300, 8800, 100
	cfg.HoldMin = 2
	return cfg
}

// Too hot on the way down the voltages: the whole run stops there (no more clocks, no confirm, no presets)
// and the miner goes back to what it ran before.
func TestAbortStopsTheRun(t *testing.T) {
	cfg := mapCfg()
	cfg.BuildPresets = true
	cfg.BaselineMHz, cfg.BaselineMV = 600, 9100
	e, s := newSimEngine(cfg, 8900, ckey{2, 5})
	hot := false
	apply := e.applyPlan
	e.applyPlan = func(p planMV) error {
		if p.MHz == 575 && p.MV == 9000 {
			hot = true // the second test of the curve overheats
		}
		return apply(p)
	}
	e.readTemp = func() (float64, bool) {
		if hot {
			return 95, true
		}
		return 65, true
	}
	e.Run()
	if !e.aborted() || !strings.Contains(e.abortReason(), "95") {
		t.Fatalf("not aborted: %q", e.abortReason())
	}
	for _, r := range e.results {
		if r.MHz > 575 {
			t.Fatalf("tested %d MHz after the safety stop", r.MHz)
		}
	}
	d := planDump(e)
	if !strings.Contains(d, "presets[skip]") || !strings.Contains(d, "confirm[skip]") || !strings.Contains(d, "finish[cut]") {
		t.Fatalf("plan:\n%s", d)
	}
	if last := s.applied[len(s.applied)-1]; last.MHz != 600 || last.MV != 9100 {
		t.Fatalf("not back on the baseline: %v", last)
	}
}

// The baseline's own setting isn't tested again in the curve; a retest shows as its own attempt.
func TestBaselineReusedAndRetestShown(t *testing.T) {
	cfg := mapCfg()
	cfg.BaselineMHz, cfg.BaselineMV = 600, 8900 // 600 MHz starts at 575's lowest clean voltage, 8900
	cfg.HoldMin = 25                            // a slow failure (the hashrate, judged at the end): retested
	e, _ := newSimEngine(cfg, 8900, ckey{2, 5})
	e.Run()
	for _, r := range e.results {
		if r.MHz == 600 && r.MV == 8900 && r.MinS < 1.5*cfg.HoldMin { // a confirm runs twice as long
			t.Fatalf("the baseline setting was tested again: %+v", r)
		}
	}
	d := planDump(e)
	if !strings.Contains(d, "PV 9200 (the baseline)[pass]") {
		t.Fatalf("baseline not shown as reused:\n%s", d)
	}
	t.Log("\n" + d)
}

// The fastest clean setting doesn't hold in its confirm: the next one is confirmed, and the run ends on it.
func TestConfirmFallsBack(t *testing.T) {
	cfg := mapCfg()
	cfg.HighestMHz = 625
	e, s := newSimEngine(cfg, 8800, ckey{2, 5})
	apply := e.applyPlan
	confirming := false
	e.applyPlan = func(p planMV) error {
		s.mu.Lock()
		s.failAboveMHz = 99999
		if confirming && p.MHz == 625 {
			s.failAboveMHz = 600 // 625 doesn't hold in its longer confirm
		}
		s.mu.Unlock()
		return apply(p)
	}
	e.onStart = func() {}
	// switch to "confirming" once the curve is done
	oldEmit := e.emit
	e.emit = func(m string) {
		if strings.HasPrefix(m, "CONFIRM ") {
			confirming = true
		}
		if oldEmit != nil {
			oldEmit(m)
		}
	}
	e.Run()
	if !e.confirmed || e.best == nil || e.best.MHz != 600 {
		t.Fatalf("best %+v confirmed %v\n%s", e.best, e.confirmed, planDump(e))
	}
	if last := s.applied[len(s.applied)-1]; last.MHz != 600 {
		t.Fatalf("not left on the confirmed 600: %v", last)
	}
	d := planDump(e)
	if !strings.Contains(d, "625 MHz · PV 9100[fail] confirm[fail]") || !strings.Contains(d, "600 MHz · PV 9100[pass] confirm[pass]") {
		t.Fatalf("confirm plan:\n%s", d)
	}
}

// Presets as blake2b-asic-control builds them: High = the confirmed setting (its confirm reused), Lowest =
// the first clock, Middle and Low a third and two thirds of the way down; only passing presets saved.
func TestPresetsLikeAsicControl(t *testing.T) {
	*presetPath = filepath.Join(t.TempDir(), "presets.json")
	cfg := mapCfg()
	cfg.LowestMHz, cfg.HighestMHz = 575, 675
	cfg.BuildPresets = true
	cfg.FanHold, cfg.FanTargetC, cfg.WarmestC, cfg.AbortC = true, 60, 70, 88
	cfg.DryRun = false
	e, _ := newSimEngine(cfg, 8800, ckey{2, 5})
	e.applyFan = func(int) error { return nil }
	e.Run()
	d := planDump(e)
	ps := loadPresets()
	got := map[string]string{}
	for _, p := range ps {
		got[p.Name] = fmt.Sprintf("%d/%d@%d", p.MHz, p.MV, p.TargetC)
	}
	// the sim's 675 never holds its hashrate: the curve tops out at 650
	want := map[string]string{"High": "650/8800@60", "Middle": "625/8800@63", "Low": "600/8800@67", "Lowest power": "575/8800@70"}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s: got %s want %s\nall %v\n%s", k, got[k], v, got, d)
		}
	}
	if !strings.Contains(d, "High[pass] 650 MHz · PV 9100 (confirm test)[pass]") {
		t.Fatalf("High didn't reuse its confirm:\n%s", d)
	}
}

// A setting the miner doesn't take is applied again; one it never takes stops the run.
func TestSettingMustTake(t *testing.T) {
	cfg := mapCfg()
	cfg.DryRun = false
	e, _ := newSimEngine(cfg, 8800, ckey{2, 5})
	calls := 0
	e.verifyPlan = func(p planMV) string {
		calls++
		if calls == 1 {
			return "it runs 615 MHz"
		}
		return ""
	}
	if err := e.settle(planMV{MHz: 575, MV: 9000, PV: 9300}, 0); err != nil {
		t.Fatalf("a late take should pass: %v", err)
	}
	e.verifyPlan = func(planMV) string { return "it runs 615 MHz" }
	if err := e.settle(planMV{MHz: 575, MV: 9000, PV: 9300}, 0); err == nil || !strings.Contains(err.Error(), "didn't take") {
		t.Fatalf("never taken: %v", err)
	}
}

// No temperature for 10 readings in a row: the run stops (no heat abort, no fan hold otherwise).
func TestNoTemperatureStops(t *testing.T) {
	cfg := mapCfg()
	e, _ := newSimEngine(cfg, 8800, ckey{2, 5})
	e.readTemp = func() (float64, bool) { return 0, false }
	e.Run()
	if !strings.Contains(e.abortReason(), "no board temperature") {
		t.Fatalf("abort: %q", e.abortReason())
	}
}

// The fan hold (blake2b-asic-control's FanHold): full fan 8 °C over, up at most 5 a reading (10 when
// more than 3 over), down at most 2 and not while still warming, back to its steady level after full.
func TestFanHoldController(t *testing.T) {
	h := fanHold{init: true, base: 50}
	if w := h.want(71, 70, 50, 20, 100); w != 53 {
		t.Fatalf("1 °C over: %d", w)
	}
	h = fanHold{init: true, base: 50}
	if w := h.want(75, 70, 50, 20, 100); w != 60 {
		t.Fatalf("5 °C over: %d (want +10)", w)
	}
	if w := h.want(79, 70, 60, 20, 100); w != 100 {
		t.Fatalf("9 °C over: %d (want full)", w)
	}
	h = fanHold{init: true, base: 50}
	h.want(70, 70, 50, 20, 100) // steady at 50
	h.want(80, 70, 50, 20, 100) // full
	if w := h.want(71, 70, 100, 20, 100); w != 94 {
		t.Fatalf("off full: %d (want 100-6, at least its steady 50)", w)
	}
	h = fanHold{init: true, base: 50}
	h.want(65, 70, 50, 20, 100)
	if w := h.want(66, 70, 50, 20, 100); w != 50 {
		t.Fatalf("below target but warming: %d (want unchanged)", w)
	}
}

// An owner's measurements: the boards read about PV − 80 mV and never below ~8.96 V (from PV 9000 down).
// The tuner stops lowering the voltage when the reading stops falling, and doesn't go below it later.
func TestVoltageFloor(t *testing.T) {
	cfg := mapCfg()
	cfg.LowestMHz, cfg.HighestMHz = 575, 600
	cfg.BaselineMHz, cfg.BaselineMV = 625, 9100
	cfg.MinMV = 8400
	e, s := newSimEngine(cfg, 0, ckey{2, 5}) // clean at any voltage
	e.readVolt = func() (float64, bool) {
		s.mu.Lock()
		defer s.mu.Unlock()
		pv := float64(s.mv + cfg.PVGap)
		return math.Max(pv, 9000) * 9320 / 9400, true
	}
	e.Run()
	var tested []string
	for _, r := range e.results {
		tested = append(tested, fmt.Sprintf("%d/%d", r.MHz, r.MV))
	}
	got := strings.Join(tested, " ")
	if e.floorPV != 9000 || strings.Contains(got, "575/8500") || strings.Contains(got, "600/8600") {
		t.Fatalf("floor %d, tests %s", e.floorPV, got)
	}
	if len(e.curve) == 0 || e.curve[0].MV != 8700 {
		t.Fatalf("575's lowest should be 8700 (PV 9000), curve %+v", e.curve)
	}
	if !strings.Contains(planDump(e), "PV 8900[pass]") {
		t.Logf("plan:\n%s", planDump(e))
	}
}

// The default voltage step is 50 mV (each step moves PV 50 mV too): the curve lands within 50 mV of what
// each clock needs.
func TestFiftyMillivoltSteps(t *testing.T) {
	cfg := mapCfg()
	cfg.MVStep = defaultTunerConfig().MVStep
	if cfg.MVStep != 50 {
		t.Fatalf("default step %d", cfg.MVStep)
	}
	cfg.LowestMHz, cfg.HighestMHz = 575, 625
	cfg.BaselineMHz, cfg.BaselineMV = 615, 9100
	need := map[int]int{575: 8850, 600: 8950, 625: 9150}
	e, s := newSimEngine(cfg, 0, ckey{2, 5})
	apply := e.applyPlan
	e.applyPlan = func(p planMV) error {
		s.mu.Lock()
		s.failBelowMV = need[p.MHz]
		s.ghs = 1150 * pll(float64(p.MHz)) / pll(575)
		s.mu.Unlock()
		return apply(p)
	}
	e.Run()
	got := fmt.Sprint(e.curve)
	if len(e.curve) != 3 || e.curve[0].MV != 8850 || e.curve[1].MV != 8950 || e.curve[2].MV != 9150 {
		t.Fatalf("curve %s", got)
	}
}

// The supply only delivers PV 9100–9600 (measured): a map below the floor is refused with the reason.
func TestTunerPVFloorValidation(t *testing.T) {
	c := defaultTunerConfig()
	if err := validateTunerConfig(c); err != nil {
		t.Fatalf("defaults (PV %d–%d) refused: %v", c.pvFor(c.MinMV), c.pvFor(c.MaxMV), err)
	}
	c.MinMV = 8700 // PV 9000
	if err := validateTunerConfig(c); err == nil || !strings.Contains(err.Error(), "floor") {
		t.Fatalf("PV 9000 should be refused for the floor, got %v", err)
	}
	c = defaultTunerConfig()
	c.MaxMV = 9400 // PV 9700
	if err := validateTunerConfig(c); err == nil || !strings.Contains(err.Error(), "9600") {
		t.Fatalf("PV 9700 should be refused, got %v", err)
	}
}

// The OEM's backend didn't answer once, right after a setting settled: the run waited for it instead of
// ending (a real run aborted on one missed reading and then couldn't put the miner back).
func TestBackendDropIsWaitedFor(t *testing.T) {
	cfg := mapCfg()
	cfg.BaselineMHz, cfg.BaselineMV = 600, 9100
	e, _ := newSimEngine(cfg, 8900, ckey{2, 5})
	read := e.readErrors
	down := 0
	apply := e.applyPlan
	e.applyPlan = func(p planMV) error {
		if p.MHz == 575 && p.MV == 9000 {
			down = 4 // the next 4 readings fail (about a minute)
		}
		return apply(p)
	}
	e.readErrors = func() (map[ckey]int, error) {
		if down > 0 {
			down--
			return nil, fmt.Errorf("the miner's backend (the OEM's minerd) didn't answer: it may be restarting")
		}
		return read()
	}
	e.Run()
	if e.aborted() {
		t.Fatalf("aborted on a short backend drop: %s", e.abortReason())
	}
	if !strings.Contains(strings.Join(e.fullLog, "\n"), "the backend answers again") {
		t.Fatal("the wait isn't logged")
	}
}

// The mining program crashed at a setting (stopped and restarted while the backend was away): that setting
// fails, the run goes on.
func TestCrashAtSettingFailsIt(t *testing.T) {
	cfg := mapCfg()
	cfg.BaselineMHz, cfg.BaselineMV = 600, 9100
	e, _ := newSimEngine(cfg, 8700, ckey{2, 5})
	read := e.readErrors
	crashAt := planMV{}
	down := 0
	apply := e.applyPlan
	e.applyPlan = func(p planMV) error {
		if p.MHz == 575 && p.MV == 9000 {
			down, crashAt = 2, p
		}
		return apply(p)
	}
	e.readErrors = func() (map[ckey]int, error) {
		if down > 0 {
			down--
			return nil, fmt.Errorf("the miner's backend (the OEM's minerd) didn't answer: x")
		}
		return read()
	}
	e.miningStops = func(since time.Time) int {
		if crashAt.MHz > 0 {
			return 1
		}
		return 0
	}
	e.Run()
	if e.aborted() {
		t.Fatalf("aborted: %s", e.abortReason())
	}
	found := false
	for _, r := range e.results {
		if r.MHz == 575 && r.MV == 9000 && strings.Contains(r.Reason, "crashed") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the crash isn't a failed test: %+v", e.results)
	}
}

// Putting the miner back at the end is retried while the backend doesn't answer.
func TestRestoreRetried(t *testing.T) {
	cfg := mapCfg()
	cfg.BaselineMHz, cfg.BaselineMV = 600, 9100
	e, _ := newSimEngine(cfg, 8900, ckey{2, 5})
	e.restoreWait = 5 * time.Minute
	hot, fails := false, 0
	apply := e.applyPlan
	e.applyPlan = func(p planMV) error {
		if e.aborted() && fails < 3 {
			fails++
			return fmt.Errorf("the miner's backend (the OEM's minerd) didn't answer: x")
		}
		if p.MHz == 575 && p.MV == 9000 {
			hot = true
		}
		return apply(p)
	}
	e.readTemp = func() (float64, bool) {
		if hot {
			return 95, true
		}
		return 65, true
	}
	e.Run()
	if !e.aborted() || fails != 3 || e.restoreFailed {
		t.Fatalf("aborted %v, fails %d, restoreFailed %v", e.aborted(), fails, e.restoreFailed)
	}
}
