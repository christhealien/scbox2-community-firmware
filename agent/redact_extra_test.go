package main

import (
	"strings"
	"testing"
)

func TestRedactExtra(t *testing.T) {
	in := "[2026-11-04 19:03:37] 68.8.235 23334: No route to host\nconnect stratum+tcp://bob:secretpw@pool.example.com:3333\nfe80::1a2b:3c4d:5e6f:7a8b%eth0 and 2001:db8:85a3:0:0:8a2e:370:7334\n12:34:56 time ok"
	out := redactLog(in, nil)
	t.Log(out)
	for _, bad := range []string{"68.8.235", "secretpw", "fe80", "2001:db8"} {
		if strings.Contains(out, bad) {
			t.Fatal("left", bad)
		}
	}
	if !strings.Contains(out, "12:34:56") || !strings.Contains(out, "19:03:37") {
		t.Fatal("ate a time")
	}
}
