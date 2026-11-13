// Package authfile reads, checks and changes the control panel's login file (auth.json) from outside the panel:
// the SC Box II's SSH server (cmd/scbox2-sshd) signs in with the same account. The format and the hashes are
// the panel's (main.go authStore, setPassword, checkPassword, saveAuth); the panel's tests check that each side
// reads what the other writes.
package authfile

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"hash"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/blake2b"
)

const (
	DefaultUser = "admin"
	DefaultPass = "admin"
	SHA256Iter  = 60000 // PBKDF2-HMAC-SHA256 rounds (the format up to v1.5.21)
	B2Iter      = 20000 // PBKDF2-HMAC-BLAKE2b-512 rounds
)

// Store: the login file's fields (the panel's authStore). Fields this package doesn't know are kept when it
// changes the file (Update works on the file's own JSON object).
type Store struct {
	Username   string `json:"username"`
	Salt       string `json:"salt"`
	Hash       string `json:"hash"`
	Iter       int    `json:"iter"`
	B2Salt     string `json:"b2_salt,omitempty"`
	B2Hash     string `json:"b2_hash,omitempty"`
	B2Iter     int    `json:"b2_iter,omitempty"`
	MustChange bool   `json:"must_change"`
	Secret     string `json:"secret"`
}

// PBKDF2With: PBKDF2 (RFC 8018) with HMAC over the given hash.
func PBKDF2With(h func() hash.Hash, password, salt []byte, iter, keyLen int) []byte {
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

func Blake2b512() hash.Hash { h, _ := blake2b.New512(nil); return h }

func RandBytes(n int) []byte {
	b := make([]byte, n)
	rand.Read(b)
	return b
}

// Usable: a login file a password can match (the panel's authUsable, without its repair of a bad signing key).
func Usable(a *Store) bool {
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
	return strings.TrimSpace(a.Username) != ""
}

// Check: is pw the password in a? (The slow hash: the caller limits how many run at once.)
func Check(a *Store, pw string) bool {
	b2 := a.B2Hash != ""
	saltS, hashS, iter := a.Salt, a.Hash, a.Iter
	if b2 {
		saltS, hashS, iter = a.B2Salt, a.B2Hash, a.B2Iter
	}
	salt, _ := base64.StdEncoding.DecodeString(saltS)
	want, _ := base64.StdEncoding.DecodeString(hashS)
	if len(want) < 16 || iter <= 0 { // no stored hash: nothing matches (an empty hash would match anything)
		return false
	}
	var got []byte
	if b2 {
		got = PBKDF2With(Blake2b512, []byte(pw), salt, iter, len(want))
	} else {
		got = PBKDF2With(sha256.New, []byte(pw), salt, iter, len(want))
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}

// SetPassword: pw's hash into a, as the panel's setPassword: PBKDF2-HMAC-BLAKE2b-512 with the SHA-256 sentinel
// beside it, or (legacy: an older panel may come back, see LegacyOnly) PBKDF2-HMAC-SHA256 only.
func SetPassword(a *Store, pw string, legacy bool) {
	if legacy {
		salt := RandBytes(16)
		a.Salt = base64.StdEncoding.EncodeToString(salt)
		a.Iter = SHA256Iter
		a.Hash = base64.StdEncoding.EncodeToString(PBKDF2With(sha256.New, []byte(pw), salt, SHA256Iter, 32))
		a.B2Salt, a.B2Hash, a.B2Iter = "", "", 0
		return
	}
	salt := RandBytes(16)
	a.B2Salt = base64.StdEncoding.EncodeToString(salt)
	a.B2Iter = B2Iter
	a.B2Hash = base64.StdEncoding.EncodeToString(PBKDF2With(Blake2b512, []byte(pw), salt, B2Iter, 64))
	// the SHA-256 fields beside it: round count 0 and a hash an older panel computes for "admin" only (main.go
	// setLegacySentinel explains why they can't be left empty)
	s2 := RandBytes(16)
	m := hmac.New(sha256.New, []byte(DefaultPass))
	m.Write(s2)
	m.Write([]byte{0, 0, 0, 1})
	a.Salt, a.Hash, a.Iter = base64.StdEncoding.EncodeToString(s2), base64.StdEncoding.EncodeToString(m.Sum(nil)), 0
}

// LegacyOnly: the panel's authLegacyOnly as seen from outside it: an update waiting to be confirmed, or a roll
// back marked, in the update directory next to the login file. (A roll back the panel is preparing right now,
// in memory, can't be seen from here.)
func LegacyOnly(authPath string) bool {
	for _, f := range []string{"pending", "rollback"} {
		if _, err := os.Stat(filepath.Join(filepath.Dir(authPath), "update", f)); err == nil {
			return true
		}
	}
	return false
}

// Read: the login at path, or its backup (path.bak) when the file is damaged; the bytes it came from. Never
// writes (repairing the file is the panel's job). os.ErrNotExist when there's neither.
func Read(path string) (Store, []byte, error) {
	var a Store
	b, err := os.ReadFile(path)
	if err == nil && json.Unmarshal(b, &a) == nil && Usable(&a) {
		return a, b, nil
	}
	missing := err != nil && os.IsNotExist(err)
	a = Store{}
	if bb, err2 := os.ReadFile(path + ".bak"); err2 == nil && json.Unmarshal(bb, &a) == nil && Usable(&a) {
		return a, bb, nil
	}
	if missing {
		return Store{}, nil, os.ErrNotExist
	}
	if err == nil {
		err = errors.New("damaged")
	}
	return Store{}, nil, err
}

// Update changes the login file's fields in place: change gets the file's JSON object (unknown fields kept) and
// returns an error to leave the file as it is. Written the way the panel's saveAuth writes it: a temp file
// (tmpName in the same folder, the panel's own is auth.json.tmp) flushed to disk, the current file kept as .bak,
// then renamed over it, then the folder flushed. Returns the bytes written.
func Update(path, tmpName string, change func(obj map[string]json.RawMessage) error) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	obj := map[string]json.RawMessage{}
	if err := json.Unmarshal(b, &obj); err != nil {
		return nil, err
	}
	if err := change(obj); err != nil {
		return nil, err
	}
	nb, err := json.MarshalIndent(obj, "", "  ")
	if err != nil {
		return nil, err
	}
	return nb, Write(path, tmpName, nb)
}

// Write: b as the login file, the saveAuth way (see Update).
func Write(path, tmpName string, b []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp := filepath.Join(dir, tmpName)
	if err := writeFileSync(tmp, b, 0o600); err != nil {
		os.Remove(tmp) // (a full partition: the part written goes, not left taking room)
		return err
	}
	if cur, err := os.ReadFile(path); err == nil && json.Valid(cur) { // the last good copy
		writeFileSync(path+".bak", cur, 0o600)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	if d, err := os.Open(dir); err == nil { // the rename reaches the FAT before success is reported
		d.Sync()
		d.Close()
	}
	return nil
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

// SetFields: the Store's password fields, MustChange and Secret into obj (the file's JSON object).
func SetFields(obj map[string]json.RawMessage, a *Store) {
	put := func(k string, v any) { b, _ := json.Marshal(v); obj[k] = b }
	put("salt", a.Salt)
	put("hash", a.Hash)
	put("iter", a.Iter)
	if a.B2Hash != "" {
		put("b2_salt", a.B2Salt)
		put("b2_hash", a.B2Hash)
		put("b2_iter", a.B2Iter)
	} else {
		delete(obj, "b2_salt")
		delete(obj, "b2_hash")
		delete(obj, "b2_iter")
	}
	put("must_change", a.MustChange)
	put("secret", a.Secret)
}
