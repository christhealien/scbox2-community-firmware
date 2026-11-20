package main

// The SC Box II's settings in RAM, the flash read-only (v0.5.2; internal/cfgwin has the mechanism).
//
// At boot /usr/libexec/scbox2/cfgmount mounts the 3 MiB FAT (/dev/spiblock2) read-only at /mnt/cfgflash and copies it
// to a tmpfs at /mnt/config, which every program uses (/usr/config links there). So the OEM's backend saving each fan
// step (cpbsetting.json, bfgminer.json), the panel's history and counters, and everything else written while the
// miner runs, land in RAM: normal running writes nothing to the flash, and nothing that keeps the miner cool waits on
// it. What must survive a reboot is copied to the flash in a short write window (cfgwin.Commit) when the user saves
// something:
//
//   - a panel setting: written by writeAtomic/cfgWrite/saveAuth and committed at once, so a failed save is answered
//     as before ("could not save: the settings storage is read-only ..."). Files that change by themselves while the
//     miner runs (cfgHot: history, counters, the throttle's saved clock, run logs) are not: they stay in RAM;
//   - anything else a user change touched (a file written or removed directly, a hot file the user reset):
//     cfgUserSave, around every change the owner makes in the panel (requireAuth, not GET), commits what changed in
//     the panel's folder while it ran;
//   - the OEM's files (pools, network, the plan): cfgUserSave commits them when the owner's change reached the OEM's
//     backend, except a change of the fan levels alone: the fan curve's steps never reach the flash;
//   - at a reboot the panel makes (and when it's stopped: shutdown), cfgCheckpoint saves the panel's folder, hot files
//     included, so a clean reboot loses no history.
//
// The safety holds (the overheat Idle, the pool guard's hold, the fan lock and the raised fan floor) are panel
// settings (committed when they are set and when they are lifted), so they survive a power cut as before: they are
// written only when a guard trips, never in normal running. "Keep the system log" (Settings -> Recovery, off by
// default) is the one feature that writes the flash while the miner runs: that is what it is for.
//
// Nothing of this is active anywhere else (the SC Lite, or a Box II whose boot step fell back to mounting the
// partition read-write, as before v0.5.2): cfgShadow() is false and every write is in place, as it was.

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"sclite-agent/internal/cfgwin"
)

// cfgShadow: the settings are a RAM copy and the flash is read-only (the SC Box II from v0.5.2).
func cfgShadow() bool { return model.Plan == planBox2V && cfgwin.Active() }

// cfgHotNames / cfgHotDirs: panel files that change by themselves while the miner runs. They stay in RAM (committed
// with a user save that touches them, and at a reboot the panel makes). Every other file of the panel's folder is a
// setting, committed when written.
var cfgHotNames = map[string]bool{
	"hashrate.json":         true, // hashrate history
	"events.log":            true, // the event history
	"shares.json":           true, // best shares
	"shares.log":            true, // the session's share log
	"throttle.json":         true, // the throttle's saved clock (the throttle is routine in summer)
	"chipwatch.json":        true, // chip error counters
	"chipwatch.json.bad":    true,
	"boards-seen.json":      true, // boards seen (notifications)
	"daily-summary":         true, // the last daily summary sent
	"board-autorestart-at":  true, // the last automatic board restart (a reboot follows: checkpointed)
	"verbose-auto-off.json": true, // the miner-health guard's note
	"chiphook.off":          true, // the chip-hook guard's note
	"schedule-before.json":  true, // the plan before a scheduled change (the schedule is routine)
	"notified":              true, // update notifications already sent
	"notified-release":      true,
	"localtime":             true, // refreshed from the firmware's zone data at start (a user change is committed by cfgUserSave)
}

var cfgHotDirs = map[string]bool{"incidents": true, "tuner": true}

// cfgHot: is path one of the files that stay in RAM?
func cfgHot(path string) bool {
	if cfgHotNames[filepath.Base(path)] {
		return true
	}
	// (v0.5.5) the chip trims while a tuning run tries them: test settings, saved at the run's end (box2TrimsInRAM)
	if box2TrimsInRAM() && filepath.Clean(path) == filepath.Clean(chipTrimPath()) {
		return true
	}
	if rel, ok := cfgPanelRel(path); ok {
		first := strings.SplitN(rel, "/", 2)[0]
		return cfgHotDirs[first] || (*tunerDir != "" && filepath.Clean(path) != filepath.Clean(*tunerDir) &&
			strings.HasPrefix(filepath.Clean(path), filepath.Clean(*tunerDir)+"/"))
	}
	return false
}

