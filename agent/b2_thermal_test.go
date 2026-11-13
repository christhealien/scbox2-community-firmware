package main

// Lane B2 — thermal guard & heat limits for the SC Box II port.
//
// Proves:
//  1. On the SC Box II profile every thermal threshold the guard uses is < the OEM's 86 °C cut-off and
//     matches the profile (so the guards actually act, unlike the old hard-coded 88/90 which sat ABOVE the
//     Box II cut-off = dead code).
//  2. The SC Lite profile reproduces the exact pre-port constants (88 / 90 / 80), so SC Lite is unchanged.
//  3. The guard's idle-fallback path on Box II: when idleAction returns "no idle plan", thermalIdle does NOT
//     silently do nothing — it drives the fans to 100 %, warns clearly that only the 86 °C cut-off will stop
//     the boards, and does NOT engage the (false) cool-down hold.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// useModel switches the active profile and seeds every window/thermal variable, restoring SC Lite after.
func useModel(t *testing.T, p modelProfile) {
	t.Helper()
	model, modelWhy = p, "test"
	setWindowsFromModel()
	setThermalFromModel()
	t.Cleanup(func() {
		model, modelWhy = profSCLite, "test"
		setWindowsFromModel()
		setThermalFromModel()
	})
}

func TestB2Box2ThermalsUnderCutoff(t *testing.T) {
	useModel(t, profSCBox2)

	// the seeded package-level thresholds match the profile exactly
	want := []struct {
		name string
		got  float64
		exp  float64
	}{
		{"heatGuardC", heatGuardC, 82},
		{"abortDefC", abortDefC, 82},
		{"abortMaxC", abortMaxC, 84},
		{"throttleDefC", throttleDefC, 82},
		{"resumeDefC", resumeDefC, 75},
	}
	cutoff := model.TempCutoffC // 86, never raised
	if cutoff != 86 {
		t.Fatalf("Box II cut-off must be 86, got %.0f", cutoff)
	}
	for _, w := range want {
		if w.got != w.exp {
			t.Errorf("%s = %.1f, want %.1f", w.name, w.got, w.exp)
		}
		if w.got >= cutoff {
			t.Errorf("%s = %.1f is NOT below the %.0f °C cut-off (guard would be dead code)", w.name, w.got, cutoff)
		}
	}
	if !(resumeDefC < throttleDefC && heatGuardC <= abortMaxC && abortDefC <= abortMaxC && model.TargetC <= heatGuardC) {
		t.Fatalf("Box II thresholds not consistently ordered under the cut-off: hg=%.0f def=%.0f max=%.0f thr=%.0f res=%.0f target=%.0f",
			heatGuardC, abortDefC, abortMaxC, throttleDefC, resumeDefC, model.TargetC)
	}

	// the default fan config and built-in curves pick up the Box II figures
	dc := defaultFanConfig()
	if dc.AbortC != 82 || dc.ThrottleC != 82 || dc.ResumeC != 75 {
		t.Fatalf("defaultFanConfig on Box II = abort %.0f throttle %.0f resume %.0f, want 82/82/75", dc.AbortC, dc.ThrottleC, dc.ResumeC)
	}
	if validateFanConfig(dc) != nil {
		t.Fatalf("Box II default fan config rejected: %v", validateFanConfig(dc))
	}
	if bc, _ := builtinCurve("curve-balanced"); bc.AbortC != 82 {
		t.Fatalf("builtin curve abort on Box II = %.0f, want 82", bc.AbortC)
	}
}

func TestB2Box2AbortMaxRefusedAboveProfile(t *testing.T) {
	useModel(t, profSCBox2)
	// 84 (= cutoff-2) is the ceiling; 85 must be refused (it would sit under 86 but the panel never lets a
	// guard climb that close — never above stock).
	ok := defaultFanConfig()
	ok.AbortC = 84
	if err := validateFanConfig(ok); err != nil {
		t.Fatalf("abort 84 °C rejected on Box II: %v", err)
	}
	bad := defaultFanConfig()
	bad.AbortC = 85
	if err := validateFanConfig(bad); err == nil {
		t.Fatal("abort 85 °C accepted on Box II (must be refused: over the 84 °C abort-max)")
	}
	// a fan.json saved on another model (abort 90) is clamped down to the Box II abort-max on load
	dir := t.TempDir()
	old := *fanPath
	fp := filepath.Join(dir, "fan.json")
	fanPath = &fp
	t.Cleanup(func() { fanPath = &old })
	b, _ := json.Marshal(fanConfig{Enabled: true, Profile: "curve-balanced", Smoothing: 1, PollS: 10, CooldownS: 45,
		AbortC: 90, ThrottleEnable: true, ThrottleC: 90, ThrottleStep: 25, ResumeC: 80})
	if err := writeAtomic(fp, b, 0o644); err != nil {
		t.Fatal(err)
	}
	loadFanConfig()
	fanMu.RLock()
	c := fanCfg
	fanMu.RUnlock()
	if c.AbortC > abortMaxC || c.ThrottleC > abortMaxC {
		t.Fatalf("loaded abort/throttle not clamped under Box II cut-off: abort=%.0f throttle=%.0f (max %.0f)", c.AbortC, c.ThrottleC, abortMaxC)
	}
}

