package main

// Who may reach the panel and SSH, and how a change is proven to come from the owner (the SC Box II security
// audit: M1, M2, L5-L8, L12).
//
//   - Local network only (M2): the panel ports and SSH answer loopback, private (RFC 1918), carrier-grade NAT
//     (100.64/10), link-local and same-LAN IPv6 peers. Settings -> Network -> "Allow access from outside the
//     local network" (off by default) lifts that. A refused web connection gets a plain one-line answer.
//   - HTTPS only (M1, optional, off by default): plain-http requests are redirected to https on the same port
//     (port 80 to 443). The certificate is this miner's own, self-signed (netlisten.go).
//   - Ports 80 and 443 can be switched off (L8); the main port (8080, or the saved one) always stays.
//   - A change (POST and the like) must name the panel's own page (L7): a same-origin Origin header,
//     Sec-Fetch-Site: same-origin, or the X-Requested-With header the page's own requests carry.
//   - Signing out ends the session on the miner too (L6): the cookie is remembered as revoked until it would
//     have expired anyway (kept across a panel restart in sessions-revoked.json).
//   - Installing firmware and changing the update source need the panel password again (M1), checked as a
//     sign-in is (the same wrong-password limits).
//
// secHardened: all of the above apply to the SC Box II builds (owner's and public). On the SC Lite build the
// panel behaves as before (IPv4 isn't filtered, a request without Origin is accepted, no new switches), except
// for two model-independent fixes: sign-out revokes the session (L6), and keys made at first start wait for
// the kernel's random pool (L12).

import (
	"bufio"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"sclite-agent/internal/sshcore"
)

var secHardened = brand == "scbox2"

// ---------------------------------------------------------------- the access switches (net.json)

var (
	accOutside   atomic.Bool // "Allow access from outside the local network"
	accHTTPSOnly atomic.Bool // "HTTPS only"
)

// accessLoad: the switches as saved (only on the hardened builds; the SC Lite never filters IPv4).
func accessLoad(c netConfig) {
	accOutside.Store(secHardened && c.AllowOutside)
	accHTTPSOnly.Store(secHardened && c.HTTPSOnly)
}

// peerAllowed: may this address reach the panel or SSH at all?
func peerAllowed(ip net.IP) bool {
	if ip == nil || localPeer(ip) {
		return true
	}
	if secHardened {
		return accOutside.Load()
	}
	return ip.To4() != nil // SC Lite: IPv4 is never filtered by address (as before); IPv6 only from the LAN
}

const outsideMsg = "This miner's control panel answers only on the local network. To reach it from outside, " +
	"switch on Settings -> Network -> \"Allow access from outside the local network\" (from a device on the local network).\n"

var refuseSem = make(chan struct{}, 16)

const refuseReadMax = 8 << 10 // bytes read from a refused peer before the answer (a browser's request head fits)

// refuseOutside: a plain-http connection from outside the local network gets a short plain-text answer
// (https and everything else: closed). At most 16 at a time, 5 s each: the internet can't hold the panel busy.
func refuseOutside(c net.Conn) {
	if !secHardened { // SC Lite: closed without a word, as before
		c.Close()
		return
	}
	select {
	case refuseSem <- struct{}{}:
	default:
		c.Close()
		return
	}
	go func() {
		defer func() { <-refuseSem }()
		defer c.Close()
		c.SetDeadline(time.Now().Add(5 * time.Second))
		// at most refuseReadMax bytes are read in all (a peer that never sends a newline can't make the
		// header read grow without end): the deadline still ends a slow one
		br := bufio.NewReader(io.LimitReader(c, refuseReadMax))
		b, err := br.Peek(1)
		if err != nil || b[0] == 0x16 { // nothing sent, or a TLS handshake: no way to show a message
			return
		}
		for i := 0; i < 64; i++ { // read the request's head (a browser shows nothing if we answer before it)
			l, err := br.ReadString('\n')
			if err != nil || l == "\r\n" || l == "\n" {
				break
			}
		}
		c.Write([]byte("HTTP/1.1 403 Forbidden\r\nContent-Type: text/plain; charset=utf-8\r\nConnection: close\r\n" +
			"Content-Length: " + strconv.Itoa(len(outsideMsg)) + "\r\n\r\n" + outsideMsg))
	}()
}