// (v0.5.5 T5) the chip trims while a tuning run tries them stay in RAM through every commit, an owner's save of
// the panel's folder and the checkpoint before a reboot included: only the run's end saves them.
func init() {
	cfgwin.Hold = func(p string) bool {
		return box2TrimsInRAM() && filepath.Clean(p) == filepath.Clean(chipTrimPath())
	}
}

// cfgPanelRel: path relative to the panel's settings folder.
func cfgPanelRel(path string) (string, bool) {
	dir := filepath.Clean(filepath.Dir(*authFile))
	p := filepath.Clean(path)
	if !strings.HasPrefix(p, dir+"/") {
		return "", false
	}
	return strings.TrimPrefix(p, dir+"/"), true
}

// cfgCommitWritten: after a panel setting's write landed in RAM, onto the flash (hot files: not).
func cfgCommitWritten(path string) error {
	if !cfgShadow() || cfgHot(path) {
		return nil
	}
	return cfgNoteErr(path, cfgwin.CommitPaths(path))
}

// cfgPersist: these paths onto the flash now, whatever they are (a write or a removal made directly).
func cfgPersist(paths ...string) error {
	if !cfgShadow() {
		return nil
	}
	var err error
	if len(paths) > 0 {
		err = cfgNoteErr(paths[0], cfgwin.CommitPaths(paths...))
	}
	return err
}

// cfgRemove: os.Remove of a panel setting, the removal saved too (a hold lifted must not come back at the next boot).
func cfgRemove(path string) error {
	err := os.Remove(path)
	if (err == nil || os.IsNotExist(err)) && !cfgHot(path) { // (gone from RAM already: the flash must follow)
		if cerr := cfgPersist(path); cerr != nil {
			return cerr
		}
	}
	return err
}

// cfgCheckpoint: the panel's whole folder onto the flash (hot files included): before a reboot the panel makes,
// and when the panel is stopped (shutdown). One window; nothing written when nothing changed.
func cfgCheckpoint() {
	if !cfgShadow() {
		return
	}
	if rel, ok := cfgwin.Rel(filepath.Dir(*authFile)); ok {
		if err := cfgwin.Commit(rel); err != nil {
			cfgNoteErr(*authFile, err)
			log.Printf("config: saving the panel's files before the restart failed: %v", err)
		}
	}
	// the OEM backend's files too (pools, network…): a change it saved later than the save around the owner's
	// request, or one made outside the panel, must not be lost at a reboot. Fan-only plan changes still aren't.
	oemPersist(false)
}

// ---- around the owner's changes ----

var (
	stockSettingPuts atomic.Int64 // PUT /mcb/setting answered 200 (stockReq)
	stockOtherPuts   atomic.Int64 // any other PUT to the OEM's backend answered 200 (pools, ...)
)

// The plan PUTs of each owner's request (v0.5.4c): stockSettingPuts counts every PUT /mcb/setting, the tuner's, the
// fan hold's, the throttle's and the schedule's included, so an owner's change that overlapped one of those could
// save that background clock as the owner's plan. cfgUserSave counts only the PUTs made on its own request's
// goroutine (the handler: the panel's plan writes run on the request's goroutine; a background writer never does).
var (
	cfgReqPuts   sync.Map     // goroutine id -> *cfgReqRec: PUT /mcb/setting answered 200 for that request
	cfgReqActive atomic.Int32 // requests counting (no goroutine id looked up while none is)
)

// cfgReqRec: one owner's request's plan PUTs, and the plan the last one asked for (v0.5.5: its plan files go onto
// the flash through box2PersistPlan's checks, not straight away).
type cfgReqRec struct {
	n    atomic.Int64
	mu   sync.Mutex
	plan powerplan
	ok   bool
}

