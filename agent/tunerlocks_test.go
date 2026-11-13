package main

// Concurrency harness for the tuner (tuner-sweep/locks). Catches the v1.5.15 class of bug generally:
//   - every engine run goes through runEngine, which fails (with every goroutine's stack) if Run doesn't
//     return within a real-time limit: a self-deadlock anywhere in the run, in any hook, shows as a timeout;
//   - every hook checks, with TryLock, that the run doesn't call it while holding one of the panel's locks
//     (nothing else runs in these tests, so a lock that can't be taken is held by the run itself);
//   - the chip-trim hooks are the REAL closures (cwSetTrim -> chiptrim file -> reapplyTrims -> the miner API),
//     against a fake cgminer API and a fresh hook stats file;
//   - Stop is driven at every phase of a run (settle, test, trim, retest after a trim, trim check, confirm,
//     presets, restore) and the run must end, release the trims it didn't keep and go back to the baseline.

import (
	"bufio"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- fake cgminer API (devs / pgaset / summary) ----

func fakeCgminer(t *testing.T) (calls func() []string) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var got []string
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				c.SetDeadline(time.Now().Add(2 * time.Second))
				var q map[string]string
				if json.NewDecoder(bufio.NewReader(c)).Decode(&q) != nil {
					return
				}
				mu.Lock()
				got = append(got, q["command"]+" "+q["parameter"])
				mu.Unlock()
				switch q["command"] {
				case "devs":
					c.Write([]byte(`{"STATUS":[{"STATUS":"S"}],"DEVS":[{"ID":2,"PGA":0}]}` + "\x00"))
				case "pgaset":
					c.Write([]byte(`{"STATUS":[{"STATUS":"S","Msg":"set OK"}]}` + "\x00"))
				default:
					c.Write([]byte(`{"STATUS":[{"STATUS":"S"}],"SUMMARY":[{"Total MH":1}]}` + "\x00"))
				}
			}(c)
		}
	}()
	old := *apiAddr
	*apiAddr = ln.Addr().String()
	t.Cleanup(func() { ln.Close(); *apiAddr = old })
	return func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), got...) }
}

// realTrimHooks: a fresh hook stats file (chain 2, 46 chips, 625 MHz), an empty trims file, the chip hook
// switched on, and the auto-trim state reset afterwards.
func realTrimHooks(t *testing.T) {
	dir := t.TempDir()
	oldAuth := *authFile // (other tests leave it pointing at a removed folder)
	*authFile = filepath.Join(dir, "auth.json")
	t.Cleanup(func() { *authFile = oldAuth })
	changeMu.Lock()
	changeAt = time.Time{} // a setting change after the stats file was written makes reapplyTrims refuse
	changeMu.Unlock()
	oldStats := chipStatsPath
	chipStatsPath = filepath.Join(dir, "chipstats.json")
	st := map[string]any{"t": time.Now().Unix(), "chains": []map[string]any{{"chain": 2, "chips": 46, "base10": 6250,
		"clock10": make([]int, 47), "nonces": make([]int, 47)}}}
	b, _ := json.Marshal(st)
	if err := os.WriteFile(chipStatsPath, b, 0o644); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(chipHookFlag(), []byte("on\n"), 0o644)
	writeChipTrims(nil)
	cwMu.Lock()
	oldCw, oldLoaded := cwSt, cwLoaded
	cwSt, cwLoaded = cwState{NoHelp: map[string]int64{}}, true
	cwMu.Unlock()
	t.Cleanup(func() {
		chipStatsPath = oldStats
		os.Remove(chipHookFlag())
		writeChipTrims(nil)
		os.Remove(chipWatchPath())
		cwMu.Lock()
		cwSt, cwLoaded = oldCw, oldLoaded
		cwMu.Unlock()
	})
}

// ---- lock guard ----

type lockGuard struct {
	mu   sync.Mutex
	bad  map[string]bool
	e    *tunerEngine
	skip bool // the test itself holds a lock on purpose
}

// check: none of the panel's locks is held while hook runs.
func (g *lockGuard) check(hook string) {
	locks := []struct {
		name string
		m    interface {
			TryLock() bool
			Unlock()
		}
	}{
		{"e.mu", &g.e.mu}, {"chipTrimMu", &chipTrimMu}, {"cwMu", &cwMu}, {"tunerMu", &tunerMu},
		{"chargeMu", &chargeMu}, {"notifyMu", &notifyMu},
		{"ceilMu", &ceilMu}, {"pgMu", &pgMu}, {"fpMu", &fpMu}, {"hgMu", &hgMu}, {"lastCurveMu", &lastCurveMu},
	}
	for _, l := range locks {
		if l.m.TryLock() {
			l.m.Unlock()
			continue
		}
		g.mu.Lock()
		if g.bad == nil {
			g.bad = map[string]bool{}
		}
		g.bad[hook+" called with "+l.name+" held"] = true
		g.mu.Unlock()
	}
}

