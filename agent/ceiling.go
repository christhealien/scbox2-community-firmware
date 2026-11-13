package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// Firmware-ceiling probe (clamp-detection only). For each axis it steps the requested value up from
// just above the safe window toward the hard ceiling, applies it live for a couple of seconds, then
// reads back what the hardware actually reports (cgminer devs clock/voltage) or what the firmware
// stored (/mcb/setting for PV). When the reported value stops following the request, the firmware/
// regulator has clamped — that value is the real ceiling for that axis. The baseline plan is
// restored after every single step, so a high setting is never left running. This finds the
// firmware cap, NOT the stable-hashing limit (that is thermal/silicon and must be found by a held
// run). The probe takes the in-charge lock so Controls/Fan can't fight it, and aborts on heat.

type ceilingAxis struct {
	Name    string `json:"name"`     // "freq" | "vrun" | "pv"
	Label   string `json:"label"`    // human label
	Unit    string `json:"unit"`     // "MHz" | "mV"
	SafeMax int    `json:"safe_max"` // top of the conservative window
	HardMax int    `json:"hard_max"` // probe stops here
	Step    int    `json:"step"`
	State   string `json:"state"`          // "todo" | "probing" | "done"
	Cap     int    `json:"cap"`            // highest requested value that applied unclamped
	Clamped bool   `json:"clamped"`        // a clamp was observed
	ClampAt int    `json:"clamp_at"`       // requested value where the clamp first appeared
	Actual  int    `json:"actual"`         // value the hardware reported at the clamp
	Note    string `json:"note"`           // summary line
	At      int    `json:"at,omitempty"`   // current requested value while probing
	Held    bool   `json:"held,omitempty"` // Box II: each step was held while mining (Cap = highest clean step), not a clamp probe
}

type ceilingState struct {
	Running   bool          `json:"running"`
	Done      bool          `json:"done"`
	Phase     string        `json:"phase"`
	Axes      []ceilingAxis `json:"axes"`
	Log       []string      `json:"log"`
	StartedAt string        `json:"started_at,omitempty"`
}

var (
	ceilMu    sync.Mutex
	ceilSt    ceilingState
	ceilAbort atomic.Bool  // set by the Stop button, the pool guard and the probe; read by the probe
	ceilGen   atomic.Int64 // safetyGen as the probe started: its writes are refused after a safety stop or Idle
)

func init() { ceilGen.Store(noGen) } // (a probe not started by handleCeiling: no note)

// ceilMarkerPlan: the restart marker's Plan for the limit probe (tunerRecover puts its baseline back).
const ceilMarkerPlan = "limit probe"

const ceilAbortC = 83.0 // stop the probe if any board reaches this while a probe value is applied

func ceilSnapshot() ceilingState {
	ceilMu.Lock()
	defer ceilMu.Unlock()
	cp := ceilSt
	cp.Axes = append([]ceilingAxis(nil), ceilSt.Axes...)
	cp.Log = append([]string(nil), ceilSt.Log...)
	return cp
}

func ceilLog(format string, a ...any) {
	line := time.Now().Format("15:04:05") + "  " + fmt.Sprintf(format, a...)
	ceilMu.Lock()
	ceilSt.Log = append(ceilSt.Log, line)
	if len(ceilSt.Log) > 300 {
		ceilSt.Log = ceilSt.Log[len(ceilSt.Log)-300:]
	}
	ceilMu.Unlock()
	activity.emit("📐 ceiling: " + fmt.Sprintf(format, a...))
}

func ceilSetAxis(i int, mut func(*ceilingAxis)) {
	ceilMu.Lock()
	if i >= 0 && i < len(ceilSt.Axes) {
		mut(&ceilSt.Axes[i])
	}
	ceilMu.Unlock()
}

// storedPlan reads the powerplan the firmware currently has stored for the manual plan. Rail-voltage
// and live-clock readback from the miner are unreliable (rail voltage is offset from the setpoint;
// the PLL re-locks on its own schedule), so the firmware's own stored value is the clamp signal — it
// reflects a clamp only if the firmware caps what it stores; if it stores verbatim, the probe
// honestly reports "no clamp".
func storedPlan() (powerplan, bool) {
	raw, err := stockGet("/mcb/setting")
	if err != nil {
		return powerplan{}, false
	}
	var cur map[string]any
	if json.Unmarshal(raw, &cur) != nil {
		return powerplan{}, false
	}
	if s, _ := cur["manualPowerplan"].(string); s != "" {
		return parsePowerplan(s)
	}
	return powerplan{}, false
}

