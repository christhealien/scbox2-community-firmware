package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// v1.5.24: "Fit to my fans" must change only the maxrpm numbers in the OEM's product.json and leave every
// other byte alone. The old code re-encoded the JSON (json.Marshal of the parsed document), which turned
// "tempcutoff":92.0 into 92; the OEM's fan control reads tempcutoff with a real-valued JSON call that
// returns 0 for an integer and then uses its 95 °C default, so a fit silently raised the thermal cut-off
// above stock. These tests pin the surgical behaviour and the boot-time guard.

const r24StockProduct = `{
	"model":"` + stockModelSCLite + `",
	"tempcutoff": 92.0,
	"temp_target": 85.0,
	"fans": [
		{ "id":0, "name":"fan0", "rpm":40, "maxrpm":2200 },
		{ "id":1, "name":"fan1", "rpm":53, "maxrpm":2200 },
		{ "id":2, "name":"fan2", "rpm":41, "maxrpm":2200 },
		{ "id":3, "name":"fan3", "rpm":50, "maxrpm":2200 }
	]
}
`

func TestR24ReplaceMaxRPMKeepsEverythingElse(t *testing.T) {
	src := []byte(r24StockProduct)
	out, err := replaceMaxRPM(src, []int{1700, 1600, 1700, 1600})
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	// the thermal fields are byte-for-byte unchanged (floats keep their ".0")
	if !strings.Contains(s, `"tempcutoff": 92.0`) {
		t.Errorf("tempcutoff was rewritten: %s", firstLineWith(s, "tempcutoff"))
	}
	if !strings.Contains(s, `"temp_target": 85.0`) {
		t.Errorf("temp_target was rewritten: %s", firstLineWith(s, "temp_target"))
	}
	// the maxrpm values are the fitted ones
	if n := strings.Count(s, "2200"); n != 0 {
		t.Errorf("a stock maxrpm survived: %d", n)
	}
	for _, want := range []string{`"maxrpm":1700`, `"maxrpm":1600`} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q", want)
		}
	}
	// and the OEM's fan control (its real-valued read) still sees 92, not 0 -> 95
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["tempcutoff"].(float64) != 92 || doc["temp_target"].(float64) != 85 {
		t.Fatalf("thermal fields changed: %v / %v", doc["tempcutoff"], doc["temp_target"])
	}
	// only the maxrpm digits differ
	if err := sameExceptMaxRPM(src, out); err != nil {
		t.Fatal(err)
	}
}

func TestR24ReplaceMaxRPMCountMustMatch(t *testing.T) {
	if _, err := replaceMaxRPM([]byte(r24StockProduct), []int{1700, 1600}); err == nil {
		t.Fatal("2 figures for 4 fans was accepted")
	}
}

// the exact regression: the old re-encode path produced an integer cut-off; the boot guard must reject it.
func TestR24OverlayGuardRejectsLostCutoff(t *testing.T) {
	d := t.TempDir()
	stock := filepath.Join(d, "stock.json")
	os.WriteFile(stock, []byte(r24StockProduct), 0o644)
	oldAuth := *authFile
	*authFile = filepath.Join(d, "auth.json")
	defer func() { *authFile = oldAuth }()
	// fanStockCopy() lives next to authFile; put the stock copy there for the comparison branch
	os.WriteFile(fanStockCopy(), []byte(r24StockProduct), 0o644)

	// what the old buggy code wrote: re-encoded, so 92.0 -> 92 and 85.0 -> 85
	var doc map[string]any
	json.Unmarshal([]byte(r24StockProduct), &doc)
	doc["fans"].([]any)[0].(map[string]any)["maxrpm"] = 1700
	bad, _ := json.MarshalIndent(doc, "", "\t")
	if strings.Contains(string(bad), "92.0") {
		t.Fatal("test setup: expected the re-encode to drop 92.0")
	}
	badPath := filepath.Join(d, "bad.json")
	os.WriteFile(badPath, bad, 0o644)
	if err := productOverlayOK(badPath); err == nil {
		t.Fatal("the boot guard accepted an overlay whose cut-off was lost to re-encoding")
	}

	// a good overlay (surgical) passes
	good, _ := replaceMaxRPM([]byte(r24StockProduct), []int{1700, 1600, 1700, 1600})
	goodPath := filepath.Join(d, "good.json")
	os.WriteFile(goodPath, good, 0o644)
	if err := productOverlayOK(goodPath); err != nil {
		t.Fatalf("a correctly fitted overlay was rejected: %v", err)
	}
}

func firstLineWith(s, sub string) string {
	for _, ln := range strings.Split(s, "\n") {
		if strings.Contains(ln, sub) {
			return strings.TrimSpace(ln)
		}
	}
	return "(not found)"
}
