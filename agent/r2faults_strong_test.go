package main

// Round-2 fault injection against the strong-chip test (strongtest.go). Each test injects one fault and checks
// the test ends safely: trims back exactly as before (or kept only when it finished), never frozen, never a
// chip above +50 or a "+" it didn't pass.

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// r2NoPlus: no chip on the test's board has a "+" trim other than what the result says it kept.
func r2NoUnpassedPlus(t *testing.T, l map[string]any) {
	t.Helper()
	kept := map[int]float64{}
	if l != nil {
		if _, ok := l["chips"].([]any); ok {
			kept = sctKept(t, l)
		}
	}
	for c, m := range sctTrimsOf(2) {
		if m > 50 {
			t.Fatalf("b2.c%d at %+v > +50", c, m)
		}
		if m > 0 && m != kept[c] {
			t.Fatalf("b2.c%d left at %+v but the test kept %+v (file %v)", c, m, kept[c], sctTrimsOf(2))
		}
	}
}

func r2Ticks(f *sctFake, d time.Duration) {
	for i := 0; i < int(d/sctTick); i++ {
		f.advance(sctTick)
		if !sctTickOnce() {
			return
		}
	}
}

// A board whose typical chip made > ~745 errors in the baseline: exp(-median) underflows to 0, poissonCDF is 0
// for every k and sctCandidates' limit loop never ends, with sct.mu held (the card, auto-trim and the chip-trim
// page then block on it too).
func TestR2HugeBaselineErrorsDontHang(t *testing.T) {
	f := sctSetup(t)
	f.errRate = func(chip int, trim float64, _ map[int]float64) float64 { return 3000 } // 1000 per 20-min baseline
	if e := sctStart(2, 0, 0); e != "" {
		t.Fatal(e)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		r2Ticks(f, 30*time.Minute)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("HANG: the baseline's end never returned (sctCandidates' poissonCDF loop, sct.mu held)")
	}
	if sctRunning() {
		t.Fatalf("still running after the baseline (step %d)", sct.step)
	}
	r2NoUnpassedPlus(t, sctLast(t))
	sctOwnTrimsIntact(t)
}

// The miner's per-chip error counters can't be read any more (API down, icinfo gone) while the hook keeps
// reporting: the test must end (trims back), not sit at a raised step forever.
func TestR2ErrorReaderDownForever(t *testing.T) {
	f := sctSetup(t)
	before, _ := os.ReadFile(chipTrimPath())
	if e := sctStart(2, 0, 0); e != "" {
		t.Fatal(e)
	}
	r2Ticks(f, 30*time.Minute) // into step 1 (+12.5)
	if sct.step != 1 {
		t.Fatalf("step %d", sct.step)
	}
	sctReadErrors = func() (map[ckey]int, error) { return nil, errors.New("connection refused") }
	end := f.now.Add(3 * time.Hour)
	for f.now.Before(end) {
		f.advance(sctTick)
		if !sctTickOnce() {
			break
		}
	}
	if sctRunning() {
		t.Fatalf("FROZEN: still running 3 h after the error counters went away, step %d, note %q, trims %v", sct.step, sct.note, sctTrimsOf(2))
	}
	after, _ := os.ReadFile(chipTrimPath())
	if !bytes.Equal(before, after) {
		t.Fatalf("trims not restored:\n%s", after)
	}
}

// No board temperature (sensor gone, dashboard down): raising chips to +50 with no heat watch at all.
func TestR2NoTemperature(t *testing.T) {
	for _, tc := range []struct {
		name  string
		board func(chain int) (sctBoardInfo, bool)
	}{
		{"temp 0", func(chain int) (sctBoardInfo, bool) { return sctBoardInfo{Found: true, Alive: true}, true }},
		{"temp NaN", func(chain int) (sctBoardInfo, bool) {
			return sctBoardInfo{Found: true, Alive: true, Temp: math.NaN()}, true
		}},
		{"dashboard down", func(chain int) (sctBoardInfo, bool) { return sctBoardInfo{}, false }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := sctSetup(t)
			before, _ := os.ReadFile(chipTrimPath())
			if e := sctStart(2, 0, 0); e != "" {
				t.Fatal(e)
			}
			sctReadBoard = tc.board
			sctRun(t, f, 4*time.Hour, nil)
			l := sctLast(t)
			if l["how"] == "done" {
				t.Fatalf("BLIND: finished with no temperature reading, kept %v", sctKept(t, l))
			}
			after, _ := os.ReadFile(chipTrimPath())
			if !bytes.Equal(before, after) {
				t.Fatalf("trims not restored:\n%s", after)
			}
		})
	}
	// and it must not start blind either
	f := sctSetup(t)
	_ = f
	sctReadBoard = func(chain int) (sctBoardInfo, bool) { return sctBoardInfo{Found: true, Alive: true}, true }
	if e := sctStart(2, 0, 0); e == "" {
		t.Fatal("started with no temperature reading")
	}
}

