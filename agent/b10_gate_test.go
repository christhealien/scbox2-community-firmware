package main

// B10: feature-gating for every hardware-control feature left ungated by A1 — chip trims (manual +
// auto-trim + strong-chip test), the limit/ceiling probe, the power estimate, and the tuner sub-handlers.
// On the SC Box II profile each must refuse cleanly with the standard "not available on this model" message,
// no background loop may start, and the chip-trim hook flag must never be written. On SC Lite every feature
// must still be reachable (the gate lets it through).

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// b10SetModel switches the active profile for a test and restores it (and the window vars) after.
func b10SetModel(t *testing.T, p modelProfile) {
	t.Helper()
	oldModel, oldWhy := model, modelWhy
	model, modelWhy = p, "b10 test"
	setWindowsFromModel()
	t.Cleanup(func() { model, modelWhy = oldModel, oldWhy; setWindowsFromModel() })
}

// b10TempAuth points the config dir (chiphook/chiptrim/power files) at a fresh temp dir and a dead API.
func b10TempAuth(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	oldAuth, oldAPI := *authFile, *apiAddr
	*authFile = filepath.Join(dir, "auth.json")
	*apiAddr = "127.0.0.1:1" // refused immediately: no backend, so no slow timeouts
	t.Cleanup(func() { *authFile = oldAuth; *apiAddr = oldAPI })
	return dir
}

func b10call(t *testing.T, h http.HandlerFunc, method, url string, body any) map[string]any {
	t.Helper()
	var r *http.Request
	if body != nil {
		b, _ := json.Marshal(body)
		r = httptest.NewRequest(method, url, bytes.NewReader(b))
	} else {
		r = httptest.NewRequest(method, url, nil)
	}
	w := httptest.NewRecorder()
	h(w, r)
	var out map[string]any
	json.Unmarshal(w.Body.Bytes(), &out)
	return out
}

// isNotAvailable: the standard A1 refusal shape {ok:false, disabled:true, err:"... not available on this model ..."}.
func isNotAvailable(m map[string]any) bool {
	if m == nil {
		return false
	}
	ok, _ := m["ok"].(bool)
	dis, _ := m["disabled"].(bool)
	errs, _ := m["err"].(string)
	return !ok && dis && bytes.Contains([]byte(errs), []byte("not available on this model"))
}

// --- every gated handler refuses cleanly on Box II, and is let through on SC Lite ---

func TestB10GatedHandlersBox2Refuse(t *testing.T) {
	b10TempAuth(t)
	b10SetModel(t, profSCBox2)
	cases := []struct {
		name        string
		h           http.HandlerFunc
		method, url string
		body        any
	}{
		{"chiptrim-get", handleChipTrim, "GET", "/api/chiptrim", nil},
		{"chiptrim-manual", handleChipTrim, "POST", "/api/chiptrim", map[string]any{"chain": 0, "chip": 1, "mhz": -12.5}},
		{"chiptrim-enable", handleChipTrim, "POST", "/api/chiptrim", map[string]any{"enable": true}},
		{"chiptrim-autotrim", handleChipTrim, "POST", "/api/chiptrim", map[string]any{"watch": true}},
		{"strongtest-get", handleStrongTest, "GET", "/api/strongtest", nil},
		{"strongtest-start", handleStrongTest, "POST", "/api/strongtest", map[string]any{"start": true, "chain": 0}},
		{"ceiling-get", handleCeiling, "GET", "/api/ceiling", nil},
		{"ceiling-start", handleCeiling, "POST", "/api/ceiling", map[string]any{"start": true}},
		{"power-get", handlePower, "GET", "/api/power", nil},
		{"power-calibrate", handlePower, "POST", "/api/power", map[string]any{"cal_watts": 600}},
		{"tuner-export", handleTunerExport, "GET", "/api/tuner/export?kind=log", nil},
		{"tuner-chipmap", handleTunerChipMap, "GET", "/api/tuner/chipmap", nil},
	}
	for _, c := range cases {
		got := b10call(t, c.h, c.method, c.url, c.body)
		if !isNotAvailable(got) {
			t.Errorf("%s: want clean not-available refusal, got %v", c.name, got)
		}
	}
	// the model name must appear so the owner sees which model it is
	got := b10call(t, handleChipTrim, "GET", "/api/chiptrim", nil)
	if errs, _ := got["err"].(string); !bytes.Contains([]byte(errs), []byte("SC Box II")) {
		t.Errorf("refusal should name the model: %q", errs)
	}
}

