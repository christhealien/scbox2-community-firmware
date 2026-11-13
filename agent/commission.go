package main

// Commissioning: per-unit switches for the features a model ships with OFF until they have been proven on that
// unit's hardware (the SC Box II: every control feature). The model profile says what the model CAN do once
// proven; this file says what the owner has proven and switched on, on THIS miner. Each feature has a hardware
// test in the commissioning plan; the owner runs it, and only then switches the feature on here.
//
//   - Stored on the config partition (commissioned.json next to auth.json), so it survives reboots and is
//     wiped by an SD flash like every other setting (a fresh image starts with everything off again).
//   - Applied once at start-up, right after the model profile is loaded, so every gate in the panel sees one
//     consistent set for the whole run. A change restarts the panel (the boot script restarts it in 5 s).
//   - Only features listed for the model can be switched on; dependencies are enforced (the tuner needs clock
//     and voltage control); switching a feature on needs the panel password again; switching off never does.
//   - On a model whose profile already has a feature on (the SC Lite: everything), there is nothing to commission.

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// commissionFeature describes one switch: which modelFeatures field it sets, what it needs first, and the
// commissioning-plan test that proves it.
type commissionFeature struct {
	Key      string   // modelFeatures field name
	Label    string   // shown in the panel
	Requires []string // other Keys that must be on first
	Test     string   // the commissioning-plan section that proves it
	set      func(f *modelFeatures, on bool)
}

// commissionable lists, per model ID, the features that may be switched on after a hardware test. Features not
// listed can never be switched on for that model (the SC Box II: per-fan-pair offset — both fans share one
// channel; the hardware watchdog — there is no watchdog driver and the kernel's locks up; "fit to my fans" —
// the stock fan controller is calibrated for the stock fans).
var commissionable = map[string][]commissionFeature{
	"scbox2": {
		{Key: "SoftIdle", Label: "Stop hashing from the panel (safety guards, schedule idle)",
			Test: "C1 soft idle", set: func(f *modelFeatures, on bool) { f.SoftIdle = on }},
		{Key: "PowerEstimate", Label: "Power estimate (watts, J/TH: wall-meter readings per setting, else the OEM's published figure for the stock modes)",
			Test: "C2 power estimate", set: func(f *modelFeatures, on bool) { f.PowerEstimate = on }},
		{Key: "ClockControl", Label: "Clock control (any clock inside the board's window)",
			Test: "C3 clock control", set: func(f *modelFeatures, on bool) { f.ClockControl = on }},
		{Key: "VoltageControl", Label: "Voltage control",
			Test: "C4 voltage control", set: func(f *modelFeatures, on bool) { f.VoltageControl = on }},
		{Key: "ChipTrims", Label: "Per-chip clock trims",
			Requires: []string{"ClockControl"},
			Test:     "C5 chip trims", set: func(f *modelFeatures, on bool) { f.ChipTrims = on }},
		{Key: "CeilingProbe", Label: "Clock ceiling probe (steps the clock above 690 MHz, up to this build's ceiling, and finds where it stops running clean)",
			Requires: []string{"ClockControl"},
			Test:     "C6 ceiling probe", set: func(f *modelFeatures, on bool) { f.CeilingProbe = on }},
		{Key: "OverclockUnlock", Label: "Overclock (clocks above 690 MHz, up to this build's ceiling; needs the probe first)",
			Requires: []string{"ClockControl", "CeilingProbe"},
			Test:     "C7 overclock", set: func(f *modelFeatures, on bool) { f.OverclockUnlock = on }},
		{Key: "Tuner", Label: "Auto-tuner",
			Requires: []string{"ClockControl", "VoltageControl"},
			Test:     "C8 tuner", set: func(f *modelFeatures, on bool) { f.Tuner = on }},
		{Key: "FirmwareUpdate", Label: "Firmware update from the panel",
			Test: "C9 in-panel update", set: func(f *modelFeatures, on bool) { f.FirmwareUpdate = on }},
	},
}

// publicFeatures: in the public release (publicBuild) these features are always on, everything else in
// commissionable is always off, and nothing can be switched (no Commissioning section, no banner). They are the ones
// that passed their hardware tests (C1 stop hashing, C2 power estimate, C3 clock, C4 voltage, C5 chip trims,
// C9 in-panel update); the ceiling probe (C6), overclock (C7) and the tuner (C8) wait for a later release.
var publicFeatures = map[string][]string{
	"scbox2": {"SoftIdle", "PowerEstimate", "ClockControl", "VoltageControl", "ChipTrims", "FirmwareUpdate"},
}

