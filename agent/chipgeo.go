package main

// Chip trims on models other than the SC Lite (commissioning lane C5): the model-specific parts of the chip-trim
// feature. On the SC Lite every function here returns exactly what the shared code used before (the clock window,
// "raise PV a step", 46 chips / 23 pairs), so its behaviour and its page are unchanged.
//
// SC Box II: 1 board (chain 0), 36 ICT580 chips in 18 pairs on a series rail (chips 2k-1, 2k share a stage, as on
// the SC Lite: chipPartner). Its trim library is kit/files/scbox2_chip.c; that library never runs a chip above
// 690 MHz (CHIP_CEIL, the top stock clock), whatever the trims file says, so the panel holds "+" trims to the same
// ceiling (chipTrimCeil) and never shows a trim the library would skip.

import (
	"log"
	"os"
)

// chipTrimCeil: the fastest a "+" trim may run a chip (MHz): the clock window's top; on a model with stock plans
// only (the SC Box II) also never above its top stock clock, the trim library's own ceiling.
func chipTrimCeil() int {
	hi := clockWindowHi()
	if model.Plan == planBox2V {
		top := 0
		for _, t := range model.Box2Plans {
			top = max(top, t.FreqMHz)
		}
		if top > 0 {
			hi = min(hi, top)
		}
	}
	return hi
}

// chipVoltFix: what to do instead of more trims when many chips of a board are weak. The SC Lite raises PV; the
// SC Box II's voltage is a PWM trim code whose direction isn't measured, so the advice there is a lower clock.
func chipVoltFix() string {
	if model.Plan == planBox2V {
		return "lower the board clock a step"
	}
	return "raise PV a step"
}

// chipGeo: the board geometry the Chip trims card describes (chips per board, series pairs). box2: show the SC Box
// II's wording (no PV) instead of the SC Lite's.
func chipGeo() map[string]any {
	n := model.ChipsPerBoard
	return map[string]any{"chips": n, "pairs": n / 2, "box2": model.Plan == planBox2V, "fix": chipVoltFix()}
}

// chipTrimBoardOK: a board (chain) id the trims may name: 0 .. Boards-1 (the SC Lite: 0..3, as before).
func chipTrimBoardOK(chain int) bool { return chain >= 0 && chain < max(model.Boards, 1) }

// chipHookInertAtStart: on a model where chip trims have to be commissioned (the SC Box II) and aren't -- never
// switched on, or switched off again in Settings -> Commissioning -- a chiphook flag left on the config partition
// would still make the miner wrapper run the trim library. It is removed at start-up (and the miner restarted if
// it runs with the library), so with ChipTrims off the miner runs exactly as without this feature. Nothing to do on
// the SC Lite (chip trims always on there) or on the "unknown" profile (nothing is commissionable: a misread
// product file must not wipe an SC Lite's own setting).
var chipHookRestart = restartMiner

func chipHookInertAtStart() {
	if model.Feat.ChipTrims || commissionFind(model.ID, "ChipTrims") == nil {
		return
	}
	// the trims themselves go too: switching C5 off used to leave them in the trims file, so the next run of the
	// miner with the library (or a re-switch-on) set the chips back to the trimmed clocks. Only the trims are
	// cleared here; a file with no trims is left alone.
	if len(readChipTrims()) > 0 {
		if err := writeChipTrims(nil); err != nil {
			log.Printf("chip trims: not commissioned on this unit, but the trims file couldn't be cleared: %v", err)
		} else {
			log.Printf("chip trims: not commissioned on this unit: cleared the per-chip trims")
		}
	}
	if _, err := os.Stat(chipHookFlag()); err != nil {
		return
	}
	if err := os.Remove(chipHookFlag()); err != nil {
		log.Printf("chip trims: not commissioned on this unit, but the chiphook flag couldn't be removed: %v", err)
		return
	}
	chipHookOffNote("chip trims switched off: they aren't commissioned on this unit (Settings → Commissioning)")
	log.Printf("chip trims: not commissioned on this unit: removed the chiphook flag")
	if minerHasLib(brandChipLib) {
		chipHookRestart() // the running miner has the trim library: the next start is without it
	}
}
