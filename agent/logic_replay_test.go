package main

// Replays the owner's real v1.5.15 run (SCLite-1, 2026-11-05 01:53: map 550–700 step 25, PV 9100–9600 step
// 50, baseline 615 MHz / PV 9400) through the v1.5.16 engine with their real per-poll errors, hashrate and
// temperature, and checks the engine makes the same decisions — then carries on past the point where v1.5.15
// deadlocked (575 MHz / PV 9100: b3.c32 +4, limit 3, at 3.9 min → slow b3.c32 12.5 MHz).

import (
	"bufio"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

type replayPoll struct {
	min, ghs, maxT float64
	total          int
	top            map[ckey]int
}

type replaySeg struct {
	name  string
	polls []replayPoll
	// filled in by build(): cumulative per-chip counts at each poll
	counts []map[ckey]int
	final  map[ckey]int // when set: counts grow linearly to these (the baseline)
}

var pollRe = regexp.MustCompile(`^\S+\s+(baseline|test) ([\d.]+)/\S+min\s+([\d.]+) GH/s\s+maxT=([\d.]+)\s+all chips \+(\d+)\s+top: (.*)$`)
var chipRe = regexp.MustCompile(`b(\d+)\.c(\d+) \+(\d+)`)

func parseReplay(t *testing.T, path string) []*replaySeg {
	fh, err := os.Open(path)
	if err != nil {
		t.Skipf("no replay data: %v", err)
	}
	defer fh.Close()
	var segs []*replaySeg
	sc := bufio.NewScanner(fh)
	for sc.Scan() {
		l := sc.Text()
		if i := strings.Index(l, "  BASELINE "); i > 0 {
			segs = append(segs, &replaySeg{name: l[i+2:]})
			continue
		}
		if i := strings.Index(l, "  TEST "); i > 0 {
			segs = append(segs, &replaySeg{name: l[i+2:]})
			continue
		}
		m := pollRe.FindStringSubmatch(l)
		if m == nil || len(segs) == 0 {
			continue
		}
		p := replayPoll{top: map[ckey]int{}}
		p.min, _ = strconv.ParseFloat(m[2], 64)
		p.ghs, _ = strconv.ParseFloat(m[3], 64)
		p.maxT, _ = strconv.ParseFloat(m[4], 64)
		p.total, _ = strconv.Atoi(m[5])
		for _, c := range chipRe.FindAllStringSubmatch(m[6], -1) {
			b, _ := strconv.Atoi(c[1])
			i, _ := strconv.Atoi(c[2])
			v, _ := strconv.Atoi(c[3])
			p.top[ckey{b, i}] = v
		}
		s := segs[len(segs)-1]
		s.polls = append(s.polls, p)
	}
	return segs
}

// build turns the top-3 lists into per-chip counts: a chip shown is at least what it last showed; what the
// total has beyond the shown chips goes, one error each, to chips never shown anywhere in the run (spread
// over the boards), so totals match and no hidden chip gets near a limit.
func (s *replaySeg) build(fillers []ckey) {
	known := map[ckey]int{}
	nf := 0
	for _, p := range s.polls {
		for k, v := range p.top {
			if v > known[k] {
				known[k] = v
			}
		}
		sum := 0
		for _, v := range known {
			sum += v
		}
		if r := p.total - sum; r > nf {
			nf = r
		}
		c := map[ckey]int{}
		for k, v := range known {
			c[k] = v
		}
		for i := 0; i < nf && i < len(fillers); i++ {
			c[fillers[i]]++
		}
		s.counts = append(s.counts, c)
	}
}

// at: the counts, hashrate and hottest board the real run read at this point of the test.
func (s *replaySeg) at(min float64) (map[ckey]int, float64, float64) {
	if len(s.polls) == 0 {
		return nil, 0, 0
	}
	i := sort.Search(len(s.polls), func(i int) bool { return s.polls[i].min > min+0.051 }) - 1
	if i < 0 {
		return map[ckey]int{}, s.polls[0].ghs, s.polls[0].maxT
	}
	if s.final != nil {
		last := s.polls[len(s.polls)-1].min
		c := map[ckey]int{}
		for k, v := range s.final {
			c[k] = int(float64(v) * min / last)
			if min >= last-0.2 {
				c[k] = v
			}
		}
		return c, s.polls[i].ghs, s.polls[i].maxT
	}
	return s.counts[i], s.polls[i].ghs, s.polls[i].maxT
}

func TestLogicReplayOwnerRun(t *testing.T) {
	segs := parseReplay(t, "testdata/tf1-tuner-log.txt")
	if len(segs) != 9 {
		t.Fatalf("expected baseline + 8 tests in the log, got %d", len(segs))
	}
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
	// the baseline's per-chip counts, recovered exactly from the limits in the run's chip-data.csv (32 in all)
	segs[0].final = map[ckey]int{{0, 2}: 2, {0, 8}: 3, {1, 13}: 4, {1, 29}: 5, {1, 37}: 1, {2, 25}: 3, {2, 37}: 5,
		{3, 10}: 1, {3, 14}: 2, {3, 25}: 6}
	byPlan := map[planMV]*replaySeg{}
	order := []planMV{{615, 9100, 9400}, {550, 9100, 9400}, {550, 9050, 9350}, {550, 9000, 9300}, {550, 8950, 9250},
		{550, 8900, 9200}, {550, 8850, 9150}, {550, 8800, 9100}, {575, 8800, 9100}}
	for i, p := range order {
		byPlan[p] = segs[i]
	}

	cfg := scfg()
	e, f := newFM(t, cfg)
	f.replay = func(p planMV, nth int) *replaySeg {
		if nth == 0 {
			return byPlan[p]
		}
		return nil
	}
	// past the log (synthetic): b3.c32 is bad at 575+ unless slowed 12.5; faster clocks need more voltage;
	// 700 isn't clean even at PV 9600
	need := map[int]int{600: 8850, 625: 8950, 650: 9100, 675: 9300, 700: 9999}
	c32 := map[int]int{575: 9999, 600: 9999, 625: 9999, 650: 9999, 675: 9999, 700: 9999}
	f.rules = []fmRule{needRule(ckey{3, 32}, c32, 0.6, -12.5), needRule(ckey{1, 20}, need, 0.6, 0), needRule(ckey{2, 9}, need, 0.6, 0)}
	// the real trimSet must return (v1.5.15 deadlocked here); on this host it can't reach a hook, so its
	// error is expected; the fake hook then takes the trim
	realSet := newTunerEngine(tunerConfig{}).trimSet
	fakeSet := e.trimSet
	realCalls := 0
	e.trimSet = func(chain, chip int, mhz float64) error {
		done := make(chan struct{})
		go func() { realSet(chain, chip, mhz); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("real trimSet(%d,%d,%g) didn't return: the v1.5.15 deadlock", chain, chip, mhz)
		}
		realCalls++
		return fakeSet(chain, chip, mhz)
	}
	runFM(t, e, f)
	if os.Getenv("LOGIC_DUMP") != "" {
		dbgDump(t, e)
	}

	got := resultsStr(e)
	wantStart := []string{"550/9400 clean", "550/9350 clean", "550/9300 clean", "550/9250 clean", "550/9200 clean",
		"550/9150 clean", "550/9100 clean", "575/9100 failed"}
	if len(got) < len(wantStart) || strings.Join(got[:len(wantStart)], ", ") != strings.Join(wantStart, ", ") {
		t.Fatalf("the replayed part decided differently from the real run:\n got %v\nwant %v...", got, wantStart)
	}
	r := e.results[7]
	if r.Reason != "chip b3.c32 +4 errors (limit 3.0)" || r.MinS < 3.5 || r.MinS > 4.5 || r.ErrAdd != 13 {
		t.Fatalf("575/9100 should fail like the real run (b3.c32 +4, limit 3.0, 3.9 min, +13): %+v", r)
	}
	// the real numbers' hashrate passes its check at every 550 step (4.0–4.1 TH/s vs 4.0 expected)
	mustContain(t, e, "575 MHz / PV 9100 (mV 8800) failed (chip b3.c32 +4 errors (limit 3.0)): slowing b3.c32 to -12.5 MHz, testing again",
		"575 MHz / PV 9100 (mV 8800) is clean with chip trims: they stay (b3.c32 -12.5 MHz)")
	if realCalls == 0 {
		t.Fatal("trimSet never called")
	}
	if c := curveStr(e.curve); c != "550/9100 575/9100 600/9150 625/9250 650/9400 675/9600" {
		t.Fatalf("curve %s\n%s", c, tailLog(e, 80))
	}
	// 700 isn't clean at PV 9600: tried up to 9600 and never past it
	for _, p := range f.applied {
		if p.MHz == 700 && p.PV > 9600 {
			t.Fatalf("PV past 9600: %v", p)
		}
	}
	// the trim check keeps b3.c32 (it's needed at 675); the confirm passes; the run ends on it
	if e.best == nil || *e.best != (planMV{675, 9300, 9600}) || !e.confirmed {
		t.Fatalf("best %v confirmed %v\n%s", e.best, e.confirmed, tailLog(e, 60))
	}
	if lastApplied(f) != (planMV{675, 9300, 9600}) {
		t.Fatalf("not left on the confirmed best: %v", lastApplied(f))
	}
	if sortedTrims(f.trims) != "3.32=-12.5" || sortedTrims(e.runTrims) != "3.32=-12.5" {
		t.Fatalf("trims %v run %v", f.trims, e.runTrims)
	}
	sc := savedCurveNow()
	if len(sc.Curve) != 6 || sc.Confirmed == nil || *sc.Confirmed != (planMV{675, 9300, 9600}) {
		t.Fatalf("saved curve %+v", sc)
	}
}
