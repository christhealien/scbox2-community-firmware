package main

// Settings export / import (B7).
//
// The SC Box II installs by SD card only, and every SD flash wipes the writable config partition
// (/mnt/config). So the panel lets an owner download its own settings as one JSON file before flashing and
// upload it afterwards to put them back. It is a nice-to-have on the SC Lite too (a plain config backup),
// so the endpoints exist on every model; the UI shows them by model (see REPORT / B9 hooks).
//
// WHAT IS IN THE BUNDLE
//   - notify   notify.json    notifications: events, thresholds, channels. Secret fields (Telegram token,
//                             Discord/webhook URLs, ntfy/Gotify tokens) are EXCLUDED unless ?secrets=1.
//   - net      net.json       the panel's own port (and SSH on/off/port).
//   - fan      fan.json       the fan curve.
//   - presets  presets.json   saved presets (clock/fan presets).
//   - schedule schedule.json  the schedule.
//
// WHAT IS DELIBERATELY NOT IN THE BUNDLE
//   - auth.json            the password hash and the cookie-signing secret and the stock miner password.
//   - tls.key / tls.crt    the panel's TLS private key (and its cert; a fresh one is made on first start).
//   - hashrate.json        hashrate history (data, not a setting) and the tuner run files.
//   - pools                POOLS ARE NOT HERE. They live in the OEM's own miner config, not the panel's,
//                           so they are neither exported nor restored; re-enter them on the Pools tab.
//
// Mirrors the private-log-export philosophy: by default export carries NO secrets. The owner opts in with
// the "include secrets" choice (?secrets=1). On import, a secret left blank keeps whatever is already stored
// (so a no-secrets export round-trips without wiping an existing token).

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const settingsFormat = brandSettings
const settingsFormatVersion = 1

// notifySecretKeys are the notify.json fields treated as secrets (same set the Settings UI masks).
var notifySecretKeys = []string{"telegram_token", "discord_url", "ntfy_token", "gotify_token", "webhook_url"}

// settingsSection is one exportable config file.
type settingsSection struct {
	name   string                            // logical name in the bundle
	path   func() string                     // on-disk path (resolved at call time so tests can redirect the flags)
	secret bool                              // holds secret fields that are stripped unless the owner opts in
	strip  func([]byte) ([]byte, int, error) // nil unless secret; returns stripped bytes + count removed
}

func notifyFilePath() string {
	if notifyPath != "" {
		return notifyPath
	}
	return filepath.Join(filepath.Dir(*authFile), "notify.json")
}

// settingsSections lists the config files the export/import covers, in a stable order.
func settingsSections() []settingsSection {
	return []settingsSection{
		{name: "notify", path: notifyFilePath, secret: true, strip: stripNotifySecrets},
		{name: "net", path: func() string { return *netPath }},
		{name: "fan", path: func() string { return *fanPath }},
		{name: "presets", path: func() string { return *presetPath }},
		{name: "schedule", path: schedulePath},
	}
}

// settingsBundle is the on-disk export format.
type settingsBundle struct {
	Format    string                     `json:"format"`     // settingsFormat
	Version   int                        `json:"version"`    // settingsFormatVersion
	Panel     string                     `json:"panel"`      // panel firmware version
	Model     string                     `json:"model"`      // model.ID — import must match
	ModelName string                     `json:"model_name"` // model.Name (display)
	Exported  string                     `json:"exported"`   // RFC3339
	Secrets   bool                       `json:"secrets"`    // true: secret fields were included
	Files     map[string]json.RawMessage `json:"files"`      // section name -> raw config JSON
	Notes     []string                   `json:"notes,omitempty"`
}

// stripNotifySecrets blanks the secret fields in a notify.json blob, keeping every other field (and any
// unknown field) intact. Returns the new bytes and how many secrets were blanked.
func stripNotifySecrets(raw []byte) ([]byte, int, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return raw, 0, err
	}
	n := 0
	for _, k := range notifySecretKeys {
		if v, ok := m[k]; ok && string(v) != `""` && string(v) != "null" {
			m[k] = json.RawMessage(`""`)
			n++
		}
	}
	b, err := json.Marshal(m)
	if err != nil {
		return raw, 0, err
	}
	return b, n, nil
}

// netAccessKeys: the access switches in net.json (access.go). A settings file can't change them: allowing access
// from outside (or switching HTTPS only or a port) needs Settings -> Network, where switching outside access on
// asks for the password again. Model-independent (the SC Lite ignores these keys anyway).
var netAccessKeys = []string{"allow_outside", "https_only", "port80_off", "port443_off"}

