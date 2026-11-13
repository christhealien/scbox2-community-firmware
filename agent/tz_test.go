package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTimeZone(t *testing.T) {
	*authFile = filepath.Join(t.TempDir(), "auth.json")
	saved := time.Local
	defer func() { time.Local = saved }()

	_, names := tzDB()
	if len(names) < 300 {
		t.Fatalf("only %d zones", len(names))
	}
	if _, err := tzData("Not/AZone"); err == nil {
		t.Fatal("unknown zone accepted")
	}
	if err := applyTZ("Asia/Tokyo", "browser"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(tzFilePath())
	if !bytes.HasPrefix(b, []byte("TZif")) || !bytes.HasSuffix(b, []byte("\nJST-9\n")) {
		t.Fatalf("system file isn't TZif with the JST-9 rule (what the miner's uClibc reads)")
	}
	if _, off := time.Now().Zone(); off != 9*3600 {
		t.Fatalf("agent clock offset %d", off)
	}
	if s := loadTZState(); s.Zone != "Asia/Tokyo" || s.By != "browser" {
		t.Fatalf("state %+v", s)
	}
	// after a restart the saved zone comes back, and a missing system file is put back
	os.Remove(tzFilePath())
	time.Local = time.UTC
	loadTZ()
	if time.Local.String() != "Asia/Tokyo" {
		t.Fatalf("after restart: %s", time.Local)
	}
	if _, err := os.Stat(tzFilePath()); err != nil {
		t.Fatal("system file not restored")
	}
	// daylight saving comes with the zone
	if err := applyTZ("Europe/Berlin", "settings"); err != nil {
		t.Fatal(err)
	}
	if _, off := time.Date(2026, 7, 1, 12, 0, 0, 0, time.Local).Zone(); off != 2*3600 {
		t.Fatalf("Berlin summer offset %d", off)
	}
	if _, off := time.Date(2026, 1, 1, 12, 0, 0, 0, time.Local).Zone(); off != 3600 {
		t.Fatalf("Berlin winter offset %d", off)
	}
}

// The embedded rules are current: British Columbia stays on -07 after 2026-11-01 (tzdata 2026b), while
// Los Angeles still falls back to -08.
func TestTZDataCurrent(t *testing.T) {
	at := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	for zone, want := range map[string]int{"America/Vancouver": -7, "America/Los_Angeles": -8, "Asia/Tokyo": 9} {
		data, err := tzData(zone)
		if err != nil {
			t.Fatal(err)
		}
		loc, err := time.LoadLocationFromTZData(zone, data)
		if err != nil {
			t.Fatal(err)
		}
		if _, off := at.In(loc).Zone(); off/3600 != want {
			t.Errorf("%s on 2026-11-02: UTC%+d, want UTC%+d", zone, off/3600, want)
		}
	}
}

// A newer firmware's rules replace the saved system file at start.
func TestTZFileRefreshed(t *testing.T) {
	*authFile = filepath.Join(t.TempDir(), "auth.json")
	if err := applyTZ("America/Vancouver", "settings"); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(tzFilePath(), []byte("old rules"), 0o644)
	loadTZ()
	b, _ := os.ReadFile(tzFilePath())
	want, _ := tzData("America/Vancouver")
	if !bytes.Equal(b, want) {
		t.Fatal("the saved zone file wasn't refreshed")
	}
	time.Local = time.UTC
}
