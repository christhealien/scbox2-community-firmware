package main

// Lane C2 (PowerEstimate on the SC Box II): the per-setting estimate is inert with the gate off; uncalibrated it
// shows the wall-meter readings measured on one unit (C2/C7: 392 W at Stock High, 251.8 W at Stock Low, 399.4 /
// 406.9 / 413.5 W at 700 / 720 / 725 MHz), the curve through them elsewhere in 525–750 MHz, nothing outside; the
// owner's wall reading once given, kept on the config store (cfgWrite, with the FAT .bak).

import (
	"math"
	"os"
	"strings"
	"testing"
)

// c2Box2 switches to the Box II profile with PowerEstimate as given, a fresh config dir, a stub miner reading and
// a cold cache.
func c2Box2(t *testing.T, on bool, d dashboard) (dir string, setDash func(dashboard)) {
	t.Helper()
	dir = b10TempAuth(t)
	p := profSCBox2
	p.Feat.PowerEstimate = on
	b10SetModel(t, p)
	oldDash, oldFAT := box2PowerDash, configFAT
	cur := d
	box2PowerDash = func() dashboard { return cur }
	configFAT = true // the Box II config store
	box2PowMu.Lock()
	box2PowCfg, box2PowRead = box2PowerConfig{}, false
	box2PowMu.Unlock()
	t.Cleanup(func() {
		box2PowerDash, configFAT = oldDash, oldFAT
		box2PowMu.Lock()
		box2PowCfg, box2PowRead = box2PowerConfig{}, false
		box2PowMu.Unlock()
	})
	return dir, func(nd dashboard) { cur = nd }
}

// a hashing Box II as the miner API reports it: one board, clock and trim code ("voltage") from devs
func c2Dash(mhz float64, code int, ghs float64) dashboard {
	return dashboard{OK: true, GHS: ghs, GHSAvg: ghs, Elapsed: 3600,
		Boards: []board{{ID: 0, Alive: true, GHS: ghs, ClockMHz: mhz, VoltLevel: code, Fans: []int{3900, 3950}}}}
}

func c2Num(v map[string]any, k string) float64 {
	switch x := v[k].(type) {
	case float64:
		return x
	case int:
		return float64(x)
	}
	return math.NaN()
}

func TestC2PowerBox2OffInert(t *testing.T) {
	dir, _ := c2Box2(t, false, c2Dash(690, 42, 1900))
	v := powerView(c2Dash(690, 42, 1900))
	if a, _ := v["available"].(bool); a {
		t.Fatalf("gate off: no estimate: %v", v)
	}
	if _, ok := v["watts"]; ok {
		t.Fatalf("gate off: no watts: %v", v)
	}
	for _, body := range []any{nil, map[string]any{"cal_watts": 400}, map[string]any{"clear": true}, map[string]any{"mains_v": 230}} {
		m := "POST"
		if body == nil {
			m = "GET"
		}
		if got := b10call(t, handlePower, m, "/api/power", body); !isNotAvailable(got) {
			t.Fatalf("gate off: %s %v must be refused: %v", m, body, got)
		}
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Fatalf("gate off: nothing may be written, found %d files", len(ents))
	}
}

func TestC2PowerBox2MeasuredBeforeCalibration(t *testing.T) {
	c2Box2(t, true, dashboard{})
	v := powerView(c2Dash(690, 42, 1900))
	if c2Num(v, "watts") != 392 || v["source"] != "measured" || v["calibrated"] != false || v["mode"] != "Hashrate" {
		t.Fatalf("690/42 uncalibrated: the 392 W measured on one unit (not the OEM's 400 W): %v", v)
	}
	if n, _ := v["source_note"].(string); !strings.Contains(n, "one unit") {
		t.Fatalf("an uncalibrated figure says it was measured on one unit: %q", n)
	}
	if j := c2Num(v, "jth"); j != 206 { // 392 W / 1.9 TH/s
		t.Fatalf("J/TH = W / reported TH/s: %v", j)
	}
	v = powerView(c2Dash(525, 61, 1450))
	if c2Num(v, "watts") != 251.8 || v["source"] != "measured" || v["mode"] != "Low-power" {
		t.Fatalf("525/61 uncalibrated: 251.8 W measured (not the OEM's 260 W): %v", v)
	}
	// the overclock readings (C7) are shown at their own settings too
	for mhz, w := range map[int]float64{700: 399.4, 720: 406.9, 725: 413.5} {
		if v := powerView(c2Dash(float64(mhz), 42, 2000)); c2Num(v, "watts") != w || v["source"] != "measured" || v["mode"] != nil {
			t.Fatalf("%d/42: %v W measured: %v", mhz, w, v)
		}
	}
	// the SC Lite model's inputs never appear
	for _, k := range []string{"pv", "pv_eff", "held_v"} {
		if _, ok := v[k]; ok {
			t.Fatalf("Box II view carries SC Lite field %q: %v", k, v)
		}
	}
}

