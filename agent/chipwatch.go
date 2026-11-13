package main

// Chip auto-trim (the watchdog): while mining, slows a chip that keeps making far more hardware errors than
// the rest of its board, one 12.5 MHz step at a time, and keeps the step only if that chip's errors then
// actually drop. Off by default (Controls -> Chip trims -> Auto-trim); needs chip trims switched on.
//
// Every minute the miner's per-chip error counters (/dbg/icinfo) are read. Every 2 hours of steady mining
// (no miner restart, no board clock change, no tuner run or strong-chip search) the window is judged:
//   - a chip trimmed by the watchdog at the start of the window: kept if its errors fell clearly (at most
//     60 % of its rate before, and unlikely to be luck), otherwise that step is undone and the chip is left
//     alone for a day;
//   - otherwise the worst chip on each board, if it is clearly bad (at least 10 errors in the window, 6 times
//     the board's typical chip, and not luck), is slowed one step (to -50 MHz at most, 6 chips per board).
// One change per board per window, so each change is judged on its own. Trims set by hand are left
// alone (a chip whose trim was changed by hand stops being the watchdog's), except that a chip set by hand to run
// faster than its board is stepped back toward the board clock when it becomes one of the board's worst.

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	cwWindow = 2 * time.Hour // <BRAND>_AUTOTRIM_WINDOW / _EVERY (brandEnv) shorten these for testing
	cwEvery  = time.Minute
)

const (
	cwMinErr    = 10
	cwRatio     = 6.0
	cwBadP      = 1e-3
	cwKeepFrac  = 0.6
	cwKeepP     = 0.05
	cwMaxBoard  = 6
	cwFloor     = -50.0  // the SC Lite's deepest auto-trim (MHz); the SC Box II's is cwFloorBox2 (cwTrimFloor)
	cwFloorBox2 = -100.0 // the SC Box II's trim library takes -100 .. +50 MHz
	cwNoHelpFor = 24 * time.Hour
)

// cwTrimFloor: the deepest trim (MHz, negative) the auto-trim gives one chip: cwFloor on the SC Lite (as before),
// cwFloorBox2 on the SC Box II.
func cwTrimFloor() float64 {
	if model.Plan == planBox2V {
		return cwFloorBox2
	}
	return cwFloor
}

type cwOwned struct {
	Chain  int     `json:"chain"`
	Chip   int     `json:"chip"`
	MHz    float64 `json:"mhz"`
	Since  int64   `json:"since"`
	Why    string  `json:"why"`
	Before float64 `json:"before,omitempty"` // errors/hour before the latest step (while it's judged)
	Prev   float64 `json:"prev"`             // the trim before the latest step
	Judged bool    `json:"judged"`           // the latest step was kept
	Origin string  `json:"origin,omitempty"` // "tuner": kept by a tuning run and handed to the watchdog
}

type cwState struct {
	On     bool             `json:"on"`
	Owned  []cwOwned        `json:"owned"`
	NoHelp map[string]int64 `json:"nohelp"` // "chain.chip" -> left alone until (unix)
	// per board: the untrimmed chips' mean errors an hour when the watchdog last added a trim there, and until
	// when the board takes no new trims because they rose since (the trims were costing the other chips)
	BoardRate map[string]float64 `json:"board_rate,omitempty"`
	BoardHrs  map[string]float64 `json:"board_hrs,omitempty"` // the chip-hours BoardRate was counted over
	Budget    map[string]int64   `json:"budget,omitempty"`
	// chain -> board clock (tenths) last seen while watching: kept across pauses (a tuning run changes the clock
	// while auto-trim is paused, and the window is reset then) and restarts, so a clock change is always seen
	Base map[int]int      `json:"base,omitempty"`
	Log  []map[string]any `json:"log"` // newest last, at most 40
}

var (
	cwMu     sync.Mutex
	cwSt     cwState
	cwLoaded bool
	// the window being collected (memory only)
	cwStart    time.Time
	cwPrev     map[ckey]int
	cwGained   map[ckey]int
	cwBase     map[int]int // chain -> board clock (tenths) when the window started
	cwTrimSig  string      // the trims when the window started
	cwStatus   = "off"
	cwChecked  time.Time
	cwPairSaid = map[string]bool{} // pair-rule notes already logged ("chain.chip partner-trim"): once, not every window
	// injected I/O (tests replace these)
	cwReadErrors = readChipErrors
	cwReadStats  = func() (chipStatsFile, bool) {
		s, mod, ok := readChipStats()
		return s, ok && time.Since(mod) < 45*time.Second
	}
	cwApply   = func() error { _, err := reapplyTrims(); return err }
	cwNow     = time.Now
	cwBlocked = cwBlockedReal
)

