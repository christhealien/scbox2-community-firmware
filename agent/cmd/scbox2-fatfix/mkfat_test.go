package main

// A small FAT32 image builder for the tests (this host has no mkfs.fat), laid out as the miner's factory-formatted
// config partition (mkdosfs: 512-byte sectors and clusters, 6 reserved sectors, FSInfo at 1, the backup boot sector
// at 3, two FATs, a lowercase "config" label), plus a reader of its own (independent of the code under test) that
// lists the files a directory walk reaches and the clusters they use.

import (
	"encoding/binary"
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

type fimg struct {
	b                           []byte
	bps, spc, rsv, nfats, fatSz int
	fsi, bk                     int
	total                       int
	dataSec                     int
	maxClus                     uint32
	root                        uint32
	stride                      uint32 // > 1: files are spread out (fragmented)
	next                        uint32
	files                       map[string][]byte // ground truth: path -> content
	ents                        map[string]int    // path -> offset of its short entry
	chains                      map[string][]uint32
	dirs                        map[string]uint32 // path ("/a/") -> first cluster
}

type fopts struct {
	total, spc, rsv, fsi, bk int
	both16                   bool // also set the 16-bit total (the factory image does)
}

func defaultOpts() fopts { return fopts{total: 6144, spc: 1, rsv: 6, fsi: 1, bk: 3, both16: true} }

func mkfat(t testing.TB, o fopts) *fimg {
	bps := 512
	f := &fimg{bps: bps, spc: o.spc, rsv: o.rsv, nfats: 2, fsi: o.fsi, bk: o.bk, total: o.total, root: 2,
		stride: 1, next: 3, files: map[string][]byte{}, ents: map[string]int{}, chains: map[string][]uint32{},
		dirs: map[string]uint32{"/": 2}}
	// FAT size: enough entries for every data cluster (as mkdosfs works it out)
	f.fatSz = 1
	for {
		data := o.total - o.rsv - 2*f.fatSz
		n := data / o.spc
		if (n+2)*4 <= f.fatSz*bps {
			f.maxClus = uint32(n + 1)
			break
		}
		f.fatSz++
	}
	f.dataSec = o.rsv + 2*f.fatSz
	f.b = make([]byte, o.total*bps)
	b := f.b
	copy(b[0:], []byte{0xEB, 0x58, 0x90})
	copy(b[3:], "mkdosfs ")
	binary.LittleEndian.PutUint16(b[11:], uint16(bps))
	b[13] = byte(o.spc)
	binary.LittleEndian.PutUint16(b[14:], uint16(o.rsv))
	b[16] = 2
	if o.both16 && o.total < 65536 {
		binary.LittleEndian.PutUint16(b[19:], uint16(o.total))
	}
	b[21] = 0xF8
	binary.LittleEndian.PutUint16(b[24:], 63)
	binary.LittleEndian.PutUint16(b[26:], 255)
	binary.LittleEndian.PutUint32(b[32:], uint32(o.total))
	binary.LittleEndian.PutUint32(b[36:], uint32(f.fatSz))
	binary.LittleEndian.PutUint32(b[44:], 2)
	binary.LittleEndian.PutUint16(b[48:], uint16(o.fsi))
	binary.LittleEndian.PutUint16(b[50:], uint16(o.bk))
	b[64], b[66] = 0x80, 0x29
	binary.LittleEndian.PutUint32(b[67:], 0x5eed1234)
	copy(b[71:], "config\x00\x00\x00\x00\x00") // the factory label: lowercase, NUL-padded
	copy(b[82:], "FAT32   ")
	b[510], b[511] = 0x55, 0xAA
	f.setFAT(0, 0x0FFFFFF8)
	f.setFAT(1, 0x0FFFFFFF)
	f.setFAT(2, 0x0FFFFFFF)
	// the label in the root directory too
	var lab [32]byte
	copy(lab[:], "config     ")
	lab[11] = 0x08
	copy(f.b[f.clusOff(2):], lab[:])
	f.syncMeta()
	return f
}

// syncMeta: the FSInfo sector (true free count) and the backup boot + FSInfo sectors, as a clean unmount leaves them.
func (f *fimg) syncMeta() {
	s := f.b[f.fsi*f.bps : (f.fsi+1)*f.bps]
	for i := range s {
		s[i] = 0
	}
	binary.LittleEndian.PutUint32(s[0:], fsiLeadSig)
	binary.LittleEndian.PutUint32(s[484:], fsiStructSig)
	binary.LittleEndian.PutUint32(s[488:], f.freeCount())
	binary.LittleEndian.PutUint32(s[492:], 2)
	binary.LittleEndian.PutUint32(s[508:], fsiTrailSig)
	copy(f.b[f.bk*f.bps:], f.b[0:f.bps])
	copy(f.b[(f.bk+1)*f.bps:], s)
}

func (f *fimg) fatOff(k int, c uint32) int { return (f.rsv+k*f.fatSz)*f.bps + int(c)*4 }
func (f *fimg) getFAT(k int, c uint32) uint32 {
	return binary.LittleEndian.Uint32(f.b[f.fatOff(k, c):])
}
func (f *fimg) setFATk(k int, c, v uint32) { binary.LittleEndian.PutUint32(f.b[f.fatOff(k, c):], v) }
func (f *fimg) setFAT(c, v uint32) {
	for k := 0; k < f.nfats; k++ {
		f.setFATk(k, c, v)
	}
}
func (f *fimg) clusOff(c uint32) int { return (f.dataSec + int(c-2)*f.spc) * f.bps }
func (f *fimg) cb() int              { return f.bps * f.spc }

func (f *fimg) freeCount() uint32 {
	n := uint32(0)
	for c := uint32(2); c <= f.maxClus; c++ {
		if f.getFAT(0, c)&entMask == 0 {
			n++
		}
	}
	return n
}

// alloc: n free clusters, linked, ending in EOC (spread out by stride).
func (f *fimg) alloc(t testing.TB, n int) []uint32 {
	var out []uint32
	tries := 0
	for len(out) < n {
		if tries++; tries > 4*int(f.maxClus) {
			t.Fatalf("image full")
		}
		c := f.next
		f.next += f.stride
		if f.next > f.maxClus {
			f.next = 2 + (f.next % 7)
		}
		if c < 2 || c > f.maxClus || f.getFAT(0, c)&entMask != 0 {
			continue
		}
		f.setFAT(c, 0x0FFFFFFF) // claimed now, linked below
		out = append(out, c)
	}
	for i := 0; i+1 < len(out); i++ {
		f.setFAT(out[i], out[i+1])
	}
	return out
}

func (f *fimg) chainOf(first uint32) []uint32 {
	var out []uint32
	for c := first; c >= 2 && c <= f.maxClus && len(out) <= int(f.maxClus); {
		out = append(out, c)
		e := f.getFAT(0, c) & entMask
		if e >= entEOCLo {
			break
		}
		c = e
	}
	return out
}

// slot: the offset of n free consecutive entries at the end of the directory (grown by a cluster when full).
func (f *fimg) slot(t testing.TB, dir uint32, n int) int {
	for {
		ch := f.chainOf(dir)
		var offs []int
		for _, c := range ch {
			for o := 0; o < f.cb(); o += 32 {
				offs = append(offs, f.clusOff(c)+o)
			}
		}
		end := -1
		for i, o := range offs {
			if f.b[o] == 0 {
				end = i
				break
			}
		}
		if end >= 0 && end+n <= len(offs) && contiguous(offs[end:end+n]) {
			return offs[end]
		}
		nc := f.alloc(t, 1)[0]
		for i := 0; i < f.cb(); i++ {
			f.b[f.clusOff(nc)+i] = 0
		}
		f.setFAT(ch[len(ch)-1], nc)
		if end >= 0 && end+n > len(offs) { // the tail of the last cluster stays unused: mark it deleted
			for _, o := range offs[end:] {
				f.b[o] = 0xE5
			}
		}
	}
}

func contiguous(o []int) bool {
	for i := 1; i < len(o); i++ {
		if o[i] != o[i-1]+32 {
			return false
		}
	}
	return true
}

func lfnChecksum(short []byte) byte {
	var s byte
	for _, c := range short[:11] {
		s = (s>>1 | s<<7) + c
	}
	return s
}

var sfnSeq int

// shortFor: an 8.3 name for name (with a ~N tail when the name doesn't fit 8.3 as it is).
func shortFor(name string) ([]byte, bool) {
	up := strings.ToUpper(name)
	base, ext := up, ""
	if i := strings.LastIndex(up, "."); i > 0 {
		base, ext = up[:i], up[i+1:]
	}
	fits := len(base) <= 8 && len(ext) <= 3 && up == name && !strings.ContainsAny(base, ". ") && base != ""
	if !fits {
		sfnSeq++
		clean := strings.Map(func(r rune) rune {
			if r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
				return r
			}
			return -1
		}, base)
		if len(clean) > 4 {
			clean = clean[:4]
		}
		base = fmt.Sprintf("%s~%d", clean, sfnSeq%1000)
		if len(ext) > 3 {
			ext = ext[:3]
		}
	}
	s := []byte(fmt.Sprintf("%-8s%-3s", base, ext))
	return s[:11], !fits
}

