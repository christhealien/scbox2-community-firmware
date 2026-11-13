package main

// The independent security re-review of the SC Box II panel (access.go, netlisten.go): its findings, fixed.
// Built on every model: the hardened builds' tests skip on the SC Lite; the model-independent fixes (revocation
// file, clock jumps, the random pool, localPeer, a garbled sign-in) are tested on both.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

type reAddrConn struct {
	net.Conn
	ra net.Addr
}

func (c *reAddrConn) RemoteAddr() net.Addr { return c.ra }

func reNetCfg(t *testing.T) {
	t.Helper()
	old := *netPath
	*netPath = filepath.Join(t.TempDir(), "net.json")
	t.Cleanup(func() { *netPath = old; accessLoad(netConfig{}) })
}

func reClearFails(t *testing.T) {
	t.Cleanup(func() {
		sshFailMu.Lock()
		for k := range sshFailBy {
			delete(sshFailBy, k)
		}
		sshFailMu.Unlock()
	})
}

// a test auth store (no password: cookies only)
func reAuth(t *testing.T) {
	oa, oauth := *authFile, auth
	*authFile = filepath.Join(t.TempDir(), "auth.json")
	authMu.Lock()
	auth = authStore{Username: "admin", Secret: base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))}
	authMu.Unlock()
	t.Cleanup(func() { *authFile = oa; authMu.Lock(); auth = oauth; authMu.Unlock(); forgetRevoked() })
	forgetRevoked()
}

// 1. a refused outside peer that never sends a newline: at most refuseReadMax bytes are read, quickly
func TestReRefuseOutsideBounded(t *testing.T) {
	if !secHardened {
		t.Skip("SC Lite: refused connections are closed without reading")
	}
	a, b := net.Pipe()
	c := &reAddrConn{a, &net.TCPAddr{IP: net.ParseIP("8.8.8.8"), Port: 1234}}
	var ms0 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&ms0)
	t0 := time.Now()
	refuseOutside(c)
	var reply bytes.Buffer
	done := make(chan struct{})
	go func() { io.Copy(&reply, b); close(done) }()
	chunk := bytes.Repeat([]byte{'A'}, 1<<20)
	sent := 0
	b.SetWriteDeadline(time.Now().Add(6 * time.Second))
	for sent < 200<<20 {
		n, err := b.Write(chunk)
		sent += n
		if err != nil {
			break
		}
	}
	d := time.Since(t0)
	<-done
	b.Close()
	var ms1 runtime.MemStats
	runtime.ReadMemStats(&ms1)
	if sent > refuseReadMax {
		t.Errorf("read %d bytes from a refused peer (cap %d)", sent, refuseReadMax)
	}
	if d > 3*time.Second {
		t.Errorf("the refusal took %v", d)
	}
	if !strings.HasPrefix(reply.String(), "HTTP/1.1 403 Forbidden") {
		t.Errorf("reply: %q", reply.String())
	}
	if ms1.HeapInuse > ms0.HeapInuse+16<<20 {
		t.Errorf("heap grew from %d to %d MiB", ms0.HeapInuse>>20, ms1.HeapInuse>>20)
	}
}

