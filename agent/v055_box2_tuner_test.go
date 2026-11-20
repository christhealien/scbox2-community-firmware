package main

// v0.5.5: the SC Lite tuner's "Trim weak chips" and "Build presets" on the SC Box II (1 board, 36 chips in 18 pairs,
// the OEM's levels 42–61). On the tuner harness (newSimEngine / c8Engine: virtual clock, a fake miner) with a chip
// model drawn from the owner's C8 run of 2026-11-15 at 675 MHz: every level from 7.03 to 7.52 V fails with errors all
// over the board; at 7.55 V only b0.c26 and b0.c23 still make errors (c26 needs 25 MHz less, c23 12.5 MHz less).
//   - the trim run: no trim below 7.55 V (errors everywhere: trims can't fix that), the two weak chips trimmed at
//     7.55 V, the trims checked, confirmed and kept (handed to auto-trim), the run left on 675 MHz / 7.55 V;
//   - trims come off on Stop (during a trim retest and after a kept trim), on the heat abort, and after a power cut
//     (the restart marker); the run's trim writes stay in RAM and only its end reaches the flash;
//   - the hashrate check and the power estimate count the trims; the chip maps record them;
//   - presets: built (High / Middle / Low / Lowest power, the OEM's levels, own fan curves, inside the tested
//     range), applied (the plan writer, the curve switched, the owner's plan saved), edited (the Box II ranges),
//     used by a schedule (not saved as the owner's plan) and still there after a restart;
//   - the trims' re-apply asks the miner nothing while a clock is being confirmed (the v0.5.4c API budget);
//   - the SC Lite: unchanged.

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// v055Box2: the Box II with the tuner commissioned and everything it builds on (clock, voltage, chip trims, the
// power estimate), the measured direction (a higher level = less voltage).
func v055Box2(t *testing.T) {
	t.Helper()
	p := profSCBox2
	p.Feat.Tuner, p.Feat.ClockControl, p.Feat.VoltageControl, p.Feat.ChipTrims, p.Feat.PowerEstimate = true, true, true, true, true
	p.VCodeDir = vcodeUpIsLess
	b10SetModel(t, p)
}

// v055Need: the volts chip i needs at chip clock f (MHz) in the C8-like model.
func v055Need(chip int, f float64) float64 {
	base := 7.55
	switch chip {
	case 26:
		base = 7.61
	case 23:
		base = 7.58
	}
	return base - 0.004*(675-f)
}

// v055Sim: a Box II tuner run on the C8-like chips: c8Engine's fake miner, every chip's errors from the model (each
// 0.01 V short is 3 errors a minute), a fake chip hook (chain 0, 36 chips) and trims file. trimLevels records the
// level (the OEM's) each trim write was made at.
type v055Sim struct {
	e          *tunerEngine
	s          *simState
	w          *[]planMV
	mu         sync.Mutex
	trims      map[[2]int]float64
	trimLevels []int
	adopted    []chipTrim
	persisted  []planMV
	onSleep    func() // (once a poll, e.g. a Stop or the heat)
}

func v055Cfg() tunerConfig {
	c := defaultTunerConfig()
	c.DryRun, c.Mode = false, "map"
	c.LowestMHz, c.HighestMHz, c.Step = 675, 675, 12.5
	c.MinMV, c.MaxMV, c.MVStep = 42, 61, 1 // (the OEM's levels: 7.67 down to 7.03 V)
	c.BaselineMin, c.HoldMin, c.SettleMin, c.PollS = 10, 10, 3, 30
	c.ChipTrim = true
	c.ConfirmWin, c.ConfirmX = true, 2
	c.FanHold = false
	return c
}

func newV055Sim(t *testing.T, cfg tunerConfig) *v055Sim {
	t.Helper()
	cfg, _ = box2TunerNormalize(cfg)
	if err := validateTunerConfig(cfg); err != nil {
		t.Fatalf("config refused: %v", err)
	}
	e, s, w := c8Engine(t, cfg, 0, 99999)
	v := &v055Sim{e: e, s: s, w: w, trims: map[[2]int]float64{}}
	e.trimGet = func() map[[2]int]float64 {
		v.mu.Lock()
		defer v.mu.Unlock()
		m := map[[2]int]float64{}
		for k, x := range v.trims {
			m[k] = x
		}
		return m
	}
	e.trimSet = func(chain, chip int, mhz float64) error {
		if mhz > 0 {
			t.Errorf("the tuner raised b%d.c%d to %+g MHz: it only ever slows chips", chain, chip, mhz)
		}
		s.mu.Lock()
		lvl := box2CodeOf(s.mv)
		s.mu.Unlock()
		v.mu.Lock()
		if mhz == 0 {
			delete(v.trims, [2]int{chain, chip})
		} else {
			v.trims[[2]int{chain, chip}] = mhz
			v.trimLevels = append(v.trimLevels, lvl)
		}
		v.mu.Unlock()
		return nil
	}
	e.hookStats = func() (chipStatsFile, bool) {
		var f chipStatsFile
		f.Chains = append(f.Chains, struct {
			Chain   int   `json:"chain"`
			Chips   int   `json:"chips"`
			Base10  int   `json:"base10"`
			Clock10 []int `json:"clock10"`
			Nonces  []int `json:"nonces"`
		}{Chain: 0, Chips: 36, Base10: 6750})
		return f, true
	}
	e.adoptTrims = func(ts []chipTrim, why string) { v.adopted = ts }
	oldPersist := box2PersistPlan
	box2PersistPlan = func(mhz, code int) { v.persisted = append(v.persisted, planMV{MHz: mhz, MV: code}) }
	t.Cleanup(func() { box2PersistPlan = oldPersist })
	e.persistPlan = func(p planMV) { h := p.hw(); box2PersistPlan(h.MHz, h.MV) }
	e.sleep = func(d time.Duration) {
		s.mu.Lock()
		s.vc = s.vc.Add(d)
		volts, _ := box2Volts(box2CodeOf(s.mv))
		base := pll(float64(s.mhz))
		s.mu.Unlock()
		v.mu.Lock()
		tr := map[int]float64{}
		for k, x := range v.trims {
			tr[k[1]] = x
		}
		v.mu.Unlock()
		s.mu.Lock()
		for i := 1; i <= 36; i++ {
			if short := v055Need(i, base+tr[i]) - volts; short > 1e-9 {
				s.counts[ckey{0, i}] += short / 0.01 * 3 / 60 * d.Seconds()
			}
		}
		s.mu.Unlock()
		if v.onSleep != nil {
			v.onSleep()
		}
	}
	return v
}

