package main

// MAC address setting (Settings -> Network). /etc/sclite-mac applies it at boot, right after Goldshell's
// maccfg and before eth0 comes up: keep (default: the first address seen, saved), random (new each boot) or
// fixed (typed here). Changes apply at the next reboot.
//
// eth0 only. Both SC Lite and the SC Box II are wired-only (no Wi-Fi chip, so no Wi-Fi interface): the one
// network interface is eth0, and macIfPath reads the Box II's own /sys/class/net/eth0/address unchanged. On the Box
// II dhcpcd identifies the miner to the router by this MAC (clientid, not a DUID the squashfs/wiped config
// can't persist), so "keep" (the default) is what gives the unit a stable DHCP lease across reboots.
//
// The saved setting lives on the config store (/mnt/config on the Box II, a small FAT that can tear on a
// power cut), so the writes go through writeAtomic (temp + fsync + rename) rather than a plain os.WriteFile.

import (
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var (
	macRe       = regexp.MustCompile(`^[0-9a-f]{2}(:[0-9a-f]{2}){5}$`)
	macIfPath   = "/sys/class/net/eth0/address"
	macBootPath = brandTmp + "mac-boot"
)

func macDir() string { return filepath.Dir(*authFile) }

func readTrim(p string) string {
	b, _ := os.ReadFile(p)
	return strings.ToLower(strings.TrimSpace(string(b)))
}

func validMAC(m string) bool {
	if !macRe.MatchString(m) || m == "00:00:00:00:00:00" || m == "ff:ff:ff:ff:ff:ff" {
		return false
	}
	var first int
	for _, c := range m[:2] {
		first <<= 4
		switch {
		case c >= '0' && c <= '9':
			first |= int(c - '0')
		default:
			first |= int(c-'a') + 10
		}
	}
	return first&1 == 0 // unicast
}

func macView() map[string]any {
	mode := readTrim(filepath.Join(macDir(), "macmode"))
	if mode != "random" && mode != "fixed" {
		mode = "keep"
	}
	return map[string]any{"ok": true, "mode": mode, "current": readTrim(macIfPath),
		"saved": readTrim(filepath.Join(macDir(), "mac")), "hardware": readTrim(macBootPath),
		"boot_step": fileExists(brandMacScript)}
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

// GET: the setting and the addresses. POST {mode, mac}: save; applies at the next reboot.
func handleMAC(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var in struct {
			Mode string `json:"mode"`
			MAC  string `json:"mac"`
		}
		if !decodeIn(w, r, &in) {
			return
		}
		mac := strings.ToLower(strings.TrimSpace(strings.ReplaceAll(in.MAC, "-", ":")))
		switch in.Mode {
		case "keep", "random":
		case "fixed":
			if !validMAC(mac) {
				writeJSON(w, map[string]any{"ok": false, "err": "enter a unicast address like 3c:2a:f4:01:02:03 (the first byte must be even)"})
				return
			}
		default:
			writeJSON(w, map[string]any{"ok": false, "err": "unknown mode"})
			return
		}
		os.MkdirAll(macDir(), 0o755)
		if in.Mode == "fixed" {
			if err := cfgWrite(filepath.Join(macDir(), "mac"), []byte(mac+"\n"), 0o644); err != nil {
				writeJSON(w, map[string]any{"ok": false, "err": err.Error()})
				return
			}
		} else if in.Mode == "keep" {
			// keep the address the miner has now
			if cur := readTrim(macIfPath); validMAC(cur) {
				cfgWrite(filepath.Join(macDir(), "mac"), []byte(cur+"\n"), 0o644)
			}
		}
		if err := cfgWrite(filepath.Join(macDir(), "macmode"), []byte(in.Mode+"\n"), 0o644); err != nil {
			writeJSON(w, map[string]any{"ok": false, "err": err.Error()})
			return
		}
		activity.emit("🖧 MAC address setting: " + in.Mode + map[bool]string{true: " " + mac, false: ""}[in.Mode == "fixed"] + " (applies at the next reboot)")
	}
	writeJSON(w, macView())
}
