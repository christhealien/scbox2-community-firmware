package main

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// Sensitivity (sigma) has no range check: a typo like 50 is accepted and makes every chip's limit 100000,
// so no test can fail on chip errors.
func TestUISigmaOutOfRangeRejected(t *testing.T) {
	if lim := tunerLimitVal(60, 25.0/60, 1, false, 1.5, 2, 46, 50); lim < 10000 {
		t.Logf("limit at sigma 50: %v", lim)
	} else {
		t.Logf("limit at sigma 50: %v (per-chip judging is off)", lim)
	}
	for _, s := range []float64{0, -3, 50} {
		c := defaultTunerConfig()
		c.Sigma = s
		if err := validateTunerConfig(c); err == nil {
			t.Errorf("sigma %v accepted", s)
		}
	}
}

// A valid PLL clock typed with its .5 (587.5) can't be decoded into the int field: the field stays 0 and
// the owner is told "lowest clock must be 300–700 MHz".
func TestUIHalfClockMessage(t *testing.T) {
	c := defaultTunerConfig()
	b, _ := json.Marshal(map[string]any{"action": "start", "config": c})
	body := strings.Replace(string(b), `"lowest_mhz":575`, `"lowest_mhz":587.5`, 1)
	rr := httptest.NewRecorder()
	handleTuner(rr, httptest.NewRequest("POST", "/api/tuner", strings.NewReader(body)))
	if s := rr.Body.String(); !strings.Contains(s, "whole MHz") || strings.Contains(s, "300") {
		t.Fatalf("587.5 MHz: %s", s)
	}
}
