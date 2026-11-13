package main

// L23-stockapi: the panel's client for the OEM's stock web backend (minerd on 127.0.0.1:80).

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// l23Backend: a fake minerd on a port of this lane (47440-47459), signed in already.
func l23Backend(t *testing.T, port string, h http.HandlerFunc) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:"+port)
	if err != nil {
		t.Skipf("port %s: %v", port, err)
	}
	srv := &httptest.Server{Listener: ln, Config: &http.Server{Handler: h}}
	srv.Start()
	t.Cleanup(srv.Close)
	d := t.TempDir()
	oldWeb, oldAuth := *minerWeb, *authFile
	*minerWeb = "127.0.0.1:" + port
	*authFile = filepath.Join(d, "auth.json")
	jwtMu.Lock()
	oldJWT := jwtToken
	jwtToken = "l23"
	jwtMu.Unlock()
	t.Cleanup(func() {
		jwtMu.Lock()
		jwtToken = oldJWT
		jwtMu.Unlock()
		*minerWeb, *authFile = oldWeb, oldAuth
	})
}

// minerd 2.2.0's PUT /mcb/restart is sync(); system("reboot") (see L23-stockapi/restart_is_reboot.py): "Restart
// mining" reboots the whole miner. Without the clean-reboot note the next boot reports "the miner restarted
// without the panel asking" (and keeps an "unexpected restart" on the Settings page) for every Restart mining,
// scheduled restart and automatic board restart.
func TestL23RestartMiningIsAReboot(t *testing.T) {
	var puts atomic.Int32
	l23Backend(t, "47441", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/mcb/restart" && r.Method == "PUT" {
			puts.Add(1)
			io.WriteString(w, `{}`)
			return
		}
		http.NotFound(w, r)
	})
	os.Remove(recCleanPath())
	if err := softRestartMining(); err != nil {
		t.Fatal(err)
	}
	if puts.Load() != 1 {
		t.Fatalf("%d restart requests", puts.Load())
	}
	if _, err := os.Stat(recCleanPath()); err != nil {
		t.Fatalf("restart (a reboot on this firmware) not noted as one the panel asked for: the next boot calls it unexpected (%v)", err)
	}
}

// A refused restart (the backend answered with an error) leaves no clean-reboot note behind, which would hide a
// later real crash; and the error is the PUT's own (minerd has no GET /mcb/restart: a fallback GET only ever
// got a 404 that replaced the real reason).
func TestL23RestartRefusedKeepsItsReason(t *testing.T) {
	var gets atomic.Int32
	l23Backend(t, "47442", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/mcb/restart" && r.Method == "PUT" {
			w.WriteHeader(500)
			io.WriteString(w, "Run Cmd reboot Error")
			return
		}
		if r.Method == "GET" {
			gets.Add(1)
		}
		http.NotFound(w, r)
	})
	err := softRestartMining()
	if err == nil || !strings.Contains(err.Error(), "reboot Error") || gets.Load() != 0 {
		t.Fatalf("err %v, GET fallbacks %d", err, gets.Load())
	}
	if _, e := os.Stat(recCleanPath()); e == nil {
		t.Fatal("a refused restart left the clean-reboot note")
	}
}

// The automatic restart for a board that stopped hashing is "at most once an hour", but the restart reboots the
// miner and the hour was kept only in memory: after the reboot a board that stays down (a bad ribbon cable)
// restarts it again 5 + 3 minutes later, forever (a reboot about every 10 minutes, a notification each time).
func TestL23BoardAutoRestartHourSurvivesTheReboot(t *testing.T) {
	*authFile = filepath.Join(t.TempDir(), "auth.json")
	captureNotify(t)
	var restarts atomic.Int32
	boardRestart = func() error { restarts.Add(1); return nil }
	now := time.Unix(1_790_000_000, 0)
	if why := boardAutoRestart(now, "board 1 stopped hashing"); why != "" {
		t.Fatalf("first restart refused: %q", why)
	}
	incidentWG.Wait()
	boardAutoAt = time.Time{} // the reboot: a new panel process
	why := boardAutoRestart(now.Add(10*time.Minute), "board 1 stopped hashing")
	incidentWG.Wait()
	if why == "" || restarts.Load() != 1 {
		t.Fatalf("10 minutes after the last automatic restart (and its reboot) it restarted again: restarts %d", restarts.Load())
	}
	boardAutoAt = time.Time{}
	if why := boardAutoRestart(now.Add(61*time.Minute), "board 1 stopped hashing"); why != "" {
		t.Fatalf("an hour later: %q", why)
	}
	incidentWG.Wait()
}

// Two pool changes at once (a double click on Add, two tabs) both read the same list from minerd: the same pool
// went to /mcb/newpool twice (a duplicate pool on the miner), and of two edits the second wrote back the list
// without the first one's change.
func TestL23ConcurrentPoolChanges(t *testing.T) {
	var mu sync.Mutex
	pools := []map[string]any{{"url": "stratum+tcp://a.example:3333", "user": "w1", "pass": "x"},
		{"url": "stratum+tcp://b.example:3333", "user": "w2", "pass": "x"}}
	var newpools atomic.Int32
	l23Backend(t, "47443", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/mcb/pools" && r.Method == "GET":
			mu.Lock()
			b, _ := json.Marshal(pools)
			mu.Unlock()
			time.Sleep(150 * time.Millisecond) // minerd answers slowly while the miner is busy
			w.Write(b)
		case r.URL.Path == "/mcb/pools" && r.Method == "PUT":
			var in []map[string]any
			json.NewDecoder(r.Body).Decode(&in)
			mu.Lock()
			pools = in
			mu.Unlock()
			io.WriteString(w, `{}`)
		case r.URL.Path == "/mcb/newpool":
			var in map[string]any
			json.NewDecoder(r.Body).Decode(&in)
			newpools.Add(1)
			mu.Lock()
			pools = append(pools, in)
			mu.Unlock()
			io.WriteString(w, `{}`)
		default:
			http.NotFound(w, r)
		}
	})
	oldLog := *logPath
	*logPath = filepath.Join(filepath.Dir(*authFile), "messages")
	t.Cleanup(func() { *logPath = oldLog })
	changeMu.Lock()
	oldAt, oldWhat := changeAt, changeWhat
	changeMu.Unlock()
	t.Cleanup(func() { changeMu.Lock(); changeAt, changeWhat = oldAt, oldWhat; changeMu.Unlock() }) // the pool guard waits after a pool change
	post := func(body string) {
		rr := httptest.NewRecorder()
		handlePools(rr, httptest.NewRequest("POST", "/api/pools", bytes.NewBufferString(body)))
	}
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			post(`{"action":"add","url":"stratum+tcp://c.example:3333","user":"w3","confirm":true}`)
		}()
	}
	wg.Wait()
	if n := newpools.Load(); n != 1 {
		t.Errorf("the same pool was added %d times", n)
	}
	// two edits at once: worker of pool a, worker of pool b
	wg.Add(2)
	go func() {
		defer wg.Done()
		post(`{"action":"edit","old_url":"stratum+tcp://a.example:3333","old_user":"w1","url":"stratum+tcp://a.example:3333","user":"w1new","confirm":true}`)
	}()
	go func() {
		defer wg.Done()
		post(`{"action":"edit","old_url":"stratum+tcp://b.example:3333","old_user":"w2","url":"stratum+tcp://b.example:3333","user":"w2new","confirm":true}`)
	}()
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if pools[0]["user"] != "w1new" || pools[1]["user"] != "w2new" {
		t.Errorf("an edit was lost: %v / %v", pools[0]["user"], pools[1]["user"])
	}
}
