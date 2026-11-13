package main

// B9: the web UI is model-aware. These tests prove (1) the /api/model JSON the UI reads carries the
// right figures and feature gates with the exact key names the front-end uses, and (2) the served page
// keeps the SC Lite template intact (no hard-coded SC Box II leakage; the model-driven helpers and the
// monitoring-mode banner container are present and start hidden). The byte-identical rendered-UI
// regression for SC Lite and the Box II hiding are proven at runtime by the Playwright harness in
// $P/B9/tests; this file nails down the data contract and the static template.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// callModel serves /api/model for the given profile and returns the decoded JSON map.
func callModel(t *testing.T, p modelProfile) map[string]any {
	t.Helper()
	model, modelWhy = p, "test"
	setWindowsFromModel()
	rec := httptest.NewRecorder()
	handleModel(rec, httptest.NewRequest(http.MethodGet, "/api/model", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/api/model status = %d", rec.Code)
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("/api/model not JSON: %v", err)
	}
	return m
}

// The JS reads these exact feature keys (Go field names, no json tags). If A1 ever renames a field the
// UI gates would silently stop working, so pin the contract here.
var uiFeatureKeys = []string{
	"ClockControl", "VoltageControl", "Tuner", "CeilingProbe", "OverclockUnlock",
	"ChipTrims", "FanPairOffset", "FanFit", "PowerEstimate", "HardwareWatchdog", "FirmwareUpdate",
}

func TestB9ModelJSONContract(t *testing.T) {
	m := callModel(t, profSCBox2)
	if m["name"] != "SC Box II" {
		t.Errorf("name = %v, want SC Box II", m["name"])
	}
	if m["id"] != "scbox2" {
		t.Errorf("id = %v, want scbox2", m["id"])
	}
	// temperature limits the still-visible fan-curve panel reads (MCUTOFF/MABORT in the JS)
	if got, _ := m["temp_cutoff"].(float64); got != 86 {
		t.Errorf("temp_cutoff = %v, want 86", m["temp_cutoff"])
	}
	if got, _ := m["abort_max"].(float64); got != 84 {
		t.Errorf("abort_max = %v, want 84", m["abort_max"])
	}
	if got, _ := m["fans"].(float64); got != 2 {
		t.Errorf("fans = %v, want 2", m["fans"])
	}
	feats, ok := m["features"].(map[string]any)
	if !ok {
		t.Fatalf("features missing/!object: %T", m["features"])
	}
	for _, k := range uiFeatureKeys {
		v, present := feats[k]
		if !present {
			t.Errorf("features.%s absent — UI gate would break", k)
			continue
		}
		if b, _ := v.(bool); b {
			t.Errorf("features.%s = true on Box II, want false (all control features off)", k)
		}
	}
}

func TestB9SCLiteModelJSONUnchanged(t *testing.T) {
	m := callModel(t, profSCLite)
	if m["name"] != "SC Lite" {
		t.Errorf("name = %v, want SC Lite", m["name"])
	}
	// MCUTOFF()/MABORT() in the JS fall back to these, so the fan-curve safety text renders exactly
	// "70–90 °C (the OEM firmware takes over at 92 °C)".
	if got, _ := m["temp_cutoff"].(float64); got != 92 {
		t.Errorf("temp_cutoff = %v, want 92", m["temp_cutoff"])
	}
	if got, _ := m["abort_max"].(float64); got != 90 {
		t.Errorf("abort_max = %v, want 90", m["abort_max"])
	}
	feats, _ := m["features"].(map[string]any)
	for _, k := range uiFeatureKeys {
		if b, _ := feats[k].(bool); !b {
			t.Errorf("features.%s = false on SC Lite, want true (unchanged behaviour)", k)
		}
	}
}

// TestB9ServedTemplateNoLeak: the single served page is model-independent (model facts are applied at
// runtime from /api/model). It must keep the SC Lite brand/title verbatim, carry the model-aware JS and
// the hidden banner container, and contain NO hard-coded SC Box II text — so SC Lite is served exactly
// as before and Box II text only ever appears via JS.
func TestB9ServedTemplateNoLeak(t *testing.T) {
	model, modelWhy = profSCLite, "test"
	rec := httptest.NewRecorder()
	handleIndex(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("index status = %d", rec.Code)
	}
	page := rec.Body.String()

	mustContain := []string{
		"<title>" + brandProduct + "</title>", // this firmware's title verbatim ("SC Lite" on the default build)
		`class="brand">` + brandHeader,        // this firmware's header brand verbatim ("SC&nbsp;LITE")
		`id="modelbanner" class="hide"`,       // banner container, starts hidden+empty
		"function applyModel(",                // model-aware driver present
		"function featOff2(",                  // feature gate helper present
		"function renderModelBanner(",         // Box II banner builder present
		"function settingsExport(",            // export/import replaces in-panel update (B7 coord)
		"/api/settings/export",                // export endpoint the UI calls
		"/api/settings/import",                // import endpoint the UI calls
		"MABORT()", "MCUTOFF()",               // temperature limits now model-driven
	}
	for _, s := range mustContain {
		if !strings.Contains(page, s) {
			t.Errorf("served page missing %q", s)
		}
	}
	// No SC Box II facts may be baked into the template: they must come only from /api/model at runtime. The
	// SC Box II firmware's own build carries its product name (title, header) — its hardware facts still may not.
	leaks := []string{"SCBOX2", "ICT580", "4500"}
	if brand != "scbox2" {
		leaks = append(leaks, "SC Box II", "SC Box Ⅱ")
	}
	for _, s := range leaks {
		if strings.Contains(page, s) {
			t.Errorf("served template leaks Box II literal %q — must be runtime-only", s)
		}
	}
}
