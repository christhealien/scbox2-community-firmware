package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFitMaxRPM(t *testing.T) {
	// PprTwls' Noctuas: flagged by the stock check (1560 RPM at 100 % < 1700)
	slow := []int{-1, 900, 900, 480, 480, 840, 900, 1140, 1260, 1440, 1560}
	if f := fitMaxRPM(slow); f != 1250 {
		t.Fatalf("slow fit %d, want 1250", f)
	}
	for i := 3; i <= 10; i++ { // every measured point clears the fitted line
		if slow[i] <= fanLimitAt(1250, i*10) {
			t.Fatalf("%d %%: %d RPM under the fitted limit %d", i*10, slow[i], fanLimitAt(1250, i*10))
		}
	}
	// a stock-like fan: never above Goldshell's 2200
	if f := fitMaxRPM([]int{-1, 300, 420, 660, 900, 1080, 1320, 1560, 1740, 1920, 2040}); f != 2125 {
		t.Fatalf("stock-like fit %d", f)
	}
	if f := fitMaxRPM([]int{-1, 600, 900, 1200, 1500, 1800, 2100, 2400, 2700, 3000, 3300}); f != fanStockMax() {
		t.Fatalf("fast fan capped at stock: %d", f)
	}
	// a fan that doesn't turn isn't fitted around
	if fitMaxRPM([]int{-1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}) != 0 {
		t.Fatal("fitted a dead fan")
	}
	sw := parseFanSweep(` [1970-01-01 09:00:31]  [Xminerd]: fan0 [-1, 300, 420, 660, 900, 1080, 1320, 1560, 1740, 1920, 2040]
 [1970-01-01 09:00:31]  [Xminerd]: fan1 [-1, 300, 480, 780, 1020, 1260, 1500, 1740, 1860, 2100, 2220]`)
	if len(sw) != 2 || sw[1][10] != 2220 {
		t.Fatalf("sweep %v", sw)
	}
}

func TestFanCheckFitWritesOverlay(t *testing.T) {
	d := t.TempDir()
	*authFile = filepath.Join(d, "auth.json")
	old := productPath
	productPath = filepath.Join(d, "product.json")
	t.Cleanup(func() { productPath = old })
	os.WriteFile(productPath, []byte(`{"model":"SC Lite","fans":[{"id":0,"maxrpm":2200,"minpwm":20},{"id":1,"maxrpm":2200,"minpwm":20}]}`), 0o644)
	oldSw := readFanSweep
	t.Cleanup(func() { readFanSweep = oldSw })
	lin := func(k int) []int {
		c := []int{-1}
		for p := 10; p <= 100; p += 10 {
			c = append(c, k*p)
		}
		return c
	}
	readFanSweep = func() map[int][]int { return map[int][]int{0: lin(10), 1: lin(20)} }
	if err := fanCheckFit([]int{875, 2125}); err == nil {
		t.Fatal("a fit more lenient than fan 0's measured speeds accepted")
	}
	if err := fanCheckFit([]int{1250, 300}); err == nil {
		t.Fatal("300 RPM accepted")
	}
	if err := fanCheckFit([]int{1250, 2125}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(fanOverlayPath())
	var doc map[string]any
	json.Unmarshal(b, &doc)
	fans := doc["fans"].([]any)
	if doc["model"] != "SC Lite" || fans[0].(map[string]any)["maxrpm"].(float64) != 1250 || fans[1].(map[string]any)["minpwm"].(float64) != 20 {
		t.Fatalf("overlay %s", b)
	}
	if _, err := os.Stat(fanStockCopy()); err != nil {
		t.Fatal("no copy of Goldshell's file kept")
	}
	if err := fanCheckStock(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(fanOverlayPath()); err == nil {
		t.Fatal("overlay kept after going back to stock")
	}
}

func TestFanLock(t *testing.T) {
	d := t.TempDir()
	*authFile = filepath.Join(d, "auth.json")
	oldB := backupRoot
	backupRoot = d
	t.Cleanup(func() { backupRoot = oldB })
	os.MkdirAll(filepath.Join(d, "xminerd/trash"), 0o755)
	idled := 0
	oldI, oldN := fanLockIdle, fanLockNotify
	fanLockIdle = func() error { idled++; return nil }
	var told string
	fanLockNotify = func(m string) { told = m }
	t.Cleanup(func() { fanLockIdle, fanLockNotify = oldI, oldN; setCharge("") })
	write := func(now time.Time, status string, f0 int) {
		l := " [" + now.Format("2006-01-02 15:04:05") + "]  [Xminerd] fan0:(96, " + itoa(f0) + ")  fan1:(96, 2160)  fan2:(96, 2100)  fan3:(96, 2040)  . fan status: " + status + "\n"
		os.WriteFile(watchdogPath(), []byte(l), 0o644)
	}
	now := time.Now()
	for i := 0; i < 3; i++ {
		write(now, "1", 0)
		fanLockTick(now)
		now = now.Add(15 * time.Second)
	}
	if fanLockErr() != nil {
		t.Fatal("locked before a minute")
	}
	write(now, "0", 2040) // a good reading resets the count
	fanLockTick(now)
	for i := 0; i < 4; i++ {
		now = now.Add(15 * time.Second)
		write(now, "1", 0)
		fanLockTick(now)
	}
	if fanLockErr() == nil || idled != 1 || !strings.Contains(told, "fan 0 (0 RPM)") {
		t.Fatalf("not locked: idled %d told %q", idled, told)
	}
	if err := applyPresetName("Quiet"); err == nil || !strings.Contains(err.Error(), "locked") {
		t.Fatalf("a preset went through the lock: %v", err)
	}
	if err := applyStockPlan(); err == nil || !strings.Contains(err.Error(), "locked") {
		t.Fatalf("Stock went through the lock: %v", err)
	}
	if fanUnlock(false, now) == nil {
		t.Fatal("unlocked without accepting")
	}
	if err := fanUnlock(true, now); err == nil || !strings.Contains(err.Error(), "still flags") {
		t.Fatalf("unlocked while the fan is still flagged: %v", err)
	}
	write(now, "0", 2040)
	if err := fanUnlock(true, now); err != nil || fanLockErr() != nil {
		t.Fatalf("unlock: %v", err)
	}
	// switched off: never locks
	saveFanLockConfig(fanLockConfig{Off: true})
	for i := 0; i < 6; i++ {
		now = now.Add(15 * time.Second)
		write(now, "1", 0)
		fanLockTick(now)
	}
	if fanLockErr() != nil {
		t.Fatal("locked while switched off")
	}
}
