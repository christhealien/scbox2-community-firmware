package main

// Fault-injection tests for the tuner and chip trims (tuner-sweep/faults). Each test names the fault and
// what the safe behaviour is; a failing test is a finding.

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// hangGuard: a run whose virtual clock passes limit is stopped (so the test ends) and reported as hung.
func hangGuard(e *tunerEngine, s *simState, limit time.Duration) *bool {
	hung := false
	sleep := e.sleep
	e.sleep = func(d time.Duration) {
		sleep(d)
		s.mu.Lock()
		over := s.vc.Sub(s.t0) > limit
		s.mu.Unlock()
		if over && !hung {
			hung = true
			e.Stop()
		}
	}
	return &hung
}

func checkPV(t *testing.T, s *simState) {
	t.Helper()
	for _, p := range s.applied {
		if p.PV != 0 && (p.PV < 9100 || p.PV > 9600) {
			t.Errorf("PV %d sent (outside 9100–9600): %+v", p.PV, p)
		}
	}
}

// F1: the chips' error counters stop answering for good in the middle of a test (the backend answers
// everything else: temperature, hashrate). The tuner must end the test/run and say why, not watch forever.
func TestFaultReadErrorsDiesMidTest(t *testing.T) {
	cfg := mapCfg()
	cfg.BaselineMHz, cfg.BaselineMV = 600, 9100
	e, s := newSimEngine(cfg, 8900, ckey{2, 5})
	read := e.readErrors
	dead := false
	apply := e.applyPlan
	e.applyPlan = func(p planMV) error {
		if p.MHz == 575 {
			dead = true // from the first curve test on, /dbg/icinfo fails
		}
		return apply(p)
	}
	n := 0
	e.readErrors = func() (map[ckey]int, error) {
		if dead {
			n++
			if n > 1 { // the test's first reading works, then never again
				return nil, fmt.Errorf("unexpected icinfo shape")
			}
		}
		return read()
	}
	hung := hangGuard(e, s, 24*time.Hour)
	e.Run()
	if *hung {
		tail := e.fullLog
		if len(tail) > 6 {
			tail = tail[len(tail)-6:]
		}
		t.Fatalf("the tuner watched a test for 24 h with no chip readings (Stop was needed); last log:\n%s", strings.Join(tail, "\n"))
	}
}

// F1b: the same in the baseline (judge=false).
func TestFaultReadErrorsDiesInBaseline(t *testing.T) {
	cfg := mapCfg()
	e, s := newSimEngine(cfg, 8900, ckey{2, 5})
	read := e.readErrors
	n := 0
	e.readErrors = func() (map[ckey]int, error) {
		n++
		if n > 1 {
			return nil, fmt.Errorf("no active chips in icinfo")
		}
		return read()
	}
	hung := hangGuard(e, s, 24*time.Hour)
	e.Run()
	if *hung {
		t.Fatalf("baseline never ended without chip readings")
	}
}

// F2: slowing a chip, the trims file is written but the live re-apply fails (the hook's report predates the
// last setting write, e.g. the fan hold's own write a moment before; or no report). The real trimSet.
// The tuner logs "no chip trim" — the file must not keep a trim nobody tracks.
func TestFaultTrimApplyFailsLeavesTrim(t *testing.T) {
	old := *authFile
	*authFile = filepath.Join(t.TempDir(), "auth.json")
	defer func() { *authFile = old }()
	oldStats := chipStatsPath
	chipStatsPath = filepath.Join(t.TempDir(), "chipstats.json")
	defer func() { chipStatsPath = oldStats }()
	// a fresh report... then a setting write (a fan change) right after it
	os.WriteFile(chipStatsPath, []byte(`{"t":1,"chains":[{"chain":2,"chips":46,"base10":6250,"clock10":[],"nonces":[]}]}`), 0o644)
	noteMinerChange("clock/voltage change")

	cfg := fastCfg()
	cfg.ChipTrim, cfg.ChipTrimMax = true, 3
	cfg.MinMV, cfg.MaxMV, cfg.MVStep = 8800, 9300, 100
	cfg.HoldMin = 2
	e, _ := newSimEngine(cfg, 9000, ckey{2, 5})
	real := newTunerEngine(cfg)
	real.sleep = e.sleep
	e.trimGet, e.trimSet = real.trimGet, real.trimSet
	e.hookStats = func() (chipStatsFile, bool) { s, _, ok := readChipStats(); return s, ok }
	e.adoptTrims = nil
	e.Run()
	log := strings.Join(e.fullLog, "\n")
	if !strings.Contains(log, "no chip trim (couldn't set the trim") {
		t.Fatalf("expected the trim to fail:\n%s", log)
	}
	if ts := readChipTrims(); len(ts) != 0 {
		t.Fatalf("the run said it made no trim, but the trims file now has %+v (runTrims %v): it goes live at the next clock change and stays as a 'hand' trim", ts, e.runTrims)
	}
}

