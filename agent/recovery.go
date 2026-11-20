package main

// Recovery (Settings → Recovery), both off by default:
//
//   - Hardware watchdog. The SoC has one (S60wdt loads imapx_wdt with tmr_atboot=0) but nothing in
//     the OEM's firmware ever opens /dev/watchdog (traced: libsys has system_enable/feed_watchdog,
//     no program calls them), so a frozen control board stays frozen until someone pulls the power
//     (seen on SClite-1: every program stopped in the same second, 2 h dead). When switched on, the
//     agent opens /dev/watchdog, asks for a 60 s timeout and feeds it every few seconds from its own
//     goroutine. A hung kernel or a frozen board stops the feeding and the chip reboots the miner. The
//     agent's own restarts are covered: the boot script restarts it within 5 s and it reopens the
//     device. Switching it off sends the magic close ('V'), which disarms drivers that allow it.
//   - Keep the system log across reboots. /var/log is RAM, so a freeze takes its last kernel messages
//     with it. Every 2 minutes (only when it changed) the tail of the log is saved to the config
//     partition; at the next start that copy becomes "system log before the last restart" in the log
//     export.
//
// Either one also keeps a "last seen" time (every minute) so the Dashboard can say when the miner
// restarted without the panel asking for it, and since when it had been silent.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

type recoveryConfig struct {
	Watchdog bool `json:"watchdog"`
	KeepLog  bool `json:"keep_log"`
}

type unexpectedRestart struct {
	LastSeen  int64  `json:"last_seen"`           // the agent's last "alive" note before the restart
	BootAt    int64  `json:"boot_at"`             // when this boot started
	ByWDT     bool   `json:"by_watchdog"`         // the watchdog driver says it caused the reset
	WDTKnown  bool   `json:"watchdog_known"`      // the driver can tell
	Dismissed bool   `json:"dismissed,omitempty"` // the user closed the note
	Note      string `json:"note,omitempty"`
}

var (
	recMu                      sync.Mutex
	recCfg                     recoveryConfig
	wdtFile                    *os.File
	wdtTimeout                 int
	wdtErr                     string
	wdtStop                    chan struct{}
	recLastSave                string // the last saved log tail (skip identical writes)
	recUnexp                   *unexpectedRestart
	wdtStarting                bool
	wdtBootByWDT, wdtBootKnown bool // the driver's word on the last reset, read when it was opened

	watchdogDev = "/dev/watchdog"
)

func recDir() string            { return filepath.Dir(*authFile) }
func recConfigPath() string     { return filepath.Join(recDir(), "recovery.json") }
func recAlivePath() string      { return filepath.Join(recDir(), "last-alive.json") }
func recCleanPath() string      { return filepath.Join(recDir(), "clean-reboot") }
func recUnexpPath() string      { return filepath.Join(recDir(), "unexpected-restart.json") }
func keptLogPath() string       { return filepath.Join(recDir(), "syslog-kept.txt") }
func keptLogBeforePath() string { return filepath.Join(recDir(), "syslog-before-restart.txt") }
func recoveryConfigLoad() recoveryConfig {
	var c recoveryConfig
	if b, ok := readConfig(recConfigPath(), json.Valid); ok { // falls back to recovery.json.bak after a FAT power cut
		json.Unmarshal(b, &c)
	}
	return c
}

// markCleanReboot: the panel (or its updater) is about to reboot on purpose. This is also the moment to
// flush the hot files: on the Box II with its settings in RAM, the hashrate history, the events and the other hot
// files reach the flash only at a checkpoint (cfgCheckpoint: a reboot the panel makes, the panel stopped) or with an
// owner's save that touches them, never on a timer; a reboot would otherwise lose everything since the last one.
// Cheap and harmless on SC Lite, where these are already written frequently.
func markCleanReboot() {
	persistHashrate()
	eventsSave()
	os.WriteFile(recCleanPath(), []byte(time.Now().Format(time.RFC3339)), 0644)
	cfgCheckpoint() // the Box II: the panel's files (hot ones included) from RAM onto the flash (cfgshadow.go)
}

const (
	wdiocGetBootStatus = 0x80045702 // _IOR('W', 2, int)
	wdiocSetTimeout    = 0xC0045706 // _IOWR('W', 6, int)
	wdiocGetTimeout    = 0x80045707 // _IOR('W', 7, int)
	wdiofCardReset     = 0x0020
)

