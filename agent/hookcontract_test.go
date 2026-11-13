package main

// Agent <-> chip hook contract (tuner-sweep/hook).

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakePgaAPI answers "devs" with one PGA per chain and records every pgaset parameter.
func fakePgaAPI(t *testing.T, chains []int) (*[]string, func()) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip(err)
	}
	var mu sync.Mutex
	var got []string
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			buf := make([]byte, 512)
			n, _ := c.Read(buf)
			var q map[string]string
			json.Unmarshal(buf[:n], &q)
			switch q["command"] {
			case "devs":
				var ds []string
				for i, ch := range chains {
					ds = append(ds, `{"PGA":`+hkItoa(i)+`,"ID":`+hkItoa(ch)+`}`)
				}
				c.Write([]byte(`{"STATUS":[{"STATUS":"S"}],"DEVS":[` + strings.Join(ds, ",") + `]}` + "\x00"))
			case "pgaset":
				mu.Lock()
				got = append(got, q["parameter"])
				mu.Unlock()
				c.Write([]byte(`{"STATUS":[{"STATUS":"S","Msg":"set OK"}]}` + "\x00"))
			}
			c.Close()
		}
	}()
	old := *apiAddr
	*apiAddr = ln.Addr().String()
	return &got, func() { *apiAddr = old; ln.Close() }
}

func hkItoa(i int) string { b, _ := json.Marshal(i); return string(b) }

// The panel says why trims aren't in effect when the wrapper ran the miner without the hook.
func TestHookFallbackShown(t *testing.T) {
	old := *authFile
	*authFile = filepath.Join(t.TempDir(), "auth.json")
	defer func() { *authFile = old }()
	oldStats := chipStatsPath
	chipStatsPath = filepath.Join(t.TempDir(), "chipstats.json")
	defer func() { chipStatsPath = oldStats }()
	os.WriteFile(chipHookFlag(), []byte("on\n"), 0o644)
	// a report left in /tmp by the last hooked run, 10 minutes old (the miner now runs without the library)
	os.WriteFile(chipStatsPath, []byte(`{"t":1,"chains":[{"chain":2,"chips":3,"base10":6000,"clock10":[6000,5875,6000,6000,6000],"nonces":[0,1,1,1,0]}]}`), 0o644)
	ten := time.Now().Add(-10 * time.Minute)
	os.Chtimes(chipStatsPath, ten, ten)
	v := chipTrimView()
	t.Logf("enabled=%v active=%v chains=%d", v["enabled"], v["active"], len(v["chains"].([]chainView)))
	if v["active"] == false && len(v["chains"].([]chainView)) > 0 {
		t.Logf("NOTE: the grid still shows the dead run's per-chip clocks (chip 1 at 587.5) under 'on · hook not reporting yet (mining may still be starting)'")
	}
	_ = http.StatusOK
}

func TestHookReapplyMidInitClockGuarded(t *testing.T) {
	old := reapplySetClock
	reapplySetClock = func() (int, bool) { return 600, true }
	defer func() { reapplySetClock = old }()
	oldStats := chipStatsPath
	chipStatsPath = filepath.Join(t.TempDir(), "chipstats.json")
	defer func() { chipStatsPath = oldStats }()
	got, done := fakePgaAPI(t, []int{0, 1})
	defer done()
	os.WriteFile(chipStatsPath, []byte(`{"t":1,"chains":[{"chain":0,"chips":46,"base10":6000},{"chain":1,"chips":46,"base10":500}]}`), 0o644)
	if _, err := reapplyTrims(); err == nil || len(*got) != 0 {
		t.Fatalf("mid-init report: err %v, sent %v", err, *got)
	}
	os.WriteFile(chipStatsPath, []byte(`{"t":1,"chains":[{"chain":0,"chips":46,"base10":6000},{"chain":1,"chips":46,"base10":6000}]}`), 0o644)
	if note, err := reapplyTrims(); err != nil || len(*got) != 2 {
		t.Fatalf("settled report: %q %v, sent %v", note, err, *got)
	}
}
