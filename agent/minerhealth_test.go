package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fakeProc(t *testing.T, names ...string) {
	t.Helper()
	d := t.TempDir()
	for i, n := range names {
		p := filepath.Join(d, string(rune('1'+i)))
		os.MkdirAll(p, 0o755)
		os.WriteFile(filepath.Join(p, "comm"), []byte(n+"\n"), 0o644)
	}
	os.MkdirAll(filepath.Join(d, "self"), 0o755)
	old := procRoot
	procRoot = d
	t.Cleanup(func() { procRoot = old })
}

func TestIntminerStopsCounted(t *testing.T) {
	mhMu.Lock()
	mhRunning, mhSeen, mhStops = false, false, nil
	mhMu.Unlock()
	now := time.Now()
	fakeProc(t, "fanctrl", "xminerd", "minerd", "intminer")
	minerHealthTick(now)
	fakeProc(t, "fanctrl", "xminerd", "minerd")
	minerHealthTick(now.Add(15 * time.Second))
	fakeProc(t, "intminer.real")
	minerHealthTick(now.Add(30 * time.Second))
	fakeProc(t)
	minerHealthTick(now.Add(45 * time.Second))
	if n := intminerStopsSince(now.Add(-time.Minute)); n != 2 {
		t.Fatalf("stops %d, want 2", n)
	}
	msg := miningDownMessage(now.Add(time.Minute))
	if !strings.Contains(msg, "keeps stopping: 2 times") || !strings.Contains(msg, "ribbon") {
		t.Fatalf("message: %s", msg)
	}
	fakeProc(t, "intminer")
	if miningDownMessage(now) != "" {
		t.Fatal("running mining program: no message expected")
	}
}

func TestMinerdExitsFromSyslog(t *testing.T) {
	now := time.Date(2026, 11, 2, 21, 40, 0, 0, time.Local)
	log := `Nov  2 21:20:00 [private] daemon.info init: process '/usr/bin/env LD_PRELOAD=/lib/sclite_bind.so minerd' (pid 1) exited. Scheduling for restart.
Nov  2 21:35:00 [private] daemon.info init: process '/usr/bin/env LD_PRELOAD=/lib/sclite_bind.so minerd' (pid 2) exited. Scheduling for restart.
Nov  2 21:39:30 [private] daemon.info init: process '/usr/bin/env LD_PRELOAD=/lib/sclite_bind.so minerd' (pid 3) exited. Scheduling for restart.
Nov  2 21:39:40 [private] daemon.info init: process '/sbin/getty -L' (pid 4) exited. Scheduling for restart.
Dec 31 18:00:04 [private] daemon.info init: starting pid 956, tty '/dev/ttyAMA3': 'minerd'`
	if n := minerdExits(log, now, 10*time.Minute); n != 2 {
		t.Fatalf("exits %d, want 2", n)
	}
}

func TestFriendlyBackendErr(t *testing.T) {
	fakeProc(t, "minerd")
	e := friendlyBackendErr(errors.New(`Put "http://127.0.0.1:80/mcb/setting": EOF`))
	if strings.Contains(e.Error(), "EOF") || !strings.Contains(e.Error(), "isn't running") {
		t.Fatalf("got %v", e)
	}
	if e := friendlyBackendErr(errors.New("HTTP 400: bad")); e.Error() != "HTTP 400: bad" {
		t.Fatalf("other errors pass through: %v", e)
	}
}

func TestReleaseWarning(t *testing.T) {
	body := "# SC Lite firmware v1.5.6\n\n**Install:** text\n\n> **⚠ This update starts tuning fresh.**\n> Presets made before…\n> - moves your presets aside\n>\n> Pools stay.\n\n## What's new\n> not this one\n"
	w := releaseWarning(body)
	if !strings.HasPrefix(w, "**⚠ This update starts tuning fresh.**") || !strings.Contains(w, "- moves your presets aside") || strings.Contains(w, "What's new") || strings.Contains(w, "not this one") {
		t.Fatalf("warning:\n%s", w)
	}
	if releaseWarning("# plain notes\n> a quote without a warning\n") != "" {
		t.Fatal("no ⚠: no warning")
	}
}

