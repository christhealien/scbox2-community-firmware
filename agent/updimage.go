package main

// System-image update from the panel (SC Box II, commissioning switch FirmwareUpdate, test C9).
//
// The SC Lite installs a file-level package onto its writable root (update.go). The SC Box II's root is a
// read-only squashfs on its own SPI-flash disk, /dev/spiblock1 ("system", 18,874,368 bytes; kernel log:
// "spi add disk dev system 820000 1200000"). Its firmware is an OEM "+ius" container (kit/tools/ius.py),
// which every port build makes from the stock 2.2.1 container by replacing ONLY the system item.
//
// What this does, and nothing else (see C9 REPORT.md for the evidence and the safety argument):
//   - Takes a .ius (upload, or the latest GitHub release's <brand>-firmware-v….ius) into RAM (/tmp), and checks
//     it: header CRC and layout exactly as the stock burner reads it, the md5 tag of every item, every item
//     except the system one byte-identical to the stock SC Box II 2.2.1 boot items (so it is made for this
//     model, and the system will run on the kernel this unit already has), and the system item: a gzip
//     squashfs 4.0 of the stock recipe that fits the partition. Before an install its SHA-256 must match the
//     release's SHA256SUMS: downloaded with it, looked up online for an uploaded file (the release its name's
//     version tags, imgLookupOnline), or pasted by the owner when that can't run.
//   - "Verify" (dry run; the panel runs it by itself once a file is in): every check above, the online look-up,
//     plus the device checks below, no writes.
//   - Install: the panel becomes its copy in RAM (installHandoff, as on the SC Lite), which depends on nothing
//     on the root any more, reads the whole system partition into RAM (the old image, for a write-back),
//     reads every running program's files through once (so their pages are cached), writes ONLY the system
//     item to /dev/spiblock1 from offset 0 (exactly where and how the stock burner puts it), syncs, drops the
//     device's cache, reads the WHOLE partition back and compares it (new image + the untouched tail). Any
//     write or compare error writes the old image back and checks it. Then it reboots (a system call, nothing
//     from the root is started again).
//   - Never written: the boot disk (uboot0, items, kernel: /dev/spiblock0), the boot environment, the config
//     partition (apart from the panel's own result files). An SD card with a known-good image always restores
//     the miner: the bootloader burns the card before the system partition is ever read.

