package main

// L30-boot: start-up with a login file that's there but can't be read, and a password change that can't be saved.

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func l30Owner(t *testing.T, d string) {
	*authFile = filepath.Join(d, "auth.json")
	authMu.Lock()
	auth = authStore{Username: "owner", Secret: "c2VjcmV0c2VjcmV0c2VjcmV0c2VjcmV0", Acknowledged: "2026-01-01"}
	setPassword(&auth, "owner-pass-1")
	authMu.Unlock()
	if err := saveAuth(); err != nil {
		t.Fatal(err)
	}
	if err := saveAuth(); err != nil { // auth.json.bak: the same login
		t.Fatal(err)
	}
}

// auth.json is there but can't be read (a read error; here: a directory in its place). The backup holds the
// owner's login: it's used, not the factory admin/admin anyone on the network could sign in with first.
func TestL30AuthUnreadableUsesBackup(t *testing.T) {
	d := t.TempDir()
	l30Owner(t, d)
	os.Remove(*authFile)
	os.MkdirAll(filepath.Join(*authFile, "x"), 0o755)
	loadAuth()
	authMu.RLock()
	u, mc := auth.Username, auth.MustChange
	authMu.RUnlock()
	if u != "owner" || mc || !checkPassword("owner-pass-1") {
		t.Fatalf("unreadable auth.json with a good backup: user=%q must_change=%v owner's password works=%v (back to admin/admin)",
			u, mc, checkPassword("owner-pass-1"))
	}
}

// A password change that can't be saved says "could not save": the old password and this session must still
// work (before: the new password was in force until the next start, the session signed out, the old one refused).
func TestL30PasswdSaveFailKeepsOld(t *testing.T) {
	d := t.TempDir()
	l30Owner(t, d)
	cookie := issueCookie("owner")
	// the next save can't land: auth.json.tmp is a directory
	os.MkdirAll(filepath.Join(*authFile+".tmp", "x"), 0o755)
	r := httptest.NewRequest("POST", "/api/passwd", strings.NewReader(`{"Current":"owner-pass-1","New":"owner-pass-2"}`))
	w := httptest.NewRecorder()
	handlePasswd(w, r)
	if w.Code != 500 || !strings.Contains(w.Body.String(), "could not save") {
		t.Fatalf("save should fail: %d %s", w.Code, w.Body.String())
	}
	if !checkPassword("owner-pass-1") || checkPassword("owner-pass-2") {
		t.Fatalf("after 'could not save': old works=%v, new works=%v", checkPassword("owner-pass-1"), checkPassword("owner-pass-2"))
	}
	if !validCookie(cookie) {
		t.Fatal("after 'could not save' the owner's session was signed out")
	}
}
