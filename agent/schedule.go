package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Weekly schedule (blake2b-asic-control parity). A schedule is a list of changes ("at 07:00 on these days,
// switch to this preset"); the preset stays on until the next change comes round (it carries over midnight).
// The week view on the page is drawn from these rules. The unpainted time follows Base: a preset, or
// "__back" = nothing scheduled (after a painted stretch the miner goes back to what it ran before it).
// Restarts are separate: a repeat (every hour … every 3 months) and/or marks at block starts. Anything that
// comes due while the tuner holds charge is skipped. Taking manual control pauses the preset changes.

const (
	blocksPerDay = 48 // 30-minute blocks (the week view)
	schedBack    = "__back"
	weekMin      = 7 * 1440
)

type schedRule struct {
	Start  string `json:"start"`  // "HH:MM" app-local time
	Days   []int  `json:"days"`   // Mon=0 … Sun=6
	Preset string `json:"preset"` // preset name, "Idle", "Stock" or "__back"
}

type schedRestart struct {
	Every  string `json:"every"`            // off | hour | 6h | 12h | day | week | month | 3months
	At     string `json:"at,omitempty"`     // "HH:MM" (hour: only the minutes count)
	Day    int    `json:"day,omitempty"`    // week: Mon=0 … Sun=6
	Mday   int    `json:"mday,omitempty"`   // month / 3months: 1–28
	Anchor *int   `json:"anchor,omitempty"` // 3months: year*12+month of the first one
	Marks  []int  `json:"marks"`            // restart marks: minutes of the week (block starts), repeat weekly
}

type schedPause struct {
	Reason string `json:"reason"`
	At     int64  `json:"at"`
}

type schedEvent struct {
	Msg string `json:"msg"`
	At  int64  `json:"at"`
}

type schedule struct {
	Enabled     bool         `json:"enabled"`
	Rules       []schedRule  `json:"rules"`
	Base        string       `json:"base"`
	Restart     schedRestart `json:"restart"`
	Paused      *schedPause  `json:"paused,omitempty"`
	LastRestart *schedEvent  `json:"restart_last,omitempty"`
	Error       string       `json:"error,omitempty"`

	// legacy grid format (read once and migrated to rules)
	Grid          *[7][blocksPerDay]string `json:"grid,omitempty"`
	Rest          string                   `json:"rest,omitempty"`
	Restarts      *[7][blocksPerDay]bool   `json:"restarts,omitempty"`
	RestartRepeat string                   `json:"restart_repeat,omitempty"`
	DailyT        string                   `json:"daily_t,omitempty"`
}

var (
	schedMu   sync.Mutex
	schedPath = "" // set in initSchedule from the sclite config dir

	schedRunMu    sync.Mutex
	lastSchKey    = -1  // minute-of-week occurrence of the change last applied (-1 = apply what's in force)
	lastSchName   = ""  // last scheduled preset we applied
	lastRstMinute int64 // unix minute of the last scheduled restart (never twice in one minute)
	schedSince    int64 // when the change in force was applied
	schedBefore   *powerplan
	schedBeforeBy string

	schedNow = time.Now // the schedule's clock (tests replace it)
)

func schedulePath() string {
	if schedPath != "" {
		return schedPath
	}
	return filepath.Join(filepath.Dir(*authFile), "schedule.json")
}

func loadSchedule() schedule {
	var s schedule
	b, err := os.ReadFile(schedulePath())
	if err == nil {
		json.Unmarshal(b, &s)
	}
	migrateSchedule(&s)
	if s.Restart.Every == "" {
		s.Restart.Every = "off"
	}
	// restart marks as the page saves them (block starts, an hour apart, at most 168): a hand-edited or damaged
	// file mustn't restart mining every minute
	marks := []int{}
	sort.Ints(s.Restart.Marks)
	for _, m := range s.Restart.Marks {
		if m >= 0 && m < weekMin && m%30 == 0 && (len(marks) == 0 || m-marks[len(marks)-1] >= 60) && len(marks) < 168 {
			marks = append(marks, m)
		}
	}
	s.Restart.Marks = marks
	if s.Rules == nil {
		s.Rules = []schedRule{}
	}
	if s.Base == "" {
		s.Base = schedBack
	}
	return s
}

// migrateSchedule turns the old 7×48 grid into change rules (a change wherever a block differs from the one
// before it, the week wrapping round) and the old per-block restart marks into minutes of the week.
func migrateSchedule(s *schedule) {
	if s.Grid != nil && len(s.Rules) == 0 {
		base := s.Rest
		if base == "" {
			base = schedBack
		}
		flat := make([]string, 0, 7*blocksPerDay)
		for d := 0; d < 7; d++ {
			for k := 0; k < blocksPerDay; k++ {
				v := s.Grid[d][k]
				if v == "" {
					v = base
				}
				flat = append(flat, v)
			}
		}
		groups := map[string]*schedRule{}
		order := []string{}
		n := len(flat)
		allSame := true
		for _, v := range flat {
			if v != flat[0] {
				allSame = false
			}
		}
		if allSame && flat[0] != schedBack {
			s.Rules = []schedRule{{Start: "00:00", Days: []int{0}, Preset: flat[0]}}
		} else if !allSame {
			for w := 0; w < n; w++ {
				if flat[w] == flat[(w-1+n)%n] {
					continue
				}
				m := (w % blocksPerDay) * 30
				key := fmt.Sprintf("%d|%s", m, flat[w])
				if groups[key] == nil {
					groups[key] = &schedRule{Start: fmt.Sprintf("%02d:%02d", m/60, m%60), Preset: flat[w]}
					order = append(order, key)
				}
				groups[key].Days = append(groups[key].Days, w/blocksPerDay)
			}
			for _, k := range order {
				s.Rules = append(s.Rules, *groups[k])
			}
			sort.SliceStable(s.Rules, func(i, j int) bool { return parseHM(s.Rules[i].Start) < parseHM(s.Rules[j].Start) })
		}
		s.Base = base
	}
	if s.Restarts != nil && len(s.Restart.Marks) == 0 {
		for d := 0; d < 7; d++ {
			for k := 0; k < blocksPerDay; k++ {
				if s.Restarts[d][k] {
					s.Restart.Marks = append(s.Restart.Marks, d*1440+k*30)
				}
			}
		}
	}
	if s.Restart.Every == "" && (s.RestartRepeat == "daily" || s.RestartRepeat == "weekdays") && parseHM(s.DailyT) >= 0 {
		s.Restart.Every, s.Restart.At = "day", s.DailyT
	}
	s.Grid, s.Restarts, s.Rest, s.RestartRepeat, s.DailyT = nil, nil, "", "", ""
}

// parseHM turns "HH:MM" into minutes-since-midnight, or -1 if it can't be parsed.
func parseHM(s string) int {
	var h, m int
	if len(s) != 5 {
		return -1
	}
	if _, err := fmt.Sscanf(s, "%d:%d", &h, &m); err != nil {
		return -1
	}
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return -1
	}
	return h*60 + m
}

