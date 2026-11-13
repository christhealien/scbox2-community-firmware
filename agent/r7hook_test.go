package main

// r7-hook: agent side of the chip-trim contract and the hook's start guard.

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The panel's reading of hand-edited lines must match the hook's (native/harness3.c section 1/2):
// a line is a trim on the miner exactly when the panel shows it.
func TestR7TrimLinesPanelVsHook(t *testing.T) {
	*authFile = filepath.Join(t.TempDir(), "auth.json")
	cases := []struct {
		line string
		hook bool // what sclite_chip.c does with it (fixed hook: harness3.c)
	}{
		{"0 5 -50", true}, {"0\t5\t+50", true}, {"0 5 -37.5", true},
		{"0 5+50", false}, {"0 5-100", false}, {"0.9 5 -50", false}, {"0 5.9 -50", false},
		{"0 5 12.5000000001", false}, {"0 5 -50.00000000001", false},
	}
	for _, c := range cases {
		os.WriteFile(chipTrimPath(), []byte(c.line+"\n"), 0o644)
		shown := len(readChipTrims()) == 1
		if shown != c.hook {
			t.Errorf("%q: panel shows it %v, the hook applies it %v", c.line, shown, c.hook)
		}
	}
}

// A POST of an almost-grid value is stored as the grid value (the hook skips anything off the grid).
func TestR7HandTrimSnapped(t *testing.T) {
	*authFile = filepath.Join(t.TempDir(), "auth.json")
	oldClk := reapplySetClock // a "+" trim needs the board clock
	reapplySetClock = func() (int, bool) { return 600, true }
	defer func() { reapplySetClock = oldClk }()
	m := 12.5000000001
	chipTrimChange(nil, nil, 0, 6, &m, false, false)
	b, _ := os.ReadFile(chipTrimPath())
	if want := "0 6 12.5\n"; len(b) < len(want) || string(b[len(b)-len(want):]) != want {
		t.Fatalf("trims file:\n%s", b)
	}
}

// Pool down: the miner exits at every start (no nonces, the hook never vouches), the wrapper's 3 hook tries
// are used up and the guarded copy without trims runs. That is not "the copy with trims didn't get going":
// chip trims must stay on (the 4-stops rule has the same exception).
func TestR7HookTriesUsedUpWhilePoolDown(t *testing.T) {
	if os.Getenv("R7_PRIVATE_TMP") == "" {
		t.Skip("writes /tmp/sclite-hook.*: run in a private /tmp (unshare -m + tmpfs)")
	}
	*authFile = filepath.Join(t.TempDir(), "auth.json")
	os.WriteFile(chipHookFlag(), []byte("on\n"), 0o644)
	old := time.Now().Add(-3 * time.Minute)
	for _, m := range []string{"/tmp/sclite-hook.1", "/tmp/sclite-hook.2", "/tmp/sclite-hook.3"} {
		os.WriteFile(m, nil, 0o600)
		os.Chtimes(m, old, old)
	}
	fakeProc(t, "intminer") // running, no sclite_chip.so in its maps
	now := time.Now()
	poolDownAt.Store(now.Add(-time.Minute).Unix())
	t.Cleanup(func() { poolDownAt.Store(0) })
	mhMu.Lock()
	mhStops = nil
	mhMu.Unlock()
	chipHookGuard(now)
	if _, err := os.Stat(chipHookFlag()); err != nil {
		b, _ := os.ReadFile(chipHookFlag() + ".off")
		t.Fatalf("chip trims switched off during a pool outage: %s", b)
	}
	if _, err := os.Stat("/tmp/sclite-hook.3"); err == nil {
		t.Fatal("the hook's start tries were not given back: the next start runs without trims again")
	}
	// same tries used up with the pools fine: switched off, as before
	poolDownAt.Store(0)
	for _, m := range []string{"/tmp/sclite-hook.1", "/tmp/sclite-hook.2", "/tmp/sclite-hook.3"} {
		os.WriteFile(m, nil, 0o600)
		os.Chtimes(m, old, old)
	}
	chipHookGuard(now)
	if _, err := os.Stat(chipHookFlag()); err == nil {
		t.Fatal("3 failed starts with the pools up: trims should be switched off")
	}
}
