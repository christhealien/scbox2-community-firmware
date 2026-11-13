package main

// L14-voltage: every path that ends in a /mcb/setting PUT keeps PV 9100–9600, run voltage in its range and the
// clock inside the window, whatever a file on disk says. The fake backend (r5Setup) records every PUT.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// l14Bad: a PUT that would hand the miner a manual plan outside today's limits (locked clock window).
func l14Bad(s *r5Stock) []string {
	var bad []string
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.puts {
		if m, _ := p["manual"].(bool); !m {
			continue
		}
		mp, _ := p["manualPowerplan"].(string)
		if strings.HasPrefix(strings.TrimSpace(mp), "-") {
			bad = append(bad, mp+" (negative clock)")
			continue
		}
		pp, ok := parsePowerplan(mp)
		if !ok {
			bad = append(bad, mp+" (unparsable)")
			continue
		}
		if pp.FreqMHz == 0 {
			continue
		}
		if pp.PVMv < pvHardMin || pp.PVMv > pvHardMax || pp.VrunMv < vrunHardMin || pp.VrunMv > vrunHardMax ||
			pp.FreqMHz > clockWindowHi() || pp.FreqMHz < freqHardMin {
			bad = append(bad, mp)
		}
	}
	return bad
}

// The chokepoint itself: a negative clock skips the window, the PV floor and the run-voltage range.
func TestL14MutatePlanNegativeClock(t *testing.T) {
	s := r5Setup(t, false)
	_, err := mutatePlan(func(p *powerplan) { p.FreqMHz, p.VrunMv, p.PVMv = -25, 5000, 5000 })
	if bad := l14Bad(s); len(bad) > 0 {
		t.Fatalf("mutatePlan sent %q to the miner (err=%v)", bad, err)
	}
}

// Reached from disk: a hand-edited (or damaged) schedule-before.json drives the schedule's "back to before".
func TestL14ScheduleBeforeFromDiskNegativeClock(t *testing.T) {
	s := r5Setup(t, false)
	now := time.Now()
	if now.Hour() == 0 && now.Minute() < 5 {
		t.Skip("too near midnight for the HH:MM rules")
	}
	schedPath = filepath.Join(t.TempDir(), "schedule.json")
	t.Cleanup(func() { schedPath = ""; schedBefore, schedBeforeBy, lastSchName, lastSchKey = nil, "", "", -1 })
	b, _ := json.Marshal(schedule{Enabled: true, Rules: []schedRule{{Start: now.Add(-time.Minute).Format("15:04"), Days: []int{0, 1, 2, 3, 4, 5, 6}, Preset: schedBack}}})
	writeAtomic(schedPath, b, 0o600)
	rec, _ := json.Marshal(schedBeforeRec{Before: powerplan{FreqMHz: -25, VrunMv: 5000, PVMv: 5000, Fan1: 50, Fan2: 50}, Name: "Eco"})
	os.WriteFile(schedBeforePath(), rec, 0o600)
	lastSchKey, lastSchName, schedBefore, schedBeforeBy = -1, "", nil, ""
	setCharge("")
	scheduleTick()
	if bad := l14Bad(s); len(bad) > 0 {
		t.Fatalf("the schedule's \"back to before\" sent %q to the miner", bad)
	}
}

// The fan check re-run's restart record is written to the stock API verbatim (putSettingGen(raw)); its plan is
// only re-checked for PV/fans afterwards, through mutatePlan, where an unchanged clock isn't checked at all.
func TestL14FanRerunRecordRawPlan(t *testing.T) {
	s := r5Setup(t, true) // left on Idle by the cut-short re-run
	old := fanRerunRecoverWait
	fanRerunRecoverWait = 0
	t.Cleanup(func() { fanRerunRecoverWait = old })
	r5Restart()
	setCharge("")
	setting := map[string]any{"manual": true, "tempcontrol": true, "select": float64(0),
		"manualPowerplan": "850 MHz 9300 V 60 RPM 60 RPM PV 9900",
		"powerplans": []any{map[string]any{"info": "615 MHz 9100 V 40 RPM 40 RPM PV 9400", "level": 0},
			map[string]any{"info": "0 MHz 0 V 100 RPM 100 RPM", "level": 3}}}
	raw, _ := json.Marshal(setting)
	b, _ := json.Marshal(fanRerunRec{At: time.Now().Add(-5 * time.Minute).Unix(), Setting: raw, Charge: ""})
	if err := writeAtomic(fanRerunPath(), b, 0o644); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.puts = nil
	s.mu.Unlock()
	fanRerunRecover()
	if bad := l14Bad(s); len(bad) > 0 {
		t.Fatalf("the fan re-run's put-back sent %q to the miner (clock locked to %d MHz, PV ≤ %d)", bad, clockWindowHi(), pvHardMax)
	}
}

// a record within the clock window but past the supply's PV: put back at PV 9600, never 9900 first
func TestL14FanRerunRecordHighPV(t *testing.T) {
	s := r5Setup(t, true)
	old := fanRerunRecoverWait
	fanRerunRecoverWait = 0
	t.Cleanup(func() { fanRerunRecoverWait = old })
	r5Restart()
	setCharge("")
	setting := map[string]any{"manual": true, "tempcontrol": true, "select": float64(0),
		"manualPowerplan": "650 MHz 9300 V 60 RPM 60 RPM PV 9900",
		"powerplans": []any{map[string]any{"info": "615 MHz 9100 V 40 RPM 40 RPM PV 9400", "level": 0},
			map[string]any{"info": "0 MHz 0 V 100 RPM 100 RPM", "level": 3}}}
	raw, _ := json.Marshal(setting)
	b, _ := json.Marshal(fanRerunRec{At: time.Now().Add(-5 * time.Minute).Unix(), Setting: raw})
	writeAtomic(fanRerunPath(), b, 0o644)
	s.mu.Lock()
	s.puts = nil
	s.mu.Unlock()
	fanRerunRecover()
	if bad := l14Bad(s); len(bad) > 0 {
		t.Fatalf("the fan re-run's put-back sent %q to the miner", bad)
	}
	s.mu.Lock()
	mp, _ := s.setting["manualPowerplan"].(string)
	s.mu.Unlock()
	if pp, ok := parsePowerplan(mp); !ok || pp.FreqMHz != 650 || pp.PVMv != 9600 {
		t.Fatalf("not put back as 650 MHz at PV 9600: %q", mp)
	}
}
