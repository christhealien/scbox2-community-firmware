package main

// Commissioning lane C5 (chip trims on the SC Box II): inert while not commissioned, the Box II geometry (1 board,
// 36 chips / 18 pairs) and the 690 MHz "+" ceiling once commissioned, and the SC Lite unchanged.

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// c5Setup: a model profile, a temp config dir, a temp stats file, no real miner restart / clock read.
func c5Setup(t *testing.T, p modelProfile, trims bool) {
	t.Helper()
	oldModel, oldAuth, oldStats, oldRestart, oldSet := model, *authFile, chipStatsPath, chipHookRestart, reapplySetClock
	oldMin, oldMax, oldHard := freqMin, freqMax, freqHardMax
	oldExp := expectedChips
	dir := t.TempDir()
	*authFile = filepath.Join(dir, "auth.json")
	chipStatsPath = filepath.Join(dir, "chipstats.json")
	model = p
	model.Feat.ChipTrims = trims
	if trims {
		model.Feat.ClockControl = true
	}
	setWindowsFromModel()
	expectedChips = model.ChipsPerBoard
	chipHookRestart = func() { t.Error("unexpected miner restart") }
	reapplySetClock = func() (int, bool) { return 690, true }
	t.Cleanup(func() {
		model, *authFile, chipStatsPath, chipHookRestart, reapplySetClock = oldModel, oldAuth, oldStats, oldRestart, oldSet
		freqMin, freqMax, freqHardMax = oldMin, oldMax, oldHard
		expectedChips = oldExp
	})
}

func c5Post(t *testing.T, body string) map[string]any {
	t.Helper()
	r := httptest.NewRequest("POST", "/api/chiptrim", strings.NewReader(body))
	w := httptest.NewRecorder()
	handleChipTrim(w, r)
	var out map[string]any
	json.Unmarshal(w.Body.Bytes(), &out)
	return out
}

func c5Exists(p string) bool { _, err := os.Stat(p); return err == nil }

// A chiphook flag left over (commissioned once, switched off again in Settings → Commissioning) is removed at start,
// with the reason kept for the card, and a miner running with the trim library is restarted without it.
func TestC5Box2OffRemovesLeftoverFlagAtStart(t *testing.T) {
	c5Setup(t, profSCBox2, false)
	os.WriteFile(chipHookFlag(), []byte("on\n"), 0o644)
	// the running miner has the trim library loaded
	fakeProc(t, "intminer")
	os.WriteFile(filepath.Join(procRoot, "1", "maps"), []byte("b6f00000-b6f02000 r-xp 00000000 1f:00 77 /lib/"+brandChipLib+"\n"), 0o644)
	restarted := 0
	chipHookRestart = func() { restarted++ }
	chipHookInertAtStart()
	if c5Exists(chipHookFlag()) {
		t.Fatal("ChipTrims off on the Box II: the chiphook flag must not survive the start")
	}
	if restarted != 1 {
		t.Fatalf("the miner running with the trim library must be restarted once, got %d", restarted)
	}
	b, err := os.ReadFile(chipHookFlag() + ".off")
	if err != nil || !strings.Contains(string(b), "Commissioning") {
		t.Fatalf("the card must say why trims are off: %q %v", b, err)
	}
	// no flag: nothing happens (no restart)
	chipHookInertAtStart()
	if restarted != 1 {
		t.Fatal("restarted again with no flag")
	}
}

func TestC5FlagKeptWhereTrimsAreOn(t *testing.T) {
	for _, tc := range []struct {
		name  string
		p     modelProfile
		trims bool
	}{{"Box II commissioned", profSCBox2, true}, {"SC Lite", profSCLite, true}, {"unknown profile (misread product file)", profUnknown, false}} {
		t.Run(tc.name, func(t *testing.T) {
			c5Setup(t, tc.p, tc.trims)
			os.WriteFile(chipHookFlag(), []byte("on\n"), 0o644)
			chipHookInertAtStart()
			if !c5Exists(chipHookFlag()) {
				t.Fatal("the flag must be left alone here")
			}
		})
	}
}

