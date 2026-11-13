package main

// Chip report: one file with everything about the chip trims, to save and send for a look: the settings,
// each chip's clock and nonce counts now, how the counts grew (a sample every 10 minutes, last 48 hours),
// the hook's log, auto-trim's state, the strong-chip test and every chip's hardware errors.

import (
	"encoding/json"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	chipHist     []map[string]any
	chipHistMu   sync.Mutex
	chipHistStep = 10 * time.Minute
)

const chipHistMax = 288 // 48 hours

func chipHistoryLoop() {
	for {
		chipHistSample()
		time.Sleep(chipHistStep)
	}
}

// trimHookLog keeps the hook's log (in RAM, /tmp) short: the miner calls its clock function every few
// seconds to a minute on its own, and the hook logs each call.
func trimHookLog(max int64, keep int) {
	if fi, err := os.Stat(chipHookLog); err == nil && fi.Size() > max {
		ls := tailLines(chipHookLog, keep)
		if os.WriteFile(chipHookLog+".tmp", []byte(strings.Join(ls, "\n")+"\n"), 0o644) == nil {
			os.Rename(chipHookLog+".tmp", chipHookLog) // (a short write to a full /tmp mustn't replace the log)
		}
	}
}

func chipHistSample() {
	trimHookLog(64<<10, 400)
	st, mod, ok := readChipStats()
	if !ok || time.Since(mod) > 45*time.Second {
		return
	}
	var chains []map[string]any
	for _, c := range st.Chains {
		chains = append(chains, map[string]any{"chain": c.Chain, "base10": c.Base10, "clock10": c.Clock10, "nonces": c.Nonces})
		chipSegNonces(c.Chain, c.Clock10, c.Nonces) // a trim changed live is noticed every 10 min with the page closed too
	}
	chipHistMu.Lock()
	chipHist = append(chipHist, map[string]any{"t": time.Now().Unix(), "chains": chains})
	if len(chipHist) > chipHistMax {
		chipHist = chipHist[len(chipHist)-chipHistMax:]
	}
	chipHistMu.Unlock()
}

func handleChipReport(w http.ResponseWriter, r *http.Request) {
	v := chipTrimView()
	raw, _ := os.ReadFile(chipStatsPath)
	chipHistMu.Lock()
	hist := append([]map[string]any(nil), chipHist...)
	chipHistMu.Unlock()
	rep := map[string]any{
		"report":     model.ID + " chip report",
		"generated":  time.Now().Format(time.RFC3339),
		"enabled":    v["enabled"],
		"active":     v["active"],
		"trims":      v["trims"],
		"chains":     v["chains"],
		"stats_raw":  json.RawMessage(orNull(raw)),
		"hook_log":   tailLines(chipHookLog, 500),
		"strong":     sctView(),
		"autotrim":   v["watch"],
		"hw_errors":  reportErrors(),
		"history":    hist,
		"history_at": "every " + chipHistStep.String() + ", last 48 hours, nonces are counts since mining started",
	}
	b, _ := json.MarshalIndent(rep, "", " ")
	name := model.ID + "-chips-" + time.Now().Format("2006-01-02-1504") + ".json"
	serveBytes(w, name, "application/json", b)
}

func orNull(b []byte) []byte {
	if !json.Valid(b) {
		return []byte("null")
	}
	return b
}

// reportErrors: every chip's hardware-error count (the miner's own per-chip counters, since mining started).
func reportErrors() any {
	m, err := readChipErrors()
	if err != nil {
		return map[string]any{"err": err.Error()}
	}
	out := []map[string]int{}
	for k, v := range m {
		out = append(out, map[string]int{"board": k.B, "chip": k.I, "hwerr": v})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i]["board"] != out[j]["board"] {
			return out[i]["board"] < out[j]["board"]
		}
		return out[i]["chip"] < out[j]["chip"]
	})
	return out
}
