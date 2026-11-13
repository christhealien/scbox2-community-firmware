package main

// Strong-chip test ("Find strong chips"): started by hand for one board from Controls → Chip trims. It looks
// for chips that stay clean when run faster than their board, at the board's own voltage, and keeps each
// such chip at the fastest step it passed, as a hand trim (the tuner and auto-trim then treat it as the
// owner's; auto-trim still steps a "+" chip back toward the board clock if it becomes one of the board's worst).
//
//  1. Baseline (sctBaseline, after sctSettle): every chip's hardware errors (the miner's per-chip counters,
//     the same source auto-trim and the tuner use) and nonces (the hook's counters) at the current setting.
//  2. Candidates: chips with no trim of any kind, that found nonces normally (not > 3 standard deviations
//     below the board's average chip: a chip that does little work makes few errors too) and whose errors were
//     clearly below the board's typical chip: e ≤ L, L the largest count with P(X ≤ L | Poisson(median)) <
//     5 %, and never below 0, so a board whose typical chip made no errors takes only chips with none. Best
//     first (fewest errors, then fewest errors since the miner started, then most nonces), at most one chip per series stage (chips 2k-1, 2k share a stage:
//     raising both would pull the stage's voltage down twice), none beside a chip with a trim, at most sctMaxChips.
//  3. Steps of +12.5 MHz, all remaining candidates together, up to +50 (trimMax). Each step settles
//     (sctSettle, and until the hook reports the new clocks), then is watched for sctStepWin. A chip fails a
//     step when its errors are clearly above its baseline: k errors in the step with k ≥ 2 and
//     P(X ≥ k | Poisson(λ)) < 1 %, λ = max(its baseline errors, the board's mean chip's) scaled to the step's
//     length (at least 0.1). It goes back one step and stays there; the others go on. The count is everything
//     seen while the chip ran at that trim (sctPool), so a chip that stopped rising is judged again at every
//     window and goes back once more when it fails; the other chip of its stage is judged too (sctPartnerChk:
//     a raised chip lowers its partner's voltage; stricter, k ≥ 3 and P < 0.1 %, against the partner's
//     baseline, its share of the board's error history or the mean chip), and its failing fails the raised chip.
//  3b. Confirm: once nothing rises, the kept trims are watched for one more step window (again after any chip
//     stepped back in it, at most sctConfirmMax windows).
//  4. Whole-board guard: the board's errors (all chips but the ones failing this step) clearly up (at least 5,
//     and P < 0.1 % in the exact two-count test: with the rate unchanged, the step's share of baseline + step
//     errors is Binomial(n, step time ÷ both times)), or its nonce rate (the hook's good nonces: the
//     hashrate without pool luck) down more than 2 % and by more than 3 standard errors, or a board temperature
//     more than 5 °C over the baseline's average or at the warm limit (the tuner's abort default − 5): every
//     chip still rising goes back one step and the test ends.
//
// A stop, an abort (the hook stops reporting or the board clock changes for more than sctUnsteadyMax, the
// board drops out, the new clocks don't reach the chips) or a panel restart put every trim back as it was
// before the test (the trims file is backed up first). Lock order: controlMu → chipJobMu → cwMu → sct.mu →
// chipTrimMu, and cwMu → sct.mu (auto-trim's pause check).
// Preset, manual clock, limit-probe and tuner starts, updates and rollbacks are refused while it runs.

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	sctBaseline    = 20 * time.Minute // defaults; the API takes other lengths (sctMinWin and up)
	sctStepWin     = 20 * time.Minute
	sctSettle      = 2 * time.Minute
	sctTick        = 15 * time.Second
	sctUnsteadyMax = 10 * time.Minute // hook silent / board clock off for this long: abort, trims back
	sctReapplyGap  = time.Minute
)

const (
	sctMinWin    = 5 * time.Minute
	sctMaxWin    = 240 * time.Minute
	sctStepMHz   = trimStep
	sctMaxMHz    = trimMax
	sctMaxChips  = 10
	sctCandP     = 0.05
	sctChipP     = 0.01
	sctChipMin   = 2
	sctChipFloor = 0.1
	sctBoardP    = 0.001
	sctBoardMin  = 5
	sctHashZ     = 3.0
	sctHashDrop  = 0.02
	sctTempRise  = 5.0
	sctStartGapC = 10.0
)

// Round-2 changes (each can be switched off, for the simulation's A/B):
//   - sctPool: a raised chip is judged on everything seen at its present trim (every window it ran there, not
//     only the one that raised it), and chips that already stopped rising are judged again at every window;
//     one that fails steps back once more.
//   - sctPartnerChk: the other chip of a raised chip's series stage gets a lower voltage as the raised chip
//     draws more: when that chip's errors are clearly up, the raised chip fails the step.
//   - sctConfirmMax: once nothing is rising, the kept trims are watched for one more window (repeated while a
//     chip steps back, at most this many windows), so the last step a chip passed isn't kept on one window.
//   - sctHistRank: among chips with no baseline errors, prefer those with the fewest errors since the miner
//     started (the counters' own history), not the most nonces.
var (
	sctPool       = true
	sctPartnerChk = true
	sctConfirmMax = 3
	sctHistRank   = true
)

// sctPartner: the other chip of chip's series stage (chips 2k-1 and 2k), 0 if none.
func sctPartner(chip, chips int) int {
	if p := chipPartner(chip); p >= 1 && p <= chips {
		return p
	}
	return 0
}

func lvl10(m float64) int { return int(math.Round(m * 10)) }

func sctAbortC() float64 { return defaultTunerConfig().AbortC }
func sctWarmC() float64  { return sctAbortC() - 5 }
func sctSteps() int      { return int(math.Round(sctMaxMHz / sctStepMHz)) }

func strongBackup() string { return filepath.Join(filepath.Dir(*authFile), "chiptrim.prestrong") }
func strongResult() string { return filepath.Join(filepath.Dir(*authFile), "strongtest.json") }

// chipJobMu: starting the strong-chip test and a hand edit of the trims, one at a time.
var chipJobMu sync.Mutex

type sctBoardInfo struct {
	Found, Alive, Idle bool
	Temp               float64 // the board's hottest sensor, °C (0: none)
}

// injected I/O (tests replace these)
var (
	sctNow       = time.Now
	sctReadStats = func() (chipStatsFile, bool) {
		s, mod, ok := readChipStats()
		return s, ok && time.Since(mod) < 45*time.Second
	}
	sctReadErrors = readChipErrors
	sctReadBoard  = func(chain int) (sctBoardInfo, bool) {
		d := buildDashboard()
		if !d.OK {
			return sctBoardInfo{}, false
		}
		in := sctBoardInfo{Idle: d.Idle || getCharge() == "idle"}
		for _, b := range d.Boards {
			if b.ID != chain {
				continue
			}
			in.Found, in.Alive = true, b.Alive
			for _, t := range b.Temps {
				if t >= 5 && t < 150 && t > in.Temp {
					in.Temp = t
				}
			}
		}
		return in, true
	}
	sctApply   = func() error { _, err := reapplyTrims(); return err }
	sctHookOn  = func() bool { _, err := os.Stat(chipHookFlag()); return err == nil }
	sctNotify  = func(msg string) { go notify(evTuning, msg) }
	sctSpawn   = func(f func()) { go f() }
	sctLater   = func(d time.Duration, f func()) { go func() { time.Sleep(d); f() }() }
	sctBlocked = sctBlockedReal
)

