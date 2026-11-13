package main

import (
	"bytes"
	"encoding/json"
	"math"
	"math/rand"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// sctFake plays one board over a virtual clock: per chip, nonces at its clock (the trims file) and errors at
// a rate the test sets (per hour, accumulated exactly so the counts are deterministic).
type sctFake struct {
	now      time.Time
	chain    int
	chips    int
	base10   int
	nonces   []float64
	errAcc   []float64
	errRate  func(chip int, trim float64, trims map[int]float64) float64 // errors per hour
	nonceMul func(trims map[int]float64) float64                         // the board's nonce rate factor
	temp     func(trims map[int]float64) float64
	stale    bool
	idle     bool
	noClock  bool // the hook doesn't take the trims (clocks stay at the board clock)
	blocked  string
	hookOn   bool
	rng      *rand.Rand // set: errors are Poisson draws instead of exact accumulation
	notes    []string
}

func (f *sctFake) trims() map[int]float64 {
	m := map[int]float64{}
	for _, t := range readChipTrims() {
		if t.Chain == f.chain {
			m[t.Chip] = t.MHz
		}
	}
	return m
}

func (f *sctFake) advance(d time.Duration) {
	f.now = f.now.Add(d)
	tr := f.trims()
	mul := 1.0
	if f.nonceMul != nil {
		mul = f.nonceMul(tr)
	}
	h := d.Hours()
	base := float64(f.base10) / 10
	for i := 1; i <= f.chips; i++ {
		clk := base + tr[i]
		f.nonces[i] += (900 - 10*float64(i)) * h * clk / base * mul // ~300 per 20 min: the real miner's order
		if f.errRate != nil {
			lam := f.errRate(i, tr[i], tr) * h
			if f.rng != nil {
				k, p := 0, math.Exp(-lam) // Knuth: lam per tick is tiny
				for u := f.rng.Float64(); u > p; u *= f.rng.Float64() {
					k++
				}
				f.errAcc[i] += float64(k)
			} else {
				f.errAcc[i] += lam
			}
		}
	}
}

func (f *sctFake) stats() (chipStatsFile, bool) {
	var st chipStatsFile
	if f.stale {
		return st, false
	}
	tr := f.trims()
	c := struct {
		Chain   int   `json:"chain"`
		Chips   int   `json:"chips"`
		Base10  int   `json:"base10"`
		Clock10 []int `json:"clock10"`
		Nonces  []int `json:"nonces"`
	}{Chain: f.chain, Chips: f.chips, Base10: f.base10}
	c.Clock10 = make([]int, f.chips+1)
	c.Nonces = make([]int, f.chips+1)
	for i := 1; i <= f.chips; i++ {
		c.Clock10[i] = f.base10
		if !f.noClock {
			c.Clock10[i] += int(math.Round(tr[i] * 10))
		}
		c.Nonces[i] = int(f.nonces[i])
	}
	st.Chains = append(st.Chains, c)
	return st, true
}

func (f *sctFake) errors() (map[ckey]int, error) {
	m := map[ckey]int{}
	for i := 1; i <= f.chips; i++ {
		m[ckey{B: f.chain, I: i}] = int(f.errAcc[i])
	}
	return m, nil
}

func (f *sctFake) restartMiner() {
	for i := range f.nonces {
		f.nonces[i], f.errAcc[i] = 0, 0
	}
}

// background errors: every 5th chip makes 3 an hour (one per 20-min window); the rest none
func sctBackground(chip int) float64 {
	if chip%5 == 0 {
		return 3
	}
	return 0
}

func sctSetup(t *testing.T) *sctFake {
	t.Helper()
	oldAuth := *authFile
	*authFile = filepath.Join(t.TempDir(), "auth.json")
	f := &sctFake{now: time.Unix(1_800_000_000, 0), chain: 2, chips: 46, base10: 6150, hookOn: true}
	f.nonces, f.errAcc = make([]float64, f.chips+1), make([]float64, f.chips+1)
	for i := range f.errAcc {
		f.errAcc[i] = math.Mod(float64(i)*0.618, 1) // each chip's errors fall at its own moments
	}
	f.errRate = func(chip int, trim float64, _ map[int]float64) float64 { return sctBackground(chip) }
	oNow, oStats, oErr, oBoard, oApply, oHook, oNotify, oSpawn, oBlocked, oLater := sctNow, sctReadStats, sctReadErrors, sctReadBoard, sctApply, sctHookOn, sctNotify, sctSpawn, sctBlocked, sctLater
	t.Cleanup(func() {
		sct.mu.Lock()
		sct.reset()
		sct.mu.Unlock()
		sctNow, sctReadStats, sctReadErrors, sctReadBoard, sctApply, sctHookOn, sctNotify, sctSpawn, sctBlocked, sctLater = oNow, oStats, oErr, oBoard, oApply, oHook, oNotify, oSpawn, oBlocked, oLater
		*authFile = oldAuth
	})
	sctNow = func() time.Time { return f.now }
	sctReadStats = f.stats
	sctReadErrors = f.errors
	sctReadBoard = func(chain int) (sctBoardInfo, bool) {
		in := sctBoardInfo{Idle: f.idle}
		if chain == f.chain {
			in.Found, in.Alive, in.Temp = true, true, 70
			if f.temp != nil {
				in.Temp = f.temp(f.trims())
			}
		}
		return in, true
	}
	sctApply = func() error { return nil }
	sctHookOn = func() bool { return f.hookOn }
	sctNotify = func(m string) { f.notes = append(f.notes, m) }
	sctSpawn = func(func()) {} // the test drives the ticks on its virtual clock
	sctBlocked = func() string { return f.blocked }
	sctLater = func(time.Duration, func()) {}
	// the owner's own trims: one on this board (never a candidate, must survive), one on another board
	writeChipTrims([]chipTrim{{0, 7, -25}, {2, 40, -12.5}})
	f.advance(time.Hour) // mining for an hour already
	return f
}

// sctRun ticks the virtual clock until the test ends (or max passes); each() runs before every tick.
func sctRun(t *testing.T, f *sctFake, max time.Duration, each func()) {
	t.Helper()
	end := f.now.Add(max)
	for f.now.Before(end) {
		if each != nil {
			each()
		}
		f.advance(sctTick)
		if !sctTickOnce() {
			return
		}
	}
	t.Fatalf("the test didn't end in %v (step %d, note %q)", max, sct.step, sct.note)
}

func sctLast(t *testing.T) map[string]any {
	t.Helper()
	l, ok := sctView()["last"].(map[string]any)
	if !ok {
		t.Fatal("no result saved")
	}
	return l
}

func sctTrimsOf(chain int) map[int]float64 {
	m := map[int]float64{}
	for _, t := range readChipTrims() {
		if t.Chain == chain {
			m[t.Chip] = t.MHz
		}
	}
	return m
}

func sctKept(t *testing.T, l map[string]any) map[int]float64 {
	m := map[int]float64{}
	for _, c := range l["chips"].([]any) {
		r := c.(map[string]any)
		m[int(r["chip"].(float64))] = r["kept"].(float64)
	}
	return m
}

func sctOwnTrimsIntact(t *testing.T) {
	t.Helper()
	if m := sctTrimsOf(0); len(m) != 1 || m[7] != -25 {
		t.Fatalf("board 0's trim changed: %v", m)
	}
	if m := sctTrimsOf(2); m[40] != -12.5 {
		t.Fatalf("the owner's b2.c40 trim changed: %v", m)
	}
	if _, err := os.Stat(strongBackup()); err == nil {
		t.Fatal("backup left behind")
	}
}

func TestStrongCleanChipsRiseToMax(t *testing.T) {
	f := sctSetup(t)
	if e := sctStart(2, 0, 0); e != "" {
		t.Fatal(e)
	}
	if e := sctStart(2, 0, 0); e == "" {
		t.Fatal("a second test should be refused")
	}
	if !sctRunning() {
		t.Fatal("not running")
	}
	// while it runs: auto-trim pauses, hand edits and the tuner are refused
	if why := cwBlockedReal(); !strings.Contains(why, "strong-chip") {
		t.Fatalf("auto-trim not paused: %q", why)
	}
	rr := httptest.NewRecorder()
	handleChipTrim(rr, httptest.NewRequest("POST", "/api/chiptrim", strings.NewReader(`{"chain":2,"chip":3,"mhz":-12.5}`)))
	if !strings.Contains(rr.Body.String(), "strong-chip test is running") {
		t.Fatalf("hand edit not refused: %s", rr.Body.String())
	}
	sawStep := map[float64]bool{}
	sctRun(t, f, 3*time.Hour, func() {
		for c, m := range sctTrimsOf(2) {
			if c != 40 {
				sawStep[m] = true
			}
		}
	})
	if f.now.Sub(time.Unix(1_800_000_000, 0).Add(time.Hour)) > sctEstimate(sctBaseline, sctStepWin)+time.Minute {
		t.Fatalf("took longer than the estimate %v", sctEstimate(sctBaseline, sctStepWin))
	}
	for _, m := range []float64{12.5, 25, 37.5, 50} {
		if !sawStep[m] {
			t.Fatalf("step %+v never applied (%v)", m, sawStep)
		}
	}
	l := sctLast(t)
	if l["how"] != "done" {
		t.Fatalf("how %v: %v", l["how"], l["message"])
	}
	kept := sctKept(t, l)
	if len(kept) != sctMaxChips {
		t.Fatalf("%d candidates, want %d: %v", len(kept), sctMaxChips, kept)
	}
	stages := map[int]bool{}
	tr := sctTrimsOf(2)
	for c, k := range kept {
		if c%5 == 0 || c == 40 {
			t.Fatalf("chip %d (errors in the baseline, or a hand trim) was a candidate", c)
		}
		if stages[(c-1)/2] {
			t.Fatalf("two candidates in stage of chip %d", c)
		}
		stages[(c-1)/2] = true
		if k != 50 || tr[c] != 50 {
			t.Fatalf("chip %d kept %v, file %v; want +50", c, k, tr[c])
		}
	}
	sctOwnTrimsIntact(t)
	if sctRunning() {
		t.Fatal("still running")
	}
}

func TestStrongChipFailsAt25(t *testing.T) {
	f := sctSetup(t)
	bad := 3 // chip 3 is a candidate (zero baseline errors, its own stage)
	f.errRate = func(chip int, trim float64, _ map[int]float64) float64 {
		if chip == bad && trim >= 25 {
			return 30 // 10 in a 20-min step
		}
		return sctBackground(chip)
	}
	if e := sctStart(2, 0, 0); e != "" {
		t.Fatal(e)
	}
	sctRun(t, f, 3*time.Hour, nil)
	l := sctLast(t)
	kept := sctKept(t, l)
	if _, ok := kept[bad]; !ok {
		t.Fatalf("chip %d wasn't a candidate: %v", bad, kept)
	}
	if kept[bad] != 12.5 || sctTrimsOf(2)[bad] != 12.5 {
		t.Fatalf("chip %d kept %v (file %v), want +12.5", bad, kept[bad], sctTrimsOf(2)[bad])
	}
	for c, k := range kept {
		if c != bad && k != 50 {
			t.Fatalf("chip %d kept %v, want +50", c, k)
		}
	}
	for _, c := range l["chips"].([]any) {
		r := c.(map[string]any)
		if int(r["chip"].(float64)) == bad && (r["state"] != "frozen" || !strings.Contains(r["why"].(string), "+25")) {
			t.Fatalf("chip %d row: %v", bad, r)
		}
	}
	sctOwnTrimsIntact(t)
}

func TestStrongBoardWorseningEndsAndStepsBack(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(f *sctFake)
		want float64
	}{
		{"errors", func(f *sctFake) {
			// at +25 the untouched chips start making errors (voltage pulled between the series stages)
			f.errRate = func(chip int, trim float64, tr map[int]float64) float64 {
				for _, m := range tr {
					if m >= 25 && trim == 0 {
						return 6 + sctBackground(chip)
					}
				}
				return sctBackground(chip)
			}
		}, 12.5},
		{"hashrate", func(f *sctFake) {
			f.nonceMul = func(tr map[int]float64) float64 {
				for _, m := range tr {
					if m >= 37.5 {
						return 0.9
					}
				}
				return 1
			}
		}, 25},
		{"temperature", func(f *sctFake) {
			f.temp = func(tr map[int]float64) float64 {
				for _, m := range tr {
					if m >= 25 {
						return 76
					}
				}
				return 70
			}
		}, 12.5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := sctSetup(t)
			tc.set(f)
			if e := sctStart(2, 0, 0); e != "" {
				t.Fatal(e)
			}
			sctRun(t, f, 3*time.Hour, nil)
			l := sctLast(t)
			if l["how"] != "guard" {
				t.Fatalf("how %v: %v", l["how"], l["message"])
			}
			kept := sctKept(t, l)
			if len(kept) == 0 {
				t.Fatal("no candidates")
			}
			tr := sctTrimsOf(2)
			for c, k := range kept {
				if k != tc.want || tr[c] != tc.want {
					t.Fatalf("chip %d kept %v (file %v), want %+v", c, k, tr[c], tc.want)
				}
			}
			sctOwnTrimsIntact(t)
		})
	}
}

