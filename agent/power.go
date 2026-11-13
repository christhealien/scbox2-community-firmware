package main

// Power: the SC Lite has no power sensor, so watts are an estimate, the model from blake2b-asic-control:
// the chips follow clock × PV² (PV is the board's real supply voltage), about 7 % doesn't change (control
// board, fans at speed, PSU overhead), scaled to 899 W at 625 MHz · PV 9400. A reading from a wall-plug
// meter calibrates it to this unit: the factor between what the meter shows and what the model says at
// the setting running then. Mains voltage only turns watts into amps.
//
// (The SC Box and HS Box need a different model, clock only; that comes with their firmware.)

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

const (
	powerRefW   = 899.0
	powerRefMHz = 625.0
	powerRefPV  = 9400.0
	powerFixed  = 0.07
)

type powerConfig struct {
	MainsV   float64 `json:"mains_v"`             // for amps; 0 = 100 V
	Factor   float64 `json:"factor,omitempty"`    // meter ÷ model at the calibration setting; 0 = not calibrated
	CalWatts float64 `json:"cal_watts,omitempty"` // the meter reading
	CalMHz   int     `json:"cal_mhz,omitempty"`   // the setting it was taken at
	CalPV    int     `json:"cal_pv,omitempty"`
	CalAt    int64   `json:"cal_at,omitempty"`
}

var (
	powerMu   sync.Mutex
	powerCfg  powerConfig
	powerRead bool
)

func powerPath() string { return filepath.Join(filepath.Dir(*authFile), "power.json") }

func getPowerConfig() powerConfig {
	powerMu.Lock()
	defer powerMu.Unlock()
	if !powerRead {
		powerRead = true
		if b, err := os.ReadFile(powerPath()); err == nil {
			json.Unmarshal(b, &powerCfg)
		}
	}
	return powerCfg
}

func savePowerConfig(c powerConfig) error {
	powerMu.Lock()
	powerCfg, powerRead = c, true
	powerMu.Unlock()
	b, _ := json.MarshalIndent(c, "", " ")
	return writeAtomic(powerPath(), b, 0o644)
}

func (c powerConfig) mains() float64 {
	if c.MainsV >= 90 && c.MainsV <= 260 {
		return c.MainsV
	}
	return 100
}

// modelWatts: the uncalibrated estimate at a clock (MHz) and PV (mV) with all four boards hashing. The
// chips run on the 12.5 MHz grid.
func modelWatts(mhz, pv float64) float64 { return modelWattsN(mhz, pv, powerRefBoards) }

// modelWattsN: the same with n boards hashing. The reference (899 W) is four boards; the chips' part is
// split evenly between them, the fixed part (control board, fans, PSU overhead) stays whatever runs. A
// miner with one board hashing draws its board's share plus the fixed part, not the whole miner's.
func modelWattsN(mhz, pv float64, n int) float64 {
	if mhz <= 0 || pv <= 0 || n <= 0 {
		return 0
	}
	eff := math.Floor(mhz/12.5) * 12.5
	return powerRefW * (powerFixed + (1-powerFixed)*float64(n)/powerRefBoards*(eff/powerRefMHz)*math.Pow(pv/powerRefPV, 2))
}

const powerRefBoards = 4

// The boards' supply voltage follows PV only (mV changes nothing, live or after a restart). Measured with a
// multimeter on one board's supply: it reads PV within about 10 mV (9.41 V at PV 9400, 9.21 at 9200) and
// doesn't go below about 9.09 V (PV 9000 and lower all read 9.09); the miner's own reading of it is 50–80
// mV lower (9.36, 9.15 … 9.01). A wall meter on a four-board miner agreed: from PV 9000 down its boards
// read 8.96 V and the meter stayed at 710 W. So the watts are worked out from what the boards read when
// they report it, scaled to PV terms; from the setting otherwise, with PV held at that floor.
const (
	pvPerReading = 9400.0 / 9320.0 // the setting at stock ÷ what the boards read then
	pvFloor      = 9100            // below this PV the supply stays at ~9.1 V (measured)
)

