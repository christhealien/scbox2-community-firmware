package main

// r7-race: concurrency around chip trims. Focused, deterministic interleavings (each forces one schedule the Go
// runtime is free to pick) plus a stress run with a deadlock watchdog. Uses the r5-races fake backend
// (rcSetup) and the tunerlocks fake cgminer (fakeCgminer). Never calls the enable/disable path (restartMiner
// runs killall) unless R7_ENABLE=1 in a copy where restartMiner is neutered.

import (
	"fmt"
	"math"
	"math/rand"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// r7Hooks: chip hook on, a fresh stats file (chain 2 at base MHz), empty trims, auto-trim state reset, fake cgminer.
func r7Hooks(t *testing.T, baseMHz int) {
	fakeCgminer(t)
	changeMu.Lock()
	changeAt = time.Time{}
	changeMu.Unlock()
	oldStats := chipStatsPath
	chipStatsPath = *authFile + ".chipstats.json"
	r7WriteStats(baseMHz)
	os.WriteFile(chipHookFlag(), []byte("on\n"), 0o644)
	writeChipTrims(nil)
	cwMu.Lock()
	oldCw, oldLoaded := cwSt, cwLoaded
	cwSt, cwLoaded = cwState{NoHelp: map[string]int64{}}, true
	cwResetWindow()
	cwMu.Unlock()
	t.Cleanup(func() {
		chipStatsPath = oldStats
		cwMu.Lock()
		cwSt, cwLoaded = oldCw, oldLoaded
		cwMu.Unlock()
		cwNow, cwReadErrors, cwBlocked, cwApply = time.Now, readChipErrors, cwBlockedReal, func() error { _, err := reapplyTrims(); return err }
	})
}

func r7WriteStats(baseMHz int) {
	st := fmt.Sprintf(`{"t":%d,"chains":[{"chain":2,"chips":46,"base10":%d,"clock10":[%s],"nonces":[%s]}]}`,
		time.Now().Unix(), baseMHz*10, strings.TrimSuffix(strings.Repeat("0,", 47), ","), strings.TrimSuffix(strings.Repeat("0,", 47), ","))
	os.WriteFile(chipStatsPath+".tmp", []byte(st), 0o644)
	os.Rename(chipStatsPath+".tmp", chipStatsPath)
}

// r7Over: the fastest chip (board clock + its "+" trim) past the clock window, or "".
func r7Over(b *rcBackend) string {
	cur, idle := b.state()
	pp, ok := resolveRunningPlan(cur)
	if !ok || idle || pp.FreqMHz <= 0 {
		return ""
	}
	if _, err := os.Stat(chipHookFlag()); err != nil {
		return ""
	}
	hi := float64(clockWindowHi())
	for _, tr := range readChipTrims() {
		if tr.MHz > 0 && float64(pp.FreqMHz)+tr.MHz > hi+1e-9 {
			return fmt.Sprintf("board %d chip %d runs %d%+g = %g MHz, past the %g MHz window", tr.Chain, tr.Chip, pp.FreqMHz, tr.MHz, float64(pp.FreqMHz)+tr.MHz, hi)
		}
	}
	return ""
}

// The owner raises the clock in Controls while a "+" hand trim is on its way: each checks against the other's
// old value (Controls: trims file has no "+"; the trim: the stored clock is the old one), both pass.
func TestR7PlusTrimVsControlsRaise(t *testing.T) {
	b := rcSetup(t, 1, 0, 0)
	r7Hooks(t, 600)
	var once sync.Once
	hand := make(chan map[string]any, 1)
	b.onPut = func(stack string, nw map[string]any) {
		if !strings.Contains(stack, "handleControl") {
			return
		}
		once.Do(func() { // Controls passed its plusTrimErr check; its PUT is about to land
			go func() { hand <- rcPost(handleChipTrim, map[string]any{"chain": 2, "chip": 5, "mhz": 50}) }()
			select {
			case <-hand: // (unfixed: it ran in between)
				hand <- nil
			case <-time.After(500 * time.Millisecond): // fixed: it waits for Controls to finish
			}
		})
	}
	out := rcPost(handleControl, map[string]any{"freq_mhz": 675, "vrun_mv": 9000, "pv_mv": 9300, "fan1": 60, "fan2": 60, "confirm": true})
	<-hand
	t.Logf("controls: %v", out["ok"])
	if v := r7Over(b); v != "" {
		t.Fatalf("a + chip ended past the clock window: %s (trims %v)", v, readChipTrims())
	}
}

// The same with a clock write through mutatePlanEx (the tuner's steps, the limit probe, presets, the schedule):
// its plusTrimErr check is under planMu, the hand trim's check isn't.
func TestR7PlusTrimVsMutatePlanRaise(t *testing.T) {
	b := rcSetup(t, 2, 0, 0)
	r7Hooks(t, 600)
	var once sync.Once
	hand := make(chan map[string]any, 1)
	b.onPut = func(stack string, nw map[string]any) {
		if !strings.Contains(stack, "mutatePlanEx") {
			return
		}
		once.Do(func() {
			go func() { hand <- rcPost(handleChipTrim, map[string]any{"chain": 2, "chip": 5, "mhz": 50}) }()
			select {
			case <-hand:
				hand <- nil
			case <-time.After(500 * time.Millisecond):
			}
		})
	}
	_, err := mutatePlanGen(noGen, func(p *powerplan) { p.FreqMHz = 675 })
	<-hand
	t.Logf("mutatePlan: %v", err)
	if v := r7Over(b); v != "" {
		t.Fatalf("a + chip ended past the clock window: %s (trims %v)", v, readChipTrims())
	}
}

// A hand trim passes its "is a tuning run going?" check, then a run starts (under controlMu, which the hand
// edit doesn't take): the trim lands inside the run, changing the chips under its tests.
func TestR7HandTrimVsTunerStart(t *testing.T) {
	rcSetup(t, 3, 0, 0)
	r7Hooks(t, 600)
	chipJobMu.Lock() // (a strong-chip start or Undo holding it a moment: the edit waits here, past its charge check)
	hand := make(chan map[string]any, 1)
	go func() { hand <- rcPost(handleChipTrim, map[string]any{"chain": 2, "chip": 7, "mhz": -25}) }()
	time.Sleep(100 * time.Millisecond)
	started := make(chan []chipTrim, 1)
	go func() {
		controlMu.Lock() // what a tuner start does (handleTuner / rcTunerStart): the checks and the charge, under controlMu
		setCharge("tuner")
		snap := readChipTrims()
		controlMu.Unlock()
		started <- snap
	}()
	time.Sleep(100 * time.Millisecond)
	chipJobMu.Unlock()
	out := <-hand
	snap := <-started
	after := readChipTrims()
	t.Logf("hand edit: ok=%v err=%v; trims when the run took charge %v, after %v", out["ok"], out["err"], snap, after)
	if fmt.Sprint(snap) != fmt.Sprint(after) {
		t.Fatalf("a hand trim landed after the tuner took charge: %v -> %v", snap, after)
	}
}

// Auto-trim reads the trims once (cur) and then writes chip by chip, board by board. A hand edit landing in
// between is overwritten from the stale view: here the owner sets a "+" chip to -100 and auto-trim's step-back
// (computed from the old +25) writes +12.5 over it. Serially that can't happen: auto-trim either runs first
// (the owner's -100 then stands) or sees -100 ("trimmed by hand: not the watchdog's") and leaves it.
func TestR7AutoTrimLostHandEdit(t *testing.T) {
	rcSetup(t, 4, 0, 0)
	r7Hooks(t, 600)
	now := time.Unix(1_790_000_000, 0)
	var nowMu sync.Mutex
	cwNow = func() time.Time { nowMu.Lock(); defer nowMu.Unlock(); return now }
	cwBlocked = func() string { return "" }
	cwApply = func() error { return nil }
	cwReadStats = func() (chipStatsFile, bool) {
		var f chipStatsFile
		for _, c := range []int{0, 2} {
			f.Chains = append(f.Chains, struct {
				Chain   int   `json:"chain"`
				Chips   int   `json:"chips"`
				Base10  int   `json:"base10"`
				Clock10 []int `json:"clock10"`
				Nonces  []int `json:"nonces"`
			}{Chain: c, Chips: 46, Base10: 6000})
		}
		return f, true
	}
	errs := map[ckey]int{}
	for _, c := range []int{0, 2} {
		for i := 0; i < 46; i++ {
			errs[ckey{c, i}] = 0
		}
	}
	var errMu sync.Mutex
	cwReadErrors = func() (map[ckey]int, error) {
		errMu.Lock()
		defer errMu.Unlock()
		o := map[ckey]int{}
		for k, v := range errs {
			o[k] = v
		}
		return o, nil
	}
	writeChipTrims([]chipTrim{{2, 20, 25}}) // a "+" hand trim
	cwMu.Lock()
	cwSt.On = true
	cwMu.Unlock()
	chipWatchTick() // the window starts
	errMu.Lock()
	errs[ckey{0, 9}] = 60  // board 0 chip 10: auto-trim slows it (first board)
	errs[ckey{2, 19}] = 60 // board 2 chip 20 (+25 by hand): auto-trim steps it back to +12.5
	errMu.Unlock()
	nowMu.Lock()
	now = now.Add(cwWindow + time.Minute)
	nowMu.Unlock()
	var once sync.Once
	hand := make(chan map[string]any, 1)
	notifyTestHook = func(ev, msg string) {
		if !strings.Contains(msg, "board 0 chip 10 slowed") {
			return
		}
		once.Do(func() { // board 0 is written, board 2 is next (from the trims read before board 0)
			go func() { hand <- rcPost(handleChipTrim, map[string]any{"chain": 2, "chip": 20, "mhz": -100}) }()
			for end := time.Now().Add(500 * time.Millisecond); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
				for _, tr := range readChipTrims() {
					if tr.Chain == 2 && tr.Chip == 20 && tr.MHz == -100 {
						return
					}
				}
			}
		})
	}
	chipWatchTick() // judges
	out := <-hand
	got := 0.0
	for _, tr := range readChipTrims() {
		if tr.Chain == 2 && tr.Chip == 20 {
			got = tr.MHz
		}
	}
	t.Logf("hand edit ok=%v; trims %v", out["ok"], readChipTrims())
	if got != -100 {
		t.Fatalf("the owner's -100 on b2.c20 was overwritten by auto-trim (from its stale +25): now %+g", got)
	}
}

