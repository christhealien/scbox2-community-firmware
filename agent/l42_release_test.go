package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// GitHub serves every release asset (browser_download_url) as a 302 to another host
// (release-assets.githubusercontent.com). The panel's download must follow it, for the update file and for
// SHA256SUMS; on base it stops at the 302 ("download failed: 302 Found") because updateClient() inherits the
// notification client's no-redirect rule.
func TestL42GitHubAssetRedirect(t *testing.T) {
	_, base := setupRoot(t)
	dir := t.TempDir()
	pkg := filepath.Join(dir, "sclite-update-v1.4.7.tar.gz")
	makePkg(t, pkg, "1.4.7", base, []pkgFile{{path: "/usr/bin/sclite-agent", data: "agent 1.4.7", mode: 0o755}}, nil, nil)
	pb, _ := os.ReadFile(pkg)
	sum := sha256.Sum256(pb)
	sums := hex.EncodeToString(sum[:]) + "  sclite-update-v1.4.7.tar.gz\n"

	// the asset host (stands in for release-assets.githubusercontent.com)
	assets := http.NewServeMux()
	assets.HandleFunc("/blob/upd", func(w http.ResponseWriter, r *http.Request) { w.Write(pb) })
	assets.HandleFunc("/blob/sums", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(sums)) })
	asrv := httptest.NewServer(assets)
	defer asrv.Close()

	var srvURL string
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/me/fork/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"tag_name": "v1.4.7", "name": "v1.4.7", "html_url": srvURL + "/rel", "body": "notes",
			"assets": []map[string]any{{"name": "sclite-update-v1.4.7.tar.gz", "browser_download_url": srvURL + "/dl/upd", "size": len(pb)},
				{"name": "SHA256SUMS", "browser_download_url": srvURL + "/dl/sums", "size": len(sums)}}})
	})
	mux.HandleFunc("/dl/upd", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, asrv.URL+"/blob/upd", http.StatusFound)
	})
	mux.HandleFunc("/dl/sums", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, asrv.URL+"/blob/sums", http.StatusFound)
	})
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
	v := call(`{"fetch":true}`)
	st, _ := v["staged"].(map[string]any)
	if st == nil || st["version"] != "1.4.7" {
		t.Fatalf("a GitHub-style (302) asset download should work: %v", v)
	}
	if st["sums"] != "github" {
		t.Fatalf("SHA256SUMS behind a 302 should be fetched and matched, sums=%v", st["sums"])
	}
	call(`{"discard":true}`)
	sums = strings.Repeat("0", 64) + "  sclite-update-v1.4.7.tar.gz\n"
	if v = call(`{"fetch":true}`); v["ok"] != false || !strings.Contains(v["err"].(string), "SHA256SUMS") {
		t.Fatalf("a file not matching SHA256SUMS (behind a 302) should be refused: %v", v)
	}
}

// The update client follows redirects but never from https to plain http, and not endlessly.
func TestL42UpdateRedirectPolicy(t *testing.T) {
	c := updateClient()
	if c.CheckRedirect == nil {
		return // Go's default policy (10 hops); nothing to check here
	}
	u := func(s string) *http.Request { p, _ := url.Parse(s); return &http.Request{URL: p} }
	if err := c.CheckRedirect(u("https://release-assets.githubusercontent.com/x"), []*http.Request{u("https://github.com/a")}); err != nil {
		t.Fatalf("https -> https redirect refused: %v", err)
	}
	if err := c.CheckRedirect(u("http://evil.example/x"), []*http.Request{u("https://github.com/a")}); err == nil {
		t.Fatal("https -> http redirect should be refused")
	}
	via := make([]*http.Request, 10)
	for i := range via {
		via[i] = u("https://github.com/a")
	}
	if err := c.CheckRedirect(u("https://github.com/b"), via); err == nil {
		t.Fatal("endless redirects should be refused")
	}
}
