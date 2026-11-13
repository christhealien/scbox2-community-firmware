package main

// Fan pair offset (Controls & fan → Fan pair offset). The SC Lite has four fan headers but only two
// speed signals (traced and measured): the board config gives fan0+fan1 PWM channel 0 (pin 72) and
// fan2+fan3 channel 1 (pin 71); /sys/kernel/debug/pwm shows only pwm-0 (fan1) and pwm-1 (fan3) in use,
// and writing fan1's level moved fan0 with it. Four separate speeds aren't possible; two pairs are.
//
// Everything that sets the fans (the panel's curve, Controls, presets and the tuner through the power
// plan; intminer, which gives all four fans the plan's one value; the OEM's fanctrl, which steps each
// fan from its own current level) keeps setting pair A as before. This loop then keeps pair B at
// pair A + offset, so a fan curve, a preset or a tuning run all work as they do, with pair B offset.
// The point is noise: fans at nearly the same speed beat against each other (a slow "wow-wow"); a clear
// offset between the pairs pushes that beat out of the annoying range.
//
// Safety: pair B never goes under the panel's fan floor (fanguard.go); when pair A is at 95 % or more
// (the OEM's over-temperature 100 %, or anyone's emergency), a negative offset isn't applied; on Idle
// and while mining is locked after a fan problem the fans are left to the OEM fan control. Both fans of a pair
// always get the same level, so the OEM's fan check sees each fan's real duty.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	fanPairMin, fanPairMax = -20, 20
	fanPairEmergency       = 95
)

var (
	fanSysDir = "/sys/class/fans" // tests point it at a temp dir

	fpMu       sync.Mutex
	fpOffset   int
	fpLoaded   bool
	fpLastNote string
	fpLastB    = -1
	fpIdle     bool
	fpIdleAt   time.Time
)

func fanPairPath() string { return filepath.Join(filepath.Dir(*authFile), "fanpair.json") }

// fanGroupPartner returns the other fan sharing fan i's PWM group when the group is a pair (fan0/fan1 and
// fan2/fan3 on an SC Lite; fan0/fan1 on an SC Box II), or -1 when i has no single pair partner (a lone fan,
// a group of more than two, or an out-of-range index). Driven by the model's FanGroups rather than the old
// hardcoded i^1, which assumed four fans in two pairs.
func fanGroupPartner(i int) int {
	for _, g := range model.FanGroups {
		if len(g) != 2 {
			continue
		}
		if g[0] == i {
			return g[1]
		}
		if g[1] == i {
			return g[0]
		}
	}
	return -1
}

func fanPairLoad() int {
	if featOff(model.Feat.FanPairOffset) {
		return 0 // no fan-pair offset on this model: there is only one fan group, and fan2/fan3 are not fans here
	}
	fpMu.Lock()
	defer fpMu.Unlock()
	if !fpLoaded {
		fpLoaded = true
		var c struct {
			Offset int `json:"offset"`
		}
		if b, err := os.ReadFile(fanPairPath()); err == nil && json.Unmarshal(b, &c) == nil {
			fpOffset = clampInt(c.Offset, fanPairMin, fanPairMax)
		}
	}
	return fpOffset
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func fanLvl(name string) (int, error) {
	b, err := os.ReadFile(filepath.Join(fanSysDir, name, "lvl"))
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(b)))
}

func setFanLvl(name string, v int) error {
	v = clampInt(v, fanFloor(), 100) // the driver turns a huge or 0 level into "PWM off": never write one
	return os.WriteFile(filepath.Join(fanSysDir, name, "lvl"), []byte(strconv.Itoa(v)), 0644)
}

// fanPairTarget: pair B's level for pair A's level a with offset off.
func fanPairTarget(a, off, floor int) int {
	if off < 0 && a >= fanPairEmergency {
		return a // never hold pair B back when the fans are being driven hard
	}
	b := a + off
	if b < floor {
		b = floor
	}
	if off < 0 && b > a {
		b = a // a floor above pair A doesn't make a "lower" offset raise pair B past A
	}
	return clampInt(b, 0, 100)
}

