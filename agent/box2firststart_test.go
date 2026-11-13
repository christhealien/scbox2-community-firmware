//go:build scbox2

package main

import "testing"

// A first start on the SC Box II selects Stock High (the "Hashrate" stock mode) once minerd answers, even when
// minerd already runs a stock mode by index; a later start leaves minerd's own selection alone.
func TestBox2FirstStartSelectsStockHigh(t *testing.T) {
	for _, first := range []bool{true, false} {
		var got []string
		oF, oA := box2FirstStart, box2FirstStartApply
		box2FirstStart = func() bool { return first }
		box2FirstStartApply = func(name string, gen int64) error { got = append(got, name); return nil }
		cur := map[string]any{"manual": false}
		box2PlanBootOnce(cur)
		box2FirstStart, box2FirstStartApply = oF, oA
		if first && (len(got) != 1 || got[0] != "Hashrate") {
			t.Fatalf("first start: applied %v, want [Hashrate] (Stock High)", got)
		}
		if !first && len(got) != 0 {
			t.Fatalf("later start: applied %v, want nothing", got)
		}
	}
}
