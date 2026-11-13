package main

import (
	"bufio"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The test binary runs the real program (main) when this is set: the process tests below start it as the
// boot script does and talk to it with signals.
func TestMain(m *testing.M) {
	if os.Getenv("SCBOX2_SSHD_TEST_MAIN") == "1" {
		main()
		return
	}
	os.Exit(m.Run())
}

func startProc(t *testing.T, dir string, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], append([]string{"-authfile", filepath.Join(dir, "auth.json"), "-netcfg",
		filepath.Join(dir, "net.json"), "-state", filepath.Join(dir, "state.json"), "-events",
		filepath.Join(dir, "events")}, args...)...)
	cmd.Env = append(os.Environ(), "SCBOX2_SSHD_TEST_MAIN=1")
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	return cmd
}

func waitState(t *testing.T, s *server, ok func(state) bool) state {
	t.Helper()
	var st state
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if _, err := os.Stat(s.statePath); err == nil {
			st = s.state(t)
			if ok(st) {
				return st
			}
		}
	}
	t.Fatalf("state never as expected: %+v", st)
	return st
}

// banner: the SSH version line a port answers with.
func banner(addr string) string {
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return ""
	}
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	l, _ := bufio.NewReader(c).ReadString('\n')
	return strings.TrimSpace(l)
}

// The program as the boot script starts it: listens where net.json says, follows a change at once on SIGHUP
// (the panel's signal; the periodic re-read is an hour here), and stops cleanly on SIGTERM.
func TestProcessFollowsSIGHUP(t *testing.T) {
	dir := t.TempDir()
	s := newServer(filepath.Join(dir, "auth.json"), filepath.Join(dir, "net.json"), filepath.Join(dir, "state.json"),
		filepath.Join(dir, "events"))
	writeLogin(t, s.authPath, "admin", ownerPass, false)
	p1 := freePort(t)
	writeNet(t, s.netPath, netConfig{SSHPort: p1})
	cmd := startProc(t, dir, "-poll", "1h")
	st := waitState(t, s, func(st state) bool { return st.Listening })
	if st.Pid != cmd.Process.Pid || st.Port != p1 {
		t.Fatalf("state %+v", st)
	}
	if b := banner("127.0.0.1:" + strconv.Itoa(p1)); b != "SSH-2.0-scbox2" {
		t.Fatalf("banner %q", b)
	}
	p2 := freePort(t)
	writeNet(t, s.netPath, netConfig{SSHPort: p2})
	t0 := time.Now().UnixNano()
	cmd.Process.Signal(syscall.SIGHUP)
	waitState(t, s, func(st state) bool { return st.Applied >= t0 && st.Port == p2 && st.Listening })
	if b := banner("127.0.0.1:" + strconv.Itoa(p2)); b != "SSH-2.0-scbox2" {
		t.Fatalf("banner after the move %q", b)
	}
	if b := banner("127.0.0.1:" + strconv.Itoa(p1)); b != "" {
		t.Fatalf("the old port still answers %q", b)
	}
	writeNet(t, s.netPath, netConfig{SSHPort: p2, SSHOff: true})
	cmd.Process.Signal(syscall.SIGHUP)
	waitState(t, s, func(st state) bool { return !st.Enabled && !st.Listening })
	cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("didn't stop on SIGTERM")
	}
	if _, err := os.Stat(s.statePath); err == nil {
		t.Fatal("state file left behind after a clean stop (the panel would think it runs)")
	}
}
