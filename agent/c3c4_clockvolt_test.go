package main

// Lanes C3 (ClockControl) and C4 (VoltageControl) on the SC Box II: refused when off, accepted inside the
// window/V rule when on, refused (never clamped) outside it, every clock/V write confirmed from the miner
// API (devs) and reverted to a stock plan on a mismatch or no answer. The fake stock stack below plays
// minerd (PUT /mcb/setting → the plan it would forward as procset vfff) and the miner API's devs reply
// (the driver's "clock"/"voltage" after the vfff write).

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type c34Fake struct {
	mu      sync.Mutex
	setting map[string]any
	puts    []map[string]any
	clock   float64 // what devs reports
	volt    float64
	ignore  bool    // the driver doesn't take the write (devs keeps the old values)
	voltOff float64 // the driver reports V off by this much
	apiDown bool    // the miner API doesn't answer
	srv     *httptest.Server
}

func newC34(t *testing.T, clock, volt bool) *c34Fake {
	t.Helper()
	useBox2(t)
	model.Feat.ClockControl, model.Feat.VoltageControl = clock, volt
	f := &c34Fake{clock: 690, volt: 42}
	json.Unmarshal([]byte(box2Setting), &f.setting)
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/user/login"):
			json.NewEncoder(w).Encode(map[string]any{"JWT Token": "tok"})
		case r.Method == "GET":
			f.mu.Lock()
			json.NewEncoder(w).Encode(f.setting)
			f.mu.Unlock()
		case r.Method == "PUT":
			var in map[string]any
			b, _ := io.ReadAll(r.Body)
			json.Unmarshal(b, &in)
			f.mu.Lock()
			f.puts = append(f.puts, in)
			f.setting = in
			f.forward(in)
			f.mu.Unlock()
			io.WriteString(w, `{"status":0}`)
		}
	}))
	*minerWeb = strings.TrimPrefix(f.srv.URL, "http://")
	*authFile = filepath.Join(t.TempDir(), "auth.json")
	*presetPath = filepath.Join(t.TempDir(), "presets.json")
	schedPath = filepath.Join(t.TempDir(), "schedule.json")
	jwtToken = ""
	authMu.Lock()
	auth.MinerPass = "pw"
	authMu.Unlock()
	setCharge("")
	oldDevs, oldWait, oldPoll := box2DevsCmd, box2ConfirmWait, box2ConfirmPoll
	box2DevsCmd = f.devs
	box2ConfirmWait, box2ConfirmPoll = 300*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() {
		box2AsyncConfirms.Wait()
		box2DevsCmd, box2ConfirmWait, box2ConfirmPoll = oldDevs, oldWait, oldPoll
		f.srv.Close()
		schedPath = ""
		setCharge("")
	})
	return f
}

// forward: what minerd + the driver do with a setting write (caller holds mu).
func (f *c34Fake) forward(in map[string]any) {
	if f.ignore {
		return
	}
	var info string
	if m, _ := in["manual"].(bool); m {
		info, _ = in["manualPowerplan"].(string)
	} else if arr, ok := in["powerplans"].([]any); ok {
		i := 0
		if s, ok := in["select"].(float64); ok {
			i = int(s)
		}
		if i >= 0 && i < len(arr) {
			pm, _ := arr[i].(map[string]any)
			info, _ = pm["info"].(string)
		}
	}
	m := box2PlanRe.FindStringSubmatch(info)
	if m == nil {
		return
	}
	fmt.Sscan(m[1], &f.clock)
	fmt.Sscan(m[2], &f.volt)
	f.volt += f.voltOff
}

func (f *c34Fake) devs() ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.apiDown {
		return nil, fmt.Errorf("connection refused")
	}
	return []byte(fmt.Sprintf(`{"STATUS":[{"STATUS":"S","Msg":"1 PGA(s)"}],"DEVS":[{"PGA":0,"ID":0,"Enabled":"Y","Status":"Alive","voltage":%.3f,"clock":%.3f}],"id":1}`, f.volt, f.clock)), nil
}

func (f *c34Fake) putPlans() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, p := range f.puts {
		s, _ := p["manualPowerplan"].(string)
		out = append(out, s)
	}
	return out
}

func (f *c34Fake) set(fn func()) { f.mu.Lock(); fn(); f.mu.Unlock() }

func c34Post(t *testing.T, body string) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	handleControl(rec, httptest.NewRequest("POST", "/api/control", strings.NewReader(body)))
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return out
}

// ---- the rule itself ----

