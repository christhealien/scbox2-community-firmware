package main

// Power estimate on the planBox2V model (the SC Box II), commission key "PowerEstimate" (test C2). Only reached
// when model.Plan == planBox2V AND model.Feat.PowerEstimate is on (powerView / handlePower gate first); with the
// gate off nothing here runs and no file is read or written.
//
// The SC Lite clock×PV² model (power.go) does not transfer: different chips (36 ICT580 on one board), a different
// power stage, and the Box II's "voltage" is a PWM trim CODE whose scale and direction are unknown. So this is a
// per-setting table, not a model:
//
//   - The running setting is what the miner itself reports (cgminer API devs: "clock" and "voltage" = trim
//     code), so it is right whichever way the setting was made (stock plan via minerd, or a later pgaset).
//   - A setting with a wall-meter reading shows that reading (the owner's own unit, fans and PSU included).
//   - A stock plan without a reading shows Goldshell's published figure for that mode, marked ≈ and "Goldshell
//     spec ±5 %" (source: Goldshell's BOX II product page; see REPORT.md).
//   - Any other setting shows NO number: "not calibrated for this setting". No interpolation between the plans
//     (they differ in clock AND trim code, and the code's effect on the rail is unmeasured) and no extrapolation.
//   - Watts are NOT scaled with the measured hashrate: at a fixed clock and rail the chips' draw doesn't follow
//     share luck or the work-rate's noise, so scaling would make the figure jump for no reason. When the hashrate
//     is well under what the setting made at calibration (or under spec) the view says so (low_hash) instead.
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

// box2PowerSpec: Goldshell's published wall power for a stock mode (BOX II product page, "Support SiaCoin
// mining", goldshell.com/?p=77152: default 1.9 T ±5 % · 400 W ±5 % · 211 W/T; low-power 1.45 T · 260 W ·
// 179 W/T). Matched to the profile's stock tuples by mode: 1.9/1.45 = 1.31 ≈ 690/525 = 1.31.
type box2PowerSpec struct {
	Mode  string
	MHz   int
	V     int
	Watts float64
	THS   float64
}

var box2PowerSpecs = []box2PowerSpec{
	{Mode: "Hashrate", MHz: 690, V: 42, Watts: 400, THS: 1.9},
	{Mode: "Low-power", MHz: 525, V: 61, Watts: 260, THS: 1.45},
}

const (
	box2SpecTolPct  = 5                                                 // Goldshell's stated ± on power and hashrate
	box2SpecSource  = "the OEM's published figure for this mode (±5 %)" // shown with an uncalibrated stock figure
	box2PowerMaxCal = 8                                                 // settings with a reading (keeps the FAT file tiny)
	box2LowHashFrac = 0.8                                               // under this × the reference hashrate: say so
	box2CalSteadyS  = 600                                               // the miner should have hashed this long before a reading
	box2CalMinW     = 50.0                                              // a plausible wall reading for this miner (its PSU is 1200 W)
	box2CalMaxW     = 1200.0
)

func box2SpecFor(mhz, v int) (box2PowerSpec, bool) {
	for _, s := range box2PowerSpecs {
		if s.MHz == mhz && s.V == v {
			return s, true
		}
	}
	return box2PowerSpec{}, false
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
	var specs []map[string]any
	for _, s := range box2PowerSpecs {
		specs = append(specs, map[string]any{"mode": s.Mode, "mhz": s.MHz, "v": s.V, "watts": s.Watts, "ths": s.THS})
	}
	cals := []map[string]any{}
	for _, k := range c.Cals {
		e := map[string]any{"mhz": k.MHz, "v": k.V, "watts": k.Watts, "ghs": k.GHS, "fan_rpm": k.FanRPM, "at": k.At}
		if s, ok := box2SpecFor(k.MHz, k.V); ok {
			e["mode"] = s.Mode
		}
		cals = append(cals, e)
	}
	v := map[string]any{"available": true, "model": "box2", "mains_v": c.mains(), "calibrated": false,
		"factor": 0.0, "specs": specs, "cals": cals, "spec_tol_pct": box2SpecTolPct, "source": ""}
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
	} else if s, ok := box2SpecFor(mhz, code); ok {
		watts, refGHS = s.Watts, s.THS*1000
		v["source"], v["source_note"], v["mode"] = "spec", box2SpecSource, s.Mode
	} else {
		// between or outside the plans: no reading here, and nothing to justify an inter/extrapolation
		v["reason"] = fmt.Sprintf("not calibrated for this setting (%d MHz · %s)", mhz, box2LevelText(code))
		return v
	}
	if s, ok := box2SpecFor(mhz, code); ok {
		v["mode"] = s.Mode
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
			if s, ok := box2SpecFor(mhz, code); ok {
				if f := in.CalWatts / s.Watts; f < 0.6 || f > 1.5 {
					writeJSON(w, map[string]any{"ok": false, "err": fmt.Sprintf("%.0f W is far from what the %s mode should draw (OEM spec: %.0f W ±%d %%). Check the meter is on this miner only and it's hashing", in.CalWatts, presetLabel(s.Mode), s.Watts, box2SpecTolPct)})
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
			writeJSON(w, map[string]any{"ok": false, "err": "could not save: " + err.Error()})
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
