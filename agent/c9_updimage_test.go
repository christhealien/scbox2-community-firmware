package main

// C9: the SC Box II system-image update (updimage.go). Fake containers and a file-backed fake system disk:
// nothing here touches a real device.

import (
	"bytes"
	"crypto/md5"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash/crc32"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeSquashfs: a superblock of the stock recipe (gzip, 128 KiB blocks, no xattrs) + filler, n bytes.
func fakeSquashfs(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := 96; i < n; i++ {
		b[i] = byte(i*7) ^ seed
	}
	le := binary.LittleEndian
	used := uint64(n - 1000)
	le.PutUint32(b[0:], sqMagic)
	le.PutUint32(b[4:], 600)                     // inodes
	le.PutUint32(b[8:], 1761955200+uint32(seed)) // mkfs time
	le.PutUint32(b[12:], sqStockBlock)
	le.PutUint32(b[16:], 18)
	le.PutUint16(b[20:], sqGzip)
	le.PutUint16(b[22:], 17)
	le.PutUint16(b[24:], 0x2c0)
	le.PutUint16(b[26:], 3)
	le.PutUint16(b[28:], 4)
	le.PutUint16(b[30:], 0)
	le.PutUint64(b[40:], used)
	le.PutUint64(b[48:], used-8)
	le.PutUint64(b[56:], ^uint64(0))
	le.PutUint64(b[64:], used-20000)
	le.PutUint64(b[72:], used-10000)
	le.PutUint64(b[80:], used-3000)
	le.PutUint64(b[88:], used-30)
	return b
}

type fakeItem struct {
	flags uint32
	typ   uint32
	data  []byte
}

// fakeIUS lays items out like the stock container (data, 64-byte md5 tag, sector padding) and pins the
// non-system ones as the "stock" boot items for the test.
func fakeIUS(t *testing.T, items []fakeItem, pin bool) []byte {
	t.Helper()
	hdr := make([]byte, iusSector)
	copy(hdr, "+ius")
	binary.LittleEndian.PutUint32(hdr[4:], 2)
	copy(hdr[0x10:], "2.2.1")
	binary.LittleEndian.PutUint32(hdr[0x1c:], uint32(len(items)))
	body := []byte{}
	pins := map[string]string{}
	for i, it := range items {
		sec := uint32((iusSector + len(body)) / iusSector)
		d := hdr[0x20+16*i:]
		binary.LittleEndian.PutUint32(d, it.flags<<24|sec)
		binary.LittleEndian.PutUint32(d[4:], it.typ)
		binary.LittleEndian.PutUint32(d[8:], 0x40000)
		binary.LittleEndian.PutUint32(d[12:], uint32(len(it.data)))
		m := md5.Sum(it.data)
		tag := make([]byte, 64)
		copy(tag, m[:])
		body = append(body, it.data...)
		body = append(body, tag...)
		body = append(body, make([]byte, (iusSector-len(body)%iusSector)%iusSector+iusSector)...)
		key := "boot"
		if it.flags == 0x82 {
			key = "type" + string(rune('0'+it.typ))
		}
		if key != "type5" {
			pins[key] = hex.EncodeToString(m[:])
		}
	}
	binary.LittleEndian.PutUint32(hdr[0x18:], iusHeaderCRC(hdr))
	if pin {
		old := iusPinnedMD5
		iusPinnedMD5 = pins
		t.Cleanup(func() { iusPinnedMD5 = old })
	}
	return append(hdr, body...)
}

func stdItems(sys []byte) []fakeItem {
	ub := bytes.Repeat([]byte("uboot0"), 100)
	return []fakeItem{{0x01, 0x3c000200, ub}, {0x82, 0, ub}, {0x82, 1, []byte("items.start\npart5 system.18432.fs\nitems.end\n")},
		{0x82, 2, bytes.Repeat([]byte("rd"), 3000)}, {0x82, 3, bytes.Repeat([]byte("kernel"), 2000)}, {0x82, 5, sys}}
}

func writeTemp(t *testing.T, b []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "f.ius")
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestC9_ParseGood(t *testing.T) {
	sys := fakeSquashfs(2<<20, 1)
	info, err := parseIUSFile(writeTemp(t, fakeIUS(t, stdItems(sys), true)))
	if err != nil {
		t.Fatal(err)
	}
	s := sha256.Sum256(sys)
	if info.SysLen != int64(len(sys)) || info.SysSHA256 != hex.EncodeToString(s[:]) || info.SWVersion != "2.2.1" {
		t.Fatalf("bad info %+v", info)
	}
}

// The real stock pins: the stock container's boot items (when the stock file is here) pass, a fake doesn't.
func TestC9_StockPinsRefuseForeignBootItems(t *testing.T) {
	b := fakeIUS(t, stdItems(fakeSquashfs(2<<20, 1)), false)
	if _, err := parseIUSFile(writeTemp(t, b)); err == nil || !strings.Contains(err.Error(), "2.2.1") {
		t.Fatalf("foreign boot items must be refused: %v", err)
	}
	for _, p := range []string{"../../base/box2/fw/firmware.ius", "../../../../base/box2/fw/firmware.ius"} {
		if _, err := os.Stat(p); err == nil {
			info, err := parseIUSFile(p)
			if err != nil {
				t.Fatalf("the stock container must pass: %v", err)
			}
			if info.SysMD5 != "063494ba70fe9e923b96ea107070324f" || info.SysLen != 13553664 {
				t.Fatalf("stock system item: %+v", info)
			}
		}
	}
}

func TestC9_ParseRefusals(t *testing.T) {
	sys := fakeSquashfs(2<<20, 1)
	good := fakeIUS(t, stdItems(sys), true)
	mut := func(f func(b []byte) []byte) []byte { return f(append([]byte(nil), good...)) }
	fixCRC := func(b []byte) []byte { binary.LittleEndian.PutUint32(b[0x18:], iusHeaderCRC(b)); return b }
	sysOff := int64(binary.LittleEndian.Uint32(good[0x20+16*5:])&0xffffff) * iusSector
	cases := map[string][]byte{
		"magic":     mut(func(b []byte) []byte { b[0] = 'x'; return b }),
		"crc":       mut(func(b []byte) []byte { b[0x10] = '9'; return b }),
		"truncated": good[:len(good)-iusSector*3],
		"tag":       mut(func(b []byte) []byte { b[sysOff+5000] ^= 1; return b }),
		"count":     mut(func(b []byte) []byte { binary.LittleEndian.PutUint32(b[0x1c:], 40); return fixCRC(b) }),
		"kind":      mut(func(b []byte) []byte { b[0x20+16*2+3] = 0x83; return fixCRC(b) }),
		"tiny":      good[:1024],
	}
	for name, b := range cases {
		if _, err := parseIUSFile(writeTemp(t, b)); err == nil {
			t.Errorf("%s: must be refused", name)
		}
	}
	// another kernel (boot item differs from the pinned one)
	it := stdItems(sys)
	fakeIUS(t, it, true) // pins these
	it[4].data = bytes.Repeat([]byte("kernel2"), 2000)
	if _, err := parseIUSFile(writeTemp(t, fakeIUS(t, it, false))); err == nil || !strings.Contains(err.Error(), "type3") {
		t.Errorf("a different kernel must be refused: %v", err)
	}
	// an extra item type / a missing item
	it = append(stdItems(sys), fakeItem{0x82, 6, []byte("ramdisk1")})
	fakeIUS(t, stdItems(sys), true)
	if _, err := parseIUSFile(writeTemp(t, fakeIUS(t, it, false))); err == nil {
		t.Error("an extra item must be refused")
	}
	it = stdItems(sys)
	fakeIUS(t, it, true)
	if _, err := parseIUSFile(writeTemp(t, fakeIUS(t, append(it[:3:3], it[4:]...), false))); err == nil {
		t.Error("a missing ramdisk item must be refused")
	}
	if _, err := parseIUSFile(writeTemp(t, fakeIUS(t, it[:5], false))); err == nil {
		t.Error("no system item must be refused")
	}
}

func TestC9_SquashfsChecks(t *testing.T) {
	for name, f := range map[string]func(b []byte){
		"xz": func(b []byte) { binary.LittleEndian.PutUint16(b[20:], 4) },
		"block": func(b []byte) {
			binary.LittleEndian.PutUint32(b[12:], 65536)
			binary.LittleEndian.PutUint16(b[22:], 16)
		},
		"v3":       func(b []byte) { binary.LittleEndian.PutUint16(b[28:], 3) },
		"used":     func(b []byte) { binary.LittleEndian.PutUint64(b[40:], uint64(len(b)+1)) },
		"xattr":    func(b []byte) { binary.LittleEndian.PutUint64(b[56:], 4096) },
		"magic":    func(b []byte) { b[0] = 0 },
		"tables":   func(b []byte) { binary.LittleEndian.PutUint64(b[64:], 1<<40) },
		"inodes":   func(b []byte) { binary.LittleEndian.PutUint32(b[4:], 0) },
		"tinyused": func(b []byte) { binary.LittleEndian.PutUint64(b[40:], 4096) },
	} {
		sys := fakeSquashfs(2<<20, 2)
		f(sys)
		if _, err := parseIUSFile(writeTemp(t, fakeIUS(t, stdItems(sys), true))); err == nil {
			t.Errorf("%s: must be refused", name)
		}
	}
	// not whole sectors; larger than the partition
	sys := fakeSquashfs(2<<20+100, 2)
	if _, err := parseIUSFile(writeTemp(t, fakeIUS(t, stdItems(sys), true))); err == nil {
		t.Error("a system item of partial sectors must be refused")
	}
	sys = fakeSquashfs(int(box2SysBytes)+iusSector, 2)
	if _, err := parseIUSFile(writeTemp(t, fakeIUS(t, stdItems(sys), true))); err == nil || !strings.Contains(err.Error(), "larger") {
		t.Errorf("a system item over 18,874,368 bytes must be refused: %v", err)
	}
}

// fakeDisk: a file standing in for /dev/spiblock1 (old squashfs + a tail), wired into the device seams.
func fakeDisk(t *testing.T, part int64, old []byte) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "spiblock1")
	b := bytes.Repeat([]byte{0xa1}, int(part))
	copy(b, old)
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	oDev, oPart, oNeed, oW, oR, oFlush := imgDevPath, imgPartBytes, imgNeedBlockDev, imgWriteHook, imgReadHook, imgFlushDev
	imgDevPath, imgPartBytes, imgNeedBlockDev, imgFlushDev = p, part, false, func(f *os.File) { f.Sync() }
	oDrop := imgDropCaches
	imgDropCaches = func() {}
	t.Cleanup(func() { imgDropCaches = oDrop })
	t.Cleanup(func() {
		imgDevPath, imgPartBytes, imgNeedBlockDev, imgWriteHook, imgReadHook, imgFlushDev = oDev, oPart, oNeed, oW, oR, oFlush
	})
	return p
}

