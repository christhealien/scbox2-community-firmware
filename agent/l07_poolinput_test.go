package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// L07: pool URL/user/password input hardening.
//
// These tests drive the real handlePools against a fake stock backend (minerd) on this lane's port range,
// capturing exactly what reaches /mcb/newpool. On base, control characters in the URL, user or password sail
// through poolURLErr (which only blocks space/tab/quotes) and the entirely-unchecked credential fields, and
// are forwarded to the backend. With the fix they are refused before any PUT.

var l07Mu sync.Mutex

// l07Backend: a fake stock backend that records PUT bodies. Returns the recorder and a cleanup.
func l07Backend(t *testing.T) (*[]l07Put, func()) {
	t.Helper()
	l07Mu.Lock()
	var puts []l07Put
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/user/login"):
			json.NewEncoder(w).Encode(map[string]any{"JWT Token": "tok"})
		case r.Method == "GET" && r.URL.Path == "/mcb/pools":
			json.NewEncoder(w).Encode([]map[string]any{
				{"url": "stratum+tcp://pool.one:3333", "user": "wallet1.w", "pass": "x"},
			})
		case r.Method == "PUT":
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			puts = append(puts, l07Put{r.URL.Path, string(b)})
			mu.Unlock()
			io.WriteString(w, `{"status":0}`)
		default:
			io.WriteString(w, `{}`)
		}
	}))
	oldWeb := *minerWeb
	*minerWeb = strings.TrimPrefix(srv.URL, "http://")
	jwtMu.Lock()
	jwtToken = "tok"
	jwtMu.Unlock()
	authMu.Lock()
	auth.MinerPass = "pw"
	authMu.Unlock()
	cleanup := func() {
		srv.Close()
		*minerWeb = oldWeb
		l07Mu.Unlock()
	}
	return &puts, cleanup
}

type l07Put struct {
	path string
	body string
}

func l07AddPool(t *testing.T, url, user, pass string) (ok bool, errMsg string) {
	t.Helper()
	t.Cleanup(func() { changeMu.Lock(); changeAt, changeWhat = time.Time{}, ""; changeMu.Unlock() })
	body, _ := json.Marshal(map[string]any{
		"action": "add", "url": url, "user": user, "pass": pass, "confirm": true,
	})
	req := httptest.NewRequest("POST", "/api/pools", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handlePools(rec, req)
	var resp struct {
		OK  bool   `json:"ok"`
		Err string `json:"err"`
	}
	json.Unmarshal(rec.Body.Bytes(), &resp)
	return resp.OK, resp.Err
}

// Each case is a hostile credential/URL that must be refused and must never reach the backend.
func TestL07PoolInputControlChars(t *testing.T) {
	cases := []struct {
		name, url, user, pass string
	}{
		{"url_newline", "stratum+tcp://a.pool.com\nstratum+tcp://evil.com:3333", "w.r", "x"},
		{"url_nul", "stratum+tcp://a.pool.com:3333\x00evil", "w.r", "x"},
		{"url_cr", "stratum+tcp://a.pool\r.com:3333", "w.r", "x"},
		{"user_newline", "stratum+tcp://a.pool.com:3333", "wallet.rig\nsecret", "x"},
		{"user_nul", "stratum+tcp://a.pool.com:3333", "wallet\x00rig", "x"},
		{"pass_newline", "stratum+tcp://a.pool.com:3333", "wallet.rig", "secretpw\ninjected"},
		{"pass_tab", "stratum+tcp://a.pool.com:3333", "wallet.rig", "a\tb"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			puts, cleanup := l07Backend(t)
			defer cleanup()
			ok, _ := l07AddPool(t, c.url, c.user, c.pass)
			if ok {
				t.Errorf("handler accepted hostile input %q/%q/%q (want refusal)", c.url, c.user, c.pass)
			}
			for _, p := range *puts {
				t.Errorf("hostile input reached backend: PUT %s %q", p.path, p.body)
			}
		})
	}
}

// A clean pool with ordinary values must still be accepted and forwarded (no false positives).
func TestL07PoolInputCleanStillWorks(t *testing.T) {
	puts, cleanup := l07Backend(t)
	defer cleanup()
	ok, msg := l07AddPool(t, "stratum+tcp://clean.pool.com:3333", "wallet.rig", "goodpass")
	if !ok {
		t.Fatalf("clean pool refused: %s", msg)
	}
	found := false
	for _, p := range *puts {
		if p.path == "/mcb/newpool" {
			found = true
		}
	}
	if !found {
		t.Errorf("clean pool did not reach backend; puts=%v", *puts)
	}
}
