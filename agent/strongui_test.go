package main

// r2-ui: findings from the browser sweep of "Find strong chips" (tuner-sweep/r2-ui).

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// The backend's per-chip error table stops answering mid-step while the hook keeps reporting: the test must not
// sit forever with chips raised and unjudged ("watching, 0:00 left"); it aborts like a silent hook does.
func TestStrongErrorsUnreadableAborts(t *testing.T) {
	f := sctSetup(t)
	before, _ := os.ReadFile(chipTrimPath())
	if e := sctStart(2, 0, 0); e != "" {
		t.Fatal(e)
	}
	down := false
	sctReadErrors = func() (map[ckey]int, error) {
		if down {
			return nil, errors.New("the miner's backend didn't answer")
		}
		return f.errors()
	}
	sctRun(t, f, 4*time.Hour, func() {
		if !down && sct.step == 2 && sct.counting {
			down = true
		}
	})
	after, _ := os.ReadFile(chipTrimPath())
	if !bytes.Equal(before, after) {
		t.Fatalf("trims not restored:\n%s", after)
	}
	if l := sctLast(t); l["how"] != "aborted" {
		t.Fatalf("how %v: %v", l["how"], l["message"])
	}
}

// A panel restart mid-test: the saved result must not show chips still "rising" (the card prints the state
// as the Why of a finished test).
func TestStrongInterruptedRowsNotRising(t *testing.T) {
	f := sctSetup(t)
	if e := sctStart(2, 0, 0); e != "" {
		t.Fatal(e)
	}
	for sct.step < 1 {
		f.advance(sctTick)
		sctTickOnce()
	}
	sct.mu.Lock()
	sct.reset() // the process dies
	sct.mu.Unlock()
	sctRecover()
	l := sctLast(t)
	if l["how"] != "interrupted" {
		t.Fatalf("how %v", l["how"])
	}
	for _, c := range l["chips"].([]any) {
		r := c.(map[string]any)
		if r["state"] == "rising" {
			t.Fatalf("row still rising after the test ended: %v", r)
		}
	}
}

// The window inputs take any number: 7.5 min is shown in the confirm, but the server ran 20 min (the JSON
// decode error was ignored and the default used).
func TestStrongAPIFractionalWindowRefused(t *testing.T) {
	sctSetup(t)
	w := httptest.NewRecorder()
	handleStrongTest(w, httptest.NewRequest("POST", "/api/strongtest", strings.NewReader(`{"start":true,"chain":2,"baseline_min":7.5,"step_min":20}`)))
	var j map[string]any
	json.Unmarshal(w.Body.Bytes(), &j)
	if j["ok"] == true {
		sct.mu.Lock()
		bw := sct.baseWin
		sct.mu.Unlock()
		t.Fatalf("7.5-min baseline accepted; the test runs a %v baseline", bw)
	}
}
