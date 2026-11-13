package main

import (
	"path/filepath"
	"testing"
)

func TestFanGuard(t *testing.T) {
	*authFile = filepath.Join(t.TempDir(), "auth.json")
	fanGuardClear()
	if fanFloor() != fanMin {
		t.Fatalf("floor %d", fanFloor())
	}
	old := readWDGuard
	t.Cleanup(func() { readWDGuard = old; fanGuardClear() })
	// healthy watchdog at 15 %: nothing changes
	readWDGuard = func() (watchdogView, bool) { return watchdogView{Fans: []int{400, 420, 410, 400}}, true }
	if fanGuardCheck(15) != 0 || fanFloor() != fanMin {
		t.Fatal("raised without a flag")
	}
	// a slow fan flagged at 15 %: floor 25 %, kept across a restart (file), cleared by saving fan settings
	readWDGuard = func() (watchdogView, bool) {
		return watchdogView{Fans: []int{180, 420, 410, 400}, Status: 1, Flagged: "the watchdog flags fan 0 (180 RPM)"}, true
	}
	if f := fanGuardCheck(15); f != 25 || fanFloor() != 25 {
		t.Fatalf("floor %d/%d, want 25", f, fanFloor())
	}
	fanGuardOK = false // as after a restart: read from the file
	if fanFloor() != 25 {
		t.Fatal("floor not kept")
	}
	if targetFan(fanConfig{Profile: "custom", Points: []curvePoint{{Temp: 20, Fan: 10}, {Temp: 90, Fan: 100}}}, 20) < 25 {
		t.Fatal("auto fan goes under the raised floor")
	}
	// a fan at 0 RPM at 40 %: wiring, not the floor
	readWDGuard = func() (watchdogView, bool) { return watchdogView{Fans: []int{0, 900, 900, 900}, Status: 1}, true }
	if fanGuardCheck(40) != 0 {
		t.Fatal("raised the floor for a dead fan")
	}
	// never past 50 %
	readWDGuard = func() (watchdogView, bool) { return watchdogView{Fans: []int{900, 900, 900, 900}, Status: 15}, true }
	if f := fanGuardCheck(45); f != 50 {
		t.Fatalf("cap: %d", f)
	}
	if fanGuardCheck(50) != 0 {
		t.Fatal("acted at 50 %")
	}
	fanGuardClear()
	if fanFloor() != fanMin {
		t.Fatal("not cleared")
	}
}