// sctBlockedReal: something else in charge that changes clocks ("" = free).
func sctBlockedReal() string {
	switch getCharge() {
	case "tuner":
		return "a tuning run is going: stop it first"
	case "ceiling":
		return "the limit probe is running: stop it first"
	case "idle":
		return "the miner is on Idle: start mining first"
	}
	if err := miningHoldErr(); err != nil {
		return err.Error()
	}
	if fanTestRunning() {
		return "the fan test is running: try again in a few minutes"
	}
	return ""
}

type sctChip struct {
	Chip      int     `json:"chip"`
	BaseErr   int     `json:"base_err"`
	BaseNonce int     `json:"base_nonces"`
	Trim      float64 `json:"trim"`   // the test trim on it now
	Passed    float64 `json:"passed"` // the fastest step it ran clean (what it keeps)
	State     string  `json:"state"`  // rising, frozen (failed a step), max (passed +50), held (board guard / stopped)
	Why       string  `json:"why,omitempty"`
	StepErr   []int   `json:"step_errors"` // errors in each step it ran
	LiveErr   int     `json:"live_errors"` // this step so far

	Partner  int             `json:"partner,omitempty"`
	PartBase int             `json:"partner_base,omitempty"`
	PartRef  float64         `json:"partner_ref,omitempty"` // the partner's usual errors per baseline window, from its share of the board's history
	Hist     int             `json:"hist_errors"`           // errors since the miner started, before the baseline
	ConfErr  []int           `json:"confirm_errors,omitempty"`
	lvlErr   map[int]int     // per trim (tenths of a MHz): errors seen while it ran there
	lvlPErr  map[int]int     // its stage partner's errors meanwhile
	lvlSec   map[int]float64 // and for how long (s)
}

type strongTestState struct {
	mu        sync.Mutex
	running   bool
	chain     int
	base10    int
	baseWin   time.Duration
	stepWin   time.Duration
	started   time.Time
	phaseAt   time.Time // the last change (start, a step): settling counts from here
	counting  bool
	snapAt    time.Time
	snapErr   map[int]int
	snapNon   map[int]int
	step      int // 0: baseline, 1..: step × 12.5 MHz
	confirm   int // > 0: the kept trims' confirm window number
	baseT     float64
	baseErr   map[int]int
	histErr   map[int]int // each chip's error counter when the baseline started (its errors since the miner started)
	baseNon   map[int]int
	tempSum   float64
	tempN     int
	baseTemp  float64
	tempNow   float64
	chips     []*sctChip
	orig      []chipTrim
	unsteady  time.Time
	errsBad   time.Time // since when the chips' errors can't be read
	blind     time.Time // since when there's no board temperature (or no dashboard): the heat guard can't work
	lastApply time.Time
	note      string
	pending   bool // the end couldn't put the trims back: sctRecover does (restore_pending in the result)
	log       []map[string]any
	stop      chan struct{}
}

var sct strongTestState

// sctLive mirrors sct.running (set with it, under s.mu) for readers that only look: the fan loop's throttle check
// and the Controls view. They mustn't wait on s.mu, which a tick holds across the miner reads (seconds each when
// the miner or its backend is slow): the thermal guard's abort tier comes after that check. Anything that acts on
// the answer (a start, a clock change) still uses sctRunning, under its own locks.
var sctLive atomic.Bool

func sctRunningNow() bool { return sctLive.Load() }

// reset: every field but the lock. Called with s.mu held.
func (s *strongTestState) reset() {
	s.running, s.chain, s.base10, s.baseWin, s.stepWin = false, 0, 0, 0, 0
	s.started, s.phaseAt, s.snapAt, s.unsteady, s.lastApply, s.errsBad = time.Time{}, time.Time{}, time.Time{}, time.Time{}, time.Time{}, time.Time{}
	s.blind = time.Time{}
	s.counting, s.snapErr, s.snapNon, s.step, s.confirm, s.histErr = false, nil, nil, 0, 0, nil
	s.baseT, s.baseErr, s.baseNon, s.tempSum, s.tempN, s.baseTemp, s.tempNow = 0, nil, nil, 0, 0, 0, 0
	s.chips, s.orig, s.note, s.log, s.stop, s.pending = nil, nil, "", nil, nil, false
	sctLive.Store(false)
}

func sctRunning() bool {
	sct.mu.Lock()
	defer sct.mu.Unlock()
	return sct.running
}

// sctRecover: a test cut short by a panel restart leaves its trims in place; put the ones from before back. Also a
// test whose end couldn't put the trims back (restore_pending in its result): retried until it can.
func sctRecover() {
	if featOff(model.Feat.ChipTrims) {
		return // chip trims (and the strong-chip test) are off on this model: no test ever ran, nothing to recover
	}
	// The result file names the chips the test raised (saved before each step's trims are written). They had no
	// trim before the test (only untrimmed chips are candidates), so they go back to 0 and every other trim
	// stays: that doesn't depend on the backup, which may be missing, cut short by a power cut, or left over
	// from an earlier test. Without a result saying a test was running, a leftover backup is stale: removed.
	b, berr := os.ReadFile(strongBackup())
	rb, rerr := os.ReadFile(strongResult())
	var last map[string]any
	perr := error(nil)
	if rerr == nil {
		perr = json.Unmarshal(rb, &last)
	}
	if rerr == nil && perr != nil && berr == nil {
		// a result cut short (a power cut while it was replaced) with the backup still there: a test was going (or
		// its end couldn't put the trims back). The backup is synced before the first result is written, so it's
		// whole: it goes back.
		chipTrimMu.Lock()
		err := writeAtomic(chipTrimPath(), b, 0o644)
		if err == nil {
			os.Remove(strongBackup())
		}
		chipTrimMu.Unlock()
		if err == nil {
			os.Remove(strongResult())
			activity.emit("💪 strong-chip test: its result file was cut short (a power cut?): every chip is back to its trim from before the test")
			sctLater(90*time.Second, func() { sctApplyRetry(15) })
		}
		return
	}
	pending := rerr == nil && perr == nil && last["restore_pending"] == true
	if rerr != nil || perr != nil || (last["running"] != true && !pending) {
		if berr == nil {
			os.Remove(strongBackup())
		}
		return
	}
	interrupted := last["running"] == true
	chain, _ := last["chain"].(float64)
	raised := map[int]bool{}
	cs, _ := last["chips"].([]any)
	for _, c := range cs {
		if m, ok := c.(map[string]any); ok {
			if n, ok := m["chip"].(float64); ok {
				raised[int(n)] = true
			}
		}
	}
	chipTrimMu.Lock()
	var ts []chipTrim
	have := map[[2]int]bool{}
	for _, t := range readChipTrims() {
		if t.Chain != int(chain) || !raised[t.Chip] {
			ts = append(ts, t)
			have[[2]int{t.Chain, t.Chip}] = true
		}
	}
	if berr == nil {
		// the power cut that cut the test short may have cut the trims file short too: a trim from before the test
		// that's missing from it comes from the backup (a raised chip had none before the test: none comes back)
		for _, t := range parseChipTrims(b) {
			if !have[[2]int{t.Chain, t.Chip}] && (t.Chain != int(chain) || !raised[t.Chip]) {
				ts = append(ts, t)
			}
		}
	}
	err := writeChipTrims(ts)
	if err != nil && berr == nil {
		err = os.WriteFile(chipTrimPath(), b, 0o644) // can't rewrite it: the backup, as before
	}
	if err == nil {
		os.Remove(strongBackup())
	}
	chipTrimMu.Unlock()
	if err != nil {
		return // still pending: the next try (or panel start) does it
	}
	if !interrupted {
		last["restore_pending"] = false
		last["message"] = fmt.Sprint(last["message"]) + " (The trims are back now.)"
		nb, _ := json.Marshal(last)
		writeAtomic(strongResult(), nb, 0o644)
		activity.emit("💪 strong-chip test: every chip is back to its trim from before the test (it couldn't be written when the test ended)")
		sctLater(time.Second, func() { sctApplyRetry(15) })
		return
	}
	last["running"], last["how"], last["ended"] = false, "interrupted", sctNow().Unix()
	if st, ok := last["started"].(float64); ok {
		last["minutes"] = int(sctNow().Sub(time.Unix(int64(st), 0)).Minutes())
	}
	last["message"] = "The panel restarted during the test: every chip is back to what it had before it."
	for _, c := range cs {
		if m, ok := c.(map[string]any); ok {
			m["kept"] = 0.0
			if m["state"] == "rising" {
				m["state"], m["why"] = "held", "interrupted"
			}
		}
	}
	nb, _ := json.Marshal(last)
	writeAtomic(strongResult(), nb, 0o644)
	activity.emit("💪 strong-chip test was interrupted (the panel restarted): every chip is back to its trim from before")
	sctLater(2*time.Minute, func() {
		notify(evTuning, "strong-chip test cut short: the panel restarted during it; every chip is back to its trim from before")
	})
	sctLater(90*time.Second, func() { sctApplyRetry(15) })
}

