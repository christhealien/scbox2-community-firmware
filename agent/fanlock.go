package main

// Fan lock. When the OEM's watchdog keeps flagging a fan (a minute in a row), the panel doesn't leave it
// to the watchdog's endless kill-and-restart: it puts the miner on Idle (hash boards powered down: no heat) and
// locks mining until someone has looked at the fans. The Dashboard explains why; unlocking needs the
// warning accepted, and isn't possible while the watchdog still flags a fan. Nothing that would start
// hashing (a preset, Stock, Controls, the schedule, the tuner) works while it's locked.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type fanLockState struct {
	At      int64  `json:"at"`
	Flagged string `json:"flagged"`
	Fans    []int  `json:"fans,omitempty"`
	Status  int    `json:"status"`
	Idle    bool   `json:"idle"` // the miner was put on Idle
}

type fanLockConfig struct {
	Off        bool  `json:"off"`                   // leave fan failures to the OEM's watchdog alone (not recommended)
	UnlockedAt int64 `json:"unlocked_at,omitempty"` // the last unlock (unix s): kept, so a restart doesn't lock again on the kills before it
}

var (
	fanLockMu     sync.Mutex
	fanLockRun    int                                         // watchdog readings in a row that flag a fan
	fanLockIdle   = func() error { return idleAction(model) } // lane B3: model-aware park (Box II API board-off, UNPROVEN)
	fanLockAfter  = 4                                         // readings (15 s apart): about a minute
	errFanLocked  = errors.New("mining is locked after a fan problem: see the Dashboard (unlock it there once the fans are fixed)")
	fanLockNotify = func(msg string) { notifyAlways(evFan, msg) }
	fanUnlockAt   int64 // the last unlock: kills and flags from before it were what the lock was about
	fanLockOnIdle = minerOnIdlePlan
)

func fanLockPath() string    { return filepath.Join(filepath.Dir(*authFile), "fan-lock.json") }
func fanLockCfgPath() string { return filepath.Join(filepath.Dir(*authFile), "fan-lock-config.json") }

// fanLockMem: the lock while its file couldn't be written (a full or read-only config partition): it holds
// until the unlock (or a restart), instead of not at all, with an Idle and a notification at every reading.
var (
	fanLockMemMu sync.Mutex
	fanLockMem   *fanLockState
)

// fanLockStore: the lock's state on disk, else in memory.
func fanLockStore(st fanLockState) {
	b, _ := json.Marshal(st)
	err := writeAtomic(fanLockPath(), b, 0o644)
	fanLockMemMu.Lock()
	if fanLockMem = nil; err != nil {
		fanLockMem = &st
	}
	fanLockMemMu.Unlock()
}

func loadFanLock() (fanLockState, bool) {
	var s fanLockState
	b, err := os.ReadFile(fanLockPath())
	if err != nil {
		if os.IsNotExist(err) {
			fanLockMemMu.Lock()
			defer fanLockMemMu.Unlock()
			if fanLockMem != nil {
				return *fanLockMem, true
			}
		}
		return s, !os.IsNotExist(err)
	}
	json.Unmarshal(b, &s) // a damaged one (a power cut, a hand edit) still locks, as the pool guard's hold does
	return s, true
}

func loadFanLockConfig() fanLockConfig {
	var c fanLockConfig
	if b, err := os.ReadFile(fanLockCfgPath()); err == nil {
		json.Unmarshal(b, &c)
	}
	return c
}

func saveFanLockConfig(c fanLockConfig) error {
	b, _ := json.Marshal(c)
	return writeAtomic(fanLockCfgPath(), b, 0o644)
}

// fanLockErr: an error while mining is locked (for everything that would start hashing).
func fanLockErr() error {
	if _, ok := loadFanLock(); ok {
		return errFanLocked
	}
	return nil
}

func fanLockView() map[string]any {
	v := map[string]any{"enabled": !loadFanLockConfig().Off}
	if s, ok := loadFanLock(); ok {
		v["locked"] = s
	}
	return v
}

