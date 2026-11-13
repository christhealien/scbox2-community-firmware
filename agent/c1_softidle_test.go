package main

// Lane C1 (commission key SoftIdle): the Box II soft idle end to end against a fake miner API that answers
// exactly as the Box II intminer does (BFGMiner 5.4.2 message table: pgadisable → STATUS "I" Code 64 "PGA 0
// set disable flag", pgaenable → "I" 63, already → "I" 61/62, not Alive → "E" 65), keeps per-board state
// (Enabled, a growing "Total MH" while hashing, a temperature that rises while hashing and falls when not) and
// can restart (every board enabled again, the counter back to 0), go silent, or ignore the stop.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeBox2Miner struct {
	mu            sync.Mutex
	enabled       []bool
	alive         []bool
	work          []float64 // Total MH
	temp          []float64
	down          bool // the API doesn't answer (the mining program is restarting)
	keepHashing   bool // takes the disable flag but the board keeps hashing (the driver ignores it)
	noFlag        bool // answers "set disable flag" but devs still lists the board enabled
	sent          []string
	restarts      int
	heatsDisabled bool // the temperature keeps rising with the board disabled
	overheat      bool // the miner's own overheat flag on every board
}

func newFakeBox2Miner(n int) *fakeBox2Miner {
	f := &fakeBox2Miner{}
	for i := 0; i < n; i++ {
		f.enabled = append(f.enabled, true)
		f.alive = append(f.alive, true)
		f.work = append(f.work, 5e6)
		f.temp = append(f.temp, 74)
	}
	return f
}

func box2Status(sev string, code int, msg string) []byte {
	return []byte(fmt.Sprintf(`{"STATUS":[{"STATUS":"%s","When":1794232992,"Code":%d,"Msg":"%s","Description":"intminer 5.4.2"}],"id":1}`, sev, code, msg))
}

func (f *fakeBox2Miner) cmd(c string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return nil, fmt.Errorf("dial tcp 127.0.0.1:4028: connect: connection refused")
	}
	if c != "devs" {
		return box2Status("E", 14, "Invalid command"), nil
	}
	var ds []string
	for i := range f.enabled {
		en, st := "Y", "Alive"
		if !f.enabled[i] {
			en = "N"
		}
		if !f.alive[i] {
			st = "Dead"
		}
		mhs := 0.0
		if f.hashing(i) {
			mhs = 6e6
		}
		ds = append(ds, fmt.Sprintf(`{"PGA":%d,"Name":"INCS","ID":%d,"Enabled":"%s","Status":"%s","MHS av":6000000.000,"MHS 20s":%.3f,"Total MH":%.4f,"tstemp-0":%.2f,"tstemp-1":%.2f,"tstemp-2":%.2f,"overheat":%d}`,
			i, i, en, st, mhs, f.work[i], f.temp[i], f.temp[i], f.temp[i]-8, boolInt(f.overheat)))
	}
	return []byte(fmt.Sprintf(`{"STATUS":[{"STATUS":"S","When":1794232969,"Code":9,"Msg":"%d PGA(s)","Description":"intminer 5.4.2"}],"DEVS":[%s],"id":1}`, len(ds), strings.Join(ds, ","))), nil
}

func (f *fakeBox2Miner) param(c, p string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return nil, fmt.Errorf("no answer from the miner (connection refused)")
	}
	f.sent = append(f.sent, c+" "+p)
	id, err := strconv.Atoi(p)
	if err != nil || id < 0 || id >= len(f.enabled) {
		return box2Status("E", 58, fmt.Sprintf("Invalid PGA id %s - range is 0 - %d", p, len(f.enabled)-1)), nil
	}
	switch c {
	case "pgadisable":
		if !f.enabled[id] {
			return box2Status("I", 62, fmt.Sprintf("PGA %d already disabled", id)), nil
		}
		if !f.noFlag {
			f.enabled[id] = false
		}
		return box2Status("I", 64, fmt.Sprintf("PGA %d set disable flag", id)), nil
	case "pgaenable":
		if f.enabled[id] {
			return box2Status("I", 61, fmt.Sprintf("PGA %d already enabled", id)), nil
		}
		if !f.alive[id] {
			return box2Status("E", 65, fmt.Sprintf("PGA %d is not flagged WELL, cannot enable", id)), nil
		}
		f.enabled[id] = true
		return box2Status("I", 63, fmt.Sprintf("PGA %d sent enable message", id)), nil
	}
	return box2Status("E", 14, "Invalid command"), nil
}

