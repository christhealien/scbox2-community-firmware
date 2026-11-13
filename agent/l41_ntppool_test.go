package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// L41: the public NTP pool is only a last resort, on by default, and the owner can switch it off
// (Settings -> Time), saved as the file "ntp-pool" = "off" that /usr/libexec/sclite/ntp reads.
func TestL41NTPPoolSwitch(t *testing.T) {
	*authFile = filepath.Join(t.TempDir(), "auth.json")
	d := t.TempDir()
	defer func(a, b, c, e string) { ntpDHCPPath, ntpLastPath, ntpFailPath, ntpNowPath = a, b, c, e }(ntpDHCPPath, ntpLastPath, ntpFailPath, ntpNowPath)
	ntpDHCPPath, ntpLastPath, ntpFailPath, ntpNowPath = filepath.Join(d, "dhcp"), filepath.Join(d, "last"), filepath.Join(d, "fail"), filepath.Join(d, "now")
	flag := filepath.Join(filepath.Dir(*authFile), "ntp-pool")
	post := func(body string) {
		w := httptest.NewRecorder()
		handleTime(w, httptest.NewRequest(http.MethodPost, "/api/time", strings.NewReader(body)))
		if !strings.Contains(w.Body.String(), `"ok":true`) {
			t.Fatalf("POST %s: %s", body, w.Body.String())
		}
	}
	if ntpView()["pool"] != true {
		t.Fatal("the pool isn't on by default")
	}
	post(`{"ntp_pool":false}`)
	if b, _ := os.ReadFile(flag); strings.TrimSpace(string(b)) != "off" {
		t.Fatalf("switching the pool off wrote %q", b)
	}
	if ntpView()["pool"] != false {
		t.Fatal("the view doesn't show the pool as off")
	}
	post(`{"ntp_pool":true}`)
	if _, err := os.Stat(flag); err == nil {
		t.Fatal("switching the pool on left the flag")
	}
	if ntpView()["pool"] != true {
		t.Fatal("the view doesn't show the pool as on")
	}
}
