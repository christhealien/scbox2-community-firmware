package main

// SC Box II (planBox2V) parts of the clock-ceiling probe (commissioning C6, Feat.CeilingProbe) and the overclock
// unlock (C7, Feat.OverclockUnlock). Both ship OFF; with the gates off nothing here changes anything:
//   - ceilBox2Window never opens (it needs Feat.CeilingProbe and the probe holding the charge);
//   - box2OverclockErr only refuses (and is a no-op for the two stock plans);
//   - freqHardMax stays the profile's FreqHardMax (690) unless OverclockUnlock is on (setWindowsFromModel).
//
// The SC Lite probe is a clamp detector: each step is applied for a moment and the firmware's stored plan read
// back. On the Box II that question is already answered by the disassembly: neither minerd (atoi of the plan's
// "%d MHz") nor the gsscbox2 driver (vfff: atof, then a 10 MHz walk through the PLL table) caps the clock below
// 1487.5 MHz, so a clamp probe would always report "no cap". What the Box II needs to know is how far its chips
// hold while mining at the stock voltage level, so here each step is HELD while mining and judged by the panel's
// guards: board temperature (abort tier), the hardware-error rate, and chip/board dropouts. The run always ends
// on the baseline, which must be the stock Hashrate plan (690 MHz, V code 42): the stock plan is what it returns to.
//
// Clock writes go through ceilMutate (= mutatePlanGen → mutatePlanBox2), the same seam as every other clock
// writer. The Box II clock backend behind Feat.ClockControl is lane C3's; this file only requires that it
// accepts a clock up to clockWindowHi() and calls box2OverclockErr (already wired in mutatePlanBox2).

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"time"
)

// box2OCHardMax: the SC Box II overclock ceiling (MHz): variant_owner.go (the owner's commissioning build) and
// variant_public.go (the public release: none, 0).

const ceilBox2Step = 10 // MHz: the driver walks the clock in 10 MHz requests; the plan carries whole MHz

var (
	ceilMutate    = mutatePlanGen  // the probe's clock writes (tests replace it with a fake miner backend)
	ceilDash      = buildDashboard // the probe's readings (tests replace it)
	ceilBox2Hold  = 3 * time.Minute
	ceilBox2Every = 10 * time.Second
	ceilRiskOK    atomic.Bool // Box II: this probe run was started with the risk accepted (ceilBox2StartErr)
)

// the Box II probe's guard thresholds
const (
	ceilHWMinErrs = 10  // fewer new HW errors than this in one step is noise
	ceilHWPctMax  = 1.0 // a step's HW-error % above this (or 3× the baseline's, if higher) fails it
	ceilNoTempN   = 3   // readings in a row without a board temperature: stop (never probe blind)
)

// ceilAckOK: the probe may (still) write steps past the stock window.
func ceilAckOK() bool {
	if model.Plan == planBox2V {
		return ocUnlocked() || ceilRiskOK.Load()
	}
	return loadOverclock().Acknowledged
}

// ceilBox2Hi: the Box II probe's top clock: the unlocked ceiling, tightened by a recorded cap (effHard).
func ceilBox2Hi() int {
	hi := model.OCHardMax
	if hi < freqHardMax {
		hi = freqHardMax
	}
	if hi < freqMax {
		hi = freqMax
	}
	return effHard("freq", hi)
}

// ceilBox2Window: while the Box II probe runs (gate on, it holds the charge, risk accepted), the clock window
// is the probe's; otherwise ok=false and clockWindowHi is unchanged. Never on the SC Lite.
func ceilBox2Window() (int, bool) {
	if model.Plan != planBox2V || !model.Feat.CeilingProbe || !model.Feat.ClockControl || !ceilRiskOK.Load() {
		return 0, false
	}
	if getCharge() != "ceiling" {
		return 0, false
	}
	return ceilBox2Hi(), true
}

// box2HashrateV: the V code of the model's highest-clock stock plan (Hashrate: 42), the code kept above it.
func box2HashrateV() (int, int) {
	f, v := 0, 0
	for _, t := range model.Box2Plans {
		if t.FreqMHz > f {
			f, v = t.FreqMHz, t.V
		}
	}
	return f, v
}

