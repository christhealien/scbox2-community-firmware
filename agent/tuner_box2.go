package main

// The auto-tuner on the SC Box II (lane C8, commissioning test C8). Everything here is inert unless the active
// model writes the planBox2V plan format AND the owner has commissioned the tuner (model.Feat.Tuner, which
// needs ClockControl + VoltageControl first): the API handlers refuse through featGate before any of it runs.
//
// What differs from the SC Lite tuner:
//   - Geometry: 1 hash board (data chain 0) of 36 ICT580 chips in 18 series pairs. The judge is data-driven
//     (it reads every chip icinfo reports), so nothing in it is SC Lite-sized; a chip map that doesn't fit the
//     profile stops the run at the baseline (tunerGeometryErr).
//   - Clock axis: 525–690 MHz (the profile's window; above 690 is refused), on the driver's PLL grid of
//     12.5 MHz x N from 500 MHz up, rounding a request down (the "ICT580 set Clock(req …, actual …)" lines of
//     the owner's log; 690 runs 687.5): the same grid pll() already models. The default map walks the whole
//     window in the chips' own 12.5 MHz steps, from the Low-power stock clock up to the Hashrate one.
//   - Voltage axis: the OEM's voltage LEVEL, 42–61, carried in the plan's "V" field (powerplan.VrunMv); a
//     higher level is a LOWER voltage (C4, rail at the power lugs under load: 42 = 7.67 V … 61 = 7.03 V, about
//     33.5 mV per level). There is no PV and no millivolt control: the tuner never searches, writes or shows
//     millivolts or PV here. (The plan's 4th field is the second voltage-trim PWM, not a fan: the plan writer
//     always keeps the stock plan's value there.) Which way a higher level moves the voltage is model.VCodeDir;
//     while the profile has it unknown the tuner tunes the CLOCK ONLY, at the stock Hashrate level, and no
//     other level is ever written. Once set, it steps levels inside [VCodeMin, VCodeMax] (42–61).
//   - Only the OEM's two stock plans (690 MHz / level 42, 525 MHz / level 61) are known to be safe; every
//     other (clock, level) pair is unproven, which is what the judge and the confirm are for.
//   - Power: the Box II has no power sensor. The tuner ranks by clock (hashrate), the hardware-error judge and the
//     voltage level (less voltage first). With the power estimate on (C2, power_box2.go), each test's row carries
//     the estimate (≈ W, with its chip trims) so the owner can compare hashrate per watt; it never ranks by it (at
//     one clock the lowest clean level IS the best hashrate per watt: W ∝ f·V², TH/s ∝ f).
//   - Chip trims (v0.5.5, "Trim weak chips"): the SC Lite's per-chip pass, through the chip hook (C5). A test that
//     fails on one chip, or on the board total when one chip made a good share of it (box2TrimBlame: the board's
//     errors on this miner are mostly a few chips, C8), slows that chip 12.5 MHz at a time before the level goes
//     up. At most 4 chips by default, at most −50 MHz each, one per pair, never faster than the board. The tuner's
//     trim writes stay in RAM during the run (box2TrimsInRAM): only the run's end (its kept trims, or the trims put
//     back) is saved to the flash.
//   - Presets (v0.5.5, "Build presets"): High / Middle / Low / Lowest power as on the SC Lite, each a clock and a
//     level from the clean curve (inside the clock window and the clocks the curve tested) plus its own fan curve,
//     so on this model building them needs the fan hold (each curve comes from the fan % that held its
//     temperature). Chip trims aren't stored per preset (the SC Lite doesn't either): the run's kept trims are the
//     miner's (Controls → Chip trims) and apply to every clock.
//   - Thermal: abort and cut-off come from the profile (abort default 82, at most 84; the OEM's 86 °C cut-off is
//     never touched); the fan hold's target is 72 °C by default and at most box2HoldMaxC (74 °C; the profile's
//     77 °C is the fan curve's target, not the hold's). The Box II's fan behaviour isn't characterised (the fan
//     hold's controller was tuned on an SC Lite), so the run leaves the fans to the OEM's own control by
//     default; the owner may still turn the hold on.
//   - The end: the confirmed best (a setting that passed its long test), else what the miner ran before if
//     that was one of its stock plans (the OEM's own setting object, PUT back verbatim), else the stock
//     Hashrate plan. Never an unverified setting.
//
// The engine searches voltages as "a higher number = more voltage". On the Box II the engine therefore works
// in its own voltage RANKS (called levels in the code below), a mirror of the OEM's level when a higher level
// means less voltage (identity otherwise, and always while the direction is unknown). Ranks never leave the
// engine: every write, read-back, restart record put-back and the views convert with planMV.hw() /
// box2CodeOf, so the owner only ever sees the OEM's level.
//
// Writes: the tuner never touches the hardware itself. Every setting goes through applyTunePlanGen →
// mutatePlanGen → mutatePlanBox2 (minerd PUT /mcb/setting, as the OEM's web UI does); the fan hold (when the
// owner turns it on) through applyTuneFan → mutatePlanNoWake (the fan duty; the 4th field stays the stock
// trim value). The non-stock clock / level write path inside mutatePlanBox2 belongs to the
// ClockControl/VoltageControl lane (C3/C4).

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"sclite-agent/internal/cfgwin"
)

// box2HoldMaxC: the highest target the tuner's fan hold may aim at on the SC Box II (v0.5.4: the simulations of its
// one board show a hold at 77 °C overshooting to the abort after a room-temperature rise; at 74 °C it stays under it).
const box2HoldMaxC = 74.0

// box2HoldMax: the hold's highest target: box2HoldMaxC, or the profile's target if that is lower.
func box2HoldMax() float64 { return math.Min(model.TargetC, box2HoldMaxC) }

// box2RaiseFanPct: before the tuner raises the clock or the voltage on the SC Box II, the fans go to at least this
// (or stay where they are when higher) in a write of their own (box2RaiseFansFirst).
const box2RaiseFanPct = 70

// box2IsRaise: going from the plan cur to mhz / level code raises the clock or the voltage (in volts: the
// direction of the level is box2Volts' table, the C4 measurement).
func box2IsRaise(cur powerplan, mhz, code int) bool {
	if mhz > cur.FreqMHz {
		return true
	}
	a, _ := box2Volts(cur.VrunMv)
	b, _ := box2Volts(code)
	return cur.VrunMv != code && b > a+1e-9
}

// box2RaiseFansFirst: before a write of mhz / level code that raises the clock or the voltage, the fans to at least
// box2RaiseFanPct, or floor when that is higher (the tuner's hold value), never lower than they run, in a fan-only
// write of their own (no clock change: nothing to confirm). Returns the fan % now set (0: no raise, nothing
// written). The setting unreadable: nothing written (the write after it fails the same way).
func box2RaiseFansFirst(gen int64, mhz, code, floor int) (int, error) {
	if model.Plan != planBox2V {
		return 0, nil
	}
	raw, err := stockGet("/mcb/setting")
	if err != nil {
		return 0, err
	}
	var cur map[string]any
	if err := json.Unmarshal(raw, &cur); err != nil {
		return 0, err
	}
	pp, ok := resolveRunningPlan(cur)
	if !ok || pp.FreqMHz <= 0 || !box2IsRaise(pp, mhz, code) {
		return 0, nil
	}
	want := min(fanMax, max(box2RaiseFanPct, floor))
	if pp.Fan1 >= want {
		return pp.Fan1, nil
	}
	if _, err := mutatePlanGen(gen, func(x *powerplan) { x.Fan1 = max(x.Fan1, want) }); err != nil {
		return 0, err
	}
	return want, nil
}

