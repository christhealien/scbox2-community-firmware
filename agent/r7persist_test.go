package main

// tuner-sweep/r7-persist: chip-trim state on disk across restarts (power cuts mid-write, panel/mining restarts,
// updates and roll-backs, a full flash). The rule checked: the miner never comes back with trims nobody asked
// for, and never loses the owner's trims.

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// r7Restart: a fresh panel process for the chip-trim area (memory gone, files kept).
func r7Restart() {
	cwMu.Lock()
	cwLoaded, cwSt, cwStatus = false, cwState{}, "off"
	cwResetWindow()
	cwMu.Unlock()
	chargeMu.Lock()
	chargeBy = ""
	chargeMu.Unlock()
}

func r7Trims() map[[2]int]float64 {
	m := map[[2]int]float64{}
	for _, t := range readChipTrims() {
		m[[2]int{t.Chain, t.Chip}] = t.MHz
	}
	return m
}

// 1. A run ends, its put-back fails (backend didn't answer), the marker is kept with Ended. Its kept trim was
// handed to auto-trim; afterwards the owner sets a hand trim. At the next start tunerRecover still undoes every
// trim back to the marker's "before" list: the run's kept trim AND the owner's later hand trim are gone.
func TestR7EndedMarkerUndoesLaterTrims(t *testing.T) {
	m := newHLMiner(t)
	captureNotify(t)
	newCwSim(t, func(int, float64) float64 { return 1 }) // (sets *authFile to a temp dir)
	test := planMV{MHz: 675, MV: 9200, PV: 9500}
	if err := applyTunePlan(test); err != nil {
		t.Fatal(err)
	}
	mk := tunerMarker{Started: time.Now().Add(-time.Hour).Unix(), Base: planMV{MHz: 600, MV: 9000, PV: 9300}, KeptSet: true, Testing: &test}
	b, _ := json.Marshal(mk)
	writeAtomic(tunerMarkerPath(), b, 0o644)
	cwSetTrim(2, 9, -12.5) // the run kept it (confirmed result)
	chipWatchAdopt([]chipTrim{{Chain: 2, Chip: 9, MHz: -12.5}}, "kept by the run")
	e := newTunerEngine(tunerConfig{})
	e.restoreFailed = true
	setCharge("tuner")
	tunerEnded(e)         // put-back failed: the marker stays (Ended)
	cwSetTrim(3, 20, -25) // the owner, days later, by hand
	r7Restart()
	r5Restart()
	tunerRecover(time.Second)
	got := r7Trims()
	if got[[2]int{3, 20}] != -25 {
		t.Errorf("BUG: the owner's hand trim set after the run ended was undone at the next start: %v", got)
	}
	if got[[2]int{2, 9}] != -12.5 {
		t.Errorf("BUG: the trim the run kept (and handed to auto-trim) was undone at the next start: %v", got)
	}
	_ = m
}

// 2. The watchdog ends a stuck run and can't put the miner back: the marker stays, without Ended, so the next
// start treats it as "cut short" and puts the old baseline over whatever the owner chose since (here Idle).
func TestR7ForceEndFailedPutBackOverridesLaterChoice(t *testing.T) {
	m := newHLMiner(t)
	captureNotify(t)
	newCwSim(t, func(int, float64) float64 { return 1 })
	w := fastWatchdog(t)
	_ = w
	tunerForceApply = func(planMV) error { return errTunerDead } // the backend doesn't answer
	e, _ := newSimEngine(fastCfg(), 8900, ckey{2, 5})
	e.cfg.DryRun = false
	base := planMV{MHz: 600, MV: 9000, PV: 9300}
	e.baseAtom.Store(&base)
	e.baseUsed = base
	e.startedAt = time.Now()
	writeTunerMarker(e)
	test := planMV{MHz: 675, MV: 9200, PV: 9500}
	applyTunePlan(test)
	markTunerTesting(test)
	setCharge("tuner")
	tunerForceEnd(e, "test")
	if err := applyPresetName("Idle"); err != nil { // the owner, after "choose Stock or a preset"
		t.Fatal(err)
	}
	r5Restart()
	tunerRecover(time.Second)
	if !m.isIdle() {
		t.Errorf("BUG: the Idle the owner chose after the stuck run was ended was woken at the next start onto the run's baseline (%d MHz)", m.planMHz())
	}
}

