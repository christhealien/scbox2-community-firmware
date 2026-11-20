package main

// C6/C7 per-unit rule (both builds): the probe steps the 12.5 MHz chip grid up to 750 MHz and saves its result for
// this unit; the overclock unlock is refused without a clean result, opens only up to it, and never above 750.

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestC6ProbeStepList(t *testing.T) {
	for _, c := range []struct {
		safe, hi int
		want     string
	}{
		{690, 750, "[700,720,725,740,750]"},
		{690, 745, "[700,720,725,740]"},
		{690, 730, "[700,720,725]"}, // 730 runs 725 on the chips: no step of its own
		{690, 733, "[700,720,725]"},
		{690, 712, "[700]"},     // 712 runs 700
		{690, 713, "[700,713]"}, // 713 runs 712.5: a grid point of its own
		{690, 690, "null"},
		{700, 750, "[720,725,740,750]"},
	} {
		if got := mustJSON(ceilBox2StepList(c.safe, c.hi)); got != c.want {
			t.Errorf("steps %d→%d: %s, want %s", c.safe, c.hi, got, c.want)
		}
	}
	// one request per chip-grid point: 700, 712.5, 725, 737.5, 750 on the chips
	var run []float64
	for _, v := range ceilBox2StepList(690, 750) {
		run = append(run, box2PLLClock(float64(v)))
	}
	if got := mustJSON(run); got != "[700,712.5,725,737.5,750]" {
		t.Fatalf("the chips run %s", got)
	}
	if box2OCHardMax != 750 || ceilHWPctMax != 1.0 || ceilBox2PowerMaxW != 460 {
		t.Fatalf("cap %d, HW limit %.1f %%, power limit %.0f W", box2OCHardMax, ceilHWPctMax, ceilBox2PowerMaxW)
	}
}

// No probe result on this unit: the unlock is refused (and says why), nothing is acknowledged, nothing opens.
func TestC7UnlockRefusedWithoutProbe(t *testing.T) {
	c67Setup(t, c67Unlock)
	r := l15Post(t, handleOverclock, "/api/overclock", `{"acknowledge":true}`)
	if r["ok"] != false || r["need_probe"] != true || !strings.Contains(r["err"].(string), "probe first") {
		t.Fatalf("unlock without a probe result: %v", r)
	}
	if loadOverclock().Acknowledged || ocUnlocked() || clockWindowHi() != 690 {
		t.Fatalf("a refused unlock acknowledged %v / window %d", loadOverclock().Acknowledged, clockWindowHi())
	}
	info := box2ControlInfo()
	if why, _ := info["unlock_err"].(string); why == "" || info["oc_limit"] != 690 || info["probe"].(map[string]any) != nil {
		t.Fatalf("Controls must say why: %v", info)
	}
	rr := httptest.NewRecorder()
	handleOverclock(rr, httptest.NewRequest("GET", "/api/overclock", nil))
	var g map[string]any
	json.Unmarshal(rr.Body.Bytes(), &g)
	if g["limit"] != float64(690) || g["unlock_err"] == "" || g["overclock"] != false {
		t.Fatalf("GET: %v", g)
	}
}

// The first unit's result: clean to 720 MHz, 725 failed on HW errors. The unlock then opens to 720 and no further,
// and that holds after a restart (read back from the config store).
func TestC7UnlockCappedAtProbeResult(t *testing.T) {
	m := c67Setup(t, c67Unlock)
	m.hwAdd = func(mhz int) int {
		if mhz >= 725 {
			return 300 // 23 % HW errors, as measured
		}
		return 1
	}
	st := c67Run(t, m)
	if a := st.Axes[0]; a.Cap != 720 || !strings.Contains(a.Note, "hardware errors") {
		t.Fatalf("result %+v", a)
	}
	if hi := c67MaxWrite(m); hi != 725 {
		t.Fatalf("wrote up to %d, want the failing 725", hi)
	}
	if _, err := os.Stat(box2ProbePath()); err != nil {
		t.Fatalf("the result isn't on the config store: %v", err)
	}
	if r := l15Post(t, handleOverclock, "/api/overclock", `{"acknowledge":true}`); r["ok"] != true {
		t.Fatalf("unlock with a clean result: %v", r)
	}
	if hi := clockWindowHi(); hi != 720 {
		t.Fatalf("window %d, want this unit's 720", hi)
	}
	if lo, hi := box2ClockWindow(); lo != 525 || hi != 720 {
		t.Fatalf("Controls' window %d–%d", lo, hi)
	}
	if err := box2CheckTuple(720, 42); err != nil {
		t.Fatalf("720 MHz: %v", err)
	}
	for _, mhz := range []int{721, 725, 750} {
		if err := box2CheckTuple(mhz, 42); err == nil {
			t.Fatalf("BUG: %d MHz passes past the 720 MHz probe result", mhz)
		}
		if err := box2OverclockErr(powerplan{FreqMHz: mhz, VrunMv: 42}, powerplan{FreqMHz: 690, VrunMv: 42}); err == nil {
			t.Fatalf("BUG: the writer would take %d MHz", mhz)
		}
	}
	// the probe itself still reaches the 750 cap (it is what can raise the result)
	if hi := ceilBox2Hi(); hi != 750 {
		t.Fatalf("the probe's top %d, want 750", hi)
	}
	// a restart: the saved result is all there is
	r, ok := box2ProbeResult()
	if !ok || r.Cap != 720 || r.FailAt != 725 || box2ProbeLimit() != 720 {
		t.Fatalf("read back %+v (ok %v)", r, ok)
	}
	if info := box2ControlInfo(); info["oc_limit"] != 720 || info["unlock_err"] != "" {
		t.Fatalf("Controls: %v", info)
	}
}

