package main

import "testing"

// The last line seen has scrolled out of the miner's log (verbose): base rescanned the whole buffer,
// so rejects already explained were put on the next X mark again. Only lines stamped after it are new.
func TestL21RejectScanAfterScroll(t *testing.T) {
	last := ` [2026-11-05 04:10:20] Accepted 00000001 INCS 0 Diff 39k/16.4k`
	lines := []string{
		` [2026-11-05 04:10:10] Rejected 00000001 INCS 0 Diff 20.1k/16.4k (Job not found)`,
		` [2026-11-05 04:10:20] Pool 0: RECV: {"error":null,"id":7,"result":true}`,
		` [2026-11-05 04:10:31] Rejected 00000002 INCS 3 Diff 18k/16.4k (Duplicate share)`,
	}
	if i := rejectScanStart(lines, last, 0, false); i != 2 {
		t.Fatalf("start %d", i)
	}
	if i := rejectScanStart(lines, "no stamp", 0, false); i != len(lines) {
		t.Fatalf("unstamped: start %d", i)
	}
	if i := rejectScanStart(lines, last, 1, true); i != 1 {
		t.Fatalf("found: start %d", i)
	}
}
