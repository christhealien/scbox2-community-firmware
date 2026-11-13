package main

// r4-autofan: the Auto fan panel's loop (fanTick = one pass of fanLoop), its throttle tier and its writes,
// against a fake OEM backend (/mcb/setting) and a fake dashboard.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type afMiner struct {
	mu       sync.Mutex
	setting  map[string]any
	puts     []map[string]any
	getDelay time.Duration // a slow backend: the reply is the setting as it was when the request came in
	putFail  bool
	srv      *httptest.Server
}

const afPlans = `[{"info":"615 MHz 9100 V 40 RPM 40 RPM PV 9400","level":0},{"info":"0 MHz 0 V 100 RPM 100 RPM","level":3}]`

func (m *afMiner) running(plan string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	json.Unmarshal([]byte(`{"manual":true,"tempcontrol":true,"select":0,"manualPowerplan":"`+plan+`","powerplans":`+afPlans+`}`), &m.setting)
}

func (m *afMiner) idle() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.setting["manual"], m.setting["select"] = false, float64(1)
}

func (m *afMiner) plan() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.setting["manual"] != true {
		return "stock select " + func() string { b, _ := json.Marshal(m.setting["select"]); return string(b) }()
	}
	return m.setting["manualPowerplan"].(string)
}

func (m *afMiner) nPuts() int { m.mu.Lock(); defer m.mu.Unlock(); return len(m.puts) }

func newAfMiner(t *testing.T) *afMiner {
	m := &afMiner{}
	m.running("650 MHz 9100 V 60 RPM 60 RPM PV 9400")
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/user/login"):
			json.NewEncoder(w).Encode(map[string]any{"JWT Token": "tok"})
		case r.Method == "GET":
			m.mu.Lock()
			b, _ := json.Marshal(m.setting)
			d := m.getDelay
			m.mu.Unlock()
			time.Sleep(d)
			w.Write(b)
		case r.Method == "PUT":
			b, _ := io.ReadAll(r.Body)
			m.mu.Lock()
			fail := m.putFail
			if !fail {
				var x map[string]any
				json.Unmarshal(b, &x)
				m.puts = append(m.puts, x)
				m.setting = x
			}
			m.mu.Unlock()
			if fail {
				http.Error(w, "backend died", 500)
				return
			}
			io.WriteString(w, `{"status":0}`)
		}
	}))
	t.Cleanup(m.srv.Close)
	dir := t.TempDir()
	*authFile = filepath.Join(dir, "auth.json")
	*fanPath = filepath.Join(dir, "fan.json")
	for i := 0; i < 4; i++ {
		os.MkdirAll(filepath.Join(dir, "fans", "fan"+string(rune('0'+i))), 0755)
		os.WriteFile(filepath.Join(dir, "fans", "fan"+string(rune('0'+i)), "lvl"), []byte("60"), 0644)
	}
	oldSys := fanSysDir
	fanSysDir = filepath.Join(dir, "fans")
	*minerWeb = strings.TrimPrefix(m.srv.URL, "http://")
	jwtToken = ""
	authMu.Lock()
	auth.MinerPass = "pw"
	authMu.Unlock()
	oldWake := wakeWait
	wakeWait = 0
	oldDash := fanDash
	setCharge("")
	heatEsc = heatEscState{}
	r4ResetLoop()
	fanMu.Lock()
	fanCfg = defaultFanConfig()
	fanHist = nil
	reflect.ValueOf(&fanSt).Elem().Set(reflect.Zero(reflect.TypeOf(fanSt))) // (any version's fields)
	fanMu.Unlock()
	t.Cleanup(func() {
		fanSysDir, wakeWait, fanDash = oldSys, oldWake, oldDash
		setCharge("")
		heatEsc = heatEscState{}
	})
	return m
}

// afTemp: the dashboard reads every board at temp; hashing says whether the boards run.
func afTemp(temp float64, hashing bool) {
	fanDash = func() dashboard {
		var d dashboard
		d.OK = true
		for i := 0; i < 4; i++ {
			g := 0.0
			if hashing {
				g = 900
			}
			d.Boards = append(d.Boards, board{ID: i, Alive: hashing, GHS: g, Temps: []float64{temp - 2, temp - 1, temp}})
		}
		return d
	}
}

func afCfg() fanConfig {
	c := defaultFanConfig()
	c.Smoothing = 0
	c.ThrottleEnable, c.ThrottleC, c.ThrottleStep, c.ResumeC = true, 85, 25, 80
	return c
}