// TestR7Stress: hand edits, Reset, Apply, auto-trim on/off/undo, auto-trim ticks, Controls clock changes,
// mutatePlan clock writes (tuner/probe/preset path) and tuner starts/ends (charge) all at once, in rounds.
// After each round settles: no + chip past the clock window, the run's charge released; a watchdog dumps all
// goroutines and fails when a round doesn't end (deadlock). R7_ROUNDS tunes it.
func TestR7Stress(t *testing.T) {
	rounds := 40
	if s := os.Getenv("R7_ROUNDS"); s != "" {
		fmt.Sscan(s, &rounds)
	}
	b := rcSetup(t, 7, 300*time.Microsecond, 0)
	r7Hooks(t, 600)
	notifyTestHook = func(string, string) {}
	var nowNs atomic.Int64
	nowNs.Store(time.Unix(1_790_000_000, 0).UnixNano())
	cwNow = func() time.Time { return time.Unix(0, nowNs.Load()) }
	cwBlocked = cwBlockedReal
	cwApply = func() error { _, err := reapplyTrims(); return err }
	var errMu sync.Mutex
	errs := map[ckey]int{}
	for i := 0; i < 46; i++ {
		errs[ckey{2, i}] = 0
	}
	cwReadErrors = func() (map[ckey]int, error) {
		errMu.Lock()
		defer errMu.Unlock()
		o := map[ckey]int{}
		for k, v := range errs {
			o[k] = v
		}
		return o, nil
	}
	cwReadStats = func() (chipStatsFile, bool) { s, _, ok := readChipStats(); return s, ok }
	viol := map[string]int{}
	var vmu sync.Mutex
	bad := func(k string) { vmu.Lock(); viol[k]++; vmu.Unlock() }
	for r := 0; r < rounds; r++ {
		rng := rand.New(rand.NewSource(int64(r)))
		seeds := make([]int64, 8)
		for i := range seeds {
			seeds[i] = rng.Int63()
		}
		stop := make(chan struct{})
		var wg sync.WaitGroup
		actor := func(seed int64, f func(*rand.Rand)) {
			wg.Add(1)
			go func() {
				defer wg.Done()
				rr := rand.New(rand.NewSource(seed))
				for {
					select {
					case <-stop:
						return
					default:
					}
					f(rr)
				}
			}()
		}
		actor(seeds[0], func(rr *rand.Rand) { // hand edits, "+" ones too
			m := float64(rr.Intn(13)-8) * trimStep
			rcPost(handleChipTrim, map[string]any{"chain": 2, "chip": 1 + rr.Intn(46), "mhz": m})
		})
		actor(seeds[1], func(rr *rand.Rand) { // Reset / Apply / auto-trim on, off, undo
			switch rr.Intn(5) {
			case 0:
				rcPost(handleChipTrim, map[string]any{"reset": true})
			case 1:
				rcPost(handleChipTrim, map[string]any{"apply": true})
			case 2:
				rcPost(handleChipTrim, map[string]any{"watch": true})
			case 3:
				rcPost(handleChipTrim, map[string]any{"watch": false})
			case 4:
				rcPost(handleChipTrim, map[string]any{"watch_undo": true})
			}
		})
		actor(seeds[2], func(rr *rand.Rand) { // auto-trim ticks, with a bad chip now and then
			errMu.Lock()
			errs[ckey{2, rr.Intn(46)}] += rr.Intn(40)
			errMu.Unlock()
			nowNs.Add(int64(cwWindow / 3))
			r7WriteStats(600)
			chipWatchTick()
		})
		actor(seeds[3], func(rr *rand.Rand) { // Controls: manual clock
			f := 550 + 12.5*float64(rr.Intn(13))
			if f != math.Trunc(f) {
				f -= 12.5
			}
			rcPost(handleControl, map[string]any{"freq_mhz": int(f), "vrun_mv": 9000, "pv_mv": 9300, "fan1": 60, "fan2": 60, "confirm": true})
		})
		actor(seeds[4], func(rr *rand.Rand) { // the tuner's / probe's / schedule's clock writes
			f := []int{575, 600, 625, 650, 675, 700}[rr.Intn(6)]
			mutatePlanGen(noGen, func(p *powerplan) { p.FreqMHz = f })
		})
		actor(seeds[5], func(rr *rand.Rand) { // a tuner start (charge, under controlMu) and its end
			controlMu.Lock()
			ok := getCharge() == ""
			if ok {
				setCharge("tuner")
			}
			controlMu.Unlock()
			time.Sleep(time.Duration(rr.Intn(3)) * time.Millisecond)
			if ok {
				swapCharge("tuner", "")
			}
		})
		if os.Getenv("R7_ENABLE") == "1" { // (only in a copy where restartMiner's killall is neutered)
			actor(seeds[7], func(rr *rand.Rand) {
				rcPost(handleChipTrim, map[string]any{"enable": rr.Intn(4) != 0})
				time.Sleep(2 * time.Millisecond)
			})
		}
		actor(seeds[6], func(rr *rand.Rand) { // views (take cwMu, sct.mu)
			chipTrimView()
			runtime.Gosched()
		})
		time.Sleep(150 * time.Millisecond)
		close(stop)
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(60 * time.Second):
			buf := make([]byte, 1<<22)
			n := runtime.Stack(buf, true)
			t.Fatalf("round %d didn't end (deadlock?):\n%s", r, buf[:n])
		}
		setCharge("")
		if v := r7Over(b); v != "" {
			bad("+ chip past the window")
			t.Logf("round %d: %s", r, v)
			writeChipTrims(nil) // next round from a clean state
		}
	}
	vmu.Lock()
	defer vmu.Unlock()
	if len(viol) > 0 {
		t.Fatalf("violations: %v", viol)
	}
}