func TestC34RuleAndWindow(t *testing.T) {
	useBox2(t)
	if lo, hi := box2VRange(); lo != 42 || hi != 61 {
		t.Fatalf("V range = %d..%d, want 42..61", lo, hi)
	}
	for mhz, want := range map[int]int{525: 61, 526: 42, 600: 42, 690: 42} {
		if v, ok := box2VFor(mhz); !ok || v != want {
			t.Fatalf("box2VFor(%d) = %d,%v want %d", mhz, v, ok, want)
		}
	}
	if _, ok := box2VFor(691); ok {
		t.Fatalf("no stock plan runs above 690: box2VFor(691) must fail")
	}
	for in, want := range map[float64]float64{690: 687.5, 600: 600, 610: 600, 525: 525, 500: 500, 510: 500} {
		if got := box2PLLClock(in); got != want {
			t.Fatalf("PLL(%v) = %v want %v (the driver's own log: req 690 → 687.5, 610 → 600)", in, got, want)
		}
	}
}

// ---- OFF: nothing changes ----

func TestC34OffRefusesEverything(t *testing.T) {
	f := newC34(t, false, false)
	if err := box2CheckTuple(600, 42); err == nil || !strings.Contains(err.Error(), "is not one of the stock plans") {
		t.Fatalf("off: 600/42 must be refused as before, got %v", err)
	}
	if err := box2CheckTuple(690, 42); err != nil {
		t.Fatalf("a stock tuple is always fine: %v", err)
	}
	out := c34Post(t, `{"freq_mhz":600,"confirm":true}`)
	if out["ok"] == true || out["disabled"] != true {
		t.Fatalf("off: Controls POST must be refused by the gate, got %v", out)
	}
	if _, err := mutatePlan(func(p *powerplan) { p.FreqMHz = 600 }); err == nil {
		t.Fatalf("off: a non-stock clock through mutatePlan must be refused")
	}
	if _, err := mutatePlan(func(p *powerplan) { p.FreqMHz, p.VrunMv = 690, 50 }); err == nil {
		t.Fatalf("off: a non-stock V must be refused")
	}
	savePresets([]preset{{Name: "Eco", MHz: 600, MV: 42}})
	if n := len(presetsList()); n != 2 {
		t.Fatalf("off: presets must be the 2 stock modes only, got %d", n)
	}
	if presetExists("Eco") || applyPresetName("Eco") == nil {
		t.Fatalf("off: a saved clock preset must not exist/apply")
	}
	if p := f.putPlans(); len(p) != 0 {
		t.Fatalf("off: nothing may be written, got %v", p)
	}
}

// ---- ON (clock only) ----

func TestC34ClockOnAcceptsInWindowKeepsRuleV(t *testing.T) {
	f := newC34(t, true, false)
	out := c34Post(t, `{"freq_mhz":600,"confirm":true}`)
	if out["ok"] != true {
		t.Fatalf("600 MHz must be accepted with ClockControl on: %v", out)
	}
	if got := f.putPlans(); len(got) != 1 || !strings.HasPrefix(got[0], "600 MHz 42 V ") {
		t.Fatalf("want one write of 600 MHz at the 690 plan's code 42, got %v", got)
	}
	// a V-less request at 525 is the Low-power stock tuple (its own text, V 61)
	out = c34Post(t, `{"freq_mhz":525,"confirm":true}`)
	if out["ok"] != true {
		t.Fatalf("525: %v", out)
	}
	if got := f.putPlans(); !strings.HasPrefix(got[len(got)-1], "525 MHz 61 V ") {
		t.Fatalf("525 must keep the Low-power code 61, got %v", got)
	}
	// 525 at 42 is a stock plan's code at a faster clock: allowed
	if out = c34Post(t, `{"freq_mhz":525,"vrun_mv":42,"confirm":true}`); out["ok"] != true {
		t.Fatalf("525/42 (690 plan's code at a lower clock) must be allowed: %v", out)
	}
	// the throttle-style relative step keeps V and is allowed
	if _, err := mutatePlan(func(p *powerplan) { p.FreqMHz = 550 }); err != nil {
		t.Fatalf("550 at V42: %v", err)
	}
	if f.clock != 550 || f.volt != 42 {
		t.Fatalf("devs shows %v/%v", f.clock, f.volt)
	}
}