// The curve W = k × the chips' clock × V² (k fitted at Stock High) goes through every reading on the first unit
// within 1 W, so it is what fills the settings between and past them; outside 525–750 MHz and the stock levels'
// interval there is no figure.
func TestC2PowerBox2EstimateCurve(t *testing.T) {
	c2Box2(t, true, dashboard{})
	curve := func(mhz, lvl int) float64 {
		v, _ := box2Volts(lvl)
		return box2PowerK * box2PLLClock(float64(mhz)) * v * v
	}
	for _, p := range box2PowerPoints {
		if d := math.Abs(curve(p.MHz, p.V) - p.Watts); d > 1 {
			t.Fatalf("the curve misses the %d MHz / %s reading (%.1f W) by %.1f W", p.MHz, box2LevelText(p.V), p.Watts, d)
		}
	}
	for _, c := range []struct {
		mhz, code int
		want      float64
	}{
		{600, 50, 317.8}, // 600 MHz at ≈ 7.39 V
		{690, 50, 364.1}, // Stock High's clock, a lower voltage: less
		{525, 42, 299.4}, // Stock Low's clock at 7.67 V: more than Stock Low
		{712, 42, 399.1}, // 712 → 700 on the chips: the 700 MHz figure's curve value
		{740, 42, 420.5}, // 737.5 on the chips: past the readings (extrapolated)
		{750, 42, 427.6}, // the probe's top: under its 460 W limit
	} {
		v := powerView(c2Dash(float64(c.mhz), c.code, 1700))
		if w := c2Num(v, "watts"); math.Abs(w-c.want) > 0.11 || v["source"] != "estimate" || v["calibrated"] != false {
			t.Fatalf("%d/%d: want ≈ %.1f W, an estimate: %v", c.mhz, c.code, c.want, v)
		}
	}
	// monotone: more clock or more voltage never shows fewer watts
	for lvl := 42; lvl <= 61; lvl++ {
		prev := 0.0
		for mhz := 525; mhz <= 750; mhz += 5 {
			w, _, _, ok := box2EstimateWatts(mhz, lvl)
			if !ok || w+0.5 < prev { // (a reading may sit up to 0.5 W off the curve)
				t.Fatalf("%d MHz / %s: %.1f W after %.1f W (ok %v)", mhz, box2LevelText(lvl), w, prev, ok)
			}
			prev = max(prev, w)
		}
	}
	for _, s := range []struct{ mhz, code int }{{500, 42}, {760, 42}, {690, 40}, {690, 62}} {
		v := powerView(c2Dash(float64(s.mhz), s.code, 1700))
		if _, ok := v["watts"]; ok {
			t.Fatalf("%d/%d: outside the measured window, no figure: %v", s.mhz, s.code, v)
		}
		if r, _ := v["reason"].(string); !strings.Contains(r, "not estimated for this setting") {
			t.Fatalf("%d/%d: reason %q", s.mhz, s.code, r)
		}
	}
	// not hashing, or no trim code reported: no figure
	if _, ok := powerView(dashboard{OK: true})["watts"]; ok {
		t.Fatal("not hashing: no figure")
	}
	d := c2Dash(690, 0, 1900)
	if _, ok := powerView(d)["watts"]; ok {
		t.Fatal("no trim code: no figure")
	}
}

