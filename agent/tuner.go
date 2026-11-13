package main

// Auto-tuner, ported from the user's blake2b-asic-control (asic_tuner.py).
//
// It finds the lowest clean voltage for a clock (undervolt) or walks a clock curve up to a
// ceiling (clock-curve), judging each candidate by watching every chip's hardware-error rate
// against a Poisson-derived limit — the same statistics the Python uses. The judge is the
// safety-critical part and is ported faithfully (verified against the Python in tuner_test.go).
//
// Safety:
//   - Every candidate is time-boxed (settle + hold) and judged; a failing one is reverted.
//   - A board-temperature ceiling (abort_c) stops the run and restores the baseline plan.
//   - A miner restart during a test fails that test (its error counters reset).
//   - DryRun runs the entire decision loop reading real chip data but writes NOTHING — it only
//     logs what it WOULD apply, so the first run on hardware can be watched with zero risk.
//   - The run can be stopped at any time; on stop/abort the baseline plan is restored.
//
// I/O is injected (function fields) so the whole engine is unit-testable with a virtual clock.

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ---- statistics (pure; verified against asic_tuner.py) ----

func erfcInv(y float64) float64 {
	lo, hi := 0.0, 10.0
	for i := 0; i < 100; i++ {
		mid := (lo + hi) / 2
		if math.Erfc(mid) > y {
			lo = mid
		} else {
			hi = mid
		}
	}
	return (lo + hi) / 2
}

// poissonLimit: largest k with P(X>k) >= p for a Poisson mean lam.
func poissonLimit(lam, p float64) int {
	if lam < 0 {
		lam = 0
	}
	if lam > 400 {
		z := math.Sqrt2 * erfcInv(2*p)
		return int(math.Ceil(lam + z*math.Sqrt(lam)))
	}
	term := math.Exp(-lam)
	cdf := term
	k := 0
	for 1-cdf >= p && k < 100000 {
		k++
		term *= lam / float64(k)
		cdf += term
	}
	return k
}

// falseFail: chance a clean test fails somewhere by luck, at sensitivity sigma (5% at 2.5).
func falseFail(sigma float64) float64 {
	tail := func(z float64) float64 { return 0.5 * math.Erfc(z/math.Sqrt2) }
	v := 0.05 * tail(sigma) / tail(2.5)
	if v > 0.5 {
		return 0.5
	}
	return v
}

// tunerLimitVal: the most errors allowed over the whole test for one chip/board.
func tunerLimitVal(ratePerHour, hours, scale float64, weak bool, weakFactor, margin float64, n int, sigma float64) float64 {
	e := ratePerHour * hours * scale
	if weak {
		e *= weakFactor
	}
	nn := n
	if nn < 1 {
		nn = 1
	}
	pl := float64(poissonLimit(e, falseFail(sigma)/float64(nn)))
	return math.Max(margin, pl)
}

// pll: the clock the chips really run (12.5 MHz steps, rounding down).
func pll(mhz float64) float64 { return math.Floor(mhz/12.5) * 12.5 }

// ---- config ----

type ckey struct{ B, I int }

func chipName(k ckey) string { return fmt.Sprintf("b%d.c%d", k.B, k.I) }

type planMV struct {
	MHz int `json:"mhz"`
	MV  int `json:"mv"`
	PV  int `json:"pv"`
}

// pvOf: the PV of a point (a curve point may carry only mV: the SC Lite's stock gap then). Box II: no PV, the
// engine's voltage rank itself (as pvFor), never the SC Lite's mV + 300.
func (p planMV) pvOf() int {
	if p.PV > 0 {
		return p.PV
	}
	if tunerOnBox2() {
		return p.MV
	}
	return p.MV + 300
}

func (p planMV) text() string {
	if tunerOnBox2() { // Goldshell's voltage level (the engine's rank converted), no PV, no mV
		return fmt.Sprintf("%d MHz / %s", p.MHz, box2LevelText(box2CodeOf(p.MV)))
	}
	return fmt.Sprintf("%d MHz / PV %d (mV %d)", p.MHz, p.PV, p.MV)
}

type tunerConfig struct {
	// section 1 — what to do: "map" (every clock & voltage) | "confirm" (one setting for hours) | "rebuild" (presets only)
	Mode string `json:"mode"`
	// section 2 — baseline: source + minutes; for "own", BaselineMHz/MV
	BaselineSrc string  `json:"baseline_src"` // "stock" | "current" | "own"
	BaselineMHz int     `json:"baseline_mhz"` // used when BaselineSrc=="own"
	BaselineMV  int     `json:"baseline_mv"`
	BaselineMin float64 `json:"baseline_min"`
	// section 3 — clocks & voltages to map (map mode)
	LowestMHz  int     `json:"lowest_mhz"`
	HighestMHz int     `json:"highest_mhz"`
	Step       float64 `json:"step"` // MHz between clocks; a multiple of 12.5 walks the chips' real clock steps
	MinMV      int     `json:"min_mv"`
	MaxMV      int     `json:"max_mv"`
	MVStep     int     `json:"mv_step"`
	FindLowest bool    `json:"find_lowest"` // find the lowest voltage at every clock
	// TopDown: the old map walk (start at the baseline's voltage, up until clean, then down until it fails). The
	// default walks up only: the first clock starts at the lowest PV, each next clock at the lowest PV the clock
	// before passed at, one step up per fail. A fail usually ends in minutes while a pass takes the whole hold, so
	// paying the full time once per clock saves hours.
	TopDown bool `json:"top_down,omitempty"`
	// per-chip pass (needs chip trims switched on): when a test fails because of one chip, slow that chip
	// 12.5 MHz at a time (to -50, or ChipTrimDeep) and test again, instead of more voltage for the whole board. A trim stays
	// only if the test then passes.
	ChipTrim     bool    `json:"chip_trim"`
	ChipTrimMax  int     `json:"chip_trim_max"`            // chips per board the tuner may trim
	ChipTrimDeep float64 `json:"chip_trim_deep,omitempty"` // how far (MHz) the tuner may slow one chip: 25–75, 0 = 50
	ConfirmWin   bool    `json:"confirm_win"`              // confirm the winner with one more, longer test
	ConfirmX     float64 `json:"confirm_x"`                // × test length for the confirm
	// section 4 — PV
	PVGap int `json:"pv_gap"` // PV = mv + gap (auto)
	// section 5 — test length & safety
	HoldMin   float64 `json:"hold_min"` // minutes per test
	SettleMin float64 `json:"settle_min"`
	Sigma     float64 `json:"sigma"` // sensitivity
	AbortC    float64 `json:"abort_c"`
	EasyNoisy bool    `json:"easy_noisy"` // go easier on chips already noisy at baseline
	// section 6 — fans during the run
	FanHold    bool    `json:"fan_hold"`
	FanTargetC float64 `json:"fan_target_c"`
	FanMinPct  int     `json:"fan_min_pct"`
	HoldOn     string  `json:"hold_on"` // "board" | "chip"
	// section 7 — presets
	BuildPresets bool    `json:"build_presets"`
	PresetMin    float64 `json:"preset_min"`
	WarmestC     float64 `json:"warmest_c"`
	// confirm mode (section 1 = confirm): the single setting to run for hours
	ConfirmMHz   int     `json:"confirm_mhz"`
	ConfirmMV    int     `json:"confirm_mv"`
	ConfirmHours float64 `json:"confirm_hours"` // split into rounds as long as one test; a failed round gets one retest
	// internal / advanced (not shown in the wizard)
	Margin     float64 `json:"margin"`
	Pseudo     float64 `json:"pseudo"`
	WeakX      float64 `json:"weak_x"`
	WeakMin    float64 `json:"weak_min"`
	WeakFactor float64 `json:"weak_factor"`
	HashTol    float64 `json:"hash_tol"`
	PollS      float64 `json:"poll_s"`
	DryRun     bool    `json:"dry_run"`
}

// trimFloor: the deepest trim (MHz, negative) the tuner gives one chip.
func (e *tunerEngine) trimFloor() float64 {
	if d := e.cfg.ChipTrimDeep; d >= 25 && d <= 75 {
		return -d
	}
	return cwFloor
}

func defaultTunerConfig() tunerConfig {
	c := tunerConfig{
		Mode:        "map",
		BaselineSrc: "stock", BaselineMHz: 625, BaselineMV: 9100, BaselineMin: 25,
		LowestMHz: 575, HighestMHz: 675, Step: 25,
		MinMV: 8800, MaxMV: 9300, MVStep: 50, FindLowest: true, ConfirmWin: true, ConfirmX: 2,
		ChipTrim: false, ChipTrimMax: 3, ChipTrimDeep: 50,
		PVGap:   300,
		HoldMin: 25, SettleMin: 3, Sigma: 2.5, AbortC: 88, EasyNoisy: true,
		FanHold: true, FanTargetC: 70, FanMinPct: 30, HoldOn: "board",
		BuildPresets: false, PresetMin: 20, WarmestC: 75,
		ConfirmMHz: 625, ConfirmMV: 9000, ConfirmHours: 4,
		Margin: 2, Pseudo: 1, WeakX: 8, WeakMin: 20, WeakFactor: 1.5, HashTol: 0.04, PollS: 15, DryRun: true,
	}
	if tunerOnBox2() {
		box2TunerDefaults(&c)
	}
	return c
}

// startMV returns the baseline voltage (kept for engine math that referenced MV).
func (c tunerConfig) startMV() int { return c.BaselineMV }

func (c tunerConfig) pvFor(mv int) int {
	if tunerOnBox2() {
		return mv // no PV on this model: the engine's "PV" is the voltage level itself
	}
	pv := mv + c.PVGap
	if pv > pvHardMax {
		pv = pvHardMax // the supply's ceiling (validation keeps runs inside it, so this is only a backstop)
	}
	return pv
}

// ---- engine ----

// chartFor: which test the live chip chart belongs to (so the page can say so, and say when it has ended).
type chartFor struct {
	Phase   string `json:"phase"`
	MHz     int    `json:"mhz"`
	PV      int    `json:"pv"`
	MV      int    `json:"mv"` // Box II: the voltage level the miner is given (SC Lite: the run voltage)
	Started int64  `json:"started"`
	Done    bool   `json:"done"`
	Result  string `json:"result,omitempty"`
	Reason  string `json:"reason,omitempty"`
	Ended   int64  `json:"ended,omitempty"`
}

type tunerStats struct {
	Reason     string  // why the test ended ("clean", or what failed it)
	VoltMV     float64 // the boards' average voltage reading during the test (mV; 0 = none)
	Gained     map[ckey]int
	Limits     map[ckey]float64
	Worst      ckey
	HaveWorst  bool
	Total      int
	WatchedMin float64
	AvgGHS     float64
	MaxT       float64
	AvgFan     int // avg fan % the hold held during this watch (0 = not holding)
	FailChip   ckey
	FailByChip bool // the test failed because this one chip went over its own limit
}

type tunerEngine struct {
	cfg tunerConfig

	// injected I/O (wired to the agent in newTunerEngine; mocked in tests)
	readErrors func() (map[ckey]int, error)
	readTemp   func() (float64, bool)
	readGHS    func() (float64, bool)
	readWork   func() (float64, bool) // the miner's total work done so far, MH (a counter: never lags)
	elapsed    func() (int, bool)
	applyPlan  func(planMV) error
	applyFan   func(int) error // set both fans to a % (hold-steady mode)
	sleep      func(time.Duration)
	now        func() time.Time
	emit       func(string)
	trimGet    func() map[[2]int]float64                // the trims file: (chain, chip) -> MHz
	trimSet    func(chain, chip int, mhz float64) error // write one trim and apply it live
	hookStats  func() (chipStatsFile, bool)
	adoptTrims func([]chipTrim, string) // hand the run's kept trims to auto-trim
	// what the miner ran before the run: a way to put it back exactly (setting, firmware level), and its text
	before func() (restore func() error, what string)
	// verifyPlan: "" when the miner runs p, else what differs (its setting read back); nil: not checked
	verifyPlan func(planMV) string
	readFan    func() (int, bool)     // the fan % the miner is on now (the fan hold starts there)
	readVolt   func() (float64, bool) // what the boards read as their voltage now (mV, median of the boards)
	// miningStops: how often the mining program stopped (crashed) since a time; nil: not known
	miningStops  func(time.Time) int
	fanFloor     func() int    // the lowest fan % the panel sets now (raised after the watchdog flagged a fan)
	fanGuard     func(int) int // after setting the fans: the new floor if the watchdog flagged one (0: not)
	backendWait  time.Duration // how long a backend that doesn't answer is waited for before the run stops
	backendGrace time.Duration // mid-test: how much longer, after backendWait, before the run stops (the test then starts over)
	gapRetried   bool          // this test already started over once after a gap in the chip readings
	restoreWait  time.Duration // how long putting the miner back is retried at the end

	lastApply     time.Time // when the setting under test was applied
	restoreFailed bool      // the end couldn't put the miner back (the boot-time recovery tries again)

	fanCur     int         // last fan % the hold set (start of a run = current)
	holdTarget float64     // temperature the hold is aiming at (changes per preset)
	confirmed  bool        // a confirm run passed — stay on the setting (don't restore baseline)
	fanAt      map[int]int // clock MHz -> fan % that held the target (for preset curves)

	// the watchdog (superviseTuner): none of these need e.mu, so a run frozen while holding it is still seen
	done      chan struct{}          // closed when Run returns (made by begin)
	beat      atomic.Int64           // monoNanos() of the run's last sign of life (a poll, a log line, a retry)
	dead      atomic.Bool            // the watchdog ended this run: its hooks do nothing any more
	deadWhy   atomic.Pointer[string] // what the watchdog told the owner (the Tuner tab shows it)
	baseAtom  atomic.Pointer[planMV]
	chargeNow func() string // who's in charge (getCharge): an Idle a safety stop took during the run isn't undone

	mu           sync.Mutex
	running      bool
	phase        string
	statusMsg    string
	baseRate     map[ckey]float64
	weak         map[ckey]float64 // chip -> multiple of median
	baseGHS      float64
	baseMHz      int
	curve        []planMV
	best         *planMV
	steps        []tunerStep
	logbuf       []string // last ~400 lines, for the live panel
	fullLog      []string // every line this run, for the Log (.txt) export
	results      []testResult
	readings     []liveReading
	startedAt    time.Time
	finishedAt   time.Time
	baseUsed     planMV       // this run's baseline (what it goes back to)
	backWhat     string       // what the end puts back: what the miner ran before the run ("" = baseUsed)
	endHeld      string       // the hold the end left the miner on Idle for ("" = none)
	abortWhy     string       // why the run (or a phase) ended early on its own
	onStart      func()       // called once the plan is built (notification, restart marker)
	onApply      func(planMV) // called after each setting is applied (the restart marker notes what's under test)
	stopCh       chan struct{}
	stopWhy      string                   // who stopped the run when it wasn't the user ("the pool guard: …"); "" = the user
	held         func() error             // a hold (fan lock, pool guard) keeping the miner on Idle: nothing is put back over it
	onTrims      func(map[[2]int]float64) // the run's kept trims changed (the restart marker records them)
	lastStat     tunerStats
	testMaps     []testChipMap
	persist      bool                          // save chip maps to the tuner folder (off in tests)
	live         tunerStats                    // the test being watched now, updated every poll (lastStat: the last finished one)
	liveFor      chartFor                      // which test live belongs to (and whether it has ended)
	lastFor      chartFor                      // the test that ended before liveFor's (shown while the next setting settles)
	cur          *curStep                      // the test running right now (nil between steps) — drives the Current-step panel
	runTrims     map[[2]int]float64            // trims this run set and kept (each made a failing test pass)
	trimsAtPass  map[[2]int]map[[2]int]float64 // (MHz, mV) -> the run's trims when the map first ran it clean
	prevCharge   string                        // who was in charge before the run (set by the start handler)
	planGen      int64                         // safetyGen as the run was started (noGen: none noted): see safetyGen
	trimsUndone  int                           // trims this run made that came off at its end (no confirmed setting)
	att          attCtx                        // the run-plan item the next test is an attempt of
	noTemp       int                           // readings in a row without a temperature
	hotN         int                           // readings in a row at or over the abort temperature (one wild sample doesn't abort)
	noTempAt     time.Time                     // since when
	begun        bool                          // begin() ran: Run uses its stop channel
	lastSettled  planMV                        // the setting the last settle was for
	trimStats    tunerStats                    // the last test trimAndRetest ran
	lastWatchEnd time.Time                     // when the last test ended
	fh           fanHold
	passes       []planMV              // every clean test of the curve, in order (the confirm's candidates)
	marginal     map[[2]int]string     // clean but close: why (a chip near its limit, or clean only on the retest)
	testStats    map[[2]int]tunerStats // the last test of each (MHz, mV), for reuse (High reuses its confirm)
	vSum         float64               // the boards' voltage readings during the test being watched
	vN           int
	lifted       map[[2]int]float64 // trims the trim check lifted (their value before), at liftedAt
	liftedTo     map[[2]int]float64 // ... and their value after it (0 = off)
	liftedAt     planMV
	trimFutile   map[futileKey]bool // (clock, chip) that still failed at the deepest trim
	floorPV      int                // the lowest PV that still lowers the boards' voltage (0 = not found): below it the miner holds
}

// attCtx: which run-plan item a test belongs to (the curve's clock, a confirm candidate, a preset), and how
// it's labelled ("retest", "after a chip trim", "confirm").
type attCtx struct {
	key  string // plan phase: "curve", "confirm", "presets"
	mhz  int    // for "curve": the clock row
	item string // for the others: the item's label
	tag  string
}

// curStep is a snapshot of the test being watched right now, for the Current-step panel's tiles
// and progress bar. Rebuilt each poll under e.mu.
type curStep struct {
	Phase      string  `json:"phase"`
	MHz        int     `json:"mhz"`
	MV         int     `json:"mv"`
	PV         int     `json:"pv"`
	ElapsedMin float64 `json:"elapsed_min"`
	HoldMin    float64 `json:"hold_min"`
	Fan        int     `json:"fan"`
	TargetC    float64 `json:"target_c"`
	Settling   bool    `json:"settling"`   // the new setting is settling: nothing is counted yet
	SettleMin  float64 `json:"settle_min"` // how long it settles
	Updated    int64   `json:"updated"`    // when the chips were last read (unix)
	started    time.Time
}

// testResult is one completed judged test — a row in the finished-tests table / Results (.csv).
type testResult struct {
	Time   string  `json:"time"`
	MHz    int     `json:"mhz"`
	MV     int     `json:"mv"`
	PV     int     `json:"pv"`
	Result string  `json:"result"` // clean | failed | aborted | stopped
	THS    float64 `json:"ths"`
	MaxT   float64 `json:"max_t"`
	Fan    int     `json:"fan"`
	ErrAdd int     `json:"err_add"`
	MinS   float64 `json:"min_s"`
	Reason string  `json:"reason"`
}

// testChipMap is one finished test's chip map: every chip's new errors against its limit. Kept for each
// test (and saved to chipmap.json in the tuner folder) so any earlier test can be shown again.
type testChipMap struct {
	N      int         `json:"n"`
	Time   string      `json:"time"`
	MHz    int         `json:"mhz"`
	MV     int         `json:"mv"`
	PV     int         `json:"pv"`
	Result string      `json:"result"`
	Reason string      `json:"reason"`
	MinS   float64     `json:"min_s"`
	Judged bool        `json:"judged"`
	Chips  [][]float64 `json:"chips,omitempty"` // [board, chip, new errors, limit, normal errors/hour]
}

// liveReading is one poll sample during a test — a row in Live readings (.csv).
type liveReading struct {
	Time string  `json:"time"`
	MHz  int     `json:"mhz"`
	MV   int     `json:"mv"`
	GHS  float64 `json:"ghs"`
	MaxT float64 `json:"max_t"`
	Err  int     `json:"err"`
	Fan  int     `json:"fan"`
}

