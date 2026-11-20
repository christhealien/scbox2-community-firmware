package main

// The FAT32 checker and repairer. Read everything it needs first (the reserved sectors, every FAT copy, and every
// directory reachable from the root), decide, and only then write. Nothing it doesn't understand is changed.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
)

const (
	entMask  = 0x0FFFFFFF // a FAT32 entry is 28 bits; the top 4 are reserved and kept as they are
	entBad   = 0x0FFFFFF7 // a cluster marked bad
	entEOCLo = 0x0FFFFFF8 // end of chain (0x0FFFFFF8..0x0FFFFFFF)

	stateByte  = 65   // BS_Reserved1 on FAT32: Linux keeps its "dirty" flag (bit 0) here
	stateDirty = 0x01 // FAT_STATE_DIRTY

	fsiLeadSig   = 0x41615252
	fsiStructSig = 0x61417272
	fsiTrailSig  = 0xAA550000

	maxDepth   = 64      // directories deeper than this: not a layout this repair takes on
	maxEntries = 1 << 20 // directory entries in all (the config partition holds a few hundred)
	writeGap   = 8       // unchanged sectors between two changed ones that are rewritten as one write
)

// device is the volume: a block device on the miner, a file in the tests.
type device interface {
	ReadAt(p []byte, off int64) (int, error)
	WriteAt(p []byte, off int64) (int, error)
	Sync() error
}

// errUnsupported: not a FAT32 volume this program understands. Nothing is changed.
type errUnsupported struct{ why string }

func (e errUnsupported) Error() string { return "not a FAT32 volume this repair understands: " + e.why }

func unsupported(f string, a ...any) error { return errUnsupported{fmt.Sprintf(f, a...)} }

type volume struct {
	dev     device
	bps     int   // bytes per sector
	spc     int   // sectors per cluster
	rsv     int   // reserved sectors (boot sector, FSInfo, backup boot sector, ...)
	nfats   int   // FAT copies
	fatSz   int   // sectors per FAT copy
	totSec  int64 // sectors in the volume
	dataSec int64 // first data sector
	root    uint32
	fsInfo  int    // FSInfo sector, -1 = none
	bkBoot  int    // backup boot sector, -1 = none
	nclus   uint32 // data clusters
	maxClus uint32 // the highest valid cluster number (nclus+1)

	meta []byte     // sectors [0, rsv+nfats*fatSz): the reserved area and every FAT copy, as read
	fats [][]uint32 // per copy: entries 0..maxClus, as read
}

func (v *volume) clusterBytes() int { return v.bps * v.spc }
func (v *volume) fatOff(copy int) int {
	return (v.rsv + copy*v.fatSz) * v.bps
}

func readFull(d device, p []byte, off int64) error {
	n, err := d.ReadAt(p, off)
	if n == len(p) {
		return nil
	}
	if err == nil {
		err = errors.New("short read")
	}
	return fmt.Errorf("read %d bytes at %d: %w", len(p), off, err)
}

func pow2(n int) bool { return n > 0 && n&(n-1) == 0 }

