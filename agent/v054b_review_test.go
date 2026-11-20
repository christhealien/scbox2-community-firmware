package main

// v0.5.4 review fixes: the tuner keeps its heat abort, fan hold and Stop while the SC Box II confirms a clock write
// (T1), the confirmation's quiet first 30 s (T2), the fan hold's Box II limits and anti-windup (T3), the tuner's
// final setting kept across a reboot and bfgminer.json kept consistent (T5), the system-log relay (S1-S3), the
// update result note through the config write path (U2), the update progress asking before it concludes (P1), and
// the build script's default (R1).

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// v054Engine: a real Box II tuner engine on the c34 fake miner (clock control on), the fan hold on, the temperature
// and fan reads mocked.
func v054Engine(t *testing.T) (*tunerEngine, *c34Fake, *atomic.Value, *[]int, *sync.Mutex) {
	t.Helper()
	f := newC34(t, true, false)
	cfg := defaultTunerConfig()
	box2TunerDefaults(&cfg)
	cfg.DryRun, cfg.FanHold, cfg.FanTargetC, cfg.FanMinPct, cfg.PollS = false, true, 72, 30, 1
	e := newTunerEngine(cfg)
	e.begin()
	temp := &atomic.Value{}
	temp.Store(70.0)
	e.readTemp = func() (float64, bool) { return temp.Load().(float64), true }
	e.readFan = func() (int, bool) { return 50, true }
	e.fanFloor, e.fanGuard = nil, nil
	var mu sync.Mutex
	fans := &[]int{}
	e.applyFan = func(p int) error { mu.Lock(); *fans = append(*fans, p); mu.Unlock(); return nil }
	e.holdTarget = 72
	return e, f, temp, fans, &mu
}

// T1: a clock confirmation that would wait its whole deadline ends at the heat abort, with the fan hold running
// meanwhile, instead of blocking the tuner.
func TestV054CoopConfirmHeatAbort(t *testing.T) {
	e, f, temp, fans, mu := v054Engine(t)
	f.set(func() { f.ignore = true }) // the driver never reports the new clock: the confirm would wait it out
	box2ConfirmWait = 20 * time.Second
	temp.Store(85.0) // over the 82 °C abort
	start := time.Now()
	err := e.applyPlan(planMV{MHz: 650, MV: box2LevelOf(42)})
	var ae abortErr
	if !errors.As(err, &ae) || !strings.Contains(ae.msg, "°C") {
		t.Fatalf("want the heat abort, got %v", err)
	}
	if d := time.Since(start); d > 8*time.Second {
		t.Fatalf("the abort took %s: the confirm wasn't cooperative", d)
	}
	mu.Lock()
	n := len(*fans)
	mu.Unlock()
	if n == 0 || (*fans)[n-1] != 100 {
		t.Fatalf("the fan hold didn't run during the confirm (writes %v)", *fans)
	}
	// and the settle turns it into the test's abort, not an "apply failed"
	e2, f2, temp2, _, _ := v054Engine(t)
	f2.set(func() { f2.ignore = true })
	box2ConfirmWait = 20 * time.Second
	temp2.Store(85.0)
	if err := e2.settle(planMV{MHz: 650, MV: box2LevelOf(42)}, 0.01); err == nil || !strings.Contains(err.Error(), "°C") || strings.Contains(err.Error(), "apply failed") {
		t.Fatalf("settle: %v", err)
	}
}

// T1: Stop during a confirmation ends the wait within about a second (the confirm loop itself stops), and the end's
// put-back is not cut short by the same Stop.
func TestV054CoopConfirmStop(t *testing.T) {
	e, f, _, _, _ := v054Engine(t)
	f.set(func() { f.ignore = true })
	box2ConfirmWait = 20 * time.Second
	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- e.applyPlan(planMV{MHz: 650, MV: box2LevelOf(42)}) }()
	time.Sleep(300 * time.Millisecond)
	e.Stop()
	select {
	case err := <-done:
		if err != errTunerStopped {
			t.Fatalf("want errTunerStopped, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Stop didn't end the confirmation")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("Stop took %s", d)
	}
	// the end puts the miner back: its confirmation runs to the end although the run is stopped
	f.set(func() { f.ignore = false })
	e.ending.Store(true)
	if err := e.applyPlan(planMV{MHz: 690, MV: box2LevelOf(42)}); err != nil {
		t.Fatalf("the put-back after a Stop: %v", err)
	}
}

