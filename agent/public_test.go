//go:build public

package main

// The public release (-tags scbox2,public). Run with: go test -tags scbox2,public -run TestPublic .
// (The rest of the suite tests the owner's commissioning build and isn't meant to run under this tag.)

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

func TestPublicFixedFeatures(t *testing.T) {
	defer commissionSetup(t)()
	// an owner's earlier commissioning file (C6/C7/C8 on, C2 off) must change nothing in the public release
	saved := `{"model":"scbox2","features":{"CeilingProbe":{"on":true},"OverclockUnlock":{"on":true},"Tuner":{"on":true},"ClockControl":{"on":true}}}`
	if err := os.WriteFile(commissionPath(), []byte(saved), 0o600); err != nil {
		t.Fatal(err)
	}
	applyCommission()
	f := model.Feat
	if !f.SoftIdle || !f.PowerEstimate || !f.ClockControl || !f.VoltageControl || !f.ChipTrims || !f.FirmwareUpdate {
		t.Fatalf("C1, C2, C3, C4, C5 and C9 must be on: %+v", f)
	}
	if !f.CeilingProbe || !f.OverclockUnlock {
		t.Fatalf("the ceiling probe (C6) and the overclock (C7) must be on: %+v", f)
	}
	if !f.Tuner {
		t.Fatalf("the tuner (C8, public from v0.5.4) must be on: %+v", f)
	}
	if model.OCHardMax != 750 {
		t.Fatalf("the public release's overclock cap is 750 MHz, got %d", model.OCHardMax)
	}
	var on []string
	for k := range commissionApplied {
		on = append(on, k)
	}
	sort.Strings(on)
	if strings.Join(on, ",") != "CeilingProbe,ChipTrims,ClockControl,FirmwareUpdate,OverclockUnlock,PowerEstimate,SoftIdle,Tuner,VoltageControl" {
		t.Fatalf("applied: %v", on)
	}
}

// The public release exposes the probe, the unlock and (v0.5.4) the tuner, but not Commissioning; the unlock keeps the
// per-unit rule (no probe result: refused), and the tuner's clocks stay inside that same per-unit window.
func TestPublicProbeUnlockTuner(t *testing.T) {
	defer commissionSetup(t)()
	t.Cleanup(setWindowsFromModel) // (after the deferred restore of the model)
	applyCommission()
	setWindowsFromModel()
	call := func(h func(http.ResponseWriter, *http.Request), method, body string) map[string]any {
		rr := httptest.NewRecorder()
		var rd io.Reader
		if body != "" {
			rd = strings.NewReader(body)
		}
		h(rr, httptest.NewRequest(method, "/x", rd))
		var out map[string]any
		json.Unmarshal(rr.Body.Bytes(), &out)
		return out
	}
	if r := call(handleCeiling, "GET", ""); r["ok"] != true || r["disabled"] == true || r["unlock_err"] == "" {
		t.Fatalf("the probe must be there: %v", r)
	}
	if r := call(handleOverclock, "POST", `{"acknowledge":true}`); r["ok"] != false || r["need_probe"] != true {
		t.Fatalf("the unlock without a probe result: %v", r)
	}
	if r := call(handleTuner, "GET", ""); r["disabled"] == true || r["ok"] == false {
		t.Fatalf("the tuner must be there: %v", r)
	}
	if v := commissionView(); v["available"] != false || len(v["features"].([]map[string]any)) != 0 {
		t.Fatalf("no Commissioning: %v", v)
	}
	if err := box2SaveProbe(box2ProbeRecord{Cap: 720, FailAt: 725, Why: "hardware errors", Top: 750, Level: 42}); err != nil {
		t.Fatal(err)
	}
	if r := call(handleOverclock, "POST", `{"acknowledge":true}`); r["ok"] != true {
		t.Fatalf("the unlock with a clean probe result: %v", r)
	}
	if hi := clockWindowHi(); hi != 720 {
		t.Fatalf("public window %d, want this unit's 720", hi)
	}
	// the tuner: up to this unit's probe result and never above it
	c := defaultTunerConfig()
	c.LowestMHz, c.HighestMHz, c.Step = 690, 720, 12.5
	if err := validateTunerConfig(c); err != nil {
		t.Fatalf("a tuner run up to the unlocked 720 MHz: %v", err)
	}
	c.HighestMHz = 725
	if err := validateTunerConfig(c); err == nil {
		t.Fatal("a tuner run above this unit's probe result (725 > 720 MHz) was accepted")
	}
}

