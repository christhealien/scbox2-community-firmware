package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// l18Srv starts a test server on a fixed address (lane L18's port range 47360-47379).
func l18Srv(t *testing.T, addr string, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Skipf("port busy: %v", err)
	}
	s := httptest.NewUnstartedServer(h)
	s.Listener.Close()
	s.Listener = ln
	s.Start()
	t.Cleanup(s.Close)
	return s
}

type l18Hit struct {
	method, path, gotify, auth string
	body                       string
}

type l18Rec struct {
	mu   sync.Mutex
	hits []l18Hit
}

func (r *l18Rec) h(code int) http.HandlerFunc {
	return func(w http.ResponseWriter, q *http.Request) {
		b, _ := io.ReadAll(q.Body)
		r.mu.Lock()
		r.hits = append(r.hits, l18Hit{q.Method, q.URL.Path, q.Header.Get("X-Gotify-Key"), q.Header.Get("Authorization"), string(b)})
		r.mu.Unlock()
		w.WriteHeader(code)
	}
}

func (r *l18Rec) all() []l18Hit {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]l18Hit(nil), r.hits...)
}

// An ntfy server reached on http:// that redirects to https:// (a reverse proxy) turns the POST into a GET:
// the message is dropped, ntfy's web app answers 200 and the alert (and the test button) count as sent.
// Same for a Discord webhook entered with http://.
func TestL18RedirectedPostIsNotDelivered(t *testing.T) {
	setupNotify(t, "", 30)
	target := &l18Rec{}
	l18Srv(t, "127.0.0.1:47361", target.h(200)) // the redirect's target: answers a GET with 200, like ntfy's web app
	l18Srv(t, "127.0.0.1:47360", func(w http.ResponseWriter, q *http.Request) {
		http.Redirect(w, q, "http://127.0.0.1:47361"+q.URL.Path, http.StatusMovedPermanently)
	})
	c := defaultNotifyConfig()
	c.NtfyOn, c.NtfyURL, c.NtfyTopic = true, "http://127.0.0.1:47360", "miners"
	if err := sendNtfy(c, evHot, "too hot: 88°C"); err == nil {
		t.Errorf("ntfy: the server redirected the POST and nothing was published, yet the send counted as done (target saw %+v)", target.all())
	}
	if err := sendDiscord("http://127.0.0.1:47360/api/webhooks/1/abc", "x"); err == nil {
		t.Errorf("discord: a redirected webhook POST counted as sent (target saw %+v)", target.all())
	}
}

// A Gotify server (or anything between) that answers 307 to another host gets the app token sent there:
// Go strips Authorization on a cross-host redirect, not X-Gotify-Key.
func TestL18GotifyKeyNotForwardedOnRedirect(t *testing.T) {
	setupNotify(t, "", 30)
	other := &l18Rec{}
	l18Srv(t, "127.0.0.1:47363", other.h(200))
	l18Srv(t, "127.0.0.1:47362", func(w http.ResponseWriter, q *http.Request) {
		http.Redirect(w, q, "http://localhost:47363/collect", http.StatusTemporaryRedirect)
	})
	c := defaultNotifyConfig()
	c.GotifyOn, c.GotifyURL, c.GotifyToken = true, "http://127.0.0.1:47362", "AppTokSecret"
	sendGotify(c, evHot, "x")
	for _, h := range other.all() {
		if h.gotify != "" {
			t.Fatalf("the Gotify app token went to the redirect's host: %+v", h)
		}
	}
}

type l18Discord struct {
	mu   sync.Mutex
	msgs []string
}

func (f *l18Discord) h(w http.ResponseWriter, q *http.Request) {
	var m struct{ Content string }
	json.NewDecoder(q.Body).Decode(&m)
	f.mu.Lock()
	f.msgs = append(f.msgs, m.Content)
	f.mu.Unlock()
	w.WriteHeader(204)
}

func (f *l18Discord) wait(pred func([]string) bool) []string {
	for i := 0; i < 100; i++ {
		f.mu.Lock()
		m := append([]string(nil), f.msgs...)
		f.mu.Unlock()
		if pred(m) {
			return m
		}
		time.Sleep(20 * time.Millisecond)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.msgs...)
}

