package main

// Round 5 (r5-safety): adversarial checks on everything that ends in a plan or fan write.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// r5Stock: a fake Goldshell backend (stock 615 MHz plan at 0, idle plan at 1) that keeps what's PUT.
type r5Stock struct {
	mu      sync.Mutex
	setting map[string]any
	puts    []map[string]any
}

func (s *r5Stock) plans() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, p := range s.puts {
		m, _ := p["manual"].(bool)
		sel, _ := p["select"].(float64)
		mp, _ := p["manualPowerplan"].(string)
		if m {
			out = append(out, "manual "+mp)
		} else {
			out = append(out, map[bool]string{true: "select idle", false: "select hashing"}[sel == 1])
		}
	}
	return out
}

func (s *r5Stock) idle() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return minerIdle(s.setting)
}

func r5Setup(t *testing.T, idle bool) *r5Stock {
	t.Helper()
	dir := t.TempDir()
	*authFile = filepath.Join(dir, "auth.json")
	*presetPath = filepath.Join(dir, "presets.json")
	*fanPath = filepath.Join(dir, "fan.json")
	wakeWait = 0
	t.Cleanup(func() { wakeWait = 20 * time.Second; setCharge(""); fanTestAt.Store(0) }) // (fanRerunRecover marks the fan test as running)
	fanGuardMu.Lock()
	fanGuardSt, fanGuardOK = fanGuardState{}, true
	fanGuardMu.Unlock()
	fanSysDir = filepath.Join(dir, "fans") // no fan files: heat guard / pair code reads fail quietly
	s := &r5Stock{}
	sel := 0
	if idle {
		sel = 1
	}
	json.Unmarshal([]byte(`{"manual":false,"tempcontrol":true,"select":`+map[bool]string{true: "1", false: "0"}[idle]+`,
		"manualPowerplan":"615 MHz 9100 V 40 RPM 40 RPM PV 9400",
		"powerplans":[{"info":"615 MHz 9100 V 40 RPM 40 RPM PV 9400","level":0},{"info":"0 MHz 0 V 100 RPM 100 RPM","level":3}]}`), &s.setting)
	_ = sel
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/user/login"):
			json.NewEncoder(w).Encode(map[string]any{"JWT Token": "tok"})
		case r.Method == "GET":
			s.mu.Lock()
			json.NewEncoder(w).Encode(s.setting)
			s.mu.Unlock()
		case r.Method == "PUT":
			var m map[string]any
			b, _ := io.ReadAll(r.Body)
			json.Unmarshal(b, &m)
			s.mu.Lock()
			s.puts = append(s.puts, m)
			s.setting = m
			s.mu.Unlock()
			io.WriteString(w, `{"status":0}`)
		}
	}))
	t.Cleanup(srv.Close)
	*minerWeb = strings.TrimPrefix(srv.URL, "http://")
	jwtToken = ""
	authMu.Lock()
	auth.MinerPass = "pw"
	authMu.Unlock()
	return s
}

// F1: mutatePlan (every automated plan write) never checks the clock. The tuner's applyPlan, the tuner's
// crash recovery (tuning.running) and the limit probe all go through it. Locked (no unlock accepted), a 2000 MHz
// plan is written to the miner.
func TestR5_MutatePlanTakesAnyClock(t *testing.T) {
	s := r5Setup(t, false)
	if loadOverclock().Acknowledged {
		t.Fatal("setup: unlocked")
	}
	err := applyTunePlan(planMV{MHz: 2000, MV: 9000, PV: 9300})
	for _, p := range s.plans() {
		if strings.Contains(p, "2000 MHz") {
			t.Fatalf("locked to 300–700 MHz, yet the miner was sent %q (err=%v)", p, err)
		}
	}
}

// F1b: a hand-edited tuner-curve.json drives a Rebuild run straight past the window (fake engine: what it applies).
func TestR5_RebuildFromHandEditedCurve(t *testing.T) {
	r5Setup(t, false)
	lastCurveMu.Lock()
	lastCurve = nil
	lastCurveMu.Unlock()
	b, _ := json.Marshal(savedCurve{Curve: []planMV{{MHz: 650, MV: 9000, PV: 9300}, {MHz: 1500, MV: 9000, PV: 9300}}})
	os.WriteFile(tunerCurvePath(), b, 0o644)
	cfg := fastCfg()
	cfg.Mode = "rebuild"
	cfg.BuildPresets = true
	cfg.PresetMin = 1
	if err := validateTunerConfig(cfg); err != nil {
		t.Logf("(config: %v)", err)
	}
	e, sim := newSimEngine(cfg, 0, ckey{B: 2, I: 5})
	e.Run()
	for _, p := range sim.applied {
		if p.MHz > freqMax {
			t.Fatalf("a rebuild from a hand-edited tuner-curve.json applied %d MHz while locked (applied: %v)", p.MHz, sim.applied)
		}
	}
}

// F1c: a hand-edited tuning.running marker: at the next start tunerRecover "puts back" its base, any clock.
func TestR5_TunerRecoverHandEditedMarker(t *testing.T) {
	s := r5Setup(t, false)
	b, _ := json.Marshal(tunerMarker{Started: time.Now().Unix(), Base: planMV{MHz: 1200, MV: 9000, PV: 9300}})
	os.WriteFile(tunerMarkerPath(), b, 0o644)
	tunerRecover(0)
	for _, p := range s.plans() {
		if strings.Contains(p, "1200 MHz") {
			t.Fatalf("tuning.running put back %q while locked", p)
		}
	}
}

