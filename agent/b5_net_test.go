package main

// Lane B5 (networking, wired-only) regression + Box II tests:
//   - DNS override never yields 114.114.114.114 (owner override wins; the Chinese default is stripped).
//   - the networking lane makes no wlan0 assumption (the Box II is wired-only, there is no Wi-Fi chip).
//   - mac.go reads the Box II's eth0 sysfs path and persists through the crash-safe writeAtomic helper.
//   - ntp.go keeps the pool as a default-on fallback with the Settings->Time switch, unchanged.
//   - the loopback net-check and per-LAN panel binding don't collide with the stock port layout.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setNetEnv points the net-config and resolv.conf at temp files for a test.
func setNetEnv(t *testing.T) (resolvPath string) {
	t.Helper()
	d := t.TempDir()
	oldNet, oldResolv, oldRoute := *netPath, resolvConf, procRoute
	*netPath = filepath.Join(d, "net.json")
	resolvConf = filepath.Join(d, "resolv.conf")
	procRoute = filepath.Join(d, "route")
	t.Cleanup(func() { *netPath, resolvConf, procRoute = oldNet, oldResolv, oldRoute })
	return resolvConf
}

// --- cleanDNSList ---

func TestB5DNSCleanRejectsChineseDefault(t *testing.T) {
	for _, bad := range []string{"114.114.114.114", "114.114.115.115", "8.8.8.8 114.114.114.114", "114.114.114.114,1.1.1.1"} {
		if _, err := cleanDNSList(bad); err == nil {
			t.Errorf("cleanDNSList(%q) should be refused (114DNS / China default)", bad)
		}
	}
}

func TestB5DNSCleanRejectsNonIPv4(t *testing.T) {
	for _, bad := range []string{"not-an-ip", "8.8.8", "2001:4860:4860::8888", "256.1.1.1", "0.0.0.0", "127.0.0.1", "224.0.0.1", "1.1.1.1 2.2.2.2 3.3.3.3 4.4.4.4"} {
		if _, err := cleanDNSList(bad); err == nil {
			t.Errorf("cleanDNSList(%q) should be refused", bad)
		}
	}
}

func TestB5DNSCleanAcceptsAndNormalises(t *testing.T) {
	got, err := cleanDNSList("8.8.8.8, 1.1.1.1  8.8.8.8")
	if err != nil {
		t.Fatal(err)
	}
	if got != "8.8.8.8 1.1.1.1" { // comma/space split, dedup, order kept
		t.Fatalf("normalised to %q", got)
	}
	if got, err := cleanDNSList(""); err != nil || got != "" {
		t.Fatalf("empty override: %q %v", got, err)
	}
}

// --- ensureSaneDNS ---

func readNS(t *testing.T, p string) []string {
	t.Helper()
	b, _ := os.ReadFile(p)
	var ns []string
	for _, ln := range strings.Split(string(b), "\n") {
		if f := strings.Fields(ln); len(f) == 2 && f[0] == "nameserver" {
			ns = append(ns, f[1])
		}
	}
	return ns
}

func TestB5DNSOverrideWinsAndKeepsSearch(t *testing.T) {
	p := setNetEnv(t)
	os.WriteFile(p, []byte("search lan\nnameserver 192.168.0.1\n"), 0o644)
	if err := ensureSaneDNS(netConfig{DNS: "9.9.9.9"}); err != nil {
		t.Fatal(err)
	}
	if ns := readNS(t, p); len(ns) != 1 || ns[0] != "9.9.9.9" {
		t.Fatalf("override should replace the nameservers, got %v", ns)
	}
	b, _ := os.ReadFile(p)
	if !strings.Contains(string(b), "search lan") {
		t.Fatalf("the DHCP search domain (DNS-rebinding guard) must be preserved: %q", b)
	}
}

func TestB5DNSStripsChineseDefaultAndFallsToGateway(t *testing.T) {
	p := setNetEnv(t)
	// a static-IP miner that (pre-patch) had only 114.114.114.114, plus a default route via 192.168.1.1
	os.WriteFile(p, []byte("nameserver 114.114.114.114\n"), 0o644)
	os.WriteFile(procRoute, []byte("Iface\tDestination\tGateway\tFlags\n"+
		"eth0\t00000000\t0101A8C0\t0003\n"), 0o644) // GW 192.168.1.1 (little-endian 0101A8C0)
	if err := ensureSaneDNS(netConfig{}); err != nil {
		t.Fatal(err)
	}
	ns := readNS(t, p)
	for _, s := range ns {
		if blockedResolvers[s] {
			t.Fatalf("114.114.114.114 must never remain, got %v", ns)
		}
	}
	if len(ns) != 1 || ns[0] != "192.168.1.1" {
		t.Fatalf("stripped list should fall back to the gateway, got %v", ns)
	}
}

