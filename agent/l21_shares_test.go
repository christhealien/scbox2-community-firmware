package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Two shares refused 10 s apart, both lines read in the same 20-s log poll: each X mark must get
// its own share's reason (base put the first share's reason on the second mark, and none on the first).
func TestL21RejectReasonsInLogOrder(t *testing.T) {
	resetMarks(t)
	now := time.Now().Unix()
	hrMu.Lock()
	hrMarks = []hrMark{{T: now - 12, Kind: "X", Note: "share rejected (+1, total 1)"}, {T: now - 2, Kind: "X", Note: "share rejected (+1, total 2)"}}
	hrMu.Unlock()
	scanRejects([]string{
		` [2026-11-05 04:10:00] Rejected 00000003 INCS 2 Diff 16.8k/16.4k (Job not found)`,
		` [2026-11-05 04:10:05] Accepted 00000001 INCS 0 Diff 39k/16.4k`,
		` [2026-11-05 04:10:10] Rejected 00000001 INCS 0 Diff 20.1k/16.4k (Duplicate share)`,
	})
	hrMu.Lock()
	defer hrMu.Unlock()
	if !strings.Contains(hrMarks[0].Note, "pool said: Job not found · board 2") ||
		!strings.Contains(hrMarks[1].Note, "pool said: Duplicate share · board 0") {
		t.Fatalf("marks %+v", hrMarks)
	}
}

// One poll saw +2 rejects (one X mark): both reasons belong to it, none is parked for a later reject.
func TestL21RejectMarkTakesOneReasonPerShare(t *testing.T) {
	resetMarks(t)
	recordMark("X", "share rejected (+2, total 2)")
	scanRejects([]string{
		` [x] Rejected 00000003 INCS 2 Diff 16.8k/16.4k (Job not found)`,
		` [x] Rejected 00000001 INCS 0 Diff 20.1k/16.4k (Duplicate share)`,
	})
	hrMu.Lock()
	n, p := hrMarks[0].Note, pendingReject.reason
	hrMu.Unlock()
	if !strings.Contains(n, "Job not found") || !strings.Contains(n, "Duplicate share") || p != "" {
		t.Fatalf("note %q pending %q", n, p)
	}
}

// bfgminer's format "%s %02x%02x%02x%02x %-6s%s Diff %s/%s%s %s%s" ends a share sent again after a
// reconnect with "(resubmit)": that isn't the pool's reason.
func TestL21RejectResubmitSuffix(t *testing.T) {
	r, b, ok := parseRejectLine(` [2026-11-05 04:10:00] Rejected 0000a1b2 INCS 1 Diff 20.1k/16.4k (Duplicate share) (resubmit)`)
	if !ok || r != "Duplicate share" || b != "board 1" {
		t.Fatalf("got (%q,%q,%v)", r, b, ok)
	}
}

// The clock jumps forward (set from the browser before NTP, then corrected): the miner didn't restart,
// so its best share must not be booked a second time.
func TestL21BestShareClockJumpNoDuplicate(t *testing.T) {
	*authFile = filepath.Join(t.TempDir(), "auth.json")
	now := time.Now().Unix()
	bestBookMu.Lock()
	sharesRead = true
	shares = shareBook{History: []shareHit{{V: 1263000, At: now - 4000}}, NowV: 1263000, NowAt: now - 4000, Since: now - 5000 - 3600}
	bestBookMu.Unlock()
	shareSeen = true
	shareObserve(dashboard{OK: true, BestShare: 1263000, Elapsed: 5000})
	bestBookMu.Lock()
	defer bestBookMu.Unlock()
	if len(shares.History) != 1 {
		t.Fatalf("history %+v", shares.History)
	}
}

// The clock goes back an hour: the chart must keep recording (base waited for the clock to pass the
// "future" sample).
func TestL21HashrateClockBack(t *testing.T) {
	resetMarks(t)
	now := time.Now().Unix()
	hrMu.Lock()
	oldH, oldTS := hrHist, hrLastTS
	hrHist, hrLastTS = []hrSample{{T: now - 3000, GHS: 1}, {T: now + 3600, GHS: 2}}, now+3600
	hrMu.Unlock()
	t.Cleanup(func() { hrMu.Lock(); hrHist, hrLastTS = oldH, oldTS; hrMu.Unlock() })
	recordHashrate(3)
	hrMu.Lock()
	defer hrMu.Unlock()
	if n := len(hrHist); n == 0 || hrHist[n-1].GHS != 3 || hrHist[n-1].T > now+5 {
		t.Fatalf("samples %+v", hrHist)
	}
}

// A pool refusing many shares must not push the restart mark off the chart.
func TestL21MarkCapKeepsRestart(t *testing.T) {
	resetMarks(t)
	now := time.Now().Unix()
	hrMu.Lock()
	hrMarks = []hrMark{{T: now - 5000, Kind: "R", Note: "miner restarted"}}
	for i := 0; i < 199; i++ {
		hrMarks = append(hrMarks, hrMark{T: now - 4000 + int64(i*10), Kind: "X", Note: "share rejected (+1, total 1)"})
	}
	hrMu.Unlock()
	recordMark("X", "share rejected (+1, total 200)")
	hrMu.Lock()
	defer hrMu.Unlock()
	if len(hrMarks) != 200 || hrMarks[0].Kind != "R" {
		t.Fatalf("%d marks, first %+v", len(hrMarks), hrMarks[0])
	}
}
