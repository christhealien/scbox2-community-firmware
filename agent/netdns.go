package main

// DNS override (Settings -> Network, "dns" field; see handleNet / netConfig.DNS).
//
// Why this exists. On the SC Box II the stock web backend (minerd) owns /etc/resolv.conf whenever a static
// IP is configured, and its hard-coded static-IP default resolver is 114.114.114.114 — a public DNS service
// in China (R06: string at minerd 0x1ff0d, `echo "nameserver 114.114.114.114" > /etc/resolv.conf`). The kit
// (A3) neutralises that command in the binary so minerd can no longer write it, but that patch alone leaves a
// static-IP miner with NO resolver at all. This override closes the gap from the panel side: the owner can
// name their own DNS server(s), and failing that the miner falls back to its default gateway — never to a
// resolver the owner didn't choose. As belt-and-braces it also strips 114.114.114.114 (and its 114.114.115.115
// sibling) from resolv.conf should it ever appear, so the Chinese default can never take effect.
//
// resolv.conf is /etc/resolv.conf, which on both the SC Lite and the Box II is a symlink into the writable
// tmpfs (/tmp/resolv.conf); the squashfs /etc itself is read-only, so writes follow the symlink to its real
// target. In DHCP mode dhcpcd fills resolv.conf from the router; we do not fight that unless a blocked
// resolver is present or the owner set an explicit override (which then wins, as the owner intends).

