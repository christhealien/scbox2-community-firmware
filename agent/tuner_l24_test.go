package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// l24Stock: a stock backend whose running setting is manual (Controls) and whose firmware level is stock.
func l24Stock(t *testing.T, manual string) {
	t.Helper()
	setting := map[string]any{"manual": true, "tempcontrol": true, "select": float64(0),
		"manualPowerplan": manual,
		"powerplans": []any{map[string]any{"info": "615 MHz 9100 V 40 RPM 40 RPM PV 9400", "level": float64(0)},
			map[string]any{"info": "0 MHz 0 V 100 RPM 100 RPM", "level": float64(3)}}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/user/login"):
			json.NewEncoder(w).Encode(map[string]any{"JWT Token": "tok"})
		case r.Method == "GET":
			json.NewEncoder(w).Encode(setting)
		default:
			io.Copy(io.Discard, r.Body)
			io.WriteString(w, `{"status":0}`)
		}
	}))
	t.Cleanup(srv.Close)
	old := *minerWeb
	*minerWeb = strings.TrimPrefix(srv.URL, "http://")
	t.Cleanup(func() { *minerWeb = old })
	jwtToken = ""
	authMu.Lock()
	auth.MinerPass = "pw"
	authMu.Unlock()
	stockCacheMu.Lock()
	stockCache = powerplan{}
	stockCacheMu.Unlock()
}

// Baseline "current": the miner runs 625 MHz at PV 9500 (run voltage 9100, set in Controls with "keep run
// voltage at PV − 300" off). The run's baseline must be what the miner runs, not run voltage + gap (PV 9400):
// a baseline 100 mV under the owner's setting learns inflated error rates (every limit opens) and is
// labelled as the miner's current setting.
func TestL24BaselineCurrentKeepsRealPV(t *testing.T) {
	l24Stock(t, "625 MHz 9100 V 50 RPM 50 RPM PV 9500")
	c := defaultTunerConfig()
	c.BaselineSrc = "current"
	e := newTunerEngine(c)
	got := e.resolveBaseline()
	if got != (planMV{MHz: 625, MV: 9100, PV: 9500}) {
		t.Fatalf("baseline current = %s, the miner runs 625 MHz / PV 9500 (mV 9100)", got.text())
	}
}

// Baseline "stock" with a PV gap other than the stock 300: the stock plan is PV 9400; gap 250 made it 9350.
func TestL24BaselineStockKeepsRealPV(t *testing.T) {
	l24Stock(t, "625 MHz 9100 V 50 RPM 50 RPM PV 9500")
	c := defaultTunerConfig()
	c.BaselineSrc, c.PVGap = "stock", 250
	e := newTunerEngine(c)
	got := e.resolveBaseline()
	if got != (planMV{MHz: 615, MV: 9100, PV: 9400}) {
		t.Fatalf("baseline stock = %s, the stock plan is 615 MHz / PV 9400 (mV 9100)", got.text())
	}
}

// A current plan outside the supply's range (an old PV over 9600) is not used as is: the gap rule, clamped.
func TestL24BaselineCurrentOutOfRangePV(t *testing.T) {
	l24Stock(t, "625 MHz 9300 V 50 RPM 50 RPM PV 9700")
	c := defaultTunerConfig()
	c.BaselineSrc = "current"
	e := newTunerEngine(c)
	got := e.resolveBaseline()
	if got.PV > pvHardMax || got.PV < pvHardMin {
		t.Fatalf("baseline PV %d outside %d–%d", got.PV, pvHardMin, pvHardMax)
	}
}

// Locked clock window (300–700) and a 62.5 MHz step: the first clock rounded down to the step grid is 250 MHz,
// under the lowest clock the owner may set (and under the Lowest asked). Validation must not pass a plan
// whose clocks leave the window.
func TestL24ClockListUnderLockedMinimum(t *testing.T) {
	c := defaultTunerConfig()
	c.DryRun = true
	c.LowestMHz, c.HighestMHz, c.Step = 300, 500, 62.5
	cl := c.clockList()
	if err := validateTunerConfig(c); err == nil && len(cl) > 0 && cl[0] < freqMin && !loadOverclock().Acknowledged {
		t.Fatalf("clocks %v accepted: %d MHz is under the locked minimum %d", cl, cl[0], freqMin)
	}
}

// "Confirm the winner with one more, longer test" for 0 × the test length (API) or 1 minute (the form sends
// confirm_x = minutes / hold): the confirm ends at its first reading, "confirms" a setting that fails any real
// confirm, leaves the miner on it and saves it as the curve's confirmed point (a rebuild's High).
func TestL24ConfirmTooShortRefused(t *testing.T) {
	for _, x := range []float64{0, -1, 0.04} {
		c := small()
		c.ConfirmX = x
		if validateTunerConfig(c) == nil {
			// what such a run does: the top clock is clean for one 25-minute test (+3 settling), its chip b1.c7 then goes bad
			if x != 0 {
				t.Errorf("confirm_x %g accepted", x)
				continue
			}
			c.TopDown = false
			e, f := newFM(t, c)
			f.rules = []fmRule{func(f *fakeMiner, k ckey) float64 {
				if k == (ckey{1, 7}) && f.plan.MHz == 625 && f.sinceApply() > 29 {
					return 1
				}
				return 0
			}}
			runFM(t, e, f)
			e.mu.Lock()
			best, confirmed := e.best, e.confirmed
			e.mu.Unlock()
			t.Fatalf("confirm_x 0 accepted: the run confirmed %v (confirmed=%v) in a %.1f-min confirm", best, confirmed,
				e.results[len(e.results)-1].MinS)
		}
	}
}

// A poll longer than the watchdog's limit: each poll sleeps that long without a sign of life, so the watchdog
// ends a healthy run as stuck (and the heat abort is checked only once a poll).
func TestL24PollOverWatchdogRefused(t *testing.T) {
	c := defaultTunerConfig()
	c.PollS = tunerStuckAfter.Seconds() + 60
	if validateTunerConfig(c) == nil {
		t.Fatalf("poll_s %g accepted: longer than the watchdog's %v", c.PollS, tunerStuckAfter)
	}
}
