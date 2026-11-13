package main

// B11: schedule & presets on the SC Box II.
//
// Box II ships only its two stock plans (690 MHz "Hashrate", 525 MHz "Low-power"); voltage is a fixed PWM
// trim and the tuner/clock control are off. These tests prove, on the Box II profile:
//   - the Presets tab offers exactly the two stock modes (no saved/custom/tuner presets);
//   - creating a custom clock/voltage preset, and building tuner presets, are refused;
//   - the schedule switches between the two modes by selecting the firmware plan by index (manual:false),
//     never writing a clock or voltage, and refuses to switch to a preset that doesn't exist on this model;
//   - the schedule's Idle goes through idleAction(model) and degrades gracefully when the Box II stop is
//     unproven/absent (idleAction errors; nothing is mis-written);
//   - loadPresets does NOT clamp/write SC Lite-style voltages into a Box II preset (R14 F-notes);
// and that SC Lite's schedule/presets behaviour is unchanged.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// b11Box2 is a minimal Box II stock miner: two plans (Hashrate at list position 0, Low-power at 1), no idle
// plan. It records every PUT /mcb/setting so a test can see exactly what the panel wrote.
type b11Box2 struct {
	mu      sync.Mutex
	setting map[string]any
	puts    []map[string]any
	srv     *httptest.Server
}

const b11Box2Setting = `{"name":"Benchmark","powerplans":[
 {"info":"690 MHz 42 V 50 RPM 41 RPM","level":0,"temp_target":77.0,"prevoltage":40.00001,"voltage":42.00001},
 {"info":"525 MHz 61 V 40 RPM 59 RPM","level":2,"temp_target":73.0,"prevoltage":59.00001,"voltage":61.00001}],
 "select":0,"tempcontrol":true,"ledcontrol":false,"manual":false,
 "manualPowerplan":"690 MHz 42 V 50 RPM 41 RPM","version":"v1.2"}`

func newB11Box2(t *testing.T) *b11Box2 {
	t.Helper()
	useBox2(t) // model -> SC Box II, restored on cleanup
	m := &b11Box2{}
	json.Unmarshal([]byte(b11Box2Setting), &m.setting)
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
			m.mu.Lock()
			m.puts = append(m.puts, in)
			m.setting = in
			m.mu.Unlock()
			io.WriteString(w, `{"status":0}`)
		}
	}))
	*minerWeb = strings.TrimPrefix(m.srv.URL, "http://")
	*authFile = filepath.Join(t.TempDir(), "auth.json")
	*presetPath = filepath.Join(t.TempDir(), "presets.json")
	schedPath = filepath.Join(t.TempDir(), "schedule.json")
	jwtToken = ""
	authMu.Lock()
	auth.MinerPass = "pw"
	authMu.Unlock()
	setCharge("")
	t.Cleanup(func() {
		m.srv.Close()
		schedPath = ""
		setCharge("")
		stockCacheMu.Lock()
		stockCache = powerplan{}
		stockCacheMu.Unlock()
	})
	return m
}

func (m *b11Box2) putsCopy() []map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]map[string]any(nil), m.puts...)
}

func (m *b11Box2) lastPut() map[string]any {
	p := m.putsCopy()
	if len(p) == 0 {
		return nil
	}
	return p[len(p)-1]
}