func TestStrongStopMidStepRestores(t *testing.T) {
	f := sctSetup(t)
	before, _ := os.ReadFile(chipTrimPath())
	if e := sctStart(2, 0, 0); e != "" {
		t.Fatal(e)
	}
	// into step 2 (+25): baseline 22 min, step 1 22 min, a few minutes more
	for i := 0; i < int((50*time.Minute)/sctTick); i++ {
		f.advance(sctTick)
		sctTickOnce()
	}
	if sct.step != 2 || sctTrimsOf(2)[sct.chips[0].Chip] != 25 {
		t.Fatalf("expected to be in step 2 at +25: step %d trims %v", sct.step, sctTrimsOf(2))
	}
	if e := sctStop(); e != "" {
		t.Fatal(e)
	}
	if e := sctStop(); e == "" {
		t.Fatal("stopping twice should say nothing runs")
	}
	after, _ := os.ReadFile(chipTrimPath())
	if !bytes.Equal(before, after) {
		t.Fatalf("trims not restored:\n%s\nwant\n%s", after, before)
	}
	l := sctLast(t)
	if l["how"] != "stopped" {
		t.Fatalf("how %v", l["how"])
	}
	for c, k := range sctKept(t, l) {
		if k != 0 {
			t.Fatalf("chip %d kept %v after a stop", c, k)
		}
	}
	sctOwnTrimsIntact(t)
}

