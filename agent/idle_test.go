package main

import (
	"encoding/json"
	"testing"
)

func TestStockPlansIdlePosition(t *testing.T) {
	// the SC Lite's own setting: the idle plan is the 2nd entry but has level 3
	var flat map[string]any
	json.Unmarshal([]byte(`{"select":0,"manual":true,"powerplans":[{"info":"615 MHz 9100 V 40 RPM 40 RPM PV 9400","level":0},{"info":"0 MHz 0 V 100 RPM 100 RPM","level":3}]}`), &flat)
	if i := idleLevel(flat); i != 1 {
		t.Fatalf("idle position %d, want 1 (the list position, not level 3)", i)
	}
	if pp, ok := stockPlanAt(flat, 0); !ok || pp.FreqMHz != 615 {
		t.Fatalf("plan 0: %+v", pp)
	}
	if _, ok := stockPlanAt(flat, 3); ok {
		t.Fatal("position 3 doesn't exist")
	}
	// idle selected, manual off: the running plan is idle, and "stock" is still the hashing plan
	flat["select"], flat["manual"] = float64(1), false
	if pp, ok := resolveRunningPlan(flat); !ok || pp.FreqMHz != 0 {
		t.Fatalf("running while idle: %+v", pp)
	}
	if i := firstHashingPlan(flat); i != 0 {
		t.Fatalf("first hashing plan %d", i)
	}
	// firmware that nests plans per algorithm: the current algorithm's modes
	var nested map[string]any
	json.Unmarshal([]byte(`{"algoname":"blake2b","select":0,"powerplans":[
		{"algo":"other","mode":[{"info":"0 MHz 0 V 100 RPM 100 RPM","level":0}]},
		{"algo":"blake2b","mode":[{"info":"700 MHz 9200 V 40 RPM 40 RPM PV 9500","level":0},{"info":"600 MHz 8800 V 40 RPM 40 RPM PV 9100","level":1},{"info":"0 MHz 0 V 100 RPM 100 RPM","level":3}]}]}`), &nested)
	if n := len(stockPlans(nested)); n != 3 {
		t.Fatalf("nested: %d plans", n)
	}
	if i := idleLevel(nested); i != 2 {
		t.Fatalf("nested idle position %d", i)
	}
	if pp, ok := resolveRunningPlan(nested); !ok || pp.FreqMHz != 700 {
		t.Fatalf("nested running: %+v", pp)
	}
}

func TestMinerIdle(t *testing.T) {
	var cur map[string]any
	json.Unmarshal([]byte(`{"select":1,"manual":false,"powerplans":[{"info":"615 MHz 9100 V 40 RPM 40 RPM PV 9400","level":0},{"info":"0 MHz 0 V 100 RPM 100 RPM","level":3}]}`), &cur)
	if !minerIdle(cur) {
		t.Fatal("idle plan selected: idle")
	}
	cur["select"] = float64(0)
	if minerIdle(cur) {
		t.Fatal("stock plan: not idle")
	}
	cur["manual"], cur["manualPowerplan"] = true, "650 MHz 9200 V 50 RPM 50 RPM PV 9500"
	if minerIdle(cur) {
		t.Fatal("manual hashing plan: not idle")
	}
}
