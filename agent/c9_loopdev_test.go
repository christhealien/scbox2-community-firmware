package main

// C9 on a real block device: a loop device holding the stock system squashfs, MOUNTED while it is written.
// Skipped unless C9_LOOP_DEV (e.g. /dev/loop0, 18,874,368 bytes), C9_LOOP_MNT (where it is mounted) and
// C9_NEW_SQSH (another squashfs) are set; see REPORT.md for the recipe. Never touches anything else.

import (
	"crypto/sha256"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func hashTree(t *testing.T, root string, limit int) (map[string][32]byte, int) {
	out, errs := map[string][32]byte{}, 0
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			errs++
			return nil
		}
		if d.Type().IsRegular() && len(out) < limit {
			b, err := os.ReadFile(p)
			if err != nil {
				errs++
				return nil
			}
			out[p] = sha256.Sum256(b)
		}
		return nil
	})
	return out, errs
}

func TestC9_LoopDeviceMounted(t *testing.T) {
	dev, mnt, newp := os.Getenv("C9_LOOP_DEV"), os.Getenv("C9_LOOP_MNT"), os.Getenv("C9_NEW_SQSH")
	if dev == "" || mnt == "" || newp == "" {
		t.Skip("set C9_LOOP_DEV, C9_LOOP_MNT, C9_NEW_SQSH to run against a loop device")
	}
	od, op := imgDevPath, imgPartBytes
	imgDevPath, imgPartBytes = dev, box2SysBytes
	t.Cleanup(func() { imgDevPath, imgPartBytes = od, op })
	cur := make([]byte, box2SysBytes)
	f, err := os.Open(dev)
	if err != nil {
		t.Fatal(err)
	}
	if sz, err := devBytes(f); err != nil || sz != box2SysBytes {
		t.Fatalf("BLKGETSIZE64: %d %v", sz, err)
	}
	f.ReadAt(cur, 0)
	f.Close()
	sb, _ := parseSqSuper(cur)
	same := cur[:(int(sb.BytesUsed)+4095)/4096*4096]
	before, e0 := hashTree(t, mnt, 1<<30)
	t.Logf("mounted tree: %d files read, %d errors", len(before), e0)

	// 1. the same bytes, while mounted: the mounted system must stay byte-identical
	res, err := writeSystemImage(dev, same, box2SysBytes)
	if err != nil || !res.ReadBack {
		t.Fatalf("same-image write: %+v %v", res, err)
	}
	after, e1 := hashTree(t, mnt, 1<<30)
	diff := 0
	for p, h := range before {
		if after[p] != h {
			diff++
		}
	}
	t.Logf("after rewriting the same image: %d files, %d errors, %d differ", len(after), e1, diff)
	if diff != 0 || e1 != e0 {
		t.Fatal("rewriting the same image changed what the mounted system reads")
	}

	// 2. another image, while mounted: written and read back whole; then what the mount sees (expected: broken)
	nb, err := os.ReadFile(newp)
	if err != nil {
		t.Fatal(err)
	}
	nb = append(nb, make([]byte, (4096-len(nb)%4096)%4096)...)
	res, err = writeSystemImage(dev, nb, box2SysBytes)
	if err != nil || !res.ReadBack {
		t.Fatalf("new-image write: %+v %v", res, err)
	}
	disk, _ := os.ReadFile(dev)
	for i := range nb {
		if disk[i] != nb[i] {
			t.Fatalf("disk differs at %d", i)
		}
	}
	for i := len(nb); i < len(disk); i++ {
		if disk[i] != cur[i] {
			t.Fatalf("tail changed at %d", i)
		}
	}
	after2, e2 := hashTree(t, mnt, 1<<30)
	same2 := 0
	for p, h := range before {
		if after2[p] == h {
			same2++
		}
	}
	t.Logf("after writing a DIFFERENT image under the mount: %d files readable, %d errors, %d still read as before (cached pages)", len(after2), e2, same2)

	// 3. put the original back (what the restore path would do), read back whole
	if res, err = writeSystemImage(dev, cur[:len(nb)], box2SysBytes); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if res, err = writeSystemImage(dev, same, box2SysBytes); err != nil {
		t.Fatalf("restore 2: %v", err)
	}
}
