package main

// Per-board health from the mining log (minersyslog). intminer's driver prints, per board C<n> (the
// per-model init tag — "SCLITE" on the SC Lite, "SCBOX2" on the SC Box II — is model.InitLogTag):
//   C1: SCLITE Init sucessed. 46 chips, 3680 Total goodcores.          (SC Lite start-up: chips answered)
//   C1: SCBOX2 Init sucessed. 36 chips, 2880 Total goodcores. Wait 5s!!! (SC Box II start-up; 1 board)
//   C1: Auto addressing failed / Init failed 5 Times, Try reboot later!!!
//   C1: !!!SEND JOB FAILD 10 TIMES, REINIT THIS CPB!!!  /  C1: Read Nonce Faild 10 Times, Reinit Device!!!
//   C1: impossible thing happened!!       (a result for a job the board isn't scanning: chips out of sync;
//                                          dropped, NOT counted as a hardware error — traced at 0x1c9410)
//   C1: Chip Avgtemp 61.0'C, MaxTemp 66.0'C, CpbTemp 58.9'C            (hottest chip; Box II has no CpbTemp)
// None of these reach the API, and the log scrolls away within minutes, so the agent keeps the
// counts per board for the Dashboard. Init has no minimum chip count (a board can come up with fewer
// than its full complement, or 0, which crashes the mining program at its first division by the count),
// so a short count is worth showing. The chip count and the init tag come from the model profile
// (setGeometryFromModel) so the SC Lite keeps its 46/SCLITE behaviour byte-for-byte.

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// expectedChips is the full per-board chip complement shown on the Dashboard and used as the chip-trim
// input ceiling (chiptrim.go). Seeded from model.ChipsPerBoard by setGeometryFromModel at start-up; the
// 46 default is the SC Lite value, so unseeded code and the tests keep SC Lite behaviour unchanged.
var expectedChips = 46

var (
	// bhInitRe matches the driver init tag. Its default (defaultBHInitRe) accepts both known models' tags
	// (SCLITE / SCBOX2); setGeometryFromModel narrows it to the active model's own tag (model.InitLogTag).
	// The both-tags default keeps parsing working before the profile is loaded and in tests that don't seed
	// geometry.
	bhInitRe   = defaultBHInitRe()
	bhAddrRe   = regexp.MustCompile(`C(\d{1,2}): Auto addressing failed`)
	bhInitFail = regexp.MustCompile(`C(\d{1,2}): Init failed \d+ Times`)
	bhReinitRe = regexp.MustCompile(`C(\d{1,2}): (?:!!!SEND JOB FAILD 10 TIMES|Read Nonce(?: \d+)? +Faild 10 Times)`)
	bhImpossRe = regexp.MustCompile(`C(\d{1,2}): impossible thing happened`)
	bhNoCPBRe  = regexp.MustCompile(`C(\d{1,2}): No CPB Detected`)
	bhTempRe   = regexp.MustCompile(`C(\d{1,2}): Chip Avgtemp ([-\d.]+)'C, MaxTemp ([-\d.]+)'C`)
	bhTimeRe   = regexp.MustCompile(`^\s*\[(\d{4}-\d\d-\d\d \d\d:\d\d:\d\d)\]`)
)

// defaultBHInitRe matches either known model's driver init tag; used before the profile is loaded and as
// the fallback for the unknown profile.
func defaultBHInitRe() *regexp.Regexp {
	return regexp.MustCompile(`C(\d{1,2}): (?:SCLITE|SCBOX2) Init sucessed\. (\d+) chips`)
}

// setGeometryFromModel seeds the board/chip geometry from the active model profile. Called once from
// main() just after setWindowsFromModel (and from tests that exercise a non-default model). It sets the
// per-board chip complement and narrows the init-log regex to the model's own driver tag. For the SC Lite
// profile (ChipsPerBoard 46, InitLogTag "SCLITE") this reproduces the old compile-time values exactly, so
// SC Lite behaviour is unchanged.
func setGeometryFromModel() {
	if model.ChipsPerBoard > 0 {
		expectedChips = model.ChipsPerBoard
	}
	tag := model.InitLogTag
	if tag == "" {
		// unknown profile: keep accepting either known tag rather than nothing
		bhInitRe = defaultBHInitRe()
		return
	}
	bhInitRe = regexp.MustCompile(`C(\d{1,2}): ` + regexp.QuoteMeta(tag) + ` Init sucessed\. (\d+) chips`)
}

type bhEvent struct {
	t    int64
	kind byte // 'a' addressing failed, 'f' init failed, 'r' reinit, 'i' impossible
}

type bhSeenRec struct {
	n int
	t int64
}

type bhBoard struct {
	chips   int  // at the last start-up (-1 = not seen)
	absent  bool // "No CPB Detected": nothing on that connector (an SC Lite with fewer boards fitted)
	chipsAt int64
	maxTemp float64
	tempAt  int64
	events  []bhEvent
}

var (
	bhMu     sync.Mutex
	bhBoards = map[int]*bhBoard{}
	bhSeen   = map[string]bhSeenRec{} // event lines already counted (log line → how many times, when)
)

