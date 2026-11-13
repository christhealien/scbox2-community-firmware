package main

// r5-races: a concurrency stress harness for every writer of the miner's power plan and fans, and every
// "who's in charge" check. The OEM backend is faked in-process (http.DefaultTransport) with random
// latency and random failures (a PUT may fail before it lands, or land and still answer 500, as the real
// backend does now and then). Every PUT is checked as it lands:
//   - PV never above 9600
//   - fans never under the floor while the plan hashes
//   - an Idle taken by the thermal guard, the fan lock or the pool guard (or by the owner's Idle) is woken only
//     by the owner (an owner action that started after that Idle landed), never by anything else; the fan
//     lock's and the pool guard's Idle not even by the owner while the hold is on
// After the actors stop and the runs end ("settled") the charge must match what runs. A watchdog dumps every
// goroutine and fails when nothing makes progress for a while (deadlock).

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const rcPlans = `[{"info":"615 MHz 9100 V 40 RPM 40 RPM PV 9400","level":0},{"info":"0 MHz 0 V 100 RPM 100 RPM","level":3},{"info":"550 MHz 8900 V 60 RPM 60 RPM PV 9200","level":1}]`

type rcIdle struct {
	kind string // heat | lock | pool | user | other
	seq  int64  // the backend's write counter when it landed
	by   string
}

type rcBackend struct {
	mu      sync.Mutex
	setting map[string]any
	rng     *rand.Rand
	maxLat  time.Duration
	failP   float64 // each request: fail (before landing, or for a PUT sometimes after landing)
	seq     int64
	idle    *rcIdle // the Idle the miner is on now and who put it there (nil = hashing)
	owners  map[int64]int64
	viol    map[string]int
	example map[string]string
	puts    int
	wakes   map[string]int
	fanDir  string
	off     atomic.Bool // stop failing/latency (settling)
	// hooks for the focused tests: called (outside mu) with the caller's stack before a GET answers / a PUT lands
	onGet      func(stack string)
	onPut      func(stack string, nw map[string]any)
	holdLanded map[string]bool // the fan lock's / pool guard's own Idle landed (cleared when the owner releases it)
	ring       []string        // the last writes, for a violation's trace
	takeover   bool            // the owner took over (Release, a run started from Idle) while a stop's Idle was on its way
	traces     []string
}

func (b *rcBackend) note(s string) { // (b.mu held)
	b.ring = append(b.ring, s)
	if len(b.ring) > 40 {
		b.ring = b.ring[1:]
	}
}

func gid() int64 {
	var b [64]byte
	n := runtime.Stack(b[:], false)
	f := strings.Fields(string(b[:n]))
	id, _ := strconv.ParseInt(f[1], 10, 64)
	return id
}

// owner: this goroutine runs an owner's action from now (its start counts against Idles landed before it).
func (b *rcBackend) owner() func() {
	g := gid()
	b.mu.Lock()
	b.owners[g] = b.seq
	b.mu.Unlock()
	return func() { b.mu.Lock(); delete(b.owners, g); b.mu.Unlock() }
}

func (b *rcBackend) bad(kind, detail string) {
	b.viol[kind]++
	if _, ok := b.example[kind]; !ok {
		b.example[kind] = detail
	}
}

func (b *rcBackend) sleep() {
	if b.off.Load() || b.maxLat <= 0 {
		return
	}
	b.mu.Lock()
	d := time.Duration(b.rng.Int63n(int64(b.maxLat)))
	b.mu.Unlock()
	time.Sleep(d)
}

