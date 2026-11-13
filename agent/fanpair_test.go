package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func fakeFans(t *testing.T, lv ...int) string {
	t.Helper()
	d := t.TempDir()
	for i, v := range lv {
		os.MkdirAll(filepath.Join(d, fmt.Sprintf("fan%d", i)), 0755)
		os.WriteFile(filepath.Join(d, fmt.Sprintf("fan%d", i), "lvl"), []byte(fmt.Sprint(v)), 0644)
	}
	old := fanSysDir
	fanSysDir = d
	t.Cleanup(func() { fanSysDir = old })
	return d
}

func TestFanPairTarget(t *testing.T) {
	cases := []struct{ a, off, floor, want int }{
		{40, 10, 10, 50},
		{40, -10, 10, 30},
		{15, -10, 10, 10},  // floor
		{96, -10, 10, 96},  // emergency: never held back
		{100, 10, 10, 100}, // cap
		{8, -5, 10, 8},     // a floor above pair A doesn't raise B past A on a "lower" offset
	}
	for _, c := range cases {
		if got := fanPairTarget(c.a, c.off, c.floor); got != c.want {
			t.Errorf("target(%d,%+d,floor %d)=%d want %d", c.a, c.off, c.floor, got, c.want)
		}
	}
}

func TestFanPairTickWritesPairB(t *testing.T) {
	fakeFans(t, 40, 40, 40, 40)
	fanPairTick(12, "")
	for i, want := range []int{40, 40, 52, 52} {
		if v, _ := fanLvl(fmt.Sprintf("fan%d", i)); v != want {
			t.Errorf("fan%d=%d want %d", i, v, want)
		}
	}
	// Goldshell's fanctrl steps every fan from its own level: the offset survives, nothing rewritten
	setFanLvl("fan0", 41)
	setFanLvl("fan1", 41)
	setFanLvl("fan2", 53)
	setFanLvl("fan3", 53)
	if n := fanPairTick(12, ""); n != "fans 0+1 41 %, fans 2+3 53 %" {
		t.Errorf("note %q", n)
	}
	// the plan sets all four equal again (intminer): pair B is put back
	for i := 0; i < 4; i++ {
		setFanLvl(fmt.Sprintf("fan%d", i), 60)
	}
	fanPairTick(-15, "")
	if v, _ := fanLvl("fan3"); v != 45 {
		t.Errorf("fan3=%d want 45", v)
	}
	// skipped (Idle / fan lock): left alone
	for i := 0; i < 4; i++ {
		setFanLvl(fmt.Sprintf("fan%d", i), 20)
	}
	fanPairTick(10, "left to Goldshell while the miner is on Idle")
	if v, _ := fanLvl("fan2"); v != 20 {
		t.Errorf("fan2 changed while skipped: %d", v)
	}
}
