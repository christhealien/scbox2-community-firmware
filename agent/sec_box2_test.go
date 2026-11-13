//go:build scbox2

package main

// The SC Box II security audit's fixes (access.go): M1 the password again for an install and a new update source,
// M2 local network only (panel and SSH), L5 the clock set by hand, L6 sign-out ends the session, L7 a change must
// name the panel's page, L8 ports 80 and 443 switch off.

import (
	"bufio"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// secClearFails: wrong passwords counted against the test's address are forgotten (other tests use it too).
func secClearFails(t *testing.T) {
	t.Cleanup(func() {
		sshFailMu.Lock()
		for k := range sshFailBy {
			delete(sshFailBy, k)
		}
		sshFailMu.Unlock()
	})
}

func secNetCfg(t *testing.T) {
	t.Helper()
	old := *netPath
	*netPath = filepath.Join(t.TempDir(), "net.json")
	t.Cleanup(func() { *netPath = old; accessLoad(netConfig{}) })
}

// ---------------------------------------------------------------- M1

func TestSecM1RepoNeedsPassword(t *testing.T) {
	box2On(t)
	secClearFails(t)
	if m := post(t, "/api/update", `{"repo":"someone/evil-fork"}`); m["ok"] != false || !strings.Contains(m["err"].(string), "password") {
		t.Fatalf("a new update source without the password must be refused: %v", m)
	}
	if updateRepo() != defaultUpdateRepo {
		t.Fatalf("refused, yet the source changed to %s", updateRepo())
	}
	if m := post(t, "/api/update", `{"repo":"someone/evil-fork","password":"wrong-one"}`); m["ok"] != false || !strings.Contains(m["err"].(string), "isn't the panel password") {
		t.Fatalf("a wrong password must be refused: %v", m)
	}
	if m := post(t, "/api/update", `{"repo":"owner/fork","password":"`+c9TestPw+`"}`); m["ok"] != true || updateRepo() != "owner/fork" {
		t.Fatalf("with the password the source changes: %v (now %s)", m, updateRepo())
	}
	// saving the same source again (the Save button) asks nothing
	if m := post(t, "/api/update", `{"repo":"owner/fork"}`); m["ok"] != true {
		t.Fatalf("an unchanged source needs no password: %v", m)
	}
	if v := updView(); v["repo_needs_password"] != true || v["install_needs_password"] != true {
		t.Fatalf("the page must be told to ask: %v", v)
	}
}

func TestSecM1BruteForceLimit(t *testing.T) {
	box2On(t)
	secClearFails(t)
	for i := 0; i < sshFailLimit; i++ {
		post(t, "/api/update", `{"repo":"a/b","password":"guess-`+string(rune('a'+i))+`"}`)
	}
	m := post(t, "/api/update", `{"repo":"a/b","password":"`+c9TestPw+`"}`)
	if m["ok"] != false || !strings.Contains(m["err"].(string), "too many wrong passwords") {
		t.Fatalf("after %d wrong passwords the address is blocked, even with the right one: %v", sshFailLimit, m)
	}
	if updateRepo() != defaultUpdateRepo {
		t.Fatal("a blocked address changed the source")
	}
}

func TestSecM1ImageInstallNeedsPassword(t *testing.T) {
	box2On(t)
	secClearFails(t)
	fakeSystem(t, 6<<20, fakeSquashfs(3<<20, 1), 200000)
	ius := fakeIUS(t, stdItems(fakeSquashfs(2<<20, 9)), true)
	post(t, "/api/update/upload?name="+brand+"-firmware-v0.5.0.ius", string(ius))
	s := sha256.Sum256(ius)
	// the checksum pasted, every check passed, all confirmations: still not without the password
	post(t, "/api/update", `{"sums":"`+hex.EncodeToString(s[:])+`  `+brand+`-firmware-v0.5.0.ius"}`)
	if m := post(t, "/api/update", `{"verify":true}`); m["verified"] != true {
		t.Fatalf("setup: the image must verify: %v", m)
	}
	for _, c := range []struct{ body, want string }{
		{`{"install":true,"ack":true}`, "enter your panel password"},
		{`{"install":true,"ack":true,"password":"not-it"}`, "isn't the panel password"},
	} {
		m := post(t, "/api/update", c.body)
		if m["ok"] != false || !strings.Contains(m["err"].(string), c.want) {
			t.Fatalf("%s: must be refused (%q): %v", c.body, c.want, m)
		}
		if _, err := os.Stat(imgReqPath); err == nil {
			t.Fatalf("%s: refused, yet the install request was written", c.body)
		}
	}
	if m := post(t, "/api/update", `{"install":true,"ack":true,"password":"`+c9TestPw+`"}`); m["ok"] != false || !strings.Contains(m["err"].(string), "handoff (test)") {
		t.Fatalf("with the password the install reaches the hand-off: %v", m)
	}
	updMu.Lock()
	busy := updBusy
	updMu.Unlock()
	if busy != "" {
		t.Fatalf("a refused install must not stay busy: %q", busy)
	}
}

// ---------------------------------------------------------------- M2

func TestSecM2Peers(t *testing.T) {
	secNetCfg(t)
	local := []string{"127.0.0.1", "10.1.2.3", "172.16.0.1", "172.31.255.254", "192.168.1.20", "100.64.0.1", "100.127.255.254",
		"169.254.3.4", "fd00::5", "fe80::1"}
	outside := []string{"8.8.8.8", "172.32.0.1", "100.128.0.1", "1.1.1.1", "2001:4860::8888"}
	for _, on := range []bool{false, true} {
		accessLoad(netConfig{AllowOutside: on})
		for _, a := range local {
			if !peerAllowed(net.ParseIP(a)) {
				t.Errorf("switch %v: %s (local) refused", on, a)
			}
		}
		for _, a := range outside {
			if peerAllowed(net.ParseIP(a)) != on {
				t.Errorf("switch %v: %s (outside) allowed=%v", on, a, !on)
			}
		}
	}
	// an IPv4 LAN on public addresses: the miner's own subnet counts as local (never locked out from the LAN)
	oa := netInterfaceAddrs
	t.Cleanup(func() { netInterfaceAddrs = oa })
	netInterfaceAddrs = func() ([]net.Addr, error) {
		_, n, _ := net.ParseCIDR("203.0.113.7/24")
		n.IP = net.ParseIP("203.0.113.7").To4()
		return []net.Addr{n}, nil
	}
	accessLoad(netConfig{})
	if !peerAllowed(net.ParseIP("203.0.113.50")) || peerAllowed(net.ParseIP("203.0.114.50")) {
		t.Error("the miner's own subnet is local, the next one isn't")
	}
}

// fake connections from a given address
type secLn struct{ ch chan net.Conn }

func (l *secLn) Accept() (net.Conn, error) {
	c, ok := <-l.ch
	if !ok {
		return nil, net.ErrClosed
	}
	return c, nil
}
func (l *secLn) Close() error   { return nil }
func (l *secLn) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4(192, 168, 1, 10), Port: 8080} }