// Backup deleted mid-test, then Stop: the test's + trims must still come off.
func TestR2BackupGoneThenStop(t *testing.T) {
	f := sctSetup(t)
	before, _ := os.ReadFile(chipTrimPath())
	if e := sctStart(2, 0, 0); e != "" {
		t.Fatal(e)
	}
	r2Ticks(f, 50*time.Minute)
	if sct.step != 2 {
		t.Fatalf("step %d", sct.step)
	}
	os.Remove(strongBackup())
	if e := sctStop(); e != "" {
		t.Fatal(e)
	}
	after, _ := os.ReadFile(chipTrimPath())
	if !bytes.Equal(before, after) {
		t.Fatalf("STOP LEFT TEST TRIMS (backup gone):\n%s\nwant\n%s", after, before)
	}
}

// The trims file can't be written at the end of a finished test (the .tmp is in the way: full/ro storage):
// the file must not keep a step a chip failed.
func TestR2KeepWriteFails(t *testing.T) {
	f := sctSetup(t)
	bad := 3
	f.errRate = func(chip int, trim float64, _ map[int]float64) float64 {
		if chip == bad && trim >= 50 {
			return 30
		}
		return sctBackground(chip)
	}
	if e := sctStart(2, 0, 0); e != "" {
		t.Fatal(e)
	}
	blocked := false
	sctRun(t, f, 4*time.Hour, func() {
		if !blocked && sct.step == 4 && sct.counting {
			os.Mkdir(chipTrimPath()+".tmp", 0o755) // writeChipTrims' tmp can't be created
			blocked = true
		}
	})
	if !blocked {
		t.Fatal("never reached step 4")
	}
	l := sctLast(t)
	t.Logf("how %v: %v", l["how"], l["message"])
	r2NoUnpassedPlus(t, l)
	if _, err := os.Stat(strongBackup()); err != nil && sctTrimsOf(2)[bad] > 37.5 {
		t.Fatal("backup removed though the final trims weren't written")
	}
}

// Panel restart mid-test with the backup gone: the test's + trims must not stay forever.
func TestR2RecoverNoBackup(t *testing.T) {
	f := sctSetup(t)
	before, _ := os.ReadFile(chipTrimPath())
	if e := sctStart(2, 0, 0); e != "" {
		t.Fatal(e)
	}
	r2Ticks(f, 50*time.Minute)
	sct.mu.Lock()
	sct.reset()
	sct.mu.Unlock()
	os.Remove(strongBackup())
	sctRecover()
	after, _ := os.ReadFile(chipTrimPath())
	if !bytes.Equal(before, after) {
		t.Fatalf("RECOVERY LEFT TEST TRIMS (no backup):\n%s", after)
	}
	if l, ok := sctView()["last"].(map[string]any); !ok || l["how"] != "interrupted" {
		t.Fatalf("result still says running / not interrupted: %v", sctView()["last"])
	}
}

// A corrupt (truncated: power cut during its write) backup at recovery: the owner's own trims must survive.
func TestR2RecoverCorruptBackup(t *testing.T) {
	f := sctSetup(t)
	if e := sctStart(2, 0, 0); e != "" {
		t.Fatal(e)
	}
	r2Ticks(f, 50*time.Minute)
	sct.mu.Lock()
	sct.reset()
	sct.mu.Unlock()
	b, _ := os.ReadFile(strongBackup())
	os.WriteFile(strongBackup(), b[:len(b)-12], 0o644) // the last line cut off
	sctRecover()
	r2NoUnpassedPlus(t, nil)
	if m := sctTrimsOf(2); m[40] != -12.5 {
		t.Fatalf("owner's trim lost by a corrupt backup: %v", m)
	}
}