// 3. Auto-trim slows a chip: the trims file is written first, chipwatch.json only at the end of the tick. A
// power cut (or panel kill) between the two: after the restart the trim is in the file but auto-trim doesn't
// own it: it reads as a hand trim, is never judged/undone, and "Undo auto-trims" leaves it on.
func TestR7AutoTrimCrashBetweenTrimAndSave(t *testing.T) {
	s := newCwSim(t, func(chip int, trim float64) float64 {
		if chip == 12 {
			return 15
		}
		return 1
	})
	on := true
	if _, err := chipWatchSet(&on, false); err != nil {
		t.Fatal(err)
	}
	var snap []byte
	cwApply = func() error { // the moment the trim is in the file: what's on disk if the power goes now
		if snap == nil {
			snap, _ = os.ReadFile(chipWatchPath())
		}
		return nil
	}
	s.run(2*time.Hour + time.Minute)
	if s.trims()[12] != -12.5 {
		t.Fatalf("setup: chip 12 not trimmed: %v", s.trims())
	}
	os.WriteFile(chipWatchPath(), snap, 0o644) // power cut: the save after the trim never happened
	r7Restart()
	chipWatchTick()
	if _, err := chipWatchSet(nil, true); err != nil { // Undo auto-trims
		t.Fatal(err)
	}
	if tr := s.trims(); tr[12] != 0 {
		t.Errorf("BUG: after a power cut right after auto-trim slowed chip 12, Undo auto-trims leaves it at %+v (it reads as a hand trim nobody set)", tr[12])
	}
}

// 3b. the same window for an undo (a step that didn't help goes back): the state must stay consistent too.
func TestR7AutoTrimCrashDuringUndo(t *testing.T) {
	s := newCwSim(t, func(chip int, trim float64) float64 {
		if chip == 21 {
			return 15
		}
		return 1
	})
	on := true
	chipWatchSet(&on, false)
	s.run(2*time.Hour + time.Minute)
	if s.trims()[21] != -12.5 {
		t.Fatalf("setup: %v", s.trims())
	}
	var snap []byte
	cwApply = func() error {
		if snap == nil {
			snap, _ = os.ReadFile(chipWatchPath())
		}
		return nil
	}
	s.run(2 * time.Hour)
	if s.trims()[21] != 0 {
		t.Fatalf("setup: the undo didn't happen: %v", s.trims())
	}
	os.WriteFile(chipWatchPath(), snap, 0o644)
	r7Restart()
	chipWatchTick()
	v := chipWatchView()
	for _, o := range v["owned"].([]cwOwned) {
		if o.Chip == 21 {
			t.Errorf("after a cut during an undo, chip 21 still owned at %+v though the file says 0", o.MHz)
		}
	}
	if s.trims()[21] != 0 {
		t.Errorf("chip 21 came back trimmed: %v", s.trims())
	}
}

// 4. Strong-chip test interrupted, and the trims file is empty at the restart (cut short by the power cut that
// interrupted the test, or by the in-place fallback write on a full flash). The backup holds the owner's trims
// intact, but sctRecover rebuilds from the (empty) file and then deletes the backup: the owner's trims are lost.
func TestR7StrongRecoverEmptyTrimsFile(t *testing.T) {
	f := sctSetup(t)
	if e := sctStart(2, 0, 0); e != "" {
		t.Fatal(e)
	}
	r2Ticks(f, 50*time.Minute)
	sct.mu.Lock()
	sct.reset()
	sct.mu.Unlock()
	os.WriteFile(chipTrimPath(), nil, 0o644) // cut to nothing
	sctRecover()
	if m := sctTrimsOf(0); m[7] != -25 {
		t.Errorf("BUG: owner's b0.c7 -25 lost although the backup held it: %v", m)
	}
	if m := sctTrimsOf(2); m[40] != -12.5 {
		t.Errorf("BUG: owner's b2.c40 -12.5 lost although the backup held it: %v", m)
	}
	r2NoUnpassedPlus(t, nil)
}

// 4b. the reverse: the backup cut short, the file intact (already covered upstream by TestR2RecoverCorruptBackup);
// with both partial, nothing the owner set before may be lost and no raised chip stays raised.
func TestR7StrongRecoverBothPartial(t *testing.T) {
	f := sctSetup(t)
	if e := sctStart(2, 0, 0); e != "" {
		t.Fatal(e)
	}
	r2Ticks(f, 50*time.Minute)
	sct.mu.Lock()
	sct.reset()
	sct.mu.Unlock()
	// file: lost its owner lines but kept the raised ones; backup: lost its last line
	var keep []chipTrim
	for _, tr := range readChipTrims() {
		if tr.MHz > 0 {
			keep = append(keep, tr)
		}
	}
	keep = append(keep, chipTrim{Chain: 2, Chip: 40, MHz: -12.5})
	writeChipTrims(keep)
	b, _ := os.ReadFile(strongBackup())
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	var cut []string
	for _, l := range lines {
		if !strings.HasPrefix(l, "2 40 ") {
			cut = append(cut, l)
		}
	}
	os.WriteFile(strongBackup(), []byte(strings.Join(cut, "\n")+"\n"), 0o644)
	sctRecover()
	r2NoUnpassedPlus(t, nil)
	if m := sctTrimsOf(0); m[7] != -25 {
		t.Errorf("owner's b0.c7 lost: %v", m)
	}
	if m := sctTrimsOf(2); m[40] != -12.5 {
		t.Errorf("owner's b2.c40 lost: %v", m)
	}
}