func (v *v055Sim) trimsNow() map[[2]int]float64 { return v.e.trimGet() }

func (v *v055Sim) runTrimCount() int {
	v.e.mu.Lock()
	defer v.e.mu.Unlock()
	return len(v.e.runTrims)
}

// --- defaults and validation ---

func TestV055Box2TrimPresetDefaultsAndValidation(t *testing.T) {
	v055Box2(t)
	d := defaultTunerConfig()
	if d.ChipTrim || d.BuildPresets || d.ChipTrimMax != 4 || d.ChipTrimDeep != 50 || d.WarmestC != 74 || d.FanTargetC != 72 {
		t.Fatalf("Box II defaults: trim %v max %d deep %g, presets %v, warmest %g, target %g", d.ChipTrim, d.ChipTrimMax, d.ChipTrimDeep, d.BuildPresets, d.WarmestC, d.FanTargetC)
	}
	ok := func(name string, mut func(c *tunerConfig)) {
		t.Helper()
		c := d
		c.DryRun = false
		mut(&c)
		c, _ = box2TunerNormalize(c)
		if err := validateTunerConfig(c); err != nil {
			t.Errorf("%s: refused: %v", name, err)
		}
	}
	bad := func(name, want string, mut func(c *tunerConfig)) {
		t.Helper()
		c := d
		c.DryRun = false
		mut(&c)
		c, _ = box2TunerNormalize(c)
		err := validateTunerConfig(c)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: want an error with %q, got %v", name, want, err)
		}
	}
	ok("trim weak chips", func(c *tunerConfig) { c.ChipTrim = true })
	ok("trim 75 MHz deep, 4 chips", func(c *tunerConfig) { c.ChipTrim, c.ChipTrimDeep, c.ChipTrimMax = true, 75, 4 })
	ok("build presets, fans held", func(c *tunerConfig) { c.BuildPresets, c.FanHold = true, true })
	ok("rebuild, fans held", func(c *tunerConfig) { c.Mode, c.FanHold = "rebuild", true })
	bad("5 chips (v0.5.5 T7: at most 4 on this model)", "1–4", func(c *tunerConfig) { c.ChipTrim, c.ChipTrimMax = true, 5 })
	bad("10 chips", "1–4", func(c *tunerConfig) { c.ChipTrim, c.ChipTrimMax = true, 10 })
	bad("0 chips", "1–4", func(c *tunerConfig) { c.ChipTrim, c.ChipTrimMax = true, 0 })
	bad("80 MHz deep", "25–75", func(c *tunerConfig) { c.ChipTrim, c.ChipTrimDeep = true, 80 })
	bad("30 MHz deep (off the grid)", "25–75", func(c *tunerConfig) { c.ChipTrim, c.ChipTrimDeep = true, 30 })
	bad("presets, fans left alone", "Hold a steady temperature", func(c *tunerConfig) { c.BuildPresets = true })
	bad("warmest over the hold's cap", "between the fan target", func(c *tunerConfig) { c.BuildPresets, c.FanHold, c.WarmestC = true, true, 75 })
	bad("warmest under the target", "between the fan target", func(c *tunerConfig) { c.BuildPresets, c.FanHold, c.WarmestC = true, true, 70 })
	bad("warmest too close to the abort", "below the abort", func(c *tunerConfig) {
		c.BuildPresets, c.FanHold, c.AbortC, c.FanTargetC, c.WarmestC = true, true, 80, 70, 73
	})
	// chip trims not commissioned: refused (a dry run writes nothing, so it may run)
	p := model
	p.Feat.ChipTrims = false
	b10SetModel(t, p)
	bad("trims not commissioned", "not available", func(c *tunerConfig) { c.ChipTrim = true })
	c := d
	c.ChipTrim, c.DryRun = true, true
	if err := validateTunerConfig(c); err != nil {
		t.Errorf("a dry run with trims, not commissioned: %v", err)
	}
	// the SC Lite keeps its 12 °C, the Box II 8 °C under the abort
	if presetRoomC() != 8 {
		t.Fatalf("Box II preset room %g", presetRoomC())
	}
	b10SetModel(t, profSCLite)
	if presetRoomC() != 12 || defaultTunerConfig().ChipTrimMax != 3 {
		t.Fatal("SC Lite preset room / trim defaults changed")
	}
}

// --- the trim run on C8-like data ---

