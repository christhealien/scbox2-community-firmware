//go:build e2e

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// UPD_ROOT: an unpacked older sclite system partition; UPD_PKG: an update package; UPD_WANT: the system
// partition of the image built with that package. After installing, ROOT must equal WANT.
func TestUpdateRealImage(t *testing.T) {
	root, pkg, want := os.Getenv("UPD_ROOT"), os.Getenv("UPD_PKG"), os.Getenv("UPD_WANT")
	*authFile = filepath.Join(t.TempDir(), "auth.json")
	updateRoot = root
	t.Logf("before: version %q", firmwareVersion())
	m, err := installUpdate(pkg)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("installed %s: %d files, %d removals; now version %q", m.Version, len(m.Files), len(m.Remove), firmwareVersion())
	compare(t, root, want)
	if err := restoreBackup(); err != nil {
		t.Fatal(err)
	}
	t.Logf("after roll back: version %q", firmwareVersion())
	if old := os.Getenv("UPD_OLD"); old != "" {
		compare(t, root, old)
	}
}

func compare(t *testing.T, got, want string) {
	t.Helper()
	diffs := 0
	seen := map[string]bool{}
	filepath.Walk(want, func(p string, fi os.FileInfo, err error) error {
		rel, _ := filepath.Rel(want, p)
		seen[rel] = true
		g := filepath.Join(got, rel)
		gi, err := os.Lstat(g)
		if err != nil {
			t.Errorf("missing: /%s", rel)
			diffs++
			return nil
		}
		wi, _ := os.Lstat(p)
		switch {
		case wi.Mode()&os.ModeSymlink != 0:
			a, _ := os.Readlink(p)
			b, _ := os.Readlink(g)
			if a != b {
				t.Errorf("link differs: /%s %q vs %q", rel, b, a)
				diffs++
			}
		case wi.Mode().IsRegular():
			a, _ := os.ReadFile(p)
			b, _ := os.ReadFile(g)
			if !bytes.Equal(a, b) || wi.Mode().Perm() != gi.Mode().Perm() {
				t.Errorf("file differs: /%s (mode %v vs %v)", rel, gi.Mode().Perm(), wi.Mode().Perm())
				diffs++
			}
		}
		return nil
	})
	filepath.Walk(got, func(p string, fi os.FileInfo, err error) error {
		rel, _ := filepath.Rel(got, p)
		if !seen[rel] {
			t.Errorf("extra: /%s", rel)
			diffs++
		}
		return nil
	})
	t.Logf("compared with %s: %d differences", filepath.Base(want), diffs)
}

// Install only (UPD_AUTH: the auth file whose folder holds update/), for the boot-script rollback test.
func TestUpdateInstallOnly(t *testing.T) {
	*authFile = os.Getenv("UPD_AUTH")
	updateRoot = os.Getenv("UPD_ROOT")
	m, err := installUpdate(os.Getenv("UPD_PKG"))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("installed %s, version now %q", m.Version, firmwareVersion())
}
