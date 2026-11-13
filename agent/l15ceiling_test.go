package main

// L15-ceiling: the firmware-limit probe and the clock window. The fake stock backend stores every plan verbatim
// (as Goldshell's firmware does: it caps nothing), so any "clamp" the probe reports is not the firmware's.

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

type l15Stock struct {
	mu      sync.Mutex
	setting map[string]any
	puts    []string
	// failPut: for a PUT of this plan, 0 = store it, -1 = drop the connection (backend restarting), else an HTTP status
	failPut func(plan string) int
}

func l15Setup(t *testing.T) *l15Stock {
	t.Helper()
	dir := t.TempDir()
	*authFile = filepath.Join(dir, "auth.json")
	*presetPath = filepath.Join(dir, "presets.json")
	*fanPath = filepath.Join(dir, "fan.json")
	oldAPI := *apiAddr
	*apiAddr = "127.0.0.1:47301" // nothing listens: no cgminer, no temperatures
	wakeWait = 0
	t.Cleanup(func() { wakeWait = 20 * time.Second; setCharge(""); *apiAddr = oldAPI; ceilAbort.Store(false) })
	fanGuardMu.Lock()
	fanGuardSt, fanGuardOK = fanGuardState{}, true
	fanGuardMu.Unlock()
	fanSysDir = filepath.Join(dir, "fans")
	s := &l15Stock{}
	json.Unmarshal([]byte(`{"manual":true,"tempcontrol":true,"select":0,
		"manualPowerplan":"650 MHz 9100 V 60 RPM 60 RPM PV 9400",
		"powerplans":[{"info":"615 MHz 9100 V 40 RPM 40 RPM PV 9400","level":0},{"info":"0 MHz 0 V 100 RPM 100 RPM","level":3}]}`), &s.setting)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/user/login"):
			json.NewEncoder(w).Encode(map[string]any{"JWT Token": "tok"})
		case r.Method == "GET":
			s.mu.Lock()
			json.NewEncoder(w).Encode(s.setting)
			s.mu.Unlock()
		case r.Method == "PUT":
			var m map[string]any
			b, _ := io.ReadAll(r.Body)
			json.Unmarshal(b, &m)
			mp, _ := m["manualPowerplan"].(string)
			s.mu.Lock()
			f := s.failPut
			s.mu.Unlock()
			if f != nil {
				switch c := f(mp); {
				case c < 0:
					if hj, ok := w.(http.Hijacker); ok {
						conn, _, _ := hj.Hijack()
						conn.Close()
					}
					return
				case c > 0:
					w.WriteHeader(c)
					return
				}
			}
			s.mu.Lock()
			s.puts = append(s.puts, mp)
			s.setting = m
			s.mu.Unlock()
			io.WriteString(w, `{"status":0}`)
		}
	}))
	t.Cleanup(srv.Close)
	*minerWeb = strings.TrimPrefix(srv.URL, "http://")
	jwtToken = ""
	authMu.Lock()
	auth.MinerPass = "pw"
	authMu.Unlock()
	setCharge("")
	return s
}

func l15Post(t *testing.T, h http.HandlerFunc, path, body string) map[string]any {
	t.Helper()
	rr := httptest.NewRecorder()
	h(rr, httptest.NewRequest("POST", path, strings.NewReader(body)))
	var out map[string]any
	json.Unmarshal(rr.Body.Bytes(), &out)
	return out
}

