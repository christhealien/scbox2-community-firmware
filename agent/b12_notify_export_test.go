package main

// B12: notifications & log export, model-aware + secret-safe on Box II.
//
// Proves:
//   - SC Lite log/export/report headers and labels use the profile name ("SC Lite", no manufacturer's name).
//   - On the SC Box II profile the same headers use its profile name (no manufacturer's name, no "SC Lite"),
//     the chip report uses the model id, and the hot-alert text uses the profile's thermal limits.
//   - The private log export's source path for the OEM's own logs is flag/model-driven: with
//     backupRoot pointed at a /dev/shm tree (the Box II layout) the export reads the backend log
//     from /dev/shm/minerd/trash/syslog, not a hardcoded /backup.
//   - Redaction still strips every secret category on Box II sample data (pools/wallet/worker,
//     webhook/token, miner name, IP, MAC), while loopback (127.x) is kept.
//
// useBox2 / l05Env are shared test helpers (r14box2plan_test.go, l05_secrets_test.go).

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---- 1. SC Lite headers (the default profile) ----

func TestB12SCLiteHeaders(t *testing.T) {
	if got := exportBrand(); got != "SC Lite" {
		t.Fatalf("exportBrand() = %q, want %q", got, "SC Lite")
	}
	if got := modelTag(); got != "SCLite" {
		t.Fatalf("modelTag() = %q, want %q", got, "SCLite")
	}
	if h := logHeader("mining log", false); !strings.HasPrefix(h, "# SC Lite — mining log\n# miner: ") {
		t.Fatalf("public logHeader not byte-identical:\n%q", h)
	}
	if h := logHeader("system log", true); !strings.HasPrefix(h, "# SC Lite — system log\n# miner: MINER (private details taken out)\n") {
		t.Fatalf("private logHeader not byte-identical:\n%q", h)
	}
	if rh := runHeader(tunerBundle{}, false); !strings.HasPrefix(rh, "# SC Lite — tuner run\n") {
		t.Fatalf("runHeader not byte-identical:\n%q", rh)
	}
	// the chip report label and filename stem are the SC Lite strings
	if got := model.ID + " chip report"; got != "sclite chip report" {
		t.Fatalf("chip report label = %q", got)
	}
}

func TestB12SCLiteHotAlertByteIdentical(t *testing.T) {
	err := settingsHotErr(t, 95) // 95 is out of range on SC Lite (50-90)
	want := "the hot alert must be 50–90 °C (0 = off): the boards cut out at 92 °C"
	if err != want {
		t.Fatalf("hot-alert error = %q, want %q", err, want)
	}
}

// ---- 2. SC Box II: model-aware headers, no manufacturer's name ----

func TestB12Box2Headers(t *testing.T) {
	useBox2(t)
	if got := exportBrand(); got != "SC Box II" {
		t.Fatalf("exportBrand() = %q, want %q", got, "SC Box II")
	}
	if oemWord.MatchString(exportBrand()) {
		t.Fatalf("Box II export brand must not name the manufacturer: %q", exportBrand())
	}
	if got := modelTag(); got != "SCBoxII" {
		t.Fatalf("modelTag() = %q, want %q", got, "SCBoxII")
	}
	for _, h := range []string{logHeader("mining log", false), logHeader("system log", true), runHeader(tunerBundle{}, false)} {
		if !strings.HasPrefix(h, "# SC Box II — ") {
			t.Fatalf("Box II header not model-aware:\n%q", h)
		}
		if oemWord.MatchString(h) || strings.Contains(h, "SC Lite") {
			t.Fatalf("Box II header leaks SC Lite or the manufacturer's name:\n%q", h)
		}
	}
}

func TestB12Box2ChipReportModelID(t *testing.T) {
	useBox2(t)
	d := t.TempDir()
	oldStats, oldHook := chipStatsPath, chipHookLog
	chipStatsPath = filepath.Join(d, "chipstats.json")
	chipHookLog = filepath.Join(d, "chiphook.log")
	t.Cleanup(func() { chipStatsPath, chipHookLog = oldStats, oldHook })

	rec := httptest.NewRecorder()
	handleChipReport(rec, httptest.NewRequest("GET", "/api/chip/report", nil))
	var rep map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil {
		t.Fatalf("report not JSON: %v\n%s", err, rec.Body.String())
	}
	if rep["report"] != "scbox2 chip report" {
		t.Fatalf("report label = %v, want %q", rep["report"], "scbox2 chip report")
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "scbox2-chips-") {
		t.Fatalf("download filename not model-aware: %q", cd)
	}
	if strings.Contains(rec.Body.String(), "sclite") {
		t.Fatalf("Box II chip report leaks 'sclite':\n%s", rec.Body.String())
	}
}

func TestB12Box2HotAlertModelAware(t *testing.T) {
	useBox2(t)
	err := settingsHotErr(t, 95) // out of range on Box II (50-84)
	// cut-off 86 (never raised), abort-max 84 (cutoff-2) — both from the profile
	if !strings.Contains(err, "50–84") || !strings.Contains(err, "86 °C") {
		t.Fatalf("Box II hot-alert text not profile-driven: %q", err)
	}
	if strings.Contains(err, "92") || strings.Contains(err, "90 °C") {
		t.Fatalf("Box II hot-alert text leaks SC Lite limits: %q", err)
	}
	// a value valid on Box II (<=84) must be accepted
	if err := settingsHotErr(t, 83); err != "" {
		t.Fatalf("83 °C should be accepted on Box II, got error: %q", err)
	}
}

// ---- 3. Box II log source is flag/model-driven (reads /dev/shm, not /backup) ----

