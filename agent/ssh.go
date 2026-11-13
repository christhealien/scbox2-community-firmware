package main

// SSH server built into the agent. It signs in with the panel's own username and password (the same
// account, so changing one changes both). While the panel still has its factory password (admin/admin),
// an SSH sign-in has to choose a new password before it gets a shell, just like the panel's first login.
//
// What it offers: an interactive shell (root, /bin/sh), running a command (ssh miner 'cmd'), and SFTP
// (sftp, and scp with OpenSSH 9+, which copies over SFTP). No port forwarding or agent forwarding.
// The host key is made on first start and kept with the agent's settings; each miner has its own.
//
// SC Box II (sshViaDaemon, set at start on the scbox2 builds): the panel serves no SSH itself. SSH is the separate
// program scbox2-sshd (cmd/scbox2-sshd, started before the panel by S47scbox2-sshd), so a damaged or crashing
// panel can't take SSH with it. The Settings controls still work: the panel saves them and signals scbox2-sshd
// (sshd_link.go), and shows its state. The session handling both use is internal/sshcore.

import (
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"sclite-agent/internal/sshcore"
)

const (
	sshDefaultPort  = sshcore.DefaultPort
	sshMaxConns     = sshcore.MaxConns     // open SSH connections at once
	sshMaxHandshake = sshcore.MaxHandshake // sign-ins being checked at once (each check costs ~PBKDF2)
	sshFailLimit    = sshcore.FailLimit    // wrong passwords from one address ...
	sshFailWindow   = sshcore.FailWindow   // ... within this long block it for the same time
	sshPreAuthPerIP = sshcore.PreAuthPerIP // connections from one address not signed in yet (idle ones can't take every slot)
)

// ---------- settings (kept in the panel's network settings file) ----------

func sshEnabled(c netConfig) bool { return !c.SSHOff }
func sshPort(c netConfig) int {
	if c.SSHPort >= 1 && c.SSHPort <= 65535 && sshPortBad(c.SSHPort) == "" {
		return c.SSHPort
	}
	return sshDefaultPort // a saved port that would take the panel's (or the miner's) can't lock anyone out
}

// sshPortBad: why this port can't be SSH's ("" = fine): not the panel's ports, not the miner's own.
func sshPortBad(p int) string { return sshcore.PortBad(p, portOf(*listenAddr)) }

// ---------- host key ----------

var (
	sshKeyOnce sync.Once
	sshSigner  ssh.Signer
	sshKeyErr  error
)

func sshHostKeyPath() string { return filepath.Join(filepath.Dir(*authFile), sshcore.HostKeyName) }

func sshHostKey() (ssh.Signer, error) {
	sshKeyOnce.Do(func() {
		// the host key lasts: not made before the kernel's random pool is seeded (access.go entropyReady)
		sshSigner, sshKeyErr = sshcore.HostKey(sshHostKeyPath(), brand, entropyReady, log.Printf)
	})
	return sshSigner, sshKeyErr
}

func sshFingerprint() string {
	if sshViaDaemon { // scbox2-sshd makes and keeps the key: the panel only reads it, never makes one
		if st, ok := sshdState(); ok && st.Fingerprint != "" {
			return st.Fingerprint
		}
		return sshcore.Fingerprint(sshHostKeyPath())
	}
	if s, err := sshHostKey(); err == nil {
		return ssh.FingerprintSHA256(s.PublicKey())
	}
	return ""
}

// ---------- wrong-password blocking ----------

type sshFails struct {
	n        int
	first    time.Time
	until    time.Time
	inflight int // panel sign-ins being checked right now
}

var (
	sshFailMu sync.Mutex
	sshFailBy = map[string]*sshFails{}
)

func sshHost(a net.Addr) string { return sshcore.HostOf(a) }

// failKey: IPv6 addresses count per /64 (one machine can use as many addresses in its /64 as it likes).
func failKey(ip string) string { return sshcore.FailKey(ip) }

