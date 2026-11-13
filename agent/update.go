package main

// Firmware update (Settings -> Firmware update): installs an update package (sclite-update-vX.Y.Z.tar.gz,
// made by the firmware kit's build.py --update-out) over the running system, without an SD card. Settings
// on the config partition are never touched.
//
// A package is a gzipped tar: manifest.json first, then files/<path> for every file it installs. The
// manifest lists each file's sha256 and size, the stock paths a build removes, and a base check (the
// stock Goldshell miner kept at /usr/libexec/sclite/intminer by every sclite build), so a damaged file or
// one for a different miner is refused before anything changes. Packages aren't signed: anyone can build
// and install their own.
//
// Installing: the current version of every path the package touches is saved to the config partition
// (update/backup.tgz, plus update/created: paths the package adds), a "pending" marker is written, then
// each file is written beside its target and renamed over it, or, when the partition hasn't room for both
// copies, written after the old one is removed; every written file is read back and checked. Any error puts
// the backup back. The miner then reboots. The new panel confirms the update once it has run for a while;
// until then the boot script (S48sclite) restores the backup if the panel doesn't come up within 5
// minutes, or on the 4th boot.
//
// The panel's own binary (~10 MB) can't be replaced while it runs from it: its blocks stay in use, and the
// system partition has ~4 MB free. So an update that replaces it is installed by a copy of the panel in
// RAM (/tmp), which the panel becomes (exec, -install-update) once the HTTP answer has gone out. For the
// same reason "Roll back" in the panel only leaves a marker and reboots: the boot script puts the backup
// back before the panel starts.

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

var (
	updateRoot     = "/"                     // tests install into a directory instead
	updateStage    = brandTmp + "update.tgz" // the package waiting to be installed
	updateMaxBytes = int64(48 << 20)
	updateConfirm  = 90 * time.Second // the new panel confirms after running this long
	updateReboot   = func() { rebootNow() }
	updateHTTP     = updateClient()
	updMu          sync.Mutex
	updBusy        string // "" or what's happening ("downloading", "installing")
	updStaged      *updManifest
	updStagedSHA   string // the staged file's sha256
	updStagedSums  string // "" (not compared), "match", "nomatch"; "github": matched the release's SHA256SUMS
	updStagedWarn  string // the release's ⚠ warning (from its notes), shown above Install
	updRepoRe      = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	updateAPI      = "https://api.github.com"
)

// defaultUpdateRepo: where "Check for updates" looks for releases, per firmware (brand_*.go).

type updFile struct {
	Path   string `json:"path"`
	Type   string `json:"type"` // file | link
	Mode   uint32 `json:"mode,omitempty"`
	Size   int64  `json:"size,omitempty"`
	Sha256 string `json:"sha256,omitempty"`
	Target string `json:"target,omitempty"`
}

type updManifest struct {
	Format  int    `json:"format"`
	Product string `json:"product"`
	Version string `json:"version"`
	Base    struct {
		Path   string `json:"path"`
		Sha256 string `json:"sha256"`
	} `json:"base"`
	Dirs []struct {
		Path string `json:"path"`
		Mode uint32 `json:"mode"`
	} `json:"dirs"`
	Files      []updFile `json:"files"`
	Remove     []string  `json:"remove"`
	RemoveDirs []string  `json:"remove_dirs"`
}

func updDir() string           { return filepath.Join(filepath.Dir(*authFile), "update") }
func rootPath(p string) string { return filepath.Join(updateRoot, p) }
func updRepoPath() string      { return filepath.Join(filepath.Dir(*authFile), "update-source") }
func updateRepo() string {
	if b, err := os.ReadFile(updRepoPath()); err == nil {
		if r := strings.TrimSpace(string(b)); updRepoRe.MatchString(r) {
			return r
		}
	}
	return defaultUpdateRepo
}

// firmwareVersion: the "version:" line of /etc/sclite-release ("" for builds before 1.4.6).
func firmwareVersion() string {
	b, _ := os.ReadFile(rootPath(brandRelease))
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "version:") {
			return strings.TrimSpace(strings.TrimPrefix(l, "version:"))
		}
	}
	return ""
}

// paths a package may never write: the settings partition and the kernel's own trees; and names the boot
// script's rollback can't take apart safely (it reads created/created_dirs/tar -t one word at a time):
// letters, digits and ._+-@,~ only, plus BusyBox's "[" and "[[" (a "[" without "]" never globs).
var updNameRe = regexp.MustCompile(`^[A-Za-z0-9._+@,~/\[-]+$`)

func updPathOK(p string) bool {
	if !strings.HasPrefix(p, "/") || path.Clean(p) != p || p == "/" || !updNameRe.MatchString(p) {
		return false
	}
	// "/mnt" already covers the Box II settings partition "/mnt/config"; it is named explicitly so a package
	// can never land config files (or a backup.tgz too big for the 3 MB FAT) on the writable store (B4/R04).
	for _, bad := range []string{"/usr/config", "/mnt/config", "/proc", "/sys", "/dev", "/tmp", "/mnt", "/media"} {
		if p == bad || strings.HasPrefix(p, bad+"/") {
			return false
		}
	}
	return true
}

func openPkg(file string) (*tar.Reader, func(), error) {
	tr, _, done, err := openPkgGz(file)
	return tr, done, err
}

func openPkgGz(file string) (*tar.Reader, *gzip.Reader, func(), error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, nil, nil, err
	}
	gz, err := gzip.NewReader(bufio.NewReader(f))
	if err != nil {
		f.Close()
		return nil, nil, nil, fmt.Errorf("not an update package (not gzip)")
	}
	return tar.NewReader(gz), gz, func() { gz.Close(); f.Close() }, nil
}

// updRealPath: where p ends up once the symlinks on the way are followed: the package's own links first,
// then the ones already on the system (/var/log -> ../tmp, say). With last, p's own name is followed too
// (a folder made with MkdirAll); a file or link replaces its own name, so only the folders above it count.
func updRealPath(p string, links map[string]string, last bool) (string, bool) {
	parts := strings.Split(strings.TrimPrefix(p, "/"), "/")
	name := ""
	if !last {
		name, parts = parts[len(parts)-1], parts[:len(parts)-1]
	}
	cur, hops := "/", 0
	for len(parts) > 0 {
		c := parts[0]
		parts = parts[1:]
		switch c {
		case "", ".":
			continue
		case "..":
			cur = path.Dir(cur)
			continue
		}
		next := path.Join(cur, c)
		t, isLink := links[next]
		if !isLink {
			if fi, err := os.Lstat(rootPath(next)); err == nil && fi.Mode()&os.ModeSymlink != 0 {
				t, err = os.Readlink(rootPath(next))
				isLink = err == nil
			}
		}
		if !isLink {
			cur = next
			continue
		}
		if hops++; hops > 40 {
			return "", false
		}
		if strings.HasPrefix(t, "/") {
			cur = "/"
		}
		parts = append(strings.Split(t, "/"), parts...)
	}
	return path.Join(cur, name), true
}

