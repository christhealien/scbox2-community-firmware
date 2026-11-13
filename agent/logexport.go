package main

// Log tab -> Save: the miner's own log (Mining) and the system log (System) as text files, or both in a
// zip with private details taken out, for sending to someone.
//
// "Private details out" replaces: the pools' addresses and accounts (wallet, worker), this miner's name
// and hostname, IP and MAC addresses (not 127.x), wallet-looking strings, and notification secrets
// (Discord webhooks, Telegram bot tokens), so the files can be posted publicly.

import (
	"archive/zip"
	"bytes"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	reIPv4     = regexp.MustCompile(`\b(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})\b`)
	reMAC      = regexp.MustCompile(`(?i)\b[0-9a-f]{2}(?::[0-9a-f]{2}){5}\b`)
	reWallet   = regexp.MustCompile(`\b(?:bc1[a-z0-9]{20,90}|[13][a-km-zA-HJ-NP-Z1-9]{25,34}|0x[0-9a-fA-F]{40}|nexa:[a-z0-9]{40,}|[a-f0-9]{76})\b`)
	reWebhook  = regexp.MustCompile(`https?://[^\s"']*discord(?:app)?\.com/api/webhooks/[^\s"']+`)
	reTgToken  = regexp.MustCompile(`\b(?:bot)?\d{6,12}:[A-Za-z0-9_-]{30,}\b`)
	reURLCred  = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)[^/\s"']+@`)                             // scheme://user:pass@ (to the last @: a user can hold one)
	reURLQuery = regexp.MustCompile(`(?i)([?&](?:password|passwd|pass|pwd|token|key|secret)=)[^&\s"')]+`) // ?password=… (the stock login), ?token=…
	// an IPv6 address, full or compressed ("2a02:8071:5a1:5600::1a2b", "fe80::1%eth0"): every run of hex groups
	// and colons is a candidate, and net.ParseIP decides (a time like 12:34:56 isn't one)
	reIPv6     = regexp.MustCompile(`(?i)[0-9a-f]{0,4}(?::[0-9a-f]{0,4}){2,7}(?:%[\w.-]+)?`)
	reSSHLogin = regexp.MustCompile(`(SSH sign-in: ).*?( from )`)                // the panel's login name (ssh.go)
	reIPPart   = regexp.MustCompile(`\b\d{1,3}\.\d{1,3}(?:\.\d{1,3})? \d{2,5}:`) // a cut-off address + port ("68.8.235 23334")
	// a pool password, whatever its length and whether or not the backend answers: the OEM's backend logs each
	// pool as JSON ("pass": "..."), the miner's protocol dump (verbose) logs mining.authorize ["user", "pass"]
	rePassJSON  = regexp.MustCompile(`(?i)("pass(?:word)?"\s*:\s*")(?:[^"\\]|\\.)*"`)
	reAuthorize = regexp.MustCompile(`(mining\.authorize"\s*,\s*"params"\s*:\s*\[\s*"(?:[^"\\]|\\.)*"\s*,\s*")(?:[^"\\]|\\.)*"`)
)

// privateTokens: the strings to take out, longest first (a worker "wallet.rig" before "wallet"): today's
// pools, accounts and names, every one known before (rememberPrivate), and the notification secrets.
func privateTokens() []string {
	set := map[string]bool{}
	addTok := func(s string) {
		if s = strings.TrimSpace(s); len(s) >= 3 {
			set[s] = true
		}
	}
	cur := poolNameTokens()
	rememberPrivate(cur)
	for _, t := range cur {
		addTok(t)
	}
	for _, t := range rememberedPrivate() {
		addTok(t)
	}
	notifyMu.Lock()
	nc := notifyCfg
	notifyMu.Unlock()
	for _, t := range []string{nc.TelegramToken, nc.TelegramChat, nc.DiscordURL, nc.NtfyTopic, nc.NtfyToken, nc.GotifyToken, nc.WebhookURL, nc.NtfyURL, nc.GotifyURL} {
		if t = strings.TrimSpace(t); len(t) >= 8 { // short words ("miner") would blank the log itself
			addTok(t)
		}
	}
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	return out
}

// poolNameTokens: this miner's pools (addresses, accounts: wallet and worker, passwords of 6+ characters)
// and its name, as they are now.
func poolNameTokens() []string {
	var out []string
	if list, _, err := fetchPoolList(); err == nil {
		out = poolListTokens(list)
	}
	out = append(out, configPoolTokens()...)
	out = append(out, minerLabel())
	if h, err := os.Hostname(); err == nil {
		out = append(out, h)
	}
	return out
}

