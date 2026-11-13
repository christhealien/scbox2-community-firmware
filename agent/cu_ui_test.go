package main

// CU-ui: the Settings → Commissioning card. The served page carries the card (hidden until /api/commission says
// there is something to commission, never fetched on the SC Lite) and its hooks; /api/model and /api/dashboard
// carry the running commissioned set on a model that has one (so an open page reloads after the panel restarted
// with other switches), and nothing new on the SC Lite.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func servedPage(t *testing.T) string {
	t.Helper()
	rec := httptest.NewRecorder()
	handleIndex(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("index status = %d", rec.Code)
	}
	return rec.Body.String()
}

// modelReply serves /api/model for the current profile without touching the clock windows or any other
// global (callModel re-seeds them, which would leak a Box II window into later SC Lite tests).
func modelReply(t *testing.T) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	handleModel(rec, httptest.NewRequest(http.MethodGet, "/api/model", nil))
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("/api/model not JSON: %v", err)
	}
	return m
}

func TestCUCommissionCardInPage(t *testing.T) {
	oldModel := model
	defer func() { model = oldModel }()
	model = profSCLite
	page := servedPage(t)
	for _, s := range []string{
		`<div class="panel hide" id="companel">`, // starts hidden: shown only when the API says available
		`<b>Commissioning</b>`, `id="comnote"`, `id="combody"`,
		"ship <b>switched off</b>", "commissioning guide", "only after its test has passed", // the explainer
		"Switching off is always safe", "resets every switch to off",
		"async function loadCommission(", "function renderCommission(", "function comOpen(", "async function comOn(",
		"async function comOff(", "function comRestartWait(",
		"if(!isOtherModel()||PUB()){p.classList.add('hide');return;}", // never asked for on the SC Lite (nor in the public release)
		"if(!v||!v.ok||!v.available){p.classList.add('hide');",        // shown only when available
		"fetch('/api/commission'", "jpost('/api/commission',{feature:key,on:true,password:pw,note:note})",
		"jpost('/api/commission',{feature:key,on:false})", // switching off sends no password
		"askPassword(", "what did the test show?", "maxlength=\"500\"",
		"fetch('/api/model',fetchTO(4000))", "Restarting the panel…", "location.reload()",
		"loadRecovery();loadCommission();",                                             // loaded with the other Settings cards
		"d.commission&&MODEL()&&MODEL().commission&&d.commission!==MODEL().commission", // reconnect: reload on a new set
		".cmf{", `:root[data-theme="light"] .cmf.on{`,
	} {
		if !strings.Contains(page, s) {
			t.Errorf("served page missing %q", s)
		}
	}
	// no product literal in the card (the model's name comes from /api/model at runtime)
	i := strings.Index(page, `id="companel"`)
	card := page[i : i+strings.Index(page[i:], `id="combody"`)]
	for _, s := range []string{"SC Box", "SC Lite", "OEM"} {
		if strings.Contains(card, s) {
			t.Errorf("card markup carries %q", s)
		}
	}
}

func TestCUCommissionSigSCLiteUnchanged(t *testing.T) {
	oldModel := model
	defer func() { model = oldModel }()
	model = profSCLite
	applyCommission()
	if s := commissionSig(); s != "" {
		t.Fatalf("SC Lite commissionSig = %q, want empty", s)
	}
	m := modelReply(t)
	if _, ok := m["commission"]; ok {
		t.Errorf("SC Lite /api/model carries commission = %v", m["commission"])
	}
	b, _ := json.Marshal(dashboard{})
	if strings.Contains(string(b), `"commission"`) {
		t.Errorf("an SC Lite dashboard would carry commission: %s", b)
	}
}

func TestCUCommissionSigBox2(t *testing.T) {
	defer commissionSetup(t)()
	applyCommission()
	if s := commissionSig(); s != "none" {
		t.Fatalf("Box II with nothing commissioned: sig = %q, want none", s)
	}
	m := modelReply(t)
	if m["commission"] != "none" {
		t.Errorf("/api/model commission = %v, want none", m["commission"])
	}
	os.WriteFile(commissionPath(), []byte(`{"model":"scbox2","features":{"ClockControl":{"on":true},"ChipTrims":{"on":true},"Tuner":{"on":true}}}`), 0o600)
	model = profSCBox2
	applyCommission()
	// the tuner needs voltage control too, so it stays off; the order is the commissionable list's
	if s := commissionSig(); s != "ClockControl,ChipTrims" {
		t.Errorf("sig = %q, want ClockControl,ChipTrims", s)
	}
	b, _ := json.Marshal(dashboard{Commission: commissionSig()})
	if !strings.Contains(string(b), `"commission":"ClockControl,ChipTrims"`) {
		t.Errorf("dashboard JSON: %s", b)
	}
	commissionMu.Lock()
	commissionApplied = map[string]bool{}
	commissionMu.Unlock()
}
