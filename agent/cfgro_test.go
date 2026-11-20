package main

// A settings storage that can't be written (the Box II's FAT, remounted read-only by the kernel after a power cut):
// the fan curve, the abort tier's 100 %, the throttle and the overheat Idle keep working from memory; the fan
// settings the owner saves are used even though they can't be kept; the Dashboard says so once (banner, activity,
// notification); and every POST that couldn't save answers ok:false with the reason.

import (
	"bytes"
	"encoding/json"
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
)

// roStore: every settings write fails with EROFS (as on a FAT the kernel remounted read-only), and the statfs probe
// says read-only. Everything is put back after the test.
func roStore(t *testing.T) {
	t.Helper()
	oldFault, oldProbe, oldTmp := cfgWriteFault, cfgROProbe, cfgTmp
	cfgWriteFault = func(string) error { return &os.PathError{Op: "open", Path: "x.tmp", Err: syscall.EROFS} }
	cfgROProbe = func(string) bool { return true }
	cfgTmp = filepath.Join(t.TempDir(), "x-")
	cfgROReset()
	t.Cleanup(func() {
		cfgWriteFault, cfgROProbe, cfgTmp = oldFault, oldProbe, oldTmp
		cfgROReset()
		roCoolDown()
	})
}

// roCoolDown: the hot readings these tests leave behind (the heat guard's last reading, the abort tier) don't carry
// into the next test.
func roCoolDown() {
	hgMu.Lock()
	hgHot, hgAt = 0, time.Time{}
	hgMu.Unlock()
	abortTierOn.Store(false)
}

func cfgROReset() {
	cfgROSeen.Store(false)
	cfgFailLogged.Store(false)
	fanWriteFailing.Store(false)
	cfgROMu.Lock()
	cfgROAt = time.Time{}
	cfgROMu.Unlock()
	cfgStoreSaid.Range(func(k, _ any) bool { cfgStoreSaid.Delete(k); return true })
}

func lastPlanFan(t *testing.T, m *hlMiner) string {
	t.Helper()
	ps := m.putsCopy()
	for i := len(ps) - 1; i >= 0; i-- {
		if ps[i].kind == "plan" {
			return ps[i].plan
		}
	}
	return ""
}

// The fan curve follows the temperature with every settings write failing.
func TestROFanCurveKeepsFollowing(t *testing.T) {
	m := newHLMiner(t)
	roStore(t)
	c := hlCfg()
	c.Smoothing = 0
	m.setTemps([3]float64{60, 60, 60}, normal, normal, normal)
	m.tick(c, 10*time.Second)
	want := targetFan(c, 67.9) // the hottest board reading
	if p := lastPlanFan(t, m); !strings.Contains(p, fmt.Sprintf(" %d RPM %d RPM", want, want)) {
		t.Fatalf("curve write at 67.9 °C: want %d %%, last plan %q (puts %+v)", want, p, m.putsCopy())
	}
	m.setTemps([3]float64{78, 78, 78.5}, normal, normal, normal)
	m.tick(c, 10*time.Second)
	want2 := targetFan(c, 78.5)
	if want2 <= want {
		t.Fatalf("setup: the curve should ask more at 78.5 °C (%d vs %d)", want2, want)
	}
	if p := lastPlanFan(t, m); !strings.Contains(p, fmt.Sprintf(" %d RPM %d RPM", want2, want2)) {
		t.Fatalf("curve write at 78.5 °C: want %d %%, last plan %q", want2, p)
	}
	if _, tg := fanGuardNow(); tg != want2 {
		t.Fatalf("fan status target %d, want %d", tg, want2)
	}
	if !cfgStoreReadOnly() {
		t.Fatal("the store isn't reported read-only")
	}
}

