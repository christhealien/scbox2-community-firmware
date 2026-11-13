package main

// The OEM's over-temperature shutdown (fanctrl, traced): every 6 s it reads each board's sensor; over
// tempcutoff (product.json: 92 °C on the SC Lite, 86 °C on the SC Box II) all fans go to 100 %, and after
// more than 5 readings in a row over it logs "Disable CPB, For Temperature Is Greater Than <cutoff>",
// powers the boards down, kills the mining program and sets miner_state 0x80. Only a reboot (or a restart
// of the OEM's watchdog, which the panel therefore refuses while this shows) clears 0x80, so the watchdog
// keeps the miner on Idle until then, and nothing on the Dashboard said why. The cut-off temperature is
// read from fanctrl's own log line (the regex captures it), never assumed, so this works on any model. This
// finds that line (since the current boot) in fanctrl's own log and shows it.

import (
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

type overTempState struct {
	At    int64 `json:"at"`    // when fanctrl shut the boards down
	Limit int   `json:"limit"` // its cut-off (°C)
}

var (
	reOverTemp  = regexp.MustCompile(`\[(\d{4}-\d\d-\d\d \d\d:\d\d:\d\d)\].*Disable CPB, For Temperature Is Greater Than (\d+)`)
	otMu        sync.Mutex
	otCached    *overTempState
	otCheckedAt time.Time
	otBootTime  = bootTime
)

// parseOverTemp: the last shutdown line at or after boot, or nil. fanctrl stamps its lines with the system
// clock, which reads 1970 after a boot until NTP or the browser sets it: a shutdown logged then is of this boot
// when it comes after fanctrl's last start line (a 1970 line can't be compared with a boot time on a set clock).
func parseOverTemp(t string, boot int64) *overTempState {
	var last *overTempState
	lines := strings.Split(t, "\n")
	started := -1 // fanctrl's last "start" line
	for i, l := range lines {
		if strings.Contains(l, "fanctrl start") {
			started = i
		}
	}
	for i, l := range lines {
		m := reOverTemp.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		ts, err := time.ParseInLocation("2006-01-02 15:04:05", m[1], time.Local)
		if err != nil {
			continue
		}
		thisBoot := ts.Unix() >= boot && boot >= clockSetUnix // (a set clock's line: by the boot time)
		if ts.Unix() < clockSetUnix {
			thisBoot = started >= 0 && i > started
		}
		if !thisBoot {
			continue
		}
		lim, _ := strconv.Atoi(m[2])
		last = &overTempState{At: ts.Unix(), Limit: lim}
	}
	return last
}

func overTempView() *overTempState {
	otMu.Lock()
	defer otMu.Unlock()
	if time.Since(otCheckedAt) < 30*time.Second {
		return otCached
	}
	otCheckedAt = time.Now()
	otCached = nil
	for _, gl := range oemLogs() {
		if !strings.Contains(gl[0], "fanctrl") {
			continue
		}
		if t, err := tailFile(gl[1], 256<<10); err == nil {
			otCached = parseOverTemp(t, otBootTime())
		}
	}
	return otCached
}
