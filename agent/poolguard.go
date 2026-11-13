package main

// Pool guard: the miner mines only the pools entered in the Pools tab. Every 30 s the pools the miner is
// actually using (its API) are compared with the Pools tab's list (Goldshell's minerd, which writes the
// miner's config). A pool, or a pool account, that isn't in the list and is still there a check later is
// disabled and removed from the running miner, logged and notified. If it can't be removed, mining is put
// in Idle: the miner never hashes for a place the owner didn't choose.
//
// Nothing in this firmware adds pools (the image has no built-in, fallback or fee pool); this guards the
// running miner against anything that would.

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	poolGuardEvery = 30 * time.Second
	pgMu           sync.Mutex
	pgStrikes      = map[string]int{}
	pgActed        = map[string]int{}
	pgIncidents    []map[string]any // newest last, at most 10
	pgLastOK       time.Time
	pgListPools    = func() ([]map[string]any, error) { l, _, err := fetchPoolList(); return l, err }
	pgLivePools    = readPools
	pgRemove       = func(n int) error {
		cgminerCmdParam("disablepool", strconv.Itoa(n))
		out, err := cgminerCmdParam("removepool", strconv.Itoa(n))
		if err == nil && !strings.Contains(string(out), `"STATUS":"S"`) {
			err = fmt.Errorf("%s", out)
		}
		return err
	}
	pgIdle = func() error { return idleAction(model) } // lane B3: model-aware park (Box II API board-off, UNPROVEN)
)

// poolKey: what decides where shares go: the pool's address (without the stratum+tcp:// prefix, any
// case) and the account (user/worker).
func poolKey(url, user string) string {
	u := strings.ToLower(strings.TrimSpace(url))
	if i := strings.Index(u, "://"); i >= 0 {
		u = u[i+3:]
	}
	return strings.TrimSuffix(u, "/") + "|" + strings.TrimSpace(user)
}

func poolGuardLoop() {
	for {
		time.Sleep(poolGuardEvery)
		poolGuardCheck()
	}
}

var pgCleanSince time.Time

// pgAddIncident keeps the newest 10 (pgMu held): a redirect that keeps coming back while the Idle write fails
// adds one every check.
func pgAddIncident(inc map[string]any) {
	pgIncidents = append(pgIncidents, inc)
	if len(pgIncidents) > 10 {
		pgIncidents = append([]map[string]any(nil), pgIncidents[len(pgIncidents)-10:]...)
	}
}

