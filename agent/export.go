package main

import (
	"archive/zip"
	"bytes"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Tuner run files — the same idea as blake2b-asic-control's exports: every run streams to a log
// that persists, plus CSV/zip exports. Files live under <tunerdir>/<miner label>/.

var labelRe = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)

// modelTag is the active model's name as a bare filesystem-safe word (letters/digits only), used as the
// default miner label when there is no usable hostname. For the SC Lite profile model.Name is "SC Lite",
// which collapses to exactly "SCLite" — the pre-port literal — so SC Lite behaviour is byte-identical. The
// SC Box II profile yields "SCBoxII"; the unknown profile ("miner") yields "miner".
func modelTag() string {
	var b strings.Builder
	for _, r := range model.Name {
		if r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "miner"
	}
	return b.String()
}

// exportBrand is the brand line that heads the saved log / tuner / report files: the model profile's
// neutral name, never the manufacturer's (hard rule #8: the firmware claims no OEM affiliation).
func exportBrand() string { return model.Name }

// minerLabel is a filesystem-safe name for this miner, used as the run-files subfolder.
func minerLabel() string {
	h, _ := os.Hostname()
	h = strings.TrimSpace(h)
	if h == "" || h == "(none)" || h == "localhost" {
		h = modelTag()
	}
	h = labelRe.ReplaceAllString(h, "_")
	if h == "" {
		h = modelTag()
	}
	return h
}

func tunerRunDir() string { return filepath.Join(*tunerDir, minerLabel()) }

// ---- file builders (operate on a run snapshot) ----

func runHeader(b tunerBundle, private bool) string {
	var sb strings.Builder
	name := minerLabel()
	if private {
		name = "MINER"
	}
	fmt.Fprintf(&sb, "# %s — tuner run\n", exportBrand())
	fmt.Fprintf(&sb, "# miner: %s\n", name)
	if !b.started.IsZero() {
		fmt.Fprintf(&sb, "# started: %s\n", b.started.Format("2006-01-02 15:04:05"))
	}
	if !b.finished.IsZero() {
		fmt.Fprintf(&sb, "# finished: %s\n", b.finished.Format("2006-01-02 15:04:05"))
	}
	base := b.cfg.BaselineSrc
	if base == "own" {
		base = fmt.Sprintf("%d MHz PV %d", b.cfg.BaselineMHz, b.cfg.pvFor(b.cfg.BaselineMV))
		if tunerOnBox2() { // no PV on this model
			base = fmt.Sprintf("%d MHz %s", b.cfg.BaselineMHz, b.cfg.vText(b.cfg.BaselineMV))
		}
	}
	if tunerOnBox2() {
		fmt.Fprintf(&sb, "# mode: %s · baseline %s · range %d–%d MHz step %g · %s–%s step %d\n",
			b.mode, base, b.cfg.LowestMHz, b.cfg.HighestMHz, b.cfg.Step, b.cfg.vText(b.cfg.MinMV), b.cfg.vText(b.cfg.MaxMV), b.cfg.MVStep)
	} else {
		fmt.Fprintf(&sb, "# mode: %s · baseline %s · range %d–%d MHz step %g · PV %d–%d step %d\n",
			b.mode, base, b.cfg.LowestMHz, b.cfg.HighestMHz, b.cfg.Step, b.cfg.pvFor(b.cfg.MinMV), b.cfg.pvFor(b.cfg.MaxMV), b.cfg.MVStep)
	}
	if b.cfg.DryRun {
		fmt.Fprintf(&sb, "# DRY-RUN (read chips, wrote nothing to the miner)\n")
	}
	if b.best != nil {
		fmt.Fprintf(&sb, "# best: %s\n", b.best.text())
	}
	return sb.String()
}

func logTxt(b tunerBundle, private bool) []byte {
	var sb strings.Builder
	sb.WriteString(runHeader(b, private))
	sb.WriteString("\n")
	for _, l := range b.fullLog {
		sb.WriteString(l)
		sb.WriteByte('\n')
	}
	return []byte(sb.String())
}

