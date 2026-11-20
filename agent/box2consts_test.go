package main

// Box II constants groom: nothing the SC Box II uses comes from the SC Lite. The voltage is the OEM's level
// (42–61, a higher level is a lower voltage), there is no PV, the overclock is capped at 750 MHz and opens only up
// to this unit's own clean probe result (none: nothing above 690 MHz), and with C2 off no Box II path reports
// watts or J/TH.

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
	// the hard overclock cap: 750 MHz in both builds (the unlock opens only up to this unit's probe result)
	if p.OCHardMax != 750 {
		t.Fatalf("overclock cap (public %v): want 750, got %d", publicBuild, p.OCHardMax)
	}
	// the SC Lite keeps its figures
	l := profSCLite
	if l.PVMin != 9100 || l.PVMax != 9600 || l.VrunMin != 8800 || l.VrunMax != 9300 || l.FreqHardMax != 900 || l.VCodeDir != vcodeDirUnknown {
		t.Fatalf("SC Lite profile changed: %+v", l)
	}
}

// No probe result on this unit: C7 opens nothing above 690 MHz on the real profile, with every gate on and an
// acknowledgment on disk; a profile with no overclock cap refuses the probe too.
func TestBox2NoClockAbove690(t *testing.T) {
	b10TempAuth(t)
	p := profSCBox2
	p.Feat.ClockControl, p.Feat.VoltageControl, p.Feat.CeilingProbe, p.Feat.OverclockUnlock = true, true, true, true
	b10SetModel(t, p)
	if err := saveOverclock(overclockState{Acknowledged: true}); err != nil {
		t.Fatal(err)
	}
	if hi := clockWindowHi(); hi != 690 || box2ProbeLimit() != 690 || box2UnlockErr() == "" {
		t.Fatalf("no probe result: window %d, limit %d, why %q", hi, box2ProbeLimit(), box2UnlockErr())
	}
	if err := box2CheckTuple(700, 42); err == nil {
		t.Fatal("700 MHz must be outside the clock window without a probe result")
	}
	p.OCHardMax = 0 // a profile with no overclock cap: nothing above 690 even with every gate on
	b10SetModel(t, p)
	if freqHardMax != 690 {
		t.Fatalf("unlocked window %d, want 690", freqHardMax)
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

// Both builds: the 750 MHz cap is the probe's top; the unlocked ceiling is 750 only with the overclock (C7) on,
// and the window inside it follows this unit's probe result.
func TestBox2Ceiling750(t *testing.T) {
	b10TempAuth(t)
	p := profSCBox2
	p.Feat.ClockControl, p.Feat.VoltageControl, p.Feat.CeilingProbe = true, true, true
	b10SetModel(t, p)
	if freqHardMax != 690 {
		t.Fatalf("without the overclock commissioned the window stays 690, got %d", freqHardMax)
	}
	if err := ceilBox2StartErr(true); err != nil && strings.Contains(err.Error(), "isn't decided") {
		t.Fatalf("the probe has a cap (750): %v", err)
	}
	if hi := ceilBox2Hi(); hi != 750 {
		t.Fatalf("the probe's top is %d, want 750", hi)
	}
	p.Feat.OverclockUnlock = true
	b10SetModel(t, p)
	if freqHardMax != 750 {
		t.Fatalf("with the overclock on the hard ceiling is 750, got %d", freqHardMax)
	}
	saveOverclock(overclockState{Acknowledged: true})
	if hi := clockWindowHi(); hi != 690 {
		t.Fatalf("no probe result: the window stays 690, got %d", hi)
	}
	box2SaveProbe(box2ProbeRecord{Cap: 720, FailAt: 725, Top: 750, Level: 42})
	if hi := clockWindowHi(); hi != 720 {
		t.Fatalf("probe clean to 720: the window opens to 720, got %d", hi)
	}
}
