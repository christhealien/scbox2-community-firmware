package main

import (
	"os"
	"testing"
	"time"
)

// The engine's real trimSet must not hold chipTrimMu around cwSetTrim (which takes it): in v1.5.15 it did, and
// the tuner froze for good the first time it slowed a chip (a 575 MHz step on an owner's miner).
func TestTunerTrimSetNoSelfDeadlock(t *testing.T) {
	old := *authFile
	*authFile = t.TempDir() + "/auth.json"
	defer func() { *authFile = old }()
	oldStats := chipStatsPath // (not /tmp's: another hook or mock on the host may be writing it right now)
	chipStatsPath = t.TempDir() + "/sclite-chipstats.json"
	defer func() { chipStatsPath = oldStats }()
	e := newTunerEngine(tunerConfig{})
	done := make(chan error, 1)
	go func() { done <- e.trimSet(3, 32, -12.5) }()
	select {
	case <-done: // an error is fine here (no chip hook in a test): it must return
	case <-time.After(5 * time.Second):
		t.Fatal("trimSet deadlocked")
	}
	b, err := os.ReadFile(chipTrimPath())
	if err != nil || len(b) == 0 {
		t.Fatalf("trim not written: %v", err)
	}
	chipTrimMu.Lock() // and the lock is free again
	chipTrimMu.Unlock()
}
