package main

// L08 sweep: hostile / corrupt update packages and the staging state around them.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// l08Pkg: a package from a raw manifest (files get size/sha filled from data) and the data per path.
func l08Pkg(t *testing.T, dst string, man map[string]any, data map[string]string) {
	t.Helper()
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	mb, _ := json.Marshal(man)
	tw.WriteHeader(&tar.Header{Name: "manifest.json", Size: int64(len(mb)), Mode: 0o644, Typeflag: tar.TypeReg})
	tw.Write(mb)
	for _, f := range man["files"].([]map[string]any) {
		if f["type"] != "file" {
			continue
		}
		d := data[f["path"].(string)]
		tw.WriteHeader(&tar.Header{Name: "files" + f["path"].(string), Size: int64(len(d)), Mode: 0o644, Typeflag: tar.TypeReg})
		tw.Write([]byte(d))
	}
	tw.Close()
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write(raw.Bytes())
	zw.Close()
	if err := os.WriteFile(dst, gz.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func l08File(p, d string) map[string]any {
	s := sha256.Sum256([]byte(d))
	return map[string]any{"path": p, "type": "file", "mode": 0o755, "size": len(d), "sha256": hex.EncodeToString(s[:])}
}

func l08Man(base string, files []map[string]any, remove []string) map[string]any {
	return map[string]any{"format": 1, "product": brand + "-firmware", "version": "9.9.9",
		"base":  map[string]string{"path": "/usr/libexec/sclite/intminer", "sha256": base},
		"files": files, "remove": remove, "remove_dirs": []string{}}
}

// A path with shell glob/space/newline characters passes updPathOK. The boot script's rollback walks
// `for f in $(cat $U/created)` unquoted, so "/usr/c*nfig/sclite/auth.json" (a new file, listed in created)
// expands to the owner's real /usr/config/sclite/auth.json and is rm -f'd; a newline-split path is
// already removed by the panel's own restoreBackup. Such a package must be refused.
func TestL08UpdateRefusesShellUnsafePaths(t *testing.T) {
	_, base := setupRoot(t)
	dir := t.TempDir()
	for _, p := range []string{"/usr/c*nfig/sclite/auth.json", "/usr/bin/a b", "/usr/bin/x\n/etc/sclite-release",
		"/usr/bin/[ab]", "/usr/bin/?", "/usr/bin/$(reboot)"} {
		pkg := filepath.Join(dir, "p.tgz")
		l08Pkg(t, pkg, l08Man(base, []map[string]any{l08File(p, "x")}, nil), map[string]string{p: "x"})
		if _, err := verifyUpdate(pkg); err == nil {
			t.Errorf("a package writing %q was accepted", p)
		}
	}
	// the names the real images have stay allowed
	for _, p := range []string{"/usr/bin/[", "/usr/bin/[[", "/usr/share/zoneinfo/Etc/GMT+5", "/root/yottamc/js/a~b.3bd9d078.js"} {
		if !updPathOK(p) {
			t.Errorf("%q refused", p)
		}
	}
}

// The newline case end to end: the update "adds" /usr/bin/x\n/etc/sclite-release; rolling back (the panel's
// restoreBackup) deletes the real /etc/sclite-release, which the backup doesn't hold.
func TestL08NewlinePathRollbackDeletesSystemFile(t *testing.T) {
	root, base := setupRoot(t)
	p := "/usr/bin/x\n/etc/sclite-release"
	pkg := filepath.Join(t.TempDir(), "p.tgz")
	l08Pkg(t, pkg, l08Man(base, []map[string]any{l08File(p, "x")}, nil), map[string]string{p: "x"})
	if _, err := installUpdate(pkg); err != nil {
		return // refused: fine
	}
	restoreBackup()
	if readF(root, "/etc/sclite-release") == "" {
		t.Fatal("rolling back a newline-path package deleted /etc/sclite-release")
	}
}

// The base check reads m.Base.Path whole with os.ReadFile, whatever it is: a FIFO blocks the check forever
// (the panel stays "receiving"), /dev/zero would read until the panel is OOM-killed.
func TestL08BaseMustBeARegularFile(t *testing.T) {
	root, _ := setupRoot(t)
	fifo := filepath.Join(root, "usr/libexec/sclite/fifo")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Skip(err)
	}
	t.Cleanup(func() { // unblock a reader stuck on the FIFO
		if f, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			f.Close()
		}
	})
	man := l08Man(strings.Repeat("0", 64), []map[string]any{l08File("/usr/bin/sclite-agent", "a")}, nil)
	man["base"] = map[string]string{"path": "/usr/libexec/sclite/fifo", "sha256": strings.Repeat("0", 64)}
	pkg := filepath.Join(t.TempDir(), "p.tgz")
	l08Pkg(t, pkg, man, map[string]string{"/usr/bin/sclite-agent": "a"})
	done := make(chan error, 1)
	go func() { _, err := verifyUpdate(pkg); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("accepted")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the base check hangs on a package whose base is a FIFO")
	}
}

// A path both installed and removed: the update installs it, then deletes it, and reports success.
func TestL08RefusesFileAlsoRemoved(t *testing.T) {
	root, base := setupRoot(t)
	pkg := filepath.Join(t.TempDir(), "p.tgz")
	l08Pkg(t, pkg, l08Man(base, []map[string]any{l08File("/usr/bin/sclite-agent", "agent 9")}, []string{"/usr/bin/sclite-agent"}),
		map[string]string{"/usr/bin/sclite-agent": "agent 9"})
	if _, err := installUpdate(pkg); err == nil {
		if _, serr := os.Stat(filepath.Join(root, "usr/bin/sclite-agent")); serr != nil {
			t.Fatal("a package that writes and removes the panel was installed: the panel is gone")
		}
	}
}