type commissionEntry struct {
	On   bool   `json:"on"`
	At   string `json:"at,omitempty"`   // when it was switched on (RFC 3339)
	Note string `json:"note,omitempty"` // the owner's note, e.g. what the test showed
}

type commissionState struct {
	Model    string                     `json:"model"` // the model ID this file was written for
	Features map[string]commissionEntry `json:"features"`
}

var (
	commissionMu      sync.Mutex
	commissionApplied = map[string]bool{} // what was applied at start-up (the running state)
	commissionRestart = func() { time.Sleep(500 * time.Millisecond); os.Exit(0) }
)

func commissionPath() string { return filepath.Join(filepath.Dir(*authFile), "commissioned.json") }

func commissionFind(id, key string) *commissionFeature {
	for i, f := range commissionable[id] {
		if f.Key == key {
			return &commissionable[id][i]
		}
	}
	return nil
}

func loadCommission() commissionState {
	st := commissionState{Features: map[string]commissionEntry{}}
	b, err := os.ReadFile(commissionPath())
	if err != nil {
		return st
	}
	if json.Unmarshal(b, &st) != nil || st.Features == nil {
		log.Printf("commissioning: %s is unreadable; every commissioned feature stays off", commissionPath())
		return commissionState{Features: map[string]commissionEntry{}}
	}
	return st
}

// commissionClosure: the features of `on` whose requirements are all on too (repeated until stable, so a
// feature whose requirement was dropped drops as well).
func commissionClosure(id string, on map[string]bool) map[string]bool {
	out := map[string]bool{}
	for k, v := range on {
		if v && commissionFind(id, k) != nil {
			out[k] = true
		}
	}
	for changed := true; changed; {
		changed = false
		for k := range out {
			for _, r := range commissionFind(id, k).Requires {
				if !out[r] {
					delete(out, k)
					changed = true
					break
				}
			}
		}
	}
	return out
}

// applyCommission turns on, in the running profile, the features the owner commissioned on this unit. Called
// once, right after the model profile is loaded. A file written for another model is ignored.
func applyCommission() {
	commissionMu.Lock()
	defer commissionMu.Unlock()
	commissionApplied = map[string]bool{}
	if len(commissionable[model.ID]) == 0 {
		return
	}
	if publicBuild { // the public release: a fixed set, whatever an earlier build saved on the config partition
		var names []string
		for _, k := range publicFeatures[model.ID] {
			if f := commissionFind(model.ID, k); f != nil {
				f.set(&model.Feat, true)
				commissionApplied[k] = true
				names = append(names, k)
			}
		}
		sort.Strings(names)
		log.Printf("public release: features on: %s", strings.Join(names, ", "))
		return
	}
	st := loadCommission()
	if st.Model != "" && st.Model != model.ID {
		log.Printf("commissioning: %s was written for %q, this miner is %q: ignored", commissionPath(), st.Model, model.ID)
		return
	}
	want := map[string]bool{}
	for k, e := range st.Features {
		want[k] = e.On
	}
	on := commissionClosure(model.ID, want)
	var names []string
	for k := range on {
		commissionFind(model.ID, k).set(&model.Feat, true)
		commissionApplied[k] = true
		names = append(names, k)
	}
	sort.Strings(names)
	if len(names) > 0 {
		log.Printf("commissioning: switched on for this unit: %s", strings.Join(names, ", "))
	}
}

func commissionView() map[string]any {
	if publicBuild { // no Commissioning section in the public release
		return map[string]any{"ok": true, "model": model.ID, "features": []map[string]any{}, "available": false, "public": true}
	}
	commissionMu.Lock()
	defer commissionMu.Unlock()
	st := loadCommission()
	var list []map[string]any
	for _, f := range commissionable[model.ID] {
		e := st.Features[f.Key]
		list = append(list, map[string]any{"key": f.Key, "label": f.Label, "requires": f.Requires, "test": f.Test,
			"saved": e.On, "running": commissionApplied[f.Key], "at": e.At, "note": e.Note})
	}
	return map[string]any{"ok": true, "model": model.ID, "features": list, "available": len(list) > 0}
}