func (e *tunerEngine) recordResult(p planMV, clean bool, reason string, st tunerStats) {
	res := "failed"
	switch {
	case clean:
		res = "clean"
	case reason == "stopped":
		res = "stopped"
	case strings.Contains(reason, "°C") || strings.Contains(reason, "overheat"):
		res = "aborted"
	}
	e.mu.Lock()
	if e.liveFor.MHz == p.MHz && e.liveFor.PV == p.pvOf() && !e.liveFor.Done {
		e.liveFor.Done, e.liveFor.Result, e.liveFor.Reason, e.liveFor.Ended = true, res, reason, e.now().Unix()
	}
	h := p.hw() // (the code on the Box II: the views and files show what the miner was given)
	e.results = append(e.results, testResult{
		Time: e.now().Format("15:04:05"), MHz: p.MHz, MV: h.MV, PV: h.PV,
		Result: res, THS: st.AvgGHS / 1000.0, MaxT: st.MaxT, Fan: st.AvgFan,
		ErrAdd: st.Total, MinS: st.WatchedMin, Reason: reason,
	})
	m := testChipMap{N: len(e.testMaps), Time: e.now().Format("2006-01-02 15:04"), MHz: p.MHz, MV: h.MV, PV: h.PV,
		Result: res, Reason: reason, MinS: st.WatchedMin, Judged: true}
	for k, g := range st.Gained {
		m.Chips = append(m.Chips, []float64{float64(k.B), float64(k.I), float64(g), math.Round(st.Limits[k]*10) / 10,
			math.Round(e.baseRate[k]*10) / 10})
	}
	sort.Slice(m.Chips, func(i, j int) bool {
		if m.Chips[i][0] != m.Chips[j][0] {
			return m.Chips[i][0] < m.Chips[j][0]
		}
		return m.Chips[i][1] < m.Chips[j][1]
	})
	e.testMaps = append(e.testMaps, m)
	maps := append([]testChipMap(nil), e.testMaps...)
	started, persist := e.startedAt, e.persist
	e.mu.Unlock()
	if persist {
		saveChipMaps(started, maps)
		saveTunerFiles(e.exportBundle()) // the run's own files, kept up to date: a stuck run and a reboot don't lose them
	}
}

// chipMaps: when the run started and its finished tests' chip maps (newest last).
func (e *tunerEngine) chipMaps() (time.Time, []testChipMap) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.startedAt, append([]testChipMap(nil), e.testMaps...)
}

func (e *tunerEngine) recordReading(p planMV, ghs, maxT float64, totalErr, fan int) {
	e.mu.Lock()
	if len(e.readings) < 20000 {
		e.readings = append(e.readings, liveReading{
			Time: e.now().Format("15:04:05"), MHz: p.MHz, MV: p.hw().MV,
			GHS: ghs, MaxT: maxT, Err: totalErr, Fan: fan,
		})
	}
	e.mu.Unlock()
}

func newTunerEngine(cfg tunerConfig) *tunerEngine {
	if tunerOnBox2() {
		cfg = box2TunerLevels(cfg) // Goldshell's levels in, ranks inside the engine (clock only while the direction is unknown)
	}
	e := &tunerEngine{cfg: cfg, now: time.Now, sleep: time.Sleep, backendWait: 3 * time.Minute, backendGrace: 10 * time.Minute, restoreWait: 10 * time.Minute, planGen: noGen}
	e.readErrors = readChipErrors
	e.miningStops = intminerStopsSince
	e.fanFloor, e.fanGuard = fanFloor, fanGuardPair // (judged at the lower fan pair)
	e.readTemp = func() (float64, bool) { t, _, ok := maxBoardTemp(buildDashboard()); return t, ok }
	e.readGHS = func() (float64, bool) { d := buildDashboard(); return d.GHS, d.OK }
	e.readWork = readTotalMH
	e.elapsed = func() (int, bool) { d := buildDashboard(); return d.Elapsed, d.OK }
	e.applyPlan = func(p planMV) error {
		if e.dead.Load() {
			return errTunerDead
		}
		return applyTunePlanGen(e.planGen, p)
	}
	e.applyFan = func(pct int) error {
		if e.dead.Load() {
			return errTunerDead
		}
		return applyTuneFan(pct)
	}
	e.chargeNow = getCharge
	e.emit = func(s string) { activity.emit("🔧 tuner: " + s) }
	e.trimGet = func() map[[2]int]float64 {
		m := map[[2]int]float64{}
		for _, t := range readChipTrims() {
			m[[2]int{t.Chain, t.Chip}] = t.MHz
		}
		return m
	}
	e.trimSet = func(chain, chip int, mhz float64) error {
		if e.dead.Load() {
			return errTunerDead
		}
		if err := cwSetTrimUnless(&e.dead, chain, chip, mhz); err != nil { // (takes chipTrimMu itself)
			return err
		}
		// the hook reports every 10 s and any setting write (the fan hold's too) makes its last report stale
		var err error
		for i := 0; i < 4; i++ {
			if _, err = reapplyTrims(); err == nil || !strings.Contains(err.Error(), "fresh") {
				break
			}
			if _, at, ok := readChipStats(); !ok || time.Since(at) > 45*time.Second {
				break // the hook stopped reporting: no use waiting
			}
			e.sleep(10 * time.Second)
		}
		return err
	}
	e.hookStats = func() (chipStatsFile, bool) { return cwReadStats() }
	e.adoptTrims = chipWatchAdopt
	e.before = snapshotMinerSetting
	e.verifyPlan = verifyTunePlan
	e.held = func() error { // the fan lock, the pool guard, or a safety stop's (or the owner's) Idle since the start
		if err := miningHoldErr(); err != nil {
			return err
		}
		if genMoved(e.planGen) {
			return errSafetyStop
		}
		return nil
	}
	e.readVolt = func() (float64, bool) { v := boardVoltMV(buildDashboard()); return v, v > 0 }
	e.readFan = func() (int, bool) {
		pp, ok := readStockBaselineNow("current")
		return pp.Fan1, ok && pp.Fan1 > 0
	}
	if tunerOnBox2() {
		e.applyPlan = func(p planMV) error {
			if e.dead.Load() {
				return errTunerDead
			}
			return box2TunerApply(e.planGen, p)
		}
		e.verifyPlan = func(p planMV) string { return verifyTunePlan(p.hw()) }
		e.before = box2TunerBefore
		e.readVolt = nil // no voltage reading to search on (PV reads 0; the floor check works in PV): "V" is Goldshell's level
	}
	e.fanAt = map[int]int{}
	e.fanCur = e.cfg.FanMinPct
	return e
}

// logQuiet appends to the tuner log only (shown in the panel); no activity-feed emit.
func (e *tunerEngine) logQuiet(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	line := e.now().Format("15:04:05") + "  " + msg
	e.alive()
	e.mu.Lock()
	e.logbuf = append(e.logbuf, line)
	if len(e.logbuf) > 400 {
		e.logbuf = e.logbuf[len(e.logbuf)-400:]
	}
	e.fullLog = append(e.fullLog, line) // uncapped-ish: the full run record for export
	if len(e.fullLog) > 20000 {
		e.fullLog = e.fullLog[len(e.fullLog)-20000:]
	}
	e.mu.Unlock()
}

// log appends to the tuner log and also emits to the activity feed (phase changes, results).
func (e *tunerEngine) log(format string, a ...any) {
	e.logQuiet(format, a...)
	// a run (or its presets phase) ending early on its own: remembered for the notification
	if m := fmt.Sprintf(format, a...); strings.HasPrefix(m, "ABORT: ") || strings.HasPrefix(m, "STOP: ") {
		e.noteAbort(m[strings.Index(m, " ")+1:])
	} else if strings.HasPrefix(m, "REBUILD: no saved curve") {
		e.noteAbort("no saved curve from a map run to rebuild presets from")
	} else if strings.Contains(m, " aborted: ") && strings.HasPrefix(m, "preset ") {
		e.noteAbort(strings.TrimSuffix(m, " — stopping presets phase"))
	}
	if e.emit != nil {
		e.emit(fmt.Sprintf(format, a...))
	}
}

func (e *tunerEngine) setStatus(phase, msg string) {
	e.mu.Lock()
	e.phase, e.statusMsg = phase, msg
	e.mu.Unlock()
}

// alive: the run is making progress (the watchdog's heartbeat; real time, not e.now). On the monotonic clock:
// with the wall clock, a run started before the clock was set (1970 after a boot) looked stuck for 56 years
// the moment NTP or the browser set it, and was ended from outside; a clock stepped back hid a stuck run.
func (e *tunerEngine) alive() { e.beat.Store(monoNanos()) }

// beatAge: how long since the heartbeat beat (at monoNanos() beat; 0 = never: as long ago as can be).
func beatAge(beat int64) time.Duration {
	if beat == 0 {
		return time.Duration(math.MaxInt64)
	}
	return time.Duration(monoNanos() - beat)
}

func (e *tunerEngine) stopped() bool {
	if e.dead.Load() {
		return true
	}
	select {
	case <-e.stopCh:
		return true
	default:
		return false
	}
}

// abortErr distinguishes a safety abort (restore baseline) from an ordinary test failure.
type abortErr struct{ msg string }

func (a abortErr) Error() string { return a.msg }

// sleepPoll sleeps in poll-sized chunks, checking for stop. Returns false if stopped.
func (e *tunerEngine) sleepPoll(total time.Duration) bool {
	step := time.Duration(e.cfg.PollS * float64(time.Second))
	if step <= 0 {
		step = time.Second
	}
	end := e.now().Add(total)
	for e.now().Before(end) {
		e.alive()
		if e.stopped() {
			return false
		}
		d := step
		if r := end.Sub(e.now()); r < d {
			d = r
		}
		e.sleep(d)
	}
	return !e.stopped()
}

// checkTemp reads the board temp and returns an abortErr if at/over the ceiling.
func (e *tunerEngine) checkTemp() error {
	t, ok := e.readTemp()
	if math.IsNaN(t) || math.IsInf(t, 0) {
		ok = false
	}
	if !ok {
		// no temperature means no heat abort and no fan hold: don't keep testing blind
		// (Goldshell's own over-temperature shutdown keeps working meanwhile.) A short backend drop is waited
		// out, as long as the backend wait plus 2 minutes; then the run stops.
		if e.noTemp == 0 {
			e.noTempAt = e.now()
		}
		e.noTemp++
		if e.noTemp >= 3 && e.now().Sub(e.noTempAt) >= e.backendWait+2*time.Minute {
			return abortErr{fmt.Sprintf("the miner reported no board temperature for %.0f min (%d readings)", e.now().Sub(e.noTempAt).Minutes(), e.noTemp)}
		}
		return nil
	}
	e.noTemp = 0
	if t < e.cfg.AbortC {
		e.hotN = 0
	} else if e.hotN++; e.hotN >= 2 { // a board sensor now and then reads one wild sample (67.9 → 95.9 → 67.9)
		return abortErr{fmt.Sprintf("board reached %.1f°C (abort at %.0f°C)", t, e.cfg.AbortC)}
	}
	return nil
}

// fanHold holds the hottest board at a target temperature with the fans (blake2b-asic-control's FanHold,
// a PI controller tuned on a simulated SC Lite whose temperature follows the fan a few readings late):
//   - fan % = start + 3 per °C over the target + a slow integral (0.3 per reading per °C, kept within ±25
//     and only grown while the fan isn't pinned at its limit in that direction);
//   - up at most 5 a reading (10 when more than 3 °C over), down at most 2, and not down while warming;
//   - full fan from 8 °C over; leaving full fan it drops back to the level it last held steadily;
//   - it starts from the fan the miner is on, and re-sends every 60 s (the firmware walks it back).
type fanHold struct {
	init       bool
	base, i    float64
	lastT      float64
	haveLast   bool
	full       bool
	steady     int
	haveSteady bool
	sent       time.Time
}

func (h *fanHold) want(temp, target float64, cur, fanMin, fanMax int) int {
	const kp, ki, iMax, up, down, fullAt = 3.0, 0.3, 25.0, 5, 2, 8.0
	err := temp - target
	rising := h.haveLast && temp > h.lastT+0.2
	h.lastT, h.haveLast = temp, true
	if err >= fullAt {
		h.full = true
		return fanMax
	}
	if h.full { // coming off full fan: back to where it held before
		h.full, h.i = false, 0
		back := int(h.base)
		if h.haveSteady {
			back = h.steady
		}
		return max(fanMin, min(fanMax, max(back, cur-3*down)))
	}
	atMin, atMax := cur <= fanMin, cur >= fanMax
	if math.Abs(err) > 0.5 && !((atMin && err < 0) || (atMax && err > 0)) {
		h.i = math.Max(-iMax, math.Min(iMax, h.i+ki*err))
	}
	w := h.base + kp*err + h.i
	c := float64(cur)
	switch {
	case w > c:
		st := float64(up)
		if err > 3 {
			st = 2 * up
		}
		w = math.Min(c+st, w)
	case rising:
		w = c // still warming: don't take fan away
	default:
		w = math.Max(c-down, w)
	}
	out := int(math.Round(math.Max(float64(fanMin), math.Min(float64(fanMax), w))))
	if math.Abs(err) <= 1.5 {
		h.steady, h.haveSteady = out, true
	}
	return out
}

// holdFan: one reading of the fan hold. Returns the fan % now set. A no-op in dry-run or with fan-hold off.
func (e *tunerEngine) holdFan(target float64) int {
	if !e.cfg.FanHold || e.cfg.DryRun {
		return e.fanCur
	}
	t, ok := e.readTemp()
	if !ok || math.IsNaN(t) || math.IsInf(t, 0) {
		return e.fanCur
	}
	if !e.fh.init {
		start := 70
		if e.readFan != nil {
			if f, ok := e.readFan(); ok && f > 0 {
				start = f
			}
		}
		start = max(e.cfg.FanMinPct, min(100, start))
		e.fh = fanHold{init: true, base: float64(start)}
		e.fanCur = start
	}
	lo := e.cfg.FanMinPct
	if e.fanFloor != nil {
		lo = max(lo, e.fanFloor())
	}
	want := e.fh.want(t, target, e.fanCur, lo, 100)
	if want != e.fanCur || e.now().Sub(e.fh.sent) >= time.Minute {
		if err := e.applyFan(want); err == nil {
			e.fanCur, e.fh.sent = want, e.now()
		}
	}
	if e.fanGuard != nil {
		if f := e.fanGuard(e.fanCur); f > 0 {
			e.log("  %s watchdog flagged a fan at %d %% (it stops mining while a fan looks failed): the fans stay at %d %% or more from now on", OemOf(), e.fanCur, f)
			if e.fanCur < f && e.applyFan(f) == nil {
				e.fanCur, e.fh.sent = f, e.now()
			}
		}
	}
	return e.fanCur
}

// settle applies a plan (or logs the intent in dry-run) and waits minutes (0 = SettleMin), reading the
// temperature and holding the fans every poll. Then it checks that the miner runs the setting (read
// back): if not, it applies it again and waits up to a minute; a miner that still runs something else
// stops the run (blake2b-asic-control's not_applied).
func (e *tunerEngine) settle(p planMV, minutes float64) error {
	if minutes <= 0 {
		minutes = e.cfg.SettleMin
		// the same setting again straight after its own test (a retest): it's already settled
		if p == e.lastSettled && !e.lastWatchEnd.IsZero() && e.now().Sub(e.lastWatchEnd) < 2*time.Minute {
			minutes = math.Min(minutes, 1)
		}
	}
	e.lastSettled = p
	if e.cfg.DryRun {
		e.log("DRY-RUN would apply %s", p.text())
	} else {
		if err := e.applyWait(p); err != nil {
			if err == errTunerStopped {
				return abortErr{"stopped"}
			}
			return abortErr{"apply failed: " + err.Error()}
		}
		e.lastApply = e.now()
		e.log("applied %s, settling %gmin", p.text(), minutes)
		if e.onApply != nil {
			e.onApply(p)
		}
	}
	if err := e.checkTemp(); err != nil {
		return err
	}
	// a new setting: the live view starts over (nothing is counted while it settles)
	e.mu.Lock()
	e.live = tunerStats{}
	if e.liveFor.Done {
		e.lastFor = e.liveFor // its result stays on the page while the next setting settles
	}
	e.liveFor = chartFor{Phase: e.phase, MHz: p.MHz, PV: p.pvOf(), MV: p.hw().MV, Started: e.now().Unix()}
	e.cur = &curStep{Phase: e.phase, MHz: p.MHz, MV: p.hw().MV, PV: p.PV, Settling: true, SettleMin: minutes,
		HoldMin: e.cfg.HoldMin, Fan: e.fanCur, TargetC: e.holdTarget, started: e.now()}
	e.mu.Unlock()
	poll := time.Duration(e.cfg.PollS * float64(time.Second))
	if poll <= 0 {
		poll = time.Second
	}
	end := e.now().Add(time.Duration(minutes * float64(time.Minute)))
	for e.now().Before(end) {
		d := poll
		if r := end.Sub(e.now()); r < d {
			d = r
		}
		if !e.sleepPoll(d) {
			return abortErr{"stopped"}
		}
		if err := e.checkTemp(); err != nil {
			return err
		}
		e.holdFan(e.holdTarget)
	}
	if !e.cfg.DryRun && e.verifyPlan != nil {
		if diff := e.verifyPlan(p); diff != "" {
			e.log("  %s not in effect yet (%s): applying it again", p.text(), diff)
			if err := e.applyPlan(p); err != nil {
				return abortErr{"apply failed: " + err.Error()}
			}
			for i := 0; i < 6 && diff != ""; i++ {
				if !e.sleepPoll(10 * time.Second) {
					return abortErr{"stopped"}
				}
				if err := e.checkTemp(); err != nil {
					return err
				}
				diff = e.verifyPlan(p)
			}
			if diff != "" {
				return abortErr{fmt.Sprintf("the miner didn't take %s: %s", p.text(), diff)}
			}
		}
	}
	return nil
}

// fanSettleMin: a step that holds the fans at a new target (the baseline, each preset) settles at least
// 5 minutes, time for the fans to get there.
func (e *tunerEngine) fanSettleMin() float64 {
	if e.cfg.FanHold && !e.cfg.DryRun {
		return math.Max(e.cfg.SettleMin, 5)
	}
	return e.cfg.SettleMin
}