// fanLockTick: one watchdog reading (from minerHealthLoop, every 15 s).
func fanLockTick(now time.Time) {
	if loadFanLockConfig().Off {
		return
	}
	if s, locked := loadFanLock(); locked {
		// locked, but the miner wasn't put on Idle (the panel stopped between the lock and the Idle, or the
		// Idle request failed): try again
		if !s.Idle && !fanLockOnIdle() && fanLockIdle() == nil {
			s.Idle = true
			setCharge("idle")
			recordMark("P", "idle (fan lock)")
			fanLockStore(s)
			activity.emit("🌀 fan lock: the miner is now on Idle")
		}
		return
	}
	w, ok := readWatchdog(now)
	// a fan flagged for a minute in a row, or the watchdog stopping the miner again and again for one
	// (each flag lasts only a second or two: a 15 s reading mostly misses it while the kills go on)
	fanLockMu.Lock()
	since := fanUnlockAt
	fanLockMu.Unlock()
	if u := loadFanLockConfig().UnlockedAt; u > since {
		since = u // an unlock from before a restart
	}
	if since > now.Unix()+60 {
		// an unlock "in the future": it was on another clock (saved before a reboot, and the clock reads 1970
		// again until NTP or the browser sets it; or a clock set from a browser and since stepped back by a time
		// server). The watchdog's lines carry this clock: against that unlock no kill would ever count, and
		// the lock could never engage on them. The watchdog's lines from before it are in the future too and
		// don't count (parseWatchdog).
		since = 0
	}
	if fw, flagged := flaggedView(w); ok && flagged && w.Status == 0 && w.Kills >= 2 && w.LastKill > since && fw.At > since {
		fanLockEngage(fw, now)
		return
	}
	fanLockMu.Lock()
	if !ok || w.Status == 0 {
		fanLockRun = 0
		fanLockMu.Unlock()
		return
	}
	fanLockRun++
	run := fanLockRun
	fanLockMu.Unlock()
	if run < fanLockAfter {
		return
	}
	fanLockEngage(w, now)
}

func fanLockEngage(w watchdogView, now time.Time) {
	st := fanLockState{At: now.Unix(), Flagged: w.Flagged, Fans: w.Fans, Status: w.Status}
	// the lock first: a tuning run told to stop checks it at its end and leaves the miner where this puts it
	// (written again below with the Idle's outcome)
	fanLockStore(st)
	safetyStop() // writes on their way (Controls, Stock, a run's step or put-back) are refused: see safetyGen
	// a tuning run can't go on: stop it (it would put its own setting back)
	tunerMu.Lock()
	t := tunerCur
	tunerMu.Unlock()
	if t != nil {
		t.StopForSoon("the fan lock: "+w.Flagged, 2*time.Second) // (a frozen run doesn't hold up the Idle below)
	}
	sctEndFor("The fan lock: " + w.Flagged) // and the strong-chip test (its raised chips go back), as the other guards' Idle does
	if err := fanLockIdle(); err == nil {
		st.Idle = true
		setCharge("idle")
		recordMark("P", "idle (fan lock)")
	}
	fanLockStore(st)
	check := "check the fans"
	if learnedSuspect(w) {
		check = "that fan turns as fast as its pair partner, so it most likely works and the watchdog's recorded speed is wrong: Controls & fan → Fan check → Re-run " + oemOf() + " fan test"
	}
	msg := fmt.Sprintf("mining stopped and locked: %s (%s watchdog keeps stopping mining while a fan looks failed). The miner is on Idle; %s, then unlock it on the Dashboard", w.Flagged, oemOf(), check) + idleActionNote()
	if !st.Idle {
		msg = fmt.Sprintf("mining locked: %s, and the miner couldn't be put on Idle. %s, then unlock it on the Dashboard", w.Flagged, check)
	}
	activity.emit("🌀 " + msg)
	fanLockNotify(msg)
}

// fanUnlock: the warning was accepted on the Dashboard. Refused while the watchdog still flags a fan.
func fanUnlock(accepted bool, now time.Time) error {
	if _, ok := loadFanLock(); !ok {
		return nil
	}
	if !accepted {
		return fmt.Errorf("accept the warning first")
	}
	if w, ok := readWatchdog(now); ok && w.Status != 0 {
		if learnedSuspect(w) {
			return fmt.Errorf("%s watchdog still %s, but that fan turns as fast as its pair partner: most likely a wrong recorded speed. Controls & fan → Fan check → Re-run %s fan test, then unlock", OemOf(), strings.TrimPrefix(w.Flagged, "the watchdog "), oemOf())
		}
		return fmt.Errorf("%s watchdog still %s: a fan that reads 0 RPM isn't turning or its speed wire isn't connected; fans that turn but are flagged are slower than its figures, which Controls & fan → Fan check can fit to them (then reboot)", OemOf(), strings.TrimPrefix(w.Flagged, "the watchdog "))
	}
	if err := os.Remove(fanLockPath()); err != nil && fileExists(fanLockPath()) { // (no file: held in memory only)
		return err
	}
	fanLockMemMu.Lock()
	fanLockMem = nil
	fanLockMemMu.Unlock()
	fanLockMu.Lock()
	fanLockRun = 0
	fanUnlockAt = now.Unix()
	fanLockMu.Unlock()
	c := loadFanLockConfig()
	c.UnlockedAt = now.Unix()
	saveFanLockConfig(c)
	activity.emit("🌀 fan lock removed by hand (the warning was accepted); the miner stays on Idle until a preset or " + stockWord() + " is applied")
	return nil
}
