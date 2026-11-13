package main

// Fake-engine harness for the tuner logic sweep: a 4-board × 46-chip miner (chips numbered 1..46 as icinfo
// does), a virtual clock, deterministic per-chip error accrual (rate rules), hashrate/temperature models,
// a miner restart, a chip hook with trims, and an optional replay of a real tuner log. Every plan, fan and
// trim the engine sends is checked against the hardware spec (PV 9100–9600, mV 8800–9300, PV = mV + 300,
// clocks on the 12.5 MHz grid, trims 0..-50 in 12.5 steps).

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

type fmRule func(f *fakeMiner, k ckey) float64 // extra errors per minute for chip k right now (f.mu held)

type fakeMiner struct {
	mu         sync.Mutex
	t          *testing.T
	e          *tunerEngine
	vc, t0, up time.Time
	boards     []int // boards the miner reports chips for
	hook       []int // chains the chip hook reports
	cnt        map[ckey]float64
	plan       planMV
	applied    []planMV
	applyAt    time.Time
	trims      map[[2]int]float64
	trimCalls  []string
	rules      []fmRule
	baseNoise  float64 // errors/min per chip (scaled by clock)
	ghsMul     func(f *fakeMiner) float64
	temp       func(f *fakeMiner) (float64, bool)
	onTick     func(f *fakeMiner) // each sleep, f.mu held
	stopWhen   func(f *fakeMiner) bool
	stopFired  bool
	fans       []int
	violations []string
	// replay
	replay   func(p planMV, nth int) *replaySeg
	seen     map[planMV]int
	seg      *replaySeg
	segWatch bool
	watchAt  time.Time
}

func (f *fakeMiner) phase() string {
	f.e.mu.Lock()
	defer f.e.mu.Unlock()
	return f.e.phase
}

func (f *fakeMiner) trimOf(k ckey) float64 { return f.trims[[2]int{k.B, k.I}] }
func (f *fakeMiner) sinceApply() float64   { return f.vc.Sub(f.applyAt).Minutes() }
func (f *fakeMiner) restart() { // the mining program restarts: uptime and every counter start over
	f.up = f.vc
	for k := range f.cnt {
		f.cnt[k] = 0
	}
}

func (f *fakeMiner) present(b int) bool {
	for _, x := range f.boards {
		if x == b {
			return true
		}
	}
	return false
}

func (f *fakeMiner) violate(format string, a ...any) {
	f.violations = append(f.violations, fmt.Sprintf(format, a...))
}

func scfg() tunerConfig {
	c := defaultTunerConfig()
	c.TopDown = true // written for the old (top-down) map walk
	c.Mode, c.DryRun = "map", false
	c.BaselineSrc, c.BaselineMHz, c.BaselineMV, c.BaselineMin = "own", 615, 9100, 25
	c.LowestMHz, c.HighestMHz, c.Step = 550, 700, 25
	c.MinMV, c.MaxMV, c.MVStep, c.PVGap = 8800, 9300, 50, 300
	c.HoldMin, c.SettleMin, c.PollS = 25, 3, 15
	c.ChipTrim, c.ChipTrimMax = true, 3
	c.ConfirmWin, c.ConfirmX = true, 2
	c.FanHold, c.FanTargetC, c.FanMinPct = true, 70, 30
	c.BuildPresets = false
	c.AbortC = 88
	return c
}

