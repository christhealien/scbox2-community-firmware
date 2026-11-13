package main

// Notifications (Telegram + Discord) and the Settings backend.
//
// Mirrors blake2b-asic-control's notify card: pick Telegram and/or Discord, choose which
// events to send, rate-limit repeats. Outbound HTTPS goes straight to api.telegram.org and
// the Discord webhook host, so it only works where the miner has internet (not from a
// restricted build sandbox). Sends are best-effort and never block the mining path.

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// event keys
const (
	evStopped   = "stopped"       // miner stopped answering / came back
	evHot       = "hot"           // too hot
	evHashLow   = "hashrate_low"  // hashrate low
	evRestarted = "restarted"     // restarted on its own
	evTuning    = "tuning_done"   // tuning: a run started / finished / was stopped / aborted / cut short (key kept for saved settings)
	evBestShare = "best_share"    // new best-share record
	evBack      = "back"          // the miner answers again after a "stopped" alert (follows evStopped's switch)
	evChipWeak  = "chip_weak"     // a chip turned weak
	evRejected  = "rejected"      // a share was rejected
	evAccepted  = "accepted"      // a share was accepted (off by default — chatty)
	evPoolGuard = "pool_guard"    // a pool that isn't in the Pools tab appeared and was removed
	evChipTrim  = "chip_autotrim" // chip auto-trim slowed a chip, or undid a step that didn't help
	evBoard     = "board_down"    // a hash board that hashes on this miner isn't hashing (and when it is again)
	evFan       = "fan"           // a fan stopped or is far slower than the others
	evPoolSw    = "pool_switch"   // the miner moved to a backup pool (and back to the primary)
	evNoPool    = "no_pool"       // no pool reachable (and when one is again)
	evThermal   = "thermal"       // the thermal guard forced the fans to 100 % or throttled the clock
	evUpdate    = "fw_update"     // a firmware update installed, rolled back or failed
	evNewFW     = "fw_new"        // a newer firmware release is out (a daily GitHub check)
	evLogin     = "login_block"   // an address was blocked after too many wrong passwords
	evDaily     = "daily"         // a summary once a day
	evSchedule  = "schedule"      // the schedule switched preset
)

// eventEmoji prefixes each notification (matches the blake2b-asic-control app's style).
var eventEmoji = map[string]string{
	evStopped:   "🛑",
	evBack:      "🟢",
	evHot:       "🔥",
	evHashLow:   "📉",
	evRestarted: "♻️",
	evTuning:    "🔧",
	evBestShare: "⭐",
	evChipWeak:  "🟡",
	evRejected:  "⚠️",
	evAccepted:  "✅",
	evPoolGuard: "🛡️",
	evChipTrim:  "✂️",
	evBoard:     "🧩",
	evFan:       "🌀",
	evPoolSw:    "🔀",
	evNoPool:    "🔌",
	evThermal:   "🧯",
	evUpdate:    "⬆️",
	evNewFW:     "🆕",
	evLogin:     "🔐",
	evDaily:     "📊",
	evSchedule:  "🗓️",
}

// notifyGap: how long a repeat of an event is held back, where the default (Settings, 30 min) doesn't fit.
// 0: the event's own watcher decides (it sends once per incident, and the recovery).
var notifyGap = map[string]time.Duration{
	evFan: 6 * time.Hour, evBoard: 0, evPoolSw: 0, evNoPool: 0, evUpdate: 0, evNewFW: 0, evDaily: 0, evSchedule: 0, evTuning: 0,
}

// composeNotify builds the final line: "<miner>: <emoji> <message>", e.g.
// "SCLite_fwtest1: 🟡 chip health: b2c14 turned weak (see the Chips page)".
func composeNotify(event, msg string) string {
	em := eventEmoji[event]
	if em != "" {
		em += " "
	}
	return minerLabel() + ": " + em + msg
}

// eventLabels drives the UI checklist (order preserved).
var eventOrder = []string{evStopped, evHot, evHashLow, evBoard, evFan, evThermal, evRestarted, evNoPool, evPoolSw, evPoolGuard,
	evChipWeak, evChipTrim, evTuning, evBestShare, evUpdate, evNewFW, evLogin, evSchedule, evDaily, evRejected, evAccepted}
