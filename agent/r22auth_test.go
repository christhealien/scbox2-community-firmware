package main

// v1.5.22: the password is stored as PBKDF2-HMAC-BLAKE2b-512. These tests check the hash against values
// worked out independently (Python: a hand-written PBKDF2 over hmac+hashlib.blake2b, which agrees with
// OpenSSL's pbkdf2_hmac('blake2b512')), the move from the old SHA-256 hash at sign-in, the old format while
// an update waits to be confirmed, the rollback by hand, and what an older panel makes of the new file.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestR22PBKDF2Blake2bVectors(t *testing.T) {
	for _, v := range []struct {
		pw, salt string
		iter, n  int
		want     string
	}{
		{"password", "salt", 1, 64, "684e7cc1dd9b241d2c977f38a896645da49b85eb13cf8f5c021efc167aad799343c06f50e2959de06a0bca80a154457d8e92e70ebdcdb3722dcf9badd6ff1dfb"},
		{"password", "salt", 2, 64, "40b77cc2ee4b4c44eeb5babc299be14af5670e39ea3ce14c0fe70e6c99369886ab4d693bad8bd811ed64c5cf65a4cc5260993e17bbf2423c77164752fcbf5a60"},
		{"correct horse battery", "0123456789abcdef", 1000, 64, "2701ea75893bc71c16d1ee96fdd7f509250298682ba6d694a68c037821fb77f925cbcac8efcad673bdc1769eeabd7b2ae183b9047f98648b49d930d34ba09224"},
		{"pass\x00word", "sa\x00lt", 4096, 80, "f6c776d6265b53e32fa746713464fe59674db162ac51908caa248fb3f71ea362c183c43fcb8500eeb009c105bde85453bd4c42d9c0e9f5a131f96ab7c0fab6b15c3f5536666644b985245d99910b327d"},
	} {
		got := hex.EncodeToString(pbkdf2With(blake2b512, []byte(v.pw), []byte(v.salt), v.iter, v.n))
		if got != v.want {
			t.Errorf("%q/%q/%d: got %s, want %s", v.pw, v.salt, v.iter, got, v.want)
		}
	}
	// the SHA-256 one is unchanged (RFC 7914 / well-known vector for PBKDF2-HMAC-SHA256)
	if got := hex.EncodeToString(pbkdf2([]byte("passwd"), []byte("salt"), 1, 64)); got != "55ac046e56e3089fec1691c22544b605f94185216dde0465e68b9d57c20dacbc49ca9cccf179b645991664b39d77ef317c71b845b1e30bd509112041d3a19783" {
		t.Errorf("sha256 vector: %s", got)
	}
}

func r22Setup(t *testing.T, pending bool) {
	t.Helper()
	oldAuth, oldFile := auth, *authFile
	*authFile = filepath.Join(t.TempDir(), "auth.json")
	os.MkdirAll(updDir(), 0o755)
	if pending {
		os.WriteFile(filepath.Join(updDir(), "pending"), []byte("{}"), 0o644)
	}
	t.Cleanup(func() { authUpgradeWG.Wait(); auth, *authFile = oldAuth, oldFile; authKeepLegacy.Store(false) })
}

// an owner coming from v1.5.21: their SHA-256 hash moves to BLAKE2b at the first sign-in
func TestR22UpgradeAtSignIn(t *testing.T) {
	r22Setup(t, false)
	var a authStore
	setPasswordSHA256(&a, "hunter2hunter2")
	a.Username, a.Secret = "owner", base64.StdEncoding.EncodeToString(randBytes(32))
	authMu.Lock()
	auth = a
	authMu.Unlock()
	saveAuth()
	if checkPassword("wrong-password") {
		t.Fatal("a wrong password matched")
	}
	authUpgradeWG.Wait()
	if auth.B2Hash != "" {
		t.Fatal("a wrong password upgraded the hash")
	}
	if !checkPassword("hunter2hunter2") {
		t.Fatal("the old SHA-256 password didn't match")
	}
	authUpgradeWG.Wait()
	b, _ := os.ReadFile(*authFile)
	var f authStore
	json.Unmarshal(b, &f)
	if f.B2Hash == "" || f.B2Iter != b2Iter || f.Iter != 0 {
		t.Fatalf("file after the sign-in: %+v", f)
	}
	if f.Secret != a.Secret {
		t.Error("the upgrade signed everyone out")
	}
	if !checkPassword("hunter2hunter2") || checkPassword("hunter2hunter3") {
		t.Fatal("the BLAKE2b hash doesn't check right")
	}
	if authAlgo(&f) != "PBKDF2-HMAC-BLAKE2b-512" {
		t.Error(authAlgo(&f))
	}
}