// mergeNetImport: the imported net.json with the access switches as they are now.
func mergeNetImport(imported []byte) ([]byte, error) {
	var im map[string]json.RawMessage
	if err := json.Unmarshal(imported, &im); err != nil {
		return nil, err
	}
	if im == nil {
		return nil, fmt.Errorf("not an object")
	}
	cur := currentNetConfig()
	vals := map[string]bool{"allow_outside": cur.AllowOutside, "https_only": cur.HTTPSOnly,
		"port80_off": cur.Port80Off, "port443_off": cur.Port443Off}
	for _, k := range netAccessKeys {
		delete(im, k)
		if vals[k] {
			im[k] = json.RawMessage("true")
		}
	}
	return json.MarshalIndent(im, "", " ")
}

// mergeNotifyImport keeps a stored secret when the imported notify.json leaves that field blank, so a
// no-secrets export can be re-imported without erasing an existing token. Mirrors handleSettings' "blank =
// keep the stored one".
func mergeNotifyImport(imported []byte) ([]byte, error) {
	var im map[string]json.RawMessage
	if err := json.Unmarshal(imported, &im); err != nil {
		return nil, err
	}
	var cur map[string]json.RawMessage
	if b, err := os.ReadFile(notifyFilePath()); err == nil {
		_ = json.Unmarshal(b, &cur)
	}
	if cur != nil {
		for _, k := range notifySecretKeys {
			v, ok := im[k]
			blank := !ok || string(v) == `""` || string(v) == "null"
			if blank {
				if cv, ok := cur[k]; ok {
					im[k] = cv
				}
			}
		}
	}
	return json.Marshal(im)
}

// buildSettingsBundle gathers the config files into a bundle. secrets=false (the default) strips secret
// fields. A missing config file is simply left out (nothing to back up yet).
func buildSettingsBundle(secrets bool) (settingsBundle, int) {
	b := settingsBundle{
		Format:    settingsFormat,
		Version:   settingsFormatVersion,
		Panel:     firmwareVersion(),
		Model:     model.ID,
		ModelName: model.Name,
		Exported:  time.Now().UTC().Format(time.RFC3339),
		Secrets:   secrets,
		Files:     map[string]json.RawMessage{},
	}
	stripped := 0
	for _, s := range settingsSections() {
		raw, err := os.ReadFile(s.path())
		if err != nil || len(raw) == 0 || !json.Valid(raw) {
			continue
		}
		if s.secret && !secrets && s.strip != nil {
			if out, n, err := s.strip(raw); err == nil {
				raw, stripped = out, stripped+n
			}
		}
		b.Files[s.name] = json.RawMessage(raw)
	}
	b.Notes = []string{
		"Pools are stored in " + oemPoss() + " own miner config, not the panel — they are not in this file; re-enter them on the Pools tab.",
		"The password, the cookie secret and the TLS private key are never exported.",
	}
	if !secrets {
		b.Notes = append(b.Notes, "Notification secrets (tokens/webhooks) were left out; export again with 'include secrets' to carry them.")
	}
	return b, stripped
}

// handleSettingsExport: GET /api/settings/export[?secrets=1] — download the panel's settings as JSON.
func handleSettingsExport(w http.ResponseWriter, r *http.Request) {
	secrets := r.URL.Query().Get("secrets") == "1"
	b, _ := buildSettingsBundle(secrets)
	data, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		http.Error(w, "export failed: "+err.Error(), 500)
		return
	}
	name := minerLabel() + "-settings"
	if secrets {
		name += "-with-secrets"
	}
	serveBytes(w, name+".json", "application/json; charset=utf-8", data)
}

