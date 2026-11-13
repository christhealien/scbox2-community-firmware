package main

// C6/C7 (commissioning): the SC Box II clock-ceiling probe and the overclock unlock. A fake Goldshell backend
// (/mcb/setting) and a fake miner (the probe's readings) whose temperature, HW errors and chip health follow
// the clock the probe set. The fake clock backend stands in for lane C3's: it writes any clock up to
// clockWindowHi() and calls box2OverclockErr, the contract ceilingbox2.go documents.

import (
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

type c67Miner struct {
	mu      sync.Mutex
	setting map[string]any
	writes  []powerplan
	hw, non int
	health  boardHealth
	// scenario, by the clock now applied (nil: fine)
	temp  func(mhz int) float64
	hwAdd func(mhz int) int
	at    func(mhz int, m *c67Miner, b *board) // free-form: drop a chip, kill the board, press Stop…
}

func (m *c67Miner) clock() int {
	pp, _ := resolveRunningPlan(m.setting)
	return pp.FreqMHz
}

// c67TestCeil: the overclock ceiling these tests give the profile (not the Box II's: that one is undecided).
const c67TestCeil = 750

func c67Setup(t *testing.T, feat modelFeatures) *c67Miner {
	t.Helper()
	dir := t.TempDir()
	oldAuth := *authFile
	*authFile = filepath.Join(dir, "auth.json")
	*presetPath = filepath.Join(dir, "presets.json")
	*fanPath = filepath.Join(dir, "fan.json")
	oldAPI := *apiAddr
	*apiAddr = "127.0.0.1:47321" // nothing listens: the probe reads ceilDash
	fanSysDir = filepath.Join(dir, "fans")
	fanGuardMu.Lock()
	fanGuardSt, fanGuardOK = fanGuardState{}, true
	fanGuardMu.Unlock()
	useBox2(t)
	model.Feat = feat
	// the SC Box II's own overclock ceiling isn't decided (c67TestCeil = 0: C6/C7 refuse above 690 MHz,
	// TestBox2NoClockAbove690); these tests exercise the probe and unlock machinery against a test ceiling
	model.OCHardMax = c67TestCeil
	setWindowsFromModel()
	m := &c67Miner{health: boardHealth{ID: 0, Chips: 36, Expected: 36}}
	json.Unmarshal([]byte(box2Setting), &m.setting)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/user/login"):
			json.NewEncoder(w).Encode(map[string]any{"JWT Token": "tok"})
		case r.Method == "GET":
			m.mu.Lock()
			json.NewEncoder(w).Encode(m.setting)
			m.mu.Unlock()
		default:
			b, _ := io.ReadAll(r.Body)
			m.mu.Lock()
			json.Unmarshal(b, &m.setting)
			m.mu.Unlock()
			io.WriteString(w, `{"status":0}`)
		}
	}))
	oldWeb := *minerWeb
	*minerWeb = strings.TrimPrefix(srv.URL, "http://")
	jwtToken = ""
	authMu.Lock()
	auth.MinerPass = "pw"
	authMu.Unlock()

	oldMut, oldDash, oldHold, oldEvery := ceilMutate, ceilDash, ceilBox2Hold, ceilBox2Every
	ceilBox2Hold, ceilBox2Every = 40*time.Millisecond, 4*time.Millisecond
	ceilMutate = func(gen int64, mut func(*powerplan)) (string, error) {
		if genMoved(gen) {
			return "", errSafetyStop
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		pp, ok := resolveRunningPlan(m.setting)
		if !ok {
			return "", errSafetyStop
		}
		before := pp
		mut(&pp)
		if err := box2OverclockErr(pp, before); err != nil {
			return "", err
		}
		if pp.FreqMHz != before.FreqMHz && (pp.FreqMHz > clockWindowHi() || pp.FreqMHz < freqMin) {
			return "", putRefusedErr{errSafetyStop}
		}
		plan := buildPowerplanBox2(pp)
		m.setting["manual"], m.setting["manualPowerplan"] = true, plan
		m.writes = append(m.writes, pp)
		return plan, nil
	}
	ceilDash = func() dashboard {
		m.mu.Lock()
		defer m.mu.Unlock()
		mhz := m.clock()
		t := 75.0
		if m.temp != nil {
			t = m.temp(mhz)
		}
		m.non += 1000
		if m.hwAdd != nil {
			m.hw += m.hwAdd(mhz)
		}
		b := board{ID: 0, Alive: true, Enabled: true, Temps: []float64{t, t - 2}, HWErrors: m.hw, Nonced: m.non,
			ClockMHz: float64(mhz), VoltLevel: 42}
		if m.at != nil {
			m.at(mhz, m, &b)
		}
		return dashboard{OK: true, Boards: []board{b}, Health: []boardHealth{m.health}}
	}
	t.Cleanup(func() {
		ceilMutate, ceilDash, ceilBox2Hold, ceilBox2Every = oldMut, oldDash, oldHold, oldEvery
		srv.Close()
		*minerWeb, *apiAddr, *authFile = oldWeb, oldAPI, oldAuth
		setCharge("")
		ceilAbort.Store(false)
		ceilRiskOK.Store(false)
		ceilMu.Lock()
		ceilSt = ceilingState{}
		ceilMu.Unlock()
	})
	setCharge("")
	ceilMu.Lock()
	ceilSt = ceilingState{}
	ceilMu.Unlock()
	return m
}