func ceilTooHot() (float64, bool) {
	d := buildDashboard()
	if !d.OK {
		return 0, false
	}
	if mt, _, ok := maxBoardTemp(d); ok && mt >= ceilAbortC {
		return mt, true
	}
	return 0, false
}

// ceilApply writes a plan, retrying a few times so a transient API hiccup is not mistaken for a
// firmware rejection. ok: written. refused: the OEM's backend answered the write and turned it down (the
// only failure that says something about its cap); any other failure (no answer, the panel's own window or
// holds) is no result.
func ceilApply(p powerplan) (ok, refused bool, err error) {
	for i := 0; i < 3; i++ {
		// the pool guard, the fan lock, the thermal guard's Idle or a re-lock: the probe ends, it's not a firmware cap
		if miningHoldErr() != nil || getCharge() != "ceiling" || !ceilAckOK() {
			ceilAbort.Store(true)
			return false, false, nil
		}
		_, err = ceilMutate(ceilGen.Load(), func(q *powerplan) { q.FreqMHz = p.FreqMHz; q.VrunMv = p.VrunMv; q.PVMv = p.PVMv })
		if err == nil {
			return true, false, nil
		} else if err == errSafetyStop { // put on Idle after the check above (the charge check can't see that)
			ceilAbort.Store(true)
			return false, false, nil
		}
		var pr putRefusedErr
		refused = errors.As(err, &pr)
		time.Sleep(800 * time.Millisecond)
	}
	return false, refused, err
}

// storedPlanRetry reads the stored plan, retrying past transient read failures.
func storedPlanRetry(n int) (powerplan, bool) {
	for i := 0; i < n; i++ {
		if pp, ok := storedPlan(); ok && pp.FreqMHz > 0 {
			return pp, true
		}
		time.Sleep(700 * time.Millisecond)
	}
	return powerplan{}, false
}