func poolGuardCheck() {
	if t, what := lastMinerChange(); strings.HasPrefix(what, "pool") && time.Since(t) < 2*time.Minute {
		return // the Pools tab just changed the list: the miner restarts with the old one for a moment
	}
	list, err := pgListPools()
	if err != nil {
		return // the Pools tab's list isn't available: nothing to compare with
	}
	if len(list) == 0 && pgSavedHasPools() {
		return // reads empty while minerd restarts (its saved list still has pools): nothing to compare with
	}
	// for the private log export: the pools (also those set on Goldshell's own page) and this miner's name,
	// remembered for after they change (written only when something is new)
	if h, err := os.Hostname(); err == nil {
		rememberPrivate(append(poolListTokens(list), h, minerLabel()))
	}
	// an empty Pools tab (and nothing saved): every pool the miner runs is one nobody entered
	live, err := pgLivePools()
	if err != nil {
		return
	}
	allowed := map[string]bool{}
	for _, p := range list {
		allowed[poolKey(poolStrAny(p, "url", "URL"), poolStrAny(p, "user", "User"))] = true
	}
	pgMu.Lock()
	defer pgMu.Unlock()
	seen := map[string]bool{}
	sort.SliceStable(live, func(i, j int) bool { return live[i].Index > live[j].Index }) // removals renumber the rest
	for _, p := range live {
		// redirected: the entry is one of yours but its connection went to another server (a pool's
		// client.reconnect; the miner starts with --no-pool-redirect, this is the second line of defence)
		if h, sh := urlHost(p.URL), urlHost(p.StratumURL); sh != "" && h != "" && sh != h && p.Active {
			k := "redirect|" + sh
			seen[k] = true
			if _, held := poolHold(); held && minerIdleForGuard() {
				continue // already held on Idle: no new Idle write, incident or notification
			}
			pgActed[k]++
			inc := map[string]any{"t": time.Now().Unix(), "pool": p.StratumURL, "user": p.User}
			what := fmt.Sprintf("%s, redirected from %s", p.StratumURL, p.URL)
			if pgActed[k] <= 2 {
				inc["action"] = "redirected: mining program restarted"
				if err := pgRestartMiner(); err != nil {
					inc["action"], inc["err"] = "redirected: restart failed", err.Error()
					activity.emit("🛡 pool guard: the pool moved the miner to another server (" + what + "); restarting the mining program failed: " + err.Error())
					notify(evPoolGuard, "the pool moved the miner to another server ("+what+"); restarting the mining program failed (the pool guard tries again, then sets Idle)")
				} else {
					activity.emit("🛡 pool guard: the pool moved the miner to another server (" + what + "): the mining program was restarted, back on your pool")
					notify(evPoolGuard, "the pool moved the miner to another server ("+what+"): the mining program was restarted")
				}
			} else {
				poolHoldSet(what)
				stopTunerFor("the pool guard put the miner on Idle")
				if err := pgIdle(); err == nil {
					setCharge("idle")
					inc["action"] = "idle (held)"
					activity.emit("🛡 pool guard: a pool keeps moving the miner to another server (" + what + "): the miner is set to Idle and held there until you resume it (Pools tab)" + idleActionNote())
					notifyAlways(evPoolGuard, "a pool keeps moving the miner to another server ("+what+"): the miner is set to Idle and held there until you resume it (Pools tab)"+idleActionNote())
				} else {
					// Idle didn't take: stop the pools in the running miner itself so it hashes for nobody
					for _, lp := range live {
						cgminerCmdParam("disablepool", strconv.Itoa(lp.Index))
					}
					inc["action"], inc["err"] = "idle failed: all pools disabled in the miner", err.Error()
					activity.emit("🛡 pool guard: a pool keeps moving the miner to another server (" + what + "); setting Idle failed (" + err.Error() + "): every pool in the running miner is disabled")
					notifyAlways(evPoolGuard, "a pool keeps moving the miner to another server ("+what+"); setting Idle failed, so every pool in the running miner is disabled: it hashes for nobody until you resume (Pools tab)")
				}
			}
			pgAddIncident(inc)
			continue
		}
		k := poolKey(p.URL, p.User)
		host := strings.SplitN(k, "|", 2)[0]
		if allowed[k] || host == "" || host == "benchmark" { // benchmark: Goldshell's no-pool placeholder, offline
			continue
		}
		seen[k] = true
		if pgStrikes[k]++; pgStrikes[k] < 2 {
			continue // seen once: the Pools tab may be mid-change (minerd restarts the miner on a change)
		}
		pgActed[k]++
		what := fmt.Sprintf("%s (account %s)", p.URL, p.User)
		inc := map[string]any{"t": time.Now().Unix(), "pool": p.URL, "user": p.User}
		if _, held := poolHold(); held && minerIdleForGuard() {
			// already held on Idle: nothing more to do (no new Idle write, notification or hold file)
			pgStrikes[k] = 0
			continue
		}
		removed := false
		if pgActed[k] <= 2 {
			if err := pgRemove(p.Index); err == nil {
				removed = true
				inc["action"] = "removed"
				activity.emit("🛡 pool guard: removed a pool that isn't in the Pools tab: " + what)
				notify(evPoolGuard, "removed a pool that isn't in the Pools tab: "+what)
			} else {
				inc["err"] = err.Error() // (the miner refuses to remove the pool it's mining on): Idle now
			}
		}
		if !removed {
			// it keeps coming back: stop hashing altogether
			// held: nothing wakes the miner (presets, Stock, the schedule, the tuner) until the owner resumes
			poolHoldSet(what)
			stopTunerFor("the pool guard put the miner on Idle")
			if err := pgIdle(); err == nil {
				setCharge("idle")
				inc["action"] = "idle"
				activity.emit("🛡 pool guard: a pool that isn't in the Pools tab keeps coming back (" + what + "): the miner is set to Idle and held there until you resume it (Pools tab)" + idleActionNote())
				notifyAlways(evPoolGuard, "a pool that isn't in the Pools tab keeps coming back ("+what+"): the miner is set to Idle and held there until you resume it (Pools tab)"+idleActionNote())
			} else {
				// Idle didn't take: stop the pools in the running miner itself so it hashes for nobody
				for _, lp := range live {
					cgminerCmdParam("disablepool", strconv.Itoa(lp.Index))
				}
				inc["action"], inc["err"] = "idle failed: all pools disabled in the miner", err.Error()
			}
		}
		pgAddIncident(inc)
		pgStrikes[k] = 0
	}
	for k := range pgStrikes {
		if !seen[k] {
			delete(pgStrikes, k)
		}
	}
	if len(seen) == 0 {
		pgLastOK = time.Now()
		if pgCleanSince.IsZero() {
			pgCleanSince = time.Now()
		} else if time.Since(pgCleanSince) > time.Hour && len(pgActed) > 0 {
			pgActed = map[string]int{} // an hour of only your pools: old sightings no longer count towards Idle
		}
	} else {
		pgCleanSince = time.Time{}
	}
}