func TestStrongPanelRestartRecovers(t *testing.T) {
	f := sctSetup(t)
	before, _ := os.ReadFile(chipTrimPath())
	if e := sctStart(2, 0, 0); e != "" {
		t.Fatal(e)
	}
	for i := 0; i < int((30*time.Minute)/sctTick); i++ {
		f.advance(sctTick)
		sctTickOnce()
	}
	if len(sctTrimsOf(2)) < 2 {
		t.Fatal("no test trims in the file")
	}
	// the panel dies: memory gone, files stay
	sct.mu.Lock()
	sct.reset()
	sct.mu.Unlock()
	sctRecover()
	after, _ := os.ReadFile(chipTrimPath())
	if !bytes.Equal(before, after) {
		t.Fatalf("trims not restored:\n%s", after)
	}
	l := sctLast(t)
	if l["how"] != "interrupted" {
		t.Fatalf("result after recovery: %v", l["how"])
	}
	sctOwnTrimsIntact(t)
	sctRecover() // nothing to do a second time
}

func TestStrongMinerRestartRedoesWindow(t *testing.T) {
	f := sctSetup(t)
	if e := sctStart(2, 0, 0); e != "" {
		t.Fatal(e)
	}
	restarted := false
	sctRun(t, f, 4*time.Hour, func() {
		if !restarted && sct.step == 2 && sct.counting && f.now.Sub(sct.snapAt) > 10*time.Minute {
			f.restartMiner()
			restarted = true
		}
	})
	if !restarted {
		t.Fatal("never restarted")
	}
	l := sctLast(t)
	if l["how"] != "done" {
		t.Fatalf("how %v: %v", l["how"], l["message"])
	}
	for c, k := range sctKept(t, l) {
		if k != 50 {
			t.Fatalf("chip %d kept %v", c, k)
		}
	}
}

