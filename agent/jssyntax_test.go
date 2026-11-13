package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"
)

// The panel's script lives in a Go raw string, where a stray ' inside a '…' JS string breaks the whole
// page. With node around, check the script parses.
func TestPanelScriptParses(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node")
	}
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	ms := regexp.MustCompile(`(?s)<script>(.*?)</script>`).FindAllSubmatch(src, -1)
	if len(ms) == 0 {
		t.Fatal("no script found")
	}
	for i, m := range ms {
		f := filepath.Join(t.TempDir(), "panel.js")
		os.WriteFile(f, m[1], 0o644)
		if out, err := exec.Command(node, "--check", f).CombinedOutput(); err != nil {
			t.Fatalf("script %d doesn't parse:\n%s", i, out)
		}
	}
}
