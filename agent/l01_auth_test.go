package main

// L01-auth (sweep22): the v1.5.22 BLAKE2b password store and the older panels that may come back.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// v1.5.11's checkPassword and pbkdf2, verbatim (sclite-release-v1.5.11 source, agent/main.go:118-164); its
// loadAuth is a bare json.Unmarshal (no check of the file). All releases v1.4.x-v1.5.11 have the same code.
func l01OldPbkdf2(password, salt []byte, iter, keyLen int) []byte {
	prf := hmac.New(sha256.New, password)
	hLen := prf.Size()
	n := (keyLen + hLen - 1) / hLen
	var dk []byte
	var blk [4]byte
	for b := 1; b <= n; b++ {
		prf.Reset()
		prf.Write(salt)
		binary.BigEndian.PutUint32(blk[:], uint32(b))
		prf.Write(blk[:])
		u := prf.Sum(nil)
		t := append([]byte(nil), u...)
		for i := 2; i <= iter; i++ {
			prf.Reset()
			prf.Write(u)
			u = prf.Sum(nil)
			for j := range t {
				t[j] ^= u[j]
			}
		}
		dk = append(dk, t...)
	}
	return dk[:keyLen]
}

func l01OldCheck(file []byte, pw string) bool {
	var auth struct {
		Username, Salt, Hash string
		Iter                 int
	}
	json.Unmarshal(file, &auth)
	salt, _ := base64.StdEncoding.DecodeString(auth.Salt)
	want, _ := base64.StdEncoding.DecodeString(auth.Hash)
	got := l01OldPbkdf2([]byte(pw), salt, auth.Iter, len(want))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// A v1.5.22 file read by a panel up to v1.5.11 (an older image flashed by hand, or its update package
// installed): with no SHA-256 hash every password signs in (panel and SSH). Also checked live: the v1.5.11
// binary against a v1.5.22 auth.json answered {"ok":true} to admin/"totally-wrong".
func TestL01OldPanelTakesAnyPassword(t *testing.T) {
	r22Setup(t, false)
	var a authStore
	setPassword(&a, "hunter2hunter2")
	a.Username, a.Secret = "owner", base64.StdEncoding.EncodeToString(randBytes(32))
	b, _ := json.Marshal(a)
	for _, pw := range []string{"", "wrong", "hunter2hunter2", "anything at all"} {
		if l01OldCheck(b, pw) {
			t.Errorf("a v1.5.11 panel signs in with %q", pw)
		}
	}
	if !authUsable(&a) || a.B2Hash == "" {
		t.Fatal("this panel can't use its own file")
	}
}

// After "Roll back" saved the SHA-256 hash, the panel keeps running ~1 s (and a failed roll back is retried at
// the next start): a sign-in then (panel or SSH) must not move it back to BLAKE2b.
func TestL01NoUpgradeAfterRollbackMarked(t *testing.T) {
	r22Setup(t, false)
	var a authStore
	setPassword(&a, "hunter2hunter2")
	a.Username, a.Secret = "owner", base64.StdEncoding.EncodeToString(randBytes(32))
	authMu.Lock()
	auth = a
	authMu.Unlock()
	saveAuth()
	os.WriteFile(filepath.Join(updDir(), "backup.tgz"), l01Tgz(), 0o644)
	oldReb := updateReboot
	updateReboot = func() {}
	defer func() { updateReboot = oldReb; updSetBusy(""); clearReboot() }()
	if j := l01Post(t, `{"rollback":true,"password":"hunter2hunter2"}`); j["ok"] != true {
		t.Fatalf("refused: %v", j)
	}
	if !checkPassword("hunter2hunter2") { // the next sign-in, before the reboot
		t.Fatal("no match")
	}
	authUpgradeWG.Wait()
	b, _ := os.ReadFile(*authFile)
	var f authStore
	json.Unmarshal(b, &f)
	if f.B2Hash != "" || f.Iter != pbkdfIter {
		t.Fatalf("moved back to BLAKE2b with the roll back marked: the older panel can't read it (%+v)", f)
	}
}

// An upgrade already hashing when the roll back is marked must not land afterwards.
func TestL01UpgradeInFlightWhenRollbackMarked(t *testing.T) {
	r22Setup(t, false)
	var a authStore
	setPasswordSHA256(&a, "hunter2hunter2")
	a.Username, a.Secret = "owner", base64.StdEncoding.EncodeToString(randBytes(32))
	authMu.Lock()
	auth = a
	authMu.Unlock()
	saveAuth()
	os.WriteFile(filepath.Join(updDir(), "rollback"), []byte("1.5.22\n"), 0o644)
	authUpgrade("hunter2hunter2", a.Hash)
	if auth.B2Hash != "" {
		t.Fatal("upgraded with a roll back marked")
	}
}

// Installing an older version's update package (no version check) over a BLAKE2b file: the older panel can't
// read it. It must ask for the password as "Roll back" does.
func TestL01InstallOlderAsksPassword(t *testing.T) {
	r22Setup(t, false)
	af := *authFile
	_, base := setupRoot(t)
	*authFile = af
	os.MkdirAll(updDir(), 0o755)
	var a authStore
	setPassword(&a, "hunter2hunter2")
	a.Username, a.Secret = "owner", base64.StdEncoding.EncodeToString(randBytes(32))
	authMu.Lock()
	auth = a
	authMu.Unlock()
	saveAuth()
	oldReb, oldStage := updateReboot, updateStage
	updateReboot = func() {}
	updateStage = filepath.Join(t.TempDir(), "stage.tgz")
	defer func() { updateReboot, updateStage = oldReb, oldStage; updSetBusy(""); clearReboot() }()
	stage := func() {
		makePkg(t, updateStage, "1.5.21", base, []pkgFile{{path: "/usr/bin/sclite-agent", data: "agent 1.5.21", mode: 0o755}}, nil, nil)
		m, err := verifyUpdate(updateStage)
		if err != nil {
			t.Fatal(err)
		}
		updMu.Lock()
		updStaged = m
		updMu.Unlock()
	}
	stage()
	if j := l01Post(t, `{"install":true}`); j["ok"] == true {
		t.Fatal("v1.5.21 installed over a BLAKE2b password without asking: it would find no password it can read")
	}
	stage()
	if j := l01Post(t, `{"install":true,"password":"hunter2hunter2"}`); j["ok"] != true {
		t.Fatalf("refused with the password: %v", j)
	}
	b, _ := os.ReadFile(*authFile)
	var f authStore
	json.Unmarshal(b, &f)
	if f.B2Hash != "" || f.Iter != pbkdfIter {
		t.Fatalf("not saved for the older version: %+v", f)
	}
}

// The panel's sign-in skipped the password hash for a wrong username: ~one hash time faster, so the username
// (which /api/me hides) can be found by timing. SSH already always hashes.
func TestL01LoginUsernameTiming(t *testing.T) {
	r22Setup(t, false)
	var a authStore
	setPasswordSHA256(&a, "hunter2hunter2")
	salt, _ := base64.StdEncoding.DecodeString(a.Salt)
	a.Iter = 1_500_000 // a slow hash makes the difference plain on a fast PC
	a.Hash = base64.StdEncoding.EncodeToString(pbkdf2([]byte("hunter2hunter2"), salt, a.Iter, 32))
	a.Username, a.Secret = "owner", base64.StdEncoding.EncodeToString(randBytes(32))
	authMu.Lock()
	auth = a
	authMu.Unlock()
	n := 0
	try := func(user string) time.Duration {
		n++
		r := httptest.NewRequest(http.MethodPost, "/api/login", bytes.NewBufferString(`{"username":"`+user+`","password":"wrong-password"}`))
		r.Host = "192.168.1.10"
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Requested-With", "panel") // the page's own request (required on the SC Box II)
		r.RemoteAddr = fmt.Sprintf("10.9.%d.%d:1234", n/200, n%200+1)
		w := httptest.NewRecorder()
		t0 := time.Now()
		handleLogin(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("code %d %s", w.Code, w.Body.String())
		}
		return time.Since(t0)
	}
	hashT := time.Now()
	pbkdf2([]byte("x"), salt, a.Iter, 32)
	h := time.Since(hashT)
	right, wrong := time.Hour, time.Hour // the quickest of three each (a busy PC only adds time)
	for i := 0; i < 3; i++ {
		right, wrong = min(right, try("owner")), min(wrong, try("nobody"))
	}
	if right-wrong > h/2 {
		t.Fatalf("a wrong username answers %v sooner (right name %v, wrong name %v; one hash %v)", right-wrong, right, wrong, h)
	}
}

// Saves at once (a sign-in's background upgrade beside a password change or the disclaimer) shared one temp
// file: a rename failed (reported "could not save"), or an older snapshot landed last.
func TestL01ConcurrentSaves(t *testing.T) {
	r22Setup(t, false)
	var a authStore
	setPasswordSHA256(&a, "hunter2hunter2")
	a.Username, a.Secret = "owner", base64.StdEncoding.EncodeToString(randBytes(32))
	authMu.Lock()
	auth = a
	authMu.Unlock()
	var wg sync.WaitGroup
	var mu sync.Mutex
	var errs []error
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				if err := saveAuth(); err != nil {
					mu.Lock()
					errs = append(errs, err)
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()
	if len(errs) > 0 {
		t.Fatalf("%d of 100 saves failed, e.g. %v", len(errs), errs[0])
	}
}

func l01Tgz() []byte { // a readable backup (one file)
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	tw := tar.NewWriter(zw)
	tw.WriteHeader(&tar.Header{Name: "usr/bin/x", Mode: 0o755, Size: 1})
	tw.Write([]byte("x"))
	tw.Close()
	zw.Close()
	return b.Bytes()
}

func l01Post(t *testing.T, body string) map[string]any {
	t.Helper()
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
