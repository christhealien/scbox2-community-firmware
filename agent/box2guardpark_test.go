package main

// Hardware-safety review (review050/r2) on the SC Box II, ported from its scratch tests:
//   H1 a wake write already on its way must not undo a safety guard's soft-idle park;
//   M1 a clock raise through the Box II writer must not run a "+"-trimmed chip past the clock window;
//   L1/L2 a running plan the panel can't read exactly (fractional / unreadable level) is never truncated: start-up
//         repairs it to stock and a fans-only write lands on the stock plan;
//   L4 a Controls stock-preset select on a soft-idled board resumes it (or refuses), never "ok" with it off.

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func guardParkEnv(t *testing.T) *c1Env {
	t.Helper()
	e := newC1Env(t, true)
	model.Feat.ClockControl, model.Feat.VoltageControl = true, true
	var puts []map[string]any
	box2HashingStock(t, &puts)
	heatIdleSince.Store(0)
	heatHoldSaved.Store(false)
	setCharge("")
	t.Cleanup(func() { heatIdleSince.Store(0); heatHoldSaved.Store(false); setCharge("") })
	slow := e.f.param
	box2APICmdParam = func(c, p string) ([]byte, error) {
		if c == "pgadisable" {
			time.Sleep(300 * time.Millisecond) // the guard holds box2Mu inside its park meanwhile
		}
		return slow(c, p)
	}
	return e
}

// H1: the thermal guard's park vs a Controls/preset write that noted gen while the park was under way.
func TestBox2GuardParkVsWakeWriter(t *testing.T) {
	e := guardParkEnv(t)
	done := make(chan error, 1)
	go func() { done <- thermalIdle("overheat: board 0 at 84.0 °C") }()
	time.Sleep(80 * time.Millisecond)
	gen := safetyGen.Load()
	_, werr := mutatePlanGen(gen, func(p *powerplan) {})
	if gerr := <-done; gerr != nil {
		t.Fatalf("the guard's park: %v", gerr)
	}
	if !heatIdleHeld() || e.f.isEnabled(0) || e.f.count("pgaenable") != 0 {
		t.Fatalf("BUG: park undone by a writer on its way (writer err %v): enabled=%v hold=%v pgaenable=%d",
			werr, e.f.isEnabled(0), heatIdleHeld(), e.f.count("pgaenable"))
	}
	if werr == nil {
		t.Fatal("BUG: the wake write reported success over the guard's park")
	}
}

// H1: the same with a stock select (Stock / a stock mode from the schedule or Presets).
func TestBox2GuardParkVsStockSelect(t *testing.T) {
	e := guardParkEnv(t)
	done := make(chan error, 1)
	go func() { done <- thermalIdle("overheat") }()
	time.Sleep(80 * time.Millisecond)
	serr := applyStockIndexGen(safetyGen.Load(), 0, "preset:Hashrate", "Hashrate")
	<-done
	if !heatIdleHeld() || e.f.isEnabled(0) || serr == nil {
		t.Fatalf("BUG: stock select (%v) re-enabled the parked board: enabled=%v hold=%v", serr, e.f.isEnabled(0), heatIdleHeld())
	}
}

// H1: during the cool-down and under the pool guard's hold the resume refuses outright; once the cool-down is
// over (the owner starts mining) it resumes.
func TestBox2ResumeRefusedWhileHeld(t *testing.T) {
	e := newC1Env(t, true)
	var puts []map[string]any
	box2HashingStock(t, &puts)
	t.Cleanup(func() { heatIdleSince.Store(0); heatHoldSaved.Store(false); setCharge("") })
	if err := thermalIdle("overheat"); err != nil {
		t.Fatal(err)
	}
	if heatCoolErr() == nil {
		t.Fatal("sanity: cooling")
	}
	if err := resumeAction(model); err == nil || e.f.isEnabled(0) {
		t.Fatalf("resumed during the cool-down: %v enabled=%v", err, e.f.isEnabled(0))
	}
	if err := box2WakeResume(noGen); err == nil {
		t.Fatal("the wake paths' resume passed during the cool-down")
	}
	heatIdleSince.Store(time.Now().Add(-24 * time.Hour).Unix()) // the cool-down is over
	heatEpMu.Lock()
	oldCool := heatEpCool
	heatEpCool = time.Minute
	heatEpMu.Unlock()
	t.Cleanup(func() { heatEpMu.Lock(); heatEpCool = oldCool; heatEpMu.Unlock() })
	if heatCoolErr() != nil {
		t.Skip("cool-down bookkeeping differs: covered by the heat tests")
	}
	if err := resumeAction(model); err != nil || !e.f.isEnabled(0) {
		t.Fatalf("after the cool-down the resume: %v enabled=%v", err, e.f.isEnabled(0))
	}
}

