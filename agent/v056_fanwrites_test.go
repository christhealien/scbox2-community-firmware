package main

import (
	"math"
	"testing"
	"time"
)

// v0.5.6: the SC Box II's fan hold writes in steps of tunerFanDeadband or after tunerFanSettle, never the same value
// again before tunerFanResend, and a raise 1 °C over the target at once.
func TestTunerFanBox2WriteRule(t *testing.T) {
	useBox2(t)
	for _, c := range []struct {
		want, cur int
		temp, tgt float64
		since     time.Duration
		write     bool
		what      string
	}{
		{61, 62, 71.5, 72, 30 * time.Second, false, "a 1 % step down waits"},
		{60, 62, 71.5, 72, 30 * time.Second, false, "a 2 % step down waits"},
		{59, 62, 71.5, 72, 30 * time.Second, true, "3 % adds up: written"},
		{61, 62, 71.5, 72, 2 * time.Minute, true, "a small change still there after 2 min: written"},
		{63, 62, 73.0, 72, 3 * time.Second, true, "a raise 1 °C over the target: at once, even straight after a write"},
		{63, 62, 72.4, 72, 30 * time.Second, false, "a 1 % raise just at the target waits for the change to add up"},
		{63, 62, 71.0, 72, 30 * time.Second, false, "a small raise under the target waits"},
		{66, 62, 71.0, 72, 5 * time.Second, false, "even a big change keeps the 10 s gap"},
		{62, 62, 72.0, 72, time.Hour, false, "no change: nothing (the re-send is holdFan's)"},
	} {
		if got := tunerFanBox2Write(c.want, c.cur, c.temp, c.tgt, c.since); got != c.write {
			t.Errorf("%s: got %v", c.what, got)
		}
	}
}

// The owner's case: a board hovering about the target, read every 15 s for 70 minutes. v0.5.5 wrote 194 times in
// 71 minutes; now a handful, and the temperature is still held.
func TestTunerFanHoldBox2FewWrites(t *testing.T) {
	useBox2(t)
	cfg := defaultTunerConfig()
	cfg.DryRun, cfg.FanHold = false, true
	e := newTunerEngine(cfg)
	clock := time.Date(2026, 11, 20, 12, 12, 0, 0, time.UTC)
	e.now = func() time.Time { return clock }
	e.fanFloor, e.fanGuard = nil, nil
	e.readFan = func() (int, bool) { return 62, true }
	fan, writes := 62.0, 0
	temp := 72.0
	e.readTemp = func() (float64, bool) { return temp, true }
	e.applyFan = func(p int) error { writes++; fan = float64(p); return nil }
	for i := 0; i < 70*4; i++ {
		// the board settles toward a temperature set by the fan (lower fan: warmer), plus a ±0.6 °C wobble
		eq := 72 + (62-fan)*0.35
		temp += (eq-temp)*0.3 + 0.6*math.Sin(float64(i)*1.7)
		e.holdFan(72)
		clock = clock.Add(15 * time.Second)
	}
	if writes > 30 {
		t.Fatalf("%d fan writes in 70 minutes (v0.5.5: 194)", writes)
	}
	if temp < 69 || temp > 75 {
		t.Fatalf("the hold lost the target: %.1f °C at fan %.0f %%", temp, fan)
	}
	t.Logf("%d writes, ended at %.1f °C, fan %.0f %%", writes, temp, fan)
}

// A board that heats up still gets its air at once: 2 °C over the target, the fans go up on the next reading.
func TestTunerFanHoldBox2HeatStillFast(t *testing.T) {
	useBox2(t)
	cfg := defaultTunerConfig()
	cfg.DryRun, cfg.FanHold = false, true
	e := newTunerEngine(cfg)
	clock := time.Date(2026, 11, 20, 12, 12, 0, 0, time.UTC)
	e.now = func() time.Time { return clock }
	e.fanFloor, e.fanGuard = nil, nil
	e.readFan = func() (int, bool) { return 62, true }
	temp, fan := 72.0, 62
	e.readTemp = func() (float64, bool) { return temp, true }
	e.applyFan = func(p int) error { fan = p; return nil }
	for i := 0; i < 8; i++ { // settled
		e.holdFan(72)
		clock = clock.Add(15 * time.Second)
	}
	start := fan
	temp = 74 // a hotter clock: 2 °C over
	clock = clock.Add(3 * time.Second)
	e.holdFan(72)
	if fan <= start {
		t.Fatalf("2 °C over the target and the fans stayed at %d %%", fan)
	}
	temp = 80.5 // 8.5 °C over (full fan from min(target+8, abort−2)): full fan
	clock = clock.Add(15 * time.Second)
	e.holdFan(72)
	if fan != 100 {
		t.Fatalf("8.5 °C over the target: fans %d %%, want 100", fan)
	}
}
