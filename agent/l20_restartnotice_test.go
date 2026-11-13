package main

import (
	"sync/atomic"
	"testing"
	"time"
)

// When the new run answers after a restart the panel made, the window closes in that same poll (buildDashboard),
// before the poll loop asks rebootActive() whether the uptime drop was the panel's: always "no", so
// "its mining software restarted on its own" went out after every Restart mining, schedule or chip-trim restart.
func TestL20PanelRestartNotOnItsOwn(t *testing.T) {
	var el atomic.Int64
	el.Store(3)
	oldAddr := *apiAddr
	*apiAddr = "127.0.0.1:47382"
	t.Cleanup(func() { *apiAddr = oldAddr })
	l20Miner(t, *apiAddr, &el)
	clearReboot()
	t.Cleanup(clearReboot)
	markReboot(150*time.Second, 5*time.Minute, "Restarting the mining software — hashing resumes shortly.")
	if d := buildDashboard0(); !d.OK {
		t.Fatalf("fake miner: %+v", d)
	}
	if _, _, ok := rebootActive(); ok {
		t.Fatal("the new run answered: the window stays open")
	}
	if !panelRestartedRecently() {
		t.Fatal("the poll loop can't tell the restart was the panel's")
	}
}
