package main

// B4 — crash-safe FAT config storage, path retargeting, wear reduction, remount guards.
// Proves: fatWrite is crash-safe (a failed write keeps the old file / the .bak); loaders fall back to
// .bak; the SC Lite ext path is unchanged (no .bak, same bytes); squashfs detection skips the rw remount;
// the wear-reduction cadence is hourly on FAT and the original fast cadence on ext; stale *.tmp are cleared.

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// withFATMode runs fn with configFAT forced, restoring it after (the tests share process state).
func withFATMode(t *testing.T, on bool, fn func()) {
	t.Helper()
	old := configFAT
	configFAT = on
	defer func() { configFAT = old }()
	fn()
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return string(b)
}

// --- fatWrite crash-safety -------------------------------------------------

func TestFATWriteKeepsBak(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "net.json")
	if err := fatWrite(p, []byte(`{"v":1}`), 0o644); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if _, err := os.Stat(p + ".bak"); err == nil {
		t.Fatalf("no .bak should exist after the first write (nothing to back up)")
	}
	if err := fatWrite(p, []byte(`{"v":2}`), 0o644); err != nil {
		t.Fatalf("second write: %v", err)
	}
	if got := readFile(t, p); got != `{"v":2}` {
		t.Fatalf("primary = %q, want v2", got)
	}
	if got := readFile(t, p+".bak"); got != `{"v":1}` {
		t.Fatalf(".bak = %q, want the previous content v1", got)
	}
	// the temp file must never be left behind after a good write
	if _, err := os.Stat(p + ".tmp"); err == nil {
		t.Fatalf("a .tmp was left behind after a successful write")
	}
}

// A failed write (the temp can't be created) must leave the existing file and its .bak untouched: nothing
// half-written, no data loss. We make the write fail by giving a path whose parent is a regular file.
func TestFATWriteFailureKeepsOld(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "fan.json")
	if err := fatWrite(p, []byte(`GOOD`), 0o644); err != nil {
		t.Fatalf("seed write: %v", err)
	}
	// now point a write under p (a file) as if p were a directory -> OpenFile on p/x.tmp fails
	bad := filepath.Join(p, "child.json")
	if err := fatWrite(bad, []byte(`NEW`), 0o644); err == nil {
		t.Fatalf("expected the write to fail when the parent is a file")
	}
	if got := readFile(t, p); got != `GOOD` {
		t.Fatalf("the existing file changed after a failed write: %q", got)
	}
	// and no stray temp inside a place it couldn't create
	if _, err := os.Stat(bad + ".tmp"); err == nil {
		t.Fatalf("a .tmp survived a failed write")
	}
}

// Simulate a power cut *during* a write: the new content only reached the .tmp, the rename never happened,
// and (worse) the primary got truncated. readConfig must recover the last good copy from .bak.
func TestLoaderFallsBackToBak(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "auth.json")
	// last good copy
	os.WriteFile(p+".bak", []byte(`{"user":"admin"}`), 0o644)
	// primary damaged by a crash mid-write (empty / truncated / invalid JSON)
	os.WriteFile(p, []byte(`{"user":`), 0o644)
	// a leftover temp from the interrupted write
	os.WriteFile(p+".tmp", []byte(`{"user":"a`), 0o644)

	b, ok := readConfig(p, nil) // nil => valid-JSON check
	if !ok {
		t.Fatalf("readConfig gave up; it should have used .bak")
	}
	if string(b) != `{"user":"admin"}` {
		t.Fatalf("readConfig = %q, want the .bak content", string(b))
	}
}

func TestLoaderPrefersPrimaryWhenValid(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.json")
	os.WriteFile(p, []byte(`{"ok":true}`), 0o644)
	os.WriteFile(p+".bak", []byte(`{"ok":false}`), 0o644)
	b, ok := readConfig(p, nil)
	if !ok || string(b) != `{"ok":true}` {
		t.Fatalf("readConfig = %q/%v, want the primary", string(b), ok)
	}
}

func TestLoaderNoFileNoBak(t *testing.T) {
	dir := t.TempDir()
	if _, ok := readConfig(filepath.Join(dir, "missing.json"), nil); ok {
		t.Fatalf("readConfig should report false when neither file exists")
	}
}

// --- SC Lite ext path unchanged -------------------------------------------