func wdtIoctl(f *os.File, req uintptr, v *int32) error {
	_, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), req, uintptr(unsafe.Pointer(v)))
	if e != 0 {
		return e
	}
	return nil
}

// watchdogBootStatus asks the driver whether it caused the last reset (without arming it: only
// when the device is already open by us).
func watchdogBootStatus(f *os.File) (byWDT, known bool) {
	var st int32
	if wdtIoctl(f, wdiocGetBootStatus, &st) != nil {
		return false, false
	}
	return st&wdiofCardReset != 0, true
}

func wdtMarkerPath() string { return filepath.Join(recDir(), "watchdog-starting") }

var (
	wdtOpenTimeout = 5 * time.Second
	wdtStartDelay  = 20 * time.Second
	wdtArmedFlag   = brandTmp + "wdt-armed" // armed (and not cleanly closed) this boot
)

// watchdogStart switches the hardware watchdog on. It never blocks the panel: the device is opened in its
// own goroutine with a time limit, no lock is held while the device is touched, and a marker written first
// (removed when it worked) lets the next start see that this step hung or crashed and switch the watchdog
// off instead of trying again. (v1.5.12 opened the device during start-up, before the web server and SSH;
// on one miner that step never returned and the panel didn't come up at all.)
func watchdogStart() error {
	if !watchdogUsable {
		return fmt.Errorf("%s", wdtUnavailable)
	}
	recMu.Lock()
	if wdtFile != nil || wdtStarting {
		recMu.Unlock()
		return nil
	}
	wdtStarting = true
	recMu.Unlock()
	defer func() { recMu.Lock(); wdtStarting = false; recMu.Unlock() }()

	os.WriteFile(wdtMarkerPath(), []byte(time.Now().Format(time.RFC3339)), 0644)
	syscall.Sync()
	type res struct {
		f   *os.File
		t   int32
		err error
	}
	done := make(chan res, 1)
	var abandoned int32
	dev := watchdogDev // (read here: the goroutine may outlive this call)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- res{err: fmt.Errorf("panic: %v", r)}
			}
		}()
		f, err := os.OpenFile(dev, os.O_WRONLY, 0)
		if err != nil {
			done <- res{err: err}
			return
		}
		t := int32(60)
		_ = wdtIoctl(f, wdiocSetTimeout, &t) // some drivers keep their own margin: read it back
		var got int32
		if wdtIoctl(f, wdiocGetTimeout, &got) == nil && got > 0 {
			t = got
		} else {
			t = 0 // the driver doesn't say: shown as unknown
		}
		f.Write([]byte{0})
		byWDT, known := watchdogBootStatus(f) // read here, inside the time limit, never under a lock later
		recMu.Lock()
		wdtBootByWDT, wdtBootKnown = byWDT, known
		recMu.Unlock()
		if atomic.LoadInt32(&abandoned) != 0 { // answered too late: disarm and let go
			f.Write([]byte("V"))
			f.Close()
			return
		}
		done <- res{f: f, t: t}
	}()
	var r res
	select {
	case r = <-done:
	case <-time.After(wdtOpenTimeout):
		atomic.StoreInt32(&abandoned, 1)
		r.err = fmt.Errorf("the watchdog device didn't answer within %s", wdtOpenTimeout)
	}
	if r.err != nil {
		os.Remove(wdtMarkerPath())
		recMu.Lock()
		wdtErr = "switched off: " + r.err.Error()
		recCfg.Watchdog = false // don't try again at every start: the user can switch it on again
		if b, err := json.Marshal(recCfg); err == nil {
			writeAtomic(recConfigPath(), b, 0644)
		}
		recMu.Unlock()
		return fmt.Errorf("can't switch the hardware watchdog on: %v", r.err)
	}
	os.Remove(wdtMarkerPath())
	t := r.t
	recMu.Lock()
	wdtFile, wdtTimeout, wdtErr = r.f, int(t), ""
	os.WriteFile(wdtArmedFlag, []byte("1"), 0o644) // RAM: a panel restarted this boot reopens it at once
	recMu.Unlock()
	if t <= 0 {
		t = 10 // for the feeding interval only
	}
	every := time.Duration(t) * time.Second / 4
	if every < time.Second {
		every = time.Second
	}
	if every > 2*time.Second { // the driver's own margin may be shorter than it reports (S60wdt: tmr_margin=10000)
		every = 2 * time.Second
	}
	stop := make(chan struct{})
	recMu.Lock()
	wdtStop = stop
	recMu.Unlock()
	f := r.f
	go func() { // feeds without holding any lock: a slow write can't hold up the panel
		tk := time.NewTicker(every)
		defer tk.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tk.C:
				f.Write([]byte{0})
			}
		}
	}()
	installShutdownHandler() // the boot script's stop / shutdown: disarm first, then exit as before (cfgshadow.go)
	activity.emit("⏱ hardware watchdog on: the miner reboots itself if the control board stops")
	return nil
}

