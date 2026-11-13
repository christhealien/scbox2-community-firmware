package main

// Lane B3: the Box II stop-hashing action (pgadisable) and its resume (pgaenable), the conservative
// SoftIdle gate, graceful degrade to "fans 100 % + 86 °C cut-off backstop", and proof the SC Lite idle
// path is unchanged. The miner API is mocked the way the r4/tuner tests mock it (func vars, and one test
// stands up a real TCP server on *apiAddr to exercise cgminerCmdParam end to end).

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// b3WithModel sets the active profile for a test and restores it (and the soft-idle flags/mocks).
// Lane C1: the fans fall-back is a stock-plan write now (never sysfs): recorded in b3FansForced; the soft-idle
// state and its marker file (in a temp dir) are reset too.
var b3FansForced int

func b3WithModel(t *testing.T, m modelProfile) {
	t.Helper()
	oldModel := model
	oldCmd, oldParam := box2APICmd, box2APICmdParam
	oldFans, oldAuth := box2FansMax, *authFile
	box2Disabled.Store(false)
	box2St = box2IdleState{}
	box2Parked.Store(false)
	b3FansForced = 0
	box2FansMax = func() { b3FansForced++ }
	*authFile = filepath.Join(t.TempDir(), "auth.json")
	t.Cleanup(func() {
		model = oldModel
		box2APICmd, box2APICmdParam = oldCmd, oldParam
		box2FansMax, *authFile = oldFans, oldAuth
		box2Disabled.Store(false)
		box2St = box2IdleState{}
		box2Parked.Store(false)
	})
	model = m
}

// box2SoftIdleProfile: the Box II profile with the (hardware-gated) stop-hashing action turned ON, as a
// hardware tester would set it after T1 Gate B.
func box2SoftIdleProfile() modelProfile {
	m := profSCBox2
	m.Feat.SoftIdle = true
	return m
}

// a mock cgminer `devs` reply listing n boards with explicit PGA numbers.
func mockDevs(n int) []byte {
	var ds []string
	for i := 0; i < n; i++ {
		ds = append(ds, fmt.Sprintf(`{"ASC":%d,"PGA":%d,"Name":"GSB2","ID":%d,"Enabled":"Y","Status":"Alive"}`, i, i, i))
	}
	return []byte(`{"STATUS":[{"STATUS":"S"}],"DEVS":[` + strings.Join(ds, ",") + `]}`)
}

func statusS(msg string) []byte {
	return []byte(`{"STATUS":[{"STATUS":"S","Msg":"` + msg + `"}]}`)
}
func statusE(msg string) []byte {
	return []byte(`{"STATUS":[{"STATUS":"E","Msg":"` + msg + `"}]}`)
}

// --- the core: idleAction sends pgadisable to every board on Box II (SoftIdle on) ---

func TestB3Box2IdleDisablesEveryBoard(t *testing.T) {
	b3WithModel(t, box2SoftIdleProfile())
	var mu sync.Mutex
	sent := map[string][]string{} // command -> parameters
	f := newFakeBox2Miner(3)      // pretend 3 boards to prove "every board", not just Box II's 1
	box2APICmd = f.cmd
	box2APICmdParam = func(c, p string) ([]byte, error) {
		mu.Lock()
		sent[c] = append(sent[c], p)
		mu.Unlock()
		return f.param(c, p)
	}

	if err := idleAction(model); err != nil {
		t.Fatalf("idleAction returned %v, want nil (every board stopped)", err)
	}
	got := append([]string(nil), sent["pgadisable"]...)
	if strings.Join(got, ",") != "0,1,2" {
		t.Fatalf("pgadisable sent to %v, want every board 0,1,2", got)
	}
	if len(sent["pgaenable"]) != 0 {
		t.Fatalf("idle must not enable boards: %v", sent["pgaenable"])
	}
	if !box2Disabled.Load() {
		t.Fatal("box2Disabled should be set so resume knows to re-enable")
	}

	// resume sends pgaenable to every board
	if err := resumeAction(model); err != nil {
		t.Fatalf("resumeAction: %v", err)
	}
	if strings.Join(sent["pgaenable"], ",") != "0,1,2" {
		t.Fatalf("pgaenable sent to %v, want 0,1,2", sent["pgaenable"])
	}
	if box2Disabled.Load() {
		t.Fatal("box2Disabled should be cleared after resume")
	}
	// resume again is a no-op (nothing disabled)
	before := len(sent["pgaenable"])
	if err := resumeAction(model); err != nil || len(sent["pgaenable"]) != before {
		t.Fatalf("second resume should be a no-op, err=%v sent=%d", err, len(sent["pgaenable"]))
	}
}

// --- conservative default: SoftIdle OFF on the stock Box II profile → no API command at all ---

