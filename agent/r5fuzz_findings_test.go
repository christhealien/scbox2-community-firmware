package main

// Round 5 fuzz: table tests for the Controls & fan tab's inputs and the files it persists.
// Each test states the invariant it checks; a failure is a finding.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// r5Env points every persisted file at a fresh temp dir and resets the fan globals.
func r5Env(t testing.TB) string { return r5EnvIn(t.TempDir()) }

// r5EnvIn: as r5Env in a given dir (the schedule POST starts a scheduleTick that may still write after the test).
func r5EnvIn(dir string) string {
	schedRunMu.Lock() // a scheduleTick a schedule POST started reads these under it
	defer schedRunMu.Unlock()
	*authFile = filepath.Join(dir, "auth.json")
	*fanPath = filepath.Join(dir, "fan.json")
	*presetPath = filepath.Join(dir, "presets.json")
	schedPath = filepath.Join(dir, "schedule.json")
	fanGuardMu.Lock()
	fanGuardSt, fanGuardOK = fanGuardState{}, false
	fanGuardMu.Unlock()
	fpMu.Lock()
	fpLoaded, fpOffset = false, 0
	fpMu.Unlock()
	fanMu.Lock()
	fanCfg = defaultFanConfig()
	fanHist = nil
	fanMu.Unlock()
	setCharge("")
	return dir
}

func r5Post(h http.HandlerFunc, path, body string) map[string]any {
	rr := httptest.NewRecorder()
	h(rr, httptest.NewRequest("POST", path, strings.NewReader(body)))
	var m map[string]any
	json.Unmarshal(rr.Body.Bytes(), &m)
	return m
}

// F1: a refused POST /api/fan must not change the running curve. `in := cur` shares cur.Points' backing array
// and the cur.Curves map with fanCfg, and json.Unmarshal writes into both in place.
func TestR5FanRefusedPostLeavesLivePoints(t *testing.T) {
	r5Env(t)
	live := []curvePoint{{50, 40}, {60, 60}, {70, 80}, {80, 100}}
	fanMu.Lock()
	fanCfg.Profile, fanCfg.Points = "custom", append([]curvePoint(nil), live...)
	fanMu.Unlock()
	// all fans at the 10 % floor, and poll_s 1 so it is refused
	res := r5Post(handleFan, "/api/fan", `{"enabled":true,"profile":"custom","poll_s":1,
		"points":[{"temp":20,"fan":10},{"temp":30,"fan":10},{"temp":40,"fan":10},{"temp":100,"fan":10}]}`)
	if res["ok"] != false {
		t.Fatalf("expected a refusal: %v", res)
	}
	fanMu.RLock()
	got := append([]curvePoint(nil), fanCfg.Points...)
	c := fanCfg
	fanMu.RUnlock()
	for i := range live {
		if got[i] != live[i] {
			t.Errorf("refused POST rewrote the running curve: %v (was %v); target at 85 °C now %d %%", got, live, targetFan(c, 85))
			break
		}
	}
}

func TestR5FanRefusedPostLeavesLibrary(t *testing.T) {
	r5Env(t)
	mine := fanCurve{Label: "Mine", Points: []curvePoint{{50, 40}, {80, 100}}, AbortC: 88, CooldownS: 45, PollS: 10, Smoothing: 1, ForceTC: true}
	fanMu.Lock()
	fanCfg.Profile = "mine"
	fanCfg.Curves = map[string]fanCurve{"mine": mine}
	fanMu.Unlock()
	res := r5Post(handleFan, "/api/fan", `{"enabled":true,"profile":"mine","throttle_enable":true,"throttle_c":1,
		"curves":{"mine":{"label":"x","points":[{"temp":20,"fan":100},{"temp":100,"fan":0}],"abort_c":0,"poll_s":0}}}`)
	if res["ok"] != false {
		t.Fatalf("expected a refusal: %v", res)
	}
	fanMu.RLock()
	got := fanCfg.Curves["mine"]
	c := fanCfg
	fanMu.RUnlock()
	if len(got.Points) != 2 || got.Points[1] != (curvePoint{80, 100}) || got.AbortC != 88 {
		t.Errorf("refused POST rewrote a library curve (bypassing validateCurve): %+v; running target at 95 °C now %d %%", got, targetFan(c, 95))
	}
}

