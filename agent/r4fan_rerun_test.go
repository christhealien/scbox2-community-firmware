package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// r4-fancheck: "Re-run Goldshell's fan test" (fanTestRerunSteps). These run the real steps, so each takes
// ~95 s (the 90 s wait for Goldshell's start-up fan test is fixed in the code). Set R4_SLOW=1 to run them.

type r4Miner struct {
	mu   sync.Mutex
	set  map[string]any
	puts []string // what each PUT selected: "idle" or the plan
}

func (m *r4Miner) state() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if minerIdle(m.set) {
		return "idle"
	}
	pp, _ := resolveRunningPlan(m.set)
	return buildPowerplan(pp)
}

func r4RerunEnv(t *testing.T) *r4Miner {
	if os.Getenv("R4_SLOW") == "" {
		t.Skip("slow (95 s): R4_SLOW=1")
	}
	m := &r4Miner{set: map[string]any{"manual": true, "tempcontrol": true, "select": float64(0),
		"manualPowerplan": "650 MHz 9100 V 50 RPM 50 RPM PV 9400",
		"powerplans": []any{
			map[string]any{"info": "615 MHz 9100 V 40 RPM 40 RPM PV 9400", "level": float64(0)},
			map[string]any{"info": "0 MHz 0 V 100 RPM 100 RPM", "level": float64(3)}}}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/user/login"):
			json.NewEncoder(w).Encode(map[string]any{"JWT Token": "tok"})
		case r.Method == "GET":
			m.mu.Lock()
			json.NewEncoder(w).Encode(m.set)
			m.mu.Unlock()
		case r.Method == "PUT":
			b, _ := io.ReadAll(r.Body)
			var s map[string]any
			json.Unmarshal(b, &s)
			m.mu.Lock()
			m.set = s
			m.mu.Unlock()
			m.mu.Lock()
			m.puts = append(m.puts, "put")
			m.mu.Unlock()
			io.WriteString(w, `{"status":0}`)
		default:
			io.WriteString(w, `{}`)
		}
	}))
	t.Cleanup(srv.Close)
	d := t.TempDir()
	oldWeb, oldA, oldB, oldP, oldCmd := *minerWeb, *authFile, backupRoot, procRoot, fanRerunCmd
	*minerWeb = strings.TrimPrefix(srv.URL, "http://")
	*authFile = filepath.Join(d, "auth.json")
	backupRoot = d
	procRoot = filepath.Join(d, "proc")
	os.MkdirAll(filepath.Join(procRoot, "77"), 0o755)
	os.WriteFile(filepath.Join(procRoot, "77", "comm"), []byte("xminerd\n"), 0o644) // the respawned watchdog
	os.WriteFile(filepath.Join(procRoot, "uptime"), []byte("5000.0 1.0"), 0o644)
	fanRerunCmd = "true" // (killall xminerd)
	t.Cleanup(func() {
		*minerWeb, *authFile, backupRoot, procRoot, fanRerunCmd = oldWeb, oldA, oldB, oldP, oldCmd
		setCharge("")
		fanTestAt.Store(0)
	})
	setCharge("preset:Balanced")
	return m
}

func r4Post(h http.HandlerFunc, path string, body any) map[string]any {
	b, _ := json.Marshal(body)
	w := httptest.NewRecorder()
	h(w, httptest.NewRequest("POST", path, bytes.NewReader(b)))
	var out map[string]any
	json.Unmarshal(w.Body.Bytes(), &out)
	return out
}

// The owner presses Idle (or the schedule switches to Idle) while the fan test is being re-run: when the
// re-run ends it puts the setting from before back, and the miner hashes again with "idle" in charge.
func TestR4RerunUndoesIdleChosenMeanwhile(t *testing.T) {
	m := r4RerunEnv(t)
	done := make(chan error, 1)
	go func() { done <- fanTestRerunSteps() }()
	time.Sleep(3 * time.Second)
	if m.state() != "idle" {
		t.Fatalf("setup: the re-run didn't put the miner on Idle: %s", m.state())
	}
	if r := r4Post(handlePresets, "/api/presets", map[string]any{"action": "idle"}); r["ok"] != true {
		t.Fatalf("Idle refused: %v", r)
	}
	if err := <-done; err != nil {
		t.Fatalf("re-run: %v", err)
	}
	if s := m.state(); s != "idle" {
		t.Fatalf("BUG: Idle chosen during the fan test was undone when it ended: the miner runs %q while %q is in charge", s, getCharge())
	}
}

// A preset applied during the re-run is overwritten with the setting from before (the banner keeps naming the
// new preset).
func TestR4RerunOverwritesPresetAppliedMeanwhile(t *testing.T) {
	m := r4RerunEnv(t)
	done := make(chan error, 1)
	go func() { done <- fanTestRerunSteps() }()
	time.Sleep(3 * time.Second)
	// what applying a preset does to the miner and the charge (applyPresetName needs saved presets)
	if _, err := mutatePlan(func(p *powerplan) { p.FreqMHz, p.VrunMv, p.PVMv = 600, 8800, 9100 }); err != nil {
		t.Fatalf("setup: %v", err)
	}
	setCharge("preset:Quiet")
	want := m.state()
	if err := <-done; err != nil {
		t.Fatalf("re-run: %v", err)
	}
	if s := m.state(); s != want {
		t.Fatalf("BUG: the preset applied during the fan test (%s) was replaced by the setting from before (%s); in charge: %q", want, s, getCharge())
	}
}

// A tuning run can be started while the fan test is being re-run (only the reverse is refused).
func TestR4TunerStartsDuringRerun(t *testing.T) {
	r4RerunEnv(t)
	go fanTestRerunSteps()
	time.Sleep(3 * time.Second)
	defer func() {
		tunerMu.Lock()
		e := tunerCur
		tunerMu.Unlock()
		if e != nil {
			e.Stop()
		}
		time.Sleep(time.Second)
	}()
	cfg := defaultTunerConfig()
	cfg.DryRun = true
	r := r4Post(handleTuner, "/api/tuner", map[string]any{"action": "start", "config": cfg})
	if r["ok"] == true {
		t.Fatalf("BUG: a tuning run started while Goldshell's fan test was being re-run (the re-run puts its own setting back when it ends): %v", r)
	}
}

// Nothing changed meanwhile: the setting from before is put back (the re-run's normal end).
func TestR4RerunRestoresWhenUntouched(t *testing.T) {
	m := r4RerunEnv(t)
	before := m.state()
	if err := fanTestRerunSteps(); err != nil {
		t.Fatalf("re-run: %v", err)
	}
	if s := m.state(); s != before {
		t.Fatalf("not put back: %s, want %s", s, before)
	}
}

// killall fails (no watchdog running): the miner is left on Idle with only an activity line.
func TestR4RerunKillFailsLeavesIdle(t *testing.T) {
	m := r4RerunEnv(t)
	fanRerunCmd = "false"
	err := fanTestRerunSteps()
	if err == nil || m.state() != "idle" {
		t.Fatalf("err %v state %s", err, m.state())
	}
	t.Logf("left on Idle, in charge %q, error: %v", getCharge(), err)
}