func TestB3Box2SoftIdleOffIsConservativeDefault(t *testing.T) {
	if profSCBox2.Feat.SoftIdle {
		t.Fatal("the stock Box II profile must ship SoftIdle OFF (conservative)")
	}
	b3WithModel(t, profSCBox2) // stock profile: SoftIdle off
	called := false
	box2APICmd = func(string) ([]byte, error) { called = true; return nil, nil }
	box2APICmdParam = func(string, string) ([]byte, error) { called = true; return nil, nil }
	useTempFans(t)

	err := idleAction(model)
	if err == nil {
		t.Fatal("with no verified stop action, idleAction must report it could not park (not nil)")
	}
	if called {
		t.Fatal("the unproven pgadisable must NOT be sent when SoftIdle is off")
	}
	if !strings.Contains(err.Error(), idleActionUnprovenMsg) {
		t.Fatalf("degrade error must carry the backstop caveat; got %q", err.Error())
	}
	// fans forced to 100 %
	assertFansMax(t)
}

// --- graceful degrade when the API doesn't answer (miner restarting) ---

func TestB3Box2IdleDegradesWhenAPIDown(t *testing.T) {
	b3WithModel(t, box2SoftIdleProfile())
	box2APICmd = func(string) ([]byte, error) { return nil, fmt.Errorf("connection refused") }
	box2APICmdParam = func(string, string) ([]byte, error) {
		t.Fatal("must not send pgadisable when the board list can't be read")
		return nil, nil
	}
	useTempFans(t)

	err := idleAction(model)
	if err == nil {
		t.Fatal("API down: idleAction must return an error, not a silent no-op")
	}
	if !strings.Contains(err.Error(), idleActionUnprovenMsg) || !strings.Contains(err.Error(), "100%") {
		t.Fatalf("degrade message must mention fans 100%% and the backstop caveat; got %q", err.Error())
	}
	assertFansMax(t)
}

// --- degrade when a board refuses pgadisable (partial failure) ---

func TestB3Box2IdleDegradesOnBoardRefusal(t *testing.T) {
	b3WithModel(t, box2SoftIdleProfile())
	box2APICmd = func(string) ([]byte, error) { return mockDevs(2), nil }
	box2APICmdParam = func(c, p string) ([]byte, error) {
		if p == "1" {
			return statusE("cannot disable"), nil // board 1 refuses
		}
		return statusS("ok"), nil
	}
	useTempFans(t)

	err := idleAction(model)
	if err == nil {
		t.Fatal("a board refusing the stop must degrade (not report success)")
	}
	if !strings.Contains(err.Error(), idleActionUnprovenMsg) {
		t.Fatalf("partial-failure error must carry the backstop caveat; got %q", err.Error())
	}
	assertFansMax(t)
}

// --- "already disabled" is treated as success (idempotent across retries) ---

func TestB3Box2IdleIdempotent(t *testing.T) {
	b3WithModel(t, box2SoftIdleProfile())
	f := newFakeBox2Miner(1)
	f.enabled[0] = false // already disabled: the miner answers STATUS "I" Code 62 "PGA 0 already disabled"
	box2APICmd, box2APICmdParam = f.cmd, f.param
	if err := idleAction(model); err != nil {
		t.Fatalf("an already-disabled board should count as stopped; got %v", err)
	}
}

// --- the guards are wired to idleAction ---

func TestB3PoolGuardAndFanLockUseIdleAction(t *testing.T) {
	b3WithModel(t, box2SoftIdleProfile())
	var mu sync.Mutex
	disabled := map[string]int{}
	f := newFakeBox2Miner(1)
	box2APICmd = f.cmd
	box2APICmdParam = func(c, p string) ([]byte, error) {
		mu.Lock()
		disabled[c]++
		mu.Unlock()
		return f.param(c, p)
	}
	if err := pgIdle(); err != nil {
		t.Fatalf("pool guard pgIdle: %v", err)
	}
	box2Disabled.Store(false)
	if err := fanLockIdle(); err != nil {
		t.Fatalf("fan lock fanLockIdle: %v", err)
	}
	if disabled["pgadisable"] < 2 {
		t.Fatalf("pool guard and fan lock should both route through pgadisable; got %v", disabled)
	}
}

// --- idleActionNote / caveat text ---

func TestB3IdleActionNote(t *testing.T) {
	b3WithModel(t, profSCBox2)
	if n := idleActionNote(); !strings.Contains(n, idleActionUnprovenMsg) {
		t.Fatalf("Box II note must carry the caveat; got %q", n)
	}
	model = profSCLite
	if n := idleActionNote(); n != "" {
		t.Fatalf("SC Lite note must be empty (idle is proven there); got %q", n)
	}
	if !strings.Contains(idleActionUnprovenMsg, "86°C") || !strings.Contains(idleActionUnprovenMsg, "unverified") {
		t.Fatalf("caveat must name the 86°C backstop and say unverified; got %q", idleActionUnprovenMsg)
	}
}

// --- the 86 °C hardware cut-off is never touched by the idle path (safety backstop stays) ---

