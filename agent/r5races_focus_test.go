package main

// r5-races: focused repros for the races the stress harness (r5races_harness_test.go) turns up. Each forces one
// interleaving with a backend hook: the safety stop (fan lock, pool guard, thermal guard) runs while a writer's
// request is between its checks and its write landing. "It got through" means the safety stop finished while
// the writer was still mid-write, i.e. nothing serializes them.

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// rcDuring: the first time match is on the caller's stack, run stop in another goroutine and wait (up to 600 ms)
// for it. Returns whether it ran to the end in that time (false = the writer held a lock it needed: serialized).
type rcDuring struct {
	once sync.Once
	done chan struct{}
	ran  bool
}

func rcHook(match string, stop func()) (func(string), *rcDuring) {
	d := &rcDuring{done: make(chan struct{})}
	return func(stack string) {
		if !strings.Contains(stack, match) {
			return
		}
		d.once.Do(func() {
			go func() { stop(); close(d.done) }()
			select {
			case <-d.done:
				d.ran = true
			case <-time.After(600 * time.Millisecond):
			}
		})
	}, d
}

// wait: the stop has finished (it may have waited for the writer's lock); then the miner's state is final.
func (d *rcDuring) wait(t *testing.T) {
	t.Helper()
	fired := false
	d.once.Do(func() { close(d.done) }) // never fired: nothing to wait for
	select {
	case <-d.done:
		fired = true
	case <-time.After(5 * time.Second):
	}
	if !fired {
		t.Fatal("the safety stop never finished (deadlock?)")
	}
}

func rcFanLock() {
	fanLockEngage(watchdogView{Flagged: "the watchdog flags fan0", Status: 1}, time.Now())
}

func rcAssertHeld(t *testing.T, b *rcBackend, what string) {
	t.Helper()
	cur, idle := b.state()
	_, locked := loadFanLock()
	_, held := poolHold()
	if (locked || held) && !idle {
		pp, _ := resolveRunningPlan(cur)
		t.Errorf("%s: mining is held (fan lock %v, pool hold %v, charge %q) but the miner hashes %d MHz", what, locked, held, getCharge(), pp.FreqMHz)
	}
}

func rcAssertChargeMatches(t *testing.T, b *rcBackend, what string) {
	t.Helper()
	cur, idle := b.state()
	ch := getCharge()
	pp, _ := resolveRunningPlan(cur)
	if ch == "idle" && !idle {
		t.Errorf("%s: charge %q (heat idle since %d) but the miner hashes %d MHz", what, ch, heatIdleSince.Load(), pp.FreqMHz)
	}
	if heatIdleSince.Load() != 0 && !idle {
		t.Errorf("%s: the thermal guard's Idle (charge now %q) was woken: the miner hashes %d MHz", what, ch, pp.FreqMHz)
	}
}

// Controls → Clock / voltage → Apply, while the fan lock engages: the lock's Idle lands, then Controls' write
// (read before, written without planMu and without looking at the lock again) starts hashing.
func TestR5ControlWakesFanLock(t *testing.T) {
	for _, tc := range []struct {
		name string
		body map[string]any
	}{
		{"manual plan", map[string]any{"confirm": true, "freq_mhz": 600, "vrun_mv": 9000, "pv_mv": 9300, "fan1": 60, "fan2": 60}},
		{"stock level", map[string]any{"confirm": true, "preset": 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := rcSetup(t, 1, 0, 0)
			h, d := rcHook("sclite-agent.handleControl(", rcFanLock)
			b.onPut = func(st string, _ map[string]any) { h(st) }
			out := rcPost(handleControl, tc.body)
			d.wait(t)
			t.Logf("Controls answered %v; the fan lock ran to the end meanwhile: %v", out, d.ran)
			rcAssertHeld(t, b, "Controls during the fan lock")
		})
	}
}

