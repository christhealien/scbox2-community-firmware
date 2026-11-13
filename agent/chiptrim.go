package main

// Chip trims (step one: manual). A per-chip clock offset from the board clock, applied inside the miner by
// the chip-hook copy of intminer and /lib/sclite_chip.so (see the firmware kit). This side:
//   - switches the hook on/off (the flag file the intminer wrapper looks for; mining restarts to change it),
//   - keeps the trims file the hook reads (chain chip mhz),
//   - re-applies trims by re-sending the current clock through the miner API (pgaset clock), which runs the
//     miner's clock-change function and so the hook; restarting mining is the fallback,
//   - reads the hook's per-chip counters (/tmp/sclite-chipstats.json: clocks in tenths of a MHz, good nonces).

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	chipStatsPath = brandTmp + "chipstats.json"
	chipHookLog   = brandTmp + "chiphook.log"
)

const (
	trimMin  = -100.0 // manual limits (MHz), the hook clamps to -100 .. +50
	trimMax  = 50.0   // a hand trim may run a strong chip up to 50 MHz faster than its board (the tuner and auto-trim only slow)
	trimStep = 12.5
)

func chipHookFlag() string { return filepath.Join(filepath.Dir(*authFile), "chiphook") }
func chipTrimPath() string { return filepath.Join(filepath.Dir(*authFile), "chiptrim") }

type chipTrim struct {
	Chain int     `json:"chain"`
	Chip  int     `json:"chip"`
	MHz   float64 `json:"mhz"`
}

var chipTrimMu sync.Mutex

func readChipTrims() []chipTrim {
	b, err := os.ReadFile(chipTrimPath())
	if err != nil {
		return nil
	}
	return parseChipTrims(b)
}

// parseChipTrims: the trims in a trims file's bytes (the file itself, or a backup of it).
func parseChipTrims(b []byte) []chipTrim {
	var out []chipTrim
	// (the file is read exactly as the trim library reads it, sclite_chip.c read_trims: a line past the limits,
	// off the 12.5 MHz grid or not plain numbers is dropped, as it is there; and as there, for a chip named twice
	// the last valid line counts, a 0 line too: the hook's want[chip] is overwritten by each line it accepts)
	at := map[[2]int]int{}
	for _, t := range parseTrimsLikeHook(b) {
		if t.MHz >= trimMin && t.MHz <= trimMax && onTrimGrid(t.MHz) && t.Chip >= 1 {
			if i, seen := at[[2]int{t.Chain, t.Chip}]; seen {
				out[i] = t
				continue
			}
			at[[2]int{t.Chain, t.Chip}] = len(out)
			out = append(out, t)
		}
	}
	kept := out[:0]
	for _, t := range out {
		if t.MHz != 0 {
			kept = append(kept, t)
		}
	}
	out = kept
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Chain != out[j].Chain {
			return out[i].Chain < out[j].Chain
		}
		return out[i].Chip < out[j].Chip
	})
	return out
}

// parseTrimsLikeHook: a line-for-line port of the library's parser (read_trims, parse_num, field_end, whole):
// the first 16383 bytes, at most 256 lines read, each "chain chip mhz" with blanks between, chain and chip whole
// numbers 0..99, mhz a plain decimal with an optional sign.
func parseTrimsLikeHook(file []byte) []chipTrim {
	buf := file
	if len(buf) > 16383 {
		buf = buf[:16383]
	}
	num := func(p int) (int, float64, bool) {
		sign, v, frac, any := 1.0, 0.0, 0.1, false
		for p < len(buf) && (buf[p] == ' ' || buf[p] == '\t') {
			p++
		}
		if p < len(buf) && buf[p] == '-' {
			sign = -1
			p++
		} else if p < len(buf) && buf[p] == '+' {
			p++
		}
		for p < len(buf) && buf[p] >= '0' && buf[p] <= '9' {
			v = v*10 + float64(buf[p]-'0')
			p++
			any = true
		}
		if p < len(buf) && buf[p] == '.' {
			p++
			for p < len(buf) && buf[p] >= '0' && buf[p] <= '9' {
				v += float64(buf[p]-'0') * frac
				frac /= 10
				p++
				any = true
			}
		}
		return p, sign * v, any
	}
	fieldEnd := func(p int) bool {
		return p == len(buf) || buf[p] == ' ' || buf[p] == '\t' || buf[p] == '\r' || buf[p] == '\n'
	}
	whole := func(v float64) bool { return v >= 0 && v <= 99 && v == math.Trunc(v) }
	var out []chipTrim
	p := 0
	for p < len(buf) && len(out) < 256 {
		if buf[p] == '#' {
			for p < len(buf) && buf[p] != '\n' {
				p++
			}
		} else {
			var a, b, c float64
			ok := false
			if p, a, ok = num(p); ok && fieldEnd(p) && whole(a) {
				if p, b, ok = num(p); ok && fieldEnd(p) && whole(b) {
					if p, c, ok = num(p); ok && fieldEnd(p) {
						out = append(out, chipTrim{Chain: int(a), Chip: int(b), MHz: c})
					}
				}
			}
		}
		for p < len(buf) && buf[p] != '\n' {
			p++
		}
		if p < len(buf) {
			p++
		}
	}
	return out
}