type boardHealth struct {
	ID          int     `json:"id"`
	Chips       int     `json:"chips"` // -1 = no start-up line seen since the agent started
	ChipsAt     int64   `json:"chips_at,omitempty"`
	Expected    int     `json:"expected"`
	AddrFail24  int     `json:"addr_fail_24h"`
	InitFail24  int     `json:"init_fail_24h"`
	Reinit1h    int     `json:"reinit_1h"`
	Reinit24    int     `json:"reinit_24h"`
	Dropped1h   int     `json:"dropped_1h"`
	Dropped24   int     `json:"dropped_24h"`
	MaxTemp     float64 `json:"max_temp,omitempty"`
	MaxTempAt   int64   `json:"max_temp_at,omitempty"`
	LastProblem int64   `json:"last_problem,omitempty"`
	Absent      bool    `json:"absent,omitempty"` // not fitted (the miner found no board there at start-up)
}

func bhGet(id int) *bhBoard {
	b := bhBoards[id]
	if b == nil {
		b = &bhBoard{chips: -1}
		bhBoards[id] = b
	}
	return b
}

// lineTime is the log line's own time (local), or now when it has none / the clock isn't set.
func lineTime(l string, now int64) int64 {
	if m := bhTimeRe.FindStringSubmatch(l); m != nil {
		if t, err := time.ParseInLocation("2006-01-02 15:04:05", m[1], time.Local); err == nil && t.Year() >= 2024 {
			if u := t.Unix(); u <= now+60 {
				return u
			}
		}
	}
	return now
}

func scanBoardHealth(lines []string) {
	now := time.Now().Unix()
	bhMu.Lock()
	defer bhMu.Unlock()
	// The same stretch of log can be read twice (it is re-read whole when its last-seen line has scrolled
	// away), and identical lines are real repeats (several "impossible" in one second). So per line text,
	// count how often it occurs in this read and add only the occurrences beyond the most seen before.
	inBatch := map[string]int{}
	add := func(id int, t int64, k byte, line string) {
		key := strings.TrimSpace(line)
		inBatch[key]++
		if inBatch[key] <= bhSeen[key].n {
			return
		}
		bhSeen[key] = bhSeenRec{n: inBatch[key], t: t}
		b := bhGet(id)
		b.events = append(b.events, bhEvent{t, k})
	}
	for _, l := range lines {
		if !strings.Contains(l, "C") {
			continue
		}
		if m := bhImpossRe.FindStringSubmatch(l); m != nil {
			id, _ := strconv.Atoi(m[1])
			add(id, lineTime(l, now), 'i', l)
			continue
		}
		if m := bhTempRe.FindStringSubmatch(l); m != nil {
			id, _ := strconv.Atoi(m[1])
			if v, err := strconv.ParseFloat(m[3], 64); err == nil && v > -40 && v < 200 {
				b := bhGet(id)
				b.maxTemp, b.tempAt = v, lineTime(l, now)
			}
			continue
		}
		if m := bhNoCPBRe.FindStringSubmatch(l); m != nil { // no board on that connector (or nothing answers there)
			id, _ := strconv.Atoi(m[1])
			b := bhGet(id)
			b.absent, b.chipsAt = true, lineTime(l, now)
			continue
		}
		if m := bhInitRe.FindStringSubmatch(l); m != nil {
			id, _ := strconv.Atoi(m[1])
			n, _ := strconv.Atoi(m[2])
			b := bhGet(id)
			b.chips, b.chipsAt, b.absent = n, lineTime(l, now), false
			continue
		}
		if m := bhReinitRe.FindStringSubmatch(l); m != nil {
			id, _ := strconv.Atoi(m[1])
			add(id, lineTime(l, now), 'r', l)
			continue
		}
		if m := bhAddrRe.FindStringSubmatch(l); m != nil {
			id, _ := strconv.Atoi(m[1])
			add(id, lineTime(l, now), 'a', l)
			continue
		}
		if m := bhInitFail.FindStringSubmatch(l); m != nil {
			id, _ := strconv.Atoi(m[1])
			add(id, lineTime(l, now), 'f', l)
		}
	}
	cut := now - 24*3600
	// the log holds a few hundred lines: minutes when busy, hours when quiet (then the map stays small)
	seenCut := now - 6*3600
	for k, r := range bhSeen {
		if r.t < seenCut {
			delete(bhSeen, k)
		}
	}
	if len(bhSeen) > 20000 { // a board spamming lines: start over rather than grow
		bhSeen = map[string]bhSeenRec{}
	}
	for _, b := range bhBoards {
		i := 0
		for i < len(b.events) && b.events[i].t < cut {
			i++
		}
		b.events = b.events[i:]
		if len(b.events) > 5000 { // a board stuck dropping results all day: keep the recent ones
			b.events = b.events[len(b.events)-5000:]
		}
	}
}

func boardHealthView() []boardHealth {
	now := time.Now().Unix()
	bhMu.Lock()
	defer bhMu.Unlock()
	var out []boardHealth
	for id, b := range bhBoards {
		h := boardHealth{ID: id, Chips: b.chips, ChipsAt: b.chipsAt, Expected: expectedChips, Absent: b.absent}
		if b.absent {
			h.Chips = -1
		}
		if b.tempAt > 0 && now-b.tempAt < 600 {
			h.MaxTemp, h.MaxTempAt = b.maxTemp, b.tempAt
		}
		for _, e := range b.events {
			recent := now-e.t <= 3600
			switch e.kind {
			case 'a':
				h.AddrFail24++
			case 'f':
				h.InitFail24++
			case 'r':
				h.Reinit24++
				if recent {
					h.Reinit1h++
				}
			case 'i':
				h.Dropped24++
				if recent {
					h.Dropped1h++
				}
			}
			if e.t > h.LastProblem {
				h.LastProblem = e.t
			}
		}
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
