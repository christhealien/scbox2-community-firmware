package main

// Round 7, chip-trim API lane (chiptrim.go handleChipTrim/chipTrimChange/readChipTrims/plusTrimErr, the crash
// switch-off in minerhealth.go, chipwatch Undo). None of these tests calls the Enable path (it runs killall).

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func r7aTrim(body string) map[string]any {
	rr := httptest.NewRecorder()
	handleChipTrim(rr, httptest.NewRequest("POST", "/api/chiptrim", strings.NewReader(body)))
	var out map[string]any
	json.Unmarshal(rr.Body.Bytes(), &out)
	return out
}

// r7aSetup: the r4 fake stock backend, chip trims switched on, no hook report (applyNote fails cleanly).
func r7aSetup(t *testing.T, setting string) *r4Stock {
	st := r4Setup(t, setting)
	oldStats := chipStatsPath
	chipStatsPath = filepath.Join(t.TempDir(), "none.json")
	t.Cleanup(func() { chipStatsPath = oldStats })
	os.WriteFile(chipHookFlag(), []byte("on\n"), 0o644)
	return st
}

// r7aWorst: the fastest any "+" chip runs at board clock mhz.
func r7aWorst(mhz int) float64 {
	w := float64(mhz)
	for _, t := range readChipTrims() {
		if t.MHz > 0 && float64(mhz)+t.MHz > w {
			w = float64(mhz) + t.MHz
		}
	}
	return w
}

const r7aManual600 = `{"manual":true,"tempcontrol":true,"select":0,"manualPowerplan":"600 MHz 9000 V 60 RPM 60 RPM PV 9300",
 "powerplans":[{"info":"615 MHz 9100 V 40 RPM 40 RPM PV 9400","level":0},{"info":"0 MHz 0 V 100 RPM 100 RPM","level":3}]}`

// F1: a hand "+" trim and a Controls clock raise each check the other's old value: both land and the chip runs
// past the clock window (680 + 50 = 730 > 700).
func TestR7APIPlusTrimRacesClockRaise(t *testing.T) {
	st := r7aSetup(t, r7aManual600)
	planMu.Lock() // hold the Controls write between its "+"-trim check and its write
	var wg sync.WaitGroup
	var ctl, trim map[string]any
	wg.Add(1)
	go func() {
		defer wg.Done()
		ctl = r4Call("POST", `{"freq_mhz":680,"vrun_mv":9100,"pv_mv":9400,"fan1":60,"fan2":60,"confirm":true}`)
	}()
	time.Sleep(300 * time.Millisecond) // Controls has passed plusTrimErr(680) (no trims yet) and waits on planMu
	wg.Add(1)
	go func() { defer wg.Done(); trim = r7aTrim(`{"chain":0,"chip":5,"mhz":50}`) }()
	time.Sleep(300 * time.Millisecond)
	planMu.Unlock()
	wg.Wait()
	run := st.running().FreqMHz
	t.Logf("controls: %v / trim: ok=%v err=%v / runs %d MHz, trims %+v", ctl["ok"], trim["ok"], trim["err"], run, readChipTrims())
	if w := r7aWorst(run); w > float64(clockWindowHi()) {
		t.Fatalf("a chip runs %.1f MHz, past the %d MHz clock window", w, clockWindowHi())
	}
}

// F2: re-locking the clock limits leaves a "+" chip past the (now lower) window.
func TestR7APIRelockLeavesPlusChipPastWindow(t *testing.T) {
	st := r7aSetup(t, strings.Replace(r7aManual600, "600 MHz", "680 MHz", 1))
	saveOverclock(overclockState{Acknowledged: true})
	if r := r7aTrim(`{"chain":0,"chip":5,"mhz":50}`); r["ok"] != true {
		t.Fatalf("+50 at 680 with the limits unlocked refused: %v", r["err"])
	}
	rr := httptest.NewRecorder()
	handleOverclock(rr, httptest.NewRequest("POST", "/api/overclock", strings.NewReader(`{"revoke":true}`)))
	run := st.running().FreqMHz
	if w := r7aWorst(run); w > float64(clockWindowHi()) {
		t.Fatalf("limits re-locked (window %d MHz) but board %d MHz + trim leaves a chip at %.1f MHz (plusTrimErr now: %v)", clockWindowHi(), run, w, plusTrimErr(run))
	}
	t.Logf("relock: %s / trims %+v", rr.Body.String(), readChipTrims())
}

// F3: on a firmware level (manual off) the "+" check and the live re-apply use the stale manualPowerplan, not
// the clock that runs: a +25 at the 615 MHz stock level is refused as "705 MHz".
func TestR7APIPlusCheckUsesRunningClock(t *testing.T) {
	r7aSetup(t, strings.Replace(strings.Replace(r7aManual600, `"manual":true`, `"manual":false`, 1), "600 MHz", "680 MHz", 1))
	set, ok := reapplySetClock()
	if !ok || set != 615 {
		t.Errorf("reapplySetClock = %d,%v; the miner runs its 615 MHz level", set, ok)
	}
	if r := r7aTrim(`{"chain":0,"chip":5,"mhz":25}`); r["ok"] != true {
		t.Errorf("+25 at the 615 MHz level refused: %v", r["err"])
	}
}