import (
	"bufio"
	"bytes"
	"crypto/md5"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

const (
	iusSector     = 512
	iusMaxBytes   = int64(24 << 20) // stock container 17.3 MB; boot items 3.7 MB + an 18 MB system at most
	box2SysDev    = "/dev/spiblock1"
	box2SysBytes  = int64(18874368) // items.st system.18432.fs; kernel: "spi add disk dev system 820000 1200000"
	imgChunk      = 64 << 10        // the write/compare unit (one SPI-NOR erase block)
	imgSyncEvery  = 1 << 20
	sqMagic       = 0x73717368 // "hsqs"
	sqGzip        = 1
	sqStockBlock  = 131072 // the stock recipe (R03: gzip, -b 131072)
	imgMarginMiB  = 48     // RAM left over for the system while the install runs
	imgHardReboot = 20 * time.Minute
)

// The SC Box II 2.2.1 container's items other than the system one (md5 of the item data, from
// base/box2/fw/firmware.ius; kit/build_box2.py's verify_only_system_changed keeps every one byte-identical
// in every port build). Key: "boot" for the BootROM descriptor, else "type<N>". A container with any other
// item, or one of these different, is refused: it isn't a port image for this model.
var iusPinnedMD5 = map[string]string{
	"boot":  "9248f78762288b4b546fbdd59da6ca2c", // uboot0.isi (BootROM copy)
	"type0": "9248f78762288b4b546fbdd59da6ca2c", // uboot0.isi
	"type1": "16f28664f441f222185dda4cf8171e37", // items.itm (items.st: system.18432.fs, config.3072.fs.fat, ...)
	"type2": "def68c2db4f0bf081b7176c36f56b616", // ramdisk.img (card burns only)
	"type3": "b63b7b4b4902ca99b1f76f297172e2f1", // uImage (the kernel this unit runs)
}

// test seams (production values: the real devices)
var (
	imgDevPath      = box2SysDev
	imgPartBytes    = box2SysBytes
	imgStage        = brandTmp + "update.ius"
	imgReqPath      = brandTmp + "update-ius.json"
	imgCmdlinePath  = "/proc/cmdline"
	imgMeminfoPath  = "/proc/meminfo"
	imgProcDir      = "/proc"
	imgNeedBlockDev = true
	imgRootStatfs   = func() (syscall.Statfs_t, error) {
		var s syscall.Statfs_t
		err := syscall.Statfs("/", &s)
		return s, err
	}
	imgReboot     = func() { syscall.Sync(); syscall.Reboot(syscall.LINUX_REBOOT_CMD_RESTART) }
	imgExit       = os.Exit
	imgFlushDev   = func(f *os.File) { blkFlushBuf(f) }
	imgDropCaches = func() { syscall.Sync(); os.WriteFile("/proc/sys/vm/drop_caches", []byte("3"), 0) } // tests: no-op
	imgWriteHook  func(off int64, b []byte) error                                                       // tests: inject a write fault
	imgReadHook   func(off int64, b []byte)                                                             // tests: corrupt what is read back
	imgSelfBytes  = func() int64 { return selfSize() }
	imgStagedInfo *iusInfo // under updMu
	imgStagedVer  string
	imgStagedSums string // "" | "github" (downloaded) | "online" (uploaded, looked up) | "match" (pasted) | "nomatch"
	imgSumsFrom   string // who said so: "github" | "online" | "pasted" ("" while nothing is known)
	imgSumsTag    string // the release the file was compared with ("v0.5.0"), when looked up or downloaded
	imgLookup     string // the online look-up of an uploaded file: "" (not tried) | "done" | "unavailable"
	imgLookupWhy  string // why it was unavailable, in plain words
	imgStagedOK   string // the miner's own checks (Verify): "" (not run yet) | "ok" | "fail"
	imgStagedWarn string
	imgStagedSame string // "" (not compared yet) | "same" | "different": vs the installed system
	imgLastCheck  []imgCheck
	imgHandoff    = installHandoff   // tests: never exec
	imgMarkClean  = markCleanReboot  // tests: no hashrate/events files
	imgLookupWait = 10 * time.Second // the online look-up of an uploaded file's release, per request
)

// updIsImage: this model updates by system image (the SC Box II), not by file package (the SC Lite).
func updIsImage() bool { return model.ID == "scbox2" }

func iusAssetRe() *regexp.Regexp {
	return regexp.MustCompile(`^` + regexp.QuoteMeta(brand) + `-firmware-v?([0-9][0-9A-Za-z.\-]*)\.ius$`)
}

func imgPendingPath() string { return filepath.Join(updDir(), "image-pending") }

// ---- the container ----

type iusInfo struct {
	Size      int64  `json:"size"`
	SHA256    string `json:"sha256"`
	SWVersion string `json:"sw_version"` // the OEM's field ("2.2.1" in every port build)
	SysOff    int64  `json:"sys_off"`
	SysLen    int64  `json:"sys_len"`
	SysMD5    string `json:"sys_md5"`
	SysSHA256 string `json:"sys_sha256"`
	SqUsed    int64  `json:"sq_bytes_used"`
	SqInodes  uint32 `json:"sq_inodes"`
	SqMkfs    uint32 `json:"sq_mkfs_time"`
}

type sqSuper struct {
	Inodes, Mkfs, BlockSize, Frags               uint32
	Comp, BlockLog, Flags, NoIDs, Major, Minor   uint16
	RootInode, BytesUsed, IDTable, XattrTable    uint64
	InodeTable, DirTable, FragTable, LookupTable uint64
}

func parseSqSuper(b []byte) (sqSuper, error) {
	var s sqSuper
	if len(b) < 96 || binary.LittleEndian.Uint32(b) != sqMagic {
		return s, errors.New("not a squashfs (no hsqs magic)")
	}
	le := binary.LittleEndian
	s.Inodes, s.Mkfs, s.BlockSize, s.Frags = le.Uint32(b[4:]), le.Uint32(b[8:]), le.Uint32(b[12:]), le.Uint32(b[16:])
	s.Comp, s.BlockLog, s.Flags, s.NoIDs, s.Major, s.Minor = le.Uint16(b[20:]), le.Uint16(b[22:]), le.Uint16(b[24:]), le.Uint16(b[26:]), le.Uint16(b[28:]), le.Uint16(b[30:])
	s.RootInode, s.BytesUsed, s.IDTable, s.XattrTable = le.Uint64(b[32:]), le.Uint64(b[40:]), le.Uint64(b[48:]), le.Uint64(b[56:])
	s.InodeTable, s.DirTable, s.FragTable, s.LookupTable = le.Uint64(b[64:]), le.Uint64(b[72:]), le.Uint64(b[80:]), le.Uint64(b[88:])
	return s, nil
}

// checkSystemSquashfs: the system item is a squashfs the SC Box II kernel mounts (4.0, gzip: the stock
// image's compressor; the stock recipe's 128 KiB blocks, no xattrs) and is whole (its tables inside it).
func checkSystemSquashfs(b []byte, itemLen int64) (sqSuper, error) {
	s, err := parseSqSuper(b)
	if err != nil {
		return s, fmt.Errorf("the system item is %v", err)
	}
	switch {
	case s.Major != 4 || s.Minor != 0:
		return s, fmt.Errorf("the system item is squashfs %d.%d, not 4.0", s.Major, s.Minor)
	case s.Comp != sqGzip:
		return s, fmt.Errorf("the system item is compressed with method %d, not gzip: this kernel couldn't mount it", s.Comp)
	case s.BlockSize != sqStockBlock || s.BlockLog != 17:
		return s, fmt.Errorf("the system item has %d-byte blocks, not the stock 131072", s.BlockSize)
	case s.Inodes == 0 || s.BytesUsed < 1<<20:
		return s, errors.New("the system item is empty or truncated")
	case int64(s.BytesUsed) > itemLen:
		return s, fmt.Errorf("the system item is cut short (%d bytes, its squashfs says %d)", itemLen, s.BytesUsed)
	case s.XattrTable != ^uint64(0):
		return s, errors.New("the system item has extended attributes (the stock recipe has none)")
	}
	for _, t := range []uint64{s.IDTable, s.InodeTable, s.DirTable} {
		if t < 96 || t >= s.BytesUsed {
			return s, errors.New("the system item's squashfs tables lie outside it: it's damaged")
		}
	}
	if s.InodeTable >= s.DirTable {
		return s, errors.New("the system item's squashfs tables are out of order: it's damaged")
	}
	return s, nil
}

func iusHeaderCRC(h []byte) uint32 {
	c := append([]byte(nil), h[:iusSector]...)
	copy(c[0x18:0x1c], []byte{0, 0, 0, 0})
	return crc32.ChecksumIEEE(c)
}

// parseIUS checks a whole container (see the top of this file) and describes its system item. It reads the
// file twice at most; nothing is written.
func parseIUS(f io.ReaderAt, size int64) (*iusInfo, error) {
	if size < 4*iusSector || size > iusMaxBytes {
		return nil, fmt.Errorf("that file is %d bytes: not an %s firmware image (.ius)", size, brandProduct)
	}
	h := make([]byte, iusSector)
	if _, err := f.ReadAt(h, 0); err != nil {
		return nil, fmt.Errorf("can't read the file: %v", err)
	}
	if string(h[:4]) != "+ius" {
		return nil, fmt.Errorf("not a firmware image (.ius): no +ius header (the .img is for SD cards)")
	}
	if got, want := iusHeaderCRC(h), binary.LittleEndian.Uint32(h[0x18:]); got != want {
		return nil, fmt.Errorf("the image header is damaged (CRC %08x, should be %08x): download it again", got, want)
	}
	n := int(binary.LittleEndian.Uint32(h[0x1c:]))
	if n < 1 || 0x20+16*n > iusSector {
		return nil, fmt.Errorf("the image header lists %d items: damaged", n)
	}
	info := &iusInfo{Size: size, SWVersion: strings.TrimRight(string(h[0x10:0x18]), "\x00")}
	seen := map[string]bool{}
	sysFound := false
	for i := 0; i < n; i++ {
		d := h[0x20+16*i:]
		w0, w1, ln := binary.LittleEndian.Uint32(d), binary.LittleEndian.Uint32(d[4:]), int64(binary.LittleEndian.Uint32(d[12:]))
		flags, off := w0>>24, int64(w0&0xffffff)*iusSector
		var key string
		switch flags {
		case 0x01:
			key = "boot"
		case 0x82:
			key = "type" + strconv.Itoa(int(w1))
		default:
			return nil, fmt.Errorf("the image has an item of an unknown kind (%#x): not a %s image", flags, brandProduct)
		}
		if ln == 0 {
			return nil, fmt.Errorf("the image has an empty %s item: not a %s image", key, brandProduct)
		}
		if off < iusSector || off+ln+64 > size {
			return nil, fmt.Errorf("the image's %s item lies outside the file: it's cut short, download it again", key)
		}
		if seen[key] {
			return nil, fmt.Errorf("the image has two %s items: not a %s image", key, brandProduct)
		}
		seen[key] = true
		sum := md5.New()
		if _, err := io.Copy(sum, io.NewSectionReader(f, off, ln)); err != nil {
			return nil, fmt.Errorf("can't read the file: %v", err)
		}
		m := sum.Sum(nil)
		tag := make([]byte, 16)
		if _, err := f.ReadAt(tag, off+ln); err != nil || !bytes.Equal(tag, m) {
			return nil, fmt.Errorf("the image's %s item doesn't match its checksum: it's damaged, download it again", key)
		}
		if key == "type5" {
			if ln%iusSector != 0 {
				return nil, fmt.Errorf("the system item is %d bytes, not whole 512-byte sectors: not a port image", ln)
			}
			if ln > box2SysBytes {
				return nil, fmt.Errorf("the system item is %d bytes: larger than the %d-byte system partition", ln, box2SysBytes)
			}
			sb := make([]byte, 96)
			if _, err := f.ReadAt(sb, off); err != nil {
				return nil, err
			}
			s, err := checkSystemSquashfs(sb, ln)
			if err != nil {
				return nil, err
			}
			sh := sha256.New()
			io.Copy(sh, io.NewSectionReader(f, off, ln))
			info.SysOff, info.SysLen, info.SysMD5, info.SysSHA256 = off, ln, hex.EncodeToString(m), hex.EncodeToString(sh.Sum(nil))
			info.SqUsed, info.SqInodes, info.SqMkfs = int64(s.BytesUsed), s.Inodes, s.Mkfs
			sysFound = true
			continue
		}
		want, ok := iusPinnedMD5[key]
		if !ok {
			return nil, fmt.Errorf("the image has a %s item a %s port image never has: refused (only the system item may differ from stock)", key, brandProduct)
		}
		if hex.EncodeToString(m) != want {
			return nil, fmt.Errorf("the image's %s item isn't the %s 2.2.1 one: it's for another model or %s version, refused", key, brandProduct, oemName())
		}
	}
	for k := range iusPinnedMD5 {
		if !seen[k] {
			return nil, fmt.Errorf("the image has no %s item: not a %s image", k, brandProduct)
		}
	}
	if !sysFound {
		return nil, errors.New("the image has no system item")
	}
	sh := sha256.New()
	if _, err := io.Copy(sh, io.NewSectionReader(f, 0, size)); err != nil {
		return nil, err
	}
	info.SHA256 = hex.EncodeToString(sh.Sum(nil))
	return info, nil
}

func parseIUSFile(p string) (*iusInfo, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	return parseIUS(f, st.Size())
}

// ---- the device ----

func blkIoctl(f *os.File, req uintptr, arg uintptr) error {
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), req, arg); e != 0 {
		return e
	}
	return nil
}