// poolListTokens: the private strings of a pool list as the backend gives it.
func poolListTokens(list []map[string]any) []string {
	var out []string
	addTok := func(s string) { out = append(out, s) }
	{
		for _, p := range list {
			u, _ := p["url"].(string)
			usr, _ := p["user"].(string)
			addTok(usr)
			if pw, _ := p["pass"].(string); len(pw) >= 6 {
				addTok(pw) // the pool password can be an account key
			}
			if i := strings.Index(usr, "."); i > 0 {
				addTok(usr[:i])   // the wallet
				addTok(usr[i+1:]) // the worker name, which the miner logs on its own
			}
			if pu, err := url.Parse(u); err == nil && pu.Hostname() != "" {
				addTok(pu.Hostname())
			} else {
				h := u
				if i := strings.Index(h, "://"); i >= 0 {
					h = h[i+3:]
				}
				addTok(strings.Split(h, ":")[0])
			}
		}
	}
	return out
}

// Files kept from before a change still carry the former pools and names: the incidents, the panel events
// (500 lines, across restarts), the system log kept from before the last restart, and the running system
// log itself (syslogd writes the hostname it started with into every line until the next reboot). So every
// private string the panel has seen is remembered (in the config folder, beside the pools and auth.json,
// the newest 300) and taken out of every later export.
var privSeenMu sync.Mutex

func privateSeenPath() string { return filepath.Join(filepath.Dir(*authFile), "private-seen.txt") }