// Auto-trim checks "is a tuning run going?" (cwBlocked) once, at the top of its tick, under cwMu; a run's start
// doesn't take cwMu, so a run that starts during the tick gets the tick's trim written (and re-sent to the
// miner) inside it.
func TestR7AutoTrimVsTunerStart(t *testing.T) {
	rcSetup(t, 5, 0, 0)
	r7Hooks(t, 600)
	notifyTestHook = func(string, string) {}
	now := time.Unix(1_790_000_000, 0)
	var nowMu sync.Mutex
	cwNow = func() time.Time { nowMu.Lock(); defer nowMu.Unlock(); return now }
	cwBlocked = cwBlockedReal
	cwReadStats = func() (chipStatsFile, bool) { s, _, ok := readChipStats(); return s, ok }
	errs := map[ckey]int{}
	for i := 0; i < 46; i++ {
		errs[ckey{2, i}] = 0
	}
	ended := make(chan struct{})
	oldEnded := tunerEnded
	tunerEnded = func(e *tunerEngine) { oldEnded(e); close(ended) }
	t.Cleanup(func() { tunerEnded = oldEnded })
	var judging atomic.Bool
	var once sync.Once
	tuner := make(chan map[string]any, 1)
	cwReadErrors = func() (map[ckey]int, error) {
		if judging.Load() {
			once.Do(func() { // past the tick's "tuner running?" check
				cfg := defaultTunerConfig()
				cfg.DryRun = true
				go func() { tuner <- rcPost(handleTuner, map[string]any{"action": "start", "config": cfg}) }()
				for end := time.Now().Add(500 * time.Millisecond); time.Now().Before(end) && getCharge() != "tuner"; time.Sleep(5 * time.Millisecond) {
				}
			})
		}
		o := map[ckey]int{}
		for k, v := range errs {
			o[k] = v
		}
		return o, nil
	}
	var during atomic.Bool
	cwApply = func() error {
		if getCharge() == "tuner" { // the run took charge (its Run starts right after): its tests have begun
			during.Store(true)
		}
		return nil
	}
	cwMu.Lock()
	cwSt.On = true
	cwMu.Unlock()
	chipWatchTick()
	errs[ckey{2, 9}] = 60
	now = now.Add(cwWindow + time.Minute)
	judging.Store(true)
	chipWatchTick()
	out := <-tuner
	tunerMu.Lock()
	e := tunerCur
	tunerMu.Unlock()
	if e != nil {
		e.Stop()
		select {
		case <-ended: // (the supervisor's end: export and notify)
		case <-time.After(20 * time.Second):
			t.Fatal("the dry run didn't end")
		}
	}
	t.Logf("tuner start: %v %v; trims %v", out["ok"], out["err"], readChipTrims())
	if during.Load() {
		t.Fatalf("auto-trim wrote and re-sent a trim after a tuning run had started (trims %v)", readChipTrims())
	}
}

