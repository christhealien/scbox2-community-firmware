package main

import (
	"os"
	"path/filepath"
	"testing"
)

// Every test writes the panel's files under a temporary folder, never /usr/config.
func TestMain(m *testing.M) {
	dir, _ := os.MkdirTemp("", "sclite-test-")
	*authFile = filepath.Join(dir, "auth.json")
	// the SC Box II's clock confirmation waits 30 s before its first miner-API poll (box2ConfirmQuiet): the tests
	// of the confirmation itself set it; everything else confirms at once
	box2ConfirmQuiet = 0
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
