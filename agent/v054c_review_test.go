package main

// v0.5.4c re-review fixes: one shared Live reading on the SC Box II while a clock write is confirmed (D1), the
// tuner's plan saved for the next boot only once the OEM's plan files in RAM hold it (D2), the stock put-back keeping
// the raised fans while the clock ramps (D3), no clock raise after a failed fan write (D4), an owner's save counting
// only its own plan writes (D5), the boot script's strict IPv4 check (D6) and its lock (D7).

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// v054cMiner: a fake miner API (:4028) answering summary and devs from the c34 fake's clock / level, counting every
// command with its time.
type v054cMiner struct {
	mu    sync.Mutex
	f     *c34Fake
	temp  float64
	delay time.Duration // each answer this late
	cmds  []v054cCmd
	ln    net.Listener
}

type v054cCmd struct {
	cmd string
	at  time.Time
}

func newV054cMiner(t *testing.T, f *c34Fake) *v054cMiner {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	m := &v054cMiner{f: f, temp: 65, ln: ln}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go m.serve(c)
		}
	}()
	old, oldDevs := *apiAddr, box2DevsCmd
	*apiAddr = ln.Addr().String()
	box2DevsCmd = func() ([]byte, error) { return cgminerCmd("devs") } // (the real poll: counted)
	dashShareReset()
	t.Cleanup(func() { *apiAddr = old; box2DevsCmd = oldDevs; ln.Close(); <-done; dashShareReset() })
	return m
}

func (m *v054cMiner) serve(c net.Conn) {
	defer c.Close()
	buf := make([]byte, 512)
	n, _ := c.Read(buf)
	var q map[string]string
	json.Unmarshal(buf[:n], &q)
	m.mu.Lock()
	m.cmds = append(m.cmds, v054cCmd{q["command"], time.Now()})
	temp, delay := m.temp, m.delay
	m.mu.Unlock()
	if delay > 0 {
		time.Sleep(delay)
	}
	var r string
	switch q["command"] {
	case "summary":
		r = `{"STATUS":[{"STATUS":"S"}],"SUMMARY":[{"Elapsed":3600,"MHS av":4000000,"Accepted":10,"Rejected":0,"Hardware Errors":0,"Stale":0}],"id":1}`
	case "devs":
		m.f.mu.Lock()
		clk, v := m.f.clock, m.f.volt
		m.f.mu.Unlock()
		r = fmt.Sprintf(`{"STATUS":[{"STATUS":"S"}],"DEVS":[{"PGA":0,"ID":0,"Enabled":"Y","Status":"Alive","MHS av":4000000,"voltage":%.3f,"clock":%.3f,"tstemp-0":%.1f,"tstemp-1":%.1f,"tstemp-2":%.1f}],"id":1}`, v, clk, temp, temp-2, temp-10)
	default:
		r = `{"STATUS":[{"STATUS":"S","Msg":"ok"}],"id":1}`
	}
	c.Write([]byte(r + "\x00"))
}

// count: the commands asked in [from, to).
func (m *v054cMiner) count(from, to time.Time) (n int, by map[string]int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	by = map[string]int{}
	for _, c := range m.cmds {
		if !c.at.Before(from) && c.at.Before(to) {
			n++
			by[c.cmd]++
		}
	}
	return n, by
}

// v054cLastAll / v054cLastPhase: (v0.5.5) the last v054cConfirmRun's commands over the whole window (quiet time and
// confirm phase together), and its confirm phase's length in real (unscaled) time.
var (
	v054cLastAll   map[string]int
	v054cLastPhase time.Duration
)

