package cfgwin

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// fake: RAMRoot and FlashRoot in a temp dir, the remounts recorded (and the flash "read-only" outside a window:
// a write to it then fails the test).
type fake struct {
	mu    sync.Mutex
	ops   []string
	rw    bool
	fail  error // the next rw remount fails with this
	roErr error // every ro remount fails with this
}

func setup(t *testing.T) (*fake, string, string) {
	t.Helper()
	d := t.TempDir()
	ram, flash := filepath.Join(d, "ram"), filepath.Join(d, "flash")
	os.MkdirAll(ram, 0o700)
	os.MkdirAll(flash, 0o700)
	f := &fake{}
	oR, oF, oL, oRm, oA, oW := RAMRoot, FlashRoot, LockPath, Remount, ActiveHook, LockWait
	RAMRoot, FlashRoot, LockPath = ram, flash, filepath.Join(d, "lock")
	ActiveHook = func() bool { return true }
	Remount = func(dir string, rw bool) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		if dir != FlashRoot {
			t.Errorf("remount of %s, not the flash", dir)
		}
		if rw && f.fail != nil {
			err := f.fail
			f.fail = nil
			f.ops = append(f.ops, "rw-failed")
			return err
		}
		if !rw && f.roErr != nil {
			f.ops = append(f.ops, "ro-failed")
			return f.roErr
		}
		f.rw = rw
		f.ops = append(f.ops, map[bool]string{true: "rw", false: "ro"}[rw])
		return nil
	}
	t.Cleanup(func() { RAMRoot, FlashRoot, LockPath, Remount, ActiveHook, LockWait = oR, oF, oL, oRm, oA, oW })
	return f, ram, flash
}

func (f *fake) seq() string { f.mu.Lock(); defer f.mu.Unlock(); return strings.Join(f.ops, ",") }

func put(t *testing.T, p, s string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(p), 0o700)
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}

func read(p string) string { b, _ := os.ReadFile(p); return string(b) }

// A commit: rw, the file (with the old one kept as .bak), ro again; a second commit of the same content: no window.
func TestCommitWindow(t *testing.T) {
	f, ram, flash := setup(t)
	put(t, filepath.Join(flash, "scbox2/fan.json"), "old")
	put(t, filepath.Join(ram, "scbox2/fan.json"), "new")
	n := Windows()
	if err := Commit("scbox2/fan.json"); err != nil {
		t.Fatal(err)
	}
	if f.seq() != "rw,ro" || Windows() != n+1 {
		t.Fatalf("remounts %q (windows %d)", f.seq(), Windows()-n)
	}
	if read(filepath.Join(flash, "scbox2/fan.json")) != "new" || read(filepath.Join(flash, "scbox2/fan.json.bak")) != "old" {
		t.Fatal("the file didn't land with its .bak")
	}
	if _, err := os.Stat(filepath.Join(flash, "scbox2/fan.json.tmp")); err == nil {
		t.Fatal("a .tmp was left")
	}
	if err := Commit("scbox2/fan.json"); err != nil || f.seq() != "rw,ro" || Windows() != n+1 {
		t.Fatalf("the same content opened a window: %q %v", f.seq(), err)
	}
	if p := Pending("scbox2/fan.json"); len(p) != 0 {
		t.Fatalf("pending %v", p)
	}
}

