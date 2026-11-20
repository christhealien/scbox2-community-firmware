package main

// C6/C7 end to end on the REAL Box II writer (ceilMutate = mutatePlanGen → mutatePlanBox2, the read-back
// confirmation included) against a fake stock stack (c34Fake: minerd + the miner API's devs), with the hard cap
// box2OCHardMax (750 MHz, both builds) and this unit's probe result bounding the unlock. c67ceiling_test.go fakes
// the writer; this one doesn't. (No build tag: the public release runs the same rule, TestPublicProbeUnlockRealWriter.)

import (
	"strings"
	"testing"
	"time"
)

// c67RealSetup: the c34 fake miner, the probe's readings faked (ceilDash), the writer real.
func c67RealSetup(t *testing.T) *c34Fake {
	t.Helper()
	f := newC34(t, true, false)
	if model.OCHardMax != 750 {
		t.Fatalf("the hard overclock cap is %d, want 750", model.OCHardMax)
	}
	model.Feat.CeilingProbe = true
	setWindowsFromModel()
	oldDash, oldHold, oldEvery := ceilDash, ceilBox2Hold, ceilBox2Every
	ceilBox2Hold, ceilBox2Every = 30*time.Millisecond, 5*time.Millisecond
	ceilDash = func() dashboard {
		f.mu.Lock()
		mhz := f.clock
		f.mu.Unlock()
		b := board{ID: 0, Alive: true, Enabled: true, Temps: []float64{70, 68}, Nonced: int(time.Now().UnixNano() / 1e6),
			ClockMHz: mhz, VoltLevel: 42}
		return dashboard{OK: true, Boards: []board{b}, Health: []boardHealth{{ID: 0, Chips: 36, Expected: 36}}}
	}
	t.Cleanup(func() {
		ceilDash, ceilBox2Hold, ceilBox2Every = oldDash, oldHold, oldEvery
		setCharge("")
		ceilAbort.Store(false)
		ceilRiskOK.Store(false)
		ceilMu.Lock()
		ceilSt = ceilingState{}
		ceilMu.Unlock()
	})
	return f
}

func c67RealSet(f *c34Fake, plan string) {
	f.set(func() {
		f.setting["manual"], f.setting["manualPowerplan"] = true, plan
		if pp, ok := parsePowerplanBox2(plan); ok {
			f.clock, f.volt = float64(pp.FreqMHz), float64(pp.VrunMv)
		}
	})
}

// C6: the probe (risk accepted, its window open) writes 700/720/725/740 and the 750 cap at level 42 (one request per
// 12.5 MHz chip-grid point) through the real writer and ends on Stock High; a 760 MHz write in its window is
// refused, so is 700 MHz at level 43. The result is saved for this unit.
func TestC6ProbeRealWriterOwner(t *testing.T) {
	f := c67RealSetup(t)
	if r := l15Post(t, handleCeiling, "/api/ceiling", `{"start":true,"accept_risk":true}`); r["ok"] != true {
		t.Fatalf("the probe didn't start: %v", r)
	}
	st := l15WaitProbe(t)
	box2AsyncConfirms.Wait()
	if a := st.Axes[0]; a.State != "done" || a.Cap != 750 {
		t.Fatalf("result %+v; log:\n%s", a, strings.Join(st.Log, "\n"))
	}
	got := f.putPlans()
	want := []string{"700 MHz 42 V 50 RPM 41 RPM", "720 MHz 42 V 50 RPM 41 RPM", "725 MHz 42 V 50 RPM 41 RPM", "740 MHz 42 V 50 RPM 41 RPM", "750 MHz 42 V 50 RPM 41 RPM"}
	if len(got) < len(want)+1 || strings.Join(got[:len(want)], "|") != strings.Join(want, "|") {
		t.Fatalf("the probe wrote %q, want %q then Stock High", got, want)
	}
	if last := got[len(got)-1]; !strings.HasPrefix(last, "690 MHz 42 V") {
		t.Fatalf("the probe ended on %q, not Stock High", last)
	}
	if r, ok := box2ProbeResult(); !ok || r.Cap != 750 || r.FailAt != 0 || box2ProbeLimit() != 750 {
		t.Fatalf("saved result %+v (ok %v), limit %d", r, ok, box2ProbeLimit())
	}

	// in the probe's window (its charge, the risk accepted): the ceiling holds, the level stays 42
	c67RealSet(f, "690 MHz 42 V 50 RPM 41 RPM")
	setCharge("ceiling")
	ceilRiskOK.Store(true)
	gen := safetyGen.Load()
	if _, err := ceilMutate(gen, func(p *powerplan) { p.FreqMHz = 750 }); err != nil {
		t.Fatalf("750 MHz in the probe's window: %v", err)
	}
	c67RealSet(f, "690 MHz 42 V 50 RPM 41 RPM")
	if _, err := ceilMutate(gen, func(p *powerplan) { p.FreqMHz = 760 }); err == nil {
		t.Fatal("BUG: 760 MHz written past the 750 MHz cap")
	}
	if _, err := ceilMutate(gen, func(p *powerplan) { p.FreqMHz, p.VrunMv = 700, 43 }); err == nil {
		t.Fatal("BUG: 700 MHz at level 43 written (above 690 the level stays 42)")
	}
	// the probe's window closes with it
	ceilRiskOK.Store(false)
	setCharge("")
	if _, err := ceilMutate(safetyGen.Load(), func(p *powerplan) { p.FreqMHz = 700 }); err == nil {
		t.Fatal("BUG: 700 MHz written with no probe running and no unlock")
	}
	for _, p := range f.putPlans() {
		if pp, ok := parsePowerplanBox2(p); !ok || pp.FreqMHz > 750 || (pp.FreqMHz > 690 && pp.VrunMv != 42) {
			t.Fatalf("BUG: wrote %q", p)
		}
	}
}

