package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// L48: the tuner's presets couldn't be saved (the config partition full or unwritable): the run must not tell
// the owner "saved 4 presets" while the Presets tab stays empty.
func TestL48TunerPresetsSaveFailureIsReported(t *testing.T) {
	d := t.TempDir()
	block := filepath.Join(d, "notadir")
	os.WriteFile(block, []byte("x"), 0o644) // a file where the folder should be: every write under it fails
	op := *presetPath
	*presetPath = filepath.Join(block, "presets.json")
	defer func() { *presetPath = op }()

	cfg := mapCfg()
	cfg.BuildPresets = true
	cfg.BaselineMHz, cfg.BaselineMV = 600, 9100
	e, s := newSimEngine(cfg, 0, ckey{2, 5})
	hung := hangGuard(e, s, 72*time.Hour)
	e.Run()
	if *hung {
		t.Fatal("hung")
	}
	if len(loadPresets()) != 0 {
		t.Fatal("test setup: the presets were saved after all")
	}
	log := strings.Join(e.logbuf, "\n")
	if !strings.Contains(log, "presets") {
		t.Fatalf("no presets phase ran:\n%s", log)
	}
	if strings.Contains(log, "RESULT: saved") {
		t.Fatalf("the presets weren't saved (the write failed) but the run says they were:\n%s", log)
	}
	if !strings.Contains(log, "couldn't be saved") {
		t.Fatalf("the failed save isn't reported:\n%s", log)
	}
}

// L48: a tuning run's restart record (tuning.running) can't be written (the config partition full or gone
// read-only). Without it, a power cut or a panel crash mid-run leaves the miner on the run's test setting with
// the run's chip trims on (tunerRecover finds nothing to put back). The limit probe refuses to start in that case
// ("couldn't save the probe's restart record"); a tuning run must not go on either.
func TestL48TunerStartNeedsItsRestartRecord(t *testing.T) {
	var mu sync.Mutex
	puts := 0
	set := map[string]any{"manual": true, "tempcontrol": true, "select": float64(0),
		"manualPowerplan": "650 MHz 9100 V 50 RPM 50 RPM PV 9400",
		"powerplans": []any{
			map[string]any{"info": "615 MHz 9100 V 40 RPM 40 RPM PV 9400", "level": float64(0)},
			map[string]any{"info": "0 MHz 0 V 100 RPM 100 RPM", "level": float64(3)}}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case strings.HasPrefix(r.URL.Path, "/user/login"):
			json.NewEncoder(w).Encode(map[string]any{"JWT Token": "tok"})
		case r.Method == "GET":
			json.NewEncoder(w).Encode(set)
		case r.Method == "PUT":
			b, _ := io.ReadAll(r.Body)
			var s map[string]any
			if json.Unmarshal(b, &s) == nil {
				set = s
			}
			puts++
			io.WriteString(w, `{"status":0}`)
		default:
			io.WriteString(w, `{}`)
		}
	}))
	defer srv.Close()
	d := t.TempDir()
	oldWeb, oldA, oldAPI := *minerWeb, *authFile, *apiAddr
	*minerWeb = strings.TrimPrefix(srv.URL, "http://")
	*authFile = filepath.Join(d, "auth.json")
	*apiAddr = "127.0.0.1:48007" // nothing listens: no chip readings
	defer func() { *minerWeb, *authFile, *apiAddr = oldWeb, oldA, oldAPI; setCharge("") }()
	// the record can't be put in place: its name is taken by a folder that isn't empty (a rename onto it fails)
	os.MkdirAll(filepath.Join(tunerMarkerPath(), "x"), 0o755)
	setCharge("preset:Balanced")

	cfg := defaultTunerConfig()
	cfg.DryRun = false
	r := r4Post(handleTuner, "/api/tuner", map[string]any{"action": "start", "config": cfg})
	tunerMu.Lock()
	e := tunerCur
	tunerMu.Unlock()
	if r["ok"] != true || e == nil {
		t.Skipf("the run didn't start in this setup: %v", r)
	}
	defer func() {
		e.Stop()
		for i := 0; i < 100 && e.isRunning(); i++ {
			time.Sleep(100 * time.Millisecond)
		}
		tunerMu.Lock()
		if tunerCur == e {
			tunerCur = nil
		}
		tunerMu.Unlock()
	}()
	// the run has built its plan and called onStart (the marker write) by the time its first step shows
	for i := 0; i < 50; i++ {
		e.mu.Lock()
		n := len(e.steps)
		e.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	time.Sleep(500 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(tunerMarkerPath(), "x")); err != nil {
		t.Fatalf("test setup: %v", err)
	}
	e.mu.Lock()
	stopWhy, log := e.stopWhy, strings.Join(e.logbuf, "\n")
	e.mu.Unlock()
	if !strings.Contains(stopWhy+log, "restart record") {
		t.Fatalf("BUG: the run goes on without its restart record (a power cut now leaves the miner on its test setting and trims): stop %q\n%s", stopWhy, log)
	}
	if strings.Contains(log, "applied ") {
		t.Fatalf("a test setting was applied before the run stopped:\n%s", log)
	}
	t.Logf("stop: %s\n%s", stopWhy, log)
}

// L48: a panic in a request handler: net/http recovers it and serves on, so the panel keeps running with whatever
// the handler left half done (a lock taken without defer stays held). The server's error log was thrown away
// whole (for the TLS handshake noise), so not even the panic's message and stack reached the log.
func TestL48HandlerPanicIsLogged(t *testing.T) {
	var buf bytes.Buffer
	var bmu sync.Mutex
	ow := log.Writer()
	log.SetOutput(writerFunc(func(p []byte) (int, error) { bmu.Lock(); defer bmu.Unlock(); return buf.Write(p) }))
	defer log.SetOutput(ow)
	srv := newPanelServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]int
		m["l48"] = 1 // a write to a nil map
	}))
	ln, err := net.Listen("tcp", "127.0.0.1:48001")
	if err != nil {
		t.Skip(err)
	}
	go srv.Serve(ln)
	defer srv.Close()
	if resp, err := http.Get("http://127.0.0.1:48001/x"); err == nil {
		resp.Body.Close()
	}
	time.Sleep(200 * time.Millisecond)
	bmu.Lock()
	out := buf.String()
	bmu.Unlock()
	if !strings.Contains(out, "panic serving") || !strings.Contains(out, "nil map") {
		t.Fatalf("a handler's panic left no trace in the log: %q", out)
	}
}

type writerFunc func(p []byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
