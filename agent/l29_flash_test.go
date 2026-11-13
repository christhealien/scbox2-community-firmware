package main

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// L29-flash: the config partition (/usr/config, ext4 on the eMMC) full or read-only.

// l29Mount: a small tmpfs as the config folder (root only; skipped elsewhere). ro remounts it read-only;
// fill takes all its room (ENOSPC for any new data).
func l29Mount(t *testing.T) (dir string, ro func(), fill func()) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root to mount a tmpfs")
	}
	dir = t.TempDir()
	if err := syscall.Mount("tmpfs", dir, "tmpfs", 0, "size=256k"); err != nil {
		t.Skipf("can't mount a tmpfs: %v", err)
	}
	t.Cleanup(func() { syscall.Unmount(dir, syscall.MNT_DETACH) })
	ro = func() {
		if err := syscall.Mount("", dir, "", syscall.MS_REMOUNT|syscall.MS_RDONLY, ""); err != nil {
			t.Fatalf("remount ro: %v", err)
		}
	}
	fill = func() {
		f, err := os.Create(filepath.Join(dir, "filler"))
		if err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 4096)
		for {
			if _, err := f.Write(buf); err != nil {
				break
			}
		}
		f.Close()
	}
	return dir, ro, fill
}

// The fan lock lives only in fan-lock.json. With the config partition read-only (or full), the lock is never
// on disk: fanLockErr says "not locked" right after the miner was put on Idle for a failed fan (presets, Stock,
// the schedule and the tuner may start hashing again), and every 15 s tick engages the lock again: a new Idle
// request, a new activity line and notification attempt, again and again.
func TestL29FanLockHoldsWhenConfigReadOnly(t *testing.T) {
	write, idled := r4LockEnv(t)
	cfg, ro, _ := l29Mount(t)
	*authFile = filepath.Join(cfg, "auth.json")
	notes := 0
	fanLockNotify = func(string) { notes++ }
	ro()
	now := time.Now().Add(-5 * time.Minute)
	for i := 0; i < 12; i++ { // 3 minutes of a flagged fan, 15 s apart
		now = now.Add(15 * time.Second)
		write(now, r4Bad)
		fanLockTick(now)
	}
	if fanLockErr() == nil {
		t.Errorf("BUG: the fan lock engaged (Idle requested %d times) but mining isn't locked: the lock file couldn't be written", *idled)
	}
	if *idled != 1 || notes != 1 {
		t.Errorf("BUG: the lock engaged %d times (%d notifications) in 3 minutes: once expected", *idled, notes)
	}
	// unlock (fans fixed) works without the file
	for i := 0; i < 4; i++ {
		now = now.Add(15 * time.Second)
		write(now, r4Good)
	}
	if err := fanUnlock(true, now); err != nil {
		t.Errorf("unlock refused: %v", err)
	}
	if fanLockErr() != nil {
		t.Error("still locked after the unlock")
	}
}

// The pool guard's hold on Idle is only pool-hold.json: on a read-only/full config partition it never holds,
// so presets, Stock and the schedule can wake the miner while a pool nobody entered keeps coming back.
func TestL29PoolHoldWhenConfigFull(t *testing.T) {
	cfg, _, fill := l29Mount(t)
	old := *authFile
	*authFile = filepath.Join(cfg, "auth.json")
	resume := func() {
		r := httptest.NewRequest("POST", "/api/poolguard", strings.NewReader(`{"resume":true}`))
		handlePoolGuard(httptest.NewRecorder(), r)
	}
	t.Cleanup(func() { resume(); *authFile = old })
	fill()
	poolHoldSet("stratum+tcp://evil:3333 (account x)")
	if poolHoldErr() == nil {
		t.Fatal("BUG: the pool guard set its hold, but with the config partition full nothing holds the miner on Idle")
	}
	if left, _ := filepath.Glob(filepath.Join(cfg, "*.tmp")); len(left) > 0 {
		t.Errorf("a failed write left %v", left)
	}
	resume()
	if poolHoldErr() != nil {
		t.Error("still held after Resume")
	}
}

// A failed saveAuth (full partition) leaves auth.json.tmp behind (writeAtomic removes its temp file; saveAuth
// doesn't), taking room on the full partition.
func TestL29SaveAuthFullLeavesNoTemp(t *testing.T) {
	cfg, _, fill := l29Mount(t)
	old := *authFile
	*authFile = filepath.Join(cfg, "auth.json")
	t.Cleanup(func() { *authFile = old })
	if err := saveAuth(); err != nil {
		t.Fatal(err)
	}
	authMu.Lock()
	saved := auth
	auth.Acknowledged = strings.Repeat("x", 64<<10) // a bigger file than the room left
	authMu.Unlock()
	t.Cleanup(func() { authMu.Lock(); auth = saved; authMu.Unlock() })
	fill()
	if err := saveAuth(); err == nil {
		t.Fatal("setup: saved on a full partition")
	}
	if _, err := os.Stat(*authFile + ".tmp"); err == nil {
		t.Error("BUG: auth.json.tmp left behind after a failed save")
	}
	if b, err := os.ReadFile(*authFile); err != nil || !strings.Contains(string(b), "username") {
		t.Errorf("auth.json damaged by the failed save: %v", err)
	}
}

// recordMark keeps "X" (rejected share) marks for the 5-minute save because they can come often, but the
// pool's reason attached to each one (scanRejects, every 20 s) rewrote the whole hashrate.json (~60 KB with a
// full day) at once: with a pool rejecting steadily, up to 3 rewrites a minute (~4300 a day) instead of 288.
func TestL29RejectReasonWaitsForTheSave(t *testing.T) {
	resetMarks(t)
	for i := 0; i < 3; i++ {
		recordMark("X", "share rejected (+1)")
		scanRejects([]string{` [x] Pool 0: RECV: {"error":"stale","id":7,"result":false}`})
		hrMu.Lock()
		hrMarks[len(hrMarks)-1].T -= 10 // (past the same-kind dedup)
		hrMu.Unlock()
	}
	if _, err := os.Stat(*hrPath); err == nil {
		t.Error("BUG: hashrate.json rewritten for a rejected share's reason (it goes with the 5-minute save)")
	}
	hrMu.Lock()
	defer hrMu.Unlock()
	if n := len(hrMarks); n == 0 || !strings.Contains(hrMarks[n-1].Note, "pool said: stale") {
		t.Errorf("the reason wasn't attached: %+v", hrMarks)
	}
}

// A power cut while an incident snapshot is being written leaves incident-<time>.txt.gz.tmp: its name is new
// each time, so nothing ever writes over or removes it (the pruning only counts *.txt.gz) and it stays on the
// small config partition for good.
func TestL29IncidentTempLeftoverRemoved(t *testing.T) {
	old := *authFile
	*authFile = filepath.Join(t.TempDir(), "auth.json")
	oldL := incidentLogs
	incidentLogs = func() map[string]string { return map[string]string{"mining-log": "x"} }
	t.Cleanup(func() { *authFile, incidentLogs = old, oldL })
	os.MkdirAll(incidentsDir(), 0o755)
	left := filepath.Join(incidentsDir(), "incident-20260101-000000.txt.gz.tmp")
	os.WriteFile(left, make([]byte, 100<<10), 0o644)
	incidentSnapshot("board 1 stopped hashing")
	if _, err := os.Stat(left); err == nil {
		t.Error("BUG: a power cut's half-written incident file stays on the config partition for good")
	}
}
