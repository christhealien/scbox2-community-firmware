package main

// Settings import: the presets and schedule sections are decoded with the panel's own types before anything is
// written (a bad one refuses the whole import), and schedule changes that point at a preset that doesn't exist
// after the import are dropped, with a note saying so.

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func importBundle(t *testing.T, files map[string]string) map[string]any {
	t.Helper()
	b := settingsBundle{Format: settingsFormat, Version: 1, Model: model.ID, Files: map[string]json.RawMessage{}}
	for k, v := range files {
		b.Files[k] = json.RawMessage(v)
	}
	payload, _ := json.Marshal(b)
	rec := httptest.NewRecorder()
	handleSettingsImport(rec, httptest.NewRequest("POST", "/api/settings/import", strings.NewReader(string(payload))))
	var res map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("import reply %q: %v", rec.Body.String(), err)
	}
	return res
}

func TestImportRejectsBadPresetsOrSchedule(t *testing.T) {
	withModel(t, profSCLite)
	dir := seedConfig(t)
	read := func(n string) string { b, _ := os.ReadFile(filepath.Join(dir, n)); return string(b) }
	before := map[string]string{}
	for _, n := range []string{"net.json", "fan.json", "presets.json", "schedule.json", "notify.json"} {
		before[n] = read(n)
	}
	for _, c := range []struct {
		files map[string]string
		bad   string
	}{
		{map[string]string{"net": `{"port":9000}`, "presets": `[{"name":"Low","mhz":"525"}]`}, "presets"},
		{map[string]string{"fan": `{"mode":"curve"}`, "presets": `{"name":"Low"}`}, "presets"},
		{map[string]string{"presets": `[{"name":"Low","mhz":525}]`, "schedule": `{"rules":[{"start":"08:00","days":"mon","preset":"Low"}]}`}, "schedule"},
		{map[string]string{"schedule": `{"enabled":"yes"}`}, "schedule"},
	} {
		res := importBundle(t, c.files)
		if e, _ := res["err"].(string); res["ok"] != false || !strings.Contains(e, "the "+c.bad+" section") || !strings.Contains(e, "nothing was changed") {
			t.Fatalf("%v: %v", c.files, res)
		}
		for n, v := range before {
			if read(n) != v {
				t.Fatalf("a refused import changed %s: %q", n, read(n))
			}
		}
	}
	if len(loadPresets()) != 1 {
		t.Fatalf("presets after refused imports: %v", loadPresets())
	}
}

func TestImportDropsScheduleRulesForMissingPresets(t *testing.T) {
	withModel(t, profSCLite)
	seedConfig(t)
	res := importBundle(t, map[string]string{
		"presets": `[{"name":"Night","mhz":600,"mv":350,"pv":9300}]`,
		"schedule": `{"enabled":true,"base":"Gone","rules":[{"start":"08:00","days":[0,1],"preset":"Night"},
			{"start":"20:00","days":[0],"preset":"Missing"},{"start":"22:00","days":[2],"preset":"Idle"}]}`,
	})
	if res["ok"] != true {
		t.Fatalf("import: %v", res)
	}
	note, _ := res["note"].(string)
	if !strings.Contains(note, "dropped") || !strings.Contains(note, `"Missing"`) || !strings.Contains(note, `"Gone"`) || strings.Contains(note, `"Night"`) {
		t.Fatalf("note: %q", note)
	}
	s := loadSchedule()
	var got []string
	for _, r := range s.Rules {
		got = append(got, r.Preset)
	}
	if strings.Join(got, ",") != "Night,Idle" || s.Base != schedBack || !s.Enabled {
		t.Fatalf("schedule after import: rules %v base %q enabled %v", got, s.Base, s.Enabled)
	}
	if err := validateSchedule(&s); err != nil {
		t.Fatalf("the imported schedule doesn't pass the panel's own check: %v", err)
	}
	// a clean import says nothing about dropping
	res = importBundle(t, map[string]string{"schedule": `{"enabled":false,"rules":[{"start":"08:00","days":[0],"preset":"Night"}]}`})
	if n, _ := res["note"].(string); res["ok"] != true || strings.Contains(n, "dropped") {
		t.Fatalf("clean import: %v", res)
	}
}