// watch watches a plan for holdMin, accumulating per-chip error deltas and judging against
// Poisson limits. judge=false (baseline) never fails. Returns (clean, reason, stats, err);
// err is set only for an abort (temp/stop/restart). Faithful port of Tester.watch.
func (e *tunerEngine) watch(p planMV, phase string, holdMin float64, judge bool) (bool, string, tunerStats, error) {
	prev, err := e.readErrors()
	if err != nil {
		// Goldshell's backend can drop out for a moment; it also fails while the mining program restarts
		// after a crash. Wait for it rather than end the whole run on one missed reading.
		e.log("  the miner's backend didn't answer (%v): waiting up to %g min", err, e.backendWait.Minutes())
		for deadline := e.now().Add(e.backendWait); err != nil && e.now().Before(deadline); {
			if !e.sleepPoll(15 * time.Second) {
				return false, "", tunerStats{}, abortErr{"stopped"}
			}
			prev, err = e.readErrors()
		}
		if err != nil {
			return false, "", tunerStats{}, abortErr{"read chips: " + err.Error()}
		}
		e.log("  the backend answers again")
		if e.miningStops != nil && !e.lastApply.IsZero() {
			if n := e.miningStops(e.lastApply); n > 0 {
				return false, "the mining program crashed at this setting (it stopped and was restarted)", tunerStats{}, nil
			}
		}
	}
	gained := map[ckey]int{}
	for k := range prev {
		gained[k] = 0
	}
	holdH := holdMin / 60.0
	scale := 1.0
	if e.baseMHz > 0 {
		scale = math.Max(0.5, float64(p.MHz)/float64(e.baseMHz))
	}
	n := len(gained)
	limits := map[ckey]float64{}
	boards := map[int]bool{}
	for k := range gained {
		boards[k.B] = true
		limits[k] = e.chipLimit(k, holdH, scale, n)
	}
	boardLimits := map[int]float64{}
	for b := range boards {
		var base float64
		for k := range gained {
			if k.B == b {
				if _, w := e.weak[k]; !w {
					base += e.baseRate[k]
				}
			}
		}
		boardLimits[b] = tunerLimitVal(base, holdH, scale, false, e.cfg.WeakFactor, e.margin(holdH), len(boards), e.cfg.Sigma)
	}

	start := e.now()
	// the live view: this test, from zero
	e.mu.Lock()
	e.live = tunerStats{Gained: copyIntMap(gained), Limits: copyFloatMap(limits)}
	if e.liveFor.MHz == p.MHz && e.liveFor.PV == p.pvOf() && !e.liveFor.Done {
		e.liveFor.Started = start.Unix() // counting starts now (the settle isn't counted)
	}
	e.cur = &curStep{Phase: phase, MHz: p.MHz, MV: p.hw().MV, PV: p.PV, HoldMin: holdMin, Fan: e.fanCur,
		TargetC: e.holdTarget, Updated: e.now().Unix()}
	e.mu.Unlock()
	up0, haveUp := 0, false
	if up, ok := e.elapsed(); ok {
		up0, haveUp = up, true
	}
	lastRead := e.now() // the last good chip reading
	hr := &hrMeter{}
	e.vSum, e.vN = 0, 0
	var fans []int // the fan % each reading (the test's fan is the average of its second half)
	maxT := 0.0

	for {
		if !e.sleepPoll(time.Duration(e.cfg.PollS * float64(time.Second))) {
			return false, "stopped", e.snap(gained, limits, start, hr, maxT, holdMin), abortErr{"stopped"}
		}
		if err := e.checkTemp(); err != nil {
			return false, err.Error(), e.snap(gained, limits, start, hr, maxT, holdMin), err
		}
		// miner restart check (its error counters reset → can't judge clean)
		// (time since the start on the monotonic clock: a wall-clock step, e.g. NTP's first sync, isn't a restart)
		if up, ok := e.elapsed(); ok && haveUp {
			if up+90 < up0+int(e.now().Sub(start).Seconds()) {
				return false, "the miner restarted during the test", e.snap(gained, limits, start, hr, maxT, holdMin), nil
			}
		}
		cur, err := e.readErrors()
		if err != nil {
			// transient: try again next poll; but a test can't be judged without the chips, so not for ever
			if gone := e.now().Sub(lastRead); gone >= e.backendWait {
				// Goldshell's web backend (where the chip counts come from) can stop answering for a few
				// minutes while mining carries on (seen: "stock login failed (HTTP 500)" for 3 min at the end of
				// a 13-hour run, which then ended the presets phase). Wait a while longer; if the readings come
				// back, this test starts over (its counts have a hole), once. Only then does the run stop.
				if !e.gapRetried {
					e.log("  no chip reading for %.0f min (%v): waiting up to %g more min for %s backend; this test then starts over", gone.Minutes(), err, e.backendGrace.Minutes(), oemOf())
					for deadline := e.now().Add(e.backendGrace); e.now().Before(deadline); {
						if !e.sleepPoll(15 * time.Second) {
							return false, "stopped", e.snap(gained, limits, start, hr, maxT, holdMin), abortErr{"stopped"}
						}
						if _, err2 := e.readErrors(); err2 == nil {
							e.log("  chip readings are back: %s starts over", p.text())
							e.gapRetried = true
							defer func() { e.gapRetried = false }()
							return e.watch(p, phase, holdMin, judge)
						}
					}
				}
				gone = e.now().Sub(lastRead)
				why := fmt.Sprintf("read chips: no chip reading for %.0f min (%v)", gone.Minutes(), err)
				return false, why, e.snap(gained, limits, start, hr, maxT, holdMin), abortErr{why}
			}
			continue
		}
		lastRead = e.now()
		for k, v := range cur {
			pv := prev[k]
			if v >= pv {
				gained[k] += v - pv
			} else {
				gained[k] += v // counter reset
			}
			if _, ok := limits[k]; !ok {
				limits[k] = e.chipLimit(k, holdH, scale, n)
			}
		}
		prev = cur
		if t, ok := e.readTemp(); ok && t > maxT {
			maxT = t
		}
		if e.cfg.FanHold && !e.cfg.DryRun {
			fans = append(fans, e.holdFan(e.holdTarget))
		}
		var w float64
		var wok bool
		if e.readWork != nil {
			w, wok = e.readWork()
		}
		g, gok := e.readGHS()
		lastGHS := hr.add(e.now(), g, gok, w, wok)
		if e.readVolt != nil {
			if v, ok := e.readVolt(); ok && v > 0 {
				e.vSum, e.vN = e.vSum+v, e.vN+1
			}
		}
		elapsedMin := e.now().Sub(start).Minutes()

		// live per-poll progress so a long run shows activity (not silence).
		total := 0
		for _, v := range gained {
			total += v
		}
		top := topChips(gained, 3)
		e.logQuiet("  %s %.1f/%gmin  %.1f GH/s  maxT=%.1f  all chips +%d  top: %s",
			phase, elapsedMin, holdMin, lastGHS, maxT, total, top)
		curFan := e.fanCur
		if e.cfg.FanHold && len(fans) > 0 {
			curFan = halfAvg(fans)
		}
		e.recordReading(p, lastGHS, maxT, total, curFan)
		avg := hr.avg()
		e.mu.Lock()
		e.cur = &curStep{Phase: phase, MHz: p.MHz, MV: p.hw().MV, PV: p.PV,
			ElapsedMin: elapsedMin, HoldMin: holdMin, Fan: curFan, TargetC: e.holdTarget, Updated: e.now().Unix()}
		e.live = tunerStats{Gained: copyIntMap(gained), Limits: copyFloatMap(limits), Total: total,
			WatchedMin: elapsedMin, AvgGHS: avg, MaxT: maxT}
		e.mu.Unlock()

		if judge {
			// worst chip by margin over limit
			worst, have := e.worstBy(gained, limits)
			if have && float64(gained[worst]) > limits[worst] {
				st := e.snap(gained, limits, start, hr, maxT, holdMin)
				st.FailChip, st.FailByChip = worst, true
				return false, fmt.Sprintf("chip %s +%d errors (limit %.1f)", chipName(worst), gained[worst], limits[worst]), st, nil
			}
			for b, lim := range boardLimits {
				var g int
				for k, v := range gained {
					if k.B == b {
						if _, w := e.weak[k]; !w {
							g += v
						}
					}
				}
				if float64(g) > lim {
					st := e.snap(gained, limits, start, hr, maxT, holdMin)
					return false, fmt.Sprintf("board %d +%d errors (limit %.1f)", b, g, lim), st, nil
				}
			}
		}
		if elapsedMin >= holdMin {
			st := e.snap(gained, limits, start, hr, maxT, holdMin)
			if len(fans) > 0 {
				st.AvgFan = halfAvg(fans)
			}
			if judge && e.baseGHS > 0 && e.baseMHz > 0 && e.cfg.HashTol > 0 {
				want := e.baseGHS * pll(float64(p.MHz)) / pll(float64(e.baseMHz))
				if tol := math.Max(e.cfg.HashTol, 0.06); st.AvgGHS < want*(1-tol) { // readings vary 2-3 % between tests
					return false, fmt.Sprintf("hashrate %.1f GH/s is %.0f%% below the %.1f this clock should give",
						st.AvgGHS, (1-st.AvgGHS/want)*100, want), st, nil
				}
			}
			// something changed the setting during the test: what was watched isn't this setting
			if judge && !e.cfg.DryRun && e.verifyPlan != nil {
				if diff := e.verifyPlan(p); diff != "" {
					return false, "the setting changed during the test (" + diff + ")", st, nil
				}
			}
			if e.cfg.FanHold && st.AvgFan > 0 {
				e.mu.Lock()
				e.fanAt[p.MHz] = st.AvgFan
				e.mu.Unlock()
			}
			return true, "clean", st, nil
		}
	}
}

func (e *tunerEngine) snap(gained map[ckey]int, limits map[ckey]float64, start time.Time, hr *hrMeter, maxT, holdMin float64) tunerStats {
	total := 0
	for _, v := range gained {
		total += v
	}
	worst, have := e.worstByRaw(gained)
	avg := hr.avg()
	st := tunerStats{Gained: copyIntMap(gained), Limits: copyFloatMap(limits), Worst: worst, HaveWorst: have,
		Total: total, WatchedMin: e.now().Sub(start).Minutes(), AvgGHS: avg, MaxT: maxT}
	if e.vN > 0 {
		st.VoltMV = e.vSum / float64(e.vN)
	}
	e.mu.Lock()
	e.lastStat = st
	e.mu.Unlock()
	return st
}

func (e *tunerEngine) worstBy(gained map[ckey]int, limits map[ckey]float64) (ckey, bool) {
	var best ckey
	have := false
	bestv := math.Inf(-1)
	for k, v := range gained {
		d := float64(v) - limits[k]
		if !have || d > bestv {
			best, bestv, have = k, d, true
		}
	}
	return best, have
}

// topChips formats the n chips with the most gained errors, e.g. "b2.c5 +3, b2.c11 +1".
func topChips(gained map[ckey]int, n int) string {
	type kv struct {
		k ckey
		v int
	}
	var xs []kv
	for k, v := range gained {
		if v > 0 {
			xs = append(xs, kv{k, v})
		}
	}
	sort.Slice(xs, func(i, j int) bool { return xs[i].v > xs[j].v })
	if len(xs) == 0 {
		return "none"
	}
	s := ""
	for i := 0; i < len(xs) && i < n; i++ {
		if i > 0 {
			s += ", "
		}
		s += fmt.Sprintf("%s +%d", chipName(xs[i].k), xs[i].v)
	}
	return s
}

func (e *tunerEngine) worstByRaw(gained map[ckey]int) (ckey, bool) {
	var best ckey
	have := false
	bestv := -1
	for k, v := range gained {
		if !have || v > bestv {
			best, bestv, have = k, v, true
		}
	}
	return best, have
}

func (e *tunerEngine) chipLimit(k ckey, hours, scale float64, n int) float64 {
	_, weak := e.weak[k]
	return tunerLimitVal(e.baseRate[k], hours, scale, weak, e.cfg.WeakFactor, e.margin(hours), n, e.cfg.Sigma)
}

// margin: the fewest errors a chip or board may make, per test length (Margin per normal test): a fixed 2
// would make a 50-minute retest or confirm as strict as one 25-minute test, so good settings failed by luck.
func (e *tunerEngine) margin(hours float64) float64 {
	h := e.cfg.HoldMin / 60
	if h <= 0 {
		return e.cfg.Margin
	}
	return e.cfg.Margin * math.Max(1, hours/h)
}

// baseline measures each chip's normal error rate and flags weak chips.
func (e *tunerEngine) baseline(p planMV) error {
	e.setStatus("baseline", fmt.Sprintf("baseline %s for %gmin", p.text(), e.cfg.BaselineMin))
	e.log("BASELINE %s for %gmin", p.text(), e.cfg.BaselineMin)
	if err := e.settle(p, e.fanSettleMin()); err != nil {
		return err
	}
	full, why, st, err := e.watch(p, "baseline", e.cfg.BaselineMin, false)
	if err == nil && !full { // the miner restarted: a short baseline's rates would open every limit
		e.log("  the baseline was cut short (%s): measuring it again", why)
		full, why, st, err = e.watch(p, "baseline", e.cfg.BaselineMin, false)
		if err == nil && !full {
			err = abortErr{"the baseline was cut short twice: " + why}
		}
	}
	if err != nil {
		return err
	}
	if gerr := tunerGeometryErr(st.Gained); gerr != nil {
		return abortErr{gerr.Error()}
	}
	hours := math.Max(st.WatchedMin, 1e-6) / 60.0
	// Each chip's normal rate: its own count plus a prior taken from its board. One extra error per chip
	// (the old way) meant a board that made 1 error in the baseline was allowed ~5 per chip and ~50 per
	// board, so marginal settings passed the map by luck and the confirm then spent hours rejecting them.
	// Now the board gets Pseudo extra errors, shared out over its chips: a quiet board stays strict, a
	// board that was noisy at baseline still allows its chips that noise.
	boardErr, boardN := map[int]float64{}, map[int]int{}
	for k, g := range st.Gained {
		boardErr[k.B] += float64(g)
		boardN[k.B]++
	}
	baseRate := map[ckey]float64{}
	for k, g := range st.Gained {
		prior := (boardErr[k.B] + e.cfg.Pseudo) / float64(max(1, boardN[k.B]))
		baseRate[k] = (float64(g) + prior) / hours
	}
	weak := map[ckey]float64{}
	if e.cfg.EasyNoisy { // off: every chip is judged on its own normal rate, noisy or not
		weak = computeWeak(baseRate, e.cfg)
	}
	// publish the baseline under the lock (statusView reads baseMHz/weak concurrently)
	e.mu.Lock()
	e.baseRate = baseRate
	e.baseMHz = p.MHz
	e.baseGHS = st.AvgGHS
	e.weak = weak
	e.mu.Unlock()
	e.log("baseline done: %d errors across all chips, %d weak chip(s)", st.Total, len(weak))
	return nil
}

// computeWeak flags chips already far noisier than the median (judged on getting clearly worse).
func computeWeak(baseRate map[ckey]float64, cfg tunerConfig) map[ckey]float64 {
	weak := map[ckey]float64{}
	if cfg.WeakX <= 0 || len(baseRate) == 0 {
		return weak
	}
	rates := make([]float64, 0, len(baseRate))
	for _, r := range baseRate {
		rates = append(rates, r)
	}
	sort.Float64s(rates)
	med := math.Max(rates[len(rates)/2], 1e-6)
	floor := math.Max(cfg.WeakX*med, cfg.WeakMin)
	for k, r := range baseRate {
		if r >= floor {
			weak[k] = r / med
		}
	}
	return weak
}

// test = settle + watch(judge=true).
func (e *tunerEngine) test(p planMV) (bool, string, tunerStats, error) {
	return e.testFor(p, e.cfg.HoldMin, "test", 0)
}

// testFor: settle (settleMin, 0 = the usual), then watch for hold minutes; the run plan shows it as an
// attempt of the current item (e.att), and the stats are kept for reuse.
func (e *tunerEngine) testFor(p planMV, hold float64, phase string, settleMin float64) (bool, string, tunerStats, error) {
	e.setStatus("test", fmt.Sprintf("testing %s", p.text()))
	e.log("TEST %s (settle %gmin, hold %gmin)", p.text(), math.Max(settleMin, e.cfg.SettleMin), hold)
	e.attBegin(p)
	if err := e.settle(p, settleMin); err != nil {
		e.attEnd("cut", err.Error())
		return false, err.Error(), tunerStats{}, err
	}
	clean, reason, st, werr := e.watch(p, phase, hold, true)
	e.lastWatchEnd = time.Time{}
	if werr == nil && (clean || strings.Contains(reason, "errors (limit")) {
		e.lastWatchEnd = e.now() // ran its whole course on this setting: a retest needs no full settle
	}
	e.recordResult(p, clean, reason, st)
	e.mu.Lock()
	if e.testStats == nil {
		e.testStats = map[[2]int]tunerStats{}
	}
	st.Reason = reason
	e.testStats[[2]int{p.MHz, p.MV}] = st
	needSnap := false
	if clean && werr == nil && phase == "test" { // (the map, its trim retests and the trim check)
		if e.trimsAtPass == nil {
			e.trimsAtPass = map[[2]int]map[[2]int]float64{}
		}
		if _, seen := e.trimsAtPass[[2]int{p.MHz, p.MV}]; !seen {
			needSnap = true
		}
	}
	e.mu.Unlock()
	if needSnap { // the trims on the chips during this clean test (a retest's new trim isn't in runTrims yet)
		snap := map[[2]int]float64{}
		if e.trimGet != nil {
			for k, v := range e.trimGet() {
				snap[k] = v
			}
		}
		e.mu.Lock()
		e.trimsAtPass[[2]int{p.MHz, p.MV}] = snap
		e.mu.Unlock()
	}
	switch {
	case werr != nil:
		e.attEnd("cut", reason)
	case clean:
		e.attEnd("pass", "")
	default:
		e.attEnd("fail", reason)
	}
	return clean, reason, st, werr
}

// attItem: the run-plan item e.att points at (callers hold e.mu).
func (e *tunerEngine) attItem() *tunerStep {
	if e.att.key == "curve" {
		return e.clockByMHz(e.att.mhz)
	}
	if ph := e.stepByKey(e.att.key); ph != nil {
		for i := range ph.Kids {
			if ph.Kids[i].Label == e.att.item {
				return &ph.Kids[i]
			}
		}
	}
	return nil
}

func (e *tunerEngine) attBegin(p planMV) {
	e.mu.Lock()
	defer e.mu.Unlock()
	it := e.attItem()
	if it == nil {
		return
	}
	label := fmt.Sprintf("%s", e.cfg.vText(p.MV))
	if e.att.key != "curve" || p.MHz != e.att.mhz {
		label = fmt.Sprintf("%d MHz · %s", p.MHz, e.cfg.vText(p.MV))
	}
	switch {
	case label == e.att.item && e.att.tag != "": // a confirm candidate's row already names the setting
		label = e.att.tag
	case label == e.att.item:
		label = "test"
	case e.att.tag != "":
		label += " (" + e.att.tag + ")"
	}
	it.Kids = append(it.Kids, tunerStep{Label: label, State: "run"})
	if it.State == "todo" {
		it.State = "run"
	}
}

func (e *tunerEngine) attEnd(state, note string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if it := e.attItem(); it != nil && len(it.Kids) > 0 {
		k := &it.Kids[len(it.Kids)-1]
		k.State = state
		if note != "" && state != "pass" {
			k.Note = note
		}
	}
}

// attNote adds a note to the item's last attempt (e.g. "over its limit in under half the test: not retested").
func (e *tunerEngine) attNote(note string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if it := e.attItem(); it != nil && len(it.Kids) > 0 {
		k := &it.Kids[len(it.Kids)-1]
		if k.Note != "" {
			k.Note += " · "
		}
		k.Note += note
	}
}

// Run executes the whole tuning run (blocking; call in a goroutine). It always restores the
// baseline plan on finish/abort (unless dry-run).
// tunerStep is one row of the run plan, shown with a status in the UI.
// tunerStep is one node of the hierarchical run plan (blake2b-asic-control parity): top-level
// phases (Baseline / Voltage curve / Confirm / Presets / Finish) each carry a description and a
// list of child steps — the per-clock steps under Voltage curve (each with its own voltage ladder
// in Kids), and the per-preset steps under Presets. State: todo, run, pass, fail, skip, cut.
type tunerStep struct {
	Key   string      `json:"-"`
	MHz   int         `json:"-"`
	Label string      `json:"label"`
	State string      `json:"state"`
	Desc  string      `json:"desc,omitempty"`
	Note  string      `json:"note,omitempty"`
	Kids  []tunerStep `json:"kids,omitempty"`
}

// copySteps: a deep copy (the run keeps changing the plan while the copy is sent).
func copySteps(s []tunerStep) []tunerStep {
	if s == nil {
		return nil
	}
	out := make([]tunerStep, len(s))
	for i, x := range s {
		x.Kids = copySteps(x.Kids)
		out[i] = x
	}
	return out
}

func (e *tunerEngine) setSteps(s []tunerStep) { e.mu.Lock(); e.steps = s; e.mu.Unlock() }

// stepByKey / clockByMHz navigate the plan tree; callers must hold e.mu.
func (e *tunerEngine) stepByKey(key string) *tunerStep {
	for i := range e.steps {
		if e.steps[i].Key == key {
			return &e.steps[i]
		}
	}
	return nil
}
func (e *tunerEngine) clockByMHz(mhz int) *tunerStep {
	cv := e.stepByKey("curve")
	if cv == nil {
		return nil
	}
	for i := range cv.Kids {
		if cv.Kids[i].MHz == mhz {
			return &cv.Kids[i]
		}
	}
	return nil
}

func (e *tunerEngine) setPhase(key, state, note string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if s := e.stepByKey(key); s != nil {
		s.State = state
		if note != "" {
			s.Note = note
		}
	}
}
func (e *tunerEngine) setClock(mhz int, state, note string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if c := e.clockByMHz(mhz); c != nil {
		c.State = state
		if note != "" {
			c.Note = note
		}
	}
}
func (e *tunerEngine) addVolt(mhz int, label, state string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if c := e.clockByMHz(mhz); c != nil {
		c.Kids = append(c.Kids, tunerStep{Label: label, State: state})
	}
}
func (e *tunerEngine) setLastVolt(mhz int, state, note string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if c := e.clockByMHz(mhz); c != nil && len(c.Kids) > 0 {
		c.Kids[len(c.Kids)-1].State = state
		if note != "" {
			c.Kids[len(c.Kids)-1].Note = note
		}
	}
}
func (e *tunerEngine) setPreset(name, state, note string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if p := e.stepByKey("presets"); p != nil {
		for i := range p.Kids {
			if p.Kids[i].Label == name {
				p.Kids[i].State = state
				if note != "" {
					p.Kids[i].Note = note
				}
			}
		}
	}
}

// skipTodo marks any still-"todo" phase (and its still-"todo" kids) as skipped — used when a run
// ends early so the plan shows what never ran.
func (e *tunerEngine) skipTodo() {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i := range e.steps {
		mark := func(s *tunerStep) {
			if s.State == "todo" {
				s.State = "skip"
			}
			for j := range s.Kids {
				if s.Kids[j].State == "todo" {
					s.Kids[j].State = "skip"
				}
			}
		}
		mark(&e.steps[i])
	}
}

// cutRunning marks any phase/clock still "run" as "cut" (stopped mid-way) — used on a user Stop.
func (e *tunerEngine) cutRunning() {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i := range e.steps {
		if e.steps[i].State == "run" {
			e.steps[i].State = "cut"
		}
		for j := range e.steps[i].Kids {
			if e.steps[i].Kids[j].State == "run" {
				e.steps[i].Kids[j].State = "cut"
			}
		}
	}
}

