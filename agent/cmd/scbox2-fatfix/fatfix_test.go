package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type result struct {
	rc     int
	out    string
	after  []byte
	marker string // "" = none
}

func runFix(t testing.TB, img []byte, extra ...string) result {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "cfg.img")
	if err := os.WriteFile(p, img, 0o644); err != nil {
		t.Fatal(err)
	}
	m := filepath.Join(dir, "marker")
	mounts := filepath.Join(dir, "mounts")
	os.WriteFile(mounts, []byte("proc /proc proc rw 0 0\n"), 0o644)
	old := mountsPath
	mountsPath = mounts
	defer func() { mountsPath = old }()
	var buf bytes.Buffer
	args := append(append([]string{"-marker", m}, extra...), p)
	rc := run(args, &buf)
	after, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	mk, _ := os.ReadFile(m)
	return result{rc, buf.String(), after, string(mk)}
}

// checkRepaired: the image is now as fsck would want it: not dirty, a valid FSInfo with the true free count, no lost
// clusters, the backup boot sector equal to the boot sector, the label untouched, and every file as it was.
func checkRepaired(t testing.TB, f *fimg, after []byte, want map[string][]byte) {
	t.Helper()
	if after[stateByte]&stateDirty != 0 {
		t.Fatalf("still dirty")
	}
	g := &fimg{b: after, bps: f.bps, spc: f.spc, rsv: f.rsv, nfats: f.nfats, fatSz: f.fatSz, maxClus: f.maxClus}
	fs := after[f.fsi*f.bps:]
	if binary.LittleEndian.Uint32(fs[0:]) != fsiLeadSig || binary.LittleEndian.Uint32(fs[484:]) != fsiStructSig ||
		binary.LittleEndian.Uint32(fs[508:]) != fsiTrailSig {
		t.Fatalf("FSInfo signatures not valid")
	}
	if got, w := binary.LittleEndian.Uint32(fs[488:]), g.freeCount(); got != w {
		t.Fatalf("FSInfo free count %d, the FAT has %d free", got, w)
	}
	nf := binary.LittleEndian.Uint32(fs[492:])
	if nf < 2 || nf > f.maxClus || g.getFAT(0, nf)&entMask != 0 {
		t.Fatalf("FSInfo next-free hint %d is not a free cluster", nf)
	}
	if !bytes.Equal(after[0:f.bps], after[f.bk*f.bps:(f.bk+1)*f.bps]) {
		t.Fatalf("the backup boot sector differs from the boot sector")
	}
	if string(after[71:82]) != "config\x00\x00\x00\x00\x00" {
		t.Fatalf("label changed: %q", after[71:82])
	}
	s := readTree(after)
	for c := uint32(2); c <= f.maxClus; c++ {
		for k := 0; k < 2; k++ {
			e := g.getFAT(k, c) & entMask
			if e != 0 && e != entBad && !s.reachable[c] {
				t.Fatalf("cluster %d still allocated (FAT %d: %#x) but not reachable", c, k+1, e)
			}
		}
		if g.getFAT(0, c) != g.getFAT(1, c) {
			t.Fatalf("FAT copies differ at %d", c)
		}
	}
	for p, data := range want {
		got, ok := s.files[p]
		if !ok {
			t.Fatalf("file %s is gone (have %v)", p, keys(s.files))
		}
		if !bytes.Equal(got, data) {
			t.Fatalf("file %s changed", p)
		}
	}
	// and a second run finds nothing to do and writes nothing
	r2 := runFix(t, after)
	if r2.rc != 0 || !bytes.Equal(r2.after, after) || !strings.Contains(r2.out, "nothing to do") {
		t.Fatalf("second run: rc %d, changed %v:\n%s", r2.rc, !bytes.Equal(r2.after, after), r2.out)
	}
}

func keys(m map[string][]byte) []string {
	var k []string
	for x := range m {
		k = append(k, x)
	}
	return k
}