// v054cWindowBudget: the commands the whole window (quiet + confirm phase) may send: in the 30 s quiet time one
// shared refresh every 15 s (2 devs, a summary), in the confirm phase the confirmation's own devs every 5 s plus at
// most one shared refresh every 15 s, one poll either side of an edge, and at most 2 summaries in all. Nothing else.
func v054cWindowBudget(t *testing.T, all map[string]int, phase time.Duration) {
	t.Helper()
	ceil := func(d, every time.Duration) int { return int((d + every - 1) / every) }
	devs := 2 + ceil(phase, 5*time.Second) + ceil(phase, 15*time.Second) + 1
	for c, n := range all {
		switch c {
		case "devs":
			if n > devs {
				t.Fatalf("the window sent devs %d times (budget %d, confirm phase %s): %v", n, devs, phase, all)
			}
		case "summary":
			if n > 2 {
				t.Fatalf("the window sent summary %d times (budget 2): %v", n, all)
			}
		default:
			if n > 0 {
				t.Fatalf("the window sent %s %d times (none allowed): %v", c, n, all)
			}
		}
	}
}

// v054cExtra: (v0.5.5) run every 2 s (scaled) during v054cConfirmRun, as one more reader: nil = none.
var v054cExtra func()

// v054cAtWrite: (v0.5.5) called just before v054cConfirmRun's clock write: nil = nothing.
var v054cAtWrite func()

// v054cScale: every time of the confirmation and the shared reading divided by k (a minute is 60/k s).
func v054cScale(t *testing.T, k time.Duration) {
	oQ, oS, oW, oP := box2ConfirmQuiet, box2ConfirmPollSlow, box2ConfirmWait, box2ConfirmPoll
	oA, oF, oFull := dashShareAge, dashShareForce, dashShareFull
	t.Cleanup(func() {
		box2ConfirmQuiet, box2ConfirmPollSlow, box2ConfirmWait, box2ConfirmPoll = oQ, oS, oW, oP
		dashShareAge, dashShareForce, dashShareFull = oA, oF, oFull
	})
	box2ConfirmQuiet, box2ConfirmPollSlow, box2ConfirmPoll = box2ConfirmQuietDefault/k, 5*time.Second/k, 2*time.Second/k
	box2ConfirmWait = 45 * time.Second / k
	dashShareAge, dashShareForce, dashShareFull = 15*time.Second/k, 20*time.Second/k, 60*time.Second/k
}

// v054cConfirmRun: a tuner clock write (690 → 650 MHz) that the driver takes after the quiet time plus confirmPhase,
// with the tuner's tick (every 10 s), the fan loop (every 10 s) and 3 open pages (every 8 s) reading the dashboard
// meanwhile, all times divided by k. Returns the miner-API commands per (scaled) minute in the quiet time and in the
// confirm phase after it.
func v054cConfirmRun(t *testing.T, k time.Duration, confirmPhase time.Duration, off bool) (quiet, phase float64, qBy, pBy map[string]int) {
	f := newC34(t, true, false)
	m := newV054cMiner(t, f)
	v054cScale(t, k)
	dashShareOff.Store(off)
	t.Cleanup(func() { dashShareOff.Store(false) })
	f.set(func() { f.ignore = true }) // the driver reports the new clock only when the test says so

	cfg := defaultTunerConfig()
	box2TunerDefaults(&cfg)
	cfg.DryRun, cfg.FanHold, cfg.FanTargetC, cfg.FanMinPct = false, true, 72, 30
	cfg.PollS = (10 * time.Second / k).Seconds()
	e := newTunerEngine(cfg)
	e.begin()
	e.readFan = func() (int, bool) { return 50, true }
	e.fanFloor, e.fanGuard = nil, nil
	e.applyFan = func(int) error { return nil }
	e.holdTarget = 72

	stop := make(chan struct{})
	var wg sync.WaitGroup
	every := func(d time.Duration, fn func()) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tk := time.NewTicker(d)
			defer tk.Stop()
			for {
				select {
				case <-stop:
					return
				case <-tk.C:
					fn()
				}
			}
		}()
	}
	fanC := fanConfig{Enabled: false, AbortC: 90, ThrottleEnable: false}
	every(10*time.Second/k, func() { fanTick(fanC) })
	if extra := v054cExtra; extra != nil { // (v0.5.5: the chip trims' re-apply, every 2 s)
		every(2*time.Second/k, extra)
	}
	for i := 0; i < 3; i++ {
		every(8*time.Second/k, func() {
			handleDashboard(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/dashboard", nil))
		})
	}
	time.Sleep(10 * time.Second / k) // (everyone running before the write)
	if at := v054cAtWrite; at != nil {
		at()
	}
	t0 := time.Now()
	takeAt := box2ConfirmQuiet + confirmPhase
	go func() {
		time.Sleep(takeAt)
		f.set(func() { f.clock, f.volt = 650, 42 })
	}()
	if err := e.applyPlan(planMV{MHz: 650, MV: box2LevelOf(42)}); err != nil {
		t.Fatalf("the write: %v", err)
	}
	t1 := time.Now()
	close(stop)
	wg.Wait()
	perMin := float64(time.Minute / k)
	qEnd := t0.Add(box2ConfirmQuiet)
	nq, qBy := m.count(t0, qEnd)
	np, pBy := m.count(qEnd, t1)
	_, v054cLastAll = m.count(t0, t1)
	v054cLastPhase = t1.Sub(qEnd) * k // (in real time)
	if t1.Sub(qEnd) < confirmPhase/2 {
		t.Fatalf("the confirm phase ran only %s", t1.Sub(qEnd))
	}
	return float64(nq) / float64(box2ConfirmQuiet) * perMin, float64(np) / float64(t1.Sub(qEnd)) * perMin, qBy, pBy
}

