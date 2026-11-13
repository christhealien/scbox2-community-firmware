package main

import (
	"errors"
	"testing"
	"time"
)

// ops sweep: the OEM's web backend (minerd: /dbg/icinfo) stops answering in the middle of a test while the
// mining program (API: temperatures, hashrate) keeps going. watch() skips every failed chip read with
// `continue` before it works out the elapsed time, so the test never reaches its hold time: the run sits on
// "test 0.2/1min" for ever (seen through the HTTP API for 7 min with the mock's web down; the panel showed
// running, the same elapsed and no new log line). Only Stop gets it out. At the start of a watch the same
// failure is waited out for backendWait and then ends the run; in the middle it should too.
func TestTunerWatchBackendDropMidTestEnds(t *testing.T) {
	c := fastCfg()
	c.PollS = 15
	e, s := newSimEngine(c, 0, ckey{2, 5})
	read := e.readErrors
	e.readErrors = func() (map[ckey]int, error) {
		s.mu.Lock()
		el := s.vc.Sub(s.t0)
		s.mu.Unlock()
		if el > 90*time.Second { // half way into the first test (the baseline is minute 0-1)
			return nil, errors.New("the miner's backend (the OEM's minerd) didn't answer")
		}
		return read()
	}
	done := make(chan struct{})
	go func() { e.Run(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second): // virtual time: days have passed by now
		e.Stop()
		<-done
		s.mu.Lock()
		el := s.vc.Sub(s.t0)
		s.mu.Unlock()
		t.Fatalf("the test never ended with the backend gone (%.0f h of virtual time; only Stop ended it); last log: %v", el.Hours(), e.logbuf[len(e.logbuf)-1])
	}
}