// applyStockPlan loads the firmware's own stock powerplan (the selected level, ignoring any manual
// override) and applies it live. Used as the scheduler's "Stock" brush so the base clock/voltage is
// always the factory setting, never a saved custom preset.
func applyStockPlan() error { return applyStockPlanGen(noGen) }

// applyStockPlanGen: applyStockPlan for a caller that checked who's in charge (gen: safetyGen then). It
// applies the firmware's own selected level (wantIdx = -1).
func applyStockPlanGen(gen int64) error { return applyStockIndexGen(gen, -1, "stock", "stock") }

// applyStockIndexGen selects a firmware stock plan the way Controls' stock preset does it (manual off) — the
// only write that also wakes the miner from idle, which a manual plan with the stock values doesn't. wantIdx
// picks a specific plan position; wantIdx<0 uses the firmware's own selected level (its idle level falls back
// to the first hashing plan). chargeTag/markName label who is in charge. On Box II this is the one path that
// switches between the two stock modes: a bare select-by-index with manual:false, exactly as the hard rules
// require (no manual/voltage/clock write). Fans are kept up (never pulled down while hot or below the floor).
func applyStockIndexGen(gen int64, wantIdx int, chargeTag, markName string) error {
	if err := miningHoldErr(); err != nil {
		return err
	}
	if err := heatCoolErr(); err != nil {
		return err
	}
	// lane B3: re-enable (pgaenable) any boards a Box II soft-idle guard powered down; no-op on SC Lite. Refused
	// while a guard parks or holds the miner, or when a safety stop came after gen was noted.
	if err := box2WakeResume(gen); err != nil {
		return err
	}
	// the firmware's own stock plan, selected the way Controls' stock preset does it (manual off): this is
	// also what wakes the miner from idle, which a manual plan with the stock values doesn't
	raw, err := stockGet("/mcb/setting")
	if err != nil {
		return err
	}
	var cur map[string]any
	if err := json.Unmarshal(raw, &cur); err != nil {
		return err
	}
	i := wantIdx
	if i < 0 && box2DefaultIndex(cur) >= 0 {
		i = box2DefaultIndex(cur) // SC Box II: "Stock" is the factory default plan, not whichever mode is selected
	}
	if i < 0 {
		i = stockSelect(cur)
		if pp, ok := stockPlanAt(cur, i); !ok || pp.FreqMHz <= 0 { // idle selected: the firmware's hashing plan
			i = firstHashingPlan(cur)
		}
	}
	pp, ok := stockPlanAt(cur, i)
	if i < 0 || !ok || pp.FreqMHz <= 0 {
		return fmt.Errorf("stock powerplan not available from firmware")
	}
	if err := plusTrimErr(pp.FreqMHz); err != nil {
		return err
	}
	if (heatGuardActive() && !minerIdle(cur)) || min(pp.Fan1, pp.Fan2) < fanFloor() {
		// the firmware level would drop the fans to its own 50 % (or under the raised floor): its plan as a manual one, fans kept up
		if _, err := mutatePlanGen(gen, func(x *powerplan) { x.FreqMHz, x.VrunMv, x.PVMv = pp.FreqMHz, pp.VrunMv, pp.PVMv }); err != nil {
			return fmt.Errorf("apply failed: %v", err)
		}
		if !setChargeGen(gen, chargeTag) {
			return errSafetyStop
		}
		recordMark("P", markName)
		return nil
	}
	cur["manual"] = false
	cur["select"] = float64(i)
	body, _ := json.Marshal(cur)
	if err := putSettingGen(gen, body); err != nil { // (read above, outside planMu: the holds again, under it)
		return fmt.Errorf("apply failed: %v", err)
	}
	if !setChargeGen(gen, chargeTag) {
		return errSafetyStop // written, but a safety stop's Idle landed after it
	}
	recordMark("P", markName)
	return nil
}

