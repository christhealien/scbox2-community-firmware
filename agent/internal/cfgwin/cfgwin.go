// Package cfgwin: the SC Box II's settings partition, read-only except for a short write window.
//
// The Box II keeps its settings on a 3 MiB FAT on raw NOR flash (/dev/spiblock2): no journal, rename-over not atomic
// against a power cut, and the flash rewrites a whole 64 KiB erase block for any change. A power cut while a file is
// being replaced can leave the FAT damaged. So (v0.5.2):
//
//   - at boot (/usr/libexec/scbox2/cfgmount, from S38initdir) the partition is mounted READ-ONLY at FlashRoot, and its
//     files are copied to a tmpfs mounted at RAMRoot (/mnt/config: the path every program on the image uses, the
//     OEM's included). Every program reads and writes the RAM copy: the OEM's backend saving each fan step, the
//     panel's history and counters. Normal running writes nothing to the flash, and nothing that keeps the miner
//     cool depends on the flash;
//   - a file that must survive a reboot is copied to the flash in a WINDOW (Commit): the partition is remounted
//     read-write, each file is written crash-safely (temp + fsync, the old copy kept as .bak, rename, directory
//     fsync), everything is synced, and the partition is remounted read-only. One window at a time across every
//     program (an flock on LockPath: the panel, the SSH server and the boot scripts' helper all take it);
//   - with no RAM copy (the boot step fell back to mounting the partition read-write at RAMRoot, as before v0.5.2,
//     or another model) Active is false and Commit does nothing: the files were written in place.
//
// Standard library only: it is linked by the panel, the SSH server and scbox2-fatfix (the boot scripts' helper).
package cfgwin

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

var (
	// RAMRoot: the tmpfs copy of the settings partition, where every program reads and writes.
	RAMRoot = "/mnt/config"
	// FlashRoot: the settings partition itself, mounted read-only outside a window.
	FlashRoot = "/mnt/cfgflash"
	// LockPath: one window at a time, across programs (flock).
	LockPath = "/tmp/scbox2-cfgwin.lock"
	// MountsPath: where Active looks for the two mounts.
	MountsPath = "/proc/mounts"
	// Aliases: other names of RAMRoot (the image's /usr/config is a symlink to /mnt/config).
	Aliases = []string{"/usr/config"}

	// Remount: the partition read-write (rw) or read-only again. A variable for the tests.
	Remount = remount
	// ActiveHook: tests decide whether the RAM copy is in use (nil: look at MountsPath).
	ActiveHook func() bool
	// Logf: where a window says what it did (nil: nowhere).
	Logf func(format string, args ...any)
	// Trace: a window opens (remount rw, write, sync, remount ro); the returned func is called when it is closed
	// again (nil: no trace). The panel's freeze-hunt breadcrumbs.
	Trace func(what string) func(err error)
	// LockWait: how long a window waits for another one to finish.
	LockWait = 30 * time.Second

	windows   atomic.Int64 // windows opened since start (tests, status)
	inProcess sync.Mutex   // flock is per open file: windows of this process also wait for each other
)

// Windows: how many write windows this process opened.
func Windows() int64 { return windows.Load() }

// Error: a window couldn't be opened (the partition couldn't be made writable). It counts as "read-only"
// (errors.Is(err, syscall.EROFS)), so the panel's banner says the settings storage is read-only.
type Error struct{ Err error }

func (e *Error) Error() string {
	return "the settings storage could not be made writable: " + e.Err.Error()
}
func (e *Error) Unwrap() error        { return e.Err }
func (e *Error) Is(target error) bool { return target == syscall.EROFS }

func logf(f string, a ...any) {
	if Logf != nil {
		Logf(f, a...)
	}
}

// Active: the settings are a RAM copy (RAMRoot is a tmpfs and the partition is mounted at FlashRoot), so a file that
// must survive a reboot needs a window.
func Active() bool {
	if ActiveHook != nil {
		return ActiveHook()
	}
	b, err := os.ReadFile(MountsPath)
	if err != nil {
		return false
	}
	ram, flash := false, false
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Fields(l)
		if len(f) < 3 {
			continue
		}
		switch f[1] {
		case RAMRoot:
			ram = f[2] == "tmpfs"
		case FlashRoot:
			flash = true
		}
	}
	return ram && flash
}

// FlashReadOnly: the partition is mounted read-only now (as it should be outside a window). false when it isn't
// mounted at FlashRoot at all.
func FlashReadOnly() bool {
	b, err := os.ReadFile(MountsPath)
	if err != nil {
		return false
	}
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Fields(l)
		if len(f) >= 4 && f[1] == FlashRoot {
			for _, o := range strings.Split(f[3], ",") {
				if o == "ro" {
					return true
				}
			}
			return false
		}
	}
	return false
}

