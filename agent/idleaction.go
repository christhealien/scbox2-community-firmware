package main

// Box II stop-hashing action ("soft idle": lane B3, audited and completed by lane C1) — the "park the miner"
// step for the safety guards, the schedule's Idle and the Presets page's Idle on a model that has NO stock
// idle power-plan.
//
// Background. On the SC Lite the thermal guard / pool guard / fan lock park the miner by selecting the
// firmware's own idle plan (applyIdle: 0 MHz, boards powered down). The SC Box II stock plans have NO idle
// entry, so there is nothing to select. What Goldshell's own stack does instead (traced, lane C1):
//   - minerd's overheat path ("CPB%d Overheat, So Shutdown It") and its PUT /cpb/control board switch send
//     {"command":"pgadisable","parameter":"%d"} / pgaenable to the miner API (R13 box2_minerd 0x16a1e, 0x19146).
//   - In intminer (BFGMiner 5.4.2 + gsscbox2) pgadisable sets the board's disable flag; the board's mining
//     thread then runs gssb2_thread_disable, which logs "C0:  Device Deinit" and de-initialises the chain.
//     pgaenable runs gssb2_thread_enable = the full board init ("SCBOX2 device init…", "SCBOX2 Init sucessed").
//   - The API answers both with STATUS "I" (info), not "S": Code 64 "PGA 0 set disable flag", Code 63
//     "PGA 0 sent enable message", 61/62 "already enabled/disabled", and STATUS "E" Code 65 "PGA 0 is not
//     flagged WELL, cannot enable" (a board that is not Alive can't be re-enabled: only a miner restart does it).
//
// So the panel does what minerd does (pgadisable per board over the API), then CHECKS it:
//   - at once: `devs` must list every board "Enabled":"N" (the miner took the flag);
//   - then, from a watch loop every 10 s: the board's work counter ("Total MH") must stop growing within
//     box2ConfirmWindow, and its temperature must not keep rising. If it does not stop, the stop is reported as
//     IGNORED: fans to 100 % (through the stock plan), a notification, and Goldshell's 86 °C cut-off is the
//     backstop; the guards then see "not parked" and take their own fall-backs again;
//   - a mining program that restarts while idle (the stock launcher relaunches it; a fresh process has every
//     board enabled) is caught by the same loop ("Enabled":"Y" while the panel holds the board off) and the stop
//     is re-applied; across a reboot a small marker file (softidle.json) does the same;
//   - resume = pgaenable (the board re-inits); if the miner refuses (board not Alive) the mining program is
//     restarted instead, which starts every board fresh.
//
// Everything here is behind model.Feat.SoftIdle (commission key "SoftIdle", test C1), which ships OFF. With it
// off the guards never send the command; they fall back to fans 100 % + the 86 °C cut-off with a clear message.
// SC Lite is untouched: idleAction on a plan-based model is exactly applyIdle.

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// idleActionUnprovenMsg is the exact caveat the UI / notifications show while the Box II stop-hashing action
// has not been commissioned on this unit. Do not reword: RV11 greps for it.
const idleActionUnprovenMsg = "stop-hashing on this model is unverified; the 86°C cutoff is the guaranteed backstop"

// idleActionCheckedMsg: the note once the owner has proven and switched on the stop (SoftIdle on): the panel
// still checks every stop, and the cut-off stays the backstop.
const idleActionCheckedMsg = "the panel checks that hashing really stops; the 86°C cutoff stays the backstop"

// box2APICmd / box2APICmdParam are the cgminer :4028 API calls, held in vars so tests can mock the miner.
var (
	box2APICmd      = cgminerCmd
	box2APICmdParam = cgminerCmdParam
)

// Seams for tests: the clock, the fans-to-100 % fall-back, and the mining-program restart used when a board
// can't be re-enabled.
var (
	box2Now           = time.Now
	box2FansMax       = box2FansMaxPlan
	box2RestartMining = restartMiner
	// box2OverTemp: Goldshell's own over-temperature shutdown happened this boot (its fanctrl log). The panel
	// never re-enables a board after that: the cut-off keeps it off until a reboot, as on stock.
	box2OverTemp = func() bool { return overTempView() != nil }
)

// Timings of the watch (vars so tests can shorten them).
var (
	box2WatchEvery    = 10 * time.Second
	box2ConfirmWindow = 2 * time.Minute // the work counter must have stopped within this after a (re)apply
	box2StillTicks    = 2               // readings in a row with no new work = stopped
	box2CoolCheck     = 5 * time.Minute // after this the temperature is compared with the one at the stop
	box2ResumeWindow  = 5 * time.Minute // after a resume the board should hash again within this
)

