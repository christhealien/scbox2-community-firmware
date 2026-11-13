package main

// r5-restart (FIXED tree): a fan test re-run cut short by a panel restart. The run's marker holds the setting
// from before its Idle; at start fanRerunRecover puts it back once the fan test is over.

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

func r5RerunMarker(t *testing.T, m *hlMiner, charge string) {
	m.mu.Lock()
	raw, _ := json.Marshal(m.setting) // what it ran before the re-run
	m.setting["manual"], m.setting["select"] = false, float64(1)
	m.mu.Unlock()
	b, _ := json.Marshal(fanRerunRec{At: time.Now().Add(-3 * time.Minute).Unix(), Setting: raw, Charge: charge})
	if err := writeAtomic(fanRerunPath(), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestR5RerunCutShortRestored(t *testing.T) {
	m := newHLMiner(t)
	r5RerunMarker(t, m, "preset:Eco")
	r5Restart()
	fanRerunRecover()
	if m.isIdle() || m.planMHz() != 625 {
		t.Fatalf("the re-run's Idle wasn't undone after the restart (idle %v, %d MHz)", m.isIdle(), m.planMHz())
	}
	if getCharge() != "preset:Eco" {
		t.Errorf("charge %q, want the one from before the re-run", getCharge())
	}
	if _, err := os.Stat(fanRerunPath()); err == nil {
		t.Error("marker left behind")
	}
}

func TestR5RerunCutShortChosenSinceStays(t *testing.T) {
	m := newHLMiner(t)
	r5RerunMarker(t, m, "")
	r5Restart()
	setCharge("idle") // chosen since the start (the owner, the schedule)
	fanRerunRecover()
	if !m.isIdle() {
		t.Fatal("woke a miner put on Idle since the restart")
	}
}

func TestR5RerunMarkerJunk(t *testing.T) {
	m := newHLMiner(t)
	r5Restart()
	os.WriteFile(fanRerunPath(), nil, 0o644)
	fanRerunRecover()
	if _, err := os.Stat(fanRerunPath()); err == nil || m.count("wake")+m.count("plan") != 0 {
		t.Fatal("a zero-length marker: kept, or the miner written")
	}
}

// the heat hold survives a restart, and ends (on disk too) when the owner applies something
func TestR5HeatHoldEndsWithCharge(t *testing.T) {
	m := newHLMiner(t)
	c := hlCfg()
	m.setTemps([3]float64{93.5, 93.5, 94}, normal, normal, normal)
	m.tick(c, 10*time.Second)
	m.tick(c, 10*time.Second)
	r5Restart()
	if !heatIdleHeld() {
		t.Fatal("hold not restored")
	}
	setCharge("stock")
	r5Restart()
	if heatIdleHeld() {
		t.Fatal("a hold the owner ended came back after the next restart")
	}
}

// a tuning run stopped by the thermal guard's Idle, the panel gone before the run cleared its marker: at start
// tunerRecover puts the run's baseline back, waking the miner the thermal guard had just put on Idle.
func TestR5TunerRecoverKeepsHeatIdle(t *testing.T) {
	m := newHLMiner(t)
	c := hlCfg()
	m.setTemps([3]float64{93.5, 93.5, 94}, normal, normal, normal)
	m.tick(c, 10*time.Second)
	m.tick(c, 10*time.Second)
	if !m.isIdle() {
		t.Fatal("setup: not idle")
	}
	b, _ := json.Marshal(tunerMarker{Started: time.Now().Add(-time.Hour).Unix(), Base: planMV{MHz: 625, MV: 9100, PV: 9400}})
	writeAtomic(tunerMarkerPath(), b, 0o644)
	r5Restart()
	tunerRecover(time.Second)
	if !m.isIdle() {
		t.Fatalf("BUG: tunerRecover woke the miner from the thermal guard's Idle (boards at 94 °C): %d MHz", m.planMHz())
	}
}
