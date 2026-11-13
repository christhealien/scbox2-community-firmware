package main

// Round 5: native fuzz targets for everything the Controls & fan tab accepts and the files it persists.
// Run one at a time: go test -run '^$' -fuzz '^FuzzR5FanPost$' -fuzztime 3m .

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"unicode"
)

var r5Seeds = []string{
	``, `{}`, `null`, `[]`, `"x"`, `{"a":`, `{"action":"profile"}`, `{"enabled":"yes"}`,
	`{"poll_s":1e308}`, `{"abort_c":-1e308}`, `{"abort_c":1e308}`, `{"abort_c":NaN}`, `{"abort_c":Infinity}`,
	`{"enabled":true,"profile":"custom","points":[{"temp":20,"fan":10},{"temp":100,"fan":10}],"poll_s":1}`,
	`{"enabled":true,"profile":"curve-60"}`, `{"enabled":true,"profile":"steps-default","throttle_enable":true,"throttle_c":75,"resume_c":60}`,
	`{"profile":"mine","curves":{"mine":{"label":"x","points":[{"temp":20,"fan":100},{"temp":100,"fan":0}]}}}`,
	`{"action":"profile","key":"","label":"<b>\u0000\n","points":[{"temp":50,"fan":40},{"temp":80,"fan":100}],"abort_c":88,"cooldown_s":45,"poll_s":10}`,
	`{"action":"profile","key":"mine","label":"M","points":[{"temp":50,"fan":40},{"temp":50,"fan":100}],"abort_c":88,"cooldown_s":45,"poll_s":10}`,
	`{"action":"profile_delete","key":"mine"}`, `{"action":"profile","action":"x"}`, `{"enabled":true,"enabled":false}`,
	`{"a":[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]}`,
	`{"enabled":true,"profile":"tuned:Evil"}`,
}

func r5Seed(f *testing.F, extra ...string) {
	for _, s := range append(r5Seeds, extra...) {
		f.Add([]byte(s))
	}
}

func r5ErrOK(t *testing.T, m map[string]any) {
	if e, _ := m["err"].(string); strings.Contains(e, "json:") || strings.Contains(e, "Go struct") || strings.Contains(e, "Go value") {
		t.Errorf("raw decoder error shown: %q", e)
	}
}

func r5PostB(h http.HandlerFunc, body []byte) map[string]any {
	rr := httptest.NewRecorder()
	h(rr, httptest.NewRequest("POST", "/x", strings.NewReader(string(body))))
	var m map[string]any
	json.Unmarshal(rr.Body.Bytes(), &m)
	return m
}

// curvePointsOK: what validateCurve demands of points (2–8, 20–100 °C, fan 10–100, distinct, never dropping).
func curvePointsOK(pts []curvePoint) error {
	return validateCurve(fanCurve{Label: "x", Points: pts, AbortC: 90, CooldownS: 45, PollS: 10})
}

func fanCfgSafe(c fanConfig) error {
	if err := validateFanConfig(c); err != nil {
		return err
	}
	for k, fc := range c.Curves {
		if err := validateCurve(fc); err != nil {
			return fmt.Errorf("library curve %q: %v", k, err)
		}
	}
	pts := curvePoints(c)
	if c.Profile != "" && !strings.HasPrefix(c.Profile, "steps") {
		if err := curvePointsOK(pts); err != nil {
			return fmt.Errorf("running points %v: %v", pts, err)
		}
	}
	c0 := c
	c0.Smoothing = 0
	last := -1
	for temp := 0.0; temp <= 120; temp += 0.5 {
		v := targetFan(c0, temp)
		if v == -1 {
			continue
		}
		if v < fanFloor() || v > 100 {
			return fmt.Errorf("target %d %% at %.1f °C", v, temp)
		}
		if v < last {
			return fmt.Errorf("fan drops to %d %% at %.1f °C (from %d)", v, temp, last)
		}
		last = v
	}
	return nil
}

