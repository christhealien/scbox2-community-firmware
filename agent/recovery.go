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
	"os"
	"os/signal"
	"path/filepath"
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
	wdtSigOnce                 sync.Once
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
// flush the hot files that are kept in RAM and only checkpointed ~hourly on the Box II (to spare the FAT
// flash): a reboot would otherwise lose up to an hour of hashrate history and events. Cheap and harmless
// on SC Lite, where these are already written frequently.
func markCleanReboot() {
	persistHashrate()
	eventsSave()
	os.WriteFile(recCleanPath(), []byte(time.Now().Format(time.RFC3339)), 0644)
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
	wdtSigOnce.Do(func() { // the boot script's stop / shutdown: disarm first, then exit as before
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM)
		go func() {
			<-sig
			watchdogStop()
			os.Exit(0)
		}()
	})
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

func saveUnexpected() {
	if recUnexp == nil {
		return
	}
	b, _ := json.Marshal(recUnexp)
	cfgWrite(recUnexpPath(), b, 0644)
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
		// ~10 min, instead of every 1 and 2 min. On SC Lite's ext the original cadence is kept.
		aliveMod := int(cfgCheckpointGap(5*time.Minute, time.Minute) / time.Minute)
		keepMod := int(cfgCheckpointGap(10*time.Minute, 2*time.Minute) / time.Minute)
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
	if wdtErr != "" {
		v["watchdog_err"] = wdtErr
	}
	if fi, err := os.Stat(keptLogBeforePath()); err == nil {
		v["kept_before"] = fi.ModTime().Unix()
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
	}
	if !decodeIn(w, r, &in) {
		return
	}
	if in.Dismiss {
		recMu.Lock()
		if recUnexp != nil {
			recUnexp.Dismissed = true
			saveUnexpected()
		}
		recMu.Unlock()
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
		writeJSON(w, map[string]any{"ok": false, "err": "could not save: " + err.Error()})
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
