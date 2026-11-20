package main

// Power estimate on the planBox2V model (the SC Box II), commission key "PowerEstimate" (test C2). Only reached
// when model.Plan == planBox2V AND model.Feat.PowerEstimate is on (powerView / handlePower gate first); with the
// gate off nothing here runs and no file is read or written.
//
// The SC Lite clock×PV² model (power.go) does not transfer: different chips (36 ICT580 on one board), a different
// power stage, and the Box II's voltage is set as one of the OEM's levels. So the Box II has its own figures:
//
//   - The running setting is what the miner itself reports (cgminer API devs: "clock" and "voltage" = the level),
//     so it is right whichever way the setting was made (stock plan via minerd, or a later pgaset).
//   - A setting with the owner's own wall-meter reading shows that reading (fans and PSU included): exact.
//   - Otherwise an ESTIMATE, marked ≈, from wall-meter readings taken on one unit (C2/C7, docs/HARDWARE.md):
//       690 MHz (687.5 on the chips) · 7.67 V  392 W    ~1.97 TH/s   (Stock High; the OEM states 400 W)
//       525 MHz · 7.03 V                        251.8 W  ~1.51 TH/s   (Stock Low;  the OEM states 260 W)
//       700 MHz · 7.67 V                        399.4 W  ~2.01 TH/s
//       720 MHz (712.5 on the chips) · 7.67 V   406.9 W  ~2.04 TH/s
//       725 MHz · 7.67 V                        413.5 W  ~1.98 TH/s   (HW errors rising)
//     At one of these settings its own reading is shown; anywhere else in the window (525–750 MHz, the levels
//     7.67–7.03 V) the curve through them: W = k × f × V², with f the clock the chips run (box2PLLClock) and V the
//     level's volts (box2Volts), k fitted at Stock High (392 W at 687.5 MHz, 7.67 V). That one-constant curve
//     goes through all five readings within 0.7 W (Stock Low: 251.5 W predicted for 251.8 W measured), so the
//     wall power of this miner follows the chips' dynamic power, with no fixed part worth fitting. Past the
//     readings (737.5/750 MHz, the levels between) it is an extrapolation: 750 MHz ≈ 427.6 W.
//   - Outside that window: no number ("not estimated for this setting": calibrate it with a wall meter).
//   - Watts are NOT scaled with the measured hashrate: at a fixed clock and rail the chips' draw doesn't follow
//     share luck or the work-rate's noise, so scaling would make the figure jump for no reason. When the hashrate
//     is well under what the setting made when measured the view says so (low_hash) instead.
//   - J/TH = watts ÷ the miner's reported hashrate (TH/s).

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// box2PowerPoint: one wall-meter reading on the first unit (the uncalibrated estimate's anchor points). Mode is
// the stock mode's key for the two stock plans, "" for the overclock readings (C7).
type box2PowerPoint struct {
	Mode  string
	MHz   int
	V     int // the voltage level as the miner reports it (42 = 7.67 V, 61 = 7.03 V)
	Watts float64
	THS   float64
}

var box2PowerPoints = []box2PowerPoint{
	{Mode: "Hashrate", MHz: 690, V: 42, Watts: 392, THS: 1.97},
	{Mode: "Low-power", MHz: 525, V: 61, Watts: 251.8, THS: 1.51},
	{MHz: 700, V: 42, Watts: 399.4, THS: 2.01},
	{MHz: 720, V: 42, Watts: 406.9, THS: 2.04},
	{MHz: 725, V: 42, Watts: 413.5, THS: 1.98},
}

// box2PowerK: W per (MHz × V²), fitted at Stock High: 392 W at 687.5 MHz on the chips and 7.67 V.
const box2PowerK = 392.0 / (687.5 * 7.67 * 7.67)

// box2GHSPerMHz: the hashrate per MHz the chips run, from Stock High on the first unit (1.97 TH/s at 687.5 MHz);
// the reference for the low-hashrate note where no reading was taken.
const box2GHSPerMHz = 1970.0 / 687.5

const (
	box2EstMinMHz   = 525                                             // the estimate's clock window (the readings span 525–725)
	box2EstMaxMHz   = 750                                             // (box2OCHardMax: the most any unit is probed to)
	box2EstSource   = "estimate from wall-meter readings on one unit" // shown with an uncalibrated figure
	box2EstMeasured = "measured with a wall meter on one unit"        // ... at one of the readings' own settings
	box2EstTolPct   = 5                                               // the ± a unit may differ by (shown as a hint)
	box2PowerMaxCal = 8                                               // settings with a reading (keeps the FAT file tiny)
	box2LowHashFrac = 0.8                                             // under this × the reference hashrate: say so
	box2CalSteadyS  = 600                                             // the miner should have hashed this long before a reading
	box2CalMinW     = 50.0                                            // a plausible wall reading for this miner (its PSU is 1200 W)
	box2CalMaxW     = 1200.0
)