// On a stock level (manual off) the setting still holds the last manual plan (Controls' Stock preset and the
// schedule's stock leave manualPowerplan as it was). reapplySetClock reads that one: the "+" check runs against a
// clock the miner isn't on, and every live re-send is refused ("not yet at the set ... MHz") while the boards
// sit at the stock clock.
func TestR7SetClockOnStockLevel(t *testing.T) {
	b := rcSetup(t, 6, 0, 0)
	b.mu.Lock()
	b.setting["manual"] = false
	b.setting["select"] = float64(0)                                      // 615 MHz stock level
	b.setting["manualPowerplan"] = "550 MHz 8900 V 60 RPM 60 RPM PV 9200" // an old manual plan
	b.mu.Unlock()
	r7Hooks(t, 615)
	set, ok := reapplySetClock()
	out := rcPost(handleChipTrim, map[string]any{"chain": 2, "chip": 3, "mhz": -25})
	t.Logf("set clock %d %v; note %v", set, ok, out["note"])
	if !ok || set != 615 {
		t.Errorf("the set clock read %d (ok=%v), the miner runs its 615 MHz stock level", set, ok)
	}
	if n, _ := out["note"].(string); !strings.HasPrefix(n, "applied") {
		t.Errorf("a hand trim on the stock level wasn't applied live: %q", n)
	}
}