func l18Has(m []string, sub string) bool {
	for _, s := range m {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// The pool guard removes a foreign pool (notified), it comes back, and a couple of minutes later the guard
// sets the miner to Idle and holds it there. That message is the one the owner must act on, and it was
// held back by the 30-minute repeat gap of the "removed a pool" message sent a minute before.
func TestL18PoolGuardIdleHoldIsNotified(t *testing.T) {
	fd := &l18Discord{}
	srv := l18Srv(t, "127.0.0.1:47364", fd.h)
	setupNotify(t, srv.URL, 30)
	dir := filepath.Dir(*authFile)
	oldB := *bfgPath
	*bfgPath = filepath.Join(dir, "bfgminer", "bfgminer.json")
	os.MkdirAll(dir, 0o755)
	oldL, oldP, oldR, oldI, oldIdle := pgListPools, pgLivePools, pgRemove, pgIdle, minerIdleForGuard
	t.Cleanup(func() {
		*bfgPath = oldB
		pgListPools, pgLivePools, pgRemove, pgIdle, minerIdleForGuard = oldL, oldP, oldR, oldI, oldIdle
		os.Remove(poolHoldPath())
		setCharge("")
	})
	noteMinerChange("test")
	pgListPools = func() ([]map[string]any, error) {
		return []map[string]any{{"url": "stratum+tcp://mine.example:3333", "user": "me.w1"}}, nil
	}
	pgLivePools = func() ([]poolView, error) { // the foreign pool keeps coming back after each removal
		return []poolView{{Index: 0, URL: "stratum+tcp://mine.example:3333", User: "me.w1", Active: true},
			{Index: 1, URL: "stratum+tcp://evil.example:3333", User: "thief.x"}}, nil
	}
	pgRemove = func(int) error { return nil }
	pgIdle = func() error { return nil }
	minerIdleForGuard = func() bool { return false }
	pgStrikes, pgActed, pgIncidents = map[string]int{}, map[string]int{}, nil
	for i := 0; i < 6; i++ {
		poolGuardCheck()
	}
	m := fd.wait(func(m []string) bool { return l18Has(m, "set to Idle") })
	if !l18Has(m, "removed a pool") {
		t.Fatalf("no removal message: %q", m)
	}
	if !l18Has(m, "set to Idle") {
		t.Fatalf("the miner was set to Idle and held by the pool guard, and the owner wasn't told: %q", m)
	}
}

// A fan alert from the fan watcher ("fan 2 stopped") and a minute later the fan lock stops and locks mining:
// the lock's message (the one that says mining is stopped until you unlock it) fell under the fan alerts'
// 6-hour repeat gap and was never sent.
func TestL18FanLockAfterFanAlertIsNotified(t *testing.T) {
	fd := &l18Discord{}
	srv := l18Srv(t, "127.0.0.1:47365", fd.h)
	setupNotify(t, srv.URL, 30)
	notifyForce(evFan, "fan 2 stopped (0 rpm). Check it isn't blocked or unplugged")
	fanLockNotify("mining stopped and locked: fan 2 (the OEM's watchdog keeps stopping mining while a fan looks failed)")
	m := fd.wait(func(m []string) bool { return len(m) >= 2 })
	if !l18Has(m, "mining stopped and locked") {
		t.Fatalf("the fan lock stopped mining and the owner wasn't told: %q", m)
	}
}

// A stored token or webhook can't be removed: a blank field means "keep". An ntfy server that no longer
// needs a login still gets the old token, and ntfy refuses a request with an unknown token (401).
func TestL18StoredSecretCanBeRemoved(t *testing.T) {
	setupNotify(t, "https://discord.example/api/webhooks/1/x", 30)
	notifyMu.Lock()
	notifyCfg.NtfyToken, notifyCfg.GotifyToken, notifyCfg.WebhookURL, notifyCfg.TelegramToken = "tk_old", "g_old", "http://h/hook", "123:abc"
	notifyMu.Unlock()
	b, _ := json.Marshal(map[string]any{"ntfy_on": true, "ntfy_url": "http://192.168.1.5", "ntfy_topic": "m", "ntfy_token": "-",
		"discord_url": "-", "webhook_url": "-", "telegram_token": "-", "gotify_token": "-"})
	w := httptest.NewRecorder()
	handleSettings(w, httptest.NewRequest("POST", "/api/settings", bytes.NewReader(b)))
	if !strings.Contains(w.Body.String(), `"ok":true`) {
		t.Fatalf("save: %s", w.Body.String())
	}
	notifyMu.Lock()
	c := notifyCfg
	notifyMu.Unlock()
	if c.NtfyToken != "" || c.DiscordURL != "" || c.WebhookURL != "" || c.TelegramToken != "" || c.GotifyToken != "" {
		t.Fatalf("a removed secret is still stored: %+v", c)
	}
}