func (b *rcBackend) roll() float64 {
	if b.off.Load() {
		return 1
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.rng.Float64()
}

// writer: who is writing, from the caller's stack (outermost known frame wins for the safety stops).
var rcWriters = []string{"main.thermalIdle", "main.fanLockEngage", "main.rcPoolGuard", "main.fanTestRerunSteps",
	"main.handleControl", "main.handlePresets", "main.scheduleTick", "main.runCeilingProbe", "main.fanTick",
	"main.(*tunerEngine).Run", "main.settingRestorer", "main.snapshotMinerSetting", "main.tunerForceEnd", "main.applyIdle"}

func writerOf(stack string) string {
	for _, w := range rcWriters {
		w2 := "sclite-agent." + strings.TrimPrefix(w, "main.")
		if strings.Contains(stack, w2+"(") || strings.Contains(stack, w2+".func") {
			return strings.TrimPrefix(w, "main.")
		}
	}
	if os.Getenv("R5_DEBUG") != "" {
		fmt.Fprintln(os.Stderr, "UNKNOWN WRITER:", stack[:min(len(stack), 3000)])
	}
	return "?"
}

func idleKindOf(w string) string {
	switch w {
	case "thermalIdle":
		return "heat"
	case "fanLockEngage":
		return "lock"
	case "rcPoolGuard":
		return "pool"
	case "handlePresets", "scheduleTick", "handleControl":
		return "user"
	}
	return "other"
}

func planOf(cur map[string]any) (powerplan, bool) { return resolveRunningPlan(cur) }

func (b *rcBackend) RoundTrip(r *http.Request) (*http.Response, error) {
	resp := func(code int, body string) *http.Response {
		return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}, Request: r}
	}
	switch {
	case strings.HasPrefix(r.URL.Path, "/user/login"):
		return resp(200, `{"JWT Token":"tok"}`), nil
	case r.URL.Path != "/mcb/setting":
		return resp(404, "no"), nil
	}
	if r.Method == "GET" {
		if b.onGet != nil {
			var st [16 << 10]byte
			b.onGet(string(st[:runtime.Stack(st[:], false)]))
		}
		b.sleep()
		if b.roll() < b.failP {
			return nil, fmt.Errorf("connection reset (fake)")
		}
		b.mu.Lock()
		out, _ := json.Marshal(b.setting)
		b.mu.Unlock()
		b.sleep()
		return resp(200, string(out)), nil
	}
	body, _ := io.ReadAll(r.Body)
	var stack [16 << 10]byte
	putStack := string(stack[:runtime.Stack(stack[:], false)])
	who := writerOf(putStack)
	floor := fanFloor()
	g := gid()
	b.sleep()
	x := b.roll()
	if x < b.failP/2 {
		return resp(500, "backend died (fake, not applied)"), nil
	}
	var nw map[string]any
	if err := json.Unmarshal(body, &nw); err != nil {
		return resp(400, "bad json"), nil
	}
	if b.onPut != nil {
		b.onPut(putStack, nw)
	}
	b.mu.Lock()
	b.puts++
	b.seq++
	b.setting = nw
	pp, ok := planOf(nw)
	idleNow := minerIdle(nw)
	b.note(fmt.Sprintf("seq %d g%d %-22s idle=%v plan=%d/%d fans %d charge=%q heatSince=%v", b.seq, g, who, idleNow, pp.FreqMHz, pp.PVMv, pp.Fan1, getCharge(), heatIdleSince.Load() != 0))
	if ok && pp.PVMv > pvHardMax {
		b.bad("PV>9600", fmt.Sprintf("%s wrote PV %d", who, pp.PVMv))
	}
	if ok && !idleNow && pp.FreqMHz > 0 && min(pp.Fan1, pp.Fan2) < floor {
		b.bad("fan<floor", fmt.Sprintf("%s wrote fans %d/%d with the floor at %d", who, pp.Fan1, pp.Fan2, floor))
	}
	if idleNow {
		if b.idle == nil {
			b.idle = &rcIdle{kind: idleKindOf(who), seq: b.seq, by: who}
		}
		if k := idleKindOf(who); k == "lock" || k == "pool" || k == "heat" {
			b.idle.kind, b.idle.by = k, who // a safety stop's Idle over an Idle that was there: now it's held
			b.holdLanded[k] = true
			if k == "heat" && b.takeover {
				b.idle.kind = "user" // the owner took it over before it landed
			}
		}
		b.takeover = false
	} else if b.takeover = false; b.idle != nil {
		// a wake: legal only for the owner (an action of theirs that started after this Idle landed), and for
		// the schedule after an Idle of the owner's
		id := b.idle
		start, isOwner := b.owners[g]
		legal := isOwner && start >= id.seq
		_, locked := loadFanLock()
		_, held := poolHold()
		locked = locked && b.holdLanded["lock"]
		held = held && b.holdLanded["pool"]
		if who == "handlePresets" || who == "handleControl" {
			// the owner's own requests: they pass their checks under controlMu, which this harness can't see, so a
			// request queued before an Idle and run after it is the owner's later choice (the focused tests cover
			// a request that passed its checks before the Idle)
			legal = true
		}
		if (id.kind == "user" || id.kind == "lock" || id.kind == "pool" || id.by == "(*tunerEngine).Run") && who == "scheduleTick" {
			// the owner's Idle, or a hold the owner has since released: the owner's own requests and the schedule
			// may change it (they're serialized with the owner's Idle by controlMu: the later one wins)
			legal = true
		}
		if locked || held {
			legal = false // nothing wakes a held miner, not even the owner
		}
		b.wakes[who]++
		if !legal {
			k := "woke " + id.kind + " Idle"
			if len(b.traces) < 3 {
				b.traces = append(b.traces, fmt.Sprintf("%s by %s (owner start %d, Idle seq %d):\n  %s", k, who, start, id.seq, strings.Join(b.ring, "\n  ")))
			}
			b.bad(k, fmt.Sprintf("%s woke the %s Idle that %s put on (owner=%v)", who, id.kind, id.by, isOwner))
			b.bad(k+" by "+who, "")
		}
		b.idle = nil
	}
	b.mu.Unlock()
	if ok {
		lv := []int{pp.Fan1, pp.Fan1, pp.Fan2, pp.Fan2}
		for i, v := range lv {
			os.WriteFile(filepath.Join(b.fanDir, "fan"+strconv.Itoa(i), "lvl"), []byte(strconv.Itoa(v)), 0644)
		}
	}
	b.sleep()
	if x < b.failP {
		return resp(500, "backend died (fake, applied)"), nil
	}
	return resp(200, `{"status":0}`), nil
}