// A result above the cap (a hand-edited file, an older build) never opens more than 750 MHz.
func TestC7UnlockNeverAbove750(t *testing.T) {
	c67Setup(t, c67Unlock)
	if err := box2SaveProbe(box2ProbeRecord{Cap: 800, Top: 800, Level: 42}); err != nil {
		t.Fatal(err)
	}
	if r := l15Post(t, handleOverclock, "/api/overclock", `{"acknowledge":true}`); r["ok"] != true {
		t.Fatalf("unlock: %v", r)
	}
	if box2ProbeLimit() != 750 || clockWindowHi() != 750 {
		t.Fatalf("limit %d window %d, want 750", box2ProbeLimit(), clockWindowHi())
	}
	if err := box2CheckTuple(760, 42); err == nil {
		t.Fatal("BUG: 760 MHz passes")
	}
	if err := box2CheckTuple(750, 42); err != nil {
		t.Fatalf("750 MHz: %v", err)
	}
}

// A probe that fails at its first step (700 MHz): the unit stays at 690, and the unlock says so.
func TestC7ProbeFailedAt700StaysAt690(t *testing.T) {
	m := c67Setup(t, c67Unlock)
	m.hwAdd = func(mhz int) int {
		if mhz >= 700 {
			return 50
		}
		return 1
	}
	st := c67Run(t, m)
	if a := st.Axes[0]; a.Cap != 690 || !strings.Contains(a.Note, "stays at 690") {
		t.Fatalf("result %+v", a)
	}
	r := l15Post(t, handleOverclock, "/api/overclock", `{"acknowledge":true}`)
	if r["ok"] != false || !strings.Contains(r["err"].(string), "no headroom") || !strings.Contains(r["err"].(string), "700 MHz failed") {
		t.Fatalf("unlock after a failed probe: %v", r)
	}
	// an acknowledgment from earlier (a result that has since been replaced) opens nothing either
	saveOverclock(overclockState{Acknowledged: true, At: "x"})
	if hi := clockWindowHi(); hi != 690 {
		t.Fatalf("window %d, want 690", hi)
	}
}

// A probe stopped early saves nothing: the result saved before stays (and so does the unlock's limit).
func TestC6ProbeStoppedKeepsSavedResult(t *testing.T) {
	m := c67Setup(t, c67Probe)
	if err := box2SaveProbe(box2ProbeRecord{Cap: 720, FailAt: 725, Why: "hardware errors", Top: 750, Level: 42}); err != nil {
		t.Fatal(err)
	}
	m.at = func(mhz int, _ *c67Miner, _ *board) {
		if mhz == 720 {
			ceilAbort.Store(true)
		}
	}
	if st := c67Run(t, m); st.Axes[0].State != "aborted" {
		t.Fatalf("not stopped: %+v", st.Axes[0])
	}
	if r, ok := box2ProbeResult(); !ok || r.Cap != 720 || r.FailAt != 725 {
		t.Fatalf("the saved result changed: %+v", r)
	}
}

// The power limit: a step whose wall power for this unit would be over 460 W is not tried. A unit whose own Stock
// High reading is 10 % over the first unit's (431 W) would reach ≈ 463 W at 737.5 MHz: the probe stops at 725.
func TestC6ProbePowerLimit(t *testing.T) {
	m := c67Setup(t, c67Probe)
	box2PowMu.Lock()
	box2PowCfg, box2PowRead = box2PowerConfig{}, false
	box2PowMu.Unlock()
	t.Cleanup(func() {
		box2PowMu.Lock()
		box2PowCfg, box2PowRead = box2PowerConfig{}, false
		box2PowMu.Unlock()
	})
	if err := saveBox2PowerConfig(box2PowerConfig{Cals: []box2PowerCal{{MHz: 690, V: 42, Watts: 431.2, GHS: 1950}}}); err != nil {
		t.Fatal(err)
	}
	st := c67Run(t, m)
	a := st.Axes[0]
	if a.Cap != 725 || !strings.Contains(a.Note, "power") || !strings.Contains(a.Note, "460 W") {
		t.Fatalf("result %+v; log:\n%s", a, strings.Join(st.Log, "\n"))
	}
	if hi := c67MaxWrite(m); hi != 725 {
		t.Fatalf("BUG: wrote up to %d MHz; 740 is over the power limit and must not be tried", hi)
	}
	if r, ok := box2ProbeResult(); !ok || r.Cap != 725 || r.FailAt != 740 {
		t.Fatalf("saved %+v", r)
	}
}

// The result goes through the config write path (cfgWrite): on the FAT store a second write keeps a .bak, and a
// result cut short by a power cut falls back to it.
func TestC6ProbeResultConfigStore(t *testing.T) {
	c67Setup(t, c67Probe)
	old := configFAT
	configFAT = true
	t.Cleanup(func() { configFAT = old })
	box2SaveProbe(box2ProbeRecord{Cap: 700, FailAt: 720, Top: 750, Level: 42})
	box2SaveProbe(box2ProbeRecord{Cap: 720, FailAt: 725, Top: 750, Level: 42})
	if _, err := os.Stat(box2ProbePath() + ".bak"); err != nil {
		t.Fatalf("no .bak: %v", err)
	}
	os.WriteFile(box2ProbePath(), []byte(`{"cap":7`), 0o644)
	if r, ok := box2ProbeResult(); !ok || r.Cap != 700 {
		t.Fatalf(".bak fallback: %+v (ok %v)", r, ok)
	}
	// not part of the settings export (it belongs to this unit)
	for _, s := range settingsSections() {
		if strings.Contains(s.path(), "box2-probe") {
			t.Fatalf("the probe result is in the settings export (%s)", s.name)
		}
	}
}