func csvField(s string) string {
	if strings.ContainsAny(s, ",\"\n") {
		return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
	}
	return s
}

func resultsCSV(b tunerBundle) []byte {
	var sb strings.Builder
	if tunerOnBox2() {
		// no mV and no PV on this model: the voltage is the OEM's voltage level (as the miner was given it) and,
		// next to it, that level in volts (measured, or the estimate; the panel marks an estimate with ≈)
		sb.WriteString("time,mhz,v_level,volts,result,ths,max_c,fan_pct,err_added,minutes,reason\n")
		for _, r := range b.results {
			sb.WriteString(strings.Join([]string{
				r.Time, itoa(r.MHz), itoa(r.MV), box2VoltsCSV(r.MV), r.Result,
				fmt.Sprintf("%.2f", r.THS), fmt.Sprintf("%.1f", r.MaxT), itoa(r.Fan),
				itoa(r.ErrAdd), fmt.Sprintf("%.1f", r.MinS), csvField(r.Reason),
			}, ","))
			sb.WriteByte('\n')
		}
		return []byte(sb.String())
	}
	sb.WriteString("time,mhz,mv,pv,result,ths,max_c,fan_pct,err_added,minutes,reason\n")
	for _, r := range b.results {
		sb.WriteString(strings.Join([]string{
			r.Time, itoa(r.MHz), itoa(r.MV), itoa(r.PV), r.Result,
			fmt.Sprintf("%.2f", r.THS), fmt.Sprintf("%.1f", r.MaxT), itoa(r.Fan),
			itoa(r.ErrAdd), fmt.Sprintf("%.1f", r.MinS), csvField(r.Reason),
		}, ","))
		sb.WriteByte('\n')
	}
	return []byte(sb.String())
}

func chipsCSV(b tunerBundle) []byte {
	var sb strings.Builder
	sb.WriteString("board,chip,errors,limit,health\n")
	for _, c := range b.chips {
		health := "ok"
		if c.Weak {
			health = "weak"
		}
		sb.WriteString(strings.Join([]string{
			itoa(c.Board), itoa(c.Chip), itoa(c.Gained),
			fmt.Sprintf("%.1f", c.Limit), health,
		}, ","))
		sb.WriteByte('\n')
	}
	return []byte(sb.String())
}

func readingsCSV(b tunerBundle) []byte {
	var sb strings.Builder
	b2 := tunerOnBox2()
	if b2 {
		sb.WriteString("time,mhz,v_level,volts,ghs,max_c,err_total,fan_pct\n") // (the level as the miner was given it, not mV, and its volts)
	} else {
		sb.WriteString("time,mhz,mv,ghs,max_c,err_total,fan_pct\n")
	}
	for _, r := range b.readings {
		v := itoa(r.MV)
		if b2 {
			v += "," + box2VoltsCSV(r.MV)
		}
		sb.WriteString(strings.Join([]string{
			r.Time, itoa(r.MHz), v,
			fmt.Sprintf("%.1f", r.GHS), fmt.Sprintf("%.1f", r.MaxT),
			itoa(r.Err), itoa(r.Fan),
		}, ","))
		sb.WriteByte('\n')
	}
	return []byte(sb.String())
}

func itoa(n int) string { return fmt.Sprintf("%d", n) }

// box2VoltsCSV: a Box II level's volts for the CSV's volts column ("" for no level).
func box2VoltsCSV(level int) string {
	if level <= 0 {
		return ""
	}
	v, _ := box2Volts(level)
	return fmt.Sprintf("%.2f", v)
}

