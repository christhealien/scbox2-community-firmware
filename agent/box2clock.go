package main

// Lanes C3/C4: clock control (Feat.ClockControl) and voltage-code control (Feat.VoltageControl) on the
// planBox2V model. Both ship OFF; with them off nothing here changes behaviour (the Box II writer accepts
// only the two stock tuples, exactly as before).
//
// How the stock stack sets clock + voltage (research, see the lane REPORT for addresses):
//   - minerd (the web backend) turns the setting's plan text "<MHz> MHz <V> V <r1> RPM <r2> RPM" (manual:true
//     → manualPowerplan) into the miner API write {"command":"procset","parameter":"<cpb>,vfff,<V>:<MHz>:<r1>:<r2>"}.
//     The panel writes the plan text through PUT /mcb/setting, as it always has: no new device access.
//   - the driver's vfff handler stores V (float) and MHz, then: raising the clock → V first, then a 10 MHz-step
//     PLL ramp; lowering → ramp first, then V. V is applied as fan2 PWM level = (int)(100 − V) (the voltage-trim
//     channel). It range-checks NOTHING (a negative clock becomes 0; V is not checked at all).
//   - the API's devs reply then reports "clock" (the driver's current clock, = the requested MHz once the ramp
//     is done) and "voltage" (the V the vfff path last applied). That is the read-back used here.
//   - the chips' PLL turns a request into floor(MHz/12.5)×12.5 between 500 and 1487.5 MHz (690 → 687.5).
//
// Limits (refuse, never clamp):
//   - clock: the profile window [FreqMin, FreqMax] = [525, 690] (the two stock clocks). Above 690 is lane C7.
//   - V with VoltageControl ON: an integer in the stock codes' closed interval [42, 61].
//   - V with VoltageControl OFF: the code of a stock plan that runs at this clock or FASTER (so a voltage
//     the stock firmware itself runs at a clock at least this high). Default = the nearest such plan: 690 MHz/42 for
//     any clock above 525, 525 MHz/61 at 525. This rule does not depend on which way the code moves the rail
//     voltage: every (clock, V) it allows is a stock (clock', V) with clock' ≥ clock.
//   - every change of clock or V is confirmed from the miner API (devs) and, on a mismatch or no answer,
//     reverted to a stock plan.

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// box2StockPlansText: the stock plans for a message, in volts: "690 MHz at 7.67 V, 525 MHz at 7.03 V".
func box2StockPlansText() string {
	var parts []string
	for _, t := range model.Box2Plans {
		parts = append(parts, fmt.Sprintf("%d MHz at %s", t.FreqMHz, box2LevelText(t.V)))
	}
	return strings.Join(parts, ", ")
}

// box2VRuleText: the voltage rule with voltage control off, in volts, for Controls' GET (box2ControlInfo).
func box2VRuleText() string {
	return "with voltage control off, a clock keeps the voltage of the stock plan at that clock or faster (stock: " + box2StockPlansText() + ")"
}

// box2VRange: the closed interval of the profile's stock V codes ([42, 61] on the SC Box II).
func box2VRange() (lo, hi int) {
	for i, t := range model.Box2Plans {
		if i == 0 || t.V < lo {
			lo = t.V
		}
		if i == 0 || t.V > hi {
			hi = t.V
		}
	}
	return lo, hi
}

// box2ClockWindow: the clocks clock control may set: the profile's window, its top raised to the effective
// ceiling clockWindowHi() (above FreqMax only while the ceiling probe runs (C6) or once the overclock unlock is
// commissioned and accepted (C7), up to this unit's probe result), and never past OCHardMax (750 MHz).
func box2ClockWindow() (lo, hi int) {
	hi = model.FreqMax
	if w := clockWindowHi(); w > hi && model.OCHardMax > hi {
		hi = min(w, model.OCHardMax)
	}
	return model.FreqMin, hi
}

// box2OCDefaultV: the V code a clock above the stock window runs at when the request names none: the stock
// Hashrate plan's (box2OverclockErr's rule). ok=false at or below FreqMax (box2VFor's rule applies there).
func box2OCDefaultV(mhz int) (int, bool) {
	if mhz <= model.FreqMax {
		return 0, false
	}
	_, v := box2HashrateV()
	return v, true
}