var eventLabels = map[string]string{
	evStopped:   "Miner stops answering (and when it's back)",
	evHot:       "Too hot",
	evHashLow:   "Hashrate low",
	evRestarted: "Restarted on its own",
	evTuning:    "Tuning or strong-chip test: started, finished, stopped or aborted",
	evBestShare: "New best-share record",
	evChipWeak:  "A chip turned weak",
	evRejected:  "A share was rejected",
	evAccepted:  "A share was accepted (chatty)",
	evPoolGuard: "A pool you didn't add appeared (it's removed)",
	evChipTrim:  "Chip auto-trim changed a chip",
	evBoard:     "A hash board isn't hashing (and when it is again)",
	evFan:       "A fan stopped or is too slow",
	evPoolSw:    "Switched to a backup pool (and back)",
	evNoPool:    "No pool reachable (and when one is again)",
	evThermal:   "Thermal guard stepped in (fans 100 % or clock throttled)",
	evUpdate:    "Firmware update installed, rolled back or failed",
	evNewFW:     "New firmware available (checks GitHub once a day)",
	evLogin:     "An address was blocked after too many wrong passwords",
	evDaily:     "Daily summary (09:00)",
	evSchedule:  "The schedule switched preset",
}

type notifyConfig struct {
	TelegramToken  string          `json:"telegram_token"`
	TelegramChat   string          `json:"telegram_chat"`
	TelegramOn     bool            `json:"telegram_on"`
	DiscordURL     string          `json:"discord_url"`
	DiscordOn      bool            `json:"discord_on"`
	Events         map[string]bool `json:"events"`
	HotC           float64         `json:"hot_c"`
	HashLowPct     int             `json:"hash_low_pct"`
	MinIntervalMin int             `json:"min_interval_min"`
	// on your own network (nothing has to reach the internet)
	NtfyOn      bool   `json:"ntfy_on"`
	NtfyURL     string `json:"ntfy_url"`   // the ntfy server, e.g. http://192.168.1.20:8080
	NtfyTopic   string `json:"ntfy_topic"` // the topic the phone app subscribes to
	NtfyToken   string `json:"ntfy_token"` // optional access token (a server with logins)
	GotifyOn    bool   `json:"gotify_on"`
	GotifyURL   string `json:"gotify_url"`   // the Gotify server, e.g. http://192.168.1.20:8070
	GotifyToken string `json:"gotify_token"` // an application token from Gotify
	WebhookOn   bool   `json:"webhook_on"`
	WebhookURL  string `json:"webhook_url"` // any URL that takes a JSON POST (Home Assistant, Node-RED, a script)
}

// anyOn: some channel is switched on.
func (c notifyConfig) anyOn() bool {
	return c.TelegramOn || c.DiscordOn || c.NtfyOn || c.GotifyOn || c.WebhookOn
}

func defaultNotifyConfig() notifyConfig {
	return notifyConfig{
		HotC: 82, HashLowPct: 70, MinIntervalMin: 30,
		Events: map[string]bool{
			evStopped: true, evHot: true, evHashLow: true, evRestarted: true,
			evTuning: true, evBestShare: true, evChipWeak: true, evRejected: false, evAccepted: false, evPoolGuard: true, evChipTrim: true,
			evBoard: true, evFan: true, evPoolSw: true, evNoPool: true, evThermal: true, evUpdate: true, evNewFW: false,
			evLogin: true, evDaily: false, evSchedule: false,
		},
	}
}

// caBundle is a current public CA root bundle (Mozilla/certifi), embedded so TLS works on
// minimal miners that ship no /etc/ssl certs — without it, api.telegram.org and Discord fail
// with "x509: certificate signed by unknown authority".
//
//go:embed ca-bundle.pem
var caBundle []byte