// _IOR(0x12, 114, size_t) and _IO(0x12, 97)
var (
	blkGetSize64 = uintptr(0x80001272 | uint32(unsafe.Sizeof(uintptr(0)))<<16)
	blkFlsBuf    = uintptr(0x1261)
)

// devBytes: the size of the disk (BLKGETSIZE64) or, for a test's plain file, its length.
func devBytes(f *os.File) (int64, error) {
	st, err := f.Stat()
	if err != nil {
		return 0, err
	}
	if st.Mode()&os.ModeDevice == 0 {
		if imgNeedBlockDev {
			return 0, fmt.Errorf("%s isn't a block device", f.Name())
		}
		return st.Size(), nil
	}
	var n uint64
	if err := blkIoctl(f, blkGetSize64, uintptr(unsafe.Pointer(&n))); err != nil {
		return 0, fmt.Errorf("can't read the size of %s: %v", f.Name(), err)
	}
	return int64(n), nil
}

// blkFlushBuf: write back and drop the disk's cached blocks, so the read-back comes from the flash. Pages a
// mounted filesystem holds stay cached (the kernel keeps them); everything else is read again.
func blkFlushBuf(f *os.File) {
	f.Sync()
	if st, err := f.Stat(); err == nil && st.Mode()&os.ModeDevice != 0 {
		blkIoctl(f, blkFlsBuf, 0) // BLKFLSBUF: sync, then invalidate_bdev
	}
}

type imgCheck struct {
	OK   bool   `json:"ok"`
	Warn bool   `json:"warn,omitempty"` // not a refusal, but read it
	Msg  string `json:"msg"`
}

// memAvailBytes: memory the install can use. Linux 3.10 has no MemAvailable: MemFree + Buffers + Cached,
// minus Shmem (files in /tmp are RAM that can't be given back).
func memAvailBytes() int64 {
	f, err := os.Open(imgMeminfoPath)
	if err != nil {
		return -1
	}
	defer f.Close()
	v := map[string]int64{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fs := strings.Fields(sc.Text())
		if len(fs) >= 2 {
			n, _ := strconv.ParseInt(fs[1], 10, 64)
			v[strings.TrimSuffix(fs[0], ":")] = n << 10
		}
	}
	if a, ok := v["MemAvailable"]; ok {
		return a
	}
	if _, ok := v["MemFree"]; !ok {
		return -1
	}
	return v["MemFree"] + v["Buffers"] + v["Cached"] - v["Shmem"]
}

// imgRAMNeeded: the new system image and the whole old partition held in RAM, the panel's copy in /tmp
// (when not running from it already), and a margin for everything else.
func imgRAMNeeded(info *iusInfo, inRAM bool) int64 {
	n := info.SysLen + imgPartBytes + imgMarginMiB<<20
	if !inRAM {
		n += imgSelfBytes()
	}
	return n
}