// The abort tier (100 %) and the escalation to Idle fire with every settings write failing; the overheat Idle's
// hold lives in memory (its file can't be written).
func TestROAbortAndIdleStillFire(t *testing.T) {
	m := newHLMiner(t)
	roStore(t)
	c := hlCfg()
	hot := [3]float64{89, 89, 89.5}
	m.setTemps(hot, normal, normal, normal)
	m.tick(c, 10*time.Second)
	m.tick(c, 10*time.Second)
	if p := lastPlanFan(t, m); !strings.Contains(p, "100 RPM 100 RPM") {
		t.Fatalf("no 100 %% write: %+v", m.putsCopy())
	}
	m.tick(c, 10*time.Second)
	m.adv(100 * time.Second)
	m.tick(c, 10*time.Second)
	if m.count("idle") != 1 || getCharge() != "idle" || !m.isIdle() {
		t.Fatalf("no Idle after the escalation time: puts=%+v charge=%q", m.putsCopy(), getCharge())
	}
	if heatIdleSince.Load() == 0 || !heatIdleHeld() {
		t.Fatal("the overheat Idle isn't held (in memory)")
	}
	if fileExists(heatHoldPath()) {
		t.Fatal("the hold's file was written on a read-only store")
	}
}

// The far-past tier (5 °C over abort, twice) goes to Idle at once, writes failing.
func TestROFarPastIdle(t *testing.T) {
	m := newHLMiner(t)
	roStore(t)
	c := hlCfg()
	m.setTemps([3]float64{93.5, 93.5, 94}, normal, normal, normal)
	m.tick(c, 10*time.Second)
	m.tick(c, 10*time.Second)
	if m.count("idle") != 1 || getCharge() != "idle" {
		t.Fatalf("not Idle: %+v", m.putsCopy())
	}
}

// The throttle steps the clock down and puts it back, its saved clock in memory only.
func TestROThrottleStepsAndRestores(t *testing.T) {
	m := newHLMiner(t)
	roStore(t)
	old := throttleEvery
	throttleEvery = 0
	t.Cleanup(func() { throttleEvery = old; throttleOnDisk = throttleRec{} })
	c := hlCfg()
	c.ThrottleEnable, c.ThrottleC, c.ResumeC = true, 85, 80
	m.setTemps([3]float64{86, 86, 86.5}, normal, normal, normal)
	m.tick(c, 10*time.Second)
	if p := lastPlanFan(t, m); !strings.HasPrefix(p, "600 MHz") {
		t.Fatalf("no clock step: %q (puts %+v)", p, m.putsCopy())
	}
	fanMu.RLock()
	saved := fanSt.SavedFreq
	fanMu.RUnlock()
	if saved != 625 {
		t.Fatalf("saved clock %d, want 625 (in memory)", saved)
	}
	if fileExists(throttlePath()) {
		t.Fatal("throttle.json written on a read-only store")
	}
	m.setTemps([3]float64{70, 70, 70}, normal, normal, normal)
	m.tick(c, 10*time.Second)
	if p := lastPlanFan(t, m); !strings.HasPrefix(p, "625 MHz") {
		t.Fatalf("clock not put back: %q", p)
	}
}