func TestV055Box2TrimRunC8Data(t *testing.T) {
	v055Box2(t)
	v := newV055Sim(t, v055Cfg())
	v.e.Run()
	e := v.e
	logs := strings.Join(e.fullLog, "\n")
	if os.Getenv("V055LOG") != "" {
		t.Log(logs)
	}
	if !e.confirmed || e.best == nil || e.best.hw().MHz != 675 || e.best.hw().MV != 45 {
		t.Fatalf("want 675 MHz / 7.55 V confirmed, got %v confirmed %v\n%s", e.best, e.confirmed, logs)
	}
	tr := v.trimsNow()
	if len(tr) != 2 || tr[[2]int{0, 26}] != -25 || tr[[2]int{0, 23}] != -12.5 {
		t.Fatalf("trims %v, want b0.c26 −25 and b0.c23 −12.5 MHz\n%s", tr, logs)
	}
	for _, l := range v.trimLevels {
		if l != 45 {
			t.Fatalf("a trim was tried at %s: below 7.55 V the errors are all over the board (levels %v)\n%s", box2LevelText(l), v.trimLevels, logs)
		}
	}
	if len(v.adopted) != 2 {
		t.Fatalf("the kept trims weren't handed to auto-trim: %v", v.adopted)
	}
	// the levels below were refused for trims (spread), not tried
	if !strings.Contains(logs, "no chip trim: the ") || !strings.Contains(logs, "more than trims could fix") {
		t.Fatalf("no spread refusal logged:\n%s", logs)
	}
	for _, want := range []string{"slowing b0.c26 to -12.5 MHz", "slowing b0.c26 to -25 MHz", "slowing b0.c23 to -12.5 MHz",
		"is clean with chip trims: they stay", "TRIM CHECK", "the trim is needed", "CONFIRMED", "CHIP TRIMS kept by this run"} {
		if !strings.Contains(logs, want) {
			t.Errorf("log lacks %q", want)
		}
	}
	// never a level below 7.55 V clean: the walk went up one level at a time from 7.03 V
	tested := map[int]bool{}
	for _, h := range *v.w {
		tested[h.MV] = true
		if err := box2TunerWriteErr(h); err != nil || h.PV != 0 {
			t.Fatalf("write %+v: %v", h, err)
		}
	}
	for l := 45; l <= 61; l++ {
		if !tested[l] {
			t.Errorf("level %d (%s) never tested", l, box2LevelText(l))
		}
	}
	if last := (*v.w)[len(*v.w)-1]; last.MHz != 675 || last.MV != 45 {
		t.Fatalf("not left on the confirmed setting: %+v", last)
	}
	if len(v.persisted) != 1 || v.persisted[0] != (planMV{MHz: 675, MV: 45}) {
		t.Fatalf("the confirmed result is the plan saved for the next boot: %v", v.persisted)
	}
	// the results: the trims and the power estimate with them; the chip maps carry the trimmed chips
	var trimmedRow *testResult
	for i := range e.results {
		if r := e.results[i]; r.Result == "clean" && r.MV == 45 && r.Trims == 2 {
			trimmedRow = &e.results[i]
		}
	}
	if trimmedRow == nil {
		t.Fatalf("no clean 7.55 V row with 2 trims: %+v", e.results)
	}
	w0, _, _, _ := box2EstimateWatts(675, 45)
	want := w0 * (1 - 37.5/(36*675))
	if trimmedRow.TrimMHz != 37.5 || math.Abs(trimmedRow.EstWatts-want) > 0.11 || trimmedRow.EstWatts >= w0 {
		t.Fatalf("row trims %g MHz, est %g W, want 37.5 MHz and %.1f W (untrimmed %.1f W)", trimmedRow.TrimMHz, trimmedRow.EstWatts, want, w0)
	}
	found := false
	for _, m := range e.testMaps {
		if m.Result == "clean" && m.MV == 45 {
			got := map[int]float64{}
			for _, c := range m.Trims {
				got[int(c[1])] = c[2]
			}
			found = got[26] == -25 && got[23] == -12.5 && len(got) == 2
		}
	}
	if !found {
		t.Fatalf("the chip map of the clean 7.55 V test doesn't show the two trimmed chips")
	}
	// the texts: volts and MHz, never PV, mV or the OEM's level numbers
	if strings.Contains(logs, "PV ") || strings.Contains(logs, " mV") || strings.Contains(logs, "level 4") {
		t.Fatalf("the log must speak volts:\n%s", logs)
	}
	sv := e.statusView()
	if ts, _ := sv["trims"].([]chipTrim); len(ts) != 2 {
		t.Fatalf("status trims %v", sv["trims"])
	}
}

// The board-total fail blames the chip that made a good share of it, and only when trims could fix the test.
func TestV055Box2TrimBlame(t *testing.T) {
	v055Box2(t)
	g := func(m map[int]int) map[ckey]int {
		out := map[ckey]int{}
		for i := 1; i <= 36; i++ {
			out[ckey{0, i}] = m[i]
		}
		return out
	}
	// the C8 confirm fail: board 0 +5 (limit 4.0), c26 +2, c23 +1, c28 +1, c1 +1
	k, ok := box2TrimBlame(tunerStats{Gained: g(map[int]int{26: 2, 23: 1, 28: 1, 1: 1})}, "board 0 +5 errors (limit 4.0)", nil)
	if !ok || k != (ckey{0, 26}) {
		t.Fatalf("blame %v %v, want b0.c26", k, ok)
	}
	// spread: one error each on many chips
	if _, ok := box2TrimBlame(tunerStats{Gained: g(map[int]int{1: 1, 5: 1, 9: 1, 26: 1})}, "board 0 +4 errors (limit 3.0)", nil); ok {
		t.Fatal("a fail spread one error per chip blamed a chip")
	}
	// one chip with 2 of 9 (under a third): not that chip's
	if _, ok := box2TrimBlame(tunerStats{Gained: g(map[int]int{26: 2, 1: 1, 2: 1, 3: 1, 4: 1, 5: 1, 6: 1, 7: 1})}, "board 0 +9 errors (limit 3.0)", nil); ok {
		t.Fatal("2 of 9 errors blamed a chip")
	}
	// a chip over its own limit is the SC Lite's case: not this function's
	if _, ok := box2TrimBlame(tunerStats{Gained: g(map[int]int{26: 9})}, "chip b0.c26 +9 errors (limit 2.0)", nil); ok {
		t.Fatal("a chip fail went through the board rule")
	}
	// the SC Lite: the engine's blame is the chip over its own limit, nothing else
	b10SetModel(t, profSCLite)
	e := newTunerEngine(fastCfg())
	if _, ok := e.trimBlame(planMV{MHz: 625, MV: 9100}, tunerStats{Gained: g(map[int]int{26: 3})}, "board 0 +5 errors (limit 4.0)"); ok {
		t.Fatal("SC Lite: a board fail blamed a chip")
	}
	if k, ok := e.trimBlame(planMV{}, tunerStats{FailChip: ckey{2, 5}, FailByChip: true}, "chip"); !ok || k != (ckey{2, 5}) {
		t.Fatal("SC Lite: the chip fail's own chip")
	}
}

// --- trims come off: Stop, the heat abort, a power cut ---