// lastCurve holds the most recent successful map-mode curve, so "rebuild" can make presets
// without mapping again.
var (
	lastCurveMu sync.Mutex
	lastCurve   []planMV
)

// The curve is kept on disk too (tuner-curve.json), so a rebuild still works after a restart; with it the
// setting the confirm accepted (a rebuild's High), and the ones it rejected (never a rebuild's High).
type savedCurve struct {
	Curve     []planMV `json:"curve"`
	Confirmed *planMV  `json:"confirmed,omitempty"`
	Rejected  []planMV `json:"rejected,omitempty"`
}

var tunerCurvePath = func() string { return filepath.Join(filepath.Dir(*authFile), "tuner-curve.json") }

func lastCurveSet(c []planMV) {
	lastCurveMu.Lock()
	lastCurve = append([]planMV(nil), c...)
	lastCurveMu.Unlock()
	sc := loadSavedCurve()
	sc.Curve, sc.Confirmed, sc.Rejected = c, nil, nil // a new map: its own confirm result follows
	saveSavedCurve(sc)
}

func loadSavedCurve() savedCurve {
	var sc savedCurve
	if b, err := os.ReadFile(tunerCurvePath()); err == nil {
		json.Unmarshal(b, &sc)
	}
	// a file from an unlocked run (re-locked since) or edited by hand: only points inside today's window
	fLo, fHi := freqMin, freqMax
	if loadOverclock().Acknowledged {
		fLo, fHi = freqHardMin, effHard("freq", freqHardMax)
	}
	ok := func(p planMV) bool {
		if tunerOnBox2() { // the clock window and Goldshell's level interval (the engine's ranks stay inside it): no PV
			return p.MHz >= freqMin && p.MHz <= clockWindowHi() && p.MV >= model.VCodeMin && p.MV <= model.VCodeMax
		}
		pv := p.pvOf()
		return p.MHz >= fLo && p.MHz <= fHi && pv >= pvMin && pv <= pvHardMax && p.MV >= vrunHardMin && p.MV <= min(vrunHardMax, pv)
	}
	var c []planMV
	for _, p := range sc.Curve {
		if ok(p) {
			c = append(c, p)
		}
	}
	sc.Curve = c
	if sc.Confirmed != nil && !ok(*sc.Confirmed) {
		sc.Confirmed = nil
	}
	return sc
}

func saveSavedCurve(sc savedCurve) {
	if b, err := json.Marshal(sc); err == nil {
		tmp := tunerCurvePath() + ".tmp"
		if os.WriteFile(tmp, b, 0o644) == nil {
			os.Rename(tmp, tunerCurvePath())
		}
	}
}

func lastCurveGet() []planMV {
	lastCurveMu.Lock()
	c := append([]planMV(nil), lastCurve...)
	lastCurveMu.Unlock()
	if len(c) == 0 {
		c = loadSavedCurve().Curve
	}
	return c
}

// buildSteps lays out the run plan up front from the config.
func (e *tunerEngine) buildSteps(base planMV, beforeWhat string) {
	fans := "fans left alone"
	if e.cfg.FanHold {
		fans = fmt.Sprintf("fans holding %.0f°C", e.cfg.FanTargetC)
	}
	steps := []tunerStep{{
		Key: "baseline", Label: "Baseline — " + base.text(), State: "todo",
		Desc: fmt.Sprintf("%s for %g min, %s: learns every chip's normal error rate. Nothing can fail here.",
			base.text(), e.cfg.BaselineMin, fans),
	}}
	confirmMin := e.cfg.HoldMin * e.cfg.ConfirmX
	switch e.cfg.Mode {
	case "confirm":
		rounds := e.confirmRounds()
		var kids []tunerStep
		for r := 1; r <= rounds; r++ {
			kids = append(kids, tunerStep{Label: fmt.Sprintf("Round %d", r), State: "todo"})
		}
		steps = append(steps, tunerStep{
			Key: "confirm", Label: fmt.Sprintf("Confirm — %d MHz · %s", e.cfg.ConfirmMHz, e.cfg.vText(e.cfg.ConfirmMV)), State: "todo", Kids: kids,
			Desc: fmt.Sprintf("runs the setting for %g h in %d rounds of %g min; a failed round gets one retest. If one still fails, the miner goes back to what it ran before.", e.cfg.ConfirmHours, rounds, e.cfg.HoldMin),
		})
	case "rebuild":
		// presets only: the Presets phase below carries the work
	default: // map
		var clocks []tunerStep
		for _, mhz := range e.cfg.clockList() {
			clocks = append(clocks, tunerStep{Key: "clk", MHz: mhz, Label: clockLabel(mhz), State: "todo"})
		}
		down := fmt.Sprintf(", then down until it fails (min PV %d), never testing a voltage that already failed at a slower clock", e.cfg.pvFor(e.cfg.MinMV))
		if !e.cfg.FindLowest {
			down = " (the lowest voltage isn't searched: Find the lowest voltage is off)"
		}
		desc := fmt.Sprintf("every clock from %d up to %d MHz in %g MHz steps, %g min per test. The first clock starts at PV %d, each faster one at the voltage that was lowest one clock slower (a faster clock never needs less). Up %d mV of PV at a time until clean (max PV %d): the first clean test is that clock's lowest. A fail on the way up isn't retested (the next step up is quicker); one at PV %d is, unless it went over its whole limit in half the test or less. A narrow pass gets one test a step higher too. The curve ends at the first clock that isn't clean even at PV %d.",
			e.cfg.LowestMHz, e.cfg.HighestMHz, e.cfg.Step, e.cfg.HoldMin, e.cfg.pvFor(e.cfg.MinMV), e.cfg.MVStep, e.cfg.pvFor(e.cfg.MaxMV), e.cfg.pvFor(e.cfg.MaxMV), e.cfg.pvFor(e.cfg.MaxMV))
		if e.cfg.TopDown {
			desc = fmt.Sprintf("every clock from %d up to %d MHz in %g MHz steps, %g min per test. The first clock starts at the baseline voltage (that setting reuses the baseline), each faster one at the voltage that was lowest one clock slower (plus what that step needed, at most %d mV). Up %d mV of PV at a time until clean (max PV %d)%s. A failed test gets one retest unless it went over its whole limit in half the test or less. The curve ends at the first clock that isn't clean even at PV %d.",
				e.cfg.LowestMHz, e.cfg.HighestMHz, e.cfg.Step, e.cfg.HoldMin, e.cfg.MVStep, e.cfg.MVStep, e.cfg.pvFor(e.cfg.MaxMV), down, e.cfg.pvFor(e.cfg.MaxMV))
		}
		label := "Voltage curve"
		if tunerOnBox2() {
			desc, label = box2CurveDesc(e.cfg), "Clock curve"
		}
		steps = append(steps, tunerStep{Key: "curve", Label: label, State: "todo", Kids: clocks, Desc: desc})
		if e.cfg.ChipTrim && !e.cfg.DryRun {
			steps = append(steps, tunerStep{
				Key: "trimcheck", Label: "Trim check", State: "todo",
				Desc: "at the best setting, lifts each chip trim this run made by one step (12.5 MHz) and tests again: a trim that isn't needed there (or was made after a test failed by bad luck) is lifted, one that is goes back. One test per step.",
			})
		}
		if e.cfg.ConfirmWin {
			steps = append(steps, tunerStep{
				Key: "confirm", Label: "Confirm", State: "todo",
				Desc: fmt.Sprintf("runs the fastest clean setting again for %g min (a failed confirm gets one retest unless it failed clearly); if it doesn't hold, the next fastest, and so on.", confirmMin),
			})
		}
	}
	if e.cfg.Mode == "rebuild" || (e.cfg.Mode == "map" && e.cfg.BuildPresets) {
		var pk []tunerStep
		for _, nm := range presetOrder {
			pk = append(pk, tunerStep{Key: "preset", Label: nm, State: "todo"})
		}
		steps = append(steps, tunerStep{
			Key: "presets", Label: "Presets", State: "todo", Kids: pk,
			Desc: fmt.Sprintf("builds and tests High, Middle, Low and Lowest power, %g min each, each held at its own temperature and saved with its own fan curve. A preset that fails gets more voltage, then one clock step less.", e.cfg.PresetMin),
		})
	}
	steps = append(steps, tunerStep{
		Key: "finish", Label: "Finish", State: "todo",
		Desc: "leaves the miner on the confirmed best setting; if nothing is confirmed, or the run stops early, it goes back to what it ran before (" + beforeWhat + ").",
	})
	e.setSteps(steps)
}

var presetOrder = []string{"High", "Middle", "Low", "Lowest power"}

func (e *tunerEngine) confirmRounds() int {
	return max(1, int(math.Round(e.cfg.ConfirmHours*60/math.Max(1, e.cfg.HoldMin))))
}

// resolveBaseline returns the baseline plan from the configured source.
func (e *tunerEngine) resolveBaseline() planMV {
	if tunerOnBox2() {
		return box2TunerBaseline()
	}
	if e.cfg.BaselineSrc == "stock" || e.cfg.BaselineSrc == "current" {
		if pp, ok := readStockBaseline(e.cfg.BaselineSrc); ok && pp.FreqMHz > 0 {
			// the miner's own PV (only PV moves the boards' voltage), not run voltage + gap: a Controls setting
			// with another gap (or a gap set here other than the stock 300) would make the baseline a setting
			// the miner never ran. One outside the supply's range falls back to the gap rule (clamped).
			pv := pp.PVMv
			if pv < pvHardMin || pv > pvHardMax || pv < pp.VrunMv {
				pv = e.cfg.pvFor(pp.VrunMv)
			}
			return planMV{MHz: pp.FreqMHz, MV: pp.VrunMv, PV: pv}
		}
	}
	return planMV{MHz: e.cfg.BaselineMHz, MV: e.cfg.BaselineMV, PV: e.cfg.pvFor(e.cfg.BaselineMV)}
}

// begin marks the run started (running, a stop channel) before its goroutine runs, so a second start or
// a Stop that comes right after the start request sees it. false: already running.
func (e *tunerEngine) begin() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.running {
		return false
	}
	e.begun = true
	e.running = true
	e.stopCh = make(chan struct{})
	e.done = make(chan struct{})
	e.alive()
	return true
}

func (e *tunerEngine) Run() {
	charge0 := "" // who was in charge as the run started (the tuner, from its start)
	if e.chargeNow != nil {
		charge0 = e.chargeNow()
	}
	e.mu.Lock()
	if e.running && !e.begun {
		e.mu.Unlock()
		return
	}
	stopCh := e.stopCh
	if !e.begun {
		stopCh = make(chan struct{})
		e.done = make(chan struct{})
	}
	done := e.done
	e.begun = false
	e.running = true
	e.startedAt = e.now()
	e.finishedAt = time.Time{}
	e.stopCh = stopCh
	e.curve = nil
	e.best = nil
	e.runTrims, e.trimsUndone = nil, 0 // (under e.mu)
	e.abortWhy = ""
	if !e.begun || e.stopCh == nil {
		e.stopWhy = ""
	}
	e.fanAt = map[int]int{}
	e.fanCur = e.cfg.FanMinPct
	e.holdTarget = e.cfg.FanTargetC
	e.passes, e.testStats, e.att, e.noTemp, e.fh, e.floorPV = nil, nil, attCtx{}, 0, fanHold{}, 0
	e.marginal = map[[2]int]string{}
	e.lifted, e.liftedTo, e.liftedAt, e.trimFutile = nil, nil, planMV{}, map[futileKey]bool{}
	e.mu.Unlock()

	if e.cfg.FanHold && !e.cfg.DryRun {
		e.log("holding ~%.0f°C with the fans during the run (recording fan %% per setting)", e.cfg.FanTargetC)
	}

	e.confirmed = false
	basePlan := e.resolveBaseline()
	// what the miner runs now: where it goes back to unless a setting is confirmed
	var restoreBefore func() error
	beforeWhat := basePlan.text()
	if e.before != nil && !e.cfg.DryRun {
		if r, w := e.before(); r != nil {
			restoreBefore, beforeWhat = r, w
		}
	}
	e.mu.Lock()
	e.baseUsed = basePlan
	e.backWhat, e.endHeld = "", ""
	if restoreBefore != nil {
		e.backWhat = beforeWhat
	}
	e.mu.Unlock()
	e.baseAtom.Store(&basePlan)
	e.buildSteps(basePlan, beforeWhat)
	if e.onStart != nil {
		e.onStart()
	}
	defer close(done)
	defer func() {
		// putting the miner back can take minutes of retries: say so, not the last test with a frozen clock
		e.setStatus("restore", "putting the miner back")
		e.mu.Lock()
		e.cur = nil
		e.mu.Unlock()
		// the end: on the confirmed setting, else back to what the miner ran before the run (unless dry-run)
		if !e.cfg.DryRun && !e.dead.Load() { // (ended by the watchdog: it put the miner back already)
			e.mu.Lock()
			best, confirmed := e.best, e.confirmed
			e.mu.Unlock()
			var hold error
			if e.held != nil {
				hold = e.held()
			}
			if hold == nil && e.chargeNow != nil && charge0 != "idle" && e.chargeNow() == "idle" {
				hold = errors.New("a safety stop put it on Idle during the run") // (no temperature while hashing)
			}
			if !(confirmed && best != nil) {
				e.undoRunTrims() // (before the clock goes back: the miner applies the trims file with it)
			}
			switch {
			case hold != nil:
				e.mu.Lock()
				e.endHeld = hold.Error()
				e.mu.Unlock()
				e.log("the miner stays on Idle (%v): nothing is put back over it", hold)
			case confirmed && best != nil:
				if err := e.retry(e.restoreWait, func() error { return e.applyPlan(*best) }); err != nil {
					e.log("WARNING: could not put the confirmed %s back: %v", best.text(), err)
					e.restoreFailed = true
				} else {
					e.log("leaving the miner on the confirmed %s", best.text())
				}
			case restoreBefore != nil:
				if err := e.retry(e.restoreWait, restoreBefore); err != nil {
					e.log("WARNING: could not put back what the miner ran before (%s): %v; trying the baseline", beforeWhat, err)
					e.mu.Lock()
					e.backWhat = "" // (the baseline, if that goes back)
					e.mu.Unlock()
					if err := e.retry(e.restoreWait, func() error { return e.applyPlan(basePlan) }); err != nil {
						e.log("WARNING: could not restore baseline %s: %v", basePlan.text(), err)
						e.restoreFailed = true
					}
				} else {
					e.log("going back to what the miner ran before the run: %s", beforeWhat)
				}
			default:
				if err := e.retry(e.restoreWait, func() error { return e.applyPlan(basePlan) }); err != nil {
					e.log("WARNING: could not restore baseline %s: %v", basePlan.text(), err)
					e.restoreFailed = true
				} else {
					e.log("restored baseline %s", basePlan.text())
				}
			}
		}
		if len(e.runTrims) > 0 && e.confirmed && e.best != nil && !e.dead.Load() { // (the watchdog took them off)
			var parts []string
			for k, v := range e.runTrims {
				parts = append(parts, fmt.Sprintf("b%d.c%d %+.4g MHz", k[0], k[1], v))
			}
			sort.Strings(parts)
			e.log("CHIP TRIMS kept by this run (in Controls -> Chip trims; each made a failing test pass): %s", strings.Join(parts, ", "))
			if e.adoptTrims != nil && !e.cfg.DryRun {
				e.adoptTrims(trimList(e.runTrims), fmt.Sprintf("kept by the tuning run of %s (made a failing test pass)", e.startedAt.Format("2006-01-02 15:04")))
			}
		}
		e.setStatus("done", "run finished")
		e.mu.Lock()
		e.running = false
		e.cur = nil
		e.finishedAt = e.now()
		e.mu.Unlock()
	}()

	e.setPhase("baseline", "run", "")
	if err := e.baseline(basePlan); err != nil {
		if e.stopped() {
			e.setPhase("baseline", "cut", "")
		} else {
			e.setPhase("baseline", "fail", "")
		}
		e.skipTodo()
		e.log("STOP: %v", err)
		e.finishPlan(beforeWhat)
		return
	}
	e.setPhase("baseline", "pass", "learned every chip's normal rate")

	switch e.cfg.Mode {
	case "confirm":
		e.runConfirm()
	case "rebuild":
		c := lastCurveGet()
		if len(c) == 0 {
			e.log("REBUILD: no saved curve from a previous map run — run 'Map every clock and voltage' first")
			e.finishPlan(beforeWhat)
			return
		}
		sc := loadSavedCurve()
		rejected := map[planMV]bool{}
		for _, r := range sc.Rejected {
			rejected[r] = true
		}
		top := c[len(c)-1]
		for i := len(c) - 1; i >= 0 && rejected[c[i]]; i-- {
			if i > 0 {
				top = c[i-1]
			}
		}
		if sc.Confirmed != nil {
			top = *sc.Confirmed
		}
		e.mu.Lock()
		e.curve = c
		e.passes = append([]planMV(nil), c...)
		e.best = &top
		e.mu.Unlock()
		e.log("REBUILD: using the %d-clock curve from the last map run", len(c))
		if !e.stopped() {
			e.buildAndTestPresets(basePlan, true)
		}
	default: // map
		e.runClockCurve(basePlan)
		if e.cfg.ChipTrim && !e.cfg.DryRun && e.cfg.ConfirmWin && !e.stopped() && !e.aborted() {
			e.trimCheck()
		}
		if e.cfg.ConfirmWin && !e.stopped() && !e.aborted() {
			e.confirmWinner(basePlan)
		}
		// confirm off: the run still ends on its best clean setting (as blake2b-asic-control does)
		if !e.cfg.ConfirmWin && !e.cfg.DryRun && !e.stopped() && !e.aborted() {
			// not a pass that was close (a chip near its limit, or clean only on its retest): those mostly fail
			// a longer run; the fastest one with room to spare
			e.mu.Lock()
			var pick *planMV
			for i := range e.passes {
				p := e.passes[i]
				if e.marginal[[2]int{p.MHz, p.MV}] != "" {
					continue
				}
				if pick == nil || p.MHz > pick.MHz || (p.MHz == pick.MHz && p.MV < pick.MV) {
					pp := p
					pick = &pp
				}
			}
			msg := ""
			if pick != nil && e.best != nil && *pick != *e.best {
				msg = fmt.Sprintf("confirm off: ending on %s, not %s (that one passed only narrowly)", pick.text(), e.best.text())
			}
			e.best = pick
			e.confirmed = pick != nil
			e.mu.Unlock()
			if msg != "" {
				e.log("%s", msg)
			}
			if pick != nil {
				sc := loadSavedCurve()
				sc.Confirmed = pick // a later rebuild's High: this, not the narrow top
				saveSavedCurve(sc)
			} else if len(e.passes) > 0 {
				e.log("confirm off: every clean setting passed only narrowly, so none is kept: back to what it ran before (turn Confirm on to test them for longer)")
			}
			// the trims are checked at the setting the run ends on: a narrow lower-voltage top may have needed
			// trims this one doesn't
			if pick != nil && e.cfg.ChipTrim && !e.stopped() && !e.aborted() {
				e.trimCheck()
			}
		}
		if e.cfg.BuildPresets && !e.stopped() && !e.aborted() {
			e.buildAndTestPresets(basePlan, false)
		}
	}
	e.finishPlan(beforeWhat)
}

// aborted: the run stopped itself (too hot, a setting the miner didn't take, no temperature…): nothing
// after that is tested; the miner goes back to what it ran before.
func (e *tunerEngine) aborted() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.abortWhy != ""
}

// finishPlan: every phase and step ends in a final state.
func (e *tunerEngine) finishPlan(beforeWhat string) {
	e.mu.Lock()
	confirmed, best := e.confirmed, e.best
	e.mu.Unlock()
	switch {
	case e.stopped():
		e.cutRunning()
		e.skipTodo()
		e.mu.Lock()
		by := e.stopWhy
		e.mu.Unlock()
		if by != "" {
			e.setPhase("finish", "cut", "stopped by "+by)
		} else if confirmed && best != nil && !e.cfg.DryRun {
			e.setPhase("finish", "cut", "stopped by you during the presets: left on the confirmed "+best.text())
		} else {
			e.setPhase("finish", "cut", "stopped by you: back to what it ran before ("+beforeWhat+")")
		}
	case e.aborted():
		e.cutRunning()
		e.skipTodo()
		if confirmed && best != nil {
			e.setPhase("finish", "cut", "safety stop during the presets: left on the confirmed "+best.text())
		} else {
			e.setPhase("finish", "cut", "safety stop: back to what it ran before ("+beforeWhat+")")
		}
	default:
		e.skipTodo() // phases never reached show skipped
		note := "nothing confirmed: back to what it ran before (" + beforeWhat + ")"
		if confirmed && best != nil {
			note = "left on the confirmed " + best.text()
			if e.cfg.Mode == "map" && !e.cfg.ConfirmWin {
				note = "left on the best clean setting, " + best.text() + " (confirm was off)"
			}
		}
		if e.cfg.DryRun {
			note = "dry run: nothing was changed"
		}
		e.setPhase("finish", "pass", note)
	}
}

