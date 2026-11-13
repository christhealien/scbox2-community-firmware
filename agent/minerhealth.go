package main

// Mining-program health: Goldshell's watchdog (xminerd) starts the mining program (intminer) once at boot.
// When intminer crashes as it starts (a hash board or its data cable, the power supply), nothing starts it
// again, and Goldshell's backend (minerd) then fails on every request: the panel saw only "API :4028 isn't
// answering", HTTP 500s and "EOF" errors. This watches the process itself so the Dashboard can say what's
// wrong, and the log export carries Goldshell's own logs from /backup.

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	procRoot   = "/proc"
	backupRoot = "/backup"
	mhMu       sync.Mutex
	mhRunning  bool        // intminer seen at the last look
	mhSeen     bool        // intminer seen at all since the panel started
	mhStops    []time.Time // when it went away (not while a restart the panel made was under way)
)

// procRunning reports whether a process with one of these names runs.
func procRunning(names ...string) bool {
	ents, err := os.ReadDir(procRoot)
	if err != nil {
		return false
	}
	for _, e := range ents {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		b, err := os.ReadFile(filepath.Join(procRoot, e.Name(), "comm"))
		if err != nil {
			continue
		}
		c := strings.TrimSpace(string(b))
		for _, n := range names {
			if c == n {
				return true
			}
		}
	}
	return false
}

func intminerRunning() bool { return procRunning("intminer", "intminer.real") }

// procAgeSec: how long the youngest process with this name has run (seconds; -1: none running).
func procAgeSec(name string) float64 {
	up, err := os.ReadFile(filepath.Join(procRoot, "uptime"))
	if err != nil {
		return -1
	}
	var upS float64
	fmt.Sscanf(string(up), "%f", &upS)
	ents, err := os.ReadDir(procRoot)
	if err != nil {
		return -1
	}
	best := -1.0
	for _, e := range ents {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		if b, err := os.ReadFile(filepath.Join(procRoot, e.Name(), "comm")); err != nil || strings.TrimSpace(string(b)) != name {
			continue
		}
		st, err := os.ReadFile(filepath.Join(procRoot, e.Name(), "stat"))
		if err != nil {
			continue
		}
		f := strings.Fields(string(st[strings.LastIndexByte(string(st), ')')+1:])) // after "(comm)": field 3 on
		if len(f) < 20 {
			continue
		}
		start, _ := strconv.ParseFloat(f[19], 64) // field 22: start time in clock ticks (100 a second)
		if age := upS - start/100; best < 0 || age < best {
			best = age
		}
	}
	return best
}

// minerHealthTick: one look (every 15 s from minerHealthLoop).
func minerHealthTick(now time.Time) {
	run := intminerRunning()
	_, _, restarting := rebootActive()
	idle := getCharge() == "idle" // put on Idle on purpose (owner, schedule, guards): not a crash
	mhMu.Lock()
	defer mhMu.Unlock()
	if mhRunning && !run && !restarting && !idle {
		mhStops = append(mhStops, now)
	}
	if run {
		mhSeen = true
	}
	mhRunning = run
	verboseGuard(now)
	chipHookGuard(now)
	if !run {
		stalePidGuard()
	} else {
		killEscalate(now)
		// the guarded mining program has run 2 minutes: its start tries count from zero again
		if _, err := os.Stat(brandTmp + "guard.1"); err == nil && procAgeSec("intminer") > 120 {
			os.Remove(brandTmp + "guard.1")
			os.Remove(brandTmp + "guard.2")
			os.Remove(brandTmp + "guard.3")
		}
	}
	cut := now.Add(-time.Hour)
	for len(mhStops) > 0 && mhStops[0].Before(cut) {
		mhStops = mhStops[1:]
	}
}

func minerHealthLoop() {
	for {
		minerHealthTick(time.Now())
		fanLockTick(time.Now())
		time.Sleep(15 * time.Second)
	}
}

func intminerStopsSince(t time.Time) int {
	mhMu.Lock()
	defer mhMu.Unlock()
	n := 0
	for _, s := range mhStops {
		if s.After(t) {
			n++
		}
	}
	return n
}