// watchdogStop disarms with the magic close where the driver allows it.
func watchdogStop() {
	recMu.Lock()
	if wdtStop != nil {
		close(wdtStop)
		wdtStop = nil
	}
	f := wdtFile
	wdtFile = nil
	recMu.Unlock()
	if f != nil {
		go func() { f.Write([]byte("V")); f.Close() }() // never wait on the device
		os.Remove(wdtArmedFlag)
		time.Sleep(50 * time.Millisecond)
	}
}

// keepLogTick saves the tail of the system log when it changed. keepLogMu: the minute loop and a Settings change
// (which starts one at once) share recLastSave and the .tmp file.
var keepLogMu sync.Mutex

func keepLogTick() {
	keepLogMu.Lock()
	defer keepLogMu.Unlock()
	t, err := systemLogText()
	if err != nil {
		return
	}
	// Cap the kept tail: 32 KiB on the Box II's FAT (R04), 48 KiB on SC Lite.
	tailCap := 48 << 10
	if configFAT {
		tailCap = 32 << 10
	}
	if len(t) > tailCap {
		t = t[len(t)-tailCap:]
		if i := strings.IndexByte(t, '\n'); i >= 0 {
			t = t[i+1:]
		}
	}
	if t == recLastSave {
		return
	}
	if cfgWrite(keptLogPath(), []byte(t), 0644) == nil {
		recLastSave = t
	}
}

func writeAlive() {
	b, _ := json.Marshal(map[string]int64{"t": time.Now().Unix()})
	cfgWrite(recAlivePath(), b, 0644)
}

func saveUnexpected() error {
	if recUnexp == nil {
		return nil
	}
	b, _ := json.Marshal(recUnexp)
	return cfgWrite(recUnexpPath(), b, 0644)
}

func bootTime() int64 {
	b, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return time.Now().Unix()
	}
	var up float64
	fmt.Sscanf(string(b), "%f", &up)
	return time.Now().Unix() - int64(up)
}

// recoveryStartup runs once at agent start. The watchdog starts at once; the "did it restart without
// being asked" check waits for the clock (the board has no battery clock: it reads 1970 until NTP).
// watchdogUsable: false on this board. Its kernel driver deadlocks a CPU when /dev/watchdog is opened (the
// start takes a spinlock that the stop it calls takes again: traced in the 3.10 kernel), which is what took
// v1.5.12's panel and SSH away; the boot script removes the device node. Kept switchable for tests only.
var watchdogUsable = false

const wdtUnavailable = "the hardware watchdog can't be used on this board: its kernel driver locks up a processor core when it's switched on (this is what stopped the panel on v1.5.12). Keep the system log is still available"

