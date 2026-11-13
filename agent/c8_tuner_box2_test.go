package main

// C8: the auto-tuner on the SC Box II. Proves, on the existing tuner harness (newSimEngine's fake miner,
// virtual clock), with the Box II profile active:
//   - shipped OFF: every tuner endpoint refuses and no run can start;
//   - with the tuner commissioned but the level's voltage direction not in the profile, the tuner tunes the
//     clock only: every write is the stock level 42, whatever the form sent;
//   - every write is a whole clock on the measured 12.5 MHz PLL grid inside the window, and a voltage level
//     inside [42, 61] (box2TunerWriteErr checked on every fake write); never a PV or millivolts;
//   - the defaults are Box II data (525–690 MHz in 12.5 MHz steps, levels, the profile's thermal limits, the
//     fans left to Goldshell), the texts show Goldshell's level with the measured C4 volts, never PV/mV/watts;
//   - the thermal guard (abort ≤ 84, from the profile) and the hardware-error judge stop/fail settings, and
//     the run always ends on the stock Hashrate plan or on the best setting that passed its confirm;
//   - with a direction set (as the C4 result would), voltage steps walk codes from the least-voltage end;
//   - a chip map that doesn't fit the model (46 chips) stops the run at the baseline;
//   - SC Lite: unchanged.

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
)

// c8Box2 switches to the Box II profile (feature set feat, direction dir) for one test, then restores.
func c8Box2(t *testing.T, tuner bool, dir vcodeDir) {
	t.Helper()
	p := profSCBox2
	p.Feat.Tuner, p.Feat.ClockControl, p.Feat.VoltageControl = tuner, tuner, tuner
	p.VCodeDir = dir
	b10SetModel(t, p)
}

// c8Engine: newSimEngine (the tuner harness's fake miner) on one 36-chip board; every write is checked
// as the miner would get it and recorded in codes.
func c8Engine(t *testing.T, cfg tunerConfig, failBelowLevel int, failAboveMHz int) (*tunerEngine, *simState, *[]planMV) {
	t.Helper()
	e, s := newSimEngine(cfg, failBelowLevel, ckey{0, 7})
	s.mv, s.mhz = e.cfg.BaselineMV, e.cfg.BaselineMHz
	e.readErrors = func() (map[ckey]int, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		out := map[ckey]int{}
		for i := 1; i <= 36; i++ {
			k := ckey{B: 0, I: i}
			out[k] = int(s.counts[k])
		}
		return out, nil
	}
	written := &[]planMV{}
	e.applyPlan = func(p planMV) error {
		h := p.hw()
		if err := box2TunerWriteErr(h); err != nil {
			t.Errorf("the tuner tried to write %+v: %v", h, err)
			return err
		}
		if h.PV != 0 {
			t.Errorf("a PV was written on the Box II: %+v", h)
		}
		if pll(float64(h.MHz)) < 500 || h.MHz > 690 {
			t.Errorf("clock %d off the measured grid / window", h.MHz)
		}
		s.mu.Lock()
		s.mv, s.mhz = p.MV, p.MHz
		// clocks over failAboveMHz flood errors on chip b0.c7, except the stock 690 MHz baseline (the fake
		// miner runs that one clean, so the baseline learns quiet chips)
		s.failAboveMHz = 99999
		if p.MHz > failAboveMHz && p.MHz != 690 {
			s.failAboveMHz = 0
		}
		s.applied = append(s.applied, p)
		*written = append(*written, h)
		s.mu.Unlock()
		return nil
	}
	return e, s, written
}

// c8Cfg: a fast Box II map run (codes in), as the panel's form would send it after box2TunerNormalize.
func c8Cfg() tunerConfig {
	c := defaultTunerConfig()
	c.DryRun = false
	c.BaselineMin, c.HoldMin, c.SettleMin, c.PollS = 1, 1, 0, 30
	c.FanHold = false
	c.LowestMHz, c.HighestMHz, c.Step = 600, 690, 25
	return c
}

func c8Logs(e *tunerEngine) string { return strings.Join(e.logbuf, "\n") }

// --- off: refused, nothing starts ---