// applyBox2ModeGen selects one of the two Box II stock modes (Hashrate / Low-power) by its position in the
// firmware's own plan list, with manual:false — the panel never writes a clock or voltage on this model. A
// name that is not one of the stock modes is refused (not approximated), so a scheduled or applied clock/
// voltage preset that doesn't exist on Box II never reaches the miner.
func applyBox2ModeGen(name string, gen int64) error {
	raw, err := stockGet("/mcb/setting")
	if err != nil {
		return err
	}
	var cur map[string]any
	if err := json.Unmarshal(raw, &cur); err != nil {
		return err
	}
	for _, m := range box2ModesFrom(cur) {
		if strings.EqualFold(m.Name, name) {
			return applyStockIndexGen(gen, m.Index, "preset:"+m.Name, "preset "+presetLabel(m.Name))
		}
	}
	return fmt.Errorf("refused: %q is not one of the two stock modes (%s / %s) on %s", name, box2StockHigh, box2StockLow, model.Name)
}

func saveSchedule(s schedule) error {
	schedMu.Lock()
	defer schedMu.Unlock()
	return saveScheduleLocked(s)
}

func saveScheduleLocked(s schedule) error {
	b, _ := json.MarshalIndent(s, "", " ")
	_ = os.MkdirAll(filepath.Dir(schedulePath()), 0755)
	return writeAtomic(schedulePath(), b, 0600)
}

// updateSchedule: change a few fields of the schedule as it is on disk now (the tick's notes, a pause). A whole
// schedule loaded earlier must not be saved back: the owner may have saved a new one meanwhile.
func updateSchedule(f func(s *schedule)) error {
	schedMu.Lock()
	defer schedMu.Unlock()
	s := loadSchedule()
	f(&s)
	return saveScheduleLocked(s)
}

// What the miner ran before a painted stretch, kept on disk while the stretch lasts: a panel restarted inside
// it otherwise took the stretch's own setting (an Idle) for "before", and "back to before" kept it for good.
func schedBeforePath() string {
	return filepath.Join(filepath.Dir(schedulePath()), "schedule-before.json")
}

type schedBeforeRec struct {
	Before powerplan `json:"before"`
	By     string    `json:"by"`
	Name   string    `json:"name"` // the painted change applied
}

func saveSchedBefore(name string) {
	if schedBefore == nil {
		return
	}
	b, _ := json.Marshal(schedBeforeRec{*schedBefore, schedBeforeBy, name})
	writeAtomic(schedBeforePath(), b, 0600)
}

// loadSchedBefore: once, at the first tick after the start.
func loadSchedBefore() {
	b, err := os.ReadFile(schedBeforePath())
	if err != nil {
		return
	}
	var r schedBeforeRec
	if json.Unmarshal(b, &r) != nil || r.Name == "" || r.Name == schedBack {
		os.Remove(schedBeforePath())
		return
	}
	schedBefore, schedBeforeBy, lastSchName = &r.Before, r.By, r.Name
}

// dayBlock returns the Mon=0..Sun=6 day index and the 0..47 half-hour block for a local time.
func dayBlock(t time.Time) (int, int) {
	day := (int(t.Weekday()) + 6) % 7
	block := t.Hour() * 2
	if t.Minute() >= 30 {
		block++
	}
	return day, block
}

func minuteOfWeek(t time.Time) int {
	day := (int(t.Weekday()) + 6) % 7
	return day*1440 + t.Hour()*60 + t.Minute()
}

// applyPresetName applies a saved preset (or "Idle" / "Stock") to the miner and takes charge. Reused by
// the Presets tab and the scheduler so both go through exactly the same safe path.
func applyPresetName(name string) error { return applyPresetNameGen(name, noGen) }