func TestB2SCLiteThermalsUnchanged(t *testing.T) {
	useModel(t, profSCLite)
	if heatGuardC != 88 || abortDefC != 88 || abortMaxC != 90 || throttleDefC != 90 || resumeDefC != 80 {
		t.Fatalf("SC Lite thermals changed: hg=%.0f def=%.0f max=%.0f thr=%.0f res=%.0f, want 88/88/90/90/80",
			heatGuardC, abortDefC, abortMaxC, throttleDefC, resumeDefC)
	}
	dc := defaultFanConfig()
	if dc.AbortC != 88 || dc.ThrottleC != 90 || dc.ResumeC != 80 {
		t.Fatalf("SC Lite default fan config = abort %.0f throttle %.0f resume %.0f, want 88/90/80", dc.AbortC, dc.ThrottleC, dc.ResumeC)
	}
	if bc, _ := builtinCurve("curve-balanced"); bc.AbortC != 88 {
		t.Fatalf("SC Lite builtin curve abort = %.0f, want 88", bc.AbortC)
	}
	// the pre-port validation window: 90 accepted, 91 refused
	okc := defaultFanConfig()
	okc.AbortC = 90
	if err := validateFanConfig(okc); err != nil {
		t.Fatalf("SC Lite abort 90 °C rejected: %v", err)
	}
	badc := defaultFanConfig()
	badc.AbortC = 91
	if err := validateFanConfig(badc); err == nil {
		t.Fatal("SC Lite abort 91 °C accepted (must be refused)")
	}
}

func TestB2IdleActionStubRefusesOnBox2(t *testing.T) {
	// the A1 stub (until B3 wires the real board-off) refuses on Box II; SC Lite would use applyIdle
	if err := idleAction(profSCBox2); err == nil {
		t.Fatal("idleAction(Box II) must return an error until B3 wires the board-off")
	} else if !strings.Contains(err.Error(), "stop-hashing") {
		t.Fatalf("idleAction(Box II) error = %q, want it to mention the unverified stop", err)
	}
}

// box2HashingStock is a fake stock web backend serving a Box II miner hashing on the 690 MHz / V42 plan.
func box2HashingStock(t *testing.T, puts *[]map[string]any) {
	t.Helper()
	setting := map[string]any{"manual": true, "tempcontrol": true, "select": float64(0),
		"manualPowerplan": "690 MHz 42 V 50 RPM 41 RPM",
		"powerplans": []any{
			map[string]any{"info": "690 MHz 42 V 50 RPM 41 RPM", "level": float64(0)},
			map[string]any{"info": "525 MHz 61 V 50 RPM 41 RPM", "level": float64(1)},
		}}
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
			*puts = append(*puts, m)
			io.WriteString(w, `{"status":0}`)
		}
	}))
	t.Cleanup(srv.Close)
	*minerWeb = strings.TrimPrefix(srv.URL, "http://")
	jwtToken = ""
	authMu.Lock()
	auth.MinerPass = "pw"
	authMu.Unlock()
}