// box2Disabled is true while the panel holds the boards off (pgadisable sent and accepted, not yet re-enabled):
// resumeAction then knows to pgaenable, and the watch keeps the stop applied. Never set on SC Lite or with
// SoftIdle off.
var box2Disabled atomic.Bool

// box2Mu serialises every soft-idle API sequence (stop, resume, the watch's re-apply). Always the innermost
// lock: nothing here takes planMu while holding it (the fans fall-back runs on its own goroutine).
var box2Mu sync.Mutex

// box2St is the soft-idle state the watch keeps (under box2Mu).
var box2St box2IdleState

// box2Parked mirrors "held off and (being) confirmed, not ignored" from box2St, so box2SoftIdled never waits
// for box2Mu (held across miner API calls) from planMu / the fan loop / the board watch.
var box2Parked atomic.Bool

// box2SyncParked: under box2Mu, after any change of box2St (deferred right after the Lock's defer).
func box2SyncParked() {
	box2Parked.Store((box2St.Phase == "stopping" || box2St.Phase == "stopped") && !box2St.Ignored)
}

type box2IdleState struct {
	Phase      string    // "" | "stopping" (sent, being confirmed) | "stopped" (confirmed) | "ignored" | "resuming"
	Since      time.Time // the stop that started this idle
	AppliedAt  time.Time // the last (re)apply
	Reapplied  int       // re-applies after the mining program restarted (or re-enabled the board) while idle
	Ignored    bool      // in this idle the miner kept hashing with the board disabled: not a trusted park
	Note       string    // the last thing the watch found, in plain words
	still      int
	lastWork   float64
	haveWork   bool
	TempStart  float64
	TempNow    float64
	confirmAt  time.Time
	coolDone   bool
	resumeWork float64
}

// ---- the model-aware idle entry points (the contract B1/B2 align to) ----

// idleAction parks the miner for a safety guard (thermal / pool / fan-lock), the schedule or the owner.
//
//	nil  => hashing was parked:
//	          SC Lite — the stock idle plan was applied (applyIdle), proven.
//	          Box II  — SoftIdle on, pgadisable accepted for every board and `devs` shows them disabled. The
//	                    watch then confirms the work counter stops (or reports it ignored and falls back).
//	err  => hashing could NOT be stopped (Box II with SoftIdle off, the stop failed, or the miner already ignored
//	          it in this idle). Before returning, the fans are pushed to 100 % and the 86 °C cut-off is relied on.
//	          Callers treat this as "not parked" and keep their own secondary measures.
func idleAction(m modelProfile) error { return idleActionThen(m, nil) }

// idleActionThen is idleAction with a callback run right after a SUCCESSFUL park. On the degrade path landed is
// NOT called (nothing was parked).
func idleActionThen(m modelProfile, landed func()) error {
	if m.Plan != planBox2V {
		return applyIdleThen(landed) // SC Lite and any plan-based model: unchanged, proven.
	}
	return box2Idle(m, landed)
}

func box2Idle(m modelProfile, landed func()) error {
	if !m.Feat.SoftIdle {
		// Shipped default: the stop is not commissioned on this unit. Do not send it; fall back.
		box2FansMax()
		box2DegradeLog("the stop-hashing action is not switched on for this unit (Settings → Commissioning, test C1)")
		return fmt.Errorf("the miner can't be parked on this model: no verified stop-hashing action — %s", idleActionUnprovenMsg)
	}
	if err := box2IdleLocked(); err != nil {
		return err
	}
	if landed != nil {
		landed() // outside box2Mu: the callback may take planMu (box2Mu is always the innermost lock)
	}
	return nil
}

func box2IdleLocked() error {
	box2Mu.Lock()
	defer box2Mu.Unlock()
	defer box2SyncParked()
	temp := box2MaxTempNow()
	stopped, total, err := box2StopVerified()
	if err != nil {
		box2FansMax()
		box2DegradeLog(fmt.Sprintf("%d of %d boards stopped: %v", stopped, total, err))
		return fmt.Errorf("stop-hashing failed (%d of %d boards stopped: %v) — fans forced to 100%%, %s", stopped, total, err, box2StopFailNote())
	}
	now := box2Now()
	wasIdle := box2Disabled.Load()
	box2Disabled.Store(true)
	if !wasIdle || box2St.Phase == "" || box2St.Phase == "resuming" {
		box2St = box2IdleState{Since: now, TempStart: temp}
		box2SaveMarker(now)
		activity.emit("⏸ stop-hashing sent (pgadisable): the board is disabled; checking that hashing stops")
	}
	if box2St.Ignored {
		// the miner already kept hashing with the board disabled in this idle: the flag is not a trusted park
		box2FansMax()
		box2DegradeLog("the miner kept hashing earlier in this idle although the board was disabled")
		return fmt.Errorf("the miner did not stop hashing when told to earlier — fans forced to 100%%, relying on the %.0f °C cut-off", model.TempCutoffC)
	}
	box2St.Phase, box2St.AppliedAt, box2St.still, box2St.haveWork = "stopping", now, 0, false
	return nil
}

