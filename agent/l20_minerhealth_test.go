package main

import (
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// l20BoardDown: board 1 of four stops hashing 1 h into mining, for long enough that boardWatch acts.
func l20BoardDown(t *testing.T) {
	t.Helper()
	bw = &boardWatchState{seen: map[int]bool{}, since: map[int]time.Time{}, alerted: map[int]bool{}, thisRun: map[int]bool{}}
	now := time.Unix(1_790_000_000, 0)
	nwNow = func() time.Time { return now }
	el := 3600
	step := func(b1 float64) {
		now = now.Add(10 * time.Second)
		el += 10
		boardWatch(dashboard{OK: true, Elapsed: el, Boards: []board{{ID: 0, Alive: true, GHS: 1150}, {ID: 1, Alive: b1 > 0, GHS: b1},
			{ID: 2, Alive: true, GHS: 1150}, {ID: 3, Alive: true, GHS: 1150}}}, getCharge())
	}
	step(1150)
	for i := 0; i < 20; i++ {
		step(0)
	}
	incidentWG.Wait()
}

// The automatic restart for a board is the panel's own restart: like Settings → Restart mining it opens the
// restart window, so the mining program's stop isn't counted as a crash (the chip-trim and Verbose guards, the
// "keeps stopping" diagnosis, "restarted on its own").
func TestL20BoardAutoRestartIsPanelRestart(t *testing.T) {
	*authFile = filepath.Join(t.TempDir(), "auth.json")
	captureNotify(t)
	clearReboot()
	t.Cleanup(clearReboot)
	var restarts atomic.Int32
	boardRestart = func() error {
		restarts.Add(1)
		if _, _, ok := rebootActive(); !ok {
			t.Error("the restart ran with no restart window open")
		}
		return nil
	}
	mhMu.Lock()
	mhRunning, mhStops = false, nil
	mhMu.Unlock()
	fakeProc(t, "intminer")
	minerHealthTick(time.Now())
	l20BoardDown(t)
	if restarts.Load() != 1 {
		t.Fatalf("restarts %d", restarts.Load())
	}
	if _, _, ok := rebootActive(); !ok {
		t.Fatal("after the automatic restart: no restart window (the Dashboard, the stop counter and the restart notice treat it as a crash)")
	}
	fakeProc(t) // the restart stops the mining program
	minerHealthTick(time.Now().Add(15 * time.Second))
	if n := intminerStopsSince(time.Now().Add(-time.Hour)); n != 0 {
		t.Fatalf("the panel's own restart counted as %d mining program stop(s)", n)
	}
}

// A failed automatic restart: the owner was already told "mining restarted by itself", so they're told it failed.
func TestL20BoardAutoRestartFailureNotified(t *testing.T) {
	*authFile = filepath.Join(t.TempDir(), "auth.json")
	got := captureNotify(t)
	clearReboot()
	t.Cleanup(clearReboot)
	boardRestart = func() error { return errors.New("HTTP 500") }
	l20BoardDown(t)
	captureMu.Lock()
	all := strings.Join(*got, "\n")
	captureMu.Unlock()
	if !strings.Contains(all, "restarted by itself") {
		t.Fatalf("no board notice: %q", all)
	}
	if !strings.Contains(all, "failed") {
		t.Fatalf("the restart failed, the owner only heard it restarted: %q", all)
	}
	if _, _, ok := rebootActive(); ok {
		t.Fatal("a failed restart left the restart window open")
	}
}

// The limit probe is in charge like the tuner (the schedule and the fan test re-run hold off for it): a board
// that drops at a probe value doesn't get mining restarted under the probe.
func TestL20NoBoardAutoRestartDuringLimitProbe(t *testing.T) {
	*authFile = filepath.Join(t.TempDir(), "auth.json")
	real := boardAutoBusy
	captureNotify(t)
	boardAutoBusy = real
	clearReboot()
	t.Cleanup(clearReboot)
	var restarts atomic.Int32
	boardRestart = func() error { restarts.Add(1); return nil }
	old := getCharge()
	setCharge("ceiling")
	t.Cleanup(func() { setCharge(old) })
	l20BoardDown(t)
	if restarts.Load() != 0 {
		t.Fatalf("mining restarted %d time(s) in the middle of the limit probe", restarts.Load())
	}
}

// l20Miner: a cgminer API answering "summary" with this mining uptime.
func l20Miner(t *testing.T, addr string, elapsed *atomic.Int64) {
	t.Helper()
	l, err := net.Listen("tcp", addr)
	if err != nil {
		t.Skip("port busy: ", err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 512)
				c.SetReadDeadline(time.Now().Add(time.Second))
				n, _ := c.Read(buf)
				if strings.Contains(string(buf[:n]), "summary") {
					fmt.Fprintf(c, `{"STATUS":[{"STATUS":"S"}],"SUMMARY":[{"Elapsed":%d,"MHS av":4600000,"Accepted":10}]}`, elapsed.Load())
				} else {
					c.Write([]byte(`{"STATUS":[{"STATUS":"S"}]}`))
				}
				c.Write([]byte{0})
			}(c)
		}
	}()
}

// Settings → Restart mining opens the restart window; the mining run that answers in the next seconds is the one
// on its way down. The window closed at that answer, so its stop counted as a crash.
func TestL20RestartWindowSurvivesOldRunAnswer(t *testing.T) {
	var el atomic.Int64
	el.Store(7200)
	oldAddr := *apiAddr
	*apiAddr = "127.0.0.1:47381"
	t.Cleanup(func() { *apiAddr = oldAddr })
	l20Miner(t, *apiAddr, &el)
	clearReboot()
	t.Cleanup(clearReboot)
	mhMu.Lock()
	mhRunning, mhStops = false, nil
	mhMu.Unlock()
	fakeProc(t, "intminer")
	minerHealthTick(time.Now())

	markReboot(150*time.Second, 5*time.Minute, "Restarting the mining software — hashing resumes shortly.")
	if d := buildDashboard0(); !d.OK {
		t.Fatalf("fake miner: %+v", d)
	}
	if _, _, ok := rebootActive(); !ok {
		t.Fatal("the old run (up 2 h) answered and closed the restart window")
	}
	fakeProc(t) // the old run stops
	minerHealthTick(time.Now().Add(15 * time.Second))
	if n := intminerStopsSince(time.Now().Add(-time.Hour)); n != 0 {
		t.Fatalf("the owner's restart counted as %d crash(es)", n)
	}
	el.Store(3) // the new run answers
	buildDashboard0()
	if _, _, ok := rebootActive(); ok {
		t.Fatal("the new run answered: the window stays open")
	}
}
