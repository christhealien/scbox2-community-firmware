package main

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// R2-ops: "Undo auto-trims" while the strong-chip test runs. The test rewrites the trims file from the trims it
// saw at its start when it ends (or puts its backup back on a stop), so an auto-trim taken off now comes back,
// and the watchdog no longer owns it: it then reads as a hand trim that neither auto-trim nor Undo touches.
// Seen end to end on the mock (b0.c5/c17 -12.5 back after the test, watch "owned" empty).
func TestR2OpsUndoAutoTrimsDuringStrongTest(t *testing.T) {
	sctSetup(t) // owner trims b0.c7 -25, b2.c40 -12.5
	cwMu.Lock()
	oSt, oLoaded := cwSt, cwLoaded
	cwSt, cwLoaded = cwState{Owned: []cwOwned{{Chain: 0, Chip: 7, MHz: -25, Judged: true}}, NoHelp: map[string]int64{}}, true
	cwMu.Unlock()
	t.Cleanup(func() { cwMu.Lock(); cwSt, cwLoaded = oSt, oLoaded; cwMu.Unlock() })
	if e := sctStart(2, 0, 0); e != "" {
		t.Fatal(e)
	}
	w := httptest.NewRecorder()
	handleChipTrim(w, httptest.NewRequest("POST", "/api/chiptrim", strings.NewReader(`{"watch_undo":true}`)))
	refused := strings.Contains(w.Body.String(), `"ok":false`)
	if e := sctStop(); e != "" {
		t.Fatal(e)
	}
	owned := false
	cwMu.Lock()
	for _, o := range cwSt.Owned {
		if o.Chain == 0 && o.Chip == 7 {
			owned = true
		}
	}
	cwMu.Unlock()
	back := sctTrimsOf(0)[7] == -25
	if back && !owned {
		t.Fatalf("Undo during the test (refused=%v): b0.c7 -25 is back in the trims file but auto-trim no longer owns it (an orphan that reads as a hand trim)", refused)
	}
}