var c67Probe = modelFeatures{ClockControl: true, CeilingProbe: true}
var c67Unlock = modelFeatures{ClockControl: true, CeilingProbe: true, OverclockUnlock: true}

// c67Run starts the probe the way the page does and waits for it to end.
func c67Run(t *testing.T, m *c67Miner) ceilingState {
	t.Helper()
	if r := l15Post(t, handleCeiling, "/api/ceiling", `{"start":true,"accept_risk":true}`); r["ok"] != true {
		t.Fatalf("the probe didn't start: %v", r)
	}
	st := l15WaitProbe(t)
	if getCharge() != "" {
		t.Fatalf("the probe left %q in charge", getCharge())
	}
	if hi := clockWindowHi(); hi != freqMax && !ocUnlocked() {
		t.Fatalf("after the probe the clock window is %d MHz, want the stock %d", hi, freqMax)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.writes) == 0 {
		t.Fatal("the probe wrote nothing")
	}
	for _, w := range m.writes {
		if w.VrunMv != 42 {
			t.Fatalf("BUG: the probe wrote V code %d (%+v): it must keep the stock Hashrate code 42", w.VrunMv, w)
		}
	}
	if last := m.writes[len(m.writes)-1]; last.FreqMHz != 690 || last.VrunMv != 42 {
		t.Fatalf("BUG: the probe ended on %+v, not the stock Hashrate plan 690/42 (writes %v)", last, m.writes)
	}
	if pp, _ := resolveRunningPlan(m.setting); pp.FreqMHz != 690 || pp.VrunMv != 42 {
		t.Fatalf("BUG: the miner runs %+v after the probe", pp)
	}
	return st
}

func c67MaxWrite(m *c67Miner) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	hi := 0
	for _, w := range m.writes {
		hi = max(hi, w.FreqMHz)
	}
	return hi
}

// Everything off (the shipped default): the probe and the unlock refuse, and nothing widens the window — not a
// stale acknowledgment on disk, not the probe's charge.
func TestC67OffIsInert(t *testing.T) {
	m := c67Setup(t, modelFeatures{})
	for _, c := range []struct {
		h    http.HandlerFunc
		body string
	}{{handleCeiling, `{"start":true,"accept_risk":true}`}, {handleOverclock, `{"acknowledge":true}`}} {
		if r := l15Post(t, c.h, "/x", c.body); r["ok"] != false || r["disabled"] != true {
			t.Fatalf("%s with the gates off: %v", c.body, r)
		}
	}
	if freqHardMax != 690 {
		t.Fatalf("unlocked ceiling %d with OverclockUnlock off, want 690", freqHardMax)
	}
	saveOverclock(overclockState{Acknowledged: true, At: "old"})
	setCharge("ceiling")
	ceilRiskOK.Store(true)
	if hi := clockWindowHi(); hi != 690 {
		t.Fatalf("BUG: with the gates off the clock window is %d MHz", hi)
	}
	if err := box2OverclockErr(powerplan{FreqMHz: 700, VrunMv: 42}, powerplan{FreqMHz: 690, VrunMv: 42}); err == nil {
		t.Fatal("BUG: a 700 MHz write passes with the gates off")
	}
	if ocUnlocked() {
		t.Fatal("BUG: a stale acknowledgment unlocks with OverclockUnlock off")
	}
	// the backend refuses it too (mutatePlanBox2: stock tuples only, and box2OverclockErr first)
	if _, err := mutatePlanEx(func(p *powerplan) { p.FreqMHz = 700 }, true, noGen); err == nil {
		t.Fatal("BUG: mutatePlanBox2 wrote 700 MHz")
	}
	if len(m.writes) != 0 {
		t.Fatalf("writes with the gates off: %v", m.writes)
	}
}

