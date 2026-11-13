package main

// Round 7: fuzz targets for the chip-trim feature (Controls → Chip trims).
// Run one at a time: go test -run '^$' -fuzz '^FuzzR7ReadChipTrims$' -fuzztime 3m .
//
//   FuzzR7ReadChipTrims    the trims file: what the panel reads (readChipTrims) against what the hook applies
//                          (a line-for-line Go port of sclite_chip.c read_trims + its filter)
//   FuzzR7ChipTrimPost     POST /api/chiptrim: every file it leaves is one the hook reads exactly as the panel does
//   FuzzR7StrongTestPost   POST /api/strongtest: windows in range, no panic
//   FuzzR7ChipStats        the hook's stats file → chipTrimView / chipHistSample / /api/chipreport / sctStart
//   FuzzR7ChipWatchState   auto-trim's state file + stats + errors → a full judged window: no panic, every trim it
//                          writes is in range and it never raises a chip above its trim

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---- a port of the hook's parser (fw/kit/files/sclite_chip.c read_trims, parse_num and the apply filter) ----

func hookParseNum(b []byte, p int) (int, float64, bool) {
	sign, v, frac, any := 1.0, 0.0, 0.1, false
	for p < len(b) && (b[p] == ' ' || b[p] == '\t') {
		p++
	}
	if p < len(b) && b[p] == '-' {
		sign = -1
		p++
	} else if p < len(b) && b[p] == '+' {
		p++
	}
	for p < len(b) && b[p] >= '0' && b[p] <= '9' {
		v = v*10 + float64(b[p]-'0')
		p++
		any = true
	}
	if p < len(b) && b[p] == '.' {
		p++
		for p < len(b) && b[p] >= '0' && b[p] <= '9' {
			v += float64(b[p]-'0') * frac
			frac /= 10
			p++
			any = true
		}
	}
	return p, sign * v, any
}

type hookTrim struct {
	chain, chip int
	mhz         float64
}