func (f *fakeBox2Miner) hashing(i int) bool { return f.alive[i] && (f.enabled[i] || f.keepHashing) }

// advance: sec seconds of mining.
func (f *fakeBox2Miner) advance(sec float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return
	}
	for i := range f.enabled {
		if f.hashing(i) {
			f.work[i] += 6e6 * sec
			f.temp[i] = min(76, f.temp[i]+0.05*sec)
		} else if f.heatsDisabled {
			f.temp[i] += 0.02 * sec
		} else {
			f.temp[i] = max(38, f.temp[i]-0.05*sec)
		}
	}
}

// restart: the launcher relaunched the mining program: every board enabled, the counters from 0.
func (f *fakeBox2Miner) restart() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.restarts++
	for i := range f.enabled {
		f.enabled[i], f.alive[i], f.work[i] = true, true, 0
	}
}

func (f *fakeBox2Miner) count(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, s := range f.sent {
		if strings.HasPrefix(s, prefix) {
			n++
		}
	}
	return n
}

func (f *fakeBox2Miner) isEnabled(i int) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.enabled[i]
}

// c1Env: a Box II with SoftIdle as given, the fake miner installed, a fake clock, the fans fall-back and the
// mining restart recorded, the marker in a temp dir. Everything restored after.
type c1Env struct {
	f        *fakeBox2Miner
	clk      time.Time
	fans     int
	restarts int
}

func newC1Env(t *testing.T, softIdle bool) *c1Env {
	t.Helper()
	m := profSCBox2
	m.Feat.SoftIdle = softIdle
	e := &c1Env{f: newFakeBox2Miner(1), clk: time.Date(2026, 11, 10, 20, 0, 0, 0, time.UTC)}
	oldModel, oldWhy := model, modelWhy
	oldCmd, oldParam := box2APICmd, box2APICmdParam
	oldNow, oldFans, oldRestart, oldOT := box2Now, box2FansMax, box2RestartMining, box2OverTemp
	box2OverTemp = func() bool { return false }
	oldAuth := *authFile
	oldCharge := getCharge()
	model, modelWhy = m, "test"
	setWindowsFromModel()
	box2APICmd, box2APICmdParam = e.f.cmd, e.f.param
	box2Now = func() time.Time { return e.clk }
	box2FansMax = func() { e.fans++ }
	box2RestartMining = func() { e.restarts++; e.f.restart() }
	*authFile = filepath.Join(t.TempDir(), "auth.json")
	box2Disabled.Store(false)
	box2St = box2IdleState{}
	box2Parked.Store(false)
	t.Cleanup(func() {
		model, modelWhy = oldModel, oldWhy
		setWindowsFromModel()
		box2APICmd, box2APICmdParam = oldCmd, oldParam
		box2Now, box2FansMax, box2RestartMining, box2OverTemp = oldNow, oldFans, oldRestart, oldOT
		*authFile = oldAuth
		box2Disabled.Store(false)
		box2St = box2IdleState{}
		box2Parked.Store(false)
		setCharge(oldCharge)
	})
	return e
}

// tick: sec seconds pass, then one watch look.
func (e *c1Env) tick(sec int) {
	e.f.advance(float64(sec))
	e.clk = e.clk.Add(time.Duration(sec) * time.Second)
	box2IdleWatchTick(e.clk)
}

func (e *c1Env) ticks(n int) {
	for i := 0; i < n; i++ {
		e.tick(10)
	}
}