func TestCleanImageUntouched(t *testing.T) {
	for _, o := range []fopts{defaultOpts(), {total: 20000, spc: 4, rsv: 32, fsi: 1, bk: 6}} {
		f := sample(t, o, nil)
		before := append([]byte(nil), f.b...)
		r := runFix(t, f.b)
		if r.rc != 0 || !bytes.Equal(r.after, before) || r.marker != "" {
			t.Fatalf("clean image: rc %d, changed %v, marker %q\n%s", r.rc, !bytes.Equal(r.after, before), r.marker, r.out)
		}
		if !strings.Contains(r.out, "nothing to do") {
			t.Fatalf("%s", r.out)
		}
		// the reader sees every file
		s := readTree(f.b)
		for p, d := range f.shortPaths() {
			if !bytes.Equal(s.files[p], d) {
				t.Fatalf("reader: %s differs", p)
			}
		}
	}
}

// The case seen on the miner after a power cut: dirty, an unwritten FSInfo sector (all 0xFF), two lost chains,
// the backup boot sector differing at offset 65 (only the boot sector carries the dirty flag), lowercase label.
func TestRealPowerCutCase(t *testing.T) {
	f := sample(t, defaultOpts(), nil)
	want := f.shortPaths()
	l1 := f.lostChain(t, 3, false)
	l2 := f.lostChain(t, 1, false)
	fs := f.b[f.fsi*f.bps : (f.fsi+1)*f.bps]
	for i := range fs {
		fs[i] = 0xFF
	}
	f.b[stateByte] |= stateDirty
	r := runFix(t, f.b)
	if r.rc != 0 || r.marker != "" {
		t.Fatalf("rc %d marker %q\n%s", r.rc, r.marker, r.out)
	}
	if !strings.Contains(r.out, "2 lost cluster chain(s), 4 cluster(s)") || !strings.Contains(r.out, "FSInfo sector invalid") {
		t.Fatalf("report:\n%s", r.out)
	}
	for _, c := range append(l1, l2...) {
		if binary.LittleEndian.Uint32(r.after[f.fatOff(0, c):]) != 0 || binary.LittleEndian.Uint32(r.after[f.fatOff(1, c):]) != 0 {
			t.Fatalf("lost cluster %d not freed", c)
		}
		if !bytes.Equal(r.after[f.clusOff(c):f.clusOff(c)+f.cb()], f.b[f.clusOff(c):f.clusOff(c)+f.cb()]) {
			t.Fatalf("the data area was written")
		}
	}
	checkRepaired(t, f, r.after, want)
	// only the FATs, FSInfo and the boot sector changed (the backup already matched once the flag is cleared)
	for i := range r.after {
		if r.after[i] != f.b[i] {
			s := i / f.bps
			if !(s == 0 && i == stateByte) && s != f.fsi && (s < f.rsv || s >= f.dataSec) {
				t.Fatalf("unexpected change at byte %d (sector %d)", i, s)
			}
		}
	}
}

func TestSafeRepairs(t *testing.T) {
	cases := map[string]func(t *testing.T, f *fimg){
		"dirty only":            func(t *testing.T, f *fimg) {},
		"FSInfo wrong count":    func(t *testing.T, f *fimg) { binary.LittleEndian.PutUint32(f.b[f.fsi*f.bps+488:], 3) },
		"FSInfo zeroed":         func(t *testing.T, f *fimg) { clear(f.b[f.fsi*f.bps : (f.fsi+1)*f.bps]) },
		"lost chain long":       func(t *testing.T, f *fimg) { f.lostChain(t, 40, false) },
		"lost in FAT1 only":     func(t *testing.T, f *fimg) { f.lostChain(t, 2, true) },
		"lost loop":             func(t *testing.T, f *fimg) { ch := f.lostChain(t, 3, false); f.setFAT(ch[2], ch[0]) },
		"lost pointing outside": func(t *testing.T, f *fimg) { ch := f.lostChain(t, 2, false); f.setFAT(ch[1], f.maxClus+9) },
		"backup dirty too": func(t *testing.T, f *fimg) {
			f.b[f.bk*f.bps+stateByte] |= stateDirty
		},
		"deleted file left its chain": func(t *testing.T, f *fimg) {
			p := "/scbox2/events.log"
			f.b[f.ents[p]] = 0xE5
			delete(f.files, p)
		},
		"bad cluster kept": func(t *testing.T, f *fimg) {
			c := f.alloc(t, 1)[0]
			f.setFAT(c, entBad)
		},
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			f := sample(t, defaultOpts(), nil)
			mut(t, f)
			want := f.shortPaths()
			f.b[stateByte] |= stateDirty
			r := runFix(t, f.b)
			if r.rc != 0 || r.marker != "" {
				t.Fatalf("rc %d marker %q\n%s", r.rc, r.marker, r.out)
			}
			checkRepaired(t, f, r.after, want)
			if name == "bad cluster kept" {
				n := 0
				for c := uint32(2); c <= f.maxClus; c++ {
					if binary.LittleEndian.Uint32(r.after[f.fatOff(0, c):])&entMask == entBad {
						n++
					}
				}
				if n != 1 {
					t.Fatalf("the bad cluster's mark is gone")
				}
			}
		})
	}
}