// pgSavedHasPools: minerd's saved pool list (where the Pools tab's changes end up) has a pool with an address
// other than Goldshell's offline placeholder. Unreadable or in a shape not understood: true (the guard then
// doesn't treat an empty answer from minerd as the owner's list).
func pgSavedHasPools() bool {
	b, err := os.ReadFile(filepath.Join(filepath.Dir(filepath.Dir(*bfgPath)), "minerd/settings/pools.json"))
	if err != nil {
		return !os.IsNotExist(err)
	}
	switch strings.TrimSpace(string(b)) {
	case "", "null", "[]", "{}", `{"pools":null}`, `{"pools":[]}`:
		return false
	}
	var arr []map[string]any
	if json.Unmarshal(b, &arr) != nil {
		var obj struct {
			Pools []map[string]any `json:"pools"`
		}
		if json.Unmarshal(b, &obj) != nil || obj.Pools == nil {
			return true
		}
		arr = obj.Pools
	}
	for _, p := range arr {
		if h := strings.SplitN(poolKey(poolStrAny(p, "url", "URL"), ""), "|", 2)[0]; h != "" && h != "benchmark" {
			return true
		}
	}
	return false
}

// minerIdleForGuard: the miner is on Idle now (a stub in tests).
var minerIdleForGuard = func() bool { return getCharge() == "idle" || minerIsIdleNow() }

// GET: the guard's state for the Pools tab.
func handlePoolGuard(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var in struct {
			Resume bool `json:"resume"`
		}
		if !decodeIn(w, r, &in) {
			return
		}
		if in.Resume {
			poolHoldRelease()
			pgMu.Lock()
			pgActed = map[string]int{}
			pgMu.Unlock()
			activity.emit("🛡 pool guard: resumed by hand: presets, " + stockWord() + " and the schedule can start mining again")
		}
	}
	pgMu.Lock()
	defer pgMu.Unlock()
	v := map[string]any{"ok": true, "incidents": append([]map[string]any(nil), pgIncidents...)}
	if h, ok := poolHold(); ok {
		v["hold"] = h
	}
	if !pgLastOK.IsZero() {
		v["checked"] = pgLastOK.Unix()
	}
	writeJSON(w, v)
}