// Rel: path (absolute, under RAMRoot or one of its Aliases) relative to RAMRoot.
func Rel(path string) (string, bool) {
	p := filepath.Clean(path)
	for _, a := range Aliases {
		if p == a || strings.HasPrefix(p, a+"/") {
			p = RAMRoot + strings.TrimPrefix(p, a)
			break
		}
	}
	root := filepath.Clean(RAMRoot)
	if !strings.HasPrefix(p, root+"/") {
		return "", false
	}
	rel := strings.TrimPrefix(p, root+"/")
	if rel == "" || rel == "." || strings.HasPrefix(rel, "../") || rel == ".." {
		return "", false
	}
	return rel, true
}

// CommitPaths: Commit for absolute paths (outside RAMRoot: ignored).
func CommitPaths(paths ...string) error {
	var rels []string
	for _, p := range paths {
		if r, ok := Rel(p); ok {
			rels = append(rels, r)
		}
	}
	if len(rels) == 0 {
		return nil
	}
	return Commit(rels...)
}

// Commit makes the partition hold what the RAM copy holds for each rel (a file, or a folder, mirrored; gone from
// RAM: removed from the partition, its .bak with it), in one window. Files that are already the same are not
// written. Nothing to do (no RAM copy in use, or nothing differs): no window at all.
func Commit(rels ...string) error {
	if !Active() {
		return nil
	}
	return withWindow(func() (int, error) {
		n := 0
		for _, r := range rels {
			c, err := mirror(filepath.Join(RAMRoot, r), filepath.Join(FlashRoot, r))
			n += c
			if err != nil {
				return n, fmt.Errorf("%s: %w", r, err)
			}
		}
		return n, nil
	}, func() bool {
		for _, r := range rels {
			if differs(filepath.Join(RAMRoot, r), filepath.Join(FlashRoot, r)) {
				return true
			}
		}
		return false
	}, strings.Join(rels, " "))
}

// Pending: the rels whose RAM copy differs from the partition (what Commit would write).
func Pending(rels ...string) []string {
	if !Active() {
		return nil
	}
	var out []string
	for _, r := range rels {
		if differs(filepath.Join(RAMRoot, r), filepath.Join(FlashRoot, r)) {
			out = append(out, r)
		}
	}
	return out
}

// Read: the partition's own copy of rel (what a reboot brings back). With no RAM copy in use: the file itself.
func Read(rel string) ([]byte, error) {
	if !Active() {
		return os.ReadFile(filepath.Join(RAMRoot, rel))
	}
	return os.ReadFile(filepath.Join(FlashRoot, rel))
}

