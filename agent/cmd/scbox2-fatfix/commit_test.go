package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"sclite-agent/internal/cfgwin"
)

// "scbox2-fatfix commit": the boot scripts' save (cfgwrite) goes through the same window as the panel's.
func TestCommitCLI(t *testing.T) {
	d := t.TempDir()
	ram, flash := filepath.Join(d, "ram"), filepath.Join(d, "flash")
	os.MkdirAll(filepath.Join(ram, "scbox2"), 0o700)
	os.MkdirAll(flash, 0o700)
	os.WriteFile(filepath.Join(ram, "random-seed"), []byte("seed"), 0o600)
	os.WriteFile(filepath.Join(ram, "scbox2", "mac"), []byte("02:00:00:00:00:01\n"), 0o600)
	var ops []string
	oA, oR := cfgwin.ActiveHook, cfgwin.Remount
	cfgwin.ActiveHook = func() bool { return true }
	cfgwin.Remount = func(dir string, rw bool) error {
		if dir != flash {
			t.Errorf("remount of %s", dir)
		}
		ops = append(ops, map[bool]string{true: "rw", false: "ro"}[rw])
		return nil
	}
	defer func() { cfgwin.ActiveHook, cfgwin.Remount = oA, oR }()
	var out bytes.Buffer
	args := []string{"commit", "-ram", ram, "-flash", flash, "-lock", filepath.Join(d, "lock")}
	if rc := run(append(args, "random-seed", "/scbox2/mac"), &out); rc != 0 {
		t.Fatalf("exit %d: %s", rc, out.String())
	}
	if b, _ := os.ReadFile(filepath.Join(flash, "random-seed")); string(b) != "seed" {
		t.Fatal("the seed didn't land")
	}
	if b, _ := os.ReadFile(filepath.Join(flash, "scbox2", "mac")); string(b) != "02:00:00:00:00:01\n" {
		t.Fatal("the MAC didn't land")
	}
	if strings.Join(ops, ",") != "rw,ro" || !strings.Contains(out.String(), "config window: rw, 2 file(s) written") {
		t.Fatalf("ops %v, out %s", ops, out.String())
	}
	// the window can't be opened: exit 1, said plainly
	cfgwin.Remount = func(string, bool) error { return syscall.EROFS }
	os.WriteFile(filepath.Join(ram, "random-seed"), []byte("seed2"), 0o600)
	out.Reset()
	if rc := run(append(args, "random-seed"), &out); rc != 1 || !strings.Contains(out.String(), "NOT SAVED") {
		t.Fatalf("exit %d: %s", rc, out.String())
	}
	// the whole partition: refused
	out.Reset()
	if rc := run(append(args, "/"), &out); rc != 1 {
		t.Fatalf("the whole partition: exit %d", rc)
	}
	// no RAM copy in use: nothing to do, exit 0
	cfgwin.ActiveHook = func() bool { return false }
	out.Reset()
	if rc := run(append(args, "random-seed"), &out); rc != 0 || !strings.Contains(out.String(), "nothing to do") {
		t.Fatalf("inactive: exit %d: %s", rc, out.String())
	}
}
