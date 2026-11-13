package main

import (
	"strings"
	"testing"
)

// A fan-test re-run first puts the miner on Idle, which needs Goldshell's backend, which needs the mining
// program. When the miner is restarting or has just booted (the mining program isn't running) the re-run
// used to go ahead and fail half-way at applyIdle, raising a ⚠ warning + notification — even though a reboot
// runs Goldshell's fan test anyway. It now refuses up front with a plain message and starts nothing.
func TestR25FanRerunRefusedWhileMinerDown(t *testing.T) {
	// clean guards: not over-temp, not tuner/ceiling, not strong-chip test
	setCharge("")
	defer setCharge("")

	// mining program NOT running (only the watchdog is up): the miner is restarting
	fakeProc(t, "xminerd")
	err := fanTestRerun()
	if err == nil || !strings.Contains(err.Error(), "restarting") {
		t.Fatalf("expected a 'restarting' refusal while the mining program is down, got: %v", err)
	}
	if fanRerunActive.Load() {
		t.Fatal("a re-run was started even though the miner is restarting")
	}

	// mining program running: the guard passes (we hold the re-run lock so it reports that instead of doing
	// real work, which proves it got past the restarting guard)
	fakeProc(t, "xminerd", "intminer")
	if !fanRerunMu.TryLock() {
		t.Fatal("setup: fan re-run lock already held")
	}
	defer fanRerunMu.Unlock()
	err = fanTestRerun()
	if err == nil || strings.Contains(err.Error(), "restarting") {
		t.Fatalf("with the miner mining the restarting guard must not fire; got: %v", err)
	}
	if !strings.Contains(err.Error(), "already being re-run") {
		t.Fatalf("expected the lock-held path, got: %v", err)
	}
}
