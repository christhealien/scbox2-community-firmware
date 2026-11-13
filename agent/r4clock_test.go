package main

// Round-4 review of the Controls tab's "Clock / voltage" panel (handleControl, mutatePlan, applyStockPlan).

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

type r4Stock struct {
	mu      sync.Mutex
	setting map[string]any
	puts    []map[string]any
}

const r4Base = `{"manual":false,"tempcontrol":true,"select":0,"manualPowerplan":"700 MHz 9300 V 80 RPM 80 RPM PV 9600",
 "powerplans":[{"info":"615 MHz 9100 V 40 RPM 40 RPM PV 9400","level":0},{"info":"0 MHz 0 V 100 RPM 100 RPM","level":3}]}`

func r4Setup(t *testing.T, js string) *r4Stock {
	t.Helper()
	*authFile = filepath.Join(t.TempDir(), "auth.json")
	oldWake, oldFans := wakeWait, fanSysDir
	wakeWait = 0
	fanSysDir = t.TempDir()
	setCharge("")
	fanGuardMu.Lock()
	fanGuardSt, fanGuardOK = fanGuardState{}, true
	fanGuardMu.Unlock()
	hgMu.Lock()
	hgHot, hgAt = 0, time.Time{}
	hgMu.Unlock()
	st := &r4Stock{}
	json.Unmarshal([]byte(js), &st.setting)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st.mu.Lock()
		defer st.mu.Unlock()
		switch {
		case strings.HasPrefix(r.URL.Path, "/user/login"):
			json.NewEncoder(w).Encode(map[string]any{"JWT Token": "tok"})
		case r.Method == "GET":
			json.NewEncoder(w).Encode(st.setting)
		case r.Method == "PUT":
			var m map[string]any
			b, _ := io.ReadAll(r.Body)
			json.Unmarshal(b, &m)
			st.puts = append(st.puts, m)
			st.setting = m
			io.WriteString(w, `{"status":0}`)
		}
	}))
	oldWeb := *minerWeb
	*minerWeb = strings.TrimPrefix(srv.URL, "http://")
	jwtToken = ""
	authMu.Lock()
	auth.MinerPass = "pw"
	authMu.Unlock()
	t.Cleanup(func() {
		srv.Close()
		*minerWeb, wakeWait, fanSysDir = oldWeb, oldWake, oldFans
		setCharge("")
		fanGuardMu.Lock()
		fanGuardSt, fanGuardOK = fanGuardState{}, false
		fanGuardMu.Unlock()
		hgMu.Lock()
		hgHot, hgAt = 0, time.Time{}
		hgMu.Unlock()
	})
	return st
}

func (s *r4Stock) running() powerplan {
	s.mu.Lock()
	defer s.mu.Unlock()
	pp, _ := resolveRunningPlan(s.setting)
	return pp
}

func (s *r4Stock) nPuts() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.puts) }

func r4Call(method, body string) map[string]any {
	rr := httptest.NewRecorder()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	handleControl(rr, httptest.NewRequest(method, "/api/control", rd))
	var res map[string]any
	json.Unmarshal(rr.Body.Bytes(), &res)
	return res
}

// F1: on a firmware level (manual off) the panel shows the stale manualPowerplan instead of what runs.
func TestR4GetShowsRunningPlan(t *testing.T) {
	st := r4Setup(t, r4Base)
	res := r4Call("GET", "")
	b, _ := json.Marshal(res["current"])
	var cur powerplan
	json.Unmarshal(b, &cur)
	if want := st.running(); cur != want {
		t.Fatalf("Controls shows %+v, the miner runs %+v (firmware level 0, manual off)", cur, want)
	}
}

// F2: a request that is refused anyway must not take the miner out of Idle first.
func TestR4RefusedPlanDoesNotWakeIdle(t *testing.T) {
	for name, body := range map[string]string{
		"pv over 9600":      `{"freq_mhz":625,"vrun_mv":9300,"pv_mv":9700,"fan1":50,"fan2":50,"confirm":true}`,
		"clock over 700":    `{"freq_mhz":750,"vrun_mv":9100,"pv_mv":9400,"fan1":50,"fan2":50,"confirm":true}`,
		"override unlocked": `{"freq_mhz":750,"vrun_mv":9100,"pv_mv":9400,"fan1":50,"fan2":50,"confirm":true,"override":true}`,
		"pv under run":      `{"freq_mhz":625,"vrun_mv":9300,"pv_mv":9200,"fan1":50,"fan2":50,"confirm":true}`,
	} {
		t.Run(name, func(t *testing.T) {
			st := r4Setup(t, strings.Replace(r4Base, `"select":0`, `"select":1`, 1)) // on the idle level
			res := r4Call("POST", body)
			if res["ok"] != false {
				t.Fatalf("accepted: %v", res)
			}
			if n := st.nPuts(); n != 0 {
				t.Fatalf("refused (%v) but the miner was woken from Idle first: %d PUT(s), now runs %+v", res["err"], n, st.running())
			}
		})
	}
}

