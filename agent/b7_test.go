package main

// B7 — in-panel update/rollback disabled on the SC Box II, settings export/import, squashfs backstop.

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withModel sets the active profile for one test and restores it after.
func withModel(t *testing.T, p modelProfile) {
	t.Helper()
	old, oldWhy := model, modelWhy
	model, modelWhy = p, "test: "+p.ID
	t.Cleanup(func() { model, modelWhy = old, oldWhy })
}

// --- update/rollback gated off on Box II ---

func TestB7_UpdateRefusedOnBox2(t *testing.T) {
	withModel(t, profSCBox2)
	for _, body := range []string{`{"install":true}`, `{"rollback":true}`, `{"check":true}`, `{"fetch":true}`} {
		req := httptest.NewRequest("POST", "/api/update", strings.NewReader(body))
		rec := httptest.NewRecorder()
		handleUpdate(rec, req)
		var m map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
			t.Fatalf("bad JSON for %s: %v", body, err)
		}
		if m["ok"] != false {
			t.Fatalf("%s: expected ok=false, got %v", body, m["ok"])
		}
		if m["disabled"] != true {
			t.Fatalf("%s: expected disabled=true, got %v", body, m["disabled"])
		}
		errs, _ := m["err"].(string)
		if !strings.Contains(errs, "SD card") || !strings.Contains(errs, "export your settings") {
			t.Fatalf("%s: message should point to SD install + export, got %q", body, errs)
		}
	}
}

func TestB7_UpdateUploadRefusedOnBox2(t *testing.T) {
	withModel(t, profSCBox2)
	req := httptest.NewRequest("POST", "/api/update/upload", strings.NewReader("junk-package-bytes"))
	rec := httptest.NewRecorder()
	handleUpdateUpload(rec, req)
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	if m["ok"] != false || m["disabled"] != true {
		t.Fatalf("upload should be refused on Box II: %v", m)
	}
}

func TestB7_UpdateGetMarkedDisabledOnBox2(t *testing.T) {
	withModel(t, profSCBox2)
	req := httptest.NewRequest("GET", "/api/update", nil)
	rec := httptest.NewRecorder()
	handleUpdate(rec, req)
	var m map[string]any
	json.Unmarshal(rec.Body.Bytes(), &m)
	if m["disabled"] != true {
		t.Fatalf("GET on Box II should mark disabled: %v", m)
	}
	if r, _ := m["disabled_reason"].(string); !strings.Contains(r, "SD card") {
		t.Fatalf("disabled_reason should name SD install: %q", r)
	}
}

// regression: SC Lite is not gated — GET has no disabled flag, upload proceeds past the gate.
func TestB7_UpdateNotGatedOnSCLite(t *testing.T) {
	withModel(t, profSCLite)
	req := httptest.NewRequest("GET", "/api/update", nil)
	rec := httptest.NewRecorder()
	handleUpdate(rec, req)
	var m map[string]any
	json.Unmarshal(rec.Body.Bytes(), &m)
	if _, ok := m["disabled"]; ok {
		t.Fatalf("SC Lite update must not be marked disabled: %v", m)
	}
}

// --- squashfs backstop ---

func TestB7_SquashfsBackstopInstall(t *testing.T) {
	withModel(t, profSCLite) // backstop is independent of the feature gate
	oldMagic, oldRoot := fsMagicAt, updateRoot
	updateRoot = "/" // the backstop only looks at updateRoot's fs type
	fsMagicAt = func(string) (int64, error) { return squashfsMagic, nil }
	t.Cleanup(func() { fsMagicAt, updateRoot = oldMagic, oldRoot })

	if _, err := installUpdate("/tmp/does-not-matter.tgz"); err == nil {
		t.Fatal("installUpdate must refuse on a squashfs root")
	} else if err != errSquashfs {
		t.Fatalf("expected errSquashfs, got %v", err)
	}
	if err := restoreBackup(); err != errSquashfs {
		t.Fatalf("restoreBackup must refuse on squashfs, got %v", err)
	}
}

