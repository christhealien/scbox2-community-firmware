package main

import (
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// hand trims run −100 … +50: past that a trim is refused by the panel and dropped from a file
func TestChipTrimLimits(t *testing.T) {
	old := *authFile
	*authFile = t.TempDir() + "/auth.json"
	defer func() { *authFile = old }()
	os.WriteFile(chipTrimPath(), []byte("2 7 62.5\n2 8 -25\n1 3 NaN\n1 4 +Inf\n1 5 25\n0 1 -112.5\n"), 0o644)
	ts := readChipTrims()
	if len(ts) != 2 || ts[0].Chip != 5 || ts[0].MHz != 25 || ts[1].Chip != 8 || ts[1].MHz != -25 {
		t.Fatalf("read %+v", ts)
	}
	rr := httptest.NewRecorder()
	handleChipTrim(rr, httptest.NewRequest("POST", "/api/chiptrim", strings.NewReader(`{"chain":2,"chip":7,"mhz":62.5}`)))
	if !strings.Contains(rr.Body.String(), `"ok":false`) {
		t.Fatalf("+62.5 accepted: %s", rr.Body.String())
	}
}