// F3: "Take manual control" while the limit probe holds charge drops the probe's lock while it keeps stepping
// the clock: Controls, the schedule and a tuning run can then start under it.
func TestR4ReleaseDuringLimitProbe(t *testing.T) {
	r4Setup(t, r4Base)
	setCharge("ceiling")
	res := r4Call("POST", `{"release":true}`)
	if res["ok"] != false || getCharge() != "ceiling" {
		t.Fatalf("release during the limit probe accepted (%v); charge now %q: tuner/schedule/Controls no longer see the probe", res, getCharge())
	}
}

// F4: Controls' stock-preset buttons select a firmware level as-is: its fans (40 %) go under a raised fan floor,
// and down while the heat guard is on. Every other path (mutatePlan, the tuner's restore) keeps them up.
func TestR4StockPresetKeepsFanFloor(t *testing.T) {
	st := r4Setup(t, strings.Replace(r4Base, `"manual":false`, `"manual":true`, 1))
	fanGuardMu.Lock()
	fanGuardSt = fanGuardState{Floor: 60}
	fanGuardMu.Unlock()
	if res := r4Call("POST", `{"preset":0,"confirm":true}`); res["ok"] != true {
		t.Fatalf("preset refused: %v", res)
	}
	if pp := st.running(); pp.FreqMHz > 0 && min(pp.Fan1, pp.Fan2) < 60 {
		t.Fatalf("stock preset from Controls runs fans %d/%d under the floor 60", pp.Fan1, pp.Fan2)
	}
}

func TestR4StockPresetHeatGuard(t *testing.T) {
	st := r4Setup(t, strings.Replace(r4Base, `"manual":false`, `"manual":true`, 1))
	for i := 0; i < 4; i++ {
		os.MkdirAll(filepath.Join(fanSysDir, "fan"+string(rune('0'+i))), 0755)
		os.WriteFile(filepath.Join(fanSysDir, "fan"+string(rune('0'+i)), "lvl"), []byte("100"), 0644)
	}
	noteHottest(89)
	if res := r4Call("POST", `{"preset":0,"confirm":true}`); res["ok"] != true {
		t.Fatalf("preset refused: %v", res)
	}
	if pp := st.running(); pp.FreqMHz > 0 && min(pp.Fan1, pp.Fan2) < 100 {
		t.Fatalf("stock preset from Controls at 89 °C pulls the fans from 100 to %d/%d", pp.Fan1, pp.Fan2)
	}
}

// applyStockPlan (the schedule's Stock): the heat guard is handled, the raised fan floor isn't.
func TestR4ApplyStockPlanFanFloor(t *testing.T) {
	st := r4Setup(t, strings.Replace(r4Base, `"manual":false`, `"manual":true`, 1))
	fanGuardMu.Lock()
	fanGuardSt = fanGuardState{Floor: 60}
	fanGuardMu.Unlock()
	if err := applyStockPlan(); err != nil {
		t.Fatal(err)
	}
	if pp := st.running(); min(pp.Fan1, pp.Fan2) < 60 {
		t.Fatalf("applyStockPlan runs fans %d/%d under the floor 60", pp.Fan1, pp.Fan2)
	}
}

// ---- checks that pass today (limits and clamps) ----

func TestR4Limits(t *testing.T) {
	type c struct {
		f, v, pv, f1 int
		over, ok     bool
	}
	cases := []c{
		{300, 8800, 9100, 10, false, true}, {700, 9300, 9600, 100, false, true},
		{299, 9100, 9400, 50, false, false}, {701, 9100, 9400, 50, false, false},
		{625, 8799, 9400, 50, false, false}, {625, 9301, 9600, 50, false, false},
		{625, 9100, 9099, 50, false, false}, {625, 9100, 9601, 50, false, false},
		{625, 9100, 9400, 9, false, false}, {625, 9100, 9400, 101, false, false},
		{625, 9300, 9200, 50, false, false},
		{200, 8800, 9100, 50, true, true}, {900, 9300, 9600, 50, true, true},
		{199, 9100, 9400, 50, true, false}, {901, 9100, 9400, 50, true, false},
		{625, 9100, 9601, 50, true, false}, {625, 9301, 9600, 50, true, false}, {625, 8799, 9400, 50, true, false},
	}
	for _, x := range cases {
		err := validateControlEx(powerplan{x.f, x.v, x.pv, x.f1, x.f1}, x.over)
		if (err == nil) != x.ok {
			t.Errorf("%+v over=%v: err=%v", x, x.over, err)
		}
	}
}