// D1: during a confirmation the tuner, the fan loop and 3 open pages together ask the miner API about 4 times a
// minute in the quiet time (one shared refresh every 15 s), and in the confirm phase only the confirmation's own
// poll plus at most one refresh every 15 s. Before v0.5.4c the same load was several times that.
func TestV054cDashShareConfirmBudget(t *testing.T) {
	const k = 10 // (a minute is 6 s)
	phase := 30 * time.Second / k
	bq, bp, bqBy, bpBy := v054cConfirmRun(t, k, phase, true)
	t.Logf("before (every reader afresh): quiet %.1f commands/min %v; confirm phase %.1f/min %v", bq, bqBy, bp, bpBy)
	q, p, qBy, pBy := v054cConfirmRun(t, k, phase, false)
	t.Logf("after (one shared reading): quiet %.1f commands/min %v; confirm phase %.1f/min %v; whole window %v", q, qBy, p, pBy, v054cLastAll)
	// (v0.5.5 F1) counted per window, quiet time and confirm phase together, not as a rate per phase: a single poll
	// landing either side of the phases' edge moved a per-minute rate across its limit
	v054cWindowBudget(t, v054cLastAll, v054cLastPhase)
	if bq <= 3*q {
		t.Fatalf("the shared reading saved nothing: before %.1f, after %.1f", bq, q)
	}
}

