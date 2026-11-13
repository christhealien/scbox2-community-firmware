package main

// Round 4 (r4-curves): the Fan curves panel and the curves presets carry.

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func r4cSetup(t *testing.T) {
	t.Helper()
	d := t.TempDir()
	oa, of, op := *authFile, *fanPath, *presetPath
	*authFile = filepath.Join(d, "auth.json")
	*fanPath = filepath.Join(d, "fan.json")
	*presetPath = filepath.Join(d, "presets.json")
	fanMu.Lock()
	oc := fanCfg
	fanCfg = defaultFanConfig()
	fanHist = nil
	fanMu.Unlock()
	oldCh := getCharge()
	setCharge("")
	fanGuardClear()
	t.Cleanup(func() {
		*authFile, *fanPath, *presetPath = oa, of, op
		fanMu.Lock()
		fanCfg = oc
		fanHist = nil
		fanMu.Unlock()
		setCharge(oldCh)
		fanGuardClear()
	})
}

func r4cPost(t *testing.T, body any) map[string]any {
	t.Helper()
	b, _ := json.Marshal(body)
	rec := httptest.NewRecorder()
	handleFan(rec, httptest.NewRequest("POST", "/api/fan", bytes.NewReader(b)))
	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	return out
}

// F1: a tuned preset curve whose target sits within 6 °C of the tuner's abort loses its 100 % point:
// the curve tops out at held+15 % up to the fan safety temperature.
func TestR4C_TunedCurveReaches100(t *testing.T) {
	// a rebuild run (BuildPresets false in the API body) with fan target 83, abort 88 is accepted
	c := defaultTunerConfig()
	c.Mode, c.FanHold, c.FanTargetC, c.AbortC, c.BuildPresets = "rebuild", true, 83, 88, false
	if err := validateTunerConfig(c); err != nil {
		t.Logf("(config rejected: %v)", err)
	}
	for _, tc := range []struct{ target, abort float64 }{{83, 88}, {82, 88}, {70, 75}, {76, 88}} {
		pts := tunedPresetCurve(tc.target, 50, 30, tc.abort)
		last := pts[len(pts)-1]
		if last.Fan != 100 {
			t.Errorf("target %.0f abort %.0f: curve %s never reaches 100 %%", tc.target, tc.abort, fanCurveText(pts))
		}
	}
}

// F2: an edited tuned curve with a safety temperature at or below the Auto fan "resume" temperature (default 80)
// saves fine, but the preset then can't put its fans on (validateFanConfig: resume must be below abort).
func TestR4C_EditedTunedCurveLowSafetyBlocksPresetFans(t *testing.T) {
	r4cSetup(t)
	pr := preset{Name: "High", MHz: 600, MV: 9000, PV: 9300, TargetC: 68, FanProfile: "custom",
		FanPoints: tunedPresetCurve(68, 50, 30, 88)}
	savePresets([]preset{pr})
	j := r4cPost(t, map[string]any{"action": "profile", "key": "tuned:High", "label": "Tuned High",
		"points": pr.FanPoints, "abort_c": 78, "cooldown_s": 45, "poll_s": 10, "smoothing": 1})
	if j["ok"] != true {
		t.Fatalf("edit refused: %v", j)
	}
	applyPresetFan(loadPresets()[0])
	fanMu.RLock()
	got := fanCfg
	fanMu.RUnlock()
	if got.Profile != "custom" || got.AbortC != 78 {
		t.Errorf("preset fans not applied: profile %q abort %.0f (resume %.0f)", got.Profile, got.AbortC, got.ResumeC)
	}
}

// F2b: editing the RUNNING curve of yours to a safety temperature <= resume saves an invalid running config
// (no validateFanConfig), and the Auto fan Save then fails with an error about the hidden resume field.
func TestR4C_RunningCurveLowSafetyInvalidConfig(t *testing.T) {
	r4cSetup(t)
	j := r4cPost(t, map[string]any{"action": "profile", "label": "Mine",
		"points": []curvePoint{{50, 30}, {70, 60}, {78, 100}}, "abort_c": 85, "cooldown_s": 45, "poll_s": 10, "smoothing": 1})
	key, _ := j["key"].(string)
	fanMu.RLock()
	c := fanCfg
	fanMu.RUnlock()
	c.Profile = key
	c.Curves = nil
	if j := r4cPost(t, c); j["ok"] != true {
		t.Fatalf("select: %v", j)
	}
	r4cPost(t, map[string]any{"action": "profile", "key": key, "label": "Mine",
		"points": []curvePoint{{50, 30}, {70, 60}, {74, 100}}, "abort_c": 75, "cooldown_s": 45, "poll_s": 10, "smoothing": 1})
	fanMu.RLock()
	c = fanCfg
	fanMu.RUnlock()
	if err := validateFanConfig(c); err != nil {
		t.Errorf("running config after a library edit is invalid: %v", err)
	}
}

