package main

// L03 (sweep22) API lane: every route the panel registers, driven through a mux built exactly like main()'s
// (the route list is checked against main.go's source), with hostile requests.

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

type l03Route struct {
	path string
	h    http.HandlerFunc
}

// l03Routes: main()'s table, word for word (TestL03RouteTableMatchesMain keeps it honest).
func l03Routes() []l03Route {
	return []l03Route{
		{"/", handleIndex},
		{"/favicon.svg", handleFavicon},
		{"/favicon.ico", handleFavicon},
		{"/api/me", handleMe},
		{"/api/model", requireAuth(handleModel)},
		{"/api/commission", requireAuth(handleCommission)},
		{"/api/login", handleLogin},
		{"/api/logout", handleLogout},
		{"/api/passwd", requireAuth(postOnly(handlePasswd))},
		{"/api/acknowledge", requireAuth(postOnly(handleAck))},
		{"/api/ssh", requireAuth(handleSSH)},
		{"/api/time", requireAuth(handleTime)},
		{"/api/power", requireAuth(handlePower)},
		{"/api/chiptrim", requireAuth(handleChipTrim)},
		{"/api/strongtest", requireAuth(handleStrongTest)},
		{"/api/chipreport", requireAuth(handleChipReport)},
		{"/api/mac", requireAuth(handleMAC)},
		{"/api/net", requireAuth(handleNet)},
		{"/api/display", requireAuth(handleDisplay)},
		{"/api/restart", requireAuth(postOnly(handleRestart))},
		{"/api/boardauto", requireAuth(handleBoardAuto)},
		{"/api/reboot", requireAuth(postOnly(handleReboot))},
		{"/api/recovery", requireAuth(handleRecovery)},
		{"/api/fanpair", requireAuth(handleFanPair)},
		{"/api/settings", requireAuth(handleSettings)},
		{"/api/settings/export", requireAuth(handleSettingsExport)},
		{"/api/settings/import", requireAuth(postOnly(handleSettingsImport))},
		{"/api/notify/test", requireAuth(handleNotifyTest)},
		{"/api/dashboard", requireAuth(handleDashboard)},
		{"/api/hashrate", requireAuth(handleHashrate)},
		{"/api/pools", requireAuth(handlePools)},
		{"/api/presets", requireAuth(handlePresets)},
		{"/api/schedule", requireAuth(handleSchedule)},
		{"/api/shares", requireAuth(handleShares)},
		{"/api/chips", requireAuth(handleChips)},
		{"/api/cmd/", requireAuth(handleCmd)},
		{"/api/activity/stream", requireAuth(handleActivity)},
		{"/api/engine/stream", requireAuth(handleEngine)},
		{"/api/verbose", requireAuth(handleVerbose)},
		{"/api/control", requireAuth(handleControl)},
		{"/api/overclock", requireAuth(handleOverclock)},
		{"/api/ceiling", requireAuth(handleCeiling)},
		{"/api/fan", requireAuth(handleFan)},
		{"/api/fancheck", requireAuth(handleFanCheck)},
		{"/api/tuner", requireAuth(handleTuner)},
		{"/api/tuner/export", requireAuth(handleTunerExport)},
		{"/api/tuner/chipmap", requireAuth(handleTunerChipMap)},
		{"/api/update", requireAuth(handleUpdate)},
		{"/api/poolguard", requireAuth(handlePoolGuard)},
		{"/api/update/upload", requireAuth(handleUpdateUpload)},
		{"/api/log/stream", requireAuth(handleLog)},
		{"/api/log/export", requireAuth(handleLogExport)},
	}
}

func l03Mux() http.Handler {
	mux := http.NewServeMux()
	for _, r := range l03Routes() {
		mux.HandleFunc(r.path, r.h)
	}
	return securityHeaders(mux)
}

// the routes that must answer without a session
var l03Open = map[string]bool{"/": true, "/favicon.svg": true, "/favicon.ico": true, "/api/me": true, "/api/login": true, "/api/logout": true}