func TestR4HandleControlAppliesFloorAndClamps(t *testing.T) {
	st := r4Setup(t, strings.Replace(r4Base, `"manual":false`, `"manual":true`, 1))
	fanGuardMu.Lock()
	fanGuardSt = fanGuardState{Floor: 40}
	fanGuardMu.Unlock()
	res := r4Call("POST", `{"freq_mhz":650,"vrun_mv":9200,"pv_mv":9500,"fan1":20,"fan2":30,"confirm":true}`)
	if res["ok"] != true || st.running() != (powerplan{650, 9200, 9500, 40, 40}) {
		t.Fatalf("%v → %+v", res, st.running())
	}
	// unlocked: 900 MHz allowed, PV still capped at 9600
	saveOverclock(overclockState{Acknowledged: true})
	if res := r4Call("POST", `{"freq_mhz":900,"vrun_mv":9300,"pv_mv":9600,"fan1":50,"fan2":50,"confirm":true,"override":true}`); res["ok"] != true {
		t.Fatalf("unlocked 900: %v", res)
	}
	if res := r4Call("POST", `{"freq_mhz":900,"vrun_mv":9300,"pv_mv":9700,"fan1":50,"fan2":50,"confirm":true,"override":true}`); res["ok"] != false {
		t.Fatalf("unlocked PV 9700 accepted: %v", res)
	}
	// without override, unlocked still uses the safe window
	if res := r4Call("POST", `{"freq_mhz":800,"vrun_mv":9300,"pv_mv":9600,"fan1":50,"fan2":50,"confirm":true}`); res["ok"] != false {
		t.Fatalf("800 without override accepted: %v", res)
	}
	// mutatePlan clamps
	for _, x := range []struct{ in, want powerplan }{
		{powerplan{650, 9400, 9700, 50, 50}, powerplan{650, 9300, 9600, 50, 50}},
		{powerplan{650, 8700, 9000, 50, 50}, powerplan{650, 8800, 9100, 50, 50}},
		{powerplan{650, 9300, 9200, 50, 50}, powerplan{650, 9200, 9200, 50, 50}},
	} {
		if _, err := mutatePlan(func(p *powerplan) { *p = x.in }); err != nil {
			t.Fatal(err)
		}
		if got := st.running(); got != x.want {
			t.Errorf("mutatePlan(%+v) wrote %+v", x.in, got)
		}
	}
	// locked: presets/tuner/idle/stock in charge, the strong test running
	for _, ch := range []string{"tuner", "idle", "stock", "preset:x", "ceiling"} {
		setCharge(ch)
		if res := r4Call("POST", `{"freq_mhz":600,"vrun_mv":9100,"pv_mv":9400,"fan1":50,"fan2":50,"confirm":true}`); res["ok"] != false || res["locked"] != true {
			t.Errorf("charge %q: %v", ch, res)
		}
	}
	setCharge("tuner")
	if res := r4Call("POST", `{"release":true}`); res["ok"] != false {
		t.Errorf("release during tuner: %v", res)
	}
	setCharge("")
}

// F7: a probe stopped early (Stop, heat, a hold) left its axis "probing" and the page said "No firmware clamp
// found in range — the real limit here is thermal/stability", a verdict it never reached.
func TestR4ProbeStoppedEarlyIsNotAVerdict(t *testing.T) {
	r4Setup(t, strings.Replace(r4Base, `"manual":false`, `"manual":true`, 1))
	setCharge("ceiling")
	ceilMu.Lock()
	ceilSt = ceilingState{Running: true, Axes: []ceilingAxis{{Name: "freq", Label: "Frequency", Unit: "MHz", SafeMax: 700, HardMax: 900, Step: 25, State: "probing", At: 725}}}
	ceilMu.Unlock()
	ceilAbort.Store(true)
	defer ceilAbort.Store(false)
	runCeilingProbe(powerplan{700, 9300, 9600, 80, 80})
	s := ceilSnapshot()
	if s.Running || s.Axes[0].State == "probing" || s.Axes[0].State == "done" {
		t.Fatalf("stopped probe reads as %q (running=%v): the page shows it as still probing / a finished verdict", s.Axes[0].State, s.Running)
	}
	if getCharge() != "" {
		t.Fatalf("charge left %q", getCharge())
	}
}

// F5 (server side of the fix): the page sends no fans; the apply keeps the ones running now, not the page's
// copy loaded minutes earlier (the fan curve moves them meanwhile).
func TestR4ApplyKeepsRunningFans(t *testing.T) {
	st := r4Setup(t, strings.Replace(strings.Replace(r4Base, `"manual":false`, `"manual":true`, 1), "80 RPM 80 RPM", "85 RPM 85 RPM", 1))
	res := r4Call("POST", `{"freq_mhz":613,"vrun_mv":9100,"pv_mv":9400,"confirm":true}`)
	if res["ok"] != true || st.running() != (powerplan{613, 9100, 9400, 85, 85}) {
		t.Fatalf("%v → %+v", res, st.running())
	}
}