// box2OverclockErr: the C6/C7 rule for every Box II clock write (called by mutatePlanBox2 before it writes). A
// clock above the stock window only inside clockWindowHi() (the unlocked window, or the probe's while it runs),
// and there the voltage code stays the Hashrate plan's stock code unless VoltageControl is commissioned too.
// Writes that change neither the clock nor the code (fans) are not judged here.
func box2OverclockErr(p, before powerplan) error {
	if model.Plan != planBox2V || p.FreqMHz <= freqMax || (p.FreqMHz == before.FreqMHz && p.VrunMv == before.VrunMv) {
		return nil
	}
	if hi := clockWindowHi(); p.FreqMHz > hi {
		return fmt.Errorf("refused: %d MHz is past the clock window (%d MHz)", p.FreqMHz, hi)
	}
	if _, v := box2HashrateV(); !model.Feat.VoltageControl && p.VrunMv != v {
		return fmt.Errorf("refused: above %d MHz the voltage stays Stock High's %s (voltage control is off), not %s", freqMax, box2LevelText(v), box2LevelText(p.VrunMv))
	}
	return nil
}

// ceilBox2StartErr: the probe's start conditions that don't need the miner.
func ceilBox2StartErr(acceptRisk bool) error {
	if model.OCHardMax <= freqMax { // (the SC Box II: its overclock ceiling isn't decided, box2OCHardMax)
		return fmt.Errorf("refused: the %s's overclock ceiling isn't decided, so nothing above %d MHz is probed", model.Name, freqMax)
	}
	if !model.Feat.ClockControl {
		return errors.New("the clock ceiling probe needs clock control switched on (Settings → Commissioning)")
	}
	if !ocUnlocked() && !acceptRisk {
		return errors.New("the probe runs the clock past the stock window: accept the risk to start it")
	}
	return nil
}

// ceilBox2BaseErr: the probe starts only from the stock Hashrate plan (it keeps that plan's V code for every
// step and returns to it), on a complete board (model geometry, every chip found, no dropout).
func ceilBox2BaseErr(base powerplan) error {
	f, v := box2HashrateV()
	if base.FreqMHz != f || base.VrunMv != v {
		return fmt.Errorf("switch to Stock High (%d MHz, %s) first: the probe starts from it, keeps its voltage and returns to it (now %d MHz, %s)", f, box2LevelText(v), base.FreqMHz, box2LevelText(base.VrunMv))
	}
	d := ceilDash()
	if !d.OK {
		return errors.New("the miner isn't answering: no readings to judge the steps by")
	}
	if _, _, ok := maxBoardTemp(d); !ok {
		return errors.New("no board temperature: the probe never runs blind")
	}
	alive := 0
	for _, b := range d.Boards {
		if b.Alive && b.Enabled {
			alive++
		}
	}
	if alive != model.Boards {
		return fmt.Errorf("%d of %d hash board(s) hashing: the probe needs every board", alive, model.Boards)
	}
	for _, h := range d.Health {
		if h.Chips >= 0 && h.Expected > 0 && h.Chips < h.Expected {
			return fmt.Errorf("board %d found %d of %d chips at start-up: the probe would judge a short board", h.ID, h.Chips, h.Expected)
		}
	}
	if t, _, _ := maxBoardTemp(d); t >= ceilAbortBox2C() {
		return fmt.Errorf("the board is already at %.1f°C (the probe stops at %.0f°C): cool it first", t, ceilAbortBox2C())
	}
	return nil
}

// ceilAbortBox2C: the probe's heat abort: the model's default abort (82 °C on the Box II: the panel's heat guard,
// 4 °C under Goldshell's 86 °C cut-off), two readings in a row; AbortMaxC (84) stops it at once.
func ceilAbortBox2C() float64 { return min(ceilAbortC, model.AbortDefC) }

// ceilReading: the counters the guards compare against.
type ceilReading struct {
	boards map[int]board
	health map[int]boardHealth
}

