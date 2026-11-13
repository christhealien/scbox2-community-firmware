package main

import (
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"

	"sclite-agent/internal/authfile"
	"sclite-agent/internal/sshcore"
)

// netConfig: the fields of the panel's network settings (main.go netConfig) this server reads.
type netConfig struct {
	Port         int  `json:"port"`
	SSHOff       bool `json:"ssh_off,omitempty"`
	SSHPort      int  `json:"ssh_port,omitempty"`
	AllowOutside bool `json:"allow_outside,omitempty"`
}

// sshPortOf: the port SSH uses for these settings (the panel's sshPort, with the panel's port as it opens it).
func sshPortOf(c netConfig) int {
	return sshcore.Port(c.SSHPort, sshcore.WebPort(c.Port, c.SSHPort))
}

type server struct {
	authPath, netPath, statePath, eventsPath string
	bindHost                                 string // "" = every address (tests: 127.0.0.1)
	createAfter                              time.Duration
	started                                  time.Time
	keepEvery, keepWait                      time.Duration
	addrs                                    func() ([]net.Addr, error)
	entropy                                  func()
	lim                                      *sshcore.Limiter
	hashSem                                  chan struct{} // password checks at once (each is a slow hash)

	mu        sync.Mutex
	ln        net.Listener
	listenOn  string // what it listens on now, "" when off
	lastErr   string
	signer    ssh.Signer
	secret    string // the login file's signing key when last read: a new one means a new password
	secretSet bool
	outside   atomic.Bool // "Allow access from outside the local network"

	authWriteMu sync.Mutex // a password change by this server, and reading the login file's key

	conns, handsh chan struct{}
	liveMu        sync.Mutex
	live          map[net.Conn]bool
	preAuth       map[string]int
	refuseSem     chan struct{}
	evMu          sync.Mutex
	connWG        sync.WaitGroup // the connections being served (stop waits for them, briefly)
}

func newServer(authPath, netPath, statePath, eventsPath string) *server {
	s := &server{authPath: authPath, netPath: netPath, statePath: statePath, eventsPath: eventsPath,
		createAfter: 2 * time.Minute, started: time.Now(), keepEvery: sshcore.KeepEvery, keepWait: sshcore.KeepWait,
		addrs: net.InterfaceAddrs, lim: sshcore.NewLimiter(), hashSem: make(chan struct{}, 2),
		conns: make(chan struct{}, sshcore.MaxConns), handsh: make(chan struct{}, sshcore.MaxHandshake),
		live: map[net.Conn]bool{}, preAuth: map[string]int{}, refuseSem: make(chan struct{}, 16)}
	s.entropy = func() {
		sshcore.WaitEntropy("/dev/random", "/dev/urandom", 90*time.Second, log.Printf)
	}
	s.lim.OnBlock = func(key, msg string) {
		log.Print(msg)
		s.event(map[string]any{"ev": "block", "msg": msg})
	}
	return s
}

func (s *server) hostKeyPath() string {
	return filepath.Join(filepath.Dir(s.authPath), sshcore.HostKeyName)
}

// netConfig: the saved settings (the backup when the file is damaged; nothing saved: SSH on, port 22).
func (s *server) netConfig() netConfig {
	var c netConfig
	for _, p := range []string{s.netPath, s.netPath + ".bak"} {
		if b, err := os.ReadFile(p); err == nil && json.Valid(b) {
			json.Unmarshal(b, &c)
			return c
		}
	}
	return c
}

// peerAllowed: the panel's rule (access.go): the local network only, unless access from outside is allowed.
func (s *server) peerAllowed(ip net.IP) bool {
	return ip == nil || sshcore.LocalPeer(ip, s.addrs) || s.outside.Load()
}

// apply: re-read the settings and the login file and follow them: listen (or not) on the port saved, close what
// is no longer allowed. Called at start, every few seconds and on SIGHUP.
func (s *server) apply() {
	c := s.netConfig()
	if was := s.outside.Swap(c.AllowOutside); was && !c.AllowOutside { // off means off: those from outside go
		if n := s.dropWhere(func(nc net.Conn) bool { return !s.peerAllowed(sshcore.RemoteIP(nc.RemoteAddr())) }); n > 0 {
			log.Printf("closed %d connection(s) from outside the local network (switched off)", n)
		}
	}
	s.followLogin()
	want := ""
	if !c.SSHOff {
		want = net.JoinHostPort(s.bindHost, strconv.Itoa(sshPortOf(c)))
	}
	s.mu.Lock()
	if !(want == s.listenOn && (want == "" || s.ln != nil)) {
		s.listen(want)
	}
	s.mu.Unlock()
	s.writeState(c)
}

