package main

// L02 (SSH lane) tests. Ports 47040-47059.

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

const l02Pass = "owner-pass-1"

// l02Server: the real SSH server on port, with an account already past the factory password.
func l02Server(t *testing.T, port int) string {
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
	authMu.Lock()
	setPassword(&auth, l02Pass)
	auth.MustChange = false
	authMu.Unlock()
	if err := saveAuth(); err != nil {
		t.Fatal(err)
	}
	if err := saveNetConfig(netConfig{SSHPort: port}); err != nil {
		t.Fatal(err)
	}
	sshApply()
	t.Cleanup(func() {
		c := currentNetConfig()
		c.SSHOff = true
		saveNetConfig(c)
		sshApply()
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

func l02Dial(addr, from, pw string, timeout time.Duration) (*ssh.Client, error) {
	d := net.Dialer{Timeout: timeout}
	if from != "" {
		d.LocalAddr = &net.TCPAddr{IP: net.ParseIP(from)}
	}
	nc, err := d.Dial("tcp", addr)
	if err != nil {
		return nil, err
	}
	nc.SetDeadline(time.Now().Add(timeout))
	authMu.RLock()
	user := auth.Username
	authMu.RUnlock()
	cc, chans, reqs, err := ssh.NewClientConn(nc, addr, &ssh.ClientConfig{User: user, Auth: []ssh.AuthMethod{ssh.Password(pw)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey()})
	if err != nil {
		nc.Close()
		return nil, err
	}
	nc.SetDeadline(time.Time{})
	return ssh.NewClient(cc, chans, reqs), nil
}

// l02Alive: can the connection still run a command?
func l02Alive(c *ssh.Client) bool {
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

func l02Post(h http.HandlerFunc, path string, body any) map[string]any {
	b, _ := json.Marshal(body)
	r := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h(w, r)
	var out map[string]any
	json.Unmarshal(w.Body.Bytes(), &out)
	return out
}

// Settings -> SSH off ("Turn SSH off?") only stopped new connections: a session already open (an intruder's,
// say) kept its root shell.
func TestL02SSHOffClosesOpenSessions(t *testing.T) {
	addr := l02Server(t, 47040)
	c, err := l02Dial(addr, "", l02Pass, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if !l02Alive(c) {
		t.Fatal("session not working before the switch")
	}
	if out := l02Post(handleSSH, "/api/ssh", map[string]any{"enabled": false}); out["ok"] != true {
		t.Fatalf("switch off: %v", out)
	}
	time.Sleep(300 * time.Millisecond)
	if l02Alive(c) {
		t.Fatal("SSH switched off, but the open SSH connection still runs commands")
	}
}

// A new password "signs out every other session (and any stolen cookie)" in the panel, but SSH connections
// signed in with the old password kept working.
func TestL02PasswordChangeClosesSSHSessions(t *testing.T) {
	addr := l02Server(t, 47042)
	c, err := l02Dial(addr, "", l02Pass, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if !l02Alive(c) {
		t.Fatal("session not working before the change")
	}
	out := l02Post(handlePasswd, "/api/passwd", map[string]any{"current": l02Pass, "new": "owner-pass-2"})
	if out["ok"] != true {
		t.Fatalf("password change: %v", out)
	}
	authUpgradeWG.Wait()
	time.Sleep(300 * time.Millisecond)
	if l02Alive(c) {
		t.Fatal("password changed, but the SSH connection signed in with the old one still runs commands")
	}
	if c2, err := l02Dial(addr, "", "owner-pass-2", 10*time.Second); err != nil {
		t.Fatalf("new password over SSH: %v", err)
	} else {
		c2.Close()
	}
}

// Three idle TCP connections from one address (no SSH version string, nothing) hold all three sign-in slots
// for a minute each; the owner's sign-in from another address waits 20 s and is dropped. Repeated, SSH is
// unusable for as long as the other device likes, with no wrong password ever counted.
func TestL02IdleConnectionsDontLockOwnerOut(t *testing.T) {
	addr := l02Server(t, 47044)
	var idle []net.Conn
	defer func() {
		for _, c := range idle {
			c.Close()
		}
	}()
	for i := 0; i < 8; i++ {
		d := net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP("127.0.0.2")}, Timeout: 2 * time.Second}
		c, err := d.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		idle = append(idle, c)
	}
	time.Sleep(300 * time.Millisecond)
	start := time.Now()
	c, err := l02Dial(addr, "127.0.0.3", l02Pass, 8*time.Second)
	if err != nil {
		t.Fatalf("owner locked out by idle connections from another address (after %v): %v", time.Since(start).Round(time.Millisecond), err)
	}
	c.Close()
}

// A web-UI port saved for the next start (not in use yet) could be given to SSH; after the reboot the panel
// finds SSH on it and falls back to :8080, after the owner was told to use the saved port.
func TestL02SSHPortRefusesSavedWebPort(t *testing.T) {
	l02Server(t, 47046)
	c := currentNetConfig()
	c.Port = 47048 // saved in Settings -> Network, applies at the next start
	if err := saveNetConfig(c); err != nil {
		t.Fatal(err)
	}
	out := l02Post(handleSSH, "/api/ssh", map[string]any{"enabled": true, "port": 47048})
	if out["ok"] == true {
		t.Fatalf("SSH was given the panel's saved web-UI port: %v (saved ssh_port %d)", out, currentNetConfig().SSHPort)
	}
}