// D1: the tuner's heat abort never sees a temperature older than dashShareAge, and a refresh is forced once the
// shared reading is older than dashShareForce (other readers take a copy up to that age while a refresh runs).
func TestV054cDashShareAges(t *testing.T) {
	f := newC34(t, true, false)
	m := newV054cMiner(t, f)
	v054cScale(t, 20) // 15 s → 750 ms, 20 s → 1 s
	box2Confirming.Add(1)
	defer box2Confirming.Add(-1)
	var maxSeen time.Duration
	var hot atomic.Bool
	for i := 0; i < 40; i++ {
		if i == 25 {
			m.mu.Lock()
			m.temp = 86
			m.mu.Unlock()
			hot.Store(true)
		}
		at := time.Now()
		d := dashShareRead(dashShareAge, dashShareAge)
		dashShare.mu.Lock()
		age := at.Sub(dashShare.at)
		dashShare.mu.Unlock()
		if age > maxSeen {
			maxSeen = age
		}
		if tt, _, _ := maxBoardTemp(d); hot.Load() && i > 25+int(dashShareAge/(60*time.Millisecond))+1 && tt < 80 {
			t.Fatalf("read %d: the heat (86 °C) not seen %s after it started: %.1f", i, time.Duration(i-25)*60*time.Millisecond, tt)
		}
		time.Sleep(60 * time.Millisecond)
	}
	if maxSeen > dashShareAge+50*time.Millisecond {
		t.Fatalf("the heat abort saw a reading %s old (limit %s)", maxSeen, dashShareAge)
	}
	// a slow refresh in flight: a page takes the copy while it is at most dashShareForce old, the heat abort waits
	m.mu.Lock()
	m.delay = 400 * time.Millisecond
	m.mu.Unlock()
	dashShare.mu.Lock()
	dashShare.at = time.Now().Add(-dashShareAge - 10*time.Millisecond) // just over 15 s old
	dashShare.mu.Unlock()
	go dashShareRead(dashShareAge, dashShareForce) // starts the refresh
	time.Sleep(50 * time.Millisecond)
	start := time.Now()
	dashShareRead(dashShareAge, dashShareForce)
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Fatalf("a page waited %s for a refresh with a copy under %s", d, dashShareForce)
	}
	start = time.Now()
	dashShareRead(dashShareAge, dashShareAge)
	if d := time.Since(start); d < 200*time.Millisecond {
		t.Fatalf("the heat abort took a copy over %s old (%s)", dashShareAge, d)
	}
	// older than dashShareForce: everyone waits for a fresh one
	dashShare.mu.Lock()
	dashShare.at = time.Now().Add(-dashShareForce - 10*time.Millisecond)
	dashShare.mu.Unlock()
	go dashShareRead(dashShareAge, dashShareForce)
	time.Sleep(50 * time.Millisecond)
	start = time.Now()
	dashShareRead(dashShareAge, dashShareForce)
	if d := time.Since(start); d < 200*time.Millisecond {
		t.Fatalf("a page took a copy over %s old (%s)", dashShareForce, d)
	}
	// only one refresh at a time
	m.mu.Lock()
	m.delay = 200 * time.Millisecond
	m.mu.Unlock()
	dashShare.mu.Lock()
	dashShare.at = time.Now().Add(-time.Hour)
	dashShare.mu.Unlock()
	from := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); dashShareRead(dashShareAge, dashShareForce) }()
	}
	wg.Wait()
	if n, by := m.count(from, time.Now()); n > 2 {
		t.Fatalf("8 readers at once: %d commands %v (one refresh wanted)", n, by)
	}
}

// D1: outside a confirmation (and on the SC Lite) every Dashboard read asks the miner API, as before.
func TestV054cDashShareOnlyDuringConfirm(t *testing.T) {
	f := newC34(t, true, false)
	m := newV054cMiner(t, f)
	from := time.Now()
	for i := 0; i < 3; i++ {
		buildDashboard()
	}
	if n, _ := m.count(from, time.Now()); n != 6 {
		t.Fatalf("Box II outside a confirmation: %d commands for 3 readings (want 6)", n)
	}
	model = profSCLite
	box2Confirming.Add(1)
	defer box2Confirming.Add(-1)
	from = time.Now()
	for i := 0; i < 3; i++ {
		buildDashboard()
	}
	if n, _ := m.count(from, time.Now()); n != 6 {
		t.Fatalf("SC Lite: %d commands for 3 readings (want 6)", n)
	}
}

