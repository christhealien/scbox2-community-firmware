package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"sclite-agent/internal/authfile"
	"sclite-agent/internal/sshcore"
)

const ownerPass = "owner-pass-1"

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return p
}

// writeLogin: a login file as the panel writes it (extra fields the SSH server must keep).
func writeLogin(t *testing.T, path, user, pw string, mustChange bool) authfile.Store {
	t.Helper()
	a := authfile.Store{Username: user, MustChange: mustChange, Secret: "c2VjcmV0LXNlY3JldC1zZWNyZXQtc2VjcmV0MTIzNA=="}
	authfile.SetPassword(&a, pw, false)
	b, _ := json.Marshal(a)
	var obj map[string]any
	json.Unmarshal(b, &obj)
	obj["miner_pass"] = "stock-web"
	obj["acknowledged"] = "2026-01-01"
	b, _ = json.MarshalIndent(obj, "", "  ")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return a
}

func writeNet(t *testing.T, path string, c netConfig) {
	t.Helper()
	b, _ := json.Marshal(c)
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

type testSrv struct {
	*server
	dir  string
	port int
	addr string
}

// newTestSrv: the SSH server on a free loopback port, with a login (user admin, pw; must change or not).
func newTestSrv(t *testing.T, pw string, mustChange bool) *testSrv {
	t.Helper()
	dir := t.TempDir()
	s := newServer(filepath.Join(dir, "auth.json"), filepath.Join(dir, "net.json"), filepath.Join(dir, "state.json"),
		filepath.Join(dir, "events"))
	s.bindHost = "127.0.0.1"
	s.entropy = func() {}
	if pw != "" {
		writeLogin(t, s.authPath, "admin", pw, mustChange)
	}
	port := freePort(t)
	writeNet(t, s.netPath, netConfig{SSHPort: port})
	s.apply()
	t.Cleanup(s.stop)
	if !s.state(t).Listening {
		t.Fatalf("not listening: %+v", s.state(t))
	}
	return &testSrv{s, dir, port, "127.0.0.1:" + strconv.Itoa(port)}
}

func (s *server) state(t *testing.T) state {
	t.Helper()
	var st state
	b, err := os.ReadFile(s.statePath)
	if err != nil {
		t.Fatal(err)
	}
	json.Unmarshal(b, &st)
	return st
}

func dial(addr, user, pw string) (*ssh.Client, error) {
	return ssh.Dial("tcp", addr, &ssh.ClientConfig{User: user, Auth: []ssh.AuthMethod{ssh.Password(pw)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 10 * time.Second})
}

func run(t *testing.T, c *ssh.Client, cmd string) (string, int) {
	t.Helper()
	s, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	out := &shell{} // (a locked buffer: stdout and stderr are copied at the same time)
	s.Stdout, s.Stderr = out, out
	err = s.Run(cmd)
	code := 0
	if ee, ok := err.(*ssh.ExitError); ok {
		code = ee.ExitStatus()
	} else if err != nil {
		t.Fatalf("run %q: %v", cmd, err)
	}
	return out.String(), code
}

func alive(c *ssh.Client) bool {
	done := make(chan bool, 1)
	go func() {
		s, err := c.NewSession()
		if err != nil {
			done <- false
			return
		}
		defer s.Close()
		out, err := s.Output("echo alive")
		done <- err == nil && strings.Contains(string(out), "alive")
	}()
	select {
	case ok := <-done:
		return ok
	case <-time.After(5 * time.Second):
		return false
	}
}

func readEvents(t *testing.T, s *server) string {
	t.Helper()
	b, _ := os.ReadFile(s.eventsPath)
	return string(b)
}

func TestSignInShellCommandSFTP(t *testing.T) {
	ts := newTestSrv(t, ownerPass, false)
	c, err := dial(ts.addr, "admin", ownerPass)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if out, code := run(t, c, "echo hello; exit 3"); !strings.Contains(out, "hello") || code != 3 {
		t.Fatalf("command: %q exit %d", out, code)
	}
	if string(c.ServerVersion()) != "SSH-2.0-scbox2" {
		t.Fatalf("version %q", c.ServerVersion())
	}
	sc, err := sftp.NewClient(c)
	if err != nil {
		t.Fatal(err)
	}
	defer sc.Close()
	p := filepath.Join(ts.dir, "via-sftp.txt")
	f, err := sc.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	f.Write([]byte("sftp works"))
	f.Close()
	if b, _ := os.ReadFile(p); string(b) != "sftp works" {
		t.Fatalf("sftp wrote %q", b)
	}
	// no forwarding
	if _, err := c.Dial("tcp", "127.0.0.1:1"); err == nil {
		t.Fatal("port forwarding was allowed")
	}
	if !strings.Contains(readEvents(t, ts.server), `"ev":"signin"`) {
		t.Fatalf("no sign-in event for the panel: %q", readEvents(t, ts.server))
	}
	st := ts.state(t)
	if !st.Enabled || st.Port != ts.port || st.Fingerprint == "" || st.Pid != os.Getpid() {
		t.Fatalf("state %+v", st)
	}
}

func TestWrongPasswordsBlock(t *testing.T) {
	ts := newTestSrv(t, ownerPass, false)
	if _, err := dial(ts.addr, "someone", ownerPass); err == nil {
		t.Fatal("a wrong username signed in")
	}
	for i := 1; i < sshcore.FailLimit; i++ {
		if _, err := dial(ts.addr, "admin", "wrong-"+strconv.Itoa(i)); err == nil {
			t.Fatal("a wrong password signed in")
		}
	}
	if !ts.lim.Blocked("127.0.0.1") {
		t.Fatal("not blocked after 5 wrong sign-ins")
	}
	if c, err := dial(ts.addr, "admin", ownerPass); err == nil {
		c.Close()
		t.Fatal("a blocked address signed in with the right password")
	}
	if !strings.Contains(readEvents(t, ts.server), `"ev":"block"`) {
		time.Sleep(200 * time.Millisecond) // (the block event is written in the background)
		if !strings.Contains(readEvents(t, ts.server), `"ev":"block"`) {
			t.Fatalf("no block event for the panel: %q", readEvents(t, ts.server))
		}
	}
}

func TestLimiterWindowAndInflight(t *testing.T) {
	l := sshcore.NewLimiter()
	now := time.Unix(1_800_000_000, 0)
	l.SetClock(func() time.Time { return now })
	var blocks atomic.Int32
	l.OnBlock = func(string, string) { blocks.Add(1) }
	// five checks at once: the sixth waits (not counted), none is blocked yet
	for i := 0; i < sshcore.FailLimit; i++ {
		if !l.Attempt("10.0.0.9") {
			t.Fatalf("attempt %d refused", i)
		}
	}
	if l.Attempt("10.0.0.9") || l.Blocked("10.0.0.9") {
		t.Fatal("a sixth check at once was let through, or counted as a block")
	}
	for i := 0; i < sshcore.FailLimit; i++ {
		l.Done("10.0.0.9", "SSH", false)
	}
	if !l.Blocked("10.0.0.9") {
		t.Fatal("five wrong: not blocked")
	}
	now = now.Add(sshcore.FailWindow + time.Second)
	if l.Blocked("10.0.0.9") || !l.Attempt("10.0.0.9") {
		t.Fatal("still blocked after the window")
	}
	l.Done("10.0.0.9", "SSH", true)
	// IPv6: one /64 counts as one address
	for i := 0; i < sshcore.FailLimit; i++ {
		ip := "2001:db8::" + strconv.Itoa(i+1)
		l.Attempt(ip)
		l.Done(ip, "SSH", false)
	}
	if !l.Blocked("2001:db8::ffff") {
		t.Fatal("other addresses of the same /64 aren't blocked")
	}
	time.Sleep(50 * time.Millisecond)
	if blocks.Load() != 2 {
		t.Fatalf("%d block notices, want 2 (one per block)", blocks.Load())
	}
}

// shell: an interactive shell with a pty.
type shell struct {
	in  io.WriteCloser
	mu  sync.Mutex
	out bytes.Buffer
	s   *ssh.Session
}

func (sh *shell) Write(p []byte) (int, error) {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	return sh.out.Write(p)
}
func (sh *shell) String() string { sh.mu.Lock(); defer sh.mu.Unlock(); return sh.out.String() }

func (sh *shell) waitFor(t *testing.T, want string) {
	t.Helper()
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if strings.Contains(sh.String(), want) {
			return
		}
	}
	t.Fatalf("timed out waiting for %q; got:\n%s", want, sh.String())
}

func openShell(t *testing.T, c *ssh.Client) *shell {
	t.Helper()
	s, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RequestPty("xterm", 40, 120, ssh.TerminalModes{}); err != nil {
		t.Fatal(err)
	}
	sh := &shell{s: s}
	s.Stdout, s.Stderr = sh, sh
	sh.in, _ = s.StdinPipe()
	if err := s.Shell(); err != nil {
		t.Fatal(err)
	}
	return sh
}

func TestFactoryPasswordMustBeChanged(t *testing.T) {
	ts := newTestSrv(t, authfile.DefaultPass, true)
	before, _, _ := authfile.Read(ts.authPath)
	c, err := dial(ts.addr, "admin", "admin")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// a command (no terminal) gets no shell while the factory password is set
	if out, code := run(t, c, "echo should-not-run"); code == 0 || strings.Contains(out, "should-not-run") ||
		!strings.Contains(out, "factory password") {
		t.Fatalf("a command ran with the factory password: %q exit %d", out, code)
	}
	sh := openShell(t, c)
	sh.waitFor(t, "New password:")
	io.WriteString(sh.in, "short\r")
	sh.waitFor(t, "at least 8 characters")
	io.WriteString(sh.in, "brand-new-pass\r")
	sh.waitFor(t, "Type it again:")
	io.WriteString(sh.in, "brand-new-pass\r")
	sh.waitFor(t, "Password changed")
	io.WriteString(sh.in, "echo in-the-shell-$((40+2))\r")
	sh.waitFor(t, "in-the-shell-42")
	ts.apply() // its own change (a new signing key) doesn't close its own connection
	if !alive(c) {
		t.Fatal("the connection that changed the password was closed")
	}
	a, raw, err := authfile.Read(ts.authPath)
	if err != nil {
		t.Fatal(err)
	}
	if a.MustChange || !authfile.Check(&a, "brand-new-pass") || authfile.Check(&a, "admin") || a.Secret == before.Secret {
		t.Fatalf("the login file wasn't changed as the panel does it: %s", raw)
	}
	var obj map[string]any
	json.Unmarshal(raw, &obj)
	if obj["miner_pass"] != "stock-web" || obj["acknowledged"] != "2026-01-01" || obj["username"] != "admin" {
		t.Fatalf("fields the SSH server doesn't know were lost: %s", raw)
	}
	if _, err := os.Stat(ts.authPath + ".bak"); err != nil {
		t.Fatal("no .bak of the previous login file (the panel's saveAuth keeps one)")
	}
	if fi, _ := os.Stat(ts.authPath); fi.Mode().Perm() != 0o600 {
		t.Fatalf("login file mode %v", fi.Mode().Perm())
	}
	if !strings.Contains(readEvents(t, ts.server), `"ev":"pwchange"`) {
		t.Fatal("no password-change event for the panel")
	}
	if _, err := dial(ts.addr, "admin", "admin"); err == nil {
		t.Fatal("the factory password still signs in")
	}
	c2, err := dial(ts.addr, "admin", "brand-new-pass")
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	if out, code := run(t, c2, "echo ok"); code != 0 || !strings.Contains(out, "ok") {
		t.Fatalf("after the change: %q %d", out, code)
	}
}

func TestForcedChangeRefusedWhenPanelChangedItFirst(t *testing.T) {
	ts := newTestSrv(t, authfile.DefaultPass, true)
	c, err := dial(ts.addr, "admin", "admin")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	sh := openShell(t, c)
	sh.waitFor(t, "New password:")
	writeLogin(t, ts.authPath, "admin", "panel-chosen-1", false) // the owner chose one in the panel meanwhile
	io.WriteString(sh.in, "ssh-chosen-12\r")
	sh.waitFor(t, "Type it again:")
	io.WriteString(sh.in, "ssh-chosen-12\r")
	sh.waitFor(t, "just changed in the control panel")
	a, _, _ := authfile.Read(ts.authPath)
	if !authfile.Check(&a, "panel-chosen-1") {
		t.Fatal("the panel's new password was overwritten")
	}
}

func TestPasswordChangedInPanelClosesSessions(t *testing.T) {
	ts := newTestSrv(t, ownerPass, false)
	c, err := dial(ts.addr, "admin", ownerPass)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ts.apply()
	if !alive(c) {
		t.Fatal("closed with nothing changed")
	}
	writeLogin(t, ts.authPath, "admin", "panel-new-pass", false)
	b, _ := os.ReadFile(ts.authPath)
	var obj map[string]any
	json.Unmarshal(b, &obj)
	obj["secret"] = "bmV3LXNpZ25pbmcta2V5LW5ldy1zaWduaW5nLWtleQ==" // the panel's password change: a new signing key
	b, _ = json.Marshal(obj)
	os.WriteFile(ts.authPath, b, 0o600)
	ts.apply()
	if alive(c) {
		t.Fatal("a session signed in with the old password stayed open")
	}
	if _, err := dial(ts.addr, "admin", ownerPass); err == nil {
		t.Fatal("the old password still signs in")
	}
	c2, err := dial(ts.addr, "admin", "panel-new-pass")
	if err != nil {
		t.Fatalf("the panel's new password doesn't sign in: %v", err)
	}
	c2.Close()
}

// reAddr: a connection that seems to come from another address.
type reAddr struct {
	net.Conn
	ra net.Addr
}

func (c *reAddr) RemoteAddr() net.Addr { return c.ra }

func TestLocalNetworkOnly(t *testing.T) {
	ts := newTestSrv(t, ownerPass, false)
	ts.addrs = func() ([]net.Addr, error) {
		_, n, _ := net.ParseCIDR("203.0.113.7/24") // a LAN with public addresses: its own subnet is local
		return []net.Addr{n}, nil
	}
	for ip, want := range map[string]bool{"127.0.0.1": true, "192.168.1.20": true, "10.1.2.3": true,
		"100.64.0.5": true, "fe80::1": true, "fd00::5": true, "203.0.113.99": true, "8.8.8.8": false,
		"2001:4860::8888": false} {
		if got := ts.peerAllowed(net.ParseIP(ip)); got != want {
			t.Errorf("peerAllowed(%s) = %v, want %v", ip, got, want)
		}
	}
	// a refused peer gets one line, then the connection is closed
	a, b := net.Pipe()
	ts.refuseOutside(a)
	got, _ := io.ReadAll(b)
	if string(got) != sshcore.RefuseOutsideLine {
		t.Fatalf("refusal %q", got)
	}
	// "Allow access from outside" on: allowed; off again: what came in from outside is closed
	writeNet(t, ts.netPath, netConfig{SSHPort: ts.port, AllowOutside: true})
	ts.apply()
	if !ts.peerAllowed(net.ParseIP("8.8.8.8")) {
		t.Fatal("outside refused with access from outside allowed")
	}
	x, y := net.Pipe()
	defer y.Close()
	out := &reAddr{x, &net.TCPAddr{IP: net.ParseIP("8.8.8.8"), Port: 1234}}
	ts.admit(out, "test")
	writeNet(t, ts.netPath, netConfig{SSHPort: ts.port})
	ts.apply()
	y.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := y.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("a connection from outside stayed open after access from outside was switched off (%v)", err)
	}
}

