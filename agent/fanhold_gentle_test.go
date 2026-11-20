package main

import "testing"

// The SC Box II's gentle fan hold: on a board whose temperature follows the fan a few readings late, starting from
// stock (68 °C at 69 %), it trickles to a 72 °C target without overshooting past 75 °C or jumping the fans.
func TestFanHoldGentleNoBlast(t *testing.T) {
	h := fanHold{init: true, base: 69, gentle: true}
	fan, temp := 69, 68.0
	hist := []int{fan, fan, fan}
	maxT, maxStep := temp, 0
	for i := 0; i < 400; i++ {
		lagged := hist[len(hist)-3]
		eq := 100 - 0.45*float64(lagged) // the board's steady temperature for a fan level (≈69 % → 69 °C)
		temp += (eq - temp) * 0.15
		w := h.want(temp, 72, fan, 30, 100)
		if d := w - fan; d > maxStep {
			maxStep = d
		}
		fan = w
		hist = append(hist, fan)
		if temp > maxT {
			maxT = temp
		}
	}
	if maxT > 75 {
		t.Errorf("overshoot: reached %.1f °C for a 72 °C target", maxT)
	}
	if maxStep > 4 {
		t.Errorf("fan jumped %d %% in one reading", maxStep)
	}
	if temp < 70.5 || temp > 73.5 {
		t.Errorf("didn't settle near 72 °C: %.1f °C at %d %%", temp, fan)
	}
}