// minerdExits: how often init restarted Goldshell's backend (minerd) in the window, from the system log
// ("init: process '… minerd' (pid N) exited").
func minerdExits(log string, now time.Time, window time.Duration) int {
	n := 0
	for _, l := range strings.Split(log, "\n") {
		// init's line for minerd: "process '…minerd' (pid N) exited" (since v1.5.13 "…exec minerd; sleep 60' (pid N)")
		// (the shell line ends "…exec minerd; sleep 60'' (pid N) exited": init adds its own quote)
		if !(strings.Contains(l, "minerd' (pid") || strings.Contains(l, "exec minerd; sleep 60")) || !strings.Contains(l, "(pid") || !strings.Contains(l, "exited") || len(l) < 15 {
			continue
		}
		t, err := time.ParseInLocation("Jan _2 15:04:05", l[:15], time.Local)
		if err != nil {
			continue
		}
		t = t.AddDate(now.Year(), 0, 0)
		if t.After(now.Add(24 * time.Hour)) {
			t = t.AddDate(-1, 0, 0)
		}
		if now.Sub(t) <= window && !t.After(now.Add(time.Minute)) {
			n++
		}
	}
	return n
}

// miningDownMessage: when the mining API doesn't answer and the miner isn't idle, starting or restarting:
// what the Dashboard says. "" when the mining program runs (the API is just slow).
// The diagnosis reads logs (and asks the backend): kept for 20 s, since every dashboard build asks.
var poolDownAt atomic.Int64

// poolDownMono: poolDownAt's value and when that was on the monotonic clock (monoSec). The guards ask "was no
// pool answering in the last 15 minutes": on the wall clock alone, a pool outage while the clock was still unset
// (1970, after a power cut the router comes up after the miner) was 56 years old once NTP set it, and the stops
// it caused switched chip trims or Verbose off.
var (
	poolDownMu   sync.Mutex
	poolDownWall int64
	poolDownMono int64
)

func poolDownNote(now time.Time) {
	poolDownMu.Lock()
	poolDownWall, poolDownMono = now.Unix(), monoSec()
	poolDownAt.Store(poolDownWall)
	poolDownMu.Unlock()
}

// poolDownRecent: no pool answered within the last 15 minutes.
func poolDownRecent(now time.Time) bool {
	t := poolDownAt.Load()
	if t <= 0 {
		return false
	}
	poolDownMu.Lock()
	wall, mono := poolDownWall, poolDownMono
	poolDownMu.Unlock()
	if wall == t && mono > 0 {
		return monoSec()-mono < 15*60
	}
	return now.Unix()-t < 15*60 // (set without poolDownNote)
}

var (
	mdMu  sync.Mutex
	mdAt  time.Time
	mdMsg string
)

func miningDownMessage(now time.Time) string {
	mdMu.Lock()
	if !mdAt.IsZero() && now.Sub(mdAt) < 20*time.Second && now.After(mdAt.Add(-time.Second)) {
		m := mdMsg
		mdMu.Unlock()
		return m
	}
	mdMu.Unlock()
	m := miningDownMessage0(now)
	mdMu.Lock()
	mdAt, mdMsg = now, m
	mdMu.Unlock()
	return m
}

// poolUnreachable: the mining program's last lines say it can't reach any pool (it then exits and is
// started again every few seconds: that's the network or the pool, not the hardware).
var poolUnreachableLog = func() string {
	t, err := minerLogText()
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(t), "\n")
	if len(lines) > 80 {
		lines = lines[len(lines)-80:]
	}
	for i := len(lines) - 1; i >= 0; i-- {
		l := lines[i]
		if strings.Contains(l, "No servers were found") || strings.Contains(l, "Stratum connect failed") || strings.Contains(l, "No route to host") {
			now := time.Now().Unix()
			if t := lineTime(l, now); now-t > 10*60 {
				return "" // an old line: not why it stops now
			}
			return l
		}
	}
	return ""
}