// Damage a repair could make worse: nothing is changed, the exit is 1 and the marker says why. Each also with the
// dirty flag set (the case where a repair would otherwise run).
func TestRefusals(t *testing.T) {
	file := func(f *fimg, p string) []uint32 { return f.chains[p] }
	cases := map[string]struct {
		mut  func(t *testing.T, f *fimg)
		want string
	}{
		"cross-linked": {func(t *testing.T, f *fimg) {
			a, b := file(f, "/scbox2/events.log"), file(f, "/scbox2/fan.json")
			f.setFAT(a[len(a)-1], b[1])
		}, "cross-linked"},
		"beyond the cluster count": {func(t *testing.T, f *fimg) {
			a := file(f, "/scbox2/events.log")
			f.setFAT(a[3], f.maxClus+5)
		}, "outside the volume"},
		"first cluster beyond": {func(t *testing.T, f *fimg) {
			binary.LittleEndian.PutUint16(f.b[f.ents["/scbox2/net.json"]+26:], 0xFFF0)
		}, "first cluster"},
		"loop": {func(t *testing.T, f *fimg) {
			a := file(f, "/scbox2/events.log")
			f.setFAT(a[len(a)-1], a[2])
		}, "loops"},
		"truncated (runs into a free cluster)": {func(t *testing.T, f *fimg) {
			a := file(f, "/scbox2/events.log")
			f.setFAT(a[len(a)-1], 0)
		}, "free cluster"},
		"bad cluster in a chain": {func(t *testing.T, f *fimg) {
			a := file(f, "/scbox2/fan.json")
			f.setFAT(a[0], entBad)
		}, "marked bad"},
		"FAT copies disagree": {func(t *testing.T, f *fimg) {
			a := file(f, "/scbox2/events.log")
			f.setFATk(1, a[1], 0)
		}, "disagree"},
		"size longer than the chain": {func(t *testing.T, f *fimg) {
			binary.LittleEndian.PutUint32(f.b[f.ents["/scbox2/fan.json"]+28:], 1300+uint32(3*f.cb()))
		}, "need"},
		"chain longer than the size": {func(t *testing.T, f *fimg) {
			binary.LittleEndian.PutUint32(f.b[f.ents["/scbox2/events.log"]+28:], 10)
		}, "need"},
		"size 0 with clusters": {func(t *testing.T, f *fimg) {
			binary.LittleEndian.PutUint32(f.b[f.ents["/scbox2/net.json"]+28:], 0)
		}, "size of 0"},
		"size without clusters": {func(t *testing.T, f *fimg) {
			binary.LittleEndian.PutUint32(f.b[f.ents["/scbox2/EMPTY"]+28:], 50)
		}, "no cluster"},
		"reserved attribute bits": {func(t *testing.T, f *fimg) {
			f.b[f.ents["/scbox2/net.json"]+11] |= 0x40
		}, "reserved attribute"},
		"control character in a name": {func(t *testing.T, f *fimg) {
			f.b[f.ents["/scbox2/net.json"]+2] = 0x01
		}, "control character"},
		"missing dot entries": {func(t *testing.T, f *fimg) {
			copy(f.b[f.clusOff(f.dirs["/minerd/"]):], "XX         ")
		}, "should be"},
		"directory cross-linked with a file": {func(t *testing.T, f *fimg) {
			e := f.ents["/scbox2/net.json"]
			c := f.dirs["/minerd/settings/"]
			binary.LittleEndian.PutUint16(f.b[e+26:], uint16(c))
		}, "cross-linked"},
		"lost cluster leading into a file": {func(t *testing.T, f *fimg) {
			ch := f.lostChain(t, 2, false)
			f.setFAT(ch[1], file(f, "/scbox2/events.log")[4])
		}, "leads into"},
		"bad mark in one copy only": {func(t *testing.T, f *fimg) {
			c := f.alloc(t, 1)[0]
			f.setFAT(c, 0)
			f.setFATk(0, c, entBad)
		}, "one FAT copy"},
	}
	for name, tc := range cases {
		for _, dirty := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/dirty=%v", name, dirty), func(t *testing.T) {
				f := sample(t, defaultOpts(), nil)
				tc.mut(t, f)
				if dirty {
					f.b[stateByte] |= stateDirty
				}
				before := append([]byte(nil), f.b...)
				r := runFix(t, f.b)
				if r.rc != 1 || !bytes.Equal(r.after, before) {
					t.Fatalf("rc %d (want 1), changed %v\n%s", r.rc, !bytes.Equal(r.after, before), r.out)
				}
				if !strings.Contains(r.marker, tc.want) || !strings.HasPrefix(r.marker, "The settings storage has damage") {
					t.Fatalf("marker %q, want %q\n%s", r.marker, tc.want, r.out)
				}
			})
		}
	}
}

