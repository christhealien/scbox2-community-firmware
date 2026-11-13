package main

import (
	"strings"
	"testing"
)

// The Dashboard's idle note says when the thermal guard put the miner there (and why).
func TestR5HeatIdleReasonText(t *testing.T) {
	heatIdleWhy.Store("overheat: still 89.0 °C (abort 88 °C) after 1m30s with the fans at 100 %")
	if s := heatIdleWhyNow(); !strings.Contains(s, "89.0 °C") {
		t.Errorf("why: %q", s)
	}
}