func TestC8TunerRefusedWhenOffBox2(t *testing.T) {
	b10TempAuth(t)
	c8Box2(t, false, vcodeDirUnknown)
	tunerMu.Lock()
	before := tunerCur
	tunerMu.Unlock()
	for _, c := range []struct {
		name, method string
		body         any
	}{
		{"get", http.MethodGet, nil},
		{"start", http.MethodPost, map[string]any{"action": "start", "config": c8Cfg()}},
		{"stop", http.MethodPost, map[string]any{"action": "stop"}},
	} {
		if got := b10call(t, handleTuner, c.method, "/api/tuner", c.body); !isNotAvailable(got) {
			t.Errorf("%s: want the not-available refusal, got %v", c.name, got)
		}
	}
	for _, h := range []http.HandlerFunc{handleTunerExport, handleTunerChipMap} {
		if got := b10call(t, h, "GET", "/api/tuner/export?kind=log", nil); !isNotAvailable(got) {
			t.Errorf("tuner sub-handler not refused: %v", got)
		}
	}
	tunerMu.Lock()
	after := tunerCur
	tunerMu.Unlock()
	if after != before {
		t.Fatal("a tuning run was created while the tuner is off")
	}
	if getCharge() == "tuner" {
		t.Fatal("the tuner took charge while off")
	}
	// the shipped profile and the commissioning gate: the tuner needs clock + voltage control first
	// (the direction is a profile fact since C4 measured it: a higher level is a lower voltage)
	if profSCBox2.Feat.Tuner || profSCBox2.VCodeDir != vcodeUpIsLess || profSCBox2.VCodeMin != 42 || profSCBox2.VCodeMax != 61 {
		t.Fatal("the Box II profile must ship the tuner off, levels 42–61, and the measured direction (higher level = less voltage)")
	}
	if f := commissionFind("scbox2", "Tuner"); f == nil || strings.Join(f.Requires, ",") != "ClockControl,VoltageControl" {
		t.Fatalf("the tuner's commissioning requirements changed: %+v", f)
	}
}

// --- defaults and validation (codes, profile thermal limits) ---