// goID: the current goroutine's id (from its stack header, "goroutine 123 [running]:"); 0 = unknown.
func goID() uint64 {
	var b [64]byte
	f := bytes.Fields(b[:runtime.Stack(b[:], false)])
	if len(f) < 2 {
		return 0
	}
	n, err := strconv.ParseUint(string(f[1]), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// stockSettingPut: a PUT /mcb/setting was answered 200 (stockReq): counted, and for the owner's request it was
// made on.
// body: the PUT's body (the OEM's whole setting): the plan it asks for is kept for that request.
func stockSettingPut(body []byte) {
	stockSettingPuts.Add(1)
	if cfgReqActive.Load() == 0 {
		return
	}
	if c, ok := cfgReqPuts.Load(goID()); ok {
		rec := c.(*cfgReqRec)
		rec.n.Add(1)
		var cur map[string]any
		pp, pok := powerplan{}, false
		if json.Unmarshal(body, &cur) == nil {
			pp, pok = resolveRunningPlan(cur)
		}
		rec.mu.Lock()
		rec.plan, rec.ok = pp, pok && pp.FreqMHz > 0
		rec.mu.Unlock()
	}
}

type cfgStat struct {
	size int64
	mod  time.Time
}

// cfgSnap: the files (size, time) under these folders in RAM (the panel's folder by default).
func cfgSnap(dirs ...string) map[string]cfgStat {
	if len(dirs) == 0 {
		dirs = []string{filepath.Dir(*authFile)}
	}
	out := map[string]cfgStat{}
	for _, dir := range dirs {
		cfgSnapInto(dir, out)
	}
	return out
}

// oemSnap: the OEM's settings folders in RAM.
func oemSnap() map[string]cfgStat {
	var dirs []string
	for _, d := range oemDirs {
		dirs = append(dirs, filepath.Join(cfgwin.RAMRoot, d))
	}
	return cfgSnap(dirs...)
}

func cfgSnapInto(dir string, out map[string]cfgStat) {
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		n := d.Name()
		if strings.HasSuffix(n, ".tmp") || strings.HasSuffix(n, ".bak") {
			return nil
		}
		if fi, err := d.Info(); err == nil {
			out[p] = cfgStat{fi.Size(), fi.ModTime()}
		}
		return nil
	})
}

// cfgChanged: the files added, changed or gone between two snapshots.
func cfgChanged(before, after map[string]cfgStat) []string {
	var changed []string
	for p, a := range after {
		if b, ok := before[p]; !ok || b != a {
			changed = append(changed, p)
		}
	}
	for p := range before {
		if _, ok := after[p]; !ok {
			changed = append(changed, p)
		}
	}
	sort.Strings(changed)
	return changed
}

// cfgUserSave: around a change the owner makes (requireAuth, not GET): what it wrote or removed in the panel's
// folder, and the OEM's files its change reached, onto the flash.
func cfgUserSave(h func()) {
	if !cfgShadow() {
		h()
		return
	}
	before, oemBefore := cfgSnap(), oemSnap()
	sp, op := stockSettingPuts.Load(), stockOtherPuts.Load()
	var mine cfgReqRec // this request's own plan PUTs
	id := goID()
	if id != 0 {
		cfgReqPuts.Store(id, &mine)
		cfgReqActive.Add(1)
	}
	func() {
		defer func() {
			if id != 0 {
				cfgReqPuts.Delete(id)
				cfgReqActive.Add(-1)
			}
		}()
		h()
	}()
	changed := cfgChanged(before, cfgSnap())
	if len(changed) > 0 {
		if err := cfgPersist(changed...); err != nil {
			log.Printf("config: saving %d changed file(s) failed: %v", len(changed), err)
		}
	}
	// (a tuner, fan-hold, throttle or schedule PUT meanwhile is not the owner's plan; no goroutine id: as before)
	setting := mine.n.Load() > 0 || (id == 0 && stockSettingPuts.Load() != sp)
	if setting || stockOtherPuts.Load() != op || len(cfgChanged(oemBefore, oemSnap())) > 0 {
		// (the panel edits the miner's config itself too: the verbose switch). v0.5.5: the plan files are not
		// committed here even when this request wrote the plan: a throttle, schedule or fan-hold clock made in the
		// next seconds (before or after the late pass) would be saved as the owner's plan. They go to
		// box2PersistPlan, which commits them only while the miner still runs the plan this request asked for and
		// the RAM files show it.
		oemPersist(false)
		// the OEM's backend may write a little after it answered (the miner's config when the pools changed)
		if setting || stockOtherPuts.Load() != op {
			late := oemLateWrite
			go func() { time.Sleep(late); oemPersist(false) }()
		}
		if setting {
			mine.mu.Lock()
			pp, ok := mine.plan, mine.ok
			mine.mu.Unlock()
			go func() {
				if !ok { // (the PUT's body didn't say: the plan running now, under the same checks)
					if pp, ok = readStockBaselineNow("current"); !ok {
						log.Printf("config: the plan this change set could not be read: the settings partition keeps the plan it had")
						return
					}
				}
				box2PersistOwnerPlan(pp.FreqMHz, pp.VrunMv)
			}()
		}
	}
}

// box2PersistOwnerPlan: box2PersistPlan for an owner's plan change, once: the preset handler and cfgUserSave around
// it both ask for the same plan (one set of passes, not two: the OEM's backend is polled by each).
var box2PersistLast struct {
	mu        sync.Mutex
	mhz, code int
	at        time.Time
}

