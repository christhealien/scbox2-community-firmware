package main

// v0.5.5 review round: the Box II tuner's presets, chip trims, the API quiet time and the public release's gating.
// Each test names the finding it covers (P = presets, T = trims, F = freeze / UI / release).

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// P1: a rebuild moves its presets by the form's clock and voltage steps: the same rules as a map.
func TestV055bRebuildStepsValidated(t *testing.T) {
	v055Box2(t)
	d := defaultTunerConfig()
	d.DryRun, d.Mode, d.FanHold = false, "rebuild", true
	try := func(mut func(c *tunerConfig)) error {
		c := d
		mut(&c)
		c, _ = box2TunerNormalize(c)
		return validateTunerConfig(c)
	}
	if err := try(func(*tunerConfig) {}); err != nil {
		t.Fatalf("the default rebuild: %v", err)
	}
	for name, c := range map[string]struct {
		mut  func(c *tunerConfig)
		want string
	}{
		"clock step 0":          {func(c *tunerConfig) { c.Step = 0 }, "clock step"},
		"clock step off grid":   {func(c *tunerConfig) { c.Step = 20 }, "clock step"},
		"clock step 150":        {func(c *tunerConfig) { c.Step = 150 }, "clock step"},
		"voltage step 0":        {func(c *tunerConfig) { c.MVStep = 0 }, "voltage step"},
		"voltage step negative": {func(c *tunerConfig) { c.MVStep = -1 }, "voltage step"},
		"level 30":              {func(c *tunerConfig) { c.MinMV = 30 }, "voltages must be between"},
		"level 70":              {func(c *tunerConfig) { c.MaxMV = 70 }, "voltages must be between"},
		"lowest over highest":   {func(c *tunerConfig) { c.MinMV, c.MaxMV = 61, 42 }, "voltages must be between"},
	} {
		if err := try(c.mut); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want an error with %q, got %v", name, c.want, err)
		}
	}
	// the SC Lite: its rebuild rules are as before
	b10SetModel(t, profSCLite)
	c := fastCfg()
	c.Mode, c.Step = "rebuild", 0
	if err := validateTunerConfig(c); err != nil {
		t.Fatalf("SC Lite rebuild (unchanged): %v", err)
	}
}

// P2: an owner's plan PUT doesn't commit the plan files at once: box2PersistPlan does, only while the miner still runs
// the plan the PUT asked for. With the live plan unreadable (or moved on) the flash keeps its plan; the other OEM
// files the change touched are saved as before.
func TestV055bOwnerPlanSaveChecked(t *testing.T) {
	sh := withShadow(t)
	*minerWeb = "127.0.0.1:1" // (no backend: the live plan can't be read)
	box2PersistLast.mu.Lock()
	box2PersistLast.at = time.Time{}
	box2PersistLast.mu.Unlock()
	const cpbRel = "minerd/settings/cpbsetting.json"
	sh.putBoth(t, cpbRel, `{"manual":true,"manualPowerplan":"650 MHz 42 V 70 RPM 41 RPM"}`, `{"manual":true,"manualPowerplan":"690 MHz 42 V 50 RPM 41 RPM"}`)
	sh.putBoth(t, "minerd/settings/pools.json", `{"pools":["b"]}`, `{"pools":["a"]}`)
	cfgUserSave(func() {
		stockSettingPut([]byte(`{"manual":true,"manualPowerplan":"650 MHz 42 V 70 RPM 41 RPM"}`))
		stockOtherPuts.Add(1)
	})
	time.Sleep(200 * time.Millisecond) // (box2PersistPlan's first pass: it reads the live plan, and can't)
	if s, _ := sh.onFlash(cpbRel); strings.Contains(s, "650") {
		t.Fatalf("the plan was saved without its checks: %q", s)
	}
	if s, _ := sh.onFlash("minerd/settings/pools.json"); !strings.Contains(s, `"b"`) {
		t.Fatalf("the other OEM file the change touched wasn't saved: %q", s)
	}
}

// P3: the Presets tab's "Build from tuner" is hidden on this model (the server refuses it: TestV055Box2BuildFromTunerButton).
func TestV055bBuildButtonHiddenOnBox2(t *testing.T) {
	if !strings.Contains(indexHTML, "if(B2T()){const b=$('btnPresetBuild');if(b&&b.classList)b.classList.add('hide');}") {
		t.Fatal("the Box II page still shows “Build from tuner”")
	}
}

