package main

// B13: board/chip geometry & log parsing made model-aware.
//
// Proves:
//   - the SC Box II driver init line ("C%d: SCBOX2 Init sucessed. %d chips, ...", from R12/box2.str:61616)
//     parses to the reported chip count, and the Dashboard's Expected is the Box II 36;
//   - chipPartner uses the model's per-board chip count (18 pairs on the Box II, 23 on the SC Lite);
//   - setGeometryFromModel narrows the init regex to the active model's own tag (model.InitLogTag);
//   - SC Lite behaviour (46 chips, SCLITE tag) is unchanged.
//
// All of board/chip geometry is global package state (model, expectedChips, bhInitRe), so each test saves
// and restores it; these run sequentially with the rest of the package.

import (
	"testing"
	"time"
)

// b13Model sets the active model + geometry for the body of a test and restores the prior globals after.
func b13Model(t *testing.T, p modelProfile) {
	t.Helper()
	oldModel, oldChips, oldRe := model, expectedChips, bhInitRe
	t.Cleanup(func() { model, expectedChips, bhInitRe = oldModel, oldChips, oldRe })
	model = p
	setGeometryFromModel()
}

func resetBH() {
	bhMu.Lock()
	bhBoards = map[int]*bhBoard{}
	bhSeen = map[string]bhSeenRec{}
	bhMu.Unlock()
}

// scanOne parses the given log lines and returns the per-board view keyed by board id.
func scanOne(lines []string) map[int]boardHealth {
	scanBoardHealth(lines)
	got := map[int]boardHealth{}
	for _, h := range boardHealthView() {
		got[h.ID] = h
	}
	return got
}

// TestB13Box2InitLine: the Box II init line parses to 36 chips and the Dashboard shows Expected 36.
func TestB13Box2InitLine(t *testing.T) {
	b13Model(t, profSCBox2)
	resetBH()
	ts := time.Now().Add(-5 * time.Minute).Format("2006-01-02 15:04:05")
	now := time.Now().Format("2006-01-02 15:04:05")
	got := scanOne([]string{
		// exactly as intminer's Box II driver prints it (R12/box2.str:61616)
		" [" + ts + "] C0: SCBOX2 Init sucessed. 36 chips, 2880 Total goodcores. Wait 5s!!!",
		// Box II temp line carries no CpbTemp (R12/box2.str:61604); bhTempRe must still match
		" [" + now + "] C0: Chip Avgtemp 70.000000'C, MaxTemp 74.500000'C",
	})
	if got[0].Chips != 36 {
		t.Fatalf("Box II init: Chips = %d, want 36 (%+v)", got[0].Chips, got[0])
	}
	if got[0].Expected != 36 {
		t.Errorf("Box II Expected = %d, want 36 (from model.ChipsPerBoard)", got[0].Expected)
	}
	if got[0].MaxTemp != 74.5 {
		t.Errorf("Box II MaxTemp = %v, want 74.5 (bhTempRe must match the no-CpbTemp Box II line)", got[0].MaxTemp)
	}
}

// TestB13Box2ShortCount: a Box II board that comes up short still parses to its reported count (no minimum).
func TestB13Box2ShortCount(t *testing.T) {
	b13Model(t, profSCBox2)
	resetBH()
	ts := time.Now().Add(-5 * time.Minute).Format("2006-01-02 15:04:05")
	got := scanOne([]string{
		" [" + ts + "] C0: SCBOX2 Init sucessed. 34 chips, 2720 Total goodcores. Wait 5s!!!",
	})
	if got[0].Chips != 34 || got[0].Expected != 36 {
		t.Fatalf("short Box II board: Chips=%d Expected=%d, want 34/36", got[0].Chips, got[0].Expected)
	}
}

// TestB13InitTagNarrowing: after seeding a model, only that model's own init tag parses.
func TestB13InitTagNarrowing(t *testing.T) {
	ts := time.Now().Add(-5 * time.Minute).Format("2006-01-02 15:04:05")
	liteLine := " [" + ts + "] C0: SCLITE Init sucessed. 46 chips, 3680 Total goodcores."
	box2Line := " [" + ts + "] C0: SCBOX2 Init sucessed. 36 chips, 2880 Total goodcores. Wait 5s!!!"

	// Box II profile: the SC Lite line must NOT be taken as an init line (no SCLITE tag on this model).
	b13Model(t, profSCBox2)
	resetBH()
	got := scanOne([]string{liteLine})
	if _, seen := got[0]; seen {
		t.Errorf("Box II profile parsed a SCLITE init line: %+v", got[0])
	}
	resetBH()
	got = scanOne([]string{box2Line})
	if got[0].Chips != 36 {
		t.Errorf("Box II profile: SCBOX2 line Chips=%d, want 36", got[0].Chips)
	}
}