// sctRestoreRetry: a test's end couldn't put the trims back: try again (one at a time with starts and hand edits).
func sctRestoreRetry(tries int) {
	chipJobMu.Lock()
	if !sctRunning() {
		sctRecover()
	}
	_, err := os.Stat(strongBackup())
	chipJobMu.Unlock()
	if err == nil && tries > 1 {
		sctLater(time.Minute, func() { sctRestoreRetry(tries - 1) })
	}
}

// sctApplyRetry: re-send the trims file's trims, retrying for a while (the chips keep whatever they were last
// sent until then: after a test, possibly its raised trims).
func sctApplyRetry(tries int) {
	if err := sctApply(); err != nil && tries > 1 && !sctRunning() {
		sctLater(time.Minute, func() {
			if !sctRunning() {
				sctApplyRetry(tries - 1)
			}
		})
	}
}

// chipErrsFor: the board's per-chip error counters, numbered like the hook's chips.
func chipErrsFor(chain int, errs map[ckey]int, st chipStatsFile) map[int]int {
	minIdx := minIndexes(errs)
	out := map[int]int{}
	for k, v := range errs {
		if c, chip, ok := errToHook(k, minIdx, st); ok && c == chain {
			out[chip] = v
		}
	}
	return out
}

func chainOf(st chipStatsFile, chain int) (chips, base10 int, clock10, nonces []int, ok bool) {
	for _, c := range st.Chains {
		if c.Chain == chain {
			return c.Chips, c.Base10, c.Clock10, c.Nonces, true
		}
	}
	return 0, 0, nil, nil, false
}

func sctEstimate(base, step time.Duration) time.Duration {
	n := sctSteps()
	if sctConfirmMax > 0 {
		n++ // the confirm window (more only when a chip steps back in it)
	}
	return sctSettle + base + time.Duration(n)*(sctSettle+step)
}

// writeTrims: the trims from before the test plus the test's own. Called with s.mu held.
func (s *strongTestState) writeTrims(final bool) error {
	ts := append([]chipTrim{}, s.orig...)
	for _, c := range s.chips {
		m := c.Trim
		if final {
			m = c.Passed
		}
		if m != 0 {
			ts = append(ts, chipTrim{Chain: s.chain, Chip: c.Chip, MHz: m})
		}
	}
	chipTrimMu.Lock()
	defer chipTrimMu.Unlock()
	return writeChipTrims(ts)
}

func (s *strongTestState) apply() {
	s.lastApply = sctNow()
	if err := s.writeTrims(false); err != nil {
		s.note = "couldn't write the trims: " + err.Error()
		return
	}
	if err := sctApply(); err != nil {
		s.note = "not applied live yet (" + err.Error() + "): retrying"
	}
}

func (s *strongTestState) logf(format string, a ...any) {
	s.log = append(s.log, map[string]any{"t": sctNow().Unix(), "msg": fmt.Sprintf(format, a...)})
}

func (s *strongTestState) rising() []*sctChip {
	var r []*sctChip
	for _, c := range s.chips {
		if c.State == "rising" {
			r = append(r, c)
		}
	}
	return r
}

// restartWindow: settle again and count from scratch (a miner restart, a pause).
func (s *strongTestState) restartWindow(now time.Time, why string) {
	s.counting, s.snapErr, s.snapNon, s.phaseAt = false, nil, nil, now
	for _, c := range s.chips {
		c.LiveErr = 0
	}
	if s.step == 0 {
		s.tempSum, s.tempN = 0, 0
	}
	s.note = why
}