func TestOEMLogsRedacted(t *testing.T) {
	d := t.TempDir()
	old := backupRoot
	backupRoot = d
	t.Cleanup(func() { backupRoot = old })
	os.MkdirAll(filepath.Join(d, "minerd/trash"), 0o755)
	os.WriteFile(filepath.Join(d, "minerd/trash/syslog"), []byte(" [x] hardware_barcode->cpbsn[1]: SCL0PSA3019015F114032BAF070\n [x] hardware_barcode->boxsn: SCL0BS403000B25E93\n"), 0o644)
	t2, err := tailFile(oemLogs()[2][1], 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	out := redactBarcodes(t2)
	if strings.Contains(out, "SCL0") || !strings.Contains(out, "cpbsn[1]: [serial]") {
		t.Fatalf("serials kept:\n%s", out)
	}
}

func TestVerboseSwitchedOffWhenMiningKeepsStopping(t *testing.T) {
	d := t.TempDir()
	*authFile = filepath.Join(d, "auth.json")
	oldShim := *shimPath
	*shimPath = filepath.Join(d, "intminer")
	t.Cleanup(func() { *shimPath = oldShim })
	os.WriteFile(*shimPath, []byte("xx "+brandShim+" xx"), 0o755)
	os.WriteFile(verboseFlagPath(), nil, 0o644)
	mhMu.Lock()
	mhRunning, mhStops = false, nil
	mhMu.Unlock()
	now := time.Now()
	for i := 0; i < 3; i++ {
		fakeProc(t, "intminer")
		minerHealthTick(now.Add(time.Duration(i*2) * time.Minute))
		fakeProc(t)
		minerHealthTick(now.Add(time.Duration(i*2+1) * time.Minute))
		if _, err := os.Stat(verboseFlagPath()); i < 2 && err != nil {
			t.Fatalf("switched off after %d stops", i+1)
		}
	}
	if _, err := os.Stat(verboseFlagPath()); err == nil {
		t.Fatal("verbose still on after 3 stops in 15 minutes")
	}
	if _, err := os.Stat(verboseAutoOffPath()); err != nil {
		t.Fatal("no note for the Log tab")
	}
}

func TestWatchdogParse(t *testing.T) {
	now := time.Date(2026, 11, 3, 17, 39, 20, 0, time.Local)
	log := ` [2026-11-03 17:38:30]  [Xminerd] fan0:(91, 0)  fan1:(91, 2100)  fan2:(91, 2040)  fan3:(91, 1980)  . fan status: 1
 [2026-11-03 17:38:31]  [Xminerd] Kill Miner Reason NCF(0,1,1)!!
 [2026-11-03 17:38:31]  [Xminerd] Kill bfgminer pid 1174
 [2026-11-03 17:39:10]  [Xminerd] Kill Miner Reason NCF(0,1,1)!!
 [2026-11-03 17:39:11]  [Xminerd] fan0:(96, 0)  fan1:(96, 2160)  fan2:(96, 2100)  fan3:(96, 2040)  . fan status: 1
 [2026-11-03 17:39:12]  [Xminerd] NCF(0,1,1,0, 0)`
	v, ok := parseWatchdog(log, now)
	if !ok || v.Status != 1 || len(v.Fans) != 4 || v.Fans[0] != 0 || v.Fans[1] != 2160 || v.Kills != 2 {
		t.Fatalf("%+v %v", v, ok)
	}
	m := watchdogMessage(v)
	if !strings.Contains(m, "fan 0 (0 RPM)") || !strings.Contains(m, "speed wire") || !strings.Contains(m, "2 times") {
		t.Fatalf("message: %s", m)
	}
	// all four turning but too slow (another miner: status f)
	slow := ` [2026-11-03 17:39:00]  [Xminerd] fan0:(100, 1560)  fan1:(100, 1680)  fan2:(100, 1680)  fan3:(100, 1560)  . fan status: f`
	v, _ = parseWatchdog(slow, now)
	if v.Status != 15 || !strings.Contains(watchdogMessage(v), "Fan check can fit") {
		t.Fatalf("slow: %+v %s", v, watchdogMessage(v))
	}
	// healthy, and a stale reading isn't used
	ok2 := ` [2026-11-03 17:39:15]  [Xminerd] fan0:(96, 2040)  fan1:(96, 2220)  fan2:(96, 2100)  fan3:(96, 2100)  . fan status: 0`
	if v, _ := parseWatchdog(ok2, now); v.Status != 0 || watchdogMessage(v) != "" || len(v.Fans) != 4 {
		t.Fatalf("healthy: %+v", v)
	}
	if _, ok := parseWatchdog(ok2, now.Add(10*time.Minute)); ok {
		t.Fatal("a 10-minute-old reading was used")
	}
}

func TestReleaseWarningScoped(t *testing.T) {
	body := "# v1.5.10\\n\\n> **⚠ Coming from v1.5.5 or earlier, this update starts tuning fresh.**\\n> more\\n"
	body = strings.ReplaceAll(body, "\\n", "\n")
	if releaseWarningFor(body, "1.5.9") != "" {
		t.Fatal("a v1.5.9 miner doesn't need the ≤v1.5.5 warning")
	}
	if releaseWarningFor(body, "1.5.5") == "" || releaseWarningFor(body, "1.4.9") == "" {
		t.Fatal("v1.5.5 and older must see it")
	}
	plain := strings.ReplaceAll("> **⚠ This update clears presets.**\\n", "\\n", "\n")
	if releaseWarningFor(plain, "1.5.9") == "" {
		t.Fatal("an unscoped warning is for everyone")
	}
}

func TestVersionLessLabels(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"1.5.11-test", "v1.5.9", false},
		{"1.5.11-test", "v1.5.11", false},
		{"1.5.11-test", "v1.5.12", true},
		{"1.4.6", "1.4.10", true},
		{"1.5.10", "1.5.9", false},
	}
	for _, c := range cases {
		if got := versionLess(c.a, c.b); got != c.want {
			t.Errorf("versionLess(%q,%q)=%v", c.a, c.b, got)
		}
	}
}

