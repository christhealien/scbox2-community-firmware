package main

// Round 7 fuzz findings (chip trims), as plain regression tests. Each failed on the v1.5.21 tree.

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// An API trim a hair off the 12.5 MHz grid passed the panel's 1e-9 tolerance and was saved as typed; the panel
// then showed it as +12.5 while the trim library (exact grid check) skipped the line: the chip ran untrimmed.
func TestR7TrimPostSnapsToGrid(t *testing.T) {
	r7Dir(t)
	r7ClockStub(t, 600)
	for _, c := range []struct {
		mhz  string
		want string
	}{{"12.500000001", "2 14 12.5\n"}, {"-99.99999999999", "2 14 -100\n"}, {"-1e-320", ""}} {
		os.Remove(chipTrimPath())
		rr := httptest.NewRecorder()
		handleChipTrim(rr, httptest.NewRequest("POST", "/api/chiptrim", strings.NewReader(`{"chain":2,"chip":14,"mhz":`+c.mhz+`}`)))
		b, _ := os.ReadFile(chipTrimPath())
		body := string(b)
		if i := strings.IndexByte(body, '\n'); i >= 0 {
			body = body[i+1:]
		}
		if body != c.want {
			t.Errorf("mhz %s saved as %q, want %q", c.mhz, body, c.want)
		}
		if d := r7Diff(b); d != "" {
			t.Errorf("mhz %s: %s", c.mhz, d)
		}
	}
}

// A hand-edited trims file: the panel must show what the library applies (no hidden trims, no phantom ones).
func TestR7ReadTrimsLikeTheLibrary(t *testing.T) {
	r7Dir(t)
	for _, f := range []string{
		"2 14 12.5-\n",           // panel showed +12.5, library skipped it
		"2 14 12.5.5\n",          // same
		"2 14.0 -12.5\n",         // library applied -12.5 to chip 14, panel showed nothing
		"2.7 14 -12.5\n",         // same (chain cut to 2)
		"2 14-12.5\n",            // same
		"2 14 -12.50000000001\n", // panel showed -12.5, library skipped it
		strings.Repeat("# padding padding padding padding padding padding\n", 340) + "3 46 -50\n",                                                                                               // past the library's 16383 bytes
		"2 14 -25\n2 14 -12.5\n2 13 -12.5\n2 12 -12.5\n2 11 -12.5\n2 10 -12.5\n2 9 -12.5\n2 8 -12.5\n2 7 -12.5\n2 6 -12.5\n2 5 -12.5\n2 4 -12.5\n2 3 -12.5\n2 2 -12.5\n2 1 -12.5\n2 14 -37.5\n", // duplicates: the last wins
	} {
		if d := r7Diff([]byte(f)); d != "" {
			t.Errorf("%q: %s", f, d)
		}
	}
}

// A damaged stats file ("chips": 2000000000) made chipTrimView build two billion rows: the Chip trims card,
// /api/chipreport and every 10-minute history sample hung the agent and ate its memory.
func TestR7StatsHugeChipCount(t *testing.T) {
	r7Dir(t)
	os.WriteFile(chipStatsPath, []byte(`{"t":1,"chains":[{"chain":2,"chips":2000000000,"base10":6250,"nonces":[0,5,5]}]}`), 0o644)
	done := make(chan []chainView, 1)
	go func() { done <- chipTrimView()["chains"].([]chainView) }()
	select {
	case ch := <-done:
		if len(ch) != 1 || len(ch[0].Chips) > 62 {
			t.Fatalf("%d rows", len(ch[0].Chips))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("chipTrimView hung on chips=2000000000")
	}
}

// auto-trim's state file with a trim budget but no board rates (a hand edit, or one map left empty): the first
// judged window that adds a trim panicked "assignment to entry in nil map" and took the whole agent down (fans
// included), again after each restart.
func TestR7WatchBudgetWithoutBoardRate(t *testing.T) {
	r7WatchRun(t, `{"on":true,"budget":{"0":1}}`, map[int]int{11: 200})
	tr := readChipTrims()
	found := false
	for _, x := range tr {
		found = found || (x.Chain == 2 && x.Chip == 11 && x.MHz == -12.5)
	}
	if !found {
		t.Fatalf("the bad chip wasn't slowed: %+v", tr)
	}
}

// A step being judged that didn't help is undone to its "prev" from the state file, unchecked: prev +50 there
// made auto-trim run a chip 50 MHz over its board (past the clock window check every other + trim gets).
func TestR7WatchUndoPrevChecked(t *testing.T) {
	r7WatchRun(t, `{"on":true,"owned":[{"chain":2,"chip":5,"mhz":-12.5,"since":1,"before":1,"prev":50}]}`, map[int]int{5: 200})
	for _, x := range readChipTrims() {
		if x.MHz > 0 && !(x.Chain == 2 && x.Chip == 9 && x.MHz == 25) {
			t.Fatalf("auto-trim set a + trim: %+v", x)
		}
	}
}

// r7WatchRun: one full auto-trim window on board 2 (46 chips; trims 2.5 -12.5, 2.9 +25, 2.20 -50), state as given.
func r7WatchRun(t *testing.T, state string, errs map[int]int) {
	t.Helper()
	r7Dir(t)
	writeChipTrims([]chipTrim{{2, 5, -12.5}, {2, 9, 25}, {2, 20, -50}})
	os.WriteFile(chipWatchPath(), []byte(state), 0o644)
	now := time.Unix(1_800_000_000, 0)
	var st chipStatsFile
	json.Unmarshal([]byte(`{"t":1,"chains":[{"chain":2,"chips":46,"base10":6000}]}`), &st)
	e0, e1 := map[ckey]int{}, map[ckey]int{}
	for i := 1; i <= 46; i++ {
		e0[ckey{2, i}], e1[ckey{2, i}] = 0, errs[i]
	}
	cur := e0
	o1, o2, o3, o4, o5 := cwReadErrors, cwReadStats, cwApply, cwNow, cwBlocked
	t.Cleanup(func() {
		cwReadErrors, cwReadStats, cwApply, cwNow, cwBlocked = o1, o2, o3, o4, o5
		cwMu.Lock()
		cwSt, cwLoaded = cwState{}, false
		cwResetWindow()
		cwMu.Unlock()
	})
	cwReadErrors = func() (map[ckey]int, error) { return cur, nil }
	cwReadStats = func() (chipStatsFile, bool) { return st, true }
	cwApply = func() error { return nil }
	cwNow = func() time.Time { return now }
	cwBlocked = func() string { return "" }
	cwMu.Lock()
	cwSt, cwLoaded = cwState{}, false
	cwResetWindow()
	cwMu.Unlock()
	func() {
		defer func() {
			if p := recover(); p != nil {
				t.Fatalf("auto-trim panicked: %v", p)
			}
		}()
		chipWatchTick()
		now = now.Add(cwWindow + time.Minute)
		cur = e1
		chipWatchTick()
	}()
}

// {"baseline_min": 307445734561825870} wrapped round time.Duration into a 9 min 44 s window and was accepted.
func TestR7StrongWindowOverflow(t *testing.T) {
	sctSetup(t)
	r7ClockStub(t, 600)
	rr := httptest.NewRecorder()
	handleStrongTest(rr, httptest.NewRequest("POST", "/api/strongtest", bytes.NewReader([]byte(`{"start":true,"chain":2,"baseline_min":307445734561825870}`))))
	var m map[string]any
	json.Unmarshal(rr.Body.Bytes(), &m)
	if m["ok"] == true {
		t.Fatalf("accepted: %v", m)
	}
}