// A board still hot just after the miner went to Idle (the thermal guard's own Idle, or the owner's): the
// throttle tier must not write a plan. mutatePlan wakes an idle miner first (wakeIfIdle), so a clock "step
// down" restarts hashing on boards that were put on Idle to cool.
func TestR4AFThrottleMustNotWakeIdle(t *testing.T) {
	m := newAfMiner(t)
	m.idle()
	setCharge("idle")
	afTemp(91, false) // boards off, heatsink still hot
	fanTick(afCfg())
	if n := m.nPuts(); n != 0 {
		t.Fatalf("on Idle the throttle tier wrote %d plan(s); the miner is now: %s (woken)", n, m.plan())
	}
}

// While hot the throttle steps the clock down once per poll (10 s), without waiting to see the effect,
// and logs + notifies every time (also when already at the floor and nothing changes).
func TestR4AFThrottleStepsEveryPoll(t *testing.T) {
	m := newAfMiner(t)
	afTemp(86, true)
	c := afCfg()
	for i := 0; i < 6; i++ { // one minute at the default 10 s poll
		fanTick(c)
	}
	if p := m.plan(); !strings.HasPrefix(p, "625 MHz") {
		t.Fatalf("one minute over the throttle temperature: the clock went 650 → %s (expected one 25 MHz step, then wait)", p)
	}
}

// The clock saved before a throttle comes back after a preset (or Controls, or the schedule) has set a
// different clock meanwhile: the preset's clock is overwritten by a stale one.
func TestR4AFThrottleRestoreOverwritesNewerClock(t *testing.T) {
	m := newAfMiner(t)
	c := afCfg()
	afTemp(86, true)
	fanTick(c) // 650 → 625, 650 saved
	if !strings.HasPrefix(m.plan(), "625 MHz") {
		t.Fatalf("no throttle: %s", m.plan())
	}
	// the owner applies a lower preset
	m.running("550 MHz 9000 V 60 RPM 60 RPM PV 9300")
	setCharge("preset:Low")
	afTemp(75, true)
	fanTick(c)
	if p := m.plan(); !strings.HasPrefix(p, "550 MHz") {
		t.Fatalf("after the preset Low (550 MHz) the throttle 'restored' the old clock: %s", p)
	}
}

// A tuning run owns the clock: the throttle tier changing it mid-step makes the tuner judge a clock it
// didn't set (and the restore puts back a stale step's clock later).
func TestR4AFThrottleDuringTuningRun(t *testing.T) {
	m := newAfMiner(t)
	setCharge("tuner")
	afTemp(86, true) // under the tuner's own abort (88)
	fanTick(afCfg())
	if p := m.plan(); !strings.HasPrefix(p, "650 MHz") {
		t.Fatalf("during a tuning run the throttle changed the clock: %s", p)
	}
}

// A fan write that fails is only retried after the cooldown: the new target was recorded as if written.
func TestR4AFFailedFanWriteRetried(t *testing.T) {
	m := newAfMiner(t)
	c := afCfg()
	c.ThrottleEnable = false
	afTemp(60, true)
	fanTick(c) // ~50 % written
	first := m.plan()
	m.mu.Lock()
	m.putFail = true
	m.mu.Unlock()
	afTemp(75, true)
	fanTick(c) // ~86 %: the write fails
	m.mu.Lock()
	m.putFail = false
	m.mu.Unlock()
	fanTick(c) // same target, 0 s after the last good write
	if p := m.plan(); p == first {
		t.Fatalf("the failed write wasn't retried on the next reading (cooldown %d s): fans still at %s", c.CooldownS, p)
	}
}

// Same at the abort tier: 100 % fails to land, fanSt says Target 100 with a LastWrite → heatEsc is told
// the fans are at 100 % (fansFull) when they are not.
func TestR4AFAbortFullWhenWriteFailed(t *testing.T) {
	m := newAfMiner(t)
	c := afCfg()
	c.ThrottleEnable = false
	afTemp(60, true)
	fanTick(c)
	m.mu.Lock()
	m.putFail = true
	m.mu.Unlock()
	afTemp(89, true)
	now := time.Now()
	oldNow := fanNow
	defer func() { fanNow = oldNow }()
	for i := 0; i < 4; i++ { // readings 10 s apart: abort engages on the 2nd (target 100, the write fails)
		fanNow = func() time.Time { return now.Add(time.Duration(i) * 10 * time.Second) }
		fanTick(c)
	}
	fanMu.RLock()
	g := fanSt.Guard
	fanMu.RUnlock()
	if g != "abort" {
		t.Fatalf("abort tier didn't engage (guard %q)", g)
	}
	if !heatEsc.fullAt.IsZero() {
		t.Fatalf("heatEsc believes the fans went to 100 %% at %v, but no 100 %% write landed (plan %s)", heatEsc.fullAt, m.plan())
	}
}