// hookReadTrims: read_trims (16383 bytes read, 256 entries at most; a NUL ends the C string early)
func hookReadTrims(file []byte) []hookTrim {
	buf := file
	if len(buf) > 16383 {
		buf = buf[:16383]
	}
	end := func(q int) bool {
		return q == len(buf) || buf[q] == ' ' || buf[q] == '\t' || buf[q] == '\r' || buf[q] == '\n'
	}
	whole := func(v float64) bool { return v >= 0 && v <= 99 && v == float64(cTrunc(v)) }
	var out []hookTrim
	p := 0
	for p < len(buf) && len(out) < 256 {
		if buf[p] == '#' {
			for p < len(buf) && buf[p] != '\n' {
				p++
			}
		} else {
			var a, b, c float64
			var ok bool
			q := p
			if q, a, ok = hookParseNum(buf, q); ok && end(q) && whole(a) {
				if q, b, ok = hookParseNum(buf, q); ok && end(q) && whole(b) {
					if q, c, ok = hookParseNum(buf, q); ok && end(q) {
						out = append(out, hookTrim{cTrunc(a), cTrunc(b), c})
					}
				}
			}
			p = q
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

func cTrunc(f float64) int {
	if f > 1e9 || f < -1e9 {
		return -999999 // UB in C; never a real chain/chip either way
	}
	return int(f)
}

// hookWant: the offset the hook gives chip `chip` of chain `chain` (nchips 46), the last matching line winning.
func hookWant(ts []hookTrim, chain, chip int) float64 {
	w := 0.0
	for _, t := range ts {
		if t.chain != chain || t.chip != chip || t.chip < 1 || t.chip > 46 {
			continue
		}
		off := t.mhz
		x := off * 2 / 25
		if !(off >= -100 && off <= 50) || x != float64(cTrunc(x)) {
			continue
		}
		w = off
	}
	return w
}

// panelWant: what the panel shows for that chip (chipTrimView's map: the last of readChipTrims' entries)
func panelWant(ts []chipTrim, chain, chip int) float64 {
	w := 0.0
	for _, t := range ts {
		if t.Chain == chain && t.Chip == chip {
			w = t.MHz
		}
	}
	return w
}

func r7Dir(t *testing.T) {
	t.Helper()
	old, oldStats, oldLog := *authFile, chipStatsPath, chipHookLog
	d := t.TempDir()
	*authFile = filepath.Join(d, "auth.json")
	chipStatsPath = filepath.Join(d, "stats.json")
	chipHookLog = filepath.Join(d, "hook.log")
	t.Cleanup(func() { *authFile, chipStatsPath, chipHookLog = old, oldStats, oldLog })
}

func r7Diff(file []byte) string {
	if err := os.WriteFile(chipTrimPath(), file, 0o644); err != nil {
		return ""
	}
	ts := readChipTrims()
	hk := hookReadTrims(file)
	for _, t := range ts {
		if t.MHz < trimMin || t.MHz > trimMax || math.IsNaN(t.MHz) {
			return fmt.Sprintf("panel read an out-of-range trim %+v", t)
		}
	}
	for chain := 0; chain <= 3; chain++ {
		for chip := 1; chip <= 46; chip++ {
			if p, h := panelWant(ts, chain, chip), hookWant(hk, chain, chip); p != h {
				return fmt.Sprintf("chain %d chip %d: the panel shows %v MHz, the hook applies %v MHz", chain, chip, p, h)
			}
		}
	}
	return ""
}

var r7TrimSeeds = []string{
	"", "# x\n", "2 14 -12.5\n", "2 14 -12.5\n2 14 -25\n", "2 14 -12.5\r\n", "  2\t14\t-12.5  \n",
	"2 14 12.5-\n", "2 14 12.5.5\n", "2 14.0 -12.5\n", "2.7 14 -12.5\n", "2 14-12.5\n", "+2 +14 +12.5\n",
	"2 14 -12.50000000001\n", "2 14 1e1\n", "2 14 NaN\n", "2 14 -0\n", "2 14 62.5\n", "2 14 -112.5\n",
	"#2 14 -12.5\n", " #2 14 -12.5\n", "2 14 -12.5 # note\n", "2 14 -12.5#\n", "-0 1 12.5\n", "2 0 -12.5\n",
	"2 47 -12.5\n", "9 14 -12.5\n", "2 14 .5\n", "2 14 -.\n", "2 14 - 12.5\n", "2 14 ++12.5\n",
	"2 14 -12.5\x00junk\n", strings.Repeat("# padding padding padding padding padding padding\n", 90) + "3 46 -50\n",
}

func FuzzR7ReadChipTrims(f *testing.F) {
	for _, s := range r7TrimSeeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, file []byte) {
		r7Dir(t)
		if d := r7Diff(file); d != "" {
			t.Fatalf("%s\nfile: %q", d, file)
		}
	})
}

// ---- POST /api/chiptrim ----

func r7ClockStub(t *testing.T, mhz int) {
	old := reapplySetClock
	reapplySetClock = func() (int, bool) { return mhz, true }
	t.Cleanup(func() { reapplySetClock = old })
}

var r7PostSeeds = []string{
	`{}`, `null`, `{"chain":2,"chip":14,"mhz":-12.5}`, `{"chain":2,"chip":14,"mhz":12.500000001}`,
	`{"chain":2,"chip":14,"mhz":-99.99999999999}`, `{"chain":2,"chip":14,"mhz":50}`, `{"chain":2,"chip":14,"mhz":62.5}`,
	`{"chain":2,"chip":14,"mhz":1e308}`, `{"chain":2,"chip":14,"mhz":-1e-320}`, `{"chain":2,"chip":14,"mhz":"x"}`,
	`{"chain":-1,"chip":14,"mhz":-12.5}`, `{"chain":4,"chip":14,"mhz":-12.5}`, `{"chain":2,"chip":64,"mhz":-12.5}`,
	`{"chain":2,"chip":0,"mhz":-12.5}`, `{"chain":2.5,"chip":14,"mhz":-12.5}`, `{"reset":true}`, `{"apply":true}`,
	`{"watch":true}`, `{"watch":false}`, `{"watch_undo":true}`, `{"enable":false}`, `{"enable":true}`,
	`{"chain":2,"chip":14,"mhz":-12.5,"reset":true}`, `{"chain":2,"chip":14,"mhz":-12.5}{"reset":true}`,
	`{"chain":2,"chip":14,"mhz":-0}`, `{"chain":9223372036854775807,"chip":14,"mhz":-12.5}`,
}

func FuzzR7ChipTrimPost(f *testing.F) {
	for _, s := range r7PostSeeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		r7Dir(t)
		r7ClockStub(t, 600)
		writeChipTrims([]chipTrim{{0, 7, -25}, {2, 40, 12.5}})
		cwMu.Lock()
		cwSt, cwLoaded = cwState{}, false
		cwMu.Unlock()
		rr := httptest.NewRecorder()
		handleChipTrim(rr, httptest.NewRequest("POST", "/api/chiptrim", bytes.NewReader(body)))
		var m map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &m); err != nil {
			t.Fatalf("answer isn't JSON: %q", rr.Body.String())
		}
		file, _ := os.ReadFile(chipTrimPath())
		// every line the panel wrote is one the hook applies exactly as the panel shows it
		n := 0
		for _, l := range strings.Split(string(file), "\n") {
			if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
				n++
			}
		}
		if got := len(readChipTrims()); got != n {
			t.Fatalf("the panel wrote %d trim line(s) but reads back %d:\n%s", n, got, file)
		}
		if d := r7Diff(file); d != "" {
			t.Fatalf("body %q: %s\nfile:\n%s", body, d, file)
		}
		for _, tr := range readChipTrims() {
			if tr.Chain < 0 || tr.Chain > 3 || tr.Chip < 1 || tr.Chip > 63 || tr.MHz < trimMin || tr.MHz > trimMax {
				t.Fatalf("out-of-range trim saved: %+v (body %q)", tr, body)
			}
			if tr.MHz > 0 && 600+tr.MHz > float64(clockWindowHi())+1e-9 {
				t.Fatalf("+ trim past the clock window saved: %+v", tr)
			}
		}
	})
}

