package main

// L16-schedule: the weekly schedule and the time zone (schedule.go, tz.go). Driven through scheduleTick with the
// hlMiner mock (heatloop_test.go) and, where the wall clock matters (DST, 1970), the schedNow seam.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// l16Env: the hlMiner mock plus a /mcb/restart counter (and an optional hook that runs inside the restart
// request), a fresh schedule file and fresh schedule state.
type l16Env struct {
	m        *hlMiner
	restarts atomic.Int32
	onRst    func()
	failGET  atomic.Int32 // the next n GET /mcb/setting answer 500
}

func newL16(t *testing.T) *l16Env {
	e := &l16Env{m: newHLMiner(t)}
	inner := e.m.srv.Config.Handler
	e.m.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/user/login") && r.Header.Get("Authorization") == "" {
			http.Error(w, "login first", 401) // (the panel's first request logs in and asks again: counted once)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/mcb/restart") {
			e.restarts.Add(1)
			if e.onRst != nil {
				e.onRst()
			}
			w.Write([]byte(`{"status":0}`))
			return
		}
		if r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/mcb/setting") && e.failGET.Load() > 0 {
			e.failGET.Add(-1)
			http.Error(w, "backend busy", 500)
			return
		}
		inner.ServeHTTP(w, r)
	})
	schedPath = filepath.Join(t.TempDir(), "schedule.json")
	lastSchKey, lastSchName, schedBefore, schedBeforeBy, lastRstMinute = -1, "", nil, "", 0
	savedLocal := time.Local
	t.Cleanup(func() {
		schedRunMu.Lock() // a tick started by handleSchedule ends first
		schedRunMu.Unlock()
		schedPath = ""
		lastSchKey, lastSchName, schedBefore, schedBeforeBy, lastRstMinute = -1, "", nil, "", 0
		time.Local = savedLocal
		rebootMu.Lock() // the scheduled restarts' "restarting" window (it pauses the fan loop's work for minutes)
		rebootStart, rebootTotal, rebootUntil, rebootLabel = time.Time{}, 0, time.Time{}, ""
		rebootMu.Unlock()
	})
	return e
}