// entry adds a directory entry (with long-name entries when the name isn't plain 8.3) and returns its offset.
func (f *fimg) entry(t testing.TB, dir uint32, name string, attr byte, first uint32, size uint32) int {
	short, long := shortFor(name)
	var lfn [][32]byte
	if long {
		u := []uint16{}
		for _, r := range name {
			u = append(u, uint16(r))
		}
		u = append(u, 0)
		for len(u)%13 != 0 {
			u = append(u, 0xFFFF)
		}
		n := len(u) / 13
		sum := lfnChecksum(short)
		for i := n; i >= 1; i-- {
			var e [32]byte
			e[0] = byte(i)
			if i == n {
				e[0] |= 0x40
			}
			part := u[(i-1)*13 : i*13]
			pos := []int{1, 3, 5, 7, 9, 14, 16, 18, 20, 22, 24, 28, 30}
			for j, p := range pos {
				binary.LittleEndian.PutUint16(e[p:], part[j])
			}
			e[11], e[13] = 0x0F, sum
			lfn = append(lfn, e)
		}
	}
	o := f.slot(t, dir, len(lfn)+1)
	for i, e := range lfn {
		copy(f.b[o+32*i:], e[:])
	}
	so := o + 32*len(lfn)
	e := f.b[so : so+32]
	copy(e, short)
	e[11] = attr
	binary.LittleEndian.PutUint16(e[20:], uint16(first>>16))
	binary.LittleEndian.PutUint16(e[26:], uint16(first))
	binary.LittleEndian.PutUint32(e[28:], size)
	return so
}

