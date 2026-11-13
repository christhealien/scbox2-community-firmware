package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPoolGuard(t *testing.T) {
	list := []map[string]any{{"url": "stratum+tcp://192.168.1.5:23334", "user": "bc1qme.SCLite_2"}}
	live := []poolView{{Index: 0, URL: "stratum+tcp://192.168.1.5:23334", User: "bc1qme.SCLite_2"}}
	removed, idled := []int{}, 0
	oldL, oldP, oldR, oldI := pgListPools, pgLivePools, pgRemove, pgIdle
	defer func() { pgListPools, pgLivePools, pgRemove, pgIdle = oldL, oldP, oldR, oldI }()
	pgListPools = func() ([]map[string]any, error) { return list, nil }
	pgLivePools = func() ([]poolView, error) { return live, nil }
	pgRemove = func(n int) error { removed = append(removed, n); return nil }
	pgIdle = func() error { idled++; return nil }
	pgStrikes, pgActed, pgIncidents = map[string]int{}, map[string]int{}, nil
	oldA := *authFile
	*authFile = filepath.Join(t.TempDir(), "auth.json")
	defer func() { *authFile = oldA }()

	poolGuardCheck()
	if len(removed) != 0 {
		t.Fatal("the user's own pool was touched")
	}
	// same pool, written differently: still the user's
	live = []poolView{{Index: 0, URL: "STRATUM+TCP://192.168.1.5:23334/", User: "bc1qme.SCLite_2"}}
	poolGuardCheck()
	poolGuardCheck()
	if len(removed) != 0 {
		t.Fatal("a case/prefix difference counted as another pool")
	}
	// the same pool with someone else's account, and a pool that isn't in the list
	live = []poolView{{Index: 0, URL: "stratum+tcp://192.168.1.5:23334", User: "bc1qme.SCLite_2"},
		{Index: 1, URL: "stratum+tcp://192.168.1.5:23334", User: "someoneelse.w1"},
		{Index: 2, URL: "stratum+tcp://ltc.viabtc.com:3333", User: "shminer.a2"}}
	poolGuardCheck()
	if len(removed) != 0 {
		t.Fatal("acted on the first sighting (the Pools tab may be mid-change)")
	}
	poolGuardCheck()
	if len(removed) != 2 || removed[0] != 2 || removed[1] != 1 {
		t.Fatalf("removed %v, want pools 2 then 1 (highest first: removals renumber the rest)", removed)
	}
	// they keep coming back: removed again, then Idle
	for i := 0; i < 6; i++ {
		poolGuardCheck()
	}
	if idled == 0 {
		t.Fatal("a pool that keeps coming back should put the miner in Idle")
	}
	if poolHoldErr() == nil || miningHoldErr() == nil {
		t.Fatal("after the guard's Idle, presets/Stock/schedule must be held until resumed")
	}
	os.Remove(poolHoldPath())
	if poolHoldErr() != nil {
		t.Fatal("hold still on after resume")
	}
	// Goldshell's no-pool placeholder is offline: left alone
	pgStrikes, pgActed, removed = map[string]int{}, map[string]int{}, nil
	list = nil
	live = []poolView{{Index: 0, URL: "stratum+tcp://benchmark", User: ""}}
	poolGuardCheck()
	poolGuardCheck()
	if len(removed) != 0 {
		t.Fatal("benchmark placeholder touched")
	}
}

// While the engine is down (idle), the Pools tab shows the configured pools: from the backend, or from
// the miner's own config file when the backend doesn't answer.
func TestConfiguredPoolsFallback(t *testing.T) {
	dir := t.TempDir()
	old, oldWeb := *bfgPath, *minerWeb
	defer func() { *bfgPath, *minerWeb = old, oldWeb }()
	*bfgPath = filepath.Join(dir, "bfgminer.json")
	*minerWeb = "127.0.0.1:1" // nothing answers
	jwtToken = ""
	os.WriteFile(*bfgPath, []byte(`{"pools":[{"url":"stratum+tcp://pool.example:3333","user":"wallet.rig","pass":"x"},{"url":"benchmark","user":""}]}`), 0o644)
	p, err := configuredPools()
	if err != nil || len(p) != 1 || p[0].URL != "stratum+tcp://pool.example:3333" || p[0].User != "wallet.rig" || p[0].Index != 0 {
		t.Fatalf("%v %+v", err, p)
	}
}

func TestUrlHost(t *testing.T) {
	for in, want := range map[string]string{
		"stratum+tcp://192.168.1.5:23334": "192.168.1.5", "192.168.1.5": "192.168.1.5", "Pool.Example.com:3333": "pool.example.com",
		"stratum+tcp://u:p@pool.com:3333/x#xnsub": "pool.com", "stratum+tcp://[fe80::1]:3333": "fe80::1", "": "",
	} {
		if got := urlHost(in); got != want {
			t.Errorf("urlHost(%q) = %q, want %q", in, got, want)
		}
	}
}
