package main

// L12-fans (sweep22): fan curve vs the OEM's start-up fan test, and the watchdog fan guard with a fan pair
// offset (fans 2+3 run at another level than fans 0+1).

import (
	"strings"
	"testing"
	"time"
)

// With a positive offset fans 2+3 run above fans 0+1. A slow fan 2 flagged at its own level (35 %) while
// fans 0+1 run at 25 %: the guard must raise the floor (to 35 %: fans 0+1 at 35, fans 2+3 at 45). It
// compared the flagged fan's duty with fans 0+1's level only, so it never raised it, and the watchdog went
// on stopping mining until the fan lock locked it.
func TestL12FanGuardPositiveOffsetFlagOnPairB(t *testing.T) {
	r4fpSetup(t, 10, 25, 25, 35, 35)
	old := readWDGuard
	t.Cleanup(func() { readWDGuard = old })
	readWDGuard = func() (watchdogView, bool) {
		return watchdogView{Fans: []int{700, 700, 200, 520}, Duty: []int{25, 25, 35, 35}, Status: 4,
			Flagged: "the watchdog flags fan 2 (200 RPM)"}, true
	}
	if f := fanGuardPair(25); f != 35 || fanFloor() != 35 {
		t.Fatalf("fan 2 flagged at its own 35 %% (fans 0+1 at 25 %%, offset +10): guard %d, floor %d; want 35", f, fanFloor())
	}
	// and the pair B level that follows is above the level that tripped
	if b := fanPairTarget(fanFloor(), 10, pairFloor()); b <= 35 {
		t.Fatalf("fans 2+3 at %d %%: not above the 35 %% that was flagged", b)
	}
}

// With a negative offset the guard judged every flag at pair B's (lower) level: a fan of pair A flagged at
// pair A's own level was never seen.
func TestL12FanGuardNegativeOffsetFlagOnPairA(t *testing.T) {
	r4fpSetup(t, -10, 35, 35, 25, 25)
	old := readWDGuard
	t.Cleanup(func() { readWDGuard = old })
	readWDGuard = func() (watchdogView, bool) {
		return watchdogView{Fans: []int{200, 800, 600, 600}, Duty: []int{35, 35, 25, 25}, Status: 1,
			Flagged: "the watchdog flags fan 0 (200 RPM)"}, true
	}
	if f := fanGuardPair(35); f != 45 || fanFloor() != 45 {
		t.Fatalf("fan 0 flagged at its own 35 %% (offset -10): guard %d, floor %d; want 45", f, fanFloor())
	}
}

// The OEM's start-up fan test (xminerd's first 90 s, or a re-run) records each fan's speed at each level:
// the fan curve must not write a fan level meanwhile (as the pair offset already doesn't). Its 100 % for
// heat still goes through.
func TestL12CurveWaitsForFanTest(t *testing.T) {
	m := newHLMiner(t)
	oldFT := fanTestRunning
	fanTestRunning = func() bool { return true }
	t.Cleanup(func() { fanTestRunning = oldFT })
	c := hlCfg()
	m.setTemps(normal, normal, normal, normal)
	m.tick(c, 10*time.Second)
	m.tick(c, 50*time.Second) // past the cooldown too
	if n := m.count("plan"); n != 0 {
		t.Fatalf("the fan curve wrote %d fan plan(s) during the OEM's start-up fan test: %+v", n, m.putsCopy())
	}
	fanMu.RLock()
	note, tgt := fanSt.Note, fanSt.Target
	fanMu.RUnlock()
	if tgt != -1 || !strings.Contains(note, "fan test") {
		t.Fatalf("status during the fan test: target %d, note %q", tgt, note)
	}
	// over the abort temperature: fans 100 % all the same
	hot := [3]float64{89, 89, 89.5}
	m.setTemps(hot, normal, normal, normal)
	m.tick(c, 10*time.Second)
	m.tick(c, 10*time.Second)
	ps := m.putsCopy()
	if len(ps) == 0 || !strings.Contains(ps[len(ps)-1].plan, "100 RPM 100 RPM") {
		t.Fatalf("no 100 %% write for heat during the fan test: %+v", ps)
	}
	// the fan test over: the curve writes again at the next reading
	fanTestRunning = func() bool { return false }
	m.setTemps(normal, normal, normal, normal)
	n0 := m.count("plan")
	for i := 0; i < 3; i++ {
		m.tick(c, 10*time.Second)
	}
	if m.count("plan") == n0 {
		t.Fatalf("the curve didn't take the fans back after the fan test: %+v", m.putsCopy())
	}
}
