package main

// The SC Box II: one Live reading shared while a clock/voltage write is being confirmed (v0.5.4c).
//
// v0.5.3 froze about 3 minutes into tuner runs, and heavy miner-API traffic while the driver ramps a new clock is the
// leading suspect. During a confirmation (box2ConfirmCoop: its quiet first 30 s and the devs polls after it) every
// Dashboard reader shared the miner API: the tuner's temperature check and fan hold (each tick), the fan loop (every
// 10 s), every open page (every 8 s), the event watcher: about 39 commands a minute with two tabs open. While a
// confirmation runs, buildDashboard instead answers from one shared reading:
//   - a reading up to dashShareAge (15 s) old is reused; an older one is refreshed by one caller at a time, the others
//     take the cached copy while it is at most dashShareForce (20 s) old, else wait for that refresh;
//   - the refresh asks devs only (the temperatures, clocks and hashrate), the whole reading (summary too) at most
//     every dashShareFull, so with any number of tabs open it is about 4 commands a minute;
//   - the confirmation's own devs poll (box2ReadBack) counts as a refresh: its boards go into the shared reading, so
//     after the quiet time nothing else asks the miner API at all;
//   - the tuner's heat abort reads with dashShareAge as its limit for a refresh in flight too: its temperature is
//     never older than 15 s.
//
// Outside a confirmation, and on the SC Lite, every reader reads afresh as before (on the SC Box II each fresh
// reading is kept too, so a confirmation starts from it).

import (
	"sync"
	"sync/atomic"
	"time"
)

var (
	dashShareAge   = 15 * time.Second // reuse a reading up to this old while a confirmation runs
	dashShareForce = 20 * time.Second // older than this: wait for the refresh in flight
	dashShareFull  = 60 * time.Second // the whole reading (summary: shares, uptime) at least this often
)

// box2Confirming: clock/voltage confirmations running now (box2ConfirmCoop).
var box2Confirming atomic.Int32

// dashShareOff: the tests' switch back to the behaviour before v0.5.4c (every reader reads afresh), to compare.
var dashShareOff atomic.Bool

// dashShareOn: the shared reading is used now (the SC Box II, a confirmation running).
func dashShareOn() bool {
	return model.Plan == planBox2V && box2Confirming.Load() > 0 && !dashShareOff.Load()
}

var dashShare struct {
	mu     sync.Mutex
	d      dashboard
	have   bool
	at     time.Time     // when the boards' reading (devs) was asked
	fullAt time.Time     // when the whole reading (summary and devs) was asked
	busy   chan struct{} // a refresh in flight (closed when it is stored)
}

// dashCopy: a copy a caller may change (its boards' slices its own).
func dashCopy(d dashboard) dashboard {
	if d.Boards != nil {
		bs := make([]board, len(d.Boards))
		for i, b := range d.Boards {
			b.Temps = append([]float64(nil), b.Temps...)
			b.Fans = append([]int(nil), b.Fans...)
			bs[i] = b
		}
		d.Boards = bs
	}
	return d
}

// dashShareStore: a reading asked at at (full: summary and devs), kept as the shared one unless a newer one is.
// Caller holds dashShare.mu.
func dashShareStore(d dashboard, at time.Time, full bool) {
	if dashShare.have && at.Before(dashShare.at) {
		return
	}
	dashShare.d, dashShare.have, dashShare.at = dashCopy(d), true, at
	if full {
		dashShare.fullAt = at
	}
}

// dashShareRead: the Live reading on the SC Box II. No confirmation running: a fresh one (kept). Else the shared
// one: reused up to maxAge old; a refresh in flight is not waited for while the reading is at most waitAge old.
func dashShareRead(maxAge, waitAge time.Duration) dashboard {
	if !dashShareOn() {
		at := time.Now()
		d := buildDashboardNow()
		dashShare.mu.Lock()
		dashShareStore(d, at, true)
		dashShare.mu.Unlock()
		return d
	}
	s := &dashShare
	for {
		s.mu.Lock()
		age := time.Since(s.at)
		full := !s.have || !s.d.OK || time.Since(s.fullAt) > dashShareFull
		if s.have && age <= maxAge && !full {
			d := dashCopy(s.d)
			s.mu.Unlock()
			return d
		}
		if s.busy != nil {
			if s.have && age <= waitAge && s.d.OK {
				d := dashCopy(s.d)
				s.mu.Unlock()
				return d
			}
			ch := s.busy
			s.mu.Unlock()
			<-ch
			continue // the refresh is stored: read it
		}
		ch := make(chan struct{})
		s.busy = ch
		base := dashCopy(s.d)
		s.mu.Unlock()
		at := time.Now()
		var d dashboard
		ok := false
		if !full {
			d, ok = dashBoardsRefresh(base)
		}
		if !ok {
			d, full = buildDashboardNow(), true
		}
		s.mu.Lock()
		dashShareStore(d, at, full)
		s.busy = nil
		close(ch)
		s.mu.Unlock()
		return d
	}
}

// dashBoardsRefresh: the shared reading with its boards (temperatures, clocks, hashrate) asked again (devs only).
// ok=false: devs didn't answer (the caller reads the whole dashboard, which says why).
func dashBoardsRefresh(base dashboard) (dashboard, bool) {
	raw, err := cgminerCmd("devs")
	if err != nil {
		return base, false
	}
	return dashWithBoards(base, raw)
}

// dashWithBoards: base with the boards of a devs reply. ok=false: no boards in it.
func dashWithBoards(base dashboard, raw []byte) (dashboard, bool) {
	bs, sum, ok := dashBoardsOf(raw)
	if !ok || len(bs) == 0 {
		return base, false
	}
	base.Boards = bs
	if sum > 0 {
		base.GHS = sum
	}
	dashDecorate(&base)
	return base, true
}

// dashShareNoteDevs: a confirmation's devs poll (asked at at), a refresh of the shared reading's boards.
func dashShareNoteDevs(raw []byte, at time.Time) {
	if model.Plan != planBox2V {
		return
	}
	dashShare.mu.Lock()
	if !dashShare.have || !dashShare.d.OK || at.Before(dashShare.at) {
		dashShare.mu.Unlock()
		return
	}
	base := dashCopy(dashShare.d)
	dashShare.mu.Unlock()
	if d, ok := dashWithBoards(base, raw); ok {
		dashShare.mu.Lock()
		dashShareStore(d, at, false) // (unless a newer reading was stored meanwhile)
		dashShare.mu.Unlock()
	}
}

// dashShareReset: forget the shared reading (tests).
func dashShareReset() {
	dashShare.mu.Lock()
	dashShare.d, dashShare.have, dashShare.at, dashShare.fullAt = dashboard{}, false, time.Time{}, time.Time{}
	dashShare.mu.Unlock()
}
