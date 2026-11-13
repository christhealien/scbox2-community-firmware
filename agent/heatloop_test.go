package main

// r3-heat: fanTick (fanLoop's body, split out) against a fake miner: cgminer API (temps) + stock web (/mcb/setting).

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type hlPut struct {
	kind string // "fan" (manual plan), "idle", "wake", "plan"
	plan string
	at   time.Time
}

type hlMiner struct {
	mu       sync.Mutex
	temps    [][3]float64 // per board
	setting  map[string]any
	puts     []hlPut
	failFans bool          // fan writes (a manual plan PUT) answer 500
	idleSlow time.Duration // the idle PUT takes this long (Goldshell's backend is slow)
	apiIdle  bool          // the cgminer API still answers while on the idle plan (stale or while stopping)
	clk      time.Time
	ln       net.Listener
	srv      *httptest.Server
}

func (m *hlMiner) idle() bool {
	man, _ := m.setting["manual"].(bool)
	return !man && m.setting["select"] == float64(1)
}

func (m *hlMiner) setTemps(ts ...[3]float64) { m.mu.Lock(); m.temps = ts; m.mu.Unlock() }
func (m *hlMiner) putsCopy() []hlPut {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]hlPut(nil), m.puts...)
}
func (m *hlMiner) isIdle() bool { m.mu.Lock(); defer m.mu.Unlock(); return m.idle() }

func newHLMiner(t *testing.T) *hlMiner {
	m := &hlMiner{}
	json.Unmarshal([]byte(`{"manual":true,"tempcontrol":true,"select":0,"manualPowerplan":"625 MHz 9100 V 50 RPM 50 RPM PV 9400",
		"powerplans":[{"info":"615 MHz 9100 V 40 RPM 40 RPM PV 9400","level":0},{"info":"0 MHz 0 V 100 RPM 100 RPM","level":3}]}`), &m.setting)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	m.ln = ln
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				c.SetDeadline(time.Now().Add(2 * time.Second))
				buf := make([]byte, 512)
				n, _ := bufio.NewReader(c).Read(buf)
				req := string(buf[:n])
				m.mu.Lock()
				down := m.idle() && !m.apiIdle
				temps := append([][3]float64(nil), m.temps...)
				m.mu.Unlock()
				if down {
					return // no answer: the boards are off
				}
				switch {
				case strings.Contains(req, "summary"):
					io.WriteString(c, `{"SUMMARY":[{"MHS av":4600000.0,"Elapsed":5000,"Accepted":10}]}`)
				case strings.Contains(req, "devs"):
					var ds []string
					for i, tt := range temps {
						ds = append(ds, fmt.Sprintf(`{"ID":%d,"Enabled":"Y","Status":"Alive","MHS av":1150000.0,"Nonced":1000,"Hardware Errors":1,"Device Hardware%%":0.1,"voltage":9100.0,"clock":625.0,"overheat":0,"tstemp-0":%.1f,"tstemp-1":%.1f,"tstemp-2":%.1f,"fan0":50,"fan1":50,"fan2":50,"fan3":50}`, i, tt[0], tt[1], tt[2]))
					}
					io.WriteString(c, `{"DEVS":[`+strings.Join(ds, ",")+`]}`)
				}
			}(c)
		}
	}()
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/user/login"):
			json.NewEncoder(w).Encode(map[string]any{"JWT Token": "tok"})
		case r.Method == "GET":
			m.mu.Lock()
			json.NewEncoder(w).Encode(m.setting)
			m.mu.Unlock()
		case r.Method == "PUT":
			var in map[string]any
			b, _ := io.ReadAll(r.Body)
			json.Unmarshal(b, &in)
			man, _ := in["manual"].(bool)
			kind := "plan"
			switch {
			case !man && in["select"] == float64(1):
				kind = "idle"
			case !man:
				kind = "wake"
			}
			m.mu.Lock()
			fail := m.failFans && kind == "plan"
			slow := m.idleSlow
			m.mu.Unlock()
			if fail {
				http.Error(w, "backend busy", 500)
				return
			}
			m.mu.Lock()
			pl, _ := in["manualPowerplan"].(string)
			m.puts = append(m.puts, hlPut{kind, pl, time.Now()})
			m.setting = in
			m.mu.Unlock()
			if kind == "idle" && slow > 0 {
				time.Sleep(slow) // applied, but the backend answers only after a moment
			}
			io.WriteString(w, `{"status":0}`)
		}
	}))
	*apiAddr = ln.Addr().String()
	*minerWeb = strings.TrimPrefix(m.srv.URL, "http://")
	*authFile = filepath.Join(t.TempDir(), "auth.json")
	jwtToken = ""
	authMu.Lock()
	auth.MinerPass = "pw"
	authMu.Unlock()
	wakeWait = 0
	m.clk = time.Now()
	fanNow = func() time.Time { m.mu.Lock(); defer m.mu.Unlock(); return m.clk }
	heatIdleSince.Store(0)
	fanMu.Lock()
	fanSt.MaxT, fanSt.HotBoard, fanSt.Target, fanSt.Guard, fanSt.Note, fanSt.LastWrite, fanSt.SavedFreq = 0, -1, 0, "", "", time.Time{}, 0
	fanHist = nil
	fanMu.Unlock()
	heatEsc = heatEscState{}
	hlResetGuards()
	setCharge("")
	heatEpMu.Lock()
	heatEpLast, heatEpCool = 0, 0
	heatEpMu.Unlock()
	heatPending.Store("")
	t.Cleanup(func() {
		ln.Close()
		m.srv.Close()
		fanNow = time.Now
		heatIdleSince.Store(0)
		wakeWait = 20 * time.Second
		setCharge("")
		heatEsc = heatEscState{}
		hlResetGuards()
	})
	return m
}

// hlResetGuards: the fan loop's own memory between tests (abort tier, per-board readings, throttle).
func hlResetGuards() {
	abortTierOn.Store(false)
	boardLast = map[int]*boardSeen{}
	noTempSince, noTempTicks = time.Time{}, 0
	throttledTo, throttleAt = 0, time.Time{}
	fanMu.Lock()
	fanSt.ThrottledTo = 0
	fanMu.Unlock()
}

func (m *hlMiner) adv(d time.Duration) { m.mu.Lock(); m.clk = m.clk.Add(d); m.mu.Unlock() }

// tick: the next poll, d after the last one
func (m *hlMiner) tick(c fanConfig, d time.Duration) { m.adv(d); fanTick(c) }

func hlCfg() fanConfig {
	c := defaultFanConfig()
	c.Enabled = true
	return c
}

func (m *hlMiner) count(kind string) int {
	n := 0
	for _, p := range m.putsCopy() {
		if p.kind == kind {
			n++
		}
	}
	return n
}

func fanGuardNow() (string, int) {
	fanMu.RLock()
	defer fanMu.RUnlock()
	return fanSt.Guard, fanSt.Target
}

var normal = [3]float64{66, 66, 67.9}

// 1. the real glitch shape (fanctrl 08:34:02: board 0's sensor 67.9 → 95.9 → 67.9 for one sample): nothing happens
func TestHeatLoopSingleGlitch(t *testing.T) {
	m := newHLMiner(t)
	c := hlCfg()
	m.setTemps(normal, normal, normal, normal)
	m.tick(c, 10*time.Second)
	m.setTemps([3]float64{66, 66, 95.9}, normal, normal, normal)
	m.tick(c, 10*time.Second)
	if g, _ := fanGuardNow(); g != "" {
		t.Fatalf("one glitch reading engaged %q", g)
	}
	m.setTemps(normal, normal, normal, normal)
	m.tick(c, 10*time.Second)
	if m.count("idle") != 0 || getCharge() == "idle" {
		t.Fatal("a glitch put the miner on Idle")
	}
}

// 2. the same glitch value seen by two polls (poll 5 s, the miner's sample held ~5–6 s): Idle at once via the
// "+5 °C twice" path. Two-in-a-row counts polls, not distinct readings.
func TestHeatLoopGlitchSeenTwice(t *testing.T) {
	m := newHLMiner(t)
	c := hlCfg()
	m.setTemps(normal, normal, normal, normal)
	m.tick(c, 10*time.Second)
	m.setTemps([3]float64{66, 66, 95.9}, normal, normal, normal) // one stuck sample, read twice (poll 5 s)
	m.tick(c, 5*time.Second)
	m.tick(c, 5*time.Second)
	if m.count("idle") != 0 {
		t.Fatalf("one glitched sample, read by two polls, put the miner on Idle (chips 66 °C, other boards normal); puts=%+v", m.putsCopy())
	}
}

