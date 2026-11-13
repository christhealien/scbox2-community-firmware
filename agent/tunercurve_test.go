package main

import (
	"fmt"
	"math"
	"strings"
	"testing"
)

// An owner's miner: chips need 8900 mV at 575 MHz, 9000 at 600, 9200 at 625, more than the 9300 maximum at
// 650. blake2b-asic-control's curve: the first clock starts at the baseline voltage and goes down until it
// fails; each faster clock starts at the previous lowest clean voltage plus that step's lift (at most one
// step), goes up until clean, and never tests a voltage that already failed at this or a slower clock.
// A clear failure (over the limit in under half the test) isn't retested. The curve ends at 650 MHz.
func curveRun(t *testing.T, baseMHz, baseMV int) []string {
	cfg := fastCfg()
	cfg.LowestMHz, cfg.HighestMHz, cfg.Step = 575, 675, 25
	cfg.MaxMV, cfg.MinMV, cfg.MVStep = 9300, 8800, 100
	cfg.BaselineMHz, cfg.BaselineMV = baseMHz, baseMV
	cfg.HoldMin = 2 // a chip that's under-volted fails at the first reading: under half the test
	need := map[int]int{575: 8900, 600: 9000, 615: 9000, 625: 9200, 650: 9400, 675: 9500}
	e, s := newSimEngine(cfg, 0, ckey{2, 5})
	apply := e.applyPlan
	e.applyPlan = func(p planMV) error {
		s.mu.Lock()
		s.failBelowMV = need[p.MHz]
		s.ghs = 1150 * pll(float64(p.MHz)) / pll(575) // a real chip's hashrate follows its clock
		s.mu.Unlock()
		return apply(p)
	}
	e.Run()
	var got []string
	for _, r := range e.results {
		got = append(got, fmt.Sprintf("%d/%d %s", r.MHz, r.MV, r.Result))
	}
	if len(e.curve) != 3 || e.curve[0].MV != 8900 || e.curve[1].MV != 9000 || e.curve[2].MHz != 625 || e.curve[2].MV != 9200 {
		t.Fatalf("curve %+v", e.curve)
	}
	return got
}

func TestClockCurveOwnerMiner(t *testing.T) {
	got := curveRun(t, 615, 9100) // his baseline: 615 MHz / 9100 mV
	want := []string{
		"575/9100 clean", "575/9000 clean", "575/8900 clean", "575/8800 failed", // down from the baseline voltage
		"600/8900 failed", "600/9000 clean", // 8900 isn't tested again
		"625/9100 failed", "625/9200 clean", // starts a step up (600 needed +100); 9000 isn't tried
		"650/9300 failed", // starts at 9200+100: not clean at the maximum, the curve ends (675 not tried)
		"625/9200 clean",  // the confirm: the fastest clean setting runs again (twice as long), and holds
	}
	if strings.Join(got, ", ") != strings.Join(want, ", ") {
		t.Fatalf("tests run:\n got  %s\n want %s", strings.Join(got, ", "), strings.Join(want, ", "))
	}
}

func TestClockCurveRetestsLateFails(t *testing.T) {
	// a fail late in the test (hashrate, judged at the end) is retested once
	e, _ := newSimEngine(fastCfg(), 0, ckey{2, 5})
	if !decisiveFail("chip b2.c5 +9 errors (limit 4.0)", tunerStats{WatchedMin: 3}, 25) {
		t.Fatal("over the limit in 3 of 25 min is decisive")
	}
	if decisiveFail("chip b2.c5 +9 errors (limit 4.0)", tunerStats{WatchedMin: 20}, 25) ||
		decisiveFail("hashrate 4327.5 GH/s is 5% below the 4556.4 this clock should give", tunerStats{WatchedMin: 25}, 25) {
		t.Fatal("late or hashrate fails get a retest")
	}
	_ = e
}

func TestClockList(t *testing.T) {
	c := defaultTunerConfig()
	got := func() string { return fmt.Sprint(c.clockList()) }
	if g := got(); g != "[575 600 625 650 675]" {
		t.Fatalf("default 575–675 by 25: %s", g)
	}
	// An owner: every clock the chips can run, 580–680 by 12.5 (580 runs 575, 680 runs 675)
	c.LowestMHz, c.HighestMHz, c.Step = 580, 680, 12.5
	if g := got(); g != "[575 588 600 613 625 638 650 663 675]" {
		t.Fatalf("12.5 steps: %s", g)
	}
	for _, m := range c.clockList() {
		if r := pll(float64(m)); r != math.Floor(float64(m)/12.5)*12.5 || float64(m)-r >= 1 {
			t.Fatalf("%d MHz runs %g: not the smallest request for its step", m, r)
		}
	}
	if validateTunerConfig(c) != nil {
		t.Fatalf("12.5 rejected: %v", validateTunerConfig(c))
	}
	// whole-MHz steps count as typed
	c.LowestMHz, c.HighestMHz, c.Step = 600, 630, 10
	if g := got(); g != "[600 610 620 630]" {
		t.Fatalf("10 MHz steps: %s", g)
	}
	// blake2b-asic-control's curve_clocks: Lowest rounds down to the step grid, an off-grid Highest is
	// still the last clock tested
	c.LowestMHz, c.HighestMHz, c.Step = 590, 690, 25
	if g := got(); g != "[575 600 625 650 675 690]" {
		t.Fatalf("off-grid range: %s", g)
	}
	c.Step = 7.5
	if validateTunerConfig(c) == nil {
		t.Fatal("7.5 MHz steps accepted")
	}
	if clockLabel(613) != "613 MHz · runs 612.5" || clockLabel(600) != "600 MHz" {
		t.Fatal(clockLabel(613), clockLabel(600))
	}
}
