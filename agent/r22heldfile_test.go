package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// v1.5.22 field report: "a firmware update to v1.5.22 failed: writing /usr/libexec/sclite/hook/intminer: ...
// no space left on device". The miner runs from that file; removing it for room freed nothing while the
// miner kept running from it. Now the programs holding it are stopped so its blocks come free.
func TestR22HeldFileFreedForRoom(t *testing.T) {
	root := t.TempDir()
	proc := t.TempDir()
	full := filepath.Join(root, "usr/libexec/sclite/hook/intminer")
	os.MkdirAll(filepath.Dir(full), 0o755)
	os.WriteFile(full, []byte("old miner"), 0o755)

	mk := func(pid, exe, maps string) {
		d := filepath.Join(proc, pid)
		os.MkdirAll(d, 0o755)
		if exe != "" {
			os.Symlink(exe, filepath.Join(d, "exe"))
		}
		os.WriteFile(filepath.Join(d, "maps"), []byte(maps), 0o644)
	}
	mk("4242", full+" (deleted)", "00010000-00200000 r-xp 00000000 1f:02 123 "+full+" (deleted)\n") // the miner
	mk("4343", "/usr/bin/other", "76f00000-76f02000 r-xp 00000000 1f:02 99 "+full+"\n")             // maps it
	mk("5555", "/usr/bin/minerd", "00010000-00020000 r-xp 00000000 1f:02 7 /usr/bin/minerd\n")      // unrelated
	mk("1", full, "")                                                                               // init: never
	mk("self", "", "")                                                                              // not a pid

	var signalled []int
	oldProc, oldSig, oldFree, oldWait, oldRoot := updProcDir, updSignal, updFreeAt, updWaitFor, updateRoot
	defer func() {
		updProcDir, updSignal, updFreeAt, updWaitFor, updateRoot = oldProc, oldSig, oldFree, oldWait, oldRoot
	}()
	updProcDir, updWaitFor, updateRoot = proc, 300*time.Millisecond, root
	updSignal = func(pid int, sig syscall.Signal) error {
		signalled = append(signalled, pid)
		if sig == syscall.SIGTERM && pid == 4343 {
			return nil // ignores TERM: gets KILL
		}
		return os.RemoveAll(filepath.Join(proc, strconv.Itoa(pid)))
	}
	got := updHolders(full)
	sort.Ints(got)
	if len(got) != 2 || got[0] != 4242 || got[1] != 4343 {
		t.Fatalf("holders: %v", got)
	}

	updFreeAt = func(string) int64 { return 1 << 10 } // short of room: remove first, free what holds it
	data := bytes.Repeat([]byte("new miner "), 100)
	s := sha256.Sum256(data)
	if err := writeReplace(full, bytes.NewReader(data), 0o755, int64(len(data)), hex.EncodeToString(s[:])); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(full); !bytes.Equal(b, data) {
		t.Fatal("new file not written")
	}
	if len(updHolders(full)) != 0 {
		t.Fatal("something still holds the old file")
	}
	for _, p := range signalled {
		if p == 5555 || p == 1 {
			t.Fatalf("signalled pid %d, which doesn't hold the file", p)
		}
	}
	if len(signalled) != 3 { // 4242 TERM, 4343 TERM, 4343 KILL
		t.Fatalf("signals: %v", signalled)
	}

	// room enough: written beside and renamed, nobody stopped
	signalled = nil
	mk("4242", full, "")
	updFreeAt = func(string) int64 { return 1 << 30 }
	if err := writeReplace(full, bytes.NewReader(data), 0o755, int64(len(data)), ""); err != nil || len(signalled) != 0 {
		t.Fatalf("err %v, signalled %v", err, signalled)
	}
}