// runConfirm runs a single setting for the full hold time and confirms (stays on it) or
// rejects (restores baseline).
func (e *tunerEngine) runConfirm() {
	e.setStatus("confirm", "confirming one setting")
	cand := planMV{MHz: e.cfg.ConfirmMHz, MV: e.cfg.ConfirmMV, PV: e.cfg.pvFor(e.cfg.ConfirmMV)}
	e.setPhase("confirm", "run", "")
	// the time is split into rounds as long as one test; each round is judged like a search test, and a
	// round that fails gets one retest; if the retest fails too, the setting is rejected
	rounds := e.confirmRounds()
	ok, why := true, ""
	for r := 1; r <= rounds && ok; r++ {
		item := fmt.Sprintf("Round %d", r)
		e.att = attCtx{key: "confirm", item: item}
		e.log("CONFIRM round %d of %d: %s", r, rounds, cand.text())
		good, w, st1, err := e.test(cand)
		if err == nil && !good && !e.stopped() {
			e.log("round %d failed (%s): one retest", r, w)
			e.att.tag = "retest"
			var st2 tunerStats
			good, w, st2, err = e.test(cand)
			if good && err == nil && st1.Gained != nil {
				if ok2, w2 := e.combinedOK(cand, st1, st2); !ok2 {
					good, w = false, w2
					e.log("round %d: clean on the retest, but not over both together (%s)", r, w2)
				}
			}
		}
		e.att = attCtx{}
		if err != nil {
			e.setItem("confirm", item, "cut", "")
			if e.stopped() {
				e.setPhase("confirm", "cut", "")
			} else {
				e.setPhase("confirm", "fail", "safety stop")
				e.log("ABORT: %v", err)
			}
			return
		}
		if good {
			e.setItem("confirm", item, "pass", "")
		} else {
			e.setItem("confirm", item, "fail", w)
		}
		ok, why = good, w
	}
	if ok {
		e.setPhase("confirm", "pass", "confirmed: left on this setting")
		e.mu.Lock()
		c := cand
		e.best, e.curve = &c, []planMV{cand}
		e.confirmed = !e.cfg.DryRun // keep the miner on this setting
		e.mu.Unlock()
		e.log("CONFIRMED: %s is clean, leaving the miner on it", cand.text())
	} else {
		e.setPhase("confirm", "fail", "rejected: back to what it ran before")
		e.log("REJECTED: %s (%s)", cand.text(), why)
	}
}

// buildAndTestPresets builds and tests High / Middle / Low / Lowest power, each held at its own
// temperature (blake2b-asic-control's build_presets):
//   - High is the confirmed setting (its confirm test is reused); after a search with nothing confirmed,
//     the baseline; in a rebuild, the top of the saved curve (which then gets the tries below too).
//   - Lowest power is the curve's first clock at its lowest clean voltage; Middle and Low sit a third and
//     two thirds of the way down from High to it (on the clock grid), at their own lowest clean voltage
//     when the curve has one, else a voltage spread the same way.
//   - Each preset gets up to three settings: as planned, one voltage step more, then one clock step less
//     (Lowest: one and two voltage steps more); each with one retest unless it failed clearly.
//   - The temperatures run from the fan target (High) to the warmest (Lowest power), at most 12 °C below
//     the safety stop. Only the presets that pass are saved, each with a fan curve through the fan % that
//     held its temperature.
func (e *tunerEngine) buildAndTestPresets(base planMV, presetsOnly bool) {
	e.mu.Lock()
	passes := append([]planMV{base}, e.passes...)
	curve := append([]planMV(nil), e.curve...)
	confirmed, best := e.confirmed, e.best
	e.mu.Unlock()
	if len(curve) == 0 {
		e.setPhase("presets", "skip", "no clean clock to build presets from")
		e.log("no curve to build presets from: skipping the presets")
		return
	}
	top := base
	if (presetsOnly || confirmed) && best != nil {
		top = *best
	}
	stepF, mvStep, pmin := e.cfg.Step, e.cfg.MVStep, e.cfg.MinMV
	grid := func(mhz float64) int { // round to the clock grid, as the curve's clocks are
		return int(math.Ceil(math.Round(mhz/stepF)*stepF - 1e-9))
	}
	mk := func(mhz, mv int) planMV {
		mv = max(pmin, min(e.cfg.MaxMV, mv))
		mhz = max(300, mhz)
		return planMV{MHz: mhz, MV: mv, PV: e.cfg.pvFor(mv)}
	}
	lowMV := func(mhz int) (int, bool) { // the lowest voltage that passed at this clock
		best, ok := 0, false
		for _, p := range passes {
			if p.MHz == mhz && (!ok || p.MV < best) {
				best, ok = p.MV, true
			}
		}
		return best, ok
	}
	orMV := func(mhz, dflt int) int {
		if v, ok := lowMV(mhz); ok {
			return v
		}
		return dflt
	}
	down := func(mhz, n int) int { return grid(float64(mhz) - float64(n)*stepF) } // n clock steps slower, on the grid
	clocks := e.cfg.clockList()
	lowestClock := curve[0].MHz
	if len(clocks) > 0 {
		lowestClock = clocks[0]
	}
	var mid, low, lowest planMV
	var lowestTries []planMV
	if lowestClock < top.MHz {
		between := func(f float64) planMV {
			m := grid(float64(top.MHz) - float64(top.MHz-lowestClock)*f)
			v := orMV(m, int(math.Round((float64(top.MV)-float64(top.MV-pmin)*f)/float64(mvStep)))*mvStep)
			return mk(m, v)
		}
		mid, low = between(1.0/3), between(2.0/3)
		lv := orMV(lowestClock, pmin)
		lowest = mk(lowestClock, lv)
		lowestTries = []planMV{lowest, mk(lowestClock, lv+mvStep), mk(lowestClock, lv+2*mvStep)}
	} else {
		mid = mk(down(top.MHz, 1), orMV(down(top.MHz, 1), top.MV))
		lowM := down(top.MHz, 2)
		if v, ok := lowMV(lowM); ok {
			low = mk(lowM, v)
		} else {
			low = mk(lowM, mid.MV-mvStep)
		}
		lowestM := min(grid(math.Floor((float64(base.MHz)-1)/stepF)*stepF), down(lowM, 1))
		lowest = mk(lowestM, pmin)
		lowestTries = []planMV{lowest, mk(down(lowestM, 1), pmin), mk(down(lowestM, 1), pmin+mvStep)}
	}
	highTries := []planMV{top}
	if presetsOnly {
		highTries = []planMV{top, mk(top.MHz, top.MV+mvStep), mk(down(top.MHz, 1), orMV(down(top.MHz, 1), top.MV))}
	}
	tries := map[string][]planMV{
		"High":         highTries,
		"Middle":       {mid, mk(mid.MHz, mid.MV+mvStep), mk(down(mid.MHz, 1), orMV(down(mid.MHz, 1), mid.MV+mvStep))},
		"Low":          {low, mk(low.MHz, low.MV+mvStep), mk(down(low.MHz, 1), orMV(down(low.MHz, 1), low.MV+mvStep))},
		"Lowest power": lowestTries,
	}
	temps := map[string]float64{}
	if e.cfg.FanHold {
		h := e.cfg.FanTargetC
		w := math.Max(h, math.Min(e.cfg.WarmestC, e.cfg.AbortC-12))
		for i, n := range presetOrder {
			temps[n] = math.Round(h + (w-h)*float64(i)/3)
		}
	}
	e.setStatus("presets", "building and testing presets")
	e.setPhase("presets", "run", "")
	e.log("PRESETS: testing each for %g min", e.cfg.PresetMin)
	today := e.now().Format("2006-01-02")
	var out []preset
	nOK := 0
	stoppedBy := ""
	for _, name := range presetOrder {
		if e.stopped() || e.aborted() {
			break
		}
		target, haveT := temps[name]
		if haveT {
			e.holdTarget = target
		}
		e.setItem("presets", name, "run", "")
		e.att = attCtx{key: "presets", item: name}
		if e.cfg.DryRun {
			p := tries[name][0]
			e.log("DRY-RUN preset %s: would test %s for %gmin%s", name, p.text(), e.cfg.PresetMin, map[bool]string{true: fmt.Sprintf(" holding %.0f°C", target)}[haveT])
			e.setItem("presets", name, "pass", "simulated (dry run)")
			continue
		}
		var plan planMV
		var st tunerStats
		ok := false
		e.mu.Lock()
		st0, have0 := e.testStats[[2]int{top.MHz, top.MV}]
		e.mu.Unlock()
		if name == "High" && !presetsOnly && confirmed && e.cfg.ConfirmWin && have0 && st0.Reason == "clean" && top != base {
			// High was just confirmed at the hold temperature: reuse that test
			plan, st, ok = top, st0, true
			e.att.tag = "confirm test"
			e.attBegin(top)
			e.attEnd("pass", "")
			e.attNote("reuses the confirm test")
			e.att.tag = ""
		} else if p := tries[name][0]; e.mapReusable(p, target, haveT) {
			// the map already ran this setting clean, with room to spare, at least this warm (or with the fans
			// at their minimum, so a preset test would run exactly the same): reuse it
			e.mu.Lock()
			plan, st, ok = p, e.testStats[[2]int{p.MHz, p.MV}], true
			e.mu.Unlock()
			e.att.tag = "map test"
			e.attBegin(p)
			e.attEnd("pass", "")
			e.attNote("reuses its clean test from the map")
			e.att.tag = ""
			e.log("PRESET %s: %s was clean in the map (%.0f°C, fans %d %%): reused, not tested again", name, p.text(), st.MaxT, st.AvgFan)
		} else {
			// one setting: a test and, unless it failed decisively, one retest. abort: the phase stops.
			testOne := func(p planMV, at string) (pass, abort bool) {
				plan = p
				e.log("PRESET %s: %s%s", name, p.text(), at)
				back := e.restoreLifted(p)
				defer func() {
					if !pass {
						e.unrestoreLifted(p, back)
					}
				}()
				for attempt := 0; attempt < 2; attempt++ {
					e.att.tag = map[bool]string{true: "retest"}[attempt > 0]
					var why string
					var err error
					pass, why, st, err = e.testFor(p, e.cfg.PresetMin, "preset "+name, e.fanSettleMin())
					e.att.tag = ""
					if err != nil {
						if !e.stopped() {
							stoppedBy = err.Error()
							e.log("preset %s aborted: %v — stopping presets phase", name, err)
						}
						e.setItem("presets", name, "cut", "")
						return false, true
					}
					e.log("%s preset %s %s: %s", map[bool]string{true: "PASS", false: "FAIL"}[pass], name, p.text(), why)
					if pass {
						return true, false
					}
					if decisiveFail(why, st, e.cfg.PresetMin) {
						e.attNote("over its limit in half the test or less: not retested")
						return false, false
					}
					if attempt == 0 {
						e.log("retest preset %s %s once, in case that was a fluke", name, p.text())
					}
				}
				return false, false
			}
			seen := map[[2]int]bool{}
			abort := false
			for _, p := range tries[name] {
				if seen[[2]int{p.MHz, p.MV}] {
					continue
				}
				seen[[2]int{p.MHz, p.MV}] = true
				if ok, abort = testOne(p, map[bool]string{true: fmt.Sprintf(" at %.0f°C", target)}[haveT]); ok || abort {
					break
				}
			}
			// The lower-power presets are tested warmer (quieter fans). A board that is clean at a clock at the
			// map's temperature but fails a slower clock warmer is failing on the heat, not the clock (seen: 550
			// and 525 MHz clean at 71–72 °C, then 500 and 475 MHz failing at 73–75 °C on the same board): one
			// more try at the map's temperature before giving up on the preset.
			if !ok && !abort && haveT && target > e.cfg.FanTargetC+0.5 && !e.stopped() && !e.aborted() {
				p := tries[name][0]
				e.log("PRESET %s: not clean at %.0f°C; trying %s at the map's %.0f°C (a few %% more fan) instead", name, target, p.text(), e.cfg.FanTargetC)
				target = e.cfg.FanTargetC
				e.holdTarget = target
				ok, _ = testOne(p, fmt.Sprintf(" at %.0f°C", target))
			}
		}
		if e.stopped() || stoppedBy != "" {
			break
		}
		if !ok {
			e.setItem("presets", name, "fail", "not clean at any of its settings: not saved")
			continue
		}
		nOK++
		pr := preset{Name: name, MHz: plan.MHz, MV: plan.MV, PV: plan.PV, Tested: today}
		note := "passed"
		if haveT {
			held := st.AvgFan
			if held <= 0 {
				held = e.fanCur
			}
			pr.TargetC = int(target)
			pr.FanProfile = "custom"
			pr.FanPoints = tunedPresetCurve(target, held, e.cfg.FanMinPct, e.cfg.AbortC)
			note = fmt.Sprintf("%s · held %.0f°C at ~%d%% fan", plan.text(), target, held)
			if st.MaxT > 0 && st.MaxT < target-3 {
				note = fmt.Sprintf("%s · %.0f°C with the fans at their minimum (~%d%%): it never got to %.0f°C", plan.text(), st.MaxT, held, target)
			}
		}
		e.setItem("presets", name, "pass", note)
		out = append(out, pr)
	}
	e.att = attCtx{}
	switch {
	case e.stopped():
		e.setPhase("presets", "cut", "")
	case stoppedBy != "":
		e.setPhase("presets", "cut", "stopped: "+stoppedBy)
	case e.cfg.DryRun:
		e.setPhase("presets", "pass", "simulated (dry run)")
	case nOK > 0:
		e.setPhase("presets", "pass", fmt.Sprintf("%d of 4 presets passed", nOK))
	default:
		e.setPhase("presets", "fail", "no preset passed")
	}
	if e.cfg.FanHold {
		e.holdTarget = e.cfg.FanTargetC
	}
	if e.cfg.DryRun {
		e.log("DRY-RUN complete: nothing was changed and no presets were saved")
		return
	}
	if len(out) > 0 {
		if err := mergePresets(out); err != nil {
			e.log("WARNING: the %d presets that passed couldn't be saved (%v): nothing was added to Presets", len(out), err)
			e.setPhase("presets", "fail", "the presets couldn't be saved ("+fsErr(err)+")")
		} else {
			e.log("RESULT: saved %d presets, each with its own fan curve", len(out))
		}
	}
}

