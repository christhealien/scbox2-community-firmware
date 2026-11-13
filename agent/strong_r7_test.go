package main

// Round-7 review of the strong-chip test ("Find strong chips"): the clock window, restores that fail or are
// cut short, and the fan lock.

import (
	"bytes"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func sctR7Window(t *testing.T, set int, ok bool) {
	t.Helper()
	old := reapplySetClock
	reapplySetClock = func() (int, bool) { return set, ok }
	t.Cleanup(func() { reapplySetClock = old })
}

// The miner on a stock (firmware) level has no manual powerplan: storedPlan, and so reapplySetClock, says
// !ok, and the start's clock-window check is skipped. A board at 700 MHz (the locked window's top) then gets
// chips raised to 750 MHz.
func TestSctR7StockLevelSkipsWindowCheck(t *testing.T) {
	f := sctSetup(t)
	f.base10 = 7000
	saveOverclock(overclockState{}) // locked: clockWindowHi() == 700
	sctR7Window(t, 0, false)
	if e := sctStart(2, 0, 0); e == "" {
		over := 0.0
		sctRun(t, f, 4*time.Hour, func() {
			for _, m := range sctTrimsOf(2) {
				if 700+m > float64(clockWindowHi()) && m > over {
					over = m
				}
			}
		})
		t.Fatalf("started on a 700 MHz board with the window at %d MHz; chips ran up to %+.4g MHz over the board", clockWindowHi(), over)
	}
}

// Re-locking the clock limits (or a lower measured cap) while the test runs: the raised chips must not stay
// past the new window, nor be kept there as the owner's trims.
func TestSctR7WindowLoweredMidTest(t *testing.T) {
	f := sctSetup(t)
	f.base10 = 7000
	if err := saveOverclock(overclockState{Acknowledged: true}); err != nil {
		t.Fatal(err)
	}
	sctR7Window(t, 700, true)
	if e := sctStart(2, 0, 0); e != "" {
		t.Fatal(e)
	}
	relocked := false
	worst := 0.0
	sctRun(t, f, 4*time.Hour, func() {
		if !relocked && sct.step == 2 {
			saveOverclock(overclockState{}) // "re-lock" in Controls: the window is 700 again
			relocked = true
			return
		}
		if relocked {
			for _, m := range sctTrimsOf(2) {
				if m > worst {
					worst = m
				}
			}
		}
	})
	if !relocked {
		t.Fatal("never got to step 2")
	}
	if worst > 0 {
		t.Errorf("after the re-lock chips still ran up to %+.4g MHz over a 700 MHz board (window %d)", worst, clockWindowHi())
	}
	for c, m := range sctTrimsOf(2) {
		if m > 0 {
			t.Errorf("b2.c%d kept %+.4g MHz: %.4g MHz, past the %d MHz window", c, m, 700+m, clockWindowHi())
		}
	}
}

// The test ends (stop, abort) but putting the trims back fails (flash full, a write error). Its message says
// "it's done at the next panel start", and the backup stays; but the result file says "not running", so the
// next start's recovery deletes the backup and the raised trims stay for good, as the owner's own.
func TestSctR7FailedRestoreRecoveredAtStart(t *testing.T) {
	f := sctSetup(t)
	before, _ := os.ReadFile(chipTrimPath())
	if e := sctStart(2, 0, 0); e != "" {
		t.Fatal(e)
	}
	for i := 0; i < int((50*time.Minute)/sctTick); i++ {
		f.advance(sctTick)
		sctTickOnce()
	}
	during, _ := os.ReadFile(chipTrimPath())
	if bytes.Equal(before, during) {
		t.Fatal("no test trims yet")
	}
	// the trims file can't be replaced now (a directory in the way stands for any write error)
	os.Remove(chipTrimPath())
	os.Mkdir(chipTrimPath(), 0o755)
	sctStop()
	l := sctLast(t)
	t.Logf("stop: %v", l["message"])
	// the panel restarts with the file as the chips last had it: the test's trims
	os.Remove(chipTrimPath())
	os.Remove(chipTrimPath() + ".tmp")
	os.WriteFile(chipTrimPath(), during, 0o644)
	sct.mu.Lock()
	sct.reset()
	sct.mu.Unlock()
	sctRecover()
	after, _ := os.ReadFile(chipTrimPath())
	if !bytes.Equal(before, after) {
		t.Fatalf("the test's trims stayed after the promised recovery:\n%s\nwant\n%s", after, before)
	}
	sctOwnTrimsIntact(t)
}

// A power cut while the result file was being replaced (it isn't synced: on flash its data can be lost while
// the rename stays) leaves it empty or cut short; the backup is there. Recovery treats that as "no test
// running", deletes the backup and the raised trims stay as the owner's.
func TestSctR7TruncatedResultRecovered(t *testing.T) {
	f := sctSetup(t)
	before, _ := os.ReadFile(chipTrimPath())
	if e := sctStart(2, 0, 0); e != "" {
		t.Fatal(e)
	}
	for i := 0; i < int((50*time.Minute)/sctTick); i++ {
		f.advance(sctTick)
		sctTickOnce()
	}
	os.WriteFile(strongResult(), []byte(`{"running":tr`), 0o644) // cut short
	sct.mu.Lock()
	sct.reset()
	sct.mu.Unlock()
	sctRecover()
	after, _ := os.ReadFile(chipTrimPath())
	if !bytes.Equal(before, after) {
		t.Fatalf("test trims left after a cut-short result:\n%s", after)
	}
	sctOwnTrimsIntact(t)
}

// The fan lock puts the miner on Idle: the test must end at once with the trims back (as the thermal guard's
// and the pool guard's Idle do), not sit "unsteady" for 10 minutes and then say "aborted".
func TestSctR7FanLockEndsTest(t *testing.T) {
	sctSetup(t)
	before, _ := os.ReadFile(chipTrimPath())
	oIdle, oNote := fanLockIdle, fanLockNotify
	fanLockIdle = func() error { return nil }
	fanLockNotify = func(string) {}
	old := getCharge()
	t.Cleanup(func() { fanLockIdle, fanLockNotify = oIdle, oNote; setCharge(old); os.Remove(fanLockPath()) })
	if e := sctStart(2, 0, 0); e != "" {
		t.Fatal(e)
	}
	fanLockEngage(watchdogView{Flagged: "fan 1 at 0 rpm", Status: 1}, time.Now())
	if sctRunning() {
		t.Fatal("the strong-chip test goes on after the fan lock put the miner on Idle")
	}
	after, _ := os.ReadFile(chipTrimPath())
	if !bytes.Equal(before, after) {
		t.Fatalf("trims not restored:\n%s", after)
	}
}

// After a failed restore, a new start first finishes the old restore (or refuses): it mustn't back up the old
// test's raised trims as "the owner's" and keep them for good.
func TestSctR7StartAfterFailedRestore(t *testing.T) {
	f := sctSetup(t)
	before, _ := os.ReadFile(chipTrimPath())
	if e := sctStart(2, 0, 0); e != "" {
		t.Fatal(e)
	}
	for i := 0; i < int((50*time.Minute)/sctTick); i++ {
		f.advance(sctTick)
		sctTickOnce()
	}
	during, _ := os.ReadFile(chipTrimPath())
	os.Remove(chipTrimPath())
	os.Mkdir(chipTrimPath(), 0o755)
	sctStop()
	os.Remove(chipTrimPath())
	os.Remove(chipTrimPath() + ".tmp")
	os.WriteFile(chipTrimPath(), during, 0o644) // writable again; the file holds the old test's trims
	if e := sctStart(2, 0, 0); e != "" {
		t.Logf("start refused: %s", e)
	} else {
		sctStop()
	}
	after, _ := os.ReadFile(chipTrimPath())
	if !bytes.Equal(before, after) {
		t.Fatalf("the old test's trims stayed:\n%s\nwant\n%s", after, before)
	}
	sctOwnTrimsIntact(t)
}

// Locks and data races: the test's ticks, starts and stops against the guards' ends (sctEndFor), auto-trim's
// tick, hand edits, the views and the restore retry.
func TestSctR7LockStress(t *testing.T) {
	f := sctSetup(t)
	var clk sync.Mutex
	now := f.now
	sctNow = func() time.Time { clk.Lock(); defer clk.Unlock(); return now }
	oStats, oErrs, oApply, oBlocked := cwReadStats, cwReadErrors, cwApply, cwBlocked
	cwReadStats = func() (chipStatsFile, bool) { clk.Lock(); defer clk.Unlock(); return f.stats() }
	cwReadErrors = func() (map[ckey]int, error) { clk.Lock(); defer clk.Unlock(); return f.errors() }
	cwApply = func() error { return nil }
	cwBlocked = func() string {
		if sctRunning() {
			return "paused while the strong-chip test runs"
		}
		return ""
	}
	sctReadStats = func() (chipStatsFile, bool) { clk.Lock(); defer clk.Unlock(); return f.stats() }
	sctReadErrors = func() (map[ckey]int, error) { clk.Lock(); defer clk.Unlock(); return f.errors() }
	defer func() { cwReadStats, cwReadErrors, cwApply, cwBlocked = oStats, oErrs, oApply, oBlocked }()
	stop, done := make(chan struct{}), make(chan struct{})
	var wg sync.WaitGroup
	go func() {
		defer close(done)
		for i := 0; i < 30; i++ {
			sctStart(2, 5, 5)
			for j := 0; j < 50; j++ {
				clk.Lock()
				f.advance(sctTick)
				now = f.now
				clk.Unlock()
				sctTickOnce()
			}
			if i%2 == 0 {
				sctStop()
			} else {
				sctEndFor("The fan lock: test")
			}
		}
	}()
	for _, g := range []func(){
		func() { sctEndFor("The thermal guard: test") },
		func() { chipWatchTick() },
		func() { chipTrimView() },
		func() { sctRestoreRetry(1) },
		func() {
			rr := httptest.NewRecorder()
			handleChipTrim(rr, httptest.NewRequest("POST", "/api/chiptrim", strings.NewReader(`{"chain":0,"chip":9,"mhz":-12.5}`)))
		},
	} {
		g := g
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					g()
					time.Sleep(time.Millisecond)
				}
			}
		}()
	}
	select {
	case <-done:
	case <-time.After(90 * time.Second):
		t.Fatal("deadlock")
	}
	close(stop)
	wg.Wait()
}
