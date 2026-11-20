package main

// Crash-safe config storage for the SC Box II (B4 — R04/R19).
//
// The SC Lite keeps its config on an ext filesystem at /usr/config: atomic rename works, unix perms are
// enforced, and a journalled fs survives a power cut. The SC Box II has none of that. Its only writable
// store is a 3 MB FAT at /mnt/config (/dev/spiblock2): no journalling, rename-over is not atomic against a
// power loss, FAT can't store unix perms, and flash wears out. Root is a read-only squashfs, so the stock
// panel's `mount -o remount,rw /` tricks fail.
//
// This file is the one place that knows those facts. It gives:
//   - fatWrite: a crash-safe write (temp+fsync → copy current to .bak+fsync → rename → dir-fsync);
//   - cfgWrite / writeAtomic routing so every config write is crash-safe on FAT and byte-for-byte the old
//     behaviour on SC Lite's ext (configFAT stays false there);
//   - readConfig: a loader that validates and falls back to .bak;
//   - statfs helpers (isFAT / isSquashfs) so remount-based writes can be skipped on squashfs;
//   - cleanStaleTmp: drop *.tmp a power cut left behind, at start.
//
// The write *cadence* (how often hot files reach flash) is wear-aware: see cfgCheckpointGap and the hot-file
// handling in main.go/recovery.go. SC Lite (ext) keeps its original cadence.

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// statfs f_type magics we care about (linux magic.h). int64 so the comparison is the same on arm (32-bit,
// where Statfs_t.Type is int32) and amd64.
const (
	magicMSDOS    int64 = 0x4d44     // FAT (vfat / msdos) — the Box II config partition
	magicSquashfs int64 = 0x73717368 // read-only root on the Box II
	magicTmpfs    int64 = 0x01021994 // RAM (tmpfs)
	magicRamfs    int64 = 0x858458f6 // RAM (ramfs)
)

// configFAT is decided once at start-up (initConfigStore): true when panel config writes must be crash-safe
// for a FAT store with no atomic rename — the Box II. On SC Lite it stays false and every config write keeps
// its exact prior behaviour (temp+fsync+rename, no .bak), so SC Lite is unchanged.
var configFAT bool

// fsMagic returns the statfs f_type of the filesystem holding path (0 if it can't be read). It is a
// variable so tests can simulate a FAT config or a squashfs root without a real mount.
var fsMagic = func(path string) int64 {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0
	}
	return int64(st.Type)
}

// isFAT reports whether path lives on a FAT filesystem.
func isFAT(path string) bool { return fsMagic(path) == magicMSDOS }

// isSquashfs reports whether path lives on a read-only squashfs (a rw remount of it will fail).
func isSquashfs(path string) bool { return fsMagic(path) == magicSquashfs }

// isRAM reports whether path lives on a RAM-backed filesystem (tmpfs/ramfs) — already wear-free.
func isRAM(path string) bool { m := fsMagic(path); return m == magicTmpfs || m == magicRamfs }

// initConfigStore decides the config write mode once at start-up and cleans up leftover *.tmp files a power
// cut left behind. model is authoritative (the Box II config is FAT and ships planBox2V); a statfs check
// also catches a FAT store on any model. Called from main() after loadModelFiles/setWindowsFromModel.
func initConfigStore() {
	root := filepath.Dir(*authFile)
	configFAT = model.Plan == planBox2V || isFAT(root)
	cleanStaleTmp(root)
	cfgShadowStart() // the Box II: settings in RAM, the flash read-only (cfgshadow.go)
}

// cleanStaleTmp removes "*.tmp" files in dir: a power cut during a temp+rename write leaves one behind, and
// on the tiny FAT they only take room (every writer re-creates its own .tmp before use). Best-effort; one
// level deep plus the known tuner subtree is enough for the panel's layout.
func cleanStaleTmp(dir string) {
	if dir == "" {
		return
	}
	removeTmpIn(dir)
}

