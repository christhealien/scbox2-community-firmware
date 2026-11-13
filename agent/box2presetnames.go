package main

import (
	"fmt"
	"log"
	"strings"
)

// box2RenameReservedPresets: on the SC Box II the two stock modes read "Stock High" and "Stock Low" (v0.5.0). A
// saved preset an owner named that way under v0.4.x (allowed then) would now be hidden behind the stock mode, and
// a schedule change naming it would switch to the stock mode instead. Once at start-up, such a preset is renamed
// "<name> (mine)" ("(mine 2)", … if taken), and the schedule's changes and "rest of the time" that named it follow.
// Nothing happens on the SC Lite or when no preset uses a stock name.
func box2RenameReservedPresets() {
	if model.Plan != planBox2V {
		return
	}
	ps := loadPresets()
	taken := map[string]bool{}
	for _, p := range ps {
		taken[strings.ToLower(p.Name)] = true
	}
	ren := map[string]string{}
	for i := range ps {
		if !isBox2StockName(ps[i].Name) {
			continue
		}
		n := ps[i].Name + " (mine)"
		for k := 2; taken[strings.ToLower(n)] || isBox2StockName(n); k++ {
			n = fmt.Sprintf("%s (mine %d)", ps[i].Name, k)
		}
		taken[strings.ToLower(n)] = true
		ren[ps[i].Name] = n
		ps[i].Name = n
	}
	if len(ren) == 0 {
		return
	}
	if err := savePresets(ps); err != nil {
		log.Printf("presets: couldn't rename presets that use a stock name: %v", err)
		return
	}
	_ = updateSchedule(func(sc *schedule) {
		for j := range sc.Rules {
			if n, ok := ren[sc.Rules[j].Preset]; ok {
				sc.Rules[j].Preset = n
			}
		}
		if n, ok := ren[sc.Base]; ok {
			sc.Base = n
		}
	})
	for o, n := range ren {
		msg := fmt.Sprintf("✎ preset %q renamed %q: on this firmware %q is a stock mode", o, n, o)
		log.Print(msg)
		activity.emit(msg)
	}
}