// D2: the tuner's plan is saved for the next boot only once the OEM's plan files in RAM hold it: files that lag the
// PUT are waited for, files that never match are never committed (the flash keeps its plan).
func TestV054cPersistWaitsForPlanFiles(t *testing.T) {
	f := newC34(t, true, false) // the live setting: 690 MHz / level 42 (stock)
	sh := withShadow(t)
	oW, oP := box2PersistMatchWait, box2PersistMatchPoll
	box2PersistMatchWait, box2PersistMatchPoll = 600*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { box2PersistMatchWait, box2PersistMatchPoll = oW, oP })
	_ = f // (the live setting the pass checks first)
	cpb := func(plan string) string {
		return `{"name":"Benchmark","powerplans":[{"info":"690 MHz 42 V 50 RPM 41 RPM"},{"info":"525 MHz 61 V 40 RPM 59 RPM"}],"select":0,"manual":true,"manualPowerplan":"` + plan + `"}`
	}
	bfg := func(set string) string {
		return `{"api-allow":"W:127.0.0.1","set":["intchains_qomo:vfff=` + set + `"],"pools":[{"url":"stratum+tcp://a:1"}]}`
	}
	const cpbRel, bfgRel = "minerd/settings/cpbsetting.json", "bfgminer/bfgminer.json"
	flashCpb, flashBfg := cpb("600 MHz 42 V 60 RPM 41 RPM"), bfg("42.000000:600:60:41")
	reset := func(ramCpb, ramBfg string) {
		sh.putBoth(t, cpbRel, ramCpb, flashCpb)
		sh.putBoth(t, bfgRel, ramBfg, flashBfg)
	}
	// the backend still writing the test clock (650) when the PUT of 690 was answered: never committed
	reset(cpb("650 MHz 42 V 70 RPM 41 RPM"), bfg("42.000000:650:70:41"))
	if box2PersistPass(690, 42, "") {
		t.Fatal("committed while the RAM plan files said 650 MHz")
	}
	if s, _ := sh.onFlash(cpbRel); s != flashCpb {
		t.Fatalf("cpbsetting.json changed on the flash: %q", s)
	}
	if s, _ := sh.onFlash(bfgRel); s != flashBfg {
		t.Fatalf("bfgminer.json changed on the flash: %q", s)
	}
	// one file right, the other still old: not committed either
	reset(cpb("690 MHz 42 V 70 RPM 41 RPM"), bfg("42.000000:650:70:41"))
	if box2PersistPass(690, 42, "") {
		t.Fatal("committed with bfgminer.json at 650 MHz")
	}
	reset(cpb("650 MHz 42 V 70 RPM 41 RPM"), bfg("42.000000:690:70:41"))
	if box2PersistPass(690, 42, "") {
		t.Fatal("committed with cpbsetting.json at 650 MHz")
	}
	// the files lag the PUT, then catch up: committed once they match
	reset(cpb("650 MHz 42 V 70 RPM 41 RPM"), bfg("42.000000:650:70:41"))
	go func() {
		time.Sleep(150 * time.Millisecond)
		os.WriteFile(sh.ram+"/"+cpbRel, []byte(cpb("690 MHz 42 V 50 RPM 41 RPM")), 0o644)
		time.Sleep(100 * time.Millisecond)
		os.WriteFile(sh.ram+"/"+bfgRel, []byte(bfg("42.000000:690:50:41")), 0o644)
	}()
	if !box2PersistPass(690, 42, "") {
		t.Fatal("the matching plan files weren't committed")
	}
	if s, _ := sh.onFlash(cpbRel); !strings.Contains(s, "690 MHz") {
		t.Fatalf("cpbsetting.json on the flash: %q", s)
	}
	if s, _ := sh.onFlash(bfgRel); !strings.Contains(s, ":690:") {
		t.Fatalf("bfgminer.json on the flash: %q", s)
	}
	// the live setting moved on (a throttle step): nothing committed, at once
	reset(cpb("690 MHz 42 V 50 RPM 41 RPM"), bfg("42.000000:690:50:41"))
	start := time.Now()
	if box2PersistPass(650, 42, "") || time.Since(start) > 300*time.Millisecond {
		t.Fatal("a plan the miner no longer runs")
	}
	if ok, why := box2PlanFilesAt(690, 42); !ok {
		t.Fatalf("box2PlanFilesAt: %s", why)
	}
}

