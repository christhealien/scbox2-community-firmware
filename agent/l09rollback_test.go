package main

// L09: what an older panel finds after a rollback by hand. A rollback marker written while the password is
// still in the SHA-256 format (or written by the rollback with the password) must not be followed by the
// sign-in upgrade to BLAKE2b: the boot script then puts back a panel that can't read the file, which starts
// over at admin/admin.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func l09Setup(t *testing.T) func(body string) map[string]any {
	t.Helper()
	r22Setup(t, false)
	var a authStore
	setPasswordSHA256(&a, "hunter2hunter2") // an owner from v1.5.21, update already confirmed
	a.Username, a.Secret = "owner", base64.StdEncoding.EncodeToString(randBytes(32))
	authMu.Lock()
	auth = a
	authMu.Unlock()
	saveAuth()
	var tgz bytes.Buffer
	zw := gzip.NewWriter(&tgz)
	tw := tar.NewWriter(zw)
	tw.WriteHeader(&tar.Header{Name: "usr/bin/x", Mode: 0o755, Size: 1})
	tw.Write([]byte("x"))
	tw.Close()
	zw.Close()
	os.WriteFile(filepath.Join(updDir(), "backup.tgz"), tgz.Bytes(), 0o644)
	oldReb := updateReboot
	updateReboot = func() {}
	t.Cleanup(func() { updateReboot = oldReb; updSetBusy(""); authKeepLegacy.Store(false); clearReboot() })
	return func(body string) map[string]any {
		updSetBusy("")
		r := httptest.NewRequest(http.MethodPost, "/api/update", bytes.NewBufferString(body))
		r.Header.Set("Content-Type", "application/json")
		r.RemoteAddr = "192.168.1.50:1234"
		w := httptest.NewRecorder()
		handleUpdate(w, r)
		var j map[string]any
		json.Unmarshal(w.Body.Bytes(), &j)
		return j
	}
}

func l09FileIsLegacy(t *testing.T, when string) {
	t.Helper()
	b, _ := os.ReadFile(*authFile)
	var f authStore
	json.Unmarshal(b, &f)
	if f.B2Hash != "" || f.Hash == "" {
		t.Fatalf("%s: auth.json is in the BLAKE2b format before an older panel comes back: it starts over at admin/admin (%+v)", when, f)
	}
}

// a sign-in after "Roll back" (another tab, SSH, the page itself; the panel runs until the reboot, or for a
// whole start when the boot script's tar fails part-way and the marker is kept)
func TestL09SignInAfterRollbackMarkerKeepsSHA256(t *testing.T) {
	post := l09Setup(t)
	if j := post(`{"rollback":true}`); j["ok"] != true {
		t.Fatalf("refused: %v", j)
	}
	if _, err := os.Stat(filepath.Join(updDir(), "rollback")); err != nil {
		t.Fatal("no rollback marker")
	}
	if !checkPassword("hunter2hunter2") {
		t.Fatal("no match")
	}
	authUpgradeWG.Wait()
	l09FileIsLegacy(t, "a sign-in after the rollback was marked")
}

// the test harness's own flow (tools/test-update.sh): sign in, then roll back at once: the upgrade the
// sign-in started finishes after the handler decided no password was needed
func TestL09RollbackRightAfterSignIn(t *testing.T) {
	post := l09Setup(t)
	if !checkPassword("hunter2hunter2") { // starts the upgrade in the background
		t.Fatal("no match")
	}
	j := post(`{"rollback":true}`)
	authUpgradeWG.Wait()
	if j["ok"] == true {
		l09FileIsLegacy(t, "a rollback right after a sign-in")
	}
}

// installing an older package (v1.5.21 or before) over a panel whose password is already BLAKE2b: the older
// panel that comes up finds no password it can read and starts over at admin/admin
func TestL09InstallOlderPackageKeepsLogin(t *testing.T) {
	root, base := setupRoot(t)
	_ = root
	oldAuth := auth
	t.Cleanup(func() { authUpgradeWG.Wait(); auth = oldAuth; authKeepLegacy.Store(false) })
	var a authStore
	setPassword(&a, "hunter2hunter2")
	a.Username, a.Secret = "owner", base64.StdEncoding.EncodeToString(randBytes(32))
	authMu.Lock()
	auth = a
	authMu.Unlock()
	saveAuth()
	pkg := filepath.Join(t.TempDir(), "u.tgz")
	makePkg(t, pkg, "1.5.21", base, []pkgFile{
		{path: brandRelease, data: brandAgent + " firmware build\nversion: 1.5.21\n", mode: 0o644},
	}, nil, nil)
	oldStage, oldReb := updateStage, updateReboot
	updateStage, updateReboot = pkg, func() {}
	t.Cleanup(func() {
		updateStage, updateReboot = oldStage, oldReb
		updMu.Lock()
		updStaged = nil
		updMu.Unlock()
		updSetBusy("")
		authKeepLegacy.Store(false)
		clearReboot()
	})
	post := func(body string) map[string]any {
		m, err := verifyUpdate(pkg)
		if err != nil {
			t.Fatal(err)
		}
		updMu.Lock()
		updStaged = m
		updMu.Unlock()
		updSetBusy("")
		r := httptest.NewRequest(http.MethodPost, "/api/update", bytes.NewBufferString(body))
		r.RemoteAddr = "192.168.1.50:1234"
		w := httptest.NewRecorder()
		handleUpdate(w, r)
		var j map[string]any
		json.Unmarshal(w.Body.Bytes(), &j)
		return j
	}
	j := post(`{"install":true}`)
	if j["ok"] == true {
		l09FileIsLegacy(t, "an older package installed")
	}
	os.Remove(filepath.Join(updDir(), "pending"))
	if j := post(`{"install":true,"password":"hunter2hunter2"}`); j["ok"] != true {
		t.Fatalf("refused with the password: %v", j)
	}
	l09FileIsLegacy(t, "an older package installed with the password")
}