// vcodeDir: which way a higher voltage level ("V") moves the board's voltage (model.VCodeDir).
type vcodeDir int

const (
	vcodeDirUnknown vcodeDir = 0  // not in the profile: the tuner tunes the clock only, at the stock level
	vcodeUpIsMore   vcodeDir = 1  // a higher level = more voltage (set only from a passed C4 measurement)
	vcodeUpIsLess   vcodeDir = -1 // a higher level = less voltage (the Box II, C4: 42 = 7.67 V, 61 = 7.03 V)
)

// tunerOnBox2: the tuner runs the Box II rules (voltage levels, no PV, no mV).
func tunerOnBox2() bool { return model.Plan == planBox2V }

// box2LevelVolts: the rail at the power lugs under load, per voltage level, as measured on the owner's unit
// (commissioning test C4). Only measured levels are listed; box2Volts estimates the others.
var box2LevelVolts = map[int]float64{42: 7.67, 43: 7.61, 44: 7.58, 45: 7.55, 46: 7.52, 61: 7.03}

// box2Volts: one level's voltage: the measured figure, else the estimate from the line through the C4 readings
// (7.66 V at level 42, about 33.5 mV less per level), the same formula as the page's boxVoltsTxt, so the two
// always agree. (The explicit float64 conversion stops the compiler fusing the multiply-add on ARM, which could
// round differently from the page's JavaScript.)
func box2Volts(level int) (v float64, measured bool) {
	if v, ok := box2LevelVolts[level]; ok {
		return v, true
	}
	return 7.66 - float64(0.0335*float64(level-42)), false
}

// box2LevelText: one OEM voltage level as the owner reads it, in volts only: "7.67 V", or "≈ 7.39 V" for a
// level whose volts are estimated (the page's boxVoltsTxt gives the same text). The level itself never shows.
func box2LevelText(level int) string {
	v, ok := box2Volts(level)
	if ok {
		return fmt.Sprintf("%.2f V", v)
	}
	return fmt.Sprintf("≈ %.2f V", v)
}

// box2VoltRangeText: the allowed levels lo–hi as the owner reads them, lowest voltage first: "7.03 V and 7.67 V".
func box2VoltRangeText(lo, hi int) string {
	a, _ := box2Volts(lo)
	b, _ := box2Volts(hi)
	if a > b {
		lo, hi = hi, lo
	}
	return box2LevelText(lo) + " and " + box2LevelText(hi)
}

// box2VoltStepText: a step of n levels in volts ("≈ 0.03 V"), for the tuner's texts.
func box2VoltStepText(n int) string {
	return fmt.Sprintf("≈ %.2f V", float64(0.0335*float64(n)))
}

// ---- chip trims in the tuner (v0.5.5) ----

// trimBlame: the chip a failed test of cand blames, for the per-chip pass (trimAndRetest). The SC Lite: the chip that
// went over its own limit, as before. The SC Box II also: a test that failed on the board's total when one chip made a
// good share of it (box2TrimBlame); and on the Box II only when trims could make the test pass at all
// (box2TrimsCanFix): a fail with errors all over the board is the board's voltage, and trying up to 4 chips × 4 depths
// there would cost hours of retests per level (C8: every level from 7.03 to 7.36 V failed within a minute with 9–90
// errors spread over the board).
func (e *tunerEngine) trimBlame(cand planMV, st tunerStats, why string) (ckey, bool) {
	if !tunerOnBox2() {
		return st.FailChip, st.FailByChip
	}
	k, ok := st.FailChip, st.FailByChip
	if !ok {
		k, ok = box2TrimBlame(st, why, e.weak)
	}
	if !ok {
		return ckey{}, false
	}
	if rest, n, can := e.box2TrimsCanFix(cand, st, k.B); !can {
		chips, errs := "chips", "errors"
		if n == 1 {
			chips = "chip"
		}
		if rest == 1 {
			errs = "error"
		}
		e.log("%s: no chip trim: the %d %s outside the %d worst made %d %s, more than trims could fix: more voltage instead", cand.text(), n, chips, e.cfg.ChipTrimMax, rest, errs)
		return ckey{}, false
	}
	return k, true
}

// box2TrimsCanFix: could slowing at most ChipTrimMax chips of board b make this test of cand pass? Only if the board's
// chips that trims can't reach stay within the board's limit for the whole test and each within its own. Which chips
// trims can reach follows trimOne's own rules (v0.5.5 T6): the worst chips first, each one trimOne would slow (not
// trimmed by hand, not at the deepest trim already, its stage partner not slowed or picked, a slot left on the board
// counting the run's trims and those just made), up to the slots left. rest: the errors the other chips made, n: how
// many of them made any.
func (e *tunerEngine) box2TrimsCanFix(cand planMV, st tunerStats, b int) (rest, n int, ok bool) {
	type kv struct {
		k ckey
		g int
	}
	var xs []kv
	for k, g := range st.Gained {
		if k.B != b {
			continue
		}
		if _, w := e.weak[k]; w {
			continue // (the board's total is judged on its normal chips; a weak chip is judged on its own)
		}
		xs = append(xs, kv{k, g})
	}
	sort.Slice(xs, func(i, j int) bool { return xs[i].g > xs[j].g || xs[i].g == xs[j].g && xs[i].k.I < xs[j].k.I })
	var trims map[[2]int]float64
	if e.trimGet != nil {
		trims = e.trimGet()
	}
	var made []madeTrim
	if e.curMade != nil {
		made = *e.curMade
	}
	picked := map[[2]int]bool{}
	over := false
	for _, x := range xs {
		if x.g > 0 && e.cfg.ChipTrimMax > 0 {
			if key, can := e.box2Trimmable(x.k, trims, made, picked); can {
				picked[key] = true
				continue
			}
		}
		rest += x.g
		if x.g > 0 {
			n++
		}
		if lim, have := st.Limits[x.k]; have && float64(x.g) > lim {
			over = true
		}
	}
	return rest, n, !over && float64(rest) <= e.box2BoardLimit(b, cand.MHz, e.cfg.HoldMin)
}

