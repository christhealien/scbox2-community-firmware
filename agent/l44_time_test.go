package main

// L44-time: wall-clock times kept and compared later, across the clock being set (the miner has no battery
// clock: 1970 after a boot until NTP or the browser sets it) or stepped by a time server. Each test writes the
// stored time as it was taken before the jump (a 1970 value, or one from a clock that ran ahead) and checks
// with the clock as it reads now.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const l44Unset = 600 // 1970-01-01 00:10 UTC: ten minutes after a boot, before the clock was set

func l44HeatEnv(t *testing.T) {
	t.Helper()
	old := *authFile
	*authFile = filepath.Join(t.TempDir(), "auth.json")
	heatEpMu.Lock()
	heatEpLast, heatEpCool = 0, 0
	heatEpMu.Unlock()
	t.Cleanup(func() {
		*authFile = old
		heatIdleSince.Store(0)
		heatHoldSaved.Store(false)
		heatPending.Store("")
		setCharge("")
		heatEpMu.Lock()
		heatEpLast, heatEpCool = 0, 0
		heatEpMu.Unlock()
	})
}

// The thermal guard put the miner on Idle ten minutes after a boot (clock still 1970); then the browser or NTP
// set the clock. The cool-down was measured from the 1970 time: it was over at once, so Stock, a preset or the
// owner's "start when cool" woke the hot boards straight away.
func TestL44HeatCoolDownSurvivesClockSet(t *testing.T) {
	l44HeatEnv(t)
	heatIdleNoted(l44Unset) // thermalIdle: noted, then stored, on the clock as it read then
	heatIdleSince.Store(l44Unset)
	setCharge("idle")
	if left := heatCoolLeft(); left < heatCoolFirst-time.Minute {
		t.Fatalf("BUG: heat Idle at a 1970 clock, clock set since: cool-down left %v (want about %v)", left, heatCoolFirst)
	}
	if heatCoolErr() == nil {
		t.Fatal("BUG: Stock or a preset may wake the miner right after the heat Idle once the clock is set")
	}
}

// A clock set from a browser that ran an hour ahead, stepped back by a time server after the heat Idle: the
// cool-down lasted 65 minutes instead of 5.
func TestL44HeatCoolDownClockSteppedBack(t *testing.T) {
	l44HeatEnv(t)
	ahead := time.Now().Add(time.Hour).Unix()
	heatIdleNoted(ahead)
	heatIdleSince.Store(ahead)
	setCharge("idle")
	if left := heatCoolLeft(); left > heatCoolFirst {
		t.Fatalf("BUG: clock stepped back an hour after the heat Idle: cool-down left %v (want at most %v)", left, heatCoolFirst)
	}
}

// The hold saved on a 1970 clock, read back by a panel restarted after the clock was set: no cool-down at all.
func TestL44HeatCoolDownRestoredFromUnsetClock(t *testing.T) {
	l44HeatEnv(t)
	b, _ := json.Marshal(map[string]int64{"since": l44Unset + 77})
	if err := os.WriteFile(heatHoldPath(), b, 0o644); err != nil {
		t.Fatal(err)
	}
	restartRestore()
	if !heatIdleHeld() {
		t.Fatal("setup: the hold wasn't restored")
	}
	if left := heatCoolLeft(); left < heatCoolFirst-time.Minute {
		t.Fatalf("BUG: a heat Idle saved on the 1970 clock, restored with the clock set: cool-down left %v", left)
	}
}

// A second heat Idle with the first one on the 1970 clock: how long ago is unknown (most likely minutes: the
// same boot), but it got the short cool-down of a first one.
func TestL44HeatRepeatAcrossClockSet(t *testing.T) {
	l44HeatEnv(t)
	heatIdleNoted(l44Unset)
	heatIdleNoted(time.Now().Unix())
	heatEpMu.Lock()
	cool := heatEpCool
	heatEpMu.Unlock()
	if cool != heatCoolRepeat {
		t.Fatalf("BUG: a second heat Idle, the first one on the 1970 clock of this boot: cool-down %v, want the repeat's %v", cool, heatCoolRepeat)
	}
}