func v055AssertBackOnStock(t *testing.T, v *v055Sim, what string) {
	t.Helper()
	if v.e.confirmed {
		t.Fatalf("%s: nothing may be confirmed", what)
	}
	if tr := v.trimsNow(); len(tr) != 0 {
		t.Fatalf("%s: the run's trims are still on: %v\n%s", what, tr, strings.Join(v.e.fullLog, "\n"))
	}
	if len(v.adopted) != 0 {
		t.Fatalf("%s: trims handed to auto-trim: %v", what, v.adopted)
	}
	if last := (*v.w)[len(*v.w)-1]; last.MHz != 690 || last.MV != 42 {
		t.Fatalf("%s: not back on Stock High: %+v", what, last)
	}
	if len(v.persisted) != 0 {
		t.Fatalf("%s: a plan was saved as the owner's: %v", what, v.persisted)
	}
}

func TestV055Box2TrimsOffOnStopDuringRetest(t *testing.T) {
	v055Box2(t)
	v := newV055Sim(t, v055Cfg())
	set := v.e.trimSet
	v.e.trimSet = func(chain, chip int, mhz float64) error {
		err := set(chain, chip, mhz)
		if mhz < 0 {
			v.e.Stop() // the owner's Stop, right after the first trim (its retest is cut)
		}
		return err
	}
	v.e.Run()
	v055AssertBackOnStock(t, v, "Stop during a trim retest")
	if !strings.Contains(strings.Join(v.e.fullLog, "\n"), "cut short: the trims made for it are undone") {
		t.Fatalf("log:\n%s", strings.Join(v.e.fullLog, "\n"))
	}
}

func TestV055Box2TrimsOffOnStopAfterKept(t *testing.T) {
	v055Box2(t)
	v := newV055Sim(t, v055Cfg())
	v.onSleep = func() {
		if v.runTrimCount() > 0 && !v.e.stopped() {
			v.e.Stop()
		}
	}
	v.e.Run()
	v055AssertBackOnStock(t, v, "Stop after a kept trim")
	if v.e.trimsUndone != 2 {
		t.Fatalf("trims undone at the end: %d, want 2", v.e.trimsUndone)
	}
}

func TestV055Box2TrimsOffOnHeatAbort(t *testing.T) {
	v055Box2(t)
	v := newV055Sim(t, v055Cfg())
	hot := false
	v.e.readTemp = func() (float64, bool) {
		if v.runTrimCount() > 0 {
			hot = true
		}
		if hot {
			return 83, true // over the 82 °C abort, under the OEM's 86 °C cut-off
		}
		return 66, true
	}
	v.e.Run()
	if !v.e.aborted() || !strings.Contains(v.e.abortReason(), "abort at 82") {
		t.Fatalf("want the heat abort: %q\n%s", v.e.abortReason(), strings.Join(v.e.fullLog, "\n"))
	}
	v055AssertBackOnStock(t, v, "heat abort")
}

// A power cut mid-run: the next start's recovery puts the trims back as the restart marker recorded them before the
// run, and the plan back on Stock High.
func TestV055Box2TrimsOffAfterPowerCut(t *testing.T) {
	b10TempAuth(t)
	v055Box2(t)
	chipTrimMu.Lock()
	writeChipTrims([]chipTrim{{Chain: 0, Chip: 5, MHz: -12.5}}) // the owner's own trim, from before the run
	chipTrimMu.Unlock()
	base := box2TunerBaseline()
	under := planMV{MHz: 675, MV: box2LevelOf(45)}
	m := tunerMarker{Started: 1, Base: base, Trims: readChipTrims(), KeptSet: true, Testing: &under, TestAt: 2}
	b, _ := json.Marshal(m)
	if err := os.WriteFile(tunerMarkerPath(), b, 0o644); err != nil {
		t.Fatal(err)
	}
	// the run then trimmed two chips (one kept, one under test) when the power went
	chipTrimMu.Lock()
	writeChipTrims([]chipTrim{{Chain: 0, Chip: 5, MHz: -12.5}, {Chain: 0, Chip: 26, MHz: -25}, {Chain: 0, Chip: 23, MHz: -12.5}})
	chipTrimMu.Unlock()
	markTunerTrims(map[[2]int]float64{{0, 26}: -25})
	var put []planMV
	old := tunerRecoverApply
	tunerRecoverApply = func(p planMV) error { put = append(put, p); return nil }
	t.Cleanup(func() { tunerRecoverApply = old })
	tunerRecover(0)
	got := readChipTrims()
	if len(got) != 1 || got[0].Chip != 5 || got[0].MHz != -12.5 {
		t.Fatalf("after the recovery the trims are %v, want only the owner's b0.c5 −12.5 MHz", got)
	}
	if len(put) != 1 || put[0].MHz != 690 || put[0].MV != 42 {
		t.Fatalf("recovery put back %v, want 690 MHz / 7.67 V", put)
	}
	if _, err := os.Stat(tunerMarkerPath()); err == nil {
		t.Fatal("the restart marker is still there")
	}
}