func recoveryStartup() {
	recMu.Lock() // (a panel start: only the tests' leftover loops can be reading these meanwhile)
	recCfg = recoveryConfigLoad()
	if !watchdogUsable && recCfg.Watchdog {
		recCfg.Watchdog = false // saved by v1.5.12–v1.5.14: never open the device
		if b, err := json.Marshal(recCfg); err == nil {
			writeAtomic(recConfigPath(), b, 0644)
		}
		activity.emit("⏱ hardware watchdog switched off: " + wdtUnavailable)
	}
	if b, err := os.ReadFile(recUnexpPath()); err == nil {
		var u unexpectedRestart
		if json.Unmarshal(b, &u) == nil {
			recUnexp = &u
		}
	}
	if _, err := os.Stat(wdtMarkerPath()); err == nil {
		// the last start never got past switching the watchdog on (it hung or crashed there): off it goes
		os.Remove(wdtMarkerPath())
		recCfg.Watchdog = false
		if b, err := json.Marshal(recCfg); err == nil {
			writeAtomic(recConfigPath(), b, 0644)
		}
		wdtErr = "switched off: the last start hung or crashed while switching it on"
		activity.emit("⚠ hardware watchdog switched off: the last start hung or crashed while switching it on")
	}
	wdtOn := recCfg.Watchdog
	recMu.Unlock()
	if wdtOn {
		go func() { // after the web server and SSH are up, never in the way of them
			d := wdtStartDelay
			if _, err := os.Stat(wdtArmedFlag); err == nil || time.Now().Unix()-bootTime() > 180 {
				// only the panel restarted (it was armed and is no longer fed): reopen quickly, before the
				// driver's own margin (possibly ~10 s) runs out and resets the board
				d = time.Second
			}
			time.Sleep(d)
			if err := watchdogStart(); err != nil {
				activity.emit("⚠ " + err.Error())
			}
		}()
	}
	// the clock is already set (a panel restart within a boot, or NTP was quick): checked now, before start-up
	// goes on; else once NTP has set it
	checked := clockOK()
	if checked {
		recoveryCheckRestart(bootTime())
	}
	go func() {
		if !checked {
			for !clockOK() {
				time.Sleep(5 * time.Second)
			}
			recoveryCheckRestart(bootTime())
		}
		tk := time.NewTicker(time.Minute)
		defer tk.Stop()
		// Both the last-alive note and the kept-log tail land on flash. On the Box II's FAT they are thinned
		// (R04/R19): the alive note every ~5 min (enough to spot an unexpected restart) and the log tail every
		// ~10 min, instead of every 1 and 2 min; with its settings in RAM (v0.5.2: each of these opens a write
		// window on the flash, the only writes while the miner runs) every 15 and 30 min (keepLogEvery). On SC
		// Lite's ext the original cadence is kept.
		aliveEvery, keepEvery := keepLogEvery()
		aliveMod, keepMod := int(aliveEvery/time.Minute), int(keepEvery/time.Minute)
		for n := 0; ; n++ {
			recMu.Lock()
			on := recCfg.Watchdog || recCfg.KeepLog
			keep := recCfg.KeepLog
			recMu.Unlock()
			if on && n%aliveMod == 0 {
				writeAlive()
			}
			if keep && n%keepMod == 0 {
				keepLogTick()
			}
			<-tk.C
		}
	}()
}

// keepLogEvery: how often the last-alive note and the kept-log tail are saved (both land on the flash).
func keepLogEvery() (alive, keep time.Duration) {
	alive, keep = cfgCheckpointGap(5*time.Minute, time.Minute), cfgCheckpointGap(10*time.Minute, 2*time.Minute)
	if cfgShadow() { // the settings in RAM: each save is a write window on the flash
		alive, keep = max(alive, 15*time.Minute), max(keep, 30*time.Minute)
	}
	return alive, keep
}

// recoveryCheckRestart: the previous boot's "alive" note is older than this boot → this is a new
// boot. Its kept log becomes "before the restart"; without a clean-reboot note it was unexpected.
func recoveryCheckRestart(boot int64) {
	var alive struct{ T int64 }
	b, err := os.ReadFile(recAlivePath())
	if err != nil || json.Unmarshal(b, &alive) != nil || alive.T <= 0 || alive.T >= boot {
		return // never written, or written during this boot (only the agent restarted)
	}
	os.Remove(recAlivePath()) // used once: a later boot compares with this boot's own note
	if fi, err := os.Stat(keptLogPath()); err == nil && fi.ModTime().Unix() < boot {
		os.Rename(keptLogPath(), keptLogBeforePath())
	}
	_, clean := os.Stat(recCleanPath())
	os.Remove(recCleanPath())
	// the Box II: the notes used up, and the kept log's new name, onto the flash too (one window, at start)
	cfgPersist(recAlivePath(), recCleanPath(), keptLogPath(), keptLogBeforePath())
	if clean == nil {
		return
	}
	recMu.Lock()
	u := &unexpectedRestart{LastSeen: alive.T, BootAt: boot}
	if wdtFile != nil {
		u.ByWDT, u.WDTKnown = wdtBootByWDT, wdtBootKnown
	}
	recUnexp = u
	saveUnexpected()
	recMu.Unlock()
	activity.emit(fmt.Sprintf("⚠ the miner restarted without the panel asking (last seen %s)", time.Unix(alive.T, 0).Format("Jan 2 15:04")))
}