// cwBlockedReal: why auto-trim pauses now ("" = it watches).
func cwBlockedReal() string {
	tunerMu.Lock()
	t := tunerCur
	tunerMu.Unlock()
	if t != nil && t.isRunning() {
		return "paused while the tuner runs"
	}
	if sctRunning() {
		return "paused while the strong-chip test runs"
	}
	if _, err := os.Stat(chipHookFlag()); err != nil {
		return "waiting: chip trims are switched off"
	}
	return ""
}

func chipWatchPath() string        { return filepath.Join(filepath.Dir(*authFile), "chipwatch.json") }
func cwKey(chain, chip int) string { return strconv.Itoa(chain) + "." + strconv.Itoa(chip) }

func cwLoad() {
	if cwLoaded {
		return
	}
	cwLoaded = true
	if b, err := os.ReadFile(chipWatchPath()); err == nil {
		if json.Unmarshal(b, &cwSt) != nil {
			// cut short (a power cut, a full flash): say so instead of coming up off without a word; the trims stay
			// in the trims file (as hand trims now), the unreadable copy is kept beside it
			cwSt = cwState{}
			os.Rename(chipWatchPath(), chipWatchPath()+".bad")
			cwLog("its saved state couldn't be read (cut short?): auto-trim is off and treats every trim as set by hand; switch it on again if you want it")
			activity.emit("✂️ chip auto-trim: its saved state couldn't be read, so it's off (switch it on again in Controls → Chip trims)")
		}
	}
	if cwSt.NoHelp == nil {
		cwSt.NoHelp = map[string]int64{}
	}
	// a damaged or hand-edited file: auto-trim only ever slows chips, so its trims and the trims it would undo
	// to are -100 .. 0 on the 12.5 MHz grid (a "prev" of +50 would be written back unchecked by an undo)
	onGrid := func(v float64) bool { return v >= trimMin && v <= 0 && v == math.Round(v/trimStep)*trimStep }
	keep := cwSt.Owned[:0]
	for _, o := range cwSt.Owned {
		// the model's boards (SC Lite 0..3, SC Box II only chain 0) and, on the Box II, its 36 chips
		chipOK := o.Chip >= 1 && o.Chip <= 63 && (model.Plan != planBox2V || o.Chip <= model.ChipsPerBoard)
		if chipTrimBoardOK(o.Chain) && chipOK && onGrid(o.MHz) && o.MHz != 0 {
			if !onGrid(o.Prev) {
				o.Prev = 0
			}
			keep = append(keep, o)
		}
	}
	cwSt.Owned = keep
}

func cwSave() {
	b, _ := json.MarshalIndent(cwSt, "", " ")
	writeAtomic(chipWatchPath(), b, 0o644) // flushed before the rename: a power cut can't leave it empty
}

func cwLog(msg string) {
	cwSt.Log = append(cwSt.Log, map[string]any{"t": cwNow().Unix(), "msg": msg})
	if len(cwSt.Log) > 40 {
		cwSt.Log = cwSt.Log[len(cwSt.Log)-40:]
	}
}

func cwResetWindow() {
	cwStart, cwPrev, cwGained, cwBase, cwTrimSig = time.Time{}, nil, nil, nil, ""
}

func chipWatchLoop() {
	if featOff(model.Feat.ChipTrims) {
		// chip auto-trim is a chip-trim feature (it writes per-chip trims): off on this model, so the
		// background watchdog never starts — no trims are ever written and no voltage/clock is touched.
		return
	}
	if d, err := time.ParseDuration(os.Getenv(brandEnv("AUTOTRIM_WINDOW"))); err == nil && d > 0 {
		cwWindow = d
	}
	if d, err := time.ParseDuration(os.Getenv(brandEnv("AUTOTRIM_EVERY"))); err == nil && d > 0 {
		cwEvery = d
	}
	for {
		time.Sleep(cwEvery)
		chipWatchTick()
	}
}