func box2PersistOwnerPlan(mhz, code int) {
	box2PersistLast.mu.Lock()
	if box2PersistLast.mhz == mhz && box2PersistLast.code == code && time.Since(box2PersistLast.at) < 5*time.Second {
		box2PersistLast.mu.Unlock()
		return
	}
	box2PersistLast.mhz, box2PersistLast.code, box2PersistLast.at = mhz, code, time.Now()
	box2PersistLast.mu.Unlock()
	box2PersistPlan(mhz, code)
}

var oemLateWrite = 8 * time.Second

// ---- the OEM's files ----

// oemDirs: the OEM's settings folders on the partition (relative to it).
var oemDirs = []string{"minerd", "bfgminer", "etc", "miner_history"}

var oemPersistMu sync.Mutex

// oemPersist: the OEM's files whose RAM copy differs from the flash, onto the flash. withPlan: the owner's change
// went to the plan (PUT /mcb/setting): only then the plan files (cpbsetting.json, bfgminer.json) are saved, and
// never for a change of the fan levels alone. (A throttle step made meanwhile would otherwise be kept as the
// owner's clock.)
func oemPersist(withPlan bool) {
	if !cfgShadow() {
		return
	}
	oemPersistMu.Lock()
	defer oemPersistMu.Unlock()
	var rels []string
	for _, d := range oemDirs {
		for _, rel := range oemFiles(d) {
			if !oemDiffers(rel, withPlan) {
				continue
			}
			rels = append(rels, rel)
		}
	}
	if len(rels) == 0 {
		return
	}
	if err := cfgwin.Commit(rels...); err != nil {
		cfgNoteErr(filepath.Join(cfgwin.RAMRoot, rels[0]), err)
		log.Printf("config: saving the OEM's settings (%s) failed: %v", strings.Join(rels, " "), err)
		return
	}
	log.Printf("config: the OEM's settings saved: %s", strings.Join(rels, " "))
}