// applyPresetNameGen: applyPresetName for a caller that checked who's in charge (gen: safetyGen then).
func applyPresetNameGen(name string, gen int64) error {
	if name == "Idle" || name == "idle" {
		safetyStop() // a run's write on its way (limit probe, fan test put-back) can't land over this Idle
		// idleAction is the model's park: the firmware idle plan on SC Lite, the miner-API board-off on
		// Box II. Box II has no stock idle plan, so if its stop is unproven/absent idleAction errors here and
		// the scheduler reports it (retrying each minute) rather than mis-writing; the thermal/pool guards then
		// fall back to the firmware's own 86 °C cut-off, as the design's idle-gap note requires.
		if err := idleAction(model); err != nil {
			return err
		}
		setCharge("idle")
		recordMark("P", "idle")
		return nil
	}
	if name == "Stock" || name == "stock" {
		return applyStockPlanGen(gen)
	}
	// Box II: the only presets are the two stock modes, selected by index. Anything else (a custom/tuner
	// preset name that doesn't exist on this model) is refused, never approximated into a plan write.
	if model.Plan == planBox2V {
		if model.Feat.ClockControl && !isBox2ModeName(name) {
			return applyBox2PresetGen(name, gen) // lane C3: the owner's saved clock preset
		}
		return applyBox2ModeGen(name, gen)
	}
	if err := miningHoldErr(); err != nil {
		return err
	}
	if err := heatCoolErr(); err != nil {
		return err
	}
	var found *preset
	for _, p := range loadPresets() {
		if p.Name == name {
			pp := p
			found = &pp
			break
		}
	}
	if found == nil {
		return fmt.Errorf("preset %q not found", name)
	}
	p := powerplan{FreqMHz: found.MHz, VrunMv: found.MV, PVMv: found.PV, Fan1: 50, Fan2: 50}
	if err := validateControlEx(p, loadOverclock().Acknowledged); err != nil { // the window the owner has unlocked
		return fmt.Errorf("preset out of safe range: %v", err)
	}
	if _, err := mutatePlanGen(gen, func(pp *powerplan) { pp.FreqMHz = found.MHz; pp.VrunMv = found.MV; pp.PVMv = found.PV }); err != nil {
		return fmt.Errorf("apply failed: %v", err)
	}
	applyPresetFan(*found)
	if !setChargeGen(gen, "preset:"+found.Name) {
		return errSafetyStop // written, but a safety stop's Idle landed after it
	}
	recordMark("P", "preset "+found.Name)
	return nil
}

// schedHasBack: a change goes "back to what it ran before".
func schedHasBack(rules []schedRule) bool {
	for _, r := range rules {
		if r.Preset == schedBack {
			return true
		}
	}
	return false
}

// presetExists: a preset name the schedule may switch to. On Box II the only valid changes are the two stock
// modes (Hashrate / Low-power), Idle, Stock and "back" — a custom/tuner clock-voltage preset name does not
// exist on this model, so validateSchedule refuses to schedule it.
func presetExists(name string) bool {
	if name == "Idle" || name == "Stock" || name == schedBack {
		return true
	}
	if model.Plan == planBox2V {
		return isBox2ModeName(name) || box2SavedPreset(name) != nil
	}
	for _, p := range loadPresets() {
		if p.Name == name {
			return true
		}
	}
	return false
}

// occurrences: every change of the week as (minute of week, rule index), sorted.
type schedOcc struct{ m, i int }

func scheduleOccs(rules []schedRule) []schedOcc {
	var o []schedOcc
	for i, r := range rules {
		hm := parseHM(r.Start)
		if hm < 0 {
			continue
		}
		for _, d := range r.Days {
			if d >= 0 && d < 7 {
				o = append(o, schedOcc{d*1440 + hm, i})
			}
		}
	}
	sort.Slice(o, func(a, b int) bool { return o[a].m < o[b].m })
	return o
}

// inForce returns the change in force at a minute of the week (the last one at or before it, wrapping round).
func inForce(o []schedOcc, mow int) (schedOcc, bool) {
	if len(o) == 0 {
		return schedOcc{}, false
	}
	cur := o[len(o)-1]
	for _, x := range o {
		if x.m <= mow {
			cur = x
		}
	}
	return cur, true
}

// nextChange returns the next change after a minute of the week and how many minutes until it.
func nextChange(o []schedOcc, mow int) (schedOcc, int, bool) {
	if len(o) == 0 {
		return schedOcc{}, 0, false
	}
	for _, x := range o {
		if x.m > mow {
			return x, x.m - mow, true
		}
	}
	return o[0], o[0].m + weekMin - mow, true
}

// restartDue: is a scheduled restart due at this exact local minute?
func restartDue(r schedRestart, t time.Time) bool { return restartDueAt(r, t, parseHM(r.At)) }

