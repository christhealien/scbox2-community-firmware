package main

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// L25: "For sending, private details out (.zip)" (tuner-shareable.zip) writes the run's log lines as they are.
// A run the pool guard ends logs its hold text, which names the pool (and its account, or the owner's own
// pool it was redirected from): those go out verbatim in the "private" zip.
func TestL25TunerShareableZipRedactsLog(t *testing.T) {
	oldA, oldT := *authFile, *tunerDir
	dir := t.TempDir()
	*authFile, *tunerDir = filepath.Join(dir, "auth.json"), filepath.Join(dir, "tuner")
	defer func() { *authFile, *tunerDir = oldA, oldT }()
	// the owner's pool, as remembered for the private exports (the Pools tab / pool guard remember it)
	os.WriteFile(privateSeenPath(), []byte("sc.mypool.example\nbc1qownerwalletxxxxxxxxxxxxxxxxxxxxxxxx.rig7\nbc1qownerwalletxxxxxxxxxxxxxxxxxxxxxxxx\nrig7\n"), 0o600)

	var lines []string
	for _, what := range []string{
		"stratum+tcp://203.0.113.9:3333 (account bc1qownerwalletxxxxxxxxxxxxxxxxxxxxxxxx.rig7)",
		"stratum+tcp://other.pool.example:4444, redirected from stratum+tcp://sc.mypool.example:3333",
	} {
		os.Remove(poolHoldPath())
		poolHoldSet(what)
		// as the tuner logs it at its end (tuner.go: "the miner stays on Idle (%v): nothing is put back over it")
		lines = append(lines, "10:00:00  "+fmt.Sprintf("the miner stays on Idle (%v): nothing is put back over it", poolHoldErr()))
	}
	os.Remove(poolHoldPath())
	e := &tunerEngine{cfg: defaultTunerConfig(), startedAt: time.Now().Add(-time.Hour), fullLog: lines}
	tunerMu.Lock()
	oldCur := tunerCur
	tunerCur = e
	tunerMu.Unlock()
	defer func() { tunerMu.Lock(); tunerCur = oldCur; tunerMu.Unlock() }()

	w := httptest.NewRecorder()
	handleTunerExport(w, httptest.NewRequest("GET", "/api/tuner/export?kind=zippriv", nil))
	if w.Code != 200 {
		t.Fatalf("HTTP %d %s", w.Code, w.Body.String())
	}
	zr, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	var all strings.Builder
	for _, f := range zr.File {
		rc, _ := f.Open()
		b, _ := io.ReadAll(rc)
		rc.Close()
		all.Write(b)
	}
	out := all.String()
	for _, bad := range []string{"203.0.113.9", "bc1qownerwallet", "rig7", "sc.mypool.example", "other.pool.example"} {
		if strings.Contains(out, bad) {
			t.Errorf("tuner-shareable.zip still holds %q", bad)
		}
	}
	if !strings.Contains(out, "the miner stays on Idle") {
		t.Errorf("too much taken out:\n%s", out)
	}
	if t.Failed() {
		t.Logf("zip contents:\n%s", out)
	}
}
