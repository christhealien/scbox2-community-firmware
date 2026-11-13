package main

import "testing"

func TestAgentAPINoise(t *testing.T) {
	noise := []string{
		"[2026-11-01 13:37:51] API: connection from 127.0.0.1 - Accepted",
		"[2026-11-01 13:37:51] API: sent all of 412 first go",
	}
	keep := []string{
		"[2026-11-01 13:37:52] C2: nonce#(1/1) 0x724de7fd46bf30f8 ntime#0 from 30.12  OK",
		"[2026-11-01 08:03:48] Accepted 0000000f INCS 2 Diff 4.14k/4.1k",
		"[2026-11-01 08:03:48] API: send (412) failed: Broken pipe",
	}
	for _, l := range noise {
		if !agentAPINoise(l) {
			t.Errorf("should hide: %s", l)
		}
	}
	for _, l := range keep {
		if agentAPINoise(l) {
			t.Errorf("should keep: %s", l)
		}
	}
}