func TestC2PowerBox2CalibratePersist(t *testing.T) {
	dir, setDash := c2Box2(t, true, c2Dash(690, 42, 1850))
	got := b10call(t, handlePower, "POST", "/api/power", map[string]any{"cal_watts": 412})
	if ok, _ := got["ok"].(bool); !ok || c2Num(got, "watts") != 412 || got["calibrated"] != true || got["source"] != "meter" {
		t.Fatalf("calibrate at 690/42: %v", got)
	}
	path := box2PowerPath()
	if _, err := os.Stat(path); err != nil || !strings.HasPrefix(path, dir) {
		t.Fatalf("reading not stored in the config dir: %v", err)
	}
	// the other mode keeps the figure measured on one unit until it gets its own reading
	if v := powerView(c2Dash(525, 61, 1450)); c2Num(v, "watts") != 251.8 || v["source"] != "measured" {
		t.Fatalf("525/61 must stay the one-unit figure: %v", v)
	}
	setDash(c2Dash(525, 61, 1440))
	if got := b10call(t, handlePower, "POST", "/api/power", map[string]any{"cal_watts": 268, "mains_v": 230}); got["ok"] != true {
		t.Fatalf("calibrate 525/61: %v", got)
	}
	// second write on FAT keeps a .bak (cfgWrite → fatWrite)
	if _, err := os.Stat(path + ".bak"); err != nil {
		t.Fatalf("FAT write must keep a .bak: %v", err)
	}
	// a restart: cold cache, read back from the file
	box2PowMu.Lock()
	box2PowRead = false
	box2PowMu.Unlock()
	c := getBox2PowerConfig()
	if len(c.Cals) != 2 || c.MainsV != 230 {
		t.Fatalf("not kept: %+v", c)
	}
	if k, i := c.find(690, 42); i < 0 || k.Watts != 412 || k.GHS != 1850 || k.FanRPM != 3925 {
		t.Fatalf("690/42 reading: %+v", k)
	}
	v := powerView(c2Dash(690, 42, 2000))
	if c2Num(v, "watts") != 412 || c2Num(v, "jth") != 206 || c2Num(v, "amps") != round1(412.0/230) {
		t.Fatalf("after restart: %v", v)
	}
	// a power cut corrupted the file: the .bak (one reading) is used, not nothing
	os.WriteFile(path, []byte("{\"cals\":[{\"mhz\":69"), 0o644)
	box2PowMu.Lock()
	box2PowRead = false
	box2PowMu.Unlock()
	if c := getBox2PowerConfig(); len(c.Cals) != 1 || c.Cals[0].Watts != 412 {
		t.Fatalf(".bak fallback: %+v", c)
	}
}

func TestC2PowerBox2NoHashrateScaling(t *testing.T) {
	c2Box2(t, true, dashboard{})
	full, half := powerView(c2Dash(690, 42, 1900)), powerView(c2Dash(690, 42, 950))
	if c2Num(full, "watts") != c2Num(half, "watts") {
		t.Fatalf("watts must not follow hashrate: %v vs %v", full["watts"], half["watts"])
	}
	if c2Num(half, "jth") != 413 { // 392 / 0.95
		t.Fatalf("J/TH follows the reported hashrate: %v", half["jth"])
	}
	if half["low_hash"] != true || full["low_hash"] != nil {
		t.Fatalf("low hashrate is flagged, not scaled: full %v half %v", full["low_hash"], half["low_hash"])
	}
}

