package main

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestTunerEndText(t *testing.T) {
	// finished, with the best setting
	e, _ := newSimEngine(fastCfg(), 8900, ckey{2, 5})
	started := ""
	e.onStart = func() { started = e.startText() }
	e.Run()
	if !strings.HasPrefix(started, "tuning run started: mapping 625 MHz, 1 min per test") || !strings.Contains(started, "back to 625 MHz") {
		t.Fatalf("start: %q", started)
	}
	if s := e.endText(); !strings.HasPrefix(s, "tuning run finished in ") || !strings.Contains(s, "best "+e.best.text()) {
		t.Fatalf("finished: %q", s)
	}
	// stopped by hand
	e, _ = newSimEngine(fastCfg(), 8900, ckey{2, 5})
	e.onStart = func() { e.Stop() }
	e.Run()
	if s := e.endText(); !strings.HasPrefix(s, "tuning run stopped by hand after") || !strings.Contains(s, "back on 625 MHz") {
		t.Fatalf("stopped: %q", s)
	}
	// aborted: too hot
	e, s := newSimEngine(fastCfg(), 0, ckey{2, 5})
	e.readTemp = func() (float64, bool) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.vc.Sub(s.t0).Minutes() > 1.2 {
			return 90, true
		}
		return 65, true
	}
	e.Run()
	if m := e.endText(); !strings.HasPrefix(m, "tuning run aborted after") || !strings.Contains(m, "90") || notifyPriority(evTuning, m) != 4 {
		t.Fatalf("aborted: %q", m)
	}
	// confirm that holds / doesn't
	cfg := fastCfg()
	cfg.Mode = "confirm"
	cfg.ConfirmMHz, cfg.ConfirmMV, cfg.ConfirmHours = 625, 9000, 0.05
	e, _ = newSimEngine(cfg, 8800, ckey{2, 5})
	e.Run()
	if m := e.endText(); !strings.Contains(m, "confirmed 625 MHz") || !strings.Contains(m, "stays on it") {
		t.Fatalf("confirm: %q", m)
	}
	cfg.ConfirmMV = 8900
	e, _ = newSimEngine(cfg, 9000, ckey{2, 5})
	e.Run()
	if m := e.endText(); !strings.Contains(m, "didn't hold") || !strings.HasPrefix(m, "tuning run finished") {
		t.Fatalf("confirm rejected: %q", m)
	}
}

// a restart mid-run: the next start puts the miner back on the baseline and says so
func TestTunerRecover(t *testing.T) {
	newCwSim(t, func(int, float64) float64 { return 1 })
	got := captureNotify(t)
	writeChipTrims([]chipTrim{{Chain: 2, Chip: 4, MHz: -25}}) // set by hand before the run
	e := newTunerEngine(fastCfg())
	e.startedAt = time.Now()
	e.baseUsed = planMV{MHz: 625, MV: 9100}
	writeTunerMarker(e)
	writeChipTrims([]chipTrim{{Chain: 2, Chip: 4, MHz: -25}, {Chain: 2, Chip: 9, MHz: -12.5}}) // the run made one
	markTunerTrims(map[[2]int]float64{{2, 9}: -12.5})                                          // and kept it
	var applied []planMV
	tunerRecoverApply = func(p planMV) error { applied = append(applied, p); return nil }
	t.Cleanup(func() { tunerRecoverApply = applyTunePlan })
	tunerRecover(time.Second)
	if len(applied) != 1 || applied[0].MHz != 625 || applied[0].MV != 9100 {
		t.Fatalf("applied %v", applied)
	}
	if _, err := os.Stat(tunerMarkerPath()); err == nil {
		t.Fatal("marker left behind")
	}
	if len(*got) != 1 || !strings.Contains((*got)[0], "cut short") || !strings.Contains((*got)[0], "put back on 625 MHz") || !strings.Contains((*got)[0], "1 chip trim(s)") {
		t.Fatalf("notified: %v", *got)
	}
	// cut short: the run's trim comes off too, the hand trim from before the run stays
	if ts := readChipTrims(); len(ts) != 1 || ts[0].Chip != 4 || ts[0].MHz != -25 {
		t.Fatalf("trims after recover: %+v", ts)
	}
	if o := chipWatchView()["owned"].([]cwOwned); len(o) != 0 {
		t.Fatalf("adopted: %+v", o)
	}
	// nothing to do without a marker
	tunerRecover(time.Second)
	if len(applied) != 1 || len(*got) != 1 {
		t.Fatal("ran without a marker")
	}
}

func TestTunerRecoverSaysWhatWasTested(t *testing.T) {
	newCwSim(t, func(int, float64) float64 { return 1 })
	got := captureNotify(t)
	e := newTunerEngine(fastCfg())
	e.startedAt = time.Now()
	e.baseUsed = planMV{MHz: 615, MV: 9100, PV: 9400}
	writeTunerMarker(e)
	markTunerTesting(planMV{MHz: 675, MV: 9000, PV: 9300})
	tunerRecoverApply = func(p planMV) error { return nil }
	t.Cleanup(func() { tunerRecoverApply = applyTunePlan })
	tunerRecover(time.Second)
	if len(*got) != 1 || !strings.Contains((*got)[0], "while testing 675 MHz") || !strings.Contains((*got)[0], "unsafe on this miner") {
		t.Fatalf("notified: %v", *got)
	}
}