// box2Trimmable: would trimOne slow chip k now, with the chips in picked slowed too (the same rules, nothing set)?
func (e *tunerEngine) box2Trimmable(k ckey, trims map[[2]int]float64, made []madeTrim, picked map[[2]int]bool) ([2]int, bool) {
	if e.hookStats == nil {
		return [2]int{}, false
	}
	if _, ok := e.hookStats(); !ok {
		return [2]int{}, false
	}
	key, ok := e.hookChip(k)
	if !ok {
		return [2]int{}, false
	}
	chain, chip := key[0], key[1]
	e.mu.Lock()
	run := copyTrimMap(e.runTrims)
	e.mu.Unlock()
	cur := trims[key]
	_, mine := run[key]
	for _, m := range made {
		if m.key == key {
			mine = true
		}
	}
	if cur != 0 && !mine {
		return key, false // trimmed by hand
	}
	if cur-trimStep < e.trimFloor()-1e-9 {
		return key, false // at the deepest trim already
	}
	if mine {
		return key, true // (deeper on a chip the run trimmed: no new slot)
	}
	if pc := chipPartner(chip); pc > 0 {
		p := [2]int{chain, pc}
		if trims[p] < 0 || picked[p] {
			return key, false // one trim per pair
		}
		for _, m := range made {
			if m.key == p {
				return key, false
			}
		}
	}
	used := map[[2]int]bool{}
	for kk := range run {
		if kk[0] == chain {
			used[kk] = true
		}
	}
	for _, m := range made {
		if m.key[0] == chain {
			used[m.key] = true
		}
	}
	for kk := range picked {
		if kk[0] == chain {
			used[kk] = true
		}
	}
	return key, len(used) < e.cfg.ChipTrimMax
}

// box2BoardLimit: the board's error limit for a whole test of holdMin at mhz (watch's boardLimits, the same formula).
func (e *tunerEngine) box2BoardLimit(b, mhz int, holdMin float64) float64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	scale := 1.0
	if e.baseMHz > 0 {
		scale = math.Max(0.5, float64(mhz)/float64(e.baseMHz))
	}
	boards := map[int]bool{}
	base := 0.0
	for k, r := range e.baseRate {
		boards[k.B] = true
		if _, w := e.weak[k]; !w && k.B == b {
			base += r
		}
	}
	h := holdMin / 60
	return tunerLimitVal(base, h, scale, false, e.cfg.WeakFactor, e.margin(h), max(1, len(boards)), e.cfg.Sigma)
}

// box2TrimBlame: a test that failed on the board's total ("board 0 +5 errors (limit 4.0)") because of one or a few
// chips: the chip with the most new errors, when it made at least 2 and at least a third of the board's (normal
// chips') errors. On the owner's unit (C8, 675 MHz) the board's fails between 7.43 and 7.55 V were 4–5 errors with 2–3
// of them on b0.c26 / c23 / c25 / c17: slowing that chip is the per-chip answer, as for a chip over its own limit. A
// board fail spread over many chips (one error each) blames none: that is the board's voltage, not a weak chip. Ties go
// to the lower chip number (the same pick every time).
func box2TrimBlame(st tunerStats, why string, weak map[ckey]float64) (ckey, bool) {
	if !strings.HasPrefix(why, "board ") || !strings.Contains(why, "errors (limit") || len(st.Gained) == 0 {
		return ckey{}, false
	}
	var b int
	if _, err := fmt.Sscanf(why, "board %d", &b); err != nil {
		return ckey{}, false
	}
	total, top, have := 0, ckey{}, false
	for k, g := range st.Gained {
		if k.B != b {
			continue
		}
		if _, w := weak[k]; w {
			continue // (the board's total is judged on its normal chips)
		}
		total += g
		if g > 0 && (!have || g > st.Gained[top] || (g == st.Gained[top] && k.I < top.I)) {
			top, have = k, true
		}
	}
	if !have || st.Gained[top] < 2 || 3*st.Gained[top] < total {
		return ckey{}, false
	}
	return top, true
}

// box2TrimRAMOwner: the tuning run whose chip-trim writes stay in RAM (not saved to the flash) while it runs (SC Box II,
// cfgShadow): a trim the tuner tries is a test setting like its clocks, and only the run's end (the trims it keeps, or
// the trims put back as they were) is saved. A power cut mid-run then boots on the trims saved before the run (and the
// restart marker's record puts them back too, tunerRecover). nil: none. Not for a dead (watchdog-ended) run.
var box2TrimRAMOwner atomic.Pointer[tunerEngine]

// box2TrimsInRAM: is the chip trims file a test setting now (cfgHot)?
func box2TrimsInRAM() bool {
	e := box2TrimRAMOwner.Load()
	return e != nil && !e.dead.Load()
}

// box2TrimsRAMBegin: the run's trim writes stay in RAM from now; the func returned ends that and saves the trims file
// as the run left it (once). SC Lite, a dry run or a run without "Trim weak chips": nothing to do.
func (e *tunerEngine) box2TrimsRAMBegin() func() {
	if !tunerOnBox2() || e.cfg.DryRun || !e.cfg.ChipTrim || e.cfg.Mode != "map" {
		return func() {}
	}
	box2TrimRAMOwner.Store(e)
	return func() {
		if !box2TrimRAMOwner.CompareAndSwap(e, nil) {
			return // (another run owns the file now: not this run's to save)
		}
		if cfgShadow() {
			chipTrimMu.Lock()
			err := cfgPersist(chipTrimPath()) // (the end's trims: kept ones, or as before the run)
			chipTrimMu.Unlock()
			if err != nil {
				e.log("WARNING: the chip trims the run ended with couldn't be saved for the next boot (%s)", fsErr(err))
			}
		}
	}
}

// box2RunTrimLoss: the MHz the run's own trims take off the board now (the trims on the chips minus those it had at the
// baseline, slower only), for the hashrate the setting should give.
func (e *tunerEngine) box2RunTrimLoss() float64 {
	if !tunerOnBox2() || e.trimGet == nil || !e.cfg.ChipTrim {
		return 0
	}
	cur := e.trimGet()
	e.mu.Lock()
	base := e.baseTrims
	e.mu.Unlock()
	// (v0.5.5 T7) bounded by what the run may do: per board its ChipTrimMax largest, each at most the deepest trim
	// (a trims file left over-full by a failed put-back can't make the run expect a much lower hashrate)
	per := map[int][]float64{}
	for k, v := range cur {
		if d := base[k] - v; d > 0 {
			per[k[0]] = append(per[k[0]], math.Min(d, -e.trimFloor()))
		}
	}
	loss := 0.0
	for _, ds := range per {
		sort.Sort(sort.Reverse(sort.Float64Slice(ds)))
		for i, d := range ds {
			if i >= max(1, e.cfg.ChipTrimMax) {
				break
			}
			loss += d
		}
	}
	return loss
}

// box2TestTrims: for a finished test's row and chip map: every chip slowed or raised on the chips during it ([board,
// chip, MHz] in the miner's own numbering, as the map's chips: the hook's (chain, chip) matched by errToHook), and how
// many of them the run itself slowed and by how much in all (box2RunTrimLoss). Nothing without the trims file or the
// hook's report.
func (e *tunerEngine) box2TestTrims(st tunerStats) (cells [][]float64, n int, lossMHz float64) {
	if e.trimGet == nil {
		return nil, 0, 0
	}
	cur := e.trimGet()
	if len(cur) == 0 {
		return nil, 0, 0
	}
	e.mu.Lock()
	base := e.baseTrims
	e.mu.Unlock()
	if e.cfg.ChipTrim {
		for k, v := range cur {
			if d := base[k] - v; d > 0 {
				n++
				lossMHz += d
			}
		}
	}
	return e.box2TrimCells(cur, st.Gained), n, lossMHz
}