func (g *lockGuard) failures() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []string
	for k := range g.bad {
		out = append(out, k)
	}
	return out
}

// guardHooks wraps every hook of e so it checks the locks first (and then calls onHook, for the stop tests).
func guardHooks(e *tunerEngine, g *lockGuard, onHook func(hook string)) {
	h := func(name string) {
		g.check(name)
		if onHook != nil {
			onHook(name)
		}
	}
	rE, rT, rG, rEl, aP, tG, tS, hS, sl := e.readErrors, e.readTemp, e.readGHS, e.elapsed, e.applyPlan, e.trimGet, e.trimSet, e.hookStats, e.sleep
	e.readErrors = func() (map[ckey]int, error) { h("readErrors"); return rE() }
	e.readTemp = func() (float64, bool) { h("readTemp"); return rT() }
	e.readGHS = func() (float64, bool) { h("readGHS"); return rG() }
	e.elapsed = func() (int, bool) { h("elapsed"); return rEl() }
	e.applyPlan = func(p planMV) error { h("applyPlan"); return aP(p) }
	e.trimGet = func() map[[2]int]float64 { h("trimGet"); return tG() }
	e.trimSet = func(c, i int, m float64) error { h("trimSet"); return tS(c, i, m) }
	e.hookStats = func() (chipStatsFile, bool) { h("hookStats"); return hS() }
	e.sleep = func(d time.Duration) { h("sleep"); sl(d) }
	if e.readWork != nil {
		w := e.readWork
		e.readWork = func() (float64, bool) { h("readWork"); return w() }
	}
	if e.adoptTrims != nil {
		a := e.adoptTrims
		e.adoptTrims = func(ts []chipTrim, why string) { h("adoptTrims"); a(ts, why) }
	}
	if e.applyFan != nil {
		f := e.applyFan
		e.applyFan = func(p int) error { h("applyFan"); return f(p) }
	}
	em := e.emit
	e.emit = func(s string) {
		h("emit")
		if em != nil {
			em(s)
		}
	}
	e.onApply = func(planMV) { h("onApply") }
	e.onStart = func() { h("onStart") }
}

// runEngine runs e.Run and fails with every goroutine's stack if it doesn't return in limit.
func runEngine(t *testing.T, e *tunerEngine, limit time.Duration) bool {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); e.Run() }()
	select {
	case <-done:
		return true
	case <-time.After(limit):
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		t.Errorf("tuner run didn't end within %v (deadlock or a step that ignores Stop). Goroutines:\n%s", limit, buf[:n])
		return false
	}
}

// ---- the engine: sim miner + REAL chip-trim closures ----

// a weak chip (icinfo b2 index 5 = hook chain 2 chip 6) needs 9000 mV at 625 MHz; slowed 25 MHz it is clean
// down to 8800 mV. The sim reads the trims from the real trims file the real closures write.
func realTrimEngine(t *testing.T) (*tunerEngine, *simState, *lockGuard) {
	fakeCgminer(t)
	realTrimHooks(t)
	cfg := fastCfg()
	cfg.ChipTrim, cfg.ChipTrimMax = true, 3
	cfg.MinMV, cfg.MaxMV, cfg.MVStep = 8800, 9300, 100
	cfg.HoldMin = 2
	cfg.SettleMin = 1
	cfg.ConfirmWin, cfg.ConfirmX = true, 1
	cfg.BuildPresets, cfg.PresetMin = true, 2
	e, s := newSimEngine(cfg, 9000, ckey{2, 5})
	real := newTunerEngine(cfg)
	real.sleep, real.now = e.sleep, e.now // (the real closures wait on their engine's clock: the sim's)
	e.trimGet, e.hookStats, e.adoptTrims = real.trimGet, real.hookStats, real.adoptTrims
	rs := real.trimSet
	e.trimSet = func(chain, chip int, mhz float64) error {
		err := rs(chain, chip, mhz) // cwSetTrim (takes chipTrimMu) + reapplyTrims (miner API)
		s.mu.Lock()
		s.failBelowMV = 9000
		for _, tr := range readChipTrims() {
			if tr.Chain == 2 && tr.Chip == 6 && tr.MHz <= -25 {
				s.failBelowMV = 8800
			}
		}
		s.mu.Unlock()
		return err
	}
	g := &lockGuard{e: e}
	return e, s, g
}

func noPresetsSaved(t *testing.T) {
	// presets are saved to the panel's folder: keep the test's own
	p := *presetPath
	b, err := os.ReadFile(p)
	t.Cleanup(func() {
		if err == nil {
			os.WriteFile(p, b, 0o644)
		} else {
			os.Remove(p)
		}
	})
}

