package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type pkgFile struct {
	path, data, link string
	mode             uint32
}

// makePkg writes an update package like build.py's.
func makePkg(t *testing.T, dst, version, baseSha string, files []pkgFile, remove []string, tamper func(name string, b []byte) []byte) {
	m := map[string]any{"format": 1, "product": brand + "-firmware", "version": version,
		"base": map[string]string{"path": "/usr/libexec/sclite/intminer", "sha256": baseSha}, "remove": remove, "remove_dirs": []string{}}
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
		if tamper != nil {
			b = tamper(name, b)
		}
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
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write(raw.Bytes())
	zw.Close()
	os.WriteFile(dst, gz.Bytes(), 0o644)
}

func setupRoot(t *testing.T) (root string, baseSha string) {
	root = t.TempDir()
	*authFile = filepath.Join(t.TempDir(), "auth.json")
	updateRoot = root
	t.Cleanup(func() { updateRoot = "/" })
	w := func(p, d string, mode os.FileMode) {
		os.MkdirAll(filepath.Dir(filepath.Join(root, p)), 0o755)
		os.WriteFile(filepath.Join(root, p), []byte(d), mode)
	}
	w("/usr/libexec/sclite/intminer", "STOCK MINER", 0o755)
	w("/usr/bin/sclite-agent", "agent 1.4.5", 0o755)
	w(brandRelease, brandAgent+" firmware build\nversion: 1.4.5\n", 0o644)
	w("/etc/init.d/S48sclite", "old boot script", 0o755)
	w("/usr/bin/wifi", "wifi daemon", 0o755)
	os.Symlink("/usr/config/sclite/localtime", filepath.Join(root, "/etc/localtime"))
	s := sha256.Sum256([]byte("STOCK MINER"))
	return root, hex.EncodeToString(s[:])
}

func readF(root, p string) string { b, _ := os.ReadFile(filepath.Join(root, p)); return string(b) }

func TestUpdateInstallAndRollback(t *testing.T) {
	root, base := setupRoot(t)
	pkg := filepath.Join(t.TempDir(), "u.tgz")
	makePkg(t, pkg, "1.4.6", base, []pkgFile{
		{path: "/usr/bin/sclite-agent", data: "agent 1.4.6", mode: 0o755},
		{path: brandRelease, data: brandAgent + " firmware build\nversion: 1.4.6\n", mode: 0o644},
		{path: "/usr/libexec/sclite/new-helper", data: "new", mode: 0o755},
		{path: "/etc/localtime", link: "/usr/config/sclite/localtime"},
	}, []string{"/usr/bin/wifi"}, nil)
	if firmwareVersion() != "1.4.5" {
		t.Fatalf("version before: %q", firmwareVersion())
	}
	m, err := installUpdate(pkg)
	if err != nil {
		t.Fatal(err)
	}
	if m.Version != "1.4.6" || firmwareVersion() != "1.4.6" || readF(root, "/usr/bin/sclite-agent") != "agent 1.4.6" ||
		readF(root, "/usr/libexec/sclite/new-helper") != "new" || readF(root, "/etc/init.d/S48sclite") != "old boot script" {
		t.Fatal("not installed as packaged")
	}
	if fi, _ := os.Stat(filepath.Join(root, "/usr/libexec/sclite/new-helper")); fi.Mode().Perm() != 0o755 {
		t.Fatalf("mode %v", fi.Mode())
	}
	if _, err := os.Stat(filepath.Join(root, "/usr/bin/wifi")); err == nil {
		t.Fatal("removal not applied")
	}
	if _, err := os.Stat(filepath.Join(updDir(), "pending")); err != nil {
		t.Fatal("no pending marker")
	}
	leftovers, _ := filepath.Glob(filepath.Join(root, "*/*/*.sclite-new"))
	if len(leftovers) > 0 {
		t.Fatalf("temp files left: %v", leftovers)
	}
	// roll back: everything as before, the added file gone
	if err := restoreBackup(); err != nil {
		t.Fatal(err)
	}
	if firmwareVersion() != "1.4.5" || readF(root, "/usr/bin/sclite-agent") != "agent 1.4.5" || readF(root, "/usr/bin/wifi") != "wifi daemon" {
		t.Fatal("rollback didn't restore the old files")
	}
	if _, err := os.Stat(filepath.Join(root, "/usr/libexec/sclite/new-helper")); err == nil {
		t.Fatal("rollback left a file the update added")
	}
	if l, _ := os.Readlink(filepath.Join(root, "/etc/localtime")); l != "/usr/config/sclite/localtime" {
		t.Fatalf("symlink after rollback: %q", l)
	}
}

