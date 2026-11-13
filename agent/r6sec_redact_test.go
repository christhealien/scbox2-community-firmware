package main

import (
	"strings"
	"testing"
)

// r6-security: the private log export must never carry a pool password. Goldshell's backend log prints every
// pool as JSON at each start ("pass": "..."), and with verbose on the miner's protocol dump (-P) logs the
// mining.authorize line. privateTokens() only knows the passwords when the backend answers, and only those
// of 6+ characters.
func TestR6RedactPoolPasswords(t *testing.T) {
	in := ` [2026-11-05 07:36:34]    "url": "stratum+tcp://pool.example:23334",
 [2026-11-05 07:36:34]    "user": "bc1qwallet.rig1",
 [2026-11-05 07:36:34]    "pass": "Hunt3rPw!",
 [2026-11-05 07:36:35]    "pass": "abc12",
 [2026-11-05 07:36:35] {"url":"stratum+tcp://p2.example:3333","user":"w.r","password":"d=8192,s3cret"}
[2026-11-05 07:37:00] SEND: {"id": 2, "method": "mining.authorize", "params": ["bc1qwallet.rig1", "Zz9secretZz"]}
[2026-11-05 07:37:00] pass 1 of 3 done, password checks ok`
	for name, toks := range map[string][]string{
		"backend down (no tokens)":       nil,
		"backend up (6+ char passwords)": {"Hunt3rPw!", "d=8192,s3cret", "Zz9secretZz"},
	} {
		out := redactLog(in, toks)
		for _, bad := range []string{"Hunt3rPw", "abc12", "s3cret", "Zz9secret"} {
			if strings.Contains(out, bad) {
				t.Errorf("%s: pool password %q left in the private export:\n%s", name, bad, out)
			}
		}
		if !strings.Contains(out, "pass 1 of 3 done, password checks ok") || !strings.Contains(out, `"pass": "[private]"`) {
			t.Errorf("%s: too much (or not the right thing) taken out:\n%s", name, out)
		}
	}
}