// tick: one look at the board. Called with s.mu held; ends the test itself (finish) when it's over.
func (s *strongTestState) tick() {
	now := sctNow()
	st, ok := sctReadStats()
	chips, base10, clock10, nonces, found := chainOf(st, s.chain)
	if found && base10 > 0 {
		// never past the clock window: it can shrink under the test (the clock limits re-locked, a lower measured cap)
		top := 0.0
		for _, c := range s.chips {
			top = math.Max(top, c.Trim)
		}
		if hi := float64(chipTrimCeil()); top > 0 && float64(base10)/10+top > hi+1e-9 {
			s.finish("aborted", fmt.Sprintf("at %+.4g MHz the raised chips would run %.4g MHz, past the %.0f MHz clock window now", top, float64(base10)/10+top, hi))
			return
		}
	}
	b, bok := sctReadBoard(s.chain)
	why := ""
	switch {
	case !ok:
		why = "waiting for the chip hook to report (is mining running?)"
	case !found || chips < 3:
		why = fmt.Sprintf("board %d isn't reporting its chips", s.chain)
	case base10 != s.base10:
		why = fmt.Sprintf("board %d is at %.1f MHz, not the %.1f MHz the test started at", s.chain, float64(base10)/10, float64(s.base10)/10)
	case bok && b.Idle:
		why = "the miner is on Idle"
	case bok && (!b.Found || !b.Alive):
		why = fmt.Sprintf("board %d isn't hashing", s.chain)
	}
	if why != "" {
		if s.unsteady.IsZero() {
			s.unsteady = now
		}
		if now.Sub(s.unsteady) >= sctUnsteadyMax {
			s.finish("aborted", why+" for "+humanDur(sctUnsteadyMax.Round(time.Minute)))
			return
		}
		s.restartWindow(now, why)
		return
	}
	s.unsteady = time.Time{}
	// no temperature (no sensor reading, or no dashboard): the heat guard below can't work, so don't keep raising
	// chips blind (the tuner doesn't test blind either): a short gap is waited out (the window stays), a long
	// one ends the test with the trims back
	if !bok || !(b.Temp > 0) {
		if s.blind.IsZero() {
			s.blind = now
		}
		if now.Sub(s.blind) >= sctUnsteadyMax {
			s.finish("aborted", fmt.Sprintf("no temperature reading for board %d for %s", s.chain, humanDur(sctUnsteadyMax.Round(time.Minute))))
			return
		}
		s.note = fmt.Sprintf("no temperature reading for board %d", s.chain)
		return
	}
	s.blind = time.Time{}
	// not running hot
	if bok && b.Temp > 0 {
		s.tempNow = b.Temp
		if s.step == 0 {
			if b.Temp >= sctWarmC() {
				s.finish("aborted", fmt.Sprintf("board %d reached %.0f °C during the baseline (warm limit %.0f °C)", s.chain, b.Temp, sctWarmC()))
				return
			}
			if s.counting {
				s.tempSum += b.Temp
				s.tempN++
			}
		} else if b.Temp >= sctWarmC() || (s.baseTemp > 0 && b.Temp > s.baseTemp+sctTempRise) {
			s.guard(fmt.Sprintf("board %d reached %.0f °C (%.0f °C in the baseline; limit +%.0f °C or %.0f °C)", s.chain, b.Temp, s.baseTemp, sctTempRise, sctWarmC()))
			return
		}
	}
	errs, err := sctReadErrors()
	if err != nil {
		if s.errsBad.IsZero() {
			s.errsBad = now
		}
		if now.Sub(s.errsBad) >= sctUnsteadyMax { // raised chips mustn't run on unjudged
			s.finish("aborted", "can't read the chips' errors ("+err.Error()+") for "+humanDur(sctUnsteadyMax.Round(time.Minute)))
			return
		}
		s.note = "can't read the chips' errors (" + err.Error() + ")"
		return // the window stays: a missed reading doesn't spoil it
	}
	s.errsBad = time.Time{}
	ce := chipErrsFor(s.chain, errs, st)
	if !s.counting {
		if now.Sub(s.phaseAt) < sctSettle {
			s.note = ""
			return
		}
		// the raised chips must really run at their new clock before the step counts
		for _, c := range s.chips {
			if c.Trim == 0 || c.Chip >= len(clock10) || clock10[c.Chip] <= 0 {
				continue
			}
			if want := s.base10 + int(math.Round(c.Trim*10)); clock10[c.Chip] != want {
				if now.Sub(s.phaseAt) >= sctUnsteadyMax {
					s.finish("aborted", fmt.Sprintf("the new clocks didn't reach the chips (c%d runs %.1f MHz, not %.1f)", c.Chip, float64(clock10[c.Chip])/10, float64(want)/10))
					return
				}
				if now.Sub(s.lastApply) >= sctReapplyGap {
					s.apply()
				}
				s.note = "waiting for the chips to take the new clocks"
				return
			}
		}
		s.snapErr, s.snapNon, s.snapAt, s.counting = ce, map[int]int{}, now, true
		if s.step == 0 {
			s.histErr = ce
		}
		for i := 1; i <= chips && i < len(nonces); i++ {
			s.snapNon[i] = nonces[i]
		}
		s.note = ""
		return
	}
	// a raised chip back at the board clock (a chain re-init the hook didn't follow): this window doesn't count
	for _, c := range s.chips {
		if c.Trim != 0 && c.Chip < len(clock10) && clock10[c.Chip] > 0 && clock10[c.Chip] != s.base10+int(math.Round(c.Trim*10)) {
			s.restartWindow(now, fmt.Sprintf("c%d isn't at its test clock: this window starts again", c.Chip))
			return
		}
	}
	// a miner restart: the counters start again
	for ch, v := range ce {
		if p, ok := s.snapErr[ch]; ok && v < p {
			s.restartWindow(now, "the miner restarted: this window starts again")
			return
		}
	}
	for ch, p := range s.snapNon {
		if ch < len(nonces) && nonces[ch] < p {
			s.restartWindow(now, "the miner restarted: this window starts again")
			return
		}
	}
	dErr := map[int]int{}
	for ch, v := range ce {
		if p, ok := s.snapErr[ch]; ok {
			dErr[ch] = v - p
		}
	}
	for _, c := range s.chips {
		c.LiveErr = dErr[c.Chip]
	}
	win := s.stepWin
	if s.step == 0 {
		win = s.baseWin
	}
	if now.Sub(s.snapAt) < win {
		return
	}
	dNon := map[int]int{}
	for ch, p := range s.snapNon {
		if ch < len(nonces) {
			dNon[ch] = nonces[ch] - p
		}
	}
	T := now.Sub(s.snapAt).Seconds()
	if s.step == 0 {
		s.endBaseline(chips, dErr, dNon, T)
		return
	}
	s.judge(chips, dErr, dNon, T)
}

func (s *strongTestState) endBaseline(chips int, dErr, dNon map[int]int, T float64) {
	s.baseErr, s.baseNon, s.baseT = dErr, dNon, T
	if s.tempN > 0 {
		s.baseTemp = s.tempSum / float64(s.tempN)
	} else {
		s.baseTemp = s.tempNow
	}
	s.chips = sctCandidatesH(chips, dErr, dNon, s.histErr, s.orig, s.chain)
	histTot, e0 := 0, 0
	for i := 1; i <= chips; i++ {
		histTot += s.histErr[i]
		e0 += dErr[i]
	}
	for _, c := range s.chips {
		c.Hist = s.histErr[c.Chip]
		if c.Partner = sctPartner(c.Chip, chips); c.Partner > 0 {
			c.PartBase = dErr[c.Partner]
			if histTot >= 20 { // a partner that always makes errors (its share of the board's) isn't "up" when it makes them now
				c.PartRef = float64(s.histErr[c.Partner]) / float64(histTot) * float64(e0)
			}
		}
		c.lvlErr, c.lvlPErr, c.lvlSec = map[int]int{}, map[int]int{}, map[int]float64{}
	}
	tot := 0
	for _, v := range dErr {
		tot += v
	}
	if len(s.chips) == 0 {
		s.logf("baseline: %d errors on the board in %d min; no chip qualifies", tot, int(T/60))
		s.finish("none", "No chip qualified: every untrimmed chip made errors (or too few nonces) in the baseline.")
		return
	}
	var names []string
	for _, c := range s.chips {
		names = append(names, fmt.Sprintf("c%d", c.Chip))
	}
	s.logf("baseline: %d errors on the board in %d min; candidates %s", tot, int(T/60), strings.Join(names, ", "))
	s.nextStep()
}

// sctCandidates: see the top of the file.
func sctCandidates(chips int, dErr, dNon map[int]int, orig []chipTrim, chain int) []*sctChip {
	return sctCandidatesH(chips, dErr, dNon, nil, orig, chain)
}