// verifyUpdate reads the whole package: the manifest, every file against its sha256 and size, nothing
// extra, and the base check against this miner.
func verifyUpdate(file string) (*updManifest, error) {
	tr, gz, done, err := openPkgGz(file)
	if err != nil {
		return nil, err
	}
	defer done()
	h, err := tr.Next()
	if err != nil || h.Name != "manifest.json" || h.Size > 4<<20 {
		return nil, fmt.Errorf("not an update package (no manifest)")
	}
	var m updManifest
	if err := json.NewDecoder(io.LimitReader(tr, 4<<20)).Decode(&m); err != nil {
		return nil, fmt.Errorf("the manifest can't be read: %v", err)
	}
	if m.Format != 1 || m.Product != brand+"-firmware" {
		return nil, fmt.Errorf("not an "+brand+" firmware update (format %d, %q)", m.Format, m.Product)
	}
	want := map[string]updFile{}
	for _, f := range m.Files {
		if !updPathOK(f.Path) || (f.Type != "file" && f.Type != "link") {
			return nil, fmt.Errorf("the package wants to write %q, which isn't allowed", f.Path)
		}
		if f.Type == "file" {
			want["files"+f.Path] = f
		}
	}
	for _, p := range append(append([]string{}, m.Remove...), m.RemoveDirs...) {
		if !updPathOK(p) {
			return nil, fmt.Errorf("the package wants to remove %q, which isn't allowed", p)
		}
	}
	for _, d := range m.Dirs {
		if !updPathOK(d.Path) {
			return nil, fmt.Errorf("the package wants to create %q, which isn't allowed", d.Path)
		}
	}
	// a path both installed and removed is a broken manifest (the install would delete what it just wrote)
	for _, p := range append(append([]string{}, m.Remove...), m.RemoveDirs...) {
		for _, f := range m.Files {
			if f.Path == p {
				return nil, fmt.Errorf("the package both installs and removes %q: it's damaged", p)
			}
		}
	}
	if !updPathOK(m.Base.Path) {
		return nil, fmt.Errorf("not an "+brand+" firmware update (base %q)", m.Base.Path)
	}
	// nothing may land in a forbidden place through a symlink (the package's own, or one on the system)
	links := map[string]string{}
	for _, f := range m.Files {
		if f.Type == "link" {
			links[f.Path] = f.Target
		}
	}
	check := func(p string, last bool) error {
		if rp, ok := updRealPath(p, links, last); !ok || !updPathOK(rp) {
			return fmt.Errorf("the package wants to write %q, which goes through a link to %q: not allowed", p, rp)
		}
		return nil
	}
	for _, f := range m.Files {
		if err := check(f.Path, false); err != nil {
			return nil, err
		}
	}
	for _, p := range append(append([]string{}, m.Remove...), m.RemoveDirs...) {
		if err := check(p, false); err != nil {
			return nil, err
		}
	}
	for _, d := range m.Dirs {
		if err := check(d.Path, true); err != nil {
			return nil, err
		}
	}
	seen := map[string]bool{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("the package is damaged: %v", err)
		}
		f, ok := want[h.Name]
		if !ok || seen[h.Name] || h.Typeflag != tar.TypeReg {
			return nil, fmt.Errorf("the package has an unexpected entry %q", h.Name)
		}
		sum := sha256.New()
		n, err := io.Copy(sum, tr)
		if err != nil {
			return nil, fmt.Errorf("the package is damaged: %v", err)
		}
		if n != f.Size || hex.EncodeToString(sum.Sum(nil)) != f.Sha256 {
			return nil, fmt.Errorf("%s doesn't match its checksum: the file is damaged, download it again", f.Path)
		}
		seen[h.Name] = true
	}
	if len(seen) != len(want) {
		return nil, fmt.Errorf("the package is incomplete (%d of %d files): download it again", len(seen), len(want))
	}
	// to the end of the gzip stream: its checksum (the only one over the manifest) and a download cut short
	if _, err := io.Copy(io.Discard, gz); err != nil {
		return nil, fmt.Errorf("the package is damaged or incomplete (%v): download it again", err)
	}
	var b []byte
	if fi, err := os.Lstat(rootPath(m.Base.Path)); err == nil && (!fi.Mode().IsRegular() || fi.Size() > 64<<20) {
		return nil, fmt.Errorf("this update is for a different miner or %s firmware version than this one", oemName())
	}
	b, err = os.ReadFile(rootPath(m.Base.Path))
	if err != nil {
		return nil, fmt.Errorf("this miner has no %s: install this firmware once from an SD card instead", m.Base.Path)
	}
	if s := sha256.Sum256(b); hex.EncodeToString(s[:]) != m.Base.Sha256 {
		return nil, fmt.Errorf("this update is for a different miner or %s firmware version than this one", oemName())
	}
	return &m, nil
}

var updFreeAt = statfsFree // tests

// Removing a file doesn't free its blocks while a running program was started from it or maps it: the miner
// runs from /usr/libexec/sclite/hook/intminer (the guarded copy, trims on or off), with sclite_bind.so and
// sclite_chip.so. When an install (or putting the backup back) has removed such a file for room, those
// programs are stopped so the blocks come free; the miner reboots after an install anyway. Meanwhile minerd
// starts the miner again through the shim, which runs the stock copy while the guarded one isn't there
// (same pools, same --no-pool-redirect and binding library), and the guarded copy again once it's back.
var (
	updProcDir = "/proc"
	updSignal  = func(pid int, sig syscall.Signal) error { return syscall.Kill(pid, sig) }
	updWaitFor = 6 * time.Second
)

// updHolders: processes started from full or mapping it (also once it's deleted), never this one or init.
func updHolders(full string) []int {
	ents, err := os.ReadDir(updProcDir)
	if err != nil {
		return nil
	}
	self := os.Getpid()
	var pids []int
	for _, e := range ents {
		pid := 0
		if _, err := fmt.Sscanf(e.Name(), "%d", &pid); err != nil || pid <= 1 || pid == self || fmt.Sprint(pid) != e.Name() {
			continue
		}
		dir := filepath.Join(updProcDir, e.Name())
		held := false
		if l, err := os.Readlink(filepath.Join(dir, "exe")); err == nil && (l == full || l == full+" (deleted)") {
			held = true
		}
		if !held {
			if f, err := os.Open(filepath.Join(dir, "maps")); err == nil {
				sc := bufio.NewScanner(f)
				for sc.Scan() {
					l := strings.TrimRight(sc.Text(), " ")
					if strings.HasSuffix(l, " "+full) || strings.HasSuffix(l, " "+full+" (deleted)") {
						held = true
						break
					}
				}
				f.Close()
			}
		}
		if held {
			pids = append(pids, pid)
		}
	}
	return pids
}

// freeHeld: stop what holds the (removed) file at full: TERM, then KILL, waiting until nothing holds it.
func freeHeld(full string) {
	if updateRoot != "/" && updProcDir == "/proc" {
		return // a test's root: never touch real processes
	}
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL} {
		pids := updHolders(full)
		if len(pids) == 0 {
			return
		}
		log.Printf("update: %s is still in use by %v: stopping them (signal %d) so its room comes free", full, pids, sig)
		for _, p := range pids {
			updSignal(p, sig)
		}
		for end := time.Now().Add(updWaitFor); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
			if len(updHolders(full)) == 0 {
				return
			}
		}
	}
}

func statfsFree(p string) int64 {
	var st syscall.Statfs_t
	if syscall.Statfs(p, &st) != nil {
		return -1
	}
	return int64(st.Bavail) * int64(st.Bsize)
}

