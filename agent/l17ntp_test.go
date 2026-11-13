package main

// L17-ntp: time servers and the 1970 clock.

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A typo'd address (192.168.1.256) was saved. The miner's BusyBox 1.21 ntpd quits on it ("ntpd: bad address
// '192.168.1.256'", traced under qemu) and takes the router's server, tried in the same run, with it: with the
// public pool off the miner never gets the time.
func TestL17NTPServerTypoRefused(t *testing.T) {
	old, oldNow := *authFile, ntpNowPath
	*authFile = filepath.Join(t.TempDir(), "auth.json")
	ntpNowPath = filepath.Join(t.TempDir(), "now")
	t.Cleanup(func() { *authFile, ntpNowPath = old, oldNow })
	for _, bad := range []string{"192.168.1.256", "300.1.1.1", "1.2.3", "10.0.0.1.5", "1.2.3.4 999.1.1.1"} {
		if e := setNTPServers(bad); e == "" {
			t.Errorf("%q saved: the miner's ntpd quits on it (and drops the router's server with it)", bad)
		}
	}
	for _, good := range []string{"192.168.1.2", "0.0.0.0 255.255.255.255", "ntp.lan", "time1.google.com 10.0.0.1", "3.pool.ntp.org", ""} {
		if e := setNTPServers(good); e != "" {
			t.Errorf("%q refused: %s", good, e)
		}
	}
}

// Sign in while the clock is still 1970 (no battery clock; NTP not reached yet): the panel then sets the clock
// from the browser at once (syncTime, main.go) or NTP sets it a little later. The session's expiry was
// 1970-01-02, so the jump to today signed the owner out right after signing in.
func TestL17SessionSurvivesClockSet(t *testing.T) {
	redteamSetup(t)
	up := filepath.Join(t.TempDir(), "uptime")
	oldUp := uptimePath
	uptimePath = up
	t.Cleanup(func() { uptimePath, cookieNow = oldUp, time.Now })
	os.WriteFile(up, []byte("95.00 80.00\n"), 0o644)
	cookieNow = func() time.Time { return time.Unix(100, 0) } // 1970, 95 s after boot
	c := issueCookie("owner")
	if !validCookie(c) {
		t.Fatal("not valid while the clock is unset")
	}
	cookieNow = time.Now // NTP / the browser set the clock
	os.WriteFile(up, []byte("130.00 100.00\n"), 0o644)
	if !validCookie(c) {
		t.Fatal("signed out as soon as the clock was set")
	}
	os.WriteFile(up, []byte("86600.00 100.00\n"), 0o644) // a day later
	if validCookie(c) {
		t.Fatal("a session from the unset clock lasted more than a day")
	}
	// a 1970 cookie this panel didn't issue (another boot, another run) isn't taken after the clock is set
	os.WriteFile(up, []byte("130.00 100.00\n"), 0o644)
	secret, _ := base64.StdEncoding.DecodeString(auth.Secret)
	payload := fmt.Sprintf("owner|%d", 100+86400-1)
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(payload))
	old := base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
	if validCookie(old) {
		t.Fatal("a 1970 session from another boot was accepted")
	}
	// ordinary sessions: unchanged
	c2 := issueCookie("owner")
	if !validCookie(c2) {
		t.Fatal("a session issued on a set clock isn't valid")
	}
}
