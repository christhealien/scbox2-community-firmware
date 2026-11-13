package sshcore

import (
	"fmt"
	"sync"
	"time"
)

// Limiter counts wrong passwords per address (FailKey): FailLimit within FailWindow block the address for
// FailWindow. A sign-in is counted before its password is checked (Attempt), so parallel guesses can't all get
// in before the first wrong one is counted; a right password clears the count (Done). The same rules as the
// panel's sign-in (ssh.go loginAttempt / loginDone).
type Limiter struct {
	mu      sync.Mutex
	by      map[string]*fails
	now     func() time.Time
	OnBlock func(key, msg string) // once per block (a new block after it ends calls it again)
}

type fails struct {
	n        int
	first    time.Time
	until    time.Time
	inflight int // sign-ins being checked right now
}

func NewLimiter() *Limiter { return &Limiter{by: map[string]*fails{}, now: time.Now} }

// SetClock: tests.
func (l *Limiter) SetClock(now func() time.Time) { l.mu.Lock(); l.now = now; l.mu.Unlock() }

// Attempt: may a sign-in from ip be checked now? false: blocked, or FailLimit checks already under way.
func (l *Limiter) Attempt(ip string) bool {
	k := FailKey(ip)
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	f := l.by[k]
	if f != nil && now.Before(f.until) {
		return false // blocked
	}
	if f == nil || (now.Sub(f.first) > FailWindow && f.inflight == 0) {
		f = &fails{first: now}
		l.by[k] = f
	}
	if f.n+f.inflight >= FailLimit {
		return false // that many checks at once: try again in a moment (not counted, no block)
	}
	f.inflight++
	return true
}

// Done: the check Attempt let through has finished; only a wrong password counts.
func (l *Limiter) Done(ip, via string, ok bool) {
	k := FailKey(ip)
	l.mu.Lock()
	defer l.mu.Unlock()
	f := l.by[k]
	if f != nil && f.inflight > 0 {
		f.inflight--
	}
	if ok {
		delete(l.by, k)
		return
	}
	now := l.now()
	if f == nil || now.Sub(f.first) > FailWindow {
		inflight := 0
		if f != nil {
			inflight = f.inflight
		}
		f = &fails{first: now, inflight: inflight}
		l.by[k] = f
	}
	f.n++
	if f.n >= FailLimit {
		f.until = now.Add(FailWindow)
		if f.n == FailLimit && l.OnBlock != nil { // once per block
			msg := fmt.Sprintf("%s blocked for %d min after %d wrong passwords (%s)", k, int(FailWindow.Minutes()), f.n, via)
			go l.OnBlock(k, msg)
		}
	}
	if len(l.by) > 1000 { // bound memory
		for kk, v := range l.by {
			if now.Sub(v.first) > FailWindow && now.After(v.until) && v.inflight == 0 {
				delete(l.by, kk)
			}
		}
	}
}

// Blocked: is ip blocked now?
func (l *Limiter) Blocked(ip string) bool {
	k := FailKey(ip)
	l.mu.Lock()
	defer l.mu.Unlock()
	f := l.by[k]
	return f != nil && l.now().Before(f.until)
}
