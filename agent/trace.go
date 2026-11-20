package main

// Breadcrumbs for a freeze hunt (v0.5.4, the SC Box II): while a tuner run or the clock-ceiling probe holds the miner
// (and a little after), every call of the panel's that reaches the mining program, its backend or the kernel is
// written to the system log (/dev/log, tag "scbox2-trace"), so a relay to another computer (Settings → Troubleshooting →
// "send the system log to this computer", busybox syslogd -R) has shipped each line before a freeze can take it:
//
//   - writes (a plan PUT to the OEM's backend, a miner API command with a parameter, a settings-partition write
//     window, a fan level written to sysfs, a mining-program restart): one line when it starts ("▶") and one when it
//     ends ("◀", with its duration), so the last line before a freeze names what was in flight;
//   - reads (the miner API's summary/devs/pools, GETs from the backend): counted, one summary line every 10 s per
//     window (count, errors, slowest), plus any read still in flight for over 3 s;
//   - the tuner's own log lines and the clock/voltage confirmation (start, polls, result).
//
// It can't flood: at most traceBurst lines at once and traceRate a second after that; what is dropped is counted in
// the next summary. Nothing here touches the hardware or the flash, and with no run active it does nothing but one
// atomic load per call.

import (
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	traceTag    = "scbox2-trace"
	traceEvery  = 10 * time.Second // the reads' summary
	traceSlow   = 3 * time.Second  // a read in flight this long is named in the summary
	traceLinger = 2 * time.Minute  // tracing goes on this long after the run ends (its restore, a late confirm)
	traceBurst  = 40
	traceRate   = 4.0 // lines per second after the burst
)

var (
	// traceSink: where a line goes (tests); nil = the system log.
	traceSink func(line string)
	// traceForce: tracing on whatever runs (tests).
	traceForce atomic.Bool
	// traceHeld: a tuner run or the probe holds the miner; traceUntil: unix nanos until which tracing lingers after.
	traceHeld    atomic.Bool
	traceUntil   atomic.Int64
	traceLogPath = "/dev/log"

	trMu      sync.Mutex
	trConnMu  sync.Mutex // trConn and trDialAt only; never held while dialling or writing (traceSyslog)
	trConn    net.Conn
	trDialAt  time.Time
	trTokens  = float64(traceBurst)
	trTokenAt time.Time
	trDropped int
	trReads   = map[string]*traceAgg{}
	trFlight  = map[int64]traceFlight{}
	trNextID  int64
	trLoop    sync.Once
)

type traceAgg struct {
	n, errs int
	max     time.Duration
}

type traceFlight struct {
	what string
	at   time.Time
}

// traceCharge: the panel's charge holder changed (setCharge): a tuner run or the probe turns tracing on.
func traceCharge(by string) {
	if by == "tuner" || by == "ceiling" {
		if !traceHeld.Swap(true) {
			trLoop.Do(func() { go traceLoop() })
			traceLine("trace on: the %s holds the miner", by)
		}
		return
	}
	if traceHeld.Swap(false) { // it ended: linger (its restore, a late confirmation), then stop
		traceUntil.Store(time.Now().Add(traceLinger).UnixNano())
		traceLine("the run ended (now %q): trace off in %s", by, traceLinger)
	}
}

// traceOn: is tracing on now? (One atomic load when it isn't.)
func traceOn() bool {
	if traceForce.Load() || traceHeld.Load() {
		return true
	}
	u := traceUntil.Load()
	return u != 0 && time.Now().UnixNano() < u
}

// traceWrite: a hardware-touching action starts; the returned func ends it (err nil = ok). Off: a no-op.
func traceWrite(what string) func(err error) {
	if !traceOn() {
		return func(error) {}
	}
	what = traceClip(what)
	t0 := time.Now()
	id := traceFlightAdd(what, t0)
	traceLine("▶ %s", what)
	return func(err error) {
		traceFlightDone(id)
		res := "ok"
		if err != nil {
			res = "ERR " + traceClip(err.Error())
		}
		traceLine("◀ %s %s %s", what, res, time.Since(t0).Round(time.Millisecond))
	}
}

// traceRead: a read starts; the returned func counts it (its duration and error) into the next summary.
func traceRead(what string) func(err error) {
	if !traceOn() {
		return func(error) {}
	}
	t0 := time.Now()
	id := traceFlightAdd(what, t0)
	return func(err error) {
		d := time.Since(t0)
		trMu.Lock()
		delete(trFlight, id)
		a := trReads[what]
		if a == nil {
			a = &traceAgg{}
			trReads[what] = a
		}
		a.n++
		if err != nil {
			a.errs++
		}
		if d > a.max {
			a.max = d
		}
		trMu.Unlock()
	}
}

// traceNote: one line (the tuner's log, the confirmation), when tracing is on.
func traceNote(format string, a ...any) {
	if traceOn() {
		traceLine("%s", traceClip(fmt.Sprintf(format, a...)))
	}
}

func traceFlightAdd(what string, at time.Time) int64 {
	trMu.Lock()
	defer trMu.Unlock()
	trNextID++
	trFlight[trNextID] = traceFlight{what, at}
	return trNextID
}

func traceFlightDone(id int64) {
	trMu.Lock()
	delete(trFlight, id)
	trMu.Unlock()
}

// traceLoop: the reads' summary every traceEvery while tracing is on.
func traceLoop() {
	for {
		time.Sleep(traceEvery)
		if s := traceSummary(time.Now()); s != "" && traceOn() {
			traceLine("%s", s)
		}
	}
}