// F2: the thermal guard (or the no-temperature rule) puts the miner on Idle while the limit probe runs: the
// probe's end "puts the starting setting back", which wakes the miner. The charge stays "idle", so the
// no-temperature rule (gated on the charge) never fires again while it hashes.
func TestR5_CeilingEndWakesThermalIdle(t *testing.T) {
	s := r5Setup(t, false)
	base := powerplan{FreqMHz: 615, VrunMv: 9100, PVMv: 9400, Fan1: 40, Fan2: 40}
	setCharge("ceiling")
	ceilMu.Lock()
	ceilSt = ceilingState{Running: true}
	ceilMu.Unlock()
	// thermalIdle as fanTick runs it: charge first, the runs told to stop, then Idle
	setCharge("idle")
	ceilAbort.Store(true)
	if err := applyIdle(); err != nil {
		t.Fatal(err)
	}
	heatIdleSince.Store(time.Now().Unix())
	defer heatIdleSince.Store(0)
	runCeilingProbe(base) // the probe goroutine reaching its end
	if !s.idle() {
		t.Fatalf("the probe's restore woke the miner the thermal guard had idled (charge %q, writes %v)", getCharge(), s.plans())
	}
}

// F2b: hashing with no temperature while the charge reads "idle" (as F2 leaves it): the no-temperature rule
// never sends it to Idle.
func TestR5_NoTempRuleTrustsCharge(t *testing.T) {
	s := r5Setup(t, false) // hashing
	setCharge("idle")
	defer func(f func() dashboard) { fanDash = f }(fanDash)
	fanDash = func() dashboard {
		return dashboard{OK: true, GHS: 1100, Boards: []board{{ID: 0, Alive: true, GHS: 1100, Temps: []float64{-150, -150}}}}
	}
	noTempSince, noTempTicks = time.Now().Add(-4*time.Minute), 10
	defer func() { noTempSince, noTempTicks = time.Time{}, 0 }()
	defer heatIdleSince.Store(0)
	fanTick(defaultFanConfig())
	if !s.idle() {
		t.Fatalf("hashing blind for 4 min, charge %q: not put on Idle (writes %v)", getCharge(), s.plans())
	}
}

// F3: the tuner's fan hold writes fans through mutatePlan, which wakes an Idle miner. The thermal guard's Idle
// landing between the run's temperature read and its fan write (StopFor doesn't interrupt holdFan) restarts hashing.
func TestR5_TunerFanHoldWakesIdle(t *testing.T) {
	s := r5Setup(t, false)
	cfg := fastCfg()
	cfg.FanHold, cfg.FanTargetC, cfg.FanMinPct = true, 65, 30
	e := newTunerEngine(cfg)
	e.fanGuard, e.readFan = nil, nil
	e.readTemp = func() (float64, bool) {
		// the thermal guard acts here (fanTick runs in its own goroutine)
		setCharge("idle")
		applyIdle()
		return 80, true
	}
	e.holdFan(65)
	if !s.idle() {
		t.Fatalf("a tuner fan-hold write woke the miner from the thermal guard's Idle: %v", s.plans())
	}
}

// F4: a damaged fan-lock.json (empty after a power cut, or hand-edited) releases the fan lock; the pool guard's
// hold file in the same state still holds.
func TestR5_DamagedFanLockReleases(t *testing.T) {
	r5Setup(t, true)
	os.WriteFile(fanLockPath(), []byte(""), 0o644)
	os.WriteFile(poolHoldPath(), []byte(""), 0o644)
	if poolHoldErr() == nil {
		t.Fatal("setup: damaged pool hold doesn't hold")
	}
	os.Remove(poolHoldPath())
	if fanLockErr() == nil {
		t.Fatal("a damaged fan-lock.json unlocks mining (presets, Stock, Controls, the schedule may wake it)")
	}
}

// F5: the fitted fan-check overlay (bound over /root/product.json at boot when it merely contains "maxrpm" and
// balanced braces) is checked at the panel's start only for parsing: maxrpm 0 passes, and the watchdog's
// limit (maxrpm/125 × duty) is then 0 RPM at every duty: no fan is ever flagged.
func TestR5_FanOverlayLenientValuesPass(t *testing.T) {
	r5Setup(t, false)
	f := filepath.Join(t.TempDir(), "product.json")
	os.WriteFile(f, []byte(`{"fans":[{"maxrpm":0},{"maxrpm":0},{"maxrpm":100},{"maxrpm":124}]}`), 0o644)
	if err := productOverlayOK(f); err == nil {
		t.Fatalf("an overlay with maxrpm 0/100/124 passes the start check (watchdog limit at 100 %%: %d RPM)", fanLimitAt(0, 100))
	}
}

// (the fix keeps the normal case: a probe stopped by its own Stop button still puts the starting setting back)
func TestR5_CeilingStopStillRestores(t *testing.T) {
	s := r5Setup(t, false)
	setCharge("ceiling")
	ceilAbort.Store(true)
	runCeilingProbe(powerplan{FreqMHz: 600, VrunMv: 9000, PVMv: 9300, Fan1: 40, Fan2: 40})
	if ps := s.plans(); len(ps) == 0 || !strings.HasPrefix(ps[0], "manual 600 MHz") || getCharge() != "" {
		t.Fatalf("stopped probe: not restored (%v, charge %q)", ps, getCharge())
	}
}