// fanPairTick sets pair B when it's off its target (every second, and right after each power-plan write). Returns a short status note.
func fanPairTick(off int, skip string) string {
	if featOff(model.Feat.FanPairOffset) {
		// HARD RULE: this loop writes fan2/fan3. On a one-group model (SC Box II) those sysfs nodes are the
		// VOLTAGE PWM, not fans — never touch them here. The feature is off, so there is nothing to do.
		return ""
	}
	lv := make([]int, 4)
	for i := range lv {
		v, err := fanLvl(fmt.Sprintf("fan%d", i))
		if err != nil {
			return "can't read the fan levels: " + shortErr(err)
		}
		lv[i] = v
	}
	if off == 0 {
		return ""
	}
	if skip != "" {
		return skip
	}
	a := lv[1] // the last of the pair written by the OEM's programs, so what the pair actually runs at
	// (clamped as setFanLvl writes it, so the check below and the note see the level really written)
	b := clampInt(fanPairTarget(a, off, pairFloor()), fanFloor(), 100)
	// a board at 88 °C or more: pair B isn't held back, and never turned down
	if heatGuardActive() {
		if off < 0 && b < a {
			b = a
		}
		if c := min(lv[2], lv[3]); b < c {
			b = c
		}
	}
	if lv[2] == b && lv[3] == b {
		return fmt.Sprintf("fans 0+1 %d %%, fans 2+3 %d %%", a, b)
	}
	if err := setFanLvl("fan2", b); err != nil {
		return "can't set fans 2+3: " + shortErr(err)
	}
	if err := setFanLvl("fan3", b); err != nil { // fan3 last: the pair runs at the last level written
		return "can't set fans 2+3: " + shortErr(err)
	}
	fpMu.Lock()
	changed := fpLastB != b
	fpLastB = b
	fpMu.Unlock()
	if changed && b == a && off < 0 && a >= fanPairEmergency {
		activity.emit(fmt.Sprintf("🌀 fan pair offset paused: fans at %d %%, both pairs run full", a))
	}
	return fmt.Sprintf("fans 0+1 %d %%, fans 2+3 %d %%", a, b)
}

// pairFloor: pair B's lowest level: the panel's fan floor, or the tuner's "never below" while its fan hold runs.
func pairFloor() int {
	fl := fanFloor()
	if tunerHoldsFans() {
		tunerMu.Lock()
		t := tunerCur
		tunerMu.Unlock()
		if t != nil {
			fl = max(fl, t.cfg.FanMinPct)
		}
	}
	return fl
}

// fanGuardPair: fanGuardCheck for fans set to pct, with each pair judged at its own level (an offset runs
// pair B over or under pct, and the watchdog's flag on fans 2+3 is at pair B's duty).
func fanGuardPair(pct int) int {
	off := fanPairLoad()
	if off == 0 {
		return fanGuardCheck(pct)
	}
	// each pair judged at its own level (fans 2+3 above fans 0+1 with a positive offset, below with a negative one)
	b := clampInt(fanPairTarget(pct, off, pairFloor()), fanFloor(), 100)
	return fanGuardLevels(pct, b, max(off, 0))
}

// fanPairSkip: why the offset is left alone right now ("" = apply it).
func fanPairSkip() string {
	if _, ok := loadFanLock(); ok {
		return "left to " + oemFanCtl() + " while mining is locked after a fan problem"
	}
	if fanTestRunning() {
		// The OEM's start-up fan test records each fan's speed at each level: an offset now would make
		// it record fans 2+3 at the wrong level (and later flag or pass them wrongly)
		return "left to " + oemFanCtl() + " during its start-up fan test"
	}
	fpMu.Lock()
	stale := time.Since(fpIdleAt) > 30*time.Second
	if stale {
		fpIdleAt = time.Now() // one read at a time: others use the last answer meanwhile
	}
	fpMu.Unlock()
	if stale {
		idle := false
		if raw, err := stockGet("/mcb/setting"); err == nil {
			var cur map[string]any
			if json.Unmarshal(raw, &cur) == nil {
				idle = minerIdle(cur)
			}
		}
		fpMu.Lock()
		fpIdle, fpIdleAt = idle, time.Now()
		fpMu.Unlock()
	}
	fpMu.Lock()
	defer fpMu.Unlock()
	if fpIdle {
		return "left to " + oemFanCtl() + " while the miner is on Idle"
	}
	return ""
}

// fanPairKick: after a power-plan write the miner applies the plan's one fan level a moment later; check
// pair B a few times in the next seconds instead of waiting for the loop.
var fpKickGen int64

