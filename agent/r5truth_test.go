package main

// r5-truth: what the Controls & fan tab shows must be what the miner runs. Uses r4autofan_test.go's fake backend.

import (
	"strings"
	"testing"
	"time"
)

func r5FanStatus() (maxT float64, hot, target int, note string) {
	fanMu.RLock()
	defer fanMu.RUnlock()
	return fanSt.MaxT, fanSt.HotBoard, fanSt.Target, fanSt.Note
}

// After a hot reading, a reading with no temperature (every sensor -60/-150) or no answer at all (the API is down,
// which is also every Idle) must not leave the old temperature and fan level on the panel as if live.
func TestR5NoReadingDoesNotShowOldTemperature(t *testing.T) {
	newAfMiner(t)
	c := afCfg()
	c.ThrottleEnable = false
	afTemp(89, true)
	fanTick(c)
	fanTick(c)
	if mt, _, tg, _ := r5FanStatus(); mt != 89 || tg != 100 {
		t.Fatalf("setup: want 89 °C / fan 100, got %.0f / %d", mt, tg)
	}
	fanDash = func() dashboard { // hashing, every sensor unreadable
		d := dashboard{OK: true}
		for i := 0; i < 4; i++ {
			d.Boards = append(d.Boards, board{ID: i, Alive: true, GHS: 900, Temps: []float64{-60, -60, -150}})
		}
		return d
	}
	fanTick(c)
	if mt, hb, tg, note := r5FanStatus(); mt > -100 || hb >= 0 || tg >= 0 {
		t.Errorf("no temperature: the panel still shows %.0f °C (board %d), fan %d %% (%s)", mt, hb, tg, note)
	}
	afTemp(89, true)
	fanTick(c)
	fanDash = func() dashboard { return dashboard{OK: false} } // API down / Idle
	fanTick(c)
	if mt, hb, tg, note := r5FanStatus(); mt > -100 || hb >= 0 || tg >= 0 {
		t.Errorf("API down: the panel still shows %.0f °C (board %d), fan %d %% (%s)", mt, hb, tg, note)
	}
}

// A fan write that fails must not be shown as the fans' level.
func TestR5FailedFanWriteNotShownAsLevel(t *testing.T) {
	m := newAfMiner(t)
	c := afCfg()
	c.ThrottleEnable = false
	afTemp(60, true)
	fanTick(c)
	m.mu.Lock()
	m.putFail = true
	m.mu.Unlock()
	afTemp(80, true) // the curve wants more fan; the write fails
	fanTick(c)
	_, _, tg, note := r5FanStatus()
	if tg >= 0 {
		t.Errorf("the write failed but the panel shows fan %d %% (%s)", tg, note)
	}
	if !strings.Contains(note, "fan write failed") {
		t.Errorf("note: %q", note)
	}
}

// The Live hashrate after the readings stopped (Idle, a board that stopped hashing) must not average the stopped
// time in: 4.4 TH/s showed as 1.1 TH/s (720 J/TH) for about two minutes after waking from a 5-minute Idle.
func TestR5LiveWorkAfterGap(t *testing.T) {
	old := liveNow
	t.Cleanup(func() { liveNow = old })
	wt := &workTrack{s: map[string][]workSample{}}
	now := time.Unix(1_800_000_000, 0)
	liveNow = func() time.Time { return now }
	w := 0.0
	const ghs = 1100.0        // per board
	for i := 0; i < 30; i++ { // 5 min hashing, a reading every 10 s
		wt.rate("dev0", w)
		now = now.Add(10 * time.Second)
		w += ghs * 1000 * 10
	}
	now = now.Add(5 * time.Minute) // Idle: no readings (the board reports no work)
	var g float64
	for i := 0; i < 5; i++ { // 40 s after it hashes again
		g, _ = wt.rate("dev0", w)
		now = now.Add(10 * time.Second)
		w += ghs * 1000 * 10
	}
	if g > 0 && (g < ghs*0.9 || g > ghs*1.1) {
		t.Errorf("40 s after the Idle the board reads %.0f GH/s, it hashes %.0f", g, ghs)
	}
}