func TestSettingsOnOffAndPort(t *testing.T) {
	ts := newTestSrv(t, ownerPass, false)
	c, err := dial(ts.addr, "admin", ownerPass)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	writeNet(t, ts.netPath, netConfig{SSHPort: ts.port, SSHOff: true})
	ts.apply()
	if alive(c) {
		t.Fatal("SSH off left an open session")
	}
	if st := ts.state(t); st.Enabled || st.Listening {
		t.Fatalf("state after off: %+v", st)
	}
	if nc, err := net.DialTimeout("tcp", ts.addr, time.Second); err == nil {
		nc.Close()
		t.Fatal("still listening with SSH off")
	}
	p2 := freePort(t)
	writeNet(t, ts.netPath, netConfig{SSHPort: p2})
	ts.apply()
	if st := ts.state(t); !st.Listening || st.Port != p2 {
		t.Fatalf("state after the move: %+v", st)
	}
	c2, err := dial("127.0.0.1:"+strconv.Itoa(p2), "admin", ownerPass)
	if err != nil {
		t.Fatal(err)
	}
	c2.Close()
}

func TestPortRules(t *testing.T) {
	for _, c := range []struct {
		cfg  netConfig
		want int
	}{
		{netConfig{}, 22},
		{netConfig{SSHPort: 2222}, 2222},
		{netConfig{SSHPort: 8080}, 22},               // the panel's default port
		{netConfig{SSHPort: 80}, 22},                 // the panel's
		{netConfig{SSHPort: 443}, 22},                // the panel's https
		{netConfig{SSHPort: 4028}, 22},               // the mining program's
		{netConfig{SSHPort: 100}, 22},                // under 1024
		{netConfig{SSHPort: 70000}, 22},              // not a port
		{netConfig{Port: 9000, SSHPort: 9000}, 9000}, // the panel can't have SSH's: it falls back to 8080
		{netConfig{Port: 9000, SSHPort: 9001}, 9001},
	} {
		if got := sshPortOf(c.cfg); got != c.want {
			t.Errorf("%+v: SSH on %d, want %d", c.cfg, got, c.want)
		}
	}
	if sshcore.WebPort(9000, 9000) != 8080 || sshcore.WebPort(9000, 0) != 9000 || sshcore.WebPort(4028, 0) != 8080 {
		t.Error("WebPort doesn't follow the panel's rule")
	}
	if sshcore.PortBad(2222, 2222) == "" || sshcore.PortBad(22, 8080) != "" {
		t.Error("PortBad")
	}
}