// imgDeviceChecks: /dev/spiblock1 is the 18 MB system disk the running root was mounted from, and it can
// take this image. Read-only. same: whether the installed system already is this image ("" when not read).
func imgDeviceChecks(info *iusInfo) (checks []imgCheck, ok bool, same string) {
	ok = true
	add := func(good bool, msg string) {
		checks = append(checks, imgCheck{OK: good, Msg: msg})
		if !good {
			ok = false
		}
	}
	cl, err := os.ReadFile(imgCmdlinePath)
	fields := strings.Fields(string(cl))
	has := func(s string) bool {
		for _, f := range fields {
			if f == s {
				return true
			}
		}
		return false
	}
	add(err == nil && has("root="+box2SysDev) && has("rootfstype=squashfs"),
		fmt.Sprintf("the kernel started the system from %s as squashfs (boot command line)", box2SysDev))
	rs, rerr := imgRootStatfs()
	add(rerr == nil && int64(rs.Type) == squashfsMagic, "the running system is a read-only squashfs")
	f, err := os.Open(imgDevPath)
	if err != nil {
		add(false, fmt.Sprintf("can't open %s: %v", imgDevPath, err))
		return checks, false, ""
	}
	defer f.Close()
	sz, err := devBytes(f)
	add(err == nil && sz == imgPartBytes, fmt.Sprintf("%s is the %d-byte system partition (it is %d bytes)", imgDevPath, imgPartBytes, sz))
	if !ok {
		return checks, false, ""
	}
	sb := make([]byte, 96)
	_, err = f.ReadAt(sb, 0)
	cur, serr := parseSqSuper(sb)
	if err != nil || serr != nil {
		add(false, fmt.Sprintf("%s doesn't start with a squashfs: not touching it", imgDevPath))
		return checks, false, ""
	}
	blocks := ((cur.BytesUsed - 1) >> cur.BlockLog) + 1
	add(rerr == nil && uint64(rs.Files) == uint64(cur.Inodes) && uint64(rs.Blocks) == blocks && uint64(rs.Bsize) == uint64(cur.BlockSize),
		fmt.Sprintf("%s holds the very system that is running (inodes %d, blocks %d)", imgDevPath, cur.Inodes, blocks))
	add(info.SysLen <= sz, fmt.Sprintf("the new system (%d bytes) fits the partition (%d bytes)", info.SysLen, sz))
	if !ok {
		return checks, false, ""
	}
	h := sha256.New()
	if _, err := io.Copy(h, io.NewSectionReader(f, 0, info.SysLen)); err != nil {
		add(false, fmt.Sprintf("reading %s failed: %v", imgDevPath, err))
		return checks, false, ""
	}
	if hex.EncodeToString(h.Sum(nil)) == info.SysSHA256 {
		same = "same"
		checks = append(checks, imgCheck{OK: true, Warn: true, Msg: "the installed system is byte-identical to this image: installing rewrites the same bytes (a safe test of the write path; nothing changes)"})
	} else {
		same = "different"
		checks = append(checks, imgCheck{OK: true, Msg: "the installed system differs from this image: installing replaces it"})
	}
	return checks, ok, same
}

// imgPreflight: everything checked before an install (and by "Verify only"). inRAM: the caller is the
// installer copy already (its own RAM is counted in what /proc/meminfo shows).
func imgPreflight(info *iusInfo, inRAM bool) ([]imgCheck, bool, string) {
	checks := []imgCheck{{OK: true, Msg: fmt.Sprintf("image: +ius header CRC, every item's md5 tag, boot items identical to the %s 2.2.1 ones, system = gzip squashfs 4.0 of %d bytes (%d used)", brandProduct, info.SysLen, info.SqUsed)}}
	dc, ok, same := imgDeviceChecks(info)
	checks = append(checks, dc...)
	need, avail := imgRAMNeeded(info, inRAM), memAvailBytes()
	good := avail >= need
	checks = append(checks, imgCheck{OK: good, Msg: fmt.Sprintf("free memory %d MB, the install needs %d MB", avail>>20, need>>20)})
	return checks, ok && good, same
}

// ---- the write ----

type imgWriteResult struct {
	Written, Rewritten int
	Restored           bool
	ReadBack           bool
}

