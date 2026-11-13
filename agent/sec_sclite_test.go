//go:build !scbox2

package main

// The SC Lite build keeps its behaviour (access.go): IPv4 isn't filtered by address, a request without Origin is
// accepted, no access switches. Sign-out does end the session there too (L6, model-independent).

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSecSCLiteUnchanged(t *testing.T) {
	if secHardened {
		t.Fatal("the SC Lite build isn't hardened")
	}
	accessLoad(netConfig{AllowOutside: false, HTTPSOnly: true})
	if !peerAllowed(net.ParseIP("8.8.8.8")) || peerAllowed(net.ParseIP("2001:4860::8888")) || accHTTPSOnly.Load() {
		t.Fatal("SC Lite: IPv4 open, IPv6 LAN only, no HTTPS only")
	}
	r := httptest.NewRequest("POST", "/api/x", nil)
	r.Host = "192.168.1.10"
	if !changeFromPanel(r) {
		t.Fatal("SC Lite: a request without Origin is accepted as before")
	}
	r.Header.Set("Origin", "http://evil.example")
	if changeFromPanel(r) {
		t.Fatal("another origin: refused")
	}
	if accessView() != nil {
		t.Fatal("no access switches on the SC Lite")
	}
	rec := httptest.NewRecorder()
	v := true
	handleAccess(rec, r, &v, nil, nil, nil, "")
	if !strings.Contains(rec.Body.String(), "not available") {
		t.Fatalf("handleAccess: %s", rec.Body.String())
	}
}

func TestSecSCLiteSignOutEndsSession(t *testing.T) {
	sid := redteamSetup(t)
	req := httptest.NewRequest("POST", "/api/logout", strings.NewReader("{}"))
	req.AddCookie(&http.Cookie{Name: "sid", Value: sid})
	handleLogout(httptest.NewRecorder(), req)
	if validCookie(sid) {
		t.Fatal("a signed-out cookie must no longer sign in")
	}
}