func TestC8Box2DefaultsAndValidation(t *testing.T) {
	c8Box2(t, true, vcodeDirUnknown)
	d := defaultTunerConfig()
	if d.MinMV != 42 || d.MaxMV != 42 || d.BaselineMHz != 690 || d.BaselineMV != 42 || d.PVGap != 0 {
		t.Fatalf("Box II defaults not clock-only at the stock code: %+v", d)
	}
	if d.AbortC != 82 || d.FanTargetC != 77 || d.ChipTrim || d.BuildPresets {
		t.Fatalf("Box II defaults: abort %v target %v trims %v presets %v", d.AbortC, d.FanTargetC, d.ChipTrim, d.BuildPresets)
	}
	// the clock axis: the whole Box II window (Low-power stock clock to Hashrate), in the chips' own steps; the
	// fan hold (tuned on an SC Lite) is off by default
	if d.LowestMHz != 525 || d.HighestMHz != 690 || d.Step != 12.5 || d.MVStep != 1 || d.FanHold || d.TopDown {
		t.Fatalf("Box II clock axis / fan defaults: %d–%d step %g, mv step %d, fan hold %v", d.LowestMHz, d.HighestMHz, d.Step, d.MVStep, d.FanHold)
	}
	if cl := d.clockList(); len(cl) != 14 || cl[0] != 525 || cl[len(cl)-1] != 688 {
		t.Fatalf("Box II default clocks %v, want 525 … 688 (runs 687.5) in 12.5 MHz steps", cl)
	}
	if err := validateTunerConfig(d); err != nil {
		t.Fatalf("the Box II defaults don't validate: %v", err)
	}
	bad := map[string]func(c *tunerConfig){
		"abort 85 (over the profile's 84)":     func(c *tunerConfig) { c.AbortC = 85 },
		"abort 86 (the cut-off)":               func(c *tunerConfig) { c.AbortC = 86 },
		"fan target over 77 (fan hold on)":     func(c *tunerConfig) { c.FanHold, c.FanTargetC = true, 79 },
		"rebuild":                              func(c *tunerConfig) { c.Mode = "rebuild" },
		"presets":                              func(c *tunerConfig) { c.BuildPresets = true },
		"chip trims":                           func(c *tunerConfig) { c.ChipTrim = true },
		"clock step off the 12.5 grid":         func(c *tunerConfig) { c.Step = 10 },
		"clock under the window":               func(c *tunerConfig) { c.LowestMHz = 450 },
		"clock over the window":                func(c *tunerConfig) { c.HighestMHz = 700 },
		"another code, direction unknown":      func(c *tunerConfig) { c.MinMV, c.MaxMV = 42, 50 },
		"confirm at another code, dir unknown": func(c *tunerConfig) { c.Mode, c.ConfirmMV = "confirm", 61 },
		"own baseline":                         func(c *tunerConfig) { c.BaselineSrc, c.BaselineMHz = "own", 650 },
		"code outside 42–61":                   func(c *tunerConfig) { c.MinMV, c.MaxMV = 30, 42 },
	}
	for name, mut := range bad {
		c := d
		mut(&c)
		if err := validateTunerConfig(c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// the SC Lite form's values (PV − 300 in mV) become the stock code, with a note: never a written mV
	in := d
	in.MinMV, in.MaxMV, in.MVStep, in.ConfirmMV, in.PVGap, in.BaselineSrc, in.BaselineMV = 8800, 9300, 50, 9000, 300, "own", 9100
	got, note := box2TunerNormalize(in)
	if got.MinMV != 42 || got.MaxMV != 42 || got.ConfirmMV != 42 || got.PVGap != 0 || got.BaselineSrc != "stock" || got.BaselineMV != 42 {
		t.Fatalf("normalize: %+v", got)
	}
	if !strings.Contains(note, "clock only") {
		t.Fatalf("normalize note: %q", note)
	}
	if err := validateTunerConfig(got); err != nil {
		t.Fatalf("normalized form refused: %v", err)
	}
	// even a config that skipped validation can't make the engine step the code
	e := newTunerEngine(in)
	if e.cfg.MinMV != 42 || e.cfg.MaxMV != 42 || e.cfg.ConfirmMV != 42 || e.cfg.BaselineMV != 42 {
		t.Fatalf("engine config not clock-only: %+v", e.cfg)
	}
	if e.readVolt != nil {
		t.Fatal("the Box II engine must not search on a voltage reading (V is a trim code)")
	}
}

// --- the measured clock grid ---

func TestC8Box2ClockGridFromDriverLog(t *testing.T) {
	c8Box2(t, true, vcodeDirUnknown)
	if model.ClockStep != 12.5 {
		t.Fatalf("Box II clock step %v, want 12.5 (driver log)", model.ClockStep)
	}
	// "ICT580 set Clock(req R, actual A)" pairs from the owner's log (05195e51-qsdk-mining-log), 500 MHz up
	for req, actual := range map[float64]float64{500: 500, 510: 500, 520: 512.5, 530: 525, 560: 550, 590: 587.5,
		610: 600, 620: 612.5, 650: 650, 660: 650, 670: 662.5, 680: 675, 690: 687.5} {
		if got := pll(req); got != actual {
			t.Errorf("pll(%v) = %v, the driver ran %v", req, got, actual)
		}
	}
	if l := clockLabel(690); l != "690 MHz · runs 687.5" {
		t.Errorf("clockLabel(690) = %q", l)
	}
}

// --- clock-only run: only code 42 is written; ends on the best verified clock ---

func TestC8Box2ClockOnlyRun(t *testing.T) {
	c8Box2(t, true, vcodeDirUnknown)
	cfg, _ := box2TunerNormalize(c8Cfg())
	if err := validateTunerConfig(cfg); err != nil {
		t.Fatal(err)
	}
	e, _, w := c8Engine(t, cfg, 0, 650) // clean up to 650, errors above
	e.Run()
	clocks := map[int]bool{}
	for _, h := range *w {
		if h.MV != 42 {
			t.Fatalf("clock-only run wrote V-code %d: %v", h.MV, *w)
		}
		clocks[h.MHz] = true
	}
	for _, c := range []int{690, 600, 625, 650, 675} {
		if !clocks[c] {
			t.Errorf("clock %d never tested (writes %v)", c, *w)
		}
	}
	if e.best == nil || e.best.MHz != 650 || !e.confirmed {
		t.Fatalf("best %v confirmed %v, want 650 MHz confirmed (log:\n%s)", e.best, e.confirmed, c8Logs(e))
	}
	if last := (*w)[len(*w)-1]; last.MHz != 650 || last.MV != 42 {
		t.Fatalf("not left on the confirmed best: %+v", last)
	}
	logs := c8Logs(e)
	if !strings.Contains(logs, "7.67 V") || strings.Contains(logs, "level 42") || strings.Contains(logs, "PV ") || strings.Contains(logs, " mV") || strings.Contains(logs, "V-code") {
		t.Fatalf("the log must speak volts, not Goldshell's levels, PV or mV:\n%s", logs)
	}
	sv := e.statusView()
	if b, _ := sv["best"].(*planMV); b == nil || b.MV != 42 || b.PV != 0 {
		t.Fatalf("status best not in codes: %+v", sv["best"])
	}
}

// nothing faster than the lowest clock is clean, and the confirm fails: the run ends on the stock plan
func TestC8Box2EndsOnStockWhenNothingVerified(t *testing.T) {
	c8Box2(t, true, vcodeDirUnknown)
	cfg, _ := box2TunerNormalize(c8Cfg())
	e, _, w := c8Engine(t, cfg, 0, 500) // every clock under test fails; the stock baseline is clean
	e.Run()
	if e.confirmed {
		t.Fatalf("nothing should be confirmed (log:\n%s)", c8Logs(e))
	}
	if last := (*w)[len(*w)-1]; last.MHz != 690 || last.MV != 42 {
		t.Fatalf("not back on the stock Hashrate plan: %+v (all %v)", last, *w)
	}
}

// --- guards: heat aborts, ends on stock; hardware errors fail a clock ---

func TestC8Box2ThermalAbortEndsOnStock(t *testing.T) {
	c8Box2(t, true, vcodeDirUnknown)
	cfg, _ := box2TunerNormalize(c8Cfg())
	e, s, w := c8Engine(t, cfg, 0, 9999)
	e.readTemp = func() (float64, bool) { // 83 °C (over the profile's 82 abort, under the 86 cut-off) once testing
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.vc.Sub(s.t0).Minutes() > 1.5 {
			return 83, true
		}
		return 70, true
	}
	e.Run()
	if !e.aborted() || e.confirmed {
		t.Fatalf("want a heat abort, nothing confirmed (log:\n%s)", c8Logs(e))
	}
	if !strings.Contains(c8Logs(e), "abort at 82°C") {
		t.Fatalf("abort must be at the profile's 82 °C:\n%s", c8Logs(e))
	}
	if last := (*w)[len(*w)-1]; last.MHz != 690 || last.MV != 42 {
		t.Fatalf("after the heat abort not back on stock: %+v", last)
	}
}

func TestC8Box2HardwareErrorsFailClock(t *testing.T) {
	c8Box2(t, true, vcodeDirUnknown)
	cfg, _ := box2TunerNormalize(c8Cfg())
	e, _, w := c8Engine(t, cfg, 0, 610) // chip b0.c7 floods hardware errors from 625 MHz up
	e.Run()
	if e.best == nil || e.best.MHz != 600 {
		t.Fatalf("best %v, want 600 (log:\n%s)", e.best, c8Logs(e))
	}
	n := 0
	for _, r := range e.results {
		if r.MHz == 625 && r.Result == "failed" {
			n++
		}
		if r.MV != 42 || r.PV != 0 {
			t.Fatalf("result row not in codes: %+v", r)
		}
	}
	if n == 0 {
		t.Fatalf("625 MHz should have failed on hardware errors: %+v", e.results)
	}
	for _, h := range *w {
		if h.MHz > 625 && h.MHz != 690 {
			t.Fatalf("a clock past the first failure was tested: %v", *w)
		}
	}
}

// --- the chip map must fit the model ---

func TestC8Box2GeometryMismatchStops(t *testing.T) {
	c8Box2(t, true, vcodeDirUnknown)
	cfg, _ := box2TunerNormalize(c8Cfg())
	e, s, w := c8Engine(t, cfg, 0, 9999)
	e.readErrors = func() (map[ckey]int, error) { // an SC Lite-sized board
		s.mu.Lock()
		defer s.mu.Unlock()
		out := map[ckey]int{}
		for i := 1; i <= 46; i++ {
			out[ckey{B: 0, I: i}] = 0
		}
		return out, nil
	}
	e.Run()
	if !strings.Contains(c8Logs(e), "36 chips per board") {
		t.Fatalf("want a geometry stop (log:\n%s)", c8Logs(e))
	}
	for _, h := range *w {
		if h.MHz != 690 || h.MV != 42 {
			t.Fatalf("a test setting was written after a geometry mismatch: %v", *w)
		}
	}
	if err := tunerGeometryErr(map[ckey]int{{0, 1}: 0, {0, 36}: 0}); err != nil {
		t.Fatalf("a 36-chip board refused: %v", err)
	}
	if err := tunerGeometryErr(map[ckey]int{{0, 1}: 0, {1, 1}: 0}); err == nil {
		t.Fatal("two boards accepted on a one-board model")
	}
}

// --- voltage steps once a direction is set (as the C4 result would) ---

func TestC8Box2VoltageStepsWhenDirectionKnown(t *testing.T) {
	for _, dir := range []vcodeDir{vcodeUpIsLess, vcodeUpIsMore} {
		t.Run(map[vcodeDir]string{vcodeUpIsLess: "up-is-less", vcodeUpIsMore: "up-is-more"}[dir], func(t *testing.T) {
			c8Box2(t, true, dir)
			cfg := c8Cfg()
			cfg.LowestMHz, cfg.HighestMHz, cfg.Step = 690, 690, 12.5 // one clock: "688" (runs 687.5, as 690 does)
			cfg.MinMV, cfg.MaxMV, cfg.MVStep = 42, 61, 1
			cfg, note := box2TunerNormalize(cfg)
			if note != "" || cfg.MinMV != 42 || cfg.MaxMV != 61 {
				t.Fatalf("a known direction must keep the codes: %+v %q", cfg, note)
			}
			if err := validateTunerConfig(cfg); err != nil {
				t.Fatal(err)
			}
			// the chips need the voltage of code 51 (up-is-less: 51 or lower; up-is-more: 51 or higher)
			need := box2LevelOf(51)
			e, s, w := c8Engine(t, cfg, need, 9999)
			inner := e.applyPlan
			e.applyPlan = func(p planMV) error { // the stock 690 MHz / 42 baseline runs clean (it's the stock plan)
				s.mu.Lock()
				s.failBelowMV = need
				if p.MHz == 690 {
					s.failBelowMV = 0
				}
				s.mu.Unlock()
				return inner(p)
			}
			e.Run()
			first := (*w)[1] // [0] is the baseline
			least := 61
			if dir == vcodeUpIsMore {
				least = 42
			}
			if first.MV != least {
				t.Fatalf("the walk must start at the least-voltage code %d, started at %d (%v)", least, first.MV, *w)
			}
			if e.best == nil || e.best.hw().MV != 51 || !e.confirmed {
				t.Fatalf("lowest clean code %v, want 51 (writes %v, log:\n%s)", e.best.hwPtr(), *w, c8Logs(e))
			}
			if last := (*w)[len(*w)-1]; last.MV != 51 || last.MHz != 688 {
				t.Fatalf("not left on the confirmed best: %+v", last)
			}
			if v := e.statusView()["cfg"].(tunerConfig); v.MinMV != 42 || v.MaxMV != 61 {
				t.Fatalf("status cfg not in codes: %d–%d", v.MinMV, v.MaxMV)
			}
		})
	}
}

// the write guard: never a code outside the interval; only the stock code while the direction is unknown
func TestC8Box2WriteGuard(t *testing.T) {
	c8Box2(t, true, vcodeDirUnknown)
	for _, h := range []planMV{{MHz: 650, MV: 50}, {MHz: 650, MV: 41}, {MHz: 650, MV: 62}, {MHz: 700, MV: 42}, {MHz: 450, MV: 42}} {
		if box2TunerWriteErr(h) == nil {
			t.Errorf("write %+v allowed with the direction unknown", h)
		}
	}
	if err := box2TunerWriteErr(planMV{MHz: 650, MV: 42}); err != nil {
		t.Errorf("650 MHz at the stock code refused: %v", err)
	}
	c8Box2(t, true, vcodeUpIsLess)
	if box2TunerWriteErr(planMV{MHz: 650, MV: 50}) != nil || box2TunerWriteErr(planMV{MHz: 650, MV: 62}) == nil {
		t.Error("a known direction: codes 42–61 only")
	}
	if box2LevelOf(42) != 61 || box2CodeOf(box2LevelOf(47)) != 47 {
		t.Error("the level mirror is not an involution on 42–61")
	}
}

// the texts: Goldshell's level with the C4 volts where measured, nothing made up elsewhere; no PV, mV or watts
func TestC8Box2LevelTexts(t *testing.T) {
	b10TempAuth(t)
	c8Box2(t, true, vcodeUpIsLess)
	d := defaultTunerConfig()
	if d.MinMV != 42 || d.MaxMV != 61 {
		t.Fatalf("a known direction: the voltage axis is every level 42–61, got %d–%d", d.MinMV, d.MaxMV)
	}
	for code, want := range map[int]string{42: "7.67 V", 44: "7.58 V", 46: "7.52 V",
		61: "7.03 V", 50: "≈ 7.39 V"} {
		if got := d.vText(box2LevelOf(code)); got != want {
			t.Errorf("vText(level %d) = %q, want %q", code, got, want)
		}
	}
	if s := (planMV{MHz: 690, MV: box2LevelOf(42), PV: box2LevelOf(42)}).text(); s != "690 MHz / 7.67 V" {
		t.Errorf("plan text %q", s)
	}
	// a point without a PV: its rank, never the SC Lite's mV + 300
	if p := (planMV{MHz: 600, MV: box2LevelOf(50)}); p.pvOf() != p.MV {
		t.Errorf("Box II pvOf %d, want the rank %d", p.pvOf(), p.MV)
	}
	info := box2TunerInfo()
	note, _ := info["note"].(string)
	for _, bad := range []string{"trim code", "V-code", "PV 9", " mV", "J/TH", " W "} {
		if strings.Contains(note, bad) {
			t.Errorf("tuner info note says %q: %s", bad, note)
		}
	}
	if !strings.Contains(note, "690 MHz at 7.67 V") || !strings.Contains(note, "525 MHz at 7.03 V") || !strings.Contains(note, "between 7.03 V and 7.67 V") || strings.Contains(note, "level") {
		t.Errorf("tuner info note: %s", note)
	}
	if lv, _ := info["level_volts"].(map[string]float64); lv["42"] != 7.67 || lv["61"] != 7.03 || len(lv) != 6 {
		t.Errorf("level_volts %v", info["level_volts"])
	}
	// the saved curve (ranks) is checked against the Box II window and levels, not the SC Lite's PV range
	saveSavedCurve(savedCurve{Curve: []planMV{{MHz: 600, MV: box2LevelOf(50), PV: box2LevelOf(50)}, {MHz: 700, MV: box2LevelOf(50)}}})
	if c := loadSavedCurve().Curve; len(c) != 1 || c[0].MHz != 600 {
		t.Errorf("Box II saved curve %v, want the 600 MHz point only", c)
	}
}

// the restart record (levels) is put back as the trim code
func TestC8Box2RecoverPutsBackStockCode(t *testing.T) {
	b10TempAuth(t)
	c8Box2(t, true, vcodeUpIsLess)
	base := box2TunerBaseline() // 690 MHz, level of code 42
	b, _ := json.Marshal(tunerMarker{Started: 1, Base: base})
	if err := os.WriteFile(tunerMarkerPath(), b, 0o644); err != nil {
		t.Fatal(err)
	}
	var got []planMV
	old := tunerRecoverApply
	tunerRecoverApply = func(p planMV) error { got = append(got, p); return nil }
	t.Cleanup(func() { tunerRecoverApply = old })
	tunerRecover(0)
	if len(got) != 1 || got[0].MHz != 690 || got[0].MV != 42 || got[0].PV != 0 {
		t.Fatalf("recover put back %v, want 690 MHz / code 42", got)
	}
}

// --- SC Lite unchanged ---

func TestC8SCLiteTunerUnchanged(t *testing.T) {
	b10SetModel(t, profSCLite)
	d := defaultTunerConfig()
	if d.BaselineMV != 9100 || d.MinMV != 8800 || d.MaxMV != 9300 || d.PVGap != 300 || d.AbortC != 88 || d.FanTargetC != 70 || d.BaselineMHz != 625 {
		t.Fatalf("SC Lite defaults changed: %+v", d)
	}
	p := planMV{MHz: 625, MV: 9100, PV: 9400}
	if p.hw() != p || p.text() != "625 MHz / PV 9400 (mV 9100)" || d.vText(9100) != "PV 9400" || d.pvFor(9100) != 9400 {
		t.Fatalf("SC Lite plan text/conversion changed: %q %q", p.text(), d.vText(9100))
	}
	if d.hwView() != d {
		t.Fatal("SC Lite config view changed")
	}
	if d.LowestMHz != 575 || d.HighestMHz != 675 || d.Step != 25 || d.MVStep != 50 || !d.FanHold || d.FanMinPct != 30 {
		t.Fatalf("SC Lite map / fan defaults changed: %+v", d)
	}
	if (planMV{MHz: 625, MV: 9100}).pvOf() != 9400 {
		t.Fatal("SC Lite pvOf (mV + the stock 300 gap) changed")
	}
	if err := validateTunerConfig(d); err != nil {
		t.Fatalf("SC Lite defaults refused: %v", err)
	}
	if tunerGeometryErr(map[ckey]int{{2, 46}: 0, {3, 1}: 0}) != nil {
		t.Fatal("SC Lite geometry must not be checked")
	}
}