func TestB10GatedHandlersSCLitePassThrough(t *testing.T) {
	b10TempAuth(t)
	b10SetModel(t, profSCLite)
	// GET status paths need no backend; under SC Lite the gate must let them through (never "disabled").
	for _, c := range []struct {
		name string
		h    http.HandlerFunc
		url  string
	}{
		{"chiptrim", handleChipTrim, "/api/chiptrim"},
		{"strongtest", handleStrongTest, "/api/strongtest"},
		{"ceiling", handleCeiling, "/api/ceiling"},
	} {
		got := b10call(t, c.h, "GET", c.url, nil)
		if dis, _ := got["disabled"].(bool); dis {
			t.Errorf("%s: SC Lite must not be gated off: %v", c.name, got)
		}
		if ok, _ := got["ok"].(bool); !ok {
			t.Errorf("%s: SC Lite GET should answer ok:true: %v", c.name, got)
		}
	}
}

// --- power estimate: no SC-Lite constants fed on Box II; SC Lite still computes ---

func TestB10PowerViewBox2NoEstimate(t *testing.T) {
	b10SetModel(t, profSCBox2)
	// a dashboard carrying a VoltageV that on Box II is a PWM-trim level, not volts: it must never reach the
	// estimator. powerView must early-return with no figure, so the level field is never used as a voltage.
	d := dashboard{OK: true, GHS: 100, Boards: []board{{ID: 0, Alive: true, GHS: 100, VoltageV: 42}}}
	v := powerView(d)
	if avail, _ := v["available"].(bool); avail {
		t.Fatalf("Box II powerView must report no estimate: %v", v)
	}
	if _, computed := v["watts"]; computed {
		t.Fatalf("Box II powerView must not compute watts (SC Lite 899W/625MHz/PV9400 model is meaningless here): %v", v)
	}
	if _, usedVolt := v["pv_eff"]; usedVolt {
		t.Fatalf("Box II powerView must not derive a PV from the voltage-level field: %v", v)
	}
}

func TestB10PowerViewSCLiteUnchanged(t *testing.T) {
	b10TempAuth(t)
	b10SetModel(t, profSCLite)
	// not hashing (GHS 0): the SC Lite path returns the config map (no "available" marker) — proving the
	// early-return is Box-II-only and SC Lite behaviour is unchanged.
	v := powerView(dashboard{OK: true, GHS: 0})
	if _, blocked := v["available"]; blocked {
		t.Fatalf("SC Lite powerView must not carry the Box II no-estimate marker: %v", v)
	}
	if _, hasCal := v["calibrated"]; !hasCal {
		t.Fatalf("SC Lite powerView should still return its config view: %v", v)
	}
}

// --- the chip-trim hook flag file is NEVER written on Box II ---

func TestB10ChipHookFlagNeverWrittenBox2(t *testing.T) {
	b10TempAuth(t)
	b10SetModel(t, profSCBox2)
	// the handler-level gate: a POST that would switch the hook on is refused before any write.
	got := b10call(t, handleChipTrim, "POST", "/api/chiptrim", map[string]any{"enable": true})
	if !isNotAvailable(got) {
		t.Fatalf("enable on Box II must refuse: %v", got)
	}
	if _, err := os.Stat(chipHookFlag()); err == nil {
		t.Fatalf("chip-trim hook flag was written on Box II (%s): the intminer wrapper would preload the trim lib", chipHookFlag())
	}
	// defense-in-depth: the write site itself refuses even if reached directly (bypassing the handler gate).
	w := httptest.NewRecorder()
	enable := true
	cwMu.Lock()
	note, done := chipTrimChange(w, &enable, 0, 0, nil, false, false)
	cwMu.Unlock()
	if !done {
		t.Fatalf("chipTrimChange enable must refuse at the write site on Box II (note=%q)", note)
	}
	if _, err := os.Stat(chipHookFlag()); err == nil {
		t.Fatalf("write-site guard failed: hook flag written on Box II")
	}
}