// box2VFor: the V code a clock runs at when the request names none (or VoltageControl is off): the code of
// the lowest-clock stock plan whose clock is ≥ mhz. ok=false when no stock plan runs that fast.
func box2VFor(mhz int) (int, bool) {
	best := -1
	v := 0
	for _, t := range model.Box2Plans {
		if t.FreqMHz >= mhz && (best < 0 || t.FreqMHz < best) {
			best, v = t.FreqMHz, t.V
		}
	}
	return v, best >= 0
}

// box2VAllowedNoVC: with VoltageControl off, the V codes a clock may run at: those of every stock plan whose
// clock is ≥ mhz.
func box2VAllowedNoVC(mhz, v int) bool {
	for _, t := range model.Box2Plans {
		if t.FreqMHz >= mhz && t.V == v {
			return true
		}
	}
	return false
}

// box2CheckTuple validates a non-stock clock/V request under the commissioned gates. A stock tuple is
// always accepted (the existing two-mode behaviour); everything else needs ClockControl.
func box2CheckTuple(mhz, v int) error {
	if box2StockMatch(powerplan{FreqMHz: mhz, VrunMv: v}) {
		return nil
	}
	if !model.Feat.ClockControl {
		return fmt.Errorf("refused: %d MHz at %s is not one of the stock plans (%s) — this model ships only its stock plans", mhz, box2LevelText(v), box2StockPlansText())
	}
	lo, hi := box2ClockWindow()
	if mhz < lo || mhz > hi {
		return fmt.Errorf("refused: %d MHz is outside this model's clock window (%d–%d MHz)", mhz, lo, hi)
	}
	if mhz > model.FreqMax && !model.Feat.VoltageControl { // above the stock window (C6/C7): box2OverclockErr's rule
		if _, hv := box2HashrateV(); v != hv {
			return fmt.Errorf("refused: above %d MHz the voltage stays Stock High's %s (voltage control is off), not %s", model.FreqMax, box2LevelText(hv), box2LevelText(v))
		}
		return nil
	}
	if model.Feat.VoltageControl {
		vlo, vhi := box2VRange()
		if v < vlo || v > vhi {
			return fmt.Errorf("refused: %s is outside the allowed voltages: it must be between %s", box2LevelText(v), box2VoltRangeText(vlo, vhi))
		}
		return nil
	}
	if !box2VAllowedNoVC(mhz, v) {
		want, _ := box2VFor(mhz)
		return fmt.Errorf("refused: voltage control is off — at %d MHz the voltage stays %s (the voltage of the stock plan at that clock or faster), not %s", mhz, box2LevelText(want), box2LevelText(v))
	}
	return nil
}

// box2PLLClock: the clock the chips actually run for a request (the driver's PLL search: the first divider
// d of 48,32,24,16,8,6,2,1 with 20 < N=int(d·MHz/25) ≤ 119; clock = N·25/d). 0 when none fits.
func box2PLLClock(mhz float64) float64 {
	for _, d := range []float64{48, 32, 24, 16, 8, 6, 2, 1} {
		n := math.Trunc(d * mhz / 25)
		if n > 20 && n <= 119 {
			return n * 25 / d
		}
	}
	return 0
}

// ---- read-back + revert ----

// box2DevsCmd reads the miner API's devs reply (a seam for tests).
var box2DevsCmd = func() ([]byte, error) { return cgminerCmd("devs") }

// how long a clock/V write is given to show up in devs (minerd forwards it, the driver ramps 10 MHz per step),
// and how often it is polled. v0.5.4: devs is not asked at all for the first box2ConfirmQuiet after the write (the
// driver ramps the clock in 10 MHz steps then, and heavy miner-API traffic during a ramp is the leading suspect for
// the v0.5.3 kernel freeze), then every box2ConfirmPollSlow; box2ConfirmPoll / box2ConfirmFastFor only matter when
// the quiet time is set to 0 (tests). The deadline (box2ConfirmWaitFor) is unchanged: only after it is a write
// reverted.
const box2ConfirmQuietDefault = 30 * time.Second

var (
	box2ConfirmWait     = 45 * time.Second
	box2ConfirmQuiet    = box2ConfirmQuietDefault
	box2ConfirmPoll     = 2 * time.Second
	box2ConfirmPollSlow = 5 * time.Second
	box2ConfirmFastFor  = 20 * time.Second
)