func TestB5DNSNeverWritesChineseDefaultEvenFromGateway(t *testing.T) {
	p := setNetEnv(t)
	os.WriteFile(p, []byte("nameserver 114.114.114.114\n"), 0o644)
	// pathological: the "gateway" is itself the blocked resolver — it must not be substituted back in
	os.WriteFile(procRoute, []byte("Iface\tDestination\tGateway\tFlags\n"+
		"eth0\t00000000\t72727272\t0003\n"), 0o644) // 114.114.114.114 little-endian
	if err := ensureSaneDNS(netConfig{}); err != nil {
		t.Fatal(err)
	}
	for _, s := range readNS(t, p) {
		if blockedResolvers[s] {
			t.Fatalf("114.114.114.114 was written back from the gateway: %v", readNS(t, p))
		}
	}
}

func TestB5DNSLeavesDHCPAlone(t *testing.T) {
	p := setNetEnv(t)
	orig := "nameserver 192.168.1.1\nnameserver 192.168.1.2\nsearch home\n"
	os.WriteFile(p, []byte(orig), 0o644)
	if err := ensureSaneDNS(netConfig{}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if string(b) != orig {
		t.Fatalf("with no override and nothing blocked, resolv.conf must be left untouched; was rewritten to %q", b)
	}
}

func TestB5DNSResolvTargetFollowsSymlink(t *testing.T) {
	d := t.TempDir()
	real := filepath.Join(d, "tmp-resolv.conf")
	link := filepath.Join(d, "resolv.conf")
	os.WriteFile(real, []byte("nameserver 114.114.114.114\n"), 0o644)
	if err := os.Symlink("tmp-resolv.conf", link); err != nil { // relative, like /etc/resolv.conf -> ../tmp/resolv.conf
		t.Skip("symlink unsupported")
	}
	old := resolvConf
	resolvConf = link
	t.Cleanup(func() { resolvConf = old })
	if tgt := resolvConfTarget(); tgt != real {
		t.Fatalf("resolvConfTarget = %q, want the symlink's real target %q", tgt, real)
	}
}

func TestB5ParseHexIPv4LE(t *testing.T) {
	cases := map[string]string{"0101A8C0": "192.168.1.1", "0000000A": "10.0.0.0", "00000000": "0.0.0.0"}
	for h, want := range cases {
		if got := parseHexIPv4LE(h); got != want {
			t.Errorf("parseHexIPv4LE(%q)=%q want %q", h, got, want)
		}
	}
	if parseHexIPv4LE("zz") != "" {
		t.Error("short/invalid hex should return \"\"")
	}
}

// --- handleNet DNS field (the API surface B9's UI reads/writes) ---

func TestB5HandleNetDNSRefusesChineseDefault(t *testing.T) {
	setNetEnv(t)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/net", strings.NewReader(`{"dns":"114.114.114.114"}`))
	handleNet(w, r)
	var out map[string]any
	json.Unmarshal(w.Body.Bytes(), &out)
	if out["ok"] == true {
		t.Fatalf("/api/net accepted 114.114.114.114: %s", w.Body.String())
	}
	if currentNetConfig().DNS != "" {
		t.Fatal("a refused DNS must not be saved")
	}
}

func TestB5HandleNetDNSSaveApplyAndReport(t *testing.T) {
	p := setNetEnv(t)
	os.WriteFile(p, []byte("nameserver 114.114.114.114\n"), 0o644)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/net", strings.NewReader(`{"dns":"1.1.1.1"}`))
	handleNet(w, r)
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad JSON: %v (%s)", err, w.Body.String())
	}
	if out["ok"] != true || out["dns"] != "1.1.1.1" {
		t.Fatalf("save/report failed: %s", w.Body.String())
	}
	if currentNetConfig().DNS != "1.1.1.1" {
		t.Fatal("DNS not persisted")
	}
	if ns := readNS(t, p); len(ns) != 1 || ns[0] != "1.1.1.1" {
		t.Fatalf("resolv.conf not applied / still has the default, got %v", ns)
	}
	// GET returns the saved override so the UI can show it
	w2 := httptest.NewRecorder()
	handleNet(w2, httptest.NewRequest(http.MethodGet, "/api/net", nil))
	var g map[string]any
	json.Unmarshal(w2.Body.Bytes(), &g)
	if g["dns"] != "1.1.1.1" {
		t.Fatalf("GET /api/net should report the saved DNS, got %s", w2.Body.String())
	}
}

