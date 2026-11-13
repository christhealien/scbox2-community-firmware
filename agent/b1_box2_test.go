package main

// Lane B1 (Wave B): the fan model made model-driven for the SC Box II (2 fans, one PWM group [[0,1]],
// FanStockMaxRPM 4500, cut-off 86, fan-pair offset and fan-fit OFF). These tests prove:
//   - the fan-pair offset feature is refused and NEVER writes fan2/fan3 (the voltage PWM on the Box II),
//   - the fan model is built from the profile's 2 fans (not a hardcoded 4),
//   - the fan-fit range uses 4500 and the fit write is gated off,
//   - learnedSuspect compares a fan with the OTHER fan in its FanGroup and needs len >= model.Fans,
//   - the stricter "no valid board temperature for 30 s while hashing" guard fires and uses idleAction.
// SC Lite stays the default profile everywhere else, so its behaviour is unchanged (regression tests pass).

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// withBox2Model switches the active profile to the SC Box II for the duration of a test.
func withBox2Model(t *testing.T) {
	t.Helper()
	oldModel, oldWhy := model, modelWhy
	model, modelWhy = profSCBox2, "test: forced SC Box II"
	t.Cleanup(func() { model, modelWhy = oldModel, oldWhy })
}

// TestB1Box2FanPairRefused: on the SC Box II the fan-pair offset is refused (one PWM group), and neither the
// handler nor the tick ever writes fan2/fan3 — those sysfs nodes are the voltage PWM on this board.
func TestB1Box2FanPairRefused(t *testing.T) {
	withBox2Model(t)
	d := fakeFans(t, 40, 40, 40, 40) // fan0..fan3 nodes present so a stray write to fan2/fan3 would be visible
	*authFile = filepath.Join(t.TempDir(), "auth.json")

	// the fan-pair offset must be off on this model
	if model.Feat.FanPairOffset {
		t.Fatal("FanPairOffset should be OFF on the SC Box II")
	}
	if off := fanPairLoad(); off != 0 {
		t.Fatalf("fanPairLoad=%d, want 0 when the feature is off", off)
	}

	// POST an offset: refused, nothing written
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/fanpair", strings.NewReader(`{"offset":12}`))
	handleFanPair(rec, req)
	var res map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("bad JSON: %v (%s)", err, rec.Body.String())
	}
	if res["ok"] != false || res["disabled"] != true {
		t.Fatalf("fan pair POST not refused: %v", res)
	}
	if s, _ := res["err"].(string); !strings.Contains(s, "not available on this model") {
		t.Fatalf("refusal text %q", s)
	}

	// the tick writes nothing either, whatever offset it is handed
	if n := fanPairTick(12, ""); n != "" {
		t.Fatalf("fanPairTick ran on the SC Box II: %q", n)
	}
	fanPairRun()
	fanPairKick()
	time.Sleep(10 * time.Millisecond)
	for i := 0; i < 4; i++ {
		if v, _ := fanLvl(fmt.Sprintf("fan%d", i)); v != 40 {
			t.Fatalf("fan%d changed to %d — fan2/fan3 are the VOLTAGE PWM on the SC Box II and must never be written (dir %s)", i, v, d)
		}
	}
}

// TestB1Box2FanModel: the fan model is built from the profile — 2 fans, stock max 4500, the fan-fit range
// uses 4500, and the fit write is gated off.
func TestB1Box2FanModel(t *testing.T) {
	withBox2Model(t)
	*authFile = filepath.Join(t.TempDir(), "auth.json")

	if model.Fans != 2 {
		t.Fatalf("model.Fans=%d, want 2", model.Fans)
	}
	if fanStockMax() != 4500 {
		t.Fatalf("fanStockMax()=%d, want 4500", fanStockMax())
	}
	// the fit range tops out at 4500 (not the SC Lite 2200): a very fast fan caps there
	fast := []int{-1, 1000, 2000, 3000, 4000, 5000, 6000, 7000, 8000, 9000, 10000}
	if f := fitMaxRPM(fast); f != 4500 {
		t.Fatalf("fit of a fast fan = %d, want 4500 (the Box II stock max)", f)
	}
	// a stock-like Box II fan (≈4500 at 100 %) fits under 4500
	boxFan := []int{-1, 500, 900, 1350, 1800, 2250, 2700, 3150, 3600, 4050, 4480}
	if f := fitMaxRPM(boxFan); f <= 0 || f > 4500 {
		t.Fatalf("Box II fan fit = %d, want 0 < f <= 4500", f)
	}

	// fan-fit is gated off: the fit write is refused (never writes an overlay)
	if model.Feat.FanFit {
		t.Fatal("FanFit should be OFF on the SC Box II")
	}
	if err := fanCheckFit([]int{4000, 4000}); err == nil || !strings.Contains(err.Error(), "not available on this model") {
		t.Fatalf("fanCheckFit not gated off: %v", err)
	}

	// the view reports the model-driven facts
	v := fanCheckView()
	if v["fans"] != 2 || v["stock_max"] != 4500 || v["fit_enabled"] != false {
		t.Fatalf("fanCheckView facts wrong: fans=%v stock_max=%v fit_enabled=%v", v["fans"], v["stock_max"], v["fit_enabled"])
	}
}

