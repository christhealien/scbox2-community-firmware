package main

import (
	"crypto/subtle"
	"encoding/base64"
	"testing"
)

// L35: what a v1.5.11 panel (no file check; copied pbkdf2 + checkPassword + handleLogin's name check) accepts
// after a v1.5.22 panel saved the owner's own login "bob"/"s3cret!": the stored name, with password "admin".
// The release notes said "admin/admin". (A documentation proof: it passes on base too.)
func TestL35OldPanelSignsInWithOwnNameAndAdmin(t *testing.T) {
	old := authLegacyOnly
	authLegacyOnly = func() bool { return false }
	defer func() { authLegacyOnly = old }()
	a := authStore{Username: "bob"}
	setPassword(&a, "s3cret!")
	salt, _ := base64.StdEncoding.DecodeString(a.Salt)
	want, _ := base64.StdEncoding.DecodeString(a.Hash)
	oldCheck := func(user, pw string) bool { // v1.5.11 handleLogin + checkPassword
		got := pbkdf2([]byte(pw), salt, a.Iter, len(want))
		return subtle.ConstantTimeCompare([]byte(user), []byte(a.Username)) == 1 && subtle.ConstantTimeCompare(got, want) == 1
	}
	if oldCheck("admin", "admin") {
		t.Fatal("admin/admin signs in")
	}
	if !oldCheck("bob", "admin") {
		t.Fatal("the owner's name + admin doesn't sign in")
	}
	if oldCheck("bob", "s3cret!") {
		t.Fatal("the real password signs in")
	}
}