func TestC9_WriteAndReadBack(t *testing.T) {
	part := int64(6 << 20)
	old, img := fakeSquashfs(3<<20, 1), fakeSquashfs(2<<20, 9)
	p := fakeDisk(t, part, old)
	before, _ := os.ReadFile(p)
	res, err := writeSystemImage(p, img, part)
	if err != nil || !res.ReadBack || res.Written != len(img) {
		t.Fatalf("write: %+v %v", res, err)
	}
	after, _ := os.ReadFile(p)
	if !bytes.Equal(after[:len(img)], img) || !bytes.Equal(after[len(img):], before[len(img):]) || int64(len(after)) != part {
		t.Fatal("the disk must hold the new image, then the untouched old bytes")
	}
}

// An image whose length isn't a multiple of the 64 KiB chunk (as every real one): the chunk across its end
// must be compared (and, when it reads back wrong, rewritten) as image + old tail. Found on the loop device.
func TestC9_WriteUnalignedLength(t *testing.T) {
	part := int64(6 << 20)
	old := fakeSquashfs(3<<20, 1)
	img := fakeSquashfs(2<<20+32<<10, 9)[: 2<<20+32<<10 : 2<<20+32<<10] // exact capacity: slicing past it panics
	p := fakeDisk(t, part, old)
	before, _ := os.ReadFile(p)
	n := 0
	imgReadHook = func(off int64, b []byte) {
		if off == 2<<20 && n == 0 { // the straddling chunk reads back wrong once
			b[40<<10] ^= 1
			n++
		}
	}
	res, err := writeSystemImage(p, img, part)
	if err != nil || res.Rewritten != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	after, _ := os.ReadFile(p)
	if !bytes.Equal(after[:len(img)], img) || !bytes.Equal(after[len(img):], before[len(img):]) {
		t.Fatal("the tail after an unaligned image must stay exactly as it was")
	}
}