// The same with the pool guard's hold.
func TestR5ControlWakesPoolHold(t *testing.T) {
	b := rcSetup(t, 1, 0, 0)
	h, d := rcHook("sclite-agent.handleControl(", rcPoolGuard)
	b.onPut = func(st string, _ map[string]any) { h(st) }
	out := rcPost(handleControl, map[string]any{"confirm": true, "freq_mhz": 600, "vrun_mv": 9000, "pv_mv": 9300, "fan1": 60, "fan2": 60})
	d.wait(t)
	t.Logf("Controls answered %v; the pool guard ran to the end meanwhile: %v", out, d.ran)
	rcAssertHeld(t, b, "Controls during the pool guard's hold")
}

// "Stock" (the schedule's Stock brush, Presets → Stock, the probe's fallback): applyStockPlan checks the holds
// first, then reads and writes the setting outside planMu.
func TestR5StockWakesFanLock(t *testing.T) {
	b := rcSetup(t, 1, 0, 0)
	h, d := rcHook("sclite-agent.applyStockPlan(", rcFanLock)
	b.onPut = func(st string, _ map[string]any) { h(st) }
	err := applyPresetName("Stock")
	d.wait(t)
	t.Logf("Stock: %v; the fan lock ran to the end meanwhile: %v; charge now %q", err, d.ran, getCharge())
	rcAssertHeld(t, b, "Stock during the fan lock")
}

// The thermal guard's Idle vs the schedule's Stock: the miner hashes again and the charge says "stock", so the
// schedule (heatIdleHeld) and everything else think the heat episode is over.
func TestR5StockWakesHeatIdle(t *testing.T) {
	b := rcSetup(t, 1, 0, 0)
	h, d := rcHook("sclite-agent.applyStockPlan(", func() { thermalIdle("overheat: test") })
	b.onPut = func(st string, _ map[string]any) { h(st) }
	err := applyPresetName("Stock")
	d.wait(t)
	t.Logf("Stock: %v; thermalIdle ran to the end meanwhile: %v; charge now %q", err, d.ran, getCharge())
	rcAssertChargeMatches(t, b, "Stock during the thermal guard's Idle")
}

// The end of a tuning run (and of the fan test's re-run) puts back what the miner ran before with a raw PUT,
// after checking the holds but outside planMu: the fan lock's Idle lands in between and the PUT wakes it.
func TestR5RestoreWakesFanLock(t *testing.T) {
	b := rcSetup(t, 1, 0, 0)
	restore, what := snapshotMinerSetting()
	t.Logf("snapshot: %s", what)
	h, d := rcHook("sclite-agent.settingRestorer.func1(", rcFanLock)
	b.onPut = func(st string, _ map[string]any) { h(st) }
	err := restore()
	d.wait(t)
	t.Logf("restore: %v; the fan lock ran to the end meanwhile: %v", err, d.ran)
	rcAssertHeld(t, b, "the tuner's restore during the fan lock")
}

// The thermal guard's Idle (overheat, or no temperature while hashing) during a limit probe: stopRunsFor only
// aborts the probe when the charge is "ceiling", but thermalIdle has just set it to "idle" — the probe goes on,
// every step's mutatePlan wakes the miner, and its end puts its baseline back over the Idle.
func TestR5ThermalIdleDuringProbe(t *testing.T) {
	b := rcSetup(t, 1, 2*time.Millisecond, 0)
	if !rcCeilingStart() {
		t.Fatal("probe didn't start")
	}
	time.Sleep(15 * time.Millisecond)
	if err := thermalIdle("no board temperature while hashing"); err != nil {
		t.Fatal(err)
	}
	if !ceilAbort.Load() {
		t.Errorf("thermalIdle didn't abort the limit probe (ceilAbort false)")
	}
	rcWaitProbe(t)
	rcAssertChargeMatches(t, b, "limit probe vs the thermal guard")
	b.mu.Lock()
	t.Logf("wakes: %v; violations: %v", b.wakes, b.example)
	b.mu.Unlock()
}