// box2ConfirmPollAt: the pause before the next devs poll, elapsed into a confirmation: the rest of the quiet time
// first, then every box2ConfirmPollSlow.
func box2ConfirmPollAt(elapsed time.Duration) time.Duration {
	if elapsed < box2ConfirmQuiet {
		return box2ConfirmQuiet - elapsed
	}
	if (elapsed >= box2ConfirmFastFor || box2ConfirmQuiet > 0) && box2ConfirmPollSlow > box2ConfirmPoll {
		return box2ConfirmPollSlow
	}
	return box2ConfirmPoll
}

// box2ConfirmWaitFor: the base wait plus a third of it for every 10 MHz the clock moves (the driver ramps 10 MHz
// per step, and a bigger move restarts hashing for longer: on the first unit 690→650 MHz took longer than 45 s),
// at most 4× the base.
func box2ConfirmWaitFor(w box2Want) time.Duration {
	d := w.MHz - w.Before.FreqMHz
	if d < 0 {
		d = -d
	}
	if w.Before.FreqMHz <= 0 {
		d = 0
	}
	steps := (d + 9) / 10
	wait := box2ConfirmWait + time.Duration(steps)*box2ConfirmWait/3
	if max := 4 * box2ConfirmWait; wait > max {
		wait = max
	}
	return wait
}

// box2WriteSeq counts clock/V writes: a confirmation only reverts the write it was started for.
var box2WriteSeq atomic.Int64

// box2ClockQuietNow: a clock/V change is being taken: its confirmation is running (box2ConfirmCoop, the tuner's,
// Controls', a preset's, or the throttle's in the background), its quiet time (box2ConfirmQuiet) included. v0.5.5:
// re-applying the chip trims (reapplyTrims' "pgaset clock") waits then, so the trims add no miner-API commands while
// the driver ramps the clock (the v0.5.3 freeze). (Every clock/V write with clock control on is confirmed, and the
// chip trims need clock control.) box2Writing covers the moments around it: a setting write on its way (its PUT
// before its confirmation starts), and a background confirmation not yet started.
func box2ClockQuietNow() bool {
	return box2Confirming.Load() > 0 || box2Writing.Load() > 0 || time.Now().UnixNano() < box2QuietUntil.Load()
}

// box2QuietUntil / box2QuietFor (v0.5.5): a quiet time of its own after a setting write that has no confirmation
// of its own to wait in: a failed confirmation's revert (the boards ramp back), the Idle's write, a whole-setting
// write put back (putSettingGen). Auto-trim and the trim re-send wait it out like a confirmation.
var (
	box2QuietUntil atomic.Int64 // unix ns
	box2WriteQuiet = 30 * time.Second
)

func box2QuietFor(d time.Duration) {
	until := time.Now().Add(d).UnixNano()
	for {
		cur := box2QuietUntil.Load()
		if cur >= until || box2QuietUntil.CompareAndSwap(cur, until) {
			return
		}
	}
}

// box2Writing: SC Box II setting writes (mutatePlanBox2Co) in progress, from before the PUT until the confirmation
// returns (for the throttle's background confirmation, until it ends).
var box2Writing atomic.Int32

// box2Want is one clock/V write awaiting confirmation.
type box2Want struct {
	MHz, V     int
	Before     powerplan // the plan running before the write
	Fan1, Fan2 int       // the fans written with it (kept on a revert)
	Plan       string    // the plan text written
	Seq        int64
}

// box2ReadBack: the clock/V every enabled board reports now. ok=false when the API can't be read or no
// enabled board is listed.
func box2ReadBack() (clocks, volts []float64, ok bool) {
	at := time.Now()
	raw, err := box2DevsCmd()
	if err != nil {
		return nil, nil, false
	}
	dashShareNoteDevs(raw, at) // (the poll is the shared Live reading's refresh too: dashshare.go)
	devs, found := apiEntries(raw, "DEVS")
	if !found {
		return nil, nil, false
	}
	for _, d := range devs {
		if apiStr(d, "Enabled") == "N" {
			continue
		}
		clocks = append(clocks, apiNum(d, "clock"))
		volts = append(volts, apiNum(d, "voltage"))
	}
	return clocks, volts, len(clocks) > 0
}

func box2Matches(w box2Want, clocks, volts []float64) bool {
	if len(clocks) == 0 {
		return false
	}
	for i := range clocks {
		if math.Abs(clocks[i]-float64(w.MHz)) >= 0.5 || math.Abs(volts[i]-float64(w.V)) >= 0.01 {
			return false
		}
	}
	return true
}