// loginAttempt: a panel sign-in attempt is counted before the password is checked, so parallel guesses
// can't all get in before the first wrong one is counted (a right password clears the count). false: blocked.
func loginAttempt(ip, via string) bool {
	k := failKey(ip)
	sshFailMu.Lock()
	defer sshFailMu.Unlock()
	now := time.Now()
	f := sshFailBy[k]
	if f != nil && now.Before(f.until) {
		return false // blocked
	}
	if f == nil || (now.Sub(f.first) > sshFailWindow && f.inflight == 0) {
		f = &sshFails{first: now}
		sshFailBy[k] = f
	}
	if f.n+f.inflight >= sshFailLimit {
		return false // that many checks at once: try again in a moment (not counted, no block)
	}
	f.inflight++
	return true
}

// loginDone: the check that loginAttempt let through has finished; only a wrong password counts.
func loginDone(ip, via string, ok bool) {
	k := failKey(ip)
	sshFailMu.Lock()
	if f := sshFailBy[k]; f != nil && f.inflight > 0 {
		f.inflight--
	}
	sshFailMu.Unlock()
	if ok {
		sshSucceeded(ip)
	} else {
		loginFailed(ip, via)
	}
}

func sshBlocked(ip string) bool {
	ip = failKey(ip)
	sshFailMu.Lock()
	defer sshFailMu.Unlock()
	f := sshFailBy[ip]
	return f != nil && time.Now().Before(f.until)
}

func sshFailed(ip string) { loginFailed(ip, "SSH") }

// loginFailed counts a wrong password from ip (SSH or the panel share the count): 5 within 10 minutes block
// the address for 10 minutes, logged and notified.
func loginFailed(ip, via string) {
	ip = failKey(ip)
	sshFailMu.Lock()
	defer sshFailMu.Unlock()
	now := time.Now()
	f := sshFailBy[ip]
	if f == nil || now.Sub(f.first) > sshFailWindow {
		f = &sshFails{first: now}
		sshFailBy[ip] = f
	}
	f.n++
	if f.n >= sshFailLimit {
		f.until = now.Add(sshFailWindow)
		msg := fmt.Sprintf("%s blocked for %d min after %d wrong passwords (%s)", ip, int(sshFailWindow.Minutes()), f.n, via)
		activity.emit("🔒 " + msg)
		if f.n == sshFailLimit { // once per block
			go notify(evLogin, msg)
		}
	}
	if len(sshFailBy) > 1000 { // bound memory
		for k, v := range sshFailBy {
			if now.Sub(v.first) > sshFailWindow && now.After(v.until) {
				delete(sshFailBy, k)
			}
		}
	}
}

func sshSucceeded(ip string) {
	connMarkOwner(ip)
	ip = failKey(ip)
	sshFailMu.Lock()
	delete(sshFailBy, ip)
	sshFailMu.Unlock()
}

// ---------- server ----------

var (
	sshMu       sync.Mutex
	sshListener net.Listener
	sshListenOn string // what it's listening on now, "" when off
	sshLastErr  string
	sshConns    = make(chan struct{}, sshMaxConns)
	sshHandsh   = make(chan struct{}, sshMaxHandshake)
)

// Open connections (closed when SSH is switched off or the password changes) and, per address, how many
// haven't signed in yet.
var (
	sshLiveMu  sync.Mutex
	sshLive    = map[net.Conn]bool{}
	sshPreAuth = map[string]int{}
)

func sshAdmit(c net.Conn, k string) bool {
	sshLiveMu.Lock()
	defer sshLiveMu.Unlock()
	if sshPreAuth[k] >= sshPreAuthPerIP {
		return false
	}
	sshPreAuth[k]++
	sshLive[c] = true
	return true
}

// sshPreAuthDone: the connection signed in, or gave up.
func sshPreAuthDone(k string) {
	sshLiveMu.Lock()
	if sshPreAuth[k]--; sshPreAuth[k] <= 0 {
		delete(sshPreAuth, k)
	}
	sshLiveMu.Unlock()
}

func sshForget(c net.Conn) {
	sshLiveMu.Lock()
	delete(sshLive, c)
	sshLiveMu.Unlock()
}

// sshDropAll closes every SSH connection (its shells are hung up); how many there were.
func sshDropAll() int { return sshDropWhere(func(net.Conn) bool { return true }) }