// errToHook maps the miner's error list (board slot, chip index as icinfo numbers them) to the hook's
// (chain, chip 1..n). On the SC Lite both number the same way: board slot 2 is chain 2 and icinfo's
// chips are 1..46 like the hook's (checked on a real unit: icinfo's per-chip work count follows the hook's
// nonce count chip for chip). A list that starts at 0 is shifted by one. ok=false when the board or chip
// isn't in the hook's counters.
func errToHook(k ckey, minIdx map[int]int, st chipStatsFile) (int, int, bool) {
	var chain *int
	for i := range st.Chains {
		if st.Chains[i].Chain == k.B {
			chain = &st.Chains[i].Chain
		}
	}
	if chain == nil && len(st.Chains) == 1 && len(minIdx) == 1 {
		chain = &st.Chains[0].Chain // one board on each side
	}
	if chain == nil {
		return 0, 0, false
	}
	chip := k.I
	if minIdx[k.B] == 0 {
		chip = k.I + 1
	}
	for _, c := range st.Chains {
		if c.Chain == *chain && chip >= 1 && chip <= c.Chips {
			return *chain, chip, true
		}
	}
	return 0, 0, false
}

func minIndexes(m map[ckey]int) map[int]int {
	out := map[int]int{}
	for k := range m {
		if v, ok := out[k.B]; !ok || k.I < v {
			out[k.B] = k.I
		}
	}
	return out
}

// poissonCDF: P(X <= n) for X ~ Poisson(lam).
func poissonCDF(n int, lam float64) float64 {
	if n < 0 {
		return 0
	}
	if lam <= 0 {
		return 1
	}
	// a big count (or a garbage counter): exp(-lam) underflows past ~745, and the sum below is O(n), with the
	// caller's lock held. The normal approximation (continuity-corrected) is plenty for these thresholds there.
	if lam > 400 || n > 4000 {
		return math.Min(1, math.Max(0, 0.5*math.Erfc(-(float64(n)+0.5-lam)/math.Sqrt(2*lam))))
	}
	sum, term := 0.0, math.Exp(-lam)
	for i := 0; i <= n; i++ {
		if i > 0 {
			term *= lam / float64(i)
		}
		sum += term
	}
	return math.Min(1, sum)
}

// poissonSF: P(X >= n).
func poissonSF(n int, lam float64) float64 { return math.Max(0, 1-poissonCDF(n-1, lam)) }

func chipWatchTick() {
	cwMu.Lock()
	defer cwMu.Unlock()
	cwLoad()
	cwChecked = cwNow()
	cwClockFix(cwChecked)
	if !cwSt.On {
		cwStatus = "off"
		cwResetWindow()
		if why := cwBlocked(); why == "" || strings.Contains(why, "switched off") {
			cwSyncOwned() // a reset or hand edit while auto-trim is off: those chips aren't its own any more
		}
		return
	}
	if why := cwBlocked(); why != "" {
		cwStatus = why
		cwResetWindow()
		return
	}
	st, ok := cwReadStats()
	if !ok {
		cwStatus = "waiting for the trim library to report"
		cwResetWindow()
		return
	}
	errs, err := cwReadErrors()
	if err != nil {
		cwStatus = "can't read the chips' errors (" + err.Error() + ")"
		return // keep the window: a missed reading doesn't spoil it
	}
	base := map[int]int{}
	for _, c := range st.Chains {
		base[c.Chain] = c.Base10
	}
	cwSyncOwned()
	cwClockSeen(base)
	sig := trimSig()
	if cwStart.IsZero() || !sameBase(base, cwBase) || sig != cwTrimSig {
		why := ""
		if !cwStart.IsZero() && !sameBase(base, cwBase) {
			why = "the board clock changed: "
		} else if !cwStart.IsZero() {
			why = "the trims changed: "
		}
		cwStart, cwPrev, cwGained, cwBase, cwTrimSig = cwNow(), errs, map[ckey]int{}, base, sig
		cwStatus = why + "collecting errors"
		return
	}
	for k, v := range errs {
		p, seen := cwPrev[k]
		if seen && v < p { // the miner restarted: its counters start again
			cwStart, cwPrev, cwGained = cwNow(), errs, map[ckey]int{}
			cwStatus = "the miner restarted: collecting errors again"
			return
		}
		if !seen {
			continue // new to the window, or back after a reading without it: its count so far is the baseline
		}
		cwGained[k] += v - p
	}
	next := make(map[ckey]int, len(errs))
	for k, p := range cwPrev {
		next[k] = p // missing from this reading (icinfo leaves out an idle chip or board): keep its last count
	}
	for k, v := range errs {
		next[k] = v
	}
	cwPrev = next
	el := cwNow().Sub(cwStart)
	if el < cwWindow {
		cwStatus = fmt.Sprintf("watching: %d of %d min collected", int(el.Minutes()), int(cwWindow.Minutes()))
		return
	}
	cwJudge(st, errs, el.Hours())
	cwStart, cwGained, cwTrimSig = cwNow(), map[ckey]int{}, trimSig()
	cwStatus = "watching: a new window started"
	cwSave()
}