// onTrimGrid: exactly a multiple of 12.5 MHz, the hook's own test (sclite_chip.c: off*2/25 a whole number); a value
// only within 1e-9 of the grid (12.5000000001) is skipped there, so the panel mustn't show it as a trim.
func onTrimGrid(m float64) bool { q := m * 2 / 25; return q == math.Trunc(q) }

func writeChipTrims(ts []chipTrim) error {
	var b strings.Builder
	b.WriteString("# chain chip mhz: per-chip clock offset from the board clock (written by the panel)\n")
	for _, t := range ts {
		if t.MHz != 0 {
			fmt.Fprintf(&b, "%d %d %s\n", t.Chain, t.Chip, strconv.FormatFloat(t.MHz, 'f', -1, 64))
		}
	}
	return writeAtomic(chipTrimPath(), []byte(b.String()), 0o644) // flushed before the rename: never an empty file after a power cut
}

// the hook's counters
type chipStatsFile struct {
	T      int64 `json:"t"`
	Chains []struct {
		Chain   int   `json:"chain"`
		Chips   int   `json:"chips"`
		Base10  int   `json:"base10"`
		Clock10 []int `json:"clock10"`
		Nonces  []int `json:"nonces"`
	} `json:"chains"`
}

type chipRow struct {
	Chip   int     `json:"chip"`
	Clock  float64 `json:"clock"`
	Trim   float64 `json:"trim"`
	Nonces int     `json:"nonces"`
	Share  float64 `json:"share"` // nonces ÷ the chain's average at this chip's clock (per MHz), %
	Low    bool    `json:"low"`   // clearly below average, not just luck (> 3 standard deviations)
}

type chainView struct {
	Chain int       `json:"chain"`
	Base  float64   `json:"base"`
	Chips []chipRow `json:"chips"`
	Avg   float64   `json:"avg"` // nonces per chip, the board average
}

func readChipStats() (chipStatsFile, time.Time, bool) {
	var s chipStatsFile
	fi, err := os.Stat(chipStatsPath)
	if err != nil {
		return s, time.Time{}, false
	}
	b, err := os.ReadFile(chipStatsPath)
	if err != nil || json.Unmarshal(b, &s) != nil {
		return s, fi.ModTime(), false
	}
	// the hook lists the boards in the order the miner set them up (2, 1, ...): show them 0, 1, 2, 3
	sort.SliceStable(s.Chains, func(i, j int) bool { return s.Chains[i].Chain < s.Chains[j].Chain })
	for i := range s.Chains {
		// the library writes at most 62 chips a chain: a damaged file can't make every view loop for ever
		s.Chains[i].Chips = max(0, min(s.Chains[i].Chips, 62))
	}
	return s, fi.ModTime(), true
}

