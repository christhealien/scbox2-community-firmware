package main

// r7-code F7 (needs the fix: trimCheckBlamesChip).

import "testing"

// F7: trimCheck's "is this fail about the lifted chip?" compares the miner's chip index with the hook's chip
// number unmapped (and has its own copy of the pair rule). With icinfo numbered 0..45 (as every fake-engine test
// models it) the lifted chip's own fail reads as "another chip's".
func TestR7TrimCheckBlamesMapsNumbering(t *testing.T) {
	e, _ := newSimEngine(fastCfg(), 0, ckey{2, 10})
	e.hookStats = cwReadStats
	newCwSim(t, func(int, float64) float64 { return 0 })
	e.mu.Lock()
	e.baseRate = map[ckey]float64{}
	for i := 0; i < 46; i++ {
		e.baseRate[ckey{2, i}] = 0
	}
	e.mu.Unlock()
	// hook chip 11 is icinfo 10 here
	if !e.trimCheckBlamesChip([2]int{2, 11}, ckey{2, 10}) {
		t.Fatalf("the lifted chip's own fail wasn't recognised")
	}
	if !e.trimCheckBlamesChip([2]int{2, 11}, ckey{2, 11}) {
		t.Fatalf("its partner's fail wasn't recognised")
	}
	if e.trimCheckBlamesChip([2]int{2, 11}, ckey{2, 12}) {
		t.Fatalf("chip 13 isn't in 11's stage")
	}
}