// C7: with the unlock on, this unit's probe clean to 720 MHz and the unlock accepted, Controls' override applies
// 712 MHz (level 42) through the real writer; 721 MHz (past the probe result) and 700 MHz at level 43 are refused;
// without override a clock above 690 is refused.
func TestC7UnlockRealWriterOwner(t *testing.T) {
	f := c67RealSetup(t)
	model.Feat.OverclockUnlock = true
	setWindowsFromModel()
	if err := box2SaveProbe(box2ProbeRecord{Cap: 720, FailAt: 725, Why: "hardware errors", Top: 750, Level: 42}); err != nil {
		t.Fatal(err)
	}
	if r := c34Post(t, `{"freq_mhz":712,"confirm":true,"override":true}`); r["ok"] != false {
		t.Fatalf("override before the risk is accepted: %v", r)
	}
	saveOverclock(overclockState{Acknowledged: true, At: "x"})
	if r := c34Post(t, `{"freq_mhz":712,"confirm":true}`); r["ok"] != false {
		t.Fatalf("712 MHz without override: %v", r)
	}
	r := c34Post(t, `{"freq_mhz":712,"confirm":true,"override":true}`)
	box2AsyncConfirms.Wait()
	if r["ok"] != true {
		t.Fatalf("712 MHz with the unlock accepted: %v", r)
	}
	if p := f.putPlans(); len(p) == 0 || p[len(p)-1] != "712 MHz 42 V 50 RPM 41 RPM" {
		t.Fatalf("wrote %q", p)
	}
	n := len(f.putPlans())
	c67RealSet(f, "690 MHz 42 V 50 RPM 41 RPM")
	for _, body := range []string{`{"freq_mhz":721,"confirm":true,"override":true}`, `{"freq_mhz":726,"confirm":true,"override":true}`, `{"freq_mhz":700,"vrun_mv":43,"confirm":true,"override":true}`} {
		if r := c34Post(t, body); r["ok"] != false {
			t.Fatalf("%s accepted: %v", body, r)
		}
	}
	if _, err := mutatePlanGen(safetyGen.Load(), func(p *powerplan) { p.FreqMHz = 721 }); err == nil {
		t.Fatal("BUG: the writer took 721 MHz, past this unit's 720 MHz probe result")
	}
	if _, err := mutatePlanGen(safetyGen.Load(), func(p *powerplan) { p.FreqMHz, p.VrunMv = 700, 43 }); err == nil {
		t.Fatal("BUG: the writer took 700 MHz at level 43")
	}
	if p := f.putPlans(); len(p) != n {
		t.Fatalf("refused requests wrote %q", p[n:])
	}
	// re-locked: back to 690
	saveOverclock(overclockState{})
	if lo, hi := box2ClockWindow(); lo != 525 || hi != 690 {
		t.Fatalf("re-locked window %d–%d", lo, hi)
	}
}
