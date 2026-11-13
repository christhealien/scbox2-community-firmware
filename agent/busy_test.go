package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The chip report answers 500 while the boards restart (the tuner's baseline, a setting change): the
// panel shows the last reading with a plain note, or a plain note alone, never "HTTP 500".
func TestChipsWhileBoardsRestart(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte("Server Error"))
	}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	oldWeb, oldBfg, oldUp := *minerWeb, *bfgPath, uptimePath
	*minerWeb = host
	dir := t.TempDir()
	*bfgPath = filepath.Join(dir, "bfgminer.json")
	os.WriteFile(*bfgPath, []byte(`{"pools":[{"url":"stratum+tcp://pool.example:3333"}]}`), 0o644)
	uptimePath = filepath.Join(dir, "uptime")
	os.WriteFile(uptimePath, []byte("86400.0 1.0"), 0o644)
	t.Cleanup(func() { *minerWeb, *bfgPath, uptimePath = oldWeb, oldBfg, oldUp })
	setCharge("tuner")
	t.Cleanup(func() { setCharge("") })
	noteMinerChange("clock/voltage change")

	get := func() map[string]any {
		w := httptest.NewRecorder()
		handleChips(w, httptest.NewRequest("GET", "/api/chips", nil))
		var m map[string]any
		json.Unmarshal(w.Body.Bytes(), &m)
		return m
	}
	chipsCacheMu.Lock()
	chipsCache = nil
	chipsCacheMu.Unlock()
	m := get()
	if m["ok"] != false || m["busy"] != true || !strings.Contains(m["err"].(string), "the tuner's new setting") || strings.Contains(m["err"].(string), "HTTP") {
		t.Fatalf("no cache: %v", m)
	}
	// with an earlier reading: shown, marked
	chipsCacheMu.Lock()
	chipsCache, chipsCacheAt = map[string]any{"ok": true, "boards": []any{map[string]any{"id": 0}}}, time.Now().Add(-40*time.Second)
	chipsCacheMu.Unlock()
	m = get()
	if m["ok"] != true || m["stale"] == nil || m["stale_s"].(float64) < 39 {
		t.Fatalf("cached: %v", m)
	}
	// a long-quiet backend with no known reason still reads calmly
	changeMu.Lock()
	changeAt = time.Time{}
	changeMu.Unlock()
	setCharge("")
	chipsCacheMu.Lock()
	chipsCache = nil
	chipsCacheMu.Unlock()
	m = get()
	if strings.Contains(m["err"].(string), "HTTP") || m["detail"] == nil {
		t.Fatalf("fallback: %v", m)
	}
}