func rememberedPrivate() []string {
	b, err := os.ReadFile(privateSeenPath())
	if err != nil {
		return nil
	}
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// rememberPrivate adds the strings it hasn't seen yet (it writes only then).
func rememberPrivate(toks []string) {
	privSeenMu.Lock()
	defer privSeenMu.Unlock()
	have := rememberedPrivate()
	seen := map[string]bool{}
	for _, t := range have {
		seen[t] = true
	}
	added := false
	for _, t := range toks {
		if t = strings.TrimSpace(t); len(t) >= 3 && !seen[t] && !strings.ContainsAny(t, "\r\n") {
			seen[t] = true
			have = append(have, t)
			added = true
		}
	}
	if !added {
		return
	}
	if len(have) > 300 {
		have = have[len(have)-300:]
	}
	writeAtomic(privateSeenPath(), []byte(strings.Join(have, "\n")+"\n"), 0o600)
}

func redactLog(s string, toks []string) string {
	// pools that aren't in the Pools tab (so not among the tokens): the pool guard's own lines name the foreign
	// pool's address and account; any stratum URL and "(account ...)" goes, whoever's it is
	s = reHoldWhat.ReplaceAllString(s, "${1}[pool]${2}")
	s = reStratumURL.ReplaceAllString(s, "[pool]")
	s = rePoolAccount.ReplaceAllString(s, "${1}[private])")
	s = rePassJSON.ReplaceAllString(s, `${1}[private]"`)
	s = reAuthorize.ReplaceAllString(s, `${1}[private]"`)
	s = reWebhook.ReplaceAllString(s, "[discord-webhook]")
	s = reURLCred.ReplaceAllString(s, "${1}[login]@")
	s = reURLQuery.ReplaceAllString(s, "${1}[private]")
	s = reTgToken.ReplaceAllString(s, "[telegram-token]")
	for _, t := range toks {
		s = strings.ReplaceAll(s, t, "[private]")
	}
	s = reWallet.ReplaceAllString(s, "[wallet]")
	s = reMAC.ReplaceAllString(s, "xx:xx:xx:xx:xx:xx")
	s = reIPv4.ReplaceAllStringFunc(s, func(ip string) string {
		if strings.HasPrefix(ip, "127.") || ip == "0.0.0.0" {
			return ip
		}
		return "x.x.x.x"
	})
	s = reIPPart.ReplaceAllString(s, "x.x.x [port]:")
	s = redactIPv6(s)
	s = reSSHLogin.ReplaceAllString(s, "${1}[login]${2}")
	return s
}

// redactIPv6 replaces every IPv6 address except ::1 and ::. A candidate that starts inside a word (a hex
// string or a name before it) is left alone.
func redactIPv6(s string) string {
	var b strings.Builder
	last := 0
	for _, m := range reIPv6.FindAllStringIndex(s, -1) {
		c := s[m[0]:m[1]]
		if m[0] > 0 {
			if p := s[m[0]-1]; p == '_' || p == '.' || p >= '0' && p <= '9' || p|0x20 >= 'a' && p|0x20 <= 'z' {
				continue
			}
		}
		addr := c
		if i := strings.IndexByte(addr, '%'); i >= 0 {
			addr = addr[:i]
		}
		ip := net.ParseIP(addr)
		if ip == nil || ip.To4() != nil && !strings.Contains(addr, "::") || ip.IsLoopback() || ip.IsUnspecified() {
			continue
		}
		b.WriteString(s[last:m[0]])
		b.WriteString("x:x::x")
		last = m[1]
	}
	if last == 0 {
		return s
	}
	b.WriteString(s[last:])
	return b.String()
}

// minerLogText: the miner's own log as the miner keeps it (its last few hundred lines).
func minerLogText() (string, error) {
	raw, err := stockGet("/dbg/minersyslog")
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, l := range strings.Split(strings.ReplaceAll(logText(raw), "\r\n", "\n"), "\n") {
		if strings.TrimSpace(l) != "" && !agentAPINoise(l) {
			b.WriteString(strings.TrimRight(l, " ") + "\n")
		}
	}
	return stockTextShown(b.String()), nil
}

// systemLogText: the system log, the older file first when it has been rotated (I2C probe noise left out,
// as in the Log tab).
func systemLogText() (string, error) {
	var b strings.Builder
	found := false
	for _, p := range []string{*logPath + ".0", *logPath} {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		found = true
		for _, l := range strings.Split(string(data), "\n") {
			l = strings.TrimRight(l, "\r")
			if strings.TrimSpace(l) != "" && !noise(l) {
				b.WriteString(l + "\n")
			}
		}
	}
	if !found {
		return "", os.ErrNotExist
	}
	return stockTextShown(b.String()), nil
}

func logHeader(what string, private bool) string {
	name := minerLabel()
	if private {
		name = "MINER (private details taken out)"
	}
	return "# " + exportBrand() + " — " + what + "\n# miner: " + name + "\n# firmware: " + firmwareVersion() +
		"\n# saved: " + time.Now().Format("2006-01-02 15:04:05 MST") + "\n\n"
}

// GET /api/log/export?src=mine|sys|private
func handleLogExport(w http.ResponseWriter, r *http.Request) {
	stamp := time.Now().Format("2006-01-02-1504")
	label := strings.ReplaceAll(minerLabel(), " ", "_")
	switch r.URL.Query().Get("src") {
	case "sys":
		t, err := systemLogText()
		if err != nil {
			http.Error(w, "the system log can't be read: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		serveBytes(w, label+"-system-log-"+stamp+".txt", "text/plain; charset=utf-8", []byte(logHeader("system log", false)+t))
	case "private":
		toks := privateTokens()
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		add := func(name, data string) {
			if f, err := zw.Create("miner-logs-" + stamp + "/" + name); err == nil {
				f.Write([]byte(data))
			}
		}
		if t, err := minerLogText(); err == nil {
			add("mining-log.txt", logHeader("mining log", true)+redactLog(t, toks))
		} else {
			add("mining-log.txt", "(unavailable: "+err.Error()+")\n")
		}
		if t, err := systemLogText(); err == nil {
			add("system-log.txt", logHeader("system log", true)+redactLog(t, toks))
		} else {
			add("system-log.txt", "(unavailable: "+err.Error()+")\n")
		}
		if b, err := os.ReadFile(keptLogBeforePath()); err == nil { // Settings → Recovery → keep the system log
			fi, _ := os.Stat(keptLogBeforePath())
			add("system-log-before-last-restart.txt", logHeader("system log before the last restart (saved "+fi.ModTime().Format("2006-01-02 15:04")+")", true)+redactLog(string(b), toks))
		}
		add("processes.txt", processesText(time.Now()))
		add("panel-events.txt", logHeader("panel events (the activity feed, kept on the miner)", true)+redactLog(eventsText(), toks))
		for _, in := range incidentTexts() {
			add(in[0], logHeader(in[0]+": the logs as they were at the incident", true)+stockTextShown(redactBarcodes(redactLog(in[1], toks))))
		}
		for _, gl := range oemLogs() {
			if t, err := tailFile(gl[1], 400<<10); err == nil {
				add(gl[0], logHeader(gl[0], true)+stockTextShown(redactBarcodes(redactLog(t, toks))))
			} else {
				add(gl[0], "(unavailable: "+err.Error()+")\n")
			}
		}
		zw.Close()
		serveBytes(w, "miner-logs-private-"+stamp+".zip", "application/zip", buf.Bytes())
	default:
		t, err := minerLogText()
		if err != nil {
			http.Error(w, "the mining log can't be read: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		serveBytes(w, label+"-mining-log-"+stamp+".txt", "text/plain; charset=utf-8", []byte(logHeader("mining log", false)+t))
	}
}

// redactBarcodes: the serial numbers the OEM's backend logs at every start (box, control board, boards).
func redactBarcodes(s string) string {
	return reBarcode.ReplaceAllString(s, "${1}[serial]")
}

var reBarcode = regexp.MustCompile(`(hardware_barcode->[a-z]+(?:\[\d+\])?: )\S+`)
