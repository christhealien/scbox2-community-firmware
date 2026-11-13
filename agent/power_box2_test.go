package main

// Lane C2 (PowerEstimate on the SC Box II): the per-setting estimate is inert with the gate off, shows Goldshell's
// figure for an uncalibrated stock mode, nothing for any other setting, the owner's wall reading once given, and
// keeps that reading on the config store (cfgWrite, with the FAT .bak).

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

func TestC2PowerBox2SpecBeforeCalibration(t *testing.T) {
	c2Box2(t, true, dashboard{})
	v := powerView(c2Dash(690, 42, 1900))
	if c2Num(v, "watts") != 400 || v["source"] != "spec" || v["calibrated"] != false || v["mode"] != "Hashrate" {
		t.Fatalf("690/42 uncalibrated: Goldshell's 400 W, marked spec: %v", v)
	}
	if j := c2Num(v, "jth"); j != 211 { // 400 W / 1.9 TH/s
		t.Fatalf("J/TH = W / reported TH/s: %v", j)
	}
	v = powerView(c2Dash(525, 61, 1450))
	if c2Num(v, "watts") != 260 || v["source"] != "spec" || v["mode"] != "Low-power" {
		t.Fatalf("525/61 uncalibrated: 260 W spec: %v", v)
	}
	// the SC Lite model's inputs never appear
	for _, k := range []string{"pv", "pv_eff", "held_v"} {
		if _, ok := v[k]; ok {
			t.Fatalf("Box II view carries SC Lite field %q: %v", k, v)
		}
	}
}

func TestC2PowerBox2OtherSettingNoNumber(t *testing.T) {
	c2Box2(t, true, dashboard{})
	for _, s := range []struct{ mhz, code int }{{600, 50}, {690, 50}, {525, 42}, {750, 42}} {
		v := powerView(c2Dash(float64(s.mhz), s.code, 1700))
		if _, ok := v["watts"]; ok {
			t.Fatalf("%d/%d: no extrapolated/interpolated figure: %v", s.mhz, s.code, v)
		}
		if r, _ := v["reason"].(string); !strings.Contains(r, "not calibrated for this setting") {
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
	// the other mode keeps Goldshell's figure until it gets its own reading
	if v := powerView(c2Dash(525, 61, 1450)); c2Num(v, "watts") != 260 || v["source"] != "spec" {
		t.Fatalf("525/61 must stay spec: %v", v)
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
	if c2Num(half, "jth") != 421 { // 400 / 0.95
		t.Fatalf("J/TH follows the reported hashrate: %v", half["jth"])
	}
	if half["low_hash"] != true || full["low_hash"] != nil {
		t.Fatalf("low hashrate is flagged, not scaled: full %v half %v", full["low_hash"], half["low_hash"])
	}
}

func TestC2PowerBox2Refusals(t *testing.T) {
	dir, setDash := c2Box2(t, true, c2Dash(690, 42, 1900))
	for _, w := range []float64{20, 5000, 1000, 150} { // out of range; far from the 400 W spec
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
	if v := powerView(c2Dash(600, 50, 1650)); v["watts"] != nil {
		t.Fatalf("cleared setting must show no figure: %v", v)
	}
	setDash(c2Dash(690, 42, 1900))
	b10call(t, handlePower, "POST", "/api/power", map[string]any{"cal_watts": 405})
	b10call(t, handlePower, "POST", "/api/power", map[string]any{"clear": true})
	if v := powerView(c2Dash(690, 42, 1900)); c2Num(v, "watts") != 400 || v["source"] != "spec" {
		t.Fatalf("clear all: back to spec: %v", v)
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
