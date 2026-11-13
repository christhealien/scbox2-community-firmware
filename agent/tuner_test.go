package main

import (
	"math"
	"sync"
	"testing"
	"time"
)

func TestPoissonLimitRef(t *testing.T) {
	// (lam, p, want) straight from asic_tuner.py
	type c struct {
		lam, p float64
		want   int
	}
	for _, x := range []c{
		{0, 0.05, 0}, {0.5, 0.05, 2}, {0.5, 0.0025, 3}, {0.5, 0.001, 4},
		{1, 0.05, 3}, {1, 0.0025, 5}, {2, 0.05, 5}, {2, 0.001, 8},
		{5, 0.05, 9}, {10, 0.0025, 20}, {25, 0.05, 33}, {100, 0.001, 132},
		{399, 0.0025, 456}, {500, 0.05, 537}, {1000, 0.001, 1098},
	} {
		if g := poissonLimit(x.lam, x.p); g != x.want {
			t.Errorf("poissonLimit(%g,%g)=%d want %d", x.lam, x.p, g, x.want)
		}
	}
}

func TestFalseFailRef(t *testing.T) {
	for _, x := range []struct{ s, want float64 }{{2.0, 0.1831832374}, {2.5, 0.05}, {3.0, 0.0108693300}} {
		if g := falseFail(x.s); math.Abs(g-x.want) > 1e-6 {
			t.Errorf("falseFail(%g)=%.10f want %.10f", x.s, g, x.want)
		}
	}
}

func TestTunerLimitRef(t *testing.T) {
	// (rate,hours,scale,weak,wf,margin,n,sigma) -> want, from the Python LIM rows
	type c struct {
		rate, hours, scale float64
		weak               bool
		wf, margin         float64
		n                  int
		sigma, want        float64
	}
	for _, x := range []c{
		{5, 0.4166667, 1.0, false, 1.5, 2, 46, 2.5, 8},
		{20, 0.4166667, 1.26, false, 1.5, 2, 46, 2.5, 22},
		{50, 0.4166667, 1.0, true, 1.5, 2, 184, 2.5, 52},
		{0, 0.4166667, 1.0, false, 1.5, 2, 46, 2.5, 2},
	} {
		if g := tunerLimitVal(x.rate, x.hours, x.scale, x.weak, x.wf, x.margin, x.n, x.sigma); g != x.want {
			t.Errorf("tunerLimitVal%v=%g want %g", x, g, x.want)
		}
	}
}

// --- engine harness: 46 chips on board 2, virtual clock, injectable error model ---

func newSimEngine(cfg tunerConfig, failBelowMV int, spikeChip ckey) (*tunerEngine, *simState) {
	s := &simState{counts: map[ckey]float64{}, mv: cfg.BaselineMV, mhz: cfg.BaselineMHz, temp: 65, ghs: 1150, failBelowMV: failBelowMV, failAboveMHz: 99999, spike: spikeChip}
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.vc = t0
	s.t0 = t0
	e := newTunerEngine(cfg)
	e.before, e.verifyPlan, e.readFan = nil, nil, nil // no miner backend in tests
	e.miningStops = nil
	e.fanFloor, e.fanGuard = nil, nil
	e.now = func() time.Time { s.mu.Lock(); defer s.mu.Unlock(); return s.vc }
	e.sleep = func(d time.Duration) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.vc = s.vc.Add(d)
		// accrue errors: spike chip gains fast when under the fail voltage
		rate := 0.0
		if s.mv < s.failBelowMV || s.mhz > s.failAboveMHz {
			rate = 0.9 // ~54/min on the spike chip
		}
		s.counts[s.spike] += rate * d.Seconds()
	}
	e.readErrors = func() (map[ckey]int, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		out := map[ckey]int{}
		for b := 0; b < 1; b++ {
			for i := 0; i < 46; i++ {
				k := ckey{B: 2, I: i}
				out[k] = int(s.counts[k])
			}
		}
		return out, nil
	}
	e.readTemp = func() (float64, bool) { s.mu.Lock(); defer s.mu.Unlock(); return s.temp, true }
	e.readGHS = func() (float64, bool) { s.mu.Lock(); defer s.mu.Unlock(); return s.ghs, true }
	e.elapsed = func() (int, bool) {
		s.mu.Lock()
		defer s.mu.Unlock()
		return 100000 + int(s.vc.Sub(s.t0).Seconds()), true
	}
	e.applyPlan = func(p planMV) error {
		s.mu.Lock()
		s.mv = p.MV
		s.mhz = p.MHz
		s.applied = append(s.applied, p)
		s.mu.Unlock()
		return nil
	}
	e.emit = func(string) {}
	return e, s
}

type simState struct {
	mu           sync.Mutex
	vc, t0       time.Time
	counts       map[ckey]float64
	mv           int
	mhz          int
	temp, ghs    float64
	failBelowMV  int
	failAboveMHz int
	spike        ckey
	applied      []planMV
}

func fastCfg() tunerConfig {
	c := defaultTunerConfig()
	c.TopDown = true // these tests were written for the old (top-down) map walk; the bottom-up walk has its own
	c.DryRun = false
	c.Mode = "map"
	c.BaselineSrc = "own"
	c.BaselineMHz = 625
	c.BaselineMV = 9100
	c.BaselineMin = 1
	c.HoldMin = 1
	c.SettleMin = 0
	c.PollS = 30
	c.LowestMHz = 625 // single clock → exercises the lowest-clean-voltage search
	c.HighestMHz = 625
	c.Step = 25
	c.MaxMV = 9300
	c.MinMV = 8800
	c.MVStep = 100
	c.FanHold = false // tests don't wire a fan-apply path
	c.BuildPresets = false
	return c
}

