package main

// R14 + A1: the plan-format abstraction on the SC Box II profile. The panel parses the Box II's stock
// /mcb/setting (cpbsetting.json, 2.2.1), rebuilds its plan strings byte-for-byte, and refuses anything
// that is not one of the two stock tuples.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const box2Setting = `{"name":"Benchmark","powerplans":[
 {"info":"690 MHz 42 V 50 RPM 41 RPM","level":0,"temp_target":77.0,"prevoltage":40.00001,"voltage":42.00001},
 {"info":"525 MHz 61 V 40 RPM 59 RPM","level":2,"temp_target":73.0,"prevoltage":59.00001,"voltage":61.00001}],
 "select":0,"tempcontrol":true,"ledcontrol":false,"manual":false,
 "manualPowerplan":"690 MHz 42 V 50 RPM 41 RPM","version":"v1.2"}`

// useBox2 switches the active profile to SC Box II for a test and restores SC Lite after.
func useBox2(t *testing.T) {
	t.Helper()
	model, modelWhy = profSCBox2, "test"
	setWindowsFromModel()
	t.Cleanup(func() { model, modelWhy = profSCLite, "test"; setWindowsFromModel() })
}

func TestR14Box2Parse(t *testing.T) {
	useBox2(t)
	// a float "V" trim must be ACCEPTED (its integer part is what matches the stock tuple)
	for _, c := range []struct {
		s          string
		freq, vrun int
	}{
		{"690 MHz 42.000011 V 50 RPM 41 RPM", 690, 42},
		{"525 MHz 61.000000 V 40 RPM 59 RPM", 525, 61},
		{"690 MHz 42 V 50 RPM 41 RPM", 690, 42}, // the stock info strings are integers
	} {
		p, ok := parsePowerplan(c.s)
		if !ok {
			t.Fatalf("parse %q failed", c.s)
		}
		if p.FreqMHz != c.freq || p.VrunMv != c.vrun || p.PVMv != 0 {
			t.Fatalf("parse %q -> %+v, want freq=%d vrun=%d pv=0", c.s, p, c.freq, c.vrun)
		}
		if err := validateControl(p); err != nil {
			t.Fatalf("stock plan %q rejected: %v", c.s, err)
		}
		t.Logf("parse %q -> %+v ; validateControl=ok", c.s, p)
	}
	// the integer stock info strings round-trip byte-for-byte through parse→build
	for _, s := range []string{"690 MHz 42 V 50 RPM 41 RPM", "525 MHz 61 V 40 RPM 59 RPM"} {
		if got := buildPowerplan(mustParse(t, s)); got != s {
			t.Fatalf("round-trip: %q -> %q", s, got)
		}
	}
	// box2WithFans preserves the exact "V" trim text (float or integer), swapping only the fans
	if got, ok := box2WithFans("690 MHz 42.000011 V 50 RPM 41 RPM", 60, 60); !ok || got != "690 MHz 42.000011 V 60 RPM 60 RPM" {
		t.Fatalf("box2WithFans float trim: %q ok=%v", got, ok)
	}
	// a non-stock clock/V is refused, not clamped
	for _, bad := range []string{
		"700 MHz 42 V 50 RPM 41 RPM", // clock not stock
		"690 MHz 50 V 50 RPM 41 RPM", // V not stock
		"525 MHz 42 V 40 RPM 59 RPM", // wrong V for this clock
	} {
		p, ok := parsePowerplan(bad)
		if !ok {
			t.Fatalf("parse %q failed", bad)
		}
		if err := validateControl(p); err == nil {
			t.Fatalf("non-stock plan %q accepted", bad)
		}
	}
}

func mustParse(t *testing.T, s string) powerplan {
	t.Helper()
	p, ok := parsePowerplan(s)
	if !ok {
		t.Fatalf("parse %q failed", s)
	}
	return p
}

func TestR14Box2MutatePlan(t *testing.T) {
	useBox2(t)
	puts := 0
	var put map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/user/login"):
			json.NewEncoder(w).Encode(map[string]any{"JWT Token": "tok"})
		case r.Method == "GET":
			io.WriteString(w, box2Setting)
		case r.Method == "PUT":
			puts++
			b, _ := io.ReadAll(r.Body)
			json.Unmarshal(b, &put)
			io.WriteString(w, `{"status":0}`)
		}
	}))
	defer srv.Close()
	*minerWeb = strings.TrimPrefix(srv.URL, "http://")
	jwtToken = ""
	authMu.Lock()
	auth.MinerPass = "pw"
	authMu.Unlock()
	// a fan-only change (what the fan curve / fan controls do): allowed, keeps the stock clock/V
	plan, err := mutatePlanNoWake(func(p *powerplan) { p.Fan1, p.Fan2 = 60, 60 })
	if err != nil {
		t.Fatalf("fan-only mutate refused: %v", err)
	}
	if puts != 1 {
		t.Fatalf("fan-only mutate: puts=%d want 1", puts)
	}
	// stock plan's own V text ("42") preserved; only the fan field changed — the 4th field (the second trim
	// PWM, not a fan) stays the stock plan's 41 whatever the caller asked for
	if plan != "690 MHz 42 V 60 RPM 41 RPM" {
		t.Fatalf("fan-only mutate built wrong plan: %q", plan)
	}
	if mp, _ := put["manualPowerplan"].(string); mp != plan {
		t.Fatalf("PUT manualPowerplan %q != built plan %q", mp, plan)
	}
	t.Logf("fan-only mutate: plan=%q puts=%d", plan, puts)
}

// a clock/V change away from a stock tuple is refused outright, with zero PUTs reaching the miner.
func TestR14Box2MutateRefusesNonStock(t *testing.T) {
	useBox2(t)
	puts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/user/login"):
			json.NewEncoder(w).Encode(map[string]any{"JWT Token": "tok"})
		case r.Method == "GET":
			io.WriteString(w, box2Setting)
		case r.Method == "PUT":
			puts++
			io.WriteString(w, `{"status":0}`)
		}
	}))
	defer srv.Close()
	*minerWeb = strings.TrimPrefix(srv.URL, "http://")
	jwtToken = ""
	authMu.Lock()
	auth.MinerPass = "pw"
	authMu.Unlock()
	for _, mut := range []func(*powerplan){
		func(p *powerplan) { p.FreqMHz = 700 },               // non-stock clock
		func(p *powerplan) { p.VrunMv = 50 },                 // non-stock V (trim)
		func(p *powerplan) { p.FreqMHz, p.VrunMv = 600, 50 }, // neither stock
	} {
		if _, err := mutatePlan(mut); err == nil {
			t.Fatalf("non-stock mutate accepted")
		}
	}
	if puts != 0 {
		t.Fatalf("non-stock mutates wrote to the miner: puts=%d want 0", puts)
	}
}

func TestR14Box2Idle(t *testing.T) {
	useBox2(t)
	var cur map[string]any
	if err := json.Unmarshal([]byte(box2Setting), &cur); err != nil {
		t.Fatal(err)
	}
	// Box II stock plans have no 0 MHz idle plan: idleLevel is -1, firstHashingPlan is the first entry.
	if il := idleLevel(cur); il != -1 {
		t.Fatalf("idleLevel=%d want -1 (Box II has no idle plan)", il)
	}
	if fh := firstHashingPlan(cur); fh != 0 {
		t.Fatalf("firstHashingPlan=%d want 0", fh)
	}
	// idleAction on the STOCK Box II profile (lane B3): SoftIdle ships OFF (conservative), so the unproven
	// miner-API stop is not sent; it reports it could not park and names the 86°C cut-off backstop. The
	// real pgadisable path (SoftIdle on) is covered in b3_idleaction_test.go.
	if err := idleAction(profSCBox2); err == nil || !strings.Contains(err.Error(), idleActionUnprovenMsg) {
		t.Fatalf("idleAction(Box2) = %v, want the UNPROVEN backstop caveat", err)
	}
	t.Logf("idleLevel=%d firstHashingPlan=%d", idleLevel(cur), firstHashingPlan(cur))
}