// runClockCurve maps every clock from LowestMHz to HighestMHz, each at its lowest clean voltage: the
// algorithm of blake2b-asic-control's asic_tuner.py ("Map every clock and voltage"). The baseline is a
// separate reference run (already done).
//
//   - Bottom-up (the default): the first clock starts at MinMV, each faster clock at the previous clock's
//     lowest clean voltage (a faster clock never needs less). Up a step at a time until clean (to MaxMV);
//     the first clean test is the clock's lowest. A fail on the way up isn't retested (the next step up
//     is cheaper); one at MaxMV is. A pass that was close (marginal) gets one test a step higher too, so
//     the confirm (or confirm off) has a setting with room to spare at that clock.
//   - TopDown (the old walk): the first clock starts at the baseline voltage, each faster clock at the
//     previous clock's lowest plus what that step needed (at most one step); up until clean, then down
//     until it fails (a voltage a slower clock, or this clock, already failed at isn't tested).
//   - A clock that isn't clean at any allowed voltage ends the curve: faster clocks won't be either.
//   - A failed test is retested once (bottom-up: only at MaxMV), unless it went over its whole limit in
//     under half the test.
func (e *tunerEngine) runClockCurve(base planMV) {
	e.setStatus("map", "mapping clocks and voltages")
	e.setPhase("curve", "run", "")
	var curve []planMV
	done := map[[2]int]bool{} // (MHz, mV) -> clean, after its retest
	// the baseline ran clean at its own setting: when the curve gets there, it's reused, not tested again
	baseKey := [2]int{base.MHz, base.MV}
	baseReuse := !e.cfg.DryRun && base.PV == e.cfg.pvFor(base.MV)
	var prevLow *planMV
	lift := 0
	step := e.cfg.MVStep
	try := func(mhz, mv int) (bool, error) {
		key := [2]int{mhz, mv}
		e.att = attCtx{key: "curve", mhz: mhz}
		if baseReuse && key == baseKey {
			e.att.tag = "the baseline"
			e.attBegin(planMV{MHz: mhz, MV: mv})
			e.attEnd("pass", "")
			e.attNote("reuses the baseline run")
			e.att.tag = ""
			e.log("%d MHz / %s: clean in the baseline, not tested again", mhz, e.cfg.vText(mv))
			done[key] = true
			return true, nil
		}
		cand := planMV{MHz: mhz, MV: mv, PV: e.cfg.pvFor(mv)}
		ok, why, st, err := e.test(cand)
		var made []madeTrim
		e.att.tag = "after a chip trim"
		ok, why, err = e.trimAndRetest(cand, ok, why, st, err, &made)
		st = e.trimStats
		e.att.tag = ""
		retested := false
		if len(made) == 0 && err == nil && !ok && !e.stopped() && !e.cfg.TopDown && mv < e.cfg.MaxMV {
			// walking up: a fail costs minutes, so the next step up is cheaper than a retest; a borderline fail
			// may leave this clock one step higher than it needs (the top of the range is still retested)
			e.log("%s failed (%s): one step more voltage instead of a retest", cand.text(), why)
			e.attNote("walking up: no retest, next voltage instead")
		} else if len(made) == 0 && err == nil && !ok && !e.stopped() {
			if decisiveFail(why, st, e.cfg.HoldMin) {
				e.log("%s: no retest, it went over its whole limit in %.1f of %g min (at least twice the allowed rate)", cand.text(), st.WatchedMin, e.cfg.HoldMin)
				e.attNote("over its limit in half the test or less: not retested")
			} else if can, _ := e.combinedOK(cand, st, tunerStats{WatchedMin: e.cfg.HoldMin}); !can && st.Gained != nil {
				e.log("%s: no retest, even a clean retest couldn't make both tests together pass (%s)", cand.text(), why)
				e.attNote("a clean retest couldn't make up for it: not retested")
			} else {
				e.log("%s failed (%s): retesting it once, it may have been bad luck", cand.text(), why)
				e.att.tag = "retest"
				var st2 tunerStats
				ok, why, st2, err = e.test(cand)
				e.att.tag = ""
				if ok && err == nil && st.Gained != nil {
					// a fail then a pass isn't clean by itself: both tests together must be within the limit
					// for their combined length (6 errors then 5 on one chip is a chip at its limit, not luck)
					if ok2, why2 := e.combinedOK(cand, st, st2); !ok2 {
						ok, why = false, why2
						e.log("%s: clean on the retest, but not over both tests together (%s)", cand.text(), why2)
						e.attNote("clean on the retest, not over both tests: " + why2)
					} else {
						retested = true
					}
				}
				st = st2
			}
		}
		if err != nil {
			if !e.stopped() {
				e.log("ABORT: %v", err)
			}
			return false, err
		}
		done[key] = ok
		if ok {
			e.mu.Lock()
			e.passes = append(e.passes, cand)
			why := marginalWhy(st, retested)
			if why == "" {
				why = e.noisyChip(cand, st)
			}
			if why == "" && len(made) > 0 {
				why = "clean with a chip trim"
			}
			if why != "" {
				e.marginal[key] = why
			}
			e.mu.Unlock()
		}
		return ok, nil
	}
	cutClock := func(mhz int) {
		if e.stopped() {
			e.mu.Lock()
			by := e.stopWhy
			e.mu.Unlock()
			e.setClock(mhz, "cut", "stopped by "+map[bool]string{true: "you", false: by}[by == ""])
		} else {
			e.setClock(mhz, "cut", "safety stop")
		}
	}
	clocks := e.cfg.clockList()
clockLoop:
	for ci, mhz := range clocks {
		if e.stopped() {
			break
		}
		e.setClock(mhz, "run", "")
		start := base.MV
		switch {
		case !e.cfg.TopDown && prevLow != nil:
			start = prevLow.MV // a faster clock never needs less: anything lower would only fail
		case !e.cfg.TopDown:
			start = e.cfg.MinMV // the bottom of the range
		case prevLow != nil:
			start = prevLow.MV + min(lift, step)
		}
		start = max(e.cfg.MinMV, min(e.cfg.MaxMV, start))
		if e.floorPV > 0 && e.cfg.pvFor(start) < e.floorPV {
			start = e.floorPV - (e.cfg.pvFor(start) - start) // the boards don't go lower: start at the floor
		}
		if e.cfg.TopDown && prevLow != nil && start != prevLow.MV {
			if tunerOnBox2() {
				e.log("%d MHz: starting at %s (the last clock step needed %s more voltage)", mhz, e.cfg.vText(start), box2VoltStepText(lift))
			} else {
				e.log("%d MHz: starting at %s (the last clock step needed +%d mV)", mhz, e.cfg.vText(start), lift)
			}
		} else if !e.cfg.TopDown {
			e.log("%d MHz: starting at %s and going up only if it fails", mhz, e.cfg.vText(start))
		}
		// up until clean
		var low *planMV
		v := start
		for {
			ok, err := try(mhz, v)
			if err != nil || e.stopped() {
				cutClock(mhz)
				break clockLoop
			}
			if ok {
				low = &planMV{MHz: mhz, MV: v, PV: e.cfg.pvFor(v)}
				break
			}
			if v >= e.cfg.MaxMV {
				break
			}
			v = min(v+step, e.cfg.MaxMV) // the last step lands on the maximum
			e.log("%d MHz: more voltage, %s", mhz, e.cfg.vText(v))
		}
		if low == nil {
			e.setClock(mhz, "fail", fmt.Sprintf("not clean up to %s (the highest allowed): the top of the curve", e.cfg.vText(e.cfg.MaxMV)))
			e.log("%d MHz: no clean voltage up to %s: faster clocks won't be clean either, the curve ends here", mhz, e.cfg.vText(e.cfg.MaxMV))
			for _, m := range clocks[ci+1:] {
				e.setClock(m, "skip", fmt.Sprintf("not tried: %d MHz wasn't clean at any allowed voltage", mhz))
			}
			break
		}
		// bottom-up: a narrow first pass is the clock's lowest, but a poor setting to end on; one test a step
		// higher gives the confirm (and confirm off) a setting with room to spare at this clock, as the old walk's
		// passes on the way down did. No chip trim for it: a fail there is luck, not a weak chip.
		if !e.cfg.TopDown && low.MV < e.cfg.MaxMV {
			e.mu.Lock()
			why := e.marginal[[2]int{mhz, low.MV}]
			e.mu.Unlock()
			if why != "" && why != "clean with a chip trim" {
				up := planMV{MHz: mhz, MV: min(low.MV+step, e.cfg.MaxMV)}
				up.PV = e.cfg.pvFor(up.MV)
				if tunerOnBox2() {
					e.log("%d MHz: %s passed narrowly (%s): testing %s too, for a setting with room to spare", mhz, e.cfg.vText(low.MV), why, e.cfg.vText(up.MV))
				} else {
					e.log("%d MHz: PV %d passed narrowly (%s): testing PV %d too, for a setting with room to spare", mhz, low.PV, why, up.PV)
				}
				e.att = attCtx{key: "curve", mhz: mhz, tag: "room to spare"}
				ok, _, st, err := e.test(up)
				e.att = attCtx{}
				if err != nil || e.stopped() {
					cutClock(mhz)
					break clockLoop
				}
				if ok {
					e.mu.Lock()
					e.passes = append(e.passes, up)
					if w := marginalWhy(st, false); w != "" {
						e.marginal[[2]int{up.MHz, up.MV}] = w
					} else if w := e.noisyChip(up, st); w != "" {
						e.marginal[[2]int{up.MHz, up.MV}] = w
					}
					e.mu.Unlock()
				}
			}
		}
		// then down until it fails: two in a row, or one a slower clock already failed at (the old walk only:
		// walking up from the bottom, the first pass is already the lowest)
		if e.cfg.FindLowest && e.cfg.TopDown {
			misses := 0
			for mv := low.MV - step; mv >= e.cfg.MinMV; mv -= step {
				if failedAt, ok := slowerFailed(done, mhz, mv); ok {
					e.log("%d MHz: not testing %s, %d MHz already failed there", mhz, e.cfg.vText(mv), failedAt)
					break
				}
				if clean, tested := done[[2]int{mhz, mv}]; tested && !clean {
					// it failed at this very clock a moment ago (on the way up): every voltage below it can only
					// fail too, so the walk ends here (blake2b-asic-control counts a miss and tries one more)
					e.log("%d MHz: not testing %s again, it already failed at this clock", mhz, e.cfg.vText(mv))
					break
				}
				if e.floorPV > 0 && e.cfg.pvFor(mv) < e.floorPV {
					e.log("%d MHz: not testing PV %d, the boards don't go below the voltage they had at PV %d", mhz, e.cfg.pvFor(mv), e.floorPV)
					break
				}
				above := mv + step // the step before this one at this clock
				ok, err := try(mhz, mv)
				if err != nil || e.stopped() {
					cutClock(mhz)
					break clockLoop
				}
				// the boards' voltage didn't fall with the setting: the miner's floor. Lower settings would
				// only repeat this test, so the walk ends; the setting above stays the lowest that counts.
				if prevV, curV := e.voltOf(mhz, above), e.voltOf(mhz, mv); prevV > 0 && curV > 0 && prevV-curV < 20 {
					e.floorPV = e.cfg.pvFor(above)
					e.dropPass(mhz, mv)
					e.attNote(fmt.Sprintf("the boards still read %.2f V: the miner doesn't go lower", curV/1000))
					e.log("%d MHz: at PV %d the boards still read %.2f V, as at PV %d: the miner doesn't supply less (its floor is about PV %d); not going lower", mhz, e.cfg.pvFor(mv), curV/1000, e.cfg.pvFor(above), e.floorPV)
					break
				}
				if ok {
					low, misses = &planMV{MHz: mhz, MV: mv, PV: e.cfg.pvFor(mv)}, 0
					continue
				}
				// a fail here was already retested (or failed decisively): less voltage can only do worse
				misses++
				break
			}
		}
		note := fmt.Sprintf("lowest clean voltage %s", e.cfg.vText(low.MV))
		if low.MV-step < e.cfg.MinMV {
			note += " (the lowest allowed)"
		}
		if n := len(e.runTrims); n > 0 {
			note += fmt.Sprintf(", %d chip(s) trimmed", n)
		}
		e.setClock(mhz, "pass", note)
		if prevLow != nil {
			lift = max(0, low.MV-prevLow.MV)
		}
		prevLow = low
		curve = append(curve, *low)
		top := *low
		e.mu.Lock()
		e.curve = append([]planMV(nil), curve...)
		e.best = &top // the highest clean clock so far
		e.mu.Unlock()
		e.log("%d MHz: lowest clean = %s", mhz, e.cfg.vText(low.MV))
	}
	var pts []string
	for _, c := range curve {
		if tunerOnBox2() {
			pts = append(pts, fmt.Sprintf("%d MHz %s", c.MHz, e.cfg.vText(c.MV)))
			continue
		}
		pts = append(pts, fmt.Sprintf("%d MHz PV %d", c.MHz, c.pvOf()))
	}
	switch {
	case e.stopped():
		e.setPhase("curve", "cut", "")
	case e.aborted():
		e.setPhase("curve", "cut", "safety stop: "+e.abortReason())
	case len(curve) == 0:
		e.setPhase("curve", "fail", "no clock in the range was clean")
	default:
		e.setPhase("curve", "pass", "lowest clean voltage per clock: "+strings.Join(pts, ", "))
	}
	if len(curve) > 0 && !e.cfg.DryRun && !e.stopped() && !e.aborted() { // a dry run's curve is made up, a cut one partial: never for a later rebuild
		lastCurveSet(curve)
		e.log("RESULT: mapped %d clean clock(s), %d–%d MHz. Review the curve before applying.", len(curve), curve[0].MHz, curve[len(curve)-1].MHz)
	} else {
		switch {
		case len(curve) > 0 && (e.aborted() || e.stopped()):
			e.log("RESULT: the run stopped after %d clock(s): the curve so far is shown, not saved (a cut curve isn't used for a rebuild)", len(curve))
		case e.aborted() || e.stopped():
			e.log("RESULT: the run stopped before a clock was finished, so there's no curve (the reason is above)")
		case len(curve) > 0:
			e.log("RESULT: dry run: %d clock(s) would have been mapped, %d–%d MHz (nothing was applied, the curve isn't saved)", len(curve), curve[0].MHz, curve[len(curve)-1].MHz)
		default:
			e.log("RESULT: no clean clocks found in the range")
		}
	}
}

func (e *tunerEngine) abortReason() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.abortWhy
}

// addItem adds a row under a plan phase (a confirm candidate), once.
func (e *tunerEngine) addItem(key, label string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if ph := e.stepByKey(key); ph != nil {
		for _, k := range ph.Kids {
			if k.Label == label {
				return
			}
		}
		ph.Kids = append(ph.Kids, tunerStep{Label: label, State: "todo"})
	}
}

// setItem sets a row's state (a preset, a confirm candidate, a round).
func (e *tunerEngine) setItem(key, label, state, note string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if ph := e.stepByKey(key); ph != nil {
		for i := range ph.Kids {
			if ph.Kids[i].Label == label {
				ph.Kids[i].State = state
				if note != "" {
					ph.Kids[i].Note = note
				}
			}
		}
	}
}

// confirmWinner: the fastest clean setting runs again for ConfirmX tests' time; one that doesn't hold
// falls back to the next (clock fastest first, then the lower voltage first), until one holds. The run
// ends on the confirmed setting. (blake2b-asic-control's confirm step.)
func (e *tunerEngine) confirmWinner(base planMV) {
	e.mu.Lock()
	cands := append([]planMV(nil), e.passes...)
	e.mu.Unlock()
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].MHz != cands[j].MHz {
			return cands[i].MHz > cands[j].MHz
		}
		return cands[i].MV < cands[j].MV
	})
	if len(cands) == 0 {
		e.setPhase("confirm", "skip", "no clean setting to confirm")
		return
	}
	// settings that passed with room to spare first (a marginal pass mostly fails a longer test), and at
	// most confirmCap of them: a run shouldn't spend hours re-testing what the map already called close
	e.mu.Lock()
	marg := map[[2]int]string{}
	for k, v := range e.marginal {
		marg[k] = v
	}
	e.mu.Unlock()
	// a marginal pass ranks as one clock step slower, not below every clean pass: after the top clock's
	// confirm fails, the next is the next clock, not the slowest clean one
	rank := func(c planMV) float64 {
		if marg[[2]int{c.MHz, c.MV}] != "" {
			return float64(c.MHz) - e.cfg.Step - 0.5
		}
		return float64(c.MHz)
	}
	sort.SliceStable(cands, func(i, j int) bool { return rank(cands[i]) > rank(cands[j]) })
	if len(cands) > confirmCap {
		// at least one setting with room to spare: when every faster clock passed only narrowly (a weak board
		// close to its limits everywhere), the three narrow ones can all fail and nothing would be confirmed,
		// while a slower clock passed with room to spare (seen: 575–700 MHz all narrow, 550 MHz solid)
		solid := -1
		for i, c := range cands {
			if marg[[2]int{c.MHz, c.MV}] == "" {
				solid = i
				break
			}
		}
		if solid >= confirmCap {
			c := cands[solid]
			copy(cands[confirmCap:solid+1], cands[confirmCap-1:solid])
			cands[confirmCap-1] = c
			e.log("confirm: %s passed with room to spare: it's tried if the faster ones don't hold", c.text())
		}
		for _, c := range cands[confirmCap:] {
			e.log("confirm: not trying %s (only the best %d candidates are confirmed)", c.text(), confirmCap)
		}
		cands = cands[:confirmCap]
	}
	for _, c := range cands {
		if why := marg[[2]int{c.MHz, c.MV}]; why != "" {
			e.log("confirm candidate %s is marginal (%s): tried after the others", c.text(), why)
		}
	}
	hold := e.cfg.HoldMin * e.cfg.ConfirmX
	e.setStatus("confirm", "confirming the best setting")
	e.setPhase("confirm", "run", "")
	chipFailedAt := map[int]string{} // clock → chip that failed its confirm there (more voltage didn't help it)
	for _, c := range cands {
		if e.stopped() || e.aborted() {
			break
		}
		if ch := chipFailedAt[c.MHz]; ch != "" {
			e.log("confirm: not trying %s: chip %s already failed the confirm at this clock", c.text(), ch)
			continue
		}
		if len(e.lifted) > 0 {
			e.confirmLifted(c)
		}
		item := fmt.Sprintf("%d MHz · %s", c.MHz, e.cfg.vText(c.MV))
		e.addItem("confirm", item)
		e.att = attCtx{key: "confirm", item: item, tag: "confirm"}
		e.log("CONFIRM %s for %g min", c.text(), hold)
		ok, why, st, err := e.testFor(c, hold, "confirm", 0)
		if err == nil && !ok && !e.stopped() {
			if decisiveFail(why, st, hold) {
				e.attNote("over its limit in half the test or less: not retested")
			} else {
				e.log("retest the confirm of %s once, in case that was a fluke", c.text())
				e.att.tag = "confirm retest"
				var st2 tunerStats
				ok, why, st2, err = e.testFor(c, hold, "confirm", 0)
				if ok && err == nil && st.Gained != nil {
					if ok2, why2 := e.combinedOK(c, st, st2); !ok2 {
						ok, why = false, why2
						e.log("%s: clean on the confirm retest, but not over both confirms together (%s)", c.text(), why2)
					}
				}
			}
		}
		e.att = attCtx{}
		if err != nil {
			e.setItem("confirm", item, "cut", "")
			if !e.stopped() {
				e.log("ABORT: %v", err)
			}
			break
		}
		if ok {
			e.setItem("confirm", item, "pass", "confirmed")
			cc := c
			e.mu.Lock()
			e.best = &cc
			e.confirmed = !e.cfg.DryRun
			e.mu.Unlock()
			e.setPhase("confirm", "pass", "confirmed "+c.text())
			e.log("CONFIRMED: %s", c.text())
			e.trimsNotNeeded(cc)
			if !e.cfg.DryRun {
				sc := loadSavedCurve()
				sc.Confirmed = &cc
				saveSavedCurve(sc)
			}
			return
		}
		e.setItem("confirm", item, "fail", why)
		e.log("not confirmed: %s (%s)", c.text(), why)
		if st.FailByChip {
			chipFailedAt[c.MHz] = chipName(st.FailChip)
		}
		if !e.cfg.DryRun {
			sc := loadSavedCurve()
			sc.Rejected = append(sc.Rejected, c)
			saveSavedCurve(sc)
		}
	}
	switch {
	case e.stopped():
		e.setPhase("confirm", "cut", "")
	case e.aborted():
		e.setPhase("confirm", "cut", "safety stop")
	default:
		e.setPhase("confirm", "fail", "nothing confirmed: the miner goes back to what it ran before")
		e.log("CONFIRM: nothing held; the presets start from the baseline %s", base.text())
	}
}

const confirmCap = 3

// noisyChip: a chip made 4 or more errors, 3 times what it normally makes in that time (a single error at
// baseline lifts a chip's limit a lot; this keeps such a pass out of the confirm's first slots).
func (e *tunerEngine) noisyChip(p planMV, st tunerStats) string {
	hours := st.WatchedMin / 60
	scale := 1.0
	if e.baseMHz > 0 {
		scale = math.Max(0.5, float64(p.MHz)/float64(e.baseMHz))
	}
	for k, g := range st.Gained {
		if exp := e.baseRate[k] * hours * scale; g >= 4 && float64(g) >= 3*exp {
			return fmt.Sprintf("chip %s made %d errors, %.0f× its normal", chipName(k), g, float64(g)/math.Max(exp, 0.1))
		}
	}
	return ""
}

// mapReusable: the map's own test of p can stand in for a preset test held at target.
func (e *tunerEngine) mapReusable(p planMV, target float64, haveT bool) bool {
	k := [2]int{p.MHz, p.MV}
	e.mu.Lock()
	st, have := e.testStats[k]
	marg := e.marginal[k]
	passed := false
	for _, q := range e.passes {
		if q.MHz == p.MHz && q.MV == p.MV {
			passed = true
		}
	}
	e.mu.Unlock()
	if !have || !passed || marg != "" || st.Reason != "clean" || st.WatchedMin < e.cfg.PresetMin {
		return false
	}
	if len(e.liftedNeeded(p)) > 0 {
		return false // the map ran it clean with a trim the trim check lifted since: not the same chips now
	}
	if !haveT {
		return true
	}
	return st.MaxT >= target-3 || (st.AvgFan > 0 && st.AvgFan <= e.cfg.FanMinPct)
}

// liftedNeeded: the trims the trim check lifted (for the best setting) that p ran clean with in the map, at the
// value p had then. Chip trims are shared by every preset: a slower clock mapped clean with a trim may need it
// even when the best setting doesn't (seen: 625 MHz / PV 9150 mapped clean with b3.c33 at -12.5; the trim check at
// 700 MHz / PV 9500 lifted it; the Middle preset then failed on b3.c33 and took PV 9200 instead).
func (e *tunerEngine) liftedNeeded(p planMV) map[[2]int]float64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	snap, ok := e.trimsAtPass[[2]int{p.MHz, p.MV}]
	if !ok || len(e.lifted) == 0 {
		return nil
	}
	cur := e.runTrims
	out := map[[2]int]float64{}
	for k := range e.lifted {
		if v, had := snap[k]; had && v < 0 && v < cur[k] {
			out[k] = v
		}
	}
	return out
}

// restoreLifted: before a preset tests p, the trims the trim check lifted that p ran clean with in the map go back
// to the value they had in that test, so p is tested on the same chips the map passed it with (the trim check only
// showed they aren't needed at the best setting). All of them at once, before the test: waiting for a fail on one
// of those chips misses a fail the miner counts on the whole board (two of them failing together) and a second
// needed trim. Returns each restored trim's value before (for unrestoreLifted). They are a subset of the trims the
// map ended with, so the per-board cap and the pair rule still hold.
func (e *tunerEngine) restoreLifted(p planMV) map[[2]int]float64 {
	need := e.liftedNeeded(p)
	keys := make([][2]int, 0, len(need))
	for k := range need {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		return keys[i][0] < keys[j][0] || keys[i][0] == keys[j][0] && keys[i][1] < keys[j][1]
	})
	back := map[[2]int]float64{}
	var parts []string
	for _, k := range keys {
		v := need[k]
		e.mu.Lock()
		cur := e.runTrims[k]
		e.mu.Unlock()
		if err := e.trimSet(k[0], k[1], v); err != nil {
			e.trimSet(k[0], k[1], cur) // the file may be written though the live apply failed: put it back
			e.log("WARNING: couldn't put b%d.c%d back to %+.4g MHz for %s: %v", k[0], k[1], v, p.text(), err)
			continue
		}
		back[k] = cur
		e.mu.Lock()
		if e.runTrims == nil {
			e.runTrims = map[[2]int]float64{}
		}
		e.runTrims[k] = v
		e.mu.Unlock()
		parts = append(parts, fmt.Sprintf("b%d.c%d %+.4g→%+.4g MHz", k[0], k[1], cur, v))
	}
	if len(parts) == 0 {
		return nil
	}
	e.mu.Lock()
	kept := copyTrimMap(e.runTrims)
	e.mu.Unlock()
	if e.onTrims != nil {
		e.onTrims(kept)
	}
	e.log("%s ran clean in the map with chip trims the trim check lifted at %s: they go back for this test: %s", p.text(), e.liftedAt.text(), strings.Join(parts, ", "))
	return back
}

// unrestoreLifted: p didn't pass with the trims restoreLifted put back: they are lifted again (as the confirmed
// setting runs them), so the next try and the trims the run keeps are what was tested.
func (e *tunerEngine) unrestoreLifted(p planMV, back map[[2]int]float64) {
	if len(back) == 0 {
		return
	}
	keys := make([][2]int, 0, len(back))
	for k := range back {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		return keys[i][0] < keys[j][0] || keys[i][0] == keys[j][0] && keys[i][1] < keys[j][1]
	})
	var parts []string
	for _, k := range keys {
		v := back[k]
		if err := e.trimSet(k[0], k[1], v); err != nil {
			e.log("WARNING: couldn't lift b%d.c%d again to %+.4g MHz: %v (it stays slowed)", k[0], k[1], v, err)
			continue
		}
		e.mu.Lock()
		if v == 0 {
			delete(e.runTrims, k)
		} else {
			e.runTrims[k] = v
		}
		e.mu.Unlock()
		parts = append(parts, fmt.Sprintf("b%d.c%d %+.4g MHz", k[0], k[1], v))
	}
	if len(parts) == 0 {
		return
	}
	e.mu.Lock()
	kept := copyTrimMap(e.runTrims)
	e.mu.Unlock()
	if e.onTrims != nil {
		e.onTrims(kept)
	}
	e.log("%s didn't pass with them: the trims put back for it are lifted again (%s)", p.text(), strings.Join(parts, ", "))
}

