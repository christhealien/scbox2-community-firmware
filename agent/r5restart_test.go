package main

// r5-restart: what the panel forgets when it is killed (or the power is cut) in the middle of a Controls & fan
// operation. Each test runs the operation, then "restarts" the panel the way a new process starts: every
// in-memory variable back to its zero value, only files kept.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// r5Restart: what a fresh panel process has in memory (package vars at their initial values); files stay.
func r5Restart() {
	chargeMu.Lock()
	chargeBy = "" // (not setCharge: a new process doesn't run it)
	chargeMu.Unlock()
	heatIdleSince.Store(0)
	heatEsc = heatEscState{}
	throttledTo, throttleAt = 0, time.Time{}
	noTempSince, noTempTicks = time.Time{}, 0
	fanMu.Lock()
	fanSt.MaxT, fanSt.HotBoard, fanSt.Target, fanSt.Guard, fanSt.Note, fanSt.LastWrite, fanSt.SavedFreq, fanSt.LastPct = 0, -1, 0, "", "", time.Time{}, 0, 0
	fanHist = nil
	fanMu.Unlock()
	fanLockMu.Lock()
	fanLockRun, fanUnlockAt = 0, 0
	fanLockMu.Unlock()
	fanGuardMu.Lock()
	fanGuardSt, fanGuardOK = fanGuardState{}, false
	fanGuardMu.Unlock()
	lastSchKey, lastSchName = -1, ""
	r5Startup()
}

// r5Startup: what main() does at start for this area (restartRestore: the fixed tree's; a no-op stub unfixed).
var r5Startup = func() { restartRestore() }

func (m *hlMiner) planMHz() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, _ := m.setting["manualPowerplan"].(string)
	var f int
	for _, w := range strings.Fields(s) {
		if n, err := json.Number(w).Int64(); err == nil {
			f = int(n)
			break
		}
	}
	return f
}

// 1. Throttle step, panel restart, boards cool: the clock the throttle saved is gone. The miner runs the
// throttled clock for good, the Fan card no longer says "CLOCK THROTTLED", and nothing will put it back.
func TestR5ThrottleSavedClockLostOnRestart(t *testing.T) {
	m := newHLMiner(t)
	c := hlCfg()
	c.ThrottleEnable, c.ThrottleC, c.ResumeC, c.ThrottleStep = true, 85, 80, 25
	m.setTemps([3]float64{86, 86, 86.5}, normal, normal, normal)
	m.tick(c, 10*time.Second)
	if got := m.planMHz(); got != 600 {
		t.Fatalf("setup: throttle didn't step 625 → 600 (plan %d MHz)", got)
	}
	r5Restart() // panel killed / restarted while throttled
	m.setTemps(normal, normal, normal, normal)
	for i := 0; i < 6; i++ {
		m.tick(c, 10*time.Second)
	}
	fanMu.RLock()
	saved := fanSt.SavedFreq
	fanMu.RUnlock()
	if got := m.planMHz(); got != 625 {
		t.Fatalf("BUG: after a panel restart while throttled the clock stays at %d MHz for good (was 625 before the throttle; the panel's saved clock is %d, so the card shows no throttle)", got, saved)
	}
}

// 2. Overheat Idle, panel restart (boards still hot): the heat hold is gone, the schedule's first tick (within a
// minute of the start) wakes the miner while the boards are still at 93 °C.
func TestR5HeatIdleHoldLostOnRestart(t *testing.T) {
	m := newHLMiner(t)
	c := hlCfg()
	schedPath = filepath.Join(t.TempDir(), "schedule.json")
	defer func() { schedPath = "" }()
	b, _ := json.Marshal(schedule{Enabled: true, Rules: []schedRule{{Start: "00:00", Days: []int{0, 1, 2, 3, 4, 5, 6}, Preset: "Stock"}}})
	writeAtomic(schedPath, b, 0o644)
	m.setTemps([3]float64{93.5, 93.5, 94}, normal, normal, normal)
	m.tick(c, 10*time.Second)
	m.tick(c, 10*time.Second)
	if getCharge() != "idle" || !m.isIdle() || !heatIdleHeld() {
		t.Fatalf("setup: not heat-idle %+v", m.putsCopy())
	}
	r5Restart()
	scheduleTick() // the first tick after the start
	if !m.isIdle() {
		t.Fatalf("BUG: after a panel restart the schedule woke the miner from its overheat Idle with the boards still at 94 °C (charge now %q, heat hold %v)", getCharge(), heatIdleHeld())
	}
}

