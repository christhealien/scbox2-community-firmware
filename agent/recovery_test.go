package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func recTestDir(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	old := *authFile
	*authFile = filepath.Join(d, "auth.json")
	recMu.Lock()
	recUnexp, recCfg, recLastSave = nil, recoveryConfig{}, ""
	recMu.Unlock()
	oldU := watchdogUsable
	watchdogUsable = true // the device logic is still tested (the board's driver is what can't be used)
	t.Cleanup(func() { watchdogUsable = oldU })
	t.Cleanup(func() { *authFile = old; watchdogStop() })
	return d
}

func TestRecoveryUnexpectedRestart(t *testing.T) {
	recTestDir(t)
	boot := time.Now().Unix() - 120
	os.WriteFile(recAlivePath(), []byte(`{"t":`+itoa(int(boot-3600))+`}`), 0644)
	os.WriteFile(keptLogPath(), []byte("Nov  4 07:32:40 kernel: last words\n"), 0644)
	old := time.Unix(boot-3600, 0)
	os.Chtimes(keptLogPath(), old, old)
	recoveryCheckRestart(boot)
	u := unexpectedForDashboard()
	if u == nil || u.LastSeen != boot-3600 || u.BootAt != boot {
		t.Fatalf("unexpected %+v", u)
	}
	if b, err := os.ReadFile(keptLogBeforePath()); err != nil || !strings.Contains(string(b), "last words") {
		t.Fatalf("kept log not moved: %v", err)
	}
}

func TestRecoveryCleanRebootAndAgentRestart(t *testing.T) {
	recTestDir(t)
	boot := time.Now().Unix() - 120
	os.WriteFile(recAlivePath(), []byte(`{"t":`+itoa(int(boot-60))+`}`), 0644)
	markCleanReboot()
	recoveryCheckRestart(boot)
	if unexpectedForDashboard() != nil {
		t.Fatal("a reboot the panel asked for was reported")
	}
	if _, err := os.Stat(recCleanPath()); err == nil {
		t.Fatal("clean-reboot note not consumed")
	}
	// the agent restarted within this boot: its alive note is newer than the boot
	os.WriteFile(recAlivePath(), []byte(`{"t":`+itoa(int(boot+30))+`}`), 0644)
	recoveryCheckRestart(boot)
	if unexpectedForDashboard() != nil {
		t.Fatal("an agent restart was reported as a miner restart")
	}
}

func TestWatchdogFeedsAndDisarms(t *testing.T) {
	d := recTestDir(t)
	dev := filepath.Join(d, "watchdog")
	os.WriteFile(dev, nil, 0644)
	old := watchdogDev
	watchdogDev = dev
	defer func() { watchdogDev = old }()
	if err := watchdogStart(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1200 * time.Millisecond) // ioctl fails on a plain file → default 10 s, fed every 2.5 s; first write is immediate
	watchdogStop()
	b, _ := os.ReadFile(dev)
	if len(b) < 2 || b[0] != 0 || b[len(b)-1] != 'V' {
		t.Fatalf("device got %q", b)
	}
	v := recoveryView()
	if v["watchdog_running"].(bool) {
		t.Fatal("still running")
	}
	if _, err := json.Marshal(v); err != nil {
		t.Fatal(err)
	}
}

func TestKeepLogOnlyWhenChanged(t *testing.T) {
	d := recTestDir(t)
	old := *logPath
	*logPath = filepath.Join(d, "messages")
	defer func() { *logPath = old }()
	os.WriteFile(*logPath, []byte("Nov  4 07:30:00 a\n"), 0644)
	keepLogTick()
	fi1, err := os.Stat(keptLogPath())
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	keepLogTick()
	fi2, _ := os.Stat(keptLogPath())
	if !fi1.ModTime().Equal(fi2.ModTime()) {
		t.Fatal("rewrote an unchanged log")
	}
}

func TestWatchdogHangNeverBlocks(t *testing.T) {
	d := recTestDir(t)
	fifo := filepath.Join(d, "wdt")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Skip(err)
	}
	old, oldT := watchdogDev, wdtOpenTimeout
	watchdogDev, wdtOpenTimeout = fifo, 200*time.Millisecond // opening a FIFO for writing blocks: a device that never answers
	defer func() { watchdogDev, wdtOpenTimeout = old, oldT }()
	recMu.Lock()
	recCfg.Watchdog = true
	recMu.Unlock()
	t0 := time.Now()
	if err := watchdogStart(); err == nil {
		t.Fatal("no error from a hung device")
	}
	if time.Since(t0) > 2*time.Second {
		t.Fatal("watchdogStart waited on the device")
	}
	if _, err := os.Stat(wdtMarkerPath()); err == nil {
		t.Fatal("marker left behind")
	}
	v := recoveryView() // the panel's own views don't wait on the device
	if v["watchdog"].(bool) || v["watchdog_running"].(bool) {
		t.Fatalf("still on: %v", v)
	}
	// unblock the stuck open so the goroutine ends (it disarms and closes, as answered too late)
	if f, err := os.OpenFile(fifo, os.O_RDONLY|syscall.O_NONBLOCK, 0); err == nil {
		time.Sleep(100 * time.Millisecond)
		f.Close()
	}
}

func TestWatchdogMarkerSwitchesOff(t *testing.T) {
	recTestDir(t)
	os.WriteFile(recConfigPath(), []byte(`{"watchdog":true}`), 0644)
	os.WriteFile(wdtMarkerPath(), []byte("x"), 0644) // the last start died while switching it on
	oldDelay := wdtStartDelay
	wdtStartDelay = time.Hour
	defer func() { wdtStartDelay = oldDelay }()
	recoveryStartup()
	if recoveryConfigLoad().Watchdog {
		t.Fatal("watchdog left on after a start that hung on it")
	}
	if _, err := os.Stat(wdtMarkerPath()); err == nil {
		t.Fatal("marker left behind")
	}
}