// Auto-trim: chip 21 doesn't get better with a trim; it's undone and left alone for a day. That happened on the
// 1970 clock (a few hours after a boot without a time server); when the clock was set, the day had "passed"
// and the chip was slowed again in the next window.
func TestL44AutoTrimNoHelpDaySurvivesClockSet(t *testing.T) {
	s := newCwSim(t, func(chip int, trim float64) float64 {
		if chip == 21 {
			return 15
		}
		return 1
	})
	s.now = time.Unix(l44Unset, 0)
	on := true
	if _, err := chipWatchSet(&on, false); err != nil {
		t.Fatal(err)
	}
	s.run(4*time.Hour + 2*time.Minute) // a window to trim it, one to judge it
	cwMu.Lock()
	until, paused := cwSt.NoHelp[cwKey(2, 21)]
	cwMu.Unlock()
	if !paused || s.trims()[21] != 0 {
		t.Fatalf("setup: chip 21 not undone and paused: %v, nohelp %d", s.trims(), until)
	}
	// the clock is set: ~56 years later on the wall clock, a minute later in real time. (The window itself is
	// on the monotonic clock in the real panel: it starts over here as it would there.)
	s.now = time.Unix(1_790_000_000, 0)
	cwMu.Lock()
	cwResetWindow()
	cwMu.Unlock()
	s.run(6 * time.Hour)
	for _, l := range chipWatchView()["log"].([]map[string]any) {
		if at, _ := l["t"].(int64); at >= 1_790_000_000 && strings.Contains(l["msg"].(string), "chip 21 slowed") {
			t.Fatalf("BUG: chip 21 slowed again after the clock was set, though it was to be left alone for a day: %v", l["msg"])
		}
	}
}

// The board budget's day ("no new trims on this board for a day") set on the 1970 clock ends at once when the
// clock is set; one set on a clock that ran a week ahead (from a browser) lasts eight days after the time
// server steps it back.
func TestL44AutoTrimBudgetDayAcrossClockSteps(t *testing.T) {
	s := newCwSim(t, func(int, float64) float64 { return 1 })
	on := true
	if _, err := chipWatchSet(&on, false); err != nil {
		t.Fatal(err)
	}
	now := s.now.Unix()
	cwMu.Lock()
	cwSt.Budget = map[string]int64{"2": l44Unset + 86400, "3": now + 8*86400}
	cwMu.Unlock()
	s.run(time.Minute)
	cwMu.Lock()
	b2, b3 := cwSt.Budget["2"], cwSt.Budget["3"]
	cwMu.Unlock()
	day := int64(86400)
	if b2 < now+day-3600 {
		t.Fatalf("BUG: board 2's day without new trims, set on the 1970 clock, is over once the clock is set (until %d, now %d)", b2, now)
	}
	if b3 > now+day+3600 {
		t.Fatalf("BUG: board 3's day without new trims lasts %.1f days after the clock was stepped back a week", float64(b3-now)/86400)
	}
}

// After a power cut the miner boots before the router: the mining program can't reach a pool and stops and
// starts again and again (stops counted on the monotonic clock). That's noted at the 1970 clock. The network
// comes up, NTP sets the clock: the "no pool answered in the last 15 minutes" exception was now 56 years old,
// and the stops switched chip trims off ("the mining program stopped 4 times in 15 minutes with them on").
func TestL44PoolOutageBeforeClockSetKeepsTrims(t *testing.T) {
	old := *authFile
	*authFile = filepath.Join(t.TempDir(), "auth.json")
	oldB, oldL := backupRoot, poolUnreachableLog
	backupRoot = t.TempDir()
	poolUnreachableLog = func() string { return "Stratum connect failed to pool.example:3333" }
	fakeProc(t) // the mining program isn't running (it keeps stopping)
	t.Cleanup(func() {
		*authFile, backupRoot, poolUnreachableLog = old, oldB, oldL
		poolDownAt.Store(0)
		mhMu.Lock()
		mhStops = nil
		mhMu.Unlock()
	})
	os.WriteFile(chipHookFlag(), []byte("on\n"), 0o644)
	if msg := miningDownMessage0(time.Unix(l44Unset, 0)); !strings.Contains(msg, "can't reach any pool") {
		t.Fatalf("setup: %q", msg)
	}
	real := time.Now()
	mhMu.Lock()
	mhStops = []time.Time{real.Add(-4 * time.Minute), real.Add(-3 * time.Minute), real.Add(-2 * time.Minute), real.Add(-time.Minute)}
	mhMu.Unlock()
	fakeProc(t, "intminer") // the network is up: it runs now
	chipHookGuard(real)     // the clock reads today now
	if _, err := os.Stat(chipHookFlag()); err != nil {
		b, _ := os.ReadFile(chipHookFlag() + ".off")
		t.Fatalf("BUG: chip trims switched off for stops a minute-old pool outage caused (noted before the clock was set): %s", b)
	}
}

// The fan lock was unlocked once (saved: unlocked_at, today's date). The miner reboots where no time server
// answers and nobody opens the panel: the clock reads 1970, and so do the watchdog's lines. Every kill was
// "before the unlock": the lock could never engage on the watchdog's kills.
func TestL44FanLockEngagesAfterRebootToUnsetClock(t *testing.T) {
	write, idled := r4LockEnv(t)
	c := loadFanLockConfig()
	c.UnlockedAt = time.Now().Unix() // the unlock, before the reboot
	if err := saveFanLockConfig(c); err != nil {
		t.Fatal(err)
	}
	boot := time.Unix(l44Unset, 0)
	write(boot, r4Bad, r4Kill)
	now := boot.Add(5 * time.Second)
	write(now, r4Bad, r4Kill)
	now = now.Add(5 * time.Second)
	write(now, r4Good)
	fanLockTick(now)
	if fanLockErr() == nil || *idled == 0 {
		t.Fatal("BUG: two fan kills by the OEM's watchdog on the 1970 clock after a reboot: no fan lock (an unlock saved before the reboot hides every kill)")
	}
	os.Remove(fanLockPath())
}

