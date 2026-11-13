package main

import (
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// An owner's miner lost a board at about 1 pm and only got it back when they restarted mining at 2:20 pm.
// Now mining restarts by itself after 3 minutes, once an hour at most, and not while something else is in charge.
func TestBoardDownRestartsMining(t *testing.T) {
	*authFile = filepath.Join(t.TempDir(), "auth.json")
	bw = &boardWatchState{seen: map[int]bool{}, since: map[int]time.Time{}, alerted: map[int]bool{}, thisRun: map[int]bool{}}
	got := captureNotify(t)
	var restarts atomic.Int32
	boardRestart = func() error { restarts.Add(1); return nil }
	now := time.Unix(1_790_000_000, 0)
	nwNow = func() time.Time { return now }
	four := func(b1 float64, el int) dashboard {
		return dashboard{OK: true, Elapsed: el, Boards: []board{{ID: 0, Alive: true, GHS: 1150}, {ID: 1, Alive: b1 > 0, GHS: b1},
			{ID: 2, Alive: true, GHS: 1150}, {ID: 3, Alive: true, GHS: 1150}}}
	}
	el := 3600
	step := func(b1 float64, d time.Duration) {
		now = now.Add(d)
		el += int(d.Seconds())
		boardWatch(four(b1, el), "")
	}
	step(1150, 0)
	for i := 0; i < 20; i++ { // board 1 drops
		step(0, 10*time.Second)
	}
	time.Sleep(50 * time.Millisecond)
	if restarts.Load() != 1 || !strings.Contains(strings.Join(*got, "\n"), "mining restarted by itself") {
		t.Fatalf("restarts %d, notices %v", restarts.Load(), *got)
	}
	el = 30 // mining restarted: a new session; the board is back
	step(1150, time.Minute)
	for i := 0; i < 40; i++ { // and drops again 20 minutes later: no second restart within the hour
		step(map[bool]float64{true: 1150, false: 0}[i < 20], time.Minute)
	}
	time.Sleep(50 * time.Millisecond)
	last := (*got)[len(*got)-1]
	if restarts.Load() != 1 || !strings.Contains(last, "already restarted for a board in the last hour") {
		t.Fatalf("restarts %d, last notice %q", restarts.Load(), last)
	}
	// switched off by the owner: notice only
	os.WriteFile(boardAutoOffPath(), nil, 0o644)
	boardAutoAt = time.Time{}
	if why := boardAutoRestart(now, "test"); why != "off" {
		t.Fatalf("switched off: %q", why)
	}
	os.Remove(boardAutoOffPath())
	boardAutoBusy = func() string { return "the strong-chip test is running" }
	if why := boardAutoRestart(now, "test"); why != "the strong-chip test is running" || restarts.Load() != 1 {
		t.Fatalf("during the strong-chip test: %q, restarts %d", why, restarts.Load())
	}
}

// The events and the logs at an incident reach the private export, redacted.
func TestIncidentAndEventsInExport(t *testing.T) {
	*authFile = filepath.Join(t.TempDir(), "auth.json")
	evMu.Lock()
	evLines, evLoaded, evDirty = nil, true, false
	evMu.Unlock()
	old := incidentLogs
	incidentLogs = func() map[string]string { return map[string]string{"mining-log": "C1: chain lost"} }
	defer func() { incidentLogs = old }()
	activity.emit("🧩 board 1 isn't hashing")
	for i := 0; i < 3; i++ {
		incidentSnapshot("board 1 stopped hashing")
		time.Sleep(1100 * time.Millisecond)
	}
	if n := len(incidentTexts()); n != 2 {
		t.Fatalf("%d snapshots kept, want 2", n)
	}
	if !strings.Contains(incidentTexts()[1][1], "C1: chain lost") {
		t.Fatal("snapshot without the log")
	}
	eventsSave()
	evMu.Lock()
	evLines, evLoaded = nil, false
	evMu.Unlock()
	if !strings.Contains(eventsText(), "board 1 isn't hashing") {
		t.Fatalf("events not kept across a restart: %q", eventsText())
	}
}