// M1: a raise through the Box II writer (the throttle's restore) refuses a clock a "+" trim can't run at.
func TestBox2PlusTrimOnRaise(t *testing.T) {
	f := newC34(t, true, true)
	model.Feat.ChipTrims = true
	os.WriteFile(chipHookFlag(), []byte("on\n"), 0o644)
	os.WriteFile(chipTrimPath(), []byte("0 5 25\n"), 0o644)
	t.Cleanup(func() { os.Remove(chipHookFlag()); os.Remove(chipTrimPath()) })
	f.set(func() {
		f.setting["manual"], f.setting["manualPowerplan"], f.clock = true, "665 MHz 42 V 50 RPM 41 RPM", 665
	})
	if out := c34Post(t, `{"freq_mhz":690,"vrun_mv":42,"confirm":true}`); out["ok"] == true {
		t.Fatalf("Controls raised past a +25 trim: %v", out)
	}
	_, err := mutatePlanNoWake(func(p *powerplan) {
		if p.FreqMHz == 665 {
			p.FreqMHz = 690 // the thermal throttle's restore
		}
	})
	box2AsyncConfirms.Wait()
	if got, _ := f.setting["manualPowerplan"].(string); err == nil || strings.HasPrefix(got, "690 MHz") {
		t.Fatalf("BUG: the restore raised the board to 690 MHz with chip 5 trimmed +25 (err %v, plan %q)", err, got)
	}
	// a lower clock or a fans-only write still goes through
	if _, err := mutatePlanNoWake(func(p *powerplan) { p.Fan1 = 80 }); err != nil {
		t.Fatalf("fans-only write refused: %v", err)
	}
	// a failed write's revert goes to the low stock plan when the plan before can't carry the trim
	if got := box2RevertTarget(powerplan{FreqMHz: 690, VrunMv: 42}); got.FreqMHz != 525 {
		t.Fatalf("revert target with a +25 trim: %+v, want the 525 MHz stock plan", got)
	}
}

// L1/L2: odd running plans are never truncated to another level.
func TestBox2OddRunningPlans(t *testing.T) {
	f := newC34(t, true, true)
	if pp, ok := parsePowerplanBox2("690 MHz 42.000011 V 50 RPM 41 RPM"); !ok || pp.VrunMv != 42 {
		t.Fatalf("the stock float text: %+v %v", pp, ok)
	}
	for _, run := range []string{
		"690 MHz 42.5 V 50 RPM 41 RPM",
		"600 MHz 50.7 V 50 RPM 41 RPM",
		"690 MHz 1e2 V 50 RPM 41 RPM",
		"690 MHz -5 V 50 RPM 41 RPM",
		"690 MHz 4.2.1 V 50 RPM 41 RPM",
		"99999999999999999999 MHz 42 V 50 RPM 41 RPM",
	} {
		zzBox2Reset(f)
		f.set(func() { f.setting["manual"], f.setting["manualPowerplan"] = true, run })
		var cur map[string]any
		json.Unmarshal([]byte(box2Setting), &cur)
		if v, _ := box2PlanVerdict(cur, run); v != "stock" {
			t.Errorf("start-up verdict on %q: %q, want stock", run, v)
		}
		if _, err := writeFansUnlessIdle(100); err != nil {
			t.Errorf("fans-100 on %q: %v", run, err)
		}
		if got, _ := f.setting["manualPowerplan"].(string); got != "690 MHz 42 V 100 RPM 41 RPM" {
			t.Errorf("fans-100 on %q wrote %q, want the stock plan with the fans at 100", run, got)
		}
	}
	box2AsyncConfirms.Wait()
}

func zzBox2Reset(f *c34Fake) {
	f.set(func() {
		f.setting = nil
		json.Unmarshal([]byte(box2Setting), &f.setting)
		f.clock, f.volt, f.ignore, f.apiDown = 690, 42, false, false
	})
}

// L4: Controls' stock preset on a soft-idled board: refused during the cool-down; after "Take manual control"
// (the owner's explicit end of the hold) it resumes the board, never ok with the board left off.
func TestBox2StockPresetResumesSoftIdle(t *testing.T) {
	e := newC1Env(t, true)
	model.Feat.ClockControl, model.Feat.VoltageControl = true, true
	var puts []map[string]any
	box2HashingStock(t, &puts)
	heatIdleSince.Store(0)
	setCharge("")
	t.Cleanup(func() { heatIdleSince.Store(0); heatHoldSaved.Store(false); setCharge("") })
	if err := thermalIdle("overheat"); err != nil {
		t.Fatal(err)
	}
	post := func(b string) map[string]any {
		rec := httptest.NewRecorder()
		handleControl(rec, httptest.NewRequest("POST", "/api/control", strings.NewReader(b)))
		var out map[string]any
		json.Unmarshal(rec.Body.Bytes(), &out)
		return out
	}
	if r := post(`{"preset":0,"confirm":true}`); r["ok"] == true || e.f.isEnabled(0) {
		t.Fatalf("stock preset during the cool-down hold: %v enabled=%v", r, e.f.isEnabled(0))
	}
	if r := post(`{"release":true}`); r["ok"] != true {
		t.Fatalf("release: %v", r)
	}
	r := post(`{"preset":0,"confirm":true}`)
	if r["ok"] == true && !e.f.isEnabled(0) {
		t.Fatalf("BUG: stock preset reported ok with the board still off: %v", r)
	}
	if r["ok"] != true || !e.f.isEnabled(0) {
		t.Fatalf("stock preset after the owner ended the hold: %v enabled=%v", r, e.f.isEnabled(0))
	}
}