// A board stopped hashing, mining was restarted automatically (a full reboot on this firmware) while the
// clock was unset: "600" saved. After the reboot NTP sets the clock; the board drops again 8 minutes in: the
// saved restart looked 56 years old and the miner was rebooted again within minutes.
func TestL44BoardAutoRestartSavedOnUnsetClock(t *testing.T) {
	*authFile = filepath.Join(t.TempDir(), "auth.json")
	captureNotify(t)
	restarts := 0
	boardRestart = func() error { restarts++; return nil }
	if err := os.WriteFile(boardAutoAtPath(), []byte("600"), 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	why := boardAutoRestart(now, "board 1 stopped hashing")
	incidentWG.Wait()
	if why == "" || restarts != 0 {
		t.Fatalf("BUG: restarted (rebooted) again right after a restart saved on the 1970 clock (%q, %d restarts)", why, restarts)
	}
	// and an hour later it may again
	if why := boardAutoRestart(now.Add(boardAutoGap+time.Minute), "board 1 stopped hashing"); why != "" {
		t.Fatalf("an hour later: %q", why)
	}
	incidentWG.Wait()
}

// fanctrl shut the boards down for heat while the clock read 1970 (the latch only a reboot clears). The owner
// opens the panel and the browser sets the clock: the line was "before this boot", so the Dashboard didn't say
// why the miner was down, and Re-run the OEM's fan test (which clears the shutdown) wasn't refused.
func TestL44OverTempLoggedOnUnsetClock(t *testing.T) {
	boot := time.Now().Add(-20 * time.Minute).Unix()
	log := " [2026-11-04 09:00:00]  Disable CPB, For Temperature Is Greater Than 92 \n" + // a boot before
		" [1970-01-01 00:00:03]  fanctrl start!!!\n" + // (another one before)
		" [1970-01-01 02:00:00]  Disable CPB, For Temperature Is Greater Than 92 \n" +
		" [1970-01-01 00:00:04]  fanctrl start!!!\n" + // this boot
		" [1970-01-01 00:12:00]  6 times: Over (93.1), Miner Maybe Shutdown\n" +
		" [1970-01-01 00:12:06]  Disable CPB, For Temperature Is Greater Than 92 \n"
	s := parseOverTemp(log, boot)
	if s == nil {
		t.Fatal("BUG: an over-temperature shutdown logged on the 1970 clock in this boot is ignored once the clock is set")
	}
	if time.Unix(s.At, 0).UTC().Minute() != 12 {
		t.Fatalf("the shutdown of an earlier boot was taken: %+v", s)
	}
	// the earlier boots' lines alone: nothing
	if s := parseOverTemp(strings.SplitN(log, " [1970-01-01 00:00:04]", 2)[0]+" [1970-01-01 00:00:04]  fanctrl start!!!\n", boot); s != nil {
		t.Fatalf("an earlier boot's shutdown counted: %+v", s)
	}
}

// A tuning run started before the clock was set (an API client, or a page left open across the reboot): its
// heartbeat was a wall-clock time, so the clock being set made it look silent for 56 years and the watchdog
// ended the run from outside ("it showed no progress for 29 million min"); a clock stepped back hid a stuck run.
func TestL44TunerHeartbeatNotWallClock(t *testing.T) {
	e := &tunerEngine{}
	e.alive()
	if d := e.beat.Load() - time.Now().UnixNano(); d > -int64(365*24*time.Hour) && d < int64(365*24*time.Hour) {
		t.Fatal("BUG: the tuner's heartbeat is a wall-clock time: setting the clock (1970 -> today) ends a running tuning run as stuck")
	}
	if q := quietSec(true, e.beat.Load()); q != 0 {
		t.Fatalf("quiet %d s right after a beat", q)
	}
}

// v1.5.6's upgrade note (presets moved aside, how to put them back) is shown for a month from the upgrade's first
// start, which is right after the update's reboot: usually still on the 1970 clock. Then it never showed.
func TestL44MigrationNoteFromUnsetClock(t *testing.T) {
	old := *authFile
	*authFile = filepath.Join(t.TempDir(), "auth.json")
	t.Cleanup(func() { *authFile = old })
	saveMigration156(migration156{At: l44Unset, Presets: 3, Stock: "applied"})
	if _, ok := updView()["migration"]; !ok {
		t.Fatal("BUG: the upgrade note made before the clock was set never shows")
	}
}