// F4: the board clock can't be read (backend restarting): a "+" trim is taken without any window check.
func TestR7APIPlusTrimUncheckedWhenClockUnknown(t *testing.T) {
	r7aSetup(t, r7aManual600)
	old := *minerWeb
	*minerWeb = "127.0.0.1:1" // nothing listens
	defer func() { *minerWeb = old }()
	if r := r7aTrim(`{"chain":0,"chip":5,"mhz":50}`); r["ok"] == true {
		t.Fatalf("+50 accepted with the board clock unknown (trims now %+v)", readChipTrims())
	}
	if r := r7aTrim(`{"chain":0,"chip":5,"mhz":-25}`); r["ok"] != true { // slowing a chip needs no clock
		t.Fatalf("-25 refused with the board clock unknown: %v", r["err"])
	}
}

// F5: the limit probe steps the clock to the unlocked ceiling; a hand edit (or switching the hook) is let in
// under it, and with a "+" chip the probe's raise is refused by plusTrimErr, which it records as a firmware cap.
func TestR7APITrimEditDuringLimitProbe(t *testing.T) {
	r7aSetup(t, r7aManual600)
	saveOverclock(overclockState{Acknowledged: true})
	setCharge("ceiling")
	if r := r7aTrim(`{"chain":0,"chip":5,"mhz":50}`); r["ok"] == true {
		t.Errorf("hand trim accepted while the limit probe is in charge")
	}
	os.WriteFile(chipTrimPath(), []byte("0 5 50\n"), 0o644)
	setCharge("")
	_, err := mutatePlan(func(p *powerplan) { p.FreqMHz = 875 }) // a probe step (ceilApply)
	t.Logf("probe step 875 with a +50 chip: %v (the probe logs this as \"firmware rejected 875 — cap is ...\")", err)
	if err == nil {
		t.Fatal("875 + 50 accepted")
	}
	// so the probe mustn't start with a "+" chip that its top steps would push past the window
	rr := httptest.NewRecorder()
	handleCeiling(rr, httptest.NewRequest("POST", "/api/ceiling", strings.NewReader(`{"start":true}`)))
	if !strings.Contains(rr.Body.String(), "faster than its board") {
		ceilAbort.Store(true)
		t.Errorf("limit probe start with a +50 chip: %s", rr.Body.String())
	}
}

// F6: the crash switch-off counts stops from the last 15 minutes; switching chip trims back on by hand right after
// it is undone at the next health tick (15 s), before the hooked miner has even started.
func TestR7APIReenableAfterCrashOffIsUndone(t *testing.T) {
	*authFile = filepath.Join(t.TempDir(), "auth.json")
	now := time.Now()
	mhMu.Lock()
	mhStops = []time.Time{now.Add(-9 * time.Minute), now.Add(-7 * time.Minute), now.Add(-5 * time.Minute), now.Add(-3 * time.Minute)}
	mhMu.Unlock()
	t.Cleanup(func() { mhMu.Lock(); mhStops = nil; mhMu.Unlock() })
	// what the Enable path does (minus restartMiner): markers cleared, the note removed, the flag written
	os.WriteFile(chipHookFlag(), []byte("on\n"), 0o644)
	chipHookArmedByHand() // the fix (a no-op stub before it)
	mhMu.Lock()
	chipHookGuard(now.Add(15 * time.Second))
	mhMu.Unlock()
	if _, err := os.Stat(chipHookFlag()); err != nil {
		b, _ := os.ReadFile(chipHookFlag() + ".off")
		t.Fatalf("switched on by hand, switched off again 15 s later: %s", b)
	}
}

// F7: Undo auto-trims takes off a chip's trim even when the owner changed it by hand since the last tick.
func TestR7APIUndoWipesHandEdit(t *testing.T) {
	r7aSetup(t, r7aManual600)
	cwMu.Lock()
	cwLoaded, cwSt = true, cwState{On: false, NoHelp: map[string]int64{}, Owned: []cwOwned{{Chain: 0, Chip: 5, MHz: -25, Judged: true}}}
	cwMu.Unlock()
	t.Cleanup(func() { cwMu.Lock(); cwLoaded, cwSt = false, cwState{}; cwMu.Unlock() })
	writeChipTrims([]chipTrim{{0, 5, -25}})
	if r := r7aTrim(`{"chain":0,"chip":5,"mhz":12.5}`); r["ok"] != true { // the owner's hand edit
		t.Fatal(r["err"])
	}
	r7aTrim(`{"watch_undo":true}`)
	ts := readChipTrims()
	if len(ts) != 1 || ts[0].MHz != 12.5 {
		t.Fatalf("hand trim +12.5 on b0.c5 gone after Undo auto-trims: %+v", ts)
	}
}

// F8: chips past 46 (the SC Lite has 46 a board) are taken; the UI never shows them, the hook never applies them,
// but plusTrimErr counts them: an invisible "+50 on chip 60" blocks every clock raise.
func TestR7APIPhantomChipBlocksClock(t *testing.T) {
	r7aSetup(t, r7aManual600)
	if r := r7aTrim(`{"chain":3,"chip":60,"mhz":50}`); r["ok"] == true {
		t.Errorf("trim on chip 60 accepted (boards have 46 chips); plusTrimErr(675) now: %v", plusTrimErr(675))
	}
	// taking one off stays possible (cleaning up an old file)
	writeChipTrims([]chipTrim{{3, 60, 50}})
	if r := r7aTrim(`{"chain":3,"chip":60,"mhz":0}`); r["ok"] != true || len(readChipTrims()) != 0 {
		t.Errorf("can't clear an out-of-range trim: %v %+v", r["err"], readChipTrims())
	}
}