// marginalWhy: a clean test that was close: its worst chip used 80 % or more of its limit (and at least 2
// errors), or it was clean only on its retest.
func marginalWhy(st tunerStats, retested bool) string {
	if retested {
		return "clean only on its retest"
	}
	worst, ratio := ckey{}, 0.0
	for k, g := range st.Gained {
		if lim := st.Limits[k]; lim > 0 && g >= 2 {
			if r := float64(g) / lim; r > ratio {
				worst, ratio = k, r
			}
		}
	}
	if ratio >= 0.8 {
		return fmt.Sprintf("chip %s used %.0f%% of its limit", chipName(worst), ratio*100)
	}
	return ""
}

// combinedOK judges two tests of the same setting as one test of their combined length.
func (e *tunerEngine) combinedOK(p planMV, a, b tunerStats) (bool, string) {
	hours := (a.WatchedMin + b.WatchedMin) / 60
	if hours <= 0 {
		return true, ""
	}
	scale := 1.0
	if e.baseMHz > 0 {
		scale = math.Max(0.5, float64(p.MHz)/float64(e.baseMHz))
	}
	sum := map[ckey]int{}
	for k, v := range a.Gained {
		sum[k] += v
	}
	for k, v := range b.Gained {
		sum[k] += v
	}
	n := len(sum)
	worst, over := ckey{}, math.Inf(-1)
	for k, g := range sum {
		if d := float64(g) - e.chipLimit(k, hours, scale, n); d > over {
			worst, over = k, d
		}
	}
	if over > 0 {
		return false, fmt.Sprintf("chip %s +%d errors over both tests (limit %.1f)", chipName(worst), sum[worst], e.chipLimit(worst, hours, scale, n))
	}
	boards := map[int]bool{}
	for k := range sum {
		boards[k.B] = true
	}
	for bd := range boards {
		var base float64
		g := 0
		for k, v := range sum {
			if k.B != bd {
				continue
			}
			if _, w := e.weak[k]; !w {
				base += e.baseRate[k]
				g += v
			}
		}
		if lim := tunerLimitVal(base, hours, scale, false, e.cfg.WeakFactor, e.margin(hours), len(boards), e.cfg.Sigma); float64(g) > lim {
			return false, fmt.Sprintf("board %d +%d errors over both tests (limit %.1f)", bd, g, lim)
		}
	}
	return true, ""
}

// slowerFailed: a slower clock already failed at mv (a faster clock needs at least as much voltage).
func slowerFailed(done map[[2]int]bool, mhz, mv int) (int, bool) {
	best, found := 0, false
	for k, clean := range done {
		if k[0] < mhz && k[1] == mv && !clean && (!found || k[0] > best) {
			best, found = k[0], true
		}
	}
	return best, found
}

// decisiveFail: a chip or board went over its whole limit in under half the test: not bad luck, so no
// retest. Anything else that failed (later in the test, or on hashrate) gets one retest.
func decisiveFail(why string, st tunerStats, holdMin float64) bool {
	return strings.Contains(why, "errors (limit") && st.WatchedMin <= holdMin/2
}

func (e *tunerEngine) Stop() { e.StopFor("") }

// StopFor stops the run on behalf of a guard (why says which and why); "" is the user's Stop.
func (e *tunerEngine) StopFor(why string) {
	if e.dead.Load() || !e.lockLive() {
		return
	}
	defer e.mu.Unlock()
	if e.running && e.stopCh != nil && e.stopWhy == "" {
		e.stopWhy = why
	}
	if e.running && e.stopCh != nil {
		select {
		case <-e.stopCh:
		default:
			close(e.stopCh)
		}
	}
}

// StopForSoon: StopFor for a safety stop (the thermal guard, the fan lock, the pool guard), which goes on to its
// Idle after at most d: a run frozen holding e.mu would otherwise hold that Idle up until the watchdog ends it
// (tunerStuckAfter). The stop is still delivered once the run can take it (or given up when the watchdog ends the
// run); the guard's safetyStop has already made the run's writes refused. running: it was asked in time and was going.
func (e *tunerEngine) StopForSoon(why string, d time.Duration) (running bool) {
	res := make(chan bool, 1)
	go func() {
		r := e.isRunning()
		if r {
			e.StopFor(why)
		}
		res <- r
	}()
	select {
	case r := <-res:
		return r
	case <-time.After(d):
		return false
	}
}

// lockLive takes e.mu, unless the watchdog ends the run while it waits (a run frozen holding e.mu): false then.
// For what the rest of the panel calls (Stop, isRunning, the status), so none of it can hang on a frozen run.
func (e *tunerEngine) lockLive() bool {
	for !e.mu.TryLock() {
		if e.dead.Load() {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
	return true
}

func (e *tunerEngine) isRunning() bool {
	if e.dead.Load() || !e.lockLive() {
		return false
	}
	defer e.mu.Unlock()
	return e.running
}

func (e *tunerEngine) getCurve() []planMV {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]planMV(nil), e.curve...)
}

func (e *tunerEngine) bestText() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.best == nil {
		return ""
	}
	return e.best.text()
}

func (e *tunerEngine) statusView() map[string]any {
	if !e.lockLive() { // ended by the watchdog while frozen holding e.mu
		return map[string]any{"running": false, "phase": "done", "message": e.endMsg(), "cfg": e.cfg.hwView()}
	}
	defer e.mu.Unlock()
	running := e.running && !e.dead.Load() // (a run the watchdog ended may still be frozen)
	logs := append([]string(nil), e.logbuf...)
	if len(logs) > 80 {
		logs = logs[len(logs)-80:]
	}
	// per-chip errors vs each chip's own limit (for the live error chart): the test being watched while
	// running (empty while a new setting settles), else the last finished one
	view := e.lastStat
	if running {
		view = e.live
	}
	var cur *curStep
	if e.cur != nil {
		c := *e.cur
		if c.Settling {
			c.ElapsedMin = e.now().Sub(c.started).Minutes()
		}
		cur = &c
	}
	chips := make([]map[string]any, 0, len(view.Gained))
	for k, g := range view.Gained {
		lim := view.Limits[k]
		pct := 0.0
		if lim > 0 {
			pct = float64(g) / lim * 100
		}
		_, weak := e.weak[k]
		chips = append(chips, map[string]any{"b": k.B, "i": k.I, "gained": g, "limit": lim, "pct": pct, "weak": weak})
	}
	sort.Slice(chips, func(i, j int) bool { return chips[i]["pct"].(float64) > chips[j]["pct"].(float64) })
	return map[string]any{
		"running": running, "phase": e.phase, "message": e.endMsg(),
		"dry_run": e.cfg.DryRun, "mode": e.cfg.Mode,
		"base_mhz": e.baseMHz, "weak": len(e.weak), "cfg": e.cfg.hwView(), "base": e.baseUsed.hw(),
		"curve": hwList(e.curve), "best": e.best.hwPtr(), "log": logs, "chips": chips, "chips_for": e.liveFor, "chips_last": e.lastFor, "plan": copySteps(e.steps),
		"results": append([]testResult(nil), e.results...),
		"ntests":  len(e.results), "nread": len(e.readings),
		"started": e.startedAt.Format("2006-01-02 15:04"),
		"stat": map[string]any{
			"total": view.Total, "watched_min": view.WatchedMin,
			"max_t": view.MaxT, "avg_ghs": view.AvgGHS,
		},
		"poll_s": e.cfg.PollS, "now": e.now().Unix(),
		// board-wide baseline error rate (errors/min) so the live view can show the margin
		"base_rate_min": e.baseRateMin(),
		// the test running right now (nil between steps) — drives the Current-step panel
		"cur": cur,
		// seconds since the run's last sign of life (the watchdog's heartbeat): the panel warns long before
		// the watchdog's 10 minutes, instead of showing a frozen timer
		"quiet_s": quietSec(e.running, e.beat.Load()),
		// chip trims this run set and kept
		"trims": trimList(e.runTrims),
	}
}

// baseRateMin sums the per-chip baseline error rates (errors/hour) into a board errors/minute value.
// Returns 0 until the baseline has been measured. Caller holds e.mu.
func (e *tunerEngine) baseRateMin() float64 {
	if len(e.baseRate) == 0 {
		return 0
	}
	sum := 0.0
	for _, r := range e.baseRate {
		sum += r
	}
	return sum / 60.0
}

// tunerBundle is a snapshot of a run for file export.
type tunerBundle struct {
	cfg      tunerConfig
	started  time.Time
	finished time.Time
	mode     string
	baseMHz  int
	best     *planMV
	curve    []planMV
	fullLog  []string
	results  []testResult
	readings []liveReading
	chips    []chipFinal
}

type chipFinal struct {
	Board, Chip int
	Gained      int
	Limit       float64
	Weak        bool
}

func (e *tunerEngine) exportBundle() tunerBundle {
	e.mu.Lock()
	defer e.mu.Unlock()
	b := tunerBundle{
		cfg: e.cfg, started: e.startedAt, finished: e.finishedAt,
		mode: e.cfg.Mode, baseMHz: e.baseMHz, best: e.best,
		curve:    append([]planMV(nil), e.curve...),
		fullLog:  append([]string(nil), e.fullLog...),
		results:  append([]testResult(nil), e.results...),
		readings: append([]liveReading(nil), e.readings...),
	}
	for k, g := range e.lastStat.Gained {
		_, weak := e.weak[k]
		b.chips = append(b.chips, chipFinal{Board: k.B, Chip: k.I, Gained: g, Limit: e.lastStat.Limits[k], Weak: weak})
	}
	sort.Slice(b.chips, func(i, j int) bool {
		if b.chips[i].Board != b.chips[j].Board {
			return b.chips[i].Board < b.chips[j].Board
		}
		return b.chips[i].Chip < b.chips[j].Chip
	})
	return b
}

func validateTunerConfig(c tunerConfig) error {
	if tunerOnBox2() {
		return validateTunerConfigBox2(c)
	}
	switch c.Mode {
	case "map", "confirm", "rebuild":
	default:
		return fmt.Errorf("mode must be map, confirm or rebuild")
	}
	// When overclock is unlocked, the tuner may push past the safe window up to the (possibly
	// probe-measured) hard ceilings — this is how the statistical held-run finds a stable overclock.
	fLo, fHi := freqMin, freqMax
	if loadOverclock().Acknowledged {
		fLo, fHi = freqHardMin, effHard("freq", freqHardMax)
	}
	// voltages are judged as PV (mV + gap): the supply works between PV 9100 and 9600 (measured), unlocked
	// or not; below it runs 9100 anyway, above it ignores the request
	pvOK := func(mv int) bool {
		return mv+c.PVGap >= pvMin && mv+c.PVGap <= pvHardMax && mv >= vrunHardMin && mv <= vrunHardMax
	}
	if c.BaselineSrc == "own" {
		if c.BaselineMHz < fLo || c.BaselineMHz > fHi {
			return fmt.Errorf("baseline clock must be %d–%d MHz", fLo, fHi)
		}
		if !pvOK(c.BaselineMV) {
			return fmt.Errorf("baseline voltage must be PV %d–%d", pvMin, pvHardMax)
		}
	}
	if !c.DryRun { // a chip trimmed "+" runs that much faster than the clocks the run sets
		top := 0
		switch c.Mode {
		case "map":
			top = c.HighestMHz
		case "confirm":
			top = c.ConfirmMHz
		}
		if c.BaselineSrc == "own" {
			top = max(top, c.BaselineMHz)
		}
		if err := plusTrimErr(top); err != nil {
			return err
		}
	}
	if c.ChipTrim && !c.DryRun && featOff(model.Feat.ChipTrims) {
		// (the SC Box II: the tuner may be commissioned without chip trims; its trims would never reach a chip)
		return fmt.Errorf("chip trims are not available on this model (%s): untick “Trim weak chips”", model.Name)
	}
	if c.ChipTrim && (c.ChipTrimMax < 1 || c.ChipTrimMax > 10) {
		return fmt.Errorf("chips the tuner may trim per board must be 1–10")
	}
	if d := c.ChipTrimDeep; d != 0 && (d < 25 || d > 75 || math.Abs(d/trimStep-math.Round(d/trimStep)) > 1e-9) {
		return fmt.Errorf("how far the tuner may slow a chip must be 25–75 MHz, in 12.5 MHz steps")
	}
	if c.Mode == "confirm" && (c.ConfirmHours < 0.5 || c.ConfirmHours > 48) {
		return fmt.Errorf("hours to run the confirm must be 0.5–48")
	}
	if c.BaselineMin <= 0 || c.HoldMin <= 0 || c.SettleMin < 0 {
		return fmt.Errorf("baseline and test minutes must be positive")
	}
	if c.Mode == "map" {
		if c.LowestMHz < fLo || c.LowestMHz > fHi {
			return fmt.Errorf("lowest clock must be %d–%d MHz", fLo, fHi)
		}
		if c.HighestMHz < c.LowestMHz || c.HighestMHz > fHi {
			return fmt.Errorf("highest clock must be between the lowest and %d MHz", fHi)
		}
		if c.Step < 12.5 || c.Step > 100 || !onPLLGrid(c.Step) {
			return fmt.Errorf("clock step must be a multiple of 12.5 MHz (the chips' clock steps), 12.5–100: other steps test the same real clock twice")
		}
		cl := c.clockList()
		if len(cl) == 0 {
			return fmt.Errorf("no clock to test between %d and %d MHz", c.LowestMHz, c.HighestMHz)
		}
		if cl[0] < fLo { // Lowest rounds down to the step grid (300 by 62.5 starts at 250)
			return fmt.Errorf("with %g MHz steps the first clock is %d MHz, under %d MHz: raise the lowest clock or use another step", c.Step, cl[0], fLo)
		}
		// below PV 9100 the supply stays at its floor: those tests would all run the same voltage
		if c.MinMV+c.PVGap < pvMin {
			return fmt.Errorf("lowest PV %d: the supply doesn't go below PV %d (its floor, measured), so lower settings all run the same voltage", c.MinMV+c.PVGap, pvMin)
		}
		if c.MinMV > c.MaxMV || !pvOK(c.MinMV) || !pvOK(c.MaxMV) {
			return fmt.Errorf("voltages must satisfy PV %d ≤ lowest ≤ highest ≤ PV %d (run voltage PV − %d within %d–%d)", pvMin, pvHardMax, c.PVGap, vrunHardMin, vrunHardMax)
		}
		// the supply doesn't go above PV 9600 (a higher request is ignored): no test past it
		if c.MaxMV+c.PVGap > pvHardMax {
			return fmt.Errorf("highest PV %d: the supply doesn't go above PV %d", c.MaxMV+c.PVGap, pvHardMax)
		}
		if c.MVStep < 10 || c.MVStep > 500 {
			return fmt.Errorf("voltage step must be 10–500 mV")
		}
	}
	if c.Mode == "confirm" {
		if c.ConfirmMHz < fLo || c.ConfirmMHz > fHi {
			return fmt.Errorf("confirm clock must be %d–%d MHz", fLo, fHi)
		}
		if !pvOK(c.ConfirmMV) {
			return fmt.Errorf("confirm voltage must be PV %d–%d", pvMin, pvHardMax)
		}
	}
	if c.Sigma < 1.5 || c.Sigma > 4 {
		return fmt.Errorf("sensitivity must be 1.5–4 (2.5 is the default; higher values stop failing anything)")
	}
	if c.AbortC < 70 || c.AbortC > 90 {
		return fmt.Errorf("abort temperature must be 70–90 °C (%s own over-temperature control takes over at 92 °C)", oemPoss())
	}
	if c.PVGap < 0 || c.PVGap > 600 {
		return fmt.Errorf("PV gap must be 0–600 mV")
	}
	if c.PollS < 1 || c.PollS > 60 { // (one poll is one sleep: the heat abort waits for it, the watchdog ends a run quiet for 10 min)
		return fmt.Errorf("poll must be 1–60 s")
	}
	if c.Mode == "map" && c.ConfirmWin && (c.ConfirmX < 1 || c.ConfirmX > 20) {
		return fmt.Errorf("the confirm must be at least as long as one test (%g min) and at most 20 times it", c.HoldMin)
	}
	if c.FanHold {
		if c.FanTargetC < 45 || c.FanTargetC > 85 {
			return fmt.Errorf("fan target must be 45–85 °C")
		}
		if c.FanTargetC > c.AbortC-5 {
			return fmt.Errorf("fan target must be at least 5 °C below the abort temperature")
		}
		if c.FanMinPct < fanMin || c.FanMinPct > 90 {
			return fmt.Errorf("lowest fan must be %d–90 %%", fanMin)
		}
	}
	if c.BuildPresets {
		if c.PresetMin <= 0 {
			return fmt.Errorf("minutes per preset must be positive")
		}
		if c.WarmestC < c.FanTargetC || c.WarmestC > 85 {
			return fmt.Errorf("warmest preset must be between the fan target and 85 °C")
		}
		if c.WarmestC > c.AbortC-12 {
			return fmt.Errorf("warmest preset must be at least 12 °C below the abort temperature (%.0f °C or less), so a preset holding it has room before the safety stop", c.AbortC-12)
		}
	}
	return nil
}

