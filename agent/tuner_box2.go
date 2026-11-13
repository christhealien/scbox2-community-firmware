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
//   - No power: the Box II has no power sensor and the power estimate (C2) is off, so the tuner never ranks or
//     labels anything by watts or J/TH. It ranks by clock (hashrate), the hardware-error judge and the voltage
//     level (less voltage first).
//   - Thermal: abort, target and cut-off come from the profile (abort default 82, at most 84; fan target 77;
//     the OEM's 86 °C cut-off is never touched). The Box II's fan behaviour isn't characterised (the fan
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
)

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
	c.PVGap = 0
	c.AbortC = model.AbortDefC
	// the fan hold's controller (and its 30 % floor) was tuned on an SC Lite; the Box II's fans aren't
	// characterised, so by default the run leaves them to the OEM's own temperature control
	c.FanHold = false
	c.FanTargetC = model.TargetC
	c.WarmestC = model.TargetC
	c.ConfirmMHz, c.ConfirmMV = hi.FreqMHz, hi.V
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
	case "map", "confirm":
	case "rebuild":
		return fmt.Errorf("rebuilding presets isn't available on this model: its presets are the two stock plans")
	default:
		return fmt.Errorf("mode must be map or confirm")
	}
	if c.BuildPresets {
		return fmt.Errorf("building presets isn't available on this model: its presets are the two stock plans")
	}
	if c.ChipTrim && featOff(model.Feat.ChipTrims) {
		return fmt.Errorf("chip trims are not available on this model (%s): untick “Trim weak chips”", model.Name)
	}
	if c.ChipTrim {
		return fmt.Errorf("“Trim weak chips” isn't available in the tuner on this model yet: untick it")
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
		if c.Step < step || c.Step > 100 || !onPLLGrid(c.Step) {
			return fmt.Errorf("clock step must be a multiple of %g MHz (the chips' clock steps), %g–100", step, step)
		}
		cl := c.clockList()
		if len(cl) == 0 || cl[0] < fLo {
			return fmt.Errorf("no clock to test between %d and %d MHz with %g MHz steps (the first must be %d MHz or more)", c.LowestMHz, c.HighestMHz, c.Step, fLo)
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
		if c.FanTargetC < 45 || c.FanTargetC > model.TargetC {
			return fmt.Errorf("fan target must be 45–%.0f °C on this model", model.TargetC)
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
func box2TunerApply(gen int64, p planMV) error {
	h := p.hw()
	if err := box2TunerWriteErr(h); err != nil {
		return err
	}
	return applyTunePlanGen(gen, h)
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
		err := putSettingGen(gen, raw)
		if miningHoldErr() != nil || genMoved(gen) {
			return nil
		}
		// (only Fan1 is a fan duty: the plan's 4th field (Fan2) is the second voltage-trim PWM, which the plan
		// writer always keeps at the stock plan's value)
		if err == nil && (pp.Fan1 < fanFloor() || heatGuardActive() || hold > 0) {
			if _, err = mutatePlanGen(gen, func(x *powerplan) { x.Fan1 = max(x.Fan1, hold) }); err == errSafetyStop {
				err = nil
			}
		}
		return err
	}, what
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
	note := fmt.Sprintf("On this model the voltage is one of %d fixed steps between %s (about 0.03 V apart, measured at the power lugs under load on one unit; values marked ≈ are estimates), not millivolts, and there is no PV. Stock: %d MHz at %s, %d MHz at %s; only these two are known to be safe. Clocks run %d–%d MHz in %g MHz steps (690 runs 687.5). Abort at most %.0f °C; the OEM's own cut-off is %.0f °C. This model has no power reading, so settings are compared by hashrate, hardware errors and voltage, not watts.",
		model.VCodeMax-model.VCodeMin+1, box2VoltRangeText(model.VCodeMin, model.VCodeMax), hi.FreqMHz, box2LevelText(hi.V), lo.FreqMHz, box2LevelText(lo.V),
		freqMin, clockWindowHi(), model.ClockStep, model.AbortMaxC, model.TempCutoffC)
	if !box2VoltKnown() {
		note += fmt.Sprintf(" Which way the voltage setting moves the voltage isn't in this model's profile yet (commissioning test C4), so the tuner tunes the clock only, at %s.", box2LevelText(hi.V))
	}
	volts := map[string]float64{}
	for l, v := range box2LevelVolts {
		volts[fmt.Sprint(l)] = v
	}
	return map[string]any{"vcode_min": model.VCodeMin, "vcode_max": model.VCodeMax, "vcode_dir": dir,
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