// sshDropWhere closes the SSH connections that match (access.go: those from outside, when that is switched off).
func sshDropWhere(match func(net.Conn) bool) int {
	if sshViaDaemon {
		// scbox2-sshd holds the connections: told to re-read the settings and the login file, it closes those
		// signed in with an old password, and those from outside once that is switched off
		sshdPoke()
	}
	sshLiveMu.Lock()
	cs := make([]net.Conn, 0, len(sshLive))
	for c := range sshLive {
		if match(c) {
			cs = append(cs, c)
		}
	}
	sshLiveMu.Unlock()
	for _, c := range cs {
		c.Close()
	}
	return len(cs)
}

// sshApply starts, moves or stops the SSH server to match the settings.
func sshApply() {
	if sshViaDaemon { // SC Box II: scbox2-sshd follows the settings (signalled, and it re-reads them by itself)
		sshdApply()
		return
	}
	c := currentNetConfig()
	want := ""
	if *sshEnable && sshEnabled(c) {
		want = ":" + strconv.Itoa(sshPort(c))
	}
	sshMu.Lock()
	defer sshMu.Unlock()
	if want == sshListenOn && (want == "" || sshListener != nil) {
		return
	}
	if sshListener != nil {
		sshListener.Close()
		sshListener = nil
	}
	sshListenOn, sshLastErr = "", ""
	if want == "" {
		if n := sshDropAll(); n > 0 { // off means off: an open session (anyone's) doesn't stay
			log.Printf("ssh: switched off, %d connection(s) closed", n)
		}
		return
	}
	if _, err := sshHostKey(); err != nil {
		sshLastErr = "host key: " + err.Error()
		return
	}
	ln, err := net.Listen("tcp", want)
	if err != nil {
		sshLastErr = err.Error()
		log.Printf("ssh: %v", err)
		return
	}
	sshListener, sshListenOn = ln, want
	log.Printf("ssh: listening on %s", want)
	go sshServe(ln)
}

func sshServe(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			time.Sleep(200 * time.Millisecond)
			continue
		}
		if p := remoteIPOf(c.RemoteAddr()); p != nil && !peerAllowed(p) {
			sshRefuseOutside(c) // from outside the local network: SSH isn't for the internet (access.go)
			continue
		}
		ip := sshHost(c.RemoteAddr())
		if sshBlocked(ip) {
			c.Close()
			continue
		}
		k := failKey(ip)
		if !sshAdmit(c, k) {
			c.Close() // this address has enough sign-ins under way
			continue
		}
		sshMu.Lock()
		current := sshListener == ln // switched off (or moved) just now: sshDropAll may have run already
		sshMu.Unlock()
		if !current {
			sshPreAuthDone(k)
			sshForget(c)
			c.Close()
			continue
		}
		select {
		case sshConns <- struct{}{}:
		default:
			sshPreAuthDone(k)
			sshForget(c)
			c.Close() // too many open connections
			continue
		}
		go func() {
			defer func() { <-sshConns }()
			defer sshForget(c)
			sshConn(c, ip, k)
		}()
	}
}

func sshConfig(ip string) *ssh.ServerConfig {
	cfg := &ssh.ServerConfig{
		MaxAuthTries:  3,
		ServerVersion: "SSH-2.0-" + brand,
		PasswordCallback: func(m ssh.ConnMetadata, pw []byte) (*ssh.Permissions, error) {
			if !loginAttempt(ip, "SSH") { // blocked, or too many checks at once (counted with the panel's)
				return nil, errors.New("blocked")
			}
			authMu.RLock()
			user, mustChange := auth.Username, auth.MustChange
			authMu.RUnlock()
			loginSem <- struct{}{} // shares the panel's limit of two password checks at a time
			ok := subtle.ConstantTimeCompare([]byte(m.User()), []byte(user)) == 1
			ok = checkPassword(string(pw)) && ok // always run the hash: no timing hint about the username
			<-loginSem
			loginDone(ip, "SSH", ok)
			if !ok {
				time.Sleep(600 * time.Millisecond)
				return nil, errors.New("wrong username or password")
			}
			p := &ssh.Permissions{Extensions: map[string]string{}}
			if mustChange {
				p.Extensions["must_change"] = "1"
			}
			return p, nil
		},
		BannerCallback: func(ssh.ConnMetadata) string {
			return brandProduct + ": sign in with your control panel username and password.\n"
		},
	}
	s, _ := sshHostKey()
	cfg.AddHostKey(s)
	return cfg
}