// ---- squashfs backstop (B7) ----
//
// On the SC Box II the root filesystem is a read-only squashfs image; the SC Lite file-level update and
// rollback cannot work there and would at best fail halfway. The in-panel update/rollback is already gated
// off by model.Feat.FirmwareUpdate, but these low-level writers refuse independently when the target root is
// squashfs, as a hard backstop even if the gate is somehow bypassed. SC Lite (a writable root) is unaffected.

const squashfsMagic = 0x73717368 // SQUASHFS_MAGIC (statfs f_type)

// updDisabledMsg is the one message shown when the panel firmware update/rollback is turned off on this model.
const updDisabledMsg = "firmware update from the panel isn't available on this model — install by SD card; export your settings first (an SD install erases them)"

// errSquashfs is the hard backstop refusal from the low-level writers.
var errSquashfs = errors.New("the system partition is read-only (squashfs): the panel can't install or roll back firmware here — install by SD card, and export your settings first (an SD install erases them)")

// fsMagicAt reads the statfs f_type of p. Replaceable in tests so the squashfs backstop can be exercised
// without a real squashfs mount. f_type is int64 on amd64 and int32 on arm; int64() covers both.
var fsMagicAt = func(p string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(p, &st); err != nil {
		return 0, err
	}
	return int64(st.Type), nil
}

// rootIsSquashfs reports whether the active update root sits on a read-only squashfs. A statfs error is
// treated as "not squashfs" so a transient error never blocks a genuine (writable-root) install.
func rootIsSquashfs() bool {
	m, err := fsMagicAt(updateRoot)
	if err != nil {
		return false
	}
	return m == squashfsMagic
}

// writeBackup saves the current version of every path the package touches (backup.tgz, paths relative to
// / so BusyBox tar can put them back from the boot script) and the paths it adds (created).
func writeBackup(m *updManifest) error {
	dir := updDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := filepath.Join(dir, "backup.tgz.tmp")
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	var created []string
	paths := []string{}
	for _, x := range m.Files {
		paths = append(paths, x.Path)
	}
	paths = append(paths, m.Remove...)
	sort.Strings(paths)
	done := map[string]bool{}
	for _, p := range paths {
		if done[p] {
			continue
		}
		done[p] = true
		full := rootPath(p)
		fi, err := os.Lstat(full)
		if err != nil {
			created = append(created, p)
			continue
		}
		name := strings.TrimPrefix(p, "/")
		switch {
		case fi.Mode()&os.ModeSymlink != 0:
			t, _ := os.Readlink(full)
			err = tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeSymlink, Linkname: t, Mode: 0o777, ModTime: fi.ModTime()})
		case fi.Mode().IsRegular():
			err = tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Size: fi.Size(), Mode: int64(fi.Mode().Perm()), ModTime: fi.ModTime()})
			if err == nil {
				var src *os.File
				if src, err = os.Open(full); err == nil {
					_, err = io.Copy(tw, src)
					src.Close()
				}
			}
		}
		if err != nil {
			tw.Close()
			gz.Close()
			f.Close()
			os.Remove(tmp)
			return fmt.Errorf("saving the current files failed (is the config partition full?): %v", err)
		}
	}
	err = tw.Close()
	if err == nil {
		err = gz.Close()
	}
	if err == nil {
		err = f.Sync()
	}
	f.Close()
	if err != nil {
		os.Remove(tmp)
		return fmt.Errorf("saving the current files failed (is the config partition full?): %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "created"), []byte(strings.Join(created, "\n")+"\n"), 0o644); err != nil {
		return err
	}
	// folders the update will create (deepest first), removed again on a roll back
	nd := map[string]bool{}
	want := []string{}
	for _, d := range m.Dirs {
		want = append(want, d.Path)
	}
	for _, x := range m.Files {
		want = append(want, path.Dir(x.Path))
	}
	for _, d := range want {
		for ; d != "/" && d != "."; d = path.Dir(d) {
			if _, err := os.Lstat(rootPath(d)); err != nil {
				nd[d] = true
			}
		}
	}
	dirs := make([]string, 0, len(nd))
	for d := range nd {
		dirs = append(dirs, d)
	}
	sort.Slice(dirs, func(i, j int) bool {
		if a, b := strings.Count(dirs[i], "/"), strings.Count(dirs[j], "/"); a != b {
			return a > b
		}
		return dirs[i] < dirs[j]
	})
	if err := os.WriteFile(filepath.Join(dir, "created_dirs"), []byte(strings.Join(dirs, "\n")+"\n"), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "backup.tgz"))
}

// restoreBackup puts back what writeBackup saved and removes what the update added.
func restoreBackup() error {
	if rootIsSquashfs() { // hard backstop: can't put a backup back onto a read-only squashfs root
		return errSquashfs
	}
	ro, err := rootRW()
	if err != nil {
		return err
	}
	defer ro()
	dir := updDir()
	tr, done, err := openPkg(filepath.Join(dir, "backup.tgz"))
	if err != nil {
		return fmt.Errorf("no backup to restore: %v", err)
	}
	defer done()
	// first take away what the update added and any half-written side files: that frees room for the restore
	for _, name := range []string{"created", "created_dirs"} { // files first, then folders (deepest first, only if empty)
		if b, err := os.ReadFile(filepath.Join(dir, name)); err == nil {
			for _, p := range strings.Split(string(b), "\n") {
				if p = strings.TrimSpace(p); p != "" && updPathOK(p) {
					os.Remove(rootPath(p))
					os.Remove(rootPath(p) + "." + brand + "-new")
				}
			}
		}
	}
	var firstErr error
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("the backup is damaged: %v", err)
		}
		p := "/" + h.Name
		if !updPathOK(p) {
			continue
		}
		full := rootPath(p)
		os.MkdirAll(filepath.Dir(full), 0o755)
		os.Remove(full + "." + brand + "-new")
		switch h.Typeflag {
		case tar.TypeSymlink:
			os.Remove(full)
			err = os.Symlink(h.Linkname, full)
		case tar.TypeReg:
			err = writeReplace(full, tr, os.FileMode(h.Mode)&0o7777, h.Size, "")
		}
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if b, err := os.ReadFile(filepath.Join(dir, "created_dirs")); err == nil { // folders emptied above
		for _, p := range strings.Split(string(b), "\n") {
			if p = strings.TrimSpace(p); p != "" && updPathOK(p) {
				os.Remove(rootPath(p))
			}
		}
	}
	syscall.Sync()
	return firstErr
}

// writeReplace writes a file beside its target and renames it over (a running binary keeps its old copy).
// When the partition hasn't room for both copies (the panel is ~10 MB, a system partition has ~4 MB free),
// the old file is removed first: the backup on the config partition holds it, and the boot script puts the
// backup back if the miner doesn't come up. The written file is read back and must match want (sha256).
func writeReplace(full string, r io.Reader, mode os.FileMode, size int64, want string) error {
	if free := updFreeAt(filepath.Dir(full)); free >= 0 && free < size+(256<<10) {
		os.Remove(full)
		freeHeld(full)
	}
	if err := writeReplaceSide(full, r, mode); err != nil {
		return err
	}
	if want != "" {
		f, err := os.Open(full)
		if err != nil {
			return err
		}
		h := sha256.New()
		_, err = io.Copy(h, f)
		f.Close()
		if err != nil {
			return err
		}
		if hex.EncodeToString(h.Sum(nil)) != want {
			return fmt.Errorf("%s reads back different from what was written", full)
		}
	}
	return nil
}