// sshRefuseOutside: SSH from outside the local network: one text line before closing (RFC 4253 lets a server
// send lines before its version; ssh -v shows it).
func sshRefuseOutside(c net.Conn) {
	if !secHardened {
		c.Close()
		return
	}
	select { // (bounded, as refuseOutside)
	case refuseSem <- struct{}{}:
	default:
		c.Close()
		return
	}
	go func() {
		defer func() { <-refuseSem }()
		c.SetWriteDeadline(time.Now().Add(2 * time.Second))
		c.Write([]byte(sshcore.RefuseOutsideLine))
		c.Close()
	}()
}

// ---------------------------------------------------------------- changes from the panel's own page (L7)

// panelReqHeader: sent by the page's own requests (jpost, the uploads). A page on another site can't add it
// without a CORS preflight, which the panel never answers.
const panelReqHeader, panelReqValue = "X-Requested-With", "panel"

// changeFromPanel: may this state-changing request be acted on? Hardened builds: the Origin is the panel's own,
// or Sec-Fetch-Site says same-origin, or the page's own header is there; a request with none of them (a form or
// script elsewhere, a client that names nothing) is refused. SC Lite: as before (sameOrigin).
func changeFromPanel(r *http.Request) bool {
	if !secHardened {
		return sameOrigin(r)
	}
	if o := r.Header.Get("Origin"); o != "" {
		u, err := url.Parse(o)
		return err == nil && u.Host != "" && strings.EqualFold(u.Host, r.Host)
	}
	switch r.Header.Get("Sec-Fetch-Site") {
	case "same-origin":
		return true
	case "":
	default: // cross-site, same-site (another host of the same site), none
		return false
	}
	return r.Header.Get(panelReqHeader) == panelReqValue
}

// ---------------------------------------------------------------- the password again (M1)

// checkPanelPassword: the current panel password, checked as a sign-in is (the same per-address limits:
// 5 wrong in 10 minutes block the address, and at most two checks run at a time).
func checkPanelPassword(r *http.Request, pw, what string) error {
	if pw == "" {
		return errors.New("enter your panel password to " + what)
	}
	ip := remoteIP(r)
	if !loginAttempt(ip, "panel") {
		if sshBlocked(ip) {
			return errors.New("too many wrong passwords from this address: try again in 10 minutes")
		}
		return errors.New("too many password checks at once from this address: try again in a moment")
	}
	loginSem <- struct{}{}
	ok := checkPassword(pw)
	<-loginSem
	loginDone(ip, "panel", ok)
	if !ok {
		time.Sleep(600 * time.Millisecond) // as a wrong sign-in
		return errors.New("that isn't the panel password: nothing was changed")
	}
	return nil
}

// ---------------------------------------------------------------- HTTPS only (M1)

// httpsRedirect: with "HTTPS only" on, a plain-http request is sent to https: on the same port (every panel
// port speaks both), or from port 80 to 443 (to 80 itself when 443 is off). Loopback stays plain (the miner's
// own tools), and without a certificate nothing is redirected (no way in would be left).
func httpsRedirect(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS != nil || !accHTTPSOnly.Load() {
			h.ServeHTTP(w, r)
			return
		}
		if ip := net.ParseIP(remoteIP(r)); ip != nil && ip.IsLoopback() || panelCert() == nil {
			h.ServeHTTP(w, r)
			return
		}
		lport := ""
		if la, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok {
			_, lport, _ = net.SplitHostPort(la.String())
		}
		host := r.Host
		if hh, _, err := net.SplitHostPort(host); err == nil {
			host = hh
		}
		host = strings.Trim(host, "[]")
		if host == "" || !loginHostOK(host) { // a name that can't be this miner: its own address instead
			if la, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok {
				host, _, _ = net.SplitHostPort(la.String())
			}
		}
		if i := strings.IndexByte(host, '%'); i >= 0 && !strings.HasPrefix(host[i:], "%25") {
			host = host[:i] + "%25" + host[i+1:] // a link-local address's zone: escaped in a URL (RFC 6874)
		}
		if strings.Contains(host, ":") {
			host = "[" + host + "]"
		}
		port := ":" + lport
		if lport == "80" {
			port = ":80"
			if port443Running() {
				port = ""
			}
		}
		if lport == "" {
			port = ""
		}
		http.Redirect(w, r, "https://"+host+port+r.URL.RequestURI(), http.StatusTemporaryRedirect)
	})
}