func sctCandidatesH(chips int, dErr, dNon, hist map[int]int, orig []chipTrim, chain int) []*sctChip {
	trimmed := map[int]bool{}
	stage := map[int]bool{} // series stages taken: a raised candidate, or a chip already running "+"
	for _, t := range orig {
		if t.Chain == chain && t.MHz != 0 {
			trimmed[t.Chip] = true
			// a "+" chip (an earlier test's, or a hand trim) already pulls its stage down; a "-" chip is a weak one, and a
			// raised partner would take voltage from it (the pair rules: no "+" beside a weak chip)
			stage[(t.Chip-1)/2] = true
		}
	}
	var errsAll []int
	nSum, nN := 0, 0
	for i := 1; i <= chips; i++ {
		if e, ok := dErr[i]; ok {
			errsAll = append(errsAll, e)
		}
		if n, ok := dNon[i]; ok {
			nSum += n
			nN++
		}
	}
	if len(errsAll) == 0 {
		return nil
	}
	sort.Ints(errsAll)
	med := float64(errsAll[len(errsAll)/2])
	// the largest k ≥ 1 with P(X ≤ k) < sctCandP (0 if none): the CDF rises with k and is past 5 % by the median,
	// so a bisection over [1, med+1] (a counting loop is O(med) calls, and med can be huge on a sick board)
	lim, lo, hi := 0, 1, int(med)+1
	for lo <= hi {
		mid := lo + (hi-lo)/2
		if poissonCDF(mid, med) < sctCandP {
			lim, lo = mid, mid+1
		} else {
			hi = mid - 1
		}
	}
	avgN := 0.0
	if nN > 0 {
		avgN = float64(nSum) / float64(nN)
	}
	type cand struct{ chip, e, n, h int }
	var cs []cand
	for i := 1; i <= chips; i++ {
		e, okE := dErr[i]
		n, okN := dNon[i]
		if !okE || !okN || trimmed[i] || e > lim || n <= 0 {
			continue // not in the miner's chip list, no nonce count, a trim already, too many errors, no work
		}
		if avgN >= 30 && (float64(n)-avgN)/math.Sqrt(avgN) < -3 {
			continue // clearly does less work than the rest: its few errors say little
		}
		h := 0
		if sctHistRank {
			h = hist[i]
		}
		cs = append(cs, cand{i, e, n, h})
	}
	sort.Slice(cs, func(i, j int) bool {
		if cs[i].e != cs[j].e {
			return cs[i].e < cs[j].e
		}
		if cs[i].h != cs[j].h { // fewer errors since the miner started: a longer record than the baseline
			return cs[i].h < cs[j].h
		}
		if cs[i].n != cs[j].n {
			return cs[i].n > cs[j].n
		}
		return cs[i].chip < cs[j].chip
	})
	var out []*sctChip
	for _, c := range cs {
		if len(out) >= sctMaxChips {
			break
		}
		if stage[(c.chip-1)/2] {
			continue
		}
		stage[(c.chip-1)/2] = true
		out = append(out, &sctChip{Chip: c.chip, BaseErr: c.e, BaseNonce: c.n, State: "rising"})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Chip < out[j].Chip })
	return out
}

func (s *strongTestState) nextStep() {
	s.step++
	m := float64(s.step) * sctStepMHz
	var names []string
	for _, c := range s.rising() {
		c.Trim = m
		names = append(names, fmt.Sprintf("c%d", c.Chip))
	}
	s.logf("step %d: %s to %+.4g MHz", s.step, strings.Join(names, ", "), m)
	s.restartWindow(sctNow(), "")
	s.save(true, "", "") // first: a panel restart's recovery takes the raised chips from it
	s.apply()
}