func miningDownMessage0(now time.Time) string {
	if v, ok := readWatchdog(now); ok {
		if m := watchdogMessage(v); m != "" {
			return m
		}
	}
	if intminerRunning() {
		return ""
	}
	stops := intminerStopsSince(now.Add(-30 * time.Minute))
	exits := 0
	if t, err := systemLogText(); err == nil {
		exits = minerdExits(t, now, 10*time.Minute)
	}
	if l := poolUnreachableLog(); l != "" {
		poolDownNote(now)
		return "The mining program can't reach any pool, so it stops and is started again every few seconds: this is the network or the pool, not the hardware. Check the miner's network (cable, router, the address it got) and the pool address on the Pools tab. Its last line: " + strings.TrimSpace(redactLog(l, privateTokens()))
	}
	msg := "The mining program (intminer) isn't running, so nothing is mining."
	if stops >= 2 {
		msg = fmt.Sprintf("The mining program (intminer) keeps stopping: %d times in the last 30 minutes, so it's crashing as it starts.", stops)
	}
	if exits >= 3 {
		msg += fmt.Sprintf(" %s backend (minerd) has restarted %d times in 10 minutes as well, because the mining program isn't there.", OemOf(), exits)
	}
	return msg + " This is usually hardware: a hash board or its data (ribbon) cable, often after a knock or a move, or the power supply. Settings → Reboot miner tries again (Restart mining goes through the backend, which fails without the mining program). To get help, Log → “For sending” saves the logs, now with " + oemOwnCurly() + " own watchdog, fan and backend logs."
}

// friendlyBackendErr: a request to Goldshell's backend that got no answer at all (connection refused, the
// connection closed mid-request = EOF, a timeout) said in words, without Go's error text.
func friendlyBackendErr(err error) error {
	if err == nil {
		return nil
	}
	s := err.Error()
	low := strings.ToLower(s)
	if !(strings.Contains(low, "eof") || strings.Contains(low, "connection refused") || strings.Contains(low, "connection reset") ||
		strings.Contains(low, "timeout") || strings.Contains(low, "deadline exceeded")) {
		return err
	}
	why := "it may be restarting; try again in a minute"
	if !intminerRunning() {
		why = "the mining program isn't running, and the backend fails without it (see the Dashboard)"
	}
	return fmt.Errorf("the miner's backend (%s minerd) didn't answer: %s", oemPoss(), why)
}

// ---- Goldshell's own logs for the log export ----

// goldshellLogs: name in the zip → path, the watchdog, fan controller and backend logs Goldshell keeps in
// /backup (they survive reboots; the system log doesn't have them).
func goldshellLogs() [][2]string {
	return [][2]string{
		{oemFile("watchdog-xminerd.txt"), filepath.Join(backupRoot, "xminerd/trash/xminerdlog")},
		{oemFile("fans-fanctrl.txt"), filepath.Join(backupRoot, "fanctrl/trash/fanctrllog")},
		{oemFile("backend-minerd.txt"), filepath.Join(backupRoot, "minerd/trash/syslog")},
	}
}

// tailFile: the last max bytes of a file, from a line start.
func tailFile(path string, max int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	off := int64(0)
	if st.Size() > max {
		off = st.Size() - max
	}
	b := make([]byte, st.Size()-off)
	n, _ := f.ReadAt(b, off)
	b = b[:n]
	if off > 0 {
		if i := strings.IndexByte(string(b), '\n'); i >= 0 {
			b = b[i+1:]
		}
	}
	return string(b), nil
}

// processesText: which of Goldshell's programs run now, and what the panel saw of the mining program.
func processesText(now time.Time) string {
	var b strings.Builder
	for _, n := range []string{"fanctrl", "xminerd", "minerd", "intminer"} {
		st := "not running"
		if procRunning(n) || (n == "intminer" && procRunning("intminer.real")) {
			st = "running"
		}
		fmt.Fprintf(&b, "%-9s %s\n", n, st)
	}
	fmt.Fprintf(&b, "\nmining program stops seen in the last hour: %d\n", intminerStopsSince(now.Add(-time.Hour)))
	if t, err := systemLogText(); err == nil {
		fmt.Fprintf(&b, "minerd restarts in the last 10 min: %d\n", minerdExits(t, now, 10*time.Minute))
	}
	fmt.Fprintf(&b, "uptime: %.0f min\n", systemUptime()/60)
	return b.String()
}