// P4: the tuner's saved curve reaches the flash (writeAtomic), so a rebuild after a reboot has its curve.
func TestV055bSavedCurveOnFlash(t *testing.T) {
	sh := withShadow(t)
	saveSavedCurve(savedCurve{Curve: []planMV{{MHz: 675, MV: 45}}})
	if s, ok := sh.onFlash("scbox2/tuner-curve.json"); !ok || !strings.Contains(s, "675") {
		t.Fatalf("tuner-curve.json not on the flash: %q %v", s, ok)
	}
	if got := loadSavedCurve(); len(got.Curve) != 1 || got.Curve[0].MHz != 675 {
		t.Fatalf("read back %+v", got)
	}
}

// P5: an edit keeps a preset's "tested" and its temperature only when its clock and level stay the same.
func TestV055bEditKeepsTestedOnlyIfSame(t *testing.T) {
	newC34(t, true, true)
	model.Feat.Tuner = true
	model.VCodeDir = vcodeUpIsLess
	fp := filepath.Join(t.TempDir(), "fan.json")
	oldFP := fanPath
	fanPath = &fp
	t.Cleanup(func() { fanPath = oldFP })
	if err := savePresets(v055Presets()); err != nil {
		t.Fatal(err)
	}
	pts := v055Presets()[1].FanPoints
	if r := v055Post(t, map[string]any{"action": "save", "name": "Middle", "mhz": 625, "mv": 52, "fan_profile": "custom", "fan_points": pts}); r["ok"] != true {
		t.Fatalf("save: %v", r)
	}
	get := func(n string) preset {
		for _, p := range loadPresets() {
			if p.Name == n {
				return p
			}
		}
		return preset{}
	}
	if p := get("Middle"); p.Tested != "2026-11-15" || p.TargetC != 73 {
		t.Fatalf("same clock and level (only the fans edited): the test tags went: %+v", p)
	}
	if r := v055Post(t, map[string]any{"action": "save", "name": "Low", "mhz": 575, "mv": 57, "fan_profile": "custom", "fan_points": pts}); r["ok"] != true {
		t.Fatalf("save: %v", r)
	}
	if p := get("Low"); p.Tested != "" || p.TargetC != 0 {
		t.Fatalf("a new level kept the old test's tags: %+v", p)
	}
}

// P6 + P7: a preset saved above the window locked again lower is listed, marked, for deleting; applying or
// scheduling it fails saying why. The owner's meter reading shows with the power estimate off.
func TestV055bOutsidePresetsAndMeter(t *testing.T) {
	newC34(t, true, true)
	model.Feat.Tuner, model.Feat.PowerEstimate = true, false
	model.VCodeDir = vcodeUpIsLess
	ps := append(v055Presets(), preset{Name: "Fast", MHz: 760, MV: 45, FanProfile: "none"})
	if err := savePresets(ps); err != nil {
		t.Fatal(err)
	}
	box2PowMu.Lock()
	oldPow, oldRead := box2PowCfg, box2PowRead
	box2PowMu.Unlock()
	t.Cleanup(func() { box2PowMu.Lock(); box2PowCfg, box2PowRead = oldPow, oldRead; box2PowMu.Unlock() })
	if err := saveBox2PowerConfig(box2PowerConfig{Cals: []box2PowerCal{{MHz: 675, V: 45, Watts: 1234.5}}}); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	handlePresets(w, httptest.NewRequest("GET", "/api/presets", nil))
	var g map[string]any
	json.Unmarshal(w.Body.Bytes(), &g)
	out, _ := g["outside"].([]any)
	if len(out) != 1 || out[0].(map[string]any)["name"] != "Fast" {
		t.Fatalf("outside: %v", g["outside"])
	}
	for _, p := range g["presets"].([]any) {
		if p.(map[string]any)["name"] == "Fast" {
			t.Fatal("the preset outside the window is listed as runnable")
		}
	}
	est, _ := g["est"].(map[string]any)
	if hi, _ := est["High"].(map[string]any); hi == nil || hi["w"].(float64) != 1234.5 || !strings.Contains(fmt.Sprint(hi["src"]), "wall-meter") {
		t.Fatalf("the owner's meter reading isn't shown with the estimate off: %v", est)
	}
	if _, ok := est["Middle"]; ok {
		t.Fatalf("an estimate shown with the estimate off: %v", est)
	}
	if err := applyBox2PresetGen("Fast", noGen); err == nil || !strings.Contains(err.Error(), "outside the current clock window") {
		t.Fatalf("apply: %v", err)
	}
	s := schedule{Rules: []schedRule{{Start: "08:00", Days: []int{0}, Preset: "Fast"}}}
	if err := validateSchedule(&s); err == nil || !strings.Contains(err.Error(), "outside the current clock window") {
		t.Fatalf("schedule: %v", err)
	}
	if !strings.Contains(indexHTML, "Outside the current clock window") || !strings.Contains(indexHTML, "function presetDeleteOut(") {
		t.Fatal("the page doesn't show the presets outside the window")
	}
}

