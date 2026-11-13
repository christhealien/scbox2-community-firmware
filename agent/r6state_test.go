package main

// r6-state: what is persisted / restored across a panel restart, and what restarts mining.
// Uses newHLMiner (heatloop_test.go) and r5Restart (r5restart_test.go).

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// 1. The limit probe ("Find firmware ceiling") applies clocks past the safe window (up to 900 MHz) for a few
// seconds each. A panel killed (crash, power cut, update) while one is applied leaves it there for good: the
// firmware stores the plan, nothing records the probe's baseline, nothing puts it back after the start.
func TestR6CeilingProbeCutShortLeavesProbeClock(t *testing.T) {
	m := newHLMiner(t)
	if err := saveOverclock(overclockState{Acknowledged: true}); err != nil {
		t.Fatal(err)
	}
	// a proxy in front of the fake miner: the probe's second plan write (the put-back after its first step)
	// never answers, as if the panel died right after the step landed
	var mu sync.Mutex
	puts := 0
	release := make(chan struct{})
	stepped := make(chan struct{}, 1)
	up := m.srv.URL
	px := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Method == "PUT" {
			mu.Lock()
			puts++
			n := puts
			mu.Unlock()
			if n == 2 {
				select {
				case stepped <- struct{}{}:
				default:
				}
				<-release
				http.Error(w, "gone", 500)
				return
			}
		}
		req, _ := http.NewRequest(r.Method, up+r.URL.RequestURI(), bytes.NewReader(body))
		req.Header = r.Header.Clone()
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			http.Error(w, err.Error(), 502)
			return
		}
		defer resp.Body.Close()
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
	}))
	*minerWeb = strings.TrimPrefix(px.URL, "http://")
	t.Cleanup(func() {
		close(release)
		for i := 0; i < 300 && ceilSnapshot().Running; i++ { // let the stale probe goroutine end
			time.Sleep(50 * time.Millisecond)
		}
		px.Close()
		setCharge("")
	})
	rr := httptest.NewRecorder()
	handleCeiling(rr, httptest.NewRequest("POST", "/api/ceiling", strings.NewReader(`{"start":true}`)))
	if !strings.Contains(rr.Body.String(), `"ok":true`) {
		t.Fatalf("probe didn't start: %s", rr.Body.String())
	}
	select {
	case <-stepped:
	case <-time.After(20 * time.Second):
		t.Fatal("the probe never wrote its first step")
	}
	if got := m.planMHz(); got <= freqMax {
		t.Fatalf("setup: first probe step not on the miner (%d MHz)", got)
	}
	r5Restart()         // the panel dies with the probe step applied; a new one starts
	r6StartupRecover(t) // what main() runs at start for this
	if got := m.planMHz(); got != 625 {
		t.Fatalf("BUG: a limit probe cut short by a panel restart leaves the miner on its probe clock %d MHz (past the %d MHz window) for good; it ran 625 before the probe", got, freqMax)
	}
}

// 2. tunerRecover's retry loop (the miner's web API answers late after a boot): an Idle the owner (or the
// thermal guard) chose between two tries is woken by the next try, which writes the run's baseline.
func TestR6TunerRecoverRetryWakesIdle(t *testing.T) {
	m := newHLMiner(t)
	captureNotify(t)
	mk := tunerMarker{Started: time.Now().Add(-time.Hour).Unix(), Base: planMV{MHz: 600, MV: 9000, PV: 9300}, KeptSet: true}
	b, _ := json.Marshal(mk)
	writeAtomic(tunerMarkerPath(), b, 0o644)
	r6FastRetry(t)
	tries := 0
	tunerRecoverApply = func(p planMV) error {
		tries++
		if tries == 1 { // the backend isn't up yet; meanwhile the owner presses Idle
			if err := applyPresetName("Idle"); err != nil {
				t.Fatalf("owner's Idle: %v", err)
			}
			return io.ErrUnexpectedEOF
		}
		return applyTunePlan(p)
	}
	t.Cleanup(func() { tunerRecoverApply = applyTunePlan })
	tunerRecover(time.Minute)
	if !m.isIdle() {
		t.Fatalf("BUG: the owner's Idle (chosen while tunerRecover was still retrying) was woken onto the run's baseline %d MHz (tries %d, charge %q)", m.planMHz(), tries, getCharge())
	}
}

