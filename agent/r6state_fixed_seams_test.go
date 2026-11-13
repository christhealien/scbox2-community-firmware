package main

// FIXED tree: what main() runs at start for a cut-short run (tunerRecover), and a short retry for tests.

import (
	"testing"
	"time"
)

func r6StartupRecover(t *testing.T) { tunerRecover(time.Second) }
func r6FastRetry(t *testing.T) {
	tunerRecoverRetry = 10 * time.Millisecond
	t.Cleanup(func() { tunerRecoverRetry = 15 * time.Second })
}
