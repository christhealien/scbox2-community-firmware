package main

import (
	"strings"
	"testing"
	"time"
)

// one weak chip (icinfo b2 index 5 = hook chain 2 chip 6) makes 625 MHz need 9000 mV; slowed 25 MHz it
// would be clean down to 8800 mV (helps=true), or trims don't help it at all (helps=false).
func trimRun(t *testing.T, helps, on bool) (*tunerEngine, map[[2]int]float64) {
	cfg := fastCfg()
	cfg.ChipTrim, cfg.ChipTrimMax = on, 3
	cfg.MinMV, cfg.MaxMV, cfg.MVStep = 8800, 9300, 100
	cfg.HoldMin = 2
	e, s := newSimEngine(cfg, 9000, ckey{2, 5})
	trims := map[[2]int]float64{}
	e.trimGet = func() map[[2]int]float64 {
		m := map[[2]int]float64{}
		for k, v := range trims {
			m[k] = v
		}
		return m
	}
	e.trimSet = func(chain, chip int, mhz float64) error {
		if mhz == 0 {
			delete(trims, [2]int{chain, chip})
		} else {
			trims[[2]int{chain, chip}] = mhz
		}
		s.mu.Lock()
		s.failBelowMV = 9000
		if helps && trims[[2]int{2, 6}] <= -25 {
			s.failBelowMV = 8800
		}
		s.mu.Unlock()
		return nil
	}
	e.hookStats = func() (chipStatsFile, bool) {
		var f chipStatsFile
		f.Chains = append(f.Chains, struct {
			Chain   int   `json:"chain"`
			Chips   int   `json:"chips"`
			Base10  int   `json:"base10"`
			Clock10 []int `json:"clock10"`
			Nonces  []int `json:"nonces"`
		}{Chain: 2, Chips: 46, Base10: 6250})
		return f, true
	}
	adopted = nil
	e.adoptTrims = func(ts []chipTrim, why string) { adopted = ts }
	e.Run()
	return e, trims
}

var adopted []chipTrim

func TestTunerChipTrimHelps(t *testing.T) {
	e, trims := trimRun(t, true, true)
	if len(e.curve) != 1 || e.curve[0].MV != 8800 {
		t.Fatalf("with chip 6 slowed the board should be clean at 8800 mV: %+v\n%s", e.curve, strings.Join(e.fullLog, "\n"))
	}
	if len(trims) != 1 || trims[[2]int{2, 6}] != -25 {
		t.Fatalf("trims %v", trims)
	}
	if tl := trimList(e.runTrims); len(tl) != 1 || tl[0].Chip != 6 || tl[0].MHz != -25 {
		t.Fatalf("runTrims %v", tl)
	}
	if len(adopted) != 1 || adopted[0].Chain != 2 || adopted[0].Chip != 6 || adopted[0].MHz != -25 {
		t.Fatalf("the kept trim wasn't handed to auto-trim: %v", adopted)
	}
	log := strings.Join(e.fullLog, "\n")
	if !strings.Contains(log, "b2.c6 at -12.5 MHz: not clean") || !strings.Contains(log, "the trim is needed, back to -25 MHz") {
		t.Fatalf("the trim check should find -25 needed:\n%s", log)
	}
	if !strings.Contains(log, "slowing b2.c5 to -12.5 MHz") || !strings.Contains(log, "CHIP TRIMS kept by this run") {
		t.Fatalf("log:\n%s", log)
	}
}

func TestTunerChipTrimDoesntHelp(t *testing.T) {
	e, trims := trimRun(t, false, true)
	if len(e.curve) != 1 || e.curve[0].MV != 9000 {
		t.Fatalf("curve %+v", e.curve)
	}
	if len(trims) != 0 || len(e.runTrims) != 0 {
		t.Fatalf("trims that didn't help must be undone: %v %v", trims, e.runTrims)
	}
	if !strings.Contains(strings.Join(e.fullLog, "\n"), "isn't clean even with chip trims") {
		t.Fatal("no undo logged")
	}
}

func TestTunerChipTrimOff(t *testing.T) {
	e, trims := trimRun(t, true, false)
	if len(e.curve) != 1 || e.curve[0].MV != 9000 || len(trims) != 0 {
		t.Fatalf("off: curve %+v trims %v", e.curve, trims)
	}
}