// F3: a safety abort (here: too hot) during the trim check must end the run as an abort, say so, and not run
// the confirm.
func TestFaultHeatDuringTrimCheck(t *testing.T) {
	cfg := fastCfg()
	cfg.ChipTrim, cfg.ChipTrimMax = true, 3
	cfg.LowestMHz, cfg.HighestMHz, cfg.Step = 600, 625, 25
	cfg.MinMV, cfg.MaxMV, cfg.MVStep = 9000, 9100, 100
	cfg.HoldMin = 2
	e, s := newSimEngine(cfg, 0, ckey{2, 5})
	var trims map[[2]int]float64
	set := func() {
		s.failBelowMV = 0
		if s.mhz == 600 && trims[[2]int{2, 6}] > -12.5 {
			s.failBelowMV = 99999
		}
	}
	trims = simTrims(e, s, func(map[[2]int]float64) { s.mu.Lock(); set(); s.mu.Unlock() })
	apply := e.applyPlan
	inCheck := false
	e.applyPlan = func(p planMV) error {
		err := apply(p)
		s.mu.Lock()
		set()
		s.mu.Unlock()
		return err
	}
	e.readTemp = func() (float64, bool) {
		e.mu.Lock()
		ph := e.stepByKey("trimcheck")
		inCheck = ph != nil && ph.State == "run"
		e.mu.Unlock()
		if inCheck {
			return 95, true
		}
		return 65, true
	}
	e.Run()
	log := strings.Join(e.fullLog, "\n")
	d := planDump(e)
	if !e.aborted() {
		t.Errorf("heat in the trim check isn't a safety stop (abortWhy empty)")
	}
	if !strings.Contains(log, "95") || !strings.Contains(log, "ABORT") {
		t.Errorf("the log never says the trim check stopped for heat:\n%s", log)
	}
	if strings.Contains(d, "trimcheck[cut]") && strings.Contains(d, "confirm[") && !strings.Contains(d, "confirm[skip]") && !strings.Contains(d, "confirm[cut]") {
		t.Errorf("confirm ran after the trim check's abort:\n%s", d)
	}
	e.mu.Lock()
	note := e.stepByKey("trimcheck").Note
	e.mu.Unlock()
	if note == "stopped" {
		t.Errorf("the trim check's note says %q (as if stopped by the user) for a heat abort", note)
	}
	if len(trims) != 0 && trims[[2]int{2, 6}] == 0 {
		t.Errorf("trims %v", trims)
	}
}