func phase() string {
	box2Mu.Lock()
	defer box2Mu.Unlock()
	return box2St.Phase
}

func markerExists() bool {
	_, err := os.Stat(box2SoftIdlePath())
	return err == nil
}

// ---- the reply format the Box II miner really sends ----

func TestC1Box2ReplyFormat(t *testing.T) {
	cases := []struct {
		reply       []byte
		ok, already bool
	}{
		{box2Status("I", 64, "PGA 0 set disable flag"), true, false},
		{box2Status("I", 63, "PGA 0 sent enable message"), true, false},
		{box2Status("I", 62, "PGA 0 already disabled"), false, true},
		{box2Status("I", 61, "PGA 0 already enabled"), false, true},
		{box2Status("E", 65, "PGA 0 is not flagged WELL, cannot enable"), false, false},
		{box2Status("E", 58, "Invalid PGA id 3 - range is 0 - 0"), false, false},
		{box2Status("E", 45, "Access denied to 'pgadisable' command"), false, false},
		{box2Status("S", 92, "PGA 0 set OK"), true, false},
		{append(box2Status("I", 64, "PGA 0 set disable flag"), 0), true, false}, // with the NUL the miner ends it with
	}
	for _, c := range cases {
		ok, already := apiStatusOK(c.reply)
		if ok != c.ok || already != c.already {
			t.Errorf("apiStatusOK(%s) = %v,%v want %v,%v", c.reply, ok, already, c.ok, c.already)
		}
	}
}

// ---- idle sent and confirmed ----

func TestC1IdleSentAndConfirmed(t *testing.T) {
	e := newC1Env(t, true)
	if err := idleAction(model); err != nil {
		t.Fatalf("idleAction: %v", err)
	}
	if e.f.count("pgadisable 0") != 1 || e.f.isEnabled(0) {
		t.Fatalf("pgadisable not sent/taken: %v", e.f.sent)
	}
	if !box2Disabled.Load() || phase() != "stopping" || !box2SoftIdled() || !markerExists() {
		t.Fatalf("after the stop: disabled=%v phase=%q softIdled=%v marker=%v", box2Disabled.Load(), phase(), box2SoftIdled(), markerExists())
	}
	e.ticks(3) // the counter has to stand still for two readings in a row
	if phase() != "stopped" {
		t.Fatalf("not confirmed after 30 s of no work: phase %q (%s)", phase(), box2St.Note)
	}
	if e.fans != 0 {
		t.Fatal("a confirmed stop must not force the fans")
	}
	// the chips cool: the watch says so after box2CoolCheck
	e.ticks(int(box2CoolCheck/(10*time.Second)) + 1)
	st := box2IdleStatus()
	if st["phase"] != "stopped" || !strings.Contains(fmt.Sprint(st["note"]), "cooled") {
		t.Fatalf("status after cooling: %v", st)
	}
	if tn, _ := st["temp_now"].(float64); tn >= 74 {
		t.Fatalf("temperature should have fallen: %v", st)
	}
	if !minerOnIdlePlan() || !minerIsIdleNow() {
		t.Fatal("a confirmed soft idle counts as Idle for the guards")
	}
}

// ---- resumed ----

func TestC1Resumed(t *testing.T) {
	e := newC1Env(t, true)
	if err := idleAction(model); err != nil {
		t.Fatal(err)
	}
	e.ticks(3)
	maybeResumeBox2() // what every wake path (a preset, Stock, the schedule) calls before its plan write
	if e.f.count("pgaenable 0") != 1 || !e.f.isEnabled(0) {
		t.Fatalf("pgaenable not sent/taken: %v", e.f.sent)
	}
	if box2Disabled.Load() || markerExists() || box2SoftIdled() || phase() != "resuming" {
		t.Fatalf("after resume: disabled=%v marker=%v phase=%q", box2Disabled.Load(), markerExists(), phase())
	}
	e.ticks(2)
	if phase() != "" || !strings.Contains(box2St.Note, "hashing again") {
		t.Fatalf("resume not followed to hashing: phase %q note %q", phase(), box2St.Note)
	}
	if e.restarts != 0 {
		t.Fatal("a clean pgaenable must not restart the mining program")
	}
	// idempotent: nothing held, nothing sent
	n := e.f.count("pgaenable")
	maybeResumeBox2()
	if e.f.count("pgaenable") != n {
		t.Fatal("a second resume must send nothing")
	}
}