// 5. Update from v1.5.21 during a chip self-test: chiptrim.pretest holds the owner's trims. If the write of the
// trims file fails (flash full: its .tmp can't be written), chipTestRecover deletes the backup anyway and says
// "your own trims are back": the self-test's trims stay, the owner's are gone for good.
func TestR7PretestRecoverWriteFails(t *testing.T) {
	newCwSim(t, func(int, float64) float64 { return 1 })
	writeChipTrims([]chipTrim{{Chain: 2, Chip: 5, MHz: -50}, {Chain: 2, Chip: 6, MHz: -50}}) // the self-test's
	owner := "# chain chip mhz\n2 40 -12.5\n0 7 -25\n"
	os.WriteFile(chipTestBackup(), []byte(owner), 0o644)
	os.Mkdir(chipTrimPath()+".tmp", 0o755) // the temp write fails (stands in for ENOSPC / a read-only remount)
	os.WriteFile(chipTrimPath()+".tmp/x", []byte("x"), 0o644)
	chipTestRecover()
	os.RemoveAll(chipTrimPath() + ".tmp")
	if _, err := os.Stat(chipTestBackup()); err != nil && r7Trims()[[2]int{2, 40}] != -12.5 {
		t.Errorf("BUG: the trims file couldn't be written but the backup with the owner's trims was deleted; trims now %v", r7Trims())
	}
}

// 6. chipwatch.json unreadable at start (garbage): auto-trim silently comes up off, forgets which trims are its
// own, and the next save overwrites the file. At least the owner must be told (status/log), not silence.
func TestR7ChipwatchGarbageIsSaid(t *testing.T) {
	newCwSim(t, func(int, float64) float64 { return 1 })
	on := true
	chipWatchSet(&on, false)
	os.WriteFile(chipWatchPath(), []byte(`{"on": true, "owned": [{"chain":2,"chip":`), 0o644)
	r7Restart()
	v := chipWatchView()
	log := ""
	for _, l := range v["log"].([]map[string]any) {
		log += l["msg"].(string) + "\n"
	}
	if v["on"] == false && !strings.Contains(log, "couldn't be read") {
		t.Errorf("auto-trim came up off with no word after its state file was cut short (log %q)", log)
	}
}

// 7. Roll-back compatibility: what the current panel writes must parse in v1.5.15's reader and as v1.5.15's
// chipwatch state (its readChipTrims took any "%d %d %g" line; its cwState had on/owned/nohelp/log).
func TestR7FilesReadByOldVersions(t *testing.T) {
	newCwSim(t, func(int, float64) float64 { return 1 })
	writeChipTrims([]chipTrim{{Chain: 2, Chip: 4, MHz: 50}, {Chain: 0, Chip: 9, MHz: -37.5}})
	b, _ := os.ReadFile(chipTrimPath())
	for _, l := range strings.Split(string(b), "\n") {
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		var c, ch int
		var m float64
		if n, _ := fmtSscanf(l, &c, &ch, &m); n != 3 {
			t.Errorf("v1.5.15 can't read %q", l)
		}
	}
	on := true
	chipWatchSet(&on, false)
	cwMu.Lock()
	cwSt.Budget, cwSt.BoardRate = map[string]int64{"2": 99}, map[string]float64{"2": 0.5}
	cwSave()
	cwMu.Unlock()
	var old struct {
		On     bool             `json:"on"`
		Owned  []cwOwned        `json:"owned"`
		NoHelp map[string]int64 `json:"nohelp"`
	}
	raw, _ := os.ReadFile(chipWatchPath())
	if err := json.Unmarshal(raw, &old); err != nil || !old.On {
		t.Errorf("old cwState can't read the new file: %v", err)
	}
}

func fmtSscanf(l string, a, b *int, c *float64) (int, error) {
	return fmt.Sscanf(l, "%d %d %g", a, b, c)
}