// writeSystemImage writes img at offset 0 of the system disk (the whole old partition kept in RAM first),
// reads the WHOLE partition back and compares it: img, then the old bytes after it. A write or compare
// failure writes the old bytes back and compares again. Never writes past len(img) or outside this disk.
func writeSystemImage(dev string, img []byte, partBytes int64) (res imgWriteResult, err error) {
	if int64(len(img))%iusSector != 0 || int64(len(img)) > partBytes || len(img) < 1<<20 {
		return res, fmt.Errorf("refusing to write a %d-byte system image", len(img))
	}
	if filepath.Clean(dev) != filepath.Clean(imgDevPath) {
		return res, fmt.Errorf("refusing to write %s: only %s, the system disk", dev, imgDevPath)
	}
	f, err := os.OpenFile(dev, os.O_RDWR, 0) // never O_EXCL: the running system is mounted from it
	if err != nil {
		return res, fmt.Errorf("can't open %s for writing: %v", dev, err)
	}
	defer f.Close()
	sz, err := devBytes(f)
	if err != nil {
		return res, err
	}
	if sz != partBytes {
		return res, fmt.Errorf("%s is %d bytes, not the %d-byte system partition: nothing written", dev, sz, partBytes)
	}
	old := make([]byte, sz)
	if _, err := f.ReadAt(old, 0); err != nil {
		return res, fmt.Errorf("can't read the current system (%v): nothing written", err)
	}
	if binary.LittleEndian.Uint32(old) != sqMagic {
		return res, fmt.Errorf("%s doesn't hold a squashfs now: nothing written", dev)
	}
	// what the partition must hold after a good write: the image, then the old bytes. A chunk can straddle the
	// image's end (its length is a multiple of 512, not of the 64 KiB chunk: stock 0xced000, v0.3.2 0x9c8000).
	want := func(off int64, n int) []byte {
		end, l := off+int64(n), int64(len(img))
		switch {
		case end <= l:
			return img[off:end]
		case off >= l:
			return old[off:end]
		}
		b := make([]byte, n)
		copy(b, img[off:])
		copy(b[l-off:], old[l:end])
		return b
	}
	restoring := false
	writeRange := func(src []byte) error { // src at offset 0, chunk by chunk
		synced := 0
		for off := 0; off < len(src); off += imgChunk {
			end := off + imgChunk
			if end > len(src) {
				end = len(src)
			}
			if imgWriteHook != nil && !restoring {
				if err := imgWriteHook(int64(off), src[off:end]); err != nil {
					return err
				}
			}
			if _, err := f.WriteAt(src[off:end], int64(off)); err != nil {
				return fmt.Errorf("writing at %#x: %v", off, err)
			}
			if end-synced >= imgSyncEvery {
				if err := f.Sync(); err != nil {
					return fmt.Errorf("flushing at %#x: %v", off, err)
				}
				synced = end
				log.Printf("update: system image %d/%d KB written", end>>10, len(src)>>10)
			}
		}
		return f.Sync()
	}
	// compare reads the partition back through a NEW descriptor, after the writer's descriptor is synced and
	// the kernel's caches are dropped: the v0.5.0 install read its own cached writes back and passed while the
	// chip held something else (2026-11-12, SQUASHFS errors in the panel binary after the reboot).
	compare := func(expect func(int64, int) []byte) (bad []int64, err error) {
		f.Sync()
		imgFlushDev(f)
		rf, err := os.Open(dev)
		if err != nil {
			return nil, fmt.Errorf("reopening %s to read it back: %v", dev, err)
		}
		defer rf.Close()
		imgFlushDev(rf)
		imgDropCaches()
		buf := make([]byte, imgChunk)
		for off := int64(0); off < sz; off += imgChunk {
			n := int64(imgChunk)
			if off+n > sz {
				n = sz - off
			}
			if _, err := rf.ReadAt(buf[:n], off); err != nil {
				return nil, fmt.Errorf("reading back at %#x: %v", off, err)
			}
			if imgReadHook != nil {
				imgReadHook(off, buf[:n])
			}
			if !bytes.Equal(buf[:n], expect(off, int(n))) {
				bad = append(bad, off)
			}
		}
		return bad, nil
	}
	restore := func(why error) (imgWriteResult, error) {
		log.Printf("update: %v: writing the previous system back", why)
		restoring = true
		if err := writeRange(old[:len(img)]); err != nil {
			return res, fmt.Errorf("%v; writing the previous system back failed too (%v): install by SD card", why, err)
		}
		bad, err := compare(func(off int64, n int) []byte { return old[off : off+int64(n)] })
		if err != nil || len(bad) > 0 {
			return res, fmt.Errorf("%v; the previous system written back doesn't read back right (%d blocks, %v): install by SD card", why, len(bad), err)
		}
		res.Restored = true
		return res, fmt.Errorf("%v; the previous system was written back and reads back right", why)
	}
	if err := writeRange(img); err != nil {
		return restore(err)
	}
	res.Written = len(img)
	bad, err := compare(want)
	if err != nil {
		return restore(err)
	}
	if len(bad) > 0 { // once more, just those blocks
		log.Printf("update: %d blocks read back different: writing them again", len(bad))
		for _, off := range bad {
			n := int64(imgChunk)
			if off+n > sz {
				n = sz - off
			}
			if _, err := f.WriteAt(want(off, int(n)), off); err != nil {
				return restore(fmt.Errorf("rewriting at %#x: %v", off, err))
			}
		}
		if err := f.Sync(); err != nil {
			return restore(err)
		}
		res.Rewritten = len(bad)
		if bad, err = compare(want); err != nil || len(bad) > 0 {
			return restore(fmt.Errorf("the system partition reads back different from what was written (%d blocks)", len(bad)))
		}
	}
	res.ReadBack = true
	return res, nil
}

// prewarmRunning reads, once, every file a running program was started from or maps from the root, so its
// pages are in memory while the system disk changes under it. Returns files and bytes read.
func prewarmRunning() (int, int64) {
	var rootDev uint64
	if st, err := os.Stat("/"); err == nil {
		rootDev = uint64(st.Sys().(*syscall.Stat_t).Dev)
	}
	ents, _ := os.ReadDir(imgProcDir)
	seen := map[string]bool{}
	var files int
	var total int64
	read := func(p string) {
		p = strings.TrimSuffix(p, " (deleted)")
		if seen[p] || !strings.HasPrefix(p, "/") || total > 96<<20 {
			return
		}
		seen[p] = true
		st, err := os.Stat(p)
		if err != nil || !st.Mode().IsRegular() || uint64(st.Sys().(*syscall.Stat_t).Dev) != rootDev {
			return
		}
		if f, err := os.Open(p); err == nil {
			n, _ := io.Copy(io.Discard, f)
			f.Close()
			files++
			total += n
		}
	}
	for _, e := range ents {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		dir := filepath.Join(imgProcDir, e.Name())
		if l, err := os.Readlink(filepath.Join(dir, "exe")); err == nil {
			read(l)
		}
		if f, err := os.Open(filepath.Join(dir, "maps")); err == nil {
			sc := bufio.NewScanner(f)
			for sc.Scan() {
				if i := strings.Index(sc.Text(), " /"); i >= 0 {
					read(strings.TrimSpace(sc.Text()[i:]))
				}
			}
			f.Close()
		}
	}
	return files, total
}

// ---- the installer (the panel's copy in RAM) ----

type imgRequest struct {
	SHA256  string `json:"sha256"`
	Version string `json:"version"`
	From    string `json:"from"`
}

type imgMarker struct {
	From      string `json:"from"`
	To        string `json:"to"`
	At        int64  `json:"at"`
	SysLen    int64  `json:"sys_len"`
	SysSHA256 string `json:"sys_sha256"`
}

// runInstallImage: "<agent> -install-update FILE" on the SC Box II. Never returns.
func runInstallImage(file string) {
	time.AfterFunc(imgHardReboot, func() {
		log.Printf("update: the system image install took over %v: rebooting", imgHardReboot)
		imgReboot()
	})
	var req imgRequest
	b, _ := os.ReadFile(imgReqPath)
	json.Unmarshal(b, &req)
	os.Remove(imgReqPath)
	m, err := installImage(file, req)
	os.Remove(file)
	if p := selfExe(); strings.HasPrefix(p, "/tmp/") {
		os.Remove(p)
	}
	switch {
	case err == nil:
		log.Printf("update: system image %s written and read back: rebooting", m.To)
	case errors.Is(err, errImgNothingWritten):
		log.Printf("update: %v", err)
		imgExit(1) // the system is as it was: the boot script starts the panel again
		return
	default:
		log.Printf("update: %v: rebooting", err)
	}
	os.WriteFile(recCleanPath(), []byte(time.Now().Format(time.RFC3339)), 0o644)
	imgReboot()
	time.Sleep(time.Minute)
	imgExit(0)
}

var errImgNothingWritten = errors.New("nothing was written")