// oemFiles: the files under dir in RAM or on the flash (relative to the partition), without temp and backup files
// (the OEM's backend writes ".name" and renames it). The exception is the plan files' dot copies (oemPlanFile):
// the OEM's backend keeps ".cpbsetting.json", ".bfgminer.json" and ".temp_bfgminer.json" next to the files as
// full second copies, and minerd builds the plan it starts mining with from them at boot (an owner's log,
// v0.5.5: the flash's ".bfgminer.json" still held 690 MHz / fan 64 while "bfgminer.json" held the owner's
// 688 MHz, and the boot started on 690 / fan 64). They are saved with their twins, by the same rules.
func oemFiles(dir string) []string {
	seen := map[string]bool{}
	for _, root := range []string{cfgwin.RAMRoot, cfgwin.FlashRoot} {
		base := filepath.Join(root, dir)
		filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			n := d.Name()
			if strings.HasSuffix(n, ".tmp") || strings.HasSuffix(n, ".bak") {
				return nil
			}
			r, err := filepath.Rel(root, p)
			if err != nil {
				return nil
			}
			if strings.HasPrefix(n, ".") && !(oemPlanFile(r) && oemDotCopy(r)) {
				return nil
			}
			seen[r] = true
			return nil
		})
	}
	out := make([]string, 0, len(seen))
	for r := range seen {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

// oemPlanFile: a file the OEM's backend rewrites with every plan (fan steps included), or its dot copy (oemFiles).
func oemPlanFile(rel string) bool {
	rel = oemTwin(rel)
	return rel == "minerd/settings/cpbsetting.json" || rel == "bfgminer/bfgminer.json" || rel == "bfgminer/temp_bfgminer.json"
}

// oemTwin: a plan file's dot copy named as the file itself ("bfgminer/.bfgminer.json" → "bfgminer/bfgminer.json");
// any other path unchanged.
func oemTwin(rel string) string {
	dir, n := filepath.Split(rel)
	return dir + strings.TrimPrefix(n, ".")
}

// oemDotCopy: rel names a dot copy (".name").
func oemDotCopy(rel string) bool { return strings.HasPrefix(filepath.Base(rel), ".") }

func oemDiffers(rel string, withPlan bool) bool {
	ram, rerr := os.ReadFile(filepath.Join(cfgwin.RAMRoot, rel))
	fl, ferr := os.ReadFile(filepath.Join(cfgwin.FlashRoot, rel))
	if rerr != nil || ferr != nil {
		if os.IsNotExist(rerr) && os.IsNotExist(ferr) {
			return false
		}
		if rerr != nil && !os.IsNotExist(rerr) {
			return false // can't read the RAM copy: leave the flash alone
		}
		return !oemPlanFile(rel) || withPlan
	}
	if bytes.Equal(ram, fl) {
		return false
	}
	// the OEM's backend rewrites its JSON files with the keys in a different order (pools.json after a plan change):
	// the same content is not a change, so nothing is written (and the owner isn't shown a pool save they didn't make)
	if oemSameJSON(ram, fl) {
		return false
	}
	if !oemPlanFile(rel) {
		return true
	}
	if !withPlan && oemTwin(rel) != "bfgminer/bfgminer.json" {
		return false
	}
	// bfgminer.json holds the pools (and the panel's own edits, the verbose switch) besides the plan: without an
	// owner's plan change it is saved only when its plan (the clock and voltage of its vfff line) is the one on the
	// flash, so a clean reboot's checkpoint (or a pool change) never saves a background clock (a throttle or schedule
	// step) here while cpbsetting.json keeps the owner's: the two stay consistent. (The OEM's minerd rewrites
	// bfgminer.json from its own settings when mining starts: the pools still come from pools.json.)
	if !withPlan && oemPlanPart(ram) != oemPlanPart(fl) {
		return false
	}
	// the plan files: saved only when something else than the fan levels changed
	return oemNoFans(ram) != oemNoFans(fl)
}

// oemPlanPart: the clock and voltage a plan file's vfff lines carry ("vfff=42.000000:690"), fans left out.
func oemPlanPart(b []byte) string {
	var out []string
	for _, m := range reVfff.FindAllStringSubmatch(string(b), -1) {
		out = append(out, m[1])
	}
	return strings.Join(out, ",")
}

// oemSameJSON: both parse as JSON and hold the same values (key order and formatting ignored).
func oemSameJSON(a, b []byte) bool {
	var va, vb any
	if json.Unmarshal(a, &va) != nil || json.Unmarshal(b, &vb) != nil {
		return false
	}
	ja, e1 := json.Marshal(va)
	jb, e2 := json.Marshal(vb)
	return e1 == nil && e2 == nil && bytes.Equal(ja, jb)
}

var (
	reRPM  = regexp.MustCompile(`([0-9]+(?:\.[0-9]+)?) RPM`)
	reVfff = regexp.MustCompile(`(vfff=[^:"]*:[^:"]*):[^:"]*:[^:"]*`)
)

// oemNoFans: a plan file with its fan levels taken out ("690 MHz 42 V 50 RPM 41 RPM" -> "... _ RPM _ RPM";
// "vfff=0.43:840:100:100" -> "vfff=0.43:840:_:_"), keys sorted, so two files that differ only in the fans compare
// equal. Not JSON: compared as text, fans taken out the same way.
func oemNoFans(b []byte) string {
	var v any
	if json.Unmarshal(b, &v) == nil {
		if nb, err := json.Marshal(v); err == nil {
			b = nb
		}
	}
	s := reRPM.ReplaceAllString(string(b), "_ RPM")
	return reVfff.ReplaceAllString(s, "${1}:_:_")
}

// ---- start and stop ----

var shutdownOnce sync.Once

// installShutdownHandler: SIGTERM (the boot script's stop, a shutdown): disarm the watchdog, save the panel's files
// (the Box II's RAM copy), exit. Installed when the watchdog is armed (as before) and on the Box II with its
// settings in RAM.
func installShutdownHandler() {
	shutdownOnce.Do(func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM)
		go func() {
			<-sig
			watchdogStop()
			if cfgShadow() {
				persistHashrate() // as markCleanReboot: the in-memory history first, then onto the flash
				eventsSave()
			}
			cfgCheckpoint()
			os.Exit(0)
		}()
	})
}

// cfgShadowStart: at start (initConfigStore), with the settings in RAM: the window's log lines go to the panel log,
// and the panel saves its files when it is stopped.
func cfgShadowStart() {
	cfgwin.Logf = log.Printf
	cfgwin.Trace = func(what string) func(error) { return traceWrite("settings-partition write window (" + what + ")") }
	if !cfgShadow() {
		if model.Plan == planBox2V {
			if why := readMarker(cfgTmp + "cfg-direct"); why != "" { // cfgmount's fallback
				log.Printf("config: no RAM copy of the settings this boot: %s", why)
				activity.emit("💾 " + why)
			}
		}
		return
	}
	log.Printf("config: settings in RAM (%s), the settings partition read-only at %s (written only when a setting is saved)", cfgwin.RAMRoot, cfgwin.FlashRoot)
	installShutdownHandler()
}