// box2Coop: what a writer that must keep working while its clock/V write is confirmed does meanwhile (the tuner:
// box2Confirm can wait minutes, and the tuner must keep its heat abort, its fan hold and its Stop all that time).
//   - stop runs about every second: an error ends the wait at once (the tuner's Stop);
//   - tick runs every `every` (the tuner's poll): the temperature check and the fan hold; quiet is true inside the
//     first box2ConfirmQuiet after the write. An error (the heat abort) ends the wait too.
//
// A wait ended this way reverts nothing itself and returns that error: the writer puts the miner back (the tuner's
// end goes back to what the miner ran before, or its baseline, as after any abort).
type box2Coop struct {
	every time.Duration
	stop  func() error
	tick  func(quiet bool) error
}

// box2CoopPause: sleep d (from start's point of view: quiet while inside box2ConfirmQuiet), running co's stop about
// every second and its tick every co.every. nil co: a plain sleep.
func box2CoopPause(co *box2Coop, start time.Time, d time.Duration, lastTick *time.Time) error {
	if co == nil {
		time.Sleep(d)
		return nil
	}
	every := co.every
	if every <= 0 {
		every = 10 * time.Second
	}
	end := time.Now().Add(d)
	for {
		left := time.Until(end)
		if left <= 0 {
			return nil
		}
		step := min(left, time.Second, every)
		time.Sleep(step)
		if co.stop != nil {
			if err := co.stop(); err != nil {
				return err
			}
		}
		if co.tick != nil && time.Since(*lastTick) >= every {
			*lastTick = time.Now()
			if err := co.tick(time.Since(start) < box2ConfirmQuiet); err != nil {
				return err
			}
		}
	}
}

// box2Confirm polls devs until every enabled board reports the written clock and V. On a mismatch at the
// deadline (or no API answer) it reverts to a stock plan and returns an error saying so. A newer clock/V
// write since (another seq) cancels the revert: that write is confirmed on its own.
func box2Confirm(w box2Want) error { return box2ConfirmCoop(w, nil) }

// box2ConfirmCoop: box2Confirm, with co's work done while it waits (nil: none). devs is first asked
// box2ConfirmQuiet after the write. Meanwhile the Live readers share one reading (dashShareOn).
func box2ConfirmCoop(w box2Want, co *box2Coop) error {
	box2Confirming.Add(1) // every Dashboard reader shares one reading meanwhile (dashshare.go)
	defer box2Confirming.Add(-1)
	wait := box2ConfirmWaitFor(w)
	start := time.Now()
	deadline := start.Add(wait)
	lastTick := start
	traceNote("confirm #%d: waiting up to %s for %d MHz / level %d (from %d MHz / level %d)", w.Seq, wait, w.MHz, w.V, w.Before.FreqMHz, w.Before.VrunMv)
	var clocks, volts []float64
	polls := 0
	ended := func(err error) error {
		traceNote("confirm #%d: the writer ended the wait after %s (%d polls): %v", w.Seq, time.Since(start).Round(time.Second), polls, err)
		return err
	}
	if q := min(box2ConfirmQuiet, wait); q > 0 { // the driver ramps: no miner-API poll yet
		if err := box2CoopPause(co, start, q, &lastTick); err != nil {
			return ended(err)
		}
	}
	for {
		var ok bool
		clocks, volts, ok = box2ReadBack()
		polls++
		if ok && box2Matches(w, clocks, volts) {
			traceNote("confirm #%d: confirmed after %s (%d polls)", w.Seq, time.Since(start).Round(time.Second), polls)
			return nil
		}
		if box2WriteSeq.Load() != w.Seq {
			traceNote("confirm #%d: superseded by a newer write after %d polls", w.Seq, polls)
			return nil // superseded by a newer write
		}
		if time.Now().After(deadline) {
			break
		}
		if err := box2CoopPause(co, start, box2ConfirmPollAt(time.Since(start)), &lastTick); err != nil {
			return ended(err)
		}
	}
	traceNote("confirm #%d: NOT confirmed after %s (%d polls, read back %v / %v): reverting", w.Seq, time.Since(start).Round(time.Second), polls, clocks, volts)
	seen := "no answer from the miner API"
	if len(clocks) > 0 {
		seen = fmt.Sprintf("the miner reports %.1f MHz / %s", clocks[0], box2ReadText(volts[0]))
	}
	to, err := box2Revert(w)
	msg := fmt.Sprintf("%d MHz / %s was not confirmed within %s (%s)", w.MHz, box2LevelText(w.V), wait.Round(time.Second), seen)
	switch {
	case err != nil:
		msg += "; putting the stock plan back FAILED: " + err.Error()
	case to == "":
		msg += "; a newer setting was written meanwhile, so nothing was put back"
	default:
		msg += "; put back the stock plan " + planShowRaw(to)
	}
	activity.emit("⚠ clock/voltage: " + msg)
	return fmt.Errorf("%s", msg)
}