// The probe's start conditions: the risk accepted, clock control on, and the stock Hashrate plan running.
func TestC6ProbeStartConditions(t *testing.T) {
	m := c67Setup(t, c67Probe)
	if r := l15Post(t, handleCeiling, "/api/ceiling", `{"start":true}`); r["ok"] != false || !strings.Contains(r["err"].(string), "risk") {
		t.Fatalf("started without the risk accepted: %v", r)
	}
	m.mu.Lock()
	m.setting["manual"], m.setting["manualPowerplan"] = true, "525 MHz 61 V 40 RPM 59 RPM"
	m.mu.Unlock()
	if r := l15Post(t, handleCeiling, "/api/ceiling", `{"start":true,"accept_risk":true}`); r["ok"] != false || !strings.Contains(r["err"].(string), "Stock High") {
		t.Fatalf("started from the Low-power plan: %v", r)
	}
	m.mu.Lock()
	m.setting["manualPowerplan"] = "690 MHz 42 V 50 RPM 41 RPM"
	m.health.Chips = 35
	m.mu.Unlock()
	if r := l15Post(t, handleCeiling, "/api/ceiling", `{"start":true,"accept_risk":true}`); r["ok"] != false || !strings.Contains(r["err"].(string), "chips") {
		t.Fatalf("started on a board short of a chip: %v", r)
	}
	model.Feat.ClockControl = false
	if r := l15Post(t, handleCeiling, "/api/ceiling", `{"start":true,"accept_risk":true}`); r["ok"] != false {
		t.Fatalf("started with clock control off: %v", r)
	}
	if len(m.writes) != 0 || getCharge() != "" {
		t.Fatalf("a refused start wrote %v / took %q", m.writes, getCharge())
	}
}

// A clean unit: every 10 MHz step to the 750 MHz ceiling holds; each was applied at V code 42; back on 690.
func TestC6ProbeStepsAndReturnsToStock(t *testing.T) {
	m := c67Setup(t, c67Probe)
	st := c67Run(t, m)
	var got []int
	for _, w := range m.writes {
		got = append(got, w.FreqMHz)
	}
	if want := "[700,710,720,730,740,750,690]"; mustJSON(got) != want {
		t.Fatalf("steps %v, want %s", got, want)
	}
	if a := st.Axes[0]; a.State != "done" || a.Cap != 750 || !a.Held || a.HardMax != c67TestCeil {
		t.Fatalf("result %+v", a)
	}
}