func (f *fimg) mkdir(t testing.TB, parent string, name string) string {
	pc := f.dirs[parent]
	c := f.alloc(t, 1)[0]
	for i := 0; i < f.cb(); i++ {
		f.b[f.clusOff(c)+i] = 0
	}
	dot := f.b[f.clusOff(c):]
	copy(dot[0:], ".          ")
	dot[11] = 0x10
	binary.LittleEndian.PutUint16(dot[26:], uint16(c))
	binary.LittleEndian.PutUint16(dot[20:], uint16(c>>16))
	copy(dot[32:], "..         ")
	dot[32+11] = 0x10
	if parent != "/" {
		binary.LittleEndian.PutUint16(dot[32+26:], uint16(pc))
		binary.LittleEndian.PutUint16(dot[32+20:], uint16(pc>>16))
	}
	f.entry(t, pc, name, 0x10, c, 0)
	p := parent + name + "/"
	f.dirs[p] = c
	return p
}

func (f *fimg) file(t testing.TB, dir, name string, data []byte) string {
	var first uint32
	var ch []uint32
	if len(data) > 0 {
		ch = f.alloc(t, (len(data)+f.cb()-1)/f.cb())
		first = ch[0]
		for i, c := range ch {
			end := min((i+1)*f.cb(), len(data))
			copy(f.b[f.clusOff(c):], data[i*f.cb():end])
		}
	}
	so := f.entry(t, f.dirs[dir], name, 0x20, first, uint32(len(data)))
	p := dir + name
	f.files[p], f.ents[p], f.chains[p] = data, so, ch
	return p
}

