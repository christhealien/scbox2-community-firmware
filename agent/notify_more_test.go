package main

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var captureMu sync.Mutex

func captureNotify(t *testing.T) *[]string {
	var got []string
	notifyTestHook = func(ev, msg string) { captureMu.Lock(); got = append(got, ev+": "+msg); captureMu.Unlock() }
	oldR, oldB := boardRestart, boardAutoBusy
	boardRestart = func() error { return nil }
	boardAutoBusy = func() string { return "" }
	boardAutoAt = time.Time{}
	oldL := incidentLogs
	incidentLogs = func() map[string]string { return map[string]string{"mining-log": "C1: board gone"} }
	t.Cleanup(func() { incidentWG.Wait(); incidentLogs = oldL; clearReboot() }) // (an automatic restart opens the restart window)
	t.Cleanup(func() {
		notifyTestHook = nil
		nwNow = time.Now
		boardRestart, boardAutoBusy = oldR, oldB
		boardAutoAt = time.Time{}
	})
	return &got
}

func TestBoardWatch(t *testing.T) {
	*authFile = filepath.Join(t.TempDir(), "auth.json")
	bw = &boardWatchState{seen: map[int]bool{}, since: map[int]time.Time{}, alerted: map[int]bool{}, thisRun: map[int]bool{}}
	got := captureNotify(t)
	now := time.Unix(1_790_000_000, 0)
	nwNow = func() time.Time { return now }
	four := func(c2 float64, el int) dashboard {
		return dashboard{OK: true, Elapsed: el, Boards: []board{{ID: 0, Alive: true, GHS: 1150}, {ID: 1, Alive: true, GHS: 1150},
			{ID: 2, Alive: c2 > 0, GHS: c2}, {ID: 3, Alive: true, GHS: 1150}}}
	}
	boardWatch(four(1150, 100), "") // all four hash: known
	// An owner's cold start: mining restarts, C2 doesn't start
	el := 10
	for i := 0; i < 60; i++ {
		el += 10
		now = now.Add(10 * time.Second)
		boardWatch(four(0, el), "")
	}
	if len(*got) != 1 || !strings.Contains((*got)[0], "board_down: board 2 isn't hashing") {
		t.Fatalf("one alert for board 2: %q", *got)
	}
	// restart mining: C2 back
	now = now.Add(time.Minute)
	boardWatch(four(1150, 30), "")
	if len(*got) != 2 || !strings.Contains((*got)[1], "board 2 is hashing again") {
		t.Fatalf("recovery: %q", *got)
	}
	// idle or tuning: no alerts
	*got = nil
	for i := 0; i < 60; i++ {
		now = now.Add(10 * time.Second)
		boardWatch(four(0, 600+i*10), "idle")
	}
	if len(*got) != 0 {
		t.Fatalf("idle: %q", *got)
	}
	// empty slots (never hashed) never alert: a single-board miner
	bw = &boardWatchState{seen: map[int]bool{}, since: map[int]time.Time{}, alerted: map[int]bool{}, thisRun: map[int]bool{}}
	incidentWG.Wait()
	*authFile = filepath.Join(t.TempDir(), "auth.json")
	for i := 0; i < 60; i++ {
		now = now.Add(10 * time.Second)
		boardWatch(dashboard{OK: true, Elapsed: 600 + i*10, Boards: []board{{ID: 2, Alive: true, GHS: 1150}, {ID: 0}, {ID: 1}, {ID: 3}}}, "")
	}
	if len(*got) != 0 {
		t.Fatalf("empty slots alerted: %q", *got)
	}
}