// openVolume reads and checks the boot sector, then reads the reserved area and the FATs. size is the device's
// size in bytes. An errUnsupported means the volume is left alone.
func openVolume(d device, size int64) (*volume, error) {
	b := make([]byte, 512)
	if err := readFull(d, b, 0); err != nil {
		return nil, err
	}
	le16 := func(o int) int { return int(binary.LittleEndian.Uint16(b[o:])) }
	le32 := func(o int) uint32 { return binary.LittleEndian.Uint32(b[o:]) }
	if b[0] != 0xEB && b[0] != 0xE9 {
		return nil, unsupported("no boot jump instruction (byte 0 is %#02x)", b[0])
	}
	if b[510] != 0x55 || b[511] != 0xAA {
		return nil, unsupported("no boot sector signature")
	}
	v := &volume{dev: d, bps: le16(11), spc: int(b[13]), rsv: le16(14), nfats: int(b[16])}
	switch {
	case !pow2(v.bps) || v.bps < 512 || v.bps > 4096:
		return nil, unsupported("bytes per sector %d", v.bps)
	case !pow2(v.spc) || v.spc > 128 || v.bps*v.spc > 65536:
		return nil, unsupported("sectors per cluster %d", v.spc)
	case v.rsv < 2:
		return nil, unsupported("%d reserved sectors", v.rsv)
	case v.nfats < 1 || v.nfats > 2:
		return nil, unsupported("%d FAT copies", v.nfats)
	case le16(17) != 0 || le16(22) != 0:
		return nil, unsupported("a FAT12/16 layout (root entries %d, 16-bit FAT size %d)", le16(17), le16(22))
	case b[21] < 0xF0 && b[21] != 0:
		return nil, unsupported("media byte %#02x", b[21])
	}
	v.fatSz = int(le32(36))
	if ext := le16(40); ext&0x80 != 0 {
		return nil, unsupported("FAT mirroring is switched off (extended flags %#04x)", ext)
	}
	if ver := le16(42); ver != 0 {
		return nil, unsupported("FAT32 version %#04x", ver)
	}
	v.totSec = int64(le16(19))
	if v.totSec == 0 {
		v.totSec = int64(le32(32))
	}
	v.root = le32(44)
	v.fsInfo, v.bkBoot = le16(48), le16(50)
	if v.fsInfo < 1 || v.fsInfo >= v.rsv {
		v.fsInfo = -1 // (0 or 0xFFFF: no FSInfo sector)
	}
	if v.bkBoot < 1 || v.bkBoot >= v.rsv || v.bkBoot == v.fsInfo {
		v.bkBoot = -1
	}
	if v.fatSz <= 0 || v.fatSz > 1<<20 {
		return nil, unsupported("FAT size %d sectors", v.fatSz)
	}
	v.dataSec = int64(v.rsv) + int64(v.nfats)*int64(v.fatSz)
	if v.totSec <= v.dataSec {
		return nil, unsupported("%d sectors in all, data would start at %d", v.totSec, v.dataSec)
	}
	if size > 0 && v.totSec*int64(v.bps) > size {
		return nil, unsupported("the volume says %d bytes, the device has %d", v.totSec*int64(v.bps), size)
	}
	n := (v.totSec - v.dataSec) / int64(v.spc)
	if n < 1 || n > entMask-16 {
		return nil, unsupported("%d data clusters", n)
	}
	v.nclus = uint32(n)
	v.maxClus = v.nclus + 1
	if int64(v.maxClus+1)*4 > int64(v.fatSz)*int64(v.bps) {
		return nil, unsupported("a FAT of %d sectors can't map %d clusters", v.fatSz, v.nclus)
	}
	if v.root < 2 || v.root > v.maxClus {
		return nil, unsupported("root directory cluster %d", v.root)
	}
	if v.bps > 512 { // the full first sector (the BPB said how long it is)
		b = make([]byte, v.bps)
		if err := readFull(d, b, 0); err != nil {
			return nil, err
		}
	}
	v.meta = make([]byte, int(v.dataSec)*v.bps)
	if err := readFull(d, v.meta, 0); err != nil {
		return nil, err
	}
	v.fats = make([][]uint32, v.nfats)
	for k := range v.fats {
		raw := v.meta[v.fatOff(k):]
		e := make([]uint32, v.maxClus+1)
		for i := range e {
			e[i] = binary.LittleEndian.Uint32(raw[i*4:])
		}
		v.fats[k] = e
	}
	return v, nil
}

func (v *volume) sector(s int) []byte { return v.meta[s*v.bps : (s+1)*v.bps] }