// D3: the stock put-back keeps the raised fans (70 %) in the setting it puts back while the clock ramps, then the
// owner's setting goes back verbatim (its own fans).
func TestV054cPutBackKeepsRaisedFans(t *testing.T) {
	f := newC34(t, true, false)
	oQ := box2ConfirmQuiet
	box2ConfirmQuiet = 200 * time.Millisecond
	t.Cleanup(func() { box2ConfirmQuiet = oQ })
	// the owner's own plan: stock 690 MHz, its fans at 40 % (manual, as the panel writes a stock plan with fans)
	f.set(func() {
		f.setting["manual"] = true
		f.setting["manualPowerplan"] = "690 MHz 42 V 40 RPM 41 RPM"
	})
	restore, what := box2TunerBefore()
	if restore == nil {
		t.Fatalf("no restorer for the stock plan (%q)", what)
	}
	ownerRaw, _ := json.Marshal(f.setting)
	// the run left a test clock at 55 %
	f.set(func() {
		f.setting["manualPowerplan"] = "650 MHz 42 V 55 RPM 41 RPM"
		f.puts = nil
	})
	if err := restore(); err != nil {
		t.Fatal(err)
	}
	plans := f.putPlans()
	if len(plans) != 3 {
		t.Fatalf("puts: %q", plans)
	}
	if !strings.HasPrefix(plans[0], "650 MHz 42 V 70 RPM") {
		t.Fatalf("the fans first: %q", plans)
	}
	if plans[1] != "690 MHz 42 V 70 RPM 41 RPM" {
		t.Fatalf("the clock raise must carry the raised fans, not the owner's 40 %%: %q", plans)
	}
	if plans[2] != "690 MHz 42 V 40 RPM 41 RPM" {
		t.Fatalf("after the ramp, the owner's setting back: %q", plans)
	}
	f.mu.Lock()
	last, _ := json.Marshal(f.puts[2])
	f.mu.Unlock()
	if string(last) != string(ownerRaw) {
		t.Fatalf("the owner's setting not put back as it was:\n%s\n%s", last, ownerRaw)
	}
	// a stock plan selected (manual off): the raise is written into the selected plan's text the same way
	f.set(func() {
		json.Unmarshal([]byte(box2Setting), &f.setting)
	})
	restore, _ = box2TunerBefore()
	f.set(func() {
		f.setting["manual"] = true
		f.setting["manualPowerplan"] = "650 MHz 42 V 55 RPM 41 RPM"
		f.puts = nil
	})
	if err := restore(); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	n := len(f.puts)
	var mid map[string]any
	if n >= 2 {
		mid = f.puts[1]
	}
	f.mu.Unlock()
	if n != 3 || mid["manual"] != false || !strings.Contains(fmt.Sprint(mid["powerplans"]), "690 MHz 42 V 70 RPM 41 RPM") {
		t.Fatalf("stock select: %d puts, the raise %v", n, mid)
	}
	// the fan control changed the fans meanwhile: they stay as it set them (no hand-back)
	f.set(func() {
		f.setting["manual"] = true
		f.setting["manualPowerplan"] = "690 MHz 42 V 40 RPM 41 RPM"
	})
	restore, _ = box2TunerBefore()
	f.set(func() {
		f.setting["manualPowerplan"] = "650 MHz 42 V 55 RPM 41 RPM"
		f.puts = nil
	})
	go func() { // the fan control writes once the setting is back (during the ramp)
		for i := 0; i < 100 && len(f.putPlans()) < 2; i++ {
			time.Sleep(5 * time.Millisecond)
		}
		f.set(func() { f.setting["manualPowerplan"] = "690 MHz 42 V 85 RPM 41 RPM" })
	}()
	if err := restore(); err != nil {
		t.Fatal(err)
	}
	if p := f.putPlans(); len(p) != 2 {
		t.Fatalf("handed back over the fan control's change: %q", p)
	}
}