func writeReplaceSide(full string, r io.Reader, mode os.FileMode) error {
	tmp := full + "." + brand + "-new"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err = io.Copy(f, r); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp, mode)
	}
	if err == nil {
		err = os.Rename(tmp, full)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

type updMarker struct {
	From string `json:"from"`
	To   string `json:"to"`
	At   int64  `json:"at"`
}

func writeLast(result, from, to, note string) {
	b, _ := json.Marshal(map[string]any{"result": result, "from": from, "to": to, "at": time.Now().Unix(), "note": note})
	os.WriteFile(filepath.Join(updDir(), "last.json"), b, 0o644)
}

// rootRW: the system partition is mounted read-only (Goldshell's inittab remounts / ro at boot); make it
// writable for an install or a roll back, and read-only again after. Only for the real root.
func rootRW() (done func(), err error) {
	if updateRoot != "/" {
		return func() {}, nil
	}
	if err := syscall.Mount("", "/", "", syscall.MS_REMOUNT, ""); err != nil {
		return nil, fmt.Errorf("can't make the system partition writable: %v", err)
	}
	return func() {
		syscall.Sync()
		syscall.Mount("", "/", "", syscall.MS_REMOUNT|syscall.MS_RDONLY, "") // the reboot does it anyway if busy
	}, nil
}

// updRoomNeeded: free space the install needs on the system partition. Files are replaced one at a time
// (removed first when room is short), so it's the growth of the files plus a margin for the largest one
// being written beside its old copy... except that a file a running program was started from keeps its
// old blocks until that program ends: the panel's own binary, unless this is the copy in RAM
// (installHandoff), counts in full.
func updRoomNeeded(m *updManifest) int64 {
	self := selfExe()
	var grow, largest int64
	for _, f := range m.Files {
		if f.Type != "file" {
			continue
		}
		if updSame(f) {
			continue
		}
		old := int64(0)
		if st, err := os.Stat(rootPath(f.Path)); err == nil && st.Mode().IsRegular() && rootPath(f.Path) != self {
			old = st.Size()
		}
		grow += f.Size - old
		if f.Size > largest && f.Size < 1<<20 {
			largest = f.Size
		}
	}
	if grow < 0 {
		grow = 0
	}
	return grow + largest + (512 << 10)
}

// updSame: the file is already on the system as the package has it (content and mode). It isn't written
// again: a running program's file (the miner, 2.2 MB, is in every package) would keep its old blocks until
// the reboot, room the check above can't count on.
func updSame(f updFile) bool {
	if f.Type != "file" {
		return false
	}
	full := rootPath(f.Path)
	st, err := os.Lstat(full)
	if err != nil || !st.Mode().IsRegular() || st.Size() != f.Size || st.Mode().Perm() != os.FileMode(f.Mode)&0o777 {
		return false
	}
	r, err := os.Open(full)
	if err != nil {
		return false
	}
	defer r.Close()
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return false
	}
	return hex.EncodeToString(h.Sum(nil)) == f.Sha256
}

// rebootCmd: "reboot"; tests set <BRAND>_REBOOT (brandEnv) (as the boot script's tests do) to see it without rebooting.
func rebootCmd() string {
	if c := os.Getenv(brandEnv("REBOOT")); c != "" {
		return c
	}
	return "reboot"
}

func selfSize() int64 {
	if st, err := os.Stat(selfExe()); err == nil {
		return st.Size()
	}
	return 0
}

func selfExe() string {
	p, err := os.Executable()
	if err != nil {
		return ""
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

// replacesSelf: the package replaces the binary this panel is running from. The running program keeps that
// file's blocks in use until it ends, and the system partition hasn't room for a second copy; so the
// install runs from a copy of the panel in RAM (installHandoff), which leaves the old file free to go.
func replacesSelf(m *updManifest) bool {
	self := selfExe()
	for _, f := range m.Files {
		if f.Type == "file" && self != "" && rootPath(f.Path) == self {
			return true
		}
	}
	return false
}

// installHandoff: copy this binary to RAM (/tmp) and become that copy (same process, same supervisor in
// S48sclite), running "-install-update FILE": it installs, then reboots. If the install fails, the backup
// goes back and the copy exits; the boot script's loop starts the panel again in 5 s, which shows why.
func installHandoff(file string) error {
	self := selfExe()
	in, err := os.Open(self)
	if err != nil {
		return err
	}
	defer in.Close()
	dst := brandTmp + "agent-install"
	os.Remove(dst)
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o700)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(dst)
		return fmt.Errorf("can't copy the panel to RAM for the install: %v", err)
	}
	go func() {
		time.Sleep(800 * time.Millisecond) // let the HTTP response go out
		args := []string{os.Args[0], "-install-update", file, "-authfile", *authFile}
		watchdogStop() // the exec closes the device: disarm it first (the installer feeds it again if it's on)
		err := syscall.Exec(dst, args, os.Environ())
		log.Printf("update: can't start the installer: %v", err) // only reached if exec failed
		os.Remove(dst)
	}()
	return nil
}

// runInstallUpdate: "sclite-agent -install-update FILE" (the panel's copy in RAM). Installs FILE, then reboots.
func runInstallUpdate(file string) {
	if updIsImage() { // SC Box II: a system image, written by updimage.go (never returns)
		runInstallImage(file)
		return
	}
	rc := recoveryConfigLoad()
	recMu.Lock()
	recCfg = rc // so a watchdog error here saves the owner's settings, not empty ones
	recMu.Unlock()
	// never stuck: whatever happens, the miner reboots within 10 minutes (the boot script then rolls back if needed)
	time.AfterFunc(10*time.Minute, func() {
		log.Printf("update: the install took over 10 minutes: rebooting")
		watchdogStop()
		exec.Command("sh", "-c", "sync; "+rebootCmd()).Run()
	})
	if rc.Watchdog { // a drive that can't be disarmed would reset the board mid-install
		go func() {
			if watchdogStart() == nil {
				log.Printf("update: feeding the hardware watchdog during the install")
			}
		}()
	}
	m, err := installUpdate(file)
	os.Remove(file) // RAM: the package and this copy of the panel (os.Exit skips defers)
	if p := selfExe(); strings.HasPrefix(p, "/tmp/") {
		os.Remove(p)
	}
	if err != nil {
		log.Printf("update: install failed: %v", err)
		watchdogStop() // the panel's restart loop reopens it if it's on
		if updRollbackReboot && updateRoot == "/" {
			markCleanReboot()
			cmd := exec.Command("sh", "-c", "sync; "+rebootCmd())
			cmd.Env = append(os.Environ(), "PATH=/usr/sbin:/usr/bin:/sbin:/bin:"+os.Getenv("PATH"))
			_ = cmd.Run()
			time.Sleep(time.Minute)
		}
		os.Exit(1)
	}
	log.Printf("update: %s installed: rebooting", m.Version)
	watchdogStop()
	markCleanReboot()
	cmd := exec.Command("sh", "-c", "sync; "+rebootCmd())
	cmd.Env = append(os.Environ(), "PATH=/usr/sbin:/usr/bin:/sbin:/bin:"+os.Getenv("PATH"))
	if updateRoot == "/" {
		_ = cmd.Run()
		time.Sleep(time.Minute)
	}
	os.Exit(0)
}