// box2TrimCells: the trims cur (the hook's chain, chip) as [board, chip, MHz] of the miner's chips keys (errToHook).
func (e *tunerEngine) box2TrimCells(cur map[[2]int]float64, keys map[ckey]int) (cells [][]float64) {
	if e.hookStats == nil || len(keys) == 0 || len(cur) == 0 {
		return nil
	}
	hs, ok := e.hookStats()
	if !ok {
		return nil
	}
	minIdx := minIndexes(keys)
	for k := range keys {
		if chain, chip, ok := errToHook(k, minIdx, hs); ok {
			if v := cur[[2]int{chain, chip}]; v != 0 {
				cells = append(cells, []float64{float64(k.B), float64(k.I), v})
			}
		}
	}
	sort.Slice(cells, func(i, j int) bool {
		return cells[i][0] < cells[j][0] || cells[i][0] == cells[j][0] && cells[i][1] < cells[j][1]
	})
	return cells
}

// box2LiveTrims: at the start of a test (settle), the chips slowed now, for the live chip map (statusView).
func (e *tunerEngine) box2LiveTrims() {
	if !tunerOnBox2() || e.trimGet == nil || e.cfg.DryRun {
		return
	}
	e.mu.Lock()
	keys := make(map[ckey]int, len(e.baseRate))
	for k := range e.baseRate {
		keys[k] = 0
	}
	e.mu.Unlock()
	cells := e.box2TrimCells(e.trimGet(), keys)
	e.mu.Lock()
	e.liveTrims = cells
	e.mu.Unlock()
}

// ---- presets (v0.5.5) ----

// box2PresetTries: the SC Box II keeps each preset's settings to clocks inside the clock window and inside the range
// the curve ran clean (its slowest clean clock lo up to the preset phase's top, the confirmed best or the baseline):
// the SC Lite's "one clock step less" for the Lowest power preset would otherwise go below the slowest clock tested, or
// below 525 MHz (the writer refuses those, which would end the presets phase). A preset left with no setting is
// skipped. The levels are the engine's ranks, clamped to the run's range already (mk).
func box2PresetTries(tries map[string][]planMV, lo, hi int) {
	wLo, wHi := max(freqMin, lo), min(clockWindowHi(), hi)
	for name, ps := range tries {
		var keep []planMV
		for _, p := range ps {
			if p.MHz >= wLo && p.MHz <= wHi {
				keep = append(keep, p)
			}
		}
		tries[name] = keep
	}
}

// box2TrimsOffBeforePresets: SC Box II, map run: nothing was confirmed, so the run's chip trims come off at its end;
// they come off before the presets instead, so each preset is tested on the chips as the miner will run it (with the
// trims on, a preset could pass only because of trims that are gone after the run). With a confirmed setting the
// trims stay, for the presets and after the run (Controls → Chip trims), as on the SC Lite.
func (e *tunerEngine) box2TrimsOffBeforePresets() {
	if !tunerOnBox2() || e.cfg.DryRun {
		return
	}
	e.mu.Lock()
	confirmed, n := e.confirmed && e.best != nil, len(e.runTrims)
	e.mu.Unlock()
	if confirmed || n == 0 {
		return
	}
	e.log("nothing was confirmed, so the run's chip trims come off now: the presets are tested without them, as the miner will run them")
	e.undoRunTrims()
}

// box2TrimmedWant: the hashrate (GH/s) a setting should give with the run's trims: the board's work follows the sum
// of its chips' clocks, so a chip slowed 25 MHz at 675 MHz takes 25 / (36 × 675) = 0.1 % off. want is the untrimmed
// figure (the baseline scaled by the clock), chips the board's chip count.
func box2TrimmedWant(want, mhz, lossMHz float64, chips int) float64 {
	if lossMHz <= 0 || chips <= 0 || mhz <= 0 {
		return want
	}
	return want * math.Max(0, 1-lossMHz/(float64(chips)*mhz))
}

// box2TrimmedWatts: the power estimate (power_box2.go) for a setting with chips slowed by lossMHz in total: the chips'
// draw follows each chip's clock at the board's voltage (W = k × f × V² per chip), so the trims take the same share off
// the watts as off the hashrate, and the hashrate per watt of a setting is its voltage's, trims or not. ok=false: no
// estimate (the power estimate isn't commissioned, or outside its window).
func box2TrimmedWatts(mhz, level int, lossMHz float64, chips int) (float64, bool) {
	if featOff(model.Feat.PowerEstimate) {
		return 0, false
	}
	w, _, _, ok := box2EstimateWatts(mhz, level)
	if !ok {
		return 0, false
	}
	if f := box2PLLClock(float64(mhz)); lossMHz > 0 && chips > 0 && f > 0 {
		w *= math.Max(0, 1-lossMHz/(float64(chips)*f))
	}
	return math.Round(w*10) / 10, true
}

// box2VoltKnown: the profile records which way the level moves the voltage (the C4 result).
func box2VoltKnown() bool { return model.VCodeDir == vcodeUpIsMore || model.VCodeDir == vcodeUpIsLess }

// box2LowPowerTuple: the lowest-clock stock plan (525 MHz / level 61): where the default map starts.
func box2LowPowerTuple() box2Tuple {
	var lo box2Tuple
	for _, t := range model.Box2Plans {
		if lo.FreqMHz == 0 || t.FreqMHz < lo.FreqMHz {
			lo = t
		}
	}
	return lo
}

// box2HashrateTuple: the highest-clock stock plan (690 MHz / level 42): the tuner's baseline, its clock-only
// level and its last-resort end.
func box2HashrateTuple() box2Tuple {
	var hi box2Tuple
	for _, t := range model.Box2Plans {
		if t.FreqMHz > hi.FreqMHz {
			hi = t
		}
	}
	return hi
}

// box2LevelOf / box2CodeOf: the OEM's level ("code") <-> the engine's voltage rank (an involution: the same
// formula both ways).
func box2LevelOf(code int) int {
	if model.VCodeDir == vcodeUpIsLess {
		return model.VCodeMin + model.VCodeMax - code
	}
	return code
}

func box2CodeOf(level int) int { return box2LevelOf(level) }

// hw: the plan as the miner is given it. SC Lite: unchanged. Box II: the OEM's level, no PV.
func (p planMV) hw() planMV {
	if !tunerOnBox2() {
		return p
	}
	return planMV{MHz: p.MHz, MV: box2CodeOf(p.MV)}
}

func (p *planMV) hwPtr() *planMV {
	if p == nil {
		return nil
	}
	h := p.hw()
	return &h
}

func hwList(c []planMV) []planMV {
	if !tunerOnBox2() || c == nil {
		return c
	}
	out := make([]planMV, len(c))
	for i, p := range c {
		out[i] = p.hw()
	}
	return out
}

// vText: one voltage as the owner reads it: "PV 9400" (SC Lite, unchanged) or "7.67 V" (Box II, volts only).
func (c tunerConfig) vText(mv int) string {
	if tunerOnBox2() {
		return box2LevelText(box2CodeOf(mv))
	}
	return fmt.Sprintf("PV %d", c.pvFor(mv))
}

