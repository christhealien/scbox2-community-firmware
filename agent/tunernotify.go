package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Tuning notifications (one switch, "Tuning"): a run started, finished, was stopped by hand, aborted, or
// was cut short by a restart.

func runDur(d time.Duration) string {
	d = d.Round(time.Minute)
	if d < time.Hour {
		return fmt.Sprintf("%d min", int(d.Minutes()))
	}
	if d%time.Hour == 0 {
		return fmt.Sprintf("%d h", int(d.Hours()))
	}
	return fmt.Sprintf("%d h %d min", int(d.Hours()), int(d.Minutes())%60)
}

func (c tunerConfig) planText() string {
	var s string
	switch c.Mode {
	case "confirm":
		s = fmt.Sprintf("confirming %d MHz · %d mV for %g h", c.ConfirmMHz, c.ConfirmMV, c.ConfirmHours)
		if tunerOnBox2() {
			s = fmt.Sprintf("confirming %d MHz · %s for %g h", c.ConfirmMHz, c.vText(c.ConfirmMV), c.ConfirmHours)
		}
	case "rebuild":
		s = fmt.Sprintf("building presets from the last curve, %g min each", c.PresetMin)
	default:
		if n := len(c.clockList()); n == 1 {
			s = fmt.Sprintf("mapping %d MHz, %g min per test", c.LowestMHz, c.HoldMin)
		} else {
			s = fmt.Sprintf("mapping %d clocks, %d–%d MHz in %g MHz steps, %g min per test", n, c.LowestMHz, c.HighestMHz, c.Step, c.HoldMin)
		}
		var extra []string
		if c.ChipTrim && !c.DryRun {
			extra = append(extra, "chip trims on")
		}
		if c.BuildPresets && c.Mode != "rebuild" {
			extra = append(extra, "presets after")
		}
		if len(extra) > 0 {
			s += ", " + strings.Join(extra, ", ")
		}
	}
	if c.DryRun {
		s += " (dry run: nothing is changed)"
	}
	return s
}

// startText: "tuning run started: …"
func (e *tunerEngine) startText() string {
	e.mu.Lock()
	base, backWhat := e.baseUsed, e.backWhat
	e.mu.Unlock()
	s := "tuning run started: " + e.cfg.planText()
	if backWhat != "" {
		s += "; back to " + backWhat + " if it stops early"
	} else if base.MHz > 0 {
		s += "; back to " + base.text() + " if it stops early"
	}
	return s
}

// noteAbort keeps the first reason a run (or a phase of it) ended early on its own; a stop by hand isn't one.
func (e *tunerEngine) noteAbort(why string) {
	if e.stopped() {
		return
	}
	e.mu.Lock()
	if e.abortWhy == "" {
		e.abortWhy = why
	}
	e.mu.Unlock()
}

