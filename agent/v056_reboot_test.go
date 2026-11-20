package main

import (
	"os"
	"testing"
)

// v0.5.6: the SC Box II reboots without the OEM's shutdown scripts when everything is on the read-only flash
// already, and otherwise forces the reboot if the normal one hasn't happened 90 s later; the SC Lite as before.
func TestRebootCmdBox2Forced(t *testing.T) {
	os.Unsetenv(brandEnv("REBOOT"))
	if c := rebootCmd(); c != "reboot" {
		t.Fatalf("SC Lite: %q", c)
	}
	useBox2(t)
	oRO := box2FlashRO
	t.Cleanup(func() { box2FlashRO = oRO })
	box2FlashRO = func() bool { return false }
	if c := rebootCmd(); c != "reboot; sleep 90; sync; reboot -f" {
		t.Fatalf("Box II, flash writable or not shadowed: %q", c)
	}
}

func TestRebootCmdBox2ShadowedRO(t *testing.T) {
	os.Unsetenv(brandEnv("REBOOT"))
	withShadow(t)
	oRO := box2FlashRO
	t.Cleanup(func() { box2FlashRO = oRO })
	box2FlashRO = func() bool { return true }
	if !cfgShadow() {
		t.Skip("the fixture doesn't shadow the settings here")
	}
	if c := rebootCmd(); c != "reboot -f" {
		t.Fatalf("Box II, settings in RAM, flash read-only: %q", c)
	}
}
