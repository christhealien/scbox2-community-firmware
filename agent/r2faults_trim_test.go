package main

// Round 2: auto-trim with several hand "+" chips on one board, and the tuner's configurable trim depth.

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// Three hand "+" chips on board 2: c3 +50 and c5 +25 make many errors while raised (c3 also at the board
// clock), c9 +12.5 is clean. Each window: at most one change on the board, a "+" chip only steps toward 0
// (never below it in that step), the clean "+" chip is never touched, a "-" hand trim is never touched.
func TestR2AutoTrimSeveralPlusChips(t *testing.T) {
	s := newCwSim(t, func(chip int, trim float64) float64 {
		switch {
		case chip == 3:
			return 25 // bad at any clock
		case chip == 5 && trim > 0:
			return 20
		}
		return 1
	})
	writeChipTrims([]chipTrim{{2, 3, 50}, {2, 5, 25}, {2, 9, 12.5}, {2, 30, -25}})
	on := true
	chipWatchSet(&on, false)
	prev := s.trims()
	minSeen := map[int]float64{3: 50, 5: 25}
	for w := 0; w < 14; w++ {
		s.run(2*time.Hour + time.Minute)
		cur := s.trims()
		n := 0
		for c := 1; c <= 46; c++ {
			if cur[c] != prev[c] {
				n++
				if prev[c] > 0 && (cur[c] < 0 || prev[c]-cur[c] != 12.5 && cur[c] != 0) {
					t.Fatalf("window %d: + chip c%d went %v -> %v", w, c, prev[c], cur[c])
				}
			}
		}
		if n > 1 {
			t.Fatalf("window %d: %d changes on one board: %v -> %v", w, n, prev, cur)
		}
		if cur[9] != 12.5 || cur[30] != -25 {
			t.Fatalf("window %d: a hand trim that isn't bad was touched: %v", w, cur)
		}
		for c := range minSeen {
			if cur[c] < minSeen[c] {
				minSeen[c] = cur[c]
			}
		}
		prev = cur
	}
	t.Logf("final %v; lowest seen c3 %v c5 %v", prev, minSeen[3], minSeen[5])
	var log []string
	for _, l := range chipWatchView()["log"].([]map[string]any) {
		log = append(log, l["msg"].(string))
	}
	if prev[5] != 0 {
		t.Fatalf("c5 should have been stepped back to the board clock: %v\n%s", prev, strings.Join(log, "\n"))
	}
	// c3 is bad at the board clock too: once its + is gone it's an ordinary untrimmed chip and auto-trim slows it.
	// Reported, not failed: whether "never below the board clock" should hold for a chip that WAS a hand + chip.
	if minSeen[3] < 0 {
		t.Logf("NOTE: former hand-+ chip c3 was slowed below the board clock to %v after reaching 0", minSeen[3])
	}
}