// The public tuner: the fan hold aims at most at 74 °C, nothing it says names Commissioning or a test lane, and
// no switch is needed to start it.
func TestPublicTunerHoldCapNoCommissioning(t *testing.T) {
	useBox2(t)
	defer commissionSetup(t)()
	applyCommission()
	setWindowsFromModel()
	if !model.Feat.Tuner {
		t.Fatal("the tuner is off in the public release")
	}
	if box2HoldMax() != 74 {
		t.Fatalf("the hold's highest target is %v °C, want 74", box2HoldMax())
	}
	c := defaultTunerConfig()
	if c.FanTargetC != 72 || c.WarmestC > 74 {
		t.Fatalf("defaults: target %v, warmest %v", c.FanTargetC, c.WarmestC)
	}
	c.FanHold, c.FanTargetC = true, 74
	if err := validateTunerConfig(c); err != nil {
		t.Fatalf("a 74 °C hold: %v", err)
	}
	c.FanTargetC = 75
	if err := validateTunerConfig(c); err == nil || !strings.Contains(err.Error(), "45–74") {
		t.Fatalf("a 75 °C hold must be refused with the 74 °C cap: %v", err)
	}
	bad := regexp.MustCompile(`(?i)commission|test C\d|lane C\d`)
	info, _ := json.Marshal(box2TunerInfo())
	if m := bad.FindString(string(info) + box2CurveDesc(c)); m != "" {
		t.Errorf("the public tuner's texts say %q: %s", m, info)
	}
	rr := httptest.NewRecorder()
	handleTuner(rr, httptest.NewRequest("GET", "/api/tuner", nil))
	if b := rr.Body.String(); bad.MatchString(b) || strings.Contains(b, `"disabled":true`) {
		t.Errorf("/api/tuner in the public release: %s", b)
	}
}

func TestPublicNoCommissioning(t *testing.T) {
	defer commissionSetup(t)()
	applyCommission()
	v := commissionView()
	if v["available"] != false || v["public"] != true || len(v["features"].([]map[string]any)) != 0 {
		t.Fatalf("view must offer nothing: %v", v)
	}
	out := commissionPost(t, `{"feature":"Tuner","on":false,"password":"owner-pass-1"}`)
	if out["ok"] != false {
		t.Fatalf("a switch must be refused: %v", out)
	}
	if !model.Feat.Tuner {
		t.Fatal("the tuner (always on in the public release) went off")
	}
	w := httptest.NewRecorder()
	handleModel(w, httptest.NewRequest("GET", "/api/model", nil))
	if b := w.Body.String(); !strings.Contains(b, `"public":true`) {
		t.Fatalf("/api/model must say public: %s", w.Body.String())
	}
}

// The Go texts a public user can meet carry no Commissioning and no "unverified" test status (the stop is on).
func TestPublicGoTextsNoCommissioning(t *testing.T) {
	useBox2(t)
	defer commissionSetup(t)()
	applyCommission()
	bad := regexp.MustCompile(`(?i)commission|hardware test|test C\d|lane C\d|not tested|unproven|unverified`)
	for what, s := range map[string]string{
		"ignored stop":     box2IgnoredMsg("the work counter kept rising"),
		"failed stop note": box2StopFailNote(),
		"idle note":        idleActionNote(),
		"heat line":        heatCoolLine(),
	} {
		if m := bad.FindString(s); m != "" {
			t.Errorf("public %s says %q: %s", what, m, s)
		}
	}
	if !strings.Contains(box2IgnoredMsg("x"), "Please report it") {
		t.Errorf("public ignored-stop alert: %s", box2IgnoredMsg("x"))
	}
}
