package main

// The chip self-tests (v1.4.2 – v1.5.20) proved that a trim reaches the chip it names and how the chips are
// powered (pairs 1+2, 3+4 … share a power stage; see chipPartner). With that settled they were removed in
// v1.5.21. What's left: a test cut short by the update that removed them saved the owner's trims in a backup;
// those are put back at start, and the old result files are cleaned up.

import (
	"os"
	"path/filepath"
	"time"
)

func chipTestBackup() string { return filepath.Join(filepath.Dir(*authFile), "chiptrim.pretest") }

// chipTestLater: the live re-apply once mining runs again (tests replace it)
var chipTestLater = func(d time.Duration, f func()) { go func() { time.Sleep(d); f() }() }

// chipTestTrimsLook: the trims file holds what a running self-test writes: nothing (a trims-off block) or only
// its -50 MHz test trims (the only depth since v1.5.7). While a test ran, nothing else could write the file
// (hand edits, the tuner, auto-trim and the strong-chip test were all refused or paused).
func chipTestTrimsLook(ts []chipTrim) bool {
	for _, t := range ts {
		if t.MHz != -50 {
			return false
		}
	}
	return true
}

func chipTestRecover() {
	dir := filepath.Dir(*authFile)
	// chiptest.json, .partial, chiptest-<kind>.json and the .tmp a power cut can leave from their atomic writes
	old, _ := filepath.Glob(filepath.Join(dir, "chiptest*"))
	for _, f := range old {
		os.Remove(f)
	}
	b, err := os.ReadFile(chipTestBackup())
	if err != nil {
		return
	}
	chipTrimMu.Lock()
	// v1.5.20 wrote the backup before it refused some starts ("needs a full board of 46 chips"): such a backup
	// is left over with no test run, and the owner may have changed their trims since. Only trims a test wrote
	// are replaced by it.
	running := chipTestTrimsLook(readChipTrims())
	if running {
		err = writeAtomic(chipTrimPath(), b, 0o644)
	}
	if err == nil {
		os.Remove(chipTestBackup())
	}
	chipTrimMu.Unlock()
	switch {
	case !running:
		activity.emit("🔧 removed a chip self-test backup left over from a test that never ran: your trims stay as they are")
	case err != nil:
		activity.emit("🔧 a chip self-test was running when the panel restarted, and your own trims couldn't be put back (" + err.Error() + "): they're kept in chiptrim.pretest for the next start")
	default:
		activity.emit("🔧 a chip self-test was running when the panel restarted (the self-tests are no longer part of the firmware): your own trims are back")
		chipTestLater(90*time.Second, func() { reapplyTrims() })
	}
}