func r5FanStart() fanConfig {
	c := defaultFanConfig()
	c.Profile = "mine"
	c.Points = []curvePoint{{50, 40}, {60, 60}, {70, 80}, {80, 100}}
	c.Curves = map[string]fanCurve{"mine": {Label: "Mine", Points: []curvePoint{{50, 40}, {80, 100}}, AbortC: 88, CooldownS: 45, PollS: 10, Smoothing: 1, ForceTC: true},
		"other": {Label: "Other", Points: []curvePoint{{55, 30}, {85, 100}}, AbortC: 88, CooldownS: 45, PollS: 10, ForceTC: true}}
	return c
}

func deepCopyFan(c fanConfig) fanConfig {
	b, _ := json.Marshal(c)
	var o fanConfig
	json.Unmarshal(b, &o)
	return o
}

// POST /api/fan: every action. Refused → nothing changed (memory and file); accepted → safe config.
func FuzzR5FanPost(f *testing.F) {
	r5Seed(f)
	f.Fuzz(func(t *testing.T, body []byte) {
		r5Env(t)
		start := r5FanStart()
		saveFanConfig(start)
		fanMu.Lock()
		fanCfg = deepCopyFan(start)
		fanMu.Unlock()
		before, _ := os.ReadFile(*fanPath)
		res := r5PostB(handleFan, body)
		r5ErrOK(t, res)
		fanMu.RLock()
		now := deepCopyFan(fanCfg)
		fanMu.RUnlock()
		after, _ := os.ReadFile(*fanPath)
		if res["ok"] != true {
			if !reflect.DeepEqual(now, deepCopyFan(start)) {
				t.Fatalf("refused (%v) but the running fan config changed:\n%+v\n->\n%+v", res["err"], start, now)
			}
			if string(before) != string(after) {
				t.Fatalf("refused (%v) but fan.json changed", res["err"])
			}
			return
		}
		if err := fanCfgSafe(now); err != nil {
			t.Fatalf("accepted an unsafe fan config: %v\n%+v", err, now)
		}
		var disk fanConfig
		if json.Unmarshal(after, &disk) != nil || !reflect.DeepEqual(deepCopyFan(disk), now) {
			t.Fatalf("accepted, but fan.json differs from what runs")
		}
	})
}

// fan.json as found on disk: what loads must be what the API would accept (or the defaults).
func FuzzR5LoadFanConfig(f *testing.F) {
	r5Seed(f, `{"enabled":true,"profile":"curve-balanced","abort_c":0}`, `{"profile":"custom","points":[{"temp":20,"fan":100},{"temp":100,"fan":0}]}`,
		`{"profile":"mine","curves":{"mine":{"label":"x","points":[{"temp":20,"fan":0},{"temp":21,"fan":0}],"abort_c":88,"poll_s":10,"cooldown_s":45}}}`,
		`{"poll_s":0,"cooldown_s":-1,"smoothing":99}`)
	f.Fuzz(func(t *testing.T, body []byte) {
		r5Env(t)
		os.WriteFile(*fanPath, body, 0o644)
		loadFanConfig()
		fanMu.RLock()
		c := fanCfg
		fanMu.RUnlock()
		if err := fanCfgSafe(c); err != nil {
			t.Fatalf("fan.json %q loaded: %v", body, err)
		}
	})
}

// presets.json as found on disk.
func FuzzR5LoadPresets(f *testing.F) {
	f.Add([]byte(`[{"name":"A","mhz":600,"mv":9100,"pv":9400}]`))
	f.Add([]byte(`[{"name":"A","mhz":600,"mv":1e9,"pv":-5,"fan_points":[{"temp":1e308,"fan":-1}],"fan_poll_s":1,"fan_smoothing":50,"fan_abort_c":10}]`))
	f.Add([]byte(`[{"name":"A","mhz":5000,"mv":9300,"pv":9600}]`))
	f.Add([]byte(`[{"name":"A"`))
	f.Fuzz(func(t *testing.T, body []byte) {
		r5Env(t)
		os.WriteFile(*presetPath, body, 0o644)
		for _, p := range loadPresets() {
			if p.MHz > 0 && (p.PV > pvHardMax || p.PV < pvMin || p.MV > p.PV || p.MV < vrunHardMin) {
				t.Fatalf("preset loaded with PV %d / mV %d", p.PV, p.MV)
			}
			for _, q := range p.FanPoints {
				if q.Fan < 0 || q.Fan > 100 {
					t.Fatalf("preset fan point %v", q)
				}
			}
			// whatever applying it would run must be safe
			fanMu.Lock()
			fanCfg = defaultFanConfig()
			fanMu.Unlock()
			applyPresetFan(p)
			fanMu.RLock()
			c := fanCfg
			fanMu.RUnlock()
			if err := fanCfgSafe(c); err != nil {
				t.Fatalf("applying preset %q runs an unsafe fan config: %v", p.Name, err)
			}
		}
	})
}