// fsInfoBad: why the FSInfo sector isn't valid ("" when it is, or when the volume has none).
func (v *volume) fsInfoBad() string {
	if v.fsInfo < 0 {
		return ""
	}
	s := v.sector(v.fsInfo)
	for _, c := range []struct {
		off  int
		want uint32
	}{{0, fsiLeadSig}, {484, fsiStructSig}, {508, fsiTrailSig}} {
		if got := binary.LittleEndian.Uint32(s[c.off:]); got != c.want {
			return fmt.Sprintf("FSInfo signature at offset %d is %#08x, not %#08x", c.off, got, c.want)
		}
	}
	return ""
}

func (v *volume) dirty() bool { return v.meta[stateByte]&stateDirty != 0 }

// report is what the check found.
type report struct {
	dirty      bool
	fsInfoBad  string
	problems   []string // damage this program won't repair: if any, nothing is changed
	lost       []uint32 // allocated clusters no directory entry reaches (to be freed)
	lostChains int
	files      int
	dirs       int
	used       uint32 // clusters in use by files and directories
}

type walker struct {
	v     *volume
	r     *report
	owner []int32  // per cluster: the chain that uses it (0 = none)
	names []string // chain id -> path
	nent  int
}

func (w *walker) problem(f string, a ...any) {
	if len(w.r.problems) < 20 {
		w.r.problems = append(w.r.problems, fmt.Sprintf(f, a...))
	}
}

// follow walks one chain from first, claiming each cluster for id. false = the chain is damaged (said why).
func (w *walker) follow(first uint32, id int32, path string) ([]uint32, bool) {
	v := w.v
	var list []uint32
	c := first
	for {
		if c < 2 || c > v.maxClus {
			if len(list) == 0 {
				w.problem("%s: its first cluster (%d) is outside the volume's clusters 2-%d", path, c, v.maxClus)
			} else {
				w.problem("%s: its cluster chain points from cluster %d to %#x, outside the volume's clusters 2-%d", path, list[len(list)-1], c, v.maxClus)
			}
			return nil, false
		}
		if o := w.owner[c]; o != 0 {
			if o == id {
				w.problem("%s: its cluster chain loops back to cluster %d", path, c)
			} else {
				w.problem("%s and %s share cluster %d (cross-linked)", w.names[o], path, c)
			}
			return nil, false
		}
		w.owner[c] = id
		list = append(list, c)
		e := v.fats[0][c] & entMask
		for k := 1; k < v.nfats; k++ {
			if v.fats[k][c]&entMask != e {
				w.problem("%s: the FAT copies disagree at cluster %d (%#x / %#x)", path, c, e, v.fats[k][c]&entMask)
				return nil, false
			}
		}
		switch {
		case e >= entEOCLo:
			return list, true
		case e == entBad:
			w.problem("%s: its cluster chain runs into a cluster marked bad (after cluster %d)", path, c)
			return nil, false
		case e == 0:
			w.problem("%s: its cluster chain runs into a free cluster (cluster %d is followed by nothing)", path, c)
			return nil, false
		}
		c = e // (1, or past the last cluster: said at the top of the loop)
	}
}

func shortName(e []byte) string {
	n := e[0:8]
	x := e[8:11]
	b := []byte{}
	for i, c := range n {
		if i == 0 && c == 0x05 {
			c = 0xE5
		}
		b = append(b, c)
	}
	s := string(trimSpace(b))
	if ext := trimSpace(x); len(ext) > 0 {
		s += "." + string(ext)
	}
	return printable(s)
}

func trimSpace(b []byte) []byte {
	for len(b) > 0 && b[len(b)-1] == ' ' {
		b = b[:len(b)-1]
	}
	return b
}

func printable(s string) string {
	out := []byte{}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c > 0x7E {
			out = append(out, fmt.Sprintf("\\x%02x", c)...)
		} else {
			out = append(out, c)
		}
	}
	return string(out)
}

var (
	dotName    = []byte(".          ")
	dotDotName = []byte("..         ")
)

func equalBytes(a, b []byte) bool { return string(a) == string(b) }

