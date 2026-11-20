package main

// v0.5.4: the freeze-hunt breadcrumbs (trace.go), the slower clock confirmation polling, one miner API call at a
// time on the SC Box II, the tuner's fan-write spacing and shared temperature read, and Settings → Recovery's
// "send the system log to this computer".

import (
	"bufio"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type traceCatch struct {
	mu    sync.Mutex
	lines []string
}

func (c *traceCatch) all() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Join(c.lines, "\n")
}

func (c *traceCatch) n() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.lines)
}

// catchTrace: tracing on (force), every line into the returned catcher.
func catchTrace(t *testing.T, force bool) *traceCatch {
	t.Helper()
	c := &traceCatch{}
	traceReset()
	traceSink = func(l string) { c.mu.Lock(); c.lines = append(c.lines, l); c.mu.Unlock() }
	traceForce.Store(force)
	t.Cleanup(func() {
		traceSink = nil
		traceForce.Store(false)
		traceHeld.Store(false)
		traceUntil.Store(0)
		traceReset()
	})
	return c
}

func TestTraceOffWritesNothing(t *testing.T) {
	c := catchTrace(t, false)
	traceHeld.Store(false)
	traceUntil.Store(0)
	traceWrite("backend PUT /mcb/setting")(nil)
	traceRead("api devs")(nil)
	traceNote("tuner: x")
	if c.n() != 0 || traceSummary(time.Now()) != "" {
		t.Fatalf("tracing off wrote %q / summary %q", c.all(), traceSummary(time.Now()))
	}
}

func TestTraceWriteBeginEndAndReadSummary(t *testing.T) {
	c := catchTrace(t, true)
	end := traceWrite("backend PUT /mcb/setting (manual true: 690 MHz 42 V 60 RPM 41 RPM)")
	end(nil)
	traceWrite("api pgaset 0,clock,650")(os.ErrDeadlineExceeded)
	for i := 0; i < 7; i++ {
		traceRead("api devs")(nil)
	}
	traceRead("api summary")(os.ErrClosed)
	all := c.all()
	for _, want := range []string{"▶ backend PUT /mcb/setting (manual true: 690 MHz 42 V 60 RPM 41 RPM)",
		"◀ backend PUT /mcb/setting (manual true: 690 MHz 42 V 60 RPM 41 RPM) ok", "◀ api pgaset 0,clock,650 ERR"} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in\n%s", want, all)
		}
	}
	if strings.Contains(all, "api devs") {
		t.Errorf("a read got a line of its own (reads are only summarised):\n%s", all)
	}
	s := traceSummary(time.Now())
	if !strings.Contains(s, "api devs×7") || !strings.Contains(s, "api summary×1") || !strings.Contains(s, "(1 failed)") {
		t.Errorf("summary %q", s)
	}
	if s2 := traceSummary(time.Now()); s2 != "" {
		t.Errorf("a second summary with nothing new: %q", s2)
	}
}

func TestTraceRateLimited(t *testing.T) {
	c := catchTrace(t, true)
	for i := 0; i < 500; i++ {
		traceNote("line %d", i)
	}
	if n := c.n(); n > traceBurst+5 {
		t.Fatalf("%d lines written at once (burst %d)", n, traceBurst)
	}
	if s := traceSummary(time.Now()); !strings.Contains(s, "dropped (rate limit)") {
		t.Fatalf("the drops aren't reported: %q", s)
	}
}

func TestTraceInFlightRead(t *testing.T) {
	catchTrace(t, true)
	traceFlightAdd("api devs", time.Now().Add(-5*time.Second))
	traceFlightAdd("api pools", time.Now())
	s := traceSummary(time.Now())
	if !strings.Contains(s, "IN FLIGHT: api devs 5s") || strings.Contains(s, "api pools") {
		t.Fatalf("summary %q", s)
	}
}

func TestTraceFollowsTheRun(t *testing.T) {
	c := catchTrace(t, false)
	setCharge("tuner")
	if !traceOn() {
		t.Fatal("a tuner run doesn't turn tracing on")
	}
	setCharge("")
	if !traceOn() {
		t.Fatal("tracing stopped at once when the run ended (it lingers for the restore)")
	}
	if !strings.Contains(c.all(), "trace on: the tuner holds the miner") || !strings.Contains(c.all(), "trace off in") {
		t.Fatalf("lines: %s", c.all())
	}
	traceUntil.Store(time.Now().Add(-time.Second).UnixNano())
	if traceOn() {
		t.Fatal("tracing still on after the linger")
	}
	setCharge("ceiling")
	if !traceOn() {
		t.Fatal("the probe doesn't turn tracing on")
	}
	safetyStopIdle()
	if traceHeld.Load() {
		t.Fatal("a safety stop's Idle didn't end the run's trace")
	}
	setCharge("")
}