// ---------------------------------------------------------------- ports 80 and 443 (L8)

var (
	extraMu      sync.Mutex
	extraHandler http.Handler
	extraMain    string        // the main port ("8080"): 80 / 443 aren't opened again when it is one of them
	lan80Stop    chan struct{} // open while port 80 is served
	tls443Stop   chan struct{} // open while port 443 is served
	tls443Up     atomic.Bool   // 443 is listening now
	port80Num    = "80"        // (tests use other ports)
	port443Addr  = ":443"
)

// port443Running: 443 listening now. (Read while 443 is being started or stopped, a redirect may name it a
// moment early or late: harmless, the browser just retries.)
func port443Running() bool { return tls443Up.Load() }

// extraPortsStart: from main, once the panel's handler and main port are known.
func extraPortsStart(h http.Handler, mainPort string) {
	extraMu.Lock()
	extraHandler, extraMain = h, mainPort
	extraMu.Unlock()
	extraPortsApply()
}

// extraPortsApply starts or stops ports 80 and 443 to match the flags and Settings.
func extraPortsApply() {
	c := currentNetConfig()
	extraMu.Lock()
	defer extraMu.Unlock()
	if extraHandler == nil {
		return
	}
	want80 := *lan80 && extraMain != port80Num && !(secHardened && c.Port80Off)
	want443 := *https443 && ":"+extraMain != port443Addr && !(secHardened && c.Port443Off)
	if want80 && lan80Stop == nil {
		lan80Stop = make(chan struct{})
		go followLAN(extraHandler, port80Num, lan80Stop)
	} else if !want80 && lan80Stop != nil {
		close(lan80Stop)
		lan80Stop = nil
	}
	if want443 && tls443Stop == nil {
		tls443Stop = make(chan struct{})
		go serve443(extraHandler, tls443Stop)
	} else if !want443 && tls443Stop != nil {
		close(tls443Stop)
		tls443Stop = nil
	}
}

// serve443: https only on :443 until stop (retried every 30 s while it can't be opened).
func serve443(h http.Handler, stop chan struct{}) {
	for {
		if l, err := net.Listen("tcp", port443Addr); err == nil {
			tls443Up.Store(true)
			go func() { <-stop; l.Close() }()
			log.Printf("panel: https on %s (%v)", port443Addr, servePanel(l, h, true))
			tls443Up.Store(false)
		}
		select {
		case <-stop:
			return
		case <-time.After(30 * time.Second):
		}
	}
}

// ---------------------------------------------------------------- sign-out ends the session (L6)

var (
	revMu   sync.Mutex
	revoked = map[string]int64{}   // signed cookie payload -> its expiry (unix s)
	revSeen = map[string]revInfo{} // when each entry was added (by the clock and by uptime)
	// revSaveMu: held from the snapshot to the file write, so concurrent sign-outs can't write the file out
	// of order (an older snapshot last would bring a revoked session back after a restart)
	revSaveMu sync.Mutex
)

// revInfo: when a revoked entry was added. Its wall-clock expiry alone isn't enough to drop it: a clock set
// forward (by hand, or a bad time server) would drop it, and setting the clock back would revive the cookie.
type revInfo struct {
	wall int64   // the clock (unix s)
	up   float64 // uptime (s): moves only forward, whatever the clock does
}

// clockPlausible: a clock the panel trusts to expire things by (2024 to 2039, as the hand-set clock allows).
func clockPlausible(t time.Time) bool { return t.Year() >= 2024 && t.Year() <= 2039 }

