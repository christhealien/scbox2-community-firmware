package main

import (
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// l46Miner: a cgminer API on 127.0.0.1:47940 whose replies the test sets.
func l46Miner(t *testing.T) (set func(sum, devs string)) {
	ln, err := net.Listen("tcp", "127.0.0.1:47940")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var sum, devs string
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			buf := make([]byte, 512)
			n, _ := c.Read(buf)
			mu.Lock()
			out := sum
			if strings.Contains(string(buf[:n]), "devs") {
				out = devs
			}
			mu.Unlock()
			c.Write([]byte(out + "\x00"))
			c.Close()
		}
	}()
	old := *apiAddr
	*apiAddr = ln.Addr().String()
	t.Cleanup(func() { *apiAddr = old; ln.Close() })
	return func(s, d string) { mu.Lock(); sum, devs = s, d; mu.Unlock() }
}

// A board that stops hashing while the miner still lists it Alive: its work counter ("Total MH") stops, but
// its rolling "MHS 20s" figure stays where it was (cgminer only updates it when the board reports work) and
// its "MHS av" (the average since the start) stays well above zero. The panel's work-counter rate is 0 then,
// and that is the figure to show: on base the 0 was dropped (`g > 0`) and the board kept showing 1100 GH/s,
// so the Live total, the chart, the "hashrate low" alert and the board-down alert (boardWatch) missed it.
func TestL46StalledBoardReadsZero(t *testing.T) {
	set := l46Miner(t)
	t0 := time.Unix(1_790_000_000, 0)
	now := t0
	liveNow = func() time.Time { return now }
	liveWork = &workTrack{s: map[string][]workSample{}}
	t.Cleanup(func() { liveNow = time.Now; liveWork = &workTrack{s: map[string][]workSample{}} })
	w0, w1 := 4e10, 4e10 // MH so far
	el := 36000
	var d dashboard
	for i := 0; i < 30; i++ { // 5 minutes, a reading every 10 s
		now = now.Add(10 * time.Second)
		el += 10
		w0 += 1100 * 1000 * 10
		if i < 6 {
			w1 += 1100 * 1000 * 10 // board 1 hashes for the first minute, then stops
		}
		set(fmt.Sprintf(`{"STATUS":[{"STATUS":"S"}],"SUMMARY":[{"Elapsed":%d,"MHS av":2200000.0,"MHS 20s":2200000.0,"Total MH":%.0f,"Accepted":5}],"id":1}`, el, w0+w1),
			fmt.Sprintf(`{"STATUS":[{"STATUS":"S"}],"DEVS":[{"ID":0,"Enabled":"Y","Status":"Alive","MHS av":1100000.0,"MHS 20s":1100000.0,"Total MH":%.0f},{"ID":1,"Enabled":"Y","Status":"Alive","MHS av":1100000.0,"MHS 20s":1100000.0,"Total MH":%.0f}],"id":1}`, w0, w1))
		d = buildDashboard0()
	}
	if !d.OK || len(d.Boards) != 2 {
		t.Fatalf("dashboard: %+v", d)
	}
	if g := d.Boards[1].GHS; g > 1 {
		t.Errorf("board 1 has done no work for 4 minutes but shows %.0f GH/s", g)
	}
	if g := d.Boards[0].GHS; math.Abs(g-1100) > 5 {
		t.Errorf("board 0: %.1f GH/s, want ~1100", g)
	}
	if math.Abs(d.GHS-1100) > 5 {
		t.Errorf("total %.0f GH/s, want ~1100 (one board hashing)", d.GHS)
	}
}

// The work-counter rate in the first two minutes after a (re)start: a counter that hasn't moved yet in a short
// window isn't read as "stopped" (the miner may update it only every few seconds); after the full window it is.
func TestL46RateZeroOnlyAfterFullWindow(t *testing.T) {
	t0 := time.Unix(1_790_000_000, 0)
	now := t0
	liveNow = func() time.Time { return now }
	t.Cleanup(func() { liveNow = time.Now })
	wt := &workTrack{s: map[string][]workSample{}}
	wt.rate("b", 1000)
	now = now.Add(40 * time.Second)
	if g, ok := wt.rate("b", 1000); ok {
		t.Fatalf("40 s without movement: %v %v (too short to call it stopped)", g, ok)
	}
	for i := 0; i < 12; i++ {
		now = now.Add(10 * time.Second)
		wt.rate("b", 1000)
	}
	if g, ok := wt.rate("b", 1000); !ok || g != 0 {
		t.Fatalf("2+ min without work: %v %v, want 0 true", g, ok)
	}
}

