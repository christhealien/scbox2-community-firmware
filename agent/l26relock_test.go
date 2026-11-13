package main

import (
	"os"
	"testing"
	"time"
)

// L26: the owner unlocked the clock limits, runs the boards at 700 MHz with a hand trim of +50 on b1.c5 (750 MHz), and
// starts "Find strong chips" on board 2. During the test the limits are re-locked (window 700): capPlusTrims skips
// the trims while the test runs, and the test's end writes its starting trims back, the +50 with them. Nothing checks
// them again: b1.c5 keeps running 750 MHz, past the 700 MHz window.
func TestL26RelockDuringStrongTest(t *testing.T) {
	f := sctSetup(t)
	f.base10 = 7000
	r7ClockStub(t, 700)
	writeChipTrims([]chipTrim{{0, 7, -25}, {1, 5, 50}, {2, 40, -12.5}})
	if err := os.WriteFile(chipHookFlag(), []byte("on\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := saveOverclock(overclockState{Acknowledged: true}); err != nil {
		t.Fatal(err)
	}
	if e := sctStart(2, 0, 0); e != "" {
		t.Fatal(e)
	}
	r2Ticks(f, 25*time.Minute)      // into step 1
	saveOverclock(overclockState{}) // re-locked: the window is 700 MHz again
	if hi := clockWindowHi(); hi != 700 {
		t.Fatalf("window %d", hi)
	}
	capPlusTrims() // what handleOverclock calls: skipped while the test runs
	sctRun(t, f, 6*time.Hour, nil)
	for _, tr := range readChipTrims() {
		if tr.MHz > 0 && 700+tr.MHz > float64(clockWindowHi())+1e-9 {
			t.Errorf("after the test b%d.c%d is trimmed %+g MHz: at the 700 MHz board clock it runs %g MHz, past the %d MHz window", tr.Chain, tr.Chip, tr.MHz, 700+tr.MHz, clockWindowHi())
		}
	}
	if tr := sctTrimsOf(0); tr[7] != -25 {
		t.Errorf("the owner's other trims changed: %v", readChipTrims())
	}
}