func TestStrongAbortsAndRestores(t *testing.T) {
	for _, tc := range []struct {
		name string
		at   time.Duration
		do   func(f *sctFake)
	}{
		{"hook silent", 40 * time.Minute, func(f *sctFake) { f.stale = true }},
		{"clock changed", 40 * time.Minute, func(f *sctFake) { f.base10 = 6000 }},
		{"trims don't reach the chips", 0, func(f *sctFake) { f.noClock = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := sctSetup(t)
			before, _ := os.ReadFile(chipTrimPath())
			if e := sctStart(2, 0, 0); e != "" {
				t.Fatal(e)
			}
			if tc.at == 0 {
				tc.do(f)
			}
			sctRun(t, f, 3*time.Hour, func() {
				if tc.at > 0 && f.now.Sub(sct.started) >= tc.at && f.now.Sub(sct.started) < tc.at+sctTick {
					tc.do(f)
				}
			})
			after, _ := os.ReadFile(chipTrimPath())
			if !bytes.Equal(before, after) {
				t.Fatalf("trims not restored:\n%s", after)
			}
			if l := sctLast(t); l["how"] != "aborted" {
				t.Fatalf("how %v: %v", l["how"], l["message"])
			}
		})
	}
}

func TestStrongRefusals(t *testing.T) {
	f := sctSetup(t)
	try := func(want string, chain int, set func(), undo func()) {
		t.Helper()
		set()
		e := sctStart(chain, 0, 0)
		undo()
		if !strings.Contains(e, want) {
			t.Fatalf("want a refusal with %q, got %q", want, e)
		}
		if sctRunning() {
			t.Fatal("started anyway")
		}
		if _, err := os.Stat(strongBackup()); err == nil {
			t.Fatal("a refused start left a backup")
		}
	}
	nop := func() {}
	try("tuning run", 2, func() { f.blocked = "a tuning run is going: stop it first" }, func() { f.blocked = "" })
	try("switch chip trims on", 2, func() { f.hookOn = false }, func() { f.hookOn = true })
	try("isn't reporting", 2, func() { f.stale = true }, func() { f.stale = false })
	try("board 1 isn't mining", 1, nop, nop)
	try("no such board", 7, nop, nop)
	try("Idle", 2, func() { f.idle = true }, func() { f.idle = false })
	try("°C", 2, func() { f.temp = func(map[int]float64) float64 { return 80 } }, func() { f.temp = nil })
	if e := sctStart(2, 4, 20); !strings.Contains(e, "5 to 240") {
		t.Fatalf("a 4-minute baseline: %q", e)
	}
	if e := sctStart(2, 20, 241); !strings.Contains(e, "5 to 240") {
		t.Fatalf("a 241-minute step: %q", e)
	}
	// every chip already trimmed
	var all []chipTrim
	for i := 1; i <= 46; i++ {
		all = append(all, chipTrim{2, i, -12.5})
	}
	writeChipTrims(all)
	try("already has a trim", 2, nop, nop)
}