// box2ReadText: a level the miner reports back (a float) in volts; one that isn't a whole level shows as read.
func box2ReadText(v float64) string {
	if r := math.Round(v); math.Abs(v-r) < 0.01 {
		return box2LevelText(int(r))
	}
	return "a voltage setting that isn't one of its 20 steps"
}

// box2RevertTarget: the stock plan a failed write goes back to: the plan before it when that was a stock
// tuple, else the lowest-clock stock plan.
//
// By design this can be the stock 690 MHz plan even while hot (the thermal throttle's step down to a non-stock
// clock that the miner didn't confirm): an unconfirmed clock is not trusted, a stock plan is, and the heat is
// handled by the guards above it (the throttle steps again at its next tick, the thermal guard parks, the fans
// stay at the heat guard's duty, the OEM's cut-off is the backstop).
//
// A chip trimmed "+" that would run past the clock window at the plan before (the trims changed since) sends it to
// the lowest-clock stock plan too: the revert runs under planMu (and, from Controls, under controlMu), so it can't
// take chipTrimMu to lower the trims (capPlusTrims: controlMu → chipTrimMu → planMu); the low plan fits any trim.
func box2RevertTarget(before powerplan) powerplan {
	if box2StockMatch(before) && plusTrimErr(before.FreqMHz) == nil {
		return before
	}
	var lo *box2Tuple
	for i := range model.Box2Plans {
		if lo == nil || model.Box2Plans[i].FreqMHz < lo.FreqMHz {
			lo = &model.Box2Plans[i]
		}
	}
	return powerplan{FreqMHz: lo.FreqMHz, VrunMv: lo.V}
}

// box2Revert writes the stock plan back (stock clock + the stock plan's own V text, the fans that were
// written kept), unless the setting no longer holds the plan being reverted. Returns the plan put back ("" =
// nothing to do).
func box2Revert(w box2Want) (string, error) {
	planMu.Lock()
	defer planMu.Unlock()
	if box2WriteSeq.Load() != w.Seq {
		return "", nil
	}
	raw, err := stockGet("/mcb/setting")
	if err != nil {
		return "", err
	}
	var cur map[string]any
	if err := json.Unmarshal(raw, &cur); err != nil {
		return "", err
	}
	if m, _ := cur["manual"].(bool); !m {
		return "", nil // a stock mode was selected since (manual off): that is a stock plan already
	}
	if s, _ := cur["manualPowerplan"].(string); s != w.Plan {
		if pp, ok := parsePowerplanBox2(s); !ok || pp.FreqMHz != w.MHz || pp.VrunMv != w.V {
			return "", nil // someone wrote another plan since
		}
	}
	t := box2RevertTarget(w.Before)
	t.Fan1 = w.Fan1
	f4, ok := box2Field4(cur, t) // the stock plan's own 4th field (a trim, not a fan): see box2Field4
	if !ok {
		return "", fmt.Errorf("revert: no stock plan gives the voltage-trim setting (the plan's 4th field, not a fan speed) for %d MHz", t.FreqMHz)
	}
	t.Fan2 = f4
	plan := buildPowerplanBox2(t)
	if info := box2StockInfoFor(cur, t); info != "" {
		if s, ok := box2WithFans(info, t.Fan1, t.Fan2); ok {
			plan = s
		}
	}
	cur["manual"] = true
	cur["manualPowerplan"] = plan
	body, _ := json.Marshal(cur)
	box2QuietFor(box2WriteQuiet) // (F2: the boards ramp back: no trim re-send, no auto-trim devs/pgaset meanwhile)
	if _, err := stockReq("PUT", "/mcb/setting", body); err != nil {
		return "", err
	}
	box2QuietFor(box2WriteQuiet) // (from the PUT on)
	box2WriteSeq.Add(1)
	recordMark("P", "reverted to stock "+planShowRaw(plan))
	return plan, nil
}

