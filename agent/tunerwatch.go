package main

// The tuner's watchdog. A run that stops making progress (a hook frozen on a lock, as in v1.5.15, or a call
// that never returns) used to stay "in charge" for good: Stop couldn't reach it, Restart mining and firmware
// updates were refused, and only a reboot cleared it. superviseTuner runs the engine and watches its
// heartbeat (every poll, log line and retry beats it; the longest legitimate gap is one backend call or the
// chip-trim apply's 30 s wait, well under the limit). With no sign of life for tunerStuckAfter it ends the run
// from outside: the engine's hooks become no-ops (so a run that wakes up later can't change the miner), "in
// charge" is released, the miner is put back on the run's baseline, and the owner is told. Nothing here needs
// the engine's own lock.

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"runtime/debug"
	"strings"
	"time"
)

var (
	tunerStuckAfter = 10 * time.Minute
	tunerWatchEvery = 30 * time.Second
	// tunerEnded: the end of a run that ended by itself (replaced in tests)
	tunerEnded = func(t *tunerEngine) {
		if t.restoreFailed {
			// the miner couldn't be put back: the marker stays, so the panel's boot-time recovery puts it
			// back on the baseline after a reboot (unless something else was chosen by then); say what to do now
			markTunerEnded()
			notify(evTuning, "tuning ended but the miner couldn't be put back on its setting (its backend didn't answer): apply "+stockWord()+" or a preset (Presets tab), or reboot the miner")
		} else {
			clearTunerMarker()
		}
		// run ended (finish/abort/stop): a preset or Stock that was in charge before is again (the miner went
		// back to its setting), unless the run left it on a confirmed setting; else back to manual. Only from
		// "tuner": a safety stop's "idle" set meanwhile stays.
		prev := t.prevCharge
		if t.confirmed || t.restoreFailed || !(prev == "stock" || strings.HasPrefix(prev, "preset:")) {
			prev = ""
		}
		swapCharge("tuner", prev)
		saveTunerRun(t.exportBundle()) // persist log/results/csv for the Tuner-log panel + exports
		notify(evTuning, t.endText())
	}
	tunerForceApply = applyTunePlan
)

// superviseTuner runs t (begun) to its end, or ends it from outside when it's stuck. Blocks.
func superviseTuner(t *tunerEngine) {
	ran := make(chan any, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("tuner: panic: %v\n%s", r, debug.Stack())
				ran <- r
				return
			}
			ran <- nil
		}()
		t.Run()
	}()
	tk := time.NewTicker(tunerWatchEvery)
	defer tk.Stop()
	for {
		select {
		case r := <-ran:
			if r != nil {
				t.mu.Lock()
				ended := !t.running && !t.finishedAt.IsZero()
				t.mu.Unlock()
				if ended { // Run's own end ran in full as it unwound (the miner put back): end it as usual, not over that
					msg := fmt.Sprintf("the tuning run failed (%v): it ended and put the miner back itself; please save the tuner log and report it", r)
					log.Print("tuner: " + msg)
					activity.emit("🔧 " + msg)
					tunerEnded(t)
					return
				}
				tunerForceEnd(t, fmt.Sprintf("it failed: %v", r))
				return
			}
			tunerEnded(t)
			return
		case <-tk.C:
			if idle := beatAge(t.beat.Load()); idle >= tunerStuckAfter {
				tunerForceEnd(t, fmt.Sprintf("it showed no progress for %.0f min", idle.Minutes()))
				return // the frozen goroutine is left behind: its hooks do nothing now
			}
		}
	}
}

// tunerForceEnd: the run is stuck. Its hooks stop working, the charge is released and the baseline put back.
func tunerForceEnd(t *tunerEngine, why string) {
	t.dead.Store(true)
	msg := "the tuning run was stuck (" + why + ")"
	put := ""
	trimsFailed := 0
	if !t.cfg.DryRun { // its chip trims (kept, or being tried) go back first, as on a Stop: the baseline below applies the file
		if b, err := os.ReadFile(tunerMarkerPath()); err == nil {
			var m tunerMarker
			if json.Unmarshal(b, &m) == nil && !m.Dry {
				undone, failed := tunerTrimsBack(m.Trims, func(chain, chip int, mhz float64) error {
					return cwSetTrimWithin(5*time.Second, chain, chip, mhz)
				})
				if undone > 0 {
					put += fmt.Sprintf("; the %d chip trim(s) it made came off", undone)
				}
				if failed > 0 {
					put += fmt.Sprintf("; %d chip trim(s) it made couldn't be taken off (Controls → Chip trims)", failed)
				}
				trimsFailed = failed
			}
		}
	}
	if b := t.baseAtom.Load(); b != nil && !t.cfg.DryRun && getCharge() == "tuner" {
		if err := miningHoldErr(); err != nil {
			put += "; the miner stays where the fan lock or the pool guard put it"
		} else if genMoved(t.planGen) {
			put += "; the miner stays on the Idle the thermal guard put it on"
		} else if err := tunerForceApply(b.hw()); err != nil { // (Box II: the engine's rank as the OEM's level)
			put += fmt.Sprintf("; putting %s back failed (%v): apply "+stockWord()+" or a preset (Presets tab)", b.text(), err)
		} else {
			put += "; the miner is back on " + b.text()
			clearTunerMarker()
		}
	}
	// the marker stays only if the miner couldn't be put back: the run is over, so the next start puts the
	// baseline back only if the miner is still on the run's last test setting (not over the owner's choice);
	// trims that couldn't come off are taken off then (a marker without Ended would put the old baseline over
	// whatever the owner chose meanwhile)
	markTunerEndedTrims(trimsFailed > 0)
	swapCharge("tuner", "") // (not over a safety stop's "idle")
	msg += ": the panel ended it" + put + ". Controls, Restart mining and updates work again; please save the tuner log and report it."
	log.Print("tuner: " + msg)
	t.deadWhy.Store(&msg)
	activity.emit("🔧 " + msg)
	notify(evTuning, msg)
}

// tunerHoldsFans: a tuning run is going that holds the fans itself (fan hold, not a dry run).
func tunerHoldsFans() bool {
	if getCharge() != "tuner" {
		return false
	}
	tunerMu.Lock()
	t := tunerCur
	tunerMu.Unlock()
	return t != nil && t.cfg.FanHold && !t.cfg.DryRun
}
