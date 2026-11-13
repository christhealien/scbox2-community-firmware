package main

// C9 (owner's redesign of Settings → Update on the SC Box II): an uploaded image is checked against its release's
// SHA256SUMS online, the way a download is; when that can't run, the owner pastes the line by hand.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// fakeGitHub: releases by tag (tag → SHA256SUMS text; "" = a release without SHA256SUMS) and a latest release.
func fakeGitHub(t *testing.T, rels map[string]string, latest string, ius []byte) {
	t.Helper()
	var srvURL string
	mux := http.NewServeMux()
	rel := func(w http.ResponseWriter, tag string) {
		sums, ok := rels[tag]
		if !ok {
			http.NotFound(w, nil)
			return
		}
		as := []map[string]any{{"name": brand + "-firmware-" + tag + ".img", "browser_download_url": srvURL + "/img", "size": 9}}
		if ius != nil {
			as = append(as, map[string]any{"name": brand + "-firmware-v" + strings.TrimPrefix(tag, "v") + ".ius", "browser_download_url": srvURL + "/ius", "size": len(ius)})
		}
		if sums != "" {
			as = append(as, map[string]any{"name": "SHA256SUMS", "browser_download_url": srvURL + "/sums/" + tag, "size": len(sums)})
		}
		json.NewEncoder(w).Encode(map[string]any{"tag_name": tag, "name": tag, "html_url": srvURL + "/rel", "body": "notes", "assets": as})
	}
	mux.HandleFunc("/repos/me/fork/releases/tags/", func(w http.ResponseWriter, r *http.Request) {
		rel(w, strings.TrimPrefix(r.URL.Path, "/repos/me/fork/releases/tags/"))
	})
	mux.HandleFunc("/repos/me/fork/releases/latest", func(w http.ResponseWriter, r *http.Request) { rel(w, latest) })
	mux.HandleFunc("/sums/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(rels[strings.TrimPrefix(r.URL.Path, "/sums/")]))
	})
	mux.HandleFunc("/ius", func(w http.ResponseWriter, r *http.Request) { w.Write(ius) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	srvURL = srv.URL
	updateAPI = srv.URL // box2On puts it back
	os.WriteFile(updRepoPath(), []byte("me/fork\n"), 0o644)
}

func stagedOf(t *testing.T, m map[string]any) map[string]any {
	t.Helper()
	st, _ := m["staged"].(map[string]any)
	if st == nil {
		t.Fatalf("nothing staged: %v", m)
	}
	return st
}

func genuineSetup(t *testing.T) []byte {
	box2On(t)
	fakeSystem(t, 6<<20, fakeSquashfs(3<<20, 1), 200000)
	return fakeIUS(t, stdItems(fakeSquashfs(2<<20, 9)), true)
}

func shaHex(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func TestC9_UploadGenuineOnline(t *testing.T) {
	ius := genuineSetup(t)
	fakeGitHub(t, map[string]string{"v0.5.0": shaHex([]byte("img")) + "  " + brand + "-firmware-v0.5.0.img\n" + shaHex(ius) + " *" + brand + "-firmware-v0.5.0.ius\n"}, "v0.5.0", nil)
	// a browser's copy number in the name doesn't hide the version
	st := stagedOf(t, post(t, "/api/update/upload?name="+brand+"-firmware-v0.5.0%20(1).ius", string(ius)))
	if st["version"] != "0.5.0" || st["genuine"] != false || st["lookup"] != "" || st["ready"] != "" {
		t.Fatalf("uploaded, not checked yet: %v", st)
	}
	m := post(t, "/api/update", `{"verify":true}`)
	st = stagedOf(t, m)
	if m["verified"] != true || st["sums"] != "online" || st["sums_from"] != "online" || st["sums_tag"] != "v0.5.0" || st["genuine"] != true || st["ready"] != "ok" || st["lookup"] != "done" {
		t.Fatalf("an uploaded release file must be found genuine online: %v", m)
	}
	if m := post(t, "/api/update", `{"install":true,"ack":true,"password":"`+c9TestPw+`"}`); m["ok"] != false || !strings.Contains(m["err"].(string), "handoff (test)") {
		t.Fatalf("a genuine file installs: %v", m)
	}
}

func TestC9_UploadMismatchOnline(t *testing.T) {
	ius := genuineSetup(t)
	fakeGitHub(t, map[string]string{"0.5.0": strings.Repeat("ab", 32) + "  " + brand + "-firmware-v0.5.0.ius\n"}, "0.5.0", nil) // tagged without the v
	post(t, "/api/update/upload?name="+brand+"-firmware-v0.5.0.ius", string(ius))
	m := post(t, "/api/update", `{"verify":true}`)
	st := stagedOf(t, m)
	if m["verified"] != false || st["sums"] != "nomatch" || st["sums_from"] != "online" || st["sums_tag"] != "0.5.0" || st["genuine"] != false {
		t.Fatalf("a file the release doesn't list must be a mismatch: %v", m)
	}
	// pasting can't overrule the release itself
	if st := stagedOf(t, post(t, "/api/update", `{"sums":"`+shaHex(ius)+`"}`)); st["sums"] != "nomatch" {
		t.Fatalf("a pasted checksum must not overrule the release's: %v", st)
	}
	if m := post(t, "/api/update", `{"install":true,"ack":true}`); m["ok"] != false || !strings.Contains(m["err"].(string), "SHA256SUMS") {
		t.Fatalf("a mismatching file must not install: %v", m)
	}
}

func TestC9_UploadLookupUnavailable(t *testing.T) {
	ius := genuineSetup(t)
	// offline (box2On points the API at a closed port)
	post(t, "/api/update/upload?name="+brand+"-firmware-v0.6.0.ius", string(ius))
	st := stagedOf(t, post(t, "/api/update", `{"verify":true}`))
	if st["lookup"] != "unavailable" || !strings.Contains(st["lookup_why"].(string), "reach GitHub") || st["genuine"] != false || st["ready"] != "ok" {
		t.Fatalf("offline: the owner pastes: %v", st)
	}
	// online, but no such release; then a release without SHA256SUMS; then a file named without its version
	fakeGitHub(t, map[string]string{"v0.4.0": "x", "v0.6.0": ""}, "v0.4.0", nil)
	st = stagedOf(t, post(t, "/api/update", `{"verify":true}`))
	if st["lookup"] != "unavailable" || !strings.Contains(st["lookup_why"].(string), "SHA256SUMS") {
		t.Fatalf("a release without SHA256SUMS: the owner pastes: %v", st)
	}
	post(t, "/api/update/upload?name="+brand+"-firmware-v0.7.0.ius", string(ius))
	st = stagedOf(t, post(t, "/api/update", `{"verify":true}`))
	if st["lookup"] != "unavailable" || !strings.Contains(st["lookup_why"].(string), "no release v0.7.0") {
		t.Fatalf("no such release: the owner pastes: %v", st)
	}
	post(t, "/api/update/upload?name=firmware.ius", string(ius))
	st = stagedOf(t, post(t, "/api/update", `{"verify":true}`))
	if st["lookup"] != "unavailable" || !strings.Contains(st["lookup_why"].(string), "doesn't say which version") {
		t.Fatalf("no version: the owner pastes: %v", st)
	}
	if m := post(t, "/api/update", `{"install":true,"ack":true}`); m["ok"] != false || !strings.Contains(m["err"].(string), "SHA256SUMS") {
		t.Fatalf("not checked: no install: %v", m)
	}
	// the manual fallback: a wrong line, then the bare hash
	if st := stagedOf(t, post(t, "/api/update", `{"sums":"`+strings.Repeat("0", 64)+`  x.ius"}`)); st["sums"] != "nomatch" || st["sums_from"] != "pasted" {
		t.Fatalf("a wrong pasted line: %v", st)
	}
	if st := stagedOf(t, post(t, "/api/update", `{"sums":"`+strings.ToUpper(shaHex(ius))+`"}`)); st["genuine"] != true || st["sums_from"] != "pasted" {
		t.Fatalf("the bare hash pasted must match: %v", st)
	}
	if m := post(t, "/api/update", `{"verify":true}`); m["verified"] != true {
		t.Fatalf("pasted and checked: %v", m)
	}
	if m := post(t, "/api/update", `{"install":true,"ack":true,"password":"`+c9TestPw+`"}`); m["ok"] != false || !strings.Contains(m["err"].(string), "handoff (test)") {
		t.Fatalf("a pasted-genuine file installs: %v", m)
	}
}

// A download from GitHub is checked as it downloads (unchanged) and says which release it matched.
func TestC9_DownloadGenuine(t *testing.T) {
	ius := genuineSetup(t)
	fakeGitHub(t, map[string]string{"v0.5.0": shaHex(ius) + "  " + brand + "-firmware-v0.5.0.ius\n"}, "v0.5.0", ius)
	st := stagedOf(t, post(t, "/api/update", `{"fetch":true}`))
	if st["sums"] != "github" || st["sums_from"] != "github" || st["sums_tag"] != "v0.5.0" || st["genuine"] != true {
		t.Fatalf("download: %v", st)
	}
	if m := post(t, "/api/update", `{"verify":true}`); m["verified"] != true || stagedOf(t, m)["ready"] != "ok" {
		t.Fatalf("download, verified: %v", m)
	}
}