func TestUpdateRefusals(t *testing.T) {
	root, base := setupRoot(t)
	dir := t.TempDir()
	ok := []pkgFile{{path: "/usr/bin/sclite-agent", data: "agent 1.4.6", mode: 0o755}}
	cases := []struct {
		name, want string
		files      []pkgFile
		base       string
		tamper     func(string, []byte) []byte
	}{
		{"damaged", "checksum", ok, base, func(n string, b []byte) []byte {
			if strings.HasPrefix(n, "files/") {
				return []byte("agent 1.4.X")
			}
			return b
		}},
		{"other miner", "different miner", ok, strings.Repeat("0", 64), nil},
		{"settings", "isn't allowed", []pkgFile{{path: "/usr/config/sclite/auth.json", data: "x", mode: 0o644}}, base, nil},
		{"escape", "isn't allowed", []pkgFile{{path: "/usr/../etc/shadow", data: "x", mode: 0o644}}, base, nil},
	}
	for _, c := range cases {
		p := filepath.Join(dir, c.name+".tgz")
		makePkg(t, p, "9.9.9", c.base, c.files, nil, c.tamper)
		if _, err := installUpdate(p); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err %v, want %q", c.name, err, c.want)
		}
	}
	os.WriteFile(filepath.Join(dir, "junk.tgz"), []byte("not a package"), 0o644)
	if _, err := verifyUpdate(filepath.Join(dir, "junk.tgz")); err == nil {
		t.Error("junk accepted")
	}
	if readF(root, "/usr/bin/sclite-agent") != "agent 1.4.5" {
		t.Fatal("a refused package changed a file")
	}
	if _, err := os.Stat(filepath.Join(updDir(), "pending")); err == nil {
		t.Fatal("a refused package left a pending marker")
	}
}

func TestVersionLess(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{{"1.4.5", "v1.4.6", true}, {"1.4.6", "v1.4.6", false}, {"1.4.9", "1.4.10", true}, {"1.5.0", "1.4.10", false}} {
		if versionLess(c.a, c.b) != c.want {
			t.Errorf("%s < %s", c.a, c.b)
		}
	}
}

// Check for updates / download against a stand-in for GitHub: the latest release, its update file and
// its SHA256SUMS. A file that doesn't match SHA256SUMS is refused.
func TestUpdateFromGitHub(t *testing.T) {
	root, base := setupRoot(t)
	_ = root
	dir := t.TempDir()
	pkg := filepath.Join(dir, "sclite-update-v1.4.7.tar.gz")
	makePkg(t, pkg, "1.4.7", base, []pkgFile{{path: "/usr/bin/sclite-agent", data: "agent 1.4.7", mode: 0o755}}, nil, nil)
	pb, _ := os.ReadFile(pkg)
	sum := sha256.Sum256(pb)
	sums := hex.EncodeToString(sum[:]) + "  sclite-update-v1.4.7.tar.gz\n"
	var srvURL string
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/me/fork/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"tag_name": "v1.4.7", "name": "v1.4.7", "html_url": srvURL + "/rel", "body": "notes",
			"assets": []map[string]any{{"name": "sclite-firmware-v1.4.7-SCLITE_MCB_V4_3.img.xz", "browser_download_url": srvURL + "/img", "size": 20000000},
				{"name": "sclite-update-v1.4.7.tar.gz", "browser_download_url": srvURL + "/upd", "size": len(pb)},
				{"name": "SHA256SUMS", "browser_download_url": srvURL + "/sums", "size": len(sums)}}})
	})
	mux.HandleFunc("/upd", func(w http.ResponseWriter, r *http.Request) { w.Write(pb) })
	mux.HandleFunc("/sums", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(sums)) })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	srvURL = srv.URL
	oldAPI, oldStage := updateAPI, updateStage
	defer func() { updateAPI, updateStage = oldAPI, oldStage }()
	updateAPI, updateStage = srv.URL, filepath.Join(dir, "stage.tgz")
	os.WriteFile(updRepoPath(), []byte("me/fork\n"), 0o644)

	call := func(body string) map[string]any {
		rec := httptest.NewRecorder()
		handleUpdate(rec, httptest.NewRequest("POST", "/api/update", strings.NewReader(body)))
		var v map[string]any
		json.Unmarshal(rec.Body.Bytes(), &v)
		return v
	}
	v := call(`{"check":true}`)
	l, _ := v["latest"].(map[string]any)
	if l == nil || l["tag"] != "v1.4.7" || l["newer"] != true || l["asset"] != "sclite-update-v1.4.7.tar.gz" {
		t.Fatalf("check: %v", v)
	}
	v = call(`{"fetch":true}`)
	if st, _ := v["staged"].(map[string]any); st == nil || st["version"] != "1.4.7" {
		t.Fatalf("fetch: %v", v)
	}
	call(`{"discard":true}`)
	sums = strings.Repeat("0", 64) + "  sclite-update-v1.4.7.tar.gz\n"
	if v = call(`{"fetch":true}`); v["ok"] != false || !strings.Contains(v["err"].(string), "SHA256SUMS") {
		t.Fatalf("a file not matching SHA256SUMS should be refused: %v", v)
	}
}

