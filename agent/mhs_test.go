package main

import (
	"net"
	"testing"
	"time"
)

func TestCurrentMHS(t *testing.T) {
	sum := []byte(`{"STATUS":[{"STATUS":"S"}],"SUMMARY":[{"Elapsed":36000,"MHS av":4300000.0,"MHS 20s":4560000.0,"Accepted":5}],"id":1}`)
	if v := currentMHS(sum, "SUMMARY", 0); v != 4560000 {
		t.Fatalf("summary: %v (want the 20 s figure, not the average)", v)
	}
	devs := []byte(`{"DEVS":[{"ID":0,"MHS av":1.0,"MHS 5s":1150000.0},{"ID":1,"MHS av":2.0,"MHS 7s":1140000.0},{"ID":2,"MHS av":3.0}],"id":1}`)
	for i, want := range []float64{1150000, 1140000, 0} {
		if v := currentMHS(devs, "DEVS", i); v != want {
			t.Errorf("dev %d: %v want %v", i, v, want)
		}
	}
}

func TestLiveWorkRate(t *testing.T) {
	t0 := time.Unix(1_790_000_000, 0)
	now := t0
	liveNow = func() time.Time { return now }
	defer func() { liveNow = time.Now }()
	wt := &workTrack{s: map[string][]workSample{}}
	w := 1e9 // MH so far
	step := func(d time.Duration, ghs float64) (float64, bool) {
		now = now.Add(d)
		w += ghs * 1000 * d.Seconds()
		return wt.rate("all", w)
	}
	wt.rate("all", w)
	if _, ok := step(20*time.Second, 4500); ok {
		t.Fatal("under 30 s of readings: not yet")
	}
	if g, ok := step(15*time.Second, 4500); !ok || g < 4499 || g > 4501 {
		t.Fatalf("35 s at 4500: %v %v", g, ok)
	}
	// the clock drops: within ~2 minutes the figure is the new rate, not a long average
	var g float64
	for i := 0; i < 20; i++ {
		g, _ = step(8*time.Second, 4200)
	}
	if g < 4195 || g > 4215 {
		t.Fatalf("160 s after the change: %.1f, want ~4200", g)
	}
	// the miner restarted (counter back near zero): start again, no nonsense rate
	w = 100
	if _, ok := wt.rate("all", w); ok {
		t.Fatal("after a counter reset: wait for 30 s of readings")
	}
	// summary without Total MH: average × elapsed
	if v, ok := workOf([]byte(`{"STATUS":[{"STATUS":"S"}],"SUMMARY":[{"MHS av":4300000.0,"Elapsed":100}],"id":1}`+"\x00"), "SUMMARY", 0); !ok || v != 4.3e8 {
		t.Fatal(v, ok)
	}
	if v, ok := workOf([]byte(`{"STATUS":[{"STATUS":"S"}],"SUMMARY":[{"MHS av":4300000.0,"Elapsed":100,"Total MH":5e8}],"id":1}`), "SUMMARY", 0); !ok || v != 5e8 {
		t.Fatal(v, ok)
	}
}

// The real miner's API reply, as intminer sends it: "id": 1 at the end and a NUL byte. Until v1.5.5 the
// "id" made every work-counter and current-rate reading fail, so the tuner and the dashboard fell back to
// the average since mining started (two users' tuner runs: hashrate "5-7 % below" at every clock).
func TestRealAPIReplyShape(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			buf := make([]byte, 256)
			c.Read(buf)
			c.Write([]byte(`{"STATUS":[{"STATUS":"S","When":1793600000,"Code":11,"Msg":"Summary","Description":"intminer"}],"SUMMARY":[{"Elapsed":36000,"MHS av":4300000.00,"MHS 20s":3800000.00,"Total MH":154800000000.0,"Accepted":5}],"id":1}` + "\x00"))
			c.Close()
		}
	}()
	old := *apiAddr
	*apiAddr = ln.Addr().String()
	defer func() { *apiAddr = old }()
	if w, ok := readTotalMH(); !ok || w != 154800000000 {
		t.Fatalf("readTotalMH: %v %v", w, ok)
	}
	raw, err := cgminerCmd("summary")
	if err != nil {
		t.Fatal(err)
	}
	if v := currentMHS(raw, "SUMMARY", 0); v != 3800000 {
		t.Fatalf("currentMHS: %v", v)
	}
}
