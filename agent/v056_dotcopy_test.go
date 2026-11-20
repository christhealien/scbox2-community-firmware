package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// v0.5.6: the plan files' dot copies (".cpbsetting.json", ".bfgminer.json", ".temp_bfgminer.json") are what minerd
// starts from at boot: an owner's plan is saved into them too, by the same rules as their twins (an owner's
// v0.5.5 log: the flash's dot copies kept 690 MHz while the twins held the owner's 688, and every boot ran 690).
func TestShadowPlanDotCopiesSaved(t *testing.T) {
	f := withShadow(t)
	cpb := func(plan string) string {
		return `{"name":"Benchmark","manual":true,"manualPowerplan":"` + plan + `","powerplans":[{"info":"690 MHz 42 V 50 RPM 41 RPM"}],"select":0}`
	}
	bfg := func(set string) string {
		return `{"set":["intchains_qomo:vfff=` + set + `","intchains_qomo:algo=blake2b(SC)"],"pools":[{"url":"stratum+tcp://a:1"}]}`
	}
	for _, n := range []string{"cpbsetting.json", ".cpbsetting.json"} {
		f.putBoth(t, "minerd/settings/"+n, cpb("690 MHz 42 V 64 RPM 41 RPM"), cpb("690 MHz 42 V 64 RPM 41 RPM"))
	}
	for _, n := range []string{"bfgminer.json", ".bfgminer.json", "temp_bfgminer.json", ".temp_bfgminer.json"} {
		f.putBoth(t, "bfgminer/"+n, bfg("42.000000:690:64:41"), bfg("42.000000:690:64:41"))
	}
	// a fan step alone (no owner's plan write): none of them, dot copies included
	for _, n := range []string{"minerd/settings/cpbsetting.json", "minerd/settings/.cpbsetting.json"} {
		os.WriteFile(filepath.Join(f.ram, n), []byte(cpb("690 MHz 42 V 70 RPM 41 RPM")), 0o600)
	}
	oemPersist(false)
	oemPersist(true)
	if f.opened() != 0 {
		t.Fatalf("a fan-only change opened %d window(s)", f.opened())
	}
	// the owner applies 688 MHz: minerd rewrites the files and their dot copies; all six reach the flash
	for _, n := range []string{"minerd/settings/cpbsetting.json", "minerd/settings/.cpbsetting.json"} {
		os.WriteFile(filepath.Join(f.ram, n), []byte(cpb("688 MHz 42 V 68 RPM 41 RPM")), 0o600)
	}
	for _, n := range []string{"bfgminer.json", ".bfgminer.json", "temp_bfgminer.json", ".temp_bfgminer.json"} {
		os.WriteFile(filepath.Join(f.ram, "bfgminer", n), []byte(bfg("42.000000:688:68:41")), 0o600)
	}
	oemPersist(true)
	for _, n := range []string{"minerd/settings/cpbsetting.json", "minerd/settings/.cpbsetting.json"} {
		if s, _ := f.onFlash(n); !strings.Contains(s, "688 MHz") {
			t.Fatalf("%s not saved: %q", n, s)
		}
	}
	for _, n := range []string{"bfgminer.json", ".bfgminer.json", "temp_bfgminer.json", ".temp_bfgminer.json"} {
		if s, _ := f.onFlash("bfgminer/" + n); !strings.Contains(s, ":688:") {
			t.Fatalf("bfgminer/%s not saved: %q", n, s)
		}
	}
	// a throttle step (no owner's plan write) reaches neither a file nor its dot copy
	for _, n := range []string{"minerd/settings/cpbsetting.json", "minerd/settings/.cpbsetting.json"} {
		os.WriteFile(filepath.Join(f.ram, n), []byte(cpb("675 MHz 42 V 80 RPM 41 RPM")), 0o600)
	}
	oemPersist(false)
	if s, _ := f.onFlash("minerd/settings/.cpbsetting.json"); !strings.Contains(s, "688 MHz") {
		t.Fatalf("an automatic clock change was saved into the dot copy: %q", s)
	}
	// other dot files stay temp files: never copied
	os.WriteFile(filepath.Join(f.ram, "minerd/settings/.login.json"), []byte("x"), 0o600)
	os.WriteFile(filepath.Join(f.ram, "minerd/settings/.rgbcontrol.json"), []byte("{}"), 0o600)
	oemPersist(true)
	for _, n := range []string{"minerd/settings/.login.json", "minerd/settings/.rgbcontrol.json"} {
		if _, ok := f.onFlash(n); ok {
			t.Fatalf("%s was saved", n)
		}
	}
}