// TestL03RouteTableMatchesMain: the table above is main()'s, line for line (wrapper included).
func TestL03RouteTableMatchesMain(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`mux\.HandleFunc\("([^"]+)",\s*(.+)\)\s*(//.*)?$`)
	var inMain []string
	sc := bufio.NewScanner(strings.NewReader(string(src)))
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		if m := re.FindStringSubmatch(strings.TrimSpace(sc.Text())); m != nil {
			inMain = append(inMain, m[1]+" "+strings.TrimSpace(m[2]))
		}
	}
	// this file's own table, as text
	self, _ := os.ReadFile("l03api_test.go")
	re2 := regexp.MustCompile(`\{"(/[^"]*)", ([^}]+)\},`)
	var mine []string
	for _, m := range re2.FindAllStringSubmatch(string(self), -1) {
		mine = append(mine, m[1]+" "+m[2])
	}
	sort.Strings(inMain)
	sort.Strings(mine)
	if strings.Join(inMain, "\n") != strings.Join(mine, "\n") {
		t.Fatalf("route table differs from main.go\nmain:\n%s\n\ntest:\n%s", strings.Join(inMain, "\n"), strings.Join(mine, "\n"))
	}
	// any other HandleFunc/Handle in the package (a second mux) would be missed: there must be none
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, _ := os.ReadFile(f)
		n := strings.Count(string(b), "HandleFunc(") + strings.Count(string(b), ".Handle(")
		if f == "main.go" {
			n -= len(inMain)
		}
		if n != 0 {
			t.Errorf("%s registers %d more route(s) outside main()'s mux", f, n)
		}
	}
}

// l03Env: every file the panel keeps under a temp dir, a fresh login, the stock backend and the cgminer API
// pointed at nothing that exists.
func l03Env(t *testing.T, mustChange bool) string {
	t.Helper()
	dir := r5Env(t)
	oldAuth := auth
	oldNet, oldHr, oldTuner, oldBfg, oldWeb, oldAPI := *netPath, *hrPath, *tunerDir, *bfgPath, *minerWeb, *apiAddr
	*netPath = filepath.Join(dir, "net.json")
	*hrPath = filepath.Join(dir, "hashrate.json")
	*tunerDir = filepath.Join(dir, "tuner")
	*bfgPath = filepath.Join(dir, "bfgminer.json")
	*minerWeb = "127.0.0.1:47061" // nothing listens: the backend is "unreachable"
	*apiAddr = "127.0.0.1:47062"
	t.Setenv(brandEnv("REBOOT"), "true")
	a := authStore{Username: "zq7owner", MustChange: mustChange, Acknowledged: "2026-01-01 00:00",
		Secret: base64.StdEncoding.EncodeToString(randBytes(32))}
	setPasswordSHA256(&a, "correct-horse-1")
	authMu.Lock()
	auth = a
	authMu.Unlock()
	saveAuth()
	t.Cleanup(func() {
		authUpgradeWG.Wait()
		authMu.Lock()
		auth = oldAuth
		authMu.Unlock()
		*netPath, *hrPath, *tunerDir, *bfgPath, *minerWeb, *apiAddr = oldNet, oldHr, oldTuner, oldBfg, oldWeb, oldAPI
	})
	return dir
}

func l03Req(h http.Handler, method, path, body string, cookie string, hdr map[string]string) *httptest.ResponseRecorder {
	var br io.Reader
	if body != "" {
		br = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, br)
	req.RemoteAddr = "192.168.1.50:40000"
	req.Host = "192.168.1.10"
	req.Header.Set("X-Requested-With", "panel") // the page's own requests carry it (required on the SC Box II)
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: "sid", Value: cookie})
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// l03Probe: a path inside each route's pattern
func l03Probe(p string) string {
	if p == "/api/cmd/" {
		return "/api/cmd/summary"
	}
	return p
}

