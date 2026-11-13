package main

import (
	"sync"
	"testing"
	"time"
)

func TestTunerConcurrentStatus(t *testing.T) {
	e, _ := newSimEngine(fastCfg(), 8900, ckey{2, 5})
	// wrap sleep so Run() takes real time and interleaves with the status poller
	base := e.sleep
	e.sleep = func(d time.Duration) { base(d); time.Sleep(80 * time.Microsecond) }
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = e.statusView()
				}
			}
		}()
	}
	e.Run()
	close(stop)
	wg.Wait()
}