func TestC9_WriteSameBytesIsNoOp(t *testing.T) {
	part := int64(4 << 20)
	img := fakeSquashfs(2<<20, 3)
	p := fakeDisk(t, part, img)
	before, _ := os.ReadFile(p)
	if _, err := writeSystemImage(p, img, part); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(p)
	if !bytes.Equal(before, after) {
		t.Fatal("rewriting the installed image must leave the disk byte-identical")
	}
}

func TestC9_WriteFaultRestoresOld(t *testing.T) {
	part := int64(6 << 20)
	old, img := fakeSquashfs(3<<20, 1), fakeSquashfs(2<<20, 9)
	p := fakeDisk(t, part, old)
	before, _ := os.ReadFile(p)
	imgWriteHook = func(off int64, b []byte) error {
		if off >= 1<<20 {
			return errors.New("EIO (test)")
		}
		return nil
	}
	res, err := writeSystemImage(p, img, part)
	if err == nil || !res.Restored || !strings.Contains(err.Error(), "written back") {
		t.Fatalf("a write fault must restore the old image: %+v %v", res, err)
	}
	after, _ := os.ReadFile(p)
	if !bytes.Equal(before, after) {
		t.Fatal("after a restore the disk must be exactly as before")
	}
}

func TestC9_ReadBackMismatch(t *testing.T) {
	part := int64(6 << 20)
	old, img := fakeSquashfs(3<<20, 1), fakeSquashfs(2<<20, 9)
	// once: the block is written again and the write succeeds
	p := fakeDisk(t, part, old)
	n := 0
	imgReadHook = func(off int64, b []byte) {
		if off == 1<<20 && n == 0 {
			b[10] ^= 0xff
			n++
		}
	}
	res, err := writeSystemImage(p, img, part)
	if err != nil || res.Rewritten != 1 {
		t.Fatalf("one bad block must be rewritten once: %+v %v", res, err)
	}
	// always (also in the tail, which is never written): the old image goes back, then the error says SD card
	p = fakeDisk(t, part, old)
	imgReadHook = func(off int64, b []byte) {
		if off == 5<<20 {
			b[0] ^= 1
		}
	}
	res, err = writeSystemImage(p, img, part)
	if err == nil || res.Restored || !strings.Contains(err.Error(), "SD card") {
		t.Fatalf("a partition that never reads back right must end in the SD-card message: %+v %v", res, err)
	}
	// only the new image's region: restored
	p = fakeDisk(t, part, old)
	imgReadHook = func(off int64, b []byte) {
		if off == 1<<20 && !bytes.Equal(b[:16], old[1<<20:1<<20+16]) {
			b[0] ^= 1 // the new bytes there always read back wrong; the old ones fine
		}
	}
	res, err = writeSystemImage(p, img, part)
	if err == nil || !res.Restored {
		t.Fatalf("a new image that never reads back right must be undone: %+v %v", res, err)
	}
	if after, _ := os.ReadFile(p); !bytes.Equal(after[:len(old)], old) {
		t.Fatal("the old image must be back")
	}
}