// The plan writer's PUT and the clock confirmation leave their lines (start, end, the confirmation's result).
func TestTracePlanWriteAndConfirm(t *testing.T) {
	f := newC34(t, true, false)
	c := catchTrace(t, true)
	if _, err := box2ApplyControl(noGen, powerplan{FreqMHz: 650}); err != nil {
		t.Fatal(err)
	}
	all := c.all()
	for _, want := range []string{"▶ backend PUT /mcb/setting (manual true: 650 MHz", "◀ backend PUT /mcb/setting", "confirm #", "confirmed after"} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in\n%s", want, all)
		}
	}
	_ = f
}

func TestBox2ConfirmPollsAtMostEvery2s(t *testing.T) {
	if box2ConfirmPoll < 2*time.Second || box2ConfirmPollSlow < 5*time.Second {
		t.Fatalf("confirm polls every %s / %s: at least 2 s / 5 s", box2ConfirmPoll, box2ConfirmPollSlow)
	}
	if d := box2ConfirmPollAt(0); d != box2ConfirmPoll {
		t.Errorf("first polls every %s", d)
	}
	if d := box2ConfirmPollAt(box2ConfirmFastFor + time.Second); d != box2ConfirmPollSlow {
		t.Errorf("a long ramp polls every %s", d)
	}
}

// A confirmation that never matches (the driver ignores the write) polls slower after the first stretch: the API
// is asked a bounded number of times, then the stock plan is put back.
func TestBox2ConfirmBacksOff(t *testing.T) {
	f := newC34(t, true, false)
	var polls atomic.Int64
	devs := box2DevsCmd
	box2DevsCmd = func() ([]byte, error) { polls.Add(1); return devs() }
	oldSlow, oldFast := box2ConfirmPollSlow, box2ConfirmFastFor
	box2ConfirmWait, box2ConfirmPoll, box2ConfirmPollSlow, box2ConfirmFastFor = 600*time.Millisecond, 20*time.Millisecond, 100*time.Millisecond, 100*time.Millisecond
	t.Cleanup(func() { box2ConfirmPollSlow, box2ConfirmFastFor = oldSlow, oldFast })
	f.mu.Lock()
	f.ignore = true
	f.mu.Unlock()
	if _, err := box2ApplyControl(noGen, powerplan{FreqMHz: 650}); err == nil {
		t.Fatal("an unconfirmed clock wasn't reported")
	}
	// the wait for 690 → 650 is 600 ms + 4 × 200 ms = 1.4 s: 5 fast polls, then about 13 slow ones (every 20 ms
	// throughout would have been 70)
	if n := polls.Load(); n > 25 {
		t.Fatalf("%d devs polls in one confirmation", n)
	}
}

// One miner API call at a time on the SC Box II.
func TestAPISlotOneAtATime(t *testing.T) {
	useBox2(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var cur, peak atomic.Int64
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				n := cur.Add(1)
				for {
					p := peak.Load()
					if n <= p || peak.CompareAndSwap(p, n) {
						break
					}
				}
				bufio.NewReader(c).ReadByte()
				time.Sleep(30 * time.Millisecond)
				cur.Add(-1)
				c.Write([]byte(`{"STATUS":[{"STATUS":"S"}]}` + "\x00"))
			}(c)
		}
	}()
	old := *apiAddr
	*apiAddr = ln.Addr().String()
	defer func() { *apiAddr = old }()
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				cgminerCmd("summary")
			} else {
				cgminerCmdParam("pgaset", "0,clock,690")
			}
		}(i)
	}
	wg.Wait()
	if p := peak.Load(); p != 1 {
		t.Fatalf("%d API connections at once", p)
	}
}

func TestTunerFanWriteSpacing(t *testing.T) {
	useBox2(t)
	for _, c := range []struct {
		want, cur   int
		temp, tgt   float64
		since       time.Duration
		wait        bool
		description string
	}{
		{40, 42, 70, 77, 3 * time.Second, true, "a step down 3 s after the last write waits"},
		{44, 42, 70, 77, 3 * time.Second, true, "a raise under the target waits too"},
		{46, 42, 78, 77, 3 * time.Second, false, "a raise at or over the target never waits"},
		{40, 42, 70, 77, 11 * time.Second, false, "after the gap it goes"},
		{42, 42, 70, 77, time.Second, false, "no change: nothing to hold"},
	} {
		if got := tunerFanWriteWait(c.want, c.cur, c.temp, c.tgt, c.since); got != c.wait {
			t.Errorf("%s: got %v", c.description, got)
		}
	}
	model, modelWhy = profSCLite, "test"
	setWindowsFromModel()
	if tunerFanWriteWait(40, 42, 70, 77, time.Second) {
		t.Error("the SC Lite's fan hold is held back")
	}
}

