package main

import (
	"sync"
	"time"
)

// What the miner is doing when its own web backend (chip report, settings, pools) doesn't answer: a
// change of clock/voltage or pools restarts the hash boards for a minute, a boot takes a couple, idle
// has no boards running. The panel shows this instead of a raw "HTTP 500".

var (
	changeMu   sync.Mutex
	changeAt   time.Time
	changeWhat string
)

// noteMinerChange: a setting the backend restarts the boards for was just written.
func noteMinerChange(what string) {
	changeMu.Lock()
	changeAt, changeWhat = time.Now(), what
	changeMu.Unlock()
}

func lastMinerChange() (time.Time, string) {
	changeMu.Lock()
	defer changeMu.Unlock()
	return changeAt, changeWhat
}

// minerBusyNote says why the miner can't report right now, in plain words ("" when there's no known
// reason). what names the missing data, e.g. "chip data".
func minerBusyNote(what string) string {
	if label, _, ok := rebootActive(); ok {
		return label + " · " + what + " comes back by itself."
	}
	if minerIsIdleNow() {
		return "The miner is idle: its hash boards are off, so there's no " + what + ". Apply " + stockWord() + " or a preset to start mining."
	}
	if noPoolSet() {
		return "No pool set yet: the hash boards start once a pool is added in the Pools tab."
	}
	if at, why := lastMinerChange(); !at.IsZero() && time.Since(at) < 3*time.Minute {
		who := "a " + why
		if getCharge() == "tuner" && why == "clock/voltage change" {
			who = "the tuner's new setting"
		}
		return "The hash boards are restarting after " + who + " (about a minute) · " + what + " comes back by itself."
	}
	if systemUptime() < 300 {
		return "The miner is starting up (a minute or two after a boot) · " + what + " comes back by itself."
	}
	if t := engineLastOK(); !t.IsZero() && time.Since(t) < 3*time.Minute {
		return "Mining is restarting (about a minute) · " + what + " comes back by itself."
	}
	return ""
}

// busyOrErr: the busy note, or a calm fallback that keeps the technical detail separate.
func busyOrErr(what string, err error) map[string]any {
	note := minerBusyNote(what)
	if note == "" {
		note = "The miner's backend isn't answering for " + what + " right now. The panel keeps trying; if it lasts more than a few minutes, Settings → Reboot miner usually clears it."
	}
	m := map[string]any{"ok": false, "busy": true, "err": note}
	if err != nil {
		m["detail"] = err.Error()
	}
	return m
}