// withWindow: lock, check (under the lock) whether anything is to be written, remount rw, do, sync, remount ro.
func withWindow(do func() (int, error), needed func() bool, what string) (winErr error) {
	inProcess.Lock()
	defer inProcess.Unlock()
	unlock, err := lock()
	if err != nil {
		return err // busy (another save), not read-only: no banner
	}
	defer unlock()
	if !needed() {
		return nil
	}
	windows.Add(1)
	if Trace != nil {
		end := Trace(what)
		defer func() { end(winErr) }()
	}
	if err := Remount(FlashRoot, true); err != nil {
		logf("config window: %s: could not remount the settings partition read-write: %v", what, err)
		return &Error{Err: err}
	}
	n, werr := do()
	syscall.Sync()
	var rerr error
	for i := 0; i < 20; i++ { // a file still open on it (nothing should be): wait a little
		if rerr = Remount(FlashRoot, false); rerr == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if rerr != nil {
		logf("config window: WARNING the settings partition stays read-write: remounting it read-only failed: %v", rerr)
	}
	if werr != nil {
		logf("config window: %s: %v (%d file(s) written), read-only again", what, werr, n)
		return werr
	}
	logf("config window: rw, %d file(s) written (%s), synced, ro", n, what)
	return nil
}

// lock: the cross-program flock on LockPath (waits up to LockWait).
func lock() (func(), error) {
	os.MkdirAll(filepath.Dir(LockPath), 0o755)
	f, err := os.OpenFile(LockPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	end := time.Now().Add(LockWait)
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			f.Close()
			return nil, err
		}
		if time.Now().After(end) {
			f.Close()
			return nil, fmt.Errorf("another save didn't finish within %s", LockWait)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func remount(dir string, rw bool) error {
	flags := uintptr(syscall.MS_REMOUNT | syscall.MS_NOATIME)
	if !rw {
		flags |= syscall.MS_RDONLY
	}
	return syscall.Mount("", dir, "", flags, "")
}

// Hold: a RAM path that stays in RAM for now (not mirrored, the partition's copy left as it is), whatever commit
// names it or its folder: the SC Box II's chip trims while a tuning run tries them. nil: none.
var Hold func(ramPath string) bool

func held(src string) bool { return Hold != nil && Hold(src) }

// skip: names never mirrored: the writers' own temp files and the window's .bak copies.
func skip(name string) bool {
	return strings.HasSuffix(name, ".tmp") || strings.HasSuffix(name, ".bak")
}

// differs: would mirror(src, dst) write anything?
func differs(src, dst string) bool {
	if held(src) {
		return false
	}
	si, serr := os.Lstat(src)
	di, derr := os.Lstat(dst)
	if serr != nil {
		return derr == nil
	}
	if derr != nil {
		return true
	}
	if si.IsDir() != di.IsDir() {
		return true
	}
	if !si.IsDir() {
		a, err1 := os.ReadFile(src)
		b, err2 := os.ReadFile(dst)
		return err1 != nil || err2 != nil || !bytes.Equal(a, b)
	}
	names := map[string]bool{}
	for _, d := range []string{src, dst} {
		es, _ := os.ReadDir(d)
		for _, e := range es {
			if !skip(e.Name()) {
				names[e.Name()] = true
			}
		}
	}
	for n := range names {
		if differs(filepath.Join(src, n), filepath.Join(dst, n)) {
			return true
		}
	}
	return false
}

// mirror: make dst (on the partition) what src (in RAM) is. Returns the files written or removed.
func mirror(src, dst string) (int, error) {
	if held(src) {
		return 0, nil
	}
	si, serr := os.Lstat(src)
	if serr != nil {
		if !os.IsNotExist(serr) {
			return 0, serr
		}
		return remove(dst)
	}
	if si.IsDir() {
		n := 0
		if di, err := os.Lstat(dst); err == nil && !di.IsDir() {
			c, err := remove(dst)
			n += c
			if err != nil {
				return n, err
			}
		}
		if err := os.MkdirAll(dst, 0o700); err != nil {
			return n, err
		}
		names := map[string]bool{}
		for _, d := range []string{src, dst} {
			es, _ := os.ReadDir(d)
			for _, e := range es {
				names[e.Name()] = true
			}
		}
		keys := make([]string, 0, len(names))
		for k := range names {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if strings.HasSuffix(k, ".tmp") { // a writer's leftover on the partition: gone
				if _, err := os.Lstat(filepath.Join(dst, k)); err == nil {
					os.Remove(filepath.Join(dst, k))
				}
				continue
			}
			if strings.HasSuffix(k, ".bak") {
				continue
			}
			c, err := mirror(filepath.Join(src, k), filepath.Join(dst, k))
			n += c
			if err != nil {
				return n, err
			}
		}
		return n, nil
	}
	if !si.Mode().IsRegular() {
		return 0, nil // (no links or devices in the settings)
	}
	b, err := os.ReadFile(src)
	if err != nil {
		return 0, err
	}
	if di, err := os.Lstat(dst); err == nil && di.IsDir() {
		if err := os.RemoveAll(dst); err != nil {
			return 0, err
		}
	}
	if cur, err := os.ReadFile(dst); err == nil && bytes.Equal(cur, b) {
		return 0, nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return 0, err
	}
	return 1, WriteFile(dst, b, si.Mode().Perm())
}

// remove: dst (a file with its .bak and .tmp, or a folder) off the partition.
func remove(dst string) (int, error) {
	di, err := os.Lstat(dst)
	n := 0
	if err == nil {
		if di.IsDir() {
			if err := os.RemoveAll(dst); err != nil {
				return 0, err
			}
		} else if err := os.Remove(dst); err != nil {
			return 0, err
		}
		n = 1
	}
	for _, x := range []string{dst + ".bak", dst + ".tmp"} {
		if os.Remove(x) == nil {
			n++
		}
	}
	if n > 0 {
		syncDir(filepath.Dir(dst))
	}
	return n, nil
}

// WriteFile: b at path crash-safely on a FAT (no atomic rename, no journal): the new content to path.tmp and
// fsync; the current file copied to path.bak and fsync (the last good copy); rename; fsync the folder. A power cut
// at any step leaves the old file, the new one, or the .bak: never a cut-short or empty file.
func WriteFile(path string, b []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := writeSync(tmp, b, mode); err != nil {
		os.Remove(tmp)
		return err
	}
	if cur, err := os.ReadFile(path); err == nil && len(cur) > 0 {
		_ = writeSync(path+".bak", cur, mode)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	syncDir(filepath.Dir(path))
	return nil
}

func writeSync(path string, b []byte, mode os.FileMode) error {
	if mode == 0 {
		mode = 0o600
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
}
