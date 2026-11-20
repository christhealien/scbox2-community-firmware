package main

// v0.5.5 screenshots: with V055SHOTS=<dir>, the Tuner tab's and the Presets panel's data as the panel sends it, from
// the C8-like trim run and the presets run of v055_box2_tuner_test.go (the engine's own views, on the virtual clock):
// tuner-running.json (a running trim retest), tuner-done.json (the finished run), chipmap-list.json, chipmap-<n>.json,
// presets.json. Skipped otherwise.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestV055ShotsData(t *testing.T) {
	dir := os.Getenv("V055SHOTS")
	if dir == "" {
		t.Skip("V055SHOTS not set")
	}
	os.MkdirAll(dir, 0o755)
	put := func(name string, v any) {
		b, err := json.MarshalIndent(v, "", " ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	v055Box2(t)
	*presetPath = filepath.Join(t.TempDir(), "presets.json")
	day := time.Date(2026, 11, 15, 9, 0, 0, 0, time.Local)

	// the trim run, with presets (fans held): a snapshot while the third trimmed retest runs
	cfg := v055Cfg()
	cfg.LowestMHz, cfg.HighestMHz = 650, 675
	cfg.BuildPresets, cfg.FanHold, cfg.PresetMin = true, true, 10
	v := newV055Sim(t, cfg)
	v.s.mu.Lock()
	v.s.vc, v.s.t0 = day, day
	v.s.mu.Unlock()
	fan := 58
	v.e.applyFan = func(p int) error { fan = p; return nil }
	v.e.readFan = func() (int, bool) { return fan, true }
	temp := 71.4
	v.e.readTemp = func() (float64, bool) { return temp, true }
	v.e.readGHS = func() (float64, bool) { return 1890 + float64(fan%7)*3, true }
	var running map[string]any
	v.onSleep = func() {
		if running != nil || len(v.trimsNow()) < 2 {
			return
		}
		v.e.mu.Lock()
		cur := v.e.cur
		ok := cur != nil && !cur.Settling && cur.ElapsedMin > 6
		v.e.mu.Unlock()
		if ok {
			running = v.e.statusView()
		}
	}
	v.e.Run()
	if running == nil {
		t.Fatal("no running snapshot")
	}
	tuner := func(status map[string]any) map[string]any {
		d := defaultTunerConfig()
		d.ChipTrim, d.BuildPresets, d.FanHold, d.DryRun = true, true, true, false
		d.LowestMHz, d.HighestMHz = 650, 690
		d.MinMV, d.MaxMV = 42, 61
		return map[string]any{"ok": true, "defaults": d, "in_charge": "the auto-tuner", "files_dir": "/mnt/config/scbox2/tuner/SCBox2-1-20261115-0900",
			"overclock": false, "ocmax": map[string]any{"freq": 690}, "stock": map[string]int{"mhz": 690, "mv": 42, "pv": 0},
			"running": map[string]int{"mhz": 690, "mv": 42, "pv": 0}, "status": status, "box2": box2TunerInfo()}
	}
	put("tuner-running.json", tuner(running))
	done := v.e.statusView()
	put("tuner-done.json", tuner(done))
	put("tuner-form.json", tuner(nil))
	_, maps := v.e.chipMaps()
	list := make([]testChipMap, len(maps))
	for i, m := range maps {
		put(fmt.Sprintf("chipmap-%d.json", i), map[string]any{"ok": true, "test": m})
		m.Chips = nil
		list[i] = m
	}
	put("chipmap-list.json", map[string]any{"ok": true, "started": "2026-11-15 09:00", "tests": list})
	// the Presets panel: the stock modes, the four tuned presets (from a 600–688 MHz presets run), their power
	v2 := v055PresetRun(t)
	_ = v2
	pl := presetsList()
	est := map[string]any{}
	for _, p := range pl {
		if w, src := box2UnitWatts(p.MHz, p.MV); w > 0 {
			est[p.Name] = map[string]any{"w": w, "src": src}
		}
	}
	for i := range pl {
		if pl[i].Tested != "" {
			pl[i].Tested = "2026-11-15"
		}
	}
	put("presets.json", map[string]any{"ok": true, "presets": pl, "in_charge": "preset Middle", "box2": true, "stock_modes": true,
		"est": est, "current": map[string]int{"mhz": 663, "mv": 44, "pv": 0}, "has_idle": true, "stock": map[string]int{"mhz": 690, "mv": 42, "pv": 0},
		"ghs": 1815.0})
}