// endText: how the run ended, for the notification. The first word says which:
// "tuning run finished…", "tuning run stopped by hand…", "tuning run aborted…".
func (e *tunerEngine) endText() string {
	e.mu.Lock()
	took := e.finishedAt.Sub(e.startedAt)
	if e.finishedAt.IsZero() {
		took = e.now().Sub(e.startedAt)
	}
	why, best, base, confirmed, trims, undone := e.abortWhy, e.best, e.baseUsed, e.confirmed, len(e.runTrims), e.trimsUndone
	backWhat, held, by := e.backWhat, e.endHeld, e.stopWhy
	e.mu.Unlock()
	dry := ""
	if e.cfg.DryRun {
		dry = " (dry run)"
	}
	var s string
	if backWhat == "" {
		backWhat = base.text()
	}
	// where the end left the miner (see Run's end): on Idle for a hold, not put back, or back on what it ran before
	back := "the miner is back on " + backWhat
	switch {
	case held != "":
		back = "the miner stays on Idle (" + held + ")"
	case e.restoreFailed:
		back = "the miner couldn't be put back on " + backWhat + ": apply " + stockWord() + " or a preset (Presets tab)"
	}
	switch {
	case e.stopped():
		who := "by hand"
		if by != "" {
			who = "by " + by
		}
		if confirmed && best != nil && !e.cfg.DryRun && held == "" && !e.restoreFailed {
			back = "the miner stays on the confirmed " + best.text()
		}
		s = fmt.Sprintf("tuning run stopped %s%s after %s; %s", who, dry, runDur(took), back)
	case why != "":
		s = fmt.Sprintf("tuning run aborted%s after %s: %s", dry, runDur(took), strings.TrimSpace(why))
		if !confirmed {
			s += "; " + back
		}
	default:
		s = fmt.Sprintf("tuning run finished%s in %s", dry, runDur(took))
		switch {
		case e.cfg.Mode == "confirm" && confirmed:
			s += " · confirmed " + best.textOr(e.cfg) + ", the miner stays on it"
		case e.cfg.Mode == "confirm":
			s += " · the setting didn't hold; " + back
		case e.cfg.Mode == "rebuild":
			s += " · presets rebuilt (see Presets)"
		case best == nil:
			s += " · no clean setting found; " + back
		case confirmed:
			s += " · best " + best.text() + ", confirmed: the miner stays on it"
		default:
			s += " · best " + best.text()
		}
	}
	if trims > 0 {
		s += fmt.Sprintf(" · %d chip trim(s) kept (Controls → Chip trims)", trims)
	}
	if undone > 0 {
		s += fmt.Sprintf(" · the %d chip trim(s) it made came off again", undone)
	}
	return s
}

func (p *planMV) textOr(c tunerConfig) string {
	if p != nil {
		return p.text()
	}
	if tunerOnBox2() {
		return fmt.Sprintf("%d MHz · %s", c.ConfirmMHz, c.vText(c.ConfirmMV))
	}
	return fmt.Sprintf("%d MHz · %d mV", c.ConfirmMHz, c.ConfirmMV)
}

// ---- a run cut short by a restart (power cut, reboot, panel crash) ----
// The tuner's test settings are saved in the firmware, so a restart mid-run would leave the miner on a test
// clock and voltage. A marker written at the start (removed at the end) lets the next start put it back.

type tunerMarker struct {
	Started int64      `json:"started"`
	Base    planMV     `json:"base"`
	Dry     bool       `json:"dry"`
	Plan    string     `json:"plan"`
	Trims   []chipTrim `json:"trims_before"`
	Testing *planMV    `json:"testing,omitempty"` // the setting on the miner when the marker was last written
	TestAt  int64      `json:"testing_at,omitempty"`
	Ended   int64      `json:"ended,omitempty"` // the run ended but couldn't put the miner back (the marker was kept)
	// TrimsLeft: the run ended (Ended) but some of its chip trims couldn't be taken off: the next start does that
	// part (its clock and voltage only if the miner is still on the run's last test setting, as for any Ended)
	TrimsLeft bool `json:"trims_left,omitempty"`
	// the trims the run has kept so far (each made a failing test pass); a trim in the file that's neither
	// this nor from before the run was being tried when the run was cut short
	Kept    []chipTrim `json:"kept,omitempty"`
	KeptSet bool       `json:"kept_set,omitempty"`
}

// markTunerTrims notes the run's kept trims in the restart marker.
func markTunerTrims(kept map[[2]int]float64) {
	b, err := os.ReadFile(tunerMarkerPath())
	if err != nil {
		return
	}
	var m tunerMarker
	if json.Unmarshal(b, &m) != nil {
		return
	}
	m.Kept, m.KeptSet = trimList(kept), true
	if nb, err := json.Marshal(m); err == nil {
		writeAtomic(tunerMarkerPath(), nb, 0o644)
	}
}

// markTunerTesting notes the setting just applied in the restart marker, so a run cut short by a freeze
// or a power cut can say what was under test (a control board that froze at one setting, as SClite-1 did
// at 675 MHz, may freeze there again).
func markTunerTesting(p planMV) {
	b, err := os.ReadFile(tunerMarkerPath())
	if err != nil {
		return
	}
	var m tunerMarker
	if json.Unmarshal(b, &m) != nil {
		return
	}
	m.Testing, m.TestAt = &p, time.Now().Unix()
	if nb, err := json.Marshal(m); err == nil {
		writeAtomic(tunerMarkerPath(), nb, 0o644) // (flushed before the rename: a power cut can't leave it empty)
	}
}