type secConn struct {
	net.Conn
	ra net.Addr
}

func (c secConn) RemoteAddr() net.Addr { return c.ra }

func secDial(l *secLn, from string) net.Conn {
	a, b := net.Pipe()
	l.ch <- secConn{a, &net.TCPAddr{IP: net.ParseIP(from), Port: 40000}}
	return b
}

func TestSecM2PanelRefusesOutside(t *testing.T) {
	secNetCfg(t)
	oa := *authFile
	*authFile = filepath.Join(t.TempDir(), "auth.json") // (the certificate, if none was made yet)
	t.Cleanup(func() { *authFile = oa })
	l := &secLn{ch: make(chan net.Conn)}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "panel") })
	go servePanel(l, h, false)
	t.Cleanup(func() { close(l.ch) })
	get := func(from string) string {
		c := secDial(l, from)
		defer c.Close()
		c.SetDeadline(time.Now().Add(5 * time.Second))
		io.WriteString(c, "GET / HTTP/1.1\r\nHost: 192.168.1.10:8080\r\nConnection: close\r\n\r\n")
		b, _ := io.ReadAll(c)
		return string(b)
	}
	if r := get("8.8.8.8"); !strings.HasPrefix(r, "HTTP/1.1 403") || !strings.Contains(r, "answers only on the local network") ||
		!strings.Contains(r, "Allow access from outside the local network") || strings.Contains(r, "panel\n") {
		t.Fatalf("a public IPv4 peer must get the plain refusal: %q", r)
	}
	if r := get("192.168.1.50"); !strings.Contains(r, "200 OK") || !strings.HasSuffix(r, "panel") {
		t.Fatalf("a LAN peer must reach the panel: %q", r)
	}
	accessLoad(netConfig{AllowOutside: true})
	if r := get("8.8.8.8"); !strings.Contains(r, "200 OK") {
		t.Fatalf("with the switch on, outside peers reach the panel: %q", r)
	}
}

