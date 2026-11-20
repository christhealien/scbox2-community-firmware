package main

import (
	"os"
	"strings"
	"testing"
)

// v0.5.6: the first start after an update puts the owner's saved plan back when minerd booted on the stale dot copy
// v0.5.2–v0.5.5 left on the flash; never when the boot already runs the saved plan, when the saved plan isn't
// allowed, or while a tuning run's record waits for its own put-back.
func TestBox2HealSavedPlan(t *testing.T) {
	f := newC34(t, true, true)
	oOn, oSaved := box2HealOn, box2SavedPlans
	t.Cleanup(func() { box2HealOn, box2SavedPlans = oOn, oSaved })
	box2HealOn = func() bool { return true }
	twin, dot := "688 MHz 42 V 68 RPM 41 RPM", "690 MHz 42 V 64 RPM 41 RPM"
	box2SavedPlans = func() (string, string) { return twin, dot }
	running := func(p string) map[string]any {
		f.set(func() { f.setting["manual"], f.setting["manualPowerplan"] = true, p })
		f.mu.Lock()
		defer f.mu.Unlock()
		cur := map[string]any{}
		for k, v := range f.setting {
			cur[k] = v
		}
		return cur
	}
	plan := func() string {
		f.mu.Lock()
		defer f.mu.Unlock()
		s, _ := f.setting["manualPowerplan"].(string)
		return s
	}

	// the boot ran the stale dot copy: the saved 688 MHz goes back
	cur := running("690 MHz 42 V 64 RPM 41 RPM")
	if !box2HealSavedPlan(cur, "690 MHz 42 V 64 RPM 41 RPM") {
		t.Fatal("the saved plan was not put back")
	}
	box2AsyncConfirms.Wait()
	if !strings.HasPrefix(plan(), "688 MHz 42 V") {
		t.Fatalf("running %q, want the saved 688 MHz", plan())
	}

	// the boot runs the saved plan already (the dot copy is current): nothing
	dot = twin
	cur = running(twin)
	if box2HealSavedPlan(cur, twin) {
		t.Fatal("put back a plan that was running already")
	}

	// the boot didn't come from the dot copy (something else wrote the running plan): left to the usual check
	dot = "690 MHz 42 V 64 RPM 41 RPM"
	cur = running("675 MHz 45 V 60 RPM 41 RPM")
	if box2HealSavedPlan(cur, "675 MHz 45 V 60 RPM 41 RPM") {
		t.Fatal("acted on a plan that didn't come from the stale dot copy")
	}

	// the saved plan isn't allowed now (clock control off): not put back
	model.Feat.ClockControl = false
	cur = running(dot)
	if box2HealSavedPlan(cur, dot) {
		t.Fatal("put back a saved plan this run doesn't allow")
	}
	model.Feat.ClockControl = true

	// a tuning run's record waits: tunerRecover decides, not this
	oDir := *tunerDir
	*tunerDir = t.TempDir()
	t.Cleanup(func() { *tunerDir = oDir })
	if err := os.WriteFile(tunerMarkerPath(), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	cur = running(dot)
	if box2HealSavedPlan(cur, dot) {
		t.Fatal("acted while a tuning run's record waits")
	}
}
