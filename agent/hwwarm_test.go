package main

import (
	"testing"
	"time"
)

func hwWarmReset() {
	hwWarmUntil, hwWarmBase, hwWarmDone, hwWarmLast = time.Time{}, 0, 0, 0
}

func TestHWWarmUpSummarised(t *testing.T) {
	hwWarmReset()
	last := 3
	prev := dashboard{OK: true, Elapsed: 600, HWErrors: 3, HWPct: 0.1, Boards: []board{{ID: 0, Enabled: false}}}
	d := dashboard{OK: true, Elapsed: 610, HWErrors: 9, HWPct: 0.3, Boards: []board{{ID: 0, Enabled: true}}}
	hwWarmTick(prev, d, &last) // board enabled again: warm-up starts, nothing listed
	if last != 9 || hwWarmUntil.IsZero() {
		t.Fatalf("warm-up not started: last=%d until=%v", last, hwWarmUntil)
	}
	d2 := d
	d2.HWErrors = 20
	hwWarmTick(d, d2, &last)
	if last != 20 {
		t.Fatalf("errors inside the warm-up must not be listed: last=%d", last)
	}
	if w, left, _ := hwWarmView(20, 0.5); w != 17 || left == 0 {
		t.Fatalf("view during the warm-up: warm=%d left=%d", w, left)
	}
	hwWarmUntil = time.Now().Add(-time.Second) // the warm-up is over
	d3 := d2
	d3.HWErrors = 21
	hwWarmTick(d2, d3, &last)
	if last != 21 || !hwWarmUntil.IsZero() || hwWarmDone != 18 {
		t.Fatalf("warm-up must end with the summary: last=%d done=%d", last, hwWarmDone)
	}
	d4 := d3
	d4.HWErrors = 22
	hwWarmTick(d3, d4, &last) // after: listed as before (last stays for the caller to emit)
	if last != 21 {
		t.Fatalf("after the warm-up HW errors are listed again: last=%d", last)
	}
	// 22 errors in 10 000 nonces, 18 of them warm-up: 4 in 9 982
	if w, left, s := hwWarmView(22, 0.22); w != 18 || left != 0 || s < 0.039 || s > 0.041 {
		t.Fatalf("steady view: warm=%d left=%d steady=%v", w, left, s)
	}
}

// at boot the panel's first look is at a run a few seconds old: its errors are warm-up errors too
func TestHWWarmUpAtConnect(t *testing.T) {
	hwWarmReset()
	last := 0
	d := dashboard{OK: true, Elapsed: 8, HWErrors: 2, HWPct: 5}
	hwWarmConnect(d, &last)
	if hwWarmUntil.IsZero() || last != 2 {
		t.Fatalf("a young run must start a warm-up: until=%v last=%d", hwWarmUntil, last)
	}
	d2 := d
	d2.Elapsed, d2.HWErrors = 30, 24
	hwWarmTick(d, d2, &last)
	if last != 24 {
		t.Fatalf("listed during the boot warm-up: last=%d", last)
	}
	if w, _, _ := hwWarmView(24, 12); w != 24 {
		t.Fatalf("all of a young run's errors are warm-up errors: %d", w)
	}
	hwWarmReset()
	old := dashboard{OK: true, Elapsed: 4000, HWErrors: 30, HWPct: 0.1}
	hwWarmConnect(old, &last)
	if !hwWarmUntil.IsZero() {
		t.Fatal("an old run is not warming up")
	}
}
