package main

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// L25: the run's end notification ("Tuning") says where the miner is. It always said "the miner is back on
// <baseline>", but the run goes back to what the miner ran before it (a preset, its own setting), stays on Idle
// when a guard holds it there, and stays on the confirmed setting when stopped during the presets; a guard's stop
// was reported as "stopped by hand".

// the miner ran something other than the baseline before the run (default baseline: Stock): it goes back to that
func TestL25EndTextSaysWhatItWentBackTo(t *testing.T) {
	e, s := newSimEngine(fastCfg(), 8900, ckey{2, 5})
	restored := false
	e.before = func() (func() error, string) {
		return func() error { restored = true; return nil }, "650 MHz · 9000 mV"
	}
	started := ""
	e.onStart = func() { started = e.startText(); e.Stop() }
	e.Run()
	_ = s
	if !restored {
		t.Fatal("the run didn't put back what the miner ran before (test setup)")
	}
	if !strings.Contains(started, "650 MHz") {
		t.Errorf("start text names the baseline, not what the run goes back to: %q", started)
	}
	if m := e.endText(); !strings.Contains(m, "back on 650 MHz") {
		t.Errorf("end text: %q (the miner went back to 650 MHz · 9000 mV)", m)
	}
}

// the pool guard stops the run and holds the miner on Idle: not "stopped by hand", not "back on …"
func TestL25EndTextGuardStopHeldIdle(t *testing.T) {
	e, _ := newSimEngine(fastCfg(), 8900, ckey{2, 5})
	hold := false
	e.held = func() error {
		if hold {
			return errors.New("mining is held on Idle by the pool guard")
		}
		return nil
	}
	e.onStart = func() { hold = true; e.StopFor("the pool guard: the pool guard put the miner on Idle") }
	e.Run()
	m := e.endText()
	if strings.Contains(m, "by hand") || strings.Contains(m, "back on") {
		t.Errorf("end text: %q (the pool guard stopped it; the miner stays on Idle)", m)
	}
	if !strings.Contains(m, "pool guard") || !strings.Contains(m, "Idle") {
		t.Errorf("end text doesn't say who stopped it / that it stays on Idle: %q", m)
	}
}

// stopped by hand during the presets, after the confirm: the miner stays on the confirmed setting
func TestL25EndTextStopAfterConfirm(t *testing.T) {
	e := newTunerEngine(fastCfg())
	e.begin()
	e.startedAt = time.Now().Add(-time.Hour)
	e.baseUsed = planMV{MHz: 625, MV: 9100, PV: 9400}
	e.best = &planMV{MHz: 675, MV: 9200, PV: 9500}
	e.confirmed = true
	e.Stop()
	if m := e.endText(); strings.Contains(m, "back on 625") || !strings.Contains(m, "675 MHz") {
		t.Errorf("end text: %q (the run left the miner on the confirmed 675 MHz)", m)
	}
}

// the end couldn't put the miner back: the end text mustn't say it's back
func TestL25EndTextRestoreFailed(t *testing.T) {
	e := newTunerEngine(fastCfg())
	e.begin()
	e.startedAt = time.Now().Add(-time.Hour)
	e.baseUsed = planMV{MHz: 625, MV: 9100, PV: 9400}
	e.restoreFailed = true
	e.Stop()
	if m := e.endText(); strings.Contains(m, "is back on") {
		t.Errorf("end text: %q (its put-back failed)", m)
	}
}
