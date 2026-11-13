package main

// More notifications (v1.5.3): hash board not hashing, fans, pool failover, no pool, firmware update
// results, new firmware, blocked sign-ins, the daily summary and schedule switches. The thermal guard's
// alert is sent from the fan loop. Each watcher sends once per incident and once when it's over, so a
// problem that lasts doesn't repeat every 30 minutes; fans repeat at most every 6 hours.

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	nwMu  sync.Mutex
	nwNow = time.Now
)

// ---- hash boards ----

type boardWatchState struct {
	seen     map[int]bool // boards that have hashed on this miner (saved: a board that never starts is still known)
	since    map[int]time.Time
	alerted  map[int]bool // told it isn't hashing (a "hashing again" is owed)
	thisRun  map[int]bool // told during this mining session (a new session may tell again)
	elapsed  int          // the engine's running time at the last check (a smaller one: mining restarted)
	loadedOK bool
}

var bw = &boardWatchState{seen: map[int]bool{}, since: map[int]time.Time{}, alerted: map[int]bool{}, thisRun: map[int]bool{}}

func boardsSeenPath() string { return filepath.Join(filepath.Dir(*authFile), "boards-seen.json") }

func (s *boardWatchState) load() {
	if s.loadedOK {
		return
	}
	s.loadedOK = true
	var ids []int
	if b, err := os.ReadFile(boardsSeenPath()); err == nil && json.Unmarshal(b, &ids) == nil {
		for _, id := range ids {
			s.seen[id] = true
		}
	}
}

