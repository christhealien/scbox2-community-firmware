package main

// Heat guard. Goldshell's fanctrl drives every fan to 100 % while a board sensor is over its cut-off
// (product.json tempcutoff: 92 °C on the SC Lite, 86 °C on the SC Box II), and shuts the boards down if
// it stays there. Every power-plan write sets all the fans to the plan's one level (intminer applies it),
// so a plan written during that (a preset, the schedule, the throttle, a tuner step, the fan curve) would
// pull the fans down exactly when the boards are hottest, until fanctrl's next 6-second cycle. While the
// hottest board reading is this hot, the panel never writes a fan level below what the fans run at now.

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// Thermal thresholds, seeded from the active model profile (model.go). At package-init time
// model == profSCLite, so these carry the exact pre-port SC Lite figures (heat-guard 88, abort 88/90,
// throttle 90, resume 80) and SC Lite behaviour is unchanged. main() calls setThermalFromModel after
// loadModelFiles to apply the loaded profile. On the SC Box II every figure lands BELOW Goldshell's own
// 86 °C cut-off (heat-guard 82, abort 82/84, throttle 82, resume 75), so the panel guards actually act
// — unlike the old hard-coded 88/90, which were ABOVE the Box II cut-off and so were dead code there.
var (
	heatGuardC   = model.HeatGuardC   // never pull the fans below this while a board is this hot (cutoff-4)
	abortDefC    = model.AbortDefC    // default fan-curve/throttle abort temperature (cutoff-4)
	abortMaxC    = model.AbortMaxC    // highest abort a user/preset/curve may set (cutoff-2; never above stock)
	throttleDefC = model.ThrottleDefC // default throttle temperature
	resumeDefC   = model.ResumeDefC   // default resume temperature
)

// setThermalFromModel re-seeds the thermal thresholds from the active profile. Called once from main()
// right after setWindowsFromModel (and from tests that switch models). All figures are derived from the
// profile's never-raised TempCutoffC and are guaranteed < the cut-off on every model.
func setThermalFromModel() {
	heatGuardC = model.HeatGuardC
	abortDefC = model.AbortDefC
	abortMaxC = model.AbortMaxC
	throttleDefC = model.ThrottleDefC
	resumeDefC = model.ResumeDefC
}

var (
	hgMu  sync.Mutex
	hgHot float64
	hgAt  time.Time
	// abortTierOn: the fan loop's abort tier holds the fans at 100 % (at its own temperature, which may be under
	// heatGuardC): every other plan write keeps the fans where they are meanwhile, as at heatGuardC
	abortTierOn atomic.Bool
	// noParkWarned: on a model with no working park action (Box II until B3 wires the real miner-API
	// board-off), thermalNoPark warns once per overheat episode instead of every poll. Reset on a cool reading.
	noParkWarned atomic.Bool
)

// noteHottest records the hottest board reading (from every dashboard build).
func noteHottest(c float64) {
	hgMu.Lock()
	hgHot, hgAt = c, time.Now()
	hgMu.Unlock()
}

// heatGuardActive: the last reading (within 90 s) is at or over heatGuardC.
func heatGuardActive() bool {
	if abortTierOn.Load() {
		return true
	}
	hgMu.Lock()
	defer hgMu.Unlock()
	// a hot last reading keeps the guard on until a fresh reading says otherwise: readings stopping
	// (backend down, a board dropping out) is no reason to let the fans come down
	return !hgAt.IsZero() && hgHot >= heatGuardC
}

// liveFanMax: the highest level any fan runs at now (-1 when it can't be read). The fan count comes from
// the model profile (4 on the SC Lite, 2 on the SC Box II), so this reads only the fans the board has.
// It reads fan levels; it never writes them (the fan files are B1's).
func liveFanMax() int {
	m := -1
	n := model.Fans
	if n <= 0 {
		n = 4 // unknown profile: fall back to reading up to the SC Lite fan count
	}
	for i := 0; i < n; i++ {
		if v, err := fanLvl(fmt.Sprintf("fan%d", i)); err == nil && v > m {
			m = v
		}
	}
	return m
}

// heatGuardFan: the fan level to write instead of pct (never lower than now while it's hot).
func heatGuardFan(pct int) int {
	if !heatGuardActive() {
		return pct
	}
	if m := liveFanMax(); m > pct {
		return m
	}
	return pct
}
