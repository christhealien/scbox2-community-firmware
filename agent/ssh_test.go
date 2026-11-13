package main

import (
	"bytes"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// sshTestServer starts the agent's SSH server on a free port with a fresh (factory) account.
func sshTestServer(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	*authFile = filepath.Join(dir, "auth.json")
	*netPath = filepath.Join(dir, "net.json")
	sshKeyOnce, sshSigner, sshKeyErr = sync.Once{}, nil, nil
	sshFailMu.Lock()
	sshFailBy = map[string]*sshFails{}
	sshFailMu.Unlock()
	authMu.Lock()
	auth = authStore{}
	authMu.Unlock()
	if err := loadAuth(); err != nil {
		t.Fatal(err)
	}
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	if err := saveNetConfig(netConfig{SSHPort: port}); err != nil {
		t.Fatal(err)
	}
	sshApply()
	t.Cleanup(func() {
		sshMu.Lock()
		if sshListener != nil {
			sshListener.Close()
			sshListener, sshListenOn = nil, ""
		}
		sshMu.Unlock()
	})
	sshMu.Lock()
	on, e := sshListenOn, sshLastErr
	sshMu.Unlock()
	if on == "" {
		t.Fatalf("ssh not listening: %s", e)
	}
	return "127.0.0.1:" + strconv.Itoa(port)
}

func dialSSH(addr, user, pw string) (*ssh.Client, error) {
	return ssh.Dial("tcp", addr, &ssh.ClientConfig{User: user, Auth: []ssh.AuthMethod{ssh.Password(pw)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 10 * time.Second})
}

func runCmd(t *testing.T, c *ssh.Client, cmd string) (string, int) {
	t.Helper()
	s, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	out := &lockedBuf{}
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

// shellSession opens an interactive shell with a pty and returns its input and collected output.
type shellIO struct {
	in  io.WriteCloser
	out *safeBuf
	s   *ssh.Session
}

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuf) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

type safeBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
	ch chan struct{}
}

func (b *safeBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	n, _ := b.b.Write(p)
	b.mu.Unlock()
	select {
	case b.ch <- struct{}{}:
	default:
	}
	return n, nil
}

func (b *safeBuf) String() string { b.mu.Lock(); defer b.mu.Unlock(); return b.b.String() }

func (sh *shellIO) waitFor(t *testing.T, want string) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for !strings.Contains(sh.out.String(), want) {
		select {
		case <-sh.out.ch:
		case <-time.After(50 * time.Millisecond):
		case <-deadline:
			t.Fatalf("timed out waiting for %q; got:\n%s", want, sh.out.String())
		}
	}
}

func openShell(t *testing.T, c *ssh.Client) *shellIO {
	t.Helper()
	s, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RequestPty("xterm", 40, 120, ssh.TerminalModes{}); err != nil {
		t.Fatal(err)
	}
	sh := &shellIO{s: s, out: &safeBuf{ch: make(chan struct{}, 1)}}
	s.Stdout, s.Stderr = sh.out, sh.out
	sh.in, _ = s.StdinPipe()
	if err := s.Shell(); err != nil {
		t.Fatal(err)
	}
	return sh
}

