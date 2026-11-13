package main

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestChipTrimFile(t *testing.T) {
	*authFile = filepath.Join(t.TempDir(), "auth.json")
	if len(readChipTrims()) != 0 {
		t.Fatal("no file should mean no trims")
	}
	if err := writeChipTrims([]chipTrim{{2, 14, -12.5}, {2, 3, 0}, {2, 30, 12.5}}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(chipTrimPath())
	// the format the hook parses: "chain chip mhz", zeros left out
	want := "# chain chip mhz: per-chip clock offset from the board clock (written by the panel)\n2 14 -12.5\n2 30 12.5\n"
	if string(b) != want {
		t.Fatalf("file:\n%s", b)
	}
	ts := readChipTrims()
	if len(ts) != 2 || ts[0].Chip != 14 || ts[0].MHz != -12.5 || ts[1].Chip != 30 {
		t.Fatalf("read back: %+v", ts)
	}
}

func TestChipStatsView(t *testing.T) {
	*authFile = filepath.Join(t.TempDir(), "auth.json")
	old := chipStatsPath
	defer func() { chipStatsPath = old }()
	chipStatsPath = filepath.Join(t.TempDir(), "stats.json")
	// what the hook writes: index = chip number, clocks in tenths of a MHz
	os.WriteFile(chipStatsPath, []byte(`{"t":1,"chains":[{"chain":2,"chips":3,"base10":6250,"clock10":[6250,6250,6125,6250,6250],"nonces":[0,100,50,150,0]}]}`), 0o644)
	writeChipTrims([]chipTrim{{2, 2, -12.5}})
	v := chipTrimView()
	ch := v["chains"].([]chainView)
	if len(ch) != 1 || ch[0].Base != 625 || len(ch[0].Chips) != 3 {
		t.Fatalf("view: %+v", ch)
	}
	c2 := ch[0].Chips[1]
	// shares are per MHz: chip 2 runs 612.5 of 625 MHz, so its 50 nonces are held against 98.7, not 100
	if c2.Chip != 2 || c2.Clock != 612.5 || c2.Trim != -12.5 || c2.Share != 50.7 {
		t.Fatalf("chip 2: %+v", c2)
	}
	if ch[0].Chips[2].Share != 149 {
		t.Fatalf("chip 3 share: %+v", ch[0].Chips[2])
	}
}

func TestChipStatsTrimmedNotLow(t *testing.T) {
	*authFile = filepath.Join(t.TempDir(), "auth.json")
	old := chipStatsPath
	defer func() { chipStatsPath = old }()
	chipStatsPath = filepath.Join(t.TempDir(), "stats.json")
	// 46 chips at 650 MHz finding 10000 each; chip 21 trimmed -100 (550 MHz) finds 550/650 of that: healthy
	clk, non := "0", "0"
	for i := 1; i <= 46; i++ {
		c, n := 6500, 10000
		if i == 21 {
			c, n = 5500, 8462
		}
		clk += "," + strconv.Itoa(c)
		non += "," + strconv.Itoa(n)
	}
	os.WriteFile(chipStatsPath, []byte(`{"t":1,"chains":[{"chain":3,"chips":46,"base10":6500,"clock10":[`+clk+`],"nonces":[`+non+`]}]}`), 0o644)
	writeChipTrims([]chipTrim{{3, 21, -100}})
	r := chipTrimView()["chains"].([]chainView)[0].Chips[20]
	if r.Chip != 21 || r.Low || r.Share < 99 || r.Share > 101 {
		t.Fatalf("a chip trimmed -100 that finds what its clock should is shown as %+v", r)
	}
}