// cwClockFix: the day-long pauses (a chip a trim didn't help, a board's trim budget) are saved as wall-clock
// deadlines. One set on the unset 1970 clock (no battery clock: before NTP or the browser set it) would end at
// once when the clock is set, and one set on a clock that ran ahead (set from a browser) would outlast the day
// once a time server stepped it back: the first starts its day again from now, the second ends a day from now.
// Caller holds cwMu.
func cwClockFix(now time.Time) {
	n, day := now.Unix(), int64(cwNoHelpFor/time.Second)
	dirty := false
	fix := func(m map[string]int64) {
		for k, until := range m {
			if until <= 0 {
				continue
			}
			if (unsetStamp(until) && n >= clockSetUnix) || until > n+day+3600 {
				m[k] = n + day
				dirty = true
			}
		}
	}
	fix(cwSt.NoHelp)
	fix(cwSt.Budget)
	if dirty {
		cwSave()
	}
}

func trimSig() string {
	var b strings.Builder
	for _, t := range readChipTrims() {
		fmt.Fprintf(&b, "%d.%d=%g ", t.Chain, t.Chip, t.MHz)
	}
	return b.String()
}

func sameBase(a, b map[int]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// cwClockSeen: a board whose clock changed (by hand, a preset, a tuning run) starts its trim budget over, and a
// step still waiting to be judged there can't be judged against errors counted at the old clock: it's kept, and
// slowed further later if the chip keeps erroring at the new clock.
func cwClockSeen(base map[int]int) {
	if cwSt.Base == nil {
		cwSt.Base = map[int]int{}
	}
	dirty := false
	for chain, b := range base {
		if b <= 0 {
			continue // no clock reported (the hook starting)
		}
		was, ok := cwSt.Base[chain]
		if ok && was == b {
			continue
		}
		cwSt.Base[chain] = b
		dirty = true
		if !ok {
			continue
		}
		bk := fmt.Sprint(chain)
		delete(cwSt.Budget, bk)
		delete(cwSt.BoardRate, bk)
		delete(cwSt.BoardHrs, bk)
		for i := range cwSt.Owned {
			if o := &cwSt.Owned[i]; o.Chain == chain && !o.Judged {
				o.Judged, o.Before = true, 0
				cwLog(fmt.Sprintf("board %d chip %d: the board clock changed before its %+.4g MHz step could be judged; the trim stays (it's slowed further if the chip keeps erroring)", o.Chain, o.Chip, o.MHz))
			}
		}
	}
	if dirty {
		cwSave()
	}
}

// cwSyncOwned: a chip whose trim was changed by hand (or reset) isn't the watchdog's any more.
func cwSyncOwned() {
	cur := map[string]float64{}
	for _, t := range readChipTrims() {
		cur[cwKey(t.Chain, t.Chip)] = t.MHz
	}
	var keep []cwOwned
	fixed := false
	for _, o := range cwSt.Owned {
		c := cur[cwKey(o.Chain, o.Chip)]
		if math.Abs(c-o.MHz) < 1e-6 {
			keep = append(keep, o)
		} else if !o.Judged && math.Abs(c-o.Prev) < 1e-6 {
			// the step being judged isn't in the file (a power cut between saving it and writing it, or its undo
			// written but not saved): the chip is where it was before that step, and still the watchdog's if trimmed
			fixed = true
			if o.Prev != 0 {
				o.MHz, o.Judged, o.Before = o.Prev, true, 0
				keep = append(keep, o)
			}
		} else {
			cwLog(fmt.Sprintf("board %d chip %d: its trim was changed by hand; the watchdog leaves it alone", o.Chain, o.Chip))
		}
	}
	if fixed || len(keep) != len(cwSt.Owned) {
		cwSt.Owned = keep
		cwSave()
	}
}

// cwHandEdit: the owner just set this chip's trim by hand (handleChipTrim, with cwMu held): a chip of the watchdog's
// isn't its own any more. Said here, not left to cwSyncOwned: a hand value equal to the one the step being judged
// started from would read there as that step not having landed (a power cut), and the chip would stay its own.
func cwHandEdit(chain, chip int, mhz float64) {
	cwLoad()
	keep := cwSt.Owned[:0]
	gone := false
	for _, o := range cwSt.Owned {
		if o.Chain == chain && o.Chip == chip && math.Abs(o.MHz-mhz) > 1e-6 {
			gone = true
			continue
		}
		keep = append(keep, o)
	}
	cwSt.Owned = keep
	if gone {
		cwLog(fmt.Sprintf("board %d chip %d: its trim was changed by hand; the watchdog leaves it alone", chain, chip))
		cwSave()
	}
}

func cwSetTrim(chain, chip int, mhz float64) error {
	chipTrimMu.Lock() // a read-modify-write: not at the same time as a hand edit or the tuner
	defer chipTrimMu.Unlock()
	return cwSetTrimLocked(chain, chip, mhz)
}

// cwSetTrimUnless: cwSetTrim, refused once dead is set (checked under chipTrimMu, so a trim from a run the
// watchdog ended can't land after the watchdog put the trims back).
func cwSetTrimUnless(dead *atomic.Bool, chain, chip int, mhz float64) error {
	chipTrimMu.Lock()
	defer chipTrimMu.Unlock()
	if dead.Load() {
		return errTunerDead
	}
	return cwSetTrimLocked(chain, chip, mhz)
}

// cwSetTrimWithin: cwSetTrim for the tuner's watchdog, which can't wait on chipTrimMu for good (a stuck run
// may hold it).
func cwSetTrimWithin(d time.Duration, chain, chip int, mhz float64) error {
	for end := time.Now().Add(d); !chipTrimMu.TryLock(); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(end) {
			return fmt.Errorf("the trims file is held by the stuck run")
		}
	}
	defer chipTrimMu.Unlock()
	return cwSetTrimLocked(chain, chip, mhz)
}