// box2AsyncConfirms: confirmations running in the background (for writers that can't wait, e.g. the thermal
// throttle inside the fan loop). Tests wait on it.
var box2AsyncConfirms sync.WaitGroup

// ---- the Controls write ----

// box2ApplyControl is Controls' custom-plan write on planBox2V (handleControl): clock (+ V code when
// VoltageControl is on; V 0 = the rule's code), fans optional (0/0 keeps the running fans). It goes through
// mutatePlanGen → mutatePlanBox2 (validation, fan floor/heat guard, the PUT, the read-back confirmation).
func box2ApplyControl(gen int64, p powerplan) (string, error) {
	if p.FreqMHz <= 0 {
		return "", fmt.Errorf("a clock is required")
	}
	v := p.VrunMv
	if v == 0 {
		if dv, ok := box2VFor(p.FreqMHz); ok {
			v = dv
		} else if dv, ok := box2OCDefaultV(p.FreqMHz); ok {
			v = dv
		}
	}
	if err := box2CheckTuple(p.FreqMHz, v); err != nil {
		return "", err
	}
	if p.Fan1 < 0 || p.Fan1 > 100 || p.Fan2 < 0 || p.Fan2 > 100 {
		return "", fmt.Errorf("fan duty must be 0–100%%")
	}
	if err := plusTrimErr(p.FreqMHz); err != nil {
		return "", err
	}
	keepFans := p.Fan1 == 0 && p.Fan2 == 0
	return mutatePlanGen(gen, func(x *powerplan) {
		x.FreqMHz, x.VrunMv, x.PVMv = p.FreqMHz, v, 0
		if !keepFans {
			x.Fan1, x.Fan2 = p.Fan1, p.Fan2
		}
	})
}

// box2ControlInfo: what Controls' GET adds on planBox2V, so the page can offer the window and the rule.
func box2ControlInfo() map[string]any {
	lo, hi := box2ClockWindow()
	vlo, vhi := box2VRange()
	return map[string]any{
		"clock_control":    model.Feat.ClockControl,
		"voltage_control":  model.Feat.VoltageControl,
		"clock":            []int{lo, hi},
		"v_codes":          []int{vlo, vhi},
		"v_is_code":        true, // the OEM's voltage LEVEL, not millivolts (and no PV): a higher level is a lower voltage (C4)
		"v_higher_is_less": model.VCodeDir == vcodeUpIsLess,
		"v_rule":           box2VRuleText(),
		"pll_note":         "the chips run floor(MHz ÷ 12.5) × 12.5 between 500 and 1487.5 MHz (690 → 687.5)",
		// C6/C7: this unit's saved probe result, the most the unlock opens (box2ProbeLimit) and why it can't yet
		"probe": box2ProbeView(), "oc_limit": box2ProbeLimit(), "unlock_err": box2UnlockErr(), "oc_hard_max": model.OCHardMax,
		"probe_abort_c": ceilAbortBox2C(), "probe_min_errs": ceilHWMinErrs, // the probe's own limits, for its dialog
	}
}

// ---- presets / schedule (lane C3) ----

// box2SavedPresets: the owner's saved clock presets that may run now — none unless ClockControl is on, and
// only those that pass today's window/V rule (a preset saved with a V code while VoltageControl was on is
// left out once it is off). A saved name that clashes with a stock mode is left out too.
func box2SavedPresets() []preset {
	if model.Plan != planBox2V || !model.Feat.ClockControl {
		return nil
	}
	var out []preset
	for _, p := range loadPresets() {
		if p.MHz <= 0 || isBox2StockName(p.Name) || box2CheckTuple(p.MHz, p.MV) != nil {
			continue
		}
		out = append(out, p)
	}
	return out
}