func fanPairKick() {
	if featOff(model.Feat.FanPairOffset) {
		return // see fanPairTick: never drive fan2/fan3 on a one-group model
	}
	if fanPairLoad() == 0 {
		return
	}
	gen := atomic.AddInt64(&fpKickGen, 1)
	go func() {
		fpMu.Lock()
		fpIdleAt = time.Time{} // the write may have put the miner on Idle or woken it: ask again
		fpMu.Unlock()
		for _, d := range []time.Duration{150, 350, 500, 1000, 1500} {
			time.Sleep(d * time.Millisecond)
			if atomic.LoadInt64(&fpKickGen) != gen {
				return // a newer write: its own kick takes over
			}
			fanPairRun()
		}
	}()
}

var fpRunMu sync.Mutex // one pair-B update at a time (the loop, kicks, and the handler's offset change)

func fanPairRun() {
	if featOff(model.Feat.FanPairOffset) {
		return // see fanPairTick: never drive fan2/fan3 on a one-group model
	}
	fpRunMu.Lock()
	defer fpRunMu.Unlock()
	off := fanPairLoad()
	skip := ""
	if off != 0 {
		skip = fanPairSkip()
	}
	note := fanPairTick(off, skip)
	fpMu.Lock()
	fpLastNote = note
	fpMu.Unlock()
}

func fanPairLoop() {
	for {
		time.Sleep(time.Second)
		fanPairRun()
	}
}

func handleFanPair(w http.ResponseWriter, r *http.Request) {
	// Gated off on models with a single fan group (SC Box II): the offset feature drives fan2/fan3, which on
	// such a board are the voltage PWM, not fans. Refuse cleanly (GET and POST) and never touch them.
	if !featGate(w, model.Feat.FanPairOffset, "fan pair offset") {
		return
	}
	if r.Method == http.MethodPost {
		if getCharge() == "tuner" { // a run's fan holds and results would change under it
			writeJSON(w, map[string]any{"ok": false, "err": "the tuner is running: stop it first, or change the offset before the next run"})
			return
		}
		var in struct {
			Offset *int `json:"offset"`
		}
		if !decodeIn(w, r, &in) {
			return
		}
		if in.Offset == nil || *in.Offset < fanPairMin || *in.Offset > fanPairMax {
			writeJSON(w, map[string]any{"ok": false, "err": fmt.Sprintf("the offset must be %d to %+d %%", fanPairMin, fanPairMax)})
			return
		}
		b, _ := json.Marshal(map[string]int{"offset": *in.Offset})
		if err := writeAtomic(fanPairPath(), b, 0644); err != nil {
			writeJSON(w, map[string]any{"ok": false, "err": "could not save: " + err.Error()})
			return
		}
		fpRunMu.Lock() // a tick that read the old offset can't write pair B after this
		fpMu.Lock()
		old := fpOffset
		fpOffset, fpLoaded, fpLastB = *in.Offset, true, -1
		fpMu.Unlock()
		if *in.Offset == 0 && old != 0 {
			// back to equal: pair B takes pair A's level now (the next fan change would do it anyway); not
			// during the fan test, on Idle or while locked, and not turned down while a board is this hot
			if a, err := fanLvl("fan1"); err == nil && fanPairSkip() == "" {
				if c, err := fanLvl("fan3"); err == nil && heatGuardActive() && c > a {
					a = c
				}
				setFanLvl("fan2", a)
				setFanLvl("fan3", a)
			}
		}
		fpRunMu.Unlock()
		if *in.Offset == 0 && old != 0 {
			activity.emit("🌀 fan pair offset off: all four fans run together again")
		} else if *in.Offset != old {
			activity.emit(fmt.Sprintf("🌀 fan pair offset %+d %%: fans 2+3 run %+d %% from fans 0+1", *in.Offset, *in.Offset))
		}
		fanPairKick()
	}
	off := fanPairLoad()
	v := map[string]any{"ok": true, "offset": off, "min": fanPairMin, "max": fanPairMax}
	var lv []int
	for i := 0; i < 4; i++ {
		if x, err := fanLvl(fmt.Sprintf("fan%d", i)); err == nil {
			lv = append(lv, x)
		}
	}
	if len(lv) == 4 {
		v["levels"] = lv
	}
	fpMu.Lock()
	if fpLastNote != "" {
		v["note"] = fpLastNote
	}
	fpMu.Unlock()
	writeJSON(w, v)
}
