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
	"os"
	"path/filepath"
	"strings"
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
	if configFAT {
		return fatWrite(path, b, mode)
	}
	return writeAtomicPlain(path, b, mode)
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
