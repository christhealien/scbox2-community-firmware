package main

import (
	"strings"
	"testing"
	"time"
)

// Both chips of pair 11+12 are bad and a trim fixes each. Auto-trim slows one of them and leaves its partner:
// one trim per pair (the partner gets the voltage the slowed chip gives up).
func TestChipWatchOneTrimPerPair(t *testing.T) {
	s := newCwSim(t, func(chip int, trim float64) float64 {
		if (chip == 11 || chip == 12) && trim == 0 {
			return 15
		}
		return 1
	})
	on := true
	chipWatchSet(&on, false)
	s.run(12 * time.Hour)
	tr := s.trims()
	if (tr[11] < 0) == (tr[12] < 0) {
		t.Fatalf("want exactly one of the pair slowed: %v", tr)
	}
	log := ""
	for _, l := range chipWatchView()["log"].([]map[string]any) {
		log += l["msg"].(string) + "\n"
	}
	if !strings.Contains(log, "one trim per pair") {
		t.Fatalf("no note about the partner:\n%s", log)
	}
}

// The untrimmed chips' errors rise after a trim (it's costing them voltage): no new trims on that board.
func TestChipWatchTrimBudget(t *testing.T) {
	s := newCwSim(t, nil)
	s.rate = func(chip int, trim float64) float64 {
		tr := s.trims()
		slowed := 0
		for _, v := range tr {
			if v < 0 {
				slowed++
			}
		}
		if trim < 0 {
			return 1
		}
		base := 1.0 + 2*float64(slowed) // every slowed chip takes voltage from the rest
		if chip == 5 || chip == 17 || chip == 29 || chip == 41 {
			return 30
		}
		return base
	}
	on := true
	chipWatchSet(&on, false)
	s.run(16 * time.Hour)
	n := 0
	for _, v := range s.trims() {
		if v < 0 {
			n++
		}
	}
	log := ""
	for _, l := range chipWatchView()["log"].([]map[string]any) {
		log += l["msg"].(string) + "\n"
	}
	if !strings.Contains(log, "no new trims on this board for a day") || n >= 4 {
		t.Fatalf("budget not applied: %d trims\n%s", n, log)
	}
}

// The tuner won't slow a chip whose partner is slowed; it raises PV instead.
func TestTunerOneTrimPerPair(t *testing.T) {
	e, _ := newSimEngine(fastCfg(), 0, ckey{2, 5})
	e.hookStats = func() (chipStatsFile, bool) {
		var f chipStatsFile
		f.Chains = append(f.Chains, struct {
			Chain   int   `json:"chain"`
			Chips   int   `json:"chips"`
			Base10  int   `json:"base10"`
			Clock10 []int `json:"clock10"`
			Nonces  []int `json:"nonces"`
		}{Chain: 2, Chips: 46, Base10: 6250})
		return f, true
	}
	e.baseRate = map[ckey]float64{}
	for i := 0; i < 46; i++ {
		e.baseRate[ckey{2, i}] = 0.1
	}
	e.trimGet = func() map[[2]int]float64 { return map[[2]int]float64{{2, 5}: -12.5} }
	set := false
	e.trimSet = func(b, c int, m float64) error { set = true; return nil }
	var made []madeTrim
	_, why := e.trimOne(ckey{2, 5}, &made) // icinfo 5 = hook chip 6, partner of chip 5
	if set || !strings.Contains(why, "one trim per pair") {
		t.Fatalf("partner rule: set %v, why %q", set, why)
	}
}