// TestB13ChipPartnerGeometry: partner pairing honours the model's per-board chip count.
func TestB13ChipPartnerGeometry(t *testing.T) {
	// SC Box II: 36 chips / 18 pairs; chip 37.. has no partner.
	func() {
		b13Model(t, profSCBox2)
		cases := map[int]int{1: 2, 2: 1, 35: 36, 36: 35, 37: 0, 46: 0, 0: 0}
		for c, want := range cases {
			if got := chipPartner(c); got != want {
				t.Errorf("Box II chipPartner(%d) = %d, want %d", c, got, want)
			}
		}
	}()
	// SC Lite: 46 chips / 23 pairs; chip 47.. has no partner (unchanged behaviour).
	b13Model(t, profSCLite)
	cases := map[int]int{1: 2, 45: 46, 46: 45, 47: 0, 0: 0}
	for c, want := range cases {
		if got := chipPartner(c); got != want {
			t.Errorf("SC Lite chipPartner(%d) = %d, want %d", c, got, want)
		}
	}
}

// TestB13ExpectedChipsSeed: expectedChips (the chip-trim input ceiling + Dashboard Expected) tracks the model.
func TestB13ExpectedChipsSeed(t *testing.T) {
	b13Model(t, profSCBox2)
	if expectedChips != 36 {
		t.Errorf("Box II expectedChips = %d, want 36", expectedChips)
	}
	b13Model(t, profSCLite)
	if expectedChips != 46 {
		t.Errorf("SC Lite expectedChips = %d, want 46", expectedChips)
	}
}

// TestB13SCLiteRegression: the SC Lite init/temp/event parsing is unchanged under its own profile.
func TestB13SCLiteRegression(t *testing.T) {
	b13Model(t, profSCLite)
	resetBH()
	ts := time.Now().Add(-5 * time.Minute).Format("2006-01-02 15:04:05")
	now := time.Now().Format("2006-01-02 15:04:05")
	got := scanOne([]string{
		" [" + ts + "] C0: SCLITE Init sucessed. 46 chips, 3680 Total goodcores.",
		" [" + ts + "] C1: SCLITE Init sucessed. 44 chips, 3500 Total goodcores.",
		" [" + now + "] C0: Chip Avgtemp 61.000000'C, MaxTemp 66.500000'C, CpbTemp 58.875000'C",
	})
	if got[0].Chips != 46 || got[0].Expected != 46 {
		t.Errorf("SC Lite board 0: Chips=%d Expected=%d, want 46/46", got[0].Chips, got[0].Expected)
	}
	if got[1].Chips != 44 {
		t.Errorf("SC Lite board 1 short count Chips=%d, want 44", got[1].Chips)
	}
	if got[0].MaxTemp != 66.5 {
		t.Errorf("SC Lite MaxTemp=%v, want 66.5", got[0].MaxTemp)
	}
}

// TestB13DefaultAcceptsBothTags: before any seeding (package default), both models' init tags parse, so the
// pre-profile boot window and un-seeded tests keep working.
func TestB13DefaultAcceptsBothTags(t *testing.T) {
	oldRe := bhInitRe
	t.Cleanup(func() { bhInitRe = oldRe })
	bhInitRe = defaultBHInitRe()
	resetBH()
	ts := time.Now().Add(-5 * time.Minute).Format("2006-01-02 15:04:05")
	got := scanOne([]string{
		" [" + ts + "] C0: SCLITE Init sucessed. 46 chips, 3680 Total goodcores.",
		" [" + ts + "] C1: SCBOX2 Init sucessed. 36 chips, 2880 Total goodcores. Wait 5s!!!",
	})
	if got[0].Chips != 46 || got[1].Chips != 36 {
		t.Errorf("default regex: board0=%d board1=%d, want 46/36", got[0].Chips, got[1].Chips)
	}
}
