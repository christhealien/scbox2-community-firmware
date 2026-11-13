package main

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// From an owner's v1.5.18 run: 575–700 MHz all passed only narrowly, 550 MHz solidly. The confirm must keep a
// solid setting among the ones it tries, so nothing narrow failing leaves the run with nothing confirmed.
func TestConfirmKeepsASolidFallback(t *testing.T) {
	c := fastCfg()
	e, s := newSimEngine(c, 0, ckey{2, 5})
	s.failAboveMHz = 600
	e.passes = []planMV{{MHz: 550, MV: 8800}, {MHz: 625, MV: 8850}, {MHz: 650, MV: 9000}, {MHz: 675, MV: 9050}, {MHz: 700, MV: 9150}}
	e.marginal = map[[2]int]string{{625, 8850}: "close", {650, 9000}: "close", {675, 9050}: "close", {700, 9150}: "close"}
	e.confirmWinner(planMV{MHz: 625, MV: 9100})
	if e.best == nil || e.best.MHz != 550 || !e.confirmed {
		t.Fatalf("confirmed %v (want the solid 550 MHz after the narrow ones failed); log:\n%s", e.best, strings.Join(e.logbuf, "\n"))
	}
	if !strings.Contains(strings.Join(e.logbuf, "\n"), "passed with room to spare") {
		t.Fatal("no line saying the solid setting was kept as a fallback")
	}
}

// The confirmed setting ran clean in the map before the run's later trims: those come off.
func TestTrimsNotNeededAtConfirmed(t *testing.T) {
	e, _ := newSimEngine(fastCfg(), 0, ckey{0, 5})
	set := map[[2]int]float64{}
	e.trimSet = func(b, c int, mhz float64) error { set[[2]int{b, c}] = mhz; return nil }
	e.runTrims = map[[2]int]float64{{3, 32}: -25, {3, 30}: -12.5, {3, 3}: -12.5}
	e.trimsAtPass = map[[2]int]map[[2]int]float64{
		{550, 8800}: {},               // clean before any trim
		{600, 8800}: {{3, 32}: -12.5}, // clean with one
	}
	e.trimsNotNeeded(planMV{MHz: 600, MV: 8800})
	if set[[2]int{3, 32}] != -12.5 || set[[2]int{3, 30}] != 0 || set[[2]int{3, 3}] != 0 || len(e.runTrims) != 1 {
		t.Fatalf("at 600: set %v, kept %v", set, e.runTrims)
	}
	e.trimsNotNeeded(planMV{MHz: 550, MV: 8800})
	if len(e.runTrims) != 0 || set[[2]int{3, 32}] != 0 {
		t.Fatalf("at 550: set %v, kept %v", set, e.runTrims)
	}
	// a setting the map never ran clean: nothing changes
	e.runTrims = map[[2]int]float64{{3, 32}: -25}
	e.trimsNotNeeded(planMV{MHz: 700, MV: 9150})
	if len(e.runTrims) != 1 {
		t.Fatal("trims taken off for a setting with no clean map test")
	}
}

// The owner's run ended its presets phase on a 3-minute gap in the chip readings (Goldshell's backend answered
// "HTTP 500" while mining went on). A gap that ends within the grace time restarts the test instead.
func TestTunerReadingGapRestartsTest(t *testing.T) {
	c := fastCfg()
	c.PollS = 15
	e, s := newSimEngine(c, 0, ckey{2, 5})
	read := e.readErrors
	e.readErrors = func() (map[ckey]int, error) {
		s.mu.Lock()
		el := s.vc.Sub(s.t0)
		s.mu.Unlock()
		if el > 90*time.Second && el < 90*time.Second+5*time.Minute {
			return nil, errors.New("stock login failed (HTTP 500)")
		}
		return read()
	}
	e.Run()
	log := strings.Join(e.logbuf, "\n")
	if !strings.Contains(log, "starts over") || strings.Contains(log, "ABORT") || e.abortWhy != "" {
		t.Fatalf("a 5-minute gap ended the run instead of restarting the test:\n%s", log)
	}
}

// The owner's run: the slower presets are tested warmer (quieter fans), and a board that's clean at the map's
// temperature failed them on the heat (500 and 475 MHz failing at 73–75 °C after 550 and 525 passed at 71–72).
// After its warm tries fail, a preset gets one try at the map's temperature.
func TestPresetTriesTheMapTemperature(t *testing.T) {
	cfg := mapCfg()
	cfg.BuildPresets = true
	cfg.FanHold = true
	cfg.FanTargetC = 70
	cfg.WarmestC = 76
	cfg.PresetMin = 2
	cfg.BaselineMHz, cfg.BaselineMV = 600, 9100
	e, s := newSimEngine(cfg, 0, ckey{2, 5})
	e.applyFan = func(int) error { return nil }
	e.readTemp = func() (float64, bool) { return e.holdTarget, true } // the fans hold it exactly
	sl := e.sleep
	e.sleep = func(d time.Duration) {
		sl(d)
		if e.holdTarget > 72 { // too warm for this board: a chip throws errors
			s.mu.Lock()
			s.counts[s.spike] += 0.9 * d.Seconds()
			s.mu.Unlock()
		}
	}
	hung := hangGuard(e, s, 72*time.Hour)
	e.Run()
	if *hung {
		t.Fatal("hung")
	}
	log := strings.Join(e.logbuf, "\n")
	if !strings.Contains(log, "at the map's 70°C") {
		t.Fatalf("no try at the map's temperature:\n%s", log)
	}
	if !strings.Contains(log, "RESULT: saved 4 presets") {
		t.Fatalf("the warm-failing presets weren't saved at the cooler temperature:\n%s", log)
	}
}
