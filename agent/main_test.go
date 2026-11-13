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
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
