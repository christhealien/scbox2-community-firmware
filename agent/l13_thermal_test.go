package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// L13-1: the hot board's sensors fail to read every other poll (the miner reports -150, or 0, for that board).
// Each miss makes the hottest valid reading the other boards' ~68 °C, which wipes the abort episode: a board
// held at 89.5 °C never gets the fans forced to 100 % for long, and never goes to Idle.
func TestL13IntermittentHotSensorStillIdles(t *testing.T) {
	for _, miss := range []float64{-150, 0} {
		m := newHLMiner(t)
		c := hlCfg()
		hot := [3]float64{89, 89, 89.5}
		bad := [3]float64{miss, miss, miss}
		for i := 0; i < 60 && m.count("idle") == 0; i++ { // 10 minutes at a 10 s poll
			if i%2 == 0 {
				m.setTemps(hot, normal, normal, normal)
			} else {
				m.setTemps(bad, normal, normal, normal)
			}
			m.tick(c, 10*time.Second)
		}
		if m.count("idle") == 0 {
			t.Fatalf("board 0 at 89.5 °C whose sensors read %v every other poll: 10 minutes, never Idle", miss)
		}
	}
}

// L13-1b: the hot board drops out of the miner's device list every other poll (board 3 here).
func TestL13HotBoardDropsOutStillIdles(t *testing.T) {
	m := newHLMiner(t)
	c := hlCfg()
	hot := [3]float64{89, 89, 89.5}
	for i := 0; i < 60 && m.count("idle") == 0; i++ {
		if i%2 == 0 {
			m.setTemps(normal, normal, normal, hot)
		} else {
			m.setTemps(normal, normal, normal) // board 3 missing from devs
		}
		m.tick(c, 10*time.Second)
	}
	if m.count("idle") == 0 {
		t.Fatal("board 3 at 89.5 °C dropping out of the device list every other poll: 10 minutes, never Idle")
	}
}

// L13-2: after an overheat Idle the miner is woken a little under the abort temperature (inside the 2 °C
// hysteresis band) and runs there for an hour on its fan curve, the abort tier off. Then two readings at the
// abort temperature: Idle at once, skipping the fans-100 % step, with "after 1m30s with the fans at 100 %"
// although the fans were never forced in this run.
func TestL13WokenUnderAbortNoInstantIdleLater(t *testing.T) {
	t0 := time.Date(2026, 11, 6, 2, 0, 0, 0, time.UTC)
	var h heatEscState
	now := t0
	for i := 0; i < 15; i++ { // 89 °C, fans 100 %: Idle within 150 s
		if _, idle, _ := h.step(89, 88, now, i >= 2); idle {
			h.idled()
			break
		}
		now = now.Add(10 * time.Second)
	}
	if h.abortAt.IsZero() {
		t.Fatal("setup: never escalated")
	}
	now = now.Add(5 * time.Minute) // the cool-down, on Idle (no readings)
	// woken: an hour at 86.5 °C (abort tier off: the curve drives the fans)
	for i := 0; i < 360; i++ {
		if a, idle, _ := h.step(86.5, 88, now, false); a || idle {
			t.Fatalf("86.5 °C (under the 88 °C abort) acted: abort %v idle %v", a, idle)
		}
		now = now.Add(10 * time.Second)
	}
	h.step(88.1, 88, now, false)
	now = now.Add(10 * time.Second)
	a, idle, why := h.step(88.2, 88, now, false)
	if idle {
		t.Fatalf("an hour under the abort temperature, then two readings at it: straight to Idle with %q (the fans were never forced to 100 %% this time)", why)
	}
	if !a {
		t.Fatal("two readings at the abort temperature: the abort tier didn't engage")
	}
}