func TestC9_WriteRefusals(t *testing.T) {
	part := int64(4 << 20)
	img := fakeSquashfs(2<<20, 3)
	p := fakeDisk(t, part, fakeSquashfs(2<<20, 1))
	if _, err := writeSystemImage(filepath.Join(filepath.Dir(p), "spiblock0"), img, part); err == nil {
		t.Error("any disk but the system disk must be refused")
	}
	if _, err := writeSystemImage(p, img, part+iusSector); err == nil {
		t.Error("a disk of another size must be refused")
	}
	if _, err := writeSystemImage(p, img[:len(img)-100], part); err == nil {
		t.Error("an image of partial sectors must be refused")
	}
	if _, err := writeSystemImage(p, append(img, make([]byte, part)...), part); err == nil {
		t.Error("an image larger than the partition must be refused")
	}
	os.WriteFile(p, make([]byte, part), 0o600) // no squashfs on it: not the system disk
	if _, err := writeSystemImage(p, img, part); err == nil || !strings.Contains(err.Error(), "nothing written") {
		t.Errorf("a disk without a squashfs must be refused: %v", err)
	}
	imgNeedBlockDev = true
	fakeSquashfsOn := fakeSquashfs(2<<20, 1)
	os.WriteFile(p, append(fakeSquashfsOn, make([]byte, int(part)-len(fakeSquashfsOn))...), 0o600)
	if _, err := writeSystemImage(p, img, part); err == nil {
		t.Error("a plain file must be refused when a block device is required")
	}
}

