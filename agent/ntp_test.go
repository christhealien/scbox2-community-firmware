package main

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestNTPSettings(t *testing.T) {
	*authFile = filepath.Join(t.TempDir(), "auth.json")
	d := t.TempDir()
	defer func(a, b, c, e string) { ntpDHCPPath, ntpLastPath, ntpFailPath, ntpNowPath = a, b, c, e }(ntpDHCPPath, ntpLastPath, ntpFailPath, ntpNowPath)
	ntpDHCPPath, ntpLastPath, ntpFailPath, ntpNowPath = filepath.Join(d, "dhcp"), filepath.Join(d, "last"), filepath.Join(d, "fail"), filepath.Join(d, "now")
	for _, bad := range []string{"a b c d", "bad name!", "-x", "x;rm"} {
		if setNTPServers(bad) == "" {
			t.Errorf("%q should be refused", bad)
		}
	}
	if e := setNTPServers("192.168.1.2, ntp.lan"); e != "" {
		t.Fatal(e)
	}
	if b, _ := os.ReadFile(ntpConfPath()); string(b) != "192.168.1.2 ntp.lan\n" {
		t.Fatalf("saved %q", b)
	}
	if _, err := os.Stat(ntpNowPath); err != nil {
		t.Fatal("saving should ask for a sync now")
	}
	now := time.Now().Unix()
	os.WriteFile(ntpDHCPPath, []byte("10.0.0.1 10.0.0.2\n"), 0o644)
	os.WriteFile(ntpLastPath, []byte(strconv.FormatInt(now-60, 10)+" 10.0.0.1\n"), 0o644)
	os.WriteFile(ntpFailPath, []byte(strconv.FormatInt(now-120, 10)+" (no usable server)\n"), 0o644)
	v := ntpView()
	if v["servers"] != "192.168.1.2 ntp.lan" || len(v["dhcp"].([]string)) != 2 || v["last"] != now-60 {
		t.Fatalf("view %v", v)
	}
	if _, ok := v["fail"]; ok {
		t.Fatal("a failure older than the last sync isn't shown")
	}
	if setNTPServers("") != "" || len(ntpFields(ntpConfPath())) != 0 {
		t.Fatal("empty clears the panel servers")
	}
}
