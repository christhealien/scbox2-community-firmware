package main

import (
	"encoding/json"
	"math/rand"
	"os"
	"testing"
)

// writes Go targetFan (smoothing off, floor 10) for random valid curves + built-ins; preview_cmp.js compares the page's JS
func TestR4C_PreviewDump(t *testing.T) {
	out := os.Getenv("R4C_PREVIEW_OUT")
	if out == "" {
		t.Skip("set R4C_PREVIEW_OUT")
	}
	fanGuardClear()
	type row struct {
		Mode string       `json:"mode"`
		Pts  []curvePoint `json:"pts"`
		T    []float64    `json:"t"`
		F    []int        `json:"f"`
	}
	var rows []row
	r := rand.New(rand.NewSource(1))
	add := func(c fanConfig, mode string, pts []curvePoint) {
		rw := row{Mode: mode, Pts: pts}
		for tt := 20.0; tt <= 100; tt += 0.25 {
			rw.T = append(rw.T, tt)
			rw.F = append(rw.F, targetFan(c, tt))
		}
		rows = append(rows, rw)
	}
	for _, k := range builtinOrder {
		c := defaultFanConfig()
		c.Profile, c.Smoothing = k, 0
		add(c, curveMode(c), builtinCurves[k])
	}
	for i := 0; i < 300; i++ {
		n := 2 + r.Intn(7)
		var pts []curvePoint
		t0, f0 := 20+r.Intn(30), 10+r.Intn(40)
		for j := 0; j < n; j++ {
			pts = append(pts, curvePoint{Temp: float64(t0), Fan: float64(f0)})
			t0 += 1 + r.Intn(8)
			f0 = min(100, f0+r.Intn(20))
			if t0 > 100 {
				break
			}
		}
		if len(pts) < 2 {
			continue
		}
		c := defaultFanConfig()
		c.Profile, c.Points, c.Smoothing = "custom", pts, 0
		if validateFanConfig(c) != nil {
			continue
		}
		add(c, "curve", pts)
	}
	b, _ := json.Marshal(rows)
	os.WriteFile(out, b, 0o644)
}