func copyIntMap(m map[ckey]int) map[ckey]int {
	out := make(map[ckey]int, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
func copyFloatMap(m map[ckey]float64) map[ckey]float64 {
	out := make(map[ckey]float64, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// ---- per-chip pass ----

type futileKey struct {
	mhz int
	k   ckey
}

type madeTrim struct {
	key  [2]int
	prev float64
}

// trimAndRetest: a test that failed because one chip went over its limit is run again with that chip
// slowed 12.5 MHz, deeper each time it's that chip again (to the run's deepest, -50 MHz by default), or the next chip if another one
// now fails (up to ChipTrimMax chips per board). The trims stay only if the test then passes; otherwise
// they're undone and the test counts as failed, as without trims.
func (e *tunerEngine) trimAndRetest(cand planMV, ok bool, why string, st tunerStats, err error, made *[]madeTrim) (bool, string, error) {
	e.trimStats = st
	if !e.cfg.ChipTrim || e.cfg.DryRun {
		return ok, why, err
	}
	for err == nil && !ok && !e.stopped() && st.FailByChip {
		fk := futileKey{cand.MHz, st.FailChip}
		if e.trimFutile[fk] {
			e.log("%s: no chip trim (%s still failed at %g MHz at this clock)", cand.text(), chipName(st.FailChip), e.trimFloor())
			break
		}
		desc, reason := e.trimOne(st.FailChip, made)
		if desc == "" {
			if strings.Contains(reason, "is already at") && e.trimFutile != nil {
				e.trimFutile[fk] = true // more voltage first: trying every depth again at the next step costs 4 tests
			}
			e.log("%s: no chip trim (%s)", cand.text(), reason)
			break
		}
		e.log("%s failed (%s): %s, testing again", cand.text(), why, desc)
		e.lastWatchEnd = time.Time{} // a chip's clock just changed: a full settle
		ok, why, st, err = e.test(cand)
		e.trimStats = st
	}
	if len(*made) == 0 {
		return ok, why, err
	}
	if ok {
		cur := e.trimGet()
		var parts []string
		e.mu.Lock()
		if e.runTrims == nil {
			e.runTrims = map[[2]int]float64{}
		}
		for _, m := range *made {
			e.runTrims[m.key] = cur[m.key]
		}
		kept := copyTrimMap(e.runTrims)
		e.mu.Unlock()
		if e.onTrims != nil {
			e.onTrims(kept)
		}
		for k, v := range e.runTrims {
			parts = append(parts, fmt.Sprintf("b%d.c%d %+.4g MHz", k[0], k[1], v))
		}
		sort.Strings(parts)
		e.log("%s is clean with chip trims: they stay (%s)", cand.text(), strings.Join(parts, ", "))
		return ok, why, err
	}
	for i := len(*made) - 1; i >= 0; i-- {
		m := (*made)[i]
		if uerr := e.trimSet(m.key[0], m.key[1], m.prev); uerr != nil {
			e.log("WARNING: couldn't undo the trim on b%d.c%d: %v", m.key[0], m.key[1], uerr)
		}
	}
	if err != nil || e.stopped() {
		e.log("%s: the test with chip trims was cut short: the trims made for it are undone", cand.text())
	} else {
		e.log("%s isn't clean even with chip trims: the trims made for it are undone", cand.text())
	}
	return ok, why, err
}

// trimOne slows the chip one step; desc is "" (with the reason) when it can't.
func (e *tunerEngine) trimOne(k ckey, made *[]madeTrim) (desc, reason string) {
	if _, ok := e.hookStats(); !ok {
		return "", "the chip hook isn't reporting"
	}
	key, ok := e.hookChip(k)
	if !ok {
		return "", "the chip isn't in the hook's chip list"
	}
	chain, chip := key[0], key[1]
	trims := e.trimGet()
	cur := trims[key]
	mine := false
	if _, ok := e.runTrims[key]; ok {
		mine = true
	}
	for _, m := range *made {
		if m.key == key {
			mine = true
		}
	}
	if cur != 0 && !mine {
		return "", fmt.Sprintf("%s has a trim set by hand", chipName(k))
	}
	next := cur - trimStep
	if next < e.trimFloor()-1e-9 {
		return "", fmt.Sprintf("%s is already at %+.4g MHz", chipName(k), cur)
	}
	// one trim per pair: the two chips of a pair share a power stage, and slowing both costs the rest of the
	// board far more than spreading the same trims (christhealien's tests: 16.4 % of the other chips' good work
	// against 6.5 %). With the partner already slowed, the run raises PV instead.
	if pc := chipPartner(chip); pc > 0 && !mine {
		if v := trims[[2]int{chain, pc}]; v < 0 {
			return "", fmt.Sprintf("its partner b%d.c%d is already slowed (%+.4g MHz): one trim per pair", chain, pc, v)
		}
		for _, m := range *made {
			if m.key == [2]int{chain, pc} {
				return "", fmt.Sprintf("its partner b%d.c%d was just slowed: one trim per pair", chain, pc)
			}
		}
	}
	if !mine {
		n := map[[2]int]bool{}
		for kk := range e.runTrims {
			if kk[0] == chain {
				n[kk] = true
			}
		}
		for _, m := range *made {
			if m.key[0] == chain {
				n[m.key] = true
			}
		}
		if len(n) >= e.cfg.ChipTrimMax {
			return "", fmt.Sprintf("%d chips on board %d are trimmed already, the most this run may", len(n), chain)
		}
	}
	if err := e.trimSet(chain, chip, next); err != nil {
		e.trimSet(chain, chip, cur) // the trims file may be written though the live apply failed: put it back
		return "", "couldn't set the trim: " + err.Error()
	}
	*made = append(*made, madeTrim{key: key, prev: cur})
	return fmt.Sprintf("slowing %s to %+.4g MHz", chipName(k), next), ""
}

// undoRunTrims: the run isn't leaving the miner on a confirmed setting, so the chip trims it made go back to
// what they were before it (none of them: the tuner only trims chips that had no trim), as the clock does.
func (e *tunerEngine) undoRunTrims() {
	e.mu.Lock()
	keys := make([][2]int, 0, len(e.runTrims))
	for k := range e.runTrims {
		keys = append(keys, k)
	}
	e.mu.Unlock()
	if len(keys) == 0 {
		return
	}
	sort.Slice(keys, func(i, j int) bool {
		return keys[i][0] < keys[j][0] || keys[i][0] == keys[j][0] && keys[i][1] < keys[j][1]
	})
	var parts []string
	for _, k := range keys {
		if err := e.trimSet(k[0], k[1], 0); err != nil && !strings.Contains(err.Error(), "fresh") && !strings.Contains(err.Error(), "reporting") && !strings.Contains(err.Error(), "not yet at") {
			e.log("WARNING: couldn't take the chip trim off b%d.c%d: %v (Controls -> Chip trims)", k[0], k[1], err)
			continue
		} // (a live re-send that has to wait: the trims file is written, and the clock change below applies it)
		parts = append(parts, fmt.Sprintf("b%d.c%d", k[0], k[1]))
		e.mu.Lock()
		e.trimsUndone++
		delete(e.runTrims, k)
		e.mu.Unlock()
	}
	if len(parts) > 0 {
		e.log("chip trims this run made are taken off, as they were before it: %s", strings.Join(parts, ", "))
	}
	if e.onTrims != nil {
		e.onTrims(copyTrimMap(e.runTrims))
	}
}

func copyTrimMap(m map[[2]int]float64) map[[2]int]float64 {
	out := make(map[[2]int]float64, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func trimList(m map[[2]int]float64) []chipTrim {
	out := []chipTrim{}
	for k, v := range m {
		out = append(out, chipTrim{Chain: k[0], Chip: k[1], MHz: v})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Chain != out[j].Chain {
			return out[i].Chain < out[j].Chain
		}
		return out[i].Chip < out[j].Chip
	})
	return out
}

// onPLLGrid: x is a whole number of the chips' 12.5 MHz clock steps.
func onPLLGrid(x float64) bool { return math.Abs(x/12.5-math.Round(x/12.5)) < 1e-9 }

// clockList: the clocks a map run tests, as the whole-MHz values sent to the miner. With a step that's a
// multiple of 12.5 it walks the chips' real clock steps (a chip runs 12.5 MHz × N, rounding a request
// down): from the step Lowest MHz really runs to the one Highest MHz really runs, each request the
// smallest whole number that lands exactly on its step (587.5 → 588, 600 → 600, 612.5 → 613), so no two
// tests run the same clock and none is skipped. Any other step counts in whole MHz as typed.
func (c tunerConfig) clockList() []int {
	var out []int
	if c.Step <= 0 || c.HighestMHz < c.LowestMHz {
		return out
	}
	// from Lowest, rounded down to the step grid (575 with 25 MHz steps; 587.5 runs as "588" with 12.5),
	// up to Highest; a Highest off the grid is still the last clock tested (blake2b-asic-control's
	// curve_clocks)
	k0 := int(math.Floor(float64(c.LowestMHz)/c.Step + 1e-9))
	for k := k0; float64(k)*c.Step <= float64(c.HighestMHz)+1e-9; k++ {
		out = append(out, int(math.Ceil(float64(k)*c.Step-1e-9)))
	}
	if len(out) > 0 && pll(float64(c.HighestMHz)) > pll(float64(out[len(out)-1])) {
		out = append(out, c.HighestMHz)
	}
	return out
}

// clockLabel: "600 MHz", or "588 MHz · runs 587.5" when the chips run a different clock than asked.
func clockLabel(mhz int) string {
	if r := pll(float64(mhz)); r != float64(mhz) {
		return fmt.Sprintf("%d MHz · runs %g", mhz, r)
	}
	return fmt.Sprintf("%d MHz", mhz)
}

// ---- hashrate during a test ----

// hrMeter measures a test's hashrate from the miner's total-work counter: the work done between two
// readings over the time between them. The miner's own hashrate figures are averages that reach back past
// the test's start (a clock change doesn't reset them), so after a change they drift for many minutes
// toward the new rate (a 575 MHz test after a 615 MHz baseline read 4491 GH/s falling to 4395 over 25
// minutes). The counter has no lag. Without it, the miner's figure is averaged as before.
type hrMeter struct {
	t     []time.Time
	w     []float64 // total work, MH
	sum   float64   // fallback: the miner's figure, averaged
	n     int
	first time.Time
}

const hrLiveWindow = 2 * time.Minute

// add takes one poll's readings and returns the hashrate to show now (GH/s): the last 2 minutes' work.
func (h *hrMeter) add(now time.Time, ghs float64, ghsOK bool, work float64, workOK bool) float64 {
	if ghsOK {
		h.sum += ghs
		h.n++
	}
	if workOK && work > 0 {
		if len(h.w) > 0 && work < h.w[len(h.w)-1] { // the counter restarted (the miner restarted)
			h.t, h.w = nil, nil
		}
		h.t, h.w = append(h.t, now), append(h.w, work)
		j := 0
		for j < len(h.t)-1 && now.Sub(h.t[j]) > hrLiveWindow {
			j++
		}
		if j > 0 && now.Sub(h.t[j]) < hrLiveWindow/2 { // keep at least a minute in the window
			j--
		}
		if dt := now.Sub(h.t[j]).Seconds(); dt >= 20 {
			return (work - h.w[j]) / dt / 1000
		}
	}
	if ghsOK {
		return ghs
	}
	return 0
}

// avg: the test's hashrate so far (GH/s): all the work since its first reading, over that time.
func (h *hrMeter) avg() float64 {
	if n := len(h.w); n >= 2 {
		if dt := h.t[n-1].Sub(h.t[0]).Seconds(); dt >= 20 {
			return (h.w[n-1] - h.w[0]) / dt / 1000
		}
	}
	if h.n > 0 {
		return h.sum / float64(h.n)
	}
	return 0
}

// readTotalMH: the miner's total work since mining started, MH. "Total MH" when the miner reports it,
// else its average since the start times the time since the start (the same number).
func readTotalMH() (float64, bool) {
	raw, err := cgminerCmd("summary")
	if err != nil {
		return 0, false
	}
	e, ok := apiEntry(raw, "SUMMARY", 0)
	if !ok {
		return 0, false
	}
	num := func(k string) float64 { f, _ := e[k].(float64); return f }
	if v := num("Total MH"); v > 0 {
		return v, true
	}
	if av, el := num("MHS av"), num("Elapsed"); av > 0 && el > 0 {
		return av * el, true
	}
	return 0, false
}

// trimCheck: at the best setting, each trim this run made is lifted one step at a time and tested again:
// kept lifted while the setting stays clean, put back at the first test that isn't. That removes trims
// made after a test failed by bad luck, and trims a slower clock needed that the best one doesn't.
func (e *tunerEngine) trimCheck() {
	e.mu.Lock()
	keys := make([][2]int, 0, len(e.runTrims))
	for k := range e.runTrims {
		keys = append(keys, k)
	}
	var best *planMV
	if e.best != nil {
		b := *e.best
		best = &b
	}
	e.mu.Unlock()
	if len(keys) == 0 {
		e.setPhase("trimcheck", "skip", "no chip trims to check")
		return
	}
	if best == nil {
		e.setPhase("trimcheck", "skip", "no clean setting to check them at")
		return
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i][0] != keys[j][0] {
			return keys[i][0] < keys[j][0]
		}
		return keys[i][1] < keys[j][1]
	})
	e.att = attCtx{} // its tests aren't attempts of the curve's last clock
	e.liftedAt, e.lifted = *best, map[[2]int]float64{}
	e.setPhase("trimcheck", "run", "")
	e.log("TRIM CHECK at %s: %d trim(s), each lifted a step at a time while the setting stays clean", best.text(), len(keys))
	lifted := 0
	for _, k := range keys {
		for !e.stopped() {
			cur := e.trimGet()[k]
			if cur >= 0 {
				break
			}
			next := cur + trimStep
			if err := e.trimSet(k[0], k[1], next); err != nil {
				e.trimSet(k[0], k[1], cur) // the file may be written though the live apply failed: put it back
				e.log("b%d.c%d: couldn't lift the trim (%v): it stays at %+.4g MHz", k[0], k[1], err, cur)
				break
			}
			ok, why, st, err := e.test(*best)
			if err == nil && !ok && !e.stopped() && st.FailByChip && !e.trimCheckBlamesChip(k, st.FailChip) {
				// another chip failed (seen: lifting b3.c41 "failed" on b3.c24, and c41 kept its trim): that's no
				// sign this chip needs its trim. Once more; only a second fail puts the trim back.
				e.log("b%d.c%d at %+.4g MHz: the errors were on %s, not this chip or its stage partner: testing the lift once more", k[0], k[1], next, chipName(st.FailChip))
				ok, why, _, err = e.test(*best)
			}
			if err != nil || e.stopped() {
				if uerr := e.trimSet(k[0], k[1], cur); uerr != nil {
					e.log("WARNING: couldn't put b%d.c%d back to %+.4g MHz: %v", k[0], k[1], cur, uerr)
				}
				if e.stopped() {
					e.setPhase("trimcheck", "cut", "stopped")
				} else {
					e.log("ABORT: %v", err)
					e.setPhase("trimcheck", "cut", "safety stop: "+err.Error())
				}
				return
			}
			if !ok {
				if uerr := e.trimSet(k[0], k[1], cur); uerr != nil {
					e.log("WARNING: couldn't put b%d.c%d back to %+.4g MHz: %v", k[0], k[1], cur, uerr)
				}
				e.log("b%d.c%d at %+.4g MHz: not clean (%s): the trim is needed, back to %+.4g MHz", k[0], k[1], next, why, cur)
				break
			}
			lifted++
			if _, ok := e.lifted[k]; !ok {
				e.lifted[k] = cur
			}
			e.mu.Lock()
			if e.liftedTo == nil {
				e.liftedTo = map[[2]int]float64{}
			}
			e.liftedTo[k] = next
			if next == 0 {
				delete(e.runTrims, k)
			} else {
				e.runTrims[k] = next
			}
			kept := copyTrimMap(e.runTrims)
			e.mu.Unlock()
			if e.onTrims != nil {
				e.onTrims(kept)
			}
			e.log("b%d.c%d at %+.4g MHz: still clean, the trim is lifted to %+.4g MHz", k[0], k[1], next, next)
		}
	}
	e.setPhase("trimcheck", "pass", fmt.Sprintf("%d trim(s) checked, %d step(s) lifted", len(keys), lifted))
}

// halfAvg: the average of the second half of the readings (the fan has found its level by then).
func halfAvg(v []int) int {
	h := v[len(v)/2:]
	sum := 0
	for _, x := range h {
		sum += x
	}
	return (sum + len(h)/2) / len(h)
}

// voltOf: the boards' average voltage reading during the last test of (MHz, mV), 0 if unknown.
func (e *tunerEngine) voltOf(mhz, mv int) float64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.testStats[[2]int{mhz, mv}].VoltMV
}

// dropPass: a test at the voltage floor doesn't count as a lower clean setting (it ran at the floor's voltage).
func (e *tunerEngine) dropPass(mhz, mv int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i, p := range e.passes {
		if p.MHz == mhz && p.MV == mv {
			e.passes = append(e.passes[:i], e.passes[i+1:]...)
			return
		}
	}
}

// applyWait applies a plan; while Goldshell's backend doesn't answer it tries again every 15 s, for up to
// backendWait (the run's own settings) or restoreWait (putting the miner back at the end).
// A Stop ends the wait (the run's own settings only: the restore at the end keeps trying).
func (e *tunerEngine) applyWait(p planMV) error {
	return e.retry(e.backendWait, func() error {
		if e.stopped() {
			return errTunerStopped
		}
		return e.applyPlan(p)
	})
}

var (
	errTunerStopped = errors.New("stopped")
	errTunerDead    = errors.New("the run was ended by the watchdog")
)

func (e *tunerEngine) retry(wait time.Duration, f func() error) error {
	err := f()
	if err == nil || wait <= 0 || !strings.Contains(err.Error(), "didn't answer") {
		return err
	}
	e.log("  the miner's backend didn't answer: trying again for up to %g min", wait.Minutes())
	for deadline := e.now().Add(wait); err != nil && err != errTunerStopped && e.now().Before(deadline); {
		e.alive()
		e.sleep(15 * time.Second)
		err = f()
	}
	return err
}

// quietSec: seconds since the heartbeat (0 when not running or not beating yet).
func quietSec(running bool, beat int64) int64 {
	if !running || beat == 0 {
		return 0
	}
	return int64(beatAge(beat).Seconds())
}

// endMsg: the status message, or the watchdog's when it ended the run. Caller may hold e.mu.
func (e *tunerEngine) endMsg() string {
	if p := e.deadWhy.Load(); p != nil {
		return *p
	}
	if e.dead.Load() {
		return "ended by the watchdog (it was stuck)"
	}
	return e.statusMsg
}

// confirmLifted: the trims the trim check lifted at liftedAt, set for the confirm of c: lifted at liftedAt (where
// the check showed they aren't needed), else as c's own clean map test ran with them (the value before the check
// when c has no record). Called for every candidate, so a candidate tried after a slower one gets its lifts back.
func (e *tunerEngine) confirmLifted(c planMV) {
	e.mu.Lock()
	snap, haveSnap := e.trimsAtPass[[2]int{c.MHz, c.MV}]
	e.mu.Unlock()
	cur := e.trimGet()
	keys := make([][2]int, 0, len(e.lifted))
	for k := range e.lifted {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		return keys[i][0] < keys[j][0] || keys[i][0] == keys[j][0] && keys[i][1] < keys[j][1]
	})
	var parts []string
	for _, k := range keys {
		want := e.liftedTo[k]
		if c != e.liftedAt {
			want = e.lifted[k]
			if haveSnap {
				want = snap[k]
			}
		}
		if cur[k] == want {
			continue
		}
		if err := e.trimSet(k[0], k[1], want); err != nil {
			e.log("WARNING: couldn't set b%d.c%d to %+.4g MHz for the confirm: %v", k[0], k[1], want, err)
			continue
		}
		e.mu.Lock()
		if e.runTrims == nil {
			e.runTrims = map[[2]int]float64{}
		}
		if want == 0 {
			delete(e.runTrims, k)
		} else {
			e.runTrims[k] = want
		}
		kept := copyTrimMap(e.runTrims)
		e.mu.Unlock()
		if e.onTrims != nil {
			e.onTrims(kept)
		}
		parts = append(parts, fmt.Sprintf("b%d.c%d %+.4g→%+.4g MHz", k[0], k[1], cur[k], want))
	}
	if len(parts) == 0 {
		return
	}
	if c == e.liftedAt {
		e.log("confirm: %s: the chip trims the trim check lifted here are lifted again: %s", c.text(), strings.Join(parts, ", "))
	} else {
		e.log("confirm: %s: chip trims the trim check lifted at %s are as when %s ran clean in the map: %s", c.text(), e.liftedAt.text(), c.text(), strings.Join(parts, ", "))
	}
}

// trimsNotNeeded: the confirmed setting ran clean in the map before some of the run's trims were made (they
// were made for faster clocks): those come off, back to what that clean test ran with. They'd only cost
// hashrate where the miner is left (seen: a run confirmed 550 MHz, clean in the map with no trims, and kept six
// trims made for 575–700 MHz). The trim check runs before the confirm, at the fastest clean setting.
func (e *tunerEngine) trimsNotNeeded(c planMV) {
	if e.cfg.DryRun {
		return
	}
	e.mu.Lock()
	snap, ok := e.trimsAtPass[[2]int{c.MHz, c.MV}]
	type ch struct {
		k        [2]int
		from, to float64
	}
	var chg []ch
	if ok {
		for k, v := range e.runTrims {
			if w := snap[k]; w != v && (w == 0 || math.Abs(w) < math.Abs(v)) {
				chg = append(chg, ch{k, v, w})
			}
		}
	}
	e.mu.Unlock()
	if len(chg) == 0 {
		return
	}
	sort.Slice(chg, func(i, j int) bool {
		if chg[i].k[0] != chg[j].k[0] {
			return chg[i].k[0] < chg[j].k[0]
		}
		return chg[i].k[1] < chg[j].k[1]
	})
	var names []string
	for _, x := range chg {
		if err := e.trimSet(x.k[0], x.k[1], x.to); err != nil {
			e.log("WARNING: couldn't take b%d.c%d back to %+.4g MHz: %v", x.k[0], x.k[1], x.to, err)
			continue
		}
		e.mu.Lock()
		if x.to == 0 {
			delete(e.runTrims, x.k)
		} else {
			e.runTrims[x.k] = x.to
		}
		e.mu.Unlock()
		names = append(names, fmt.Sprintf("b%d.c%d %+.4g→%+.4g MHz", x.k[0], x.k[1], x.from, x.to))
	}
	if len(names) == 0 {
		return
	}
	e.mu.Lock()
	kept := copyTrimMap(e.runTrims)
	e.mu.Unlock()
	if e.onTrims != nil {
		e.onTrims(kept)
	}
	e.log("TRIMS: %s ran clean in the map without them, so the trims made for faster clocks come off: %s", c.text(), strings.Join(names, ", "))
}

// trimCheckBlames: a failing chip that says something about the trim of chip k: k itself or the other chip of
// its power stage (1+2, 3+4 …: lifting k's trim takes a little voltage from it).
func trimCheckBlames(k [2]int, f [2]int) bool {
	return f[0] == k[0] && (f[1] == k[1] || f[1] == chipPartner(k[1]))
}

// hookChip: the miner's chip k (icinfo's numbering) as the hook's (chain, chip).
func (e *tunerEngine) hookChip(k ckey) ([2]int, bool) {
	st, ok := e.hookStats()
	if !ok {
		return [2]int{}, false
	}
	e.mu.Lock()
	keys := map[ckey]int{}
	for kk := range e.baseRate {
		keys[kk] = 0
	}
	e.mu.Unlock()
	keys[k] = 0
	chain, chip, ok := errToHook(k, minIndexes(keys), st)
	return [2]int{chain, chip}, ok
}

// trimCheckBlamesChip: the miner's failing chip f is the lifted chip k or its stage partner (unknown: it counts).
func (e *tunerEngine) trimCheckBlamesChip(k [2]int, f ckey) bool {
	h, ok := e.hookChip(f)
	return !ok || trimCheckBlames(k, h)
}
