package main

// Round 3: the bottom-up map walk against the old (top-down) walk: time, tests, curves, edge cases.

import (
	"fmt"
	"math"
	"math/rand"
	"os"
	"strings"
	"testing"
	"time"
)

type walkStats struct {
	tests    int     // tests run while the curve phase ran (trim retests and retests included)
	fails    int     // of those, failed
	mapH     float64 // virtual hours of the curve phase
	totalH   float64 // virtual hours of the whole run
	curve    string
	walk     string
	best     string
	trims    string
	marginal int
}

// instrument: count the curve phase's tests and virtual time.
func instrument(e *tunerEngine, f *fakeMiner) func() walkStats {
	var mapDur time.Duration
	n0, n1 := -1, -1
	orig := e.sleep
	e.sleep = func(d time.Duration) {
		e.mu.Lock()
		st := ""
		if s := e.stepByKey("curve"); s != nil {
			st = s.State
		}
		nres := len(e.results)
		e.mu.Unlock()
		if st == "run" {
			if n0 < 0 {
				n0 = nres
			}
			n1 = -1
			mapDur += d
		} else if n0 >= 0 && n1 < 0 {
			n1 = nres
		}
		orig(d)
	}
	return func() walkStats {
		var w walkStats
		if n1 < 0 {
			n1 = len(e.results)
		}
		var walk []string
		for _, r := range e.results[max(n0, 0):n1] {
			w.tests++
			if r.Result != "clean" {
				w.fails++
			}
			walk = append(walk, r.Result[:1]+itoa(r.MHz)+"/"+itoa(r.PV))
		}
		w.walk = strings.Join(walk, " ")
		w.mapH = mapDur.Hours()
		w.totalH = f.vc.Sub(f.t0).Hours()
		w.curve = curveStr(e.curve)
		if e.best != nil {
			w.best = fmt.Sprintf("%d/%d", e.best.MHz, e.best.PV)
			if e.confirmed {
				w.best += " confirmed"
			}
		}
		w.trims = sortedTrims(e.runTrims)
		w.marginal = len(e.marginal)
		return w
	}
}

func (w walkStats) String() string {
	return fmt.Sprintf("map: %d tests (%d failed), %.1f h; whole run %.1f h; curve %s; best %s; trims [%s]", w.tests, w.fails, w.mapH, w.totalH, w.curve, w.best, w.trims)
}

// ---------------------------------------------------------------- the owner's run, both walks

func ownerReplay(t *testing.T, topDown bool) walkStats {
	segs := parseReplay(t, "testdata/tf1-tuner-log.txt")
	shown := map[ckey]bool{{3, 32}: true}
	for _, s := range segs {
		for _, p := range s.polls {
			for k := range p.top {
				shown[k] = true
			}
		}
	}
	var fillers []ckey
	for i := 1; i <= 46; i++ {
		for b := 0; b < 4; b++ {
			if k := (ckey{b, i}); !shown[k] {
				fillers = append(fillers, k)
			}
		}
	}
	for _, s := range segs {
		s.build(fillers)
	}
	segs[0].final = map[ckey]int{{0, 2}: 2, {0, 8}: 3, {1, 13}: 4, {1, 29}: 5, {1, 37}: 1, {2, 25}: 3, {2, 37}: 5,
		{3, 10}: 1, {3, 14}: 2, {3, 25}: 6}
	byPlan := map[planMV]*replaySeg{}
	order := []planMV{{615, 9100, 9400}, {550, 9100, 9400}, {550, 9050, 9350}, {550, 9000, 9300}, {550, 8950, 9250},
		{550, 8900, 9200}, {550, 8850, 9150}, {550, 8800, 9100}, {575, 8800, 9100}}
	for i, p := range order {
		byPlan[p] = segs[i]
	}
	cfg := scfg()
	cfg.TopDown = topDown
	e, f := newFM(t, cfg)
	f.replay = func(p planMV, nth int) *replaySeg {
		if nth == 0 {
			return byPlan[p]
		}
		return nil
	}
	need := map[int]int{600: 8850, 625: 8950, 650: 9100, 675: 9300, 700: 9999}
	c32 := map[int]int{575: 9999, 600: 9999, 625: 9999, 650: 9999, 675: 9999, 700: 9999}
	f.rules = []fmRule{needRule(ckey{3, 32}, c32, 0.6, -12.5), needRule(ckey{1, 20}, need, 0.6, 0), needRule(ckey{2, 9}, need, 0.6, 0)}
	get := instrument(e, f)
	runFM(t, e, f)
	dump(t, e)
	return get()
}