// judge: the end of a step's (or the confirm) window.
func (s *strongTestState) judge(chips int, dErr, dNon map[int]int, T float64) {
	m := float64(s.step) * sctStepMHz
	scale := T / s.baseT
	e0, n0, n1 := 0, 0, 0
	expN := 0.0 // the board's nonces at the baseline's rate, with the raised chips' faster clocks
	trimNow := map[int]float64{}
	for _, c := range s.chips {
		trimNow[c.Chip] = c.Trim
	}
	baseMHz := float64(s.base10) / 10
	for i := 1; i <= chips; i++ {
		e0 += s.baseErr[i]
		n0 += s.baseNon[i]
		n1 += dNon[i]
		expN += float64(s.baseNon[i]) * (baseMHz + trimNow[i]) / baseMHz
	}
	mean := float64(e0) / float64(chips)
	// the evidence: each raised chip's errors (and its stage partner's) at the trim it ran this window
	for _, c := range s.chips {
		if c.Trim <= 0 {
			continue
		}
		if c.lvlErr == nil {
			c.lvlErr, c.lvlPErr, c.lvlSec = map[int]int{}, map[int]int{}, map[int]float64{}
		}
		L := lvl10(c.Trim)
		if !sctPool && c.State == "rising" { // the original rule: this window alone
			c.lvlErr[L], c.lvlPErr[L], c.lvlSec[L] = 0, 0, 0
		}
		c.lvlErr[L] += dErr[c.Chip]
		c.lvlSec[L] += T
		if c.Partner > 0 {
			c.lvlPErr[L] += dErr[c.Partner]
		}
		if c.State == "rising" {
			c.StepErr = append(c.StepErr, dErr[c.Chip])
		} else if s.confirm > 0 {
			c.ConfErr = append(c.ConfErr, dErr[c.Chip])
		}
	}
	failed := map[int]string{}
	var parts []string
	for _, c := range s.chips {
		if c.Trim <= 0 || (!sctPool && c.State != "rising") {
			continue
		}
		L := lvl10(c.Trim)
		sc := c.lvlSec[L] / s.baseT
		k := c.lvlErr[L]
		lam := math.Max(sctChipFloor, sc*math.Max(float64(c.BaseErr), mean))
		if c.State == "rising" || dErr[c.Chip] > 0 {
			parts = append(parts, fmt.Sprintf("c%d %d", c.Chip, dErr[c.Chip]))
		}
		mins := int(math.Round(c.lvlSec[L] / 60))
		if k >= sctChipMin && poissonSF(k, lam) < sctChipP {
			failed[c.Chip] = fmt.Sprintf("%d errors in %d min at %+.4g MHz (%.1f expected from its baseline)", k, mins, c.Trim, lam)
			continue
		}
		if c.State == "rising" {
			if exp := scale * float64(c.BaseNonce); exp >= 30 && (float64(dNon[c.Chip])-exp)/math.Sqrt(exp) < -sctHashZ {
				// a chip that stops finding nonces makes no errors either (the miner may even drop it from its list):
				// at a faster clock it should find at least its baseline's share
				failed[c.Chip] = fmt.Sprintf("found %d nonces at %+.4g MHz (%.0f expected from its baseline)", dNon[c.Chip], m, exp)
				continue
			}
		}
		if sctPartnerChk && c.Partner > 0 {
			kp := c.lvlPErr[L]
			lamP := math.Max(sctChipFloor, sc*math.Max(math.Max(float64(c.PartBase), c.PartRef), mean))
			// stricter than the chip's own test: the partner's usual rate is less well known (it wasn't picked for
			// making none), and a false alarm here costs a clean chip its step
			if kp >= sctChipMin+1 && poissonSF(kp, lamP) < sctBoardP {
				failed[c.Chip] = fmt.Sprintf("the other chip of its stage, c%d, made %d errors in %d min while it ran %+.4g MHz (%.1f expected)", c.Partner, kp, mins, c.Trim, lamP)
			}
		}
	}
	// the rest of the board (the chips failing now are dealt with on their own)
	sB := 0
	for ch, k := range dErr {
		if _, f := failed[ch]; !f && ch >= 1 && ch <= chips {
			sB += k
		}
	}
	// two Poisson counts compared exactly: with the rate unchanged, of the n = baseline + step errors the step's
	// share is Binomial(n, T ÷ (T + baseline T)); the baseline count is itself noisy, so it isn't taken as the rate
	lamB := scale * float64(e0)
	boardBad := sB >= sctBoardMin && binomSF(sB, sB+e0, T/(T+s.baseT)) < sctBoardP
	r, z := 1.0, 0.0
	if n0 >= 100 && n1 >= 100 {
		r = (float64(n1) / T) / (expN / s.baseT) // against the rate the faster chips should give
		z = math.Log(r) / math.Sqrt(1/float64(n1)+1/float64(n0))
	}
	hashBad := z < -sctHashZ && r < 1-sctHashDrop
	what := fmt.Sprintf("step %d (%+.4g MHz)", s.step, m)
	if s.confirm > 0 {
		what = fmt.Sprintf("confirm %d", s.confirm)
	}
	s.log = append(s.log, map[string]any{"t": sctNow().Unix(), "step": s.step, "mhz": m, "minutes": round1(T / 60), "confirm": s.confirm,
		"msg": fmt.Sprintf("%s after %d min: chip errors %s; board %d errors (%.1f expected), nonce rate %+.1f %%",
			what, int(T/60), strings.Join(parts, ", "), sB, lamB, (r-1)*100)})
	// chips that already hold a step and fail now: one step further back
	changed := false
	for _, c := range s.chips {
		if why, f := failed[c.Chip]; f && c.State != "rising" {
			c.Passed = math.Max(0, c.Passed-sctStepMHz)
			c.Trim, c.State, c.Why, changed = c.Passed, "frozen", why+fmt.Sprintf(": back to %+.4g", c.Passed), true
		}
	}
	if boardBad || hashBad {
		why := fmt.Sprintf("the board's errors rose to %d (%.1f expected from the baseline)", sB, lamB)
		if !boardBad {
			why = fmt.Sprintf("the board's nonce rate fell %.1f %% (beyond noise)", (1-r)*100)
		}
		if s.confirm > 0 { // nothing rising: the kept trims as a whole made it worse
			for _, c := range s.chips {
				if c.Passed > 0 && failed[c.Chip] == "" {
					c.Passed = math.Max(0, c.Passed-sctStepMHz)
					c.Trim, c.State, c.Why = c.Passed, "held", fmt.Sprintf("board guard in the confirm window: back to %+.4g", c.Passed)
				}
			}
			s.logf("board guard (confirm): %s; every raised chip back one step", why)
			s.finish("guard", "Ended in the confirm window: "+why+". Every raised chip went back one step.")
			return
		}
		s.guard(why + fmt.Sprintf(" at %+.4g MHz", m))
		return
	}
	for _, c := range s.rising() {
		if why, f := failed[c.Chip]; f {
			c.State, c.Trim, c.Why, changed = "frozen", c.Passed, why, true
			continue
		}
		c.Passed = c.Trim
		if s.step >= sctSteps() {
			c.State, c.Why = "max", fmt.Sprintf("clean up to %+.4g MHz, the most a trim allows", sctMaxMHz)
		}
	}
	if len(s.rising()) > 0 {
		s.nextStep()
		return
	}
	held := false
	for _, c := range s.chips {
		if c.Passed > 0 {
			held = true
		}
	}
	// confirm: after the last step, and again after a confirm window in which a chip stepped back
	if held && s.confirm < sctConfirmMax && (s.confirm == 0 || changed) {
		s.confirm++
		var names []string
		for _, c := range s.chips {
			if c.Passed > 0 {
				names = append(names, fmt.Sprintf("c%d %+.4g", c.Chip, c.Passed))
			}
		}
		s.logf("confirm %d: the kept trims (%s MHz) watched once more", s.confirm, strings.Join(names, ", "))
		s.restartWindow(sctNow(), "")
		s.save(true, "", "") // first, as in nextStep
		if changed {
			s.apply()
		}
		return
	}
	s.finish("done", "")
}

// binomSF: P(X ≥ k) for X ~ Binomial(n, p).
func binomSF(k, n int, p float64) float64 {
	if k <= 0 {
		return 1
	}
	if k > n || p <= 0 {
		return 0
	}
	if p >= 1 {
		return 1
	}
	sum := 0.0
	for i := k; i <= n; i++ {
		lg1, _ := math.Lgamma(float64(n + 1))
		lg2, _ := math.Lgamma(float64(i + 1))
		lg3, _ := math.Lgamma(float64(n - i + 1))
		sum += math.Exp(lg1 - lg2 - lg3 + float64(i)*math.Log(p) + float64(n-i)*math.Log1p(-p))
	}
	return math.Min(1, sum)
}

// guard: the whole board got worse. Every rising chip goes back one step; the test ends.
func (s *strongTestState) guard(why string) {
	for _, c := range s.rising() {
		c.State, c.Trim = "held", c.Passed
		c.Why = fmt.Sprintf("board guard at %+.4g MHz: back one step", float64(s.step)*sctStepMHz)
	}
	s.logf("board guard: %s; every raised chip back one step", why)
	s.finish("guard", "Ended early: "+why+". Every chip still rising went back one step.")
}

// result: what the card and the result file show. Called with s.mu held.
func (s *strongTestState) result(how, msg string, running bool) map[string]any {
	type row struct {
		sctChip
		Kept float64 `json:"kept"`
	}
	var rows []row
	for _, c := range s.chips {
		k := c.Passed
		if how == "stopped" || how == "aborted" {
			k = 0
		}
		rows = append(rows, row{*c, k})
	}
	now := sctNow()
	return map[string]any{"running": running, "how": how, "message": msg, "chain": s.chain, "started": s.started.Unix(),
		"ended": now.Unix(), "minutes": int(now.Sub(s.started).Minutes()), "baseline_min": int(s.baseWin.Minutes()),
		"step_min": int(s.stepWin.Minutes()), "step": s.step, "base_clock": float64(s.base10) / 10, "base_temp": round1(s.baseTemp),
		"chips": rows, "log": s.log, "restore_pending": s.pending}
}

func (s *strongTestState) save(running bool, how, msg string) {
	b, _ := json.Marshal(s.result(how, msg, running))
	writeAtomic(strongResult(), b, 0o644) // synced: after a power cut, recovery reads it to put the trims back
}