type dirJob struct {
	first uint32
	path  string
	depth int
	id    int32 // already claimed by its parent's walk (0 for the root)
}

// check reads every directory from the root, follows every chain, and finds what is lost.
func (v *volume) check() (*report, error) {
	r := &report{dirty: v.dirty(), fsInfoBad: v.fsInfoBad()}
	w := &walker{v: v, r: r, owner: make([]int32, v.maxClus+1), names: []string{""}}
	newID := func(path string) int32 {
		w.names = append(w.names, path)
		return int32(len(w.names) - 1)
	}
	cb := v.clusterBytes()
	buf := make([]byte, cb)
	stack := []dirJob{{first: v.root, path: "/", depth: 0}}
	for len(stack) > 0 && len(r.problems) == 0 {
		j := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		id := j.id
		if id == 0 {
			id = newID(j.path)
		}
		chain, ok := w.follow(j.first, id, "directory "+j.path)
		if !ok {
			break
		}
		r.dirs++
		r.used += uint32(len(chain))
		idx := 0
	clusters:
		for _, c := range chain {
			off := (v.dataSec + int64(c-2)*int64(v.spc)) * int64(v.bps)
			if err := readFull(v.dev, buf, off); err != nil {
				return nil, err
			}
			for o := 0; o < cb; o, idx = o+32, idx+1 {
				e := buf[o : o+32]
				if w.nent++; w.nent > maxEntries {
					w.problem("more than %d directory entries", maxEntries)
					break clusters
				}
				if j.depth > 0 && idx < 2 { // a subdirectory starts with "." and ".."
					want := dotName
					if idx == 1 {
						want = dotDotName
					}
					if !equalBytes(e[0:11], want) || e[11]&0x10 == 0 {
						w.problem("directory %s: entry %d should be %q", j.path, idx, string(trimSpace(want)))
						break clusters
					}
					continue
				}
				if e[0] == 0x00 {
					break clusters // the end of the directory
				}
				if e[0] == 0xE5 {
					continue // deleted
				}
				attr := e[11]
				if attr&0x3F == 0x0F {
					continue // a long-name part
				}
				name := shortName(e)
				path := j.path + name
				if equalBytes(e[0:11], dotName) || equalBytes(e[0:11], dotDotName) {
					w.problem("directory %s: a %q entry where there should be none", j.path, name)
					break clusters
				}
				if attr&0x08 != 0 && attr&0x10 == 0 {
					continue // the volume label (left as it is)
				}
				if bad := badEntry(e); bad != "" {
					w.problem("%s: %s", path, bad)
					break clusters
				}
				first := uint32(binary.LittleEndian.Uint16(e[20:]))<<16 | uint32(binary.LittleEndian.Uint16(e[26:]))
				size := binary.LittleEndian.Uint32(e[28:])
				if attr&0x10 != 0 {
					switch {
					case first == 0:
						w.problem("directory %s: no first cluster", path)
					case size != 0:
						w.problem("directory %s: a size (%d) where a directory has none", path, size)
					case j.depth+1 > maxDepth:
						w.problem("directory %s: more than %d levels deep", path, maxDepth)
					}
					if len(r.problems) > 0 {
						break clusters
					}
					stack = append(stack, dirJob{first: first, path: path + "/", depth: j.depth + 1, id: 0})
					continue
				}
				r.files++
				if first == 0 {
					if size != 0 {
						w.problem("%s: %d bytes long but no cluster", path, size)
						break clusters
					}
					continue
				}
				if size == 0 {
					w.problem("%s: has clusters (from %d) but a size of 0", path, first)
					break clusters
				}
				fc, ok := w.follow(first, newID(path), path)
				if !ok {
					break clusters
				}
				r.used += uint32(len(fc))
				need := (int64(size) + int64(cb) - 1) / int64(cb)
				if int64(len(fc)) != need {
					w.problem("%s: %d bytes need %d clusters, its chain has %d", path, size, need, len(fc))
					break clusters
				}
			}
		}
	}
	if len(r.problems) > 0 {
		return r, nil
	}
	// lost: in use in a FAT copy, reached by no directory entry
	lost := map[uint32]bool{}
	for c := uint32(2); c <= v.maxClus; c++ {
		if w.owner[c] != 0 {
			continue
		}
		bad, inUse := 0, false
		for k := 0; k < v.nfats; k++ {
			e := v.fats[k][c] & entMask
			if e == entBad {
				bad++
			}
			inUse = inUse || e != 0
		}
		if bad == v.nfats {
			continue // marked bad in every copy: stays so
		}
		if bad > 0 {
			w.problem("cluster %d is marked bad in one FAT copy only", c)
			continue
		}
		if inUse {
			lost[c] = true
			r.lost = append(r.lost, c)
		}
	}
	// a lost cluster that leads into a file's chain means a file's start may be wrong: not freed blindly
	pointed := map[uint32]bool{}
	for _, c := range r.lost {
		for k := 0; k < v.nfats; k++ {
			e := v.fats[k][c] & entMask
			if e >= 2 && e <= v.maxClus {
				if o := w.owner[e]; o != 0 {
					w.problem("an unreachable cluster (%d) leads into %s at cluster %d", c, w.names[o], e)
					break
				}
				if lost[e] {
					pointed[e] = true
				}
			}
		}
	}
	for _, c := range r.lost {
		if !pointed[c] {
			r.lostChains++
		}
	}
	if r.lostChains == 0 && len(r.lost) > 0 {
		r.lostChains = 1 // only loops among the lost clusters: still something to free
	}
	sort.Slice(r.lost, func(i, j int) bool { return r.lost[i] < r.lost[j] })
	return r, nil
}

