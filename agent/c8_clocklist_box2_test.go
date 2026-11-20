package main

import (
	"fmt"
	"strings"
	"testing"
)

// v0.5.4: on the SC Box II a map run's Lowest MHz is itself the first clock when it is off the step grid (690–690
// tests 690, not 675 as well); on the grid, and on the SC Lite, the list is as before. The page's run-plan
// estimate (tClockN) counts the same clocks.

var clockListCases = []struct {
	lo, hi int
	step   float64
	box2   string // the Box II's list
	lite   string // the SC Lite's list (unchanged)
}{
	{690, 690, 25, "[690]", "[675 690]"},
	{690, 690, 12.5, "[690]", "[688]"},
	{660, 690, 25, "[660 675 690]", "[650 675 690]"},
	{660, 690, 12.5, "[660 663 675 688]", "[650 663 675 688]"},
	{525, 690, 12.5, "[525 538 550 563 575 588 600 613 625 638 650 663 675 688]", "[525 538 550 563 575 588 600 613 625 638 650 663 675 688]"},
	{600, 690, 25, "[600 625 650 675 690]", "[600 625 650 675 690]"},
	{588, 613, 12.5, "[588 600 613]", "[588 600 613]"},
	{650, 675, 25, "[650 675]", "[650 675]"},
}

func TestBox2ClockListStartsAtLowest(t *testing.T) {
	list := func(lo, hi int, step float64) []int {
		c := defaultTunerConfig()
		c.LowestMHz, c.HighestMHz, c.Step = lo, hi, step
		return c.clockList()
	}
	for _, x := range clockListCases {
		if got := fmt.Sprint(list(x.lo, x.hi, x.step)); got != x.lite {
			t.Errorf("SC Lite %d–%d step %g: %s, want %s (unchanged)", x.lo, x.hi, x.step, got, x.lite)
		}
	}
	useBox2(t)
	for _, x := range clockListCases {
		cl := list(x.lo, x.hi, x.step)
		if got := fmt.Sprint(cl); got != x.box2 {
			t.Errorf("Box II %d–%d step %g: %s, want %s", x.lo, x.hi, x.step, got, x.box2)
		}
		if len(cl) == 0 || cl[0] < x.lo || pll(float64(cl[len(cl)-1])) != pll(float64(x.hi)) {
			t.Errorf("Box II %d–%d: %v must start at or above the lowest and end on the highest's clock", x.lo, x.hi, cl)
		}
		for i := 1; i < len(cl); i++ {
			if pll(float64(cl[i])) <= pll(float64(cl[i-1])) {
				t.Errorf("Box II %d–%d: %v runs one chip clock twice", x.lo, x.hi, cl)
			}
		}
	}
	// a 690–690 run on the Box II validates (one clock, inside the window)
	c := defaultTunerConfig()
	c.LowestMHz, c.HighestMHz, c.Step = 690, 690, 25
	if err := validateTunerConfig(c); err != nil {
		t.Fatalf("690–690 on the Box II: %v", err)
	}
}

// The page's estimate counts the clocks the Go run tests, on the Box II; the SC Lite keeps its old count.
func TestBox2ClockCountJSMatchesGo(t *testing.T) {
	var js strings.Builder
	for _, x := range clockListCases {
		fmt.Fprintf(&js, "console.log('N %d %d %g '+tClockN({lowest_mhz:%d,highest_mhz:%d,step:%g}));\n", x.lo, x.hi, x.step, x.lo, x.hi, x.step)
	}
	for _, b2 := range []bool{false, true} {
		mdl := "window._MODEL=null;"
		if b2 {
			mdl = "window._MODEL={ok:true,id:'scbox2',plan:'box2v',features:{}};"
		}
		out := l31Node(t, []string{"B2T", "tClockN"}, mdl+js.String())
		for _, x := range clockListCases {
			want := 0
			if b2 {
				want = strings.Count(x.box2, " ") + 1
			} else {
				want = int(float64(x.hi-x.lo)/x.step) + 1 // the SC Lite's count, as before
			}
			line := fmt.Sprintf("N %d %d %g %d\n", x.lo, x.hi, x.step, want)
			if !strings.Contains(out, line) {
				t.Errorf("box2 %v: want %q in\n%s", b2, line, out)
			}
		}
	}
}
