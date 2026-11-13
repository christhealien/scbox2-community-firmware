package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestMigrate156(t *testing.T) {
	d := t.TempDir()
	*authFile = filepath.Join(d, "auth.json")
	*presetPath = filepath.Join(d, "presets.json")
	old := []preset{{Name: "Quiet"}, {Name: "Eco"}}
	if err := savePresets(old); err != nil {
		t.Fatal(err)
	}
	migrateStock = func() error { return nil }
	t.Cleanup(func() { migrateStock = applyStockPlan })
	setCharge("preset:Quiet")
	t.Cleanup(func() { setCharge("") })

	migrate156(0)
	<-migrate156Done
	if n := len(loadPresets()); n != 0 {
		t.Fatalf("presets left: %d", n)
	}
	if getCharge() != "" {
		t.Fatalf("charge %q", getCharge())
	}
	m, ok := loadMigration156()
	if !ok || m.Presets != 2 || m.Backup == "" {
		t.Fatalf("marker %+v %v", m, ok)
	}
	var bk []preset
	b, _ := os.ReadFile(m.Backup)
	if json.Unmarshal(b, &bk) != nil || len(bk) != 2 {
		t.Fatalf("backup %s", b)
	}

	// runs once: a new preset made after the upgrade survives a second start
	savePresets([]preset{{Name: "Eco", MHz: 500}})
	migrate156(0)
	if n := len(loadPresets()); n != 1 {
		t.Fatalf("second start touched presets: %d", n)
	}
	// put back: the old ones join, the new "Eco" wins over the old one
	n, err := restorePresets156()
	if err != nil || n != 1 || len(loadPresets()) != 2 {
		t.Fatalf("restore n=%d err=%v have %d", n, err, len(loadPresets()))
	}
	if m, _ := loadMigration156(); m.Restored == 0 {
		t.Fatal("restore not recorded")
	}
}

func TestMigrate156NothingToDo(t *testing.T) {
	d := t.TempDir()
	*authFile = filepath.Join(d, "auth.json")
	*presetPath = filepath.Join(d, "presets.json")
	migrateStock = func() error { return nil }
	t.Cleanup(func() { migrateStock = applyStockPlan })
	migrate156(0)
	<-migrate156Done
	if _, err := restorePresets156(); err == nil {
		t.Fatal("restore with nothing moved aside should say so")
	}
}