// markTunerEnded: the run is over but its put-back failed; the marker stays for the next start, which puts the
// baseline back only if nothing else was chosen since.
func markTunerEnded() { markTunerEndedTrims(false) }

// markTunerEndedTrims: markTunerEnded; trimsLeft: some of the run's trims are still on (the next start takes them off).
func markTunerEndedTrims(trimsLeft bool) {
	b, err := os.ReadFile(tunerMarkerPath())
	if err != nil {
		return
	}
	var m tunerMarker
	if json.Unmarshal(b, &m) != nil {
		return
	}
	m.Ended = time.Now().Unix()
	m.TrimsLeft = m.TrimsLeft || trimsLeft
	if nb, err := json.Marshal(m); err == nil {
		writeAtomic(tunerMarkerPath(), nb, 0o644)
	}
}

// fsErr: a file error without its paths ("no space left on device", "read-only file system").
func fsErr(err error) string {
	var pe *os.PathError
	var le *os.LinkError
	if errors.As(err, &le) {
		err = le.Err
	} else if errors.As(err, &pe) {
		err = pe.Err
	}
	return err.Error()
}

func tunerMarkerPath() string { return filepath.Join(filepath.Dir(*authFile), "tuning.running") }

func writeTunerMarker(e *tunerEngine) error {
	e.mu.Lock()
	m := tunerMarker{Started: e.startedAt.Unix(), Base: e.baseUsed, Dry: e.cfg.DryRun, Plan: e.cfg.planText()}
	e.mu.Unlock()
	m.Trims = readChipTrims()
	m.KeptSet = true // (none kept yet)
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return writeAtomic(tunerMarkerPath(), b, 0o644) // a power cut mid-write mustn't leave a marker the next start can't read
}

func clearTunerMarker() { os.Remove(tunerMarkerPath()) }

// tunerTrimsBack: every chip trim back to what it was when the run started (the marker's record).
func tunerTrimsBack(trims []chipTrim, set func(chain, chip int, mhz float64) error) (undone, failed int) {
	before := map[[2]int]float64{}
	for _, t := range trims {
		before[[2]int{t.Chain, t.Chip}] = t.MHz
	}
	now := map[[2]int]float64{}
	for _, t := range readChipTrims() {
		now[[2]int{t.Chain, t.Chip}] = t.MHz
	}
	for k, v := range now {
		if b := before[k]; b != v {
			if set(k[0], k[1], b) == nil {
				undone++
			} else {
				failed++
			}
		}
	}
	for k, b := range before {
		if _, ok := now[k]; !ok && b != 0 {
			if set(k[0], k[1], b) == nil {
				undone++
			} else {
				failed++
			}
		}
	}
	return undone, failed
}

// tunerRecover: at start, a marker means the last run never ended. Put the miner back on the run's
// baseline, hand the trims it made to auto-trim (as a stop would), and say so.
var (
	tunerRecoverApply = applyTunePlan
	tunerRecoverRetry = 15 * time.Second
)

