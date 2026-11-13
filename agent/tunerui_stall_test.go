package main

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// The miner's chip list (icinfo) stops answering in the middle of a test while the API (temperature,
// hashrate) still answers: watch() `continue`s on every failed read, so the test never reaches its hold
// time and never ends. The run sits in that step for ever: no log line, frozen elapsed time.
func TestUIWatchStallsWhenChipReadsFailMidTest(t *testing.T) {
	cfg := fastCfg()
	cfg.LowestMHz, cfg.HighestMHz = 625, 650
	e, s := newSimEngine(cfg, 0, ckey{2, 5})
	var reads, broken atomic.Int32
	inner := e.readErrors
	e.readErrors = func() (map[ckey]int, error) {
		if broken.Load() == 1 {
			return nil, errors.New("Server Error")
		}
		e.mu.Lock()
		testing := e.cur != nil && !e.cur.Settling && e.cur.Phase != "baseline"
		e.mu.Unlock()
		if testing && reads.Add(1) > 2 { // a couple of good readings into the first test, then it breaks
			broken.Store(1)
		}
		return inner()
	}
	done := make(chan struct{})
	go func() { e.Run(); close(done) }()
	time.Sleep(3 * time.Second)
	s.mu.Lock()
	virt := s.vc.Sub(s.t0)
	s.mu.Unlock()
	st := e.statusView()
	select {
	case <-done:
		t.Logf("run ended on its own after %v (virtual): good", virt)
	default:
		cur, _ := st["cur"].(*curStep)
		e.Stop()
		<-done
		if cur != nil {
			t.Fatalf("still running after %v of miner time with no chip readings; cur %.1f/%g min, last reading at %v",
				virt.Round(time.Minute), cur.ElapsedMin, cur.HoldMin, time.Unix(cur.Updated, 0).UTC().Sub(s.t0))
		}
		t.Fatalf("still running after %v of miner time", virt)
	}
}