func removeTmpIn(dir string) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() {
			// one extra level (tuner/<miner>/..., minerd/..., etc.) — don't walk the whole tree
			sub := filepath.Join(dir, name)
			subents, err := os.ReadDir(sub)
			if err != nil {
				continue
			}
			for _, se := range subents {
				if !se.IsDir() && strings.HasSuffix(se.Name(), ".tmp") {
					os.Remove(filepath.Join(sub, se.Name()))
				}
			}
			continue
		}
		if strings.HasSuffix(name, ".tmp") {
			os.Remove(filepath.Join(dir, name))
		}
	}
}

// fatWrite writes b to path crash-safely on a store with no atomic rename and no journalling:
//  1. write path+".tmp" and fsync it (the new content reaches flash);
//  2. copy the current path to path+".bak" and fsync it (the last good copy is preserved);
//  3. rename the temp over path;
//  4. fsync the directory so the rename's metadata reaches flash.
//
// A power cut at any step leaves the old file, the new file, or the .bak — never a truncated or empty
// config. FAT can't store unix perms, so mode is a hint only (the /mnt/config mount's umask=077 enforces
// 0600) and a chmod EPERM is never fatal. Loaders pair with readConfig to fall back to .bak.
func fatWrite(path string, b []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp := path + ".tmp"
	if err := writeFileSync(tmp, b, mode); err != nil {
		os.Remove(tmp) // a full partition: don't leave the half-written temp taking room
		return err
	}
	// Preserve the last good copy before overwriting. Best-effort: if it fails the rename below still leaves
	// the old file intact until the moment it is replaced, so no data is lost by skipping it.
	if cur, err := os.ReadFile(path); err == nil && len(cur) > 0 {
		_ = writeFileSync(path+".bak", cur, mode)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	fsyncDir(dir)
	return nil
}

// fsyncDir flushes a directory entry change (the rename) to flash. On FAT this is what makes the rename
// durable; it is harmless on ext. Best-effort: a kernel that refuses to fsync a directory is not fatal.
func fsyncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	d.Sync()
	d.Close()
}

// cfgWrite is the single entry point for writing a panel config file. On a FAT store (Box II) it uses the
// crash-safe fatWrite (temp+fsync, .bak, rename, dir-fsync). Everywhere else it is the pre-port writeAtomic
// (temp+fsync, rename), so SC Lite keeps the exact file it wrote before — only its crash-safety is unchanged.
func cfgWrite(path string, b []byte, mode os.FileMode) error {
	if err := cfgFault(path); err != nil {
		return err
	}
	var err error
	if configFAT {
		err = fatWrite(path, b, mode)
	} else {
		err = writeAtomicPlain(path, b, mode)
	}
	if err != nil {
		return cfgNoteErr(path, err)
	}
	return cfgCommitWritten(path) // the Box II: the RAM copy onto the flash (cfgshadow.go)
}

// readConfig reads a panel config file, validating the bytes with ok(); if the primary file is missing or
// fails validation it falls back to path+".bak". Returns the chosen bytes and true, or (nil,false) when
// neither is usable — the caller then falls back to its built-in defaults. A nil ok means "valid JSON".
func readConfig(path string, ok func([]byte) bool) ([]byte, bool) {
	if ok == nil {
		ok = json.Valid
	}
	if b, err := os.ReadFile(path); err == nil && ok(b) {
		return b, true
	}
	if b, err := os.ReadFile(path + ".bak"); err == nil && ok(b) {
		return b, true
	}
	return nil, false
}

// cfgCheckpointGap is how often a hot file (hashrate history, events log, last-alive, kept system log) is
// allowed to reach the config store. On FAT the flash wears, so hot data lives in RAM and is checkpointed
// about hourly (and on events / before a panel-initiated reboot); on SC Lite's ext the original frequent
// cadence is kept so its behaviour is unchanged.
func cfgCheckpointGap(onFAT, fast time.Duration) time.Duration {
	if configFAT {
		return onFAT
	}
	return fast
}