// T1: before a clock raise the fans go to at least 70 % in a write of their own, then the clock.
func TestV054FansBeforeRaise(t *testing.T) {
	e, f, _, _, _ := v054Engine(t)
	if err := e.applyPlan(planMV{MHz: 600, MV: box2LevelOf(42)}); err != nil { // down: no fan write first
		t.Fatal(err)
	}
	plans := f.putPlans()
	if len(plans) != 1 || !strings.HasPrefix(plans[0], "600 MHz") {
		t.Fatalf("a clock decrease: %q", plans)
	}
	e.fanCur = 55
	if err := e.applyPlan(planMV{MHz: 650, MV: box2LevelOf(42)}); err != nil {
		t.Fatal(err)
	}
	plans = f.putPlans()
	if len(plans) != 3 || !strings.HasPrefix(plans[1], "600 MHz 42 V 70 RPM") || !strings.HasPrefix(plans[2], "650 MHz") {
		t.Fatalf("a raise: the fans to 70 %% first, then the clock: %q", plans)
	}
	if e.fanCur != 70 {
		t.Fatalf("the hold carries on from %d", e.fanCur)
	}
	// the hold's own value when higher
	e.fanCur = 85
	if err := e.applyPlan(planMV{MHz: 675, MV: box2LevelOf(42)}); err != nil {
		t.Fatal(err)
	}
	plans = f.putPlans()
	if !strings.HasPrefix(plans[len(plans)-2], "650 MHz 42 V 85 RPM") {
		t.Fatalf("the hold's 85 %% first: %q", plans)
	}
}