// fakeSystem: a booted system whose root is the fake disk's squashfs, with memory to spare.
func fakeSystem(t *testing.T, part int64, cur []byte, memKB int) {
	t.Helper()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "cmdline"), []byte("console=ttyAMA3,115200 mem=256M rootfstype=squashfs root=/dev/spiblock1 rw\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "meminfo"), []byte("MemTotal: 255000 kB\nMemFree: "+strconv.Itoa(memKB)+" kB\nBuffers: 0 kB\nCached: 0 kB\nShmem: 0 kB\n"), 0o644)
	sb, _ := parseSqSuper(cur)
	oc, om, os_, osb, opd := imgCmdlinePath, imgMeminfoPath, imgRootStatfs, imgSelfBytes, imgProcDir
	imgCmdlinePath, imgMeminfoPath, imgProcDir = filepath.Join(dir, "cmdline"), filepath.Join(dir, "meminfo"), dir
	imgSelfBytes = func() int64 { return 11 << 20 }
	imgRootStatfs = func() (syscall.Statfs_t, error) {
		var s syscall.Statfs_t
		s.Type = squashfsMagic
		setNum(&s.Bsize, sb.BlockSize)
		s.Blocks = ((sb.BytesUsed - 1) >> sb.BlockLog) + 1
		s.Files = uint64(sb.Inodes)
		return s, nil
	}
	t.Cleanup(func() {
		imgCmdlinePath, imgMeminfoPath, imgRootStatfs, imgSelfBytes, imgProcDir = oc, om, os_, osb, opd
	})
	fakeDisk(t, part, cur)
}

func setNum[T int32 | int64](p *T, v uint32) { *p = T(v) } // Statfs_t.Bsize: int64 on amd64, int32 on arm

func TestC9_Preflight(t *testing.T) {
	part := int64(6 << 20)
	cur := fakeSquashfs(3<<20, 1)
	newSys := fakeSquashfs(2<<20, 9)
	info, err := parseIUSFile(writeTemp(t, fakeIUS(t, stdItems(newSys), true)))
	if err != nil {
		t.Fatal(err)
	}
	fakeSystem(t, part, cur, 200000)
	checks, ok, same := imgPreflight(info, false)
	if !ok || same != "different" {
		t.Fatalf("a good system must pass: %v %+v", same, checks)
	}
	// the same image as installed
	curInfo, _ := parseIUSFile(writeTemp(t, fakeIUS(t, stdItems(cur), true)))
	if _, ok, same := imgPreflight(curInfo, false); !ok || same != "same" {
		t.Fatalf("the installed image must be recognised: %v %v", ok, same)
	}
	// low memory
	fakeSystem(t, part, cur, 40000)
	if c, ok, _ := imgPreflight(info, false); ok || !strings.Contains(c[len(c)-1].Msg, "memory") {
		t.Fatalf("low memory must refuse: %+v", c)
	}
	// booted from somewhere else
	fakeSystem(t, part, cur, 200000)
	os.WriteFile(imgCmdlinePath, []byte("root=/dev/mmcblk0p2 rootfstype=ext4"), 0o644)
	if _, ok, _ := imgPreflight(info, false); ok {
		t.Fatal("a system not started from the system disk must refuse")
	}
	// a disk that isn't the running root (another squashfs on it)
	fakeSystem(t, part, cur, 200000)
	other := fakeSquashfs(3<<20, 1)
	binary.LittleEndian.PutUint32(other[4:], 601)
	b, _ := os.ReadFile(imgDevPath)
	copy(b, other)
	os.WriteFile(imgDevPath, b, 0o600)
	if _, ok, _ := imgPreflight(info, false); ok {
		t.Fatal("a disk that doesn't hold the running system must refuse")
	}
	// wrong partition size
	fakeSystem(t, part, cur, 200000)
	imgPartBytes = part + 4096
	if _, ok, _ := imgPreflight(info, false); ok {
		t.Fatal("a disk of another size must refuse")
	}
}

