package main

import (
	"math"
	"path/filepath"
	"testing"
)

func TestPowerModel(t *testing.T) {
	*authFile = filepath.Join(t.TempDir(), "auth.json")
	powerMu.Lock()
	powerCfg, powerRead = powerConfig{}, false
	powerMu.Unlock()

	// the reference point of blake2b-asic-control's model
	if w := modelWatts(625, 9400); math.Abs(w-899) > 0.01 {
		t.Fatalf("625 MHz · PV 9400: %.2f W", w)
	}
	// chips follow clock × PV²; 7 % stays
	want := 899 * (0.07 + 0.93*(550.0/625)*math.Pow(9000.0/9400, 2))
	if w := modelWatts(550, 9000); math.Abs(w-want) > 0.01 {
		t.Fatalf("550 · 9000: %.2f, want %.2f", w, want)
	}
	// the chips run on the 12.5 MHz grid
	if modelWatts(630, 9400) != modelWatts(625, 9400) {
		t.Fatal("630 MHz should count as 625")
	}
	if modelWatts(0, 9400) != 0 {
		t.Fatal("no clock, no watts")
	}
	// calibration scales everything; mains only turns watts into amps
	c := powerConfig{Factor: 905.0 / 899, MainsV: 100}
	if w := c.watts(625, 9400); math.Abs(w-905) > 0.01 {
		t.Fatalf("calibrated: %.2f", w)
	}
	if (powerConfig{MainsV: 30}).mains() != 100 || (powerConfig{MainsV: 230}).mains() != 230 {
		t.Fatal("mains voltage")
	}
	if err := savePowerConfig(c); err != nil {
		t.Fatal(err)
	}
	powerMu.Lock()
	powerRead = false
	powerMu.Unlock()
	if got := getPowerConfig(); math.Abs(got.Factor-c.Factor) > 1e-9 {
		t.Fatal("calibration not kept")
	}
}

// An owner's wall meter at 500 MHz (calibrated at 9.32 V / 763 W): PV 8800 set, boards at 8.96 V, 710 W.
// The watts follow what the boards read, not the PV set below their floor.
func TestWattsFollowBoardVoltage(t *testing.T) {
	c := powerConfig{Factor: 763 / modelWatts(500, 9400)}
	d := dashboard{Boards: []board{{Alive: true, VoltageV: 8.96}, {Alive: true, VoltageV: 8.96}, {Alive: true, VoltageV: 8.8}, {Alive: true, VoltageV: 8.96}}}
	w := c.watts(500, effectivePV(8800, d))
	if math.Abs(w-710) > 10 {
		t.Fatalf("at the floor: %.0f W, the meter read 710", w)
	}
	if w0 := c.watts(500, 8800); w0 > 680 {
		t.Fatalf("the set PV alone should under-read (that's the bug): %.0f", w0)
	}
	// no reading: the setting, held at the floor
	if pv := effectivePV(8800, dashboard{}); pv != pvFloor {
		t.Fatalf("no reading: %v", pv)
	}
	// at stock the reading maps back to the setting
	if pv := effectivePV(9400, dashboard{Boards: []board{{Alive: true, VoltageV: 9.32}}}); math.Abs(pv-9400) > 1 {
		t.Fatalf("stock: %v", pv)
	}
}

// One board hashing (the other three empty or dead): its own share plus the fixed part, not the whole
// miner's watts. Seen: 1.058 TH/s at 500 MHz showed ≈ 732 W and 692 J/TH.
func TestWattsOneBoard(t *testing.T) {
	one, four := modelWattsN(500, 9440, 1), modelWattsN(500, 9440, 4)
	if one < 200 || one > 260 {
		t.Fatalf("one board at 500 MHz: %.0f W", one)
	}
	if math.Abs(four-modelWatts(500, 9440)) > 0.01 || four < 3*one {
		t.Fatalf("four boards %.0f vs one %.0f", four, one)
	}
	if jth := one / 1.058; jth < 190 || jth > 245 {
		t.Fatalf("J/TH %.0f", jth)
	}
}

// A one-second dip in the miner's voltage reading (8.80 V while the supply measured 9.09) doesn't show.
func TestVoltReadingSmoothed(t *testing.T) {
	for _, v := range []float64{9.01, 9.01, 8.80, 9.01, 9.02} {
		if got := smoothVolt(77, v); got < 9.0 {
			t.Fatalf("a dip showed: %v", got)
		}
	}
}