// Every route but the open ones answers 401, for every method, without a valid session, and with any
// forged/expired/other-user cookie. The handler is never reached (a panic or a 200 would show it).
func TestL03EveryRouteNeedsAuth(t *testing.T) {
	l03Env(t, false)
	h := l03Mux()
	good := issueCookie("zq7owner")
	parts := strings.SplitN(good, ".", 2)
	expPayload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("zq7owner|%d", time.Now().Add(-time.Minute).Unix())))
	otherUser := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("admin|%d", time.Now().Add(time.Hour).Unix())))
	bad := []string{"", "x", ".", good + "x", parts[0] + ".", "." + parts[1], expPayload + "." + parts[1], otherUser + "." + parts[1],
		parts[0] + "." + base64.RawURLEncoding.EncodeToString(make([]byte, 32)), strings.Repeat("A", 5000) + "." + parts[1]}
	for _, r := range l03Routes() {
		if l03Open[r.path] {
			continue
		}
		for _, m := range []string{"GET", "POST", "PUT", "DELETE", "PATCH", "HEAD", "OPTIONS"} {
			for _, c := range bad {
				rr := l03Req(h, m, l03Probe(r.path), `{"confirm":true}`, c, nil)
				if rr.Code != http.StatusUnauthorized {
					t.Errorf("%s %s with cookie %.20q: %d, want 401 (%s)", m, r.path, c, rr.Code, rr.Body.String())
				}
			}
		}
	}
	// the good cookie gets through (sanity: the probe above means something)
	if rr := l03Req(h, "GET", "/api/overclock", "", good, nil); rr.Code != 200 {
		t.Fatalf("a valid session was refused: %d %s", rr.Code, rr.Body.String())
	}
	// path tricks reach no handler without a session
	for _, p := range []string{"/api/cmd/../pools", "/api//pools", "/api/pools/", "/API/pools", "/api/pools%2f", "/api/cmd/summary/../../reboot"} {
		rr := l03Req(h, "POST", p, `{}`, "", nil)
		if rr.Code == 200 && !strings.Contains(rr.Body.String(), "<!doctype") {
			t.Errorf("POST %s without a session: %d %s", p, rr.Code, rr.Body.String())
		}
	}
}

// While the factory password is still set, a signed-in session reaches only /api/passwd and /api/acknowledge.
func TestL03MustChangeGate(t *testing.T) {
	l03Env(t, true)
	h := l03Mux()
	c := issueCookie("zq7owner")
	for _, r := range l03Routes() {
		if l03Open[r.path] || r.path == "/api/passwd" || r.path == "/api/acknowledge" {
			continue
		}
		for _, m := range []string{"GET", "POST"} {
			if rr := l03Req(h, m, l03Probe(r.path), `{}`, c, nil); rr.Code != http.StatusForbidden {
				t.Errorf("%s %s under must-change: %d, want 403", m, r.path, rr.Code)
			}
		}
	}
}

// postOnly routes refuse every other method even with a session.
func TestL03PostOnlyRoutes(t *testing.T) {
	l03Env(t, false)
	h := l03Mux()
	c := issueCookie("zq7owner")
	for _, p := range []string{"/api/passwd", "/api/acknowledge", "/api/restart", "/api/reboot"} {
		for _, m := range []string{"GET", "PUT", "DELETE", "HEAD"} {
			if rr := l03Req(h, m, p, "", c, nil); rr.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s %s: %d, want 405", m, p, rr.Code)
			}
		}
	}
}

// The open routes: what they tell someone who isn't signed in.
func TestL03OpenRoutesLeakNothing(t *testing.T) {
	l03Env(t, false)
	authMu.Lock()
	auth.MinerPass = "stock-secret-pw"
	authMu.Unlock()
	h := l03Mux()
	for _, p := range []string{"/", "/favicon.svg", "/favicon.ico", "/api/logout", "/api/me"} {
		for _, m := range []string{"GET", "POST"} {
			rr := l03Req(h, m, p, "", "", nil)
			b := rr.Body.String()
			for _, s := range []string{"zq7owner", "stock-secret-pw", auth.Hash, auth.Secret} {
				if s != "" && strings.Contains(b, s) {
					t.Errorf("%s %s leaks %q", m, p, s)
				}
			}
			if rr.Header().Get("X-Frame-Options") != "DENY" || rr.Header().Get("Content-Security-Policy") == "" {
				t.Errorf("%s %s: security headers missing", m, p)
			}
		}
	}
	// a path under "/" that isn't the page: 404, not the page
	if rr := l03Req(h, "GET", "/etc/passwd", "", "", nil); rr.Code != 404 {
		t.Errorf("/etc/passwd: %d", rr.Code)
	}
}