// the same aliasing is a data race (concurrent map writes are fatal in Go): run with -race.
func TestR5FanPostRacesFanLoop(t *testing.T) {
	r5Env(t)
	fanMu.Lock()
	fanCfg.Profile = "mine"
	fanCfg.Curves = map[string]fanCurve{"mine": {Label: "Mine", Points: []curvePoint{{50, 40}, {80, 100}}, AbortC: 88, CooldownS: 45, PollS: 10}}
	fanMu.Unlock()
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() { // what fanLoop does each tick: copy under the lock, read without it
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			fanMu.RLock()
			c := fanCfg
			fanMu.RUnlock()
			_ = curvePoints(c)
		}
	}()
	for i := 0; i < 200; i++ {
		r5Post(handleFan, "/api/fan", `{"poll_s":1,"curves":{"mine":{"label":"x","points":[{"temp":20,"fan":10},{"temp":90,"fan":10}]},"k`+string(rune('a'+i%26))+`":{}}}`)
	}
	close(stop)
	wg.Wait()
}

// F2: a preset's fan_points are only checked when fan_profile is "custom"; any other profile saves them
// unchecked, and they become the selectable "tuned:<name>" curve, which /api/fan runs without checking points.
func TestR5PresetSaveUncheckedFanPoints(t *testing.T) {
	r5Env(t)
	res := r5Post(handlePresets, "/api/presets", `{"action":"save","name":"Evil","mhz":600,"mv":9100,"pv":9400,
		"fan_profile":"curve-60","fan_points":[{"temp":40,"fan":100},{"temp":95,"fan":0}]}`)
	if res["ok"] == true {
		t.Errorf("preset saved with a fan curve that drops to 0 %% as it heats up")
	}
	res = r5Post(handleFan, "/api/fan", `{"enabled":true,"profile":"tuned:Evil"}`)
	fanMu.RLock()
	c := fanCfg
	fanMu.RUnlock()
	if res["ok"] == true {
		t.Errorf("auto fan accepted curve tuned:Evil: fan at 85 °C = %d %% (at 45 °C = %d %%)", targetFan(c, 85), targetFan(c, 45))
	}
}

// F3: a damaged fan-lock.json (power cut mid-write, bad flash) unlocks mining: loadFanLock says "not locked"
// whenever the file doesn't parse. The pool guard's hold treats a damaged file as held.
func TestR5FanLockDamagedFileStillLocks(t *testing.T) {
	r5Env(t)
	for _, body := range []string{`{"at":1730000000,"flagged":"fan 2 at 0 RP`, ``, "\x00\x00\x00", `[]`, `"x"`} {
		os.WriteFile(fanLockPath(), []byte(body), 0o644)
		if fanLockErr() == nil {
			t.Errorf("fan-lock.json %q present but mining is unlocked", body)
		}
	}
}

// F4: fan-guard.json's floor is used unchecked: a floor of 500 becomes every fan write (the driver turns a huge
// level into "PWM off"), and validated plans carry "500 RPM".
func TestR5FanGuardFloorFromFileBounded(t *testing.T) {
	r5Env(t)
	os.WriteFile(fanGuardPath(), []byte(`{"floor":500,"at":1,"why":"x"}`), 0o644)
	if f := fanFloor(); f > fanGuardMax {
		t.Errorf("fanFloor() = %d from the file (guard max %d)", f, fanGuardMax)
	}
	if v := targetFan(defaultFanConfig(), 70); v > 100 {
		t.Errorf("targetFan = %d %%", v)
	}
}

// F5: fan.json is loaded without validation: values the API refuses (abort 0, throttle at 0 °C) run.
func TestR5LoadFanConfigDangerousValues(t *testing.T) {
	cases := []string{
		`{"enabled":true,"profile":"curve-balanced","abort_c":0}`,
		`{"enabled":true,"profile":"curve-balanced","abort_c":40}`,
		`{"enabled":true,"profile":"curve-balanced","abort_c":-1e308}`,
		`{"enabled":true,"profile":"curve-balanced","throttle_enable":true,"throttle_c":0,"throttle_step":5000}`,
		`{"enabled":true,"profile":"custom","points":[{"temp":20,"fan":100},{"temp":100,"fan":0}]}`,
	}
	for _, body := range cases {
		r5Env(t)
		os.WriteFile(*fanPath, []byte(body), 0o644)
		loadFanConfig()
		fanMu.RLock()
		c := fanCfg
		fanMu.RUnlock()
		if err := validateFanConfig(c); err != nil {
			t.Errorf("fan.json %s loaded and running though the API refuses it: %v", body, err)
		}
	}
}