// uptimeKnown: systemUptime answers 1e9 when /proc/uptime can't be read.
func uptimeKnown(u float64) bool { return u < 1e9 }

// revPurgeable: may this revoked entry be forgotten now? Only once it has expired by a plausible clock that
// hasn't gone back since it was added, and at least sessionTTL of uptime later (the session it ended can't
// still be live by any clock).
func revPurgeable(exp int64, seen revInfo, now time.Time, up float64) bool {
	if !clockPlausible(now) || exp >= now.Unix() || now.Unix() < seen.wall {
		return false
	}
	return uptimeKnown(up) && up-seen.up >= sessionTTL.Seconds()
}

// revSeenOf: an entry's revInfo (one loaded from the file, or reset by a test, counts from now). revMu held.
func revSeenOf(k string, now time.Time, up float64) revInfo {
	si, ok := revSeen[k]
	if !ok || si.up > up { // (uptime went back: a reboot since)
		si = revInfo{now.Unix(), up}
		revSeen[k] = si
	}
	return si
}

const revokedMax = 500

func revokedPath() string { return filepath.Join(filepath.Dir(*authFile), "sessions-revoked.json") }

// cookiePayload: the payload of a correctly signed cookie, else "".
func cookiePayload(v string) string {
	parts := strings.SplitN(v, ".", 2)
	if len(parts) != 2 {
		return ""
	}
	payload, err1 := base64.RawURLEncoding.DecodeString(parts[0])
	sig, err2 := base64.RawURLEncoding.DecodeString(parts[1])
	if err1 != nil || err2 != nil {
		return ""
	}
	authMu.RLock()
	secret, _ := base64.StdEncoding.DecodeString(auth.Secret)
	authMu.RUnlock()
	mac := hmac.New(sha256.New, secret)
	mac.Write(payload)
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return ""
	}
	return string(payload)
}

func cookieRevoked(payload string) bool {
	revMu.Lock()
	defer revMu.Unlock()
	_, ok := revoked[payload]
	return ok
}

// revokeCookie: this (signed) session no longer signs in, here or anywhere it was copied to.
func revokeCookie(v string) {
	p := cookiePayload(v)
	if p == "" {
		return
	}
	f := strings.Split(p, "|")
	if len(f) < 2 {
		return
	}
	exp, _ := strconv.ParseInt(f[1], 10, 64)
	preClockMu.Lock()
	delete(preClockSess, p)
	preClockMu.Unlock()
	revSaveMu.Lock()
	defer revSaveMu.Unlock()
	revMu.Lock()
	now, up := cookieNow(), systemUptime()
	for k, e := range revoked {
		if revPurgeable(e, revSeenOf(k, now, up), now, up) {
			delete(revoked, k)
			delete(revSeen, k)
		}
	}
	for k := range revSeen {
		if _, ok := revoked[k]; !ok {
			delete(revSeen, k)
		}
	}
	revoked[p] = exp
	revSeen[p] = revInfo{now.Unix(), up}
	for len(revoked) > revokedMax { // bound: the soonest to expire go first
		ks := make([]string, 0, len(revoked))
		for k := range revoked {
			ks = append(ks, k)
		}
		sort.Slice(ks, func(i, j int) bool { return revoked[ks[i]] < revoked[ks[j]] })
		delete(revoked, ks[0])
		delete(revSeen, ks[0])
	}
	b, _ := json.Marshal(revoked)
	revMu.Unlock()
	if err := cfgWrite(revokedPath(), b, 0o600); err != nil {
		log.Printf("auth: couldn't save the signed-out sessions (%v): they stay signed out until the panel restarts", err)
	}
}

// A session lasts sessionTTL from when it was really issued, whatever the clock does: one issued while the clock
// read 2039 (set by hand, or a bad time server) isn't valid until 2039 once the clock is right again. Two bounds:
//   - this panel's own sessions: sessionTTL of uptime from the sign-in (sessUp; the newest revokedMax kept)
//   - every session (one from before a restart too): its expiry at most sessionTTL ahead of the clock now
//     (plus an hour for a clock corrected a little), while the clock is plausible
var (
	sessUpMu sync.Mutex
	sessUp   = map[string]float64{} // payload -> uptime (s) until which it is valid
)