func TestStrongRealBlockers(t *testing.T) {
	sctSetup(t)
	old := getCharge()
	defer setCharge(old)
	// the real check: tuner / probe / Idle in charge
	for _, c := range []string{"tuner", "ceiling", "idle"} {
		setCharge(c)
		if why := sctBlockedReal(); why == "" {
			t.Fatalf("charge %q not refused", c)
		}
	}
	setCharge("")
}

func TestStrongAPIWindows(t *testing.T) {
	f := sctSetup(t)
	post := func(body string) map[string]any {
		rr := httptest.NewRecorder()
		handleStrongTest(rr, httptest.NewRequest("POST", "/api/strongtest", strings.NewReader(body)))
		var m map[string]any
		json.Unmarshal(rr.Body.Bytes(), &m)
		return m
	}
	if m := post(`{"start":true,"chain":2,"baseline_min":3,"step_min":5}`); m["ok"] != false || !strings.Contains(m["err"].(string), "5 to 240") {
		t.Fatalf("3-minute baseline accepted: %v", m)
	}
	if m := post(`{"start":true}`); m["ok"] != false {
		t.Fatalf("no board accepted: %v", m)
	}
	m := post(`{"start":true,"chain":2,"baseline_min":5,"step_min":5}`)
	if m["ok"] != true {
		t.Fatalf("start: %v", m)
	}
	now := m["strong"].(map[string]any)["now"].(map[string]any)
	if now["baseline_min"].(float64) != 5 || now["step_min"].(float64) != 5 || now["eta_max_min"].(float64) > 36 {
		t.Fatalf("view: %v", now)
	}
	sctRun(t, f, time.Hour, nil)
	if l := sctLast(t); l["how"] != "done" || l["step_min"].(float64) != 5 {
		t.Fatalf("short-window run: %v %v", l["how"], l["message"])
	}
	if m := post(`{"stop":true}`); m["ok"] != false {
		t.Fatalf("stop with nothing running: %v", m)
	}
}