// configPoolTokens: pool users and hosts from the miner's own config files, for the private export when the
// backend can't be asked (it's down exactly when the logs matter).
func configPoolTokens() []string {
	var out []string
	add := func(u, usr string) {
		out = append(out, usr)
		if i := strings.Index(usr, "."); i > 0 {
			out = append(out, usr[:i], usr[i+1:])
		}
		h := u
		if i := strings.Index(h, "://"); i >= 0 {
			h = h[i+3:]
		}
		out = append(out, strings.Split(h, ":")[0])
	}
	if b, err := os.ReadFile(*bfgPath); err == nil {
		var m struct {
			Pools []struct{ URL, User string } `json:"pools"`
		}
		if json.Unmarshal(b, &m) == nil {
			for _, p := range m.Pools {
				add(p.URL, p.User)
			}
		}
	}
	if b, err := os.ReadFile(filepath.Join(filepath.Dir(filepath.Dir(*bfgPath)), "minerd/settings/pools.json")); err == nil {
		var ps []struct{ URL, User string }
		if json.Unmarshal(b, &ps) == nil {
			for _, p := range ps {
				add(p.URL, p.User)
			}
		}
	}
	return out
}

// verboseGuard (with mhMu held): Verbose runs the miner in its debug mode (-D -P). On one miner (a board
// knocked in shipping) the mining program crashed again and again in debug mode ("Floating point
// exception") and ran cleanly without it. When the mining program stops 3 times in 15 minutes with
// Verbose on, Verbose is switched off (the next start runs without debug mode) and the Log tab says why.
func verboseGuard(now time.Time) {
	if !shimInstalled() {
		return
	}
	flag := verboseFlagPath()
	if _, err := os.Stat(flag); err != nil {
		return
	}
	n := 0
	for _, s := range mhStops {
		if now.Sub(s) <= 15*time.Minute {
			n++
		}
	}
	if n < 3 {
		return
	}
	if poolDownRecent(now) {
		return // it stops because no pool answers, not because of debug mode
	}
	if os.Remove(flag) != nil {
		return
	}
	b, _ := json.Marshal(map[string]any{"at": now.Unix(), "stops": n})
	cfgWrite(verboseAutoOffPath(), b, 0o644)
	go activity.emit(fmt.Sprintf("⚠ Verbose switched off: the mining program stopped %d times in 15 minutes with it on (it runs the miner in debug mode)", n))
}

// chipHookGuard: with chip trims on, the miner runs a hooked copy of the mining program. If that copy keeps
// dying (4 stops in 15 minutes, and not because no pool answers), chip trims are switched off so the next
// start is Goldshell's own program: mining comes back, the owner is told why.
// chipHookLib: where the wrapper loads the chip-trim library from.
var chipHookLib = brandChipLibPath

func chipHookGuard(now time.Time) {
	if _, err := os.Stat(chipHookFlag()); err != nil {
		return
	}
	if fi, err := os.Stat(brandTmp + "hook.3"); err == nil && intminerRunning() && time.Since(fi.ModTime()) > 2*time.Minute && !minerHasLib(brandChipLib) {
		// the wrapper started the hooked copy 3 times without it getting going, and now runs the plain miner
		if poolDownRecent(now) {
			// no pool answered: the miner exits at every start before its chips find a nonce, so the hook never
			// clears the tries. That's not the trims: give the tries back (the next start has trims again)
			for _, m := range []string{brandTmp + "hook.1", brandTmp + "hook.2", brandTmp + "hook.3"} {
				os.Remove(m)
			}
			return
		}
		if os.Remove(chipHookFlag()) == nil {
			msg := "chip trims switched off: the mining program with chip trims didn't get going in 3 starts, so the miner runs without them (Controls → Chip trims to try again)"
			chipHookOffNote(msg)
			go activity.emit("⚠ " + msg)
			go notify(evChipTrim, msg)
		}
		return
	}
	if _, err := os.Stat(chipHookLib); err != nil && intminerRunning() && procAgeSec("intminer") > 180 && !minerHasLib(brandChipLib) {
		// the trim library isn't on the miner (a hand edit, a half-written update): the wrapper can't load it
		if os.Remove(chipHookFlag()) == nil {
			msg := "chip trims switched off: the chip-trim library (" + chipHookLib + ") is missing, so the miner runs without trims (reinstall the firmware update to bring it back)"
			chipHookOffNote(msg)
			go activity.emit("⚠ " + msg)
			go notify(evChipTrim, msg)
		}
		return
	}
	n := 0
	for _, s := range mhStops {
		if now.Sub(s) <= 15*time.Minute {
			n++
		}
	}
	if n < 4 {
		return
	}
	if poolDownRecent(now) {
		return
	}
	if os.Remove(chipHookFlag()) != nil {
		return
	}
	msg := fmt.Sprintf("chip trims switched off: the mining program stopped %d times in 15 minutes with them on, so it now starts as %s own program (Controls → Chip trims to try again)", n, oemPoss())
	chipHookOffNote(msg)
	go activity.emit("⚠ " + msg)
	go notify(evChipTrim, msg)
}