// listen: start, move or stop the listener (s.mu held).
func (s *server) listen(want string) {
	if s.ln != nil {
		s.ln.Close()
		s.ln = nil
	}
	s.listenOn, s.lastErr = "", ""
	if want == "" {
		if n := s.dropAll(); n > 0 { // off means off: an open session (anyone's) doesn't stay
			log.Printf("switched off, %d connection(s) closed", n)
		}
		return
	}
	if s.signer == nil {
		sg, err := sshcore.HostKey(s.hostKeyPath(), brand, s.entropy, log.Printf)
		if err != nil {
			s.lastErr = "host key: " + err.Error()
			return
		}
		s.signer = sg
	}
	ln, err := net.Listen("tcp", want)
	if err != nil {
		s.lastErr = err.Error()
		log.Printf("%v", err)
		return
	}
	s.ln, s.listenOn = ln, want
	log.Printf("listening on %s", want)
	go s.serve(ln)
}

// followLogin: a new signing key in the login file means a new password (the panel's Account page): the
// connections signed in with the old one are closed, as the panel's sessions are. No login file long after start
// (the panel never made one): the factory login is made here, so SSH can still be used to repair the miner.
func (s *server) followLogin() {
	s.authWriteMu.Lock()
	defer s.authWriteMu.Unlock()
	a, _, err := authfile.Read(s.authPath)
	if errors.Is(err, os.ErrNotExist) && s.createAfter > 0 && time.Since(s.started) > s.createAfter {
		s.makeFactoryLogin()
		return
	}
	if err != nil {
		return
	}
	s.mu.Lock()
	changed := s.secretSet && a.Secret != s.secret
	s.secret, s.secretSet = a.Secret, true
	s.mu.Unlock()
	if changed {
		if n := s.dropAll(); n > 0 {
			log.Printf("password changed: %d connection(s) closed", n)
			s.event(map[string]any{"ev": "pwdrop", "n": n})
		}
	}
}

// makeFactoryLogin: the panel's first-start login (admin/admin, to be changed at the first sign-in). authWriteMu
// held.
func (s *server) makeFactoryLogin() {
	if _, err := os.Lstat(s.authPath); !os.IsNotExist(err) {
		return
	}
	s.entropy() // the signing key lasts: not before the kernel's random pool is seeded
	a := authfile.Store{Username: authfile.DefaultUser, MustChange: true,
		Secret: base64.StdEncoding.EncodeToString(authfile.RandBytes(32))}
	authfile.SetPassword(&a, authfile.DefaultPass, authfile.LegacyOnly(s.authPath))
	b, _ := json.MarshalIndent(a, "", "  ")
	if _, err := os.Lstat(s.authPath); !os.IsNotExist(err) { // the panel made one meanwhile
		return
	}
	if err := authfile.Write(s.authPath, filepath.Base(s.authPath)+".sshd.tmp", b); err != nil {
		log.Printf("no login file and couldn't make one (%v)", err)
		return
	}
	s.mu.Lock()
	s.secret, s.secretSet = a.Secret, true
	s.mu.Unlock()
	log.Printf("no login file %s after %s (the control panel never started): made the factory login (must be changed at the first sign-in)", s.authPath, s.createAfter)
}

func (s *server) stop() {
	s.mu.Lock()
	if s.ln != nil {
		s.ln.Close()
		s.ln = nil
	}
	s.listenOn = ""
	s.mu.Unlock()
	s.dropAll()
	done := make(chan struct{})
	go func() { s.connWG.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
	}
	os.Remove(s.statePath)
}

// ---------- connections ----------