// installUpdate: verify again, back up, mark pending, install. On any error the backup goes back.
func installUpdate(file string) (*updManifest, error) {
	if rootIsSquashfs() { // hard backstop: the SC Box II root is read-only squashfs
		return nil, errSquashfs
	}
	m, err := verifyUpdate(file)
	if err != nil {
		return nil, err
	}
	if need, free := updRoomNeeded(m), statfsFree(updateRoot); free >= 0 && free < need {
		return nil, fmt.Errorf("not enough room on the system partition (%d KB free, %d KB needed)", free>>10, need>>10)
	}
	ro, err := rootRW()
	if err != nil {
		return nil, err
	}
	defer ro()
	if err := writeBackup(m); err != nil {
		return nil, err
	}
	from := firmwareVersion()
	syscall.Sync() // the backup is on disk before anything is marked or replaced
	mk, _ := json.Marshal(updMarker{From: from, To: m.Version, At: time.Now().Unix()})
	if err := writeFileSync(filepath.Join(updDir(), "pending"), mk, 0o644); err != nil {
		return nil, err
	}
	os.Remove(filepath.Join(updDir(), "boots"))
	changed := 0 // files actually replaced or removed so far
	fail := func(e error) (*updManifest, error) {
		note := "nothing was changed"
		keepPending := false
		if changed > 0 {
			note = "the previous version was put back"
			if rerr := restoreBackup(); rerr != nil {
				// keep "pending" and ask the boot script to roll back: it runs before the panel starts, when
				// nothing holds the old files and there's the most room
				keepPending = true
				writeFileSync(filepath.Join(updDir(), "rollback"), []byte("restore failed\n"), 0o644)
				note = "putting the previous version back failed here (" + rerr.Error() + "): the miner reboots and puts it back while starting"
				updRollbackReboot = true
			}
		}
		if !keepPending {
			os.Remove(filepath.Join(updDir(), "pending"))
		}
		writeLast("failed", from, m.Version, e.Error()+"; "+note)
		return nil, fmt.Errorf("%v; %s", e, note)
	}
	for _, d := range m.Dirs {
		if err := os.MkdirAll(rootPath(d.Path), os.FileMode(d.Mode)&0o7777|0o700); err != nil {
			return fail(err)
		}
	}
	files := map[string]updFile{}
	for _, f := range m.Files {
		if f.Type == "link" {
			full := rootPath(f.Path)
			os.MkdirAll(filepath.Dir(full), 0o755)
			tmp := full + "." + brand + "-new"
			os.Remove(tmp)
			if err := os.Symlink(f.Target, tmp); err != nil {
				return fail(err)
			}
			if err := os.Rename(tmp, full); err != nil {
				os.Remove(tmp)
				return fail(err)
			}
			changed++
			continue
		}
		if !updSame(f) {
			files["files"+f.Path] = f
		}
	}
	tr, done, err := openPkg(file)
	if err != nil {
		return fail(err)
	}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			done()
			return fail(err)
		}
		f, ok := files[h.Name]
		if !ok {
			continue
		}
		full := rootPath(f.Path)
		os.MkdirAll(filepath.Dir(full), 0o755)
		changed++ // before: a failed write may already have removed or replaced the old file
		if err := writeReplace(full, tr, os.FileMode(f.Mode)&0o7777, f.Size, f.Sha256); err != nil {
			done()
			return fail(fmt.Errorf("writing %s: %v", f.Path, err))
		}
	}
	done()
	for _, p := range m.Remove {
		os.Remove(rootPath(p))
	}
	for _, d := range m.RemoveDirs {
		os.Remove(rootPath(d)) // only if empty
	}
	syscall.Sync()
	return m, nil
}

// confirmUpdateLater: started with the panel. A pending update is confirmed once this (new) panel has run
// for a while; the boot script's rollback then leaves it alone.
func confirmUpdateLater() {
	imgConfirmAfterBoot() // SC Box II: only after an image install (its marker)
	pf := filepath.Join(updDir(), "pending")
	b, err := os.ReadFile(pf)
	if err != nil {
		return
	}
	var mk updMarker
	json.Unmarshal(b, &mk)
	go func() {
		time.Sleep(updateConfirm)
		// only a panel that answers on its port AND is reached from the network confirms (the page that started
		// the install keeps asking, so it does within seconds of the reboot). A miner that mines but can't be
		// reached isn't confirmed: the boot script rolls back 5 minutes after the start.
		// (on the monotonic clock: after the update's reboot the clock reads 1970 until NTP or the browser sets
		// it, and a time server stepping back a clock set from a browser left every later hit "too old" until the
		// boot script rolled the update back)
		since := monoSec() - int64(updateConfirm/time.Second)
		for !panelAnswers() || (lastNetHit.Load() < since && os.Getenv(brandEnv("CONFIRM_LOCAL")) == "") {
			log.Printf("update: not confirming yet: the panel %s", map[bool]string{true: "hasn't been reached from the network", false: "isn't answering on " + *listenAddr}[panelAnswers()])
			time.Sleep(15 * time.Second)
		}
		if _, err := os.Stat(pf); err != nil {
			return
		}
		os.Remove(pf)
		os.Remove(filepath.Join(updDir(), "boots"))
		v := firmwareVersion()
		writeLast("installed", mk.From, v, "")
		activity.emit("⬆ firmware updated to " + v + map[bool]string{true: " (from " + mk.From + ")", false: ""}[mk.From != ""])
	}()
}

var updRollbackReboot bool

// lastNetHit: when a request last came from another machine (not the miner itself), in monoSec().
var lastNetHit atomic.Int64

// backupReadable: the whole backup reads back (gzip and tar intact).
func backupReadable() error {
	tr, done, err := openPkg(filepath.Join(updDir(), "backup.tgz"))
	if err != nil {
		return err
	}
	defer done()
	for {
		_, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err := io.Copy(io.Discard, tr); err != nil {
			return err
		}
	}
}

// panelAnswers: this panel's own port serves a page (a plain request from the miner itself).
var panelAnswers = func() bool {
	port := portOf(*listenAddr)
	if port == 0 {
		port = 8080
	}
	c := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	r, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d/", port))
	if err != nil {
		return false
	}
	r.Body.Close()
	return r.StatusCode < 500
}

func rebootNow() {
	markCleanReboot()
	go func() {
		time.Sleep(800 * time.Millisecond) // let the HTTP response flush first
		cmd := exec.Command("sh", "-c", "sync; "+rebootCmd())
		cmd.Env = append(os.Environ(), "PATH=/usr/sbin:/usr/bin:/sbin:/bin:"+os.Getenv("PATH"))
		_ = cmd.Run()
	}()
}

// ---- GitHub releases ----

func updateClient() *http.Client {
	c := notifyClient()
	c.Timeout = 5 * time.Minute
	// unlike notifications (no redirects), this client must follow them: GitHub serves every release file
	// (browser_download_url) as a 302 to release-assets.githubusercontent.com. Only GETs without secrets go
	// through here; https is never left for plain http.
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("too many redirects")
		}
		if via[0].URL.Scheme == "https" && req.URL.Scheme != "https" {
			return errors.New("redirect from https to " + req.URL.Scheme + " refused")
		}
		return nil
	}
	if t, ok := c.Transport.(*http.Transport); ok {
		t.Proxy = http.ProxyFromEnvironment // a proxy, if the network needs one (HTTPS_PROXY)
	}
	return c
}

type ghRelease struct {
	Tag       string `json:"tag_name"`
	Name      string `json:"name"`
	URL       string `json:"html_url"`
	Published string `json:"published_at"`
	Body      string `json:"body"`
	Assets    []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
		Size int64  `json:"size"`
	} `json:"assets"`
}