// boardVoltMV: the median of the hashing boards' reported voltages (mV), 0 when none report.
func boardVoltMV(d dashboard) float64 {
	var v []float64
	for _, b := range d.Boards {
		if b.Alive && b.VoltageV > 5 {
			v = append(v, b.VoltageV*1000)
		}
	}
	if len(v) == 0 {
		return 0
	}
	sort.Float64s(v)
	return v[len(v)/2]
}

// effectivePV: the PV that matches what the boards really get (for the power model).
func effectivePV(setPV float64, d dashboard) float64 {
	if r := boardVoltMV(d); r > 0 {
		return r * pvPerReading
	}
	return math.Min(math.Max(setPV, pvFloor), float64(pvHardMax)) // the supply's range: 9100–9600
}

func (c powerConfig) watts(mhz, pv float64) float64 { return c.wattsN(mhz, pv, powerRefBoards) }

func (c powerConfig) wattsN(mhz, pv float64, n int) float64 {
	w := modelWattsN(mhz, pv, n)
	if c.Factor > 0 {
		w *= c.Factor
	}
	return w
}

// the running setting, cached briefly (it's read from the stock backend)
var (
	planCacheMu sync.Mutex
	planCache   powerplan
	planCacheAt time.Time
)

var planTryAt time.Time

func runningPlan() (powerplan, bool) {
	planCacheMu.Lock()
	if (time.Since(planCacheAt) < 20*time.Second && planCache.FreqMHz > 0) || time.Since(planTryAt) < 10*time.Second {
		pc := planCache
		planCacheMu.Unlock()
		return pc, pc.FreqMHz > 0 // fresh, or a read is in flight / just failed: don't queue up behind the backend
	}
	planTryAt = time.Now()
	planCacheMu.Unlock()
	pp, ok := readStockBaseline("current") // without the lock: a slow backend holds up nobody else
	planCacheMu.Lock()
	defer planCacheMu.Unlock()
	if ok && pp.FreqMHz > 0 && pp.PVMv > 0 {
		planCache, planCacheAt = pp, time.Now()
		return pp, true
	}
	return planCache, planCache.FreqMHz > 0
}

// powerView: watts for the whole miner and each hashing board (the chip part split between the boards
// that are hashing, the fixed part too), plus what the page needs to show how it was worked out.
func powerView(d dashboard) map[string]any {
	if featOff(model.Feat.PowerEstimate) {
		// Box II: the watt model here is SC Lite's (899 W @ 625 MHz · PV 9400, 4 boards) and is meaningless
		// for this miner, and its "voltage" field is a PWM-trim level, not a rail voltage — never feed it to
		// the estimator (effectivePV/boardVoltMV). No figure on this model until it is measured on hardware.
		return map[string]any{"available": false, "reason": "no estimate on this model (" + model.Name + ")"}
	}
	if model.Plan == planBox2V {
		return powerViewBox2(d) // per-setting wall-meter table (power_box2.go), never the clock×PV² model
	}
	c := getPowerConfig()
	v := map[string]any{"calibrated": c.Factor > 0, "factor": c.Factor, "mains_v": c.mains(),
		"cal_watts": c.CalWatts, "cal_mhz": c.CalMHz, "cal_pv": c.CalPV, "cal_at": c.CalAt}
	pp, ok := runningPlan()
	if !ok || d.GHS <= 0 {
		return v // not hashing or setting unknown: no figure rather than a wrong one
	}
	var hashing []board
	for _, b := range d.Boards {
		if b.Alive && b.GHS > 0 {
			hashing = append(hashing, b)
		}
	}
	n := len(hashing)
	if n == 0 {
		n = powerRefBoards // the API lists no boards: assume a full miner
	}
	eff := effectivePV(float64(pp.PVMv), d)
	total := c.wattsN(float64(pp.FreqMHz), eff, n)
	v["mhz"], v["pv"], v["watts"], v["amps"] = pp.FreqMHz, pp.PVMv, round1(total), round1(total/c.mains())
	v["pv_eff"], v["boards_hashing"] = math.Round(eff), n
	// set at (or under) the supply's floor while the boards read more: they're held at 9.1 V. (Near the top the
	// reading is a little under the setting anyway: that's not "held".)
	if r := boardVoltMV(d); r > 0 && pp.PVMv <= pvHardMin && float64(pp.PVMv) < r*pvPerReading-60 {
		// set below what the miner will supply: the boards stay at their floor
		v["held_v"] = math.Round(r) / 1000
	}
	if len(hashing) > 0 {
		// each hashing board: its chips' share plus an even share of the fixed part
		per := map[string]float64{}
		for _, b := range hashing {
			per[fmt.Sprint(b.ID)] = round1(total / float64(len(hashing)))
		}
		v["boards"] = per
	}
	return v
}

