package main

import (
	"archive/zip"
	"bytes"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// L25: after a panel restart (an update, a crash, the reboot the stuck-run message asks for) there's no run in
// memory: "For sending, private details out (.zip)" answered 404 while every other download works from the saved
// files. It's built from the saved files now, with the private details taken out.
func TestL25TunerShareableZipAfterPanelRestart(t *testing.T) {
	oldA, oldT := *authFile, *tunerDir
	dir := t.TempDir()
	*authFile, *tunerDir = filepath.Join(dir, "auth.json"), filepath.Join(dir, "tuner")
	defer func() { *authFile, *tunerDir = oldA, oldT }()
	os.WriteFile(privateSeenPath(), []byte("sc.mypool.example\n"), 0o600)
	tunerMu.Lock()
	oldCur := tunerCur
	tunerCur = nil
	tunerMu.Unlock()
	defer func() { tunerMu.Lock(); tunerCur = oldCur; tunerMu.Unlock() }()
	saveTunerRun(tunerBundle{cfg: defaultTunerConfig(), started: time.Now().Add(-time.Hour), finished: time.Now(), mode: "map",
		fullLog: []string{"10:00:00  the miner stays on Idle (mining is held on Idle by the pool guard (a pool that isn't in the Pools tab kept coming back: stratum+tcp://other.pool.example:4444, redirected from stratum+tcp://sc.mypool.example:3333): check the Pools tab, then Resume there): nothing is put back over it"},
		results: []testResult{{Time: "10:30:00", MHz: 575, Result: "clean"}}})
	w := httptest.NewRecorder()
	handleTunerExport(w, httptest.NewRequest("GET", "/api/tuner/export?kind=zippriv", nil))
	if w.Code != 200 {
		t.Fatalf("kind=zippriv after a panel restart: HTTP %d %q (the run's files are on disk)", w.Code, strings.TrimSpace(w.Body.String()))
	}
	zr, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	var all strings.Builder
	for _, f := range zr.File {
		if strings.Contains(f.Name, minerLabel()) {
			t.Errorf("zip path names the miner: %s", f.Name)
		}
		rc, _ := f.Open()
		b, _ := io.ReadAll(rc)
		rc.Close()
		all.Write(b)
	}
	out := all.String()
	for _, bad := range []string{"sc.mypool.example", "other.pool.example", "# miner: " + minerLabel()} {
		if strings.Contains(out, bad) {
			t.Errorf("still holds %q:\n%s", bad, out)
		}
	}
	if !strings.Contains(out, "575") || !strings.Contains(out, "the miner stays on Idle") {
		t.Errorf("files missing:\n%s", out)
	}
}