// 3. sustained heat, fan write lands: 100 %, then Idle after heatEscalateAfter
func TestHeatLoopSustainedFansLand(t *testing.T) {
	m := newHLMiner(t)
	c := hlCfg()
	hot := [3]float64{89, 89, 89.5}
	m.setTemps(hot, normal, normal, normal)
	m.tick(c, 10*time.Second)
	m.tick(c, 10*time.Second) // abort: 100 % written
	ps := m.putsCopy()
	if len(ps) == 0 || !strings.Contains(ps[len(ps)-1].plan, "100 RPM 100 RPM") {
		t.Fatalf("no 100 %% write: %+v", ps)
	}
	m.tick(c, 10*time.Second) // fullAt set
	m.adv(100 * time.Second)
	m.tick(c, 10*time.Second)
	if m.count("idle") != 1 || getCharge() != "idle" || !m.isIdle() {
		t.Fatalf("no Idle after the escalation time: puts=%+v charge=%q", m.putsCopy(), getCharge())
	}
}

// 4. fan writes fail (backend answers 500) after an earlier curve write landed: "fans at 100 %" must not be believed
func TestHeatLoopFanWriteFailsNotFull(t *testing.T) {
	m := newHLMiner(t)
	c := hlCfg()
	m.setTemps(normal, normal, normal, normal)
	m.tick(c, 10*time.Second) // the curve writes once: LastWrite set
	if m.count("plan") != 1 {
		t.Fatalf("setup: curve didn't write: %+v", m.putsCopy())
	}
	m.mu.Lock()
	m.failFans = true
	m.mu.Unlock()
	hot := [3]float64{89, 89, 89.5}
	m.setTemps(hot, normal, normal, normal)
	m.tick(c, 10*time.Second)
	m.tick(c, 10*time.Second) // abort: the 100 % write fails
	m.tick(c, 10*time.Second)
	if !heatEsc.fullAt.IsZero() {
		_, tg := fanGuardNow()
		t.Fatalf("fullAt set although no 100 %% write ever landed (fanSt.Target=%d, last landed write %+v)", tg, m.putsCopy())
	}
}

// 5. cooling midway: the episode resets
func TestHeatLoopCoolsMidway(t *testing.T) {
	m := newHLMiner(t)
	c := hlCfg()
	hot := [3]float64{89, 89, 89.5}
	m.setTemps(hot, normal, normal, normal)
	m.tick(c, 10*time.Second)
	m.tick(c, 10*time.Second)
	m.setTemps([3]float64{84, 84, 85}, normal, normal, normal)
	m.tick(c, 10*time.Second)
	m.adv(100 * time.Second)
	m.setTemps(hot, normal, normal, normal)
	m.tick(c, 10*time.Second)
	if m.count("idle") != 0 {
		t.Fatal("idle although it cooled between")
	}
	if g, _ := fanGuardNow(); g != "" {
		t.Fatalf("one hot reading after cooling engaged %q", g)
	}
}

// 6. +5 °C twice: Idle at once
func TestHeatLoopFarPast(t *testing.T) {
	m := newHLMiner(t)
	c := hlCfg()
	m.setTemps([3]float64{93.5, 93.5, 94}, normal, normal, normal)
	m.tick(c, 10*time.Second)
	m.tick(c, 10*time.Second)
	if m.count("idle") != 1 || getCharge() != "idle" {
		t.Fatalf("not Idle: %+v", m.putsCopy())
	}
}

// 7. already Idle (by itself / the owner) while the API still answers hot: nothing may wake it. With the
// throttle tier on, its clock step goes through mutatePlan → wakeIfIdle.
func TestHeatLoopIdleNotWokenByThrottle(t *testing.T) {
	m := newHLMiner(t)
	c := hlCfg()
	c.ThrottleEnable, c.ThrottleC, c.ResumeC = true, 85, 80
	m.mu.Lock()
	m.setting["manual"], m.setting["select"] = false, float64(1) // on the idle plan
	m.apiIdle = true                                             // the API still answers (stopping / stale)
	m.mu.Unlock()
	setCharge("idle")
	m.setTemps([3]float64{86, 86, 87}, normal, normal, normal)
	m.tick(c, 10*time.Second)
	if n := m.count("wake"); n != 0 || !m.isIdle() {
		t.Fatalf("the thermal throttle woke an idle miner while hot: %+v", m.putsCopy())
	}
}

