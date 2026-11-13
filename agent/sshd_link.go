package main

// The SC Box II's SSH is the separate program scbox2-sshd (cmd/scbox2-sshd): a damaged or crashing panel can't
// take SSH with it. What the panel still does for SSH there (sshViaDaemon):
//   - Settings -> SSH on/off and port are saved to net.json as before, then scbox2-sshd is sent SIGHUP and the
//     panel waits (briefly) until it has applied them; it also re-reads them by itself every 2 s.
//   - The status (on, listening, port, host key fingerprint, an error) comes from scbox2-sshd's state file.
//   - A password changed here (a new signing key in auth.json) or access from outside switched off: scbox2-sshd
//     is signalled and closes the connections that may no longer stay (it notices by itself within 2 s anyway).
//   - A password changed over SSH (the factory password's forced change) is written to auth.json by scbox2-sshd:
//     the panel re-reads the file when it changed on disk (authFollowFile).
//   - Sign-ins, blocks and that password change reach the activity log and notifications through scbox2-sshd's
//     events file.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// sshViaDaemon: SSH is scbox2-sshd's, not the panel's. Set at start on the SC Box II builds (main); off in tests
// unless a test sets it.
var sshViaDaemon bool

var (
	sshdName       = brand + "-sshd"
	sshdStatePath  = brandTmp + "sshd.json"
	sshdEventsPath = brandTmp + "sshd.events"
	sshdApplyWait  = 3 * time.Second
	// sshdIsOurs: is pid the SSH server (and not a reused pid)? Its /proc comm is the program's name.
	sshdIsOurs = func(pid int) bool {
		b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/comm")
		return err == nil && strings.TrimSpace(string(b)) == sshdName
	}
)

// sshdStateInfo: scbox2-sshd's state file (cmd/scbox2-sshd server.go state).
type sshdStateInfo struct {
	Pid         int    `json:"pid"`
	Enabled     bool   `json:"enabled"`
	Port        int    `json:"port"`
	Listening   bool   `json:"listening"`
	Err         string `json:"err"`
	Fingerprint string `json:"fingerprint"`
	Applied     int64  `json:"applied"`
}

// sshdState: the state scbox2-sshd last wrote, and whether it is running now.
func sshdState() (sshdStateInfo, bool) {
	var st sshdStateInfo
	b, err := os.ReadFile(sshdStatePath)
	if err != nil || json.Unmarshal(b, &st) != nil || st.Pid <= 0 {
		return st, false
	}
	return st, sshdIsOurs(st.Pid)
}

// sshdPoke: tell scbox2-sshd to re-read the settings and the login file now. false: it isn't running.
func sshdPoke() bool {
	st, ok := sshdState()
	if !ok {
		return false
	}
	return syscall.Kill(st.Pid, syscall.SIGHUP) == nil
}

// sshdApply: the settings were saved: scbox2-sshd applies them, waited for at most sshdApplyWait (so the answer
// shows the new state).
func sshdApply() {
	t0 := time.Now().UnixNano()
	if !sshdPoke() {
		return
	}
	for end := time.Now().Add(sshdApplyWait); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		if st, ok := sshdState(); !ok || st.Applied >= t0 {
			return
		}
	}
}

func sshdStatus() map[string]any {
	c := currentNetConfig()
	st, running := sshdState()
	authMu.RLock()
	user := auth.Username
	authMu.RUnlock()
	port, errMsg := sshPort(c), st.Err
	if running && st.Listening && st.Port > 0 {
		port = st.Port // where it really listens
	}
	if !running {
		errMsg = "the SSH server (" + sshdName + ") isn't running"
		if !sshEnabled(c) {
			errMsg = ""
		}
	}
	return map[string]any{"ok": true, "enabled": sshEnabled(c), "port": port,
		"listening": running && st.Listening, "err": errMsg, "fingerprint": sshFingerprint(), "username": user,
		"available": true}
}

// ---------- the login file, changed by scbox2-sshd ----------

var (
	authFileMu      sync.Mutex
	authFileLast    []byte    // auth.json as this panel last read or wrote it
	authFileChecked time.Time // (requests check at most once a second)
)

// authSeen: b is auth.json as the panel just read or wrote it (loadAuth, saveAuth).
func authSeen(b []byte) {
	authFileMu.Lock()
	authFileLast = append([]byte(nil), b...)
	authFileMu.Unlock()
}

// authFollowFile: auth.json changed on disk (scbox2-sshd's forced password change): use it. A file that isn't a
// usable login (damaged, half written) is left alone: the panel's own copy stays. force: now, else at most once a
// second. Only with sshViaDaemon (nothing else writes the file).
func authFollowFile(force bool) {
	if !sshViaDaemon {
		return
	}
	authFileMu.Lock()
	defer authFileMu.Unlock()
	if !force && time.Since(authFileChecked) < time.Second {
		return
	}
	authFileChecked = time.Now()
	b, err := os.ReadFile(*authFile)
	if err != nil || bytes.Equal(b, authFileLast) {
		return
	}
	var a authStore
	if json.Unmarshal(b, &a) != nil || !authUsable(&a) {
		return
	}
	authFileLast = b
	authMu.Lock()
	oldSecret := auth.Secret
	auth = a
	authMu.Unlock()
	if a.Secret != oldSecret {
		forgetRevoked() // a new signing key ends every older session anyway
	}
	log.Printf("auth: %s was changed by %s: reloaded", *authFile, sshdName)
}

// ---------- scbox2-sshd's events ----------

// sshdEventsLoop: every 2 s, the login file and scbox2-sshd's new events. Events from before the panel started
// are skipped (already shown, or from a panel that was down).
func sshdEventsLoop() {
	var off int64
	if fi, err := os.Stat(sshdEventsPath); err == nil {
		off = fi.Size()
	}
	for {
		time.Sleep(2 * time.Second)
		authFollowFile(true)
		off = sshdReadEvents(off)
	}
}

// sshdReadEvents: the complete lines after off, acted on; the new offset (the file starts over when it grows
// too long: shorter than off means from the top).
func sshdReadEvents(off int64) int64 {
	f, err := os.Open(sshdEventsPath)
	if err != nil {
		return 0
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return off
	}
	if fi.Size() < off {
		off = 0
	}
	if fi.Size() == off {
		return off
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return off
	}
	data, _ := io.ReadAll(io.LimitReader(f, 256<<10))
	last := bytes.LastIndexByte(data, '\n')
	if last < 0 {
		return off
	}
	for _, line := range bytes.Split(data[:last], []byte{'\n'}) {
		var ev struct {
			Ev   string `json:"ev"`
			User string `json:"user"`
			IP   string `json:"ip"`
			Msg  string `json:"msg"`
			N    int    `json:"n"`
		}
		if json.Unmarshal(line, &ev) != nil {
			continue
		}
		switch ev.Ev {
		case "signin":
			connMarkOwner(ev.IP)
			activity.emit(fmt.Sprintf("🔑 SSH sign-in: %s from %s", ev.User, ev.IP))
		case "block":
			activity.emit("🔒 " + ev.Msg)
			go notify(evLogin, ev.Msg)
		case "pwchange":
			authFollowFile(true)
			activity.emit("🔑 password changed (first SSH sign-in)")
		case "pwdrop":
			activity.emit(fmt.Sprintf("🔑 password changed: %d SSH connection(s) closed", ev.N))
		}
	}
	return off + int64(last+1)
}