// The run's trim writes stay in RAM (a power cut boots on the trims saved before it); its end is saved once: the kept
// trims after a confirmed run, the trims as before after a Stop.
func TestV055Box2TrimWritesStayInRAM(t *testing.T) {
	for _, stop := range []bool{false, true} {
		f := withShadow(t)
		v055Box2(t)
		v := newV055Sim(t, v055Cfg())
		var flashDuring []string
		inRAM := true
		v.e.trimGet = func() map[[2]int]float64 {
			m := map[[2]int]float64{}
			for _, x := range readChipTrims() {
				m[[2]int{x.Chain, x.Chip}] = x.MHz
			}
			return m
		}
		v.e.trimSet = func(chain, chip int, mhz float64) error {
			if !box2TrimsInRAM() || !cfgHot(chipTrimPath()) {
				inRAM = false
			}
			if err := cwSetTrimUnless(&v.e.dead, chain, chip, mhz); err != nil {
				return err
			}
			if s, ok := f.onFlash("scbox2/chiptrim"); ok {
				flashDuring = append(flashDuring, s)
			}
			if stop && mhz < 0 && len(readChipTrims()) >= 2 {
				v.e.Stop()
			}
			return nil
		}
		// (the model reads the real trims file)
		inner := v.e.sleep
		v.e.sleep = func(d time.Duration) {
			v.mu.Lock()
			v.trims = map[[2]int]float64{}
			for _, x := range readChipTrims() {
				v.trims[[2]int{x.Chain, x.Chip}] = x.MHz
			}
			v.mu.Unlock()
			inner(d)
		}
		v.e.Run()
		if !inRAM {
			t.Fatal("a trim was written while the trims file wasn't a RAM-only file")
		}
		if len(flashDuring) > 0 {
			t.Fatalf("stop %v: the run's trims reached the flash during the run: %q", stop, flashDuring)
		}
		if box2TrimsInRAM() || cfgHot(chipTrimPath()) {
			t.Fatal("the trims file is still RAM-only after the run")
		}
		s, ok := f.onFlash("scbox2/chiptrim")
		switch {
		case !stop && (!ok || !strings.Contains(s, "0 26 -25") || !strings.Contains(s, "0 23 -12.5")):
			t.Fatalf("confirmed: the kept trims should be saved at the end: %q %v\n%s", s, ok, strings.Join(v.e.fullLog, "\n"))
		case stop && ok && (strings.Contains(s, "0 26") || strings.Contains(s, "0 23")):
			t.Fatalf("stopped: run trims on the flash: %q", s)
		}
	}
}

// --- the hashrate check counts the trims ---

func TestV055Box2TrimmedHashrateWant(t *testing.T) {
	if got := box2TrimmedWant(1000, 675, 37.5, 36); math.Abs(got-1000*(1-37.5/(36*675))) > 1e-9 {
		t.Fatalf("trimmed want %g", got)
	}
	if box2TrimmedWant(1000, 675, 0, 36) != 1000 {
		t.Fatal("no trims: unchanged")
	}
	v055Box2(t)
	w, ok := box2TrimmedWatts(675, 45, 37.5, 36)
	w0, _, _, _ := box2EstimateWatts(675, 45)
	if !ok || w >= w0 || math.Abs(w-w0*(1-37.5/(36*675))) > 0.11 {
		t.Fatalf("trimmed watts %g (untrimmed %g)", w, w0)
	}
	// the hashrate per watt of a setting doesn't change with its trims (both follow the chips' clocks)
	h0 := 1970.0 * 675 / 687.5
	h := box2TrimmedWant(h0, 675, 37.5, 36)
	if math.Abs(h0/w0-h/w) > 0.01 {
		t.Fatalf("GH/s per W: %g untrimmed, %g trimmed", h0/w0, h/w)
	}
	p := model
	p.Feat.PowerEstimate = false
	b10SetModel(t, p)
	if _, ok := box2TrimmedWatts(675, 45, 0, 36); ok {
		t.Fatal("no estimate without the power estimate commissioned")
	}
}

// --- presets ---

// v055PresetRun: a map run 600–688 MHz with presets, the fans held: the curve clean from 7.55 V (levels 45) at
// 675 MHz, less voltage at slower clocks (the model: 4 mV per MHz), more at 688.
func v055PresetRun(t *testing.T) *v055Sim {
	t.Helper()
	cfg := v055Cfg()
	cfg.LowestMHz, cfg.HighestMHz, cfg.Step = 600, 688, 12.5
	cfg.ChipTrim = false
	cfg.BuildPresets, cfg.FanHold, cfg.PresetMin = true, true, 10
	v := newV055Sim(t, cfg)
	fan := 60
	v.e.applyFan = func(p int) error { fan = p; return nil }
	v.e.readFan = func() (int, bool) { return fan, true }
	temp := 70.0
	v.e.readTemp = func() (float64, bool) { return temp, true }
	v.e.Run()
	return v
}

func TestV055Box2BuildPresets(t *testing.T) {
	v055Box2(t)
	*presetPath = filepath.Join(t.TempDir(), "presets.json")
	v := v055PresetRun(t)
	logs := strings.Join(v.e.fullLog, "\n")
	if os.Getenv("V055LOG") != "" {
		t.Log(logs)
		t.Logf("presets: %+v", loadPresets())
	}
	ps := loadPresets()
	by := map[string]preset{}
	for _, p := range ps {
		by[p.Name] = p
	}
	if len(by) != 4 {
		t.Fatalf("want 4 presets, got %+v\n%s", ps, logs)
	}
	curve := hwList(v.e.getCurve())
	lo, hi := curve[0].MHz, curve[len(curve)-1].MHz
	for _, n := range presetOrder {
		p, ok := by[n]
		if !ok {
			t.Fatalf("no %s preset", n)
		}
		if p.MV < 42 || p.MV > 61 || p.PV != 0 || p.MHz < lo || p.MHz > hi || p.MHz < 525 || !box2OnClockGrid(p.MHz) {
			t.Errorf("%s: %d MHz / level %d (pv %d) outside the Box II ranges or the tested %d–%d MHz", n, p.MHz, p.MV, p.PV, lo, hi)
		}
		if err := box2CheckTuple(p.MHz, p.MV); err != nil {
			t.Errorf("%s: the plan writer would refuse it: %v", n, err)
		}
		if p.FanProfile != "custom" || len(p.FanPoints) < 3 || p.TargetC < 72 || p.TargetC > 74 || p.Tested == "" {
			t.Errorf("%s: fan curve %q %v, target %d, tested %q", n, p.FanProfile, p.FanPoints, p.TargetC, p.Tested)
		}
		for _, q := range p.FanPoints {
			if q.Temp > 80 { // (the curve reaches 100 % 2 °C under the 82 °C abort)
				t.Errorf("%s: fan point at %g °C", n, q.Temp)
			}
		}
		// each preset's level is the curve's lowest clean one at its clock (or more voltage)
		for _, c := range curve {
			if c.MHz == p.MHz && p.MV > c.MV {
				t.Errorf("%s: %d MHz at %s, but the curve ran it clean at %s", n, p.MHz, box2LevelText(p.MV), box2LevelText(c.MV))
			}
		}
	}
	if !(by["High"].MHz >= by["Middle"].MHz && by["Middle"].MHz >= by["Low"].MHz && by["Low"].MHz >= by["Lowest power"].MHz && by["Lowest power"].MHz == lo) {
		t.Fatalf("preset clocks not in order: %+v", by)
	}
	// the saved presets are the Box II's runnable presets (the Presets panel, the schedule)
	if n := len(box2SavedPresets()); n != 4 {
		t.Fatalf("box2SavedPresets: %d", n)
	}
	if strings.Contains(logs, "PV ") || strings.Contains(logs, " mV") {
		t.Fatalf("log speaks PV/mV:\n%s", logs)
	}
}