func zipBundle(b tunerBundle, private bool) ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	add := func(name string, data []byte) error {
		f, err := zw.Create(name)
		if err != nil {
			return err
		}
		_, err = f.Write(data)
		return err
	}
	base := minerLabel()
	if private {
		base = "miner"
		b = privateBundle(b)
	}
	stamp := ""
	if !b.started.IsZero() {
		stamp = "-" + b.started.Format("20060102-1504")
	}
	root := base + stamp + "/"
	if err := add(root+"log.txt", logTxt(b, private)); err != nil {
		return nil, err
	}
	if err := add(root+"results.csv", resultsCSV(b)); err != nil {
		return nil, err
	}
	if err := add(root+"chip-data.csv", chipsCSV(b)); err != nil {
		return nil, err
	}
	if err := add(root+"live-readings.csv", readingsCSV(b)); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// the pool guard's hold text names the pool it held the miner for (a pool not in the Pools tab, so not among the
// private tokens, or the owner's pool it was redirected from), and its account
var (
	reHoldWhat    = regexp.MustCompile(`(kept coming back: ).*?(\): check the Pools tab)`)
	reStratumURL  = regexp.MustCompile(`(?i)\bstratum[a-z0-9+.-]*://[^\s,()'"]+`)
	rePoolAccount = regexp.MustCompile(`(\(account )[^)]*\)`)
)

// tunerRedactor: takes the private details out of a tuner log line (or a whole saved file).
func tunerRedactor() func(string) string {
	toks := privateTokens()
	return func(s string) string { return redactLog(s, toks) }
}

// privateBundle: the run with private details taken out of its log lines and test reasons, as in the Log tab's
// private export (pools, accounts, names, IPs, MACs, notification secrets).
func privateBundle(b tunerBundle) tunerBundle {
	clean := tunerRedactor()
	b.fullLog = append([]string(nil), b.fullLog...)
	for i, l := range b.fullLog {
		b.fullLog[i] = clean(l)
	}
	b.results = append([]testResult(nil), b.results...)
	for i := range b.results {
		b.results[i].Reason = clean(b.results[i].Reason)
	}
	return b
}

// saveTunerRun writes this run's files to <tunerdir>/<miner>/ and appends to the rolling
// log-all.txt. Best-effort: a read-only or missing path is logged, not fatal.
func saveTunerRun(b tunerBundle) {
	lt := saveTunerFiles(b)
	if lt == nil {
		return
	}
	dir := tunerRunDir()
	// append this run to the rolling every-run log
	all := filepath.Join(dir, "log-all.txt")
	if f, err := os.OpenFile(all, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644); err == nil {
		f.WriteString("\n===== run " + b.started.Format("2006-01-02 15:04:05") + " =====\n")
		f.Write(lt)
		f.Close()
	}
	// kept to about 1 MB: the oldest runs go first (the config partition is small)
	if st, err := os.Stat(all); err == nil && st.Size() > 1<<20 {
		if old, err := os.ReadFile(all); err == nil {
			keep := old[len(old)-(768<<10):]
			if i := strings.Index(string(keep), "\n===== run "); i >= 0 {
				keep = keep[i:]
			}
			tmp := all + ".tmp"
			if os.WriteFile(tmp, keep, 0644) == nil {
				os.Rename(tmp, all)
			}
		}
	}
}

// saveTunerFiles writes this run's log and CSVs (also after each test while it runs, so a run that never
// ends, then a reboot, still leaves its own files). Returns the log text (nil: not written).
func saveTunerFiles(b tunerBundle) []byte {
	dir := tunerRunDir()
	if err := os.MkdirAll(dir, 0755); err != nil {
		log.Printf("tuner: could not create %s: %v", dir, err)
		return nil
	}
	write := func(name string, data []byte) {
		tmp := filepath.Join(dir, name+".tmp")
		if os.WriteFile(tmp, data, 0644) == nil {
			_ = os.Rename(tmp, filepath.Join(dir, name))
		}
	}
	lt := logTxt(b, false)
	write("log.txt", lt)
	write("results.csv", resultsCSV(b))
	write("chip-data.csv", chipsCSV(b))
	write("live-readings.csv", readingsCSV(b))
	return lt
}

// ---- HTTP export ----