// Off: the API refuses everything, switching on never writes the flag, the loops don't start.
func TestC5Box2OffAPIRefusesAndNoFlag(t *testing.T) {
	c5Setup(t, profSCBox2, false)
	for _, body := range []string{`{"enable":true}`, `{"chain":0,"chip":3,"mhz":-25}`, `{"apply":true}`, `{"watch":true}`} {
		out := c5Post(t, body)
		if out["ok"] != false || out["disabled"] != true {
			t.Fatalf("%s: want a disabled refusal, got %v", body, out)
		}
	}
	r := httptest.NewRequest("GET", "/api/chiptrim", nil)
	w := httptest.NewRecorder()
	handleChipTrim(w, r)
	if !strings.Contains(w.Body.String(), `"disabled":true`) {
		t.Fatalf("GET must be refused too: %s", w.Body.String())
	}
	if c5Exists(chipHookFlag()) || c5Exists(chipTrimPath()) {
		t.Fatal("nothing may be written with ChipTrims off")
	}
	chipWatchLoop() // returns at once with the feature off (it would loop for ever otherwise)
	// the tuner can be commissioned without chip trims: its "trim weak chips" must be refused
	c := defaultTunerConfig()
	c.ChipTrim, c.DryRun = true, false
	if err := validateTunerConfig(c); err == nil || !strings.Contains(err.Error(), "chip trims are not available") {
		t.Fatalf("tuner chip trims with ChipTrims off: %v", err)
	}
}

// On: the Box II's geometry (board 0 only, chips 1..36, pairs 2k-1/2k, 18 of them) and the 690 MHz ceiling.
func TestC5Box2OnGeometryAndLimits(t *testing.T) {
	c5Setup(t, profSCBox2, true)
	g := chipGeo()
	if g["chips"] != 36 || g["pairs"] != 18 || g["box2"] != true || g["fix"] != "lower the board clock a step" {
		t.Fatalf("geo %v", g)
	}
	if chipPartner(35) != 36 || chipPartner(36) != 35 || chipPartner(1) != 2 || chipPartner(37) != 0 {
		t.Fatal("Box II pairs are 1+2 … 35+36")
	}
	for _, tc := range []struct {
		body string
		ok   bool
		err  string
	}{
		{`{"chain":0,"chip":36,"mhz":-25}`, true, ""},
		{`{"chain":0,"chip":37,"mhz":-25}`, false, "no such chip"},
		{`{"chain":1,"chip":3,"mhz":-25}`, false, "no such chip"},
		{`{"chain":0,"chip":3,"mhz":-112.5}`, false, "a trim is"},
		{`{"chain":0,"chip":3,"mhz":12.5}`, false, "past the 690 MHz"}, // board at 690: no "+" at all
	} {
		out := c5Post(t, tc.body)
		if (out["ok"] != false) != tc.ok || (!tc.ok && !strings.Contains(out["err"].(string), tc.err)) {
			t.Fatalf("%s: %v", tc.body, out)
		}
	}
	// at the Low-power plan's 525 MHz a "+50" fits (575 <= 690)
	reapplySetClock = func() (int, bool) { return 525, true }
	if out := c5Post(t, `{"chain":0,"chip":4,"mhz":50}`); out["ok"] == false {
		t.Fatalf("+50 at 525: %v", out)
	}
	ts := readChipTrims()
	if len(ts) != 2 || ts[0] != (chipTrim{0, 4, 50}) || ts[1] != (chipTrim{0, 36, -25}) {
		t.Fatalf("trims file %v", ts)
	}
	// even with a wider clock window (an overclock unlock) a "+" trim never passes the top stock clock: the trim
	// library (scbox2_chip.c CHIP_CEIL) would skip it, so the panel refuses it
	freqMax = 750
	if chipTrimCeil() != 690 {
		t.Fatalf("Box II trim ceiling %d, want 690", chipTrimCeil())
	}
	// (plusTrimErr only checks with trims switched on)
	os.WriteFile(chipHookFlag(), []byte("on\n"), 0o644)
	if err := plusTrimErr(650); err == nil || !strings.Contains(err.Error(), "690") { // 650+50 = 700 > 690
		t.Fatalf("plusTrimErr at 650 with a +50 chip: %v", err)
	}
	if err := plusTrimErr(640); err != nil {
		t.Fatalf("640+50 = 690 fits: %v", err)
	}
}