// runCeilingProbe performs the clamp-detection sweep. base is the plan restored after each step.
func runCeilingProbe(base powerplan) {
	if model.Plan == planBox2V {
		runCeilingProbeBox2(base) // held steps while mining (ceilingbox2.go)
		return
	}
	defer ceilFinish(base)

	tol := map[string]int{"freq": 12, "vrun": 10, "pv": 10}

	for i := range ceilSt.Axes {
		if ceilAbort.Load() {
			return
		}
		ax := ceilSt.Axes[i]
		ceilMu.Lock()
		ceilSt.Phase = ax.Name
		ceilMu.Unlock()
		ceilSetAxis(i, func(a *ceilingAxis) { a.State = "probing" })
		ceilLog("%s: probing %d→%d %s in %d %s steps", ax.Label, ax.SafeMax+ax.Step, ax.HardMax, ax.Unit, ax.Step, ax.Unit)

		lastGood := ax.SafeMax
		clamped := false
		// All axes are detected from the firmware's stored plan: after a write, the stored value
		// reflects a clamp only if the firmware actually caps what it stores. The live clock looked
		// tempting for frequency, but its re-lock timing is too unpredictable to tell "hasn't moved
		// yet" from "clamped", so we rely on the stored plan everywhere and restore after each step.
		for v := ax.SafeMax + ax.Step; v <= ax.HardMax; v += ax.Step {
			if ceilAbort.Load() {
				ceilLog("aborted")
				return
			}
			// build the probe plan from baseline, moving only this axis; keep PV ≥ run
			p := base
			switch ax.Name {
			case "freq":
				p.FreqMHz = v
			case "vrun":
				p.VrunMv = v
				if p.PVMv < v {
					p.PVMv = v
				}
			case "pv":
				p.PVMv = v
			}
			ceilSetAxis(i, func(a *ceilingAxis) { a.At = v })
			applied, refused, aerr := ceilApply(p)
			if !applied && ceilAbort.Load() {
				ceilLog("aborted: mining is held (pool guard, fan lock, thermal guard) or the clock limit was re-locked")
				return
			}
			if !applied && !refused { // no answer, or the panel's own refusal: says nothing about the OEM's cap
				ceilLog("%s: %d %s couldn't be written (%v): stopping, no result", ax.Label, v, ax.Unit, aerr)
				ceilAbort.Store(true)
				return
			}
			if !applied {
				// firmware refused the write (after retries) → clamp is just below this value
				clamped = true
				ceilSetAxis(i, func(a *ceilingAxis) { a.Clamped = true; a.ClampAt = v; a.Actual = lastGood })
				ceilLog("%s: firmware rejected %d %s (3 tries) — cap is %d %s", ax.Label, v, ax.Unit, lastGood, ax.Unit)
				break
			}

			// read back what the firmware stored, then restore baseline before deciding
			var got int
			var ok bool
			if sp, o := storedPlanRetry(3); o {
				switch ax.Name {
				case "freq":
					got, ok = sp.FreqMHz, true
				case "vrun":
					got, ok = sp.VrunMv, true
				default:
					got, ok = sp.PVMv, true
				}
			}
			ceilApply(base) // restore immediately, never leave a high setting running

			if mt, hot := ceilTooHot(); hot {
				ceilLog("%s: aborting — board hit %.1f°C", ax.Label, mt)
				ceilAbort.Store(true)
				return
			}

			if ok && got < v-tol[ax.Name] {
				clamped = true
				ceilSetAxis(i, func(a *ceilingAxis) { a.Clamped = true; a.ClampAt = v; a.Actual = got })
				ceilLog("%s: requested %d %s but miner reported %d %s → clamped (cap ≈ %d)", ax.Label, v, ax.Unit, got, ax.Unit, got)
				break
			}
			lastGood = v
			if ok {
				ceilLog("%s: %d %s accepted (read back %d)", ax.Label, v, ax.Unit, got)
			} else {
				ceilLog("%s: %d %s applied (no readback for this axis)", ax.Label, v, ax.Unit)
			}
		}
		ceilApply(base) // back to baseline before the next axis

		note := ""
		if clamped {
			note = fmt.Sprintf("firmware cap ≈ %d %s", ceilSt.Axes[i].Actual, ax.Unit)
			if ceilSt.Axes[i].Actual == 0 {
				note = fmt.Sprintf("rejected above %d %s", lastGood, ax.Unit)
			}
		} else {
			note = fmt.Sprintf("%s firmware accepts every setting up to %d %s: it doesn't cap them%s. That says nothing about whether the chips can run there: each step was held only for a moment and nothing was mined at it; voltage, heat and the chips set the real limit", OemOf(), ax.HardMax, ax.Unit,
				map[bool]string{true: fmt.Sprintf(" (the probe stopped at the %d %s window you tightened to; re-lock and unlock to probe up to %d)", ax.HardMax, ax.Unit, freqHardMax)}[ax.Name == "freq" && ax.HardMax < freqHardMax])
		}
		ceilSetAxis(i, func(a *ceilingAxis) { a.State = "done"; a.Cap = lastGood; a.Note = note; a.At = 0 })
		ceilLog("%s: %s", ax.Label, note)
	}
}