// --- no wlan0 assumption ---

func TestB5NoWlan0InNetworkingLane(t *testing.T) {
	// The Box II (like the SC Lite) has no Wi-Fi chip, so there is no wlan0. The networking lane must not
	// name or assume it: panel binding enumerates real interfaces (lanIPv4s), it never hard-codes eth0/wlan0.
	for _, f := range []string{"netlisten.go", "mac.go", "ntp.go", "netdns.go"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "wlan") {
			t.Errorf("%s names wlan* — the Box II is wired-only, there must be no Wi-Fi assumption", f)
		}
	}
}

func TestB5LoopbackPortLayout(t *testing.T) {
	// the net-check responder sits on a different loopback address from minerd's 127.0.0.1:80, so the panel's
	// per-LAN :80 binding and the stock loopback-bound backend never collide.
	if netCheckAddr != "127.0.0.2:80" {
		t.Fatalf("net-check must stay on 127.0.0.2:80 (minerd keeps 127.0.0.1:80), got %q", netCheckAddr)
	}
}

func TestB5LanIPv4sSkipsLoopback(t *testing.T) {
	// whatever interfaces this host has, lanIPv4s must never hand back a loopback address to bind the panel on.
	for _, ip := range lanIPv4s() {
		if ip.IsLoopback() {
			t.Fatalf("lanIPv4s returned a loopback address %v (would collide with the stock backend)", ip)
		}
	}
}

// --- mac.go: eth0 only, writes through the crash-safe helper ---

func TestB5MACPathIsEth0(t *testing.T) {
	if macIfPath != "/sys/class/net/eth0/address" {
		t.Fatalf("mac.go must read the Box II eth0 sysfs path, got %q", macIfPath)
	}
}

func TestB5MACFixedWriteAtomic(t *testing.T) {
	*authFile = filepath.Join(t.TempDir(), "auth.json")
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/mac", strings.NewReader(`{"mode":"fixed","mac":"3c:2a:f4:01:02:03"}`))
	handleMAC(w, r)
	var out map[string]any
	json.Unmarshal(w.Body.Bytes(), &out)
	if out["ok"] != true {
		t.Fatalf("fixed MAC save failed: %s", w.Body.String())
	}
	// the crash-safe writeAtomic leaves no .tmp behind and the final file holds exactly the address
	if b, _ := os.ReadFile(filepath.Join(macDir(), "mac")); strings.TrimSpace(string(b)) != "3c:2a:f4:01:02:03" {
		t.Fatalf("saved mac = %q", b)
	}
	if _, err := os.Stat(filepath.Join(macDir(), "mac.tmp")); err == nil {
		t.Fatal("writeAtomic should not leave a .tmp file behind")
	}
	if v, _ := macView()["mode"].(string); v != "fixed" {
		t.Fatalf("mode view = %q", v)
	}
}

// --- ntp.go: pool is a default-on fallback with the Settings->Time switch (unchanged policy) ---

func TestB5NTPPoolDefaultOnSwitch(t *testing.T) {
	*authFile = filepath.Join(t.TempDir(), "auth.json")
	os.MkdirAll(filepath.Dir(*authFile), 0o755)
	if !ntpPoolOn() {
		t.Fatal("the public NTP pool must default ON (fallback) so a miner on a router without an NTP server still gets the time")
	}
	if err := setNTPPool(false); err != nil {
		t.Fatal(err)
	}
	if ntpPoolOn() {
		t.Fatal("the Settings->Time switch must be able to turn the pool off")
	}
	if err := setNTPPool(true); err != nil {
		t.Fatal(err)
	}
	if !ntpPoolOn() {
		t.Fatal("the switch must turn it back on")
	}
}
