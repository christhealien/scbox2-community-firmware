package main

import (
	"strings"
	"testing"
	"time"
)

// with the fix: the throttle's own clock comes back when it cools
func TestR4AFThrottleRestoresOwnClock(t *testing.T) {
	m := newAfMiner(t)
	c := afCfg()
	afTemp(86, true)
	fanTick(c)
	if !strings.HasPrefix(m.plan(), "625 MHz") {
		t.Fatalf("no throttle: %s", m.plan())
	}
	afTemp(75, true)
	fanTick(c)
	if p := m.plan(); !strings.HasPrefix(p, "650 MHz") {
		t.Fatalf("not restored: %s", p)
	}
	fanMu.RLock()
	sf := fanSt.SavedFreq
	fanMu.RUnlock()
	if sf != 0 {
		t.Fatal("saved clock kept after restore")
	}
}

// with the fix: still hot after throttleEvery → the next step, from the clock the throttle wrote
func TestR4AFThrottleNextStepAfterInterval(t *testing.T) {
	m := newAfMiner(t)
	old := throttleEvery
	defer func() { throttleEvery = old }()
	c := afCfg()
	afTemp(86, true)
	fanTick(c)
	fanTick(c) // within the minute: no step
	if !strings.HasPrefix(m.plan(), "625 MHz") {
		t.Fatalf("stepped twice within the minute: %s", m.plan())
	}
	throttleEvery = 0
	fanTick(c)
	if !strings.HasPrefix(m.plan(), "600 MHz") {
		t.Fatalf("no second step: %s", m.plan())
	}
	afTemp(70, true)
	fanTick(c)
	if p := m.plan(); !strings.HasPrefix(p, "650 MHz") {
		t.Fatalf("restore should go back to the first saved clock (650): %s", p)
	}
}

// with the fix: the tuner held the fans (target -1); when it lets go the curve writes again at once even if
// its target equals the last one it wrote
func TestR4AFCurveRewritesAfterTunerHold(t *testing.T) {
	m := newAfMiner(t)
	c := afCfg()
	c.ThrottleEnable = false
	afTemp(60, true)
	fanTick(c)
	n := m.nPuts()
	fanMu.Lock()
	fanSt.Target, fanSt.LastPct = -1, -1 // as a tuner hold tick leaves it
	fanMu.Unlock()
	m.running("650 MHz 9100 V 90 RPM 90 RPM PV 9400") // the tuner left its fans
	fanTick(c)
	if m.nPuts() == n || strings.Contains(m.plan(), " 90 RPM 90 RPM") {
		t.Fatalf("curve didn't take the fans back: %s", m.plan())
	}
	_ = time.Second
}