// F4: lifting a trim in the trim check, the file is written but the live apply fails (real trimSet's
// behaviour). The run's record of what it kept must match the trims file.
func TestFaultTrimCheckLiftApplyFails(t *testing.T) {
	cfg := fastCfg()
	cfg.ChipTrim, cfg.ChipTrimMax = true, 3
	cfg.LowestMHz, cfg.HighestMHz, cfg.Step = 600, 625, 25
	cfg.MinMV, cfg.MaxMV, cfg.MVStep = 9000, 9100, 100
	cfg.HoldMin = 2
	e, s := newSimEngine(cfg, 0, ckey{2, 5})
	var trims map[[2]int]float64
	set := func() {
		s.failBelowMV = 0
		if s.mhz == 600 && trims[[2]int{2, 6}] > -12.5 {
			s.failBelowMV = 99999
		}
	}
	trims = simTrims(e, s, func(map[[2]int]float64) { s.mu.Lock(); set(); s.mu.Unlock() })
	inner := e.trimSet
	e.trimSet = func(chain, chip int, mhz float64) error {
		inner(chain, chip, mhz) // the file is written...
		e.mu.Lock()
		ph := e.stepByKey("trimcheck")
		checking := ph != nil && ph.State == "run"
		e.mu.Unlock()
		if checking {
			return fmt.Errorf("the chip hook's report isn't fresh yet: try again in a minute") // ...the live apply isn't
		}
		return nil
	}
	var adoptedTs []chipTrim
	e.adoptTrims = func(ts []chipTrim, _ string) { adoptedTs = ts }
	apply := e.applyPlan
	e.applyPlan = func(p planMV) error { err := apply(p); s.mu.Lock(); set(); s.mu.Unlock(); return err }
	e.Run()
	file := trims[[2]int{2, 6}]
	run := e.runTrims[[2]int{2, 6}]
	if file != run {
		t.Fatalf("after a failed lift the trims file says b2.c6 %+g MHz, the run's record (adopted: %+v) says %+g", file, adoptedTs, run)
	}
}

// F5: the panel restarts while a trim is being tried (the test that decides whether it stays is running).
// The next start must not keep that untested trim as one "kept by the tuning run".
func TestFaultPanelRestartMidTrimTest(t *testing.T) {
	newCwSim(t, func(int, float64) float64 { return 1 })
	captureNotify(t)
	e := newTunerEngine(fastCfg())
	e.startedAt = time.Now()
	e.baseUsed = planMV{MHz: 625, MV: 9100, PV: 9400}
	writeTunerMarker(e)
	// the run slowed b2.c6 for a retest and the panel died during that retest (made, not yet kept)
	writeChipTrims([]chipTrim{{Chain: 2, Chip: 6, MHz: -12.5}})
	tunerRecoverApply = func(planMV) error { return nil }
	t.Cleanup(func() { tunerRecoverApply = applyTunePlan })
	tunerRecover(time.Second)
	o := chipWatchView()["owned"].([]cwOwned)
	if len(readChipTrims()) != 0 || len(o) != 0 {
		t.Fatalf("an untested trim survived the restart: file %+v, auto-trim owns %+v (marked judged/kept)", readChipTrims(), o)
	}
}

// F7: the pool guard (or fan lock) stops the run: the log/plan must not say the user stopped it, and must
// not claim the miner went back to its old setting when the hold left it on Idle.
func TestFaultPoolHoldStopWording(t *testing.T) {
	cfg := mapCfg()
	cfg.BaselineMHz, cfg.BaselineMV = 600, 9100
	e, s := newSimEngine(cfg, 8900, ckey{2, 5})
	held := false
	// what snapshotMinerSetting's restore does under a hold: nothing, and no error
	restored := false
	e.before = func() (func() error, string) {
		return func() error {
			if held {
				return nil
			}
			restored = true
			return nil
		}, "600 MHz · 9100 mV"
	}
	e.held = func() error {
		if held {
			return fmt.Errorf("the pool guard holds the miner on Idle")
		}
		return nil
	}
	_ = restored
	sleep := e.sleep
	e.sleep = func(d time.Duration) {
		sleep(d)
		s.mu.Lock()
		late := s.vc.Sub(s.t0) > 6*time.Minute
		s.mu.Unlock()
		if late && !held {
			held = true
			e.StopFor("the pool guard: no pool answers") // stopTunerFor(...)
		}
	}
	e.Run()
	log := strings.Join(e.fullLog, "\n")
	d := planDump(e)
	e.mu.Lock()
	note := e.stepByKey("finish").Note
	e.mu.Unlock()
	var bad []string
	if strings.Contains(note, "stopped by you") {
		bad = append(bad, "finish note: "+note)
	}
	if strings.Contains(log, "going back to what the miner ran before the run") {
		bad = append(bad, "log claims the miner went back (it stays on Idle under the hold)")
	}
	if len(bad) > 0 {
		t.Fatalf("misleading after a pool-hold stop: %v\n%s", bad, d)
	}
}