// The panel shows "fan N %" from status.target; on Idle nothing is written but target keeps the curve value.
func TestR4AFStatusOnIdle(t *testing.T) {
	m := newAfMiner(t)
	m.idle()
	setCharge("idle")
	afTemp(50, false)
	c := afCfg()
	c.ThrottleEnable = false
	fanTick(c)
	fanMu.RLock()
	tg, note := fanSt.Target, fanSt.Note
	fanMu.RUnlock()
	if m.nPuts() != 0 {
		t.Fatalf("wrote on idle")
	}
	if tg >= 0 {
		t.Fatalf("on Idle (nothing written) status.target=%d → the panel shows \"fan %d %%\" next to note %q", tg, tg, note)
	}
}

// Two writers of /mcb/setting (read-modify-write each): the fan write reverts a clock change made at the
// same time (a preset, the tuner's step, the schedule), or the clock change reverts the fans.
func TestR4AFConcurrentPlanWritesLoseOne(t *testing.T) {
	m := newAfMiner(t)
	m.getDelay = 150 * time.Millisecond
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); writeFansUnlessIdle(80) }()
	go func() {
		defer wg.Done()
		time.Sleep(30 * time.Millisecond)
		mutatePlan(func(p *powerplan) { p.FreqMHz, p.VrunMv, p.PVMv = 600, 9100, 9400 })
	}()
	wg.Wait()
	if p := m.plan(); !strings.HasPrefix(p, "600 MHz") || !strings.Contains(p, "80 RPM 80 RPM") {
		t.Fatalf("both writes should land (600 MHz, fans 80): got %q", p)
	}
}

// Idle applied while a fan write is between its idle check and its write: the fan write wakes the miner.
func TestR4AFFanWriteRacesIdle(t *testing.T) {
	m := newAfMiner(t)
	m.getDelay = 150 * time.Millisecond
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); applyIdle() }()
	go func() { defer wg.Done(); time.Sleep(30 * time.Millisecond); writeFansUnlessIdle(80) }()
	wg.Wait()
	if p := m.plan(); p != "stock select 1" {
		t.Fatalf("Idle then a fan write: the miner should stay on its idle plan, is on %q", p)
	}
}

// A fan.json without some fields (hand-edited, or written by an older/other tool) loads zeros: abort_c 0
// means every reading is "at the abort temperature" → fans 100 % and Idle two readings later.
func TestR4AFLoadPartialFanConfig(t *testing.T) {
	dir := t.TempDir()
	old := *fanPath
	*fanPath = filepath.Join(dir, "fan.json")
	defer func() { *fanPath = old; fanMu.Lock(); fanCfg = defaultFanConfig(); fanMu.Unlock() }()
	os.WriteFile(*fanPath, []byte(`{"enabled":true,"profile":"curve-quiet"}`), 0644)
	loadFanConfig()
	fanMu.RLock()
	c := fanCfg
	fanMu.RUnlock()
	if c.AbortC < 70 || c.PollS < 5 || c.ResumeC < 40 {
		var h heatEscState
		h.step(60, c.AbortC, time.Now(), false)
		_, idle, _ := h.step(60, c.AbortC, time.Now(), false)
		t.Fatalf("partial fan.json loaded abort_c=%.0f poll_s=%d cooldown_s=%d resume_c=%.0f (validate: %v); at 60 °C the thermal guard says idle=%v",
			c.AbortC, c.PollS, c.CooldownS, c.ResumeC, validateFanConfig(c), idle)
	}
}

// A clock set by someone else between two throttle steps (a preset applied while still hot): the next step
// starts from it, so when it cools that clock is what comes back, not the one saved before the first step.
func TestR4AFPresetBetweenThrottleSteps(t *testing.T) {
	m := newAfMiner(t)
	c := afCfg()
	afTemp(86, true)
	fanTick(c) // 650 → 625, 650 saved
	m.running("550 MHz 9000 V 60 RPM 60 RPM PV 9300")
	setCharge("preset:Low")
	r4AllowStep()
	fanTick(c) // still hot: 550 → 525
	if !strings.HasPrefix(m.plan(), "525 MHz") {
		t.Fatalf("second step: %s", m.plan())
	}
	afTemp(75, true)
	fanTick(c)
	if p := m.plan(); !strings.HasPrefix(p, "550 MHz") {
		t.Fatalf("restore after a preset between steps: want the preset's 550 MHz back, got %s", p)
	}
}