// badEntry: why a short directory entry is one this repair won't go past ("" = fine). Long names, the label and
// the dot entries are handled before this.
func badEntry(e []byte) string {
	if e[11]&0xC0 != 0 {
		return fmt.Sprintf("its entry has reserved attribute bits set (%#02x)", e[11])
	}
	if e[0] == ' ' {
		return "its entry's name starts with a space"
	}
	for i := 0; i < 11; i++ {
		c := e[i]
		if c < 0x20 && !(i == 0 && c == 0x05) {
			return fmt.Sprintf("its entry's name has a control character (%#02x)", c)
		}
	}
	return ""
}

// plan is what a repair writes: the new contents of the reserved area and the FATs, in the order of writing.
type plan struct {
	newMeta  []byte
	freed    int
	free     uint32
	nextFree uint32
	backup   string // what was done (or not) about the backup boot sector
}

// makePlan builds the repaired reserved area and FATs: the lost clusters freed in every copy, the FSInfo sector
// made valid with the true free count, the dirty flag cleared, the backup boot sector kept equal to the boot sector.
func (v *volume) makePlan(r *report) *plan {
	p := &plan{newMeta: append([]byte(nil), v.meta...)}
	for _, c := range r.lost {
		for k := 0; k < v.nfats; k++ {
			o := v.fatOff(k) + int(c)*4
			old := binary.LittleEndian.Uint32(p.newMeta[o:])
			binary.LittleEndian.PutUint32(p.newMeta[o:], old&^entMask) // free (the reserved top bits stay)
		}
		p.freed++
	}
	p.nextFree = 0xFFFFFFFF
	for c := uint32(2); c <= v.maxClus; c++ {
		if binary.LittleEndian.Uint32(p.newMeta[v.fatOff(0)+int(c)*4:])&entMask == 0 {
			p.free++
			if p.nextFree == 0xFFFFFFFF {
				p.nextFree = c
			}
		}
	}
	if v.fsInfo >= 0 {
		s := p.newMeta[v.fsInfo*v.bps : (v.fsInfo+1)*v.bps]
		if r.fsInfoBad != "" { // a fresh one: all the reserved bytes zero
			for i := range s {
				s[i] = 0
			}
		}
		binary.LittleEndian.PutUint32(s[0:], fsiLeadSig)
		binary.LittleEndian.PutUint32(s[484:], fsiStructSig)
		binary.LittleEndian.PutUint32(s[488:], p.free)
		binary.LittleEndian.PutUint32(s[492:], p.nextFree)
		binary.LittleEndian.PutUint32(s[508:], fsiTrailSig)
	}
	p.newMeta[stateByte] &^= stateDirty
	if v.bkBoot >= 0 {
		boot := p.newMeta[0:v.bps]
		bk := p.newMeta[v.bkBoot*v.bps : (v.bkBoot+1)*v.bps]
		var diff []int
		for i := range boot {
			if boot[i] != bk[i] {
				diff = append(diff, i)
			}
		}
		switch {
		case len(diff) == 0:
			p.backup = "the backup boot sector matches"
		case len(diff) == 1 && diff[0] == stateByte:
			bk[stateByte] = boot[stateByte]
			p.backup = "the backup boot sector's state byte set to match"
		default:
			p.backup = fmt.Sprintf("the backup boot sector differs in %d bytes (first at offset %d): left as it is", len(diff), diff[0])
		}
	}
	return p
}