func notifyClient() *http.Client {
	pool, _ := x509.SystemCertPool() // use the miner's certs too, if it has any
	if pool == nil {
		pool = x509.NewCertPool()
	}
	pool.AppendCertsFromPEM(caBundle)
	return &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}},
		// no redirects: a 301/302 turns the POST into a GET (the message is dropped, yet a 200 from the page it
		// lands on counted as sent), and a 307 to another host took the Gotify key (a custom header) along
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// httpStatusErr: a refused send; a redirect says what to change.
func httpStatusErr(what string, code int) error {
	if code >= 300 && code < 400 {
		return fmt.Errorf("%sHTTP %d: the server redirects, enter the address it redirects to", what, code)
	}
	return fmt.Errorf("%sHTTP %d", what, code)
}

var (
	notifyMu   sync.Mutex
	notifyCfg  = defaultNotifyConfig()
	notifyLast = map[string]time.Time{} // event -> last sent (rate limit)
	notifyPath = ""                     // beside auth.json (set from -authfile at start; tests use their own folder)
	notifyHTTP = notifyClient()
)

func loadNotifyConfig() {
	if notifyPath == "" {
		notifyPath = filepath.Join(filepath.Dir(*authFile), "notify.json")
	}
	b, ok := readConfig(notifyPath, json.Valid) // falls back to notify.json.bak after a FAT power cut
	if !ok {
		return
	}
	c := defaultNotifyConfig() // a field missing from the file (an older or hand-edited one) keeps its default
	if json.Unmarshal(b, &c) == nil {
		if c.Events == nil {
			c.Events = map[string]bool{}
		}
		for k, v := range defaultNotifyConfig().Events { // events added since this was saved: their default
			if _, ok := c.Events[k]; !ok {
				c.Events[k] = v
			}
		}
		notifyMu.Lock()
		notifyCfg = c
		notifyMu.Unlock()
	}
}

func saveNotifyConfig(c notifyConfig) error {
	if notifyPath == "" {
		notifyPath = filepath.Join(filepath.Dir(*authFile), "notify.json")
	}
	// notify.json holds the Telegram bot token and Discord webhook URL — both secrets —
	// so keep it owner-only (0600) in an owner-only dir, same as the auth store.
	_ = os.MkdirAll(filepath.Dir(notifyPath), 0700)
	b, _ := json.MarshalIndent(c, "", " ")
	return writeAtomic(notifyPath, b, 0600) // flushed before the rename: a power cut can't leave alerts silently off
}

// notify sends a message for an event if it's enabled and not rate-limited. Fire-and-forget.
// Returns true only if the message was actually dispatched (enabled + a channel on + not
// rate-limited) — callers like the best-share throttle use this to advance their own baseline.
const bestShareGap = 2 * time.Minute

// notifyTestHook: tests capture notifications here instead of sending them.
var notifyTestHook func(event, msg string)

func notify(event, msg string) bool {
	if notifyTestHook != nil {
		notifyTestHook(event, msg)
		return true
	}
	notifyMu.Lock()
	c := notifyCfg
	if c.Events == nil || !c.Events[event] || !c.anyOn() {
		notifyMu.Unlock()
		return false
	}
	// rate-limit repeats of the same event. New best-share records are rare by nature (about one per
	// doubling of uptime) and each one is news, so they get their own short gap instead: just enough to
	// fold records found seconds apart into one message.
	iv := time.Duration(c.MinIntervalMin) * time.Minute
	if event == evBestShare {
		iv = bestShareGap
	}
	if g, ok := notifyGap[event]; ok {
		iv = g
	}
	if iv > 0 {
		if last, ok := notifyLast[event]; ok && time.Since(last) < iv {
			notifyMu.Unlock()
			return false
		}
	}
	notifyLast[event] = time.Now()
	notifyMu.Unlock()
	go sendAll(c, event, msg)
	return true
}

// notifyAlways: notify, past the event's repeat gap (an escalation of the same event: the thermal guard's Idle
// right after its "fans forced 100 %").
func notifyAlways(event, msg string) bool {
	notifyMu.Lock()
	delete(notifyLast, event)
	notifyMu.Unlock()
	return notify(event, msg)
}

type notifyRecord struct {
	T      string `json:"t"`
	Text   string `json:"text"`
	Result string `json:"result"`
}