func TestPreAuthPerAddressAndConnectionLimit(t *testing.T) {
	ts := newTestSrv(t, ownerPass, false)
	var held []net.Conn
	defer func() {
		for _, c := range held {
			c.Close()
		}
	}()
	for i := 0; i < sshcore.PreAuthPerIP; i++ { // idle, never signing in
		c, err := net.Dial("tcp", ts.addr)
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, c)
		c.SetReadDeadline(time.Now().Add(3 * time.Second))
		if _, err := c.Read(make([]byte, 7)); err != nil {
			t.Fatalf("connection %d got no SSH greeting: %v", i, err)
		}
	}
	c, err := net.Dial("tcp", ts.addr)
	if err != nil {
		t.Fatal(err)
	}
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if b, err := io.ReadAll(c); err != nil || len(b) != 0 {
		t.Fatalf("a third idle connection from one address was served: %q %v", b, err)
	}
	c.Close()
	for _, c := range held {
		c.Close()
	}
	held = nil
	time.Sleep(200 * time.Millisecond)
	// every connection slot taken: closed at once
	for i := 0; i < sshcore.MaxConns; i++ {
		ts.conns <- struct{}{}
	}
	c, err = net.Dial("tcp", ts.addr)
	if err != nil {
		t.Fatal(err)
	}
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if b, _ := io.ReadAll(c); len(b) != 0 {
		t.Fatalf("served past %d connections: %q", sshcore.MaxConns, b)
	}
	c.Close()
	for i := 0; i < sshcore.MaxConns; i++ {
		<-ts.conns
	}
	c2, err := dial(ts.addr, "admin", ownerPass)
	if err != nil {
		t.Fatal(err)
	}
	c2.Close()
}

