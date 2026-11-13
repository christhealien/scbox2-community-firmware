package main

import (
	"strings"
	"testing"
	"time"
)

// A board that sits right at the abort temperature with the fans already at 100 % reads a little either side
// of it (sensor noise ±0.3 °C). One reading 0.1 °C under the abort temperature must not wipe the episode:
// otherwise the 90 s escalation to Idle never comes and the fans flap between the abort and the curve.
func TestR3HeatEscHoverAtAbort(t *testing.T) {
	t0 := time.Date(2026, 11, 6, 2, 0, 0, 0, time.UTC)
	var h heatEscState
	idle := false
	// every 10 s: 88.2, 88.1, 87.9, 88.2, 88.1, 87.9 … (fans at 100 % from the first abort on)
	seq := []float64{88.2, 88.1, 87.9}
	for i := 0; i < 60 && !idle; i++ {
		_, idle, _ = h.step(seq[i%3], 88, t0.Add(time.Duration(i)*10*time.Second), i >= 2)
	}
	if !idle {
		t.Fatal("10 minutes at 87.9–88.2 °C with the fans at 100 %: never went to Idle (one reading 0.1 °C under the abort temperature restarts the episode)")
	}
}

// The voltage-curve plan row must describe the walk the run really does: the default walks up only.
func TestR3BottomUpPlanDesc(t *testing.T) {
	c := scfg()
	c.TopDown = false
	e := newTunerEngine(c)
	e.buildSteps(planMV{MHz: 615, MV: 9100, PV: 9400}, "615 MHz")
	for _, s := range e.steps {
		if s.Key == "curve" {
			if strings.Contains(s.Desc, "then down until it fails") || strings.Contains(s.Desc, "starts at the baseline voltage") {
				t.Fatalf("bottom-up run, plan says the old walk: %s", s.Desc)
			}
			return
		}
	}
	t.Fatal("no curve step")
}