// stalePidGuard: Goldshell's watchdog removes its "miner running" file only when its own child exits. A
// mining program started by an earlier watchdog (it was restarted) isn't its child: when that one exits the
// file stays, and the watchdog never starts mining again. With no mining program running, a file naming a
// process that isn't one is removed.
var xminerdPidFile = "/var/run/xminerd.pid"

// killEscalate: Goldshell's watchdog only ever sends the mining program SIGINT for a fan fault, and powers
// the boards down only once it has exited. One that is still running 15 s after such a kill is ended for it.
var lastEscalated int64

func killEscalate(now time.Time) {
	v, ok := readWatchdog(now)
	if !ok || v.LastKill == 0 || v.LastKill == lastEscalated || now.Unix()-v.LastKill < 15 || now.Unix()-v.LastKill > 120 {
		return
	}
	if age := procAgeSec("intminer"); age < 0 || now.Unix()-int64(age) > v.LastKill {
		return // started after that kill: a new run
	}
	lastEscalated = v.LastKill
	cmd := exec.Command("sh", "-c", "killall -9 intminer")
	cmd.Env = append(os.Environ(), "PATH=/usr/sbin:/usr/bin:/sbin:/bin:"+os.Getenv("PATH"))
	cmd.Run()
	go activity.emit("⛏ the mining program didn't stop 15 s after " + oemOf() + " watchdog told it to (fan fault): ended")
}

var stalePidSeen string

// minerHasLib: the running mining program has this library loaded (its /proc/<pid>/maps).
func minerHasLib(name string) bool {
	ents, _ := os.ReadDir(procRoot)
	for _, e := range ents {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		if b, _ := os.ReadFile(filepath.Join(procRoot, e.Name(), "comm")); strings.TrimSpace(string(b)) == "intminer" {
			if m, err := os.ReadFile(filepath.Join(procRoot, e.Name(), "maps")); err == nil && strings.Contains(string(m), name) {
				return true
			}
		}
	}
	return false
}

func stalePidGuard() {
	b, err := os.ReadFile(xminerdPidFile)
	if err != nil {
		return
	}
	pid := strings.TrimSpace(string(b))
	if _, err := strconv.Atoi(pid); err != nil {
		return
	}
	if _, err := os.Stat(filepath.Join(procRoot, pid)); err == nil {
		// alive: the mining program, or the watchdog's child between writing the file and starting it
		// (xminerd writes it, syncs, then execs). Only the same live non-miner pid on two looks 15 s apart.
		c, _ := os.ReadFile(filepath.Join(procRoot, pid, "cmdline"))
		if strings.Contains(string(c), "intminer") || pid != stalePidSeen {
			stalePidSeen = pid
			return
		}
	}
	stalePidSeen = ""
	if os.Remove(xminerdPidFile) == nil {
		go activity.emit("⛏ " + OemOf() + " watchdog had lost track of the mining program (a stale running-file): cleared, it starts mining again")
	}
}

func verboseAutoOffPath() string {
	return filepath.Join(filepath.Dir(*authFile), "verbose-auto-off.json")
}

// ---- Goldshell's watchdog (xminerd): fan check ----
//
// Every few seconds the watchdog logs each fan's duty and measured RPM and a "fan status" bitmask (bit n set:
// fan n looks failed). While any bit is set it kills the mining program ("Kill Miner Reason NCF(0,1,1)") and
// starts it again, over and over. Seen on two miners with replacement fans: one whose speed wire wasn't
// connected (0 RPM), one whose fans all ran slower than the firmware accepts.