// POST /api/presets save/delete: what's saved must be safe and well-named.
func FuzzR5PresetsPost(f *testing.F) {
	for _, s := range []string{
		`{"action":"save","name":"A","mhz":600,"mv":9100,"pv":9400}`,
		`{"action":"save","name":"A","mhz":600,"mv":9100,"pv":9400,"fan_profile":"curve-60","fan_points":[{"temp":40,"fan":100},{"temp":95,"fan":0}]}`,
		`{"action":"save","name":"A","mhz":600,"mv":9100,"pv":9700}`, `{"action":"save","name":"a\nb","mhz":600,"mv":9100,"pv":9400,"fan_profile":"none"}`,
		`{"action":"save","name":"A","mhz":"600"}`, `{"action":"delete","name":"A"}`, `{"action":"save","name":"Stock","mhz":600,"mv":9100,"pv":9400}`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		var probe struct {
			Action string `json:"action"`
		}
		json.Unmarshal(body, &probe)
		if probe.Action != "save" && probe.Action != "delete" {
			return // apply/idle/build talk to the miner: covered by the handler tests
		}
		r5Env(t)
		res := r5PostB(handlePresets, body)
		r5ErrOK(t, res)
		for _, p := range loadPresets() {
			if err := validateControl(powerplan{p.MHz, p.MV, p.PV, 50, 50}); err != nil {
				t.Fatalf("saved preset out of range: %v", err)
			}
			if strings.IndexFunc(p.Name, unicode.IsControl) >= 0 || p.Name == "Stock" || p.Name == "Idle" || p.Name == "stock" || p.Name == "idle" || p.Name == schedBack {
				t.Fatalf("saved preset name %q", p.Name)
			}
			if len(p.FanPoints) > 0 {
				if err := curvePointsOK(p.FanPoints); err != nil {
					t.Fatalf("saved preset fan points %v: %v", p.FanPoints, err)
				}
			}
		}
	})
}

// a fake miner backend for /api/control: records every PUT.
var (
	r5MockOnce sync.Once
	r5MockMu   sync.Mutex
	r5Puts     []map[string]any
)

func r5Mock() {
	r5MockOnce.Do(func() {
		setting := map[string]any{"manual": true, "tempcontrol": true, "select": float64(0),
			"manualPowerplan": "625 MHz 9100 V 50 RPM 50 RPM PV 9400",
			"powerplans": []any{map[string]any{"info": "625 MHz 9100 V 50 RPM 50 RPM PV 9400", "level": float64(0)},
				map[string]any{"info": "0 MHz 0 V 100 RPM 100 RPM", "level": float64(3)}}}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasPrefix(r.URL.Path, "/user/login"):
				json.NewEncoder(w).Encode(map[string]any{"JWT Token": "tok"})
			case r.Method == "GET":
				json.NewEncoder(w).Encode(setting)
			case r.Method == "PUT":
				b, _ := io.ReadAll(r.Body)
				var m map[string]any
				json.Unmarshal(b, &m)
				r5MockMu.Lock()
				r5Puts = append(r5Puts, m)
				r5MockMu.Unlock()
				io.WriteString(w, `{"status":0}`)
			}
		}))
		*minerWeb = strings.TrimPrefix(srv.URL, "http://")
		jwtToken = ""
		authMu.Lock()
		auth.MinerPass = "pw"
		authMu.Unlock()
		wakeWait = 0
	})
}