func recoveryView() map[string]any {
	recMu.Lock()
	defer recMu.Unlock()
	v := map[string]any{"ok": true, "watchdog": recCfg.Watchdog, "keep_log": recCfg.KeepLog,
		"watchdog_running": wdtFile != nil, "watchdog_timeout_s": wdtTimeout}
	alive, keep := keepLogEvery()
	v["keep_log_every_min"], v["alive_every_min"] = int(keep/time.Minute), int(alive/time.Minute)
	v["keep_log_flash"] = cfgShadow()
	if wdtErr != "" {
		v["watchdog_err"] = wdtErr
	}
	if fi, err := os.Stat(keptLogBeforePath()); err == nil {
		v["kept_before"] = fi.ModTime().Unix()
	}
	if syslogRemoteAvail() {
		v["syslog_remote_avail"] = true
		v["syslog_remote"] = syslogRemoteGet()
		if e := syslogApplyErrGet(); e != "" {
			v["syslog_remote_err"] = e // saved, but syslogd wasn't restarted for it: what it does now is unknown
		}
	}
	if recUnexp != nil && !recUnexp.Dismissed {
		v["unexpected"] = recUnexp
	}
	return v
}

func handleRecovery(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, recoveryView())
		return
	}
	var in struct {
		Watchdog *bool `json:"watchdog"`
		KeepLog  *bool `json:"keep_log"`
		Dismiss  bool  `json:"dismiss"`
		// the SC Box II: Settings → Troubleshooting "send the system log to this computer" (host:port, "" = off)
		SyslogRemote *string `json:"syslog_remote"`
	}
	if !decodeIn(w, r, &in) {
		return
	}
	if in.SyslogRemote != nil {
		if err := syslogRemoteSet(*in.SyslogRemote); err != nil {
			writeJSON(w, map[string]any{"ok": false, "err": err.Error()})
			return
		}
		writeJSON(w, recoveryView())
		return
	}
	if in.Dismiss {
		recMu.Lock()
		var err error
		if recUnexp != nil {
			recUnexp.Dismissed = true
			err = saveUnexpected()
		}
		recMu.Unlock()
		if err != nil { // (dismissed in memory all the same)
			writeJSON(w, map[string]any{"ok": false, "err": "dismissed until the panel restarts, but " + cfgSaveErr(err)})
			return
		}
		writeJSON(w, recoveryView())
		return
	}
	recMu.Lock()
	c := recCfg
	if in.Watchdog != nil {
		if *in.Watchdog && !watchdogUsable {
			recMu.Unlock()
			writeJSON(w, map[string]any{"ok": false, "err": wdtUnavailable})
			return
		}
		c.Watchdog = *in.Watchdog
	}
	if in.KeepLog != nil {
		c.KeepLog = *in.KeepLog
	}
	b, _ := json.Marshal(c)
	err := writeAtomic(recConfigPath(), b, 0644)
	recMu.Unlock()
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "err": cfgSaveErr(err)})
		return
	}
	recMu.Lock()
	recCfg = c
	recMu.Unlock()
	if !c.Watchdog && !c.KeepLog {
		os.Remove(recAlivePath()) // no "last seen" note goes stale while both are off (a false restart warning later)
	}
	if c.Watchdog {
		if err := watchdogStart(); err != nil {
			recMu.Lock()
			recCfg.Watchdog = false
			b, _ := json.Marshal(recCfg)
			writeAtomic(recConfigPath(), b, 0644)
			recMu.Unlock()
			writeJSON(w, map[string]any{"ok": false, "err": err.Error()})
			return
		}
	} else {
		watchdogStop()
	}
	if c.KeepLog {
		go keepLogTick()
		go writeAlive()
	}
	writeJSON(w, recoveryView())
}

// unexpectedForDashboard: the note for the Dashboard, or nil.
func unexpectedForDashboard() *unexpectedRestart {
	recMu.Lock()
	defer recMu.Unlock()
	if recUnexp == nil || recUnexp.Dismissed || time.Now().Unix()-recUnexp.BootAt > 7*86400 {
		return nil
	}
	u := *recUnexp
	return &u
}

// ---- the system log to another computer (the SC Box II, every build; off by default) ----
//
// The setting is one line, host:port, in syslog-remote next to the panel's other settings (a setting: committed to
// the flash when written). The boot script (S48scbox2) reads it at every start and runs busybox syslogd again with
// -R host:port -L, so the kernel's and every program's log lines (the tuner's breadcrumbs, trace.go, included) also
// go to that computer over UDP as they are logged. Changing it here runs the boot script's "syslog" step at once.