// box2StopFailNote: the caveat after a failed stop. The owner's build keeps idleActionUnprovenMsg; the public
// release (where the stop is always on, having passed its test) has no "unverified": the cut-off is the backstop.
func box2StopFailNote() string {
	if publicBuild {
		return "the 86°C cutoff stays the backstop"
	}
	return idleActionUnprovenMsg
}

// resumeAction undoes the stop when a guard lifts and the owner starts mining again (a preset, Stock, the
// schedule, a manual control). No-op on SC Lite, with SoftIdle off, or when nothing is held off. Idempotent.
func resumeAction(m modelProfile) error {
	if m.Plan != planBox2V || !m.Feat.SoftIdle {
		return nil
	}
	box2Mu.Lock()
	defer box2Mu.Unlock()
	defer box2SyncParked()
	if !box2Disabled.Load() {
		return nil
	}
	// a safety guard is parking (from its safetyGen bump until its hold is in place) or holds the miner: never
	// undo its stop. heatCoolErr, not heatIdleHeld: once the cool-down has ended the owner's start wakes it.
	if box2GuardParking.Load() > 0 {
		return &resumeHeldErr{errors.New("a safety guard is putting the miner on Idle: the board stays off")}
	}
	if err := heatCoolErr(); err != nil {
		return &resumeHeldErr{err}
	}
	if err := miningHoldErr(); err != nil {
		return &resumeHeldErr{err}
	}
	if why := box2CutoffHolds(); why != "" {
		// never undo Goldshell's own heat shutdown: the board stays off (until a reboot), the idle stays held
		return fmt.Errorf("the board is not re-enabled: %s — it stays off until the miner reboots (%s cut-off)", why, oemPoss())
	}
	n, total, err := box2ResumeVerified()
	if err != nil {
		// pgaenable refused (Code 65: the board isn't Alive) or didn't take: a fresh mining program starts every
		// board enabled, so restart it (the stock launcher relaunches it within seconds).
		activity.emit(fmt.Sprintf("▶ resume: the miner did not re-enable the board (%d of %d: %v): restarting the mining program, which starts every board fresh", n, total, err))
		box2RestartMining()
	}
	box2Disabled.Store(false)
	box2RemoveMarker()
	box2St = box2IdleState{Phase: "resuming", AppliedAt: box2Now(), resumeWork: -1, Note: "resumed: waiting for the board to hash again"}
	activity.emit("▶ stop-hashing lifted (pgaenable): the board re-initialises and hashes again in about a minute")
	return nil
}

// box2GuardParking: safety guards' parks under way (guardParkBegin): resumeAction refuses meanwhile.
var box2GuardParking atomic.Int32

// guardParkBegin: a safety guard (thermal, mining-blind) is about to park the Box II: writes on their way are
// refused (safetyGen) and no wake path re-enables the board until the returned end is called (after the guard's
// hold is in place). A writer blocked on box2Mu behind the park otherwise sent pgaenable right after it.
func guardParkBegin() (end func()) {
	safetyStop()
	box2GuardParking.Add(1)
	return func() { box2GuardParking.Add(-1) }
}

// resumeHeldErr: resumeAction refused because a guard parks or holds the miner (the wake write is refused too).
type resumeHeldErr struct{ err error }

func (e *resumeHeldErr) Error() string { return e.err.Error() }

// box2WakeResume: the wake paths' resume (mutatePlanBox2Write, applyStockIndexGen, Controls' stock preset): a
// guard's park or hold refuses the wake, and a safety stop that moved gen while it waited refuses the write.
func box2WakeResume(gen int64) error {
	if err := resumeAction(model); err != nil {
		var held *resumeHeldErr
		if errors.As(err, &held) {
			return held
		}
		activity.emit("▶ resume: " + err.Error()) // (Goldshell's cut-off holds the board: logged, the write goes on)
	}
	if genMoved(gen) {
		return errSafetyStop
	}
	return nil
}

// maybeResumeBox2 is the one-line hook the Box II wake paths call (mutatePlanBox2, applyStockPlanGen) before
// they write a hashing plan. No-op on SC Lite / with SoftIdle off; never blocks the write.
func maybeResumeBox2() {
	if err := resumeAction(model); err != nil {
		activity.emit("▶ resume: " + err.Error())
	}
}

