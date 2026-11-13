package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeDiscord records the messages a Discord webhook would receive.
type fakeDiscord struct {
	mu   sync.Mutex
	msgs []string
}

func (f *fakeDiscord) handler(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	var m struct{ Content string }
	json.Unmarshal(b, &m)
	f.mu.Lock()
	f.msgs = append(f.msgs, m.Content)
	f.mu.Unlock()
	w.WriteHeader(204)
}

func (f *fakeDiscord) waitN(t *testing.T, n int) []string {
	t.Helper()
	for i := 0; i < 100; i++ {
		f.mu.Lock()
		if len(f.msgs) >= n {
			out := append([]string(nil), f.msgs...)
			f.mu.Unlock()
			return out
		}
		f.mu.Unlock()
		time.Sleep(20 * time.Millisecond)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	t.Fatalf("expected %d messages, got %d: %q", n, len(f.msgs), f.msgs)
	return nil
}

func (f *fakeDiscord) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.msgs) }

func setupNotify(t *testing.T, url string, interval int) {
	t.Helper()
	*authFile = filepath.Join(t.TempDir(), "auth.json")
	bestBookMu.Lock()
	sharesRead, shares = false, shareBook{}
	bestBookMu.Unlock()
	c := defaultNotifyConfig()
	c.DiscordOn, c.DiscordURL, c.MinIntervalMin = true, url, interval
	notifyMu.Lock()
	notifyCfg, notifyLast, notifyHistory = c, map[string]time.Time{}, nil
	notifyMu.Unlock()
	bestNotified = 0
}

func TestBestShareAlerts(t *testing.T) {
	fd := &fakeDiscord{}
	srv := httptest.NewServer(http.HandlerFunc(fd.handler))
	defer srv.Close()
	setupNotify(t, srv.URL, 30)

	// first 10 minutes: records climb fast, nothing is sent
	maybeNotifyBest(dashboard{Elapsed: 60, BestShare: 1000})
	maybeNotifyBest(dashboard{Elapsed: 300, BestShare: 5000})
	time.Sleep(100 * time.Millisecond)
	if fd.count() != 0 {
		t.Fatal("sent during the warm-up")
	}
	// warm-up over: the record reached during it is sent, even though it didn't move just now
	maybeNotifyBest(dashboard{Elapsed: 601, BestShare: 5000})
	msgs := fd.waitN(t, 1)
	if !strings.Contains(msgs[0], "new best share") {
		t.Fatalf("message: %q", msgs[0])
	}
	// the same record isn't sent twice
	maybeNotifyBest(dashboard{Elapsed: 700, BestShare: 5000})
	// a record found seconds after the last alert waits for the 2-minute gap ...
	maybeNotifyBest(dashboard{Elapsed: 610, BestShare: 9000})
	time.Sleep(100 * time.Millisecond)
	if fd.count() != 1 {
		t.Fatalf("sent inside the 2-minute gap: %d", fd.count())
	}
	// ... and goes out (with the latest value) once it's over, even though the Settings gap for
	// alerts is 30 minutes (that's for problem alerts, not records)
	notifyMu.Lock()
	notifyLast[evBestShare] = time.Now().Add(-bestShareGap - time.Second)
	notifyMu.Unlock()
	maybeNotifyBest(dashboard{Elapsed: 2500, BestShare: 12000})
	msgs = fd.waitN(t, 2)
	if !strings.Contains(msgs[1], "(was ") {
		t.Fatalf("second message: %q", msgs[1])
	}

	// switched off in Settings: nothing
	notifyMu.Lock()
	notifyCfg.Events[evBestShare] = false
	notifyLast = map[string]time.Time{}
	notifyMu.Unlock()
	maybeNotifyBest(dashboard{Elapsed: 5000, BestShare: 99000})
	time.Sleep(100 * time.Millisecond)
	if fd.count() != 2 {
		t.Fatal("sent while switched off")
	}
}

func TestNotifyFailureIsReported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) }))
	defer srv.Close()
	setupNotify(t, srv.URL+"/api/webhooks/123/SECRET-TOKEN", 0)
	sendAll(notifyCfg, evHot, "test")
	notifyMu.Lock()
	h := append([]notifyRecord(nil), notifyHistory...)
	notifyMu.Unlock()
	if len(h) != 1 || !strings.Contains(h[0].Result, "Discord ✗") || !strings.Contains(h[0].Result, "404") {
		t.Fatalf("history: %+v", h)
	}
	if strings.Contains(h[0].Result, "SECRET-TOKEN") {
		t.Fatal("the webhook token leaked into the result")
	}
	// a connection error keeps the URL out of the message too
	if e := shortErr(errors.New(`Post "https://discord.com/api/webhooks/1/SECRET": dial tcp: i/o timeout`)); strings.Contains(e, "SECRET") {
		t.Fatalf("shortErr leaked the URL: %s", e)
	}
}

func TestHashLowRule(t *testing.T) {
	run := 0
	// just restarted: the average since restart is still climbing, never an alert
	for i := 0; i < 10; i++ {
		if hashLowDue(120, "", 200, 1150, 70, &run) {
			t.Fatal("alert during warm-up")
		}
	}
	// tuner running: no alert
	for i := 0; i < 10; i++ {
		if hashLowDue(5000, "tuner", 200, 1150, 70, &run) {
			t.Fatal("alert while tuning")
		}
	}
	// one dip, then back to normal: no alert
	if hashLowDue(5000, "", 600, 1150, 70, &run) || hashLowDue(5000, "", 1140, 1150, 70, &run) {
		t.Fatal("alert on one dip")
	}
	// low three checks in a row: alert
	got := []bool{hashLowDue(5000, "", 600, 1150, 70, &run), hashLowDue(5000, "", 600, 1150, 70, &run), hashLowDue(5000, "", 600, 1150, 70, &run)}
	if got[0] || got[1] || !got[2] {
		t.Fatalf("sustained low: %v", got)
	}
}

func TestSystemUptime(t *testing.T) {
	p := filepath.Join(t.TempDir(), "uptime")
	old := uptimePath
	defer func() { uptimePath = old }()
	uptimePath = p
	writeFile(t, p, "123.45 400.00\n")
	if v := systemUptime(); v < 123 || v > 124 {
		t.Fatalf("uptime %v", v)
	}
	uptimePath = filepath.Join(t.TempDir(), "missing")
	if systemUptime() < bootRecentS {
		t.Fatal("an unreadable uptime must not look like a fresh boot")
	}
}

func writeFile(t *testing.T, p, s string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}
