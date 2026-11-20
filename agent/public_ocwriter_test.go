//go:build public

package main

import "testing"

// The public release on the real writer: the probe and the unlock are on (publicFeatures), under the per-unit
// rule. No probe result: nothing above 690 MHz goes through the writer or Controls even with an acknowledgment on
// disk. With this unit's probe clean to 720 MHz: Controls' override applies 712 MHz; 721 and 750 are refused.
func TestPublicProbeUnlockRealWriter(t *testing.T) {
	f := c67RealSetup(t)
	model.Feat.OverclockUnlock = true
	setWindowsFromModel()
	saveOverclock(overclockState{Acknowledged: true, At: "x"})
	if _, hi := box2ClockWindow(); hi != 690 {
		t.Fatalf("no probe result: public clock window top %d", hi)
	}
	for _, b := range []string{`{"freq_mhz":700,"confirm":true,"override":true}`, `{"freq_mhz":712,"confirm":true}`} {
		if r := c34Post(t, b); r["ok"] != false {
			t.Fatalf("%s accepted without a probe result: %v", b, r)
		}
	}
	if _, err := mutatePlanGen(safetyGen.Load(), func(p *powerplan) { p.FreqMHz = 700 }); err == nil {
		t.Fatal("BUG: 700 MHz written without a probe result")
	}
	if err := box2SaveProbe(box2ProbeRecord{Cap: 720, FailAt: 725, Why: "hardware errors", Top: 750, Level: 42}); err != nil {
		t.Fatal(err)
	}
	r := c34Post(t, `{"freq_mhz":712,"confirm":true,"override":true}`)
	box2AsyncConfirms.Wait()
	if r["ok"] != true {
		t.Fatalf("712 MHz with the probe result and the unlock: %v", r)
	}
	c67RealSet(f, "690 MHz 42 V 50 RPM 41 RPM")
	for _, b := range []string{`{"freq_mhz":721,"confirm":true,"override":true}`, `{"freq_mhz":750,"confirm":true,"override":true}`} {
		if r := c34Post(t, b); r["ok"] != false {
			t.Fatalf("%s accepted past this unit's 720 MHz: %v", b, r)
		}
	}
	for _, p := range f.putPlans() {
		if pp, ok := parsePowerplanBox2(p); !ok || pp.FreqMHz > 720 {
			t.Fatalf("BUG: wrote %q", p)
		}
	}
}
