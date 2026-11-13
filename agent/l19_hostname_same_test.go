package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// F: the Settings page sends the hostname field with every "Save network" (a port or MAC change too), and each
// one runs setHostname: a read-write remount of the system partition, a rewrite of /etc/hostname, a
// read-only remount (which can land in the middle of an update install that has / writable) and a
// "hostname set to" line in the activity log, for a name that didn't change.
func TestL19SameHostnameNotRewritten(t *testing.T) {
	l03Env(t, false)
	l19NoShell(t) // were setHostname to run, its `sh` isn't found: the reply says "may not persist"
	cur, _ := os.Hostname()
	f := filepath.Join(t.TempDir(), "hostname")
	os.WriteFile(f, []byte(cur+"\n"), 0o644)
	old := hostnameFile
	hostnameFile = f
	t.Cleanup(func() { hostnameFile = old })
	rr := l03Req(l03Mux(), "POST", "/api/net", `{"hostname":"`+cur+`"}`, issueCookie("zq7owner"), nil)
	if strings.Contains(rr.Body.String(), "persist") || strings.Contains(rr.Body.String(), "hostname updated") {
		t.Errorf("an unchanged hostname (%s) is set again: %s", cur, rr.Body.String())
	}
}