// installImage: check again, then write. err wraps errImgNothingWritten when the disk wasn't touched.
func installImage(file string, req imgRequest) (*imgMarker, error) {
	from := firmwareVersion()
	os.MkdirAll(updDir(), 0o755)
	fail0 := func(e error) (*imgMarker, error) {
		writeLast("failed", from, req.Version, e.Error()+"; nothing was changed")
		return nil, fmt.Errorf("%v: %w", e, errImgNothingWritten)
	}
	if featOff(model.Feat.FirmwareUpdate) || !updIsImage() {
		return fail0(errors.New("firmware update from the panel is switched off on this miner"))
	}
	info, err := parseIUSFile(file)
	if err != nil {
		return fail0(err)
	}
	if req.SHA256 == "" || info.SHA256 != req.SHA256 {
		return fail0(errors.New("the image changed after it was checked"))
	}
	checks, ok, _ := imgPreflight(info, true)
	if !ok {
		for _, c := range checks {
			if !c.OK {
				return fail0(errors.New(c.Msg))
			}
		}
		return fail0(errors.New("a check failed"))
	}
	img := make([]byte, info.SysLen)
	f, err := os.Open(file)
	if err == nil {
		_, err = f.ReadAt(img, info.SysOff)
		f.Close()
	}
	if err != nil {
		return fail0(err)
	}
	if s := sha256.Sum256(img); hex.EncodeToString(s[:]) != info.SysSHA256 {
		return fail0(errors.New("the system item changed in memory"))
	}
	os.Remove(file) // RAM back for the old partition's copy
	n, sz := prewarmRunning()
	log.Printf("update: cached %d running programs' files (%d KB)", n, sz>>10)
	mk := imgMarker{From: from, To: req.Version, At: time.Now().Unix(), SysLen: info.SysLen, SysSHA256: info.SysSHA256}
	os.MkdirAll(updDir(), 0o755)
	mb, _ := json.Marshal(mk)
	if err := cfgWrite(imgPendingPath(), mb, 0o644); err != nil {
		return fail0(fmt.Errorf("can't save the update marker on the config partition: %v", err))
	}
	syscall.Sync()
	log.Printf("update: writing the %d-byte system image to %s", len(img), imgDevPath)
	res, err := writeSystemImage(imgDevPath, img, imgPartBytes)
	if err != nil {
		os.Remove(imgPendingPath()) // nothing to confirm after the reboot: last.json says what happened
		writeLast("failed", from, req.Version, err.Error())
		return nil, err
	}
	writeLast("written", from, req.Version, fmt.Sprintf("system image written and the whole partition read back (%d blocks rewritten once); checked again after the reboot", res.Rewritten))
	syscall.Sync()
	return &mk, nil
}

// imgConfirmAfterBoot: started with the panel. After an image install, the system the miner booted is
// compared with what was written; the result goes to the last-update note.
func imgConfirmAfterBoot() {
	b, err := os.ReadFile(imgPendingPath())
	if err != nil {
		return
	}
	var mk imgMarker
	if json.Unmarshal(b, &mk) != nil || mk.SysLen <= 0 {
		os.Remove(imgPendingPath())
		return
	}
	go func() {
		res, note := "installed", "the system partition reads back as the image written"
		f, err := os.Open(imgDevPath)
		if err == nil {
			h := sha256.New()
			_, err = io.Copy(h, io.NewSectionReader(f, 0, mk.SysLen))
			f.Close()
			if err == nil && hex.EncodeToString(h.Sum(nil)) != mk.SysSHA256 {
				res, note = "failed", "the system partition isn't the image that was written: install by SD card if anything misbehaves"
			}
		}
		if err != nil {
			res, note = "installed", "started; the system partition couldn't be read back ("+err.Error()+")"
		}
		os.Remove(imgPendingPath())
		writeLast(res, mk.From, mk.To, note)
		activity.emit("⬆ firmware image " + map[bool]string{true: mk.To, false: "update"}[mk.To != ""] + ": " + note)
	}()
}

// ---- HTTP (only reached with the feature on, on the SC Box II) ----

func imgView() map[string]any {
	v := updView()
	v["image"] = true
	v["can_rollback"] = false
	delete(v, "pending")
	delete(v, "staged")
	delete(v, "migration")
	v["partition"] = imgPartBytes
	v["asset_pattern"] = brand + "-firmware-v….ius"
	updMu.Lock()
	if imgStagedInfo != nil {
		v["staged"] = map[string]any{"version": imgStagedVer, "size": imgStagedInfo.Size, "sha256": imgStagedInfo.SHA256,
			"sys_len": imgStagedInfo.SysLen, "sys_used": imgStagedInfo.SqUsed, "sys_sha256": imgStagedInfo.SysSHA256,
			"sums": imgStagedSums, "sums_from": imgSumsFrom, "sums_tag": imgSumsTag, "lookup": imgLookup, "lookup_why": imgLookupWhy,
			"genuine": imgSumsOK(imgStagedSums), "ready": imgStagedOK,
			"warning": imgStagedWarn, "same": imgStagedSame, "checks": imgLastCheck}
	}
	updMu.Unlock()
	return v
}

func imgClearStaged() {
	imgStagedInfo, imgStagedVer, imgStagedSums, imgStagedWarn, imgStagedSame, imgLastCheck = nil, "", "", "", "", nil
	imgSumsFrom, imgSumsTag, imgLookup, imgLookupWhy, imgStagedOK = "", "", "", "", ""
}

// imgSumsOK: the staged file's SHA-256 matched a release's SHA256SUMS (downloaded with it, looked up online
// for an uploaded file, or pasted by the owner). Required before an install.
func imgSumsOK(sums string) bool { return sums == "github" || sums == "online" || sums == "match" }

// a browser's copy number ("… (1).ius") isn't part of the name the release published
var imgCopySuffix = regexp.MustCompile(`\s*\(\d+\)(\.ius)$`)

func imgVersionOf(name string) string {
	name = imgCopySuffix.ReplaceAllString(filepath.Base(name), "$1")
	if m := iusAssetRe().FindStringSubmatch(name); m != nil {
		return m[1]
	}
	return ""
}