// F3: a reboot while a preset's own curve runs (profile "custom") and no curves of your own exist turns the
// preset's curve into "My curve"; edits to the tuned curve then no longer reach the fans.
func TestR4C_RebootMigratesPresetCurve(t *testing.T) {
	r4cSetup(t)
	pts := tunedPresetCurve(68, 50, 30, 88)
	savePresets([]preset{{Name: "High", MHz: 600, MV: 9000, PV: 9300, TargetC: 68, FanProfile: "custom", FanPoints: pts}})
	applyPresetFan(loadPresets()[0])
	setCharge("preset:High")
	// reboot
	fanMu.Lock()
	fanCfg = defaultFanConfig()
	fanMu.Unlock()
	loadFanConfig()
	fanMu.RLock()
	c := fanCfg
	fanMu.RUnlock()
	if c.Profile != "custom" {
		t.Errorf("after reboot the running preset curve became %q (library: %v)", c.Profile, c.Curves)
	}
}

// F4: a fan.json missing abort_c (hand-edited / partial) loads AbortC 0: every reading is "5 °C past the abort"
// and the miner is put on Idle after two readings.
func TestR4C_PartialFanJSONIdles(t *testing.T) {
	r4cSetup(t)
	os.WriteFile(*fanPath, []byte(`{"enabled":true,"profile":"curve-balanced"}`), 0o644)
	loadFanConfig()
	fanMu.RLock()
	c := fanCfg
	fanMu.RUnlock()
	var h heatEscState
	now := time.Now()
	h.step(62, c.AbortC, now, false)
	_, idle, why := h.step(62, c.AbortC, now.Add(10*time.Second), false)
	if idle || c.AbortC < 70 {
		t.Errorf("partial fan.json: abort %.0f, poll %d, re-send %d → idle=%v (%s)", c.AbortC, c.PollS, c.CooldownS, idle, why)
	}
}

// F5: saving or deleting a curve in the library (even one not in use) clears the watchdog fan floor.
func TestR4C_LibraryEditKeepsFanFloor(t *testing.T) {
	r4cSetup(t)
	old := readWDGuard
	t.Cleanup(func() { readWDGuard = old })
	readWDGuard = func() (watchdogView, bool) {
		return watchdogView{Fans: []int{180, 420, 410, 400}, Status: 1, Flagged: "the watchdog flags fan 0"}, true
	}
	if fanGuardCheck(15) != 25 {
		t.Fatal("setup: floor not raised")
	}
	r4cPost(t, map[string]any{"action": "profile", "label": "Unused copy",
		"points": []curvePoint{{50, 30}, {80, 100}}, "abort_c": 88, "cooldown_s": 45, "poll_s": 10, "smoothing": 1})
	if f := fanFloor(); f != 25 {
		t.Errorf("saving an unused library curve dropped the watchdog floor to %d %%", f)
	}
}

// Sound: targetFan (smoothing off) equals the curve value clamped to the floor; built-ins validate.
func TestR4C_BuiltinsValidAndTargetFan(t *testing.T) {
	r4cSetup(t)
	for _, k := range builtinOrder {
		fc, _ := builtinCurve(k)
		fc.Label = k
		if err := validateCurve(fc); err != nil {
			t.Errorf("%s: %v", k, err)
		}
		c := defaultFanConfig()
		c.Profile, c.Smoothing = k, 0
		if err := validateFanConfig(c); err != nil {
			t.Errorf("%s cfg: %v", k, err)
		}
		for tt := 30.0; tt <= 100; tt += 0.5 {
			got := targetFan(c, tt)
			if k == "steps-default" {
				if tt < 70 && got != -1 {
					t.Errorf("steps below first: %d", got)
				}
				continue
			}
			if got < fanMin || got > 100 {
				t.Errorf("%s %.1f: %d", k, tt, got)
			}
		}
	}
}