// F: /api/me tells anyone on the network whether the factory login (admin/admin) still works, whether the stock
// web password is stored and whether the disclaimer was accepted. The page reads them only after signing in.
func TestL03MeUnauthenticatedLeaksAccountState(t *testing.T) {
	l03Env(t, true)
	rr := l03Req(l03Mux(), "GET", "/api/me", "", "", nil)
	var m map[string]any
	json.Unmarshal(rr.Body.Bytes(), &m)
	for _, k := range []string{"must_change", "miner_pass_set", "acknowledged", "username"} {
		if _, has := m[k]; has {
			t.Errorf("/api/me without a session says %s=%v", k, m[k])
		}
	}
	if m["authenticated"] != false {
		t.Fatalf("authenticated=%v", m["authenticated"])
	}
}

// the login: only JSON, from its own origin, and a host that is on the local network
func TestL03LoginGuards(t *testing.T) {
	l03Env(t, false)
	h := l03Mux()
	ok := `{"username":"zq7owner","password":"correct-horse-1"}`
	cases := []struct {
		m, body string
		hdr     map[string]string
		want    int
	}{
		{"GET", ok, map[string]string{"Content-Type": "application/json"}, 403},
		{"POST", ok, map[string]string{"Content-Type": "text/plain"}, 403},
		{"POST", ok, map[string]string{"Content-Type": "application/x-www-form-urlencoded"}, 403},
		{"POST", ok, map[string]string{"Content-Type": "application/json", "Origin": "http://evil.example"}, 403},
		{"POST", ok, map[string]string{"Content-Type": "application/json", "Origin": "null"}, 403},
		{"POST", ok, map[string]string{"Content-Type": "application/json"}, 200},
	}
	for i, c := range cases {
		rr := l03Req(h, c.m, "/api/login", c.body, "", c.hdr)
		if rr.Code != c.want {
			t.Errorf("case %d: %d want %d (%s)", i, rr.Code, c.want, rr.Body.String())
		}
	}
	// a rebinding host name
	req := httptest.NewRequest("POST", "/api/login", strings.NewReader(ok))
	req.Host = "attacker.example.com"
	req.RemoteAddr = "192.168.1.51:1"
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 403 {
		t.Errorf("rebinding host: %d", rr.Code)
	}
}

// ---------------- hostile bodies against every JSON handler ----------------

var l03Hostile = func() []string {
	deep := strings.Repeat("[", 20000) + strings.Repeat("]", 20000)
	deepObj := strings.Repeat(`{"a":`, 20000) + "1" + strings.Repeat("}", 20000)
	return []string{
		"", " ", "{}", "null", "[]", `"x"`, "0", "-1", "true", "{", `{"a":`, "{}{}", `{"x":1}garbage`,
		"\x00\x01\x02", "\xff\xfe{}", deep, deepObj, `{"a":` + deep + `}`,
		`{"index":1e400}`, `{"index":-9223372036854775809}`, `{"index":9223372036854775807,"action":"edit","confirm":true,"old_url":""}`,
		`{"index":-1,"action":"switch","confirm":true}`, `{"index":-1,"action":"delete","confirm":true}`,
		`{"freq_mhz":NaN}`, `{"mains_v":Infinity}`, `{"cal_watts":-1e308}`, `{"mains_v":1e308}`, `{"hot_c":-0}`,
		`{"action":5}`, `{"action":["start"]}`, `{"action":{"x":1}}`, `{"enabled":"true"}`, `{"on":1}`,
		`{"confirm":true,"confirm":false}`, `{"action":"stop","action":"start"}`, `{"mode":"keep","mode":"bogus"}`,
		`{"caps":{"freq":-5,"pv":99999999}}`, `{"caps":[1,2]}`, `{"events":{"x":"y"}}`, `{"events":[1]}`,
		`{"min_interval_min":-1}`, `{"hash_low_pct":1e30}`, `{"share_unit":"<script>"}`, `{"hash_unit":"\u0000"}`,
		`{"port":-1}`, `{"port":70000}`, `{"enabled":null,"port":"22"}`, `{"zone":"../../../etc/passwd"}`,
		`{"chain":-1,"chip":-1,"mhz":1e9}`, `{"chain":99,"chip":99999,"mhz":-1e9}`,
		`{"start":"yes"}`, `{"stop":1}`, `{"watchdog":"on"}`, `{"dismiss":"x"}`, `{"resume":[]}`,
		`{"config":null,"action":"stop"}`, `{"config":5}`, `{"repo":"../../x"}`, `{"sums":"zz"}`,
		`{"Channel":"carrier-pigeon"}`, `{"preset":-99,"confirm":true}`, `{"release":"x"}`,
		strings.Repeat(`{"a":1},`, 10),
	}
}()