// L13-3: the throttle stepped the clock down (625 → 600 MHz) in unscheduled time; a painted stretch starts and
// remembers "what it ran before" as the throttled 600 MHz; it cools during the stretch (the throttle sees the
// stretch's own clock, so it lets go); at the stretch's end the schedule goes "back" to 600 MHz, and nothing
// ever restores 625: the throttle's step stays for good.
func TestL13ScheduleBackKeepsThrottledClock(t *testing.T) {
	now := time.Now()
	if now.Hour() == 0 && now.Minute() < 5 {
		t.Skip("too near midnight for the HH:MM rules")
	}
	m := newHLMiner(t)
	c := hlCfg()
	c.ThrottleEnable, c.ThrottleC, c.ResumeC = true, 85, 80
	throttleAt, throttledTo = time.Time{}, 0
	schedPath = filepath.Join(t.TempDir(), "schedule.json")
	t.Cleanup(func() {
		schedPath = ""
		schedBefore, schedBeforeBy, lastSchName, lastSchKey = nil, "", "", -1
		throttleAt, throttledTo = time.Time{}, 0
	})
	put := func(preset string) {
		b, _ := json.Marshal(schedule{Enabled: true, Rules: []schedRule{{Start: now.Add(-time.Minute).Format("15:04"), Days: []int{0, 1, 2, 3, 4, 5, 6}, Preset: preset}}})
		writeAtomic(schedPath, b, 0o600)
		lastSchKey = -1
	}
	lastSchName, schedBefore, schedBeforeBy = "", nil, ""

	m.setTemps([3]float64{85, 85, 86}, normal, normal, normal)
	m.tick(c, 10*time.Second)
	m.mu.Lock()
	pl := m.setting["manualPowerplan"].(string)
	m.mu.Unlock()
	if !strings.HasPrefix(pl, "600 MHz") {
		t.Fatalf("setup: the throttle didn't step 625 → 600: %q", pl)
	}
	put("Stock") // the stretch starts
	scheduleTick()
	if lastSchName != "Stock" {
		t.Fatalf("setup: the stretch didn't start: %+v", m.putsCopy())
	}
	m.setTemps(normal, normal, normal, normal) // cooled during the stretch
	m.tick(c, 10*time.Second)
	put(schedBack) // the stretch ends
	scheduleTick()
	m.mu.Lock()
	pl = m.setting["manualPowerplan"].(string)
	m.mu.Unlock()
	if strings.HasPrefix(pl, "600 MHz") {
		t.Fatalf("back from the stretch to the throttle's 600 MHz (it ran 625 MHz before the throttle), and nothing restores it: %q", pl)
	}
}

// L13-4: the throttle stepped the clock down (625 → 600); the owner starts a limit probe (it snapshots the running
// plan, 600 MHz, to put back after each step and at its end); the boards cool during the probe and the throttle,
// seeing the clock owned, forgets its saved 625; the probe ends and puts back 600: nothing restores 625.
func TestL13ProbeEndKeepsThrottledClock(t *testing.T) {
	m := newHLMiner(t)
	c := hlCfg()
	c.ThrottleEnable, c.ThrottleC, c.ResumeC = true, 85, 80
	throttleAt, throttledTo = time.Time{}, 0
	t.Cleanup(func() { throttleAt, throttledTo = time.Time{}, 0 })
	m.setTemps([3]float64{85, 85, 86}, normal, normal, normal)
	m.tick(c, 10*time.Second)
	base, ok := readStockBaseline("current") // the probe's snapshot
	if !ok || base.FreqMHz != 600 {
		t.Fatalf("setup: %+v", base)
	}
	setCharge("ceiling")
	mutatePlan(func(p *powerplan) { p.FreqMHz = 650 }) // a probe step
	m.setTemps(normal, normal, normal, normal)
	m.tick(c, 10*time.Second)                                   // cooled while the probe owns the clock
	mutatePlan(func(p *powerplan) { p.FreqMHz = base.FreqMHz }) // the probe ends: its snapshot back
	setCharge("")
	m.tick(c, 10*time.Second)
	m.tick(c, 10*time.Second)
	m.mu.Lock()
	pl := m.setting["manualPowerplan"].(string)
	m.mu.Unlock()
	if !strings.HasPrefix(pl, "625 MHz") {
		t.Fatalf("after the probe, cool: the clock stays at the throttle's step instead of the 625 MHz it ran: %q", pl)
	}
}

// L13-5: throttle temperature under the abort temperature (85/88): the throttle engages first; when the abort
// tier then forces the fans to 100 %, nothing says so (no activity line, no notification, the note keeps
// talking about the clock step).
func TestL13AbortAfterThrottleIsSaid(t *testing.T) {
	m := newHLMiner(t)
	c := hlCfg()
	c.ThrottleEnable, c.ThrottleC, c.ResumeC = true, 85, 80
	throttleAt, throttledTo = time.Time{}, 0
	t.Cleanup(func() { throttleAt, throttledTo = time.Time{}, 0 })
	m.setTemps([3]float64{85, 85, 86}, normal, normal, normal)
	m.tick(c, 10*time.Second)
	m.setTemps([3]float64{88, 88, 89}, normal, normal, normal)
	m.tick(c, 10*time.Second)
	m.tick(c, 10*time.Second)
	fanMu.RLock()
	note, tgt := fanSt.Note, fanSt.Target
	fanMu.RUnlock()
	if tgt != 100 {
		t.Fatalf("setup: abort tier not on (target %d)", tgt)
	}
	if !strings.Contains(note, "fans forced 100") {
		t.Fatalf("fans forced to 100 %% by the abort tier, but the note says only %q", note)
	}
}
