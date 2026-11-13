package main

// tuner-sweep/locks: the watchdog (tunerwatch.go, part of the proposed patch). A run frozen in a hook is ended
// from outside: "in charge" released, the baseline put back, the engine answers again, a new run can start.

import (
	"strings"
	"sync"
	"testing"
	"time"
)

type watchRec struct {
	mu      sync.Mutex
	ended   int
	applied []planMV
}

func fastWatchdog(t *testing.T) *watchRec {
	w := &watchRec{}
	oa, oe, os, oend := tunerStuckAfter, tunerWatchEvery, tunerForceApply, tunerEnded
	tunerStuckAfter, tunerWatchEvery = 400*time.Millisecond, 20*time.Millisecond
	tunerForceApply = func(p planMV) error { w.mu.Lock(); w.applied = append(w.applied, p); w.mu.Unlock(); return nil }
	tunerEnded = func(e *tunerEngine) {
		w.mu.Lock()
		w.ended++
		w.mu.Unlock()
		if getCharge() == "tuner" {
			setCharge("")
		}
	}
	old := getCharge()
	t.Cleanup(func() {
		tunerStuckAfter, tunerWatchEvery, tunerForceApply, tunerEnded = oa, oe, os, oend
		setCharge(old)
	})
	return w
}

func waitFor(t *testing.T, what string, d time.Duration, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// a hook that never returns (v1.5.15: trimSet frozen on chipTrimMu), with and without e.mu held by the frozen run
func TestTunerWatchdogEndsFrozenRun(t *testing.T) {
	for _, holdMu := range []bool{false, true} {
		w := fastWatchdog(t)
		e, s := newSimEngine(fastCfg(), 8900, ckey{2, 5})
		release := make(chan struct{})
		n := 0
		apply := e.applyPlan
		e.applyPlan = func(p planMV) error {
			n++
			if n == 2 { // the first test's setting: frozen
				if holdMu {
					e.mu.Lock()
					defer e.mu.Unlock()
				}
				<-release
			}
			return apply(p)
		}
		setCharge("tuner")
		e.begin()
		done := make(chan struct{})
		go func() { superviseTuner(e); close(done) }()
		time.Sleep(50 * time.Millisecond)
		e.Stop() // doesn't reach it (and mustn't hang when the run holds e.mu... it does without the watchdog)
		waitFor(t, "the watchdog", 3*time.Second, func() bool { return getCharge() != "tuner" })
		<-done
		w.mu.Lock()
		if len(w.applied) != 1 || w.applied[0] != e.resolveBaseline() {
			t.Errorf("holdMu=%v: baseline not put back: %+v", holdMu, w.applied)
		}
		if w.ended != 0 {
			t.Errorf("holdMu=%v: the normal end ran too", holdMu)
		}
		w.mu.Unlock()
		if e.isRunning() {
			t.Errorf("holdMu=%v: still running", holdMu)
		}
		if v := e.statusView(); v["running"] != false {
			t.Errorf("holdMu=%v: status %v", holdMu, v["running"])
		}
		e.Stop() // returns
		// the frozen call returns later: the run ends on its own and changes nothing more
		s.mu.Lock()
		before := len(s.applied)
		s.mu.Unlock()
		close(release)
		select {
		case <-e.done:
		case <-time.After(3 * time.Second):
			t.Fatalf("holdMu=%v: the run didn't end once unfrozen", holdMu)
		}
		s.mu.Lock()
		after := len(s.applied)
		s.mu.Unlock()
		if after > before+1 { // (the call that was frozen completes; nothing after it)
			t.Errorf("holdMu=%v: the dead run applied %d more settings", holdMu, after-before-1)
		}
	}
}

// a hook that panics: the run's own end runs (deferred), the supervisor releases the charge
func TestTunerWatchdogPanicInHook(t *testing.T) {
	w := fastWatchdog(t)
	e, _ := newSimEngine(fastCfg(), 8900, ckey{2, 5})
	n := 0
	e.readErrors = func() (map[ckey]int, error) {
		n++
		if n == 5 {
			var m map[ckey]int
			m[ckey{}] = 1 // a nil-map write
		}
		out := map[ckey]int{}
		for i := 0; i < 46; i++ {
			out[ckey{2, i}] = 0
		}
		return out, nil
	}
	setCharge("tuner")
	e.begin()
	done := make(chan struct{})
	go func() { superviseTuner(e); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("supervisor didn't return")
	}
	if getCharge() == "tuner" || e.isRunning() {
		t.Fatalf("charge %q running %v", getCharge(), e.isRunning())
	}
	_ = w
}

// a normal run: the normal end, no forced one
func TestTunerWatchdogNormalRun(t *testing.T) {
	w := fastWatchdog(t)
	tunerStuckAfter = 2 * time.Second
	e, _ := newSimEngine(fastCfg(), 8900, ckey{2, 5})
	setCharge("tuner")
	e.begin()
	superviseTuner(e)
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ended != 1 || len(w.applied) != 0 || getCharge() != "" {
		t.Fatalf("ended %d forced %v charge %q\n%s", w.ended, w.applied, getCharge(), strings.Join(e.fullLog, "\n"))
	}
}