// box2TunerDefaults: the Box II starting form. The OEM's levels, not the engine's ranks (the engine converts
// at its start). Clock axis: the whole window (Low-power stock clock up to the Hashrate one) in the chips' own
// clock steps; voltage axis: every allowed level once the profile knows the level's direction. Nothing here is
// an SC Lite figure; the test lengths, sensitivity and judge settings stay the generic defaults (no Box II
// value is known for them).
func box2TunerDefaults(c *tunerConfig) {
	hi, lo := box2HashrateTuple(), box2LowPowerTuple()
	c.BaselineSrc, c.BaselineMHz, c.BaselineMV = "stock", hi.FreqMHz, hi.V
	c.LowestMHz, c.HighestMHz, c.Step = lo.FreqMHz, hi.FreqMHz, model.ClockStep
	c.MinMV, c.MaxMV, c.MVStep = hi.V, hi.V, 1
	if box2VoltKnown() {
		c.MinMV, c.MaxMV = model.VCodeMin, model.VCodeMax
	}
	c.TopDown, c.ChipTrim, c.BuildPresets = false, false, false
	// "Trim weak chips" (off by default, as on the SC Lite): at most 4 of the 36 chips, each at most 50 MHz slower
	c.ChipTrimMax, c.ChipTrimDeep = box2TrimMaxDef, box2TrimDeepDef
	c.PVGap = 0
	c.AbortC = model.AbortDefC
	// the fan hold's controller (and its 30 % floor) was tuned on an SC Lite; the Box II's fans aren't
	// characterised, so by default the run leaves them to the OEM's own temperature control
	c.FanHold = false
	// the hold's target: a little above where stock runs (about 68 °C on the first unit), well under the 82 °C
	// abort, so a test never sits near the guard
	c.FanTargetC = math.Min(box2HoldMax(), 72)
	c.WarmestC = math.Min(box2HoldMax(), c.AbortC-box2PresetRoomC)
	c.ConfirmMHz, c.ConfirmMV = hi.FreqMHz, hi.V
}

// The Box II's "Trim weak chips" defaults: at most this many chips (of 36, one per pair), each at most this far slower.
const (
	box2TrimMaxDef  = 4
	box2TrimMaxMax  = 4 // (v0.5.5 T7: the most chips per board the tuner may trim on this model)
	box2TrimDeepDef = 50.0
)

// box2PresetRoomC: on the SC Box II the warmest preset is held at least this far under the abort temperature (the SC
// Lite keeps 12 °C). The hold itself is capped at box2HoldMaxC (74 °C) and the abort is 82 °C by default: the v0.5.4
// fan-hold simulations of the one board (scratchpad sim scen054*.py) stay under 82 °C from a 74 °C hold through a
// 10 °C room rise and a 690 MHz jump, so 8 °C is the room the hold was proven with.
const box2PresetRoomC = 8.0

// presetRoomC: how far under the abort the warmest preset is held (SC Lite: 12 °C, unchanged).
func presetRoomC() float64 {
	if tunerOnBox2() {
		return box2PresetRoomC
	}
	return 12
}

// box2TunerNormalize: what a Box II start request may and may not ask for, before validation. The baseline is
// always the stock Hashrate plan; with the voltage direction unknown every voltage field is the stock level (the
// panel's form can't know that, so it is set here and said in the note, not refused).
func box2TunerNormalize(c tunerConfig) (tunerConfig, string) {
	hi := box2HashrateTuple()
	c.BaselineSrc, c.BaselineMHz, c.BaselineMV = "stock", hi.FreqMHz, hi.V
	c.PVGap = 0
	note := ""
	if !box2VoltKnown() {
		if c.MinMV != hi.V || c.MaxMV != hi.V || (c.Mode == "confirm" && c.ConfirmMV != hi.V) {
			note = fmt.Sprintf("which way the voltage setting moves the voltage isn't in this model's profile yet (commissioning test C4): clock only, at the stock %s", box2LevelText(hi.V))
		}
		c.MinMV, c.MaxMV, c.MVStep, c.ConfirmMV = hi.V, hi.V, 1, hi.V
		c.TopDown = false
	}
	return c, note
}