import (
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// blockedResolvers: resolvers the firmware must never leave in resolv.conf. 114.114.114.114 is minerd's
// hard-coded static-IP default (and 114.114.115.115 its documented secondary); both are 114DNS (China).
var blockedResolvers = map[string]bool{
	"114.114.114.114": true,
	"114.114.115.115": true,
}

// procRoute is the default-gateway source (/proc/net/route); a var so tests can point it at a fixture.
var procRoute = "/proc/net/route"

// maxDNSServers caps how many resolvers the owner may set (matches minerd's DNS1/DNS2 plus a little room).
const maxDNSServers = 3

// cleanDNSList parses, validates and normalises the owner's DNS override string (space/comma separated). It
// returns the cleaned, space-joined list ("" means "no override"), or an error with a message fit for the UI.
// Only IPv4 unicast resolvers are accepted (minerd's static-IP DNS path is IPv4), and the Chinese default is
// refused outright so it can never be shipped or re-entered.
func cleanDNSList(s string) (string, error) {
	f := strings.Fields(strings.ReplaceAll(s, ",", " "))
	if len(f) > maxDNSServers {
		return "", fmt.Errorf("at most %d DNS servers", maxDNSServers)
	}
	out := make([]string, 0, len(f))
	seen := map[string]bool{}
	for _, x := range f {
		ip := net.ParseIP(x)
		if ip == nil || ip.To4() == nil {
			return "", fmt.Errorf("%q isn't an IPv4 address (four numbers 0-255, like 192.168.1.1)", x)
		}
		v := ip.To4().String()
		if blockedResolvers[v] {
			return "", fmt.Errorf("%s is a public DNS server in China that the stock firmware forces on static IP; this firmware never uses it — set your router/gateway or a resolver you trust", v)
		}
		if ip.IsLoopback() || ip.IsUnspecified() || ip.IsMulticast() {
			return "", fmt.Errorf("%s can't be a DNS server", v)
		}
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return strings.Join(out, " "), nil
}

// resolvConfTarget returns the path to actually write: if resolvConf is a symlink (it is: /etc/resolv.conf ->
// ../tmp/resolv.conf on these boxes), follow it to its real, writable target so the temp-file-plus-rename in
// writeAtomic lands on tmpfs and not the read-only squashfs /etc.
func resolvConfTarget() string {
	p := resolvConf
	if fi, err := os.Lstat(p); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		if dst, err := os.Readlink(p); err == nil {
			if !filepath.IsAbs(dst) {
				dst = filepath.Join(filepath.Dir(p), dst)
			}
			return filepath.Clean(dst)
		}
	}
	return p
}

// readResolv returns the nameserver IPs currently in resolv.conf (in order) and the other lines verbatim
// (search/domain/options/comments), so a rewrite preserves the DHCP-given search domain that the DNS-rebinding
// guard (localDomains) relies on.
func readResolv() (ns []string, other []string) {
	b, err := os.ReadFile(resolvConf)
	if err != nil {
		return nil, nil
	}
	for _, ln := range strings.Split(string(b), "\n") {
		t := strings.TrimSpace(ln)
		if t == "" {
			continue
		}
		if f := strings.Fields(t); len(f) >= 2 && f[0] == "nameserver" {
			ns = append(ns, f[1])
			continue
		}
		other = append(other, ln)
	}
	return ns, other
}

// activeResolvers reports the non-blocked nameservers currently in resolv.conf (for the UI/status).
func activeResolvers() []string {
	ns, _ := readResolv()
	out := make([]string, 0, len(ns))
	for _, s := range ns {
		if !blockedResolvers[strings.TrimSpace(s)] {
			out = append(out, s)
		}
	}
	return out
}

// writeResolv writes resolv.conf with the given nameservers first (in order) followed by the preserved other
// lines. It never writes a blocked resolver. It writes through writeAtomic (temp + fsync + rename) so a power
// cut can't leave a half-written file.
func writeResolv(ns []string, other []string) error {
	var b strings.Builder
	for _, s := range ns {
		s = strings.TrimSpace(s)
		if s == "" || blockedResolvers[s] {
			continue
		}
		b.WriteString("nameserver ")
		b.WriteString(s)
		b.WriteByte('\n')
	}
	for _, ln := range other {
		b.WriteString(ln)
		b.WriteByte('\n')
	}
	return writeAtomic(resolvConfTarget(), []byte(b.String()), 0o644)
}

// defaultGatewayV4 reads the IPv4 default gateway (destination 0.0.0.0) from /proc/net/route, or "" if none.
// The gateway is the safest universal fallback resolver: most home routers also answer DNS on their own
// address, and it is on the owner's own network, never the internet at large.
func defaultGatewayV4() string {
	b, err := os.ReadFile(procRoute)
	if err != nil {
		return ""
	}
	lines := strings.Split(string(b), "\n")
	for _, ln := range lines[1:] { // skip the header row
		f := strings.Fields(ln)
		if len(f) < 3 {
			continue
		}
		// columns: Iface Destination Gateway Flags ... ; default route has Destination 00000000 and the
		// RTF_GATEWAY flag (0x2) set. Gateway is a little-endian hex IPv4.
		if f[1] != "00000000" {
			continue
		}
		gw := parseHexIPv4LE(f[2])
		if gw != "" && gw != "0.0.0.0" {
			return gw
		}
	}
	return ""
}

// parseHexIPv4LE turns an 8-hex-digit little-endian word (as /proc/net/route prints addresses) into dotted
// IPv4, e.g. "0101A8C0" -> "192.168.1.1".
func parseHexIPv4LE(h string) string {
	if len(h) != 8 {
		return ""
	}
	var o [4]int
	for i := 0; i < 4; i++ {
		var v int
		for _, c := range h[i*2 : i*2+2] {
			v <<= 4
			switch {
			case c >= '0' && c <= '9':
				v |= int(c - '0')
			case c >= 'a' && c <= 'f':
				v |= int(c-'a') + 10
			case c >= 'A' && c <= 'F':
				v |= int(c-'A') + 10
			default:
				return ""
			}
		}
		o[3-i] = v // little-endian: first byte is the low octet
	}
	return fmt.Sprintf("%d.%d.%d.%d", o[0], o[1], o[2], o[3])
}

var dnsMu sync.Mutex

// ensureSaneDNS brings resolv.conf into line with policy. Called at start-up and whenever the DNS setting
// changes; the dnsGuard goroutine re-asserts it periodically. The rules, in order:
//
//   - owner set an override  -> resolv.conf's nameservers become exactly that list (dropping anything else),
//     so the owner's choice wins over whatever minerd/dhcpcd wrote. search/domain lines are preserved.
//   - no override, a blocked resolver (114.114.114.114) is present -> strip it; if that empties the list,
//     substitute the default gateway (a resolver on the owner's own network), else leave the rest.
//   - no override, nothing blocked -> do nothing (don't disturb the router's DHCP-provided DNS).
//
// It never writes 114.114.114.114, and only writes when the file would actually change.
func ensureSaneDNS(cfg netConfig) error {
	dnsMu.Lock()
	defer dnsMu.Unlock()

	override, _ := cleanDNSList(cfg.DNS) // already validated on save; re-clean defensively
	cur, other := readResolv()

	var want []string
	if override != "" {
		want = strings.Fields(override)
	} else {
		for _, s := range cur {
			if !blockedResolvers[strings.TrimSpace(s)] {
				want = append(want, s)
			}
		}
		strippedBlocked := len(want) != len(cur)
		if len(want) == 0 && strippedBlocked {
			if gw := defaultGatewayV4(); gw != "" && !blockedResolvers[gw] {
				want = []string{gw}
			}
		}
		if !strippedBlocked {
			// nothing blocked and no override: leave resolv.conf exactly as DHCP/minerd left it.
			return nil
		}
	}

	if sameResolvers(cur, want) {
		return nil // already correct: don't churn the file
	}
	if err := writeResolv(want, other); err != nil {
		return err
	}
	log.Printf("dns: resolv.conf set to [%s]%s", strings.Join(want, " "),
		map[bool]string{true: " (owner override)", false: " (stripped a forced resolver)"}[override != ""])
	return nil
}

// sameResolvers reports whether a and b are the same nameserver list in the same order.
func sameResolvers(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if strings.TrimSpace(a[i]) != strings.TrimSpace(b[i]) {
			return false
		}
	}
	return true
}

// dnsGuard re-asserts the DNS policy every minute, so an override keeps winning across DHCP lease renewals and
// a forced 114.114.114.114 can never linger if minerd (somehow) writes one. resolv.conf lives on tmpfs, so a
// rewrite is cheap; ensureSaneDNS only writes when the file would change.
func dnsGuard() {
	for {
		if err := ensureSaneDNS(currentNetConfig()); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Printf("dns guard: %v", err)
		}
		time.Sleep(60 * time.Second)
	}
}
