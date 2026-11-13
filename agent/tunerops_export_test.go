package main

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ops sweep: after the panel restarts (an update, a crash, the owner restarting it, or the reboot the stuck-run
// message leads to) there's no run in memory, and the export falls back to the files saved on disk. The
// single files work, but the zip (the "everything" download the UI offers, and the one an owner sends in a
// report) answers 404 "no tuner run in memory". Seen through the HTTP API after killing the panel mid-run.
func TestTunerExportZipAfterPanelRestart(t *testing.T) {
	old := *tunerDir
	*tunerDir = t.TempDir()
	defer func() { *tunerDir = old }()
	tunerMu.Lock()
	oldCur := tunerCur
	tunerCur = nil // a fresh panel: no run in memory
	tunerMu.Unlock()
	defer func() { tunerMu.Lock(); tunerCur = oldCur; tunerMu.Unlock() }()
	saveTunerRun(tunerBundle{cfg: defaultTunerConfig(), started: time.Now().Add(-time.Hour), finished: time.Now(), mode: "map",
		fullLog: []string{"10:00:00  BASELINE ..."}, results: []testResult{{Time: "10:30:00", MHz: 575, Result: "clean"}}})
	for _, kind := range []string{"log", "results", "zip"} {
		w := httptest.NewRecorder()
		handleTunerExport(w, httptest.NewRequest("GET", "/api/tuner/export?kind="+kind, nil))
		if w.Code != 200 {
			t.Errorf("kind=%s after a panel restart: HTTP %d %q (the run's files are on disk)", kind, w.Code, strings.TrimSpace(w.Body.String()))
		}
	}
}