func tunerRecover(wait time.Duration) {
	if featOff(model.Feat.Tuner) && featOff(model.Feat.CeilingProbe) {
		// neither the tuner nor the limit probe can run on this model, so no restart marker is ever written
		// and there is nothing to recover: the background recovery never does anything.
		return
	}
	b, err := os.ReadFile(tunerMarkerPath())
	if err != nil {
		return
	}
	var m tunerMarker
	if json.Unmarshal(b, &m) != nil {
		clearTunerMarker()
		msg := "a tuning run was cut short by a restart, and its record can't be read: the miner may still be on a test setting. Apply " + stockWord() + " or a preset (Presets tab), and check Controls → Chip trims"
		activity.emit("🔧 " + msg)
		notify(evTuning, msg)
		return
	}
	started := time.Unix(m.Started, 0).Format("Jan 2 15:04")
	what := "tuning run"
	if m.Plan == ceilMarkerPlan {
		what = "limit probe"
	}
	// a setting of the record as the owner reads it: the limit probe's record holds Goldshell's level as the miner
	// was given it, the tuner's its own rank (text() converts that one)
	markText := func(p planMV) string {
		if tunerOnBox2() && m.Plan == ceilMarkerPlan {
			return fmt.Sprintf("%d MHz / %s", p.MHz, box2LevelText(p.MV))
		}
		return p.text()
	}
	msg := fmt.Sprintf("%s cut short: the miner restarted during it (started %s)", what, started)
	if m.Testing != nil && m.Testing.MHz > 0 {
		msg += fmt.Sprintf(", while testing %s (applied %s)", markText(*m.Testing), time.Unix(m.TestAt, 0).Format("15:04"))
		if m.Testing.MHz > m.Base.MHz || m.Testing.PV > m.Base.PV {
			msg += ": if nobody cut the power, treat that setting as unsafe on this miner"
		}
	}
	if !m.Dry && m.Base.MHz > 0 {
		// the run was cut short, so everything goes back to what it was before it: the clock and voltage below,
		// and every chip trim the run made or changed (first, so the baseline applies the trims file without them)
		// (a run that ended already dealt with its trims, undone or kept and handed to auto-trim, and the owner
		// may have set trims since: only a run cut short has trims to put back)
		undone, failed := 0, 0
		if m.Ended == 0 || m.TrimsLeft {
			undone, failed = tunerTrimsBack(m.Trims, cwSetTrim)
		}
		if undone > 0 {
			msg += fmt.Sprintf(" · %d chip trim(s) the run made put back as they were before it", undone)
		}
		if failed > 0 {
			msg += fmt.Sprintf(" · %d chip trim(s) couldn't be put back (check Controls → Chip trims)", failed)
		}
		// the miner's web API answers a while after boot: keep trying
		var aerr error
		gen := safetyGen.Load() // an Idle (the owner's, a safety stop's) taken while this retries stays
		for t0 := time.Now(); ; {
			if heatIdleHeld() { // the thermal guard's Idle (kept over the restart, or taken meanwhile) stays
				aerr = fmt.Errorf("the thermal guard put it on Idle for heat")
				break
			}
			if genMoved(gen) || getCharge() != "" {
				aerr = fmt.Errorf("something else was chosen meanwhile")
				break
			}
			if m.Ended > 0 {
				// the run ended, its put-back failed, and the panel ran on: only if the miner is still on the
				// run's last test setting (the owner may have chosen Stock, a preset or Idle since)
				cur, ok := readStockBaselineNow("current")
				same := m.Testing != nil && cur.FreqMHz == m.Testing.MHz && cur.PVMv == m.Testing.PV
				if tunerOnBox2() && m.Testing != nil && m.Plan != ceilMarkerPlan { // no PV there: Goldshell's level
					same = cur.FreqMHz == m.Testing.MHz && cur.VrunMv == m.Testing.hw().MV
				}
				if ok && !same {
					aerr = fmt.Errorf("it runs something else since the run ended")
					break
				}
				if !ok {
					aerr = fmt.Errorf("its setting can't be read")
				}
			}
			if aerr == nil {
				base := m.Base
				if m.Plan != ceilMarkerPlan {
					base = base.hw() // the tuner's record is in its own voltage levels (Box II); SC Lite: unchanged
				}
				if aerr = tunerRecoverApply(base); aerr == nil {
					break
				}
			}
			if time.Since(t0) > wait {
				break
			}
			aerr = nil
			time.Sleep(tunerRecoverRetry)
		}
		if aerr == nil {
			msg += "; put back on " + markText(m.Base)
		} else {
			msg += "; couldn't put it back on " + markText(m.Base) + " (" + aerr.Error() + "): set it in Presets"
		}
	}
	clearTunerMarker()
	activity.emit("🔧 " + msg)
	notify(evTuning, msg)
}
