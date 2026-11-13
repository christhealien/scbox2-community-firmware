package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestParseRejectLine(t *testing.T) {
	cases := []struct {
		line, reason, board string
		ok                  bool
	}{
		{` [2026-11-04 00:10:01] Pool 0: RECV: {"error":[21,"Job not found",null],"id":1059,"result":false}`, "Job not found (code 21)", "", true},
		{` [2026-11-04 00:10:01] Pool 0: RECV: {"error":"duplicate share","id":7,"result":false}`, "duplicate share", "", true},
		{` [2026-11-04 00:10:01] Pool 0: RECV: {"error":{"code":23,"message":"Low difficulty share"},"id":7,"result":false}`, "Low difficulty share", "", true},
		{` [2026-11-04 00:10:01] Pool 0: RECV: {"error":null,"id":7,"result":false}`, "no reason given", "", true},
		{` [2026-11-04 00:10:01] Rejected 00000003 INCS 2 Diff 16.8k/16.4k (job not found)`, "job not found", "board 2", true},
		{` [2026-11-04 00:10:01] Rejected 00000003 INCS 1 Diff 16.8k/16.4k`, "", "board 1", true},
		{` [2026-11-04 00:10:01] Pool 0: RECV: {"error":null,"id":1059,"result":true}`, "", "", false},
		{` [2026-11-04 00:10:01] Accepted 00000003 INCS 2 Diff 16.8k/16.4k`, "", "", false},
		{` [2026-11-04 00:10:01] Rejected shares: 1 + 0 stale (0.09%)`, "", "", false},
	}
	for _, c := range cases {
		r, b, ok := parseRejectLine(c.line)
		if ok != c.ok || r != c.reason || b != c.board {
			t.Errorf("%q → (%q,%q,%v), want (%q,%q,%v)", c.line, r, b, ok, c.reason, c.board, c.ok)
		}
	}
}

func resetMarks(t *testing.T) {
	t.Helper()
	old := *hrPath
	*hrPath = filepath.Join(t.TempDir(), "hashrate.json")
	hrMu.Lock()
	hrMarks = nil
	pendingReject.reason = ""
	hrMu.Unlock()
	t.Cleanup(func() { *hrPath = old })
}

func TestRejectReasonAfterMark(t *testing.T) {
	resetMarks(t)
	recordMark("X", "share rejected (+1, total 1)")
	scanRejects([]string{
		` [x] Pool 0: RECV: {"error":[21,"Job not found",null],"id":1059,"result":false}`,
		` [x] Rejected 00000003 INCS 2 Diff 16.8k/16.4k (job not found)`,
	})
	hrMu.Lock()
	defer hrMu.Unlock()
	if len(hrMarks) != 1 || !strings.Contains(hrMarks[0].Note, "pool said: Job not found (code 21) · board 2") {
		t.Fatalf("marks %+v", hrMarks)
	}
}

func TestRejectReasonBeforeMark(t *testing.T) {
	resetMarks(t)
	scanRejects([]string{` [x] Pool 0: RECV: {"error":"duplicate share","id":7,"result":false}`})
	recordMark("X", "share rejected (+1, total 2)")
	hrMu.Lock()
	defer hrMu.Unlock()
	if len(hrMarks) != 1 || !strings.Contains(hrMarks[0].Note, "pool said: duplicate share") {
		t.Fatalf("marks %+v", hrMarks)
	}
	if pendingReject.reason != "" {
		t.Fatal("pending not consumed")
	}
}

func TestRejectReasonNotReusedTwice(t *testing.T) {
	resetMarks(t)
	recordMark("X", "a")
	scanRejects([]string{` [x] Pool 0: RECV: {"error":"stale","id":7,"result":false}`})
	scanRejects([]string{` [x] Pool 0: RECV: {"error":"duplicate share","id":8,"result":false}`})
	hrMu.Lock()
	n := hrMarks[0].Note
	p := pendingReject.reason
	hrMu.Unlock()
	if strings.Count(n, "pool said:") != 1 || !strings.Contains(p, "duplicate") {
		t.Fatalf("note %q pending %q", n, p)
	}
}