// box2SoftIdled: the panel holds the Box II boards off and the stop is (being) confirmed. The idle checks
// (minerOnIdlePlan, minerIsIdleNow, the no-wake plan writes, the board watch) count it as Idle. false on SC
// Lite, with SoftIdle off, and when the miner ignored the stop (then the guards act again).
func box2SoftIdled() bool {
	return model.Plan == planBox2V && model.Feat.SoftIdle && box2Disabled.Load() && box2Parked.Load()
}

// idleActionNote returns the caveat to append to any guard message that used (or tried to use) the stop on a
// model with no stock idle plan. "" on SC Lite, where idle is proven.
func idleActionNote() string {
	if model.Plan != planBox2V {
		return ""
	}
	if model.Feat.SoftIdle {
		return " — " + idleActionCheckedMsg
	}
	return " — " + idleActionUnprovenMsg
}

// box2IdleStatus: the soft-idle state for /api/model (Box II only).
func box2IdleStatus() map[string]any {
	box2Mu.Lock()
	defer box2Mu.Unlock()
	st := map[string]any{"held": box2Disabled.Load(), "phase": box2St.Phase, "ignored": box2St.Ignored,
		"reapplied": box2St.Reapplied, "note": box2St.Note}
	if !box2St.Since.IsZero() {
		st["since"] = box2St.Since.Unix()
	}
	if box2St.TempStart > 0 {
		st["temp_start"] = box2St.TempStart
	}
	if box2St.TempNow > 0 {
		st["temp_now"] = box2St.TempNow
	}
	return st
}

// ---- the miner-API board on/off ----

// box2Dev: one board as `devs` lists it.
type box2Dev struct {
	PGA      int
	Enabled  string // "Y" / "N" ("" when the miner doesn't say)
	Status   string // "Alive", "Sick", "Dead", …
	Work     float64
	HaveWork bool
	MHS      float64
	Temp     float64 // hottest valid of tstemp-0/1/2 (chips and the board sensor; the board sensor only while disabled), 0 when none
	Overheat bool    // the miner's own overheat flag (minerd shuts such a board down with pgadisable)
}

func box2ReadDevs() ([]box2Dev, error) {
	raw, err := box2APICmd("devs")
	if err != nil {
		return nil, err // the API isn't answering (the mining program is restarting)
	}
	devs, ok := apiEntries(raw, "DEVS")
	if !ok {
		return nil, fmt.Errorf("the miner's devs answer couldn't be read")
	}
	var out []box2Dev
	for i, d := range devs {
		b := box2Dev{PGA: i, Enabled: apiStr(d, "Enabled"), Status: apiStr(d, "Status"), MHS: currentMHSOf(d), Overheat: apiNum(d, "overheat") != 0}
		if _, ok := d["PGA"]; ok {
			b.PGA = apiInt(d, "PGA")
		}
		if v, ok := d["Total MH"]; ok && v != nil {
			b.Work, b.HaveWork = apiNum(d, "Total MH"), true
		}
		// chips (tstemp-0/1) and the board sensor (tstemp-2). A disabled board's chip readings are frozen at
		// their last value (the miner stops polling a de-initialised board), so only the board sensor counts.
		keys := []string{"tstemp-0", "tstemp-1", "tstemp-2"}
		if b.Enabled == "N" {
			keys = keys[2:]
		}
		for _, k := range keys {
			if t := apiNum(d, k); t >= 5 && t < 150 && t > b.Temp {
				b.Temp = t
			}
		}
		out = append(out, b)
	}
	return out, nil
}

func box2MaxTempNow() float64 {
	ds, err := box2ReadDevs()
	if err != nil {
		return 0
	}
	var t float64
	for _, d := range ds {
		t = max(t, d.Temp)
	}
	return t
}

// box2BoardPGAs lists the PGA numbers to switch, from `devs`. Falls back to the profile's board count
// (0..Boards-1) only when the API answers with an empty device list.
func box2BoardPGAs() ([]int, error) {
	ds, err := box2ReadDevs()
	if err != nil {
		return nil, err
	}
	var pgas []int
	for _, d := range ds {
		pgas = append(pgas, d.PGA)
	}
	if len(pgas) == 0 {
		for i := 0; i < model.Boards; i++ {
			pgas = append(pgas, i)
		}
	}
	return pgas, nil
}

// box2StopHashing sends pgadisable to every board. Returns (stopped, total, err). A board already disabled
// counts as stopped (idempotent). err is non-nil unless every board stopped.
func box2StopHashing() (stopped, total int, err error) {
	return box2SwitchBoards("pgadisable", "disable")
}

