package main

// L27-locks: the fan loop (its throttle check, then the thermal guard's abort tier: fans 100 %) must not wait on
// the strong-chip test's lock, which its tick holds across the miner reads (buildDashboard over the cgminer API,
// the stock web's error counters, the trims' re-apply): each can take seconds to tens of seconds when the miner
// or its backend is slow.

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestL27FanAbortNotHeldBySctTick(t *testing.T) {
	m := newHLMiner(t)
	f := sctSetup(t)
	if e := sctStart(2, 0, 0); e != "" {
		t.Fatal(e)
	}
	for i := 0; i < int((5*time.Minute)/sctTick); i++ {
		f.advance(sctTick)
		sctTickOnce()
	}
	if !sctRunning() {
		t.Fatal("setup: the test isn't running")
	}
	entered, release := make(chan struct{}), make(chan struct{})
	sctReadBoard = func(int) (sctBoardInfo, bool) { // a slow miner read inside the tick (sct.mu held)
		close(entered)
		<-release
		return sctBoardInfo{}, false
	}
	tickDone := make(chan struct{})
	go func() { defer close(tickDone); f.advance(sctTick); sctTickOnce() }()
	<-entered
	stop := func() {
		select {
		case <-release:
		default:
			close(release)
		}
		<-tickDone
	}
	t.Cleanup(stop)

	c := hlCfg()
	c.ThrottleEnable, c.ThrottleC = true, 89 // the throttle would step the clock: not while the test owns it
	hot := [3]float64{91, 91, 91.5}
	m.setTemps(hot, normal, normal, normal)
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.tick(c, 10*time.Second)
		m.tick(c, 10*time.Second) // abort: 100 % written
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		stop()
		<-done
		t.Fatal("HANG: the fan loop (throttle check, then the abort tier) waited on sct.mu while the strong-chip tick was in a slow miner read")
	}
	ps := m.putsCopy()
	if len(ps) == 0 || !strings.Contains(ps[len(ps)-1].plan, "100 RPM 100 RPM") {
		t.Fatalf("no 100 %% write: %+v", ps)
	}
	for _, p := range ps {
		if !strings.HasPrefix(p.plan, "625 MHz") {
			t.Fatalf("the throttle stepped the clock while the strong-chip test owns it: %+v", ps)
		}
	}
	stop()
	if !sctRunning() {
		t.Fatal("the test ended")
	}
}

// A tuning run frozen holding its own lock (e.mu: the case lockLive and the watchdog exist for, e.g. a panic in
// one of its e.mu sections leaves it locked and its deferred end then blocks on it) must not hold up the thermal
// guard's Idle until the watchdog ends the run (tunerStuckAfter, 10 min): safetyStop already refuses the run's
// writes, so the stop can be delivered when the run can take it.
func TestL27ThermalIdleNotHeldByFrozenRun(t *testing.T) {
	m := newHLMiner(t)
	e := newTunerEngine(defaultTunerConfig())
	e.running, e.stopCh = true, make(chan struct{})
	e.mu.Lock() // frozen holding e.mu
	tunerMu.Lock()
	oldCur := tunerCur
	tunerCur = e
	tunerMu.Unlock()
	t.Cleanup(func() {
		e.dead.Store(true) // the watchdog: lockLive gives up
		time.Sleep(100 * time.Millisecond)
		e.mu.Unlock()
		tunerMu.Lock()
		tunerCur = oldCur
		tunerMu.Unlock()
	})
	done := make(chan error, 1)
	go func() { done <- thermalIdle("test overheat") }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("thermalIdle: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("HANG: the thermal guard's Idle waits for the frozen tuning run (until the watchdog ends it, up to 10 min)")
	}
	if m.count("idle") != 1 || !m.isIdle() {
		t.Fatalf("no Idle written: %+v", m.putsCopy())
	}
	e.dead.Store(true) // let the stop delivered meanwhile give up too
}

// The same for the fan lock (fans failed): its Idle mustn't wait for the frozen run either.
func TestL27FanLockIdleNotHeldByFrozenRun(t *testing.T) {
	m := newHLMiner(t)
	e := newTunerEngine(defaultTunerConfig())
	e.running, e.stopCh = true, make(chan struct{})
	e.mu.Lock()
	tunerMu.Lock()
	oldCur := tunerCur
	tunerCur = e
	tunerMu.Unlock()
	t.Cleanup(func() {
		e.dead.Store(true)
		time.Sleep(100 * time.Millisecond)
		e.mu.Unlock()
		tunerMu.Lock()
		tunerCur = oldCur
		tunerMu.Unlock()
		os.Remove(fanLockPath())
		setCharge("")
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		fanLockEngage(watchdogView{Flagged: "fan 1", Fans: []int{0, 3000, 3000, 3000}}, time.Now())
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("HANG: the fan lock's Idle waits for the frozen tuning run (until the watchdog ends it, up to 10 min)")
	}
	if m.count("idle") != 1 {
		t.Fatalf("no Idle written: %+v", m.putsCopy())
	}
}

// A tuner start while the last run is frozen holding its lock (the charge was taken from it by a safety stop, so
// the start gets past the charge check) waits in isRunning: not with tunerMu held, which the guards' stops need.
func TestL27TunerStartDoesNotHoldTunerMuOnFrozenRun(t *testing.T) {
	m := newHLMiner(t)
	e := newTunerEngine(defaultTunerConfig())
	e.running, e.stopCh = true, make(chan struct{})
	e.mu.Lock()
	tunerMu.Lock()
	oldCur := tunerCur
	tunerCur = e
	tunerMu.Unlock()
	started := make(chan struct{})
	t.Cleanup(func() {
		e.dead.Store(true)
		<-started
		time.Sleep(100 * time.Millisecond)
		e.mu.Unlock()
		tunerMu.Lock()
		nt := tunerCur
		tunerMu.Unlock()
		if nt != nil && nt != e && nt.done != nil { // the start went on once the frozen run was ended: refused by
			nt.Stop() // the safety stop's generation, it ends at once; wait for it
			select {
			case <-nt.done:
			case <-time.After(20 * time.Second):
				t.Error("the run started after the frozen one didn't end")
			}
		}
		tunerMu.Lock()
		tunerCur = oldCur
		tunerMu.Unlock()
		setCharge("")
	})
	setCharge("idle") // a safety stop took the charge from the frozen run
	go func() {
		defer close(started)
		rcPost(handleTuner, map[string]any{"action": "start"}) // waits for the frozen run (until the watchdog)
	}()
	time.Sleep(300 * time.Millisecond)
	done := make(chan error, 1)
	go func() { done <- thermalIdle("test overheat") }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("HANG: the thermal guard's stop waits on tunerMu, held by a tuner start waiting for the frozen run")
	}
	if m.count("idle") != 1 {
		t.Fatalf("no Idle written: %+v", m.putsCopy())
	}
}