func (s *boardWatchState) save() {
	var ids []int
	for id := range s.seen {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	b, _ := json.Marshal(ids)
	cfgWrite(boardsSeenPath(), b, 0o644)
}

// boardWatch: a board that has hashed on this miner before, missing or hashing under 5 % of the others,
// 5 minutes or more into mining, for 3 minutes: once per board per mining session, and again when it's back.
func boardWatch(d dashboard, charge string) {
	nwMu.Lock()
	defer nwMu.Unlock()
	s := bw
	s.load()
	now := nwNow()
	if s.thisRun == nil {
		s.thisRun = map[int]bool{}
	}
	if d.Elapsed < s.elapsed { // mining restarted: a new session (a board still down may be told again)
		s.since, s.thisRun = map[int]time.Time{}, map[int]bool{}
	}
	s.elapsed = d.Elapsed
	ghs := map[int]float64{}
	var sum float64
	var n int
	for _, b := range d.Boards {
		if b.Alive && b.GHS > 0 {
			ghs[b.ID] = b.GHS
			sum += b.GHS
			n++
			if !s.seen[b.ID] {
				s.seen[b.ID] = true
				s.save()
			}
		}
	}
	// a board that was reported down and hashes again: say so at once (even right after a restart)
	for id := range s.alerted {
		if ghs[id] > 0 && (n == 1 || ghs[id] >= 0.05*(sum-ghs[id])/float64(n-1)) {
			notifyForce(evBoard, fmt.Sprintf("board %d is hashing again (%.0f GH/s)", id, ghs[id]))
			activity.emit(fmt.Sprintf("🧩 board %d is hashing again", id))
			delete(s.alerted, id)
			delete(s.since, id)
		}
	}
	if charge == "idle" || charge == "tuner" || d.Elapsed < 300 || box2SoftIdled() {
		s.since = map[int]time.Time{}
		return
	}
	ids := make([]int, 0, len(s.seen))
	for id := range s.seen {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	for _, id := range ids {
		others := 0.0
		if n-boolInt(ghs[id] > 0) > 0 {
			others = (sum - ghs[id]) / float64(n-boolInt(ghs[id] > 0))
		}
		down := ghs[id] <= 0 || (others > 0 && ghs[id] < 0.05*others)
		if !down {
			if s.alerted[id] {
				notifyForce(evBoard, fmt.Sprintf("board %d is hashing again (%.0f GH/s)", id, ghs[id]))
				activity.emit(fmt.Sprintf("🧩 board %d is hashing again", id))
			}
			delete(s.since, id)
			delete(s.alerted, id)
			delete(s.thisRun, id)
			continue
		}
		if _, ok := s.since[id]; !ok {
			s.since[id] = now
		}
		if !s.thisRun[id] && now.Sub(s.since[id]) >= 3*time.Minute {
			s.alerted[id], s.thisRun[id] = true, true
			msg := fmt.Sprintf("board %d isn't hashing (it has before on this miner). Settings → Restart mining often brings it back; if it keeps happening, especially after cold starts, reseat its ribbon cable", id)
			snap := fmt.Sprintf("board %d stopped hashing", id)
			if why := boardAutoRestart(now, snap); why == "" {
				msg = fmt.Sprintf("board %d isn't hashing (it has before on this miner): mining restarted by itself to bring it back (the miner reboots; at most once an hour). If it keeps happening, reseat its ribbon cable", id)
			} else if why != "off" {
				msg += " (not restarted automatically: " + why + ")"
			}
			activity.emit("🧩 " + msg)
			notifyForce(evBoard, msg)
			if !boardAutoAt.Equal(now) { // (a restart takes its snapshot first)
				incidentWG.Add(1)
				go func() { defer incidentWG.Done(); incidentSnapshot(snap) }()
			}
		}
	}
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ---- fans ----

type fanWatchState struct {
	seen    map[int]bool
	since   map[int]time.Time
	alerted map[int]bool
	last    map[int]time.Time
}

var fw = &fanWatchState{seen: map[int]bool{}, since: map[int]time.Time{}, alerted: map[int]bool{}, last: map[int]time.Time{}}

// fanWatch: a fan that has been seen spinning reads 0, or under 40 % of the other fans, while mining, for 2
// minutes: once, then at most every 6 hours while it lasts, and once when it's running again.
func fanWatch(d dashboard, charge string) {
	nwMu.Lock()
	defer nwMu.Unlock()
	var rpm []int
	for _, b := range d.Boards {
		if b.Alive && len(b.Fans) > 0 {
			rpm = b.Fans
			break
		}
	}
	if len(rpm) == 0 {
		return
	}
	now := nwNow()
	for i, v := range rpm {
		if v >= 300 {
			fw.seen[i] = true
		}
	}
	if charge == "idle" || !d.OK {
		fw.since = map[int]time.Time{}
		return
	}
	for i, v := range rpm {
		if !fw.seen[i] {
			continue
		}
		var others []int
		for j, o := range rpm {
			if j != i && fw.seen[j] {
				others = append(others, o)
			}
		}
		med := 0
		if len(others) > 0 {
			sort.Ints(others)
			med = others[len(others)/2]
		}
		bad := v == 0 || (med > 0 && float64(v) < 0.4*float64(med))
		if !bad {
			if fw.alerted[i] {
				notifyForce(evFan, fmt.Sprintf("fan %d is running again (%d rpm)", i+1, v))
				activity.emit(fmt.Sprintf("🌀 fan %d is running again", i+1))
			}
			delete(fw.since, i)
			delete(fw.alerted, i)
			continue
		}
		if _, ok := fw.since[i]; !ok {
			fw.since[i] = now
		}
		if now.Sub(fw.since[i]) >= 2*time.Minute && (!fw.alerted[i] || now.Sub(fw.last[i]) >= 6*time.Hour) {
			fw.alerted[i], fw.last[i] = true, now
			what := "stopped (0 rpm)"
			if v > 0 {
				what = fmt.Sprintf("is too slow: %d rpm, the others about %d", v, med)
			}
			msg := fmt.Sprintf("fan %d %s. Check it isn't blocked or unplugged; the boards heat up fast without it", i+1, what)
			activity.emit("🌀 " + msg)
			notifyForce(evFan, msg)
		}
	}
}

// ---- pools ----

type poolWatchState struct {
	active      int // pool index the miner uses (-1: none)
	activeSince time.Time
	switched    bool // a "backup pool" alert was sent
	deadSince   time.Time
	deadAlerted bool
}

var pw = &poolWatchState{active: -2}

// poolWatch: the miner moved off its primary pool (stable for 30 s): once per switch, and once when it's
// back on the primary. No pool reachable for 3 minutes: once, and once when one is again.
func poolWatch(pools []poolView, charge string) {
	nwMu.Lock()
	defer nwMu.Unlock()
	now := nwNow()
	if charge == "idle" || len(pools) == 0 {
		pw.deadSince = time.Time{}
		return
	}
	primary, active, alive := pools[0], -1, 0
	for _, p := range pools {
		if p.Priority < primary.Priority {
			primary = p
		}
		if p.Active {
			active = p.Index
		}
		if strings.EqualFold(p.Status, "Alive") {
			alive++
		}
	}
	// nothing reachable
	if alive == 0 {
		if pw.deadSince.IsZero() {
			pw.deadSince = now
		}
		if !pw.deadAlerted && now.Sub(pw.deadSince) >= 3*time.Minute {
			pw.deadAlerted = true
			msg := "no pool is reachable, so the miner isn't mining: check the network and the pool addresses (Pools tab)"
			activity.emit("🔌 " + msg)
			notifyForce(evNoPool, msg)
		}
		return
	}
	if pw.deadAlerted {
		notifyForce(evNoPool, "a pool is reachable again: mining")
		activity.emit("🔌 a pool is reachable again")
	}
	pw.deadSince, pw.deadAlerted = time.Time{}, false
	// failover
	if active != pw.active {
		pw.active, pw.activeSince = active, now
		return
	}
	if active < 0 || now.Sub(pw.activeSince) < 30*time.Second {
		return
	}
	if active != primary.Index && !pw.switched {
		pw.switched = true
		var url string
		for _, p := range pools {
			if p.Index == active {
				url = p.URL
			}
		}
		msg := fmt.Sprintf("switched to a backup pool: %s (the primary, %s, is %s)", url, primary.URL, strings.ToLower(primary.Status))
		activity.emit("🔀 " + msg)
		notifyForce(evPoolSw, msg)
	} else if active == primary.Index && pw.switched {
		pw.switched = false
		msg := "back on the primary pool: " + primary.URL
		activity.emit("🔀 " + msg)
		notifyForce(evPoolSw, msg)
	}
}

// ---- firmware updates ----

func updNotifiedPath() string { return filepath.Join(updDir(), "notified") }

// updateResultNotify: sends the last update's result once (an install that confirmed itself, a roll back,
// or a failed install), a couple of minutes after the start, when the network and clock are up.
var updNotifyDelay = 2 * time.Minute

func updateResultNotify() {
	time.Sleep(updNotifyDelay)
	b, err := os.ReadFile(filepath.Join(updDir(), "last.json"))
	if err != nil {
		return
	}
	var l struct {
		Result, From, To, Note string
		At                     int64
	}
	if json.Unmarshal(b, &l) != nil || l.At == 0 {
		return
	}
	if nb, err := os.ReadFile(updNotifiedPath()); err == nil {
		if v, _ := strconv.ParseInt(strings.TrimSpace(string(nb)), 10, 64); v >= l.At {
			return
		}
	}
	var msg string
	switch l.Result {
	case "installed":
		msg = "firmware updated to v" + l.To
		if l.From != "" {
			msg += " (from v" + l.From + ")"
		}
	case "rolled back":
		msg = "firmware rolled back"
		if l.From != "" {
			msg += " from v" + l.From
		}
		if l.To != "" {
			msg += " to v" + l.To
		}
	case "failed":
		msg = "a firmware update to v" + l.To + " failed"
	default:
		return
	}
	if l.Note != "" && l.Result != "installed" {
		msg += ": " + l.Note
	}
	notifyForce(evUpdate, msg)
	cfgWrite(updNotifiedPath(), []byte(strconv.FormatInt(l.At, 10)+"\n"), 0o644)
}

// newFirmwareLoop: once a day (the first check 10 minutes after the start), only while the notification is
// switched on: a release newer than this firmware on the update source, told once per release.
func newFirmwareLoop() {
	time.Sleep(10 * time.Minute)
	for {
		notifyMu.Lock()
		on := notifyCfg.Events[evNewFW] && notifyCfg.anyOn()
		notifyMu.Unlock()
		if on {
			if rel, err := latestRelease(updateRepo()); err == nil {
				cur := firmwareVersion()
				p := filepath.Join(updDir(), "notified-release")
				told, _ := os.ReadFile(p)
				if cur != "" && versionLess(cur, rel.Tag) && strings.TrimSpace(string(told)) != rel.Tag {
					notifyForce(evNewFW, fmt.Sprintf("firmware %s is out (this miner runs v%s): Settings → Firmware update → Check for updates", rel.Tag, cur))
					os.MkdirAll(updDir(), 0o755)
					cfgWrite(p, []byte(rel.Tag+"\n"), 0o644)
				}
			}
		}
		time.Sleep(24 * time.Hour)
	}
}

// ---- daily summary ----

func dailyPath() string { return filepath.Join(filepath.Dir(*authFile), "daily-summary") }

var dailyPrev struct {
	acc, rej int
	set      bool
}

// dailyLoop: at 09:00 local time, once a day (remembered across restarts), while switched on.
func dailyLoop() {
	for {
		time.Sleep(time.Minute)
		dailyTick(time.Now())
	}
}

// dailyDone: the day already sent, also when its file couldn't be written (a full or read-only config
// partition: else every minute from 09:00 to 09:59 sent it again, up to notifyForce's 4 an hour).
var dailyDone string

func dailyTick(now time.Time) {
	if now.Hour() != 9 || !clockOK() {
		return
	}
	day := now.Format("2006-01-02")
	if dailyDone == day {
		return
	}
	if b, _ := os.ReadFile(dailyPath()); strings.TrimSpace(string(b)) == day {
		dailyDone = day
		return
	}
	notifyMu.Lock()
	on := notifyCfg.Events[evDaily]
	notifyMu.Unlock()
	writeAtomic(dailyPath(), []byte(day+"\n"), 0o644)
	dailyDone = day
	if !on {
		return
	}
	notifyForce(evDaily, dailySummary())
}

func dailySummary() string {
	d := buildDashboard()
	var parts []string
	if avg, n := hrAvgSince(time.Now().Unix() - 24*3600); n > 0 {
		parts = append(parts, fmt.Sprintf("24 h average %s", fmtGHS(avg)))
	}
	if !d.OK {
		parts = append(parts, "not mining right now ("+strings.TrimSuffix(d.Err, ".")+")")
		return "daily summary: " + strings.Join(parts, " · ")
	}
	parts = append(parts, "now "+fmtGHS(d.GHS))
	acc, rej := d.Accepted, d.Rejected
	if dailyPrev.set && acc >= dailyPrev.acc && rej >= dailyPrev.rej {
		da, dr := acc-dailyPrev.acc, rej-dailyPrev.rej
		pct := 0.0
		if da+dr > 0 {
			pct = float64(dr) / float64(da+dr) * 100
		}
		parts = append(parts, fmt.Sprintf("%d shares accepted, %d rejected (%.2f %%) since yesterday", da, dr, pct))
	} else {
		parts = append(parts, fmt.Sprintf("%d accepted, %d rejected since mining started", acc, rej))
	}
	dailyPrev.acc, dailyPrev.rej, dailyPrev.set = acc, rej, true
	if d.BestShareT != "" {
		parts = append(parts, "best share "+d.BestShareT)
	}
	if mx, _, ok := maxBoardTemp(d); ok {
		parts = append(parts, fmt.Sprintf("hottest board %.0f°C", mx))
	}
	parts = append(parts, "mining for "+fmtUptime(d.Elapsed))
	return "daily summary: " + strings.Join(parts, " · ")
}

func fmtGHS(g float64) string {
	if g >= 1000 {
		return fmt.Sprintf("%.2f TH/s", math.Round(g/10)/100)
	}
	return fmt.Sprintf("%.0f GH/s", g)
}

// A board that stops hashing mid-run usually stays down until mining restarts (seen: a board dropped at
// about 1 pm and hashed again only after the owner restarted mining at 2:20 pm). boardWatch restarts mining by
// itself, once an hour at most, when nothing else is in charge. "" = restarting; otherwise why not.
var (
	boardAutoAt   time.Time // the last automatic restart for a board
	boardAutoGap  = time.Hour
	boardAutoOff  = func() bool { _, err := os.Stat(boardAutoOffPath()); return err == nil }
	boardRestart  = softRestartMining
	boardAutoBusy = func() string {
		switch {
		case getCharge() == "ceiling":
			return "the limit probe is running"
		case sctRunning():
			return "the strong-chip test is running"
		case heatIdleHeld():
			return "the thermal guard holds the miner on Idle"
		}
		if err := miningHoldErr(); err != nil {
			return err.Error()
		}
		return ""
	}
)

func boardAutoOffPath() string {
	return filepath.Join(filepath.Dir(*authFile), "board-autorestart-off")
}

// boardAutoAtPath: when the last automatic restart was, on disk. The restart (PUT /mcb/restart) reboots the
// whole miner on this firmware: kept only in memory, "once an hour" started again at every boot, and a board
// that stays down rebooted the miner every ~10 minutes (5 minutes of mining, 3 minutes down, the boot).
func boardAutoAtPath() string { return filepath.Join(filepath.Dir(*authFile), "board-autorestart-at") }

// boardAutoLoad: a panel that just started (after that reboot) takes the last restart's time from disk.
func boardAutoLoad() {
	if !boardAutoAt.IsZero() {
		return
	}
	if b, err := os.ReadFile(boardAutoAtPath()); err == nil {
		if u, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64); err == nil && u > 0 {
			boardAutoAt = time.Unix(u, 0)
		}
	}
}

func boardAutoRestart(now time.Time, snap string) string {
	if boardAutoOff() {
		return "off"
	}
	if why := boardAutoBusy(); why != "" {
		return why
	}
	boardAutoLoad()
	if boardAutoAt.After(now) || (unsetStamp(boardAutoAt.Unix()) && now.Unix() >= clockSetUnix) {
		// a clock that jumped back (no RTC, before NTP), or a restart saved on the unset 1970 clock read with the
		// clock set (its age is unknown, not 56 years): the gap counts from now. Kept in memory with now's
		// monotonic reading, so the clock being set later doesn't end the gap either.
		boardAutoAt = now
	}
	if !boardAutoAt.IsZero() && now.Sub(boardAutoAt) < boardAutoGap {
		return "it already restarted for a board in the last hour, so this needs a look"
	}
	boardAutoAt = now
	writeAtomic(boardAutoAtPath(), []byte(strconv.FormatInt(now.Unix(), 10)), 0o644) // before the reboot it starts
	incidentWG.Add(1)
	go func() {
		defer incidentWG.Done()
		incidentSnapshot(snap + ": the logs before the automatic restart")
		// the panel's own restart: its mining program stop isn't a crash (the stop counters, "restarted on its own")
		markReboot(150*time.Second, 5*time.Minute, "Restarting mining (the miner reboots): a hash board stopped hashing — it will be back in a minute or two.")
		if err := boardRestart(); err != nil {
			clearReboot()
			activity.emit("🧩 automatic restart for a board that stopped hashing failed: " + err.Error())
			notifyForce(evBoard, "the automatic restart for the board that stopped hashing failed ("+err.Error()+"): Settings → Reboot miner")
			return
		}
		recordMark("R", "mining restarted: a board stopped hashing")
	}()
	return ""
}

// handleBoardAuto: GET {on}; POST {on}: the automatic restart when a board stops hashing (on by default).
func handleBoardAuto(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var in struct {
			On bool `json:"on"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in); err != nil {
			writeJSON(w, map[string]any{"ok": false, "err": "not a complete settings message"})
			return
		}
		var err error
		if in.On {
			err = os.Remove(boardAutoOffPath())
			if os.IsNotExist(err) {
				err = nil
			}
		} else {
			err = cfgWrite(boardAutoOffPath(), nil, 0o644)
		}
		if err != nil {
			writeJSON(w, map[string]any{"ok": false, "err": "couldn't save: " + err.Error()})
			return
		}
		activity.emit(map[bool]string{true: "🧩 mining restarts by itself when a board stops hashing", false: "🧩 a board that stops hashing: notice only, no automatic restart"}[in.On])
	}
	writeJSON(w, map[string]any{"ok": true, "on": !boardAutoOff()})
}