func round1(x float64) float64 { return math.Round(x*10) / 10 }

// GET: settings and the current estimate. POST {mains_v} sets the mains voltage; {cal_watts} calibrates
// from a wall-meter reading taken now; {clear:true} removes the calibration.
func handlePower(w http.ResponseWriter, r *http.Request) {
	if !featGate(w, model.Feat.PowerEstimate, "the power estimate") {
		return
	}
	if model.Plan == planBox2V {
		handlePowerBox2(w, r)
		return
	}
	if r.Method == http.MethodPost {
		var in struct {
			MainsV   float64 `json:"mains_v"`
			CalWatts float64 `json:"cal_watts"`
			Clear    bool    `json:"clear"`
		}
		if !decodeIn(w, r, &in) {
			return
		}
		c := getPowerConfig()
		if in.MainsV != 0 {
			if in.MainsV < 90 || in.MainsV > 260 {
				writeJSON(w, map[string]any{"ok": false, "err": "mains voltage must be 90–260 V"})
				return
			}
			c.MainsV = in.MainsV
		}
		if in.Clear {
			c.Factor, c.CalWatts, c.CalMHz, c.CalPV, c.CalAt = 0, 0, 0, 0, 0
			activity.emit("⚡ power calibration cleared")
		}
		if in.CalWatts != 0 {
			if in.CalWatts < 50 || in.CalWatts > 3000 {
				writeJSON(w, map[string]any{"ok": false, "err": "enter the meter's reading in watts (50–3000)"})
				return
			}
			pp, ok := readStockBaseline("current")
			if !ok || pp.FreqMHz <= 0 || pp.PVMv <= 0 {
				writeJSON(w, map[string]any{"ok": false, "err": "couldn't read the miner's current setting; try again in a moment"})
				return
			}
			d := buildDashboard()
			if !d.OK || d.GHS <= 0 {
				writeJSON(w, map[string]any{"ok": false, "err": "the miner isn't hashing right now; calibrate while it mines"})
				return
			}
			eff := effectivePV(float64(pp.PVMv), d)
			nb := 0
			for _, b := range d.Boards {
				if b.Alive && b.GHS > 0 {
					nb++
				}
			}
			if nb == 0 {
				nb = powerRefBoards
			}
			model := modelWattsN(float64(pp.FreqMHz), eff, nb)
			f := in.CalWatts / model
			if f < 0.5 || f > 1.6 {
				writeJSON(w, map[string]any{"ok": false, "err": fmt.Sprintf("%.0f W is far from what %d MHz · PV %d should draw (about %.0f W). Check the meter is on this miner only and it's hashing", in.CalWatts, pp.FreqMHz, pp.PVMv, model)})
				return
			}
			c.Factor, c.CalWatts, c.CalMHz, c.CalPV, c.CalAt = math.Round(f*1000)/1000, in.CalWatts, pp.FreqMHz, pp.PVMv, time.Now().Unix()
			activity.emit(fmt.Sprintf("⚡ power calibrated: %.0f W at %d MHz · PV %d (factor %.3f)", in.CalWatts, pp.FreqMHz, pp.PVMv, c.Factor))
		}
		if err := savePowerConfig(c); err != nil {
			writeJSON(w, map[string]any{"ok": false, "err": "could not save: " + err.Error()})
			return
		}
	}
	v := powerView(buildDashboard())
	v["ok"] = true
	writeJSON(w, v)
}