// restartDueAt: restartDue with r.At already parsed (nextRestart asks it for up to 100 days of minutes).
func restartDueAt(r schedRestart, t time.Time, at int) bool {
	if len(r.Marks) > 0 {
		mow := minuteOfWeek(t)
		for _, m := range r.Marks {
			if m == mow {
				return true
			}
		}
	}
	if r.Every == "off" || r.Every == "" || at < 0 {
		return false
	}
	hh, mm := at/60, at%60
	h, m, _ := t.Clock()
	if m != mm {
		return false
	}
	switch r.Every {
	case "hour":
		return m == mm
	case "6h", "12h":
		st := 6
		if r.Every == "12h" {
			st = 12
		}
		return m == mm && ((h-hh)%st+st)%st == 0
	case "day":
		return h == hh && m == mm
	case "week":
		return (int(t.Weekday())+6)%7 == r.Day && h == hh && m == mm
	case "month", "3months":
		if t.Day() != r.Mday || h != hh || m != mm {
			return false
		}
		if r.Every == "3months" && r.Anchor != nil {
			mon := t.Year()*12 + int(t.Month()) - 1
			return ((mon-*r.Anchor)%3+3)%3 == 0
		}
		return true
	}
	return false
}

// wallMinute: t's wall-clock minute as a UTC time (the fields restartDue reads, with no zone changes).
func wallMinute(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), 0, 0, time.UTC)
}

// wallRepeat: the clocks went back (fall back) and t's wall-clock minute already came round before, an offset
// change ago.
func wallRepeat(t time.Time) bool {
	_, off := t.Zone()
	_, offE := t.Add(-3 * time.Hour).Zone()
	if offE <= off {
		return false
	}
	return wallMinute(t.Add(-time.Duration(offE-off) * time.Second)).Equal(wallMinute(t))
}

// schedRestartDue: restartDue for the minute ticks, by the wall clock across a DST change: a restart in the hour
// the clocks go back is made once (the hourly one goes on by the real hour), and one in the hour the clocks skip
// is made in the first minute after it.
func schedRestartDue(r schedRestart, now time.Time, repeat bool) bool {
	hourly := r.Every == "hour" && parseHM(r.At) >= 0 && now.Minute() == parseHM(r.At)%60
	if repeat {
		return hourly
	}
	if restartDue(r, now) {
		return true
	}
	prev := now.Add(-time.Minute)
	_, op := prev.Zone()
	if _, on := now.Zone(); op < on { // the first minute after the clocks went forward
		rr := r
		if rr.Every == "hour" {
			rr.Every = "off" // by the real hour: an hour after the last one, not caught up
		}
		for w := wallMinute(prev).Add(time.Minute); w.Before(wallMinute(now)); w = w.Add(time.Minute) {
			if restartDue(rr, w) {
				return true
			}
		}
	}
	return false
}

// nextRestart scans forward minute by minute (up to ~100 days) for the next scheduled restart.
func nextRestart(r schedRestart, from time.Time) (time.Time, bool) {
	if (r.Every == "off" || r.Every == "") && len(r.Marks) == 0 {
		return time.Time{}, false
	}
	t := from.Truncate(time.Minute).Add(time.Minute)
	at := parseHM(r.At)
	for i := 0; i < 100*1440; i++ {
		if restartDueAt(r, t, at) {
			return t, true
		}
		t = t.Add(time.Minute)
	}
	return time.Time{}, false
}

var weekdays = []string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"}

func restartLine(r schedRestart) string {
	bits := []string{}
	switch r.Every {
	case "hour":
		at := parseHM(r.At)
		bits = append(bits, fmt.Sprintf("every hour at :%02d", at%60))
	case "6h", "12h":
		bits = append(bits, fmt.Sprintf("every %s from %s", strings.TrimSuffix(r.Every, "h")+" hours", r.At))
	case "day":
		bits = append(bits, "every day at "+r.At)
	case "week":
		d := r.Day
		if d < 0 || d > 6 {
			d = 0
		}
		bits = append(bits, "every "+weekdays[d]+" at "+r.At)
	case "month":
		bits = append(bits, fmt.Sprintf("on day %d of every month at %s", r.Mday, r.At))
	case "3months":
		bits = append(bits, fmt.Sprintf("on day %d every 3 months at %s", r.Mday, r.At))
	}
	if n := len(r.Marks); n > 0 {
		bits = append(bits, fmt.Sprintf("%d marked on the week", n))
	}
	return strings.Join(bits, " · ")
}