func chipTrimView() map[string]any {
	_, flagErr := os.Stat(chipHookFlag())
	trims := readChipTrims()
	tmap := map[[2]int]float64{}
	for _, t := range trims {
		tmap[[2]int{t.Chain, t.Chip}] = t.MHz
	}
	st, mod, ok := readChipStats()
	active := ok && time.Since(mod) < 45*time.Second
	var chains []chainView
	if ok && time.Since(mod) < 10*time.Minute { // an older report is from an earlier run: its clocks aren't live
		for _, c := range st.Chains {
			cv := chainView{Chain: c.Chain, Base: float64(c.Base10) / 10}
			// a chip's nonces follow its own clock: a chip trimmed −25 MHz at 650 finds ~4 % fewer with nothing
			// wrong. Compare each chip with what the board's average finds per MHz at that chip's clock, so a
			// trimmed chip isn't shown as low (and a "+" chip as strong) just for its trim.
			clk := func(i int) float64 {
				if i < len(c.Clock10) && c.Clock10[i] > 0 {
					return float64(c.Clock10[i])
				}
				return float64(c.Base10)
			}
			// the counters run since mining started, but a trim changes a chip's clock live: count from the
			// clocks' last change (a chip slowed an hour ago after 10 h at the board clock read 107 %)
			c.Nonces = chipSegNonces(c.Chain, c.Clock10, c.Nonces)
			sum, n, sumClk := 0, 0, 0.0
			for i := 1; i <= c.Chips && i < len(c.Nonces); i++ {
				sum += c.Nonces[i]
				sumClk += clk(i)
				n++
			}
			avg := 0.0
			if n > 0 {
				avg = float64(sum) / float64(n)
			}
			cv.Avg = math.Round(avg*10) / 10
			for i := 1; i <= c.Chips; i++ {
				r := chipRow{Chip: i, Trim: tmap[[2]int{c.Chain, i}]}
				if i < len(c.Clock10) {
					r.Clock = float64(c.Clock10[i]) / 10
				}
				if i < len(c.Nonces) {
					r.Nonces = c.Nonces[i]
					if exp := float64(sum) / sumClk * clk(i); avg > 0 && sumClk > 0 && exp > 0 {
						r.Share = math.Round(float64(r.Nonces)/exp*1000) / 10
						r.Low = avg >= 30 && (float64(r.Nonces)-exp)/math.Sqrt(exp) < -3
					}
				}
				cv.Chips = append(cv.Chips, r)
			}
			chains = append(chains, cv)
		}
	}
	var offWhy map[string]any
	if flagErr != nil {
		if b, err := os.ReadFile(chipHookFlag() + ".off"); err == nil {
			json.Unmarshal(b, &offWhy)
		}
	}
	age, onFor := int64(-1), int64(-1) // seconds since the hook's last report, and since trims were switched on
	if !mod.IsZero() {
		age = int64(time.Since(mod).Seconds())
	}
	if fi, err := os.Stat(chipHookFlag()); err == nil {
		onFor = int64(time.Since(fi.ModTime()).Seconds())
	}
	return map[string]any{"ok": true, "enabled": flagErr == nil, "active": active, "off_why": offWhy, "stats_age_s": age, "on_s": onFor, "trims": trims, "strong": sctView(),
		"chains": chains, "log": tailLines(chipHookLog, 8), "watch": chipWatchView(), "limits": map[string]float64{"min": trimMin, "max": trimMax, "step": trimStep},
		"geo": chipGeo()}
}

// chipSeg: each chain's counters at the last reading before its chip clocks changed, so the per-MHz comparison
// uses (almost) only nonces found at the clocks the chips run now (the readings come every 10 min at least,
// chipHistSample). After a mining restart the counters start again from 0 at the new clocks. Until the panel
// has seen a change (just after it started) the counts since mining started are used, as before.
var (
	chipSegMu sync.Mutex
	chipSeg   = map[int]chipSegBase{}
)

type chipSegBase struct{ clock10, base, last []int }

func chipSegReset() { chipSegMu.Lock(); chipSeg = map[int]chipSegBase{}; chipSegMu.Unlock() }