// 2. "Allow access from outside" needs the panel password to switch on; off needs nothing
func TestReAllowOutsideNeedsPassword(t *testing.T) {
	if !secHardened {
		t.Skip()
	}
	redteamSetup(t)
	reNetCfg(t)
	reClearFails(t)
	net1 := func(body string) map[string]any {
		r := httptest.NewRequest("POST", "/api/net", strings.NewReader(body))
		r.RemoteAddr = "192.168.1.77:4000"
		rec := httptest.NewRecorder()
		handleNet(rec, r)
		var m map[string]any
		json.Unmarshal(rec.Body.Bytes(), &m)
		return m
	}
	if m := net1(`{"allow_outside":true}`); m["ok"] != false || !strings.Contains(m["err"].(string), "password") || accOutside.Load() || currentNetConfig().AllowOutside {
		t.Fatalf("on without the password: %v", m)
	}
	if m := net1(`{"allow_outside":true,"password":"wrong-one"}`); m["ok"] != false || !strings.Contains(m["err"].(string), "isn't the panel password") || accOutside.Load() {
		t.Fatalf("on with a wrong password: %v", m)
	}
	if m := net1(`{"allow_outside":true,"password":"owner-password-1"}`); m["ok"] != true || !accOutside.Load() || !currentNetConfig().AllowOutside {
		t.Fatalf("on with the password: %v", m)
	}
	if m := net1(`{"allow_outside":true}`); m["ok"] != true {
		t.Fatalf("already on: nothing to check: %v", m)
	}
	if m := net1(`{"allow_outside":false}`); m["ok"] != true || accOutside.Load() || currentNetConfig().AllowOutside {
		t.Fatalf("off needs no password: %v", m)
	}
	if m := net1(`{"port80":false}`); m["ok"] != true || !currentNetConfig().Port80Off {
		t.Fatalf("the other switches need no password: %v", m)
	}
}

// 2. a settings file never changes the access switches
func TestReImportKeepsAccessSwitches(t *testing.T) {
	reNetCfg(t)
	saveNetConfig(netConfig{Port: 8080, Port443Off: true})
	bundle, _ := json.Marshal(map[string]any{"format": settingsFormat, "version": 1, "model": model.ID,
		"files": map[string]any{"net": map[string]any{"port": 8081, "dns": "1.1.1.1", "allow_outside": true, "https_only": true, "port80_off": true, "port443_off": false}}})
	r := httptest.NewRequest("POST", "/api/settings/import", bytes.NewReader(bundle))
	rec := httptest.NewRecorder()
	handleSettingsImport(rec, r)
	var m map[string]any
	json.Unmarshal(rec.Body.Bytes(), &m)
	if m["ok"] != true {
		t.Fatalf("import: %s", rec.Body.String())
	}
	c := currentNetConfig()
	if c.Port != 8081 || c.DNS != "1.1.1.1" {
		t.Errorf("the rest of the net section is imported: %+v", c)
	}
	if c.AllowOutside || c.HTTPSOnly || c.Port80Off || !c.Port443Off {
		t.Errorf("the access switches must stay as they were: %+v", c)
	}
	if secHardened && !strings.Contains(m["note"].(string), "access switches") {
		t.Errorf("the note must say so: %v", m["note"])
	}
	// a net section that isn't an object: refused before anything is written
	bundle, _ = json.Marshal(map[string]any{"format": settingsFormat, "version": 1, "model": model.ID, "files": map[string]any{"net": []int{1}}})
	rec = httptest.NewRecorder()
	handleSettingsImport(rec, httptest.NewRequest("POST", "/api/settings/import", bytes.NewReader(bundle)))
	if !strings.Contains(rec.Body.String(), `"ok":false`) || currentNetConfig().Port != 8081 {
		t.Errorf("a bad net section: %s", rec.Body.String())
	}
}

// 3. 240 concurrent sign-outs: all of them survive a reload
func TestReRevokeConcurrentPersist(t *testing.T) {
	reAuth(t)
	var cs []string
	for i := 0; i < 240; i++ {
		cs = append(cs, issueCookie("admin"))
	}
	var wg sync.WaitGroup
	for _, c := range cs {
		wg.Add(1)
		go func(c string) { defer wg.Done(); revokeCookie(c) }(c)
	}
	wg.Wait()
	revMu.Lock()
	revoked = map[string]int64{}
	revMu.Unlock()
	loadRevoked()
	lost := 0
	for _, c := range cs {
		if validCookie(c) {
			lost++
		}
	}
	if lost > 0 {
		t.Errorf("%d of 240 revoked sessions valid again after a reload", lost)
	}
}

