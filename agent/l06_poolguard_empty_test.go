package main

import (
	"os"
	"path/filepath"
	"testing"
)

// L06: with no pool in the Pools tab, a pool the miner is running anyway is someone else's pool. The guard
// must act on it, not skip the check because the list is empty.
func TestL06PoolGuardEmptyTabForeignPool(t *testing.T) {
	dir := t.TempDir()
	oldB, oldA := *bfgPath, *authFile
	*bfgPath = filepath.Join(dir, "bfgminer", "bfgminer.json")
	*authFile = filepath.Join(dir, "sclite", "auth.json")
	os.MkdirAll(filepath.Dir(*authFile), 0o755)
	oldL, oldP, oldR, oldI, oldIdle := pgListPools, pgLivePools, pgRemove, pgIdle, minerIdleForGuard
	defer func() {
		*bfgPath, *authFile = oldB, oldA
		pgListPools, pgLivePools, pgRemove, pgIdle, minerIdleForGuard = oldL, oldP, oldR, oldI, oldIdle
	}()
	noteMinerChange("test")
	removed, idled := []int{}, 0
	pgListPools = func() ([]map[string]any, error) { return []map[string]any{}, nil } // the Pools tab: empty
	pgLivePools = func() ([]poolView, error) {
		return []poolView{{Index: 0, URL: "stratum+tcp://ltc.viabtc.com:3333", User: "shminer.a2", Active: true}}, nil
	}
	pgRemove = func(n int) error { removed = append(removed, n); return nil }
	pgIdle = func() error { idled++; return nil }
	minerIdleForGuard = func() bool { return false }
	pgStrikes, pgActed, pgIncidents = map[string]int{}, map[string]int{}, nil

	for i := 0; i < 3; i++ {
		poolGuardCheck()
	}
	if len(removed) == 0 && idled == 0 {
		t.Fatal("the Pools tab is empty and the miner runs a pool nobody entered: the guard did nothing")
	}

	// a transient empty answer while minerd's saved list still has the owner's pool: left alone
	removed, idled = nil, 0
	pgStrikes, pgActed, pgIncidents = map[string]int{}, map[string]int{}, nil
	saved := filepath.Join(dir, "minerd", "settings", "pools.json")
	os.MkdirAll(filepath.Dir(saved), 0o755)
	os.WriteFile(saved, []byte(`[{"url":"stratum+tcp://ltc.viabtc.com:3333","user":"shminer.a2","pass":"x"}]`), 0o644)
	for i := 0; i < 3; i++ {
		poolGuardCheck()
	}
	if len(removed) != 0 || idled != 0 {
		t.Fatalf("an empty read with pools saved acted on the owner's pool (removed %v, idle %d)", removed, idled)
	}
	os.Remove(poolHoldPath())
}