// handleSettingsImport: POST /api/settings/import — validate a bundle and restore the config files it holds.
// A bundle from a different model is refused (its figures may not be safe to write here).
func handleSettingsImport(w http.ResponseWriter, r *http.Request) {
	var b settingsBundle
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&b); err != nil {
		writeJSON(w, map[string]any{"ok": false, "err": "this doesn't look like a settings file: " + badJSON(err)})
		return
	}
	if b.Format != settingsFormat && b.Format != legacySettingsFormat {
		writeJSON(w, map[string]any{"ok": false, "err": "this isn't a panel settings file (wrong format)"})
		return
	}
	if b.Version > settingsFormatVersion {
		writeJSON(w, map[string]any{"ok": false, "err": fmt.Sprintf("this settings file is from a newer panel (format v%d); update the panel first", b.Version)})
		return
	}
	if b.Model != "" && b.Model != model.ID {
		from := b.ModelName
		if from == "" {
			from = b.Model
		}
		writeJSON(w, map[string]any{"ok": false, "err": fmt.Sprintf(
			"these settings were exported from a %s — this is a %s, so they can't be imported (settings only restore onto the same model)", from, model.Name)})
		return
	}
	if len(b.Files) == 0 {
		writeJSON(w, map[string]any{"ok": false, "err": "the settings file has nothing in it"})
		return
	}
	known := map[string]settingsSection{}
	for _, s := range settingsSections() {
		known[s.name] = s
	}
	// presets and schedule: decoded with the panel's own types before anything is written (a wrong field type
	// would make loadPresets return no presets at all, or loadSchedule a half-read schedule)
	if err := importCheckSections(b.Files); err != nil {
		writeJSON(w, map[string]any{"ok": false, "err": err.Error() + " — nothing was changed"})
		return
	}
	// the access switches are never set from a file (access.go): kept as they are (checked before anything is written)
	if raw, ok := b.Files["net"]; ok && json.Valid(raw) {
		merged, err := mergeNetImport(raw)
		if err != nil {
			writeJSON(w, map[string]any{"ok": false, "err": "the net section isn't valid (" + err.Error() + ") — nothing was changed"})
			return
		}
		b.Files["net"] = merged
	}
	restored := []string{}
	skipped := []string{}
	for name, raw := range b.Files {
		s, ok := known[name]
		if !ok {
			skipped = append(skipped, name) // a section this panel doesn't know (forward-compat)
			continue
		}
		if !json.Valid(raw) {
			writeJSON(w, map[string]any{"ok": false, "err": "the " + name + " section isn't valid JSON — nothing was changed"})
			return
		}
		out := []byte(raw)
		if name == "notify" { // keep an already-stored secret when the import left it blank
			if merged, err := mergeNotifyImport(out); err == nil {
				out = merged
			}
		}

		p := s.path()
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			writeJSON(w, map[string]any{"ok": false, "err": "could not prepare the config folder: " + err.Error()})
			return
		}
		// B4's FAT-safe write helper is used when present; this base has only writeAtomic (temp+fsync+rename).
		if err := writeAtomic(p, out, 0o600); err != nil {
			writeJSON(w, map[string]any{"ok": false, "err": "could not write " + name + ": " + err.Error()})
			return
		}
		restored = append(restored, name)
	}
	// Re-read the one config the panel keeps in memory; the rest are read from disk on demand.
	for _, name := range restored {
		if name == "notify" {
			loadNotifyConfig()
		}
	}
	note := "settings restored — pools aren't included, re-enter them on the Pools tab"
	for _, name := range restored {
		if name == "schedule" { // (after the presets: the rules are checked against the presets there now)
			if dropped := importDropMissingPresets(); dropped != "" {
				note += "; " + dropped
			}
		}
	}
	if secHardened {
		for _, n := range restored {
			if n == "net" {
				note += "; the access switches (access from outside, HTTPS only, ports 80 and 443) were kept as they are: change them under Settings → Network"
			}
		}
	}
	activity.emit(fmt.Sprintf("⬆ settings imported (%d section(s))", len(restored)))
	writeJSON(w, map[string]any{
		"ok":       true,
		"restored": restored,
		"skipped":  skipped,
		"note":     note,
	})
}

// importCheckSections: the presets and schedule sections must decode into the types the Presets and Schedule
// tabs read them with (loadPresets: []preset; loadSchedule: schedule). An error names the bad section.
func importCheckSections(files map[string]json.RawMessage) error {
	if raw, ok := files["presets"]; ok {
		var p []preset
		if err := json.Unmarshal(raw, &p); err != nil {
			return fmt.Errorf("the presets section can't be read as presets (%s)", badJSON(err))
		}
	}
	if raw, ok := files["schedule"]; ok {
		var sc schedule
		if err := json.Unmarshal(raw, &sc); err != nil {
			return fmt.Errorf("the schedule section can't be read as a schedule (%s)", badJSON(err))
		}
	}
	return nil
}

// importDropMissingPresets: the imported schedule's changes that switch to a preset that doesn't exist here
// (after the import) are dropped, and a "rest of the time" preset that doesn't exist goes back to "back", so the
// schedule doesn't fail every minute. Returns what was dropped, in words ("" = nothing).
func importDropMissingPresets() string {
	var gone []string
	_ = updateSchedule(func(sc *schedule) {
		kept := sc.Rules[:0]
		for _, r := range sc.Rules {
			if presetExists(r.Preset) {
				kept = append(kept, r)
				continue
			}
			gone = append(gone, fmt.Sprintf("%s → %q", r.Start, r.Preset))
		}
		sc.Rules = kept
		if !presetExists(sc.Base) {
			gone = append(gone, fmt.Sprintf("the rest of the time → %q", sc.Base))
			sc.Base = schedBack
		}
		if sc.Enabled && len(sc.Rules) == 0 {
			sc.Enabled = false
		}
	})
	if len(gone) == 0 {
		return ""
	}
	return fmt.Sprintf("%d schedule change(s) dropped because their preset doesn't exist on this miner: %s", len(gone), strings.Join(gone, ", "))
}
