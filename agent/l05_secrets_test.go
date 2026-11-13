package main

// L05-secrets: the private log export ("For sending, private details out") cleans every file with the
// private strings known at the moment of the export: today's pools and today's hostname. Files that were
// written before a change keep the old ones: the saved incidents (gzip), the panel events (events.log, 500
// lines across restarts), the system log kept from before the last restart, and the running system log
// itself (busybox syslogd writes the hostname it read at its start into every line).

import (
	"archive/zip"
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
)

const l05Sia = "a7f3c19d2e8b4f6a0c5d1e9b3f7a2c8d4e6b0f1a3c5e7d9b2f4a6c8e0d1b3f5a7c9e2d4f6b8a"

// l05Env: a miner with its backend (pools + mining log), its files in a temp dir; returns setters.
func l05Env(t *testing.T) (setPools func(string), setMinerLog func(string)) {
	t.Helper()
	d := t.TempDir()
	var pools, mlog atomic.Value
	pools.Store(`[]`)
	mlog.Store(``)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/mcb/pools":
			io.WriteString(w, pools.Load().(string))
		case "/dbg/minersyslog":
			io.WriteString(w, mlog.Load().(string))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	oldWeb, oldAuth, oldLog, oldBfg, oldBR := *minerWeb, *authFile, *logPath, *bfgPath, backupRoot
	*minerWeb = strings.TrimPrefix(srv.URL, "http://")
	*authFile = filepath.Join(d, "config", "auth.json")
	*logPath = filepath.Join(d, "messages")
	*bfgPath = filepath.Join(d, "config", "bfgminer", "bfgminer.json")
	backupRoot = filepath.Join(d, "backup")
	os.MkdirAll(filepath.Dir(*authFile), 0o755)
	oldInc := incidentLogs
	jwtMu.Lock()
	oldJWT := jwtToken
	jwtToken = "test-session" // signed in to the backend already
	jwtMu.Unlock()
	t.Cleanup(func() {
		jwtMu.Lock()
		jwtToken = oldJWT
		jwtMu.Unlock()
		*minerWeb, *authFile, *logPath, *bfgPath, backupRoot = oldWeb, oldAuth, oldLog, oldBfg, oldBR
		incidentLogs = oldInc
		evMu.Lock()
		evLines, evLoaded, evDirty = nil, false, false
		evMu.Unlock()
	})
	evMu.Lock()
	evLines, evLoaded, evDirty = nil, true, false
	evMu.Unlock()
	return func(s string) { pools.Store(s) }, func(s string) { mlog.Store(s) }
}

// l05Export: GET /api/log/export?src=private, every file in the zip joined.
func l05Export(t *testing.T) string {
	t.Helper()
	rec := httptest.NewRecorder()
	handleLogExport(rec, httptest.NewRequest("GET", "/api/log/export?src=private", nil))
	zr, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
	if err != nil {
		t.Fatalf("not a zip: %v", err)
	}
	var all strings.Builder
	for _, f := range zr.File {
		rc, _ := f.Open()
		b, _ := io.ReadAll(rc)
		rc.Close()
		all.WriteString("=== " + f.Name + "\n" + string(b) + "\n")
	}
	return all.String()
}

// The owner mined on one pool, a board dropped (an incident was saved), then they moved to a new pool and
// a new wallet and sent the private export: the former pool's account, worker and servers are in it.
func TestL05FormerPoolInIncidentAndEvents(t *testing.T) {
	setPools, setLog := l05Env(t)
	setPools(`[{"url":"stratum+tcp://us-east.oldpool.example:3333","user":"joesmith.basement","pass":"x"},
		{"url":"stratum+tcp://backup.oldpool.example:443","user":"joesmith.basement","pass":"x"}]`)
	setLog(`{"body":" [2026-11-01 12:58:02] Pool 0 stratum+tcp://us-east.oldpool.example:3333 alive, user joesmith.basement\n [2026-11-01 13:02:11] Stratum connection to pool 0 interrupted\n [2026-11-01 13:02:12] Pool 1 stratum+tcp://backup.oldpool.example:443 alive\n"}`)
	activity.emit("🔀 switched to a backup pool: stratum+tcp://backup.oldpool.example:443 (the primary, stratum+tcp://us-east.oldpool.example:3333, is dead)")
	incidentSnapshot("board 1 stopped hashing") // the real snapshot: the mining log as it is now
	if n := len(incidentTexts()); n != 1 || !strings.Contains(incidentTexts()[0][1], "joesmith.basement") {
		t.Fatalf("setup: the incident wasn't saved with the log (%d): %q", n, incidentTexts())
	}
	eventsSave()

	// days later: a new pool and a Siacoin wallet; the miner log has rolled over
	setPools(`[{"url":"stratum+tcp://sia.newpool.example:3334","user":"` + l05Sia + `.attic","pass":"x"}]`)
	setLog(`{"body":" [2026-11-09 09:00:01] Pool 0 stratum+tcp://sia.newpool.example:3334 alive, user ` + l05Sia + `.attic\n"}`)
	out := l05Export(t)
	for _, bad := range []string{"joesmith", "basement", "oldpool.example", "attic", l05Sia[:20], "newpool"} {
		if strings.Contains(out, bad) {
			i := strings.Index(out, bad)
			t.Errorf("%q in the private export: …%s…", bad, out[max(0, i-120):min(len(out), i+60)])
		}
	}
	if !strings.Contains(out, "board 1 stopped hashing") || !strings.Contains(out, "switched to a backup pool") {
		t.Fatalf("too much taken out:\n%s", out)
	}
}