func (s *server) admit(c net.Conn, k string) bool {
	s.liveMu.Lock()
	defer s.liveMu.Unlock()
	if s.preAuth[k] >= sshcore.PreAuthPerIP {
		return false
	}
	s.preAuth[k]++
	s.live[c] = true
	return true
}

func (s *server) preAuthDone(k string) {
	s.liveMu.Lock()
	if s.preAuth[k]--; s.preAuth[k] <= 0 {
		delete(s.preAuth, k)
	}
	s.liveMu.Unlock()
}

func (s *server) forget(c net.Conn) {
	s.liveMu.Lock()
	delete(s.live, c)
	s.liveMu.Unlock()
}

func (s *server) dropAll() int { return s.dropWhere(func(net.Conn) bool { return true }) }

func (s *server) dropWhere(match func(net.Conn) bool) int {
	s.liveMu.Lock()
	cs := make([]net.Conn, 0, len(s.live))
	for c := range s.live {
		if match(c) {
			cs = append(cs, c)
		}
	}
	s.liveMu.Unlock()
	for _, c := range cs {
		c.Close()
	}
	return len(cs)
}

// refuseOutside: one line for a peer from outside the local network, then closed (at most 16 at a time).
func (s *server) refuseOutside(c net.Conn) {
	select {
	case s.refuseSem <- struct{}{}:
	default:
		c.Close()
		return
	}
	go func() {
		defer func() { <-s.refuseSem }()
		c.SetWriteDeadline(time.Now().Add(2 * time.Second))
		c.Write([]byte(sshcore.RefuseOutsideLine))
		c.Close()
	}()
}

func (s *server) serve(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			time.Sleep(200 * time.Millisecond)
			continue
		}
		if p := sshcore.RemoteIP(c.RemoteAddr()); p != nil && !s.peerAllowed(p) {
			s.refuseOutside(c) // from outside the local network: SSH isn't for the internet
			continue
		}
		ip := sshcore.HostOf(c.RemoteAddr())
		if s.lim.Blocked(ip) {
			c.Close()
			continue
		}
		k := sshcore.FailKey(ip)
		if !s.admit(c, k) {
			c.Close() // this address has enough sign-ins under way
			continue
		}
		s.mu.Lock()
		current := s.ln == ln // switched off (or moved) just now: dropAll may have run already
		s.mu.Unlock()
		if !current {
			s.preAuthDone(k)
			s.forget(c)
			c.Close()
			continue
		}
		select {
		case s.conns <- struct{}{}:
		default:
			s.preAuthDone(k)
			s.forget(c)
			c.Close() // too many open connections
			continue
		}
		s.connWG.Add(1)
		go func() {
			defer s.connWG.Done()
			defer func() { <-s.conns }()
			defer s.forget(c)
			s.conn(c, ip, k)
		}()
	}
}

func (s *server) config(ip string) *ssh.ServerConfig {
	cfg := &ssh.ServerConfig{
		MaxAuthTries:  3,
		ServerVersion: "SSH-2.0-" + brand,
		PasswordCallback: func(m ssh.ConnMetadata, pw []byte) (*ssh.Permissions, error) {
			if !s.lim.Attempt(ip) { // blocked, or too many checks at once
				return nil, errors.New("blocked")
			}
			a, _, err := authfile.Read(s.authPath) // read at every sign-in: the panel may have changed it
			s.hashSem <- struct{}{}
			ok := err == nil && subtle.ConstantTimeCompare([]byte(m.User()), []byte(a.Username)) == 1
			ok = authfile.Check(&a, string(pw)) && ok // always run the hash: no timing hint about the username
			<-s.hashSem
			s.lim.Done(ip, "SSH", ok)
			if !ok {
				time.Sleep(600 * time.Millisecond)
				return nil, errors.New("wrong username or password")
			}
			p := &ssh.Permissions{Extensions: map[string]string{}}
			if a.MustChange {
				p.Extensions[sshcore.MustChangeExt] = "1"
			}
			return p, nil
		},
		BannerCallback: func(ssh.ConnMetadata) string {
			return brandProduct + ": sign in with your control panel username and password.\n"
		},
	}
	s.mu.Lock()
	cfg.AddHostKey(s.signer)
	s.mu.Unlock()
	return cfg
}