func TestR3WalkOwnerReplayBoth(t *testing.T) {
	td := ownerReplay(t, true)
	bu := ownerReplay(t, false)
	t.Logf("top-down : %s", td)
	t.Logf("bottom-up: %s", bu)
	t.Logf("top-down walk : %s", td.walk)
	t.Logf("bottom-up walk: %s", bu.walk)
	if td.curve != bu.curve {
		t.Fatalf("the walks found different curves: %s vs %s", td.curve, bu.curve)
	}
	if bu.tests >= td.tests || bu.mapH >= td.mapH {
		t.Fatalf("bottom-up isn't cheaper: %v vs %v", bu, td)
	}
	if td.best != bu.best || td.trims != bu.trims {
		t.Fatalf("different end: %v vs %v", td, bu)
	}
}

// ---------------------------------------------------------------- synthetic boards, both walks

func synth(t *testing.T, topDown bool, setup func(c *tunerConfig, f *fakeMiner)) walkStats {
	c := scfg()
	c.TopDown = topDown
	e, f := newFM(t, c)
	if setup != nil {
		setup(&e.cfg, f)
	}
	get := instrument(e, f)
	runFM(t, e, f)
	dump(t, e)
	return get()
}

func TestR3WalkSynthetic(t *testing.T) {
	cases := []struct {
		name  string
		setup func(c *tunerConfig, f *fakeMiner)
	}{
		{"all clean at PV 9100", nil},
		{"+50 mV per clock (board-wide)", func(c *tunerConfig, f *fakeMiner) {
			// 550 → 8800, 575 → 8850, … 700 → 9100: every clock one step more than the last
			need := map[int]int{}
			for i, m := range c.clockList() {
				need[m] = 8800 + 50*i + 1 // +1: mV must reach 8800+50i
			}
			f.rules = []fmRule{boardRule(2, need, 0.5)}
		}},
		{"+50 mV per clock (one chip, trims help 12.5)", func(c *tunerConfig, f *fakeMiner) {
			need := map[int]int{}
			for i, m := range c.clockList() {
				need[m] = 8800 + 50*i + 1
			}
			f.rules = []fmRule{needRule(ckey{2, 7}, need, 0.5, 0)}
		}},
	}
	for _, tc := range cases {
		td := synth(t, true, tc.setup)
		bu := synth(t, false, tc.setup)
		t.Logf("%s\n  top-down : %s\n  bottom-up: %s", tc.name, td, bu)
		if td.curve != bu.curve {
			t.Errorf("%s: different curves %s vs %s", tc.name, td.curve, bu.curve)
		}
	}
}

// ---------------------------------------------------------------- noisy borderline chips: Monte Carlo

// poisson: a Poisson draw (small means).
func poisson(r *rand.Rand, lam float64) int {
	if lam <= 0 {
		return 0
	}
	if lam > 30 {
		return int(math.Max(0, math.Round(lam+math.Sqrt(lam)*r.NormFloat64())))
	}
	L, k, p := math.Exp(-lam), 0, 1.0
	for {
		p *= r.Float64()
		if p <= L {
			return k
		}
		k++
	}
}

// borderline: chips whose errors per 25-min test fall by perStep for every 50 mV above what the clock needs
// (lamAt errors per test at the needed voltage): random, so a test near the edge passes or fails by luck.
type bChip struct {
	k    ckey
	need map[int]int // mV the chip needs per clock
}