func TestSecM2SSHRefusesOutside(t *testing.T) {
	secNetCfg(t)
	oa := *authFile
	*authFile = filepath.Join(t.TempDir(), "auth.json")
	t.Cleanup(func() { *authFile = oa })
	l := &secLn{ch: make(chan net.Conn)}
	sshMu.Lock()
	ol := sshListener
	sshListener = l
	sshMu.Unlock()
	t.Cleanup(func() { sshMu.Lock(); sshListener = ol; sshMu.Unlock(); close(l.ch) })
	go sshServe(l)
	first := func(from string) string {
		c := secDial(l, from)
		defer c.Close()
		c.SetDeadline(time.Now().Add(5 * time.Second))
		s, _ := bufio.NewReader(c).ReadString('\n')
		return s
	}
	if s := first("8.8.8.8"); !strings.HasPrefix(s, "Refused: SSH") || !strings.Contains(s, "local network") {
		t.Fatalf("SSH from a public IPv4 address must be refused: %q", s)
	}
	if s := first("192.168.1.50"); !strings.HasPrefix(s, "SSH-2.0-") {
		t.Fatalf("SSH from the LAN must get the server's version: %q", s)
	}
	accessLoad(netConfig{AllowOutside: true})
	if s := first("8.8.8.8"); !strings.HasPrefix(s, "SSH-2.0-") {
		t.Fatalf("with the switch on, SSH answers outside peers: %q", s)
	}
}

// ---------------------------------------------------------------- the switches through the API

func secMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/login", handleLogin)
	mux.HandleFunc("/api/logout", handleLogout)
	mux.HandleFunc("/api/time", requireAuth(handleTime))
	mux.HandleFunc("/api/net", requireAuth(handleNet))
	mux.HandleFunc("/api/settings", requireAuth(handleSettings))
	mux.HandleFunc("/api/passwd", requireAuth(postOnly(handlePasswd)))
	return httpsRedirect(securityHeaders(mux))
}

var secPage = map[string]string{"Content-Type": "application/json", "X-Requested-With": "panel"}