// scheduleTick runs once a minute: applies the change in force when it changes (or when the schedule was
// just saved / resumed), and fires a scheduled restart when one is due — unless the tuner is in charge.
func scheduleTick() {
	schedRunMu.Lock()
	defer schedRunMu.Unlock()
	controlMu.Lock() // the "tuning" check below and the change it allows, in one go
	defer controlMu.Unlock()
	now := schedNow()
	if now.Year() < 2024 { // a 1970 clock (before NTP) would put the wrong preset in charge (every caller: the loop, a save, a resume)
		return
	}
	s := loadSchedule()
	repeat := wallRepeat(now) // the clocks went back: this wall-clock minute already came round once
	// (held: no restarts either; the strong-chip test: its clock change would end it, so the change waits)
	// (and an Idle the thermal guard took: nothing wakes it but the owner, until it has run cool again)
	gen := safetyGen.Load() // noted with the checks: a safety stop after this wins over the change (see safetyGen)
	tuning := getCharge() == "tuner" || getCharge() == "ceiling" || poolHoldErr() != nil || sctRunning() || heatIdleHeld()

	// restarts: independent of the preset changes (they work even without presets)
	if !tuning && schedRestartDue(s.Restart, now, repeat) {
		um := now.Unix() / 60
		if um != lastRstMinute {
			lastRstMinute = um
			ev := &schedEvent{At: now.Unix()}
			if err := softRestartMining(); err == nil {
				recordMark("R", "scheduled restart")
				markReboot(150*time.Second, 5*time.Minute, "Scheduled restart: the miner reboots — it will be back in a minute or two.")
				activity.emit("⏱ schedule: restarting mining (the miner reboots)")
				ev.Msg = "restarted"
			} else {
				ev.Msg = "restart failed: " + err.Error()
			}
			s.LastRestart = ev
			updateSchedule(func(d *schedule) { d.LastRestart = ev })
		}
	}

	if !s.Enabled || s.Paused != nil || tuning {
		if !s.Enabled {
			lastSchKey = -1
			if lastSchName != "" || schedBefore != nil { // off: a later "on" starts afresh, not from a stretch long gone
				lastSchName, schedBefore, schedBeforeBy = "", nil, ""
				os.Remove(schedBeforePath())
			}
		}
		return
	}
	if lastSchName == "" && schedBefore == nil {
		loadSchedBefore() // a restart inside a painted stretch: what it ran before it
	}
	if repeat && lastSchKey != -1 {
		return // every change in this hour was made the first time round
	}
	o := scheduleOccs(s.Rules)
	cur, ok := inForce(o, minuteOfWeek(now))
	if !ok || cur.m == lastSchKey {
		return
	}
	target := s.Rules[cur.i].Preset
	if target == schedBack {
		// nothing scheduled here: go back to what the miner ran before the painted stretch
		if schedBefore != nil && lastSchName != "" && lastSchName != schedBack {
			b := *schedBefore
			var err error
			by := schedBeforeBy
			switch {
			case b.FreqMHz == 0 || by == "idle": // it was on Idle: Idle again (not a made-up 0 MHz plan)
				err = applyPresetNameGen("Idle", gen)
				by = "idle"
			default:
				_, err = mutatePlanGen(gen, func(x *powerplan) { x.FreqMHz = b.FreqMHz; x.VrunMv = b.VrunMv; x.PVMv = b.PVMv })
			}
			if err != nil {
				if e := "going back failed: " + err.Error(); s.Error != e { // said once, not every minute
					s.Error = e
					updateSchedule(func(d *schedule) { d.Error = e })
					activity.emit("⏱ schedule: " + e)
				}
				return
			}
			if by == "ceiling" || by == "tuner" { // that run is over: nothing is in charge now
				by = ""
			}
			if by != "idle" { // (the Idle set it itself)
				setChargeGen(gen, by)
			}
			os.Remove(schedBeforePath())
			recordMark("P", "schedule: back to before")
			activity.emit("⏱ schedule: back to what it ran before")
		}
		lastSchKey, lastSchName, schedSince = cur.m, schedBack, now.Unix()
		if s.Error != "" {
			updateSchedule(func(d *schedule) { d.Error = "" })
		}
		return
	}
	if lastSchName == "" || lastSchName == schedBack {
		// leaving unscheduled time: remember what it ran so "back" can restore it
		if cp, ok := readStockBaseline("current"); ok {
			cp.FreqMHz = throttleUnder(cp.FreqMHz) // the thermal throttle's step isn't what it ran: going back to it would keep it for good
			schedBefore, schedBeforeBy = &cp, getCharge()
		} else if schedHasBack(s.Rules) {
			// not noted, this change would be applied with nothing to go back to at the end of the stretch: wait
			if e := "could not read what the miner runs now (to go back to it after " + presetLabel(target) + ")"; s.Error != e {
				updateSchedule(func(d *schedule) { d.Error = e })
			}
			return // retried next minute
		}
	}
	if target == lastSchName && getCharge() == "preset:"+target {
		lastSchKey = cur.m // already on it: nothing to apply or announce
		return
	}
	if err := applyPresetNameGen(target, gen); err != nil {
		if e := err.Error(); s.Error != e { // retried every minute, but saved and said only when the reason changes
			updateSchedule(func(d *schedule) { d.Error = e })
			activity.emit("⏱ schedule: could not apply " + presetLabel(target) + " — " + err.Error())
		}
		return // retried next minute
	}
	lastSchKey, lastSchName, schedSince = cur.m, target, now.Unix()
	saveSchedBefore(target)
	if s.Error != "" {
		updateSchedule(func(d *schedule) { d.Error = "" })
	}
	activity.emit("⏱ schedule: switched to " + presetLabel(target))
	notify(evSchedule, "schedule: switched to "+presetLabel(target))
}