// routes a hostile body may be posted to here: not the ones that run commands on this machine (hostname,
// reboot, killall, settimeofday, SSH listener), the streams (they never end) or the upload (a shared /tmp file).
var l03Skip = map[string]string{
	"/api/net": "runs `hostname` as root", "/api/reboot": "runs reboot", "/api/verbose": "runs killall",
	"/api/time": "settimeofday", "/api/ssh": "opens a listener", "/api/activity/stream": "stream",
	"/api/engine/stream": "stream", "/api/log/stream": "stream", "/api/update/upload": "shared /tmp stage",
	"/api/notify/test": "network", "/api/fancheck": "runs Goldshell's fan test",
}

func TestL03HostileBodies(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	dir := l03Env(t, false)
	h := l03Mux()
	c := issueCookie("zq7owner")
	ctypes := []string{"application/json", "text/plain", "application/x-www-form-urlencoded", "multipart/form-data; boundary=x", ""}
	for _, r := range l03Routes() {
		if l03Open[r.path] || l03Skip[r.path] != "" || r.path == "/api/passwd" {
			continue
		}
		for i, b := range l03Hostile {
			ct := ctypes[i%len(ctypes)]
			func() {
				defer func() {
					if p := recover(); p != nil {
						t.Errorf("POST %s %.60q: panic %v", r.path, b, p)
					}
				}()
				done := make(chan *httptest.ResponseRecorder, 1)
				go func() { done <- l03Req(h, "POST", l03Probe(r.path), b, c, map[string]string{"Content-Type": ct}) }()
				select {
				case rr := <-done:
					if rr.Code >= 500 && rr.Code != 502 && rr.Code != 503 {
						t.Errorf("POST %s %.60q: %d %s", r.path, b, rr.Code, rr.Body.String())
					}
					if s := rr.Body.String(); strings.Contains(s, "Go struct") || strings.Contains(s, "Go value") {
						t.Logf("POST %s %.40q: raw decoder text: %.160s", r.path, b, s)
					}
				case <-time.After(30 * time.Second):
					t.Errorf("POST %s %.60q: no answer in 30 s", r.path, b)
				}
			}()
		}
	}
	// the overclock caps: a hostile body never leaves a cap outside the guard's window
	if oc := loadOverclock(); oc.Caps != nil {
		for k, v := range oc.Caps {
			t.Logf("caps after the sweep: %s=%d", k, v)
		}
	}
	_ = dir
}

// a huge body: cut at 1 MiB with a plain message (and never read whole into memory)
func TestL03HugeBody(t *testing.T) {
	l03Env(t, false)
	h := l03Mux()
	c := issueCookie("zq7owner")
	big := `{"label":"` + strings.Repeat("A", 3<<20) + `"}`
	for _, p := range []string{"/api/settings", "/api/fan", "/api/presets", "/api/schedule", "/api/overclock", "/api/pools", "/api/control", "/api/chiptrim"} {
		rr := l03Req(h, "POST", p, big, c, map[string]string{"Content-Type": "application/json"})
		var m map[string]any
		json.Unmarshal(rr.Body.Bytes(), &m)
		if m["ok"] == true {
			t.Errorf("%s accepted a 3 MiB body: %s", p, rr.Body.String())
		}
	}
}