// The same with REAL failing writes (no hook): the settings directory is a regular file, so every write under it
// fails (ENOTDIR). The curve, the abort tier and the Idle still work.
func TestROReal_ConfigDirUnwritable(t *testing.T) {
	m := newHLMiner(t)
	cfgROReset()
	fanMu.RLock()
	keep := fanCfg
	fanMu.RUnlock()
	t.Cleanup(func() { cfgROReset(); roCoolDown(); fanMu.Lock(); fanCfg = keep; fanMu.Unlock() })
	dir := t.TempDir()
	blocker := filepath.Join(dir, "config")
	os.WriteFile(blocker, []byte("not a directory"), 0o644)
	*authFile = filepath.Join(blocker, "auth.json")
	fp := filepath.Join(blocker, "fan.json")
	oldFan := fanPath
	fanPath = &fp
	t.Cleanup(func() { fanPath = oldFan })
	if err := writeAtomic(heatHoldPath(), []byte("{}"), 0o644); err == nil {
		t.Fatal("setup: the write should fail")
	}
	c := hlCfg()
	m.setTemps(normal, normal, normal, normal)
	m.tick(c, 10*time.Second)
	if m.count("plan") != 1 {
		t.Fatalf("no curve write: %+v", m.putsCopy())
	}
	// the owner's fan settings: used, though they can't be saved
	nc := c
	nc.Profile = "curve-60"
	if err := setFanConfig(nc); err == nil || !strings.Contains(err.Error(), "in use until the panel restarts") {
		t.Fatalf("setFanConfig: %v", err)
	}
	fanMu.RLock()
	got := fanCfg.Profile
	fanMu.RUnlock()
	if got != "curve-60" {
		t.Fatalf("the new curve isn't in use: %q", got)
	}
	hot := [3]float64{93.5, 93.5, 94}
	m.setTemps(hot, normal, normal, normal)
	m.tick(nc, 10*time.Second)
	m.tick(nc, 10*time.Second)
	if p := m.putsCopy(); m.count("idle") != 1 || getCharge() != "idle" {
		t.Fatalf("no Idle: %+v", p)
	}
	if v, _ := cfgLastErr.Load().(string); !strings.Contains(v, "not a directory") {
		t.Fatalf("the failure wasn't recorded: %q", v)
	}
}

// POST /api/fan with the store read-only: ok:false with the reason, but the settings are in use.
func TestROFanPostAppliesAndSaysNotSaved(t *testing.T) {
	newHLMiner(t)
	roStore(t)
	fanMu.Lock()
	before := fanCfg
	fanCfg = defaultFanConfig()
	fanMu.Unlock()
	t.Cleanup(func() { fanMu.Lock(); fanCfg = before; fanMu.Unlock() })
	body := `{"enabled":true,"profile":"curve-60","smoothing":1,"poll_s":10,"cooldown_s":45,"abort_c":` +
		fmt.Sprint(abortDefC) + `,"resume_c":` + fmt.Sprint(resumeDefC) + `,"throttle_c":` + fmt.Sprint(throttleDefC) + `,"throttle_step":25}`
	w := httptest.NewRecorder()
	handleFan(w, httptest.NewRequest(http.MethodPost, "/api/fan", strings.NewReader(body)))
	var j map[string]any
	json.Unmarshal(w.Body.Bytes(), &j)
	if j["ok"] != false || !strings.Contains(fmt.Sprint(j["err"]), "read-only") || !strings.Contains(fmt.Sprint(j["err"]), "in use until the panel restarts") {
		t.Fatalf("answer %s", w.Body.String())
	}
	fanMu.RLock()
	got := fanCfg.Profile
	fanMu.RUnlock()
	if got != "curve-60" {
		t.Fatalf("not in use: %q", got)
	}
	// saving a library curve: the same
	w = httptest.NewRecorder()
	handleFan(w, httptest.NewRequest(http.MethodPost, "/api/fan", strings.NewReader(
		`{"action":"profile","label":"Mine","points":[{"temp":50,"fan":40},{"temp":75,"fan":100}],"abort_c":80,"cooldown_s":45,"poll_s":10,"smoothing":1}`)))
	json.Unmarshal(w.Body.Bytes(), &j)
	if j["ok"] != false || !strings.Contains(fmt.Sprint(j["err"]), "read-only") {
		t.Fatalf("library save answer %s", w.Body.String())
	}
}