func l16Put(t *testing.T, s schedule) {
	b, _ := json.Marshal(s)
	if err := writeAtomic(schedPath, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func l16Zone(t *testing.T, name string) *time.Location {
	data, err := tzData(name)
	if err != nil {
		t.Fatal(err)
	}
	loc, err := time.LoadLocationFromTZData(name, data)
	if err != nil {
		t.Fatal(err)
	}
	time.Local = loc
	return loc
}

var l16All = []int{0, 1, 2, 3, 4, 5, 6}

// 1. The owner saves the schedule while a tick is busy with the miner (a scheduled restart, a preset change: the
// backend is slow then). The tick then saves the schedule IT loaded (to note the restart or clear an error) over
// the owner's save: the page said "saved", the file has the old schedule again (here: still on, still restarting
// every hour, though the owner switched both off).
func TestL16TickSaveOverwritesOwnersSave(t *testing.T) {
	e := newL16(t)
	if time.Now().Second() > 45 {
		time.Sleep(time.Duration(61-time.Now().Second()) * time.Second) // the restart minute must not roll over
	}
	now := time.Now()
	l16Put(t, schedule{Enabled: true, Rules: []schedRule{{Start: "00:00", Days: l16All, Preset: "Stock"}},
		Restart: schedRestart{Every: "hour", At: now.Format("15:04")}})
	inRst, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	e.onRst = func() { once.Do(func() { close(inRst) }); <-release }
	done := make(chan struct{})
	go func() { scheduleTick(); close(done) }()
	select {
	case <-inRst:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("setup: the scheduled restart didn't start")
	}
	// meanwhile the owner saves: schedule off, no restarts
	body := `{"action":"save","enabled":false,"rules":[],"base":"__back","restart":{"every":"off","marks":[]}}`
	rec := httptest.NewRecorder()
	saved := make(chan struct{})
	go func() { // (its answer waits for the tick: it re-arms the schedule under the tick's lock)
		handleSchedule(rec, httptest.NewRequest("POST", "/api/schedule", bytes.NewBufferString(body)))
		close(saved)
	}()
	for i := 0; i < 100 && loadSchedule().Enabled; i++ { // the owner's schedule is on disk
		time.Sleep(20 * time.Millisecond)
	}
	if loadSchedule().Enabled {
		close(release)
		t.Fatal("setup: the owner's save didn't reach the file")
	}
	close(release)
	<-done
	<-saved
	if !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Fatalf("setup: save refused: %s", rec.Body.String())
	}
	s := loadSchedule()
	if s.Enabled || s.Restart.Every != "off" {
		t.Fatalf("BUG: the tick's save put the old schedule back over the owner's (enabled %v, restart %q): the owner's \"saved\" schedule is gone", s.Enabled, s.Restart.Every)
	}
	if s.LastRestart == nil || s.LastRestart.Msg != "restarted" {
		t.Errorf("the restart that ran isn't noted: %+v", s.LastRestart)
	}
}

// 2. "Back to what it ran before": the stretch begins in the same minute the backend doesn't answer one read
// (it does that for a while after a change or a restart). What it ran before isn't noted, the stretch's preset is
// applied anyway, and at the end of the stretch nothing goes back: the miner stays on the stretch's Idle for good,
// with no error shown.
func TestL16BackLostWhenBeforeUnreadable(t *testing.T) {
	e := newL16(t)
	now := time.Now()
	if now.Hour() == 0 && now.Minute() < 5 {
		t.Skip("too near midnight for the HH:MM rules")
	}
	hm := func(d time.Duration) string { return now.Add(d).Format("15:04") }
	l16Put(t, schedule{Enabled: true, Rules: []schedRule{{Start: hm(-3 * time.Minute), Days: l16All, Preset: "Idle"}, {Start: hm(2 * time.Hour), Days: l16All, Preset: schedBack}}})
	e.failGET.Store(1) // the one read of what it runs now fails; the Idle's own read works
	scheduleTick()
	scheduleTick() // (a retry, if the fixed tree waits for the read)
	if !e.m.isIdle() {
		t.Fatalf("setup: the stretch didn't idle the miner")
	}
	l16Put(t, schedule{Enabled: true, Rules: []schedRule{{Start: hm(-3 * time.Minute), Days: l16All, Preset: "Idle"}, {Start: hm(-1 * time.Minute), Days: l16All, Preset: schedBack}}})
	scheduleTick() // the stretch is over
	if e.m.isIdle() {
		t.Fatalf("BUG: the read of what the miner ran before the stretch failed once, the Idle stretch was applied anyway, and \"back to before\" never came: the miner stays on Idle (charge %q, error %q)", getCharge(), loadSchedule().Error)
	}
	if got := e.m.planMHz(); got != 625 {
		t.Errorf("back to %d MHz, not the 625 it ran before", got)
	}
}

// 3. Fall back (America/New_York, Sun 2026-11-01: 01:00–01:59 comes twice). A daily restart at 01:30 restarts
// mining twice that night, an hour apart.
func TestL16FallBackRestartTwice(t *testing.T) {
	e := newL16(t)
	ny := l16Zone(t, "America/New_York")
	l16Put(t, schedule{Restart: schedRestart{Every: "day", At: "01:30"}})
	defer func() { schedNow = time.Now }()
	start := time.Date(2026, 11, 1, 0, 0, 30, 0, ny) // EDT
	for i := 0; i < 4*60; i++ {                      // 00:00 EDT … 02:59 EST, one tick a real minute
		at := start.Add(time.Duration(i) * time.Minute)
		schedNow = func() time.Time { return at }
		scheduleTick()
	}
	if n := e.restarts.Load(); n != 1 {
		t.Fatalf("BUG: a daily restart at 01:30 ran %d times on the night the clocks go back (01:30 EDT and again 01:30 EST)", n)
	}
}

// 4. Spring forward (America/New_York, Sun 2026-03-08: 02:00 → 03:00). A daily restart at 02:30 doesn't happen that
// day at all (its minute never comes); the same for a restart mark or a weekly one there.
func TestL16SpringForwardRestartSkipped(t *testing.T) {
	e := newL16(t)
	ny := l16Zone(t, "America/New_York")
	l16Put(t, schedule{Restart: schedRestart{Every: "day", At: "02:30"}})
	defer func() { schedNow = time.Now }()
	start := time.Date(2026, 3, 8, 0, 0, 30, 0, ny)
	for i := 0; i < 4*60; i++ { // 00:00 EST … 04:59 EDT
		at := start.Add(time.Duration(i) * time.Minute)
		schedNow = func() time.Time { return at }
		scheduleTick()
	}
	if n := e.restarts.Load(); n != 1 {
		t.Fatalf("BUG: a daily restart at 02:30 ran %d times on the day the clocks go forward (want it once, right after the jump)", n)
	}
	// "every hour" isn't caught up: its hours go on by the real clock (01:30 EST, then 03:30 EDT an hour later)
	e.restarts.Store(0)
	lastRstMinute = 0
	l16Put(t, schedule{Restart: schedRestart{Every: "hour", At: "00:30"}})
	for i := 0; i < 4*60; i++ {
		at := start.Add(time.Duration(i) * time.Minute)
		schedNow = func() time.Time { return at }
		scheduleTick()
	}
	if n := e.restarts.Load(); n != 4 { // 00:30 01:30 03:30 04:30
		t.Fatalf("hourly restarts on the spring-forward night: %d, want 4", n)
	}
}

// 5. Fall back with two changes in the repeated hour (01:00 Idle, 01:30 Stock, 00:00 Stock before): the second
// pass through 01:00–01:59 switches to Idle and back to Stock again (two extra changes, notifications, a 30-minute
// Idle the owner never painted twice).
func TestL16FallBackChangesTwice(t *testing.T) {
	e := newL16(t)
	ny := l16Zone(t, "America/New_York")
	l16Put(t, schedule{Enabled: true, Rules: []schedRule{{Start: "00:00", Days: l16All, Preset: "Stock"}, {Start: "01:00", Days: l16All, Preset: "Idle"}, {Start: "01:30", Days: l16All, Preset: "Stock"}}})
	defer func() { schedNow = time.Now }()
	start := time.Date(2026, 11, 1, 0, 10, 30, 0, ny)
	for i := 0; i < 3*60; i++ { // 00:10 EDT … 02:09 EST
		at := start.Add(time.Duration(i) * time.Minute)
		schedNow = func() time.Time { return at }
		scheduleTick()
	}
	if n := e.m.count("idle"); n != 1 {
		t.Fatalf("BUG: the 01:00 Idle change ran %d times on the night the clocks go back (the repeated hour applies it again)", n)
	}
}

// 6. The clock is still 1970 (no time server reached yet) and the owner saves or resumes the schedule: the save
// starts a tick at once, which doesn't check the clock (only the minute loop does), so the 1970 clock (Thursday
// 00:xx) picks the change in force and the restarts. Here: a Thursday-night Idle stretch idles the miner.
func TestL16NoActionOn1970Clock(t *testing.T) {
	e := newL16(t)
	l16Zone(t, "UTC")
	defer func() { schedNow = time.Now }()
	at := time.Unix(90, 0) // 1970-01-01 00:01:30 UTC, a Thursday (as the miner reads after boot)
	schedNow = func() time.Time { return at }
	l16Put(t, schedule{Enabled: true, Rules: []schedRule{{Start: "00:00", Days: []int{3}, Preset: "Idle"}, {Start: "06:00", Days: []int{3}, Preset: schedBack}},
		Restart: schedRestart{Every: "hour", At: "00:01"}})
	scheduleTick() // what handleSchedule's "go scheduleTick()" runs right after a save or a resume
	if e.m.isIdle() || e.restarts.Load() != 0 {
		t.Fatalf("BUG: with the clock at 1970 the schedule acted (idle %v, restarts %d): a 1970 clock puts the wrong preset in charge", e.m.isIdle(), e.restarts.Load())
	}
	// a 3-monthly restart saved then takes its anchor from 1970 too
	ns := schedule{Restart: schedRestart{Every: "3months", At: "03:00", Mday: 1}}
	if err := validateSchedule(&ns); err == nil && ns.Restart.Anchor != nil && *ns.Restart.Anchor < 2024*12 {
		t.Errorf("BUG: the 3-month restart's anchor is from the 1970 clock (%d)", *ns.Restart.Anchor)
	}
}

// Sound: the same zones without a change (Asia/Tokyo) and a midnight-crossing / whole-day schedule behave.
func TestL16NoDSTZonesAndWraps(t *testing.T) {
	e := newL16(t)
	tk := l16Zone(t, "Asia/Tokyo")
	l16Put(t, schedule{Enabled: true, Rules: []schedRule{{Start: "23:00", Days: l16All, Preset: "Idle"}, {Start: "01:00", Days: l16All, Preset: "Stock"}},
		Restart: schedRestart{Every: "day", At: "00:30"}})
	defer func() { schedNow = time.Now }()
	start := time.Date(2026, 11, 1, 22, 0, 30, 0, tk)
	for i := 0; i < 4*60; i++ { // 22:00 … 01:59 next day
		at := start.Add(time.Duration(i) * time.Minute)
		schedNow = func() time.Time { return at }
		scheduleTick()
	}
	if e.m.count("idle") != 1 || e.restarts.Load() != 1 || e.m.isIdle() {
		t.Fatalf("Tokyo midnight-crossing: idles %d restarts %d idle-now %v", e.m.count("idle"), e.restarts.Load(), e.m.isIdle())
	}
}

// The same in the other zones people pick: a daily restart in the repeated hour runs once, one in the skipped hour
// runs once (right after the jump), on each zone's 2026 change days. Lord Howe moves by 30 minutes.
func TestL16DSTZones(t *testing.T) {
	e := newL16(t)
	defer func() { schedNow = time.Now }()
	for _, c := range []struct {
		zone, at string
		y, m, d  int
		fromH    int
	}{
		{"Europe/Berlin", "02:30", 2026, 3, 29, 0},       // 02:00 → 03:00
		{"Europe/Berlin", "02:30", 2026, 10, 25, 0},      // 03:00 → 02:00
		{"Australia/Sydney", "02:30", 2026, 10, 4, 0},    // 02:00 → 03:00
		{"Australia/Sydney", "02:30", 2026, 4, 5, 0},     // 03:00 → 02:00
		{"Australia/Lord_Howe", "02:15", 2026, 10, 4, 0}, // 02:00 → 02:30
		{"Australia/Lord_Howe", "01:45", 2026, 4, 5, 0},  // 02:00 → 01:30
		{"Asia/Tokyo", "02:30", 2026, 3, 8, 0},
	} {
		loc := l16Zone(t, c.zone)
		e.restarts.Store(0)
		lastRstMinute = 0
		l16Put(t, schedule{Restart: schedRestart{Every: "day", At: c.at}})
		start := time.Date(c.y, time.Month(c.m), c.d, c.fromH, 0, 30, 0, loc)
		for i := 0; i < 5*60; i++ {
			at := start.Add(time.Duration(i) * time.Minute)
			schedNow = func() time.Time { return at }
			scheduleTick()
		}
		if n := e.restarts.Load(); n != 1 {
			t.Errorf("BUG: %s %d-%02d-%02d: a daily restart at %s ran %d times (want 1)", c.zone, c.y, c.m, c.d, c.at, n)
		}
	}
}