// The tuner's trim depth: with chip_trim_deep 25 the tuner stops at -25 (and says so), with 75 it can reach
// -75 and keep it, 37.5/62.5 accepted, off-grid / out of range refused.
func TestR2TunerTrimDepth(t *testing.T) {
	for _, tc := range []struct {
		deep      float64
		helpsAt   float64 // the chip is clean only from this trim
		wantTrim  float64 // kept (0: none)
		wantFloor string
	}{
		{25, -37.5, 0, "-25"},
		{25, -25, -25, ""},
		{50, -62.5, 0, "-50"},
		{75, -75, -75, ""},
		{75, -87.5, 0, "-75"},
		{0, -50, -50, ""}, // 0 = the default 50
	} {
		t.Run(fmt.Sprintf("deep%v_helps%v", tc.deep, tc.helpsAt), func(t *testing.T) {
			cfg := fastCfg()
			cfg.ChipTrim, cfg.ChipTrimMax, cfg.ChipTrimDeep = true, 3, tc.deep
			cfg.MinMV, cfg.MaxMV, cfg.MVStep = 8800, 9300, 100
			cfg.HoldMin = 2
			if err := validateTunerConfig(cfg); err != nil {
				t.Fatalf("config refused: %v", err)
			}
			e, s := newSimEngine(cfg, 9000, ckey{2, 5})
			trims := map[[2]int]float64{}
			lowest := 0.0
			e.trimGet = func() map[[2]int]float64 {
				m := map[[2]int]float64{}
				for k, v := range trims {
					m[k] = v
				}
				return m
			}
			e.trimSet = func(chain, chip int, mhz float64) error {
				if mhz == 0 {
					delete(trims, [2]int{chain, chip})
				} else {
					trims[[2]int{chain, chip}] = mhz
				}
				if mhz < lowest {
					lowest = mhz
				}
				s.mu.Lock()
				s.failBelowMV = 9000
				if trims[[2]int{2, 6}] <= tc.helpsAt+1e-9 {
					s.failBelowMV = 8800
				}
				s.mu.Unlock()
				return nil
			}
			e.hookStats = func() (chipStatsFile, bool) {
				var f chipStatsFile
				f.Chains = append(f.Chains, struct {
					Chain   int   `json:"chain"`
					Chips   int   `json:"chips"`
					Base10  int   `json:"base10"`
					Clock10 []int `json:"clock10"`
					Nonces  []int `json:"nonces"`
				}{Chain: 2, Chips: 46, Base10: 6250})
				return f, true
			}
			e.adoptTrims = func(ts []chipTrim, why string) {}
			e.Run()
			log := strings.Join(e.fullLog, "\n")
			floor := -tc.deep
			if tc.deep == 0 {
				floor = -50
			}
			if lowest < floor-1e-9 {
				t.Fatalf("trimmed to %v, deeper than %v\n%s", lowest, floor, log)
			}
			if got := trims[[2]int{2, 6}]; got != tc.wantTrim {
				t.Fatalf("kept %v, want %v (lowest %v)\n%s", got, tc.wantTrim, lowest, log)
			}
			if tc.wantFloor != "" && !strings.Contains(log, "is already at "+tc.wantFloor+" MHz") {
				t.Fatalf("no 'already at %s' in the log\n%s", tc.wantFloor, log)
			}
			if tc.wantFloor != "" && strings.Contains(log, "still failed at") && !strings.Contains(log, "still failed at "+strings.TrimPrefix(tc.wantFloor, "+")+" MHz") {
				t.Fatalf("trimFutile log names the wrong depth\n%s", log)
			}
		})
	}
	for _, d := range []float64{12.5, 30, 87.5, 100, -50} {
		cfg := fastCfg()
		cfg.ChipTrim, cfg.ChipTrimDeep = true, d
		if validateTunerConfig(cfg) == nil {
			t.Fatalf("chip_trim_deep %v accepted", d)
		}
	}
	for _, d := range []float64{37.5, 62.5} {
		cfg := fastCfg()
		cfg.ChipTrim, cfg.ChipTrimDeep = true, d
		if err := validateTunerConfig(cfg); err != nil {
			t.Fatalf("chip_trim_deep %v refused: %v", d, err)
		}
	}
}

// A tuner-kept -75 trim handed to auto-trim: the watchdog (floor -50) must not "slow" it further nor treat it
// as out of range; Undo removes it.
func TestR2AutoTrimAdoptsDeepTunerTrim(t *testing.T) {
	s := newCwSim(t, func(chip int, trim float64) float64 {
		if chip == 6 {
			return 30
		}
		return 1
	})
	writeChipTrims([]chipTrim{{2, 6, -75}})
	chipWatchAdopt([]chipTrim{{2, 6, -75}}, "tuner")
	on := true
	chipWatchSet(&on, false)
	s.run(6 * time.Hour)
	if tr := s.trims(); tr[6] != -75 {
		t.Fatalf("adopted -75 changed: %v", tr)
	}
	if _, err := chipWatchSet(nil, true); err != nil {
		t.Fatal(err)
	}
	if tr := s.trims(); tr[6] != 0 {
		t.Fatalf("undo left %v", tr)
	}
	_ = os.Remove
}
