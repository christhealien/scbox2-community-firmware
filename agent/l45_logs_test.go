package main

// L45-logs: the Log tab's exports and live views. The tests run the real private export
// (handleLogExport, the l05Env backend mock) and grep the unzipped files, and stream the real
// /api/log/stream handler over HTTP.

import (
	"bufio"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// An address the miner gets by DHCPv6 or SLAAC is written compressed ("::"). With 1-3 groups after the
// "::" the export hid only the part before it: "2a02:8071:5a1:5600::1a2b" -> "x:x::x1a2b".
func TestL45IPv6CompressedTail(t *testing.T) {
	_, _ = l05Env(t)
	os.WriteFile(*logPath, []byte(
		"Nov  9 10:00:01 rig daemon.info dhcpcd[812]: eth0: adding address 2001:db8:85a3::8a2e:370:7334/64\n"+
			"Nov  9 10:00:02 rig daemon.info dhcpcd[812]: eth0: leased 2a02:8071:5a1:5600::1a2b for 7200 seconds\n"+
			"Nov  9 10:00:03 rig daemon.info dhcpcd[812]: eth0: adding route to fd12:3456:789a:1::/64 via fe80::1%eth0\n"+
			"Nov  9 10:00:04 rig user.notice sclite: fan check passed at 10:00:04, api ::1 port 4028\n"), 0o644)
	out := l05Export(t)
	sys := out[strings.Index(out, "=== miner-logs-"):]
	for _, bad := range []string{"8a2e:370:7334", "8a2e", "7334", "1a2b", "5600", "789a", "fd12"} {
		if strings.Contains(sys, bad) {
			i := strings.Index(sys, bad)
			t.Errorf("%q (part of an IPv6 address) in the private export: …%s…", bad, sys[max(0, i-60):min(len(sys), i+40)])
		}
	}
	if !strings.Contains(sys, "fan check passed at 10:00:04") || !strings.Contains(sys, "::1 port 4028") {
		t.Fatalf("too much taken out:\n%s", sys)
	}
}

// The panel's login name is half of the sign-in. Every SSH sign-in puts it in the panel events
// ("🔑 SSH sign-in: <login> from <ip>"), which go into the private export.
func TestL45LoginNameInPanelEvents(t *testing.T) {
	_, _ = l05Env(t)
	authMu.Lock()
	old := auth
	auth.Username = "joe.ops"
	authMu.Unlock()
	t.Cleanup(func() { authMu.Lock(); auth = old; authMu.Unlock() })
	activity.emit("🔑 SSH sign-in: joe.ops from 192.168.1.44")
	out := l05Export(t)
	if strings.Contains(out, "joe.ops") {
		i := strings.Index(out, "joe.ops")
		t.Fatalf("the panel's login name in the private export: …%s…", out[max(0, i-80):min(len(out), i+40)])
	}
	if !strings.Contains(out, "SSH sign-in") {
		t.Fatalf("too much taken out:\n%s", out)
	}
}

// busybox syslogd (S01logging: "syslogd -n", so -s 200 KB -b 1) rotates /var/log/messages: it renames it to
// messages.0 and starts a new file. The System view kept reading the renamed file and showed nothing new
// until the page was reloaded.
func TestL45SystemStreamAfterRotation(t *testing.T) {
	d := t.TempDir()
	old := *logPath
	*logPath = d + "/messages"
	t.Cleanup(func() { *logPath = old })
	os.WriteFile(*logPath, []byte("Nov  9 10:00:01 rig user.notice sclite: before rotation\n"), 0o644)
	srv := httptest.NewServer(http.HandlerFunc(handleLog))
	defer srv.Close()
	resp, err := srv.Client().Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	lines := make(chan string, 64)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if l, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
				lines <- l
			}
		}
		close(lines)
	}()
	want := func(s string) bool {
		dl := time.After(4 * time.Second)
		for {
			select {
			case l, ok := <-lines:
				if !ok {
					return false
				}
				if strings.Contains(l, s) {
					return true
				}
			case <-dl:
				return false
			}
		}
	}
	if !want("before rotation") {
		t.Fatal("setup: the history line didn't come")
	}
	f, _ := os.OpenFile(*logPath, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("Nov  9 10:00:02 rig user.notice sclite: still the first file\n")
	f.Close()
	if !want("still the first file") {
		t.Fatal("setup: a new line in the same file didn't come")
	}
	os.Rename(*logPath, *logPath+".0") // what syslogd does at 200 KB
	os.WriteFile(*logPath, []byte("Nov  9 10:05:00 rig kern.warn kernel: after rotation\n"), 0o644)
	if !want("after rotation") {
		t.Fatal("the System view stopped at the log's rotation: a line written to the new /var/log/messages never came")
	}
}