func TestC2PowerBox2Refusals(t *testing.T) {
	dir, setDash := c2Box2(t, true, c2Dash(690, 42, 1900))
	for _, w := range []float64{20, 5000, 1000, 150} { // out of range; far from the 392 W measured on one unit
		if got := b10call(t, handlePower, "POST", "/api/power", map[string]any{"cal_watts": w}); got["ok"] == true {
			t.Fatalf("%v W at 690/42 must be refused: %v", w, got)
		}
	}
	setDash(dashboard{OK: true})
	if got := b10call(t, handlePower, "POST", "/api/power", map[string]any{"cal_watts": 400}); got["ok"] == true {
		t.Fatalf("not hashing: refused: %v", got)
	}
	if _, err := os.Stat(box2PowerPath()); err == nil {
		t.Fatalf("a refused calibration wrote %s (dir %s)", box2PowerPath(), dir)
	}
	// a non-stock setting can be calibrated (an exact reading there, not an extrapolation)
	setDash(c2Dash(600, 50, 1650))
	if got := b10call(t, handlePower, "POST", "/api/power", map[string]any{"cal_watts": 330}); got["ok"] != true || c2Num(got, "watts") != 330 {
		t.Fatalf("calibrate 600/50: %v", got)
	}
	// clear one, then all
	if got := b10call(t, handlePower, "POST", "/api/power", map[string]any{"clear_mhz": 600, "clear_v": 50}); got["ok"] != true {
		t.Fatalf("clear one: %v", got)
	}
	if v := powerView(c2Dash(600, 50, 1650)); c2Num(v, "watts") != 317.8 || v["source"] != "estimate" {
		t.Fatalf("a cleared setting goes back to the estimate: %v", v)
	}
	setDash(c2Dash(690, 42, 1900))
	b10call(t, handlePower, "POST", "/api/power", map[string]any{"cal_watts": 405})
	b10call(t, handlePower, "POST", "/api/power", map[string]any{"clear": true})
	if v := powerView(c2Dash(690, 42, 1900)); c2Num(v, "watts") != 392 || v["source"] != "measured" {
		t.Fatalf("clear all: back to the one-unit figure: %v", v)
	}
	// far from the estimate at a non-measured setting: refused too
	setDash(c2Dash(600, 50, 1650))
	if got := b10call(t, handlePower, "POST", "/api/power", map[string]any{"cal_watts": 700}); got["ok"] == true {
		t.Fatalf("700 W at 600 MHz (≈ 318 W estimated): refused: %v", got)
	}
}

// The probe's power limit reads box2UnitWatts: the owner's meter reading at that setting, else the estimate
// scaled by how this unit's Stock High reading compares with the first unit's.
func TestC2PowerBox2UnitWattsForLimit(t *testing.T) {
	_, setDash := c2Box2(t, true, c2Dash(690, 42, 1950))
	if w, src := box2UnitWatts(750, 42); math.Abs(w-427.6) > 0.11 || src != "estimate" {
		t.Fatalf("750/42 uncalibrated: %.1f W (%s)", w, src)
	}
	if got := b10call(t, handlePower, "POST", "/api/power", map[string]any{"cal_watts": 431.2}); got["ok"] != true { // 10 % over 392
		t.Fatalf("calibrate 690/42: %v", got)
	}
	if w, src := box2UnitWatts(750, 42); math.Abs(w-427.6*1.1) > 0.2 || !strings.Contains(src, "scaled") || w <= ceilBox2PowerMaxW {
		t.Fatalf("750/42 on a unit drawing 10 %% more at Stock High: %.1f W (%s), over the %.0f W limit", w, src, ceilBox2PowerMaxW)
	}
	setDash(c2Dash(725, 42, 1980))
	b10call(t, handlePower, "POST", "/api/power", map[string]any{"cal_watts": 420})
	if w, src := box2UnitWatts(725, 42); w != 420 || !strings.Contains(src, "meter") {
		t.Fatalf("725/42 with its own reading: %.1f W (%s)", w, src)
	}
}

// The SC Lite path is untouched: its view has no Box II fields, and its model is unchanged.
func TestC2PowerSCLiteUnchanged(t *testing.T) {
	b10TempAuth(t)
	b10SetModel(t, profSCLite)
	v := powerView(dashboard{OK: true, GHS: 0})
	if _, ok := v["model"]; ok {
		t.Fatalf("SC Lite view must not be the Box II one: %v", v)
	}
	if math.Abs(modelWatts(625, 9400)-899) > 0.01 {
		t.Fatal("SC Lite model changed")
	}
	if _, err := os.Stat(box2PowerPath()); err == nil {
		t.Fatal("SC Lite must never write the Box II file")
	}
}