func cwSetTrimLocked(chain, chip int, mhz float64) error {
	ts := readChipTrims()
	found := false
	for i := range ts {
		if ts[i].Chain == chain && ts[i].Chip == chip {
			ts[i].MHz, found = mhz, true
		}
	}
	if !found {
		ts = append(ts, chipTrim{Chain: chain, Chip: chip, MHz: mhz})
	}
	return writeChipTrims(ts)
}

func cwJudge(st chipStatsFile, errs map[ckey]int, hours float64) {
	minIdx := minIndexes(errs)
	type row struct {
		k           ckey
		chain, chip int
		n           int
	}
	var rows []row
	byHook := map[string]row{}
	for k := range errs {
		chain, chip, ok := errToHook(k, minIdx, st)
		if !ok {
			continue
		}
		r := row{k, chain, chip, cwGained[k]}
		rows = append(rows, r)
		byHook[cwKey(chain, chip)] = r
	}
	if len(rows) == 0 {
		cwLog("couldn't match the miner's chip list to the hook's chips: nothing judged")
		return
	}
	changed := false
	// 1. the steps being judged
	judgedBoard := map[int]bool{}
	for i := range cwSt.Owned {
		o := &cwSt.Owned[i]
		if o.Judged {
			continue
		}
		judgedBoard[o.Chain] = true
		r, ok := byHook[cwKey(o.Chain, o.Chip)]
		if !ok {
			// not in the miner's chip list any more (no work and no errors since a restart): it can't be judged, and
			// waiting would keep its board "judging" for good. The step is undone, like one that didn't help.
			if err := cwSetTrim(o.Chain, o.Chip, o.Prev); err != nil {
				cwLog("undo failed: " + err.Error())
				continue
			}
			cwLog(fmt.Sprintf("board %d chip %d isn't in the miner's chip list any more: its %+.4g MHz step can't be judged, undone", o.Chain, o.Chip, o.MHz))
			o.MHz, o.Judged, o.Before = o.Prev, true, 0
			changed = true
			continue
		}
		lam := o.Before * hours
		if float64(r.n) <= cwKeepFrac*lam && poissonCDF(r.n, lam) < cwKeepP {
			o.Judged = true
			msg := fmt.Sprintf("board %d chip %d at %+.4g MHz: its errors fell from %.1f to %.1f an hour, the trim stays", o.Chain, o.Chip, o.MHz, o.Before, float64(r.n)/hours)
			cwLog(msg)
			activity.emit("✂️ chip auto-trim: " + msg)
			o.Before = 0
		} else {
			back := "the board clock"
			if o.Prev != 0 {
				back = fmt.Sprintf("%+.4g MHz", o.Prev)
			}
			msg := fmt.Sprintf("board %d chip %d: %+.4g MHz didn't lower its errors (%.1f an hour before, %.1f after): back to %s, left alone for a day",
				o.Chain, o.Chip, o.MHz, o.Before, float64(r.n)/hours, back)
			if err := cwSetTrim(o.Chain, o.Chip, o.Prev); err != nil {
				cwLog("undo failed: " + err.Error())
				continue
			}
			cwSt.NoHelp[cwKey(o.Chain, o.Chip)] = cwNow().Add(cwNoHelpFor).Unix()
			cwLog(msg)
			activity.emit("✂️ chip auto-trim: " + msg)
			notify(evChipTrim, "auto-trim: "+msg)
			o.MHz, o.Judged, o.Before = o.Prev, true, 0
			changed = true
		}
	}
	var owned []cwOwned
	for _, o := range cwSt.Owned {
		if o.MHz != 0 {
			owned = append(owned, o)
		}
	}
	cwSt.Owned = owned
	// 2. a new step on each board that isn't judging one
	cur := map[string]float64{}
	for _, t := range readChipTrims() {
		cur[cwKey(t.Chain, t.Chip)] = t.MHz
	}
	boards := map[int][]row{}
	for _, r := range rows {
		boards[r.chain] = append(boards[r.chain], r)
	}
	chains := make([]int, 0, len(boards))
	for c := range boards {
		chains = append(chains, c)
	}
	sort.Ints(chains)
	for _, chain := range chains {
		if judgedBoard[chain] {
			continue
		}
		rs := boards[chain]
		ns := make([]int, len(rs))
		for i, r := range rs {
			ns[i] = r.n
		}
		sort.Ints(ns)
		med := math.Max(float64(ns[len(ns)/2]), 0.5) // typical chip's errors in the window
		sort.Slice(rs, func(i, j int) bool { return rs[i].n > rs[j].n })
		nOwned, nAuto := 0, 0
		for _, o := range cwSt.Owned {
			if o.Chain == chain {
				nOwned++
				if o.Origin == "" {
					nAuto++
				}
			}
		}
		// the trim budget: every slowed chip takes a little voltage from the board's other stages (christhealien's
		// tests: 12 chips at -50 MHz cost the rest of the board 6.5 % of its good work, or 16.4 % with both
		// chips of 6 pairs slowed). If the untrimmed chips' errors have clearly risen since before this board's
		// auto-trims (at this clock), more trims would feed that: none for a day (or until the board clock changes),
		// and a notice to raise PV a step instead. The yardstick is every window before the board's first
		// auto-trim; both sides leave out the clearly bad chips (the ones trims are for: a new weak chip isn't the
		// rest of the board getting worse), the test allows for the yardstick's own luck, and it's only asked when
		// a chip is about to be slowed (stepping a "+" chip back gives the board voltage).
		bk := fmt.Sprint(chain)
		if cwSt.Budget == nil {
			cwSt.Budget = map[string]int64{}
		}
		if cwSt.BoardRate == nil {
			cwSt.BoardRate = map[string]float64{}
		}
		if cwSt.BoardHrs == nil {
			cwSt.BoardHrs = map[string]float64{}
		}
		untrimSum, untrimN := 0, 0
		for _, r := range rs {
			if cur[cwKey(chain, r.chip)] == 0 && !(r.n >= cwMinErr && float64(r.n) >= cwRatio*med) {
				untrimSum += r.n
				untrimN++
			}
		}
		untrimRate, untrimHrs := 0.0, float64(untrimN)*hours
		if untrimN > 0 {
			untrimRate = float64(untrimSum) / untrimHrs
		}
		if nAuto == 0 && untrimN > 0 { // no auto-trims here yet: this window adds to the yardstick
			h0 := cwSt.BoardHrs[bk]
			cwSt.BoardRate[bk] = (cwSt.BoardRate[bk]*h0 + float64(untrimSum)) / (h0 + untrimHrs)
			cwSt.BoardHrs[bk] = h0 + untrimHrs
		}
		budgetSpent := func() bool {
			if until := cwSt.Budget[bk]; until > cwNow().Unix() {
				return true
			}
			was, h0 := cwSt.BoardRate[bk], cwSt.BoardHrs[bk]
			if nAuto == 0 || was <= 0 || untrimN == 0 {
				return false
			}
			if h0 <= 0 {
				h0 = untrimHrs // saved by an older version: one window's worth
			}
			// were these errors and the yardstick's drawn at the same rate? Given both counts, this window's share
			// is binomial: is it clearly bigger than its share of the chip-hours? (binomSF: strongtest.go)
			x1 := int(math.Round(was * h0))
			if !(float64(untrimSum) > 1.5*was*untrimHrs && untrimSum >= cwMinErr &&
				binomSF(untrimSum, x1+untrimSum, untrimHrs/(h0+untrimHrs)) < cwBadP) {
				return false
			}
			again := cwSt.Budget[bk] != 0
			cwSt.Budget[bk] = cwNow().Add(cwNoHelpFor).Unix()
			msg := fmt.Sprintf("board %d: its untrimmed chips' errors rose from %.2f to %.2f an hour per chip since before its auto-trims; the trims are taking voltage from them, so no new trims on this board for a day. If the board keeps erroring, %s", chain, was, untrimRate, chipVoltFix())
			cwLog(msg)
			activity.emit("✂️ chip auto-trim: " + msg)
			if !again { // once per yardstick, not every day it's still so
				notify(evChipTrim, "auto-trim: "+msg)
			}
			changed = true
			return true
		}
		for _, r := range rs {
			if r.n < cwMinErr || float64(r.n) < cwRatio*med || poissonSF(r.n, 3*med) > cwBadP {
				break // sorted: no worse chip after this one
			}
			key := cwKey(chain, r.chip)
			if until, ok := cwSt.NoHelp[key]; ok && cwNow().Unix() < until {
				continue
			}
			var mine *cwOwned
			for i := range cwSt.Owned {
				if cwKey(cwSt.Owned[i].Chain, cwSt.Owned[i].Chip) == key {
					mine = &cwSt.Owned[i]
				}
			}
			now := cur[key]
			if now > 0 && mine == nil {
				// a chip set by hand to run faster than its board, and now among its worst: step it back toward
				// the board clock (the owner's choice otherwise stands; it's never slowed below the board here)
				next := math.Max(0, now-trimStep)
				if err := cwSetTrim(chain, r.chip, next); err != nil {
					cwLog("couldn't write the trim: " + err.Error())
					break
				}
				msg := fmt.Sprintf("board %d chip %d, set by hand to run %+.4g MHz faster, is making the board's most errors (%d, %.0f times its typical chip): lowered to %+.4g MHz", chain, r.chip, now, r.n, float64(r.n)/med, next)
				cwLog(msg)
				activity.emit("✂️ chip auto-trim: " + msg)
				notify(evChipTrim, "auto-trim: "+msg)
				changed = true
				break
			}
			if now != 0 && mine == nil {
				continue // trimmed by hand: not the watchdog's
			}
			if now-trimStep < cwTrimFloor()-1e-9 {
				continue
			}
			if mine == nil && nOwned >= cwMaxBoard {
				continue
			}
			// one trim per pair: the two chips of a pair share a power stage, and slowing both costs the rest
			// of the board far more than spreading the same trims one per pair. A slowed chip also gives its
			// partner a little voltage, which may be all the partner needed: it's left alone.
			if pc := chipPartner(r.chip); pc > 0 && cur[cwKey(chain, pc)] < 0 {
				if said := fmt.Sprintf("%s %g", key, cur[cwKey(chain, pc)]); !cwPairSaid[said] {
					cwPairSaid[said] = true
					cwLog(fmt.Sprintf("board %d chip %d is among the board's worst (%d errors), but its partner chip %d is already slowed: one trim per pair, so it's left alone (%s if it keeps erroring)", chain, r.chip, r.n, pc, chipVoltFix()))
				}
				continue
			}
			if budgetSpent() {
				break
			}
			next := now - trimStep
			rate := float64(r.n) / hours
			why := fmt.Sprintf("%d errors in %s, %.0f times the board's typical chip", r.n, humanDur(time.Duration(hours*float64(time.Hour)).Round(time.Minute)), float64(r.n)/med)
			var old cwOwned
			if mine == nil {
				cwSt.Owned = append(cwSt.Owned, cwOwned{Chain: chain, Chip: r.chip, MHz: next, Since: cwNow().Unix(), Why: why, Before: rate, Prev: now})
			} else {
				old = *mine
				mine.MHz, mine.Prev, mine.Before, mine.Judged, mine.Why = next, now, rate, false, why
			}
			// saved before the trim is written: a power cut between the two leaves a step that didn't land
			// (cwSyncOwned takes it back), never a trim in the file that auto-trim doesn't know is its own
			cwSave()
			if err := cwSetTrim(chain, r.chip, next); err != nil {
				if mine == nil {
					cwSt.Owned = cwSt.Owned[:len(cwSt.Owned)-1]
				} else {
					*mine = old
				}
				cwSave()
				cwLog("couldn't write the trim: " + err.Error())
				break
			}
			msg := fmt.Sprintf("board %d chip %d slowed to %+.4g MHz (%s); it's checked over the next %s", chain, r.chip, next, why, humanDur(cwWindow))
			cwLog(msg)
			activity.emit("✂️ chip auto-trim: " + msg)
			notify(evChipTrim, "auto-trim: "+msg)
			changed = true
			break // one change per board per window
		}
	}
	if changed {
		if err := cwApply(); err != nil {
			cwLog("saved, but not applied live (" + err.Error() + "): it applies at the next mining restart")
		}
	}
}

