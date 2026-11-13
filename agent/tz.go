package main

// Time zone and clock.
//
// The OEM's firmware is fixed to China time (/etc/localtime = Asia/Shanghai, UTC+8), and everything on
// the miner reads it: the miner's log times, and this agent's schedule. In this firmware /etc/localtime
// points to /usr/config/sclite/localtime, which the agent writes when a time zone is chosen (by default
// the browser's, on first sign-in). Zone data is the IANA tz database (zoneinfo.zip, embedded, built by
// firmware-kit/tools/make-zoneinfo.sh with "zic -b fat"); each file is TZif2 with the POSIX rule at the
// end, the format the miner's uClibc reads. When a newer firmware brings new rules, the saved system file
// is refreshed at start.
//
// The clock itself comes from NTP. If the miner can't reach a time server, the panel sets it from the
// browser while it's still unset (1970).

import (
	"archive/zip"
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// tzdataVersion: the IANA tz database release zoneinfo.zip was built from (firmware-kit/tools/make-zoneinfo.sh).
// 2026f has British Columbia on permanent -07 from 2026-11-01 (tzdata 2026b) and Cuba's 2026 change.
const tzdataVersion = "2026f"

//go:embed zoneinfo.zip
var zoneinfoZip []byte

var (
	tzMu    sync.Mutex
	tzZip   *zip.Reader
	tzNames []string
)

func tzDB() (*zip.Reader, []string) {
	tzMu.Lock()
	defer tzMu.Unlock()
	if tzZip == nil {
		r, err := zip.NewReader(bytes.NewReader(zoneinfoZip), int64(len(zoneinfoZip)))
		if err != nil {
			return nil, nil
		}
		tzZip = r
		for _, f := range r.File {
			n := f.Name
			// the zones people pick: Region/City, plus UTC
			if n == "UTC" || (strings.Contains(n, "/") && !strings.HasPrefix(n, "Etc/") && !strings.HasPrefix(n, "posix") &&
				!strings.HasPrefix(n, "right") && !strings.HasPrefix(n, "SystemV/") && !strings.HasPrefix(n, "US/") &&
				!strings.HasPrefix(n, "Canada/") && !strings.HasPrefix(n, "Mexico/") && !strings.HasPrefix(n, "Brazil/") &&
				!strings.HasPrefix(n, "Chile/")) {
				tzNames = append(tzNames, n)
			}
		}
		sort.Strings(tzNames)
	}
	return tzZip, tzNames
}

func tzData(name string) ([]byte, error) {
	r, _ := tzDB()
	if r == nil {
		return nil, errors.New("time zone database unavailable")
	}
	for _, f := range r.File {
		if f.Name == name {
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			defer rc.Close()
			return io.ReadAll(rc)
		}
	}
	return nil, fmt.Errorf("unknown time zone %q", name)
}

func tzFilePath() string  { return filepath.Join(filepath.Dir(*authFile), "localtime") }
func tzStatePath() string { return filepath.Join(filepath.Dir(*authFile), "timezone.json") }

type tzState struct {
	Zone string `json:"zone"`
	By   string `json:"by,omitempty"` // "browser" (picked up automatically) or "settings"
}

func loadTZState() tzState {
	var s tzState
	if b, err := os.ReadFile(tzStatePath()); err == nil {
		json.Unmarshal(b, &s)
	}
	return s
}

// applyTZ makes name the miner's time zone: the system file (/etc/localtime points to it), this agent's
// clock, and the saved choice.
func applyTZ(name, by string) error {
	data, err := tzData(name)
	if err != nil {
		return err
	}
	loc, err := time.LoadLocationFromTZData(name, data)
	if err != nil {
		return err
	}
	p := tzFilePath()
	os.MkdirAll(filepath.Dir(p), 0o755)
	tzWriteMu.Lock() // two changes at once mustn't share the temp file
	defer tzWriteMu.Unlock()
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		return err
	}
	b, _ := json.Marshal(tzState{Zone: name, By: by})
	writeAtomic(tzStatePath(), b, 0o644)
	time.Local = loc
	return nil
}

// loadTZ at start: use the saved zone; without one, the system's (the firmware's default).
var tzWriteMu sync.Mutex

func loadTZ() {
	if s := loadTZState(); s.Zone != "" {
		if data, err := tzData(s.Zone); err == nil {
			if loc, err := time.LoadLocationFromTZData(s.Zone, data); err == nil {
				time.Local = loc
				// the system file (the miner's own log follows it): put it back if it went missing, and refresh it
				// when this firmware's zone data is newer than the copy saved before (time zone rules change)
				if old, err := os.ReadFile(tzFilePath()); err != nil || !bytes.Equal(old, data) {
					tmp := tzFilePath() + ".tmp"
					if os.WriteFile(tmp, data, 0o644) == nil {
						os.Rename(tmp, tzFilePath())
					}
				}
			}
		}
	}
}

