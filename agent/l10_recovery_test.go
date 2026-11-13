package main

// sweep22/L10-recovery: crash/restart recovery.

import (
	"os"
	"testing"
	"time"
)

// The watchdog ends a stuck run whose hook holds the trims file (v1.5.15's freeze) and whose backend doesn't
// answer: neither its trims nor its baseline go back. The marker is then kept WITHOUT Ended, so the next panel
// start (days later) treats it as a run "cut short" and puts the old baseline over whatever the owner chose
// since, here the Idle they chose after being told "choose Stock or a preset".
func TestL10ForceEndTrimsFailedOverridesLaterIdle(t *testing.T) {
	m := newHLMiner(t)
	captureNotify(t)
	newCwSim(t, func(int, float64) float64 { return 1 })
	fastWatchdog(t)
	tunerForceApply = func(planMV) error { return errTunerDead } // the backend doesn't answer
	e, _ := newSimEngine(fastCfg(), 8900, ckey{2, 5})
	e.cfg.DryRun = false
	base := planMV{MHz: 600, MV: 9000, PV: 9300}
	e.baseAtom.Store(&base)
	e.baseUsed = base
	e.startedAt = time.Now()
	writeTunerMarker(e)
	test := planMV{MHz: 675, MV: 9200, PV: 9500}
	applyTunePlan(test)
	markTunerTesting(test)
	cwSetTrim(2, 5, -12.5) // being tried by the run
	setCharge("tuner")
	chipTrimMu.Lock() // the stuck run holds the trims file
	tunerForceEnd(e, "test")
	chipTrimMu.Unlock()
	if err := applyPresetName("Idle"); err != nil { // the owner, after "choose Stock or a preset"
		t.Fatal(err)
	}
	r5Restart()
	r7Restart()
	tunerRecover(time.Second)
	if !m.isIdle() {
		t.Errorf("BUG: the Idle the owner chose after the stuck run was ended was woken at the next start onto the run's baseline (%d MHz)", m.planMHz())
	}
	if got := r7Trims(); got[[2]int{2, 5}] != 0 {
		t.Errorf("the run's trim should still be taken off at the next start: %v", got)
	}
}

// A hook that panics: Run's own end runs as the panic unwinds (here: back to what the miner ran before the
// run, a preset). The supervisor then also forces the run's baseline over it, so the miner ends on the
// baseline instead of the setting it ran before.
func TestL10PanicEndKeepsRunsOwnPutBack(t *testing.T) {
	w := fastWatchdog(t)
	e, _ := newSimEngine(fastCfg(), 8900, ckey{2, 5})
	restored := 0
	e.before = func() (func() error, string) { return func() error { restored++; return nil }, "preset X" }
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
	superviseTuner(e)
	w.mu.Lock()
	defer w.mu.Unlock()
	if restored != 1 {
		t.Fatalf("the run's own end didn't put the setting from before back (%d)", restored)
	}
	if len(w.applied) != 0 {
		t.Errorf("BUG: after the run's own end put the setting from before back, the supervisor forced the baseline over it: %+v", w.applied)
	}
}

// (run with -race) Settings → Recovery starts a keepLogTick at once while the minute loop runs its own: both
// read and write recLastSave and the same .tmp file without a lock.
func TestL10KeepLogTickConcurrent(t *testing.T) {
	d := recTestDir(t)
	old := *logPath
	*logPath = d + "/messages"
	defer func() { *logPath = old }()
	done := make(chan struct{})
	for i := 0; i < 4; i++ {
		go func(i int) {
			for j := 0; j < 20; j++ {
				recMu.Lock() // (a new line in the log each time: every tick saves)
				os.WriteFile(*logPath, []byte("Nov  4 07:30:00 line "+itoa(i*100+j)+"\n"), 0644)
				recMu.Unlock()
				keepLogTick()
			}
			done <- struct{}{}
		}(i)
	}
	for i := 0; i < 4; i++ {
		<-done
	}
}
