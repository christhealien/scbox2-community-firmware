package main

// Two safety nets for the SC Box II's plan (review of v0.4.0, H1/H2):
//
//   - hwRunActive: a hardware run (tuner, clock-ceiling probe, strong-chip test) that owns the clock right now.
//     Switching a feature off (commissioning) and installing an image are refused while one runs, so the run
//     can put the miner back itself.
//   - box2PlanBoot: at start-up, the plan minerd will keep sending is checked against what this panel run
//     allows. A clock/trim tuple the gates no longer allow (a feature was switched off) is replaced by the stock
//     Hashrate plan; a plan whose 4th field isn't the stock one (written by v0.4.0's predecessors, which put
//     the fan duty there: on the Box II that field is the second voltage-trim PWM) is re-sent with the stock
//     value. Nothing at all on other models.

import (
	"encoding/json"
	"log"
	"time"
)

// test seams: a first start is a start with no panel files yet (freshInstall)
var (
	box2FirstStart      = func() bool { return freshInstall }
	box2FirstStartApply = applyBox2ModeGen
)

func hwRunActive() string {
	switch {
	case getCharge() == "tuner":
		return "the tuner is running: stop it first"
	case ceilSnapshot().Running:
		return "the clock-ceiling probe is running: stop it first"
	case sctRunning():
		return "the strong-chip test is running (Controls → Chip trims): stop it first"
	}
	return ""
}

// box2PlanVerdict: what start-up should do with the running manual plan s ("" = nothing).
func box2PlanVerdict(cur map[string]any, s string) (string, powerplan) {
	pp, ok := parsePowerplanBox2(s)
	if !ok {
		if s != "" { // a plan the panel can't read ("1e2 V", "-5 V", a fractional level): the stock plan goes back
			return "stock", pp
		}
		return "", pp
	}
	allowed := box2StockMatch(pp) || (model.Feat.ClockControl && box2CheckTuple(pp.FreqMHz, pp.VrunMv) == nil)
	if !allowed {
		return "stock", pp
	}
	if f4, ok := box2Field4(cur, pp); ok && f4 != pp.Fan2 {
		return "field4", pp
	}
	return "", pp
}

func box2PlanBoot() {
	if model.Plan != planBox2V {
		return
	}
	go func() {
		for i := 0; i < 40; i++ { // minerd answers a minute or so after a boot
			if i > 0 {
				time.Sleep(15 * time.Second)
			}
			raw, err := stockReq("GET", "/mcb/setting", nil)
			if err != nil {
				continue
			}
			var cur map[string]any
			if json.Unmarshal(raw, &cur) != nil {
				continue
			}
			box2PlanBootOnce(cur)
			return
		}
	}()
}

// box2PlanBootOnce: start-up's one look at the plan minerd answered with (cur: its /mcb/setting).
func box2PlanBootOnce(cur map[string]any) {
	if box2FirstStart() {
		// a first start (stock, or a wiped config): mine on Stock High, selected the way Controls does it,
		// so the panel shows what runs; the fans are on auto (balanced curve) by default already
		err := box2FirstStartApply("Hashrate", noGen) // the key of the mode that reads Stock High
		log.Printf("first start: %s selected, fans on auto (balanced curve) (%v)", box2StockHigh, err)
		if err == nil {
			activity.emit("⚙ first start: " + box2StockHigh + " selected, fans on auto (balanced curve)")
		}
		return
	}
	if m, _ := cur["manual"].(bool); !m {
		return // a stock mode selected by index: minerd's own plan
	}
	s, _ := cur["manualPowerplan"].(string)
	what, _ := box2PlanVerdict(cur, s)
	var err error
	switch what {
	case "stock":
		hi := box2HashrateTuple()
		_, err = mutatePlanNoWake(func(p *powerplan) { p.FreqMHz, p.VrunMv = hi.FreqMHz, hi.V })
		log.Printf("start-up: %q is not allowed with the features switched on now: stock %d MHz / %s put back (%v)", planShowRaw(s), hi.FreqMHz, box2LevelText(hi.V), err)
		activity.emit("⚙ start-up: the running clock setting isn't allowed any more — Stock High put back")
	case "field4":
		_, err = mutatePlanNoWake(func(*powerplan) {})
		log.Printf("start-up: the running plan (%s) had a fan duty in its 4th field, which on this model is a voltage-trim setting, not a fan speed: re-sent with the stock trim value (%v)", planShowRaw(s), err)
	}
	// (on Idle nothing is written, errMinerIdle: the wake-up write goes through the same writer)
}
