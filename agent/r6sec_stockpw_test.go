package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// r6-security: a failed sign-in to the OEM's backend must not carry the stock web password into an error
// text (the tuner log, the activity feed, the page). The password goes in the login URL's query, encrypted
// with a fixed, public key (encStockPw), i.e. readable by anyone; Go's *url.Error prints that whole URL.
func TestR6StockLoginErrorHasNoPassword(t *testing.T) {
	// a backend that wants a sign-in (401), then answers the sign-in with garbage (not EOF/refused/timeout)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/user/login") {
			c, buf, _ := w.(http.Hijacker).Hijack()
			buf.WriteString("GARBAGE NOT HTTP\r\n\r\n")
			buf.Flush()
			c.Close()
			return
		}
		w.WriteHeader(401)
	}))
	defer srv.Close()

	old := *minerWeb
	*minerWeb = srv.Listener.Addr().String()
	defer func() { *minerWeb = old }()
	authMu.Lock()
	oldPw := auth.MinerPass
	auth.MinerPass = "S3cretStockPw"
	authMu.Unlock()
	defer func() { authMu.Lock(); auth.MinerPass = oldPw; authMu.Unlock() }()

	_, err := stockReq("GET", "/mcb/pools", nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	enc := encStockPw("S3cretStockPw")
	t.Logf("error: %v", err)
	if m := err.Error(); strings.Contains(m, enc) || strings.Contains(m, "password=") {
		t.Errorf("the backend error carries the (trivially decryptable) stock password: %s", m)
	}
}
