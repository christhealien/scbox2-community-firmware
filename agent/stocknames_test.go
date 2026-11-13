package main

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// Box II: the OEM's two stock modes read "Stock High" (690 MHz; also the factory "Stock") and "Stock Low" (525 MHz)
// in every Go text a person sees; the keys ("Hashrate", "Low-power", "Stock", "preset:Hashrate") stay as they were.
// The SC Lite's texts are unchanged.

func TestStockNamesBox2Labels(t *testing.T) {
	newB11Box2(t)
	for in, want := range map[string]string{"Hashrate": "Stock High", "Low-power": "Stock Low", "Stock": "Stock High", "stock": "Stock High", "Quiet": "Quiet", "Idle": "Idle"} {
		if got := presetLabel(in); got != want {
			t.Errorf("presetLabel(%q) = %q, want %q", in, got, want)
		}
	}
	if stockWord() != "Stock High" {
		t.Errorf("stockWord on Box II = %q", stockWord())
	}
	for c, want := range map[string]string{"preset:Hashrate": "preset Stock High", "preset:Low-power": "preset Stock Low", "stock": "Stock High", "idle": "idle mode", "": ""} {
		setCharge(c)
		if got := chargeText(); got != want {
			t.Errorf("chargeText (charge %q) = %q, want %q", c, got, want)
		}
	}
	setCharge("preset:Hashrate")
	if chargeLabel() != "preset Hashrate" { // the in_charge value the page compares: unchanged
		t.Errorf("chargeLabel changed: %q", chargeLabel())
	}
	setCharge("")
	// a saved clock preset can't take a stock mode's shown name either
	if !isBox2StockName("Stock High") || !isBox2StockName("stock low") || !isBox2StockName("Hashrate") || isBox2StockName("Stock") {
		t.Error("isBox2StockName must reserve the stock modes' keys and names")
	}
}

func TestStockNamesBox2ScheduleAndPresets(t *testing.T) {
	m := newB11Box2(t)
	// the brushes: keys unchanged, labels Stock High / Stock Low
	got := map[string]string{}
	for _, b := range schedulePresets() {
		got[b["key"].(string)] = b["label"].(string)
	}
	if got["Hashrate"] != "Stock High" || got["Low-power"] != "Stock Low" || got["Idle"] != "Idle" || len(got) != 3 {
		t.Fatalf("Box II brushes: %v", got)
	}
	// the schedule's "now" line names them too; the stored rules keep the keys
	s := schedule{Enabled: true, Rules: []schedRule{{Start: "00:00", Days: l11All, Preset: "Low-power"}}, Base: schedBack}
	v := scheduleView(s)
	if v["now"] != "Stock Low" {
		t.Errorf("schedule now = %v, want Stock Low", v["now"])
	}
	if s.Rules[0].Preset != "Low-power" {
		t.Errorf("a rule's stored preset changed: %v", s.Rules[0].Preset)
	}
	// the Presets tab's apply: the key goes in, the name comes back; the charge keeps the key
	rec := httptest.NewRecorder()
	handlePresets(rec, httptest.NewRequest("POST", "/api/presets", strings.NewReader(`{"action":"apply","name":"Low-power"}`)))
	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	if out["ok"] != true || out["note"] != "applied Stock Low" {
		t.Fatalf("apply Low-power: %s", rec.Body.String())
	}
	if getCharge() != "preset:Low-power" {
		t.Errorf("charge = %q, want preset:Low-power", getCharge())
	}
	if p := m.lastPut(); p == nil || p["select"] != float64(1) {
		t.Errorf("Low-power must select index 1: %v", p)
	}
	// Stock is still the factory default (box2DefaultIndex: Hashrate's plan), whatever mode was picked last
	if err := applyPresetNameGen("Stock", noGen); err != nil {
		t.Fatal(err)
	}
	if p := m.lastPut(); p == nil || p["select"] != float64(0) || getCharge() != "stock" {
		t.Errorf("Stock must select the factory plan (index 0) with charge stock: %v %q", p, getCharge())
	}
	if !strings.Contains(heatCoolLine(), "apply Stock High or a preset") {
		t.Errorf("heat line: %q", heatCoolLine())
	}
	// a name that isn't a stock mode: refused, naming the stock modes as a person sees them
	if err := applyBox2ModeGen("High", noGen); err == nil || !strings.Contains(err.Error(), "(Stock High / Stock Low)") {
		t.Errorf("refusal: %v", err)
	}
}

func TestStockNamesSCLiteUnchanged(t *testing.T) {
	if model.Plan == planBox2V {
		t.Skip("SC Lite profile only")
	}
	for _, n := range []string{"Hashrate", "Low-power", "Stock", "High"} {
		if presetLabel(n) != n {
			t.Errorf("SC Lite presetLabel(%q) = %q", n, presetLabel(n))
		}
	}
	if stockWord() != "Stock" || !strings.Contains(heatCoolLine(), "apply Stock or a preset") {
		t.Errorf("SC Lite stock word: %q / %q", stockWord(), heatCoolLine())
	}
	defer setCharge("")
	for _, c := range []string{"preset:Hashrate", "stock", "idle", "tuner"} {
		setCharge(c)
		if chargeText() != chargeLabel() {
			t.Errorf("SC Lite chargeText(%q) = %q, chargeLabel %q", c, chargeText(), chargeLabel())
		}
	}
}