// box2PointFor: the first unit's reading at exactly this setting.
func box2PointFor(mhz, v int) (box2PowerPoint, bool) {
	for _, s := range box2PowerPoints {
		if s.MHz == mhz && s.V == v {
			return s, true
		}
	}
	return box2PowerPoint{}, false
}

// box2ModeFor: the stock mode's key when (mhz, v) is a stock plan's setting, else "".
func box2ModeFor(mhz, v int) string {
	if p, ok := box2PointFor(mhz, v); ok {
		return p.Mode
	}
	return ""
}

// box2EstimateWatts: the uncalibrated wall power for a setting and the hashrate it made (TH/s), and whether it
// is the first unit's own reading there (measured) or the curve (ok=false outside 525–750 MHz or the levels).
func box2EstimateWatts(mhz, v int) (watts, ths float64, measured, ok bool) {
	if p, found := box2PointFor(mhz, v); found {
		return p.Watts, p.THS, true, true
	}
	lo, hi := model.VCodeMin, model.VCodeMax
	if lo <= 0 || hi <= 0 {
		lo, hi = 42, 61
	}
	if mhz < box2EstMinMHz || mhz > box2EstMaxMHz || v < lo || v > hi {
		return 0, 0, false, false
	}
	f := box2PLLClock(float64(mhz))
	if f <= 0 {
		return 0, 0, false, false
	}
	volts, _ := box2Volts(v)
	return math.Round(box2PowerK*f*volts*volts*10) / 10, math.Round(box2GHSPerMHz*f/10) / 100, false, true
}

// box2UnitWatts: this unit's wall power at a setting, for a limit (the probe's 460 W): its own meter reading
// there, else the estimate scaled by how this unit's Stock High reading (if any) compares with the first
// unit's; src says which. 0 when there is no figure.
func box2UnitWatts(mhz, v int) (float64, string) {
	c := getBox2PowerConfig()
	if k, i := c.find(mhz, v); i >= 0 {
		return k.Watts, "this unit's wall-meter reading"
	}
	w, _, _, ok := box2EstimateWatts(mhz, v)
	if !ok {
		return 0, ""
	}
	hf, hv := box2HashrateV()
	if k, i := c.find(hf, hv); i >= 0 {
		if ref, _, _, ok := box2EstimateWatts(hf, hv); ok && ref > 0 {
			return w * k.Watts / ref, "estimate, scaled to this unit's Stock High reading"
		}
	}
	return w, "estimate"
}

// box2PowerCal is one wall-meter reading, at one setting.
type box2PowerCal struct {
	MHz    int     `json:"mhz"`
	V      int     `json:"v"`     // trim code as the miner reports it
	Watts  float64 `json:"watts"` // what the meter showed
	GHS    float64 `json:"ghs"`   // the miner's hashrate then
	FanRPM int     `json:"fan_rpm,omitempty"`
	At     int64   `json:"at"`
}

type box2PowerConfig struct {
	MainsV float64        `json:"mains_v,omitempty"` // for amps; 0 = 100 V
	Cals   []box2PowerCal `json:"cals,omitempty"`
}

func (c box2PowerConfig) mains() float64 { return powerConfig{MainsV: c.MainsV}.mains() }

func (c box2PowerConfig) find(mhz, v int) (box2PowerCal, int) {
	for i, k := range c.Cals {
		if k.MHz == mhz && k.V == v {
			return k, i
		}
	}
	return box2PowerCal{}, -1
}

var (
	box2PowMu   sync.Mutex
	box2PowCfg  box2PowerConfig
	box2PowRead bool
	// box2PowerDash: where the handler reads the miner (a variable so tests can give it a dashboard).
	box2PowerDash = buildDashboard
)

func box2PowerPath() string { return filepath.Join(filepath.Dir(*authFile), "power-plans.json") }

func getBox2PowerConfig() box2PowerConfig {
	box2PowMu.Lock()
	defer box2PowMu.Unlock()
	if !box2PowRead {
		box2PowRead = true
		box2PowCfg = box2PowerConfig{}
		// readConfig falls back to the .bak fatWrite keeps, after a power cut mid-write on the FAT store
		if b, ok := readConfig(box2PowerPath(), json.Valid); ok {
			json.Unmarshal(b, &box2PowCfg)
		}
	}
	c := box2PowCfg
	c.Cals = append([]box2PowerCal(nil), box2PowCfg.Cals...)
	return c
}

