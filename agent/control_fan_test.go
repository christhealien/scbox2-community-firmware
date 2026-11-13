package main

import (
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func approxF(a, b float64) bool { return math.Abs(a-b) < 0.01 }

func TestParsePowerplanRoundTrip(t *testing.T) {
	cases := map[string]powerplan{
		"625 MHz 9100 V 50 RPM 50 RPM PV 9400": {625, 9100, 9400, 50, 50},
		"615 MHz 9100 V 40 RPM 40 RPM PV 9400": {615, 9100, 9400, 40, 40},
		"0 MHz 0 V 100 RPM 100 RPM":            {0, 0, 0, 100, 100},
	}
	for s, want := range cases {
		p, ok := parsePowerplan(s)
		if !ok || p != want {
			t.Fatalf("parse %q = %+v,%v want %+v", s, p, ok, want)
		}
	}
	if buildPowerplan(powerplan{625, 9100, 9400, 50, 50}) != "625 MHz 9100 V 50 RPM 50 RPM PV 9400" {
		t.Fatal("build mismatch")
	}
}

func TestValidateControlLimits(t *testing.T) {
	if validateControl(powerplan{625, 9100, 9400, 50, 50}) != nil {
		t.Fatal("valid rejected")
	}
	bad := []powerplan{{800, 9100, 9400, 50, 50}, {625, 8000, 9400, 50, 50}, {625, 9100, 9800, 50, 50}, {625, 9100, 9400, 5, 50}, {625, 9400, 9200, 50, 50}}
	for i, p := range bad {
		if validateControl(p) == nil {
			t.Fatalf("[%d] bad %+v accepted", i, p)
		}
	}
}

func TestCurveAndFanConfig(t *testing.T) {
	pts := builtinCurves["curve-balanced"]
	for _, c := range []struct{ temp, want float64 }{{40, 40}, {60, 50}, {76, 90}, {90, 100}} {
		if g := curveFan(pts, c.temp); !approxF(g, c.want) {
			t.Fatalf("curveFan(%.0f)=%.2f want %.2f", c.temp, g, c.want)
		}
	}
	if validateFanConfig(defaultFanConfig()) != nil {
		t.Fatal("default fan config rejected")
	}
	bad := defaultFanConfig()
	bad.Profile = "custom"
	bad.Points = []curvePoint{{50, 60}, {70, 40}}
	if validateFanConfig(bad) == nil {
		t.Fatal("decreasing-fan curve accepted")
	}
}

func mockStock(t *testing.T, put *map[string]any) *httptest.Server {
	setting := map[string]any{"manual": true, "tempcontrol": true, "select": float64(0),
		"manualPowerplan": "625 MHz 9100 V 50 RPM 50 RPM PV 9400",
		"powerplans":      []any{map[string]any{"info": "615 MHz 9100 V 40 RPM 40 RPM PV 9400", "level": float64(0)}}}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/user/login"):
			json.NewEncoder(w).Encode(map[string]any{"JWT Token": "tok"})
		case r.Method == "GET":
			json.NewEncoder(w).Encode(setting)
		case r.Method == "PUT":
			b, _ := io.ReadAll(r.Body)
			json.Unmarshal(b, put)
			io.WriteString(w, `{"status":0}`)
		}
	}))
}