func TestUndervoltFindsLowestClean(t *testing.T) {
	// clean at >=8900 mV, spike below 8900 → lowest clean should be 8900
	e, s := newSimEngine(fastCfg(), 8900, ckey{2, 5})
	e.Run()
	if e.best == nil {
		t.Fatal("no best plan")
	}
	if e.best.MV != 8900 {
		t.Fatalf("lowest clean = %d mV, want 8900 (log: %v)", e.best.MV, e.logbuf)
	}
	// the best setting was confirmed: the run ends on it (blake2b-asic-control)
	last := s.applied[len(s.applied)-1]
	if last.MV != 8900 || last.MHz != 625 || !e.confirmed {
		t.Fatalf("not left on the confirmed best, last applied = %v", last)
	}
}

func TestTempAbortRestoresBaseline(t *testing.T) {
	e, s := newSimEngine(fastCfg(), 0, ckey{2, 5}) // never spikes
	// deterministically overheat once virtual time passes 1.2 min (during the first undervolt test)
	e.readTemp = func() (float64, bool) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.vc.Sub(s.t0).Minutes() > 1.2 {
			return 90, true
		}
		return 65, true
	}
	e.Run()
	last := s.applied[len(s.applied)-1]
	if last.MV != 9100 {
		t.Fatalf("after abort, baseline not restored: last=%v", last)
	}
	found := false
	for _, l := range e.logbuf {
		if len(l) > 0 && (contains(l, "abort") || contains(l, "ABORT") || contains(l, "90.0°C")) {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected an abort log; got %v", e.logbuf)
	}
}

func TestErrorSpikeFailsTest(t *testing.T) {
	// spike at ALL voltages below baseline → first undervolt step (9000) must FAIL,
	// so lowest clean stays the baseline 9100
	e, _ := newSimEngine(fastCfg(), 9100, ckey{2, 5})
	e.Run()
	if e.best == nil || e.best.MV != 9100 {
		t.Fatalf("expected lowest clean to stay at baseline 9100, got %v", e.best)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestClockCurveStopsAtCeiling(t *testing.T) {
	cfg := fastCfg()
	cfg.Mode = "map"
	cfg.LowestMHz = 625
	cfg.HighestMHz = 700
	cfg.Step = 25
	cfg.MinMV = 8700 // undervolt headroom so voltage isn't the limiter
	e, s := newSimEngine(cfg, 8700, ckey{2, 5})
	s.failAboveMHz = 650 // 650 ok, 675 fails at every voltage
	e.Run()
	if e.best == nil {
		t.Fatal("no best")
	}
	if e.best.MHz != 650 {
		t.Fatalf("top clean clock = %d, want 650 (log: %v)", e.best.MHz, e.logbuf)
	}
	// left on the confirmed best
	last := s.applied[len(s.applied)-1]
	if last.MHz != 650 || !e.confirmed {
		t.Fatalf("not left on the confirmed best: %v", last)
	}
	// with confirm off it still ends on the best clean setting
	cfg.ConfirmWin = false
	e, s = newSimEngine(cfg, 8700, ckey{2, 5})
	s.failAboveMHz = 650
	e.Run()
	if last := s.applied[len(s.applied)-1]; last.MHz != 650 {
		t.Fatalf("confirm off: not left on the best: %v", last)
	}
}

// confirm (tuner.html parity): hours split into rounds of one test; all clean → CONFIRMED and left on it
func TestConfirmRoundsClean(t *testing.T) {
	cfg := fastCfg()
	cfg.Mode = "confirm"
	cfg.ConfirmMHz, cfg.ConfirmMV, cfg.ConfirmHours = 625, 9000, 0.05 // 3 min / 1-min tests = 3 rounds
	e, s := newSimEngine(cfg, 8800, ckey{2, 5})
	e.Run()
	n := 0
	for _, l := range e.logbuf {
		if containsStr(l, "CONFIRM round") {
			n++
		}
	}
	if n != 3 {
		t.Fatalf("rounds = %d, want 3 (log: %v)", n, e.logbuf)
	}
	if !e.confirmed {
		t.Fatalf("expected CONFIRMED (log: %v)", e.logbuf)
	}
	if last := s.applied[len(s.applied)-1]; last.MV != 9000 {
		t.Fatalf("a confirmed setting must stay on the miner, last applied %v", last)
	}
}

// a failing setting gets one retest per round, then REJECTED and the baseline restored
func TestConfirmRejectAfterRetest(t *testing.T) {
	cfg := fastCfg()
	cfg.Mode = "confirm"
	cfg.ConfirmMHz, cfg.ConfirmMV, cfg.ConfirmHours = 625, 8900, 0.05
	e, s := newSimEngine(cfg, 9000, ckey{2, 5}) // dirty below 9000 mV
	e.Run()
	retest, rejected := false, false
	for _, l := range e.logbuf {
		if containsStr(l, "one retest") {
			retest = true
		}
		if containsStr(l, "REJECTED") {
			rejected = true
		}
	}
	if !retest || !rejected || e.confirmed {
		t.Fatalf("want a retest then REJECTED (log: %v)", e.logbuf)
	}
	if last := s.applied[len(s.applied)-1]; last.MV != 9100 {
		t.Fatalf("baseline not restored, last applied %v", last)
	}
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