func TestC9_MemAvailFallback(t *testing.T) {
	dir := t.TempDir()
	om := imgMeminfoPath
	t.Cleanup(func() { imgMeminfoPath = om })
	imgMeminfoPath = filepath.Join(dir, "m")
	os.WriteFile(imgMeminfoPath, []byte("MemTotal: 255000 kB\nMemFree: 100000 kB\nBuffers: 1000 kB\nCached: 50000 kB\nShmem: 20000 kB\n"), 0o644)
	if got := memAvailBytes(); got != (100000+1000+50000-20000)<<10 {
		t.Fatalf("3.10 fallback: %d", got)
	}
	os.WriteFile(imgMeminfoPath, []byte("MemFree: 1 kB\nMemAvailable: 7 kB\n"), 0o644)
	if got := memAvailBytes(); got != 7<<10 {
		t.Fatalf("MemAvailable: %d", got)
	}
}

// box2On: the SC Box II with FirmwareUpdate commissioned, a temp config dir, no real reboot/exec.
func box2On(t *testing.T) {
	t.Helper()
	p := profSCBox2
	p.Feat.FirmwareUpdate = true
	withModel(t, p)
	oa, oauth := *authFile, auth
	*authFile = filepath.Join(t.TempDir(), "auth.json")
	var a authStore // a known panel password: an install asks for it again on the SC Box II (access.go)
	a.Username, a.Secret = "admin", "c2VjcmV0c2VjcmV0c2VjcmV0c2VjcmV0c2VjcmV0MTI="
	setPassword(&a, c9TestPw)
	authMu.Lock()
	auth = a
	authMu.Unlock()
	dir := t.TempDir()
	os_, or, oh, ore, oex, omc, oapi := imgStage, imgReqPath, imgHandoff, imgReboot, imgExit, imgMarkClean, updateAPI
	imgStage, imgReqPath = filepath.Join(dir, "update.ius"), filepath.Join(dir, "req.json")
	updateAPI = "http://127.0.0.1:1" // no GitHub: an uploaded file's online look-up can't run (the owner pastes)
	imgMarkClean = func() {}
	imgHandoff = func(string) error { return errors.New("handoff (test)") }
	imgReboot = func() { t.Error("reboot called in a test") }
	imgExit = func(int) {}
	updMu.Lock()
	imgClearStaged()
	updBusy = ""
	updMu.Unlock()
	t.Cleanup(func() {
		authUpgradeWG.Wait()
		authMu.Lock()
		auth = oauth
		authMu.Unlock()
		*authFile = oa
		imgStage, imgReqPath, imgHandoff, imgReboot, imgExit, imgMarkClean, updateAPI = os_, or, oh, ore, oex, omc, oapi
		updMu.Lock()
		imgClearStaged()
		updMu.Unlock()
	})
}

const c9TestPw = "c9-test-password"

func post(t *testing.T, path, body string) map[string]any {
	t.Helper()
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	if strings.HasPrefix(path, "/api/update/upload") {
		handleUpdateUpload(rec, req)
	} else {
		handleUpdate(rec, req)
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("%s: bad JSON %q", path, rec.Body.String())
	}
	return m
}

