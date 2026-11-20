// scbox2-fatfix: the SC Box II's start-up check of its settings partition (a 3 MiB FAT32 on /dev/spiblock2), run by
// /etc/init.d/S38initdir BEFORE the partition is mounted. A program of its own: it links none of the panel.
//
// After a power cut the partition can come up "dirty", with clusters allocated that no file reaches any more (a
// file being replaced when the power went), and with an FSInfo sector the kernel distrusts. The kernel mounts it
// anyway; the first time it trips over the damage it remounts it read-only (errors=remount-ro), and from then on
// no setting can be saved until the next boot. The image has no fsck.fat. This is a small, conservative stand-in:
//
//   - it only acts on a FAT32 volume whose boot sector it fully understands; anything else is left alone;
//   - it reads the reserved sectors, every FAT copy and every directory reachable from the root, and checks every
//     chain and directory entry, before it writes anything;
//   - when the volume is dirty or its FSInfo sector is invalid, and nothing else is wrong, it repairs: clusters
//     allocated in a FAT but reached by no directory entry ("lost chains") are freed in every FAT copy, the FSInfo
//     sector gets valid signatures, the true free count and a next-free hint, the dirty flag is cleared, and the
//     backup boot sector is kept equal to the boot sector. Changed sectors only, in as few writes as it can, the
//     dirty flag last; then everything written is read back and compared;
//   - it REFUSES to change anything when it finds damage that a repair could make worse: chains shared by two
//     files (cross-linked), chains that leave the volume's clusters, loops, chains that run into a free or bad
//     cluster, FAT copies that disagree on a file, sizes that don't match their chains, malformed directory
//     entries, a lost cluster leading into a file. It then exits non-zero and writes a one-line reason to the
//     marker file (default /tmp/scbox2-cfg-repair) for the panel to show; S38initdir mounts the partition anyway
//     (the kernel's errors=remount-ro still protects it);
//   - the volume label is left as it is (the factory one is lowercase, which some checkers flag; it is harmless).
//
// Exit status: 0 clean or repaired (and read back), 1 damage it won't repair (nothing changed), 2 a repair write
// failed or didn't read back, 3 not a volume it understands, mounted, or unreadable (nothing changed).
//
//	scbox2-fatfix [-n] [-marker FILE] DEVICE
//
// It is also the boot scripts' way to save a setting (v0.5.2): the partition is mounted read-only at
// /mnt/cfgflash and every program works on a RAM copy at /mnt/config (internal/cfgwin). "commit" copies the named
// files or folders of the RAM copy to the partition in one short write window (remount read-write, crash-safe
// writes with a .bak, sync, remount read-only), taking the same lock as the panel and the SSH server:
//
//	scbox2-fatfix commit [-ram DIR] [-flash DIR] [-lock FILE] REL...    (exit 0 saved or nothing to save, 1 not saved)
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"sclite-agent/internal/cfgwin"
)

const version = "scbox2-fatfix 2"

var mountsPath = "/proc/mounts" // (tests)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout))
}

