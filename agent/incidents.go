package main

// Keeping evidence for later (v1.5.19). An owner's board dropped out at about 1 pm; by the time they exported
// the logs that evening, the OEM's logs only went back half an hour to a few hours, and the panel's own
// activity feed lived only in the open page. Now:
//   - the panel's events (the activity feed) are kept, the last 500 lines, saved every 10 minutes and at an
//     incident, and go into the private log export as panel-events.txt;
//   - when a hash board stops hashing, the mining log and the OEM's logs as they are then are saved
//     (gzip, the last 2 incidents) and go into the export as incident-*.txt.
// Both are redacted like every other file in the private export.

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const eventKeep = 500

var (
	evMu      sync.Mutex
	evLines   []string
	evDirty   bool
	evLoaded  bool
	evSaveGap = 10 * time.Minute
)

func eventsPath() string    { return filepath.Join(filepath.Dir(*authFile), "events.log") }
func incidentsDir() string  { return filepath.Join(filepath.Dir(*authFile), "incidents") }
func eventsDateFmt() string { return "2006-01-02 " }

func eventsLoad() {
	if evLoaded {
		return
	}
	evLoaded = true
	if b, err := os.ReadFile(eventsPath()); err == nil {
		for _, l := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
			if l != "" {
				evLines = append(evLines, l)
			}
		}
		if len(evLines) > eventKeep {
			evLines = evLines[len(evLines)-eventKeep:]
		}
	}
}

// eventRecord: every activity line, with the date (the feed's own lines carry only the time).
func eventRecord(line string) {
	evMu.Lock()
	defer evMu.Unlock()
	eventsLoad()
	evLines = append(evLines, time.Now().Format(eventsDateFmt())+line)
	if len(evLines) > eventKeep {
		evLines = evLines[len(evLines)-eventKeep:]
	}
	evDirty = true
}

func eventsSave() {
	evMu.Lock()
	if !evDirty {
		evMu.Unlock()
		return
	}
	data := []byte(strings.Join(evLines, "\n") + "\n")
	evDirty = false
	evMu.Unlock()
	if err := writeAtomic(eventsPath(), data, 0o644); err != nil {
		evMu.Lock()
		evDirty = true
		evMu.Unlock()
	}
}

func eventsText() string {
	evMu.Lock()
	defer evMu.Unlock()
	eventsLoad()
	if len(evLines) == 0 {
		return "(no panel events kept yet)\n"
	}
	return strings.Join(evLines, "\n") + "\n"
}

// eventsSaveGap: how often the events log is checkpointed. The lines live in RAM (evLines); on the Box II's
// FAT flash a 10-min write cadence is thinned to hourly (R04) — events are also flushed on an incident
// (incidentSnapshot) and before a panel-initiated reboot (markCleanReboot). SC Lite's ext keeps 10 min.
func eventsSaveGap() time.Duration { return cfgCheckpointGap(time.Hour, evSaveGap) }

func eventsLoop() {
	for {
		time.Sleep(eventsSaveGap())
		eventsSave()
	}
}

// incidentSnapshot: the logs as they are now, saved for a later export (the last 2 kept).
var incidentLogs = func() map[string]string {
	out := map[string]string{}
	if t, err := minerLogText(); err == nil {
		out["mining-log"] = t
	}
	for _, gl := range oemLogs() {
		if t, err := tailFile(gl[1], 200<<10); err == nil {
			out[strings.TrimSuffix(gl[0], ".txt")] = t
		}
	}
	return out
}

var (
	incidentMu sync.Mutex
	incidentWG sync.WaitGroup // the background snapshots and restarts (tests wait for them)
)

func incidentSnapshot(what string) {
	incidentMu.Lock()
	defer incidentMu.Unlock()
	eventsSave()
	rememberPrivate(poolNameTokens()) // the pools and name these logs carry, for an export after a change
	logs := incidentLogs()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	fmt.Fprintf(zw, "# %s\n# saved %s\n\n", what, time.Now().Format("2006-01-02 15:04:05 MST"))
	keys := make([]string, 0, len(logs))
	for k := range logs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(zw, "\n===== %s =====\n%s\n", k, logs[k])
	}
	zw.Close()
	os.MkdirAll(incidentsDir(), 0o755)
	name := filepath.Join(incidentsDir(), "incident-"+time.Now().Format("20060102-150405")+".txt.gz")
	if err := writeAtomic(name, buf.Bytes(), 0o644); err != nil {
		return
	}
	files, _ := filepath.Glob(filepath.Join(incidentsDir(), "incident-*.txt.gz"))
	sort.Strings(files)
	left, _ := filepath.Glob(filepath.Join(incidentsDir(), "incident-*.tmp")) // a power cut's half-written one
	for _, f := range left {
		os.Remove(f)
	}
	for len(files) > 2 {
		os.Remove(files[0])
		files = files[1:]
	}
}

// incidentTexts: the kept snapshots, oldest first, as (name, text).
func incidentTexts() [][2]string {
	files, _ := filepath.Glob(filepath.Join(incidentsDir(), "incident-*.txt.gz"))
	sort.Strings(files)
	left, _ := filepath.Glob(filepath.Join(incidentsDir(), "incident-*.tmp")) // a power cut's half-written one
	for _, f := range left {
		os.Remove(f)
	}
	var out [][2]string
	for _, f := range files {
		fh, err := os.Open(f)
		if err != nil {
			continue
		}
		zr, err := gzip.NewReader(fh)
		if err != nil {
			fh.Close()
			continue
		}
		b, _ := io.ReadAll(io.LimitReader(zr, 4<<20))
		zr.Close()
		fh.Close()
		out = append(out, [2]string{strings.TrimSuffix(filepath.Base(f), ".gz"), string(b)})
	}
	return out
}
