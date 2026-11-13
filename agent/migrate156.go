package main

// The v1.5.6 upgrade: a clean start for tuning. Presets made before v1.5.6 were judged with the lagging
// hashrate (fixed in v1.5.5), with voltage settings below the supply's floor, and by the old tuner (100 mV
// steps, no confirm). On the first start of v1.5.6 (once):
//   - the saved presets are moved aside to presets.pre-1.5.6.json (Settings → Firmware update can put them
//     back) and the list is emptied: the Stock and Idle cards stay;
//   - a schedule that switched presets is switched off (its rules would name presets that are gone);
//   - auto fan on a preset's tuned curve goes back to the balanced curve;
//   - a miner on a manual setting (a preset, the tuner, Controls) goes back to the firmware's stock plan.
//     An idle miner stays idle (stock applies when it's woken).

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type migration156 struct {
	At             int64  `json:"at"`
	Presets        int    `json:"presets_removed"`
	Backup         string `json:"backup,omitempty"`
	SchedulePaused bool   `json:"schedule_off,omitempty"`
	FanReset       bool   `json:"fan_reset,omitempty"`
	Stock          string `json:"stock"` // "applied", "already", "idle", "failed: …", "pending"
	Restored       int64  `json:"restored,omitempty"`
}

func migration156Path() string { return filepath.Join(filepath.Dir(*authFile), "migrated-1.5.6.json") }
func presetsBackup156() string {
	return filepath.Join(filepath.Dir(*presetPath), "presets.pre-1.5.6.json")
}

func loadMigration156() (migration156, bool) { return loadMigration156At(migration156Path()) }

func loadMigration156At(path string) (migration156, bool) {
	var m migration156
	b, err := os.ReadFile(path)
	if err != nil {
		return m, !os.IsNotExist(err) // there but unreadable: done (running it twice would clear presets made since)
	}
	json.Unmarshal(b, &m)
	return m, true // a damaged record still means it ran
}

func saveMigration156(m migration156) { saveMigration156At(migration156Path(), m) }

func saveMigration156At(path string, m migration156) {
	migMu.Lock()
	defer migMu.Unlock()
	b, _ := json.MarshalIndent(m, "", " ")
	tmp := path + ".tmp"
	if writeFileSync(tmp, b, 0o644) == nil {
		os.Rename(tmp, path)
	}
}

var migMu sync.Mutex

// migrate156Done closes when the stock part has finished (tests wait on it)
var migrate156Done = make(chan struct{})

// migrate156 runs once, at start; stockWait is how long to keep trying the stock plan (the backend answers
// a while after boot).
var migrateStock = applyStockPlan

func migrate156(stockWait time.Duration) {
	if _, done := loadMigration156(); done {
		return
	}
	m := migration156{At: time.Now().Unix(), Stock: "pending"}
	if ps := loadPresets(); len(ps) > 0 {
		if b, err := os.ReadFile(*presetPath); err == nil {
			if _, err := os.Stat(presetsBackup156()); err == nil {
				m.Backup = presetsBackup156() // an earlier (interrupted) run's backup is the original: keep it
			} else if writeFileSync(presetsBackup156(), b, 0o644) == nil {
				m.Backup = presetsBackup156()
			}
		}
		if m.Backup != "" && savePresets([]preset{}) == nil {
			m.Presets = len(ps)
		}
	}
	if s := loadSchedule(); s.Enabled && len(s.Rules) > 0 {
		s.Enabled = false
		if saveSchedule(s) == nil {
			m.SchedulePaused = true
		}
	}
	fanMu.RLock()
	fc := fanCfg
	fanMu.RUnlock()
	if strings.HasPrefix(fc.Profile, "tuned:") || (fc.Profile == "custom" && m.Presets > 0 && strings.HasPrefix(getCharge(), "preset:")) {
		fc.Profile = "curve-balanced"
		if setFanConfig(fc) == nil {
			m.FanReset = true
		}
	}
	// a tuning run cut short by the upgrade's restart: stock replaces its baseline, so nothing to put back
	clearTunerMarker()
	if strings.HasPrefix(getCharge(), "preset:") {
		setCharge("")
	}
	saveMigration156(m) // the presets part is done even if the stock part below never gets an answer
	activity.emit(fmt.Sprintf("🧹 v1.5.6: presets cleared for a fresh tuning (%d moved aside; Settings → Firmware update can put them back)", m.Presets))
	path := migration156Path()
	done := make(chan struct{})
	migrate156Done = done
	go func() {
		defer close(done)
		st := migrateToStock(stockWait)
		if cur, ok := loadMigration156At(path); ok { // reread: a restore may have been recorded meanwhile
			m = cur
		}
		m.Stock = st
		saveMigration156At(path, m)
		if st == "applied" {
			activity.emit("🧹 v1.5.6: the miner is back on its stock settings")
		}
	}()
}

// migrateToStock: a miner on a manual plan goes back to the firmware's stock plan; idle and stock stay.
func migrateToStock(wait time.Duration) string {
	var lastErr error
	for t0 := time.Now(); ; {
		raw, err := stockGet("/mcb/setting")
		if err == nil {
			var cur map[string]any
			if json.Unmarshal(raw, &cur) == nil {
				if minerIdle(cur) {
					return "idle"
				}
				if manual, _ := cur["manual"].(bool); !manual {
					return "already"
				}
				if err = migrateStock(); err == nil {
					return "applied"
				}
			}
		}
		lastErr = err
		if time.Since(t0) > wait {
			break
		}
		time.Sleep(15 * time.Second)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no answer from the miner")
	}
	return "failed: " + lastErr.Error()
}

// restorePresets156: Settings → Firmware update → "Put my old presets back".
func restorePresets156() (int, error) {
	b, err := os.ReadFile(presetsBackup156())
	if err != nil {
		return 0, fmt.Errorf("no presets were moved aside")
	}
	var old []preset
	if err := json.Unmarshal(b, &old); err != nil {
		return 0, fmt.Errorf("the saved presets can't be read: %v", err)
	}
	cur := loadPresets()
	have := map[string]bool{}
	for _, p := range cur {
		have[p.Name] = true
	}
	n := 0
	for _, p := range old {
		if !have[p.Name] { // a preset made since (a new tuning run) wins over the old one of the same name
			cur = append(cur, p)
			n++
		}
	}
	if err := savePresets(cur); err != nil {
		return 0, err
	}
	if m, ok := loadMigration156(); ok {
		m.Restored = time.Now().Unix()
		saveMigration156(m)
	}
	return n, nil
}