// F: /api/overclock {"caps":{...}} stores any key with any positive number. A "freq" cap under the hard minimum
// (or under the locked window's 700) makes the "unlocked" clock window empty or narrower than the locked one.
func TestL03OverclockCapsUnchecked(t *testing.T) {
	l03Env(t, false)
	h := l03Mux()
	c := issueCookie("zq7owner")
	l03Req(h, "POST", "/api/overclock", `{"acknowledge":true}`, c, nil)
	rr := l03Req(h, "POST", "/api/overclock", `{"caps":{"freq":1,"vrun":5,"pv":2,"junk":7}}`, c, nil)
	var m map[string]any
	json.Unmarshal(rr.Body.Bytes(), &m)
	oc := loadOverclock()
	t.Logf("reply %v; stored caps %v; clockWindowHi=%d", m["ok"], oc.Caps, clockWindowHi())
	if hi := clockWindowHi(); hi < freqMax {
		t.Errorf("after caps {freq:1} the unlocked clock window ends at %d MHz (locked: %d, hard min %d): every setting is refused", hi, freqMax, freqHardMin)
	}
	if _, has := oc.Caps["junk"]; has {
		t.Errorf("an unknown cap key was stored: %v", oc.Caps)
	}
	if err := validateControlEx(powerplan{FreqMHz: 650, VrunMv: 9000, PVMv: 9400, Fan1: 50, Fan2: 50}, true); err != nil {
		t.Errorf("a plan inside the locked window is refused once unlocked: %v", err)
	}
}

var _ = sync.Mutex{}

// ---------------- with a fake stock backend ----------------

type l03Put struct {
	path string
	body []byte
}

type l03Stock struct {
	mu      sync.Mutex
	setting map[string]any
	pools   []map[string]any
	puts    []l03Put
}

const l03Setting = `{"manual":true,"tempcontrol":true,"select":0,"manualPowerplan":"625 MHz 9100 V 50 RPM 50 RPM PV 9400",
 "powerplans":[{"info":"625 MHz 9100 V 50 RPM 50 RPM PV 9400","level":0},{"info":"0 MHz 0 V 100 RPM 100 RPM","level":3}]}`

var l03PortMu sync.Mutex // one fake backend on the lane's port at a time

func l03Backend(t *testing.T) *l03Stock {
	t.Helper()
	l03PortMu.Lock()
	st := &l03Stock{}
	json.Unmarshal([]byte(l03Setting), &st.setting)
	st.pools = []map[string]any{
		{"url": "stratum+tcp://pool.one:3333", "user": "wallet1.w", "pass": "x"},
		{"url": "stratum+tcp://pool.two:3333", "user": "wallet2.w", "pass": "secretpw"},
	}
	ln, err := netListen("127.0.0.1:47063")
	if err != nil {
		l03PortMu.Unlock()
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st.mu.Lock()
		defer st.mu.Unlock()
		switch {
		case strings.HasPrefix(r.URL.Path, "/user/login"):
			json.NewEncoder(w).Encode(map[string]any{"JWT Token": "tok"})
		case r.Method == "GET" && r.URL.Path == "/mcb/pools":
			json.NewEncoder(w).Encode(st.pools)
		case r.Method == "GET":
			json.NewEncoder(w).Encode(st.setting)
		case r.Method == "PUT":
			b, _ := io.ReadAll(r.Body)
			st.puts = append(st.puts, l03Put{r.URL.Path, b})
			if r.URL.Path == "/mcb/setting" {
				var m map[string]any
				if json.Unmarshal(b, &m) == nil {
					st.setting = m
				}
			}
			io.WriteString(w, `{"status":0}`)
		}
	}))
	srv.Listener.Close()
	srv.Listener = ln
	srv.Start()
	oldWeb, oldWake, oldFans := *minerWeb, wakeWait, fanSysDir
	*minerWeb = "127.0.0.1:47063"
	wakeWait = 0
	fanSysDir = t.TempDir()
	jwtToken = ""
	authMu.Lock()
	auth.MinerPass = "pw"
	authMu.Unlock()
	fanGuardMu.Lock()
	fanGuardSt, fanGuardOK = fanGuardState{}, true
	fanGuardMu.Unlock()
	t.Cleanup(func() {
		srv.Close()
		*minerWeb, wakeWait, fanSysDir = oldWeb, oldWake, oldFans
		fanGuardMu.Lock()
		fanGuardSt, fanGuardOK = fanGuardState{}, false
		fanGuardMu.Unlock()
		setCharge("")
		l03PortMu.Unlock()
	})
	return st
}

func (s *l03Stock) takePuts() []l03Put {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.puts
	s.puts = nil
	return p
}