// POST /api/update/upload?name=FILE: the .ius as the body; checked and kept in RAM, not installed.
func handleImageUpload(w http.ResponseWriter, r *http.Request) {
	if a := memAvailBytes(); a >= 0 && a < iusMaxBytes+imgMarginMiB<<20 {
		writeJSON(w, map[string]any{"ok": false, "err": fmt.Sprintf("not enough free memory to take an image (%d MB free)", a>>20)})
		return
	}
	if !updSetBusy("receiving") {
		writeJSON(w, map[string]any{"ok": false, "err": "an update is already in progress"})
		return
	}
	defer updSetBusy("")
	updMu.Lock()
	imgClearStaged()
	updMu.Unlock()
	f, err := os.Create(imgStage)
	if err == nil {
		var n int64
		n, err = io.Copy(f, io.LimitReader(r.Body, iusMaxBytes+1))
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err == nil && n > iusMaxBytes {
			// read (and drop) the rest, up to a bound, so the browser gets this answer instead of a reset
			// connection (an SD image is ~30 MB)
			io.Copy(io.Discard, io.LimitReader(r.Body, 64<<20))
			err = errors.New("the file is larger than a firmware image can be (the .img is for SD cards)")
		}
	}
	if err != nil {
		os.Remove(imgStage)
		writeJSON(w, map[string]any{"ok": false, "err": "upload failed: " + err.Error()})
		return
	}
	info, err := parseIUSFile(imgStage)
	if err != nil {
		os.Remove(imgStage)
		writeJSON(w, map[string]any{"ok": false, "err": err.Error()})
		return
	}
	updMu.Lock()
	imgStagedInfo, imgStagedVer = info, imgVersionOf(r.URL.Query().Get("name"))
	updMu.Unlock()
	writeJSON(w, imgView())
}

func imgFetchLatest() error {
	rel, err := latestRelease(updateRepo())
	if err != nil {
		return err
	}
	name, url, _ := rel.asset(iusAssetRe().MatchString)
	if url == "" {
		return fmt.Errorf("release %s has no %s firmware image (%s-firmware-v….ius)", rel.Tag, brandProduct, brand)
	}
	if a := memAvailBytes(); a >= 0 && a < iusMaxBytes+imgMarginMiB<<20 {
		return fmt.Errorf("not enough free memory to download an image (%d MB free)", a>>20)
	}
	got, err := download(url, imgStage, iusMaxBytes)
	if err != nil {
		os.Remove(imgStage)
		return err
	}
	sums := ""
	if list, err := releaseSums(rel); err == nil {
		want := list[name]
		if want != "" && want != got {
			os.Remove(imgStage)
			return errors.New("the downloaded image doesn't match the release's SHA256SUMS: try again")
		}
		if want == got {
			sums = "github"
		}
	}
	info, err := parseIUSFile(imgStage)
	if err != nil {
		os.Remove(imgStage)
		return err
	}
	updMu.Lock()
	imgStagedInfo, imgStagedVer, imgStagedSums, imgStagedWarn = info, imgVersionOf(name), sums, releaseWarningFor(rel.Body, firmwareVersion())
	if sums != "" {
		imgSumsFrom, imgSumsTag = "github", rel.Tag
	}
	updMu.Unlock()
	return nil
}

// releaseSums: the release's SHA256SUMS asset, as file name → lower-case SHA-256 (the "<hash>  <name>"
// lines sha256sum writes; a "*" before the name, binary mode, is ignored).
func releaseSums(rel *ghRelease) (map[string]string, error) {
	_, surl, _ := rel.asset(func(n string) bool { return n == "SHA256SUMS" })
	if surl == "" {
		return nil, fmt.Errorf("release %s has no SHA256SUMS file", rel.Tag)
	}
	sp := imgStage + ".sums"
	defer os.Remove(sp)
	if _, err := download(surl, sp, 1<<20); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(sp)
	if err != nil {
		return nil, err
	}
	m := map[string]string{}
	for _, l := range strings.Split(string(b), "\n") {
		if f := strings.Fields(l); len(f) == 2 && len(f[0]) == 64 {
			m[strings.TrimPrefix(f[1], "*")] = strings.ToLower(f[0])
		}
	}
	return m, nil
}

// imgLookupOnline: is an uploaded image the one its release published? The version its name carries picks the
// release (tag v<version>, else <version>) of the update source; that release's SHA256SUMS line for the
// firmware image is compared with sha, the way a download from GitHub is checked.
// res: "online" (matches), "nomatch" (the release lists a different file), "" (couldn't tell: why says so, and
// the owner can paste the release's SHA256SUMS instead).
func imgLookupOnline(ver, sha string) (res, tag, why string) {
	if ver == "" {
		return "", "", "the file's name doesn't say which version it is"
	}
	repo := updateRepo()
	tags := []string{"v" + strings.TrimPrefix(ver, "v"), strings.TrimPrefix(ver, "v")}
	var rel *ghRelease
	for _, t := range tags {
		r, err := releaseByTag(repo, t, imgLookupWait)
		if err == nil {
			rel = r
			break
		}
		if !errors.Is(err, errNoSuchRelease) {
			return "", "", "the miner couldn't reach GitHub (" + err.Error() + ")"
		}
	}
	if rel == nil {
		return "", "", fmt.Sprintf("there is no release v%s on %s", strings.TrimPrefix(ver, "v"), repo)
	}
	list, err := releaseSums(rel)
	if err != nil {
		if strings.Contains(err.Error(), "no SHA256SUMS") {
			return "", rel.Tag, err.Error()
		}
		return "", rel.Tag, "the miner couldn't download the release's SHA256SUMS (" + err.Error() + ")"
	}
	want := list[brand+"-firmware-v"+strings.TrimPrefix(ver, "v")+".ius"]
	if want == "" { // named differently: the release's one firmware image
		for n, h := range list {
			if iusAssetRe().MatchString(n) {
				if want != "" {
					return "", rel.Tag, "release " + rel.Tag + " lists more than one firmware image"
				}
				want = h
			}
		}
	}
	if want == "" {
		return "", rel.Tag, "release " + rel.Tag + "'s SHA256SUMS has no line for the firmware image"
	}
	if want == strings.ToLower(sha) {
		return "online", rel.Tag, ""
	}
	return "nomatch", rel.Tag, ""
}

type imgIn struct {
	Check, Fetch, Install, Discard, Rollback, Verify, Ack bool
	Sums                                                  *string
	Password                                              string // Install: the panel password again (access.go)
}