func TestC9_HTTPFlow(t *testing.T) {
	box2On(t)
	part := int64(6 << 20)
	cur := fakeSquashfs(3<<20, 1)
	fakeSystem(t, part, cur, 200000)
	ius := fakeIUS(t, stdItems(fakeSquashfs(2<<20, 9)), true)
	if m := post(t, "/api/update/upload?name=x.tgz", "junk"); m["ok"] != false {
		t.Fatalf("junk must be refused: %v", m)
	}
	if _, err := os.Stat(imgStage); err == nil {
		t.Fatal("a refused upload must not stay in RAM")
	}
	m := post(t, "/api/update/upload?name="+brand+"-firmware-v0.4.0.ius", string(ius))
	st, _ := m["staged"].(map[string]any)
	if m["image"] != true || st == nil || st["version"] != "0.4.0" {
		t.Fatalf("a good image must be staged with its version: %v", m)
	}
	if m := post(t, "/api/update", `{"rollback":true}`); m["ok"] != false || !strings.Contains(m["err"].(string), "SD card") {
		t.Fatalf("no roll back on this model: %v", m)
	}
	// dry run: every check, nothing written; not "verified" until the sums match
	before, _ := os.ReadFile(imgDevPath)
	m = post(t, "/api/update", `{"verify":true}`)
	if m["verified"] != false || m["staged"].(map[string]any)["same"] != "different" {
		t.Fatalf("verify without sums: %v", m)
	}
	if m := post(t, "/api/update", `{"install":true,"ack":true}`); m["ok"] != false || !strings.Contains(m["err"].(string), "SHA256SUMS") {
		t.Fatalf("install without sums must be refused: %v", m)
	}
	if m := post(t, "/api/update", `{"sums":"0000000000000000000000000000000000000000000000000000000000000000  x.ius"}`); m["staged"].(map[string]any)["sums"] != "nomatch" {
		t.Fatalf("wrong sums: %v", m)
	}
	s := sha256.Sum256(ius)
	post(t, "/api/update", `{"sums":"`+hex.EncodeToString(s[:])+`  `+brand+`-firmware-v0.4.0.ius"}`)
	if m := post(t, "/api/update", `{"verify":true}`); m["verified"] != true {
		t.Fatalf("all checks must pass now: %v", m)
	}
	if m := post(t, "/api/update", `{"install":true}`); m["ok"] != false || !strings.Contains(m["err"].(string), "SD card") {
		t.Fatalf("install without the SD-card confirmation must be refused: %v", m)
	}
	if m := post(t, "/api/update", `{"install":true,"ack":true,"password":"`+c9TestPw+`"}`); m["ok"] != false || !strings.Contains(m["err"].(string), "handoff (test)") {
		t.Fatalf("a good install must reach the hand-off: %v", m)
	}
	if b, err := os.ReadFile(imgReqPath); err == nil {
		t.Fatalf("a failed hand-off must not leave the request: %s", b)
	}
	if after, _ := os.ReadFile(imgDevPath); !bytes.Equal(before, after) {
		t.Fatal("the panel itself never writes the disk")
	}
	// low memory at install time
	os.WriteFile(imgMeminfoPath, []byte("MemFree: 30000 kB\n"), 0o644)
	if m := post(t, "/api/update", `{"install":true,"ack":true}`); m["ok"] != false || !strings.Contains(m["err"].(string), "memory") {
		t.Fatalf("low memory must refuse the install: %v", m)
	}
	// a staged image changed in RAM
	os.WriteFile(imgMeminfoPath, []byte("MemFree: 200000 kB\n"), 0o644)
	b := append([]byte(nil), ius...)
	b[len(b)-1] ^= 1
	os.WriteFile(imgStage, b, 0o644)
	if m := post(t, "/api/update", `{"verify":true}`); m["ok"] != false {
		t.Fatalf("a changed staged file must be refused: %v", m)
	}
}