// buildAndTestPresets keeps every Box II preset inside the window and the tested range (the SC Lite's "one clock step
// less" for Lowest would go under the slowest clock tested, or under 525 MHz).
func TestV055Box2PresetTriesInsideTestedRange(t *testing.T) {
	v055Box2(t)
	tries := map[string][]planMV{"Lowest power": {{MHz: 525, MV: 50}, {MHz: 513, MV: 50}, {MHz: 500, MV: 51}}, "High": {{MHz: 700, MV: 59}, {MHz: 675, MV: 59}}}
	box2PresetTries(tries, 525, 675)
	if len(tries["Lowest power"]) != 1 || tries["Lowest power"][0].MHz != 525 || len(tries["High"]) != 1 || tries["High"][0].MHz != 675 {
		t.Fatalf("tries %+v", tries)
	}
}

// Nothing confirmed: the run's trims come off before the presets (they'd be gone after the run).
func TestV055Box2TrimsOffBeforePresetsWhenNothingConfirmed(t *testing.T) {
	v055Box2(t)
	v := newV055Sim(t, v055Cfg())
	v.e.mu.Lock()
	v.e.runTrims = map[[2]int]float64{{0, 26}: -25}
	v.e.mu.Unlock()
	v.trims[[2]int{0, 26}] = -25
	v.e.box2TrimsOffBeforePresets()
	if len(v.trimsNow()) != 0 || v.runTrimCount() != 0 {
		t.Fatal("trims still on before the presets")
	}
	// confirmed: they stay
	v.e.mu.Lock()
	v.e.runTrims, v.e.confirmed, v.e.best = map[[2]int]float64{{0, 26}: -25}, true, &planMV{MHz: 675}
	v.e.mu.Unlock()
	v.trims[[2]int{0, 26}] = -25
	v.e.box2TrimsOffBeforePresets()
	if len(v.trimsNow()) != 1 {
		t.Fatal("a confirmed run's trims came off")
	}
}

// v055Presets: the four tuner presets as a run saves them, in the OEM's levels, each with its own curve.
func v055Presets() []preset {
	mk := func(n string, mhz, v, tc int) preset {
		return preset{Name: n, MHz: mhz, MV: v, Tested: "2026-11-15", TargetC: tc, FanProfile: "custom",
			FanPoints: tunedPresetCurve(float64(tc), 55, 30, 82)}
	}
	return []preset{mk("High", 675, 45, 72), mk("Middle", 625, 52, 73), mk("Low", 575, 58, 73), mk("Lowest power", 525, 61, 74)}
}

func v055Post(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	b, _ := json.Marshal(body)
	w := httptest.NewRecorder()
	handlePresets(w, httptest.NewRequest("POST", "/api/presets", strings.NewReader(string(b))))
	var out map[string]any
	json.Unmarshal(w.Body.Bytes(), &out)
	return out
}

