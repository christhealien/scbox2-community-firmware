package main

// Box II constants groom: nothing the SC Box II uses comes from the SC Lite. The voltage is the OEM's level
// (42–61, a higher level is a lower voltage), there is no PV, the overclock ceiling is undecided (C6/C7 refuse
// above 690 MHz), and with C2 off no Box II path reports watts or J/TH.

import (
	"encoding/json"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func TestBox2ProfileFacts(t *testing.T) {
	p := profSCBox2
	if p.Boards != 1 || p.ChipsPerBoard != 36 || p.ChipModel != "ICT580" || p.FreqMin != 525 || p.FreqMax != 690 || p.FreqHardMax != 690 {
		t.Fatalf("geometry/clock: %+v", p)
	}
	if p.PVMin != 0 || p.PVMax != 0 || p.VrunMin != 0 || p.VrunMax != 0 || p.PVGap != 0 || p.PowerRefW != 0 {
		t.Fatalf("no PV / mV / SC Lite power reference on the Box II: %+v", p)
	}
	if p.VCodeMin != 42 || p.VCodeMax != 61 || p.VCodeDir != vcodeUpIsLess {
		t.Fatalf("voltage levels 42–61, a higher level is a lower voltage (C4): %+v", p)
	}
	// the overclock ceiling: none in the public release; the owner's first C6 ceiling (725 MHz) in the owner's build
	if want := map[bool]int{true: 0, false: 725}[publicBuild]; p.OCHardMax != want {
		t.Fatalf("overclock ceiling (public %v): want %d, got %d", publicBuild, want, p.OCHardMax)
	}
	// the SC Lite keeps its figures
	l := profSCLite
	if l.PVMin != 9100 || l.PVMax != 9600 || l.VrunMin != 8800 || l.VrunMax != 9300 || l.FreqHardMax != 900 || l.VCodeDir != vcodeDirUnknown {
		t.Fatalf("SC Lite profile changed: %+v", l)
	}
}

// C6 and C7 refuse anything above 690 MHz on the real profile, with every gate commissioned.
func TestBox2NoClockAbove690(t *testing.T) {
	b10TempAuth(t)
	p := profSCBox2
	p.OCHardMax = 0 // an undecided ceiling (the public release's): nothing above 690 even with every gate on
	p.Feat.ClockControl, p.Feat.VoltageControl, p.Feat.CeilingProbe, p.Feat.OverclockUnlock = true, true, true, true
	b10SetModel(t, p)
	if freqHardMax != 690 {
		t.Fatalf("unlocked window %d, want 690", freqHardMax)
	}
	if err := saveOverclock(overclockState{Acknowledged: true}); err != nil {
		t.Fatal(err)
	}
	if hi := clockWindowHi(); hi != 690 {
		t.Fatalf("clock window with the unlock accepted: %d, want 690", hi)
	}
	err := ceilBox2StartErr(true)
	if err == nil || !strings.Contains(err.Error(), "isn't decided") {
		t.Fatalf("the probe must refuse: %v", err)
	}
	if err := box2OverclockErr(powerplan{FreqMHz: 700, VrunMv: 42}, powerplan{FreqMHz: 690, VrunMv: 42}); err == nil {
		t.Fatal("700 MHz must be refused")
	}
	if err := box2CheckTuple(700, 42); err == nil {
		t.Fatal("700 MHz must be outside the clock window")
	}
}

// With C2 off, the dashboard's power block carries no watts, no per-board figure and no J/TH, and the Box II
// estimator and handler refuse even when called directly.
func TestBox2NoWattsWithC2Off(t *testing.T) {
	c2Box2(t, false, c2Dash(690, 42, 1900))
	d := c2Dash(690, 42, 1900)
	for _, v := range []map[string]any{powerView(d), powerViewBox2(d)} {
		for _, k := range []string{"watts", "boards", "jth", "amps", "specs"} {
			if _, ok := v[k]; ok {
				t.Fatalf("C2 off: %q reported: %v", k, v)
			}
		}
	}
	rr := httptest.NewRecorder()
	handlePowerBox2(rr, httptest.NewRequest("GET", "/api/power", nil))
	var got map[string]any
	json.Unmarshal(rr.Body.Bytes(), &got)
	if got["ok"] != false || got["watts"] != nil {
		t.Fatalf("handlePowerBox2 with C2 off: %v", got)
	}
}

func TestBox2MessagesNoMillivolts(t *testing.T) {
	b10TempAuth(t)
	p := profSCBox2
	b10SetModel(t, p)
	err := box2CheckTuple(600, 50)
	if err == nil || regexp.MustCompile(`\d V \d+ RPM`).MatchString(err.Error()) || !strings.Contains(err.Error(), "600 MHz at ≈ 7.39 V") ||
		!strings.Contains(err.Error(), "690 MHz at 7.67 V, 525 MHz at 7.03 V") || strings.Contains(err.Error(), "level") {
		t.Fatalf("stock-only refusal: %v", err)
	}
	if !chipTrimBoardOK(0) || chipTrimBoardOK(1) {
		t.Fatal("the Box II has one board, chain 0")
	}
	if msg := sctStart(2, 0, 0); msg != "no such board" {
		t.Fatalf("strong-chip test on board 2: %q", msg)
	}
	b := tunerBundle{cfg: defaultTunerConfig(), results: []testResult{{Time: "t", MHz: 690, MV: 42, Result: "clean"}},
		readings: []liveReading{{Time: "t", MHz: 690, MV: 42}}}
	if r := string(resultsCSV(b)); !strings.HasPrefix(r, "time,mhz,v_level,volts,result,") || !strings.Contains(r, "t,690,42,7.67,clean,") {
		t.Fatalf("results.csv on the Box II: %q", r)
	}
	if r := string(readingsCSV(b)); !strings.HasPrefix(r, "time,mhz,v_level,volts,") {
		t.Fatalf("live-readings.csv on the Box II: %q", r)
	}
}

func TestLiteExportUnchanged(t *testing.T) {
	b10SetModel(t, profSCLite)
	b := tunerBundle{cfg: defaultTunerConfig(), results: []testResult{{Time: "t", MHz: 625, MV: 9100, PV: 9400, Result: "clean"}}}
	if r := string(resultsCSV(b)); !strings.HasPrefix(r, "time,mhz,mv,pv,result,") {
		t.Fatalf("SC Lite results.csv header changed: %q", r)
	}
	if r := string(readingsCSV(b)); !strings.HasPrefix(r, "time,mhz,mv,ghs,") {
		t.Fatalf("SC Lite live-readings.csv header changed: %q", r)
	}
	if msg := sctStart(4, 0, 0); msg != "no such board" {
		t.Fatalf("SC Lite board 4: %q", msg)
	}
}

// The owner's build: the first C6 ceiling (725 MHz) opens only with the overclock (C7) commissioned.
func TestBox2OwnerCeiling725(t *testing.T) {
	if publicBuild {
		t.Skip("the public release has no ceiling")
	}
	b10TempAuth(t)
	p := profSCBox2
	p.Feat.ClockControl, p.Feat.VoltageControl, p.Feat.CeilingProbe = true, true, true
	b10SetModel(t, p)
	if freqHardMax != 690 {
		t.Fatalf("without the overclock commissioned the window stays 690, got %d", freqHardMax)
	}
	if err := ceilBox2StartErr(true); err != nil && strings.Contains(err.Error(), "isn't decided") {
		t.Fatalf("the owner's probe has a ceiling (725): %v", err)
	}
	p.Feat.OverclockUnlock = true
	b10SetModel(t, p)
	if freqHardMax != 725 {
		t.Fatalf("with the overclock commissioned the window opens to 725, got %d", freqHardMax)
	}
}
