package main

// Upgrade path from v1.5.20/v1.5.21 with a chip self-test RUNNING (or with its files lying around): the owner's
// own trims come back from chiptrim.pretest, the old chiptest*.json files go, and the trims view and the chip
// report still work.

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type r7Env struct {
	dir      string
	reapply  int
	laterDur time.Duration
}

func r7Setup(t *testing.T) *r7Env {
	t.Helper()
	e := &r7Env{dir: t.TempDir()}
	oAuth, oStats, oLater, oWeb := *authFile, chipStatsPath, chipTestLater, *minerWeb
	*authFile = filepath.Join(e.dir, "auth.json")
	chipStatsPath = filepath.Join(e.dir, "sclite-chipstats.json")
	*minerWeb = "127.0.0.1:1" // the report's per-chip errors: nothing listens, an error is fine
	chipTestLater = func(d time.Duration, f func()) { e.laterDur = d; e.reapply++ }
	t.Cleanup(func() { *authFile, chipStatsPath, chipTestLater, *minerWeb = oAuth, oStats, oLater, oWeb })
	return e
}

// what v1.5.21 leaves in the data dir mid-test
func (e *r7Env) oldFiles(t *testing.T) []string {
	names := []string{"chiptest.json", "chiptest.partial", "chiptest-trim.json", "chiptest-pairs.json",
		"chiptest-scan.json", "chiptest-both.json", "chiptest-both.json.tmp", "chiptest.partial.tmp"}
	for i, n := range names {
		body := `{"verdict":"measuring","mode":"both","blocks":[{"on":true}]}`
		if i%3 == 2 {
			body = "{not json" // cut short by a power cut
		}
		if err := os.WriteFile(filepath.Join(e.dir, n), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return names
}

// the old test's own trims: -50 on chips 2, 6 … 46 of every board (the "trim" kind)
func r7TestTrims(chains ...int) []chipTrim {
	var ts []chipTrim
	for _, ch := range chains {
		for c := 2; c <= 46; c += 4 {
			ts = append(ts, chipTrim{ch, c, -50})
		}
	}
	return ts
}

func r7Owner() []chipTrim {
	return []chipTrim{{0, 7, -25}, {0, 12, 25}, {1, 3, -12.5}, {3, 46, 50}, {3, 45, -100}}
}

// the backup the old test wrote at its start: the trims file byte for byte
func r7WriteBackup(t *testing.T, ts []chipTrim) []byte {
	t.Helper()
	if err := writeChipTrims(ts); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(chipTrimPath())
	if err := os.WriteFile(chipTestBackup(), b, 0o644); err != nil {
		t.Fatal(err)
	}
	return b
}

func r7NoOldFiles(t *testing.T, e *r7Env) {
	t.Helper()
	left, _ := filepath.Glob(filepath.Join(e.dir, "chiptest*"))
	if len(left) > 0 {
		t.Fatalf("old self-test files left: %v", left)
	}
}

func r7Sorted(ts []chipTrim) []chipTrim {
	writeChipTrims(ts)
	return readChipTrims()
}

func TestR7UpgradeMidTestTrimsOnBlock(t *testing.T) {
	e := r7Setup(t)
	own := r7Sorted(r7Owner())
	saved := r7WriteBackup(t, own)
	writeChipTrims(r7TestTrims(0, 1, 2, 3)) // the test's -50 MHz block was on
	e.oldFiles(t)
	chipTestRecover()
	if got := readChipTrims(); !reflect.DeepEqual(got, own) {
		t.Fatalf("trims after the upgrade %v, want the owner's %v", got, own)
	}
	if b, _ := os.ReadFile(chipTrimPath()); string(b) != string(saved) {
		t.Fatalf("trims file not the backup byte for byte:\n%q\n%q", b, saved)
	}
	if _, err := os.Stat(chipTestBackup()); !os.IsNotExist(err) {
		t.Fatal("chiptrim.pretest still there: it would be put back again at the next start")
	}
	r7NoOldFiles(t, e)
	if e.reapply != 1 || e.laterDur != 90*time.Second {
		t.Fatalf("live re-apply scheduled %d times after %v, want once after 90s", e.reapply, e.laterDur)
	}
	// a second start (a reboot later) changes nothing
	writeChipTrims(append(own, chipTrim{2, 9, -12.5}))
	chipTestRecover()
	if got := readChipTrims(); len(got) != len(own)+1 || e.reapply != 1 {
		t.Fatalf("second start touched the trims: %v (re-apply %d)", got, e.reapply)
	}
}

func TestR7UpgradeMidTestTrimsOffBlock(t *testing.T) {
	e := r7Setup(t)
	own := r7Sorted(r7Owner())
	r7WriteBackup(t, own)
	writeChipTrims(nil) // an "off" block: the test had written an empty trims file
	e.oldFiles(t)
	chipTestRecover()
	if got := readChipTrims(); !reflect.DeepEqual(got, own) {
		t.Fatalf("trims %v, want %v", got, own)
	}
	r7NoOldFiles(t, e)
}

func TestR7UpgradeMidTestOwnerHadNoTrims(t *testing.T) {
	for _, name := range []string{"empty backup (no trims file before the test)", "header-only backup"} {
		t.Run(name, func(t *testing.T) {
			e := r7Setup(t)
			if strings.HasPrefix(name, "empty") {
				os.WriteFile(chipTestBackup(), nil, 0o644) // v1.5.21: os.WriteFile(chipTestBackup(), nil) when there was none
			} else {
				r7WriteBackup(t, nil)
			}
			writeChipTrims(r7TestTrims(0, 2))
			chipTestRecover()
			if got := readChipTrims(); len(got) != 0 {
				t.Fatalf("test trims survived the upgrade: %v", got)
			}
			if _, err := os.Stat(chipTestBackup()); !os.IsNotExist(err) {
				t.Fatal("backup left")
			}
			v := chipTrimView()
			if v["ok"] != true {
				t.Fatalf("trims view after an empty restore: %v", v)
			}
			_ = e
		})
	}
}

// v1.5.21 copied the trims to chiptrim.pretest and only then refused a "scan"/"both" start on a board with fewer
// than 46 chips: no test ran, the backup stayed, and the owner went on changing trims. The upgrade must not
// put those days-old trims back over the current ones. (Fails on the tree as it is: the stale backup wins.)
func TestR7UpgradeStaleBackupFromRefusedStart(t *testing.T) {
	e := r7Setup(t)
	r7WriteBackup(t, []chipTrim{{0, 7, -25}})
	now := r7Sorted([]chipTrim{{0, 7, -12.5}, {0, 30, 25}, {2, 11, -37.5}})
	chipTestRecover()
	if got := readChipTrims(); !reflect.DeepEqual(got, now) {
		t.Fatalf("the owner's current trims %v were replaced by a stale backup: %v", now, got)
	}
	if _, err := os.Stat(chipTestBackup()); !os.IsNotExist(err) {
		t.Fatal("stale backup left: it would be judged again at every start")
	}
	if e.reapply != 0 {
		t.Fatal("nothing changed, nothing to re-apply")
	}
}

func TestR7UpgradeNoBackupOnlyOldFiles(t *testing.T) {
	e := r7Setup(t)
	own := r7Sorted(r7Owner())
	e.oldFiles(t)
	os.Mkdir(filepath.Join(e.dir, "chiptest-dir"), 0o755) // not a file: skipped, nothing breaks
	chipTestRecover()
	if got := readChipTrims(); !reflect.DeepEqual(got, own) || e.reapply != 0 {
		t.Fatalf("trims %v (re-apply %d), want untouched %v", got, e.reapply, own)
	}
	if left, _ := filepath.Glob(filepath.Join(e.dir, "chiptest*.json*")); len(left) > 0 {
		t.Fatalf("left: %v", left)
	}
	// other panel files are not touched by the clean-up
	for _, keep := range []string{"chiptrim", "chiptrim.prestrong", "chipwatch.json", "chiphook"} {
		os.WriteFile(filepath.Join(e.dir, keep+".keep"), nil, 0o644)
	}
	chipTestRecover()
	for _, keep := range []string{"chiptrim", "chiptrim.prestrong", "chipwatch.json", "chiphook"} {
		if _, err := os.Stat(filepath.Join(e.dir, keep+".keep")); err != nil {
			t.Fatalf("%s removed", keep)
		}
	}
}

// the chip report and the trims view still work on a data dir that came from v1.5.21 mid-test
func TestR7ChipReportAfterUpgrade(t *testing.T) {
	e := r7Setup(t)
	own := r7Sorted(r7Owner())
	r7WriteBackup(t, own)
	writeChipTrims(r7TestTrims(0, 1))
	e.oldFiles(t)
	st := map[string]any{"t": time.Now().Unix(), "chains": []map[string]any{{"chain": 0, "chips": 46, "base10": 6250,
		"clock10": make([]int, 46), "nonces": make([]int, 46)}}}
	b, _ := json.Marshal(st)
	os.WriteFile(chipStatsPath, b, 0o644)
	chipTestRecover()

	w := httptest.NewRecorder()
	handleChipReport(w, httptest.NewRequest("GET", "/api/chipreport", nil))
	if w.Code != 200 {
		t.Fatalf("report: %d %s", w.Code, w.Body.String())
	}
	var rep map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &rep); err != nil {
		t.Fatalf("report not JSON: %v", err)
	}
	if rep["report"] != "sclite chip report" {
		t.Fatalf("report: %v", rep["report"])
	}
	for k := range rep {
		if strings.Contains(k, "test") {
			t.Fatalf("report still has a self-test field %q", k)
		}
	}
	tb, _ := json.Marshal(rep["trims"])
	var got []chipTrim
	json.Unmarshal(tb, &got)
	if !reflect.DeepEqual(got, own) {
		t.Fatalf("report trims %v, want the owner's %v", got, own)
	}
	if strings.Contains(w.Body.String(), "measuring") {
		t.Fatal("an old self-test result leaked into the report")
	}

	w = httptest.NewRecorder()
	handleChipTrim(w, httptest.NewRequest("GET", "/api/chiptrim", nil))
	var v map[string]any
	if json.Unmarshal(w.Body.Bytes(), &v) != nil || v["ok"] != true {
		t.Fatalf("trims view: %s", w.Body.String())
	}
}