// T1: a run that ended well leaves no restart marker on the flash: a power cut afterwards undoes nothing.
func TestV055bMarkerClearReachesFlash(t *testing.T) {
	sh := withShadow(t)
	useModel(t, profSCBox2)
	model.Feat.Tuner = true
	m := tunerMarker{Started: 1, Base: box2TunerBaseline(), Trims: []chipTrim{{Chain: 0, Chip: 5, MHz: -12.5}}, KeptSet: true}
	b, _ := json.Marshal(m)
	if err := writeAtomic(tunerMarkerPath(), b, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := sh.onFlash("scbox2/tuning.running"); !ok {
		t.Fatal("the marker isn't on the flash while the run goes")
	}
	clearTunerMarker() // (the run's end, tunerEnded)
	if _, ok := sh.onFlash("scbox2/tuning.running"); ok {
		t.Fatal("the cleared marker is still on the flash")
	}
	// the power cut: the RAM copy is what the flash holds
	os.RemoveAll(sh.ram)
	os.MkdirAll(filepath.Dir(sh.ram), 0o700)
	if err := copyTree(sh.flash, sh.ram); err != nil {
		t.Fatal(err)
	}
	var put []planMV
	old := tunerRecoverApply
	tunerRecoverApply = func(p planMV) error { put = append(put, p); return nil }
	t.Cleanup(func() { tunerRecoverApply = old })
	tunerRecover(0)
	if len(put) != 0 {
		t.Fatalf("the recovery undid a run that had ended: %v", put)
	}
}

// T1: the watchdog's end marks the run over before it clears the marker (the flash copy never reads as cut short).
func TestV055bForceEndMarksFirst(t *testing.T) {
	sh := withShadow(t)
	useModel(t, profSCBox2)
	model.Feat.Tuner = true
	captureNotify(t)
	oldApply := tunerForceApply
	t.Cleanup(func() { tunerForceApply = oldApply })
	var seen []string
	tunerForceApply = func(planMV) error {
		s, _ := sh.onFlash("scbox2/tuning.running")
		seen = append(seen, s)
		return nil
	}
	e := newTunerEngine(fastCfg())
	e.cfg.DryRun = false
	base := box2TunerBaseline()
	e.baseAtom.Store(&base)
	e.startedAt = time.Now()
	if err := writeTunerMarker(e); err != nil {
		t.Fatal(err)
	}
	setCharge("tuner")
	t.Cleanup(func() { setCharge("") })
	tunerForceEnd(e, "test")
	if len(seen) != 1 || !strings.Contains(seen[0], `"ended"`) {
		t.Fatalf("the flash's marker as the miner went back: %q", seen)
	}
	if _, ok := sh.onFlash("scbox2/tuning.running"); ok {
		t.Fatal("the marker is still on the flash after the put-back")
	}
}

// T2 + T3: the run's end sets the trims file to the trims before the run (unconfirmed) or those plus the kept ones
// (confirmed), whatever the run's writes left, and the file only (no live re-send).
func TestV055bEndTrimsReconciled(t *testing.T) {
	v055Box2(t)
	for _, keep := range []bool{false, true} {
		e := newTunerEngine(v055Cfg())
		file := map[[2]int]float64{{0, 5}: -12.5, {0, 26}: -25, {0, 23}: -12.5} // (b0.c23: a put-back that failed)
		var liveOff []bool
		e.trimGet = func() map[[2]int]float64 { return copyTrimMap(file) }
		e.trimSet = func(chain, chip int, mhz float64) error {
			liveOff = append(liveOff, e.trimFileOnly.Load())
			if mhz == 0 {
				delete(file, [2]int{chain, chip})
			} else {
				file[[2]int{chain, chip}] = mhz
			}
			return nil
		}
		e.baseTrims = map[[2]int]float64{{0, 5}: -12.5}
		e.runTrims = map[[2]int]float64{{0, 26}: -25}
		e.box2EndTrims(keep)
		want := map[[2]int]float64{{0, 5}: -12.5}
		if keep {
			want[[2]int{0, 26}] = -25
		}
		if fmt.Sprint(file) != fmt.Sprint(want) {
			t.Errorf("keep=%v: the trims file %v, want %v", keep, file, want)
		}
		for _, off := range liveOff {
			if !off {
				t.Errorf("keep=%v: a live re-send at the run's end", keep)
			}
		}
		if e.trimFileOnly.Load() {
			t.Error("file-only left on")
		}
		if !keep && (e.trimsUndone != 1 || len(e.runTrims) != 0) {
			t.Errorf("undone %d, run trims %v", e.trimsUndone, e.runTrims)
		}
	}
	// the production trimSet with file-only: written, nothing asked of the hook (none reports here)
	b10TempAuth(t)
	dir := t.TempDir()
	oldStats := chipStatsPath
	chipStatsPath = filepath.Join(dir, "none.json")
	t.Cleanup(func() { chipStatsPath = oldStats })
	e := newTunerEngine(v055Cfg())
	e.trimFileOnly.Store(true)
	if err := e.trimSet(0, 7, -12.5); err != nil {
		t.Fatalf("file-only trim: %v", err)
	}
	if ts := readChipTrims(); len(ts) != 1 || ts[0].Chip != 7 {
		t.Fatalf("trims file %v", ts)
	}
	e.trimFileOnly.Store(false)
	if err := e.trimSet(0, 7, 0); err == nil {
		t.Fatal("the live re-send ran without a hook report")
	}
}

// T4 + F2: the trims' re-send waits out an Idle, a mining hold and the quiet time after a revert or a whole-setting write.
func TestV055bReapplyRefusedIdleAndQuiet(t *testing.T) {
	newC34(t, true, true)
	dir := t.TempDir()
	oldStats := chipStatsPath
	chipStatsPath = filepath.Join(dir, "chipstats.json")
	t.Cleanup(func() { chipStatsPath = oldStats; setCharge("") })
	fresh := func() {
		os.WriteFile(chipStatsPath, []byte(`{"t":1,"chains":[{"chain":0,"chips":36,"base10":6900}]}`), 0o644)
	}
	fresh()
	setCharge("idle")
	if _, err := reapplyTrims(); err == nil || !strings.Contains(err.Error(), "Idle") {
		t.Fatalf("on Idle: %v", err)
	}
	setCharge("")
	box2QuietFor(time.Hour)
	if !box2ClockQuietNow() {
		t.Fatal("no quiet time")
	}
	fresh()
	if _, err := reapplyTrims(); err == nil || !strings.Contains(err.Error(), "fresh") {
		t.Fatalf("in the quiet time: %v", err)
	}
	box2QuietUntil.Store(0)
	// a whole-setting write (a snapshot put back) starts a quiet time of its own
	if err := putSettingGen(noGen, []byte(box2Setting)); err != nil {
		t.Fatal(err)
	}
	if !box2ClockQuietNow() {
		t.Fatal("putSettingGen: no quiet time after it")
	}
	box2QuietUntil.Store(0)
	// a failed confirmation's revert: its own 30 s
	w := box2Want{MHz: 650, V: 42, Before: powerplan{FreqMHz: 690, VrunMv: 42}, Seq: box2WriteSeq.Load()}
	var cur map[string]any
	json.Unmarshal([]byte(box2Setting), &cur)
	cur["manual"] = true
	cur["manualPowerplan"] = "650 MHz 42 V 70 RPM 41 RPM"
	b, _ := json.Marshal(cur)
	if err := putSettingGen(noGen, b); err != nil {
		t.Fatal(err)
	}
	box2QuietUntil.Store(0)
	w.Plan, w.Seq = "650 MHz 42 V 70 RPM 41 RPM", box2WriteSeq.Load()
	if plan, err := box2Revert(w); err != nil || plan == "" {
		t.Fatalf("revert: %q %v", plan, err)
	}
	if left := time.Until(time.Unix(0, box2QuietUntil.Load())); left < 25*time.Second {
		t.Fatalf("revert: quiet for %s, want about 30 s", left)
	}
}

// T5: while a run tries trims, neither an owner's save of the panel's folder nor the checkpoint saves the trims
// file; a run that no longer owns the file doesn't save it at its end.
func TestV055bTrimsStayInRAMEverywhere(t *testing.T) {
	sh := withShadow(t)
	v055Box2(t)
	sh.putBoth(t, "scbox2/chiptrim", "# before\n", "# before\n")
	e := newTunerEngine(v055Cfg())
	end := e.box2TrimsRAMBegin()
	os.WriteFile(chipTrimPath(), []byte("0 26 -25\n"), 0o644)
	cfgUserSave(func() { os.WriteFile(chipTrimPath(), []byte("0 26 -25\n0 23 -12.5\n"), 0o644) })
	cfgCheckpoint()
	if s, _ := sh.onFlash("scbox2/chiptrim"); s != "# before\n" {
		t.Fatalf("the run's test trims reached the flash: %q", s)
	}
	e2 := newTunerEngine(v055Cfg())
	box2TrimRAMOwner.Store(e2) // (another run owns the file now)
	end()
	if s, _ := sh.onFlash("scbox2/chiptrim"); s != "# before\n" {
		t.Fatalf("a run that didn't own the file saved it: %q", s)
	}
	box2TrimRAMOwner.Store(nil)
	cfgCheckpoint()
	if s, _ := sh.onFlash("scbox2/chiptrim"); !strings.Contains(s, "0 23") {
		t.Fatalf("after the run the file is saved: %q", s)
	}
}

// T6: whether trims could fix a fail follows trimOne's rules: a hand-trimmed chip, a chip at the deepest trim and the
// second chip of a pair can't be trimmed, so their errors count against the fix.
func TestV055bCanFixFollowsTrimRules(t *testing.T) {
	v055Box2(t)
	cfg := v055Cfg()
	cfg.ChipTrimMax = 2
	e := newTunerEngine(cfg)
	trims := map[[2]int]float64{}
	e.trimGet = func() map[[2]int]float64 { return copyTrimMap(trims) }
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
	e.baseRate = map[ckey]float64{}
	for i := 1; i <= 36; i++ {
		e.baseRate[ckey{0, i}] = 0.01
	}
	e.baseMHz = 675
	st := func(m map[int]int) tunerStats {
		g := map[ckey]int{}
		for i := 1; i <= 36; i++ {
			g[ckey{0, i}] = m[i]
		}
		return tunerStats{Gained: g}
	}
	cand := planMV{MHz: 675, MV: 3}
	if _, _, ok := e.box2TrimsCanFix(cand, st(map[int]int{3: 30, 9: 30}), 0); !ok {
		t.Fatal("two worst chips, both trimmable: should be fixable")
	}
	trims[[2]int{0, 3}] = -12.5 // set by hand
	if rest, _, ok := e.box2TrimsCanFix(cand, st(map[int]int{3: 30, 9: 30}), 0); ok || rest != 30 {
		t.Fatalf("a hand-trimmed chip counted as trimmable: rest %d ok %v", rest, ok)
	}
	delete(trims, [2]int{0, 3})
	if rest, _, ok := e.box2TrimsCanFix(cand, st(map[int]int{3: 30, 4: 30}), 0); ok || rest != 30 {
		t.Fatalf("both chips of a pair counted as trimmable: rest %d ok %v", rest, ok)
	}
	e.runTrims = map[[2]int]float64{{0, 11}: -12.5, {0, 15}: -12.5} // (the run's two slots used)
	if rest, _, ok := e.box2TrimsCanFix(cand, st(map[int]int{3: 30}), 0); ok || rest != 30 {
		t.Fatalf("no slot left, yet trimmable: rest %d ok %v", rest, ok)
	}
	e.runTrims = map[[2]int]float64{{0, 3}: -50} // (the run's own, at the deepest trim)
	trims[[2]int{0, 3}] = -50
	if rest, _, ok := e.box2TrimsCanFix(cand, st(map[int]int{3: 30}), 0); ok || rest != 30 {
		t.Fatalf("a chip at the deepest trim counted as trimmable: rest %d ok %v", rest, ok)
	}
}

// T7: at most 4 chips per board on this model (validation and the form); the hashrate allowance is bounded by it.
func TestV055bTrimMaxClamped(t *testing.T) {
	v055Box2(t)
	if !strings.Contains(indexHTML, `id="t_ctmax" min="1" max="'+(B2T()?4:10)+'"`) {
		t.Fatal("the form's limit isn't 4 on this model")
	}
	cfg := v055Cfg()
	e := newTunerEngine(cfg)
	file := map[[2]int]float64{}
	for i := 1; i <= 12; i += 2 {
		file[[2]int{0, i}] = -75 // six chips, deeper than the run may go
	}
	e.trimGet = func() map[[2]int]float64 { return copyTrimMap(file) }
	e.baseTrims = map[[2]int]float64{}
	if got := e.box2RunTrimLoss(); got != 4*50 {
		t.Fatalf("trim loss %g, want 200 (4 chips × 50 MHz)", got)
	}
}

// T8: the trims auto-trim owns (the tuner's it took over) survive a power cut: their record reaches the flash.
func TestV055bAdoptedTrimsOnFlash(t *testing.T) {
	sh := withShadow(t)
	cwMu.Lock()
	oSt, oLoaded := cwSt, cwLoaded
	cwSt, cwLoaded = cwState{NoHelp: map[string]int64{}}, true
	cwMu.Unlock()
	t.Cleanup(func() { cwMu.Lock(); cwSt, cwLoaded = oSt, oLoaded; cwMu.Unlock() })
	chipWatchAdopt([]chipTrim{{Chain: 0, Chip: 26, MHz: -25}}, "kept by the tuning run")
	if s, ok := sh.onFlash("scbox2/chipwatch.json"); !ok || !strings.Contains(s, `"chip": 26`) {
		t.Fatalf("the kept trims' record isn't on the flash: %q", s)
	}
	// its counters alone don't open a window
	n := sh.opened()
	cwMu.Lock()
	cwLog("a tick")
	cwSave()
	cwMu.Unlock()
	if sh.opened() != n {
		t.Fatal("a counters-only save wrote the flash")
	}
}

// F3: the public release (v0.5.5) offers the tuner's chip trims and preset building as the owner's build does: the
// server takes them and the page shows them. (Named TestPublic*: it runs in the public build's test pass too.)
func TestPublicV055TunerExtrasOffered(t *testing.T) {
	v055Box2(t)
	c := defaultTunerConfig()
	c.DryRun = false
	c, _ = box2TunerNormalize(c)
	for name, mut := range map[string]func(c *tunerConfig){
		"the clock and voltage search": func(c *tunerConfig) {},
		"trim weak chips":              func(c *tunerConfig) { c.ChipTrim = true },
		"build presets":                func(c *tunerConfig) { c.BuildPresets, c.FanHold = true, true },
		"rebuild":                      func(c *tunerConfig) { c.Mode, c.FanHold = "rebuild", true },
	} {
		x := c
		mut(&x)
		if err := validateTunerConfig(x); err != nil {
			t.Errorf("public=%v: %s refused: %v", publicBuild, name, err)
		}
	}
	page := pageForVariant(indexHTML, publicBuild)
	for _, want := range []string{"chk('t_chiptrim'", "chk('t_buildpresets'", "'rebuild',mode,'Rebuild the presets", "function pubNoTunerP(){return PUB()&&featOff2(F().Tuner);}"} {
		if !strings.Contains(page, want) {
			t.Errorf("public=%v: the page lacks %s", publicBuild, want)
		}
	}
	for _, gone := range []string{"extras_off", "tuner_extras_off"} {
		if strings.Contains(page, gone) {
			t.Errorf("public=%v: the page still hides the tuner's extras (%s)", publicBuild, gone)
		}
	}
}

// F4 + F5: the Box II "Hold on" wording doesn't wait for Controls; the watts note only with the power estimate on.
func TestV055bTunerTexts(t *testing.T) {
	if strings.Contains(indexHTML, "((CTRL&&CTRL.box2)?'the Dashboard") {
		t.Fatal("“Hold on” still waits for Controls")
	}
	v055Box2(t)
	if n := fmt.Sprint(box2TunerInfo()["note"]); !strings.Contains(n, "the watts shown are estimates") {
		t.Fatalf("estimate on: %s", n)
	}
	model.Feat.PowerEstimate = false
	if n := fmt.Sprint(box2TunerInfo()["note"]); strings.Contains(n, "watts") {
		t.Fatalf("estimate off: %s", n)
	}
}

// copyTree: src's files into dst (a power cut's "the RAM copy is what the flash holds").
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o700)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), b, 0o600)
	})
}