func TestB3Box2CutoffUnchanged(t *testing.T) {
	if profSCBox2.TempCutoffC != 86 {
		t.Fatalf("Box II cut-off must stay 86, got %.0f", profSCBox2.TempCutoffC)
	}
	b3WithModel(t, box2SoftIdleProfile())
	f := newFakeBox2Miner(1)
	box2APICmd, box2APICmdParam = f.cmd, f.param
	_ = idleAction(model)
	if model.TempCutoffC != 86 {
		t.Fatalf("the stop-hashing action must not change the cut-off; got %.0f", model.TempCutoffC)
	}
}

// --- SC Lite idle path unchanged: idleAction selects the stock idle plan via the web backend, never pga ---

func TestB3SCLiteIdlePathUnchanged(t *testing.T) {
	b3WithModel(t, profSCLite)
	box2APICmd = func(string) ([]byte, error) { t.Fatal("SC Lite idle must not call the cgminer API"); return nil, nil }
	box2APICmdParam = func(string, string) ([]byte, error) { t.Fatal("SC Lite idle must not pgadisable"); return nil, nil }

	var put map[string]any
	setting := map[string]any{"manual": true, "select": float64(0),
		"powerplans": []any{
			map[string]any{"info": "615 MHz 9100 V 40 RPM 40 RPM PV 9400", "level": float64(0)},
			map[string]any{"info": "0 MHz 0 V 100 RPM 100 RPM", "level": float64(3)}}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/user/login"):
			json.NewEncoder(w).Encode(map[string]any{"JWT Token": "tok"})
		case r.Method == "GET":
			json.NewEncoder(w).Encode(setting)
		case r.Method == "PUT":
			b, _ := io.ReadAll(r.Body)
			json.Unmarshal(b, &put)
			io.WriteString(w, `{"status":0}`)
		}
	}))
	defer srv.Close()
	oldWeb := *minerWeb
	*minerWeb = strings.TrimPrefix(srv.URL, "http://")
	jwtToken = ""
	defer func() { *minerWeb = oldWeb }()

	if err := idleAction(model); err != nil {
		t.Fatalf("SC Lite idleAction: %v", err)
	}
	// applyIdle selects the idle plan (list position 1) with manual off
	if put["manual"] != false {
		t.Fatalf("SC Lite idle must set manual=false; got %v", put["manual"])
	}
	if s, _ := put["select"].(float64); int(s) != 1 {
		t.Fatalf("SC Lite idle must select the 0 MHz plan (position 1); got %v", put["select"])
	}
}

// --- end-to-end against a real TCP server on *apiAddr (mock the miner API as r4/tuner tests do) ---

func TestB3Box2IdleOverRealSocket(t *testing.T) {
	b3WithModel(t, box2SoftIdleProfile())
	// restore the real API funcs for this test (b3WithModel saved them)
	box2APICmd, box2APICmdParam = cgminerCmd, cgminerCmdParam

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	oldAddr := *apiAddr
	*apiAddr = ln.Addr().String()
	defer func() { *apiAddr = oldAddr }()

	var mu sync.Mutex
	gotDisable := map[string]bool{}
	f := newFakeBox2Miner(1) // the board state behind the socket (devs lists it disabled once it is)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 4096)
				n, _ := c.Read(buf)
				var q struct{ Command, Parameter string }
				json.Unmarshal(buf[:n], &q)
				switch q.Command {
				case "devs":
					out, _ := f.cmd("devs")
					c.Write(append(out, 0))
				case "pgadisable":
					mu.Lock()
					gotDisable[q.Parameter] = true
					mu.Unlock()
					out, _ := f.param(q.Command, q.Parameter) // the Box II's own reply: STATUS "I" Code 64
					c.Write(append(out, 0))
				default:
					c.Write(append(statusS("ok"), 0))
				}
			}(c)
		}
	}()

	if err := idleAction(model); err != nil {
		t.Fatalf("idleAction over socket: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !gotDisable["0"] {
		t.Fatalf("board 0 was not disabled over the real socket; got %v", gotDisable)
	}
}

// ---- small helpers ----

func useTempFans(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	for _, f := range []string{"fan0", "fan1"} {
		p := filepath.Join(dir, f)
		os.MkdirAll(p, 0o755)
		os.WriteFile(filepath.Join(p, "lvl"), []byte("40"), 0o644)
	}
	old := fanSysDir
	fanSysDir = dir
	t.Cleanup(func() { fanSysDir = old })
}

// assertFansMax: the degrade path forced the fans to 100 % — through the stock plan (box2FansMax), and never
// by a raw sysfs write (lane C1: the levels in the temp fan dir stay as they were).
func assertFansMax(t *testing.T) {
	t.Helper()
	if b3FansForced == 0 {
		t.Fatal("the degrade path should force the fans to 100% (box2FansMax)")
	}
	for _, f := range []string{"fan0", "fan1"} {
		b, err := os.ReadFile(filepath.Join(fanSysDir, f, "lvl"))
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if v, _ := strconv.Atoi(strings.TrimSpace(string(b))); v != 40 {
			t.Fatalf("%s sysfs level was written directly (%d): fans go through the stock plan only", f, v)
		}
	}
}
