package main

import "testing"

// v0.5.6 (the SC5 review): a connection whose address can't be read is refused on the hardened builds (SC Box II),
// allowed on the SC Lite as before.
func TestPeerAllowedNil(t *testing.T) {
	old := secHardened
	t.Cleanup(func() { secHardened = old })
	secHardened = true
	if peerAllowed(nil) {
		t.Fatal("hardened: an unreadable address was allowed")
	}
	secHardened = false
	if !peerAllowed(nil) {
		t.Fatal("SC Lite: an unreadable address was refused")
	}
}