// The headline safety test: thermalIdle on Box II with no park available must drive the fans to 100 %, warn
// that only the 86 °C cut-off will stop the boards, and NOT pretend the miner is parked/cooling.
func TestB2ThermalIdleFallbackFansTo100(t *testing.T) {
	useModel(t, profSCBox2)
	af := filepath.Join(t.TempDir(), "auth.json")
	authFile = &af

	var puts []map[string]any
	box2HashingStock(t, &puts)

	// fresh episode state
	heatIdleSince.Store(0)
	heatHoldSaved.Store(false)
	noParkWarned.Store(false)
	abortTierOn.Store(false)
	t.Cleanup(func() { abortTierOn.Store(false); noParkWarned.Store(false); heatIdleSince.Store(0) })

	// capture the activity line the guard emits
	ch := activity.sub()
	defer activity.unsub(ch)

	err := thermalIdle("overheat: board 0 at 83.0 °C")
	if err == nil {
		t.Fatal("thermalIdle on Box II returned nil — it must report that it could not park the miner")
	}
	if !strings.Contains(err.Error(), "86") || !strings.Contains(err.Error(), "100 %") {
		t.Fatalf("thermalIdle error must name the 86 °C cut-off and the 100 %% fans, got: %v", err)
	}

	// the fans were actually driven to 100 % via the running stock plan (clock/V kept, fans swapped)
	if len(puts) == 0 {
		t.Fatal("no plan written: the fans were not driven to 100 %")
	}
	last := puts[len(puts)-1]
	pp, _ := last["manualPowerplan"].(string)
	// the fan field goes to 100; the 4th field stays the stock plan's (the second trim PWM, not a fan)
	if !strings.Contains(pp, "100 RPM 41 RPM") {
		t.Fatalf("fans not forced to 100 %%: manualPowerplan = %q", pp)
	}
	if !strings.HasPrefix(pp, "690 MHz 42 V") {
		t.Fatalf("clock/V not preserved (never re-encoded by the panel): %q", pp)
	}

	// it must NOT engage the cool-down hold (the boards are still hashing, not cooling on Idle)
	if heatIdleSince.Load() != 0 {
		t.Fatalf("heatIdleSince set (%d): the guard falsely marked the miner as parked/cooling", heatIdleSince.Load())
	}
	if heatHoldSaved.Load() {
		t.Fatal("heatHoldSaved set: a cool-down hold was saved though the miner never parked")
	}
	if !abortTierOn.Load() {
		t.Fatal("abortTierOn not set: other plan writes could pull the fans back down")
	}

	// a clear warning was emitted naming the cut-off
	select {
	case line := <-ch:
		if !strings.Contains(line, "86") || !strings.Contains(line, "100 %") {
			t.Fatalf("warning does not name the 86 °C cut-off / 100 %% fans: %q", line)
		}
	case <-time.After(time.Second):
		t.Fatal("no warning emitted — the guard was silent (must say only the cut-off will stop it)")
	}

	// a second episode-internal call must not spam a new warning (rate-limited per episode)
	drain(ch)
	_ = thermalIdle("overheat: board 0 at 83.5 °C")
	select {
	case line := <-ch:
		t.Fatalf("duplicate warning within one episode: %q", line)
	case <-time.After(200 * time.Millisecond):
		// good: no repeat
	}

}

func drain(ch chan string) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

// SC Lite still parks via the vendor idle plan (manual off, select idle) — the fallback path is Box-II only.
func TestB2SCLiteThermalIdleUsesVendorPlan(t *testing.T) {
	useModel(t, profSCLite)
	af := filepath.Join(t.TempDir(), "auth.json")
	authFile = &af

	var puts []map[string]any
	setting := map[string]any{"manual": true, "tempcontrol": true, "select": float64(0),
		"manualPowerplan": "625 MHz 9100 V 50 RPM 50 RPM PV 9400",
		"powerplans": []any{
			map[string]any{"info": "625 MHz 9100 V 50 RPM 50 RPM PV 9400", "level": float64(0)},
			map[string]any{"info": "0 MHz 0 V 100 RPM 100 RPM", "level": float64(3)},
		}}
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

	heatIdleSince.Store(0)
	heatHoldSaved.Store(false)
	setCharge("")
	t.Cleanup(func() { heatIdleSince.Store(0); setCharge("") })

	if err := thermalIdle("overheat"); err != nil {
		t.Fatalf("SC Lite thermalIdle failed: %v", err)
	}
	if len(puts) == 0 {
		t.Fatal("SC Lite: no idle plan written")
	}
	last := puts[len(puts)-1]
	// the vendor path turns manual off and selects the idle powerplan by its array index (here index 1,
	// the "0 MHz" plan) — never a synthesized manual plan and never the Box II fans-to-100 fallback.
	if last["manual"] != false {
		t.Fatalf("SC Lite idle did not turn manual off: %+v", last)
	}
	sel, _ := last["select"].(float64)
	plans, _ := last["powerplans"].([]any)
	if int(sel) < 0 || int(sel) >= len(plans) {
		t.Fatalf("SC Lite idle select out of range: %+v", last)
	}
	if info, _ := plans[int(sel)].(map[string]any)["info"].(string); !strings.HasPrefix(info, "0 MHz") {
		t.Fatalf("SC Lite idle selected a non-idle plan (%q): %+v", info, last)
	}
	// SC Lite marks the miner held for cool-down (unchanged behaviour)
	if heatIdleSince.Load() == 0 {
		t.Fatal("SC Lite: heatIdleSince not set (cool-down hold lost)")
	}
}