// pgaenable refused (Code 65: the board isn't Alive) → the mining program is restarted (every board fresh).
func TestC1ResumeRefusedRestartsMining(t *testing.T) {
	e := newC1Env(t, true)
	if err := idleAction(model); err != nil {
		t.Fatal(err)
	}
	e.f.mu.Lock()
	e.f.alive[0] = false
	e.f.mu.Unlock()
	if err := resumeAction(model); err != nil {
		t.Fatalf("resume must not fail the wake write: %v", err)
	}
	if e.restarts != 1 || !e.f.isEnabled(0) || box2Disabled.Load() {
		t.Fatalf("refused pgaenable: restarts=%d enabled=%v held=%v", e.restarts, e.f.isEnabled(0), box2Disabled.Load())
	}
}

// ---- re-applied after a miner restart ----

func TestC1ReappliedAfterMinerRestart(t *testing.T) {
	e := newC1Env(t, true)
	if err := idleAction(model); err != nil {
		t.Fatal(err)
	}
	e.ticks(3)
	if phase() != "stopped" {
		t.Fatalf("phase %q", phase())
	}
	// the launcher relaunches the mining program: first the API is silent, then a fresh process hashes
	e.f.mu.Lock()
	e.f.down = true
	e.f.mu.Unlock()
	e.ticks(2)
	if !box2Disabled.Load() {
		t.Fatal("an API that doesn't answer must not end the idle")
	}
	e.f.mu.Lock()
	e.f.down = false
	e.f.mu.Unlock()
	e.f.restart()
	e.tick(10)
	if e.f.isEnabled(0) || e.f.count("pgadisable 0") != 2 {
		t.Fatalf("the stop was not re-applied after the restart: %v", e.f.sent)
	}
	if box2St.Reapplied != 1 || phase() != "stopping" {
		t.Fatalf("reapplied=%d phase=%q", box2St.Reapplied, phase())
	}
	e.ticks(3)
	if phase() != "stopped" || e.fans != 0 {
		t.Fatalf("after re-apply: phase %q fans %d", phase(), e.fans)
	}
}

// ---- reboot while idle: the marker brings the idle back ----

func TestC1RebootWhileIdle(t *testing.T) {
	e := newC1Env(t, true)
	if err := idleAction(model); err != nil {
		t.Fatal(err)
	}
	if !markerExists() {
		t.Fatal("no marker")
	}
	// the miner reboots: the panel starts with nothing in memory, the mining program fresh (board enabled)
	box2Disabled.Store(false)
	box2St = box2IdleState{}
	setCharge("")
	e.f.restart()
	box2SoftIdleBoot()
	if !box2Disabled.Load() || getCharge() != "idle" {
		t.Fatalf("boot restore: held=%v charge=%q", box2Disabled.Load(), getCharge())
	}
	e.tick(10)
	if e.f.isEnabled(0) || box2St.Reapplied != 1 {
		t.Fatalf("the stop was not re-applied after the reboot: %v", e.f.sent)
	}
	e.ticks(3)
	if phase() != "stopped" {
		t.Fatalf("phase %q", phase())
	}
}

// ---- fall-back when the miner ignores the stop ----