type watchdogView struct {
	Fans    []int  `json:"fans,omitempty"` // RPM per fan, as the watchdog measured them
	Duty    []int  `json:"duty,omitempty"` // each fan's level (%) at that reading
	Status  int    `json:"status"`         // bitmask of flagged fans
	Flagged string `json:"flagged,omitempty"`
	Kills   int    `json:"kills,omitempty"` // mining program kills in the last 10 minutes
	At      int64  `json:"at,omitempty"`
	// the last reading in the last 10 minutes that flagged a fan: the flag lasts a second or two each time
	// (the watchdog kills the miner, restarts it at another fan level, then flags again), so the latest
	// reading is often clear while the kills go on
	Last     *watchdogView `json:"last_flag,omitempty"`
	LastKill int64         `json:"last_kill,omitempty"`
}

// flaggedView: the reading that explains the kills (the latest if it flags a fan, else the last that did
// while kills went on). ok=false: nothing flagged.
func flaggedView(v watchdogView) (watchdogView, bool) {
	if v.Status != 0 {
		return v, true
	}
	if v.Kills > 0 && v.Last != nil && v.Last.Status != 0 {
		f := *v.Last
		f.Kills = v.Kills
		return f, true
	}
	return v, false
}

func flaggedText(fans []int, status int) string {
	var parts []string
	for i := 0; i < 8; i++ {
		if status&(1<<i) != 0 {
			if i < len(fans) {
				parts = append(parts, fmt.Sprintf("fan %d (%d RPM)", i, fans[i]))
			} else {
				parts = append(parts, fmt.Sprintf("fan %d", i))
			}
		}
	}
	return "the watchdog flags " + strings.Join(parts, ", ")
}

var (
	reWdFan    = regexp.MustCompile(`fan(\d):\((\d+), (-?\d+)\)`)
	reWdStatus = regexp.MustCompile(`fan status: ([0-9a-fA-F]+)`)
	reWdTime   = regexp.MustCompile(`\[(\d{4}-\d\d-\d\d \d\d:\d\d:\d\d)\]`)
)

func watchdogPath() string { return filepath.Join(backupRoot, "xminerd/trash/xminerdlog") }

// readWatchdog: the watchdog's latest fan reading (if it's from the last 2 minutes) and its recent kills.
func readWatchdog(now time.Time) (watchdogView, bool) {
	t, err := tailFile(watchdogPath(), 16<<10)
	if err != nil {
		return watchdogView{}, false
	}
	return parseWatchdog(t, now)
}

func parseWatchdog(t string, now time.Time) (watchdogView, bool) {
	var v watchdogView
	found := false
	lines := strings.Split(t, "\n")
	for _, l := range lines {
		ts := time.Time{}
		if m := reWdTime.FindStringSubmatch(l); m != nil {
			ts, _ = time.ParseInLocation("2006-01-02 15:04:05", m[1], time.Local)
		}
		if strings.Contains(l, "Kill Miner Reason") && !ts.IsZero() && now.Sub(ts) <= 10*time.Minute && !ts.After(now.Add(time.Minute)) {
			v.Kills++
			v.LastKill = ts.Unix()
		}
		sm := reWdStatus.FindStringSubmatch(l)
		if sm == nil {
			continue
		}
		fm := reWdFan.FindAllStringSubmatch(l, -1)
		if len(fm) == 0 || ts.IsZero() || now.Sub(ts) > 10*time.Minute || ts.After(now.Add(time.Minute)) {
			continue
		}
		st, _ := strconv.ParseInt(sm[1], 16, 32)
		fans := make([]int, 0, len(fm))
		duty := make([]int, 0, len(fm))
		for _, f := range fm {
			r, _ := strconv.Atoi(f[3])
			d, _ := strconv.Atoi(f[2])
			fans = append(fans, r)
			duty = append(duty, d)
		}
		if st != 0 {
			v.Last = &watchdogView{Fans: fans, Duty: duty, Status: int(st), At: ts.Unix(), Flagged: flaggedText(fans, int(st))}
		}
		if now.Sub(ts) > 2*time.Minute {
			continue // the current reading: the last 2 minutes only
		}
		v.Fans, v.Duty, v.Status, v.At, found = fans, duty, int(st), ts.Unix(), true
	}
	if !found {
		return v, v.Kills > 0
	}
	if v.Status != 0 {
		v.Flagged = flaggedText(v.Fans, v.Status)
	}
	return v, true
}