// lostChain: n clusters allocated and linked (in every FAT copy unless only1), with junk in them, and no entry.
func (f *fimg) lostChain(t testing.TB, n int, only1 bool) []uint32 {
	ch := f.alloc(t, n)
	for _, c := range ch {
		copy(f.b[f.clusOff(c):], "lost data from a write the power cut ended")
	}
	if only1 {
		for _, c := range ch {
			f.setFATk(1, c, 0)
		}
	}
	return ch
}

// sample builds the panel's layout: a few small settings files (some with .bak twins), one larger log, a
// subdirectory tree, and an empty file.
func sample(t testing.TB, o fopts, rnd *rand.Rand) *fimg {
	f := mkfat(t, o)
	if rnd != nil && rnd.Intn(2) == 0 {
		f.stride = uint32(2 + rnd.Intn(5))
	}
	content := func(n int) []byte {
		b := make([]byte, n)
		for i := range b {
			b[i] = byte('a' + (i*7+n)%26)
		}
		if rnd != nil {
			rnd.Read(b[:min(n, 16)])
		}
		return b
	}
	d := f.mkdir(t, "/", "scbox2")
	f.file(t, d, "auth.json", content(600))
	f.file(t, d, "auth.json.bak", content(590))
	f.file(t, d, "fan.json", content(1300))
	f.file(t, d, "net.json", content(200))
	f.file(t, d, "EMPTY", nil)
	f.file(t, d, "events.log", content(9000))
	m := f.mkdir(t, "/", "minerd")
	s := f.mkdir(t, m, "settings")
	f.file(t, s, "CPBSET.JSN", content(468))
	f.file(t, s, "pools.json", content(300))
	if rnd != nil {
		for i := 0; i < rnd.Intn(30); i++ {
			dir := []string{d, m, s, "/"}[rnd.Intn(4)]
			f.file(t, dir, fmt.Sprintf("extra%d.dat", i), content(rnd.Intn(5000)))
		}
		if rnd.Intn(3) == 0 {
			x := f.mkdir(t, s, "deeper")
			f.file(t, x, "x.txt", content(rnd.Intn(3000)+1))
		}
	}
	f.syncMeta()
	return f
}

// ---- an independent, lenient reader (as a directory walk would see the files) ----

type seen struct {
	files     map[string][]byte
	reachable map[uint32]bool
}