// validateTunerConfigBox2: validateTunerConfig for planBox2V. The config is in the OEM's LEVELS here.
func validateTunerConfigBox2(c tunerConfig) error {
	switch c.Mode {
	case "map", "confirm", "rebuild":
	default:
		return fmt.Errorf("mode must be map, confirm or rebuild")
	}
	// "Trim weak chips" (map only: the engine trims in the curve's tests): through the chip hook, so chip trims must be
	// commissioned (a dry run writes nothing, as on the SC Lite)
	if c.ChipTrim && c.Mode == "map" {
		if !c.DryRun && featOff(model.Feat.ChipTrims) {
			return fmt.Errorf("chip trims are not available on this model (%s): untick “Trim weak chips”", model.Name)
		}
		if c.ChipTrimMax < 1 || c.ChipTrimMax > box2TrimMaxMax {
			return fmt.Errorf("chips the tuner may trim per board must be 1–%d on this model", box2TrimMaxMax)
		}
		if d := c.ChipTrimDeep; d != 0 && (d < 25 || d > 75 || !onPLLGrid(d)) {
			return fmt.Errorf("how far the tuner may slow a chip must be 25–75 MHz, in 12.5 MHz steps")
		}
	}
	// "Build presets" / rebuild: each preset is saved with its own fan curve, made from the fan % that held its
	// temperature, so on this model the fans must be held (section Fans); the warmest preset stays under the hold's
	// cap and box2PresetRoomC under the abort
	if c.BuildPresets || c.Mode == "rebuild" {
		if !c.FanHold {
			return fmt.Errorf("building presets needs “Hold a steady temperature” in the fan section on this model: each preset’s fan curve is made from the fan speed that held its temperature")
		}
		if c.PresetMin <= 0 || c.PresetMin > 240 {
			return fmt.Errorf("minutes per preset must be 1–240")
		}
		if c.WarmestC < c.FanTargetC || c.WarmestC > box2HoldMax() {
			return fmt.Errorf("the warmest preset must be between the fan target (%.0f °C) and %.0f °C on this model", c.FanTargetC, box2HoldMax())
		}
		if c.WarmestC > c.AbortC-box2PresetRoomC {
			return fmt.Errorf("the warmest preset must be at least %.0f °C below the abort temperature (%.0f °C or less), so a preset holding it has room before the safety stop", box2PresetRoomC, c.AbortC-box2PresetRoomC)
		}
	}
	hi := box2HashrateTuple()
	if c.BaselineSrc != "stock" || c.BaselineMHz != hi.FreqMHz || c.BaselineMV != hi.V {
		return fmt.Errorf("the baseline on this model is its Stock High plan (%d MHz, %s)", hi.FreqMHz, box2LevelText(hi.V))
	}
	fLo, fHi := freqMin, clockWindowHi()
	if fLo < 500 { // the PLL grid below 500 MHz is another one (other dividers): not measured for the tuner
		fLo = 500
	}
	step := model.ClockStep
	if step <= 0 {
		return fmt.Errorf("this model's clock steps aren't known: the tuner can't run")
	}
	codeOK := func(v int) bool { return v >= model.VCodeMin && v <= model.VCodeMax }
	if c.Mode == "map" {
		if c.LowestMHz < fLo || c.LowestMHz > fHi {
			return fmt.Errorf("lowest clock must be %d–%d MHz", fLo, fHi)
		}
		if c.HighestMHz < c.LowestMHz || c.HighestMHz > fHi {
			return fmt.Errorf("highest clock must be between the lowest and %d MHz", fHi)
		}
		cl := c.clockList()
		if c.Step >= step && c.Step <= 100 && onPLLGrid(c.Step) && (len(cl) == 0 || cl[0] < fLo) {
			return fmt.Errorf("no clock to test between %d and %d MHz with %g MHz steps (the first must be %d MHz or more)", c.LowestMHz, c.HighestMHz, c.Step, fLo)
		}
	}
	// the clock and voltage steps a rebuild's presets move by ("one voltage step more", "one clock step less") are
	// the form's, as in a map: the same rules (a zero or off-grid step would build presets off the chips' grid)
	if c.Mode == "map" || c.Mode == "rebuild" {
		if c.Step < step || c.Step > 100 || !onPLLGrid(c.Step) {
			return fmt.Errorf("clock step must be a multiple of %g MHz (the chips' clock steps), %g–100", step, step)
		}
		if !codeOK(c.MinMV) || !codeOK(c.MaxMV) || c.MinMV > c.MaxMV {
			return fmt.Errorf("the voltages must be between %s, with the lowest voltage at or below the highest", box2VoltRangeText(model.VCodeMin, model.VCodeMax))
		}
		if c.MVStep < 1 || c.MVStep > model.VCodeMax-model.VCodeMin+1 {
			return fmt.Errorf("the voltage step must be %s–%s", box2VoltStepText(1), box2VoltStepText(model.VCodeMax-model.VCodeMin+1))
		}
		if !box2VoltKnown() && (c.MinMV != hi.V || c.MaxMV != hi.V) {
			return fmt.Errorf("which way the voltage setting moves the voltage isn't in this model's profile yet (C4): the tuner runs the clock only, at %s", box2LevelText(hi.V))
		}
	}
	if c.Mode == "confirm" {
		if c.ConfirmMHz < fLo || c.ConfirmMHz > fHi {
			return fmt.Errorf("confirm clock must be %d–%d MHz", fLo, fHi)
		}
		if !codeOK(c.ConfirmMV) {
			return fmt.Errorf("the confirm voltage must be between %s", box2VoltRangeText(model.VCodeMin, model.VCodeMax))
		}
		if !box2VoltKnown() && c.ConfirmMV != hi.V {
			return fmt.Errorf("which way the voltage setting moves the voltage isn't in this model's profile yet (C4): confirm at %s", box2LevelText(hi.V))
		}
		if c.ConfirmHours < 0.5 || c.ConfirmHours > 48 {
			return fmt.Errorf("hours to run the confirm must be 0.5–48")
		}
	}
	if c.BaselineMin <= 0 || c.HoldMin <= 0 || c.SettleMin < 0 {
		return fmt.Errorf("baseline and test minutes must be positive")
	}
	if c.Sigma < 1.5 || c.Sigma > 4 {
		return fmt.Errorf("sensitivity must be 1.5–4 (2.5 is the default; higher values stop failing anything)")
	}
	if c.AbortC < 70 || c.AbortC > model.AbortMaxC || c.AbortC >= model.TempCutoffC {
		return fmt.Errorf("abort temperature must be 70–%.0f °C (%s own over-temperature control takes over at %.0f °C)", model.AbortMaxC, oemPoss(), model.TempCutoffC)
	}
	if c.PollS < 1 || c.PollS > 60 {
		return fmt.Errorf("poll must be 1–60 s")
	}
	if c.Mode == "map" && c.ConfirmWin && (c.ConfirmX < 1 || c.ConfirmX > 20) {
		return fmt.Errorf("the confirm must be at least as long as one test (%g min) and at most 20 times it", c.HoldMin)
	}
	if c.FanHold {
		if c.FanTargetC < 45 || c.FanTargetC > box2HoldMax() {
			return fmt.Errorf("fan target must be 45–%.0f °C on this model", box2HoldMax())
		}
		if c.FanTargetC > c.AbortC-5 {
			return fmt.Errorf("fan target must be at least 5 °C below the abort temperature")
		}
		if c.FanMinPct < fanMin || c.FanMinPct > 90 {
			return fmt.Errorf("lowest fan must be %d–90 %%", fanMin)
		}
	}
	return nil
}

// box2TunerLevels: the engine's config: voltages as ranks (a no-op unless a higher level means less voltage),
// and, whatever it was given, the clock only at the stock level while the direction is unknown.
func box2TunerLevels(c tunerConfig) tunerConfig {
	if !box2VoltKnown() {
		v := box2HashrateTuple().V
		c.MinMV, c.MaxMV, c.ConfirmMV, c.BaselineMV, c.MVStep = v, v, v, v, 1
	}
	lo, hi := box2LevelOf(c.MinMV), box2LevelOf(c.MaxMV)
	if lo > hi {
		lo, hi = hi, lo
	}
	c.MinMV, c.MaxMV = lo, hi
	c.BaselineMV, c.ConfirmMV = box2LevelOf(c.BaselineMV), box2LevelOf(c.ConfirmMV)
	c.PVGap = 0
	return c
}

// hwView: the config as the owner gave it (the OEM's levels), for the views. SC Lite: unchanged.
func (c tunerConfig) hwView() tunerConfig {
	if !tunerOnBox2() {
		return c
	}
	lo, hi := box2CodeOf(c.MinMV), box2CodeOf(c.MaxMV)
	if lo > hi {
		lo, hi = hi, lo
	}
	c.MinMV, c.MaxMV = lo, hi
	c.BaselineMV, c.ConfirmMV = box2CodeOf(c.BaselineMV), box2CodeOf(c.ConfirmMV)
	return c
}

// box2TunerWriteErr: the last check before a tuner setting goes to the miner (the OEM's levels): a clock
// inside the window, a level inside the allowed interval, and only the stock level while the direction is
// unknown.
func box2TunerWriteErr(h planMV) error {
	if h.MV < model.VCodeMin || h.MV > model.VCodeMax {
		return fmt.Errorf("refused: %s is outside the allowed voltages (between %s)", box2LevelText(h.MV), box2VoltRangeText(model.VCodeMin, model.VCodeMax))
	}
	if hi := box2HashrateTuple(); !box2VoltKnown() && h.MV != hi.V && !box2StockMatch(powerplan{FreqMHz: h.MHz, VrunMv: h.MV}) {
		return fmt.Errorf("refused: %s: which way the voltage setting moves the voltage isn't in this model's profile yet, only %s", box2LevelText(h.MV), box2LevelText(hi.V))
	}
	if h.MHz < freqMin || h.MHz > clockWindowHi() {
		return fmt.Errorf("refused: %d MHz is outside %d–%d MHz", h.MHz, freqMin, clockWindowHi())
	}
	return nil
}

// box2TunerApply: the Box II engine's applyPlan: rank -> the OEM's level, checked, then the panel's one plan
// writer.
func box2TunerApply(gen int64, p planMV) error { return box2TunerApplyCoop(gen, p, nil) }