// A client that vanishes without a word is dropped by the keepalive.
func TestKeepaliveDropsVanishedClient(t *testing.T) {
	ts := newTestSrv(t, ownerPass, false)
	ts.keepEvery, ts.keepWait = 200*time.Millisecond, 400*time.Millisecond
	relay, _ := net.Listen("tcp", "127.0.0.1:0")
	defer relay.Close()
	var frozen atomic.Bool
	go func() {
		c, err := relay.Accept()
		if err != nil {
			return
		}
		s, err := net.Dial("tcp", ts.addr)
		if err != nil {
			c.Close()
			return
		}
		pipe := func(dst, src net.Conn) {
			buf := make([]byte, 4096)
			for {
				n, err := src.Read(buf)
				if err != nil {
					return
				}
				if !frozen.Load() { // gone: nothing passes either way (the sockets stay open)
					dst.Write(buf[:n])
				}
			}
		}
		go pipe(s, c)
		go pipe(c, s)
	}()
	c, err := dial(relay.Addr().String(), "admin", ownerPass)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	frozen.Store(true)
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
		ts.liveMu.Lock()
		n := len(ts.live)
		ts.liveMu.Unlock()
		if n == 0 {
			return
		}
	}
	t.Fatal("a vanished client's connection was kept")
}

func TestHostKeyMadeOnceAndKept(t *testing.T) {
	ts := newTestSrv(t, ownerPass, false)
	p := filepath.Join(ts.dir, sshcore.HostKeyName)
	fi, err := os.Stat(p)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("host key %v %v", fi, err)
	}
	fp := ts.state(t).Fingerprint
	ts.stop()
	s2 := newServer(ts.authPath, ts.netPath, ts.statePath, ts.eventsPath)
	s2.bindHost, s2.entropy = "127.0.0.1", func() {}
	s2.apply()
	defer s2.stop()
	if got := s2.state(t).Fingerprint; got != fp || sshcore.Fingerprint(p) != fp {
		t.Fatalf("host key changed on restart: %s -> %s", fp, got)
	}
}