func TestStrongCandidates(t *testing.T) {
	// a noisy board (typical chip 10 errors): only chips clearly below that qualify; a chip doing little work
	// doesn't; a trimmed chip doesn't
	dErr, dNon := map[int]int{}, map[int]int{}
	for i := 1; i <= 46; i++ {
		dErr[i], dNon[i] = 10, 1000
	}
	dErr[3], dErr[9], dErr[10], dErr[21] = 4, 0, 0, 5
	dErr[30], dNon[30] = 0, 500 // no errors, but half the work
	dErr[41] = 0                // a hand trim
	cs := sctCandidates(46, dErr, dNon, []chipTrim{{2, 41, 25}}, 2)
	var got []int
	for _, c := range cs {
		got = append(got, c.Chip)
	}
	// 9 and 10 share a stage: 9 (fewer errors tie → more nonces tie → lower number) is taken; 21 (5) is above
	// the limit (P(X ≤ 5 | 10) = 6.7 %)
	if len(got) != 2 || got[0] != 3 || got[1] != 9 {
		t.Fatalf("candidates %v", got)
	}
}

// With random (Poisson) errors and nothing getting worse, the test should rarely stop a chip or the board:
// the user's real board made about 0.3 errors per chip per hour.
func TestStrongFalseAlarms(t *testing.T) {
	runs, guards, chips, short := 60, 0, 0, 0
	for r := 0; r < runs; r++ {
		t.Run("", func(t *testing.T) {
			f := sctSetup(t)
			f.rng = rand.New(rand.NewSource(int64(r + 1)))
			f.errRate = func(chip int, _ float64, _ map[int]float64) float64 {
				if chip%9 == 0 {
					return 3
				}
				return 0.3
			}
			if e := sctStart(2, 0, 0); e != "" {
				t.Fatal(e)
			}
			sctRun(t, f, 3*time.Hour, nil)
			l := sctLast(t)
			if l["how"] == "guard" {
				guards++
			}
			for _, k := range sctKept(t, l) {
				chips++
				if k != 50 {
					short++
				}
			}
		})
	}
	t.Logf("%d runs: board guard in %d, %d of %d chips stopped short of +50", runs, guards, short, chips)
	if guards > runs/10 || short > chips/10 {
		t.Fatalf("too many false alarms: guard %d/%d, chips %d/%d", guards, runs, short, chips)
	}
}