func syslogRemotePath() string { return filepath.Join(recDir(), "syslog-remote") }

// syslogRemoteAvail: the setting is offered (Settings → Troubleshooting, the SC Box II only; not the SC Lite).
func syslogRemoteAvail() bool { return model.Plan == planBox2V }

// syslogRemoteApply: syslogd restarted for the setting now (the boot script's step; a variable for the tests).
var syslogRemoteApply = func() error {
	cmd := exec.Command("/bin/sh", "/etc/init.d/S48scbox2", "syslog")
	cmd.Env = append(os.Environ(), "PATH=/usr/sbin:/usr/bin:/sbin:/bin:"+os.Getenv("PATH"))
	done := make(chan error, 1)
	var out []byte
	go func() {
		var err error
		out, err = cmd.CombinedOutput()
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("restarting the system log failed (%v %s)", err, strings.TrimSpace(string(out)))
		}
		return nil
	case <-time.After(20 * time.Second):
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
		return fmt.Errorf("restarting the system log didn't finish within 20 s")
	}
}

// syslogRemoteNorm: the setting as stored: "" (off) or ipv4:port (the port 1-65535, 514 when none is given). An
// IPv4 address only, no host name: busybox syslogd looks a name up in its main loop, and a name server that doesn't
// answer would then hold up /dev/log, so every program that logs.
func syslogRemoteNorm(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	host, port := s, "514"
	if i := strings.LastIndexByte(s, ':'); i >= 0 {
		host, port = s[:i], s[i+1:]
	}
	a, err := netip.ParseAddr(host)
	if err != nil || !a.Is4() || a.IsUnspecified() || a.IsMulticast() || a.String() != host {
		return "", fmt.Errorf("enter the computer as an IPv4 address with an optional port, e.g. 192.168.1.20:514 (UDP; no host names); empty turns it off")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port {
		return "", fmt.Errorf("the port must be 1-65535 (syslog is 514)")
	}
	return host + ":" + port, nil
}

func syslogRemoteGet() string {
	if !syslogRemoteAvail() {
		return ""
	}
	b, err := os.ReadFile(syslogRemotePath())
	if err != nil {
		return ""
	}
	v, err := syslogRemoteNorm(strings.SplitN(string(b), "\n", 2)[0])
	if err != nil {
		return ""
	}
	return v
}

// syslogRemoteMu: one change of the setting and its syslogd restart at a time (two overlapping restarts could
// each start a syslogd; the boot script's step takes its own lock as well).
var syslogRemoteMu sync.Mutex

// syslogRemoteSet: store the setting and restart syslogd for it.
func syslogRemoteSet(s string) error {
	syslogRemoteMu.Lock()
	defer syslogRemoteMu.Unlock()
	if !syslogRemoteAvail() {
		return fmt.Errorf("sending the system log to another computer isn't available in this build")
	}
	v, err := syslogRemoteNorm(s)
	if err != nil {
		return err
	}
	if v == "" {
		if err := cfgRemove(syslogRemotePath()); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("could not turn it off: %s", cfgSaveErr(err))
		}
	} else if err := cfgWrite(syslogRemotePath(), []byte(v+"\n"), 0o644); err != nil {
		return fmt.Errorf("could not save it: %s", cfgSaveErr(err))
	}
	if err := syslogRemoteApply(); err != nil {
		syslogApplyErrSet(err.Error())
		return fmt.Errorf("saved, but %v: the system log may not be sent as set (the boot script applies it again at the next start)", err)
	}
	syslogApplyErrSet("")
	if v == "" {
		activity.emit("📜 the system log is no longer sent to another computer")
	} else {
		activity.emit("📜 the system log is also sent to " + v + " (UDP syslog)")
	}
	return nil
}

// syslogApplyErr: the last restart of syslogd for the setting failed (shown on the card instead of "On"); "" = it
// worked (or nothing was changed since the start).
var (
	syslogApplyMu  sync.Mutex
	syslogApplyErr string
)

func syslogApplyErrSet(e string) { syslogApplyMu.Lock(); syslogApplyErr = e; syslogApplyMu.Unlock() }
func syslogApplyErrGet() string {
	syslogApplyMu.Lock()
	defer syslogApplyMu.Unlock()
	return syslogApplyErr
}