// handleImageAction: the update actions on the SC Box II (feature on). Repository and preset actions stay
// in handleUpdate.
func handleImageAction(w http.ResponseWriter, r *http.Request, in imgIn) {
	fail := func(e string) { writeJSON(w, map[string]any{"ok": false, "err": e}) }
	switch {
	case in.Rollback:
		fail("there is no roll back on this model: the previous system image isn't kept (the flash has room for one). An SD card with a known-good image always restores the miner.")
		return
	case in.Sums != nil:
		updMu.Lock()
		if imgStagedInfo == nil {
			updMu.Unlock()
			fail("no image is waiting: check a file first")
			return
		}
		res, n := sumsMatch(*in.Sums, imgStagedInfo.SHA256)
		if n == 0 {
			updMu.Unlock()
			fail("no SHA-256 checksum found in that text: paste the line for " + brand + "-firmware-v….ius from SHA256SUMS")
			return
		}
		if imgSumsFrom != "online" && imgSumsFrom != "github" { // the release's own answer stands
			imgStagedSums, imgSumsFrom = res, "pasted"
		}
		updMu.Unlock()
	case in.Check:
		rel, err := latestRelease(updateRepo())
		if err != nil {
			fail(err.Error())
			return
		}
		cur := firmwareVersion()
		name, _, size := rel.asset(iusAssetRe().MatchString)
		body := rel.Body
		if len(body) > 3000 {
			body = body[:3000] + "…"
		}
		v := imgView()
		v["latest"] = map[string]any{"tag": rel.Tag, "name": rel.Name, "url": rel.URL, "published": rel.Published, "notes": body,
			"warning": releaseWarningFor(rel.Body, cur), "asset": name, "size": size, "newer": cur == "" || versionLess(cur, rel.Tag)}
		writeJSON(w, v)
		return
	case in.Fetch:
		if !updSetBusy("downloading") {
			fail("an update is already in progress")
			return
		}
		updMu.Lock()
		imgClearStaged()
		updMu.Unlock()
		err := imgFetchLatest()
		updSetBusy("")
		if err != nil {
			fail(err.Error())
			return
		}
	case in.Discard:
		updMu.Lock()
		imgClearStaged()
		updMu.Unlock()
		os.Remove(imgStage)
	case in.Verify, in.Install:
		updMu.Lock()
		info, sums, ver, from := imgStagedInfo, imgStagedSums, imgStagedVer, imgSumsFrom
		updMu.Unlock()
		if info == nil {
			fail("no image is waiting: choose a file or check for updates first")
			return
		}
		if !updSetBusy(map[bool]string{true: "installing", false: "verifying"}[in.Install]) {
			fail("an update is already in progress")
			return
		}
		// an uploaded file: look its release up online (as a download is checked), unless that already answered
		if in.Verify && !imgSumsOK(sums) && from != "online" && from != "github" {
			res, tag, why := imgLookupOnline(ver, info.SHA256)
			updMu.Lock()
			if imgStagedInfo == info {
				imgSumsTag, imgLookupWhy = tag, why
				if res != "" {
					imgStagedSums, imgSumsFrom, imgLookup = res, "online", "done"
				} else {
					imgLookup = "unavailable"
				}
				sums = imgStagedSums
			}
			updMu.Unlock()
		}
		// the file again: it sits in RAM, but it is what will be written
		again, err := parseIUSFile(imgStage)
		if err == nil && again.SHA256 != info.SHA256 {
			err = errors.New("the waiting image changed: check it again")
		}
		if err != nil {
			updSetBusy("")
			updMu.Lock()
			imgClearStaged()
			updMu.Unlock()
			os.Remove(imgStage)
			fail(err.Error())
			return
		}
		checks, ok, same := imgPreflight(info, false)
		sumsOK := imgSumsOK(sums)
		checks = append(checks, imgCheck{OK: sumsOK, Msg: map[bool]string{true: "SHA-256 matches the release's SHA256SUMS",
			false: "SHA-256 not yet matched against the release's SHA256SUMS (paste it below): required before installing"}[sumsOK]})
		updMu.Lock()
		if imgStagedInfo == info {
			imgLastCheck, imgStagedSame, imgStagedOK = checks, same, map[bool]string{true: "ok", false: "fail"}[ok]
		}
		updMu.Unlock()
		if in.Verify {
			updSetBusy("")
			v := imgView()
			v["verified"] = ok && sumsOK
			writeJSON(w, v)
			return
		}
		refuse := func(e string) { updSetBusy(""); fail(e) }
		switch {
		case !ok:
			for _, c := range checks {
				if !c.OK {
					refuse("not installing: " + c.Msg)
					return
				}
			}
			refuse("not installing: a check failed")
			return
		case !sumsOK:
			refuse("not installing: match the image against the release's SHA256SUMS first (download from GitHub, or paste it)")
			return
		case !in.Ack:
			refuse("not installing: confirm that an SD card with a known-good image is ready and the power is reliable")
			return
		case getCharge() == "tuner":
			refuse("the tuner is running: stop it first")
			return
		case sctRunning():
			refuse("the strong-chip test is running (Controls → Chip trims): stop it first")
			return
		case ceilSnapshot().Running:
			refuse("the clock-ceiling probe is running: stop it first")
			return
		}
		// writing a system image is the owner's decision: the panel password again, whatever else was shown or
		// pasted (a matching checksum or a signed-in browser alone never installs). Checked as a sign-in is (M1),
		// last, so a refusal for another reason costs no password try.
		if secHardened {
			if err := checkPanelPassword(r, in.Password, "install firmware"); err != nil {
				refuse(err.Error())
				return
			}
		}
		rb, _ := json.Marshal(imgRequest{SHA256: info.SHA256, Version: ver, From: firmwareVersion()})
		if err := os.WriteFile(imgReqPath, rb, 0o600); err != nil {
			refuse(err.Error())
			return
		}
		imgMarkClean() // the hashrate and events kept in RAM go to the config partition now
		if err := imgHandoff(imgStage); err != nil {
			os.Remove(imgReqPath)
			refuse(err.Error())
			return
		}
		label := map[bool]string{true: "v" + strings.TrimPrefix(ver, "v"), false: "the new image"}[ver != ""]
		activity.emit("⬆ writing firmware image " + label + " to the system partition: the miner reboots when it's done")
		recordMark("RB", "firmware image "+label)
		markReboot(4*time.Minute, imgHardReboot, "Writing firmware "+label+": the miner reboots when it's done (keep the power on).")
		writeJSON(w, map[string]any{"ok": true, "note": "writing " + label + ": the miner reboots when it's done (a few minutes). Keep the power on."})
		return
	}
	writeJSON(w, imgView())
}

// updViewAny: the update state in this model's shape.
func updViewAny() map[string]any {
	if updIsImage() && !featOff(model.Feat.FirmwareUpdate) {
		return imgView()
	}
	return updView()
}