// box2TunerApplyCoop: box2TunerApply whose clock/V confirmation does co's work while it waits (box2Coop: the
// tuner's temperature check, fan hold and Stop).
func box2TunerApplyCoop(gen int64, p planMV, co *box2Coop) error {
	h := p.hw()
	if err := box2TunerWriteErr(h); err != nil {
		return err
	}
	_, err := mutatePlanGenCoop(gen, func(pp *powerplan) { pp.FreqMHz = h.MHz; pp.VrunMv = h.MV; pp.PVMv = h.PV }, co)
	return err
}

// box2TunerBaseline: the run's baseline, as a level: the stock Hashrate plan.
func box2TunerBaseline() planMV {
	hi := box2HashrateTuple()
	l := box2LevelOf(hi.V)
	return planMV{MHz: hi.FreqMHz, MV: l, PV: l}
}

// box2TunerBefore: what the miner runs before the run, and how to put it back at the end. Only one of its
// stock plans is put back (the OEM's own setting object, verbatim, the way its web UI writes it, then the
// fans raised if it's hot or they sit under the floor); anything else (a clock the owner set through clock
// control, an unreadable setting) gives no restorer, so the run ends on the stock Hashrate baseline instead.
func box2TunerBefore() (func() error, string) {
	raw, err := stockGet("/mcb/setting")
	if err != nil {
		return nil, ""
	}
	var cur map[string]any
	if json.Unmarshal(raw, &cur) != nil {
		return nil, ""
	}
	pp, ok := resolveRunningPlan(cur)
	if !ok || pp.FreqMHz <= 0 || !box2StockMatch(pp) {
		return nil, ""
	}
	what := fmt.Sprintf("its stock %s plan, %d MHz · %s", box2ModeName(pp.FreqMHz), pp.FreqMHz, box2LevelText(pp.VrunMv))
	gen := safetyGen.Load()
	return func() error {
		if miningHoldErr() != nil || genMoved(gen) {
			return nil // the fan lock, the pool guard, the thermal guard or the owner put it on Idle meanwhile
		}
		hold := -1
		if heatGuardActive() {
			hold = liveFanMax()
		}
		// going back up (a test at a lower clock, back to Stock High): the fans first (box2RaiseFansFirst), and the
		// setting put back carries those fans too while the clock ramps (the owner's own fans in it would undo the
		// raise with the clock raise); after the ramp the owner's setting goes back verbatim, its own fans in it,
		// and the fan curve takes over after the run as before
		body := raw
		raised, ferr := box2RaiseFansFirst(gen, pp.FreqMHz, pp.VrunMv, 0)
		if ferr == nil && raised > pp.Fan1 {
			if b, ok := box2SettingWithFan(raw, raised); ok {
				body = b
			} else {
				raised = 0
			}
		} else {
			raised = 0
		}
		err := putSettingGen(gen, body)
		if miningHoldErr() != nil || genMoved(gen) {
			return nil
		}
		if err == nil && raised > 0 {
			time.Sleep(box2ConfirmQuiet) // (the driver ramps the clock meanwhile)
			if miningHoldErr() != nil || genMoved(gen) {
				return nil
			}
			if box2StillAt(pp.FreqMHz, pp.VrunMv, raised) && !heatGuardActive() {
				err = putSettingGen(gen, raw)
			} // (else the fan control or a guard changed the fans meanwhile: they stay as it set them)
			if miningHoldErr() != nil || genMoved(gen) {
				return nil
			}
		}
		// (only Fan1 is a fan duty: the plan's 4th field (Fan2) is the second voltage-trim PWM, which the plan
		// writer always keeps at the stock plan's value)
		if err == nil && (pp.Fan1 < fanFloor() || heatGuardActive() || hold > 0) {
			if _, err = mutatePlanGen(gen, func(x *powerplan) { x.Fan1 = max(x.Fan1, hold) }); err == errSafetyStop {
				err = nil
			}
		}
		if err == nil {
			box2PersistPlan(pp.FreqMHz, pp.VrunMv) // the owner's own plan, back: what the next boot starts with too
		}
		return err
	}, what
}

// box2SettingWithFan: the OEM's setting raw with the running plan's fan duty (Fan1) set to fan, everything else
// (the clock, the "V" text, the 4th field, stock or manual) kept. ok=false: not a setting with a plan to change.
func box2SettingWithFan(raw []byte, fan int) ([]byte, bool) {
	var cur map[string]any
	if json.Unmarshal(raw, &cur) != nil {
		return nil, false
	}
	with := func(info string) (string, bool) {
		q, ok := parsePowerplanBox2(info)
		if !ok {
			return "", false
		}
		return box2WithFans(info, fan, q.Fan2)
	}
	if m, _ := cur["manual"].(bool); m {
		s, _ := cur["manualPowerplan"].(string)
		n, ok := with(s)
		if !ok {
			return nil, false
		}
		cur["manualPowerplan"] = n
	} else {
		arr, _ := cur["powerplans"].([]any)
		i := stockSelect(cur)
		if i < 0 || i >= len(arr) {
			return nil, false
		}
		pm, _ := arr[i].(map[string]any)
		s, _ := pm["info"].(string)
		n, ok := with(s)
		if pm == nil || !ok {
			return nil, false
		}
		pm["info"] = n
	}
	b, err := json.Marshal(cur)
	return b, err == nil
}

// box2StillAt: the miner's setting still runs mhz / level code with the fans at fan.
func box2StillAt(mhz, code, fan int) bool {
	pp, ok := readStockBaselineNow("current")
	return ok && pp.FreqMHz == mhz && pp.VrunMv == code && pp.Fan1 == fan
}

// box2PersistPlan: the tuner's end left the miner on a plan the owner keeps (its confirmed best, or the owner's own
// plan put back): the OEM's plan files onto the flash as for an owner's change (oemPersist(true), cfgshadow.go), so
// a reboot starts on it. Again a little later (the OEM's backend writes after it answered), and each time only while
// the miner still runs that clock and level: a throttle or schedule step made meanwhile is not kept as the owner's
// plan. Nothing to do without the settings in RAM (cfgShadow). A variable: tests watch it.
//
// v0.5.4c: the live setting is not enough: the OEM's backend may still be rewriting its plan files in RAM after it
// answered the PUT (a test clock, 650 MHz, would then be committed as the owner's plan). Each pass commits only when
// the RAM copies themselves say the plan: bfgminer.json's vfff clock and voltage and cpbsetting.json's MHz / V text,
// waiting up to box2PersistMatchWait for them; files that never match are logged and the flash is left as it was.
var box2PersistPlan = func(mhz, code int) {
	if !cfgShadow() {
		return
	}
	box2PersistPass(mhz, code, "")
	late := oemLateWrite
	go func() {
		time.Sleep(late)
		box2PersistPass(mhz, code, " (the second look)")
	}()
}

// box2PersistMatchWait / box2PersistMatchPoll: how long one pass waits for the RAM plan files to show the plan.
var (
	box2PersistMatchWait = 15 * time.Second
	box2PersistMatchPoll = time.Second
)

