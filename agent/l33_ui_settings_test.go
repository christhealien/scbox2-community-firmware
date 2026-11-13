package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// l33FakeShell puts a `sh` first on PATH that only records its arguments: setHostname's command never runs.
func l33FakeShell(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	rec := filepath.Join(dir, "called")
	os.WriteFile(filepath.Join(dir, "sh"), []byte("#!/bin/sh\necho \"$@\" >> "+rec+"\n"), 0o755)
	t.Setenv("PATH", dir)
	return rec
}

// F: Settings -> Network "Save network" with a new name and a refused port (80): the reply is only the port
// error, but the name was already changed (setHostname ran before the port was checked).
func TestL33NetRefusedPortLeavesHostnameAlone(t *testing.T) {
	l03Env(t, false)
	rec := l33FakeShell(t)
	f := filepath.Join(t.TempDir(), "hostname")
	os.WriteFile(f, []byte("old-name\n"), 0o644)
	old := hostnameFile
	hostnameFile = f
	t.Cleanup(func() { hostnameFile = old })
	for _, port := range []string{"80", "4028", "70000"} {
		os.Remove(rec)
		rr := l03Req(l03Mux(), "POST", "/api/net", `{"hostname":"l33-new-name","port":`+port+`}`, issueCookie("zq7owner"), nil)
		var out map[string]any
		json.Unmarshal(rr.Body.Bytes(), &out)
		if out["ok"] != false {
			t.Fatalf("port %s accepted: %s", port, rr.Body.String())
		}
		if b, err := os.ReadFile(rec); err == nil {
			t.Errorf("port %s refused (%v), but the hostname command ran anyway: %s", port, out["err"], strings.TrimSpace(string(b)))
		}
	}
}

// F: Settings -> "Hashrate units": /api/settings doesn't say which unit is saved, so after a reload on the
// Settings tab the card shows "Auto-scale" ticked whatever was chosen (Playwright: pw/t_hashunit.js).
func TestL33SettingsReportsHashUnit(t *testing.T) {
	l03Env(t, false)
	shareMu.Lock()
	old := hashUnit
	hashUnit = "ths"
	shareMu.Unlock()
	t.Cleanup(func() { shareMu.Lock(); hashUnit = old; shareMu.Unlock() })
	rr := l03Req(l03Mux(), "GET", "/api/settings", "", issueCookie("zq7owner"), nil)
	var out struct {
		Display map[string]any `json:"display"`
	}
	json.Unmarshal(rr.Body.Bytes(), &out)
	if out.Display["hash_unit"] != "ths" {
		t.Errorf("settings display = %v: the saved hash unit (ths) is missing", out.Display)
	}
}

func l33Script(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?s)<script>(.*?)</script>`).FindSubmatch(b)
	if m == nil {
		t.Fatal("no script")
	}
	return string(m[1])
}

// F: the page's Enter handler calls doLogin() whenever #loginbox isn't hidden, and after sign-in only <main
// id=auth> is hidden: every Enter in any field of the app posts the sign-in form again (its password is still
// in it). After a password change each Enter is a wrong password: 5 lock the owner's address out of the panel
// and SSH for 10 minutes; before that, each Enter restarts the app and leaves another event stream open
// until the page's requests stall (Playwright: pw/t_enter.js, pw/t_enter2.js).
func TestL33EnterOnlySignsInOnTheSignInScreen(t *testing.T) {
	js := l33Script(t)
	m := regexp.MustCompile(`document\.addEventListener\('keydown',e=>\{[^\n]*doLogin\(\)[^\n]*`).FindString(js)
	if m == "" {
		t.Fatal("Enter handler not found")
	}
	if !strings.Contains(m, "$('auth')") {
		t.Errorf("the Enter handler doesn't check that the sign-in screen is showing: %s", m)
	}
	if !regexp.MustCompile(`function start\(\)\{if\(es\)es\.close\(\);`).MatchString(js) {
		t.Errorf("start() opens a new event stream without closing the open one")
	}
	if !regexp.MustCompile(`(?s)async function doLogin\(\)\{.{0,400}?\$\('p'\)\.value=''`).MatchString(js) {
		t.Errorf("the password stays in the sign-in form after signing in")
	}
}

// F: saving one Settings card (SSH, network, notifications) re-renders every card: what's typed in the others
// (a new password, a chat ID, a time server) is lost and the card's own result vanishes in under a second
// (Playwright: pw/t_wipe.js, pw/t_net2.js).
func TestL33SettingsSaveKeepsOtherCards(t *testing.T) {
	js := l33Script(t)
	for _, f := range []string{"saveNetwork", "saveSSH", "saveNotify"} {
		m := regexp.MustCompile(`(?s)async function ` + f + `\(\)\{.*?\n(async )?function `).FindString(js)
		if m == "" {
			t.Fatalf("%s not found", f)
		}
		if strings.Contains(m, "setTimeout(loadSettings") {
			t.Errorf("%s reloads every Settings card", f)
		}
	}
}

// F: "Save network" with a MAC change and a new port saves the MAC first, then asks about the port: Cancel
// leaves the MAC changed with no message (Playwright: pw/t_net.js case B).
func TestL33NetworkAsksBeforeSavingMAC(t *testing.T) {
	js := l33Script(t)
	m := regexp.MustCompile(`(?s)async function saveNetwork\(\)\{.*?\n(async )?function `).FindString(js)
	ci, mi := strings.Index(m, "confirm('Change the web-UI port"), strings.Index(m, "await saveMAC()")
	if ci < 0 || mi < 0 || ci > mi {
		t.Errorf("the port question comes after the MAC is saved (confirm at %d, saveMAC at %d)", ci, mi)
	}
}

// F: Update source -> Save with a bad repository shows the error, then "Update source saved" over it.
func TestL33UpdateRepoToastOnlyOnSuccess(t *testing.T) {
	js := l33Script(t)
	m := regexp.MustCompile(`async function updRepo\(\)\{[^\n]*`).FindString(js)
	if strings.Contains(m, "await updPost({repo:$('updrepo').value});toast(") {
		t.Errorf("updRepo says saved whatever the answer: %s", m)
	}
}

// F: "Restart mining" asks "…hashing pauses ~1 min. No reboot." and answers the same, but PUT /mcb/restart
// reboots the whole miner (softRestartMining; the card's own text says so).
func TestL33RestartMiningSaysItReboots(t *testing.T) {
	js := l33Script(t)
	m := regexp.MustCompile(`async function softRestart\(\)\{[^\n]*`).FindString(js)
	if strings.Contains(m, "No reboot") {
		t.Errorf("the Restart mining question says no reboot: %s", m)
	}
	src, _ := os.ReadFile("main.go")
	if strings.Contains(string(src), `"note": "restarting the mining software — hashing pauses ~1 min"`) {
		t.Errorf("handleRestart answers that hashing pauses a minute")
	}
}