func TestC1IgnoredFallsBack(t *testing.T) {
	e := newC1Env(t, true)
	e.f.keepHashing = true // the flag is taken, devs says "N", but the work counter keeps growing
	if err := idleAction(model); err != nil {
		t.Fatalf("the flag was taken: idleAction returns nil and the watch judges it: %v", err)
	}
	e.ticks(int(box2ConfirmWindow/(10*time.Second)) + 2)
	if phase() != "ignored" || !box2St.Ignored {
		t.Fatalf("an ignored stop must be reported: phase %q note %q", phase(), box2St.Note)
	}
	if e.fans != 1 {
		t.Fatalf("fans must be forced to 100%% once: %d", e.fans)
	}
	if box2SoftIdled() {
		t.Fatal("an ignored stop must not count as Idle (the guards act again)")
	}
	// a guard tries again: it gets an error (→ its own no-park fall-back), with the cut-off named
	err := idleAction(model)
	if err == nil || !strings.Contains(err.Error(), "86") {
		t.Fatalf("idleAction after an ignored stop = %v", err)
	}
	// resume still re-enables the board the panel disabled
	if err := resumeAction(model); err != nil || box2Disabled.Load() {
		t.Fatalf("resume after ignored: %v held=%v", err, box2Disabled.Load())
	}
}

// the miner answers "set disable flag" but keeps the board enabled: not parked, fall back at once.
func TestC1FlagNotTakenFallsBack(t *testing.T) {
	e := newC1Env(t, true)
	e.f.noFlag = true
	err := idleAction(model)
	if err == nil || !strings.Contains(err.Error(), "still lists board") || !strings.Contains(err.Error(), idleActionUnprovenMsg) {
		t.Fatalf("idleAction = %v", err)
	}
	if box2Disabled.Load() || e.fans != 1 || markerExists() {
		t.Fatalf("held=%v fans=%d marker=%v", box2Disabled.Load(), e.fans, markerExists())
	}
	if e.f.count("pgadisable 0") != 2 {
		t.Fatalf("the stop should be sent once more before giving up: %v", e.f.sent)
	}
}

// the stop looked fine, but the temperature keeps rising: reported as ignored.
func TestC1TemperatureRisingFallsBack(t *testing.T) {
	e := newC1Env(t, true)
	e.f.heatsDisabled = true
	if err := idleAction(model); err != nil {
		t.Fatal(err)
	}
	e.ticks(3)
	if phase() != "stopped" {
		t.Fatalf("phase %q", phase())
	}
	e.ticks(int(box2CoolCheck/(10*time.Second)) + 1)
	if phase() != "ignored" || e.fans != 1 {
		t.Fatalf("rising temperature with the board off: phase %q fans %d note %q", phase(), e.fans, box2St.Note)
	}
}

// a re-apply that fails (the restarted miner refuses) falls back, and the next look tries again.
func TestC1ReapplyFailureFallsBackAndRetries(t *testing.T) {
	e := newC1Env(t, true)
	if err := idleAction(model); err != nil {
		t.Fatal(err)
	}
	e.f.restart()
	e.f.noFlag = true
	e.tick(10)
	if phase() != "ignored" || e.fans != 1 || box2SoftIdled() {
		t.Fatalf("re-apply failure: phase %q fans %d", phase(), e.fans)
	}
	e.f.noFlag = false
	e.tick(10)
	if phase() != "stopping" || e.f.isEnabled(0) || !box2SoftIdled() {
		t.Fatalf("retry: phase %q enabled %v", phase(), e.f.isEnabled(0))
	}
}

// ---- inert when OFF ----

func TestC1OffIsInert(t *testing.T) {
	e := newC1Env(t, false)
	box2SoftIdleBoot() // no marker: nothing
	e.ticks(20)
	if err := resumeAction(model); err != nil {
		t.Fatal(err)
	}
	maybeResumeBox2()
	if len(e.f.sent) != 0 || box2Disabled.Load() || box2SoftIdled() || e.restarts != 0 {
		t.Fatalf("SoftIdle off must send nothing: %v", e.f.sent)
	}
	if err := idleAction(model); err == nil || len(e.f.sent) != 0 || e.fans != 1 {
		t.Fatalf("SoftIdle off: idleAction=%v sent=%v fans=%d", err, e.f.sent, e.fans)
	}
	if n := idleActionNote(); !strings.Contains(n, idleActionUnprovenMsg) {
		t.Fatalf("note with SoftIdle off: %q", n)
	}
	// SC Lite: none of it
	model = profSCLite
	box2Disabled.Store(true) // (even with the flag set by mistake)
	box2SoftIdleBoot()
	e.ticks(3)
	if box2SoftIdled() || len(e.f.sent) != 0 || idleActionNote() != "" {
		t.Fatal("SC Lite: the soft idle must be completely inert")
	}
}