func currentBundle() (tunerBundle, bool) {
	tunerMu.Lock()
	t := tunerCur
	tunerMu.Unlock()
	if t == nil {
		return tunerBundle{}, false
	}
	return t.exportBundle(), true
}

func serveBytes(w http.ResponseWriter, filename, ctype string, data []byte) {
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
	w.Write(data)
}

func handleTunerExport(w http.ResponseWriter, r *http.Request) {
	if !featGate(w, model.Feat.Tuner, "the auto-tuner") {
		return
	}
	kind := r.URL.Query().Get("kind")
	// "log, every run" lives only on disk (it spans runs)
	if kind == "logall" {
		p := filepath.Join(tunerRunDir(), "log-all.txt")
		data, err := os.ReadFile(p)
		if err != nil {
			http.Error(w, "no saved runs yet — run the tuner once", 404)
			return
		}
		serveBytes(w, minerLabel()+"-tuner-log-all.txt", "text/plain; charset=utf-8", data)
		return
	}
	b, ok := currentBundle()
	if !ok {
		// agent restarted since the last run — fall back to the saved files on disk
		if diskExport(w, kind) {
			return
		}
		http.Error(w, "no tuner run in memory — run the tuner, or use the saved files", 404)
		return
	}
	name := minerLabel()
	switch kind {
	case "log":
		serveBytes(w, name+"-tuner-log.txt", "text/plain; charset=utf-8", logTxt(b, false))
	case "results":
		serveBytes(w, name+"-results.csv", "text/csv", resultsCSV(b))
	case "chips":
		serveBytes(w, name+"-chip-data.csv", "text/csv", chipsCSV(b))
	case "readings":
		serveBytes(w, name+"-live-readings.csv", "text/csv", readingsCSV(b))
	case "zip":
		data, err := zipBundle(b, false)
		if err != nil {
			http.Error(w, "zip failed: "+err.Error(), 500)
			return
		}
		serveBytes(w, name+"-tuner.zip", "application/zip", data)
	case "zippriv":
		data, err := zipBundle(b, true)
		if err != nil {
			http.Error(w, "zip failed: "+err.Error(), 500)
			return
		}
		serveBytes(w, "tuner-shareable.zip", "application/zip", data)
	default:
		http.Error(w, "unknown export kind", 400)
	}
}

// diskExport serves a single saved file when no run is in memory (after an agent restart).
func diskExport(w http.ResponseWriter, kind string) bool {
	dir := tunerRunDir()
	m := map[string][2]string{
		"log":      {"log.txt", "text/plain; charset=utf-8"},
		"results":  {"results.csv", "text/csv"},
		"chips":    {"chip-data.csv", "text/csv"},
		"readings": {"live-readings.csv", "text/csv"},
	}
	if kind == "zip" || kind == "zippriv" { // the saved files (the private copy with the private details taken out)
		private := kind == "zippriv"
		root := minerLabel()
		var clean func(string) string
		if private {
			root, clean = "miner", tunerRedactor()
		}
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		n := 0
		for _, f := range []string{"log.txt", "results.csv", "chip-data.csv", "live-readings.csv"} {
			if data, err := os.ReadFile(filepath.Join(dir, f)); err == nil {
				if private {
					t := strings.Replace(string(data), "# miner: "+minerLabel()+"\n", "# miner: MINER\n", 1)
					data = []byte(clean(t))
				}
				if w, err := zw.Create(root + "/" + f); err == nil {
					w.Write(data)
					n++
				}
			}
		}
		if zw.Close() != nil || n == 0 {
			return false
		}
		if private {
			serveBytes(w, "tuner-shareable.zip", "application/zip", buf.Bytes())
		} else {
			serveBytes(w, minerLabel()+"-tuner.zip", "application/zip", buf.Bytes())
		}
		return true
	}
	fc, ok := m[kind]
	if !ok {
		return false
	}
	data, err := os.ReadFile(filepath.Join(dir, fc[0]))
	if err != nil {
		return false
	}
	serveBytes(w, minerLabel()+"-"+fc[0], fc[1], data)
	return true
}
