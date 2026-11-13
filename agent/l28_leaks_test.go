package main

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// syslogd (BusyBox, -s 200 KB by default) rotates /var/log/messages to messages.0 and starts a new file. An open
// Log tab (System log) keeps tailing the renamed file: no new line ever shows, until the page reconnects; after
// the next rotation the old file is deleted but still held open (tmpfs: RAM) for as long as the stream stays.
func TestL28LogStreamFollowsRotation(t *testing.T) {
	dir := t.TempDir()
	lp := filepath.Join(dir, "messages")
	os.WriteFile(lp, []byte("old line 1\n"), 0o644)
	old := *logPath
	*logPath = lp
	defer func() { *logPath = old }()
	srv := httptest.NewServer(http.HandlerFunc(handleLog))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	lines := make(chan string, 64)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if s := strings.TrimPrefix(sc.Text(), "data: "); s != sc.Text() {
				lines <- s
			}
		}
		close(lines)
	}()
	wait := func(want string) bool {
		tm := time.After(3 * time.Second)
		for {
			select {
			case l, ok := <-lines:
				if !ok {
					return false
				}
				if l == want {
					return true
				}
			case <-tm:
				return false
			}
		}
	}
	if !wait("old line 1") {
		t.Fatal("the stream didn't show the log's existing line")
	}
	// syslogd's rotation: rename, then a new file at the same path
	time.Sleep(400 * time.Millisecond)
	os.Rename(lp, lp+".0")
	os.WriteFile(lp, []byte("after rotation\n"), 0o644)
	if !wait("after rotation") {
		t.Fatal("after the system log rotated (messages -> messages.0), the Log tab's stream never showed a new line: it still tails the renamed file")
	}
}

// The pool guard's list of incidents is "at most 10", but the redirect branch appends without the cap: a pool
// that keeps redirecting while the Idle write fails adds one every 30 s, for as long as it goes on.
func TestL28PoolGuardRedirectIncidentsBounded(t *testing.T) {
	oldList, oldLive, oldIdle, oldRm, oldIG := pgListPools, pgLivePools, pgIdle, pgRemove, minerIdleForGuard
	oldAuth := *authFile
	*authFile = filepath.Join(t.TempDir(), "auth.json")
	defer func() {
		poolHoldRelease()
		*authFile = oldAuth
		pgListPools, pgLivePools, pgIdle, pgRemove, minerIdleForGuard = oldList, oldLive, oldIdle, oldRm, oldIG
		pgStrikes, pgActed, pgIncidents = map[string]int{}, map[string]int{}, nil
	}()
	pgListPools = func() ([]map[string]any, error) {
		return []map[string]any{{"url": "stratum+tcp://mine.example:3333", "user": "w.1"}}, nil
	}
	pgLivePools = func() ([]poolView, error) {
		return []poolView{{Index: 0, URL: "stratum+tcp://mine.example:3333", StratumURL: "stratum+tcp://evil.example:3333", User: "w.1", Active: true}}, nil
	}
	pgIdle = func() error { return errTest("backend down") }
	pgRemove = func(int) error { return nil }
	pgRestartMinerSave := pgRestartMiner
	pgRestartMiner = func() error { return nil }
	defer func() { pgRestartMiner = pgRestartMinerSave }()
	minerIdleForGuard = func() bool { return false }
	pgStrikes, pgActed, pgIncidents = map[string]int{}, map[string]int{}, nil
	for i := 0; i < 200; i++ { // 100 minutes at the guard's 30 s
		poolGuardCheck()
	}
	pgMu.Lock()
	n := len(pgIncidents)
	pgMu.Unlock()
	if n > 10 {
		t.Fatalf("pool guard incidents grew to %d (the list is meant to hold at most 10)", n)
	}
}

type errTest string

func (e errTest) Error() string { return string(e) }
