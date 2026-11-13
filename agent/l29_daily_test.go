package main

import (
	"path/filepath"
	"testing"
	"time"
)

// The daily summary is remembered only in its file: on a read-only (or full) config partition the 09:00 check,
// every minute until 10:00, sent it again and again (notifyForce lets 4 an hour through).
func TestL29DailySummaryOnceWhenConfigReadOnly(t *testing.T) {
	cfg, ro, _ := l29Mount(t)
	oldA, oldP := *authFile, *apiAddr
	*authFile, *apiAddr = filepath.Join(cfg, "auth.json"), "127.0.0.1:47619"
	sent := 0
	notifyTestHook = func(ev, msg string) {
		if ev == evDaily {
			sent++
		}
	}
	notifyMu.Lock()
	oldC := notifyCfg
	notifyCfg.Events = map[string]bool{evDaily: true}
	notifyMu.Unlock()
	t.Cleanup(func() {
		*authFile, *apiAddr = oldA, oldP
		notifyTestHook = nil
		notifyMu.Lock()
		notifyCfg = oldC
		notifyMu.Unlock()
	})
	ro()
	nine := time.Date(2026, 11, 9, 9, 0, 0, 0, time.Local)
	for m := 0; m < 60; m++ {
		dailyTick(nine.Add(time.Duration(m) * time.Minute))
	}
	if sent != 1 {
		t.Errorf("BUG: the daily summary was sent %d times between 09:00 and 10:00 (once expected)", sent)
	}
}