// chipWatchView: the watchdog's state for the Chip trims panel.
func chipWatchView() map[string]any {
	cwMu.Lock()
	defer cwMu.Unlock()
	cwLoad()
	owned := append([]cwOwned{}, cwSt.Owned...)
	sort.Slice(owned, func(i, j int) bool {
		if owned[i].Chain != owned[j].Chain {
			return owned[i].Chain < owned[j].Chain
		}
		return owned[i].Chip < owned[j].Chip
	})
	v := map[string]any{"on": cwSt.On, "status": cwStatus, "owned": owned, "log": cwSt.Log,
		"window_min": int(cwWindow.Minutes()), "floor": cwTrimFloor(), "max_board": cwMaxBoard}
	if !cwChecked.IsZero() {
		v["checked"] = cwChecked.Unix()
	}
	if !cwStart.IsZero() {
		v["window_started"] = cwStart.Unix()
	}
	return v
}

// chipWatchSet switches the watchdog on or off, or undoes its trims.
func chipWatchSet(on *bool, undo bool) (string, error) {
	cwMu.Lock()
	defer cwMu.Unlock()
	cwLoad()
	if on != nil {
		cwSt.On = *on
		cwResetWindow()
		if *on {
			cwStatus = "collecting errors"
			cwLog("switched on")
			activity.emit("✂️ chip auto-trim switched on")
		} else {
			cwStatus = "off"
			cwLog("switched off (its trims stay; Undo auto-trims removes them)")
			activity.emit("✂️ chip auto-trim switched off")
		}
		cwSave()
		return "", nil
	}
	if undo {
		cwSyncOwned() // a chip changed by hand since the last tick is the owner's: Undo mustn't reset it
		n := 0
		var keys []string
		for _, o := range cwSt.Owned {
			if err := cwSetTrim(o.Chain, o.Chip, 0); err != nil {
				return "", err
			}
			keys = append(keys, fmt.Sprintf("b%d.c%d", o.Chain, o.Chip))
			n++
		}
		cwSt.Owned, cwSt.NoHelp = nil, map[string]int64{}
		cwSt.Budget, cwSt.BoardRate, cwSt.BoardHrs = nil, nil, nil // no auto-trims left: the budget starts over
		cwResetWindow()
		cwLog(fmt.Sprintf("undone by hand: %d trim(s) removed%s", n, map[bool]string{true: " (" + strings.Join(keys, ", ") + ")", false: ""}[n > 0]))
		cwSave()
		activity.emit(fmt.Sprintf("✂️ chip auto-trim: %d trim(s) undone", n))
		return applyNote(), nil
	}
	return "", nil
}