func FuzzR5ControlPost(f *testing.F) {
	for _, s := range []string{
		`{"freq_mhz":600,"vrun_mv":9100,"pv_mv":9400,"fan1":50,"fan2":50,"confirm":true}`,
		`{"freq_mhz":600,"vrun_mv":9100,"pv_mv":9400,"confirm":true}`,
		`{"freq_mhz":900,"vrun_mv":9300,"pv_mv":9700,"fan1":0,"fan2":0,"confirm":true,"override":true}`,
		`{"preset":0,"confirm":true}`, `{"preset":1,"confirm":true}`, `{"preset":-1,"confirm":true}`, `{"preset":1e308,"confirm":true}`,
		`{"freq_mhz":"600","confirm":true}`, `{"release":true}`, `{"freq_mhz":600,"freq_mhz":2000,"confirm":true}`,
		`{"freq_mhz":600,"vrun_mv":9100,"pv_mv":9400,"fan1":101,"fan2":5,"confirm":true}`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		r5Env(t)
		r5Mock()
		r5MockMu.Lock()
		r5Puts = nil
		r5MockMu.Unlock()
		res := r5PostB(handleControl, body)
		r5ErrOK(t, res)
		r5MockMu.Lock()
		puts := r5Puts
		r5MockMu.Unlock()
		for _, m := range puts {
			if man, _ := m["manual"].(bool); !man {
				continue // a firmware level (its own plan)
			}
			s, _ := m["manualPowerplan"].(string)
			p, ok := parsePowerplan(s)
			if !ok {
				t.Fatalf("PUT unparseable plan %q", s)
			}
			if p.FreqMHz == 0 {
				continue
			}
			if err := validateControl(p); err != nil {
				t.Fatalf("PUT unsafe plan %q: %v (body %q)", s, err, body)
			}
			if p.Fan1 < fanFloor() || p.Fan2 < fanFloor() {
				t.Fatalf("PUT fans under the floor: %q", s)
			}
		}
	})
}

func FuzzR5SchedulePost(f *testing.F) {
	for _, s := range []string{
		`{"action":"save","enabled":true,"rules":[{"start":"07:00","days":[0,1],"preset":"Stock"}],"base":"Idle","restart":{"every":"day","at":"03:00"}}`,
		`{"action":"save","rules":[{"start":"7:00","days":[7],"preset":"Stock"}]}`,
		`{"action":"save","restart":{"every":"3months","at":"03:00","mday":28,"anchor":-99999999999}}`,
		`{"action":"save","restart":{"every":"week","at":"03:00","day":9}}`,
		`{"action":"save","restart":{"marks":[0,30,60,90]}}`, `{"action":"pause","paused":true}`,
		`{"action":"save","rules":[{"start":"07:00","days":[0,0],"preset":"Stock"}]}`,
	} {
		f.Add([]byte(s))
	}
	dir, _ := os.MkdirTemp("", "r5sched-")
	f.Cleanup(func() { schedRunMu.Lock(); os.RemoveAll(dir); schedRunMu.Unlock() })
	f.Fuzz(func(t *testing.T, body []byte) {
		os.RemoveAll(dir + "/schedule.json")
		r5EnvIn(dir)
		r5Mock()
		res := r5PostB(handleSchedule, body)
		r5ErrOK(t, res)
		s := loadSchedule()
		if res["ok"] == true {
			if err := validateSchedule(&s); err != nil {
				t.Fatalf("saved schedule fails its own check: %v", err)
			}
		}
	})
}

// schedule.json on disk.
func FuzzR5LoadSchedule(f *testing.F) {
	f.Add([]byte(`{"restart":{"every":"hour","at":"00:00","marks":[0,1,2,3]}}`))
	f.Add([]byte(`{"grid":[["a"]],"restarts":[[true]]}`))
	f.Add([]byte(`{"rules":[{"start":"07:00","days":[99,-1],"preset":"x"}]}`))
	f.Fuzz(func(t *testing.T, body []byte) {
		r5Env(t)
		os.WriteFile(schedulePath(), body, 0o600)
		s := loadSchedule()
		_ = scheduleView(s) // no panic
		if len(s.Restart.Marks) > 168 {
			t.Fatalf("%d restart marks loaded", len(s.Restart.Marks))
		}
		for _, m := range s.Restart.Marks {
			if m < 0 || m >= weekMin || m%30 != 0 {
				t.Fatalf("restart mark %d loaded", m)
			}
		}
	})
}