// Each guard stops the probe at the step that trips it; the result is the last clean step; the miner is back
// on the stock Hashrate plan and nothing past the failing step was ever written.
func TestC6ProbeGuards(t *testing.T) {
	for _, c := range []struct {
		name   string
		trip   int
		why    string
		set    func(m *c67Miner)
		atStep func(mhz int, m *c67Miner, b *board)
	}{
		{name: "heat 82 twice", trip: 750, why: "heat", set: func(m *c67Miner) {
			m.temp = func(mhz int) float64 { return 70 + float64(mhz-690)/5 } // 750 → 82
		}},
		{name: "heat 84 once", trip: 720, why: "heat", set: func(m *c67Miner) {
			m.temp = func(mhz int) float64 {
				if mhz >= 720 {
					return 84.5
				}
				return 76
			}
		}},
		{name: "hw errors", trip: 720, why: "hardware errors", set: func(m *c67Miner) {
			m.hwAdd = func(mhz int) int {
				if mhz >= 720 {
					return 20 // 20 per 1000 nonces: 1.96 %
				}
				return 1
			}
		}},
		{name: "chip re-init", trip: 730, why: "chip dropout", atStep: func(mhz int, m *c67Miner, b *board) {
			if mhz >= 730 {
				m.health.Reinit24 = 1
			}
		}},
		{name: "chip lost", trip: 740, why: "chip dropout", atStep: func(mhz int, m *c67Miner, b *board) {
			if mhz >= 740 {
				m.health.Chips = 34
			}
		}},
		{name: "board dead", trip: 700, why: "dropout", atStep: func(mhz int, m *c67Miner, b *board) {
			if mhz >= 700 {
				b.Alive = false
			}
		}},
		{name: "no temperature", trip: 710, why: "no board temperature", atStep: func(mhz int, m *c67Miner, b *board) {
			if mhz >= 710 {
				b.Temps = []float64{-150, -150}
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := c67Setup(t, c67Probe)
			if c.set != nil {
				c.set(m)
			}
			m.at = c.atStep
			st := c67Run(t, m)
			a := st.Axes[0]
			if a.State != "done" || a.Cap != c.trip-10 || !strings.Contains(a.Note, c.why) {
				t.Fatalf("result %+v; want clean to %d, failed at %d for %q; log:\n%s", a, c.trip-10, c.trip, c.why, strings.Join(st.Log, "\n"))
			}
			if hi := c67MaxWrite(m); hi != c.trip {
				t.Fatalf("BUG: the probe wrote up to %d MHz, it must stop at the failing %d", hi, c.trip)
			}
		})
	}
}

// Stop (or a safety stop / a re-lock) mid-run: no result, back on the stock plan.
func TestC6ProbeStopReturnsToStock(t *testing.T) {
	m := c67Setup(t, c67Probe)
	m.at = func(mhz int, _ *c67Miner, _ *board) {
		if mhz == 720 {
			ceilAbort.Store(true) // the Stop button
		}
	}
	st := c67Run(t, m)
	if a := st.Axes[0]; a.State != "aborted" {
		t.Fatalf("a stopped probe reports %+v", a)
	}
	if hi := c67MaxWrite(m); hi != 720 {
		t.Fatalf("stopped at 720, wrote up to %d", hi)
	}
}

// The unlock: the window opens to c67TestCeil only with OverclockUnlock on and accepted, the voltage code stays
// stock above 690 unless VoltageControl is on, a probe cap narrows it, and re-lock closes it. SC Lite unchanged.
func TestC7UnlockWindow(t *testing.T) {
	c67Setup(t, c67Probe)
	if freqHardMax != 690 || clockWindowHi() != 690 {
		t.Fatalf("probe only: hard %d window %d, want 690", freqHardMax, clockWindowHi())
	}
	if r := l15Post(t, handleOverclock, "/api/overclock", `{"acknowledge":true}`); r["disabled"] != true {
		t.Fatalf("unlock with OverclockUnlock off: %v", r)
	}
	model.Feat = c67Unlock
	setWindowsFromModel()
	if freqHardMax != c67TestCeil || clockWindowHi() != 690 {
		t.Fatalf("unlock on, not accepted: hard %d window %d", freqHardMax, clockWindowHi())
	}
	if r := l15Post(t, handleOverclock, "/api/overclock", `{"acknowledge":true}`); r["ok"] != true {
		t.Fatalf("accept: %v", r)
	}
	if hi := clockWindowHi(); hi != c67TestCeil {
		t.Fatalf("unlocked window %d, want %d", hi, c67TestCeil)
	}
	from := powerplan{FreqMHz: 690, VrunMv: 42}
	for _, c := range []struct {
		p  powerplan
		ok bool
	}{
		{powerplan{FreqMHz: 700, VrunMv: 42}, true},
		{powerplan{FreqMHz: 750, VrunMv: 42}, true},
		{powerplan{FreqMHz: 760, VrunMv: 42}, false}, // past the ceiling
		{powerplan{FreqMHz: 740, VrunMv: 61}, false}, // not the stock Hashrate code
		{powerplan{FreqMHz: 740, VrunMv: 40}, false},
	} {
		if err := box2OverclockErr(c.p, from); (err == nil) != c.ok {
			t.Fatalf("%+v: err %v, want ok=%v", c.p, err, c.ok)
		}
	}
	model.Feat.VoltageControl = true // C4's range rules apply instead (not this lane's)
	if err := box2OverclockErr(powerplan{FreqMHz: 740, VrunMv: 44}, from); err != nil {
		t.Fatalf("with voltage control on the code isn't pinned here: %v", err)
	}
	model.Feat.VoltageControl = false
	if r := l15Post(t, handleOverclock, "/api/overclock", `{"caps":{"freq":720}}`); r["ok"] != true {
		t.Fatalf("caps: %v", r)
	}
	if hi := clockWindowHi(); hi != 720 {
		t.Fatalf("window with a 720 cap: %d", hi)
	}
	if r := l15Post(t, handleOverclock, "/api/overclock", `{"revoke":true}`); r["ok"] != true || clockWindowHi() != 690 {
		t.Fatalf("re-lock: %v, window %d", r, clockWindowHi())
	}
	// the SC Lite: its own window and ceilings, untouched by any of this
	model = profSCLite
	setWindowsFromModel()
	if freqHardMax != 900 || freqMax != 700 {
		t.Fatalf("SC Lite window changed: %d/%d", freqMax, freqHardMax)
	}
	setCharge("ceiling")
	ceilRiskOK.Store(true)
	if _, ok := ceilBox2Window(); ok {
		t.Fatal("BUG: the Box II probe window opens on the SC Lite")
	}
}

// With the unlock accepted the probe needs no per-run risk flag, and runs to the (capped) unlocked ceiling.
func TestC7ProbeFollowsUnlockedCeiling(t *testing.T) {
	m := c67Setup(t, c67Unlock)
	saveOverclock(overclockState{Acknowledged: true, At: "x", Caps: map[string]int{"freq": 730}})
	if r := l15Post(t, handleCeiling, "/api/ceiling", `{"start":true}`); r["ok"] != true {
		t.Fatalf("start with the unlock accepted: %v", r)
	}
	st := l15WaitProbe(t)
	if a := st.Axes[0]; a.HardMax != 730 || a.Cap != 730 {
		t.Fatalf("result %+v, want the 730 cap as the top", a)
	}
	if hi := c67MaxWrite(m); hi != 730 {
		t.Fatalf("wrote up to %d", hi)
	}
}