// An owner's run: after a 615 MHz baseline, a 575 MHz test. The miner's own figure is an average reaching
// back before the test, drifting down for the whole test; the total-work counter shows the true rate.
func TestHashrateFromWorkCounter(t *testing.T) {
	cfg := fastCfg()
	cfg.BaselineMHz, cfg.BaselineMV = 625, 9100
	cfg.LowestMHz, cfg.HighestMHz, cfg.Step = 575, 575, 25
	cfg.HoldMin, cfg.BaselineMin, cfg.PollS = 20, 10, 15
	e, s := newSimEngine(cfg, 0, ckey{2, 5})
	rate := func() float64 { return 4700 * pll(float64(s.mhz)) / pll(625) } // GH/s, follows the clock at once
	var work, lagged float64                                                // MH; the miner's lagging average, GH/s
	sleep := e.sleep
	e.sleep = func(d time.Duration) {
		sleep(d)
		s.mu.Lock()
		r := rate()
		s.mu.Unlock()
		work += r * 1000 * d.Seconds()
		if lagged == 0 {
			lagged = r
		}
		lagged += (r - lagged) * d.Seconds() / 3600 // an hour-long average: barely moves in a test
	}
	e.readGHS = func() (float64, bool) { return lagged, true }
	e.readWork = func() (float64, bool) { return work, true }
	e.Run()
	if len(e.results) == 0 {
		t.Fatal("no test ran")
	}
	r := e.results[0]
	want := 4700 * 575.0 / 625
	if r.MHz != 575 || r.THS*1000 < want*0.99 || r.THS*1000 > want*1.01 {
		t.Fatalf("575 MHz test measured %.1f GH/s, the chips did %.1f (the lagging figure was %.1f)", r.THS*1000, want, lagged)
	}
}

func simTrims(e *tunerEngine, s *simState, onChange func(map[[2]int]float64)) map[[2]int]float64 {
	trims := map[[2]int]float64{}
	e.trimGet = func() map[[2]int]float64 {
		m := map[[2]int]float64{}
		for k, v := range trims {
			m[k] = v
		}
		return m
	}
	e.trimSet = func(chain, chip int, mhz float64) error {
		if mhz == 0 {
			delete(trims, [2]int{chain, chip})
		} else {
			trims[[2]int{chain, chip}] = mhz
		}
		onChange(trims)
		return nil
	}
	e.hookStats = func() (chipStatsFile, bool) {
		var f chipStatsFile
		f.Chains = append(f.Chains, struct {
			Chain   int   `json:"chain"`
			Chips   int   `json:"chips"`
			Base10  int   `json:"base10"`
			Clock10 []int `json:"clock10"`
			Nonces  []int `json:"nonces"`
		}{Chain: 2, Chips: 46, Base10: 6250})
		return f, true
	}
	e.adoptTrims = nil
	return trims
}

// A chip that's bad already in the baseline is judged against its own baseline, so it doesn't fail tests
// and the tuner doesn't trim it (auto-trim, which compares chips with the rest of the board, does).
func TestTunerLeavesBaselineBadChip(t *testing.T) {
	cfg := fastCfg()
	cfg.ChipTrim, cfg.ChipTrimMax = true, 3
	cfg.BaselineMin, cfg.HoldMin = 20, 2
	e, s := newSimEngine(cfg, 0, ckey{2, 8})
	trims := simTrims(e, s, func(map[[2]int]float64) {})
	sleep := e.sleep
	e.sleep = func(d time.Duration) {
		sleep(d)
		s.mu.Lock()
		s.counts[ckey{2, 8}] += 2 * d.Minutes()
		s.mu.Unlock()
	}
	e.Run()
	if len(trims) != 0 || len(e.curve) != 1 || e.curve[0].MV != 8800 {
		t.Fatalf("trims %v curve %+v", trims, e.curve)
	}
}

// B: a trim only one clock needed (or one made after bad luck) is lifted at the best setting.
func TestTunerTrimCheckLifts(t *testing.T) {
	cfg := fastCfg()
	cfg.ChipTrim, cfg.ChipTrimMax = true, 3
	cfg.LowestMHz, cfg.HighestMHz, cfg.Step = 600, 625, 25
	cfg.MinMV, cfg.MaxMV, cfg.MVStep = 9000, 9100, 100
	cfg.HoldMin = 2
	e, s := newSimEngine(cfg, 0, ckey{2, 5})
	var trims map[[2]int]float64
	set := func() {
		s.failBelowMV = 0
		if s.mhz == 600 && trims[[2]int{2, 6}] > -12.5 { // only 600 MHz trips chip 6, whatever the voltage
			s.failBelowMV = 99999
		}
	}
	trims = simTrims(e, s, func(map[[2]int]float64) { s.mu.Lock(); set(); s.mu.Unlock() })
	apply := e.applyPlan
	e.applyPlan = func(p planMV) error {
		err := apply(p)
		s.mu.Lock()
		set()
		s.mu.Unlock()
		return err
	}
	e.Run()
	log := strings.Join(e.fullLog, "\n")
	if !strings.Contains(log, "slowing b2.c5 to -12.5 MHz") {
		t.Fatalf("600 MHz should have needed the trim:\n%s", log)
	}
	if len(trims) != 0 || len(e.runTrims) != 0 || !strings.Contains(log, "still clean, the trim is lifted to +0 MHz") {
		t.Fatalf("at the best setting (625) the trim isn't needed and should be lifted: %v %v\n%s", trims, e.runTrims, log)
	}
}