// The Dashboard's banner, the activity line and the notification: once per kind of trouble.
func TestROBannerAndNotifyOnce(t *testing.T) {
	useModel(t, profSCBox2)
	roStore(t)
	var mu sync.Mutex
	var got []string
	notifyTestHook = func(ev, msg string) { mu.Lock(); got = append(got, ev+": "+msg); mu.Unlock() }
	t.Cleanup(func() { notifyTestHook = nil })
	fanWriteFailing.Store(true)
	for i := 0; i < 3; i++ {
		st := cfgStoreWatch()
		if st == nil || st.Kind != "read_only" {
			t.Fatalf("banner %+v", st)
		}
		for _, w := range []string{"Settings can't be saved", "read-only", "NOT reaching the fans", "86 °C cut-off"} {
			if !strings.Contains(st.Msg, w) {
				t.Fatalf("banner %q lacks %q", st.Msg, w)
			}
		}
		if !strings.Contains(st.Fix, "Reboot the miner") || !strings.Contains(st.Fix, "repairs what is safe") {
			t.Fatalf("fix %q", st.Fix)
		}
	}
	time.Sleep(50 * time.Millisecond) // (the notification is sent on its own goroutine)
	mu.Lock()
	n := 0
	for _, g := range got {
		if strings.HasPrefix(g, evStorage+": ") {
			n++
		}
	}
	mu.Unlock()
	if n != 1 {
		t.Fatalf("%d storage notifications, want 1: %v", n, got)
	}
	// in the dashboard API (also when the miner doesn't answer)
	b, _ := json.Marshal(dashboard{CfgStore: cfgStoreView()})
	if !bytes.Contains(b, []byte(`"cfg_store":{"kind":"read_only"`)) {
		t.Fatalf("dashboard JSON %s", b)
	}
	// with the store writable again: nothing to say
	cfgROProbe = func(string) bool { return false }
	cfgROReset()
	if st := cfgStoreView(); st != nil {
		t.Fatalf("banner on a writable store: %+v", st)
	}
}

// The start-up check refused a repair: its reason on the Dashboard (the store still writable), with the backup
// advice; with the store read-only as well, both.
func TestRORepairMarkerBanner(t *testing.T) {
	useModel(t, profSCBox2)
	oldTmp, oldProbe := cfgTmp, cfgROProbe
	cfgTmp = filepath.Join(t.TempDir(), "x-")
	cfgROProbe = func(string) bool { return false }
	cfgROReset()
	t.Cleanup(func() { cfgTmp, cfgROProbe = oldTmp, oldProbe; cfgROReset() })
	why := "The settings storage has damage the start-up check won't repair by itself: /SCBOX2/FAN.JSO and /SCBOX2/NET.JSO share cluster 9 (cross-linked)"
	os.WriteFile(cfgRepairMarker(), []byte(why+"\n"), 0o644)
	st := cfgStoreView()
	if st == nil || st.Kind != "damaged" || !strings.Contains(st.Msg, "cross-linked") || !strings.Contains(st.Fix, "Export your settings") {
		t.Fatalf("banner %+v", st)
	}
	cfgROProbe = func(string) bool { return true }
	cfgROReset()
	st = cfgStoreView()
	if st == nil || st.Kind != "read_only" || !strings.Contains(st.Msg, "cross-linked") || !strings.Contains(st.Fix, "SD card") {
		t.Fatalf("banner %+v", st)
	}
	// the boot script's own note (not mounted / not writable)
	os.Remove(cfgRepairMarker())
	cfgROProbe = func(string) bool { return false }
	cfgROReset()
	os.WriteFile(cfgBrokenMarker(), []byte("scbox2: /mnt/config is not mounted (S38initdir failed?)\n"), 0o644)
	st = cfgStoreView()
	if st == nil || st.Kind != "broken" || !strings.HasPrefix(st.Msg, "Settings can't be saved: /mnt/config is not mounted") {
		t.Fatalf("banner %+v", st)
	}
	// the start-up check's repair goes to the activity log
	os.WriteFile(cfgFatfixLog(), []byte("fatfix: repair: free 4 lost cluster(s), FSInfo free count 6000 next free 40, clear the dirty flag; the backup boot sector matches (4 write(s))\nfatfix: repaired in 4 write(s), read back and verified\n"), 0o644)
	ch := activity.sub()
	defer activity.unsub(ch)
	cfgStoreBoot()
	select {
	case l := <-ch:
		if !strings.Contains(l, "the start-up check repaired it (free 4 lost cluster(s)") {
			t.Fatalf("activity %q", l)
		}
	case <-time.After(time.Second):
		t.Fatal("no activity line for the repair")
	}
}

