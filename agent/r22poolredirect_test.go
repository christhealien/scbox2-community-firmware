package main

import (
	"bufio"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// A pool that keeps redirecting the miner, and setting Idle fails: the guard must still stop the miner hashing
// for that pool (disable every pool in the running miner), as it does for a pool nobody entered. Before, it only
// said "set to Idle and held" and the miner kept mining on the redirected server.
func TestR22PoolRedirectIdleFailedDisablesPools(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var mu sync.Mutex
	var cmds []string
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			b, _ := bufio.NewReader(c).ReadString('}')
			mu.Lock()
			cmds = append(cmds, b)
			mu.Unlock()
			c.Write([]byte(`{"STATUS":[{"STATUS":"S","Msg":"ok"}],"id":1}` + "\x00"))
			c.Close()
		}
	}()
	oldAPI := *apiAddr
	*apiAddr = ln.Addr().String()
	oldList, oldLive, oldIdle, oldRm, oldIG, oldRst := pgListPools, pgLivePools, pgIdle, pgRemove, minerIdleForGuard, pgRestartMiner
	oldAuth := *authFile
	*authFile = filepath.Join(t.TempDir(), "auth.json")
	defer func() {
		poolHoldRelease()
		*authFile, *apiAddr = oldAuth, oldAPI
		pgListPools, pgLivePools, pgIdle, pgRemove, minerIdleForGuard, pgRestartMiner = oldList, oldLive, oldIdle, oldRm, oldIG, oldRst
		pgStrikes, pgActed, pgIncidents = map[string]int{}, map[string]int{}, nil
	}()
	pgListPools = func() ([]map[string]any, error) {
		return []map[string]any{{"url": "stratum+tcp://mine.example:3333", "user": "w.1"}}, nil
	}
	pgLivePools = func() ([]poolView, error) {
		return []poolView{{Index: 0, URL: "stratum+tcp://mine.example:3333", StratumURL: "stratum+tcp://evil.example:3333", User: "w.1", Active: true}}, nil
	}
	pgIdle = func() error { return errTest("backend down") }
	pgRemove = func(int) error { return nil }
	pgRestartMiner = func() error { return nil }
	minerIdleForGuard = func() bool { return false }
	pgStrikes, pgActed, pgIncidents = map[string]int{}, map[string]int{}, nil
	for i := 0; i < 3; i++ { // two restarts, then the Idle that fails
		poolGuardCheck()
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(strings.Join(cmds, " "), "disablepool") {
		t.Fatalf("Idle failed on a redirecting pool and the miner's pools weren't disabled (commands: %q)", cmds)
	}
}