func TestNetCheckResponder(t *testing.T) {
	old := netCheckAddr
	defer func() { netCheckAddr = old }()
	ln, _ := net.Listen("tcp4", "127.0.0.1:0")
	netCheckAddr = ln.Addr().String()
	ln.Close()
	go netCheckResponder()
	for i := 0; i < 50; i++ {
		if c, err := net.Dial("tcp4", netCheckAddr); err == nil {
			b := make([]byte, 64)
			n, _ := c.Read(b)
			c.Close()
			if !strings.HasPrefix(string(b[:n]), "HTTP/1.0 204") {
				t.Fatalf("answer %q", b[:n])
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("nothing listening")
}

// A system partition that can't be written: the update stops at the first file, says nothing changed,
// and everything is as before.
func TestUpdateReadOnlyRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	root, base := setupRoot(t)
	pkg := filepath.Join(t.TempDir(), "u.tgz")
	makePkg(t, pkg, "1.4.6", base, []pkgFile{{path: "/usr/bin/sclite-agent", data: "agent 1.4.6", mode: 0o755}}, nil, nil)
	os.Chmod(filepath.Join(root, "usr/bin"), 0o555)
	defer os.Chmod(filepath.Join(root, "usr/bin"), 0o755)
	_, err := installUpdate(pkg)
	if err == nil || !strings.Contains(err.Error(), "nothing was changed") {
		t.Fatalf("err %v", err)
	}
	if readF(root, "/usr/bin/sclite-agent") != "agent 1.4.5" {
		t.Fatal("changed")
	}
}

func TestSumsMatch(t *testing.T) {
	sha := "bf6dc8d38d794681d0d40eafd1623b49b7a628ba1ad83fc2cf3e853748f9d2ee"
	sums := "7a01a3244403f756161cc4a8447b204a3f969ebda1384ece4aa596e62f99043f  sclite-firmware-v1.4.8-SCLITE_MCB_V4_3.img.xz\n" +
		strings.ToUpper(sha) + "  sclite-update-v1.4.8.tar.gz\n"
	if r, n := sumsMatch(sums, sha); r != "match" || n != 2 {
		t.Fatal(r, n)
	}
	if r, _ := sumsMatch(sha, sha); r != "match" {
		t.Fatal("a bare checksum")
	}
	if r, n := sumsMatch("7a01a3244403f756161cc4a8447b204a3f969ebda1384ece4aa596e62f99043f  x", sha); r != "nomatch" || n != 1 {
		t.Fatal(r, n)
	}
	if _, n := sumsMatch("sclite-update-v1.4.8.tar.gz deadbeef", sha); n != 0 {
		t.Fatal("no checksum in that text")
	}
}