// chipSegNonces: the chain's nonces since its clocks last changed.
func chipSegNonces(chain int, clock10, nonces []int) []int {
	chipSegMu.Lock()
	defer chipSegMu.Unlock()
	s, seen := chipSeg[chain]
	same := seen && len(s.last) == len(nonces)
	restarted := false
	for i := 0; same && i < len(nonces); i++ {
		restarted = restarted || nonces[i] < s.last[i]
	}
	if !seen || !same || restarted {
		s.base = make([]int, len(nonces)) // first reading, or the counters restarted with mining: from 0
	} else if len(s.clock10) != len(clock10) || !slicesEqualInt(s.clock10, clock10) {
		s.base = s.last // the clocks changed since the last reading: count from that reading
	}
	s.clock10, s.last = append([]int(nil), clock10...), append([]int(nil), nonces...)
	chipSeg[chain] = s
	out := make([]int, len(nonces))
	for i := range nonces {
		out[i] = nonces[i]
		if i < len(s.base) {
			out[i] -= s.base[i]
		}
	}
	return out
}

func slicesEqualInt(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func tailLines(p string, n int) []string {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	ls := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(ls) > n {
		ls = ls[len(ls)-n:]
	}
	return ls
}

// cgminerCmdParam sends {"command": c, "parameter": p} to the miner API.
func cgminerCmdParam(c, p string) ([]byte, error) {
	d := net.Dialer{Timeout: *apiTimeout}
	conn, err := d.Dial("tcp", *apiAddr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(*apiTimeout))
	q, _ := json.Marshal(map[string]string{"command": c, "parameter": p})
	if _, err := conn.Write(q); err != nil {
		return nil, err
	}
	out, done, err := apiRead(conn)
	if len(out) == 0 || (!done && !strings.Contains(string(out), "STATUS")) {
		if err == nil {
			err = io.EOF
		}
		return nil, fmt.Errorf("no answer from the miner (%v)", err) // e.g. a timeout: not "refused"
	}
	return []byte(strings.TrimRight(string(out), "\x00")), nil
}

// reapplyTrims re-sends each chain's current board clock through the miner API: the miner's clock-change
// function runs (nothing to ramp, the clock is the same) and its hook applies the trims file.
// reapplySetClock: the clock (MHz) the miner is set to: the plan it runs (the manual one, or the selected stock
// level's: on a stock level the setting still holds the last manual plan); on Idle the manual plan, as before (a
// "+" is still checked against a clock); ok=false when it can't be read.
var reapplySetClock = func() (int, bool) {
	raw, err := stockGet("/mcb/setting")
	if err != nil {
		return 0, false
	}
	var cur map[string]any
	if json.Unmarshal(raw, &cur) != nil {
		return 0, false
	}
	if pp, ok := resolveRunningPlan(cur); ok && pp.FreqMHz > 0 {
		return pp.FreqMHz, true
	}
	pp, ok := storedPlan()
	return pp.FreqMHz, ok && pp.FreqMHz > 0
}

func reapplyTrims() (string, error) {
	st, at, ok := readChipStats()
	if !ok || len(st.Chains) == 0 {
		return "", fmt.Errorf("the trim library isn't reporting yet")
	}
	// the clock is re-sent from this file: only a fresh one, written after the last setting change (an old
	// one would set the boards back to an old clock)
	if ch, _ := lastMinerChange(); time.Since(at) > 45*time.Second || at.Before(ch) {
		return "", fmt.Errorf("the trim library's report isn't fresh yet: try again in a minute")
	}
	// the report holds each board's live clock: while the miner (re)initialises a board it passes through 50 MHz
	// and a ramp via 500, and just after a setting change it can hold the old clock. Re-sending that would set it
	// as the board's clock: only re-send a clock within a step of the one the miner is set to.
	if set, ok := reapplySetClock(); ok {
		for _, c := range st.Chains {
			if d := c.Base10 - set*10; d > 124 || d < -124 {
				return "", fmt.Errorf("board %d is at %s MHz, not yet at the set %d MHz: try again in a minute", c.Chain, strconv.FormatFloat(float64(c.Base10)/10, 'f', -1, 64), set)
			}
		}
	}
	raw, err := cgminerCmd("devs")
	if err != nil {
		return "", err
	}
	var dv struct {
		Devs []map[string]any `json:"DEVS"`
	}
	json.Unmarshal(raw, &dv)
	var notes []string
	for _, c := range st.Chains {
		pga := -1
		for i, d := range dv.Devs {
			id, _ := d["ID"].(float64)
			if int(id) == c.Chain {
				pga = i
				if v, ok := d["PGA"].(float64); ok {
					pga = int(v)
				}
			}
		}
		if pga < 0 && len(dv.Devs) == 1 {
			pga = 0
		}
		if pga < 0 {
			return "", fmt.Errorf("the miner doesn't list board %d", c.Chain)
		}
		clk := strconv.FormatFloat(float64(c.Base10)/10, 'f', -1, 64)
		out, err := cgminerCmdParam("pgaset", fmt.Sprintf("%d,clock,%s", pga, clk))
		if err != nil {
			return "", err
		}
		s := string(out)
		if !strings.Contains(s, `"STATUS":"S"`) && !strings.Contains(s, "set OK") {
			msg := s
			if i := strings.Index(s, `"Msg":"`); i >= 0 {
				msg = s[i+7:]
				if j := strings.Index(msg, `"`); j >= 0 {
					msg = msg[:j]
				}
			}
			return "", fmt.Errorf("the miner refused it: %s", msg)
		}
		notes = append(notes, fmt.Sprintf("board %d at %s MHz", c.Chain, clk))
	}
	return strings.Join(notes, ", "), nil
}

// GET: state. POST {enable}, {chain, chip, mhz}, {reset:true}, {apply:true}.
func handleChipTrim(w http.ResponseWriter, r *http.Request) {
	if !featGate(w, model.Feat.ChipTrims, "chip trims") {
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, chipTrimView())
		return
	}
	var in struct {
		Enable *bool    `json:"enable"`
		Chain  int      `json:"chain"`
		Chip   int      `json:"chip"`
		MHz    *float64 `json:"mhz"`
		Reset  bool     `json:"reset"`
		Apply  bool     `json:"apply"`
		Watch  *bool    `json:"watch"`
		Undo   bool     `json:"watch_undo"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		// (a bad "mhz" leaves MHz pointing at 0: without this check it would take the chip's trim off)
		writeJSON(w, map[string]any{"ok": false, "err": badJSON(err)})
		return
	}
	ctl := in.Undo || in.Watch == nil // (switching auto-trim on/off changes no trim)
	if ctl {
		// the charge check and the change in one go (a tuning run starts under controlMu), and no clock change
		// between a "+" trim's check against the clock and its write (Controls, presets, the schedule and the
		// tuner's start check the trims under controlMu). Lock order: controlMu → chipJobMu → cwMu → sct.mu →
		// chipTrimMu → planMu.
		controlMu.Lock()
	}
	unlockControl := func() {
		if ctl {
			ctl = false
			controlMu.Unlock()
		}
	}
	defer unlockControl()
	if c := getCharge(); (c == "tuner" || c == "ceiling") && (in.Undo || in.Watch == nil) {
		// a trim edit, Reset, Undo auto-trims or switching the hook (a mining restart) would change the chips
		// under the run's tests and its own trims; switching auto-trim on/off is fine (it pauses during runs).
		// The limit probe steps the clock to the unlocked ceiling: a "+" chip makes its steps fail (each read as
		// a firmware cap), a mining restart spoils its readings.
		what := map[bool]string{true: "a tuning run is going", false: "the limit probe is running"}[c == "tuner"]
		writeJSON(w, map[string]any{"ok": false, "err": what + ": change chip trims after it (or stop it)"})
		return
	}
	if in.Undo {
		// the strong-chip test rewrites the trims file from the trims it saw at its start when it ends: auto-trims
		// taken off now would come back then, no longer the watchdog's (Undo forgets them), so they'd read as hand
		// trims. One at a time with its start (chipJobMu, then cwMu in chipWatchSet).
		chipJobMu.Lock()
		defer chipJobMu.Unlock()
		if sctRunning() {
			writeJSON(w, map[string]any{"ok": false, "err": "the strong-chip test is running: undo auto-trims after it (or stop it)"})
			return
		}
	}
	if in.Watch != nil || in.Undo {
		note, err := chipWatchSet(in.Watch, in.Undo)
		unlockControl()
		if err != nil {
			writeJSON(w, map[string]any{"ok": false, "err": err.Error()})
			return
		}
		if in.Watch != nil && *in.Watch {
			if _, err := os.Stat(chipHookFlag()); err != nil {
				note = "on: it starts watching once chip trims are switched on"
			} else {
				note = fmt.Sprintf("on: the first check is after %s of mining", humanDur(cwWindow))
			}
		}
		v := chipTrimView()
		v["note"] = note
		writeJSON(w, v)
		return
	}
	chipJobMu.Lock() // no strong-chip search may start between the checks below and the edit (lock order: chipJobMu first)
	if sctRunning() {
		chipJobMu.Unlock()
		writeJSON(w, map[string]any{"ok": false, "err": "the strong-chip test is running: stop it first (every chip goes back to its trim from before)"})
		return
	}
	cwMu.Lock() // not inside an auto-trim tick: it writes from the trims it read at its start (a hand edit would be lost)
	note, done := chipTrimChange(w, in.Enable, in.Chain, in.Chip, in.MHz, in.Reset, in.Apply)
	cwMu.Unlock()
	chipJobMu.Unlock()
	unlockControl() // (before the wait below)
	if done {
		return
	}
	if strings.Contains(note, "isn't fresh yet") {
		// any settings write (the fan curve's, every few tens of seconds) makes the hook's last report stale until
		// its next one, within 10 s: wait for it once (outside the locks) instead of saying "use Restart mining"
		if _, at, ok := readChipStats(); ok {
			for i := 0; i < 12; i++ {
				time.Sleep(time.Second)
				if _, at2, ok2 := readChipStats(); ok2 && at2.After(at) {
					note = applyNote()
					break
				}
			}
		}
	}
	v := chipTrimView() // after chipTrimMu is released: the view takes the strong test's lock (always that order)
	v["note"] = note
	writeJSON(w, v)
}

// chipTrimChange: the change itself, under chipTrimMu. done: an error was already written.
func chipTrimChange(w http.ResponseWriter, enable *bool, chain, chip int, mhz *float64, reset, apply bool) (note string, done bool) {
	chipTrimMu.Lock()
	defer chipTrimMu.Unlock()
	in := struct {
		Enable      *bool
		Chain, Chip int
		MHz         *float64
		Reset       bool
		Apply       bool
	}{enable, chain, chip, mhz, reset, apply}
	switch {
	case in.Enable != nil:
		if *in.Enable {
			planMu.Lock() // no clock write (mutatePlanEx, and mutatePlanBox2Write on the Box II, check the trims under planMu on a raise) between the check and the switch
			set, ok := reapplySetClock()
			for _, t := range readChipTrims() {
				if t.MHz > 0 && !ok { // (unchecked, the "+" could run past the window at whatever clock it is)
					planMu.Unlock()
					writeJSON(w, map[string]any{"ok": false, "err": fmt.Sprintf("board %d chip %d is trimmed %+.4g MHz and the board clock can't be read to check it against the %d MHz clock window: try again in a minute", t.Chain, t.Chip, t.MHz, chipTrimCeil())})
					return note, true
				}
				if t.MHz > 0 && float64(set)+t.MHz > float64(chipTrimCeil())+1e-9 {
					planMu.Unlock()
					writeJSON(w, map[string]any{"ok": false, "err": fmt.Sprintf("board %d chip %d is trimmed %+.4g MHz: at the %d MHz board clock it would run past the %d MHz clock window (lower the board clock first)", t.Chain, t.Chip, t.MHz, set, chipTrimCeil())})
					return note, true
				}
			}
			if featOff(model.Feat.ChipTrims) {
				// Defense-in-depth: the chip-trim hook flag file must NEVER be written on a model with chip
				// trims off. The kit's intminer wrapper preloads the trim library only when this flag exists, so
				// leaving it unwritten keeps the stock (untrimmed) miner running. handleChipTrim already refuses,
				// this guards the one write of the flag directly.
				planMu.Unlock()
				writeJSON(w, map[string]any{"ok": false, "disabled": true,
					"err": "chip trims are not available on this model (" + model.Name + ")"})
				return note, true
			}
			chipHookArmedByHand()
			for _, m := range []string{brandTmp + "hook.1", brandTmp + "hook.2", brandTmp + "hook.3"} {
				os.Remove(m) // the wrapper's start tries: switched on by hand, it gets its 3 again
			}
			os.Remove(chipHookFlag() + ".off")
			err := cfgWrite(chipHookFlag(), []byte("on\n"), 0o644)
			planMu.Unlock() // (not held over the mining restart)
			if err != nil {
				writeJSON(w, map[string]any{"ok": false, "err": err.Error()})
				return note, true
			}
		} else {
			os.Remove(chipHookFlag())
		}
		markReboot(150*time.Second, 5*time.Minute, "Restarting the mining software to switch chip trims "+map[bool]string{true: "on", false: "off"}[*in.Enable]+" — hashing resumes shortly.")
		restartMiner()
		activity.emit("🔧 chip trims " + map[bool]string{true: "switched on", false: "switched off (stock miner)"}[*in.Enable] + " — mining restarting")
		note = "mining is restarting (about a minute)"
	case in.Reset:
		if err := writeChipTrims(nil); err != nil {
			writeJSON(w, map[string]any{"ok": false, "err": err.Error()})
			return note, true
		}
		activity.emit("🔧 chip trims: all reset to the board clock")
		note = applyNote()
	case in.Apply:
		note = applyNote()
	case in.MHz != nil:
		m := *in.MHz
		if in.Chip < 1 || in.Chip > 63 || !chipTrimBoardOK(in.Chain) {
			writeJSON(w, map[string]any{"ok": false, "err": "no such chip"})
			return note, true
		}
		if math.IsNaN(m) || m < trimMin || m > trimMax || math.Abs(m/trimStep-math.Round(m/trimStep)) > 1e-9 {
			writeJSON(w, map[string]any{"ok": false, "err": fmt.Sprintf("a trim is %.4g to +%.4g MHz in steps of %.4g", trimMin, trimMax, trimStep)})
			return note, true
		}
		m = math.Round(m/trimStep) * trimStep // exactly on the grid: the hook skips 12.5000000001
		if m != 0 && in.Chip > expectedChips {
			// never applied (the hook stops at the board's 46) nor shown, but a "+" one would block every clock
			// raise. 0 stays possible: it clears a line an older panel took.
			writeJSON(w, map[string]any{"ok": false, "err": "no such chip"})
			return note, true
		}
		if m > 0 {
			// no clock write (mutatePlanEx, and mutatePlanBox2Write on the Box II, check the trims under planMu: the tuner's steps, the limit probe) between
			// this check and the write below
			planMu.Lock()
			set, ok := reapplySetClock()
			if !ok {
				planMu.Unlock()
				writeJSON(w, map[string]any{"ok": false, "err": fmt.Sprintf("the board clock can't be read to check a %+.4g MHz trim against the %d MHz clock window: try again in a minute", m, chipTrimCeil())})
				return note, true
			}
			if float64(set)+m > float64(chipTrimCeil())+1e-9 {
				planMu.Unlock()
				writeJSON(w, map[string]any{"ok": false, "err": fmt.Sprintf("at %+.4g MHz this chip would run %.4g MHz, past the %d MHz clock window (Unlock clock limit in Clock / voltage, or lower the board clock)", m, float64(set)+m, chipTrimCeil())})
				return note, true
			}
		}
		ts := readChipTrims()
		found := false
		for i := range ts {
			if ts[i].Chain == in.Chain && ts[i].Chip == in.Chip {
				ts[i].MHz, found = m, true
			}
		}
		if !found {
			ts = append(ts, chipTrim{Chain: in.Chain, Chip: in.Chip, MHz: m})
		}
		err := writeChipTrims(ts)
		if m > 0 {
			planMu.Unlock()
		}
		if err != nil {
			writeJSON(w, map[string]any{"ok": false, "err": err.Error()})
			return note, true
		}
		cwHandEdit(in.Chain, in.Chip, m)
		if m == 0 {
			activity.emit(fmt.Sprintf("🔧 chip trim: board %d chip %d back to the board clock", in.Chain, in.Chip))
		} else {
			activity.emit(fmt.Sprintf("🔧 chip trim: board %d chip %d %+.4g MHz", in.Chain, in.Chip, m))
		}
		note = applyNote()
	}
	return note, false
}

// plusTrimErr: with chip trims on, a chip trimmed faster than its board ("+") would run past the clock window
// at board clock mhz. Every clock raise checks it: the "+" is only checked against the clock when it's set.
func plusTrimErr(mhz int) error {
	if mhz <= 0 {
		return nil
	}
	if _, err := os.Stat(chipHookFlag()); err != nil {
		return nil // the stock miner runs: no trim applies (switching them on checks it)
	}
	hi := chipTrimCeil()
	for _, t := range readChipTrims() {
		if t.MHz > 0 && float64(mhz)+t.MHz > float64(hi)+1e-9 {
			return fmt.Errorf("board %d chip %d is trimmed %+.4g MHz: at %d MHz it would run %.4g MHz, past the %d MHz clock window (lower its trim in Controls → Chip trims first)", t.Chain, t.Chip, t.MHz, mhz, float64(mhz)+t.MHz, hi)
		}
	}
	return nil
}

// capPlusTrims: the clock window got lower (the clock limits re-locked, or tightened to measured caps): a chip
// trimmed faster than its board is lowered until it fits at the board clock (to 0 when the clock can't be read),
// and the trims re-applied. "" when nothing changed. Lock order as handleChipTrim's.
func capPlusTrims() string {
	if _, err := os.Stat(chipHookFlag()); err != nil {
		return "" // the stock miner runs: switching trims on checks them
	}
	controlMu.Lock()
	defer controlMu.Unlock()
	chipJobMu.Lock()
	defer chipJobMu.Unlock()
	if sctRunning() {
		return "" // the strong-chip test checks its raised chips at every tick, and its end checks the trims it puts back
	}
	cwMu.Lock()
	defer cwMu.Unlock()
	chipTrimMu.Lock()
	defer chipTrimMu.Unlock()
	msg := capPlusTrimsLocked(0)
	if msg == "" {
		return ""
	}
	return msg + "; " + applyNote()
}

// capPlusTrimsLocked: capPlusTrims' check and write, with chipTrimMu held (it takes planMu). fallback: the board
// clock (MHz) to check against when the set clock can't be read (0: lower every "+" trim to 0 then). With chip trims
// off, nothing (switching them on checks them).
func capPlusTrimsLocked(fallback int) string {
	if _, err := os.Stat(chipHookFlag()); err != nil {
		return ""
	}
	ts := readChipTrims()
	plus := false
	for _, t := range ts {
		plus = plus || t.MHz > 0
	}
	if !plus {
		return "" // (no "+" trim: no need to read the clock)
	}
	planMu.Lock()
	set, ok := reapplySetClock()
	if !ok && fallback > 0 {
		set, ok = fallback, true
	}
	hi := float64(chipTrimCeil())
	var notes []string
	for i := range ts {
		if t := ts[i]; t.MHz > 0 && (!ok || float64(set)+t.MHz > hi+1e-9) {
			room := 0.0
			if ok && hi > float64(set) {
				room = math.Floor((hi-float64(set))/trimStep+1e-9) * trimStep
			}
			ts[i].MHz = math.Min(t.MHz, room)
			notes = append(notes, fmt.Sprintf("board %d chip %d %+.4g → %+.4g MHz", t.Chain, t.Chip, t.MHz, ts[i].MHz))
		}
	}
	if len(notes) == 0 {
		planMu.Unlock()
		return ""
	}
	err := writeChipTrims(ts)
	planMu.Unlock()
	if err != nil {
		return "couldn't lower the faster-than-board chip trims: " + err.Error()
	}
	msg := fmt.Sprintf("chip trims lowered to fit the %.0f MHz clock window: %s", hi, strings.Join(notes, ", "))
	activity.emit("🔧 " + msg)
	return msg
}

func applyNote() string {
	if _, err := os.Stat(chipHookFlag()); err != nil {
		return "saved; chip trims are off, so nothing is applied until you switch them on"
	}
	if s, err := reapplyTrims(); err == nil {
		return "applied (" + s + ")"
	} else {
		return "saved, but not applied live (" + err.Error() + "): press Re-apply in a minute, or Settings → Restart mining"
	}
}

func humanDur(d time.Duration) string {
	if d >= time.Hour && d%time.Hour == 0 {
		if d == time.Hour {
			return "1 hour"
		}
		return fmt.Sprintf("%d hours", int(d.Hours()))
	}
	return fmt.Sprintf("%d minutes", int(d.Minutes()))
}
