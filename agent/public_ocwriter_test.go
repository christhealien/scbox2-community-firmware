//go:build public

package main

import "testing"

// The public build (OCHardMax 0): even with every C6/C7 gate forced on, the probe's window open and an unlock
// accepted, nothing above 690 MHz goes through the real writer or Controls.
func TestPublicNoClockAbove690RealWriter(t *testing.T) {
	f := newC34(t, true, false)
	if model.OCHardMax != 0 {
		t.Fatalf("public ceiling %d, want 0", model.OCHardMax)
	}
	model.Feat.CeilingProbe, model.Feat.OverclockUnlock = true, true
	setWindowsFromModel()
	saveOverclock(overclockState{Acknowledged: true, At: "x"})
	setCharge("ceiling")
	ceilRiskOK.Store(true)
	t.Cleanup(func() { ceilRiskOK.Store(false); setCharge("") })
	if _, hi := box2ClockWindow(); hi != 690 {
		t.Fatalf("public clock window top %d", hi)
	}
	for _, mhz := range []int{691, 700, 712, 725, 730} {
		if _, err := ceilMutate(safetyGen.Load(), func(p *powerplan) { p.FreqMHz = mhz }); err == nil {
			t.Fatalf("BUG: %d MHz written in the public build", mhz)
		}
	}
	setCharge("")
	ceilRiskOK.Store(false)
	for _, b := range []string{`{"freq_mhz":700,"confirm":true,"override":true}`, `{"freq_mhz":712,"confirm":true}`} {
		if r := c34Post(t, b); r["ok"] != false {
			t.Fatalf("%s accepted: %v", b, r)
		}
	}
	for _, p := range f.putPlans() {
		if pp, ok := parsePowerplanBox2(p); !ok || pp.FreqMHz > 690 {
			t.Fatalf("BUG: wrote %q", p)
		}
	}
}