var notifyHistory []notifyRecord // guarded by notifyMu; last 8

func addNotifyRecord(text, result string) {
	notifyMu.Lock()
	notifyHistory = append(notifyHistory, notifyRecord{time.Now().Format("15:04:05"), text, result})
	if len(notifyHistory) > 8 {
		notifyHistory = notifyHistory[len(notifyHistory)-8:]
	}
	notifyMu.Unlock()
}

func sendAll(c notifyConfig, event, msg string) {
	text := composeNotify(event, msg)
	// TLS (Telegram, Discord, an https server) can't check certificates against a 1970 clock: right after
	// boot, wait for time sync (up to 10 min) instead of failing. Servers on plain http on your own network
	// don't need the clock: they're sent first, at once.
	var res []string
	res = append(res, sendLocal(c, event, msg, false)...)
	if c.TelegramOn || c.DiscordOn || c.hasHTTPS() {
		for i := 0; i < 120 && !clockOK(); i++ {
			time.Sleep(5 * time.Second)
		}
	}
	res = append(res, sendLocal(c, event, msg, true)...)
	if c.TelegramOn && c.TelegramToken != "" && c.TelegramChat != "" {
		if err := sendTelegram(c.TelegramToken, c.TelegramChat, text); err != nil {
			res = append(res, "Telegram ✗ "+shortErr(err))
		} else {
			res = append(res, "Telegram ✓")
		}
	}
	if c.DiscordOn && c.DiscordURL != "" {
		if err := sendDiscord(c.DiscordURL, text); err != nil {
			res = append(res, "Discord ✗ "+shortErr(err))
		} else {
			res = append(res, "Discord ✓")
		}
	}
	if len(res) > 0 {
		r := strings.Join(res, " · ")
		addNotifyRecord(text, r)
		if strings.Contains(r, "✗") {
			activity.emit("⚠ notification not delivered: " + r)
		}
	}
}

// shortErr keeps a send error readable and free of the webhook URL or bot token.
func shortErr(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err // never the request's address: it is the webhook, or holds the bot token
	}
	m := err.Error()
	if i := strings.LastIndex(m, ": "); i >= 0 && strings.Contains(m, "http") {
		m = m[i+2:]
	}
	if len(m) > 60 {
		m = m[:60]
	}
	return "(" + m + ")"
}

func sendTelegram(token, chat, text string) error {
	body, _ := json.Marshal(map[string]any{"chat_id": chat, "text": text})
	url := "https://api.telegram.org/bot" + token + "/sendMessage"
	resp, err := notifyHTTP.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return httpStatusErr("telegram ", resp.StatusCode)
	}
	return nil
}

func sendDiscord(webhook, text string) error {
	// no @everyone/@here/role pings from text a pool or the miner chose
	body, _ := json.Marshal(map[string]any{"content": text, "allowed_mentions": map[string]any{"parse": []string{}}})
	resp, err := notifyHTTP.Post(webhook, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return httpStatusErr("discord ", resp.StatusCode)
	}
	return nil
}

// ---- settings (network info + notify config) ----

func localIP() string {
	// Prefer the first non-loopback IPv4 from the interface list — a miner has one NIC, and this
	// keeps the firmware from ever referencing a public address. Fall back to a routing lookup
	// (a UDP "connect" that sends no packets, just to resolve the egress interface) only if needed.
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok && !ipnet.IP.IsLoopback() && ipnet.IP.To4() != nil {
			return ipnet.IP.String()
		}
	}
	if conn, err := net.Dial("udp", "192.168.0.0:9"); err == nil { // private, discard port; no packets sent
		defer conn.Close()
		if a, ok := conn.LocalAddr().(*net.UDPAddr); ok {
			return a.IP.String()
		}
	}
	return ""
}

// mask shows only the last 4 chars of a secret for display.
func mask(s string) string {
	if s == "" {
		return ""
	}
	if len(s) <= 4 {
		return "••••"
	}
	return "••••" + s[len(s)-4:]
}

func handleSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var in notifyConfig
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeJSON(w, map[string]any{"ok": false, "err": badJSON(err)})
			return
		}
		notifyMu.Lock()
		cur := notifyCfg
		// blank secrets mean "keep the stored one" (the UI shows them masked); "-" removes it
		for _, f := range []struct{ in, cur *string }{{&in.TelegramToken, &cur.TelegramToken}, {&in.DiscordURL, &cur.DiscordURL},
			{&in.NtfyToken, &cur.NtfyToken}, {&in.GotifyToken, &cur.GotifyToken}, {&in.WebhookURL, &cur.WebhookURL}} {
			switch v := strings.TrimSpace(*f.in); v {
			case "":
			case "-":
				*f.cur = ""
			default:
				*f.cur = v
			}
		}
		cur.NtfyOn, cur.NtfyURL, cur.NtfyTopic = in.NtfyOn, strings.TrimSpace(in.NtfyURL), strings.TrimSpace(in.NtfyTopic)
		cur.GotifyOn, cur.GotifyURL = in.GotifyOn, strings.TrimSpace(in.GotifyURL)
		cur.WebhookOn = in.WebhookOn
		for _, u := range []string{cur.NtfyURL, cur.GotifyURL, cur.WebhookURL, cur.DiscordURL} {
			if u != "" && !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
				notifyMu.Unlock()
				writeJSON(w, map[string]any{"ok": false, "err": "server addresses start with http:// or https:// (" + u + ")"})
				return
			}
		}
		cur.TelegramChat = strings.TrimSpace(in.TelegramChat)
		cur.TelegramOn = in.TelegramOn
		cur.DiscordOn = in.DiscordOn
		if in.Events != nil {
			cur.Events = in.Events
		}
		if in.HotC != 0 && (in.HotC < 50 || in.HotC > model.AbortMaxC) {
			notifyMu.Unlock()
			writeJSON(w, map[string]any{"ok": false, "err": fmt.Sprintf("the hot alert must be 50–%.0f °C (0 = off): the boards cut out at %.0f °C", model.AbortMaxC, model.TempCutoffC)})
			return
		}
		if in.HashLowPct < 0 || in.HashLowPct > 99 {
			notifyMu.Unlock()
			writeJSON(w, map[string]any{"ok": false, "err": "the hashrate-low alert must be 0–99 % of the usual"})
			return
		}
		if in.MinIntervalMin == 0 { // not sent: keep it
			in.MinIntervalMin = max(cur.MinIntervalMin, 1)
		}
		if in.MinIntervalMin < 1 || in.MinIntervalMin > 1440 {
			notifyMu.Unlock()
			writeJSON(w, map[string]any{"ok": false, "err": "the minimum gap between alerts must be 1–1440 minutes"})
			return
		}
		cur.HotC = in.HotC
		cur.HashLowPct = in.HashLowPct
		cur.MinIntervalMin = in.MinIntervalMin
		notifyCfg = cur
		notifyMu.Unlock()
		if err := saveNotifyConfig(cur); err != nil {
			writeJSON(w, map[string]any{"ok": false, "err": "save failed: " + err.Error()})
			return
		}
		writeJSON(w, map[string]any{"ok": true})
		return
	}
	// GET
	notifyMu.Lock()
	c := notifyCfg
	hist := append([]notifyRecord(nil), notifyHistory...)
	notifyMu.Unlock()
	host, _ := os.Hostname()
	authMu.RLock()
	user := auth.Username
	authMu.RUnlock()
	labels := make([]map[string]string, 0, len(eventOrder))
	for _, k := range eventOrder {
		lbl := eventLabels[k]
		if k == evTuning && model.Plan == planBox2V && !model.Feat.Tuner { // no tuner on this unit: don't name it
			lbl = "Strong-chip test: started, finished, stopped or aborted"
		}
		labels = append(labels, map[string]string{"key": k, "label": lbl, "emoji": eventEmoji[k]})
	}
	writeJSON(w, map[string]any{
		"ok": true,
		"notify": map[string]any{
			"telegram_chat": c.TelegramChat, "telegram_on": c.TelegramOn, "telegram_set": c.TelegramToken != "", "telegram_hint": mask(c.TelegramToken),
			"discord_on": c.DiscordOn, "discord_set": c.DiscordURL != "", "discord_hint": mask(c.DiscordURL),
			"ntfy_on": c.NtfyOn, "ntfy_url": c.NtfyURL, "ntfy_topic": c.NtfyTopic, "ntfy_token_set": c.NtfyToken != "", "ntfy_token_hint": mask(c.NtfyToken),
			"gotify_on": c.GotifyOn, "gotify_url": c.GotifyURL, "gotify_set": c.GotifyToken != "", "gotify_hint": mask(c.GotifyToken),
			"webhook_on": c.WebhookOn, "webhook_set": c.WebhookURL != "", "webhook_hint": mask(c.WebhookURL),
			"events": c.Events, "hot_c": c.HotC, "hash_low_pct": c.HashLowPct, "min_interval_min": c.MinIntervalMin,
		},
		"event_labels": labels,
		"notify_log":   hist,
		"network": map[string]any{
			"hostname": host, "ip": localIP(), "ssh": sshStatus(), "listen": *listenAddr,
			"port_saved": currentNetConfig().Port, // the panel's port from the next start (0: the default)
			"access":     accessView(),            // SC Box II: who may reach the panel (nil on the SC Lite)
		},
		"account": map[string]any{"username": user},
		"display": map[string]any{"share_unit": getShareUnit(), "hash_unit": getHashUnit()},
	})
}

