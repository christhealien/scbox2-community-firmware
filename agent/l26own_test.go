package main

import (
	"bytes"
	"net/http/httptest"
	"testing"
)

// L26: auto-trim slowed b2.c7 from -12.5 to -25 and is judging that step; the owner then sets the chip to -12.5 by
// hand. That's a hand trim now: the watchdog must leave it alone (and Undo auto-trims must not take it off).
func TestL26HandEditToPrevIsHand(t *testing.T) {
	r7Dir(t)
	r7ClockStub(t, 600)
	writeChipTrims([]chipTrim{{2, 7, -25}})
	cwMu.Lock()
	cwSt = cwState{On: true, NoHelp: map[string]int64{}, Owned: []cwOwned{{Chain: 2, Chip: 7, MHz: -25, Prev: -12.5, Before: 5, Since: 1}}}
	cwLoaded = true
	cwMu.Unlock()
	rr := httptest.NewRecorder()
	handleChipTrim(rr, httptest.NewRequest("POST", "/api/chiptrim", bytes.NewReader([]byte(`{"chain":2,"chip":7,"mhz":-12.5}`))))
	if got := readChipTrims(); len(got) != 1 || got[0].MHz != -12.5 {
		t.Fatalf("hand edit not saved: %v %s", got, rr.Body.String())
	}
	cwMu.Lock()
	cwSyncOwned()
	owned := append([]cwOwned{}, cwSt.Owned...)
	cwMu.Unlock()
	for _, o := range owned {
		if o.Chain == 2 && o.Chip == 7 {
			t.Errorf("the owner's hand trim is still counted as auto-trim's: %+v", o)
		}
	}
	if _, err := chipWatchSet(nil, true); err != nil {
		t.Fatal(err)
	}
	if got := readChipTrims(); len(got) != 1 || got[0].MHz != -12.5 {
		t.Errorf("Undo auto-trims took the owner's hand trim off: %v", got)
	}
}
