package main

// Round-2 hook sweep: the agent's trim limits against the hook's (sclite_chip.c: clamps to -100..+50, reads
// "[+-]digits[.digits]" and nothing else, no 12.5 snapping). What the hook does with each line was measured with
// the real library (r2-hook/native/h2 section G, and qemu).

import (
	"bytes"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func r2Setup(t *testing.T) {
	old := *authFile
	*authFile = filepath.Join(t.TempDir(), "auth.json")
	t.Cleanup(func() { *authFile = old })
}

// the hook's clamp, applied to what it parses (see parse_num): returns the offset the chip really gets, ok=false
// when the hook skips the line
func r2HookOffset(line string) (float64, bool) {
	// mirror of parse_num for the third number only (the test lines are "0 5 <x>")
	s := line[4:]
	sign, v, frac, any := 1.0, 0.0, 0.1, false
	i := 0
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	if i < len(s) && s[i] == '-' {
		sign = -1
		i++
	} else if i < len(s) && s[i] == '+' {
		i++
	}
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		v = v*10 + float64(s[i]-'0')
		i++
		any = true
	}
	if i < len(s) && s[i] == '.' {
		i++
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			v += float64(s[i]-'0') * frac
			frac /= 10
			i++
			any = true
		}
	}
	if !any {
		return 0, false
	}
	return math.Max(-100, math.Min(50, sign*v)), true
}

func TestR2TrimFileAgentVsHook(t *testing.T) {
	r2Setup(t)
	cases := []string{"+50", "50", "+62.5", "-100", "-112.5", "0", "NaN", "inf", "7", "+6.25", "-1e2", "+5e1", "50.0000001", ".5", "+12.5", "-12.5"}
	mismatch := 0
	for _, c := range cases {
		line := "0 5 " + c
		os.WriteFile(chipTrimPath(), []byte(line+"\n"), 0o644)
		ts := readChipTrims()
		agent := 0.0
		if len(ts) == 1 {
			agent = ts[0].MHz
		}
		hook, ok := r2HookOffset(line)
		if !ok {
			hook = 0
		}
		same := agent == hook
		if !same {
			mismatch++
		}
		t.Logf("%-12q agent shows %-6g hook runs the chip at %+g%s", c, agent, hook, map[bool]string{true: "", false: "   <-- differ"}[same])
	}
	t.Logf("%d of %d hand-edited values read differently by the panel and the hook", mismatch, len(cases))
	// the exact edges must agree
	for _, c := range []string{"+50", "-100", "0", "+12.5", "-12.5"} {
		os.WriteFile(chipTrimPath(), []byte("0 5 "+c+"\n"), 0o644)
		ts := readChipTrims()
		h, _ := r2HookOffset("0 5 " + c)
		a := 0.0
		if len(ts) == 1 {
			a = ts[0].MHz
		}
		if a != h {
			t.Errorf("edge %s: agent %g hook %g", c, a, h)
		}
	}
}

func TestR2HandlerLimits(t *testing.T) {
	r2Setup(t)
	old := chipStatsPath
	chipStatsPath = filepath.Join(t.TempDir(), "none.json")
	defer func() { chipStatsPath = old }()
	oldClk := reapplySetClock // a "+" trim needs the board clock (refused when it can't be read)
	reapplySetClock = func() (int, bool) { return 600, true }
	defer func() { reapplySetClock = oldClk }()
	for _, tc := range []struct {
		body string
		ok   bool
	}{
		{`{"chain":0,"chip":5,"mhz":50}`, true},
		{`{"chain":0,"chip":5,"mhz":62.5}`, false},
		{`{"chain":0,"chip":5,"mhz":50.0000001}`, false},
		{`{"chain":0,"chip":5,"mhz":-100}`, true},
		{`{"chain":0,"chip":5,"mhz":-112.5}`, false},
		{`{"chain":0,"chip":5,"mhz":0}`, true},
		{`{"chain":0,"chip":5,"mhz":6.25}`, false},
		{`{"chain":0,"chip":5,"mhz":37.5}`, true},
		{`{"chain":0,"chip":5,"mhz":"NaN"}`, false}, // JSON has no NaN: the decode fails, MHz stays nil, nothing changes
		{`{"chain":0,"chip":5,"mhz":1e400}`, false},
	} {
		os.Remove(chipTrimPath())
		rec := httptest.NewRecorder()
		handleChipTrim(rec, httptest.NewRequest(http.MethodPost, "/api/chiptrim", bytes.NewBufferString(tc.body)))
		var out map[string]any
		json.Unmarshal(rec.Body.Bytes(), &out)
		b, _ := os.ReadFile(chipTrimPath())
		wrote := len(readChipTrims()) > 0 || bytes.Contains(b, []byte("0 5"))
		ok, _ := out["ok"].(bool)
		t.Logf("%-40s ok=%v err=%v file=%q", tc.body, ok, out["err"], string(b))
		if tc.ok && !ok {
			t.Errorf("%s refused", tc.body)
		}
		if !tc.ok && wrote {
			t.Errorf("%s wrote the trims file: %q", tc.body, b)
		}
	}
	// NaN can't come through JSON, but chipTrimChange itself lets it through (all comparisons with NaN are false)
	nan := math.NaN()
	rec := httptest.NewRecorder()
	os.Remove(chipTrimPath())
	chipTrimChange(rec, nil, 0, 5, &nan, false, false)
	b, _ := os.ReadFile(chipTrimPath())
	t.Logf("chipTrimChange(NaN) direct: response %q, file %q", rec.Body.String(), b)
}

// reapplyTrims' guard: a board within +-12.4 MHz of the set clock is re-sent as is. The miner ramps a board in
// 10 MHz steps (0x1c5b80), updating the board-clock field each step, and the hook's stats thread can report it
// mid-ramp: a report of 590 with the set clock 600 passes the guard, and 590 is sent as the board's clock.
func TestR2ReapplyGuardMidRamp(t *testing.T) {
	old := reapplySetClock
	reapplySetClock = func() (int, bool) { return 600, true }
	defer func() { reapplySetClock = old }()
	oldStats := chipStatsPath
	chipStatsPath = filepath.Join(t.TempDir(), "chipstats.json")
	defer func() { chipStatsPath = oldStats }()
	for _, b10 := range []int{6000, 5900, 5875, 5876, 6124, 6125, 6100} {
		got, done := fakePgaAPI(t, []int{0})
		os.WriteFile(chipStatsPath, []byte(`{"t":1,"chains":[{"chain":0,"chips":46,"base10":`+hkItoa(b10)+`}]}`), 0o644)
		_, err := reapplyTrims()
		t.Logf("set 600, report base10 %d: err=%v sent=%v", b10, err, *got)
		if b10 != 6000 && err == nil && len(*got) > 0 && (*got)[0] != "0,clock,600" {
			t.Logf("  -> re-sent a clock that isn't the set one: %v", *got)
		}
		done()
	}
}
