package main

// After the thermal guard's Idle: Stock, a preset or Controls can't wake the miner during a cool-down.
// On Idle the miner reports no board temperatures (its API is down with the boards), so the cool-down is timed:
// the boards stop making heat the moment hashing stops and cool by themselves (the panel can't raise the fans on
// Idle without waking it). heatCoolFirst after a heat Idle; heatCoolRepeat if it went to Idle for heat again
// within an hour of the last one (the cause is likely still there). Once it runs again, the guard watches as usual.
// The owner can ask for their choice to start by itself when the cool-down ends ("Start this when cool").

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

var (
	heatCoolFirst  = 5 * time.Minute
	heatCoolRepeat = 15 * time.Minute

	heatPending atomic.Value // string: the preset ("Stock" or a name) to start once the cool-down ends; "" = none

	heatEpMu   sync.Mutex
	heatEpLast int64         // the previous heat Idle (unix s)
	heatEpCool time.Duration // this episode's cool-down
	// heatIdleSince's value and when that was on this process's monotonic clock: the cool-down is timed on
	// it, so a wall clock set (1970 -> today after a boot) or stepped by a time server neither ends it early nor
	// stretches it
	heatPinSince int64
	heatPinAt    time.Time
)

// heatIdleNoted: from thermalIdle, at a new heat Idle (now: its unix time).
func heatIdleNoted(now int64) {
	heatEpMu.Lock()
	defer heatEpMu.Unlock()
	heatEpCool = heatCoolFirst
	// (the last one on the unset 1970 clock and this one on a set clock: how long ago is unknown, so the
	// longer cool-down, as for a repeat)
	if heatEpLast != 0 && (now-heatEpLast < 3600 || (unsetStamp(heatEpLast) && now >= clockSetUnix)) {
		heatEpCool = heatCoolRepeat
	}
	heatEpLast = now
	heatPinSince, heatPinAt = now, time.Now()
}

// heatPinLocked: when the heat Idle at since was, on the monotonic clock. A since not noted by this process
// (restored after a restart) is pinned now, from the wall clock: one from the unset 1970 clock while the clock
// is set now, or one in the future (the clock went back), counts from now. Caller holds heatEpMu.
func heatPinLocked(since int64) time.Time {
	if since != heatPinSince || heatPinAt.IsZero() {
		now := time.Now()
		el := now.Unix() - since
		if el < 0 || (unsetStamp(since) && now.Unix() >= clockSetUnix) {
			el = 0
		}
		heatPinSince, heatPinAt = since, now.Add(-time.Duration(el)*time.Second)
	}
	return heatPinAt
}

// heatCoolLeft: how long until the miner may start again (0 = not held, or cooled down).
func heatCoolLeft() time.Duration {
	if !heatIdleHeld() {
		return 0
	}
	heatEpMu.Lock()
	cool := heatEpCool
	at := heatPinLocked(heatIdleSince.Load())
	heatEpMu.Unlock()
	if cool == 0 {
		cool = heatCoolFirst // (held over a restart: counted from the saved time)
	}
	left := cool - time.Since(at)
	if left < 0 {
		return 0
	}
	return left
}

func mmss(d time.Duration) string {
	s := int((d + time.Second - 1) / time.Second)
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}

type heatCoolingErr struct{ left time.Duration }

func (e *heatCoolingErr) Error() string {
	return fmt.Sprintf("the thermal guard put the miner on Idle to cool down: it can start again in %s (on Idle the boards stop making heat and cool by themselves; starting early would bring the heat straight back)", mmss(e.left))
}

// heatCoolErr: non-nil while the miner is on the thermal guard's Idle and its cool-down hasn't ended.
func heatCoolErr() error {
	if left := heatCoolLeft(); left > 0 {
		return &heatCoolingErr{left}
	}
	return nil
}

func heatPendingName() string { s, _ := heatPending.Load().(string); return s }

// heatPendingCheck: from every fan-loop tick (also on Idle). Starts the owner's waiting choice once cooled down.
func heatPendingCheck() {
	name := heatPendingName()
	if name == "" {
		return
	}
	if !heatIdleHeld() { // something else started it, or the hold ended: nothing left to wait for
		heatPending.Store("")
		return
	}
	if heatCoolLeft() > 0 {
		return
	}
	go heatPendingStart(name)
}

func heatPendingStart(name string) {
	controlMu.Lock()
	defer controlMu.Unlock()
	if heatPendingName() != name || !heatIdleHeld() || heatCoolErr() != nil {
		return
	}
	heatPending.Store("")
	gen := safetyGen.Load()
	if err := applyPresetNameGen(name, gen); err != nil {
		msg := "the cool-down ended, but starting " + presetLabel(name) + " failed: " + err.Error() + ". Apply " + stockWord() + " or a preset."
		activity.emit("🔥 thermal guard: " + msg)
		notifyAlways(evThermal, msg)
		return
	}
	msg := "the cool-down ended: " + presetLabel(name) + " is on the miner again, as you asked. The thermal guard is watching as usual."
	activity.emit("🔥 thermal guard: " + msg)
	notifyAlways(evThermal, msg)
}

// heatCoolLine: the dashboard's next step while the thermal guard holds the miner on Idle.
func heatCoolLine() string {
	left := heatCoolLeft()
	switch {
	case left > 0 && heatPendingName() != "":
		return fmt.Sprintf("Cooling down: %s starts by itself in %s.", presetLabel(heatPendingName()), mmss(left))
	case left > 0:
		return fmt.Sprintf("Cooling down: %s or a preset can start it in %s (or ask it to start by itself then). Meanwhile check the fans, the airflow and the room.", stockWord(), mmss(left))
	}
	return "Check the fans, the airflow and the room, then apply " + stockWord() + " or a preset."
}
