package main

// Round-2 review of the strong-chip test: state machine and safety repros.

import (
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// The limit probe's start checks "who's in charge" and sctRunning() without controlMu, then reads the plan
// (a network call, plus a 2 s retry) and only then takes charge: a strong-chip test that starts inside that
// window (it holds controlMu and sees nobody in charge) runs together with the probe, which steps the board
// clock under the raised chips.
func TestSctR2CeilingStartRace(t *testing.T) {
	f := sctSetup(t)
	_ = f
	fb := newFakeBackend(t, false)
	sctBlocked = sctBlockedReal
	if err := saveOverclock(overclockState{Acknowledged: true}); err != nil {
		t.Fatal(err)
	}
	old := getCharge()
	setCharge("")
	defer setCharge(old)
	sctErr, done := "not tried", make(chan struct{})
	fb.mu.Lock()
	fb.onGet = func(n int) {
		if n == 1 { // the probe's plan read: the strong test's start comes in now
			go func() { sctErr = sctStart(2, 0, 0); close(done) }()
			time.Sleep(100 * time.Millisecond)
		}
	}
	fb.mu.Unlock()
	ceilAbort.Store(false)
	rr := httptest.NewRecorder()
	handleCeiling(rr, httptest.NewRequest("POST", "/api/ceiling", strings.NewReader(`{"start":true}`)))
	probeOn := strings.Contains(rr.Body.String(), `"ok":true`)
	<-done
	ceilAbort.Store(true)
	// wait for the probe goroutine to put the plan back and end
	for i := 0; i < 400; i++ {
		ceilMu.Lock()
		r := ceilSt.Running
		ceilMu.Unlock()
		if !r {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if probeOn && sctErr == "" {
		t.Fatalf("both started: limit probe (%s) and the strong-chip test", strings.TrimSpace(rr.Body.String()))
	}
	t.Logf("probe: %s; strong test: %q", strings.TrimSpace(rr.Body.String()), sctErr)
}

// A preset (or a manual clock) applied while the test runs changes the board clock under the raised chips:
// they run their test trim on top of the new clock (+37.5 over a faster clock, at a voltage chosen for the
// old one) until the test notices 10 minutes later.
func TestSctR2PresetDuringTest(t *testing.T) {
	sctSetup(t)
	newFakeBackend(t, false)
	old := getCharge()
	setCharge("")
	defer setCharge(old)
	if e := sctStart(2, 0, 0); e != "" {
		t.Fatal(e)
	}
	rr := httptest.NewRecorder()
	handleControl(rr, httptest.NewRequest("POST", "/api/control", strings.NewReader(`{"freq_mhz":650,"vrun_mv":9100,"pv_mv":9400,"fan1":60,"fan2":60,"confirm":true}`)))
	if strings.Contains(rr.Body.String(), `"ok":true`) {
		t.Errorf("a manual clock was applied while the strong-chip test runs: %s", rr.Body.String())
	}
	if testing.Verbose() {
		t.Logf("manual: %s", rr.Body.String())
	}
	savePresets([]preset{{Name: "fast", MHz: 700, MV: 9300, PV: 9600}})
	rr = httptest.NewRecorder()
	handlePresets(rr, httptest.NewRequest("POST", "/api/presets", strings.NewReader(`{"action":"apply","name":"fast"}`)))
	if strings.Contains(rr.Body.String(), `"ok":true`) {
		t.Errorf("a preset was applied while the strong-chip test runs: %s", rr.Body.String())
	}
}

// The ordinary interrupted test still puts the trims back.
func TestSctR2InterruptedStillRestored(t *testing.T) {
	f := sctSetup(t)
	if e := sctStart(2, 0, 0); e != "" {
		t.Fatal(e)
	}
	for i := 0; i < 100; i++ {
		f.advance(sctTick)
		sctTickOnce()
	}
	sct.mu.Lock()
	sct.reset()
	sct.mu.Unlock()
	sctRecover()
	sctOwnTrimsIntact(t)
	if m := sctTrimsOf(2); len(m) != 1 {
		t.Fatalf("test trims left: %v", m)
	}
}

func sctRow(t *testing.T, l map[string]any, chip int) map[string]any {
	t.Helper()
	for _, c := range l["chips"].([]any) {
		r := c.(map[string]any)
		if int(r["chip"].(float64)) == chip {
			return r
		}
	}
	t.Fatalf("chip %d not in the result", chip)
	return nil
}

// A chip that fails +25 and sits at +12.5 keeps being watched there: 2 errors per 20 min at +12.5 passes one
// window (P = 1.8 %) but not three.
func TestSctR2FrozenChipRechecked(t *testing.T) {
	f := sctSetup(t)
	f.errRate = func(chip int, trim float64, _ map[int]float64) float64 {
		switch {
		case chip == 3 && trim >= 25:
			return 30
		case chip == 3 && trim > 0:
			return 6
		}
		return sctBackground(chip)
	}
	if e := sctStart(2, 0, 0); e != "" {
		t.Fatal(e)
	}
	sctRun(t, f, 4*time.Hour, nil)
	l := sctLast(t)
	r := sctRow(t, l, 3)
	if r["kept"].(float64) != 0 || sctTrimsOf(2)[3] != 0 || !strings.Contains(r["why"].(string), "back to +0") {
		t.Fatalf("chip 3: %v (file %v)", r, sctTrimsOf(2)[3])
	}
	if l["how"] != "done" {
		t.Fatalf("how %v: %v", l["how"], l["message"])
	}
	sctOwnTrimsIntact(t)
}

// The last step a chip passes is watched once more: 5 errors an hour at +50 slips through one 20-min window
// (1 error) but not the confirm window on top of it.
func TestSctR2ConfirmWindow(t *testing.T) {
	f := sctSetup(t)
	f.errRate = func(chip int, trim float64, _ map[int]float64) float64 {
		if chip == 3 && trim >= 50 {
			return 5
		}
		return sctBackground(chip)
	}
	if e := sctStart(2, 0, 0); e != "" {
		t.Fatal(e)
	}
	sctRun(t, f, 5*time.Hour, nil)
	l := sctLast(t)
	if r := sctRow(t, l, 3); r["kept"].(float64) != 37.5 || sctTrimsOf(2)[3] != 37.5 {
		t.Fatalf("chip 3: %v (file %v)", r, sctTrimsOf(2)[3])
	}
	for c, k := range sctKept(t, l) {
		if c != 3 && k != 50 {
			t.Fatalf("chip %d kept %v", c, k)
		}
	}
	sctOwnTrimsIntact(t)
}

// The other chip of a raised chip's stage starts making errors: the raised chip goes back.
func TestSctR2PartnerErrors(t *testing.T) {
	f := sctSetup(t)
	f.errRate = func(chip int, _ float64, tr map[int]float64) float64 {
		if chip == 4 && tr[3] >= 25 {
			return 30
		}
		return sctBackground(chip)
	}
	if e := sctStart(2, 0, 0); e != "" {
		t.Fatal(e)
	}
	sctRun(t, f, 5*time.Hour, nil)
	l := sctLast(t)
	if r := sctRow(t, l, 3); r["kept"].(float64) != 12.5 || !strings.Contains(r["why"].(string), "c4") {
		t.Fatalf("chip 3: %v", r)
	}
	sctOwnTrimsIntact(t)
}

// A rollback (or an update) while the test runs: the older panel doesn't know the test, so the raised chips
// would stay raised (the old hook takes up to +25) and its backup would be left behind.
func TestSctR2RollbackRefusedDuringTest(t *testing.T) {
	sctSetup(t)
	if e := sctStart(2, 0, 0); e != "" {
		t.Fatal(e)
	}
	oldReboot := updateReboot
	updateReboot = func() { t.Fatal("rebooted") }
	defer func() { updateReboot = oldReboot }()
	for _, body := range []string{`{"rollback":true}`} { // (install: the same check, after its staged-file checks)
		rr := httptest.NewRecorder()
		handleUpdate(rr, httptest.NewRequest("POST", "/api/update", strings.NewReader(body)))
		if !strings.Contains(rr.Body.String(), "strong-chip") {
			t.Errorf("%s while the test runs: %s", body, strings.TrimSpace(rr.Body.String()))
		}
	}
}

// Locks: the test's ticks, start, stop and finish against everything that asks "is it running?" under its own
// locks (auto-trim's pause check under cwMu, the schedule and preset paths under controlMu, the views). A deadlock fails on the timeout.
func TestSctR2LockStress(t *testing.T) {
	f := sctSetup(t)
	var clk sync.Mutex
	now := f.now
	sctNow = func() time.Time { clk.Lock(); defer clk.Unlock(); return now }
	stop := make(chan struct{})
	done := make(chan struct{})
	var wg sync.WaitGroup
	go func() { // the test itself: start, tick, stop, again
		defer close(done)
		for i := 0; i < 30; i++ {
			sctStart(2, 5, 5)
			for j := 0; j < 50; j++ {
				clk.Lock()
				f.advance(sctTick)
				now = f.now
				clk.Unlock()
				sctTickOnce()
			}
			sctStop()
		}
	}()
	for _, g := range []func(){
		func() { cwMu.Lock(); cwBlockedReal(); cwMu.Unlock() },
		func() { controlMu.Lock(); sctRunning(); controlMu.Unlock() },
		func() { sctView() },
		func() { chipWatchView() },
	} {
		g := g
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					g()
				}
			}
		}()
	}
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("deadlock: the strong test's loop didn't finish")
	}
	close(stop)
	wg.Wait()
}