func sessIssued(payload string) {
	up := systemUptime()
	if !uptimeKnown(up) {
		return
	}
	sessUpMu.Lock()
	defer sessUpMu.Unlock()
	sessUp[payload] = up + sessionTTL.Seconds()
	for k, u := range sessUp {
		if u < up {
			delete(sessUp, k)
		}
	}
	for len(sessUp) > revokedMax {
		old := ""
		for k, u := range sessUp {
			if old == "" || u < sessUp[old] {
				old = k
			}
		}
		delete(sessUp, old)
	}
}

func sessWithinTTL(payload string, exp int64) bool {
	if now := cookieNow(); clockPlausible(now) && exp > now.Add(sessionTTL+time.Hour).Unix() {
		return false // issued by a clock ahead of today's
	}
	sessUpMu.Lock()
	upTo, ok := sessUp[payload]
	sessUpMu.Unlock()
	if up := systemUptime(); ok && uptimeKnown(up) && up > upTo {
		return false
	}
	return true
}

// loadRevoked at start: the sessions signed out before the panel restarted.
func loadRevoked() {
	b, ok := readConfig(revokedPath(), json.Valid)
	if !ok {
		return
	}
	var m map[string]int64
	if json.Unmarshal(b, &m) != nil {
		return
	}
	// all are kept (at most revokedMax): which have really expired isn't known by the clock alone (it may have
	// been set forward), so each counts as added now and is dropped later by revPurgeable's rules
	now, up := cookieNow(), systemUptime()
	revMu.Lock()
	for k, e := range m {
		revoked[k] = e
		revSeen[k] = revInfo{now.Unix(), up}
	}
	for len(revoked) > revokedMax {
		ks := make([]string, 0, len(revoked))
		for k := range revoked {
			ks = append(ks, k)
		}
		sort.Slice(ks, func(i, j int) bool { return revoked[ks[i]] < revoked[ks[j]] })
		delete(revoked, ks[0])
		delete(revSeen, ks[0])
	}
	revMu.Unlock()
}

// forgetRevoked: a new signing key (password change) ends every older session anyway.
func forgetRevoked() {
	revSaveMu.Lock()
	defer revSaveMu.Unlock()
	revMu.Lock()
	n := len(revoked)
	revoked = map[string]int64{}
	revSeen = map[string]revInfo{}
	revMu.Unlock()
	if n > 0 {
		os.Remove(revokedPath())
	}
}

// ---------------------------------------------------------------- the kernel's random pool (L12)

// Go 1.24's crypto/rand uses getrandom(2), which waits for the kernel's pool to be seeded. The Box II's Linux
// 3.10 has no getrandom (ENOSYS), and Go then reads /dev/urandom without any wait, which on 3.10 answers even
// before the pool has gathered anything (right after a first boot: no saved seed yet). So before a key that
// lasts (the session-signing key, the SSH host key, the TLS key) is made, wait until /dev/random is readable:
// on 3.10 that means the input pool holds enough entropy. At most 90 s, logged when it had to wait.
//
// Readable means 64 bits in the input pool, but /dev/urandom's own pool may not have been reseeded from it yet.
// So then 32 bytes are read from /dev/random (blocking, within the same 90 s) and written to /dev/urandom,
// which mixes them into the pool urandom reads from.
var (
	entropyOnce sync.Once
	entropyWait = 90 * time.Second
	devRandom   = "/dev/random"
	devURandom  = "/dev/urandom"
)

func entropyReady() {
	entropyOnce.Do(func() { sshcore.WaitEntropy(devRandom, devURandom, entropyWait, log.Printf) })
}

// ---------------------------------------------------------------- Settings -> Network (the switches)