// box2ResumeHashing sends pgaenable to every board. Same contract as box2StopHashing.
func box2ResumeHashing() (enabled, total int, err error) {
	return box2SwitchBoards("pgaenable", "enable")
}

// box2StopVerified: box2StopHashing, then `devs` must list every board disabled ("Enabled":"N"). A board the
// miner still lists enabled gets the command once more before it counts as a failure.
func box2StopVerified() (int, int, error) { return box2SwitchVerified("pgadisable", "disable", "N") }

// box2ResumeVerified: box2ResumeHashing, then `devs` must list every board enabled again.
func box2ResumeVerified() (int, int, error) { return box2SwitchVerified("pgaenable", "enable", "Y") }

func box2SwitchVerified(cmd, verb, want string) (int, int, error) {
	n, total, err := box2SwitchBoards(cmd, verb)
	if err != nil {
		return n, total, err
	}
	for try := 0; ; try++ {
		bad, err := box2NotIn(want)
		if err != nil {
			return 0, total, fmt.Errorf("sent, but the board state couldn't be read back: %v", err)
		}
		if len(bad) == 0 {
			return total, total, nil
		}
		if try == 1 {
			return total - len(bad), total, fmt.Errorf("the miner accepted %s but still lists board %v as %s", cmd, bad, map[string]string{"N": "enabled", "Y": "disabled"}[want])
		}
		for _, p := range bad {
			box2APICmdParam(cmd, strconv.Itoa(p))
		}
	}
}

// box2NotIn: the boards whose "Enabled" isn't want (a miner that doesn't report the field is taken at its word).
func box2NotIn(want string) ([]int, error) {
	ds, err := box2ReadDevs()
	if err != nil {
		return nil, err
	}
	var bad []int
	for _, d := range ds {
		if d.Enabled != "" && d.Enabled != want {
			bad = append(bad, d.PGA)
		}
	}
	return bad, nil
}

func box2SwitchBoards(cmd, verb string) (done, total int, err error) {
	pgas, e := box2BoardPGAs()
	if e != nil {
		return 0, 0, e
	}
	total = len(pgas)
	var firstErr error
	for _, p := range pgas {
		out, e := box2APICmdParam(cmd, strconv.Itoa(p))
		if e != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("board %d: %v", p, e)
			}
			continue
		}
		if ok, already := apiStatusOK(out); ok || already {
			done++
			continue
		}
		if firstErr == nil {
			firstErr = fmt.Errorf("the miner refused to %s board %d: %s", verb, p, apiStatusMsg(out))
		}
	}
	if done < total && firstErr == nil {
		firstErr = fmt.Errorf("only %d of %d boards answered", done, total)
	}
	if done < total {
		return done, total, firstErr
	}
	return done, total, nil
}

// apiStatusOK reads a cgminer/BFGMiner STATUS reply to pgadisable / pgaenable: ok = the command was taken,
// already = the board was already in the asked state (idempotent across retries).
//
// The Box II miner answers these with STATUS "I", not "S" (its message table, R10 intminer.box2 @0x217684):
// 61 "PGA %d already enabled", 62 "already disabled", 63 "PGA %d sent enable message", 64 "PGA %d set disable
// flag" are all INFO; 65 "is not flagged WELL, cannot enable" and 58 "Invalid PGA id" are ERROR. Lane B3 only
// accepted "S", so every real stop would have been read as refused (with the board actually disabled).
func apiStatusOK(out []byte) (ok, already bool) {
	sev, code, msg := apiStatusOf(out)
	low := strings.ToLower(msg)
	if sev == "E" || sev == "F" {
		return false, false
	}
	if strings.Contains(low, "already") || code == 61 || code == 62 {
		return false, true
	}
	switch {
	case sev == "S":
		return true, false
	case code == 63 || code == 64,
		strings.Contains(low, "sent enable message"), strings.Contains(low, "set disable flag"),
		strings.Contains(low, "set ok"):
		return true, false
	}
	return false, false
}

// apiStatusOf: the first STATUS entry's severity letter, code and message (code -1 when absent). A reply that
// isn't JSON falls back to plain text.
func apiStatusOf(out []byte) (sev string, code int, msg string) {
	var r struct {
		STATUS []map[string]any `json:"STATUS"`
	}
	if json.Unmarshal(apiClean(out), &r) == nil && len(r.STATUS) > 0 {
		s := r.STATUS[0]
		code = -1
		if _, ok := s["Code"]; ok {
			code = apiInt(s, "Code")
		}
		return apiStr(s, "STATUS"), code, apiStr(s, "Msg")
	}
	return "", -1, string(out)
}