func tzView() map[string]any {
	s := loadTZState()
	now := time.Now()
	zone := s.Zone
	if zone == "" {
		zone = "firmware default (China, UTC+8)"
	}
	return map[string]any{"ok": true, "zone": zone, "zone_set": s.Zone != "", "by": s.By,
		"offset": now.Format("-07:00"), "abbr": now.Format("MST"), "now": now.Unix(),
		"local": now.Format("2006-01-02 15:04:05"), "ntp": ntpView(), "tzdata": tzdataVersion,
		// plausible: the clock reads 2024 or later (it was set, by a time server or by hand); "synced", its old
		// name, said more than it knew and stays for older pages
		"plausible": clockOK(), "synced": clockOK(), "manual_clock": secHardened}
}

// ntpConfigured: a time server that will correct a hand-set clock (one typed in, the router's, or the public pool).
func ntpConfigured() bool {
	return len(ntpFields(ntpConfPath())) > 0 || len(ntpFields(ntpDHCPPath)) > 0 || ntpPoolOn()
}

// GET: zone and clock (?zones=1 adds the list). POST {zone, by} sets the zone; {clock: unix seconds} sets
// the clock, only while it's still unset (NTP hasn't reached a server); SC Box II: {clock, manual: true} sets
// it by hand from Settings -> Time at any time (2024-01-01 .. 2040-01-01).
// clockSet sets the system clock (tests replace it).
var clockSet = syscall.Settimeofday

func handleTime(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var in struct {
			Zone  string  `json:"zone"`
			By    string  `json:"by"`
			Clock int64   `json:"clock"`
			NTP   *string `json:"ntp"`
			Pool  *bool   `json:"ntp_pool"`
			Sync  bool    `json:"sync"`
			// Manual: the owner set the date and time by hand (Settings -> Time), allowed even when the clock
			// already reads a plausible time (SC Box II)
			Manual bool `json:"manual"`
		}
		if !decodeIn(w, r, &in) {
			return
		}
		if in.Zone != "" {
			by := "settings"
			if in.By == "browser" {
				by = "browser"
				if loadTZState().Zone != "" { // automatic pick-up only when nothing was chosen yet
					writeJSON(w, tzView())
					return
				}
			}
			old := time.Local.String()
			if err := applyTZ(in.Zone, by); err != nil {
				writeJSON(w, map[string]any{"ok": false, "err": err.Error()})
				return
			}
			activity.emit(fmt.Sprintf("🕒 time zone set to %s (was %s)", in.Zone, old))
		}
		if in.NTP != nil {
			if e := setNTPServers(*in.NTP); e != "" {
				writeJSON(w, map[string]any{"ok": false, "err": e})
				return
			}
			if strings.TrimSpace(*in.NTP) == "" {
				activity.emit("🕒 time servers: the router's")
			} else {
				activity.emit("🕒 time servers set to " + strings.TrimSpace(*in.NTP))
			}
		}
		if in.Pool != nil {
			if err := setNTPPool(*in.Pool); err != nil {
				writeJSON(w, map[string]any{"ok": false, "err": err.Error()})
				return
			}
			if *in.Pool {
				activity.emit("🕒 public time servers (pool.ntp.org) allowed when no other time server answers")
			} else {
				activity.emit("🕒 public time servers (pool.ntp.org) off")
			}
		}
		if in.Sync {
			ntpSyncNow()
		}
		if in.Clock > 0 && in.Manual && secHardened {
			if in.Clock < time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC).Unix() || in.Clock > time.Date(2040, 1, 1, 0, 0, 0, 0, time.UTC).Unix() {
				writeJSON(w, map[string]any{"ok": false, "err": "that date is outside 2024-01-01 .. 2040-01-01: not set"})
				return
			}
			old := time.Now()
			tv := syscall.NsecToTimeval(in.Clock * 1e9)
			if err := clockSet(&tv); err != nil {
				writeJSON(w, map[string]any{"ok": false, "err": "could not set the clock: " + err.Error()})
				return
			}
			activity.emit(fmt.Sprintf("🕒 clock set by hand to %s (it read %s)", time.Unix(in.Clock, 0).Format("2006-01-02 15:04:05 MST"),
				old.Format("2006-01-02 15:04:05")))
			v := tzView()
			v["note"] = "clock set"
			if ntpConfigured() {
				v["note"] = "clock set; the time servers correct it at their next sync"
			}
			writeJSON(w, v)
			return
		}
		if in.Clock > 0 {
			if clockOK() {
				writeJSON(w, map[string]any{"ok": false, "err": "the clock is already set (by the time server)"})
				return
			}
			if in.Clock < time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC).Unix() || in.Clock > time.Date(2040, 1, 1, 0, 0, 0, 0, time.UTC).Unix() {
				writeJSON(w, map[string]any{"ok": false, "err": "that time looks wrong"})
				return
			}
			tv := syscall.NsecToTimeval(in.Clock * 1e9)
			if err := clockSet(&tv); err != nil {
				writeJSON(w, map[string]any{"ok": false, "err": "could not set the clock: " + err.Error()})
				return
			}
			activity.emit("🕒 clock set from the browser (no time server reached yet)")
		}
	}
	v := tzView()
	if r.URL.Query().Get("zones") == "1" {
		_, names := tzDB()
		v["zones"] = names
	}
	writeJSON(w, v)
}