func borderlineTick(r *rand.Rand, chips []bChip, lamAt, perStep float64, trimHelps bool) func(f *fakeMiner) {
	last := time.Time{}
	return func(f *fakeMiner) {
		if last.IsZero() {
			last = f.vc
			return
		}
		d := f.vc.Sub(last).Minutes()
		last = f.vc
		if f.seg != nil {
			return
		}
		for _, c := range chips {
			n, ok := c.need[f.plan.MHz]
			if !ok {
				continue
			}
			steps := float64(n-f.plan.MV) / 50 // >0: under what it needs
			if trimHelps {
				steps -= -f.trimOf(c.k) / 25 // each 12.5 MHz of trim is worth half a voltage step
			}
			lam := lamAt * math.Pow(perStep, steps) / 25 // per minute
			f.cnt[c.k] += float64(poisson(r, lam*d))
		}
	}
}

type mcOut struct {
	high, low, exact int // clock entries one step (or more) above / below the true lowest, or exact
	tests            int
	mapH             float64
	short            int // runs whose curve ended early (a clock not clean at PV 9600 by luck)
	bestMHz          map[int]int
	trims            int
}

func mcRun(t *testing.T, topDown, trims bool, seeds int, lamAt, perStep float64) mcOut {
	out := mcOut{bestMHz: map[int]int{}}
	for s := 0; s < seeds; s++ {
		r := rand.New(rand.NewSource(int64(1000 + s)))
		c := scfg()
		c.TopDown = topDown
		c.ChipTrim = trims
		c.LowestMHz, c.HighestMHz = 550, 675
		e, f := newFM(t, c)
		// three chips on different boards; truth: 550 8800, 575 8850, 600 8900, 625 8950, 650 9050, 675 9150
		truth := map[int]int{550: 8800, 575: 8850, 600: 8900, 625: 8950, 650: 9050, 675: 9150}
		var chips []bChip
		for i, k := range []ckey{{0, 11}, {1, 22}, {3, 33}} {
			need := map[int]int{}
			for m, v := range truth {
				need[m] = v - 50*(i%2) // two chips need the truth, one a step less
			}
			chips = append(chips, bChip{k, need})
		}
		f.onTick = borderlineTick(r, chips, lamAt, perStep, true)
		get := instrument(e, f)
		runFM(t, e, f)
		w := get()
		out.tests += w.tests
		out.mapH += w.mapH
		if len(e.curve) < len(truth) {
			out.short++
		}
		for _, p := range e.curve {
			switch d := p.MV - truth[p.MHz]; {
			case d > 0:
				out.high++
			case d < 0:
				out.low++
			default:
				out.exact++
			}
		}
		if e.best != nil {
			out.bestMHz[e.best.MHz]++
			if os.Getenv("MC_DEBUG") != "" && e.best.MHz == 550 {
				t.Logf("seed %d topDown=%v trims=%v best 550: curve %s marginal %v\n%s", s, topDown, trims, w.curve, e.marginal, tailLog(e, 70))
				os.Setenv("MC_DEBUG", "")
			}
		}
		out.trims += len(e.runTrims)
	}
	return out
}

// MC_SEEDS=n runs it (it takes a while); off by default.
func TestR3WalkBorderlineMC(t *testing.T) {
	n := 0
	fmt.Sscan(os.Getenv("MC_SEEDS"), &n)
	if n == 0 {
		t.Skip("MC_SEEDS not set")
	}
	for _, lam := range []struct{ at, per float64 }{{0.5, 6}, {1.0, 4}, {1.5, 3}} {
		for _, trims := range []bool{false, true} {
			td := mcRun(t, true, trims, n, lam.at, lam.per)
			bu := mcRun(t, false, trims, n, lam.at, lam.per)
			for _, x := range []struct {
				n string
				o mcOut
			}{{"top-down ", td}, {"bottom-up", bu}} {
				o := x.o
				tot := o.high + o.low + o.exact
				t.Logf("λ@need=%.1f ×%g/step trims=%v %s: clocks high %d (%.1f%%) low %d exact %d of %d; short curves %d/%d; tests/run %.1f; map h/run %.1f; trims/run %.2f; best %v",
					lam.at, lam.per, trims, x.n, o.high, 100*float64(o.high)/float64(max(tot, 1)), o.low, o.exact, tot, o.short, n,
					float64(o.tests)/float64(n), o.mapH/float64(n), float64(o.trims)/float64(n), o.bestMHz)
			}
		}
	}
}