// newFM builds an engine on a fake miner with 4 boards / 46 chips, all quiet unless rules say otherwise.
func newFM(t *testing.T, cfg tunerConfig) (*tunerEngine, *fakeMiner) {
	t.Helper()
	if err := validateTunerConfig(cfg); err != nil {
		t.Fatalf("scenario config invalid: %v", err)
	}
	dir := t.TempDir()
	oldPath := tunerCurvePath
	tunerCurvePath = func() string { return filepath.Join(dir, "tuner-curve.json") }
	t.Cleanup(func() { tunerCurvePath = oldPath })
	lastCurveMu.Lock()
	lastCurve = nil
	lastCurveMu.Unlock()

	t0 := time.Date(2026, 11, 5, 1, 53, 0, 0, time.UTC)
	f := &fakeMiner{t: t, vc: t0, t0: t0, up: t0.Add(-time.Hour), boards: []int{0, 1, 2, 3}, hook: []int{0, 1, 2, 3},
		cnt: map[ckey]float64{}, trims: map[[2]int]float64{}, seen: map[planMV]int{},
		plan: planMV{MHz: cfg.BaselineMHz, MV: cfg.BaselineMV, PV: cfg.BaselineMV + cfg.PVGap}}
	for b := 0; b < 4; b++ {
		for i := 1; i <= 46; i++ {
			// a fixed fractional start per chip, so slow error rates cross whole errors at different times
			f.cnt[ckey{b, i}] = math.Mod(float64(b*46+i)*0.618034, 1) * 0.98
		}
	}
	e := newTunerEngine(cfg)
	f.e = e
	e.before, e.verifyPlan, e.miningStops, e.fanFloor, e.fanGuard = nil, nil, nil, nil, nil
	e.readVolt, e.readWork = nil, nil
	e.emit = func(string) {}
	e.now = func() time.Time { f.mu.Lock(); defer f.mu.Unlock(); return f.vc }
	e.sleep = func(d time.Duration) {
		f.mu.Lock()
		f.vc = f.vc.Add(d)
		if f.seg == nil {
			scale := pll(float64(f.plan.MHz)) / pll(615)
			for k := range f.cnt {
				if !f.present(k.B) {
					continue
				}
				r := f.baseNoise * scale
				for _, rule := range f.rules {
					r += rule(f, k)
				}
				f.cnt[k] += r * d.Minutes()
			}
		}
		if f.onTick != nil {
			f.onTick(f)
		}
		sw := f.stopWhen
		f.mu.Unlock()
		if sw != nil && !f.stopFired && sw(f) {
			f.stopFired = true
			e.Stop()
		}
	}
	e.readErrors = func() (map[ckey]int, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var add map[ckey]int
		if f.seg != nil {
			if !f.segWatch {
				f.segWatch, f.watchAt = true, f.vc
			}
			add, _, _ = f.seg.at(f.vc.Sub(f.watchAt).Minutes())
		}
		out := map[ckey]int{}
		for k, v := range f.cnt {
			if f.present(k.B) {
				out[k] = int(math.Floor(v)) + add[k]
			}
		}
		return out, nil
	}
	e.readGHS = func() (float64, bool) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.seg != nil && f.segWatch {
			_, g, _ := f.seg.at(f.vc.Sub(f.watchAt).Minutes())
			return g, true
		}
		g := 4470 * pll(float64(f.plan.MHz)) / pll(615) * float64(len(f.boards)) / 4
		g *= 1 + 0.02*math.Sin(f.vc.Sub(f.t0).Minutes()/3)
		if f.ghsMul != nil {
			g *= f.ghsMul(f)
		}
		return g, true
	}
	e.readTemp = func() (float64, bool) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.temp != nil {
			return f.temp(f)
		}
		if f.seg != nil && f.segWatch {
			_, _, mt := f.seg.at(f.vc.Sub(f.watchAt).Minutes())
			if mt > 0 {
				return mt, true
			}
		}
		return 70, true
	}
	e.elapsed = func() (int, bool) {
		f.mu.Lock()
		defer f.mu.Unlock()
		return 100000 + int(f.vc.Sub(f.up).Seconds()), true
	}
	e.applyPlan = func(p planMV) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		if p.PV < 9100 || p.PV > 9600 {
			f.violate("PV %d sent (%s)", p.PV, p.text())
		}
		if p.MV < 8800 || p.MV > 9300 {
			f.violate("mV %d sent (%s)", p.MV, p.text())
		}
		if p.PV != p.MV+f.e.cfg.PVGap && p.PV != 9600 {
			f.violate("PV %d isn't mV %d + gap", p.PV, p.MV)
		}
		if r := pll(float64(p.MHz)); float64(p.MHz)-r >= 1 && p.MHz != f.e.cfg.BaselineMHz { // (the baseline is the miner's own)
			f.violate("clock %d MHz runs %g: not the request for a 12.5 MHz step", p.MHz, r)
		}
		if f.seg != nil && f.segWatch { // the replayed test is over: its errors stay on the counters
			add, _, _ := f.seg.at(f.vc.Sub(f.watchAt).Minutes())
			for k, v := range add {
				f.cnt[k] += float64(v)
			}
		}
		f.seg, f.segWatch = nil, false
		if f.replay != nil {
			f.seg = f.replay(p, f.seen[p])
		}
		f.seen[p]++
		f.plan, f.applyAt = p, f.vc
		f.applied = append(f.applied, p)
		return nil
	}
	e.applyFan = func(p int) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		if p < cfg.FanMinPct || p > 100 {
			f.violate("fan %d %%", p)
		}
		f.fans = append(f.fans, p)
		return nil
	}
	e.readFan = func() (int, bool) { return 40, true }
	e.trimGet = func() map[[2]int]float64 {
		f.mu.Lock()
		defer f.mu.Unlock()
		m := map[[2]int]float64{}
		for k, v := range f.trims {
			m[k] = v
		}
		return m
	}
	e.trimSet = func(chain, chip int, mhz float64) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		if mhz > 0 || mhz < cwFloor || !onPLLGrid(mhz) {
			f.violate("trim %g MHz on %d.%d", mhz, chain, chip)
		}
		ok := false
		for _, c := range f.hook {
			ok = ok || c == chain
		}
		if !ok || chip < 1 || chip > 46 {
			f.violate("trim on chain %d chip %d, which the hook doesn't have", chain, chip)
		}
		f.trimCalls = append(f.trimCalls, fmt.Sprintf("%d.%d=%g", chain, chip, mhz))
		if mhz == 0 {
			delete(f.trims, [2]int{chain, chip})
		} else {
			f.trims[[2]int{chain, chip}] = mhz
		}
		return nil
	}
	e.hookStats = func() (chipStatsFile, bool) {
		var s chipStatsFile
		for _, c := range f.hook {
			s.Chains = append(s.Chains, struct {
				Chain   int   `json:"chain"`
				Chips   int   `json:"chips"`
				Base10  int   `json:"base10"`
				Clock10 []int `json:"clock10"`
				Nonces  []int `json:"nonces"`
			}{Chain: c, Chips: 46, Base10: 6150})
		}
		return s, true
	}
	e.adoptTrims = func([]chipTrim, string) {}
	return e, f
}