func TestControlApplyPreservesTempcontrol(t *testing.T) {
	var put map[string]any
	srv := mockStock(t, &put)
	defer srv.Close()
	*minerWeb = strings.TrimPrefix(srv.URL, "http://")
	jwtToken = ""
	authMu.Lock()
	auth.MinerPass = "pw"
	authMu.Unlock()

	rr := httptest.NewRecorder()
	handleControl(rr, httptest.NewRequest("POST", "/api/control",
		strings.NewReader(`{"freq_mhz":600,"vrun_mv":9000,"pv_mv":9300,"fan1":55,"fan2":55,"confirm":true}`)))
	var res map[string]any
	json.Unmarshal(rr.Body.Bytes(), &res)
	if res["ok"] != true {
		t.Fatalf("control POST failed: %s", rr.Body.String())
	}
	if put["manualPowerplan"] != "600 MHz 9000 V 55 RPM 55 RPM PV 9300" || put["tempcontrol"] != true {
		t.Fatalf("PUT wrong: %+v", put)
	}
	// unconfirmed rejected
	rr = httptest.NewRecorder()
	handleControl(rr, httptest.NewRequest("POST", "/api/control", strings.NewReader(`{"freq_mhz":600,"vrun_mv":9000,"pv_mv":9300,"fan1":55,"fan2":55}`)))
	json.Unmarshal(rr.Body.Bytes(), &res)
	if res["ok"] != false {
		t.Fatal("unconfirmed accepted")
	}
}

func TestPresetFanCurve(t *testing.T) {
	// typical: hold 70°C at 55% fan, abort 88
	pts := presetFanCurve(70, 55, 88, 30)
	if len(pts) < 2 {
		t.Fatalf("want >=2 points, got %d", len(pts))
	}
	// monotonic non-decreasing in both temp and fan; fans within [30,100]
	for i, p := range pts {
		if p.Fan < 30 || p.Fan > 100 {
			t.Errorf("point %d fan %.0f out of [30,100]", i, p.Fan)
		}
		if i > 0 {
			if pts[i].Temp <= pts[i-1].Temp {
				t.Errorf("temp not increasing at %d: %.0f then %.0f", i, pts[i-1].Temp, pts[i].Temp)
			}
			if pts[i].Fan < pts[i-1].Fan {
				t.Errorf("fan drops at %d: %.0f then %.0f", i, pts[i-1].Fan, pts[i].Fan)
			}
		}
	}
	// the target point carries the held fan, and it reaches 100% below abort
	last := pts[len(pts)-1]
	if last.Fan != 100 {
		t.Errorf("curve should reach 100%%, got %.0f", last.Fan)
	}
	if last.Temp > 88 {
		t.Errorf("100%% point should be below abort, got %.0f", last.Temp)
	}
	// clamps: held below fanMin is raised; a hot low target still yields a valid rising curve
	p2 := presetFanCurve(32, 10, 90, 30)
	for i := 1; i < len(p2); i++ {
		if p2[i].Temp <= p2[i-1].Temp || p2[i].Fan < p2[i-1].Fan {
			t.Errorf("degenerate curve not monotonic: %+v", p2)
		}
	}
}

func TestPresetFanCurveValidates(t *testing.T) {
	// the derived curve must pass the fan-config validator as a custom profile
	pts := presetFanCurve(72, 60, 88, 30)
	c := defaultFanConfig()
	c.Profile = "custom"
	c.Points = pts
	if err := validateFanConfig(c); err != nil {
		t.Fatalf("derived curve rejected by validator: %v (points %+v)", err, pts)
	}
}