// F8: the miner restarts mid-test with the backend up all along: counters reset to 0 and uptime resets.
// The test must fail (not judged clean), the run go on, PV stay in range.
func TestFaultMinerRestartMidTest(t *testing.T) {
	cfg := mapCfg()
	cfg.BaselineMHz, cfg.BaselineMV = 600, 9100
	e, s := newSimEngine(cfg, 8700, ckey{2, 5})
	var restartAt time.Time
	el := e.elapsed
	e.elapsed = func() (int, bool) {
		s.mu.Lock()
		at, now := restartAt, s.vc
		s.mu.Unlock()
		if !at.IsZero() && !now.Before(at) {
			return int(now.Sub(at).Seconds()), true
		}
		return el()
	}
	apply := e.applyPlan
	e.applyPlan = func(p planMV) error {
		if p.MHz == 575 && p.MV == 9000 && restartAt.IsZero() {
			s.mu.Lock()
			restartAt = s.vc.Add(time.Minute) // a minute into the test
			s.mu.Unlock()
		}
		return apply(p)
	}
	sleep := e.sleep
	reset := false
	e.sleep = func(d time.Duration) {
		sleep(d)
		s.mu.Lock()
		if !restartAt.IsZero() && !s.vc.Before(restartAt) && !reset {
			reset = true
			for k := range s.counts {
				s.counts[k] = 0
			}
		}
		s.mu.Unlock()
	}
	e.Run()
	found := false
	for _, r := range e.results {
		if r.MHz == 575 && r.MV == 9000 && strings.Contains(r.Reason, "restarted") {
			found = true
		}
	}
	if !found {
		t.Fatalf("restart not caught: %+v", e.results)
	}
	if e.aborted() {
		t.Fatalf("a restart aborted the run: %s", e.abortReason())
	}
	checkPV(t, s)
}

// F9: odd readings — NaN/Inf/negative/huge temperature, GHS and voltage. NaN must not count as a
// temperature (else no heat abort and the fan maths goes NaN), PV stays in range, the run ends.
func TestFaultNaNTemperature(t *testing.T) {
	cfg := mapCfg()
	cfg.FanHold = true
	cfg.BaselineMHz, cfg.BaselineMV = 600, 9100
	e, s := newSimEngine(cfg, 8900, ckey{2, 5})
	var fans []int
	e.applyFan = func(p int) error { fans = append(fans, p); return nil }
	e.readTemp = func() (float64, bool) { return math.NaN(), true }
	hung := hangGuard(e, s, 48*time.Hour)
	e.Run()
	if *hung {
		t.Fatal("hung")
	}
	for _, f := range fans {
		if f < cfg.FanMinPct || f > 100 {
			t.Fatalf("fan %d %% sent from a NaN temperature: %v", f, fans)
		}
	}
	if !e.aborted() {
		t.Fatalf("a NaN temperature for the whole run was treated as a real reading (no abort): results %d", len(e.results))
	}
}