// runFM runs the engine and fails the test if it doesn't end (a hang) or broke a hardware invariant.
func runFM(t *testing.T, e *tunerEngine, f *fakeMiner) {
	t.Helper()
	done := make(chan struct{})
	go func() { e.Run(); close(done) }()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		e.mu.Lock()
		l := e.logbuf
		if len(l) > 15 {
			l = l[len(l)-15:]
		}
		e.mu.Unlock()
		t.Fatalf("Run didn't end (hang): last log\n%s", strings.Join(l, "\n"))
	}
	if e.isRunning() {
		t.Fatal("still running after Run returned")
	}
	if len(f.violations) > 0 {
		t.Fatalf("hardware spec violated:\n%s", strings.Join(f.violations, "\n"))
	}
}

// --- rules ---

// needRule: chip k makes rate errors/min while the plan's mV is under need[MHz] (a clock missing from need:
// never; need > 9300: always), unless it's trimmed at least trimFix MHz (0: trims don't help).
func needRule(k ckey, need map[int]int, rate, trimFix float64) fmRule {
	return func(f *fakeMiner, c ckey) float64 {
		if c != k {
			return 0
		}
		n, ok := need[f.plan.MHz]
		if !ok || f.plan.MV >= n {
			return 0
		}
		if trimFix < 0 && f.trimOf(k) <= trimFix+1e-9 {
			return 0
		}
		return rate
	}
}

// boardRule: every chip of board b makes rate errors/min while mV < need[MHz] (a board-wide fail, no single chip).
func boardRule(b int, need map[int]int, rate float64) fmRule {
	return func(f *fakeMiner, c ckey) float64 {
		if c.B != b {
			return 0
		}
		if n, ok := need[f.plan.MHz]; ok && f.plan.MV < n {
			return rate
		}
		return 0
	}
}

// --- result helpers ---

func resultsStr(e *tunerEngine) []string {
	var out []string
	for _, r := range e.results {
		out = append(out, fmt.Sprintf("%d/%d %s", r.MHz, r.PV, r.Result))
	}
	return out
}

func logStr(e *tunerEngine) string { return strings.Join(e.fullLog, "\n") }

func lastApplied(f *fakeMiner) planMV { return f.applied[len(f.applied)-1] }

func curveStr(c []planMV) string {
	var s []string
	for _, p := range c {
		s = append(s, fmt.Sprintf("%d/%d", p.MHz, p.PV))
	}
	return strings.Join(s, " ")
}

func savedCurveNow() savedCurve { return loadSavedCurve() }

func sortedTrims(m map[[2]int]float64) string {
	var s []string
	for k, v := range m {
		s = append(s, fmt.Sprintf("%d.%d=%g", k[0], k[1], v))
	}
	sort.Strings(s)
	return strings.Join(s, ",")
}

func mustContain(t *testing.T, e *tunerEngine, subs ...string) {
	t.Helper()
	l := logStr(e)
	for _, s := range subs {
		if !strings.Contains(l, s) {
			t.Fatalf("log lacks %q:\n%s", s, tailLog(e, 60))
		}
	}
}

func tailLog(e *tunerEngine, n int) string {
	var keep []string
	for _, l := range e.fullLog {
		if strings.Contains(l, " test ") && strings.Contains(l, "/25min") || strings.Contains(l, "/50min ") || strings.Contains(l, " baseline ") && strings.Contains(l, "min ") {
			continue // per-poll lines
		}
		keep = append(keep, l)
	}
	if len(keep) > n {
		keep = keep[len(keep)-n:]
	}
	return strings.Join(keep, "\n")
}

func planState(e *tunerEngine, key string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if s := e.stepByKey(key); s != nil {
		return s.State + ": " + s.Note
	}
	return "(none)"
}

// dbgDump: LOGIC_DUMP=1 prints a run.
func dbgDump(t *testing.T, e *tunerEngine) {
	t.Log("\n" + tailLog(e, 400))
	for _, r := range e.results {
		t.Logf("%s %d/%d %s %.1f %s", r.Time, r.MHz, r.PV, r.Result, r.MinS, r.Reason)
	}
}

func dump(t *testing.T, e *tunerEngine) {
	if os.Getenv("LOGIC_DUMP") != "" {
		dbgDump(t, e)
	}
}