func saveBox2PowerConfig(c box2PowerConfig) error {
	sort.Slice(c.Cals, func(i, j int) bool { return c.Cals[i].MHz > c.Cals[j].MHz })
	b, _ := json.MarshalIndent(c, "", " ")
	if err := cfgWrite(box2PowerPath(), b, 0o644); err != nil {
		return err
	}
	box2PowMu.Lock()
	box2PowCfg, box2PowRead = c, true
	box2PowMu.Unlock()
	return nil
}

// box2RunningSetting: the clock and trim code the miner reports for its hashing board(s), and the board ids.
// ok is false when no board hashes or the miner doesn't report a clock and code, or the hashing boards
// disagree (then no single setting's figure applies).
func box2RunningSetting(d dashboard) (mhz, v int, ids []int, ok bool) {
	for _, b := range d.Boards {
		if !b.Alive || b.GHS <= 0 {
			continue
		}
		m := int(math.Round(b.ClockMHz))
		if m <= 0 || b.VoltLevel <= 0 {
			return 0, 0, nil, false
		}
		if len(ids) > 0 && (m != mhz || b.VoltLevel != v) {
			return 0, 0, nil, false
		}
		mhz, v = m, b.VoltLevel
		ids = append(ids, b.ID)
	}
	return mhz, v, ids, len(ids) > 0
}

func box2FanRPM(d dashboard) int {
	n, s := 0, 0
	for _, b := range d.Boards {
		for _, f := range b.Fans {
			if f > 0 {
				n, s = n+1, s+f
			}
		}
	}
	if n == 0 {
		return 0
	}
	return s / n
}

// powerViewBox2: the Box II estimate for the dashboard and Settings → Power.
func powerViewBox2(d dashboard) map[string]any {
	if featOff(model.Feat.PowerEstimate) { // (powerView gates first): no watts / J/TH from any Box II path with C2 off
		return map[string]any{"available": false, "reason": "no estimate on this model (" + model.Name + ")"}
	}
	c := getBox2PowerConfig()
	var points []map[string]any
	for _, s := range box2PowerPoints {
		points = append(points, map[string]any{"mode": s.Mode, "mhz": s.MHz, "v": s.V, "watts": s.Watts, "ths": s.THS})
	}
	cals := []map[string]any{}
	for _, k := range c.Cals {
		e := map[string]any{"mhz": k.MHz, "v": k.V, "watts": k.Watts, "ghs": k.GHS, "fan_rpm": k.FanRPM, "at": k.At}
		if m := box2ModeFor(k.MHz, k.V); m != "" {
			e["mode"] = m
		}
		cals = append(cals, e)
	}
	v := map[string]any{"available": true, "model": "box2", "mains_v": c.mains(), "calibrated": false,
		"factor": 0.0, "points": points, "cals": cals, "est_tol_pct": box2EstTolPct, "source": "",
		"est_range": []int{box2EstMinMHz, box2EstMaxMHz}, "k": box2PowerK}
	if !d.OK || d.GHS <= 0 {
		v["reason"] = "shown while hashing"
		return v
	}
	mhz, code, ids, ok := box2RunningSetting(d)
	if !ok {
		v["reason"] = "the miner doesn't report one clock and voltage: no figure"
		return v
	}
	v["mhz"], v["vcode"] = mhz, code
	var watts, refGHS float64
	if k, i := c.find(mhz, code); i >= 0 {
		watts, refGHS = k.Watts, k.GHS
		v["calibrated"], v["source"], v["factor"] = true, "meter", 1.0
		v["cal_watts"], v["cal_at"], v["cal_mhz"], v["cal_v"] = k.Watts, k.At, k.MHz, k.V
	} else if w, ths, measured, ok := box2EstimateWatts(mhz, code); ok {
		watts, refGHS = w, ths*1000
		v["source"], v["source_note"] = "estimate", box2EstSource
		if measured {
			v["source"], v["source_note"] = "measured", box2EstMeasured
		}
	} else {
		// outside the window the readings support (525–750 MHz, the stock levels' interval): no number
		v["reason"] = fmt.Sprintf("not estimated for this setting (%d MHz · %s): calibrate it with a wall meter", mhz, box2LevelText(code))
		return v
	}
	if m := box2ModeFor(mhz, code); m != "" {
		v["mode"] = m
	}
	v["watts"], v["amps"] = round1(watts), round1(watts/c.mains())
	if ths := d.GHS / 1000; ths > 0.01 {
		v["jth"] = math.Round(watts / ths)
	}
	// the setting's figure is for the chain hashing as it did when measured; say so when it clearly isn't
	ghs := d.GHS
	if d.GHSAvg > 0 && d.Elapsed >= box2CalSteadyS {
		ghs = d.GHSAvg
	}
	if refGHS > 0 && ghs < box2LowHashFrac*refGHS {
		v["low_hash"] = true
		v["low_hash_note"] = fmt.Sprintf("hashrate %.2f TH/s is well under the %.2f TH/s this figure was taken at: the real draw may be lower", ghs/1000, refGHS/1000)
	}
	if len(ids) == 1 { // one board (the Box II): the whole figure is its
		v["boards"] = map[string]float64{fmt.Sprint(ids[0]): round1(watts)}
	}
	v["boards_hashing"] = len(ids)
	return v
}

