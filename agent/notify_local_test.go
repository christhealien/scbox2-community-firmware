package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type capSrv struct {
	mu   sync.Mutex
	reqs []*http.Request
	body []map[string]any
}

func (c *capSrv) handler(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	var m map[string]any
	json.Unmarshal(b, &m)
	c.mu.Lock()
	c.reqs = append(c.reqs, r)
	c.body = append(c.body, m)
	c.mu.Unlock()
}

func (c *capSrv) n() int { c.mu.Lock(); defer c.mu.Unlock(); return len(c.body) }

// ntfy, Gotify and a webhook on the LAN get the alert, with the emoji, the miner and a priority.
func TestLocalChannels(t *testing.T) {
	nt, gt, wh := &capSrv{}, &capSrv{}, &capSrv{}
	s1, s2, s3 := httptest.NewServer(http.HandlerFunc(nt.handler)), httptest.NewServer(http.HandlerFunc(gt.handler)), httptest.NewServer(http.HandlerFunc(wh.handler))
	defer s1.Close()
	defer s2.Close()
	defer s3.Close()
	setupNotify(t, "", 0)
	notifyMu.Lock()
	notifyCfg.DiscordOn = false
	notifyCfg.NtfyOn, notifyCfg.NtfyURL, notifyCfg.NtfyTopic, notifyCfg.NtfyToken = true, s1.URL+"/", "miners", "tk_1"
	notifyCfg.GotifyOn, notifyCfg.GotifyURL, notifyCfg.GotifyToken = true, s2.URL, "AppTok"
	notifyCfg.WebhookOn, notifyCfg.WebhookURL = true, s3.URL+"/hook"
	notifyCfg.Events[evHot] = true
	notifyMu.Unlock()
	if !notifyCfg.anyOn() {
		t.Fatal("anyOn")
	}
	if !notify(evHot, "board 2 at 91 °C") {
		t.Fatal("not sent")
	}
	for i := 0; i < 50 && (nt.n() == 0 || gt.n() == 0 || wh.n() == 0); i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if nt.n() != 1 || gt.n() != 1 || wh.n() != 1 {
		t.Fatalf("ntfy %d gotify %d webhook %d", nt.n(), gt.n(), wh.n())
	}
	em := eventEmoji[evHot]
	if b := nt.body[0]; b["topic"] != "miners" || b["message"] != em+" board 2 at 91 °C" || b["priority"].(float64) != 5 || b["title"] != minerLabel() {
		t.Fatalf("ntfy: %v", b)
	}
	if nt.reqs[0].Header.Get("Authorization") != "Bearer tk_1" || nt.reqs[0].URL.Path != "/" {
		t.Fatalf("ntfy request: %v %v", nt.reqs[0].URL, nt.reqs[0].Header)
	}
	if gt.reqs[0].URL.Path != "/message" || gt.reqs[0].Header.Get("X-Gotify-Key") != "AppTok" || gt.body[0]["priority"].(float64) != 10 {
		t.Fatalf("gotify: %v %v", gt.reqs[0].URL, gt.body[0])
	}
	if b := wh.body[0]; b["event"] != evHot || b["emoji"] != em || b["message"] != "board 2 at 91 °C" || !strings.Contains(b["text"].(string), em) {
		t.Fatalf("webhook: %v", b)
	}
	time.Sleep(50 * time.Millisecond)
	notifyMu.Lock()
	h := notifyHistory
	notifyMu.Unlock()
	if len(h) != 1 || !strings.Contains(h[0].Result, "ntfy ✓") || !strings.Contains(h[0].Result, "Gotify ✓") || !strings.Contains(h[0].Result, "webhook ✓") {
		t.Fatalf("history: %+v", h)
	}
}

func TestNotifyPriority(t *testing.T) {
	for _, c := range []struct {
		ev, msg string
		p       int
	}{
		{evHot, "board 1 at 90 °C", 5},
		{evBoard, "board 3 isn't hashing", 4},
		{evBoard, "board 3 is hashing again", 3},
		{evDaily, "24 h: 1.2 TH/s", 2},
		{evChipWeak, "chip 7 turned weak", 3},
	} {
		if p := notifyPriority(c.ev, c.msg); p != c.p {
			t.Errorf("%s %q: %d, want %d", c.ev, c.msg, p, c.p)
		}
	}
}

// saving with blank secrets keeps them; a bad address is refused
func TestLocalSettingsSave(t *testing.T) {
	setupNotify(t, "", 0)
	notifyMu.Lock()
	notifyCfg.NtfyToken, notifyCfg.GotifyToken, notifyCfg.WebhookURL = "keepN", "keepG", "http://h/keep"
	notifyMu.Unlock()
	post := func(m map[string]any) map[string]any {
		b, _ := json.Marshal(m)
		w := httptest.NewRecorder()
		handleSettings(w, httptest.NewRequest("POST", "/api/settings", bytes.NewReader(b)))
		var out map[string]any
		json.Unmarshal(w.Body.Bytes(), &out)
		return out
	}
	if o := post(map[string]any{"ntfy_on": true, "ntfy_url": "192.168.1.5", "ntfy_topic": "m"}); o["ok"] != false {
		t.Fatalf("an address without http:// was accepted: %v", o)
	}
	if o := post(map[string]any{"ntfy_on": true, "ntfy_url": "http://192.168.1.5", "ntfy_topic": "m", "gotify_on": true, "gotify_url": "http://g", "webhook_on": true}); o["ok"] != true {
		t.Fatalf("save: %v", o)
	}
	notifyMu.Lock()
	c := notifyCfg
	notifyMu.Unlock()
	if !c.NtfyOn || c.NtfyURL != "http://192.168.1.5" || c.NtfyToken != "keepN" || c.GotifyToken != "keepG" || c.WebhookURL != "http://h/keep" || !c.WebhookOn {
		t.Fatalf("saved: %+v", c)
	}
	w := httptest.NewRecorder()
	handleSettings(w, httptest.NewRequest("GET", "/api/settings", nil))
	if strings.Contains(w.Body.String(), "keepN") || strings.Contains(w.Body.String(), "keepG") {
		t.Fatal("a token was shown in the settings")
	}
}
