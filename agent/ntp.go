package main

// Time servers (Settings -> Time). The firmware's NTP loop (/usr/libexec/sclite/ntp) uses, together: the
// servers set here and the ones the router hands out by DHCP (saved by a dhcpcd hook). Only when those give
// no time, the public pool.ntp.org, unless the owner switched it off here (the file "ntp-pool" = "off"): then
// nothing reaches the internet for the time. It writes what it last used; the panel shows it and can
// ask for a sync now.

import (
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

var (
	ntpDHCPPath = brandTmp + "ntp-dhcp"
	ntpLastPath = brandTmp + "ntp-last"
	ntpFailPath = brandTmp + "ntp-fail"
	ntpNowPath  = brandTmp + "ntp-now"
	ntpNameRe   = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$`)
)

func ntpConfPath() string { return filepath.Join(filepath.Dir(*authFile), "ntp") }

func ntpPoolPath() string { return filepath.Join(filepath.Dir(*authFile), "ntp-pool") }

// ntpPoolOn: the public pool (pool.ntp.org) may be used as the last resort. On unless the owner switched it off
// (a miner whose router hands out no time server would otherwise never get the time: TLS, schedules, logs).
func ntpPoolOn() bool {
	b, _ := os.ReadFile(ntpPoolPath())
	return strings.TrimSpace(string(b)) != "off"
}

func setNTPPool(on bool) error {
	if on {
		if err := os.Remove(ntpPoolPath()); err != nil && !os.IsNotExist(err) {
			return err
		}
		ntpSyncNow()
		return nil
	}
	return cfgWrite(ntpPoolPath(), []byte("off\n"), 0o644)
}

func ntpFields(p string) []string { b, _ := os.ReadFile(p); return strings.Fields(string(b)) }

// "<unix time> server server ..." -> time, servers
func ntpStamp(p string) (int64, []string) {
	f := ntpFields(p)
	if len(f) == 0 {
		return 0, nil
	}
	t, _ := strconv.ParseInt(f[0], 10, 64)
	return t, f[1:]
}

func ntpView() map[string]any {
	lt, ls := ntpStamp(ntpLastPath)
	ft, fs := ntpStamp(ntpFailPath)
	v := map[string]any{"servers": strings.Join(ntpFields(ntpConfPath()), " "), "dhcp": ntpFields(ntpDHCPPath), "pool": ntpPoolOn()}
	if lt > 0 {
		v["last"], v["last_from"] = lt, ls
	}
	if ft > lt {
		v["fail"], v["fail_from"] = ft, strings.Join(fs, " ")
	}
	return v
}

// setNTPServers saves up to three servers (names or IPv4 addresses) and asks the loop to sync now.
func setNTPServers(s string) string {
	f := strings.Fields(strings.ReplaceAll(s, ",", " "))
	if len(f) > 3 {
		return "at most three time servers"
	}
	for _, x := range f {
		if !ntpNameRe.MatchString(x) {
			return "\"" + x + "\" isn't a server name or address"
		}
		// digits and dots only: an IPv4 address or a typo. The miner's ntpd quits on one it can't use ("bad
		// address"), and the router's server, tried in the same run, goes with it.
		if strings.Trim(x, "0123456789.") == "" && (net.ParseIP(x) == nil || strings.Count(x, ".") != 3) {
			return "\"" + x + "\" isn't an IPv4 address (four numbers 0-255, like 192.168.1.2)"
		}
	}
	if err := cfgWrite(ntpConfPath(), []byte(strings.Join(f, " ")+"\n"), 0o644); err != nil {
		return err.Error()
	}
	ntpSyncNow()
	return ""
}

func ntpSyncNow() { os.WriteFile(ntpNowPath, nil, 0o644) }