// The owner renamed the miner (Settings → hostname) and sent the private export before the next reboot:
// busybox syslogd keeps writing the name it started with into every system-log line, and the events say
// "hostname set to <old name>". (The miner's real syslogd: see L05-secrets/syslog_repro.sh.)
// It changes the hostname, so it runs only when asked, in a UTS namespace of its own:
//
//	L05_OWN_UTS=1 unshare -u go test -run TestL05FormerHostnameAfterRename .
func TestL05FormerHostnameAfterRename(t *testing.T) {
	if os.Getenv("L05_OWN_UTS") != "1" {
		t.Skip("changes the hostname: L05_OWN_UTS=1 unshare -u go test -run TestL05FormerHostnameAfterRename")
	}
	setPools, _ := l05Env(t)
	setPools(`[{"url":"stratum+tcp://sia.newpool.example:3334","user":"` + l05Sia + `.attic","pass":"x"}]`)
	old, _ := os.Hostname()
	t.Cleanup(func() { syscall.Sethostname([]byte(old)) })
	if err := syscall.Sethostname([]byte("joes-garage")); err != nil {
		t.Skip(err)
	}
	// the panel was running under the old name for a while (the pool guard checks every 30 s)
	oldL, oldP := pgListPools, pgLivePools
	t.Cleanup(func() { pgListPools, pgLivePools = oldL, oldP })
	pgListPools = func() ([]map[string]any, error) {
		return []map[string]any{{"url": "stratum+tcp://sia.newpool.example:3334", "user": l05Sia + ".attic"}}, nil
	}
	pgLivePools = func() ([]poolView, error) {
		return []poolView{{Index: 0, URL: "stratum+tcp://sia.newpool.example:3334", User: l05Sia + ".attic", Active: true}}, nil
	}
	pgStrikes, pgActed, pgIncidents = map[string]int{}, map[string]int{}, nil
	poolGuardCheck()
	activity.emit("🖧 hostname set to joes-garage")
	os.WriteFile(*logPath, []byte("Nov  9 10:20:52 joes-garage user.notice sclite: fan check passed\n"+
		"Nov  9 10:20:53 joes-garage daemon.info udhcpc[812]: sending renew to 192.168.1.1\n"), 0o644)
	syscall.Sethostname([]byte("rig2")) // what the panel's rename does (setHostname: hostname rig2)
	activity.emit("🖧 hostname set to rig2")
	out := l05Export(t)
	if strings.Contains(out, "joes-garage") {
		i := strings.Index(out, "joes-garage")
		t.Fatalf("the former name in the private export: …%s…", out[max(0, i-120):min(len(out), i+60)])
	}
	if !strings.Contains(out, "fan check passed") {
		t.Fatalf("too much taken out:\n%s", out)
	}
}

// Goldshell's backend (ulfius) has a request log "%s: verb(%s) url(%s)"; the panel signs in to it with
// GET /user/login?username=admin&password=<AES, fixed public key>&cipher=true. A password (or token) in a
// URL's query must not survive the export in any log.
func TestL05PasswordInURLQuery(t *testing.T) {
	in := "[2026-11-09 10:00:00] http: verb(GET) url(/user/login?username=admin&password=6f1c0a9b8e7d6c5b4a39281706f5e4d3&cipher=true)\n" +
		"GET http://192.168.1.20:8070/message?token=AbCdEf123456&x=1\n" +
		"stratum+tcp://joe@mail.com:hunter2@pool.example:3333"
	out := redactLog(in, nil)
	for _, bad := range []string{"6f1c0a9b8e7d", "AbCdEf123456", "hunter2"} {
		if strings.Contains(out, bad) {
			t.Errorf("%q left in:\n%s", bad, out)
		}
	}
	if !strings.Contains(out, "username=admin") || !strings.Contains(out, "cipher=true") {
		t.Errorf("too much taken out:\n%s", out)
	}
}