func TestNotUnderstoodLeftAlone(t *testing.T) {
	cases := map[string]func(b []byte){
		"zeros":        func(b []byte) { clear(b) },
		"no signature": func(b []byte) { b[510] = 0 },
		"FAT16 layout": func(b []byte) { binary.LittleEndian.PutUint16(b[22:], 12) },
		"odd sector":   func(b []byte) { binary.LittleEndian.PutUint16(b[11:], 1000) },
		"mirroring off": func(b []byte) {
			binary.LittleEndian.PutUint16(b[40:], 0x80)
		},
		"bigger than the device": func(b []byte) { binary.LittleEndian.PutUint32(b[32:], 1<<30); b[19], b[20] = 0, 0 },
		"root outside":           func(b []byte) { binary.LittleEndian.PutUint32(b[44:], 0x0FFFFFF0) },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			f := sample(t, defaultOpts(), nil)
			f.b[stateByte] |= stateDirty
			mut(f.b)
			before := append([]byte(nil), f.b...)
			r := runFix(t, f.b)
			if r.rc != 3 || !bytes.Equal(r.after, before) || r.marker != "" {
				t.Fatalf("rc %d changed %v marker %q\n%s", r.rc, !bytes.Equal(r.after, before), r.marker, r.out)
			}
		})
	}
}