// The chart history keeps 1440 points (one a minute while the miner answers); with Idle hours (a schedule) or
// the agent stopped, those points reach back more than 24 h. The "24 h average" of the daily summary and of
// /api/hashrate, and the "usual" hashrate the low-hashrate alert compares with, must use the last 24 h only.
func TestL46Average24hOnly(t *testing.T) {
	hrMu.Lock()
	oldH := hrHist
	now := time.Now().Unix()
	hrHist = nil
	for i := 0; i < 600; i++ { // yesterday morning, an older clock: 1000 GH/s
		hrHist = append(hrHist, hrSample{T: now - 40*3600 + int64(i*60), GHS: 1000})
	}
	for i := 0; i < 600; i++ { // the last 10 hours: 4000 GH/s
		hrHist = append(hrHist, hrSample{T: now - 36000 + int64(i*60), GHS: 4000})
	}
	hrMu.Unlock()
	t.Cleanup(func() { hrMu.Lock(); hrHist = oldH; hrMu.Unlock() })
	rec := httptest.NewRecorder()
	handleHashrate(rec, httptest.NewRequest("GET", "/api/hashrate", nil))
	var v struct {
		Avg float64 `json:"avg"`
	}
	json.Unmarshal(rec.Body.Bytes(), &v)
	if math.Abs(v.Avg-4000) > 1 {
		t.Errorf("/api/hashrate avg %.0f, want 4000 (the last 24 h)", v.Avg)
	}
	if a, _ := hrAvgSince(now - 24*3600); math.Abs(a-4000) > 1 {
		t.Errorf("usual hashrate %.0f, want 4000", a)
	}
}

// Chip "%": nonces are counted since mining started, but a trim changes a chip's clock live (no restart). The
// per-MHz comparison has to use the counts since the clocks last changed: on base a chip slowed 650 -> 600
// an hour ago, after 10 h at 650, read 107 % (it looks strong; it's exactly average), and a chip raised to
// +50 an hour ago read 94 % and was flagged "low".
func TestL46ChipShareAfterLiveTrim(t *testing.T) {
	dir := t.TempDir()
	oldS, oldA := chipStatsPath, *authFile
	chipStatsPath = filepath.Join(dir, "stats.json")
	*authFile = filepath.Join(dir, "auth.json")
	t.Cleanup(func() { chipStatsPath, *authFile = oldS, oldA; chipSegReset() })
	write := func(clk []int, non []int) {
		b, _ := json.Marshal(map[string]any{"chains": []map[string]any{{"chain": 0, "base10": 6500, "chips": 46, "clock10": clk, "nonces": non}}})
		if err := os.WriteFile(chipStatsPath, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	clk, non := make([]int, 47), make([]int, 47)
	for i := 1; i <= 46; i++ {
		clk[i], non[i] = 6500, 65000 // 10 h at 650 MHz
	}
	write(clk, non)
	chipTrimView()
	// trims applied live: chip 5 -> 600 MHz, chip 9 -> 700 MHz; one more hour
	clk[5], clk[9] = 6000, 7000
	for i := 1; i <= 46; i++ {
		non[i] += clk[i]
	}
	write(clk, non)
	v := chipTrimView()
	ch := v["chains"].([]chainView)
	if len(ch) != 1 {
		t.Fatalf("chains: %+v", ch)
	}
	for _, r := range ch[0].Chips {
		if r.Chip == 5 || r.Chip == 9 || r.Chip == 1 {
			if math.Abs(r.Share-100) > 0.5 || r.Low {
				t.Errorf("chip %d at %.0f MHz, exactly average per MHz: shows %.1f %% (low=%v)", r.Chip, r.Clock, r.Share, r.Low)
			}
		}
	}
}