func TestFactoryLoginWhenPanelNeverMadeOne(t *testing.T) {
	dir := t.TempDir()
	s := newServer(filepath.Join(dir, "auth.json"), filepath.Join(dir, "net.json"), filepath.Join(dir, "state.json"),
		filepath.Join(dir, "events"))
	s.bindHost, s.entropy = "127.0.0.1", func() {}
	writeNet(t, s.netPath, netConfig{SSHPort: freePort(t)})
	s.createAfter = time.Hour
	s.apply()
	defer s.stop()
	if _, err := os.Stat(s.authPath); err == nil {
		t.Fatal("made a login file while the panel may still make it")
	}
	s.started = time.Now().Add(-2 * time.Hour)
	s.apply()
	a, _, err := authfile.Read(s.authPath)
	if err != nil || !a.MustChange || a.Username != "admin" || !authfile.Check(&a, "admin") {
		t.Fatalf("factory login %+v %v", a, err)
	}
}

func TestDamagedLoginFileUsesBackup(t *testing.T) {
	ts := newTestSrv(t, ownerPass, false)
	b, _ := os.ReadFile(ts.authPath)
	os.WriteFile(ts.authPath+".bak", b, 0o600)
	os.WriteFile(ts.authPath, []byte(`{"username":"admin","hash`), 0o600) // cut short by a power loss
	c, err := dial(ts.addr, "admin", ownerPass)
	if err != nil {
		t.Fatalf("the backup login didn't sign in: %v", err)
	}
	c.Close()
}