func run(args []string, out io.Writer) int {
	if len(args) > 0 && args[0] == "commit" {
		return commit(args[1:], out)
	}
	fs := flag.NewFlagSet("scbox2-fatfix", flag.ContinueOnError)
	fs.SetOutput(out)
	dry := fs.Bool("n", false, "check only: report what a repair would do, write nothing (no marker either)")
	marker := fs.String("marker", "/tmp/scbox2-cfg-repair", "where a refusal's reason is written for the panel (\"\" = nowhere)")
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 {
		fmt.Fprintln(out, "usage: scbox2-fatfix [-n] [-marker FILE] DEVICE")
		return 3
	}
	path := fs.Arg(0)
	say := func(f string, a ...any) { fmt.Fprintf(out, "fatfix: "+f+"\n", a...) }
	say("%s: checking %s%s", version, path, map[bool]string{true: " (check only)", false: ""}[*dry])
	setMarker := func(why string) {
		if *dry || *marker == "" {
			return
		}
		if why == "" {
			os.Remove(*marker)
			return
		}
		if err := os.WriteFile(*marker, []byte(why+"\n"), 0o644); err != nil {
			say("couldn't write %s: %v", *marker, err)
		}
	}
	if m := mountedAs(path); m != "" {
		say("%s is mounted on %s: nothing done", path, m)
		return 3
	}
	flags := os.O_RDWR
	if *dry {
		flags = os.O_RDONLY
	}
	f, err := os.OpenFile(path, flags, 0)
	if err != nil {
		say("can't open %s: %v: nothing done", path, err)
		return 3
	}
	defer f.Close()
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		say("can't size %s: %v: nothing done", path, err)
		return 3
	}
	v, err := openVolume(f, size)
	if err != nil {
		say("%v: nothing done", err)
		return 3
	}
	say("FAT32: %d clusters of %d bytes, %d FAT copies of %d sectors, root at cluster %d", v.nclus, v.clusterBytes(), v.nfats, v.fatSz, v.root)
	r, err := v.check()
	if err != nil {
		say("%v: nothing done", err)
		return 3
	}
	state := "clean"
	if r.dirty {
		state = "dirty (not cleanly unmounted)"
	}
	say("%d directories, %d files, %d clusters in use; the volume is %s", r.dirs, r.files, r.used, state)
	if r.fsInfoBad != "" {
		say("FSInfo sector invalid: %s", r.fsInfoBad)
	}
	if len(r.lost) > 0 {
		say("%d lost cluster chain(s), %d cluster(s) in all: allocated, but no file or directory reaches them", r.lostChains, len(r.lost))
	}
	if len(r.problems) > 0 {
		for _, p := range r.problems {
			say("DAMAGE: %s", p)
		}
		say("damage this check won't repair by itself: nothing changed")
		setMarker("The settings storage has damage the start-up check won't repair by itself: " + strings.Join(r.problems, "; "))
		return 1
	}
	if !r.dirty && r.fsInfoBad == "" {
		if len(r.lost) > 0 {
			say("the volume is clean: the lost clusters are left as they are")
		}
		say("nothing to do")
		setMarker("")
		return 0
	}
	p := v.makePlan(r)
	runs := v.writeRuns(p.newMeta)
	say("repair: free %d lost cluster(s), FSInfo free count %d next free %d, clear the dirty flag; %s (%d write(s))", p.freed, p.free, p.nextFree, p.backup, len(runs))
	if *dry {
		say("check only: nothing written")
		return 0
	}
	n, err := v.apply(p, func() { flushBuffers(f) })
	if err != nil {
		say("REPAIR FAILED: %v", err)
		setMarker("The start-up repair of the settings storage failed: " + err.Error())
		return 2
	}
	say("repaired in %d write(s), read back and verified", n)
	setMarker("")
	return 0
}

// flushBuffers drops the kernel's cached copy of a block device (BLKFLSBUF), so a read-back comes from the device.
// On a regular file (the tests) it fails and does nothing.
func flushBuffers(f *os.File) {
	const blkflsbuf = 0x1261
	syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), blkflsbuf, 0)
}

// mountedAs: where path is mounted ("" = not mounted).
func mountedAs(path string) string {
	b, err := os.ReadFile(mountsPath)
	if err != nil {
		return ""
	}
	want := map[string]bool{path: true}
	if p, err := filepath.EvalSymlinks(path); err == nil {
		want[p] = true
	}
	for _, l := range strings.Split(string(b), "\n") {
		fl := strings.Fields(l)
		if len(fl) >= 2 && want[fl[0]] {
			return fl[1]
		}
	}
	return ""
}

// commit: the named files/folders of the RAM copy onto the settings partition, in one write window.
func commit(args []string, out io.Writer) int {
	fs := flag.NewFlagSet("scbox2-fatfix commit", flag.ContinueOnError)
	fs.SetOutput(out)
	ram := fs.String("ram", cfgwin.RAMRoot, "the RAM copy every program uses")
	flash := fs.String("flash", cfgwin.FlashRoot, "where the settings partition is mounted (read-only)")
	lk := fs.String("lock", cfgwin.LockPath, "the write window's lock (shared with the panel and the SSH server)")
	if err := fs.Parse(args); err != nil || fs.NArg() == 0 {
		fmt.Fprintln(out, "usage: scbox2-fatfix commit [-ram DIR] [-flash DIR] [-lock FILE] REL...")
		return 1
	}
	cfgwin.RAMRoot, cfgwin.FlashRoot, cfgwin.LockPath = *ram, *flash, *lk
	cfgwin.Logf = func(f string, a ...any) { fmt.Fprintf(out, "fatfix: "+f+"\n", a...) }
	var rels []string
	for _, r := range fs.Args() {
		r = strings.TrimPrefix(filepath.Clean("/"+r), "/")
		if r == "" || r == "." {
			fmt.Fprintln(out, "fatfix: commit: refusing the whole partition (name the files)")
			return 1
		}
		rels = append(rels, r)
	}
	if !cfgwin.Active() {
		fmt.Fprintf(out, "fatfix: commit: no RAM copy in use (%s is the partition itself): nothing to do\n", *ram)
		return 0
	}
	if err := cfgwin.Commit(rels...); err != nil {
		fmt.Fprintf(out, "fatfix: commit %s: NOT SAVED: %v\n", strings.Join(rels, " "), err)
		return 1
	}
	return 0
}