// D4: a fan-only write that fails before a clock raise: the error is returned and the clock isn't raised.
func TestV054cFanWriteFailNoRaise(t *testing.T) {
	e, f, _, _, _ := v054Engine(t)
	if err := e.applyPlan(planMV{MHz: 600, MV: box2LevelOf(42)}); err != nil {
		t.Fatal(err)
	}
	e.fanCur = 55
	var fail atomic.Bool
	fail.Store(true)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PUT" && fail.Load() {
			http.Error(w, "busy", 500)
			return
		}
		f.srv.Config.Handler.ServeHTTP(w, r)
	}))
	defer proxy.Close()
	oldWeb := *minerWeb
	*minerWeb = strings.TrimPrefix(proxy.URL, "http://")
	defer func() { *minerWeb = oldWeb }()
	f.set(func() { f.puts = nil })
	err := e.applyPlan(planMV{MHz: 650, MV: box2LevelOf(42)})
	if err == nil {
		t.Fatal("a failed fan write before a raise: no error")
	}
	f.mu.Lock()
	clk, puts := f.clock, len(f.puts)
	f.mu.Unlock()
	if clk != 600 || puts != 0 {
		t.Fatalf("the clock was raised anyway: %v MHz, %d puts", clk, puts)
	}
	fail.Store(false)
	if err := e.applyPlan(planMV{MHz: 650, MV: box2LevelOf(42)}); err != nil {
		t.Fatal(err)
	}
	if p := f.putPlans(); len(p) != 2 || !strings.HasPrefix(p[0], "600 MHz 42 V 70 RPM") || !strings.HasPrefix(p[1], "650 MHz") {
		t.Fatalf("again: %q", p)
	}
}

// D5: an owner's save counts only the plan PUTs made by its own request: a tuner / fan-hold / throttle PUT that
// overlaps it doesn't make it save the plan files.
func TestV054cOwnerSaveOwnPutsOnly(t *testing.T) {
	sh := withShadow(t)
	const cpbRel = "minerd/settings/cpbsetting.json"
	sh.putBoth(t, cpbRel, `{"manual":true,"manualPowerplan":"650 MHz 42 V 70 RPM 41 RPM"}`, `{"manual":true,"manualPowerplan":"690 MHz 42 V 50 RPM 41 RPM"}`)
	// a background PUT while the owner's request (no plan write of its own) runs
	cfgUserSave(func() {
		done := make(chan struct{})
		go func() { stockSettingPut(nil); close(done) }()
		<-done
		stockOtherPuts.Add(1) // (the owner's own change: a pool)
	})
	if s, _ := sh.onFlash(cpbRel); strings.Contains(s, "650") {
		t.Fatalf("a background clock saved as the owner's plan: %q", s)
	}
	// the owner's own plan PUT: handed to box2PersistPlan (its checks), with the plan the PUT asked for; not
	// committed straight away (v0.5.5 P2)
	got := make(chan planMV, 4)
	oldPersist := box2PersistPlan
	box2PersistPlan = func(mhz, code int) { got <- planMV{MHz: mhz, MV: code} }
	t.Cleanup(func() { box2PersistPlan = oldPersist })
	cfgUserSave(func() { stockSettingPut([]byte(`{"manual":true,"manualPowerplan":"650 MHz 42 V 70 RPM 41 RPM"}`)) })
	select {
	case p := <-got:
		if p.MHz != 650 || p.MV != 42 {
			t.Fatalf("persisted %+v", p)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the owner's plan wasn't handed to box2PersistPlan")
	}
	if s, _ := sh.onFlash(cpbRel); strings.Contains(s, "650") {
		t.Fatalf("the plan files were committed without box2PersistPlan's checks: %q", s)
	}
	if goID() == 0 {
		t.Fatal("no goroutine id")
	}
	// an owner's request whose own plan PUT runs while a background one does too: its own is counted
	var mine atomic.Int64
	cfgUserSave(func() {
		go stockSettingPut(nil)
		stockSettingPut(nil)
		mine.Add(1)
	})
	if mine.Load() != 1 {
		t.Fatal("the request didn't run")
	}
}

// D7: syslogRemoteSet / the start's restart are serialised: two restarts never overlap.
func TestV054cSyslogApplySerialised(t *testing.T) {
	useBox2(t)
	dir := t.TempDir()
	old := *authFile
	*authFile = dir + "/auth.json"
	var in, most atomic.Int32
	oldApply := syslogRemoteApply
	syslogRemoteApply = func() error {
		n := in.Add(1)
		if n > most.Load() {
			most.Store(n)
		}
		time.Sleep(30 * time.Millisecond)
		in.Add(-1)
		return nil
	}
	t.Cleanup(func() { syslogRemoteApply = oldApply; *authFile = old; syslogApplyErrSet("") })
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			syslogRemoteSet(fmt.Sprintf("192.168.1.%d:514", 20+i))
		}(i)
	}
	wg.Wait()
	if most.Load() != 1 {
		t.Fatalf("%d syslogd restarts at once", most.Load())
	}
}