func TestMemoTempSharesOneRead(t *testing.T) {
	n := 0
	r := memoTemp(time.Hour, func() (float64, bool) { n++; return 70, true })
	r()
	r()
	if v, ok := r(); n != 1 || v != 70 || !ok {
		t.Fatalf("reads %d, %v %v", n, v, ok)
	}
	m := 0
	r2 := memoTemp(0, func() (float64, bool) { m++; return 70, true })
	r2()
	r2()
	if m != 2 {
		t.Fatalf("a zero memo read %d times", m)
	}
}

func TestSyslogRemoteNorm(t *testing.T) {
	for in, want := range map[string]string{"": "", " 192.168.1.20:514 ": "192.168.1.20:514", "192.168.1.20": "192.168.1.20:514"} {
		if got, err := syslogRemoteNorm(in); err != nil || got != want {
			t.Errorf("%q: %q %v", in, got, err)
		}
	}
	for _, in := range []string{"1.2.3.4:0", "1.2.3.4:70000", "1.2.3.4:x", "a b:514", "host;reboot:514", "[::1]:514", "::1", ":514", "-x:514", "h:0514", "$(id):514", "logs.lan:1514", "pc-1:514"} {
		if got, err := syslogRemoteNorm(in); err == nil {
			t.Errorf("%q accepted as %q", in, got)
		}
	}
}

func TestSyslogRemoteSetting(t *testing.T) {
	useBox2(t)
	*authFile = filepath.Join(t.TempDir(), "auth.json")
	applied := 0
	old := syslogRemoteApply
	syslogRemoteApply = func() error { applied++; return nil }
	defer func() { syslogRemoteApply = old }()
	post := func(body string) map[string]any {
		w := httptest.NewRecorder()
		handleRecovery(w, httptest.NewRequest(http.MethodPost, "/api/recovery", strings.NewReader(body)))
		var v map[string]any
		json.Unmarshal(w.Body.Bytes(), &v)
		return v
	}
	v := post(`{"syslog_remote":"192.168.1.20:514"}`)
	if v["ok"] != true || v["syslog_remote"] != "192.168.1.20:514" || v["syslog_remote_avail"] != true || applied != 1 {
		t.Fatalf("set: %v (applied %d)", v, applied)
	}
	if b, _ := os.ReadFile(syslogRemotePath()); string(b) != "192.168.1.20:514\n" {
		t.Fatalf("file %q", b)
	}
	if v := post(`{"syslog_remote":"192.168.1.20:99999"}`); v["ok"] != false || applied != 1 {
		t.Fatalf("a bad port: %v", v)
	}
	if v := post(`{"syslog_remote":""}`); v["ok"] != true || v["syslog_remote"] != "" || applied != 2 {
		t.Fatalf("off: %v", v)
	}
	if _, err := os.Stat(syslogRemotePath()); !os.IsNotExist(err) {
		t.Fatalf("the file stays after turning it off: %v", err)
	}
	model, modelWhy = profSCLite, "test"
	setWindowsFromModel()
	if v := post(`{"syslog_remote":"192.168.1.20:514"}`); v["ok"] != false {
		t.Fatalf("offered on the SC Lite: %v", v)
	}
}

// Every SC Box II build offers the relay (Settings → Troubleshooting), the public one included.
func TestSyslogRemoteEveryBuild(t *testing.T) {
	useBox2(t)
	if !syslogRemoteAvail() {
		t.Fatalf("the system-log relay isn't offered (public build: %v)", publicBuild)
	}
	if v := recoveryView(); v["syslog_remote_avail"] != true {
		t.Fatalf("the Recovery answer doesn't offer it: %v", v)
	}
}

// The page offers the relay only when the panel says it's available, in Troubleshooting (not in Recovery).
func TestSyslogRemoteUI(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Skip(err)
	}
	s := string(src)
	if !strings.Contains(s, "if(!v||!v.syslog_remote_avail){p.classList.add('hide');") || !strings.Contains(s, "recSet({syslog_remote:") {
		t.Fatal("Settings → Troubleshooting has no relay field")
	}
	if rb := l34Func(t, "renderRecoveryBox2"); strings.Contains(rb, "recsyslog") || strings.Contains(rb, "(below)") {
		t.Fatal("the relay field (or a pointer to it below) is still in Recovery")
	}
}