// 3. The schedule is switched off inside a painted stretch. What it ran before the stretch is kept (memory and
// schedule-before.json). The owner sets 650 MHz by hand; weeks later turns the schedule on again in unpainted
// time: "back to what it ran before" puts the weeks-old 625 MHz back over the owner's own setting. A panel
// restart in between does the same from the file.
func TestR6ScheduleReEnableRestoresStaleBefore(t *testing.T) {
	for _, restart := range []bool{false, true} {
		m := newHLMiner(t)
		schedPath = filepath.Join(t.TempDir(), "schedule.json")
		now := time.Now()
		if now.Hour() == 0 && now.Minute() < 5 || now.Hour() == 23 && now.Minute() > 50 {
			t.Skip("too near midnight for the HH:MM rules")
		}
		hm := func(d time.Duration) string { return now.Add(d).Format("15:04") }
		all := []int{0, 1, 2, 3, 4, 5, 6}
		put := func(en bool, rs ...schedRule) {
			b, _ := json.Marshal(schedule{Enabled: en, Rules: rs})
			writeAtomic(schedPath, b, 0o644)
		}
		lastSchKey, lastSchName, schedBefore, schedBeforeBy = -1, "", nil, ""
		put(true, schedRule{Start: hm(-3 * time.Minute), Days: all, Preset: "Idle"}, schedRule{Start: hm(2 * time.Hour), Days: all, Preset: schedBack})
		scheduleTick() // the painted Idle stretch begins (before: 625 MHz)
		if !m.isIdle() {
			t.Fatal("setup: stretch didn't idle")
		}
		put(false, schedRule{Start: hm(-3 * time.Minute), Days: all, Preset: "Idle"}, schedRule{Start: hm(2 * time.Hour), Days: all, Preset: schedBack})
		scheduleTick() // switched off
		setCharge("")
		if _, err := mutatePlan(func(p *powerplan) { p.FreqMHz = 650 }); err != nil { // the owner's own clock
			t.Fatal(err)
		}
		if restart {
			r5Restart()
			schedBefore, schedBeforeBy = nil, ""
		}
		// on again, now in unpainted time
		put(true, schedRule{Start: hm(-1 * time.Minute), Days: all, Preset: schedBack}, schedRule{Start: hm(2 * time.Hour), Days: all, Preset: "Idle"})
		lastSchKey = -1
		scheduleTick()
		if got := m.planMHz(); got != 650 {
			t.Errorf("BUG (restart %v): turning the schedule back on put the miner on %d MHz, what it ran before a stretch the schedule was switched off in, over the owner's 650 MHz (charge %q)", restart, got, getCharge())
		}
		schedPath, schedBefore, schedBeforeBy, lastSchName, lastSchKey = "", nil, "", "", -1
	}
}

// 4. A run whose end couldn't put the miner back (the OEM's backend didn't answer) keeps its restart marker on
// purpose: "choose Stock or a preset, or reboot". The owner chooses (here Idle). At the next panel restart
// (an update, a power cut: days later) tunerRecover still finds the marker and puts the run's baseline back over
// the owner's choice: an Idle is woken. Untouched, the baseline must still come back.
func TestR6KeptMarkerOverridesLaterChoice(t *testing.T) {
	for _, ownerIdle := range []bool{true, false} {
		m := newHLMiner(t)
		captureNotify(t)
		test := planMV{MHz: 675, MV: 9200, PV: 9500}
		if err := applyTunePlan(test); err != nil { // the run's last test setting, left on the miner
			t.Fatal(err)
		}
		mk := tunerMarker{Started: time.Now().Add(-time.Hour).Unix(), Base: planMV{MHz: 600, MV: 9000, PV: 9300}, KeptSet: true, Testing: &test}
		b, _ := json.Marshal(mk)
		writeAtomic(tunerMarkerPath(), b, 0o644)
		e := newTunerEngine(tunerConfig{})
		e.restoreFailed = true
		setCharge("tuner")
		tunerEnded(e) // the run's end: put-back failed, the marker stays
		if ownerIdle {
			if err := applyPresetName("Idle"); err != nil {
				t.Fatal(err)
			}
		}
		r5Restart()
		tunerRecover(time.Second)
		switch {
		case ownerIdle && !m.isIdle():
			t.Errorf("BUG: the Idle the owner chose after the run ended was woken at the next panel start onto the run's old baseline (%d MHz)", m.planMHz())
		case !ownerIdle && m.planMHz() != 600:
			t.Errorf("regression: nothing chosen since, but the baseline wasn't put back (%d MHz)", m.planMHz())
		}
	}
}