func TestV055Box2PresetsApplyEditScheduleReboot(t *testing.T) {
	f := newC34(t, true, true)
	model.Feat.Tuner, model.Feat.ChipTrims, model.Feat.PowerEstimate = true, true, true
	model.VCodeDir = vcodeUpIsLess
	oldFan := fanCfg
	fp := filepath.Join(t.TempDir(), "fan.json")
	oldFP := fanPath
	fanPath = &fp
	t.Cleanup(func() { fanMu.Lock(); fanCfg = oldFan; fanMu.Unlock(); fanPath = oldFP })
	if err := savePresets(v055Presets()); err != nil {
		t.Fatal(err)
	}
	var persisted []planMV
	var pmu sync.Mutex
	oldPersist := box2PersistPlan
	box2PersistPlan = func(mhz, code int) {
		pmu.Lock()
		persisted = append(persisted, planMV{MHz: mhz, MV: code})
		pmu.Unlock()
	}
	t.Cleanup(func() { box2PersistPlan = oldPersist })

	// GET: the four presets next to the stock modes, each with its power figure
	w := httptest.NewRecorder()
	handlePresets(w, httptest.NewRequest("GET", "/api/presets", nil))
	var g map[string]any
	json.Unmarshal(w.Body.Bytes(), &g)
	names := []string{}
	for _, p := range g["presets"].([]any) {
		names = append(names, p.(map[string]any)["name"].(string))
	}
	if strings.Join(names, "|") != "Hashrate|Low-power|High|Middle|Low|Lowest power" {
		t.Fatalf("presets %v", names)
	}
	est, _ := g["est"].(map[string]any)
	hi, _ := est["High"].(map[string]any)
	if w0, _, _, _ := box2EstimateWatts(675, 45); hi == nil || math.Abs(hi["w"].(float64)-w0) > 0.11 {
		t.Fatalf("High's power figure %v, want ≈ %.1f W", est["High"], w0)
	}

	// apply (the owner): the plan writer (read back), its fan curve switched on, the owner's plan saved
	if r := v055Post(t, map[string]any{"action": "apply", "name": "Middle"}); r["ok"] != true {
		t.Fatalf("apply: %v", r)
	}
	f.mu.Lock()
	clk, lvl := f.clock, f.volt
	f.mu.Unlock()
	if clk != 625 || lvl != 52 {
		t.Fatalf("the miner runs %g MHz / level %g, want 625 / 52", clk, lvl)
	}
	fanMu.RLock()
	fc := fanCfg
	fanMu.RUnlock()
	if !fc.Enabled || fc.Profile != "custom" || len(fc.Points) != len(v055Presets()[1].FanPoints) || fc.Points[2] != v055Presets()[1].FanPoints[2] {
		t.Fatalf("the preset's curve isn't on: %+v", fc)
	}
	if getCharge() != "preset:Middle" {
		t.Fatalf("charge %q", getCharge())
	}
	waitFor := func(n int) []planMV {
		for i := 0; i < 200; i++ {
			pmu.Lock()
			c := append([]planMV(nil), persisted...)
			pmu.Unlock()
			if len(c) >= n {
				return c
			}
			time.Sleep(5 * time.Millisecond)
		}
		pmu.Lock()
		defer pmu.Unlock()
		return append([]planMV(nil), persisted...)
	}
	if p := waitFor(1); len(p) != 1 || p[0] != (planMV{MHz: 625, MV: 52}) {
		t.Fatalf("the owner's apply should save its plan for the next boot: %v", p)
	}
	// apply again
	if r := v055Post(t, map[string]any{"action": "apply", "name": "Middle"}); r["ok"] != true {
		t.Fatalf("apply again: %v", r)
	}
	waitFor(2)

	// a schedule step: the same writer, never saved as the owner's plan
	pmu.Lock()
	persisted = nil
	pmu.Unlock()
	if err := applyPresetNameGen("Lowest power", noGen); err != nil {
		t.Fatalf("schedule: %v", err)
	}
	f.mu.Lock()
	clk, lvl = f.clock, f.volt
	f.mu.Unlock()
	if clk != 525 || lvl != 61 {
		t.Fatalf("schedule: %g / %g", clk, lvl)
	}
	time.Sleep(30 * time.Millisecond)
	pmu.Lock()
	if len(persisted) != 0 {
		t.Fatalf("a schedule step was saved as the owner's plan: %v", persisted)
	}
	pmu.Unlock()
	// the schedule lists them
	found := 0
	for _, p := range schedulePresets() {
		b, _ := json.Marshal(p)
		for _, n := range []string{`"High"`, `"Middle"`, `"Low"`, `"Lowest power"`} {
			if strings.Contains(string(b), n) {
				found++
			}
		}
	}
	if found < 4 {
		t.Fatalf("the schedule doesn't offer the four presets: %v", schedulePresets())
	}

	// edit: the Box II ranges (levels 42–61, the clock on the 12.5 MHz grid, inside the window)
	for _, c := range []struct {
		mhz, v int
		want   string
	}{
		{590, 52, "enter 588"},
		{625, 62, "outside the allowed voltages"},
		{625, 41, "outside the allowed voltages"},
		{760, 45, "outside this model's clock window"},
		{500, 61, "outside this model's clock window"},
	} {
		r := v055Post(t, map[string]any{"action": "save", "name": "Middle", "mhz": c.mhz, "mv": c.v, "fan_profile": "custom", "fan_points": v055Presets()[1].FanPoints})
		if r["ok"] == true || !strings.Contains(r["err"].(string), c.want) {
			t.Errorf("edit %d MHz / level %d: %v, want %q", c.mhz, c.v, r, c.want)
		}
	}
	if r := v055Post(t, map[string]any{"action": "save", "name": "Middle", "mhz": 638, "mv": 50, "fan_profile": "custom", "fan_points": v055Presets()[1].FanPoints}); r["ok"] != true {
		t.Fatalf("edit 638 MHz (637.5): %v", r)
	}

	// a restart: the presets come back from their file, edited one included; its clock and level changed, so its
	// tuner tags are gone (v0.5.5 P5: the test was of the old setting)
	got := map[string]preset{}
	for _, p := range box2SavedPresets() {
		got[p.Name] = p
	}
	if len(got) != 4 || got["Middle"].MHz != 638 || got["Middle"].MV != 50 || got["Middle"].Tested != "" || got["Middle"].TargetC != 0 || got["Middle"].FanProfile != "custom" {
		t.Fatalf("after a restart: %+v", got)
	}
	// and they apply (the schedule's first step after the boot)
	if err := applyPresetNameGen("High", noGen); err != nil {
		t.Fatalf("after a restart: %v", err)
	}
}

// "Build from tuner" (v0.5.5 P3): refused on this model (the tuner's rebuild builds and tests them); nothing saved.
func TestV055Box2BuildFromTunerButton(t *testing.T) {
	newC34(t, true, true)
	model.Feat.Tuner = true
	model.VCodeDir = vcodeUpIsLess
	e := newTunerEngine(defaultTunerConfig())
	e.curve = []planMV{{MHz: 525, MV: box2LevelOf(61)}, {MHz: 600, MV: box2LevelOf(55)}, {MHz: 650, MV: box2LevelOf(49)}, {MHz: 675, MV: box2LevelOf(45)}}
	tunerMu.Lock()
	old := tunerCur
	tunerCur = e
	tunerMu.Unlock()
	t.Cleanup(func() { tunerMu.Lock(); tunerCur = old; tunerMu.Unlock() })
	r := v055Post(t, map[string]any{"action": "build"})
	if r["ok"] == true || !strings.Contains(fmt.Sprint(r["err"]), "Rebuild presets") {
		t.Fatalf("build on the Box II: %v", r)
	}
	if ps := loadPresets(); len(ps) != 0 {
		t.Fatalf("presets saved anyway: %+v", ps)
	}
}

// --- the API budget: the trims' re-apply asks nothing while a clock is taken ---