// ceilFinish ends a probe run (deferred by both probes): always put the miner back exactly where it started.
func ceilFinish(base powerplan) {
	// always put the miner back exactly where it started
	// the thermal guard (heat, no temperature), the pool guard or the fan lock took the miner (charge no longer
	// "ceiling"): it stays where they put it
	restored := getCharge() != "ceiling" || genMoved(ceilGen.Load())
	for try := 0; try < 6 && !restored; try++ { // the miner may be mid-restart after a refused step
		if try > 0 {
			time.Sleep(10 * time.Second)
		}
		if _, err := ceilMutate(ceilGen.Load(), func(p *powerplan) { p.FreqMHz = base.FreqMHz; p.VrunMv = base.VrunMv; p.PVMv = base.PVMv }); err == errSafetyStop {
			restored = true // put on Idle meanwhile: it stays there
		} else if err == nil {
			if model.Plan == planBox2V { // no PV here: the clock and the voltage level are what was put back
				if cur, ok := readStockBaseline("current"); ok && cur.FreqMHz == base.FreqMHz && cur.VrunMv == base.VrunMv {
					restored = true
				}
			} else {
				wantPV := min(max(base.PVMv, pvHardMin), pvHardMax) // what mutatePlan sends for it
				if cur, ok := readStockBaseline("current"); ok && cur.FreqMHz == base.FreqMHz && cur.PVMv == wantPV {
					restored = true
				}
			}
		}
	}
	if !restored && miningHoldErr() == nil { // never leave it on a probe step: the OEM's own plan
		if err := applyStockPlanGen(ceilGen.Load()); err != nil {
			ceilLog("couldn't put the starting setting back, and %s stock plan failed too: %v", oemOf(), err)
			activity.emit("⚠ limit probe: the starting setting couldn't be put back and " + oemOf() + " stock plan failed (" + err.Error() + "): apply " + stockWord() + " or a preset")
		} else {
			ceilLog("couldn't put the starting setting back: applied %s stock plan instead", oemOf())
			activity.emit("⚠ limit probe: the starting setting couldn't be put back, so " + oemOf() + " stock plan was applied")
		}
	}
	clearTunerMarker()        // (put back, or left where a safety stop put it: nothing for the next start to do)
	swapCharge("ceiling", "") // not over the Idle a safety stop or the owner set meanwhile
	ceilRiskOK.Store(false)   // (Box II) the probe's own window closes with it
	ceilGen.Store(noGen)
	ceilMu.Lock()
	for i := range ceilSt.Axes { // stopped early (Stop, heat, a hold): not a finished axis
		if ax := &ceilSt.Axes[i]; ax.State != "done" {
			ax.State, ax.At, ax.Note = "aborted", 0, "stopped before the end: no result"
		}
	}
	ceilSt.Running = false
	ceilSt.Done = true
	ceilSt.Phase = "done"
	ceilMu.Unlock()
	if model.Plan == planBox2V {
		ceilLog("done — baseline %d MHz / %s (stock) restored", base.FreqMHz, box2LevelText(base.VrunMv))
		return
	}
	ceilLog("done — baseline %d MHz / %d mV / PV %d restored", base.FreqMHz, base.VrunMv, base.PVMv)
}