//  1. The Presets tab (handlePresets GET) offers EXACTLY the two stock modes on Box II — no saved presets,
//     even if a presets.json with custom entries is lying on disk.
func TestB11Box2PresetsAreStockModesOnly(t *testing.T) {
	m := newB11Box2(t)
	// a stray SC Lite-style presets file must not appear as a Box II preset
	savePresets([]preset{{Name: "High", MHz: 625, MV: 9100, PV: 9400}})
	rec := httptest.NewRecorder()
	handlePresets(rec, httptest.NewRequest("GET", "/api/presets", nil))
	var out struct {
		OK      bool     `json:"ok"`
		Box2    bool     `json:"box2"`
		Stock   bool     `json:"stock_modes"`
		Presets []preset `json:"presets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	if !out.OK || !out.Box2 || !out.Stock {
		t.Fatalf("GET presets: ok=%v box2=%v stock_modes=%v", out.OK, out.Box2, out.Stock)
	}
	if len(out.Presets) != 2 {
		t.Fatalf("Box II should offer exactly the 2 stock modes, got %d: %+v", len(out.Presets), out.Presets)
	}
	byName := map[string]preset{}
	for _, p := range out.Presets {
		byName[p.Name] = p
	}
	if h, ok := byName["Hashrate"]; !ok || h.MHz != 690 {
		t.Fatalf("missing/ wrong Hashrate mode: %+v", byName)
	}
	if l, ok := byName["Low-power"]; !ok || l.MHz != 525 {
		t.Fatalf("missing/ wrong Low-power mode: %+v", byName)
	}
	if _, ok := byName["High"]; ok {
		t.Fatalf("a saved custom preset leaked into Box II presets: %+v", byName)
	}
	t.Logf("Box II presets = %+v", out.Presets)
	_ = m
}

//  2. Creating a custom clock/voltage preset (save) and building tuner presets (build) are both refused on
//     Box II — and nothing is written to the presets file.
func TestB11Box2CustomAndTunerPresetRefused(t *testing.T) {
	m := newB11Box2(t)
	post := func(body string) map[string]any {
		rec := httptest.NewRecorder()
		handlePresets(rec, httptest.NewRequest("POST", "/api/presets", strings.NewReader(body)))
		var r map[string]any
		json.Unmarshal(rec.Body.Bytes(), &r)
		return r
	}
	// custom clock/voltage preset
	r := post(`{"action":"save","name":"Turbo","mhz":690,"mv":42,"pv":0}`)
	if ok, _ := r["ok"].(bool); ok {
		t.Fatalf("Box II accepted a custom preset: %v", r)
	}
	if d, _ := r["disabled"].(bool); !d {
		t.Fatalf("custom-preset refusal should be marked disabled: %v", r)
	}
	// tuner-built presets
	r = post(`{"action":"build"}`)
	if ok, _ := r["ok"].(bool); ok {
		t.Fatalf("Box II accepted tuner-built presets: %v", r)
	}
	// the refusals created no presets file / wrote nothing
	if ps := loadPresets(); len(ps) != 0 {
		t.Fatalf("a refused save/build still wrote presets: %+v", ps)
	}
	// presetsList (what the UI shows) is still only the two stock modes
	if pl := presetsList(); len(pl) != 2 {
		t.Fatalf("presetsList on Box II = %d, want 2", len(pl))
	}
	t.Logf("save refused=%v ; build refused", r)
	_ = m
}

//  3. The schedule/Presets apply path switches between the two stock modes by SELECTING the firmware plan by
//     index with manual:false — never a manual/clock/voltage write — and refuses a preset that doesn't exist.
func TestB11Box2ScheduleSwitchesModesByIndex(t *testing.T) {
	m := newB11Box2(t)

	if err := applyPresetNameGen("Low-power", noGen); err != nil {
		t.Fatalf("switch to Low-power: %v", err)
	}
	put := m.lastPut()
	if man, _ := put["manual"].(bool); man {
		t.Fatalf("mode switch wrote a MANUAL plan (manual:true) instead of selecting by index: %+v", put)
	}
	if sel, _ := put["select"].(float64); int(sel) != 1 { // 525 MHz is list position 1
		t.Fatalf("Low-power should select index 1, got %v", put["select"])
	}
	if getCharge() != "preset:Low-power" {
		t.Fatalf("charge = %q, want preset:Low-power", getCharge())
	}

	if err := applyPresetNameGen("Hashrate", noGen); err != nil {
		t.Fatalf("switch to Hashrate: %v", err)
	}
	put = m.lastPut()
	if man, _ := put["manual"].(bool); man {
		t.Fatalf("mode switch wrote a MANUAL plan: %+v", put)
	}
	if sel, _ := put["select"].(float64); int(sel) != 0 { // 690 MHz is list position 0
		t.Fatalf("Hashrate should select index 0, got %v", put["select"])
	}
	if getCharge() != "preset:Hashrate" {
		t.Fatalf("charge = %q, want preset:Hashrate", getCharge())
	}

	// no PUT ever carried a clock or voltage (manualPowerplan), i.e. the panel only ever selected a plan
	for i, p := range m.putsCopy() {
		if _, has := p["manualPowerplan"]; has {
			if man, _ := p["manual"].(bool); man {
				t.Fatalf("PUT %d wrote a manual plan on Box II mode switch: %+v", i, p)
			}
		}
	}

	// a preset that doesn't exist on Box II (a tuner preset name) is refused, with nothing written
	before := len(m.putsCopy())
	if err := applyPresetNameGen("High", noGen); err == nil {
		t.Fatalf("Box II applied a non-existent preset 'High'")
	}
	if n := len(m.putsCopy()); n != before {
		t.Fatalf("refused preset still wrote to the miner: %d new PUTs", n-before)
	}
}

//  4. The schedule's Idle goes through idleAction(model). On Box II there is no stock idle plan and the
//     miner-API stop is unproven, so idleAction errors — the apply path returns that error and writes nothing
//     (the scheduler then reports it and the guards fall back to the firmware cut-off).
func TestB11Box2ScheduleIdleDegradesGracefully(t *testing.T) {
	m := newB11Box2(t)
	before := len(m.putsCopy())
	err := applyPresetNameGen("Idle", noGen)
	if err == nil {
		t.Fatalf("Box II Idle should fail gracefully (no idle plan), got nil")
	}
	if !strings.Contains(err.Error(), "stop-hashing") {
		t.Fatalf("Idle error = %v, want it to mention the missing idle plan", err)
	}
	if n := len(m.putsCopy()); n != before {
		t.Fatalf("Box II Idle mis-wrote to the miner: %d new PUTs", n-before)
	}
	// idleAction itself, both profiles: Box II refuses, SC Lite would use the firmware idle plan.
	if e := idleAction(profSCBox2); e == nil || !strings.Contains(e.Error(), "stop-hashing") {
		t.Fatalf("idleAction(Box2) = %v", e)
	}
	t.Logf("Idle degraded: %v", err)
}

//  5. presetExists / validateSchedule on Box II: the two modes + Idle/Stock/back are valid; a clock/voltage
//     preset that doesn't exist is refused by schedule validation.
func TestB11Box2ScheduleValidation(t *testing.T) {
	m := newB11Box2(t)
	for _, n := range []string{"Hashrate", "Low-power", "Idle", "Stock", schedBack} {
		if !presetExists(n) {
			t.Fatalf("presetExists(%q) = false on Box II", n)
		}
	}
	if presetExists("High") {
		t.Fatalf("presetExists(\"High\") = true on Box II (no such preset)")
	}
	// a schedule switching between the two modes validates
	good := schedule{Enabled: true, Rules: []schedRule{
		{Start: "08:00", Days: l11All, Preset: "Hashrate"},
		{Start: "23:00", Days: l11All, Preset: "Low-power"},
	}, Base: schedBack}
	if err := validateSchedule(&good); err != nil {
		t.Fatalf("two-mode schedule rejected: %v", err)
	}
	// scheduling a preset that doesn't exist on Box II is refused
	bad := schedule{Enabled: true, Rules: []schedRule{{Start: "08:00", Days: l11All, Preset: "High"}}, Base: schedBack}
	if err := validateSchedule(&bad); err == nil {
		t.Fatalf("schedule with a non-existent Box II preset was accepted")
	}
	// the schedule brushes are the two modes + Idle (no tuner presets, no Stock brush)
	br := schedulePresets()
	keys := map[string]bool{}
	for _, b := range br {
		keys[b["key"].(string)] = true
	}
	if !keys["Hashrate"] || !keys["Low-power"] || !keys["Idle"] {
		t.Fatalf("schedule brushes missing a stock mode / Idle: %+v", br)
	}
	if len(br) != 3 {
		t.Fatalf("Box II schedule brushes = %d, want 3 (Hashrate, Low-power, Idle): %+v", len(br), br)
	}
	_ = m
}

var l11All = []int{0, 1, 2, 3, 4, 5, 6}

//  6. loadPresets must NOT clamp/write SC Lite-style voltages into a Box II preset: on Box II the voltage
//     window is empty (0..0), so the SC Lite clamp would zero out a preset's mv/pv (R14 F-notes). The fix
//     leaves Box II preset entries untouched; the SC Lite clamp is unchanged (regression, test 7).
func TestB11Box2LoadPresetsNoVoltageInject(t *testing.T) {
	newB11Box2(t)
	// a presets file carrying SC Lite-style voltages (e.g. migrated/hand-edited) must survive loadPresets
	// on Box II without being clamped to the empty voltage window.
	savePresets([]preset{{Name: "x", MHz: 625, MV: 9100, PV: 9400}})
	got := loadPresets()
	if len(got) != 1 {
		t.Fatalf("loadPresets = %+v", got)
	}
	if got[0].MV != 9100 || got[0].PV != 9400 {
		t.Fatalf("Box II loadPresets clamped voltages to the empty window (mv=%d pv=%d): it must not touch voltages on this model", got[0].MV, got[0].PV)
	}
	t.Logf("Box II preset kept mv=%d pv=%d (not clamped)", got[0].MV, got[0].PV)
}

//  7. SC Lite regression: loadPresets still clamps an out-of-range voltage to the SC Lite window, and the SC
//     Lite preset/schedule name rules are unchanged.
func TestB11SCLitePresetsUnchanged(t *testing.T) {
	// default model is SC Lite; make sure the windows are seeded.
	model, modelWhy = profSCLite, "test"
	setWindowsFromModel()
	t.Cleanup(func() { model, modelWhy = profSCLite, "test"; setWindowsFromModel() })

	*presetPath = filepath.Join(t.TempDir(), "presets.json")
	// a preset with an absurd PV is clamped into the SC Lite window (behaviour the port must preserve)
	savePresets([]preset{{Name: "p", MHz: 625, MV: 9999, PV: 99999}})
	got := loadPresets()
	if len(got) != 1 {
		t.Fatalf("loadPresets = %+v", got)
	}
	if got[0].PV > pvHardMax || got[0].PV != pvHardMax {
		t.Fatalf("SC Lite loadPresets no longer clamps PV (pv=%d, want %d)", got[0].PV, pvHardMax)
	}
	if got[0].MV > got[0].PV {
		t.Fatalf("SC Lite loadPresets: MV %d exceeds PV %d (clamp broken)", got[0].MV, got[0].PV)
	}
	// SC Lite schedule names: a saved preset exists; a stock-mode name does not (that's Box II's vocabulary)
	if !presetExists("p") {
		t.Fatalf("SC Lite presetExists(\"p\") = false")
	}
	if !presetExists("Stock") || !presetExists("Idle") {
		t.Fatalf("SC Lite built-in schedule names broke")
	}
	t.Logf("SC Lite PV clamped to %d; MV=%d", got[0].PV, got[0].MV)
}