func sshConn(nc net.Conn, ip, k string) {
	defer nc.Close()
	select {
	case sshHandsh <- struct{}{}:
	case <-time.After(20 * time.Second):
		sshPreAuthDone(k)
		return
	}
	nc.SetDeadline(time.Now().Add(60 * time.Second)) // sign in within a minute
	conn, chans, reqs, err := ssh.NewServerConn(nc, sshConfig(ip))
	<-sshHandsh
	sshPreAuthDone(k)
	if err != nil {
		return
	}
	nc.SetDeadline(time.Time{})
	defer conn.Close()
	activity.emit(fmt.Sprintf("🔑 SSH sign-in: %s from %s", conn.User(), ip))
	// keepalives, shell / command / SFTP, and the forced first password change: internal/sshcore (shared with
	// scbox2-sshd)
	sshcore.Serve(conn, chans, reqs, sshForcedChange)
}

// sshLineReader: a typed line (no echo), sshcore.LineReader (the forced password change reads with it).
type sshLineReader struct {
	rw io.ReadWriter
	lr *sshcore.LineReader
}

const sshMaxLine = sshcore.MaxLine

func (r *sshLineReader) read() (line string, tooLong, ok bool) {
	if r.lr == nil {
		r.lr = sshcore.NewLineReader(r.rw)
	}
	return r.lr.Read()
}

// sshForcedChange makes the first SSH sign-in choose a new password (the panel's own rules: 8+
// characters). It changes the panel login too, because it is the same account.
func sshForcedChange(ch ssh.Channel) bool {
	return sshcore.ForcedChange(ch, func(p1 string) string {
		var nh authStore
		setPassword(&nh, p1) // the slow hash, before taking the lock
		authMu.Lock()
		if !auth.MustChange { // changed meanwhile (in the panel): don't overwrite it
			authMu.Unlock()
			return "The password was just changed in the control panel. Sign in again with that one.\r\n"
		}
		auth.setHashFrom(&nh)
		auth.Secret = base64.StdEncoding.EncodeToString(randBytes(32)) // signs out every session made with admin/admin
		auth.MustChange = false
		authMu.Unlock()
		if err := saveAuth(); err != nil {
			return "Could not save the new password: " + err.Error() + "\r\n"
		}
		activity.emit("🔑 password changed (first SSH sign-in)")
		return ""
	})
}

// ---------- panel API: GET status, POST {enabled, port} ----------

func handleSSH(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var in struct {
			Enabled *bool `json:"enabled"`
			Port    int   `json:"port"`
		}
		if !decodeIn(w, r, &in) {
			return
		}
		c := currentNetConfig()
		if in.Enabled != nil {
			c.SSHOff = !*in.Enabled
		}
		if in.Port != 0 {
			bad := sshPortBad(in.Port)
			if bad == "" && in.Port == c.Port {
				// the panel's port saved for the next start: SSH would take it and the panel fall back to :8080
				bad = fmt.Sprintf("port %d is saved as the control panel's port (from the next start)", in.Port)
			}
			if bad != "" {
				writeJSON(w, map[string]any{"ok": false, "err": bad})
				return
			}
			c.SSHPort = in.Port
		}
		if err := saveNetConfig(c); err != nil {
			writeJSON(w, map[string]any{"ok": false, "err": "could not save: " + err.Error()})
			return
		}
		sshApply()
		if sshEnabled(c) {
			activity.emit(fmt.Sprintf("🖧 SSH on (port %d)", sshPort(c)))
		} else {
			activity.emit("🖧 SSH off")
		}
	}
	writeJSON(w, sshStatus())
}

func sshStatus() map[string]any {
	if sshViaDaemon {
		return sshdStatus()
	}
	c := currentNetConfig()
	sshMu.Lock()
	listening, lastErr := sshListenOn != "", sshLastErr
	sshMu.Unlock()
	authMu.RLock()
	user := auth.Username
	authMu.RUnlock()
	return map[string]any{"ok": true, "enabled": sshEnabled(c) && *sshEnable, "port": sshPort(c),
		"listening": listening, "err": lastErr, "fingerprint": sshFingerprint(), "username": user,
		"available": *sshEnable}
}