func latestRelease(repo string) (*ghRelease, error) {
	return getRelease(repo, "latest", 25*time.Second, fmt.Errorf("no release found on %s", repo))
}

// errNoSuchRelease: GitHub answered 404 for a release asked for by its tag (releaseByTag).
var errNoSuchRelease = errors.New("no such release")

var relTagRe = regexp.MustCompile(`^v?[0-9][0-9A-Za-z.\-]*$`)

// releaseByTag: the release tagged tag (e.g. "v0.5.0") of repo; errNoSuchRelease when there is none.
func releaseByTag(repo, tag string, timeout time.Duration) (*ghRelease, error) {
	if !relTagRe.MatchString(tag) {
		return nil, errNoSuchRelease
	}
	return getRelease(repo, "tags/"+tag, timeout, errNoSuchRelease)
}

// getRelease: GET /repos/<repo>/releases/<which>; notFound is the error for a 404.
func getRelease(repo, which string, timeout time.Duration, notFound error) (*ghRelease, error) {
	req, _ := http.NewRequest("GET", updateAPI+"/repos/"+repo+"/releases/"+which, nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", brandAgent)
	ctx, cancel := context.WithTimeout(context.Background(), timeout) // the release info is small
	defer cancel()
	resp, err := updateHTTP.Do(req.WithContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("can't reach GitHub: %v", shortErr(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		return nil, notFound
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("GitHub answered %s", resp.Status)
	}
	var r ghRelease
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&r); err != nil {
		return nil, err
	}
	return &r, nil
}

var updAssetRe = regexp.MustCompile(`^sclite-update-v?[0-9][0-9A-Za-z.\-]*\.tar\.gz$`)

func (r *ghRelease) asset(match func(string) bool) (string, string, int64) {
	for _, a := range r.Assets {
		if match(a.Name) {
			return a.Name, a.URL, a.Size
		}
	}
	return "", "", 0
}

// versionLess compares dotted versions ("1.4.6" < "1.4.10"); a leading v is ignored, and so is a label
// after a part's digits ("1.5.11-test" compares as 1.5.11, so a test build isn't offered older releases).
func versionLess(a, b string) bool {
	pa := strings.Split(strings.TrimPrefix(a, "v"), ".")
	pb := strings.Split(strings.TrimPrefix(b, "v"), ".")
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var x, y int
		if i < len(pa) {
			x = leadInt(pa[i])
		}
		if i < len(pb) {
			y = leadInt(pb[i])
		}
		if x != y {
			return x < y
		}
	}
	return false
}

// leadInt: the number a version part starts with ("11-test" → 11, "x" → 0).
func leadInt(s string) int {
	n := 0
	for i, c := range s {
		if c < '0' || c > '9' || i >= 9 { // 9 digits at most: no overflow
			break
		}
		n = n*10 + int(c-'0')
	}
	return n
}

func download(url, dst string, max int64) (string, error) {
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("User-Agent", brandAgent)
	resp, err := updateHTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("download failed: %v", shortErr(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("download failed: %s", resp.Status)
	}
	f, err := os.Create(dst)
	if err != nil {
		return "", err
	}
	defer f.Close()
	sum := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, sum), io.LimitReader(resp.Body, max+1))
	if err != nil {
		return "", fmt.Errorf("download failed: %v", shortErr(err))
	}
	if n > max {
		return "", fmt.Errorf("the file is larger than an update can be")
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

// fetchLatest downloads the latest release's update package, checks it against the release's SHA256SUMS
// and stages it.
func fetchLatest() (*updManifest, error) {
	rel, err := latestRelease(updateRepo())
	if err != nil {
		return nil, err
	}
	name, url, _ := rel.asset(updAssetRe.MatchString)
	if url == "" {
		return nil, fmt.Errorf("release %s has no update file (sclite-update-….tar.gz): flash its SD image instead", rel.Tag)
	}
	got, err := download(url, updateStage, updateMaxBytes)
	if err != nil {
		os.Remove(updateStage) // /tmp is RAM
		return nil, err
	}
	updMu.Lock()
	updStagedSHA, updStagedSums, updStagedWarn = got, "", releaseWarningFor(rel.Body, firmwareVersion())
	updMu.Unlock()
	if _, surl, _ := rel.asset(func(n string) bool { return n == "SHA256SUMS" }); surl != "" {
		sums := updateStage + ".sums"
		if _, err := download(surl, sums, 1<<20); err == nil {
			b, _ := os.ReadFile(sums)
			os.Remove(sums)
			want := ""
			for _, l := range strings.Split(string(b), "\n") {
				if f := strings.Fields(l); len(f) == 2 && strings.TrimPrefix(f[1], "*") == name {
					want = f[0]
				}
			}
			if want != "" && want != got {
				os.Remove(updateStage)
				return nil, fmt.Errorf("the downloaded file doesn't match the release's SHA256SUMS: try again")
			}
			if want == got {
				updMu.Lock()
				updStagedSums = "github"
				updMu.Unlock()
			}
		}
	}
	m, err := verifyUpdate(updateStage)
	if err != nil {
		os.Remove(updateStage)
	}
	return m, err
}

// ---- HTTP ----

// rollbackNeedsPassword: the stored password is in the BLAKE2b format, which panels before v1.5.22 can't read.
func rollbackNeedsPassword() bool {
	authMu.RLock()
	defer authMu.RUnlock()
	return auth.B2Hash != ""
}

// passwordForOlder: before an older panel comes back (a roll back, or installing a version before v1.5.22),
// check the password and save it the way that panel reads it. nil: nothing needed, or done.
func passwordForOlder(r *http.Request, pw, to string) error {
	if !rollbackNeedsPassword() {
		return nil
	}
	ip := remoteIP(r)
	if pw == "" {
		return errors.New("enter your password: " + to + " needs it saved its own way")
	}
	if !loginAttempt(ip, "panel") {
		return errors.New("too many wrong passwords from this address: try again in 10 minutes")
	}
	loginSem <- struct{}{}
	ok := checkPassword(pw)
	<-loginSem
	loginDone(ip, "panel", ok)
	if !ok {
		return errors.New("that isn't the password: nothing was changed")
	}
	if err := authToLegacy(pw); err != nil {
		return fmt.Errorf("couldn't save the password for %s (%v): nothing was changed", to, err)
	}
	return nil
}

func updView() map[string]any {
	v := map[string]any{"ok": true, "version": firmwareVersion(), "repo": updateRepo(), "default_repo": defaultUpdateRepo,
		"rollback_needs_password": rollbackNeedsPassword()}
	if secHardened { // installing and changing the update source ask for the panel password (access.go)
		v["install_needs_password"], v["repo_needs_password"] = true, true
	}
	if b, err := os.ReadFile(filepath.Join(updDir(), "last.json")); err == nil {
		var last map[string]any
		if json.Unmarshal(b, &last) == nil {
			v["last"] = last
		}
	}
	if b, err := os.ReadFile(filepath.Join(updDir(), "pending")); err == nil {
		var mk updMarker
		json.Unmarshal(b, &mk)
		v["pending"] = mk
	}
	if _, err := os.Stat(filepath.Join(updDir(), "backup.tgz")); err == nil {
		v["can_rollback"] = true
	}
	m, ok := loadMigration156()
	if ok && unsetStamp(m.At) && clockOK() {
		// made at the upgrade's first start, before the clock was set (1970): its month starts now, not 56
		// years ago (the note, and with it the way to put the presets back, never showed)
		m.At = time.Now().Unix()
		saveMigration156(m)
	}
	if ok && (m.Presets > 0 || m.Stock == "applied" || m.SchedulePaused) &&
		time.Since(time.Unix(m.At, 0)) < 30*24*time.Hour { // the note stays a month after the upgrade
		v["migration"] = m
	}
	updMu.Lock()
	v["busy"] = updBusy
	if updStaged != nil {
		var size int64
		for _, f := range updStaged.Files {
			size += f.Size
		}
		v["staged"] = map[string]any{"version": updStaged.Version, "files": len(updStaged.Files), "size": size, "remove": len(updStaged.Remove),
			"sha256": updStagedSHA, "sums": updStagedSums, "warning": updStagedWarn}
	}
	updMu.Unlock()
	return v
}

func updSetBusy(s string) bool {
	updMu.Lock()
	defer updMu.Unlock()
	if s != "" && updBusy != "" {
		return false
	}
	updBusy = s
	return true
}

// POST /api/update/upload: the package as the request body. It's checked and staged, not installed.
func handleUpdateUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, updView())
		return
	}
	if featOff(model.Feat.FirmwareUpdate) { // SC Box II: SD-only, don't even accept the package
		writeJSON(w, map[string]any{"ok": false, "disabled": true, "err": updDisabledMsg})
		return
	}
	if updIsImage() { // SC Box II (FirmwareUpdate commissioned): a .ius system image
		handleImageUpload(w, r)
		return
	}
	if !updSetBusy("receiving") {
		writeJSON(w, map[string]any{"ok": false, "err": "an update is already in progress"})
		return
	}
	defer updSetBusy("")
	updMu.Lock() // the staged file is overwritten now: whatever was staged is gone
	updStaged, updStagedSHA, updStagedSums, updStagedWarn = nil, "", "", ""
	updMu.Unlock()
	f, err := os.Create(updateStage)
	h := sha256.New()
	if err == nil {
		var n int64
		n, err = io.Copy(io.MultiWriter(f, h), io.LimitReader(r.Body, updateMaxBytes+1))
		f.Close()
		if err == nil && n > updateMaxBytes {
			err = fmt.Errorf("the file is larger than an update can be")
		}
	}
	if err != nil {
		os.Remove(updateStage)
		writeJSON(w, map[string]any{"ok": false, "err": "upload failed: " + err.Error()})
		return
	}
	m, err := verifyUpdate(updateStage)
	updMu.Lock()
	updStaged, updBusy = m, ""
	updStagedSHA, updStagedSums, updStagedWarn = hex.EncodeToString(h.Sum(nil)), "", ""
	updMu.Unlock()
	if err != nil {
		os.Remove(updateStage)
		writeJSON(w, map[string]any{"ok": false, "err": err.Error()})
		return
	}
	writeJSON(w, updView())
}