func TestFaultOddReadingsDontHang(t *testing.T) {
	for name, f := range map[string]func(e *tunerEngine){
		"ghs NaN":      func(e *tunerEngine) { e.readGHS = func() (float64, bool) { return math.NaN(), true } },
		"ghs Inf":      func(e *tunerEngine) { e.readGHS = func() (float64, bool) { return math.Inf(1), true } },
		"ghs negative": func(e *tunerEngine) { e.readGHS = func() (float64, bool) { return -5, true } },
		"work missing": func(e *tunerEngine) { e.readWork = func() (float64, bool) { return 0, false } },
		"volt NaN":     func(e *tunerEngine) { e.readVolt = func() (float64, bool) { return math.NaN(), true } },
		"elapsed missing": func(e *tunerEngine) {
			e.elapsed = func() (int, bool) { return 0, false }
		},
		"errors negative": func(e *tunerEngine) {
			r := e.readErrors
			e.readErrors = func() (map[ckey]int, error) {
				m, err := r()
				m[ckey{2, 7}] = -3
				return m, err
			}
		},
		"temp huge": func(e *tunerEngine) { e.readTemp = func() (float64, bool) { return 1e9, true } },
		"temp -Inf": func(e *tunerEngine) { e.readTemp = func() (float64, bool) { return math.Inf(-1), true } },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := mapCfg()
			cfg.BaselineMHz, cfg.BaselineMV = 600, 9100
			e, s := newSimEngine(cfg, 8900, ckey{2, 5})
			f(e)
			hung := hangGuard(e, s, 48*time.Hour)
			e.Run()
			if *hung {
				t.Fatal("hung")
			}
			checkPV(t, s)
			if e.isRunning() {
				t.Fatal("still running")
			}
		})
	}
}

// F10: every hook fails. The run must end, say why, and try to put the miner back.
func TestFaultEverythingFails(t *testing.T) {
	cfg := mapCfg()
	cfg.ChipTrim = true
	e, s := newSimEngine(cfg, 8900, ckey{2, 5})
	bad := fmt.Errorf("the miner's backend (the OEM's minerd) didn't answer: x")
	e.readErrors = func() (map[ckey]int, error) { return nil, bad }
	e.readTemp = func() (float64, bool) { return 0, false }
	e.readGHS = func() (float64, bool) { return 0, false }
	e.readWork = func() (float64, bool) { return 0, false }
	e.elapsed = func() (int, bool) { return 0, false }
	e.applyPlan = func(planMV) error { return bad }
	e.applyFan = func(int) error { return bad }
	e.trimSet = func(int, int, float64) error { return bad }
	e.trimGet = func() map[[2]int]float64 { return nil }
	e.hookStats = func() (chipStatsFile, bool) { return chipStatsFile{}, false }
	e.restoreWait = 5 * time.Minute
	hung := hangGuard(e, s, 6*time.Hour)
	e.Run()
	if *hung {
		t.Fatal("hung")
	}
	log := strings.Join(e.fullLog, "\n")
	if !strings.Contains(log, "STOP:") || !strings.Contains(log, "WARNING: could not restore baseline") || !e.restoreFailed {
		t.Fatalf("log:\n%s", log)
	}
}

// F11: chain/chip indexes the hook doesn't have (chain 7, chip 99, negative): no trim, said why.
func TestFaultOutOfRangeChips(t *testing.T) {
	st := chipStatsFile{}
	st.Chains = append(st.Chains, struct {
		Chain   int   `json:"chain"`
		Chips   int   `json:"chips"`
		Base10  int   `json:"base10"`
		Clock10 []int `json:"clock10"`
		Nonces  []int `json:"nonces"`
	}{Chain: 2, Chips: 46, Base10: 6250}, struct {
		Chain   int   `json:"chain"`
		Chips   int   `json:"chips"`
		Base10  int   `json:"base10"`
		Clock10 []int `json:"clock10"`
		Nonces  []int `json:"nonces"`
	}{Chain: 0, Chips: 46, Base10: 6250})
	min := map[int]int{2: 1, 7: 1, 0: 1}
	for _, k := range []ckey{{7, 5}, {2, 99}, {2, 47}, {2, -1}, {-1, 5}, {2, 0}} {
		if c, ch, ok := errToHook(k, min, st); ok {
			t.Errorf("%v mapped to chain %d chip %d", k, c, ch)
		}
	}
}

