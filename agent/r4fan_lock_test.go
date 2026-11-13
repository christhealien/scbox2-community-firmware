package main

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// r4-fancheck: fan lock findings.

func r4LockEnv(t *testing.T) (write func(now time.Time, lines ...string), idled *int) {
	d := t.TempDir()
	oldA := *authFile
	*authFile = filepath.Join(d, "auth.json")
	oldB := backupRoot
	backupRoot = d
	os.MkdirAll(filepath.Join(d, "xminerd/trash"), 0o755)
	n := 0
	r4ResetUnlock()
	oldI, oldN := fanLockIdle, fanLockNotify
	fanLockIdle = func() error { n++; return nil }
	fanLockNotify = func(string) {}
	t.Cleanup(func() {
		*authFile, backupRoot = oldA, oldB
		fanLockIdle, fanLockNotify = oldI, oldN
		setCharge("")
		fanLockMu.Lock()
		fanLockRun = 0
		fanLockMu.Unlock()
	})
	var all []string
	write = func(now time.Time, lines ...string) {
		for _, l := range lines {
			all = append(all, " ["+now.Format("2006-01-02 15:04:05")+"]  "+l)
		}
		os.WriteFile(watchdogPath(), []byte(strings.Join(all, "\n")+"\n"), 0o644)
	}
	return write, &n
}

const (
	r4Bad  = "[Xminerd] fan0:(60, 0)  fan1:(60, 1500)  fan2:(60, 1500)  fan3:(60, 1500)  . fan status: 1"
	r4Good = "[Xminerd] fan0:(60, 1500)  fan1:(60, 1500)  fan2:(60, 1500)  fan3:(60, 1500)  . fan status: 0"
	r4Kill = "[Xminerd] Kill Miner Reason NCF(0,1,1)!!"
)

// After the fans are fixed and the lock is removed (the watchdog reads every fan fine), the next 15 s tick
// locks mining again on the kills from BEFORE the unlock (they count for 10 minutes), puts the miner on Idle
// again and sends a second "mining stopped and locked" notification. Nothing new happened.
func TestR4FanLockRelocksOnOldKillsAfterUnlock(t *testing.T) {
	write, idled := r4LockEnv(t)
	now := time.Now().Add(-5 * time.Minute)
	// a fan stops: the watchdog flags it and kills the miner twice
	write(now, r4Bad, r4Kill)
	now = now.Add(5 * time.Second)
	write(now, r4Bad, r4Kill)
	now = now.Add(5 * time.Second)
	write(now, r4Good) // between kills the latest reading is often clear
	fanLockTick(now)
	if fanLockErr() == nil {
		t.Fatal("setup: not locked on 2 kills")
	}
	// the owner fixes the fan; the watchdog reads it fine from then on; unlock after 3 minutes
	for i := 0; i < 12; i++ {
		now = now.Add(15 * time.Second)
		write(now, r4Good)
	}
	if err := fanUnlock(true, now); err != nil {
		t.Fatalf("unlock refused: %v", err)
	}
	before := *idled
	now = now.Add(15 * time.Second)
	write(now, r4Good)
	fanLockTick(now) // the next minerHealthLoop tick: fans fine
	if fanLockErr() != nil {
		t.Fatalf("BUG: re-locked right after the unlock with every fan reading fine (idled again: %v); the kills were from before the unlock", *idled > before)
	}
	// a NEW failure after the unlock still locks at once
	now = now.Add(15 * time.Second)
	write(now, r4Bad, r4Kill)
	now = now.Add(5 * time.Second)
	write(now, r4Bad, r4Kill, r4Good)
	fanLockTick(now)
	if fanLockErr() == nil {
		t.Fatal("a new fan failure after the unlock didn't lock")
	}
}

// fanLockEngage stops a tuning run BEFORE the lock (the hold the run checks at its end) exists: a run that
// ends while the Idle request is still on its way finds no hold and puts its setting back, over the Idle.
// The pool guard sets its hold first (poolguard.go poolHoldSet before stopTunerFor).
func TestR4FanLockTunerRestoreRace(t *testing.T) {
	write, _ := r4LockEnv(t)
	cfg := fastCfg()
	cfg.BaselineMin, cfg.HoldMin = 60, 60 // a long run: the lock comes while it tests
	e, s := newSimEngine(cfg, 0, ckey{2, 5})
	e.held = miningHoldErr
	e.chargeNow = getCharge
	realSleep := e.sleep
	e.sleep = func(d time.Duration) { time.Sleep(time.Millisecond); realSleep(d) }
	setCharge("tuner")
	tunerMu.Lock()
	oldT := tunerCur
	tunerCur = e
	tunerMu.Unlock()
	t.Cleanup(func() { tunerMu.Lock(); tunerCur = oldT; tunerMu.Unlock() })
	if !e.begin() {
		t.Fatal("begin")
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); e.Run() }()
	time.Sleep(200 * time.Millisecond) // the run is testing
	// the Idle request takes a moment (two HTTP calls to the OEM's backend; seconds when it's busy)
	var appliedAtIdle int
	fanLockIdle = func() error {
		s.mu.Lock()
		appliedAtIdle = len(s.applied) // the run was told to stop just before this
		s.mu.Unlock()
		time.Sleep(300 * time.Millisecond)
		return nil
	}
	now := time.Now()
	if !e.isRunning() {
		t.Fatal("setup: the run ended too soon")
	}
	write(now, r4Bad, r4Kill, r4Bad, r4Kill, r4Good)
	fanLockTick(now)
	wg.Wait()
	s.mu.Lock()
	after := s.applied[appliedAtIdle:]
	s.mu.Unlock()
	if fanLockErr() == nil {
		t.Fatal("setup: not locked")
	}
	log := strings.Join(e.logbuf, "\n")
	if len(after) > 0 || !strings.Contains(log, "stays on Idle") {
		t.Fatalf("BUG: the run, stopped by the fan lock, put a setting back while the lock's Idle request was under way (plans written: %v); its end log:\n%s", after, tail(log, 400))
	}
}

func tail(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

// r4ResetUnlock: the fixed tree keeps the last unlock time (fanUnlockAt); start each test without one.
// (On the unfixed tree this file needs the one-line stub in r4stub_unfixed.go.)