// A POST whose setting couldn't be saved answers ok:false with the reason (the page shows it).
func TestROPostsSayNotSaved(t *testing.T) {
	b10TempAuth(t)
	roStore(t)
	post := func(h http.HandlerFunc, body string) map[string]any {
		w := httptest.NewRecorder()
		h(w, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)))
		var j map[string]any
		json.Unmarshal(w.Body.Bytes(), &j)
		return j
	}
	// best-share reset
	bestBookMu.Lock()
	sharesRead = true
	bestBookMu.Unlock()
	if j := post(handleShares, `{"action":"reset"}`); j["ok"] != false || !strings.Contains(fmt.Sprint(j["err"]), "read-only") {
		t.Fatalf("shares reset: %v", j)
	}
	// recovery settings and the unexpected-restart dismiss
	if j := post(handleRecovery, `{"keep_log":true}`); j["ok"] != false || !strings.Contains(fmt.Sprint(j["err"]), "read-only") {
		t.Fatalf("recovery: %v", j)
	}
	recMu.Lock()
	recUnexp = &unexpectedRestart{}
	recMu.Unlock()
	t.Cleanup(func() { recMu.Lock(); recUnexp = nil; recMu.Unlock() })
	if j := post(handleRecovery, `{"dismiss":true}`); j["ok"] != false || !strings.Contains(fmt.Sprint(j["err"]), "dismissed until the panel restarts") {
		t.Fatalf("dismiss: %v", j)
	}
	// the pool guard's resume, when its hold can't be removed (a non-empty directory stands in for a file on a
	// read-only store): refused, and the miner stays held
	os.MkdirAll(filepath.Join(poolHoldPath(), "x"), 0o755)
	if j := post(handlePoolGuard, `{"resume":true}`); j["ok"] != false || !strings.Contains(fmt.Sprint(j["err"]), "not resumed") {
		t.Fatalf("pool guard resume: %v", j)
	}
	if _, held := poolHold(); !held {
		t.Fatal("the hold was dropped")
	}
	os.RemoveAll(poolHoldPath())
	// the Box II power calibration (the case seen on the miner)
	p := profSCBox2
	p.Feat.PowerEstimate = true
	useModel(t, p)
	box2PowMu.Lock()
	box2PowRead, box2PowCfg = true, box2PowerConfig{Cals: []box2PowerCal{{MHz: 690, V: 42, Watts: 400}}}
	box2PowMu.Unlock()
	t.Cleanup(func() { box2PowMu.Lock(); box2PowRead, box2PowCfg = false, box2PowerConfig{}; box2PowMu.Unlock() })
	if j := post(handlePowerBox2, `{"clear":true}`); j["ok"] != false || !strings.Contains(fmt.Sprint(j["err"]), "read-only") {
		t.Fatalf("power clear: %v", j)
	}
}

// The page shows the failure of every POST whose answer it checks: no `if(j.ok){...}` without an else on a save.
func TestROPageShowsSaveFailures(t *testing.T) {
	for _, want := range []string{
		"jpost('/api/time',{sync:true});if(j.ok){$('s_ntpmsg').textContent='syncing… (up to a minute)';setTimeout(loadTime,30000);}else toast(",
		"jpost('/api/power',{clear:true});if(j.ok){toast('Calibration cleared');renderPower(j);if(typeof refresh==='function')refresh();}else toast(",
		"jpost('/api/tuner',{action:'stop'});if(j.ok)$('tunernote').textContent=j.note||'stopping';else toast(",
		"const {j}=await jpost('/api/poolguard',{resume:true});if(!j.ok)toast(",
		"{dismiss:true}).then(r=>{if(!r.j.ok)toast(",
		"function cfgStoreHTML(d){",
		"cfgStoreHTML(d)+overTempHTML(d)+chainHTML(d)+unexpHTML(d)+liveTiles(d)",
	} {
		if !strings.Contains(indexHTML, want) {
			t.Errorf("page lacks %q", want)
		}
	}
}
