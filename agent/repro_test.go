package main

// tuner-sweep/locks: repros. Each FAILS on v1.5.16 (/home/claude/sclite-agent) and passes with the patch.

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Stop during a backend outage: the setting under test is being applied and the backend doesn't answer. The
// run should go to its restore at once; retry() doesn't look at Stop, so it keeps retrying for backendWait.
func TestLocksStopDuringBackendOutage(t *testing.T) {
	cfg := fastCfg()
	e, _ := newSimEngine(cfg, 8900, ckey{2, 5})
	e.backendWait, e.restoreWait = 3*time.Minute, 10*time.Minute
	base := e.resolveBaseline()
	n, testTries := 0, 0
	var stopAt time.Time
	apply := e.applyPlan
	e.applyPlan = func(p planMV) error {
		n++
		if n == 1 {
			return apply(p) // the baseline
		}
		if n == 2 { // the first test's setting: the backend goes away, the owner presses Stop
			stopAt = e.now()
			e.Stop()
		}
		if p != base {
			testTries++
		}
		if e.now().Sub(stopAt) >= 150*time.Second {
			return apply(p) // the backend is back after 2.5 min
		}
		return fmt.Errorf("the miner's backend (the OEM's minerd) didn't answer: test")
	}
	if !runEngine(t, e, 20*time.Second) {
		return
	}
	t.Logf("tries of the test's setting after Stop: %d; Run ended %v after Stop", testTries, e.now().Sub(stopAt))
	if testTries > 1 {
		t.Errorf("Stop during a backend outage: the run kept retrying the test's setting %d times (every 15 s) instead of going to its restore (retry ignores Stop)", testTries)
	}
}

// ---- the panel's other controllers vs a running tuner ----

// A hand edit of the trims (set, reset, switch the hook off = mining restart) is refused while a tuning run goes.
func TestLocksHandTrimRefusedWhileTuning(t *testing.T) {
	realTrimHooks(t)
	fakeCgminer(t)
	e := newTunerEngine(fastCfg())
	e.begin()
	oc := getCharge()
	setCharge("tuner") // (as the real start does)
	defer setCharge(oc)
	tunerMu.Lock()
	old := tunerCur
	tunerCur = e
	tunerMu.Unlock()
	defer func() { tunerMu.Lock(); tunerCur = old; tunerMu.Unlock() }()
	cwSetTrim(2, 6, -12.5) // the run's trim
	rr := httptest.NewRecorder()
	handleChipTrim(rr, httptest.NewRequest("POST", "/api/chip-trim", strings.NewReader(`{"reset":true}`)))
	if len(readChipTrims()) == 0 {
		t.Fatalf("a hand reset wiped the running tuner's trims: %s", rr.Body.String())
	}
}

// The tuner can't start while the limit probe ("ceiling") is in charge.
func TestLocksTunerStartRefusedDuringLimitProbe(t *testing.T) {
	old := getCharge()
	setCharge("ceiling")
	defer setCharge(old)
	oldWeb := *minerWeb
	*minerWeb = "127.0.0.1:1" // nothing answers
	defer func() { *minerWeb = oldWeb }()
	cfg := defaultTunerConfig() // dry run
	cfg.PollS = 1
	b, _ := json.Marshal(map[string]any{"action": "start", "config": cfg})
	rr := httptest.NewRecorder()
	handleTuner(rr, httptest.NewRequest("POST", "/api/tuner", strings.NewReader(string(b))))
	tunerMu.Lock()
	t2 := tunerCur
	tunerMu.Unlock()
	if strings.Contains(rr.Body.String(), `"ok":true`) {
		if t2 != nil { // stop it and let it end before the backend address goes back
			t2.Stop()
			for i := 0; i < 200 && (t2.isRunning() || getCharge() == "tuner"); i++ {
				time.Sleep(50 * time.Millisecond)
			}
			time.Sleep(200 * time.Millisecond) // (its end saves the run and notifies)
		}
		t.Fatalf("tuner started while the limit probe was in charge (charge now %q): %s", getCharge(), rr.Body.String())
	}
}

// ceilAbort is a plain bool written by the pool guard (stopTunerFor), the probe's Stop button and the probe's
// own goroutine, and read by the probe, without a lock: run with -race.
func TestLocksCeilAbortRace(t *testing.T) {
	old := getCharge()
	setCharge("ceiling")
	defer setCharge(old)
	done := make(chan struct{})
	go func() { defer close(done); stopTunerFor("test") }() // the pool guard
	rr := httptest.NewRecorder()
	handleCeiling(rr, httptest.NewRequest("POST", "/api/ceiling", strings.NewReader(`{"stop":true}`))) // the Stop button
	<-done
}

