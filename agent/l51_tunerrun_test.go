package main

// L51: the owner's v1.5.21 tuner run (SCLite-1, 2026-11-08): map 500–700 MHz step 25, PV 9100–9600 step 50,
// bottom-up, chip trims on (3 per board). Board 3 is simulated like that miner's: b3.c30 needs a trim from
// 600 MHz on, b3.c33 and b3.c3 need one at 625 MHz below PV 9200 (not at 700 MHz / PV 9500), b3.c41 limits
// 650–700 MHz, b3.c21 too. The trim check at 700 / PV 9500 lifts c3 and c33; the Middle preset is 625 / PV 9150,
// mapped clean WITH both trims.

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func l51cfg() tunerConfig {
	c := scfg()
	c.TopDown = false
	c.LowestMHz, c.HighestMHz, c.Step = 500, 700, 25
	c.MinMV, c.MaxMV, c.MVStep = 8800, 9300, 50
	c.BuildPresets, c.PresetMin, c.WarmestC = true, 20, 75
	return c
}

// l51board: the run's board 3 (rate 0.6 errors/min while under the need, unless trimmed -12.5 when trimFix).
func l51board() []fmRule { return l51boardC3(0.6) }

// l51boardC3: c3's error rate as given (slower than c33's: the miner then names c33 alone first, as in the run).
func l51boardC3(c3rate float64) []fmRule {
	always := 9301
	return []fmRule{
		needRule(ckey{3, 30}, map[int]int{600: always, 625: always, 650: always, 675: always, 700: always}, 0.6, -12.5),
		needRule(ckey{3, 33}, map[int]int{625: 8900, 650: 8950, 675: 9050, 700: 9150}, 0.6, -12.5),
		needRule(ckey{3, 3}, map[int]int{625: 8900, 650: 8950, 675: 9050, 700: 9150}, c3rate, -12.5),
		needRule(ckey{3, 41}, map[int]int{625: 8850, 650: 8900, 675: 9100, 700: 9200}, 0.6, -12.5),
		needRule(ckey{3, 21}, map[int]int{650: 9000, 675: 9100, 700: 9150}, 0.6, -12.5),
	}
}

// l51clean: the chips that make errors at p with trims (the simulated truth).
func l51clean(f *fakeMiner, p planMV, trims map[[2]int]float64) []string {
	saveP, saveT := f.plan, f.trims
	f.plan, f.trims = p, trims
	defer func() { f.plan, f.trims = saveP, saveT }()
	var bad []string
	for b := 0; b < 4; b++ {
		for i := 1; i <= 46; i++ {
			for _, r := range f.rules {
				if r(f, ckey{b, i}) > 0 {
					bad = append(bad, fmt.Sprintf("b%d.c%d", b, i))
				}
			}
		}
	}
	return bad
}

// l51guard: the per-board cap and the pair rule hold at every moment of the run.
func l51guard(t *testing.T, f *fakeMiner, max int) *[]string {
	var bad []string
	prev := f.onTick
	f.onTick = func(f *fakeMiner) {
		if prev != nil {
			prev(f)
		}
		n := map[int]int{}
		for k, v := range f.trims {
			if v < 0 {
				n[k[0]]++
				if p := chipPartner(k[1]); f.trims[[2]int{k[0], p}] < 0 && len(bad) < 5 {
					bad = append(bad, fmt.Sprintf("pair b%d.c%d+c%d both slowed", k[0], k[1], p))
				}
			}
		}
		for b, c := range n {
			if c > max && len(bad) < 5 {
				bad = append(bad, fmt.Sprintf("board %d has %d trims (cap %d): %v", b, c, max, f.trims))
			}
		}
	}
	return &bad
}

func l51presets(t *testing.T) {
	op := *presetPath
	*presetPath = filepath.Join(t.TempDir(), "presets.json")
	t.Cleanup(func() { *presetPath = op })
}

