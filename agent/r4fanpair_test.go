package main

// Round-4 fan pair offset review (tuner-sweep/r4-fanpair).

import (
	"bytes"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func r4fpSetup(t *testing.T, off int, lv ...int) {
	t.Helper()
	*authFile = filepath.Join(t.TempDir(), "auth.json")
	fakeFans(t, lv...)
	fanGuardClear()
	oldFT := fanTestRunning
	fanTestRunning = func() bool { return false }
	fpMu.Lock()
	fpOffset, fpLoaded, fpLastB, fpIdle, fpIdleAt = off, true, -1, false, time.Now()
	fpMu.Unlock()
	hgMu.Lock()
	hgHot, hgAt = 0, time.Time{}
	hgMu.Unlock()
	t.Cleanup(func() {
		fanTestRunning = oldFT
		fanGuardClear()
		fpMu.Lock()
		fpOffset, fpLoaded, fpLastB, fpIdle, fpIdleAt = 0, false, -1, false, time.Time{}
		fpMu.Unlock()
		hgMu.Lock()
		hgHot, hgAt = 0, time.Time{}
		hgMu.Unlock()
	})
}

func r4lv(t *testing.T) []int {
	out := make([]int, 4)
	for i := range out {
		out[i], _ = fanLvl(fmt.Sprintf("fan%d", i))
	}
	return out
}

func r4post(off int) string {
	w := httptest.NewRecorder()
	handleFanPair(w, httptest.NewRequest("POST", "/api/fanpair", bytes.NewBufferString(fmt.Sprintf(`{"offset":%d}`, off))))
	return w.Body.String()
}

// Heat guard: while a board is at 88 °C or more the panel never writes a fan level below what the fans
// run at now. Lowering a positive offset (+20 → +5) while hot pulls fans 2+3 down.
func TestR4HeatGuardPairBNotLoweredWhileHot(t *testing.T) {
	r4fpSetup(t, 20, 70, 70, 90, 90)
	noteHottest(89)
	fpMu.Lock()
	fpOffset = 5 // as handleFanPair stores it
	fpMu.Unlock()
	fanPairRun()
	if lv := r4lv(t); lv[2] < 90 || lv[3] < 90 {
		t.Fatalf("hot (89 °C): fans 2+3 pulled down from 90 to %d/%d", lv[2], lv[3])
	}
}

// Same through the handler: offset back to 0 while hot writes fans 2+3 = fans 0+1 directly.
func TestR4HandlerOffZeroWhileHot(t *testing.T) {
	r4fpSetup(t, 20, 70, 70, 90, 90)
	noteHottest(89)
	r4post(0)
	if lv := r4lv(t); lv[2] < 90 || lv[3] < 90 {
		t.Fatalf("hot (89 °C): offset 0 pulled fans 2+3 down from 90 to %d/%d", lv[2], lv[3])
	}
}

// The handler's "offset 0" write ignores the skips: during the OEM's start-up fan test it writes fans 2+3
// (and clamps them to the raised floor, so they differ from what the test set).
func TestR4HandlerOffZeroDuringFanTest(t *testing.T) {
	r4fpSetup(t, -10, 10, 10, 10, 10) // the fan test's 10 % step
	fanGuardMu.Lock()
	fanGuardSt, fanGuardOK = fanGuardState{Floor: 30}, true
	fanGuardMu.Unlock()
	fanTestRunning = func() bool { return true }
	r4post(0)
	if lv := r4lv(t); lv[2] != 10 || lv[3] != 10 {
		t.Fatalf("fan test running: offset 0 wrote fans 2+3 to %d/%d (the test set 10)", lv[2], lv[3])
	}
}

// Race: a tick that read the old offset before the user set 0 writes pair B after the handler reset it;
// with offset 0 the loop never touches pair B again, so it stays off by the old offset.
func TestR4OffsetStuckAfterRace(t *testing.T) {
	r4fpSetup(t, -20, 60, 60, 40, 40)
	fb := newFakeBackend(t, false)
	fpMu.Lock()
	fpIdleAt = time.Time{} // stale: this tick reads the backend
	fpMu.Unlock()
	fb.onGet = func(n int) {
		if n == 1 {
			r4post(0) // the owner saves 0 while the tick waits on the backend
		}
	}
	fanPairRun()
	time.Sleep(3600 * time.Millisecond) // let any kick finish
	fanPairRun()
	if lv := r4lv(t); lv[2] != 60 || lv[3] != 60 {
		t.Fatalf("offset 0 saved, but fans 2+3 stuck at %d/%d (fans 0+1 %d)", lv[2], lv[3], lv[1])
	}
}

// Idle: "left to the OEM fan control on Idle". The kick after the Idle write uses the idle answer cached up to 30 s
// earlier ("not idle") and applies the offset on Idle anyway.
func TestR4OffsetAppliedRightAfterIdle(t *testing.T) {
	r4fpSetup(t, 10, 60, 60, 70, 70)
	fb := newFakeBackend(t, false)
	fb.mu.Lock()
	fb.set["powerplans"].([]any)[1].(map[string]any)["info"] = "0 MHz 0 V 40 RPM 40 RPM"
	fb.mu.Unlock()
	fpMu.Lock()
	fpIdle, fpIdleAt = false, time.Now() // a fresh "not idle" answer from the loop
	fpMu.Unlock()
	if err := applyIdle(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ { // intminer applies the idle plan's fan level
		setFanLvl(fmt.Sprintf("fan%d", i), 40)
	}
	time.Sleep(3600 * time.Millisecond) // the kick
	if lv := r4lv(t); lv[2] != 40 {
		t.Fatalf("on Idle: fans 2+3 set to %d by the offset (idle plan 40)", lv[2])
	}
}

// Tuner fan hold + negative offset: the watchdog flags fans 2+3 at pair B's level, but the hold asks the
// fan guard about pair A's level, so the guard never raises the floor (the fan loop judges the lower pair).
func TestR4TunerHoldGuardSeesPairB(t *testing.T) {
	r4fpSetup(t, -20, 30, 30, 10, 10)
	old := readWDGuard
	t.Cleanup(func() { readWDGuard = old })
	readWDGuard = func() (watchdogView, bool) {
		return watchdogView{Fans: []int{900, 900, 150, 150}, Duty: []int{30, 30, 10, 10}, Status: 0b1100,
			Flagged: "the watchdog flags fans 2, 3"}, true
	}
	cfg := fastCfg()
	cfg.FanHold, cfg.FanMinPct = true, 30
	e, _ := newSimEngine(cfg, 0, ckey{B: 2, I: 5})
	e.fanFloor, e.fanGuard = fanFloor, newTunerEngine(cfg).fanGuard // the real hooks
	e.applyFan = func(int) error { return nil }
	e.readFan = func() (int, bool) { return 30, true } // the miner runs 30 % (pair B 10 %)
	t.Logf("hold -> %d", e.holdFan(70))
	if fanFloor() <= 10 {
		t.Fatalf("watchdog flagged fans 2+3 at 10 %% (pair B) during a fan hold at 30 %%: floor still %d", fanFloor())
	}
}

// Tuner "Fans: … never below 30 %": with a −20 offset fans 2+3 run at 10 % during the run.
func TestR4TunerFanMinAppliesToPairB(t *testing.T) {
	r4fpSetup(t, -20, 30, 30, 30, 30)
	cfg := fastCfg()
	cfg.FanHold, cfg.FanMinPct, cfg.DryRun = true, 30, false
	tunerMu.Lock()
	tunerCur = &tunerEngine{cfg: cfg}
	tunerMu.Unlock()
	setCharge("tuner")
	t.Cleanup(func() { setCharge(""); tunerMu.Lock(); tunerCur = nil; tunerMu.Unlock() })
	fanPairTick(-20, "")
	if lv := r4lv(t); lv[2] < 30 {
		t.Fatalf("tuner says never below 30 %%: fans 2+3 at %d", lv[2])
	}
}

// Pair A under the floor with a negative offset: target says A, setFanLvl writes the floor, the tick sees
// "off target" and rewrites every second, and the note reports a level the fans aren't at.
func TestR4NoteUnderFloor(t *testing.T) {
	r4fpSetup(t, -5, 8, 8, 10, 10)
	n := fanPairTick(-5, "")
	if lv := r4lv(t); n != fmt.Sprintf("fans 0+1 8 %%, fans 2+3 %d %%", lv[2]) {
		t.Fatalf("note %q but fans 2+3 at %d", n, lv[2])
	}
}

// Positive offset under the heat guard: every plan write takes the highest live fan (pair B), so pair A
// ratchets up by the offset per write (safe direction; documents the behaviour).
func TestR4HeatGuardRatchetPositive(t *testing.T) {
	r4fpSetup(t, 15, 60, 60, 75, 75)
	noteHottest(89)
	if got := heatGuardFan(60); got != 75 {
		t.Fatalf("heatGuardFan(60)=%d", got)
	}
}