// accessView: the switches for Settings -> Network; nil where they don't exist (SC Lite).
func accessView() map[string]any {
	if !secHardened {
		return nil
	}
	c := currentNetConfig()
	extraMu.Lock()
	on80, on443 := lan80Stop != nil, tls443Stop != nil
	extraMu.Unlock()
	return map[string]any{"allow_outside": c.AllowOutside, "https_only": c.HTTPSOnly,
		"port80": !c.Port80Off, "port443": !c.Port443Off, "port80_on": on80, "port443_on": on443 && port443Running(),
		"port80_available": *lan80, "port443_available": *https443}
}

// handleAccess: POST /api/net {allow_outside, https_only, port80, port443} (any of them). Applies at once.
// Switching "Allow access from outside" on opens the panel and root SSH to any address: it needs the panel
// password again (pw, checked as a sign-in is); switching it off needs nothing, and drops the connections from
// outside that are still open (an SSH session, a browser's keep-alive).
func handleAccess(w http.ResponseWriter, r *http.Request, outside, httpsOnly, p80, p443 *bool, pw string) {
	if !secHardened {
		writeJSON(w, map[string]any{"ok": false, "err": "not available on this model"})
		return
	}
	if httpsOnly != nil && *httpsOnly && panelCert() == nil {
		writeJSON(w, map[string]any{"ok": false, "err": "the miner has no https certificate, so HTTPS only can't be switched on"})
		return
	}
	c := currentNetConfig()
	if outside != nil && *outside && !c.AllowOutside {
		if err := checkPanelPassword(r, pw, "allow access from outside the local network"); err != nil {
			writeJSON(w, map[string]any{"ok": false, "err": err.Error(), "access": accessView()})
			return
		}
	}
	wasOutside := c.AllowOutside
	var notes []string
	if outside != nil && c.AllowOutside != *outside {
		c.AllowOutside = *outside
		notes = append(notes, map[bool]string{true: "access from outside the local network allowed",
			false: "local network only"}[*outside])
	}
	if httpsOnly != nil && c.HTTPSOnly != *httpsOnly {
		c.HTTPSOnly = *httpsOnly
		notes = append(notes, map[bool]string{true: "HTTPS only: plain http is redirected", false: "plain http allowed again"}[*httpsOnly])
	}
	if p80 != nil && c.Port80Off == *p80 {
		c.Port80Off = !*p80
		notes = append(notes, map[bool]string{true: "port 80 on", false: "port 80 off"}[*p80])
	}
	if p443 != nil && c.Port443Off == *p443 {
		c.Port443Off = !*p443
		notes = append(notes, map[bool]string{true: "port 443 on", false: "port 443 off"}[*p443])
	}
	if len(notes) == 0 {
		writeJSON(w, map[string]any{"ok": true, "note": "nothing to change", "access": accessView()})
		return
	}
	if err := saveNetConfig(c); err != nil {
		writeJSON(w, map[string]any{"ok": false, "err": "could not save: " + err.Error()})
		return
	}
	accessLoad(c)
	extraPortsApply()
	if wasOutside && !c.AllowOutside { // off means off: what came in from outside while it was on goes too
		if n := dropOutsideConns(); n > 0 {
			notes = append(notes, strconv.Itoa(n)+" connection(s) from outside closed")
		}
	}
	for _, n := range notes {
		activity.emit("🖧 " + n)
	}
	if outside != nil && *outside {
		log.Printf("access: the panel and SSH now answer outside the local network too (switched on from %s)", remoteIP(r))
	}
	writeJSON(w, map[string]any{"ok": true, "note": strings.Join(notes, " · "), "access": accessView()})
}

// dropOutsideConns closes the panel connections and SSH sessions whose peer may no longer reach the miner
// (accepted while "Allow access from outside" was on); how many.
func dropOutsideConns() int {
	gone := func(c net.Conn) bool {
		ip := remoteIPOf(c.RemoteAddr())
		return ip != nil && !peerAllowed(ip)
	}
	connMu.Lock()
	var cs []net.Conn
	for c := range connLive {
		if gone(c) {
			cs = append(cs, c)
		}
	}
	connMu.Unlock()
	for _, c := range cs {
		c.Close()
	}
	n := len(cs) + sshDropWhere(gone)
	if n > 0 {
		log.Printf("access: closed %d connection(s) from outside the local network (switched off)", n)
	}
	return n
}