// The run's situation: the Middle preset 625 MHz / PV 9150 mapped clean with b3.c3 and b3.c33 trimmed; the trim
// check at 700 / PV 9500 lifted both. Every preset saved must be clean with the trims the run leaves on, Middle
// must stay at PV 9150 (both trims back: not PV 9200 for every chip, and not reused from the map without them),
// the pair rule and the 3-per-board cap hold throughout, and the restart marker (onTrims), runTrims, the
// end-of-run "CHIP TRIMS kept" line and the hand-off to auto-trim all agree with the miner's trims.
func TestL51MiddlePresetGetsLiftedTrimsBack(t *testing.T) {
	// 0.6: c3 and c33 fail the preset together (the miner says "board 3"); 0.25: c33 fails alone first, as in
	// the run, and c3 after its trim is back
	for _, rate := range []float64{0.6, 0.25} {
		t.Run(fmt.Sprintf("c3rate=%g", rate), func(t *testing.T) { l51middle(t, rate) })
	}
}

func l51middle(t *testing.T, c3rate float64) {
	l51presets(t)
	e, f := newFM(t, l51cfg())
	f.rules = l51boardC3(c3rate)
	f.temp = func(f *fakeMiner) (float64, bool) { return f.e.holdTarget, true } // the fans hold it
	bad := l51guard(t, f, 3)
	var marker map[[2]int]float64
	e.onTrims = func(m map[[2]int]float64) { marker = copyTrimMap(m) }
	var adopted []chipTrim
	e.adoptTrims = func(ts []chipTrim, _ string) { adopted = ts }
	runFM(t, e, f)
	dump(t, e)
	if len(*bad) > 0 {
		t.Fatalf("trim rules broken: %v", *bad)
	}
	mustContain(t, e, "TRIM CHECK at 700 MHz / PV 9500", "CONFIRMED: 700 MHz / PV 9500")
	if !strings.Contains(logStr(e), "b3.c33 at +0 MHz: still clean") || !strings.Contains(logStr(e), "b3.c3 at +0 MHz: still clean") {
		t.Fatalf("scenario: the trim check didn't lift c3 and c33:\n%s", tailLog(e, 40))
	}
	final := map[[2]int]float64{}
	for k, v := range f.trims {
		final[k] = v
	}
	ps := loadPresets()
	var got []string
	for _, p := range ps {
		got = append(got, fmt.Sprintf("%s %d/%d", p.Name, p.MHz, p.PV))
		if bad := l51clean(f, planMV{p.MHz, p.MV, p.PV}, final); len(bad) > 0 {
			t.Errorf("preset %s %d MHz / PV %d isn't clean with the trims the run leaves (%s): %v\n%s", p.Name, p.MHz, p.PV, sortedTrims(final), bad, tailLog(e, 30))
		}
		if p.Name == "Middle" && (p.MHz != 625 || p.PV != 9150) {
			t.Errorf("Middle is %d MHz / PV %d: 625 / PV 9150 ran clean in the map with c3 and c33 trimmed", p.MHz, p.PV)
		}
	}
	if len(ps) != 4 {
		t.Fatalf("presets: %v\n%s", got, tailLog(e, 40))
	}
	if sortedTrims(e.runTrims) != sortedTrims(final) || sortedTrims(marker) != sortedTrims(final) {
		t.Fatalf("trims disagree: miner %s, runTrims %s, marker %s", sortedTrims(final), sortedTrims(e.runTrims), sortedTrims(marker))
	}
	var ad, kept []string
	for _, c := range adopted {
		ad = append(ad, fmt.Sprintf("%d.%d=%g", c.Chain, c.Chip, c.MHz))
	}
	for k, v := range final {
		kept = append(kept, fmt.Sprintf("b%d.c%d %+.4g MHz", k[0], k[1], v))
	}
	sort.Strings(ad)
	sort.Strings(kept)
	if strings.Join(ad, ",") != sortedTrims(final) {
		t.Fatalf("auto-trim got %v, the miner has %s", ad, sortedTrims(final))
	}
	mustContain(t, e, "CHIP TRIMS kept by this run (in Controls -> Chip trims; each made a failing test pass): "+strings.Join(kept, ", "))
	// the High preset is the confirm test, at the lifted trims (they only came back for Middle, later)
	mustContain(t, e, "PRESET Middle: 625 MHz / PV 9150")
	if strings.Contains(logStr(e), "PRESET Middle: 625 MHz / PV 9200") {
		t.Fatalf("Middle went up to PV 9200:\n%s", tailLog(e, 30))
	}
}