// SoftIdle switched off while the panel held the board off: the leftover is re-enabled once.
func TestC1OffWithLeftoverReenables(t *testing.T) {
	e := newC1Env(t, true)
	if err := idleAction(model); err != nil {
		t.Fatal(err)
	}
	m := profSCBox2 // the owner switches SoftIdle off; the panel restarts
	model = m
	box2Disabled.Store(false)
	box2ReleaseLeftover(time.Second)
	if !e.f.isEnabled(0) || markerExists() || e.f.count("pgaenable 0") != 1 {
		t.Fatalf("leftover not released: enabled=%v marker=%v sent=%v", e.f.isEnabled(0), markerExists(), e.f.sent)
	}
}

// ---- the plan writes while soft idled: the fan curve / throttle may not write; a wake resumes first ----

func TestC1PlanWritesWhileSoftIdled(t *testing.T) {
	e := newC1Env(t, true)
	puts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/user/login"):
			json.NewEncoder(w).Encode(map[string]any{"JWT Token": "tok"})
		case r.Method == "GET":
			io.WriteString(w, box2Setting)
		case r.Method == "PUT":
			puts++
			io.WriteString(w, `{"status":0}`)
		}
	}))
	defer srv.Close()
	oldWeb := *minerWeb
	*minerWeb = strings.TrimPrefix(srv.URL, "http://")
	jwtToken = ""
	authMu.Lock()
	auth.MinerPass = "pw"
	authMu.Unlock()
	t.Cleanup(func() { *minerWeb = oldWeb })

	if err := idleAction(model); err != nil {
		t.Fatal(err)
	}
	if idle, err := writeFansUnlessIdle(60); !idle || err != nil || puts != 0 {
		t.Fatalf("a fan write while soft idled: idle=%v err=%v puts=%d (a plan write would reach the disabled board)", idle, err, puts)
	}
	// a wake (a preset / Stock) re-enables the board first, then writes
	if _, err := mutatePlanEx(func(p *powerplan) {}, true, noGen); err != nil {
		t.Fatalf("wake write: %v", err)
	}
	if !e.f.isEnabled(0) || box2Disabled.Load() || puts != 1 {
		t.Fatalf("wake: enabled=%v held=%v puts=%d", e.f.isEnabled(0), box2Disabled.Load(), puts)
	}
}

// The OEM's own heat shutdown is never undone: after it (its log, or the miner's overheat flag) a wake does
// not re-enable the board, with SoftIdle on or when releasing a leftover with it off.
func TestC1NeverUndoesOEMCutoff(t *testing.T) {
	e := newC1Env(t, true)
	if err := idleAction(model); err != nil {
		t.Fatal(err)
	}
	e.f.overheat = true
	if err := resumeAction(model); err == nil || !strings.Contains(err.Error(), "overheated") {
		t.Fatalf("resume over the miner's overheat flag = %v", err)
	}
	e.f.overheat = false
	box2OverTemp = func() bool { return true }
	if err := resumeAction(model); err == nil || !strings.Contains(err.Error(), "over-temperature") {
		t.Fatalf("resume after the OEM's shutdown = %v", err)
	}
	box2ReleaseLeftover(time.Second)
	if e.f.count("pgaenable") != 0 || e.f.isEnabled(0) || e.restarts != 0 {
		t.Fatalf("the board was re-enabled over the cut-off: %v", e.f.sent)
	}
}