func (b *rcBackend) state() (map[string]any, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	c := map[string]any{}
	for k, v := range b.setting {
		c[k] = v
	}
	return c, minerIdle(c)
}

// rcResetShared: package state other tests leave behind (a run still ending, a probe, the schedule's memory),
// so the stress run starts the same way alone or in the full suite.
func rcResetShared() {
	for i := 0; i < 500; i++ { // a run or probe of an earlier test still finishing
		tunerMu.Lock()
		tr := tunerCur != nil && tunerCur.isRunning()
		tunerMu.Unlock()
		ceilMu.Lock()
		cr := ceilSt.Running
		ceilMu.Unlock()
		if !tr && !cr {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	tunerMu.Lock()
	tunerCur = nil
	tunerMu.Unlock()
	ceilMu.Lock()
	ceilSt = ceilingState{}
	ceilMu.Unlock()
	ceilAbort.Store(false)
	ceilGen.Store(noGen)
	schedRunMu.Lock()
	lastSchKey, lastSchName, schedSince, schedBefore, schedBeforeBy = -1, "", 0, nil, ""
	schedRunMu.Unlock()
	fanLockMu.Lock()
	fanLockRun, fanUnlockAt = 0, 0
	fanLockMu.Unlock()
}

// rcSetup: temp config dir, fake backend, fan files, presets, schedule; everything the writers read.
func rcSetup(t *testing.T, seed int64, maxLat time.Duration, failP float64) *rcBackend {
	dir := t.TempDir()
	b := &rcBackend{rng: rand.New(rand.NewSource(seed)), maxLat: maxLat, failP: failP, owners: map[int64]int64{},
		viol: map[string]int{}, example: map[string]string{}, wakes: map[string]int{}, holdLanded: map[string]bool{}}
	json.Unmarshal([]byte(`{"manual":true,"tempcontrol":true,"select":0,"manualPowerplan":"600 MHz 9000 V 60 RPM 60 RPM PV 9300","powerplans":`+rcPlans+`}`), &b.setting)
	b.fanDir = filepath.Join(dir, "fans")
	for i := 0; i < 4; i++ {
		os.MkdirAll(filepath.Join(b.fanDir, "fan"+strconv.Itoa(i)), 0755)
		os.WriteFile(filepath.Join(b.fanDir, "fan"+strconv.Itoa(i), "lvl"), []byte("60"), 0644)
	}
	oldT := http.DefaultTransport
	http.DefaultTransport = b
	saved := []any{*authFile, *fanPath, *presetPath, *hrPath, *tunerDir, *apiAddr, *minerWeb, backupRoot, fanSysDir,
		wakeWait, fanDash, fanNow, throttleEvery, schedPath, fanLockIdle, fanLockNotify, pgIdle, readWDGuard, heatEscalateAfter}
	*authFile = filepath.Join(dir, "auth.json")
	*fanPath = filepath.Join(dir, "fan.json")
	*presetPath = filepath.Join(dir, "presets.json")
	*hrPath = filepath.Join(dir, "hr.json")
	*tunerDir = filepath.Join(dir, "tuner")
	*apiAddr = "127.0.0.1:1"
	*minerWeb = "fake.miner"
	backupRoot = filepath.Join(dir, "backup")
	fanSysDir = b.fanDir
	wakeWait = 2 * time.Millisecond
	throttleEvery = 0
	schedPath = filepath.Join(dir, "schedule.json")
	fanLockIdle = func() error { return applyIdle() }
	fanLockNotify = func(string) {}
	pgIdle = applyIdle
	notifyTestHook = func(string, string) {}
	jwtMu.Lock()
	jwtToken = ""
	jwtMu.Unlock()
	authMu.Lock()
	auth.MinerPass = "pw"
	authMu.Unlock()
	setCharge("")
	heatEsc = heatEscState{}
	heatIdleSince.Store(0)
	heatHoldSaved.Store(false)
	r4ResetLoop()
	rcResetShared() // what earlier tests in the package may have left behind
	fanGuardClear()
	hgMu.Lock()
	hgHot, hgAt = 0, time.Time{}
	hgMu.Unlock()
	fanMu.Lock()
	fanCfg = defaultFanConfig()
	fanHist = nil
	reflect.ValueOf(&fanSt).Elem().Set(reflect.Zero(reflect.TypeOf(fanSt)))
	fanMu.Unlock()
	presets := []preset{
		{Name: "Fast", MHz: 650, MV: 9100, PV: 9400, FanProfile: "none"},
		{Name: "Slow", MHz: 500, MV: 8800, PV: 9100, FanProfile: "curve-quiet"},
		{Name: "Top", MHz: 700, MV: 9300, PV: 9600, FanProfile: "none"},
	}
	pb, _ := json.Marshal(presets)
	os.WriteFile(*presetPath, pb, 0644)
	t.Cleanup(func() {
		http.DefaultTransport = oldT
		*authFile, *fanPath, *presetPath, *hrPath, *tunerDir, *apiAddr, *minerWeb = saved[0].(string), saved[1].(string), saved[2].(string), saved[3].(string), saved[4].(string), saved[5].(string), saved[6].(string)
		backupRoot, fanSysDir = saved[7].(string), saved[8].(string)
		wakeWait, fanDash, fanNow, throttleEvery = saved[9].(time.Duration), saved[10].(func() dashboard), saved[11].(func() time.Time), saved[12].(time.Duration)
		schedPath, fanLockIdle, fanLockNotify, pgIdle = saved[13].(string), saved[14].(func() error), saved[15].(func(string)), saved[16].(func() error)
		readWDGuard, heatEscalateAfter = saved[17].(func() (watchdogView, bool)), saved[18].(time.Duration)
		notifyTestHook = nil
		setCharge("")
		heatEsc = heatEscState{}
		heatIdleSince.Store(0)
		fanGuardClear()
		hgMu.Lock()
		hgHot, hgAt = 0, time.Time{}
		hgMu.Unlock()
		tunerMu.Lock()
		tunerCur = nil
		tunerMu.Unlock()
	})
	return b
}

var rcStatMu sync.Mutex
var rcStats = map[string]int{}

func rcStat(k string) { rcStatMu.Lock(); rcStats[k]++; rcStatMu.Unlock() }

var rcAct func(string) // the stress test's action log (nil elsewhere)

func rcPost(h http.HandlerFunc, v any) map[string]any {
	body, _ := json.Marshal(v)
	defer func() {
		if rcAct != nil {
			b, _ := json.Marshal(v)
			rcAct(fmt.Sprintf("g%d post %s -> charge %q", gid(), b, getCharge()))
		}
	}()
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest("POST", "/", bytes.NewReader(body)))
	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	e, _ := out["err"].(string)
	if len(e) > 50 {
		e = e[:50]
	}
	rcStat(fmt.Sprintf("%v %s", out["ok"], e))
	return out
}

// rcPoolGuard: the pool guard's hold, the same calls in the same order as pgCheck's Idle branch.
func rcPoolGuard() {
	poolHoldSet("stratum+tcp://evil.example:3333 (account x)")
	stopTunerFor("the pool guard put the miner on Idle")
	if err := pgIdle(); err == nil {
		setCharge("idle")
	}
}

// rcTunerStart: handleTuner's start, with an engine whose chip readings are simulated but whose plan writes,
// "before" snapshot, hold and charge checks are the real ones.
func rcTunerStart(b *rcBackend) (*tunerEngine, func()) {
	controlMu.Lock()
	defer controlMu.Unlock()
	gen := rcGenNote()
	done := b.owner()          // the owner's start counts from here (after controlMu, as the handler's checks)
	if getCharge() == "idle" { // a run started from an Idle (the owner's choice, whoever's Idle it was)
		b.mu.Lock()
		b.owners[gid()] = 1 << 62
		b.mu.Unlock()
	}
	if c := getCharge(); c == "tuner" || c == "ceiling" || miningHoldErr() != nil || sctRunning() {
		done()
		return nil, nil
	}
	cfg := fastCfg()
	cfg.MinMV, cfg.MaxMV, cfg.MVStep = 8800, 9300, 250
	e, _ := newSimEngine(cfg, 8900, ckey{2, 5})
	base := e.sleep
	e.sleep = func(d time.Duration) { base(d); time.Sleep(50 * time.Microsecond) }
	e.before = snapshotMinerSetting
	e.chargeNow = getCharge
	e.restoreWait = 50 * time.Millisecond
	rcRealTunerHooks(e, gen) // applyPlan and held as newTunerEngine sets them (and, patched, handleTuner's safetyGen note)
	tunerMu.Lock()
	if tunerCur != nil && tunerCur.isRunning() {
		tunerMu.Unlock()
		done()
		return nil, nil
	}
	tunerCur = e
	e.begin()
	tunerMu.Unlock()
	e.prevCharge = getCharge()
	if !rcTakeCharge(gen, "tuner") {
		e.StopFor("safety stop at the start")
	} else if b.mu.Lock(); e.prevCharge == "idle" || b.takeover {
		b.mu.Unlock()
		// the owner started a run from an Idle (handleTuner allows it, whoever's Idle it was): the run's writes are
		// the owner's, and the Idle is the owner's from now on
		b.mu.Lock()
		b.owners[gid()] = 1 << 62
		if b.idle != nil {
			b.idle.kind = "user"
		}
		b.takeover = true
		b.mu.Unlock()
	} else {
		b.mu.Unlock()
	}
	return e, done
}

// rcCeilingStart: the limit probe's Start button (handleCeiling, the real one).
func rcCeilingStart() bool {
	if !loadOverclock().Acknowledged {
		saveOverclock(overclockState{Acknowledged: true})
	}
	out := rcPost(handleCeiling, map[string]any{"start": true})
	return out["ok"] == true
}

type rcTemp struct {
	mu    sync.Mutex
	t     float64
	blind bool
}

// TestR5RaceStress: every writer at once, for a while, under -race. R5_ITERS / R5_SEED / R5_LAT_MS tune it.
func TestR5RaceStress(t *testing.T) {
	iters := 400
	if s := os.Getenv("R5_ITERS"); s != "" {
		iters, _ = strconv.Atoi(s)
	}
	seed := time.Now().UnixNano()
	if s := os.Getenv("R5_SEED"); s != "" {
		seed, _ = strconv.ParseInt(s, 10, 64)
	}
	lat := 3 * time.Millisecond
	if s := os.Getenv("R5_LAT_MS"); s != "" {
		v, _ := strconv.Atoi(s)
		lat = time.Duration(v) * time.Millisecond
	}
	b := rcSetup(t, seed, lat, 0.08)
	rcAct = func(s string) { b.mu.Lock(); b.note("      " + s); b.mu.Unlock() }
	defer func() { rcAct = nil }()
	t.Logf("seed %d, %d iterations per actor, latency up to %v", seed, iters, lat)

	// fan curve config: throttle on and low, so the throttle tier writes often
	fanMu.Lock()
	fanCfg.Smoothing = 0
	fanCfg.ThrottleEnable, fanCfg.ThrottleC, fanCfg.ThrottleStep, fanCfg.ResumeC = true, 84, 25, 78
	fanCfg.CooldownS = 15
	fanMu.Unlock()
	pairOn := filepath.Join(filepath.Dir(*authFile), "fan-pair.json")
	_ = pairOn

	temp := &rcTemp{t: 70}
	fanDash = func() dashboard {
		_, idle := b.state()
		temp.mu.Lock()
		tc, blind := temp.t, temp.blind
		temp.mu.Unlock()
		noteHottest(tc)
		var d dashboard
		d.OK = true
		for i := 0; i < 4; i++ {
			g := 900.0
			if idle {
				g = 0
			}
			ts := []float64{tc - 2, tc - 1, tc}
			if blind {
				ts = []float64{-150, -150, -150}
			}
			d.Boards = append(d.Boards, board{ID: i, Alive: !idle, GHS: g, Temps: ts})
		}
		return d
	}
	var vclock atomic.Int64
	vclock.Store(time.Now().UnixNano())
	fanNow = func() time.Time { return time.Unix(0, vclock.Add(int64(10*time.Second))) }
	heatEscalateAfter = 20 * time.Second
	var wdFlag atomic.Bool
	readWDGuard = func() (watchdogView, bool) {
		if !wdFlag.Load() {
			return watchdogView{}, true
		}
		return watchdogView{Status: 1, Fans: []int{0, 3000, 3000, 3000}, Duty: []int{40, 40, 40, 40}, Flagged: "fan0"}, true
	}

	var progress atomic.Int64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	rng := func(s int64) *rand.Rand { return rand.New(rand.NewSource(seed + s)) }
	actor := func(name string, n int, f func(r *rand.Rand, i int)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := rng(int64(len(name)*7919) + int64(name[0]))
			for i := 0; i < n; i++ {
				select {
				case <-stop:
					return
				default:
				}
				f(r, i)
				progress.Add(1)
				time.Sleep(time.Duration(r.Intn(3000)) * time.Microsecond)
			}
		}()
	}

	// deadlock watchdog
	dead := make(chan string, 1)
	go func() {
		last, at := progress.Load(), time.Now()
		for {
			select {
			case <-stop:
				return
			case <-time.After(200 * time.Millisecond):
			}
			if p := progress.Load(); p != last {
				last, at = p, time.Now()
				continue
			}
			if time.Since(at) > 20*time.Second {
				buf := make([]byte, 4<<20)
				dead <- string(buf[:runtime.Stack(buf, true)])
				return
			}
		}
	}()

	// the fan loop (one goroutine, as fanLoop): curve, throttle, abort tier, heat escalation → thermalIdle
	actor("fanTick", iters*3, func(r *rand.Rand, i int) {
		temp.mu.Lock()
		temp.t = 60 + r.Float64()*40
		temp.blind = r.Intn(40) == 0
		temp.mu.Unlock()
		fanMu.RLock()
		c := fanCfg
		fanMu.RUnlock()
		fanTick(c)
		// the no-temperature rule's Idle (fanTick waits 3 real minutes for it): from this same goroutine, as fanLoop
		if r.Intn(12) == 0 && !minerOnIdlePlan() {
			thermalIdle("no board temperature while hashing")
		}
	})
	// Controls → Clock / voltage (manual plans, stock levels, release)
	actor("handleControl", iters, func(r *rand.Rand, i int) {
		done := b.owner()
		defer done()
		switch r.Intn(5) {
		case 0:
			if out := rcPost(handleControl, map[string]any{"release": true}); out["ok"] == true {
				// "Take manual control": the owner takes the miner over, the thermal guard's Idle included (and the
				// schedule is paused until the owner resumes it)
				b.mu.Lock()
				if b.idle != nil && b.idle.kind == "heat" {
					b.idle.kind = "user"
				}
				b.takeover = true // (a stop whose Idle is still on its way: the owner has taken that over too)
				b.mu.Unlock()
			}
		case 1:
			rcPost(handleControl, map[string]any{"preset": []int{0, 2}[r.Intn(2)], "confirm": true})
		default:
			pv := 9100 + 50*r.Intn(11)
			rcPost(handleControl, map[string]any{"confirm": true, "freq_mhz": 500 + 25*r.Intn(7), "vrun_mv": pv - 300, "pv_mv": pv,
				"fan1": []int{0, 10, 30, 60}[r.Intn(4)], "fan2": []int{0, 10, 30, 60}[r.Intn(4)]})
		}
	})
	// Presets: apply, Idle button
	actor("presets", iters, func(r *rand.Rand, i int) {
		done := b.owner()
		defer done()
		if r.Intn(4) == 0 {
			rcPost(handlePresets, map[string]any{"action": "idle"})
		} else {
			rcPost(handlePresets, map[string]any{"action": "apply", "name": []string{"Fast", "Slow", "Top"}[r.Intn(3)]})
		}
	})
	// the schedule: a rule in force all week whose preset changes every tick
	actor("schedule", iters, func(r *rand.Rand, i int) {
		tgt := []string{"Fast", "Slow", "Stock", "Idle", schedBack}[r.Intn(5)]
		var rules []schedRule
		rules = append(rules, schedRule{Start: "00:00", Days: []int{0, 1, 2, 3, 4, 5, 6}, Preset: tgt})
		saveSchedule(schedule{Enabled: true, Rules: rules})
		schedRunMu.Lock()
		lastSchKey = -1
		if r.Intn(2) == 0 {
			lastSchName = ""
		}
		schedRunMu.Unlock()
		scheduleTick()
		rcAct(fmt.Sprintf("g%d schedule tick target %s -> charge %q heatHeld %v", gid(), tgt, getCharge(), heatIdleHeld()))
		if r.Intn(10) == 0 { // the owner resumes a paused schedule now and then
			s := loadSchedule()
			s.Paused = nil
			saveSchedule(s)
			rcAct("      schedule resumed")
		}
	})
	// tuning runs: start (owner), stop (owner) — the engine's end puts the miner back
	actor("tuner", iters/8, func(r *rand.Rand, i int) {
		e, done := rcTunerStart(b)
		if e == nil {
			time.Sleep(2 * time.Millisecond)
			return
		}
		d := time.Duration(r.Intn(30)) * time.Millisecond
		go func() { time.Sleep(d); e.Stop() }()
		rcAct(fmt.Sprintf("g%d tuner started prev=%q", gid(), e.prevCharge))
		e.Run()
		tunerEnded(e)
		rcAct(fmt.Sprintf("g%d tuner ended -> charge %q", gid(), getCharge()))
		done()
	})
	// limit probe
	actor("ceiling", iters/8, func(r *rand.Rand, i int) {
		if !rcCeilingStart() {
			time.Sleep(2 * time.Millisecond)
			return
		}
		time.Sleep(time.Duration(r.Intn(40)) * time.Millisecond)
		if r.Intn(2) == 0 {
			ceilAbort.Store(true)
		}
		for j := 0; j < 2000; j++ {
			ceilMu.Lock()
			run := ceilSt.Running
			ceilMu.Unlock()
			if !run {
				break
			}
			time.Sleep(time.Millisecond)
		}
	})
	// fan lock: engage (safety) then, later, the owner unlocks
	actor("fanlock", iters/3, func(r *rand.Rand, i int) {
		time.Sleep(time.Duration(r.Intn(30)) * time.Millisecond)
		if _, locked := loadFanLock(); !locked {
			if r.Intn(6) == 0 {
				fanLockEngage(watchdogView{Flagged: "the watchdog flags fan0", Status: 1}, time.Now())
			}
		} else {
			time.Sleep(time.Duration(r.Intn(10)) * time.Millisecond)
			b.mu.Lock()
			b.holdLanded["lock"] = false
			b.mu.Unlock()
			fanUnlock(true, time.Now())
		}
	})
	// pool guard: hold (safety) and the owner's Resume
	actor("poolguard", iters/3, func(r *rand.Rand, i int) {
		time.Sleep(time.Duration(r.Intn(30)) * time.Millisecond)
		if _, held := poolHold(); !held {
			if r.Intn(6) == 0 {
				rcPoolGuard()
			}
		} else {
			time.Sleep(time.Duration(r.Intn(10)) * time.Millisecond)
			b.mu.Lock()
			b.holdLanded["pool"] = false
			b.mu.Unlock()
			rcPost(handlePoolGuard, map[string]any{"resume": true})
		}
	})
	// fan guard floor: raised (the watchdog flags a fan at the level written) and cleared (fan settings saved)
	actor("fanguard", iters, func(r *rand.Rand, i int) {
		switch r.Intn(6) {
		case 0:
			wdFlag.Store(true)
			fanGuardCheck(40)
			wdFlag.Store(false)
		case 1:
			fanGuardClear()
		}
		time.Sleep(time.Millisecond)
	})
	// fan pair loop
	os.WriteFile(filepath.Join(filepath.Dir(*authFile), "fan-pair.json"), []byte(`{"offset":-10}`), 0644)
	actor("fanpair", iters*2, func(r *rand.Rand, i int) {
		fanPairRun()
		time.Sleep(time.Millisecond)
	})

	waitAll := make(chan struct{})
	go func() { wg.Wait(); close(waitAll) }()
	select {
	case <-waitAll:
	case d := <-dead:
		close(stop)
		os.WriteFile(filepath.Join(os.TempDir(), "r5races-goroutines.txt"), []byte(d), 0644)
		t.Fatalf("DEADLOCK: no progress for 20 s; goroutines:\n%s", d[:min(len(d), 60000)])
	}
	close(stop)

	// settle: no more latency or failures; let a probe or run that is still going finish
	b.off.Store(true)
	for i := 0; i < 16000; i++ {
		ceilMu.Lock()
		run := ceilSt.Running
		ceilMu.Unlock()
		tunerMu.Lock()
		tr := tunerCur != nil && tunerCur.isRunning()
		tunerMu.Unlock()
		if !run && !tr {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	cur, idle := b.state()
	ch := getCharge()
	pp, _ := resolveRunningPlan(cur)
	switch {
	case ch == "idle" && !idle:
		b.bad("charge mismatch", fmt.Sprintf("charge %q but the miner runs %v", ch, pp))
	case (ch == "stock" || strings.HasPrefix(ch, "preset:")) && idle:
		b.bad("charge mismatch", fmt.Sprintf("charge %q but the miner is on Idle", ch))
	case ch == "tuner" || ch == "ceiling":
		b.bad("charge mismatch", fmt.Sprintf("charge %q with no run going", ch))
	}
	if _, locked := loadFanLock(); locked && !idle {
		b.bad("settled: fan lock but hashing", fmt.Sprint(pp))
	}
	if _, held := poolHold(); held && !idle {
		b.bad("settled: pool hold but hashing", fmt.Sprint(pp))
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	t.Logf("PUTs landed: %d; wakes by writer: %v", b.puts, b.wakes)
	rcStatMu.Lock()
	for k, v := range rcStats {
		t.Logf("  %5d %s", v, k)
	}
	rcStats = map[string]int{}
	rcStatMu.Unlock()
	for _, tr := range b.traces {
		t.Log(tr)
	}
	if len(b.viol) > 0 {
		for k, n := range b.viol {
			t.Errorf("%-40s x%d  e.g. %s", k, n, b.example[k])
		}
	}
}
