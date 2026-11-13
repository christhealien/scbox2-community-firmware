package main

import (
	"strings"
	"testing"
)

// fakeTGToken: a made-up Telegram bot token for the redaction tests, built at run time so secret scanners
// don't take this source for a leaked one.
var fakeTGToken = "1234567890" + ":" + "AAH9v8u7t6s5r4q3p2o1n0m9l8k7j6i5h4g"

func TestRedactLog(t *testing.T) {
	in := `[2026-11-02 13:00:01] Pool 0 stratum+tcp://192.0.2.235:23334 alive, user bc1qexamplewalletaddress000000000000000000.SCLite_2
eth0: link up, HWaddr 02:1a:2b:3c:4d:5e, ip 10.0.0.7, api 127.0.0.1:4028
notify: discord https://discord.com/api/webhooks/123456789/AbCdEfGhIjKlMnOpQrStUvWxYz0123456789 failed
telegram bot@TG@ refused
mining at sc.pool2.example:3334 as SCLite_fwtest1`
	in = strings.ReplaceAll(in, "@TG@", fakeTGToken)
	out := redactLog(in, []string{"bc1qexamplewalletaddress000000000000000000.SCLite_2", "bc1qexamplewalletaddress000000000000000000", "sc.pool2.example", "SCLite_fwtest1"})
	for _, bad := range []string{"bc1qexample", "SCLite_2", "192.168", "10.0.0.7", "02:1a", "AbCdEf", "AAH9v8", "sc.pool2", "fwtest1"} {
		if strings.Contains(out, bad) {
			t.Fatalf("%q left in:\n%s", bad, out)
		}
	}
	if !strings.Contains(out, "127.0.0.1:4028") || !strings.Contains(out, "alive") {
		t.Fatalf("too much taken out:\n%s", out)
	}
}