// pauseScheduleFor pauses the preset changes (Take manual control). The restarts keep running.
func pauseScheduleFor(reason string) {
	s := loadSchedule()
	if !s.Enabled || s.Paused != nil {
		return
	}
	updateSchedule(func(s *schedule) {
		if s.Enabled && s.Paused == nil {
			s.Paused = &schedPause{Reason: reason, At: time.Now().Unix()}
		}
	})
}

func scheduleLoop() {
	time.Sleep(time.Duration(60-time.Now().Second()) * time.Second)
	tick := func() {
		if clockOK() { // a 1970 clock (before NTP) would put the wrong preset in charge
			scheduleTick()
		}
	}
	tick()
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for range t.C {
		tick()
	}
}

func validateSchedule(s *schedule) error {
	seen := map[string]bool{}
	for i := range s.Rules {
		r := &s.Rules[i]
		if parseHM(r.Start) < 0 {
			return fmt.Errorf("every change needs a time")
		}
		if len(r.Days) == 0 {
			return fmt.Errorf("the %s change needs at least one day", r.Start)
		}
		sort.Ints(r.Days)
		for _, d := range r.Days {
			if d < 0 || d > 6 {
				return fmt.Errorf("bad day in the %s change", r.Start)
			}
			k := fmt.Sprintf("%d|%s", d, r.Start)
			if seen[k] {
				return fmt.Errorf("two changes at %s on %s", r.Start, weekdays[d])
			}
			seen[k] = true
		}
		if !presetExists(r.Preset) {
			return fmt.Errorf("preset %q for the %s change doesn't exist", r.Preset, r.Start)
		}
	}
	if s.Enabled && len(s.Rules) == 0 {
		return fmt.Errorf("add at least one change before turning the schedule on")
	}
	if s.Base == "" {
		s.Base = schedBack
	}
	if !presetExists(s.Base) {
		return fmt.Errorf("the rest of the time: preset %q doesn't exist", s.Base)
	}
	r := &s.Restart
	switch r.Every {
	case "", "off":
		r.Every = "off"
	case "hour", "6h", "12h", "day", "week", "month", "3months":
		if parseHM(r.At) < 0 {
			return fmt.Errorf("set a time for the restart")
		}
		if r.Every == "week" && (r.Day < 0 || r.Day > 6) {
			return fmt.Errorf("pick a day for the weekly restart")
		}
		if (r.Every == "month" || r.Every == "3months") && (r.Mday < 1 || r.Mday > 28) {
			return fmt.Errorf("the restart's day of the month must be 1-28 (so it's in every month)")
		}
		if r.Every == "3months" && r.Anchor == nil {
			t := schedNow()
			if t.Year() < 2024 {
				return fmt.Errorf("the miner's clock isn't set yet (no time server reached): set the 3-monthly restart once it is")
			}
			a := t.Year()*12 + int(t.Month()) - 1
			if t.Day() > r.Mday {
				a++ // this month's day has passed: the first one is next month
			}
			r.Anchor = &a
		}
	default:
		return fmt.Errorf("unknown restart repeat %q", r.Every)
	}
	if r.Marks == nil {
		r.Marks = []int{}
	}
	if len(r.Marks) > 168 {
		return fmt.Errorf("at most 168 restart marks")
	}
	sort.Ints(r.Marks)
	for i, m := range r.Marks {
		if m < 0 || m >= weekMin || m%30 != 0 {
			return fmt.Errorf("bad restart mark")
		}
		if i > 0 && m-r.Marks[i-1] < 60 {
			return fmt.Errorf("restart marks must be at least an hour apart")
		}
	}
	return nil
}

func schedPresetView(p preset) map[string]any {
	return map[string]any{"key": p.Name, "label": p.Name, "mhz": p.MHz, "mv": p.MV, "pv": p.PV}
}