// handleCommission: GET the switches; POST {feature, on, password, note}. Switching on needs the panel
// password and every requirement switched on already; switching off also switches off what depends on it.
// The panel then restarts so the whole panel runs with one consistent set.
func handleCommission(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, commissionView())
		return
	}
	if publicBuild {
		writeJSON(w, map[string]any{"ok": false, "err": "this release has a fixed set of features: nothing to switch"})
		return
	}
	var in struct {
		Feature  string `json:"feature"`
		On       bool   `json:"on"`
		Password string `json:"password"`
		Note     string `json:"note"`
	}
	if !decodeIn(w, r, &in) {
		return
	}
	f := commissionFind(model.ID, in.Feature)
	if f == nil {
		writeJSON(w, map[string]any{"ok": false, "err": fmt.Sprintf("%q can't be switched on for this model (%s)", in.Feature, model.Name)})
		return
	}
	if len(in.Note) > 500 {
		in.Note = in.Note[:500]
	}
	commissionMu.Lock()
	st := loadCommission()
	st.Model = model.ID
	if in.On {
		for _, req := range f.Requires {
			if !st.Features[req].On {
				commissionMu.Unlock()
				writeJSON(w, map[string]any{"ok": false, "err": fmt.Sprintf("%s needs %s switched on first", f.Label, commissionFind(model.ID, req).Label)})
				return
			}
		}
		commissionMu.Unlock()
		if !checkPassword(in.Password) { // outside the lock: the hash is slow
			time.Sleep(600 * time.Millisecond)
			writeJSON(w, map[string]any{"ok": false, "err": "wrong password"})
			return
		}
		commissionMu.Lock()
		st = loadCommission()
		st.Model = model.ID
		st.Features[f.Key] = commissionEntry{On: true, At: time.Now().UTC().Format(time.RFC3339), Note: strings.TrimSpace(in.Note)}
	} else {
		// a run that owns the clock (tuner, ceiling probe, strong-chip test) must end first: it puts the miner
		// back itself, which the restart below would cut short
		why := hwRunActive()
		if why == "" && model.Plan == planBox2V && box2SoftIdled() {
			why = "the miner is soft-idled (a guard or a schedule stopped hashing): let it resume first"
		}
		if why != "" {
			commissionMu.Unlock()
			writeJSON(w, map[string]any{"ok": false, "err": "not switched off: " + why})
			return
		}
		// off: this feature and everything that (transitively) needs it
		want := map[string]bool{}
		for k, e := range st.Features {
			want[k] = e.On && k != f.Key
		}
		keep := commissionClosure(model.ID, want)
		for k, e := range st.Features {
			if e.On && !keep[k] {
				st.Features[k] = commissionEntry{On: false, Note: e.Note}
			}
		}
		st.Features[f.Key] = commissionEntry{On: false, Note: st.Features[f.Key].Note}
	}
	b, _ := json.MarshalIndent(st, "", " ")
	err := cfgWrite(commissionPath(), b, 0o600)
	commissionMu.Unlock()
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "err": "could not save: " + err.Error()})
		return
	}
	state := map[bool]string{true: "on", false: "off"}[in.On]
	activity.emit(fmt.Sprintf("🧪 commissioning: %s switched %s (panel restarting to apply)", f.Label, state))
	writeJSON(w, map[string]any{"ok": true, "restarting": true})
	if fl, ok := w.(http.Flusher); ok {
		fl.Flush()
	}
	go commissionRestart()
}

// commissionSig names the commissioned features this panel run applied ("none" when none; "" on a model with
// nothing to commission, so the SC Lite's replies are unchanged). /api/model and /api/dashboard both carry it:
// an open page whose model profile was read before the panel restarted with other switches sees the two
// differ and reloads, so every feature gate on the page follows the running set.
func commissionSig() string {
	commissionMu.Lock()
	defer commissionMu.Unlock()
	if len(commissionable[model.ID]) == 0 {
		return ""
	}
	var on []string
	for _, f := range commissionable[model.ID] {
		if commissionApplied[f.Key] {
			on = append(on, f.Key)
		}
	}
	if len(on) == 0 {
		return "none"
	}
	return strings.Join(on, ",")
}
