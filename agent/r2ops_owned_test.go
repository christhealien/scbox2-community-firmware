package main

import (
	"path/filepath"
	"testing"
)

// R2-ops: a tuning run hands its kept trims to auto-trim (chipWatchAdopt) even with auto-trim off. A Reset (or a
// hand edit) of those chips while auto-trim is off never drops them from its list: the Chip trims card keeps
// showing them as auto-trims (seen on the mock: owned b1.c29 -75, b3.c32 -50 with an empty trims file), and
// a later run's adopt skips those chips ("already the watchdog's").
func TestR2OpsOwnedStaleWhileAutoTrimOff(t *testing.T) {
	old := *authFile
	*authFile = filepath.Join(t.TempDir(), "auth.json")
	t.Cleanup(func() { *authFile = old })
	cwMu.Lock()
	oSt, oLoaded, oBlocked := cwSt, cwLoaded, cwBlocked
	cwSt, cwLoaded = cwState{On: false, NoHelp: map[string]int64{}}, true
	cwBlocked = func() string { return "" }
	cwMu.Unlock()
	t.Cleanup(func() { cwMu.Lock(); cwSt, cwLoaded, cwBlocked = oSt, oLoaded, oBlocked; cwMu.Unlock() })
	writeChipTrims([]chipTrim{{1, 29, -75}})
	chipWatchAdopt([]chipTrim{{1, 29, -75}}, "kept by a tuning run")
	writeChipTrims(nil) // Controls -> Chip trims -> Reset
	chipWatchTick()
	chipWatchTick()
	if o := chipWatchView()["owned"].([]cwOwned); len(o) != 0 {
		t.Fatalf("auto-trim still lists %v as its own after the trims were reset", o)
	}
}