// A stale backup (left by an earlier test) at a restart while no test runs: must not undo trims kept since.
func TestR2RecoverStaleBackup(t *testing.T) {
	f := sctSetup(t)
	if e := sctStart(2, 0, 0); e != "" {
		t.Fatal(e)
	}
	stale, _ := os.ReadFile(strongBackup())
	sctRun(t, f, 4*time.Hour, nil)
	kept := sctTrimsOf(2)
	os.WriteFile(strongBackup(), stale, 0o644) // e.g. the remove failed, or an old file put back by hand
	sctRecover()
	if got := sctTrimsOf(2); fmt.Sprint(got) != fmt.Sprint(kept) {
		t.Fatalf("STALE BACKUP undid the finished test's kept trims: %v -> %v", kept, got)
	}
	if l := sctLast(t); l["how"] != "done" {
		t.Fatalf("finished result rewritten: %v", l["how"])
	}
}

// The live apply reaches only some chips (the others stay at the board clock): abort, everything back.
func TestR2PartialApply(t *testing.T) {
	f := sctSetup(t)
	before, _ := os.ReadFile(chipTrimPath())
	if e := sctStart(2, 0, 0); e != "" {
		t.Fatal(e)
	}
	r2Ticks(f, 40*time.Minute) // in step 1, counting
	inner := sctReadStats
	sctReadStats = func() (chipStatsFile, bool) {
		st, ok := inner()
		for i := range st.Chains {
			c := st.Chains[i].Clock10
			for ch := 1; ch < len(c); ch += 4 { // every 4th chip ignores its trim
				c[ch] = st.Chains[i].Base10
			}
		}
		return st, ok
	}
	sctRun(t, f, 3*time.Hour, nil)
	l := sctLast(t)
	after, _ := os.ReadFile(chipTrimPath())
	if l["how"] != "aborted" || !bytes.Equal(before, after) {
		t.Fatalf("how %v (%v), trims\n%s", l["how"], l["message"], after)
	}
}

// The board disappears mid-step (hook drops it, dashboard "No CPB"): abort after the unsteady limit, trims back.
func TestR2BoardVanishes(t *testing.T) {
	for _, where := range []string{"hook", "dashboard"} {
		t.Run(where, func(t *testing.T) {
			f := sctSetup(t)
			before, _ := os.ReadFile(chipTrimPath())
			if e := sctStart(2, 0, 0); e != "" {
				t.Fatal(e)
			}
			r2Ticks(f, 55*time.Minute)
			if where == "hook" {
				inner := sctReadStats
				sctReadStats = func() (chipStatsFile, bool) {
					st, ok := inner()
					st.Chains = nil
					return st, ok
				}
			} else {
				sctReadBoard = func(chain int) (sctBoardInfo, bool) { return sctBoardInfo{Temp: 70}, true }
			}
			start := f.now
			sctRun(t, f, 2*time.Hour, nil)
			if d := f.now.Sub(start); d > sctUnsteadyMax+time.Minute {
				t.Fatalf("took %v to give up", d)
			}
			after, _ := os.ReadFile(chipTrimPath())
			if l := sctLast(t); l["how"] != "aborted" || !bytes.Equal(before, after) {
				t.Fatalf("how %v, trims\n%s", l["how"], after)
			}
		})
	}
}

// Miner restarts (counters back to 0) in every phase: settle, baseline, each step's settle and its counting.
func TestR2MinerRestartEveryPhase(t *testing.T) {
	f := sctSetup(t)
	if e := sctStart(2, 0, 0); e != "" {
		t.Fatal(e)
	}
	type ph struct {
		step     int
		counting bool
	}
	hit := map[ph]bool{}
	sctRun(t, f, 8*time.Hour, func() {
		p := ph{sct.step, sct.counting}
		if !hit[p] && (!p.counting || f.now.Sub(sct.snapAt) > 7*time.Minute) {
			hit[p] = true
			f.restartMiner()
		}
	})
	if len(hit) < 10 {
		t.Fatalf("restarted in %d phases only: %v", len(hit), hit)
	}
	l := sctLast(t)
	if l["how"] != "done" {
		t.Fatalf("how %v %v", l["how"], l["message"])
	}
	for c, k := range sctKept(t, l) {
		if k != 50 {
			t.Fatalf("c%d kept %v", c, k)
		}
	}
	r2NoUnpassedPlus(t, l)
	sctOwnTrimsIntact(t)
}

