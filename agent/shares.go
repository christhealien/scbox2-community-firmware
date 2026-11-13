package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Best-share book (blake2b-asic-control parity). The miner's best share starts again from 0 every time it
// restarts; this logs every new best the moment it's seen, with the setting it was on, so the record survives
// restarts. It keeps the 20 biggest (the page shows the top 10, plus the best since restart if it's lower).

type shareHit struct {
	V     float64 `json:"v"`
	At    int64   `json:"at"`
	After int64   `json:"after,omitempty"` // first seen after a gap: found some time between After and At
	On    string  `json:"on"`
}

type shareBook struct {
	History []shareHit `json:"history"`
	ResetAt int64      `json:"reset_at,omitempty"`
	NowV    float64    `json:"now_v"`  // best since the miner's last restart
	NowAt   int64      `json:"now_at"` // when that one was seen
	NowOn   string     `json:"now_on"`
	Since   int64      `json:"since"` // the miner's last restart
}

var (
	bestBookMu sync.Mutex
	shares     shareBook
	sharesRead bool
	shareSeen  bool // seen the miner at least once since the agent started
)

func sharesPath() string { return filepath.Join(filepath.Dir(*authFile), "shares.json") }

func loadShares() {
	if sharesRead {
		return
	}
	sharesRead = true
	if b, ok := readConfig(sharesPath(), json.Valid); ok {
		json.Unmarshal(b, &shares)
	}
}

func saveShares() {
	b, _ := json.Marshal(shares)
	cfgWrite(sharesPath(), b, 0600)
}

// settingText: what the miner runs now, for the "On" column.
func settingText() string {
	on := ""
	if pp, ok := readStockBaseline("current"); ok && pp.FreqMHz > 0 {
		on = fmt.Sprintf("%d MHz · %d mV · PV %d", pp.FreqMHz, pp.VrunMv, pp.PVMv)
		if model.Plan == planBox2V { // no mV / PV here: "V" is the OEM's voltage level
			on = fmt.Sprintf("%d MHz · %s", pp.FreqMHz, box2LevelText(pp.VrunMv))
		}
	}
	if c := getCharge(); strings.HasPrefix(c, "preset:") {
		on += " (" + presetLabel(strings.TrimPrefix(c, "preset:")) + ")"
	} else if c == "tuner" {
		on += " (tuning)"
	} else if c == "idle" {
		on = "Idle"
	}
	return on
}

// shareObserve runs on every successful poll of the miner.
func shareObserve(d dashboard) {
	if !clockOK() {
		return // caught up once the clock is set: the miner keeps its best-since-restart
	}
	bestBookMu.Lock()
	defer bestBookMu.Unlock()
	loadShares()
	now := time.Now().Unix()
	since := now - int64(d.Elapsed)
	restarted := shares.Since > 0 && since > shares.Since+90
	if restarted && shares.NowV > 0 && d.BestShare >= shares.NowV-0.5 && d.BestShare <= shares.NowV+0.5 {
		// the very same best share: the clock jumped forward (set from a browser, then by a time
		// server), the miner didn't restart. A fresh run doesn't land on the same number.
		restarted = false
	}
	if restarted || d.BestShare+0.5 < shares.NowV {
		shares.NowV, shares.NowAt, shares.NowOn = 0, 0, ""
	}
	shares.Since = since
	first := !shareSeen
	shareSeen = true
	if d.BestShare <= shares.NowV+0.5 || d.BestShare <= 0 {
		return
	}
	hit := shareHit{V: d.BestShare, At: now, On: settingText()}
	if first && d.Elapsed > 120 {
		// the agent wasn't watching: it was found some time since the miner started
		hit.After = since
	}
	shares.NowV, shares.NowAt, shares.NowOn = hit.V, hit.At, hit.On
	shares.History = append(shares.History, hit)
	sort.SliceStable(shares.History, func(i, j int) bool { return shares.History[i].V > shares.History[j].V })
	if len(shares.History) > 20 {
		shares.History = shares.History[:20]
	}
	saveShares()
}

func handleShares(w http.ResponseWriter, r *http.Request) {
	bestBookMu.Lock()
	defer bestBookMu.Unlock()
	loadShares()
	if r.Method == http.MethodPost {
		var in struct {
			Action string `json:"action"`
		}
		if !decodeIn(w, r, &in) {
			return
		}
		if in.Action != "reset" {
			writeJSON(w, map[string]any{"ok": false, "err": "unknown action"})
			return
		}
		keep := []shareHit{}
		if shares.NowV > 0 { // the best since restart stays: the miner keeps that one itself
			keep = append(keep, shareHit{V: shares.NowV, At: shares.NowAt, On: shares.NowOn})
		}
		shares.History, shares.ResetAt = keep, time.Now().Unix()
		saveShares()
		writeJSON(w, map[string]any{"ok": true})
		return
	}
	hist := []map[string]any{}
	recV := 0.0
	var rec *shareHit
	for i := range shares.History {
		h := shares.History[i]
		if h.V > recV {
			recV, rec = h.V, &shares.History[i]
		}
	}
	for i, h := range shares.History {
		hist = append(hist, map[string]any{"rank": i + 1, "text": fmtShareUnit(h.V), "at": h.At, "after": h.After, "on": h.On,
			"current": shares.NowV > 0 && h.V == shares.NowV && h.At == shares.NowAt, "record": rec != nil && h.V == rec.V && h.At == rec.At})
	}
	out := map[string]any{"ok": true, "history": hist, "since": shares.Since, "reset_at": shares.ResetAt,
		"now_text": fmtShareUnit(shares.NowV), "now_at": shares.NowAt, "now_on": shares.NowOn}
	if rec != nil {
		out["record_text"], out["record_at"], out["record_on"] = fmtShareUnit(rec.V), rec.At, rec.On
	} else {
		out["record_text"] = "—"
	}
	writeJSON(w, out)
}

// bestEver: the highest best share the agent has on record for this miner (history and this run).
func bestEver() float64 {
	bestBookMu.Lock()
	defer bestBookMu.Unlock()
	loadShares()
	m := shares.NowV
	for _, h := range shares.History {
		if h.V > m {
			m = h.V
		}
	}
	return m
}
