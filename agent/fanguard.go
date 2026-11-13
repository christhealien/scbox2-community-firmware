package main

// Low fan speeds. Fans better than stock may run very low, so the panel's floor is 10 % (it was 30 % for
// fan curves and the tuner's fan hold, which the tuner's own "never below 20 %" couldn't get under).
// What stops it going too low is the miner itself: the OEM's watchdog measures every fan and stops mining
// while one looks failed (a fan that stops turning at low duty reads 0 RPM; one that turns too slowly for the
// firmware is flagged too). When the watchdog flags a turning fan while the panel runs the fans below 50 %,
// the panel raises its own floor 10 % above what tripped it and keeps it there until the fan settings are
// saved again, so the miner isn't stopped again and again.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const fanGuardMax = 50 // the guard never forces more than this: above it a flagged fan is a fan problem

// --- mining-blind guard (no board temperature at all while hashing) ---
//
// The OEM's fan controller (fanctrl) runs the fans at full when it loses every sensor but never shuts the
// boards down for heat (traced). So the panel parks the miner itself after a spell with no temperature at
// all while hashing. The SC Box II is stricter: R11 F4 — its fanctrl likewise never stops without a reading,
// and the panel cannot see the boards' heat, so it parks after 30 s rather than SC Lite's 3 minutes. With no
// model loaded the default profile is SC Lite, so these keep SC Lite's exact figures and behaviour.

func noTempIdleWait() time.Duration {
	if model.Plan == planBox2V {
		return 30 * time.Second
	}
	return 3 * time.Minute
}

func noTempIdleTicks() int {
	if model.Plan == planBox2V {
		return 2 // two readings in a row, so a single glitched poll doesn't trip the stricter 30 s window
	}
	return 6
}

func noTempIdleMsg() string {
	if model.Plan == planBox2V {
		return "no board temperature for 30 seconds while hashing (every sensor fails to read): the miner is set to Idle so nothing overheats unseen. Check the hash boards' cables, then apply " + stockWord() + " or a preset"
	}
	return "no board temperature for 3 minutes while hashing (every sensor fails to read): the miner is set to Idle so nothing overheats unseen. Check the hash boards' cables, then apply " + stockWord() + " or a preset"
}

// noTempStop parks the miner for the mining-blind guard. On a model with a firmware idle plan (SC Lite) it
// is the full thermal-guard Idle, unchanged. On the SC Box II there is no stock idle plan yet: idleAction is
// A1's stub (lane B3 wires the real board-off via the miner API). We call idleAction(model) and tolerate its
// "no idle plan" error gracefully — log it and leave the firmware's own 86 °C cut-off as the backstop, never
// crash — so this guard is in place the moment B3 lands a working idleAction. A seam for tests.
var noTempStop = func(why string) error {
	if model.Plan == planBox2V {
		end := guardParkBegin() // (as thermalIdle: no wake write lands behind the park)
		defer end()
		if err := idleAction(model); err != nil {
			activity.emit(fmt.Sprintf("🔥 thermal guard: %s, but %v — the panel cannot stop the boards on this model yet; the firmware's own %.0f °C cut-off still applies", why, err, model.TempCutoffC))
			return err
		}
		heatIdleWhy.Store(why)
		heatIdleNoted(fanNow().Unix())
		heatIdleSince.Store(fanNow().Unix())
		return nil
	}
	return thermalIdle(why)
}

type fanGuardState struct {
	Floor int    `json:"floor"`
	At    int64  `json:"at"`
	Why   string `json:"why"`
}

var (
	fanGuardMu  sync.Mutex
	fanGuardSt  fanGuardState
	fanGuardOK  bool
	readWDGuard = func() (watchdogView, bool) { return readWatchdog(time.Now()) }
)

func fanGuardPath() string { return filepath.Join(filepath.Dir(*authFile), "fan-guard.json") }

func fanGuardLoad() fanGuardState {
	fanGuardMu.Lock()
	defer fanGuardMu.Unlock()
	if !fanGuardOK {
		if b, err := os.ReadFile(fanGuardPath()); err == nil {
			json.Unmarshal(b, &fanGuardSt)
			// only fanGuardCheck writes it (at most fanGuardMax): a damaged or hand-edited file can't set more
			// (a floor over 100 became a fan level the driver takes as "PWM off")
			fanGuardSt.Floor = clampInt(fanGuardSt.Floor, 0, fanGuardMax)
		}
		fanGuardOK = true
	}
	return fanGuardSt
}

// fanFloor: the lowest fan % the panel sets now: 10 %, or higher after the watchdog flagged a fan.
func fanFloor() int {
	if g := fanGuardLoad(); g.Floor > fanMin {
		return g.Floor
	}
	return fanMin
}

// fanGuardCheck: after setting the fans to pct, look at what the watchdog says. Returns the new floor when
// it raised it (0 when not).
func fanGuardCheck(pct int) int { return fanGuardLevels(pct, pct, 0) }

// fanGuardLevels: fanGuardCheck with fans 0+1 at a and fans 2+3 at b (a fan pair offset). Each flagged fan is
// judged at its own pair's level; shiftB is how far a raised floor puts fans 2+3 above it (a positive offset
// runs them that much over fans 0+1, which run at the floor), so a flag on fans 2+3 at b raises the floor to
// b-shiftB+10.
func fanGuardLevels(a, b, shiftB int) int {
	if a >= fanGuardMax && b >= fanGuardMax {
		return 0
	}
	v, ok := readWDGuard()
	if !ok || v.Status == 0 {
		return 0
	}
	if learnedSuspect(v) {
		return 0 // a working fan flagged on a wrong recorded speed: more speed doesn't help (Re-run fixes it)
	}
	turning, at, base := false, -1, -1
	for i, r := range v.Fans {
		if v.Status&(1<<i) == 0 {
			continue
		}
		if r > 0 {
			turning = true
		}
		lvl, sh := a, 0
		if i >= 2 {
			lvl, sh = b, shiftB
		}
		if lvl >= fanGuardMax {
			continue // above it a flagged fan is a fan problem
		}
		if len(v.Duty) > 0 && (i >= len(v.Duty) || v.Duty[i] > lvl+1 || v.Duty[i] < lvl-1) {
			continue // the flag was at another fan level (before this one took): not this level's verdict
		}
		at, base = max(at, lvl), max(base, lvl-sh)
	}
	if at < 0 {
		return 0
	}
	if !turning && at >= 30 {
		return 0 // a fan at 0 RPM at a normal speed: its wiring or the fan, not the floor
	}
	pct := at
	floor := min(fanGuardMax, max(fanFloor(), base)+10)
	if floor <= fanFloor() {
		return 0
	}
	g := fanGuardState{Floor: floor, At: time.Now().Unix(),
		Why: fmt.Sprintf("at %d %% %s", pct, v.Flagged)}
	fanGuardMu.Lock()
	fanGuardSt, fanGuardOK = g, true
	fanGuardMu.Unlock()
	if b, err := json.Marshal(g); err == nil {
		writeAtomic(fanGuardPath(), b, 0o644)
	}
	activity.emit(fmt.Sprintf("🌀 fans: %s watchdog flagged a fan %s (it stops mining while a fan looks failed): the fans now stay at %d %% or more. Saving the fan settings again clears this.", oemOf(), g.Why, floor))
	return floor
}

// fanGuardClear: the user saved the fan settings: start from 10 % again.
func fanGuardClear() {
	fanGuardMu.Lock()
	fanGuardSt, fanGuardOK = fanGuardState{}, true
	fanGuardMu.Unlock()
	os.Remove(fanGuardPath())
}