// l03CheckPuts: what reached the backend is safe: a plan inside the hard window (PV ≤ 9600), pools well-formed.
func l03CheckPuts(t *testing.T, what string, puts []l03Put) {
	t.Helper()
	for _, p := range puts {
		switch p.path {
		case "/mcb/setting":
			var m map[string]any
			json.Unmarshal(p.body, &m)
			s, _ := m["manualPowerplan"].(string)
			pp, ok := parsePowerplan(s)
			if !ok {
				t.Errorf("%s: PUT unparseable plan %q", what, s)
				continue
			}
			if pp.FreqMHz != 0 {
				if err := validateControlEx(pp, true); err != nil || pp.PVMv > 9600 {
					t.Errorf("%s: PUT unsafe plan %q: %v", what, s, err)
				}
			}
		case "/mcb/newpool":
			var m map[string]any
			json.Unmarshal(p.body, &m)
			u, _ := m["url"].(string)
			if e := poolURLErr(u); e != "" {
				t.Errorf("%s: new pool %q: %s", what, u, e)
			}
		case "/mcb/pools":
			var arr []map[string]any
			if json.Unmarshal(p.body, &arr) != nil || len(arr) != 2 {
				t.Errorf("%s: pool list PUT changes the count: %s", what, p.body)
			}
		}
	}
}

func TestL03HostileBodiesWithBackend(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	l03Env(t, false)
	st := l03Backend(t)
	h := l03Mux()
	c := issueCookie("zq7owner")
	for _, r := range l03Routes() {
		if l03Open[r.path] || l03Skip[r.path] != "" || r.path == "/api/passwd" || r.path == "/api/restart" {
			continue
		}
		for _, b := range l03Hostile {
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				defer func() {
					if p := recover(); p != nil {
						t.Errorf("POST %s %.60q: panic %v", r.path, b, p)
						done <- httptest.NewRecorder()
					}
				}()
				done <- l03Req(h, "POST", l03Probe(r.path), b, c, map[string]string{"Content-Type": "application/json"})
			}()
			select {
			case rr := <-done:
				if rr.Code >= 500 && rr.Code != 502 && rr.Code != 503 {
					t.Errorf("POST %s %.60q: %d %s", r.path, b, rr.Code, rr.Body.String())
				}
			case <-time.After(30 * time.Second):
				t.Fatalf("POST %s %.60q: no answer in 30 s", r.path, b)
			}
			l03CheckPuts(t, r.path+" "+fmt.Sprintf("%.40q", b), st.takePuts())
		}
		// stop anything a body may have started
		tunerMu.Lock()
		if tunerCur != nil {
			tunerCur.Stop()
		}
		tunerMu.Unlock()
		ceilAbort.Store(true)
	}
}

func netListen(addr string) (net.Listener, error) { return net.Listen("tcp", addr) }

// F: a request the decoder refuses part-way is still acted on: the fields before the bad one are applied.
// /api/net saves a new web-UI port from {"port":47064,"hostname":["x"]}; /api/mac saves "random" (a new MAC, so a
// new DHCP address, at the next reboot) from {"mode":"random","mac":0}; /api/recovery switches the kept log on.
func TestL03HalfDecodedRequestApplied(t *testing.T) {
	dir := l03Env(t, false)
	h := l03Mux()
	c := issueCookie("zq7owner")
	rr := l03Req(h, "POST", "/api/net", `{"port":47064,"hostname":["x"]}`, c, nil)
	if b, err := os.ReadFile(*netPath); err == nil && strings.Contains(string(b), "47064") {
		t.Errorf("/api/net saved the port from a bad request: %s (reply %s)", b, rr.Body.String())
	}
	rr = l03Req(h, "POST", "/api/mac", `{"mode":"random","mac":0}`, c, nil)
	if m := readTrim(filepath.Join(dir, "macmode")); m == "random" {
		t.Errorf("/api/mac saved mode %q from a bad request (reply %s)", m, rr.Body.String())
	}
	rr = l03Req(h, "POST", "/api/recovery", `{"keep_log":true,"dismiss":"no"}`, c, nil)
	if b, _ := os.ReadFile(recConfigPath()); strings.Contains(string(b), `"keep_log":true`) || strings.Contains(string(b), `"KeepLog":true`) {
		t.Errorf("/api/recovery saved %s from a bad request", b)
	}
	recMu.Lock()
	recCfg = recoveryConfig{}
	recMu.Unlock()
	// an empty body still reads as "{}" (a GET-style view), not an error
	if rr := l03Req(h, "POST", "/api/power", "", c, nil); strings.Contains(rr.Body.String(), "bad request") {
		t.Errorf("an empty POST is refused: %s", rr.Body.String())
	}
}