func TestC34ClockOnRefusesOutOfRange(t *testing.T) {
	f := newC34(t, true, false)
	for _, body := range []string{
		`{"freq_mhz":700,"confirm":true}`,                 // above the window (overclock is lane C7)
		`{"freq_mhz":520,"confirm":true}`,                 // below it
		`{"freq_mhz":600,"vrun_mv":61,"confirm":true}`,    // V other than the rule's while VoltageControl is off
		`{"freq_mhz":600,"vrun_mv":50,"confirm":true}`,    // ditto
		`{"freq_mhz":690,"override":true,"confirm":true}`, // overclock path refused on this model here
	} {
		if out := c34Post(t, body); out["ok"] == true {
			t.Fatalf("%s must be refused, got %v", body, out)
		}
	}
	if got := f.putPlans(); len(got) != 0 {
		t.Fatalf("refusals must write nothing, got %v", got)
	}
}

// The latent hazard this lane closes: with the gate on, a V-less request used to reach the SC Lite window
// check (Box II's mV window is 0–0) and would have been written as "0 V" (fan2 PWM 100).
func TestC34NeverWritesVZero(t *testing.T) {
	f := newC34(t, true, true)
	out := c34Post(t, `{"freq_mhz":690,"vrun_mv":0,"pv_mv":0,"confirm":true}`)
	if out["ok"] != true {
		t.Fatalf("690 with no V must take the rule's code: %v", out)
	}
	for _, p := range f.putPlans() {
		if strings.Contains(p, " 0 V ") {
			t.Fatalf("a 0 V plan was written: %q", p)
		}
	}
}

// ---- ON (clock + voltage) ----

func TestC34VoltageOnAcceptsCodesInRange(t *testing.T) {
	f := newC34(t, true, true)
	if out := c34Post(t, `{"freq_mhz":600,"vrun_mv":50,"confirm":true}`); out["ok"] != true {
		t.Fatalf("600/50 with VoltageControl on: %v", out)
	}
	if got := f.putPlans(); !strings.HasPrefix(got[len(got)-1], "600 MHz 50 V ") {
		t.Fatalf("got %v", got)
	}
	for _, v := range []int{41, 62, 100, -1} {
		if out := c34Post(t, fmt.Sprintf(`{"freq_mhz":600,"vrun_mv":%d,"confirm":true}`, v)); out["ok"] == true {
			t.Fatalf("V %d must be refused (allowed 42..61)", v)
		}
	}
	if n := len(f.putPlans()); n != 1 {
		t.Fatalf("refusals wrote: %v", f.putPlans())
	}
}

func TestC34VoltageNeedsClock(t *testing.T) {
	newC34(t, false, true) // commission.go enforces the dependency; even if forced, nothing opens
	if err := box2CheckTuple(690, 50); err == nil {
		t.Fatalf("VoltageControl without ClockControl must not open anything")
	}
}

// ---- read-back + revert ----

func TestC34MismatchReverts(t *testing.T) {
	f := newC34(t, true, true)
	f.set(func() { f.ignore = true }) // the driver never takes it: devs keeps 690/42
	out := c34Post(t, `{"freq_mhz":600,"vrun_mv":50,"confirm":true}`)
	if out["ok"] == true || !strings.Contains(fmt.Sprint(out["err"]), "put back the stock plan") {
		t.Fatalf("an unconfirmed write must fail and revert: %v", out)
	}
	got := f.putPlans()
	if len(got) != 2 || !strings.HasPrefix(got[1], "690 MHz 42 V ") {
		t.Fatalf("want the write then the stock 690/42 put back, got %v", got)
	}
}

func TestC34WrongVoltageReadBackReverts(t *testing.T) {
	f := newC34(t, true, true)
	f.set(func() { f.voltOff = 1 }) // the driver reports a different V than written
	if out := c34Post(t, `{"freq_mhz":600,"vrun_mv":50,"confirm":true}`); out["ok"] == true {
		t.Fatalf("a V read-back mismatch must fail: %v", out)
	}
	got := f.putPlans()
	if !strings.HasPrefix(got[len(got)-1], "690 MHz 42 V ") {
		t.Fatalf("not reverted to the stock plan: %v", got)
	}
}

func TestC34NoAPIAnswerReverts(t *testing.T) {
	f := newC34(t, true, false)
	f.set(func() { f.apiDown = true })
	if out := c34Post(t, `{"freq_mhz":600,"confirm":true}`); out["ok"] == true {
		t.Fatalf("no read-back = not confirmed: %v", out)
	}
	if got := f.putPlans(); len(got) != 2 || !strings.HasPrefix(got[1], "690 MHz 42 V ") {
		t.Fatalf("want revert to 690/42, got %v", got)
	}
}

