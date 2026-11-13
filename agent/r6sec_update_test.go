package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// r6-security: a package must not write outside its allowed paths through a symlink (one it brings along,
// or one already on the system), and a package whose gzip stream is cut short or damaged is refused.

func r6Pkg(t *testing.T, dst, baseSha string, files []pkgFile, pad int) []byte {
	m := map[string]any{"format": 1, "product": brand + "-firmware", "version": "9.9.9",
		"base": map[string]string{"path": "/usr/libexec/sclite/intminer", "sha256": baseSha}, "remove": []string{}, "remove_dirs": []string{}}
	var fl []map[string]any
	for _, f := range files {
		if f.link != "" {
			fl = append(fl, map[string]any{"path": f.path, "type": "link", "target": f.link})
		} else {
			s := sha256.Sum256([]byte(f.data))
			fl = append(fl, map[string]any{"path": f.path, "type": "file", "mode": f.mode, "size": len(f.data), "sha256": hex.EncodeToString(s[:])})
		}
	}
	m["files"] = fl
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	add := func(name string, b []byte) {
		tw.WriteHeader(&tar.Header{Name: name, Size: int64(len(b)), Mode: 0o644, Typeflag: tar.TypeReg})
		tw.Write(b)
	}
	mb, _ := json.Marshal(m)
	add("manifest.json", mb)
	for _, f := range files {
		if f.link == "" {
			add("files"+f.path, []byte(f.data))
		}
	}
	tw.Close()
	raw.Write(make([]byte, pad)) // Python's tarfile pads the archive to a 10240-byte record
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write(raw.Bytes())
	zw.Close()
	os.WriteFile(dst, gz.Bytes(), 0o644)
	return gz.Bytes()
}

func TestR6UpdateLinkEscape(t *testing.T) {
	root, base := setupRoot(t)
	os.MkdirAll(filepath.Join(root, "usr/config/sclite"), 0o755)
	os.WriteFile(filepath.Join(root, "usr/config/sclite/auth.json"), []byte("OWNER LOGIN"), 0o600)
	pkg := filepath.Join(t.TempDir(), "u.tgz")
	r6Pkg(t, pkg, base, []pkgFile{
		{path: "/opt/c", link: "../usr/config/sclite"},
		{path: "/opt/c/auth.json", data: "ATTACKER LOGIN", mode: 0o600},
	}, 0)
	_, verr := verifyUpdate(pkg)
	_, ierr := installUpdate(pkg)
	got := readF(root, "/usr/config/sclite/auth.json")
	if got != "OWNER LOGIN" {
		t.Errorf("the package wrote the settings partition through its own symlink: auth.json = %q", got)
	}
	if verr == nil || ierr == nil {
		t.Errorf("a file under a link the package itself brings must be refused (verify %v, install %v)", verr, ierr)
	}
}

func TestR6UpdateExistingLinkEscape(t *testing.T) {
	root, base := setupRoot(t)
	os.MkdirAll(filepath.Join(root, "tmp"), 0o755)
	os.MkdirAll(filepath.Join(root, "var"), 0o755)
	os.Symlink("../tmp", filepath.Join(root, "var/log")) // as on the real system partition
	pkg := filepath.Join(t.TempDir(), "u.tgz")
	r6Pkg(t, pkg, base, []pkgFile{{path: "/var/log/x", data: "X", mode: 0o644}}, 0)
	if _, err := verifyUpdate(pkg); err == nil {
		t.Errorf("/var/log/x lands in /tmp through the system's own /var/log -> ../tmp link: must be refused")
	}
}

func TestR6UpdateLinkNotEscaping(t *testing.T) { // a package's ordinary link and a file beside it still install
	root, base := setupRoot(t)
	pkg := filepath.Join(t.TempDir(), "u.tgz")
	r6Pkg(t, pkg, base, []pkgFile{
		{path: "/usr/bin/sclite-agent", data: "agent 9.9.9", mode: 0o755},
		{path: "/usr/sbin/agent", link: "../bin/sclite-agent"},
		{path: brandRelease, data: "version: 9.9.9\n", mode: 0o644},
	}, 10240)
	if _, err := installUpdate(pkg); err != nil {
		t.Fatalf("a normal package was refused: %v", err)
	}
	if readF(root, "/usr/bin/sclite-agent") != "agent 9.9.9" {
		t.Errorf("not installed")
	}
}

func TestR6UpdateTruncatedOrDamagedGzip(t *testing.T) {
	_, base := setupRoot(t)
	dir := t.TempDir()
	good := r6Pkg(t, filepath.Join(dir, "good.tgz"), base, []pkgFile{{path: "/usr/bin/sclite-agent", data: strings.Repeat("agent 9.9.9 ", 50), mode: 0o755}}, 10240)
	if _, err := verifyUpdate(filepath.Join(dir, "good.tgz")); err != nil {
		t.Fatalf("good package refused: %v", err)
	}
	cases := map[string][]byte{}
	for _, cut := range []int{1, 4, 8, 12, 20} {
		cases["cut "+itoa(cut)] = append([]byte(nil), good[:len(good)-cut]...)
	}
	crc := append([]byte(nil), good...)
	crc[len(crc)-8] ^= 0xff // the gzip trailer's CRC32
	cases["bad crc"] = crc
	for name, b := range cases {
		p := filepath.Join(dir, "bad.tgz")
		os.WriteFile(p, b, 0o644)
		if _, err := verifyUpdate(p); err == nil {
			t.Errorf("%s: a damaged package was accepted", name)
		}
	}
}
