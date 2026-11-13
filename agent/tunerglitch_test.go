package main

import (
	"strings"
	"testing"
)

// r3-heat: the tuner's own heat abort acts on ONE reading. The owner's log has a single 95.9 °C board-sensor
// sample (67.9 before and after) at the moment a tuner step was applied: that one sample ends the whole run.
func TestTunerAbortOnSingleGlitch(t *testing.T) {
	e, s := newSimEngine(fastCfg(), 0, ckey{2, 5}) // never fails
	n := 0
	e.readTemp = func() (float64, bool) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.vc.Sub(s.t0).Minutes() > 1.2 && n == 0 {
			n++
			return 95.9, true // one sample
		}
		return 67.9, true
	}
	e.Run()
	for _, l := range e.logbuf {
		if strings.Contains(l, "95.9") {
			t.Fatalf("one glitched reading ended the run: %q", l)
		}
	}
}