func (s *server) conn(nc net.Conn, ip, k string) {
	defer nc.Close()
	select {
	case s.handsh <- struct{}{}:
	case <-time.After(20 * time.Second):
		s.preAuthDone(k)
		return
	}
	nc.SetDeadline(time.Now().Add(60 * time.Second)) // sign in within a minute
	conn, chans, reqs, err := ssh.NewServerConn(nc, s.config(ip))
	<-s.handsh
	s.preAuthDone(k)
	if err != nil {
		return
	}
	nc.SetDeadline(time.Time{})
	log.Printf("sign-in: %s from %s", conn.User(), ip)
	s.event(map[string]any{"ev": "signin", "user": conn.User(), "ip": ip})
	sshcore.ServeEvery(conn, chans, reqs, s.forcedChange, s.keepEvery, s.keepWait)
}

// forcedChange: the first sign-in with the factory password chooses a new one, saved to the panel's login file.
func (s *server) forcedChange(ch ssh.Channel) bool {
	return sshcore.ForcedChange(ch, func(pw string) string {
		var nh authfile.Store
		authfile.SetPassword(&nh, pw, authfile.LegacyOnly(s.authPath)) // the slow hash, before taking the lock
		s.authWriteMu.Lock()
		defer s.authWriteMu.Unlock()
		cur, _, err := authfile.Read(s.authPath)
		if err != nil {
			return "Could not save the new password: " + err.Error() + "\r\n"
		}
		if !cur.MustChange { // changed meanwhile (in the panel): don't overwrite it
			return "The password was just changed in the control panel. Sign in again with that one.\r\n"
		}
		nh.MustChange = false
		nh.Secret = base64.StdEncoding.EncodeToString(authfile.RandBytes(32)) // signs out every session made with admin/admin
		if _, err := authfile.Update(s.authPath, filepath.Base(s.authPath)+".sshd.tmp", func(obj map[string]json.RawMessage) error {
			authfile.SetFields(obj, &nh)
			return nil
		}); err != nil {
			return "Could not save the new password: " + err.Error() + "\r\n"
		}
		s.mu.Lock()
		s.secret, s.secretSet = nh.Secret, true // this server's own change: its connections stay
		s.mu.Unlock()
		log.Printf("password changed (first SSH sign-in)")
		s.event(map[string]any{"ev": "pwchange"})
		return ""
	})
}

// ---------- state and events for the panel ----------

// state: what the panel's Settings page shows (main.go sshdState).
type state struct {
	Pid         int    `json:"pid"`
	Enabled     bool   `json:"enabled"`
	Port        int    `json:"port"`
	Listening   bool   `json:"listening"`
	Err         string `json:"err"`
	Fingerprint string `json:"fingerprint"`
	Applied     int64  `json:"applied"` // when the settings were last applied (unix ns)
}

func (s *server) writeState(c netConfig) {
	s.mu.Lock()
	st := state{Pid: os.Getpid(), Enabled: !c.SSHOff, Port: sshPortOf(c), Listening: s.listenOn != "", Err: s.lastErr,
		Applied: time.Now().UnixNano()}
	if s.signer != nil {
		st.Fingerprint = ssh.FingerprintSHA256(s.signer.PublicKey())
	}
	s.mu.Unlock()
	if st.Fingerprint == "" {
		st.Fingerprint = sshcore.Fingerprint(s.hostKeyPath())
	}
	b, _ := json.Marshal(st)
	tmp := s.statePath + ".tmp"
	if os.WriteFile(tmp, b, 0o644) == nil {
		os.Rename(tmp, s.statePath)
	}
}

const eventsMax = 64 << 10

// event: one line for the panel's activity log (it reads the file every few seconds). Kept small: past
// eventsMax it starts over (the panel notices the file got shorter).
func (s *server) event(ev map[string]any) {
	ev["t"] = time.Now().Unix()
	b, _ := json.Marshal(ev)
	s.evMu.Lock()
	defer s.evMu.Unlock()
	if fi, err := os.Stat(s.eventsPath); err == nil && fi.Size() > eventsMax {
		os.Truncate(s.eventsPath, 0)
	}
	f, err := os.OpenFile(s.eventsPath, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return
	}
	fmt.Fprintf(f, "%s\n", b)
	f.Close()
}