func apiStatusMsg(out []byte) string {
	if _, _, m := apiStatusOf(out); m != "" && len(m) <= 160 {
		return strings.TrimSpace(m)
	}
	s := string(out)
	if len(s) > 160 {
		s = s[:160]
	}
	return strings.TrimSpace(s)
}

// box2CutoffHolds: why the board must not be re-enabled ("" = it may): Goldshell's over-temperature shutdown
// happened (its log), or the miner flags a board as overheated.
func box2CutoffHolds() string {
	if box2OverTemp() {
		return OemPoss() + " over-temperature shutdown happened since the boot"
	}
	if ds, err := box2ReadDevs(); err == nil {
		for _, d := range ds {
			if d.Overheat {
				return fmt.Sprintf("the miner flags board %d as overheated", d.PGA)
			}
		}
	}
	return ""
}

// ---- the watch: confirm, re-apply after a miner restart, report an ignored stop, follow the resume ----

// box2IdleWatchLoop runs only when SoftIdle is on (started from main): one look every box2WatchEvery.
func box2IdleWatchLoop() {
	for {
		time.Sleep(box2WatchEvery)
		box2IdleWatchTick(box2Now())
	}
}

// box2IdleWatchTick: one look. Split out so tests drive it with their own clock.
func box2IdleWatchTick(now time.Time) {
	if model.Plan != planBox2V || !model.Feat.SoftIdle {
		return
	}
	box2Mu.Lock()
	defer box2Mu.Unlock()
	defer box2SyncParked()
	st := &box2St
	if !box2Disabled.Load() {
		if st.Phase == "resuming" {
			box2WatchResume(now, st)
		}
		return
	}
	ds, err := box2ReadDevs()
	if err != nil {
		st.Note = "the miner API isn't answering (the mining program is restarting?): the stop is re-applied once it answers"
		return
	}
	var enabled []int
	var work, temp float64
	have := len(ds) > 0
	for _, d := range ds {
		if d.Enabled == "Y" {
			enabled = append(enabled, d.PGA)
		}
		work += d.Work
		have = have && d.HaveWork
		temp = max(temp, d.Temp)
	}
	st.TempNow = temp
	if len(ds) == 0 {
		st.Note = "the miner lists no boards yet (starting up): the stop is re-applied once it does"
		return
	}
	if len(enabled) > 0 {
		// a fresh mining program (the launcher relaunched it, or the miner rebooted) has every board enabled
		// again, or something re-enabled one: the panel still holds it off, so re-apply.
		if _, _, err := box2StopVerified(); err != nil {
			st.Note = "re-applying the stop failed: " + err.Error()
			if st.Phase != "ignored" {
				st.Phase = "ignored"
				box2FansMax()
				msg := fmt.Sprintf("the miner is hashing again while it should be idle (the mining program restarted?) and the stop couldn't be re-applied (%v): fans forced to 100%%, relying on %s %.0f °C cut-off; it is tried again every %s", err, oemPoss(), model.TempCutoffC, box2WatchEvery)
				activity.emit("🧯 " + msg)
				notifyAlways(evThermal, msg)
			}
			return
		}
		st.Reapplied++
		st.Phase, st.AppliedAt, st.still, st.haveWork = "stopping", now, 0, false
		st.Note = "the mining program restarted while idle: the stop was re-applied"
		activity.emit("⏸ the mining program restarted (or re-enabled the board) while the miner was on Idle: stop-hashing re-applied (pgadisable)")
		return
	}
	switch st.Phase {
	case "stopping", "ignored", "":
		if st.Phase == "" { // held off since a restart of the panel: start confirming now
			st.Phase, st.AppliedAt = "stopping", now
		}
		grew := box2WorkGrew(st, work, have, ds)
		if !grew {
			st.still++
		} else {
			st.still = 0
		}
		if st.still >= box2StillTicks && st.Ignored {
			// it ignored the stop earlier in this idle (work, or a rising temperature): a later standstill doesn't
			// make it a trusted park again — it stays "ignored" (fans 100 %, the guards act) until a resume
			st.Note = "the miner ignored the stop earlier in this idle; no new work now, but it stays untrusted until a resume"
			return
		}
		if st.still >= box2StillTicks {
			if st.Phase != "stopped" {
				was := st.Phase
				st.Phase, st.confirmAt, st.coolDone = "stopped", now, false
				// the cooling check starts here, on the same sensor it is judged on: a disabled board reports only
				// its board sensor (its chips' readings freeze), so a start taken while it hashed (chips) can't be used
				st.TempStart = temp
				st.Note = fmt.Sprintf("hashing stopped (confirmed): board disabled, no new work for %s", time.Duration(box2StillTicks)*box2WatchEvery)
				msg := "⏸ stop-hashing confirmed: the board is disabled and has done no work for " + (time.Duration(box2StillTicks) * box2WatchEvery).String()
				if temp > 0 {
					msg += fmt.Sprintf(" (%.0f °C now)", temp)
				}
				if was == "ignored" {
					msg += " — it stopped late"
				}
				activity.emit(msg)
			}
			return
		}
		if st.Phase == "stopping" && now.Sub(st.AppliedAt) > box2ConfirmWindow {
			st.Phase, st.Ignored = "ignored", true
			box2ReportIgnored(st, fmt.Sprintf("the board is disabled but still did work %s after the stop", box2ConfirmWindow))
		}
	case "stopped":
		if box2WorkGrew(st, work, have, ds) {
			// hashing again with the board still disabled
			st.Phase, st.Ignored, st.still = "ignored", true, 0
			box2ReportIgnored(st, "the board is disabled but started doing work again")
			return
		}
		if !st.coolDone && now.Sub(st.confirmAt) >= box2CoolCheck {
			st.coolDone = true
			switch {
			case temp <= 0:
				st.Note = "hashing stopped; no temperature reading while idle"
			case st.TempStart > 0 && temp >= st.TempStart+2:
				st.Phase, st.Ignored = "ignored", true
				box2ReportIgnored(st, fmt.Sprintf("the temperature keeps rising (%.0f → %.0f °C)", st.TempStart, temp))
			case st.TempStart > 0 && temp <= st.TempStart-3:
				st.Note = fmt.Sprintf("hashing stopped; cooled %.0f → %.0f °C", st.TempStart, temp)
				activity.emit("❄ " + st.Note)
			default:
				st.Note = fmt.Sprintf("hashing stopped, but the temperature hasn't fallen (%.0f → %.0f °C)", st.TempStart, temp)
				activity.emit("⚠ " + st.Note + ": check the fans and the room; the 86 °C cut-off stays the backstop")
			}
		}
	}
}

