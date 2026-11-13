package main

// (patched tree) what handleTuner does at a start: note safetyGen before the checks, the run's plan-write hooks
// as newTunerEngine sets them, and taking the charge only if no safety stop came since.
func rcGenNote() int64                      { return safetyGen.Load() }
func rcTakeCharge(gen int64, s string) bool { return setChargeGen(gen, s) }

func rcRealTunerHooks(e *tunerEngine, gen int64) {
	e.planGen = gen
	e.applyPlan = func(p planMV) error {
		if e.dead.Load() {
			return errTunerDead
		}
		return applyTunePlanGen(e.planGen, p)
	}
	e.held = func() error {
		if err := miningHoldErr(); err != nil {
			return err
		}
		if genMoved(e.planGen) {
			return errSafetyStop
		}
		return nil
	}
}