func TestLearnedSuspect(t *testing.T) {
	v := watchdogView{Fans: []int{780, 840, 840, 840}, Duty: []int{33, 33, 33, 33}, Status: 1}
	// "Re-run the OEM fan test" (oemOf)
	if !learnedSuspect(v) || !strings.Contains(watchdogMessage(v), "Re-run "+oemOf()+" fan test") {
		t.Fatal("today's case (fan0 at its partner's speed) not recognised")
	}
	if learnedSuspect(watchdogView{Fans: []int{0, 840, 840, 840}, Duty: []int{33, 33, 33, 33}, Status: 1}) {
		t.Fatal("a stopped fan isn't a learned reading")
	}
	if learnedSuspect(watchdogView{Fans: []int{400, 840, 840, 840}, Duty: []int{33, 33, 33, 33}, Status: 1}) {
		t.Fatal("a fan at half its partner's speed is a real problem")
	}
	if learnedSuspect(watchdogView{Fans: []int{800, 840, 840, 840}, Duty: []int{33, 33, 33, 33}, Status: 3}) {
		t.Fatal("both of a pair flagged: not this case")
	}
}

// the v1.5.13+ inittab line, as init really logs it (two quotes before "(pid"): an owner's export said
// "minerd restarts in the last 10 min: 0" a minute after one
func TestMinerdExitsShellLine(t *testing.T) {
	now := time.Date(2026, 11, 5, 11, 15, 0, 0, time.Local)
	log := `Nov  5 11:14:11 [private] daemon.info init: process '/bin/sh -c 'echo "988b  /lib/sclite_bind.so" | sha256sum -c -s && LD_PRELOAD=/lib/sclite_bind.so exec minerd; sleep 60'' (pid 9372) exited. Scheduling for restart.
Nov  5 11:14:11 [private] daemon.info init: starting pid 17221, tty '/dev/ttyAMA3': '/bin/sh -c 'echo "988b  /lib/sclite_bind.so" | sha256sum -c -s && LD_PRELOAD=/lib/sclite_bind.so exec minerd; sleep 60''`
	if n := minerdExits(log, now, 10*time.Minute); n != 1 {
		t.Fatalf("exits %d, want 1", n)
	}
}