func FuzzR5FanPairPost(f *testing.F) {
	for _, s := range []string{`{"offset":-20}`, `{"offset":1e308}`, `{"offset":-1e308}`, `{"offset":"5"}`, `{"offset":5.5}`, `{}`, `{"offset":null}`, `{"offset":-30}`} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		r5Env(t)
		fanSysDir = t.TempDir()
		res := r5PostB(handleFanPair, body)
		r5ErrOK(t, res)
		fpMu.Lock()
		fpLoaded = false
		fpMu.Unlock()
		if o := fanPairLoad(); o < fanPairMin || o > fanPairMax {
			t.Fatalf("offset %d", o)
		}
	})
}

// fanpair.json, fan-guard.json, fan-lock.json, pool-hold.json, overclock.json on disk.
func FuzzR5SmallStateFiles(f *testing.F) {
	for _, s := range []string{`{"offset":-999}`, `{"floor":500}`, `{"floor":-5}`, `{"at":1`, ``, `{"caps":{"pv":99999,"freq":-1}}`, `{"acknowledged":true,"caps":{"pv":1}}`} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		r5Env(t)
		for _, p := range []string{fanPairPath(), fanGuardPath(), fanLockPath(), poolHoldPath(), overclockPath()} {
			os.WriteFile(p, body, 0o644)
		}
		if o := fanPairLoad(); o < fanPairMin || o > fanPairMax {
			t.Fatalf("pair offset %d", o)
		}
		if fl := fanFloor(); fl < fanMin || fl > fanGuardMax {
			t.Fatalf("fan floor %d", fl)
		}
		if fanLockErr() == nil {
			t.Fatalf("fan-lock.json present (%q) but not locked", body)
		}
		if poolHoldErr() == nil {
			t.Fatalf("pool-hold.json present but not held")
		}
		if v := effHard("pv", pvHardMax); v > pvHardMax || v <= 0 {
			t.Fatalf("pv ceiling %d", v)
		}
		if v := effHard("freq", freqHardMax); v > freqHardMax || v <= 0 {
			t.Fatalf("freq ceiling %d", v)
		}
	})
}

// POST /api/fancheck fit: the file it writes stays within the fit's bounds; refused → nothing written.
func FuzzR5FanCheckPost(f *testing.F) {
	for _, s := range []string{`{"fit":[2200,2200,2200,2200]}`, `{"fit":[0,0,0,0]}`, `{"fit":[1700,1e308]}`, `{"fit":[-1,-1,-1,-1]}`,
		`{"fit":[1500,1500,1500,1500]}`, `{"fit":"x"}`, `{"stock":true}`, `{"lock_off":false}`, `{"unlock":true}`} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		var probe struct {
			Rerun  bool `json:"rerun"`
			Unlock bool `json:"unlock"`
		}
		json.Unmarshal(body, &probe)
		if probe.Rerun {
			return // restarts Goldshell's watchdog: not here
		}
		dir := r5Env(t)
		productPath = dir + "/root-product.json"
		os.WriteFile(productPath, []byte(`{"name":"SC Lite","fans":[{"maxrpm":2200},{"maxrpm":2200},{"maxrpm":2200},{"maxrpm":2200}]}`), 0o644)
		sweep := []int{0, 300, 500, 700, 900, 1100, 1250, 1350, 1450, 1550, 1600}
		readFanSweep = func() map[int][]int { return map[int][]int{0: sweep, 1: sweep, 2: sweep, 3: sweep} }
		res := r5PostB(handleFanCheck, body)
		r5ErrOK(t, res)
		b, err := os.ReadFile(fanOverlayPath())
		if err != nil {
			return
		}
		if res["ok"] != true {
			t.Fatalf("refused but a fitted file was written")
		}
		_, fans, err := readProductFansBytes(b)
		if err != nil {
			t.Fatalf("fitted file unreadable: %v", err)
		}
		lo := fitMaxRPM(sweep)
		for i, fm := range fans {
			v, _ := fm["maxrpm"].(float64)
			if v < float64(lo) || v > float64(fanStockMax()) {
				t.Fatalf("fan %d fitted to %v (measured fit %d)", i, v, lo)
			}
		}
	})
}