// ===================== a config store that can't be written =====================
//
// After a power cut the Box II's FAT can come up damaged; the kernel then remounts it read-only (errors=remount-ro)
// and every settings write fails with EROFS until a reboot (whose start-up check, scbox2-fatfix, repairs what is safe
// to repair). Nothing that keeps the miner safe may depend on a write landing: the fan loop, the thermal guard and
// every other guard run from the settings in memory, a write that fails is logged once and they go on. The
// Dashboard says so (cfgStoreView), and a POST that couldn't save answers ok:false with the reason.

var (
	// cfgWriteFault: tests make config writes fail here (nil in the panel).
	cfgWriteFault func(path string) error
	cfgROSeen     atomic.Bool // a config write failed with EROFS since start
	cfgFailLogged atomic.Bool
	cfgLastErr    atomic.Value // string: the last config write error
)

func cfgFault(path string) error {
	if cfgWriteFault == nil {
		return nil
	}
	return cfgNoteErr(path, cfgWriteFault(path))
}

// cfgNoteErr: a config write's result. A failure is remembered (EROFS: the store is read-only) and logged once.
func cfgNoteErr(path string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, syscall.EROFS) {
		cfgROSeen.Store(true)
	}
	cfgLastErr.Store(err.Error())
	if cfgFailLogged.CompareAndSwap(false, true) {
		log.Printf("config store: a settings write failed (%s: %v): the panel keeps running on the settings in memory (logged once)", filepath.Base(path), err)
	}
	return err
}

// cfgSaveErr: the text a POST answers when its setting couldn't be written ("could not save: ..."), with the reason
// in plain words when the store is read-only.
func cfgSaveErr(err error) string {
	if errors.Is(err, syscall.EROFS) || cfgStoreReadOnly() {
		return "could not save: the settings storage is read-only (see the Dashboard): " + err.Error()
	}
	return "could not save: " + err.Error()
}

// cfgROProbe: the config dir's filesystem is mounted read-only (statfs ST_RDONLY). A variable for the tests.
var cfgROProbe = func(dir string) bool {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return false
	}
	return int64(st.Flags)&1 != 0 // ST_RDONLY
}

var (
	cfgROMu sync.Mutex
	cfgROAt time.Time
	cfgROv  bool
)

// cfgStoreReadOnly: the store is read-only now (statfs, at most every 10 s), or a write failed with EROFS.
func cfgStoreReadOnly() bool {
	if cfgROSeen.Load() {
		return true
	}
	cfgROMu.Lock()
	defer cfgROMu.Unlock()
	if cfgROAt.IsZero() || time.Since(cfgROAt) > 10*time.Second {
		cfgROv, cfgROAt = cfgROProbe(filepath.Dir(*authFile)), time.Now()
	}
	return cfgROv
}

// fanWriteFailing: the fan loop's last fan write failed (with the store read-only on the Box II: the OEM's backend
// saves each plan, fans included, to that storage before it applies it, and gives up when it can't).
var fanWriteFailing atomic.Bool

// cfgTmp: where the boot scripts leave their notes about the settings storage (a variable for the tests).
var cfgTmp = brandTmp

func cfgRepairMarker() string { return cfgTmp + "cfg-repair" } // scbox2-fatfix: damage it wouldn't repair
func cfgBrokenMarker() string { return cfgTmp + "cfg-broken" } // the boot scripts: not mounted / not writable
func cfgFatfixLog() string    { return cfgTmp + "fatfix.log" } // scbox2-fatfix's report of this boot

// cfgStoreState: the Dashboard's banner about the settings storage (nil = nothing to say).
type cfgStoreState struct {
	Kind string `json:"kind"` // "read_only", "damaged" (the start-up check refused a repair), "broken" (not mounted / not writable)
	Msg  string `json:"msg"`
	Fix  string `json:"fix"`
}

func readMarker(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	s := strings.TrimSpace(string(b))
	if i := strings.Index(s, ": "); i > 0 && !strings.Contains(s[:i], " ") && !strings.Contains(s[:i], "/") {
		s = s[i+2:] // the writer's name ("scbox2: ", "S38initdir: ")
	}
	if len(s) > 600 {
		s = s[:600] + "…"
	}
	return s
}