// finish: keep (done, guard, none) or put back (stopped, aborted). Called with s.mu held.
func (s *strongTestState) finish(how, msg string) {
	keep := how == "done" || how == "guard" || how == "none"
	if !keep {
		for _, c := range s.rising() {
			c.State, c.Why = "held", how
		}
	}
	var kept []string
	for _, c := range s.chips {
		if keep && c.Passed > 0 {
			kept = append(kept, fmt.Sprintf("c%d %+.4g", c.Chip, c.Passed))
		}
	}
	if msg == "" {
		if len(kept) > 0 {
			msg = fmt.Sprintf("%d chip(s) keep a faster clock: %s MHz (saved as your own trims).", len(kept), strings.Join(kept, ", "))
		} else {
			msg = "No chip stayed clean one step faster: nothing changed."
		}
	} else if keep && len(kept) > 0 {
		msg += fmt.Sprintf(" Kept: %s MHz (saved as your own trims).", strings.Join(kept, ", "))
	} else if !keep {
		msg += "."
	}
	if keep {
		if err := s.writeTrims(true); err != nil {
			// the file still holds the last step (a chip that just failed it included): put everything back instead
			how, keep = "aborted", false
			msg = "Couldn't save the trims it found (" + err.Error() + ")."
			for _, c := range s.chips {
				c.Why = "not saved"
			}
		}
	}
	if !keep {
		if err := s.restore(); err != nil {
			s.pending = true
			msg += " Couldn't put the trims back yet (" + err.Error() + "): retrying for 15 minutes, and at the next panel start."
			sctLater(time.Minute, func() { sctRestoreRetry(15) })
		} else {
			msg += " Every chip is back to what it had before the test."
		}
	} else {
		chipTrimMu.Lock()
		os.Remove(strongBackup())
		chipTrimMu.Unlock()
	}
	// the clock window may have got lower during the test (capPlusTrims skips the trims while it runs): the "+" trims
	// written back now (the owner's from before it, and the ones it kept) must fit it at the board clock
	chipTrimMu.Lock()
	if m := capPlusTrimsLocked((s.base10 + 9) / 10); m != "" {
		msg += " " + strings.ToUpper(m[:1]) + m[1:] + "."
	}
	chipTrimMu.Unlock()
	s.running = false
	sctLive.Store(false)
	if s.stop != nil {
		select {
		case <-s.stop:
		default:
			close(s.stop)
		}
	}
	if err := sctApply(); err != nil {
		msg += " Not applied live yet (" + err.Error() + "): retrying for 15 minutes; it also applies at the next mining restart, or use Re-apply now."
		sctLater(time.Minute, func() { sctApplyRetry(15) })
	}
	s.logf("ended (%s): %s", how, msg)
	s.save(false, how, msg)
	dur := runDur(sctNow().Sub(s.started))
	activity.emit(fmt.Sprintf("💪 strong-chip test on board %d %s after %s: %s", s.chain, sctHowText(how), dur, msg))
	for _, c := range s.chips {
		if c.Why != "" || c.Passed > 0 {
			activity.emit(fmt.Sprintf("💪   b%d.c%d: baseline %d errors, ends %+.4g MHz — %s", s.chain, c.Chip, c.BaseErr, map[bool]float64{true: c.Passed, false: 0}[keep], sctChipWhy(c)))
		}
	}
	sctNotify(fmt.Sprintf("strong-chip test on board %d %s after %s: %s", s.chain, sctHowText(how), dur, msg))
}

// restore: every trim back as before the test: the backup's bytes, else the trims read at the start. The
// backup is removed only once that's written (a panel restart's recovery retries otherwise). Called with s.mu held.
func (s *strongTestState) restore() error {
	chipTrimMu.Lock()
	defer chipTrimMu.Unlock()
	var err error
	if saved, rerr := os.ReadFile(strongBackup()); rerr == nil {
		tmp := chipTrimPath() + ".tmp"
		if err = os.WriteFile(tmp, saved, 0o644); err == nil {
			err = os.Rename(tmp, chipTrimPath())
		}
		if err != nil {
			err = os.WriteFile(chipTrimPath(), saved, 0o644) // the .tmp in the way: write in place
		}
	} else if err = writeChipTrims(s.orig); err != nil {
		var b strings.Builder
		for _, t := range s.orig {
			fmt.Fprintf(&b, "%d %d %s\n", t.Chain, t.Chip, strconv.FormatFloat(t.MHz, 'f', -1, 64))
		}
		err = os.WriteFile(chipTrimPath(), []byte(b.String()), 0o644)
	}
	if err == nil {
		os.Remove(strongBackup())
	}
	return err
}

func sctHowText(how string) string {
	return map[string]string{"done": "finished", "guard": "ended by the board guard", "none": "found no candidate",
		"stopped": "stopped", "aborted": "aborted"}[how]
}

func sctChipWhy(c *sctChip) string {
	switch {
	case c.Why != "":
		return c.Why
	case c.State == "max":
		return "clean at every step"
	}
	return c.State
}

func (s *strongTestState) loop(stop chan struct{}) {
	t := time.NewTicker(sctTick)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		}
		if !sctTickOnce() {
			return
		}
	}
}

// sctTickOnce: one tick under the lock; false once the test is over.
func sctTickOnce() bool {
	s := &sct
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.running {
		return false
	}
	defer func() {
		// a panic here would take the whole panel down with the test's trims on: end the test, trims back
		if r := recover(); r != nil {
			log.Printf("strong-chip test: panic: %v\n%s", r, debug.Stack())
			if s.running {
				s.finish("aborted", fmt.Sprintf("internal error (%v)", r))
			}
		}
	}()
	s.tick()
	return s.running
}

func sctView() map[string]any {
	s := &sct
	s.mu.Lock()
	defer s.mu.Unlock()
	v := map[string]any{"running": s.running, "defaults": map[string]any{"baseline_min": int(sctBaseline.Minutes()),
		"step_min": int(sctStepWin.Minutes()), "min_min": int(sctMinWin.Minutes()), "max_min": int(sctMaxWin.Minutes()),
		"settle_min": sctSettle.Minutes(), "steps": sctSteps(), "confirm": sctConfirmMax > 0, "step_mhz": sctStepMHz, "max_mhz": sctMaxMHz, "max_chips": sctMaxChips,
		"warm_c": sctWarmC(), "start_max_c": sctAbortC() - sctStartGapC}}
	if s.running {
		now := sctNow()
		a := s.result("", s.note, true)
		a["note"], a["counting"] = s.note, s.counting
		phase, left := "settling", sctSettle-now.Sub(s.phaseAt)
		if s.counting {
			w := s.stepWin
			if s.step == 0 {
				w = s.baseWin
			}
			phase, left = "watching", w-now.Sub(s.snapAt)
		}
		if left < 0 {
			left = 0
		}
		a["phase"], a["left_s"], a["step_mhz"], a["steps"] = phase, int(left.Seconds()), float64(s.step)*sctStepMHz, sctSteps()
		a["temp_now"] = s.tempNow
		// the most it can still take: this window, then every remaining step
		rest := left
		if !s.counting {
			if s.step == 0 {
				rest += s.baseWin
			} else {
				rest += s.stepWin
			}
		}
		if n := sctSteps() - s.step; n > 0 {
			rest += time.Duration(n) * (sctSettle + s.stepWin)
		}
		if sctConfirmMax > 0 && s.confirm == 0 && s.step > 0 {
			rest += sctSettle + s.stepWin // the confirm window
		}
		a["confirm"] = s.confirm
		a["eta_max_min"] = int(math.Ceil(rest.Minutes()))
		v["now"] = a
	}
	if b, err := os.ReadFile(strongResult()); err == nil {
		var last map[string]any
		if json.Unmarshal(b, &last) == nil && last["running"] != true {
			v["last"] = last
		}
	}
	return v
}