func TestB7_SquashfsRollbackBackstop(t *testing.T) {
	// FirmwareUpdate ON but squashfs: the rollback action itself must refuse.
	p := profSCLite
	withModel(t, p)
	oldMagic := fsMagicAt
	fsMagicAt = func(string) (int64, error) { return squashfsMagic, nil }
	t.Cleanup(func() { fsMagicAt = oldMagic })

	req := httptest.NewRequest("POST", "/api/update", strings.NewReader(`{"rollback":true}`))
	rec := httptest.NewRecorder()
	handleUpdate(rec, req)
	var m map[string]any
	json.Unmarshal(rec.Body.Bytes(), &m)
	if m["ok"] != false {
		t.Fatalf("rollback on squashfs should fail: %v", m)
	}
	if e, _ := m["err"].(string); !strings.Contains(e, "squashfs") {
		t.Fatalf("rollback refusal should mention squashfs: %q", e)
	}
}

// non-squashfs root (tests' usual temp dir) does not trip the backstop.
func TestB7_NonSquashfsNoBackstop(t *testing.T) {
	oldMagic := fsMagicAt
	fsMagicAt = func(string) (int64, error) { return 0xEF53, nil } // ext*
	t.Cleanup(func() { fsMagicAt = oldMagic })
	if rootIsSquashfs() {
		t.Fatal("ext root wrongly detected as squashfs")
	}
}

// --- settings export/import ---

// seedConfig writes a full set of config files into a temp dir and points the flags at them.
func seedConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// notify.json with a secret (telegram_token) and non-secret fields
	write("notify.json", `{"telegram_token":"SECRET-TG-123","telegram_chat":"42","telegram_on":true,"webhook_url":"https://hook.example/abcd","hot_c":82,"events":{"hot":true}}`)
	write("net.json", `{"port":8443}`)
	write("fan.json", `{"mode":"curve","points":[[40,30],[80,100]]}`)
	write("presets.json", `[{"name":"Low","mhz":525}]`)
	write("schedule.json", `{"on":true,"entries":[]}`)

	oldAuth, oldNet, oldFan, oldPreset, oldNotify, oldSched := *authFile, *netPath, *fanPath, *presetPath, notifyPath, schedPath
	*authFile = filepath.Join(dir, "auth.json")
	*netPath = filepath.Join(dir, "net.json")
	*fanPath = filepath.Join(dir, "fan.json")
	*presetPath = filepath.Join(dir, "presets.json")
	notifyPath = filepath.Join(dir, "notify.json")
	schedPath = filepath.Join(dir, "schedule.json")
	t.Cleanup(func() {
		*authFile, *netPath, *fanPath, *presetPath, notifyPath, schedPath = oldAuth, oldNet, oldFan, oldPreset, oldNotify, oldSched
	})
	return dir
}

func exportBundleFor(t *testing.T, secrets bool) settingsBundle {
	t.Helper()
	url := "/api/settings/export"
	if secrets {
		url += "?secrets=1"
	}
	req := httptest.NewRequest("GET", url, nil)
	rec := httptest.NewRecorder()
	handleSettingsExport(rec, req)
	var b settingsBundle
	if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil {
		t.Fatalf("export not valid JSON: %v (%s)", err, rec.Body.String())
	}
	return b
}

func TestB7_ExportExcludesSecretsByDefault(t *testing.T) {
	withModel(t, profSCLite)
	seedConfig(t)
	b := exportBundleFor(t, false)

	if b.Format != settingsFormat || b.Secrets {
		t.Fatalf("export header wrong: %+v", b)
	}
	// auth / tls never present
	for _, bad := range []string{"auth", "tls", "tls.key", "hashrate", "pools"} {
		if _, ok := b.Files[bad]; ok {
			t.Fatalf("export must not contain %q", bad)
		}
	}
	// notify present but secrets blanked, non-secrets kept
	nraw, ok := b.Files["notify"]
	if !ok {
		t.Fatal("notify section missing")
	}
	var nm map[string]any
	json.Unmarshal(nraw, &nm)
	if nm["telegram_token"] != "" {
		t.Fatalf("telegram_token should be blank, got %v", nm["telegram_token"])
	}
	if nm["webhook_url"] != "" {
		t.Fatalf("webhook_url should be blank, got %v", nm["webhook_url"])
	}
	if nm["telegram_chat"] != "42" {
		t.Fatalf("non-secret telegram_chat should survive, got %v", nm["telegram_chat"])
	}
	// other sections present
	for _, name := range []string{"net", "fan", "presets", "schedule"} {
		if _, ok := b.Files[name]; !ok {
			t.Fatalf("section %q missing from export", name)
		}
	}
}