func handleNotifyTest(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Channel string `json:"channel"` // "telegram" | "discord"
	}
	json.NewDecoder(r.Body).Decode(&in)
	notifyMu.Lock()
	c := notifyCfg
	notifyMu.Unlock()
	var err error
	switch in.Channel {
	case "telegram":
		if c.TelegramToken == "" || c.TelegramChat == "" {
			writeJSON(w, map[string]any{"ok": false, "err": "set the Telegram bot token and chat ID first (Save), then test"})
			return
		}
		err = sendTelegram(c.TelegramToken, c.TelegramChat, minerLabel()+": ✅ test message")
	case "discord":
		if c.DiscordURL == "" {
			writeJSON(w, map[string]any{"ok": false, "err": "set the Discord webhook URL first (Save), then test"})
			return
		}
		err = sendDiscord(c.DiscordURL, minerLabel()+": ✅ test message")
	case "ntfy":
		if c.NtfyURL == "" || c.NtfyTopic == "" {
			writeJSON(w, map[string]any{"ok": false, "err": "set the ntfy server and topic first (Save), then test"})
			return
		}
		err = sendNtfy(c, evAccepted, "test message")
	case "gotify":
		if c.GotifyURL == "" || c.GotifyToken == "" {
			writeJSON(w, map[string]any{"ok": false, "err": "set the Gotify server and app token first (Save), then test"})
			return
		}
		err = sendGotify(c, evAccepted, "test message")
	case "webhook":
		if c.WebhookURL == "" {
			writeJSON(w, map[string]any{"ok": false, "err": "set the webhook URL first (Save), then test"})
			return
		}
		err = sendWebhook(c, "test", "test message")
	default:
		writeJSON(w, map[string]any{"ok": false, "err": "unknown channel"})
		return
	}
	if err != nil {
		addNotifyRecord("test message", in.Channel+" ✗")
		m := err.Error() // without the secrets in it: this text ends up in screenshots
		for _, t := range []string{c.DiscordURL, c.WebhookURL, c.TelegramToken, c.NtfyToken, c.GotifyToken, c.NtfyTopic} {
			if t != "" {
				m = strings.ReplaceAll(m, t, "[secret]")
			}
		}
		m = reWebhook.ReplaceAllString(reTgToken.ReplaceAllString(m, "[telegram-token]"), "[discord-webhook]")
		writeJSON(w, map[string]any{"ok": false, "err": m})
		return
	}
	addNotifyRecord("test message", in.Channel+" ✓")
	writeJSON(w, map[string]any{"ok": true, "note": "sent — check " + in.Channel})
}

// ---- channels on your own network: ntfy, Gotify, a webhook ----

