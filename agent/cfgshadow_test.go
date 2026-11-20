package main

// v0.5.2: the Box II's settings in RAM, the flash read-only except for a write window (cfgshadow.go,
// internal/cfgwin). The flash is a temp folder; the remounts are recorded by a hook, and a write to the "flash"
// outside a window fails the test (the hook tracks rw).

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"sclite-agent/internal/cfgwin"
)

type shadowFake struct {
	mu      sync.Mutex
	ops     []string
	failRW  error
	ram     string
	flash   string
	panel   string // the panel's folder in RAM
	windows int64
}

// withShadow: the Box II with its settings in RAM: RAMRoot/FlashRoot in a temp dir, the panel's folder
// RAMRoot/scbox2 (*authFile there), remounts recorded.
func withShadow(t *testing.T) *shadowFake {
	t.Helper()
	useModel(t, profSCBox2)
	d := t.TempDir()
	f := &shadowFake{ram: filepath.Join(d, "ram"), flash: filepath.Join(d, "flash")}
	f.panel = filepath.Join(f.ram, "scbox2")
	os.MkdirAll(f.panel, 0o700)
	os.MkdirAll(f.flash, 0o700)
	oR, oF, oL, oRm, oA := cfgwin.RAMRoot, cfgwin.FlashRoot, cfgwin.LockPath, cfgwin.Remount, cfgwin.ActiveHook
	oAuth, oFan, oHr, oPre, oNet, oTun := *authFile, fanPath, hrPath, presetPath, netPath, *tunerDir
	cfgwin.RAMRoot, cfgwin.FlashRoot, cfgwin.LockPath = f.ram, f.flash, filepath.Join(d, "lock")
	cfgwin.ActiveHook = func() bool { return true }
	cfgwin.Remount = func(dir string, rw bool) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		if rw && f.failRW != nil {
			f.ops = append(f.ops, "rw-failed")
			return f.failRW
		}
		f.ops = append(f.ops, map[bool]string{true: "rw", false: "ro"}[rw])
		return nil
	}
	*authFile = filepath.Join(f.panel, "auth.json")
	fp, hp, pp, np := filepath.Join(f.panel, "fan.json"), filepath.Join(f.panel, "hashrate.json"), filepath.Join(f.panel, "presets.json"), filepath.Join(f.panel, "net.json")
	fanPath, hrPath, presetPath, netPath = &fp, &hp, &pp, &np
	*tunerDir = filepath.Join(f.panel, "tuner")
	oLate := oemLateWrite
	oemLateWrite = time.Hour // (the delayed second look: not in these tests)
	f.windows = cfgwin.Windows()
	cfgROReset()
	t.Cleanup(func() {
		cfgwin.RAMRoot, cfgwin.FlashRoot, cfgwin.LockPath, cfgwin.Remount, cfgwin.ActiveHook = oR, oF, oL, oRm, oA
		*authFile, fanPath, hrPath, presetPath, netPath, *tunerDir = oAuth, oFan, oHr, oPre, oNet, oTun
		oemLateWrite = oLate
		cfgROReset()
	})
	return f
}

