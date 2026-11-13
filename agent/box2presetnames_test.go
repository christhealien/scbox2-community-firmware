package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A v0.4.x preset named like a v0.5.0 stock mode ("Stock High") is renamed once at start-up, and the schedule that
// named it follows; other presets, the SC Lite and a second start are left alone.
func TestBox2RenameReservedPresets(t *testing.T) {
	withModel(t, profSCBox2)
	dir := seedConfig(t)
	os.WriteFile(filepath.Join(dir, "presets.json"), []byte(`[{"name":"Stock High","mhz":600,"mv":44},{"name":"stock low","mhz":550,"mv":50},{"name":"Stock High (mine)","mhz":610,"mv":44},{"name":"Night","mhz":525,"mv":61}]`), 0o600)
	os.WriteFile(filepath.Join(dir, "schedule.json"), []byte(`{"enabled":true,"base":"stock low","rules":[{"start":"08:00","days":[0],"preset":"Stock High"},{"start":"22:00","days":[0],"preset":"Night"}]}`), 0o600)
	box2RenameReservedPresets()
	var names []string
	for _, p := range loadPresets() {
		names = append(names, p.Name)
	}
	if got := strings.Join(names, "|"); got != "Stock High (mine 2)|stock low (mine)|Stock High (mine)|Night" {
		t.Fatalf("presets after rename: %s", got)
	}
	sc := loadSchedule()
	if sc.Base != "stock low (mine)" || sc.Rules[0].Preset != "Stock High (mine 2)" || sc.Rules[1].Preset != "Night" {
		t.Fatalf("schedule after rename: base %q rules %+v", sc.Base, sc.Rules)
	}
	box2RenameReservedPresets() // a second start changes nothing
	if n := len(loadPresets()); n != 4 {
		t.Fatalf("second start: %d presets", n)
	}
	// the SC Lite: no rename
	withModel(t, profSCLite)
	os.WriteFile(filepath.Join(dir, "presets.json"), []byte(`[{"name":"Stock High","mhz":600}]`), 0o600)
	box2RenameReservedPresets()
	if p := loadPresets(); len(p) != 1 || p[0].Name != "Stock High" {
		t.Fatalf("SC Lite renamed: %+v", p)
	}
}
