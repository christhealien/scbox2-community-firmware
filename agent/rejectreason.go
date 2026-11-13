package main

// Reject reasons: when the pool refuses a share, the mining log (minersyslog) carries the pool's
// answer, e.g. `Pool 0: RECV: {"error":[21,"Job not found",null],"id":1059,"result":false}` and
// bfgminer's own `Rejected 00000003 INCS 2 Diff 16.8k/16.4k (job not found)`. The log scrolls
// away within minutes (seconds in verbose), so we keep the reason on the chart's X mark, where it
// stays for 24 h. The X mark itself comes from the API's Rejected counter (dashboard poll), and the
// log line can arrive before or after it, so both orders are handled.

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

var (
	rejLineRe = regexp.MustCompile(`\bRejected [0-9a-fA-F]{8} +(INCS ?\d+|[A-Z]{2,5} ?\d+)?.*?Diff (\S+)(.*)$`)
	rejRecvRe = regexp.MustCompile(`RECV: (\{.*"result"\s*:\s*false.*\})\s*$`)
	parenRe   = regexp.MustCompile(`\(([^()]{1,120})\)\s*$`)
	// bfgminer prints "… Diff 20k/16.4k (Duplicate share) (resubmit)" for a share sent again after a
	// reconnect: that marker is not the pool's reason
	resubRe = regexp.MustCompile(`\s*\(resubmit\)\s*$`)
	// the count in an X mark's note: "share rejected (+3, total 9)"
	markNRe = regexp.MustCompile(`\(\+(\d+),`)
)

// pendingReject is a reason seen in the log that has no X mark yet.
var pendingReject struct {
	t      int64
	reason string
}

const rejectMatchWindow = 180 // seconds between the log line and the counter change

// stratumErrText pulls a human message out of a stratum error value: [code,"msg",…],
// {"code":…,"message":"…"} or a bare string.
func stratumErrText(raw json.RawMessage) string {
	var arr []any
	if json.Unmarshal(raw, &arr) == nil && len(arr) >= 2 {
		if s, ok := arr[1].(string); ok && s != "" {
			if c, ok := arr[0].(float64); ok {
				return fmt.Sprintf("%s (code %d)", s, int(c))
			}
			return s
		}
	}
	var obj struct {
		Code    any    `json:"code"`
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &obj) == nil && obj.Message != "" {
		return obj.Message
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return ""
}

// parseRejectLine returns the pool's reason (and board, when the line names one) for a log line
// that reports a refused share; ok=false for every other line.
func parseRejectLine(l string) (reason, board string, ok bool) {
	if m := rejRecvRe.FindStringSubmatch(l); m != nil {
		var o struct {
			Error json.RawMessage `json:"error"`
		}
		if json.Unmarshal([]byte(m[1]), &o) != nil {
			return "", "", false
		}
		r := stratumErrText(o.Error)
		if r == "" {
			r = "no reason given"
		}
		return cleanReason(r), "", true
	}
	if m := rejLineRe.FindStringSubmatch(l); m != nil {
		if strings.HasPrefix(m[1], "INCS") {
			board = "board " + strings.TrimSpace(strings.TrimPrefix(m[1], "INCS"))
		}
		if p := parenRe.FindStringSubmatch(resubRe.ReplaceAllString(m[3], "")); p != nil {
			reason = cleanReason(p[1])
		}
		return reason, board, true
	}
	return "", "", false
}

func cleanReason(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return -1
		}
		return r
	}, strings.TrimSpace(s))
	if len(s) > 100 {
		i := 100
		for i > 0 && !utf8.RuneStart(s[i]) {
			i--
		}
		s = s[:i] + "…"
	}
	return s
}

// scanRejects feeds new minersyslog lines; called from pollDTFS (every 20 s, viewers or not).
// Each refused share gets its own reason, in log order: bfgminer logs the pool's RECV answer (in
// verbose) and then its own "Rejected …" line for the same share.
func scanRejects(lines []string) {
	now := time.Now().Unix()
	var texts []string
	recv, open := "", false
	emit := func(reason, board string) {
		text := reason
		if text == "" {
			text = "no reason in the log"
		}
		if board != "" {
			text += " · " + board
		}
		texts = append(texts, "pool said: "+text)
	}
	for _, l := range lines {
		r, b, ok := parseRejectLine(l)
		if !ok {
			continue
		}
		if !rejLineRe.MatchString(l) { // a RECV answer: its Rejected line follows
			if open {
				emit(recv, "")
			}
			recv, open = r, true
			continue
		}
		// the RECV line and the Rejected line describe the same share: merge, prefer the text reason
		reason := r
		if open && recv != "" && (recv != "no reason given" || reason == "") {
			reason = recv
		}
		emit(reason, b)
		recv, open = "", false
	}
	if open {
		emit(recv, "")
	}
	for _, t := range texts {
		attachRejectReason(now, t)
	}
	// the reason also goes to the activity feed, so it is in the panel's events (and their log export) after
	// the chart marks are gone; a burst is summed up rather than flooding the feed
	const maxFeed = 5
	for i, t := range texts {
		if i == maxFeed {
			activity.emit(fmt.Sprintf("✗ … and %d more rejected share(s) in this burst", len(texts)-maxFeed))
			break
		}
		activity.emit("✗ rejected share — " + t)
	}
}

// markRoom: how many more reasons an X mark takes (one per share it stands for).
func markRoom(m *hrMark) int {
	n := 1
	if s := markNRe.FindStringSubmatch(m.Note); s != nil {
		fmt.Sscan(s[1], &n)
	}
	return n - strings.Count(m.Note, "pool said:")
}

// attachRejectReason adds the reason to the oldest X mark (within the window) that still lacks
// one, so reasons line up with the shares in log order, or parks it for the next X mark.
func attachRejectReason(now int64, text string) {
	hrMu.Lock()
	var target *hrMark
	for i := len(hrMarks) - 1; i >= 0; i-- {
		m := &hrMarks[i]
		if now-m.T > rejectMatchWindow {
			break
		}
		if m.Kind == "X" && markRoom(m) > 0 {
			target = m
		}
	}
	if target != nil {
		target.Note = strings.TrimSpace(target.Note + " · " + text)
		hrMu.Unlock() // saved with its mark, in the 5-minute save (rejects can come often: flash wear)
		return
	}
	pendingReject.t, pendingReject.reason = now, text
	hrMu.Unlock()
}

// takePendingReject is used by recordMark for an X mark (hrMu held by the caller).
func takePendingReject(now int64) string {
	if pendingReject.reason == "" || now-pendingReject.t > rejectMatchWindow {
		pendingReject.reason = ""
		return ""
	}
	r := pendingReject.reason
	pendingReject.reason = ""
	return r
}