// F12: garbage / truncated files the readers parse.
func TestFaultGarbageFiles(t *testing.T) {
	old := *authFile
	*authFile = filepath.Join(t.TempDir(), "auth.json")
	defer func() { *authFile = old }()
	oldStats := chipStatsPath
	chipStatsPath = filepath.Join(t.TempDir(), "chipstats.json")
	defer func() { chipStatsPath = oldStats }()
	for _, body := range []string{"", "{", `{"t":1,"chains":[{"chain":2,"chips":46,"base10":62`, "\x00\x00\x00", `[1,2,3]`, `{"chains":"x"}`} {
		os.WriteFile(chipStatsPath, []byte(body), 0o644)
		if _, _, ok := readChipStats(); ok && body != "" {
			t.Errorf("garbage stats %q read as ok", body)
		}
		if _, err := reapplyTrims(); err == nil {
			t.Errorf("reapply with garbage stats %q succeeded", body)
		}
	}
	os.WriteFile(chipTrimPath(), []byte("2 6 -12.5\n2 6 -25\n7 99 -12.5\n-1 -1 -12.5\n2 7 abc\n2 8 nan\n\x00\x01garbage\n2 9 -12.5junk\n"), 0o644)
	ts := readChipTrims()
	t.Logf("trims parsed from a messy file: %+v", ts)
	e := newTunerEngine(fastCfg())
	m := e.trimGet()
	if v := m[[2]int{2, 6}]; v != -25 {
		t.Errorf("duplicate lines: the hook uses the last (-25), the tuner sees %+g", v)
	}
	// a write keeps working on that file and leaves a single line per chip
	if err := cwSetTrim(2, 6, -12.5); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, t2 := range readChipTrims() {
		if t2.Chain == 2 && t2.Chip == 6 {
			n++
			if t2.MHz != -12.5 {
				t.Errorf("after a write b2.c6 is %+g", t2.MHz)
			}
		}
	}
	if n != 1 {
		t.Logf("after a write b2.c6 is on %d lines (harmless, same value)", n)
	}
}

// F13: the trims file can't be written (read-only dir / disk full): no trim, said why, the run goes on.
func TestFaultTrimsNotWritable(t *testing.T) {
	if os.Getuid() == 0 {
		t.Log("running as root: emulate with a path whose dir is a file")
	}
	old := *authFile
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "notadir"), nil, 0o644)
	*authFile = filepath.Join(dir, "notadir", "auth.json") // chiptrim's dir is a file: every write fails
	defer func() { *authFile = old }()
	cfg := fastCfg()
	cfg.ChipTrim, cfg.ChipTrimMax = true, 3
	cfg.MinMV, cfg.MaxMV, cfg.MVStep = 8800, 9300, 100
	cfg.HoldMin = 2
	e, s := newSimEngine(cfg, 9000, ckey{2, 5})
	real := newTunerEngine(cfg)
	real.sleep = e.sleep
	e.trimGet, e.trimSet = real.trimGet, real.trimSet
	e.hookStats = func() (chipStatsFile, bool) {
		var f chipStatsFile
		f.Chains = append(f.Chains, struct {
			Chain   int   `json:"chain"`
			Chips   int   `json:"chips"`
			Base10  int   `json:"base10"`
			Clock10 []int `json:"clock10"`
			Nonces  []int `json:"nonces"`
		}{Chain: 2, Chips: 46, Base10: 6250})
		return f, true
	}
	e.adoptTrims = nil
	hung := hangGuard(e, s, 48*time.Hour)
	e.Run()
	log := strings.Join(e.fullLog, "\n")
	if *hung || !strings.Contains(log, "no chip trim (couldn't set the trim") || len(e.curve) != 1 || e.curve[0].MV != 9000 {
		t.Fatalf("hung %v curve %+v\n%s", *hung, e.curve, log)
	}
}