func handleSchedule(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" {
		var in struct {
			Action  string       `json:"action"` // save | pause
			Enabled bool         `json:"enabled"`
			Rules   []schedRule  `json:"rules"`
			Base    string       `json:"base"`
			Restart schedRestart `json:"restart"`
			Paused  bool         `json:"paused"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeJSON(w, map[string]any{"ok": false, "err": badJSON(err)})
			return
		}
		old := loadSchedule()
		if in.Action == "pause" {
			if err := updateSchedule(func(s *schedule) {
				if in.Paused {
					s.Paused = &schedPause{Reason: "paused by hand", At: time.Now().Unix()}
				} else {
					s.Paused = nil
				}
			}); err != nil {
				writeJSON(w, map[string]any{"ok": false, "err": "save failed: " + err.Error()})
				return
			}
			schedRunMu.Lock()
			lastSchKey = -1
			schedRunMu.Unlock()
			go scheduleTick()
			writeJSON(w, map[string]any{"ok": true})
			return
		}
		ns := schedule{Enabled: in.Enabled, Rules: in.Rules, Base: in.Base, Restart: in.Restart, LastRestart: old.LastRestart}
		if ns.Rules == nil {
			ns.Rules = []schedRule{}
		}
		// keep the 3-month anchor when the repeat didn't change
		if ns.Restart.Every == "3months" && old.Restart.Every == "3months" && old.Restart.Mday == ns.Restart.Mday {
			ns.Restart.Anchor = old.Restart.Anchor
		}
		if err := validateSchedule(&ns); err != nil {
			writeJSON(w, map[string]any{"ok": false, "err": err.Error()})
			return
		}
		if err := saveSchedule(ns); err != nil {
			writeJSON(w, map[string]any{"ok": false, "err": "save failed: " + err.Error()})
			return
		}
		schedRunMu.Lock()
		lastSchKey = -1 // apply the change in force now
		schedRunMu.Unlock()
		go scheduleTick()
		writeJSON(w, map[string]any{"ok": true, "schedule": scheduleView(ns)})
		return
	}
	s := loadSchedule()
	writeJSON(w, map[string]any{"ok": true, "schedule": scheduleView(s), "presets": schedulePresets(),
		"timezone": time.Local.String(), "utc_offset": time.Now().Format("-0700"),
		"active": chargeLabel(), "charge": getCharge(), "tuning": getCharge() == "tuner", "restart_s": 75})
}

// schedulePresets: the brushes — the tuner's four first, then the others, Stock, then Idle. On Box II the
// brushes are just the two stock modes (Hashrate / Low-power) and Idle: no tuner presets, no "Stock" brush
// (both brushes already are stock plans).
func schedulePresets() []map[string]any {
	if model.Plan == planBox2V {
		out := []map[string]any{}
		// "mv" carries the OEM's voltage LEVEL here (not millivolts; no PV), kept for the page; "level" says so
		for _, m := range box2Modes() {
			out = append(out, map[string]any{"key": m.Name, "label": presetLabel(m.Name), "mhz": m.Freq, "mv": m.V, "level": m.V, "stock": true})
		}
		for _, p := range box2SavedPresets() { // lane C3: none unless ClockControl is on
			out = append(out, map[string]any{"key": p.Name, "label": p.Name, "mhz": p.MHz, "mv": p.MV, "level": p.MV})
		}
		out = append(out, map[string]any{"key": "Idle", "label": "Idle", "idle": true})
		return out
	}
	ps := loadPresets()
	rank := func(n string) int {
		switch strings.ToLower(n) {
		case "high":
			return 0
		case "middle":
			return 1
		case "low":
			return 2
		case "lowest", "lowest power":
			return 3
		}
		return 4
	}
	sort.SliceStable(ps, func(i, j int) bool { return rank(ps[i].Name) < rank(ps[j].Name) })
	out := []map[string]any{}
	for _, p := range ps {
		out = append(out, schedPresetView(p))
	}
	if sp, ok := readStockBaseline("stock"); ok {
		out = append(out, map[string]any{"key": "Stock", "label": "Stock", "mhz": sp.FreqMHz, "mv": sp.VrunMv, "pv": sp.PVMv, "stock": true})
	} else {
		out = append(out, map[string]any{"key": "Stock", "label": "Stock", "stock": true})
	}
	out = append(out, map[string]any{"key": "Idle", "label": "Idle", "idle": true})
	return out
}

// scheduleView: the saved schedule plus what's in force, the next change and the next restart.
func scheduleView(s schedule) map[string]any {
	now := schedNow()
	mow := minuteOfWeek(now)
	v := map[string]any{"enabled": s.Enabled, "rules": s.Rules, "base": s.Base, "restart": s.Restart,
		"paused": s.Paused, "restart_last": s.LastRestart, "error": s.Error}
	o := scheduleOccs(s.Rules)
	if cur, ok := inForce(o, mow); ok {
		label := func(p string) string {
			if p == schedBack {
				return "nothing (back to what it ran before)"
			}
			return presetLabel(p)
		}
		v["now"] = label(s.Rules[cur.i].Preset)
		since := cur.m
		back := mow - since
		if back < 0 {
			back += weekMin
		}
		v["since"] = now.Truncate(time.Minute).Add(-time.Duration(back) * time.Minute).Unix()
		if nx, in, ok := nextChange(o, mow); ok {
			v["next"] = label(s.Rules[nx.i].Preset)
			v["next_at"] = now.Truncate(time.Minute).Add(time.Duration(in) * time.Minute).Unix()
		}
	}
	on := s.Restart.Every != "off" && s.Restart.Every != "" || len(s.Restart.Marks) > 0
	if on {
		v["restart_line"] = restartLine(s.Restart)
		if t, ok := nextRestart(s.Restart, now); ok {
			v["restart_next"] = t.Unix()
		}
	}
	return v
}