func (f *shadowFake) seq() string   { f.mu.Lock(); defer f.mu.Unlock(); return strings.Join(f.ops, ",") }
func (f *shadowFake) opened() int64 { return cfgwin.Windows() - f.windows }
func (f *shadowFake) onFlash(rel string) (string, bool) {
	b, err := os.ReadFile(filepath.Join(f.flash, rel))
	return string(b), err == nil
}
func (f *shadowFake) putBoth(t *testing.T, rel, ram, flash string) {
	t.Helper()
	for _, x := range [][2]string{{f.ram, ram}, {f.flash, flash}} {
		p := filepath.Join(x[0], rel)
		os.MkdirAll(filepath.Dir(p), 0o700)
		if err := os.WriteFile(p, []byte(x[1]), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// A panel setting (the fan settings) is saved in one window: rw, written, ro; its RAM copy is what the panel uses.
func TestShadowSettingSavedInWindow(t *testing.T) {
	f := withShadow(t)
	c := defaultFanConfig()
	c.Profile = "curve-60"
	if err := saveFanConfig(c); err != nil {
		t.Fatal(err)
	}
	if f.seq() != "rw,ro" || f.opened() != 1 {
		t.Fatalf("remounts %q, windows %d", f.seq(), f.opened())
	}
	got, ok := f.onFlash("scbox2/fan.json")
	if !ok || !strings.Contains(got, "curve-60") {
		t.Fatalf("not on the flash: %q", got)
	}
	// saved again unchanged: no window
	saveFanConfig(c)
	if f.opened() != 1 {
		t.Fatalf("an unchanged save opened a window (%d)", f.opened())
	}
	// the login file (saveAuth) too
	authMu.Lock()
	keep := auth
	auth.Acknowledged = "2026-11-13 12:00"
	authMu.Unlock()
	defer func() { authMu.Lock(); auth = keep; authMu.Unlock() }()
	if err := saveAuth(); err != nil {
		t.Fatal(err)
	}
	if got, ok := f.onFlash("scbox2/auth.json"); !ok || !strings.Contains(got, "2026-11-13 12:00") || f.opened() != 2 {
		t.Fatalf("auth.json on the flash: %v, windows %d", ok, f.opened())
	}
}

// Files that change by themselves while the miner runs stay in RAM; a panel-initiated reboot saves them.
func TestShadowHotFilesStayInRAM(t *testing.T) {
	f := withShadow(t)
	if err := cfgWrite(*hrPath, []byte(`{"h":[1,2,3]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	writeAtomic(throttlePath(), []byte(`{"saved":625}`), 0o644)
	os.MkdirAll(filepath.Join(*tunerDir, "unit"), 0o755)
	cfgWrite(filepath.Join(*tunerDir, "unit", "log.txt"), []byte("step 1"), 0o644) // (the tuner's run folder)
	if f.opened() != 0 || f.seq() != "" {
		t.Fatalf("a hot file opened a window: %q", f.seq())
	}
	for _, rel := range []string{"scbox2/hashrate.json", "scbox2/throttle.json", "scbox2/tuner/unit/log.txt"} {
		if _, ok := f.onFlash(rel); ok {
			t.Fatalf("%s reached the flash", rel)
		}
	}
	cfgCheckpoint() // markCleanReboot's last step
	if f.opened() != 1 || f.seq() != "rw,ro" {
		t.Fatalf("checkpoint: %q", f.seq())
	}
	for _, rel := range []string{"scbox2/hashrate.json", "scbox2/throttle.json", "scbox2/tuner/unit/log.txt"} {
		if _, ok := f.onFlash(rel); !ok {
			t.Fatalf("%s not saved by the checkpoint", rel)
		}
	}
	for _, n := range []string{"hashrate.json", "events.log", "shares.json", "throttle.json", "chipwatch.json", "schedule-before.json"} {
		if !cfgHot(filepath.Join(f.panel, n)) {
			t.Errorf("%s is not a hot file", n)
		}
	}
	for _, n := range []string{"auth.json", "fan.json", "net.json", "heat-idle.json", "pool-hold.json", "fan-lock.json", "softidle.json", "chiptrim", "notify.json", "schedule.json"} {
		if cfgHot(filepath.Join(f.panel, n)) {
			t.Errorf("%s is a hot file: a setting or a safety hold must be saved when written", n)
		}
	}
}

// The window can't be opened: the save fails as "read-only" (the POST says so), the setting is in use from RAM,
// and the Dashboard's banner says the fans are not affected.
func TestShadowRemountFailsBanner(t *testing.T) {
	f := withShadow(t)
	oldTmp, oldProbe := cfgTmp, cfgROProbe
	cfgTmp = filepath.Join(t.TempDir(), "x-")
	cfgROProbe = func(string) bool { return false } // (the RAM folder is never read-only)
	t.Cleanup(func() { cfgTmp, cfgROProbe = oldTmp, oldProbe })
	f.failRW = syscall.EIO
	c := defaultFanConfig()
	c.Profile = "curve-70"
	err := saveFanConfig(c)
	if err == nil || !errors.Is(err, syscall.EROFS) {
		t.Fatalf("err %v", err)
	}
	if s := cfgSaveErr(err); !strings.Contains(s, "read-only") || !strings.Contains(s, "could not be made writable") {
		t.Fatalf("answer %q", s)
	}
	if b, _ := os.ReadFile(*fanPath); !strings.Contains(string(b), "curve-70") {
		t.Fatal("the RAM copy wasn't written")
	}
	if _, ok := f.onFlash("scbox2/fan.json"); ok {
		t.Fatal("written to the flash without a window")
	}
	st := cfgStoreView()
	if st == nil || st.Kind != "read_only" || !strings.Contains(st.Msg, "keep reaching the fans") || strings.Contains(st.Msg, "NOT reaching") {
		t.Fatalf("banner %+v", st)
	}
}

// The fan curve, the abort tier's 100 % and the throttle never open a window: every fan step reaches the miner,
// none reaches the flash.
func TestShadowFanLoopNeverOpensAWindow(t *testing.T) {
	m := newHLMiner(t)
	f := withShadow(t)
	m.mu.Lock()
	json.Unmarshal([]byte(`{"manual":true,"tempcontrol":true,"select":0,"manualPowerplan":"690 MHz 42 V 50 RPM 41 RPM",
		"powerplans":[{"info":"690 MHz 42 V 50 RPM 41 RPM","level":0},{"info":"525 MHz 61 V 40 RPM 59 RPM","level":2}]}`), &m.setting)
	m.mu.Unlock()
	c := hlCfg()
	c.Smoothing = 0
	c.ThrottleEnable, c.ThrottleC, c.ResumeC = true, 85, 80
	temps := [][3]float64{{55, 55, 56}, {60, 61, 62}, {66, 67, 68}, {70, 71, 72}, {75, 76, 77}, {72, 73, 74}, {64, 65, 66}, {86, 86, 86.5}, {70, 70, 70}, {89, 89, 89.5}, {89, 89, 89.5}}
	for _, tt := range temps {
		m.setTemps(tt, tt, tt, tt)
		m.tick(c, 15*time.Second)
	}
	if n := m.count("plan"); n < 4 {
		t.Fatalf("the curve wrote only %d plan(s): %+v", n, m.putsCopy())
	}
	if f.opened() != 0 || f.seq() != "" {
		t.Fatalf("the fan loop opened %d window(s): %q", f.opened(), f.seq())
	}
	roCoolDown()
	t.Cleanup(func() { throttleOnDisk = throttleRec{} })
}

// Around an owner's change: what it wrote or removed directly in the panel's folder is saved; another file isn't.
func TestShadowUserSaveCommitsChanges(t *testing.T) {
	f := withShadow(t)
	f.putBoth(t, "scbox2/verbose", "", "")
	f.putBoth(t, "scbox2/untouched.json", "a", "a")
	os.WriteFile(filepath.Join(f.panel, "elsewhere.json"), []byte("1"), 0o644) // changed before the request: not this save's
	cfgUserSave(func() {
		os.Remove(filepath.Join(f.panel, "verbose"))
		os.WriteFile(filepath.Join(f.panel, "ntp"), []byte("10.0.0.1\n"), 0o644)
		os.WriteFile(filepath.Join(f.panel, "shares.json"), []byte(`{"best":0}`), 0o644) // a hot file the owner reset
	})
	if _, ok := f.onFlash("scbox2/verbose"); ok {
		t.Fatal("the removal wasn't saved")
	}
	if s, ok := f.onFlash("scbox2/ntp"); !ok || s != "10.0.0.1\n" {
		t.Fatal("the new file wasn't saved")
	}
	if _, ok := f.onFlash("scbox2/shares.json"); !ok {
		t.Fatal("the reset hot file wasn't saved")
	}
	if _, ok := f.onFlash("scbox2/elsewhere.json"); ok {
		t.Fatal("a file this change didn't touch was saved")
	}
	if f.opened() != 1 {
		t.Fatalf("%d windows for one change", f.opened())
	}
	// a change that writes nothing: no window
	cfgUserSave(func() {})
	if f.opened() != 1 {
		t.Fatal("a window for nothing")
	}
}

// The OEM's files: saved when the owner's change reached the OEM's backend, except a change of the fans alone.
func TestShadowOEMFilesFanOnlyNeverSaved(t *testing.T) {
	f := withShadow(t)
	cpb := func(plan string) string {
		return `{"name":"Benchmark","manual":true,"manualPowerplan":"` + plan + `","powerplans":[{"info":"690 MHz 42 V 50 RPM 41 RPM"}],"select":0}`
	}
	bfg := func(set, pool string) string {
		return `{"api-allow":"W:127.0.0.1","set":["intchains_qomo:vfff=` + set + `","intchains_qomo:algo=blake2b(SC)"],"pools":[{"url":"` + pool + `"}]}`
	}
	f.putBoth(t, "minerd/settings/cpbsetting.json", cpb("690 MHz 42 V 73 RPM 41 RPM"), cpb("690 MHz 42 V 50 RPM 41 RPM"))
	f.putBoth(t, "bfgminer/bfgminer.json", bfg("42.000000:690:73:41", "stratum+tcp://a:1"), bfg("42.000000:690:50:41", "stratum+tcp://a:1"))
	f.putBoth(t, "minerd/settings/pools.json", `{"pools":["a"]}`, `{"pools":["a"]}`)
	// the fan curve's steps (or the owner's own fan settings): never
	oemPersist(true)
	oemPersist(false)
	if f.opened() != 0 {
		t.Fatalf("a fan-only change opened %d window(s)", f.opened())
	}
	// the owner changes the pool: pools.json and bfgminer.json (not the plan file: no plan was written)
	f.putBoth(t, "minerd/settings/pools.json", `{"pools":["b"]}`, `{"pools":["a"]}`)
	os.WriteFile(filepath.Join(f.ram, "bfgminer/bfgminer.json"), []byte(bfg("42.000000:690:73:41", "stratum+tcp://b:1")), 0o600)
	oemPersist(false)
	if s, _ := f.onFlash("minerd/settings/pools.json"); s != `{"pools":["b"]}` {
		t.Fatalf("pools.json not saved: %q", s)
	}
	if s, _ := f.onFlash("bfgminer/bfgminer.json"); !strings.Contains(s, "b:1") {
		t.Fatalf("bfgminer.json not saved: %q", s)
	}
	if s, _ := f.onFlash("minerd/settings/cpbsetting.json"); !strings.Contains(s, "50 RPM") {
		t.Fatalf("the plan file was saved without a plan change: %q", s)
	}
	// the owner picks another clock: the plan file is saved (with the fan level of the moment)
	os.WriteFile(filepath.Join(f.ram, "minerd/settings/cpbsetting.json"), []byte(cpb("525 MHz 61 V 73 RPM 59 RPM")), 0o600)
	oemPersist(true)
	if s, _ := f.onFlash("minerd/settings/cpbsetting.json"); !strings.Contains(s, "525 MHz") {
		t.Fatalf("the new plan wasn't saved: %q", s)
	}
	// a throttle step (no owner's plan write): not kept as the owner's clock
	os.WriteFile(filepath.Join(f.ram, "minerd/settings/cpbsetting.json"), []byte(cpb("500 MHz 61 V 90 RPM 59 RPM")), 0o600)
	oemPersist(false)
	if s, _ := f.onFlash("minerd/settings/cpbsetting.json"); !strings.Contains(s, "525 MHz") {
		t.Fatalf("an automatic clock change was saved: %q", s)
	}
	// the OEM's temp files are never copied
	os.WriteFile(filepath.Join(f.ram, "minerd/settings/.login.json"), []byte("x"), 0o600)
	oemPersist(true)
	if _, ok := f.onFlash("minerd/settings/.login.json"); ok {
		t.Fatal("a temp file was saved")
	}
}

// cfgUserSave hands the plan files on only when the change wrote the plan (PUT /mcb/setting).
func TestShadowUserSaveOEMOnlyAfterAPut(t *testing.T) {
	f := withShadow(t)
	f.putBoth(t, "minerd/settings/ip.json", `{"dhcp":true}`, `{"dhcp":true}`)
	os.WriteFile(filepath.Join(f.ram, "minerd/settings/ip.json"), []byte(`{"dhcp":false}`), 0o600) // (the backend, by itself)
	cfgUserSave(func() {})
	if s, _ := f.onFlash("minerd/settings/ip.json"); s != `{"dhcp":true}` {
		t.Fatal("saved without a change reaching the OEM's backend")
	}
	cfgUserSave(func() { stockOtherPuts.Add(1) })
	if s, _ := f.onFlash("minerd/settings/ip.json"); s != `{"dhcp":false}` {
		t.Fatalf("not saved after the backend took the change: %q", s)
	}
	// the panel's own edit of the miner's config (the verbose switch): saved
	f.putBoth(t, "bfgminer/bfgminer.json", `{"set":["intchains_qomo:vfff=42.000000:690:50:41"]}`, `{"set":["intchains_qomo:vfff=42.000000:690:50:41"]}`)
	cfgUserSave(func() {
		os.WriteFile(filepath.Join(f.ram, "bfgminer/bfgminer.json"), []byte(`{"verbose":true,"set":["intchains_qomo:vfff=42.000000:690:50:41"]}`), 0o600)
	})
	if s, _ := f.onFlash("bfgminer/bfgminer.json"); !strings.Contains(s, "verbose") {
		t.Fatalf("the panel's edit of the miner's config wasn't saved: %q", s)
	}
}

// A safety hold survives a power cut (saved when set), and lifting it is saved too (it can't come back at boot).
func TestShadowHoldSetAndLifted(t *testing.T) {
	f := withShadow(t)
	poolHoldSet("no pool answers")
	if _, ok := f.onFlash("scbox2/pool-hold.json"); !ok {
		t.Fatal("the hold wasn't saved")
	}
	if err := poolHoldRelease(); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.onFlash("scbox2/pool-hold.json"); ok {
		t.Fatal("the lifted hold is still on the flash")
	}
	if _, ok := f.onFlash("scbox2/pool-hold.json.bak"); ok {
		t.Fatal("its .bak is still on the flash (the loaders fall back to it)")
	}
}

// requireAuth wraps an owner's change (not a GET) in cfgUserSave.
func TestShadowRequireAuthWrapsChanges(t *testing.T) {
	f := withShadow(t)
	h := func(w http.ResponseWriter, r *http.Request) {
		os.WriteFile(filepath.Join(f.panel, "display.json"), []byte(r.Method), 0o644)
	}
	oldAuth := auth
	a := authStore{Username: "owner", Acknowledged: "2026-01-01 00:00", Secret: base64.StdEncoding.EncodeToString(randBytes(32))}
	setPasswordSHA256(&a, "correct-horse-1")
	authMu.Lock()
	auth = a
	authMu.Unlock()
	saveAuth()
	t.Cleanup(func() { authUpgradeWG.Wait(); authMu.Lock(); auth = oldAuth; authMu.Unlock() })
	cookie := issueCookie("owner")
	for _, m := range []string{"GET", "POST"} {
		r := httptest.NewRequest(m, "/api/display", strings.NewReader("{}"))
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		r.Header.Set(panelReqHeader, panelReqValue)
		r.AddCookie(&http.Cookie{Name: "sid", Value: cookie})
		requireAuth(h)(httptest.NewRecorder(), r)
		s, ok := f.onFlash("scbox2/display.json")
		if m == "GET" && ok {
			t.Fatal("a GET saved to the flash")
		}
		if m == "POST" && (!ok || s != "POST") {
			t.Fatalf("the POST's file isn't on the flash (%q, %v)", s, ok)
		}
	}
}

// The SC Lite (and a Box II whose partition is used directly): no window, ever.
func TestShadowNotOnSCLite(t *testing.T) {
	f := withShadow(t)
	useModel(t, profSCLite)
	saveFanConfig(defaultFanConfig())
	cfgUserSave(func() { os.WriteFile(filepath.Join(f.panel, "x"), []byte("1"), 0o644) })
	cfgCheckpoint()
	oemPersist(true)
	if f.opened() != 0 {
		t.Fatalf("%d windows on the SC Lite", f.opened())
	}
	useModel(t, profSCBox2)
	cfgwin.ActiveHook = func() bool { return false }
	saveFanConfig(defaultFanConfig())
	if f.opened() != 0 {
		t.Fatal("a window with the partition used directly")
	}
}

// The fan levels are taken out of both plan files' shapes (and nothing else).
func TestOEMNoFans(t *testing.T) {
	for _, c := range [][3]string{
		{`{"manualPowerplan":"690 MHz 42 V 50 RPM 41 RPM"}`, `{"manualPowerplan":"690 MHz 42 V 100 RPM 41.0 RPM"}`, "same"},
		{`{"manualPowerplan":"690 MHz 42 V 50 RPM 41 RPM"}`, `{"manualPowerplan":"525 MHz 61 V 50 RPM 41 RPM"}`, "differ"},
		{`{"set":["intchains_qomo:vfff=42.000000:690:50:41"]}`, `{"set":["intchains_qomo:vfff=42.000000:690:90:41"]}`, "same"},
		{`{"set":["intchains_qomo:vfff=42.000000:690:50:41"]}`, `{"set":["intchains_qomo:vfff=61.000000:525:50:41"]}`, "differ"},
		{`{"b":1,"a":2}`, `{"a":2, "b":1}`, "same"},
	} {
		same := oemNoFans([]byte(c[0])) == oemNoFans([]byte(c[1]))
		if same != (c[2] == "same") {
			t.Errorf("%s vs %s: same=%v, want %s (%q)", c[0], c[1], same, c[2], oemNoFans([]byte(c[0])))
		}
	}
	_ = fmt.Sprint
}

func TestOEMSameJSONIgnoresKeyOrder(t *testing.T) {
	a := []byte(`[{"url":"stratum+tcp://pool.example:3333","legal":true,"active":true,"user":"w.1","pass":"x"}]`)
	b := []byte("[\n {\n  \"legal\": true,\n  \"url\": \"stratum+tcp://pool.example:3333\",\n  \"user\": \"w.1\",\n  \"active\": true,\n  \"pass\": \"x\"\n }\n]")
	if !oemSameJSON(a, b) {
		t.Fatal("same pools in another key order must compare equal")
	}
	c := []byte(`[{"url":"stratum+tcp://other.example:3333","legal":true,"active":true,"user":"w.1","pass":"x"}]`)
	if oemSameJSON(a, c) {
		t.Fatal("a changed pool URL must not compare equal")
	}
	if oemSameJSON([]byte("not json"), []byte("not json ")) {
		t.Fatal("non-JSON is never 'the same JSON'")
	}
}