// 7b. our own escalation, then the API answers once more with the still-hot reading (boards cool slowly):
// the throttle wakes it, and since charge is "idle" the escalation never fires again.
func TestHeatLoopEscalationThenThrottleWakes(t *testing.T) {
	m := newHLMiner(t)
	c := hlCfg()
	c.ThrottleEnable, c.ThrottleC, c.ResumeC = true, 85, 80
	m.mu.Lock()
	m.apiIdle = true
	m.mu.Unlock()
	m.setTemps([3]float64{93.5, 93.5, 94}, normal, normal, normal)
	m.tick(c, 10*time.Second)
	m.tick(c, 10*time.Second) // far twice: Idle
	if getCharge() != "idle" || !m.isIdle() {
		t.Fatalf("setup: not idle %+v", m.putsCopy())
	}
	m.setTemps([3]float64{90, 90, 91}, normal, normal, normal)
	m.tick(c, 10*time.Second)
	if !m.isIdle() {
		t.Fatalf("woken right after the overheat Idle (charge %q says idle): %+v", getCharge(), m.putsCopy())
	}
}

// 8. a tuning run is going: the overheat Idle must not be undone by the run's end (it restores its plan)
func TestHeatLoopTunerRestoreRace(t *testing.T) {
	m := newHLMiner(t)
	m.mu.Lock()
	m.idleSlow = 300 * time.Millisecond // the stock backend takes a moment for the idle PUT
	m.mu.Unlock()
	c := hlCfg()
	cfg := fastCfg()
	cfg.BaselineMin, cfg.HoldMin = 60, 60
	e, _ := newSimEngine(cfg, 0, ckey{2, 5})
	e.sleep = func(d time.Duration) { time.Sleep(5 * time.Millisecond) }
	e.now = time.Now
	e.readTemp = func() (float64, bool) { return 70, true } // the tuner's own reading stays under its abort
	e.applyPlan = func(p planMV) error {
		_, err := mutatePlan(func(x *powerplan) { x.FreqMHz, x.VrunMv, x.PVMv = p.MHz, p.MV-300, p.MV })
		return err
	}
	e.held = miningHoldErr
	e.chargeNow = getCharge
	e.restoreWait = 0
	tunerMu.Lock()
	tunerCur = e
	tunerMu.Unlock()
	defer func() { tunerMu.Lock(); tunerCur = nil; tunerMu.Unlock() }()
	e.begin()
	setCharge("tuner")
	done := make(chan struct{})
	go func() { e.Run(); close(done) }()
	time.Sleep(200 * time.Millisecond)
	m.setTemps([3]float64{93.5, 93.5, 94}, normal, normal, normal)
	m.tick(c, 10*time.Second)
	m.tick(c, 10*time.Second)
	<-done
	time.Sleep(100 * time.Millisecond)
	ps := m.putsCopy()
	var kinds []string
	for _, p := range ps {
		kinds = append(kinds, p.kind)
	}
	t.Logf("puts %v charge %q", kinds, getCharge())
	if !m.isIdle() {
		t.Fatalf("the tuner's end woke the miner after the overheat Idle: charge %q, puts %v", getCharge(), kinds)
	}
}

// 9. the schedule's next change after an overheat Idle: it wakes the miner (nothing holds a heat Idle)
func TestHeatLoopScheduleWakesOverheatIdle(t *testing.T) {
	m := newHLMiner(t)
	c := hlCfg()
	schedPath = filepath.Join(t.TempDir(), "schedule.json")
	defer func() { schedPath = "" }()
	b, _ := json.Marshal(schedule{Enabled: true, Rules: []schedRule{{Start: "00:00", Days: []int{0, 1, 2, 3, 4, 5, 6}, Preset: "Stock"}}})
	writeAtomic(schedPath, b, 0o644)
	m.setTemps([3]float64{93.5, 93.5, 94}, normal, normal, normal)
	m.tick(c, 10*time.Second)
	m.tick(c, 10*time.Second)
	if getCharge() != "idle" || !m.isIdle() {
		t.Fatalf("setup: not idle %+v", m.putsCopy())
	}
	lastSchKey = -1 // the next painted change comes round
	scheduleTick()
	if !m.isIdle() {
		t.Fatalf("the schedule woke the miner a moment after the overheat Idle (nothing seen cool): charge %q", getCharge())
	}
}