// On ext (configFAT=false) writeAtomic must be the old plain atomic write: temp+rename, and crucially NO
// .bak file (SC Lite never kept one for these writers). On FAT it must keep a .bak.
func TestWriteAtomicRoutingByFS(t *testing.T) {
	dir := t.TempDir()

	withFATMode(t, false, func() {
		p := filepath.Join(dir, "ext.json")
		if err := writeAtomic(p, []byte(`A`), 0o644); err != nil {
			t.Fatalf("ext write: %v", err)
		}
		if err := writeAtomic(p, []byte(`B`), 0o644); err != nil {
			t.Fatalf("ext write 2: %v", err)
		}
		if got := readFile(t, p); got != `B` {
			t.Fatalf("ext primary = %q", got)
		}
		if _, err := os.Stat(p + ".bak"); err == nil {
			t.Fatalf("ext (SC Lite) writeAtomic must NOT create a .bak — behaviour changed")
		}
	})

	withFATMode(t, true, func() {
		p := filepath.Join(dir, "fat.json")
		writeAtomic(p, []byte(`A`), 0o644)
		writeAtomic(p, []byte(`B`), 0o644)
		if got := readFile(t, p+".bak"); got != `A` {
			t.Fatalf("FAT writeAtomic .bak = %q, want A", got)
		}
	})
}

func TestCfgWriteRoutingByFS(t *testing.T) {
	dir := t.TempDir()
	withFATMode(t, true, func() {
		p := filepath.Join(dir, "c.json")
		cfgWrite(p, []byte(`1`), 0o644)
		cfgWrite(p, []byte(`2`), 0o644)
		if _, err := os.Stat(p + ".bak"); err != nil {
			t.Fatalf("cfgWrite on FAT should keep a .bak: %v", err)
		}
	})
	withFATMode(t, false, func() {
		p := filepath.Join(dir, "c2.json")
		cfgWrite(p, []byte(`1`), 0o644)
		cfgWrite(p, []byte(`2`), 0o644)
		if _, err := os.Stat(p + ".bak"); err == nil {
			t.Fatalf("cfgWrite on ext must not keep a .bak")
		}
	})
}

// --- filesystem detection + squashfs remount guard ------------------------

func TestFSDetectionDiscriminates(t *testing.T) {
	dir := t.TempDir() // a real (tmpfs/ext/overlay) dir — never FAT, never squashfs
	if isFAT(dir) {
		t.Fatalf("a scratch dir reported as FAT")
	}
	if isSquashfs(dir) {
		t.Fatalf("a scratch dir reported as squashfs")
	}
	if fsMagic(dir) == 0 {
		t.Fatalf("statfs of a real dir returned 0 (lookup failed)")
	}
	if fsMagic("/no/such/path/at/all") != 0 {
		t.Fatalf("statfs of a missing path should return 0")
	}
}

func TestSquashfsMagicConstants(t *testing.T) {
	// guard against a typo in the kernel magic numbers (used to skip the rw remount)
	if magicSquashfs != 0x73717368 {
		t.Fatalf("squashfs magic wrong: %#x", magicSquashfs)
	}
	if magicMSDOS != 0x4d44 {
		t.Fatalf("FAT magic wrong: %#x", magicMSDOS)
	}
}

// With a squashfs root, installService must refuse cleanly (no remount, no write) rather than crash.
func TestInstallServiceRefusedOnSquashfs(t *testing.T) {
	old := fsMagic
	defer func() { fsMagic = old }()
	fsMagic = func(path string) int64 { return magicSquashfs } // the whole world looks read-only
	err := installService()
	if err == nil {
		t.Fatalf("installService should refuse on a squashfs root")
	}
	if _, statErr := os.Stat("/etc/init.d/S48sclite"); statErr == nil {
		t.Fatalf("installService wrote the boot script despite a read-only root")
	}
}

// initConfigStore is model-aware: a Box II profile selects FAT writes even when the real fs is not FAT, and
// it records a squashfs root. SC Lite on ext selects the ext (unchanged) path.
func TestInitConfigStoreModelAware(t *testing.T) {
	oldModel, oldCFG, oldMagic, oldAuth := model, configFAT, fsMagic, *authFile
	defer func() {
		model, configFAT, fsMagic = oldModel, oldCFG, oldMagic
		*authFile = oldAuth
	}()

	dir := t.TempDir()
	af := filepath.Join(dir, "sclite", "auth.json")
	os.MkdirAll(filepath.Dir(af), 0o755)
	*authFile = af

	// Box II: config FAT by model even though the scratch dir isn't FAT; root looks like squashfs.
	fsMagic = func(path string) int64 {
		if path == "/" {
			return magicSquashfs
		}
		return magicTmpfs
	}
	model = profSCBox2
	initConfigStore()
	if !configFAT {
		t.Fatalf("Box II profile should select FAT config writes")
	}

	// SC Lite on ext: not FAT, not squashfs.
	fsMagic = func(path string) int64 { return magicTmpfs }
	model = profSCLite
	initConfigStore()
	if configFAT {
		t.Fatalf("SC Lite on ext must not select FAT writes (behaviour must be unchanged)")
	}
}

