package main

// Monte-Carlo of the strong-chip test through the real state machine (the sctFake harness), with the owner's
// real error rates (tf1/tuner/SCLite-1-20261105-0153: ~27 errors per 25 min over 184 chips, about half from one
// or two "hot" chips per board): per board one chip at 10/h, three at 1/h, the other 42 at 0.08/h (≈16/h a
// board; the board's mean chip ≈ 0.36/h, 0.12 per 20 min).
//
// Run: SCT_SIM=400 go test -run TestStrongSim -v .   (runs per scenario)

import (
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"testing"
	"time"
)

const simRef = 16.4 / 46 // the board's mean chip, errors per hour

type simTruth struct {
	r0   float64 // errors/h at the board clock
	bad  bool
	from int     // the first step (1..4) at which it's worse
	m    float64 // worse = m × the board's mean chip
}

type simTally struct {
	runs, guards, aborted           int
	fine, fineShort, fineShortGuard int
	bad, missed                     int
	med, medShort                   int // 1/h chips that slipped in as "clean"
	candRuns, candWithMed           int
	minutes                         float64
}

func simBoard(rng *rand.Rand) []float64 {
	r := make([]float64, 47)
	perm := rng.Perm(46)
	for k, p := range perm {
		c := p + 1
		switch {
		case k == 0:
			r[c] = 10
		case k <= 3:
			r[c] = 1
		default:
			r[c] = 0.08
		}
	}
	return r
}

func simOne(t *testing.T, seed int64, m float64, fracBad float64, tally *simTally, base, step int) {
	f := sctSetup(t)
	rng := rand.New(rand.NewSource(seed))
	f.rng = rand.New(rand.NewSource(seed * 7919))
	r := simBoard(rng)
	truth := make([]simTruth, 47)
	for c := 1; c <= 46; c++ {
		truth[c] = simTruth{r0: r[c]}
		if m > 0 && rng.Float64() < fracBad {
			truth[c].bad, truth[c].from, truth[c].m = true, 1+rng.Intn(4), m
		}
	}
	partnerMode := os.Getenv("SCT_PARTNER") != ""
	f.errRate = func(chip int, trim float64, trims map[int]float64) float64 {
		tr := truth[chip]
		if partnerMode { // the raised chip stays clean; the other chip of its stage gets worse
			p := chip + 1
			if chip%2 == 0 {
				p = chip - 1
			}
			if p >= 1 && p <= 46 {
				tp := truth[p]
				if tp.bad && int(trims[p]/12.5+0.5) >= tp.from {
					return max(tr.r0, tp.m*simRef)
				}
			}
			return tr.r0
		}
		s := int(trim/12.5 + 0.5)
		if tr.bad && s >= tr.from {
			if v := tr.m * simRef; v > tr.r0 {
				return v
			}
		}
		return tr.r0
	}
	if h, _ := strconv.Atoi(os.Getenv("SCT_HIST")); h > 0 {
		for i := range f.errAcc { // a miner restart, then h hours at these rates (not sctSetup's background hour)
			f.errAcc[i] = 0
		}
		f.advance(time.Duration(h) * time.Hour) // the miner's counters since it started
	}
	start := f.now
	if e := sctStart(2, base, step); e != "" {
		t.Fatal(e)
	}
	sctRun(t, f, 12*time.Hour, nil)
	l := sctLast(t)
	tally.runs++
	tally.minutes += f.now.Sub(start).Minutes()
	switch l["how"] {
	case "guard":
		tally.guards++
	case "aborted", "stopped":
		tally.aborted++
	}
	kept := sctKept(t, l)
	if len(kept) > 0 {
		tally.candRuns++
	}
	hasMed := false
	for c, k := range kept {
		tr := truth[c]
		switch {
		case tr.r0 >= 1:
			hasMed = true
			tally.med++
			if k < 50 {
				tally.medShort++
			}
		case tr.bad:
			tally.bad++
			if k >= float64(tr.from)*12.5 {
				tally.missed++
			}
		default:
			tally.fine++
			if k < 50 && os.Getenv("SCT_DBG") != "" {
				r := sctRow(t, l, c)
				p := int(r["partner"].(float64))
				t.Logf("DBG fine c%d kept %v partner c%d r0=%.2f pref=%v: %v", c, k, p, truth[p].r0, r["partner_ref"], r["why"])
			}
			if k < 50 {
				tally.fineShort++
				if l["how"] == "guard" {
					tally.fineShortGuard++
				}
			}
		}
	}
	if hasMed {
		tally.candWithMed++
	}
}

func pct(a, b int) string {
	if b == 0 {
		return "  -  "
	}
	return fmt.Sprintf("%5.1f%%", 100*float64(a)/float64(b))
}

func TestStrongSim(t *testing.T) {
	n, _ := strconv.Atoi(os.Getenv("SCT_SIM"))
	if n == 0 {
		t.Skip("set SCT_SIM=<runs per scenario>")
	}
	switch os.Getenv("SCT_V") { // A/B: the round-2 changes off (same code otherwise)
	case "orig":
		sctPool, sctPartnerChk, sctConfirmMax, sctHistRank = false, false, 0, false
	case "noconfirm":
		sctConfirmMax = 0
	case "nohist":
		sctHistRank = false
	case "nopartner":
		sctPartnerChk = false
	}
	defer func() { sctPool, sctPartnerChk, sctConfirmMax, sctHistRank = true, true, 3, true }()
	base, _ := strconv.Atoi(os.Getenv("SCT_BASE"))
	step, _ := strconv.Atoi(os.Getenv("SCT_STEP"))
	for _, m := range []float64{0, 2, 5, 20} {
		var tl simTally
		for i := 0; i < n; i++ {
			t.Run("", func(t *testing.T) { simOne(t, int64(i+1)*1000+int64(m), m, 0.5, &tl, base, step) })
		}
		t.Logf("m=%-3v runs %d  avg %.0f min  guard %s  | fine chips %d: short of +50 %s (by guard %s) | bad chips %d: kept a bad step %s | 1/h chips taken %d (in %s of runs): short %s",
			m, tl.runs, tl.minutes/float64(tl.runs), pct(tl.guards, tl.runs), tl.fine, pct(tl.fineShort, tl.fine), pct(tl.fineShortGuard, tl.fine),
			tl.bad, pct(tl.missed, tl.bad), tl.med, pct(tl.candWithMed, tl.candRuns), pct(tl.medShort, tl.med))
	}
}