func TestV055Box2TrimReapplyQuietDuringConfirm(t *testing.T) {
	b10TempAuth(t)
	useBox2(t)
	dir := t.TempDir()
	oldStats := chipStatsPath
	chipStatsPath = filepath.Join(dir, "chipstats.json")
	t.Cleanup(func() { chipStatsPath = oldStats })
	os.WriteFile(chipStatsPath, []byte(`{"t":1,"chains":[{"chain":0,"chips":36,"base10":6900}]}`), 0o644)
	var calls int
	oldSet := reapplySetClock
	reapplySetClock = func() (int, bool) { calls++; return 690, true }
	t.Cleanup(func() { reapplySetClock = oldSet })
	box2Confirming.Add(1)
	_, err := reapplyTrims()
	box2Confirming.Add(-1)
	if err == nil || !strings.Contains(err.Error(), "isn't fresh yet") || calls != 0 {
		t.Fatalf("during a confirmation: %v (backend reads %d)", err, calls)
	}
	if _, err := reapplyTrims(); err != nil && strings.Contains(err.Error(), "taking a new clock") {
		t.Fatalf("after the quiet time it should go on: %v", err)
	}
	// the SC Lite: no such wait
	b10SetModel(t, profSCLite)
	box2Confirming.Add(1)
	_, err = reapplyTrims()
	box2Confirming.Add(-1)
	if err != nil && strings.Contains(err.Error(), "taking a new clock") {
		t.Fatalf("SC Lite: %v", err)
	}
}

// The v0.5.4c budget (TestV054cDashShareConfirmBudget's tuner clock write, fan loop and 3 pages) with the trims'
// re-apply tried every 2 s all through it, as a trim write would: still within the budget, and no "pgaset" (the
// re-apply's clock re-send) while the clock is confirmed.
func TestV055Box2ConfirmBudgetWithTrimReapply(t *testing.T) {
	const k = 10
	b10TempAuth(t)
	dir := t.TempDir()
	oldStats := chipStatsPath
	chipStatsPath = filepath.Join(dir, "chipstats.json")
	t.Cleanup(func() { chipStatsPath = oldStats })
	type call struct {
		start, end time.Time
		ok, busy   bool // went through; a setting write or a confirmation was in flight as it started
	}
	var mu sync.Mutex
	var calls []call
	var t0 time.Time
	v054cExtra = func() {
		os.WriteFile(chipStatsPath, []byte(`{"t":1,"chains":[{"chain":0,"chips":36,"base10":6900}]}`), 0o644) // (a fresh report)
		c := call{start: time.Now(), busy: box2Writing.Load() > 0 || box2Confirming.Load() > 0}
		_, err := reapplyTrims()
		c.end, c.ok = time.Now(), err == nil
		mu.Lock()
		calls = append(calls, c)
		mu.Unlock()
	}
	v054cAtWrite = func() { mu.Lock(); t0 = time.Now(); mu.Unlock() }
	t.Cleanup(func() { v054cExtra, v054cAtWrite = nil, nil })
	q, p, qBy, pBy := v054cConfirmRun(t, k, 30*time.Second/k, false)
	tEnd := time.Now()
	// a re-apply already on its way as the write starts (under planMu: its clock goes out before the write's PUT) may
	// end after t0; none that started while the write or its confirmation was in flight may go through (until the
	// confirmation ended, as the run returns)
	mu.Lock()
	inflight := 0
	for _, c := range calls {
		switch {
		case c.ok && c.busy && c.end.Before(tEnd.Add(-50*time.Millisecond)):
			t.Fatalf("a re-apply that started during the write/confirmation went through (%s after t0)", c.start.Sub(t0))
		case c.ok && !c.busy && c.end.After(t0):
			inflight++
		}
	}
	mu.Unlock()
	if inflight > 1 {
		t.Fatalf("%d re-applies ended after the write", inflight)
	}
	// (each in-flight re-apply: its devs and pgaset, sent before the write's PUT)
	all := v054cLastAll
	all["devs"] -= inflight
	all["pgaset"] -= inflight
	qBy["pgaset"] -= inflight
	t.Logf("with the trims' re-apply: quiet %.1f commands/min %v; confirm phase %.1f/min %v; whole window %v", q, qBy, p, pBy, all)
	// (v0.5.5 F1) the v0.5.4c budget counted per window, quiet time and confirm phase together (TestV054cDashShareConfirmBudget's
	// own): the re-apply adds none, no pgaset (checked below too), and it asks nothing while a write or a confirmation
	// is in flight
	v054cWindowBudget(t, all, v054cLastPhase)
	if qBy["pgaset"] > 0 || pBy["pgaset"] > 0 {
		t.Fatalf("the trims' re-apply sent the clock while it was confirmed: %v / %v", qBy, pBy)
	}
}

// --- the SC Lite: unchanged ---

func TestV055SCLiteTrimsPresetsUnchanged(t *testing.T) {
	b10SetModel(t, profSCLite)
	d := defaultTunerConfig()
	if d.ChipTrimMax != 3 || d.ChipTrimDeep != 50 || d.WarmestC != 75 || d.PresetMin != 20 {
		t.Fatalf("SC Lite defaults: %+v", d)
	}
	c := fastCfg()
	c.ChipTrim, c.BuildPresets = true, true
	c.WarmestC = 76 // (88 − 12 = 76: the SC Lite's 12 °C room, unchanged)
	if err := validateTunerConfig(c); err != nil {
		t.Fatalf("SC Lite: %v", err)
	}
	c.WarmestC = 77
	if err := validateTunerConfig(c); err == nil {
		t.Fatal("SC Lite: warmest 11 °C under the abort accepted")
	}
	c = fastCfg()
	c.Mode = "rebuild"
	if err := validateTunerConfig(c); err != nil { // (presets without the fan hold: the SC Lite allows it, as before)
		t.Fatalf("SC Lite rebuild: %v", err)
	}
	e := newTunerEngine(fastCfg())
	if done := e.box2TrimsRAMBegin(); box2TrimsInRAM() {
		t.Fatal("SC Lite: the trims file must never be RAM-only")
	} else {
		done()
	}
	r := testResult{}
	b, _ := json.Marshal(r)
	if strings.Contains(string(b), "trims") || strings.Contains(string(b), "est_w") {
		t.Fatalf("SC Lite result rows changed: %s", b)
	}
	m, _ := json.Marshal(testChipMap{})
	if strings.Contains(string(m), "trims") {
		t.Fatalf("SC Lite chip maps changed: %s", m)
	}
	if box2TrimmedWant(1000, 625, 0, 46) != 1000 {
		t.Fatal("no trims: the hashrate want is unchanged")
	}
}