// ---- POST /api/strongtest ----

func FuzzR7StrongTestPost(f *testing.F) {
	for _, s := range []string{`{}`, `{"stop":true}`, `{"start":true}`, `{"start":true,"chain":2}`,
		`{"start":true,"chain":2,"baseline_min":4,"step_min":241}`, `{"start":true,"chain":2,"baseline_min":5,"step_min":240}`,
		`{"start":true,"chain":-1}`, `{"start":true,"chain":4}`, `{"start":true,"chain":2,"baseline_min":-1}`,
		`{"start":true,"chain":2,"baseline_min":153722867280912931}`, `{"start":true,"chain":2,"baseline_min":307445734561825870}`,
		`{"start":true,"chain":2,"baseline_min":1e3}`, `{"start":true,"stop":true,"chain":2}`, `{"start":"yes"}`} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		fk := sctSetup(t)
		r7ClockStub(t, 600)
		old := chipStatsPath
		chipStatsPath = filepath.Join(t.TempDir(), "s.json")
		t.Cleanup(func() { chipStatsPath = old })
		rr := httptest.NewRecorder()
		handleStrongTest(rr, httptest.NewRequest("POST", "/api/strongtest", bytes.NewReader(body)))
		var m map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &m); err != nil {
			t.Fatalf("answer isn't JSON: %q", rr.Body.String())
		}
		sct.mu.Lock()
		run, bw, sw := sct.running, sct.baseWin, sct.stepWin
		sct.mu.Unlock()
		if run {
			var in struct {
				BaseMin int `json:"baseline_min"`
				StepMin int `json:"step_min"`
			}
			json.Unmarshal(body, &in)
			for _, p := range []struct {
				asked int
				got   time.Duration
				def   time.Duration
			}{{in.BaseMin, bw, sctBaseline}, {in.StepMin, sw, sctStepWin}} {
				if p.got < sctMinWin || p.got > sctMaxWin {
					t.Fatalf("window %v out of range (body %q)", p.got, body)
				}
				if p.asked != 0 && time.Duration(p.asked)*time.Minute == p.got && (p.asked < 5 || p.asked > 240) {
					t.Fatalf("asked for %d minutes, got a %v window (overflow): body %q", p.asked, p.got, body)
				}
			}
		}
		_ = fk
	})
}

// ---- the hook's stats file ----

func FuzzR7ChipStats(f *testing.F) {
	for _, s := range []string{
		`{"t":1,"chains":[{"chain":2,"chips":3,"base10":6250,"clock10":[6250,6250,6125,6250,6250],"nonces":[0,100,50,150,0]}]}`,
		`{"t":1,"chains":[{"chain":2,"chips":46,"base10":6250,"clock10":[],"nonces":[]}]}`,
		`{"t":1,"chains":[{"chain":2,"chips":-5,"base10":-1,"clock10":[-1],"nonces":[-100,-100]}]}`,
		`{"t":1,"chains":[{"chain":2,"chips":2000000000,"base10":6250}]}`,
		`{"t":1,"chains":[{"chain":2,"chips":3,"nonces":[0,9223372036854775807,9223372036854775807,1]}]}`,
		`{"t":1,"chains":[{"chain":2,"chips":3},{"chain":2,"chips":3}]}`, `{"chains":null}`, `[]`, `x`, ``,
		`{"t":1,"chains":[{"chain":-7,"chips":1,"base10":6250,"clock10":[0,0],"nonces":[0,5]}]}`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		r7Dir(t)
		os.WriteFile(chipStatsPath, raw, 0o644)
		os.WriteFile(chipHookLog, []byte("1 <img src=x onerror=alert(1)>\n"), 0o644)
		done := make(chan struct{})
		go func() {
			defer close(done)
			v := chipTrimView()
			for _, c := range v["chains"].([]chainView) {
				if len(c.Chips) > 64 {
					t.Errorf("chain %d: %d chip rows from one stats file", c.Chain, len(c.Chips))
				}
				for _, r := range c.Chips {
					if math.IsNaN(r.Share) || math.IsInf(r.Share, 0) {
						t.Errorf("share %v", r.Share)
					}
				}
			}
			if _, err := json.Marshal(v); err != nil {
				t.Errorf("view doesn't marshal: %v", err)
			}
			chipHistSample()
			rr := httptest.NewRecorder()
			handleChipReport(rr, httptest.NewRequest("GET", "/api/chipreport", nil))
			if !json.Valid(rr.Body.Bytes()) {
				t.Errorf("report isn't JSON")
			}
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("view hung on %q", raw)
		}
	})
}