// 4. the clock set forward and back: a revoked cookie stays revoked; a cookie issued by a clock in 2039 isn't
// valid once the clock is right again; a session lasts sessionTTL of uptime whatever the clock says
func TestReRevokedSurviveClockJump(t *testing.T) {
	reAuth(t)
	onow, oup := cookieNow, uptimePath
	up := filepath.Join(t.TempDir(), "uptime")
	uptimePath = up
	t.Cleanup(func() { cookieNow, uptimePath = onow, oup })
	os.WriteFile(up, []byte("1000.00 900.00\n"), 0o644)
	stolen := issueCookie("admin")
	revokeCookie(stolen)
	if validCookie(stolen) {
		t.Fatal("setup")
	}
	future := time.Date(2039, 6, 1, 0, 0, 0, 0, time.UTC)
	cookieNow = func() time.Time { return future }
	other := issueCookie("admin")
	kept := issueCookie("admin")
	revokeCookie(other) // the 2039 clock: the stolen entry has "expired", but not by uptime
	cookieNow = onow
	revMu.Lock()
	revoked = map[string]int64{}
	revMu.Unlock()
	loadRevoked()
	if validCookie(stolen) {
		t.Error("a revoked cookie signs in again after the clock went forward and back")
	}
	if validCookie(kept) {
		t.Error("a cookie issued with the clock in 2039 is valid with the clock back in the present")
	}
	// a day of uptime later the entry may go, by a plausible clock that hasn't gone back
	os.WriteFile(up, []byte("90000.00 900.00\n"), 0o644)
	now := time.Now()
	if !revPurgeable(now.Unix()-1, revInfo{now.Unix() - 10, 1000}, now, 90000) {
		t.Error("expired, a day of uptime later: may be dropped")
	}
	if revPurgeable(now.Unix()-1, revInfo{now.Unix() + 10, 1000}, now, 90000) {
		t.Error("the clock went back since the entry was added: kept")
	}
	if revPurgeable(now.Unix()-1, revInfo{now.Unix() - 10, 89000}, now, 90000) {
		t.Error("less than a day of uptime: kept")
	}
	if revPurgeable(future.Unix()-1, revInfo{now.Unix() - 10, 1000}, future.AddDate(1, 0, 0), 90000) {
		t.Error("an implausible clock (2040): kept")
	}
	// uptime bounds this panel's own sessions, whatever the clock says
	os.WriteFile(up, []byte("2000.00 900.00\n"), 0o644)
	s := issueCookie("admin")
	if !validCookie(s) {
		t.Fatal("a fresh session")
	}
	os.WriteFile(up, []byte("88401.00 900.00\n"), 0o644)                      // a day and a bit of uptime later
	cookieNow = func() time.Time { return time.Now().Add(-30 * time.Minute) } // the clock set back a little
	if validCookie(s) {
		t.Error("a session outlived sessionTTL of uptime because the clock was set back")
	}
}