// A file gone from RAM goes from the flash (with its .bak); a folder is mirrored, temp and .bak files left out.
func TestCommitRemoveAndFolder(t *testing.T) {
	f, ram, flash := setup(t)
	put(t, filepath.Join(flash, "scbox2/pool-hold.json"), "held")
	put(t, filepath.Join(flash, "scbox2/pool-hold.json.bak"), "held before")
	if err := Commit("scbox2/pool-hold.json"); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"pool-hold.json", "pool-hold.json.bak"} {
		if _, err := os.Stat(filepath.Join(flash, "scbox2", n)); err == nil {
			t.Fatalf("%s is still on the flash", n)
		}
	}
	put(t, filepath.Join(ram, "scbox2/tuner/a/log.txt"), "run")
	put(t, filepath.Join(ram, "scbox2/tuner/a/log.txt.tmp"), "half")
	put(t, filepath.Join(ram, "scbox2/x.json.bak"), "ram bak")
	put(t, filepath.Join(flash, "scbox2/tuner/gone.txt"), "old run")
	put(t, filepath.Join(flash, "scbox2/stale.tmp"), "half")
	if err := Commit("scbox2"); err != nil {
		t.Fatal(err)
	}
	if read(filepath.Join(flash, "scbox2/tuner/a/log.txt")) != "run" {
		t.Fatal("the folder's file didn't land")
	}
	for _, n := range []string{"tuner/a/log.txt.tmp", "x.json.bak", "tuner/gone.txt", "stale.tmp"} {
		if _, err := os.Stat(filepath.Join(flash, "scbox2", n)); err == nil {
			t.Fatalf("%s is on the flash", n)
		}
	}
	if f.seq() != "rw,ro,rw,ro" {
		t.Fatalf("remounts %q", f.seq())
	}
}

// The partition can't be made writable: the save fails as "read-only", nothing is written, no ro remount needed.
func TestCommitRemountFails(t *testing.T) {
	f, ram, flash := setup(t)
	put(t, filepath.Join(ram, "scbox2/net.json"), "{}")
	f.fail = syscall.EIO
	err := Commit("scbox2/net.json")
	if err == nil || !errors.Is(err, syscall.EROFS) || !errors.Is(err, syscall.EIO) || !strings.Contains(err.Error(), "could not be made writable") {
		t.Fatalf("err %v", err)
	}
	if _, err := os.Stat(filepath.Join(flash, "scbox2/net.json")); err == nil {
		t.Fatal("written without a window")
	}
	if f.seq() != "rw-failed" {
		t.Fatalf("remounts %q", f.seq())
	}
	// the next one works
	if err := Commit("scbox2/net.json"); err != nil || read(filepath.Join(flash, "scbox2/net.json")) != "{}" {
		t.Fatalf("retry: %v", err)
	}
}

// A write that fails inside the window: reported, and the partition is read-only again all the same.
func TestCommitWriteFailsStillRO(t *testing.T) {
	f, ram, flash := setup(t)
	put(t, filepath.Join(ram, "scbox2/a.json"), "a")
	put(t, filepath.Join(flash, "scbox2"), "a file where the folder should be")
	os.Chmod(flash, 0o500) // (root ignores this: the file-for-a-folder makes it fail)
	defer os.Chmod(flash, 0o700)
	err := Commit("scbox2/a.json")
	if err == nil {
		t.Fatal("no error")
	}
	if f.seq() != "rw,ro" {
		t.Fatalf("remounts %q", f.seq())
	}
}

// Remounting read-only keeps failing: the save still counts (the data is on the flash), retried, logged.
func TestCommitROFailsLogged(t *testing.T) {
	f, ram, flash := setup(t)
	var logged []string
	Logf = func(s string, a ...any) { logged = append(logged, s) }
	defer func() { Logf = nil }()
	put(t, filepath.Join(ram, "k"), "v")
	f.roErr = syscall.EBUSY
	if err := Commit("k"); err != nil {
		t.Fatal(err)
	}
	if read(filepath.Join(flash, "k")) != "v" {
		t.Fatal("not written")
	}
	if c := strings.Count(f.seq(), "ro-failed"); c != 20 {
		t.Fatalf("%d ro tries", c)
	}
	if !strings.Contains(strings.Join(logged, "\n"), "stays read-write") {
		t.Fatalf("not logged: %q", logged)
	}
}