func TestB7_ExportWithSecretsAndImportRoundTrip(t *testing.T) {
	withModel(t, profSCLite)
	dir := seedConfig(t)
	b := exportBundleFor(t, true)
	if !b.Secrets {
		t.Fatal("expected Secrets=true")
	}
	var nm map[string]any
	json.Unmarshal(b.Files["notify"], &nm)
	if nm["telegram_token"] != "SECRET-TG-123" {
		t.Fatalf("with-secrets export should carry the token, got %v", nm["telegram_token"])
	}

	// wipe every config file (simulating a fresh SD flash)
	for _, f := range []string{"notify.json", "net.json", "fan.json", "presets.json", "schedule.json"} {
		os.Remove(filepath.Join(dir, f))
	}

	// import the with-secrets bundle
	payload, _ := json.Marshal(b)
	req := httptest.NewRequest("POST", "/api/settings/import", strings.NewReader(string(payload)))
	rec := httptest.NewRecorder()
	handleSettingsImport(rec, req)
	var res map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("import response not JSON: %v (%s)", err, rec.Body.String())
	}
	if res["ok"] != true {
		t.Fatalf("import should succeed: %v", res)
	}
	// files restored with content
	got, err := os.ReadFile(filepath.Join(dir, "net.json"))
	if err != nil || !strings.Contains(string(got), "8443") {
		t.Fatalf("net.json not restored: %v %s", err, got)
	}
	ng, _ := os.ReadFile(filepath.Join(dir, "notify.json"))
	if !strings.Contains(string(ng), "SECRET-TG-123") {
		t.Fatalf("notify secret should be restored from a with-secrets bundle: %s", ng)
	}
}

func TestB7_ImportNoSecretsKeepsStoredSecret(t *testing.T) {
	withModel(t, profSCLite)
	dir := seedConfig(t)
	b := exportBundleFor(t, false) // stripped

	// the stored notify.json still has the real token; import must not wipe it
	payload, _ := json.Marshal(b)
	req := httptest.NewRequest("POST", "/api/settings/import", strings.NewReader(string(payload)))
	rec := httptest.NewRecorder()
	handleSettingsImport(rec, req)

	ng, _ := os.ReadFile(filepath.Join(dir, "notify.json"))
	if !strings.Contains(string(ng), "SECRET-TG-123") {
		t.Fatalf("a no-secrets import must keep the already-stored token: %s", ng)
	}
	if !strings.Contains(string(ng), `"telegram_chat":"42"`) {
		t.Fatalf("non-secret field should still be present: %s", ng)
	}
}

func TestB7_ImportRejectsModelMismatch(t *testing.T) {
	withModel(t, profSCLite)
	seedConfig(t)
	bundle := settingsBundle{
		Format: settingsFormat, Version: 1, Model: "scbox2", ModelName: "SC Box II",
		Files: map[string]json.RawMessage{"net": json.RawMessage(`{"port":9000}`)},
	}
	payload, _ := json.Marshal(bundle)
	req := httptest.NewRequest("POST", "/api/settings/import", strings.NewReader(string(payload)))
	rec := httptest.NewRecorder()
	handleSettingsImport(rec, req)
	var res map[string]any
	json.Unmarshal(rec.Body.Bytes(), &res)
	if res["ok"] != false {
		t.Fatalf("mismatched model import must be refused: %v", res)
	}
	if e, _ := res["err"].(string); !strings.Contains(e, "SC Box II") || !strings.Contains(e, "SC Lite") {
		t.Fatalf("error should name both models, got %q", e)
	}
}

func TestB7_ImportRejectsWrongFormat(t *testing.T) {
	withModel(t, profSCLite)
	seedConfig(t)
	req := httptest.NewRequest("POST", "/api/settings/import", strings.NewReader(`{"format":"something-else","files":{}}`))
	rec := httptest.NewRecorder()
	handleSettingsImport(rec, req)
	var res map[string]any
	json.Unmarshal(rec.Body.Bytes(), &res)
	if res["ok"] != false {
		t.Fatalf("wrong-format import must be refused: %v", res)
	}
}