// sctStart / sctStop: see handleStrongTest.
func sctStart(chain, baseMin, stepMin int) string {
	if !chipTrimBoardOK(chain) { // the model's boards: 0..3 on the SC Lite, only 0 on the SC Box II
		return "no such board"
	}
	bw, sw := sctBaseline, sctStepWin
	for _, p := range []struct {
		m int
		d *time.Duration
	}{{baseMin, &bw}, {stepMin, &sw}} {
		if p.m == 0 {
			continue
		}
		// (checked in minutes before multiplying: a huge number wraps time.Duration round into a short window)
		if p.m < int(sctMinWin/time.Minute) || p.m > int(sctMaxWin/time.Minute) {
			return fmt.Sprintf("the baseline and step windows are %d to %d minutes", int(sctMinWin.Minutes()), int(sctMaxWin.Minutes()))
		}
		*p.d = time.Duration(p.m) * time.Minute
	}
	controlMu.Lock() // no tuning run or limit probe can start between this check and the test's start
	defer controlMu.Unlock()
	chipJobMu.Lock()
	defer chipJobMu.Unlock()
	if sctRunning() {
		return "a strong-chip test is already running"
	}
	if _, err := os.Stat(strongBackup()); err == nil {
		sctRecover() // the last test's trims couldn't be put back when it ended: first that
		if _, err := os.Stat(strongBackup()); err == nil {
			return "the last strong-chip test's trims couldn't be put back yet (see its result): try again in a minute"
		}
	}
	if why := sctBlocked(); why != "" {
		return why
	}
	if !sctHookOn() {
		return "switch chip trims on first"
	}
	st, ok := sctReadStats()
	if !ok || len(st.Chains) == 0 {
		return "the trim library isn't reporting yet: wait until mining is running"
	}
	chips, base10, _, _, found := chainOf(st, chain)
	if !found || chips < 3 || base10 <= 0 {
		return fmt.Sprintf("board %d isn't mining (not fitted, or it didn't start)", chain)
	}
	b, ok := sctReadBoard(chain)
	if !ok {
		return "the miner isn't answering: try again in a minute"
	}
	if b.Idle {
		return "the miner is on Idle: start mining first"
	}
	if !b.Found || !b.Alive {
		return fmt.Sprintf("board %d isn't hashing", chain)
	}
	if !(b.Temp > 0) {
		return fmt.Sprintf("board %d has no temperature reading: the test needs one to watch the heat", chain)
	}
	if lim := sctAbortC() - sctStartGapC; b.Temp > lim {
		return fmt.Sprintf("board %d is at %.1f °C: the test needs it at %.0f °C or cooler (10 °C under the tuner's %.0f °C abort)", chain, b.Temp, lim, sctAbortC())
	}
	// the board's live clock, and the set one when it can be read (a stock level has no manual plan: then only
	// the live one): the fastest raised chip must stay in the clock window
	clk := float64(base10) / 10
	if set, ok := reapplySetClock(); ok && float64(set) > clk {
		clk = float64(set)
	}
	if clk+sctMaxMHz > float64(chipTrimCeil())+1e-9 {
		return fmt.Sprintf("the boards run %.4g MHz: chips up to +%.4g MHz faster would pass the %d MHz clock window (Unlock clock limit in Clock / voltage, or lower the clock)", clk, sctMaxMHz, chipTrimCeil())
	}
	cwMu.Lock() // auto-trim can't be between its pause check and its write (lock order: chipJobMu → cwMu → sct.mu → chipTrimMu)
	defer cwMu.Unlock()
	orig := readChipTrims()
	free := chips
	for _, t := range orig {
		if t.Chain == chain && t.MHz != 0 && t.Chip >= 1 && t.Chip <= chips {
			free--
		}
	}
	if free <= 0 {
		return fmt.Sprintf("every chip on board %d already has a trim", chain)
	}
	chipTrimMu.Lock()
	raw, err := os.ReadFile(chipTrimPath())
	if err != nil {
		raw = nil
	}
	werr := writeAtomic(strongBackup(), raw, 0o644) // synced before the result first says "running" (see sctRecover)
	chipTrimMu.Unlock()
	if werr != nil {
		return "couldn't back up the trims: " + werr.Error()
	}
	s := &sct
	s.mu.Lock()
	defer s.mu.Unlock()
	now := sctNow()
	s.reset()
	s.running, s.chain, s.base10, s.baseWin, s.stepWin, s.started, s.phaseAt = true, chain, base10, bw, sw, now, now
	s.orig, s.tempNow, s.stop = orig, b.Temp, make(chan struct{})
	sctLive.Store(true)
	s.logf("started on board %d at %.1f MHz: baseline %d min, steps of %+.4g MHz watched %d min each", chain, float64(base10)/10, int(bw.Minutes()), sctStepMHz, int(sw.Minutes()))
	s.save(true, "", "")
	est := runDur(sctEstimate(bw, sw))
	activity.emit(fmt.Sprintf("💪 strong-chip test started on board %d: %d min baseline, then up to %d steps of +%.4g MHz (at most %s)", chain, int(bw.Minutes()), sctSteps(), sctStepMHz, est))
	sctNotify(fmt.Sprintf("strong-chip test started on board %d (at most %s)", chain, est))
	stop := s.stop
	sctSpawn(func() { s.loop(stop) })
	return ""
}

// sctEndFor: the miner was put where the test can't go on (Idle, the pool guard's hold): end it now, every chip
// back as before, instead of showing it "settling" on a board that isn't hashing until sctUnsteadyMax.
func sctEndFor(why string) {
	s := &sct
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		s.finish("aborted", why)
	}
}

func sctStop() string {
	s := &sct
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.running {
		return "no strong-chip test is running"
	}
	s.finish("stopped", "Stopped by hand")
	return ""
}

// POST {start:true, chain, baseline_min, step_min} or {stop:true}; GET: the chip-trim view (with "strong").
func handleStrongTest(w http.ResponseWriter, r *http.Request) {
	if !featGate(w, model.Feat.ChipTrims, "the strong-chip test") {
		return
	}
	if r.Method == http.MethodPost {
		var in struct {
			Start   bool `json:"start"`
			Stop    bool `json:"stop"`
			Chain   *int `json:"chain"`
			BaseMin int  `json:"baseline_min"`
			StepMin int  `json:"step_min"`
		}
		e := "nothing to do"
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			e = fmt.Sprintf("the baseline and step windows are whole minutes, %d to %d", int(sctMinWin.Minutes()), int(sctMaxWin.Minutes()))
			in.Start, in.Stop = false, false
		}
		switch {
		case in.Stop:
			e = sctStop()
		case in.Start && in.Chain == nil:
			e = "pick a board"
		case in.Start:
			e = sctStart(*in.Chain, in.BaseMin, in.StepMin)
		}
		if e != "" {
			writeJSON(w, map[string]any{"ok": false, "err": e})
			return
		}
	}
	v := chipTrimView()
	v["ok"] = true
	writeJSON(w, v)
}
