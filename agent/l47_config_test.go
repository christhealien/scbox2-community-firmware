package main

// L47 (settings across versions): files a v1.5.22 panel keeps over a restart, left behind by a roll back to a
// version that doesn't know them (v1.5.18 and older: heat-idle.json, schedule-before.json, throttle.json came
// in v1.5.19), are read again by v1.5.22 after the next update, weeks later, as if no time had passed.
//
// The sequence: v1.5.15 -> update to v1.5.22 -> the thermal guard idles the miner (or a painted schedule stretch
// runs) -> the owner rolls back to v1.5.15 (Settings -> Firmware update) -> mines on v1.5.15 for weeks -> updates
// to v1.5.22 again. v1.5.15's own installUpdate writes update/pending {"from":"v1.5.15",...}.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// l47OlderInterlude: the files of a v1.5.22 run from `ago` back, then an update from `from` just now.
func l47OlderInterlude(t *testing.T, from string, ago time.Duration, files map[string]string) {
	t.Helper()
	dir := filepath.Dir(*authFile)
	old := time.Now().Add(-ago)
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		os.Chtimes(p, old, old)
	}
	if from == "" {
		return
	}
	os.MkdirAll(updDir(), 0o755)
	mk, _ := json.Marshal(updMarker{From: from, To: "v1.5.22", At: time.Now().Add(-time.Minute).Unix()})
	if err := os.WriteFile(filepath.Join(updDir(), "pending"), mk, 0o644); err != nil {
		t.Fatal(err)
	}
}

// 1. The thermal guard's Idle hold from before the roll back comes back after the re-update although the miner
// has been hashing for weeks: the charge says "idle", the schedule makes no change (it leaves a heat Idle alone)
// and the board auto-restart refuses ("the thermal guard holds the miner on Idle"), until the owner happens to
// apply a preset. A hold the same version left (a crash, a power cut, an update from v1.5.21) must stay.
func TestL47HeatHoldFromBeforeOlderVersion(t *testing.T) {
	for _, tc := range []struct {
		from     string
		wantHeld bool
	}{{"v1.5.15", false}, {"v1.5.18", false}, {"", true}, {"v1.5.21", true}} {
		newHLMiner(t) // hashing at 625 MHz (the owner resumed it on the older version)
		since := time.Now().Add(-21 * 24 * time.Hour).Unix()
		l47OlderInterlude(t, tc.from, 21*24*time.Hour, map[string]string{"heat-idle.json": `{"since":` + jsonInt(since) + `}`})
		r5Restart() // the new panel process: restartRestore
		if held := heatIdleHeld(); held != tc.wantHeld {
			t.Errorf("update from %q: heat hold %v, want %v (charge %q): a hold v1.5.22 saved weeks ago, before a roll back to a version that doesn't know it, is taken as current", tc.from, held, tc.wantHeld, getCharge())
		}
		heatIdleSince.Store(0)
		setCharge("")
		os.RemoveAll(updDir())
	}
}

// 2. "What it ran before the painted stretch", saved before the roll back, is put back after the re-update in
// unpainted time, over the setting the owner chose on the older version since (625 MHz here; the stale one 650).
func TestL47ScheduleBeforeFromBeforeOlderVersion(t *testing.T) {
	now := time.Now()
	if now.Hour() == 0 && now.Minute() < 5 || now.Hour() == 23 && now.Minute() > 50 {
		t.Skip("too near midnight for the HH:MM rules")
	}
	for _, tc := range []struct {
		from string
		want int
	}{{"v1.5.15", 625}, {"", 650}} {
		m := newHLMiner(t)
		schedPath = filepath.Join(filepath.Dir(*authFile), "schedule.json")
		hm := func(d time.Duration) string { return now.Add(d).Format("15:04") }
		all := []int{0, 1, 2, 3, 4, 5, 6}
		b, _ := json.Marshal(schedule{Enabled: true, Rules: []schedRule{{Start: hm(-time.Minute), Days: all, Preset: schedBack}, {Start: hm(2 * time.Hour), Days: all, Preset: "Idle"}}})
		writeAtomic(schedPath, b, 0o644)
		l47OlderInterlude(t, tc.from, 14*24*time.Hour, map[string]string{
			"schedule-before.json": `{"before":{"freq_mhz":650,"vrun_mv":9100,"pv_mv":9400,"fan1":50,"fan2":50},"by":"","name":"Idle"}`})
		r5Restart()
		lastSchKey, lastSchName, schedBefore, schedBeforeBy = -1, "", nil, ""
		scheduleTick()
		if got := m.planMHz(); got != tc.want {
			t.Errorf("update from %q: the miner runs %d MHz, want %d: \"back to before\" took a stretch's record from before a roll back (weeks old) over the owner's later setting", tc.from, got, tc.want)
		}
		schedPath, schedBefore, schedBeforeBy, lastSchName, lastSchKey = "", nil, "", "", -1
		os.RemoveAll(updDir())
	}
}

func jsonInt(v int64) string { b, _ := json.Marshal(v); return string(b) }