// box2WorkGrew: the work counter grew since the last look (any growth over 1 GH: a disabled board does none;
// without a counter, a non-zero recent hashrate). Keeps the last reading; a counter that went back is a new
// mining session (taken as the new base, not as work).
func box2WorkGrew(st *box2IdleState, work float64, have bool, ds []box2Dev) bool {
	if !have {
		for _, d := range ds {
			if d.MHS > 0 {
				return true
			}
		}
		return false
	}
	prev, had := st.lastWork, st.haveWork
	st.lastWork, st.haveWork = work, true
	if !had || work < prev {
		return true // first reading (or a reset counter): nothing to compare yet
	}
	return work-prev > 1000
}

// box2IgnoredMsg: the alert when the miner kept hashing through the stop. The owner's build says to switch the
// stop off (Settings → Commissioning); the public release has no Commissioning, so it asks for a report.
func box2IgnoredMsg(why string) string {
	what := "Switch Soft idle off in Settings → Commissioning and send the logs"
	if publicBuild {
		what = "Please report it, with the logs (Log → “For sending”)"
	}
	return fmt.Sprintf("stop-hashing was IGNORED by the miner (%s): fans forced to 100%%, relying on %s %.0f °C cut-off. The guards treat the miner as NOT parked. %s", why, oemPoss(), model.TempCutoffC, what)
}

func box2ReportIgnored(st *box2IdleState, why string) {
	st.Note = "the miner ignored the stop: " + why
	box2FansMax()
	msg := box2IgnoredMsg(why)
	activity.emit("🧯 " + msg)
	notifyAlways(evThermal, msg)
}

func box2WatchResume(now time.Time, st *box2IdleState) {
	ds, err := box2ReadDevs()
	if err != nil {
		return
	}
	var work float64
	have := len(ds) > 0
	for _, d := range ds {
		work += d.Work
		have = have && d.HaveWork
	}
	hashing := false
	if have {
		if st.resumeWork < 0 || work < st.resumeWork {
			st.resumeWork = work
		} else {
			hashing = work-st.resumeWork > 1000
		}
	} else {
		for _, d := range ds {
			hashing = hashing || d.MHS > 0
		}
	}
	switch {
	case hashing:
		st.Phase, st.Note = "", "hashing again after the idle"
		activity.emit("▶ hashing again after the idle")
	case now.Sub(st.AppliedAt) > box2ResumeWindow:
		st.Phase, st.Note = "", "the board hasn't hashed since the resume"
		activity.emit(fmt.Sprintf("⚠ the board hasn't hashed in the %s since the idle ended: Settings → Restart mining usually brings it back", box2ResumeWindow))
	}
}

// ---- across a panel restart or a reboot ----