// watchdogMessage: why the watchdog keeps stopping mining, in words ("" when it doesn't).
func watchdogMessage(v watchdogView) string {
	v, ok := flaggedView(v)
	if !ok {
		return ""
	}
	zero, n, startup := false, 0, true
	for i, r := range v.Fans {
		if v.Status&(1<<i) != 0 {
			n++
			if r <= 0 {
				zero = true
			}
			if i < len(v.Duty) && v.Duty[i] < 99 {
				startup = false // its start-up check runs at 100 %: below that it's the running check
			}
		}
	}
	msg := OemOf() + " watchdog stops mining while a fan looks failed, and " + strings.TrimPrefix(v.Flagged, "the watchdog ") + "."
	if v.Kills > 0 {
		times := "times"
		if v.Kills == 1 {
			times = "time"
		}
		msg += fmt.Sprintf(" It stopped the mining program %d %s in the last 10 minutes.", v.Kills, times)
	}
	switch {
	case !zero && learnedSuspect(v):
		msg += " That fan turns as fast as its pair partner at the same level, so it's most likely working: " + oemOf() + " check records each fan's speed the first time it sees it at a level, and a reading taken while the fan was turning at a different speed (for example while its partner drove their shared signal higher) makes it flag a working fan. Controls & fan → Fan check → Re-run " + oemOf() + " fan test makes it forget those records and test the fans again."
	case zero:
		msg += " 0 RPM means the fan isn't turning or its speed wire isn't connected: on the board's fan header pin 1 is GND, 2 is 12V, 3 is speed (the fan's RPM wire), 4 is PWM."
	case n == len(v.Fans) && n > 0 && startup:
		msg += fmt.Sprintf(" All fans are flagged while they turn: they run slower than %s check expects (it's set for the stock %d RPM fans).", oemOf(), model.FanStockMaxRPM)
		if !featOff(model.Feat.FanFit) { // (not on the SC Box II: "fit to my fans" isn't available there)
			msg += " Controls & fan → Fan check can fit it to these fans."
		}
	case n == len(v.Fans) && n > 0:
		msg += " All fans are flagged while they turn: while mining, " + oemOf() + " check compares each fan with 60 % of the speed it measured in its start-up fan test (fitting the check doesn't change that). Check the fans' power and cables; if they turn normally, Controls & fan → Fan check → Re-run " + oemOf() + " fan test."
	default:
		msg += " Check that fan and its cable."
	}
	return msg
}

// learnedSuspect: every flagged fan turns at about the speed of its pair partner (fan0/fan1, fan2/fan3:
// one shared speed signal) at the same level, and the partner isn't flagged: the fan works, and the flag
// most likely comes from a speed the watchdog recorded earlier at that level (xminerd 0xa18e).
func learnedSuspect(v watchdogView) bool {
	if len(v.Fans) < model.Fans || len(v.Duty) < model.Fans {
		return false
	}
	any := false
	for i := 0; i < model.Fans; i++ {
		if v.Status&(1<<i) == 0 {
			continue
		}
		p := fanGroupPartner(i) // the other fan on this fan's shared PWM (model FanGroups, not the old i^1)
		if p < 0 || p >= len(v.Fans) || p >= len(v.Duty) {
			return false // no single pair partner to compare against: can't call it a learned-reading flag
		}
		if v.Status&(1<<p) != 0 || v.Fans[i] <= 0 || v.Fans[p] <= 0 || absInt(v.Duty[i]-v.Duty[p]) > 1 {
			return false
		}
		lo, hi := v.Fans[i], v.Fans[p]
		if lo > hi {
			lo, hi = hi, lo
		}
		if float64(lo) < 0.8*float64(hi) {
			return false
		}
		any = true
	}
	return any
}

func absInt(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// chipHookArmedByHand: chip trims switched on by hand: the stops counted so far (they may be what switched
// them off) don't count against the new start, or the next health tick would switch them straight off again.
func chipHookArmedByHand() {
	mhMu.Lock()
	mhStops = nil
	mhMu.Unlock()
}

// chipHookOffNote: why the guard switched chip trims off, kept for the Chip trims card (the activity line
// is gone by the time the owner looks, and notifications may not be set up).
func chipHookOffNote(msg string) {
	b, _ := json.Marshal(map[string]any{"at": time.Now().Unix(), "why": msg})
	cfgWrite(chipHookFlag()+".off", b, 0o644)
}