// GET: state. POST {check}, {fetch}, {install}, {discard}, {rollback}, {repo}.
func handleUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		v := updViewAny()
		if featOff(model.Feat.FirmwareUpdate) { // SC Box II: let the UI hide update/rollback, show export/import
			v["disabled"] = true
			v["disabled_reason"] = updDisabledMsg
		}
		writeJSON(w, v)
		return
	}
	var in struct {
		Check, Fetch, Install, Discard, Rollback bool
		RestorePresets                           bool `json:"restore_presets"`
		Repo                                     *string
		Sums                                     *string // SHA256SUMS (or one line of it) pasted from the release page
		Password                                 string  // a rollback (or an older version installed): the password, saved for the older panel; SC Box II: an install or a new update source needs it (access.go)
		Verify, Ack                              bool    // SC Box II image: dry run; the owner's "SD card ready" confirmation
	}
	if !decodeIn(w, r, &in) {
		return
	}
	// SC Box II: every firmware action (check/fetch/install/rollback/sums/repo/discard) is refused with the
	// SD-install message. restore_presets is a Presets-tab migration, not firmware, so it stays available.
	if featOff(model.Feat.FirmwareUpdate) && !in.RestorePresets {
		writeJSON(w, map[string]any{"ok": false, "disabled": true, "err": updDisabledMsg})
		return
	}
	if updIsImage() && !in.RestorePresets && in.Repo == nil { // SC Box II (FirmwareUpdate commissioned): updimage.go
		handleImageAction(w, r, imgIn{Check: in.Check, Fetch: in.Fetch, Install: in.Install, Discard: in.Discard,
			Rollback: in.Rollback, Verify: in.Verify, Ack: in.Ack, Sums: in.Sums, Password: in.Password})
		return
	}
	fail := func(e error) { writeJSON(w, map[string]any{"ok": false, "err": e.Error()}) }
	switch {
	case in.RestorePresets:
		n, err := restorePresets156()
		if err != nil {
			fail(err)
			return
		}
		activity.emit(fmt.Sprintf("🧹 %d preset(s) from before v1.5.6 put back", n))
		v := updViewAny()
		v["note"] = fmt.Sprintf("%d preset(s) put back on the Presets page", n)
		writeJSON(w, v)
		return
	case in.Sums != nil:
		updMu.Lock()
		sha := updStagedSHA
		if updStaged == nil || sha == "" {
			updMu.Unlock()
			fail(errors.New("no update is waiting: check a file first"))
			return
		}
		res, n := sumsMatch(*in.Sums, sha)
		if n == 0 {
			updMu.Unlock()
			fail(errors.New("no SHA-256 checksum found in that text: paste the line for sclite-update-….tar.gz from SHA256SUMS"))
			return
		}
		updStagedSums = res
		updMu.Unlock()
	case in.Repo != nil:
		repo := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(*in.Repo, "https://github.com/"), "github.com/"))
		repo = strings.TrimSuffix(repo, "/")
		if repo != "" && !updRepoRe.MatchString(repo) {
			fail(errors.New("enter the GitHub repository as owner/name"))
			return
		}
		// SC Box II: where the firmware comes from is the owner's to change: the password again (M1)
		if secHardened && repo != strings.TrimSpace(readFileString(updRepoPath())) {
			if err := checkPanelPassword(r, in.Password, "change the update source"); err != nil {
				fail(err)
				return
			}
		}
		if repo == "" {
			os.Remove(updRepoPath())
		} else if !updRepoRe.MatchString(repo) {
			fail(errors.New("enter the GitHub repository as owner/name"))
			return
		} else if err := os.WriteFile(updRepoPath(), []byte(repo+"\n"), 0o644); err != nil {
			fail(err)
			return
		}
	case in.Check:
		rel, err := latestRelease(updateRepo())
		if err != nil {
			fail(err)
			return
		}
		cur := firmwareVersion()
		name, _, size := rel.asset(updAssetRe.MatchString)
		body := rel.Body
		if len(body) > 3000 {
			body = body[:3000] + "…"
		}
		v := updView()
		v["latest"] = map[string]any{"tag": rel.Tag, "name": rel.Name, "url": rel.URL, "published": rel.Published,
			"notes": body, "warning": releaseWarningFor(rel.Body, firmwareVersion()), "asset": name, "size": size, "newer": cur == "" || versionLess(cur, rel.Tag)}
		writeJSON(w, v)
		return
	case in.Fetch:
		if !updSetBusy("downloading") {
			fail(errors.New("an update is already in progress"))
			return
		}
		m, err := fetchLatest()
		updMu.Lock()
		updStaged, updBusy = m, ""
		updMu.Unlock()
		if err != nil {
			fail(err)
			return
		}
	case in.Discard:
		updMu.Lock()
		updStaged = nil
		updMu.Unlock()
		os.Remove(updateStage)
	case in.Install:
		updMu.Lock()
		staged := updStaged != nil
		updMu.Unlock()
		if !staged {
			fail(errors.New("no update is waiting: choose a file or check for updates first"))
			return
		}
		if secHardened {
			if err := checkPanelPassword(r, in.Password, "install firmware"); err != nil {
				fail(err)
				return
			}
		}
		if _, err := os.Stat(filepath.Join(updDir(), "pending")); err == nil {
			// installing now would replace the backup of the last good version with this unconfirmed one
			fail(errors.New("the last update isn't confirmed yet (the panel confirms it about 90 s after starting): try again in a minute"))
			return
		}
		if !updSetBusy("installing") {
			fail(errors.New("an update is already in progress"))
			return
		}
		if getCharge() == "tuner" {
			updSetBusy("")
			fail(errors.New("the tuner is running: stop it first"))
			return
		}
		if sctRunning() { // an older panel wouldn't put the raised chips back
			updSetBusy("")
			fail(errors.New("the strong-chip test is running (Controls → Chip trims): stop it first"))
			return
		}
		updMu.Lock()
		st := updStaged
		updMu.Unlock()
		if st != nil && versionLess(st.Version, "1.5.22") { // it can't read the BLAKE2b hash (see setPassword)
			if err := passwordForOlder(r, in.Password, "v"+strings.TrimPrefix(st.Version, "v")); err != nil {
				updSetBusy("")
				fail(err)
				return
			}
			defer func() {
				if _, err := os.Stat(filepath.Join(updDir(), "pending")); err != nil && !replacesSelf(st) {
					authKeepLegacy.Store(false) // not installed: the next sign-in may move it to BLAKE2b again
				}
			}()
		}
		if updateRoot == "/" && st != nil && replacesSelf(st) {
			if need, free := updRoomNeeded(st), statfsFree(updateRoot); free >= 0 && free+selfSize() < need {
				updSetBusy("")
				fail(fmt.Errorf("not enough room on the system partition (%d KB free, %d KB needed)", free>>10, (need-selfSize())>>10))
				return
			}
			if err := installHandoff(updateStage); err != nil {
				updSetBusy("")
				fail(err)
				return
			}
			v := st.Version
			activity.emit("⬆ installing firmware update " + v + ": the miner reboots when it's done")
			recordMark("RB", "firmware update "+v)
			markReboot(150*time.Second, 6*time.Minute, "Installing firmware "+v+": the miner is rebooting.")
			writeJSON(w, map[string]any{"ok": true, "note": "installing " + v + ": the miner reboots when it's done"})
			return
		}
		m, err := installUpdate(updateStage)
		updMu.Lock()
		updBusy, updStaged = "", nil
		updMu.Unlock()
		os.Remove(updateStage)
		if err != nil {
			fail(err)
			return
		}
		activity.emit("⬆ firmware update " + m.Version + " installed: rebooting")
		recordMark("RB", "firmware update "+m.Version)
		markReboot(150*time.Second, 6*time.Minute, "Installing firmware "+m.Version+": the miner is rebooting.")
		updateReboot()
		writeJSON(w, map[string]any{"ok": true, "note": "installed " + m.Version + ": the miner is rebooting"})
		return
	case in.Rollback:
		if rootIsSquashfs() { // hard backstop: the boot-script rollback can't rewrite a read-only squashfs root
			fail(errSquashfs)
			return
		}
		if !updSetBusy("rolling back") {
			fail(errors.New("an update is already in progress"))
			return
		}
		if sctRunning() { // the older panel doesn't know the test: its raised chips would stay raised
			updSetBusy("")
			fail(errors.New("the strong-chip test is running (Controls → Chip trims): stop it first"))
			return
		}
		// The boot script puts the backup back, before the panel starts: while this panel runs, its binary's
		// blocks stay in use and the system partition hasn't room for the old one beside it.
		if _, err := os.Stat(filepath.Join(updDir(), "backup.tgz")); err != nil {
			updSetBusy("")
			fail(errors.New("there is no previous version to go back to"))
			return
		}
		if err := backupReadable(); err != nil { // the boot script's tar wouldn't notice a damaged one
			updSetBusy("")
			fail(fmt.Errorf("the backup of the previous version is damaged (%v): not rolling back", err))
			return
		}
		if err := passwordForOlder(r, in.Password, "the older version"); err != nil { // it can't read the BLAKE2b hash
			updSetBusy("")
			fail(err)
			return
		}
		if err := os.WriteFile(filepath.Join(updDir(), "rollback"), []byte(firmwareVersion()+"\n"), 0o644); err != nil {
			authKeepLegacy.Store(false)
			updSetBusy("")
			fail(err)
			return
		}
		syscall.Sync()
		activity.emit("⬇ rolling the firmware back: rebooting")
		markReboot(150*time.Second, 6*time.Minute, "Rolling the firmware back: the miner is rebooting.")
		updateReboot()
		writeJSON(w, map[string]any{"ok": true, "note": "the miner is rebooting and puts the previous version back as it starts"})
		return
	}
	writeJSON(w, updViewAny())
}