// chipWatchAdopt: the trims a tuning run kept become the watchdog's, so it can slow those chips further if
// they keep making errors, and "Undo auto-trims" removes them too. Each made a failing test pass, so they
// count as kept (judged). Chips the watchdog already has are left as they are.
func chipWatchAdopt(ts []chipTrim, why string) {
	if len(ts) == 0 {
		return
	}
	cwMu.Lock()
	defer cwMu.Unlock()
	cwLoad()
	have := map[string]bool{}
	for _, o := range cwSt.Owned {
		have[cwKey(o.Chain, o.Chip)] = true
	}
	var names []string
	for _, t := range ts {
		if t.MHz == 0 || have[cwKey(t.Chain, t.Chip)] {
			continue
		}
		cwSt.Owned = append(cwSt.Owned, cwOwned{Chain: t.Chain, Chip: t.Chip, MHz: t.MHz, Since: cwNow().Unix(),
			Why: why, Judged: true, Origin: "tuner"})
		names = append(names, fmt.Sprintf("b%d.c%d %+.4g MHz", t.Chain, t.Chip, t.MHz))
	}
	if len(names) > 0 {
		cwLog("took over the tuner's trims: " + strings.Join(names, ", "))
		cwSave()
	}
}

// chipPartner: the other chip of chip c's power stage (pairs 1+2, 3+4 …; confirmed by the v1.5.x
// self-tests on the SC Lite — 46 chips / 23 pairs — and the same 2-chips-per-stage layout on the SC Box II
// — 36 chips / 18 pairs, product.json cpbs chips.layout all 2s). The upper bound is the model's per-board
// chip count, so chip 37.. correctly has no partner on the Box II; 0 for none.
func chipPartner(c int) int {
	if c < 1 || c > model.ChipsPerBoard {
		return 0
	}
	if c%2 == 1 {
		return c + 1
	}
	return c - 1
}