// F6: the fitted fan file is only checked to parse at start; a parseable one with maxrpm 0 (no fan is ever
// flagged: the OEM's fan check is off) or 1e9 (every fan flagged) is kept. productFansSane is the proposed check.
func TestR5FittedFanFileRange(t *testing.T) {
	dir := r5Env(t)
	p := filepath.Join(dir, "product.json")
	for _, body := range []string{`{"fans":[{"maxrpm":0},{"maxrpm":0}]}`, `{"fans":[{"maxrpm":1e9}]}`, `{"fans":[{"maxrpm":"2200"}]}`, `{"fans":[{"maxrpm":-5}]}`} {
		os.WriteFile(p, []byte(body), 0o644)
		if _, _, err := readProductFans(p); err == nil {
			if r5ProductFansSane(p) == nil {
				t.Errorf("fitted fan file %s would stay bound (only parse is checked)", body)
			}
		}
	}
}

// F7: request decode errors leak Go's internals into the page's toast.
func TestR5NoRawJSONErrors(t *testing.T) {
	r5Env(t)
	var put map[string]any
	srv := mockStock(t, &put)
	defer srv.Close()
	*minerWeb = strings.TrimPrefix(srv.URL, "http://")
	jwtToken = ""
	authMu.Lock()
	auth.MinerPass = "pw"
	authMu.Unlock()
	type hc struct {
		name string
		h    http.HandlerFunc
		body string
	}
	for _, c := range []hc{
		{"control", handleControl, `{"freq_mhz":"600","confirm":true}`},
		{"control-float", handleControl, `{"freq_mhz":600.5,"confirm":true}`},
		{"fan", handleFan, `{"enabled":"yes"}`},
		{"fan-big", handleFan, `{"poll_s":1e308}`},
		{"schedule", handleSchedule, `{"action":"save","rules":{}}`},
		{"overclock", handleOverclock, `{"caps":{"pv":"x"}}`},
		{"schedule-trunc", handleSchedule, `{"action":"save","rules":[`},
	} {
		res := r5Post(c.h, "/x", c.body)
		e, _ := res["err"].(string)
		if strings.Contains(e, "json:") || strings.Contains(e, "unexpected EOF") || strings.Contains(e, "Go struct") {
			t.Errorf("%s: raw decoder error shown to the user: %q", c.name, e)
		}
	}
}

// F8: preset names: control characters break the page's Apply/Edit buttons (a raw newline inside a JS string in
// onclick), and "Stock"/"Idle" are shadowed by the built-ins (the saved one can never be applied).
func TestR5PresetNames(t *testing.T) {
	r5Env(t)
	for _, n := range []string{"a\nb", "tab\there", "nul\x00", "Stock", "Idle", "__back"} {
		b, _ := json.Marshal(map[string]any{"action": "save", "name": n, "mhz": 600, "mv": 9100, "pv": 9400, "fan_profile": "none"})
		res := r5Post(handlePresets, "/api/presets", string(b))
		if res["ok"] == true {
			t.Errorf("preset name %q accepted", n)
		}
	}
}

// F9: schedule.json is loaded without validation: restart marks every minute restart mining every minute.
func TestR5ScheduleFileRestartMarks(t *testing.T) {
	r5Env(t)
	marks := make([]int, 0, weekMin)
	for m := 0; m < weekMin; m++ {
		marks = append(marks, m)
	}
	b, _ := json.Marshal(map[string]any{"enabled": false, "restart": map[string]any{"every": "off", "marks": marks}})
	os.WriteFile(schedulePath(), b, 0o600)
	s := loadSchedule()
	n := 0
	t0 := time.Date(2026, 11, 2, 10, 1, 0, 0, time.Local)
	for i := 0; i < 60; i++ {
		if restartDue(s.Restart, t0.Add(time.Duration(i)*time.Minute)) {
			n++
		}
	}
	if n > 2 {
		t.Errorf("a hand-edited schedule.json restarts mining %d times an hour", n)
	}
}

// r5ProductFansSane: what productCheckAtStart checks today (that the file parses).
func r5ProductFansSane(p string) error { return productOverlayOK(p) }