func poolStrAny(p map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := p[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// ---- the guard's hold on Idle ----

func poolHoldPath() string { return filepath.Join(filepath.Dir(*authFile), "pool-hold.json") }

type poolHoldState struct {
	At   int64  `json:"at"`
	What string `json:"what"`
}

func poolHoldSet(what string) {
	if h, ok := poolHold(); ok && h.At > 0 {
		return // already held: keep when it started
	}
	h := poolHoldState{At: time.Now().Unix(), What: what}
	b, _ := json.Marshal(h)
	err := writeAtomic(poolHoldPath(), b, 0644)
	poolHoldMemMu.Lock()
	if poolHoldMem = nil; err != nil {
		poolHoldMem = &h // a full or read-only config partition: held until Resume (or a restart) all the same
	}
	poolHoldMemMu.Unlock()
}

var (
	poolHoldMemMu sync.Mutex
	poolHoldMem   *poolHoldState // the hold while its file couldn't be written
)

func poolHoldRelease() {
	os.Remove(poolHoldPath())
	poolHoldMemMu.Lock()
	poolHoldMem = nil
	poolHoldMemMu.Unlock()
}

// poolHold: the file being there is the hold (a damaged one, after a power cut, still holds).
func poolHold() (poolHoldState, bool) {
	var h poolHoldState
	b, err := os.ReadFile(poolHoldPath())
	if err != nil {
		if os.IsNotExist(err) {
			poolHoldMemMu.Lock()
			defer poolHoldMemMu.Unlock()
			if poolHoldMem != nil {
				return *poolHoldMem, true
			}
		}
		return h, !os.IsNotExist(err)
	}
	json.Unmarshal(b, &h)
	return h, true
}

// poolHoldErr: refuse anything that would start mining while the guard holds the miner on Idle.
func poolHoldErr() error {
	if h, ok := poolHold(); ok {
		return fmt.Errorf("mining is held on Idle by the pool guard (a pool that isn't in the Pools tab kept coming back: %s): check the Pools tab, then Resume there", h.What)
	}
	return nil
}

// miningHoldErr: the fan lock or the pool guard's hold.
func miningHoldErr() error {
	if err := fanLockErr(); err != nil {
		return err
	}
	return poolHoldErr()
}

func stopTunerFor(why string) { stopRunsFor("the pool guard", why) }

// stopRunsFor: who ("the pool guard", "the thermal guard") ends a tuning run, the limit probe and the strong-chip test.
func stopRunsFor(who, why string) {
	safetyStop() // (before its Idle: see safetyGen)
	tunerMu.Lock()
	t := tunerCur
	tunerMu.Unlock()
	// (tunerCur stays set after a run ends: no "stopped" line for a finished run; a run frozen holding its lock
	// doesn't hold up the Idle that follows: see StopForSoon)
	if t != nil && t.StopForSoon(who+": "+why, 2*time.Second) {
		activity.emit("🔧 tuning run stopped: " + why)
	}
	if getCharge() == "ceiling" {
		ceilAbort.Store(true) // the limit probe too
	}
	sctEndFor(strings.ToUpper(who[:1]) + who[1:] + ": " + why) // and the strong-chip test (its raised chips go back)
}

// urlHost: the host name of a pool address, lower case, without scheme, port or path.
func urlHost(u string) string {
	u = strings.ToLower(strings.TrimSpace(u))
	if i := strings.Index(u, "://"); i >= 0 {
		u = u[i+3:]
	}
	if i := strings.IndexAny(u, "/#?"); i >= 0 {
		u = u[:i]
	}
	if i := strings.LastIndex(u, "@"); i >= 0 {
		u = u[i+1:]
	}
	if h, _, err := net.SplitHostPort(u); err == nil {
		return strings.Trim(h, "[]")
	}
	return strings.Trim(u, "[]")
}

// pgRestartMiner: the mining program is restarted (Goldshell's watchdog starts it again, on the configured pools).
var pgRestartMiner = func() error {
	cmd := exec.Command("sh", "-c", "killall intminer")
	cmd.Env = append(os.Environ(), "PATH=/usr/sbin:/usr/bin:/sbin:/bin:"+os.Getenv("PATH"))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%v %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