// secReq: as l03Req, but with only the headers given (l03Req adds the page's own)
func secReq(method, path, body, sid string, hdr map[string]string) *httptest.ResponseRecorder {
	var br io.Reader
	if body != "" {
		br = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, br)
	req.RemoteAddr, req.Host = "192.168.1.50:40000", "192.168.1.10"
	if sid != "" {
		req.AddCookie(&http.Cookie{Name: "sid", Value: sid})
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	secMux().ServeHTTP(rr, req)
	return rr
}

func TestSecAccessSwitchesAPI(t *testing.T) {
	sid := redteamSetup(t)
	secNetCfg(t)
	rr := secReq("POST", "/api/net", `{"allow_outside":true,"password":"owner-password-1"}`, sid, secPage)
	var m map[string]any
	json.Unmarshal(rr.Body.Bytes(), &m)
	if m["ok"] != true || !currentNetConfig().AllowOutside || !accOutside.Load() {
		t.Fatalf("allow_outside: %s", rr.Body.String())
	}
	rr = secReq("GET", "/api/settings", "", sid, nil)
	if !strings.Contains(rr.Body.String(), `"allow_outside":true`) {
		t.Fatalf("Settings must show the switch: %s", rr.Body.String())
	}
	secReq("POST", "/api/net", `{"allow_outside":false}`, sid, secPage)
	if currentNetConfig().AllowOutside || accOutside.Load() {
		t.Fatal("switching it off must apply at once")
	}
	// HTTPS only: plain http from the LAN is sent to https on the same port
	tlsDir := t.TempDir()
	oa := *authFile
	*authFile = filepath.Join(tlsDir, "auth.json")
	panelCert()
	*authFile = oa
	secReq("POST", "/api/net", `{"https_only":true}`, sid, secPage)
	if !accHTTPSOnly.Load() {
		t.Fatal("https_only must apply at once")
	}
	req := httptest.NewRequest("GET", "/api/me?x=1", nil)
	req.RemoteAddr, req.Host = "192.168.1.50:40000", "192.168.1.10:8080"
	rec := httptest.NewRecorder()
	secMux().ServeHTTP(rec, req.WithContext(req.Context()))
	if rec.Code != http.StatusTemporaryRedirect || !strings.HasPrefix(rec.Header().Get("Location"), "https://192.168.1.10") ||
		!strings.HasSuffix(rec.Header().Get("Location"), "/api/me?x=1") {
		t.Fatalf("HTTPS only: want a redirect to https, got %d %v", rec.Code, rec.Header())
	}
	req.RemoteAddr = "127.0.0.1:5000" // loopback stays plain
	rec = httptest.NewRecorder()
	secMux().ServeHTTP(rec, req)
	if rec.Code == http.StatusTemporaryRedirect {
		t.Fatal("loopback must not be redirected")
	}
	// switching it off: from the page, which is on https by now (a plain-http request would be redirected)
	off := httptest.NewRequest("POST", "/api/net", strings.NewReader(`{"https_only":false}`))
	off.RemoteAddr, off.Host, off.TLS = "192.168.1.50:40000", "192.168.1.10:8080", &tls.ConnectionState{}
	off.AddCookie(&http.Cookie{Name: "sid", Value: sid})
	off.Header.Set("Origin", "https://192.168.1.10:8080")
	rec = httptest.NewRecorder()
	secMux().ServeHTTP(rec, off)
	if accHTTPSOnly.Load() || rec.Code != http.StatusOK {
		t.Fatalf("https_only off: %d %s", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------- L5

func TestSecL5ManualClock(t *testing.T) {
	sid := redteamSetup(t)
	var set []int64
	oc := clockSet
	clockSet = func(tv *syscall.Timeval) error { set = append(set, tv.Sec); return nil }
	t.Cleanup(func() { clockSet = oc })
	when := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC).Unix()
	body := func(ts int64) string {
		b, _ := json.Marshal(map[string]any{"clock": ts, "manual": true})
		return string(b)
	}
	if rr := secReq("POST", "/api/time", body(when), "", secPage); rr.Code != http.StatusUnauthorized || len(set) != 0 {
		t.Fatalf("without a session the clock must not be set: %d %v", rr.Code, set)
	}
	// the clock already reads a plausible time here: a hand-set is still allowed
	rr := secReq("POST", "/api/time", body(when), sid, secPage)
	var m map[string]any
	json.Unmarshal(rr.Body.Bytes(), &m)
	if m["ok"] != true || len(set) != 1 || set[0] != when || !strings.Contains(m["note"].(string), "clock set") {
		t.Fatalf("a plausible hand-set time must be set: %s %v", rr.Body.String(), set)
	}
	if _, ok := m["plausible"]; !ok {
		t.Fatalf("the API calls it plausible now: %s", rr.Body.String())
	}
	for _, ts := range []int64{time.Date(2023, 12, 31, 0, 0, 0, 0, time.UTC).Unix(), time.Date(2040, 1, 2, 0, 0, 0, 0, time.UTC).Unix()} {
		rr := secReq("POST", "/api/time", body(ts), sid, secPage)
		if !strings.Contains(rr.Body.String(), `"ok":false`) || len(set) != 1 {
			t.Fatalf("%d is outside 2024..2040: must be refused: %s", ts, rr.Body.String())
		}
	}
	// without "manual" the browser's automatic set still only fills an unset clock
	b, _ := json.Marshal(map[string]any{"clock": when})
	if rr := secReq("POST", "/api/time", string(b), sid, secPage); !strings.Contains(rr.Body.String(), "already set") || len(set) != 1 {
		t.Fatalf("the automatic set must leave a set clock alone: %s", rr.Body.String())
	}
}

// ---------------------------------------------------------------- L6

func TestSecL6SignOutEndsSession(t *testing.T) {
	sid := redteamSetup(t)
	other := issueCookie("owner")
	if !validCookie(sid) || !validCookie(other) {
		t.Fatal("setup: cookies must be valid")
	}
	if strings.Count(cookiePayload(sid), "|") != 2 || cookiePayload(sid) == cookiePayload(other) {
		t.Fatalf("each sign-in's cookie must carry its own session id: %q %q", cookiePayload(sid), cookiePayload(other))
	}
	rr := secReq("POST", "/api/logout", "{}", sid, secPage)
	if rr.Code != http.StatusOK {
		t.Fatalf("sign out: %d %s", rr.Code, rr.Body.String())
	}
	if validCookie(sid) {
		t.Fatal("a signed-out cookie (a copy kept by someone) must no longer sign in")
	}
	if rr := secReq("GET", "/api/settings", "", sid, nil); rr.Code != http.StatusUnauthorized {
		t.Fatalf("a signed-out cookie reached the API: %d", rr.Code)
	}
	if !validCookie(other) {
		t.Fatal("another session stays signed in")
	}
	// kept across a panel restart
	revMu.Lock()
	revoked = map[string]int64{}
	revMu.Unlock()
	loadRevoked()
	if validCookie(sid) {
		t.Fatal("after a restart the signed-out cookie must still be refused")
	}
	// a password change: new key, and the list isn't needed any more
	rr = secReq("POST", "/api/passwd", `{"new":"owner-password-2","current":"owner-password-1"}`, other, secPage)
	if rr.Code != http.StatusOK {
		t.Fatalf("password change: %d %s", rr.Code, rr.Body.String())
	}
	if _, err := os.Stat(revokedPath()); err == nil || validCookie(sid) || validCookie(other) {
		t.Fatal("after a password change no old cookie signs in and the list is gone")
	}
	// a sign-out without the page's marks (a link elsewhere) is refused, the session stays
	nc := issueCookie("owner")
	if rr := secReq("POST", "/api/logout", "{}", nc, map[string]string{"Content-Type": "application/json"}); rr.Code != http.StatusForbidden || !validCookie(nc) {
		t.Fatalf("a sign-out from elsewhere: %d", rr.Code)
	}
}

// ---------------------------------------------------------------- L7

func TestSecL7ChangesNeedThePage(t *testing.T) {
	sid := redteamSetup(t)
	secNetCfg(t)
	body := `{"port80":true}`
	for _, c := range []struct {
		name string
		hdr  map[string]string
		want int
	}{
		{"nothing", map[string]string{"Content-Type": "application/json"}, http.StatusForbidden},
		{"text/plain form", map[string]string{"Content-Type": "text/plain"}, http.StatusForbidden},
		{"same Origin", map[string]string{"Content-Type": "application/json", "Origin": "http://192.168.1.10"}, http.StatusOK},
		{"Sec-Fetch-Site", map[string]string{"Content-Type": "application/json", "Sec-Fetch-Site": "same-origin"}, http.StatusOK},
		{"page header", secPage, http.StatusOK},
		{"other Origin + header", map[string]string{"X-Requested-With": "panel", "Origin": "http://evil.example"}, http.StatusForbidden},
		{"same-site + header", map[string]string{"X-Requested-With": "panel", "Sec-Fetch-Site": "same-site"}, http.StatusForbidden},
		{"cross-site", map[string]string{"Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
		{"Origin null", map[string]string{"Origin": "null", "X-Requested-With": "panel"}, http.StatusForbidden},
	} {
		if rr := secReq("POST", "/api/net", body, sid, c.hdr); rr.Code != c.want {
			t.Errorf("%s: HTTP %d, want %d (%s)", c.name, rr.Code, c.want, rr.Body.String())
		}
	}
	// reading needs none of them
	if rr := secReq("GET", "/api/settings", "", sid, nil); rr.Code != http.StatusOK {
		t.Errorf("GET: %d", rr.Code)
	}
	// the sign-in too
	if rr := secReq("POST", "/api/login", `{"username":"owner","password":"owner-password-1"}`, "", map[string]string{"Content-Type": "application/json"}); rr.Code != http.StatusForbidden {
		t.Errorf("a sign-in without the page's marks: %d", rr.Code)
	}
	if rr := secReq("POST", "/api/login", `{"username":"owner","password":"owner-password-1"}`, "", secPage); rr.Code != http.StatusOK {
		t.Errorf("the page's sign-in: %d %s", rr.Code, rr.Body.String())
	}
	// the page itself sends the header with every change
	for _, w := range []string{`headers:{'Content-Type':'application/json','X-Requested-With':'panel'}`,
		`xhr.setRequestHeader('X-Requested-With','panel');`} {
		if !strings.Contains(indexHTML, w) {
			t.Errorf("the page lacks %q", w)
		}
	}
	if n := strings.Count(indexHTML, "method:'POST'"); n != strings.Count(indexHTML, "method:'POST',headers:{'Content-Type':'application/json','X-Requested-With':'panel'}") {
		t.Errorf("every fetch POST of the page must carry the header (%d POSTs)", n)
	}
	if strings.Count(indexHTML, "xhr.open('POST'") != strings.Count(indexHTML, "xhr.setRequestHeader('X-Requested-With','panel')") {
		t.Error("every upload must carry the header")
	}
}

// ---------------------------------------------------------------- L8

func secFreePort(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, p, _ := net.SplitHostPort(l.Addr().String())
	l.Close()
	return p
}

func secUp(addr string) bool {
	c, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
	if err == nil {
		c.Close()
	}
	return err == nil
}

func secWait(t *testing.T, addr string, up bool) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if secUp(addr) == up {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s: listening=%v, want %v", addr, !up, up)
}

func TestSecL8PortSwitches(t *testing.T) {
	sid := redteamSetup(t)
	secNetCfg(t)
	oa := *authFile
	*authFile = filepath.Join(t.TempDir(), "auth.json")
	panelCert()
	*authFile = oa
	ips := lanIPv4s()
	if len(ips) == 0 {
		t.Skip("no LAN address to serve port 80 on")
	}
	o80, o443 := port80Num, port443Addr
	port80Num, port443Addr = secFreePort(t), "127.0.0.1:"+secFreePort(t)
	extraMu.Lock()
	oh, om := extraHandler, extraMain
	extraMu.Unlock()
	t.Cleanup(func() {
		saveNetConfig(netConfig{Port80Off: true, Port443Off: true})
		extraPortsApply()
		extraMu.Lock()
		extraHandler, extraMain = oh, om
		extraMu.Unlock()
		port80Num, port443Addr = o80, o443
	})
	a80 := net.JoinHostPort(ips[0].String(), port80Num)
	extraPortsStart(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}), "8080")
	secWait(t, a80, true)
	secWait(t, port443Addr, true)
	if rr := secReq("POST", "/api/net", `{"port80":false}`, sid, secPage); !strings.Contains(rr.Body.String(), "port 80 off") {
		t.Fatalf("port80 off: %s", rr.Body.String())
	}
	secWait(t, a80, false)
	secWait(t, port443Addr, true)
	secReq("POST", "/api/net", `{"port443":false}`, sid, secPage)
	secWait(t, port443Addr, false)
	v := accessView()
	if v["port80"] != false || v["port443"] != false || v["port80_on"] != false || v["port443_on"] != false {
		t.Fatalf("the view after switching both off: %v", v)
	}
	secReq("POST", "/api/net", `{"port80":true,"port443":true}`, sid, secPage)
	secWait(t, a80, true)
	secWait(t, port443Addr, true)
	if c := currentNetConfig(); c.Port80Off || c.Port443Off {
		t.Fatalf("saved: %+v", c)
	}
}