func ceilRead(d dashboard) ceilReading {
	r := ceilReading{boards: map[int]board{}, health: map[int]boardHealth{}}
	for _, b := range d.Boards {
		r.boards[b.ID] = b
	}
	for _, h := range d.Health {
		r.health[h.ID] = h
	}
	return r
}

func healthProblems(h boardHealth) int { return h.AddrFail24 + h.InitFail24 + h.Reinit24 + h.Dropped24 }

// ceilGuards holds the per-run guard state (consecutive hot / no-temperature readings).
type ceilGuards struct {
	start ceilReading // when the probe started (on the stock plan)
	hot   int
	noT   int
}

// check judges one reading taken while a step is applied; step is the reading taken as the step began.
// It returns why the step fails ("" = fine so far).
func (g *ceilGuards) check(d dashboard, step ceilReading) string {
	t, _, okT := maxBoardTemp(d)
	if !d.OK || !okT {
		if g.noT++; g.noT >= ceilNoTempN {
			return fmt.Sprintf("no board temperature for %d readings in a row (the probe never runs blind)", g.noT)
		}
		return ""
	}
	g.noT = 0
	if t >= model.AbortMaxC {
		return fmt.Sprintf("heat: the board reached %.1f°C (stops at once from %.0f°C)", t, model.AbortMaxC)
	}
	if t >= ceilAbortBox2C() {
		if g.hot++; g.hot >= 2 {
			return fmt.Sprintf("heat: the board read %.1f°C twice in a row (abort at %.0f°C)", t, ceilAbortBox2C())
		}
	} else {
		g.hot = 0
	}
	now := ceilRead(d)
	for id, b0 := range g.start.boards {
		b, ok := now.boards[id]
		if !ok || !b.Alive || !b.Enabled {
			return fmt.Sprintf("dropout: board %d stopped hashing", id)
		}
		if b0.Alive && b.Overheat {
			return fmt.Sprintf("heat: board %d reports overheat", id)
		}
	}
	for id, h0 := range g.start.health {
		h, ok := now.health[id]
		if !ok {
			continue
		}
		if healthProblems(h) > healthProblems(h0) {
			return fmt.Sprintf("chip dropout: board %d re-initialised chips or lost results (%d new problem(s) in its log)", id, healthProblems(h)-healthProblems(h0))
		}
		if h0.Chips >= 0 && h.Chips >= 0 && h.Chips < h0.Chips {
			return fmt.Sprintf("chip dropout: board %d now finds %d of %d chips", id, h.Chips, h0.Chips)
		}
	}
	for id, b := range now.boards {
		s, ok := step.boards[id]
		if !ok {
			continue
		}
		dHW, dN := b.HWErrors-s.HWErrors, b.Nonced-s.Nonced
		if dHW < 0 || dN < 0 {
			return fmt.Sprintf("board %d's counters went back to zero: the mining software restarted", id)
		}
		if dHW < ceilHWMinErrs {
			continue
		}
		lim := ceilHWPctMax
		if b0, ok := g.start.boards[id]; ok && 3*b0.HWPct > lim {
			lim = 3 * b0.HWPct
		}
		if dN == 0 {
			return fmt.Sprintf("hardware errors: board %d made %d HW errors and no good nonces in this step", id, dHW)
		}
		if pct := 100 * float64(dHW) / float64(dHW+dN); pct > lim {
			return fmt.Sprintf("hardware errors: board %d at %.2f%% in this step (%d errors; the limit is %.2f%%)", id, pct, dHW, lim)
		}
	}
	return ""
}

// ceilMarkTesting notes the step on the miner in the restart marker, so a panel killed mid-step says which.
func ceilMarkTesting(p powerplan) {
	b, err := os.ReadFile(tunerMarkerPath())
	if err != nil {
		return
	}
	var m tunerMarker
	if json.Unmarshal(b, &m) != nil {
		return
	}
	m.Testing, m.TestAt = &planMV{MHz: p.FreqMHz, MV: p.VrunMv, PV: p.PVMv}, time.Now().Unix()
	if nb, err := json.Marshal(m); err == nil {
		_ = writeAtomic(tunerMarkerPath(), nb, 0o644)
	}
}