// handleCeiling: GET status; POST {start:true} launches the probe, {stop:true} aborts it.
func handleCeiling(w http.ResponseWriter, r *http.Request) {
	if !featGate(w, model.Feat.CeilingProbe, "the limit probe") {
		return
	}
	if r.Method == http.MethodPost {
		var in struct {
			Start bool `json:"start"`
			Stop  bool `json:"stop"`
			// AcceptRisk (Box II): the owner accepted, for this run, that the probe steps past the stock clock
			// window (the overclock unlock may not be commissioned yet: the probe is what proves it)
			AcceptRisk bool `json:"accept_risk"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeJSON(w, map[string]any{"ok": false, "err": "bad request"})
			return
		}
		if in.Stop {
			ceilAbort.Store(true)
			writeJSON(w, map[string]any{"ok": true, "note": "stopping"})
			return
		}
		if !in.Start {
			writeJSON(w, map[string]any{"ok": false, "err": "nothing to do"})
			return
		}
		box2 := model.Plan == planBox2V
		if box2 {
			if err := ceilBox2StartErr(in.AcceptRisk); err != nil {
				writeJSON(w, map[string]any{"ok": false, "err": err.Error()})
				return
			}
		} else if !loadOverclock().Acknowledged {
			writeJSON(w, map[string]any{"ok": false, "err": "unlock the limits first (accept the disclaimer)"})
			return
		}
		controlMu.Lock() // the charge and strong-test checks and taking charge, in one go (see controlMu)
		defer controlMu.Unlock()
		gen := safetyGen.Load() // before the checks: a safety stop from here on wins over the probe (see safetyGen)
		ceilMu.Lock()
		running := ceilSt.Running
		ceilMu.Unlock()
		if running {
			writeJSON(w, map[string]any{"ok": false, "err": "a probe is already running"})
			return
		}
		if lbl := chargeText(); lbl != "" {
			writeJSON(w, map[string]any{"ok": false, "err": "locked — " + lbl + " is in charge. Take manual control first."})
			return
		}
		if sctRunning() {
			writeJSON(w, map[string]any{"ok": false, "err": "the strong-chip test is running (Controls → Chip trims): stop it first"})
			return
		}
		probeHi := effHard("freq", freqHardMax)
		if box2 {
			probeHi = ceilBox2Hi()
		}
		if err := plusTrimErr(probeHi); err != nil {
			// its clock steps would be refused for the "+" chip, and each refusal read as the OEM's cap
			writeJSON(w, map[string]any{"ok": false, "err": "a chip is trimmed faster than its board, so the clock steps would stop early (" + err.Error() + "): set it to 0 or switch chip trims off for the probe"})
			return
		}
		// snapshot the current running plan to restore after each step
		base, ok := readStockBaseline("current")
		if !ok || base.FreqMHz == 0 { // minerd may be mid-restart after a plan change: one more look
			time.Sleep(2 * time.Second)
			base, ok = readStockBaseline("current")
		}
		if !ok || base.FreqMHz == 0 {
			writeJSON(w, map[string]any{"ok": false, "err": "could not read the current plan to use as a baseline"})
			return
		}
		if box2 {
			if err := ceilBox2BaseErr(base); err != nil {
				writeJSON(w, map[string]any{"ok": false, "err": err.Error()})
				return
			}
		}
		// on disk first, as a tuning run's marker: a panel killed (a crash, a power cut, an update) while a step
		// past the window is applied leaves it stored in the firmware; the next start puts the baseline back
		mk, _ := json.Marshal(tunerMarker{Started: time.Now().Unix(), Base: planMV{MHz: base.FreqMHz, MV: base.VrunMv, PV: base.PVMv},
			Plan: ceilMarkerPlan, Trims: readChipTrims(), KeptSet: true})
		if err := writeAtomic(tunerMarkerPath(), mk, 0o644); err != nil {
			writeJSON(w, map[string]any{"ok": false, "err": "couldn't save the probe's restart record: " + err.Error()})
			return
		}
		if !setChargeGen(gen, "ceiling") {
			clearTunerMarker()
			writeJSON(w, map[string]any{"ok": false, "err": errSafetyStop.Error()})
			return
		}
		ceilGen.Store(gen)
		ceilAbort.Store(false)
		if box2 {
			ceilRiskOK.Store(true) // the probe's own window (ceilBox2Window) opens while it holds the charge
			ceilMu.Lock()
			ceilSt = ceilingState{Running: true, Phase: "starting", StartedAt: time.Now().Format("2006-01-02 15:04"),
				Axes: []ceilingAxis{{Name: "freq", Label: "Frequency", Unit: "MHz", SafeMax: freqMax, HardMax: probeHi,
					Step: ceilBox2Step, State: "todo", Held: true}}}
			ceilMu.Unlock()
			ceilLog("start — baseline %d MHz / %s (stock, kept for every step); steps of %d MHz to %d MHz, each held %s while mining; back to the baseline at the end",
				base.FreqMHz, box2LevelText(base.VrunMv), ceilBox2Step, probeHi, ceilBox2Hold)
			go runCeilingProbe(base)
			writeJSON(w, map[string]any{"ok": true, "note": "probe started"})
			return
		}
		ceilMu.Lock()
		ceilSt = ceilingState{
			Running: true, Phase: "starting", StartedAt: time.Now().Format("2006-01-02 15:04"),
			Axes: []ceilingAxis{
				{Name: "freq", Label: "Frequency", Unit: "MHz", SafeMax: freqMax, HardMax: clockWindowHi(), Step: 25, State: "todo"},
				// no voltage axes: PV's range is measured (the supply gives 9.1–9.6 V and ignores anything
				// higher), and run voltage doesn't change the boards' voltage at all
			},
		}
		ceilMu.Unlock()
		ceilLog("start — baseline %d MHz / %d mV / PV %d (restored after every step)", base.FreqMHz, base.VrunMv, base.PVMv)
		go runCeilingProbe(base)
		writeJSON(w, map[string]any{"ok": true, "note": "probe started"})
		return
	}
	s := ceilSnapshot()
	writeJSON(w, map[string]any{"ok": true, "state": s, "unlocked": ocUnlocked()})
}
