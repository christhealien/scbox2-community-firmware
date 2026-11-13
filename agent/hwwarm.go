package main

// Warm-up HW errors. Each time the chips start (the mining program starts, or a stopped board is enabled
// again), the miner's driver initialises them at 500 MHz and ramps straight to the plan's clock while they
// are still cold. For the first minute or so a few dozen nonces come back bad (seen on the SC Box II: ~24 in
// the first 30 s, then ~1 per minute), and over so few nonces that reads as 10 % at first.
//
//   - The activity feed doesn't list those one by one: it waits for the warm-up to end and sums them up in
//     one line, then lists HW errors as before.
//   - The dashboard keeps the miner's own totals, and adds how many of them were warm-up errors and the
//     HW % without them ("since warm-up"), which is the figure that says how the board is doing.
//
// The miner's own counters are untouched.

import (
	"fmt"
	"sync"
	"time"
)

const hwWarmFor = 3 * time.Minute

var (
	hwWarmMu    sync.Mutex
	hwWarmUntil time.Time
	hwWarmBase  int    // HW errors when the running warm-up began
	hwWarmWhy   string // what started it
	hwWarmDone  int    // warm-up errors of this miner run, from finished warm-ups
	hwWarmLast  int    // the HW total at the last look (a drop = a new miner run)
)

func hwWarmStart(now time.Time, hw int, why string, left time.Duration) {
	if hwWarmUntil.IsZero() {
		hwWarmBase = hw
	}
	hwWarmUntil, hwWarmWhy = now.Add(left), why
}

// hwWarmConnect: the panel's first look at a miner (boot, or the miner answers again after being down).
// A run younger than the warm-up is still warming up, and every error it has so far belongs to it.
func hwWarmConnect(d dashboard, lastHW *int) {
	hwWarmMu.Lock()
	defer hwWarmMu.Unlock()
	run := time.Duration(d.Elapsed) * time.Second
	if d.HWErrors < hwWarmLast || run < hwWarmFor {
		hwWarmDone, hwWarmUntil = 0, time.Time{} // a new miner run
	}
	hwWarmLast = d.HWErrors
	if run < hwWarmFor {
		hwWarmBase, hwWarmUntil, hwWarmWhy = 0, time.Now().Add(hwWarmFor-run), "the mining program started"
		*lastHW = d.HWErrors
	}
}

// hwWarmTick runs on every dashboard poll before the HW-error line. While a warm-up runs it moves lastHW
// along (so nothing is listed); at its end it writes the summary.
func hwWarmTick(prev, d dashboard, lastHW *int) {
	hwWarmMu.Lock()
	defer hwWarmMu.Unlock()
	now := time.Now()
	if d.HWErrors < hwWarmLast { // the counters went back: a new miner run
		hwWarmDone, hwWarmUntil = 0, time.Time{}
	}
	hwWarmLast = d.HWErrors
	if prev.OK && d.Elapsed > 0 && d.Elapsed < prev.Elapsed {
		hwWarmBase, hwWarmUntil, hwWarmWhy = 0, now.Add(hwWarmFor), "the mining program started"
	} else {
		was := map[int]bool{}
		for _, b := range prev.Boards {
			was[b.ID] = b.Enabled
		}
		for _, b := range d.Boards {
			if e, ok := was[b.ID]; ok && !e && b.Enabled {
				hwWarmStart(now, *lastHW, fmt.Sprintf("board %d was enabled again", b.ID), hwWarmFor)
			}
		}
	}
	if hwWarmUntil.IsZero() {
		return
	}
	if d.HWErrors < hwWarmBase {
		hwWarmBase = 0
	}
	if now.Before(hwWarmUntil) {
		*lastHW = d.HWErrors
		return
	}
	n := d.HWErrors - hwWarmBase
	if n > 0 {
		activity.emit(fmt.Sprintf("⚠ warm-up: %d HW error(s) in the first %s after %s — normal while the cold chips ramp to full clock; HW errors are listed one by one again from now (total %d, %.2f%%)",
			n, hwWarmFor, hwWarmWhy, d.HWErrors, d.HWPct))
		hwWarmDone += n
	}
	*lastHW = d.HWErrors
	hwWarmUntil = time.Time{}
}

// hwWarmView: for the dashboard. warm = warm-up errors of this miner run (finished warm-ups plus the running
// one), left = seconds of warm-up still to go (0 = none running), steady = HW % without the warm-up errors
// (-1 while it can't be worked out).
func hwWarmView(hw int, pct float64) (warm, left int, steady float64) {
	hwWarmMu.Lock()
	defer hwWarmMu.Unlock()
	warm = hwWarmDone
	if !hwWarmUntil.IsZero() {
		if hw >= hwWarmBase {
			warm += hw - hwWarmBase
		}
		if l := time.Until(hwWarmUntil); l > 0 {
			left = int(l.Seconds()) + 1
		}
	}
	if warm > hw {
		warm = hw
	}
	steady = -1
	if pct > 0 && hw > 0 {
		total := float64(hw) * 100 / pct // nonces + HW errors
		if rest := total - float64(warm); rest > 0 {
			steady = float64(hw-warm) * 100 / rest
		}
	} else if hw == 0 {
		steady = 0
	}
	return
}