func TestC9_OffAndSCLiteUnchanged(t *testing.T) {
	withModel(t, profSCBox2) // feature off: the B7 refusal, nothing image-shaped
	if m := post(t, "/api/update", `{"verify":true}`); m["disabled"] != true {
		t.Fatalf("feature off: refused: %v", m)
	}
	withModel(t, profSCLite)
	if updIsImage() {
		t.Fatal("the SC Lite updates by package")
	}
	req := httptest.NewRequest("GET", "/api/update", nil)
	rec := httptest.NewRecorder()
	handleUpdate(rec, req)
	if strings.Contains(rec.Body.String(), `"image"`) {
		t.Fatalf("SC Lite view must not change: %s", rec.Body.String())
	}
}

// The installer (the panel's copy in RAM), end to end against the fake disk, then the check after the reboot.
func TestC9_InstallerEndToEnd(t *testing.T) {
	box2On(t)
	part := int64(6 << 20)
	cur, newSys := fakeSquashfs(3<<20, 1), fakeSquashfs(2<<20, 9)
	fakeSystem(t, part, cur, 200000)
	ius := fakeIUS(t, stdItems(newSys), true)
	os.WriteFile(imgStage, ius, 0o644)
	s := sha256.Sum256(ius)
	// a different file than the one checked: nothing written
	if _, err := installImage(imgStage, imgRequest{SHA256: strings.Repeat("0", 64), Version: "0.4.0"}); !errors.Is(err, errImgNothingWritten) {
		t.Fatalf("a changed image must write nothing: %v", err)
	}
	m, err := installImage(imgStage, imgRequest{SHA256: hex.EncodeToString(s[:]), Version: "0.4.0"})
	if err != nil || m.To != "0.4.0" {
		t.Fatalf("install: %v", err)
	}
	disk, _ := os.ReadFile(imgDevPath)
	if !bytes.Equal(disk[:len(newSys)], newSys) || !bytes.Equal(disk[len(newSys):len(cur)], cur[len(newSys):]) {
		t.Fatal("the disk must hold the new system, the rest untouched")
	}
	if _, err := os.Stat(imgStage); err == nil {
		t.Fatal("the image is removed from RAM before the write")
	}
	last, _ := os.ReadFile(filepath.Join(updDir(), "last.json"))
	if !strings.Contains(string(last), `"written"`) {
		t.Fatalf("last.json: %s", last)
	}
	imgConfirmAfterBoot()
	for i := 0; i < 50; i++ {
		if _, err := os.Stat(imgPendingPath()); err != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	last, _ = os.ReadFile(filepath.Join(updDir(), "last.json"))
	if !strings.Contains(string(last), `"installed"`) {
		t.Fatalf("after the reboot the written image must be confirmed: %s", last)
	}
	// feature switched off in the meantime: the installer writes nothing
	p := profSCBox2
	withModel(t, p)
	os.WriteFile(imgStage, ius, 0o644)
	before, _ := os.ReadFile(imgDevPath)
	if _, err := installImage(imgStage, imgRequest{SHA256: hex.EncodeToString(s[:])}); !errors.Is(err, errImgNothingWritten) {
		t.Fatalf("feature off: %v", err)
	}
	if after, _ := os.ReadFile(imgDevPath); !bytes.Equal(before, after) {
		t.Fatal("feature off: the disk must not change")
	}
}

func TestC9_HeaderCRCMatchesZlib(t *testing.T) {
	h := make([]byte, iusSector)
	copy(h, "+ius")
	h[0x18] = 0xff
	c := append([]byte(nil), h...)
	copy(c[0x18:0x1c], []byte{0, 0, 0, 0})
	if iusHeaderCRC(h) != crc32.ChecksumIEEE(c) {
		t.Fatal("CRC must ignore its own field (zlib.crc32 of the header with it zeroed)")
	}
}

func TestC9_AssetName(t *testing.T) {
	if v := imgVersionOf(brand + "-firmware-v0.4.1.ius"); v != "0.4.1" {
		t.Fatalf("version: %q", v)
	}
	for _, n := range []string{brand + "-firmware-v0.4.1.img", "x" + brand + "-firmware-v1.ius", brand + "-update-v1.tar.gz"} {
		if iusAssetRe().MatchString(n) {
			t.Fatalf("%s must not match", n)
		}
	}
}
