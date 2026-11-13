package main

import (
	"errors"
	"testing"
	"time"
)

// hcIdle: drive a far-past reading pair so the guard puts the fake miner on Idle.
func hcIdle(t *testing.T, m *hlMiner, c fanConfig) {
	t.Helper()
	m.setTemps([3]float64{93.5, 93.5, 94}, normal, normal, normal)
	m.tick(c, 10*time.Second)
	m.tick(c, 10*time.Second)
	if !m.isIdle() || !heatIdleHeld() {
		t.Fatalf("setup: not heat-idled: %+v", m.putsCopy())
	}
}

// Stock, a preset and a manual wake are refused during the cool-down, with how long is left; Idle itself isn't.
func TestHeatCoolRefusesEarlyStart(t *testing.T) {
	m := newHLMiner(t)
	c := hlCfg()
	hcIdle(t, m, c)
	err := applyPresetName("Stock")
	var hc *heatCoolingErr
	if !errors.As(err, &hc) || hc.left <= 0 || hc.left > heatCoolFirst {
		t.Fatalf("Stock during the cool-down: %v", err)
	}
	if _, err := mutatePlan(func(p *powerplan) { p.FreqMHz = 600 }); !errors.As(err, &hc) {
		t.Fatalf("a manual change woke it during the cool-down: %v", err)
	}
	if m.count("wake") != 0 || !m.isIdle() {
		t.Fatalf("woken: %+v", m.putsCopy())
	}
	if err := applyPresetName("Idle"); err != nil {
		t.Fatalf("Idle refused: %v", err)
	}
	// cool-down over: Stock goes through
	heatIdleSince.Store(time.Now().Add(-heatCoolFirst).Unix() - 1)
	setCharge("idle")
	if err := applyPresetName("Stock"); err != nil {
		t.Fatalf("Stock after the cool-down: %v", err)
	}
}

// A second heat Idle within the hour gets the longer cool-down.
func TestHeatCoolRepeatIsLonger(t *testing.T) {
	newHLMiner(t)
	now := time.Now().Unix()
	heatIdleNoted(now - 1200)
	heatIdleNoted(now)
	heatEpMu.Lock()
	cool := heatEpCool
	heatEpMu.Unlock()
	if cool != heatCoolRepeat {
		t.Fatalf("repeat cool-down %v", cool)
	}
	heatIdleNoted(now + 7200)
	heatEpMu.Lock()
	cool = heatEpCool
	heatEpMu.Unlock()
	if cool != heatCoolFirst {
		t.Fatalf("after two hours: %v", cool)
	}
}

// "Start this when cool": waits through the cool-down (fan ticks on Idle give no readings), then starts it once.
func TestHeatCoolPendingStarts(t *testing.T) {
	m := newHLMiner(t)
	c := hlCfg()
	hcIdle(t, m, c)
	heatPending.Store("Stock")
	m.tick(c, 10*time.Second)
	time.Sleep(50 * time.Millisecond)
	if m.count("wake") != 0 || heatPendingName() != "Stock" {
		t.Fatalf("started during the cool-down: %+v", m.putsCopy())
	}
	heatIdleSince.Store(time.Now().Add(-heatCoolFirst).Unix() - 1)
	m.tick(c, 10*time.Second)
	deadline := time.Now().Add(3 * time.Second)
	for m.isIdle() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	controlMu.Lock() // heatPendingStart holds it to its end: done before the next test's setup
	controlMu.Unlock()
	if m.isIdle() || heatPendingName() != "" || getCharge() != "stock" {
		t.Fatalf("not started after the cool-down: charge %q pending %q %+v", getCharge(), heatPendingName(), m.putsCopy())
	}
}

// The owner's own choice (Idle, or another preset that went through) cancels a waiting start.
func TestHeatCoolPendingCancelled(t *testing.T) {
	m := newHLMiner(t)
	c := hlCfg()
	hcIdle(t, m, c)
	heatPending.Store("Stock")
	setCharge("preset:x")
	if heatPendingName() != "" {
		t.Fatal("another controller took charge: the waiting start stayed")
	}
	setCharge("idle")
	heatPending.Store("Stock")
	heatIdleSince.Store(0) // the hold ended some other way
	heatPendingCheck()
	if heatPendingName() != "" {
		t.Fatal("hold over: the waiting start stayed")
	}
}