// 3. Fan lock removed by hand, then the panel restarts within 10 minutes (an update, a crash, the owner's
// restart): the unlock time is memory only, so the kills from BEFORE the unlock lock mining again and put the
// miner on Idle, with a second "mining stopped and locked" notification.
func TestR5FanUnlockForgottenOnRestart(t *testing.T) {
	write, idled := r4LockEnv(t)
	now := time.Now().Add(-5 * time.Minute)
	write(now, r4Bad, r4Kill)
	now = now.Add(5 * time.Second)
	write(now, r4Bad, r4Kill)
	now = now.Add(5 * time.Second)
	write(now, r4Good)
	fanLockTick(now)
	if fanLockErr() == nil {
		t.Fatal("setup: not locked")
	}
	for i := 0; i < 12; i++ {
		now = now.Add(15 * time.Second)
		write(now, r4Good)
	}
	if err := fanUnlock(true, now); err != nil {
		t.Fatalf("unlock refused: %v", err)
	}
	before := *idled
	r5Restart()
	now = now.Add(15 * time.Second)
	write(now, r4Good)
	fanLockTick(now)
	if fanLockErr() != nil {
		t.Fatalf("BUG: a panel restart after the unlock re-locked mining on the kills from before the unlock (Idle again: %v)", *idled > before)
	}
}

// 4. The fan lock is written before the Idle (on purpose: a stopping tuning run must see it). A panel killed
// between the two leaves a lock that says Idle:false, and fanLockTick returns early on any lock: the miner
// keeps hashing (the watchdog stopping it again and again) under a lock that never puts it on Idle. The same
// happens when the Idle request failed once (Goldshell's backend busy): it's never tried again.
func TestR5FanLockWithoutIdleNeverRetried(t *testing.T) {
	_, idled := r4LockEnv(t)
	st := fanLockState{At: time.Now().Unix(), Flagged: "the watchdog flags fan 0 (0 RPM)", Status: 1}
	b, _ := json.Marshal(st)
	writeAtomic(fanLockPath(), b, 0o644) // the first write landed; the panel died before the Idle
	r5Restart()
	oldOnIdle := fanLockOnIdle
	fanLockOnIdle = func() bool { return *idled > 0 }
	defer func() { fanLockOnIdle = oldOnIdle }()
	for i := 0; i < 4; i++ {
		fanLockTick(time.Now())
	}
	s, _ := loadFanLock()
	if *idled == 0 || !s.Idle {
		t.Fatalf("BUG: locked (Idle:%v) but the miner was never put on Idle after the restart (Idle requests: %d)", s.Idle, *idled)
	}
}

// 5. A zero-length or half-written lock file (a power cut on a file system without the rename's ordering, or a
// damaged flash page) reads as "not locked": mining is allowed again. A lock that can't be read should hold.
func TestR5FanLockGarbageFailsOpen(t *testing.T) {
	r4LockEnv(t)
	for _, junk := range []string{"", "{\"at\":17", "\x00\x00\x00"} {
		os.WriteFile(fanLockPath(), []byte(junk), 0o644)
		if fanLockErr() == nil {
			t.Errorf("BUG: fan-lock.json %q (unreadable) lets mining start: the lock is lost", junk)
		}
	}
}

