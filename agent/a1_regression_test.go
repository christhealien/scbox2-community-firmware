package main

// A1 regression: the SC Lite profile must reproduce the exact pre-port constants and the SC Lite plan
// path must be byte-identical to before (the port must not change SC Lite behaviour).

import "testing"

func TestA1SCLiteWindowsUnchanged(t *testing.T) {
	// the default active profile is SC Lite; these are the pre-port compile-time constants verbatim
	model, modelWhy = profSCLite, "test"
	setWindowsFromModel()
	cases := []struct {
		name      string
		got, want int
	}{
		{"freqMin", freqMin, 300}, {"freqMax", freqMax, 700},
		{"vrunMin", vrunMin, 8800}, {"vrunMax", vrunMax, 9300},
		{"pvMin", pvMin, 9100}, {"pvMax", pvMax, 9600},
		{"fanMin", fanMin, 10}, {"fanMax", fanMax, 100},
		{"freqHardMin", freqHardMin, 200}, {"freqHardMax", freqHardMax, 900},
		{"vrunHardMin", vrunHardMin, 8800}, {"vrunHardMax", vrunHardMax, 9300},
		{"pvHardMin", pvHardMin, 9100}, {"pvHardMax", pvHardMax, 9600},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d", c.name, c.got, c.want)
		}
	}
	if capFloor["freq"] != 700 || capFloor["vrun"] != 9300 || capFloor["pv"] != 9600 {
		t.Errorf("capFloor = %v, want freq700/vrun9300/pv9600", capFloor)
	}
}

func TestA1SCLitePlanByteIdentical(t *testing.T) {
	model, modelWhy = profSCLite, "test"
	setWindowsFromModel()
	// the exact format the panel has always written, unchanged by the plan-format abstraction
	p := powerplan{FreqMHz: 625, VrunMv: 9100, PVMv: 9400, Fan1: 50, Fan2: 50}
	const want = "625 MHz 9100 V 50 RPM 50 RPM PV 9400"
	if got := buildPowerplan(p); got != want {
		t.Fatalf("buildPowerplan = %q, want %q", got, want)
	}
	got, ok := parsePowerplan(want)
	if !ok {
		t.Fatal("parsePowerplan failed on the canonical SC Lite string")
	}
	if got != p {
		t.Fatalf("parse round-trip = %+v, want %+v", got, p)
	}
	// the idle preset (no PV) still parses with PV defaulting to Vrun, as before
	idle, ok := parsePowerplan("0 MHz 9100 V 100 RPM 100 RPM")
	if !ok || idle.FreqMHz != 0 || idle.PVMv != 9100 {
		t.Fatalf("idle parse = %+v ok=%v", idle, ok)
	}
}

func TestA1SCLiteFeaturesAllOn(t *testing.T) {
	f := profSCLite.Feat
	if !(f.ClockControl && f.VoltageControl && f.Tuner && f.CeilingProbe && f.OverclockUnlock &&
		f.ChipTrims && f.FanPairOffset && f.FanFit && f.PowerEstimate && f.HardwareWatchdog && f.FirmwareUpdate) {
		t.Fatalf("SC Lite must have every feature on: %+v", f)
	}
	b := profSCBox2.Feat
	if b.ClockControl || b.VoltageControl || b.Tuner || b.CeilingProbe || b.OverclockUnlock ||
		b.ChipTrims || b.FanPairOffset || b.FanFit || b.PowerEstimate || b.HardwareWatchdog || b.FirmwareUpdate {
		t.Fatalf("SC Box II must have every control feature off: %+v", b)
	}
}

func TestA1Box2DerivedThermals(t *testing.T) {
	// the Box II guards are derived from the never-raised 86°C cut-off: heat-guard -4, abort-max -2, target 77
	p := profSCBox2
	if p.TempCutoffC != 86 || p.HeatGuardC != 82 || p.AbortMaxC != 84 || p.TargetC != 77 {
		t.Fatalf("Box II thermals wrong: cutoff=%.0f heatguard=%.0f abortmax=%.0f target=%.0f",
			p.TempCutoffC, p.HeatGuardC, p.AbortMaxC, p.TargetC)
	}
	if !(p.HeatGuardC < p.TempCutoffC && p.AbortMaxC < p.TempCutoffC && p.TargetC <= p.HeatGuardC) {
		t.Fatalf("Box II guards not safely under the cut-off")
	}
}