func TestFanWatch(t *testing.T) {
	fw = &fanWatchState{seen: map[int]bool{}, since: map[int]time.Time{}, alerted: map[int]bool{}, last: map[int]time.Time{}}
	got := captureNotify(t)
	now := time.Unix(1_790_000_000, 0)
	nwNow = func() time.Time { return now }
	fans := func(f ...int) dashboard { return dashboard{OK: true, Boards: []board{{ID: 2, Alive: true, Fans: f}}} }
	fanWatch(fans(1320, 1260, 1320, 1260), "")
	for i := 0; i < 30; i++ { // fan 3 stops
		now = now.Add(10 * time.Second)
		fanWatch(fans(1320, 1260, 0, 1260), "")
	}
	if len(*got) != 1 || !strings.Contains((*got)[0], "fan 3 stopped") {
		t.Fatalf("%q", *got)
	}
	for i := 0; i < 6*360; i++ { // still stopped: one repeat after 6 hours
		now = now.Add(10 * time.Second)
		fanWatch(fans(1320, 1260, 0, 1260), "")
	}
	if len(*got) != 2 {
		t.Fatalf("repeat every 6 h: %d", len(*got))
	}
	fanWatch(fans(1320, 1260, 1300, 1260), "")
	if len(*got) != 3 || !strings.Contains((*got)[2], "fan 3 is running again") {
		t.Fatalf("%q", *got)
	}
	// a fan slot that never spun (2-fan unit) never alerts
	*got = nil
	fw = &fanWatchState{seen: map[int]bool{}, since: map[int]time.Time{}, alerted: map[int]bool{}, last: map[int]time.Time{}}
	for i := 0; i < 30; i++ {
		now = now.Add(10 * time.Second)
		fanWatch(fans(1320, 1260, 0, 0), "")
	}
	if len(*got) != 0 {
		t.Fatalf("%q", *got)
	}
}

func TestPoolWatch(t *testing.T) {
	pw = &poolWatchState{active: -2}
	got := captureNotify(t)
	now := time.Unix(1_790_000_000, 0)
	nwNow = func() time.Time { return now }
	pl := func(active int, s0, s1 string) []poolView {
		return []poolView{{Index: 0, URL: "stratum+tcp://main:3333", Status: s0, Priority: 0, Active: active == 0},
			{Index: 1, URL: "stratum+tcp://backup:3333", Status: s1, Priority: 1, Active: active == 1}}
	}
	tick := func(p []poolView, n int) {
		for i := 0; i < n; i++ {
			now = now.Add(10 * time.Second)
			poolWatch(p, "")
		}
	}
	tick(pl(0, "Alive", "Alive"), 6)
	tick(pl(1, "Dead", "Alive"), 6)
	if len(*got) != 1 || !strings.Contains((*got)[0], "pool_switch: switched to a backup pool: stratum+tcp://backup:3333 (the primary, stratum+tcp://main:3333, is dead)") {
		t.Fatalf("%q", *got)
	}
	tick(pl(0, "Alive", "Alive"), 6)
	if len(*got) != 2 || !strings.Contains((*got)[1], "back on the primary pool") {
		t.Fatalf("%q", *got)
	}
	tick(pl(-1, "Dead", "Dead"), 20)
	if len(*got) != 3 || !strings.Contains((*got)[2], "no_pool: no pool is reachable") {
		t.Fatalf("%q", *got)
	}
	tick(pl(0, "Alive", "Dead"), 1)
	if len(*got) != 4 || !strings.Contains((*got)[3], "a pool is reachable again") {
		t.Fatalf("%q", *got)
	}
	// a brief blip (under 3 minutes) is not an outage
	*got = nil
	tick(pl(-1, "Dead", "Dead"), 5)
	tick(pl(0, "Alive", "Alive"), 3)
	if len(*got) != 0 {
		t.Fatalf("blip: %q", *got)
	}
}

func TestLoginBlockNotifies(t *testing.T) {
	got := captureNotify(t)
	for i := 0; i < 7; i++ {
		loginFailed("192.0.2.99", "panel")
	}
	time.Sleep(50 * time.Millisecond)
	captureMu.Lock()
	defer captureMu.Unlock()
	if !sshBlocked("192.0.2.99") || len(*got) != 1 || !strings.Contains((*got)[0], "login_block: 192.0.2.99 blocked for 10 min after 5 wrong passwords (panel)") {
		t.Fatalf("%v %q", sshBlocked("192.0.2.99"), *got)
	}
	sshSucceeded("192.0.2.99")
}

func TestUpdateResultNotifyOnce(t *testing.T) {
	*authFile = filepath.Join(t.TempDir(), "auth.json")
	got := captureNotify(t)
	updNotifyDelay = 0
	defer func() { updNotifyDelay = 2 * time.Minute }()
	os.MkdirAll(updDir(), 0o755)
	os.WriteFile(filepath.Join(updDir(), "last.json"), []byte(`{"result":"rolled back","from":"1.5.3","to":"1.5.2","at":1793609754,"note":"the panel didn't start within 5 minutes after the update"}`), 0o644)
	updateResultNotify()
	updateResultNotify() // after another restart: not again
	if len(*got) != 1 || (*got)[0] != "fw_update: firmware rolled back from v1.5.3 to v1.5.2: the panel didn't start within 5 minutes after the update" {
		t.Fatalf("%q", *got)
	}
}
