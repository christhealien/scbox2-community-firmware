package main

// tuner-sweep/r6-tuner: whole-firmware pass over the tuner and chip-trim features.

import (
	"os"
	"strings"
	"testing"
	"time"
)

// F1: the watchdog ends a stuck run (tunerForceEnd) and clears its restart marker, but leaves the chip trims the
// run made (kept ones and the one being tried) in the trims file: they apply at the baseline it puts back and
// at every later clock change, and nothing undoes them (the stuck run's trimSet is a no-op now, the marker that
// would let tunerRecover undo them is gone). A Stop or a restart both undo them.
func TestR6ForceEndLeavesRunTrims(t *testing.T) {
	old := *authFile
	*authFile = t.TempDir() + "/auth.json"
	defer func() { *authFile = old }()
	w := fastWatchdog(t)
	_ = w
	cwSetTrim(1, 7, -25) // a hand trim from before the run: stays
	e, _ := newSimEngine(fastCfg(), 8900, ckey{2, 5})
	base := e.resolveBaseline()
	e.baseAtom.Store(&base)
	writeTunerMarker(e)
	cwSetTrim(2, 5, -12.5) // kept by the run
	cwSetTrim(2, 9, -25)   // being tried when it froze
	e.runTrims = map[[2]int]float64{{2, 5}: -12.5}
	setCharge("tuner")
	tunerForceEnd(e, "test")
	got := map[[2]int]float64{}
	for _, tr := range readChipTrims() {
		got[[2]int{tr.Chain, tr.Chip}] = tr.MHz
	}
	if got[[2]int{1, 7}] != -25 {
		t.Errorf("the hand trim from before the run was changed: %v", got)
	}
	if len(got) != 1 {
		t.Fatalf("the stuck run was ended but its chip trims stay in the file: %v", got)
	}
}

// F2: a "+" hand trim is only checked against the clock window when it is set. Raising the board clock later
// (Controls, a preset, the schedule, the tuner) runs the chip past the window: +50 set at 650 MHz, then
// 700 MHz in Controls = that chip at 750 MHz with the clock limits locked at 700.
func TestR6PlusTrimPastWindowAfterClockRaise(t *testing.T) {
	st := r4Setup(t, `{"manual":true,"tempcontrol":true,"select":0,"manualPowerplan":"650 MHz 9200 V 60 RPM 60 RPM PV 9500",
 "powerplans":[{"info":"615 MHz 9100 V 40 RPM 40 RPM PV 9400","level":0},{"info":"0 MHz 0 V 100 RPM 100 RPM","level":3}]}`)
	os.WriteFile(chipHookFlag(), []byte("on\n"), 0o644) // chip trims on
	cwSetTrim(2, 11, 50)                                // allowed at 650: 700
	t.Run("controls", func(t *testing.T) {
		res := r4Call("POST", `{"freq_mhz":700,"vrun_mv":9200,"pv_mv":9500,"fan1":60,"fan2":60,"confirm":true}`)
		if res["ok"] == true {
			t.Errorf("Controls set 700 MHz with b2.c11 at +50: that chip runs %d MHz, window %d (%v)", st.running().FreqMHz+50, clockWindowHi(), res)
		}
	})
	t.Run("mutatePlan", func(t *testing.T) { // the tuner, presets, the schedule
		if _, err := mutatePlan(func(p *powerplan) { p.FreqMHz = 700 }); err == nil {
			t.Errorf("mutatePlan set 700 MHz with b2.c11 at +50 (chip at 750, window %d)", clockWindowHi())
		}
	})
	t.Run("tuner config", func(t *testing.T) {
		c := defaultTunerConfig()
		c.Mode, c.LowestMHz, c.HighestMHz, c.Step, c.DryRun = "map", 600, 700, 25, false
		if err := validateTunerConfig(c); err == nil {
			t.Errorf("a map run up to 700 MHz is accepted with b2.c11 at +50 (it would run that chip at 750)")
		}
	})
}

// F2 (no false refusals): lowering the clock, a "+" that fits, and the stock miner (trims off) all go through.
func TestR6PlusTrimFitsOrOff(t *testing.T) {
	st := r4Setup(t, `{"manual":true,"tempcontrol":true,"select":0,"manualPowerplan":"650 MHz 9200 V 60 RPM 60 RPM PV 9500",
 "powerplans":[{"info":"615 MHz 9100 V 40 RPM 40 RPM PV 9400","level":0},{"info":"0 MHz 0 V 100 RPM 100 RPM","level":3}]}`)
	cwSetTrim(2, 11, 50)
	if res := r4Call("POST", `{"freq_mhz":700,"vrun_mv":9200,"pv_mv":9500,"fan1":60,"fan2":60,"confirm":true}`); res["ok"] != true {
		t.Fatalf("trims off (stock miner): 700 refused: %v", res)
	}
	os.WriteFile(chipHookFlag(), []byte("on\n"), 0o644)
	if _, err := mutatePlan(func(p *powerplan) { p.FreqMHz = 650 }); err != nil {
		t.Fatalf("lowering the clock refused: %v", err)
	}
	if res := r4Call("POST", `{"freq_mhz":650,"vrun_mv":9200,"pv_mv":9500,"fan1":60,"fan2":60,"confirm":true}`); res["ok"] != true {
		t.Fatalf("650 + 50 = 700 refused: %v", res)
	}
	_ = st
}

// F1 (no new hang): the watchdog must still end a run frozen while it holds chipTrimMu (the v1.5.15 deadlock):
// putting the trims back gives up after a few seconds and says so.
func TestR6ForceEndWithTrimLockHeld(t *testing.T) {
	old := *authFile
	*authFile = t.TempDir() + "/auth.json"
	defer func() { *authFile = old }()
	fastWatchdog(t)
	e, _ := newSimEngine(fastCfg(), 8900, ckey{2, 5})
	base := e.resolveBaseline()
	e.baseAtom.Store(&base)
	writeTunerMarker(e)
	cwSetTrim(2, 5, -12.5)
	chipTrimMu.Lock()
	defer chipTrimMu.Unlock()
	setCharge("tuner")
	done := make(chan struct{})
	go func() { tunerForceEnd(e, "test"); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("the watchdog hangs on chipTrimMu")
	}
	if w := e.deadWhy.Load(); w == nil || !strings.Contains(*w, "couldn't be taken off") {
		t.Fatalf("not told: %v", w)
	}
	if getCharge() == "tuner" {
		t.Fatal("still in charge")
	}
}