func TestSSHFirstSignInForcesChangeAndFollowsPanel(t *testing.T) {
	addr := sshTestServer(t)

	// wrong password and wrong username are refused
	if _, err := dialSSH(addr, "admin", "nope"); err == nil {
		t.Fatal("wrong password accepted")
	}
	if _, err := dialSSH(addr, "root", "admin"); err == nil {
		t.Fatal("wrong username accepted")
	}

	// factory admin/admin signs in, but a command is refused until the password is changed
	c, err := dialSSH(addr, "admin", "admin")
	if err != nil {
		t.Fatalf("factory sign-in: %v", err)
	}
	out, code := runCmd(t, c, "echo should-not-run")
	if code == 0 || strings.Contains(out, "should-not-run") || !strings.Contains(out, "factory password") {
		t.Fatalf("command ran before the password change: code %d, %q", code, out)
	}

	// interactive sign-in: forced change (too short, mismatch, then good), then a shell
	sh := openShell(t, c)
	sh.waitFor(t, "New password: ")
	io.WriteString(sh.in, "short\r")
	sh.waitFor(t, "at least 8 characters")
	io.WriteString(sh.in, "first-pass-1\r")
	sh.waitFor(t, "Type it again: ")
	io.WriteString(sh.in, "different-2\r")
	sh.waitFor(t, "don't match")
	io.WriteString(sh.in, "first-pass-1\r")
	sh.waitFor(t, "Type it again: ")
	io.WriteString(sh.in, "first-pass-1\r")
	sh.waitFor(t, "Password changed")
	if strings.Contains(sh.out.String(), "first-pass-1") {
		t.Fatal("the password was echoed to the terminal")
	}
	io.WriteString(sh.in, "echo shell-ok-$((6*7)); exit 3\r")
	sh.waitFor(t, "shell-ok-42")
	err = sh.s.Wait()
	if ee, ok := err.(*ssh.ExitError); !ok || ee.ExitStatus() != 3 {
		t.Fatalf("shell exit: %v", err)
	}
	c.Close()

	// the panel account changed with it
	authMu.RLock()
	mc := auth.MustChange
	authMu.RUnlock()
	if mc || !checkPassword("first-pass-1") || checkPassword("admin") {
		t.Fatal("the panel password didn't follow the SSH change")
	}
	if _, err := dialSSH(addr, "admin", "admin"); err == nil {
		t.Fatal("factory password still works")
	}

	// a password changed in the panel works for SSH straight away (same account)
	authMu.Lock()
	setPassword(&auth, "panel-pass-2")
	auth.Username = "christhealien"
	authMu.Unlock()
	if _, err := dialSSH(addr, "christhealien", "first-pass-1"); err == nil {
		t.Fatal("old password still works after a panel change")
	}
	c, err = dialSSH(addr, "christhealien", "panel-pass-2")
	if err != nil {
		t.Fatalf("panel password over SSH: %v", err)
	}
	defer c.Close()

	// commands: output, exit status, stdin
	if out, code := runCmd(t, c, "echo hi; echo err >&2; exit 5"); code != 5 || !strings.Contains(out, "hi") || !strings.Contains(out, "err") {
		t.Fatalf("exec: code %d %q", code, out)
	}
	s, _ := c.NewSession()
	s.Stdin = strings.NewReader("piped-in\n")
	b, err := s.Output("cat")
	if err != nil || string(b) != "piped-in\n" {
		t.Fatalf("stdin: %v %q", err, b)
	}

	// sftp: write, read back, list
	sc, err := sftp.NewClient(c)
	if err != nil {
		t.Fatalf("sftp: %v", err)
	}
	p := filepath.Join(t.TempDir(), "hello.txt")
	f, err := sc.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	f.Write([]byte("sftp works"))
	f.Close()
	if got, _ := os.ReadFile(p); string(got) != "sftp works" {
		t.Fatalf("sftp write: %q", got)
	}
	if fis, err := sc.ReadDir(filepath.Dir(p)); err != nil || len(fis) != 1 {
		t.Fatalf("sftp list: %v %d", err, len(fis))
	}
	sc.Close()

	// port forwarding is refused
	if _, err := c.Dial("tcp", "127.0.0.1:1"); err == nil {
		t.Fatal("port forwarding allowed")
	}
}

func TestSSHBlocksAfterWrongPasswords(t *testing.T) {
	addr := sshTestServer(t)
	for i := 0; i < sshFailLimit; i++ {
		dialSSH(addr, "admin", "wrong-"+strconv.Itoa(i))
	}
	if _, err := dialSSH(addr, "admin", "admin"); err == nil {
		t.Fatal("still accepted after too many wrong passwords")
	}
	sshFailMu.Lock()
	sshFailBy = map[string]*sshFails{}
	sshFailMu.Unlock()
	if c, err := dialSSH(addr, "admin", "admin"); err != nil {
		t.Fatalf("not accepted after the block was lifted: %v", err)
	} else {
		c.Close()
	}
}

func TestSSHOffInSettings(t *testing.T) {
	addr := sshTestServer(t)
	c := currentNetConfig()
	c.SSHOff = true
	saveNetConfig(c)
	sshApply()
	if conn, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		conn.Close()
		t.Fatal("still listening after SSH was switched off")
	}
}