func l15WaitProbe(t *testing.T) ceilingState {
	t.Helper()
	for i := 0; i < 1200; i++ {
		if s := ceilSnapshot(); !s.Running {
			return s
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("the probe didn't finish")
	return ceilingState{}
}

// the caps the page's "Tighten" button would post for this result (renderCeiling: clamped axes, actual||cap)
func l15UICaps(s ceilingState) map[string]int {
	caps := map[string]int{}
	for _, a := range s.Axes {
		if a.Clamped && (a.Actual != 0 || a.Cap != 0) {
			v := a.Actual
			if v == 0 {
				v = a.Cap
			}
			caps[a.Name] = v
		}
	}
	return caps
}

// 1. Probe again after "Tighten": the panel's own window (the recorded cap) refuses every step past it, and the
// probe reports that as Goldshell's firmware rejecting the value.
func TestL15ReprobeAfterTightenBlamesFirmware(t *testing.T) {
	s := l15Setup(t)
	if err := saveOverclock(overclockState{Acknowledged: true, At: "x", Caps: map[string]int{"freq": 800}}); err != nil {
		t.Fatal(err)
	}
	if r := l15Post(t, handleCeiling, "/api/ceiling", `{"start":true}`); r["ok"] != true {
		t.Fatalf("probe didn't start: %v", r)
	}
	st := l15WaitProbe(t)
	for _, p := range s.puts {
		if pp, ok := parsePowerplan(p); ok && pp.FreqMHz > 800 {
			t.Fatalf("setup: %q reached the miner past the 800 MHz window", p)
		}
	}
	a := st.Axes[0]
	if a.Clamped || strings.Contains(strings.Join(st.Log, "\n"), "firmware rejected") {
		t.Fatalf("BUG: the fake firmware stores every value, yet the probe reports a firmware cap (clamped=%v at %d, note %q); log:\n%s",
			a.Clamped, a.ClampAt, a.Note, strings.Join(st.Log, "\n"))
	}
}

// 2. Re-lock while the probe runs: every later step is refused by the re-locked window, read as "firmware rejected
// 725 — cap is 700". The page then offers "Tighten" (caps freq 700), the caps endpoint takes it while locked, and
// the next unlock keeps it: the "unlocked" window stops at 700 MHz for good (until another re-lock).
func TestL15RelockDuringProbeLeavesUnlockAt700(t *testing.T) {
	l15Setup(t)
	saveOverclock(overclockState{Acknowledged: true, At: "x"})
	base := powerplan{FreqMHz: 650, VrunMv: 9100, PVMv: 9400, Fan1: 60, Fan2: 60}
	setCharge("ceiling")
	ceilGen.Store(safetyGen.Load())
	ceilAbort.Store(false)
	ceilMu.Lock()
	ceilSt = ceilingState{Running: true, Axes: []ceilingAxis{{Name: "freq", Label: "Frequency", Unit: "MHz", SafeMax: freqMax, HardMax: freqHardMax, Step: 25, State: "todo"}}}
	ceilMu.Unlock()
	// the owner presses Re-lock as the probe starts stepping
	if r := l15Post(t, handleOverclock, "/api/overclock", `{"revoke":true}`); r["ok"] != true {
		t.Fatalf("re-lock: %v", r)
	}
	runCeilingProbe(base)
	st := ceilSnapshot()
	caps := l15UICaps(st)
	if len(caps) > 0 {
		l15Post(t, handleOverclock, "/api/overclock", mustJSON(map[string]any{"caps": caps})) // "Tighten"
	}
	l15Post(t, handleOverclock, "/api/overclock", `{"acknowledge":true}`) // unlock again
	if hi := clockWindowHi(); hi <= freqMax {
		t.Fatalf("BUG: after a re-lock during the probe (result %q, Tighten caps %v) and a new unlock, the clock window stops at %d MHz",
			st.Axes[0].Note, caps, hi)
	}
}

// 3. The stock backend drops the connection for one step (it may be restarting after the last plan change): the
// probe's 3 quick tries fail and it records Goldshell's cap just below that step.
func TestL15TransientBackendErrorIsNoCap(t *testing.T) {
	s := l15Setup(t)
	saveOverclock(overclockState{Acknowledged: true, At: "x"})
	var n int
	s.failPut = func(plan string) int {
		if strings.HasPrefix(plan, "775 MHz") && n < 3 {
			n++
			return -1
		}
		return 0
	}
	if r := l15Post(t, handleCeiling, "/api/ceiling", `{"start":true}`); r["ok"] != true {
		t.Fatalf("probe didn't start: %v", r)
	}
	st := l15WaitProbe(t)
	if caps := l15UICaps(st); len(caps) > 0 {
		t.Fatalf("BUG: a backend that didn't answer for ~2 s is recorded as Goldshell's cap: Tighten would set %v (note %q)", caps, st.Axes[0].Note)
	}
}

// 3b. (kept by the fix) a real refusal from the firmware (an HTTP 4xx answer to the write) still reads as its cap.
func TestL15FirmwareRefusalStillCap(t *testing.T) {
	s := l15Setup(t)
	saveOverclock(overclockState{Acknowledged: true, At: "x"})
	s.failPut = func(plan string) int {
		if pp, ok := parsePowerplan(plan); ok && pp.FreqMHz > 800 {
			return 400
		}
		return 0
	}
	if r := l15Post(t, handleCeiling, "/api/ceiling", `{"start":true}`); r["ok"] != true {
		t.Fatalf("probe didn't start: %v", r)
	}
	st := l15WaitProbe(t)
	if caps := l15UICaps(st); caps["freq"] != 800 {
		t.Fatalf("a firmware that refuses 825 MHz: caps %v (note %q)", caps, st.Axes[0].Note)
	}
	// and the end put the starting plan back
	if p := s.puts[len(s.puts)-1]; !strings.HasPrefix(p, "650 MHz") {
		t.Fatalf("not restored: last write %q", p)
	}
}

// 4. Caps posted while locked are refused (they'd only take effect at a later unlock nobody tied them to).
func TestL15CapsRefusedWhileLocked(t *testing.T) {
	l15Setup(t)
	if r := l15Post(t, handleOverclock, "/api/overclock", `{"caps":{"freq":700}}`); r["ok"] == true {
		l15Post(t, handleOverclock, "/api/overclock", `{"acknowledge":true}`)
		t.Fatalf("BUG: caps accepted while locked; after unlocking, the window is %d MHz", clockWindowHi())
	}
}

func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }
