package main

// The fan check, fitted to this miner's fans.
//
// Goldshell's watchdog (xminerd) reads each fan's "maxrpm" from /root/product.json (2200 on an SC Lite) and
// flags a fan whose RPM isn't above maxrpm/125 × fan % (whole-number division): 80 % of a straight line from
// 0 to maxrpm. At 100 % that's 1700 RPM, so quieter replacement fans (1600 RPM at full speed) are all flagged
// and the watchdog keeps killing mining, while a fan that stops is still caught by it (below 30 % fan it only
// asks for some RPM at all). Taken apart from xminerd 2.2.0; the figures match both miners we saw.
//
// "Fit to my fans" doesn't touch Goldshell's programs: it gives them their own figures. The watchdog's sweep
// at boot (each fan's RPM at 10 … 100 %) shows what each fan really does; the fitted maxrpm is the highest
// that keeps that whole curve 15 % clear of the line, never above stock. A fan that slows down or stops is
// still flagged, by Goldshell's own watchdog. The adjusted copy lives on the config partition and the boot
// script binds it over /root/product.json before Goldshell's programs start (files/S48sclite in the kit), so
// a change takes effect at the next reboot: Goldshell's programs read the file once, as they start, and
// restarting the watchdog on its own could leave the mining program without (or with two) watchdogs.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	productPath = "/root/product.json"
	reSweep     = regexp.MustCompile(`\[Xminerd\]: fan(\d) \[([-0-9, ]+)\]`)
)

const fanFitMin = 400

// fanStockMax: the fan "maxrpm" Goldshell's watchdog uses and the most the fit ever sets — now taken from
// the model profile (2200 on an SC Lite, 4500 on an SC Box II) instead of a hardcoded 2200. The fit range
// and values adapt to it. With no model loaded the default profile is SC Lite, so this stays 2200 and SC
// Lite behaviour is unchanged.
func fanStockMax() int { return model.FanStockMaxRPM }

func fanOverlayPath() string { return filepath.Join(filepath.Dir(*authFile), "product.json") }
func fanStockCopy() string   { return filepath.Join(filepath.Dir(*authFile), "product.stock.json") }

// fanOverlayMounted: the adjusted copy is bound over /root/product.json now.
func fanOverlayMounted() bool {
	b, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return false
	}
	return strings.Contains(string(b), " "+productPath+" ")
}

// readFanSweep: the watchdog's latest boot sweep, fan → RPM at 0, 10 … 100 % (index = % / 10).
var readFanSweep = readFanSweep0

func readFanSweep0() map[int][]int {
	t, err := tailFile(watchdogPath(), 512<<10)
	if err != nil {
		return nil
	}
	return parseFanSweep(t)
}

func parseFanSweep(t string) map[int][]int {
	out := map[int][]int{}
	for _, m := range reSweep.FindAllStringSubmatch(t, -1) {
		id, _ := strconv.Atoi(m[1])
		var rpm []int
		for _, f := range strings.Split(m[2], ",") {
			v, err := strconv.Atoi(strings.TrimSpace(f))
			if err != nil {
				rpm = nil
				break
			}
			rpm = append(rpm, v)
		}
		if len(rpm) == 11 {
			out[id] = rpm // the last sweep wins
		}
	}
	return out
}

// fitMaxRPM: the highest maxrpm (≤ stock) whose line, maxrpm/125 × %, stays 15 % under the fan's own RPM
// from 30 % up (below 30 % the watchdog only asks that the fan turns). 0: not enough readings.
func fitMaxRPM(rpm []int) int {
	best, n := -1.0, 0
	for i := 3; i < len(rpm) && i <= 10; i++ {
		if rpm[i] <= 0 {
			return 0 // a fan that doesn't turn at 30 % or more isn't fitted around: it's a fault
		}
		v := float64(rpm[i]) * 125 / float64(i*10)
		if best < 0 || v < best {
			best = v
		}
		n++
	}
	if n < 5 {
		return 0
	}
	fit := int(best * 0.85)
	return max(fanFitMin, min(fanStockMax(), fit/125*125)) // whole steps of 125: the watchdog divides by 125
}

// fanLimitAt: the RPM under which the watchdog flags a fan with this maxrpm at pct.
func fanLimitAt(maxrpm, pct int) int {
	if pct < 30 {
		return 10
	}
	return maxrpm / 125 * pct
}

func readProductFans(path string) (map[string]any, []map[string]any, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, nil, err
	}
	arr, _ := doc["fans"].([]any)
	var fans []map[string]any
	for _, f := range arr {
		if m, ok := f.(map[string]any); ok {
			fans = append(fans, m)
		}
	}
	if len(fans) == 0 {
		return nil, nil, fmt.Errorf("no fans in %s", path)
	}
	return doc, fans, nil
}

// fanCheckView: GET /api/fancheck.
func fanCheckView() map[string]any {
	_, pend := os.Stat(fanOverlayPath())
	v := map[string]any{"ok": true, "fitted": fanOverlayMounted(), "saved": pend == nil, "stock_max": fanStockMax(), "fit_enabled": model.Feat.FanFit, "fans": model.Fans, "lock": fanLockView()}
	if _, fans, err := readProductFans(productPath); err == nil {
		var cur []int
		for _, f := range fans {
			n, _ := f["maxrpm"].(float64)
			cur = append(cur, int(n))
		}
		v["maxrpm"] = cur
	}
	sw := readFanSweep()
	var curves [][]int
	var suggest []int
	for i := 0; i < 8; i++ {
		c, ok := sw[i]
		if !ok {
			break
		}
		curves = append(curves, c)
		suggest = append(suggest, fitMaxRPM(c))
	}
	v["sweep"], v["suggest"] = curves, suggest
	if w, ok := readWatchdog(time.Now()); ok {
		v["watchdog"] = w
	}
	return v
}

// fanCheckFit writes the adjusted copy with these maxrpm per fan and puts it in place now.
func fanCheckFit(maxrpm []int) error {
	if featOff(model.Feat.FanFit) { // gated off on models not yet measured (e.g. SC Box II): never write an overlay
		return fmt.Errorf("fit to my fans is not available on this model (%s)", model.Name)
	}
	src := productPath
	if fanOverlayMounted() {
		src = fanStockCopy()
	} else if b, err := os.ReadFile(productPath); err == nil {
		if _, _, err := readProductFans(fanStockCopy()); err != nil { // none yet, or one cut short
			writeAtomic(fanStockCopy(), b, 0o644)
		}
	}
	raw, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("%s fan settings can't be read: %v", OemOf(), err)
	}
	_, fans, err := readProductFansBytes(raw)
	if err != nil {
		return fmt.Errorf("%s fan settings can't be read: %v", OemOf(), err)
	}
	if len(maxrpm) != len(fans) {
		return fmt.Errorf("%d fan figures for %d fans", len(maxrpm), len(fans))
	}
	sw := readFanSweep()
	for i, r := range maxrpm {
		// never more lenient than what this miner's own fans measured (the fit is the most lenient allowed)
		if r < fanStockMax() { // anything more lenient than Goldshell's figure needs this fan's own measurements
			c, ok := sw[i]
			if !ok {
				return fmt.Errorf("fan %d: no start-up fan test in the log to fit to (Re-run %s fan test, then fit)", i, oemOf())
			}
			f := fitMaxRPM(c)
			if f <= 0 {
				return fmt.Errorf("fan %d didn't turn at 30 %% or more in the fan test: that's a fault, not something to fit around", i)
			}
			if r < f {
				return fmt.Errorf("fan %d: %d is under what its measured speeds support (%d): the check would let a failing fan pass", i, r, f)
			}
		}
		if r < fanFitMin || r > fanStockMax() {
			return fmt.Errorf("fan %d: %d RPM is outside %d–%d", i, r, fanFitMin, fanStockMax())
		}
	}
	// Replace only the maxrpm digits in the stock file's own bytes; never re-encode the JSON. Re-encoding
	// (json.Marshal of the parsed document) rewrites every number, turning "tempcutoff":92.0 into 92 — and
	// Goldshell's fan control reads tempcutoff with a real-valued JSON call that returns 0 for an integer,
	// so a 0 there becomes its 95 °C default: the fit would quietly RAISE the thermal cut-off above stock.
	// Keeping every other byte of Goldshell's file verbatim leaves tempcutoff/temp_target exactly as they were.
	out, err := replaceMaxRPM(raw, maxrpm)
	if err != nil {
		return err
	}
	if err := sameExceptMaxRPM(raw, out); err != nil { // nothing but the maxrpm values changed
		return err
	}
	tmp := fanOverlayPath() + ".new"
	if err := writeFileSync(tmp, out, 0o644); err != nil { // on disk before the rename: the boot script binds it
		return err
	}
	if _, _, err := readProductFansBytes(out); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, fanOverlayPath()); err != nil {
		return err
	}
	activity.emit(fmt.Sprintf("🌀 fan check fitted to this miner's fans: max RPM %v (stock %d), from the next reboot", maxrpm, fanStockMax()))
	return nil
}

func readProductFansBytes(b []byte) (map[string]any, []map[string]any, error) {
	f := filepath.Join(os.TempDir(), brand+"-product-check.json")
	defer os.Remove(f)
	if err := os.WriteFile(f, b, 0o644); err != nil {
		return nil, nil, err
	}
	return readProductFans(f)
}

// walkMaxRPM calls fn for the number after each "maxrpm" key in src (in document order, i.e. fan order),
// passing the key's ordinal; fn returns the replacement text for that number. Every other byte of src is
// copied through unchanged. It returns the rewritten bytes and the number of maxrpm keys found.
func walkMaxRPM(src []byte, fn func(i int) string) ([]byte, int, error) {
	key := []byte(`"maxrpm"`)
	ws := func(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }
	num := func(c byte) bool {
		return c == '-' || c == '+' || c == '.' || c == 'e' || c == 'E' || (c >= '0' && c <= '9')
	}
	var out []byte
	pos, n := 0, 0
	for {
		rel := indexOf(src[pos:], key)
		if rel < 0 {
			break
		}
		j := pos + rel + len(key)
		for j < len(src) && ws(src[j]) {
			j++
		}
		if j >= len(src) || src[j] != ':' { // "maxrpm" not used as a key here: leave it be
			out = append(out, src[pos:j]...)
			pos = j
			continue
		}
		j++ // past ':'
		for j < len(src) && ws(src[j]) {
			j++
		}
		k := j
		for k < len(src) && num(src[k]) {
			k++
		}
		if k == j {
			return nil, 0, fmt.Errorf(`"maxrpm" with no number in %s fan settings`, oemOf())
		}
		out = append(out, src[pos:j]...)
		out = append(out, fn(n)...)
		pos, n = k, n+1
	}
	out = append(out, src[pos:]...)
	return out, n, nil
}

// replaceMaxRPM rewrites the number after each "maxrpm" key, in fan order, to the given values, and leaves
// every other byte untouched. It never parses-and-re-encodes the JSON: that would rewrite Goldshell's other
// numbers too (notably turn "tempcutoff":92.0 into 92, which the miner's fan control reads as a 0 cut-off and
// replaces with its 95 °C default), quietly raising the thermal cut-off above stock. The count of maxrpm
// keys must equal len(maxrpm), so an unexpected product.json is refused rather than half-written.
func replaceMaxRPM(src []byte, maxrpm []int) ([]byte, error) {
	out, n, err := walkMaxRPM(src, func(i int) string {
		if i < len(maxrpm) {
			return strconv.Itoa(maxrpm[i])
		}
		return "0" // extra key: count check below rejects it
	})
	if err != nil {
		return nil, err
	}
	if n != len(maxrpm) {
		return nil, fmt.Errorf("found %d maxrpm fields in %s fan settings, expected %d", n, oemOf(), len(maxrpm))
	}
	return out, nil
}

func indexOf(b, sub []byte) int {
	for i := 0; i+len(sub) <= len(b); i++ {
		if string(b[i:i+len(sub)]) == string(sub) {
			return i
		}
	}
	return -1
}

// sameExceptMaxRPM confirms a and b differ only in the digits of maxrpm values: everything the miner's fan
// control reads (tempcutoff, temp_target, fan names, ...) is byte-for-byte unchanged. Both sides have their
// maxrpm numbers blanked to "0" and must then match exactly.
func sameExceptMaxRPM(a, b []byte) error {
	za, _, err := walkMaxRPM(a, func(int) string { return "0" })
	if err != nil {
		return err
	}
	zb, _, err := walkMaxRPM(b, func(int) string { return "0" })
	if err != nil {
		return err
	}
	if string(za) != string(zb) {
		return fmt.Errorf("the fitted fan settings changed more than the maxrpm values")
	}
	return nil
}

// fanCheckStock: back to Goldshell's own figures (from the next reboot).
func fanCheckStock() error {
	if err := os.Remove(fanOverlayPath()); err != nil && !os.IsNotExist(err) {
		return err
	}
	activity.emit("🌀 fan check back to " + oemOf() + " figures, from the next reboot")
	return nil
}

// GET/POST /api/fancheck: the fan check (fit / stock) and the fan lock (unlock, on/off).
func handleFanCheck(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var in struct {
			Fit     []int `json:"fit"`
			Stock   bool  `json:"stock"`
			Unlock  bool  `json:"unlock"`
			Accept  bool  `json:"accept"`
			LockOff *bool `json:"lock_off"`
			Rerun   bool  `json:"rerun"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeJSON(w, map[string]any{"ok": false, "err": "bad request"})
			return
		}
		var err error
		note := ""
		switch {
		case in.Rerun:
			err, note = fanTestRerun(), "re-running "+oemOf()+" fan test: the miner goes to Idle, the watchdog restarts and tests the fans (about 2 minutes), then the miner goes back to its setting"
		case in.Unlock:
			err, note = fanUnlock(in.Accept, time.Now()), "unlocked: apply a preset or "+stockWord()+" to start mining again"
		case in.Stock:
			err, note = fanCheckStock(), "back to "+oemOf()+" figures from the next reboot"
		case len(in.Fit) > 0:
			if !featGate(w, model.Feat.FanFit, "fit to my fans") { // refuses cleanly on models without fan-fit (SC Box II)
				return
			}
			err, note = fanCheckFit(in.Fit), "fitted figures saved: they take effect at the next reboot"
		case in.LockOff != nil:
			lc := loadFanLockConfig()
			lc.Off = *in.LockOff
			err = saveFanLockConfig(lc)
			note = "fan lock on"
			if *in.LockOff {
				note = "fan lock off: fan failures are left to " + oemOf() + " watchdog"
			}
		default:
			err = fmt.Errorf("nothing to do")
		}
		if err != nil {
			writeJSON(w, map[string]any{"ok": false, "err": err.Error()})
			return
		}
		v := fanCheckView()
		v["note"] = note
		writeJSON(w, v)
		return
	}
	writeJSON(w, fanCheckView())
}

// fanTestRerun restarts Goldshell's watchdog (xminerd; init starts it again within seconds). It forgets the
// fan speeds it learned and runs its start-up fan test again. Why: besides its fixed rule, the watchdog
// records each fan's RPM the first time it sees that fan at a given duty, and from then on flags the fan
// under 60 % of that (traced in xminerd at 0xa18e). A fan whose level said one thing while it really turned
// at another (a pair partner driving the shared signal, a fan still spinning down) leaves a wrong record,
// and the watchdog keeps stopping mining for a fan that works. Nothing is skipped: the full check runs again.
var fanRerunCmd = "killall xminerd"

func fanTestRerun() error {
	controlMu.Lock() // the charge check and the start in one go: a tuning run can't start in between
	defer controlMu.Unlock()
	if c := getCharge(); c == "tuner" || c == "ceiling" {
		return fmt.Errorf("the %s is running: stop it first", map[string]string{"tuner": "tuner", "ceiling": "limit probe"}[c])
	}
	if sctRunning() {
		return fmt.Errorf("the strong-chip test is running (Controls → Chip trims): stop it first")
	}
	if overTempView() != nil {
		// a restarted watchdog forgets the over-temperature shutdown (it clears that state when it starts)
		return fmt.Errorf("the boards were shut down for heat: reboot the miner instead (that also runs the fan test)")
	}
	if !intminerRunning() {
		// The re-run first puts the miner on Idle, which needs Goldshell's backend, which needs the mining
		// program. While the miner is restarting or has just booted, that isn't up — and a reboot runs
		// Goldshell's fan test anyway, so there's nothing to do. (At boot, productCheckAtStart waits for the
		// mining program before it re-runs; here we refuse rather than fail half-way with a scary warning.)
		return fmt.Errorf("the miner is restarting (the mining program isn't running): a reboot runs %s fan test anyway — try again once it's mining", oemOf())
	}
	if !fanRerunMu.TryLock() {
		return fmt.Errorf("the fan test is already being re-run")
	}
	fanRerunActive.Store(true) // a tuning run can't start meanwhile (fanTestRunning)
	go func() {
		defer fanRerunMu.Unlock()
		defer fanRerunActive.Store(false)
		if err := fanTestRerunSteps(); err != nil {
			activity.emit("⚠ re-running " + oemOf() + " fan test: " + err.Error())
			notify(evFan, "re-running "+oemOf()+" fan test: "+err.Error())
		}
	}()
	return nil
}

var (
	fanRerunMu     sync.Mutex
	fanRerunActive atomic.Bool
)

// fanTestRerunSteps, in this order (traced in xminerd): the miner goes to Idle and the mining program stops
// (a program started by the old watchdog isn't the new one's child: it would never be restarted after an
// exit), the watchdog restarts and runs its start-up fan test with no boards hashing, then the setting from
// before is put back (Idle stays Idle; the fan lock and the pool guard's hold stay).
var fanTestRerunSteps = func() error {
	raw, _ := stockGet("/mcb/setting")
	restore, _ := settingRestorer(raw)
	if raw == nil {
		restore = nil
	}
	wasIdle := minerIsIdleNow()
	charge0 := getCharge()
	if !wasIdle && restore != nil {
		// on disk first: a panel that stops during the test (a crash, an update, a power cut) puts it back at start
		if b, err := json.Marshal(fanRerunRec{At: time.Now().Unix(), Setting: raw, Charge: charge0}); err == nil && writeAtomic(fanRerunPath(), b, 0o644) == nil {
			defer os.Remove(fanRerunPath())
		}
	}
	if !wasIdle {
		if err := applyIdle(); err != nil {
			return fmt.Errorf("couldn't put the miner on Idle first: %v", err)
		}
	}
	for i := 0; i < 30 && intminerRunning(); i++ {
		time.Sleep(time.Second)
	}
	fanTestAt.Store(monoSec())
	cmd := exec.Command("sh", "-c", fanRerunCmd)
	cmd.Env = append(os.Environ(), "PATH=/usr/sbin:/usr/bin:/sbin:/bin:"+os.Getenv("PATH"))
	if out, err := cmd.CombinedOutput(); err != nil {
		if !wasIdle {
			setCharge("idle") // it stays there: say so (the banner, the schedule)
		}
		return fmt.Errorf("couldn't restart the watchdog (%v %s): the miner stays on Idle; apply "+stockWord()+" or a preset, or reboot it", err, strings.TrimSpace(string(out)))
	}
	up := false
	for i := 0; i < 20 && !up; i++ {
		time.Sleep(time.Second)
		up = procRunning("xminerd")
	}
	if !up {
		if !wasIdle {
			setCharge("idle")
		}
		return fmt.Errorf("%s watchdog didn't start again within 20 s: reboot the miner (Settings → Reboot miner) so the fan check runs", OemOf())
	}
	activity.emit("🌀 " + OemOf() + " fan check restarted: it forgets the fan speeds it had recorded and runs its start-up fan test (about 2 minutes, boards idle)")
	fanSweepUntil.Store(monoSec() + 90) // nothing wakes the boards during the sweep
	time.Sleep(90 * time.Second)        // its start-up test: 10 % … 100 %
	fanTestAt.Store(monoSec())
	if wasIdle || miningHoldErr() != nil || restore == nil {
		return nil
	}
	// Idle, a preset, Stock or Controls chosen meanwhile (by hand or the schedule) stays
	changed := getCharge() != charge0
	if raw, err := stockGet("/mcb/setting"); err == nil && !changed {
		var cur map[string]any
		changed = json.Unmarshal(raw, &cur) == nil && !minerIdle(cur)
	}
	if changed {
		activity.emit("🌀 fan test done: the setting chosen meanwhile stays")
		return nil
	}
	if err := restore(); err != nil {
		return fmt.Errorf("the fan test ran, but the setting from before couldn't be put back (%v): apply "+stockWord()+" or a preset", err)
	}
	activity.emit("🌀 fan test done: the miner is back on its setting")
	return nil
}

var fanTestAt atomic.Int64 // monoSec()

// monoSec: seconds on this process's monotonic clock (from 1). For short in-memory windows: the wall clock
// jumps ~56 years when NTP (or the browser) first sets it after a boot (no battery clock: 1970 until then).
var monoStart = time.Now()

func monoSec() int64 { return int64(time.Since(monoStart)/time.Second) + 1 }

// fanSweepUntil (monoSec()): until then Goldshell's fan test is sweeping the fans from 10 % (boards must stay idle).
var fanSweepUntil atomic.Int64

func fanRerunPath() string { return filepath.Join(filepath.Dir(*authFile), "fan-rerun.json") }

type fanRerunRec struct {
	At      int64           `json:"at"`
	Setting json.RawMessage `json:"setting"` // /mcb/setting before the run's Idle
	Charge  string          `json:"charge"`
}

var fanRerunRecoverWait = 10 * time.Minute

// fanRerunRecover (at start): a fan test re-run that never finished left the miner on its Idle. Once
// Goldshell's fan test is over, the setting from before goes back, as the run would have done, unless something
// was chosen since (the schedule, the owner), the fan lock or the pool guard holds it, or it isn't on Idle.
func fanRerunRecover() {
	b, err := os.ReadFile(fanRerunPath())
	if err != nil {
		return
	}
	defer os.Remove(fanRerunPath())
	var r fanRerunRec
	if json.Unmarshal(b, &r) != nil || len(r.Setting) == 0 || !json.Valid(r.Setting) {
		activity.emit("🌀 a fan test re-run was cut short by a restart: the miner may still be on its Idle. Apply " + stockWord() + " or a preset")
		return
	}
	fanTestAt.Store(monoSec()) // a tuning run waits for the fan test meanwhile
	t0 := time.Now()
	for {
		a := procAgeSec("xminerd")
		_, apiErr := stockGet("/mcb/setting")
		if time.Since(t0) > fanRerunRecoverWait {
			break
		}
		if time.Now().Unix()-r.At >= 120 && (a < 0 || a >= 90) && apiErr == nil {
			break
		}
		fanTestAt.Store(monoSec())
		time.Sleep(5 * time.Second)
	}
	controlMu.Lock()
	defer controlMu.Unlock()
	switch {
	case getCharge() != "" || miningHoldErr() != nil:
		activity.emit("🌀 a fan test re-run was cut short by a restart; the setting chosen since stays")
		return
	case !minerOnIdlePlan():
		return // not on Idle (woken some other way): nothing to undo
	}
	restore, what := settingRestorer(r.Setting)
	if restore == nil {
		activity.emit("🌀 a fan test re-run was cut short by a restart and left the miner on Idle: apply " + stockWord() + " or a preset")
		return
	}
	if err := restore(); err != nil {
		msg := "a fan test re-run was cut short by a restart and left the miner on Idle; putting " + what + " back failed (" + err.Error() + "): apply " + stockWord() + " or a preset"
		activity.emit("🌀 " + msg)
		notifyAlways(evFan, msg) // left on Idle: not held back by an earlier fan alert
		return
	}
	if r.Charge != "tuner" && r.Charge != "ceiling" && r.Charge != "idle" {
		setCharge(r.Charge)
	}
	activity.emit("🌀 a fan test re-run was cut short by a restart: its Idle is over, the miner is back on " + what)
}

// fanTestRunning: Goldshell's start-up fan test is (probably) under way: within 2 minutes of a re-run, or
// while its watchdog has run for less than 90 s (it tests the fans when it starts).
var fanTestRunning = func() bool {
	if fanRerunActive.Load() {
		return true
	}
	if t := fanTestAt.Load(); t > 0 && monoSec()-t < 120 {
		return true
	}
	if a := procAgeSec("xminerd"); a >= 0 && a < 90 {
		return true
	}
	return false
}

// productCheckAtStart: the fitted copy bound over /root/product.json must parse, or Goldshell's fan control
// and watchdog exit at every start (no fan control, no over-temperature shutdown). A broken one is unbound
// and set aside: they read Goldshell's own figures again.
func productCheckAtStart() {
	if !fanOverlayMounted() {
		return
	}
	err := productOverlayOK(productPath)
	if err == nil {
		return
	}
	cmd := exec.Command("sh", "-c", "umount /root/product.json")
	cmd.Env = append(os.Environ(), "PATH=/usr/sbin:/usr/bin:/sbin:/bin:"+os.Getenv("PATH"))
	cmd.Run()
	os.Rename(fanOverlayPath(), fanOverlayPath()+".bad")
	activity.emit("⚠ the fan check's fitted figures can't be used (" + err.Error() + "): back to " + oemOf() + " figures (Controls & fan → Fan check to fit them again)")
	if _, _, perr := readProductFans(productPath); perr == nil {
		// It parsed, so Goldshell's watchdog started with its figures and needs restarting to pick up the
		// stock ones. The re-run has to put the miner on Idle first, which needs the mining program — not up
		// yet at boot. Wait for it (the overlay is already unbound, so if it never comes up the next reboot is
		// clean) rather than firing the re-run now and failing with a warning.
		go func() {
			for i := 0; i < 180 && !intminerRunning(); i++ {
				time.Sleep(time.Second)
			}
			if intminerRunning() {
				fanTestRerun()
			}
		}()
	}
}

// productOverlayOK: the fitted copy parses, every fan's maxrpm is one the fit could have written (a figure
// under fanFitMin makes the watchdog's limit 0 RPM at every duty: no fan would ever be flagged), and the
// thermal fields the miner's fan control reads are intact. An overlay written by an older panel re-encoded
// the JSON and lost tempcutoff/temp_target (they became 0, which the fan control turns into its 95 °C
// default — a cut-off above stock); such an overlay is refused here so the miner falls back to Goldshell's
// own file. When the stock copy is on hand, the fields must equal it exactly; otherwise they must at least
// be present and above zero.
func productOverlayOK(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	_, fans, err := readProductFansBytes(raw)
	if err != nil {
		return err
	}
	for i, f := range fans {
		n, ok := f["maxrpm"].(float64)
		if !ok || n != float64(int(n)) || int(n) < fanFitMin || int(n) > fanStockMax() {
			return fmt.Errorf("fan %d: maxrpm %v is outside %d–%d", i, f["maxrpm"], fanFitMin, fanStockMax())
		}
	}
	// tempcutoff and temp_target must be written as reals (with a decimal point). Goldshell's fan control
	// reads them with a real-valued JSON call: a whole-number token is a JSON integer, read as 0, and turned
	// into its 95 °C default — a cut-off above stock. An older panel re-encoded the file and dropped the ".0".
	// Note: a Go JSON parse can't tell 92 from 92.0 (both decode to float64), so this is a byte-level check.
	for _, key := range []string{"tempcutoff", "temp_target"} {
		tok, ok := jsonNumberToken(raw, key)
		if !ok {
			return fmt.Errorf("%s is missing (%s fan control would use its 95 °C default)", key, oemPoss())
		}
		if !strings.Contains(tok, ".") {
			return fmt.Errorf("%s is %q, a whole number: %s fan control would read it as a 0 cut-off and use its 95 °C default", key, tok, oemPoss())
		}
	}
	// When Goldshell's own copy is on hand, the overlay must match it byte-for-byte apart from the maxrpm
	// values: nothing else the fan control reads may drift.
	if sraw, serr := os.ReadFile(fanStockCopy()); serr == nil {
		if err := sameExceptMaxRPM(sraw, raw); err != nil {
			return err
		}
	}
	return nil
}

// jsonNumberToken returns the number token written after "key": in raw (the exact characters, so "92.0"
// keeps its decimal point), and whether the key was found as a number.
func jsonNumberToken(raw []byte, key string) (string, bool) {
	k := []byte(`"` + key + `"`)
	ws := func(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }
	num := func(c byte) bool {
		return c == '-' || c == '+' || c == '.' || c == 'e' || c == 'E' || (c >= '0' && c <= '9')
	}
	rel := indexOf(raw, k)
	if rel < 0 {
		return "", false
	}
	j := rel + len(k)
	for j < len(raw) && ws(raw[j]) {
		j++
	}
	if j >= len(raw) || raw[j] != ':' {
		return "", false
	}
	j++
	for j < len(raw) && ws(raw[j]) {
		j++
	}
	s := j
	for j < len(raw) && num(raw[j]) {
		j++
	}
	if j == s {
		return "", false
	}
	return string(raw[s:j]), true
}