func box2SoftIdlePath() string { return filepath.Join(filepath.Dir(*authFile), "softidle.json") }

func box2SaveMarker(now time.Time) {
	b, _ := json.Marshal(map[string]int64{"since": now.Unix()})
	writeAtomic(box2SoftIdlePath(), b, 0o644)
}

func box2RemoveMarker() {
	os.Remove(box2SoftIdlePath())
	os.Remove(box2SoftIdlePath() + ".bak")
}

// box2SoftIdleBoot (at start, after the commission is applied): an idle the panel held when it stopped.
//   - SoftIdle on: the board is held off again — a rebooted miner, or a relaunched mining program, has it
//     enabled and would hash; the watch re-applies the stop as soon as the API answers.
//   - SoftIdle switched off meanwhile (a commission change restarts the panel): nothing could re-enable the
//     board the panel left disabled, so it is re-enabled once (pgaenable) and the marker dropped.
//
// No marker (every unit that never had SoftIdle on, and every SC Lite): nothing at all.
func box2SoftIdleBoot() {
	if model.Plan != planBox2V {
		return
	}
	b, err := os.ReadFile(box2SoftIdlePath())
	if err != nil {
		return
	}
	var mk struct {
		Since int64 `json:"since"`
	}
	json.Unmarshal(b, &mk)
	if !model.Feat.SoftIdle {
		go box2ReleaseLeftover(10 * time.Minute)
		return
	}
	box2Mu.Lock()
	since := time.Unix(mk.Since, 0)
	if mk.Since <= 0 {
		since = box2Now()
	}
	box2Disabled.Store(true)
	box2St = box2IdleState{Phase: "stopping", Since: since, AppliedAt: box2Now(), Note: "held on Idle across a restart: the stop is re-applied once the miner answers"}
	box2SyncParked()
	box2Mu.Unlock()
	chargeMu.Lock()
	if chargeBy == "" {
		chargeBy = "idle"
	}
	chargeMu.Unlock()
	activity.emit("⏸ the miner was on Idle (stop-hashing) when the panel stopped: it stays on Idle until you apply " + stockWord() + " or a preset")
}

// box2ReleaseLeftover: SoftIdle is off but the panel left the board disabled: re-enable it once the API answers.
func box2ReleaseLeftover(wait time.Duration) {
	end := box2Now().Add(wait)
	for {
		if why := box2CutoffHolds(); why != "" {
			box2RemoveMarker()
			activity.emit("⚠ stop-hashing is switched off; the board the panel had disabled is left off: " + why)
			return
		}
		box2Mu.Lock()
		n, total, err := box2ResumeHashing()
		box2Mu.Unlock()
		if err == nil {
			box2RemoveMarker()
			activity.emit(fmt.Sprintf("▶ stop-hashing is switched off: the board the panel had disabled was re-enabled (%d of %d)", n, total))
			return
		}
		if box2Now().After(end) {
			box2RemoveMarker()
			activity.emit("⚠ stop-hashing is switched off, but the board the panel had disabled couldn't be re-enabled (" + err.Error() + "): Settings → Restart mining")
			return
		}
		time.Sleep(box2WatchEvery)
	}
}

// ---- the fall-back ----

// box2FansMaxPlan pushes both fans to 100 % through the running stock plan (the way minerd sets fans, the same
// write the thermal guard's no-park path uses) — never a raw sysfs write, never fan2/fan3 (the Box II voltage
// trim channels). On its own goroutine: callers may hold controlMu/planMu-adjacent locks, and the write takes
// planMu. A plan write while the board is held off is refused by mutatePlanBox2 (box2SoftIdled), so this only
// lands when the stop failed or was ignored.
func box2FansMaxPlan() {
	go func() {
		if _, err := writeFansUnlessIdle(100); err != nil {
			// (the system log, not the activity feed: the guard's own message there is the one the owner reads)
			log.Printf("soft idle: forcing the fans to 100%% failed: %v", err)
		}
	}()
}

// box2DegradeLog records the degrade to the activity log, rate-limited to one line a minute so a guard that
// retries every reading can't flood it. The guards' own notifications carry the caveat via idleActionNote().
var (
	box2DegradeMu   sync.Mutex
	box2DegradeLast time.Time
)

func box2DegradeLog(reason string) {
	box2DegradeMu.Lock()
	spam := !box2DegradeLast.IsZero() && time.Since(box2DegradeLast) < time.Minute
	box2DegradeLast = time.Now()
	box2DegradeMu.Unlock()
	if spam {
		return
	}
	activity.emit("🧯 the miner could not be parked (" + reason + "): fans forced to 100 % and relying on the 86 °C hardware cut-off (the guaranteed backstop)")
}
