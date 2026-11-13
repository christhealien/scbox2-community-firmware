package main

import (
	"strings"
	"testing"
)

// The default map walk goes up only: the first clock starts at the bottom of the PV range, each next clock at the
// lowest PV the clock before passed at, one step up per fail (no retest on the way up), never down.
func TestBottomUpWalk(t *testing.T) {
	c := scfg()
	c.TopDown = false
	c.LowestMHz, c.HighestMHz = 550, 625
	c.ChipTrim = false
	e, f := newFM(t, c)
	// board 1 needs: 550 → any, 575 → mV 8900 (PV 9200), 600 → 8900, 625 → 9000 (PV 9300)
	f.rules = []fmRule{boardRule(1, map[int]int{575: 8900, 600: 8900, 625: 9000}, 0.5)}
	runFM(t, e, f)
	dump(t, e)
	if got := curveStr(e.curve); got != "550/9100 575/9200 600/9200 625/9300" {
		t.Fatalf("curve %s", got)
	}
	// what was tested at each clock, in order (baseline and confirm left out)
	var walk []string
	for _, r := range e.results {
		if r.MHz >= 550 && r.MHz <= 625 && r.MHz != 615 {
			walk = append(walk, r.Result[:1]+itoa(r.MHz)+"/"+itoa(r.PV))
		}
	}
	w := strings.Join(walk, " ")
	// 550 at the bottom passes at once; 575 starts at 9100: two fails, then a pass; 600 starts at 9200 (passes);
	// 625 starts at 9200: one fail, then a pass. No retests, no walking down.
	want := "c550/9100 f575/9100 f575/9150 c575/9200 c600/9200 f625/9200 f625/9250 c625/9300"
	if !strings.HasPrefix(w, want) {
		t.Fatalf("walk\n got %s\nwant %s…", w, want)
	}
	for _, p := range f.applied {
		if p.PV < 9100 || p.PV > 9600 {
			t.Fatalf("PV out of range: %v", p)
		}
	}
	mustContain(t, e, "550 MHz: starting at PV 9100 and going up only if it fails", "one step more voltage instead of a retest")
}

// the top of the range is still retested (the curve would end on that one fail)
func TestBottomUpRetestAtTop(t *testing.T) {
	c := scfg()
	c.TopDown = false
	c.LowestMHz, c.HighestMHz = 550, 575
	c.ChipTrim = false
	c.MinMV = 9200 // PV 9500–9600: two steps
	e, f := newFM(t, c)
	f.rules = []fmRule{needRule(ckey{1, 5}, map[int]int{575: 9400}, 0.15, 0)} // borderline: not a decisive fail
	runFM(t, e, f)
	dump(t, e)
	if !strings.Contains(logStr(e), "PV 9600 (mV 9300) failed") || !strings.Contains(logStr(e), "retesting it once") {
		t.Fatalf("no retest at the top:\n%s", tailLog(e, 40))
	}
}