type wrun struct {
	sec, n int // first sector, sectors
}

// writeRuns: the changed sectors of newMeta against meta, in this order: the FATs, the FSInfo sector, the backup
// boot sector, the boot sector (the dirty flag is cleared last: a power cut before it leaves the volume dirty, and
// the next start repairs again). Changed sectors close together are written as one run.
func (v *volume) writeRuns(newMeta []byte) []wrun {
	changed := func(s int) bool {
		a, b := v.meta[s*v.bps:(s+1)*v.bps], newMeta[s*v.bps:(s+1)*v.bps]
		return string(a) != string(b)
	}
	var phases [][]int
	var fat []int
	for s := v.rsv; s < int(v.dataSec); s++ {
		if changed(s) {
			fat = append(fat, s)
		}
	}
	phases = append(phases, fat)
	for _, s := range []int{v.fsInfo, v.bkBoot, 0} {
		if s >= 0 && changed(s) {
			phases = append(phases, []int{s})
		}
	}
	var runs []wrun
	for _, ph := range phases {
		for i, s := range ph {
			if i > 0 {
				last := &runs[len(runs)-1]
				if s-(last.sec+last.n) <= writeGap {
					last.n = s - last.sec + 1
					continue
				}
			}
			runs = append(runs, wrun{s, 1})
		}
	}
	return runs
}

// apply writes the plan and reads it back.
func (v *volume) apply(p *plan, flush func()) (int, error) {
	runs := v.writeRuns(p.newMeta)
	for _, r := range runs {
		b := p.newMeta[r.sec*v.bps : (r.sec+r.n)*v.bps]
		if n, err := v.dev.WriteAt(b, int64(r.sec)*int64(v.bps)); err != nil || n != len(b) {
			if err == nil {
				err = errors.New("short write")
			}
			return 0, fmt.Errorf("writing %d sector(s) at sector %d: %w", r.n, r.sec, err)
		}
	}
	if err := v.dev.Sync(); err != nil {
		return 0, fmt.Errorf("flushing the writes: %w", err)
	}
	if flush != nil {
		flush() // drop the cached copy, so the read-back below comes from the device
	}
	got := make([]byte, len(p.newMeta))
	if err := readFull(v.dev, got, 0); err != nil {
		return 0, fmt.Errorf("reading the writes back: %w", err)
	}
	if string(got) != string(p.newMeta) {
		for i := range got {
			if got[i] != p.newMeta[i] {
				return 0, fmt.Errorf("the writes didn't read back (first difference at byte %d)", i)
			}
		}
	}
	return len(runs), nil
}
