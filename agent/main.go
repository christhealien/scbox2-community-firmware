// sclite-agent — on-device agent for SC Lite custom firmware (v2).
//
// One static ARMv7 binary (Bitaxe-style: one program, one UI). It does NOT mine —
// the closed intminer does that — it reads intminer's state over the local :4028
// socket, tails the miner logfile for the realtime log, serves the UI, and gates
// everything behind a login with a forced password change on first use.
//
//	/                       dashboard (login -> change-password -> dashboard)
//	/api/me                 auth state for the UI
//	/api/login   (POST)     log in, issues a signed session cookie
//	/api/logout  (POST)     clears the session
//	/api/passwd  (POST)     change password (clears must-change)
//	/api/dashboard          normalized live data (auth)
//	/api/cmd/<name>         raw read-only :4028 proxy (auth)
//	/api/activity/stream    SSE mining feed (auth)
//	/api/log/stream         SSE system log, filtered (auth)
//
// Build:  CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -ldflags="-s -w"
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"hash"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"golang.org/x/crypto/blake2b"
)

var (
	listenAddr = flag.String("listen", ":8080", "HTTP listen address")
	lan80      = flag.Bool("lan80", true, "also serve the panel on port 80 of each LAN address (the stock backend keeps 127.0.0.1:80)")
	sshEnable  = flag.Bool("ssh", true, "allow the built-in SSH server (signs in with the panel account; on/off and port in Settings)")
	https443   = flag.Bool("https443", true, "also serve the panel over https on :443 (self-signed, made on first start)")
	apiAddr    = flag.String("api", "127.0.0.1:4028", "stock cgminer/bfgminer API address")
	logPath    = flag.String("log", "/var/log/messages", "system log file to stream")
	authFile   = flag.String("authfile", brandCfgDir+"/auth.json", "auth store path")
	apiTimeout = flag.Duration("api-timeout", 4*time.Second, "cgminer API timeout")
	pollEvery  = flag.Duration("poll", 2*time.Second, "mining-activity poll interval")
	minerWeb   = flag.String("miner-web", "127.0.0.1:80", "stock miner web host:port (for /dbg per-chip data)")
	installUpd = flag.String("install-update", "", "install this firmware update package, then reboot (the panel runs this from a copy in RAM)")
	minerPass  = flag.String("miner-pass", "", "stock miner web password (admin); stored once to authfile")
	bfgPath    = flag.String("bfg", "/usr/config/bfgminer/bfgminer.json", "miner config path (for verbose toggle)")
	shimPath   = flag.String("intminer", "/usr/bin/intminer", "intminer binary path (shim target for persistent -P verbose)")
	fanPath    = flag.String("fan", brandCfgDir+"/fan.json", "fan-curve config path")
	presetPath = flag.String("presets", brandCfgDir+"/presets.json", "saved presets path")
	hrPath     = flag.String("hashrate", brandCfgDir+"/hashrate.json", "hashrate history path")
	netPath    = flag.String("netcfg", brandCfgDir+"/net.json", "network config path (web port)")
	tunerDir   = flag.String("tunerdir", brandCfgDir+"/tuner", "directory for saved tuner run files (log/csv/zip)")
	installSvc = flag.Bool("install-service", false, "install "+brandInitScript+" boot script and exit")
)

// installService writes a Buildroot init script so the agent auto-starts on boot.
func installService() error {
	self, err := filepath.Abs(os.Args[0])
	if err != nil {
		self = os.Args[0]
	}
	script := "#!/bin/sh\n# " + brandAgent + " boot script (auto-generated)\ncase \"$1\" in\n" +
		"  start) [ -x " + self + " ] && " + self + " -listen :8080 -log /var/log/messages -authfile " + *authFile + " >/tmp/agent.out 2>&1 & ;;\n" +
		"  stop) killall " + filepath.Base(self) + " 2>/dev/null ;;\nesac\n"
	// A read-only squashfs root (Box II) cannot be remounted rw and /etc/init.d can't be written: refuse
	// cleanly instead of crashing. On the Box II the panel is started by the firmware's own S48 boot script
	// (built by B8/kit), so there is nothing to install here.
	if isSquashfs("/etc/init.d") || isSquashfs("/") {
		return fmt.Errorf("the root filesystem is a read-only squashfs: the panel's boot script is installed by the firmware image (kit), not at runtime")
	}
	exec.Command("mount", "-o", "remount,rw", "/").Run()
	werr := os.WriteFile(brandInitScript, []byte(script), 0755)
	exec.Command("sync").Run()
	exec.Command("mount", "-o", "remount,ro", "/").Run()
	return werr
}

// ===================== auth =====================

const (
	defaultUser = "admin"
	defaultPass = "admin"
	pbkdfIter   = 60000 // PBKDF2-HMAC-SHA256 rounds (the format up to v1.5.21)
	// PBKDF2-HMAC-BLAKE2b-512 rounds: one costs ~3.4x a SHA-256 round on the ARM CPU, so a sign-in takes about
	// as long as before
	b2Iter     = 20000
	sessionTTL = 24 * time.Hour
)

type authStore struct {
	Username     string `json:"username"`
	Salt         string `json:"salt"` // base64, PBKDF2-HMAC-SHA256 (up to v1.5.21; see setPassword)
	Hash         string `json:"hash"` // base64
	Iter         int    `json:"iter"`
	B2Salt       string `json:"b2_salt,omitempty"` // base64, PBKDF2-HMAC-BLAKE2b-512 (v1.5.22)
	B2Hash       string `json:"b2_hash,omitempty"` // base64
	B2Iter       int    `json:"b2_iter,omitempty"`
	MustChange   bool   `json:"must_change"`
	Secret       string `json:"secret"`                 // base64, cookie-signing key
	MinerPass    string `json:"miner_pass,omitempty"`   // stock web password (admin), for /dbg
	Acknowledged string `json:"acknowledged,omitempty"` // date the risk disclaimer was accepted
}

var (
	auth   authStore
	authMu sync.RWMutex
)

func pbkdf2(password, salt []byte, iter, keyLen int) []byte {
	return pbkdf2With(sha256.New, password, salt, iter, keyLen)
}

func blake2b512() hash.Hash { h, _ := blake2b.New512(nil); return h }

// pbkdf2With: PBKDF2 (RFC 8018) with HMAC over the given hash.
func pbkdf2With(h func() hash.Hash, password, salt []byte, iter, keyLen int) []byte {
	prf := hmac.New(h, password)
	hLen := prf.Size()
	n := (keyLen + hLen - 1) / hLen
	var dk []byte
	var blk [4]byte
	for b := 1; b <= n; b++ {
		prf.Reset()
		prf.Write(salt)
		binary.BigEndian.PutUint32(blk[:], uint32(b))
		prf.Write(blk[:])
		u := prf.Sum(nil)
		t := append([]byte(nil), u...)
		for i := 2; i <= iter; i++ {
			prf.Reset()
			prf.Write(u)
			u = prf.Sum(nil)
			for j := range t {
				t[j] ^= u[j]
			}
		}
		dk = append(dk, t...)
	}
	return dk[:keyLen]
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	rand.Read(b)
	return b
}

// The password is stored as PBKDF2-HMAC-BLAKE2b-512 (v1.5.22; BLAKE2b is the SC Lite's own mining hash). Panels up
// to v1.5.21 only read the SHA-256 fields, so those are written instead while they might come back:
//   - while a firmware update is unconfirmed (the boot script rolls it back by itself if it isn't confirmed);
//   - when the owner rolls back by hand (the panel asks for the password and saves it for the older version).
//
// An owner's SHA-256 hash is replaced at their next sign-in. The two never sit side by side: an older panel
// finds no SHA-256 hash, calls the file damaged and starts over at admin/admin with a forced change, instead
// of locking everyone out (that only happens if an older image is flashed by hand).
func setPassword(a *authStore, pw string) {
	if authLegacyOnly() {
		setPasswordSHA256(a, pw)
		return
	}
	salt := randBytes(16)
	a.B2Salt = base64.StdEncoding.EncodeToString(salt)
	a.B2Iter = b2Iter
	a.B2Hash = base64.StdEncoding.EncodeToString(pbkdf2With(blake2b512, []byte(pw), salt, b2Iter, 64))
	setLegacySentinel(a)
}

// setLegacySentinel: the SHA-256 fields beside a BLAKE2b hash. Left empty, a panel up to v1.5.11 (no check of
// the file) takes ANY password: PBKDF2 to an empty hash is empty and two empty strings compare equal. Instead
// round count 0 (v1.5.12+ call the file damaged and start over at admin/admin, as before) and a hash that panel
// computes for "admin" only (0 rounds = HMAC-SHA256(password, salt||1)): it signs in with the stored user name
// and the password admin (it keeps the user name from the file).
func setLegacySentinel(a *authStore) {
	salt := randBytes(16)
	m := hmac.New(sha256.New, []byte(defaultPass))
	m.Write(salt)
	m.Write([]byte{0, 0, 0, 1})
	a.Salt, a.Hash, a.Iter = base64.StdEncoding.EncodeToString(salt), base64.StdEncoding.EncodeToString(m.Sum(nil)), 0
}

func setPasswordSHA256(a *authStore, pw string) {
	salt := randBytes(16)
	a.Salt = base64.StdEncoding.EncodeToString(salt)
	a.Iter = pbkdfIter
	a.Hash = base64.StdEncoding.EncodeToString(pbkdf2([]byte(pw), salt, pbkdfIter, 32))
	a.B2Salt, a.B2Hash, a.B2Iter = "", "", 0
}

// authLegacyOnly: an update is waiting to be confirmed (the older panel may be put back by itself), a roll back
// is marked (done at the next start; a failed one is tried again), or a password was just saved for an older
// version that is about to be installed or put back.
var authLegacyOnly = func() bool {
	if authKeepLegacy.Load() {
		return true
	}
	for _, f := range []string{"pending", "rollback"} {
		if _, err := os.Stat(filepath.Join(updDir(), f)); err == nil {
			return true
		}
	}
	return false
}

// authKeepLegacy: set by authToLegacy until the roll back / older install is marked on disk (or fails).
var authKeepLegacy atomic.Bool

// setHashFrom: copy the password fields (whichever format) from a freshly made store.
func (a *authStore) setHashFrom(n *authStore) {
	a.Salt, a.Iter, a.Hash = n.Salt, n.Iter, n.Hash
	a.B2Salt, a.B2Iter, a.B2Hash = n.B2Salt, n.B2Iter, n.B2Hash
}

func authAlgo(a *authStore) string {
	if a.B2Hash != "" {
		return "PBKDF2-HMAC-BLAKE2b-512"
	}
	return "PBKDF2-HMAC-SHA256"
}

func checkPassword(pw string) bool {
	authMu.RLock()
	b2 := auth.B2Hash != ""
	saltS, hashS, iter := auth.Salt, auth.Hash, auth.Iter
	if b2 {
		saltS, hashS, iter = auth.B2Salt, auth.B2Hash, auth.B2Iter
	}
	authMu.RUnlock() // the slow hash runs without the lock: nothing else waits on a sign-in
	salt, _ := base64.StdEncoding.DecodeString(saltS)
	want, _ := base64.StdEncoding.DecodeString(hashS)
	if len(want) < 16 || iter <= 0 { // no stored hash: nothing matches (an empty hash would match anything)
		return false
	}
	var got []byte
	if b2 {
		got = pbkdf2With(blake2b512, []byte(pw), salt, iter, len(want))
	} else {
		got = pbkdf2([]byte(pw), salt, iter, len(want))
	}
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return false
	}
	if !b2 && !authLegacyOnly() {
		authUpgradeWG.Add(1)
		go func() { defer authUpgradeWG.Done(); authUpgrade(pw, hashS) }()
	}
	return true
}

var authUpgradeWG sync.WaitGroup // (tests wait for it)

// authUpgrade: the SHA-256 hash the owner just signed in with becomes a BLAKE2b one. Sessions stay signed in.
func authUpgrade(pw, oldHash string) {
	var nh authStore
	setPassword(&nh, pw)
	if nh.B2Hash == "" {
		return
	}
	authMu.Lock()
	// changed meanwhile, or an older version may now come back (a roll back marked during the hash): leave it
	if auth.Hash != oldHash || auth.B2Hash != "" || authLegacyOnly() {
		authMu.Unlock()
		return
	}
	prev := auth
	auth.setHashFrom(&nh)
	authMu.Unlock()
	if err := saveAuth(); err != nil {
		authMu.Lock()
		if auth.B2Hash == nh.B2Hash {
			auth.setHashFrom(&prev)
		}
		authMu.Unlock()
		return
	}
	activity.emit("🔒 the password is now stored as " + authAlgo(&nh))
}

// authToLegacy: before a rollback by hand, the password in the format the older panel reads.
// Until the caller clears authKeepLegacy (on failure), no sign-in moves it back to BLAKE2b.
func authToLegacy(pw string) error {
	var nh authStore
	setPasswordSHA256(&nh, pw)
	authKeepLegacy.Store(true)
	authMu.Lock()
	prev := auth
	auth.setHashFrom(&nh)
	authMu.Unlock()
	if err := saveAuth(); err != nil {
		authMu.Lock()
		auth.setHashFrom(&prev)
		authMu.Unlock()
		authKeepLegacy.Store(false)
		return err
	}
	return nil
}

// authUsable: a login file a password can match (and whose signing key is real). A bad signing key alone is
// replaced (everyone signs in again); a bad hash, salt, round count or name makes the file damaged.
func authUsable(a *authStore) bool {
	ok := func(saltS, hashS string, iter, lo int) bool {
		salt, e1 := base64.StdEncoding.DecodeString(saltS)
		hash, e2 := base64.StdEncoding.DecodeString(hashS)
		return e1 == nil && e2 == nil && len(salt) >= 8 && len(hash) >= 16 && iter >= lo && iter <= 10_000_000
	}
	if a.B2Hash != "" {
		if !ok(a.B2Salt, a.B2Hash, a.B2Iter, 1000) {
			return false
		}
	} else if !ok(a.Salt, a.Hash, a.Iter, 1000) {
		return false
	}
	if strings.TrimSpace(a.Username) == "" {
		return false
	}
	if k, err := base64.StdEncoding.DecodeString(a.Secret); err != nil || len(k) < 16 {
		a.Secret = base64.StdEncoding.EncodeToString(randBytes(32))
	}
	return true
}

func loadAuth() error {
	b, err := os.ReadFile(*authFile)
	unreadable := err != nil && !os.IsNotExist(err) // there, but can't be read (a read error, not a file)
	if err == nil || unreadable {
		var a authStore
		if err == nil && json.Unmarshal(b, &a) == nil && authUsable(&a) {
			authMu.Lock()
			auth = a
			authMu.Unlock()
			authSeen(b) // (sshd_link.go: a change on disk is noticed)
			return nil
		}
		// damaged (e.g. cut short by a power loss) or unreadable: the backup, else the defaults. Never exit: a
		// panel that stops at start-up takes the web page and SSH with it.
		if bb, err := os.ReadFile(*authFile + ".bak"); err == nil && json.Unmarshal(bb, &a) == nil && authUsable(&a) {
			authMu.Lock()
			auth = a
			authMu.Unlock()
			log.Printf("auth: %s was damaged, restored from the backup", *authFile)
			return saveAuth()
		}
		log.Printf("auth: %s is damaged and there's no usable backup: back to the default login", *authFile)
	}
	// first run: create defaults
	entropyReady() // the signing key lasts: not before the kernel's random pool is seeded (access.go)
	a := authStore{Username: defaultUser, MustChange: true,
		Secret: base64.StdEncoding.EncodeToString(randBytes(32))}
	setPassword(&a, defaultPass)
	authMu.Lock()
	auth = a
	authMu.Unlock()
	if unreadable { // a file that couldn't be read may still hold the owner's login: never written over
		return fmt.Errorf("%s can't be read: %v", *authFile, err)
	}
	if err := saveAuth(); err != nil {
		return err
	}
	log.Printf("auth: created default login %q/%q (must change on first login) at %s", defaultUser, defaultPass, *authFile)
	return nil
}

// saveMu: one save at a time. Two at once share auth.json.tmp: one's rename could fail (reported as not
// saved though it was), or an older snapshot land last (a password change lost to a sign-in's upgrade).
var saveMu sync.Mutex

func saveAuth() error {
	saveMu.Lock()
	defer saveMu.Unlock()
	authMu.RLock()
	b, _ := json.MarshalIndent(auth, "", "  ")
	authMu.RUnlock()
	if err := os.MkdirAll(filepath.Dir(*authFile), 0700); err != nil {
		return err
	}
	tmp := *authFile + ".tmp"
	if err := writeFileSync(tmp, b, 0600); err != nil {
		os.Remove(tmp) // (a full partition: the part written goes, not left taking room)
		return err
	}
	if cur, err := os.ReadFile(*authFile); err == nil && json.Valid(cur) { // the last good copy
		writeFileSync(*authFile+".bak", cur, 0600)
	}
	if err := os.Rename(tmp, *authFile); err != nil {
		return err
	}
	fsyncDir(filepath.Dir(*authFile)) // the rename reaches the FAT before we report success (RV5 F2)
	authSeen(b)                       // (sshd_link.go: only someone else's change is reloaded)
	return nil
}

// writeFileSync writes and fsyncs, so a power cut can't leave an empty file behind a rename.
// writeAtomic: a temp file beside it, flushed, then renamed over: a power cut leaves the old file or the
// new one, never half of one.
//
// On the Box II (configFAT, set by initConfigStore) a rename-over is NOT atomic against a power loss and the
// store has no journal, so writeAtomic routes to fatWrite, which also keeps a .bak and fsyncs the directory
// (see fatstore.go). On SC Lite's ext configFAT is false and this is byte-for-byte the pre-port behaviour.
func writeAtomic(path string, b []byte, mode os.FileMode) error {
	if configFAT {
		return fatWrite(path, b, mode)
	}
	return writeAtomicPlain(path, b, mode)
}

// writeAtomicPlain is the original (pre-port) atomic write: temp+fsync, then rename over. Used directly on
// ext (SC Lite) and as the non-FAT branch of cfgWrite.
func writeAtomicPlain(path string, b []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := writeFileSync(tmp, b, mode); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// badJSON: a request body the page wouldn't send (a wrong type, a number too big, cut short, too large) as a
// plain message, not the decoder's Go wording.
func badJSON(err error) string {
	var te *json.UnmarshalTypeError
	var mb *http.MaxBytesError
	switch {
	case errors.As(err, &te) && te.Field != "":
		f := te.Field
		if i := strings.LastIndex(f, "."); i >= 0 {
			f = f[i+1:]
		}
		return "bad request: " + f + " has the wrong kind of value"
	case errors.As(err, &mb):
		return "bad request: too large"
	}
	return "bad request: not a complete settings message"
}

// decodeIn reads a request's JSON into v. An empty body is an empty message; anything else the decoder refuses
// (a wrong type, cut short, too large) is answered here and nothing is changed: the decoder goes on past a wrong
// type and fills the other fields, which must not be acted on by themselves.
func decodeIn(w http.ResponseWriter, r *http.Request, v any) bool {
	err := json.NewDecoder(r.Body).Decode(v)
	if err == nil || err == io.EOF {
		return true
	}
	writeJSON(w, map[string]any{"ok": false, "err": badJSON(err), "error": badJSON(err)})
	return false
}

func writeFileSync(path string, b []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// signed cookie:  base64(user|expiry) . base64(hmac)
func issueCookie(user string) string {
	authMu.RLock()
	secret, _ := base64.StdEncoding.DecodeString(auth.Secret)
	authMu.RUnlock()
	now := cookieNow()
	exp := now.Add(sessionTTL).Unix()
	// a random session id: each sign-in's cookie is its own, so signing out can end just that one (L6)
	payload := fmt.Sprintf("%s|%d|%s", user, exp, base64.RawURLEncoding.EncodeToString(randBytes(9)))
	if now.Year() < 2024 { // the clock is still unset (1970): NTP or the browser will set it soon
		preClockMu.Lock()
		preClockSess[payload] = systemUptime() + sessionTTL.Seconds()
		preClockMu.Unlock()
	}
	sessIssued(payload) // and it lasts sessionTTL of uptime at most, whatever the clock does later (access.go)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(payload))
	sig := mac.Sum(nil)
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func validCookie(v string) bool {
	parts := strings.SplitN(v, ".", 2)
	if len(parts) != 2 {
		return false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	authMu.RLock()
	secret, _ := base64.StdEncoding.DecodeString(auth.Secret)
	curUser := auth.Username
	authMu.RUnlock()
	mac := hmac.New(sha256.New, secret)
	mac.Write(payload)
	if subtle.ConstantTimeCompare(sig, mac.Sum(nil)) != 1 {
		return false
	}
	fields := strings.SplitN(string(payload), "|", 3) // user|expiry|session id (before v0.5: user|expiry)
	if len(fields) < 2 || fields[0] != curUser || cookieRevoked(string(payload)) {
		return false
	}
	exp, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return false
	}
	if now := cookieNow(); now.Year() >= 2024 && exp < time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC).Unix() {
		// issued on the unset 1970 clock, which has been set since (a jump of ~56 years): its expiry counts in
		// this boot's uptime, if this panel issued it (a cookie from another boot or panel run: sign in again)
		preClockMu.Lock()
		upTo, ok := preClockSess[string(payload)]
		preClockMu.Unlock()
		return ok && systemUptime() <= upTo
	}
	if !sessWithinTTL(string(payload), exp) {
		return false
	}
	return cookieNow().Unix() <= exp
}

// Sessions issued while the clock was still unset (the board has no battery clock: 1970 until NTP or the
// browser sets it, and the panel sets it from the browser right after sign-in): payload -> uptime (s) until
// which they're valid. Without this the clock's jump to today expires them at once (signed out right away).
var (
	cookieNow    = time.Now
	preClockMu   sync.Mutex
	preClockSess = map[string]float64{}
)

func isAuthed(r *http.Request) bool {
	authFollowFile(false) // SC Box II: a password changed over SSH (sshd_link.go)
	c, err := r.Cookie("sid")
	if err != nil {
		return false
	}
	return validCookie(c.Value)
}

func requireAuth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !isAuthed(r) {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		// a change only from the panel's own page: SameSite keeps the cookie from other sites, but not from
		// another host of the same site (every <name>.fritz.box is one site: "box" is a public ending), and a
		// form there can post JSON as text/plain
		if r.Method != http.MethodGet && r.Method != http.MethodHead && !changeFromPanel(r) {
			http.Error(w, `{"error":"changes only from the panel's own page"}`, http.StatusForbidden)
			return
		}
		connMarkOwner(remoteIP(r))
		// Until the factory default password is changed, a valid session may touch ONLY the
		// bootstrap endpoints (set password, acknowledge). This closes the window where a miner
		// still on admin/admin could be controlled over the LAN before its owner secures it.
		authMu.RLock()
		mc := auth.MustChange
		authMu.RUnlock()
		if mc && r.URL.Path != "/api/passwd" && r.URL.Path != "/api/acknowledge" {
			http.Error(w, `{"error":"set a new password first"}`, http.StatusForbidden)
			return
		}
		h(w, r)
	}
}

var meAutoPass int32

var freshInstall bool

func handleMe(w http.ResponseWriter, r *http.Request) {
	authMu.RLock()
	needPw := auth.MinerPass == ""
	authMu.RUnlock()
	authed := isAuthed(r)
	if needPw && authed && atomic.CompareAndSwapInt32(&meAutoPass, 0, 1) {
		// a signed-in owner shouldn't be asked for a factory-default password; tried in the background so
		// a slow backend never holds up the page
		go func() { autoMinerPass(); atomic.StoreInt32(&meAutoPass, 0) }()
	}
	authMu.RLock()
	resp := map[string]any{"authenticated": authed, "pw_algo": authAlgo(&auth), "pw_upgrade_waits": authLegacyOnly()}
	if authed { // not to anyone who isn't signed in: half of a login, and whether the factory one still works
		resp["username"] = auth.Username
		resp["must_change"], resp["miner_pass_set"], resp["acknowledged"] = auth.MustChange, auth.MinerPass != "", auth.Acknowledged
	}
	authMu.RUnlock()
	writeJSON(w, resp)
}

func remoteIP(r *http.Request) string {
	h, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return h
}

// loginHostOK: a sign-in only through a name that can be this miner on the local network. A web page on
// some other site can point its own domain name at the miner's address ("DNS rebinding") and then post
// guesses (or admin/admin) to the login as if it were the panel; the browser sends that site's name.
// The IP address, a one-word name, a local-network name or this miner's own host name always work.
func loginHostOK(host string) bool {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.ToLower(strings.Trim(host, "[]")), ".")
	if host == "" || net.ParseIP(host) != nil || !strings.Contains(host, ".") {
		return true
	}
	if host == "fritz.box" {
		return true
	}
	// only endings nobody can register on the internet (.box, say, is a real public ending)
	for _, suf := range []string{".local", ".lan", ".home", ".home.arpa", ".internal", ".localdomain", ".localhost", ".intranet", ".corp", ".private"} {
		if strings.HasSuffix(host, suf) {
			return true
		}
	}
	// the local domain the router gives out by DHCP ("search"/"domain" in resolv.conf): the owner's own network
	for _, d := range localDomains() {
		if strings.HasSuffix(host, "."+d) {
			return true
		}
	}
	return false
}

var resolvConf = "/etc/resolv.conf"

// localDomains: the DHCP-given domain(s) from resolv.conf, lower case; a bare public ending ("com") never counts.
func localDomains() []string {
	b, err := os.ReadFile(resolvConf)
	if err != nil {
		return nil
	}
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Fields(strings.SplitN(l, "#", 2)[0])
		if len(f) < 2 || (f[0] != "search" && f[0] != "domain") {
			continue
		}
		for _, d := range f[1:] {
			d = strings.Trim(strings.ToLower(d), ".")
			if d == "" || len(d) > 100 || strings.ContainsAny(d, "/:[] ") {
				continue
			}
			if !strings.Contains(d, ".") && publicTLD[d] {
				continue
			}
			out = append(out, d)
		}
	}
	return out
}

// publicTLD: common internet endings a router might (wrongly) hand out as its whole domain
var publicTLD = map[string]bool{"com": true, "net": true, "org": true, "io": true, "info": true, "biz": true, "co": true,
	"app": true, "dev": true, "box": true, "uk": true, "de": true, "jp": true, "fr": true, "nl": true, "us": true}

var loginSem = make(chan struct{}, 2)

// postOnly: a change only on POST (a link or an image can't reboot the miner).
func postOnly(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			writeJSON(w, map[string]any{"ok": false, "err": "POST only"})
			return
		}
		h(w, r)
	}
}

// sameOrigin: a request from the panel's own page (a browser sends Origin on a POST; a page elsewhere has
// its own). No Origin: not a browser form from elsewhere (curl, the panel's older pages).
func sameOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	u, err := url.Parse(o)
	return err == nil && strings.EqualFold(u.Host, r.Host)
}

func handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") || !changeFromPanel(r) {
		// a page on another site can post a form here (no CORS check for that): its wrong guesses would block
		// the owner's own address for 10 minutes. The panel's page always sends JSON from its own origin.
		w.WriteHeader(http.StatusForbidden)
		writeJSON(w, map[string]any{"ok": false, "error": "sign in from the panel's own page"})
		return
	}
	if !loginHostOK(r.Host) {
		w.WriteHeader(http.StatusForbidden)
		writeJSON(w, map[string]any{"ok": false, "error": "sign in from the miner's IP address (this name, " + r.Host + ", isn't a local-network name)"})
		return
	}
	var in struct{ Username, Password string }
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil {
		// not a sign-in at all (garbled, or over 64 KiB): not counted as a wrong password
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]any{"ok": false, "error": "bad request"})
		return
	}
	authFollowFile(true) // SC Box II: a password changed over SSH (sshd_link.go)
	authMu.RLock()
	user := auth.Username
	authMu.RUnlock()
	ip := remoteIP(r)
	if !loginAttempt(ip, "panel") {
		w.WriteHeader(http.StatusTooManyRequests)
		msg := "too many sign-ins at once from this address: try again in a moment"
		if sshBlocked(ip) {
			msg = "too many wrong passwords from this address: try again in 10 minutes"
		}
		writeJSON(w, map[string]any{"ok": false, "error": msg})
		return
	}
	loginSem <- struct{}{} // at most two password checks at a time: guesses can't load the miner's CPU
	ok := subtle.ConstantTimeCompare([]byte(in.Username), []byte(user)) == 1
	ok = checkPassword(in.Password) && ok // always run the hash: a wrong name mustn't answer faster (as SSH)
	<-loginSem
	loginDone(ip, "panel", ok)
	if !ok {
		time.Sleep(600 * time.Millisecond) // slow brute force
		w.WriteHeader(http.StatusUnauthorized)
		writeJSON(w, map[string]any{"ok": false, "error": "invalid credentials"})
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "sid", Value: issueCookie(user), Path: "/", Secure: r.TLS != nil,
		HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: int(sessionTTL.Seconds())})
	authMu.RLock()
	mc := auth.MustChange
	authMu.RUnlock()
	writeJSON(w, map[string]any{"ok": true, "must_change": mc})
}

func handleLogout(w http.ResponseWriter, r *http.Request) {
	if secHardened && (r.Method != http.MethodPost || !changeFromPanel(r)) {
		w.WriteHeader(http.StatusForbidden)
		writeJSON(w, map[string]any{"ok": false, "error": "sign out from the panel's own page"})
		return
	}
	if c, err := r.Cookie("sid"); err == nil && c.Value != "" {
		revokeCookie(c.Value) // the cookie no longer signs in, even where it was copied to (L6)
	}
	http.SetCookie(w, &http.Cookie{Name: "sid", Value: "", Path: "/", MaxAge: -1, HttpOnly: true})
	writeJSON(w, map[string]any{"ok": true})
}

// setHostname sets the system hostname live and (best-effort) persists it to /etc/hostname
// through a temporary read-write remount of the rootfs.
func setHostname(h string) error {
	oldName, _ := os.Hostname()
	rememberPrivate([]string{oldName, minerLabel()}) // syslogd keeps writing the old name until the next reboot
	oldRun := tunerRunDir()
	defer func() { // the tuner's files follow the name: move them along (or they'd vanish from the panel)
		if nr := tunerRunDir(); nr != oldRun {
			if _, err := os.Stat(nr); os.IsNotExist(err) {
				os.Rename(oldRun, nr)
			}
		}
	}()
	widePath := "PATH=/usr/sbin:/usr/bin:/sbin:/bin:" + os.Getenv("PATH")
	if rootIsSquashfs() {
		// Box II: root is a read-only squashfs, so `mount -o remount,rw /` fails and writing /etc/hostname is
		// impossible. Set the name live, and persist it to a file on the config partition that the boot script
		// (B8/kit) symlinks /etc/hostname to. Never crash: a live hostname is what matters this boot.
		cmd := exec.Command("sh", "-c", "hostname "+shellQuote(h))
		cmd.Env = append(os.Environ(), widePath)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("%v (%s)", err, strings.TrimSpace(string(out)))
		}
		if err := cfgWrite(hostnameCfgPath(), []byte(h+"\n"), 0o644); err != nil {
			return fmt.Errorf("saving the hostname to the config partition: %v", err)
		}
		return nil
	}
	cmd := exec.Command("sh", "-c", "hostname "+shellQuote(h)+
		"; mount -o remount,rw / 2>/dev/null; printf '%s\\n' "+shellQuote(h)+" > /etc/hostname 2>/dev/null; sync; mount -o remount,ro / 2>/dev/null")
	cmd.Env = append(os.Environ(), widePath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// hostnameCfgPath is where the persisted hostname lives on a squashfs-root miner (Box II): a file on the
// config partition that the boot script symlinks /etc/hostname to (coordinated with B8/kit).
func hostnameCfgPath() string { return filepath.Join(filepath.Dir(*authFile), "hostname") }

// shellQuote single-quotes a string for safe use in sh -c.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

var hostnameFile = "/etc/hostname"

func readFileString(p string) string { b, _ := os.ReadFile(p); return string(b) }

var hostnameRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?$`)

type netConfig struct {
	Port      int    `json:"port"`
	ShareUnit string `json:"share_unit,omitempty"` // datum | miner | both
	HashUnit  string `json:"hash_unit,omitempty"`  // auto | ghs | ths | both
	SSHOff    bool   `json:"ssh_off,omitempty"`    // SSH server switched off in Settings
	SSHPort   int    `json:"ssh_port,omitempty"`   // 0 = 22
	DNS       string `json:"dns,omitempty"`        // owner's DNS override (space/comma-separated IPv4), see netdns.go; never 114.114.114.114
	// SC Box II only (access.go): the panel and SSH from outside the local network; plain http redirected to
	// https; ports 80 and 443 switched off. All off (the zero value) by default.
	AllowOutside bool `json:"allow_outside,omitempty"`
	HTTPSOnly    bool `json:"https_only,omitempty"`
	Port80Off    bool `json:"port80_off,omitempty"`
	Port443Off   bool `json:"port443_off,omitempty"`
}

var (
	shareMu   sync.RWMutex
	shareUnit = "both" // how best-share / share-difficulty numbers are displayed
	hashUnit  = "auto" // how hashrate is displayed: auto-scale | GH/s | TH/s | both
)

func getShareUnit() string { shareMu.RLock(); defer shareMu.RUnlock(); return shareUnit }
func getHashUnit() string  { shareMu.RLock(); defer shareMu.RUnlock(); return hashUnit }

func validShareUnit(u string) bool { return u == "datum" || u == "miner" || u == "both" }
func validHashUnit(u string) bool {
	return u == "auto" || u == "ghs" || u == "ths" || u == "both"
}

func loadNetConfig() {
	b, ok := readConfig(*netPath, json.Valid) // falls back to net.json.bak if the primary is damaged (FAT power cut)
	if !ok {
		return
	}
	var c netConfig
	if json.Unmarshal(b, &c) != nil {
		return
	}
	// the same rules as saving it (an older version saved any port): not the mining program's 4028, not
	// under 1024 (80 is Goldshell's backend, 22/443 are SSH's and https'), not SSH's own
	if c.Port >= 1024 && c.Port <= 65535 && c.Port != 4028 && c.Port != sshPort(c) {
		*listenAddr = fmt.Sprintf(":%d", c.Port)
	} else if c.Port != 0 && c.Port != 8080 {
		go activity.emit(fmt.Sprintf("⚠ the saved web-UI port %d can't be used (it's taken by the miner or SSH, or under 1024): the panel is on :8080", c.Port))
	}
	if validShareUnit(c.ShareUnit) {
		shareMu.Lock()
		shareUnit = c.ShareUnit
		shareMu.Unlock()
	}
	if validHashUnit(c.HashUnit) {
		shareMu.Lock()
		hashUnit = c.HashUnit
		shareMu.Unlock()
	}
}

// currentNetConfig reads the persisted config so a save of one field preserves the others.
func currentNetConfig() netConfig {
	var c netConfig
	if b, err := os.ReadFile(*netPath); err == nil {
		json.Unmarshal(b, &c)
	}
	return c
}

func saveShareUnit(u string) error {
	c := currentNetConfig()
	c.ShareUnit = u
	if err := saveNetConfig(c); err != nil {
		return err
	}
	shareMu.Lock()
	shareUnit = u
	shareMu.Unlock()
	return nil
}

func saveHashUnit(u string) error {
	c := currentNetConfig()
	c.HashUnit = u
	if err := saveNetConfig(c); err != nil {
		return err
	}
	shareMu.Lock()
	hashUnit = u
	shareMu.Unlock()
	return nil
}

// The miner counts best share in units of 2^32 hashes (standard share difficulty); DATUM and the
// mempool count in raw hashes. fmtShareUnit renders a best-share difficulty number per the user's
// chosen display mode: "datum" (×2^32, hash-scale), "miner" (the miner's own number), or "both".
const shareHashUnit = 4294967296.0 // 2^32

func siShare(x float64) string {
	a := math.Abs(x)
	switch {
	case a >= 1e18:
		return fmt.Sprintf("%.2fE", x/1e18)
	case a >= 1e15:
		return fmt.Sprintf("%.2fP", x/1e15)
	case a >= 1e12:
		return fmt.Sprintf("%.2fT", x/1e12)
	case a >= 1e9:
		return fmt.Sprintf("%.2fG", x/1e9)
	case a >= 1e6:
		return fmt.Sprintf("%.2fM", x/1e6)
	case a >= 1e3:
		return fmt.Sprintf("%.2fk", x/1e3)
	default:
		return fmt.Sprintf("%.0f", x)
	}
}

func fmtShareUnit(d float64) string {
	switch getShareUnit() {
	case "datum":
		return siShare(d * shareHashUnit)
	case "miner":
		return siShare(d)
	default:
		return siShare(d*shareHashUnit) + " (" + siShare(d) + ")"
	}
}

func saveNetConfig(c netConfig) error {
	_ = os.MkdirAll(filepath.Dir(*netPath), 0755)
	b, _ := json.MarshalIndent(c, "", " ")
	return cfgWrite(*netPath, b, 0644)
}

// handleNet sets the hostname (live + persisted) and/or saves a new web-UI port (applied on
// the next agent start / reboot — changing it live would drop the current connection).
func handleNet(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Hostname string  `json:"hostname"`
		Port     int     `json:"port"`
		DNS      *string `json:"dns"` // pointer: absent = leave as is; "" = clear the override; else set it
		// SC Box II (access.go): absent = leave as is
		AllowOutside *bool  `json:"allow_outside"`
		HTTPSOnly    *bool  `json:"https_only"`
		Port80       *bool  `json:"port80"`
		Port443      *bool  `json:"port443"`
		Password     string `json:"password"` // the panel password: switching allow_outside on asks for it again
	}
	if !decodeIn(w, r, &in) {
		return
	}
	if in.AllowOutside != nil || in.HTTPSOnly != nil || in.Port80 != nil || in.Port443 != nil {
		handleAccess(w, r, in.AllowOutside, in.HTTPSOnly, in.Port80, in.Port443, in.Password)
		return
	}
	// validate the DNS override up front (before anything is changed): IPv4 resolvers only, and never the
	// Chinese static-IP default minerd ships (114.114.114.114) — see netdns.go.
	var dnsClean *string
	if in.DNS != nil {
		clean, err := cleanDNSList(*in.DNS)
		if err != nil {
			writeJSON(w, map[string]any{"ok": false, "err": err.Error()})
			return
		}
		dnsClean = &clean
	}
	// check everything before changing anything: a refused port mustn't leave the name changed behind an error
	h := strings.TrimSpace(in.Hostname)
	if h != "" {
		if !hostnameRe.MatchString(h) {
			writeJSON(w, map[string]any{"ok": false, "err": "hostname must be 1–63 letters, digits or hyphens (not starting/ending with a hyphen)"})
			return
		}
		if h == "localhost" { // dhcpcd's hostname hook treats it as "no name": the router's name replaces it at the next lease
			writeJSON(w, map[string]any{"ok": false, "err": "\"localhost\" can't be this miner's name: pick another"})
			return
		}
	}
	if in.Port != 0 {
		if in.Port < 1024 || in.Port > 65535 {
			writeJSON(w, map[string]any{"ok": false, "err": "port must be 1024–65535 (8080 is the default)"})
			return
		}
		switch in.Port {
		case 4028, sshPort(currentNetConfig()):
			writeJSON(w, map[string]any{"ok": false, "err": fmt.Sprintf("port %d is used by something else on the miner: pick another (8080 is the default)", in.Port)})
			return
		}
		if in.Port != portOf(*listenAddr) { // must be free now, or the panel would have to fall back after a reboot
			if l, err := net.Listen("tcp", fmt.Sprintf(":%d", in.Port)); err != nil {
				writeJSON(w, map[string]any{"ok": false, "err": fmt.Sprintf("port %d can't be opened: %v", in.Port, err)})
				return
			} else {
				l.Close()
			}
		}
	}
	var notes []string
	if h != "" {
		if cur, _ := os.Hostname(); h == cur && strings.TrimSpace(readFileString(hostnameFile)) == h {
			// the Save button sends the name field every time: nothing to do (no remount of the system partition)
		} else if err := setHostname(h); err != nil {
			notes = append(notes, "hostname set live but may not persist: "+err.Error())
		} else {
			notes = append(notes, "hostname updated")
			activity.emit("🖧 hostname set to " + h)
		}
	}
	if in.Port != 0 {
		c := currentNetConfig()
		c.Port = in.Port
		if err := saveNetConfig(c); err != nil {
			writeJSON(w, map[string]any{"ok": false, "err": "could not save port: " + err.Error() + strings.Join(append([]string{""}, notes...), " · ")})
			return
		}
		notes = append(notes, fmt.Sprintf("web-UI port saved as %d — reboot (or restart the agent) to apply, then use :%d", in.Port, in.Port))
	}
	if dnsClean != nil {
		c := currentNetConfig()
		if c.DNS != *dnsClean {
			c.DNS = *dnsClean
			if err := saveNetConfig(c); err != nil {
				writeJSON(w, map[string]any{"ok": false, "err": "could not save DNS: " + err.Error() + strings.Join(append([]string{""}, notes...), " · ")})
				return
			}
			if *dnsClean == "" {
				notes = append(notes, "DNS override cleared (the router's/DHCP servers are used)")
				activity.emit("🖧 DNS override cleared")
			} else {
				notes = append(notes, "DNS set to "+*dnsClean)
				activity.emit("🖧 DNS set to " + *dnsClean)
			}
			// apply now so the owner's choice (or the gateway, when cleared) takes effect without a reboot
			if err := ensureSaneDNS(c); err != nil {
				notes = append(notes, "DNS saved but resolv.conf couldn't be written now: "+err.Error())
			}
		}
	}
	if len(notes) == 0 {
		notes = append(notes, "nothing to change")
	}
	cfg := currentNetConfig()
	writeJSON(w, map[string]any{"ok": true, "note": strings.Join(notes, " · "),
		"dns": cfg.DNS, "dns_active": activeResolvers(), "port_saved": cfg.Port})
}

// A restart/reboot takes the mining engine (and, for a full reboot, the whole box) down for a
// minute or two. During that window the cgminer API at :4028 refuses connections, which would
// otherwise surface on the dashboard as a raw "dial tcp ... connection refused". markReboot opens
// a grace window so buildDashboard can show a calm "restarting" message instead.
var (
	rebootMu    sync.Mutex
	rebootStart time.Time
	rebootTotal time.Duration // nominal duration (drives the progress bar)
	rebootUntil time.Time     // hard cap: keep showing "restarting" until here even if the bar is full
	rebootLabel string
)

// markReboot opens a restart grace window. total paces the bar; cap is how long we keep showing the
// restarting state while the miner is unreachable (generous — a stuck restart falls back to the
// plain unreachable message only after cap). buildDashboard clears it the moment the miner answers.
func markReboot(total, cap time.Duration, label string) {
	rebootMu.Lock()
	rebootStart = time.Now()
	rebootTotal = total
	rebootUntil = rebootStart.Add(cap)
	rebootLabel = label
	rebootMu.Unlock()
}

// rebootActive reports the restart label and a 0..1 progress fraction (held below 1 until the miner
// actually answers again), or ok=false when no restart window is open.
func rebootActive() (string, float64, bool) {
	rebootMu.Lock()
	defer rebootMu.Unlock()
	if rebootUntil.IsZero() || !time.Now().Before(rebootUntil) {
		return "", 0, false
	}
	p := 0.02
	if rebootTotal > 0 {
		p = time.Since(rebootStart).Seconds() / rebootTotal.Seconds()
	}
	if p > 0.95 {
		p = 0.95 // hold near-full until the miner is back
	}
	if p < 0.02 {
		p = 0.02
	}
	return rebootLabel, p, true
}

// clearReboot ends the restarting state (called when the miner answers again).
func clearReboot() {
	rebootMu.Lock()
	if rebootUntil.IsZero() {
		rebootMu.Unlock()
		return
	}
	rebootUntil = time.Time{}
	rebootEnded = time.Now()
	rebootMu.Unlock()
}

// rebootEnded: when the last restart window closed because the miner answered (the poll loop's restart
// check runs after that: the uptime drop it sees is the panel's restart, not one "on its own").
var rebootEnded time.Time

// clearRebootRun: the miner answered with this mining uptime (s; 0 = not known). The run that answers in the
// first seconds after a restart was asked is the old one, still on its way down: the window stays open for it.
func clearRebootRun(elapsed int) {
	rebootMu.Lock()
	open := !rebootUntil.IsZero() && elapsed > 0 && time.Duration(elapsed)*time.Second > time.Since(rebootStart)+10*time.Second
	rebootMu.Unlock()
	if !open {
		clearReboot()
	}
}

// panelRestartedRecently: a restart or reboot the panel made is under way or ended in the last 2 minutes.
func panelRestartedRecently() bool {
	if _, _, ok := rebootActive(); ok {
		return true
	}
	rebootMu.Lock()
	defer rebootMu.Unlock()
	return !rebootEnded.IsZero() && time.Since(rebootEnded) < 2*time.Minute
}

// handleDisplay saves display preferences: the best-share mode (datum | miner | both) and/or the
// hashrate unit (auto | ghs | ths | both). Either field may be sent on its own.
func handleDisplay(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ShareUnit string `json:"share_unit"`
		HashUnit  string `json:"hash_unit"`
	}
	if !decodeIn(w, r, &in) {
		return
	}
	if in.HashUnit != "" {
		if !validHashUnit(in.HashUnit) {
			writeJSON(w, map[string]any{"ok": false, "err": "hash_unit must be auto, ghs, ths or both"})
			return
		}
		if err := saveHashUnit(in.HashUnit); err != nil {
			writeJSON(w, map[string]any{"ok": false, "err": "could not save: " + err.Error()})
			return
		}
		writeJSON(w, map[string]any{"ok": true, "hash_unit": in.HashUnit})
		return
	}
	if !validShareUnit(in.ShareUnit) {
		writeJSON(w, map[string]any{"ok": false, "err": "share_unit must be datum, miner or both"})
		return
	}
	if err := saveShareUnit(in.ShareUnit); err != nil {
		writeJSON(w, map[string]any{"ok": false, "err": "could not save: " + err.Error()})
		return
	}
	writeJSON(w, map[string]any{"ok": true, "share_unit": in.ShareUnit})
}

// softRestartMining restarts mining through the stock firmware's PUT /mcb/restart. In minerd 2.2.0 that
// handler is Goldshell's "Restart miner": sync(), then system("reboot"), a whole-miner reboot (the panel
// with it); minerd has no GET /mcb/restart. So it's noted as a reboot the panel asked for (not "the miner
// restarted without the panel asking"), and the note goes again when the backend refused it.
func softRestartMining() error {
	markCleanReboot()
	if _, err := stockReq("PUT", "/mcb/restart", []byte{}); err != nil {
		os.Remove(recCleanPath())
		return err
	}
	return nil
}

// handleRestart: "Restart mining" via the stock firmware's PUT /mcb/restart. Goldshell's minerd answers it with
// sync() and system("reboot"): on this firmware it is a full reboot (see softRestartMining).
func handleRestart(w http.ResponseWriter, r *http.Request) {
	if getCharge() == "tuner" {
		writeJSON(w, map[string]any{"ok": false, "err": "the tuner is running — stop it first"})
		return
	}
	err := softRestartMining()
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "err": "restart failed: " + err.Error()})
		return
	}
	recordMark("R", "restart mining (reboot)")
	activity.emit("↻ restart mining: the miner reboots (" + oemOf() + " restart), back in 1–2 minutes")
	markReboot(150*time.Second, 5*time.Minute, "Restarting mining: the miner reboots — it will be back in a minute or two.")
	writeJSON(w, map[string]any{"ok": true, "note": "restarting mining: the miner reboots, back in 1–2 minutes"})
}

// handleReboot triggers a full OS reboot of the miner.
func handleReboot(w http.ResponseWriter, r *http.Request) {
	activity.emit("⟳ full reboot requested")
	recordMark("RB", "full reboot")
	markCleanReboot()
	markReboot(150*time.Second, 6*time.Minute, "Rebooting the miner — it will be back in a minute or two.")
	go func() {
		time.Sleep(600 * time.Millisecond) // let the HTTP response flush first
		cmd := exec.Command("sh", "-c", "sync; "+rebootCmd())
		cmd.Env = append(os.Environ(), "PATH=/usr/sbin:/usr/bin:/sbin:/bin:"+os.Getenv("PATH"))
		_ = cmd.Run()
	}()
	writeJSON(w, map[string]any{"ok": true, "note": "rebooting the miner — it'll be back in 1–2 minutes"})
}

// handleAck records that the user accepted the risk disclaimer.
func handleAck(w http.ResponseWriter, r *http.Request) {
	authMu.Lock()
	if auth.Acknowledged == "" {
		auth.Acknowledged = time.Now().Format("2006-01-02 15:04")
	}
	when := auth.Acknowledged
	authMu.Unlock()
	_ = saveAuth()
	writeJSON(w, map[string]any{"ok": true, "acknowledged": when})
}

func handlePasswd(w http.ResponseWriter, r *http.Request) {
	var in struct {
		New, Username, MinerPass, Current string
	}
	if !decodeIn(w, r, &in) {
		return
	}
	changingPw := in.New != ""
	authMu.RLock()
	mustChange := auth.MustChange
	authMu.RUnlock()
	if changingPw && len(in.New) < 8 {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]any{"ok": false, "error": "password must be at least 8 characters"})
		return
	}
	if !changingPw && mustChange {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]any{"ok": false, "error": "set a password to continue"})
		return
	}
	user := strings.TrimSpace(in.Username) // optional: keep current if blank
	if user != "" && (len(user) < 3 || len(user) > 32 || strings.ContainsAny(user, " \t:|")) {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]any{"ok": false, "error": "username must be 3–32 chars, no spaces or : |"})
		return
	}
	// changing the login (not the forced first change) needs the current password: a session alone (a browser
	// left signed in) can't take the panel and SSH away from the owner
	authMu.RLock()
	renaming := user != "" && user != auth.Username
	authMu.RUnlock()
	if !mustChange && (changingPw || renaming) {
		ip := remoteIP(r)
		if in.Current == "" {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]any{"ok": false, "error": "enter your current password to change the username or password"})
			return
		}
		if !loginAttempt(ip, "panel") {
			w.WriteHeader(http.StatusTooManyRequests)
			writeJSON(w, map[string]any{"ok": false, "error": "too many wrong passwords from this address: try again in 10 minutes"})
			return
		}
		loginSem <- struct{}{}
		ok := checkPassword(in.Current)
		<-loginSem
		loginDone(ip, "panel", ok)
		if !ok {
			w.WriteHeader(http.StatusForbidden)
			writeJSON(w, map[string]any{"ok": false, "error": "the current password isn't right: nothing was changed"})
			return
		}
	}
	var nh authStore
	if changingPw {
		setPassword(&nh, in.New) // the slow hash, before taking the lock
	}
	authMu.Lock()
	prev := auth // put back if it can't be saved: "nothing was changed" must hold (old password, this session)
	if user != "" {
		auth.Username = user
	}
	// optional: set the stock miner web password (needed for chip map / controls / fan on a
	// freshly flashed unit). Blank keeps whatever is stored.
	if mp := strings.TrimSpace(in.MinerPass); mp != "" {
		auth.MinerPass = mp
		jwtMu.Lock()
		jwtToken = "" // force a fresh stock login with the new password
		jwtMu.Unlock()
	}
	if changingPw {
		auth.setHashFrom(&nh)
		// a new password signs out every other session (and any stolen cookie): new signing key
		auth.Secret = base64.StdEncoding.EncodeToString(randBytes(32))
	}
	auth.MustChange = false
	newUser := auth.Username
	authMu.Unlock()
	if err := saveAuth(); err != nil {
		authMu.Lock()
		auth = prev
		authMu.Unlock()
		w.WriteHeader(http.StatusInternalServerError)
		writeJSON(w, map[string]any{"ok": false, "error": "could not save (nothing was changed): " + err.Error()})
		return
	}
	if changingPw { // SSH connections signed in with the old password go too (as the panel's sessions do)
		forgetRevoked() // the old key's sessions are all gone now: no list of signed-out ones needed
		if n := sshDropAll(); n > 0 {
			activity.emit(fmt.Sprintf("🔑 password changed: %d SSH connection(s) closed", n))
		}
	}
	// re-issue the session cookie so it reflects the (possibly new) username
	http.SetCookie(w, &http.Cookie{Name: "sid", Value: issueCookie(newUser), Path: "/", Secure: r.TLS != nil,
		HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: int(sessionTTL.Seconds())})
	writeJSON(w, map[string]any{"ok": true, "username": newUser})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// ===================== stock API client =====================

func cgminerCmd(command string) ([]byte, error) {
	d := net.Dialer{Timeout: *apiTimeout}
	conn, err := d.Dial("tcp", *apiAddr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(*apiTimeout))
	if _, err := fmt.Fprintf(conn, `{"command":"%s"}`, command); err != nil {
		return nil, err
	}
	out, done, err := apiRead(conn)
	if !done && err != nil { // cut off (a reset, the timeout, too long): half a reply isn't the answer
		return nil, fmt.Errorf("the mining program's API answer was cut off (%v)", err)
	}
	out = []byte(strings.TrimRight(string(out), "\x00"))
	if len(out) == 0 {
		return nil, fmt.Errorf("the mining program's API gave no answer")
	}
	return out, nil
}

// apiReplyMax: the most of one API reply the panel reads (its largest real replies are a few tens of KB).
const apiReplyMax = 4 << 20

// apiRead reads one API reply: up to the NUL the miner ends it with, or the end of the connection.
// done = the whole reply was read; else err says why not (a reset, the timeout, a runaway reply).
func apiRead(conn net.Conn) (out []byte, done bool, err error) {
	buf := make([]byte, 8192)
	for {
		n, rerr := conn.Read(buf)
		if n > 0 {
			out = append(out, buf[:n]...)
			if buf[n-1] == 0 {
				return out, true, nil
			}
			if len(out) > apiReplyMax {
				return out[:apiReplyMax], false, fmt.Errorf("reply longer than %d bytes", apiReplyMax)
			}
		}
		if rerr == io.EOF {
			return out, true, nil
		}
		if rerr != nil {
			return out, false, rerr
		}
	}
}

// apiClean: a reply as the JSON decoder can read it: without the NUL at the end and, only when it isn't
// valid JSON as it is (a valid reply is never rewritten, so a pool's address or account stays exactly as
// sent), with the "}{" older cgminer/bfgminer builds print between entries made "},{" and a bare nan/inf
// (C's printf of 0/0) made 0.
func apiClean(raw []byte) []byte {
	raw = bytes.TrimRight(raw, "\x00 \t\r\n")
	if json.Valid(raw) {
		return raw
	}
	raw = bytes.ReplaceAll(raw, []byte("}{"), []byte("},{"))
	for i := 0; i < 2; i++ { // twice: a match takes the comma the next one ([nan,nan]) starts with
		raw = apiNanRe.ReplaceAll(raw, []byte("${1}0${2}"))
	}
	return raw
}

var apiNanRe = regexp.MustCompile(`([:\[,]\s*)[-+]?(?i:nan|inf(?:inity)?)(\s*[,\]}])`)

// apiNum: a number in an API entry: a JSON number, or a number sent as a string; 0 for anything else
// (also nan/inf).
func apiNum(e map[string]any, k string) float64 {
	var f float64
	switch v := e[k].(type) {
	case float64:
		f = v
	case string:
		f, _ = strconv.ParseFloat(strings.TrimSpace(v), 64)
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0
	}
	return f
}

// apiInt: apiNum as an int, held within int's range (32 bits on the miner's ARM).
func apiInt(e map[string]any, k string) int {
	f := apiNum(e, k)
	if mx := float64(int(^uint(0) >> 1)); f > mx {
		return int(^uint(0) >> 1)
	} else if f < -mx {
		return -int(^uint(0)>>1) - 1
	}
	return int(f)
}

func apiStr(e map[string]any, k string) string { s, _ := e[k].(string); return s }

// apiEntries: every entry of a section ("SUMMARY", "DEVS", "POOLS", …) of a reply; ok = the reply could
// be read and has the section.
func apiEntries(raw []byte, section string) ([]map[string]any, bool) {
	var top map[string]json.RawMessage
	if json.Unmarshal(apiClean(raw), &top) != nil {
		return nil, false
	}
	var arr []map[string]any
	if json.Unmarshal(top[section], &arr) != nil || arr == nil {
		return nil, false
	}
	return arr, true
}

// currentMHS: the miner's recent hashrate for entry idx of section (SUMMARY or DEVS), from its rolling
// "MHS <n>s" / "MHS 1m" figures, or 0 if it reports none. "MHS av" is the average since mining started:
// after a clock change it takes hours to show it, so the panel and the tuner use the recent one.
// ---- live hashrate from the total-work counter ----
//
// The miner's own hashrate figures are averages that reach back a long way, and a clock change doesn't
// reset them: after a change the Live tile drifted for many minutes. Its total-work counter ("Total MH")
// doesn't lag: the work done over the last ~2 minutes, divided by the time, is the hashrate now.

type workSample struct {
	t time.Time
	w float64 // MH
}

type workTrack struct {
	mu sync.Mutex
	s  map[string][]workSample
}

var liveWork = &workTrack{s: map[string][]workSample{}}

const (
	liveWorkWindow = 2 * time.Minute
	liveWorkMin    = 30 * time.Second // less than this since the start (or a miner restart): use the miner's figure
)

// rate adds a reading of key's counter and returns its hashrate over the last ~2 minutes (GH/s).
var liveNow = time.Now

func (wt *workTrack) rate(key string, w float64) (float64, bool) {
	now := liveNow()
	wt.mu.Lock()
	defer wt.mu.Unlock()
	ss := wt.s[key]
	if n := len(ss); n > 0 && (w < ss[n-1].w || now.Sub(ss[n-1].t) > liveWorkWindow) {
		ss = nil // the counter restarted, or the readings stopped for a while (an Idle, a board that stopped
		// hashing): a base from before the gap would average the stopped time in for minutes
	}
	if n := len(ss); n == 0 || now.Sub(ss[n-1].t) >= 2*time.Second {
		ss = append(ss, workSample{now, w})
	}
	for len(ss) > 2 && now.Sub(ss[1].t) >= liveWorkWindow { // keep the oldest sample at least 2 min back
		ss = ss[1:]
	}
	wt.s[key] = ss
	dt := now.Sub(ss[0].t)
	if dt < liveWorkMin || (w <= ss[0].w && dt < liveWorkWindow) {
		// (a counter that hasn't moved yet in a short window may just update every few seconds: "stopped"
		// only after the whole window without work)
		return 0, false
	}
	return (w - ss[0].w) / dt.Seconds() / 1000, true
}

// workOf: section[idx]'s total work since mining started, MH: "Total MH", or its average since the start
// times the time since the start (the same number).
// apiEntry: one entry of a section ("SUMMARY", "DEVS", …) of a cgminer API reply. The reply also has
// "id": 1, a number, so it can't be decoded straight into a map of arrays: Go's decoder then reports an
// error for "id" (while filling in the rest), which threw away every reading until v1.5.5.
func apiEntry(raw []byte, section string, idx int) (map[string]any, bool) {
	arr, ok := apiEntries(raw, section)
	if !ok || idx < 0 || idx >= len(arr) {
		return nil, false
	}
	return arr[idx], true
}

func workOf(raw []byte, section string, idx int) (float64, bool) {
	e, ok := apiEntry(raw, section, idx)
	if !ok {
		return 0, false
	}
	return workOfEntry(e, section)
}

// workOfEntry: workOf for an entry already read from its reply.
func workOfEntry(e map[string]any, section string) (float64, bool) {
	num := func(k string) float64 { return apiNum(e, k) }
	if v := num("Total MH"); v > 0 {
		return v, true
	}
	if section == "SUMMARY" {
		if av, el := num("MHS av"), num("Elapsed"); av > 0 && el > 0 {
			return av * el, true
		}
	}
	return 0, false
}

func currentMHS(raw []byte, section string, idx int) float64 {
	e, ok := apiEntry(raw, section, idx)
	if !ok {
		return 0
	}
	return currentMHSOf(e)
}

// currentMHSOf: currentMHS for an entry already read from its reply.
func currentMHSOf(e map[string]any) float64 {
	num := func(k string) float64 { return apiNum(e, k) }
	for _, k := range []string{"MHS 20s", "MHS 30s", "MHS 15s", "MHS 10s", "MHS 1m", "MHS 5s"} {
		if v := num(k); v > 0 {
			return v
		}
	}
	for k := range e {
		if mhsKeyRe.MatchString(k) {
			if v := num(k); v > 0 {
				return v
			}
		}
	}
	return num("MHS 5m")
}

var mhsKeyRe = regexp.MustCompile(`^MHS [0-9]+s$`)

var fanKeyRe = regexp.MustCompile(`^fan([0-9]+)$`)

// boardFanRPMs: a board's fan readings, one per `fan%d` key actually present in the devs entry, in
// numeric order. Different hardware reports a different number of fans (SC Lite: fan0-3; SC Box II:
// fan0/fan1 only), so the list follows the reply instead of a fixed 0-3 — a fan the miner doesn't
// report is omitted, never shown as a zero-RPM (stopped) fan. A present fan that reads -1 (a board that
// is off) is kept, so the count stays stable while a board is down.
func boardFanRPMs(dev map[string]any) []int {
	idxs := make([]int, 0, len(dev))
	for k := range dev {
		if m := fanKeyRe.FindStringSubmatch(k); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil {
				idxs = append(idxs, n)
			}
		}
	}
	sort.Ints(idxs)
	fans := make([]int, 0, len(idxs))
	for _, n := range idxs {
		fans = append(fans, apiInt(dev, fmt.Sprintf("fan%d", n)))
	}
	return fans
}

type board struct {
	ID       int     `json:"id"`
	Alive    bool    `json:"alive"`
	Enabled  bool    `json:"enabled"`
	GHS      float64 `json:"ghs"`
	HWErrors int     `json:"hw_errors"`
	HWPct    float64 `json:"hw_pct"`
	ClockMHz float64 `json:"clock_mhz"`
	VoltageV float64 `json:"voltage_v"`
	// VoltLevel: Box II only. Its devs "voltage" field is a PWM-plan trim LEVEL, not a board millivolt
	// reading, so it is never divided, smoothed or fed to the power model; it is carried here as-is for
	// display and left 0 (omitted) on models whose "voltage" really is a board voltage (SC Lite).
	VoltLevel int       `json:"volt_level,omitempty"`
	Temps     []float64 `json:"temps"`
	Fans      []int     `json:"fans"`
	Overheat  bool      `json:"overheat"`
	Nonced    int       `json:"nonced"`
}

type dashboard struct {
	OK         bool               `json:"ok"`
	Err        string             `json:"err,omitempty"`
	Restarting bool               `json:"restarting,omitempty"`
	NoPool     bool               `json:"no_pool,omitempty"`
	Starting   bool               `json:"starting,omitempty"` // just after a boot or a mining restart: the mining software is starting
	Idle       bool               `json:"idle,omitempty"`     // the miner is on its idle plan: not hashing, by choice
	RebootPct  float64            `json:"reboot_pct,omitempty"`
	GHS        float64            `json:"ghs"`     // now: the boards' work over the last ~30 s – 2 min
	GHSAvg     float64            `json:"ghs_avg"` // the miner's average since mining (re)started
	Accepted   int                `json:"accepted"`
	Rejected   int                `json:"rejected"`
	Stale      int                `json:"stale"`
	HWErrors   int                `json:"hw_errors"`
	HWPct      float64            `json:"hw_pct"`
	BestShare  float64            `json:"best_share"`
	BestShareT string             `json:"best_share_fmt"`
	ShareUnit  string             `json:"share_unit"`
	HashUnit   string             `json:"hash_unit"`
	InCharge   string             `json:"in_charge,omitempty"`
	Build      string             `json:"build,omitempty"`      // this panel's page build: an open page reloads when it changes
	Commission string             `json:"commission,omitempty"` // the commissioned features this panel run applied (commissionSig; "" on the SC Lite)
	Elapsed    int                `json:"elapsed"`
	Boards     []board            `json:"boards"`
	Watchdog   *watchdogView      `json:"watchdog,omitempty"`           // Goldshell's watchdog: fan RPM and the fans it flags
	Warn       string             `json:"warn,omitempty"`               // shown above the Live tiles while mining still answers
	FanLock    *fanLockState      `json:"fan_lock,omitempty"`           // mining locked after a fan problem (fanlock.go)
	Health     []boardHealth      `json:"board_health,omitempty"`       // per-board start-up chips, re-inits, dropped results (boardhealth.go)
	Unexpected *unexpectedRestart `json:"unexpected_restart,omitempty"` // restarted without the panel asking (recovery.go)
	OverTemp   *overTempState     `json:"over_temp,omitempty"`          // Goldshell shut the boards down for heat (overtemp.go)
}

var (
	engineOKMu sync.Mutex
	engineOKAt time.Time // the mining engine last answered
)

func engineSeen() { engineOKMu.Lock(); engineOKAt = time.Now(); engineOKMu.Unlock() }
func engineLastOK() time.Time {
	engineOKMu.Lock()
	defer engineOKMu.Unlock()
	return engineOKAt
}

// minerIsIdleNow: put in Idle by the panel, or on the firmware's idle plan (set some other way).
func minerIsIdleNow() bool {
	if getCharge() == "idle" || box2SoftIdled() {
		return true
	}
	raw, err := stockGet("/mcb/setting")
	if err != nil {
		return false
	}
	var cur map[string]any
	return json.Unmarshal(raw, &cur) == nil && minerIdle(cur)
}

// smoothVolt: a board's voltage reading, the median of its last minute. The miner's reading sometimes dips
// for a second or two (8.80 V for a moment while a multimeter on the supply stays at 9.09): one such reading
// shouldn't show on the card, move the watts, or look like the voltage fell.
var (
	voltHistMu sync.Mutex
	voltHist   = map[int][]voltSample{}
)

type voltSample struct {
	t time.Time
	v float64
}

func smoothVolt(id int, v float64) float64 {
	if v <= 0 {
		return v
	}
	voltHistMu.Lock()
	defer voltHistMu.Unlock()
	now := time.Now()
	h := append(voltHist[id], voltSample{now, v})
	for len(h) > 1 && now.Sub(h[0].t) > time.Minute {
		h = h[1:]
	}
	if len(h) > 30 {
		h = h[len(h)-30:]
	}
	voltHist[id] = h
	vals := make([]float64, len(h))
	for i, x := range h {
		vals[i] = x.v
	}
	sort.Float64s(vals)
	return vals[len(vals)/2]
}

func buildDashboard() dashboard {
	d := buildDashboard0()
	if s, ok := loadFanLock(); ok {
		d.FanLock = &s
	}
	d.Health = boardHealthView()
	if t, _, ok := maxBoardTemp(d); ok {
		noteHottest(t)
	}
	d.Unexpected = unexpectedForDashboard()
	d.OverTemp = overTempView()
	return d
}

func buildDashboard0() dashboard {
	var d dashboard
	sRaw, err := cgminerCmd("summary")
	if err != nil {
		if label, pct, ok := rebootActive(); ok {
			return dashboard{OK: false, Restarting: true, Err: label, RebootPct: pct}
		}
		// a fresh setup has no pool, and the stock launcher doesn't start the miner without one
		if minerIsIdleNow() {
			if h, held := poolHold(); held {
				return dashboard{OK: false, Idle: true, Err: "The miner is held on Idle by the pool guard: a pool that isn't in your Pools tab kept coming back (" + h.What + "). Check the Pools tab, then Resume there." + idleActionNote()}
			}
			if heatIdleHeld() {
				return dashboard{OK: false, Idle: true, Err: "The thermal guard put the miner on Idle (" + heatIdleWhyNow() + "): the hash boards are off. " + heatCoolLine() + idleActionNote()}
			}
			return dashboard{OK: false, Idle: true, Err: "The miner is idle: the hash boards are off and it isn't mining. Apply " + stockWord() + " or any preset to start mining again." + idleActionNote()}
		}
		if noPoolSet() {
			return dashboard{OK: false, NoPool: true, Err: "No pool set yet. Add one in the Pools tab and mining starts on its own (about a minute)."}
		}
		if overTempView() != nil { // the boards were shut down for heat: not "restarting" (the banner says why)
			return dashboard{OK: false, Err: "The hash boards are off after an over-temperature shutdown (see above): they stay off until the miner reboots."}
		}
		if systemUptime() < 300 {
			return dashboard{OK: false, Starting: true, Err: "The miner is starting up: its mining software takes a minute or two after a boot. This page updates by itself."}
		}
		if t := engineLastOK(); !t.IsZero() && time.Since(t) < 3*time.Minute {
			return dashboard{OK: false, Starting: true, Err: "The mining software stopped answering a moment ago. After a pool or setting change it restarts, which takes about a minute; if it isn't back in 3 minutes this page says why. This page updates by itself."}
		}
		if msg := miningDownMessage(time.Now()); msg != "" {
			return dashboard{OK: false, Err: msg}
		}
		return dashboard{OK: false, Err: "The mining software isn't answering (API :4028) and hasn't for a few minutes. Try Settings → Restart mining; the Log tab (Mining) usually shows why."}
	}
	engineSeen()
	x, sumOK := apiEntry(sRaw, "SUMMARY", 0)
	if sumOK {
		clearRebootRun(apiInt(x, "Elapsed")) // the miner answered: a restart window is over (unless it's the run before it)
	} else {
		clearReboot()
	}
	if sumOK {
		mhsAv := apiNum(x, "MHS av")
		d.GHS = mhsAv / 1000.0
		d.GHSAvg = mhsAv / 1000.0
		if r := currentMHSOf(x); r > 0 {
			d.GHS = r / 1000.0
		}
		if w, ok := workOfEntry(x, "SUMMARY"); ok {
			if g, ok := liveWork.rate("all", w); ok {
				d.GHS = g // the work done over the last ~2 minutes: no lag after a clock change, and 0 when the
				// work stopped (the miner's rolling figure then stays where it was, its average stays above 0)
			}
		}
		d.Accepted, d.Rejected, d.HWErrors, d.Stale = apiInt(x, "Accepted"), apiInt(x, "Rejected"), apiInt(x, "Hardware Errors"), apiInt(x, "Stale")
		d.HWPct, d.BestShare, d.Elapsed = apiNum(x, "Device Hardware%"), apiNum(x, "Best Share"), apiInt(x, "Elapsed")
	} else {
		// it answered, but not with a summary the panel can read: not a reading of zeros (uptime 0 looks like
		// a restart, and the next good reading like a burst of new shares)
		return dashboard{OK: false, Err: "The mining software gave an answer the panel couldn't read. This page updates by itself."}
	}
	if vRaw, err := cgminerCmd("devs"); err == nil {
		if devs, ok := apiEntries(vRaw, "DEVS"); ok {
			for _, dev := range devs { // (each entry read from the one parse: not the whole reply again per board)
				id := apiInt(dev, "ID")
				ghs := apiNum(dev, "MHS av") / 1000.0
				if r := currentMHSOf(dev); r > 0 {
					ghs = r / 1000.0
				}
				if w, ok := workOfEntry(dev, "DEVS"); ok {
					if g, ok := liveWork.rate(fmt.Sprintf("dev%d", id), w); ok {
						ghs = g // 0 for a board that is listed Alive but has stopped doing work
					}
				}
				bd := board{
					ID: id, Alive: apiStr(dev, "Status") == "Alive", Enabled: apiStr(dev, "Enabled") == "Y",
					GHS: ghs, HWErrors: apiInt(dev, "Hardware Errors"), HWPct: apiNum(dev, "Device Hardware%"),
					ClockMHz: apiNum(dev, "clock"),
					Temps:    []float64{apiNum(dev, "tstemp-0"), apiNum(dev, "tstemp-1"), apiNum(dev, "tstemp-2")},
					Fans:     boardFanRPMs(dev),
					Overheat: apiNum(dev, "overheat") != 0, Nonced: apiInt(dev, "Nonced"),
				}
				if model.Plan == planBox2V {
					// Box II: "voltage" is the plan's PWM trim LEVEL, not a board millivolt reading. Do not
					// divide by 1000, smooth it, or let it reach the power model (power.go gates on VoltageV);
					// carry the raw level for display only.
					bd.VoltLevel = apiInt(dev, "voltage")
					// a disabled board (stop hashing / idle) is de-initialised: the miner stops reading its chips and
					// keeps reporting their last temperature (70 °C for ever, seen on hardware). Only the board
					// sensor (tstemp-2) is live then.
					if !bd.Enabled {
						bd.Temps[0], bd.Temps[1] = 0, 0
					}
				} else {
					bd.VoltageV = smoothVolt(id, apiNum(dev, "voltage")/1000.0)
				}
				d.Boards = append(d.Boards, bd)
			}
			sort.SliceStable(d.Boards, func(i, j int) bool { return d.Boards[i].ID < d.Boards[j].ID }) // board 0, 1, 2, 3
			// the Live hashrate is the boards' own figures added up: the miner's total lags behind them for
			// the first minutes after a (re)start (787 GH/s while its boards made 3.7 TH/s)
			var sum float64
			for _, b := range d.Boards {
				if b.Alive {
					sum += b.GHS
				}
			}
			if sum > 0 {
				d.GHS = sum
			}
		}
	}
	d.OK = true
	if v, ok := readWatchdog(time.Now()); ok {
		d.Watchdog = &v
		d.Warn = watchdogMessage(v)
	}
	d.ShareUnit = getShareUnit()
	d.HashUnit = getHashUnit()
	d.BestShareT = fmtShareUnit(d.BestShare)
	d.InCharge = chargeLabel()
	return d
}

var (
	tcMu   sync.Mutex
	tcVal  any
	tcRead time.Time
)

// tempcontrolState reads the firmware's tempcontrol switch (cached 30 s) for the Live tiles.
func tempcontrolState() any {
	tcMu.Lock()
	if time.Since(tcRead) < 30*time.Second {
		v := tcVal
		tcMu.Unlock()
		return v
	}
	tcRead = time.Now() // also while this read runs, and after a failed one: the last value
	tcMu.Unlock()
	if raw, err := stockGet("/mcb/setting"); err == nil {
		var cur map[string]any
		if json.Unmarshal(raw, &cur) == nil {
			tcMu.Lock()
			tcVal = cur["tempcontrol"]
			tcMu.Unlock()
		}
	}
	tcMu.Lock()
	defer tcMu.Unlock()
	return tcVal
}

func handleDashboard(w http.ResponseWriter, r *http.Request) {
	d := buildDashboard()
	d.Build = pageBuild
	d.Commission = commissionSig()
	if !d.OK {
		writeJSON(w, d)
		return
	}
	warm, left, steady := hwWarmView(d.HWErrors, d.HWPct)
	writeJSON(w, struct { // the dashboard's fields plus these, in one encoding
		dashboard
		Tempcontrol any            `json:"tempcontrol"`
		Power       map[string]any `json:"power"`
		HWWarm      int            `json:"hw_warm"`       // warm-up HW errors of this miner run (hwwarm.go)
		HWWarmLeft  int            `json:"hw_warm_left"`  // seconds of warm-up still running, 0 = none
		HWPctSteady float64        `json:"hw_pct_steady"` // HW % without the warm-up errors, -1 = unknown
	}{d, tempcontrolState(), powerView(d), warm, left, steady})
}

// ===================== pools =====================
//
// Reads rich status from the cgminer `pools` command, and makes changes through the stock
// web's own PUT /mcb/pools (the mechanism the vendor page + blake2b-asic-control use): fetch
// the current list, reorder/remove/append, put it back — preserving the exact object shape so
// we never guess field names. Pool changes restart mining (stock behavior), so the UI confirms.

type poolView struct {
	Index    int    `json:"index"`
	URL      string `json:"url"`
	User     string `json:"user"`
	Status   string `json:"status"`
	Priority int    `json:"priority"`
	Accepted int    `json:"accepted"`
	Rejected int    `json:"rejected"`
	Stale    int    `json:"stale"`
	Active   bool   `json:"active"`
	// where its stratum connection really goes ("" = not reported): a pool's client.reconnect changes this,
	// not URL (the pool guard compares the two)
	StratumURL string `json:"stratum_url,omitempty"`
}

var (
	minerLabelMu  sync.Mutex
	minerLabelVal string
	minerLabelAt  time.Time
)

// minerDisplayName returns a short name for this miner to tag the current-temperature marker with:
// the worker name of the active (else highest-priority) pool — the part after the last "." in the
// pool user — falling back to the system hostname. Cached for a minute so it doesn't hit the miner
// API on every status poll.
func minerDisplayName() string {
	minerLabelMu.Lock()
	defer minerLabelMu.Unlock()
	if minerLabelVal != "" && time.Since(minerLabelAt) < time.Minute {
		return minerLabelVal
	}
	label := ""
	if pools, err := readPools(); err == nil {
		best, bestPri := "", int(^uint(0)>>1)
		for _, p := range pools {
			if p.User == "" {
				continue
			}
			if p.Active {
				best = p.User
				break
			}
			if p.Priority < bestPri {
				bestPri, best = p.Priority, p.User
			}
		}
		if i := strings.LastIndex(best, "."); i >= 0 && i < len(best)-1 {
			label = best[i+1:]
		}
	}
	if label == "" {
		if h, err := os.Hostname(); err == nil {
			label = strings.TrimSpace(h)
		}
	}
	if label != "" {
		minerLabelVal, minerLabelAt = label, time.Now()
	}
	return label
}

func readPools() ([]poolView, error) {
	raw, err := cgminerCmd("pools")
	if err != nil {
		return nil, err
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(apiClean(raw), &top); err != nil {
		return nil, fmt.Errorf("parse pools: %v", err)
	}
	var arr []map[string]any
	if p, ok := top["POOLS"]; ok { // (no POOLS section: the miner has no pools)
		if err := json.Unmarshal(p, &arr); err != nil {
			return nil, fmt.Errorf("parse pools: %v", err)
		}
	}
	out := []poolView{}
	for _, p := range arr {
		active, _ := p["Stratum Active"].(bool)
		out = append(out, poolView{apiInt(p, "POOL"), apiStr(p, "URL"), apiStr(p, "User"), apiStr(p, "Status"), apiInt(p, "Priority"),
			apiInt(p, "Accepted"), apiInt(p, "Rejected"), apiInt(p, "Stale"), active, apiStr(p, "Stratum URL")})
	}
	return out, nil
}

// poolURLErr: why Goldshell's programs can't take this pool address ("" = fine). Their watchdog (xminerd)
// cuts the first 14 characters ("stratum+tcp://") off the address without checking: a shorter one crashed it
// at every start (no fan check at all), and one of 128+ characters overran its buffer.
func poolURLErr(u string) string {
	if !strings.HasPrefix(u, "stratum+tcp://") && !strings.HasPrefix(u, "stratum+ssl://") {
		return "the pool address must start with stratum+tcp:// (or stratum+ssl://)"
	}
	if len(u) < 15 || len(u) > 120 || strings.ContainsAny(u, " \t\"'") || hasCtrl(u) {
		return "the pool address must be 15–120 characters, with no spaces, quotes or control characters"
	}
	return ""
}

// hasCtrl reports whether s holds any C0 control byte (NUL, tab, CR, LF, …) or DEL. The stock programs write
// pool fields into line- and JSON-based config/log formats where such bytes break parsing, and the private-log
// redaction drops any token containing a newline (see rememberPrivate), so a control char in a credential can
// slip that secret past the "private" export.
func hasCtrl(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] == 0x7f {
			return true
		}
	}
	return false
}

// poolCredErr: why Goldshell's programs can't take this pool user/worker or password ("" = fine; the caller
// defaults a blank password to "x"). Mirrors poolURLErr's intent for the credential fields, which otherwise
// reach the same stock programs and the log exporter unchecked.
func poolCredErr(what, s string) string {
	if len(s) > 200 {
		return "the pool " + what + " must be 200 characters or fewer"
	}
	if hasCtrl(s) {
		return "the pool " + what + " must not contain control characters"
	}
	return ""
}

// fetchPoolList gets the editable pool list from the stock web, returning the opaque pool
// objects and the wrapper shape ("array" or "pools") so putPoolList can put it back exactly.
// noPoolSet: no pool is configured, so the stock launcher doesn't start the miner (a fresh setup). Asks
// Goldshell's backend first; if that doesn't answer either, the miner's own config file decides (missing,
// or no pool with an address in it).
func noPoolSet() bool {
	if arr, _, err := fetchPoolList(); err == nil {
		return len(arr) == 0
	}
	b, err := os.ReadFile(*bfgPath)
	if err != nil {
		return os.IsNotExist(err)
	}
	var c struct {
		Pools []map[string]any `json:"pools"`
	}
	if json.Unmarshal(b, &c) != nil {
		return false
	}
	for _, p := range c.Pools {
		if u, _ := p["url"].(string); strings.TrimSpace(u) != "" && !strings.EqualFold(strings.TrimSpace(u), "benchmark") {
			return false
		}
	}
	return true
}

func fetchPoolList() ([]map[string]any, string, error) {
	raw, err := stockGet("/mcb/pools")
	if err != nil {
		return nil, "", err
	}
	// no pools yet (a fresh setup): the backend may answer with nothing, null, {} or {"pools": null}
	switch t := strings.TrimSpace(string(raw)); t {
	case "", "null", "{}", "[]", `{"pools":null}`, `{"pools":[]}`:
		return []map[string]any{}, "array", nil
	}
	var arr []map[string]any
	if json.Unmarshal(raw, &arr) == nil {
		return arr, "array", nil
	}
	var obj map[string]any
	if json.Unmarshal(raw, &obj) == nil {
		if v, has := obj["pools"]; has && v == nil {
			return []map[string]any{}, "pools", nil
		}
		if p, ok := obj["pools"].([]any); ok {
			out := make([]map[string]any, 0, len(p))
			for _, it := range p {
				if m, ok := it.(map[string]any); ok {
					out = append(out, m)
				}
			}
			return out, "pools", nil
		}
		// an icinfo-style {file_name, body:"<json>"} wrapper
		if body, ok := obj["body"].(string); ok {
			if t := strings.TrimSpace(body); t == "" || t == "null" {
				return []map[string]any{}, "body", nil
			}
			if json.Unmarshal([]byte(body), &arr) == nil {
				return arr, "body", nil
			}
		}
	}
	msg := string(raw)
	if len(msg) > 160 {
		msg = msg[:160]
	}
	return nil, "", fmt.Errorf("unexpected /mcb/pools shape: %s", msg)
}

func putPoolList(arr []map[string]any, shape string) error {
	// renumber any priority field to match the new order (first = primary)
	for i, p := range arr {
		if _, ok := p["pool-priority"]; ok {
			p["pool-priority"] = i
		}
		if _, ok := p["priority"]; ok {
			p["priority"] = i
		}
	}
	var body []byte
	switch shape {
	case "pools":
		body, _ = json.Marshal(map[string]any{"pools": arr})
	case "body":
		inner, _ := json.Marshal(arr)
		body, _ = json.Marshal(map[string]any{"body": string(inner)})
	default:
		body, _ = json.Marshal(arr)
	}
	_, err := stockReq("PUT", "/mcb/pools", body)
	return err
}

// setPoolField sets val on the first key variant that already exists (falling back to the
// first candidate), so an edit updates the real key whatever its case.
func poolStr(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func setPoolField(m map[string]any, val any, keys ...string) {
	for _, k := range keys {
		if _, ok := m[k]; ok {
			m[k] = val
			return
		}
	}
	m[keys[0]] = val
}

func poolsHasPriorityKey(arr []map[string]any) bool {
	for _, p := range arr {
		if _, ok := p["pool-priority"]; ok {
			return true
		}
	}
	return false
}

// poolEditMu: one pool change (read the list, check it, write it) at a time. Two at once (a double click, two
// tabs) both read the same list: an add went to minerd twice (the duplicate check saw neither), and of two
// edits the second wrote back the list without the first.
var poolEditMu sync.Mutex

func handlePools(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		poolEditMu.Lock()
		defer poolEditMu.Unlock()
		var in struct {
			Action  string `json:"action"`
			Index   int    `json:"index"`
			URL     string `json:"url"`
			User    string `json:"user"`
			Pass    string `json:"pass"`
			Confirm bool   `json:"confirm"`
			// the pool as the page showed it: the page's numbers are the running miner's, which can differ
			// from the saved list (a pool the guard is about to remove, a list mid-change)
			OldURL  string `json:"old_url"`
			OldUser string `json:"old_user"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil { // half a request mustn't edit a pool
			writeJSON(w, map[string]any{"ok": false, "err": "bad request"})
			return
		}
		if !in.Confirm {
			writeJSON(w, map[string]any{"ok": false, "err": "confirmation required"})
			return
		}
		arr, shape, err := fetchPoolList()
		rememberPrivate(poolListTokens(arr)) // the logs and events keep the pools being changed
		if err != nil {
			// allow adding the very first pool even if the list couldn't be read as non-empty
			if in.Action == "add" {
				arr, shape = []map[string]any{}, "array"
			} else {
				writeJSON(w, map[string]any{"ok": false, "err": "read pools: " + err.Error()})
				return
			}
		}
		if in.Action != "add" && in.OldURL == "" && (in.Index < 0 || in.Index >= len(arr) || poolStrAny(arr[in.Index], "url", "URL") != "") {
			writeJSON(w, map[string]any{"ok": false, "err": "reload the Pools tab and try again (this page didn't say which pool)"})
			return
		}
		if in.Action != "add" && in.OldURL != "" {
			want, found := poolKey(in.OldURL, in.OldUser), -1
			for i, p := range arr {
				if poolKey(poolStrAny(p, "url", "URL"), poolStrAny(p, "user", "User")) == want {
					found = i
					break
				}
			}
			if found < 0 {
				writeJSON(w, map[string]any{"ok": false, "err": "that pool isn't in your saved list (if it isn't yours, the pool guard removes it from the running miner)"})
				return
			}
			in.Index = found
		}
		switch in.Action {
		case "add":
			// the stock page adds with PUT /mcb/newpool; PUT /mcb/pools only reorders/edits the existing
			// list (minerd answers 500 when the count changes), so it can't add the first pool
			url := strings.TrimSpace(in.URL)
			if url == "" {
				writeJSON(w, map[string]any{"ok": false, "err": "pool address required"})
				return
			}
			if !strings.Contains(url, "://") {
				url = "stratum+tcp://" + url
			}
			if e := poolURLErr(url); e != "" {
				writeJSON(w, map[string]any{"ok": false, "err": e})
				return
			}
			if e := poolCredErr("worker/user", strings.TrimSpace(in.User)); e != "" {
				writeJSON(w, map[string]any{"ok": false, "err": e})
				return
			}
			if e := poolCredErr("password", in.Pass); e != "" {
				writeJSON(w, map[string]any{"ok": false, "err": e})
				return
			}
			pass := in.Pass
			if pass == "" {
				pass = "x"
			}
			for _, p := range arr {
				if strings.EqualFold(poolStr(p, "url", "URL"), url) && poolStr(p, "user", "User") == strings.TrimSpace(in.User) {
					writeJSON(w, map[string]any{"ok": false, "err": "that pool and worker are already in the list"})
					return
				}
			}
			body, _ := json.Marshal(map[string]any{"url": url, "user": strings.TrimSpace(in.User), "pass": pass})
			if _, err := stockReq("PUT", "/mcb/newpool", body); err != nil {
				writeJSON(w, map[string]any{"ok": false, "err": "save failed: " + err.Error()})
				return
			}
			recordMark("P", "pool added")
			activity.emit("⚙ pool added")
			writeJSON(w, map[string]any{"ok": true, "note": "saved — the miner restarts briefly to apply the pool change"})
			return
		case "edit":
			if in.Index < 0 || in.Index >= len(arr) {
				writeJSON(w, map[string]any{"ok": false, "err": "bad pool index"})
				return
			}
			url := strings.TrimSpace(in.URL)
			if url == "" {
				writeJSON(w, map[string]any{"ok": false, "err": "pool address required"})
				return
			}
			if !strings.Contains(url, "://") {
				url = "stratum+tcp://" + url
			}
			if e := poolURLErr(url); e != "" {
				writeJSON(w, map[string]any{"ok": false, "err": e})
				return
			}
			if e := poolCredErr("worker/user", strings.TrimSpace(in.User)); e != "" {
				writeJSON(w, map[string]any{"ok": false, "err": e})
				return
			}
			if e := poolCredErr("password", in.Pass); e != "" {
				writeJSON(w, map[string]any{"ok": false, "err": e})
				return
			}
			p := arr[in.Index]
			setPoolField(p, url, "url", "URL")
			setPoolField(p, strings.TrimSpace(in.User), "user", "User")
			if strings.TrimSpace(in.Pass) != "" { // blank keeps the existing password
				setPoolField(p, in.Pass, "pass", "Pass")
			}
		case "delete":
			// the stock page removes with PUT /mcb/delpool and the pool's own object
			if in.Index < 0 || in.Index >= len(arr) {
				writeJSON(w, map[string]any{"ok": false, "err": "bad pool index"})
				return
			}
			body, _ := json.Marshal(arr[in.Index])
			if _, err := stockReq("PUT", "/mcb/delpool", body); err != nil {
				writeJSON(w, map[string]any{"ok": false, "err": "remove failed: " + err.Error()})
				return
			}
			recordMark("P", "pool removed")
			activity.emit("⚙ pool removed")
			writeJSON(w, map[string]any{"ok": true, "note": "removed — the miner restarts briefly to apply the pool change"})
			return
		case "switch":
			if in.Index < 0 || in.Index >= len(arr) {
				writeJSON(w, map[string]any{"ok": false, "err": "bad pool index"})
				return
			}
			chosen := arr[in.Index]
			rest := append(arr[:in.Index:in.Index], arr[in.Index+1:]...)
			arr = append([]map[string]any{chosen}, rest...)
		default:
			writeJSON(w, map[string]any{"ok": false, "err": "unknown action"})
			return
		}
		if err := putPoolList(arr, shape); err != nil {
			writeJSON(w, map[string]any{"ok": false, "err": "save failed: " + err.Error()})
			return
		}
		recordMark("P", "pools changed")
		activity.emit("⚙ pools updated")
		writeJSON(w, map[string]any{"ok": true, "note": "saved — the miner restarts briefly to apply the pool change"})
		return
	}
	pools, err := readPools()
	if err != nil {
		// the engine isn't running (idle, starting, or no pool yet): show the pools it's set up with
		if cp, cerr := configuredPools(); cerr == nil {
			writeJSON(w, map[string]any{"ok": true, "pools": cp, "configured": true, "idle": getCharge() == "idle"})
			return
		}
		writeJSON(w, busyOrErr("the pool list", err))
		return
	}
	writeJSON(w, map[string]any{"ok": true, "pools": pools})
}

// configuredPools: the pool list as set up (Goldshell's backend, or the miner's own config file if the
// backend doesn't answer either), for when the mining engine isn't running and can't report its pools.
func configuredPools() ([]poolView, error) {
	list, _, err := fetchPoolList()
	if err != nil {
		b, ferr := os.ReadFile(*bfgPath)
		if ferr != nil {
			return nil, err
		}
		var c struct {
			Pools []map[string]any `json:"pools"`
		}
		if json.Unmarshal(b, &c) != nil {
			return nil, err
		}
		list = c.Pools
	}
	out := []poolView{}
	for i, p := range list {
		u, _ := p["url"].(string)
		usr, _ := p["user"].(string)
		if strings.TrimSpace(u) == "" || strings.EqualFold(strings.TrimSpace(u), "benchmark") {
			continue
		}
		out = append(out, poolView{Index: i, URL: u, User: usr, Status: "not mining", Priority: i})
	}
	return out, nil
}

// ===================== presets =====================
//
// Named clock/voltage/PV presets, saved on the miner (persist across reboots). Built from a
// clock-curve search (High/Middle/Low/Lowest-power from the curve's clean points), or saved by
// hand from the current Controls. Applying one sets the plan live, like a manual change.

type preset struct {
	Name       string       `json:"name"`
	MHz        int          `json:"mhz"`
	MV         int          `json:"mv"`
	PV         int          `json:"pv"`
	TargetC    int          `json:"target_c,omitempty"`    // fan target temp (tuner-built presets)
	Tested     string       `json:"tested,omitempty"`      // date built/tested
	FanProfile string       `json:"fan_profile,omitempty"` // "", builtin key, or "custom"
	FanPoints  []curvePoint `json:"fan_points,omitempty"`  // when FanProfile=="custom"
	// the preset's own curve settings (set when its tuned curve is edited); 0 = use the Auto fan settings
	FanAbortC   float64 `json:"fan_abort_c,omitempty"`
	FanCooldown int     `json:"fan_cooldown_s,omitempty"`
	FanPoll     int     `json:"fan_poll_s,omitempty"`
	FanSmooth   *int    `json:"fan_smoothing,omitempty"`
	FanEdited   bool    `json:"fan_edited,omitempty"` // the tuner's curve was changed by hand
}

var presetMu sync.Mutex

func loadPresets() []preset {
	presetMu.Lock()
	defer presetMu.Unlock()
	b, err := os.ReadFile(*presetPath)
	if err != nil {
		return []preset{}
	}
	var p []preset
	if json.Unmarshal(b, &p) != nil {
		return []preset{}
	}
	for i := range p { // a preset restored from an old file or edited by hand: within today's bounds
		// Box II has no run/board voltage (the plan "V" is a fixed PWM trim, not volts) and its voltage window
		// is empty (0..0). Clamping here would write SC Lite-style voltages — PV/MV — into a Box II preset
		// (R14 F-notes), and zero out its stored clock pairing. Only the voltage models clamp voltages.
		if p[i].MHz > 0 && model.Plan != planBox2V {
			p[i].PV = clampInt(p[i].PV, pvMin, pvHardMax) // the supply's range (below 9100 it ran 9100 anyway)
			p[i].MV = clampInt(p[i].MV, vrunHardMin, min(vrunHardMax, p[i].PV))
		}
		if p[i].FanAbortC != 0 {
			p[i].FanAbortC = clampf(p[i].FanAbortC, 70, abortMaxC) // model abort-max (cutoff-2): never above the cut-off
		}
		if p[i].FanCooldown < 0 {
			p[i].FanCooldown = 0
		}
		if p[i].FanPoll != 0 {
			p[i].FanPoll = clampInt(p[i].FanPoll, 2, 120)
		}
		if p[i].FanSmooth != nil {
			v := clampInt(*p[i].FanSmooth, 0, 100)
			p[i].FanSmooth = &v
		}
		for j := range p[i].FanPoints {
			p[i].FanPoints[j].Fan = clampf(p[i].FanPoints[j].Fan, 0, 100)
		}
	}
	return p
}

// presetFanCurve derives a small monotonic fan curve that holds ~targetC, using the fan %
// the tuner measured while holding that temperature. Fans never drop below fanMin and reach
// 100% a couple of degrees below the abort temperature.
// tunedPresetCurve: a fan curve through (target, the fan % that held it), gentle below and steeper above
// (blake2b-asic-control's preset_curve): (T−10, f−20), (T−4, f−8), (T, f), (T+4, f+15),
// (min(T+8, safety stop−2), 100); fans never fall as the temperature rises.
func tunedPresetCurve(target float64, fan, fanMin int, abortC float64) []curvePoint {
	f := float64(max(fanMin, min(100, fan)))
	pts := [][2]float64{{target - 10, f - 20}, {target - 4, f - 8}, {target, f}, {target + 4, f + 15},
		{math.Min(target+8, abortC-2), 100}}
	var out []curvePoint
	last := 0.0
	for _, p := range pts {
		v := math.Round(math.Max(float64(fanMin), math.Min(100, p[1])))
		v = math.Max(v, last)
		t := math.Round(math.Max(20, math.Min(100, p[0])))
		if len(out) > 0 && t <= out[len(out)-1].Temp {
			continue
		}
		out = append(out, curvePoint{Temp: t, Fan: v})
		last = v
	}
	if n := len(out); n > 0 && out[n-1].Fan < 100 { // a target close to the safety stop: the top point was skipped
		out[n-1].Fan = 100
	}
	return out
}

func presetFanCurve(targetC float64, heldFan int, abortC float64, fanMin int) []curvePoint {
	t := math.Round(targetC)
	if heldFan < fanMin {
		heldFan = fanMin
	}
	if heldFan > 100 {
		heldFan = 100
	}
	hiT := math.Round(abortC) - 2
	if hiT <= t {
		hiT = t + 3
	}
	pts := []curvePoint{}
	loT := t - 8
	loF := heldFan - 15
	if loF < fanMin {
		loF = fanMin
	}
	if loT >= 30 && loT < t && loF < heldFan {
		pts = append(pts, curvePoint{Temp: loT, Fan: float64(loF)})
	}
	pts = append(pts, curvePoint{Temp: t, Fan: float64(heldFan)})
	pts = append(pts, curvePoint{Temp: hiT, Fan: 100})
	return pts
}

// fanCurveText renders a fan curve as "62°→30%, 70°→55%, 86°→100%" for logs.
func fanCurveText(pts []curvePoint) string {
	parts := make([]string, 0, len(pts))
	for _, p := range pts {
		parts = append(parts, fmt.Sprintf("%.0f°→%.0f%%", p.Temp, p.Fan))
	}
	return strings.Join(parts, ", ")
}

// mergePresets saves new presets, replacing any with the same name and keeping the rest.
func mergePresets(np []preset) error {
	list := loadPresets()
	byName := map[string]bool{}
	for _, p := range np {
		byName[p.Name] = true
	}
	var out []preset
	for _, p := range list {
		if !byName[p.Name] {
			out = append(out, p)
		}
	}
	out = append(out, np...)
	return savePresets(out)
}

func savePresets(p []preset) error {
	presetMu.Lock()
	defer presetMu.Unlock()
	_ = os.MkdirAll(filepath.Dir(*presetPath), 0755)
	b, _ := json.MarshalIndent(p, "", " ")
	return writeAtomic(*presetPath, b, 0644)
}

// buildPresetsFromCurve derives up to 4 named presets from a tuner curve's clean points.
func buildPresetsFromCurve(curve []planMV) []preset {
	if len(curve) == 0 {
		return nil
	}
	cp := append([]planMV(nil), curve...)
	sort.Slice(cp, func(i, j int) bool { return cp[i].MHz < cp[j].MHz })
	pick := func(frac float64) planMV {
		i := int(frac * float64(len(cp)-1))
		if i < 0 {
			i = 0
		}
		if i >= len(cp) {
			i = len(cp) - 1
		}
		return cp[i]
	}
	today := time.Now().Format("2006-01-02")
	mk := func(name string, p planMV) preset {
		return preset{Name: name, MHz: p.MHz, MV: p.MV, PV: p.PV, Tested: today}
	}
	if len(cp) == 1 {
		return []preset{mk("Tuned", cp[0])}
	}
	return []preset{
		mk("High", pick(1)),
		mk("Middle", pick(0.66)),
		mk("Low", pick(0.33)),
		mk("Lowest power", pick(0)),
	}
}

// captureCurrentFan snapshots the active fan curve so every saved preset carries one.
func captureCurrentFan() (string, []curvePoint) {
	fanMu.RLock()
	defer fanMu.RUnlock()
	if fanCfg.Profile == "custom" && len(fanCfg.Points) >= 2 {
		return "custom", append([]curvePoint(nil), fanCfg.Points...)
	}
	if strings.HasPrefix(fanCfg.Profile, "tuned:") {
		if fc, ok := lookupCurve(fanCfg, fanCfg.Profile); ok {
			return "custom", append([]curvePoint(nil), fc.Points...)
		}
	}
	if fanCfg.Profile != "" {
		return fanCfg.Profile, nil
	}
	return "curve-balanced", nil
}

// applyPresetFan applies a preset's own fan curve (enabling auto-fan). Internal — bypasses
// the HTTP lock because it IS the preset taking charge of the fans.
func applyPresetFan(p preset) {
	if p.FanProfile == "none" || (p.FanProfile == "" && len(p.FanPoints) == 0) {
		return // "Leave the fans as they are"
	}
	fanMu.Lock()
	c := fanCfg
	c.Enabled = true
	if p.FanProfile == "custom" && len(p.FanPoints) >= 2 {
		c.Profile, c.Points = "custom", append([]curvePoint(nil), p.FanPoints...)
		if p.FanAbortC > 0 { // its own curve settings, when the tuned curve was edited
			c.AbortC = p.FanAbortC
		}
		if p.FanCooldown > 0 {
			c.CooldownS = p.FanCooldown
		}
		if p.FanPoll > 0 {
			c.PollS = p.FanPoll
		}
		if p.FanSmooth != nil {
			c.Smoothing = *p.FanSmooth
		}
	} else if p.FanProfile != "" {
		c.Profile = p.FanProfile
		if fc, ok := lookupCurve(c, p.FanProfile); ok { // the curve carries its own settings
			c.AbortC, c.CooldownS, c.PollS, c.Smoothing = fc.AbortC, fc.CooldownS, fc.PollS, fc.Smoothing
		}
	}
	fitResume(&c)
	if err := validateFanConfig(c); err != nil { // a preset's curve from an old or hand-edited file: not applied
		fanMu.Unlock()
		activity.emit("⚠ preset " + p.Name + ": its fan curve isn't valid (" + err.Error() + "): the fans stay as they were")
		return
	}
	fanCfg = c
	fanHist = nil
	fanSt.Guard, fanSt.Note = "", ""
	fanSt.LastWrite = time.Time{}
	snapshot := c
	fanMu.Unlock()
	_ = saveFanConfig(snapshot)
}

func handlePresets(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var in struct {
			Action     string       `json:"action"`
			Name       string       `json:"name"`
			WhenCool   bool         `json:"when_cool"` // apply: after a heat Idle, start it by itself once the boards are cool
			MHz        int          `json:"mhz"`
			MV         int          `json:"mv"`
			PV         int          `json:"pv"`
			FanProfile string       `json:"fan_profile"`
			FanPoints  []curvePoint `json:"fan_points"`
		}
		if !decodeIn(w, r, &in) {
			return
		}
		list := loadPresets()
		switch in.Action {
		case "save":
			// Box II ships only its two stock modes (Hashrate / Low-power); the panel never builds a custom
			// clock/voltage preset (that is the tuner's, which is off on this model). Refuse, don't create.
			if model.Plan == planBox2V && !model.Feat.ClockControl {
				writeJSON(w, map[string]any{"ok": false, "disabled": true,
					"err": "custom presets aren't available on " + model.Name + " — it runs only its two stock modes (" + box2StockHigh + " / " + box2StockLow + "); switch between them from Controls or the schedule"})
				return
			}
			name := strings.TrimSpace(in.Name)
			if name == "" {
				writeJSON(w, map[string]any{"ok": false, "err": "name required"})
				return
			}
			if model.Plan == planBox2V { // lane C3: a clock preset (+ V code with C4), checked like Controls
				var err error
				if in.MV, err = box2PresetCheck(name, in.MHz, in.MV); err != nil {
					writeJSON(w, map[string]any{"ok": false, "err": err.Error()})
					return
				}
				in.PV = 0
			} else if err := validateControlEx(powerplan{FreqMHz: in.MHz, VrunMv: in.MV, PVMv: in.PV, Fan1: 50, Fan2: 50}, loadOverclock().Acknowledged); err != nil {
				writeJSON(w, map[string]any{"ok": false, "err": err.Error()})
				return
			}
			if in.FanProfile == "custom" {
				fanMu.RLock()
				tc := fanCfg
				fanMu.RUnlock()
				tc.Profile, tc.Points = "custom", in.FanPoints
				if err := validateFanConfig(tc); err != nil {
					writeJSON(w, map[string]any{"ok": false, "err": "fan curve: " + err.Error()})
					return
				}
			} else if len(in.FanPoints) > 0 { // they'd still run, as the preset's "tuned:" curve
				if err := validatePoints(in.FanPoints); err != nil {
					writeJSON(w, map[string]any{"ok": false, "err": "fan curve: " + err.Error()})
					return
				}
			}
			if len(name) > 40 {
				writeJSON(w, map[string]any{"ok": false, "err": "the name can be at most 40 characters"})
				return
			}
			if strings.IndexFunc(name, unicode.IsControl) >= 0 {
				writeJSON(w, map[string]any{"ok": false, "err": "the name can't contain line breaks or other control characters"})
				return
			}
			switch strings.ToLower(name) {
			case "stock", "idle", schedBack: // the built-ins answer to these names: a preset called that could never be applied
				writeJSON(w, map[string]any{"ok": false, "err": "“" + name + "” is taken by the built-in setting: pick another name"})
				return
			}
			np := preset{Name: name, MHz: in.MHz, MV: in.MV, PV: in.PV, FanProfile: in.FanProfile, FanPoints: in.FanPoints}
			if np.FanProfile == "" && len(np.FanPoints) == 0 {
				np.FanProfile, np.FanPoints = captureCurrentFan() // every preset carries a fan curve
			}
			replaced := false
			for i := range list {
				if list[i].Name == name {
					np.Tested = list[i].Tested
					np.TargetC = list[i].TargetC
					if np.FanProfile == "custom" { // keeps its own (possibly edited) curve settings
						np.FanAbortC, np.FanCooldown, np.FanPoll, np.FanSmooth, np.FanEdited =
							list[i].FanAbortC, list[i].FanCooldown, list[i].FanPoll, list[i].FanSmooth, list[i].FanEdited
					}
					list[i] = np
					replaced = true
				}
			}
			if !replaced {
				list = append(list, np)
			}
			if err := savePresets(list); err != nil {
				writeJSON(w, map[string]any{"ok": false, "err": "save failed: " + err.Error()})
				return
			}
			writeJSON(w, map[string]any{"ok": true})
		case "delete":
			out := list[:0]
			for _, p := range list {
				if p.Name != in.Name {
					out = append(out, p)
				}
			}
			if err := savePresets(out); err != nil {
				writeJSON(w, map[string]any{"ok": false, "err": "save failed: " + err.Error()})
				return
			}
			writeJSON(w, map[string]any{"ok": true})
		case "build":
			// Tuner-built presets come from the auto-tuner, which is off on Box II (no clock/voltage control).
			if !featGate(w, model.Feat.Tuner, "tuner-built presets") {
				return
			}
			tunerMu.Lock()
			t := tunerCur
			tunerMu.Unlock()
			if t == nil {
				writeJSON(w, map[string]any{"ok": false, "err": "no tuner run yet: run the tuner with “Map every clock and voltage” first"})
				return
			}
			built := buildPresetsFromCurve(t.getCurve())
			if len(built) == 0 {
				writeJSON(w, map[string]any{"ok": false, "err": "no curve to build from: run the tuner with “Map every clock and voltage” first"})
				return
			}
			// every built preset carries a fan curve (the active one, until the hold-steady
			// tuner phase computes a per-preset curve)
			fp, fpts := captureCurrentFan()
			for i := range built {
				built[i].FanProfile, built[i].FanPoints = fp, fpts
			}
			// merge: replace same-named, keep others
			byName := map[string]bool{}
			for _, b := range built {
				byName[b.Name] = true
			}
			out := built
			for _, p := range list {
				if !byName[p.Name] {
					out = append(out, p)
				}
			}
			if err := savePresets(out); err != nil {
				writeJSON(w, map[string]any{"ok": false, "err": "save failed: " + err.Error()})
				return
			}
			writeJSON(w, map[string]any{"ok": true, "built": len(built)})
		case "apply":
			controlMu.Lock()
			defer controlMu.Unlock()
			gen := safetyGen.Load() // before the checks: a safety stop from here on wins (see safetyGen)
			if c := getCharge(); c == "tuner" || c == "ceiling" {
				writeJSON(w, map[string]any{"ok": false, "err": "the " + map[string]string{"tuner": "tuner", "ceiling": "limit probe"}[c] + " is running — stop it first"})
				return
			}
			if sctRunning() { // its raised chips would run faster still on a new clock until it aborts
				writeJSON(w, map[string]any{"ok": false, "err": "the strong-chip test is running (Controls → Chip trims): stop it first"})
				return
			}
			if err := applyPresetNameGen(in.Name, gen); err != nil {
				var hc *heatCoolingErr
				if errors.As(err, &hc) {
					if in.WhenCool {
						heatPending.Store(in.Name)
						note := fmt.Sprintf("%s starts by itself when the cool-down ends (in %s)", presetLabel(in.Name), mmss(hc.left))
						activity.emit("🔥 " + note)
						writeJSON(w, map[string]any{"ok": true, "waiting": true, "note": note})
						return
					}
					writeJSON(w, map[string]any{"ok": false, "cooling": true, "left": mmss(hc.left), "err": err.Error()})
					return
				}
				writeJSON(w, map[string]any{"ok": false, "err": err.Error()})
				return
			}
			activity.emit("⚙ preset applied: " + presetLabel(in.Name))
			writeJSON(w, map[string]any{"ok": true, "note": "applied " + presetLabel(in.Name)})
		case "idle":
			controlMu.Lock()
			defer controlMu.Unlock()
			if getCharge() == "tuner" {
				writeJSON(w, map[string]any{"ok": false, "err": "the tuner is running — stop it first"})
				return
			}
			heatPending.Store("") // the owner's Idle: no waiting "start when cool"
			safetyStop()          // a limit probe step or the fan test's put-back on its way can't land over this Idle
			// idleAction is the model's park: the firmware idle plan on SC Lite, lane B2's miner-API board-off on
			// Box II (which has no idle plan). It fails cleanly on an unproven/absent stop rather than mis-writing.
			if err := idleAction(model); err != nil {
				writeJSON(w, map[string]any{"ok": false, "err": err.Error()})
				return
			}
			setCharge("idle")
			recordMark("P", "idle")
			sctEndFor("The miner was put on Idle")
			activity.emit("⏸ idle applied")
			writeJSON(w, map[string]any{"ok": true, "note": "miner set to idle"})
		default:
			writeJSON(w, map[string]any{"ok": false, "err": "unknown action"})
		}
		return
	}
	// GET: presets + what's currently on the miner (to flag the active one) + live
	// hashrate and running clock (so the UI can show a reliable ≈TH/s per preset by
	// scaling the measured hashrate against the running clock — no fabricated wattage).
	// On Box II the "presets" ARE the two stock modes (Hashrate / Low-power) read from the firmware's own
	// plans: the panel only selects between them, it never stores a custom clock/voltage preset.
	resp := map[string]any{"ok": true, "presets": presetsList(), "in_charge": chargeLabel()}
	if model.Plan == planBox2V {
		resp["box2"] = true
		resp["stock_modes"] = true
	}
	if raw, err := stockGet("/mcb/setting"); err == nil {
		var cur map[string]any
		if json.Unmarshal(raw, &cur) == nil {
			if pp, ok := resolveRunningPlan(cur); ok {
				resp["current"] = map[string]any{"mhz": pp.FreqMHz, "mv": pp.VrunMv, "pv": pp.PVMv}
			}
			resp["has_idle"] = idleLevel(cur) >= 0 || (model.Plan == planBox2V && model.Feat.SoftIdle) // Box II: the soft idle (lane C1)
		}
	}
	if sp, ok := readStockBaseline("stock"); ok && sp.FreqMHz > 0 {
		resp["stock"] = map[string]int{"mhz": sp.FreqMHz, "mv": sp.VrunMv, "pv": sp.PVMv}
	}
	d := buildDashboard()
	if d.OK {
		resp["ghs"] = d.GHS
	}
	writeJSON(w, resp)
}

// stockPlans returns the firmware's power plans in the order its own page lists them. "select" is a
// position in this list (Goldshell's page: value = the list index), not the plan's "level" field: on the
// SC Lite the idle plan is at position 1 with level 3. Some firmware nests the plans per algorithm
// ({algo, mode: [...]}, chosen by "algoname"); then the list is the current algorithm's modes.
func stockPlans(cur map[string]any) []map[string]any {
	arr, _ := cur["powerplans"].([]any)
	var out []map[string]any
	if algo, _ := cur["algoname"].(string); algo != "" {
		for _, it := range arr {
			mm, _ := it.(map[string]any)
			if a, _ := mm["algo"].(string); a == algo {
				modes, _ := mm["mode"].([]any)
				for _, m := range modes {
					if x, ok := m.(map[string]any); ok {
						out = append(out, x)
					}
				}
				return out
			}
		}
	}
	for _, it := range arr {
		if mm, ok := it.(map[string]any); ok {
			out = append(out, mm)
		}
	}
	return out
}

// stockPlanAt: the plan at list position sel.
func stockPlanAt(cur map[string]any, sel int) (powerplan, bool) {
	ps := stockPlans(cur)
	if sel < 0 || sel >= len(ps) {
		return powerplan{}, false
	}
	return stockPlanOf(ps[sel])
}

// stockPlanOf parses one stock plan entry. On the SC Box II Goldshell's backend sometimes rewrites a plan's
// info text with "0.000000 V" (seen on hardware: "690 MHz 0.000000 V 40 RPM 40 RPM" for the 690 MHz / code 42
// plan); the entry's own "voltage" field (42.00001) still holds the code, so the code is taken from there.
// Without this every write on top of that plan (the fan curve, the thermal guard's 100 %) was refused as
// "690 MHz / 0 V is not a stock plan".
func stockPlanOf(mm map[string]any) (powerplan, bool) {
	info, _ := mm["info"].(string)
	pp, ok := parsePowerplan(info)
	if ok && model.Plan == planBox2V && pp.VrunMv == 0 && pp.FreqMHz > 0 {
		if v, isNum := mm["voltage"].(float64); isNum && v >= 1 && v < 100 {
			pp.VrunMv = int(v)
		}
	}
	return pp, ok
}

func stockSelect(cur map[string]any) int {
	if f, ok := cur["select"].(float64); ok {
		return int(f)
	}
	return 0
}

// box2DefaultIndex: on the SC Box II, the list position of Goldshell's factory default plan (the Hashrate
// tuple, 690 MHz / level 42). "Stock" means that plan: the firmware's "select" moves with every stock mode
// picked (Low-power selects the 525 MHz plan), so it can't stand for the factory setting. -1 elsewhere.
func box2DefaultIndex(cur map[string]any) int {
	if model.Plan != planBox2V {
		return -1
	}
	hi := box2HashrateTuple()
	for i := range stockPlans(cur) {
		if pp, ok := stockPlanAt(cur, i); ok && pp.FreqMHz == hi.FreqMHz && pp.VrunMv == hi.V {
			return i
		}
	}
	return -1
}

// firstHashingPlan: the list position of the first plan that hashes (clock > 0), or -1.
func firstHashingPlan(cur map[string]any) int {
	for i := range stockPlans(cur) {
		if pp, ok := stockPlanAt(cur, i); ok && pp.FreqMHz > 0 {
			return i
		}
	}
	return -1
}

// idleLevel finds the list position of the stock firmware's idle plan (the 0 MHz entry), or -1.
func idleLevel(cur map[string]any) int {
	for i, mm := range stockPlans(cur) {
		info, _ := mm["info"].(string)
		if pp, ok := parsePowerplan(info); ok && pp.FreqMHz == 0 {
			return i
		}
	}
	return -1
}

// applyIdle selects the stock idle plan (manual off) so the miner parks itself using the vendor's own
// idle plan, rather than us synthesizing a 0 MHz manual plan.
func applyIdle() error { return applyIdleThen(nil) }

// applyIdleThen: applyIdle, running landed (when not nil) under planMu right after the Idle landed, before any
// other plan write can follow it.
func applyIdleThen(landed func()) error {
	planMu.Lock()
	defer planMu.Unlock()
	raw, err := stockGet("/mcb/setting")
	if err != nil {
		return err
	}
	var cur map[string]any
	if err := json.Unmarshal(raw, &cur); err != nil {
		return err
	}
	idx := idleLevel(cur)
	if idx < 0 {
		return fmt.Errorf("this firmware has no idle powerplan")
	}
	cur["manual"] = false
	cur["select"] = float64(idx)
	body, _ := json.Marshal(cur)
	if _, err = stockReq("PUT", "/mcb/setting", body); err != nil {
		return fmt.Errorf("the miner refused idle: %v", err)
	}
	if landed != nil {
		landed()
	}
	return nil
}

// ===================== hashrate history (for the chart) =====================

type hrSample struct {
	T   int64   `json:"t"`
	GHS float64 `json:"ghs"`
}
type hrMark struct {
	T    int64  `json:"t"`
	Kind string `json:"kind"` // P=setting changed, T=tuning started, R=restart, RB=reboot, X=share rejected
	Note string `json:"note,omitempty"`
}

const hrMaxPoints = 1440 // one point/minute → 24h

var (
	hrMu       sync.Mutex
	hrHist     []hrSample
	hrMarks    []hrMark
	hrLastTS   int64
	hrLastSave int64
)

// recordHashrate adds at most one sample per minute, and persists at most every 5 min.
// agentAPINoise: in verbose (debug) mode the miner logs every API connection, including the agent's
// own status checks every few seconds ("API: connection from 127.0.0.1 - Accepted", "API: sent all of
// … first go"). They aren't shares, so they stay out of the log. Other API lines (failures,
// device requests) still show.
func agentAPINoise(l string) bool {
	return strings.Contains(l, "API: connection from 127.0.0.1") || strings.Contains(l, "API: sent ") ||
		strings.Contains(l, "API: recv")
}

// clockOK: the miner has no battery-backed clock, so it reads 1970 after boot until NTP sets it.
// Nothing time-stamped is recorded, and the schedule doesn't act, until then.
func clockOK() bool { return time.Now().Year() >= 2024 }

func recordHashrate(ghs float64) {
	if !clockOK() {
		return
	}
	now := time.Now().Unix()
	save := false
	hrMu.Lock()
	if now < hrLastTS-60 {
		// the clock went back (set from a browser, then corrected by a time server): samples from the
		// "future" would hold the chart still until the clock caught up with them
		i := len(hrHist)
		for i > 0 && hrHist[i-1].T > now {
			i--
		}
		hrHist, hrLastTS = hrHist[:i], 0
	}
	if now-hrLastTS >= 60 {
		hrHist = append(hrHist, hrSample{T: now, GHS: ghs})
		if len(hrHist) > hrMaxPoints {
			hrHist = hrHist[len(hrHist)-hrMaxPoints:]
		}
		hrLastTS = now
		// The in-RAM hrHist is the live copy; the on-disk file only has to survive an agent restart. On the
		// Box II (FAT) a write every 5 min would be ~16 MB/day of flash wear (R04), so checkpoint ~hourly there
		// (and before any panel-initiated reboot, via persistHashrate in markCleanReboot). SC Lite keeps 5 min.
		if now-hrLastSave >= int64(hrSaveGap().Seconds()) {
			hrLastSave, save = now, true
		}
	}
	hrMu.Unlock()
	if save {
		persistHashrate()
	}
}

// persistHashrate writes the chart history to disk so it survives agent restarts.
func persistHashrate() {
	hrMu.Lock()
	h := append([]hrSample(nil), hrHist...)
	m := append([]hrMark(nil), hrMarks...)
	hrLastSave = time.Now().Unix()
	hrMu.Unlock()
	b, err := json.Marshal(struct {
		Samples []hrSample `json:"samples"`
		Marks   []hrMark   `json:"marks"`
	}{h, m})
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(*hrPath), 0755)
	_ = cfgWrite(*hrPath, b, 0644)
}

// hrSaveGap: how often the hashrate history is checkpointed to the config store. Hourly on the Box II's FAT
// flash, every 5 min on SC Lite's ext (its original cadence, unchanged).
func hrSaveGap() time.Duration { return cfgCheckpointGap(time.Hour, 5*time.Minute) }

// loadHashrate restores the chart history on boot (dropping anything older than 24h).
func loadHashrate() {
	b, ok := readConfig(*hrPath, json.Valid) // falls back to hashrate.json.bak if a FAT power cut damaged the primary
	if !ok {
		return
	}
	var in struct {
		Samples []hrSample `json:"samples"`
		Marks   []hrMark   `json:"marks"`
	}
	if json.Unmarshal(b, &in) != nil {
		return
	}
	cut := time.Now().Unix() - 24*3600
	hrMu.Lock()
	for _, s := range in.Samples {
		if s.T >= cut {
			hrHist = append(hrHist, s)
		}
	}
	for _, m := range in.Marks {
		if m.T >= cut {
			hrMarks = append(hrMarks, m)
		}
	}
	if len(hrHist) > hrMaxPoints {
		hrHist = hrHist[len(hrHist)-hrMaxPoints:]
	}
	if n := len(hrHist); n > 0 {
		hrLastTS = hrHist[n-1].T
	}
	hrMu.Unlock()
}

// recordMark records a timeline event for the chart. Same-kind marks are deduped within a short
// window; "R" (restart) uses a longer window so the manual Restart button and the reconnect
// uptime-reset detection don't both mark the same restart.
func recordMark(kind, note string) {
	if !clockOK() {
		return
	}
	now := time.Now().Unix()
	dedup := int64(2)
	if kind == "R" {
		dedup = 150
	}
	hrMu.Lock()
	if n := len(hrMarks); n > 0 && hrMarks[n-1].Kind == kind && now-hrMarks[n-1].T < dedup {
		hrMu.Unlock()
		return
	}
	if kind == "X" {
		if r := takePendingReject(now); r != "" {
			note = strings.TrimSpace(note + " · " + r)
		}
	}
	hrMarks = append(hrMarks, hrMark{T: now, Kind: kind, Note: note})
	cut := now - 24*3600
	i := 0
	for i < len(hrMarks) && hrMarks[i].T < cut {
		i++
	}
	hrMarks = hrMarks[i:]
	if len(hrMarks) > 200 {
		// drop the oldest stale/reject marks first: a pool refusing many shares mustn't push the
		// restart/setting marks off the chart
		over := len(hrMarks) - 200
		k := hrMarks[:0]
		for _, m := range hrMarks {
			if over > 0 && (m.Kind == "X" || m.Kind == "S") {
				over--
				continue
			}
			k = append(k, m)
		}
		hrMarks = k
		if len(hrMarks) > 200 {
			hrMarks = hrMarks[len(hrMarks)-200:]
		}
	}
	hrMu.Unlock()
	if kind != "S" && kind != "X" { // stale/rejected shares can come often: they go with the 5-minute save
		persistHashrate() // other marks are rare (restart / preset / tuning)
	}
}

// hrAvgSince: the average of the chart samples taken at or after cut (unix s), and how many there are. The
// history keeps 1440 points, one a minute while the miner answers: with Idle hours or the agent stopped they
// reach back more than 24 h, so a "24 h" figure has to pick its own samples.
func hrAvgSince(cut int64) (float64, int) {
	hrMu.Lock()
	defer hrMu.Unlock()
	var sum float64
	n := 0
	for _, s := range hrHist {
		if s.T >= cut {
			sum += s.GHS
			n++
		}
	}
	if n == 0 {
		return 0, 0
	}
	return sum / float64(n), n
}

func handleHashrate(w http.ResponseWriter, r *http.Request) {
	hrMu.Lock()
	samples := append([]hrSample(nil), hrHist...)
	marks := append([]hrMark(nil), hrMarks...)
	hrMu.Unlock()
	avg, _ := hrAvgSince(time.Now().Unix() - 24*3600)
	now := 0.0
	if n := len(samples); n > 0 {
		now = samples[n-1].GHS
	}
	writeJSON(w, map[string]any{"ok": true, "samples": samples, "marks": marks, "avg": avg, "now": now})
}

var allowedCmds = map[string]bool{"summary": true, "stats": true, "devs": true, "pools": true, "config": true, "version": true, "estats": true}

func handleCmd(w http.ResponseWriter, r *http.Request) {
	cmd := strings.TrimPrefix(r.URL.Path, "/api/cmd/")
	if !allowedCmds[cmd] {
		http.Error(w, "command not allowed", 403)
		return
	}
	out, err := cgminerCmd(cmd)
	if err != nil {
		http.Error(w, "miner API unreachable: "+err.Error(), 502)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(out)
}

// ===================== stock web (JWT) for per-chip icinfo =====================

var (
	jwtMu    sync.Mutex
	jwtToken string
)

// same cipher the stock UI uses: AES-128-CBC, key = 16x '!', IV=0, ZeroPadding, hex
func encStockPw(pw string) string {
	key := []byte("!!!!!!!!!!!!!!!!")
	data := []byte(pw)
	if r := len(data) % 16; r != 0 {
		data = append(data, make([]byte, 16-r)...)
	}
	block, _ := aes.NewCipher(key)
	ct := make([]byte, len(data))
	cipher.NewCBCEncrypter(block, make([]byte, 16)).CryptBlocks(ct, data)
	return hex.EncodeToString(ct)
}

// tryStockPw signs in to the stock web backend with one password. unreachable=true means minerd didn't
// answer at all (still starting), as opposed to answering "wrong password".
func tryStockPw(pw string) (tok string, unreachable bool, err error) {
	q := url.Values{"username": {"admin"}, "password": {encStockPw(pw)}, "cipher": {"true"}}
	r, err := (&http.Client{Timeout: 8 * time.Second}).Get("http://" + *minerWeb + "/user/login?" + q.Encode())
	if err != nil {
		// not the URL: its query holds the password (encrypted with a fixed, public key), and this text ends up
		// in the tuner log, the activity feed and the page
		var ue *url.Error
		if errors.As(err, &ue) {
			err = fmt.Errorf("stock login: %w", ue.Err)
		}
		return "", true, err
	}
	defer r.Body.Close()
	var lj map[string]any
	json.NewDecoder(r.Body).Decode(&lj)
	tok, _ = lj["JWT Token"].(string)
	if tok == "" {
		return "", false, fmt.Errorf("stock login failed (HTTP %d)", r.StatusCode)
	}
	return tok, false, nil
}

// factoryStockPws: Goldshell's factory web passwords (SC Lite 2.2.0 ships 123456789; older units use admin).
var factoryStockPws = []string{"123456789", "admin"}

var (
	autoPwMu      sync.Mutex
	autoPwRefused bool // both factory defaults were refused: don't keep retrying them
)

// autoMinerPass finds the stock web password on its own when none is stored, by trying the factory
// defaults. The stock web API only listens on 127.0.0.1 on the sclite firmware, so the owner never needs
// to know it. Returns done=true when there's nothing left to try (found, already set, or every default
// was refused); done=false when minerd isn't answering yet.
func autoMinerPass() (done bool) {
	autoPwMu.Lock()
	defer autoPwMu.Unlock()
	authMu.RLock()
	have := auth.MinerPass != ""
	authMu.RUnlock()
	if have || autoPwRefused {
		return true
	}
	for _, pw := range factoryStockPws {
		tok, unreachable, err := tryStockPw(pw)
		if unreachable {
			return false
		}
		if err != nil {
			continue
		}
		authMu.Lock()
		auth.MinerPass = pw
		authMu.Unlock()
		if err := saveAuth(); err != nil {
			log.Printf("stock password found but could not be saved: %v", err)
		}
		jwtMu.Lock()
		jwtToken = tok
		jwtMu.Unlock()
		log.Printf("stock web password: factory default detected and stored")
		return true
	}
	log.Printf("stock web password: not a factory default; the owner enters it once on first sign-in")
	autoPwRefused = true
	return true
}

// autoMinerPassLoop retries until minerd is up (it starts after the agent at boot), for up to 15 minutes.
func autoMinerPassLoop() {
	for i := 0; i < 60; i++ {
		if autoMinerPass() {
			return
		}
		time.Sleep(15 * time.Second)
	}
}

func stockLogin() error {
	authMu.RLock()
	pw := auth.MinerPass
	authMu.RUnlock()
	if pw == "" {
		autoMinerPass()
		authMu.RLock()
		pw = auth.MinerPass
		authMu.RUnlock()
		if pw == "" {
			return fmt.Errorf("the stock miner password isn't set: enter it in Settings")
		}
	}
	tok, _, err := tryStockPw(pw)
	if err != nil {
		return err
	}
	jwtMu.Lock()
	jwtToken = tok
	jwtMu.Unlock()
	return nil
}

// stockReq performs an authenticated request to the stock web backend, re-logging in
// once on a 401 or when no token is cached yet. body may be nil (GET/DELETE).
func stockReq(method, path string, body []byte) ([]byte, error) {
	if method == "PUT" && strings.HasPrefix(path, "/mcb/setting") {
		// a power-plan write sets all four fans to one level (intminer): put fans 2+3 back on their offset
		// right away, so Goldshell's fan check never sees them at a level they aren't really turning at
		defer fanPairKick()
	}
	var tok string
	do := func() (*http.Response, error) {
		jwtMu.Lock()
		tok = jwtToken
		jwtMu.Unlock()
		var br io.Reader
		if body != nil {
			br = bytes.NewReader(body)
		}
		req, _ := http.NewRequest(method, "http://"+*minerWeb+path, br)
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		return (&http.Client{Timeout: 10 * time.Second}).Do(req)
	}
	r, err := do()
	if err != nil {
		return nil, friendlyBackendErr(err)
	}
	if r.StatusCode == 401 || tok == "" {
		r.Body.Close()
		if err := stockLogin(); err != nil {
			return nil, friendlyBackendErr(err)
		}
		if r, err = do(); err != nil {
			return nil, friendlyBackendErr(err)
		}
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	if r.StatusCode == 200 && method != "GET" {
		switch path {
		case "/mcb/setting":
			noteMinerChange("clock/voltage change")
		case "/mcb/pools", "/mcb/newpool", "/mcb/delpool":
			noteMinerChange("pool change")
		}
	}
	if r.StatusCode != 200 {
		msg := strings.TrimSpace(string(b))
		if len(msg) > 200 {
			msg = msg[:200]
		}
		if msg == "" {
			return nil, fmt.Errorf("HTTP %d", r.StatusCode)
		}
		return nil, fmt.Errorf("HTTP %d: %s", r.StatusCode, msg)
	}
	return b, nil
}

func stockGet(path string) ([]byte, error) { return stockReq("GET", path, nil) }

type chip struct {
	Index   int     `json:"index"`
	HWErr   int     `json:"hwerr"`
	Perf    int     `json:"perf"`
	Bist    int     `json:"bist"`
	Slowed  bool    `json:"slowed"`
	PerfRel float64 `json:"perf_rel,omitempty"` // perf / board-typical chip (0 = unknown)
	Health  string  `json:"health,omitempty"`   // "" = ok, "watch", "weak"
	Why     string  `json:"why,omitempty"`
}
type chipBoard struct {
	ID       int       `json:"id"`
	Status   int       `json:"status"`
	Reason   string    `json:"reason"`
	Active   bool      `json:"active"`
	Chips    []chip    `json:"chips"`
	Typical  int       `json:"typical_perf,omitempty"` // median perf of working chips
	Temps    []float64 `json:"temps,omitempty"`        // board sensors (°C)
	TempMax  float64   `json:"temp_max,omitempty"`
	VoltMv   int       `json:"volt_mv,omitempty"`
	ClockMHz int       `json:"clock_mhz,omitempty"`
	UptimeS  int       `json:"uptime_s,omitempty"`
	HWTotal  int       `json:"hw_total"`
	NSlowed  int       `json:"nslowed"`
}

// classifyBoard computes each chip's perf-relative and a health flag from the data the
// firmware actually has (HW errors, reported perf, and the driver's DTFS slowdown). It is
// deliberately conservative: perf is a noisy count, so a low number alone rarely flags a
// chip — a slowdown or an error count well above the board's typical chip is the real signal.
func classifyBoard(b *chipBoard) {
	var perfs, hws []int
	for _, c := range b.Chips {
		if c.Perf > 0 {
			perfs = append(perfs, c.Perf)
		}
		hws = append(hws, c.HWErr)
	}
	typ := medianInt(perfs)
	medHw := medianInt(hws)
	b.Typical = typ
	for i := range b.Chips {
		c := &b.Chips[i]
		b.HWTotal += c.HWErr
		if c.Slowed {
			b.NSlowed++
		}
		if typ > 0 && c.Perf > 0 {
			c.PerfRel = float64(c.Perf) / float64(typ)
		}
		// weak: half the work, or a large error count far above the board's typical chip
		switch {
		case c.PerfRel > 0 && c.PerfRel < 0.5:
			c.Health, c.Why = "weak", fmt.Sprintf("does under half the work of a typical chip (%d vs ~%d)", c.Perf, typ)
		case c.HWErr >= 20 && c.HWErr >= 6*max(1, medHw):
			c.Health, c.Why = "weak", fmt.Sprintf("%d HW errors — far above the board's typical chip (~%d)", c.HWErr, medHw)
		case c.Slowed:
			c.Health, c.Why = "watch", "driver has slowed this chip (DTFS — heat or errors)"
		case c.HWErr >= 8 && c.HWErr >= 3*max(1, medHw):
			c.Health, c.Why = "watch", fmt.Sprintf("%d HW errors — above the board's typical chip (~%d)", c.HWErr, medHw)
		case c.PerfRel > 0 && c.PerfRel < 0.8:
			c.Health, c.Why = "watch", fmt.Sprintf("reports %d%% of the board's typical chip", int(c.PerfRel*100))
		default:
			c.Health = ""
			if c.PerfRel > 0 && c.PerfRel < 1 {
				c.Why = fmt.Sprintf("%d%% of typical — within normal scatter for a count", int(c.PerfRel*100))
			}
		}
	}
}

func medianInt(v []int) int {
	if len(v) == 0 {
		return 0
	}
	s := append([]int(nil), v...)
	sort.Ints(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

var (
	chipsCacheMu sync.Mutex
	chipsCache   map[string]any // the last good chip report
	chipsCacheAt time.Time
)

// handleChips: the chip map. While the boards restart (a setting change, the tuner, a boot) the backend
// answers 500 for a minute: the last good report is shown meanwhile, marked as such, or a plain note.
func handleChips(w http.ResponseWriter, r *http.Request) {
	fail := func(err error) {
		chipsCacheMu.Lock()
		c, at := chipsCache, chipsCacheAt
		chipsCacheMu.Unlock()
		if c != nil && time.Since(at) < 20*time.Minute && !minerIsIdleNow() {
			out := map[string]any{}
			for k, v := range c {
				out[k] = v
			}
			note := minerBusyNote("fresh chip data")
			if note == "" {
				note = "The miner's chip report isn't answering right now; the panel keeps trying."
			}
			out["stale"], out["stale_s"] = note, int(time.Since(at).Seconds())
			writeJSON(w, out)
			return
		}
		writeJSON(w, busyOrErr("chip data", err))
	}
	raw, err := stockGet("/dbg/icinfo")
	if err != nil {
		fail(err)
		return
	}
	var outer struct {
		Body string `json:"body"`
	}
	if json.Unmarshal(raw, &outer) != nil || outer.Body == "" {
		fail(fmt.Errorf("unexpected icinfo shape"))
		return
	}
	var inner struct {
		Tabledata []icBoardStatus `json:"tabledata"`
		Drawdata  [][]struct {
			HWErr     int `json:"hwerr"`
			Chipindex int `json:"chipindex"`
			Perf      int `json:"perf"`
			Bist      int `json:"bist"`
		} `json:"drawdata"`
	}
	if err := json.Unmarshal([]byte(outer.Body), &inner); err != nil {
		fail(fmt.Errorf("parse icinfo: %v", err))
		return
	}
	slowed := dtfsSnapshot()
	// per-board telemetry (temps/volt/clock/uptime) from the cgminer side, keyed by board ID
	type tele struct {
		temps    []float64
		tmax     float64
		voltMv   int
		clockMHz int
		uptimeS  int
	}
	telem := map[int]tele{}
	if d := buildDashboard(); d.OK {
		for _, bd := range d.Boards {
			var tmax float64
			for _, t := range bd.Temps {
				if t > tmax {
					tmax = t
				}
			}
			telem[bd.ID] = tele{temps: bd.Temps, tmax: tmax, voltMv: int(bd.VoltageV * 1000), clockMHz: int(bd.ClockMHz), uptimeS: d.Elapsed}
		}
	}
	var boards []chipBoard
	nSlowed := 0
	for i, chips := range inner.Drawdata {
		b := chipBoard{ID: i}
		if i < len(inner.Tabledata) {
			b.Status = inner.Tabledata[i].Status
			b.Reason = strings.TrimSpace(inner.Tabledata[i].Reason)
		}
		alive := aliveBoards(inner.Tabledata)
		for _, c := range chips {
			if (c.Perf > 0 || c.HWErr > 0 || c.Bist > 0) && (alive == nil || alive[i]) {
				b.Active = true
			}
			sl := slowed[ckey{B: i, I: c.Chipindex}]
			b.Chips = append(b.Chips, chip{Index: c.Chipindex, HWErr: c.HWErr, Perf: c.Perf, Bist: c.Bist, Slowed: sl})
		}
		classifyBoard(&b)
		nSlowed += b.NSlowed
		if t, ok := telem[b.ID]; ok {
			b.Temps, b.TempMax, b.VoltMv, b.ClockMHz, b.UptimeS = t.temps, t.tmax, t.voltMv, t.clockMHz, t.uptimeS
		}
		boards = append(boards, b)
	}
	if len(boards) == 0 {
		fail(fmt.Errorf("no boards in the chip report"))
		return
	}
	out := map[string]any{"ok": true, "boards": boards, "slowed": nSlowed}
	chipsCacheMu.Lock()
	chipsCache, chipsCacheAt = out, time.Now()
	chipsCacheMu.Unlock()
	writeJSON(w, out)
}

// ===================== verbose (protocol-dump) toggle =====================
//
// Enables bfgminer's stratum protocol dump so the engine log (minersyslog) carries the
// full every-submit AxeOS-style detail. Backs the config up once, edits it, and restarts
// mining via the cgminer API. Reversible (toggle off restores the key; .sclite-bak keeps
// the original).

func readBfg() (map[string]any, error) {
	b, err := os.ReadFile(*bfgPath)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// verboseFlagPath is the flag file the intminer shim checks: present → launch with -P.
func verboseFlagPath() string { return filepath.Join(filepath.Dir(*authFile), "verbose") }

// shimInstalled reports whether /usr/bin/intminer is our wrapper (adds -P from the flag file)
// rather than the stock binary. When it is, verbose persists across restarts and reboots because
// -P is a launch argument, not a config key the firmware can strip.
func shimInstalled() bool {
	b, err := os.ReadFile(*shimPath)
	if err != nil {
		return false
	}
	return strings.Contains(string(b), brandShim)
}

// restartMiner relaunches the mining engine so the shim re-reads the verbose flag. The shimmed
// process runs as intminer.real; kill both names and let minerd relaunch it.
func restartMiner() {
	os.Remove(sharelogPath()) // start each session's share log fresh (bounds its growth)
	widePath := "PATH=/usr/sbin:/usr/bin:/sbin:/bin:" + os.Getenv("PATH")
	cmd := exec.Command("sh", "-c", "killall intminer.real intminer 2>/dev/null; exit 0")
	cmd.Env = append(os.Environ(), widePath)
	cmd.Run()
}

func handleVerbose(w http.ResponseWriter, r *http.Request) {
	// Preferred path: the intminer shim is installed, so verbose is a persistent launch flag
	// toggled by a flag file. Works across restarts and reboots.
	if shimInstalled() {
		flagP := verboseFlagPath()
		if r.Method != http.MethodPost {
			_, err := os.Stat(flagP)
			v := map[string]any{"ok": true, "enabled": err == nil, "shim": true}
			if b, e := os.ReadFile(verboseAutoOffPath()); e == nil && err != nil {
				var ao map[string]any
				if json.Unmarshal(b, &ao) == nil {
					v["auto_off"] = ao
				}
			}
			writeJSON(w, v)
			return
		}
		var in struct{ Enable bool }
		if !decodeIn(w, r, &in) {
			return
		}
		os.Remove(verboseAutoOffPath()) // switched by hand: the note about the automatic switch-off goes
		if in.Enable {
			if err := os.WriteFile(flagP, []byte{}, 0644); err != nil {
				writeJSON(w, map[string]any{"ok": false, "err": "could not set flag: " + err.Error()})
				return
			}
		} else {
			os.Remove(flagP)
		}
		// like Restart mining: the dashboard shows "restarting", and no stopped/back alerts for this gap
		activity.emit(map[bool]string{true: "⚙ verbose logging switched on — the mining software restarts to apply it", false: "⚙ verbose logging switched off — the mining software restarts to apply it"}[in.Enable])
		markReboot(150*time.Second, 5*time.Minute, "Restarting the mining software to switch verbose logging — hashing resumes shortly.")
		restartMiner()
		note := "verbose ON — mining restarting (~1 min) to apply; the submitted nonce will show in the Mining log"
		if !in.Enable {
			note = "verbose OFF — mining restarting (~1 min)"
		}
		writeJSON(w, map[string]any{"ok": true, "enabled": in.Enable, "shim": true, "note": note})
		return
	}

	// Fallback: no shim — try the config key (works only on firmware that doesn't regenerate it).
	m, err := readBfg()
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "err": "read config: " + err.Error()})
		return
	}
	if r.Method != http.MethodPost {
		en, _ := m["protocol-dump"].(bool)
		writeJSON(w, map[string]any{"ok": true, "enabled": en, "shim": false})
		return
	}
	var in struct{ Enable bool }
	if !decodeIn(w, r, &in) {
		return
	}
	// back up the original once
	bak := *bfgPath + "." + brand + "-bak"
	if _, err := os.Stat(bak); os.IsNotExist(err) {
		if cur, e := os.ReadFile(*bfgPath); e == nil {
			os.WriteFile(bak, cur, 0644)
		}
	}
	if in.Enable {
		m["protocol-dump"] = true
	} else {
		delete(m, "protocol-dump")
	}
	out, _ := json.MarshalIndent(m, "", " ")
	if err := os.WriteFile(*bfgPath, out, 0644); err != nil {
		writeJSON(w, map[string]any{"ok": false, "err": "write config: " + err.Error()})
		return
	}
	// On this firmware the closed `minerd` daemon regenerates bfgminer.json from its own settings
	// every time the mining software (re)starts, stripping any key it didn't write — so a plain
	// config edit like protocol-dump does NOT survive a restart or reboot. Detect that (minerd
	// stages through temp_bfgminer.json) and tell the truth instead of promising it will apply.
	managed := false
	if _, e := os.Stat(filepath.Join(filepath.Dir(*bfgPath), "temp_bfgminer.json")); e == nil {
		managed = true
	}
	note := "saved — takes effect when mining next restarts"
	if managed {
		note = "saved, but this firmware's miner daemon rebuilds its config on every restart and drops it. To make verbose persist, install the intminer shim (see docs) — then this button toggles it reliably."
	}
	writeJSON(w, map[string]any{"ok": true, "enabled": in.Enable, "managed": managed, "shim": false, "note": note})
}

// ===================== controls (clock / voltage / PV / fans) =====================
//
// The stock web exposes the live powerplan at /mcb/setting as a human-readable string:
//
//     "625 MHz 9100 V 50 RPM 50 RPM PV 9400"
//      freq      Vrun    fan1   fan2    PV(peak)
//
// A PUT back to /mcb/setting with a modified manualPowerplan + manual:true applies it LIVE
// through the driver (set device vfff) with NO reboot and WITHOUT the cgminer API "restart"
// that previously took intminer down uncleanly. This is exactly the mechanism the stock UI
// uses to change powerplans, so it is the safe seam for tuning.
//
// Safety: every value is clamped to a conservative window and PV must be >= Vrun. The stock
// UI itself warns that changing the powerplan "may cause permanent damage to the miner", so
// the front-end also requires an explicit confirmation before Apply.

// conservative limits (mV for voltages, MHz, % duty). These are now seeded from the active model profile
// (model.go) at start-up via setWindowsFromModel; at package-init time model == profSCLite, which carries
// the exact pre-port SC Lite figures (625 MHz / 9100 Vrun / 9400 PV, window 300–700 MHz), so SC Lite
// behaviour is unchanged. fanMin/fanMax are model-independent (duty %, the fan guard keeps it safe).
var (
	freqMin, freqMax = model.FreqMin, model.FreqMax
	vrunMin, vrunMax = model.VrunMin, model.VrunMax // run voltage: PV − 300 at PV 9100–9600 (it changes nothing measurable; kept at the stock gap)
	pvMin, pvMax     = model.PVMin, model.PVMax     // 9100: the supply's own floor (measured); lower PV changes nothing
	fanMin, fanMax   = 10, 100                      // 10: fans better than stock can run low; the watchdog guard (fanguard.go) keeps it safe
)

// Unlocked ("overclock") hard ceilings. These are the absolute outer bounds the agent will PUT even
// after the user accepts the unlock disclaimer — they exist only to stop a bug or typo from writing
// a nonsense register value, NOT as a safety recommendation. Going past the conservative window
// above risks permanent damage; that is the user's call once acknowledged.
// Unlocked ("overclock") hard ceilings, seeded from the active model profile (see note above). For SC
// Lite these are the exact pre-port values (freq hard-max 900, vrun 8800–9300, PV 9100–9600). freqHardMin
// is model-independent (a typo/bug floor, not a profile figure).
var (
	freqHardMin, freqHardMax = 200, model.FreqHardMax
	vrunHardMin, vrunHardMax = model.VrunMin, model.VrunMax // the same unlocked: only the clock's window widens
	// PV 9600 is the supply's ceiling, measured: 9600 gave 9.60 V on a multimeter, and 9700 was ignored
	// (the supply stayed where it was). A higher PV would only show a setting the boards don't get.
	pvHardMin, pvHardMax = model.PVMin, model.PVMax // measured on the supply: below 9100 it stays at ~9.1 V, above 9600 it's ignored; unlocking doesn't change that
)

type powerplan struct {
	FreqMHz int `json:"freq_mhz"`
	VrunMv  int `json:"vrun_mv"`
	PVMv    int `json:"pv_mv"`
	Fan1    int `json:"fan1"`
	Fan2    int `json:"fan2"`
}

var ppRe = regexp.MustCompile(`(\d+)\s*MHz\s+(\d+)\s*V\s+(\d+)\s*RPM\s+(\d+)\s*RPM(?:\s+PV\s+(\d+))?`)

func parsePowerplan(s string) (powerplan, bool) {
	if model.Plan == planBox2V {
		return parsePowerplanBox2(s)
	}
	m := ppRe.FindStringSubmatch(s)
	if m == nil {
		return powerplan{}, false
	}
	atoi := func(x string) int { n, _ := strconv.Atoi(x); return n }
	pp := powerplan{
		FreqMHz: atoi(m[1]),
		VrunMv:  atoi(m[2]),
		Fan1:    atoi(m[3]),
		Fan2:    atoi(m[4]),
	}
	if m[5] != "" {
		pp.PVMv = atoi(m[5])
	} else {
		pp.PVMv = pp.VrunMv // no explicit PV (e.g. the idle preset)
	}
	return pp, true
}

func buildPowerplan(p powerplan) string {
	if model.Plan == planBox2V {
		return buildPowerplanBox2(p)
	}
	return fmt.Sprintf("%d MHz %d V %d RPM %d RPM PV %d", p.FreqMHz, p.VrunMv, p.Fan1, p.Fan2, p.PVMv)
}

// planShow: a plan as the owner reads it (activity log, the chart's marks, Controls' "applied"). SC Lite: the
// plan string itself, unchanged. Box II: its raw string reads "690 MHz 42 V 50 RPM 41 RPM", where "42 V" is
// Goldshell's voltage level (not volts) and the 4th field is a voltage-trim PWM (not a fan), so say what it is.
func planShow(p powerplan) string {
	if model.Plan == planBox2V {
		return fmt.Sprintf("%d MHz · %s", p.FreqMHz, box2LevelText(p.VrunMv))
	}
	return buildPowerplan(p)
}

// planShowRaw: planShow for a plan string (as the writers return it); Box II only rewords, SC Lite unchanged.
func planShowRaw(plan string) string {
	if model.Plan == planBox2V {
		if p, ok := parsePowerplanBox2(plan); ok {
			return planShow(p)
		}
	}
	return plan
}

// validateControl clamps/checks a requested powerplan against the safe window.
func validateControl(p powerplan) error {
	if model.Plan == planBox2V {
		return validateControlBox2(p)
	}
	return validateControlEx(p, false)
}

// validateControlEx checks a requested powerplan. With over=false it uses the conservative safe
// window; with over=true (unlock accepted) it uses the wider hard ceilings. The peak≥run rule and
// the fan range always apply.
func validateControlEx(p powerplan, over bool) error {
	fMin, fMax := freqMin, freqMax
	vMin, vMax := vrunMin, vrunMax
	pMin, pMax := pvMin, pvMax
	if over {
		fMin, fMax = freqHardMin, effHard("freq", freqHardMax)
		vMin, vMax = vrunHardMin, effHard("vrun", vrunHardMax)
		pMin, pMax = pvHardMin, effHard("pv", pvHardMax)
	}
	if p.FreqMHz < fMin || p.FreqMHz > fMax {
		return fmt.Errorf("frequency must be %d–%d MHz", fMin, fMax)
	}
	if p.VrunMv < vMin || p.VrunMv > vMax {
		return fmt.Errorf("run voltage must be %d–%d mV", vMin, vMax)
	}
	if p.PVMv < pMin || p.PVMv > pMax {
		return fmt.Errorf("PV (board voltage) must be %d–%d mV", pMin, pMax)
	}
	if p.Fan1 < fanMin || p.Fan1 > fanMax || p.Fan2 < fanMin || p.Fan2 > fanMax {
		return fmt.Errorf("fan duty must be %d–%d%%", fanMin, fanMax)
	}
	if p.PVMv < p.VrunMv {
		return fmt.Errorf("PV (%d) must be ≥ run voltage (%d)", p.PVMv, p.VrunMv)
	}
	return nil
}

// ---- overclock unlock state (persisted acknowledgment) ----
type overclockState struct {
	Acknowledged bool           `json:"acknowledged"`
	At           string         `json:"at"`
	Caps         map[string]int `json:"caps,omitempty"` // measured firmware caps: "freq"|"vrun"|"pv" → mV/MHz
}

// effHard returns the effective hard ceiling for an axis: the compile-time guard, tightened to the
// measured firmware cap when a ceiling probe has recorded one (never looser than the guard).
func effHard(axis string, guard int) int {
	oc := loadOverclock()
	if oc.Caps != nil {
		// a cap only narrows the unlocked range down to the locked window, never below it
		if c, ok := oc.Caps[axis]; ok && c >= capFloor[axis] && c < guard {
			return c
		}
	}
	return guard
}

// capFloor: the locked window's top per axis; a measured cap under it is no cap (the firmware stored something odd)
var capFloor = map[string]int{"freq": freqMax, "vrun": vrunMax, "pv": pvMax}

func overclockPath() string { return filepath.Join(filepath.Dir(*authFile), "overclock.json") }

// ocUnlocked: the owner accepted the unlock AND the model may unlock (the SC Lite always; the SC Box II once
// OverclockUnlock is commissioned). An acknowledgment left on disk from an earlier commissioning unlocks nothing.
func ocUnlocked() bool { return model.Feat.OverclockUnlock && loadOverclock().Acknowledged }

func loadOverclock() overclockState {
	var s overclockState
	if b, err := os.ReadFile(overclockPath()); err == nil {
		json.Unmarshal(b, &s)
	}
	return s
}

func saveOverclock(s overclockState) error {
	b, _ := json.MarshalIndent(s, "", " ")
	_ = os.MkdirAll(filepath.Dir(overclockPath()), 0755)
	return writeAtomic(overclockPath(), b, 0644)
}

// handleOverclock reports/sets the unlock acknowledgment. POST {acknowledge:true} records it (after
// the user accepts the disclaimer in the UI); POST {revoke:true} re-locks the conservative window.
func handleOverclock(w http.ResponseWriter, r *http.Request) {
	if !featGate(w, model.Feat.OverclockUnlock, "overclock unlock") {
		return
	}
	if r.Method == http.MethodPost {
		var in struct {
			Acknowledge bool           `json:"acknowledge"`
			Revoke      bool           `json:"revoke"`
			Caps        map[string]int `json:"caps"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeJSON(w, map[string]any{"ok": false, "err": badJSON(err)})
			return
		}
		// record measured firmware caps without otherwise changing the unlock state
		if in.Caps != nil && !in.Revoke {
			s := loadOverclock()
			if s.Caps == nil {
				s.Caps = map[string]int{}
			}
			if !s.Acknowledged { // they'd only bite at a later unlock, from a result nobody tied to it
				writeJSON(w, map[string]any{"ok": false, "err": "the clock limit is locked: caps only narrow an unlocked window (unlock it, then probe again)"})
				return
			}
			for k, v := range in.Caps {
				if lo, known := capFloor[k]; !known || v < lo {
					writeJSON(w, map[string]any{"ok": false, "err": fmt.Sprintf("not a usable cap: %q %d (a cap only narrows the unlocked range, down to the locked window)", k, v)})
					return
				}
			}
			for k, v := range in.Caps {
				s.Caps[k] = v
			}
			if err := saveOverclock(s); err != nil {
				writeJSON(w, map[string]any{"ok": false, "err": "save failed: " + err.Error()})
				return
			}
			activity.emit("📐 overclock ceilings set to measured firmware caps")
			writeJSON(w, map[string]any{"ok": true, "overclock": s.Acknowledged, "caps": s.Caps, "note": capPlusTrims()})
			return
		}
		var s overclockState
		if in.Acknowledge && !in.Revoke {
			s = overclockState{Acknowledged: true, At: time.Now().Format("2006-01-02 15:04"), Caps: loadOverclock().Caps}
			activity.emit("🔓 overclock limits UNLOCKED — safety caps raised (user acknowledged)")
		} else {
			if getCharge() == "ceiling" {
				ceilAbort.Store(true) // its steps are past the window now: it stops (no result), it doesn't read them as caps
			}
			msg := "🔒 overclock limits re-locked — back to the safe window"
			if cp, ok := readStockBaseline("current"); ok && cp.FreqMHz > freqMax {
				msg += fmt.Sprintf(" (the miner still runs %d MHz until you change it)", cp.FreqMHz)
			}
			activity.emit(msg)
		}
		if err := saveOverclock(s); err != nil {
			writeJSON(w, map[string]any{"ok": false, "err": "save failed: " + err.Error()})
			return
		}
		writeJSON(w, map[string]any{"ok": true, "overclock": s.Acknowledged, "at": s.At, "note": capPlusTrims()})
		return
	}
	s := loadOverclock()
	writeJSON(w, map[string]any{"ok": true, "overclock": s.Acknowledged, "at": s.At})
}

type presetInfo struct {
	Index int       `json:"index"` // list position: what the firmware's "select" takes
	Level int       `json:"level"`
	Raw   string    `json:"raw"`
	Plan  powerplan `json:"plan"`
}

// ===================== "in charge" lock =====================
//
// Mirrors blake2b-asic-control: while a preset is applied or the tuner is running, that
// controller is "in charge" and manual clock/voltage/PV/fan edits are refused server-side.
// "Take manual control" (release) clears it so the user can edit again.
var (
	chargeMu sync.Mutex
	chargeBy string // "" = manual, "preset:<name>", "idle", "tuner"
)

// controlMu: held across "who's in charge?" and the change it allows (a preset, Stock, Idle, the
// schedule's change, a tuner start), so a tuning run can't start between another controller's check and its
// write (that write then took the tuner's charge). Safety stops (fan lock, pool guard, no temperature) don't
// take it: they always win.
var controlMu sync.Mutex

func setCharge(s string) {
	chargeMu.Lock()
	chargeBy = s
	chargeMu.Unlock()
	heatHoldLeft(s)
}

// heatHoldLeft: the charge became s: off the overheat Idle unless s is "idle".
func heatHoldLeft(s string) {
	if s != "idle" {
		heatPending.Store("") // something else is in charge now: no waiting "start when cool"
	}
	if s != "idle" && heatHoldSaved.Load() {
		heatHoldSaved.Store(false)
		os.Remove(heatHoldPath()) // the overheat Idle was left (a preset, Stock, manual control): nothing to hold after a restart
	}
}
func getCharge() string { chargeMu.Lock(); defer chargeMu.Unlock(); return chargeBy }

// safetyGen: bumped by every stop that puts the miner on Idle (the thermal guard, the fan lock, the pool guard, the
// owner's Idle) before it takes the charge and writes its Idle. A writer notes it where it checked who's in charge
// (under controlMu, or as a run starts) and passes it to its write; under planMu (which the Idle's own write needs
// too) a moved generation refuses the write. So a request already on its way can't land after the Idle and wake it,
// nor take the charge back from it. noGen: no such check (the fan curve and the throttle never wake the miner).
var safetyGen atomic.Int64

const noGen = int64(-1)

var errSafetyStop = errors.New("the miner was put on Idle meanwhile (thermal guard, fan lock, pool guard or Idle): nothing was written")

func safetyStop() { safetyGen.Add(1) }

// safetyStopIdle: the thermal guard's start: writes on their way are refused and the charge says idle, in one step
// (a request that noted gen before its charge check sees either both or neither).
func safetyStopIdle() {
	chargeMu.Lock()
	safetyGen.Add(1)
	chargeBy = "idle"
	chargeMu.Unlock()
}

// swapCharge: the charge goes from "from" to "to" only if it's still "from" (a run's end handing it back must not
// overwrite the "idle" a safety stop set meanwhile).
func swapCharge(from, to string) bool {
	chargeMu.Lock()
	if chargeBy != from {
		chargeMu.Unlock()
		return false
	}
	chargeBy = to
	chargeMu.Unlock()
	heatHoldLeft(to)
	return true
}

// genMoved: a stop came after gen was noted.
func genMoved(gen int64) bool { return gen != noGen && safetyGen.Load() != gen }

// setChargeGen: setCharge, unless a stop came after gen was noted (its "idle" stays): false then. gen must be
// noted before the charge checks the caller made (a stop sets "idle" without controlMu).
func setChargeGen(gen int64, s string) bool {
	chargeMu.Lock()
	if genMoved(gen) {
		chargeMu.Unlock()
		return false
	}
	chargeBy = s
	chargeMu.Unlock()
	heatHoldLeft(s)
	return true
}

// putSettingGen: a whole-setting write (a firmware level, a manual plan, a snapshot put back) under planMu, refused
// while mining is held or when a stop came after gen was noted.
func putSettingGen(gen int64, body []byte) error {
	planMu.Lock()
	defer planMu.Unlock()
	if err := miningHoldErr(); err != nil {
		return err
	}
	if genMoved(gen) {
		return errSafetyStop
	}
	_, err := stockReq("PUT", "/mcb/setting", body)
	return err
}

// chargeLabel returns a human string for the UI banner ("" when nothing is in charge).
func chargeLabel() string {
	c := getCharge()
	switch {
	case c == "":
		return ""
	case c == "tuner":
		return "the auto-tuner"
	case c == "idle":
		return "idle mode"
	case c == "stock":
		return "stock settings"
	case c == "ceiling":
		return "the limit probe"
	case strings.HasPrefix(c, "preset:"):
		return "preset " + strings.TrimPrefix(c, "preset:")
	}
	return c
}

// chargeText: chargeLabel as a person reads it (messages: "locked — … is in charge"). chargeLabel itself is the
// in_charge value the page compares ("preset Hashrate", "stock settings"), so it keeps the keys; on the SC Box II
// this one names the stock modes "Stock High" / "Stock Low" (presetLabel), and the stock setting "Stock High".
func chargeText() string {
	c := getCharge()
	if model.Plan == planBox2V {
		switch {
		case c == "stock":
			return box2StockHigh
		case strings.HasPrefix(c, "preset:"):
			return "preset " + presetLabel(strings.TrimPrefix(c, "preset:"))
		}
	}
	return chargeLabel()
}

// handleControl: GET current powerplan + presets + limits; POST applies a custom plan or a preset.
func handleControl(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost && !featGate(w, model.Feat.ClockControl, "clock & power control") {
		return // reading the current plan (GET) stays available on every model; applying one does not
	}
	if r.Method == http.MethodPost {
		if err := miningHoldErr(); err != nil {
			writeJSON(w, map[string]any{"ok": false, "err": err.Error()})
			return
		}
	}
	raw, err := stockGet("/mcb/setting")
	if err != nil {
		if r.Method != http.MethodPost {
			writeJSON(w, busyOrErr("the current settings", err))
		} else {
			writeJSON(w, map[string]any{"ok": false, "err": "the miner's backend isn't answering right now (" + err.Error() + "); try again in a minute"})
		}
		return
	}
	var cur map[string]any
	if err := json.Unmarshal(raw, &cur); err != nil {
		writeJSON(w, map[string]any{"ok": false, "err": "parse setting: " + err.Error()})
		return
	}

	if r.Method != http.MethodPost {
		mp, _ := cur["manualPowerplan"].(string)
		pp, ok := resolveRunningPlan(cur) // what the miner runs: a firmware level (manual off) isn't the manual plan
		if !ok || pp.FreqMHz == 0 {
			pp, _ = parsePowerplan(mp) // idle: the boxes start from the last manual plan
		}
		var presets []presetInfo
		for i, mm := range stockPlans(cur) {
			info, _ := mm["info"].(string)
			lvl := i
			if f, ok := mm["level"].(float64); ok {
				lvl = int(f)
			}
			p, _ := stockPlanOf(mm)
			presets = append(presets, presetInfo{Index: i, Level: lvl, Raw: info, Plan: p})
		}
		manual, _ := cur["manual"].(bool)
		sel := 0
		if f, ok := cur["select"].(float64); ok {
			sel = int(f)
		}
		resp := map[string]any{
			"ok":        true,
			"current":   pp,
			"raw":       mp,
			"manual":    manual,
			"select":    sel,
			"presets":   presets,
			"in_charge": chargeLabel(),
			"busy":      map[bool]string{true: "the strong-chip test (Controls → Chip trims)"}[sctRunningNow()], // owns the clock: Apply is refused
			"idle":      minerIdle(cur),
			"limits": map[string]any{
				"freq": []int{freqMin, freqMax}, "vrun": []int{vrunMin, vrunMax},
				"pv": []int{pvMin, pvMax}, "fan": []int{fanMin, fanMax},
			},
			"hardlimits": map[string]any{
				"freq": []int{freqHardMin, effHard("freq", freqHardMax)}, "vrun": []int{vrunHardMin, effHard("vrun", vrunHardMax)},
				"pv": []int{pvHardMin, effHard("pv", pvHardMax)}, "fan": []int{fanMin, fanMax},
			},
			"overclock": ocUnlocked(),
		}
		if model.Plan == planBox2V {
			resp["box2"] = box2ControlInfo() // lanes C3/C4: the clock window, the V codes and the V rule
		}
		writeJSON(w, resp)
		return
	}

	// POST — apply (or release manual control)
	var in struct {
		Preset *int `json:"preset"` // if set, select a stock preset by its list position (the firmware's "select")
		powerplan
		Confirm  bool `json:"confirm"`
		Release  bool `json:"release"`  // "Take manual control": clear whoever is in charge
		Override bool `json:"override"` // apply past the safe window (requires unlock acknowledged)
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, map[string]any{"ok": false, "err": badJSON(err)})
		return
	}
	controlMu.Lock() // the checks below and the write, in one go (see controlMu): no tuner or strong-chip test start in between
	defer controlMu.Unlock()
	gen := safetyGen.Load() // a safety stop from here on wins over this request (see safetyGen)
	if in.Release {
		switch getCharge() {
		case "tuner":
			writeJSON(w, map[string]any{"ok": false, "err": "the tuner is running — stop it first to take manual control"})
			return
		case "ceiling": // it keeps stepping the clock and puts its baseline back at the end: stop it instead
			writeJSON(w, map[string]any{"ok": false, "err": "the limit probe is running — stop it first to take manual control"})
			return
		}
		if !setChargeGen(gen, "") {
			writeJSON(w, map[string]any{"ok": false, "err": errSafetyStop.Error()})
			return
		}
		pauseScheduleFor("you took manual control")
		activity.emit("🔓 manual control taken")
		writeJSON(w, map[string]any{"ok": true, "note": "manual control — the preset no longer drives the miner"})
		return
	}
	if lbl := chargeText(); lbl != "" {
		writeJSON(w, map[string]any{"ok": false, "locked": true, "err": "locked — " + lbl + " is in charge. Take manual control first."})
		return
	}
	if sctRunning() { // a new board clock would run the raised chips' test trims on top of it
		writeJSON(w, map[string]any{"ok": false, "err": "the strong-chip test is running (Controls → Chip trims): stop it first"})
		return
	}
	if !in.Confirm {
		writeJSON(w, map[string]any{"ok": false, "err": "confirmation required"})
		return
	}

	var applied string
	if in.Preset != nil {
		pp, ok := stockPlanAt(cur, *in.Preset)
		if !ok {
			writeJSON(w, map[string]any{"ok": false, "err": "no such stock preset"})
			return
		}
		if err := plusTrimErr(pp.FreqMHz); err != nil {
			writeJSON(w, map[string]any{"ok": false, "err": err.Error()})
			return
		}
		if pp.FreqMHz > 0 && (min(pp.Fan1, pp.Fan2) < fanFloor() || heatGuardActive()) {
			// its fans are under the floor, or it's hot: the level's plan as a manual one, through the checks
			plan, err := mutatePlanGen(gen, func(x *powerplan) { *x = pp })
			if err != nil {
				writeJSON(w, map[string]any{"ok": false, "err": "Not applied: " + err.Error()})
				return
			}
			shown := planShowRaw(plan)
			activity.emit("⚙ powerplan applied: stock preset " + shown + " (fans kept up)")
			recordMark("P", "stock preset "+shown)
			writeJSON(w, map[string]any{"ok": true, "applied": "stock preset " + shown + " (fans kept at the floor / heat guard)", "note": "applied live — no reboot needed"})
			return
		}
		if model.Plan == planBox2V && pp.FreqMHz > 0 {
			// a soft-idled board (pgadisable) stays off under a bare select: resume it first, as every other wake
			// path does (refused while a guard parks or holds the miner, so this never reports ok on a parked board)
			if err := box2WakeResume(gen); err != nil {
				writeJSON(w, map[string]any{"ok": false, "err": "Not applied: " + err.Error()})
				return
			}
		}
		cur["manual"] = false
		cur["select"] = float64(*in.Preset)
		if pp.FreqMHz == 0 {
			applied = "stock idle"
		} else {
			applied = "stock preset " + planShow(pp)
		}
	} else if model.Plan == planBox2V {
		// lanes C3/C4: a custom clock (+ V code with VoltageControl) on Box II goes through the Box II writer
		// (window/V rule, refuse-not-clamp, read-back confirmation, revert on mismatch). The SC Lite window check
		// below must never see it: Box II's mV window is 0–0, so a V-less request would pass it as "0 V".
		// override (C7, owner build): a clock above the stock window needs override, as on the SC Lite, and
		// override needs the unlock commissioned (OverclockUnlock) and its risk accepted (ocUnlocked). Then the
		// request goes through exactly the same writer and checks as any other (box2ApplyControl → box2CheckTuple
		// with the window's top at clockWindowHi(), box2OverclockErr's stock-level rule, the "+" trims, the
		// read-back confirm): override only lets a clock above 690 be asked for, it skips no check. The public
		// build (OCHardMax 0) and a unit without the unlock refuse it as before.
		if in.Override && (publicBuild || model.OCHardMax <= freqMax || !model.Feat.OverclockUnlock) {
			writeJSON(w, map[string]any{"ok": false, "err": "overclock is not available on " + model.Name + " here: the clock window is " +
				fmt.Sprintf("%d–%d MHz", freqMin, freqMax)})
			return
		}
		if in.Override && !ocUnlocked() {
			writeJSON(w, map[string]any{"ok": false, "err": "overclock not unlocked — accept the unlock disclaimer first"})
			return
		}
		if !in.Override && in.FreqMHz > freqMax {
			writeJSON(w, map[string]any{"ok": false, "err": fmt.Sprintf("Not applied: refused: %d MHz is outside this model's clock window (%d–%d MHz)", in.FreqMHz, freqMin, freqMax)})
			return
		}
		plan, err := box2ApplyControl(gen, in.powerplan)
		if err != nil {
			writeJSON(w, map[string]any{"ok": false, "err": "Not applied: " + err.Error()})
			return
		}
		pp, _ := parsePowerplanBox2(plan)
		note := "applied live and confirmed by the miner"
		if eff := box2PLLClock(float64(pp.FreqMHz)); eff > 0 && eff != float64(pp.FreqMHz) {
			note += fmt.Sprintf(" (the chips' PLL runs %g MHz for %d MHz)", eff, pp.FreqMHz)
		}
		shown := planShowRaw(plan)
		activity.emit("⚙ powerplan applied: " + shown)
		recordMark("P", shown)
		writeJSON(w, map[string]any{"ok": true, "applied": shown, "note": note})
		return
	} else {
		p := in.powerplan
		keepFans := p.Fan1 == 0 && p.Fan2 == 0 // the page leaves the fans alone: the ones running now
		if keepFans {
			p.Fan1, p.Fan2 = 50, 50 // (for the check; replaced below)
		}
		over := in.Override && loadOverclock().Acknowledged
		if in.Override && !over {
			writeJSON(w, map[string]any{"ok": false, "err": "overclock not unlocked — accept the unlock disclaimer first"})
			return
		}
		if err := validateControlEx(p, over); err != nil {
			writeJSON(w, map[string]any{"ok": false, "err": err.Error()})
			return
		}
		if err := plusTrimErr(p.FreqMHz); err != nil {
			writeJSON(w, map[string]any{"ok": false, "err": err.Error()})
			return
		}
		var werr error // only now: a refused request mustn't take the miner out of Idle
		planMu.Lock()
		if werr = miningHoldErr(); werr == nil && genMoved(gen) {
			werr = errSafetyStop
		}
		if werr == nil {
			cur, _, werr = wakeIfIdle(cur)
		}
		planMu.Unlock()
		if werr != nil {
			writeJSON(w, map[string]any{"ok": false, "err": werr.Error()})
			return
		}
		if rp, ok := resolveRunningPlan(cur); keepFans && ok && rp.FreqMHz > 0 {
			p.Fan1, p.Fan2 = rp.Fan1, rp.Fan2
		}
		if fl := fanFloor(); p.FreqMHz > 0 { // the floor and the heat guard, as on every other path
			p.Fan1, p.Fan2 = max(p.Fan1, fl), max(p.Fan2, fl)
		}
		p.Fan1, p.Fan2 = heatGuardFan(p.Fan1), heatGuardFan(p.Fan2)
		cur["manual"] = true
		cur["manualPowerplan"] = buildPowerplan(p)
		applied = buildPowerplan(p)
		if over && (p.FreqMHz > freqMax || p.VrunMv > vrunMax || p.PVMv > pvMax) {
			applied += " (OVERCLOCK — past safe limits)"
		}
	}

	body, _ := json.Marshal(cur)
	if err := putSettingGen(gen, body); err != nil { // (cur was read before the checks: the holds again, under planMu)
		writeJSON(w, map[string]any{"ok": false, "err": "Not applied: " + err.Error()})
		return
	}
	activity.emit("⚙ powerplan applied: " + applied)
	recordMark("P", applied)
	writeJSON(w, map[string]any{"ok": true, "applied": applied, "note": "applied live — no reboot needed"})
}

// resolveRunningPlan returns the powerplan the miner is actually running right now: the
// manualPowerplan when manual is on, else the selected firmware level's plan string.
func resolveRunningPlan(cur map[string]any) (powerplan, bool) {
	if manual, _ := cur["manual"].(bool); manual {
		if s, _ := cur["manualPowerplan"].(string); s != "" {
			return parsePowerplan(s)
		}
	}
	if pp, ok := stockPlanAt(cur, stockSelect(cur)); ok {
		return pp, true
	}
	// fall back to whatever manualPowerplan holds
	if s, _ := cur["manualPowerplan"].(string); s != "" {
		return parsePowerplan(s)
	}
	return powerplan{}, false
}

// mutatePlan reads the live setting, applies mut to the running plan, and PUTs it back as a
// manual plan (manual:true) so the change is honored. tempcontrol is left exactly as it was,
// preserving the firmware's own hard thermal cutoff. Returns the plan string written.
// wakeWait: how long the boards take to power up after leaving idle (an owner's log: the engine restarts at
// once and every board is at its clock about 20 s later).
var wakeWait = 20 * time.Second

// wakeIfIdle: while idle the hash boards are powered off and the engine ignores clock changes ("Device is
// Poweroff, do nothing"), so a manual plan doesn't wake it. What does is going back to the firmware's own
// hashing plan (manual off, its select), as Controls' stock preset does: the engine restarts and powers the
// boards. Returns the setting as it is after waking (re-read), or cur unchanged when not idle.
func wakeIfIdle(cur map[string]any) (map[string]any, bool, error) {
	if !minerIdle(cur) {
		return cur, false, nil
	}
	if err := miningHoldErr(); err != nil {
		return cur, false, err
	}
	if err := heatCoolErr(); err != nil {
		return cur, false, err
	}
	if left := fanSweepUntil.Load() - monoSec(); left > 0 {
		return cur, false, fmt.Errorf("%s fan test is sweeping the fans (boards idle): try again in %d s", OemOf(), left)
	}
	i := firstHashingPlan(cur)
	if i < 0 {
		return cur, false, fmt.Errorf("the firmware has no hashing plan to leave idle with")
	}
	cur["manual"] = false
	cur["select"] = float64(i)
	body, _ := json.Marshal(cur)
	if _, err := stockReq("PUT", "/mcb/setting", body); err != nil {
		return cur, false, fmt.Errorf("couldn't leave idle: %v", err)
	}
	time.Sleep(wakeWait)
	raw, err := stockGet("/mcb/setting")
	if err != nil {
		return cur, true, err
	}
	var now map[string]any
	if err := json.Unmarshal(raw, &now); err != nil {
		return cur, true, err
	}
	return now, true, nil
}

// planMu: one read-modify-write of /mcb/setting at a time. Without it the fan curve's write (read at clock A,
// written back with new fans) reverts a preset's or the tuner's clock written in between, or wakes an Idle
// applied between its idle check and its write.
var planMu sync.Mutex

// errMinerIdle: mutatePlanNoWake found the miner on Idle and wrote nothing.
var errMinerIdle = errors.New("the miner is idle")

func mutatePlan(mut func(*powerplan)) (string, error) { return mutatePlanEx(mut, true, noGen) }

// mutatePlanGen: mutatePlan for a writer that checked who's in charge (gen: safetyGen then): refused
// (errSafetyStop) when the miner was put on Idle since.
func mutatePlanGen(gen int64, mut func(*powerplan)) (string, error) {
	return mutatePlanEx(mut, true, gen)
}

// mutatePlanNoWake: as mutatePlan, but on Idle it writes nothing (errMinerIdle): for the fan curve and the
// throttle, which must never restart hashing.
func mutatePlanNoWake(mut func(*powerplan)) (string, error) { return mutatePlanEx(mut, false, noGen) }

// clockWindowHi: the fastest clock (MHz) anything may run now: 700, or the unlocked ceiling once the owner has
// accepted the clock-limit unlock. A chip trimmed faster than its board counts too.
func clockWindowHi() int {
	if hi, ok := ceilBox2Window(); ok { // Box II: the clock-ceiling probe's own window, only while it runs
		return hi
	}
	if ocUnlocked() {
		return effHard("freq", freqHardMax)
	}
	return freqMax
}

func mutatePlanEx(mut func(*powerplan), wake bool, gen int64) (string, error) {
	if model.Plan == planBox2V {
		return mutatePlanBox2(mut, wake, gen)
	}
	planMu.Lock()
	defer planMu.Unlock()
	if genMoved(gen) {
		return "", errSafetyStop
	}
	raw, err := stockGet("/mcb/setting")
	if err != nil {
		return "", err
	}
	var cur map[string]any
	if err := json.Unmarshal(raw, &cur); err != nil {
		return "", err
	}
	if !wake && minerIdle(cur) {
		return "", errMinerIdle
	}
	if cur, _, err = wakeIfIdle(cur); err != nil {
		return "", err
	}
	if genMoved(gen) { // (the wake waits wakeWait)
		return "", errSafetyStop
	}
	pp, ok := resolveRunningPlan(cur)
	if ok && pp.FreqMHz == 0 { // idle now: start from the firmware's normal plan (the idle one has fans at 100)
		if i := firstHashingPlan(cur); i >= 0 {
			pp, ok = stockPlanAt(cur, i)
		}
	}
	if !ok {
		return "", fmt.Errorf("could not read running powerplan")
	}
	before := pp
	mut(&pp)
	if pp.FreqMHz <= 0 {
		// a plan with no clock (or a negative one, from a damaged or hand-edited file) would skip every check
		// below: Idle is the firmware's own idle plan (applyIdle), never a made-up manual one
		return "", fmt.Errorf("refused: %d MHz is not a clock (Idle is its own plan)", pp.FreqMHz)
	}
	if (pp.FreqMHz != before.FreqMHz || pp.PVMv != before.PVMv || pp.VrunMv != before.VrunMv) && poolHoldErr() != nil {
		// held by the pool guard (its Idle may have failed): fans may still change, the clock may not
		return "", poolHoldErr()
	}
	if pp.FreqMHz != before.FreqMHz && pp.FreqMHz > 0 {
		// every automated clock write (the tuner, its crash recovery, the schedule, the limit probe, presets) comes
		// through here: never past the window the owner has unlocked, whatever a file on disk says
		fHi := clockWindowHi()
		if pp.FreqMHz > fHi || pp.FreqMHz < freqHardMin {
			return "", fmt.Errorf("refused: %d MHz is outside %d–%d MHz (the clock window now)", pp.FreqMHz, freqHardMin, fHi)
		}
		if err := plusTrimErr(pp.FreqMHz); err != nil && pp.FreqMHz > before.FreqMHz {
			return "", fmt.Errorf("refused: %v", err)
		}
	}
	// the supply's measured range: it ignores a PV above 9600 (a plan asking more would leave the boards where
	// they were while showing 9600+), and run voltage can't be above PV
	if pp.PVMv > pvHardMax {
		pp.PVMv = pvHardMax
	}
	if pp.FreqMHz > 0 && pp.PVMv >= 8000 && pp.PVMv < pvHardMin {
		pp.PVMv = pvHardMin // an old preset under the supply's floor: the boards get 9100 anyway, so say so
	}
	if pp.FreqMHz > 0 && pp.PVMv > 0 && (pp.VrunMv < vrunHardMin || pp.VrunMv > vrunHardMax) {
		pp.VrunMv = min(max(pp.PVMv-300, vrunHardMin), vrunHardMax) // the stock gap (run voltage moves nothing)
	}
	if pp.VrunMv > pp.PVMv && pp.PVMv > 0 {
		pp.VrunMv = pp.PVMv
	}
	if pp.FreqMHz > 0 && (pp.PVMv < pvHardMin || pp.VrunMv < vrunHardMin || pp.VrunMv > pp.PVMv) {
		return "", fmt.Errorf("refused: PV %d / run voltage %d is outside what the supply takes (PV %d–%d, run voltage at most PV)", pp.PVMv, pp.VrunMv, pvHardMin, pvHardMax)
	}
	if pp.FreqMHz > 0 { // every path (Controls, presets, the schedule, the tuner) keeps the fans at or over the floor
		fl := fanFloor()
		if pp.Fan1 < fl {
			pp.Fan1 = fl
		}
		if pp.Fan2 < fl {
			pp.Fan2 = fl
		}
	}
	pp.Fan1, pp.Fan2 = heatGuardFan(pp.Fan1), heatGuardFan(pp.Fan2) // never pull the fans down while it's this hot
	pp.Fan1, pp.Fan2 = min(pp.Fan1, fanMax), min(pp.Fan2, fanMax)   // a level over 100 is "PWM off" to the driver
	plan := buildPowerplan(pp)
	if idx := idleLevel(cur); idx >= 0 && stockSelect(cur) == idx { // leaving idle: off the idle plan too
		if i := firstHashingPlan(cur); i >= 0 {
			cur["select"] = float64(i)
		}
	}
	if m, _ := cur["manual"].(bool); m && cur["manualPowerplan"] == plan {
		// the same plan again (the fan curve re-sends every cooldown): only when the fans have drifted from it,
		// so the backend doesn't rewrite its settings every 45 s for nothing
		if lv, err := fanLvl("fan1"); err == nil && lv == pp.Fan1 {
			return plan, nil
		}
	}
	cur["manual"] = true
	cur["manualPowerplan"] = plan
	body, _ := json.Marshal(cur)
	if _, err := stockReq("PUT", "/mcb/setting", body); err != nil {
		if strings.HasPrefix(err.Error(), "HTTP 4") {
			return "", putRefusedErr{err} // the backend answered and turned the plan down (the limit probe's "cap")
		}
		return "", err
	}
	return plan, nil
}

// putRefusedErr: Goldshell's backend answered a plan write with a 4xx: it refused that plan. Not a backend that
// didn't answer, and not one of the panel's own refusals (the clock window, a hold).
type putRefusedErr struct{ error }

func (e putRefusedErr) Unwrap() error { return e.error }

// writeFansUnlessIdle sets both fans, except while the miner runs its idle plan: fans are part of the
// power plan, and a manual plan with fans would take the miner out of idle (and start it hashing).
func writeFansUnlessIdle(pct int) (idle bool, err error) {
	raw, err := stockGet("/mcb/setting")
	if err != nil {
		return false, err
	}
	var cur map[string]any
	if err := json.Unmarshal(raw, &cur); err != nil {
		return false, err
	}
	if minerIdle(cur) {
		return true, nil
	}
	_, err = mutatePlanNoWake(func(p *powerplan) { p.Fan1, p.Fan2 = pct, pct })
	if err == errMinerIdle { // put on Idle since the check above
		return true, nil
	}
	return false, err
}

// minerIdle: the miner is on its idle plan (picked from the Presets page, Controls, the schedule or the
// pool guard), or on a manual plan with no clock.
func minerIdle(cur map[string]any) bool {
	if pp, ok := resolveRunningPlan(cur); ok && pp.FreqMHz == 0 {
		return true
	}
	if idx := idleLevel(cur); idx >= 0 && stockSelect(cur) == idx {
		if m, _ := cur["manual"].(bool); !m {
			return true
		}
	}
	return false
}

// ===================== fan curve + thermal guard =====================
//
// Ported from the user's blake2b-asic-control (fan_addon.py): a piecewise-linear
// temperature→fan% curve driven by the hottest board sensor, re-asserted on a cooldown
// (the firmware walks fan settings back toward tempcontrol's own target, so we re-send),
// with optional smoothing. The firmware's tempcontrol is left ON so its hard cut-off
// (92°C SC Lite / 86°C Box II, from the model profile) stays as the final backstop. A thermal
// guard layers on top: at abort_c the curve is overridden and fans forced to 100%; an optional
// throttle tier steps the clock down.

type curvePoint struct {
	Temp float64 `json:"temp"`
	Fan  float64 `json:"fan"`
}

type fanConfig struct {
	Enabled        bool         `json:"enabled"`
	Profile        string       `json:"profile"` // builtin key, or "custom"
	Points         []curvePoint `json:"points"`  // used when Profile=="custom"
	Smoothing      int          `json:"smoothing"`
	PollS          int          `json:"poll_s"`
	CooldownS      int          `json:"cooldown_s"`
	AbortC         float64      `json:"abort_c"`
	ThrottleEnable bool         `json:"throttle_enable"`
	ThrottleC      float64      `json:"throttle_c"`
	ThrottleStep   int          `json:"throttle_step"` // MHz per step
	ResumeC        float64      `json:"resume_c"`
	// the curve library (blake2b-asic-control parity): your own named curves, each with its own settings
	Curves map[string]fanCurve `json:"curves,omitempty"`
}

// fanCurve is one curve in the library: points plus the settings it runs with.
type fanCurve struct {
	Label     string       `json:"label"`
	Points    []curvePoint `json:"points"`
	AbortC    float64      `json:"abort_c"`
	CooldownS int          `json:"cooldown_s"`
	PollS     int          `json:"poll_s"`
	Smoothing int          `json:"smoothing"`
	ForceTC   bool         `json:"force_tempcontrol_on"`
	Mode      string       `json:"mode,omitempty"` // "" / "curve" or "steps" (built-in stock profile)
}

// the app's three built-in curves (fan_addon.py BUILTIN_CURVES), same keys, labels and settings
var builtinOrder = []string{"curve-60", "curve-balanced", "curve-quiet", "steps-default"}
var builtinCurves = map[string][]curvePoint{
	"curve-60":       {{50, 45}, {57, 60}, {62, 75}, {68, 90}, {74, 100}},
	"curve-balanced": {{55, 40}, {65, 60}, {72, 80}, {80, 100}},
	"curve-quiet":    {{60, 30}, {72, 50}, {80, 75}, {85, 100}},
	"steps-default":  {{70, 70}, {80, 80}, {85, 90}}, // the dashboard's stock step profile
}
var builtinLabels = map[string]string{
	"curve-60":       "Curve: cool, holds about 60 °C",
	"curve-balanced": "Curve: balanced, about 65-70 °C",
	"curve-quiet":    "Curve: quiet, lets boards run warmer",
	"steps-default":  "Steps 70/80/85 → fan 70/80/90",
}
var builtinSmoothing = map[string]int{"curve-60": 1, "curve-balanced": 1, "curve-quiet": 2, "steps-default": 0}

// builtinMode: "steps" holds each step's fan from its temperature up (below the first step the miner's own
// fan control is left in charge); everything else is a "curve" (a line through the points).
var builtinMode = map[string]string{"steps-default": "steps"}

func curveMode(c fanConfig) string {
	if m := builtinMode[c.Profile]; m != "" {
		return m
	}
	return "curve"
}

// builtinCurve returns a built-in as a library curve (poll 10 s, re-send 45 s, safety = the model's abort
// default: 88 °C on the SC Lite, 82 °C on the SC Box II — always under the cut-off).
func builtinCurve(k string) (fanCurve, bool) {
	pts, ok := builtinCurves[k]
	if !ok {
		return fanCurve{}, false
	}
	return fanCurve{Label: builtinLabels[k], Points: pts, AbortC: abortDefC, CooldownS: 45, PollS: 10, Smoothing: builtinSmoothing[k], ForceTC: true, Mode: builtinMode[k]}, true
}

// lookupCurve resolves a curve key: a built-in, one of yours, or a tuned preset's own ("tuned:<preset>").
func lookupCurve(c fanConfig, k string) (fanCurve, bool) {
	if bc, ok := builtinCurve(k); ok {
		return bc, true
	}
	if uc, ok := c.Curves[k]; ok {
		return uc, true
	}
	if strings.HasPrefix(k, "tuned:") {
		name := strings.TrimPrefix(k, "tuned:")
		for _, p := range loadPresets() {
			if p.Name == name && len(p.FanPoints) >= 2 {
				lbl := "Tuned " + p.Name
				if p.TargetC > 0 {
					lbl += fmt.Sprintf(" (%d °C)", p.TargetC)
				}
				if p.FanEdited {
					lbl += ", edited"
				}
				fc := fanCurve{Label: lbl, Points: p.FanPoints, AbortC: c.AbortC, CooldownS: c.CooldownS, PollS: c.PollS, Smoothing: c.Smoothing, ForceTC: true}
				if p.FanAbortC > 0 {
					fc.AbortC = p.FanAbortC
				}
				if p.FanCooldown > 0 {
					fc.CooldownS = p.FanCooldown
				}
				if p.FanPoll > 0 {
					fc.PollS = p.FanPoll
				}
				if p.FanSmooth != nil {
					fc.Smoothing = *p.FanSmooth
				}
				return fc, true
			}
		}
	}
	return fanCurve{}, false
}

func defaultFanConfig() fanConfig {
	return fanConfig{
		// on from the first start (no saved fan settings yet): the balanced curve holds about 65–70 °C, and
		// at the model's abort temperature (88 °C SC Lite / 82 °C Box II) the fans go to 100 %. A saved
		// choice (on or off) is kept as it is. Every threshold comes from the active model profile.
		Enabled: true, Profile: "curve-balanced", Smoothing: 1,
		PollS: 10, CooldownS: 45, AbortC: abortDefC, ThrottleEnable: false,
		ThrottleC: throttleDefC, ThrottleStep: 25, ResumeC: resumeDefC,
	}
}

var (
	fanMu  sync.RWMutex
	fanCfg = defaultFanConfig()
	fanSt  struct {
		MaxT        float64
		HotBoard    int
		Target      int
		Guard       string // "", "abort", "throttle"
		Note        string
		LastWrite   time.Time
		LastPct     int // the level of the last write that landed
		SavedFreq   int // clock to restore after throttle (0 = none saved)
		ThrottledTo int // the clock the throttle last set (with SavedFreq)
	}
	fanHist []float64
)

func loadFanConfig() {
	b, ok := readConfig(*fanPath, json.Valid) // falls back to fan.json.bak after a FAT power cut
	if !ok {
		return // keep defaults
	}
	c := defaultFanConfig() // a field missing from the file keeps its default (abort_c 0 would mean "always too hot")
	if json.Unmarshal(b, &c) == nil {
		if c.Profile == "curve-cool" {
			c.Profile = "curve-60"
		}
		// safety/throttle temperatures stop at the model's abort-max (cutoff-2): 90 °C on the SC Lite
		// (cut-off 92), 84 °C on the SC Box II (cut-off 86). A saved file from another model, or one edited
		// by hand, is pulled back under this model's cut-off so a guard can never sit above it (dead code).
		if c.AbortC > abortMaxC {
			c.AbortC = abortMaxC
		}
		if c.ThrottleC > abortMaxC {
			c.ThrottleC = abortMaxC
		}
		if c.ResumeC >= c.AbortC && c.AbortC > 0 {
			c.ResumeC = c.AbortC - 5
		}
		for k, cv := range c.Curves {
			if cv.AbortC > abortMaxC {
				cv.AbortC = abortMaxC
				c.Curves[k] = cv
			}
		}
		// an old single custom curve becomes "My curve" in the library
		if c.Profile == "custom" && len(c.Points) >= 2 && len(c.Curves) == 0 && !presetOwnsCurve(c.Points) {
			c.Curves = map[string]fanCurve{"my-curve": {Label: "My curve", Points: c.Points, AbortC: c.AbortC, CooldownS: c.CooldownS, PollS: c.PollS, Smoothing: c.Smoothing, ForceTC: true}}
			c.Profile = "my-curve"
		}
		if sc, fixed := sanitizeFanConfig(c); len(fixed) > 0 {
			c = sc
			log.Printf("fan.json: out-of-range settings set back to their defaults: %s", strings.Join(fixed, ", "))
		}
		fanMu.Lock()
		fanCfg = c
		fanMu.Unlock()
	}
}

// presetOwnsCurve: these points are a saved preset's own curve (applied by the preset, not an old custom curve).
func presetOwnsCurve(pts []curvePoint) bool {
	for _, p := range loadPresets() {
		if slices.Equal(p.FanPoints, pts) {
			return true
		}
	}
	return false
}

// fitResume: the throttle's resume temperature stays below a (curve's) lower safety temperature.
func fitResume(c *fanConfig) {
	if c.AbortC > 0 && c.ResumeC >= c.AbortC {
		c.ResumeC = math.Max(40, c.AbortC-5)
	}
}

func saveFanConfig(c fanConfig) error {
	_ = os.MkdirAll(filepath.Dir(*fanPath), 0755)
	b, _ := json.MarshalIndent(c, "", " ")
	return writeAtomic(*fanPath, b, 0644) // flushed before the rename: a power cut can't leave an empty fan.json
}

func curvePoints(c fanConfig) []curvePoint {
	if c.Profile != "custom" {
		if fc, ok := lookupCurve(c, c.Profile); ok && len(fc.Points) >= 2 {
			return fc.Points
		}
	}
	if len(c.Points) >= 2 {
		return c.Points
	}
	return builtinCurves["curve-balanced"]
}

// curveFan: fan % on the line through the points (flat before the first, after the last).
func curveFan(pts []curvePoint, temp float64) float64 {
	if len(pts) == 0 {
		return 70
	}
	s := append([]curvePoint(nil), pts...)
	sort.Slice(s, func(i, j int) bool { return s[i].Temp < s[j].Temp })
	if temp <= s[0].Temp {
		return s[0].Fan
	}
	for i := 0; i+1 < len(s); i++ {
		if temp <= s[i+1].Temp {
			t0, f0, t1, f1 := s[i].Temp, s[i].Fan, s[i+1].Temp, s[i+1].Fan
			d := t1 - t0
			if d < 1e-6 {
				d = 1e-6
			}
			return f0 + (temp-t0)/d*(f1-f0)
		}
	}
	return s[len(s)-1].Fan
}

func clampf(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// stepFan: the fan of the last step at or below temp, or -1 below the first step (leave the miner's own).
func stepFan(pts []curvePoint, temp float64) float64 {
	s := append([]curvePoint(nil), pts...)
	sort.Slice(s, func(i, j int) bool { return s[i].Temp < s[j].Temp })
	f := -1.0
	for _, p := range s {
		if temp >= p.Temp {
			f = p.Fan
		}
	}
	return f
}

// targetFan applies smoothing the same way fan_addon.target_fan does. -1 = leave the miner's own control.
func targetFan(c fanConfig, temp float64) int {
	if curveMode(c) == "steps" {
		f := stepFan(curvePoints(c), temp)
		if f < 0 {
			return -1
		}
		return int(math.Round(clampf(f, float64(fanFloor()), 100)))
	}
	instant := curveFan(curvePoints(c), temp)
	if c.Smoothing <= 0 {
		return int(math.Round(clampf(instant, float64(fanFloor()), 100)))
	}
	keep := 4
	if c.Smoothing >= 2 {
		keep = 10
	}
	fanMu.Lock()
	fanHist = append([]float64{instant}, fanHist...)
	if len(fanHist) > keep {
		fanHist = fanHist[:keep]
	}
	var num, den float64
	for i, v := range fanHist {
		w := float64(len(fanHist) - i)
		num += v * w
		den += w
	}
	fanMu.Unlock()
	avg := num / den
	out := math.Max(avg, instant-10) // slowing is gentle, heating is handled fast
	return int(math.Round(clampf(out, float64(fanFloor()), 100)))
}

// maxBoardTemp returns the hottest valid board sensor reading and that board's id.
func maxBoardTemp(d dashboard) (float64, int, bool) {
	best, board, have := -1000.0, -1, false
	for _, b := range d.Boards {
		// a board that stopped hashing still counts while it reports a real temperature (one that dropped
		// out hot is the one that matters); boards without a sensor report -150
		for _, t := range b.Temps {
			// a sensor that can't be read says -150; 0 (or a few °C) inside a running miner is a sensor fault too
			if t >= 5 && t < 150 && t > best {
				best, board, have = t, b.ID, true
			}
		}
	}
	return best, board, have
}

// boardLast: each board's last valid reading (fanLoop only). A board whose sensors miss a poll (-150, 0) or that
// is missing from the miner's device list for one isn't read as cool: its last reading counts for heatHoldMiss
// more polls (a board read hot, then not at all every other poll, otherwise never reached the abort tier's Idle).
type boardSeen struct {
	t    float64
	miss int
}

var boardLast = map[int]*boardSeen{}

const heatHoldMiss = 2

// guardTemp: maxBoardTemp, with each board's last valid reading standing in for heatHoldMiss missed polls (while
// another board still reads: with none, it is the no-temperature rule's case).
func guardTemp(d dashboard) (float64, int, bool) {
	cur := map[int]float64{}
	for _, b := range d.Boards {
		for _, t := range b.Temps {
			if t >= 5 && t < 150 {
				if v, ok := cur[b.ID]; !ok || t > v {
					cur[b.ID] = t
				}
			}
		}
	}
	if len(cur) == 0 { // no board reads at all: the no-temperature rule's case, nothing held over it
		return -1000, -1, false
	}
	best, board, have := -1000.0, -1, false
	for id, bs := range boardLast {
		if _, ok := cur[id]; ok {
			continue
		}
		if bs.miss++; bs.miss > heatHoldMiss {
			delete(boardLast, id)
		} else if bs.t > best || (bs.t == best && id < board) {
			best, board, have = bs.t, id, true
		}
	}
	for id, t := range cur {
		boardLast[id] = &boardSeen{t: t}
		if t > best || (t == best && id < board) {
			best, board, have = t, id, true
		}
	}
	return best, board, have
}

var (
	noTempSince time.Time // (fanLoop only)
	noTempTicks int       // readings in a row, while hashing, with no temperature
	heatEsc     heatEscState
	throttledTo int       // the clock the throttle tier last set (its restore only undoes its own change)
	throttleAt  time.Time // when it last stepped the clock (fanLoop only)
)

// throttleEvery: the throttle tier's shortest time between two clock steps (the boards take about that long to
// show one).
var throttleEvery = 60 * time.Second

// heatEscState: the abort tier's confirmation and escalation (fanLoop only).
type heatEscState struct {
	over    int       // readings in a row at or above the abort temperature
	far     int       // readings in a row 5 °C or more past it
	overAt  time.Time // the first of those readings
	farAt   time.Time // the first of the 5 °C-past readings
	fullAt  time.Time // when the fans were first forced to 100 % for this episode (zero = not yet)
	abortAt time.Time // when the abort tier engaged (a fan write that never lands still escalates)
}

// heatEscalateAfter: how long a board may stay at the abort temperature with the fans at 100 % before the miner
// goes to Idle.
var heatEscalateAfter = 90 * time.Second

// heatConfirmSpan: the confirming readings must span this long. The miner refreshes a board's temperatures every
// ~5–6 s, so two polls 5 s apart can read the same (glitched) sample twice: a time span, not a count, makes them two.
const heatConfirmSpan = 8 * time.Second

// heatEscHyst: once the abort tier is engaged, only a reading this far under the abort temperature ends the episode
// (a board sitting at it reads a tenth either side: one reading 87.9 °C at 100 % fan isn't cooling).
const heatEscHyst = 2.0

// fanNow: the fan loop's clock (tests replace it).
var fanNow = time.Now

// step: one temperature reading. abort: force the fans to 100 % (two readings in a row at or above abortC, so a
// single wild sensor reading doesn't). idle: go to Idle (still that hot 90 s after the fans went to 100 %, or two
// readings in a row 5 °C or more past abortC). fansFull says whether 100 % was actually written this episode.
func (h *heatEscState) step(maxT, abortC float64, now time.Time, fansFull bool) (abort, idle bool, why string) {
	switch {
	case maxT >= abortC:
		if h.over == 0 {
			h.overAt = now
		}
		h.over++
	case h.abortAt.IsZero() || maxT <= abortC-heatEscHyst || h.over == 0:
		// (over 0 with the timers set: back from its Idle, and the first reading is under the abort temperature:
		// the old episode is over, or an hour later two readings at it would go straight to Idle)
		*h = heatEscState{}
		return false, false, ""
	}
	// (else: engaged and only a hair under it, sensor noise at 100 % fan: the episode and its timers go on)
	if maxT >= abortC+5 {
		if h.far == 0 {
			h.farAt = now
		}
		h.far++
	} else {
		h.far = 0
	}
	if h.over < 2 || now.Sub(h.overAt) < heatConfirmSpan {
		return false, false, ""
	}
	if h.abortAt.IsZero() {
		h.abortAt = now
	}
	if fansFull && h.fullAt.IsZero() {
		h.fullAt = now
	}
	switch {
	case h.far >= 2 && now.Sub(h.farAt) >= heatConfirmSpan:
		return true, true, fmt.Sprintf("%.1f °C, 5 °C or more past the abort temperature (%.0f °C)", maxT, abortC)
	case !h.fullAt.IsZero() && now.Sub(h.fullAt) >= heatEscalateAfter:
		return true, true, fmt.Sprintf("still %.1f °C (abort %.0f °C) after %s with the fans at 100 %%", maxT, abortC, heatEscalateAfter.Round(time.Second))
	case now.Sub(h.abortAt) >= heatEscalateAfter+30*time.Second:
		return true, true, fmt.Sprintf("still %.1f °C (abort %.0f °C) %s after the fans were told to go to 100 %%", maxT, abortC, (heatEscalateAfter + 30*time.Second).Round(time.Second))
	}
	return true, false, ""
}

// idled: the miner was put on Idle for this episode. The readings must confirm again (two, spanning
// heatConfirmSpan) but the timers stay elapsed: woken while still that hot (the schedule, a run's end, the owner),
// it goes straight back to Idle. A cool reading ends the episode.
func (h *heatEscState) idled() { h.over, h.far = 0, 0 }

// heatIdleSince: when the thermal guard (or the no-temperature rule) put the miner on Idle (unix s; 0 = not).
// While the miner is still on that Idle, the schedule leaves it there.
var heatIdleSince atomic.Int64

func heatIdleHeld() bool { return heatIdleSince.Load() != 0 && getCharge() == "idle" }

// heatIdleWhy: why the thermal guard last put the miner on Idle (for the Dashboard's idle note).
var heatIdleWhy atomic.Value

func heatIdleWhyNow() string {
	if s, _ := heatIdleWhy.Load().(string); s != "" {
		return s
	}
	return "overheat"
}

// The overheat Idle and the throttle's saved clock outlive a panel restart (a crash, an update, a power cut):
// without them a restarted panel let the schedule wake a miner the thermal guard had just put on Idle, and
// left a throttled clock in place for good.
var heatHoldSaved atomic.Bool

func heatHoldPath() string { return filepath.Join(filepath.Dir(*authFile), "heat-idle.json") }

// throttleUnder: the clock from before the throttle when f is the clock the throttle set, else f (what the miner
// "ran before", for whatever remembers the running plan to put it back later).
func throttleUnder(f int) int {
	fanMu.RLock()
	defer fanMu.RUnlock()
	if fanSt.SavedFreq > 0 && fanSt.ThrottledTo == f {
		return fanSt.SavedFreq
	}
	return f
}

func throttlePath() string { return filepath.Join(filepath.Dir(*authFile), "throttle.json") }

type throttleRec struct {
	Saved int `json:"saved"` // the clock from before the throttle
	To    int `json:"to"`    // the clock it last set
}

var throttleOnDisk throttleRec // (fanLoop only)

// throttleKeep: the throttle's state on disk, written only when it changes.
func throttleKeep(saved, to int) {
	r := throttleRec{saved, to}
	if r == throttleOnDisk {
		return
	}
	if saved == 0 {
		if os.Remove(throttlePath()) == nil || !fileExists(throttlePath()) {
			throttleOnDisk = r
		}
		return
	}
	b, _ := json.Marshal(r)
	if writeAtomic(throttlePath(), b, 0o644) == nil {
		throttleOnDisk = r
	}
}

// dropHoldsFromBeforeOlder: an update from a version before v1.5.19 (which doesn't know these files) that is
// still waiting to be confirmed: the heat Idle's hold, the throttle's saved clock and the schedule's "before"
// written before that update are from an earlier run of a newer panel, before a roll back (the older version
// ran since, maybe for weeks, and the owner chose other settings there): dropped, not taken as current.
func dropHoldsFromBeforeOlder() {
	b, err := os.ReadFile(filepath.Join(updDir(), "pending"))
	if err != nil {
		return
	}
	var mk updMarker
	if json.Unmarshal(b, &mk) != nil || mk.From == "" || !versionLess(mk.From, "1.5.19") {
		return
	}
	for _, p := range []string{heatHoldPath(), throttlePath(), schedBeforePath()} {
		if fi, err := os.Stat(p); err == nil && (mk.At <= 0 || fi.ModTime().Unix() < mk.At) {
			os.Remove(p)
			log.Printf("update from %s: %s is from before a roll back to it: dropped", mk.From, filepath.Base(p))
		}
	}
}

// restartRestore (at start, before the loops): the overheat Idle's hold and the throttle's saved clock, as they
// were when the panel stopped. A file that can't be read is dropped (no hold, no saved clock: as before).
func restartRestore() {
	dropHoldsFromBeforeOlder()
	if b, err := os.ReadFile(heatHoldPath()); err == nil {
		var h struct {
			Since int64 `json:"since"`
		}
		if json.Unmarshal(b, &h) == nil && h.Since > 0 {
			heatIdleSince.Store(h.Since)
			heatEpMu.Lock()
			heatPinLocked(h.Since) // pinned now: a clock set later doesn't end the cool-down (heatcool.go)
			heatEpMu.Unlock()
			chargeMu.Lock()
			chargeBy = "idle"
			chargeMu.Unlock()
			heatHoldSaved.Store(true)
			activity.emit("🔥 thermal guard: the miner stays on the Idle it was put on for heat (" + time.Unix(h.Since, 0).Format("Jan 2 15:04") + ") until you apply " + stockWord() + " or a preset")
		} else {
			os.Remove(heatHoldPath())
		}
	}
	if b, err := os.ReadFile(throttlePath()); err == nil {
		var r throttleRec
		if json.Unmarshal(b, &r) == nil && r.Saved > 0 && r.To > 0 {
			throttledTo, throttleOnDisk = r.To, r
			fanMu.Lock()
			fanSt.SavedFreq, fanSt.ThrottledTo = r.Saved, r.To
			fanMu.Unlock()
		} else {
			os.Remove(throttlePath())
		}
	}
}

// minerOnIdlePlan: the stock setting runs the idle plan (the charge alone isn't proof: a write after ours can
// have woken it).
func minerOnIdlePlan() bool {
	if box2SoftIdled() { // Box II: the board held off by the soft idle (lane C1; false unless SoftIdle is on)
		return true
	}
	raw, err := stockGet("/mcb/setting")
	if err != nil {
		return false
	}
	var cur map[string]any
	return json.Unmarshal(raw, &cur) == nil && minerIdle(cur)
}

// thermalIdle: the miner goes to Idle for heat (or for no temperature). The charge is taken first, so a tuning run
// or chip test ended here sees it at its end and doesn't put its own setting back over it.
//
// The park is the model profile's own idle action (idleAction, model.go). On the SC Lite that is the vendor's
// idle powerplan (applyIdleThen, re-stopping under planMu the moment it lands — unchanged). On the SC Box II
// there is NO stock idle plan: idleAction is A1's stub (returns "no idle plan") until lane B3 wires the real
// miner-API board-off. While there is no working park, the guard must NOT pretend the boards are cooling on
// Idle (that would falsely block presets and tell the owner it is off) — instead thermalNoPark drives the fans
// to 100 % and says plainly that only Goldshell's own cut-off will actually stop the boards. It never silently
// does nothing.
func thermalIdle(why string) error {
	now := time.Now().Unix()
	heatIdleWhy.Store(why)

	// Models with no vendor idle powerplan (the SC Box II): park through idleAction(model). B3's board-off
	// returns nil on success and we fall through to the normal cool-down bookkeeping; the stub (and any API
	// failure) returns an error and we fall back to fans-at-100 % + cut-off.
	if model.Plan != planSCLitePV {
		// writes on their way are refused from before the park (safetyGen), and no wake path re-enables the board
		// until the hold below is in place (guardParkBegin): a request blocked behind the park would undo it
		end := guardParkBegin()
		defer end()
		if err := idleAction(model); err != nil {
			return thermalNoPark(why, err)
		}
	}

	heatIdleNoted(now)
	heatIdleSince.Store(now) // before the charge: heatIdleHeld from the moment the charge says idle
	safetyStopIdle()         // every write on its way (Controls, a preset, the schedule, a run) is refused
	stopRunsFor("the thermal guard", why)
	// For the vendor-idle-plan models (SC Lite), select the stock idle plan now. Box II has already parked
	// above (through idleAction) before heatIdleSince was set, so a no-park degrade there doesn't falsely
	// block presets. Once its Idle has landed, again: a request that came after the stop began (the owner's
	// preset, a run's start) and wrote before the Idle mustn't hand the charge back to itself afterwards.
	if model.Plan == planSCLitePV {
		if err := applyIdleThen(safetyStopIdle); err != nil {
			activity.emit("🔥 thermal guard: setting Idle failed (" + err.Error() + "): trying again at the next reading")
			return err // (the next hot reading tries again: that checks the miner's plan, not the charge)
		}
	}
	noParkWarned.Store(false) // a real park landed: a later no-park episode may warn again
	if b, _ := json.Marshal(map[string]int64{"since": now}); writeAtomic(heatHoldPath(), b, 0o644) == nil {
		heatHoldSaved.Store(true)
	}
	return nil
}

// thermalNoPark is the emergency when a model has no working park action yet (the SC Box II until B3 wires
// idleAction's real board-off). It drives the fans to 100 % for maximum cooling and warns — once per
// overheat episode — that the panel cannot stop the boards, so only Goldshell's own TempCutoffC cut-off
// will. It deliberately does NOT engage the cool-down hold (the boards are still hashing, not cooling on
// Idle) and returns a non-nil error so the fan loop keeps trying (and keeps the fans pinned) each tick.
func thermalNoPark(why string, parkErr error) error {
	_, ferr := writeFansUnlessIdle(100) // write the running stock plan with both fans at 100 %
	abortTierOn.Store(true)             // every other plan write keeps the fans where they are (heatGuardFan)
	if noParkWarned.CompareAndSwap(false, true) {
		msg := fmt.Sprintf("%s — this model has no panel stop yet (idle board-off not wired), so the miner CANNOT be parked. Fans forced to 100 %%; only %s own %.0f °C cut-off will stop the boards. Fix the cooling (fans, airflow, room) now.", why, oemPoss(), model.TempCutoffC)
		activity.emit("🔥 thermal guard: " + msg)
		notifyAlways(evThermal, "thermal guard: "+msg)
	}
	if ferr != nil {
		return fmt.Errorf("no idle plan on this model (%v); forcing the fans to 100 %% also failed: %v", parkErr, ferr)
	}
	return fmt.Errorf("no idle plan on this model (%v); fans forced to 100 %%, relying on the %.0f °C cut-off", parkErr, model.TempCutoffC)
}

func fanLoop() {
	for {
		fanMu.RLock()
		c := fanCfg
		fanMu.RUnlock()
		poll := c.PollS
		if poll < 5 {
			poll = 5
		}
		time.Sleep(time.Duration(poll) * time.Second)

		fanTick(c)
	}
}

// fanTick: one fan-loop reading (split out so tests can drive it).
// fanDash: the dashboard fanTick reads (tests replace it).
var fanDash = buildDashboard

func fanTick(c fanConfig) {
	curveOff := !c.Enabled // the curve is off, but the thermal guard (throttle, fans 100 % at abort) keeps working
	if curveOff {
		fanMu.Lock()
		fanHist = nil
		fanMu.Unlock()
	}
	heatPendingCheck() // the owner's "start when cool" after a heat Idle (on Idle too: no reading needed)
	d := fanDash()
	if !d.OK {
		fanMu.Lock()
		fanSt.Note, fanSt.Guard = "miner API unreachable", ""    // no reading: no guard to show
		fanSt.MaxT, fanSt.HotBoard, fanSt.Target = -1000, -1, -1 // nor a temperature or fan level from an old one
		fanMu.Unlock()
		noTempSince, noTempTicks = time.Time{}, 0
		clear(boardLast) // (after a gap, an old reading isn't held over the next ones)
		return
	}
	maxT, hot, ok := guardTemp(d)
	if !ok {
		clear(boardLast)
		fanMu.Lock()
		fanSt.Note, fanSt.Guard = "no temperature readings", ""
		fanSt.MaxT, fanSt.HotBoard, fanSt.Target = -1000, -1, -1 // the last reading isn't today's temperature
		fanMu.Unlock()
		// hashing with no temperature at all: Goldshell's fan controller then runs the fans full but never
		// shuts the boards down for heat (traced). After 3 minutes blind, the miner goes to Idle.
		hashing := false
		for _, b := range d.Boards {
			hashing = hashing || (b.Alive && b.GHS > 0)
		}
		if !hashing {
			noTempSince, noTempTicks = time.Time{}, 0
		} else {
			noTempTicks++
			if noTempSince.IsZero() {
				noTempSince = fanNow()
			} else if noTempTicks >= noTempIdleTicks() && fanNow().Sub(noTempSince) > noTempIdleWait() && !minerOnIdlePlan() { // (not the charge: something may have woken it)
				// model-driven window & stop: SC Lite parks after 3 min via the firmware idle plan; the SC
				// Box II parks after 30 s via idleAction (B3's board-off, A1 stub for now) — see noTempStop.
				if noTempStop("no board temperature while hashing") == nil {
					msg := noTempIdleMsg()
					activity.emit("🔥 " + msg)
					notifyAlways(evThermal, msg)
				}
				noTempSince, noTempTicks = time.Time{}, 0
			}
		}
		return
	}
	noTempSince, noTempTicks = time.Time{}, 0
	if maxT < c.AbortC {
		noParkWarned.Store(false) // cooled under the abort temperature: a later no-park episode may warn again
		if heatIdleSince.Load() != 0 && getCharge() != "idle" {
			heatIdleSince.Store(0) // running again and cool: that episode is over
		}
	}

	// snapshot the persistent state this tick needs (shared with the HTTP status reader)
	fanMu.Lock()
	savedFreq := fanSt.SavedFreq
	lastPct := fanSt.LastPct
	lastWrite := fanSt.LastWrite
	lastGuard := fanSt.Guard
	fanMu.Unlock()
	if lastWrite.IsZero() {
		lastPct = -1 // a config change (or nothing written yet): write on this reading
	}

	target := targetFan(c, maxT)
	guard, note := "", ""
	if curveOff {
		target, note = -1, "fan curve off: "+oemPoss()+" own fan control, with the thermal guard still on"
	}

	// throttle tier (opt-in): step the clock down while very hot, restore when cooled. Never on Idle (a plan
	// write would wake it), never while a tuning run, limit probe or strong-chip test owns the clock, one step a
	// minute at most; the saved clock comes back only if nothing else set one since.
	chg := getCharge()
	clockOwned := chg == "tuner" || chg == "ceiling" || sctRunningNow() // (not sct.mu: a tick holds it across miner reads)
	if c.ThrottleEnable && maxT >= c.ThrottleC && !clockOwned && chg != "idle" && !minerOnIdlePlan() {
		if time.Since(throttleAt) >= throttleEvery {
			step := c.ThrottleStep
			if step <= 0 {
				step = 25
			}
			from, to := 0, 0
			_, err := mutatePlanNoWake(func(p *powerplan) {
				if savedFreq == 0 || p.FreqMHz != throttledTo { // first step, or a clock set by someone else since
					savedFreq = p.FreqMHz
				}
				from = p.FreqMHz
				nf := p.FreqMHz - step
				if nf < freqMin {
					nf = min(freqMin, p.FreqMHz) // never a step "down" that raises the clock
				}
				p.FreqMHz, to = nf, nf
			})
			if err == nil {
				throttledTo, throttleAt = to, time.Now()
				guard = "throttle"
				note = fmt.Sprintf("board %d at %.1f°C — clock stepped down", hot, maxT)
				if throttledTo != from {
					note = fmt.Sprintf("board %d at %.1f°C — clock stepped down %d → %d MHz", hot, maxT, from, throttledTo)
					activity.emit("🔥 thermal: " + note)
					notify(evThermal, "thermal guard: "+note)
				}
			}
		} else if savedFreq != 0 {
			guard, note = "throttle", fmt.Sprintf("board %d at %.1f°C — clock stepped down (another step within a minute if it stays this hot)", hot, maxT)
		}
	} else if savedFreq != 0 && maxT <= c.ResumeC && clockOwned {
		// a run owns the clock: wait for its end. It puts back the plan it found (the throttle's step, restored
		// below then); a clock it leaves in place instead differs from throttledTo and stays.
	} else if savedFreq != 0 && maxT <= c.ResumeC && (chg == "idle" || minerIsIdleNow()) {
		savedFreq = 0 // put on Idle (restoring would wake it)
	} else if savedFreq != 0 && maxT <= c.ResumeC {
		// cooled down — restore the pre-throttle clock
		// (only if the clock is still the one the throttle set: a clock you chose meanwhile stays)
		sf, changed := savedFreq, false
		if _, err := mutatePlanNoWake(func(p *powerplan) {
			if p.FreqMHz == throttledTo {
				p.FreqMHz, changed = sf, true
			}
		}); err == nil || err == errMinerIdle {
			if changed && err == nil {
				activity.emit(fmt.Sprintf("✅ thermal: cooled to %.1f°C — clock restored to %d MHz", maxT, sf))
			} else if err == nil {
				activity.emit(fmt.Sprintf("✅ thermal: cooled to %.1f°C — the clock was changed meanwhile, so it stays as set", maxT))
			}
			savedFreq, throttledTo = 0, 0
		}
	}

	// abort tier: override the curve and force fans to 100% (two readings in a row: one wild sensor reading
	// doesn't). If that doesn't bring it down within 90 s, or a board is 5 °C past it, the miner goes to Idle.
	fanMu.RLock()
	full := fanSt.LastPct == 100 && !fanSt.LastWrite.IsZero() // a 100 % write that landed (Target is set even when it failed)
	fanMu.RUnlock()
	abortOn, idleNow, idleWhy := heatEsc.step(maxT, c.AbortC, fanNow(), full && (lastGuard == "abort" || lastGuard == "throttle"))
	if idleNow && !minerOnIdlePlan() { // (not the charge: something may have woken it since)
		if thermalIdle("overheat: "+idleWhy) == nil {
			msg := fmt.Sprintf("board %d %s: the miner is set to Idle so it can cool. Check the fans and the room, then apply "+stockWord()+" or a preset", hot, idleWhy)
			activity.emit("🔥 " + msg)
			notifyAlways(evThermal, "thermal guard: "+msg) // (the "fans forced 100 %" a minute ago mustn't hold this back)
			heatEsc.idled()
		}
	}
	abortSay := "" // the abort tier's "fans forced 100 %" line, written after the fan write (see below)
	wasAbort := abortTierOn.Load()
	abortTierOn.Store(abortOn) // every other plan write keeps the fans where they are meanwhile (heatGuardFan)
	if abortOn {
		target = 100
		if guard == "" {
			guard = "abort"
		}
		an := fmt.Sprintf("board %d at %.1f°C — fans forced 100%%", hot, maxT)
		if guard == "abort" { // (not the curve-off note: this is what the owner reads)
			note = an
		} else {
			note += "; fans forced 100 %" // (the throttle's note: the fans are at 100 % as well)
		}
		if !wasAbort && lastGuard != "abort" && getCharge() != "idle" { // (on Idle nothing is forced: said above)
			abortSay = an // said once the write is done: only a write that landed says "forced 100 %"
		}
	}

	// a tuning run that holds the fans owns them: two writers would fight (every write is a settings PUT that
	// Goldshell's backend now and then dies on). Only the abort tier's 100 % still goes through.
	if target >= 0 && target < 100 && tunerHoldsFans() {
		target, note = -1, "the tuner is holding the fans for its run (the thermal guard stays on)"
	}
	// Goldshell's start-up fan test records each fan's speed at each level (xminerd's first 90 s, or a re-run):
	// a curve write now would have it record the wrong speeds (as the fan pair offset, it waits). Only the
	// abort tier's 100 % still goes through.
	sweeping := target >= 0 && fanTestRunning()
	if sweeping && target < 100 {
		target, note = -1, OemOf()+" start-up fan test is sweeping the fans: the curve waits for it (the thermal guard stays on)"
	}
	// apply fans when the target changed or the cooldown elapsed (the miner walks it back).
	cool := c.CooldownS
	if cool < 15 {
		cool = 15
	}
	wrote := false
	defer func() {
		if abortSay == "" {
			return
		}
		if wrote {
			activity.emit("🔥 thermal: " + abortSay)
			notify(evThermal, "thermal guard: "+abortSay)
			return
		}
		msg := fmt.Sprintf("board %d at %.1f°C — forcing the fans to 100 %% FAILED: %s", hot, maxT, note)
		activity.emit("🔥 thermal: " + msg)
		notifyAlways(evThermal, "thermal guard: "+msg)
	}()
	if target < 0 {
		// a step profile below its first step: the miner's own fan control stays in charge
		if note == "" {
			note = "below the first step: the miner’s own fan control"
		}
	} else if target != lastPct || time.Since(lastWrite) >= time.Duration(cool)*time.Second {
		if idle, err := writeFansUnlessIdle(target); idle {
			target, note = -1, "the miner is idle: its own fan control is in charge (writing fans would wake it)"
		} else if err == nil {
			wrote = true
		} else {
			note = fmt.Sprintf("fan write failed (%v): the fans stay where they were, not at %d %%", err, target)
			target = -1 // "Now" shows what the fans run at, not what was asked (written again at the next reading)
		}
	}
	if target >= 0 && !sweeping { // (a flag in the sweep isn't this level's verdict)
		if f := fanGuardPair(target); f > 0 { // with a fan pair offset, pair B may run below: judge the lower pair
			note = fmt.Sprintf("%s watchdog flagged a fan at %d %%: the fans stay at %d %% or more", OemOf(), target, f)
		}
	}

	fanMu.Lock()
	fanSt.MaxT, fanSt.HotBoard, fanSt.Target, fanSt.Guard, fanSt.Note = maxT, hot, target, guard, note
	fanSt.SavedFreq, fanSt.ThrottledTo = savedFreq, throttledTo
	throttleKeep(savedFreq, throttledTo)
	if wrote {
		fanSt.LastWrite, fanSt.LastPct = time.Now(), target
	}
	if target < 0 {
		fanSt.LastPct = -1 // someone else drives the fans meanwhile: write again when the curve takes over
	}
	fanMu.Unlock()
}

func validateFanConfig(c fanConfig) error {
	if c.Profile == "custom" {
		if len(c.Points) < 2 || len(c.Points) > 8 {
			return fmt.Errorf("a custom curve needs 2–8 points")
		}
		pts := append([]curvePoint(nil), c.Points...)
		sort.Slice(pts, func(i, j int) bool { return pts[i].Temp < pts[j].Temp })
		for i, p := range pts {
			if p.Temp < 20 || p.Temp > 100 {
				return fmt.Errorf("point temperature %.0f°C is outside 20–100", p.Temp)
			}
			if p.Fan < float64(fanMin) || p.Fan > 100 {
				return fmt.Errorf("point fan %.0f%% is outside %d–100", p.Fan, fanMin)
			}
			if i > 0 {
				if pts[i].Temp == pts[i-1].Temp {
					return fmt.Errorf("two points at %.0f°C", p.Temp)
				}
				if pts[i].Fan < pts[i-1].Fan {
					return fmt.Errorf("fan must not drop as temperature rises")
				}
			}
		}
	} else if fc, ok := lookupCurve(c, c.Profile); !ok {
		return fmt.Errorf("unknown fan curve %q", c.Profile)
	} else if err := validatePoints(fc.Points); err != nil { // a library or tuned curve from a file: checked as the page checks it
		return fmt.Errorf("fan curve %q: %v", c.Profile, err)
	}
	if c.Smoothing < 0 || c.Smoothing > 2 {
		return fmt.Errorf("smoothing must be 0, 1 or 2")
	}
	if c.PollS < 5 || c.PollS > 60 {
		return fmt.Errorf("poll must be 5–60 s")
	}
	if c.CooldownS < 15 || c.CooldownS > 600 {
		return fmt.Errorf("cooldown must be 15–600 s")
	}
	if c.AbortC < 70 || c.AbortC > abortMaxC {
		return fmt.Errorf("abort temperature must be 70–%.0f °C (%s own over-temperature control takes over at %.0f °C)", abortMaxC, oemPoss(), model.TempCutoffC)
	}
	if c.ThrottleEnable {
		if c.ThrottleC < 70 || c.ThrottleC > abortMaxC {
			return fmt.Errorf("throttle temperature must be 70–%.0f °C", abortMaxC)
		}
		if c.ThrottleStep < 5 || c.ThrottleStep > 100 {
			return fmt.Errorf("throttle step must be 5–100 MHz")
		}
	}
	if c.ResumeC < 40 || c.ResumeC >= c.AbortC {
		return fmt.Errorf("resume temperature must be 40 °C or more and below the abort temperature")
	}
	if c.ThrottleEnable && c.ResumeC > c.ThrottleC-3 {
		return fmt.Errorf("resume temperature must be at least 3 °C below the throttle temperature (or the clock goes down and up every few seconds)")
	}
	return nil
}

// fanProfilesView lists every curve the page can show: yours, the tuner's, the built-ins.
func fanProfilesView(c fanConfig) map[string]any {
	out := map[string]any{}
	view := func(fc fanCurve, builtin, tuned bool) map[string]any {
		mode := fc.Mode
		if mode == "" {
			mode = "curve"
		}
		return map[string]any{"label": fc.Label, "mode": mode, "points": fc.Points, "abort_c": fc.AbortC,
			"cooldown_s": fc.CooldownS, "poll_s": fc.PollS, "smoothing": fc.Smoothing,
			"force_tempcontrol_on": fc.ForceTC, "builtin": builtin, "tuned": tuned}
	}
	for k, fc := range c.Curves {
		out[k] = view(fc, false, false)
	}
	for _, p := range loadPresets() {
		if len(p.FanPoints) >= 2 {
			if fc, ok := lookupCurve(c, "tuned:"+p.Name); ok {
				out["tuned:"+p.Name] = view(fc, false, true)
			}
		}
	}
	for _, k := range builtinOrder {
		fc, _ := builtinCurve(k)
		out[k] = view(fc, true, false)
	}
	return out
}

var curveKeyRe = regexp.MustCompile(`[^a-z0-9]+`)

func validateCurve(fc fanCurve) error {
	if strings.TrimSpace(fc.Label) == "" {
		return fmt.Errorf("give the curve a name")
	}
	if err := validatePoints(fc.Points); err != nil {
		return err
	}
	pts := append([]curvePoint(nil), fc.Points...)
	sort.Slice(pts, func(i, j int) bool { return pts[i].Temp < pts[j].Temp })
	if fc.AbortC < 70 || fc.AbortC > abortMaxC {
		return fmt.Errorf("safety temperature must be 70–%.0f °C (%s own over-temperature control takes over at %.0f °C)", abortMaxC, oemPoss(), model.TempCutoffC)
	}
	if fc.AbortC <= pts[0].Temp {
		return fmt.Errorf("the safety temperature must be above the first point")
	}
	if fc.CooldownS < 15 || fc.CooldownS > 600 {
		return fmt.Errorf("re-send must be 15-600 s")
	}
	if fc.PollS < 5 || fc.PollS > 60 {
		return fmt.Errorf("check every must be 5-60 s")
	}
	if fc.Smoothing < 0 || fc.Smoothing > 2 {
		return fmt.Errorf("smoothing must be off, light or strong")
	}
	return nil
}

// validatePoints: 2–8 points, 20–100 °C, fan 10–100 %, no two at one temperature, the fan never dropping.
func validatePoints(points []curvePoint) error {
	if len(points) < 2 || len(points) > 8 {
		return fmt.Errorf("a curve needs 2 to 8 points")
	}
	pts := append([]curvePoint(nil), points...)
	sort.Slice(pts, func(i, j int) bool { return pts[i].Temp < pts[j].Temp })
	for i, p := range pts {
		if p.Temp < 20 || p.Temp > 100 {
			return fmt.Errorf("%.0f °C is outside 20-100 °C", p.Temp)
		}
		if p.Fan < float64(fanMin) || p.Fan > 100 {
			return fmt.Errorf("fan %.0f %% is outside %d-100 %%", p.Fan, fanMin)
		}
		if i > 0 && pts[i].Temp == pts[i-1].Temp {
			return fmt.Errorf("two points at %.0f °C", p.Temp)
		}
		if i > 0 && pts[i].Fan < pts[i-1].Fan {
			return fmt.Errorf("the fan drops from %.0f %% to %.0f %% as it gets hotter", pts[i-1].Fan, pts[i].Fan)
		}
	}
	return nil
}

// sanitizeFanConfig: fan.json from an old version or edited by hand. Whatever the page would refuse goes back to
// its default (a library curve it would refuse is dropped), so the fan loop never runs it. Returns what changed.
func sanitizeFanConfig(c fanConfig) (fanConfig, []string) {
	d := defaultFanConfig()
	var fixed []string
	fix := func(what string) { fixed = append(fixed, what) }
	if len(c.Curves) > 0 {
		lib := map[string]fanCurve{}
		for k, cv := range c.Curves {
			if validateCurve(cv) == nil {
				lib[k] = cv
			} else {
				fix("curve " + k)
			}
		}
		c.Curves = lib
	}
	if c.Smoothing < 0 || c.Smoothing > 2 {
		c.Smoothing = d.Smoothing
		fix("smoothing")
	}
	if c.PollS < 5 || c.PollS > 60 {
		c.PollS = d.PollS
		fix("poll")
	}
	if c.CooldownS < 15 || c.CooldownS > 600 {
		c.CooldownS = d.CooldownS
		fix("re-send")
	}
	if !(c.AbortC >= 70 && c.AbortC <= 90) {
		c.AbortC = d.AbortC
		fix("safety temperature")
	}
	if !(c.ThrottleC >= 70 && c.ThrottleC <= 90) {
		c.ThrottleC = d.ThrottleC
		fix("throttle temperature")
	}
	if c.ThrottleStep < 5 || c.ThrottleStep > 100 {
		c.ThrottleStep = d.ThrottleStep
		fix("throttle step")
	}
	if !(c.ResumeC >= 40 && c.ResumeC < c.AbortC) || (c.ThrottleEnable && c.ResumeC > c.ThrottleC-3) {
		c.ResumeC = math.Max(40, math.Min(c.AbortC-5, c.ThrottleC-3))
		fix("resume temperature")
	}
	if validateFanConfig(c) != nil { // the curve itself (unknown, or points the page refuses): the default curve
		c.Profile, c.Points = d.Profile, nil
		if bc, ok := builtinCurve(d.Profile); ok {
			c.AbortC, c.CooldownS, c.PollS, c.Smoothing = bc.AbortC, bc.CooldownS, bc.PollS, bc.Smoothing
		}
		fitResume(&c)
		fix("curve")
	}
	return c, fixed
}

func setFanConfig(c fanConfig) error {
	if err := saveFanConfig(c); err != nil {
		return err
	}
	fanGuardClear()
	return useFanConfig(c)
}

// storeFanConfig: save and use the settings, keeping the watchdog's fan floor (a library change only).
func storeFanConfig(c fanConfig) error {
	if err := saveFanConfig(c); err != nil {
		return err
	}
	return useFanConfig(c)
}

func useFanConfig(c fanConfig) error {
	fanMu.Lock()
	fanCfg = c
	fanHist = nil
	fanSt.Guard, fanSt.Note = "", ""
	fanSt.LastWrite = time.Time{} // re-assert on next tick
	fanMu.Unlock()
	return nil
}

func handleFan(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			writeJSON(w, map[string]any{"ok": false, "err": "bad request"})
			return
		}
		var act struct {
			Action string `json:"action"`
			Key    string `json:"key"`
			fanCurve
		}
		if err := json.Unmarshal(raw, &act); err != nil {
			writeJSON(w, map[string]any{"ok": false, "err": badJSON(err)})
			return
		}
		fanMu.RLock()
		cur := fanCfg
		fanMu.RUnlock()
		switch act.Action {
		case "profile": // save one of your curves (new when key is empty)
			fc := act.fanCurve
			fc.Label = strings.TrimSpace(strings.Map(func(r rune) rune {
				if strings.ContainsRune("<>\"'`&", r) {
					return -1
				}
				return r
			}, fc.Label))
			if len([]rune(fc.Label)) > 48 {
				fc.Label = string([]rune(fc.Label)[:48])
			}
			sort.Slice(fc.Points, func(i, j int) bool { return fc.Points[i].Temp < fc.Points[j].Temp })
			if err := validateCurve(fc); err != nil {
				writeJSON(w, map[string]any{"ok": false, "err": err.Error()})
				return
			}
			key := act.Key
			if strings.HasPrefix(key, "tuned:") {
				// a curve the tuner made belongs to its preset: the edit is saved there
				name := strings.TrimPrefix(key, "tuned:")
				list := loadPresets()
				found := -1
				for i := range list {
					if list[i].Name == name && len(list[i].FanPoints) >= 2 {
						found = i
					}
				}
				if found < 0 {
					writeJSON(w, map[string]any{"ok": false, "err": "no preset called " + name + " with its own curve"})
					return
				}
				sm := fc.Smoothing
				p := &list[found]
				p.FanPoints = append([]curvePoint(nil), fc.Points...)
				p.FanAbortC, p.FanCooldown, p.FanPoll, p.FanSmooth, p.FanEdited = fc.AbortC, fc.CooldownS, fc.PollS, &sm, true
				if err := savePresets(list); err != nil {
					writeJSON(w, map[string]any{"ok": false, "err": "save failed: " + err.Error()})
					return
				}
				// the preset is on the miner right now: its fans follow the edit straight away
				if getCharge() == "preset:"+name && (cur.Profile == "custom" || cur.Profile == key) {
					nc := cur
					nc.Points = append([]curvePoint(nil), fc.Points...)
					nc.AbortC, nc.CooldownS, nc.PollS, nc.Smoothing = fc.AbortC, fc.CooldownS, fc.PollS, fc.Smoothing
					fitResume(&nc)
					if err := setFanConfig(nc); err != nil {
						writeJSON(w, map[string]any{"ok": false, "err": "saved to the preset, but the running fan settings failed: " + err.Error()})
						return
					}
				}
				fanMu.RLock()
				nv := fanProfilesView(fanCfg)
				fanMu.RUnlock()
				writeJSON(w, map[string]any{"ok": true, "key": key, "profile": nv[key]})
				return
			}
			if key != "" {
				if _, ok := cur.Curves[key]; !ok {
					writeJSON(w, map[string]any{"ok": false, "err": "built-in and tuner curves can't be changed: copy one to edit it"})
					return
				}
			} else {
				base := strings.Trim(curveKeyRe.ReplaceAllString(strings.ToLower(fc.Label), "-"), "-")
				if base == "" {
					base = "curve"
				}
				key = base
				for i := 2; ; i++ {
					_, mine := cur.Curves[key]
					_, bi := builtinCurves[key]
					if !mine && !bi {
						break
					}
					key = fmt.Sprintf("%s-%d", base, i)
				}
			}
			nc := cur
			nc.Curves = map[string]fanCurve{}
			for k, v := range cur.Curves {
				nc.Curves[k] = v
			}
			nc.Curves[key] = fc
			store := storeFanConfig // a curve that isn't running: the watchdog's fan floor stays
			if nc.Profile == key {  // the running curve changed: its settings follow
				nc.AbortC, nc.CooldownS, nc.PollS, nc.Smoothing = fc.AbortC, fc.CooldownS, fc.PollS, fc.Smoothing
				fitResume(&nc)
				store = setFanConfig
			}
			if err := store(nc); err != nil {
				writeJSON(w, map[string]any{"ok": false, "err": "save failed: " + err.Error()})
				return
			}
			writeJSON(w, map[string]any{"ok": true, "key": key, "profile": fanProfilesView(nc)[key]})
			return
		case "profile_delete":
			if _, ok := cur.Curves[act.Key]; !ok {
				writeJSON(w, map[string]any{"ok": false, "err": "only your own curves can be deleted"})
				return
			}
			if cur.Profile == act.Key && cur.Enabled {
				writeJSON(w, map[string]any{"ok": false, "err": "the auto fan is running this curve: pick another one first"})
				return
			}
			for _, p := range loadPresets() {
				if p.FanProfile == act.Key {
					writeJSON(w, map[string]any{"ok": false, "err": "the " + p.Name + " preset uses this curve: change its fan curve first"})
					return
				}
			}
			nc := cur
			nc.Curves = map[string]fanCurve{}
			for k, v := range cur.Curves {
				if k != act.Key {
					nc.Curves[k] = v
				}
			}
			store := storeFanConfig
			if nc.Profile == act.Key {
				nc.Profile = "curve-balanced"
				store = setFanConfig
			}
			if err := store(nc); err != nil {
				writeJSON(w, map[string]any{"ok": false, "err": "save failed: " + err.Error()})
				return
			}
			writeJSON(w, map[string]any{"ok": true})
			return
		}
		// the auto fan itself (on/off, which curve, the thermal guard): locked while something is in charge
		if lbl := chargeText(); lbl != "" {
			writeJSON(w, map[string]any{"ok": false, "locked": true, "err": "locked — " + lbl + " is in charge. Take manual control first."})
			return
		}
		// decode onto a copy that shares nothing with the running config: json.Unmarshal writes a slice into its
		// backing array and a map in place, so a refused request would otherwise change the curve that runs
		in := cur
		in.Points = slices.Clone(cur.Points)
		in.Curves = nil
		if err := json.Unmarshal(raw, &in); err != nil {
			writeJSON(w, map[string]any{"ok": false, "err": badJSON(err)})
			return
		}
		in.Curves = cur.Curves // the library is only changed by the profile actions
		if strings.HasPrefix(in.Profile, "tuned:") {
			if fc, ok := lookupCurve(in, in.Profile); ok {
				in.Points = append([]curvePoint(nil), fc.Points...)
			}
		}
		if fc, ok := lookupCurve(in, in.Profile); ok && in.Profile != "custom" { // the curve carries its own settings
			in.AbortC, in.CooldownS, in.PollS, in.Smoothing = fc.AbortC, fc.CooldownS, fc.PollS, fc.Smoothing
			fitResume(&in)
		}
		if err := validateFanConfig(in); err != nil {
			writeJSON(w, map[string]any{"ok": false, "err": err.Error()})
			return
		}
		if err := setFanConfig(in); err != nil {
			writeJSON(w, map[string]any{"ok": false, "err": "save failed: " + err.Error()})
			return
		}
		activity.emit("⚙ auto fan " + map[bool]string{true: "on", false: "off"}[in.Enabled])
		writeJSON(w, map[string]any{"ok": true})
		return
	}
	fanMu.RLock()
	c := fanCfg
	st := fanSt
	fanMu.RUnlock()
	// Always report the live hottest-board temperature, even when auto-fan is off, so the UI can
	// draw the current-temperature marker on the curve at all times.
	liveTemp, liveBoard := st.MaxT, st.HotBoard
	var fans []int
	if d := buildDashboard(); d.OK {
		if mt, hb, ok := maxBoardTemp(d); ok && st.Guard == "" { // the live reading (the loop's may be a poll old); with a guard on, the reading it acted on
			liveTemp, liveBoard = mt, hb
		}
		for _, b := range d.Boards {
			if b.Alive && len(b.Fans) > 0 {
				fans = b.Fans
				break
			}
		}
	}
	var builtins []map[string]any
	for _, k := range builtinOrder {
		builtins = append(builtins, map[string]any{"key": k, "label": builtinLabels[k], "points": builtinCurves[k]})
	}
	writeJSON(w, map[string]any{
		"ok": true, "config": c, "builtins": builtins, "profiles": fanProfilesView(c), "in_charge": chargeLabel(),
		"status": map[string]any{
			"max_temp": liveTemp, "hot_board": liveBoard, "target": st.Target,
			"guard": st.Guard, "note": st.Note, "throttled": st.SavedFreq != 0,
			"label": minerDisplayName(), "fans_rpm": fans,
		},
		"fan_min": fanMin, "fan_floor": fanFloor(), "fan_guard": fanGuardLoad(),
	})
}

// ===================== auto-tuner wiring =====================

// readChipErrors parses /dbg/icinfo into per-chip hardware-error counts (active chips only).
// icBoardStatus: a board slot's row in the miner's icinfo table (status 0 = running, reason "Alive").
type icBoardStatus struct {
	Status int    `json:"status"`
	Reason string `json:"reason"`
}

// aliveBoards: the board slots the miner reports as running, or nil when it reports none (then every slot
// with chip data counts, as before).
func aliveBoards(t []icBoardStatus) map[int]bool {
	var m map[int]bool
	for i, b := range t {
		if b.Status == 0 || strings.EqualFold(strings.TrimSpace(b.Reason), "alive") {
			if m == nil {
				m = map[int]bool{}
			}
			m[i] = true
		}
	}
	return m
}

func readChipErrors() (map[ckey]int, error) {
	raw, err := stockGet("/dbg/icinfo")
	if err != nil {
		return nil, err
	}
	var outer struct {
		Body string `json:"body"`
	}
	if json.Unmarshal(raw, &outer) != nil || outer.Body == "" {
		return nil, fmt.Errorf("unexpected icinfo shape")
	}
	var inner struct {
		Tabledata []icBoardStatus `json:"tabledata"`
		Drawdata  [][]struct {
			HWErr     int `json:"hwerr"`
			Chipindex int `json:"chipindex"`
			Perf      int `json:"perf"`
			Bist      int `json:"bist"`
		} `json:"drawdata"`
	}
	if err := json.Unmarshal([]byte(outer.Body), &inner); err != nil {
		return nil, err
	}
	out := map[ckey]int{}
	alive := aliveBoards(inner.Tabledata)
	for b, chips := range inner.Drawdata {
		if alive != nil && !alive[b] {
			continue // a board slot the miner reports as not running
		}
		for _, c := range chips {
			if c.Perf > 0 || c.Bist > 0 || c.HWErr > 0 {
				out[ckey{B: b, I: c.Chipindex}] = c.HWErr
			}
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no active chips in icinfo")
	}
	return out, nil
}

// applyTunePlan applies a clock/voltage/PV plan, keeping the current fan values.
// snapshotMinerSetting: what the miner runs before a tuning run, and a way to put exactly that back at the
// end (its manual plan, or its firmware level: that level's plan is written first, since turning manual
// off alone may leave the last clock written, then the setting goes back as it was).
func snapshotMinerSetting() (func() error, string) {
	raw, err := stockGet("/mcb/setting")
	if err != nil {
		return nil, ""
	}
	return settingRestorer(raw)
}

// settingRestorer: snapshotMinerSetting's restore, for a setting read earlier (raw /mcb/setting).
func settingRestorer(raw []byte) (func() error, string) {
	var cur map[string]any
	if json.Unmarshal(raw, &cur) != nil {
		return nil, ""
	}
	pp, ok := resolveRunningPlan(cur)
	what := "its own setting"
	if ok && pp.FreqMHz > 0 {
		what = fmt.Sprintf("%d MHz · %d mV", pp.FreqMHz, pp.VrunMv)
		if model.Plan == planBox2V { // its "voltage" is Goldshell's level, not millivolts
			what = planShow(pp)
		}
	}
	manual, _ := cur["manual"].(bool)
	if !manual {
		if model.Plan == planBox2V {
			what += " (its firmware mode)"
		} else {
			what += " (its firmware level)"
		}
	}
	gen := safetyGen.Load() // an Idle a safety stop (or the owner) takes after this stays: see safetyGen
	return func() error {
		if miningHoldErr() != nil || genMoved(gen) {
			return nil // the fan lock, the pool guard, the thermal guard or the owner put it on Idle meanwhile: it stays there
		}
		if !manual && ok && pp.FreqMHz > 0 {
			_ = applyTunePlanGen(gen, planMV{MHz: pp.FreqMHz, MV: pp.VrunMv, PV: pp.PVMv})
		}
		hold := -1
		if heatGuardActive() {
			hold = liveFanMax() // before the write below: it sets the fans to the old plan's level at once
		}
		if manual && ok && pp.FreqMHz > 0 && (pp.PVMv < pvHardMin || pp.PVMv > pvHardMax || pp.VrunMv < vrunHardMin ||
			pp.VrunMv > min(vrunHardMax, pp.PVMv) || pp.FreqMHz < freqHardMin || pp.FreqMHz > clockWindowHi()) {
			// a manual plan outside today's limits (a record from disk, an older firmware, the clock window re-locked
			// since): never written verbatim, only through the checks (PV clamped, a clock past the window refused)
			_, err := mutatePlanGen(gen, func(x *powerplan) {
				x.FreqMHz, x.VrunMv, x.PVMv = pp.FreqMHz, pp.VrunMv, pp.PVMv
				x.Fan1, x.Fan2 = max(pp.Fan1, hold), max(pp.Fan2, hold)
			})
			if err == errSafetyStop {
				err = nil
			}
			return err
		}
		err := putSettingGen(gen, raw) // (the holds again under planMu: one may have come since the check above)
		if miningHoldErr() != nil || genMoved(gen) {
			return nil
		}
		if err == nil && ok && pp.FreqMHz > 0 && (pp.PVMv < pvHardMin || pp.PVMv > pvHardMax || min(pp.Fan1, pp.Fan2) < fanFloor() || heatGuardActive() || hold > 0) {
			// what it ran before is outside today's limits (an old PV, fans under the floor) or it's hot now:
			// the same setting again through the checks, fans not under what they ran at before it
			if _, err = mutatePlanGen(gen, func(x *powerplan) { x.Fan1, x.Fan2 = max(x.Fan1, hold), max(x.Fan2, hold) }); err == errSafetyStop {
				err = nil
			}
		}
		return err
	}, what
}

// verifyTunePlan: "" when the miner's setting (read back) is p, else what it runs instead. A backend that
// doesn't answer (it can't for a minute while the boards restart) counts as "" : nothing to go on.
func verifyTunePlan(p planMV) string {
	pp, ok := readStockBaselineNow("current")
	if !ok || pp.FreqMHz <= 0 {
		return ""
	}
	var d []string
	if pp.FreqMHz != p.MHz {
		d = append(d, fmt.Sprintf("%d MHz", pp.FreqMHz))
	}
	if pp.VrunMv != p.MV {
		if model.Plan == planBox2V {
			d = append(d, box2LevelText(pp.VrunMv))
		} else {
			d = append(d, fmt.Sprintf("%d mV", pp.VrunMv))
		}
	}
	if p.PV > 0 && pp.PVMv != p.PV {
		d = append(d, fmt.Sprintf("PV %d", pp.PVMv))
	}
	if len(d) == 0 {
		return ""
	}
	return "it runs " + strings.Join(d, " · ")
}

func applyTunePlan(p planMV) error { return applyTunePlanGen(noGen, p) }

// applyTunePlanGen: applyTunePlan for a run that noted safetyGen as it started (refused after a safety stop).
func applyTunePlanGen(gen int64, p planMV) error {
	_, err := mutatePlanGen(gen, func(pp *powerplan) { pp.FreqMHz = p.MHz; pp.VrunMv = p.MV; pp.PVMv = p.PV })
	return err
}

// readStockBaseline resolves the tuner baseline from the miner: "current" = the running plan;
// "stock" = the selected firmware level's plan (ignoring any manual override).
var (
	stockCacheMu sync.Mutex
	stockCache   powerplan // the last stock plan read: the backend doesn't answer for a while after a change
)

func readStockBaseline(src string) (powerplan, bool) {
	pp, ok := readStockBaselineNow(src)
	if src == "stock" {
		stockCacheMu.Lock()
		defer stockCacheMu.Unlock()
		if ok && pp.FreqMHz > 0 {
			stockCache = pp
		} else if stockCache.FreqMHz > 0 {
			return stockCache, true
		}
	}
	return pp, ok
}

func readStockBaselineNow(src string) (powerplan, bool) {
	raw, err := stockGet("/mcb/setting")
	if err != nil {
		return powerplan{}, false
	}
	var cur map[string]any
	if json.Unmarshal(raw, &cur) != nil {
		return powerplan{}, false
	}
	if src == "current" {
		return resolveRunningPlan(cur)
	}
	// stock: the plan at the selected level (not the manual override). On the SC Box II "Stock" is Goldshell's
	// factory default (the 690 MHz / level 42 plan), whichever of its two stock modes is selected now
	if i := box2DefaultIndex(cur); i >= 0 {
		return stockPlanAt(cur, i)
	}
	if pp, ok := stockPlanAt(cur, stockSelect(cur)); ok && pp.FreqMHz > 0 {
		return pp, true
	}
	if i := firstHashingPlan(cur); i >= 0 { // the miner is idle: stock is the firmware's own hashing plan
		return stockPlanAt(cur, i)
	}
	if s, _ := cur["manualPowerplan"].(string); s != "" {
		return parsePowerplan(s)
	}
	return powerplan{}, false
}

// applyTuneFan sets both board fans to pct% (hold-steady mode). Clamped to the safe range.
func applyTuneFan(pct int) error {
	if f := fanFloor(); pct < f {
		pct = f
	}
	if pct > fanMax {
		pct = fanMax
	}
	// never from Idle: a fan write that woke the miner would undo an Idle the thermal guard or the fan lock just set
	_, err := mutatePlanNoWake(func(pp *powerplan) { pp.Fan1 = pct; pp.Fan2 = pct })
	return err
}

var (
	tunerMu  sync.Mutex
	tunerCur *tunerEngine
)

func handleTuner(w http.ResponseWriter, r *http.Request) {
	if !featGate(w, model.Feat.Tuner, "the auto-tuner") {
		return
	}
	if r.Method == http.MethodPost {
		var in struct {
			Action string       `json:"action"`
			Config *tunerConfig `json:"config"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			msg := "couldn't read the settings: " + err.Error()
			if strings.Contains(err.Error(), "chip_trim_max") {
				msg = "chips the tuner may trim per board must be a whole number, 1–10"
			} else if strings.Contains(err.Error(), "into Go struct field") {
				msg = "the clocks and voltages here are whole MHz and mV (587.5 MHz: enter 588, the chips run 587.5)"
				if model.Plan == planBox2V {
					msg = "the clocks here are whole MHz and the voltages one of the listed steps (587.5 MHz: enter 588, the chips run 587.5)"
				}
			}
			writeJSON(w, map[string]any{"ok": false, "err": msg})
			return
		}
		switch in.Action {
		case "start":
			controlMu.Lock() // the charge check and taking charge, in one go (see controlMu)
			defer controlMu.Unlock()
			gen := safetyGen.Load() // before the checks: a safety stop from here on wins over the run (see safetyGen)
			switch getCharge() {
			case "ceiling":
				writeJSON(w, map[string]any{"ok": false, "err": "the limit probe is running: stop it first"})
				return
			case "tuner": // the last run is still putting the miner back
				writeJSON(w, map[string]any{"ok": false, "err": "a tuning run is going (or still putting the miner back): stop it, or wait for it to end"})
				return
			}
			if err := miningHoldErr(); err != nil {
				writeJSON(w, map[string]any{"ok": false, "err": err.Error()})
				return
			}
			if sctRunning() { // it raises chips and watches their errors: a run would change the clock under it
				writeJSON(w, map[string]any{"ok": false, "err": "the strong-chip test is running (Controls → Chip trims): stop it first"})
				return
			}
			if fanTestRunning() { // the re-run puts its own setting back at its end; the fans are being swept
				writeJSON(w, map[string]any{"ok": false, "err": OemOf() + " fan test is running (about 2 minutes): try again when it's done"})
				return
			}
			cfg := defaultTunerConfig()
			if in.Config != nil {
				cfg = *in.Config
			}
			box2Note := ""
			if tunerOnBox2() { // trim codes, the stock baseline; clock only while the code's direction is unknown
				cfg, box2Note = box2TunerNormalize(cfg)
			}
			if err := validateTunerConfig(cfg); err != nil {
				writeJSON(w, map[string]any{"ok": false, "err": err.Error()})
				return
			}
			if cfg.ChipTrim && cfg.Mode == "map" && !cfg.DryRun {
				if _, ok := cwReadStats(); !ok {
					writeJSON(w, map[string]any{"ok": false, "err": "“Trim weak chips” needs chip trims switched on and reporting (Controls → Chip trims), or untick it"})
					return
				}
			}
			// the last run's check outside tunerMu: isRunning waits out a run frozen holding its lock (until the
			// watchdog ends it), and the guards' stops (stopRunsFor, the fan lock) need tunerMu meanwhile. Only
			// this start (under controlMu) sets tunerCur, so nothing can come in between.
			tunerMu.Lock()
			prev := tunerCur
			tunerMu.Unlock()
			if prev != nil && prev.isRunning() {
				writeJSON(w, map[string]any{"ok": false, "err": "a tuning run is already going"})
				return
			}
			tunerMu.Lock()
			tunerCur = newTunerEngine(cfg)
			tunerCur.persist = true
			tunerCur.planGen = gen // its writes are refused after a safety stop
			t := tunerCur
			t.begin() // running from now: a second start is refused, a Stop is kept
			tunerMu.Unlock()
			cwMu.Lock()   // an auto-trim tick past its "a run is going?" check ends before the run takes charge (its
			cwMu.Unlock() // trim isn't written inside the run); the next tick sees the run and pauses (controlMu → cwMu)
			// Lock manual controls/fans for the whole run — dry runs read the real chips too, so a
			// manual clock/voltage change mid-run would skew the readings either way.
			t.prevCharge = getCharge()       // a preset or Stock in charge before: back in charge after, unless a setting is confirmed
			if !setChargeGen(gen, "tuner") { // a safety stop since the checks: its Idle stays; the run ends at once
				t.StopFor("a safety stop put the miner on Idle as the run started")
			}
			t.onStart = func() {
				// without its restart record a power cut or a crash mid-run would leave the miner on the run's test
				// setting with its trims on (nothing to put back at the next start): as the limit probe, no run then
				if err := writeTunerMarker(t); err != nil && !cfg.DryRun {
					t.StopFor("the panel: the run's restart record couldn't be saved (" + fsErr(err) + "), so it can't go on")
					activity.emit("🔧 tuning run not started: its restart record couldn't be saved (" + fsErr(err) + ")")
					return
				}
				notify(evTuning, t.startText())
			}
			t.onApply = func(p planMV) { markTunerTesting(p) }
			t.onTrims = markTunerTrims
			go superviseTuner(t) // runs it; ends it from outside if it gets stuck (tunerwatch.go)
			note := "started"
			if cfg.DryRun {
				note = "started in DRY-RUN (reads chips, writes nothing)"
			} else {
				recordMark("T", "tuning started ("+cfg.Mode+")")
			}
			if box2Note != "" {
				note += " · " + box2Note
			}
			writeJSON(w, map[string]any{"ok": true, "note": note})
		case "stop":
			tunerMu.Lock()
			t := tunerCur
			tunerMu.Unlock()
			if t != nil {
				t.Stop()
			}
			writeJSON(w, map[string]any{"ok": true, "note": "stopping: the miner goes back to what it ran before the run, chip trims included"})
		default:
			writeJSON(w, map[string]any{"ok": false, "err": "unknown action"})
		}
		return
	}
	tunerMu.Lock()
	t := tunerCur
	tunerMu.Unlock()
	resp := map[string]any{"ok": true, "defaults": defaultTunerConfig(), "in_charge": chargeLabel(), "files_dir": tunerRunDir(),
		"overclock": loadOverclock().Acknowledged,
		"ocmax":     map[string]any{"freq": effHard("freq", freqHardMax), "vrun": effHard("vrun", vrunHardMax)}}
	if _, err := os.Stat(filepath.Join(tunerRunDir(), "log.txt")); err == nil {
		resp["has_files"] = true
	}
	// the stock plan and what it runs now, for the form's Miner / Baseline sections
	if sp, ok := readStockBaseline("stock"); ok {
		resp["stock"] = map[string]int{"mhz": sp.FreqMHz, "mv": sp.VrunMv, "pv": sp.PVMv}
	}
	if cp, ok := readStockBaseline("current"); ok {
		resp["running"] = map[string]int{"mhz": cp.FreqMHz, "mv": cp.VrunMv, "pv": cp.PVMv}
	}
	if t != nil {
		resp["status"] = t.statusView()
	}
	if tunerOnBox2() {
		resp["box2"] = box2TunerInfo()
	}
	writeJSON(w, resp)
}

// ===================== activity hub + log =====================

type hub struct {
	mu   sync.Mutex
	subs map[chan string]bool
}

func newHub() *hub { return &hub{subs: map[chan string]bool{}} }
func (h *hub) sub() chan string {
	ch := make(chan string, 64)
	h.mu.Lock()
	h.subs[ch] = true
	h.mu.Unlock()
	return ch
}
func (h *hub) unsub(ch chan string) {
	h.mu.Lock()
	delete(h.subs, ch)
	close(ch)
	h.mu.Unlock()
}
func (h *hub) emitRaw(line string) {
	h.mu.Lock()
	for ch := range h.subs {
		select {
		case ch <- line:
		default:
		}
	}
	h.mu.Unlock()
}
func (h *hub) emit(msg string) {
	line := time.Now().Format("15:04:05") + "  " + msg
	if h == activity {
		eventRecord(line) // kept for the private log export (incidents.go)
	}
	h.emitRaw(line)
}
func (h *hub) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

var activity = newHub()

// fmtShare renders a best-share difficulty with an SI suffix (e.g. 1.50 G, 287.00 M, 45.69 T).
func fmtShare(v float64) string {
	a := math.Abs(v)
	switch {
	case a >= 1e15:
		return fmt.Sprintf("%.2f P", v/1e15)
	case a >= 1e12:
		return fmt.Sprintf("%.2f T", v/1e12)
	case a >= 1e9:
		return fmt.Sprintf("%.2f G", v/1e9)
	case a >= 1e6:
		return fmt.Sprintf("%.2f M", v/1e6)
	case a >= 1e3:
		return fmt.Sprintf("%.1f K", v/1e3)
	default:
		return fmt.Sprintf("%.0f", v)
	}
}

func fmtUptime(sec int) string {
	if sec < 0 {
		sec = 0
	}
	h, m := sec/3600, (sec%3600)/60
	if h > 0 {
		return fmt.Sprintf("%dh %dm", h, m)
	}
	return fmt.Sprintf("%dm", m)
}

func curClockMHz(d dashboard) int {
	for _, b := range d.Boards {
		if b.Alive && b.ClockMHz > 0 {
			return int(b.ClockMHz + 0.5)
		}
	}
	return 0
}

// curPresetName is the active preset's bare name ("Middle"), or "" when not on a named preset.
func curPresetName() string {
	c := getCharge()
	if strings.HasPrefix(c, "preset:") {
		return strings.TrimPrefix(c, "preset:")
	}
	return ""
}

// Best-share alerts. Checked on every poll (not only when the record moves), so a record that's still
// waiting is sent as soon as it's allowed:
//   - not in the first 10 minutes after a restart, while the record climbs every few seconds; the best
//     reached by then is sent when the 10 minutes are up
//   - after that, every new record, as it happens. Records naturally get rarer the longer the miner runs
//     (about one per doubling of uptime). Best-share alerts have their own 2-minute gap (not the Settings
//     gap for problem alerts); a record found inside it is sent when the 2 minutes are up, latest value
var bestNotified float64 // last best share sent, or known when the agent connected (0 = none since restart)

const bestWarmupS = 600

const (
	hashLowWarmupS = 900 // no low-hashrate alert in the first 15 min after a restart (the average is still climbing)
	hashLowChecks  = 3   // ... and only when it's low on 3 checks in a row (about 30 s)
	bootRecentS    = 600 // agent started within 10 min of the system: treat the first connect as a reboot
)

// hashLowDue decides the low-hashrate alert: not while the miner is warming up after a restart or the tuner
// is changing settings, and only when it's been low on several checks in a row. run counts those checks.
func hashLowDue(elapsed int, charge string, ghs, avg float64, pct int, run *int) bool {
	if elapsed < hashLowWarmupS || charge == "tuner" || charge == "idle" || charge == "ceiling" || avg <= 0 || ghs >= avg*float64(pct)/100 {
		*run = 0
		return false
	}
	*run++
	return *run >= hashLowChecks
}

var uptimePath = "/proc/uptime"

var stopGrace = 3 * time.Minute // "stopped answering" only after this long without a reply

// bootMarkPath lives on /tmp (RAM, emptied at every boot), so it marks "this boot was already reported".
var bootMarkPath = brandTmp + "boot-alerted"

func bootAlertOnce() bool {
	f, err := os.OpenFile(bootMarkPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return false // already reported this boot (or /tmp unusable: better quiet than twice)
	}
	f.Close()
	return true
}

// systemUptime: seconds since the miner's Linux system started (/proc/uptime), or a large value if unknown.
func systemUptime() float64 {
	b, err := os.ReadFile(uptimePath)
	if err != nil {
		return 1e9
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return 1e9
	}
	v, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return 1e9
	}
	return v
}

func maybeNotifyBest(d dashboard) {
	if d.Elapsed < bestWarmupS || d.BestShare <= bestNotified+0.5 || d.BestShare <= 0 {
		return
	}
	was := bestNotified
	msg := "new best share " + fmtShareUnit(d.BestShare)
	if was > 0 {
		msg += " (was " + fmtShareUnit(was) + ")"
	}
	msg += ", up " + fmtUptime(d.Elapsed)
	if clk := curClockMHz(d); clk > 0 {
		msg += fmt.Sprintf(", on %d MHz", clk)
	}
	if p := curPresetName(); p != "" {
		msg += " · " + presetLabel(p)
	}
	if ever := bestEver(); ever > 0 && d.BestShare+0.5 >= ever {
		msg += " · this miner's best ever"
	}
	if notify(evBestShare, msg) { // false while the spacing holds it back: tried again next poll
		bestNotified = d.BestShare
	}
}

func pollActivity() {
	var prev dashboard
	var have, wasDown, stoppedSent bool
	var downSince time.Time
	var lastHW, prevSlowed, lowRun int
	bootAlert := systemUptime() < bootRecentS // the agent started with the system: a reboot or power-on
	var weakPrev map[ckey]bool
	lastHB := time.Now()
	t := time.NewTicker(*pollEvery)
	defer t.Stop()
	for range t.C {
		d := buildDashboard()
		if !d.OK {
			if have {
				activity.emit("⚠ miner API unreachable")
				have, wasDown, downSince = false, true, time.Now()
			}
			// a short gap is normal (the miner restarts its mining program after a boot or a settings
			// change): alert only when it's been silent for a while, and never during a panel restart/reboot
			if wasDown && !stoppedSent && time.Since(downSince) >= stopGrace {
				if _, _, rb := rebootActive(); !rb {
					notify(evStopped, fmt.Sprintf("the miner stopped answering (no reply for %s)", fmtUptime(int(time.Since(downSince).Seconds()))))
					stoppedSent = true
				}
			}
			continue
		}
		recordHashrate(d.GHS)
		shareObserve(d)
		if !have {
			// A restart almost always passes through an unreachable window, and the reconnect
			// path below used to `continue` before the uptime-reset check — so the chart never
			// got an R mark for real restarts. Detect it here, against the last pre-down uptime.
			restarted := prev.Elapsed > 0 && d.Elapsed+30 < prev.Elapsed
			if restarted {
				panel := panelRestartedRecently() // (the window closed in this very poll: the miner answered)
				if !panel {                       // a user-triggered restart/reboot already marked it
					recordMark("R", "miner restarted")
				}
				activity.emit("↻ miner restarted (uptime reset)")
				if !panel {
					notify(evRestarted, fmt.Sprintf("its mining software restarted on its own (it had been up %s); best share before it: %s",
						fmtUptime(prev.Elapsed), fmtShareUnit(prev.BestShare)))
				}
				bestNotified = 0 // "since restart" starts fresh; warm-up suppresses the early climb
			} else if bootAlert && bootAlertOnce() {
				// the whole miner rebooted (power cut, reboot button, watchdog): the agent restarted with it,
				// so it never saw the uptime drop. It's a restart all the same. Once per boot, even if the
				// agent itself restarts during it.
				bootAlert = false
				activity.emit("↻ miner booted up (reboot or power-on)")
				notify(evRestarted, fmt.Sprintf("the miner rebooted (power cut, reboot or watchdog) and is mining again · system up %s",
					fmtUptime(int(systemUptime()))))
				bestNotified = 0
			} else {
				bestNotified = d.BestShare // fresh connect: only ping for records set after we connected
			}
			bootAlert = false
			activity.emit(fmt.Sprintf("connected · %.1f GH/s · %d accepted · best %s", d.GHS, d.Accepted, fmtShareUnit(d.BestShare)))
			if wasDown {
				if stoppedSent { // only follow up an alert that was actually sent
					notifyForce(evBack, fmt.Sprintf("the miner is answering again after %s down", fmtUptime(int(time.Since(downSince).Seconds()))))
				}
				wasDown, stoppedSent = false, false
			}
			prev, have, lastHW, lastHB = d, true, d.HWErrors, time.Now()
			hwWarmConnect(d, &lastHW) // a run younger than 3 min is still warming up: its HW errors are summed up
			continue
		}
		// miner restart: cgminer uptime reset (mining relaunched or power-cycled)
		if d.Elapsed+30 < prev.Elapsed {
			panel := panelRestartedRecently()
			if !panel { // a user-triggered restart/reboot already marked it
				recordMark("R", "miner restarted")
			}
			activity.emit("↻ miner restarted (uptime reset)")
			if !panel {
				notify(evRestarted, fmt.Sprintf("its mining software restarted on its own (it had been up %s); best share before it: %s",
					fmtUptime(prev.Elapsed), fmtShareUnit(prev.BestShare)))
			}
			bestNotified = 0 // "since restart" starts fresh; warm-up suppresses the early climb
			lastHW = d.HWErrors
		}
		// Accepted/rejected/best are mirrored into the engine ("Mining") log too, so the work
		// log shows GOOD shares being submitted — the raw miner log only prints bad nonces.
		if d.Accepted > prev.Accepted {
			n := d.Accepted - prev.Accepted
			activity.emit(fmt.Sprintf("✓ share accepted  (total %d)", d.Accepted))
			engine.emit(fmt.Sprintf("✓ accepted +%d  (total %d · best %s)", n, d.Accepted, fmtShareUnit(d.BestShare)))
		}
		if d.Rejected > prev.Rejected {
			n := d.Rejected - prev.Rejected
			activity.emit(fmt.Sprintf("✗ share rejected  (total %d)", d.Rejected))
			engine.emit(fmt.Sprintf("✗ rejected +%d  (total %d)", n, d.Rejected))
			// a reject (not a stale — bfgminer counts stale separately) → mark the chart
			recordMark("X", fmt.Sprintf("share rejected (+%d, total %d)", n, d.Rejected))
		}
		if d.Stale > prev.Stale {
			// a stale share (pool timing, not the miner): kept for the rejected-shares strip, not the chart
			recordMark("S", fmt.Sprintf("stale share (+%d, total %d)", d.Stale-prev.Stale, d.Stale))
		}
		if d.Rejected > prev.Rejected {
			notify(evRejected, fmt.Sprintf("a share was rejected (total %d)", d.Rejected))
		}
		if d.BestShare > prev.BestShare+0.5 {
			activity.emit(fmt.Sprintf("★ new best share: %s", fmtShareUnit(d.BestShare)))
			engine.emit(fmt.Sprintf("★ new best share: %s", fmtShareUnit(d.BestShare)))
		}
		maybeNotifyBest(d) // every poll: also sends a record that was waiting (warm-up, spacing)
		if d.Accepted > prev.Accepted {
			notify(evAccepted, fmt.Sprintf("share accepted (total %d)", d.Accepted))
		}
		hwWarmTick(prev, d, &lastHW)
		if d.HWErrors > lastHW {
			activity.emit(fmt.Sprintf("⚠ HW error  (+%d, total %d, %.2f%%)", d.HWErrors-lastHW, d.HWErrors, d.HWPct))
			lastHW = d.HWErrors
		}
		pb := map[int]board{}
		for _, b := range prev.Boards {
			pb[b.ID] = b
		}
		for _, b := range d.Boards {
			if o, ok := pb[b.ID]; ok {
				if o.Alive != b.Alive {
					st := "DEAD"
					if b.Alive {
						st = "ALIVE"
					}
					activity.emit(fmt.Sprintf("board %d → %s", b.ID, st))
				}
				if !o.Overheat && b.Overheat {
					activity.emit(fmt.Sprintf("⚠ board %d OVERHEAT", b.ID))
				}
			}
		}
		if time.Since(lastHB) >= 10*time.Second {
			var mx float64
			for _, b := range d.Boards {
				if b.Alive {
					for _, tt := range b.Temps {
						if tt > mx {
							mx = tt
						}
					}
				}
			}
			activity.emit(fmt.Sprintf("· %.1f GH/s · HW %.2f%% · %.0f°C", d.GHS, d.HWPct, mx))
			lastHB = time.Now()
			// --- notification checks (rate-limited inside notify) ---
			notifyMu.Lock()
			hotC, lowPct := notifyCfg.HotC, notifyCfg.HashLowPct
			notifyMu.Unlock()
			if hotC > 0 && mx >= hotC {
				notify(evHot, fmt.Sprintf("too hot: %.0f°C (alert at %.0f°C)", mx, hotC))
			}
			if lowPct > 0 {
				avg, n := hrAvgSince(time.Now().Unix() - 24*3600)
				if n <= 5 {
					avg = 0
				}
				if hashLowDue(d.Elapsed, getCharge(), d.GHS, avg, lowPct, &lowRun) {
					notify(evHashLow, fmt.Sprintf("hashrate low: %.0f GH/s (under %d%% of the usual %.0f GH/s)", d.GHS, lowPct, avg))
				}
			}
			// chip turned weak: a chip newly slowed by the driver (DTFS). Name the new one(s).
			weakNow := dtfsSnapshot()
			if len(weakNow) > prevSlowed {
				var fresh []string
				for k := range weakNow {
					if !weakPrev[k] {
						fresh = append(fresh, fmt.Sprintf("b%dc%d", k.B, k.I)) // no-dot, matches the Chips page
					}
				}
				sort.Strings(fresh)
				switch {
				case len(fresh) == 1:
					notify(evChipWeak, fmt.Sprintf("chip health: %s turned weak (see Dashboard → Chips)", fresh[0]))
				case len(fresh) > 1:
					notify(evChipWeak, fmt.Sprintf("chip health: %s turned weak (see Dashboard → Chips)", strings.Join(fresh, ", ")))
				default:
					notify(evChipWeak, fmt.Sprintf("chip health: %d chip(s) now slowed (see Dashboard → Chips)", len(weakNow)))
				}
			}
			weakPrev = weakNow
			prevSlowed = len(weakNow)
			// boards, fans and pools (each sends once per incident and when it's over)
			ch := getCharge()
			boardWatch(d, ch)
			fanWatch(d, ch)
			if pools, err := readPools(); err == nil {
				poolWatch(pools, ch)
			}
		}
		prev = d
	}
}

// notifyForce sends an event bypassing the repeat rate-limit (recoveries and records).
func notifyForce(event, msg string) {
	if notifyTestHook != nil {
		notifyTestHook(event, msg)
		return
	}
	key := event
	if event == evBack { // "back" follows the "stops answering" switch
		key = evStopped
	}
	notifyMu.Lock()
	c := notifyCfg
	if c.Events == nil || !c.Events[key] || !c.anyOn() {
		notifyMu.Unlock()
		return
	}
	// a pool that keeps dropping and coming back: at most 4 a hour per event (the activity log has them all)
	now := time.Now()
	recent := notifyForceLog[event][:0]
	for _, t := range notifyForceLog[event] {
		if now.Sub(t) < time.Hour {
			recent = append(recent, t)
		}
	}
	if (event == evNoPool || event == evPoolSw) && len(recent) >= 4 {
		notifyForceLog[event] = recent
		notifyMu.Unlock()
		return
	}
	notifyForceLog[event] = append(recent, now)
	notifyLast[event] = now
	notifyMu.Unlock()
	go sendAll(c, event, msg)
}

var notifyForceLog = map[string][]time.Time{}

func sseStream(w http.ResponseWriter, r *http.Request, source func(ctx context.Context) <-chan string) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "no streaming", 500)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	ctx := r.Context()
	ch := source(ctx)
	ka := time.NewTicker(15 * time.Second)
	defer ka.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case line, ok := <-ch:
			if !ok {
				return
			}
			fmt.Fprintf(w, "data: %s\n\n", strings.NewReplacer("\r", " ", "\n", " ").Replace(line))
			fl.Flush()
		case <-ka.C:
			io.WriteString(w, ": ka\n\n")
			fl.Flush()
		}
	}
}

func handleActivity(w http.ResponseWriter, r *http.Request) {
	sseStream(w, r, func(ctx context.Context) <-chan string {
		ch := activity.sub()
		go func() { <-ctx.Done(); activity.unsub(ch) }()
		return ch
	})
}

// ---- real engine log: stream the stock /dbg/minersyslog (AxeOS-style feed) ----

var engine = newHub()

func logText(raw []byte) string {
	var o struct {
		Body string `json:"body"`
	}
	if json.Unmarshal(raw, &o) == nil && o.Body != "" {
		return o.Body
	}
	return string(raw)
}

func pollEngine() {
	var last string
	first := true
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for range t.C {
		if engine.count() == 0 { // nobody watching: idle, reset so next viewer gets fresh tail
			first, last = true, ""
			continue
		}
		raw, err := stockGet("/dbg/minersyslog")
		if err != nil {
			if first {
				if !intminerRunning() {
					engine.emitRaw("(no mining log: the mining program (intminer) isn't running, so it has nothing to write. The Dashboard says more.)")
				} else {
					engine.emitRaw("(mining log unavailable: " + err.Error() + ")")
				}
				first = false
			}
			continue
		}
		lines := strings.Split(strings.ReplaceAll(logText(raw), "\r\n", "\n"), "\n")
		start := 0
		if !first && last != "" {
			idx := -1
			for i := len(lines) - 1; i >= 0; i-- {
				if lines[i] == last {
					idx = i
					break
				}
			}
			if idx >= 0 {
				start = idx + 1
			} else if len(lines) > 30 { // rotated/reset: just show recent tail
				start = len(lines) - 30
			}
		} else if len(lines) > 30 {
			start = len(lines) - 30
		}
		for _, l := range lines[start:] {
			if strings.TrimSpace(l) != "" && !agentAPINoise(l) {
				engine.emitRaw(strings.TrimRight(l, " "))
			}
		}
		for i := len(lines) - 1; i >= 0; i-- {
			if strings.TrimSpace(lines[i]) != "" {
				last = lines[i]
				break
			}
		}
		first = false
	}
}

// ---- share log: tail intminer's --sharelog and surface the submitted nonce ----
//
// When verbose is on, the intminer shim adds `--sharelog <file>`. bfgminer writes one CSV row per
// submitted share: ts,disposition,target,pool,device,thr,share-hash,80-byte-header. The 8-byte
// Blake2b nonce sits at bytes 32..39 of the header (right after the 32-byte parent id), stored
// little-endian — confirmed against live shares (the 4-byte prefix is shared, these 8 bytes
// differ per share). We show every submitted share with its nonce, AxeOS-style, in the work log.

func sharelogPath() string { return filepath.Join(filepath.Dir(*authFile), "shares.log") }

// shareNonce extracts the 8-byte Siacoin nonce from the sharelog header field and returns it as a
// big-endian hex string (reversed from the little-endian bytes in the header).
func shareNonce(header string) string {
	if len(header) < 80 {
		return ""
	}
	le := header[64:80] // bytes 32..39
	var sb strings.Builder
	for i := len(le) - 2; i >= 0; i -= 2 {
		sb.WriteString(le[i : i+2])
	}
	return sb.String()
}

func formatShare(line string) string {
	f := strings.Split(line, ",")
	if len(f) < 8 {
		return ""
	}
	disp, dev := strings.TrimSpace(f[1]), strings.TrimSpace(f[4])
	if strings.HasPrefix(dev, "INCS") { // intminer's name for the hashboard/chain → friendlier
		dev = "board " + strings.TrimPrefix(dev, "INCS")
	}
	verb := disp
	switch disp {
	case "accept":
		verb = "accepted"
	case "reject":
		verb = "rejected"
	case "stale":
		verb = "stale"
	}
	if n := shareNonce(strings.TrimSpace(f[7])); n != "" {
		return fmt.Sprintf("➦ share %s · nonce 0x%s · %s", verb, n, dev)
	}
	return fmt.Sprintf("➦ share %s · %s", verb, dev)
}

func pollShareLog() {
	var pos int64 = -1
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	n := 0
	for range t.C {
		if n++; n%30 == 0 { // the miner appends a line per share for as long as Verbose is on: kept small
			if fi, err := os.Stat(sharelogPath()); err == nil && fi.Size() > 256<<10 {
				os.Truncate(sharelogPath(), 0) // bfgminer appends (O_APPEND): it carries on at the new end
				pos = -1
			}
		}
		if engine.count() == 0 {
			pos = -1 // no viewers: next time, start from the current end (skip backlog)
			continue
		}
		fi, err := os.Stat(sharelogPath())
		if err != nil {
			pos = -1
			continue
		}
		if pos < 0 || pos > fi.Size() { // first read, or file reset/rotated
			pos = fi.Size()
			continue
		}
		if fi.Size() == pos {
			continue
		}
		f, err := os.Open(sharelogPath())
		if err != nil {
			continue
		}
		f.Seek(pos, io.SeekStart)
		rd := bufio.NewReader(f)
		for {
			line, err := rd.ReadString('\n')
			if len(line) > 0 && strings.HasSuffix(line, "\n") {
				pos += int64(len(line))
				if s := formatShare(strings.TrimRight(line, "\r\n")); s != "" {
					engine.emit(s)
				}
			}
			if err != nil {
				break
			}
		}
		f.Close()
	}
}

// ---- DTFS per-chip slowdown tracking ----
//
// The driver logs `C<chain>: DTFS chip(<n>) slowdown` / `C<chain>: chip(<n>) Individual` when it
// auto-slows an individual chip (there is no user API for per-chip clock — see per_chip_findings.md).
// We tail /dbg/minersyslog and record which chips the driver has slowed, so the chip map can flag
// them. Read-only. Regex matches the user's own app (health_addon.py). Entries expire after 1h.

var dtfsRe = regexp.MustCompile(`C(\d{1,3}): (?:DTFS chip\((\d{1,4})\)|chip\((\d{1,4})\) Individual)`)

var (
	dtfsMu   sync.Mutex
	dtfsSeen = map[ckey]time.Time{}
)

func scanDTFS(lines []string) {
	now := time.Now()
	dtfsMu.Lock()
	for _, l := range lines {
		m := dtfsRe.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		b, _ := strconv.Atoi(m[1])
		cs := m[2]
		if cs == "" {
			cs = m[3]
		}
		ci, _ := strconv.Atoi(cs)
		dtfsSeen[ckey{B: b, I: ci}] = now
	}
	for k, t := range dtfsSeen {
		if now.Sub(t) > time.Hour {
			delete(dtfsSeen, k)
		}
	}
	dtfsMu.Unlock()
}

func dtfsSnapshot() map[ckey]bool {
	now := time.Now()
	dtfsMu.Lock()
	defer dtfsMu.Unlock()
	out := make(map[ckey]bool, len(dtfsSeen))
	for k, t := range dtfsSeen {
		if now.Sub(t) <= time.Hour {
			out[k] = true
		}
	}
	return out
}

// logStampRe: the time stamp bfgminer puts on each log line, " [2026-11-05 04:03:06] …".
var logStampRe = regexp.MustCompile(`^\s*\[(\d{4}-\d\d-\d\d \d\d:\d\d:\d\d)\]`)

// rejectScanStart: where the reject scan starts. When the last line seen has scrolled out of the
// miner's log (it keeps a few hundred lines; verbose fills that in seconds), the lines already read
// are still in the buffer: only those stamped later than the last line seen are new, or reasons
// already shown would be put on the next reject again.
func rejectScanStart(lines []string, last string, start int, found bool) int {
	if found {
		return start
	}
	m := logStampRe.FindStringSubmatch(last)
	if m == nil {
		return len(lines)
	}
	for i, l := range lines {
		if t := logStampRe.FindStringSubmatch(l); t != nil && t[1] > m[1] {
			return i
		}
	}
	return len(lines)
}

func pollDTFS() {
	var last string
	t := time.NewTicker(20 * time.Second)
	defer t.Stop()
	for range t.C {
		raw, err := stockGet("/dbg/minersyslog")
		if err != nil {
			continue
		}
		lines := strings.Split(strings.ReplaceAll(logText(raw), "\r\n", "\n"), "\n")
		start, found := 0, false
		if last != "" {
			for i := len(lines) - 1; i >= 0; i-- {
				if lines[i] == last {
					start, found = i+1, true
					break
				}
			}
		}
		if start < len(lines) {
			scanDTFS(lines[start:])
			scanBoardHealth(lines[start:])
		}
		if last != "" { // not the first read: old rejects in the backlog have no mark to match
			if rs := rejectScanStart(lines, last, start, found); rs < len(lines) {
				scanRejects(lines[rs:])
			}
		}
		for i := len(lines) - 1; i >= 0; i-- {
			if strings.TrimSpace(lines[i]) != "" {
				last = lines[i]
				break
			}
		}
	}
}

func handleEngine(w http.ResponseWriter, r *http.Request) {
	sseStream(w, r, func(ctx context.Context) <-chan string {
		ch := engine.sub()
		go func() { <-ctx.Done(); engine.unsub(ch) }()
		return ch
	})
}

func noise(line string) bool {
	return strings.Contains(line, "imapx_i2c_handle_tx_abort") ||
		(strings.Contains(line, "I2C") && strings.Contains(line, "stat=0x"))
}

// logReopen: the log at path is no longer the file f has open (rotated), or it was cut shorter than where f
// reads (truncated): the file to read from now on (from its start), and true.
func logReopen(f *os.File, path string) (*os.File, bool) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, false // between the rename and the new file: next time
	}
	cur, err := f.Stat()
	if err != nil {
		return nil, false
	}
	if os.SameFile(fi, cur) {
		if off, err := f.Seek(0, io.SeekCurrent); err == nil && fi.Size() < off {
			f.Seek(0, io.SeekStart) // truncated in place: from its start
		}
		return nil, false
	}
	nf, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	return nf, true
}

func handleLog(w http.ResponseWriter, r *http.Request) {
	sseStream(w, r, func(ctx context.Context) <-chan string {
		out := make(chan string, 256)
		go func() {
			defer close(out)
			f, err := os.Open(*logPath)
			if err != nil {
				out <- fmt.Sprintf("(log %s: %v)", *logPath, err)
				return
			}
			defer func() { f.Close() }() // f changes when the log is rotated
			// show recent history (noise-filtered) so the panel isn't blank on connect
			existing, _ := io.ReadAll(f)
			var kept []string
			for _, l := range strings.Split(string(existing), "\n") {
				l = strings.TrimRight(l, "\r")
				if strings.TrimSpace(l) != "" && !noise(l) {
					kept = append(kept, l)
				}
			}
			if len(kept) > 40 {
				kept = kept[len(kept)-40:]
			}
			if len(kept) == 0 {
				out <- "(system log is quiet — only filtered I2C/kernel probe noise right now)"
			}
			for _, l := range kept {
				select {
				case out <- l:
				case <-ctx.Done():
					return
				}
			}
			// f is now at EOF after ReadAll; tail new lines from here
			rd := bufio.NewReader(f)
			for {
				select {
				case <-ctx.Done():
					return
				default:
				}
				line, err := rd.ReadString('\n')
				if len(line) > 0 {
					if l := strings.TrimRight(line, "\r\n"); !noise(l) {
						select {
						case out <- l:
						case <-ctx.Done():
							return
						}
					}
				}
				if err == io.EOF {
					// busybox syslogd renames the log to .0 at 200 KB and starts a new file: follow it
					if nf, err := os.Open(*logPath); err == nil {
						cur, e1 := f.Stat()
						now, e2 := nf.Stat()
						if e1 == nil && e2 == nil && !os.SameFile(cur, now) {
							f.Close()
							f, rd = nf, bufio.NewReader(nf)
							continue
						}
						nf.Close()
						if off, err := f.Seek(0, io.SeekCurrent); err == nil && e1 == nil && cur.Size() < off {
							f.Seek(0, io.SeekStart) // cut back to nothing (syslogd -b 0): read it again from the start
							rd.Reset(f)
							continue
						}
					}
					time.Sleep(300 * time.Millisecond)
					// syslogd rotates the log (messages -> messages.0, then a new messages): follow the new
					// file, else the stream goes quiet for good and keeps the old (later deleted) file open
					if nf, ok := logReopen(f, *logPath); ok {
						f.Close()
						f, rd = nf, bufio.NewReader(nf)
					}
				} else if err != nil {
					return
				}
			}
		}()
		return out
	})
}

// faviconSVG: the tab icon, a small gold miner box with a dark ₿-style B (drawn for this panel; the Bitcoin
// symbol is public domain).
const faviconSVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64"><defs><linearGradient id="g" x1="0" y1="0" x2="0" y2="1"><stop offset="0" stop-color="#f6c344"/><stop offset="1" stop-color="#c98a10"/></linearGradient></defs><rect x="3" y="9" width="58" height="46" rx="8" fill="url(#g)" stroke="#6b4400" stroke-width="2.5"/><g stroke="#3a2600" stroke-width="2.4" stroke-linecap="round" opacity=".55"><path d="M11 15H17M21 15H27M37 15H43M47 15H53M11 49H17M21 49H27M37 49H43M47 49H53"/></g><g fill="none" stroke="#161616" stroke-linecap="round" stroke-linejoin="round"><path d="M25 21V43M25 21H34a5.5 5.5 0 0 1 0 11H25M25 32H36a5.5 5.5 0 0 1 0 11H25" stroke-width="4.6"/><path d="M28.5 16.5V21M33 16.5V21M28.5 43V47.5M33 43V47.5" stroke-width="3.2"/></g></svg>`

func handleFavicon(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	io.WriteString(w, faviconSVG)
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache") // a new firmware's page, not the browser's old copy
	io.WriteString(w, indexPage)
}

// pageBuild identifies this panel's page. An open page that sees another build in /api/dashboard (after a
// firmware update or a roll back: the page reconnects by itself) reloads, so it never runs the old page.
var pageBuild, indexPage = func() (string, string) {
	// the page's own names (browser storage keys like 'sclite-theme', the fallback download name, the MAC boot
	// step, the tab title and product name shown before the model profile loads) carry the firmware's name:
	// identical on the SC Lite build, the SC Box II's own on that build
	page := strings.NewReplacer("'sclite-", "'"+brand+"-", "/etc/sclite-mac", brandMacScript,
		"<title>SC Lite</title>", "<title>"+brandProduct+"</title>", // the tab title before the model loads (sign-in)
		`class="brand">SC&nbsp;LITE`, `class="brand">`+brandHeader, // the header name before the model loads
		"return (m&&m.name)||'SC Lite';}", "return (m&&m.name)||'"+brandProduct+"';}",
		"?'OEM':'Goldshell');}", "?'OEM':'"+brandOEM+"');}").Replace(indexHTML) // the manufacturer's name before the model loads (OEM())
	h := sha256.Sum256([]byte(page))
	b := hex.EncodeToString(h[:6])
	return b, strings.Replace(page, "__PAGE_BUILD__", b, 1)
}()

func main() {
	backupFlag := flag.String("backup", "/backup", "where Goldshell's own programs keep their logs")
	productFlag := flag.String("product", "/root/product.json", "Goldshell's product file (fans' max RPM)")
	mcbFlag := flag.String("mcbversion", "/root/mcbversion", "Goldshell's control-board version file (model profile match)")
	flag.Parse()
	backupRoot, productPath = *backupFlag, *productFlag
	// Pick the model profile from Goldshell's own identity files. A mismatch yields the "unknown" profile
	// (monitoring only, every write feature off). This seeds the clock/voltage windows for the whole run.
	model, modelWhy = loadModelFiles(productPath, *mcbFlag)
	applyCommission() // the features the owner has proven and switched on, on this unit (commission.go)
	setWindowsFromModel()
	setGeometryFromModel() // board/chip geometry + init-log tag from the profile (boardhealth.go)
	setThermalFromModel()  // seed the thermal guard/throttle thresholds from the profile (all < the cut-off)
	// Decide the config write mode (FAT vs ext), note a read-only squashfs root, and clear any *.tmp a power
	// cut left in the config dir. Must run after the model is known and after flag.Parse (it reads *authFile).
	initConfigStore()
	// On the Box II the Goldshell daemons' log dir (/backup) would be on flash; keep those scratch logs in RAM
	// instead (R04/R19). The SC Lite's /backup is on ext and unchanged. A boot script that passes -backup wins.
	if model.Plan == planBox2V && *backupFlag == "/backup" {
		backupRoot = "/dev/shm"
	}
	log.Printf("model: %s [%s] (%s) — %d board(s), %d chips/board, %d fan(s), cutoff %.0f°C; config %s, root %s",
		model.Name, model.ID, modelWhy, model.Boards, model.ChipsPerBoard, model.Fans, model.TempCutoffC,
		map[bool]string{true: "FAT (crash-safe writes)", false: "ext"}[configFAT],
		map[bool]string{true: "read-only squashfs", false: "writable"}[rootIsSquashfs()])
	if *installUpd != "" {
		if r := os.Getenv(brandEnv("UPDATE_ROOT")); r != "" {
			updateRoot = r
		}
		runInstallUpdate(*installUpd)
	}
	loadNetConfig()                                           // a saved web-UI port overrides the -listen flag default
	if err := ensureSaneDNS(currentNetConfig()); err != nil { // apply the owner's DNS / strip 114.114.114.114 (netdns.go)
		log.Printf("dns: %v", err)
	}
	// Handle auth + stock password first so that `-install-service -miner-pass X` both stores
	// the password and installs the boot script before exiting.
	_, aerr := os.Stat(*authFile)
	freshInstall = os.IsNotExist(aerr) // no panel files yet: first start (from stock, or a wiped config)
	if err := loadAuth(); err != nil {
		log.Printf("auth: couldn't save the login file (%v): using it from memory this time", err) // never exit: no panel = no SSH either
	}
	loadRevoked()                  // sessions signed out before a restart stay signed out (access.go)
	accessLoad(currentNetConfig()) // local network only / HTTPS only, as saved (access.go)
	if sshViaDaemon = brand == "scbox2"; sshViaDaemon {
		go sshdEventsLoop() // SSH is scbox2-sshd's (sshd_link.go): its sign-ins, blocks and password change
	}
	loadTZ()
	chipTestRecover()
	sctRecover()
	// chip trims not commissioned on this unit: no chiphook flag (chipgeo.go)
	chipHookInertAtStart()
	if r := os.Getenv(brandEnv("UPDATE_ROOT")); r != "" { // testing: install into a copy of a system tree, no reboot
		updateRoot = r
		updateReboot = func() { log.Printf("update: (test) reboot skipped") }
	}
	confirmUpdateLater()
	if *minerPass != "" {
		authMu.Lock()
		auth.MinerPass = *minerPass
		authMu.Unlock()
		if err := saveAuth(); err != nil {
			log.Printf("warning: could not store miner-pass: %v", err)
		} else {
			log.Printf("stored stock miner web password for /dbg access")
		}
	}
	if *installSvc {
		if _, err := os.Stat(brandRelease); err == nil {
			// this firmware's own S48sclite (rollback, product.json, the restart loop) must not be replaced
			log.Fatalf("install-service: this miner runs the " + brand + " firmware, whose boot script already starts the panel: nothing to install")
		}
		if err := installService(); err != nil {
			log.Fatalf("install-service failed: %v", err)
		}
		log.Printf("installed %s — agent will auto-start on boot", brandInitScript)
		return
	}
	loadFanConfig()
	restartRestore() // an overheat Idle's hold, a throttled clock: as the last run left them
	// Box II: a soft idle held when the panel stopped (lane C1; nothing at all without its marker file)
	box2SoftIdleBoot()
	box2RenameReservedPresets() // Box II: a v0.4.x preset named "Stock High"/"Stock Low" gets its own name (box2presetnames.go)
	box2PlanBoot()              // Box II: a plan the gates no longer allow, or one with a fan duty in its 4th field (box2planboot.go)
	productCheckAtStart()
	loadHashrate()
	loadNotifyConfig()
	recoveryStartup() // hardware watchdog / kept system log, when switched on (recovery.go)
	if freshInstall {
		saveMigration156(migration156{At: time.Now().Unix(), Stock: "fresh install"}) // nothing to migrate: the owner's own setting stays
	}
	if model.Plan != planBox2V { // v1.5.6's fresh start was the SC Lite's (its old PV readings); nothing of it applies on Box II
		migrate156(10 * time.Minute) // once: presets cleared, miner back on stock (see migrate156.go)
	}
	go minerHealthLoop()
	if model.Plan == planBox2V && model.Feat.SoftIdle {
		go box2IdleWatchLoop() // lane C1: confirm each stop, re-apply it after a miner restart, report one ignored
	}
	go tunerRecover(10 * time.Minute) // a tuning run cut short by a restart: put the miner back
	go fanRerunRecover()              // a fan test re-run cut short by a restart: its Idle ends
	go pollActivity()
	go pollEngine()
	go pollShareLog()
	go scheduleLoop()
	go autoMinerPassLoop()
	go pollDTFS()
	go fanLoop()
	go eventsLoop()
	go fanPairLoop()
	go chipHistoryLoop()
	go poolGuardLoop()
	go chipWatchLoop()
	go updateResultNotify()
	go newFirmwareLoop()
	go dailyLoop()
	mux := http.NewServeMux()
	mux.HandleFunc("/", handleIndex)
	mux.HandleFunc("/favicon.svg", handleFavicon)
	mux.HandleFunc("/favicon.ico", handleFavicon) // browsers that ask for the old name get the same icon
	mux.HandleFunc("/api/me", handleMe)
	mux.HandleFunc("/api/model", requireAuth(handleModel))
	mux.HandleFunc("/api/commission", requireAuth(handleCommission))
	mux.HandleFunc("/api/login", handleLogin)
	mux.HandleFunc("/api/logout", handleLogout)
	mux.HandleFunc("/api/passwd", requireAuth(postOnly(handlePasswd)))
	mux.HandleFunc("/api/acknowledge", requireAuth(postOnly(handleAck)))
	mux.HandleFunc("/api/ssh", requireAuth(handleSSH))
	mux.HandleFunc("/api/time", requireAuth(handleTime))
	mux.HandleFunc("/api/power", requireAuth(handlePower))
	mux.HandleFunc("/api/chiptrim", requireAuth(handleChipTrim))
	mux.HandleFunc("/api/strongtest", requireAuth(handleStrongTest))
	mux.HandleFunc("/api/chipreport", requireAuth(handleChipReport))
	mux.HandleFunc("/api/mac", requireAuth(handleMAC))
	mux.HandleFunc("/api/net", requireAuth(handleNet))
	mux.HandleFunc("/api/display", requireAuth(handleDisplay))
	mux.HandleFunc("/api/restart", requireAuth(postOnly(handleRestart)))
	mux.HandleFunc("/api/boardauto", requireAuth(handleBoardAuto))
	mux.HandleFunc("/api/reboot", requireAuth(postOnly(handleReboot)))
	mux.HandleFunc("/api/recovery", requireAuth(handleRecovery))
	mux.HandleFunc("/api/fanpair", requireAuth(handleFanPair))
	mux.HandleFunc("/api/settings", requireAuth(handleSettings))
	mux.HandleFunc("/api/settings/export", requireAuth(handleSettingsExport))
	mux.HandleFunc("/api/settings/import", requireAuth(postOnly(handleSettingsImport)))
	mux.HandleFunc("/api/notify/test", requireAuth(handleNotifyTest))
	mux.HandleFunc("/api/dashboard", requireAuth(handleDashboard))
	mux.HandleFunc("/api/hashrate", requireAuth(handleHashrate))
	mux.HandleFunc("/api/pools", requireAuth(handlePools))
	mux.HandleFunc("/api/presets", requireAuth(handlePresets))
	mux.HandleFunc("/api/schedule", requireAuth(handleSchedule))
	mux.HandleFunc("/api/shares", requireAuth(handleShares))
	mux.HandleFunc("/api/chips", requireAuth(handleChips))
	mux.HandleFunc("/api/cmd/", requireAuth(handleCmd))
	mux.HandleFunc("/api/activity/stream", requireAuth(handleActivity))
	mux.HandleFunc("/api/engine/stream", requireAuth(handleEngine))
	mux.HandleFunc("/api/verbose", requireAuth(handleVerbose))
	mux.HandleFunc("/api/control", requireAuth(handleControl))
	mux.HandleFunc("/api/overclock", requireAuth(handleOverclock))
	mux.HandleFunc("/api/ceiling", requireAuth(handleCeiling))
	mux.HandleFunc("/api/fan", requireAuth(handleFan))
	mux.HandleFunc("/api/fancheck", requireAuth(handleFanCheck))
	mux.HandleFunc("/api/tuner", requireAuth(handleTuner))
	mux.HandleFunc("/api/tuner/export", requireAuth(handleTunerExport))
	mux.HandleFunc("/api/tuner/chipmap", requireAuth(handleTunerChipMap))
	mux.HandleFunc("/api/update", requireAuth(handleUpdate))
	mux.HandleFunc("/api/poolguard", requireAuth(handlePoolGuard))
	mux.HandleFunc("/api/update/upload", requireAuth(handleUpdateUpload))
	mux.HandleFunc("/api/log/stream", requireAuth(handleLog))
	mux.HandleFunc("/api/log/export", requireAuth(handleLogExport))
	log.Printf(brandAgent+" v2 on %s (api %s, log %s, auth %s)", *listenAddr, *apiAddr, *logPath, *authFile)
	h := httpsRedirect(securityHeaders(mux))
	ln, err := net.Listen("tcp", *listenAddr) // the panel's port first: SSH can never take it
	if err != nil && *listenAddr != ":8080" {
		// a saved port that can't be opened (taken, or reserved) must not lock the owner out: fall back
		log.Printf("can't listen on %s (%v): falling back to :8080", *listenAddr, err)
		activity.emit(fmt.Sprintf("⚠ the web-UI port %s couldn't be opened (%v): the panel is on :8080", strings.TrimPrefix(*listenAddr, ":"), err))
		*listenAddr = ":8080"
		ln, err = net.Listen("tcp", *listenAddr)
	}
	sshApply()       // after the panel's own port is taken (SSH can't take it), before any wait for it
	for err != nil { // :8080 itself busy (e.g. an old panel still exiting): keep trying, never exit
		log.Printf("can't listen on %s (%v): retrying", *listenAddr, err)
		time.Sleep(3 * time.Second)
		ln, err = net.Listen("tcp", *listenAddr)
	}
	go netCheckResponder() // xminerd's "internet" check (127.0.0.2:80, see netlisten.go)
	go dnsGuard()          // keep the owner's DNS (or strip 114.114.114.114) in resolv.conf (netdns.go)
	_, mainPort, _ := net.SplitHostPort(*listenAddr)
	// port 80 on each LAN address (just the IP in the browser opens the panel) and https on 443, unless
	// switched off in Settings -> Network (access.go)
	extraPortsStart(h, mainPort)
	log.Fatal(servePanel(ln, h, false))
}

// securityHeaders adds defense-in-depth headers: no framing (clickjacking on the control UI),
// no MIME sniffing, no referrer leakage, and a CSP that blocks any externally-injected script
// (the page is a single self-contained file; inline is allowed, external origins are not).
func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ip := net.ParseIP(remoteIP(r)); ip != nil && !ip.IsLoopback() {
			lastNetHit.Store(monoSec()) // reached from the network (an update is confirmed only then)
		}
		// cap request bodies so a huge POST can't exhaust memory on a low-RAM miner (no uploads here)
		if r.Body != nil {
			max := int64(1 << 20)
			if r.URL.Path == "/api/update/upload" { // the one upload: a firmware update package
				max = updateMaxBytes + 1<<20
			}
			r.Body = http.MaxBytesReader(w, r.Body, max)
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				// a body trickled in a byte at a time mustn't hold a connection (and a file handle) for ever
				d := 30 * time.Second
				if r.URL.Path == "/api/update/upload" {
					d = 10 * time.Minute
				}
				http.NewResponseController(w).SetReadDeadline(time.Now().Add(d))
			}
		}
		hd := w.Header()
		hd.Set("X-Frame-Options", "DENY")
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("Referrer-Policy", "no-referrer")
		hd.Set("Content-Security-Policy",
			"default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'; "+
				"connect-src 'self'; img-src 'self' data:; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
		h.ServeHTTP(w, r)
	})
}

const indexHTML = `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>SC Lite</title><link rel="icon" type="image/svg+xml" href="/favicon.svg"><style>
 :root{color-scheme:dark;--pair:#5aa9ff;--pairbg:rgba(90,169,255,.07);--bg:#000;--panel:#0e0e0e;--panel2:#1a1a1a;--border:#2c2c2c;--line:#2c2c2c;--text:#f4f4f4;--muted:#a0a0a0;--accent:#fff;--accent2:#d6d6d6;--ok:#3dd68c;--warn:#f0b429;--bad:#ff6b6b;--btn:#161616;--btn-h:#222;--logbg:#060606;--logtx:#cfcfcf;--hdr:rgba(0,0,0,.9);--hover:#222;--sel-bg:#1f1f1f;--tip-bg:#141414;--cell-empty:#161616;--cell-edge:#444;--over:#d03b3b;--ramp-0:#5a5a5a;--ramp-1:#7a7a7a;--ramp-2:#9a9a9a;--ramp-3:#b8b8b8;--ramp-4:#d6d6d6;--ramp-5:#f4f4f4}
 :root[data-theme="light"]{color-scheme:light;--pair:#1d6fd1;--pairbg:rgba(29,111,209,.06);--bg:#ffffff;--panel:#f6f6f6;--panel2:#ececec;--border:#8f8f8f;--line:#c4c4c4;--text:#0a0a0a;--muted:#555555;--accent:#0a0a0a;--accent2:#2c2c2c;--ok:#0a7a3d;--warn:#8a5c00;--bad:#c62828;--btn:#f1f1f1;--btn-h:#e4e4e4;--logbg:#ffffff;--logtx:#1a1a1a;--hdr:rgba(255,255,255,.9);--hover:#e2e2e2;--sel-bg:#e4e4e4;--tip-bg:#ffffff;--cell-empty:#ffffff;--cell-edge:#969696;--over:#c62828;--ramp-0:#9e9e9e;--ramp-1:#808080;--ramp-2:#626262;--ramp-3:#474747;--ramp-4:#2c2c2c;--ramp-5:#0a0a0a}
 :root[data-theme="light"] button.primary,:root[data-theme="light"] button.active:not(.tab-btn){color:#fff}
 :root[data-theme="light"] .pill.ok{border-color:#9fd4b4;background:#e8f6ee}
 :root[data-theme="light"] .pill.bad{border-color:#e3a1a1;background:#fdeaea}
 :root[data-theme="light"] .warnbar{background:#fbf3de;border-color:#e2c98a;color:#7a5b00}
 .warnbar.ok{background:#0c1a0c;border-color:#1f5a3c;color:var(--ok)}
 :root[data-theme="light"] .warnbar.ok{background:#e8f6ee;border-color:#9fd4b4;color:#0a7a3d}
 *{box-sizing:border-box}html,body{margin:0;background:var(--bg);color:var(--text);font:14px/1.45 ui-monospace,SFMono-Regular,Menlo,monospace}
 header{padding:0 16px;border-bottom:1px solid var(--border);background:var(--bg);position:sticky;top:0;z-index:20;display:flex;align-items:stretch;gap:18px;flex-wrap:wrap}
 .brand{align-self:center;white-space:nowrap}
 .hdr-right{align-self:center;margin-left:auto;display:flex;align-items:center;gap:8px;white-space:nowrap}
 header b{letter-spacing:1px}.muted{color:var(--muted)}
 main{padding:16px;display:grid;gap:16px;max-width:1240px;margin:0 auto;grid-template-columns:minmax(0,1fr)}
 main>*,.tabpane>*{min-width:0}
 .tabpane{grid-template-columns:minmax(0,1fr)}
 .panel{background:var(--panel);border:1px solid var(--border);border-radius:8px;padding:14px}
 .hdr{display:flex;justify-content:space-between;align-items:center;margin-bottom:10px;gap:8px}
 .row{display:flex;gap:12px;flex-wrap:wrap}
 .kv{background:var(--panel2);border:1px solid var(--border);border-radius:6px;padding:8px 10px;min-width:110px}
 .kv .k{color:var(--muted);font-size:11px;text-transform:uppercase}.kv .v{font-size:18px}
 .kv-sub{font-size:11px;color:var(--muted);margin-top:1px}
 .sublink{color:inherit;text-decoration:underline dotted;text-underline-offset:2px}
 .ctgrid{display:grid;grid-template-columns:repeat(auto-fill,minmax(7.4rem,1fr));gap:6px;margin-top:6px}
 .ctc{border:1px solid var(--border);border-radius:6px;padding:3px 4px;font-size:11.5px;background:var(--panel2);min-width:0}
 .ctgrid.pairs{grid-template-columns:repeat(auto-fill,minmax(9.6rem,1fr));gap:6px}
 .ctpair{border:1.5px solid var(--pair);background:var(--pairbg);border-radius:8px;padding:3px;min-width:0}
 .ctpair .ctpl{font-size:10.5px;color:var(--pair);margin:0 0 3px 2px;letter-spacing:.02em;font-weight:600}
 .ctpair .ctpi{display:grid;grid-template-columns:1fr 1fr;gap:3px}
 .ctc.down{border-color:var(--accent2)}.ctc.up{border-color:var(--ok)}
 .ctc .ctt{display:flex;justify-content:space-between;gap:3px;white-space:nowrap}
 details.ctboard,details.ctsec{border:1px solid var(--border);border-radius:8px;margin-top:8px;padding:0 10px}
 details.ctboard>summary,details.ctsec>summary{cursor:pointer;padding:8px 0;list-style-position:inside}
 details.ctboard[open],details.ctsec[open]{padding-bottom:10px}
 .ctsum{margin-left:6px}
 .ctb{display:inline-block;font-size:11px;border-radius:9px;padding:0 7px;border:1px solid var(--border)}
 .ctb.down{border-color:var(--accent2);color:var(--accent2)}.ctb.up{border-color:var(--ok);color:var(--ok)}.ctb.low{border-color:var(--warn);background:var(--warn);color:#000}
 .ctxt{font-size:12.5px;line-height:1.55}.ctxt p{margin:6px 0}.ctxt ul{margin:6px 0 6px 18px;padding:0}
 .ctc select{width:100%;margin-top:2px;font-size:11px;padding:1px 0}
 @media (pointer:coarse){.ctc select{min-height:30px;font-size:12px}}
 :root[data-theme="light"] #ctpanel{--warn:#8a5c00}:root[data-theme="light"] .ctb.low{color:#fff}
 .okpill{color:var(--ok)}.badpill{color:var(--bad)}
 .sct{border-collapse:collapse;font-size:12px;margin-top:6px;width:100%}.sct th,.sct td{padding:3px 6px 3px 0;text-align:left;vertical-align:top;border-bottom:1px solid var(--line)}
 .sct th{font-weight:600;color:var(--muted);font-size:11px;white-space:nowrap}.sct td.why{font-family:inherit;color:var(--muted);font-size:11.5px;width:100%}.sct .nw{white-space:nowrap}
 .sct .up{color:var(--ok)}.sct .bad{color:var(--warn)}
 .sctform{display:flex;flex-wrap:wrap;gap:8px 14px;align-items:center;margin-top:8px}.sctform .nw{white-space:nowrap}.sctform input[type=number]{width:4.5rem}
 .boards{display:grid;grid-template-columns:repeat(auto-fill,minmax(200px,1fr));gap:12px}
 .bd{background:var(--panel2);border:1px solid var(--border);border-radius:8px;padding:10px}.bd.dead{opacity:.45}
 .bd .t{display:flex;justify-content:space-between;align-items:center;margin-bottom:6px}
 .pill{font-size:11px;border:1px solid var(--border);border-radius:999px;padding:1px 8px;color:var(--muted)}
 .pill.ok{color:var(--ok);border-color:#1f5a3c;background:#0c2016}.pill.bad{color:var(--bad);border-color:#5a2323;background:#220c0c}.pill.warn{color:var(--warn);border-color:#5a4a1f;background:#221b09}
 :root[data-theme="light"] .pill.warn{border-color:#e2c98a;background:#fbf3de}
 .bd .line{display:flex;justify-content:space-between;font-size:12px;padding:2px 0;border-top:1px solid var(--line)}
 .cmboard{margin-bottom:16px}
 .cmhead{display:flex;flex-wrap:wrap;align-items:baseline;gap:4px 10px;margin:2px 0 7px}
 .cmhead b{font-size:13px}.cmhead .muted{font-size:11.5px}
 .cmgrid{display:grid;grid-template-columns:repeat(auto-fill,26px);gap:3px;justify-content:start}
 .cell{width:26px;height:26px;border-radius:3px;display:flex;align-items:center;justify-content:center;font-size:9px;cursor:default;user-select:none;background:var(--cell-empty);box-shadow:inset 0 0 0 1px var(--cell-edge);color:var(--muted)}
 .cell.strong{color:var(--bg)}
 .cell.ring{box-shadow:0 0 0 2px var(--text)}
 .cell.over{background:var(--over)!important;color:#fff;font-weight:700;box-shadow:none}
 .cmgrid2{display:grid;grid-template-columns:repeat(auto-fill,26px);gap:3px;justify-content:start}
 .chip{aspect-ratio:1;border:1px solid #444;border-radius:3px;display:flex;align-items:center;justify-content:center;font-size:9px;cursor:default;user-select:none}
 .chip.slow,.leg i.slow{box-shadow:inset 0 0 0 2px #5bc8ff}
 .cmtip{position:fixed;z-index:50;display:none;pointer-events:none;background:var(--panel);border:1px solid var(--border);border-radius:7px;padding:8px 10px;font-size:12px;line-height:1.5;max-width:260px;box-shadow:0 6px 24px rgba(0,0,0,.35)}
 .maplegend{display:flex;flex-wrap:wrap;gap:6px 16px;font-size:11.5px;color:var(--muted);margin-top:10px;align-items:center}
 .maplegend .sw{display:inline-block;width:13px;height:13px;border-radius:3px;vertical-align:middle;margin-right:4px}
 .maplegend .ramp{display:inline-flex;margin:0 5px;vertical-align:middle}.maplegend .ramp .sw{margin:0;border-radius:0}
 .ctable{width:100%;border-collapse:collapse;font-size:12px}
 .ctable th,.ctable td{padding:4px 8px;border-bottom:1px solid var(--border);text-align:left}
 .ctable th{color:var(--muted);font-weight:600;cursor:pointer;white-space:nowrap;position:sticky;top:0;background:var(--panel);z-index:1}
 .ctable td.num,.ctable th.num{text-align:right;font-variant-numeric:tabular-nums}
 .ctwrap{max-height:340px;overflow:auto;border:1px solid var(--border);border-radius:6px;margin-top:4px}
 .shadeby{display:flex;align-items:center;gap:7px;font-size:12px;color:var(--muted)}
 .shadeby select{width:auto;padding:4px 8px;font-size:12px}
 /* fan curve editor */
 .fcurve-wrap{border:1px solid var(--border);border-radius:8px;background:var(--panel2);padding:8px 6px 4px;margin:10px 0 6px}
 .fcurve-wrap svg{width:100%;height:auto;display:block;touch-action:none;user-select:none}
 .fpt{cursor:grab}
 .ftools{display:flex;align-items:center;gap:10px;font-size:12px;color:var(--muted);margin:2px 2px 8px;flex-wrap:wrap}
 .ftools input[type=range]{width:150px;margin:0}
 .ftools .readout{color:var(--text);font-variant-numeric:tabular-nums;font-weight:600}
 #f_copy{width:auto;padding:4px 10px;font-size:12px}
 .leg{display:flex;align-items:center;gap:6px;font-size:11px;color:var(--muted);margin-top:8px;flex-wrap:wrap}
 .leg i{width:14px;height:14px;border:1px solid #444;border-radius:3px;display:inline-block;vertical-align:middle}
 button{background:var(--btn);border:1px solid var(--border);color:var(--text);border-radius:6px;padding:6px 12px;cursor:pointer}
 button:hover{background:var(--btn-h);border-color:var(--muted)}button.active{background:var(--text);color:#000;border-color:var(--text)}
 button.primary{background:var(--text);color:#000;border-color:var(--text);font-weight:600}
 input{background:var(--panel2);border:1px solid var(--border);color:var(--text);border-radius:6px;padding:8px 10px;width:100%;margin:6px 0}
 input[type=checkbox],input[type=radio]{width:auto}
 .auth{max-width:340px;margin:60px auto}.err{color:var(--bad);font-size:12px;min-height:16px}
 #log{background:var(--logbg);border:1px solid var(--border);border-radius:6px;height:300px;overflow:auto;padding:8px;font-size:12px}
 #log .ln{white-space:pre-wrap;word-break:break-all;padding:0 2px;color:var(--logtx)}
 #log .ln.accept{color:#3dd68c;font-weight:600}#log .ln.reject{color:#ff5c5c;font-weight:600}
 #log .ln.invalid{color:#ff9f43}#log .ln.nonce{color:#f2c94c}#log .ln.best{color:#5bc8ff;font-weight:600}#log .ln.pool{color:#7aa2ff}#log .ln.err{color:var(--bad)}#log .ln.dim{color:#7c7c7c}
 :root[data-theme="light"] #log .ln.accept{color:#0a7a3d}:root[data-theme="light"] #log .ln.reject{color:#c62828}:root[data-theme="light"] #log .ln.invalid{color:#a14800}:root[data-theme="light"] #log .ln.nonce{color:#7a5b00}:root[data-theme="light"] #log .ln.best{color:#0b62a8}:root[data-theme="light"] #log .ln.pool{color:#2f55c8}:root[data-theme="light"] #log .ln.dim{color:#6b6b6b}
 #hrchart svg{width:100%;height:auto;display:block}
 .hrline{fill:none;stroke:var(--text);stroke-width:1.5;vector-effect:non-scaling-stroke}
 .hrgrid{stroke:var(--border);stroke-width:1;vector-effect:non-scaling-stroke}
 .hrlbl{fill:var(--muted);font-size:var(--hrfs,10px);font-family:inherit}
 .hrunit{fill:var(--muted);font-size:9px;font-family:inherit;opacity:.8}
 .hrtick{stroke:var(--muted);stroke-width:1;opacity:.45;vector-effect:non-scaling-stroke}
 .hrtick.maj{opacity:.8}
 .rbar{height:7px;background:var(--panel2);border:1px solid var(--border);border-radius:5px;overflow:hidden;width:100%;max-width:340px}
 .rfill{height:100%;background:var(--ok);border-radius:4px;transition:width .9s linear}
 .rbar.indet{position:relative}
 .rbar.indet .rfill{width:40%!important;position:absolute;left:0;animation:indet 1.3s ease-in-out infinite;transition:none}
 @keyframes indet{0%{left:-42%}100%{left:100%}}
 .busy{display:flex;flex-direction:column;gap:9px;max-width:560px;padding:2px 0}
 .busy .rbar{max-width:260px;height:5px}
 .stalenote{font-size:12px;color:var(--muted);margin:0 0 8px;display:flex;gap:8px;align-items:center}
 .stalenote .rbar{max-width:90px;height:4px;flex:none}
 .hrmark{stroke:var(--muted);stroke-width:1;stroke-dasharray:3 2;opacity:.5;vector-effect:non-scaling-stroke}
 .hrmark.rej{stroke:var(--bad);opacity:.8}
 .hrmlbl{fill:var(--muted);font-size:var(--hrfs,11px);font-weight:600;font-family:inherit}
 .hrmlbl.rej{fill:var(--bad)}
 .hrkl{display:inline-block;border:1px solid var(--border);border-radius:3px;padding:0 5px;color:var(--muted);font-size:11px;margin-right:2px}
 .hrkl.rej{border-color:var(--bad);color:var(--bad)}
 .rejgrid{font-size:13px;line-height:1.7}.rejgrid b{color:var(--text)}
 .rejleg{font-size:11.5px;color:var(--muted);line-height:1.6;margin-top:8px;border-top:1px solid var(--border);padding-top:8px}
 .rejleg .rk{color:var(--text);font-weight:600}.rejleg .rx{color:var(--bad);font-weight:600}
 .warnbar{background:#1c1606;border:1px solid #5a4a1a;color:var(--warn);border-radius:6px;padding:8px 10px;font-size:12px;margin-bottom:12px}
 .lockbar{display:flex;align-items:center;gap:10px;flex-wrap:wrap;background:var(--panel2);border:1px solid var(--text);border-radius:8px;padding:9px 12px;font-size:12.5px;margin-bottom:12px}
 .lockbar b{font-weight:650}.lockbar .takemanual{width:auto;padding:5px 12px;margin-left:auto;font-size:12px}
 .ocbtn{width:auto;padding:6px 12px;font-size:12px;border-color:var(--border)}
 .ocbar{display:flex;align-items:center;gap:12px;flex-wrap:wrap;background:#2a0d0d;border:1px solid #7a2a2a;color:#f3b4b4;border-radius:8px;padding:9px 12px;font-size:12.5px;line-height:1.5;margin:2px 0 12px}
 .ocbar b{color:#ffd4d4}.ocbar button{width:auto;padding:5px 12px;margin-left:auto;font-size:12px;white-space:nowrap}
 .occheck{display:flex;gap:9px;align-items:flex-start;font-size:12.5px;color:var(--text);line-height:1.5;margin-top:16px;cursor:pointer}
 .occheck input{width:auto;margin:2px 0 0;flex:0 0 auto}
 button.danger{background:#b4232a;color:#fff;border-color:#b4232a;font-weight:600}
 button.danger:hover{background:#c92a32}button.danger:disabled{opacity:.4;cursor:not-allowed}
 .ceilpanel{border:1px solid var(--border);border-radius:9px;background:var(--panel2);padding:12px 14px;margin:2px 0 12px}
 .ceilhd{display:flex;align-items:center;gap:10px;margin-bottom:8px;font-size:13px}.ceilhd .muted{font-size:11.5px}.ceilhd .ebtn{margin-left:auto}
 .ceilrow{display:grid;grid-template-columns:120px 150px 1fr;gap:10px;align-items:center;padding:4px 0;font-size:12.5px;border-top:1px solid var(--border)}
 .ceilrow .cl{color:var(--muted)}.ceilrow .cn{font-size:11.5px}
 @media (max-width:640px){.ceilrow{grid-template-columns:auto 1fr}.ceilrow .cn{grid-column:1/-1}}
 .ceillog{font-family:ui-monospace,Menlo,Consolas,monospace;font-size:11px;color:var(--muted);background:var(--logbg);border-radius:6px;padding:8px 10px;margin-top:6px;max-height:200px;overflow:auto;line-height:1.5}
 input:disabled,select:disabled,button:disabled{opacity:.45;cursor:not-allowed}
 .lockbar .takemanual:disabled{opacity:1}
 .modalwrap{position:fixed;inset:0;background:rgba(0,0,0,.6);display:flex;align-items:center;justify-content:center;z-index:100;padding:16px}
 .modalbox{background:var(--panel);border:1px solid var(--border);border-radius:12px;max-width:560px;width:100%;max-height:90vh;overflow:auto;padding:22px 24px;box-shadow:0 20px 60px rgba(0,0,0,.5)}
 .modalbox h3{margin:0 0 4px;font-size:17px}.modalbox h4{margin:16px 0 4px;font-size:13px}
 .modalbox p,.modalbox li{font-size:12.5px;color:var(--muted);line-height:1.55;margin:4px 0}
 .modalbox ul{margin:4px 0 4px 18px;padding:0}
 .modalbox .mrow{display:flex;gap:8px;justify-content:flex-end;margin-top:18px;align-items:center}
 .setgrid{display:grid;grid-template-columns:160px 1fr;gap:8px 12px;align-items:center;font-size:13px;max-width:540px}
 .setgrid label{color:var(--muted)}.setgrid .full{grid-column:1/3}
 .evlist{display:grid;grid-template-columns:repeat(auto-fill,minmax(19rem,1fr));gap:6px 18px;margin:8px 0}
 .evlist label{display:flex;gap:8px;align-items:flex-start;font-size:12.5px;line-height:1.35}.evlist input{width:auto;margin:2px 0 0;flex:none}
 .evlist .em{width:1.4em;text-align:center;flex:none}
 .ninfo{font-size:13px;line-height:1.8}.nchan{border:1px solid var(--border);border-radius:8px;padding:12px;background:var(--panel2);margin-bottom:10px}
 .ctrl{display:grid;grid-template-columns:120px auto 1fr;gap:12px;align-items:center;margin:9px 0}
 .ctrl label{color:var(--muted);font-size:12px}
 @media(max-width:640px){.ctrl{grid-template-columns:1fr;gap:5px;margin:12px 0}}
 .ctrl .val{text-align:right;font-variant-numeric:tabular-nums;font-size:13px}
 .cstep{display:flex;align-items:stretch;border:1px solid var(--border);border-radius:9px;overflow:hidden;background:var(--panel2);width:max-content}
 .cstep button{border:none;background:transparent;color:var(--text);width:34px;font-size:18px;line-height:1;cursor:pointer;padding:0;font-family:inherit}
 .cstep button:hover{background:var(--btn-h)}
 .cstep button:active{background:var(--btn)}
 .cstep input{border:none;background:transparent;width:78px;height:34px;text-align:center;font-variant-numeric:tabular-nums;font-size:14px;font-weight:600;color:var(--text);padding:0;-moz-appearance:textfield;border-left:1px solid var(--border);border-right:1px solid var(--border)}
 .cstep input:focus{outline:none;background:rgba(255,255,255,.04)}
 .cstep input::-webkit-outer-spin-button,.cstep input::-webkit-inner-spin-button{-webkit-appearance:none;margin:0}
 .cstep .u{display:flex;align-items:center;color:var(--muted);font-size:11px;padding:0 10px;border-left:1px solid var(--border)}
 .chint{font-size:11px;color:var(--muted);font-variant-numeric:tabular-nums}
 .chint b{color:var(--text);font-weight:600}
 .presets{display:flex;gap:8px;flex-wrap:wrap;align-items:center;margin-bottom:12px}
 .presets .mut{font-size:11px;color:var(--muted);width:100%}
 select{background:var(--panel2);border:1px solid var(--border);color:var(--text);border-radius:6px;padding:7px 10px}
 .fset{display:grid;grid-template-columns:140px 1fr;gap:10px;align-items:center;margin:7px 0}
 .fset input,.fset select{margin:0}.fset input[type=number]{width:110px}
 .frow{display:grid;grid-template-columns:150px 1fr;gap:10px;align-items:center;margin:9px 0}
 .frow>label{font-size:12.5px;color:var(--muted)}.frow select{width:100%;max-width:260px;margin:0}
 .fgrid{display:grid;grid-template-columns:repeat(auto-fit,minmax(150px,1fr));gap:12px 16px;margin:8px 0}
 .ff{display:flex;flex-direction:column;gap:4px}
 .ff label{font-size:11px;color:var(--muted);line-height:1.3}
 .ff input{width:100%;margin:0}
 .fsub{border:1px solid var(--border);border-radius:9px;background:var(--panel2);padding:11px 13px;margin:12px 0}
 .fsub.off{opacity:.55}
 .fsub>.swlbl{font-size:12.5px;display:flex;align-items:center;gap:8px}
 .fsec{font-size:11px;letter-spacing:.4px;text-transform:uppercase;color:var(--muted);margin:16px 0 2px}
 .pts{display:flex;flex-wrap:wrap;gap:8px 14px;margin:8px 0}
 .pt{display:flex;gap:8px;align-items:center}.pt input{width:70px;margin:0}.pt button{padding:4px 9px}
 .fstat{font-size:12px;color:var(--muted);margin-bottom:10px}.fstat b{color:var(--text)}
 .gpill{border:1px solid var(--border);border-radius:999px;padding:1px 8px;font-size:11px;color:var(--muted)}
 .gpill.abort,.gpill.throttle{color:var(--bad);border-color:#5a2323;background:#220c0c}
 .gpill.ok{color:var(--ok);border-color:#1f5a3c;background:#0c2016}
 .modecards{display:grid;grid-template-columns:1fr 1fr;gap:10px;margin:4px 0 8px}
 .modecard{border:1px solid var(--border);border-radius:8px;padding:10px;background:var(--panel2);cursor:pointer;display:flex;gap:8px;align-items:flex-start}
 .modecard input{width:auto;margin:3px 0 0;flex:0 0 auto}.modecard b{display:block;font-size:13px}.modecard small{color:var(--muted);font-size:11px;line-height:1.35;display:block;margin-top:2px}
 .modecard:has(input:checked){border-color:var(--text);background:var(--panel)}
 @media(max-width:560px){.modecards{grid-template-columns:1fr}}
 /* tuner wizard */
 .wsec{margin:18px 0 6px}
 .wsec-h{display:flex;align-items:baseline;gap:9px;margin-bottom:7px}
 .wsec-n{display:inline-flex;align-items:center;justify-content:center;width:21px;height:21px;border:1px solid var(--border);border-radius:5px;font-size:12px;font-weight:650;color:var(--muted);flex:0 0 auto}
 .wsec-t{font-size:14px;font-weight:650}.wsec-tag{font-size:11px;color:var(--muted);border:1px solid var(--border);border-radius:999px;padding:1px 8px}
 .wsec-help{font-size:11.5px;color:var(--muted);line-height:1.5;margin:6px 0}
 .rcards{display:grid;grid-template-columns:repeat(auto-fit,minmax(220px,1fr));gap:10px;margin:4px 0}
 .rcard{border:1px solid var(--border);border-radius:9px;padding:12px;background:var(--panel2);cursor:pointer;display:flex;gap:9px;align-items:flex-start}
 .rcard input{width:auto;margin:2px 0 0;flex:0 0 auto}.rcard b{display:block;font-size:13px}.rcard small{color:var(--muted);font-size:11.5px;line-height:1.4;display:block;margin-top:3px}
 .rcard:has(input:checked){border-color:var(--text);background:var(--panel);box-shadow:inset 0 0 0 1px var(--text)}
 .wgrid{display:grid;grid-template-columns:repeat(auto-fit,minmax(150px,1fr));gap:9px 14px;margin:8px 0}
 .wgrid label{display:block;font-size:12px;color:var(--muted);margin-bottom:3px}.wgrid input,.wgrid select{width:100%}
 .wchk{display:flex;gap:7px;align-items:flex-start;font-size:12.5px;margin:6px 0}.wchk input{width:auto;margin:2px 0 0}
 .runplan{width:100%;border-collapse:collapse;font-size:12.5px}
 .runplan td{padding:5px 10px;border-bottom:1px solid var(--border);vertical-align:top}
 .runplan td.k{color:var(--muted);white-space:nowrap;width:120px}.runplan td.k b{color:var(--text);font-weight:600}
 .planbox{margin:10px 0;border:1px solid var(--border);border-radius:8px;padding:10px;background:var(--panel2)}
 .planrow{font-size:12px;padding:2px 0;color:var(--text)}.planrow b.sic{display:inline-block;width:15px;text-align:center;font-weight:700}
 .pr-run{color:var(--text)}.pr-pass{color:var(--muted)}
 .st-pass{color:var(--ok)}.st-fail{color:var(--bad)}.st-run{color:var(--warn)}.st-todo,.st-skip{color:var(--muted)}
 /* dashboard (miner page parity) */
 .livekv{display:grid;grid-template-columns:repeat(auto-fit,minmax(10.5rem,1fr));gap:10px}
 .livekv .kv{min-width:0}
 @media (min-width:641px) and (max-width:1099px){.livekv{grid-template-columns:repeat(3,1fr)}}
 @media (max-width:640px){.livekv{grid-template-columns:repeat(2,1fr)}}
 .hrcap{font-size:11.5px;margin:6px 0 0;line-height:1.45}
 #hrchart svg .hrarea{fill:var(--text);opacity:.07}
 #rejsec{margin-top:13px;padding-top:11px;border-top:1px solid var(--line)}
 #rejsec h3{margin:0 0 5px;font-size:13.5px}
 .rj-line{font-size:12.5px;line-height:1.5}.rj-line b.alert{color:var(--bad)}
 #rejstrip svg{display:block;width:100%;height:auto;margin:6px 0 0}
 .hrtap{touch-action:pan-y;cursor:crosshair}
 .hrread{font-size:13px;min-height:1.4em;margin:4px 0 2px;color:var(--text)}
 .hrread:empty::before{content:"Tap or hover the chart to read the exact time.";color:var(--muted);font-size:12px}
 .hrcur{stroke:var(--text);stroke-width:1;stroke-dasharray:2 2;pointer-events:none}
 #rejstrip svg text{fill:var(--muted);font-size:12px}
 #rejstrip svg .grid,.rj-key svg .grid{stroke:var(--line);stroke-width:1}
 #rejstrip svg .stale,.rj-key svg .stale{stroke:var(--muted);stroke-width:1.6}
 #rejstrip svg .other,.rj-key svg .other{fill:var(--bad)}
 #rejstrip svg .rjlead{stroke:var(--muted);stroke-width:1;stroke-dasharray:2 2}
 #rejstrip svg .rjlead.rej{stroke:var(--bad)}
 #rejstrip svg text.rjts{fill:var(--muted);font-variant-numeric:tabular-nums}
 #rejstrip svg text.rjts.rej{fill:var(--bad);font-weight:600}
 #rejstrip svg text.otherlbl{fill:var(--bad);font-weight:700}
 .rj-key{display:flex;flex-wrap:wrap;gap:3px 16px;font-size:11.5px;color:var(--muted);margin-top:6px}
 .rj-key svg{display:inline;margin:0 5px 0 0;vertical-align:-1px}
 .cm-bh{display:grid;grid-template-columns:repeat(auto-fill,minmax(13rem,1fr));gap:6px 18px;line-height:1.4;font-size:11.5px;color:var(--muted);margin:2px 0 8px}
 .cm-bh b{color:var(--text);font-weight:600}
 .cm-how{margin-top:10px;font-size:12px}.cm-how summary{cursor:pointer}.cm-how p{margin:6px 0;max-width:62rem;line-height:1.5;color:var(--muted)}.cm-how b{color:var(--text)}
 .dash-row2{display:grid;grid-template-columns:minmax(0,1fr) minmax(0,1fr);gap:16px}
 .dash-col{display:flex;flex-direction:column;gap:16px;min-width:0}.dash-col>.panel{flex:1 1 auto}
 @media(max-width:960px){.dash-row2{grid-template-columns:minmax(0,1fr)}}
 .share-stats{display:flex;flex-wrap:wrap;gap:10px 34px;margin-top:4px}
 .share-stats>div{display:flex;flex-direction:column;gap:2px}.share-stats span.muted{font-size:11.5px}
 .share-stats b{font-size:20px;font-weight:600;line-height:1.2}.share-stats small{font-size:11px}
 #shtable td,#shtable th{padding:5px 7px}
 /* fan curves board (profiles.html parity) */
 .cv-board{display:grid;grid-template-columns:17rem 1fr;gap:16px;align-items:start}
 @media(max-width:900px){.cv-board{grid-template-columns:1fr}}
 .cv-plist{display:flex;flex-direction:column;gap:5px}
 .cv-plist h4{margin:10px 0 2px;font-size:10.5px;text-transform:uppercase;letter-spacing:.06em;color:var(--muted);font-weight:600}
 .cv-plist h4:first-child{margin-top:0}
 .cv-item{text-align:left;display:flex;flex-direction:column;gap:2px;padding:7px 10px}
 .cv-item small{color:var(--muted);font-size:10.5px}
 .cv-new{border-style:dashed}
 .cv-note{display:flex;flex-wrap:wrap;gap:8px 12px;align-items:center;margin:0 0 10px}.cv-note button{padding:5px 12px}
 .cv-item.on,.cv-item.on:hover{background:var(--sel-bg)!important;border-color:var(--text)!important;box-shadow:0 0 0 1px var(--text) inset;color:var(--text)!important}
 #cvSvg{width:100%;height:auto;display:block;touch-action:none;user-select:none;outline:none;background:var(--panel2);border:1px solid var(--border);border-radius:12px}
 #cvSvg:focus-visible{outline:2px solid var(--text);outline-offset:2px}
 .cv-ptable{display:flex;flex-wrap:wrap;gap:6px;margin:6px 0 4px}
 .cv-prow{display:flex;align-items:center;gap:4px;padding:4px 6px;border:1px solid var(--border);border-radius:10px;background:var(--panel2)}
 .cv-prow.sel{border-color:var(--text);box-shadow:0 0 0 1px var(--text) inset}
 .cv-prow input{width:3.8rem;padding:4px 6px;margin:0}.cv-prow span{font-size:11px;color:var(--muted)}.cv-prow button{padding:2px 8px}
 .af-table td,.af-table th{padding:7px 9px;vertical-align:middle}.af-table select{max-width:16rem;margin:0}
 @media(max-width:640px){.af-table thead{display:none}.af-table tr,.af-table td{display:block}.af-table td,.af-table td.num{display:grid;grid-template-columns:6.5rem minmax(0,1fr);gap:8px;text-align:left;padding:5px 2px}.af-table td::before{content:attr(data-l);color:var(--muted);font-weight:600}.af-table select{max-width:100%;min-width:0;width:100%}}
 .af-guard{margin-top:10px}.af-guard summary{cursor:pointer;font-size:12px}
 .table-wrap{overflow-x:auto}
 /* schedule (schedule.html parity) */
 .toast{position:fixed;right:18px;bottom:18px;z-index:200;max-width:420px;background:#141414;border:1px solid var(--border);color:var(--text);border-radius:10px;padding:10px 14px;font-size:12.5px;line-height:1.45;opacity:0;transform:translateY(8px);transition:opacity .2s,transform .2s;pointer-events:none;box-shadow:0 10px 40px rgba(0,0,0,.4)}
 .toast.show{opacity:1;transform:none}.toast.err{border-color:#6e2626;color:#ffb3b3}
 :root[data-theme="light"] .toast{background:#fff;box-shadow:0 10px 40px rgba(0,0,0,.15)}:root[data-theme="light"] .toast.err{border-color:#e3a1a1;color:#b42318}
 @media(max-width:640px){.toast{left:12px;right:12px;max-width:none}}
 .note{border-radius:10px;padding:8px 11px;font-size:12.5px;margin:8px 0;border:1px solid var(--border)}
 .note.bad{color:var(--bad);border-color:#5a2323;background:#220c0c}.note.warn{color:var(--warn);border-color:#5a4a1f;background:#221b09}
 :root[data-theme="light"] .note.bad{border-color:#e3a1a1;background:#fdeaea}:root[data-theme="light"] .note.warn{border-color:#e2c98a;background:#fbf3de}
 .cm-intro{font-size:12.5px;line-height:1.55;margin-bottom:4px}
 .cmf{border:1px solid var(--border);background:var(--panel2);border-radius:8px;padding:9px 11px;margin-top:8px}
 .cmf.on{border-color:#1f5a3c}:root[data-theme="light"] .cmf.on{border-color:#9fd4b4}
 .cmf-h{display:flex;align-items:center;gap:8px;flex-wrap:wrap}
 .cmf-id{font-weight:700;font-size:12px;border:1px solid var(--border);border-radius:5px;padding:0 6px;background:var(--panel)}
 .cmf-l{flex:1;min-width:12rem;font-size:13px}
 .cmf-m{font-size:11.5px;color:var(--muted);margin-top:4px;line-height:1.5;overflow-wrap:anywhere}
 .cmf-a{display:flex;gap:8px;align-items:center;flex-wrap:wrap;margin-top:7px}.cmf-a button{padding:4px 10px;font-size:12px}
 .cmf-f{margin-top:8px;border-top:1px solid var(--line);padding-top:8px;font-size:12.5px;line-height:1.5}.cmf-f input{margin:4px 0 8px}
 .sc-intro{font-size:12.5px;margin:-3px 0 10px;max-width:72rem;line-height:1.5}
 .sc-hint{color:var(--muted);font-size:11.5px;margin:6px 0 8px;line-height:1.45;max-width:72rem}.sc-hint:empty{display:none}
 .sc-minerbar{display:flex;flex-wrap:wrap;gap:8px 16px;align-items:center}
 .sc-big{font-size:16px;font-weight:600;line-height:1.3}
 .sc-switch{display:inline-flex;gap:7px;align-items:center;font-size:13.5px;cursor:pointer}.sc-switch input{width:16px;height:16px;margin:0}
 .sc-brushes{display:flex;flex-wrap:wrap;gap:6px;align-items:center;margin:3px 0 11px}
 .sc-brush{display:inline-flex;align-items:center;gap:7px;padding:5px 9px}
 .sc-brush i{width:16px;height:16px;border-radius:4px;box-shadow:inset 0 0 0 1px var(--cell-edge);flex:0 0 auto}
 .sc-brush small{color:var(--muted);font-size:10.5px}
 .sc-brush.on,.sc-brush.on:hover{background:var(--sel-bg)!important;border-color:var(--text)!important;box-shadow:0 0 0 1px var(--text) inset;color:var(--text)!important}
 .sc-brush.rsb i{background:transparent!important;box-shadow:none;color:var(--text);font-style:normal;font-weight:700;text-align:center;line-height:16px;width:auto;min-width:16px}
 .sc-gridwrap{overflow-x:auto;padding:4px 4px 6px}
 .sc-wk{display:grid;grid-template-columns:3rem 1fr;gap:4px .6rem;align-items:center;min-width:560px;position:relative}
 .sc-wk .day{font-size:12px;color:var(--muted)}.sc-wk .day.today{color:var(--text);font-weight:700}
 .sc-blocks{display:grid;grid-template-columns:repeat(48,1fr);gap:2px;touch-action:pan-y;user-select:none}
 .sc-blk{height:26px;border-radius:3px;background:var(--cell-empty);box-shadow:inset 0 0 0 1px var(--cell-edge);cursor:crosshair;display:flex;align-items:center;justify-content:center;font-size:8.5px;font-weight:600;color:var(--text);position:relative}
 .sc-blk.strong{color:var(--bg)}.sc-blk.hr{margin-left:2px}
 .sc-blk.rs{box-shadow:inset 0 0 0 2px var(--text)}
 .sc-blk.rs::after{content:"↻";position:absolute;top:-3px;right:0;font-size:11px;font-weight:700;color:var(--text);text-shadow:0 0 2px var(--bg),0 0 2px var(--bg),0 0 3px var(--bg)}
 .sc-blk:hover{outline:2px solid var(--text);outline-offset:1px;z-index:2}
 .sc-nowseg{position:absolute;width:2px;margin-left:-1px;background:var(--ok);opacity:.4;border-radius:1px;pointer-events:none;z-index:3}
 .sc-nowseg.today{opacity:1;width:3px;margin-left:-1.5px;box-shadow:0 0 0 1px var(--bg)}
 .sc-nowseg.today::before{content:"";position:absolute;left:50%;top:-4px;width:7px;height:7px;margin-left:-3.5px;border-radius:50%;background:var(--ok);box-shadow:0 0 0 1px var(--bg)}
 .sc-nowlab{position:absolute;transform:translateX(-50%);pointer-events:none;z-index:3;padding:0 4px;border-radius:4px;background:var(--ok);color:var(--bg);font-size:10px;font-weight:700;line-height:1.35;white-space:nowrap}
 .sc-baserow{display:flex;flex-wrap:wrap;gap:6px 9px;align-items:center;margin:-4px 0 11px}
 .sc-rsrow{display:flex;flex-wrap:wrap;gap:8px 13px;align-items:center}.sc-rsrow select{min-width:13rem}
 .sc-rsrow input[type=time]{width:9rem;margin:0}.sc-rsrow input[type=number]{width:5rem;margin:0}
 .sc-hrs{display:grid;grid-template-columns:repeat(48,1fr);gap:2px;font-size:10px;color:var(--muted)}.sc-hrs span{grid-column:span 6}
 @media(max-width:700px){.sc-wk{min-width:0;grid-template-columns:2.4rem 1fr}.sc-blocks,.sc-hrs{grid-template-columns:repeat(24,1fr)}.sc-hrs .pm{display:none}.sc-blk{height:20px}}
 .sc-phone{display:none}@media(max-width:700px){.sc-phone{display:block}}
 .sc-keyrow{display:flex;flex-wrap:wrap;gap:5px 16px;font-size:11.5px;color:var(--muted);margin-top:10px}
 .sc-keyrow i{display:inline-block;width:14px;height:14px;border-radius:3px;vertical-align:middle;margin-right:5px;box-shadow:inset 0 0 0 1px var(--cell-edge)}
 .sc-tip{position:fixed;z-index:60;pointer-events:none;background:var(--tip-bg);border:1px solid var(--border);border-radius:10px;padding:6px 10px;font-size:12px;display:none;box-shadow:0 8px 28px rgba(0,0,0,.4)}
 .sc-rules{display:flex;flex-direction:column;gap:7px;margin-top:6px}
 .sc-rule{display:flex;flex-wrap:wrap;gap:7px 11px;align-items:center;padding:8px 10px;border:1px solid var(--border);border-radius:10px;background:var(--panel2)}
 .sc-rule .lbl{font-size:12px;color:var(--muted)}.sc-rule input[type=time]{width:9rem;margin:0}.sc-rule select{min-width:11rem}
 .sc-days{display:inline-flex;flex-wrap:wrap;gap:3px}.sc-days button{padding:4px 7px;font-size:11.5px;min-width:2.6rem}
 .sc-days button.on{background:var(--text);color:var(--bg);border-color:var(--text)}
 @media(max-width:640px){.sc-days button{min-width:2.3rem;padding:4px 5px}}
 .sc-errs{color:var(--bad);font-size:12.5px;margin:6px 0}.sc-errs:empty{display:none}
 /* tuner start form (blake2b-asic-control parity) */
 .tintro{font-size:12.5px;margin:-2px 0 10px;line-height:1.5}
 .tsec{border-top:1px solid var(--border);padding:14px 0 6px}
 .tsec-h{display:flex;align-items:center;gap:9px;margin-bottom:9px;flex-wrap:wrap}
 .tsec-h h3{margin:0;font-size:14px}
 .tnum{width:23px;height:23px;border-radius:50%;display:inline-flex;align-items:center;justify-content:center;background:var(--panel2);border:1px solid var(--border);font-size:11.5px;font-weight:600;color:var(--text)}
 .ttag{font-size:10.5px;padding:2px 8px;border-radius:999px;border:1px solid var(--border);color:var(--muted)}
 .ttag.search{color:var(--text);border-color:#555}.ttag.confirm{color:var(--text);border-color:#555;border-style:dashed}.ttag.presets{color:var(--text);border-color:#555;border-style:dotted}
 :root[data-theme="light"] .ttag.search,:root[data-theme="light"] .ttag.confirm,:root[data-theme="light"] .ttag.presets{border-color:#999}
 :root[data-theme="light"] .tsec{border-top-color:#9e9e9e}
 .mode-cards{display:grid;grid-template-columns:1fr 1fr;gap:10px}
 .mode-cards.three{grid-template-columns:1fr 1fr 1fr}
 @media(max-width:1000px){.mode-cards.three{grid-template-columns:1fr}}
 @media(max-width:760px){.mode-cards{grid-template-columns:1fr}}
 .mode-card{display:flex;gap:10px;align-items:flex-start;padding:12px 13px;border-radius:12px;cursor:pointer;background:var(--panel2);border:1px solid var(--border)}
 .mode-card.on{border-color:var(--text);box-shadow:0 0 0 1px var(--text) inset;background:var(--sel-bg)}
 :root[data-theme="light"] .mode-card.on{background:#fff}
 .mode-card input{width:auto;margin:3px 0 0;flex:0 0 auto}
 .mode-card b{display:block;margin-bottom:4px;font-size:13px}
 .mode-card small{color:var(--muted);font-size:11.5px;line-height:1.45}
 .tform-grid{display:grid;gap:9px 14px;grid-template-columns:repeat(auto-fill,minmax(170px,1fr))}
 .tform-grid label{display:flex;flex-direction:column;gap:4px;font-size:11.5px;color:var(--muted)}
 .tform-grid input,.tform-grid select{width:100%;margin:0}
 .thint{color:var(--muted);font-size:11.5px;margin:6px 0 8px;line-height:1.5;max-width:70rem}
 .thint:empty{display:none}
 .tcheck{display:flex;gap:7px;align-items:center;font-size:13px;margin-top:8px}
 .tcheck input{width:auto;margin:0}
 .topt-row{display:flex;flex-wrap:wrap;gap:6px 18px;align-items:center;margin-top:8px}.topt-row .tcheck{margin-top:0}
 .tinline{display:flex;gap:7px;align-items:center;font-size:12.5px;color:var(--muted)}.tinline input{width:90px;margin:0}
 .treadout{color:var(--text);padding:5px 0}
 .tplan{margin:6px 0 0;padding-left:20px}.tplan li{margin:7px 0;line-height:1.5;font-size:12.5px}
 .kvline{display:grid;grid-template-columns:130px 1fr;gap:5px 12px;font-size:12.5px;margin:4px 0 10px}.kvline dt{color:var(--muted)}.kvline dd{margin:0}
 /* tuner 5-panel live view (blake2b-asic-control parity) */
 .tlegend{display:flex;flex-wrap:wrap;gap:4px 16px;font-size:11.5px;color:var(--muted);margin-top:3px}
 .tc-sum{font-size:12px;color:var(--muted);line-height:1.5;margin-bottom:8px}.tc-sum b{color:var(--text)}
 .tc-empty{font-size:12.5px;color:var(--muted);padding:16px 4px;text-align:center;line-height:1.5}
 .tc-rows{display:flex;flex-direction:column;gap:6px;margin-bottom:6px}
 .tc-row{display:grid;grid-template-columns:8.25rem 1fr 9.5rem;align-items:center;gap:10px;font-size:12px}
 .tc-l{font-family:ui-monospace,Menlo,Consolas,monospace;white-space:nowrap}
 .tc-t{position:relative;height:12px;background:var(--panel2);border-radius:6px;box-shadow:inset 0 0 0 1px var(--border)}
 .tc-f{position:absolute;left:0;top:0;bottom:0;border-radius:6px}.tc-f.ok{background:var(--ok)}.tc-f.warn{background:var(--warn)}.tc-f.bad{background:var(--bad)}
 .tc-lim{position:absolute;top:-3px;bottom:-3px;width:2px;background:var(--bad);border-radius:1px}
 .tc-v{font-variant-numeric:tabular-nums;white-space:nowrap}
 @media(max-width:640px){.tc-row{grid-template-columns:1fr auto;gap:4px 8px}.tc-t{grid-column:1/-1;order:3}}
 .restable tr.picked td{background:var(--panel2)}
 .tlegend i{display:inline-block;width:12px;height:3px;vertical-align:middle;margin-right:5px;border-radius:2px}
 .board-row{display:grid;grid-template-columns:6.2rem 1fr;gap:.7rem;align-items:center;margin:.45rem 0}
 .board-label{font-size:12.5px}.board-label .muted{display:block;font-size:11px}
 .tcells{min-width:0;display:grid;grid-template-columns:repeat(var(--cols,46),1fr);gap:3px}
 @media(max-width:1100px){.tcells{grid-template-columns:repeat(var(--half,23),1fr)}}
 @media(max-width:640px){.tcells{gap:3px;grid-template-columns:repeat(12,1fr)}.tcell{font-size:9.5px;letter-spacing:-.3px;border-radius:4px}.tcell.over{font-size:10px}.board-row{grid-template-columns:1fr;gap:.35rem;margin:.6rem 0}.board-label .muted{display:inline;margin-left:.5rem}}
 @media(max-width:360px){.tcells{grid-template-columns:repeat(10,1fr)}}
 .tcell{width:auto;height:auto;min-width:0;overflow:hidden;aspect-ratio:1;border-radius:4px;background:var(--cell-empty);box-shadow:inset 0 0 0 1px var(--cell-edge);display:flex;align-items:center;justify-content:center;font-size:10px;font-weight:600;color:var(--text);user-select:none}
 .tcell.strong{color:var(--bg)}.tcell.over{background:var(--over);color:#fff;font-size:12px;box-shadow:none}
 .tcell:hover{outline:2px solid var(--text);outline-offset:1px}
 .tcell.ring{box-shadow:0 0 0 2px var(--text)}

 .tkv{display:grid;grid-template-columns:repeat(auto-fit,minmax(150px,1fr));gap:10px}
 .tkv .kv .v{font-size:16px}
 .tbar{height:8px;background:var(--panel2);border:1px solid var(--border);border-radius:5px;overflow:hidden}
 .tbar-f{height:100%;background:var(--text);width:0;transition:width .5s linear}
 .runparams{display:grid;grid-template-columns:repeat(auto-fit,minmax(230px,1fr));gap:0 18px;margin:2px 0 12px;font-size:12.5px}
 .runparams>div{display:flex;gap:10px;padding:4px 0;border-bottom:1px solid var(--line)}
 .runparams .k{color:var(--muted);min-width:76px;flex:0 0 76px}.runparams .v{color:var(--text)}
 .rpsteps{display:flex;flex-direction:column}
 .rp-step{display:grid;grid-template-columns:18px 1fr;gap:9px;padding:6px 0;border-top:1px solid var(--line);font-size:12.5px;align-items:start}
 .rp-step:first-child{border-top:none}
 .rp-step.now{background:var(--sel-bg);border-radius:7px;padding:7px 9px;margin:2px 0}
 .rp-step .ic{text-align:center;font-weight:700}
 .rp-step .t{font-weight:600}.rp-step .t small{color:var(--muted);font-weight:400;margin-left:7px;font-size:11px}
 .rp-step .d{color:var(--muted);font-size:11.5px;margin-top:1px}.rp-step .n{color:var(--muted);font-size:11px;margin-top:2px}
 .rp-step details.d{margin-top:2px}.rp-step details.d summary{cursor:pointer;font-size:11px;list-style:none;display:inline}
 .rp-step details.d summary::-webkit-details-marker{display:none}
 .rp-step details.d summary::before{content:'▸ ';font-size:9px}.rp-step details.d[open] summary::before{content:'▾ '}
 .rp-step details.d p{margin:3px 0 0;line-height:1.5}
 .rp-items{display:grid;gap:3px;margin-top:6px}
 .rp-item{display:grid;grid-template-columns:16px 122px 1fr;gap:6px;font-size:12px;align-items:baseline}
 .rp-item>.ic{text-align:center;font-weight:700}
 .rp-item .lbl{font-variant-numeric:tabular-nums}
 .rp-item .att{color:var(--muted);overflow-wrap:anywhere}.rp-item .att b{color:var(--text);font-weight:600}
 @media (max-width:640px){.rp-item{grid-template-columns:16px 100px 1fr}}
 /* the step running now: its marker breathes slowly (calm, not a blink) */
 .st-run.ic,.rp-item .st-run,.rp-step .att .st-run{animation:rp-pulse 2.6s ease-in-out infinite}
 @keyframes rp-pulse{0%,100%{opacity:1}50%{opacity:.25}}
 @media (prefers-reduced-motion:reduce){.st-run.ic,.rp-item .st-run,.rp-step .att .st-run{animation:none}}
 .bestbig{font-size:22px;font-weight:650;letter-spacing:-.3px;color:var(--ok)}
 .preset-list{display:grid;grid-template-columns:repeat(auto-fit,minmax(200px,1fr));gap:10px;margin:12px 0 4px}
 .preset-list>div{border:1px solid var(--border);border-radius:9px;background:var(--panel2);padding:10px 12px;font-size:12px;line-height:1.5}
 .restable td{padding:6px 8px}.restable th{text-transform:uppercase;font-size:10.5px;letter-spacing:.4px}
 .res-pass{color:var(--ok)}.res-fail{color:var(--bad)}.res-base{color:var(--muted)}
 .preset-list b{font-size:12.5px}.preset-list .mono{color:var(--muted);font-size:11.5px}
 #tlog{background:var(--logbg);border:1px solid var(--border);border-radius:6px;height:300px;overflow:auto;padding:8px;font-size:11px;font-family:ui-monospace,Menlo,Consolas,monospace}
 #tlog div{white-space:pre-wrap;color:var(--logtx);padding:0 2px}
 .exprow{display:flex;flex-wrap:wrap;gap:6px;align-items:center;margin-top:12px}
 .ebtn{font-size:12px;padding:5px 11px;border:1px solid var(--border);border-radius:6px;background:var(--btn);color:var(--text);cursor:pointer;font-family:inherit}
 .ebtn:hover{background:var(--btn-h)}
 .ebtn.priv{border-color:#5a4a1f}
 #tlogpanel .hdr{cursor:pointer;user-select:none}
 #tlogpanel.collapsed #tlogwrap{display:none}
 .swlbl{font-size:12px;display:flex;align-items:center;gap:5px;margin-right:10px}
 .sched-brushes{display:flex;flex-wrap:wrap;gap:6px;align-items:center;margin-bottom:8px}
 .brush{font-size:12px;padding:5px 10px;border:1px solid var(--border);border-radius:7px;background:var(--btn);color:var(--text);cursor:pointer;display:flex;align-items:center;gap:6px;font-family:inherit}
 .brush:hover{background:var(--btn-h)}
 .brush.sel{border-color:var(--text);box-shadow:inset 0 0 0 1px var(--text)}
 .brush .sw{width:11px;height:11px;border-radius:3px;display:inline-block;flex:0 0 auto}
 .sgtimes{display:grid;grid-template-columns:repeat(8,1fr);font-size:10px;color:var(--muted);margin:6px 0 2px 44px}
 .sggridwrap{position:relative}
 .sgrid{display:grid;grid-template-columns:38px 1fr;gap:2px 6px;user-select:none;touch-action:none}
 .sglabel{font-size:11px;color:var(--muted);align-self:center;text-align:right;padding-right:2px}
 .sgrow{display:grid;grid-template-columns:repeat(48,1fr);gap:1px}
 .sgcell{height:19px;background:var(--cell-empty);border-radius:2px;cursor:pointer;position:relative;font-size:8px;line-height:19px;text-align:center;color:#06110a;font-weight:700}
 :root[data-theme="light"] .sgcell{box-shadow:inset 0 0 0 1px var(--cell-edge)}
 .sgcell.rst::after{content:"↻";position:absolute;inset:0;color:var(--text);font-size:11px;font-weight:400;line-height:19px}
 .sgnowline{position:absolute;top:-2px;bottom:-2px;width:2px;background:var(--ok);pointer-events:none;z-index:4}
 .sgnowbar{position:absolute;left:50%;transform:translateX(-50%);width:4px;background:var(--ok);border-radius:2px;z-index:5}
 .sgnowdot{position:absolute;left:50%;transform:translateX(-50%);width:7px;height:7px;border-radius:50%;background:var(--ok);z-index:6}
 .sgnowlbl{position:absolute;top:-15px;left:50%;transform:translateX(-50%);background:var(--ok);color:#06110a;font-size:10px;font-weight:700;padding:0 5px;border-radius:4px;white-space:nowrap;line-height:14px}
 .sgcell:hover{outline:1.5px solid var(--text);outline-offset:-1px;z-index:5}
 #schedtip{position:fixed;z-index:60;background:var(--panel);border:1px solid var(--border);border-radius:7px;padding:6px 10px;font-size:11.5px;color:var(--text);pointer-events:none;box-shadow:0 3px 14px rgba(0,0,0,.45);white-space:nowrap;display:none}
 #schedtip b{display:block;font-size:12px;margin-bottom:1px}
 #schedtip small{color:var(--muted)}
 .sched-leg{font-size:11px;color:var(--muted);margin-top:10px;display:flex;flex-wrap:wrap;gap:4px 10px;align-items:center}
 .sgsec{margin-top:16px;border-top:1px solid var(--border);padding-top:12px}
 .sgsec h4{margin:0 0 7px;font-size:12.5px;font-weight:600;color:var(--text);letter-spacing:.2px}
 .sgsec .hint{font-size:11.5px;color:var(--muted);margin:0 0 8px;line-height:1.5}
 .sgchg{display:flex;flex-direction:column;gap:3px;font-size:12px;max-height:220px;overflow:auto}
 .sgchg .row{display:grid;grid-template-columns:52px 54px 1fr;gap:8px;align-items:center;padding:4px 8px;border-radius:6px;background:var(--panel2)}
 .sgchg .row.rst{background:transparent;border:1px dashed var(--border)}
 .sgchg .d{color:var(--muted);font-weight:600}.sgchg .t{font-variant-numeric:tabular-nums;color:var(--text)}
 .sgchg .sw{width:11px;height:11px;border-radius:3px;display:inline-block;margin-right:7px;vertical-align:-1px}
 .sgrep{display:flex;gap:10px;align-items:center;flex-wrap:wrap;font-size:12.5px}
 .sgrep input[type=time]{background:var(--panel2);border:1px solid var(--border);color:var(--text);border-radius:6px;padding:6px 8px;font:inherit}
 .curve{display:flex;gap:6px;flex-wrap:wrap;margin:6px 0}.curve .pt2{background:var(--panel2);border:1px solid var(--border);border-radius:6px;padding:3px 8px;font-size:12px}
 nav#tabs{display:flex;gap:2px;flex-wrap:wrap;align-items:stretch}
 #tabs.hide{display:none}
 .setgrid select{min-width:0;max-width:100%;width:100%}
 .setgrid .full button{margin:2px 0}
 /* wide screens: logo left, tabs centred in the bar, account right */
 @media (min-width:1100px){
   header{display:grid;grid-template-columns:minmax(max-content,1fr) auto minmax(max-content,1fr);gap:18px}
   .brand{justify-self:start}
   nav#tabs{justify-self:center;flex-wrap:nowrap}
   nav#tabs.hide{display:none}
   .hdr-right{justify-self:end;margin-left:0}
 }
 /* mobile: keep the bar compact — logo + account on one row, tabs scroll horizontally below */
 @media (max-width:640px){
   header{gap:6px;padding:0 10px}
   .brand{order:1}
   .hdr-right{order:2}
   nav#tabs{order:3;flex:1 0 100%;flex-wrap:nowrap;overflow-x:auto;overflow-y:hidden;scrollbar-width:none}
   nav#tabs::-webkit-scrollbar{height:0}
   .tab-btn{padding:11px 12px;white-space:nowrap}
   main{padding:12px}
 }
 .tab-btn,.tab-btn:hover{background:transparent;border:none;border-bottom:2px solid transparent;border-radius:0;margin-bottom:-1px}
 .tab-btn{padding:14px 14px;color:var(--muted);cursor:pointer;font:inherit;font-size:13px;letter-spacing:.3px}
 .tab-btn:hover{color:var(--text)}
 .tab-btn.active,.tab-btn.active:hover{background:transparent;color:var(--text);border-bottom-color:var(--text)}
 .tabpane{display:grid;gap:16px}
 .poolrow{display:grid;grid-template-columns:1fr auto auto;gap:12px;align-items:center;padding:10px 12px;border:1px solid var(--border);border-radius:8px;background:var(--panel2);margin-bottom:8px}
 .pl-url{word-break:break-all;font-size:13px}.pl-stat{font-size:12px;white-space:nowrap}.pl-act{display:flex;gap:6px}
 @media(max-width:640px){.poolrow{grid-template-columns:1fr}.pl-act{justify-content:flex-start}}
 /* preset cards (profiles.html parity) */
 .p-minerbar{display:flex;flex-wrap:wrap;gap:8px 14px;align-items:center}
 .p-stat{display:inline-flex;flex-direction:column;padding:5px 11px;border-radius:10px;background:var(--panel2);border:1px solid var(--border);min-width:7rem}
 .p-stat small{color:var(--muted);font-size:10.5px}.p-stat b{font-size:13.5px}
 .pcards{display:grid;gap:11px;grid-template-columns:repeat(auto-fill,minmax(230px,1fr))}
 .pcard{border:1px solid var(--border);border-radius:12px;padding:12px 13px;background:var(--panel2);display:flex;flex-direction:column;gap:5px;min-height:11rem}
 .pcard.active{border-color:var(--text);box-shadow:0 0 0 1px var(--text) inset;background:var(--sel-bg)}
 .pcard.empty{border-style:dashed;opacity:.8}
 :root[data-theme="light"] .pcard.active{background:#fff}
 .pcard h3{margin:0;font-size:14px;display:flex;gap:8px;align-items:center;justify-content:space-between}
 .pcard .clock{font-size:20px;font-weight:600;line-height:1.2}
 .pcard .sub{font-size:12px;color:var(--muted)}
 .pcard .meta{font-size:11.5px;color:var(--muted);line-height:1.45}
 .pcard .acts{margin-top:auto;display:flex;gap:6px;flex-wrap:wrap;padding-top:4px}
 .pcard svg{width:100%;height:38px;display:block}
 .hide{display:none!important}
</style></head><body>
<header><b class="brand">SC&nbsp;LITE</b>
 <nav id="tabs" class="hide">
   <button class="tab-btn active" data-tab="dash" onclick="showTab('dash')">Dashboard</button>
   <button class="tab-btn" data-tab="controls" onclick="showTab('controls')">Controls &amp; fan</button>
   <button class="tab-btn" data-tab="presets" onclick="showTab('presets')">Presets</button>
   <button class="tab-btn" data-tab="schedule" onclick="showTab('schedule')">Schedule</button>
   <button class="tab-btn" data-tab="pools" onclick="showTab('pools')">Pools</button>
   <button class="tab-btn" data-tab="tuner" onclick="showTab('tuner')">Tuner</button>
   <button class="tab-btn" data-tab="log" onclick="showTab('log')">Log</button>
   <button class="tab-btn" data-tab="settings" onclick="showTab('settings')">Settings</button>
 </nav>
 <span class="hdr-right"><button id="themebtn" onclick="toggleTheme()" title="Toggle light/dark" style="padding:4px 9px">☾</button> <span id="hdrright"></span></span></header>

<main id="auth" class="hide"><div class="panel auth">
 <div class="hdr"><b id="authtitle">Sign in</b></div>
 <div id="loginbox"><input id="u" placeholder="username" autocomplete="username">
   <input id="p" type="password" placeholder="password" autocomplete="current-password">
   <div class="err" id="loginerr"></div>
   <button class="primary" style="width:100%" onclick="doLogin()">Sign in</button>
   <div class="muted" id="pwalgo" style="font-size:11.5px;margin-top:10px;text-align:center"><b>PBKDF2-HMAC-BLAKE2b-512</b></div></div>
 <div id="pwbox" class="hide"><p class="muted">Choose a username and password to continue. They are also your SSH login.</p>
   <input id="nu" placeholder="username" autocomplete="username">
   <input id="np" type="password" placeholder="new password (min 8)" autocomplete="new-password">
   <input id="np2" type="password" placeholder="confirm new password" autocomplete="new-password">
   <div id="mpwrap" class="hide"><p class="muted" style="margin-bottom:0">This miner’s stock web password isn’t the factory default (<b>123456789</b>), so enter it once here. It’s needed for the chip map, controls and fan.</p>
   <input id="nmp" type="password" placeholder="stock miner password" autocomplete="off"></div>
   <div class="err" id="pwerr"></div>
   <button class="primary" style="width:100%" onclick="doChange()">Save</button></div>
</div></main>

<main id="app" class="hide">
 <div class="tabpane" data-pane="dash">
   <div id="modelbanner" class="hide"></div>
   <div class="panel"><div class="hdr"><b>Live</b><button onclick="refresh()">Refresh</button></div><div class="livekv" id="ov"><div class="busy"><span class="muted">Loading…</span><div class="rbar indet"><div class="rfill"></div></div></div></div></div>
   <div class="panel" id="hrpanel"><div class="hdr"><b>Hashrate, 24 h</b><span class="muted" id="hrnote"></span></div>
     <div id="hrchart" class="hrtap"><span class="muted">collecting…</span></div><div id="hrread" class="hrread"></div>
     <p class="muted hrcap" id="hrcap"></p>
     <div id="rejsec"><h3>Rejected shares</h3><div id="rejbody" class="rj-line muted">—</div><div id="rejstrip" class="hrtap"></div><div id="rjread" class="hrread"></div>
       <div class="rj-key"><span><svg width="10" height="12"><line class="stale" x1="5" x2="5" y1="1" y2="11"/></svg>stale: found just as the pool moved to new work, and the miner caught it itself (pool timing, not the miner; a few a day is normal)</span>
         <span><svg width="12" height="12"><path class="other" d="M6 1L11 11L1 11Z"/></svg><b style="color:var(--bad)">rejected by the pool</b>: the pool refused it after it was sent. Its time is printed under it; tap the strip for the pool’s reason. Usually it still arrived too late (“job not found”, “stale”); several an hour are worth a look</span></div></div></div>
   <div class="panel" id="chipspanel"><div class="hdr"><b>Chips</b><span class="hide" id="cmnote"></span><span style="flex:1"></span>
       <span class="shadeby">Shade by <select id="cmmetric" onchange="renderChipMap()"><option value="hw">HW errors</option><option value="hwrate">HW errors / hour</option><option value="perf">Perf</option><option value="dtfs">Driver-slowed</option></select></span>
       <button onclick="refreshChips()">Refresh</button></div>
     <p class="muted" id="cmcap" style="font-size:12px;margin:2px 0 6px;line-height:1.5"></p>
     <div class="cm-bh" id="cmbh"></div>
     <div id="chips"><div class="busy"><span class="muted">Loading…</span><div class="rbar indet"><div class="rfill"></div></div></div></div>
     <div class="maplegend" id="cmleg"></div>
     <details class="cm-how"><summary>How is a chip judged?</summary>
       <p>Each chip is compared with the <b>typical chip on its own board</b> (the median), so a hot or busy board doesn’t make every chip look bad. <b>Weak</b>: it does under half the work of the typical chip, or it has at least 20 HW errors and 6 times the typical chip’s count. <b>Watch</b>: the driver has slowed it (DTFS: heat or errors), or it has at least 8 HW errors and 3 times the typical count, or it reports under 80 % of the typical chip’s work.</p>
       <p>Counts are since the boards last started, so a chip that just had a bad minute can look worse right after a restart. A weak chip is worth watching across restarts<span id="cmtuner">; the <a href="#" onclick="showTab('tuner');return false">Tuner</a> goes easier on chips that are already noisy at its baseline</span>.</p></details>
     <details style="margin-top:9px"><summary class="muted" id="cmsum" style="font-size:12px;cursor:pointer">Full chip list</summary><div id="cmtable"></div></details></div>
   <div class="dash-row2">
     <div class="dash-col"><div class="panel"><div class="hdr"><b>Boards</b><span class="muted" id="bcount"></span></div><div class="boards" id="boards"></div></div></div>
     <div class="dash-col"><div class="panel" id="sharespanel"><div class="hdr"><b>Best share</b><button onclick="sharesReset()" title="Clear the record and history (the best since restart stays)">Reset record</button></div>
       <div class="share-stats" id="shstats"><div class="busy"><span class="muted">Loading…</span><div class="rbar indet"><div class="rfill"></div></div></div></div>
       <p class="muted" style="font-size:11.5px;margin:8px 0 5px;line-height:1.45">The miner’s best share starts again from 0 every time it restarts. The panel logs every new best the moment it’s seen, with the setting it was on, so the record survives restarts (it keeps the 20 biggest and shows the top 10). Mostly luck, so it says little about a setting. Shown the way Settings → Best share numbers says.</p>
       <table class="ctable" id="shtable"><thead><tr><th class="num">#</th><th class="num">Best share</th><th>On</th><th style="text-align:right">Found</th></tr></thead><tbody id="shbody"></tbody></table></div></div>
   </div>
 </div>
 <div class="tabpane hide" data-pane="controls">
   <div class="panel" id="ctrlpanel"><div class="hdr"><b>Clock / voltage</b><span class="muted" id="ctrlnote"></span></div>
     <p class="muted sc-intro">While a preset or a running schedule is in charge of this miner, its clock and fans are <b>locked</b> here so nothing fights it. <i>Take manual control</i> unlocks them: it turns the preset off and pauses the schedule. To hand control back, apply a preset (<a href="#" onclick="showTab('presets');return false">Presets</a>) and resume the schedule (<a href="#" onclick="showTab('schedule');return false">Schedule</a>).<span class="lite-only"><br><b>PV</b> is the voltage the hash boards get: a multimeter on the supply reads PV within about 10 mV, and the miner's own reading shows 50–80 mV less. It drives power and heat. <b>mV</b> doesn't change it: measured with a multimeter and a wall meter, nothing moved whatever mV was set, live or after a restart; keep the stock gap (PV = mV + 300) anyway. <b>The supply doesn't go below about 9.0–9.1 V</b> (the board cards then read about 8.96–9.01 V): set PV lower and the boards, and the power, stay there.</span> Clocks run in 12.5 MHz steps.</p>
     <div class="warnbar">⚠ Changing clock or voltage can permanently damage the miner. <span data-box2="The clock box keeps to the range shown under it, the voltage is one of the listed steps, and anything outside them is refused, never applied.">Values are clamped to a safe range.</span> Changes apply live to the running board — no reboot.</div>
     <div id="ctrlbody"><div class="busy"><span class="muted">Loading…</span><div class="rbar indet"><div class="rfill"></div></div></div></div></div>
   <div class="panel" id="ctpanel"><div class="hdr"><b>Chip trims</b><span class="pill" id="cttag">—</span></div>
     <div id="ctbody"><div class="busy"><span class="muted">Loading…</span><div class="rbar indet"><div class="rfill"></div></div></div></div></div>
   <div class="panel" id="fanpanel"><div class="hdr"><b>Auto fan</b><span class="pill" id="fannote">off</span></div>
     <p class="muted sc-intro">This miner runs the curve picked here. Changes take effect straight away. While auto fan is on, the hottest board sets the fan speed from the curve; while it’s off, the miner’s own fan control is in charge.</p>
     <div id="fanbody"><div class="busy"><span class="muted">Loading…</span><div class="rbar indet"><div class="rfill"></div></div></div></div></div>
   <div class="panel" id="fppanel"><div class="hdr"><b>Fan pair offset</b><span class="pill" id="fpnote">off</span></div>
     <div id="fpbody"><div class="busy"><span class="muted">Loading…</span><div class="rbar indet"><div class="rfill"></div></div></div></div></div>
   <div class="panel" id="fcpanel"><div class="hdr"><b>Fan check</b><span class="pill" id="fcnote">—</span></div>
     <p class="muted sc-intro"><span data-box2="The OEM watchdog">Goldshell’s watchdog</span> checks every fan and stops mining while one looks failed. It expects the stock fans (2200 RPM at full speed) and flags a fan under 80 % of that, scaled to the fan speed (1700 RPM at 100 %): quieter replacement fans are flagged even while they work. Here you can fit its figures to this miner’s own fans, measured at boot; a fan that slows down a lot or stops is still caught.</p>
     <div id="fcbody"><div class="busy"><span class="muted">Loading…</span><div class="rbar indet"><div class="rfill"></div></div></div></div></div>
   <div class="panel" id="curvepanel"><div class="hdr"><b>Fan curves</b><span class="pill" id="cvDirty">saved</span></div>
     <p class="muted sc-intro">The curve says how fast the fans run at each board temperature (the hottest board counts). Drag a point to move it, click an empty spot to add one, double-click a point to remove it (or select it and press Delete). Built-in curves can’t be changed: press <b>Copy to edit</b>. The miner’s own fan control slowly turns the fans back down, so the panel re-sends the curve’s value every few seconds.</p>
     <div class="cv-board"><div class="cv-plist" id="cvList"></div>
       <div style="min-width:0">
         <div class="row" style="gap:8px;margin-bottom:8px;align-items:center;flex-wrap:nowrap"><input id="cvLabel" maxlength="48" style="flex:1;min-width:8rem;font-weight:600;margin:0"><span class="pill" id="cvKind">curve</span></div>
         <div id="cvNote" class="note cv-note hide"></div>
         <svg id="cvSvg" viewBox="0 0 720 340" tabindex="0" role="img" aria-label="Fan curve: fan percent against board temperature"></svg>
         <div class="ftools" style="margin:9px 0">Try a temperature <input type="range" id="cvTry" min="30" max="100" value="62"><span class="readout" id="cvTryOut">—</span></div>
         <div id="cvEdit">
           <div class="cv-ptable" id="cvPts"></div>
           <div class="row" style="gap:8px;align-items:center"><button id="cvAddPt">+ Add point</button><span class="muted" style="font-size:11.5px">2 to 8 points · 20-100 °C · fans 10-100 % · a curve can only stay level or rise</span></div>
           <div class="tform-grid" style="margin-top:12px">
             <label>Safety temperature °C<input type="number" id="cvAbort" step="1"></label>
             <label>Re-send every (s)<input type="number" id="cvCool" step="5"></label>
             <label>Check temperature every (s)<input type="number" id="cvPoll" step="1"></label>
             <label>Smoothing<select id="cvSmooth"><option value="0">Off</option><option value="1">Light</option><option value="2">Strong</option></select></label>
           </div>
           <label class="tcheck" title="This panel never turns the firmware’s temperature control off"><input type="checkbox" id="cvTc" checked disabled> Keep the miner’s own temperature control on (recommended)</label>
           <p class="thint"><b>Safety temperature:</b> at or above it (two readings in a row) the fans go to 100 %, whatever the curve says. If a board is still that hot 90 s later, or reaches 5 °C past it, the miner goes to Idle to cool down: Stock and presets wait 5 minutes (15 if it happens again within the hour; you can ask for your choice to start by itself then), and after that it stays on Idle until you apply one. This works with the curve on or off. <b>Re-send:</b> how often the value is sent again; the miner’s own control walks the fans back in between. <b>Smoothing:</b> follows the average of the last few readings so the fans don’t hunt up and down; it never lets the fan lag more than 10 % behind a rise. <b>Temperature control:</b> the miner’s own protection; this panel always leaves it on.</p>
         </div>
         <div class="sc-errs" id="cvErr"></div>
         <div class="row" style="margin-top:6px"><button class="primary" id="cvSave">Save</button><button id="cvSaveAs">Save as new</button><button id="cvCopy">Copy to edit</button><button id="cvUndo">Undo changes</button><button class="danger" id="cvDel">Delete</button><button id="cvUse">Use for auto fan</button></div>
       </div></div></div>
 </div>
 <div class="tabpane hide" data-pane="presets">
   <div class="panel">
     <div class="p-minerbar"><b style="font-size:14px">This miner</b>
       <span class="p-stat"><small>Hottest board</small><b id="p_temp">—</b></span>
       <span class="p-stat"><small>Fans</small><b id="p_fans">—</b></span>
       <span class="p-stat"><small>Auto fan</small><b id="p_auto">—</b></span>
       <span class="p-stat"><small>Preset</small><b id="p_preset">—</b></span></div>
     <p class="sc-hint" style="margin-bottom:0">Presets belong to this miner. Fan curves are on <a href="#" onclick="showTab('controls');return false">Controls &amp; fan</a>, where any preset can pick one.</p>
   </div>
   <div class="panel" id="presetPanel">
     <div class="hdr"><b>Presets</b><span class="muted" id="presetnote"></span><span style="flex:1"></span>
       <button id="btnPresetBuild" onclick="presetBuild()" title="Build High / Middle / Low / Lowest power from the last “Map every clock and voltage” tuner run">Build from tuner</button>
       <button onclick="presetNew()">+ New preset</button></div>
     <p class="muted sc-intro" id="p_intro">A preset is <span data-box2="a clock and voltage">a clock, voltage and PV</span> plus the fan curve to run them with. The tuner builds <b>High</b>, <b>Middle</b>, <b>Low</b> and <b>Lowest power</b> for this miner and tests every one with its own fan curve (tick <i>Build presets</i> on the Tuner tab). <b>Apply</b> puts the clock on the miner and switches its auto fan to that curve.</p>
     <div id="p_tuning" class="note warn hide"></div>
     <div class="pcards" id="pcards"><div class="busy"><span class="muted">Loading…</span><div class="rbar indet"><div class="rfill"></div></div></div></div>
     <div id="presetForm" class="hide" style="margin-top:16px;border-top:1px solid var(--line);padding-top:14px">
       <h3 id="pfTitle" style="margin:0 0 10px;font-size:14px">New preset</h3>
       <div class="tform-grid">
         <label>Name<input id="pfLabel" maxlength="32"></label>
         <label>MHz<input id="pfMhz" type="number" step="25"></label>
         <label>mV<input id="pfMv" type="number" step="100" oninput="pfSync()"></label>
         <label>PV<input id="pfPv" type="number" step="50"></label>
         <label style="grid-column:span 2">Fan curve<select id="pfFan"></select></label>
       </div>
       <label class="tcheck"><input type="checkbox" id="pfPvAuto" checked onchange="pfSync()"> PV follows mV (keep the gap of <span id="pfGap">+300</span>)</label>
       <div class="note warn" id="pfWarn">Clocks you type in are <b>not tested</b>. A clock or voltage the chips can’t run makes errors and can overheat the boards. Use the tuner to find safe settings; hand-set presets are marked <i>not tested</i>.</div>
       <div class="sc-errs" id="pfErr"></div>
       <div class="row" style="margin-top:9px"><button class="primary" onclick="presetFormSave()">Save preset</button><button onclick="presetFormCancel()">Cancel</button><button class="danger hide" id="btnPfDelete" onclick="presetDeleteCur()">Delete preset</button></div>
     </div>
   </div>
 </div>
 <div class="tabpane hide" data-pane="schedule">
   <div class="panel">
     <div class="sc-minerbar"><b style="font-size:14px">Schedule</b>
       <label class="sc-switch"><input type="checkbox" id="sc_on" onchange="scChanged()"> Schedule on</label>
       <span class="pill" id="sc_state">—</span></div>
     <div style="margin-top:11px"><div class="sc-big" id="sc_now">—</div>
       <div class="muted" id="sc_next" style="font-size:12.5px;margin-top:3px">—</div></div>
     <div id="sc_notes"></div>
     <p class="sc-hint" id="sc_hint">The schedule switches between this miner’s presets (the High / Middle / Low / Lowest power the tuner built, your own from Presets, the firmware’s Stock plan, or Idle). Each change puts on the preset’s <span data-box2="clock, voltage">clock, voltage, PV</span> and fan curve once; a preset you apply by hand in between stays on until the next scheduled change. Times are the miner’s local time (<span id="sc_tz">—</span>).</p>
   </div>
   <div class="panel">
     <div class="hdr"><b>This week</b><span class="muted" style="font-size:11.5px">each block = 30 minutes</span></div>
     <p class="muted sc-intro">Pick a preset, then click a block to paint it, or hold and drag to paint several (drag down to cover several days). The blocks you don’t paint follow <b>The rest of the time</b> under the brushes: nothing (after a painted stretch the miner goes back to what it ran before), or a preset (shown lighter). <b>↻ Restart</b> marks a restart at the start of a block (click a mark again, or use <b>⌫ Erase</b>, to remove it). Each block is half an hour. Nothing changes on the miner until you press <b>Save schedule</b>.</p>
     <div class="sc-brushes" id="sc_brushes"></div>
     <div class="sc-baserow" id="sc_base"></div>
     <p class="muted sc-phone" style="font-size:11.5px;margin:0 0 6px">On a phone each day takes two lines: 00:00–12:00 on the first, 12:00–24:00 on the second. Tap a block or slide sideways to paint; swiping up or down scrolls the page.</p>
     <div class="sc-gridwrap"><div class="sc-wk" id="sc_week"></div></div>
     <div class="sc-keyrow" id="sc_key"></div>
   </div>
   <div class="panel">
     <div class="hdr"><b>Restarts</b><span class="pill" id="sc_rspill">off</span></div>
     <p class="muted sc-intro">Restart this miner on a schedule (the same as Restart mining in Settings: on this firmware that reboots the miner, so hashing stops for 2–3 minutes). Mark restarts on the week above with <b>↻ Restart</b>, pick a repeat here, or both. It’s separate from the preset changes and works even without presets.<span id="sctunerskip"> A restart that comes due while the miner is being tuned is skipped.</span> Every restart shows as <b>↻</b> (a ringed block) on the week.</p>
     <div class="sc-rsrow">
       <label class="muted">Repeat <select id="sc_rsevery" onchange="scRsChange()"></select></label>
       <label class="muted sc-rs-at">at <input type="time" step="60" id="sc_rsat" value="04:00" onchange="scRsChange()"></label>
       <label class="muted sc-rs-day">on <select id="sc_rsday" onchange="scRsChange()"></select></label>
       <label class="muted sc-rs-mday">on day <input type="number" id="sc_rsmday" min="1" max="28" value="1" onchange="scRsChange()"> of the month</label>
     </div>
     <p class="sc-hint" id="sc_rsmarks"></p>
     <div id="sc_rscost"></div>
     <div class="muted" id="sc_rsnext" style="font-size:12.5px;margin-top:7px">—</div>
     <div class="row" style="margin-top:11px"><button class="primary" id="sc_save2" onclick="scSave()">Save schedule</button></div>
   </div>
   <div class="panel">
     <div class="hdr"><b>Changes</b><span class="pill" id="sc_dirty">saved</span></div>
     <p class="muted sc-intro">Each line is “at this time, on these days, switch to this preset”. The preset stays on until the next line comes round (it carries over midnight and into the next day).</p>
     <div class="row" style="gap:6px;margin-bottom:5px;align-items:center"><span class="muted" style="font-size:12.5px">Start from</span>
       <button onclick="scTpl('nights')">Quiet nights</button><button onclick="scTpl('tariff')">Cheaper night power</button><button onclick="scTpl('weekend')">Quieter weekends</button></div>
     <div class="sc-rules" id="sc_rules"></div>
     <div class="row" style="margin-top:9px"><button onclick="scAdd()">+ Add a change</button></div>
     <div class="sc-errs" id="sc_err"></div>
     <div class="row" style="margin-top:11px"><button class="primary" id="sc_save" onclick="scSave()">Save schedule</button><button id="sc_undo" onclick="scUndo()">Undo changes</button><button class="danger" onclick="scClear()">Clear preset changes</button></div>
   </div>
 </div>
 <div class="tabpane hide" data-pane="pools">
   <div class="panel"><div class="hdr"><b>Pools</b><span class="muted" id="poolnote"></span></div>
     <div class="warnbar">Pool changes restart mining briefly to apply. The first pool is the primary; the rest are failover.</div>
     <div id="poolbody"><div class="busy"><span class="muted">Loading…</span><div class="rbar indet"><div class="rfill"></div></div></div></div>
     <div id="poolguard" class="muted" style="font-size:11.5px;margin-top:10px;line-height:1.5"></div>
     <div style="margin-top:14px;border-top:1px solid var(--border);padding-top:12px">
       <div class="muted" style="font-size:12px;margin-bottom:4px">Add a pool</div>
       <input id="pl_url" placeholder="stratum+tcp://host:port  (or host:port)">
       <input id="pl_user" placeholder="wallet address . worker name">
       <input id="pl_pass" placeholder="password (usually x)">
       <button class="primary" onclick="addPool()" style="margin-top:4px">Add pool</button></div></div>
 </div>
 <div class="tabpane hide" data-pane="tuner">
   <div class="panel hide" id="curPanel"><div class="hdr"><b id="curTitle">Current step</b><button id="btnStopCur" class="danger hide" onclick="stopTuner()" style="padding:5px 12px;font-size:12px">Stop</button></div>
     <div id="tstuck" class="warnbar hide" style="margin-bottom:10px"></div>
     <div id="curEmpty" class="muted" style="font-size:12.5px">Not running. Start a run below.</div>
     <div id="curBody" class="hide">
       <div class="row" style="align-items:baseline;gap:10px;margin-bottom:9px"><span id="curPlan" style="font-size:22px;font-weight:650;letter-spacing:-.3px"></span><span class="pill" id="curPhase"></span><span class="pill" id="curVerdict"></span></div>
       <div class="tbar"><div class="tbar-f" id="curBar"></div></div><div class="muted" id="curProgress" style="font-size:11.5px;margin:4px 0 13px"></div>
       <div class="tkv">
         <div class="kv"><div class="k">Hashrate</div><div class="v" id="kvHash">—</div></div>
         <div class="kv"><div class="k">Hottest board</div><div class="v" id="kvTemp">—</div></div>
         <div class="kv"><div class="k">Closest chip to its limit</div><div class="v" id="kvChip">—</div></div>
         <div class="kv"><div class="k">New errors · all chips</div><div class="v" id="kvAll">—</div></div>
       </div>
       <div id="tchartFor" class="muted" style="font-size:12px;margin-top:13px"></div><div id="tchart" style="margin-top:4px"></div><div class="muted" id="tchartNote" style="font-size:11.5px;margin-top:3px"></div>
     </div></div>
   <div class="panel hide" id="tmapPanel"><div class="hdr"><b>Chip map</b><label class="muted" style="font-size:12px;display:flex;align-items:center;gap:6px">Showing <select id="tmapSel" onchange="tmapPick(this.value)" style="width:auto;max-width:22rem;font-size:12px;padding:3px 6px"></select></label></div>
     <div class="muted" id="tmapCap" style="font-size:11.5px;margin:2px 0 11px;line-height:1.5"></div>
     <div id="tmap"></div><div class="maplegend" id="tmapLeg"></div></div>
   <div class="panel hide" id="planPanel"><div class="hdr"><b>Run plan</b><span class="pill" id="planState">running</span></div>
     <div class="runparams" id="planParams"></div>
     <div class="rpsteps" id="planSteps"></div></div>
   <div class="panel hide" id="bestPanel"><div class="hdr"><b>Best setting</b></div><div id="bestBody"></div></div>
   <div class="panel hide" id="tresPanel"><div class="hdr"><b>Finished tests</b><span class="muted" id="tresnote"></span></div><div id="tunerresults"></div></div>
   <div class="panel" id="tunerpanel"><div class="hdr"><b>Start a tuning run</b><span class="muted" id="tunernote"></span></div>
     <div class="warnbar">⚠ Experimental. The tuner changes clock/voltage repeatedly and judges stability from live hardware errors. Start in DRY-RUN (reads chips, writes nothing) and supervise the first real run — it restores your baseline on stop or abort. Don’t change the miner from any other program or page while it runs.</div>
     <div id="tunerbody"><div class="busy"><span class="muted">Loading…</span><div class="rbar indet"><div class="rfill"></div></div></div></div></div>
   <div class="panel" id="tlogpanel">
     <div class="hdr" onclick="toggleTlog()"><b><span id="tlogcar">▾</span> Tuner log</b><span class="muted" id="tlogmeta"></span></div>
     <div id="tlogwrap">
       <div id="tlog"><span class="muted">No tuner activity yet. Start a run (or dry-run) above — the log streams here and stays after the run finishes.</span></div>
       <div class="exprow">
         <span class="muted" style="font-size:12px">Save this miner's tuner files:</span>
         <button class="ebtn" onclick="tdl('log')">Log (.txt)</button>
         <button class="ebtn" onclick="tdl('logall')">Log, every run (.txt)</button>
         <button class="ebtn" onclick="tdl('results')">Results (.csv)</button>
         <button class="ebtn" onclick="tdl('chips')">Chip data (.csv)</button>
         <button class="ebtn" onclick="tdl('readings')">Live readings (.csv)</button>
         <button class="ebtn" onclick="tdl('zip')">Everything (.zip)</button>
         <button class="ebtn priv" onclick="tdl('zippriv')">For sending, private details out (.zip)</button>
       </div>
       <div class="muted" id="tlogpath" style="font-size:12px;margin-top:9px"></div>
     </div></div>
 </div>
 <div class="tabpane hide" data-pane="log">
   <div class="panel"><div class="hdr"><b>Log</b><span>
     <button id="tab-mine" class="active" onclick="setSrc('mine')">Mining</button>
     <button id="tab-sys" onclick="setSrc('sys')">System</button>
     <button id="workonly" onclick="toggleWork()">Work only</button>
     <button id="verbose" onclick="toggleVerbose()" title="Logs every submitted share's nonce (stratum protocol-dump) — applied after a reboot">Verbose</button>
     <button id="pause" class="active" onclick="toggle()">⏸ Pause</button></span></div>
    <div class="leg" style="margin:0 0 6px"><span style="color:#3dd68c">●</span> accepted / submitted · <span style="color:#ff5c5c">●</span> rejected · <span style="color:#ff9f43">●</span> invalid / HW err · <span style="color:#f2c94c">●</span> bad nonce · <span style="color:#5bc8ff">●</span> best share · <span style="color:#7aa2ff">●</span> pool</div>
    <div class="muted" id="vhint" style="font-size:11.5px;margin:0 0 6px"><b>Verbose</b> asks the engine to log every submitted share's nonce (the <span style="font-family:ui-monospace,monospace">mining.submit</span> line). Note: this firmware's miner daemon rebuilds its config on restart, so it may not stick — accepted shares and their diff are always shown either way.</div>
    <div id="log"></div>
    <div class="row" style="margin-top:10px;gap:8px;flex-wrap:wrap;align-items:center"><span class="muted" style="font-size:12px">Save this miner’s logs:</span>
     <button onclick="logSave('mine')" title="The miner’s own log as it keeps it: its last few hundred lines">Mining log (.txt)</button>
     <button onclick="logSave('sys')" title="The system log since the last reboot (I2C probe noise left out)">System log (.txt)</button>
     <button onclick="logSave('private')" style="border-color:var(--warn)" title="Both logs with the pools’ addresses and accounts (wallet, worker), the miner’s name, IP and MAC addresses and notification secrets taken out: safe to post">For sending, private details out (.zip)</button></div></div>
 </div>
 <div class="tabpane hide" data-pane="settings">
   <div class="panel hide" id="companel"><div class="hdr"><b>Commissioning</b><span class="muted" id="comnote"></span></div>
     <div class="muted cm-intro">This miner’s control features ship <b>switched off</b>. Each one has a hardware test in the commissioning guide (C1–C9). Switch a feature on only after its test has passed on this miner. Switching off is always safe and needs no password. Flashing the SD card resets every switch to off.</div>
     <div id="combody"><div class="busy"><span class="muted">Loading…</span><div class="rbar indet"><div class="rfill"></div></div></div></div></div>
   <div class="panel"><div class="hdr"><b>Restart &amp; reboot</b></div>
     <div class="muted" style="font-size:12.5px;margin-bottom:10px;line-height:1.5"><b>Restart mining</b> asks <span data-box2="the OEM backend">Goldshell’s backend</span> to restart the miner: on this firmware that reboots it (the panel too, 1–2 min). <b>Reboot</b> reboots the whole miner directly (1–2 min).</div>
     <div class="row"><button onclick="softRestart()">↻ Restart mining</button><button onclick="fullReboot()" style="color:var(--bad)">⟳ Reboot miner</button><span class="muted" id="powmsg" style="align-self:center"></span></div>
     <label style="display:flex;gap:8px;align-items:flex-start;margin-top:10px;font-size:12.5px"><input type="checkbox" id="bdauto" onchange="setBoardAuto(this.checked)" checked> <span><b>Restart mining by itself when a hash board stops hashing</b> <span class="muted">(after 3 minutes; at most once an hour, and not during <span id="bdautotuner">the tuner, </span>the strong-chip test or a thermal Idle). A board that drops out usually stays out until mining restarts.</span></span></label></div>
   <div class="panel"><div class="hdr"><b>Recovery</b><span class="muted" id="recnote"></span></div>
     <div id="recbody"><div class="busy"><span class="muted">Loading…</span><div class="rbar indet"><div class="rfill"></div></div></div></div></div>
   <div class="panel"><div class="hdr"><b>Firmware update</b><span class="muted" id="updnote"></span></div>
     <div id="updbody"><div class="busy"><span class="muted">Loading…</span><div class="rbar indet"><div class="rfill"></div></div></div></div></div>
   <div class="panel"><div class="hdr"><b>Notifications</b><span class="muted" id="notifynote"></span></div>
     <div id="notifybody"><div class="busy"><span class="muted">Loading…</span><div class="rbar indet"><div class="rfill"></div></div></div></div></div>
   <div class="panel"><div class="hdr"><b>Network &amp; SSH</b></div>
     <div id="netbody"><div class="busy"><span class="muted">Loading…</span><div class="rbar indet"><div class="rfill"></div></div></div></div></div>
   <div class="panel"><div class="hdr"><b>Account</b><span class="muted" id="acctnote"></span></div>
     <div id="acctbody"><div class="busy"><span class="muted">Loading…</span><div class="rbar indet"><div class="rfill"></div></div></div></div></div>
   <div class="panel"><div class="hdr"><b>Power &amp; calibration</b></div>
     <div id="powerbody"><div class="busy"><span class="muted">Loading…</span><div class="rbar indet"><div class="rfill"></div></div></div></div></div>
   <div class="panel"><div class="hdr"><b>Time</b></div>
     <div id="timebody"><div class="busy"><span class="muted">Loading…</span><div class="rbar indet"><div class="rfill"></div></div></div></div></div>
   <div class="panel"><div class="hdr"><b>Best share numbers</b><span class="muted" id="dispnote"></span></div>
     <div id="dispbody"><div class="busy"><span class="muted">Loading…</span><div class="rbar indet"><div class="rfill"></div></div></div></div></div>
   <div class="panel"><div class="hdr"><b>Safety</b></div>
     <div class="row"><button onclick="showDisclaimer(window._ackdate||'')">View the risk disclaimer</button>
       <span class="muted" id="ackinfo" style="align-self:center"></span></div></div>
   <div class="panel"><div class="hdr"><b>Support this project</b><span class="muted" style="font-size:12px">optional</span></div>
     <div class="muted" style="font-size:12.5px;line-height:1.6">This firmware is free and always will be, made in spare time and shared with everyone. If it’s been useful to you and you’d like to say thanks, a small Bitcoin tip is very much appreciated, but never expected. Thank you for using it, and happy mining!</div>
     <div class="row" style="margin-top:10px;gap:8px;align-items:center;flex-wrap:wrap"><span class="muted" style="font-size:12px">Bitcoin</span>
       <span class="mono" id="donaddr" style="font-size:12.5px;word-break:break-all">bc1qrrw3jj6f2n3xs3gm93z93mtrz0qde0ztnc0mr3</span>
       <button onclick="copyDonation()" style="padding:4px 10px;font-size:12px">Copy</button></div>
     <div class="muted" style="font-size:11px;margin-top:6px">Just an address to copy: the miner never mines to it or sends it anything.</div></div>
 </div>
</main>
<div class="toast" id="toast"></div><div class="sc-tip" id="sc_tip"></div>
<div id="discmodal" class="modalwrap hide"><div class="modalbox" id="discbox"></div></div>
<div id="ocmodal" class="modalwrap hide"><div class="modalbox" id="ocbox"></div></div>
<div id="pwmodal" class="modalwrap hide"><div class="modalbox" id="pwmbox"></div></div>

<script>
const $=id=>document.getElementById(id),show=(id,on)=>$(id).classList.toggle('hide',!on);
(function(){try{const t=localStorage.getItem('sclite-theme');if(t)document.documentElement.setAttribute('data-theme',t);}catch(e){}})();
function showTab(name){{const tb=document.querySelector('.tab-btn[data-tab="'+name+'"]');if(tb&&tb.classList.contains('hide')&&name!=='dash')name='dash';} // a hidden tab (its feature is off) never opens: the dashboard instead
 const changed=window._curTab!==undefined&&window._curTab!==name;window._curTab=name;
 document.querySelectorAll('.tab-btn').forEach(b=>b.classList.toggle('active',b.dataset.tab===name));
 {const n=$('tabs'),ab=n&&n.querySelector('.tab-btn.active'); // a phone's tab bar scrolls: bring the open tab into view
  if(ab&&n.scrollWidth>n.clientWidth){const nr=n.getBoundingClientRect(),br=ab.getBoundingClientRect();if(br.left<nr.left||br.right>nr.right)n.scrollLeft+=br.left-nr.left-(n.clientWidth-br.width)/2;}}
 if(changed)window.scrollTo(0,0); // a new tab opens at its top, not at the last tab's scroll position
 document.querySelectorAll('.tabpane').forEach(p=>p.classList.toggle('hide',p.dataset.pane!==name));
 try{localStorage.setItem('sclite-tab',name);}catch(e){}
 // reload the tab's data on switch so the lock state (and live values) are fresh
 if(name==='settings')loadSettings();
 else if(name==='controls'){loadControl();loadFan();loadFanPair();loadFanCheck();loadChipTrim();}
 else if(name==='presets')loadPresets();
 else if(name==='schedule')loadSchedule();
 else if(name==='tuner')loadTuner();
 else if(name==='pools')loadPools();}
function copyDonation(){const t=$('donaddr').textContent.trim();
 const ok=()=>toast('Address copied. Thank you!'),fb=()=>{const r=document.createRange();r.selectNodeContents($('donaddr'));const sel=getSelection();sel.removeAllRanges();sel.addRange(r);
  try{document.execCommand('copy');ok();}catch(e){toast('Select the address and copy it',true);}};
 try{navigator.clipboard?navigator.clipboard.writeText(t).then(ok,fb):fb();}catch(e){fb();}}
function isDark(){return document.documentElement.getAttribute('data-theme')!=='light';}
function updateThemeBtn(){const b=$('themebtn');if(b)b.textContent=isDark()?'☾':'☀';}
function toggleTheme(){const nt=isDark()?'light':'dark';document.documentElement.setAttribute('data-theme',nt);
 try{localStorage.setItem('sclite-theme',nt);}catch(e){}updateThemeBtn();if(typeof refreshChips==='function')refreshChips();}
updateThemeBtn();
const f1=x=>x==null?'–':(Math.round(x*100)/100);
const pct2=x=>(x>0&&x<0.005)?'<0.01':f1(x); // a share in %: errors there but under 0.005 % aren't shown as 0
async function jpost(u,b){let r;try{r=await fetch(u,{method:'POST',headers:{'Content-Type':'application/json','X-Requested-With':'panel'},body:JSON.stringify(b||{})});}
 catch(e){return{s:0,j:{ok:false,err:'Couldn’t reach the miner (it may be restarting, or the network dropped). Try again in a minute.'}};}
 return{s:r.status,j:await r.json().catch(()=>({ok:false,err:'The miner sent an unexpected answer (HTTP '+r.status+'). Try again in a minute.'}))};}
// busyHTML: a calm "working on it" state with a moving bar, for data the miner can't give yet
function busyHTML(msg,detail){return '<div class="busy"><span class="muted"'+(detail?' title="'+hesc(detail)+'"':'')+'>'+hesc(msg||'Loading…')+'</span><div class="rbar indet"><div class="rfill"></div></div></div>';}
const OFFLINE='Can’t reach the miner just now (it may be restarting, or the network dropped). Reconnecting…';
// retryLater: try a loader again in a few seconds (one timer per loader)
function retryLater(fn,ms){clearTimeout(fn._rt);fn._rt=setTimeout(fn,ms||8000);}
// fetchTO: fetch options that give up after ms (a dead connection otherwise hangs for minutes and the page shows old figures as live)
function fetchTO(ms){try{if(window.AbortSignal&&AbortSignal.timeout)return {signal:AbortSignal.timeout(ms)};}catch(e){}return {};}

// ---- model profile (/api/model) ----------------------------------------------------------------
// Every hardware-specific figure and every control feature is driven from the active model profile so
// nothing hard-codes SC Lite values. The profile is fetched once at start (window._MODEL). On the SC
// Lite profile — and before the profile has loaded — isOtherModel() is false and nothing below changes,
// so the SC Lite UI is reproduced byte-for-byte. Any other model gets its own name (from the profile),
// its own temperature limits, a monitoring-mode banner, and every control feature whose Feat gate is
// off is disabled in place with a clear note.
function MODEL(){return window._MODEL||null;}
// PUB: the public release (variant_public.go): no Commissioning section, no commissioning banner, no test-only helpers
function PUB(){const m=MODEL();return !!(m&&m.public);}
function modelName(){const m=MODEL();return (m&&m.name)||'SC Lite';}
function F(){const m=MODEL();return (m&&m.features)||{};}
function isOtherModel(){const m=MODEL();return !!(m&&m.id&&m.id!=='sclite');}
// OEM: the miner's manufacturer in text people read (/api/model "oem", or the Box II plan): never named on the Box II ("OEM"),
// "Goldshell" on the SC Lite (its text unchanged). oemOf: "Goldshell’s watchdog" / "the OEM watchdog";
// oemPoss: "Goldshell’s figures" / "the OEM’s figures"; the capitalised forms start a sentence; oemFw: who
// cuts the boards off ("Goldshell" / "the OEM firmware").
function OEM(){const m=MODEL();return (m&&m.oem)||(m&&m.plan==='box2v'?'OEM':'Goldshell');}
function oemLite(){return OEM()==='Goldshell';}
function oemOf(){return oemLite()?'Goldshell’s':'the '+OEM();}
function OemOf(){return oemLite()?'Goldshell’s':'The '+OEM();}
function oemPoss(){return oemLite()?'Goldshell’s':'the '+OEM()+'’s';}
function OemPoss(){return oemLite()?'Goldshell’s':'The '+OEM()+'’s';}
function oemFw(){return oemLite()?'Goldshell':'the '+OEM()+' firmware';}
function OemFw(){return oemLite()?'Goldshell':'The '+OEM()+' firmware';}
// featOff2: true only once a non-SC-Lite profile is loaded AND its gate is off. SC Lite (all gates on)
// and the pre-load window both return false everywhere, so SC Lite behaviour is unchanged.
function featOff2(on){return isOtherModel()&&!on;}
// B2T: the tuner form speaks trim codes (no PV, no +300 gap) on a planBox2V model. TGAP: the PV gap the form adds.
function B2T(){const m=MODEL();return !!(m&&m.plan==='box2v');}
function TGAP(){return B2T()?0:300;}
// b2Name: the name a person sees for a preset key. On Box II Goldshell's two stock modes ("Hashrate" /
// "Low-power": the keys the API, the schedule's rules and the charge keep) read "Stock High" / "Stock Low", and
// "Stock" (the factory default: the same plan as Hashrate) reads "Stock High" (Go: presetLabel). Unchanged elsewhere.
function b2Name(n){if(!B2T())return n;const l=String(n==null?'':n).toLowerCase();return (l==='hashrate'||l==='stock')?'Stock High':l==='low-power'?'Stock Low':n;}
// b2Stock: a Box II stock mode's key (its card is Goldshell's plan, not a preset someone set or tested)
function b2Stock(n){return B2T()&&/^(hashrate|low-power)$/i.test(String(n==null?'':n));}
// chargeTxt: an in_charge value ("preset Hashrate", "stock settings": compared as is) as a person reads it (Go: chargeText)
function chargeTxt(lk){if(!B2T()||!lk)return lk;if(lk==='stock settings')return 'Stock High';const m=/^preset (.*)$/.exec(lk);return m?('preset '+b2Name(m[1])):lk;}
// model temperature limits used by still-visible panels (fan curve safety). SC Lite = 90/92 exactly.
function MABORT(){const m=MODEL();return (m&&m.abort_max!=null)?m.abort_max:90;}
function MCUTOFF(){const m=MODEL();return (m&&m.temp_cutoff!=null)?m.temp_cutoff:92;}
function featNoteHTML(label){const nm=modelName();
 if(PUB())return '<div class="note" style="margin:4px 0;font-size:12.5px;line-height:1.55"><b>'+hesc(label)+' isn’t in this release yet.</b> Monitoring, the pools and the stock power plans work as usual.</div>';
 return '<div class="note" style="margin:4px 0;font-size:12.5px;line-height:1.55"><b>'+hesc(label)+' is not available on this model ('+hesc(nm)+').</b> It writes to the mining hardware and stays off until it has been tested on real hardware. Monitoring, the pools and the two stock power plans work as usual.</div>';}
// featBlock: when a feature is off for this model, fill its panel body with the note and return true so
// the loader stops before fetching/rendering the real control.
function featBlock(bodyId,on,label){if(!featOff2(on))return false;const el=$(bodyId);if(el)el.innerHTML=featNoteHTML(label);return true;}
async function loadModel(){try{const r=await fetch('/api/model');if(r.ok){const m=await r.json();if(m&&m.ok)window._MODEL=m;}}catch(e){}try{applyModel();}catch(e){}}
function applyModel(){const m=MODEL();if(!isOtherModel())return; // SC Lite / not-yet-loaded: leave the stock UI exactly as it is
 try{document.title=m.name;}catch(e){}
 const br=document.querySelector('header .brand');if(br)br.textContent=m.name;
 // the whole Tuner tab is a control feature: hide the tab button when it is off
 if(featOff2(F().Tuner)){const tb=document.querySelector('.tab-btn[data-tab="tuner"]');if(tb)tb.classList.add('hide');
  if(window._curTab==='tuner')showTab('dash');
  ['cmtuner','bdautotuner','sctunerskip'].forEach(id=>{const e=$(id);if(e)e.classList.add('hide');});} // no fixed text points at the hidden tab
 // the gated panels' loaders put a note in the body; also hide their static SC-Lite intros so no
 // hard-coded figure (PV/12.5 MHz steps, 2200/1700 RPM) leaks above the note on this model.
 const hideIn=(pid,sel)=>{const p=$(pid);if(p)p.querySelectorAll(sel).forEach(e=>e.classList.add('hide'));};
 if(featOff2(F().ClockControl)){hideIn('ctrlpanel','.sc-intro');hideIn('ctrlpanel','.warnbar');}
 if(featOff2(F().FanFit))hideIn('fcpanel','.sc-intro');
 // chip trims not commissioned on this unit: the whole Chip trims card is hidden (its loader still stops at featBlock)
 if(featOff2(F().ChipTrims)){const p=$('ctpanel');if(p)p.classList.add('hide');}
 // fan-pair offset and fitting the fan check are SC Lite hardware features (this model's fans share one channel and
 // keep the stock fan check): when off, hide their cards instead of promising them
 if(featOff2(F().FanPairOffset)){const p=$('fppanel');if(p)p.classList.add('hide');}
 if(featOff2(F().FanFit)){const p=$('fcpanel');if(p)p.classList.add('hide');}
 // static SC-Lite-only wording (PV, mV, the 9.1–9.6 V supply): hidden on any other model; a [data-box2]
 // span carries the Box II wording for the same place (the SC Lite page shows its own text unchanged)
 document.querySelectorAll('.lite-only').forEach(e=>e.classList.add('hide'));
 if(B2T())document.querySelectorAll('[data-box2]').forEach(e=>{e.textContent=e.getAttribute('data-box2');});
 if(PUB()&&featOff2(F().Tuner))pubNoTuner();
 if(PUB()){const cp=$('companel');if(cp)cp.innerHTML='';} // the public release: no commissioning text in the page at all
 b2Folds();
 renderModelBanner(m);}
// pubNoTuner: the public release has no tuner on this model: the page's fixed texts stop pointing at it (the
// tuner-built High / Middle / Low / Lowest power, "Build from tuner", "tested" / "not tested" tags)
function pubNoTuner(){const hide=id=>{const e=$(id);if(e&&e.classList)e.classList.add('hide');};hide('btnPresetBuild');
 const pi=$('p_intro');if(pi)pi.innerHTML='A preset is '+(B2T()?'a clock and voltage':'a clock, voltage and PV')+' plus the fan curve to run them with. <b>Apply</b> puts the clock on the miner and switches its auto fan to that curve.';
 const pw=$('pfWarn');if(pw)pw.innerHTML='Clocks you type in haven’t been tried on this miner. A clock or voltage the chips can’t run makes errors and can overheat the boards.';
 const sh=$('sc_hint');if(sh)sh.innerHTML=String(sh.innerHTML||'').replace(/\(the High[^)]*\)/,B2T()?'(Stock High, Stock Low, your own from Presets, or Idle)':'(your own from Presets, the firmware’s Stock plan, or Idle)');}
// renderModelBanner: the "monitoring mode" note shown on the Dashboard for any non-SC-Lite model — what
// works, what is off, and the thermal backstop (the profile's own cut-off; the stop/thermal note per B3).
function renderModelBanner(m){const el=$('modelbanner');if(!el)return;
 if(!isOtherModel()||PUB()){el.classList.add('hide');el.innerHTML='';return;}
 const cut=MCUTOFF(),f=F();
 // the commissioning switches (Settings → Commissioning): what this unit has proven and switched on
 const all=[['SoftIdle','stop hashing from the panel'],['PowerEstimate','the power estimate'],['ClockControl','clock control'],['VoltageControl','voltage control'],['ChipTrims','chip trims'],['CeilingProbe','the ceiling probe'],['OverclockUnlock','the overclock unlock'],['Tuner','the tuner'],['FirmwareUpdate','in-panel firmware update']];
 const on=all.filter(x=>f[x[0]]).map(x=>x[1]),off=all.filter(x=>!f[x[0]]).map(x=>x[1]);
 const pill=on.length?(on.length+' of '+all.length+' on'):'control off';
 const idle=f.SoftIdle
  ?'The panel’s thermal and pool guards stop hashing themselves (the board is disabled, as the OEM’s own overheat stop does); the OEM’s cut-off at <b>'+hesc(cut)+' °C</b> stays as the backstop.'
  :'For heat, the OEM’s own cut-off at <b>'+hesc(cut)+' °C</b> stays as the backstop: until “stop hashing from the panel” is switched on, the panel’s thermal and pool guards fall back to that cut-off rather than stopping the miner themselves.';
 el.innerHTML='<div class="panel" style="border-left:3px solid var(--warn)"><div class="hdr"><b>'+hesc(m.name)+' — '+(on.length?'commissioning':'monitoring mode')+'</b><span class="pill">'+hesc(pill)+'</span></div>'
  +'<div class="muted" style="font-size:12.5px;line-height:1.6"><b>Monitoring and pools work normally</b> on this miner: live hashrate, chips, boards, best share, temperatures, the pool list and its two stock power plans.'
  +imgMore(off.length?'Still off on this unit, and the heat backstop':'Switched on for this unit, and the heat backstop',
   (on.length?'<b>Switched on</b> for this unit: '+hesc(on.join(', '))+'. ':'')
   +(off.length?'<b>Still off</b> until tested on this unit (Settings → Commissioning): '+hesc(off.join(', '))+'. ':'')
   +idle,off.length?'you':'')+'</div></div>';
 el.classList.remove('hide');}

async function boot(){let m;try{const r=await fetch('/api/me');m=await r.json();pwAlgo(m.pw_algo,m.pw_upgrade_waits);}catch(e){if($('app')&&$('app').classList.contains('hide')){showAuth('login');$('authtitle').textContent=OFFLINE;}retryLater(boot,5000);return;}
 window._me=m;window._ackdate=m.acknowledged||'';window._needMP=!m.miner_pass_set;
 if(!m.authenticated){showAuth('login');return;}
 if(m.must_change){showAuth('change');$('nu').value=m.username||'admin';return;}
 if(!m.acknowledged){show('auth',false);show('app',false);showDisclaimer('');return;}
 syncTime();startApp(m);}
// The miner's time zone follows the browser the first time someone signs in (nothing chosen yet), and an
// unset clock (no time server reachable) is set from the browser. Both quietly; Settings → Time changes it.
function browserZone(){try{return Intl.DateTimeFormat().resolvedOptions().timeZone||'';}catch(e){return '';}}
async function syncTime(){try{const r=await fetch('/api/time');if(!r.ok)return;const t=await r.json();
 const z=browserZone();
 if(!t.zone_set&&z){const {j}=await jpost('/api/time',{zone:z,by:'browser'});if(j.ok&&j.zone_set&&j.zone===z)toast('Time zone set to '+z+' (from this browser). Change it in Settings → Time.');}
 if(!(t.plausible||t.synced)){const {j}=await jpost('/api/time',{clock:Math.floor(Date.now()/1000)});if(j.ok)toast('Clock set from this browser (the miner hasn’t reached a time server yet).');}
}catch(e){}}
// Settings → Recovery (recovery.go): hardware watchdog and the kept system log, both off by default
async function loadRecovery(){try{const r=await fetch('/api/recovery');renderRecovery(await r.json());}catch(e){$('recbody').innerHTML=busyHTML(OFFLINE);retryLater(loadRecovery);}}
function renderRecovery(v){if(!v||!v.ok){$('recbody').innerHTML='<span class="muted">'+hesc((v&&v.err)||'unavailable')+'</span>';return;}
 $('recnote').textContent=(v.watchdog_running?'watchdog on':'watchdog off')+' · '+(v.keep_log?'log kept':'log not kept');
 if(isOtherModel()){renderRecoveryBox2(v);return;}
 let h='<div class="muted" style="font-size:12.5px;line-height:1.5;margin-bottom:8px">If the control board itself freezes (every program stops, the miner drops off the network), nothing in Goldshell’s firmware brings it back: it stays dead until the power is cut. The hardware watchdog that could fix that can’t be used on this board; keeping the system log at least shows afterwards what happened.</div>';
 h+='<label style="display:flex;gap:7px;align-items:center;font-size:13px;opacity:.55"><input type="checkbox" id="recwdt" style="width:auto;margin:0" disabled> <span><b>Reboot by itself if the control board freezes</b> (hardware watchdog): not available</span></label>'
  +'<div class="muted" style="font-size:12px;margin:2px 0 8px 24px;line-height:1.5">This board’s kernel driver for the watchdog chip locks up a processor core the moment it’s switched on (found by taking the kernel apart; it’s what stopped the panel and SSH on v1.5.12). So it stays off, and the panel never touches it.</div>';
 h+='<label style="display:flex;gap:7px;align-items:center;font-size:13px"><input type="checkbox" id="reclog" style="width:auto;margin:0"'+(v.keep_log?' checked':'')+' onchange="recSet({keep_log:this.checked})"> <span><b>Keep the system log across restarts</b></span></label>'
  +'<div class="muted" style="font-size:12px;margin:2px 0 6px 24px;line-height:1.5">The system log (the kernel’s messages) lives in memory and is lost at every restart, so a freeze takes its last messages with it. With this on, its last 48 KB are saved every 2 minutes (only when they changed) and the private log export (Log → “For sending”) includes them as “system log before the last restart”.'+(v.kept_before?(' Saved copy from '+hesc(fmtWhen(v.kept_before))+'.'):'')+'</div>';
 if(v.unexpected)h+='<div style="font-size:12.5px;margin-top:6px">'+unexpText(v.unexpected)+'</div>';
 $('recbody').innerHTML=h;}
// renderRecoveryBox2: the same switches and words, the explanations folded
function renderRecoveryBox2(v){
 let h='<div class="muted" style="font-size:12.5px;line-height:1.5;margin-bottom:8px">If the control board itself freezes (every program stops, the miner drops off the network), nothing in the OEM firmware brings it back: it stays dead until the power is cut.'
  +imgMore('Why there’s no watchdog','The hardware watchdog that could fix that can’t be used on this board; keeping the system log at least shows afterwards what happened.<br>This board’s kernel driver for the watchdog chip locks up a processor core the moment it’s switched on (found by taking the kernel apart; it’s what stopped the panel and SSH on v1.5.12). So it stays off, and the panel never touches it.')+'</div>';
 h+='<label style="display:flex;gap:7px;align-items:center;font-size:13px;opacity:.55"><input type="checkbox" id="recwdt" style="width:auto;margin:0" disabled> <span><b>Reboot by itself if the control board freezes</b> (hardware watchdog): not available</span></label>';
 h+='<label style="display:flex;gap:7px;align-items:center;font-size:13px;margin-top:8px"><input type="checkbox" id="reclog" style="width:auto;margin:0"'+(v.keep_log?' checked':'')+' onchange="recSet({keep_log:this.checked})"> <span><b>Keep the system log across restarts</b></span></label>'
  +'<div class="muted" style="font-size:12px;margin:2px 0 6px 24px;line-height:1.5">'+(v.kept_before?('Saved copy from '+hesc(fmtWhen(v.kept_before))+'.'):'')
  +imgMore('What it keeps','The system log (the kernel’s messages) lives in memory and is lost at every restart, so a freeze takes its last messages with it. With this on, its last 48 KB are saved every 2 minutes (only when they changed) and the private log export (Log → “For sending”) includes them as “system log before the last restart”.')+'</div>';
 if(v.unexpected)h+='<div style="font-size:12.5px;margin-top:6px">'+unexpText(v.unexpected)+'</div>';
 $('recbody').innerHTML=h;}
async function recSet(o){if(o&&o.watchdog===true&&!confirm('Turn on the hardware watchdog? It is experimental: the board resets itself if the panel stops answering for a while.')){loadRecovery();return;}const {j}=await jpost('/api/recovery',o);if(j&&!j.ok)toast(j.err||'failed',true);loadRecovery();}
// Settings → Commissioning (commission.go): the per-unit switches for the control features a model ships with
// off until its hardware test has passed on this miner. Shown only when /api/commission says there is
// something to commission; on the SC Lite (every feature on in its profile) it isn't even asked for.
async function loadCommission(){const p=$('companel');if(!p)return;if(PUB())p.innerHTML=''; // (public: not even hidden text)
 if(!isOtherModel()||PUB()){p.classList.add('hide');return;}
 if(window._comBusy)return; // a switch is being applied: the restart wait redraws the card
 try{const r=await fetch('/api/commission',fetchTO(15000));if(r.status===401){boot();return;}renderCommission(await r.json());}
 catch(e){if(!p.classList.contains('hide'))$('combody').innerHTML=busyHTML(OFFLINE);retryLater(loadCommission);}}
function comTestId(f){return (String(f.test||'').match(/^C\d+/)||[''])[0];}
function comWhen(at){const d=at?new Date(at):null;return d&&!isNaN(d)?d.toLocaleString():'';}
function renderCommission(v){const p=$('companel');if(!p)return;if(!v||!v.ok||!v.available){p.classList.add('hide');return;}
 p.classList.remove('hide');window._com=v;const fs=v.features||[],by={};fs.forEach(f=>by[f.key]=f);
 const lab=k=>by[k]?by[k].label:k,pending=fs.some(f=>!!f.saved!==!!f.running);
 $('comnote').textContent=pending?'restarting the panel…':(fs.filter(f=>f.running).length+' of '+fs.length+' on');
 let h=pending?'<div class="note warn">⚠ What is saved differs from what the panel is running: it is restarting to apply the change (about 5 s). If this stays, reload the page; if it still stays, reboot the miner.</div>':'';
 fs.forEach(f=>{const id=comTestId(f),req=f.requires||[],miss=req.filter(k=>!(by[k]&&by[k].saved)),k=hesc(f.key);
  const st=f.saved&&f.running?'<span class="pill ok">on</span>':!f.saved&&!f.running?'<span class="pill">off</span>':'<span class="pill warn">'+(f.saved?'on after the restart':'off after the restart')+'</span>';
  let m='Test: <b>'+hesc(f.test||'–')+'</b>'+(req.length?' · needs '+req.map(r=>hesc(lab(r))+((by[r]&&by[r].saved)?' ✓':'')).join(' and '):'');
  if(f.saved&&f.at)m+='<br>Switched on '+hesc(comWhen(f.at));
  if(f.note)m+='<br>Note: “'+hesc(f.note)+'”';
  let a;
  if(f.saved)a='<button onclick="comOff(\''+k+'\')">Switch off</button>';
  else if(miss.length)a='<button disabled>Switch on…</button><span class="muted" style="font-size:11.5px">Switch on '+miss.map(r=>hesc(lab(r))).join(' and ')+' first.</span>';
  else a='<button onclick="comOpen(\''+k+'\')">Switch on…</button>';
  h+='<div class="cmf'+(f.running?' on':'')+'" id="cmf_'+k+'"><div class="cmf-h">'+(id?'<span class="cmf-id">'+hesc(id)+'</span>':'')+'<span class="cmf-l">'+hesc(f.label)+'</span>'+st+'</div>'
   +'<div class="cmf-m">'+m+'</div><div class="cmf-a">'+a+'</div><div class="cmf-f hide" id="cmff_'+k+'"></div></div>';});
 $('combody').innerHTML=h;if(pending)retryLater(loadCommission,4000);}
// comOpen: the switch-on form: the risk in one sentence, an optional note, then the password (askPassword)
function comOpen(key){const f=((window._com||{}).features||[]).find(x=>x.key===key);const el=$('cmff_'+key);if(!f||!el)return;
 document.querySelectorAll('#combody .cmf-f').forEach(x=>{if(x!==el){x.classList.add('hide');x.innerHTML='';}});
 document.querySelectorAll('#combody .cmf-a').forEach(x=>x.classList.toggle('hide',x===el.previousElementSibling)); // its own button: the form replaces it
 const id=comTestId(f)||'its';
 el.innerHTML='<div><b>Switch on only if test '+hesc(id)+' passed on this miner:</b> once on, the panel changes the mining hardware this way, and on a miner where the test didn’t pass that can make it unstable, stop it hashing or damage it.</div>'
  +'<label class="muted" for="cmfn_'+hesc(key)+'" style="font-size:12px">Note (optional): what did the test show?</label>'
  +'<input id="cmfn_'+hesc(key)+'" maxlength="500" placeholder="e.g. '+hesc(id)+' passed on '+hesc(new Date().toLocaleDateString())+': all steps as expected">'
  +'<div class="row" style="gap:8px"><button class="primary" onclick="comOn(\''+hesc(key)+'\')">Switch on (asks for your password)</button><button onclick="loadCommission()">Cancel</button></div>'
  +'<div class="muted" style="font-size:11.5px;margin-top:6px">The panel restarts to apply it (about 5 s).</div>';
 el.classList.remove('hide');const n=$('cmfn_'+key);if(n)n.focus();}
async function comOn(key){const f=((window._com||{}).features||[]).find(x=>x.key===key);if(!f)return;
 const n=$('cmfn_'+key),note=n?n.value.trim():'';
 const pw=await askPassword('Switching on '+f.label+' needs your panel password.');if(pw==null)return;
 const {j}=await jpost('/api/commission',{feature:key,on:true,password:pw,note:note});
 if(!j.ok){toast(j.err||'failed',true);return;}comRestartWait(key,true,f.label);}
async function comOff(key){const fs=(window._com||{}).features||[],f=fs.find(x=>x.key===key);if(!f)return;
 // what needs it goes off too (the server does the same, transitively)
 const off=new Set([key]);for(let ch=true;ch;){ch=false;fs.forEach(x=>{if(x.saved&&!off.has(x.key)&&(x.requires||[]).some(r=>off.has(r))){off.add(x.key);ch=true;}});}
 const also=fs.filter(x=>x.key!==key&&off.has(x.key)).map(x=>x.label);
 if(!confirm('Switch off '+f.label+'?'+(also.length?'\n\nThis also switches off what needs it: '+also.join(', ')+'.':'')+'\n\nThe panel restarts to apply it (about 5 s).'))return;
 const {j}=await jpost('/api/commission',{feature:key,on:false});
 if(!j.ok){toast(j.err||'failed',true);return;}comRestartWait(key,false,f.label);}
// comRestartWait: the panel restarts itself to apply a switch (the boot script starts it again in ~5 s). Wait
// until /api/model answers with the new state, redraw the card, then reload the page so every tab's feature
// gates follow (they are read once from the model profile).
function comRestartWait(key,on,label){window._comBusy=true;const t0=Date.now();
 $('comnote').textContent='restarting the panel…';
 $('combody').innerHTML=busyHTML('Restarting the panel… so '+label+' is '+(on?'on':'off')+' (about 5 s).');
 const tick=async()=>{let m=null,s=0;try{const r=await fetch('/api/model',fetchTO(4000));s=r.status;if(r.ok)m=await r.json();}catch(e){}
  if(s===401){window._comBusy=false;boot();return;}
  if(m&&m.ok&&m.features&&!!m.features[key]===on){window._comBusy=false;window._MODEL=m;await loadCommission();
   $('comnote').textContent=label+' is '+(on?'on':'off')+' · reloading the page…';
   if(!window._reloading){window._reloading=true;setTimeout(()=>location.reload(),1800);}return;}
  if(Date.now()-t0>90000){window._comBusy=false;$('comnote').textContent='';
   $('combody').innerHTML='<div class="note bad">The panel hasn’t come back after 90 s. Reload the page in a minute; if it still doesn’t answer, reboot the miner.</div>';return;}
  setTimeout(tick,1500);};
 setTimeout(tick,1500);}
function unexpText(u){return '⚠ The miner restarted at about '+hesc(fmtWhen(u.boot_at))+' without the panel asking. It was last seen working at '+hesc(fmtWhen(u.last_seen))+' ('+Math.max(1,Math.round((u.boot_at-u.last_seen)/60))+' min before). '
 +(u.watchdog_known?(u.by_watchdog?'The <b>hardware watchdog</b> restarted it: the control board had stopped.':'The watchdog didn’t cause it: the power was cut, the reset button was used, or it was rebooted from SSH.'):'A power cut, the reset button or a reboot from SSH can do this.');}
function overTempHTML(d){const o=d.over_temp;if(!o)return '';return '<div style="grid-column:1/-1;color:var(--bad);font-size:13px;line-height:1.5;margin-bottom:4px">⚠ <b>'+OemFw()+' shut the hash boards down for heat at '+hesc(fmtWhen(o.at))+'</b>: a board sensor stayed over '+(+o.limit)+' °C for about half a minute. They stay off until the miner reboots. Let it cool, check the fans and the airflow (intake, exhaust, dust), then reboot it (Settings → Restart &amp; reboot).</div>';}
function chainHTML(d){const hs=(d.board_health||[]).filter(h=>!h.absent&&h.chips>=0&&h.chips<h.expected);if(!hs.length)return '';
 return hs.map(h=>'<div style="grid-column:1/-1;color:var(--bad);font-size:13px;line-height:1.5;margin-bottom:4px">⚠ <b>Board '+h.id+(h.chips===0?' didn’t start: none of its chips answered':(' started with '+h.chips+' of '+h.expected+' chips'))+'</b>'+(h.chips===0?(isOtherModel()&&(MODEL().boards||1)<2?'.':'. The other boards keep mining.'):': part of its chip chain isn’t answering, so it hashes less than it should.')+' Check its data cable (reseat both ends) and its power cable; a cracked joint after a knock does this too. This clears when it starts with all '+h.expected+'.</div>').join('');}
function unexpHTML(d){const u=d.unexpected_restart;if(!u)return '';return '<div style="grid-column:1/-1;font-size:13px;line-height:1.5;margin-bottom:4px">'+unexpText(u)+' <a href="#" onclick="jpost(\'/api/recovery\',{dismiss:true}).then(refresh);return false">Dismiss</a></div>';}
async function loadTime(){try{const r=await fetch('/api/time?zones=1');const t=await r.json();renderTime(t);}catch(e){$('timebody').innerHTML=busyHTML(OFFLINE);retryLater(loadTime);}}
function renderTime(t){const z=browserZone(),opts=(t.zones||[]).map(n=>'<option'+(n===t.zone?' selected':'')+'>'+hesc(n)+'</option>').join('');
 const diff=Math.round((t.now*1000-Date.now())/1000);
 $('timebody').innerHTML='<div class="ninfo"><div><span class="muted">Miner time</span> &nbsp;<b>'+hesc(t.local)+'</b> '+hesc(t.abbr)+' (UTC'+hesc(t.offset)+')</div>'+
  '<div><span class="muted">Clock</span> &nbsp;'+((t.plausible||t.synced)?(Math.abs(diff)<=5?'<b class="okpill">matches this browser</b>':'<b class="badpill">'+Math.abs(diff)+' s '+(diff>0?'ahead of':'behind')+' this browser</b>'):'<b class="badpill">not set yet</b>')+'</div>'+ntpInfo(t.ntp||{})+
  '<div><span class="muted">Time zone</span> &nbsp;<b>'+hesc(t.zone)+'</b>'+(t.by==='browser'?' <span class="muted">(from your browser)</span>':'')+(t.tzdata?' <span class="muted">· time zone rules '+hesc(t.tzdata)+'</span>':'')+'</div></div>'+
  '<div class="muted" style="font-size:11.5px;margin-top:8px">Used for the log, charts, best-share times and the schedule. The miner’s own log lines follow a change after its next restart.</div>'+
  '<div class="setgrid" style="margin-top:10px"><label>Time zone</label><select id="s_tz">'+opts+'</select>'+
  '<div class="full"><button onclick="saveTZ($(\'s_tz\').value)">Save time zone</button>'+(z&&z!==t.zone?' <button onclick="saveTZ(\''+hesc(z)+'\')">Use my browser’s ('+hesc(z)+')</button>':'')+' <span class="muted" id="s_tzmsg"></span></div>'+
  '<label>Time server</label><input id="s_ntp" class="mono" placeholder="optional, e.g. 192.168.1.2 or ntp.lan" value="'+hesc((t.ntp||{}).servers||'')+'">'+
  '<div class="full muted" style="font-size:11.5px">Used together with the time server your router hands out (DHCP). Up to three, separated by spaces. Leave empty to use only the router’s.</div>'+
  '<div class="full"><label style="display:inline"><input type="checkbox" id="s_ntppool"'+((t.ntp||{}).pool?' checked':'')+' onchange="saveNTPPool(this.checked)"> Use the public time servers (pool.ntp.org, on the internet) when neither of those gives a time</label></div>'+
  '<div class="full"><button onclick="saveNTP()">Save time server</button> <button onclick="syncNTP()">Sync now</button> <span class="muted" id="s_ntpmsg"></span></div></div>'+clockHTML(t);}
// clockHTML (when the panel offers it: manual_clock): set the date and time by hand, at any time (pre-filled with this browser's time)
function clockHTML(t){if(!t||!t.manual_clock)return '';const n=t.ntp||{},on=!!(String(n.servers||'').trim()||(n.dhcp||[]).length||n.pool);
 return '<div style="border-top:1px solid var(--border);margin-top:14px;padding-top:12px"><b style="font-size:13px">Set the clock by hand</b>'
  +'<div class="setgrid" style="margin-top:8px"><label>Date and time</label><input id="s_clock" type="datetime-local" step="1" value="'+localDT(new Date())+'">'
  +'<div class="full"><button onclick="setClock()">Set clock</button> <button onclick="setClockBrowser()">Use this browser’s time</button> <span class="muted" id="s_clockmsg"></span></div>'
  +'<div class="full muted" style="font-size:11.5px">In this browser’s time zone, from 2024 to 2039.'+(on?' Time servers are on: they correct a hand-set clock at their next sync.':'')+'</div></div></div>';}
function localDT(d){const p=n=>String(n).padStart(2,'0');return d.getFullYear()+'-'+p(d.getMonth()+1)+'-'+p(d.getDate())+'T'+p(d.getHours())+':'+p(d.getMinutes())+':'+p(d.getSeconds());}
async function setClockTo(sec){const m=$('s_clockmsg');const {j}=await jpost('/api/time',{clock:sec,manual:true});
 if(!j.ok){if(m)m.textContent=j.err||'failed';return;}toast(j.note||'Clock set');setTimeout(loadTime,800);}
async function setClock(){const v=(($('s_clock')||{}).value||'').trim(),ms=v?Date.parse(v):NaN;if(isNaN(ms)){const m=$('s_clockmsg');if(m)m.textContent='enter a date and time';return;}
 await setClockTo(Math.floor(ms/1000));}
async function setClockBrowser(){const e=$('s_clock');if(e)e.value=localDT(new Date());await setClockTo(Math.floor(Date.now()/1000));}
function ntpInfo(n){const ago=s=>{const m=Math.round((Date.now()/1000-s)/60);return m<1?'just now':m<60?m+' min ago':Math.round(m/60)+' h ago';};
 let h='<div><span class="muted">Time source</span> &nbsp;';
 if(n.last)h+='<b class="okpill">synced '+ago(n.last)+'</b> <span class="muted">from '+hesc((n.last_from||[]).join(', '))+'</span>';
 else h+='<span class="muted">no time server reached yet</span>';
 if(n.fail)h+=' <span class="muted">· last try failed: '+hesc(n.fail_from||'')+'</span>';
 h+='</div>';
 if((n.dhcp||[]).length)h+='<div><span class="muted">From your router</span> &nbsp;<span class="mono">'+hesc(n.dhcp.join(', '))+'</span></div>';
 return h;}
async function saveNTP(){const {j}=await jpost('/api/time',{ntp:$('s_ntp').value});if(!j.ok){$('s_ntpmsg').textContent=j.err||'failed';return;}
 toast('Time server saved: syncing now');setTimeout(loadTime,1500);}
async function saveNTPPool(on){const {j}=await jpost('/api/time',{ntp_pool:on});if(!j.ok){$('s_ntpmsg').textContent=j.err||'failed';loadTime();return;}
 toast(on?'Public time servers allowed: syncing now':'Public time servers off');setTimeout(loadTime,1500);}
async function syncNTP(){const {j}=await jpost('/api/time',{sync:true});if(j.ok){$('s_ntpmsg').textContent='syncing… (up to a minute)';setTimeout(loadTime,30000);}}
// Chip trims (step one, manual): per-chip clock offsets applied inside the miner by the chip-hook copy.
async function loadChipTrim(){if(featBlock('ctbody',F().ChipTrims,'Chip trims'))return;
 try{const r=await fetch('/api/chiptrim');renderChipTrim(await r.json());}catch(e){$('ctbody').innerHTML=busyHTML(OFFLINE);retryLater(loadChipTrim);}}
function ctOpen(k,def){const o=window._ctOpenSet||(window._ctOpenSet={});return (k in o?o[k]:def)?' open':'';}
function ctTog(el){(window._ctOpenSet||(window._ctOpenSet={}))[el.dataset.k]=el.open;}
function renderChipTrim(c){const L=c.limits||{min:-50,max:0,step:12.5};const busy=!!(c.strong&&c.strong.running);const G=c.geo||{chips:46,pairs:23,fix:'raise PV a step'};
 window._ctLast=c;
 clearTimeout(window._ctTimer);
 const again=ms=>{window._ctTimer=setTimeout(()=>{const ae=document.activeElement;if(ae&&(ae.tagName==='SELECT'||ae.tagName==='INPUT')&&$('ctbody')&&$('ctbody').contains(ae))again(15000);else if($('ctbody')&&$('ctbody').offsetParent)loadChipTrim();else window._ctTimer=setTimeout(loadChipTrim,30000);},ms);};
 if(busy)again(15000);else if(c.enabled)again(60000);
 const opts=v=>{let h='';for(let x=L.max;x>=L.min-1e-9;x-=L.step){const s=(x>0?'+':x<0?'−':'')+Math.abs(Math.round(x*10)/10);h+='<option value="'+x+'"'+(Math.abs(x-v)<1e-6?' selected':'')+'>'+(x===0?'0':s)+'</option>';}return h;};
 const age=c.stats_age_s,stale=c.on_s>300&&(age==null||age<0||age>300);
 const tag=$('cttag');if(tag){tag.textContent=!c.enabled?'off':c.active?'on':'on · waiting for the miner';}
 let st=!c.enabled?'<b class="badpill">off</b> <span class="muted">The mining program runs without the trim library: every chip at its board’s clock.</span>'+(c.off_why?'<div style="color:var(--bad);font-size:12.5px;margin-top:4px">⚠ Switched off by the panel '+hesc(new Date(c.off_why.at*1000).toLocaleString())+': '+hesc(c.off_why.why)+'</div>':'')
  :c.active?'<b class="okpill">on</b> <span class="muted">the trim library is reporting</span>'
  :(stale?'<b class="badpill">on · not reporting</b> <span class="muted">('+(age>0?'last report '+Math.round(age/60)+' min ago':'no report since mining started')+'): restart mining, or switch chip trims off and on.</span>':'<b class="badpill">on · not reporting yet</b> <span class="muted">(mining is still starting)</span>');
 const ch=c.chains||[];
 let nT=0,nL=0,nD=0;ch.forEach(b=>(b.chips||[]).forEach(r=>{if(r.trim)nT++;if(r.trim<0)nD++;if(r.low)nL++;}));
 let h='<p class="muted sc-intro" style="margin-top:0">Runs single chips slower (down to −'+Math.abs(L.min)+' MHz) or faster (up to +'+L.max+' MHz) than the rest of their board, so one weak chip doesn’t set the clock or voltage for all '+G.chips+'. <a href="#" onclick="event.preventDefault();const d=$(\'ctHow\');d.open=true;ctTog(d);d.scrollIntoView({behavior:\'smooth\',block:\'start\'})">How it works</a></p>'
  +'<div class="ninfo"><div><span class="muted">Status</span> &nbsp;'+st+'</div>'
  +(c.enabled&&ch.length?'<div><span class="muted">Trims</span> &nbsp;<b>'+nT+'</b> chip'+(nT===1?'':'s')+(nT?' <span class="muted">('+nD+' slower, '+(nT-nD)+' faster)</span>':'')+(c.watch&&c.watch.on?' · auto-trim on':'')+'</div>'+(nL?'<div><span class="muted">Low</span> &nbsp;<span style="color:var(--warn)">'+nL+' chip'+(nL===1?'':'s')+'</span> <span class="muted">below their board: open a board to see which, and what to do</span></div>':''):'')+'</div>'
  +'<div class="row" style="margin-top:10px;gap:8px;flex-wrap:wrap"><button'+(c.enabled?'':' class="primary"')+' onclick="ctEnable('+(!c.enabled)+')">'+(c.enabled?'Switch off':'Switch on')+'</button>'
  +(c.enabled&&!busy?'<button onclick="ctPost({apply:true})" title="Write every trim to the chips again now">Re-apply</button><button onclick="ctReset()">Reset all trims</button>':'')
  +'<button onclick="ctReport()" title="One file with the trims, every chip’s clock and nonce counts, 48 hours of history and the library’s log: save it to keep or share">⤓ Save report</button>'
  +' <span class="muted" id="ctmsg">'+hesc(c.note||'')+'</span></div>';
 // the boards: one collapsible row each, with a summary
 ch.forEach(b=>{const avg=b.avg||0,luck=avg>0?Math.round(200/Math.sqrt(avg)):0,young=avg<30,cs=b.chips||[];
  const tr=cs.filter(r=>r.trim),low=cs.filter(r=>r.low),dn=tr.filter(r=>r.trim<0).length,up=tr.length-dn;
  const sum='<b>Board '+b.chain+'</b> <span class="muted">'+b.base+' MHz</span>'
   +' <span class="ctsum">'+(tr.length?(dn?' <span class="ctb down">'+dn+' slower</span>':'')+(up?' <span class="ctb up">'+up+' faster</span>':''):'<span class="muted">no trims</span>')
   +(low.length?' <span class="ctb low" title="chips with clearly fewer good results per MHz than the board’s average">low: '+low.slice().sort((x,y)=>x.share-y.share).slice(0,2).map(r=>'c'+r.chip+' '+Math.round(r.share)+'%').join(', ')+(low.length>2?' +'+(low.length-2):'')+'</span>':'')+(young?'<span class="muted"> · counting</span>':'')+'</span>';
  h+='<details class="ctboard" data-k="b'+b.chain+'" ontoggle="ctTog(this)"'+ctOpen('b'+b.chain,false)+'><summary>'+sum+'</summary>'
   +'<div class="muted" style="font-size:11.5px;margin:4px 0 6px;line-height:1.5">% = the chip’s good results (nonces) per MHz since mining started or the chips’ clocks last changed, against its board’s average'+(avg?' ('+Math.round(avg)+' each now, so ±'+luck+' % is luck'+(young?'; give it a few hours':'')+')':'')+'. <span style="color:var(--warn)">Orange</span>: clearly low. Each box is a pair sharing a power stage: one trim per pair.<br>'
   +'<b>A low chip</b>: a few % low isn’t worth a trim (−12.5 MHz takes about '+(b.base>0?Math.round(1250/b.base):2)+' % of that chip’s work); trim one far below, or one making many errors (<a href="#" onclick="event.preventDefault();showTab(\'dash\');setTimeout(()=>{const p=$(\'chipspanel\');if(p)p.scrollIntoView({block:\'start\'})},300)">errors per chip</a>), and see whether its % recovers. Many low chips on one board: '+G.fix+' instead.</div><div class="ctgrid pairs">';
  let open=0;
  cs.forEach(r=>{const cls=r.trim<0?' down':r.trim>0?' up':'';
   if(r.chip%2===1||!open){if(open)h+='</div></div>';const a=r.chip%2===1?r.chip:r.chip-1;h+='<div class="ctpair" title="chips '+a+' and '+(a+1)+' share a power stage"><div class="ctpi">';open=1;}
   const sty=r.low?' style="color:var(--warn)"':young?' class="muted"':'';
   h+='<div class="ctc'+cls+'" title="c'+r.chip+': '+(r.clock||b.base)+' MHz, '+(r.nonces||0)+' nonces (board average '+Math.round(avg)+')"><div class="ctt"><b>c'+r.chip+'</b><span'+sty+'>'+(r.nonces?Math.round(r.share)+'%':'—')+'</span></div>'
    +'<select aria-label="board '+b.chain+' chip '+r.chip+' trim (MHz)" data-chain="'+b.chain+'" data-chip="'+r.chip+'" onchange="ctSet(this)"'+(c.enabled&&!busy?'':' disabled')+'>'+opts(r.trim||0)+'</select></div>';
   if(r.chip%2===0){h+='</div></div>';open=0;}});
  if(open)h+='</div></div>';
  h+='</div></details>';});
 if(c.enabled&&!ch.length)h+='<div class="muted" style="margin-top:10px">The chips appear once the miner is running with the trim library (about a minute after switching on).</div>';
 // auto-trim and strong chips, collapsible
 const W=c.watch||{};
 if(c.enabled||W.on||(W.owned||[]).length)h+='<details class="ctsec" data-k="auto" ontoggle="ctTog(this)"'+ctOpen('auto',false)+'><summary><b>Auto-trim</b> '+(W.on?'<b class="okpill">on</b> <span class="muted">'+hesc(W.status||'')+'</span>':'<b class="badpill">off</b>')+'</summary>'+ctWatchHTML(c)+'</details>';
 const S=c.strong||{};
 if(c.enabled||S.now||S.last)h+='<details class="ctsec" data-k="strong" ontoggle="ctTog(this)"'+ctOpen('strong',!!S.now)+'><summary><b>Find strong chips</b> '+(S.now?'<b class="okpill">running</b>':'')+'</summary>'+ctStrongHTML(c)+'</details>';
 h+='<details class="ctsec" id="ctHow" data-k="how" ontoggle="ctTog(this)"'+ctOpen('how',!c.enabled&&!isOtherModel())+'><summary><b>How chip trims work</b></summary><div class="ctxt">'
  +'<p>'+OemOf()+' mining program can already set each chip to its own clock, but always gives every chip the board’s clock. With chip trims on, the miner runs a copy with a small library that sets the chips you trim, right after each clock change, in 12.5 MHz steps (the clock’s own step). With chip trims off, '+oemPoss()+' unmodified program runs. If the trimmed copy fails to start, the miner falls back to it by itself, and trims switch themselves off if mining keeps stopping.</p>'
  +'<p><b>Slower (−)</b> for a chip that makes more errors than the rest: the rest of the board can then run faster or at a lower voltage. <b>Faster (+)</b> for a strong chip, never above '+(PUB()&&G.box2?(((MODEL()||{}).freq_max||690)+' MHz'):'the clock ceiling')+'. Trims follow the board clock and come back after a restart.</p>'
  +(G.box2?'<p><b>Chip pairs.</b> The board’s supply runs through '+G.pairs+' power stages in series, two chips side by side in each: chips 1+2, 3+4 … '+(G.chips-1)+'+'+G.chips+' (the boxes). '+(PUB()?'Expected from this layout (not measured on this model)':'Measured on a real SC Lite (same two-chips-per-stage layout)')+': slowing a chip gives its partner a little extra voltage and takes a little from every other chip; slowing <b>both chips of a pair</b> costs the rest of the board about 2½ times as much as spreading the same trims one per pair.</p>':'<p><b>Chip pairs.</b> A board’s voltage (PV, about 9.4 V) runs through 23 power stages in series, two chips side by side in each: chips 1+2, 3+4 … 45+46 (the boxes). Measured on a real SC Lite: slowing a chip gives its partner a little extra voltage and takes a little from every other chip; slowing <b>both chips of a pair</b> costs the rest of the board about 2½ times as much as spreading the same trims one per pair.</p>')
  +'<ul><li><b>Prefer:</b> one trim per pair; trim the weak chip and look at its partner again before trimming it too; a + trim on a chip whose partner is clean.</li>'
  +(G.box2?'<li><b>Avoid:</b> − trims on both chips of a pair; + on a chip whose partner is weak; many trims on one board. If the untrimmed chips start making more errors after a few trims, '+G.fix+' instead.</li></ul>':'<li><b>Avoid:</b> − trims on both chips of a pair; + on a chip whose partner is weak; many trims on one board with PV at its floor. If the untrimmed chips start making more errors after a few trims, raise PV a step instead.</li></ul>')
  +'<p><b>The %</b> on each chip is its good results (nonces) since mining started, against its board’s average. A chip a little short of voltage gets more results wrong, and those are thrown away, so its % drops even though its clock is the same. Small counts are mostly luck; the board summary says how much.</p>'
  +'<p><b>Auto-trim</b> slows chips that keep making far more errors than the rest, one per board at a time and one per pair, and keeps a trim only if it helps. '+(PUB()&&featOff2(F().Tuner)?'':'<b>The tuner</b> can trim the chip that fails a test (Tuner → “Trim weak chips”). ')+'<b>Find strong chips</b> tries running a board’s cleanest chips faster.</p>'
  +'</div></details>';
 if((c.log||[]).length)h+='<details class="ctsec" data-k="log" ontoggle="ctTog(this)"'+ctOpen('log',false)+'><summary class="muted">Library log</summary><pre class="mono" style="font-size:11px;white-space:pre-wrap">'+hesc(c.log.map(l=>{const m=l.match(/^(\d+) (.*)$/);return m?new Date(+m[1]*1000).toLocaleTimeString()+'  '+m[2]:l;}).join('\n'))+'</pre></details>';
 const fe=document.activeElement,fk=fe&&$('ctbody').contains(fe)?(fe.id?'#'+fe.id:fe.dataset.chip!=null?'select[data-chain="'+fe.dataset.chain+'"][data-chip="'+fe.dataset.chip+'"]':fe.tagName==='SUMMARY'&&fe.parentElement.dataset.k?'details[data-k="'+fe.parentElement.dataset.k+'"]>summary':fe.tagName==='BUTTON'&&fe.getAttribute('onclick')?'button[onclick="'+fe.getAttribute('onclick').replace(/["\\]/g,'\\$&')+'"]':''):'';
 const keep={};['sct_b','sct_bw','sct_sw'].forEach(id=>{const e=$(id);if(e)keep[id]=e.value;}); // the strong-chip form survives a refresh
 $('ctbody').innerHTML=h;for(const id in keep){const e=$(id);if(e&&(e.tagName!=='SELECT'||[...e.options].some(o=>o.value===keep[id])))e.value=keep[id];}
 if(fk){const e=$('ctbody').querySelector(fk);if(e)e.focus({preventScroll:true});}}
async function ctPost(b){const m=$('ctmsg');if(m)m.textContent='applying… (up to ~15 s right after a setting change)';const {j}=await jpost('/api/chiptrim',b);if(!j.ok){toast(j.err||'failed',true);if(window._ctLast)renderChipTrim(window._ctLast);return null;}renderChipTrim(j);if(j.note)toast(j.note);return j;}
function ctWatchHTML(c){const W=c.watch||{};if(!c.enabled&&!W.on&&!(W.owned||[]).length)return '';
 const wm=W.window_min||120,win=wm%60===0?(wm===60?'hour':wm/60+' hours'):wm+' minutes';
 const ago=t=>{const m=Math.round((Date.now()/1000-t)/60);return m<1?'just now':m<60?m+' min ago':Math.floor(m/60)+' h '+(m%60)+' min ago';};
 let h='<div><div class="muted" style="font-size:11.5px;margin-top:4px;line-height:1.5">Watches every chip’s hardware errors while mining. Every '+win+' it looks at the errors since the last check: a chip that made far more than the rest of its board (at least 10, and 6 times the typical chip, not just bad luck) is slowed 12.5 MHz. The next '+win+' show whether that helped: if the chip’s errors fell clearly the trim stays, otherwise it’s undone and that chip is left alone for a day. One chip per board at a time, to '+(W.floor||-50)+' MHz at most and '+(W.max_board||6)+' chips per board, and <b>one chip per pair</b> (the blue boxes: slowing both chips of a pair costs the rest of the board far more). If the board’s untrimmed chips start making more errors after a trim, it adds no more trims there for a day and suggests '+((c.geo&&c.geo.box2)?'you '+hesc(c.geo.fix||'lower the board clock a step'):'raising PV a step')+'. Trims you set by hand are left alone, except a chip you set to run faster than its board (+): if it becomes one of the board’s worst it is stepped back toward the board clock, 12.5 MHz a window. Pauses while the tuner or the strong-chip test runs and starts counting again after a mining restart or a clock change. It keeps running across reboots until you switch it off.</div>';
 if(W.on)h+='<div class="ninfo" style="margin-top:8px"><div><span class="muted">Now</span> &nbsp;'+hesc(W.status||'')+(W.checked?' <span class="muted">· checked '+ago(W.checked)+'</span>':'')+'</div></div>';
 const ow=W.owned||[];
 if(ow.length)h+='<div style="margin-top:8px;font-size:12.5px"><span class="muted">Its trims</span><table class="mono" style="font-size:12px;margin-top:4px;border-collapse:collapse">'+ow.map(o=>'<tr><td style="padding:2px 10px 2px 0">b'+o.chain+'.c'+o.chip+'</td><td style="padding:2px 10px 2px 0">'+(o.mhz>0?'+':'')+o.mhz+' MHz</td><td class="muted" style="padding:2px 0;font-family:inherit">'+(o.origin==='tuner'?'from the tuner':o.judged?'kept: its errors fell':'being checked')+' · '+hesc(o.why||'')+'</td></tr>').join('')+'</table></div>';
 h+='<div class="row" style="margin-top:8px;gap:8px"><button '+(W.on?'':'class="primary" ')+'onclick="ctWatch('+(!W.on)+')"'+(!c.enabled&&!W.on?' disabled title="Switch chip trims on first"':'')+'>'+(W.on?'Switch auto-trim off':'Switch auto-trim on')+'</button>'
  +(ow.length?'<button onclick="ctWatchUndo()">Undo auto-trims</button>':'')+'</div>';
 const L=(W.log||[]).slice(-8).reverse();
 if(L.length)h+='<details style="margin-top:8px" data-k="alog" ontoggle="ctTog(this)"'+ctOpen('alog',false)+'><summary class="muted">Auto-trim log</summary><pre class="mono" style="font-size:11px;white-space:pre-wrap">'+hesc(L.map(l=>new Date(l.t*1000).toLocaleString()+'  '+l.msg).join('\n'))+'</pre></details>';
 return h+'</div>';}
async function ctWatch(on){const wm=(((window._ctLast||{}).watch)||{}).window_min||120;if(on&&!confirm('Switch auto-trim on?\n\nEvery '+(wm%60?wm+' minutes':wm===60?'hour':wm/60+' hours')+' it may slow one chip per board that keeps making far more errors than the rest, and keeps the trim only if that chip’s errors then fall. Your own trims are left alone, except that a chip you set to run faster than its board is stepped back toward it if it becomes one of the board’s worst.'))return;
 if(!on&&!confirm('Switch auto-trim off?\n\nIts trims stay as they are; “Undo auto-trims” removes them.'))return;ctPost({watch:on});}
async function ctWatchUndo(){if(!confirm('Remove every trim auto-trim set? Trims you set by hand stay.'))return;ctPost({watch_undo:true});}
function ctStrongHTML(c){const S=c.strong||{},n=S.now,L=S.last,D=S.defaults||{};if(!c.enabled&&!n&&!L)return '';
 const hm=m=>m>=60?Math.floor(m/60)+' h '+(m%60)+' min':m+' min',mhz=v=>(v>0?'+':'')+v;
 const stTxt={rising:'rising',frozen:'stopped (errors)',max:'clean to the max',held:'held back'};
 const tbl=(a,done)=>{const cs=a.chips||[];if(!cs.length)return '';
  return '<div style="overflow-x:auto"><table class="sct"><tr><th>Chip</th><th title="errors in the baseline">Base</th><th>'+(done?'Kept':'Trim')+', MHz</th><th title="'+(done?'errors in each step':'errors so far in this step')+'">'+(done?'Steps':'Now')+'</th><th>Why</th></tr>'
   +cs.map(r=>{const v=done?r.kept:r.trim,se=(r.step_errors||[]).join('/')||'—';
    return '<tr><td class="mono">c'+hesc(r.chip)+'</td><td class="mono">'+hesc(r.base_err)+'</td><td class="mono nw'+(v>0?' up':'')+'">'+(v?hesc(mhz(v)):'0')+'</td><td class="mono nw'+(r.state==='frozen'?' bad':'')+'">'+hesc(done?se:r.live_errors)+'</td><td class="why">'+hesc(r.why||stTxt[r.state]||r.state||'')+'</td></tr>';}).join('')
   +'</table></div>';};
 const logH=a=>{const g=(a.log||[]).slice(-12).reverse();return g.length?'<details style="margin-top:8px" data-k="tlog" ontoggle="ctTog(this)"'+ctOpen('tlog',false)+'><summary class="muted">Test log</summary><pre class="mono" style="font-size:11px;white-space:pre-wrap">'+hesc(g.map(l=>new Date(l.t*1000).toLocaleTimeString()+'  '+l.msg).join('\n'))+'</pre></details>':'';};
 let h='<div>';
 if(n){const mm=Math.floor(n.left_s/60),ss=('0'+n.left_s%60).slice(-2),el=n.minutes||0,pc=Math.min(100,Math.max(2,Math.round(100*el/Math.max(1,el+(n.eta_max_min||0)))));
  h+='<div style="margin-top:6px;font-size:12.5px"><b class="okpill">running · '+hm(el)+'</b> Board <b>'+n.chain+'</b> · '+(n.confirm?'<b>confirming</b> the kept steps (window '+n.confirm+')':n.step?'step <b>'+n.step+' of '+n.steps+'</b> ('+mhz(n.step_mhz)+' MHz)':'<b>baseline</b> at the board clock ('+n.base_clock+' MHz)')
   +' · '+(n.phase==='watching'?'watching, '+mm+':'+ss+' left':'settling')+(n.temp_now?' · '+Math.round(n.temp_now)+' °C':'')+'</div>'
   +'<div style="height:6px;background:var(--panel2);border-radius:3px;margin-top:8px;overflow:hidden"><div style="height:100%;width:'+pc+'%;background:var(--ok)"></div></div>'
   +'<div class="muted" style="font-size:11.5px;margin-top:4px">at most about '+hm(n.eta_max_min||0)+' left (less once chips stop rising) · windows '+n.baseline_min+' / '+n.step_min+' min'+(n.note?' · '+hesc(n.note):'')+'</div>'
   +((n.chips||[]).length?tbl(n,false):'<div class="muted" style="font-size:11.5px;margin-top:6px">The chips to raise are picked when the baseline ends.</div>')
   +logH(n)
   +'<div class="row" style="margin-top:8px;gap:8px;align-items:center"><button onclick="ctStrong(false)">Stop test</button><span class="muted" style="font-size:11.5px">Stopping puts every chip back to its trim from before. Trims can’t be changed while it runs.</span></div>';
 }else if(c.enabled){const bs=(c.chains||[]).map(x=>x.chain),bw=D.baseline_min||20,sw=D.step_min||20,mn=D.min_min||5;
  h+='<div class="muted" style="font-size:11.5px;margin-top:4px;line-height:1.5">For one board: watches every chip’s errors for a while at the current setting, then runs up to '+(D.max_chips||10)+' of its cleanest chips (no trim, and no errors or clearly fewer errors than the board’s typical chip) faster, '+mhz(D.step_mhz||12.5)+' MHz at a time up to '+mhz(D.max_mhz||50)+' MHz, watching each step. Never both chips of a pair, and never the partner of a trimmed chip (a faster chip takes voltage from its partner). A chip that starts making errors goes back one step and stays there; if the whole board gets worse (errors, nonce rate, or more than 5 °C warmer) every raised chip goes back one step and the test ends. Chips keep the fastest step they passed, saved as your own trims. Those chips then run faster than the board at the same voltage, so check them now and then (auto-trim steps a “+” chip back if it turns bad). Needs the board at '+Math.round(D.start_max_c||78)+' °C or cooler.</div>'
   +(bs.length?'<div class="sctform"><label class="muted" for="sct_b" style="font-size:12px">Board</label><select id="sct_b">'+bs.map(b=>'<option value="'+b+'">'+b+'</option>').join('')+'</select>'
    +'<button class="primary" onclick="ctStrong(true)">Find strong chips</button></div>'
    +'<details style="margin-top:6px" data-k="win" ontoggle="ctTog(this)"'+ctOpen('win',false)+'><summary class="muted" style="font-size:11.5px">Window lengths</summary><div class="sctform"><span class="nw"><label class="muted" for="sct_bw" style="font-size:12px">Baseline</label> <input type="number" id="sct_bw" min="'+mn+'" max="'+(D.max_min||240)+'" step="1" value="'+bw+'"> <span class="muted" style="font-size:12px">min</span></span>'
    +'<span class="nw"><label class="muted" for="sct_sw" style="font-size:12px">Each step</label> <input type="number" id="sct_sw" min="'+mn+'" max="'+(D.max_min||240)+'" step="1" value="'+sw+'"> <span class="muted" style="font-size:12px">min</span></span></div>'
    +'<div class="muted" style="font-size:11.5px;margin-top:4px">Whole minutes, '+mn+' to '+(D.max_min||240)+'. Longer windows catch rarer errors.</div></details>'
    :'<div class="muted" style="font-size:11.5px;margin-top:6px">The board list appears once mining runs with chip trims on (about a minute after switching on).</div>');
 }
 if(L&&!n){const good=L.how==='done'||L.how==='guard'||L.how==='none',lbl={done:'finished',guard:'ended by the board guard',none:'no candidates',stopped:'stopped',aborted:'aborted',interrupted:'interrupted'}[L.how]||L.how;
  h+='<div style="margin-top:10px"><span class="muted">Last test</span> &nbsp;<b class="'+(good?'okpill':'badpill')+'">'+hesc(lbl)+'</b> <span class="muted">board '+hesc(L.chain)+' · '+hesc(new Date(L.ended*1000).toLocaleString())+' · '+hesc(hm(L.minutes||0))+'</span>'
   +'<div style="font-size:12.5px;margin-top:4px">'+hesc(L.message||'')+'</div>'+tbl(L,true)+logH(L)+'</div>';}
 return h+'</div>';}
async function ctStrong(start){
 if(start){const S=(window._ctLast||{}).strong||{},D=S.defaults||{},b=+$('sct_b').value,bw=+(($('sct_bw')||{}).value||D.baseline_min||20),sw=+(($('sct_sw')||{}).value||D.step_min||20),mn=D.min_min||5;
  const mx=D.max_min||240;if(![bw,sw].every(v=>Number.isInteger(v)&&v>=mn&&v<=mx)){toast('the windows are whole minutes, '+mn+' to '+mx,true);return;}
  const tot=(D.settle_min||2)+bw+((D.steps||4)+(D.confirm?1:0))*((D.settle_min||2)+sw),hm=m=>m>=60?Math.floor(m/60)+' h '+(Math.round(m)%60)+' min':Math.round(m)+' min';
  if(!confirm('Find strong chips on board '+b+'?\n\nFirst it watches every chip’s errors for '+bw+' min at the current setting. Then up to '+(D.max_chips||10)+' of its cleanest chips (no trim, and no errors or clearly fewer errors than the board’s typical chip) run faster, +'+(D.step_mhz||12.5)+' MHz at a time up to +'+(D.max_mhz||50)+' MHz, each step watched for '+sw+' min, then one more window to confirm the result. About '+hm(tot)+' (a little longer if a chip has to step back at the end).\n\nThose chips run faster than the rest of the board at the same voltage. Only a chip that stays clean keeps a step: one that makes errors goes back a step, and if the whole board gets worse (errors, hashrate, temperature) every raised chip goes back a step and the test ends. What passes is saved as your own trims. Stop puts every chip back as it was.\n\nIt catches a chip that gets clearly worse; one only a little worse than the board’s typical chip can’t be told apart in a test this long, so watch the chips’ errors on this card for a day afterwards.'))return;
  const {j}=await jpost('/api/strongtest',{start:true,chain:b,baseline_min:bw,step_min:sw});if(!j.ok){toast(j.err||'failed',true);return;}renderChipTrim(j);loadControl();toast('Strong-chip test started on board '+b);}
 else{if(!confirm('Stop the strong-chip test?\n\nEvery chip goes back to its trim from before the test.'))return;const {j}=await jpost('/api/strongtest',{stop:true});if(!j.ok){toast(j.err||'failed',true);return;}renderChipTrim(j);loadControl();toast('Strong-chip test stopped');}}
async function ctReport(){try{const r=await fetch('/api/chipreport');if(!r.ok){toast('report unavailable',true);return;}
 const blob=await r.blob(),cd=r.headers.get('Content-Disposition')||'';const fn=(cd.match(/filename="?([^"]+)"?/)||[])[1]||'sclite-chips.json';
 const u=URL.createObjectURL(blob),a=document.createElement('a');a.href=u;a.download=fn;document.body.appendChild(a);a.click();a.remove();setTimeout(()=>URL.revokeObjectURL(u),2000);
 toast('Saved '+fn);}catch(e){toast('report failed: '+e.message,true);}}
async function ctSet(el){const ch=+el.dataset.chain,c=+el.dataset.chip,v=+el.value,p=c%2?c+1:c-1;
 const pe=document.querySelector('[data-chain="'+ch+'"][data-chip="'+p+'"]');
 const row=k=>((((window._ctLast||{}).chains||[]).find(b=>b.chain===ch)||{}).chips||[]).find(r=>r.chip===k)||{},me=row(c),pr=row(p);
 if(v>0&&!(me.trim>0)&&(me.low||pr.low||(pe&&+pe.value<0))&&!confirm('Run chip '+c+' faster than its board?\n\n'+(me.low?'Chip '+c+' itself is low ('+Math.round(me.share)+' %): it’s more likely short of voltage than strong.':'Its partner, chip '+p+' (same power stage), is '+(pe&&+pe.value<0?'slowed ('+pe.options[pe.selectedIndex].text+' MHz)':'low ('+Math.round(pr.share)+' %)')+': a faster chip takes voltage from it.')+'\n\n“Find strong chips” picks clean chips and watches them for you. Set +'+v+' MHz anyway?')){if(window._ctLast)renderChipTrim(window._ctLast);return;}
 if(v<0&&pe&&+pe.value<0&&!confirm('Chip '+p+' shares a power stage with chip '+c+' and is already slowed ('+pe.options[pe.selectedIndex].text+' MHz).\n\nSlowing both chips of a pair costs the rest of the board far more than spreading trims one per pair (tests on a real SC Lite: 16 % of the other chips\u2019 good work against 6.5 %). Usually better: keep one trim per pair and '+((window._ctLast&&window._ctLast.geo&&window._ctLast.geo.fix)||'raise PV a step')+'.\n\nSlow chip '+c+' anyway?')){if(window._ctLast)renderChipTrim(window._ctLast);return;}
 await ctPost({chain:ch,chip:c,mhz:v});}
async function ctReset(){const n=(((window._ctLast||{}).trims)||[]).filter(t=>t.mhz).length;if(!confirm('Remove all '+n+' trim'+(n===1?'':'s')+' (yours, auto-trim’s and the tuner’s) and put every chip back to its board clock?\n\nWith auto-trim on, it may slow a chip again later.'))return;await ctPost({reset:true});}
async function ctEnable(on){if(!confirm(on?'Switch chip trims on?\n\nMining restarts (about a minute) with a copy of '+oemPoss()+' miner that loads the trim library. With no trims set it mines exactly like stock.':'Switch chip trims off?\n\nMining restarts with '+oemPoss()+' own miner; every chip runs at the board clock. Your trims are kept and come back when you switch chip trims on again.'))return;
 window._rebStart=Date.now();window._rebNominal=150000;await ctPost({enable:on});setTimeout(loadChipTrim,90000);}
// MAC address: applied at boot by /etc/sclite-mac, right after Goldshell's maccfg.
async function loadMAC(){try{const r=await fetch('/api/mac');renderMAC(await r.json());}catch(e){}}
function renderMAC(m){window._mac=m;const md=m.mode||'keep';const sel=$('s_macmode');if(!sel)return;
 sel.value=md;const i=$('s_mac');if(i)i.value=md==='fixed'?(m.saved||''):'';macModeUI();
 const next=md==='random'?'random':(m.saved||m.current||'—');
 $('s_macinfo').innerHTML='<b class="mono">'+hesc(m.current||'—')+'</b>'+(next!==m.current?' <span class="muted">→ next boot</span> <span class="mono">'+hesc(next)+'</span>':'')
  +(m.hardware&&m.hardware!==m.current?' <span class="muted">(chip started with '+hesc(m.hardware)+')</span>':'');}
function macModeUI(){const v=($('s_macmode')||{}).value;const f=v==='fixed';['s_mac','s_maclbl'].forEach(id=>{const e=$(id);if(e)e.style.display=f?'':'none';});}
async function saveMAC(){const m=window._mac||{},v=$('s_macmode').value,mac=($('s_mac').value||'').trim();
 if(v===(m.mode||'keep')&&(v!=='fixed'||mac.toLowerCase().replace(/-/g,':')===(m.saved||'')))return null;
 const {j}=await jpost('/api/mac',{mode:v,mac:mac});if(!j.ok)return {err:j.err||'MAC not saved'};
 renderMAC(j);return {note:'MAC: '+(v==='fixed'?j.saved:v==='keep'?'keep '+(j.saved||j.current):'random')+' from the next reboot'};}
// ---- firmware update (Settings) ----
async function loadUpdate(){if(featOff2(F().FirmwareUpdate)){renderUpdateOff();return;}
 try{const r=await fetch('/api/update');renderUpdate(await r.json());}catch(e){$('updbody').innerHTML=busyHTML(OFFLINE);if(updProgGet())updProgInSettings();retryLater(loadUpdate);}}
function updWhen(t){return t?new Date(t*1000).toLocaleString():'';}
// renderUpdateOff: on a model whose FirmwareUpdate gate is off (SD-card-only install), the in-panel
// update/rollback is replaced by settings export/import (coordinated with B7). Endpoints:
//   GET  /api/settings/export  -> JSON download of the portable settings
//   POST /api/settings/import  (JSON body) -> restore them
function renderUpdateOff(){const nm=modelName();const ne=$('updnote');if(ne)ne.textContent='SD card only';
 $('updbody').innerHTML=
  '<div class="muted" style="font-size:12.5px;line-height:1.6">In-panel firmware update and rollback are <b>off on this model</b> ('+hesc(nm)+'): its system runs from a read-only image, so a new release is installed by <b>writing a fresh SD card</b>, which always restores a known-good system. An SD flash wipes saved settings, so export them first and import them afterwards.</div>'
  +'<div class="ninfo" style="margin-top:10px"><div><b>Settings backup</b></div>'
  +'<div class="row" style="gap:8px;align-items:center;margin-top:6px"><button onclick="settingsExport()">Export settings (.json)</button><span class="muted" style="font-size:11.5px">network, fan curves, presets, schedule and notifications — not the pools, hostname, time settings, passwords or secrets</span></div>'
  +'<div class="row" style="gap:8px;align-items:center;margin-top:8px"><input type="file" id="setimport" accept=".json,application/json" style="max-width:20rem"><button onclick="settingsImport()">Import settings</button><span class="muted" id="setimpmsg" style="font-size:11.5px"></span></div></div>'
  +'<div class="muted" style="font-size:11.5px;margin-top:12px">Writing an SD card always works for a first install, or to recover a miner that won’t boot.</div>';}
async function settingsExport(){try{const r=await fetch('/api/settings/export');if(!r.ok){toast('Settings export isn’t available on this miner yet (HTTP '+r.status+')',true);return;}
 const b=await r.blob(),u=URL.createObjectURL(b),a=document.createElement('a');
 a.href=u;a.download=(modelName().replace(/\s+/g,'-').toLowerCase()||'miner')+'-settings.json';document.body.appendChild(a);a.click();a.remove();setTimeout(()=>URL.revokeObjectURL(u),2000);}catch(e){toast('Settings export failed',true);}}
async function settingsImport(){const f=$('setimport'),msg=$('setimpmsg');if(!f||!f.files||!f.files[0]){if(msg)msg.textContent='choose a settings file first';return;}
 let body;try{body=JSON.parse(await f.files[0].text());}catch(e){if(msg)msg.textContent='that file isn’t valid settings JSON';return;}
 const {s,j}=await jpost('/api/settings/import',body);if(j&&j.ok){if(msg)msg.textContent='';toast(j.note||'Settings imported — reboot to apply');}else if(msg)msg.textContent=(j&&j.err)||('import failed (HTTP '+s+')');}
function renderUpdate(u,latest){if(u&&u.image){renderImageUpdate(u,latest);return;}window._upd=u;const v=u.version||'';
 $('updnote').textContent=v?('v'+v):'';
 let h='<div class="muted" style="font-size:12px;line-height:1.5">Install a new release without an SD card: your settings, pools, presets and schedule stay as they are (unless a release’s “What’s new” says otherwise). The miner checks the file, saves the files it replaces, installs and reboots. If the new version doesn’t start properly, the previous one is put back by itself.</div>'
  +'<div class="ninfo" style="margin-top:10px"><div><span class="muted">Installed</span> &nbsp;<b>'+(v?'v'+hesc(v):'v1.4.5 or earlier')+'</b></div>';
 const L=u.last;
 if(u.pending)h+='<div><span class="muted">Just updated</span> &nbsp;<b class="okpill">v'+hesc(u.pending.to)+' starting</b> <span class="muted">it’s confirmed once the panel has run for a minute and a half and this page reaches it</span></div>';
 else if(L){const good=L.result==='installed';
  h+='<div><span class="muted">Last update</span> &nbsp;<b class="'+(good?'okpill':'badpill')+'">'+hesc(L.result)+'</b> <span class="muted">'
   +(L.result==='installed'?('v'+hesc(L.to)+(L.from?' (from v'+hesc(L.from)+')':'')):L.result==='rolled back'?('from v'+hesc(L.from||'?')+' to '+(L.to?'v'+hesc(L.to):'the previous version')):('v'+hesc(L.to||'?')))
   +' · '+hesc(updWhen(L.at))+'</span>'+(L.note?'<div class="muted" style="font-size:11.5px">'+hesc(L.note)+'</div>':'')+'</div>';}
 h+='</div>';
 if(u.migration)h+=updMigHTML(u.migration);
 if(u.busy)h+='<div class="note" style="margin-top:10px">'+hesc(u.busy)+'…</div>';
 if(u.staged){const s=u.staged;
  h+='<div class="note" style="margin-top:10px"><b>Ready to install: v'+hesc(s.version)+'</b> <span class="muted">('+s.files+' files, '+Math.round(s.size/1048576*10)/10+' MB'+(s.remove?(', removes '+s.remove):'')+'; checked: complete and made for this miner)</span>'
   +updSumsHTML(s)
   +updWarnHTML(s.warning)
   +'<div class="row" style="margin-top:8px;gap:8px"><button class="primary" onclick="updInstall()">Install v'+hesc(s.version)+' and reboot</button><button onclick="updPost({discard:true})">Cancel</button></div>'
   +'<div class="muted" style="font-size:11.5px;margin-top:6px">Mining stops for 2–3 minutes while it installs and reboots. Don’t switch the miner off meanwhile, and keep this page open: the new version is kept once this page reaches it again over the network (otherwise it goes back to the previous one by itself after 5 minutes).</div></div>';}
 else{
  h+='<div style="margin-top:12px"><b style="font-size:13px">From a file</b><div class="muted" style="font-size:11.5px;margin:3px 0 6px">The <span class="mono">sclite-update-v….tar.gz</span> file from a release’s downloads (not the .img.xz, which is for SD cards).</div>'
   +'<div class="row" style="gap:8px;align-items:center"><input type="file" id="updfile" accept=".gz,.tgz" style="max-width:20rem"><button onclick="updUpload()">Check file</button><span class="muted" id="updmsg" style="font-size:12px"></span></div></div>'
   +'<div style="margin-top:14px"><b style="font-size:13px">From GitHub</b><div class="muted" style="font-size:11.5px;margin:3px 0 6px">The miner looks up the latest release of <span class="mono">'+hesc(u.repo)+'</span> itself (it needs internet access).</div>'
   +'<div class="row" style="gap:8px"><button onclick="updCheck()">Check for updates</button><span class="muted" id="updcheck" style="font-size:12px"></span></div><div id="updlatest"></div>'
   +'<details style="margin-top:8px"><summary class="muted" style="font-size:12px">Update source</summary><div class="setgrid" style="margin-top:6px"><label>GitHub repository</label><input id="updrepo" class="mono" value="'+hesc(u.repo)+'" placeholder="'+hesc(u.default_repo)+'">'
   +'<div class="full"><button onclick="updRepo()">Save</button> <span class="muted" style="font-size:11.5px">owner/name, for your own build or a fork. Empty: '+hesc(u.default_repo)+'.</span></div></div></details></div>';}
 if(u.can_rollback&&!u.staged)h+='<div style="margin-top:14px;border-top:1px solid var(--border);padding-top:10px"><button onclick="updRollback()">Roll back to the previous version</button> <span class="muted" style="font-size:11.5px">reboots and puts back the files the last update replaced as it starts</span></div>';
 h+='<div class="muted" style="font-size:11.5px;margin-top:12px">A full SD-card flash always works as well, for a first install or if anything goes wrong.</div>';
 $('updbody').innerHTML=h;if(updProgGet())updProgInSettings();
 if(latest)updShowLatest(latest);}
function updMigHTML(m){const st=m.stock||'';
 let h='<div class="note" style="margin-top:10px"><b>v1.5.6: a fresh start for tuning</b> <span class="muted">'+hesc(updWhen(m.at))+'</span>'
  +'<div style="font-size:12px;line-height:1.5;margin-top:4px">Presets made before v1.5.6 were measured with the old, lagging hashrate reading and voltage settings the supply ignores, so they were cleared when this version first started:</div><ul style="font-size:12px;margin:4px 0 0 18px;padding:0;line-height:1.5">';
 if(m.presets_removed)h+='<li>'+m.presets_removed+' preset'+(m.presets_removed==1?'':'s')+' moved aside'+(m.restored?' (<b>put back</b> '+hesc(updWhen(m.restored))+')':' (kept in a backup file)')+'</li>';
 h+='<li>'+(st==='applied'?'The miner was put back on its stock settings':st==='idle'?'The miner was idle and stays idle: waking it uses the stock settings':st==='already'?'The miner was already on its stock settings':st==='pending'?'Putting the miner back on its stock settings…':'The miner couldn’t be switched to stock ('+hesc(st.replace(/^failed: /,''))+'): choose Stock under Controls &amp; fan')+'</li>';
 if(m.schedule_off)h+='<li>The schedule was switched off (its rules named presets that are gone). Turn it back on in Schedule once you’ve tuned new presets.</li>';
 if(m.fan_reset)h+='<li>Auto fan went from a preset’s tuned curve back to the balanced curve.</li>';
 h+='</ul><div style="font-size:12px;margin-top:6px">Next: run the Tuner (Tuner tab) to build new presets with the finer 50 mV steps.</div>';
 if(m.presets_removed&&!m.restored)h+='<div class="row" style="margin-top:8px;gap:8px;align-items:center"><button onclick="updRestorePresets()">Put my old presets back</button><span class="muted" style="font-size:11.5px">not recommended: they were judged with the old readings. New presets of the same name are kept.</span></div>';
 return h+'</div>';}
async function updRestorePresets(){const j=await updPost({restore_presets:true});if(j)toast(j.note||'Presets put back');if(typeof loadPresets==='function')try{loadPresets();}catch(e){}}
function updWarnHTML(w){if(!w)return '';
 const inl=t=>hesc(t).replace(/\*\*([^*]+)\*\*/g,'<b>$1</b>').replace(/\x60([^\x60]+)\x60/g,'<span class="mono">$1</span>');
 let h='',ul=false;
 w.split('\n').forEach(l=>{if(/^[-*] /.test(l)){if(!ul){h+='<ul style="margin:4px 0 4px 18px;padding:0">';ul=true;}h+='<li>'+inl(l.slice(2))+'</li>';return;}
  if(ul){h+='</ul>';ul=false;}if(l.trim())h+='<div style="margin:3px 0">'+inl(l)+'</div>';});
 if(ul)h+='</ul>';
 return '<div class="note" style="margin-top:8px;border-color:var(--warn,#d4a72c);font-size:12px;line-height:1.5">'+h+'</div>';}
function updSumsHTML(s){if(!s.sha256)return '';
 let h='<div style="margin-top:8px;font-size:12px"><span class="muted">SHA-256</span> <span class="mono" style="word-break:break-all">'+hesc(s.sha256)+'</span></div>';
 if(s.sums==='github')return h+'<div style="margin-top:4px;font-size:12px"><b class="okpill">✓ matches the release’s SHA256SUMS</b> <span class="muted">(checked when it downloaded)</span></div>';
 if(s.sums==='match')return h+'<div style="margin-top:4px;font-size:12px"><b class="okpill">✓ matches the SHA256SUMS you pasted</b> <span class="muted">· it’s the published file</span></div>';
 if(s.sums==='nomatch')h+='<div style="margin-top:4px;font-size:12px"><b class="badpill">✗ not in the SHA256SUMS you pasted</b> <span class="muted">· it isn’t the file that release published (or the text is from another release). Don’t install it unless you built it yourself.</span></div>';
 return h+'<details style="margin-top:6px"'+(s.sums==='nomatch'?' open':'')+'><summary class="muted" style="font-size:12px">Check it against the release’s SHA256SUMS (optional, no internet needed)</summary>'
  +'<div class="muted" style="font-size:11.5px;margin:4px 0">The file was already checked: complete, and made for this miner. To also be sure it’s the exact file the release published, open the release’s <span class="mono">SHA256SUMS</span> on any device and paste its text (or just the line for <span class="mono">sclite-update-….tar.gz</span>) here.</div>'
  +'<textarea id="updsums" rows="3" class="mono" style="width:100%;font-size:11.5px" placeholder="e.g. 3f5a…c901  sclite-update-v1.4.9.tar.gz"></textarea>'
  +'<div class="row" style="margin-top:6px"><button onclick="updSums()">Compare</button></div></details>';}
async function updSums(){const t=($('updsums')||{}).value||'';if(!t.trim()){toast('paste the SHA256SUMS text first',true);return;}
 const j=await updPost({sums:t});if(j&&j.staged)toast(j.staged.sums==='match'?'Matches the published file':'Doesn’t match',j.staged.sums!=='match');}
function updShowLatest(l){const el=$('updlatest');if(!el)return;
 if(!l.newer){el.innerHTML='<div class="muted" style="font-size:12px;margin-top:6px">✓ Up to date: the latest release is '+hesc(l.tag)+'.</div>';return;}
 el.innerHTML='<div class="note" style="margin-top:8px"><b>'+hesc(l.name||l.tag)+'</b> <span class="muted">'+hesc((l.published||'').slice(0,10))+' · <a href="'+hesc(/^https:\/\//i.test(l.url||'')?l.url:'#')+'" target="_blank" rel="noopener">release page</a></span>'
  +updWarnHTML(l.warning)
  +(l.notes?'<details style="margin-top:6px"><summary class="muted" style="font-size:12px">What’s new</summary><pre style="white-space:pre-wrap;font-size:11.5px;max-height:18rem;overflow:auto">'+hesc(l.notes)+'</pre></details>':'')
  +(l.asset?'<div class="row" style="margin-top:8px;gap:8px"><button class="primary" onclick="updFetch()">Download '+hesc(l.tag)+'</button><span class="muted" style="font-size:11.5px">'+Math.round(l.size/1048576*10)/10+' MB · checked against the release’s SHA256SUMS before anything is installed</span></div>'
   :'<div class="muted" style="font-size:12px;margin-top:6px">This release has no update file: flash its SD image instead.</div>')+'</div>';}
async function updPost(b){const {j}=await jpost('/api/update',b);if(!j.ok){toast(j.err||'failed',true);loadUpdate();return null;}renderUpdate(j,j.latest);return j;}
async function updCheck(){$('updcheck').textContent='checking…';const {j}=await jpost('/api/update',{check:true});
 if(!j.ok){$('updcheck').textContent=j.err||'failed';return;}renderUpdate(j,j.latest);}
async function updFetch(){const el=$('updlatest');if(el)el.innerHTML='<div class="note" style="margin-top:8px">Downloading and checking… (a minute or so)</div>';
 const j=await updPost({fetch:true});if(j)toast('Downloaded and checked: ready to install');}
async function updUpload(){const f=($('updfile').files||[])[0];if(!f){$('updmsg').textContent='choose the file first';return;}
 if(f.size>48*1048576){$('updmsg').textContent='that file is too big for an update (the .img.xz is for SD cards)';return;}
 $('updmsg').textContent='uploading 0 %';
 const xhr=new XMLHttpRequest();xhr.open('POST','/api/update/upload');xhr.setRequestHeader('Content-Type','application/octet-stream');xhr.setRequestHeader('X-Requested-With','panel');
 xhr.upload.onprogress=e=>{if(e.lengthComputable)$('updmsg').textContent=e.loaded<e.total?('uploading '+Math.round(e.loaded/e.total*100)+' %'):'checking…';};
 xhr.onload=()=>{let j={};try{j=JSON.parse(xhr.responseText);}catch(e){}
  if(!j.ok){$('updmsg').textContent=j.err||('upload failed ('+xhr.status+')');return;}renderUpdate(j);toast('Checked: ready to install');};
 xhr.onerror=()=>{$('updmsg').textContent='upload failed';};xhr.send(f);}
function verLess(a,b){const p=x=>String(x||'').replace(/^v/,'').split('.').map(n=>parseInt(n,10)||0);const A=p(a),B=p(b);for(let i=0;i<3;i++){if((A[i]||0)!==(B[i]||0))return (A[i]||0)<(B[i]||0);}return false;}
async function updInstall(){const s=(window._upd||{}).staged||{},inst=(window._upd||{}).version||'';
 let wl=(s.warning||'').split('\n')[0].replace(/\*\*/g,'');
 if(inst&&verLess(inst,'1.5.12')&&!verLess(s.version,'1.5.12'))wl=(wl?wl+'\n\n':'')+'⚠ From v'+inst+': the miner may get a NEW IP address once after this update (it now identifies itself to your router by its MAC address). If the panel doesn’t come back at this address, look for the miner in your router’s list of devices.';
 if(!confirm('Install v'+s.version+' now?'+(wl?('\n\n'+wl+' (see the note above the Install button)'):'')+'\n\nThe miner saves the files it replaces, installs, and reboots (mining stops for 2–3 minutes). Your settings stay. If it doesn’t start properly, the previous version is put back by itself.'))return;
 let ib={install:true};
 if((window._upd||{}).rollback_needs_password&&s.version&&verLess(s.version,'1.5.22')){const pw=await askPassword('Versions before v1.5.22 store the password differently. Enter your password once, so v'+s.version+' can still sign you in:');if(pw===null)return;ib.password=pw;}
 if((window._upd||{}).install_needs_password&&!ib.password){const pw=await askPassword('Installing firmware needs your panel password:');if(pw===null)return;ib.password=pw;}
 const {j}=await jpost('/api/update',ib);if(!j.ok){toast(j.err||'failed',true);loadUpdate();return;}
 updProgSet(s.version?'v'+String(s.version).replace(/^v/,''):'','pkg');updProgInSettings();
 toast(j.note||'installed: rebooting');window._rebStart=Date.now();window._rebNominal=150000;if(typeof refresh==='function')setTimeout(refresh,1500);}
// the login text: what the password is stored as (SHA-256 only until the first sign-in after the update, or
// while an update waits to be confirmed)
function pwAlgo(a,waits){const el=$('pwalgo');if(!el||!a)return;el.innerHTML=a.indexOf('BLAKE2b')>=0
 ?'<b>'+hesc(a)+'</b>'
 :waits?'Passwords are stored as a <b>'+hesc(a)+'</b> hash while a firmware update waits to be confirmed (or a rollback is under way), so the older version can still read it; it becomes <b>PBKDF2-HMAC-BLAKE2b-512</b> at a sign-in after that.'
 :'Passwords are stored as a <b>'+hesc(a)+'</b> hash for now; it becomes <b>PBKDF2-HMAC-BLAKE2b-512</b> ('+(isOtherModel()||modelName()!=='SC Lite'?'BLAKE2b':'BLAKE2b, the hash the SC Lite mines')+') at the next sign-in.';}
// askPassword: a password box (not window.prompt, which shows what's typed); null when cancelled.
function askPassword(msg){return new Promise(res=>{
 $('pwmbox').innerHTML='<div class="hdr"><b>Your password</b></div><p class="muted" style="font-size:13px">'+hesc(msg)+'</p>'
  +'<input id="pwm_in" type="password" autocomplete="current-password" style="width:100%">'
  +'<div class="row" style="gap:8px;margin-top:10px"><button class="primary" id="pwm_ok">Continue</button><button id="pwm_no">Cancel</button></div>';
 const prev=document.activeElement;show('pwmodal',true);const inp=$('pwm_in');inp.focus();
 const done=v=>{show('pwmodal',false);$('pwmbox').innerHTML='';if(prev&&prev.focus)prev.focus();res(v);};
 const ok=()=>{if(!inp.value){inp.focus();return;}done(inp.value);};
 $('pwm_ok').onclick=ok;$('pwm_no').onclick=()=>done(null);
 inp.onkeydown=e=>{if(e.key==='Enter')ok();else if(e.key==='Escape')done(null);};});}
// dialogs: Escape cancels the open one (not the first-time terms, which need an answer), Tab stays inside it
document.addEventListener('keydown',e=>{const m=[...document.querySelectorAll('.modalwrap')].find(x=>!x.classList.contains('hide'));if(!m)return;
 if(e.key==='Escape'){if(m.id==='pwmodal'){const b=$('pwm_no');if(b)b.click();}else if(m.id==='ocmodal')ocClose();else if(m.id==='discmodal'&&window._ackdate)closeDisclaimer();else return;e.preventDefault();return;}
 if(e.key!=='Tab')return;const f=[...m.querySelectorAll('button,input,select,textarea,a[href]')].filter(x=>!x.disabled&&x.offsetParent);if(!f.length)return;
 const i=f.indexOf(document.activeElement);
 if(e.shiftKey&&i<=0){f[f.length-1].focus();e.preventDefault();}else if(!e.shiftKey&&(i<0||i===f.length-1)){f[0].focus();e.preventDefault();}});
async function updRollback(){if(!confirm('Roll back to the previous version?\n\nThe miner reboots and puts back the files the last update replaced as it starts (2–3 minutes). Settings stay, but what the older version doesn\'t have doesn\'t apply while it runs (for example: before v1.5.22 the public time servers, pool.ntp.org, are always asked, even when switched off in Settings → Time).'))return;
 let body={rollback:true};
 const ask=window._upd&&window._upd.rollback_needs_password;
 if(ask){const pw=await askPassword('Versions before v1.5.22 store the password differently. Enter your password once, so the older version can still sign you in:');if(pw===null)return;body.password=pw;}
 const {j}=await jpost('/api/update',body);if(!j.ok){toast(j.err||'failed',true);return;}toast(j.note||'rolling back');window._rebStart=Date.now();window._rebNominal=150000;}
async function updRepo(){const b={repo:$('updrepo').value};
 if((window._upd||{}).repo_needs_password){const pw=await askPassword('Changing where this miner gets its firmware needs your panel password:');if(pw===null)return;b.password=pw;}
 if(await updPost(b))toast('Update source saved');}
// renderImageUpdate: the in-panel update of a model with a read-only system image (commissioned: FirmwareUpdate), a whole system image
// (.ius) written to the system partition. Three steps: ① get the file; ② check it, by itself (the file, whether it is the one the
// release published, and the miner: the backend's Verify, which writes nothing); ③ install, after four confirmations.
// Each part shows a heading, one line and its controls; longer text sits in drop-downs. Dots (the tuner's ● colours): red =
// must read / blocks you, yellow = needs you, green = done.
function renderImageUpdate(u,latest){window._upd=u;const v=u.version||'',pat=u.asset_pattern||'.ius',nm=modelName();
 $('updnote').textContent=v?('v'+v):'';
 const s=u.staged||null,busy=u.busy||'',vb=!!window._imgVerifying||busy==='verifying',ver=s&&s.version?'v'+s.version:'',ch=window._imgChain,chr=!!(ch&&!ch.err&&!ch.ok);
 let h=imgMore('<b>No automatic roll back: an SD card is the recovery</b>','Updating replaces the miner’s system. The '+hesc(nm)+' keeps only one system, so if an install goes wrong, <b>an SD card with the firmware always brings the miner back</b> (the install step shows how). The bootloader and the settings storage are never written.','bad','imgrbnote')
  +'<div class="ninfo" style="margin-top:8px"><div><span class="muted">Installed</span> &nbsp;<b>'+(v?'v'+hesc(v):'unknown')+'</b></div>';
 const L=u.last;
 if(L){const good=L.result==='installed'||L.result==='written';
  h+='<div><span class="muted">Last update</span> &nbsp;<b class="'+(good?'okpill':'badpill')+'">'+hesc(L.result)+'</b> <span class="muted">'+(L.to?'v'+hesc(L.to):'')+(L.from?' (from v'+hesc(L.from)+')':'')+' · '+hesc(updWhen(L.at))+'</span>'+(L.note?'<div class="muted" style="font-size:11.5px">'+hesc(L.note)+'</div>':'')+'</div>';}
 h+='</div>';
 // the owner's commissioning build: GitHub's latest release is the PUBLIC build (no Commissioning section)
 // settings backup: export before an update, import after an SD-card recovery (the recovery steps point here)
 h+='<details style="margin-top:8px"><summary class="muted" style="font-size:12.5px"><b>Settings backup</b>: export, or import after an SD-card recovery</summary>'
  +'<div class="row" style="gap:8px;align-items:center;margin-top:6px"><button onclick="settingsExport()">Export settings (.json)</button><span class="muted" style="font-size:11.5px">network, fan curves, presets, schedule and notifications — not the pools, hostname, time settings, passwords or secrets</span></div>'
  +'<div class="row" style="gap:8px;align-items:center;margin-top:8px"><input type="file" id="setimport" accept=".json,application/json" style="max-width:20rem"><button onclick="settingsImport()">Import settings</button><span class="muted" id="setimpmsg" style="font-size:11.5px"></span></div></details>';
 if(!PUB())h+='<div class="note" style="margin-top:8px;font-size:12px"><span class="st-run">●</span> <b>This is the commissioning build.</b> “Check for updates” offers the public release, which has no Commissioning section and a fixed set of features. Install commissioning builds with <b>Upload the file</b>.</div>';
 if(busy&&busy!=='verifying')h+='<div class="note" style="margin-top:10px">'+hesc(busy)+'…</div>';
 h+=imgChainHTML();
 // ① get the file
 let b1;
 if(chr&&!s)b1='<div class="muted" style="font-size:12px;margin-top:6px">Downloading '+hesc(ch.ver)+'…</div>';
 else if(s)b1='<div style="font-size:13px;margin-top:6px">'+(ver?'Firmware <b>'+hesc(ver)+'</b>':'A firmware file <span class="muted">(its name doesn’t say which version)</span>')+' <span class="muted">· '+Math.round(s.size/1048576*10)/10+' MB'+(s.sums_from==='github'?' · downloaded from GitHub':' · uploaded')+'</span></div>'
  +'<div class="row" style="margin-top:8px;gap:8px"><button onclick="imgDiscard()">Use a different file</button></div>';
 else b1='<div class="row" style="gap:8px;margin-top:8px;align-items:center"><button class="primary" onclick="updCheck()">Check for updates</button><span class="muted" id="updcheck" style="font-size:12px"></span></div>'
  +'<div class="muted" style="font-size:11.5px;margin-top:4px">The miner looks up the latest release on GitHub (it needs internet).</div><div id="updlatest"></div>'
  +'<div style="margin-top:12px"><b style="font-size:13px">Or upload the file</b> <span class="muted" style="font-size:11.5px">the <span class="mono">'+hesc(pat)+'</span> from the release page</span>'
  +'<div class="row" style="gap:8px;align-items:center;margin-top:6px"><input type="file" id="imgfile" accept=".ius" style="max-width:20rem"><button onclick="imgUpload()">Upload</button><span class="muted" id="imgmsg" style="font-size:12px"></span></div></div>'
  +'<details style="margin-top:10px"><summary class="muted" style="font-size:12px">Update source</summary><div class="setgrid" style="margin-top:6px"><label>GitHub repository</label><input id="updrepo" class="mono" value="'+hesc(u.repo)+'" placeholder="'+hesc(u.default_repo)+'">'
  +'<div class="full"><button onclick="updRepo()">Save</button> <span class="muted" style="font-size:11.5px">owner/name, for your own build or a fork. Empty: '+hesc(u.default_repo)+'.</span></div></div></details>';
 h+=imgStep(1,'Get the update file',s?'done':(chr||busy==='downloading'||busy==='receiving')?'run':'you',b1);
 // ② check it
 const g=!!(s&&s.genuine),mism=!!(s&&s.sums==='nomatch'&&s.sums_from!=='pasted'),nr=!!(s&&s.ready==='fail'),bad=mism||nr,
  manual=!!(s&&!g&&!vb&&(s.lookup==='unavailable'||(s.sums==='nomatch'&&s.sums_from==='pasted'))&&s.sums_from!=='online');
 let b2,st2;
 if(!s){st2='wait';b2='<div class="muted" style="font-size:12px;margin-top:6px">Starts by itself once the file is here. Nothing on the miner changes.</div>';}
 else{const it=(k,t)=>'<li style="list-style:none;margin:3px 0">'+(k==='ok'?'<b class="okpill">✓</b> ':k==='bad'?imgDot('bad'):k==='you'?imgDot('you'):'<span class="muted">…</span> ')+t+'</li>';
  const tag=hesc(s.sums_tag||ver),fname=hesc(s.version?pat.replace(/v…\.ius$/,'v'+s.version+'.ius'):pat);
  let gi;
  if(g)gi=it('ok',s.sums_from==='pasted'?'Genuine: it matches the release line you pasted':'Genuine: it matches the official release'+(tag?' '+tag:''));
  else if(s.sums==='nomatch')gi=it(s.sums_from==='pasted'?'you':'bad',s.sums_from==='pasted'?'<b>This file doesn’t match the line you pasted. Don’t install it.</b> <span class="muted">Check you copied the line for this version.</span>'
   :'<b class="badpill">This file doesn’t match the official release'+(tag?' '+tag:'')+'. Don’t install it.</b> <span class="muted">Download it again, or use “Check for updates”.</span>');
  else if(vb||!s.lookup)gi=it('run','Checking that it’s the official file…');
  else gi=it('you','Not yet confirmed as the official file: one step for you below');
  let ri;
  if(s.ready==='ok')ri=it('ok','The miner is ready for it'+(s.same==='same'?' <span class="muted">(same version as installed)</span>':''));
  else if(nr){const c=(s.checks||[]).find(c=>!c.ok);ri=it('you','The miner isn’t ready for it'+(c?': <span class="muted">'+hesc(c.msg)+'</span>':''));}
  else ri=it('run',vb?'Checking the miner…':'The miner hasn’t been checked yet');
  b2='<ul style="font-size:13px;margin:8px 0 0;padding:0;line-height:1.5">'+it('ok','The file is complete')+it('ok','Made for the '+hesc(nm))+gi+ri+'</ul>';
  if(vb)b2+='<div style="margin-top:8px"><div class="rbar indet"><div class="rfill"></div></div><div class="muted" style="font-size:11.5px;margin-top:4px">About half a minute. Nothing on the miner changes.</div></div>';
  if(manual)b2+='<div class="note" style="margin-top:10px;border-color:var(--warn);font-size:12.5px;line-height:1.55"><b>One step by hand:</b> '+(s.sums==='nomatch'?'paste the line again, from the release of this version.':'copy the line for <span class="mono">'+fname+'</span> from the release’s SHA256SUMS file and paste it here.')
   +'<textarea id="imgsums" rows="2" class="mono" style="width:100%;font-size:11.5px;margin-top:6px" placeholder="paste the line here"></textarea>'
   +'<div class="row" style="margin-top:6px;gap:8px"><button class="primary" onclick="imgSums()">Check</button><button onclick="imgVerify()">Try online again</button></div>'
   +imgMore('Where do I find that line?','We couldn’t check this file online'+(s.lookup_why?' ('+hesc(s.lookup_why)+')':'')+'. On the release page, next to the download, there is a file called <b>SHA256SUMS</b>. Open it, find the line that ends with <span class="mono">'+fname+'</span>, copy it and paste it above.'
    +'<div class="muted" style="font-size:11.5px;margin-top:4px;word-break:break-all">Example: <span class="mono">9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08 &nbsp;'+fname+'</span><br>The whole file, that one line, or just the long code all work.</div>')+'</div>';
  b2+='<details style="margin-top:8px"><summary class="muted" style="font-size:12px">Show details</summary><div style="font-size:11.5px;margin-top:4px;line-height:1.5">'
   +'<div><span class="muted">The file’s SHA-256 checksum</span> <span class="mono" style="word-break:break-all">'+hesc(s.sha256)+'</span></div>'
   +'<div class="muted">'+(s.sums_from==='github'?'Compared with the SHA256SUMS of release '+tag+' when it downloaded.':s.sums_from==='online'?'Compared with the SHA256SUMS of release '+tag+' on GitHub.':s.sums_from==='pasted'?'Compared with the SHA256SUMS text you pasted.':'Not compared with a release’s SHA256SUMS yet.')+'</div>'
   +((s.checks&&s.checks.length)?'<ul style="margin:6px 0 0 18px;padding:0">'+s.checks.map(c=>'<li class="'+(c.ok?(c.warn?'':'okpill'):'badpill')+'">'+(c.ok?'✓ ':'✗ ')+hesc(c.msg)+'</li>').join('')+'</ul>':'')
   +(vb?'':'<div class="row" style="margin-top:6px"><button onclick="imgVerify()">Check again</button></div>')+'</div></details>';
  st2=vb?'run':mism?'bad':nr?'you':(g&&s.ready==='ok')?'done':manual?'you':'run';}
 h+=imgStep(2,'Check the file',st2,b2);
 // ③ install
 const can=!!(s&&g&&s.ready==='ok'&&!vb&&!busy&&!chr&&!(ch&&ch.ok));window._imgCan=can;
 let b3;
 if(!s||bad)b3='<div class="muted" style="font-size:12px;margin-top:6px">'+(mism?'Not possible with this file: it didn’t pass the check. Use a different file (step ①).':nr?'Not possible until the miner is ready (step ②).':'After the check: four quick confirmations, then Install.')+'</div>';
 else{const ack=window._imgAck&&window._imgAck.sha===s.sha256?window._imgAck:{sha:s.sha256};window._imgAck=ack;
  b3=imgConfirmHTML('imgack',ack,'imgAckUI()',{ver:s.sums_tag||ver})
   +updWarnHTML(s.warning)
   +imgPwHTML('imgpw',u)
   +'<div class="row" style="margin-top:12px;gap:8px;align-items:center"><button class="primary" id="imginst" onclick="imgInstall()" disabled>Install '+(ver?hesc(ver):'the update')+' and restart</button><span class="muted" id="imginsthint" style="font-size:12px"></span></div>'
   +'<div class="muted" style="font-size:11.5px;margin-top:6px">Mining stops for about 4 minutes. Keep the power on and this page open.</div>';}
 h+=imgStep(3,'Install',bad?'wait':can?'you':'wait',b3);
 $('updbody').innerHTML=h;if(updProgGet())updProgInSettings();
 imgAckUI();
 if(latest&&!s)imgShowLatest(latest);
 // step ② runs by itself once a file is here (Verify writes nothing), once per file
 if(s&&!busy&&!vb&&!s.ready&&!chr&&window._imgAuto!==s.sha256){window._imgAuto=s.sha256;setTimeout(imgVerify,50);}}
// imgDot: the tuner's step dot (● in its st-pass / st-run / st-fail colours): ok green (done), you yellow (needs you), bad red (must read).
function imgDot(k){return '<b class="st-'+({ok:'pass',you:'run',bad:'fail'}[k]||'todo')+'" style="font-size:13px;margin-right:6px">●</b>';}
// imgMore: a collapsed drop-down with a one-line summary (and a dot when it matters).
function imgMore(sum,body,dot,id){return '<details'+(id?' id="'+id+'"':'')+' style="margin-top:6px"><summary style="font-size:12.5px;cursor:pointer">'+(dot?imgDot(dot):'')+sum+'</summary><div style="font-size:12px;line-height:1.55;margin:6px 0 2px 16px">'+body+'</div></details>';}
// Box II: long explanations fold into a closed drop-down (imgMore, as on the update screen). The first sentence (or the
// part before "at") stays in view, with the tuner's dot where it matters. The SC Lite page never calls these.
// b2Split: [the first sentence, the rest], cut at the first ". " outside tags and brackets ("e.g." isn't an end)
function b2Split(h){h=String(h||'');let t=0,d=0;for(let i=0;i<h.length-1;i++){const c=h[i];if(c==='<')t=1;else if(c==='>')t=0;else if(!t){if(c==='('||c==='“')d++;else if(c===')'||c==='”')d--;
 else if(c==='.'&&d<=0&&h[i+1]===' '&&!/(e\.g|i\.e)$/.test(h.slice(Math.max(0,i-3),i)))return [h.slice(0,i+1),h.slice(i+2).trim()];}}return [h,''];}
// b2More: the first sentence of h (dot: imgDot's ok / you / bad before it) and the rest in a closed drop-down; null when nothing's left to fold
function b2More(h,label,dot,at){h=String(h||'');let a,b;if(at){const i=h.indexOf(at);if(i<0)return null;a=h.slice(0,i).trim();b=h.slice(i).trim();}else [a,b]=b2Split(h);
 return b?(dot?imgDot(dot):'')+a+imgMore(label,b):null;}
// b2Fold: the same for a fixed paragraph of the page (once; sel: an id or a selector; a leading ⚠ gives way to the dot)
function b2Fold(sel,label,dot,at){const e=document.getElementById(sel)||document.querySelector(sel);if(!e||e._b2f)return;
 const h=b2More(String(e.innerHTML||'').replace(dot?/^\s*⚠\s*/:/^$/,''),label,dot,at);if(h!=null){e._b2f=1;e.innerHTML=h;}}
// b2Wrap: a whole block of the page into a closed drop-down
function b2Wrap(sel,label){const e=document.querySelector(sel);if(!e||e._b2f||!e.parentNode)return;e._b2f=1;
 const d=document.createElement('details');d.style.margin='6px 0';d.innerHTML='<summary style="font-size:12.5px;cursor:pointer">'+label+'</summary>';e.parentNode.insertBefore(d,e);d.appendChild(e);}
// b2Folds (Box II, from applyModel): the page's fixed explanations
function b2Folds(){const sp=n=>'.tabpane[data-pane="schedule"] > .panel:nth-of-type('+n+') > .sc-intro';
 b2Fold('#ctrlpanel .sc-intro','Taking and handing back control');
 b2Fold('#ctrlpanel .warnbar','What keeps it in range','bad');
 b2Fold('#fanpanel .sc-intro','How auto fan works',null,'While auto fan');
 b2Fold('#fcpanel .sc-intro','How the fan check works');
 b2Fold('#curvepanel .sc-intro','How to edit a curve');
 b2Fold('#cvEdit > .thint','If a board stays hot, and the other settings','bad');
 b2Fold('p_intro','How presets are made and applied');
 b2Fold('sc_hint','How it works',null,'Each change');
 b2Fold(sp(2),'How painting works');
 b2Fold(sp(3),'How scheduled restarts work');
 b2Wrap('#rejsec .rj-key','What the marks mean');
 b2Wrap('#sharespanel > p.muted','How the record works');
 b2Fold('#companel .cm-intro','How switching on works',null,'Each one has');
 b2Fold('#tunerpanel > .warnbar','Before you start','bad','Start in DRY-RUN');}
// imgConfirmHTML: the four confirmations before an install (step ③ and the release card's one-click install), how to recover
// by SD card right under them, and why each is asked. rel: {url, ver} of the release, when known.
function imgConfirmHTML(id,ack,onch,rel){const nm=modelName();
 const cb=(n,t,extra)=>'<div style="display:flex;gap:6px 10px;align-items:center;margin-top:7px;flex-wrap:wrap"><label style="display:flex;gap:7px;align-items:flex-start;font-size:13px;line-height:1.4"><span id="'+id+'d'+n+'">'+imgDot(ack&&ack[n]?'ok':'you')+'</span><input type="checkbox" id="'+id+n+'" onchange="'+onch+'"'+(ack&&ack[n]?' checked':'')+' style="margin-top:2px"><b>'+t+'</b></label>'+(extra||'')+'</div>';
 return cb(1,'I saved my settings','<button onclick="settingsExport()">Export settings</button>')
  +cb(2,'I have an SD card with the '+hesc(nm)+' firmware ready')
  +cb(3,'The power is stable')
  +cb(4,'I understand that if the install fails, the miner only comes back with the firmware on an SD card')
  +imgRecoverHTML(rel)
  +imgMore('Why these four?','<ul style="margin:0 0 0 16px;padding:0">'
   +'<li><b>Settings:</b> if the install fails, the SD card recovery erases them.</li>'
   +'<li><b>SD card:</b> the miner keeps only one system. If writing is interrupted, the SD card is how you get it back.</li>'
   +'<li><b>Power:</b> a power cut during the 2–3 minute write leaves the miner unable to start until it’s re-flashed by SD card.</li>'
   +'<li><b>Recovery:</b> there is no automatic roll back. Best to prepare the card before you update.</li></ul>');}
// imgRecoverHTML: "How to recover with an SD card" (from docs/INSTALL.md: sections 2–5), collapsed.
function imgRecoverHTML(rel){const u=window._upd||{},ok=r=>/^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/.test(r||'');
 const repo=ok(u.repo)?u.repo:ok(u.default_repo)?u.default_repo:'';
 const br=((u.asset_pattern||'').split('-firmware')[0])||'scbox2',v=rel&&rel.ver?String(rel.ver).replace(/^v/,''):'';
 let rurl=rel&&/^https:\/\//i.test(rel.url||'')?rel.url:'';if(!rurl&&repo)rurl='https://github.com/'+repo+(v?'/releases/tag/v'+v:'/releases/latest');
 const sd=hesc(br+'-sd-v'+(v||'…')+'.img.xz');
 return imgMore('How to recover with an SD card','<ol style="margin:0 0 0 16px;padding:0">'
  +'<li>On a computer, download <span class="mono">'+sd+'</span> and <span class="mono">SHA256SUMS</span> from the '+(rurl?'<a href="'+hesc(rurl)+'" target="_blank" rel="noopener">release page</a>':'release page')+'.</li>'
  +'<li>Write it to a microSD card with <b>balenaEtcher</b> (select the .img.xz, select the card, Flash).</li>'
  +'<li>Power the miner <b>off</b>, insert the card, power it <b>on</b>.</li>'
  +'<li>While it writes, the <b>SYS LED flashes blue and red</b> (about 4 minutes). Don’t remove the card or cut the power while red is flashing.</li>'
  +'<li>When only the <b>blue</b> SYS LED flashes, the write is done: <b>remove the card</b>. The miner starts the new firmware.</li>'
  +'<li>Find the miner in your router’s list of devices, open <span class="mono">http://&lt;miner-ip&gt;/</span>, sign in with <span class="mono">admin</span> / <span class="mono">admin</span> and set your password.</li>'
  +'<li>Set your pool, hostname and other settings again, or import the settings file you exported (Settings → Firmware update).</li></ol>'
  +'<div class="muted" style="font-size:11.5px;margin-top:4px">Tip: prepare the card <b>before</b> you update, so it’s ready if you need it.'+(repo?' <a href="'+hesc('https://github.com/'+repo+'/blob/main/docs/INSTALL.md')+'" target="_blank" rel="noopener">Full instructions</a>':'')+'</div>','bad');}
// imgChainHTML: the one-click install's progress (window._imgChain: {ver, stage 0..3, err}); only on the page that clicked
function imgChainHTML(){const c=window._imgChain;if(!c)return '';
 const names=['Downloading','Checking the file','Installing','Restarting'];
 const rows=names.map((n,i)=>{const done=i<c.stage||(c.ok&&i===c.stage),cur=i===c.stage&&!c.err&&!c.ok,bad=i===c.stage&&c.err;
  return '<li style="list-style:none;margin:3px 0'+(i>c.stage?';color:var(--muted)':'')+'">'+(bad?'<b class="badpill">✗</b> ':done?'<b class="okpill">✓</b> ':cur?'<b>›</b> ':'<span class="muted">·</span> ')+n+(cur?'…':'')+'</li>';}).join('');
 return '<div class="note" id="imgchain" style="margin-top:10px;border-color:'+(c.err?'var(--bad)':'var(--border)')+'"><b>'+(c.err?'Stopped: ':'')+'Download and install '+hesc(c.ver)+'</b>'
  +'<ul style="font-size:13px;margin:6px 0 0;padding:0;line-height:1.5">'+rows+'</ul>'
  +(!c.err&&!c.ok?'<div class="rbar indet" style="margin-top:6px"><div class="rfill"></div></div><div class="muted" style="font-size:11.5px;margin-top:4px">'+['A minute or so.','About half a minute. Nothing on the miner changes yet.','Keep the power on and this page open.',''][c.stage]+'</div>':'')
  +(c.ok?'<div style="font-size:12.5px;margin-top:6px">The miner is writing the new system and restarts by itself (about 4 minutes). Keep the power on and this page open.</div>':'')
  +(c.err?'<div style="font-size:12.5px;margin-top:6px"><b class="badpill">'+hesc(c.err)+'</b> <b>Nothing was written to the miner.</b> The details are in the steps below.</div>':'')+'</div>';}
function imgChainShow(){const e=document.getElementById('imgchain');if(e){e.outerHTML=imgChainHTML();return;}if(window._upd&&window._upd.image)renderImageUpdate(window._upd);}
// imgPwHTML: the panel password, asked again for an install (the backend refuses one without it)
function imgPwHTML(id,u){if(!u||!u.install_needs_password)return '';
 return '<div style="display:flex;gap:6px 10px;align-items:center;margin-top:10px;flex-wrap:wrap"><label for="'+id+'" style="font-size:13px"><b>Your panel password</b></label>'
  +'<input id="'+id+'" type="password" autocomplete="current-password" style="max-width:16rem"><span class="muted" style="font-size:11.5px">needed to install</span></div>';}
// imgPw: the password typed for an install ('' when none is asked); null when it's asked and empty
function imgPw(id){if(!(window._upd||{}).install_needs_password)return '';const v=($(id)||{}).value||'';return v?v:null;}
function imgQAll(){return [1,2,3,4].every(n=>{const e=$('imgq'+n);return !!(e&&e.checked);});}
function imgQUI(){const b=$('imgquick');if(b)b.disabled=!imgQAll();imgDots('imgq');const h=$('imgquickhint');if(h)h.textContent=imgQAll()?'':'tick all four to continue';}
// imgQuick: one click from the release card: download, check (step ②), install. Stops before anything is written
// when a stage fails; the install itself is refused by the backend unless the file is genuine and the miner ready.
async function imgQuick(tag){if(!imgQAll()){toast('Tick all four boxes first',true);return;}if(window._imgChain&&!window._imgChain.err)return;
 const qpw=imgPw('imgqpw');if(qpw===null){toast('Enter your panel password first',true);return;}
 const c=window._imgChain={ver:tag||'the update',stage:0};window._imgAuto=null;
 const stop=(m,u)=>{c.err=m;if(u&&u.image){window._upd=u;}if(window._upd&&window._upd.image)renderImageUpdate(window._upd);else imgChainShow();};
 imgChainShow();
 let {j}=await jpost('/api/update',{fetch:true});
 if(!j||!j.ok)return stop('The download didn’t work'+(j&&j.err?': '+j.err:'')+'.');
 const s0=j.staged;if(!s0)return stop('The download didn’t leave a file to install.',j);
 window._imgAuto=s0.sha256;window._imgAck={sha:s0.sha256,1:true,2:true,3:true,4:true};c.stage=1;window._imgVerifying=true;window._upd=j;renderImageUpdate(j);
 try{({j}=await jpost('/api/update',{verify:true}));}finally{window._imgVerifying=false;}
 if(!j||!j.ok)return stop('The check couldn’t run'+(j&&j.err?': '+j.err:'')+'.');
 const s=j.staged||{};
 if(!s.genuine)return stop(s.sums==='nomatch'?'The downloaded file doesn’t match the official release. Don’t install it.':'The miner couldn’t confirm it’s the official file'+(s.lookup_why?' ('+s.lookup_why+')':'')+'.',j);
 if(s.ready!=='ok'){const f=(s.checks||[]).find(x=>!x.ok);return stop('The miner isn’t ready for it'+(f?': '+f.msg:'')+'.',j);}
 c.stage=2;window._upd=j;renderImageUpdate(j);
 ({j}=await jpost('/api/update',Object.assign({install:true,ack:true},qpw?{password:qpw}:{})));
 if(!j||!j.ok)return stop('The install didn’t start'+(j&&j.err?': '+j.err:'')+'.');
 c.stage=3;c.ok=true;imgChainShow();updProgSet(tag||'');updProgInSettings();
 toast(j.note||'writing: the miner restarts');window._rebStart=Date.now();window._rebNominal=240000;if(typeof refresh==='function')setTimeout(refresh,1500);}
function imgStep(n,title,state,body){
 const tag={done:'<b class="okpill">Done</b>',you:'<b style="color:var(--warn)">Needs you</b>',bad:'<b class="badpill">Stopped</b>',run:'<span class="muted">Working…</span>',wait:'<span class="muted">Waiting</span>'}[state]||'';
 const dot={done:'ok',you:'you',bad:'bad'}[state];
 return '<div class="imgstep" data-state="'+state+'" style="margin-top:12px;border:1px solid '+(state==='bad'?'var(--bad)':state==='you'?'var(--warn)':'var(--border)')+';border-radius:10px;padding:10px 12px">'
  +'<div style="display:flex;justify-content:space-between;align-items:center;gap:8px;flex-wrap:wrap"><b style="font-size:14px'+(state==='wait'?';color:var(--muted)':'')+'">'+(dot?imgDot(dot):'')+['①','②','③'][n-1]+' '+title+'</b><span style="font-size:12px">'+tag+'</span></div>'
  +'<div'+(state==='wait'?' style="opacity:.75"':'')+'>'+body+'</div></div>';}
function imgDots(id){[1,2,3,4].forEach(n=>{const d=document.getElementById(id+'d'+n),e=document.getElementById(id+n);if(d&&e)d.innerHTML=imgDot(e.checked?'ok':'you');});}
function imgAckAll(){return [1,2,3,4].every(n=>{const e=$('imgack'+n);return !!(e&&e.checked);});}
function imgAckUI(){const b=$('imginst');if(!b)return;const a=window._imgAck||{};
 [1,2,3,4].forEach(n=>{const e=$('imgack'+n);if(e)a[n]=!!e.checked;});
 const all=imgAckAll();b.disabled=!(window._imgCan&&all);imgDots('imgack');
 const hint=$('imginsthint');if(hint)hint.textContent=!window._imgCan?'waits for step ② to pass':all?'':'tick all four to continue';}
function imgShowLatest(l){const el=$('updlatest');if(!el)return;
 if(!l.newer&&!l.asset){el.innerHTML='<div class="muted" style="font-size:12px;margin-top:6px">✓ Up to date: the latest release is '+hesc(l.tag)+'.</div>';return;}
 el.innerHTML='<div class="note" style="margin-top:8px"><b>'+hesc(l.name||l.tag)+'</b>'+(l.newer?'':' <span class="muted">(the version installed)</span>')+' <span class="muted">'+hesc((l.published||'').slice(0,10))+' · <a href="'+hesc(/^https:\/\//i.test(l.url||'')?l.url:'#')+'" target="_blank" rel="noopener">release page</a></span>'
  +updWarnHTML(l.warning)
  +(l.notes?'<details style="margin-top:6px"><summary class="muted" style="font-size:12px">What’s new</summary><pre style="white-space:pre-wrap;font-size:11.5px;max-height:18rem;overflow:auto">'+hesc(l.notes)+'</pre></details>':'')
  +(l.asset&&l.newer?'<div style="margin-top:10px"><b style="font-size:13px">Install it in one go</b> <span class="muted" style="font-size:11.5px">'+Math.round(l.size/1048576*10)/10+' MB · download, check, install, restart</span>'
   +imgConfirmHTML('imgq',null,'imgQUI()',{url:l.url,ver:l.tag})
   +imgPwHTML('imgqpw',window._upd)
   +'<div class="row" style="margin-top:12px;gap:8px;align-items:center"><button class="primary" id="imgquick" onclick="imgQuick('+hesc(JSON.stringify(l.tag||''))+')" disabled>Download and install '+hesc(l.tag)+'</button><span class="muted" id="imgquickhint" style="font-size:12px">tick all four to continue</span></div>'
   +'<div class="muted" style="font-size:11.5px;margin-top:6px">No automatic roll back on this miner: if writing fails, the SD card brings it back.</div>'
   +'<div style="margin-top:8px"><button onclick="imgFetch()">Download only</button> <span class="muted" style="font-size:11.5px">and go through the steps yourself</span></div></div>'
  :l.asset?'<div class="row" style="margin-top:8px;gap:8px;align-items:center"><button class="primary" onclick="imgFetch()">Download '+hesc(l.tag)+'</button><span class="muted" style="font-size:11.5px">'+Math.round(l.size/1048576*10)/10+' MB · checked by itself; nothing is installed until you choose to</span></div>'
   :'<div class="muted" style="font-size:12px;margin-top:6px">This release has no firmware file for this model: use its SD image instead.</div>')+'</div>';}
async function imgFetch(){const el=$('updlatest');if(el)el.innerHTML='<div class="note" style="margin-top:8px">Downloading… (a minute or so)<div class="rbar indet" style="margin-top:6px"><div class="rfill"></div></div></div>';
 await updPost({fetch:true});}
async function imgUpload(){const f=($('imgfile').files||[])[0],m=$('imgmsg');if(!f){m.textContent='choose the file first';return;}
 if(f.size>24*1048576){m.textContent='that file is too big for a firmware file (the .img is for SD cards)';return;}
 m.textContent='uploading 0 %';
 const xhr=new XMLHttpRequest();xhr.open('POST','/api/update/upload?name='+encodeURIComponent(f.name));xhr.setRequestHeader('Content-Type','application/octet-stream');xhr.setRequestHeader('X-Requested-With','panel');
 xhr.upload.onprogress=e=>{if(e.lengthComputable)m.textContent=e.loaded<e.total?('uploading '+Math.round(e.loaded/e.total*100)+' %'):'reading the file…';};
 xhr.onload=()=>{let j={};try{j=JSON.parse(xhr.responseText);}catch(e){}
  if(!j.ok){m.textContent=j.err||('upload failed ('+xhr.status+')');return;}renderUpdate(j);};
 xhr.onerror=()=>{m.textContent='upload failed';};xhr.send(f);}
async function imgDiscard(){window._imgAck=null;window._imgAuto=null;await updPost({discard:true});}
// imgVerify: step ②, the backend's Verify (reads only: the file, the release's SHA256SUMS online, the system partition)
async function imgVerify(){if(window._imgVerifying)return;window._imgVerifying=true;
 try{if(window._upd&&window._upd.image)renderImageUpdate(window._upd);await updPost({verify:true});}finally{window._imgVerifying=false;}
 if(window._upd&&window._upd.image)renderImageUpdate(window._upd);}
async function imgSums(){const t=($('imgsums')||{}).value||'';if(!t.trim()){toast('Paste the line from SHA256SUMS first',true);return;}
 const j=await updPost({sums:t});if(j&&j.staged)toast(j.staged.genuine?'It matches: the file is genuine':'That doesn’t match this file',!j.staged.genuine);}
async function imgInstall(){const s=(window._upd||{}).staged||{};
 if(!window._imgCan){toast('Wait for step ② to pass first',true);return;}
 if(!imgAckAll()){toast('Tick all four boxes first',true);return;}
 const pw=imgPw('imgpw');if(pw===null){toast('Enter your panel password first',true);const e=$('imgpw');if(e&&e.focus)e.focus();return;}
 if(!confirm('Install '+(s.version?'v'+s.version:'the update')+' now?\n\nMining stops for about 4 minutes while the miner writes the new system (2–3 minutes) and restarts. Keep the power on.\n\nIf anything goes wrong, the SD card brings the miner back (that erases saved settings).'))return;
 const {j}=await jpost('/api/update',Object.assign({install:true,ack:true},pw?{password:pw}:{}));if(!j.ok){toast(j.err||'failed',true);loadUpdate();return;}
 updProgSet(s.version?'v'+String(s.version).replace(/^v/,''):'');updProgInSettings();
 toast(j.note||'writing: the miner restarts');window._rebStart=Date.now();window._rebNominal=240000;if(typeof refresh==='function')setTimeout(refresh,1500);}
async function loadPower(){if(featBlock('powerbody',F().PowerEstimate,'The power estimate'))return;
 try{const r=await fetch('/api/power');renderPower(await r.json());}catch(e){$('powerbody').innerHTML=busyHTML(OFFLINE);retryLater(loadPower);}}
function renderPower(p){if(isOtherModel()){renderPowerBox2(p);return;}window._powF=p.factor||0;window._powMains=p.mains_v||100;
 const when=p.cal_at?new Date(p.cal_at*1000).toLocaleString():'';
 $('powerbody').innerHTML='<div class="muted" style="font-size:11.5px">The SC Lite has no power sensor, so its watts are worked out from the clock and PV (the board voltage). A reading from a plug-in wall meter makes them match your unit; it’s used everywhere watts and J/TH are shown (Live, boards, presets, tuner).</div>'+
  '<div class="ninfo" style="margin-top:10px"><div><span class="muted">Now</span> &nbsp;'+(p.watts?('<b>'+(p.calibrated?'':'≈ ')+Math.round(p.watts)+' W</b> · '+f1(p.amps)+' A at '+p.mhz+' MHz · PV '+p.pv):'<span class="muted">shown while it’s hashing</span>')+'</div>'+
  '<div><span class="muted">Calibration</span> &nbsp;'+(p.calibrated?('<b class="okpill">'+Math.round(p.cal_watts)+' W at '+p.cal_mhz+' MHz · PV '+p.cal_pv+'</b> <span class="muted">'+hesc(when)+' · ×'+(+p.factor).toFixed(3)+'</span>'):'<b class="badpill">not calibrated</b> <span class="muted">(typical SC Lite figures)</span>')+'</div></div>'+
  '<div class="setgrid" style="margin-top:10px"><label>Mains voltage (V)</label><input id="s_mainsv" type="number" min="90" max="260" step="1" value="'+Math.round(p.mains_v||100)+'">'+
  '<div class="full"><button onclick="savePowerMains()">Save voltage</button> <span class="muted" style="font-size:11.5px">Only for amps. Japan 100 V, North America 120 V, most other places 220–240 V.</span></div>'+
  '<label>Wall meter now (W)</label><input id="s_calw" type="number" min="50" max="3000" step="1" placeholder="e.g. 905">'+
  '<div class="full"><button class="primary" onclick="calibratePower()">Calibrate</button>'+(p.calibrated?' <button onclick="clearPowerCal()">Clear calibration</button>':'')+' <span class="muted" id="s_powmsg"></span>'+
  '<div class="muted" style="font-size:11.5px;margin-top:4px">Plug the miner (only the miner) into the meter, let it hash for a few minutes at a steady setting, then enter what the meter shows. The miner’s current clock and PV are recorded with it.</div></div></div>';}
async function savePowerMains(){const v=+$('s_mainsv').value;const {j}=await jpost('/api/power',{mains_v:v});
 if(!j.ok){$('s_powmsg').textContent=j.err||'failed';return;}toast('Mains voltage saved');renderPower(j);if(typeof refresh==='function')refresh();}
async function calibratePower(){const w=+$('s_calw').value;if(!(w>0)){$('s_powmsg').textContent='enter the meter reading';return;}
 const {j}=await jpost('/api/power',{cal_watts:w});if(!j.ok){$('s_powmsg').textContent=j.err||'failed';return;}
 toast('Calibrated: '+Math.round(w)+' W at '+j.mhz+' MHz · PV '+j.pv);renderPower(j);if(typeof refresh==='function')refresh();}
async function clearPowerCal(){if(!confirm('Clear the wall-meter calibration?\n\nWatts go back to typical SC Lite figures.'))return;
 const {j}=await jpost('/api/power',{clear:true});if(j.ok){toast('Calibration cleared');renderPower(j);if(typeof refresh==='function')refresh();}}
// Box II (planBox2V): a per-setting table of wall-meter readings, Goldshell's published figure for an uncalibrated
// stock mode, nothing for any other setting (power_box2.go). The SC Lite text above is never shown here.
function pwSetting(m,v){return m+' MHz · '+boxVoltsTxt(v);}
function pwSrcText(p){return p.source==='meter'?'wall meter':(p.source==='spec'?'OEM spec ±'+(p.spec_tol_pct||5)+' %':'');}
function renderPowerBox2(p){window._powF=p.calibrated?1:0;window._powMains=p.mains_v||100;const nm=hesc(modelName());
 const cals=p.cals||[],specs=p.specs||[],seen={};let rows='';
 specs.forEach(s=>{const k=cals.find(c=>c.mhz===s.mhz&&c.v===s.v);seen[s.mhz+'/'+s.v]=1;
  rows+='<tr><td>'+hesc(b2Name(s.mode))+'</td><td>'+pwSetting(s.mhz,s.v)+'</td><td class="num">'+(k?('<b>'+Math.round(k.watts)+' W</b>'):'<span class="muted">not calibrated</span>')+'</td><td class="num">≈ '+Math.round(s.watts)+' W · '+s.ths+' TH/s</td><td>'+(k?('<span class="muted">'+hesc(new Date(k.at*1000).toLocaleString())+' · '+(k.ghs/1000).toFixed(2)+' TH/s</span> <button onclick="clearPowerCalBox2('+k.mhz+','+k.v+')">Clear</button>'):'')+'</td></tr>';});
 cals.filter(k=>!seen[k.mhz+'/'+k.v]).forEach(k=>{rows+='<tr><td>other</td><td>'+pwSetting(k.mhz,k.v)+'</td><td class="num"><b>'+Math.round(k.watts)+' W</b></td><td class="num"><span class="muted">—</span></td><td><span class="muted">'+hesc(new Date(k.at*1000).toLocaleString())+' · '+(k.ghs/1000).toFixed(2)+' TH/s</span> <button onclick="clearPowerCalBox2('+k.mhz+','+k.v+')">Clear</button></td></tr>';});
 const now=p.watts?('<b>'+(p.calibrated?'':'≈ ')+Math.round(p.watts)+' W</b>'+(p.jth?(' · '+p.jth+' J/TH'):'')+' · '+f1(p.amps)+' A at '+pwSetting(p.mhz,p.vcode)+' <span class="muted">('+pwSrcText(p)+')</span>'+(p.low_hash?('<div class="muted" style="color:var(--warn)">'+hesc(p.low_hash_note||'')+'</div>'):'')):('<span class="muted">'+hesc(p.reason||'shown while it’s hashing')+'</span>');
 $('powerbody').innerHTML='<div class="muted" style="font-size:11.5px">The '+nm+' has no power sensor, and its watts can’t be worked out from the clock and voltage.'+imgMore('How the figures work','Each setting it runs gets its own reading from a plug-in wall meter. Until a stock mode has one, it shows the OEM’s published figure for that mode (±5 %). Any other setting shows no figure until you calibrate it. J/TH is the watts ÷ the hashrate the miner reports.')+'</div>'+
  '<div class="ninfo" style="margin-top:10px"><div><span class="muted">Now</span> &nbsp;'+now+'</div></div>'+
  '<div class="ctwrap" style="margin-top:10px;border:none"><table class="ctable"><thead><tr><th>Mode</th><th>Setting</th><th class="num">Wall meter</th><th class="num">OEM spec</th><th></th></tr></thead><tbody>'+rows+'</tbody></table></div>'+
  '<div class="setgrid" style="margin-top:10px"><label>Mains voltage (V)</label><input id="s_mainsv" type="number" min="90" max="260" step="1" value="'+Math.round(p.mains_v||100)+'">'+
  '<div class="full"><button onclick="savePowerMains()">Save voltage</button> <span class="muted" style="font-size:11.5px">Only for amps. Japan 100 V, North America 120 V, most other places 220–240 V.</span></div>'+
  '<label>Wall meter now (W)</label><input id="s_calw" type="number" min="50" max="1200" step="1" placeholder="e.g. 400">'+
  '<div class="full"><button class="primary" onclick="calibratePowerBox2()">Calibrate this setting</button>'+(cals.length?' <button onclick="clearPowerCalBox2()">Clear all</button>':'')+' <span class="muted" id="s_powmsg"></span>'+
  '<div class="muted" style="font-size:11.5px;margin-top:4px">'+imgMore('How to calibrate','Plug the miner and its power supply (nothing else) into the meter, let it hash at one setting for 10 minutes, then enter what the meter shows. The clock and voltage the miner reports now are recorded with it. Do this once in each mode.',cals.length?'':'you')+'</div></div></div>';}
async function calibratePowerBox2(){const w=+$('s_calw').value;if(!(w>0)){$('s_powmsg').textContent='enter the meter reading';return;}
 const {j}=await jpost('/api/power',{cal_watts:w});if(!j.ok){$('s_powmsg').textContent=j.err||'failed';return;}
 toast('Calibrated: '+Math.round(w)+' W at '+pwSetting(j.mhz,j.vcode)+(j.note?(' · '+j.note):''));renderPowerBox2(j);if(typeof refresh==='function')refresh();}
async function clearPowerCalBox2(m,v){const one=m!=null;
 if(!confirm(one?('Clear the wall-meter reading for '+pwSetting(m,v)+'?\n\nA stock mode goes back to the OEM’s published figure; any other setting shows no figure.'):'Clear every wall-meter reading?\n\nThe stock modes go back to the OEM’s published figure; any other setting shows no figure.'))return;
 const {j}=await jpost('/api/power',one?{clear_mhz:m,clear_v:v}:{clear:true});if(j.ok){toast('Calibration cleared');renderPowerBox2(j);if(typeof refresh==='function')refresh();}}
async function saveTZ(z){const {j}=await jpost('/api/time',{zone:z});if(!j.ok){$('s_tzmsg').textContent=j.err||'failed';return;}
 toast('Time zone set to '+z+'. The miner’s own log lines switch at its next restart (Settings → Restart & reboot → Restart mining).');loadTime();}
function discHTML(ackDate){const b=
  '<h3>⚠ Use at your own risk</h3>'+
  '<p>'+(ackDate?'The terms you acknowledged when signing in.':'Please read this before you continue.')+'</p>'+
  '<h4>What this firmware does</h4>'+
  '<p>This firmware changes settings on your mining hardware: '+((isOtherModel()||modelName()!=='SC Lite')?'clock speed (MHz), voltage':'clock speed (MHz), voltage (mV), PV')+' and fan speed. The tuner, presets and fan curves change them automatically. Settings other than the manufacturer&#39;s defaults can:</p>'+
  '<ul><li>damage hash chips, hash boards, power supplies and fans, or shorten their life</li>'+
  '<li>make the hardware overheat and, in the worst case, cause a fire</li>'+
  '<li>void your warranty</li>'+
  '<li>lower or stop your hashrate and earnings</li></ul>'+
  '<h4>Automatic doesn&#39;t mean safe</h4>'+
  '<p>The tuner&#39;s tests, the presets it builds and the fan curves can be wrong, and a setting that passed a test can still fail later (a hotter room, an aging chip, a dusty fan). The safety limits lower the risk; they can&#39;t remove it. Keep the miner somewhere heat, smoke or fire would be noticed, and check on it.</p>'+
  '<h4>No warranty, no liability</h4>'+
  '<p>This software is provided &quot;as is&quot;, without warranty of any kind. Its authors and contributors take no responsibility and accept no liability for any damage to hardware or property, loss of earnings, injury or any other loss that comes from using it. You alone are responsible for your hardware and for every setting you apply with it, whether by hand or automatically.</p>'+
  '<h4>Not an official product</h4>'+
  '<p>This firmware is not affiliated with, endorsed by or supported by '+(oemLite()?'Goldshell':'the OEM (the miner’s manufacturer)')+' or any miner manufacturer. Product and model names are only used to say which hardware it works with.</p>';
 if(ackDate)return b+'<div class="mrow"><span class="muted" style="margin-right:auto;font-size:11px">Acknowledged '+hesc(ackDate)+'</span><button onclick="closeDisclaimer()">Close</button></div>';
 return b+'<div class="mrow"><button class="primary" onclick="acceptDisclaimer()">I understand — continue</button></div>';}
function showDisclaimer(ackDate){$('discbox').innerHTML=discHTML(ackDate||'');show('discmodal',true);}
function closeDisclaimer(){show('discmodal',false);}
async function acceptDisclaimer(){const {j}=await jpost('/api/acknowledge',{});window._ackdate=(j&&j.acknowledged)||'';closeDisclaimer();syncTime();startApp(window._me||{});}
// ---- settings ----
async function setBoardAuto(on){const {j}=await jpost('/api/boardauto',{on});if(!j.ok)toast(j.err||'failed',true);else toast(on?'Mining restarts by itself when a board stops hashing':'Only a notice when a board stops hashing');}
async function loadBoardAuto(){try{const r=await fetch('/api/boardauto');const j=await r.json();if(j.ok&&$('bdauto'))$('bdauto').checked=!!j.on;}catch(e){}}
async function loadSettings(){loadBoardAuto();try{const r=await fetch('/api/settings');if(r.status===401){boot();return;}const j=await r.json();
 if(!j.ok){$('acctbody').innerHTML=busyHTML(j.err||'Waiting for the settings…');retryLater(loadSettings);return;}
 window._settings=j;loadTime();loadRecovery();loadCommission();loadUpdate();loadPower();renderAccount(j.account||{});renderNetwork(j.network||{});renderDisplay(j.display||{});renderNotify(j.notify||{},j.event_labels||[]);
 $('ackinfo').textContent=window._ackdate?('Acknowledged '+window._ackdate):'not yet acknowledged';
}catch(e){$('acctbody').innerHTML=busyHTML(OFFLINE);retryLater(loadSettings);}}
function siShareJS(x){const a=Math.abs(x);
 if(a>=1e18)return (x/1e18).toFixed(2)+'E';
 if(a>=1e15)return (x/1e15).toFixed(2)+'P';
 if(a>=1e12)return (x/1e12).toFixed(2)+'T';
 if(a>=1e9)return (x/1e9).toFixed(2)+'G';
 if(a>=1e6)return (x/1e6).toFixed(2)+'M';
 if(a>=1e3)return (x/1e3).toFixed(2)+'k';
 return Math.round(x||0).toString();}
function fmtShareDisp(d){const u=window._shareUnit||'both';d=d||0;
 if(u==='datum')return siShareJS(d*4294967296);
 if(u==='miner')return siShareJS(d);
 return siShareJS(d*4294967296)+' ('+siShareJS(d)+')';}
const SHARELBL={datum:'DATUM scale',miner:"Miner's own",both:'Both'};
function renderDisplay(d){const u=(d&&d.share_unit)||window._shareUnit||'both';window._shareUnit=u;
 $('dispnote').textContent=SHARELBL[u]||'';
 // live preview from the current best share when we have it, else illustrative numbers
 const b=window._bestRaw||293601; // ~1.26M-ish illustrative fallback
 const datum=siShareJS(b*4294967296),miner=siShareJS(b);
 const rc=(v,t,num,desc)=>'<label class="rcard"><input type="radio" name="s_share" value="'+v+'"'+(u===v?' checked':'')+' onchange="saveDisplay()"><div><b>'+t+' <span style="font-family:ui-monospace,Menlo,Consolas,monospace;color:var(--muted)">'+num+'</span></b><small>'+desc+'</small></div></label>';
 const hu=(d&&d.hash_unit)||window._hrUnit||'auto';window._hrUnit=hu;
 const hrc=(v,t,num,desc)=>'<label class="rcard"><input type="radio" name="s_hash" value="'+v+'"'+(hu===v?' checked':'')+' onchange="saveHashDisplay()"><div><b>'+t+' <span style="font-family:ui-monospace,Menlo,Consolas,monospace;color:var(--muted)">'+num+'</span></b><small>'+desc+'</small></div></label>';
 $('dispbody').innerHTML='<div class="muted" style="font-size:12.5px;margin-bottom:10px;line-height:1.5">'+(isOtherModel()?'How best shares and records are shown on the dashboard, work log and notifications.'+imgMore('How the numbers are counted','The miner counts share difficulty in units of 2<sup>32</sup> hashes; DATUM and mempool count in hashes. Only the display changes — the saved history stays as it is, so you can switch any time.'):'How best shares and records are shown on the dashboard, work log and notifications. The miner counts share difficulty in units of 2<sup>32</sup> hashes; DATUM and mempool count in hashes. Only the display changes — the saved history stays as it is, so you can switch any time.')+'</div>'
  +'<div class="rcards" style="grid-template-columns:1fr">'
  +rc('datum','Like DATUM and mempool',datum,'The same scale as DATUM\'s share difficulty and the network difficulty, so you can compare them.')
  +rc('miner','The miner\'s own numbers',miner,'The number the miner itself reports.')
  +rc('both','Both',datum+' ('+miner+')','DATUM\'s scale, with the miner\'s own number after it in brackets.')
  +'</div>'
  +'<div style="border-top:1px solid var(--border);margin:18px 0 10px"></div>'
  +'<div style="font-weight:600;font-size:13px;margin-bottom:4px">Hashrate units</div>'
  +'<div class="muted" style="font-size:12.5px;margin-bottom:10px;line-height:1.5">How the hashrate is shown on the Overview tile and the hashrate chart (the chart’s axis follows this too).</div>'
  +'<div class="rcards" style="grid-template-columns:1fr">'
  +hrc('auto','Auto-scale','1.15 TH/s','Picks GH/s, TH/s or PH/s to fit the number.')
  +hrc('ths','Terahash','1.152 TH/s','Always summed to TH/s.')
  +hrc('ghs','Gigahash','1152.34 GH/s','The raw number the miner reports.')
  +hrc('both','Both','1.152 TH/s (1,152 GH/s)','TH/s with the GH/s in brackets.')
  +'</div>';}
async function saveDisplay(){const r=document.querySelector('input[name=s_share]:checked');if(!r)return;
 window._shareUnit=r.value;$('dispnote').textContent=SHARELBL[r.value]||'';
 const {j}=await jpost('/api/display',{share_unit:r.value});
 if(j.ok){renderDisplay({share_unit:r.value,hash_unit:window._hrUnit});if(typeof refresh==='function')refresh();}}
async function saveHashDisplay(){const r=document.querySelector('input[name=s_hash]:checked');if(!r)return;
 window._hrUnit=r.value;
 const {j}=await jpost('/api/display',{hash_unit:r.value});
 if(j.ok){if(typeof refresh==='function')refresh();if(typeof loadHashrate==='function')loadHashrate();}}
function renderAccount(a){$('acctbody').innerHTML='<div class="setgrid">'+
 '<label>Username</label><input id="s_user" value="'+hesc(a.username||'')+'">'+
 '<label>Current password</label><input id="s_pwcur" type="password" placeholder="needed to change the username or password" autocomplete="current-password">'+
 '<label>New password</label><input id="s_pw" type="password" placeholder="leave blank to keep current" autocomplete="new-password">'+
 '<label>Confirm password</label><input id="s_pw2" type="password" placeholder="confirm new password" autocomplete="new-password">'+
 '<div class="full muted" style="font-size:11.5px">This is also your SSH login: the same username and password sign in to SSH, so changing them here changes SSH too.</div>'+
'<div class="full" style="margin-top:4px"><button class="primary" onclick="saveAccount()">Save account</button> <span class="muted" id="s_acctmsg"></span></div></div>';}
function netPorts(n){const live=(n.listen||'').replace(/^.*:/,'')||'8080';return {live:live,saved:n.port_saved?(''+n.port_saved):live};}
function renderNetwork(n){const np=netPorts(n),port=np.saved;
 $('netbody').innerHTML='<div class="setgrid">'+
 '<label>Hostname</label><input id="s_host" value="'+hesc(n.hostname||'')+'">'+
 '<label>Web UI port</label><input id="s_port" type="number" min="1" max="65535" value="'+hesc(port)+'">'+
 (np.saved!==np.live?'<div class="full muted" style="font-size:11.5px">The panel is on port '+hesc(np.live)+' now; '+hesc(np.saved)+' is saved for the next start (enter '+hesc(np.live)+' and save to keep '+hesc(np.live)+').</div>':'')+
 '<label>MAC address</label><select id="s_macmode" onchange="macModeUI()"><option value="keep">Keep same</option><option value="random">Random each boot</option><option value="fixed">Fixed…</option></select>'+
 '<label id="s_maclbl" style="display:none">Fixed MAC</label><input id="s_mac" class="mono" placeholder="3c:2a:f4:01:02:03" style="display:none">'+
 '<div class="full muted" style="font-size:11.5px">Keep (recommended) reuses the same address at every boot, so your router keeps the same IP. MAC changes apply at the next reboot.</div>'+
 '<label>DNS server(s)</label><input id="s_dns" class="mono" placeholder="e.g. 1.1.1.1 (blank = use your router)" value="'+hesc(n.dns||'')+'">'+
 (n.dns_active&&n.dns_active.length?'<div class="full muted" style="font-size:11.5px">In use now: '+hesc(n.dns_active.join(', '))+'</div>':'')+
 '<div class="full muted" style="font-size:11.5px">Space-separated. Leave blank to use the servers your router hands out; the miner never uses a public resolver you didn\'t enter.</div>'+
 '<div class="full"><button onclick="saveNetwork()">Save network</button> <span class="muted" id="s_netmsg"></span></div></div>'+
 '<div class="ninfo" style="margin-top:12px">'+
 '<div><span class="muted">IP address</span> &nbsp;<b>'+hesc(n.ip||'–')+'</b></div>'+
 '<div><span class="muted">MAC</span> &nbsp;<span id="s_macinfo">…</span></div>'+
 '</div>'+sshBlock(n.ssh||{},n.ip)+accessBlock(n.access,netPorts(n).live);loadMAC();}
// accessBlock (when the panel sends the switches: n.access): who may reach the panel and SSH, HTTPS only, ports 80 / 443 (applied at once)
function accessBlock(a,main){if(!a)return '';
 const cb=(id,on,lbl,dis)=>'<label style="display:flex;gap:7px;align-items:flex-start;font-size:12.5px;margin-top:7px;color:var(--text)"><input type="checkbox" id="'+id+'" style="width:auto;margin:2px 0 0" onchange="saveAccess(\''+id+'\',this.checked)"'+(on?' checked':'')+(dis?' disabled':'')+'><span>'+lbl+'</span></label>';
 return '<div style="border-top:1px solid var(--border);margin-top:14px;padding-top:12px"><b style="font-size:13px">Access</b>'
  +cb('s_acc_out',a.allow_outside,'<b>Allow access from outside the local network</b>')
  +(a.allow_outside?'<div id="s_acc_warn" style="color:var(--bad);font-size:12px;line-height:1.5;margin:4px 0 0 22px">'+imgDot('bad')+'<b>On: the panel and SSH answer any address that reaches this miner</b>, the internet too if your router forwards a port to it. Only your password protects the miner then. Switch it off unless you need it.</div>'
   :'<div class="muted" style="font-size:11.5px;margin:2px 0 0 22px">Off (recommended): only your local network (and VPNs on private addresses) reach the panel and SSH. Other addresses get a short refusal.</div>')
  +cb('s_acc_https',a.https_only,'<b>HTTPS only</b> <span class="muted">plain http is sent to https (the miner’s own certificate: the browser warns once)</span>')
  +cb('s_acc_80',a.port80,'<b>Port 80</b> <span class="muted">http://&lt;miner-ip&gt;/ without a port number</span>',!a.port80_available)
  +cb('s_acc_443',a.port443,'<b>Port 443</b> <span class="muted">https://&lt;miner-ip&gt;/</span>',!a.port443_available)
  +'<div class="muted" style="font-size:11.5px;margin-top:6px">The main port ('+hesc(main||'8080')+') always stays on. Changes apply at once. <span id="s_accmsg"></span></div></div>';}
async function saveAccess(id,on){const k={s_acc_out:'allow_outside',s_acc_https:'https_only',s_acc_80:'port80',s_acc_443:'port443'}[id],e=$(id);if(!k)return;
 if(k==='allow_outside'&&on&&!confirm('Allow access from outside the local network?\n\nThe panel and SSH then answer any address that can reach this miner (the internet too, if your router forwards a port to it). Only your password protects it then.')){if(e)e.checked=false;return;}
 if(k==='https_only'&&on&&!confirm('HTTPS only?\n\nhttp:// addresses are then sent to https://. The miner’s certificate is its own (self-signed): your browser warns once, accept it to go on.')){if(e)e.checked=false;return;}
 const b={};b[k]=on;
 if(k==='allow_outside'&&on){const pw=await askPassword('Allowing access from outside the local network needs your panel password:');if(pw===null){if(e)e.checked=false;return;}b.password=pw;}
 const {j}=await jpost('/api/net',b);
 if(!j.ok){toast(j.err||'failed',true);if(e)e.checked=!on;return;}toast(j.note||'saved');setTimeout(()=>reloadSettingsCard('net','s_accmsg'),400);}
function sshBlock(x,ip){const u=x.username||'admin',st=!x.available?'<b class="badpill">not available</b>':!x.enabled?'<b class="badpill">off</b>':x.listening?'<b class="okpill">on · port '+x.port+'</b>':'<b class="badpill">not running</b>';
 const cmd='ssh '+(x.port&&x.port!==22?'-p '+x.port+' ':'')+hesc(u)+'@'+hesc(ip||'<miner-ip>');
 return '<div style="border-top:1px solid var(--border);margin-top:14px;padding-top:12px"><b style="font-size:13px">SSH</b> &nbsp;'+st+
  '<div class="muted" style="font-size:11.5px;margin-top:4px">Signs in with your panel username and password (a root shell, plus <code>sftp</code> and <code>scp</code>). '+
  (isOtherModel()?imgMore('Password and wrong tries','Change the password under Account and SSH follows. Five wrong passwords block that address for 10 minutes.'):'Change the password under Account and SSH follows. Five wrong passwords block that address for 10 minutes.')+'</div>'+
  (x.enabled&&x.listening?'<div style="margin-top:8px"><code>'+cmd+'</code></div>':'')+
  (x.err?'<div class="note bad" style="margin-top:8px">'+hesc(x.err)+'</div>':'')+
  (x.fingerprint?'<div class="muted" style="font-size:11.5px;margin-top:6px">Host key: <code style="word-break:break-all">'+hesc(x.fingerprint)+'</code> (your SSH client shows this the first time; it should match)</div>':'')+
  '<div class="setgrid" style="margin-top:10px"><label>SSH</label><label style="font-size:12.5px;color:var(--text);display:flex;gap:7px;align-items:center"><input type="checkbox" id="s_sshon" style="width:auto;margin:0"'+(x.enabled?' checked':'')+(x.available?'':' disabled')+'> on</label>'+
  '<label>SSH port</label><input id="s_sshport" type="number" min="1" max="65535" value="'+(x.port||22)+'"'+(x.available?'':' disabled')+'>'+
  '<div class="full"><button onclick="saveSSH()"'+(x.available?'':' disabled')+'>Save SSH</button> <span class="muted" id="s_sshmsg"></span></div></div></div>';}
async function saveNetwork(){const h=$('s_host').value.trim(),p=+$('s_port').value;
 const payload={};if(h)payload.hostname=h;
 const curPort=netPorts(((window._settings||{}).network)||{}).saved; // what the panel will use from the next start
 if(p&&(''+p)!==curPort)payload.port=p;
 const curDns=(((window._settings||{}).network)||{}).dns||'',dnsEl=$('s_dns'),dns=(dnsEl&&dnsEl.value!=null?dnsEl.value.trim():curDns);
 if(dns!==curDns)payload.dns=dns; // absent = unchanged; "" = clear; else set (server validates, refuses 114.114.114.114)
 if(payload.port&&!confirm('Change the web-UI port to '+payload.port+'?\n\nIt applies after a reboot (or panel restart) — you will then open the UI at :'+payload.port+'. Make sure you can still reach the miner.'))return; // before anything is saved
 const mr=await saveMAC();if(mr&&mr.err){$('s_netmsg').textContent=mr.err;return;}
 if(!payload.hostname&&!payload.port&&payload.dns===undefined){$('s_netmsg').textContent=mr?mr.note:'nothing changed';if(mr)toast(mr.note);return;}
 const {j}=await jpost('/api/net',payload);
 $('s_netmsg').textContent=(j.ok?(j.note||'saved'):(j.err||'failed'))+(mr?' · '+mr.note:'');if(j.ok&&payload.hostname)setTimeout(()=>reloadSettingsCard('net','s_netmsg'),700);}
// reloadSettingsCard: redraw one Settings card from the server (not all of them, which would drop what's typed in
// the others), keeping its message
async function reloadSettingsCard(which,msgId){const keep=$(msgId)?$(msgId).textContent:'';
 try{const r=await fetch('/api/settings');const j=await r.json();if(!j.ok)return;window._settings=j;
  if(which==='net')renderNetwork(j.network||{});else renderNotify(j.notify||{},j.event_labels||[]);
  if($(msgId))$(msgId).textContent=keep;}catch(e){}}
async function saveSSH(){const on=$('s_sshon').checked,p=+$('s_sshport').value;
 if(!(p>=1&&p<=65535)){$('s_sshmsg').textContent='port must be 1–65535';return;}
 if(!on&&!confirm('Turn SSH off?\n\nThe control panel keeps working; you can turn SSH back on here.'))return;
 const {j}=await jpost('/api/ssh',{enabled:on,port:p});
 if(!j.ok){$('s_sshmsg').textContent=j.err||'failed';return;}
 $('s_sshmsg').textContent=j.err?('saved, but: '+j.err):(j.enabled?('on · port '+j.port):'off');setTimeout(()=>reloadSettingsCard('net','s_sshmsg'),600);}
async function softRestart(){if(!confirm('Restart mining?\n\nOn this firmware '+oemPoss()+' restart reboots the whole miner (the panel too): it goes offline for 1–2 minutes and mining stops until it is back.'))return;
 window._rebStart=Date.now();window._rebNominal=150000;refresh();$('powmsg').textContent='restarting…';const {j}=await jpost('/api/restart',{});$('powmsg').textContent=j.ok?(j.note||'restarting'):(j.err||'failed');}
async function fullReboot(){if(!confirm('REBOOT the whole miner?\n\nIt goes offline for 1–2 minutes and all mining stops until it is back.'))return;
 window._rebStart=Date.now();window._rebNominal=150000;refresh();$('powmsg').textContent='rebooting…';const {j}=await jpost('/api/reboot',{});$('powmsg').textContent=j.ok?(j.note||'rebooting'):(j.err||'failed');}
function renderNotify(c,labels){window._evlabels=labels;const ev=c.events||{};
 const line=(lab,inner)=>'<div class="setgrid" style="margin-bottom:6px"><label>'+lab+'</label><div>'+inner+'</div></div>';
 let evh='';labels.forEach(e=>{evh+='<label><input type="checkbox" class="s_ev" data-k="'+e.key+'"'+(ev[e.key]?' checked':'')+'><span class="em">'+hesc(e.emoji||'')+'</span><span>'+hesc(e.label)+'</span></label>';});
 $('notifybody').innerHTML=
  '<div class="nchan"><label style="font-weight:600;display:flex;gap:7px;align-items:center"><input type="checkbox" id="s_tgon" style="width:auto;margin:0"'+(c.telegram_on?' checked':'')+'>Telegram</label>'+
    '<div class="setgrid" style="margin-top:8px"><label>Bot token</label><input id="s_tgtok" placeholder="'+(c.telegram_set?hesc(c.telegram_hint)+' (blank: keep · - : remove)':'123456:ABC…')+'">'+
    '<label>Chat ID</label><input id="s_tgchat" value="'+hesc(c.telegram_chat||'')+'" placeholder="123456789">'+
    '<div class="full"><button onclick="testNotify(\'telegram\')">Send a test</button></div></div>'+
    '<p class="muted" style="font-size:11px;margin:6px 0 0">In Telegram: message @BotFather → /newbot to get a token. Then message your bot, and get your chat ID (e.g. from @userinfobot).</p></div>'+
  '<div class="nchan"><label style="font-weight:600;display:flex;gap:7px;align-items:center"><input type="checkbox" id="s_dcon" style="width:auto;margin:0"'+(c.discord_on?' checked':'')+'>Discord</label>'+
    '<div class="setgrid" style="margin-top:8px"><label>Webhook URL</label><input id="s_dcurl" placeholder="'+(c.discord_set?hesc(c.discord_hint)+' (blank: keep · - : remove)':'https://discord.com/api/webhooks/…')+'">'+
    '<div class="full"><button onclick="testNotify(\'discord\')">Send a test</button></div></div>'+
    '<p class="muted" style="font-size:11px;margin:6px 0 0">In Discord: Channel → Edit → Integrations → Webhooks → New Webhook → Copy URL.</p></div>'+
  '<h4 style="margin:14px 0 2px;font-size:13px">On your own network</h4><p class="muted" style="font-size:11px;margin:0 0 6px">For miners kept off the internet: these send to a server on your network (plain http works; nothing leaves your network). Emojis show as usual.</p>'+
  '<div class="nchan"><label style="font-weight:600;display:flex;gap:7px;align-items:center"><input type="checkbox" id="s_ntfyon" style="width:auto;margin:0"'+(c.ntfy_on?' checked':'')+'>ntfy</label>'+
    '<div class="setgrid" style="margin-top:8px"><label>Server</label><input id="s_ntfyurl" value="'+hesc(c.ntfy_url||'')+'" placeholder="http://192.168.1.20:8080">'+
    '<label>Topic</label><input id="s_ntfytopic" value="'+hesc(c.ntfy_topic||'')+'" placeholder="miners">'+
    '<label>Access token</label><input id="s_ntfytok" placeholder="'+(c.ntfy_token_set?hesc(c.ntfy_token_hint)+' (blank: keep · - : remove)':'optional, only if your server needs a login')+'">'+
    '<div class="full"><button onclick="testNotify(\'ntfy\')">Send a test</button></div></div>'+
    '<p class="muted" style="font-size:11px;margin:6px 0 0">Run ntfy on a PC or Pi on your network, subscribe to the topic in the ntfy phone app (add your server under Settings). Problems arrive as high priority, records and the daily summary as low.</p></div>'+
  '<div class="nchan"><label style="font-weight:600;display:flex;gap:7px;align-items:center"><input type="checkbox" id="s_goton" style="width:auto;margin:0"'+(c.gotify_on?' checked':'')+'>Gotify</label>'+
    '<div class="setgrid" style="margin-top:8px"><label>Server</label><input id="s_goturl" value="'+hesc(c.gotify_url||'')+'" placeholder="http://192.168.1.20:8070">'+
    '<label>App token</label><input id="s_gottok" placeholder="'+(c.gotify_set?hesc(c.gotify_hint)+' (blank: keep · - : remove)':'from Gotify → Apps → Create application')+'">'+
    '<div class="full"><button onclick="testNotify(\'gotify\')">Send a test</button></div></div>'+
    '<p class="muted" style="font-size:11px;margin:6px 0 0">In Gotify’s web page: Apps → Create application → copy its token. The Gotify Android app shows the messages.</p></div>'+
  '<div class="nchan"><label style="font-weight:600;display:flex;gap:7px;align-items:center"><input type="checkbox" id="s_whon" style="width:auto;margin:0"'+(c.webhook_on?' checked':'')+'>Webhook</label>'+
    '<div class="setgrid" style="margin-top:8px"><label>URL</label><input id="s_whurl" placeholder="'+(c.webhook_set?hesc(c.webhook_hint)+' (blank: keep · - : remove)':'http://homeassistant.local:8123/api/webhook/miner')+'">'+
    '<div class="full"><button onclick="testNotify(\'webhook\')">Send a test</button></div></div>'+
    '<p class="muted" style="font-size:11px;margin:6px 0 0">Sends a JSON POST (miner, ip, event, emoji, message, text, priority, time) to Home Assistant, Node-RED or your own script.</p></div>'+
  '<h4 style="margin:12px 0 2px;font-size:13px">What to send</h4><div class="evlist">'+evh+'</div>'+
  '<div class="setgrid">'+
    '<label>Too hot at (°C)</label><input type="number" id="s_hotc" value="'+(c.hot_c??82)+'">'+
    '<label>Hashrate low under (%)</label><input type="number" id="s_lowpct" value="'+(c.hash_low_pct??70)+'">'+
    '<label style="align-self:start;padding-top:7px">Min gap per alert (min)</label><div><input type="number" id="s_interval" value="'+(c.min_interval_min||30)+'"><div class="muted" style="font-size:11px;margin-top:3px">How long the same alert waits before repeating. New best-share records don’t wait for this: each is sent as it happens (at most one every 2 minutes).</div></div>'+
    '<div class="full" style="margin-top:4px"><button class="primary" onclick="saveNotify()">Save notifications</button> <span class="muted" id="s_notifymsg"></span></div>'+
  '</div>'+
  (function(){const h=(window._settings&&window._settings.notify_log)||[];if(!h.length)return '';
    let r='<h4 style="margin:14px 0 2px;font-size:13px">Recent messages</h4><div style="font-size:12px;line-height:1.7">';
    h.slice().reverse().forEach(m=>{const ok=/✓/.test(m.result)&&!/✗/.test(m.result);r+='<div><span class="muted">'+hesc(m.t)+'</span> '+hesc(m.text)+' <span class="'+(ok?'okpill':'badpill')+'">'+hesc(m.result)+'</span></div>';});
    return r+'</div>';})();}
async function saveAccount(){const u=$('s_user').value.trim(),pw=$('s_pw').value,pw2=$('s_pw2').value;
 if(pw&&pw!==pw2){$('s_acctmsg').textContent='passwords do not match';return;}
 const {j}=await jpost('/api/passwd',{username:u,new:pw,current:$('s_pwcur').value});
 if(!j.ok){$('s_acctmsg').textContent=j.error||'failed';return;}
 $('s_acctmsg').textContent='saved · panel and SSH both use it now';$('s_pw').value='';$('s_pw2').value='';$('s_pwcur').value='';
 if(window._me)window._me.username=u||window._me.username;$('hdrright').innerHTML='<span class="muted">'+hesc((window._me&&window._me.username)||'')+'</span> <button onclick="doLogout()">Logout</button>';}
async function saveNotify(){const ev={};document.querySelectorAll('.s_ev').forEach(c=>ev[c.dataset.k]=c.checked);
 const cfg={telegram_on:$('s_tgon').checked,telegram_token:$('s_tgtok').value.trim(),telegram_chat:$('s_tgchat').value.trim(),
   discord_on:$('s_dcon').checked,discord_url:$('s_dcurl').value.trim(),events:ev,
   ntfy_on:$('s_ntfyon').checked,ntfy_url:$('s_ntfyurl').value.trim(),ntfy_topic:$('s_ntfytopic').value.trim(),ntfy_token:$('s_ntfytok').value.trim(),
   gotify_on:$('s_goton').checked,gotify_url:$('s_goturl').value.trim(),gotify_token:$('s_gottok').value.trim(),
   webhook_on:$('s_whon').checked,webhook_url:$('s_whurl').value.trim(),
   hot_c:+$('s_hotc').value,hash_low_pct:+$('s_lowpct').value,min_interval_min:+$('s_interval').value};
 const {j}=await jpost('/api/settings',cfg);
 if(j.ok){$('s_notifymsg').textContent='saved';setTimeout(()=>reloadSettingsCard('notify','s_notifymsg'),500);}else $('s_notifymsg').textContent=j.err||'failed';}
async function testNotify(ch){const {j}=await jpost('/api/notify/test',{channel:ch});
 toast(j.ok?('Test sent to '+ch+'.'):('Test failed: '+(j.err||'')),!j.ok);}
function showAuth(mode){show('auth',true);show('app',false);$('hdrright').textContent='';$('tabs').classList.add('hide');
 show('loginbox',mode==='login');show('pwbox',mode==='change');
 if($('mpwrap'))$('mpwrap').classList.toggle('hide',!(mode==='change'&&window._needMP));
 $('authtitle').textContent=mode==='login'?'Sign in':'Change password';}
async function doLogin(){$('loginerr').textContent='';
 const {s,j}=await jpost('/api/login',{username:$('u').value,password:$('p').value});
 if(s!==200||!j.ok){$('loginerr').textContent=j.error||'login failed';return;}
 $('p').value=''; // signed in: the password doesn't stay in the page
 if(j.must_change){try{const r=await fetch('/api/me');const m=await r.json();window._needMP=!m.miner_pass_set;}catch(e){}showAuth('change');return;} boot();}
async function doChange(){$('pwerr').textContent='';
 if($('np').value!==$('np2').value){$('pwerr').textContent='passwords do not match';return;}
 const mp=$('nmp')?$('nmp').value:'';
 const {s,j}=await jpost('/api/passwd',{new:$('np').value,username:$('nu').value,miner_pass:mp});
 if(s!==200||!j.ok){$('pwerr').textContent=j.error||'could not set password';return;} boot();}
async function doLogout(){await jpost('/api/logout',{});if(es){es.close();es=null;}clearInterval(window._iv);clearInterval(window._cv);window._iv=window._cv=null;boot();}

async function startApp(m){show('auth',false);show('app',true);$('tabs').classList.remove('hide');
 await loadModel(); // pick up the model profile first, so feature gates/limits apply before anything renders
 let st='dash';try{st=localStorage.getItem('sclite-tab')||'dash';}catch(e){}if(!$('tabs').querySelector('[data-tab="'+st+'"]')||$('tabs').querySelector('[data-tab="'+st+'"]').classList.contains('hide'))st='dash';showTab(st);
 $('hdrright').innerHTML='<span class="muted">'+hesc(m.username||'')+'</span> <button onclick="doLogout()">Logout</button>';
 refresh();refreshChips();loadVerbose();loadControl();loadFan();loadTuner();loadHashrate();loadPools();loadPresets();start();
 // (a page in a background tab doesn't poll: the miner's CPU mines; it catches up the moment it's shown again)
 if(window._iv)clearInterval(window._iv);window._iv=setInterval(()=>{if(document.hidden)return;refresh();pollFanStat();loadHashrate();},8000); // (the Log tab's Pause pauses only the log)
 if(window._cv)clearInterval(window._cv);window._cv=setInterval(()=>{if(!document.hidden)refreshChips();},10000);}
document.addEventListener('visibilitychange',()=>{if(!document.hidden&&window._iv){refresh();pollFanStat();loadHashrate();refreshChips();}});

// ---- chip map (per-board grid, shade-by, hover tooltip, full list) ----
function cmMetric(){return ($('cmmetric')||{}).value||'hw';}
function rampIdx(r){return Math.min(5,Math.max(0,Math.ceil(r*6)-1));}
function cmShade(c,m,ctx){
 if(m==='dtfs')return [c.slowed?1:0, ''];
 if(m==='perf'){if(!c.perf_rel)return[0,''];const dd=1-c.perf_rel;if(dd<=0)return[0,''];return[Math.min(1,dd/0.5), c.perf_rel<0.8?Math.round(c.perf_rel*100):''];}
 if(m==='hwrate'){const hrs=ctx.hrs||0;if(!c.hwerr||hrs<0.05)return[0,''];const v=c.hwerr/hrs,r=Math.log(v+1)/Math.log(ctx.maxRate+1);
  return[Math.min(1,r),(v>=0.6*ctx.maxRate&&v>=1)?(v>=10?String(Math.round(v)):v.toFixed(1)):''];}
 if(!c.hwerr)return[0,''];const r=Math.log(c.hwerr+1)/Math.log(ctx.maxHw+1);return[r,(c.hwerr>=0.6*ctx.maxHw&&c.hwerr>=10)?String(c.hwerr):''];
}
async function refreshChips(){try{const r=await fetch('/api/chips');if(r.status===401){boot();return;}const d=await r.json();
 if(!d.ok){$('chips').innerHTML=busyHTML(d.err||'Waiting for the miner’s chip report…',d.detail);$('cmnote').textContent='';$('cmcap').textContent='';window._chipdata=null;return;}
 window._chipdata=d;wireChipTip();renderChipMap();
}catch(e){if(!window._chipdata)$('chips').innerHTML=busyHTML(OFFLINE);}}
function renderChipMap(){renderChipMap0();const d=window._chipdata;if(!d||!d.stale||!$('chips'))return;
 const ago=d.stale_s<90?d.stale_s+' s':Math.round(d.stale_s/60)+' min';
 $('chips').insertAdjacentHTML('afterbegin','<div class="stalenote"><div class="rbar indet"><div class="rfill"></div></div><span>'+hesc(d.stale)+' Showing the reading from '+ago+' ago.</span></div>');}
function renderChipMap0(){const d=window._chipdata;if(!d)return;const m=cmMetric();
 const all=(d.boards||[]).flatMap(b=>b.chips||[]);
 const ctx={maxHw:Math.max(10,...all.map(c=>c.hwerr||0)),
  // errors per hour since the board started mining (its uptime); scale floor 1/h so a quiet miner stays pale
  maxRate:Math.max(1,...(d.boards||[]).flatMap(b=>{const hh=(b.uptime_s||0)/3600;return hh>=0.05?(b.chips||[]).map(c=>(c.hwerr||0)/hh):[];}))};
 const weak=[],watch=[];let shown=0,h='',bh='';
 (d.boards||[]).forEach(b=>{if(!b.active)return;shown++;
   let sub=[];if(b.clock_mhz)sub.push(b.clock_mhz+' MHz');if(b.volt_mv)sub.push((b.volt_mv/1000).toFixed(2)+' V');
   let temps='';if(b.temps&&b.temps.length){const tt=tempParts(b.temps);temps='chips avg '+tt.chips+' · board '+tt.board;}
   bh+='<div><b>Board '+b.id+'</b><br>'+(b.chips||[]).length+' chips'+(sub.length?(' · '+sub.join(' · ')):'')+(temps?('<br>'+temps):'')+'</div>';
   const n=(b.chips||[]).length;
   h+='<div class="board-row"><div class="board-label">Board '+b.id+'<span class="muted">'+(m==='hwrate'&&b.uptime_s>=180?((b.hw_total||0)/(b.uptime_s/3600)).toFixed(1)+' errors/h':(b.hw_total||0)+' errors')+(b.nslowed?(' · '+b.nslowed+' slowed'):'')+'</span></div><div class="tcells" style="--cols:'+n+';--half:'+Math.ceil(n/2)+'">';
   ctx.hrs=(b.uptime_s||0)/3600;
   (b.chips||[]).forEach(c=>{const sh=cmShade(c,m,ctx),rr=sh[0],txt=sh[1];let cls='cell',st='';
     if(c.health==='weak'){cls+=' over';weak.push('b'+b.id+'c'+c.index);}
     else{if(rr>0){const i=rampIdx(rr);st='background:var(--ramp-'+i+')';if(i>=3)cls+=' strong';}
          if(c.health==='watch'){cls+=' ring';watch.push('b'+b.id+'c'+c.index);}}
     h+='<div class="'+cls.replace('cell','cell tcell')+'" style="'+st+'" data-k="'+b.id+','+c.index+'">'+(c.health==='weak'?(txt||'!'):txt)+'</div>';});
   h+='</div></div>';});
 if(shown===0)h='<span class="muted">no active boards</span>';
 $('chips').innerHTML=h;$('cmbh').innerHTML=bh;
 const what=m==='perf'?'how far each chip is below its board typical chip (normal scatter left blank)':m==='dtfs'?'whether the driver has slowed the chip (DTFS)':m==='hwrate'?'each chip HW errors per hour since its board started mining (comparable between boards and between runs of any length)':'each chip HW errors';
 let cap='Each square is a chip, shaded by '+what+'. ';
 if(weak.length)cap+='Weak: '+weak.join(', ')+'. ';
 if(watch.length)cap+='Watch: '+watch.slice(0,6).join(', ')+(watch.length>6?(' +'+(watch.length-6)+' more'):'')+'. ';
 if(!weak.length&&!watch.length)cap+='Every chip looks healthy. ';
 const up=(d.boards||[]).map(b=>b.uptime_s||0).filter(Boolean);
 if(up.length)cap+='Boards up '+(Math.max.apply(null,up)/3600).toFixed(1)+' h. ';
 cap+='Hover a chip for its numbers.';
 $('cmcap').textContent=cap;
 $('cmnote').textContent=weak.length?(weak.length+' weak'):watch.length?(watch.length+' to watch'):'all healthy';
 const steps=[0,1,2,3,4,5].map(i=>'<span class="sw" style="background:var(--ramp-'+i+')"></span>').join('');
 const lo=m==='perf'?'typical':m==='dtfs'?'normal':'none',hi=m==='perf'?'half the work':m==='dtfs'?'slowed':m==='hwrate'?((ctx.maxRate>=10?Math.round(ctx.maxRate):ctx.maxRate.toFixed(1))+' err/h'):(ctx.maxHw+' err');
 $('cmleg').innerHTML='<span><span class="sw" style="background:var(--cell-empty);box-shadow:inset 0 0 0 1px var(--cell-edge)"></span>'+lo+'</span>'+
   '<span><span class="ramp">'+steps+'</span>'+hi+'</span>'+
   '<span><span class="sw" style="background:var(--ramp-2);box-shadow:0 0 0 2px var(--text)"></span>watch</span>'+
   '<span><span class="sw" style="background:var(--over)"></span>weak</span>';
 $('cmsum').textContent='Full chip list ('+all.length+' chips · click a heading to sort)';
 renderChipTable();
}
function chipTipHtml(k){const d=window._chipdata;if(!d)return'';const p=k.split(',').map(Number),bn=p[0],cn=p[1];
 const b=(d.boards||[]).find(x=>x.id===bn);const c=b&&(b.chips||[]).find(x=>x.index===cn);if(!c)return'';
 const hrs=(b.uptime_s||0)/3600;
 let s='Board '+bn+' · chip '+cn+'<br><b>'+c.hwerr+'</b> HW error'+(c.hwerr===1?'':'s')+(hrs>0.05?(' ('+(c.hwerr/hrs).toFixed(1)+'/hour)'):'');
 if(c.perf_rel)s+='<br>perf <b>'+c.perf+'</b> · '+Math.round(c.perf_rel*100)+'% of the board typical ('+(b.typical_perf||'?')+')';
 else s+='<br>perf '+c.perf;
 s+=' · '+c.bist+' cores';
 if(c.health==='weak')s+='<br><span style="color:var(--bad)">✕ weak — '+hesc(c.why||'')+'</span>';
 else if(c.health==='watch')s+='<br><span style="color:var(--text)">▲ watch — '+hesc(c.why||'')+'</span>';
 else s+='<br><span style="color:var(--ok)">✓ healthy</span>'+(c.why?('<br><span class="muted">'+hesc(c.why)+'</span>'):'');
 return s;}
function wireChipTip(){if(window._chiptipwired)return;window._chiptipwired=true;
 const tip=document.createElement('div');tip.className='cmtip';tip.id='cmtip';document.body.appendChild(tip);
 const host=$('chips');if(!host)return;
 let _tipk='';
 host.addEventListener('mousemove',e=>{const c=e.target.closest('.cell');if(!c){tip.style.display='none';_tipk='';return;}
   // anchor to the chip CELL (not the cursor) so every chip's tooltip sits in the same spot:
   // centred, just above the cell; flip below only if there isn't room above.
   if(c.dataset.k!==_tipk){tip.innerHTML=chipTipHtml(c.dataset.k);_tipk=c.dataset.k;}
   tip.style.display='block';
   const r=c.getBoundingClientRect(),tw=tip.offsetWidth,th=tip.offsetHeight;
   let x=r.left+r.width/2-tw/2;x=Math.max(8,Math.min(x,innerWidth-tw-8));
   let y=r.top-th-8;if(y<8)y=r.bottom+8;
   tip.style.left=x.toFixed(0)+'px';tip.style.top=y.toFixed(0)+'px';});
 host.addEventListener('mouseleave',()=>{tip.style.display='none';});}
var _chipsort='h';
var _chipAll=false;
function renderChipTable(){const d=window._chipdata;if(!d)return;const rank={weak:2,watch:1};
 let rows=[];(d.boards||[]).forEach(b=>{const hrs=(b.uptime_s||0)/3600;(b.chips||[]).forEach(c=>{
   rows.push({b:b.id,c:c.index,e:c.hwerr,r:hrs>0.05?c.hwerr/hrs:null,pr:c.perf_rel||0,perf:c.perf,hs:c.health||'ok',why:c.why,sl:c.slowed,h:rank[c.health]||0});});});
 const total=rows.length;
 const probs=rows.filter(r=>r.hs!=='ok'||r.sl);
 const show=_chipAll?rows:(probs.length?probs:rows);
 const k=_chipsort;
 show.sort((x,y)=>k==='b'?(x.b-y.b||x.c-y.c):k==='c'?(x.c-y.c||x.b-y.b):k==='pr'?((x.pr||9)-(y.pr||9)):(((y[k]||0)-(x[k]||0))||((y.e||0)-(x.e||0))));
 const HL={weak:'weak',watch:'watch',ok:''};
 const note=probs.length?(probs.length+' chip'+(probs.length===1?'':'s')+' flagged of '+total):('all '+total+' healthy');
 const toggle=(total>probs.length&&probs.length>0)?('<button onclick="_chipAll=!_chipAll;renderChipTable()" style="margin-left:auto;font-size:11.5px;padding:3px 10px">'+(_chipAll?'Flagged only':('Show all '+total))+'</button>'):'';
 let h='<div class="row" style="margin:2px 0 4px;align-items:center"><span class="muted" style="font-size:12px">'+note+(_chipAll?' · showing all':(probs.length?' · showing flagged':''))+'</span>'+toggle+'</div>';
 h+='<div class="ctwrap"><table class="ctable"><thead><tr><th data-k="b">Board</th><th class="num" data-k="c">Chip</th><th class="num" data-k="e">HW err</th><th class="num" data-k="r">/hour</th><th class="num" data-k="pr">Perf</th><th data-k="h">Health</th></tr></thead><tbody>';
 show.forEach(r=>{h+='<tr><td>'+r.b+'</td><td class="num">'+r.c+'</td><td class="num">'+r.e+'</td><td class="num">'+(r.r==null?'—':r.r.toFixed(1))+'</td><td class="num">'+(r.pr?Math.round(r.pr*100)+'%':'—')+'</td><td title="'+hesc(r.why||'')+'" style="'+(r.hs==='weak'?'color:var(--bad);font-weight:600':r.hs==='watch'?'font-weight:600':'color:var(--muted)')+'">'+(HL[r.hs]||(r.sl?'slowed':'ok'))+'</td></tr>';});
 h+='</tbody></table></div>';
 const el=$('cmtable');if(el){el.innerHTML=h;el.querySelectorAll('th[data-k]').forEach(th=>th.onclick=()=>{_chipsort=th.dataset.k;renderChipTable();});}
}

// restart/reboot progress: a single client clock (_rebStart ms, _rebNominal ms). The bar fills
// toward the nominal duration but is held below 100% until the miner actually answers again.
function rebootSeed(nominalMs,srvPct){
 const total=nominalMs||150000;
 // seed only if we don't already have a fresh window, so the bar doesn't jump each poll
 if(!window._rebStart||Date.now()-window._rebStart>total*3){
  const elapsed=Math.min(0.95,Math.max(0,srvPct||0.02))*total;
  window._rebStart=Date.now()-elapsed;window._rebNominal=total;}}
function rebootFrac(){if(!window._rebStart)return 0.02;
 let p=(Date.now()-window._rebStart)/(window._rebNominal||150000);
 if(p>0.96)p=0.96;if(p<0.02)p=0.02;return p;}
function rebootClear(){window._rebStart=0;window._rebNominal=0;stopRebootTick();}
function rebootingHTML(msg){const p=rebootFrac();
 startRebootTick();
 return '<div style="display:flex;flex-direction:column;gap:9px;max-width:360px">'
  +'<span class="muted">↻ '+hesc(msg||'Miner is restarting…')+'</span>'
  +'<div class="rbar"><div class="rfill" id="rfill" style="width:'+(p*100).toFixed(1)+'%"></div></div>'
  +'<span class="muted" id="rpct" style="font-size:12px">'+Math.round(p*100)+'%'+(p>=0.96?' · almost there…':'')+'</span></div>';}
function startRebootTick(){if(window._rebTick)return;
 window._rebTick=setInterval(()=>{const f=$('rfill');if(!f){stopRebootTick();return;}
  const p=rebootFrac();f.style.width=(p*100).toFixed(1)+'%';
  const pe=$('rpct');if(pe)pe.textContent=Math.round(p*100)+'%'+(p>=0.96?' · almost there…':'');},1000);}
function stopRebootTick(){if(window._rebTick){clearInterval(window._rebTick);window._rebTick=null;}}
// firmware-image install progress on the dashboard (image-update models): kept in this browser across the reload the new
// panel causes, so the page that started it shows each stage, then the result from Settings' last-update note.
function updProgSet(ver,kind){const pk=kind==='pkg',p={ver:ver||'',kind:pk?'pkg':'image',at:Date.now(),ms:pk?150000:240000,down:false};window._updProg=p;updProgSave(p);}
function updProgSave(p){try{localStorage.setItem('fw-updprog',JSON.stringify(p));}catch(e){}}
function updProgGet(){if(window._updProg!==undefined)return window._updProg;let p=null;
 try{p=JSON.parse(localStorage.getItem('fw-updprog')||'null');}catch(e){}
 if(!p||typeof p.at!=='number'||!(Date.now()-p.at<30*60000))p=null;window._updProg=p;return p;}
function updProgClear(){window._updProg=null;try{localStorage.removeItem('fw-updprog');}catch(e){}stopUpdTick();}
function updProgHTML(p){const t=Date.now()-p.at,f=Math.min(0.96,Math.max(0.02,t/p.ms));
 const st=p.kind==='pkg'?[[0.10,'Saving the files it replaces, mining stops'],[0.30,'Installing the new files'],[2,'Restarting the miner (if it doesn’t start properly, the previous version is put back by itself)']]
  :[[0.06,'Preparing: the panel moves into memory, mining stops'],[0.72,'Writing the new system to the flash'],[0.86,'Reading every block back from the chip to check it'],[2,'Restarting the miner with the new system']];
 let cur=st.findIndex(x=>f<x[0]);if(cur<0)cur=st.length-1;
 const rows=st.map((x,i)=>'<li style="list-style:none;margin:2px 0'+(i>cur?';color:var(--muted)':'')+'">'+(i<cur?'<b class="okpill">✓</b> ':i===cur?'<b>›</b> ':'<span class="muted">·</span> ')+x[1]+(i===cur?'…':'')+'</li>').join('');
 const late=t>10*60000;
 return '<div class="note updprog" style="max-width:520px'+(late?';border-color:var(--bad)':'')+'"><b>⬆ Updating the firmware'+(p.ver?' to '+hesc(p.ver):'')+'</b>'
  +'<ul style="font-size:13px;margin:6px 0 0;padding:0;line-height:1.5">'+rows+'</ul>'
  +'<div class="rbar" style="margin-top:8px"><div class="rfill" style="width:'+(f*100).toFixed(1)+'%"></div></div>'
  +'<div class="muted" style="font-size:12px;margin-top:4px">'+Math.round(f*100)+'% · '+Math.floor(t/60000)+':'+String(Math.floor(t/1000)%60).padStart(2,'0')+' elapsed (about '+(p.kind==='pkg'?'2–3':'4')+' minutes in all). Keep the power on; this page reconnects by itself.</div>'
  +(late?'<div style="font-size:12.5px;margin-top:6px;color:var(--bad)">This is taking much longer than it should. Keep the miner powered. If it isn’t back within 15 minutes, recover it with an SD card'+(p.kind==='pkg'?' with the firmware':' (Settings → Firmware update → How to recover with an SD card)')+'.</div>':'')+'</div>';}
function startUpdTick(){if(window._updTick)return;window._updTick=setInterval(()=>{const p=updProgGet(),es=document.querySelectorAll('.updprog');if(!es.length||!p){stopUpdTick();return;}es.forEach(e=>{e.outerHTML=updProgHTML(p);});},1000);}
// updProgInSettings: the same progress in Settings → Firmware update, where the install was started
function updProgInSettings(){const p=updProgGet(),b=$('updbody');if(!p||!b)return;
 b.innerHTML='<!--updprog-->'+updProgHTML(p)+'<!--/updprog-->'+String(b.innerHTML||'').replace(/<!--updprog-->[\s\S]*?<!--\/updprog-->/g,'');startUpdTick();}
function stopUpdTick(){if(window._updTick){clearInterval(window._updTick);window._updTick=null;}}
// updProgShow: the dashboard's overview shows the install while the miner is away (true = shown)
function updProgShow(){const p=updProgGet();if(!p)return false;if(!p.down){p.down=true;updProgSave(p);}
 $('ov').innerHTML='<div style="grid-column:1/-1">'+updProgHTML(p)+'</div>';startUpdTick();return true;}
// updProgBack: the panel answers again after the install: say how it went (last-update note), once
async function updProgBack(){const p=updProgGet();if(!p||(!p.down&&Date.now()-p.at<90000))return;updProgClear(); // (not seen going down after 90 s: the install didn't start)

 let u={};try{const r=await fetch('/api/update');u=await r.json();}catch(e){}
 const l=u.last||{},ok=l.result==='installed',now=u.version?'v'+String(u.version).replace(/^v/,''):'';
 window._updDone='<div class="note '+(ok?'':'warn')+'" style="grid-column:1/-1;margin-bottom:6px">'+(ok?'✓ Firmware update finished'+(now?': now running '+hesc(now):'')+'.':'⚠ The firmware update didn’t finish as planned'+(now?' (running '+hesc(now)+')':'')+(l.note?': '+hesc(l.note):'')+'. Details in Settings → Firmware update.')
  +' <a href="#" onclick="window._updDone=\'\';refresh();return false">dismiss</a></div>';
 toast(ok?'Firmware update finished':'The firmware update didn’t finish: see Settings → Firmware update',!ok);refresh();if(typeof loadUpdate==='function')loadUpdate();}
const PAGE_BUILD='__PAGE_BUILD__';
async function refresh(){try{const r=await fetch('/api/dashboard',fetchTO(15000));if(r.status===401){boot();return;}const d=await r.json().catch(()=>{throw {http:r.status};});window._noPool=!!d.no_pool;window._offSince=0;
 if(d.build&&d.build!==PAGE_BUILD&&!window._reloading){window._reloading=true;toast('The panel was updated: reloading the page…');setTimeout(()=>location.reload(),1500);return;}
 // the panel restarted with other commissioned features (Settings → Commissioning): reload so every gate follows
 if(d.commission&&MODEL()&&MODEL().commission&&d.commission!==MODEL().commission&&!window._reloading){window._reloading=true;toast('The panel restarted with other features switched on: reloading the page…');setTimeout(()=>location.reload(),1500);return;}
 if(!d.ok){
  window._lastDash=d;fanLockUI(d);
  if(d.fan_lock&&d.idle){$('ov').innerHTML='<div class="note warn" style="grid-column:1/-1;color:var(--bad)">⚠ Mining is locked after a fan problem: the miner is on Idle (hash boards powered down). <a href="#" onclick="window._flDismiss=false;fanLockUI(window._lastDash);return false">Details and unlock</a></div>';return;}
  {const bd=$('boards');if(bd){bd.style.opacity='.45';bd.title='Not live: the miner isn’t reporting right now';}if($('bcount'))$('bcount').textContent='last reading, not live';}
  if(d.restarting&&updProgShow())return;
  if(d.restarting){rebootSeed(150000,d.reboot_pct);$('ov').innerHTML='<div style="grid-column:1/-1">'+rebootingHTML(d.err)+'</div>';return;}
  rebootClear();$('ov').innerHTML=d.idle?('<div class="note" style="grid-column:1/-1">☾ '+hesc(d.err)+' <button onclick="showTab(\'presets\')">Presets</button></div>'):d.starting?('<div style="grid-column:1/-1">'+busyHTML(d.err)+'</div>'):d.no_pool?('<div class="note warn" style="grid-column:1/-1">'+hesc(d.err)+' <button class="primary" onclick="showTab(\'pools\')">Go to Pools</button></div>'):('<div style="grid-column:1/-1;color:var(--bad)">'+hesc(d.err||'API error')+'</div>');
  {const ot=overTempHTML(d)+chainHTML(d)+unexpHTML(d);if(ot)$('ov').insertAdjacentHTML('afterbegin',ot);}return;}
 // recovered — clear any reboot progress state
 rebootClear();updProgBack();
 {const bd=$('boards');if(bd){bd.style.opacity='';bd.title='';}}
 if(d.share_unit)window._shareUnit=d.share_unit;
 if(d.hash_unit)window._hrUnit=d.hash_unit;
 if(d.best_share!=null)window._bestRaw=d.best_share;
 // lock state transition (e.g. the tuner takes/releases charge) → refresh any visible controls/fan/presets
 const lk=d.in_charge||'';if(lk!==window._lockState){window._lockState=lk;
  const vis=pn=>{const el=document.querySelector('.tabpane[data-pane="'+pn+'"]');return el&&!el.classList.contains('hide');};
  if(vis('controls')){loadControl();loadFan();}if(vis('presets'))loadPresets();}
 const mk=(k,v)=>'<div class="kv"><div class="k">'+k+'</div><div class="v">'+v+'</div></div>';
 const hrs=Math.floor(d.elapsed/3600),mins=Math.floor(d.elapsed%3600/60);
 const bestTxt=d.best_share_fmt||fmtShareDisp(d.best_share);
 const hp=hrParts(d.ghs);
 window._lastDash=d;{const nh=(d.boards||[]).filter(b=>b.alive&&b.ghs>0).length;if(nh>0)window._nHash=nh;}fanLockUI(d);$('ov').innerHTML=(d.fan_lock?'<div style="grid-column:1/-1;color:var(--bad);font-size:13px;line-height:1.5;margin-bottom:4px">⚠ Mining is locked after a fan problem (the miner is on Idle). <a href="#" onclick="window._flDismiss=false;fanLockUI(window._lastDash);return false">Details and unlock</a></div>':'')+(d.warn?'<div style="grid-column:1/-1;color:var(--bad);font-size:13px;line-height:1.5;margin-bottom:4px">⚠ '+hesc(d.warn)+'</div>':'')+(window._updDone||'')+overTempHTML(d)+chainHTML(d)+unexpHTML(d)+liveTiles(d);
 const bc=$('boards');bc.innerHTML='';let alive=0;
 // boards that aren't fitted (the miner found no board on that connector, and it isn't hashing): hidden
 // unless "show" is chosen (remembered in this browser)
 const absentIds=new Set((d.board_health||[]).filter(h=>h.absent).map(h=>h.id)),showAll=lsGet('sclite-show-unfitted')==='1';
 const unfitted=(d.boards||[]).filter(b=>absentIds.has(b.id)&&!b.alive&&!(b.ghs>0));
 (d.boards||[]).forEach(b=>{if(b.alive)alive++;if(!showAll&&unfitted.includes(b))return;const tt=tempParts(b.temps);
  bc.innerHTML+='<div class="bd'+(b.alive?'':' dead')+'"><div class="t"><b>Board '+b.id+'</b><span>'+((d.board_health||[]).some(h=>h.id===b.id&&!h.absent&&h.chips>=0&&h.chips<h.expected)?'<span class="pill bad" style="margin-right:6px" title="This board started with fewer chips than it has">SHORT CHAIN</span>':'')+(b.alive&&!b.enabled?'<span class="pill warn" title="Hashing is stopped: the board is disabled in the miner (stop hashing / idle). Its clock reads 0 until it is enabled again.">STOPPED</span>':'<span class="pill '+(b.alive?'ok':'bad')+'">'+(b.alive?'ALIVE':'—')+'</span>')+'</span></div>'
   +'<div class="line"><span class="muted">Hashrate</span><span>'+(b.alive&&!b.enabled?'0 <span class="muted" title="The miner still reports this run’s average ('+fmtHRc(b.ghs)+'), but the board is stopped and not hashing.">(stopped)</span>':fmtHRc(b.ghs))+'</span></div>'
   +'<div class="line"><span class="muted">Clock</span><span>'+(b.alive&&!b.enabled?'— <span class="muted">(stopped)</span>':f1(b.clock_mhz)+' MHz')+'</span></div>'
   +'<div class="line"><span class="muted">Voltage</span><span>'+(isOtherModel()?(b.volt_level?'<span title="Measured at the power lugs under load on one unit (the miner has no voltage sensor); “≈” marks an estimate. '+boxVoltsTxt(42)+' on Stock High, '+boxVoltsTxt(61)+' on Stock Low.">'+boxVoltsTxt(b.volt_level)+'</span>':'—'):f1(b.voltage_v)+' V'+((d.power||{}).held_v&&b.alive?' <span class="muted" title="The miner doesn\'t supply less than about 9.0–9.1 V: set lower, the boards stay here (and so does the power).">(set PV '+(((d.power||{}).pv||0)/1000).toFixed(2)+': its lowest)</span>':''))+'</span></div>'
   +(function(){if(featOff2(F().PowerEstimate))return ''; // no power figure on a model whose estimate isn't commissioned (Box II: C2)
     const bw=((d.power||{}).boards||{})[b.id];if(!bw||!b.alive)return '';const ths=b.ghs/1000;
     return '<div class="line"><span class="muted">Power</span><span>'+((d.power||{}).calibrated?'':'≈ ')+Math.round(bw)+' W</span></div>'
      +'<div class="line"><span class="muted">Efficiency</span><span>'+(ths>0.01?Math.round(bw/ths)+' J/TH':'—')+'</span></div>';})()
   +'<div class="line"><span class="muted" title="Nonces the board\'s chips found since mining started (the chips\' own count, far more than the shares the pool accepts)">Nonces</span><span>'+(b.nonced!=null?(+b.nonced).toLocaleString():'—')+'</span></div>'
   +'<div class="line"><span class="muted" title="Hardware errors ÷ (nonces + hardware errors)">HW err</span><span>'+pct2(b.hw_pct)+'% ('+b.hw_errors+')</span></div>'
   +'<div class="line"><span class="muted" title="The chips’ own average temperature, as the miner reports it">Chips (avg)</span><span>'+tt.chips+'</span></div>'
   +'<div class="line"><span class="muted" title="The temperature sensor on the hash board'+(isOtherModel()?'':' (TMP112)')+'">Board sensor</span><span>'+tt.board+'</span></div>'
   +bhLines(d,b)
   +(b.overheat?'<div class="line"><span class="pill bad">OVERHEAT</span></div>':'')+'</div>';});
 {const fitted=(d.boards||[]).length-unfitted.length;$('bcount').innerHTML=hesc(alive+' of '+fitted+' active')+(unfitted.length?' · <a href="#" onclick="lsSet(\'sclite-show-unfitted\','+(showAll?'\'0\'':'\'1\'')+');refresh();return false" title="Board '+unfitted.map(b=>b.id).join(', ')+': the miner found no board on '+(unfitted.length>1?'those connectors':'that connector')+' at start-up">'+(showAll?'hide':'show')+' '+unfitted.length+' not fitted</a>':'');}
 renderRejected(d);loadBestShares();
}catch(e){
 // the agent itself is unreachable — during a full reboot that's expected
 if(updProgShow())return;
 if(window._rebStart&&Date.now()-window._rebStart<(window._rebNominal||150000)+240000){
  $('ov').innerHTML=rebootingHTML('Rebooting the miner — the control panel reconnects automatically when it\'s back.');return;}
 // it answered, but not with the figures (an error page): say that, not "can't reach"
 if(e&&e.http){$('ov').innerHTML='<div style="grid-column:1/-1">'+busyHTML('The miner answered with an error (HTTP '+e.http+'), not its figures. Trying again…')+'</div>';return;}
 // a few minutes of calm "reconnecting" first (a restart or a Wi-Fi blip), then say it plainly
 if(!window._offSince)window._offSince=Date.now();
 $('ov').innerHTML=Date.now()-window._offSince<180000?('<div style="grid-column:1/-1">'+busyHTML(OFFLINE)+'</div>'):'<span style="color:var(--bad)">Can\'t reach the miner for '+Math.round((Date.now()-window._offSince)/60000)+' min: check that it’s powered and on the network.</span>';}}

let es=null,paused=false,src='mine',workOnly=false;
function url(){return src==='mine'?'/api/engine/stream':'/api/log/stream';}
async function logSave(kind){try{const r=await fetch('/api/log/export?src='+kind);if(!r.ok){toast((await r.text())||'unavailable',true);return;}
 const blob=await r.blob(),cd=r.headers.get('Content-Disposition')||'',fn=(cd.match(/filename="?([^"]+)"?/)||[])[1]||('miner-log.'+(kind==='private'?'zip':'txt'));
 const a=document.createElement('a');a.href=URL.createObjectURL(blob);a.download=fn;document.body.appendChild(a);a.click();setTimeout(()=>{URL.revokeObjectURL(a.href);a.remove();},1000);}catch(e){toast('save failed',true);}}
function classify(t){
 // a good/submitted nonce (and the accepted share it becomes) is green — shown before the
 // generic nonce rule so a "nonce# … GOOD" or "mining.submit … nonce=…" line isn't mistaken
 // for a bad one.
 if(/API: connection/.test(t))return 'dim';
 // the pool's answer to a submit: result true with no error is an accepted share, not an error
 if(/RECV:[^\n]*"result":\s*true/.test(t)&&/"error":\s*null/.test(t))return 'accept';
 if(/RECV:[^\n]*"result":\s*false/.test(t))return 'reject';
 // local work dropped because the pool sent a new job: normal on every job change, never sent to the pool
 if(/Work stale due to work update/.test(t))return 'dim';
 if(/\bAccepted\b|✓ ?accept|accepted \+|mining\.submit|submitted|share found|→ ?accepted|nonce#[^\n]*\b(GOOD|OK)\s*$|\bGOOD\b[^\n]*nonce/i.test(t))return 'accept';
 if(/best share|★/i.test(t))return 'best';
 if(/\bRejected\b|✗ ?reject|rejected \+|\bstale\b/i.test(t))return 'reject';
 if(/invalid nonce|hw error|hardware error/i.test(t))return 'invalid';
 if(/BAD\(|nonce#|\bnonce\b/i.test(t))return 'nonce';
 if(/Stratum|new block|detected new block/i.test(t))return 'pool';
 if(/OVERHEAT|overheat|unreachable|fail|FAIL|rror/.test(t))return 'err';
 if(/WatchDog Exit|Device is Poweroff|Chip Avgtemp|tvout:|set device vfff|clk old|read cpb voltage|scanhash|Work restart|gshs4|targetRPM|Change vout/.test(t))return 'dim';
 return '';}
const WORK=new Set(['accept','reject','invalid','nonce','best','pool','err']);
function appendLine(t){const l=$('log');const cls=classify(t);
 if(workOnly&&src==='mine'&&!WORK.has(cls))return;
 const d=document.createElement('div');d.className='ln '+cls;d.textContent=t;l.appendChild(d);
 while(l.childElementCount>2000)l.removeChild(l.firstChild);
 l.scrollTop=l.scrollHeight;}
function start(){if(es)es.close(); // one stream: signing in again after the session ended must not add a second
 es=new EventSource(url());es.onmessage=e=>appendLine(e.data);}
function toggle(){const b=$('pause');
 if(paused){start();paused=false;b.textContent='⏸ Pause';b.classList.add('active');}
 else{if(es){es.close();es=null;}paused=true;b.textContent='▶ Resume';b.classList.remove('active');}}
function setSrc(s){src=s;$('tab-mine').classList.toggle('active',s==='mine');$('tab-sys').classList.toggle('active',s==='sys');
 $('log').innerHTML='';if(es){es.close();es=null;}if(!paused)start();}
function toggleWork(){workOnly=!workOnly;$('workonly').classList.toggle('active',workOnly);
 $('log').innerHTML='';if(es){es.close();es=null;}if(!paused)start();}
let CTRL=null;
async function loadControl(){if(featBlock('ctrlbody',F().ClockControl,'Clock and voltage control'))return;
 try{const r=await fetch('/api/control');if(r.status===401){boot();return;}const j=await r.json();
 if(!j.ok){if(!CTRL||!j.busy)$('ctrlbody').innerHTML=busyHTML(j.err,j.detail);$('ctrlnote').textContent='';retryLater(loadControl);return;}
 CTRL=j;renderControl(j);}catch(e){if(!CTRL)$('ctrlbody').innerHTML=busyHTML(OFFLINE);retryLater(loadControl);}}
function renderControl(j){const c=j.current,oc=!!j.overclock;
 const L=(oc&&j.hardlimits)?j.hardlimits:j.limits;
 const safe=j.limits||L;
 const nb=(id,lab,unit,min,max,step,val,smin,smax)=>'<div class="ctrl"><label>'+lab+'</label>'+
   '<div class="cstep">'+
     '<button type="button" tabindex="-1" onclick="cstep(\''+id+'\',-'+step+')">−</button>'+
     '<input type="number" id="'+id+'" min="'+min+'" max="'+max+'" step="'+step+'" value="'+val+'" '+
       'onchange="cclamp(this);ctrlWarn()" oninput="ctrlWarn()" onfocus="this.select()">'+
     '<button type="button" tabindex="-1" onclick="cstep(\''+id+'\','+step+')">+</button>'+
     '<span class="u">'+unit+'</span>'+
   '</div>'+
   '<span class="chint">range <b>'+min+'</b>–<b>'+max+'</b> '+unit+(oc&&smax!=null&&smax<max?' · safe ≤'+smax:'')+'</span></div>';
 let p='';
 // (the public release has none on Box II: people switch on the Presets page; the owner's build keeps the row for its tests)
 if(j.presets&&j.presets.length&&!(PUB()&&isOtherModel())){p='<div class="presets"><span class="mut">Stock presets — click one to switch the miner to it (asks first)</span>';
   j.presets.forEach(ps=>{const pl=ps.plan||{};const d=(B2T()&&pl.freq_mhz?b2Name(b2ModeAt(pl.freq_mhz,j.presets))+' · ':'')+(pl.freq_mhz?(pl.freq_mhz+' MHz · '+(isOtherModel()?boxVoltsTxt(pl.vrun_mv):'PV '+pl.pv_mv)):'idle / stop');
     p+='<button onclick="applyPreset('+ps.index+',\''+d+'\')" title="'+hesc(isOtherModel()?d:(ps.raw||''))+'">'+d+'</button>';});
   p+='</div>';}
 const lk=j.in_charge||'';
 // unlock bar: open the disclaimer, or (when unlocked) a red banner + re-lock
 // (a model with the unlock or the probe not commissioned: no unlock bar / only the probe — C6/C7)
 const ub=featOff2(F().OverclockUnlock)
   ?(featOff2(F().CeilingProbe)?'':'<div class="row" style="margin:2px 0 10px"><button class="ocbtn" onclick="ocProbe()">📐 Probe the clock ceiling…</button><span class="muted" style="font-size:11.5px;align-self:center">steps the clock past '+safe.freq[1]+' MHz while mining at the stock voltage, stops on heat, HW errors or a chip dropout, then goes back to the stock plan</span></div><div id="ceilbox"></div>')
   :(oc&&isOtherModel())
   ?'<div class="ocbar"><span>🔓 <b>Clock limit unlocked.</b> '+(L.freq[1]>safe.freq[1]?'You can set the clock past '+safe.freq[1]+' MHz, up to '+L.freq[1]+' MHz, at the stock voltage — permanent damage is possible and unsupported.':'No overclock ceiling has been decided for this model yet, so the clock still stays at '+safe.freq[1]+' MHz or below.')+(j.at?' Accepted '+hesc(j.at)+'.':'')+'</span><button onclick="ocProbe()">Probe the clock ceiling</button><button onclick="ocRelock()">Re-lock</button></div><div id="ceilbox"></div>'
   :oc
   ?'<div class="ocbar"><span>🔓 <b>Clock limit unlocked.</b> You can set the clock past 700 MHz — permanent damage is possible and unsupported. (Voltage never needed unlocking: PV 9100–9600 is the supply’s whole range.)'+(j.at?' Accepted '+hesc(j.at)+'.':'')+'</span><button onclick="ocProbe()">Find firmware ceiling</button><button onclick="ocRelock()">Re-lock</button></div><div id="ceilbox"></div>'
   :'<div class="row" style="margin:2px 0 10px"><button class="ocbtn" onclick="ocUnlock()">🔓 Unlock clock limit…</button><span class="muted" style="font-size:11.5px;align-self:center">'+(isOtherModel()?'raise the max clock past the safe window ('+safe.freq[1]+' MHz)':'raise the max clock past the safe window (the voltage range is the supply’s own: PV 9100–9600)')+'</span></div>';
 $('ctrlbody').innerHTML=(lk?lockBar(lk):j.busy?'<div class="lockbar">🔒 <b>Locked</b> — '+hesc(j.busy)+' is running: it owns the clock until it ends.</div>':'')+p+ub
   +'<div class="muted" style="font-size:11.5px;margin:2px 2px 8px">Type a value or use − / + to step. Nothing changes on the miner until you press <b>Apply</b>.</div>'
   +nb('c_freq','Frequency','MHz',L.freq[0],L.freq[1],25,c.freq_mhz,safe.freq[0],safe.freq[1])
   +(B2T()?b2VoltCtl(j,nb):(nb('c_pv','Board voltage (PV)','mV',L.pv[0],L.pv[1],10,c.pv_mv,safe.pv[0],safe.pv[1])
   +nb('c_vrun','Run voltage','mV',L.vrun[0],L.vrun[1],10,c.vrun_mv,safe.vrun[0],safe.vrun[1])
   +'<label class="muted" style="display:flex;gap:7px;align-items:center;font-size:11.5px;margin:2px 2px 6px"><input type="checkbox" id="c_follow" style="width:auto;margin:0" onchange="ctrlWarn()"'+((c.pv_mv-c.vrun_mv===300)?' checked':'')+'>Keep run voltage at PV − 300 (the stock gap). Only PV changes the boards\' voltage: run voltage is replaced by PV a few seconds after it\'s set (measured: same voltage and watts whatever it is).</label>'
   +'<div class="muted" style="font-size:11.5px;margin:0 2px 8px">The supply works between about <b>9.1 V and 9.6 V</b> (PV 9100–9600, measured on the supply): a lower PV runs at 9.1 V with the same power, and a higher one is ignored, so PV stops at 9600 even with the limits unlocked. To use less power below 9.1 V, lower the clock.</div>'))
   +'<div id="ctrlwarn"></div><div id="ctrlest" class="muted" style="font-size:12px;margin:8px 2px 0"></div>'+calNote()
   +'<div class="muted" style="font-size:11.5px;margin:6px 2px 0">Fans are set on the <b>Auto fan</b> panel below — a clock/voltage change here leaves the current fan speeds alone.'+(B2T()?imgMore('The plan’s 4th field',b2F4Note()):'')+'</div>'
   +'<div class="row" style="margin-top:14px"><button class="primary" onclick="applyControl()">Apply</button><button onclick="renderControl(CTRL)">Revert</button></div>';
 if(lk||j.busy)document.querySelectorAll('#ctrlbody input,#ctrlbody select,#ctrlbody button:not(.takemanual)').forEach(e=>e.disabled=true);
 $('ctrlnote').textContent=lk?('locked — '+chargeTxt(lk)):((j.idle?'idle (boards off) · ':'')+(j.manual?'manual':b2SelTxt(j))+(j.idle?'':(j.current&&j.current.freq_mhz?' · '+j.current.freq_mhz+' MHz · '+(B2T()?boxVoltsTxt(j.current.vrun_mv):'PV '+j.current.pv_mv+' mV (run '+j.current.vrun_mv+' mV)'):(j.raw&&!B2T()?' · '+j.raw:''))));
 ctrlWarn();
 if(oc)ocProbeRefresh();}
// Box II (planBox2V): no PV and no millivolts. The only voltage setting is Goldshell's voltage LEVEL (42–61: a
// higher level is a lower voltage), shown in volts next to it (boxVoltsTxt). With voltage control off (test C4
// not commissioned) the level follows the clock: the level of the stock plan at that clock or faster (the
// backend's rule, box2VFor), and Apply sends 0 for it ("the rule's level"), as before.
// b2ModeAt: the Box II stock mode's key for a stock plan's clock (the fastest of the stock plans is Hashrate, as box2ModeName)
function b2ModeAt(fq,presets){let hi=0;(presets||[]).forEach(ps=>{const f=(ps.plan||{}).freq_mhz||0;if(f>hi)hi=f;});return fq>=hi?'Hashrate':'Low-power';}
// b2SelTxt: the selected stock plan in the Controls header: "preset 0" on the SC Lite, its name ("Stock High") on Box II
function b2SelTxt(j){if(B2T()){const ps=(j.presets||[]).find(x=>x.index===j.select),f=ps&&ps.plan&&ps.plan.freq_mhz;if(f)return b2Name(b2ModeAt(f,j.presets));}return 'preset '+j.select;}
function b2VC(){return !!(CTRL&&CTRL.box2&&CTRL.box2.voltage_control);}
function b2Levels(){const v=CTRL&&CTRL.box2&&CTRL.box2.v_codes;return (v&&v.length===2)?v:[42,61];}
function b2RuleLevel(fq){let best=-1,v=0;((CTRL&&CTRL.presets)||[]).forEach(ps=>{const pl=ps.plan||{};if(pl.freq_mhz>=fq&&pl.vrun_mv>0&&(best<0||pl.freq_mhz<best)){best=pl.freq_mhz;v=pl.vrun_mv;}});return best<0?0:v;}
function b2VoltCtl(j,nb){const c=j.current||{},lv=b2Levels(),vc=!!(j.box2&&j.box2.voltage_control);
 const val=vc?(c.vrun_mv>=lv[0]&&c.vrun_mv<=lv[1]?c.vrun_mv:lv[0]):(b2RuleLevel(c.freq_mhz)||c.vrun_mv||lv[0]);
 return '<div class="ctrl"><label>Voltage</label><div class="cstep"><select id="c_vlvl" onchange="ctrlWarn()"'+(vc?'':' disabled')+'>'+b2VOpts(val,lv[0],lv[1])+'</select></div>'
  +'<span class="chint">range <b>'+boxVoltsTxt(lv[1])+'</b>–<b>'+boxVoltsTxt(lv[0])+'</b></span></div>'
  +'<div class="muted" style="font-size:11.5px;margin:0 2px 8px">'+(vc?'':imgDot('you')+'<b>Voltage control is off</b> on this unit'+(PUB()?'':' (Settings → Commissioning)')+': the voltage follows the clock (the stock plan’s voltage at that clock or faster).')
  +imgMore('About the voltage steps','The miner offers '+(lv[1]-lv[0]+1)+' fixed voltage steps between '+boxVoltsTxt(lv[1])+' and '+boxVoltsTxt(lv[0])+', about 0.03 V apart, measured at the power lugs under load on one unit: '+boxVoltsTxt(lv[0])+' on Stock High, '+boxVoltsTxt(lv[1])+' on Stock Low. Values marked “≈” are estimates.')+'</div>';}
// ctrlWarn flags, live, when any typed value is past the conservative safe window (only possible while unlocked).
function ctrlWarn(){const w=$('ctrlwarn');if(!w)return;
 // the value as it will be sent: a box past its own range is clamped to it on change (cclamp) and on Apply, so the
 // warnings must not come and go between mousedown and click on Apply (the button would move and the click be lost)
 const g=id=>{const e=document.getElementById(id)||{},v=+e.value;return e.value===''||isNaN(v)||!(e.min&&e.max)?v:Math.max(+e.min,Math.min(+e.max,v));};
 // run voltage follows PV (− 300) while the box is ticked
 {const f=$('c_follow'),vr=$('c_vrun');if(f&&vr){vr.disabled=f.checked||!!(CTRL&&(CTRL.in_charge||CTRL.busy));
   if(f.checked&&g('c_pv')>0){vr.value=Math.max(+vr.min,Math.min(+vr.max,g('c_pv')-300));}}}
 {const vl=$('c_vlvl');if(vl&&!b2VC()){const r=b2RuleLevel(g('c_freq'));if(r)vl.value=String(r);vl.disabled=true;}}
 const ce=$('ctrlest'),pw=(window._lastDash||{}).power||{};
 const ew=(pw.watts&&pw.mhz===g('c_freq')&&pw.pv===g('c_pv'))?pw.watts:estW(g('c_freq'),g('c_pv')); // the running setting: the Dashboard's figure
 {const fq=g('c_freq'),run=Math.floor(fq/12.5)*12.5;window._runsNote=(fq>0&&run!==fq)?(' · the chips run '+run+' MHz (12.5 MHz steps)'):'';}
 if(ce)ce.innerHTML=ew?('At this setting'+(window._runsNote||'')+': <b>'+(window._powF?'':'≈ ')+Math.round(ew)+' W</b> · '+(ew/(window._powMains||100)).toFixed(1)+' A at '+Math.round(window._powMains||100)+' V'+(window._powF?' (calibrated)':' (estimate)')):'';
 const s=(CTRL&&CTRL.limits);if(!s){w.innerHTML='';return;}
 const over=[];
 if(g('c_freq')>s.freq[1])over.push('clock '+g('c_freq')+' MHz');
 if(g('c_vrun')>s.vrun[1])over.push('run '+g('c_vrun')+' mV');
 if(g('c_pv')>s.pv[1])over.push('PV '+g('c_pv')+' mV');
 w.innerHTML=(over.length?'<div class="warnbar" style="margin:10px 0 0">⚠ Past the safe window: '+over.join(' · ')+'. This can permanently damage the miner and is entirely at your own risk.</div>':'')
  +(g('c_pv')>0&&g('c_pv')<9100?'<div class="note" style="margin:10px 0 0">PV '+g('c_pv')+' is below what the supply delivers: the boards will run at about 9.1 V (as at PV 9100), with the same power.</div>':'');}
function cstep(id,d){const el=document.getElementById(id);if(!el||el.disabled)return;
 const st=+el.step||1,mn=+el.min,mx=+el.max;let v=Math.round(((+el.value||mn)+d)/st)*st;
 v=Math.max(mn,Math.min(mx,v));el.value=v;ctrlWarn();}
function cclamp(el){if(el.value==='')return;let v=+el.value;const mn=+el.min,mx=+el.max;
 if(isNaN(v)){el.value=mn;return;}if(v<mn)el.value=mn;else if(v>mx)el.value=mx;}
function lockBar(who){const run=who==='the auto-tuner'||who==='the limit probe'; // the server refuses a release while these run
 return '<div class="lockbar">🔒 <b>Locked</b> — '+hesc(chargeTxt(who))+' is in charge of the clock, voltage and fans. '+(run?'<span class="muted">Stop it to take manual control.</span>':'<button class="takemanual" onclick="takeManual('+(who==='idle mode'?1:0)+')">Take manual control</button>')+'</div>';}
async function takeManual(idle){if(!confirm('Take manual control?\n\nWhatever is in charge now (a preset, Stock, Idle or the schedule) stops driving the miner. The current clock and fans stay as they are until you change them.'+(idle?'\n\n⚠ If the thermal guard put the miner on Idle to cool down, this ENDS its cool-down hold: you can start mining again while the boards are still hot.':'')))return;
 const {j}=await jpost('/api/control',{release:true});
 if(j.ok){loadControl();loadFan();loadPresets();}else toast(j.err||'failed',true);}
async function applyControl(){['c_freq','c_pv','c_vrun','c_vlvl'].forEach(id=>{const e=$(id);if(e)cclamp(e);});ctrlWarn(); // what's sent is what the boxes allow
 const v=id=>+document.getElementById(id).value;
 const oc=!!(CTRL&&CTRL.overclock);
 let fq=v('c_freq');if(fq!==Math.round(fq))fq=Math.ceil(Math.floor(fq/12.5)*12.5); // whole MHz: 612.5 is sent as 613 (the chips run 612.5)
 if(B2T()){ // Box II: the clock and, with voltage control on, the level (vrun_mv carries it; 0 = the clock's stock level); no PV
  const vc=b2VC(),lvl=vc?Math.round(v('c_vlvl')):0,show=vc?lvl:b2RuleLevel(fq);
  const bp={freq_mhz:fq,vrun_mv:lvl,pv_mv:0,confirm:true,override:oc};
  if(!confirm('Apply this powerplan?\n\n'+fq+' MHz · '+(show?boxVoltsTxt(show):'the stock voltage for this clock')+(vc?'':' (follows the clock)')+' (fans unchanged)\n\n⚠ Changing clock/voltage can PERMANENTLY DAMAGE the miner. This applies live to the running board.'))return;
  const {j}=await jpost('/api/control',bp);
  if(j.ok){$('ctrlnote').textContent='applied: '+j.applied;setTimeout(loadControl,2000);}else{toast(/^Not applied/.test(j.err||'')?j.err:('apply failed: '+(j.err||'')),true);if(j.locked||/strong-chip test is running/.test(j.err||''))loadControl();}
  return;}
 // no fans: the server keeps the ones running now (the page's copy may be minutes old)
 const p={freq_mhz:fq,vrun_mv:Math.round(v('c_vrun')),pv_mv:Math.round(v('c_pv')),confirm:true,override:oc};
 if(p.pv_mv<p.vrun_mv){toast('PV must be at least the run voltage.',true);return;}
 const s=CTRL&&CTRL.limits;let past=[];
 if(oc&&s){if(p.freq_mhz>s.freq[1])past.push('clock '+p.freq_mhz+' MHz (safe ≤'+s.freq[1]+')');
  if(p.vrun_mv>s.vrun[1])past.push('run '+p.vrun_mv+' mV (safe ≤'+s.vrun[1]+')');
  if(p.pv_mv>s.pv[1])past.push('PV '+p.pv_mv+' mV (safe ≤'+s.pv[1]+')');}
 let msg='Apply this powerplan?\n\n'+p.freq_mhz+' MHz · PV '+p.pv_mv+' mV · run '+p.vrun_mv+' mV (fans unchanged)\n\n⚠ Changing clock/voltage can PERMANENTLY DAMAGE the miner. This applies live to the running board.';
 if(past.length)msg+='\n\n🔓 PAST THE SAFE WINDOW:\n  • '+past.join('\n  • ')+'\n\nYou accepted the unlock disclaimer — this runs entirely at your own risk.';
 if(!confirm(msg))return;
 const {j}=await jpost('/api/control',p);
 if(j.ok){$('ctrlnote').textContent='applied: '+j.applied;setTimeout(loadControl,2000);}else{toast(/^Not applied/.test(j.err||'')?j.err:('apply failed: '+(j.err||'')),true);if(j.locked||/strong-chip test is running/.test(j.err||''))loadControl();}} // show who holds it
// ---- overclock unlock ----
function ocUnlock(){$('ocbox').innerHTML=ocDiscHTML();show('ocmodal',true);
 const cb=$('oc_agree'),bt=$('oc_accept');if(cb&&bt){cb.checked=false;bt.disabled=true;cb.onchange=()=>bt.disabled=!cb.checked;}}
function ocClose(){show('ocmodal',false);}
async function ocAccept(){const cb=$('oc_agree');if(!cb||!cb.checked)return;
 const {j}=await jpost('/api/overclock',{acknowledge:true});
 ocClose();if(j&&j.ok){loadControl();}else toast('could not unlock: '+((j&&j.err)||''),true);}
async function ocRelock(){if(!confirm('Re-lock to the safe window?\n\nThe clock/voltage boxes go back to the conservative range. Anything already applied stays until you change it.'))return;
 const {j}=await jpost('/api/overclock',{revoke:true});
 if(j&&j.ok){loadControl();}else toast('could not re-lock: '+((j&&j.err)||''),true);}
function ocDiscHTML(){return ''
 +'<h3>Unlock clock &amp; voltage limits</h3>'
 +'<p>'+(B2T()?('By default this tool refuses any clock outside a conservative window'+(CTRL&&CTRL.limits&&CTRL.limits.freq?' ('+CTRL.limits.freq[0]+'–'+CTRL.limits.freq[1]+' MHz)':'')+' that is unlikely to harm the hardware, and any voltage that isn’t one of the listed steps. Unlocking raises the clock cap so you can push the miner further.'):'By default this tool clamps clock and voltage to a conservative range that is unlikely to harm the hardware. Unlocking raises those caps so you can push the miner further.')+' Read this first.</p>'
 +'<h4>What unlocking does</h4>'
 +'<ul><li>Raises the maximum you can set for <b>frequency</b> up to a wider hard ceiling.'+(isOtherModel()?'':' <b>PV</b> stays within 9100–9600: that’s what the supply delivers (measured), whatever is unlocked.')+'</li>'
 +'<li>Values above the normal safe window are flagged, but are sent to the board exactly as you enter them.</li>'
 +'<li>Fan limits are unchanged. The firmware\'s own thermal cutoff still applies, but must not be relied on as protection.</li></ul>'
 +'<h4>Risks you accept</h4>'
 +'<ul><li>Running past the safe window can cause <b>overheating, instability, reduced lifespan, or permanent, irreversible damage</b> to the miner and attached hardware.</li>'
 +'<li>It may <b>void any warranty</b> and could create a fire or electrical hazard if cooling or power delivery is inadequate.</li>'
 +'<li>Results vary by unit; a setting that is stable on one board may destroy another.</li></ul>'
 +'<h4>No warranty, no liability</h4>'
 +'<p>This firmware and tool are provided <b>“as is”, without warranty of any kind</b>, express or implied. The author(s) and distributor(s) accept <b>no responsibility or liability</b> for any damage, loss, injury, downtime or cost of any kind arising from using this unlock or any value you apply with it. <b>You use this feature entirely at your own risk and are solely responsible for your hardware.</b></p>'
 +'<label class="occheck"><input type="checkbox" id="oc_agree"> I have read and understand the above, accept all risk, and release the author(s) from any liability.</label>'
 +'<div class="mrow"><button onclick="ocClose()">Cancel</button><button class="danger" id="oc_accept" onclick="ocAccept()" disabled>Unlock limits</button></div>';}
// ---- firmware-ceiling probe (clamp detection) ----
let _ceilTimer=null;
async function ocProbe(){
 if(isOtherModel()){ // held steps while mining (ceilingbox2.go): the confirm is this run's risk acceptance
  if(!confirm('Probe the clock ceiling?\n\nThis runs the clock PAST the stock window, 10 MHz at a time, at the stock voltage, holding each step for a few minutes while mining. It stops at the first step that gets too hot (82 °C), makes too many hardware errors or loses a chip, and always goes back to the stock Hashrate plan at the end.\n\n⚠ Running past the stock clock can PERMANENTLY DAMAGE the miner. Stay with it, and Stop it if anything looks wrong. Controls and fans are locked during the probe.'))return;
  const {j}=await jpost('/api/ceiling',{start:true,accept_risk:true});
  if(!j.ok){toast(j.err||'could not start',true);return;}
  window._ceilLogOpen=undefined;ocProbePoll();return;}
 if(!confirm('Find the firmware ceiling?\n\nThis steps Frequency past the safe window one value at a time, applies each for ~2–3 s, reads what the hardware actually reports, then restores your current plan after EVERY step. It never leaves a high setting running and aborts if a board gets hot.\n\nControls and fans are locked during the probe — supervise it.'))return;
 const {j}=await jpost('/api/ceiling',{start:true});
 if(!j.ok){toast(j.err||'could not start',true);return;}
 window._ceilLogOpen=undefined; // fresh run: default the log open while it streams
 ocProbePoll();}
async function ocStopProbe(){await jpost('/api/ceiling',{stop:true});}
function ocProbePoll(){ocProbeRefresh();}
async function ocProbeRefresh(){try{const r=await fetch('/api/ceiling');const j=await r.json();if(j.ok){renderCeiling(j.state);
  if(j.state&&j.state.running&&!_ceilTimer){_ceilTimer=setInterval(ocProbeRefresh,2000);}
  if(j.state&&!j.state.running&&_ceilTimer){clearInterval(_ceilTimer);_ceilTimer=null;}}}catch(e){}}
function renderCeiling(s){const el=$('ceilbox');if(!el)return;if(!s||(!s.running&&!s.done&&!(s.axes&&s.axes.length))){return;}
 window._ceil=s;
 const held=(s.axes||[]).some(a=>a.held);
 let h='<div class="ceilpanel"><div class="ceilhd"><b>'+(held?'Clock ceiling probe':'Firmware-ceiling probe')+'</b><span class="muted">'+(s.running?('running · '+hesc(s.phase)):'done')+'</span>'+(s.running?'<button class="ebtn" onclick="ocStopProbe()">Stop</button>':'')+'</div>';
 (s.axes||[]).forEach(a=>{let st;
  if(a.state==='done'&&a.held)st='<b style="color:'+(a.cap>a.safe_max?'var(--ok)':'var(--warn)')+'">clean to '+a.cap+' '+a.unit+'</b>';
  else if(a.state==='done')st=a.clamped?('<b style="color:var(--bad)">cap ≈ '+(a.actual||a.cap)+' '+a.unit+'</b>'):('<b style="color:var(--ok)">≥ '+a.hard_max+' '+a.unit+'</b>');
  else if(a.state==='probing')st='<span style="color:var(--warn)">probing '+a.at+' '+a.unit+'…</span>';
  else if(a.state==='aborted')st='<span class="muted">stopped</span>';
  else st='<span class="muted">queued</span>';
  h+='<div class="ceilrow"><span class="cl">'+a.label+'</span><span class="cv">'+st+'</span><span class="cn muted">'+(a.note?hesc(a.note):('safe ≤'+a.safe_max+' · probes to '+a.hard_max))+'</span></div>';});
 if(s.done&&!held){const caps={};(s.axes||[]).forEach(a=>{if(a.clamped&&(a.actual||a.cap))caps[a.name]=(a.actual||a.cap);});
  window._ceilCaps=caps;const n=Object.keys(caps).length;
  if(!(s.axes||[]).some(a=>a.state==='aborted'))h+='<div class="row" style="margin-top:10px">'+(n?'<button class="primary" onclick="ocApplyCaps()">Tighten unlock ceilings to these caps</button><span class="muted" style="font-size:11.5px;align-self:center">keeps you from re-entering values the firmware would clamp</span>':'<span class="muted" style="font-size:12px">'+OemOf()+' firmware accepted every step: it doesn’t cap this setting. Each step was held only for a moment and nothing was mined at it, so this isn’t a clock the chips can run: voltage (PV 9600 at most), heat and the chips set that. Use the Tuner to find it.</span>')+'</div>';}
 // keep the log open across the 2s re-renders: default open while running, then honor the user's toggle
 const lo=(window._ceilLogOpen!==undefined)?window._ceilLogOpen:!!s.running;
 if(s.log&&s.log.length)h+='<details id="ceillogdet"'+(lo?' open':'')+' style="margin-top:8px" ontoggle="window._ceilLogOpen=this.open"><summary class="muted" style="font-size:11.5px;cursor:pointer">Probe log</summary><div class="ceillog" id="ceilloginner">'+s.log.map(l=>hesc(l)).join('<br>')+'</div></details>';
 h+='</div>';el.innerHTML=h;
 if(lo){const li=$('ceilloginner');if(li)li.scrollTop=li.scrollHeight;}}
async function ocApplyCaps(){const caps=window._ceilCaps||{};if(!Object.keys(caps).length)return;
 const {j}=await jpost('/api/overclock',{caps:caps});
 if(j&&j.ok){loadControl();}else toast('could not set: '+((j&&j.err)||''),true);}
async function applyPreset(idx,label){if(!confirm('Switch to the stock preset '+label+'?\n\n⚠ This changes clock/voltage live.'))return;
 const {j}=await jpost('/api/control',{preset:idx,confirm:true});
 if(j.ok){$('ctrlnote').textContent='applied: '+(j.applied||label);setTimeout(loadControl,2000);}else toast('failed: '+(j.err||''),true);}
let FAN=null;
// ---- fan curves board (profiles.html parity) + Auto fan ----
let cvKey=null,cv=null,cvSaved='',cvSel=-1,cvHover=null,cvDrag=-1;
const CVG={w:720,h:340,l:50,r:18,t:16,b:42,tmin:30,tmax:100};
const cvX=t=>CVG.l+(t-CVG.tmin)/(CVG.tmax-CVG.tmin)*(CVG.w-CVG.l-CVG.r);
const cvY=f=>CVG.t+(100-f)/100*(CVG.h-CVG.t-CVG.b);
const cvInvX=px=>CVG.tmin+(px-CVG.l)/(CVG.w-CVG.l-CVG.r)*(CVG.tmax-CVG.tmin);
const cvInvY=py=>100-(py-CVG.t)/(CVG.h-CVG.t-CVG.b)*100;
function fanAt(pts,t){if(!pts.length)return null;if(t<=pts[0].temp)return pts[0].fan;for(let i=1;i<pts.length;i++)if(t<=pts[i].temp)return pts[i-1].fan+(t-pts[i-1].temp)/Math.max(1e-6,pts[i].temp-pts[i-1].temp)*(pts[i].fan-pts[i-1].fan);return pts[pts.length-1].fan;}
const cvPts=p=>((p&&p.points)||[]).map(x=>({temp:+x.temp,fan:+x.fan})).sort((a,b)=>a.temp-b.temp);
function cvFanAt(p,pts,t){if((p&&p.mode)==='steps'){let f=null;for(const q of pts)if(t>=q.temp)f=q.fan;return f;}return fanAt(pts,t);}
function cvEditable(){const p=FAN&&FAN.profiles[cvKey];return !!(cv&&p&&!p.builtin&&cv.mode!=='steps');}
function cvSlim(p){return {label:p.label,points:cvPts(p),abort_c:+p.abort_c,cooldown_s:+p.cooldown_s,poll_s:+p.poll_s,smoothing:+(p.smoothing||0),force_tempcontrol_on:p.force_tempcontrol_on!==false};}
function cvDirty(){return cv&&JSON.stringify(cvSlim(cv))!==cvSaved;}
function fanStatus(s){if(!s)return '';
 const g=s.guard?('<span class="gpill '+s.guard+'">'+s.guard.toUpperCase()+'</span>'):'';
 return (s.throttled?'<span class="gpill throttle">CLOCK THROTTLED</span> ':'')+g+(s.note?(' '+hesc(s.note)):'');}
async function loadFan(){try{const r=await fetch('/api/fan');if(r.status===401){boot();return;}const j=await r.json();
 if(!j.ok){if(!FAN)$('fanbody').innerHTML=busyHTML(j.err||'Waiting for the fan settings…',j.detail);retryLater(loadFan);return;}
 FAN=j;cvWire();renderAutoFan();
 if(!cvKey||!FAN.profiles[cvKey])cvKey=cvRunningKey()||'curve-balanced';
 if(!cvDirty())cvOpen(cvKey,true);else{cvRenderList();cvDraw();}}catch(e){if(!FAN)$('fanbody').innerHTML=busyHTML(OFFLINE);retryLater(loadFan);}}
function cvRunningKey(){const c=FAN.config,P=FAN.profiles;if(P[c.profile])return c.profile;if(c.profile!=='custom')return null;
 const tk='tuned:'+(FAN.in_charge||'').replace(/^preset /,''),same=k=>JSON.stringify(cvPts(P[k]))===JSON.stringify(cvPts(c));
 return P[tk]&&same(tk)?tk:(Object.keys(P).find(k=>P[k].tuned&&same(k))||null);}
function curveOptions(sel){const P=FAN.profiles,groups=[['Yours',k=>!P[k].builtin&&!P[k].tuned],['Made by the tuner',k=>P[k].tuned],['Built-in curves',k=>P[k].builtin&&P[k].mode==='curve'],['Stock profiles',k=>P[k].builtin&&P[k].mode!=='curve']];
 let h='';groups.forEach(([t,f])=>{const ks=Object.keys(P).filter(f).sort((a,b)=>String(P[a].label).localeCompare(P[b].label));if(!ks.length)return;
  h+='<optgroup label="'+t+'">'+ks.map(k=>'<option value="'+hesc(k)+'"'+(k===sel?' selected':'')+'>'+hesc(P[k].label||k)+'</option>').join('')+'</optgroup>';});
 if(sel==='custom')h='<option value="custom" selected>the preset’s own curve</option>'+h;
 return h;}
function renderAutoFan(){const j=FAN,c=j.config,s=j.status||{},flk=j.in_charge||'';window._fanLocked=!!flk;
 const t=s.max_temp>-100?((+s.max_temp).toFixed(0)+' °C'):'—';
 const rpm=(s.fans_rpm||[]).filter(x=>x>0);
 $('fanbody').innerHTML=(flk?lockBar(flk):'')
  +'<div class="table-wrap"><table class="ctable af-table"><thead><tr><th class="num">Hottest</th><th class="num">Fans</th><th>Preset</th><th>Auto fan</th><th>Fan curve</th><th>Now</th></tr></thead><tbody><tr>'
  +'<td class="num" data-l="Hottest"><span>'+t+(s.hot_board>=0&&s.max_temp>-100?' <span class="muted">(board '+s.hot_board+')</span>':'')+'</span></td>'
  +'<td class="num" data-l="Fans"><span>'+(rpm.length?rpm.join(' / ')+fanUnit(rpm):'—')+'</span></td>'
  +'<td data-l="Preset"><span>'+hesc(chargeTxt(flk)||'none')+'</span></td>'
  +'<td data-l="Auto fan"><label class="tcheck" style="margin:0"><input type="checkbox" id="f_en"'+(c.enabled?' checked':'')+'> on</label></td>'
  +'<td data-l="Fan curve"><select id="f_profile" onchange="cvOpen(this.value)">'+curveOptions(c.profile)+'</select></td>'
  +'<td data-l="Now"><span>'+(s.target>=0?('fan <b>'+(s.target||0)+' %</b>'):(s.note?'':'<span class="muted">the miner’s own control</span>'))+' '+fanStatus(s)+'</span></td>'
  +'</tr></tbody></table></div>'
  +'<details class="af-guard"'+((window._afgOpen!==undefined?window._afgOpen:c.throttle_enable)?' open':'')+' ontoggle="window._afgOpen=this.open"><summary class="muted">Thermal guard: clock throttle (this panel)</summary>'
  +'<label class="tcheck"><input type="checkbox" id="f_then"'+(c.throttle_enable?' checked':'')+'> Step the clock down when a board gets very hot, put it back when it cools</label>'
  +'<div class="tform-grid" style="margin-top:8px"><label>Throttle at °C<input type="number" id="f_tc" min="70" max="'+MABORT()+'" value="'+c.throttle_c+'"></label><label>Step (MHz)<input type="number" id="f_tstep" min="5" max="100" value="'+c.throttle_step+'"></label><label>Resume below °C<input type="number" id="f_resume" min="40" max="'+(MABORT()-1)+'" value="'+c.resume_c+'"></label></div>'
  +'<p class="thint">The firmware’s own temperature control stays on either way, so its ~'+MCUTOFF()+' °C cutoff is always the final backstop.</p></details>'
  +(isOtherModel()?'<div class="row" style="margin-top:10px"><button class="primary" onclick="saveFan()">Save</button><span class="muted" style="font-size:12px;align-self:center">Changes here take effect straight away.</span></div>'
   +'<div class="muted">'+imgMore('Tips','Turn off your external blake2b-asic-control app’s auto fan for this miner so the two don’t both drive the fans.'+(B2T()?'<br>'+b2F4Note():''))+'</div>'
  :(B2T()?'<p class="thint">'+b2F4Note()+'</p>':'')
  +'<div class="row" style="margin-top:10px"><button class="primary" onclick="saveFan()">Save</button><span class="muted" style="font-size:12px;align-self:center">Changes here take effect straight away. Turn off your external blake2b-asic-control app’s auto fan for this miner so the two don’t both drive the fans.</span></div>');
 if(flk)document.querySelectorAll('#fanbody input,#fanbody select,#fanbody button:not(.takemanual)').forEach(e=>e.disabled=true);
 window._fanEdit=false;{const fb=$('fanbody');if(!fb._ed){fb._ed=1;['input','change'].forEach(ev=>fb.addEventListener(ev,e=>{if(e.target.closest('input,select'))window._fanEdit=true;}));}}
 $('fannote').textContent=flk?'locked':(c.enabled?'on':'off');$('fannote').className='pill'+(c.enabled&&!flk?' ok':'');}
async function saveFan(){const c=FAN.config,key=$('f_profile').value,thOn=$('f_then').checked,M=MODEL()||{};
 // the throttle's fields are checked only while the throttle is on (as the miner checks them): a value saved by an
 // older build outside this model's range must not block saving the fan curve; with the throttle off it is
 // replaced by the model's default instead
 const thDef={f_tc:M.throttle_def||82,f_tstep:25,f_resume:M.resume_def||75};
 for(const [id,nm] of [['f_tc','Throttle at °C'],['f_tstep','Step (MHz)'],['f_resume','Resume below °C']]){const e=$(id),v=+e.value;
  if(e.value===''||!Number.isInteger(v)||v<+e.min||v>+e.max){if(!thOn){e.value=thDef[id];continue;}
   toast(nm+': a whole number from '+e.min+' to '+e.max+' (open “Thermal guard” to change it)',true);const d=e.closest('details');if(d)d.open=true;e.focus();return;}}
 if($('f_en').checked&&!c.enabled&&!confirm('Turn auto fan on?\n\nMake sure your external blake2b-asic-control app isn’t also driving this miner’s fans.'))return;
 const cfg=Object.assign({},c,{enabled:$('f_en').checked,profile:key,throttle_enable:$('f_then').checked,throttle_c:+$('f_tc').value,throttle_step:+$('f_tstep').value,resume_c:+$('f_resume').value});
 delete cfg.curves;
 const {j}=await jpost('/api/fan',cfg);
 if(j.ok){toast(cfg.enabled?('Auto fan on: '+((FAN.profiles[key]||{}).label||key)):'Auto fan off');setTimeout(loadFan,400);}else toast(j.err||'save failed',true);}
// pollFanStat: Fan pair offset isn't reloaded over an offset typed but not saved yet; "Use for auto fan" counts as an
// edit of the Auto fan panel (window._fanEdit), so this refresh doesn't put the saved curve back before Save.
async function pollFanStat(){try{const r=await fetch('/api/fan');const j=await r.json();if(!j.ok||!FAN)return;FAN.status=j.status;FAN.in_charge=j.in_charge;
 {const pn=document.querySelector('.tabpane[data-pane="controls"]'),ae=document.activeElement,inp=id=>ae&&ae.closest&&ae.closest('#'+id);
  if(pn&&!pn.classList.contains('hide')){if(!inp('fpbody')&&!($('fpoff')&&$('fpoff').value!==$('fpoff').defaultValue))loadFanPair();if(!inp('fcbody'))loadFanCheck();}} // their "now" figures are live too
 if(!window._fanEdit&&(!document.activeElement||!document.activeElement.closest||!document.activeElement.closest('#fanbody')))renderAutoFan();cvDraw();}catch(e){}}
function cvRenderList(){const P=FAN.profiles,c=FAN.config,groups=[['Yours',k=>!P[k].builtin&&!P[k].tuned],['Made by the tuner',k=>P[k].tuned],['Built-in curves',k=>P[k].builtin&&P[k].mode==='curve'],['Stock profiles',k=>P[k].builtin&&P[k].mode!=='curve']];
 let h='';groups.forEach(([t,f],gi)=>{const ks=Object.keys(P).filter(f);if(!ks.length&&gi)return;h+='<h4>'+t+'</h4>';
  if(!gi)h+='<button class="cv-item cv-new" onclick="cvNew()">+ New curve<small>'+(ks.length?'start from the one shown':'none yet: start from the one shown')+'</small></button>';
  const rk=cvRunningKey();ks.forEach(k=>{const on=c.enabled&&k===rk;h+='<button class="cv-item'+(k===cvKey?' on':'')+'" data-k="'+hesc(k)+'">'+hesc(P[k].label||k)+'<small>'+(on?'on this miner':'not in use')+'</small></button>';});});
 $('cvList').innerHTML=h;$('cvList').querySelectorAll('.cv-item[data-k]').forEach(b=>b.onclick=()=>cvOpen(b.dataset.k));}
function cvOpen(key,force){if(!force&&cvDirty()&&key!==cvKey&&!confirm('Leave this curve without saving your changes?'))return false;
 const p=FAN.profiles[key];if(!p)return false;cvKey=key;cv=JSON.parse(JSON.stringify(p));cv.points=cvPts(cv);cvSaved=JSON.stringify(cvSlim(cv));cvSel=-1;
 const ed=cvEditable();$('cvLabel').value=cv.label||key;$('cvLabel').disabled=!ed||!!p.tuned;
 $('cvKind').textContent=(p.builtin?'built-in · ':p.tuned?'made by the tuner · ':'yours · ')+({curve:'curve',steps:'steps (stock)'}[p.mode]||p.mode);
 $('cvCopy').classList.toggle('hide',ed);['cvSave','cvSaveAs','cvDirty','cvUndo'].forEach(id=>$(id).classList.toggle('hide',!ed));$('cvDel').classList.toggle('hide',!ed||!!p.tuned);
 const pn=p.tuned?key.slice(6):'';
 $('cvNote').className='note cv-note'+(ed?(p.tuned?'':' hide'):' warn');
 $('cvNote').innerHTML=!ed?('<span>'+(p.mode==='steps'?'A stock step profile':'A built-in curve')+' is read-only, so there’s always a known-good one to go back to. Make your own editable version:</span> <button class="primary" onclick="$(\'cvCopy\').click()">Copy to edit</button>')
  :(p.tuned?('Made by the tuner for the <b>'+hesc(pn)+'</b> preset. Changes are saved into that preset'+((FAN.in_charge||'')==='preset '+pn?', and its fans follow them straight away (it’s on the miner now).':'.')+' <b>Save as new</b> keeps the tuner’s version and makes your own copy.'):'');
 cvRenderList();cvChanged();return true;}
function cvDraw(){const svg=$('cvSvg');if(!svg||!cv)return;
 const cw=Math.round(svg.getBoundingClientRect().width)||720,w=Math.max(320,Math.min(720,cw)),hh=w<520?300:340;
 if(CVG.w!==w||CVG.h!==hh){CVG.w=w;CVG.h=hh;CVG.l=w<520?40:50;svg.setAttribute('viewBox','0 0 '+w+' '+hh);}
 const pts=cvPts(cv);let h='';
 for(let t=CVG.tmin;t<=CVG.tmax;t+=5){const maj=t%10===0;h+='<line x1="'+cvX(t)+'" x2="'+cvX(t)+'" y1="'+CVG.t+'" y2="'+(CVG.h-CVG.b)+'" stroke="var(--line)" stroke-width="'+(maj?1:.5)+'"'+(maj?'':' stroke-dasharray="2 4"')+'/>';
  if(maj)h+='<text x="'+cvX(t)+'" y="'+(CVG.h-CVG.b+17)+'" text-anchor="middle" font-size="12" fill="var(--muted)">'+t+'</text>';}
 for(let f=0;f<=100;f+=10){h+='<line x1="'+CVG.l+'" x2="'+(CVG.w-CVG.r)+'" y1="'+cvY(f)+'" y2="'+cvY(f)+'" stroke="var(--line)" stroke-width="'+(f%20?.5:1)+'"'+(f%20?' stroke-dasharray="2 4"':'')+'/>';
  if(f%20===0)h+='<text x="'+(CVG.l-8)+'" y="'+(cvY(f)+4)+'" text-anchor="end" font-size="12" fill="var(--muted)">'+f+'%</text>';}
 h+='<text x="'+((CVG.l+CVG.w-CVG.r)/2)+'" y="'+(CVG.h-6)+'" text-anchor="middle" font-size="12" fill="var(--muted)">hottest board °C</text>';
 const ab=Number(cv.abort_c||90);
 if(ab<CVG.tmax){h+='<rect x="'+cvX(ab)+'" y="'+CVG.t+'" width="'+(cvX(CVG.tmax)-cvX(ab))+'" height="'+(CVG.h-CVG.t-CVG.b)+'" fill="var(--bad)" opacity=".12"/>';
  h+='<line x1="'+cvX(ab)+'" x2="'+cvX(ab)+'" y1="'+CVG.t+'" y2="'+(CVG.h-CVG.b)+'" stroke="var(--bad)" stroke-width="1.5"/>';
  h+='<text x="'+(cvX(ab)+5)+'" y="'+(CVG.t+14)+'" font-size="11" fill="var(--bad)">safety '+ab+'°</text>';}
 const steps=cv.mode==='steps';
 if(pts.length){let path;if(steps){path=[];pts.forEach((p,i)=>{const nx=i+1<pts.length?pts[i+1].temp:CVG.tmax;path.push([p.temp,p.fan],[nx,p.fan]);});}else path=[[CVG.tmin,pts[0].fan]].concat(pts.map(p=>[p.temp,p.fan]),[[CVG.tmax,pts[pts.length-1].fan]]);
  const d=path.map(([t,f],i)=>(i?'L':'M')+cvX(Math.max(CVG.tmin,Math.min(CVG.tmax,t))).toFixed(1)+','+cvY(f).toFixed(1)).join(' ');
  h+='<path d="'+d+' L'+cvX(CVG.tmax)+','+cvY(0)+' L'+cvX(Math.max(CVG.tmin,path[0][0]))+','+cvY(0)+' Z" fill="var(--text)" opacity=".07"/>';
  if(steps)h+='<text x="'+(cvX(CVG.tmin)+6)+'" y="'+cvY(8)+'" font-size="11" fill="var(--muted)">below '+pts[0].temp+'°: the miner’s own fan control</text>';
  h+='<path d="'+d+'" fill="none" stroke="var(--text)" stroke-width="2.5" stroke-linejoin="round"/>';}
 // this miner: where it is right now
 const st=FAN.status||{},tm=st.max_temp;
 if(tm!=null&&tm>-100){const f=cvFanAt(cv,pts,tm),x=cvX(Math.max(CVG.tmin,Math.min(CVG.tmax,tm)));
  h+='<line x1="'+x+'" x2="'+x+'" y1="'+CVG.t+'" y2="'+(CVG.h-CVG.b)+'" stroke="var(--ok)" stroke-width="1.5" stroke-dasharray="5 4"/>';
  if(f!=null)h+='<circle cx="'+x+'" cy="'+cvY(f)+'" r="5" fill="var(--ok)"/>';
  const right=x<CVG.w-150;h+='<text x="'+(right?x+5:x-5)+'" y="'+(CVG.h-CVG.b-6)+'" text-anchor="'+(right?'start':'end')+'" font-size="11" fill="var(--ok)">'+hesc((st.label||'this miner')+(st.hot_board>=0?(' · board '+st.hot_board):''))+' '+tm.toFixed(0)+'°</text>';}
 if(cvHover!=null){const f=cvFanAt(cv,pts,cvHover),x=cvX(cvHover);
  h+='<line x1="'+x+'" x2="'+x+'" y1="'+CVG.t+'" y2="'+(CVG.h-CVG.b)+'" stroke="var(--muted)" stroke-width="1"/>';
  if(f!=null){h+='<circle cx="'+x+'" cy="'+cvY(f)+'" r="4" fill="var(--muted)"/>';const right=x<CVG.w-120;
   h+='<text x="'+(right?x+8:x-8)+'" y="'+Math.max(CVG.t+14,cvY(f)-8)+'" text-anchor="'+(right?'start':'end')+'" font-size="12" fill="var(--text)">'+cvHover.toFixed(0)+' °C → '+Math.round(f)+' %</text>';}}
 const ed=cvEditable();
 pts.forEach((p,i)=>{const on=ed&&i===cvSel;
  h+='<circle class="pt" data-i="'+i+'" cx="'+cvX(p.temp)+'" cy="'+cvY(p.fan)+'" r="'+(ed?8:5)+'" fill="'+(on?'var(--text)':'var(--bg)')+'" stroke="var(--text)" stroke-width="2.5" style="cursor:'+(ed?'grab':'default')+'"/>';
  if(ed)h+='<circle class="pt" data-i="'+i+'" cx="'+cvX(p.temp)+'" cy="'+cvY(p.fan)+'" r="18" fill="transparent"/>';});
 svg.innerHTML=h;cvTry();}
function cvTry(){if(!cv)return;const t=Number($('cvTry').value),f=cvFanAt(cv,cvPts(cv),t);$('cvTryOut').textContent=f==null?(t+' °C → the miner’s own control'):(t+' °C → fan '+Math.round(f)+' %');}
function cvSvgPt(ev){const svg=$('cvSvg'),pt=svg.createSVGPoint();pt.x=ev.clientX;pt.y=ev.clientY;return pt.matrixTransform(svg.getScreenCTM().inverse());}
function cvClamp(i,t,f){const P=cv.points,lo=i>0?P[i-1].temp+1:20,hi=i<P.length-1?P[i+1].temp-1:100;
 const flo=Math.max(FMIN(),i>0?P[i-1].fan:FMIN()),fhi=Math.min(100,i<P.length-1?P[i+1].fan:100);
 return [Math.round(Math.max(lo,Math.min(hi,t))),Math.round(Math.max(flo,Math.min(fhi,f)))];}
function cvWire(){const svg=$('cvSvg');if(!svg||svg._wired)return;svg._wired=true;
 svg.addEventListener('pointerdown',ev=>{if(!cvEditable())return;const p=cvSvgPt(ev),hit=ev.target.closest&&ev.target.closest('.pt');
  cv.points.sort((a,b)=>a.temp-b.temp);
  if(hit){cvDrag=cvSel=Number(hit.dataset.i);}
  else{if(p.x<CVG.l||p.x>CVG.w-CVG.r||p.y<CVG.t||p.y>CVG.h-CVG.b)return;
   if(cv.points.length>=8){toast('A curve can have up to 8 points',true);return;}
   const t=Math.round(cvInvX(p.x));if(cv.points.some(q=>q.temp===t))return;
   cv.points.push({temp:t,fan:Math.round(cvInvY(p.y))});cv.points.sort((a,b)=>a.temp-b.temp);
   cvSel=cvDrag=cv.points.findIndex(q=>q.temp===t);const c=cvClamp(cvSel,t,cv.points[cvSel].fan);cv.points[cvSel]={temp:c[0],fan:c[1]};}
  svg.setPointerCapture(ev.pointerId);svg.focus({preventScroll:true});cvChanged();ev.preventDefault();});
 svg.addEventListener('pointermove',ev=>{const p=cvSvgPt(ev);
  if(cvDrag>=0&&cvEditable()){const c=cvClamp(cvDrag,cvInvX(p.x),cvInvY(p.y));cv.points[cvDrag]={temp:c[0],fan:c[1]};cvHover=c[0];cvChanged(true);return;}
  const t=cvInvX(p.x);cvHover=(p.x>=CVG.l&&p.x<=CVG.w-CVG.r)?Math.round(Math.max(CVG.tmin,Math.min(CVG.tmax,t))):null;cvDraw();});
 svg.addEventListener('pointerleave',()=>{if(cvDrag<0){cvHover=null;cvDraw();}});
 const end=()=>{if(cvDrag>=0){cvDrag=-1;cvChanged();}};svg.addEventListener('pointerup',end);svg.addEventListener('pointercancel',end);
 svg.addEventListener('dblclick',ev=>{const hit=ev.target.closest&&ev.target.closest('.pt');if(hit&&cvEditable())cvRemove(Number(hit.dataset.i));});
 svg.addEventListener('keydown',ev=>{if(!cvEditable()||cvSel<0||cvSel>=cv.points.length)return;
  const d={ArrowLeft:[-1,0],ArrowRight:[1,0],ArrowUp:[0,1],ArrowDown:[0,-1]}[ev.key];
  if(d){const q=cv.points[cvSel],c=cvClamp(cvSel,q.temp+d[0],q.fan+d[1]);cv.points[cvSel]={temp:c[0],fan:c[1]};cvChanged();ev.preventDefault();}
  else if(ev.key==='Delete'||ev.key==='Backspace'){cvRemove(cvSel);ev.preventDefault();}});
 [['cvAbort','abort_c'],['cvCool','cooldown_s'],['cvPoll','poll_s']].forEach(([id,k])=>{$(id).addEventListener('input',()=>{if($(id).value.trim()==='')return;cv[k]=Number($(id).value);cvChanged();});
  $(id).addEventListener('change',()=>{if($(id).value.trim()===''&&cv)$(id).value=cv[k];});});
 $('cvSmooth').onchange=()=>{cv.smoothing=Number($('cvSmooth').value);cvChanged();};
 $('cvLabel').addEventListener('input',()=>{if(cv){cv.label=$('cvLabel').value;cvChanged();}});
 $('cvTry').oninput=cvTry;
 $('cvAddPt').onclick=()=>{const P=cv.points;let gi=0,gap=-1;for(let i=1;i<P.length;i++)if(P[i].temp-P[i-1].temp>gap){gap=P[i].temp-P[i-1].temp;gi=i;}
  if(gap<2){toast('No room for another point',true);return;}P.splice(gi,0,{temp:Math.round((P[gi-1].temp+P[gi].temp)/2),fan:Math.round((P[gi-1].fan+P[gi].fan)/2)});cvSel=gi;cvChanged();};
 $('cvSave').onclick=()=>cvSaveCurve(false);$('cvSaveAs').onclick=()=>cvSaveCurve(true);$('cvUndo').onclick=()=>cvOpen(cvKey,true);
 $('cvCopy').onclick=()=>{const src=FAN.profiles[cvKey];let pts=cvPts(src).map(p=>({temp:Math.round(Math.max(20,Math.min(100,p.temp))),fan:Math.round(Math.max(FMIN(),Math.min(100,p.fan)))}));
  if(pts.length<2)pts=[{temp:55,fan:40},{temp:75,fan:90}];
  if(src.mode==='steps')toast('Your copy is a smooth curve through the same points (the step mode is for the stock profile only)');
  cv={mode:'curve',label:((src.label||cvKey)+(src.mode==='steps'?' (copy, as a curve)':' (copy)')).slice(0,48),points:pts,abort_c:Math.min(MABORT(),Math.max(70,Number(src.abort_c||88))),cooldown_s:Number(src.cooldown_s||45),poll_s:Math.max(5,Number(src.poll_s||10)),smoothing:Number(src.smoothing||0),force_tempcontrol_on:true};
  cvSaveCurve(true);};
 $('cvDel').onclick=async()=>{if(!confirm('Delete the curve "'+cv.label+'"?'))return;const {j}=await jpost('/api/fan',{action:'profile_delete',key:cvKey});
  if(j.ok){toast('Curve deleted');cvKey=null;cv=null;await loadFan();}else toast(j.err||'failed',true);};
 $('cvUse').onclick=()=>{if(!$('f_profile'))return;$('f_profile').value=cvKey;if($('f_en'))$('f_en').checked=true;window._fanEdit=true;$('fanpanel').scrollIntoView({block:'nearest'});toast('Picked for auto fan: press Save under Auto fan to put it on');};}
function cvRemove(i){if(cv.points.length<=2){toast('A curve needs at least 2 points',true);return;}cv.points.splice(i,1);cvSel=Math.min(cvSel,cv.points.length-1);cvChanged();}
function cvValidate(){const errs=[];if(!cv)return errs;const P=cvPts(cv);
 if(P.length<2||P.length>8)errs.push('A curve needs 2 to 8 points.');
 P.forEach(p=>{if(!(p.temp>=20&&p.temp<=100))errs.push(p.temp+' °C is outside 20-100 °C.');if(!(p.fan>=10&&p.fan<=100))errs.push('Fan '+p.fan+' % is outside 10-100 %.');});
 for(let i=1;i<P.length;i++){if(P[i].temp===P[i-1].temp)errs.push('Two points at '+P[i].temp+' °C.');if(P[i].fan<P[i-1].fan)errs.push('The fan drops from '+P[i-1].fan+' % to '+P[i].fan+' % as it gets hotter.');}
 const ab=Number(cv.abort_c);if(!(ab>=70&&ab<=MABORT()))errs.push('Safety temperature must be 70–'+MABORT()+' °C ('+oemFw()+' takes over at '+MCUTOFF()+' °C).');else if(P.length&&ab<=P[0].temp)errs.push('The safety temperature must be above the first point.');
 if(!(Number(cv.cooldown_s)>=15&&Number(cv.cooldown_s)<=600))errs.push('Re-send must be 15-600 s.');
 if(!(Number(cv.poll_s)>=5&&Number(cv.poll_s)<=60))errs.push('Check every must be 5-60 s.');
 if(!String(cv.label||'').trim())errs.push('Give the curve a name.');return errs;}
function cvRenderPts(){const ed=cvEditable();if(!cv)return;$('cvEdit').classList.toggle('hide',cv.mode==='steps');
 $('cvPts').innerHTML=cv.points.map((p,i)=>'<div class="cv-prow'+(i===cvSel&&ed?' sel':'')+'" data-i="'+i+'"><input type="number" data-k="temp" value="'+p.temp+'" min="20" max="100"'+(ed?'':' disabled')+'><span>°C</span><input type="number" data-k="fan" value="'+p.fan+'" min="'+FMIN()+'" max="100"'+(ed?'':' disabled')+'><span>%</span>'+(ed?'<button data-del="'+i+'" title="Remove this point">×</button>':'')+'</div>').join('');
 $('cvPts').querySelectorAll('input').forEach(el=>{el.addEventListener('focus',()=>{cvSel=Number(el.closest('.cv-prow').dataset.i);cvDraw();});
  el.addEventListener('change',()=>{const i=Number(el.closest('.cv-prow').dataset.i);cv.points[i]=Object.assign({},cv.points[i],{[el.dataset.k]:Math.round(Number(el.value))});cvSel=i;cvChanged();});});
 $('cvPts').querySelectorAll('[data-del]').forEach(b=>b.onclick=()=>cvRemove(Number(b.dataset.del)));
 [['cvAbort','abort_c'],['cvCool','cooldown_s'],['cvPoll','poll_s']].forEach(([id,k])=>{if(document.activeElement!==$(id))$(id).value=cv[k];$(id).disabled=!ed;});
 $('cvSmooth').value=String(cv.smoothing||0);$('cvSmooth').disabled=!ed;$('cvAddPt').disabled=!ed||cv.points.length>=8;}
function cvChanged(fast){if(cvEditable())cv.points.sort((a,b)=>a.temp-b.temp);cvDraw();if(fast)return;cvRenderPts();
 const errs=cvValidate(),d=cvDirty();$('cvErr').innerHTML=errs.map(hesc).join('<br>');
 $('cvDirty').textContent=d?'unsaved changes':'saved';$('cvDirty').className='pill'+(d?' warn':'');
 $('cvSave').disabled=!cvEditable()||!d||errs.length>0;$('cvSaveAs').disabled=errs.length>0;$('cvUndo').disabled=!d;}
function cvNew(){if(cvDirty()&&!confirm('Leave this curve without saving your changes?'))return;
 const src=cv||FAN.profiles['curve-balanced'],P=FAN.profiles,taken=new Set(Object.values(P).map(x=>x.label));let n=1,lbl='My curve';while(taken.has(lbl))lbl='My curve '+(++n);
 let pts=cvPts(src).map(p=>({temp:Math.round(Math.max(20,Math.min(100,p.temp))),fan:Math.round(Math.max(FMIN(),Math.min(100,p.fan)))}));if(pts.length<2)pts=[{temp:55,fan:40},{temp:75,fan:90}];
 cv={mode:'curve',label:lbl,points:pts,abort_c:Math.min(MABORT(),Math.max(70,Number(src.abort_c||88))),cooldown_s:Number(src.cooldown_s||45),poll_s:Math.max(5,Number(src.poll_s||10)),smoothing:Number(src.smoothing||0),force_tempcontrol_on:true};
 cvSaveCurve(true);}
async function cvSaveCurve(asNew){const errs=cvValidate();if(errs.length){toast(errs[0],true);return;}
 const body=Object.assign({action:'profile'},cvSlim(cv));if(!asNew)body.key=cvKey;
 else if(body.label===((FAN.profiles[cvKey]||{}).label))body.label=(body.label+' (copy)').slice(0,48);
 const {j}=await jpost('/api/fan',body);
 if(!j.ok){toast(j.err||'save failed',true);return;}
 toast(asNew?('Saved as "'+j.profile.label+'"'):'Curve saved');cvKey=j.key;cv=null;await loadFan();cvOpen(j.key,true);}
let TUNERDEF=null;
async function loadTuner(){if(featOff2(F().Tuner)){if($('tunerbody'))$('tunerbody').innerHTML=featNoteHTML('The tuner');
  ['curPanel','tmapPanel','planPanel','bestPanel','tresPanel','tlogpanel'].forEach(id=>{const e=$(id);if(e)e.classList.add('hide');});return;}
 try{const r=await fetch('/api/tuner');if(r.status===401){boot();return;}const j=await r.json();
 if(!j.ok){$('tunerbody').innerHTML=busyHTML(j.err||'Waiting for the tuner…',j.detail);retryLater(loadTuner);return;}
 j.defaults.overclock=j.overclock;j.defaults.ocmax=j.ocmax;j.defaults._stock=j.stock;j.defaults._box2=j.box2;j.defaults._running=j.running;TUNERDEF=j.defaults;renderTunerForm(j.defaults);updateTunerFiles(j);updateTunerStatus(j.status);}catch(e){if(!TUNERDEF){$('tunerbody').innerHTML=busyHTML(OFFLINE);retryLater(loadTuner);}}}
function updateTunerFiles(j){const pe=$('tlogpath');if(pe)pe.textContent=j.files_dir?('Tuner files: '+j.files_dir):'';window._tfiles=!!j.has_files;
 if(!window._tlogInit){window._tlogInit=true;try{if(localStorage.getItem('tlogcollapsed')==='1'){$('tlogpanel').classList.add('collapsed');$('tlogcar').textContent='▸';}}catch(e){}}}
function toggleTlog(){const p=$('tlogpanel');if(!p)return;const c=p.classList.toggle('collapsed');$('tlogcar').textContent=c?'▸':'▾';try{localStorage.setItem('tlogcollapsed',c?'1':'0');}catch(e){}}
async function tdl(kind){try{const r=await fetch('/api/tuner/export?kind='+kind);
 if(!r.ok){toast((await r.text())||'export unavailable',true);return;}
 const blob=await r.blob(),cd=r.headers.get('Content-Disposition')||'';
 let fn=(cd.match(/filename="?([^"]+)"?/)||[])[1]||('tuner-'+kind);
 const u=URL.createObjectURL(blob),a=document.createElement('a');a.href=u;a.download=fn;document.body.appendChild(a);a.click();a.remove();setTimeout(()=>URL.revokeObjectURL(u),2000);
 }catch(e){toast('export failed: '+e.message,true);}}
function modeVal(){const r=document.querySelector('input[name=t_mode]:checked');return r?r.value:'map';}
function baseVal(){const r=document.querySelector('input[name=t_base]:checked');return r?r.value:'stock';}
function fanVal(){const r=document.querySelector('input[name=t_fan]:checked');return r?r.value:'hold';}
// Box II tuner wording: the SC Lite hint's PV fields named by what they are on a V-code model; tTrimCost: one
// chip's share of the board per 12.5 MHz at the stock clock (the SC Lite figure, 0.04 %, is 1 of 46 at 625 MHz)
function tB2Hint(t){return B2T()?t.replace(/<i>Lowest PV<\/i>/g,'<i>Lowest voltage</i>').replace(/<i>Highest PV<\/i>/g,'<i>Highest voltage</i>'):t;}
// tB2Note: the tuner's Box II note: the sentence on what is known to be safe stays in view (yellow dot), the rest folds
function tB2Note(n){const h=hesc(n),p=h.split(/(?<=\.) /),k=p.findIndex(x=>/known to be safe/.test(x));
 return k<0?h:imgDot('you')+p[k]+imgMore('This model’s voltage steps and limits',p.filter((x,i)=>i!==k).join(' '));}
function tTrimCost(st){const m=MODEL()||{},n=m.chips_per_board||46,f=(st&&st.mhz)||m.freq_max||625;return (100/n*12.5/f).toFixed(2);}
function renderTunerForm(d){
 const sec=(n,t,tag,tagcls,body,cls,id)=>'<div class="tsec '+(cls||'')+'"'+(id?(' id="'+id+'"'):'')+'><div class="tsec-h"><span class="tnum">'+n+'</span><h3>'+t+'</h3>'+(tag?'<span class="ttag '+(tagcls||'')+'">'+tag+'</span>':'')+'</div>'+body+'</div>';
 const mc=(name,v,cur,t,desc,id)=>'<label class="mode-card'+(cur===v?' on':'')+'"><input type="radio" name="'+name+'" value="'+v+'"'+(id?' id="'+id+'"':'')+(cur===v?' checked':'')+' onchange="tunerWizUI()"><span><b>'+t+'</b><small>'+desc+'</small></span></label>';
 const f=(id,lab,v,step,cls)=>'<label'+(cls?' class="'+cls+'"':'')+'>'+lab+'<input type="number" id="'+id+'" value="'+(v==null?'':v)+'"'+(step?' step="'+step+'"':'')+' oninput="updateEstimate()"></label>';
 // Box II: a voltage is picked from the miner's fixed steps, shown in volts (the value sent is the level)
 const vr=(d._box2&&d._box2.vcode_min&&d._box2.vcode_max)?[d._box2.vcode_min,d._box2.vcode_max]:[42,61];
 const fv=(id,lab,v,cls)=>'<label'+(cls?' class="'+cls+'"':'')+'>'+lab+'<select id="'+id+'" onchange="tunerWizUI()">'+b2VOpts(v,vr[0],vr[1])+'</select></label>';
 const hint=(t,id,cls,lab)=>{const m=lab&&!id&&isOtherModel()?b2More(t,lab):null;return m?'<div class="thint'+(cls?' '+cls:'')+'">'+m+'</div>':'<p class="thint'+(cls?' '+cls:'')+'"'+(id?' id="'+id+'"':'')+'>'+t+'</p>';};
 const chk=(id,on,t)=>'<label class="tcheck"><input type="checkbox" id="'+id+'"'+(on?' checked':'')+' onchange="tunerWizUI()"> '+t+'</label>';
 const st=d._stock,rn=d._running,fansel=d.fan_hold?'hold':'leave',mode=d.mode||'map';
 const stockTxt=st?(st.mhz+' MHz · '+(B2T()?boxVoltsTxt(st.mv):'PV '+st.pv)):'not read yet';
 const ocnote=d.overclock?('<div class="warnbar ok" style="margin-bottom:12px">🔓 Limits unlocked: the tuner may map or confirm <b>above the safe window</b>, up to '+((d.ocmax&&d.ocmax.freq)||900)+' MHz'+(B2T()?'':' (the voltage stays within PV 9100–9600, the supply\'s measured range)')+'. Each step is held and judged against each chip’s own baseline error rate: a <b>statistical</b> clean/dirty call over the hold time, not a hard wall. Push one step at a time and supervise.</div>'):'';
 $('tunerbody').innerHTML=(d._box2?('<div class="warnbar" style="margin-bottom:12px">'+tB2Note(d._box2.note)+'</div>'):'')+ocnote
  +'<p class="muted tintro">Work through the sections below, then press <b>Review</b>. Nothing changes on the miner until you confirm on the next screen. Sections that don’t apply to the option you pick are hidden.</p>'
  +sec(1,'What do you want to do?','','',
   '<div class="mode-cards three">'
   +mc('t_mode','map',mode,'Map every clock and voltage','Tests every clock from your lowest (underclocked) to your highest (overclocked), each at its own lowest clean voltage, so you get the whole curve: what each clock really needs. The fastest clean clock is confirmed and the presets are built from the curve. Takes several hours.')
   +mc('t_mode','confirm',mode,'Confirm one setting for hours','For a setting you already have in mind, such as the winner of a search. It runs just that setting for hours and ends with CONFIRMED (the miner stays on it) or REJECTED (back to the baseline).')
   +mc('t_mode','rebuild',mode,'Rebuild the presets from the last search','Builds and tests High, Middle, Low and Lowest power again from the voltage curve the last search measured, without mapping it again: for example at other temperatures. About 2–4 hours.')
   +'</div>')
  +sec(2,'Miner','All options','',
   '<div class="treadout"><b>'+(isOtherModel()?'This '+hesc(modelName()):'This SC Lite')+'</b> <span class="muted" style="font-size:12px">(the miner this panel runs on)</span></div>'
   +'<div class="mono" style="font-size:12px;margin:2px 0 4px">Stock <b>'+stockTxt+'</b>'+(rn?(' · runs now <b>'+rn.mhz+' MHz · '+(B2T()?boxVoltsTxt(rn.mv):'PV '+rn.pv)+'</b>'):'')+'</div>'
   +hint('The tuner reads this miner’s stock plan and its running plan straight from the firmware. Clocks run in 12.5 MHz steps, so a 25 MHz step lands every test on a clock it really runs.'))
  +sec(3,'Setting to confirm','Confirm only','confirm',
   '<div class="tform-grid">'+f('t_cmhz','MHz',d.confirm_mhz,25)+(B2T()?fv('t_cmv','Voltage',d.confirm_mv):f('t_cmv','PV',(d.confirm_mv||9100)+300,50))+f('t_chours','Hours to run it',d.confirm_hours||4,0.5)+(B2T()?'':'<label>mV sent along<span class="mono treadout" id="t_cpv">—</span></label>')+'</div>'
   +hint('The time is split into rounds as long as one test (section 6). Each round is judged like a search test. A round that fails gets one retest; if the retest fails too, the setting is REJECTED and the miner goes back to the baseline.','','','How the rounds are judged'),'confirm-only','t_sec3b')
  +sec(3,'Baseline','Search and confirm','',
   '<div class="treadout">Stock: <b class="mono">'+stockTxt+'</b></div>'
   +'<div class="mode-cards three">'
   +mc('t_base','stock',d.baseline_src,'Stock (recommended)','The firmware’s own setting, shown above. Right for a healthy miner.')
   +mc('t_base','current',d.baseline_src,'What it runs now',rn?('Runs '+rn.mhz+' MHz · '+(B2T()?boxVoltsTxt(rn.mv):'PV '+rn.pv)+' now. For a miner turned down because it can’t run stock steadily.'):'For a miner turned down because it can’t run stock steadily.')
   +mc('t_base','own',d.baseline_src,'My own','Any clock and voltage inside this miner’s hard limits that you know it runs cleanly.')
   +'</div><div class="tform-grid" style="margin-top:.7rem">'+f('t_basemhz','Baseline MHz',d.baseline_mhz,25,'base-custom')+(B2T()?fv('t_basemv','Baseline voltage',d.baseline_mv,'base-custom'):f('t_basemv','Baseline PV',(d.baseline_mv||9100)+300,50,'base-custom'))+f('t_base','Baseline minutes',d.baseline_min,1)+'</div>'
   +hint('The baseline runs first so the tuner can learn every chip’s normal error rate, and every test is compared with that (a faster clock is allowed proportionally more errors, since it does more work). It has to be a setting the miner runs <b>steadily</b>: a board reset during the baseline stops the run at once. Whatever you pick, if nothing is confirmed or the run stops early, the miner goes back to <b>what it ran before the run</b>, not to stock. Longer = a more accurate baseline (60 min is a good choice).','','','How the baseline works'),'not-rebuild','t_secbase')
  +sec(4,'Clocks and voltages to map','Search only','search',
   '<div class="tform-grid">'+f('t_low','Lowest MHz',d.lowest_mhz,25)+f('t_high','Highest MHz',d.highest_mhz,25)+f('t_step','MHz per step',d.step,12.5)+(B2T()?(fv('t_maxmv','Lowest voltage',d.max_mv)+fv('t_minmv','Highest voltage',d.min_mv)+'<label>Voltage step<select id="t_mvstep" onchange="updateEstimate()">'+b2StepOpts(d.mv_step,vr[1]-vr[0])+'</select></label>')
     :(f('t_minmv','Lowest PV',Math.max(9100,(d.min_mv||8800)+300),50)+f('t_maxmv','Highest PV',Math.min(9600,(d.max_mv||9300)+300),50)+f('t_mvstep','PV per step (mV)',d.mv_step,10)))+'</div>'
   +hint(tB2Hint('The curve starts at <b>Lowest MHz</b> and works up one step at a time to <b>Highest MHz</b>, so it covers underclocking and overclocking in one run. At each clock it finds the <b>lowest voltage that stays clean</b>, walking <b>up</b> only: the lowest clock starts at <i>Lowest PV</i>; every higher clock starts at the voltage that was lowest one clock slower (a faster clock never needs less). If that isn’t clean it adds one voltage step and tests again (up to <i>Highest PV</i>); the first clean test is that clock’s lowest. A fail usually ends within minutes while a pass takes the whole test, so this pays the full time about once per clock. A fail on the way up isn’t retested (the next step up is quicker); one at <i>Highest PV</i> is, since the curve would end on it. <b>Every voltage is tested at its own clock.</b> The curve ends at the first clock that isn’t clean at any allowed voltage.<br><b>Clock steps:</b> the chips run clocks in 12.5 MHz steps (12.5 × N MHz) and round a request down: 615 runs 612.5. With 12.5, 25, 37.5 … MHz per step the tuner walks those real clocks from the one <i>Lowest MHz</i> runs to the one <i>Highest MHz</i> runs; <b>12.5 tests every clock the chips can run</b>, 25 every second one. Each clock is asked for as the smallest whole number that lands on it (588 for 587.5), and shown as “588 MHz · runs 587.5”.'),'','','How the curve is mapped, and clock steps')
   +hint(B2T()?'<b>Voltage:</b> this miner offers '+(vr[1]-vr[0]+1)+' fixed voltage steps between <b>'+boxVoltsTxt(vr[1])+'</b> and <b>'+boxVoltsTxt(vr[0])+'</b>, about 0.03 V apart, measured at the power lugs under load on one unit: '+boxVoltsTxt(vr[0])+' is Stock High, '+boxVoltsTxt(vr[1])+' Stock Low. Values marked “≈” are estimates. Pick the range with <i>Lowest voltage</i> and <i>Highest voltage</i>; <i>Voltage step</i> is how much is added each time a test isn’t clean.'
    :'<b>Voltage is PV</b>, the voltage the hash boards really get: a multimeter on the supply follows it to within about 10 mV, and it drives power and heat. The supply only delivers <b>PV 9100–9600</b> (measured: lower settings stay at about 9.1 V, higher ones are ignored), so that’s the range to map. The run voltage (mV) on its own changes nothing on the boards; the tuner sends it along as PV − 300, as the stock plan does. The tuner also stops lowering the voltage when the boards’ own reading stops falling.','','','About the voltage steps')
   +chk('t_topdown',d.top_down,'Old walk: start at the baseline voltage, go up until clean, then down until it fails (slower: every step down is a full test)')
   +hint('Off: each clock only gets voltage added until it’s clean, never lowered: a quicker, rougher curve.')
   +'<div class="topt-row">'+chk('t_chiptrim',d.chip_trim,'Trim weak chips instead of raising the whole board')+'<label class="tinline">up to <input type="number" id="t_ctmax" min="1" max="10" step="1" value="'+(d.chip_trim_max||3)+'"> chips per board, each slowed as far as <select id="t_ctdeep">'+[25,37.5,50,62.5,75].map(x=>'<option value="'+x+'"'+(x===(d.chip_trim_deep||50)?' selected':'')+'>−'+x+' MHz</option>').join('')+'</select></label></div>'
   +hint('Needs <b>Controls → Chip trims</b> switched on. When a test fails because <b>one chip</b> went over its limit, that chip is slowed 12.5 MHz at a time (as far as the depth you pick here: −25 to −75 MHz, −50 by default) and the same setting is tested again, instead of adding voltage for all '+(isOtherModel()?((MODEL()||{}).chips_per_board||46):46)+' chips. A trim <b>stays only if the test then passes</b>; otherwise it’s undone and the curve goes on as usual. At the end, a <b>trim check</b> at the best setting lifts each trim a step at a time while that setting stays clean, so trims a slower clock needed, or that were made after a test failed by bad luck, don’t stay (one test per step). Each trimmed chip costs about '+(isOtherModel()?tTrimCost(st):'0.04')+' % of the board’s hashrate per 12.5 MHz. Trims the run keeps are listed at the end and stay in Controls → Chip trims, where you can change or reset them. Off by default.','','','How trimming works')
   +'<div class="topt-row">'+chk('t_confirmwin',d.confirm_win,'Confirm the winner with one more, longer test')+'<label class="tinline">for <input type="number" id="t_confirmmin" step="5" value="'+Math.round((d.confirm_x||2)*(d.hold_min||25))+'" oninput="updateEstimate()"> min</label></div>'
   +hint('Runs the best setting once more at the end, by default for twice the test length, so the winner has the most time behind it.'),'search-only','t_sec3')
  +sec(6,'Test length and safety','All options','',
   '<div class="tform-grid">'+f('t_hold','Minutes per test',d.hold_min,1)+f('t_sigma','Sensitivity',d.sigma,0.5)+f('t_abort','Abort at °C',d.abort_c,1)+'</div>'
   +hint('<b>Minutes per test:</b> how long each setting is watched (plus '+(d.settle_min||3)+' minutes to settle). Longer catches a slightly weak chip more reliably; bad settings still fail early. <b>Sensitivity:</b> how sure the tuner must be that a chip got worse before a test fails. Errors are random, and every chip is checked, so the limits are set so that a perfectly good setting fails by bad luck only about 1 test in 20 at 2.5 (the default). Lower is stricter (2.0: about 1 in 5, more false fails); higher is more forgiving (3.0: about 1 in 100, but a slightly weak setting can slip through). <b>Abort:</b> if any board reaches this temperature the run stops and the miner goes back to what it ran before the run.','','','Sensitivity and Abort')
   +chk('t_easy',d.easy_noisy,'Go easier on chips that are already noisy at the baseline (recommended)')
   +hint('A weak or damaged chip makes far more errors than the rest no matter the clock or voltage. Judged like the others, its random ups and downs fail good settings and push the voltage up for every chip. With this on, a chip at least '+(d.weak_x||8)+' times noisier than the typical chip during the baseline only fails a step when it gets clearly worse ('+(d.weak_factor||1.5)+' times its own normal rate). The board totals are judged on the normal chips.','','','Why go easier on noisy chips'))
  +sec(7,'Fans during the run','All options','',
   '<div class="mode-cards">'
   +mc('t_fan','hold',fansel,'Hold a steady temperature (recommended)','The tuner takes over this miner’s fans and keeps the board sensor (or the hottest chip) at your target, so every setting is judged at the same temperature. It records how much fan each setting needs; that’s what the preset fan curves are built from. The auto fan waits until the run ends.')
   +mc('t_fan','leave',fansel,'Leave the fans alone','Your auto fan curve (or the miner’s own control) keeps running and the tuner only changes the clock and voltage. Temperatures can drift between tests, and presets get no fan curve of their own.')
   +'</div><div class="tform-grid hold-only" id="t_fanwrap" style="margin-top:.7rem">'+f('t_fantarget','Target temperature °C',d.fan_target_c,1)+f('t_fanmin','Lowest fan %',d.fan_min_pct,5)
   +'<label>Hold on<select id="t_holdon" onchange="updateEstimate()"><option value="board"'+(d.hold_on!=='chip'?' selected':'')+'>the board sensor (recommended)</option><option value="chip"'+(d.hold_on==='chip'?' selected':'')+'>hottest chip</option></select></label></div>')
  +sec(8,'Presets','Search only','search',
   chk('t_buildpresets',d.build_presets,'Build and test presets at the end')
   +hint('After the search, four presets are made for this miner and each is tested on its own: <b>High</b> (the best clock), <b>Lowest power</b> (the <i>Lowest MHz</i> from section 4), and <b>Middle</b> and <b>Low</b> spread evenly between them. Each uses the lowest clean voltage the curve measured at its clock. They appear on the <a href="#" onclick="showTab(\'presets\');return false">Presets</a> tab, where one click puts a preset and its fan curve on the miner.','','','How the presets are built')
   +'<div class="tform-grid presets-only" id="t_presetwrap">'+f('t_presetmin','Minutes per preset',d.preset_min,1)+f('t_warmest','Warmest preset °C',d.warmest_c,1,'hold-only')+'</div>'
   +hint('','t_presettemps','presets-only hold-only'),'search-only','t_sec7')
  +sec(8,'Presets to build','Rebuild only','presets',
   '<div class="tform-grid">'+f('t_presetmin2','Minutes per preset',d.preset_min,1)+f('t_warmest2','Warmest preset °C',d.warmest_c,1,'hold-only')+'</div>'
   +hint('Only the presets are tested again; the voltage curve isn’t changed. Each preset starts at the curve’s voltage for its clock. Warmer than the search’s temperature, chips make more errors, so a preset may need one voltage step more, or one clock step less (that’s the right answer for that temperature, not a wrong reading). Cooler is always safe, with louder fans.','','','Warmer or cooler than the search'),'rebuild-only','t_sec7b')
  +'<label class="tcheck" style="margin-top:1rem"><input type="checkbox" id="t_dry"'+(d.dry_run?' checked':'')+'> Dry run: reads chips and logs what it would do, writes nothing (recommended first)</label>'
  +'<div class="row" style="margin-top:.7rem;align-items:center"><button class="primary" onclick="tunerReview()">Review before starting</button><span class="muted" id="t_est"></span></div>'
  +'<div id="t_reviewbox"></div>';
 tunerWizUI();
}
function tunerWizUI(){const m=modeVal(),hold=fanVal()==='hold';
 document.querySelectorAll('#tunerbody .mode-card').forEach(c=>c.classList.toggle('on',c.querySelector('input').checked));
 const vis=(sel,on)=>document.querySelectorAll('#tunerbody '+sel).forEach(e=>e.classList.toggle('hide',!on));
 vis('.search-only',m==='map');vis('.confirm-only',m==='confirm');vis('.rebuild-only',m==='rebuild');vis('.not-rebuild',m!=='rebuild');
 vis('.base-custom',baseVal()==='own');
 vis('.hold-only',hold);
 const bp=$('t_buildpresets');vis('.presets-only',m==='rebuild'||(bp&&bp.checked));
 if($('t_cpv')){$('t_cpv').textContent=B2T()?boxVoltsTxt(+($('t_cmv')||{}).value||0):(((+($('t_cmv')||{}).value||0)-300)+' mV');}
 // renumber the visible sections 1..n, like the app
 let n=0;document.querySelectorAll('#tunerbody .tsec').forEach(sc=>{if(!sc.classList.contains('hide')){n++;const k=sc.querySelector('.tnum');if(k)k.textContent=n;}});
 const pt=$('t_presettemps');if(pt){const lo=+(($('t_fantarget')||{}).value||70),hi=+(($('t_warmest')||{}).value||75);
  pt.textContent=hold?('Each preset is held at its own temperature while it’s tested, spread from your target ('+lo+' °C) up to the warmest ('+hi+' °C): High '+lo+' · Middle '+Math.round(lo+(hi-lo)/3)+' · Low '+Math.round(lo+2*(hi-lo)/3)+' · Lowest power '+hi+' °C, and always at least 12 °C below the abort.'):'';}
 updateEstimate();}
function tunerCfg(){const n=id=>+(($(id)||{}).value||0);const b=id=>!!(($(id)||{}).checked);const m=modeVal();
 const hold=n('t_hold')||25,cm=n('t_confirmmin');
 return Object.assign({},TUNERDEF,{
  mode:m,dry_run:b('t_dry'),
  baseline_src:baseVal(),baseline_mhz:n('t_basemhz'),baseline_mv:n('t_basemv')-TGAP(),baseline_min:n('t_base'),
  lowest_mhz:n('t_low'),highest_mhz:n('t_high'),step:n('t_step'),min_mv:n('t_minmv')-TGAP(),max_mv:n('t_maxmv')-TGAP(),mv_step:n('t_mvstep'),find_lowest:true,top_down:b('t_topdown'),
  chip_trim:b('t_chiptrim'),chip_trim_max:n('t_ctmax')||3,chip_trim_deep:parseFloat(($('t_ctdeep')||{}).value)||50,
  confirm_win:b('t_confirmwin'),confirm_x:cm>0?cm/hold:2,pv_gap:TGAP(),
  confirm_mhz:n('t_cmhz'),confirm_mv:n('t_cmv')-TGAP(),confirm_hours:n('t_chours')||4,
  hold_min:hold,sigma:n('t_sigma'),abort_c:n('t_abort'),easy_noisy:b('t_easy'),
  fan_hold:fanVal()==='hold',fan_target_c:n('t_fantarget'),fan_min_pct:n('t_fanmin'),hold_on:(($('t_holdon')||{}).value||'board'),
  build_presets:(m==='rebuild')||b('t_buildpresets'),
  preset_min:m==='rebuild'?n('t_presetmin2'):n('t_presetmin'),warmest_c:m==='rebuild'?n('t_warmest2'):n('t_warmest')});}
function estHours(c){let tests=0;
 if(c.mode==='map'){const clocks=Math.max(1,Math.floor((c.highest_mhz-c.lowest_mhz)/Math.max(1,c.step))+1);const vsteps=Math.max(1,Math.floor((c.max_mv-c.min_mv)/Math.max(1,c.mv_step))+1);tests=c.top_down?clocks*Math.min(vsteps,4):clocks*(1+Math.min(vsteps-1,1.5)*(c.settle_min+5)/Math.max(1,c.hold_min+c.settle_min));} // bottom-up: one full pass per clock, plus short fails on the way up
 else if(c.mode==='confirm')tests=Math.max(1,Math.ceil((c.confirm_hours||4)*60/Math.max(1,c.hold_min)));
 const presets=c.build_presets?4:0;
 const conf=(c.mode==='map'&&c.confirm_win)?(c.hold_min*(c.confirm_x||2)+c.settle_min):0;
 const mins=c.baseline_min+tests*(c.hold_min+c.settle_min)+conf+presets*(c.preset_min+Math.max(c.settle_min,c.fan_hold?5:0));
 return mins/60;}
function updateEstimate(){try{const h=estHours(tunerCfg());$('t_est').textContent='roughly '+(h<1?Math.round(h*60)+' min':((h*0.55).toFixed(1)+'–'+h.toFixed(1)+' h'))+' (failed steps end early)';}catch(e){}}
function tunerReview(){const c=tunerCfg(),b2=B2T(),pv=mv=>mv+c.pv_gap,vt=mv=>b2?boxVoltsTxt(mv):('PV '+pv(mv)); // Box II: the V-code in volts, no PV
 const base=c.baseline_src==='own'?(c.baseline_mhz+' MHz · '+vt(c.baseline_mv)):(c.baseline_src==='current'?'what it runs now':'stock'+(TUNERDEF._stock?(' ('+TUNERDEF._stock.mhz+' MHz · '+(b2?boxVoltsTxt(TUNERDEF._stock.mv):'PV '+TUNERDEF._stock.pv)+')'):''));
 const kv=[['Miner',isOtherModel()?'this '+hesc(modelName()):'this SC Lite'],['Option',{map:'Map every clock and voltage',confirm:'Confirm one setting for hours',rebuild:'Rebuild the presets from the last search'}[c.mode]],['Baseline',base+' · '+c.baseline_min+' min'],['Fans',c.fan_hold?('hold '+c.fan_target_c+' °C on the '+(c.hold_on==='chip'?'hottest chip':'board sensor')+' · never below '+c.fan_min_pct+' %'):'left alone'],['Safety','stops at '+c.abort_c+' °C'+(c.dry_run?' · <b>dry run: writes nothing</b>':'')]];
 const steps=[];
 if(c.mode!=='rebuild')steps.push('<b>Baseline</b>: runs '+base+' for '+c.baseline_min+' min and learns every chip’s normal error rate. Nothing can fail here.');
 if(c.mode==='map'){steps.push('<b>Map</b> '+c.lowest_mhz+'–'+c.highest_mhz+' MHz in '+c.step+' MHz steps. At each clock: the lowest clean voltage between '+(b2?(boxVoltsTxt(c.max_mv)+' and '+boxVoltsTxt(c.min_mv)+' (steps of '+b2StepTxt(c.mv_step)+')'):('PV '+pv(c.min_mv)+' and PV '+pv(c.max_mv)+' ('+c.mv_step+' mV steps)'))+(c.top_down?(c.find_lowest?', from the baseline voltage up then down':', only raised'):', from the bottom up')+'. Each test: '+c.settle_min+' min to settle, then '+c.hold_min+' min watched at sensitivity '+c.sigma+'.');
  if(c.chip_trim)steps.push('<b>Weak chips</b>: a test that fails on one chip is run again with that chip slowed 12.5 MHz at a time (to −'+(c.chip_trim_deep||50)+' MHz, up to '+c.chip_trim_max+' chips per board). A trim stays only if the test then passes; at the end each is checked at the best setting and lifted if it isn’t needed there. The trims stay only with a confirmed setting: if the run is stopped, aborted or ends without one, they come off again with the clock and voltage, so the miner is as it was before the run.');
  if(c.confirm_win)steps.push('<b>Confirm the winner</b> for '+Math.round(c.confirm_x*c.hold_min)+' min. If it fails, the next-best passing setting is confirmed instead.');}
 if(c.mode==='confirm')steps.push('<b>Confirm</b> '+c.confirm_mhz+' MHz · '+vt(c.confirm_mv)+' for '+c.confirm_hours+' h: '+Math.max(1,Math.round(c.confirm_hours*60/Math.max(1,c.hold_min)))+' rounds of '+c.hold_min+' min, a failed round gets one retest. Ends CONFIRMED (the miner stays on it) or REJECTED (back to the baseline).');
 if(c.build_presets)steps.push('<b>Presets</b>: High, Middle, Low and Lowest power built '+(c.mode==='rebuild'?'from the last search’s curve':'from the curve')+' and tested '+c.preset_min+' min each'+(c.fan_hold?(', held '+c.fan_target_c+'–'+c.warmest_c+' °C'):'')+'.');
 steps.push('<b>Finish</b>: '+(c.mode==='confirm'?'a confirmed setting stays on the miner; otherwise the miner goes back to what it ran before the run.':c.mode==='map'?(c.confirm_win?'the miner stays on the confirmed setting; if nothing is confirmed, or the run stops early, it goes back to what it ran before.':'the miner stays on the best clean setting; if the run stops early, it goes back to what it ran before.'):'the miner goes back to what it ran before the run.'));
 const h=estHours(c);
 let o='<div class="planbox" style="margin-top:14px"><div class="hdr" style="margin-bottom:6px"><b>Review</b><span class="muted">nothing changes until you press Start</span></div>'
  +'<dl class="kvline">'+kv.map(r=>'<dt>'+r[0]+'</dt><dd>'+r[1]+'</dd>').join('')+'</dl>'
  +'<ol class="tplan">'+steps.map(x=>'<li>'+x+'</li>').join('')+'</ol>'
  +'<p class="muted" style="font-size:12px">'+(h<1?('About '+Math.round(h*60)+' min'):('About '+(h*0.55).toFixed(1)+'–'+h.toFixed(1)+' h'))+' (failed steps end early). Close the miner’s own settings page while it runs.</p>'
  +'<div class="row" style="margin-top:10px"><button class="primary" onclick="startTuner()">'+(c.dry_run?'Start dry run':'Start tuning')+'</button><button onclick="tunerBack()">Back</button></div></div>';
 $('t_reviewbox').innerHTML=o;$('t_reviewbox').scrollIntoView({block:'nearest'});}
function tunerBack(){$('t_reviewbox').innerHTML='';}
async function startTuner(){const c=tunerCfg();window._tcfg=c;
 const warn=c.dry_run?'Start a DRY-RUN? It reads chips and logs what it would do, writing nothing.':
   '⚠ Start a LIVE tuning run?\n\nIt changes clock/voltage repeatedly and judges from live hardware errors. It restores your baseline on stop or abort (a confirmed setting stays). Don’t change the miner from any other program or page meanwhile, and supervise it.';
 if(!confirm(warn))return;
 const {j}=await jpost('/api/tuner',{action:'start',config:c});
 if(j.ok){$('tunernote').textContent=j.note||'started';$('t_reviewbox').innerHTML='';setTimeout(pollTunerOnce,800);if(window._tv)clearInterval(window._tv);window._tv=setInterval(pollTunerOnce,5000);}
 else toast('could not start: '+(j.err||''),true);}
async function stopTuner(){if(!confirm('Stop the tuning run?\n\nThe miner goes back to what it ran before the run: its clock, voltage and fans, and any chip trims the run made come off again.'))return;
 const {j}=await jpost('/api/tuner',{action:'stop'});if(j.ok)$('tunernote').textContent=j.note||'stopping';}
async function pollTunerOnce(){try{const r=await fetch('/api/tuner');const j=await r.json();if(j.ok){updateTunerFiles(j);updateTunerStatus(j.status);}}catch(e){}}
function planTxt(m,mv,pv){return B2T()?(m+' MHz · '+boxVoltsTxt(+mv||0)):(m+' MHz · PV '+(pv||((+mv||0)+300)));}
function chipLbl(b,i){return 'board '+b+' chip '+i;}
function planParamsHtml(s){const c=s.cfg||window._tcfg||{},md=s.mode||c.mode,rows=[],b=s.base||{};
 rows.push(['Option',{map:'Map every clock and voltage',confirm:'Confirm one setting',rebuild:'Rebuild the presets from the last search'}[md]||md||'—']);
 if(B2T()){ // Box II: V-codes (the tuner's views give codes), shown in volts; no PV and no mV row
  if(b.mhz||s.base_mhz)rows.push(['Baseline',(b.mhz||s.base_mhz)+' MHz'+(b.mv?(' · '+boxVoltsTxt(b.mv)):'')+(c.baseline_min?(' · '+c.baseline_min+' min'):'')]);
  if(md==='map'&&c.lowest_mhz){rows.push(['Clocks',c.lowest_mhz+'–'+c.highest_mhz+' MHz in '+c.step+' MHz steps']);rows.push(['Voltage',boxVoltsTxt(c.max_mv)+' – '+boxVoltsTxt(c.min_mv)+' in steps of '+b2StepTxt(c.mv_step||1)]);}
  if(md==='confirm'&&c.confirm_mhz)rows.push(['Confirm',c.confirm_mhz+' MHz · '+boxVoltsTxt(c.confirm_mv)+' for '+(c.confirm_hours||4)+' h']);}
 else{
 if(b.mhz||s.base_mhz)rows.push(['Baseline',(b.mhz||s.base_mhz)+' MHz'+(b.pv?(' · PV '+b.pv):b.mv?(' · PV '+(b.mv+(c.pv_gap||300))):'')+(c.baseline_min?(' · '+c.baseline_min+' min'):'')]);
 if(md==='map'&&c.lowest_mhz){rows.push(['Clocks',c.lowest_mhz+'–'+c.highest_mhz+' MHz in '+c.step+' MHz steps']);rows.push(['Voltage','PV '+(c.min_mv+(c.pv_gap||300))+'–'+(c.max_mv+(c.pv_gap||300))+' in '+c.mv_step+' mV steps']);}
 if(md==='confirm'&&c.confirm_mhz)rows.push(['Confirm',c.confirm_mhz+' MHz · PV '+(c.confirm_mv+(c.pv_gap||300))+' for '+(c.confirm_hours||4)+' h']);}
 if(c.pv_gap&&!B2T())rows.push(['mV','sent along as PV − '+c.pv_gap+' (stock gap; only PV moves the boards’ voltage)']);
 if(c.hold_min)rows.push(['Tests',c.hold_min+' min each · sensitivity '+c.sigma+(c.easy_noisy?' · easier on noisy chips':'')]);
 if(c.fan_hold!=null)rows.push(['Fans',c.fan_hold?('hold '+c.fan_target_c+' °C on the '+(c.hold_on==='chip'?'hottest chip':'board sensor')+' · never below '+c.fan_min_pct+'%'):'left alone']);
 if(c.build_presets!=null&&md!=='confirm')rows.push(['Presets',c.build_presets?(c.preset_min+' min each · up to '+c.warmest_c+' °C'):'off']);
 if(c.abort_c)rows.push(['Safety','stops at '+c.abort_c+' °C'+(s.dry_run?' · dry run':'')]);
 if(s.started)rows.push(['Started',s.started]);
 return rows.map(r=>'<div><span class="k">'+hesc(r[0])+'</span><span class="v">'+hesc(r[1])+'</span></div>').join('');}
const RPIC={pass:'✓',fail:'✗',run:'●',todo:'–',skip:'⤼',cut:'■'},RPWD={pass:'done',fail:'failed',run:'running now',todo:'not done yet',skip:'skipped',cut:'cut short'};
function planStepsHtml(steps){return steps.map((ph,pi)=>{const st=ph.state||'todo';let kids='';
  // one row per clock (or preset): its marker, its name, then each voltage tried there in order
  (ph.kids||[]).forEach(k=>{const ks=k.state||'todo';
   let att=(k.kids||[]).map(v=>{const vs=v.state||'todo';
     return '<span class="st-'+vs+'" title="'+hesc(RPWD[vs]||'')+'">'+(RPIC[vs]||'–')+'</span> <b>'+hesc(v.label||'')+'</b>'+(vs==='run'?' · running now':'')+(v.note?' · '+hesc(v.note):'');}).join(' &nbsp;→&nbsp; ');
   if(k.note)att+=(att?' · ':'')+hesc(k.note);
   if(!att&&ks==='todo')att='not done yet';
   kids+='<div class="rp-item"><span class="ic st-'+ks+'" title="'+hesc(RPWD[ks]||'')+'">'+(RPIC[ks]||'–')+'</span><span class="lbl">'+hesc(k.label||'')+'</span><span class="att">'+att+'</span></div>';});
  // the long "how this step works" text folds away; the step's own note (what happened) stays visible
  const how=ph.desc?'<details class="d" data-k="'+pi+'"><summary>how this step works</summary><p>'+hesc(ph.desc)+'</p></details>':'';
  return '<div class="rp-step'+(st==='run'?' now':'')+'"><span class="ic st-'+st+'" title="'+(RPWD[st]||'')+'">'+(RPIC[st]||'–')+'</span><div><div class="t">'+hesc(ph.label||'')+'<small>'+(RPWD[st]||'')+'</small></div>'
   +(ph.note?'<div class="n">'+hesc(ph.note)+'</div>':'')+how+(kids?'<div class="rp-items">'+kids+'</div>':'')+'</div></div>';}).join('');}
// setPlanSteps: redraw only when something changed (so the running marker keeps breathing smoothly and an
// opened "how this step works" stays open)
function setPlanSteps(steps){const el=$('planSteps');const h=planStepsHtml(steps);if(el._h===h)return;
 const open=[...el.querySelectorAll('details[open]')].map(d=>d.dataset.k);el.innerHTML=h;el._h=h;
 open.forEach(k=>{const d=el.querySelector('details[data-k="'+k+'"]');if(d)d.open=true;});}
function tChipMap(chips){const byB={};chips.forEach(c=>{(byB[c.b]=byB[c.b]||[]).push(c);});let h='';
 Object.keys(byB).sort((a,b)=>a-b).forEach(b=>{const list=byB[b].slice().sort((x,y)=>x.i-y.i),tot=list.reduce((a,c)=>a+c.gained,0),over=list.filter(c=>c.limit>0&&c.gained>c.limit).length;
  h+='<div class="board-row"><div class="board-label">Board '+b+'<span class="muted">+'+tot+' errors'+(over?(' · '+over+' over'):'')+'</span></div><div class="tcells" style="--cols:'+list.length+';--half:'+Math.ceil(list.length/2)+'">';
  list.forEach(c=>{const r=c.limit>0?c.gained/c.limit:0,ov=c.limit>0&&c.gained>c.limit,bin=c.gained<=0?-1:Math.min(5,Math.max(0,Math.ceil(r*6)-1));
   h+='<div class="tcell'+(ov?' over':(bin>=3?' strong':''))+'" style="'+(!ov&&bin>=0?'background:var(--ramp-'+bin+')':'')+'" title="'+chipLbl(c.b,c.i)+': +'+c.gained+' new error'+(c.gained===1?'':'s')+(c.limit>0?(' of a limit of '+c.limit.toFixed(1)+' ('+Math.round(c.pct)+'%)'):'')+(ov?' · over the limit: this step fails':'')+'">'+(ov?'✕':'')+'</div>';});
  h+='</div></div>';});return h;}
function tMapLegend(){return '<span><span class="sw" style="background:var(--cell-empty);box-shadow:inset 0 0 0 1px var(--cell-edge)"></span>no new errors</span>'
 +'<span>few<span class="ramp">'+[0,1,2,3,4,5].map(i=>'<span class="sw" style="background:var(--ramp-'+i+')"></span>').join('')+'</span>at its limit</span>'
 +'<span><span class="sw" style="background:var(--over);color:#fff;text-align:center;font-size:9px;line-height:13px">✕</span>over its limit (fails the test)</span>';}
// tcfTxt/tcfWord: a chips_for/chips_last record as text, and its result as [pill text, pill class]
function tcfTxt(cf){if(B2T())return cf&&cf.mhz?(cf.phase==='baseline'?'the baseline ('+cf.mhz+' MHz'+(cf.mv?' · '+boxVoltsTxt(cf.mv):'')+')':cf.mhz+' MHz'+(cf.mv?' · '+boxVoltsTxt(cf.mv):'')):''; // Box II: the clock and the level the miner is given (chartFor.mv)
 return cf&&cf.mhz?(cf.phase==='baseline'?'the baseline ('+cf.mhz+' MHz · PV '+cf.pv+')':cf.mhz+' MHz · PV '+cf.pv):'';}
function tcfWord(cf){return {clean:['PASSED','ok'],failed:['FAILED','bad'],aborted:['ABORTED','bad'],stopped:['STOPPED','warn']}[cf.result]||[cf.result||'ended',''];}
function tAgo(x){return x<120?x+' s':x<7200?Math.round(x/60)+' min':(x/3600).toFixed(1)+' h';}
// tunerStuck: a run logs a line every poll while it tests; minutes of silence mean it's stuck (a frozen
// timer otherwise looks like a slow test)
function tunerStuck(s){const el=$('tstuck');if(!el)return;const q=s&&s.running?(s.quiet_s||0):0,lim=Math.max(180,6*((s&&s.poll_s)||15));
 if(q<lim){el.classList.add('hide');return;}
 el.classList.remove('hide');el.innerHTML='⚠ <b>No progress for '+tAgo(q)+'</b>: no reading or log line since '+hesc(((s.log||[]).slice(-1)[0]||'').slice(0,8))+'. A test logs a reading every '+(s.poll_s||15)+' s. Press Stop. The panel ends a run with no sign of life for 10 minutes by itself; if it’s still here after that, reboot the miner (Settings → Restart &amp; reboot) and send the tuner log (Tuner log → For sending).';}
function updateTunerStatus(s){tunerStuck(s);
 if(!s){$('tunernote').textContent='';$('curPanel').classList.remove('hide');$('curEmpty').textContent='Not running. Start a run below.';$('bestPanel').classList.remove('hide');$('bestBody').innerHTML='<span class="muted">No passing setting yet.</span>';tmapUpdate(null);return;}
 if(window._tv&&!s.running){clearInterval(window._tv);window._tv=null;}
 $('tunernote').textContent=s.running?('running · '+s.phase+(s.dry_run?' · dry-run':'')):'';
 const cur=s.cur,cp=$('curPanel');
 if(s.running&&!window._tv)window._tv=setInterval(pollTunerOnce,5000); // keep updating however the page was opened
 $('btnStopCur').classList.toggle('hide',!s.running);
 if(s.running&&cur){
  cp.classList.remove('hide');$('curEmpty').classList.add('hide');$('curBody').classList.remove('hide');
  $('curPlan').textContent=planTxt(cur.mhz,cur.mv,cur.pv);
  $('curPhase').textContent=cur.phase;
  if(cur.settling){
   const sm=cur.settle_min||3,el=cur.elapsed_min||0;
   $('curTitle').textContent='Settling the next setting';
   $('curBar').style.width=Math.min(100,el/sm*100)+'%';
   $('curProgress').textContent='settling '+Math.min(el,sm).toFixed(1)+' / '+sm+' min · errors are counted once it has settled';
   ['kvHash','kvTemp','kvChip','kvAll'].forEach(k=>$(k).textContent='—');
   $('curVerdict').textContent='settling';$('curVerdict').className='pill';
   $('tchart').innerHTML='<div class="tc-empty">The new clock and voltage settle for '+sm+' minutes first. The chart starts from zero when counting begins.</div>';$('tchartNote').textContent='';
   const lf=s.chips_last||{},lw=lf.done&&lf.ended?tcfWord(lf):null;
   $('tchartFor').innerHTML=(lw?('Last test: <b>'+lw[0]+'</b> · '+hesc(tcfTxt(lf))+(lf.reason&&lf.result!=='clean'?' · '+hesc(lf.reason):'')+(s.now?' · ended '+tAgo(Math.max(0,s.now-lf.ended))+' ago':'')+'<br>'):'')
    +'Chip errors for '+hesc(planTxt(cur.mhz,cur.mv,cur.pv))+': counting starts after the settle';
  }else{
  $('curTitle').textContent=cur.phase==='baseline'?'Baseline (learning normal error rates)':(/^preset/.test(cur.phase)?('Preset test: '+cur.phase.replace(/^preset\s*/,'')):(cur.phase==='confirm'?'Confirming':'Current test'));
  const hold=cur.hold_min||25,el=cur.elapsed_min||0;
  $('curBar').style.width=Math.min(100,el/hold*100)+'%';
  const ago=(s.now&&cur.updated)?Math.max(0,s.now-cur.updated):null;
  $('curProgress').textContent=el.toFixed(1)+' / '+hold+' min · live: chips read every '+(s.poll_s||15)+' s'+(ago!=null?(', last '+tAgo(ago)+' ago'):'');
  $('kvHash').textContent=s.stat&&s.stat.avg_ghs?fmtHRc(s.stat.avg_ghs):'—';
  $('kvTemp').textContent=(s.stat&&s.stat.max_t?(+s.stat.max_t).toFixed(1)+' °C':'—')+(cur.fan?('  ·  fan '+cur.fan+' %'+(cur.target_c?(' → '+Math.round(cur.target_c)+' °C'):'')):'');
  const top=(s.chips&&s.chips.length)?s.chips[0]:null,lim=top?top.limit:0;
  if(cur.phase==='baseline'||!lim){$('kvChip').textContent=top&&top.gained?(chipLbl(top.b,top.i)+': +'+top.gained):'—';$('curVerdict').textContent='learning';$('curVerdict').className='pill';}
  else{const n=top.gained;$('kvChip').textContent=n?(chipLbl(top.b,top.i)+': '+n+' of '+lim.toFixed(1)):'none yet';
   const v=n>lim?['over limit','bad']:n>=0.6*lim?['close','warn']:['clean so far','ok'];$('curVerdict').textContent=v[0];$('curVerdict').className='pill '+v[1];}
  let all='+'+((s.stat&&s.stat.total)||0);
  if(s.stat&&s.stat.watched_min>0&&cur.phase!=='baseline'){const em=s.stat.total/s.stat.watched_min,bm=s.base_rate_min||0;all+='  ·  '+em.toFixed(2)+'/min'+(bm>0?(' vs '+bm.toFixed(2)+' normal'):'');}
  $('kvAll').textContent=all;
  // which test the chart is for: this one while it's counting; once it has ended, say so (until the next
  // setting loads) so its bars can't be read as the next test's
  const cf=s.chips_for||{},same=cf.mhz===cur.mhz&&(!cf.pv||!cur.pv||cf.pv===cur.pv);
  const cfTxt=tcfTxt(cf);
  if(cf.done&&same){const w=tcfWord(cf);
   $('curTitle').textContent='Test finished: '+cfTxt;$('curBar').style.width='100%';if(cf.ended&&s.now)$('curProgress').textContent='ended '+tAgo(Math.max(0,s.now-cf.ended))+' ago · the next step loads in a moment';$('curVerdict').textContent=w[0];$('curVerdict').className='pill '+w[1];
   $('tchartFor').innerHTML='<b>'+w[0]+'</b> · chip errors from the test that just ended, '+hesc(cfTxt)+(cf.reason&&cf.result!=='clean'?' · '+hesc(cf.reason):'');}
  else $('tchartFor').textContent=cfTxt&&same?('Chip errors for this test: '+cfTxt+(cf.started?' · counting since '+new Date(cf.started*1000).toTimeString().slice(0,5):'')):'';
  if(cur.phase!=='baseline'&&s.chips&&s.chips.some(c=>c.limit>0)&&(same||!cf.mhz)){$('tchart').innerHTML=tunerChart(s.chips);$('tchartNote').textContent='';}
  else{$('tchart').innerHTML='<div class="tc-empty">'+(cur.phase==='baseline'?'Baseline: nothing can fail here. It’s learning each chip’s normal error rate, which sets each chip’s limit for the tests.':'Waiting for the first reading of this test…')+'</div>';$('tchartNote').textContent='';}
  }
 }else{
  $('curBody').classList.add('hide');$('curEmpty').classList.remove('hide');$('curTitle').textContent='Current step';
  $('curEmpty').textContent=s.running?'Starting… the first reading appears a few minutes after each step begins (up to about 5 while the fans settle).':(s.message?((/^the tuning run was stuck|watchdog/.test(s.message)?'⚠ ':'Finished. ')+s.message):'Not running. Start a run below.');
  cp.classList.remove('hide');
 }
 const pp=$('planPanel');
 if(s.plan&&s.plan.length){pp.classList.remove('hide');
  $('planState').textContent=s.running?'running':'finished';$('planState').className='pill '+(s.running?'warn':'ok');
  {const ph=planParamsHtml(s),pe=$('planParams');if(pe._h!==ph){pe.innerHTML=ph;pe._h=ph;}}setPlanSteps(s.plan);}
 else pp.classList.add('hide');
 tmapUpdate(s);
 const bp=$('bestPanel');
 if(s.best){bp.classList.remove('hide');
  const lastClean=(s.results||[]).slice().reverse().find(r=>r.result==='clean'&&r.mhz===s.best.mhz&&r.mv===s.best.mv);
  let bh='<span class="bestbig">'+planTxt(s.best.mhz,s.best.mv,s.best.pv)+'</span><span class="pill '+(s.running?'warn':'ok')+'" style="margin-left:10px">'+(s.running?'not confirmed yet':'best clean setting')+'</span>'
   +(lastClean&&lastClean.ths?'<span class="muted" style="margin-left:10px">'+lastClean.ths.toFixed(2)+' TH/s</span>':'');
  if(s.curve&&s.curve.length)bh+='<div class="muted" style="font-size:12px;margin-top:10px">'+(B2T()?'Clean curve (MHz / voltage): '+s.curve.map(p=>p.mhz+' / '+boxVoltsTxt(p.mv)).join(' · '):'Clean curve (MHz / PV): '+s.curve.map(p=>p.mhz+' / '+(p.pv||p.mv+300)).join(' · '))+'</div>';
  if(s.trims&&s.trims.length)bh+='<div class="muted" style="font-size:12px;margin-top:6px">Chip trims kept by this run: '+s.trims.map(t=>'b'+t.chain+'.c'+t.chip+' '+(t.mhz>0?'+':'')+t.mhz+' MHz').join(' · ')+' <span style="opacity:.8">(Controls → Chip trims)</span></div>';
  bh+='<p class="muted" style="font-size:11.5px;margin:10px 0 0">'+(!((s.cfg||{}).build_presets||(s.cfg||{}).mode==='rebuild')?'This run doesn\u2019t build presets.':s.running?'Presets are built and tested at the end of the run.':'Built presets are saved to this miner: see <a href="#" onclick="showTab(\'presets\');return false">Presets</a>.')+'</p>';
  $('bestBody').innerHTML=bh;}
 else{bp.classList.remove('hide');const np=(s.results||[]).filter(r=>r.result==='clean').length;$('bestBody').innerHTML='<span class="muted">'+(np?np+' setting'+(np>1?'s':'')+' passed so far: the best is chosen once a clock is finished.':'No passing setting yet.')+'</span>';}
 $('tunerpanel').classList.toggle('hide',!!s.running);
 const rp=$('tresPanel');
 if(s.results&&s.results.length){rp.classList.remove('hide');
  $('tunerresults').innerHTML=tunerResults(s.results,s.base_rate_min);
  $('tresnote').textContent=s.results.length+' test'+(s.results.length===1?'':'s')+(s.base_rate_min>0?(' · baseline '+s.base_rate_min.toFixed(2)+' err/min'):'');}
 else rp.classList.add('hide');
 const meta=$('tlogmeta');if(meta){const nt=s.ntests||0;
  meta.textContent=s.running?('● running · '+s.phase+(nt?(' · '+nt+' tests'):'')):(nt?('last run · '+nt+' test'+(nt===1?'':'s')+(s.nread?(' · '+s.nread+' readings'):'')):'');}
 // stream the log into the permanent panel (persists after the run and across tab switches)
 const lg=$('tlog');if(lg&&s.log){
  const atBottom=lg.scrollHeight-lg.scrollTop-lg.clientHeight<40;
  let from=0;
  if(window._tlast!==undefined){const i=s.log.lastIndexOf(window._tlast);if(i>=0){from=i+1;}else{lg.textContent='';}}else{lg.textContent='';}
  for(let i=from;i<s.log.length;i++){const d=document.createElement('div');d.textContent=s.log[i];lg.appendChild(d);}
  while(lg.childElementCount>600)lg.removeChild(lg.firstChild);
  if(s.log.length)window._tlast=s.log[s.log.length-1];
  if(atBottom)lg.scrollTop=lg.scrollHeight;
 }}
async function loadHashrate(){try{const r=await fetch('/api/hashrate');if(r.status===401){boot();return;}const d=await r.json();if(d.ok){renderHR(d);if(window._lastDash)renderRejected(window._lastDash);}}catch(e){}}
// ===== dashboard (miner page parity) =====
function fmtWhen(t){if(!t)return '–';const d=new Date(t*1000),now=new Date(),p2=n=>String(n).padStart(2,'0'),hm=p2(d.getHours())+':'+p2(d.getMinutes());
 if(d.toDateString()===now.toDateString())return hm;
 if(Date.now()/1000-t<6*86400)return ['Sun','Mon','Tue','Wed','Thu','Fri','Sat'][d.getDay()]+' '+hm;
 return ['Jan','Feb','Mar','Apr','May','Jun','Jul','Aug','Sep','Oct','Nov','Dec'][d.getMonth()]+' '+d.getDate()+' '+hm;}
function fmtAgo(t){const s=Date.now()/1000-t;return s<3600?(Math.max(1,Math.round(s/60))+' min'):s<172800?((s/3600).toFixed(1)+' h'):(Math.round(s/86400)+' days');}
// tempParts: the miner reports three temperatures per board (tstemp-0/1/2). Its own debug log names them
// chip average, hottest chip and board sensor (CPB, a TMP112), but the first two always come out the same
// (the chips' average, whole degrees); the third is the board sensor (1/16 °C steps).
function tempParts(ts){const v=(ts||[]).map(t=>(t>=5&&t<150)?t:null),r=t=>t==null?'–':(Math.round(t*10)/10)+'°';
 const chips=v[0]!=null?v[0]:v[1],c2=v[1];
 return {chips:(c2!=null&&chips!=null&&Math.abs(c2-chips)>=1)?(r(Math.min(chips,c2))+'–'+r(Math.max(chips,c2))):r(chips),board:r(v.length>2?v[2]:null)};}
// ---- fan check (Controls & fan) and fan lock (Dashboard) ----
// Fan pair offset (fanpair.go): fans 2+3 run at fans 0+1 + offset; the board has two fan signals, not four
async function loadFanPair(){if(featBlock('fpbody',F().FanPairOffset,'The fan-pair offset'))return;
 try{const r=await fetch('/api/fanpair');renderFanPair(await r.json());}catch(e){if($('fpbody'))$('fpbody').innerHTML=busyHTML(OFFLINE);retryLater(loadFanPair);}}
function renderFanPair(v){const el=$('fpbody');if(!el)return;if(!v||!v.ok){el.innerHTML='<span class="muted">'+hesc((v&&v.err)||'unavailable')+'</span>';return;}
 $('fpnote').textContent=v.offset?((v.offset>0?'+':'')+v.offset+' %'):'off';
 el.innerHTML='<p class="muted sc-intro">The board has four fan plugs but only <b>two speed signals</b>: fans 0 and 1 always run at the same level, and so do fans 2 and 3. Fans at nearly the same speed beat against each other (a slow “wow-wow”); running one pair a little apart from the other moves that beat out of the annoying range. Fan curves, Controls, presets, the tuner and '+oemPoss()+' own fan control keep setting fans 0+1; fans 2+3 follow at the offset. They never go under the fan floor (or a tuning run’s lowest fan %), are never held below fans 0+1 when those run at 95 % or more, are never turned down while a board is at 88 °C or more, and are left to '+(oemLite()?'Goldshell':'the OEM fan control')+' on Idle, during its start-up fan test and while mining is locked after a fan problem.</p>'
  +'<div class="row" style="gap:8px;align-items:center;flex-wrap:wrap"><span>Fans 2+3 run</span><input type="number" id="fpoff" min="'+v.min+'" max="'+v.max+'" step="1" value="'+v.offset+'" style="width:5rem"><span>% from fans 0+1</span><button onclick="fanPairSave()">Save</button><span class="muted" style="font-size:12px">0 = off · '+v.min+' to +'+v.max+'</span></div>'
  +(v.levels?('<div class="muted" style="font-size:12px;margin-top:6px">Now: fan0 '+v.levels[0]+' % · fan1 '+v.levels[1]+' % · fan2 '+v.levels[2]+' % · fan3 '+v.levels[3]+' %'+(v.note&&!/^fans 0\+1/.test(v.note)?(' · '+hesc(v.note)):'')+'</div>'):'')
  +'<div class="muted" style="font-size:11.5px;margin-top:4px">Try 8–15 % either way. Lower (−) is quieter, higher (+) cools a little more; the fan curve keeps the boards at its temperature either way. A tuning run uses whatever offset is set when it starts.</div>';}
async function fanPairSave(){const o=parseInt($('fpoff').value,10);if(isNaN(o)){toast('enter a number',true);return;}const {j}=await jpost('/api/fanpair',{offset:o});if(!j||!j.ok){toast((j&&j.err)||'failed',true);return;}toast(o?('Fans 2+3 now run '+(o>0?'+':'')+o+' % from fans 0+1'):'Fan pair offset off');renderFanPair(j);setTimeout(loadFanPair,4000);}
async function loadFanCheck(){if(featBlock('fcbody',F().FanFit,'Fitting the fan check to your fans'))return;
 try{const r=await fetch('/api/fancheck');renderFanCheck(await r.json());}catch(e){if($('fcbody'))$('fcbody').innerHTML=busyHTML(OFFLINE);retryLater(loadFanCheck);}}
function renderFanCheck(v){const el=$('fcbody');if(!el||!v.ok)return;window._fc=v;
 const mx=v.maxrpm||[],sw=v.sweep||[],sg=v.suggest||[],w=v.watchdog||{},lk=v.lock||{};
 $('fcnote').textContent=v.fitted?'fitted to these fans':(v.saved?'fitted: after a reboot':oemPoss()+' figures');
 let h='<table class="ctbl" style="width:100%;font-size:12.5px"><tr><th>Fan</th><th>now</th><th>at boot, full speed</th><th>check uses (max RPM)</th><th>flags it under, at 100 %</th><th>fitted</th></tr>';
 const n=Math.max(mx.length,sw.length);
 for(let i=0;i<n;i++){const c=sw[i]||[],full=c[10],m=mx[i]||0,f=sg[i]||0,lim=Math.floor(m/125)*100,flag=w.status&(1<<i);
  h+='<tr><td>fan '+i+'</td><td>'+((w.fans||[])[i]!=null?(w.fans[i]+' RPM'+(flag?' <b style="color:var(--bad)">flagged</b>':'')):'–')+'</td><td>'+(full>0?full+' RPM':'–')+'</td><td>'+(m||'–')+'</td><td>'+(m?lim+' RPM':'–')+(full>0&&m&&full<=lim?' <b style="color:var(--bad)">(its own full speed is under this)</b>':'')+'</td><td>'+(f?f+' → under '+Math.floor(f/125)*100+' RPM':'<span class="muted">–</span>')+'</td></tr>';}
 h+='</table>';
 const can=sg.length&&sg.every(x=>x>0);
 h+='<div class="row" style="margin-top:10px;gap:8px;align-items:center;flex-wrap:wrap">'
  +'<button class="primary" '+(can?'':'disabled')+' onclick="fanCheckFit()">Fit the check to these fans</button>'
  +'<button onclick="fanCheckStock()" '+((v.fitted||v.saved)?'':'disabled')+'>Back to '+oemPoss()+' figures</button>'
  +'<button onclick="fanTestRerun()" title="Restarts '+oemOf()+' fan check: it forgets the fan speeds it recorded and runs its start-up fan test again (the miner goes to Idle for about 2 minutes, then back to its setting). Nothing is skipped.">Re-run '+oemOf()+' fan test</button>'
  +'<span class="muted" style="font-size:11.5px">'+(can?'The fitted figure keeps each fan’s own measured speeds 15 % clear of the line, never above stock. Takes effect at the next reboot.':'Needs the watchdog’s sweep at boot (every fan measured from 10 to 100 %), with every fan turning: reboot the miner with all fans connected.')+'</span></div>'
  +'<div style="margin-top:12px;border-top:1px solid var(--border);padding-top:10px"><label class="tcheck"><input type="checkbox" id="fclock" '+(lk.enabled!==false?'checked':'')+' onchange="fanLockSet(!this.checked)"> <b>Stop and lock mining on a fan failure</b> (recommended)</label>'
  +'<div class="muted" style="font-size:11.5px;margin-top:3px">When the watchdog flags a fan for a minute, the miner goes to Idle (hash boards powered down) and stays locked until you’ve checked the fans and unlocked it on the Dashboard. Off: the watchdog keeps stopping and restarting mining by itself.</div></div>';
 el.innerHTML=h;}
async function fanCheckFit(){const v=window._fc||{};const fit=(v.suggest||[]);
 if(!confirm('Fit the fan check to these fans?\n\nNew max RPM per fan: '+fit.join(', ')+' ('+(oemLite()?'Goldshell’s':OEM())+': '+(v.maxrpm||[]).join(', ')+').\nA fan is then flagged under '+fit.map(f=>Math.floor(f/125)*100).join(' / ')+' RPM at full speed.\n\nTakes effect at the next reboot.'))return;
 const {j}=await jpost('/api/fancheck',{fit:fit});if(!j.ok){toast(j.err||'failed',true);return;}renderFanCheck(j);toast(j.note||'saved');}
async function fanTestRerun(){if(!confirm('Re-run '+oemOf()+' fan test?\n\nIts fan check restarts: it forgets the speeds it recorded for each fan and tests them again. The miner goes to Idle for about 2 minutes while the fan check restarts and tests the fans, then goes back to its setting.'))return;
 const {j}=await jpost('/api/fancheck',{rerun:true});if(!j||!j.ok){toast((j&&j.err)||'failed',true);return;}toast(j.note||'restarted');if($('flmsg'))$('flmsg').textContent=j.note||'';}
async function fanCheckStock(){const {j}=await jpost('/api/fancheck',{stock:true});if(!j.ok){toast(j.err||'failed',true);return;}renderFanCheck(j);toast(j.note||'saved');}
async function fanLockSet(off){if(off&&!confirm('Turn off the fan lock? Fan failures are then left to '+oemOf()+' watchdog alone (it stops the miner over and over, but nothing holds it stopped).')){loadFanCheck();return;}const {j}=await jpost('/api/fancheck',{lock_off:off});if(!j.ok){toast(j.err||'failed',true);loadFanCheck();return;}toast(j.note||'saved');}
function fanLockUI(d){const L=d.fan_lock,id='fanlockdlg';let dlg=$(id);
 if(!L){if(dlg)dlg.remove();window._flDismiss=false;return;}
 if(window._flDismiss){return;}
 if(!dlg){dlg=document.createElement('div');dlg.id=id;dlg.style.cssText='position:fixed;inset:0;background:rgba(0,0,0,.6);z-index:999;display:flex;align-items:center;justify-content:center;padding:16px';document.body.appendChild(dlg);}
 if(dlg._at===L.at)return;dlg._at=L.at; // built once per lock: a refresh mustn't untick the box or wipe a result
 const when=new Date(L.at*1000).toLocaleString();
 dlg.innerHTML='<div class="panel" style="max-width:560px;width:100%;margin:0;border-color:var(--bad)"><div class="hdr"><b style="color:var(--bad)">⚠ Mining stopped: fan problem</b></div>'
  +'<div style="font-size:13px;line-height:1.55">'
  +'<p>At '+hesc(when)+' '+oemOf()+' watchdog had flagged a fan for a minute: <b>'+hesc((L.flagged||'').replace(/^the watchdog flags /,''))+'</b>'+(L.fans&&L.fans.length?' (fans read '+L.fans.join(' · ')+' RPM)':'')+'.</p>'
  +'<p>'+(L.idle?'The miner was put on <b>Idle</b>: the hash boards are powered down, so they make no heat.':'The miner <b>couldn’t</b> be put on Idle: check it.')+' Presets, Stock, Controls, the schedule and the tuner are <b>locked</b> until you unlock here.</p>'
  +'<p><b>Check:</b> is every fan turning? Its plug and speed wire'+(isOtherModel()?'':' (on the board’s fan header: 1 GND, 2 12V, 3 speed, 4 PWM)')+'?'+(featOff2(F().FanFit)?'':' Quieter replacement fans: Controls &amp; fan → Fan check → Fit the check to these fans.')+'</p>'
  +'<label class="tcheck" style="margin-top:6px"><input type="checkbox" id="flaccept" onchange="$(\'flbtn\').disabled=!this.checked"> I’ve checked the fans. I understand that mining with a fan that isn’t working can overheat and damage the hash boards.</label>'
  +'<div class="row" style="margin-top:12px;gap:8px"><button class="primary" id="flbtn" disabled onclick="fanUnlock()">Unlock</button><button onclick="fanTestRerun()" title="If the flagged fan is turning normally: '+oemOf()+' check may be using a speed it recorded at a bad moment. This makes it test the fans again.">Re-run fan test</button><button onclick="window._flDismiss=true;$(\'fanlockdlg\').remove()">Later</button><span class="muted" id="flmsg" style="font-size:12px;align-self:center"></span></div></div></div>';}
async function fanUnlock(){const {j}=await jpost('/api/fancheck',{unlock:true,accept:!!($('flaccept')||{}).checked});
 if(!j.ok){$('flmsg').textContent=j.err||'failed';return;}const dlg=$('fanlockdlg');if(dlg)dlg.remove();toast(j.note||'unlocked');if(typeof refresh==='function')refresh();}
function FMIN(){return (typeof FAN!=='undefined'&&FAN&&FAN.fan_min)||10;}
// fanUnit: the miner reports fan speed in RPM, a plan's fan in % (small numbers): the same rule everywhere
function fanUnit(list){return (list||[]).some(f=>f>200)?' RPM':' %';}
function fanLine(d){const w=d.watchdog||{},rpm=(w.fans&&w.fans.length?w.fans:null);
 const alive=(d.boards||[]).filter(b=>b.alive),bf=(alive[0]&&alive[0].fans||[]).filter(f=>f>0);
 const list=rpm||bf,unit=(rpm||bf.some(f=>f>200))?' RPM':' %';
 let h='fans '+(list.length?list.join(' · ')+unit:'—');
 if(w.status)h+='<br><span style="color:var(--bad)">⚠ '+hesc(w.flagged)+'</span>';
 else if(rpm)h+=' · OK';
 return h;}
// HW errors tile: the miner's own % and, when some of the errors came from a start-up warm-up (cold chips ramping
// to full clock), how many, and the % without them: that one says how the board is doing.
// a plan's voltage text: mV and PV on the SC Lite; on other models the figure is Goldshell's level number (not volts) and there is no PV
// Box II: the voltage in volts, from the C4 lug readings (the rail at the power lugs under load, 690 and 525 MHz):
// level 42 = 7.67 V, 43 = 7.61, 44 = 7.58, 45 = 7.55, 46 = 7.52, 61 = 7.03. Other levels are estimated from the
// line through them (about 33.5 mV per level); "≈" marks an estimate. Plain text: it also goes in button labels.
const BOX_V_MEAS={42:7.67,43:7.61,44:7.58,45:7.55,46:7.52,61:7.03};
// Box II: the plan's 4th field reads like a second fan RPM in Goldshell's plan text, but it is a voltage trim
function b2F4Note(){return 'On this model the plan’s 4th field (written like a second fan “RPM” in the OEM plan text) is a voltage-trim setting, not a fan speed: the panel always keeps it at the stock value.';}
function boxVoltsTxt(l){if(!l)return '—';const m=BOX_V_MEAS[l];const v=(m!=null)?m:(7.66-0.0335*(l-42));return (m!=null?'':'≈ ')+v.toFixed(2)+' V';}
// b2VOpts: the Box II voltage choices for a <select>: every level lo..hi shown as its volts, highest voltage first
// (a higher level is a lower voltage); each option's value is the level the page sends. b2StepTxt / b2StepOpts:
// a step of n levels in volts (about 0.03 V per level; the value sent is n).
function b2VOpts(sel,lo,hi){lo=lo||42;hi=hi||61;let o='';for(let l=lo;l<=hi;l++)o+='<option value="'+l+'"'+(l===+sel?' selected':'')+'>'+boxVoltsTxt(l)+'</option>';return o;}
function b2StepTxt(n){return '≈ '+(0.0335*(+n||1)).toFixed(2)+' V';}
function b2StepOpts(sel,max){let o='';for(let n=1;n<=(max||19);n++)o+='<option value="'+n+'"'+(n===(+sel||1)?' selected':'')+'>'+b2StepTxt(n)+' per step</option>';return o;}
function vLine(mv,pv){return isOtherModel()?boxVoltsTxt(mv):(mv+' mV · PV '+pv);}
function hwSub(d){const w=+d.hw_warm||0,l=+d.hw_warm_left||0,s=d.hw_pct_steady;
 if(l>0)return pct2(d.hw_pct)+'% · <span title="The chips have just started: for the first 3 minutes bad nonces are expected while they are cold and ramp to full clock. They are counted separately.">warming up, '+(l>=60?Math.ceil(l/60)+' min':l+' s')+' left</span>';
 if(w>0&&s!=null&&s>=0)return '<span title="'+w+' of these came in the first 3 minutes after the chips started (cold chips ramping to full clock). Without them the rate is '+pct2(s)+'%; with them, the miner’s own figure is '+pct2(d.hw_pct)+'%.">'+pct2(s)+'% since warm-up · '+w+' at start-up</span>';
 return pct2(d.hw_pct)+'%';}
function liveTiles(d){const mk=(k,v,sub)=>'<div class="kv"><div class="k">'+k+'</div><div class="v">'+v+'</div>'+(sub?'<div class="kv-sub">'+sub+'</div>':'')+'</div>';
 const alive=(d.boards||[]).filter(b=>b.alive);
 // the same readings the thermal guard acts on (maxBoardTemp): every board, a board that stopped hashing too, and
 // only 5–150 °C (-60, 0 and -150 are sensors that can't be read)
 let tmax=null,tb=null;(d.boards||[]).forEach(b=>(b.temps||[]).forEach(t=>{if(t>=5&&t<150&&(tmax==null||t>tmax)){tmax=t;tb=b;}}));
 const fans=alive.flatMap(b=>b.fans||[]).filter(f=>f>0),fav=fans.length?Math.round(fans.reduce((a,b)=>a+b,0)/fans.length):null;
 const now=d.ghs>0?d.ghs:((window._hrNow!=null&&window._hrNowT>=Date.now()/1000-(d.elapsed||0))?window._hrNow:0),hp=hrParts(now); // (a chart point from before the last restart isn't the rate now)
 const hrs=Math.floor(d.elapsed/3600),mins=Math.floor(d.elapsed%3600/60);
 return mk('Hashrate',hp.num+' <span class="muted" style="font-size:12px">'+hp.unit+'</span>',(hp.sub?(hp.sub+' now<br>'):'')+'avg since restart<br>'+fmtHRc(d.ghs_avg||d.ghs))
  +powerTile(d,now,mk)
  +mk('Temperature',tmax!=null?(tmax.toFixed(1)+'°C'+(tb&&!tb.alive?' <span class="muted" style="font-size:12px">board '+tb.id+', not hashing</span>':'')):'—',fanLine(d)+' · temp control '+(d.tempcontrol==null?'—':(d.tempcontrol?'on':'off')))
  +mk('Shares',(+d.accepted).toLocaleString()+' <span class="muted" style="font-size:12px">accepted</span>',(+d.rejected).toLocaleString()+' rejected'+(d.stale?(' · '+(+d.stale).toLocaleString()+' stale'):''))
  +mk('HW errors',(+d.hw_errors).toLocaleString(),hwSub(d))
  +mk('Uptime',hrs+'h '+mins+'m','since '+new Date(Date.now()-d.elapsed*1000).toLocaleString([],{month:'short',day:'numeric',hour:'2-digit',minute:'2-digit'}));}
// Power and efficiency: the estimate (calibrated to a wall-meter reading when one was given in Settings →
// Power), and J/TH from it and the hashrate shown above.
function powerTile(d,ghsNow,mk){const p=d.power||{};window._powF=p.factor||0;window._powMains=p.mains_v||100;
 if(featOff2(F().PowerEstimate))return mk('Power','—','not available on this model');
 if(isOtherModel())return powerTileBox2(p,d,ghsNow,mk);
 if(!p.watts)return mk('Power','—','shown while hashing');
 const ths=(ghsNow||d.ghs)/1000,jth=ths>0.01?Math.round(p.watts/ths):null;
 return mk('Power',(p.calibrated?'':'≈ ')+Math.round(p.watts)+' <span class="muted" style="font-size:12px">W</span>',
  (jth?('<b style="color:var(--text)">'+jth+' J/TH</b> · '):'')+f1(p.amps)+' A at '+Math.round(p.mains_v)+' V · '+(p.calibrated?'calibrated':'<a class="sublink" href="#" onclick="gotoPower();return false" title="Calibrate with a wall meter in Settings → Power &amp; calibration">estimate</a>'));}
// Box II: the setting's wall-meter reading, or Goldshell's figure for an uncalibrated stock mode, or none
function powerTileBox2(p,d,ghsNow,mk){if(!p.watts)return mk('Power','—',hesc(p.reason||'shown while hashing'));
 const ths=(ghsNow||d.ghs)/1000,jth=ths>0.01?Math.round(p.watts/ths):null;
 return mk('Power',(p.calibrated?'':'≈ ')+Math.round(p.watts)+' <span class="muted" style="font-size:12px">W</span>',
  (jth?('<b style="color:var(--text)">'+jth+' J/TH</b> · '):'')+f1(p.amps)+' A at '+Math.round(p.mains_v)+' V · '+(p.calibrated?'wall meter':'<a class="sublink" href="#" onclick="gotoPower();return false" title="The OEM’s published figure for this mode (±5 %). Calibrate with a wall meter in Settings → Power &amp; calibration">OEM spec</a>')+(p.low_hash?' · <span style="color:var(--warn)" title="'+hesc(p.low_hash_note||'')+'">low hashrate</span>':''));}
// the 24-hour hashrate graph: a fixed window ending now, y from 0, the hour on the clock along the bottom
// per-board health from the mining log (boardhealth.go): hottest chip, chips at start-up, re-inits, dropped results
function bhLines(d,b){const h=(d.board_health||[]).find(x=>x.id===b.id);if(!h)return '';let o='';
 if(h.max_temp)o+='<div class="line"><span class="muted" title="The hottest chip on the board, from the mining log">Hottest chip</span><span>'+f1(h.max_temp)+' °C</span></div>';
 if(h.chips>=0)o+='<div class="line"><span class="muted" title="Chips that answered when the board started ('+hesc(fmtWhen(h.chips_at))+'). A board can start with fewer than '+h.expected+' when its chain is broken part-way.">Chips found</span><span'+(h.chips<h.expected?' style="color:var(--bad);font-weight:600"':'')+'>'+h.chips+' of '+h.expected+'</span></div>';
 const bits=[];if(h.reinit_24h)bits.push(h.reinit_24h+' re-init'+(h.reinit_24h>1?'s':''));if(h.addr_fail_24h)bits.push(h.addr_fail_24h+' addressing fail'+(h.addr_fail_24h>1?'s':''));if(h.init_fail_24h)bits.push(h.init_fail_24h+' start-up fail'+(h.init_fail_24h>1?'s':''));
 if(h.dropped_24h)bits.push(h.dropped_24h.toLocaleString()+' dropped');
 if(bits.length)o+='<div class="line"><span class="muted" title="Last 24 h, from the mining log. Re-init: the driver restarted this board after 10 failed job sends or result reads. Dropped: results for a job the board was not working on (chips out of sync, or garbled on the cable); they are thrown away without counting as HW errors. Last one: '+hesc(fmtWhenS(h.last_problem))+'">Link problems</span><span style="color:var(--bad)">'+bits.join(' · ')+'<span class="muted" style="font-weight:400"> · last '+hesc(fmtWhen(h.last_problem))+'</span></span></div>';
 return o;}
function fmtWhenS(t){const d=new Date(t*1000),p2=n=>String(n).padStart(2,'0');return fmtWhen(t)+':'+p2(d.getSeconds());}
const HRNAMES={R:'restarted',RB:'rebooted',P:'setting changed',T:'tuning started',X:'share rejected by the pool',S:'stale share (caught by the miner)'};
// Tap/hover inspector for the hashrate chart and the rejects strip: shows the exact time under the
// finger, the hashrate there, and any mark within a finger width (phones have no hover tooltips).
function hrInspect(ev,which){const box=$(which==='hr'?'hrchart':'rejstrip'),out=$(which==='hr'?'hrread':'rjread'),lay=window._hrLayout||{W:1200,L:34,R:34};
 const svg=box&&box.querySelector('svg');if(!svg||!lay)return;const r=svg.getBoundingClientRect();if(!r.width)return;
 const cx=(ev.touches&&ev.touches[0]?ev.touches[0].clientX:ev.clientX),vx=(cx-r.left)/r.width*lay.W;
 const now=Date.now()/1000,span=86400,frac=(vx-lay.L)/(lay.W-lay.L-lay.R);if(frac<0||frac>1)return;
 const t=now-span*(1-frac),tol=span*(16/r.width)*(lay.W/(lay.W-lay.L-lay.R));
 const marks=(window._hrmarks||[]).filter(m=>(which==='hr'?m.kind!=='S':(m.kind==='X'||m.kind==='S'))&&Math.abs(m.t-t)<=tol).sort((a,b)=>Math.abs(a.t-t)-Math.abs(b.t-t));
 let txt='';
 if(which==='hr'){const S=(window._hrLast&&window._hrLast.samples)||[];let best=null;S.forEach(s=>{if(!best||Math.abs(s.t-t)<Math.abs(best.t-t))best=s;});
  txt='<b>'+hesc(fmtWhen(best&&Math.abs(best.t-t)<180?best.t:t))+'</b>'+(best&&Math.abs(best.t-t)<180?(' · '+fmtHRc(best.ghs)):' · no reading');}
 else txt='<b>'+hesc(fmtWhen(t))+'</b>'+(marks.length?'':' · nothing here');
 marks.slice(0,3).forEach(m=>{txt+='<br><b'+(m.kind==='X'?' style="color:var(--bad)"':'')+'>'+hesc(m.kind)+'</b> '+hesc(fmtWhenS(m.t))+' · '+hesc(HRNAMES[m.kind]||m.kind)+(m.note?(': '+hesc(m.note)):'');});
 out.innerHTML=txt;
 let cur=svg.querySelector('.hrcur');if(!cur){cur=document.createElementNS('http://www.w3.org/2000/svg','line');cur.setAttribute('class','hrcur');svg.appendChild(cur);}
 const vb=svg.viewBox.baseVal,mx=marks.length?(lay.L+(lay.W-lay.L-lay.R)*(1-(now-marks[0].t)/span)):vx;
 cur.setAttribute('x1',mx.toFixed(1));cur.setAttribute('x2',mx.toFixed(1));cur.setAttribute('y1','0');cur.setAttribute('y2',String(vb.height));}
function hrInspectWire(){['hr','rj'].forEach(w=>{const el=$(w==='hr'?'hrchart':'rejstrip');if(!el||el._insp)return;el._insp=1;
 const f=e=>hrInspect(e,w);el.addEventListener('mousemove',f);el.addEventListener('click',f);el.addEventListener('touchstart',f,{passive:true});el.addEventListener('touchmove',f,{passive:true});});}
if(document.readyState==='loading')document.addEventListener('DOMContentLoaded',hrInspectWire);else hrInspectWire();
function renderHR(d){hrInspectWire();const el=$('hrchart'),now=Math.floor(Date.now()/1000),span=86400,t0=now-span;
 const S=(d.samples||[]).filter(s=>s.t>=t0);window._hrmarks=d.marks||[];
 window._hrNow=S.length?S[S.length-1].ghs:null;window._hrNowT=S.length?S[S.length-1].t:0;
 if(S.length<2){$('hrnote').textContent='';el.innerHTML='<span class="muted">collecting… (a point every minute)</span>';$('hrcap').textContent='';renderRejected(window._lastDash);return;}
 const avg=S.reduce((a,s)=>a+s.ghs,0)/S.length;
 $('hrnote').innerHTML='avg 24 h <b style="color:var(--text)">'+fmtHRc(avg)+'</b>';
 window._hrLast=d;const W=Math.max(280,Math.min(1400,Math.round(el.clientWidth||1000))),phone=W<520,fs=phone?12:11,H=phone?210:W<900?190:170,L=phone?30:36,R=phone?30:36,T=22,B=26;
 window._hrLayout={W,L,R,at:Date.now()};
 let hi=Math.max(...S.map(s=>s.ghs));const sc=hrScale(hi);
 const raw=hi/sc.div,mag=Math.pow(10,Math.floor(Math.log10(raw||1))),stepv=[0.1,0.2,0.25,0.5,1,2,2.5,5,10].map(x=>x*mag).find(x=>raw/x<=4)||mag*10,top=Math.max(stepv,Math.ceil(raw*1.08/stepv)*stepv);
 const X=t=>L+(W-L-R)*(t-t0)/span,Y=v=>T+(1-(v/sc.div)/top)*(H-T-B);
 let g='';
 for(let v=stepv;v<top+1e-9;v+=stepv){const y=Y(v*sc.div).toFixed(1),lab=+v.toFixed(3);
  g+='<line class="hrgrid" x1="'+L+'" x2="'+(W-R)+'" y1="'+y+'" y2="'+y+'" opacity=".6"/><text class="hrlbl" x="'+(L-6)+'" y="'+(+y+3)+'" text-anchor="end">'+lab+'</text><text class="hrlbl" x="'+(W-R+6)+'" y="'+(+y+3)+'">'+lab+'</text>';}
 g+='<line class="hrgrid" x1="'+L+'" x2="'+(W-R)+'" y1="'+Y(0)+'" y2="'+Y(0)+'"/>';
 const every=W>=900?3:W>=640?4:6,ticks=hrTicks(now,span,every);
 ticks.forEach(tk=>{const x=X(tk.t).toFixed(1);g+='<line class="hrtick maj" x1="'+x+'" x2="'+x+'" y1="'+Y(0)+'" y2="'+(Y(0)+6)+'"/><line class="hrgrid" x1="'+x+'" x2="'+x+'" y1="'+T+'" y2="'+Y(0)+'" opacity=".45"/>'+(+x>W-R-fs*3.6?'':'<text class="hrlbl" x="'+x+'" y="'+(H-5)+'" text-anchor="middle">'+tk.label+'</text>');});
 g+='<text class="hrlbl" x="'+(W-R)+'" y="'+(H-5)+'" text-anchor="end">now</text>';
 // a point a minute: more than 5 min without one (miner off, rebooting, idle, not answering) is a gap, not a line across
 let line='',area='',seg=[];const flush=()=>{if(!seg.length)return;const p=seg.map((s,i)=>(i?'L':'M')+X(s.t).toFixed(1)+','+Y(s.ghs).toFixed(1)).join(' ')+' ';
  line+=p;area+=p+'L'+X(seg[seg.length-1].t).toFixed(1)+','+Y(0)+' L'+X(seg[0].t).toFixed(1)+','+Y(0)+' Z ';seg=[];};
 S.forEach((s,i)=>{if(i&&s.t-S[i-1].t>300)flush();seg.push(s);});flush();
 g+='<path class="hrarea" d="'+area+'"/>';
 g+='<path class="hrline" d="'+line+'"/>';
 const last=S[S.length-1];g+='<circle cx="'+X(last.t).toFixed(1)+'" cy="'+Y(last.ghs).toFixed(1)+'" r="2.5" fill="var(--text)"/>';
 const names={R:'restarted',RB:'rebooted',P:'setting changed',T:'tuning started',X:'a share rejected by the pool'};
 let lastLbl=-1e9;const minGap=fs*1.6;
 (d.marks||[]).filter(m=>m.t>=t0&&m.kind!=='S').sort((a,b)=>a.t-b.t).forEach(m=>{const x=X(m.t).toFixed(1),rej=m.kind==='X',lw=fs*0.65*m.kind.length,show=(+x-lastLbl)>=minGap+lw/2;if(show)lastLbl=+x+lw/2;
  g+='<g><title>'+hesc(fmtWhen(m.t)+' · '+(names[m.kind]||m.kind)+(m.note?(': '+m.note):''))+'</title><line class="hrmark'+(rej?' rej':'')+'" x1="'+x+'" y1="'+(T-4)+'" x2="'+x+'" y2="'+Y(0)+'"/>'
   +(show?('<text class="hrmlbl'+(rej?' rej':'')+'" x="'+x+'" y="'+(T-7)+'" text-anchor="middle">'+m.kind+'</text>'):'')+'<rect x="'+(+x-7)+'" y="0" width="14" height="'+H+'" fill="transparent"/></g>';});
 el.innerHTML='<svg viewBox="0 0 '+W+' '+H+'" style="--hrfs:'+fs+'px" role="img" aria-label="hashrate, last 24 hours">'+g+'</svg>';
 $('hrcap').innerHTML='A point every minute from the miner’s hashrate (now '+fmtHRc(last.ghs)+(window._lastDash&&window._lastDash.ok?(', '+fmtHRc(window._lastDash.ghs_avg||window._lastDash.ghs)+' on average since its last restart'):'')+'). Marks: <b>R</b> restarted · <b>RB</b> rebooted · <b>P</b> setting changed'+(featOff2(F().Tuner)?'':' · <b>T</b> tuning started')+' · <b style="color:var(--bad)">X</b> a share rejected by the pool. Tap or hover the chart for the exact time and what happened.';
 renderRejected(window._lastDash);}
// full hours inside the window, every "every" hours, left out next to the edges
window.addEventListener('resize',()=>{clearTimeout(window._hrRz);window._hrRz=setTimeout(()=>{if(window._hrLast)renderHR(window._hrLast);},150);});
function hrTicks(now,span,every){const out=[];for(let t=Math.ceil((now-span)/3600)*3600;t<now;t+=3600){const d=new Date(t*1000),hh=d.getHours();
  if(hh%every||now-t<span*0.07||t-(now-span)<span*0.04)continue;
  out.push({t,label:hh===0?(['Sun','Mon','Tue','Wed','Thu','Fri','Sat'][d.getDay()]+' 00:00'):(String(hh).padStart(2,'0')+':00')});}return out;}
// Rejected shares: bfgminer's Stale counts shares it caught as stale itself; Rejected is anything the
// pool refused after sending (often still a pool-side stale / job not found). The pool's reason rides on the X mark's note.
function renderRejected(d){const body=$('rejbody'),strip=$('rejstrip');if(!body||!d)return;window._lastDash=d;
 const now=Date.now()/1000,day=now-86400,M=(window._hrmarks||[]).filter(m=>m.t>=day);
 const nOf=k=>M.filter(m=>m.kind===k).reduce((a,m)=>{const r=/\(\+(\d+),/.exec(m.note||'');return a+(r?+r[1]:1);},0),nX=nOf('X'),nS=nOf('S');
 const bits=[];if(nX)bits.push('<b class="alert">'+nX+' rejected by the pool</b>');bits.push(nX?(nS+' stale'):(nS?'all stale':'none'));
 const all=(+d.accepted||0)+(+d.rejected||0)+(+d.stale||0),pct=all>0&&d.accepted>0?((+d.rejected+(+d.stale||0))/all*100):0; // of all the shares
 body.innerHTML=(d.ok===false?'<span class="muted">Since restart: no reading (the miner isn’t reporting right now)</span>':'<span class="muted">Since restart:</span> '+(+d.accepted).toLocaleString()+' accepted · '+(+d.rejected).toLocaleString()+' rejected'+(d.stale?(' · '+(+d.stale).toLocaleString()+' stale'):'')+(pct?(' ('+(pct<0.005?'<0.01':pct<1?pct.toFixed(2):pct.toFixed(1))+' %)'):''))
  +'<br><span class="muted">Last 24 h:</span> '+bits.join(' · ')
  +(()=>{const X=M.filter(m=>m.kind==='X').slice(-8).reverse(),withR=X.filter(m=>(m.note||'').includes('pool said: ')),noR=X.filter(m=>!(m.note||'').includes('pool said: '));
   return withR.map(m=>'<br><span class="muted">'+hesc(fmtWhenS(m.t))+':</span> '+hesc(m.note.slice(m.note.indexOf('pool said: ')+11))).join('')
    +(noR.length?('<br><span class="muted">'+noR.map(m=>hesc(fmtWhenS(m.t))).join(', ')+': no reason in the log (it is only there while verbose is on)</span>'):'');})();
 if(!nX&&!nS){strip.innerHTML='';return;}
 const lay=window._hrLayout||{W:1200,L:34,R:34},W=lay.W,L=lay.L,R=lay.R,span=86400,xOf=t=>L+(W-L-R)*(1-(now-t)/span),narrow=W<640;
 // timestamp under every mark, on a leader line; labels that would overlap drop to the next row
 const ev=M.filter(m=>m.kind==='X'||m.kind==='S').sort((a,b)=>a.t-b.t),lfs=narrow?12:11,lw=lfs*0.62*8+8;
 // marks closer together than a label share one: "HH:MM:SS +n" (tap the strip for each one)
 const cl=[];ev.forEach(e=>{const x=xOf(e.t),c=cl[cl.length-1];if(c&&x-c.x0<lw*0.5){c.n++;c.rej=c.rej||e.kind==='X';c.x1=x;}else cl.push({t:e.t,x0:x,x1:x,n:1,rej:e.kind==='X'});});
 const rowEnd=[];cl.forEach(c=>{const w=lw+(c.n>1?lfs*2.2:0),x=Math.min(W-2-w/2,Math.max(2+w/2,c.x0));let r=0;while(r<rowEnd.length&&x-w/2<rowEnd[r])r++;rowEnd[r]=x+w/2;c.row=r;c.lx=x;});
 const rows=cl.length?Math.max(...cl.map(c=>c.row))+1:0,y=17,base=34,rowH=lfs+5,H=base+rows*rowH+4,p2=n=>String(n).padStart(2,'0');
 let svg='<svg viewBox="0 0 '+W+' '+H+'" role="img" aria-label="rejected shares, last 24 hours">';
 hrTicks(now,span,W>=900?3:W>=640?4:6).forEach(tk=>{const x=xOf(tk.t).toFixed(1);svg+='<line class="grid" x1="'+x+'" x2="'+x+'" y1="2" y2="'+(base-4)+'"/>';});
 svg+='<line class="grid" x1="'+L+'" x2="'+(W-R)+'" y1="'+(base-4)+'" y2="'+(base-4)+'"/>';
 cl.forEach(c=>{const ly=base+c.row*rowH+lfs,dt=new Date(c.t*1000),k=c.rej?' rej':'';
  svg+='<line class="rjlead'+k+'" x1="'+c.x0.toFixed(1)+'" x2="'+c.x0.toFixed(1)+'" y1="'+(y+7)+'" y2="'+(ly-lfs+1)+'"/>'
   +'<text class="rjts'+k+'" x="'+c.lx.toFixed(1)+'" y="'+ly+'" text-anchor="middle" style="font-size:'+lfs+'px">'+p2(dt.getHours())+':'+p2(dt.getMinutes())+':'+p2(dt.getSeconds())+(c.n>1?(' +'+(c.n-1)):'')+'</text>';});
 ev.forEach(e=>{const x=xOf(e.t),st=e.kind==='S';
  svg+='<g><title>'+hesc(fmtWhen(e.t)+' · '+(st?'stale (caught by the miner)':'rejected by the pool')+(e.note?(' ('+e.note+')'):''))+'</title>';
  if(st)svg+='<line class="stale" x1="'+x.toFixed(1)+'" x2="'+x.toFixed(1)+'" y1="'+(y-7)+'" y2="'+(y+7)+'"/>';
  else svg+='<path class="other" d="M'+x.toFixed(1)+' '+(y-7)+'L'+(x+7).toFixed(1)+' '+(y+6)+'L'+(x-7).toFixed(1)+' '+(y+6)+'Z"/>';
  svg+='<rect x="'+(x-8).toFixed(1)+'" y="0" width="16" height="'+H+'" fill="transparent"/></g>';});
 strip.innerHTML=svg+'</svg>';}
async function loadBestShares(){try{const r=await fetch('/api/shares');if(!r.ok)return;const sh=await r.json();if(!sh.ok)return;
 $('shstats').innerHTML='<div><span class="muted">Since restart</span><b>'+hesc(sh.now_text)+'</b><small class="muted">'+(sh.now_at?('found '+fmtWhen(sh.now_at)+(sh.now_on?(' · '+hesc(sh.now_on)):'')):'none yet')+(sh.since?(' · up '+fmtAgo(sh.since)):'')+'</small></div>'
  +'<div><span class="muted">Record</span><b>'+hesc(sh.record_text)+'</b><small class="muted">'+(sh.record_at?('found '+fmtWhen(sh.record_at)+(sh.record_on?(' · '+hesc(sh.record_on)):'')):'')+(sh.reset_at?(' · since reset '+fmtWhen(sh.reset_at)):'')+'</small></div>';
 const all=sh.history||[],SHOW=10;let rows=all.slice(0,SHOW);const cur=all.slice(SHOW).find(h=>h.current);if(cur)rows=rows.concat([cur]);const more=all.length-rows.length;
 $('shbody').innerHTML=(rows.map(h=>'<tr'+(h.current||h.record?' style="font-weight:600"':'')+'><td class="num">'+h.rank+'</td><td class="num">'+hesc(h.text)+(h.record?' ★':'')+'</td><td>'+hesc(h.on||'')+(h.current?' <span class="muted">(best since restart)</span>':'')+'</td>'
   +'<td style="text-align:right"'+(h.after?(' title="The panel wasn’t watching for a while, so this share was found some time between '+fmtWhen(h.after)+' and '+fmtWhen(h.at)+'"'):'')+'>'+(h.after?(fmtWhen(h.after)+' – '):'')+fmtWhen(h.at)+'</td></tr>').join('')
  +(more>0?('<tr><td colspan="4" class="muted" style="font-size:11px">'+more+' smaller one'+(more>1?'s':'')+' kept but not shown (the record keeps the 20 biggest)</td></tr>'):''))||'<tr><td colspan="4" class="muted">Nothing yet</td></tr>';}catch(e){}}
async function sharesReset(){if(!confirm('Clear the best-share record and history?\nThe best since its last restart stays (the miner keeps that one itself).'))return;
 const {j}=await jpost('/api/shares',{action:'reset'});if(j.ok){toast('Best-share record cleared');loadBestShares();}else toast(j.err||'failed',true);}
// ---- hashrate unit formatting (driven by the Settings "Hashrate units" choice) ----
function hrScale(ref){const u=window._hrUnit||'auto';
 if(u==='ghs')return {div:1,unit:'GH/s'};
 if(u==='ths'||u==='both')return {div:1000,unit:'TH/s'};
 const a=Math.abs(ref||0); // auto: pick a sensible SI step
 if(a>=1e6)return {div:1e6,unit:'PH/s'};
 if(a>=999.995)return {div:1000,unit:'TH/s'}; // (999.996 GH/s would print as "1000.00 GH/s")
 return {div:1,unit:'GH/s'};}
// hrParts: {num, unit, sub?} for the big Overview tile (sub carries the GH/s in "both" mode)
function hrParts(ghs){ghs=ghs||0;const u=window._hrUnit||'auto';
 if(u==='both')return {num:(ghs/1000).toFixed(3),unit:'TH/s',sub:Math.round(ghs).toLocaleString()+' GH/s'};
 const sc=hrScale(ghs),dec=sc.unit==='GH/s'?2:sc.unit==='TH/s'?3:4;
 return {num:(ghs/sc.div).toFixed(dec),unit:sc.unit};}
// fmtHRc: compact "value unit" for headers/axes (no "both" parenthetical)
function fmtHRc(ghs){ghs=ghs||0;const sc=hrScale(ghs),dec=sc.unit==='GH/s'?2:sc.unit==='TH/s'?3:4;return (ghs/sc.div).toFixed(dec)+' '+sc.unit;}
function lsGet(k){try{return localStorage.getItem(k);}catch(e){return null;}}
function lsSet(k,v){try{localStorage.setItem(k,v);}catch(e){}}
function hesc(s){return String(s==null?'':s).replace(/[<>&"']/g,c=>({'<':'&lt;','>':'&gt;','&':'&amp;','"':'&quot;',"'":'&#39;'}[c]));}
// ===== presets (profiles.html parity) =====
const PSLOTS=[['high','High'],['middle','Middle'],['low','Low'],['lowest','Lowest power']];
let PR=null,pfKey=null,pfGap=300;
const pSlotOf=n=>{const l=String(n||'').toLowerCase();return l==='lowest power'?'lowest':(['high','middle','low','lowest'].includes(l)?l:null);};
function pIsOn(p){const lk=PR&&PR.in_charge;return lk==='preset '+p.name||(B2T()&&lk==='stock settings'&&b2Name(p.name)==='Stock High'&&b2Stock(p.name));} // Box II: Stock is Stock High's plan
function pTHS(p){const g=(PR&&PR.ghs)||0,c=PR&&PR.current;if(!(g>0&&c&&c.mhz>0))return 0;
 const pll=m=>Math.floor(m/12.5)*12.5;return (g/1000)*pll(p.mhz)/pll(c.mhz);}
function pFanProfiles(){return (FAN&&FAN.profiles)||{};}
function pSpark(p){let pts=[],steps=false;
 if(p.fan_profile==='custom'||(p.fan_points&&p.fan_points.length>=2))pts=(p.fan_points||[]);
 else{const pr=pFanProfiles()[p.fan_profile];if(pr){pts=pr.points||[];steps=pr.mode==='steps';}}
 pts=pts.map(q=>({temp:+q.temp,fan:+q.fan})).sort((a,b)=>a.temp-b.temp);if(!pts.length)return '';
 const x=t=>(t-30)/70*200,y=f=>36-f/100*34;let d;
 if(steps){d='';pts.forEach((q,i)=>{const nx=i+1<pts.length?pts[i+1].temp:100;d+=(i?' L':'M')+x(q.temp).toFixed(1)+','+y(q.fan).toFixed(1)+' L'+x(nx).toFixed(1)+','+y(q.fan).toFixed(1);});}
 else d='M0,'+y(pts[0].fan)+' '+pts.map(q=>'L'+x(q.temp).toFixed(1)+','+y(q.fan).toFixed(1)).join(' ')+' L200,'+y(pts[pts.length-1].fan);
 return '<svg viewBox="0 0 200 38" preserveAspectRatio="none" aria-hidden="true"><path d="'+d+'" fill="none" stroke="var(--muted)" stroke-width="1.6" vector-effect="non-scaling-stroke"/></svg>';}
function pFanLine(p){if(p.fan_profile==='none')return 'Fans: left as they are';
 if(p.fan_profile==='custom'||(p.fan_points&&p.fan_points.length>=2&&!p.fan_profile))return 'Fans: its own tuned curve'+(p.target_c?(' (holds about '+p.target_c+' °C)'):'');
 const pr=pFanProfiles()[p.fan_profile];return 'Fans: '+(pr?pr.label:(p.fan_profile||'the curve that was running when it was saved'));}
async function loadPresets(){try{const r=await fetch('/api/presets');if(r.status===401){boot();return;}const j=await r.json();if(!j.ok){retryLater(loadPresets);return;}
 if(!j.stock&&PR&&PR.stock)j.stock=PR.stock; // the backend pauses after a change: keep the card
 PR=j;
 if(!FAN){try{const f=await fetch('/api/fan');const fj=await f.json();if(fj.ok)FAN=fj;}catch(e){}}
 renderPresets();}catch(e){if(!PR)$('pcards').innerHTML=busyHTML(OFFLINE);retryLater(loadPresets);}}
function renderPresets(){const ps=PR.presets||[],lk=PR.in_charge||'',tuning=lk==='the auto-tuner';
 window._presets=ps;
 // miner bar: the live readings this page cares about
 const fs=(FAN&&FAN.status)||{},fc=(FAN&&FAN.config)||{},rpm=(fs.fans_rpm||[]).filter(x=>x>0);
 $('p_temp').textContent=fs.max_temp>-100?((+fs.max_temp).toFixed(0)+' °C'):'—';
 $('p_fans').textContent=rpm.length?(rpm.join(' / ')+fanUnit(rpm)):'—';
 $('p_auto').textContent=fc.enabled?(((pFanProfiles()[fc.profile]||{}).label)||'on'):'off';
 $('p_preset').textContent=lk?chargeTxt(lk).replace(/^preset /,''):'none';
 $('p_tuning').classList.toggle('hide',!tuning);
 $('p_tuning').innerHTML=tuning?'A tuning run is going on this miner and it is holding the fans. Presets can be applied when it ends; new ones it builds appear here then. <a href="#" onclick="showTab(\'tuner\');return false">Open the tuner</a>':'';
 const bySlot={};ps.forEach(p=>{const sl=pSlotOf(p.name);if(sl&&!bySlot[sl])bySlot[sl]=p;});
 const others=ps.filter(p=>{const sl=pSlotOf(p.name);return !sl||bySlot[sl]!==p;});
 const card=p=>{const on=pIsOn(p),w=estW(p.mhz,p.pv),ths=pTHS(p),run=Math.floor(p.mhz/12.5)*12.5;
  // Box II's stock modes are Goldshell's plans (Stock High is the factory setting: the Stock card folded in); the
  // public release has no tuner, so no tested / not-tested tag on a preset someone set
  const tested=b2Stock(p.name)?(b2Name(p.name)==='Stock High'?'The OEM’s factory setting for this miner, read from its firmware. Use it to start mining again after Idle, or to go back to the factory clock and voltage.':'The OEM’s low-power stock plan, read from the miner’s firmware.')
   :p.tested&&PUB()&&featOff2(F().Tuner)?'' // a tuner-built preset left on the config partition: no tuner here, no test tags
   :p.tested?('Clean'+(p.target_c?(' at '+p.target_c+' °C'):'')+(ths?(' · ≈ '+ths.toFixed(2)+' TH/s'):'')+'<br>tested '+hesc(p.tested)):PUB()?'Set by hand':'<span style="color:var(--warn)">Set by hand, not tested</span>';
  return '<div class="pcard'+(on?' active':'')+'"><h3>'+hesc(b2Name(p.name))+(on?' <span class="pill ok">on the miner</span>':'')+'</h3>'
   +'<div class="clock">'+p.mhz+' MHz</div>'
   +'<div class="sub">'+vLine(p.mv,p.pv)+(run!==p.mhz?(' · runs '+run+' MHz'):'')+'</div>'
   +(w?'<div class="meta" title="Estimate: chips follow clock × PV² (PV is the real board voltage), calibrated to 899 W at 625 MHz · PV 9400.">≈ '+Math.round(w)+' W'+(ths>0.05?(' · '+Math.round(w/ths)+' J/TH'):'')+'</div>':'')
   +'<div class="meta">'+hesc(pFanLine(p))+'</div>'+pSpark(p)
   +'<div class="meta">'+tested+'</div>'
   +'<div class="acts"><button class="primary" onclick="presetApply(\''+esc1(p.name)+'\')"'+(tuning?' disabled':'')+'>'+(on?'Apply again':'Apply')+'</button>'+(b2Stock(p.name)?'':'<button onclick="presetEdit(\''+esc1(p.name)+'\')">Edit</button>')+'</div></div>';};
 // the tuner's four slots: an empty one points at the tuner, which the public release hasn't got on Box II
 let h=(PUB()&&featOff2(F().Tuner)?PSLOTS.filter(([k])=>bySlot[k]):PSLOTS).map(([k,lbl])=>bySlot[k]?card(bySlot[k]):('<div class="pcard empty"><h3>'+lbl+'</h3><div class="meta">Not built yet. Run a search on the Tuner tab with <i>Build presets</i> ticked, or set it by hand.</div><div class="acts"><button onclick="showTab(\'tuner\')">Tuner</button><button onclick="presetNewNamed(\''+esc1(lbl)+'\')">Set by hand</button></div></div>')).join('');
 h+=others.map(card).join('');
 if(PR.stock&&!B2T()){const st=PR.stock,on=lk==='stock settings',w=estW(st.mhz,st.pv),srun=Math.floor(st.mhz/12.5)*12.5;
  h+='<div class="pcard'+(on?' active':'')+'"><h3>Stock'+(on?' <span class="pill ok">on the miner</span>':'')+'</h3><div class="clock">'+st.mhz+' MHz</div>'
   +'<div class="sub">'+vLine(st.mv,st.pv)+(srun!==st.mhz?(' · runs '+srun+' MHz'):'')+'</div>'+(w?'<div class="meta">≈ '+Math.round(w)+' W</div>':'')
   +'<div class="meta">'+OemPoss()+' factory setting for this miner, read from its firmware. Use it to start mining again after Idle, or to go back to the factory clock and voltage. The fans stay as they are (auto fan, if it’s on).</div>'
   +'<div class="acts"><button class="primary" onclick="presetStock()"'+(tuning?' disabled':'')+'>'+(on?'Apply again':'Apply')+'</button></div></div>';}
 if(PR.has_idle){const on=lk==='idle mode';
  h+='<div class="pcard'+(on?' active':'')+'"><h3>☾ Idle'+(on?' <span class="pill ok">on the miner</span>':'')+'</h3><div class="clock">not hashing</div>'
   +'<div class="meta">'+(PR.box2?'The panel disables the hash board through the miner (the way the OEM’s own overheat stop does) and checks that hashing really stops; the OEM’s 86 °C cut-off stays the backstop. Either mode starts mining again. A schedule can use it too, for example for expensive hours.':'The firmware’s own Idle mode (the same as Idle on the miner’s own Miner settings page): the hash boards stop and draw little power. <b>Stock</b>, or any other preset, starts mining again. A schedule can use it too, for example for expensive hours.')+'</div>'
   +'<div class="acts"><button class="primary" onclick="presetIdle()"'+(tuning?' disabled':'')+'>'+(on?'Apply again':'Apply')+'</button></div></div>';}
 $('pcards').innerHTML=calNote()+h;
 $('presetnote').textContent='';}
function esc1(s){return hesc(String(s).replace(/\\/g,'\\\\').replace(/'/g,"\\'"));}
function pfFanOptions(sel,p){const P=pFanProfiles();let h='<option value="none"'+(sel==='none'?' selected':'')+'>Leave the fans as they are</option>';
 if(p&&p.fan_points&&p.fan_points.length>=2)h+='<option value="custom"'+(sel==='custom'?' selected':'')+'>Its own tuned curve'+(p.target_c?(' ('+p.target_c+' °C)'):'')+'</option>';
 Object.keys(P).filter(k=>!P[k].tuned).forEach(k=>h+='<option value="'+hesc(k)+'"'+(k===sel?' selected':'')+'>'+hesc(P[k].label||k)+'</option>');return h;}
function presetNew(){pfOpen(null);}
function presetNewNamed(n){pfOpen(null);$('pfLabel').value=n;}

function presetEdit(name){pfOpen((window._presets||[]).find(x=>x.name===name)||null);}
function pfOpen(p){pfKey=p?p.name:'';const ps=window._presets||[],any=ps.find(x=>x.mhz>0&&x.mv>0);
 pfGap=p?(p.pv-p.mv):(any?any.pv-any.mv:300);
 const st=(PR&&PR.stock)||{};
 const base=p||(B2T()?{mhz:st.mhz||'',mv:st.mv||42,pv:0,fan_profile:(FAN&&FAN.config&&FAN.config.profile)||'curve-balanced'} // Box II: its stock setting, no PV
  :{mhz:625,mv:9100,pv:9100+pfGap,fan_profile:(FAN&&FAN.config&&FAN.config.profile)||'curve-balanced'});
 pfBox2();
 $('pfTitle').textContent=p?('Edit '+b2Name(p.name)):'New preset';
 $('pfLabel').value=p?p.name:'';$('pfLabel').readOnly=!!p;$('pfLabel').title=p?'A preset keeps its name (the schedule finds it by name). Make a new one to use another name.':'';
 $('pfMhz').value=base.mhz;$('pfMv').value=base.mv;$('pfPv').value=base.pv;
 const sel=p?(p.fan_profile||((p.fan_points&&p.fan_points.length>=2)?'custom':'none')):base.fan_profile;
 $('pfFan').innerHTML=pfFanOptions(sel,p);
 $('pfGap').textContent=(pfGap>=0?'+':'')+pfGap;$('pfPvAuto').checked=true;$('pfPv').disabled=true;if(B2T())pfSync();
 $('pfWarn').classList.toggle('hide',!!(p&&p.tested));
 $('btnPfDelete').classList.toggle('hide',!p);$('pfErr').textContent='';
 $('presetForm').classList.remove('hide');$('presetForm').scrollIntoView({block:'nearest'});}
function pfSync(){if(B2T())return; // Box II: no PV; the voltage is picked in volts (pfBox2)
 if($('pfPvAuto').checked)$('pfPv').value=(+$('pfMv').value||0)+pfGap;$('pfPv').disabled=$('pfPvAuto').checked;}
// pfBox2: on Box II the form's "mV" box becomes "Voltage", a choice of the miner's fixed voltage steps in volts
// (b2VOpts: the value sent is Goldshell's level); the PV box and the "PV follows mV" tick are hidden. The SC
// Lite form is untouched.
function pfBox2(){if(!B2T())return;const mv=$('pfMv');if(!mv||mv._b2)return;
 const lb=mv.parentNode;if(lb&&lb.firstChild&&lb.firstChild.nodeType===3)lb.firstChild.textContent='Voltage';
 mv.outerHTML='<select id="pfMv" title="The miner’s fixed voltage steps, measured at the power lugs under load on one unit (≈: estimated)">'+b2VOpts(0,42,61)+'</select>';
 const ns=$('pfMv');if(ns)ns._b2=1;
 [$('pfPv'),$('pfPvAuto')].forEach(e=>{if(e&&e.parentNode)e.parentNode.classList.add('hide');});}
function presetFormCancel(){$('presetForm').classList.add('hide');}
async function presetFormSave(){const name=$('pfLabel').value.trim(),mhz=+$('pfMhz').value,mv=+$('pfMv').value,pv=B2T()?0:+$('pfPv').value,fan=$('pfFan').value;
 const errs=[];if(!name)errs.push('Give the preset a name.');
 if(!pfKey&&(window._presets||[]).some(x=>x.name.toLowerCase()===name.toLowerCase()))errs.push('There is already a preset called '+name+'.');
 if(name.toLowerCase()==='idle'||name.toLowerCase()==='stock')errs.push('Idle and Stock are built in: pick another name.');
 if(errs.length){$('pfErr').textContent=errs.join(' ');return;}
 const old=(window._presets||[]).find(x=>x.name===pfKey);
 const body={action:'save',name,mhz,mv,pv,fan_profile:fan,fan_points:fan==='custom'&&old?old.fan_points:[]};
 const {j}=await jpost('/api/presets',body);
 if(j.ok){presetFormCancel();toast((pfKey?'Saved ':'Made ')+name);loadPresets();}else $('pfErr').textContent=j.err||'save failed';}
async function presetDeleteCur(){if(!pfKey||!confirm('Delete the preset "'+pfKey+'"?'))return;
 const {j}=await jpost('/api/presets',{action:'delete',name:pfKey});if(j.ok){presetFormCancel();toast('Preset deleted');loadPresets();}else toast(j.err||'failed',true);}
async function presetCool(name,j){
 if(!j.cooling)return false;const nm=b2Name(name);
 if(confirm('The thermal guard put this miner on Idle to cool down. It can start again in '+j.left+'.\n\nOn Idle the boards stop making heat and cool by themselves. Starting early would bring the heat straight back, and the panel can’t raise the fans without waking the miner.\n\nStart '+nm+' by itself when the cool-down ends?')){
  const r=await jpost('/api/presets',{action:'apply',name,when_cool:true});
  if(r.j.ok)toast(r.j.note||('Waiting to start '+nm));else toast(r.j.err||'failed',true);}
 return true;}
async function presetApply(name){const p=(window._presets||[]).find(x=>x.name===name);if(!p)return;
 const nm=b2Name(p.name);
 if(!confirm('Put '+nm+' on this miner?\n\n'+p.mhz+' MHz · '+vLine(p.mv,p.pv)+'\n'+pFanLine(p)+(p.fan_profile!=='none'?' (auto fan on)':'')+(p.tested||b2Stock(p.name)?'':PUB()?'\n\nThis preset was set by hand.':'\n\nThis preset was set by hand and hasn’t been tested.')))return;
 if(PR&&PR.in_charge==='idle mode')toast('Waking the miner from Idle, then applying '+nm+' (about 20 s)…');
 const {j}=await jpost('/api/presets',{action:'apply',name});
 if(j.ok){toast(nm+' is on the miner');setTimeout(()=>{loadPresets();loadControl();loadFan();},1200);}else if(!(await presetCool(p.name,j)))toast(j.err||'failed',true);}
async function presetStock(){const st=PR.stock||{};if(!confirm('Put the stock settings on this miner?\n\n'+st.mhz+' MHz · '+vLine(st.mv,st.pv)+' ('+oemPoss()+' factory setting)'+(PR.in_charge==='idle mode'?'\n\nIt starts mining again.':'')))return;
 const {j}=await jpost('/api/presets',{action:'apply',name:'Stock'});
 if(j.ok){toast('Stock settings are on the miner');setTimeout(()=>{loadPresets();loadControl();loadFan();},1200);}else if(!(await presetCool('Stock',j)))toast(j.err||'failed',true);}
async function presetIdle(){if(!confirm('Put this miner in Idle?\n\nIt stops hashing until you pick another preset (or its schedule does).'))return;
 const {j}=await jpost('/api/presets',{action:'idle'});if(j.ok){toast('The miner is idle');setTimeout(()=>{loadPresets();loadControl();},1200);}else toast(j.err||'failed',true);}
async function presetBuild(){if(!confirm('Build High / Middle / Low / Lowest power from the last clock-curve tuner run?\n\nThey replace presets with the same names.'))return;
 const {j}=await jpost('/api/presets',{action:'build'});if(j.ok){toast('Built '+(j.built||0)+' presets');loadPresets();}else toast(j.err||'failed',true);}
// ===== weekly schedule (schedule.html parity) =====
function toast(msg,err){const t=$('toast');if(!t)return;t.textContent=msg;t.className='toast show'+(err?' err':'');clearTimeout(toast._t);toast._t=setTimeout(()=>t.className='toast',5000);}
const SC_DAYS=['Mon','Tue','Wed','Thu','Fri','Sat','Sun'];
const SC_SHADE={high:5,middle:3,low:1,lowest:0,'lowest power':0};
const SC_EVERY=[['off','Off'],['hour','Every hour'],['6h','Every 6 hours'],['12h','Every 12 hours'],['day','Every day'],['week','Every week'],['month','Every month'],['3months','Every 3 months']];
const SC_RS='__restart',SC_ERASE='__erase',SC_BACK='__back',SC_SLOT=30,SC_N=48;
let SC=null,scRules=[],scSaved='',scRs={every:'off'},scBrush=null,scBase=null;
const scHm=s=>{const a=String(s).split(':').map(Number);return a[0]*60+a[1];};
const scHHMM=m=>String(Math.floor(m/60)).padStart(2,'0')+':'+String(m%60).padStart(2,'0');
const scFmt=ts=>new Date(ts*1000).toLocaleString([],{weekday:'short',hour:'2-digit',minute:'2-digit',hourCycle:'h23',hour12:false});
const scFmtD=ts=>new Date(ts*1000).toLocaleString([],{weekday:'short',month:'short',day:'numeric',hour:'2-digit',minute:'2-digit',hourCycle:'h23',hour12:false});
function scPresets(){return ((SC&&SC.presets)||[]).map(p=>[p.key,p]);}
function scPreset(k){return ((SC&&SC.presets)||[]).find(p=>p.key===k);}
function scShade(k){const l=String(k||'').toLowerCase();if(l in SC_SHADE)return SC_SHADE[l];
 const others=scPresets().map(x=>x[0]).filter(x=>!(String(x).toLowerCase() in SC_SHADE));const i=others.indexOf(k);return i<0?4:[4,2][i%2];}
function scLabel(k){return k===SC_BACK?'nothing (back to what it ran before)':((scPreset(k)||{}).label||b2Name(k));}
function scOcc(rs){const o=[];rs.forEach((r,i)=>{if(!/^\d{2}:\d{2}$/.test(r.start||''))return;(r.days||[]).forEach(d=>o.push([d*1440+scHm(r.start),i]));});return o.sort((a,b)=>a[0]-b[0]);}
function scAt(o,mow){if(!o.length)return null;let cur=o[o.length-1];for(const x of o)if(x[0]<=mow)cur=x;return cur[1];}
function scAppNow(){const m=/^([+-])(\d{2})(\d{2})$/.exec((SC&&SC.utc_offset)||'');
 const off=m?(m[1]==='-'?-1:1)*(Number(m[2])*60+Number(m[3])):-new Date().getTimezoneOffset();
 const a=new Date(Date.now()+off*60000);
 return {y:a.getUTCFullYear(),mo:a.getUTCMonth(),d:a.getUTCDate(),dow:(a.getUTCDay()+6)%7,min:a.getUTCHours()*60+a.getUTCMinutes()+a.getUTCSeconds()/60};}
function scGrid(rs){const o=scOcc(rs),g=[];for(let d=0;d<7;d++){g.push([]);for(let k=0;k<SC_N;k++){const i=scAt(o,d*1440+k*SC_SLOT);g[d].push(i==null?null:rs[i].preset);}}return g;}
function scRulesFromGrid(g){const flat=g.flat().map(x=>x==null?scBase:x),n=flat.length,ch=[];
 if(flat.every(x=>x==null||x===SC_BACK))return [];
 if(flat.every(x=>x===flat[0]))return [{start:'00:00',days:[0],preset:flat[0]}];
 for(let w=0;w<n;w++){const prev=flat[(w-1+n)%n];if(flat[w]!=null&&flat[w]!==prev)ch.push({d:Math.floor(w/SC_N),m:(w%SC_N)*SC_SLOT,p:flat[w]});}
 const groups={};ch.forEach(c=>{const k=c.m+'|'+c.p;(groups[k]=groups[k]||{m:c.m,p:c.p,days:[]}).days.push(c.d);});
 return Object.values(groups).sort((x,y)=>x.m-y.m).map(x=>({start:scHHMM(x.m),days:x.days.sort(),preset:x.p}));}
function scShort1(k){const l0=scLabel(k)||k,l=l0.replace(/[^\p{L}\p{N}\s]+/gu,' ').trim()||l0,w=l.split(/\s+/);return (w.length>1?w.map(x=>x[0]).join(''):l.slice(0,2)).toUpperCase().slice(0,2);}
function scShort(k){const keys=scPresets().map(x=>x[0]),mine=scShort1(k);
 if(keys.filter(x=>x!==k&&scShort1(x)===mine).length===0)return mine;
 const l=(scLabel(k)||k).replace(/\s+/g,''),alt=(l[0]+l[l.length-1]).toUpperCase();
 return keys.indexOf(k)===keys.findIndex(x=>scShort1(x)===mine)?mine:alt;}
function scSub(p){return p.idle?'not hashing':(p.mhz?(p.mhz+' MHz · '+(B2T()?boxVoltsTxt(p.mv):p.mv+' mV')):'');}
function scRenderBrushes(){const ps=scPresets();
 if(scBrush!==SC_RS&&scBrush!==SC_ERASE&&!ps.some(x=>x[0]===scBrush))scBrush=ps.length?ps[0][0]:SC_RS;
 let h='<span class="muted" style="font-size:12.5px">Paint with</span>'+ps.map(([k,p])=>'<button class="sc-brush'+(k===scBrush?' on':'')+'" data-k="'+hesc(k)+'"><i style="background:var(--ramp-'+scShade(k)+')"></i>'+hesc(p.label)+' <small>'+hesc(scSub(p))+'</small></button>').join('');
 h+='<button class="sc-brush rsb'+(scBrush===SC_RS?' on':'')+'" data-k="'+SC_RS+'" title="Click a block to restart at its start, or drag to mark several (at least an hour apart); click a mark again to remove it"><i>↻</i> Restart</button>';
 h+='<button class="sc-brush rsb'+(scBrush===SC_ERASE?' on':'')+'" data-k="'+SC_ERASE+'" title="Click or drag to clear blocks: painted presets go back to the rest-of-the-time preset, and restart marks are removed"><i>⌫</i> Erase</button>';
 h+='<button id="sc_fill" title="Fill every block of the week with what’s picked">Fill the whole week</button>';
 $('sc_brushes').innerHTML=h;
 $('sc_base').innerHTML=ps.length?('<span class="muted" style="font-size:12.5px">The rest of the time</span><select id="sc_basesel"><option value="'+SC_BACK+'"'+(scBase===SC_BACK||!scBase?' selected':'')+'>nothing: back to what it ran before</option>'
  +ps.map(([k,p])=>'<option value="'+hesc(k)+'"'+(k===scBase?' selected':'')+'>runs '+hesc(p.label)+'</option>').join('')+'</select><span class="muted" style="font-size:11.5px">'
  +(scBase&&scBase!==SC_BACK?'(the unpainted blocks, shown lighter)':'(unpainted blocks change nothing: after a painted stretch the miner goes back to the preset or clock it was on before it)')+'</span>'):'';
 if($('sc_basesel'))$('sc_basesel').onchange=()=>{const old=scBase,g=scGrid(scRules).map(r=>r.map(x=>x===old?null:x));scBase=$('sc_basesel').value;if(scRules.length)scRules=scRulesFromGrid(g);scChanged(true);};
 $('sc_brushes').querySelectorAll('.sc-brush').forEach(b=>b.onclick=()=>{scBrush=b.dataset.k;scRenderBrushes();});
 $('sc_fill').onclick=scFillWeek;}
function scFillWeek(){
 if(scBrush===SC_RS){const per=7*24,sec=((SC&&SC.restart_s)||75)+60;
  if(!confirm('Mark a restart every hour of the week ('+per+' a week)?\n\nEach restart stops hashing for about '+(sec-60)+' s plus a minute or so to get back to full speed: about '+(per*sec/(7*86400)*100).toFixed(0)+'% less mining, and the best share since restart starts again every hour. Once a day or once a week is plenty for keeping a miner fresh.'))return;
  const hourly=[];for(let h=0;h<per;h++)hourly.push(h*60);scRs.marks=hourly;scChanged();return;}
 if(scBrush===SC_ERASE){if(!confirm('Clear the whole week: every painted preset and every marked restart?'))return;scRules=[];scRs.marks=[];scChanged(true);return;}
 if(!scBrush)return;scRules=[{start:'00:00',days:[0],preset:scBrush}];scChanged(true);}
function scRestartsWeek(){const out={},an=scAppNow(),today=an.dow;
 const add=(d,min)=>{const k=Math.floor(min/SC_SLOT);out[d]=out[d]||{};const l=(out[d][k]=out[d][k]||[]),t=scHHMM(min);if(!l.includes(t))l.push(t);};
 (scRs.marks||[]).forEach(m=>add(Math.floor(m/1440),m%1440));
 if(!scRs||scRs.every==='off')return out;
 const at=/^\d{2}:\d{2}$/.test(scRs.at||'')?scHm(scRs.at):240,hh=Math.floor(at/60),mm=at%60;
 for(let d=0;d<7;d++){const date=new Date(an.y,an.mo,an.d+(d-today));
  if(scRs.every==='hour')for(let h=0;h<24;h++)add(d,h*60+mm);
  else if(scRs.every==='6h'||scRs.every==='12h'){const st=scRs.every==='6h'?6:12;for(let h=0;h<24;h++)if(((h-hh)%st+st)%st===0)add(d,h*60+mm);}
  else if(scRs.every==='day')add(d,at);
  else if(scRs.every==='week'){if(d===Number(scRs.day||0))add(d,at);}
  else if(scRs.every==='month'||scRs.every==='3months'){if(date.getDate()!==Number(scRs.mday||1))continue;
   if(scRs.every==='3months'&&scRs.anchor!=null&&(((date.getFullYear()*12+date.getMonth()-scRs.anchor)%3)+3)%3)continue;add(d,at);}}
 return out;}
function scDrawWeek(){const g=scGrid(scRules),today=scAppNow().dow,RS=scRestartsWeek(),marks=new Set(scRs.marks||[]);
 let h='<div></div><div class="sc-hrs">'+[0,3,6,9,12,15,18,21].map(x=>'<span class="'+(x>=12?'pm':'')+'">'+String(x).padStart(2,'0')+':00</span>').join('')+'</div>';
 for(let d=0;d<7;d++){let cells='';
  for(let k=0;k<SC_N;k++){const key0=g[d][k],key=key0===SC_BACK?null:key0,sh=key==null?null:scShade(key),rest=key!=null&&key===scBase;
   const first=key!=null&&!rest&&(k===0?g[(d+6)%7][SC_N-1]!==key0:g[d][k-1]!==key0);
   const t0=scHHMM(k*SC_SLOT),t1=k===SC_N-1?'24:00':scHHMM((k+1)*SC_SLOT),rsAt=(RS[d]||{})[k],marked=marks.has(d*1440+k*SC_SLOT);
   const tip=SC_DAYS[d]+' '+t0+'–'+t1+'<br><b>'+(key==null?(key0===SC_BACK?'nothing scheduled (back to what it ran before)':'no schedule'):hesc(scLabel(key)))+'</b>'+(rest?' (the rest of the time)':'')+(first?'<br>switches here':'')+(rsAt?('<br>↻ restart at '+rsAt.join(', ')+(marked?' (marked)':' (repeat)')):'');
   const bg=sh!=null?('background:'+(rest?('color-mix(in srgb, var(--ramp-'+sh+') 40%, var(--cell-empty))'):('var(--ramp-'+sh+')'))):'';
   cells+='<div class="sc-blk'+(sh!=null&&sh>=3&&!rest?' strong':'')+(k%2===0&&k?' hr':'')+(rsAt?' rs':'')+'" data-d="'+d+'" data-k="'+k+'" data-tip="'+hesc(tip).replace(/"/g,'&quot;')+'" style="'+bg+'">'+(first?hesc(scShort(key)):'')+'</div>';}
  h+='<div class="day'+(d===today?' today':'')+'">'+SC_DAYS[d]+'</div><div class="sc-blocks">'+cells+'</div>';}
 $('sc_week').innerHTML=h;scDrawWeek.day=today;scPlaceNow();
 const ps=scPresets();
 $('sc_key').innerHTML=(ps.length?ps.map(([k,p])=>'<span><i style="background:var(--ramp-'+scShade(k)+')"></i>'+hesc(scShort(k))+' = '+hesc(p.label)+'</span>').join('')
   +(scBase&&scBase!==SC_BACK?('<span><i style="background:color-mix(in srgb, var(--ramp-'+scShade(scBase)+') 40%, var(--cell-empty))"></i>lighter = the rest of the time ('+hesc(scLabel(scBase))+')</span>'):'<span><i style="background:var(--cell-empty)"></i>empty = nothing scheduled</span>')
   +'<span><i style="width:3px;border-radius:1px;background:var(--ok);box-shadow:none"></i>now (the miner’s clock)</span><span>Letters mark where a preset starts.</span>':'')
  +(Object.keys(RS).length?'<span><i style="box-shadow:inset 0 0 0 2px var(--text)"></i><b>↻</b> = restart</span>':'');}
function scPlaceNow(){const wk=$('sc_week');if(!wk||!wk.offsetParent)return;
 wk.querySelectorAll('.sc-nowseg,.sc-nowlab').forEach(e=>e.remove());
 const an=scAppNow();if(scDrawWeek.day!=null&&an.dow!==scDrawWeek.day)return scDrawWeek();
 const slot=Math.min(SC_N-1,Math.floor(an.min/SC_SLOT)),frac=(an.min-slot*SC_SLOT)/SC_SLOT;
 for(let d=0;d<7;d++){const el=wk.querySelector('.sc-blk[data-d="'+d+'"][data-k="'+slot+'"]');if(!el)continue;
  const seg=document.createElement('div');seg.className='sc-nowseg'+(d===an.dow?' today':'');
  seg.style.left=(el.offsetLeft+frac*el.offsetWidth)+'px';seg.style.top=(el.offsetTop-3)+'px';seg.style.height=(el.offsetHeight+6)+'px';wk.appendChild(seg);
  if(d===an.dow){const lab=document.createElement('div'),mm=Math.floor(an.min);lab.className='sc-nowlab';lab.textContent=scHHMM(mm);
   const hrs=wk.querySelector('.sc-hrs');lab.style.left=seg.style.left;lab.style.top=(hrs?hrs.offsetTop-1:0)+'px';wk.appendChild(lab);}}}
setInterval(()=>{if(!document.hidden)scPlaceNow();},15000);
window.addEventListener('resize',()=>{clearTimeout(scPlaceNow._r);scPlaceNow._r=setTimeout(scPlaceNow,100);});
// painting: a click paints one block, a drag paints the rectangle from the first block (drag down for several days)
let scPainting=false,scFrom=null,scPaintBase=null,scMarkBase=null,scMarkMode=null;
function scRect(el){const d=+el.dataset.d,k=+el.dataset.k,f=scFrom||[d,k],out=[];
 for(let dd=Math.min(f[0],d);dd<=Math.max(f[0],d);dd++)for(let kk=Math.min(f[1],k);kk<=Math.max(f[1],k);kk++)out.push([dd,kk]);return out;}
function scPaintAt(el){if(!el)return;const cells=scRect(el);
 if(scBrush===SC_RS||scBrush===SC_ERASE){const set=new Set(scMarkBase),add=scBrush===SC_RS&&scMarkMode==='add';
  for(const [dd,kk] of cells.sort((a,b)=>a[0]*48+a[1]-(b[0]*48+b[1]))){const m=dd*1440+kk*SC_SLOT;
   if(!add){set.delete(m);continue;}
   if(set.size>=168||[...set].some(x=>Math.min(Math.abs(x-m),7*1440-Math.abs(x-m))<60))continue;set.add(m);}
  scRs.marks=[...set].sort((a,b)=>a-b);if(scBrush===SC_RS){scChanged();return;}}
 if(scBrush!==SC_RS&&scPaintBase){const g=scPaintBase.map(r=>r.slice());for(const [dd,kk] of cells)g[dd][kk]=scBrush===SC_ERASE?null:scBrush;scRules=scRulesFromGrid(g);}
 scChanged(true);}
// a finger on the week only paints on a tap or a sideways slide: an up/down swipe scrolls the page (touch-action:pan-y)
// and paints nothing (the browser cancels the pointer when it scrolls)
let scTouch=null;
document.addEventListener('pointerdown',e=>{const el=e.target.closest&&e.target.closest('.sc-blk');if(!el||!scBrush)return;
 if(e.pointerType==='touch'){scTouch={el,x:e.clientX,y:e.clientY};return;}
 scBegin(el);e.preventDefault();});
function scBegin(el){
 scPainting=true;scFrom=[+el.dataset.d,+el.dataset.k];scMarkBase=[...(scRs.marks||[])];
 scMarkMode=scMarkBase.includes(scFrom[0]*1440+scFrom[1]*SC_SLOT)?'remove':'add';
 if(scBrush===SC_RS&&scMarkMode==='add'){const m=scFrom[0]*1440+scFrom[1]*SC_SLOT,near=scMarkBase.find(x=>Math.min(Math.abs(x-m),7*1440-Math.abs(x-m))<60);
  if(near!=null)toast('Restarts are kept at least an hour apart ('+SC_DAYS[Math.floor(near/1440)]+' '+scHHMM(near%1440)+' is too close)',true);}
 if(scBrush!==SC_RS){const g=scGrid(scRules);if(!scBase){scBase=SC_BACK;scRenderBrushes();}scPaintBase=g.map(r=>r.map(x=>x===scBase?null:x));}
 scPaintAt(el);}
window.addEventListener('pointermove',e=>{
 if(scTouch&&!scPainting){const dx=e.clientX-scTouch.x,dy=e.clientY-scTouch.y;if(Math.abs(dx)>8&&Math.abs(dx)>Math.abs(dy)){const el=scTouch.el;scTouch=null;scBegin(el);}}
 const tip=$('sc_tip');if(!tip)return;
 const el=document.elementFromPoint(e.clientX,e.clientY),blk=el&&el.closest&&el.closest('.sc-blk');
 if(scPainting&&blk)scPaintAt(blk);
 if(blk&&!scPainting){tip.innerHTML=blk.dataset.tip;tip.style.display='block';tip.style.left=Math.min(innerWidth-tip.offsetWidth-8,e.clientX+14)+'px';tip.style.top=(e.clientY+16)+'px';}
 else tip.style.display='none';});
window.addEventListener('pointerup',()=>{if(scTouch){const el=scTouch.el;scTouch=null;scBegin(el);}scPainting=false;scFrom=null;scPaintBase=null;scMarkBase=null;});
window.addEventListener('pointercancel',()=>{scTouch=null;scPainting=false;scFrom=null;scPaintBase=null;scMarkBase=null;});
function scRenderRules(){
 const opts=sel=>scPresets().map(([k,p])=>'<option value="'+hesc(k)+'"'+(k===sel?' selected':'')+'>'+hesc(p.label)+(p.mhz?(' ('+p.mhz+' MHz)'):'')+'</option>').join('')+'<option value="'+SC_BACK+'"'+(sel===SC_BACK?' selected':'')+'>back to what it ran before</option>';
 $('sc_rules').innerHTML=scRules.map((r,i)=>'<div class="sc-rule" data-i="'+i+'"><span class="lbl">At</span><input type="time" step="60" value="'+hesc(r.start)+'" data-k="start">'
  +'<span class="lbl">on</span><span class="sc-days">'+SC_DAYS.map((d,di)=>'<button data-d="'+di+'" class="'+((r.days||[]).includes(di)?'on':'')+'">'+d+'</button>').join('')+'</span>'
  +'<span class="lbl">switch to</span><select data-k="preset">'+opts(r.preset)+'</select><button data-del="'+i+'" title="Remove">×</button></div>').join('')||'<p class="muted" style="margin:3px 0">No changes yet.</p>';
 $('sc_rules').querySelectorAll('.sc-rule').forEach(row=>{const i=+row.dataset.i;
  row.querySelector('[data-k=start]').onchange=e=>{scRules[i].start=e.target.value;scChanged();};
  row.querySelector('[data-k=preset]').onchange=e=>{scRules[i].preset=e.target.value;scChanged();};
  row.querySelectorAll('[data-d]').forEach(b=>b.onclick=()=>{const d=+b.dataset.d,set=new Set(scRules[i].days||[]);set.has(d)?set.delete(d):set.add(d);scRules[i].days=[...set].sort();scChanged(true);});
  row.querySelector('[data-del]').onclick=()=>{scRules.splice(i,1);scChanged(true);};});}
function scCheck(){const errs=[],seen={},ok=Object.fromEntries(scPresets());
 scRules.forEach(r=>{if(!/^\d{2}:\d{2}$/.test(r.start||''))errs.push('Every change needs a time.');
  if(!(r.days||[]).length)errs.push('The '+(r.start||'?')+' change needs at least one day.');
  if(!ok[r.preset]&&r.preset!==SC_BACK)errs.push('Pick a preset for the '+(r.start||'?')+' change.');
  (r.days||[]).forEach(d=>{const k=d+'|'+r.start;if(seen[k])errs.push('Two changes at '+r.start+' on '+SC_DAYS[d]+'.');seen[k]=1;});});
 if($('sc_on').checked&&!scRules.length)errs.push('Add at least one change before turning the schedule on.');
 if(['hour','6h','12h','day','week','month','3months'].includes(scRs.every)&&!/^\d{2}:\d{2}$/.test(scRs.at||''))errs.push('Set a time for the restart.');
 if((scRs.every==='month'||scRs.every==='3months')&&!(scRs.mday>=1&&scRs.mday<=28))errs.push('The restart’s day of the month must be 1-28 (so it’s in every month).');
 return [...new Set(errs)];}
function scRsBody(){const b={every:scRs.every,marks:scRs.marks||[]};
 if(scRs.every!=='off'){b.at=scRs.at;if(scRs.every==='week')b.day=Number(scRs.day||0);if(scRs.every==='month'||scRs.every==='3months')b.mday=Number(scRs.mday||1);}
 return b;}
const scSnap=()=>JSON.stringify({e:$('sc_on').checked,r:scRules,rs:scRsBody(),b:scBase});
function scRenderRestart(){const sel=$('sc_rsevery');
 if(!sel.options.length){sel.innerHTML=SC_EVERY.map(x=>'<option value="'+x[0]+'">'+x[1]+'</option>').join('');$('sc_rsday').innerHTML=SC_DAYS.map((d,i)=>'<option value="'+i+'">'+d+'</option>').join('');}
 sel.value=scRs.every;$('sc_rsat').value=scRs.at||'04:00';$('sc_rsday').value=String(scRs.day||0);$('sc_rsmday').value=scRs.mday||1;
 const e=scRs.every;
 document.querySelector('.sc-rs-at').classList.toggle('hide',e==='off');
 document.querySelector('.sc-rs-day').classList.toggle('hide',e!=='week');
 document.querySelector('.sc-rs-mday').classList.toggle('hide',e!=='month'&&e!=='3months');
 $('sc_rsat').title=e==='hour'?'Only the minutes count: every hour at this many minutes past':'';
 scRenderRestartCost();}
function scRenderRestartCost(){const nm=(scRs.marks||[]).length;
 const per=({hour:168,'6h':28,'12h':14,day:7,week:1,month:12/52,'3months':4/52}[scRs.every]||0)+nm;
 $('sc_rsmarks').textContent=nm?(nm+' restart'+(nm===1?'':'s')+' marked on the week (each repeats weekly).'):'No restarts marked on the week.';
 const el=$('sc_rscost');if(!per){el.innerHTML='';return;}
 const sec=((SC&&SC.restart_s)||75)+60,pct=per*sec/(7*86400)*100,day=per/7;
 const txt=(day>=1?('About '+(+day.toFixed(1))+' restart'+(day>=1.05?'s':'')+' a day'):('About '+(+per.toFixed(per<1?2:0))+' restart'+(per===1?'':'s')+' a week'))+'. Each one stops hashing for about '+(sec-60)+' s plus a minute or so to get back to full speed, so about '+(pct<0.1?'<0.1':pct.toFixed(pct<1?1:0))+'% less mining, and the best share since restart starts again from 0 each time.';
 el.innerHTML=day>=4?('<div class="note warn">'+txt+' That’s a lot: once a day or once a week is plenty for keeping a miner fresh.</div>'):('<p class="sc-hint">'+txt+'</p>');}
function scRenderRestartStatus(){const s=(SC&&SC.schedule)||{},r=s.restart||{},pill=$('sc_rspill');
 const on=!!((r.every&&r.every!=='off')||(r.marks||[]).length);pill.textContent=on?'on':'off';pill.className='pill'+(on?' ok':'');
 const last=s.restart_last;
 $('sc_rsnext').textContent=(on?('Saved: restart '+(s.restart_line||'')+(s.restart_next?(' · next '+scFmtD(s.restart_next)):'')):'No scheduled restart saved.')+(last?(' · last: '+last.msg+' '+scFmtD(last.at)):'');}
function scRsChange(){scRs.every=$('sc_rsevery').value;scRs.at=$('sc_rsat').value;scRs.day=Number($('sc_rsday').value);scRs.mday=Number($('sc_rsmday').value);scRenderRestart();scChanged();}
function scChanged(rerender){if(rerender)scRenderRules();scRenderRestartCost();scDrawWeek();
 const errs=scCheck(),d=scSnap()!==scSaved;
 $('sc_err').innerHTML=errs.map(hesc).join('<br>');
 $('sc_dirty').textContent=d?'unsaved changes':'saved';$('sc_dirty').className='pill'+(d?' warn':'');
 $('sc_save').disabled=$('sc_save2').disabled=!d||errs.length>0;$('sc_undo').disabled=!d;}
function scRenderStatus(){const s=SC.schedule||{};
 const st=SC.tuning?['waiting: being tuned','warn']:!s.enabled?['off','']:s.paused?['paused','warn']:['running','ok'];
 $('sc_state').textContent=st[0];$('sc_state').className='pill '+st[1];
 const ch=SC.charge||'',pn=ch.indexOf('preset:')===0?ch.slice(7):(ch==='stock'?(B2T()?'Hashrate':'Stock'):''),ap=pn?scPreset(pn):null; // Box II: Stock is Stock High's plan
 $('sc_now').textContent=ch==='idle'?'On the miner now: Idle (not hashing)':ch==='tuner'?'On the miner now: the tuner is running':ap?('On the miner now: '+ap.label+(ap.mhz?(' ('+ap.mhz+' MHz · '+vLine(ap.mv,ap.pv)+')'):'')):'On the miner now: no preset (its own or a hand-set clock)';
 $('sc_next').textContent=s.now?('Schedule: '+s.now+' since '+scFmt(s.since)+(s.next?(' · next '+s.next+' at '+scFmt(s.next_at)):'')):'No schedule yet.';
 let n='';
 if(scPresets().length<=2)n+='<div class="note warn">This miner has no presets of its own yet (only Stock and Idle). Run a search on the <a href="#" onclick="showTab(\'tuner\');return false">Tuner</a> with <i>Build presets</i> ticked, or make one on <a href="#" onclick="showTab(\'presets\');return false">Presets</a>.</div>';
 if(SC.tuning)n+='<div class="note warn">A tuning run is going on this miner, so the schedule waits. It carries on by itself when the run ends.</div>';
 if(s.enabled&&s.paused)n+='<div class="note warn">Paused: '+hesc(s.paused.reason)+' ('+scFmt(s.paused.at)+'). <button onclick="scResume()">Resume the schedule</button></div>';
 if(s.error)n+='<div class="note bad">Last change didn’t go through: '+hesc(s.error)+'. It’s tried again every minute.</div>';
 $('sc_notes').innerHTML=n;}
function scPick(force){const s=(SC&&SC.schedule)||{};
 scRules=JSON.parse(JSON.stringify(s.rules||[]));
 scRs=Object.assign({every:'off',at:'04:00',day:0,mday:1,marks:[]},JSON.parse(JSON.stringify(s.restart||{})));
 scBase=s.base||SC_BACK;$('sc_on').checked=!!s.enabled;scSaved=scSnap();
 scRenderStatus();scRenderRestart();scRenderRestartStatus();scRenderBrushes();scRenderRules();scChanged();}
async function loadSchedule(keep){let j;try{const r=await fetch('/api/schedule');if(r.status===401){boot();return;}j=await r.json().catch(()=>({ok:false,err:'the miner sent an unexpected answer (HTTP '+r.status+')'}));}catch(e){j={ok:false,err:'can’t reach the miner'};}
 // not loaded: say so and try again; until it loads, Save and Clear refuse (they'd overwrite the real schedule with this empty page)
 if(!j.ok){if(!SC){$('sc_now').textContent='—';$('sc_next').textContent='Couldn’t load the schedule ('+(j.err||'error')+'). Trying again…';retryLater(loadSchedule,5000);}else if(!keep)toast('Couldn’t load the schedule: '+(j.err||'error'),true);return;}
 SC=j;
 $('sc_tz').textContent=SC.timezone+' (UTC'+SC.utc_offset.slice(0,3)+':'+SC.utc_offset.slice(3)+')';
 if(keep&&scSnap()!==scSaved){scRenderStatus();scRenderRestartStatus();scDrawWeek();return;}
 scPick();}
function scAdd(){const ps=scPresets();if(!ps.length)return toast('No presets yet',true);scRules.push({start:'08:00',days:[0,1,2,3,4,5,6],preset:ps[0][0]});scChanged(true);}
function scPickKey(){const ok=Object.fromEntries(scPresets()),keys=[...arguments],low=Object.keys(ok).reduce((m,k)=>(m[k.toLowerCase()]=k,m),{});for(const k of keys){if(low[k])return low[k];}return (scPresets()[0]||[])[0];}
function scTpl(t){if(!scPresets().length)return toast('No presets yet',true);
 if(scRules.length&&!confirm('Replace the changes below with this example?'))return;const ALL=[0,1,2,3,4,5,6];
 if(t==='nights')scRules=[{start:'07:00',days:ALL,preset:scPickKey('high','middle','stock')},{start:'23:00',days:ALL,preset:scPickKey('lowest power','lowest','low','stock')}];
 if(t==='tariff')scRules=[{start:'08:00',days:ALL,preset:scPickKey('low','middle','stock')},{start:'23:00',days:ALL,preset:scPickKey('high','middle','stock')}];
 if(t==='weekend')scRules=[{start:'00:00',days:[0,1,2,3,4],preset:scPickKey('high','middle','stock')},{start:'00:00',days:[5,6],preset:scPickKey('low','lowest power','lowest','stock')}];
 $('sc_on').checked=true;scChanged(true);}
function scUndo(){scPick();}
async function scSave(){if(!SC)return toast('The schedule hasn’t loaded yet: nothing was saved',true);const errs=scCheck();if(errs.length)return toast(errs[0],true);
 const sorted=[...scRules].sort((a,b)=>scHm(a.start)-scHm(b.start));
 const {j}=await jpost('/api/schedule',{action:'save',enabled:$('sc_on').checked,rules:sorted,restart:scRsBody(),base:scBase});
 if(!j.ok)return toast(j.err||'save failed',true);
 const rl=j.schedule&&j.schedule.restart_next?(' · next restart '+scFmtD(j.schedule.restart_next)):'';
 toast(($('sc_on').checked?'Schedule saved: the change in force now is applied within about 30 s':'Schedule saved (preset changes off)')+rl);
 await loadSchedule();}
async function scClear(){if(!SC)return toast('The schedule hasn’t loaded yet: nothing was changed',true);if(!confirm('Clear the preset changes? The presets and the scheduled restart stay; nothing changes on the miner.'))return;
 const {j}=await jpost('/api/schedule',{action:'save',enabled:false,rules:[],restart:scRsBody(),base:scBase});
 if(j.ok){toast('Schedule cleared');await loadSchedule();}else toast(j.err||'failed',true);}
async function scResume(){const {j}=await jpost('/api/schedule',{action:'pause',paused:false});
 if(j.ok){toast('Resumed: applying the change in force now');await loadSchedule(true);}else toast(j.err||'failed',true);}
window.addEventListener('beforeunload',e=>{if(SC&&scSnap()!==scSaved){e.preventDefault();e.returnValue='';}});
setInterval(()=>{if(!document.hidden&&SC&&!$('sc_week').closest('.tabpane').classList.contains('hide'))loadSchedule(true);},30000);
async function loadPoolGuard(){try{const g=await (await fetch('/api/poolguard')).json();const el=$('poolguard');if(!el||!g.ok)return;
 let h='🛡 <b>Pool guard:</b> the miner only mines the pools listed here. A pool or account that isn’t in this list is removed from the running miner within a minute (and if it keeps coming back, the miner goes to Idle).';
 if(g.checked)h+=' Last check '+new Date(g.checked*1000).toLocaleTimeString()+': only your pools.';
 if(g.hold)h+='<div class="warnbar" style="margin-top:8px">⛔ <b>Mining is held on Idle by the pool guard</b> since '+hesc(new Date(g.hold.at*1000).toLocaleString())+': a pool that isn’t in this list kept coming back ('+hesc(g.hold.what||'')+'). Presets, Stock and the schedule won’t start mining until you resume. Check the pool list (and anything else that can change the miner’s pools), then <button onclick="poolGuardResume()">Resume</button></div>';
 (g.incidents||[]).slice(-3).reverse().forEach(i=>{h+='<div style="color:var(--warn)">'+hesc(new Date(i.t*1000).toLocaleString())+': '+hesc(i.action)+' '+hesc(i.pool)+' (account '+hesc(i.user||'-')+')'+(i.err?' · '+hesc(i.err):'')+'</div>';});
 el.innerHTML=h;}catch(e){}}
async function poolGuardResume(){if(!confirm('Resume? The hold is lifted; apply '+(B2T()?'Stock High':'Stock')+' or a preset to start mining again.'))return;try{await jpost('/api/poolguard',{resume:true});}catch(e){}loadPoolGuard();}
async function loadPools(){loadPoolGuard();try{const r=await fetch('/api/pools');if(r.status===401){boot();return;}const j=await r.json();
 if(!j.ok){$('poolbody').innerHTML=busyHTML(j.err,j.detail);$('poolnote').textContent='';retryLater(loadPools);return;}
 renderPools(j.pools||[],j.configured,j.idle);}catch(e){$('poolbody').innerHTML=busyHTML(OFFLINE);retryLater(loadPools);}}
function renderPools(P,cfgOnly,idle){window._pools=P;$('poolnote').textContent=P.length+' pool'+(P.length===1?'':'s');
 if(!P.length){$('poolbody').innerHTML='<span class="muted">no pools configured</span>';return;}
 let h='';
 if(cfgOnly){
  h+='<div class="note" style="margin-bottom:8px">'+(idle?'The miner is <b>idle</b>, so it isn’t connected to a pool right now. These are the pools it uses when it mines again (Presets → '+(B2T()?'Stock High':'Stock')+', or any preset).':'The miner isn’t mining right now (starting up, or stopped), so it can’t report its pools’ status. These are the pools it’s set up with.')+' Editing them here is available while it mines.</div>';
  P.forEach(p=>{h+='<div class="poolrow"><div><div class="pl-url">'+(p.index===0?'<span class="pill">primary</span> ':'')+hesc(p.url)+'</div>'
   +'<div class="muted" style="font-size:12px;word-break:break-all">'+hesc(p.user)+'</div></div>'
   +'<div class="pl-stat"><span class="pill">'+hesc(p.status)+'</span></div><div class="pl-act"></div></div>';});
  $('poolbody').innerHTML=h;return;}
 P.forEach(p=>{const alive=/alive/i.test(p.status);
  h+='<div class="poolrow" id="pool'+p.index+'"><div><div class="pl-url">'+(p.active?'<span class="pill ok">ACTIVE</span> ':'')+hesc(p.url)+'</div>'
   +'<div class="muted" style="font-size:12px;word-break:break-all">'+hesc(p.user)+'</div></div>'
   +'<div class="pl-stat"><span class="pill '+(alive?'ok':'bad')+'">'+hesc(p.status||'—')+'</span><br><span class="muted">A '+p.accepted+' · R '+p.rejected+' · S '+p.stale+'</span></div>'
   +'<div class="pl-act">'+(p.active?'':'<button onclick="poolSwitch('+p.index+')">Make primary</button>')+'<button onclick="poolEdit('+p.index+')">Edit</button><button onclick="poolDelete('+p.index+')">Delete</button></div></div>';});
 $('poolbody').innerHTML=h;}
function poolAt(i){return (window._pools||[]).find(p=>p.index===i);}
function poolId(i){const p=poolAt(i)||{};return {old_url:p.url||'',old_user:p.user||''};}
function poolEdit(i){const p=poolAt(i);if(!p)return;const row=$('pool'+i);if(!row)return;
 row.style.gridTemplateColumns='1fr';
 row.innerHTML='<div class="muted" style="font-size:12px;margin-bottom:4px">Edit pool '+(i+1)+'</div>'
  +'<input id="ped_url'+i+'" value="'+hesc(p.url)+'">'
  +'<input id="ped_user'+i+'" value="'+hesc(p.user)+'">'
  +'<input id="ped_pass'+i+'" placeholder="password — leave blank to keep current">'
  +'<div class="row" style="margin-top:4px"><button class="primary" onclick="poolSaveEdit('+i+')">Save</button><button onclick="loadPools()">Cancel</button></div>';}
async function poolSaveEdit(i){const url=$('ped_url'+i).value.trim();if(!url){toast('Enter a pool address.',true);return;}
 if(!confirm('Save changes to this pool? The miner restarts briefly to apply.'))return;
 const {j}=await jpost('/api/pools',{...poolId(i),action:'edit',index:i,url,user:$('ped_user'+i).value.trim(),pass:$('ped_pass'+i).value,confirm:true});
 if(j.ok){$('poolnote').textContent='saving…';setTimeout(loadPools,4000);}else toast('failed: '+(j.err||''),true);}
async function addPool(){const url=$('pl_url').value.trim();if(!url){toast('Enter a pool address.',true);return;}
 if(!confirm('Add this pool? The miner restarts briefly to apply.\n\n'+url))return;
 const {j}=await jpost('/api/pools',{action:'add',url,user:$('pl_user').value.trim(),pass:$('pl_pass').value,confirm:true});
 if(j.ok){$('pl_url').value='';$('pl_user').value='';$('pl_pass').value='';$('poolnote').textContent='saved';setTimeout(loadPools,4000);}else toast('failed: '+(j.err||''),true);}
async function poolSwitch(i){if(!confirm('Make this pool the primary? The miner restarts briefly.'))return;
 const {j}=await jpost('/api/pools',{...poolId(i),action:'switch',index:i,confirm:true});if(j.ok){$('poolnote').textContent='switching…';setTimeout(loadPools,4000);}else toast('failed: '+(j.err||''),true);}
async function poolDelete(i){if(!confirm('Delete this pool? The miner restarts briefly.'))return;
 const {j}=await jpost('/api/pools',{...poolId(i),action:'delete',index:i,confirm:true});if(j.ok){$('poolnote').textContent='deleting…';setTimeout(loadPools,4000);}else toast('failed: '+(j.err||''),true);}
const SIC={pass:'✓',fail:'✗',run:'●',todo:'⇢',skip:'⇢',cut:'■'};
const SCL={pass:'var(--ok)',fail:'var(--bad)',run:'var(--warn)',todo:'var(--muted)',skip:'var(--muted)',cut:'var(--warn)'};
const SBADGE={pass:'done',fail:'failed',run:'running now',skip:'skipped',cut:'cut short',todo:''};
function sIc(st){st=st||'todo';return '<b class="sic" style="color:'+(SCL[st]||'var(--muted)')+'">'+(SIC[st]||'⇢')+'</b>';}
// hierarchical run plan (app parity): phases → clocks (with inline voltage ladder) / presets
function tunerPlan(steps){if(!steps||!steps.length)return '';
 let h='<div class="planbox"><div class="muted" style="font-size:12px;margin-bottom:6px">Run plan</div>';
 steps.forEach(ph=>{const st=ph.state||'todo';
  h+='<div class="planrow" style="margin-top:6px">'+sIc(st)+' <b>'+hesc(ph.label||'')+'</b>'
   +(SBADGE[st]?' <span class="muted" style="font-size:11px">'+SBADGE[st]+'</span>':'')
   +(ph.note?' <span class="muted" style="font-size:11px">· '+hesc(ph.note)+'</span>':'')+'</div>';
  if(ph.desc)h+='<div class="muted" style="font-size:11px;padding-left:18px;margin:1px 0 3px;line-height:1.4">'+hesc(ph.desc)+'</div>';
  (ph.kids||[]).forEach(k=>{const ks=k.state||'todo';
   let row='<div class="planrow" style="padding-left:18px;font-size:12.5px;margin:1px 0">'+sIc(ks)+' '+hesc(k.label||'');
   (k.kids||[]).forEach(v=>{const vs=v.state||'todo';row+=' <span class="muted">→</span> '+sIc(vs)+' '+hesc(v.label||'')+(v.note?' <span class="muted" style="font-size:10px">('+hesc(v.note)+')</span>':'');});
   if(k.note)row+=' <span class="muted" style="font-size:11px">· '+hesc(k.note)+'</span>';
   h+=row+'</div>';});
 });
 h+='<div class="muted" style="font-size:11px;margin-top:9px;display:flex;flex-wrap:wrap;gap:3px 10px">'
  +'<span style="color:var(--ok)">✓</span> done · <span style="color:var(--bad)">✗</span> failed · <span style="color:var(--warn)">●</span> running now · <span>⇢</span> to do / skipped · <span style="color:var(--warn)">■</span> cut short</div>';
 return h+'</div>';}
// finished-tests table (each completed, judged test), newest first — the app's run-results grid
// SC Lite power estimate (blake2b-asic-control's model): chips follow clock × PV², calibrated to 899 W at 625 MHz · PV 9400
// calNote: while no wall-meter reading was given, say the watts are estimates and where to calibrate.
function calNote(){if(featOff2(F().PowerEstimate))return ''; // no SC Lite power model on a model that hasn't been measured
 if(isOtherModel())return ''; // Box II: no clock×PV² estimate is shown, so there's nothing here to caveat (Settings → Power explains its own figures)
 if(window._powF)return '';
 return '<div class="note" style="margin:8px 0;font-size:12px">Watts and J/TH here are estimates from typical SC Lite figures. For your unit’s real numbers, calibrate with a plug-in wall meter in <a href="#" onclick="gotoPower();return false">Settings → Power &amp; calibration</a>.</div>';}
function gotoPower(){showTab('settings');setTimeout(()=>{const e=$('powerbody');if(e)e.scrollIntoView({behavior:'smooth',block:'center'});},600);}
function estW(mhz,pv){if(featOff2(F().PowerEstimate))return null; // the clock×PV² model is SC Lite's; off until this model is measured
 if(isOtherModel())return null; // Box II: watts only per measured setting (power_box2.go), never this model
 if(!mhz||!pv)return null;pv=Math.max(pv,9100); // the supply doesn't go below ~9.1 V (PV 9100, measured)
const eff=Math.floor(mhz/12.5)*12.5,nb=window._nHash||4; // the chips' part counts the boards that hash
return 899*(0.07+0.93*(nb/4)*(eff/625)*Math.pow(pv/9400,2))*(window._powF||1);}
// finished-tests table, newest first — the app's columns (TIME MHZ MV PV RESULT TH/S ≈W J/TH MAX°C) plus err/min, then the reason
function tunerResults(rows,baseMin){if(!rows||!rows.length)return '';return calNote()+tunerResultsTable(rows,baseMin);}
function tunerResultsTable(rows,baseMin){
 const word={clean:'PASS',failed:'FAIL',aborted:'FAIL (safety)',stopped:'stopped',confirmed:'CONFIRMED',baseline:'BASELINE'};
 const bm=baseMin||0;
 if(B2T())return tunerResultsBox2(rows,bm,word);
 let h='<div class="ctwrap" style="max-height:420px;border:none"><table class="ctable restable"><thead><tr><th>Time</th><th class="num">MHz</th><th class="num">mV</th><th class="num">PV</th><th>Result</th><th class="num">TH/s</th><th class="num">≈ W</th><th class="num">J/TH</th><th class="num">Max °C</th><th class="num">err/min</th><th>Reason</th></tr></thead><tbody>';
 for(let i=rows.length-1;i>=0;i--){const r=rows[i],res=r.result||'',pass=res==='clean'||res==='confirmed';
  const cls=pass?'res-pass':res==='stopped'?'res-base':'res-fail';
  const em=(r.min_s>0)?(r.err_add/r.min_s):null,hot=(em!=null)&&(bm>0?em>bm*1.5+0.05:em>0.2);
  const w=estW(r.mhz,r.pv);
  h+='<tr data-n="'+i+'" class="'+(String(i)===window._tmapSel?'picked':'')+'" style="cursor:pointer" title="Show this test on the chip map" onclick="tmapPick('+i+',true)"><td class="mono">'+hesc(r.time||'')+'</td><td class="num">'+r.mhz+'</td><td class="num">'+r.mv+'</td><td class="num">'+r.pv+'</td>'
   +'<td class="'+cls+'">'+hesc(word[res]||res.toUpperCase())+'</td>'
   +'<td class="num">'+(r.ths?r.ths.toFixed(2):'—')+'</td>'
   +'<td class="num">'+(w?Math.round(w):'—')+'</td>'
   +'<td class="num">'+(w&&r.ths>0.05?Math.round(w/r.ths):'—')+'</td>'
   +'<td class="num">'+(r.max_t?r.max_t.toFixed(1):'—')+'</td>'
   +'<td class="num"'+(hot?' style="color:var(--bad);font-weight:600"':'')+'>'+(em!=null?em.toFixed(2):'—')+'</td>'
   +'<td>'+hesc(r.reason||'')+'</td></tr>';}
 return h+'</tbody></table></div>';}
// tunerResultsBox2: the finished-tests table on Box II: the V-code in volts, no mV / PV columns and no watts or
// J/TH (this model's watts come only from wall-meter readings per stock setting: Settings → Power)
function tunerResultsBox2(rows,bm,word){
 let h='<div class="ctwrap" style="max-height:420px;border:none"><table class="ctable restable"><thead><tr><th>Time</th><th class="num">MHz</th><th>Voltage</th><th>Result</th><th class="num">TH/s</th><th class="num">Max °C</th><th class="num">err/min</th><th>Reason</th></tr></thead><tbody>';
 for(let i=rows.length-1;i>=0;i--){const r=rows[i],res=r.result||'',pass=res==='clean'||res==='confirmed';
  const cls=pass?'res-pass':res==='stopped'?'res-base':'res-fail';
  const em=(r.min_s>0)?(r.err_add/r.min_s):null,hot=(em!=null)&&(bm>0?em>bm*1.5+0.05:em>0.2);
  h+='<tr data-n="'+i+'" class="'+(String(i)===window._tmapSel?'picked':'')+'" style="cursor:pointer" title="Show this test on the chip map" onclick="tmapPick('+i+',true)"><td class="mono">'+hesc(r.time||'')+'</td><td class="num">'+r.mhz+'</td><td>'+boxVoltsTxt(r.mv)+'</td>'
   +'<td class="'+cls+'">'+hesc(word[res]||res.toUpperCase())+'</td>'
   +'<td class="num">'+(r.ths?r.ths.toFixed(2):'—')+'</td>'
   +'<td class="num">'+(r.max_t?r.max_t.toFixed(1):'—')+'</td>'
   +'<td class="num"'+(hot?' style="color:var(--bad);font-weight:600"':'')+'>'+(em!=null?em.toFixed(2):'—')+'</td>'
   +'<td>'+hesc(r.reason||'')+'</td></tr>';}
 return h+'</tbody></table></div>';}
// Live error chart: this test's chips with new errors, each as a bar towards its own limit (red line).
function tunerChart(chips){
 const rows=chips.filter(c=>c.limit>0).map(c=>({b:c.b,c:c.i,n:c.gained,lim:+c.limit,r:c.gained/c.limit}));
 const hit=rows.filter(x=>x.n>0).sort((a,b)=>b.r-a.r||b.n-a.n);
 const intro='<div class="tc-sum">'+(hit.length?('<b>'+hit.length+'</b> of '+rows.length+' chips have new errors in this test'):('<b>No new errors</b> on any of the '+rows.length+' chips so far'))
  +'. Each chip has its own limit, set from its normal error rate in the baseline; a bar past the red line fails the test.</div>';
 if(!hit.length)return intro;
 const show=hit.slice(0,8),scale=Math.max(1.25,Math.ceil(Math.max(...show.map(x=>x.r))*4)/4),limX=100/scale;
 let h=intro+'<div class="tc-rows">';
 show.forEach(x=>{const over=x.n>x.lim,cls=over?'bad':x.r>=0.6?'warn':'ok';
  h+='<div class="tc-row" title="'+hesc(chipLbl(x.b,x.c)+': '+x.n+' new error'+(x.n===1?'':'s')+', limit '+x.lim.toFixed(1)+(over?' (over: this test fails)':''))+'">'
   +'<span class="tc-l">'+hesc(chipLbl(x.b,x.c))+'</span>'
   +'<span class="tc-t"><span class="tc-f '+cls+'" style="width:'+Math.min(100,x.r/scale*100).toFixed(1)+'%"></span><span class="tc-lim" style="left:'+limX.toFixed(1)+'%"></span></span>'
   +'<span class="tc-v"><b>'+x.n+'</b> / '+x.lim.toFixed(x.lim<10?1:0)+' <span class="muted">· '+Math.round(x.r*100)+' %</span></span></div>';});
 h+='</div>';
 if(hit.length>show.length)h+='<div class="muted" style="font-size:11.5px;margin-top:4px">+ '+(hit.length-show.length)+' more chip'+(hit.length-show.length===1?'':'s')+' further from their limit (all of them are on the chip map)</div>';
 return h+'<div class="tlegend"><span><i style="background:var(--ok)"></i>under 60 % of its limit</span><span><i style="background:var(--warn)"></i>close</span><span><i style="background:var(--bad)"></i>over: the test fails</span><span><i style="background:var(--bad);width:2px;height:10px"></i>the chip’s limit</span></div>';}
// Chip map: Live (the test being watched) or any finished test, from the "Showing" menu or a click on a
// row of Finished tests. Finished tests come from the tuner (and its saved chipmap.json after a restart).
window._tmapSel=window._tmapSel||'live';
let TMAP={n:-1,list:[],cache:{},running:false,status:null};
async function tmapList(n){if(TMAP.n===n&&TMAP.list.length===n)return;
 try{const j=await (await fetch('/api/tuner/chipmap')).json();if(j.ok){TMAP.list=j.tests||[];TMAP.n=n==null?TMAP.list.length:n;}}catch(e){}}
function tmapOptions(){const sel=$('tmapSel');if(!sel)return;
 const word={clean:'PASS',failed:'FAIL',aborted:'FAIL (safety)',stopped:'stopped'};
 let o=TMAP.running?'<option value="live">Live: current test</option>':'';
 TMAP.list.slice().reverse().forEach(t=>{o+='<option value="'+t.n+'">'+hesc((t.time||'').slice(11,16)+'  '+t.mhz+' MHz · '+(B2T()?boxVoltsTxt(t.mv):t.mv+' mV')+'  '+(word[t.result]||t.result))+'</option>';});
 if(sel._h!==o){sel.innerHTML=o;sel._h=o;}
 if(window._tmapSel==='live'&&!TMAP.running)window._tmapSel=TMAP.list.length?String(TMAP.list.length-1):'live';
 if(![...sel.options].some(x=>x.value===window._tmapSel))window._tmapSel=sel.options.length?sel.options[0].value:'live';
 sel.value=window._tmapSel;}
async function tmapUpdate(s){TMAP.status=s;TMAP.running=!!(s&&s.running);
 await tmapList(s?(s.ntests||0):null);tmapOptions();tmapRender();}
function tmapPick(v,scroll){window._tmapSel=String(v);const sel=$('tmapSel');if(sel)sel.value=window._tmapSel;tmapRender();
 document.querySelectorAll('.restable tr[data-n]').forEach(tr=>tr.classList.toggle('picked',tr.dataset.n===window._tmapSel));
 if(scroll)$('tmapPanel').scrollIntoView({behavior:'smooth',block:'start'});}
async function tmapRender(){const mp=$('tmapPanel'),s=TMAP.status,v=window._tmapSel;
 if(!TMAP.running&&!TMAP.list.length){mp.classList.add('hide');return;}
 mp.classList.remove('hide');$('tmapLeg').innerHTML=tMapLegend();
 if(v==='live'){const cur=s&&s.cur;
  if(cur&&cur.settling){$('tmap').innerHTML='';$('tmapCap').textContent='Live: '+planTxt(cur.mhz,cur.mv,cur.pv)+' is settling; the map fills in once counting begins.';return;}
  if(!s||!s.chips||!s.chips.length||s.phase==='baseline'||!s.chips.some(c=>c.limit>0)){$('tmap').innerHTML='';
   $('tmapCap').textContent=s&&s.phase==='baseline'?'Live: the baseline only learns each chip’s normal error rate; the map shows the tests that follow.':'Live: waiting for the first reading of this test…';return;}
  $('tmap').innerHTML=tChipMap(s.chips);
  $('tmapCap').textContent='Live: '+(cur?planTxt(cur.mhz,cur.mv,cur.pv)+', '+(cur.elapsed_min||0).toFixed(1)+' of '+(cur.hold_min||25)+' min watched. ':'')+'Each square is a chip; the stronger the shade, the closer it is to its own limit.';
  return;}
 const n=+v;let t=TMAP.cache[n];
 if(!t){try{const j=await (await fetch('/api/tuner/chipmap?test='+n)).json();if(j.ok){t=j.test;TMAP.cache[n]=t;}}catch(e){}}
 if(window._tmapSel!==v)return;
 if(!t){$('tmap').innerHTML='';$('tmapCap').textContent='No chip data saved for that test.';return;}
 const chips=(t.chips||[]).map(c=>({b:c[0],i:c[1],gained:c[2],limit:c[3],pct:c[3]?c[2]/c[3]*100:0,base:c[4]}));
 $('tmap').innerHTML=tChipMap(chips);
 const word={clean:'passed',failed:'failed',aborted:'stopped for safety',stopped:'stopped'};
 $('tmapCap').textContent='Test at '+(t.time||'')+': '+planTxt(t.mhz,t.mv,t.pv)+', '+(+t.min_s||0).toFixed(1)+' min watched, '+(word[t.result]||t.result)+(t.reason&&t.reason!=='clean'?(' ('+t.reason+')'):'')+'. Each square is a chip; the stronger the shade, the closer it got to its own limit.';}

async function loadVerbose(){try{const r=await fetch('/api/verbose');const j=await r.json();if(j.ok){window._verboseShim=!!j.shim;$('verbose').classList.toggle('active',!!j.enabled);const vh=$('vhint');if(vh&&j.shim)vh.innerHTML='<b>Verbose</b> logs every submitted share as <span style="font-family:ui-monospace,monospace">➦ share accepted · nonce 0x… · INCS2</span>. Persists across reboots; toggling restarts mining (~1 min). It runs the miner in its debug mode: if mining keeps stopping with it on, it’s switched off by itself.'+(j.auto_off?('<div style="color:var(--bad);margin-top:4px">Verbose was switched off '+new Date(j.auto_off.at*1000).toLocaleString()+': the mining program stopped '+j.auto_off.stops+' times in 15 minutes with it on. Switch it on again only to look at something, and watch the Dashboard.</div>'):'');}}catch(e){}}
async function toggleVerbose(){const on=$('verbose').classList.contains('active');
 const msg=window._verboseShim
   ? (on?'Disable':'Enable')+' stratum protocol-dump (logs every submitted share'+String.fromCharCode(39)+'s nonce)?\n\nMining restarts (~1 min) to apply. This persists across reboots.'
   : (on?'Disable':'Enable')+' stratum protocol-dump?\n\nNote: this firmware rebuilds its miner config on restart and may drop it.';
 if(!confirm(msg))return;
 try{const r=await fetch('/api/verbose',{method:'POST',headers:{'Content-Type':'application/json','X-Requested-With':'panel'},body:JSON.stringify({enable:!on})});
  const j=await r.json();if(j.ok){
   window._verboseShim=!!j.shim;
   // light the button only when it actually took (shim) or the config path isn't firmware-managed
   $('verbose').classList.toggle('active',!!j.enabled&&(j.shim||!j.managed));
   $('log').innerHTML='';appendLine('('+(j.note||(j.enabled?'verbose enabled':'verbose disabled'))+')');}
  else toast('failed: '+(j.err||''),true);}catch(e){toast('Couldn’t reach the miner: try again in a minute.',true);}}
document.addEventListener('keydown',e=>{if(e.key==='Enter'&&!$('auth').classList.contains('hide')){if(!$('loginbox').classList.contains('hide'))doLogin();else if(!$('pwbox').classList.contains('hide'))doChange();}});
boot();
</script></body></html>`

// portOf: the port number of a listen address like ":8080" (0 if none).
func portOf(addr string) int {
	_, p, err := net.SplitHostPort(addr)
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(p)
	return n
}