// A lifted trim put back for a preset whose setting still fails: it comes off again, so the next try (and the
// trims kept) are what was tested, and nothing loops: each lifted trim comes back at most once per setting.
func TestL51RestoredTrimUndoneWhenPresetStillFails(t *testing.T) {
	l51presets(t)
	e, f := newFM(t, l51cfg())
	f.rules = append(l51board(),
		// b0.c9 makes the preset's 625 / PV 9150 fail anyway (warmer than the map): a trim can't help it
		func(f *fakeMiner, k ckey) float64 {
			if k == (ckey{0, 9}) && f.plan.MHz == 625 && f.plan.MV == 8850 && f.e.confirmed {
				return 0.6
			}
			return 0
		})
	f.temp = func(f *fakeMiner) (float64, bool) { return f.e.holdTarget, true }
	bad := l51guard(t, f, 3)
	runFM(t, e, f)
	dump(t, e)
	if len(*bad) > 0 {
		t.Fatalf("trim rules broken: %v", *bad)
	}
	n := strings.Count(logStr(e), "the trim goes back, same voltage tested again")
	if n > 2 {
		t.Fatalf("lifted trims put back %d times (2 lifted trims, one setting):\n%s", n, tailLog(e, 40))
	}
	var mid preset
	for _, p := range loadPresets() {
		if p.Name == "Middle" {
			mid = p
		}
	}
	if mid.MHz != 625 || mid.PV != 9200 {
		t.Fatalf("Middle %d/%d, want 625 / PV 9200\n%s", mid.MHz, mid.PV, tailLog(e, 40))
	}
	if f.trims[[2]int{3, 3}] != 0 || f.trims[[2]int{3, 33}] != 0 {
		t.Fatalf("trims put back for a setting that failed anyway are still on: %s\n%s", sortedTrims(f.trims), tailLog(e, 40))
	}
}

// A Stop during the preset test that has the lifted trims back: they are lifted again, and the trims kept, the
// restart marker and auto-trim's hand-off are the confirmed setting's.
func TestL51StopDuringRestoredPreset(t *testing.T) {
	l51presets(t)
	e, f := newFM(t, l51cfg())
	f.rules = l51board()
	f.temp = func(f *fakeMiner) (float64, bool) { return f.e.holdTarget, true }
	f.stopWhen = func(f *fakeMiner) bool { return f.e.confirmed && f.plan.MHz == 625 && f.sinceApply() > 8 }
	var marker map[[2]int]float64
	e.onTrims = func(m map[[2]int]float64) { marker = copyTrimMap(m) }
	var adopted []chipTrim
	e.adoptTrims = func(ts []chipTrim, _ string) { adopted = ts }
	runFM(t, e, f)
	dump(t, e)
	if !f.stopFired {
		t.Fatalf("scenario: no stop in the Middle preset\n%s", tailLog(e, 30))
	}
	want := "3.30=-12.5"
	if sortedTrims(f.trims) != want || sortedTrims(e.runTrims) != want || sortedTrims(marker) != want || len(adopted) != 1 {
		t.Fatalf("after a stop: miner %s runTrims %s marker %s adopted %v (want %s)\n%s", sortedTrims(f.trims), sortedTrims(e.runTrims), sortedTrims(marker), adopted, want, tailLog(e, 20))
	}
}