// One window at a time, across goroutines and across processes (the flock): never two rw at once.
func TestCommitSerialized(t *testing.T) {
	_, ram, _ := setup(t)
	var mu sync.Mutex
	open, maxOpen := 0, 0
	inner := Remount
	Remount = func(dir string, rw bool) error {
		mu.Lock()
		if rw {
			open++
			if open > maxOpen {
				maxOpen = open
			}
		} else {
			open--
		}
		mu.Unlock()
		time.Sleep(2 * time.Millisecond)
		return inner(dir, rw)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			n := filepath.Join("f", string(rune('a'+i)))
			put(t, filepath.Join(ram, n), strings.Repeat("x", i+1))
			if err := Commit(n); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	if maxOpen != 1 {
		t.Fatalf("%d windows open at once", maxOpen)
	}
	// another program holds the lock: this one waits, then gives up with a clear error
	f2, err := os.OpenFile(LockPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f2.Close()
	other, _ := os.Open(LockPath) // a second open file description: its flock conflicts as another process's would
	defer other.Close()
	if err := syscall.Flock(int(other.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	LockWait = 100 * time.Millisecond
	put(t, filepath.Join(ram, "late"), "x")
	err = Commit("late")
	if err == nil || !strings.Contains(err.Error(), "another save") {
		t.Fatalf("err %v", err)
	}
	syscall.Flock(int(other.Fd()), syscall.LOCK_UN)
	if err := Commit("late"); err != nil {
		t.Fatal(err)
	}
}

// Not a RAM copy (another model, or the partition used directly): nothing happens.
func TestInactiveNoop(t *testing.T) {
	f, ram, flash := setup(t)
	ActiveHook = func() bool { return false }
	put(t, filepath.Join(ram, "x"), "1")
	if err := Commit("x"); err != nil || f.seq() != "" {
		t.Fatalf("%v %q", err, f.seq())
	}
	if _, err := os.Stat(filepath.Join(flash, "x")); err == nil {
		t.Fatal("written")
	}
}

// Rel maps the panel's /usr/config paths and refuses anything outside the RAM copy.
func TestRel(t *testing.T) {
	_, _, _ = setup(t)
	old := Aliases
	Aliases = []string{"/usr/config"}
	defer func() { Aliases = old }()
	RAMRoot = "/mnt/config"
	for in, want := range map[string]string{
		"/usr/config/scbox2/auth.json": "scbox2/auth.json",
		"/mnt/config/random-seed":      "random-seed",
		"/mnt/config/../etc/passwd":    "",
		"/mnt/config":                  "",
		"/tmp/x":                       "",
		"/usr/configx/a":               "",
	} {
		got, ok := Rel(in)
		if got != want || ok != (want != "") {
			t.Errorf("Rel(%q) = %q %v, want %q", in, got, ok, want)
		}
	}
}

// Active reads /proc/mounts: a tmpfs at the RAM path and the partition mounted.
func TestActiveFromMounts(t *testing.T) {
	_, _, _ = setup(t)
	ActiveHook = nil
	RAMRoot, FlashRoot = "/mnt/config", "/mnt/cfgflash"
	m := filepath.Join(t.TempDir(), "mounts")
	old := MountsPath
	MountsPath = m
	defer func() { MountsPath = old }()
	os.WriteFile(m, []byte("/dev/spiblock2 /mnt/cfgflash vfat ro,noatime 0 0\nscbox2-config /mnt/config tmpfs rw 0 0\n"), 0o644)
	if !Active() || !FlashReadOnly() {
		t.Fatal("not active / not ro")
	}
	os.WriteFile(m, []byte("/dev/spiblock2 /mnt/config vfat rw 0 0\n"), 0o644)
	if Active() || FlashReadOnly() {
		t.Fatal("the partition used directly counts as a RAM copy")
	}
}

// Trace: a window that opens is announced before the remount and closed after the read-only remount (v0.5.4's
// freeze-hunt breadcrumbs); one that has nothing to write isn't.
func TestCommitTraced(t *testing.T) {
	f, ram, flash := setup(t)
	put(t, filepath.Join(flash, "scbox2/fan.json"), "old")
	put(t, filepath.Join(ram, "scbox2/fan.json"), "new")
	var got []string
	Trace = func(what string) func(error) {
		got = append(got, "begin "+what+" after "+f.seq())
		return func(err error) { got = append(got, fmt.Sprintf("end %v after %s", err, f.seq())) }
	}
	defer func() { Trace = nil }()
	if err := Commit("scbox2/fan.json"); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !strings.HasPrefix(got[0], "begin ") || !strings.HasSuffix(got[0], "after ") || got[1] != "end <nil> after rw,ro" {
		t.Fatalf("trace %q", got)
	}
	if err := Commit("scbox2/fan.json"); err != nil || len(got) != 2 {
		t.Fatalf("a window with nothing to write was traced: %q %v", got, err)
	}
}