// TestB1Box2LearnedSuspect: learnedSuspect compares a fan with the OTHER fan in its FanGroup, using
// len >= model.Fans rather than the old hardcoded 4. On the Box II that is the fan0/fan1 pair.
func TestB1Box2LearnedSuspect(t *testing.T) {
	withBox2Model(t)
	// fan0 flagged, turning near fan1's speed, fan1 clear: a learned-reading flag, not a real fault
	if !learnedSuspect(watchdogView{Fans: []int{780, 840}, Duty: []int{50, 50}, Status: 1}) {
		t.Fatal("Box II pair (fan0 ≈ fan1) not recognised as a learned-reading flag")
	}
	// a stopped fan is a real fault
	if learnedSuspect(watchdogView{Fans: []int{0, 840}, Duty: []int{50, 50}, Status: 1}) {
		t.Fatal("a stopped fan counted as a learned reading")
	}
	// a fan at half its partner's speed is a real problem
	if learnedSuspect(watchdogView{Fans: []int{400, 840}, Duty: []int{50, 50}, Status: 1}) {
		t.Fatal("a slow fan counted as a learned reading")
	}
	// both flagged: not this case
	if learnedSuspect(watchdogView{Fans: []int{800, 840}, Duty: []int{50, 50}, Status: 3}) {
		t.Fatal("both of the pair flagged counted as a learned reading")
	}
	// only one fan reported (len < model.Fans): can't compare, not suspect
	if learnedSuspect(watchdogView{Fans: []int{780}, Duty: []int{50}, Status: 1}) {
		t.Fatal("a single-fan reading should not be a learned reading")
	}
}

// fakeClock is a settable clock for the fan loop.
type fakeClock struct{ t time.Time }

// TestB1Box2NoTempGuardFires: with no valid board temperature while hashing, the SC Box II parks the miner
// after 30 s (SC Lite waits 3 minutes). Driven through the fanDash/fanNow/noTempStop seams.
func TestB1Box2NoTempGuardFires(t *testing.T) {
	withBox2Model(t)

	clk := &fakeClock{t: time.Now()}
	oldNow, oldDash, oldStop := fanNow, fanDash, noTempStop
	fanNow = func() time.Time { return clk.t }
	// a hashing board with no readable temperature (all samples < 5 °C ⇒ guardTemp sees none)
	fanDash = func() dashboard {
		return dashboard{OK: true, Boards: []board{{ID: 0, Alive: true, GHS: 1000, Temps: []float64{0, 0, 0}}}}
	}
	var stops int
	var lastWhy string
	noTempStop = func(why string) error { stops++; lastWhy = why; return nil }
	noTempSince, noTempTicks = time.Time{}, 0
	boardLast = map[int]*boardSeen{}
	t.Cleanup(func() {
		fanNow, fanDash, noTempStop = oldNow, oldDash, oldStop
		noTempSince, noTempTicks = time.Time{}, 0
		boardLast = map[int]*boardSeen{}
	})

	c := defaultFanConfig()
	c.Enabled = true

	// 20 s of blind hashing: not yet (the window is 30 s)
	clk.t = clk.t.Add(5 * time.Second)
	fanTick(c)
	clk.t = clk.t.Add(15 * time.Second)
	fanTick(c)
	if stops != 0 {
		t.Fatalf("parked after 20 s blind; the Box II window is 30 s (stops=%d)", stops)
	}

	// past 30 s: it fires once
	clk.t = clk.t.Add(20 * time.Second) // 40 s since the first blind reading
	fanTick(c)
	if stops != 1 {
		t.Fatalf("no-temp guard did not fire after 30 s blind (stops=%d)", stops)
	}
	if lastWhy != "no board temperature while hashing" {
		t.Fatalf("stop reason %q", lastWhy)
	}
}

// TestB1NoTempGuardSCLiteUnchanged: the default (SC Lite) profile still waits 3 minutes, so the Box II rule
// is strictly stricter and SC Lite behaviour is unchanged.
func TestB1NoTempGuardSCLiteUnchanged(t *testing.T) {
	if noTempIdleWait() != 3*time.Minute || noTempIdleTicks() != 6 {
		t.Fatalf("SC Lite no-temp window changed: wait=%v ticks=%d", noTempIdleWait(), noTempIdleTicks())
	}
	if !strings.Contains(noTempIdleMsg(), "3 minutes") {
		t.Fatalf("SC Lite no-temp message changed: %q", noTempIdleMsg())
	}
	withBox2Model(t)
	if noTempIdleWait() != 30*time.Second || noTempIdleTicks() != 2 {
		t.Fatalf("Box II no-temp window wrong: wait=%v ticks=%d", noTempIdleWait(), noTempIdleTicks())
	}
	if !strings.Contains(noTempIdleMsg(), "30 seconds") {
		t.Fatalf("Box II no-temp message wrong: %q", noTempIdleMsg())
	}
}

// TestB1Box2NoTempStopGraceful: the default noTempStop on the Box II calls idleAction (B3's board-off,
// gated off by SoftIdle until hardware-proven) and tolerates its error gracefully — it returns the error
// (so no false "parked" is announced) and does not panic.
func TestB1Box2NoTempStopGraceful(t *testing.T) {
	withBox2Model(t)
	err := noTempStop("no board temperature while hashing")
	// B3's idleAction (merged) returns a "no verified stop-hashing action / 86°C cutoff is the backstop"
	// error while SoftIdle is off (default). noTempStop must surface it (no false "parked") and not panic.
	if err == nil {
		t.Fatal("Box II noTempStop should surface idleAction's error until SoftIdle is proven on hardware")
	}
}