// A whole run (map, chip trims, trim check, confirm, presets) with the real trim closures: it ends, no hook is
// called with a lock held, the kept trim is in the trims file and handed to auto-trim.
func TestLocksFullRunRealTrimClosures(t *testing.T) {
	noPresetsSaved(t)
	e, _, g := realTrimEngine(t)
	seen := map[string]bool{}
	var smu sync.Mutex
	guardHooks(e, g, func(h string) {
		e.mu.Lock()
		for _, k := range []string{"trimcheck", "confirm", "presets"} {
			if s := e.stepByKey(k); s != nil && s.State == "run" {
				smu.Lock()
				seen[k] = true
				smu.Unlock()
			}
		}
		e.mu.Unlock()
	})
	if !runEngine(t, e, 30*time.Second) {
		return
	}
	if f := g.failures(); len(f) > 0 {
		t.Fatalf("hooks called with a lock held: %v", f)
	}
	for _, k := range []string{"trimcheck", "confirm", "presets"} {
		if !seen[k] {
			t.Errorf("phase %s never ran: the test doesn't cover it\n%s", k, strings.Join(e.fullLog, "\n"))
		}
	}
	tr := readChipTrims()
	if len(tr) != 1 || tr[0].Chain != 2 || tr[0].Chip != 6 || tr[0].MHz != -25 {
		t.Fatalf("trims file %+v\n%s", tr, strings.Join(e.fullLog, "\n"))
	}
	cwMu.Lock()
	owned := append([]cwOwned(nil), cwSt.Owned...)
	cwMu.Unlock()
	if len(owned) != 1 || owned[0].Origin != "tuner" {
		t.Fatalf("auto-trim didn't take the kept trim: %+v", owned)
	}
}

// The harness itself catches the v1.5.15 bug class: a trimSet that holds a lock around a call that takes it
// again. (A local mutex stands in for chipTrimMu: the frozen goroutine is leaked and must not hold the real one.)
func TestLocksHarnessCatches1515Deadlock(t *testing.T) {
	e, _, g := realTrimEngine(t)
	var m sync.Mutex                                                                               // chipTrimMu in v1.5.15
	setTrim := func(chain, chip int, mhz float64) error { m.Lock(); defer m.Unlock(); return nil } // cwSetTrim
	e.trimSet = func(chain, chip int, mhz float64) error {                                         // v1.5.15's closure
		m.Lock()
		defer m.Unlock()
		return setTrim(chain, chip, mhz)
	}
	guardHooks(e, g, nil)
	done := make(chan struct{})
	go func() { defer close(done); e.Run() }()
	select {
	case <-done:
		t.Fatal("the v1.5.15 closure should have frozen the run")
	case <-time.After(5 * time.Second):
	}
	e.Stop() // Stop can't end it: the run never looks at the stop channel again
	select {
	case <-done:
		t.Fatal("Stop ended a run frozen in a hook?")
	case <-time.After(time.Second):
	}
	if !e.isRunning() {
		t.Fatal("a frozen run says it isn't running")
	}
	// so the panel's charge ("tuner") is never released: handleTuner clears it only after t.Run() returns
	t.Log("frozen as in v1.5.15; Stop has no effect; the goroutine is leaked (test only)")
}

// ---- Stop at every phase ----

type stopCase struct {
	name string
	at   func(e *tunerEngine, hook string) bool // called with e.mu held
}

func stepRun(e *tunerEngine, key string) bool {
	s := e.stepByKey(key)
	return s != nil && s.State == "run"
}

var stopCases = []stopCase{
	{"baseline settle", func(e *tunerEngine, h string) bool {
		return h == "sleep" && e.phase == "baseline" && e.cur != nil && e.cur.Settling
	}},
	{"baseline watch", func(e *tunerEngine, h string) bool {
		return h == "readErrors" && e.phase == "baseline" && e.cur != nil && !e.cur.Settling
	}},
	{"test settle", func(e *tunerEngine, h string) bool {
		return h == "sleep" && e.phase == "test" && e.cur != nil && e.cur.Settling
	}},
	{"test watch", func(e *tunerEngine, h string) bool {
		return h == "readErrors" && e.phase == "test" && e.cur != nil && !e.cur.Settling && e.att.tag == ""
	}},
	{"inside trimSet", func(e *tunerEngine, h string) bool { return h == "trimSet" }},
	{"retest after a trim (settle)", func(e *tunerEngine, h string) bool {
		return h == "sleep" && e.att.tag == "after a chip trim" && e.cur != nil && e.cur.Settling
	}},
	{"retest after a trim (watch)", func(e *tunerEngine, h string) bool {
		return h == "readErrors" && e.att.tag == "after a chip trim" && e.cur != nil && !e.cur.Settling
	}},
	{"trim check", func(e *tunerEngine, h string) bool { return h == "readErrors" && stepRun(e, "trimcheck") }},
	{"confirm", func(e *tunerEngine, h string) bool { return h == "readErrors" && stepRun(e, "confirm") }},
	{"presets", func(e *tunerEngine, h string) bool { return h == "readErrors" && stepRun(e, "presets") }},
}