// handlePowerBox2: GET the view. POST {mains_v}; {cal_watts} records a wall reading at the setting running now;
// {clear:true} drops every reading; {clear_mhz, clear_v} drops one. Every write goes to the config store
// through cfgWrite (crash-safe on the FAT, with a .bak).
func handlePowerBox2(w http.ResponseWriter, r *http.Request) {
	if !featGate(w, model.Feat.PowerEstimate, "the power estimate") { // (handlePower gates first: belt and braces)
		return
	}
	if r.Method == http.MethodPost {
		var in struct {
			MainsV   float64 `json:"mains_v"`
			CalWatts float64 `json:"cal_watts"`
			Clear    bool    `json:"clear"`
			ClearMHz int     `json:"clear_mhz"`
			ClearV   int     `json:"clear_v"`
		}
		if !decodeIn(w, r, &in) {
			return
		}
		c := getBox2PowerConfig()
		note := ""
		var d dashboard
		haveD := false
		if in.MainsV != 0 {
			if in.MainsV < 90 || in.MainsV > 260 {
				writeJSON(w, map[string]any{"ok": false, "err": "mains voltage must be 90–260 V"})
				return
			}
			c.MainsV = in.MainsV
		}
		if in.Clear {
			c.Cals = nil
			activity.emit("⚡ power calibration cleared (every setting)")
		}
		if in.ClearMHz != 0 {
			if _, i := c.find(in.ClearMHz, in.ClearV); i >= 0 {
				c.Cals = append(c.Cals[:i], c.Cals[i+1:]...)
				activity.emit(fmt.Sprintf("⚡ power calibration cleared for %d MHz · %s", in.ClearMHz, box2LevelText(in.ClearV)))
			}
		}
		if in.CalWatts != 0 {
			if in.CalWatts < box2CalMinW || in.CalWatts > box2CalMaxW {
				writeJSON(w, map[string]any{"ok": false, "err": fmt.Sprintf("enter the meter's reading in watts (%.0f–%.0f)", box2CalMinW, box2CalMaxW)})
				return
			}
			d, haveD = box2PowerDash(), true
			if !d.OK || d.GHS <= 0 {
				writeJSON(w, map[string]any{"ok": false, "err": "the miner isn't hashing right now; calibrate while it mines"})
				return
			}
			mhz, code, _, ok := box2RunningSetting(d)
			if !ok {
				writeJSON(w, map[string]any{"ok": false, "err": "couldn't read the clock and voltage the miner runs; try again in a moment"})
				return
			}
			if est, _, _, ok := box2EstimateWatts(mhz, code); ok {
				if f := in.CalWatts / est; f < 0.6 || f > 1.5 {
					writeJSON(w, map[string]any{"ok": false, "err": fmt.Sprintf("%.0f W is far from what this setting should draw (about %.0f W, measured on one unit). Check the meter is on this miner only and it's hashing", in.CalWatts, est)})
					return
				}
			}
			k := box2PowerCal{MHz: mhz, V: code, Watts: in.CalWatts, GHS: math.Round(d.GHS*10) / 10, FanRPM: box2FanRPM(d), At: time.Now().Unix()}
			if _, i := c.find(mhz, code); i >= 0 {
				c.Cals[i] = k
			} else if len(c.Cals) >= box2PowerMaxCal {
				writeJSON(w, map[string]any{"ok": false, "err": fmt.Sprintf("%d settings already have a reading; clear one first", box2PowerMaxCal)})
				return
			} else {
				c.Cals = append(c.Cals, k)
			}
			if d.Elapsed > 0 && d.Elapsed < box2CalSteadyS {
				note = fmt.Sprintf("saved, but the miner has only hashed %d min since it (re)started: for a steady reading, take it again after 10 min", d.Elapsed/60)
			}
			activity.emit(fmt.Sprintf("⚡ power calibrated: %.0f W at %d MHz · %s (%.2f TH/s)", in.CalWatts, mhz, box2LevelText(code), d.GHS/1000))
		}
		if err := saveBox2PowerConfig(c); err != nil {
			writeJSON(w, map[string]any{"ok": false, "err": cfgSaveErr(err)})
			return
		}
		if !haveD {
			d = box2PowerDash()
		}
		v := powerViewBox2(d)
		v["ok"] = true
		if note != "" {
			v["note"] = note
		}
		writeJSON(w, v)
		return
	}
	v := powerViewBox2(box2PowerDash())
	v["ok"] = true
	writeJSON(w, v)
}