// T2: no devs poll for box2ConfirmQuiet after the write, then every box2ConfirmPollSlow; the deadline unchanged.
func TestV054ConfirmQuietThenSlow(t *testing.T) {
	if box2ConfirmQuietDefault < 30*time.Second {
		t.Fatalf("the quiet time is %s", box2ConfirmQuietDefault)
	}
	f := newC34(t, true, false)
	oldQ, oldSlow := box2ConfirmQuiet, box2ConfirmPollSlow
	t.Cleanup(func() { box2ConfirmQuiet, box2ConfirmPollSlow = oldQ, oldSlow })
	box2ConfirmQuiet, box2ConfirmPollSlow, box2ConfirmWait = 400*time.Millisecond, 150*time.Millisecond, 1200*time.Millisecond
	if d := box2ConfirmPollAt(0); d != 400*time.Millisecond {
		t.Fatalf("PollAt(0) = %s", d)
	}
	if d := box2ConfirmPollAt(500 * time.Millisecond); d != box2ConfirmPollSlow {
		t.Fatalf("after the quiet time: %s", d)
	}
	var mu sync.Mutex
	var at []time.Duration
	var t0 time.Time
	devs := box2DevsCmd
	box2DevsCmd = func() ([]byte, error) { mu.Lock(); at = append(at, time.Since(t0)); mu.Unlock(); return devs() }
	f.set(func() { f.ignore = true })
	t0 = time.Now()
	_, err := box2ApplyControl(noGen, powerplan{FreqMHz: 650})
	if err == nil || !strings.Contains(err.Error(), "not confirmed") {
		t.Fatalf("an unconfirmed write: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(at) == 0 || at[0] < 380*time.Millisecond {
		t.Fatalf("devs asked during the quiet time: %v", at)
	}
	for i := 1; i < len(at); i++ {
		if at[i]-at[i-1] < 120*time.Millisecond {
			t.Fatalf("polled faster than every %s: %v", box2ConfirmPollSlow, at)
		}
	}
	if last := at[len(at)-1]; last < 1200*time.Millisecond { // (the 690 → 650 deadline: 1.2 s + 4 × 0.4 s)
		t.Fatalf("reverted before the deadline: %v", at)
	}
}

// T3: the Box II hold: target at most 74 °C, full fan from min(target+8, abort−2), no wind-up while step-limited,
// bumpless after full fan and after a raise from outside.
func TestV054FanHoldBox2(t *testing.T) {
	c8Box2(t, true, vcodeDirUnknown)
	d := defaultTunerConfig()
	box2TunerDefaults(&d)
	d.FanHold = true
	for tg, ok := range map[float64]bool{72: true, 74: true, 74.5: false, 75: false, 77: false} {
		c := d
		c.FanTargetC = tg
		if err := validateTunerConfig(c); (err == nil) != ok {
			t.Errorf("target %v: %v", tg, err)
		}
	}
	if d.WarmestC > box2HoldMaxC || d.FanTargetC > box2HoldMaxC {
		t.Fatalf("defaults over the cap: %+v", d)
	}
	// full fan: abort 82, target 74 → from 80 °C (not 82)
	h := fanHold{init: true, base: 60, gentle: true, abortC: 82}
	if w := h.want(79.9, 74, 60, 30, 100); w == 100 {
		t.Fatal("full fan below abort−2")
	}
	if w := h.want(80, 74, 60, 30, 100); w != 100 {
		t.Fatalf("not full fan at abort−2: %d", w)
	}
	// bumpless off full fan: the integral carries the level it drops back to
	h.steady, h.haveSteady = 70, true
	out := h.want(75, 74, 72, 30, 100)
	if next := h.base + 2.0*(75-74) + h.i; out != 70 || next < float64(out)-0.01 || next > float64(out)+0.01 {
		t.Fatalf("off full fan: out %d, next %.2f", out, next)
	}
	h.full = true
	if h.want(75, 74, 100, 30, 100); h.i > 25 {
		t.Fatalf("the integral past its limit: %.2f", h.i)
	}
	// anti-windup: far over the target with the fans held back by the step limit, the integral doesn't grow
	h2 := fanHold{init: true, base: 40, gentle: true, abortC: 82}
	h2.want(75.5, 72, 40, 30, 100) // (err 3.5: output 40+7+0.5 > 40+4 → limited)
	if h2.i != 0 {
		t.Fatalf("the integral grew while step-limited: %.2f", h2.i)
	}
	h3 := fanHold{init: true, base: 60, gentle: true, abortC: 82}
	h3.want(73, 72, 60, 30, 100) // not limited: grows
	if h3.i <= 0 {
		t.Fatalf("the integral didn't grow when free: %.2f", h3.i)
	}
	// the SC Lite controller is unchanged (no abort rule, no anti-windup)
	hl := fanHold{init: true, base: 40}
	hl.want(75.5, 72, 40, 30, 100)
	if hl.i == 0 {
		t.Fatal("the SC Lite controller changed")
	}
	// rebase: after the tuner's own raise to 70, the next reading starts from 70
	h4 := fanHold{init: true, base: 69, gentle: true, abortC: 82, i: -25, lastT: 68, haveLast: true}
	h4.rebase(70, 72)
	if w := h4.want(68, 72, 70, 30, 100); w < 69 {
		t.Fatalf("after the raise the hold pulled the fans to %d", w)
	}
}

// T4: memoTemp only on the Box II tuner.
func TestV054MemoTempBox2Only(t *testing.T) {
	e := newTunerEngine(defaultTunerConfig()) // SC Lite
	if e.readTemp == nil {
		t.Fatal("no reader")
	}
	src, _ := os.ReadFile("tuner.go")
	if !strings.Contains(string(src), "if tunerOnBox2() { // (the SC Box II: each Dashboard read") {
		t.Fatal("memoTemp is not limited to the Box II")
	}
}

// T5: a run that ends on its confirmed best saves the plan for the next boot; one that doesn't, doesn't.
func TestV054TunerEndPersists(t *testing.T) {
	c8Box2(t, true, vcodeDirUnknown)
	var got []planMV
	old := box2PersistPlan
	box2PersistPlan = func(mhz, code int) { got = append(got, planMV{MHz: mhz, MV: code}) }
	t.Cleanup(func() { box2PersistPlan = old })
	cfg, _ := box2TunerNormalize(c8Cfg())
	e, _, _ := c8Engine(t, cfg, 0, 650)
	e.Run()
	if !e.confirmed || len(got) != 1 || got[0].MHz != 650 || got[0].MV != 42 {
		t.Fatalf("confirmed %v, persisted %v", e.confirmed, got)
	}
	got = nil
	e2, _, _ := c8Engine(t, cfg, 0, 500) // nothing clean below stock: no confirmed best
	e2.cfg.ConfirmWin = true
	e2.Run()
	for _, p := range got {
		if p.MHz != 690 {
			t.Fatalf("an unconfirmed end persisted %v", got)
		}
	}
}

// T5: without the owner's plan write, bfgminer.json is not saved while its clock differs from the flash's (a
// throttle step), even when its pools changed; with the plan, it is.
func TestV054BfgminerConsistent(t *testing.T) {
	f := withShadow(t)
	bfg := func(set, pool string) string {
		return `{"api-allow":"W:127.0.0.1","set":["intchains_qomo:vfff=` + set + `"],"pools":[{"url":"` + pool + `"}]}`
	}
	f.putBoth(t, "bfgminer/bfgminer.json", bfg("42.000000:500:90:41", "stratum+tcp://b:1"), bfg("42.000000:690:50:41", "stratum+tcp://a:1"))
	oemPersist(false)
	if s, _ := f.onFlash("bfgminer/bfgminer.json"); !strings.Contains(s, ":690:") || strings.Contains(s, ":500:") {
		t.Fatalf("a background clock was saved in bfgminer.json: %q", s)
	}
	cfgCheckpoint()
	if s, _ := f.onFlash("bfgminer/bfgminer.json"); strings.Contains(s, ":500:") {
		t.Fatalf("the clean-reboot checkpoint saved a background clock: %q", s)
	}
	oemPersist(true)
	if s, _ := f.onFlash("bfgminer/bfgminer.json"); !strings.Contains(s, ":500:") {
		t.Fatalf("the owner's plan wasn't saved: %q", s)
	}
}

// S1: an IPv4 address only, no host names.
func TestV054SyslogIPv4Only(t *testing.T) {
	for in, want := range map[string]string{"10.0.0.5": "10.0.0.5:514", "192.168.1.20:1514": "192.168.1.20:1514"} {
		if got, err := syslogRemoteNorm(in); err != nil || got != want {
			t.Errorf("%q: %q %v", in, got, err)
		}
	}
	for _, in := range []string{"logs.lan:514", "pc-1", "localhost:514", "0.0.0.0:514", "224.0.0.1:514", "192.168.001.020:514", "1.2.3:514", "1.2.3.4.5:514"} {
		if got, err := syslogRemoteNorm(in); err == nil {
			t.Errorf("%q accepted as %q", in, got)
		}
	}
}

// S1: the boot script's syslog step waits for the old syslogd (killing it after ~5 s), never starts a second one,
// and takes IPv4 only. Every SC Box II build relays (Settings → Troubleshooting): no build check, nothing removed.
func TestV054S48SyslogStep(t *testing.T) {
	src, err := os.ReadFile("../kit/files/S48scbox2")
	if err != nil {
		t.Skip(err)
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	s := string(src)
	if strings.Contains(s, "build-variant") || strings.Contains(s, "public_build") {
		t.Fatal("the boot script still asks which build it is")
	}
	fn := s[strings.Index(s, "syslog_addr_ok() {"):strings.Index(s, "wait_for() {")]
	run := func(t *testing.T, remote, variant string, stubborn, unkillable bool) (string, string, error) {
		d := t.TempDir()
		os.MkdirAll(d+"/cfg", 0o755)
		os.MkdirAll(d+"/run", 0o755)
		if remote != "" {
			os.WriteFile(d+"/cfg/syslog-remote", []byte(remote+"\n"), 0o644)
		}
		os.WriteFile(d+"/state", []byte("100\n"), 0o644) // syslogd runs (pid 100), stock
		os.WriteFile(d+"/run/syslogd.pid", []byte("100\n"), 0o644)
		os.MkdirAll(d+"/proc/100", 0o755)
		os.WriteFile(d+"/proc/100/cmdline", []byte("/sbin/syslogd\x00-n\x00-R\x001.2.3.4:514"), 0o644)
		f := strings.NewReplacer("/var/run/", d+"/run/", "/proc/", d+"/proc/").Replace(fn)
		os.MkdirAll(d+"/bin", 0o755)
		os.WriteFile(d+"/bin/start-stop-daemon", []byte(`#!/bin/sh
case "$*" in
*-K*) [ `+map[bool]string{true: "1", false: "0"}[stubborn]+` = 1 ] || : > `+d+`/state ;;
*-S*) if [ -s `+d+`/state ]; then echo TWO >> `+d+`/log; fi; echo 200 > `+d+`/state; echo "start $*" >> `+d+`/log ;;
esac
`), 0o755)
		script := `CFG=` + d + `/cfg
SAVE=true
PATH=` + d + `/bin:$PATH
pidof() { [ -s ` + d + `/state ] && cat ` + d + `/state; [ -s ` + d + `/state ]; }
usleep() { :; }
kill() { [ ` + map[bool]string{true: "1", false: "0"}[unkillable] + ` = 1 ] || : > ` + d + `/state; echo "kill $*" >> ` + d + `/log; }
` + f + "\nsyslog_remote\n"
		os.WriteFile(d+"/t.sh", []byte(script), 0o755)
		out, err := exec.Command(sh, d+"/t.sh").CombinedOutput()
		log, _ := os.ReadFile(d + "/log")
		_, ferr := os.Stat(d + "/cfg/syslog-remote")
		return string(out) + "|file=" + map[bool]string{true: "yes", false: "no"}[ferr == nil], string(log), err
	}
	if out, log, err := run(t, "192.168.1.20:514", "owner", false, false); err != nil || !strings.Contains(log, "-R 192.168.1.20:514 -L") || strings.Contains(log, "TWO") || strings.Contains(log, "kill") {
		t.Fatalf("owner, normal: %v %s %s", err, out, log)
	}
	if out, log, err := run(t, "192.168.1.20:514", "owner", true, false); err != nil || !strings.Contains(log, "kill -9 100") || strings.Contains(log, "TWO") || !strings.Contains(log, "start") {
		t.Fatalf("owner, an old syslogd that doesn't exit: %v %s %s", err, out, log)
	}
	if out, log, err := run(t, "192.168.1.20:514", "owner", true, true); err == nil || strings.Contains(log, "start") {
		t.Fatalf("an old syslogd that can't be killed: no second one, exit 1: %v %s %s", err, out, log)
	}
	if out, log, _ := run(t, "logs.lan:514", "owner", false, false); strings.Contains(log, "logs.lan") || !strings.Contains(log, "start -b -S -q -m -p") || strings.Contains(log, "-R") {
		t.Fatalf("a host name: syslogd as stock, no relay: %s %s", out, log)
	}
	// v0.5.4c: a strict IPv4 check (exactly 4 octets 0-255, a port 1-65535): anything else is ignored with one line
	// and syslogd runs as stock
	for _, bad := range []string{"1.2.3.999", "1.2.3.:514", "1.2.3.4:0", "1.2.3.4:70000", "logs.lan", "01.2.3.4:514", "1.2.3.4:5:6"} {
		out, log, err := run(t, bad, "owner", false, false)
		if err != nil || strings.Contains(log, "-R") || !strings.Contains(log, "start -b -S -q -m -p") || strings.Count(out, "syslog-remote ignored") != 1 || strings.Contains(log, "TWO") {
			t.Fatalf("%q: syslogd as stock, one line: %v %s %s", bad, err, out, log)
		}
	}
	if out, log, err := run(t, "10.0.0.5", "owner", false, false); err != nil || !strings.Contains(log, "-R 10.0.0.5:514 -L") || strings.Contains(out, "ignored") {
		t.Fatalf("a valid address without a port: %v %s %s", err, out, log)
	}
	if out, log, err := run(t, "192.168.1.20:65535", "owner", false, false); err != nil || !strings.Contains(log, "-R 192.168.1.20:65535 -L") {
		t.Fatalf("a valid address: %v %s %s", err, out, log)
	}
	if out, log, err := run(t, "192.168.1.20:514", "public", false, false); err != nil || !strings.Contains(log, "-R 192.168.1.20:514 -L") || !strings.Contains(out, "file=yes") {
		t.Fatalf("every build relays and keeps the setting: %v %s %s", err, out, log)
	}
}

// S2 (since the Troubleshooting panel): every SC Box II build offers the relay, the public one included, and a saved
// setting is kept and read back (nothing clears it at start); the SC Lite never offers it.
func TestV054SyslogRemoteEveryBox2Build(t *testing.T) {
	useBox2(t)
	*authFile = filepath.Join(t.TempDir(), "auth.json")
	if !syslogRemoteAvail() {
		t.Fatal("the relay isn't offered on the SC Box II")
	}
	os.WriteFile(syslogRemotePath(), []byte("192.168.1.20:514\n"), 0o644)
	if v := recoveryView(); v["syslog_remote_avail"] != true || v["syslog_remote"] != "192.168.1.20:514" {
		t.Fatalf("the saved setting isn't read back: %v", v)
	}
	if _, err := os.Stat(syslogRemotePath()); err != nil {
		t.Fatal("the relay setting was removed")
	}
	model, modelWhy = profSCLite, "test"
	setWindowsFromModel()
	if syslogRemoteAvail() || syslogRemoteGet() != "" {
		t.Fatal("offered on the SC Lite")
	}
	if _, ok := recoveryView()["syslog_remote_avail"]; ok {
		t.Fatal("the SC Lite's Recovery view offers it")
	}
}

// S3: a syslogd restart that failed: the answer and the card say so, not "On".
func TestV054SyslogApplyFailShown(t *testing.T) {
	useBox2(t)
	*authFile = filepath.Join(t.TempDir(), "auth.json")
	fail := true
	old := syslogRemoteApply
	syslogRemoteApply = func() error {
		if fail {
			return errors.New("restarting the system log failed (exit status 1 the old syslogd doesn't exit)")
		}
		return nil
	}
	t.Cleanup(func() { syslogRemoteApply = old; syslogApplyErrSet("") })
	post := func(body string) map[string]any {
		w := httptest.NewRecorder()
		handleRecovery(w, httptest.NewRequest(http.MethodPost, "/api/recovery", strings.NewReader(body)))
		var v map[string]any
		json.Unmarshal(w.Body.Bytes(), &v)
		return v
	}
	if v := post(`{"syslog_remote":"192.168.1.20:514"}`); v["ok"] != false || !strings.Contains(v["err"].(string), "saved, but") {
		t.Fatalf("a failed restart: %v", v)
	}
	if v := recoveryView(); v["syslog_remote_err"] == nil {
		t.Fatalf("the card isn't told: %v", v)
	}
	out := l34Node(t, troubleshootingJS, `
renderTroubleshooting({ok:true,syslog_remote_avail:true,syslog_remote:'192.168.1.20:514',syslog_remote_err:'restarting the system log failed'});console.log('=== ERR '+$('tsbody').innerHTML);
renderTroubleshooting({ok:true,syslog_remote_avail:true,syslog_remote:'192.168.1.20:514'});console.log('=== OK '+$('tsbody').innerHTML);`)
	if e := between(out, "=== ERR ", "\n"); strings.Contains(e, "On: every line") || !strings.Contains(e, "couldn’t be restarted") {
		t.Fatalf("the card after a failed restart: %s", e)
	}
	if o := between(out, "=== OK ", "\n"); !strings.Contains(o, "On: every line") {
		t.Fatalf("the card after a good save: %s", o)
	}
	fail = false
	if v := post(`{"syslog_remote":"192.168.1.20:514"}`); v["ok"] != true || v["syslog_remote_err"] != nil {
		t.Fatalf("a good save clears it: %v", v)
	}
}

// U2: the update result note goes through the config write path: on the Box II with its settings in RAM it is on
// the flash (a reboot keeps it).
func TestV054WriteLastPersists(t *testing.T) {
	f := withShadow(t)
	writeLast("installed", "0.5.3", "0.5.4", "the system partition reads back as the image written")
	s, ok := f.onFlash("scbox2/update/last.json")
	if !ok || !strings.Contains(s, `"result":"installed"`) {
		t.Fatalf("not on the flash: %q %v", s, ok)
	}
}

// U3: the "clean reboot" note is taken off again when the install's handoff fails.
func TestV054CleanMarkerClearedOnHandoffFail(t *testing.T) {
	src, _ := os.ReadFile("updimage.go")
	s := string(src)
	i := strings.Index(s, "if err := imgHandoff(imgStage); err != nil {")
	if i < 0 || !strings.Contains(s[i:i+300], "imgClearClean()") {
		t.Fatal("a failed handoff leaves the clean-reboot note")
	}
	j := strings.Index(s, "case errors.Is(err, errImgNothingWritten):")
	if j < 0 || !strings.Contains(s[j:j+300], "imgClearClean()") {
		t.Fatal("an install that wrote nothing leaves the clean-reboot note")
	}
	*authFile = filepath.Join(t.TempDir(), "auth.json")
	os.WriteFile(recCleanPath(), []byte("x"), 0o644)
	imgClearClean()
	if _, err := os.Stat(recCleanPath()); err == nil {
		t.Fatal("imgClearClean left the note")
	}
}

// P1: before saying "didn't start" (20 s / 90 s), the page asks /api/update: a result written after the click, or
// the version it was installing already running, is a result.
func TestV054UpdProgAsksFirst(t *testing.T) {
	out := l34Node(t, updProgFuncs, updProgPrelude+`
refresh=async()=>{};
// a result noted after the click (the page never saw the panel go away)
fresh();window._upd={version:'0.5.2'};updProgSet('v0.5.3','image',true);window._updProg.at=Date.now()-25000;
globalThis.fetch=async()=>({json:async()=>({version:'0.5.2',last:{result:'failed',to:'0.5.3',note:'the image changed after it was checked',at:Math.floor(Date.now()/1000)-10}})});
await updProgBack();console.log('=== NOTED '+(mem['fw-updprog']?'kept':'gone')+' '+calls.toast.join(' | ')+' done='+window._updDone);
// the new version already running
fresh();window._upd={version:'0.5.2'};updProgSet('v0.5.3','image',false);window._updProg.at=Date.now()-95000;
globalThis.fetch=async()=>({json:async()=>({version:'0.5.3'})});
await updProgBack();console.log('=== MOVED '+calls.toast.join(' | ')+' done='+window._updDone);
// an old note and the old version: didn't start
fresh();window._upd={version:'0.5.2'};updProgSet('v0.5.3','image',true);window._updProg.at=Date.now()-25000;
globalThis.fetch=async()=>({json:async()=>({version:'0.5.2',last:{result:'installed',at:Math.floor(Date.now()/1000)-86400}})});
await updProgBack();console.log('=== OLD '+calls.toast.join(' | '));
// busy: wait
fresh();window._upd={version:'0.5.2'};updProgSet('v0.5.3','image',true);window._updProg.at=Date.now()-25000;
globalThis.fetch=async()=>({json:async()=>({version:'0.5.2',busy:'installing'})});
await updProgBack();console.log('=== BUSY '+(mem['fw-updprog']?'kept':'gone')+' '+calls.toast.join(' | '));
// /api/update doesn't answer: nothing concluded
fresh();window._upd={version:'0.5.2'};updProgSet('v0.5.3','image',true);window._updProg.at=Date.now()-25000;
globalThis.fetch=async()=>{throw new TypeError('Failed to fetch');};
await updProgBack();console.log('=== NOANSWER '+(mem['fw-updprog']?'kept':'gone')+' '+calls.toast.join(' | '));`)
	get := func(k string) string { return between(out, "=== "+k+" ", "\n") }
	if s := get("NOTED"); !strings.HasPrefix(s, "gone") || strings.Contains(s, "didn’t start") || !strings.Contains(s, "the image changed after it was checked") {
		t.Errorf("a result noted after the click: %s\n%s", s, out)
	}
	if s := get("MOVED"); strings.Contains(s, "didn’t start") || !strings.Contains(s, "now running v0.5.3") {
		t.Errorf("already running the new version: %s", s)
	}
	if s := get("OLD"); !strings.Contains(s, "The install didn’t start") {
		t.Errorf("an old note: %s", s)
	}
	if s := get("BUSY"); !strings.HasPrefix(s, "kept") || strings.Contains(s, "didn’t start") {
		t.Errorf("busy: %s", s)
	}
	if s := get("NOANSWER"); !strings.HasPrefix(s, "kept") || strings.Contains(s, "didn’t start") {
		t.Errorf("no answer: %s", s)
	}
}

// P2/P3: the probe's Stop shows a refusal, and a {"error":…} answer (sign-in, same-page check) reads as j.err.
func TestV054JpostErrorAndProbeStop(t *testing.T) {
	out := l34Node(t, nil, l34Func(t, "jpost")+`
globalThis.fetch=async()=>({status:401,json:async()=>({error:'unauthorized'})});
console.log('=== N '+JSON.stringify((await jpost('/api/x',{})).j));
globalThis.fetch=async()=>({status:200,json:async()=>({ok:false,err:'x',error:'y'})});
console.log('=== K '+JSON.stringify((await jpost('/api/x',{})).j));`)
	if n := between(out, "=== N ", "\n"); !strings.Contains(n, `"err":"unauthorized"`) || !strings.Contains(n, `"ok":false`) {
		t.Fatalf("{error} not read as err: %s", n)
	}
	if k := between(out, "=== K ", "\n"); !strings.Contains(k, `"err":"x"`) {
		t.Fatalf("an err was replaced: %s", k)
	}
	src, _ := os.ReadFile("main.go")
	s := string(src)
	i := strings.Index(s, "async function ocStopProbe(){")
	if i < 0 || !strings.Contains(s[i:i+300], "toast(") {
		t.Fatal("ocStopProbe shows no error")
	}
	if !strings.Contains(s, "if(j&&typeof j==='object'&&!j.err&&j.error){j.err=String(j.error);") {
		t.Fatal("jpost doesn't normalise {error}")
	}
}

// P5: the Box II clock − / + go to the next clock the chips run.
func TestV054ClockStepGrid(t *testing.T) {
	out := l34Node(t, []string{"cstep"}, `
globalThis.ctrlWarn=()=>{};globalThis.document=globalThis.document||{};const el={value:'690',step:'1',min:'525',max:'750',dataset:{grid:'12.5'},disabled:false};document.getElementById=()=>el;
const up=[],dn=[];for(let i=0;i<5;i++){cstep('c_freq',1);up.push(el.value);}el.value='690';for(let i=0;i<3;i++){cstep('c_freq',-1);dn.push(el.value);}
el.value='700';cstep('c_freq',-1);console.log('=== G '+up.join(',')+' | '+dn.join(',')+' | '+el.value);`)
	if g := between(out, "=== G ", "\n"); g != "700,713,725,738,750 | 675,663,650 | 688" {
		t.Fatalf("grid steps: %s\n%s", g, out)
	}
}

// R1: the build script builds the public release unless told otherwise.
func TestV054BuildScriptDefaultPublic(t *testing.T) {
	b, err := os.ReadFile("../kit/tools/build-agent-box2.sh")
	if err != nil {
		t.Skip(err)
	}
	if !strings.Contains(string(b), "TAGS=${TAGS:-scbox2,public}") {
		t.Fatal("the default TAGS isn't the public build")
	}
}

// R5: the public build serves the page without the owner-only parts; the owner's keeps them. The system-log relay
// (Settings → Troubleshooting) is in both.
func TestV054PublicPageStripped(t *testing.T) {
	pub, own := pageForVariant(indexHTML, true), pageForVariant(indexHTML, false)
	for _, s := range []string{"id=\"tspanel\"", "<b>Troubleshooting</b>", "function renderTroubleshooting(", "Send the system log to this computer", "recSet({syslog_remote:"} {
		if !strings.Contains(pub, s) || !strings.Contains(own, s) {
			t.Errorf("%q isn't in both pages", s)
		}
	}
	for _, s := range []string{"id=\"companel\"", "function renderCommission(", "This is the commissioning build", "owner{"} {
		if strings.Contains(pub, s) {
			t.Errorf("the public page still has %q", s)
		}
		if s != "owner{" && !strings.Contains(own, s) {
			t.Errorf("the owner's page lost %q", s)
		}
	}
	if !strings.Contains(pub, "function renderModelBanner(m){const el=$('modelbanner');if(el){el.classList.add('hide');") {
		t.Error("the public page has no stand-in banner")
	}
	if publicBuild != strings.Contains(indexPage, "function renderModelBanner(m){const el=$('modelbanner');if(el){el.classList.add('hide');el.innerHTML='';}}\n") {
		t.Error("the served page isn't the build's variant")
	}
}