// The fan loop's "no board temperature for 3 minutes while hashing" safety Idle (main.go fanLoop) happens
// during a tuning run: the fan loop doesn't stop the run, and the run's own end (it aborts 2 minutes later on
// the same missing temperature) puts the miner back on its setting, which wakes it from that Idle. The
// boards hash again with no temperature reading.
func TestLocksSafetyIdleUndoneByTunerRestore(t *testing.T) {
	fb := newFakeBackend(t, false)
	oldCharge := getCharge()
	setCharge("tuner")
	defer setCharge(oldCharge)
	cfg := fastCfg()
	cfg.HoldMin = 20
	e, s := newSimEngine(cfg, 8900, ckey{2, 5})
	e.backendWait = 3 * time.Minute
	simApply := e.applyPlan
	blind := false
	e.readTemp = func() (float64, bool) {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.temp, !blind
	}
	n := 0
	e.applyPlan = func(p planMV) error {
		if blind { // the end of the run: the real thing, against the backend
			return applyTunePlan(p)
		}
		return simApply(p)
	}
	e.readErrors = func() (map[ckey]int, error) {
		n++
		if n == 20 && !blind {
			// every sensor stops reading; 3 minutes later the fan loop puts the miner on Idle and takes charge
			blind = true
			if err := applyIdle(); err != nil {
				t.Fatal(err)
			}
			setCharge("idle")
		}
		out := map[ckey]int{}
		for i := 0; i < 46; i++ {
			out[ckey{2, i}] = 0
		}
		return out, nil
	}
	if !runEngine(t, e, 20*time.Second) {
		return
	}
	if fb.idle() {
		return // stays on the safety Idle: good
	}
	t.Errorf("the tuner's restore woke the miner from the no-temperature safety Idle (charge %q); log tail:\n%s",
		getCharge(), strings.Join(e.fullLog[max(0, len(e.fullLog)-5):], "\n"))
}

// Check-then-act on "in charge": the Presets tab's apply checks getCharge() != "tuner", then talks to the
// backend for a few seconds, then sets its own charge. A tuning run started in that window loses its charge
// (Restart mining and firmware update are no longer refused, manual controls unlock). The schedule
// (scheduleTick) has the same shape.
func TestLocksChargeCheckThenAct(t *testing.T) {
	fb := newFakeBackend(t, false)
	oldCharge := getCharge()
	setCharge("")
	defer setCharge(oldCharge)
	started := make(chan string, 1)
	fb.onGet = func(n int) {
		if n != 1 {
			return
		}
		// a tuning run is started (the real start path) while the preset is being applied
		go func() {
			cfg := defaultTunerConfig() // dry run
			cfg.PollS = 1
			b, _ := json.Marshal(map[string]any{"action": "start", "config": cfg})
			rr := httptest.NewRecorder()
			handleTuner(rr, httptest.NewRequest("POST", "/api/tuner", strings.NewReader(string(b))))
			started <- rr.Body.String()
		}()
		select {
		case <-started:
			started <- "" // (put back: read below)
		case <-time.After(time.Second): // (the patch makes the start wait for the preset)
		}
	}
	rr := httptest.NewRecorder()
	handlePresets(rr, httptest.NewRequest("POST", "/api/presets", strings.NewReader(`{"action":"apply","name":"Stock"}`)))
	<-started
	tunerMu.Lock()
	tc := tunerCur
	tunerMu.Unlock()
	running := tc != nil && tc.isRunning()
	c := getCharge()
	if tc != nil {
		tc.Stop()
		for i := 0; i < 200 && (tc.isRunning() || getCharge() == "tuner"); i++ {
			time.Sleep(50 * time.Millisecond)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if running && c != "tuner" {
		t.Errorf("a tuning run is going but %q is in charge (%s)", c, strings.TrimSpace(rr.Body.String()))
	}
}

// reapplyTrims fails (the hook's report is older than the last setting change, as right after a change or
// while mining restarts): the real trimSet has already written the trim to the file, so trimOne reports "couldn't
// set the trim" and the run goes on as if nothing changed, but the trim stays in the file (applied at the next
// mining restart), isn't the run's (never undone, never handed to auto-trim), and from then on counts as a trim
// "set by hand" for that chip.
func TestLocksFailedTrimLeftInFile(t *testing.T) {
	noPresetsSaved(t)
	e, _, g := realTrimEngine(t)
	noteMinerChange("clock/voltage change") // newer than the hook's stats file: every reapply now fails
	guardHooks(e, g, nil)
	if !runEngine(t, e, 30*time.Second) {
		return
	}
	log := strings.Join(e.fullLog, "\n")
	if !strings.Contains(log, "couldn't set the trim") {
		t.Fatalf("reapply didn't fail?\n%s", log)
	}
	if tr := readChipTrims(); len(tr) > 0 {
		t.Errorf("trims file %+v after a run that kept no trim (runTrims %v); log:\n%s", tr, e.runTrims, log)
	}
}