// box2OutsidePresets: the owner's saved clock presets that box2SavedPresets leaves out because today's clock window
// or V rule refuses them (the window locked again lower, voltage control off): for the Presets tab to show, marked,
// so they can be deleted. why: the rule's own words.
func box2OutsidePresets() []map[string]any {
	if model.Plan != planBox2V || !model.Feat.ClockControl {
		return nil
	}
	var out []map[string]any
	for _, p := range loadPresets() {
		if p.MHz <= 0 || isBox2StockName(p.Name) {
			continue
		}
		if err := box2CheckTuple(p.MHz, p.MV); err != nil {
			out = append(out, map[string]any{"name": p.Name, "mhz": p.MHz, "mv": p.MV, "why": err.Error()})
		}
	}
	return out
}

// box2OutsideWhy: why the saved preset of that name is outside the clock window now ("": it isn't, or no such preset).
func box2OutsideWhy(name string) string {
	for _, o := range box2OutsidePresets() {
		if o["name"] == name {
			return fmt.Sprint(o["why"])
		}
	}
	return ""
}

// box2SavedPreset: the runnable saved preset of that name, or nil.
func box2SavedPreset(name string) *preset {
	for _, p := range box2SavedPresets() {
		if p.Name == name {
			pp := p
			return &pp
		}
	}
	return nil
}

// isBox2StockName: a stock-mode label (from the profile, so no miner read).
func isBox2StockName(name string) bool {
	for _, t := range model.Box2Plans {
		if n := box2ModeName(t.FreqMHz); strings.EqualFold(n, name) || strings.EqualFold(presetLabel(n), name) { // "Stock High" too
			return true
		}
	}
	return false
}

// box2PresetCheck validates a preset save on planBox2V: the name must not be a stock mode's, the clock/V
// must pass box2CheckTuple. V 0 = the rule's code. Returns the V code to store.
func box2PresetCheck(name string, mhz, v int) (int, error) {
	if isBox2StockName(name) {
		return 0, fmt.Errorf("“%s” is a stock mode's name: pick another name", name)
	}
	if v == 0 {
		if dv, ok := box2VFor(mhz); ok {
			v = dv
		} else if dv, ok := box2OCDefaultV(mhz); ok {
			v = dv
		}
	}
	if err := box2CheckTuple(mhz, v); err != nil {
		return 0, err
	}
	if !box2OnClockGrid(mhz) { // (v0.5.5) as the tuner's clocks: one the chips run, as its smallest whole MHz
		r := pll(float64(mhz))
		return 0, fmt.Errorf("%d MHz runs %g MHz on the chips (they run 12.5 MHz steps): enter %d", mhz, r, int(math.Ceil(r-1e-9)))
	}
	return v, nil
}

// box2OnClockGrid: a clock as the panel writes it: a step of the chips' 12.5 MHz grid as its smallest whole MHz (600,
// 613 for 612.5, 688 for 687.5), or one of the stock plans' clocks (690 runs 687.5).
func box2OnClockGrid(mhz int) bool {
	for _, t := range model.Box2Plans {
		if t.FreqMHz == mhz {
			return true
		}
	}
	return mhz == int(math.Ceil(pll(float64(mhz))-1e-9))
}

// applyBox2PresetGen applies a saved clock preset on planBox2V (Presets tab, schedule): the same checks
// and writer as Controls (mutatePlanGen → mutatePlanBox2: window/V rule, read-back confirmation, revert).
func applyBox2PresetGen(name string, gen int64) error {
	if err := miningHoldErr(); err != nil {
		return err
	}
	if err := heatCoolErr(); err != nil {
		return err
	}
	found := box2SavedPreset(name)
	if found == nil {
		for _, o := range box2OutsidePresets() { // (P6: a schedule rule on a preset the window no longer takes)
			if o["name"] == name {
				return fmt.Errorf("refused: the preset %q (%v MHz) is outside the current clock window (%v): edit or delete it on the Presets tab", name, o["mhz"], o["why"])
			}
		}
		return fmt.Errorf("refused: %q is neither a stock mode nor a saved clock preset that can run on %s now", name, model.Name)
	}
	if err := plusTrimErr(found.MHz); err != nil {
		return err
	}
	if _, err := mutatePlanGen(gen, func(pp *powerplan) { pp.FreqMHz, pp.VrunMv, pp.PVMv = found.MHz, found.MV, 0 }); err != nil {
		return fmt.Errorf("apply failed: %v", err)
	}
	applyPresetFan(*found)
	if !setChargeGen(gen, "preset:"+found.Name) {
		return errSafetyStop
	}
	recordMark("P", "preset "+found.Name)
	return nil
}