// 6. Zero-length / truncated config files at start: every loader must survive them (no panic) and keep safe values.
func TestR5LoadersTolerateJunk(t *testing.T) {
	d := t.TempDir()
	oldA, oldF, oldP := *authFile, *fanPath, *presetPath
	*authFile, *fanPath, *presetPath = filepath.Join(d, "auth.json"), filepath.Join(d, "fan.json"), filepath.Join(d, "presets.json")
	defer func() { *authFile, *fanPath, *presetPath = oldA, oldF, oldP }()
	for _, junk := range []string{"", "{\"enabled\":tr", "\x00\x00\x00\x00", "null", "[]", "{\"floor\":\"x\"}"} {
		for _, f := range []string{"fan.json", "presets.json", "fan-guard.json", "fan-lock.json", "fan-lock-config.json", "fanpair.json", "schedule.json", "overclock.json", "power.json"} {
			os.WriteFile(filepath.Join(d, f), []byte(junk), 0o644)
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("panic on %q: %v", junk, r)
				}
			}()
			fanMu.Lock()
			fanCfg = defaultFanConfig()
			fanMu.Unlock()
			loadFanConfig()
			fanMu.RLock()
			c := fanCfg
			fanMu.RUnlock()
			if c.AbortC <= 0 || c.AbortC > 90 || c.PollS < 5 {
				t.Errorf("fan.json %q: unsafe config loaded %+v", junk, c)
			}
			fanGuardMu.Lock()
			fanGuardOK = false
			fanGuardMu.Unlock()
			if fl := fanFloor(); fl < fanMin {
				t.Errorf("fan-guard.json %q: floor %d", junk, fl)
			}
			_ = loadPresets()
			_ = loadFanLockConfig()
			fpMu.Lock()
			fpLoaded = false
			fpMu.Unlock()
			if off := fanPairLoad(); off < fanPairMin || off > fanPairMax {
				t.Errorf("fanpair.json %q: offset %d", junk, off)
			}
			schedPath = filepath.Join(d, "schedule.json")
			_ = loadSchedule()
			schedPath = ""
			_ = loadOverclock()
			powerMu.Lock()
			powerRead = false
			powerMu.Unlock()
			_ = getPowerConfig()
		}()
	}
	fpMu.Lock()
	fpLoaded, fpOffset = false, 0
	fpMu.Unlock()
}

// 7. A painted Idle stretch on the schedule (e.g. peak hours), "back to before" after it. The panel restarts during
// the stretch: "what it ran before" was memory only, so the first tick after the start takes the Idle it finds
// as "before". At the end of the stretch the schedule says "back to what it ran before" and applies Idle: the
// miner stays on Idle for good.
func TestR5ScheduleBackAfterRestartStaysIdle(t *testing.T) {
	m := newHLMiner(t)
	schedPath = filepath.Join(t.TempDir(), "schedule.json")
	defer func() { schedPath = ""; schedBefore, schedBeforeBy = nil, "" }()
	now := time.Now()
	if now.Hour() == 0 && now.Minute() < 5 {
		t.Skip("too near midnight for the HH:MM rules")
	}
	hm := func(d time.Duration) string { return now.Add(d).Format("15:04") }
	all := []int{0, 1, 2, 3, 4, 5, 6}
	put := func(rs ...schedRule) {
		b, _ := json.Marshal(schedule{Enabled: true, Rules: rs})
		writeAtomic(schedPath, b, 0o644)
	}
	lastSchKey, lastSchName, schedBefore, schedBeforeBy = -1, "", nil, ""
	put(schedRule{Start: hm(-3 * time.Minute), Days: all, Preset: "Idle"}, schedRule{Start: hm(2 * time.Hour), Days: all, Preset: schedBack})
	scheduleTick() // the painted Idle stretch begins: before = 625 MHz
	if !m.isIdle() {
		t.Fatalf("setup: the stretch didn't idle the miner")
	}
	r5Restart()
	schedBefore, schedBeforeBy = nil, ""
	scheduleTick() // first tick after the panel's start, still inside the stretch
	put(schedRule{Start: hm(-3 * time.Minute), Days: all, Preset: "Idle"}, schedRule{Start: hm(-1 * time.Minute), Days: all, Preset: schedBack})
	scheduleTick() // the stretch is over: back to before
	if m.isIdle() {
		t.Fatalf("BUG: after a panel restart inside a scheduled Idle stretch, \"back to what it ran before\" put the miner on Idle for good (charge %q)", getCharge())
	}
	if got := m.planMHz(); got != 625 {
		t.Errorf("back to %d MHz, not the 625 it ran before the stretch", got)
	}
}
