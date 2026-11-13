package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ops sweep: the engine's real trimSet writes the trims file, then re-applies live. When the live re-apply is
// refused (the hook's report is older than the last /mcb/setting write: with the fan hold on, any fan change
// in the last 10 s), trimOne got an error and didn't record the trim, and nothing undid it: the trim stayed in
// the file and the next clock change made the hook apply it, silently, for the rest of the run and after.
// Seen through the HTTP API twice (v1.5.16 tree of 10:2x and 11:30): "no chip trim (couldn't set the trim: the
// chip hook's report isn't fresh yet...)", then the chip ran at -12.5 MHz from the next setting on, with the
// run's trims list empty and the curve/presets recorded as clean without trims.
func TestTunerTrimOneRefusedLeavesNoTrim(t *testing.T) {
	old := *authFile
	*authFile = t.TempDir() + "/auth.json"
	defer func() { *authFile = old }()
	oldStats := chipStatsPath
	chipStatsPath = filepath.Join(t.TempDir(), "sclite-chipstats.json")
	defer func() { chipStatsPath = oldStats }()
	if err := os.WriteFile(chipStatsPath, []byte(`{"t":1,"chains":[{"chain":2,"chips":46,"base10":5750,"clock10":[5750],"nonces":[0]}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-5 * time.Second)
	os.Chtimes(chipStatsPath, past, past)
	noteMinerChange("clock/voltage change") // a fan write after the hook's last report
	e := newTunerEngine(tunerConfig{ChipTrim: true, ChipTrimMax: 3})
	e.sleep = func(time.Duration) {} // (trimSet's wait for a fresh report)
	e.hookStats = func() (chipStatsFile, bool) { s, _, ok := readChipStats(); return s, ok }
	e.baseRate = map[ckey]float64{{B: 2, I: 1}: 1, {B: 2, I: 42}: 1}
	var made []madeTrim
	desc, why := e.trimOne(ckey{B: 2, I: 42}, &made)
	t.Logf("trimOne: %q %q", desc, why)
	if desc != "" {
		t.Fatal("expected the trim to be refused")
	}
	if got := readChipTrims(); len(got) != 0 {
		t.Fatalf("trim refused but %+v left in the trims file: the next clock change applies it, and no one undoes it", got)
	}
}