func cfgStoreView() *cfgStoreState {
	box2 := model.Plan == planBox2V
	reboot := "Reboot the miner (Settings → Restart & reboot)."
	if box2 {
		reboot = "Reboot the miner (Settings → Restart & reboot): at start-up it checks the settings storage and repairs what is safe to repair; your settings come back as they were last saved."
	}
	backup := "Export your settings (Settings → Firmware update → Settings backup) to keep a copy."
	if box2 {
		backup += " If this stays after a reboot, re-flash the firmware from an SD card (that formats the settings storage) and import them."
	}
	if ro := cfgStoreReadOnly(); ro || readMarker(cfgBrokenMarker()) != "" {
		st := &cfgStoreState{Kind: "read_only"}
		if !ro {
			st.Kind = "broken"
		}
		st.Msg = "Settings can't be saved: the settings storage is read-only (after a power cut?). Mining keeps running on the settings you had, and the fan curve and the thermal guard keep running from memory."
		if !ro {
			st.Msg = "Settings can't be saved: " + readMarker(cfgBrokenMarker()) + ". Mining keeps running on the settings you had, and the fan curve and the thermal guard keep running from memory."
		}
		if box2 && cfgShadow() {
			// v0.5.2: every program works on a copy of the settings in RAM; the storage is written only to save a
			// setting, so nothing that runs the fans waits on it
			if ro {
				st.Msg = "Settings can't be saved: the settings storage is read-only and couldn't be made writable to save them (after a power cut?). Mining keeps running on the settings you had; what you changed since is in use until the next restart."
			}
			st.Msg += fmt.Sprintf(" The %s programs and the panel work on a copy of the settings in memory, so the fan curve and the thermal guard keep reaching the fans, and %s own %.0f °C cut-off stays on.", oemName()+"'s", oemPoss(), model.TempCutoffC)
		} else if box2 {
			st.Msg += fmt.Sprintf(" But %s backend saves every fan change to this storage before it applies it, so ", oemPoss())
			if fanWriteFailing.Load() {
				st.Msg += "fan changes are NOT reaching the fans now: they stay where they were."
			} else {
				st.Msg += "fan changes may not reach the fans: they would stay where they are."
			}
			st.Msg += fmt.Sprintf(" The thermal guard can still stop the boards, and %s own %.0f °C cut-off stays on.", oemPoss(), model.TempCutoffC)
		}
		st.Fix = reboot
		if r := readMarker(cfgRepairMarker()); r != "" {
			st.Msg += " " + r
			st.Fix = backup
		}
		return st
	}
	if r := readMarker(cfgRepairMarker()); r != "" {
		return &cfgStoreState{Kind: "damaged", Msg: r + " Saving works for now; the kernel makes the storage read-only the moment it trips over the damage.", Fix: backup}
	}
	return nil
}

// cfgStoreWatch: the banner's state as the Dashboard is built (every fan-loop reading): each new kind of trouble is
// said once in the activity log and as a notification.
var cfgStoreSaid sync.Map

func cfgStoreWatch() *cfgStoreState {
	st := cfgStoreView()
	if st == nil {
		return nil
	}
	if _, said := cfgStoreSaid.LoadOrStore(st.Kind, true); !said {
		msg := st.Msg + " Fix: " + st.Fix
		activity.emit("💾 " + msg)
		go notifyAlways(evStorage, msg)
	}
	return st
}

// cfgStoreBoot: the start-up check's report of this boot, in the activity log when it repaired something.
func cfgStoreBoot() {
	b, err := os.ReadFile(cfgFatfixLog())
	if err != nil {
		return
	}
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "fatfix: repair: ") && strings.Contains(string(b), "read back and verified") {
			activity.emit("🧹 settings storage: the start-up check repaired it (" + strings.TrimPrefix(l, "fatfix: repair: ") + ")")
		}
	}
}