// Error counters going backwards on one chip only (an icinfo glitch, not a restart) and huge spikes on one chip.
func TestR2CounterGlitches(t *testing.T) {
	f := sctSetup(t)
	if e := sctStart(2, 0, 0); e != "" {
		t.Fatal(e)
	}
	inner := sctReadErrors
	n := 0
	sctReadErrors = func() (map[ckey]int, error) {
		m, err := inner()
		n++
		switch {
		case n%97 == 0:
			m[ckey{2, 11}] = 0 // goes backwards: a restart as far as the test can tell
		case n%89 == 0:
			m[ckey{2, 11}] = 1 << 30 // garbage (fits a 32-bit int: the tests also build for the miner's ARM)
		}
		return m, err
	}
	sctRun(t, f, 12*time.Hour, nil)
	l := sctLast(t)
	t.Logf("how %v: %v", l["how"], l["message"])
	r2NoUnpassedPlus(t, l)
	sctOwnTrimsIntact(t)
}

// A panic inside a tick (a reader blowing up): the panel process must not die with the test's trims on (the
// loop has no recover; the whole agent goes down). With a recover it should end "aborted", trims back.
func TestR2PanicInTick(t *testing.T) {
	f := sctSetup(t)
	before, _ := os.ReadFile(chipTrimPath())
	if e := sctStart(2, 0, 0); e != "" {
		t.Fatal(e)
	}
	r2Ticks(f, 50*time.Minute)
	sctReadErrors = func() (map[ckey]int, error) { panic("index out of range [47] with length 47") }
	var pv any
	func() {
		defer func() { pv = recover() }()
		f.advance(sctTick)
		sctTickOnce()
	}()
	if pv != nil {
		t.Fatalf("PANIC escaped the tick (kills the panel; trims stay until restart recovery): %v", pv)
	}
	after, _ := os.ReadFile(chipTrimPath())
	if sctRunning() || !bytes.Equal(before, after) {
		t.Fatalf("running %v, trims\n%s", sctRunning(), after)
	}
}

// Stop racing ticks (a step change) from real goroutines: one clean end, trims exactly back. Run with -race.
func TestR2StopRacesStep(t *testing.T) {
	for i := 0; i < 20; i++ {
		f := sctSetup(t)
		var fmu sync.Mutex
		before, _ := os.ReadFile(chipTrimPath())
		if e := sctStart(2, 5, 5); e != "" {
			t.Fatal(e)
		}
		sctNow = func() time.Time { fmu.Lock(); defer fmu.Unlock(); return f.now }
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 400; j++ {
				fmu.Lock()
				f.advance(sctTick)
				fmu.Unlock()
				if !sctTickOnce() {
					return
				}
			}
		}()
		go func() {
			defer wg.Done()
			time.Sleep(time.Duration(i) * 200 * time.Microsecond)
			sctStop()
		}()
		wg.Wait()
		if sctRunning() {
			sctStop()
		}
		after, _ := os.ReadFile(chipTrimPath())
		l := sctLast(t)
		if l["how"] == "stopped" && !bytes.Equal(before, after) {
			t.Fatalf("run %d: stopped but trims\n%s", i, after)
		}
		r2NoUnpassedPlus(t, l)
	}
}

// Two starts at once through the API path: exactly one runs.
func TestR2TwoStarts(t *testing.T) {
	sctSetup(t)
	var wg sync.WaitGroup
	res := make([]string, 8)
	for i := range res {
		wg.Add(1)
		go func(i int) { defer wg.Done(); res[i] = sctStart(2, 0, 0) }(i)
	}
	wg.Wait()
	ok := 0
	for _, r := range res {
		if r == "" {
			ok++
		}
	}
	if ok != 1 {
		t.Fatalf("%d starts succeeded: %q", ok, res)
	}
	sctStop()
	sctOwnTrimsIntact(t)
}