// sumsMatch: is sha among the 64-hex-digit checksums in text (SHA256SUMS, one line of it, or a bare
// checksum)? n = how many checksums the text had.
func sumsMatch(text, sha string) (string, int) {
	n := 0
	for _, f := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f')
	}) {
		if len(f) == 64 {
			n++
			if f == sha {
				return "match", n
			}
		}
	}
	return "nomatch", n
}

// releaseWarning: the "> **⚠ …" block quote at the top of a release's notes (a release that changes
// something on install, as v1.5.6 cleared presets), so the panel can show it outside "What's new" and
// again above Install. Plain text with **bold** kept for the panel to render.
func releaseWarning(body string) string {
	var out []string
	in := false
	for _, l := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		t := strings.TrimSpace(l)
		if !in {
			if strings.HasPrefix(t, ">") && strings.Contains(t, "⚠") {
				in = true
			} else {
				continue
			}
		}
		if !strings.HasPrefix(t, ">") {
			break
		}
		out = append(out, strings.TrimSpace(strings.TrimPrefix(t, ">")))
	}
	w := strings.TrimSpace(strings.Join(out, "\n"))
	if len(w) > 2000 {
		w = w[:2000] + "…"
	}
	return w
}

// releaseWarningFor: the release's warning, unless it says who it's for ("Coming from v1.5.5 or earlier")
// and this miner is past that: a miner on v1.5.6 doesn't need v1.5.6's one-time warning in every later
// release's notes.
func releaseWarningFor(body, installed string) string {
	w := releaseWarning(body)
	if w == "" || installed == "" {
		return w
	}
	first := strings.SplitN(w, "\n", 2)[0]
	if m := reWarnUpTo.FindStringSubmatch(first); m != nil && versionLess(m[1], installed) {
		return ""
	}
	return w
}

var reWarnUpTo = regexp.MustCompile(`(?i)from v?(\d+\.\d+\.\d+) or (?:earlier|older)`)
