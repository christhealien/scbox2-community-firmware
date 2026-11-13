package main

// r7-code: chip-trim code review tests. Each test names the finding it proves (see the r7-code report).

import (
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// F3: readChipTrims must keep exactly the lines the trim library (sclite_chip.c read_trims + the hook's filter)
// applies, so the panel never shows a trim the chips don't run, or hides one they do. The expected list is what
// the real hook does with this file (r7-code/hook/harness.c, built from the kit's sclite_chip.c: chains 0..3,
// 46 chips; the kit's current sclite_chip.c with r7-hook's field_end/whole checks): for a chip named twice the
// last line counts (a 0 line too), "2.0" is chain 2, "12.5.5"/"12.5-" and off-grid "12.50000000001" are skipped.
func TestR7TrimParserMatchesHook(t *testing.T) {
	*authFile = filepath.Join(t.TempDir(), "auth.json")
	lines := []string{"# header", "2 5 -12.5", "2 6 12.50000000001", "1.5 7 -12.5", "2.0 8 -25", "2 9 -12.5 junk", "2 10 -0",
		"2 11 --12.5", "2 12 +50", "2 13 -1e2", "2  14  -25", "02 15 -25", "2 17 .5", "2 19 12.5.5", "2 21 12.5-", "2 23-12.5",
		"2 24 -37.5x", "3 1 -25", "3 1 -50", "3 2 -12.5", "3 2 0", "1 46 25", " 1 3 -25", "1 4 +Inf", "1 5 NaN", "0 6 -100", "0 7 -112.5"}
	os.WriteFile(chipTrimPath(), []byte(strings.Join(lines, "\n")+"\n"), 0o644)
	hook := []string{"0 6 -100", "1 3 -25", "1 46 25", "2 5 -12.5", "2 8 -25", "2 9 -12.5", "2 12 50", "2 14 -25", "2 15 -25", "3 1 -50"}
	var got []string
	for _, tr := range readChipTrims() {
		if tr.Chip >= 1 && tr.Chip <= 46 { // (the hook skips chips past the chain's count)
			got = append(got, fmt.Sprintf("%d %d %g", tr.Chain, tr.Chip, tr.MHz))
		}
	}
	sort.Strings(got)
	sort.Strings(hook)
	if strings.Join(got, "|") != strings.Join(hook, "|") {
		t.Fatalf("panel reads\n  %v\nthe hook applies\n  %v", got, hook)
	}
}

// F3b: the API takes a trim within 1e-9 of the 12.5 grid and writes it verbatim ("12.50000000001"), a line the
// hook skips (its grid test is exact): the panel shows +12.5, the chip runs the board clock.
func TestR7HandlerWritesExactGrid(t *testing.T) {
	*authFile = filepath.Join(t.TempDir(), "auth.json")
	old := chipStatsPath
	chipStatsPath = filepath.Join(t.TempDir(), "none.json")
	defer func() { chipStatsPath = old }()
	rr := httptest.NewRecorder()
	handleChipTrim(rr, httptest.NewRequest("POST", "/api/chiptrim", strings.NewReader(`{"chain":2,"chip":6,"mhz":-12.50000000001}`)))
	b, _ := os.ReadFile(chipTrimPath())
	if strings.Contains(string(b), "12.50000000001") {
		t.Fatalf("wrote an off-grid trim the hook skips: %q (response %s)", b, rr.Body.String())
	}
}

// F2: Undo auto-trims sets every chip the watchdog owns to 0, without first dropping the chips whose trim was
// changed by hand since its last sync (the sync runs only on a tick that gets past the pause checks: while chip
// trims are off or the hook isn't reporting it never runs). The owner's hand trim is wiped.
func TestR7UndoKeepsHandEdit(t *testing.T) {
	s := newCwSim(t, func(int, float64) float64 { return 1 })
	_ = s
	on := true
	chipWatchSet(&on, false)
	cwSetTrim(2, 12, -12.5)
	chipWatchAdopt([]chipTrim{{Chain: 2, Chip: 12, MHz: -12.5}}, "test")
	cwSetTrim(2, 12, -37.5) // the owner's own edit (handleChipTrim writes the file the same way)
	if _, err := chipWatchSet(nil, true); err != nil {
		t.Fatal(err)
	}
	if tr := s.trims(); tr[12] != -37.5 {
		t.Fatalf("undo wiped a hand trim: chip 12 is %v, want -37.5", tr[12])
	}
}

// F1: the trim budget's yardstick (BoardRate) is the untrimmed chips' rate *including the chip being trimmed*,
// which by selection makes ≥ 6× the typical chip's errors. That inflates the yardstick, so the budget only trips
// when the other chips' errors rise far more than the designed 1.5×: here they double and nothing happens.
func TestR7BudgetYardstick(t *testing.T) {
	newCwSim(t, func(int, float64) float64 { return 1 })
	on := true
	chipWatchSet(&on, false)
	st, _ := cwReadStats()
	errs := map[ckey]int{}
	for i := 0; i < 46; i++ {
		errs[ckey{2, i}] = 0
	}
	judge := func(g func(chip int) int) {
		cwMu.Lock()
		defer cwMu.Unlock()
		cwGained = map[ckey]int{}
		for i := 0; i < 46; i++ {
			cwGained[ckey{2, i}] = g(i + 1)
		}
		cwJudge(st, errs, 2)
	}
	// window 1: chip 5 makes 60 errors, the other 45 chips 2 each (1 an hour): chip 5 is slowed
	judge(func(c int) int {
		if c == 5 {
			return 60
		}
		return 2
	})
	if tr := readChipTrims(); len(tr) != 1 || tr[0].Chip != 5 {
		t.Fatalf("setup: chip 5 not slowed: %v", tr)
	}
	cwMu.Lock()
	was := cwSt.BoardRate["2"]
	cwMu.Unlock()
	if was > 1.05 {
		t.Errorf("yardstick %.2f errors/h per untrimmed chip; the untrimmed chips made 1.00 (the trimmed chip's 30/h is in it)", was)
	}
	// window 2: chip 5's trim is judged (its errors fell): kept
	judge(func(c int) int { return 2 })
	// window 3: every untrimmed chip's errors doubled (4 each): the budget should stop new trims on board 2
	judge(func(c int) int {
		if c == 9 {
			return 30 // a new bad chip (7.5x the others)
		}
		return 4
	})
	cwMu.Lock()
	until := cwSt.Budget["2"]
	cwMu.Unlock()
	if until <= cwNow().Unix() {
		t.Fatalf("the untrimmed chips' errors doubled after a trim and the budget didn't trip; trims now %v", readChipTrims())
	}
}

// F5: an auto-trim step being judged whose chip is no longer in the miner's chip list (a dead chip after a
// miner restart: readChipErrors drops chips with no work, no BIST and no errors) is never judged, and its board
// is marked "judging" every window: no new auto-trim on that board, for good.
func TestR7JudgedChipMissingBlocksBoard(t *testing.T) {
	newCwSim(t, func(int, float64) float64 { return 1 })
	on := true
	chipWatchSet(&on, false)
	st, _ := cwReadStats()
	errs := map[ckey]int{}
	for i := 0; i < 46; i++ {
		errs[ckey{2, i}] = 0
	}
	judge := func(e map[ckey]int, g func(chip int) int) {
		cwMu.Lock()
		defer cwMu.Unlock()
		cwGained = map[ckey]int{}
		for k := range e {
			cwGained[k] = g(k.I + 1)
		}
		cwJudge(st, e, 2)
	}
	judge(errs, func(c int) int {
		if c == 5 {
			return 60
		}
		return 1
	})
	gone := map[ckey]int{}
	for k, v := range errs {
		if k.I != 4 { // hook chip 5 drops out of icinfo
			gone[k] = v
		}
	}
	for w := 0; w < 2; w++ { // (window 1 after: chip 5 undone; window 2: chip 21 slowed)
		judge(gone, func(c int) int {
			if c == 21 {
				return 60
			}
			return 1
		})
	}
	tr := map[int]float64{}
	for _, x := range readChipTrims() {
		tr[x.Chip] = x.MHz
	}
	if tr[21] == 0 {
		for _, l := range chipWatchView()["log"].([]map[string]any) {
			t.Log(l["msg"])
		}
		t.Fatalf("board 2 never took another trim after chip 5 left the list: %v", tr)
	}
}

// F6: the strong-chip test takes a chip whose stage partner is slowed (a weak chip) as a candidate and runs it
// faster: the panel's own pair rules say "avoid + on a chip whose partner is weak", and a raised chip pulls its
// partner's voltage down.
func TestR7StrongSkipsPartnerOfSlowedChip(t *testing.T) {
	dErr, dNon := map[int]int{}, map[int]int{}
	for i := 1; i <= 46; i++ {
		dErr[i], dNon[i] = 3, 1000
	}
	dErr[6] = 0 // the cleanest chip; its partner c5 is trimmed −25
	cs := sctCandidates(46, dErr, dNon, []chipTrim{{Chain: 2, Chip: 5, MHz: -25}}, 2)
	for _, c := range cs {
		if c.Chip == 6 {
			t.Fatalf("c6 picked to run faster though its partner c5 is slowed")
		}
	}
}

var _ = time.Second