// A second upload that fails (too big, cut off) truncates and removes the staged file but leaves the earlier
// package shown as "Ready to install"; Install then fails on a file that isn't there.
func TestL08FailedUploadClearsStaged(t *testing.T) {
	_, base := setupRoot(t)
	dir := t.TempDir()
	oldStage, oldMax := updateStage, updateMaxBytes
	defer func() { updateStage, updateMaxBytes = oldStage, oldMax; updStaged = nil }()
	updateStage = filepath.Join(dir, "stage.tgz")
	pkg := filepath.Join(dir, "p.tgz")
	l08Pkg(t, pkg, l08Man(base, []map[string]any{l08File("/usr/bin/sclite-agent", "agent 9")}, nil), map[string]string{"/usr/bin/sclite-agent": "agent 9"})
	pb, _ := os.ReadFile(pkg)
	up := func(b []byte) map[string]any {
		rec := httptest.NewRecorder()
		handleUpdateUpload(rec, httptest.NewRequest("POST", "/api/update/upload", bytes.NewReader(b)))
		var v map[string]any
		json.Unmarshal(rec.Body.Bytes(), &v)
		return v
	}
	if v := up(pb); v["staged"] == nil {
		t.Fatalf("first upload: %v", v)
	}
	updateMaxBytes = 100
	if v := up(bytes.Repeat([]byte("x"), 1000)); v["ok"] != false {
		t.Fatalf("too big accepted: %v", v)
	}
	if v := updView(); v["staged"] != nil {
		if _, err := os.Stat(updateStage); err != nil {
			t.Fatalf("after a failed upload the panel still offers %v for install, but its file is gone", v["staged"])
		}
	}
}

// A download that fails its check leaves up to 48 MB in /tmp (RAM) with nothing staged.
func TestL08FailedFetchLeavesNoFileInRAM(t *testing.T) {
	setupRoot(t)
	dir := t.TempDir()
	var srvURL string
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/me/fork/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"tag_name": "v9.9.9",
			"assets": []map[string]any{{"name": "sclite-update-v9.9.9.tar.gz", "browser_download_url": srvURL + "/upd"}}})
	})
	mux.HandleFunc("/upd", func(w http.ResponseWriter, r *http.Request) { w.Write(bytes.Repeat([]byte("junk"), 4096)) })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	srvURL = srv.URL
	oldAPI, oldStage := updateAPI, updateStage
	defer func() { updateAPI, updateStage = oldAPI, oldStage; updStaged = nil }()
	updateAPI, updateStage = srv.URL, filepath.Join(dir, "stage.tgz")
	os.WriteFile(updRepoPath(), []byte("me/fork\n"), 0o644)
	rec := httptest.NewRecorder()
	handleUpdate(rec, httptest.NewRequest("POST", "/api/update", strings.NewReader(`{"fetch":true}`)))
	if !strings.Contains(rec.Body.String(), `"ok":false`) {
		t.Fatalf("junk download accepted: %s", rec.Body.String())
	}
	if _, err := os.Stat(updateStage); err == nil {
		t.Fatal("a download that failed its check stays in /tmp (RAM)")
	}
}

// On the miner the running miner (/usr/libexec/sclite/hook/intminer, 2.2 MB, in every package) keeps its old
// blocks after it is replaced, but updRoomNeeded counts them as freed. Here: a 4 MB partition, a 1.5 MB
// "running" file (held open) that the package rewrites unchanged, and 1.5 MB of new files: the room check
// passes, the install then runs out of space. Files already as packaged needn't be written at all.
func TestL08RoomWithRunningUnchangedBinary(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root for a small tmpfs")
	}
	root, _ := setupRoot(t)
	mnt := filepath.Join(t.TempDir(), "sys")
	os.MkdirAll(mnt, 0o755)
	if err := syscall.Mount("tmpfs", mnt, "tmpfs", 0, "size=4m"); err != nil {
		t.Skip(err)
	}
	defer syscall.Unmount(mnt, syscall.MNT_DETACH)
	updateRoot = mnt
	_ = root
	big := strings.Repeat("M", 1536<<10)
	os.MkdirAll(filepath.Join(mnt, "usr/libexec/sclite/hook"), 0o755)
	os.WriteFile(filepath.Join(mnt, "usr/libexec/sclite/intminer"), []byte("STOCK MINER"), 0o755)
	os.WriteFile(filepath.Join(mnt, "usr/libexec/sclite/hook/intminer"), []byte(big), 0o755)
	running, _ := os.Open(filepath.Join(mnt, "usr/libexec/sclite/hook/intminer")) // the miner runs from it
	defer running.Close()
	s := sha256.Sum256([]byte("STOCK MINER"))
	newf := strings.Repeat("N", 1536<<10)
	pkg := filepath.Join(t.TempDir(), "p.tgz")
	l08Pkg(t, pkg, l08Man(hex.EncodeToString(s[:]), []map[string]any{
		l08File("/usr/libexec/sclite/hook/intminer", big), l08File("/usr/libexec/sclite/zz-new", newf)}, nil),
		map[string]string{"/usr/libexec/sclite/hook/intminer": big, "/usr/libexec/sclite/zz-new": newf})
	m, err := verifyUpdate(pkg)
	if err != nil {
		t.Fatal(err)
	}
	if need, free := updRoomNeeded(m), statfsFree(mnt); free < need {
		t.Skipf("room check refuses (%d < %d): consistent", free, need)
	}
	if _, err := installUpdate(pkg); err != nil {
		t.Fatalf("the room check passed, then the install failed: %v", err)
	}
}
