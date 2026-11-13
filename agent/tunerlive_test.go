package main

import (
	"testing"
	"time"
)

// The live view: while a new setting settles the chip list is empty (not the previous test's), and while a
// test is watched it shows this test's counts, poll by poll. Each finished test keeps its own chip map.
func TestTunerLiveViewAndChipMaps(t *testing.T) {
	cfg := fastCfg()
	cfg.HighestMHz = 675                        // several clocks, so clean tests follow a failed one
	cfg.SettleMin = 1                           // and each new setting settles
	e, _ := newSimEngine(cfg, 9100, ckey{2, 5}) // chip 5 gains errors below 9100 mV: those tests fail
	inner := e.sleep
	var settlingWithChips, liveBad, liveGood, stale int
	e.sleep = func(d time.Duration) {
		inner(d)
		v := e.statusView()
		cur, _ := v["cur"].(*curStep)
		chips, _ := v["chips"].([]map[string]any)
		if cur == nil || !v["running"].(bool) {
			return
		}
		if cur.Settling {
			if len(chips) > 0 {
				settlingWithChips++
			}
			return
		}
		if cur.Phase == "baseline" || len(chips) == 0 {
			return
		}
		g5 := 0
		for _, c := range chips {
			if c["b"] == 2 && c["i"] == 5 {
				g5 = c["gained"].(int)
			}
		}
		if cur.MV < 9100 { // chip 5 is failing at this voltage
			if g5 > 0 {
				liveBad++
			}
		} else {
			liveGood++
			if g5 > 0 { // chip 5 is clean here: errors on it are the previous (failed) test's
				stale++
			}
		}
	}
	e.Run()
	if settlingWithChips != 0 {
		t.Fatalf("%d polls showed chip counts while a new setting was settling (the previous test's)", settlingWithChips)
	}
	failed := 0
	for _, r := range e.results {
		if r.Result == "failed" {
			failed++
		}
	}
	if failed == 0 || liveGood == 0 {
		t.Fatalf("expected failed tests and live polls of clean ones (failed %d, clean polls %d)", failed, liveGood)
	}
	_ = liveBad
	if stale != 0 {
		t.Fatalf("%d polls of a clean test showed the previous test's errors", stale)
	}
	_, maps := e.chipMaps()
	if len(maps) == 0 || len(maps) != len(e.results) {
		t.Fatalf("%d chip maps for %d finished tests", len(maps), len(e.results))
	}
	failedWithOver := false
	for i, m := range maps {
		if m.N != i || len(m.Chips) != 46 || m.MHz != e.results[i].MHz || m.MV != e.results[i].MV || m.Result != e.results[i].Result {
			t.Fatalf("map %d doesn't match its test: %+v vs %+v", i, m, e.results[i])
		}
		if m.Result == "failed" {
			for _, c := range m.Chips {
				if c[1] == 5 && c[2] > c[3] {
					failedWithOver = true
				}
			}
		}
	}
	if !failedWithOver {
		t.Fatal("a failed test's map should show chip 5 over its limit")
	}
}

func TestAliveBoards(t *testing.T) {
	if m := aliveBoards([]icBoardStatus{{1, ""}, {1, ""}, {0, "Alive"}, {1, ""}}); len(m) != 1 || !m[2] {
		t.Fatalf("alive: %v", m)
	}
	if m := aliveBoards([]icBoardStatus{{1, ""}, {1, ""}}); m != nil {
		t.Fatal("no board reported running: no filter (every slot with chip data counts)")
	}
	if m := aliveBoards(nil); m != nil {
		t.Fatal("no table: no filter")
	}
}
