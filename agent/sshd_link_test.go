package main

// The SC Box II's SSH is scbox2-sshd (cmd/scbox2-sshd, its own tests there); these test the panel's side
// (sshd_link.go): the shared login file, the settings signal, the status, the events. Both builds compile them;
// sshViaDaemon is switched on by each test.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"sclite-agent/internal/authfile"
	"sclite-agent/internal/sshcore"
)

// sshdPanel: a panel with a fresh login (password pw, already changed from the factory one unless pw is
// "admin") in a temp dir, SSH served by "scbox2-sshd".
func sshdPanel(t *testing.T, pw string) string {
	t.Helper()
	dir := t.TempDir()
	oa, on, osp, oev, ours, ov := *authFile, *netPath, sshdStatePath, sshdEventsPath, sshdIsOurs, sshViaDaemon
	t.Cleanup(func() {
		*authFile, *netPath, sshdStatePath, sshdEventsPath, sshdIsOurs, sshViaDaemon = oa, on, osp, oev, ours, ov
		authSeen(nil)
	})
	*authFile, *netPath = filepath.Join(dir, "auth.json"), filepath.Join(dir, "net.json")
	sshdStatePath, sshdEventsPath = filepath.Join(dir, "sshd.json"), filepath.Join(dir, "sshd.events")
	sshViaDaemon = true
	authMu.Lock()
	auth = authStore{}
	authMu.Unlock()
	if err := loadAuth(); err != nil {
		t.Fatal(err)
	}
	if pw != defaultPass {
		authMu.Lock()
		setPassword(&auth, pw)
		auth.MustChange = false
		authMu.Unlock()
		if err := saveAuth(); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// The two sides read each other's login file: the panel's (saveAuth) signs in over SSH, and a password the SSH
// server saved (authfile, both hash formats) signs in to the panel.
func TestSSHDaemonLoginFileShared(t *testing.T) {
	sshdPanel(t, "panel-pass-1")
	a, _, err := authfile.Read(*authFile)
	if err != nil || !authfile.Check(&a, "panel-pass-1") || authfile.Check(&a, "admin") || a.MustChange {
		t.Fatalf("the SSH server can't use the panel's login file: %+v %v", a, err)
	}
	for _, legacy := range []bool{false, true} {
		var nh authfile.Store
		authfile.SetPassword(&nh, "ssh-chosen-pass", legacy)
		nh.Secret = "bmV3LXNpZ25pbmcta2V5LW5ldy1zaWduaW5nLWtleQ=="
		if _, err := authfile.Update(*authFile, "auth.json.sshd.tmp", func(o map[string]json.RawMessage) error {
			authfile.SetFields(o, &nh)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		authMu.Lock()
		auth = authStore{}
		authMu.Unlock()
		if err := loadAuth(); err != nil {
			t.Fatal(err)
		}
		if !checkPassword("ssh-chosen-pass") || checkPassword("panel-pass-1") {
			t.Fatalf("legacy=%v: the panel doesn't take the password the SSH server saved", legacy)
		}
		authUpgradeWG.Wait()
	}
}

// A password chosen over SSH (written by scbox2-sshd) is the panel's at once: its sessions made with the old
// signing key end, and a damaged file is not taken.
func TestSSHDaemonPanelFollowsLoginFile(t *testing.T) {
	sshdPanel(t, defaultPass)
	cookie := issueCookie(defaultUser)
	if !validCookie(cookie) {
		t.Fatal("setup: cookie")
	}
	var nh authfile.Store
	authfile.SetPassword(&nh, "chosen-over-ssh", false)
	nh.Secret = "bmV3LXNpZ25pbmcta2V5LW5ldy1zaWduaW5nLWtleQ=="
	if _, err := authfile.Update(*authFile, "auth.json.sshd.tmp", func(o map[string]json.RawMessage) error {
		authfile.SetFields(o, &nh)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	r.AddCookie(&http.Cookie{Name: "sid", Value: cookie})
	if isAuthed(r) { // isAuthed re-reads the changed file
		t.Fatal("a session made with the factory password survived the password change over SSH")
	}
	authMu.RLock()
	mc := auth.MustChange
	authMu.RUnlock()
	if mc || !checkPassword("chosen-over-ssh") || checkPassword(defaultPass) {
		t.Fatal("the panel didn't take the password chosen over SSH")
	}
	// damaged on disk: the panel keeps what it has
	os.WriteFile(*authFile, []byte(`{"username":"adm`), 0o600)
	authFollowFile(true)
	if !checkPassword("chosen-over-ssh") {
		t.Fatal("a damaged login file replaced the panel's login")
	}
	// the panel's own save isn't "a change on disk" (nothing reloaded over it)
	authMu.Lock()
	auth.Acknowledged = "2026-11-13"
	authMu.Unlock()
	if err := saveAuth(); err != nil {
		t.Fatal(err)
	}
	authFollowFile(true)
	authMu.RLock()
	ack := auth.Acknowledged
	authMu.RUnlock()
	if ack != "2026-11-13" {
		t.Fatal("the panel's own save was reloaded over itself")
	}
}

// fakeSSHD: answers SIGHUP as scbox2-sshd does (re-reads net.json, writes its state). Returns the count of
// signals received.
func fakeSSHD(t *testing.T) *atomic.Int32 {
	t.Helper()
	hup := make(chan os.Signal, 8)
	signal.Notify(hup, syscall.SIGHUP)
	stop := make(chan struct{})
	t.Cleanup(func() { signal.Stop(hup); close(stop) })
	pid := os.Getpid()
	sshdIsOurs = func(p int) bool { return p == pid }
	n := new(atomic.Int32)
	write := func() {
		c := currentNetConfig()
		st := sshdStateInfo{Pid: pid, Enabled: !c.SSHOff, Port: sshcore.Port(c.SSHPort, sshcore.WebPort(c.Port, c.SSHPort)),
			Listening: !c.SSHOff, Fingerprint: "SHA256:test-fingerprint", Applied: time.Now().UnixNano()}
		b, _ := json.Marshal(st)
		os.WriteFile(sshdStatePath, b, 0o644)
	}
	write()
	go func() {
		for {
			select {
			case <-hup:
				n.Add(1)
				time.Sleep(100 * time.Millisecond) // (a moment to apply)
				write()
			case <-stop:
				return
			}
		}
	}()
	return n
}

func TestSSHDaemonSettingsAndStatus(t *testing.T) {
	sshdPanel(t, "panel-pass-1")
	hups := fakeSSHD(t)
	out := l02Post(handleSSH, "/api/ssh", map[string]any{"enabled": true, "port": 2222})
	if out["ok"] != true || out["listening"] != true || out["port"] != float64(2222) ||
		out["fingerprint"] != "SHA256:test-fingerprint" || out["available"] != true || out["err"] != "" {
		t.Fatalf("status after saving: %v", out)
	}
	if c := currentNetConfig(); c.SSHPort != 2222 || c.SSHOff {
		t.Fatalf("not saved: %+v", c)
	}
	sshMu.Lock()
	l := sshListener
	sshMu.Unlock()
	if l != nil {
		t.Fatal("the panel opened an SSH listener itself")
	}
	if hups.Load() == 0 {
		t.Fatal("scbox2-sshd wasn't told to apply the settings")
	}
	out = l02Post(handleSSH, "/api/ssh", map[string]any{"enabled": false})
	if out["enabled"] != false || out["listening"] != false {
		t.Fatalf("status after off: %v", out)
	}
	// the panel's port rules stay
	if out := l02Post(handleSSH, "/api/ssh", map[string]any{"port": 8080}); out["ok"] != false {
		t.Fatalf("the panel's port was taken for SSH: %v", out)
	}
	// a password change, access from outside switched off: scbox2-sshd is told (it closes what may not stay)
	n := hups.Load()
	sshDropAll()
	dropOutsideConns()
	for end := time.Now().Add(3 * time.Second); hups.Load() <= n && time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
	}
	if hups.Load() <= n {
		t.Fatal("scbox2-sshd wasn't told about a password change / access switched off")
	}
	// not running: said so, never "listening"
	sshdIsOurs = func(int) bool { return false }
	l02Post(handleSSH, "/api/ssh", map[string]any{"enabled": true})
	st := sshStatus()
	if st["listening"] != false || !strings.Contains(st["err"].(string), "isn't running") {
		t.Fatalf("status with no SSH server: %v", st)
	}
}

// The panel never makes the host key itself when scbox2-sshd keeps it (two writers would make two keys).
func TestSSHDaemonPanelMakesNoHostKey(t *testing.T) {
	dir := sshdPanel(t, "panel-pass-1")
	sshdIsOurs = func(int) bool { return false }
	if fp := sshFingerprint(); fp != "" {
		t.Fatalf("fingerprint %q with no key", fp)
	}
	if _, err := os.Stat(filepath.Join(dir, sshcore.HostKeyName)); err == nil {
		t.Fatal("the panel made a host key")
	}
	sshApply()
	if _, err := os.Stat(filepath.Join(dir, sshcore.HostKeyName)); err == nil {
		t.Fatal("the panel made a host key")
	}
}

func TestSSHDaemonEvents(t *testing.T) {
	sshdPanel(t, "panel-pass-1")
	ch := activity.sub()
	defer activity.unsub(ch)
	lines := `{"ev":"signin","user":"admin","ip":"192.168.1.20","t":1}` + "\n" +
		`{"ev":"block","msg":"192.168.1.21 blocked for 10 min after 5 wrong passwords (SSH)","t":2}` + "\n" +
		`{"ev":"pwdrop","n":2,"t":3}` + "\n" + `{"ev":"signin","user":"half`
	os.WriteFile(sshdEventsPath, []byte(lines), 0o600)
	off := sshdReadEvents(0)
	if want := int64(strings.LastIndexByte(lines, '\n') + 1); off != want {
		t.Fatalf("offset %d, want %d (a half-written line waits)", off, want)
	}
	var got []string
	for len(got) < 3 {
		select {
		case l := <-ch:
			got = append(got, l)
		case <-time.After(2 * time.Second):
			t.Fatalf("activity lines: %q", got)
		}
	}
	all := strings.Join(got, "\n")
	for _, w := range []string{"SSH sign-in: admin from 192.168.1.20", "🔒 192.168.1.21 blocked", "2 SSH connection(s) closed"} {
		if !strings.Contains(all, w) {
			t.Fatalf("missing %q in %q", w, all)
		}
	}
	if sshdReadEvents(off) != off {
		t.Fatal("read the same lines twice")
	}
	os.WriteFile(sshdEventsPath, []byte(`{"ev":"pwchange"}`+"\n"), 0o600) // started over (shorter)
	if sshdReadEvents(off) != int64(len(`{"ev":"pwchange"}`)+1) {
		t.Fatal("a file that started over wasn't read from the top")
	}
}