func readTree(b []byte) seen {
	s := seen{files: map[string][]byte{}, reachable: map[uint32]bool{}}
	bps := int(binary.LittleEndian.Uint16(b[11:]))
	spc := int(b[13])
	rsv := int(binary.LittleEndian.Uint16(b[14:]))
	nf := int(b[16])
	fsz := int(binary.LittleEndian.Uint32(b[36:]))
	if bps < 512 || bps > 4096 || spc == 0 || nf == 0 || fsz == 0 {
		return s
	}
	data := rsv + nf*fsz
	tot := int(binary.LittleEndian.Uint16(b[19:]))
	if tot == 0 {
		tot = int(binary.LittleEndian.Uint32(b[32:]))
	}
	if tot*bps > len(b) || data >= tot {
		return s
	}
	maxc := uint32((tot-data)/spc + 1)
	cb := bps * spc
	fat := func(c uint32) uint32 { return binary.LittleEndian.Uint32(b[rsv*bps+int(c)*4:]) & entMask }
	chain := func(first uint32) []uint32 {
		var out []uint32
		vis := map[uint32]bool{}
		for c := first; c >= 2 && c <= maxc && !vis[c]; {
			vis[c] = true
			out = append(out, c)
			e := fat(c)
			if e >= entBad || e == 0 {
				break
			}
			c = e
		}
		return out
	}
	off := func(c uint32) int { return (data + int(c-2)*spc) * bps }
	type job struct {
		c    uint32
		p    string
		deep int
	}
	root := binary.LittleEndian.Uint32(b[44:])
	stack := []job{{root, "/", 0}}
	dirsSeen := map[uint32]bool{}
	for len(stack) > 0 {
		j := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if dirsSeen[j.c] || j.deep > 70 {
			continue
		}
		dirsSeen[j.c] = true
		ch := chain(j.c)
	out:
		for _, c := range ch {
			s.reachable[c] = true
			for o := 0; o < cb; o += 32 {
				e := b[off(c)+o : off(c)+o+32]
				if e[0] == 0 {
					break out
				}
				if e[0] == 0xE5 || e[11]&0x3F == 0x0F || e[0] == '.' || (e[11]&0x08 != 0 && e[11]&0x10 == 0) {
					continue
				}
				first := uint32(binary.LittleEndian.Uint16(e[20:]))<<16 | uint32(binary.LittleEndian.Uint16(e[26:]))
				name := strings.TrimRight(string(e[0:8]), " ")
				if x := strings.TrimRight(string(e[8:11]), " "); x != "" {
					name += "." + x
				}
				if e[11]&0x10 != 0 {
					stack = append(stack, job{first, j.p + name + "/", j.deep + 1})
					continue
				}
				size := int(binary.LittleEndian.Uint32(e[28:]))
				var content []byte
				for _, fc := range chain(first) {
					s.reachable[fc] = true
					if len(content) < size {
						content = append(content, b[off(fc):off(fc)+cb]...)
					}
				}
				if len(content) > size {
					content = content[:size]
				}
				s.files[j.p+name] = content
			}
		}
	}
	return s
}

// shortPaths: every ground-truth path as the reader names it (8.3 short names).
func (f *fimg) shortPaths() map[string][]byte {
	out := map[string][]byte{}
	for p, data := range f.files {
		so := f.ents[p]
		dir := p[:strings.LastIndex(p, "/")+1]
		e := f.b[so : so+32]
		name := strings.TrimRight(string(e[0:8]), " ")
		if x := strings.TrimRight(string(e[8:11]), " "); x != "" {
			name += "." + x
		}
		// the directory part, in short names too
		sd := "/"
		parts := strings.Split(strings.Trim(dir, "/"), "/")
		cur := "/"
		for _, pp := range parts {
			if pp == "" {
				continue
			}
			cur += pp + "/"
			sd += f.shortDirName(cur) + "/"
		}
		out[sd+name] = data
	}
	return out
}

func (f *fimg) shortDirName(p string) string {
	// find the entry for directory p in its parent
	parent := p[:strings.LastIndex(strings.TrimSuffix(p, "/"), "/")+1]
	pc := f.dirs[parent]
	want := f.dirs[p]
	for _, c := range f.chainOf(pc) {
		for o := 0; o < f.cb(); o += 32 {
			e := f.b[f.clusOff(c)+o : f.clusOff(c)+o+32]
			if e[0] == 0 || e[0] == 0xE5 || e[11]&0x3F == 0x0F || e[11]&0x10 == 0 || e[0] == '.' {
				continue
			}
			first := uint32(binary.LittleEndian.Uint16(e[20:]))<<16 | uint32(binary.LittleEndian.Uint16(e[26:]))
			if first == want {
				return strings.TrimRight(string(e[0:8]), " ") + func() string {
					if x := strings.TrimRight(string(e[8:11]), " "); x != "" {
						return "." + x
					}
					return ""
				}()
			}
		}
	}
	return "?"
}