// ceilHeld: the probe may keep writing (Stop, a safety stop, a hold or a re-lock end it).
func ceilHeld() bool {
	return ceilAbort.Load() || getCharge() != "ceiling" || genMoved(ceilGen.Load()) || miningHoldErr() != nil || !ceilAckOK()
}

// runCeilingProbeBox2: steps the clock from the stock window's top in ceilBox2Step steps up to the axis' HardMax,
// keeping the baseline's V code, holding each step ceilBox2Hold while mining; the first step a guard fails
// ends the run. The baseline (the stock Hashrate plan) is put back at the end, whatever happens (ceilFinish).
func runCeilingProbeBox2(base powerplan) {
	defer ceilFinish(base)
	ceilMu.Lock()
	if len(ceilSt.Axes) == 0 {
		ceilMu.Unlock()
		return
	}
	ax := ceilSt.Axes[0]
	ceilSt.Phase = ax.Name
	ceilMu.Unlock()
	ceilSetAxis(0, func(a *ceilingAxis) { a.State = "probing" })

	d0 := ceilDash()
	g := &ceilGuards{start: ceilRead(d0)}
	lastGood, failAt, why := ax.SafeMax, 0, ""
	for v := ax.SafeMax + ax.Step; v-ax.Step < ax.HardMax; v += ax.Step {
		if v > ax.HardMax { // a ceiling off the 10 MHz grid (725) is the last step itself
			v = ax.HardMax
		}
		if ceilHeld() {
			ceilAbort.Store(true)
			ceilLog("stopped (Stop, a safety stop, a mining hold or a re-lock)")
			return
		}
		p := base
		p.FreqMHz = v // the clock only: V code (and fans) stay the baseline's
		ceilSetAxis(0, func(a *ceilingAxis) { a.At = v })
		ceilMarkTesting(p)
		applied, refused, err := ceilApply(p)
		if !applied {
			if !ceilAbort.Load() {
				ceilLog("%d MHz couldn't be written (refused=%v: %v): stopping, no result", v, refused, err)
			}
			ceilAbort.Store(true)
			return
		}
		ceilLog("%d MHz applied (%s): holding %s", v, box2LevelText(p.VrunMv), ceilBox2Hold)
		step := ceilRead(ceilDash())
		until := time.Now().Add(ceilBox2Hold)
		for why == "" {
			if ceilHeld() {
				ceilAbort.Store(true)
				ceilLog("stopped at %d MHz (Stop, a safety stop, a mining hold or a re-lock)", v)
				return
			}
			why = g.check(ceilDash(), step)
			if why != "" || !time.Now().Before(until) {
				break
			}
			time.Sleep(ceilBox2Every)
		}
		if why != "" {
			failAt = v
			ceilLog("%d MHz failed — %s", v, why)
			break
		}
		lastGood = v
		ceilLog("%d MHz held clean", v)
	}
	var note string
	switch {
	case failAt > 0 && lastGood > ax.SafeMax:
		note = fmt.Sprintf("held clean up to %d MHz at the stock voltage; %d MHz failed (%s). Don't set the clock above %d MHz, and test any clock above %d MHz yourself with a long run (TEST-C7)", lastGood, failAt, why, lastGood, ax.SafeMax)
	case failAt > 0:
		note = fmt.Sprintf("the first step past the stock window, %d MHz, failed (%s): this unit has no clock headroom at the stock voltage — keep it at %d MHz", failAt, why, ax.SafeMax)
	default:
		note = fmt.Sprintf("every step up to %d MHz held clean for %s at the stock voltage. A short hold is not a soak: run any clock above %d MHz for hours before you keep it (TEST-C7)", lastGood, ceilBox2Hold, ax.SafeMax)
	}
	ceilSetAxis(0, func(a *ceilingAxis) { a.State = "done"; a.Cap = lastGood; a.At = 0; a.Note = note })
	ceilLog("result: %s", note)
}