// The panel reads the stats file exactly as the Box II library writes it (this JSON is the library's own output
// from the qemu run, lanes/C5-chiptrims/qemu: chain 0, 36 chips, chip 3 trimmed -25 at 690).
func TestC5Box2StatsFromLibrary(t *testing.T) {
	c5Setup(t, profSCBox2, true)
	const lib = `{"t":1794307966,"chains":[{"chain":0,"chips":36,"base10":6900,"clock10":[0,6900,6900,6650,6900,6900,6900,6900,6900,6900,6900,6900,6900,6900,6900,6900,6900,6900,6900,6900,6900,6900,6900,6900,6900,6900,6900,6900,6900,6900,6900,6900,6900,6900,6900,6900,6900,0],"nonces":[0,101,102,103,104,105,106,107,108,109,110,111,112,113,114,115,116,117,118,119,120,121,122,123,124,125,126,127,128,129,130,131,132,133,134,135,136,0]}]}`
	os.WriteFile(chipStatsPath, []byte(lib+"\n"), 0o644)
	writeChipTrims([]chipTrim{{0, 3, -25}})
	chipSegReset()
	v := chipTrimView()
	cs := v["chains"].([]chainView)
	if len(cs) != 1 || cs[0].Chain != 0 || cs[0].Base != 690 || len(cs[0].Chips) != 36 {
		t.Fatalf("view %+v", cs)
	}
	if r := cs[0].Chips[2]; r.Chip != 3 || r.Clock != 665 || r.Trim != -25 || r.Nonces != 103 {
		t.Fatalf("chip 3 %+v", r)
	}
	if r := cs[0].Chips[35]; r.Chip != 36 || r.Clock != 690 || r.Nonces != 136 {
		t.Fatalf("chip 36 %+v", r)
	}
	if v["geo"].(map[string]any)["chips"] != 36 {
		t.Fatal("geo missing from the view")
	}
}

// The SC Lite: every helper gives exactly what the shared code used before.
func TestC5SCLiteUnchanged(t *testing.T) {
	c5Setup(t, profSCLite, true)
	if chipTrimCeil() != clockWindowHi() {
		t.Fatal("SC Lite trim ceiling must be the clock window")
	}
	g := chipGeo()
	if g["chips"] != 46 || g["pairs"] != 23 || g["box2"] != false || g["fix"] != "raise PV a step" {
		t.Fatalf("SC Lite geo %v", g)
	}
	for c := 0; c <= 4; c++ {
		if chipTrimBoardOK(c) != (c <= 3) {
			t.Fatalf("SC Lite board %d", c)
		}
	}
	if !strings.Contains(indexHTML, "G=c.geo||{chips:46,pairs:23,fix:'raise PV a step'}") {
		t.Fatal("the card's SC Lite defaults changed")
	}
}

// Switched off (v0.4.3): the trims go with the feature, not just the flag. A trim left in the file survived the
// switch-off in v0.4.2 and stayed on the chip until it was reset by hand.
func TestC5Box2OffClearsTrims(t *testing.T) {
	c5Setup(t, profSCBox2, false)
	os.WriteFile(chipTrimPath(), []byte("# header\n0 26 -12.5\n0 7 -25\n"), 0o644)
	os.WriteFile(chipHookFlag(), []byte("on\n"), 0o644)
	chipHookRestart = func() {}
	chipHookInertAtStart()
	if n := len(readChipTrims()); n != 0 {
		t.Fatalf("ChipTrims off: %d trim(s) survived the start, want 0", n)
	}
	if c5Exists(chipHookFlag()) {
		t.Fatal("ChipTrims off: the chiphook flag must not survive the start")
	}
	// a file with no trims is left alone (no rewrite needed)
	chipHookInertAtStart()
	if n := len(readChipTrims()); n != 0 {
		t.Fatal("trims reappeared")
	}
}
