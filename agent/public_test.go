//go:build public

package main

// The public release (-tags scbox2,public). Run with: go test -tags scbox2,public -run TestPublic .
// (The rest of the suite tests the owner's commissioning build and isn't meant to run under this tag.)

import (
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
	if f.CeilingProbe || f.OverclockUnlock || f.Tuner {
		t.Fatalf("C6, C7 and C8 must be off: %+v", f)
	}
	if model.OCHardMax != 0 {
		t.Fatalf("no overclock ceiling in the public release, got %d", model.OCHardMax)
	}
	var on []string
	for k := range commissionApplied {
		on = append(on, k)
	}
	sort.Strings(on)
	if strings.Join(on, ",") != "ChipTrims,ClockControl,FirmwareUpdate,PowerEstimate,SoftIdle,VoltageControl" {
		t.Fatalf("applied: %v", on)
	}
}

func TestPublicNoCommissioning(t *testing.T) {
	defer commissionSetup(t)()
	applyCommission()
	v := commissionView()
	if v["available"] != false || v["public"] != true || len(v["features"].([]map[string]any)) != 0 {
		t.Fatalf("view must offer nothing: %v", v)
	}
	out := commissionPost(t, `{"feature":"Tuner","on":true,"password":"owner-pass-1"}`)
	if out["ok"] != false {
		t.Fatalf("a switch must be refused: %v", out)
	}
	if model.Feat.Tuner {
		t.Fatal("the tuner came on")
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