// while an update is unconfirmed, the boot script may put the older panel back: nothing moves to BLAKE2b
func TestR22PendingUpdateKeepsSHA256(t *testing.T) {
	r22Setup(t, true)
	var a authStore
	setPassword(&a, "hunter2hunter2")
	if a.B2Hash != "" || a.Hash == "" {
		t.Fatalf("a password set during a pending update: %+v", a)
	}
	a.Username, a.Secret = "owner", base64.StdEncoding.EncodeToString(randBytes(32))
	authMu.Lock()
	auth = a
	authMu.Unlock()
	if !checkPassword("hunter2hunter2") {
		t.Fatal("no match")
	}
	authUpgradeWG.Wait()
	if auth.B2Hash != "" || auth.Hash != a.Hash {
		t.Fatal("upgraded during a pending update")
	}
}

// what a panel up to v1.5.21 does with the new file: its authUsable (copied) calls it damaged, so it starts
// over at admin/admin with a forced change instead of matching nothing forever
func TestR22OlderPanelSeesDamagedNotLocked(t *testing.T) {
	r22Setup(t, false)
	var a authStore
	setPassword(&a, "hunter2hunter2")
	a.Username, a.Secret = "owner", base64.StdEncoding.EncodeToString(randBytes(32))
	b, _ := json.Marshal(a)
	var old struct {
		Username, Salt, Hash string
		Iter                 int
	}
	json.Unmarshal(b, &old)
	salt, e1 := base64.StdEncoding.DecodeString(old.Salt)
	hash, e2 := base64.StdEncoding.DecodeString(old.Hash)
	oldUsable := !(e1 != nil || e2 != nil || len(salt) < 8 || len(hash) < 16 || old.Iter < 1000 || old.Iter > 10_000_000 || strings.TrimSpace(old.Username) == "")
	if oldUsable {
		t.Fatal("an older panel would take the file as usable and match nothing: everyone locked out")
	}
	if !authUsable(&a) {
		t.Fatal("this panel can't use its own file")
	}
}

func TestR22UsableChecks(t *testing.T) {
	var a authStore
	setPassword(&a, "hunter2hunter2")
	a.Username = "owner"
	for name, f := range map[string]func(x *authStore){
		"short b2 hash": func(x *authStore) { x.B2Hash = base64.StdEncoding.EncodeToString([]byte("short")) },
		"bad b2 salt":   func(x *authStore) { x.B2Salt = "!!" },
		"b2 iter 0":     func(x *authStore) { x.B2Iter = 0 },
		"b2 iter huge":  func(x *authStore) { x.B2Iter = 20_000_000 },
		"no user":       func(x *authStore) { x.Username = " " },
	} {
		x := a
		f(&x)
		if authUsable(&x) {
			t.Errorf("%s: usable", name)
		}
	}
}

// the rollback by hand asks for the password and saves it the way the older panel reads it
func TestR22RollbackSavesSHA256(t *testing.T) {
	r22Setup(t, false)
	var a authStore
	setPassword(&a, "hunter2hunter2")
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
	defer func() { updateReboot = oldReb; updSetBusy(""); clearReboot() }()
	post := func(body string) map[string]any {
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
	if v := updView(); v["rollback_needs_password"] != true {
		t.Fatal("the page isn't told to ask for the password")
	}
	if j := post(`{"rollback":true}`); j["ok"] == true {
		t.Fatal("rolled back without the password")
	}
	if j := post(`{"rollback":true,"password":"nope-nope"}`); j["ok"] == true {
		t.Fatal("rolled back with a wrong password")
	}
	if _, err := os.Stat(filepath.Join(updDir(), "rollback")); err == nil {
		t.Fatal("rollback marker written after a refusal")
	}
	if j := post(`{"rollback":true,"password":"hunter2hunter2"}`); j["ok"] != true {
		t.Fatalf("refused: %v", j)
	}
	b, _ := os.ReadFile(*authFile)
	var f authStore
	json.Unmarshal(b, &f)
	if f.B2Hash != "" || f.Hash == "" || f.Iter != pbkdfIter {
		t.Fatalf("file before the rollback: %+v", f)
	}
	salt, _ := base64.StdEncoding.DecodeString(f.Salt)
	want, _ := base64.StdEncoding.DecodeString(f.Hash)
	if !bytes.Equal(pbkdf2([]byte("hunter2hunter2"), salt, f.Iter, len(want)), want) {
		t.Fatal("the older panel couldn't check the saved password")
	}
	if _, err := os.Stat(filepath.Join(updDir(), "rollback")); err != nil {
		t.Fatal("no rollback marker")
	}
}