// ---- auto-trim's state file ----

func FuzzR7ChipWatchState(f *testing.F) {
	for _, s := range []string{
		`{"on":true}`, `{"on":true,"owned":null,"nohelp":null}`,
		`{"on":true,"budget":{"2":1}}`, `{"on":true,"board_rate":{"2":1}}`,
		`{"on":true,"owned":[{"chain":2,"chip":5,"mhz":-12.5,"before":100,"prev":50}]}`,
		`{"on":true,"owned":[{"chain":2,"chip":5,"mhz":-12.5,"before":100,"prev":-200}]}`,
		`{"on":true,"owned":[{"chain":2,"chip":5,"mhz":0,"before":1e308,"prev":12.5}]}`,
		`{"on":true,"owned":[{"chain":2,"chip":5,"mhz":-12.5,"before":-1,"prev":0},{"chain":2,"chip":5,"mhz":-12.5}]}`,
		`{"on":true,"nohelp":{"2.9":9999999999}}`, `{"on":true,"log":[1,2,3]}`, `x`,
	} {
		f.Add([]byte(s), []byte{9: 40, 10: 200})
	}
	f.Fuzz(func(t *testing.T, state, errb []byte) {
		r7Dir(t)
		writeChipTrims([]chipTrim{{2, 5, -12.5}, {2, 9, 25}, {2, 20, -50}})
		before := map[[2]int]float64{}
		for _, tr := range readChipTrims() {
			before[[2]int{tr.Chain, tr.Chip}] = tr.MHz
		}
		os.WriteFile(chipWatchPath(), state, 0o644)
		now := time.Unix(1_800_000_000, 0)
		var st chipStatsFile
		json.Unmarshal([]byte(`{"t":1,"chains":[{"chain":2,"chips":46,"base10":6000}]}`), &st)
		errs0 := map[ckey]int{}
		for i := 1; i <= 46; i++ {
			errs0[ckey{2, i}] = 0
		}
		errs1 := map[ckey]int{}
		for i := 1; i <= 46; i++ {
			v := 0
			if i-1 < len(errb) {
				v = int(errb[i-1])
				if len(errb) > 46 && errb[46]&1 == 1 {
					v *= 1000
				}
			}
			errs1[ckey{2, i}] = v
		}
		cur := errs0
		o1, o2, o3, o4, o5 := cwReadErrors, cwReadStats, cwApply, cwNow, cwBlocked
		t.Cleanup(func() {
			cwReadErrors, cwReadStats, cwApply, cwNow, cwBlocked = o1, o2, o3, o4, o5
			cwMu.Lock()
			cwSt, cwLoaded = cwState{}, false
			cwResetWindow()
			cwMu.Unlock()
		})
		cwReadErrors = func() (map[ckey]int, error) { return cur, nil }
		cwReadStats = func() (chipStatsFile, bool) { return st, true }
		cwApply = func() error { return nil }
		cwNow = func() time.Time { return now }
		cwBlocked = func() string { return "" }
		cwMu.Lock()
		cwSt, cwLoaded = cwState{}, false
		cwResetWindow()
		cwMu.Unlock()
		var perr any
		func() {
			defer func() { perr = recover() }()
			chipWatchTick() // starts the window
			now = now.Add(cwWindow + time.Minute)
			cur = errs1
			chipWatchTick() // judges it
			chipWatchView()
		}()
		if perr != nil {
			t.Fatalf("auto-trim panicked: %v\nstate: %s", perr, state)
		}
		file, _ := os.ReadFile(chipTrimPath())
		n := 0
		for _, l := range strings.Split(string(file), "\n") {
			if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
				n++
			}
		}
		after := readChipTrims()
		if len(after) != n {
			t.Fatalf("auto-trim wrote a trim line the panel and hook drop:\n%s\nstate: %s", file, state)
		}
		for _, tr := range after {
			k := [2]int{tr.Chain, tr.Chip}
			if tr.MHz > 0 && tr.MHz > before[k] {
				t.Fatalf("auto-trim raised chip %v from %v to %+v MHz (state %s)", k, before[k], tr.MHz, state)
			}
			if tr.MHz < cwFloor-1e-9 && tr.MHz != before[k] {
				t.Fatalf("auto-trim slowed chip %v to %v, past its floor (state %s)", k, tr.MHz, state)
			}
		}
	})
}