// a custom plan running before a failed write: the revert goes to the Low-power stock plan (the lowest stock
// clock), never to an unconfirmed custom tuple.
func TestC34RevertFromCustomGoesToLowPower(t *testing.T) {
	f := newC34(t, true, true)
	if out := c34Post(t, `{"freq_mhz":600,"vrun_mv":50,"confirm":true}`); out["ok"] != true {
		t.Fatalf("setup: %v", out)
	}
	f.set(func() { f.ignore = true })
	c34Post(t, `{"freq_mhz":650,"vrun_mv":45,"confirm":true}`)
	got := f.putPlans()
	if !strings.HasPrefix(got[len(got)-1], "525 MHz 61 V ") {
		t.Fatalf("revert from a custom plan must go to the Low-power stock plan, got %v", got)
	}
}

// the fan loop's writer (thermal throttle, wake=false) is not held up: confirmed in the background, reverted
// there on a mismatch.
func TestC34AsyncConfirmReverts(t *testing.T) {
	f := newC34(t, true, false)
	f.set(func() { f.ignore = true })
	if _, err := mutatePlanNoWake(func(p *powerplan) { p.FreqMHz = 665 }); err != nil {
		t.Fatalf("throttle-style step: %v", err)
	}
	box2AsyncConfirms.Wait()
	got := f.putPlans()
	if len(got) != 2 || !strings.HasPrefix(got[0], "665 MHz 42 V ") || !strings.HasPrefix(got[1], "690 MHz 42 V ") {
		t.Fatalf("want 665/42 then the 690/42 revert, got %v", got)
	}
}

// ---- presets / schedule ----

func TestC34PresetsWithClockControl(t *testing.T) {
	f := newC34(t, true, false)
	post := func(body string) map[string]any {
		rec := httptest.NewRecorder()
		handlePresets(rec, httptest.NewRequest("POST", "/api/presets", strings.NewReader(body)))
		var out map[string]any
		json.Unmarshal(rec.Body.Bytes(), &out)
		return out
	}
	if out := post(`{"action":"save","name":"Eco","mhz":600}`); out["ok"] != true {
		t.Fatalf("save 600 MHz preset: %v", out)
	}
	for _, body := range []string{
		`{"action":"save","name":"Hot","mhz":700}`,
		`{"action":"save","name":"Odd","mhz":600,"mv":61}`,
		`{"action":"save","name":"Hashrate","mhz":600}`,
	} {
		if out := post(body); out["ok"] == true {
			t.Fatalf("%s must be refused", body)
		}
	}
	ps := presetsList()
	if len(ps) != 3 || ps[2].Name != "Eco" || ps[2].MV != 42 || ps[2].PV != 0 {
		t.Fatalf("want the 2 stock modes + Eco (600 MHz, V42), got %+v", ps)
	}
	if !presetExists("Eco") {
		t.Fatalf("the schedule must accept the saved preset")
	}
	if err := applyPresetName("Eco"); err != nil {
		t.Fatalf("apply Eco: %v", err)
	}
	if got := f.putPlans(); !strings.HasPrefix(got[len(got)-1], "600 MHz 42 V ") {
		t.Fatalf("got %v", got)
	}
	// a stock mode by name is still the select-by-index switch (manual:false)
	if err := applyPresetName("Low-power"); err != nil {
		t.Fatalf("Low-power: %v", err)
	}
	f.mu.Lock()
	last := f.puts[len(f.puts)-1]
	f.mu.Unlock()
	if m, _ := last["manual"].(bool); m {
		t.Fatalf("a stock mode must still be selected by index (manual:false): %v", last)
	}
}

// a preset saved with a V code while VoltageControl was on is not runnable once it is off (refused, not
// re-coded).
func TestC34PresetVCodeNeedsVoltageControl(t *testing.T) {
	newC34(t, true, false)
	savePresets([]preset{{Name: "V50", MHz: 600, MV: 50}})
	if presetExists("V50") {
		t.Fatalf("a V50 preset must not be runnable with VoltageControl off")
	}
	if err := applyPresetName("V50"); err == nil {
		t.Fatalf("applying it must be refused")
	}
}

func TestC34ControlGETInfo(t *testing.T) {
	newC34(t, true, false)
	rec := httptest.NewRecorder()
	handleControl(rec, httptest.NewRequest("GET", "/api/control", nil))
	var out struct {
		Box2 struct {
			Clock          []int `json:"clock"`
			VCodes         []int `json:"v_codes"`
			ClockControl   bool  `json:"clock_control"`
			VoltageControl bool  `json:"voltage_control"`
		} `json:"box2"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	if !out.Box2.ClockControl || out.Box2.VoltageControl || fmt.Sprint(out.Box2.Clock) != "[525 690]" || fmt.Sprint(out.Box2.VCodes) != "[42 61]" {
		t.Fatalf("GET box2 info: %s", rec.Body.String())
	}
}
