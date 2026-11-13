package main

import "testing"

// R2-ops: a second "Find strong chips" on a board whose first run kept chip 1 at +12.5 and chip 13 at +50:
// chips 2 and 14 share those chips' series stages, so raising them pulls a stage down that a "+" chip is
// already pulling (what the one-chip-per-stage rule is there to avoid).
func TestR2OpsStrongCandidatesSkipStageOfPlusChip(t *testing.T) {
	dErr, dNon := map[int]int{}, map[int]int{}
	for i := 1; i <= 46; i++ {
		dErr[i], dNon[i] = 0, 1000
	}
	dNon[2], dNon[14] = 1100, 1100 // best by nonces: picked first if allowed
	orig := []chipTrim{{0, 1, 12.5}, {0, 13, 50}}
	for _, c := range sctCandidates(46, dErr, dNon, orig, 0) {
		if c.Chip == 2 || c.Chip == 14 {
			t.Errorf("c%d picked: its stage partner c%d already runs faster (hand trim)", c.Chip, c.Chip-1)
		}
	}
}
