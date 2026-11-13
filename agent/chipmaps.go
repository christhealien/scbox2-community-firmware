package main

// Tuner chip maps for earlier tests: the chip map's "Showing" menu (Live, or any finished test). Each
// finished test's per-chip counts are kept by the engine and saved to <tuner folder>/chipmap.json, so
// they can be looked at again after the run ends or the panel restarts.

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

type chipMapFile struct {
	Started string        `json:"started"`
	Tests   []testChipMap `json:"tests"`
}

func chipMapPath() string { return filepath.Join(tunerRunDir(), "chipmap.json") }

func saveChipMaps(started time.Time, maps []testChipMap) {
	b, err := json.Marshal(chipMapFile{Started: started.Format("2006-01-02 15:04"), Tests: maps})
	if err != nil {
		return
	}
	dir := tunerRunDir()
	if os.MkdirAll(dir, 0o755) != nil {
		return
	}
	tmp := chipMapPath() + ".tmp"
	if os.WriteFile(tmp, b, 0o644) == nil {
		os.Rename(tmp, chipMapPath())
	}
}

// currentChipMaps: the engine's (this run) or, with no run since the panel started, the saved file.
func currentChipMaps() chipMapFile {
	tunerMu.Lock()
	t := tunerCur
	tunerMu.Unlock()
	if t != nil {
		st, maps := t.chipMaps()
		return chipMapFile{Started: st.Format("2006-01-02 15:04"), Tests: maps}
	}
	var f chipMapFile
	if b, err := os.ReadFile(chipMapPath()); err == nil {
		json.Unmarshal(b, &f)
	}
	return f
}

// GET /api/tuner/chipmap: the list of finished tests (no chips); ?test=N: that test with its chips.
func handleTunerChipMap(w http.ResponseWriter, r *http.Request) {
	if !featGate(w, model.Feat.Tuner, "the auto-tuner") {
		return
	}
	f := currentChipMaps()
	if q := r.URL.Query().Get("test"); q != "" {
		n, err := strconv.Atoi(q)
		if err != nil || n < 0 || n >= len(f.Tests) {
			writeJSON(w, map[string]any{"ok": false, "err": "no such test"})
			return
		}
		writeJSON(w, map[string]any{"ok": true, "test": f.Tests[n]})
		return
	}
	list := make([]testChipMap, len(f.Tests))
	for i, t := range f.Tests {
		t.Chips = nil
		list[i] = t
	}
	writeJSON(w, map[string]any{"ok": true, "started": f.Started, "tests": list})
}