func TestLocksStopAtEveryPhase(t *testing.T) {
	for _, sc := range stopCases {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			noPresetsSaved(t)
			e, s, g := realTrimEngine(t)
			var once sync.Once
			fired := false
			var stopAt time.Time
			guardHooks(e, g, func(h string) {
				e.mu.Lock()
				hit := sc.at(e, h)
				e.mu.Unlock()
				if hit {
					once.Do(func() {
						fired = true
						stopAt = e.now()
						e.Stop()
						e.Stop() // twice: no double close
					})
				}
			})
			if !runEngine(t, e, 30*time.Second) {
				return
			}
			if !fired {
				t.Fatalf("never reached %q\n%s", sc.name, strings.Join(e.fullLog, "\n"))
			}
			if f := g.failures(); len(f) > 0 {
				t.Fatalf("hooks called with a lock held: %v", f)
			}
			if e.isRunning() {
				t.Fatal("still running after Run returned")
			}
			// Stop is seen within one poll (+ the restore)
			if d := e.now().Sub(stopAt); d > 2*time.Duration(e.cfg.PollS*float64(time.Second)) {
				t.Errorf("Run took %v of (virtual) time after Stop", d)
			}
			s.mu.Lock()
			last := s.applied[len(s.applied)-1]
			s.mu.Unlock()
			want := e.resolveBaseline()
			if e.confirmed && e.best != nil {
				want = *e.best // a confirmed setting is kept (by design)
			}
			if last.MHz != want.MHz || last.MV != want.MV {
				t.Errorf("not restored: last applied %+v, want %+v", last, want)
			}
			// only trims the run kept may stay (they're handed to auto-trim); a trim made for the cut test is undone
			kept := map[[2]int]float64{}
			for k, v := range e.runTrims {
				kept[k] = v
			}
			for _, tr := range readChipTrims() {
				if kept[[2]int{tr.Chain, tr.Chip}] != tr.MHz {
					t.Errorf("trim %+v left behind (kept by the run: %v)\n%s", tr, kept, strings.Join(e.fullLog, "\n"))
				}
			}
			if st := e.stepByKey("finish"); st == nil || st.State != "cut" {
				t.Errorf("finish not marked cut: %+v", st)
			}
			chipTrimMu.Lock() // free again
			chipTrimMu.Unlock()
		})
	}
}

type fakeBackend struct {
	mu    sync.Mutex
	set   map[string]any
	puts  []map[string]any
	onGet func(n int)
	gets  int
}

func newFakeBackend(t *testing.T, idle bool) *fakeBackend {
	fb := &fakeBackend{}
	sel := 0.0
	if idle {
		sel = 1
	}
	fb.set = map[string]any{"manual": false, "tempcontrol": true, "select": sel,
		"manualPowerplan": "625 MHz 9100 V 50 RPM 50 RPM PV 9400",
		"powerplans": []any{
			map[string]any{"info": "625 MHz 9100 V 50 RPM 50 RPM PV 9400", "level": float64(0)},
			map[string]any{"info": "0 MHz 0 V 100 RPM 100 RPM", "level": float64(1)},
		}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/user/login"):
			json.NewEncoder(w).Encode(map[string]any{"JWT Token": "tok"})
		case r.Method == "GET" && r.URL.Path == "/mcb/setting":
			fb.mu.Lock()
			fb.gets++
			n, f := fb.gets, fb.onGet
			b, _ := json.Marshal(fb.set)
			fb.mu.Unlock()
			if f != nil {
				f(n)
			}
			w.Write(b)
		case r.Method == "PUT" && r.URL.Path == "/mcb/setting":
			var m map[string]any
			b, _ := io.ReadAll(r.Body)
			json.Unmarshal(b, &m)
			fb.mu.Lock()
			fb.puts = append(fb.puts, m)
			for k, v := range m {
				fb.set[k] = v
			}
			fb.mu.Unlock()
			io.WriteString(w, `{"status":0}`)
		default:
			w.WriteHeader(404)
		}
	}))
	oldWeb, oldWake := *minerWeb, wakeWait
	*minerWeb = strings.TrimPrefix(srv.URL, "http://")
	wakeWait = 10 * time.Millisecond
	jwtToken = ""
	authMu.Lock()
	auth.MinerPass = "pw"
	authMu.Unlock()
	t.Cleanup(func() { srv.Close(); *minerWeb, wakeWait = oldWeb, oldWake; jwtToken = "" })
	return fb
}

func (fb *fakeBackend) idle() bool {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	return minerIdle(fb.set)
}