// 5. after the poll, 32 bytes from /dev/random are mixed into /dev/urandom
func TestReEntropyMix(t *testing.T) {
	od, ou, ow := devRandom, devURandom, entropyWait
	t.Cleanup(func() { devRandom, devURandom, entropyWait, entropyOnce = od, ou, ow, sync.Once{} })
	dir := t.TempDir()
	fifo, ur := filepath.Join(dir, "random"), filepath.Join(dir, "urandom")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skip(err)
	}
	os.WriteFile(ur, nil, 0o600)
	w, err := os.OpenFile(fifo, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	seed := []byte("0123456789abcdefghijklmnopqrstuvwxyzEXTRA")
	w.Write(seed)
	devRandom, devURandom, entropyWait, entropyOnce = fifo, ur, 1500*time.Millisecond, sync.Once{}
	t0 := time.Now()
	entropyReady()
	if d := time.Since(t0); d > 500*time.Millisecond {
		t.Errorf("a ready pool: waited %v", d)
	}
	if b, _ := os.ReadFile(ur); string(b) != string(seed[:32]) {
		t.Errorf("urandom got %q, want the first 32 bytes of random", b)
	}
	// only 8 bytes come: waits (bounded), then goes on without writing
	rest := make([]byte, 64)
	f, _ := os.OpenFile(fifo, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	f.Read(rest) // drain what's left
	f.Close()
	os.WriteFile(ur, nil, 0o600)
	w.Write([]byte("12345678"))
	entropyOnce = sync.Once{}
	t0 = time.Now()
	entropyReady()
	if d := time.Since(t0); d < time.Second || d > 5*time.Second {
		t.Errorf("8 of 32 bytes: waited %v, want about the limit", d)
	}
	if b, _ := os.ReadFile(ur); len(b) != 0 {
		t.Errorf("an incomplete read must not be written: %q", b)
	}
	// no /dev/random: nothing at all
	devRandom, entropyOnce = filepath.Join(dir, "none"), sync.Once{}
	entropyReady()
	if b, _ := os.ReadFile(ur); len(b) != 0 {
		t.Errorf("no /dev/random: %q", b)
	}
}

// 6. switching outside access off closes the panel connections and SSH sessions from outside
func TestReOutsideConnsDroppedOnSwitchOff(t *testing.T) {
	if !secHardened {
		t.Skip()
	}
	reNetCfg(t)
	saveNetConfig(netConfig{AllowOutside: true})
	accessLoad(currentNetConfig())
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	pair := func(peer string) (client, server net.Conn) {
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		s, _ := ln.Accept()
		cc, ok := connAllow(&reAddrConn{s, &net.TCPAddr{IP: net.ParseIP(peer), Port: 1}})
		if !ok {
			t.Fatalf("setup: %s refused", peer)
		}
		t.Cleanup(func() { c.Close(); cc.Close() })
		return c, cc
	}
	oc, os1 := pair("8.8.8.8")
	lc, ls := pair("192.168.1.9")
	sa, sb := net.Pipe()
	defer sb.Close()
	sshOut := &reAddrConn{sa, &net.TCPAddr{IP: net.ParseIP("1.2.3.4"), Port: 2}}
	sshAdmit(sshOut, "re-test")
	t.Cleanup(func() { sshForget(sshOut); sshPreAuthDone("re-test") })
	_ = oc
	r := httptest.NewRequest("POST", "/api/net", nil)
	r.RemoteAddr = "192.168.1.9:5000"
	rec := httptest.NewRecorder()
	f := false
	handleAccess(rec, r, &f, nil, nil, nil, "")
	if !strings.Contains(rec.Body.String(), `"ok":true`) || accOutside.Load() {
		t.Fatalf("switch off: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "2 connection(s) from outside closed") {
		t.Errorf("note: %s", rec.Body.String())
	}
	os1.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := os1.Read(make([]byte, 1)); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Errorf("the outside panel connection must be closed: %v", err)
	}
	sb.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := sb.Read(make([]byte, 1)); err != io.EOF {
		t.Errorf("the outside SSH session must be closed: %v", err)
	}
	lc.Write([]byte("x"))
	ls.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := io.ReadFull(ls, make([]byte, 1)); err != nil {
		t.Errorf("a local connection stays: %v", err)
	}
}

// 7a. ::a.b.c.d (IPv4-compatible) shares ::1's /64 but isn't local
func TestReLocalPeerEdges(t *testing.T) {
	old := netInterfaceAddrs
	t.Cleanup(func() { netInterfaceAddrs = old })
	netInterfaceAddrs = func() ([]net.Addr, error) {
		mk := func(s string) net.Addr { ip, n, _ := net.ParseCIDR(s); n.IP = ip; return n }
		return []net.Addr{mk("127.0.0.1/8"), mk("::1/128"), mk("203.0.113.5/24"), mk("2001:db8:1:2::5/64"), mk("fe80::1/64")}, nil
	}
	for _, c := range []struct {
		ip   string
		want bool
	}{
		{"8.8.8.8", false}, {"::ffff:8.8.8.8", false}, {"::ffff:192.168.1.9", true}, {"203.0.113.77", true},
		{"203.0.114.1", false}, {"0.0.0.0", false}, {"255.255.255.255", false}, {"100.64.0.1", true}, {"100.128.0.1", false},
		{"169.254.3.3", true}, {"fe80::99", true}, {"fd00::1", true}, {"2001:db8:1:2::99", true}, {"2001:db8:1:3::1", false},
		{"::1", true}, {"::8.8.8.8", false}, {"64:ff9b::808:808", false}, {"2002:808:808::1", false},
	} {
		if got := localPeer(net.ParseIP(c.ip)); got != c.want {
			t.Errorf("localPeer(%s) = %v, want %v", c.ip, got, c.want)
		}
	}
}

// 7b. the HTTPS redirect: a link-local address's zone is escaped (%25)
func TestReHTTPSRedirectTargets(t *testing.T) {
	if !secHardened {
		t.Skip()
	}
	oa := *authFile
	*authFile = filepath.Join(t.TempDir(), "auth.json")
	t.Cleanup(func() { *authFile = oa })
	if panelCert() == nil {
		t.Skip("no cert")
	}
	accHTTPSOnly.Store(true)
	t.Cleanup(func() { accHTTPSOnly.Store(false) })
	h := httpsRedirect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("served")) }))
	for _, c := range []struct{ host, remote, local, want string }{
		{"192.168.1.5", "192.168.1.9:5555", "192.168.1.5:80", "https://192.168.1.5:80/x"},
		{"192.168.1.5:8080", "192.168.1.9:5555", "192.168.1.5:8080", "https://192.168.1.5:8080/x"},
		{"evil.com", "192.168.1.9:5555", "192.168.1.5:8080", "https://192.168.1.5:8080/x"},
		{"evil.com", "[fe80::9]:5555", "[fe80::1%eth0]:8080", "https://[fe80::1%25eth0]:8080/x"},
		{"m", "127.0.0.1:5555", "127.0.0.1:8080", ""},
	} {
		r := httptest.NewRequest("GET", "/x", nil)
		r.Host, r.RemoteAddr = c.host, c.remote
		la, _ := net.ResolveTCPAddr("tcp", strings.Replace(c.local, "%eth0", "", 1))
		if strings.Contains(c.local, "%") {
			la.Zone = "eth0"
		}
		r = r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey, net.Addr(la)))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if got := rec.Header().Get("Location"); got != c.want {
			t.Errorf("host %s via %s: Location %q, want %q (code %d)", c.host, c.local, got, c.want, rec.Code)
		}
	}
}

// 7c. a garbled or oversized sign-in body: 400, not counted as a wrong password
func TestReLoginBadBodyNotCounted(t *testing.T) {
	redteamSetup(t)
	reClearFails(t)
	login := func(body string) int {
		r := httptest.NewRequest("POST", "/api/login", strings.NewReader(body))
		r.RemoteAddr, r.Host = "192.168.1.66:4000", "192.168.1.10"
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Requested-With", "panel")
		rec := httptest.NewRecorder()
		handleLogin(rec, r)
		return rec.Code
	}
	big := `{"username":"owner","password":"` + strings.Repeat("x", 70<<10) + `"}`
	for i := 0; i < 8; i++ {
		for _, b := range []string{"", "{", "not json", big} {
			if code := login(b); code != http.StatusBadRequest {
				t.Fatalf("body %.20q: %d, want 400", b, code)
			}
		}
	}
	if code := login(`{"username":"owner","password":"owner-password-1"}`); code != http.StatusOK {
		t.Fatalf("after garbled bodies the right password must still sign in: %d", code)
	}
}