func (c notifyConfig) hasHTTPS() bool {
	h := func(on bool, u string) bool {
		return on && strings.HasPrefix(strings.ToLower(strings.TrimSpace(u)), "https://")
	}
	return h(c.NtfyOn, c.NtfyURL) || h(c.GotifyOn, c.GotifyURL) || h(c.WebhookOn, c.WebhookURL)
}

// notifyPriority: 5 urgent, 4 high, 3 normal, 2 low (ntfy's scale; Gotify gets it doubled, 0–10).
// Problems alert loudest; recoveries, records and the daily summary quietly.
func notifyPriority(event, msg string) int {
	if strings.Contains(msg, " again") || strings.HasPrefix(msg, "back on") {
		return 3
	}
	if event == evTuning && (strings.HasPrefix(msg, "tuning run aborted") || strings.HasPrefix(msg, "tuning run cut short")) {
		return 4
	}
	switch event {
	case evHot, evThermal:
		return 5
	case evStopped, evBoard, evFan, evNoPool, evPoolGuard, evLogin:
		return 4
	case evBestShare, evDaily, evRejected, evAccepted, evSchedule:
		return 2
	}
	return 3
}

// sendLocal sends to the ntfy, Gotify and webhook servers that are switched on: those on https when https
// is true, the others when it's false.
func sendLocal(c notifyConfig, event, msg string, https bool) []string {
	isHTTPS := func(u string) bool { return strings.HasPrefix(strings.ToLower(strings.TrimSpace(u)), "https://") }
	var res []string
	add := func(name string, err error) {
		if err != nil {
			res = append(res, name+" ✗ "+shortErr(err))
		} else {
			res = append(res, name+" ✓")
		}
	}
	if c.NtfyOn && c.NtfyURL != "" && c.NtfyTopic != "" && isHTTPS(c.NtfyURL) == https {
		add("ntfy", sendNtfy(c, event, msg))
	}
	if c.GotifyOn && c.GotifyURL != "" && c.GotifyToken != "" && isHTTPS(c.GotifyURL) == https {
		add("Gotify", sendGotify(c, event, msg))
	}
	if c.WebhookOn && c.WebhookURL != "" && isHTTPS(c.WebhookURL) == https {
		add("webhook", sendWebhook(c, event, msg))
	}
	return res
}

func emojiMsg(event, msg string) string {
	if em := eventEmoji[event]; em != "" {
		return em + " " + msg
	}
	return msg
}

func postJSON(url string, body any, hdr map[string]string) error {
	b, _ := json.Marshal(body)
	req, err := http.NewRequest("POST", url, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := notifyHTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return httpStatusErr("", resp.StatusCode)
	}
	return nil
}

// sendNtfy: ntfy's JSON publish (POST to the server's root), title = the miner, emoji in the message.
func sendNtfy(c notifyConfig, event, msg string) error {
	hdr := map[string]string{}
	if c.NtfyToken != "" {
		hdr["Authorization"] = "Bearer " + c.NtfyToken
	}
	return postJSON(strings.TrimRight(strings.TrimSpace(c.NtfyURL), "/"), map[string]any{
		"topic": strings.TrimSpace(c.NtfyTopic), "title": minerLabel(), "message": emojiMsg(event, msg),
		"priority": notifyPriority(event, msg),
	}, hdr)
}

// sendGotify: POST /message with the application token.
func sendGotify(c notifyConfig, event, msg string) error {
	return postJSON(strings.TrimRight(strings.TrimSpace(c.GotifyURL), "/")+"/message", map[string]any{
		"title": minerLabel(), "message": emojiMsg(event, msg), "priority": notifyPriority(event, msg) * 2,
	}, map[string]string{"X-Gotify-Key": c.GotifyToken})
}

// sendWebhook: a JSON POST any automation can read.
func sendWebhook(c notifyConfig, event, msg string) error {
	return postJSON(strings.TrimSpace(c.WebhookURL), map[string]any{
		"miner": minerLabel(), "ip": localIP(), "event": event, "emoji": eventEmoji[event], "message": msg,
		"text": composeNotify(event, msg), "priority": notifyPriority(event, msg), "time": time.Now().Unix(),
	}, nil)
}
