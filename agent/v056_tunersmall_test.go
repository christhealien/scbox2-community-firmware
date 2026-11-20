package main

import "testing"

// v0.5.6: a preset with the same setting as one already built isn't saved twice; a test the hold didn't hold fans
// for shows an empty fan cell, not 0 %.
func TestSamePresetSettingAndFanCSV(t *testing.T) {
	out := []preset{{Name: "High", MHz: 688, MV: 42}, {Name: "Low", MHz: 675, MV: 44}}
	if got := samePresetSetting(out, planMV{MHz: 675, MV: 44}); got != "Low" {
		t.Fatalf("duplicate not found: %q", got)
	}
	if got := samePresetSetting(out, planMV{MHz: 675, MV: 45}); got != "" {
		t.Fatalf("a different voltage counted as the same: %q", got)
	}
	if fanCSV(0) != "" || fanCSV(55) != "55" {
		t.Fatalf("fanCSV: %q %q", fanCSV(0), fanCSV(55))
	}
}