func TestB10ChipHookFlagWritableSCLite(t *testing.T) {
	b10TempAuth(t)
	b10SetModel(t, profSCLite)
	// prove the guard is Box-II-only: under SC Lite the write-site guard does NOT short-circuit. With no
	// backend reachable, reapplySetClock returns (0,false) and, with no trims on file, the enable path reaches
	// the flag write (os.WriteFile) — the flag ends up present. (SC Lite behaviour, unchanged by B10.)
	if err := writeChipTrims(nil); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	enable := true
	// guard against restartMiner side effects in the unit test by neutralising the reboot marker path only
	cwMu.Lock()
	_, done := chipTrimChange(w, &enable, 0, 0, nil, false, false)
	cwMu.Unlock()
	if done {
		var out map[string]any
		json.Unmarshal(w.Body.Bytes(), &out)
		if isNotAvailable(out) {
			t.Fatalf("SC Lite must not be gated at the hook-flag write site: %v", out)
		}
	}
	if _, err := os.Stat(chipHookFlag()); err != nil {
		t.Fatalf("SC Lite: enable should have written the hook flag, but it is absent (%v)", err)
	}
}

// --- background loops / recovery do not start or do work on Box II ---

func TestB10ChipWatchLoopDoesNotStartBox2(t *testing.T) {
	b10SetModel(t, profSCBox2)
	done := make(chan struct{})
	go func() { chipWatchLoop(); close(done) }()
	select {
	case <-done: // early-returned immediately: the auto-trim watchdog never runs on Box II
	case <-time.After(2 * time.Second):
		t.Fatal("chipWatchLoop did not early-return on Box II: the auto-trim background loop would run")
	}
}

func TestB10ChipWatchLoopRunsSCLite(t *testing.T) {
	b10TempAuth(t)
	b10SetModel(t, profSCLite)
	// under SC Lite the loop proceeds past the gate (it then blocks on its ticker Sleep). Prove it did NOT
	// early-return by observing it is still running after a short wait.
	oldEvery := cwEvery
	cwEvery = time.Hour // make the first sleep long so the goroutine is still alive if it passed the gate
	t.Cleanup(func() { cwEvery = oldEvery })
	t.Setenv(brandEnv("AUTOTRIM_EVERY"), "") // don't let env override to something tiny
	done := make(chan struct{})
	go func() { chipWatchLoop(); close(done) }()
	select {
	case <-done:
		t.Fatal("chipWatchLoop returned immediately on SC Lite: the gate must not block SC Lite")
	case <-time.After(300 * time.Millisecond): // still sleeping in the loop: it passed the gate (correct)
	}
}

func TestB10SctRecoverNoWorkBox2(t *testing.T) {
	dir := b10TempAuth(t)
	b10SetModel(t, profSCBox2)
	// plant a leftover backup + result as if a test had been cut short. On Box II sctRecover must early-return
	// and touch nothing (no test can ever have run).
	bk := strongBackup()
	if err := os.WriteFile(bk, []byte("0 1 -12.5\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(strongResult(), []byte(`{"running":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	sctRecover()
	if _, err := os.Stat(bk); err != nil {
		t.Fatalf("sctRecover did work on Box II (removed the backup): it must early-return. dir=%s", dir)
	}
}

func TestB10TunerRecoverNoWorkBox2(t *testing.T) {
	b10TempAuth(t)
	b10SetModel(t, profSCBox2)
	mk := tunerMarkerPath()
	if err := os.WriteFile(mk, []byte(`{"started":1,"base":{"mhz":690,"mv":42,"pv":0}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	tunerRecover(0)
	// on Box II it must early-return before clearing/processing the marker (neither tuner nor ceiling can run)
	if _, err := os.Stat(mk); err != nil {
		t.Fatalf("tunerRecover processed a marker on Box II: it must early-return")
	}
}