func TestB12Box2LogSourceDevShm(t *testing.T) {
	useBox2(t)
	setPools, setLog := l05Env(t)
	setPools(`[{"url":"stratum+tcp://sia.pool.example:3334","user":"s1aWALLETxxxxxxxxxxxxxxxxxxxxxxxxxxxx.boxrig","pass":"poolsecretpw"}]`)
	setLog(`{"body":" [2026-11-09 09:00:01] Pool 0 stratum+tcp://sia.pool.example:3334 alive, user s1aWALLETxxxxxxxxxxxxxxxxxxxxxxxxxxxx.boxrig\n"}`)

	// Box II keeps the OEM's own logs under /dev/shm (RAM), not /backup. backupRoot is the
	// flag-driven knob (main.go -backup, coordinated with B4 = /dev/shm). Point it at a stand-in tree.
	shm := filepath.Join(t.TempDir(), "dev-shm")
	backupRoot = shm // l05Env's cleanup restores the original afterwards
	mp := filepath.Join(shm, "minerd", "trash", "syslog")
	if err := os.MkdirAll(filepath.Dir(mp), 0o755); err != nil {
		t.Fatal(err)
	}
	backendLog := "minerd boot: pool stratum+tcp://sia.pool.example:3334 user s1aWALLETxxxxxxxxxxxxxxxxxxxxxxxxxxxx.boxrig\n" +
		"peer 203.0.113.9 eth0 HWaddr aa:bb:cc:dd:ee:ff api 127.0.0.1:4028\n"
	if err := os.WriteFile(mp, []byte(backendLog), 0o644); err != nil {
		t.Fatal(err)
	}

	// oemLogs resolves the backend log under /dev/shm, never a hardcoded /backup
	var backendPath string
	for _, gl := range oemLogs() {
		if gl[0] == "oem-backend-minerd.txt" {
			backendPath = gl[1]
		}
	}
	if backendPath != mp {
		t.Fatalf("backend log path = %q, want the /dev/shm path %q", backendPath, mp)
	}
	if strings.Contains(backendPath, "/backup") {
		t.Fatalf("log source still hardcoded to /backup: %q", backendPath)
	}

	// the private export reads that file and redacts it
	all := l05Export(t)
	if !strings.Contains(all, "oem-backend-minerd.txt") {
		t.Fatalf("private export did not include the /dev/shm backend log")
	}
	for _, secret := range []string{"s1aWALLET", "boxrig", "sia.pool.example", "203.0.113.9", "aa:bb:cc:dd:ee:ff"} {
		if strings.Contains(all, secret) {
			i := strings.Index(all, secret)
			t.Fatalf("secret %q leaked from the Box II backend log: …%s…", secret, all[max(0, i-80):min(len(all), i+40)])
		}
	}
	if !strings.Contains(all, "127.0.0.1:4028") {
		t.Fatalf("loopback address wrongly redacted (should be kept)")
	}
}

// ---- 4. redaction still covers every secret category on Box II ----

func TestB12Box2RedactionAllCategories(t *testing.T) {
	useBox2(t)
	// a Box II has no Wi-Fi (no PSK category); the categories are the same as SC Lite otherwise.
	toks := []string{"s1aWALLETabc123.boxrig", "s1aWALLETabc123", "boxrig", "sia.pool.example", "my-box-miner"}
	sample := strings.Join([]string{
		"[2026-11-09 09:00:01] Pool 0 stratum+tcp://sia.pool.example:3334 alive, user s1aWALLETabc123.boxrig",
		`backend pools: [{"url":"stratum+tcp://sia.pool.example:3334","user":"s1aWALLETabc123.boxrig","pass":"poolsecretpw"}]`,
		"eth0: link up HWaddr 02:1a:2b:3c:4d:5e ip 192.168.9.42 gw 192.168.9.1",
		"notify discord https://discord.com/api/webhooks/111222333444/AbCdEfGhIjKlMnOpQrStUvWxYz0123456789abcdefSECRET failed",
		"telegram bot" + fakeTGToken + " refused",
		"host my-box-miner api 127.0.0.1:4028 (loopback)",
	}, "\n")

	out := redactLog(sample, toks)
	for _, bad := range []string{
		"s1aWALLET", "boxrig", "sia.pool.example", "poolsecretpw", // pool / wallet / worker / pass
		"02:1a:2b:3c:4d:5e",   // MAC
		"192.168.9.42",        // IP
		"AbCdEfGhIjKlMnOpQr",  // discord webhook secret
		"AAH9v8u7t6s5r4q3p2o", // telegram token
		"my-box-miner",        // miner name
	} {
		if strings.Contains(out, bad) {
			i := strings.Index(out, bad)
			t.Fatalf("%q left in redacted output: …%s…", bad, out[max(0, i-60):min(len(out), i+40)])
		}
	}
	// loopback kept, and the line structure survives
	if !strings.Contains(out, "127.0.0.1:4028") {
		t.Fatalf("loopback wrongly redacted:\n%s", out)
	}
	if !strings.Contains(out, "alive") {
		t.Fatalf("too much removed (non-secret text gone):\n%s", out)
	}
}

// ---- helpers ----

// settingsHotErr POSTs a hot-alert threshold to handleSettings and returns the "err" string ("" = accepted).
func settingsHotErr(t *testing.T, hotC float64) string {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"hot_c": hotC})
	rec := httptest.NewRecorder()
	handleSettings(rec, httptest.NewRequest("POST", "/api/settings", bytes.NewReader(body)))
	var resp struct {
		OK  bool   `json:"ok"`
		Err string `json:"err"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("settings response not JSON: %v\n%s", err, rec.Body.String())
	}
	if resp.OK {
		return ""
	}
	return resp.Err
}