// While the miner is idle, auto fan must not write a plan (a manual plan with fans would wake it), and
// Idle uses the fan speed set for it.
func TestIdleFans(t *testing.T) {
	*authFile = filepath.Join(t.TempDir(), "auth.json")
	var puts []map[string]any
	setting := map[string]any{"manual": false, "tempcontrol": true, "select": float64(1),
		"manualPowerplan": "625 MHz 9100 V 50 RPM 50 RPM PV 9400",
		"powerplans": []any{map[string]any{"info": "615 MHz 9100 V 40 RPM 40 RPM PV 9400", "level": float64(0)},
			map[string]any{"info": "0 MHz 0 V 100 RPM 100 RPM", "level": float64(3)}}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/user/login"):
			json.NewEncoder(w).Encode(map[string]any{"JWT Token": "tok"})
		case r.Method == "GET":
			json.NewEncoder(w).Encode(setting)
		case r.Method == "PUT":
			var m map[string]any
			b, _ := io.ReadAll(r.Body)
			json.Unmarshal(b, &m)
			puts = append(puts, m)
			io.WriteString(w, `{"status":0}`)
		}
	}))
	defer srv.Close()
	*minerWeb = strings.TrimPrefix(srv.URL, "http://")
	jwtToken = ""
	authMu.Lock()
	auth.MinerPass = "pw"
	authMu.Unlock()

	if idle, err := writeFansUnlessIdle(60); !idle || err != nil || len(puts) != 0 {
		t.Fatalf("idle: fans written anyway (idle=%v err=%v puts=%d)", idle, err, len(puts))
	}
	setting["select"], setting["manual"] = float64(0), true
	if idle, err := writeFansUnlessIdle(60); idle || err != nil || len(puts) != 1 || puts[0]["manualPowerplan"] != "625 MHz 9100 V 60 RPM 60 RPM PV 9400" {
		t.Fatalf("hashing: fans not written: %v %v %+v", idle, err, puts)
	}
	// Idle leaves the idle plan exactly as the firmware has it
	if err := applyIdle(); err != nil {
		t.Fatal(err)
	}
	last := puts[len(puts)-1]
	plans := last["powerplans"].([]any)
	if last["select"] != float64(1) || last["manual"] != false || plans[1].(map[string]any)["info"] != "0 MHz 0 V 100 RPM 100 RPM" {
		t.Fatalf("idle PUT: %+v", last)
	}
}

// From idle, Stock and presets must wake the miner the way the firmware does it: back to its own hashing
// plan (manual off). A manual plan alone is ignored while the boards are powered off.
func TestWakeFromIdle(t *testing.T) {
	*authFile = filepath.Join(t.TempDir(), "auth.json")
	wakeWait = 0
	defer func() { wakeWait = 20 * time.Second }()
	var puts []map[string]any
	setting := map[string]any{}
	reset := func() {
		json.Unmarshal([]byte(`{"manual":false,"tempcontrol":true,"select":1,"manualPowerplan":"615 MHz 9100 V 40 RPM 40 RPM PV 9400",
			"powerplans":[{"info":"615 MHz 9100 V 40 RPM 40 RPM PV 9400","level":0},{"info":"0 MHz 0 V 100 RPM 100 RPM","level":3}]}`), &setting)
		puts = nil
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/user/login"):
			json.NewEncoder(w).Encode(map[string]any{"JWT Token": "tok"})
		case r.Method == "GET":
			json.NewEncoder(w).Encode(setting)
		case r.Method == "PUT":
			var m map[string]any
			b, _ := io.ReadAll(r.Body)
			json.Unmarshal(b, &m)
			puts = append(puts, m)
			setting = m
			io.WriteString(w, `{"status":0}`)
		}
	}))
	defer srv.Close()
	*minerWeb = strings.TrimPrefix(srv.URL, "http://")
	jwtToken = ""
	authMu.Lock()
	auth.MinerPass = "pw"
	authMu.Unlock()

	reset()
	if err := applyStockPlan(); err != nil {
		t.Fatal(err)
	}
	if len(puts) != 1 || puts[0]["manual"] != false || puts[0]["select"] != float64(0) {
		t.Fatalf("Stock from idle: %+v", puts)
	}
	reset()
	if _, err := mutatePlan(func(p *powerplan) { p.FreqMHz, p.VrunMv, p.PVMv = 650, 9200, 9500 }); err != nil {
		t.Fatal(err)
	}
	if len(puts) != 2 || puts[0]["manual"] != false || puts[0]["select"] != float64(0) ||
		puts[1]["manual"] != true || !strings.HasPrefix(puts[1]["manualPowerplan"].(string), "650 MHz 9200 V") {
		t.Fatalf("preset from idle: wake first, then the plan: %+v", puts)
	}
	// not idle: no wake step
	setting["select"], setting["manual"] = float64(0), true
	puts = nil
	if _, err := mutatePlan(func(p *powerplan) { p.FreqMHz = 600 }); err != nil || len(puts) != 1 {
		t.Fatalf("hashing: one PUT: %v %+v", err, puts)
	}
}
