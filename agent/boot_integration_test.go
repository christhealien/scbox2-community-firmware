//go:build integration

package main

// Runs the real polling loop against a test miner (the demo mock on 127.0.0.1:14028), through a relay that
// can be cut to make the miner go quiet, with the system uptime faked to a fresh boot.
//   go test -tags integration -run Alerts .

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// relay forwards a local port to the test miner; cut() makes the miner unreachable, mend() brings it back.
type relay struct {
	addr string
	mu   sync.Mutex
	ln   net.Listener
}

func newRelay(t *testing.T) *relay {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r := &relay{addr: l.Addr().String()}
	r.serve(l)
	return r
}

func (r *relay) serve(l net.Listener) {
	r.mu.Lock()
	r.ln = l
	r.mu.Unlock()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				m, err := net.Dial("tcp", "127.0.0.1:14028")
				if err != nil {
					return
				}
				defer m.Close()
				go io.Copy(m, c)
				io.Copy(c, m)
			}()
		}
	}()
}
func (r *relay) cut() { r.mu.Lock(); r.ln.Close(); r.mu.Unlock() }
func (r *relay) mend(t *testing.T) {
	l, err := net.Listen("tcp", r.addr)
	if err != nil {
		t.Fatal(err)
	}
	r.serve(l)
}

func countWith(msgs []string, s string) int {
	n := 0
	for _, m := range msgs {
		if strings.Contains(m, s) {
			n++
		}
	}
	return n
}

func TestAlertsEndToEnd(t *testing.T) {
	fd := &fakeDiscord{}
	srv := httptest.NewServer(http.HandlerFunc(fd.handler))
	defer srv.Close()
	setupNotify(t, srv.URL, 30)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "uptime"), "95.0 10.0\n")
	uptimePath = filepath.Join(dir, "uptime")
	bootMarkPath = filepath.Join(dir, "boot-alerted")
	stopGrace = 1500 * time.Millisecond
	rl := newRelay(t)
	*apiAddr = rl.addr
	*pollEvery = 200 * time.Millisecond
	go pollActivity()

	// 1. fresh boot: one reboot alert
	fd.waitN(t, 1)
	time.Sleep(time.Second)

	// 2. a short gap (shorter than the grace): nothing
	rl.cut()
	time.Sleep(700 * time.Millisecond)
	rl.mend(t)
	time.Sleep(time.Second)
	msgs := fd.waitN(t, 1)
	if n := countWith(msgs, "stopped answering"); n != 0 {
		t.Fatalf("alert for a short gap: %q", msgs)
	}

	// 3. a long gap: "stopped", then a positive "answering again"
	rl.cut()
	time.Sleep(2500 * time.Millisecond)
	rl.mend(t)
	time.Sleep(1500 * time.Millisecond)
	msgs = fd.waitN(t, 1)
	stop, back := countWith(msgs, "🛑 the miner stopped answering"), countWith(msgs, "🟢 the miner is answering again")
	if stop != 1 || back != 1 {
		t.Fatalf("long gap: stopped=%d back=%d in %q", stop, back, msgs)
	}

	// 4. the agent restarting during the same boot doesn't report the boot again
	if bootAlertOnce() {
		t.Fatal("second reboot alert allowed in the same boot")
	}
	if n := countWith(msgs, "rebooted"); n != 1 {
		t.Fatalf("reboot alerts: %d", n)
	}
	for _, m := range msgs {
		t.Logf("Discord got: %s", m)
	}
}