// Auto-trim while the test runs: paused, no write; and after it, it doesn't mistake the test's kept + trims.
func TestR2AutoTrimPausedDuringTest(t *testing.T) {
	f := sctSetup(t)
	if e := sctStart(2, 0, 0); e != "" {
		t.Fatal(e)
	}
	r2Ticks(f, 50*time.Minute)
	if why := cwBlockedReal(); !strings.Contains(why, "strong-chip") {
		t.Fatalf("auto-trim not paused: %q", why)
	}
	sctStop()
}

// A raised chip that stops hashing at +25 (no nonces, and so no errors; icinfo may drop it from its list): the
// step judge looks only at errors and the board's total nonce rate (1 chip of 46 = -2.2 %, inside its 3-sigma
// band), so the dead chip "passes" and is kept at +50.
func TestR2RaisedChipStopsHashing(t *testing.T) {
	f := sctSetup(t)
	dead := 3
	inner := sctReadStats
	var frozenAt int
	sctReadStats = func() (chipStatsFile, bool) {
		st, ok := inner()
		for i := range st.Chains {
			n := st.Chains[i].Nonces
			if f.trims()[dead] >= 25 {
				if frozenAt == 0 {
					frozenAt = n[dead]
				}
				n[dead] = frozenAt
			} else {
				frozenAt = 0
			}
		}
		return st, ok
	}
	ie := sctReadErrors
	sctReadErrors = func() (map[ckey]int, error) {
		m, err := ie()
		if f.trims()[dead] >= 25 {
			delete(m, ckey{2, dead}) // perf 0, hwerr 0: not listed
		}
		return m, err
	}
	if e := sctStart(2, 0, 0); e != "" {
		t.Fatal(e)
	}
	sctRun(t, f, 4*time.Hour, nil)
	l := sctLast(t)
	kept := sctKept(t, l)
	if _, ok := kept[dead]; !ok {
		t.Skipf("c%d wasn't a candidate: %v", dead, kept)
	}
	if kept[dead] > 12.5 {
		t.Fatalf("DEAD CHIP KEPT: c%d made no nonces from +25 on but kept %+v (how %v: %v)", dead, kept[dead], l["how"], l["message"])
	}
}

// One raised chip's error counter jumps to a garbage value and stays there (icinfo glitch / wrap): the step's
// judge calls poissonSF(k≈2e9, λ), an O(k) loop, with sct.mu held.
func TestR2GarbageCounterInStepDoesntHang(t *testing.T) {
	f := sctSetup(t)
	if e := sctStart(2, 0, 0); e != "" {
		t.Fatal(e)
	}
	r2Ticks(f, 30*time.Minute) // step 1
	c := sct.chips[0].Chip
	inner := sctReadErrors
	jump := false
	sctReadErrors = func() (map[ckey]int, error) {
		m, err := inner()
		if jump {
			m[ckey{2, c}] += 2_000_000_000
		}
		return m, err
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < int(3*time.Hour/sctTick); i++ {
			if sct.counting && sct.step == 1 {
				jump = true
			}
			f.advance(sctTick)
			if !sctTickOnce() {
				return
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second): // ~7 s on amd64 before the fix; ARMv7 is several times slower
		panic("HANG: the step judge never returned (poissonSF O(k) with a garbage count, sct.mu held)")
	}
	r2NoUnpassedPlus(t, sctLast(t))
}

// Same in auto-trim: a chip's counter jumps by 2e9 and stays: cwJudge's poissonSF(r.n, 3·med) loops 2e9 times
// with cwMu held (the chip-trim page's chipWatchView blocks behind it).
func TestR2AutoTrimGarbageCounterDoesntHang(t *testing.T) {
	s := newCwSim(t, func(chip int, trim float64) float64 { return 1 })
	on := true
	chipWatchSet(&on, false)
	s.run(30 * time.Minute)
	s.errs[ckey{2, 7}] += 2_000_000_000
	done := make(chan struct{})
	go func() { defer close(done); s.run(2 * time.Hour) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		panic("HANG: cwJudge never returned (poissonSF O(n) with a garbage count, cwMu held)")
	}
	t.Logf("trims %v", s.trims())
}