// F14: applying a setting takes minutes (minerd slow) / is refused (pool hold): the run ends, puts back.
func TestFaultApplySlowAndRefused(t *testing.T) {
	cfg := mapCfg()
	cfg.BaselineMHz, cfg.BaselineMV = 600, 9100
	e, s := newSimEngine(cfg, 8900, ckey{2, 5})
	apply := e.applyPlan
	calls := 0
	e.applyPlan = func(p planMV) error {
		calls++
		e.sleep(4 * time.Minute) // minerd takes 4 minutes to answer each write
		if p.MHz == 575 && p.MV == 9000 {
			return fmt.Errorf("the pool guard holds the miner on Idle")
		}
		return apply(p)
	}
	hung := hangGuard(e, s, 48*time.Hour)
	e.Run()
	log := strings.Join(e.fullLog, "\n")
	if *hung || !e.aborted() || !strings.Contains(log, "apply failed: the pool guard") {
		t.Fatalf("hung %v aborted %v\n%s", *hung, e.aborted(), log)
	}
	if last := s.applied[len(s.applied)-1]; last.MHz != 600 || last.MV != 9100 {
		t.Fatalf("not put back: %+v", last)
	}
	checkPV(t, s)
}

// F15: a broken restart marker (power cut while it was written; it's written in place, not atomically): the
// next start must still say a run was cut short, not drop it silently and leave the test setting.
func TestFaultBrokenTunerMarker(t *testing.T) {
	newCwSim(t, func(int, float64) float64 { return 1 })
	got := captureNotify(t)
	os.WriteFile(tunerMarkerPath(), []byte(`{"started":1790000000,"base":{"mhz":6`), 0o644)
	applied := 0
	tunerRecoverApply = func(planMV) error { applied++; return nil }
	t.Cleanup(func() { tunerRecoverApply = applyTunePlan })
	tunerRecover(time.Second)
	if len(*got) == 0 {
		t.Fatalf("a run cut short with a broken marker: nothing said, nothing put back (applied %d)", applied)
	}
}

// F16: the stats file is from before the last setting change: re-apply refuses it (it would re-send an old
// clock) — sound, checked here.
func TestFaultStaleStatsRefused(t *testing.T) {
	oldStats := chipStatsPath
	chipStatsPath = filepath.Join(t.TempDir(), "chipstats.json")
	defer func() { chipStatsPath = oldStats }()
	os.WriteFile(chipStatsPath, []byte(`{"t":1,"chains":[{"chain":2,"chips":46,"base10":6250}]}`), 0o644)
	old := time.Now().Add(-2 * time.Minute)
	os.Chtimes(chipStatsPath, old, old)
	if _, err := reapplyTrims(); err == nil || !strings.Contains(err.Error(), "fresh") {
		t.Fatalf("stale stats: %v", err)
	}
	os.Chtimes(chipStatsPath, time.Now(), time.Now())
	noteMinerChange("clock/voltage change")
	if _, err := reapplyTrims(); err == nil || !strings.Contains(err.Error(), "fresh") {
		t.Fatalf("stats from before the last change: %v", err)
	}
}

// F17: the chip hook isn't loaded (sclite_chip.so absent / hook not reporting): no trim, said why, the run
// goes on as without trims.
func TestFaultHookAbsent(t *testing.T) {
	cfg := fastCfg()
	cfg.ChipTrim, cfg.ChipTrimMax = true, 3
	cfg.MinMV, cfg.MaxMV, cfg.MVStep = 8800, 9300, 100
	cfg.HoldMin = 2
	e, _ := newSimEngine(cfg, 9000, ckey{2, 5})
	sets := 0
	e.trimSet = func(int, int, float64) error { sets++; return nil }
	e.trimGet = func() map[[2]int]float64 { return nil }
	e.hookStats = func() (chipStatsFile, bool) { return chipStatsFile{}, false }
	e.adoptTrims = nil
	e.Run()
	log := strings.Join(e.fullLog, "\n")
	if sets != 0 || !strings.Contains(log, "no chip trim (the chip hook isn't reporting)") || len(e.curve) != 1 || e.curve[0].MV != 9000 {
		t.Fatalf("sets %d curve %+v\n%s", sets, e.curve, log)
	}
}