// F: the panel sign-in skips the slow password hash when the name is wrong, so a wrong name answers sooner by
// the hash's time: the reply's timing says whether a guessed name is the account's (SSH runs the hash always).
func TestL03LoginNameTiming(t *testing.T) {
	l03Env(t, false)
	a := auth
	setPasswordSHA256(&a, "correct-horse-1")
	a.Iter = 600000 // a slow hash, so the difference stands out of the noise
	salt, _ := base64.StdEncoding.DecodeString(a.Salt)
	a.Hash = base64.StdEncoding.EncodeToString(pbkdf2([]byte("correct-horse-1"), salt, a.Iter, 32))
	authMu.Lock()
	auth = a
	authMu.Unlock()
	t0 := time.Now()
	checkPassword("nope")
	hashT := time.Since(t0)
	if hashT < 80*time.Millisecond {
		t.Skipf("the hash takes only %v here", hashT)
	}
	h := l03Mux()
	login := func(n int, body string) time.Duration {
		req := httptest.NewRequest("POST", "/api/login", strings.NewReader(body))
		req.RemoteAddr = fmt.Sprintf("10.9.%d.%d:1", n/200, n%200+1) // a fresh address each time (no lock-out)
		req.Host = "192.168.1.10"
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Requested-With", "panel")
		rr := httptest.NewRecorder()
		t0 := time.Now()
		h.ServeHTTP(rr, req)
		if rr.Code != 401 {
			t.Fatalf("login: %d %s", rr.Code, rr.Body.String())
		}
		return time.Since(t0)
	}
	var wn, rn []time.Duration
	for i := 0; i < 6; i++ { // alternating order, the median of each (the machine is shared: noisy)
		if i%2 == 0 {
			wn = append(wn, login(2*i, `{"username":"admin","password":"guess"}`))
			rn = append(rn, login(2*i+1, `{"username":"zq7owner","password":"guess"}`))
		} else {
			rn = append(rn, login(2*i+1, `{"username":"zq7owner","password":"guess"}`))
			wn = append(wn, login(2*i, `{"username":"admin","password":"guess"}`))
		}
	}
	med := func(v []time.Duration) time.Duration {
		sort.Slice(v, func(i, j int) bool { return v[i] < v[j] })
		return (v[len(v)/2-1] + v[len(v)/2]) / 2
	}
	wrongName, rightName := med(wn), med(rn)
	d := rightName - wrongName
	t.Logf("hash %v; wrong name %v, right name %v (medians of %v / %v)", hashT, wrongName, rightName, wn, rn)
	if d > hashT/2 {
		t.Errorf("a wrong user name answers %v sooner than the right one: the timing gives the name away", d)
	}
}

// F (4): /api/settings and /api/chiptrim show the Go decoder's own text ("json: cannot unmarshal … Go value of type
// main.notifyConfig"); /api/settings saves a Discord address that isn't http(s) (the other servers' are checked).
func TestL03SettingsMessages(t *testing.T) {
	dir := l03Env(t, false)
	notifyMu.Lock()
	old := notifyPath
	notifyPath = filepath.Join(dir, "notify.json")
	notifyCfg = defaultNotifyConfig()
	notifyMu.Unlock()
	t.Cleanup(func() { notifyMu.Lock(); notifyPath = old; notifyMu.Unlock() })
	h := l03Mux()
	c := issueCookie("zq7owner")
	for _, p := range []string{"/api/settings", "/api/chiptrim"} {
		rr := l03Req(h, "POST", p, `[]`, c, nil)
		if s := rr.Body.String(); strings.Contains(s, "Go value") || strings.Contains(s, "json:") {
			t.Errorf("%s shows the decoder's text: %s", p, s)
		}
	}
	l03Req(h, "POST", "/api/settings", `{"discord_url":"file:///etc/passwd","discord_on":true}`, c, nil)
	notifyMu.Lock()
	d := notifyCfg.DiscordURL
	notifyMu.Unlock()
	if d != "" && !strings.HasPrefix(d, "https://") && !strings.HasPrefix(d, "http://") {
		t.Errorf("saved Discord address %q", d)
	}
}