// A FAT config detected by statfs alone (any model) also selects crash-safe writes.
func TestInitConfigStoreFATByStatfs(t *testing.T) {
	oldModel, oldCFG, oldMagic, oldAuth := model, configFAT, fsMagic, *authFile
	defer func() { model, configFAT, fsMagic, *authFile = oldModel, oldCFG, oldMagic, oldAuth }()
	dir := t.TempDir()
	*authFile = filepath.Join(dir, "auth.json")
	fsMagic = func(path string) int64 { return magicMSDOS }
	model = profSCLite // model says ext, but the store is really FAT
	initConfigStore()
	if !configFAT {
		t.Fatalf("a FAT config store should select crash-safe writes even on the SC Lite profile")
	}
}

// --- wear-reduction cadence ------------------------------------------------

func TestCheckpointCadenceByFS(t *testing.T) {
	withFATMode(t, true, func() {
		if hrSaveGap() != time.Hour {
			t.Fatalf("hashrate checkpoint on FAT = %v, want hourly", hrSaveGap())
		}
		if eventsSaveGap() != time.Hour {
			t.Fatalf("events checkpoint on FAT = %v, want hourly", eventsSaveGap())
		}
		if cfgCheckpointGap(time.Hour, time.Minute) != time.Hour {
			t.Fatalf("cfgCheckpointGap on FAT should pick the slow (RAM-sparing) gap")
		}
	})
	withFATMode(t, false, func() {
		if hrSaveGap() != 5*time.Minute {
			t.Fatalf("hashrate checkpoint on ext = %v, want the original 5 min", hrSaveGap())
		}
		if eventsSaveGap() != evSaveGap {
			t.Fatalf("events checkpoint on ext = %v, want the original %v", eventsSaveGap(), evSaveGap)
		}
		if cfgCheckpointGap(time.Hour, time.Minute) != time.Minute {
			t.Fatalf("cfgCheckpointGap on ext should pick the original fast gap")
		}
	})
}

// --- stale temp cleanup ----------------------------------------------------

func TestCleanStaleTmp(t *testing.T) {
	dir := t.TempDir()
	keep := filepath.Join(dir, "auth.json")
	tmp := filepath.Join(dir, "auth.json.tmp")
	os.WriteFile(keep, []byte(`{}`), 0o644)
	os.WriteFile(tmp, []byte(`half`), 0o644)
	// one level down (e.g. tuner/<miner>/live.csv.tmp)
	sub := filepath.Join(dir, "tuner")
	os.MkdirAll(sub, 0o755)
	subTmp := filepath.Join(sub, "run.csv.tmp")
	subKeep := filepath.Join(sub, "run.csv")
	os.WriteFile(subTmp, []byte(`half`), 0o644)
	os.WriteFile(subKeep, []byte(`ok`), 0o644)

	cleanStaleTmp(dir)

	if _, err := os.Stat(tmp); err == nil {
		t.Fatalf("top-level .tmp not removed")
	}
	if _, err := os.Stat(subTmp); err == nil {
		t.Fatalf("nested .tmp not removed")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("a real config file was removed: %v", err)
	}
	if _, err := os.Stat(subKeep); err != nil {
		t.Fatalf("a real nested file was removed: %v", err)
	}
}

// --- hostname redirect target ---------------------------------------------

// On a squashfs root the hostname is persisted to a file on the config partition (that the boot script
// symlinks /etc/hostname to), not to the read-only rootfs. Here we check the target is inside the config
// dir so the kit lane can wire the symlink.
func TestHostnameCfgPathInConfigDir(t *testing.T) {
	old := *authFile
	defer func() { *authFile = old }()
	*authFile = "/mnt/config/sclite/auth.json"
	if got, want := hostnameCfgPath(), "/mnt/config/sclite/hostname"; got != want {
		t.Fatalf("hostnameCfgPath = %q, want %q", got, want)
	}
}
