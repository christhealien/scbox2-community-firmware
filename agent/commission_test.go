package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// commissionSetup: a Box II profile, a temp config dir, a known password, no real restart.
func commissionSetup(t *testing.T) func() {
	t.Helper()
	oldModel, oldAuth, oldRestart := model, *authFile, commissionRestart
	dir := t.TempDir()
	*authFile = filepath.Join(dir, "auth.json")
	model = profSCBox2
	commissionRestart = func() {}
	authMu.Lock()
	setPassword(&auth, "owner-pass-1")
	authMu.Unlock()
	return func() { model, *authFile, commissionRestart = oldModel, oldAuth, oldRestart }
}

func commissionPost(t *testing.T, body string) map[string]any {
	t.Helper()
	r := httptest.NewRequest("POST", "/api/commission", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handleCommission(w, r)
	var out map[string]any
	json.Unmarshal(w.Body.Bytes(), &out)
	return out
}

func TestCommissionBox2StartsAllOff(t *testing.T) {
	defer commissionSetup(t)()
	applyCommission()
	f := model.Feat
	if f.SoftIdle || f.PowerEstimate || f.ClockControl || f.VoltageControl || f.ChipTrims || f.CeilingProbe ||
		f.OverclockUnlock || f.Tuner || f.FirmwareUpdate || f.FanFit || f.FanPairOffset || f.HardwareWatchdog {
		t.Fatalf("a fresh Box II must have every control feature off: %+v", f)
	}
}

func TestCommissionApplyEnforcesRequirements(t *testing.T) {
	defer commissionSetup(t)()
	st := commissionState{Model: "scbox2", Features: map[string]commissionEntry{
		"Tuner": {On: true}, "ClockControl": {On: true}, // VoltageControl missing: the tuner must not come on
		"PowerEstimate": {On: true},
		"FanFit":        {On: true}, // never commissionable on the Box II
	}}
	b, _ := json.Marshal(st)
	os.WriteFile(commissionPath(), b, 0o600)
	applyCommission()
	if !model.Feat.ClockControl || !model.Feat.PowerEstimate {
		t.Fatalf("commissioned features not applied: %+v", model.Feat)
	}
	if model.Feat.Tuner {
		t.Fatal("tuner applied without voltage control")
	}
	if model.Feat.FanFit {
		t.Fatal("a feature that isn't commissionable on this model was switched on")
	}
}

func TestCommissionFileForOtherModelIgnored(t *testing.T) {
	defer commissionSetup(t)()
	b, _ := json.Marshal(commissionState{Model: "sclite", Features: map[string]commissionEntry{"ClockControl": {On: true}}})
	os.WriteFile(commissionPath(), b, 0o600)
	applyCommission()
	if model.Feat.ClockControl {
		t.Fatal("a file written for another model was applied")
	}
}

func TestCommissionPostOnNeedsPasswordAndRequirements(t *testing.T) {
	defer commissionSetup(t)()
	if out := commissionPost(t, `{"feature":"ClockControl","on":true,"password":"wrong"}`); out["ok"] == true {
		t.Fatal("switched on with a wrong password")
	}
	if out := commissionPost(t, `{"feature":"Tuner","on":true,"password":"owner-pass-1"}`); out["ok"] == true {
		t.Fatal("tuner switched on before its requirements")
	}
	if out := commissionPost(t, `{"feature":"FanFit","on":true,"password":"owner-pass-1"}`); out["ok"] == true {
		t.Fatal("a non-commissionable feature was accepted")
	}
	for _, k := range []string{"ClockControl", "VoltageControl", "Tuner"} {
		if out := commissionPost(t, `{"feature":"`+k+`","on":true,"password":"owner-pass-1","note":"test passed"}`); out["ok"] != true {
			t.Fatalf("switching %s on: %v", k, out)
		}
	}
	st := loadCommission()
	if !st.Features["Tuner"].On || st.Features["Tuner"].At == "" || st.Model != "scbox2" {
		t.Fatalf("saved state wrong: %+v", st)
	}
	// switching voltage control off takes the tuner (which needs it) off too, with no password
	if out := commissionPost(t, `{"feature":"VoltageControl","on":false}`); out["ok"] != true {
		t.Fatalf("switching off: %v", out)
	}
	st = loadCommission()
	if st.Features["VoltageControl"].On || st.Features["Tuner"].On || !st.Features["ClockControl"].On {
		t.Fatalf("off didn't cascade correctly: %+v", st.Features)
	}
}

func TestCommissionNothingOnSCLite(t *testing.T) {
	defer commissionSetup(t)()
	model = profSCLite
	before := model.Feat
	applyCommission()
	if model.Feat != before {
		t.Fatal("commissioning changed the SC Lite profile")
	}
	if v := commissionView(); v["available"] != false {
		t.Fatalf("SC Lite should have nothing to commission: %v", v)
	}
}