// traceSummary: the reads since the last summary, the dropped lines and the slow reads in flight ("" = nothing).
func traceSummary(now time.Time) string {
	trMu.Lock()
	defer trMu.Unlock()
	var parts []string
	keys := make([]string, 0, len(trReads))
	for k := range trReads {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		a := trReads[k]
		p := fmt.Sprintf("%s×%d max %s", k, a.n, a.max.Round(time.Millisecond))
		if a.errs > 0 {
			p += fmt.Sprintf(" (%d failed)", a.errs)
		}
		parts = append(parts, p)
	}
	trReads = map[string]*traceAgg{}
	var slow []string
	for _, f := range trFlight {
		if d := now.Sub(f.at); d >= traceSlow {
			slow = append(slow, fmt.Sprintf("%s %s", f.what, d.Round(time.Second)))
		}
	}
	sort.Strings(slow)
	out := ""
	if len(parts) > 0 {
		out = "reads " + traceEvery.String() + ": " + strings.Join(parts, ", ")
	}
	if len(slow) > 0 {
		if out != "" {
			out += "; "
		}
		out += "IN FLIGHT: " + strings.Join(slow, ", ")
	}
	if trDropped > 0 {
		if out != "" {
			out += "; "
		}
		out += fmt.Sprintf("%d trace line(s) dropped (rate limit)", trDropped)
		trDropped = 0
	}
	return out
}

func traceClip(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// traceLine: one line to the system log, rate-limited (a token bucket).
func traceLine(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	trMu.Lock()
	now := time.Now()
	if !trTokenAt.IsZero() {
		trTokens = min(float64(traceBurst), trTokens+now.Sub(trTokenAt).Seconds()*traceRate)
	}
	trTokenAt = now
	if trTokens < 1 {
		trDropped++
		trMu.Unlock()
		return
	}
	trTokens--
	sink := traceSink
	trMu.Unlock()
	if sink != nil {
		sink(msg)
		return
	}
	traceSyslog(now, msg)
}

// traceSyslog: one datagram to /dev/log in the classic BSD format busybox syslogd reads (user.notice). Never
// blocks for long (a 200 ms deadline); a syslogd that was restarted is dialled again (at most once a second). The
// line is built first, and neither trMu nor trConnMu is held while dialling or writing (a syslogd that is slow to
// read /dev/log holds up only this line, not the next trace call's bookkeeping).
func traceSyslog(now time.Time, msg string) {
	line := []byte(fmt.Sprintf("<13>%s %s: %s", now.Format(time.Stamp), traceTag, msg))
	trConnMu.Lock()
	c := trConn
	if c == nil {
		if now.Sub(trDialAt) < time.Second {
			trConnMu.Unlock()
			return
		}
		trDialAt = now
	}
	trConnMu.Unlock()
	if c == nil {
		nc, err := net.DialTimeout("unixgram", traceLogPath, 200*time.Millisecond)
		if err != nil {
			return
		}
		trConnMu.Lock()
		if trConn == nil {
			trConn = nc
		} else { // another line dialled meanwhile: use that one
			nc.Close()
		}
		c = trConn
		trConnMu.Unlock()
	}
	c.SetWriteDeadline(time.Now().Add(200 * time.Millisecond))
	if _, err := c.Write(line); err != nil {
		trConnMu.Lock()
		if trConn == c {
			trConn = nil
		}
		trConnMu.Unlock()
		c.Close()
	}
}

// traceReset: tests start from nothing.
func traceReset() {
	trMu.Lock()
	trTokens, trTokenAt, trDropped = float64(traceBurst), time.Time{}, 0
	trReads, trFlight = map[string]*traceAgg{}, map[int64]traceFlight{}
	trMu.Unlock()
}

// ---- one miner API call at a time (the SC Box II) ----

// apiSlotCh: the miner API's one slot. The mining program answers one API connection at a time anyway; on the Box II
// the panel's own pollers (the fan loop, the Dashboard, a tuner run, a clock confirmation) take turns here instead of
// piling connections onto it. A call that waits longer than the API's own timeout goes ahead regardless (the slot
// never stops a guard), and says so in the trace.
var apiSlotCh = make(chan struct{}, 1)

// apiSlot: take the slot; the returned func gives it back. Not the Box II: a no-op.
func apiSlot() func() {
	if model.Plan != planBox2V {
		return func() {}
	}
	select {
	case apiSlotCh <- struct{}{}:
		return func() { <-apiSlotCh }
	default:
	}
	t := time.NewTimer(*apiTimeout + time.Second)
	defer t.Stop()
	select {
	case apiSlotCh <- struct{}{}:
		return func() { <-apiSlotCh }
	case <-t.C:
		traceNote("miner API: a call waited %s for the one before it: going ahead", *apiTimeout+time.Second)
		return func() {}
	}
}

// traceBodyNote: what a write to the OEM's backend carries, for its trace line (the plan text of a setting).
func traceBodyNote(path string, body []byte) string {
	if !traceOn() || len(body) == 0 || !strings.HasPrefix(path, "/mcb/setting") {
		return ""
	}
	var cur map[string]any
	if json.Unmarshal(body, &cur) != nil {
		return ""
	}
	m, _ := cur["manual"].(bool)
	pp, _ := cur["manualPowerplan"].(string)
	return fmt.Sprintf(" (manual %v: %s)", m, pp)
}