// Presets → Idle during a limit probe: allowed (it refuses only the tuner), then the probe's next step and its
// end wake the miner while the charge stays "idle".
func TestR5IdleButtonDuringProbe(t *testing.T) {
	b := rcSetup(t, 1, 2*time.Millisecond, 0)
	if !rcCeilingStart() {
		t.Fatal("probe didn't start")
	}
	time.Sleep(15 * time.Millisecond)
	out := rcPost(handlePresets, map[string]any{"action": "idle"})
	t.Logf("Idle button: %v", out)
	rcWaitProbe(t)
	cur, idle := b.state()
	if out["ok"] == true && !idle {
		pp, _ := resolveRunningPlan(cur)
		t.Errorf("Idle was accepted during the probe, but after the probe the miner hashes %d MHz (charge %q)", pp.FreqMHz, getCharge())
	}
}

func rcWaitProbe(t *testing.T) {
	t.Helper()
	for i := 0; i < 6000; i++ {
		ceilMu.Lock()
		run := ceilSt.Running
		ceilMu.Unlock()
		if !run {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("probe never ended")
}

// A tuning run's step write lands just after the thermal guard put the miner on Idle (it runs in the fan loop,
// at any moment): mutatePlan wakes the miner (a heat Idle isn't a "hold"), the run then sees the Stop and the
// "idle" charge and puts nothing back — leaving the miner hashing with the charge saying "idle" (so the schedule,
// the throttle and the Controls lock all believe it's resting).
func TestR5TunerStepWakesHeatIdle(t *testing.T) {
	b := rcSetup(t, 1, 0, 0)
	e, done := rcTunerStart(b)
	if e == nil {
		t.Fatal("no run")
	}
	n := 0
	inner := e.applyPlan
	e.applyPlan = func(p planMV) error {
		n++
		if n == 3 { // the fan loop's thermal guard fires between the run's stop check and this write
			thermalIdle("overheat: test")
		}
		return inner(p)
	}
	e.Run()
	tunerEnded(e)
	done()
	rcAssertChargeMatches(t, b, "a tuner step vs the thermal guard")
}

// The abort tier forces 100 %, then another writer (the tuner's fan hold, Controls, Stock) writes a lower level:
// the next readings don't put 100 % back (target == LastPct, the level the loop itself last wrote) until the
// cooldown has passed, while the boards stay at the abort temperature.
func TestR5AbortTierLostToOtherFanWrite(t *testing.T) {
	b := rcSetup(t, 1, 0, 0)
	temp := 86.0
	fanDash = func() dashboard {
		noteHottest(temp)
		var d dashboard
		d.OK = true
		for i := 0; i < 4; i++ {
			d.Boards = append(d.Boards, board{ID: i, Alive: true, GHS: 900, Temps: []float64{temp}})
		}
		return d
	}
	vt := time.Now()
	fanNow = func() time.Time { vt = vt.Add(10 * time.Second); return vt }
	c := defaultFanConfig()
	c.AbortC, c.ResumeC, c.CooldownS, c.Smoothing = 85, 80, 45, 0
	fanTick(c)
	fanTick(c)
	if l, _ := fanLvl("fan1"); l != 100 {
		t.Fatalf("abort tier didn't force 100 %%: fan1 %d", l)
	}
	if _, err := mutatePlan(func(p *powerplan) { p.Fan1, p.Fan2 = 50, 50 }); err != nil { // e.g. applyTuneFan(50)
		t.Fatal(err)
	}
	fanTick(c)
	fanTick(c)
	cur, _ := b.state()
	pp, _ := resolveRunningPlan(cur)
	fanMu.RLock()
	g := fanSt.Guard
	fanMu.RUnlock()
	if pp.Fan1 < 100 {
		t.Errorf("abort tier on (guard %q, boards %.0f °C ≥ abort 85) but the fans run %d %% after two more readings", g, temp, pp.Fan1)
	}
}