// 10. what the Fan card shows: the guard pill comes from the last full tick, max_temp is re-read live. A tick that
// returns early (API down: e.g. on Idle) never clears the guard: "ABORT" stays, with the last hot reading.
func TestHeatLoopStaleAbortPill(t *testing.T) {
	m := newHLMiner(t)
	c := hlCfg()
	m.setTemps([3]float64{93.5, 93.5, 94}, normal, normal, normal)
	m.tick(c, 10*time.Second)
	m.tick(c, 10*time.Second) // Idle; the API stops answering (apiIdle false)
	for i := 0; i < 3; i++ {
		m.tick(c, 10*time.Second)
	}
	rec := httptest.NewRecorder()
	handleFan(rec, httptest.NewRequest("GET", "/api/fan", nil))
	var out struct {
		Status map[string]any `json:"status"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	t.Logf("status on Idle: guard=%v max_temp=%v target=%v note=%v", out.Status["guard"], out.Status["max_temp"], out.Status["target"], out.Status["note"])
	if out.Status["guard"] == "abort" {
		t.Fatalf("the Fan card still shows ABORT while the miner is on Idle and nothing is read")
	}
}

// 11. the guard pill next to a live reading: a hot tick, then the card is read after the boards read normal
// again but before the next tick (up to PollS = 10 s): ABORT beside ~67 °C.
func TestHeatLoopAbortPillBesideLiveTemp(t *testing.T) {
	m := newHLMiner(t)
	c := hlCfg()
	hot := [3]float64{66, 66, 89}
	m.setTemps(hot, normal, normal, normal)
	m.tick(c, 10*time.Second)
	m.tick(c, 10*time.Second)
	m.setTemps(normal, normal, normal, normal)
	rec := httptest.NewRecorder()
	handleFan(rec, httptest.NewRequest("GET", "/api/fan", nil))
	var out struct {
		Status map[string]any `json:"status"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	t.Logf("guard=%v max_temp=%v note=%v", out.Status["guard"], out.Status["max_temp"], out.Status["note"])
	if out.Status["guard"] == "abort" && out.Status["max_temp"].(float64) < 80 {
		t.Fatalf("ABORT shown beside a live %.1f °C", out.Status["max_temp"])
	}
}

// 12. the overheat stop is logged as the thermal guard's (it said "the pool guard: overheat …")
func TestHeatLoopStopReasonThermal(t *testing.T) {
	m := newHLMiner(t)
	_ = m
	e, _ := newSimEngine(fastCfg(), 0, ckey{2, 5})
	e.begin()
	tunerMu.Lock()
	tunerCur = e
	tunerMu.Unlock()
	defer func() { tunerMu.Lock(); tunerCur = nil; tunerMu.Unlock() }()
	e.mu.Lock()
	e.running = true
	e.mu.Unlock()
	if err := thermalIdle("overheat: still 89.0 °C"); err != nil {
		t.Fatal(err)
	}
	e.mu.Lock()
	why := e.stopWhy
	e.mu.Unlock()
	if !strings.HasPrefix(why, "the thermal guard: overheat") {
		t.Fatalf("overheat stop reason: %q", why)
	}
}

// 13. woken while still hot (the owner applies a preset at once): back on Idle after two readings 8 s+ apart
func TestHeatLoopWokenWhileHotReIdles(t *testing.T) {
	m := newHLMiner(t)
	c := hlCfg()
	hot := [3]float64{89, 89, 89.5}
	m.setTemps(hot, normal, normal, normal)
	for i := 0; i < 4; i++ {
		m.tick(c, 10*time.Second)
	}
	m.adv(100 * time.Second)
	m.tick(c, 10*time.Second)
	if m.count("idle") != 1 {
		t.Fatalf("setup: %+v", m.putsCopy())
	}
	heatIdleSince.Store(time.Now().Add(-heatCoolRepeat).Unix() - 1) // past the cool-down (heatcool.go)
	if err := applyPresetName("Stock"); err != nil {                // the owner, right after it
		t.Fatal(err)
	}
	setCharge("stock")
	if m.isIdle() {
		t.Fatal("setup: Stock didn't wake it")
	}
	m.tick(c, 10*time.Second)
	if m.count("idle") != 1 {
		t.Fatal("one reading after the wake went to Idle (no confirmation)")
	}
	m.tick(c, 10*time.Second)
	if m.count("idle") != 2 || !m.isIdle() {
		t.Fatalf("still hot after a wake: not back on Idle: %+v", m.putsCopy())
	}
}

// 14. curve off (Goldshell's own fan control): the abort tier and the escalation still work
func TestHeatLoopCurveOff(t *testing.T) {
	m := newHLMiner(t)
	c := hlCfg()
	c.Enabled = false
	m.setTemps([3]float64{89, 89, 89.5}, normal, normal, normal)
	m.tick(c, 10*time.Second)
	m.tick(c, 10*time.Second)
	ps := m.putsCopy()
	if len(ps) != 1 || !strings.Contains(ps[0].plan, "100 RPM 100 RPM") {
		t.Fatalf("curve off: no 100 %% write: %+v", ps)
	}
	m.tick(c, 10*time.Second)
	m.tick(c, 100*time.Second)
	if m.count("idle") != 1 {
		t.Fatalf("curve off: no Idle: %+v", m.putsCopy())
	}
}

// 15. a run that holds the fans: below-100 targets stand back, the abort's 100 % goes through, then Idle
func TestHeatLoopTunerHoldsFans(t *testing.T) {
	m := newHLMiner(t)
	c := hlCfg()
	cfg := fastCfg()
	cfg.FanHold = true
	e, _ := newSimEngine(cfg, 0, ckey{2, 5})
	tunerMu.Lock()
	tunerCur = e
	tunerMu.Unlock()
	defer func() { tunerMu.Lock(); tunerCur = nil; tunerMu.Unlock() }()
	setCharge("tuner")
	m.setTemps(normal, normal, normal, normal)
	m.tick(c, 10*time.Second)
	if len(m.putsCopy()) != 0 {
		t.Fatalf("the curve wrote while the tuner holds the fans: %+v", m.putsCopy())
	}
	m.setTemps([3]float64{89, 89, 89.5}, normal, normal, normal)
	m.tick(c, 10*time.Second)
	m.tick(c, 10*time.Second)
	if ps := m.putsCopy(); len(ps) != 1 || !strings.Contains(ps[0].plan, "100 RPM 100 RPM") {
		t.Fatalf("no 100 %% write while the tuner holds the fans: %+v", ps)
	}
	m.tick(c, 10*time.Second)
	m.tick(c, 100*time.Second)
	if m.count("idle") != 1 || getCharge() != "idle" {
		t.Fatalf("no Idle: %+v charge %q", m.putsCopy(), getCharge())
	}
}

// 16. a strong-chip test is running: the overheat Idle ends it, its raised chips go back
func TestHeatLoopStrongTestEnds(t *testing.T) {
	m := newHLMiner(t)
	f := sctSetup(t)
	before, _ := os.ReadFile(chipTrimPath())
	if e := sctStart(2, 0, 0); e != "" {
		t.Fatal(e)
	}
	for i := 0; i < int((30*time.Minute)/sctTick); i++ {
		f.advance(sctTick)
		sctTickOnce()
	}
	if !sctRunning() {
		t.Fatal("setup: the test isn't running")
	}
	c := hlCfg()
	m.setTemps([3]float64{93.5, 93.5, 94}, normal, normal, normal)
	m.tick(c, 10*time.Second)
	m.tick(c, 10*time.Second)
	if sctRunning() || !m.isIdle() {
		t.Fatalf("strong test still running (%v) or not idle (%v)", sctRunning(), m.isIdle())
	}
	after, _ := os.ReadFile(chipTrimPath())
	if !bytes.Equal(before, after) {
		t.Fatalf("trims not put back:\n%s\nwant\n%s", after, before)
	}
	l := sctLast(t)
	if l["how"] != "aborted" || !strings.Contains(fmt.Sprint(l), "thermal guard") {
		t.Fatalf("result: %v", l)
	}
}

// 17. already on Idle (the owner's), API down: nothing written, no notification loop
func TestHeatLoopAlreadyIdle(t *testing.T) {
	m := newHLMiner(t)
	c := hlCfg()
	m.mu.Lock()
	m.setting["manual"], m.setting["select"] = false, float64(1)
	m.mu.Unlock()
	setCharge("idle")
	m.setTemps([3]float64{93.5, 93.5, 94}, normal, normal, normal)
	for i := 0; i < 5; i++ {
		m.tick(c, 10*time.Second)
	}
	if len(m.putsCopy()) != 0 {
		t.Fatalf("wrote to an idle miner: %+v", m.putsCopy())
	}
}