// box2PersistPass: one pass of box2PersistPlan. Reports whether it committed.
func box2PersistPass(mhz, code int, what string) bool {
	deadline := time.Now().Add(box2PersistMatchWait)
	why := ""
	for {
		pp, ok := readStockBaselineNow("current")
		if !ok || pp.FreqMHz != mhz || pp.VrunMv != code {
			return false // the miner moved on (a throttle or schedule step, the owner): not the owner's plan to keep
		}
		var match bool
		if match, why = box2PlanFilesAt(mhz, code); match {
			oemPersist(true)
			return true
		}
		if !time.Now().Before(deadline) {
			break
		}
		time.Sleep(box2PersistMatchPoll)
	}
	log.Printf("tuner: the plan %d MHz / %s was not saved for the next boot%s: the OEM's plan files in RAM don't show it after %s (%s); the settings partition keeps the plan it had",
		mhz, box2LevelText(code), what, box2PersistMatchWait, why)
	return false
}

// box2PlanFilesAt: the OEM's plan files in RAM both hold mhz / level code: bfgminer.json's vfff lines (voltage:clock)
// and cpbsetting.json's running plan (its MHz / V text). why: what doesn't match.
func box2PlanFilesAt(mhz, code int) (ok bool, why string) {
	cpb, err := os.ReadFile(filepath.Join(cfgwin.RAMRoot, "minerd/settings/cpbsetting.json"))
	if err != nil {
		return false, "cpbsetting.json: " + err.Error()
	}
	var cur map[string]any
	if json.Unmarshal(cpb, &cur) != nil {
		return false, "cpbsetting.json doesn't parse"
	}
	if pp, ok := resolveRunningPlan(cur); !ok || pp.FreqMHz != mhz || pp.VrunMv != code {
		return false, fmt.Sprintf("cpbsetting.json says %d MHz / level %d", pp.FreqMHz, pp.VrunMv)
	}
	bfg, err := os.ReadFile(filepath.Join(cfgwin.RAMRoot, "bfgminer/bfgminer.json"))
	if err != nil {
		return false, "bfgminer.json: " + err.Error()
	}
	part := oemPlanPart(bfg)
	if part == "" {
		return false, "bfgminer.json has no vfff line"
	}
	for _, one := range strings.Split(part, ",") {
		f := strings.SplitN(strings.TrimPrefix(one, "vfff="), ":", 2)
		if len(f) != 2 {
			return false, "bfgminer.json says " + one
		}
		v, err1 := strconv.ParseFloat(f[0], 64)
		c, err2 := strconv.ParseFloat(f[1], 64)
		if err1 != nil || err2 != nil || math.Abs(v-float64(code)) >= 0.01 || math.Abs(c-float64(mhz)) >= 0.5 {
			return false, "bfgminer.json says " + one
		}
	}
	return true, ""
}

// tunerGeometryErr: the baseline's chip map must fit the model (Box II: at most 1 board of 36 chips). A map
// that doesn't means icinfo isn't what the judge was built for: the run stops before any test. SC Lite: nil.
func tunerGeometryErr(gained map[ckey]int) error {
	if !tunerOnBox2() {
		return nil
	}
	boards := map[int]int{}
	for k := range gained {
		boards[k.B]++
		if k.I < 0 || k.I > model.ChipsPerBoard {
			return fmt.Errorf("the miner reports chip %d on board %d; this model has %d chips per board", k.I, k.B, model.ChipsPerBoard)
		}
	}
	if len(boards) > model.Boards {
		return fmt.Errorf("the miner reports %d boards; this model has %d", len(boards), model.Boards)
	}
	for b, n := range boards {
		if n > model.ChipsPerBoard {
			return fmt.Errorf("the miner reports %d chips on board %d; this model has %d", n, b, model.ChipsPerBoard)
		}
	}
	return nil
}

// box2TunerInfo: what the Tuner tab shows on this model (GET /api/tuner "box2"). (The "vcode_*" / "stock_code"
// keys are the page's names for the OEM's voltage level.)
func box2TunerInfo() map[string]any {
	hi, lo := box2HashrateTuple(), box2LowPowerTuple()
	dir := "unknown"
	switch model.VCodeDir {
	case vcodeUpIsMore:
		dir = "higher level = more voltage"
	case vcodeUpIsLess:
		dir = "higher level = less voltage"
	}
	note := fmt.Sprintf("On this model the voltage is one of %d fixed steps between %s (about 0.03 V apart, measured at the power lugs under load on one unit; values marked ≈ are estimates), not millivolts, and there is no PV. Stock: %d MHz at %s, %d MHz at %s; only these two are known to be safe. Clocks run %d–%d MHz in %g MHz steps (690 runs 687.5). Abort at most %.0f °C; the OEM's own cut-off is %.0f °C. This model has no power reading: settings are compared by hashrate, hardware errors and voltage.",
		model.VCodeMax-model.VCodeMin+1, box2VoltRangeText(model.VCodeMin, model.VCodeMax), hi.FreqMHz, box2LevelText(hi.V), lo.FreqMHz, box2LevelText(lo.V),
		freqMin, clockWindowHi(), model.ClockStep, model.AbortMaxC, model.TempCutoffC)
	if !featOff(model.Feat.PowerEstimate) { // (F5: the watts are shown only with the power estimate on)
		note = strings.TrimSuffix(note, ".") + "; the watts shown are estimates from wall-meter readings on one unit."
	}
	if !box2VoltKnown() {
		note += fmt.Sprintf(" Which way the voltage setting moves the voltage isn't in this model's profile yet (commissioning test C4), so the tuner tunes the clock only, at %s.", box2LevelText(hi.V))
	}
	volts := map[string]float64{}
	for l, v := range box2LevelVolts {
		volts[fmt.Sprint(l)] = v
	}
	return map[string]any{"vcode_min": model.VCodeMin, "vcode_max": model.VCodeMax, "vcode_dir": dir, "hold_max_c": box2HoldMax(),
		"trim_max":   box2TrimMaxMax,
		"clock_only": !box2VoltKnown(), "stock_code": hi.V, "clock_step": model.ClockStep, "note": note,
		"level_volts": volts}
}

// box2CurveDesc: the Box II run plan's curve item (the engine's config, in ranks; shown as levels).
func box2CurveDesc(c tunerConfig) string {
	if c.MinMV == c.MaxMV {
		return fmt.Sprintf("every clock from %d up to %d MHz in %g MHz steps, %g min per test, all at %s (clock only: which way the voltage setting moves the voltage isn't in this model's profile yet). The curve ends at the first clock that isn't clean.",
			c.LowestMHz, c.HighestMHz, c.Step, c.HoldMin, c.vText(c.MinMV))
	}
	return fmt.Sprintf("every clock from %d up to %d MHz in %g MHz steps, %g min per test. The first clock starts at the lowest voltage (%s), each faster one at the lowest clean voltage one clock slower; one voltage step (about 0.03 V) more at a time until clean (at most %s). The curve ends at the first clock that isn't clean even at %s.",
		c.LowestMHz, c.HighestMHz, c.Step, c.HoldMin, c.vText(c.MinMV), c.vText(c.MaxMV), c.vText(c.MaxMV))
}