func TestMountedLeftAlone(t *testing.T) {
	f := sample(t, defaultOpts(), nil)
	f.b[stateByte] |= stateDirty
	dir := t.TempDir()
	p := filepath.Join(dir, "cfg.img")
	os.WriteFile(p, f.b, 0o644)
	mounts := filepath.Join(dir, "mounts")
	os.WriteFile(mounts, []byte(p+" /mnt/config vfat rw 0 0\n"), 0o644)
	old := mountsPath
	mountsPath = mounts
	defer func() { mountsPath = old }()
	var buf bytes.Buffer
	if rc := run([]string{"-marker", "", p}, &buf); rc != 3 || !strings.Contains(buf.String(), "mounted") {
		t.Fatalf("rc %d\n%s", rc, buf.String())
	}
	if after, _ := os.ReadFile(p); !bytes.Equal(after, f.b) {
		t.Fatal("a mounted volume was written")
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	f := sample(t, defaultOpts(), nil)
	f.lostChain(t, 2, false)
	f.b[stateByte] |= stateDirty
	before := append([]byte(nil), f.b...)
	r := runFix(t, f.b, "-n")
	if r.rc != 0 || !bytes.Equal(r.after, before) || r.marker != "" || !strings.Contains(r.out, "nothing written") {
		t.Fatalf("rc %d\n%s", r.rc, r.out)
	}
}

// memDev: an in-memory device that records writes, and can fail or "lose" them.
type memDev struct {
	b        []byte
	writes   []wrun
	failAt   int  // the write with this index fails (and nothing after it lands); -1 = none
	drop     bool // writes report success but don't land (a device that lies)
	syncFail bool
}

func (m *memDev) ReadAt(p []byte, off int64) (int, error) {
	if off >= int64(len(m.b)) {
		return 0, errors.New("eof")
	}
	return copy(p, m.b[off:]), nil
}
func (m *memDev) WriteAt(p []byte, off int64) (int, error) {
	i := len(m.writes)
	m.writes = append(m.writes, wrun{int(off / 512), len(p) / 512})
	if m.failAt >= 0 && i >= m.failAt {
		return 0, errors.New("power gone")
	}
	if !m.drop {
		copy(m.b[off:], p)
	}
	return len(p), nil
}
func (m *memDev) Sync() error {
	if m.syncFail {
		return errors.New("sync failed")
	}
	return nil
}

func repairOn(t *testing.T, d *memDev) (int, error) {
	v, err := openVolume(d, int64(len(d.b)))
	if err != nil {
		t.Fatal(err)
	}
	r, err := v.check()
	if err != nil || len(r.problems) > 0 {
		t.Fatalf("check: %v %v", err, r.problems)
	}
	return v.apply(v.makePlan(r), nil)
}

// The writes: changed sectors only, FATs first, the boot sector (the dirty flag) last, few of them.
func TestWriteOrderAndCount(t *testing.T) {
	f := sample(t, defaultOpts(), nil)
	f.lostChain(t, 2, false)
	f.lostChain(t, 1, false)
	clear(f.b[f.fsi*f.bps : (f.fsi+1)*f.bps])
	f.b[stateByte] |= stateDirty
	f.b[f.bk*f.bps+stateByte] |= stateDirty
	d := &memDev{b: append([]byte(nil), f.b...), failAt: -1}
	n, err := repairOn(t, d)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(d.writes) || n > 5 {
		t.Fatalf("%d writes: %+v", len(d.writes), d.writes)
	}
	last := d.writes[len(d.writes)-1]
	if last.sec != 0 || last.n != 1 {
		t.Fatalf("the boot sector isn't written last: %+v", d.writes)
	}
	for _, w := range d.writes[:len(d.writes)-1] {
		if w.sec == 0 {
			t.Fatalf("the boot sector written before the end: %+v", d.writes)
		}
	}
	if d.writes[0].sec < f.rsv {
		t.Fatalf("the FATs aren't written first: %+v", d.writes)
	}
	// every write is whole sectors and only covers changed sectors (or a small gap between them)
	for _, w := range d.writes {
		for s := w.sec; s < w.sec+w.n; s++ {
			if s == w.sec || s == w.sec+w.n-1 {
				if bytes.Equal(d.b[s*512:(s+1)*512], f.b[s*512:(s+1)*512]) {
					t.Fatalf("write %+v starts or ends on an unchanged sector", w)
				}
			}
		}
	}
}

// A power cut at any point of the repair: the next start repairs again, and no file is lost.
func TestPowerCutDuringRepair(t *testing.T) {
	f := sample(t, defaultOpts(), nil)
	want := f.shortPaths()
	f.lostChain(t, 2, false)
	f.lostChain(t, 5, false)
	for i := range f.b[f.fsi*f.bps : (f.fsi+1)*f.bps] {
		f.b[f.fsi*f.bps+i] = 0xFF
	}
	f.b[stateByte] |= stateDirty
	probe := &memDev{b: append([]byte(nil), f.b...), failAt: -1}
	total, err := repairOn(t, probe)
	if err != nil {
		t.Fatal(err)
	}
	for cut := 0; cut < total; cut++ {
		d := &memDev{b: append([]byte(nil), f.b...), failAt: cut}
		if _, err := repairOn(t, d); err == nil {
			t.Fatalf("cut %d: no error", cut)
		}
		if d.b[stateByte]&stateDirty == 0 {
			t.Fatalf("cut %d: the dirty flag was cleared before the rest landed", cut)
		}
		r := runFix(t, d.b)
		if r.rc != 0 {
			t.Fatalf("cut %d: next start rc %d\n%s", cut, r.rc, r.out)
		}
		checkRepaired(t, f, r.after, want)
	}
}

func TestWriteFailuresReported(t *testing.T) {
	f := sample(t, defaultOpts(), nil)
	f.lostChain(t, 2, false)
	f.b[stateByte] |= stateDirty
	for name, d := range map[string]*memDev{
		"write fails":  {b: append([]byte(nil), f.b...), failAt: 0},
		"lying device": {b: append([]byte(nil), f.b...), failAt: -1, drop: true},
		"sync fails":   {b: append([]byte(nil), f.b...), failAt: -1, syncFail: true},
	} {
		if _, err := repairOn(t, d); err == nil {
			t.Fatalf("%s: no error", name)
		}
	}
}

// ---- properties ----

type mutation struct {
	name  string
	risky bool
	apply func(t testing.TB, f *fimg, rnd *rand.Rand) bool // false = didn't apply (no suitable file)
}

func pickFile(f *fimg, rnd *rand.Rand, minClusters int) (string, []uint32) {
	var ps []string
	for p, ch := range f.chains {
		if len(ch) >= minClusters {
			ps = append(ps, p)
		}
	}
	if len(ps) == 0 {
		return "", nil
	}
	// deterministic order for a given seed
	sortStrings(ps)
	p := ps[rnd.Intn(len(ps))]
	return p, f.chains[p]
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

var mutations = []mutation{
	{"dirty", false, func(t testing.TB, f *fimg, r *rand.Rand) bool { f.b[stateByte] |= stateDirty; return true }},
	{"fsinfo-ff", false, func(t testing.TB, f *fimg, r *rand.Rand) bool {
		for i := 0; i < f.bps; i++ {
			f.b[f.fsi*f.bps+i] = 0xFF
		}
		return true
	}},
	{"fsinfo-count", false, func(t testing.TB, f *fimg, r *rand.Rand) bool {
		binary.LittleEndian.PutUint32(f.b[f.fsi*f.bps+488:], r.Uint32())
		return true
	}},
	{"lost", false, func(t testing.TB, f *fimg, r *rand.Rand) bool { f.lostChain(t, 1+r.Intn(6), false); return true }},
	{"lost-fat1", false, func(t testing.TB, f *fimg, r *rand.Rand) bool { f.lostChain(t, 1+r.Intn(3), true); return true }},
	{"delete-entry", false, func(t testing.TB, f *fimg, r *rand.Rand) bool {
		p, _ := pickFile(f, r, 1)
		if p == "" {
			return false
		}
		f.b[f.ents[p]] = 0xE5
		delete(f.files, p)
		delete(f.chains, p)
		return true
	}},
	{"cross", true, func(t testing.TB, f *fimg, r *rand.Rand) bool {
		p, a := pickFile(f, r, 1)
		q, b := pickFile(f, r, 1)
		if p == "" || p == q {
			return false
		}
		f.setFAT(a[len(a)-1], b[r.Intn(len(b))])
		return true
	}},
	{"beyond", true, func(t testing.TB, f *fimg, r *rand.Rand) bool {
		_, a := pickFile(f, r, 1)
		if a == nil {
			return false
		}
		f.setFAT(a[r.Intn(len(a))], f.maxClus+1+uint32(r.Intn(1000)))
		return true
	}},
	{"loop", true, func(t testing.TB, f *fimg, r *rand.Rand) bool {
		_, a := pickFile(f, r, 2)
		if a == nil {
			return false
		}
		f.setFAT(a[len(a)-1], a[r.Intn(len(a)-1)])
		return true
	}},
	{"truncated", true, func(t testing.TB, f *fimg, r *rand.Rand) bool {
		_, a := pickFile(f, r, 2)
		if a == nil {
			return false
		}
		f.setFAT(a[len(a)-1], 0)
		return true
	}},
	{"size", true, func(t testing.TB, f *fimg, r *rand.Rand) bool {
		p, a := pickFile(f, r, 1)
		if p == "" {
			return false
		}
		sz := binary.LittleEndian.Uint32(f.b[f.ents[p]+28:])
		n := uint32(f.cb()) * uint32(len(a)+1+r.Intn(3))
		if r.Intn(2) == 0 && len(a) > 1 {
			n = uint32(f.cb()) * uint32(len(a)-1)
		}
		if n == sz {
			return false
		}
		binary.LittleEndian.PutUint32(f.b[f.ents[p]+28:], n)
		return true
	}},
	{"disagree", true, func(t testing.TB, f *fimg, r *rand.Rand) bool {
		_, a := pickFile(f, r, 1)
		if a == nil {
			return false
		}
		c := a[r.Intn(len(a))]
		f.setFATk(1, c, f.getFAT(1, c)^uint32(1+r.Intn(5)))
		return true
	}},
}

// TestPropertyRandomDamage: random trees, random damage. Safe damage only: repaired, every file intact, idempotent.
// Any risky damage: refused, the image byte for byte as it was.
func TestPropertyRandomDamage(t *testing.T) {
	iters := 400
	if testing.Short() {
		iters = 60
	}
	for seed := 0; seed < iters; seed++ {
		rnd := rand.New(rand.NewSource(int64(seed)))
		o := defaultOpts()
		if seed%5 == 4 {
			o = fopts{total: 16384, spc: 2, rsv: 32, fsi: 1, bk: 6}
		}
		f := sample(t, o, rnd)
		var applied []string
		risky := false
		for i := 0; i < 1+rnd.Intn(4); i++ {
			m := mutations[rnd.Intn(len(mutations))]
			if m.risky && rnd.Intn(3) != 0 {
				continue // mostly safe damage
			}
			if m.apply(t, f, rnd) {
				applied = append(applied, m.name)
				risky = risky || m.risky
			}
		}
		want := f.shortPaths()
		before := append([]byte(nil), f.b...)
		r := runFix(t, f.b)
		ctx := fmt.Sprintf("seed %d %v\n%s", seed, applied, r.out)
		if risky {
			if r.rc != 1 || !bytes.Equal(r.after, before) {
				t.Fatalf("risky damage: rc %d, changed %v: %s", r.rc, !bytes.Equal(r.after, before), ctx)
			}
			continue
		}
		if r.rc != 0 {
			t.Fatalf("safe damage refused: %s", ctx)
		}
		dirty := before[stateByte]&stateDirty != 0
		fsiOK := binary.LittleEndian.Uint32(before[f.fsi*f.bps:]) == fsiLeadSig
		if !dirty && fsiOK {
			if !bytes.Equal(r.after, before) {
				t.Fatalf("a clean volume was changed: %s", ctx)
			}
			continue
		}
		checkRepaired(t, f, r.after, want)
		allowedDiff(t, before, r.after, ctx)
	}
}

// allowedDiff: what a repair may change: the dirty flag, the FSInfo sector, the backup boot sector, and FAT entries of
// clusters the directory walk of the BEFORE image doesn't reach (freed, reserved bits kept). Never the data area.
func allowedDiff(t testing.TB, before, after []byte, ctx string) {
	t.Helper()
	bps := int(binary.LittleEndian.Uint16(before[11:]))
	rsv := int(binary.LittleEndian.Uint16(before[14:]))
	nf := int(before[16])
	fsz := int(binary.LittleEndian.Uint32(before[36:]))
	fsi := int(binary.LittleEndian.Uint16(before[48:]))
	bk := int(binary.LittleEndian.Uint16(before[50:]))
	data := (rsv + nf*fsz) * bps
	if !bytes.Equal(before[data:], after[data:]) {
		t.Fatalf("the data area changed: %s", ctx)
	}
	reach := readTree(before).reachable
	for i := 0; i < data; i++ {
		if before[i] == after[i] {
			continue
		}
		s := i / bps
		switch {
		case s == 0 && i == stateByte && after[i] == before[i]&^stateDirty:
		case s == fsi || s == bk:
		case s >= rsv:
			e := (i - rsv*bps) % (fsz * bps) / 4
			o := i - (i-rsv*bps)%4
			bv, av := binary.LittleEndian.Uint32(before[o:]), binary.LittleEndian.Uint32(after[o:])
			if reach[uint32(e)] || av != bv&^entMask || e < 2 {
				t.Fatalf("FAT entry %d changed %#x -> %#x (reachable %v): %s", e, bv, av, reach[uint32(e)], ctx)
			}
		default:
			t.Fatalf("byte %d (sector %d) changed: %s", i, s, ctx)
		}
	}
}

// FuzzFatfix: arbitrary bytes written over the reserved area, the FATs and the directories of a populated image.
// Whatever it gets: no panic; a volume that was clean (not dirty, valid FSInfo) is never changed; a refusal or an
// unknown volume changes nothing; a repair changes only what allowedDiff allows, and a second run finds nothing.
func FuzzFatfix(f *testing.F) {
	f.Add([]byte{65, 1})
	f.Add([]byte{0, 0, 0, 0})
	f.Add([]byte{200, 7, 3, 0xFF, 65, 1, 4, 0})
	f.Add([]byte{14, 0, 1, 0xFF, 2, 0xFF, 3, 0xFF, 65, 1})
	f.Add([]byte{40, 9, 41, 0, 65, 1})
	base := sample(f, defaultOpts(), nil)
	// the offsets the fuzzer may hit: the reserved area and FATs, and the first directory clusters
	meta := base.dataSec * base.bps
	f.Fuzz(func(t *testing.T, script []byte) {
		img := append([]byte(nil), base.b...)
		for i := 0; i+1 < len(script); i += 2 {
			off := int(script[i]) * 7 % (meta + 8*base.cb())
			if script[i] == 65 {
				off = stateByte
			}
			if off >= meta {
				off = base.dataSec*base.bps + (off - meta)
			}
			img[off] = script[i+1]
		}
		before := append([]byte(nil), img...)
		r := runFix(t, img)
		dirty := before[stateByte]&stateDirty != 0
		fsi := int(binary.LittleEndian.Uint16(before[48:]))
		fsiOK := fsi*512+512 <= len(before) && binary.LittleEndian.Uint32(before[fsi*512:]) == fsiLeadSig &&
			binary.LittleEndian.Uint32(before[fsi*512+484:]) == fsiStructSig && binary.LittleEndian.Uint32(before[fsi*512+508:]) == fsiTrailSig
		changed := !bytes.Equal(r.after, before)
		switch {
		case r.rc != 0 && changed:
			t.Fatalf("rc %d but the image changed\n%s", r.rc, r.out)
		case r.rc == 0 && !dirty && fsiOK && changed:
			t.Fatalf("a clean volume was changed\n%s", r.out)
		case r.rc == 0 && changed:
			allowedDiff(t, before, r.after, r.out)
			r2 := runFix(t, r.after)
			if r2.rc != 0 || !bytes.Equal(r2.after, r.after) {
				t.Fatalf("second run rc %d changed %v\n%s", r2.rc, !bytes.Equal(r2.after, r.after), r2.out)
			}
		}
	})
}

// With dosfstools on the host, its own checker must agree that a repaired image is clean.
func TestAgainstFsckFat(t *testing.T) {
	fsck, err := exec.LookPath("fsck.fat")
	if err != nil {
		t.Skip("fsck.fat not on this host")
	}
	f := sample(t, defaultOpts(), nil)
	dir := t.TempDir()
	p := filepath.Join(dir, "clean.img")
	os.WriteFile(p, f.b, 0o644)
	if out, err := exec.Command(fsck, "-n", p).CombinedOutput(); err != nil {
		t.Fatalf("fsck.fat on the builder's clean image: %v\n%s", err, out)
	}
	f.lostChain(t, 3, false)
	f.b[stateByte] |= stateDirty
	r := runFix(t, f.b)
	if r.rc != 0 {
		t.Fatalf("%s", r.out)
	}
	os.WriteFile(p, r.after, 0o644)
	if out, err := exec.Command(fsck, "-n", p).CombinedOutput(); err != nil {
		t.Fatalf("fsck.fat on the repaired image: %v\n%s", err, out)
	}
}