// D6/D7: the boot script's address check (exactly 4 octets 0-255, a port 1-65535) and its lock.
func TestV054cS48AddrAndLock(t *testing.T) {
	src, err := os.ReadFile("../kit/files/S48scbox2")
	if err != nil {
		t.Skip(err)
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	s := string(src)
	fn := s[strings.Index(s, "syslog_addr_ok() {"):strings.Index(s, "syslog_remote() {")]
	d := t.TempDir()
	fn = strings.NewReplacer("/var/run/", d+"/run/", "/proc/", d+"/proc/").Replace(fn)
	os.MkdirAll(d+"/run", 0o755)
	os.MkdirAll(d+"/proc", 0o755)
	run := func(body string) (string, error) {
		out, err := exec.Command(sh, "-c", "usleep() { :; }\n"+fn+"\n"+body).CombinedOutput()
		return string(out), err
	}
	for a, ok := range map[string]bool{
		"1.2.3.4:514": true, "192.168.1.20:1514": true, "10.0.0.5:65535": true, "255.255.255.255:1": true,
		"1.2.3.999:514": false, "1.2.3.:514": false, "1.2.3.4:0": false, "1.2.3.4:70000": false, "logs.lan:514": false,
		"01.2.3.4:514": false, "1..2.3:514": false, "1.2.3:514": false, "1.2.3.4.5:514": false, ".1.2.3:514": false,
		"1.2.3.4:": false, "1.2.3.4:01": false, "0.0.0.0:514": false, "224.0.0.1:514": false, "1.2.3.4:5:6": false,
		"1.2.3.256:514": false, "1.2.3.0004:514": false,
	} {
		out, err := run(`syslog_addr_ok "` + a + `" && echo YES || echo NO`)
		if err != nil || strings.Contains(out, "YES") != ok {
			t.Errorf("%q: %q %v (want ok=%v)", a, out, err, ok)
		}
	}
	// the lock: taken, held against a second taker, a stale one (its holder gone) taken over
	if out, err := run(`syslog_lock && cat ` + d + `/run/scbox2-syslog.lock/pid`); err != nil || strings.TrimSpace(out) == "" {
		t.Fatalf("lock: %q %v", out, err)
	}
	os.MkdirAll(d+"/proc/4242", 0o755) // a holder that runs
	os.WriteFile(d+"/run/scbox2-syslog.lock/pid", []byte("4242\n"), 0o644)
	if out, err := run(`syslog_lock && echo GOT || echo BUSY`); err != nil || !strings.Contains(out, "BUSY") || !strings.Contains(out, "still running") {
		t.Fatalf("a held lock was taken: %q %v", out, err)
	}
	os.RemoveAll(d + "/proc/4242") // its holder is gone
	if out, err := run(`syslog_lock && echo GOT && syslog_unlock`); err != nil || !strings.Contains(out, "GOT") {
		t.Fatalf("a stale lock wasn't taken over: %q %v", out, err)
	}
	if _, err := os.Stat(d + "/run/scbox2-syslog.lock"); err == nil {
		t.Fatal("not released")
	}
	// a lock with no holder written (a step killed between mkdir and its pid): taken over after ~2 s
	os.MkdirAll(d+"/run/scbox2-syslog.lock", 0o755)
	if out, err := run(`syslog_lock && echo GOT`); err != nil || !strings.Contains(out, "GOT") {
		t.Fatalf("an empty lock wasn't taken over: %q %v", out, err)
	}
}
