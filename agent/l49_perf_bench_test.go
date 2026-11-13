package main

// L49 (perf): benchmarks for the panel's per-tick work on the miner's ARM CPU.
// Run on the host:   go test -run '^$' -bench L49 -benchmem .
// Run for ARM:       GOARCH=arm GOARM=7 go test -c -o l49.arm . && qemu-arm-static ./l49.arm -test.run '^$' -test.bench L49

import (
	"encoding/json"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func l49Data(b testing.TB, name string) []byte {
	raw, err := os.ReadFile(filepath.Join("testdata", "l49", name))
	if err != nil {
		b.Skip("no ", name)
	}
	return raw
}

// l49FakeAPI answers summary/devs with the real replies captured from an SC Lite (testdata/l49).
func l49FakeAPI(b testing.TB) func() {
	sum, devs := l49Data(b, "api_summary.json"), l49Data(b, "api_devs.json")
	// compact them, as the miner sends them, NUL-ended
	var v any
	json.Unmarshal(sum, &v)
	sum, _ = json.Marshal(v)
	json.Unmarshal(devs, &v)
	devs, _ = json.Marshal(v)
	ln, err := net.Listen("tcp", "127.0.0.1:48030") // (L49's port range)
	if err != nil {
		ln, err = net.Listen("tcp", "127.0.0.1:0")
	}
	if err != nil {
		b.Skip(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				var in struct{ Command string }
				json.NewDecoder(c).Decode(&in)
				out := sum
				if in.Command == "devs" {
					out = devs
				}
				c.Write(append(append([]byte(nil), out...), 0))
			}(c)
		}
	}()
	old, oldB := *apiAddr, backupRoot
	*apiAddr = ln.Addr().String()
	d := b.(interface{ TempDir() string }).TempDir()
	backupRoot = d
	os.MkdirAll(filepath.Join(d, "xminerd/trash"), 0o755)
	if w, err := os.ReadFile(filepath.Join("testdata", "l49", "xminerdlog16k")); err == nil {
		os.WriteFile(filepath.Join(d, "xminerd/trash/xminerdlog"), w, 0o644)
	}
	return func() { ln.Close(); *apiAddr, backupRoot = old, oldB }
}

// the parsing buildDashboard0 does for one summary + one devs reply, as v1.5.22 does it
func l49ParseOld(sRaw, vRaw []byte) float64 {
	var s float64
	if x, ok := apiEntry(sRaw, "SUMMARY", 0); ok {
		s += apiNum(x, "MHS av")
	}
	s += currentMHS(sRaw, "SUMMARY", 0)
	if w, ok := workOf(sRaw, "SUMMARY", 0); ok {
		s += w
	}
	if devs, ok := apiEntries(vRaw, "DEVS"); ok {
		for i, dev := range devs {
			s += apiNum(dev, "MHS av") + currentMHS(vRaw, "DEVS", i)
			if w, ok := workOf(vRaw, "DEVS", i); ok {
				s += w
			}
		}
	}
	return s
}

func l49Compact(b testing.TB, name string) []byte {
	var v any
	json.Unmarshal(l49Data(b, name), &v)
	out, _ := json.Marshal(v)
	return out
}

func BenchmarkL49ParseDashOld(b *testing.B) {
	sRaw, vRaw := l49Compact(b, "api_summary.json"), l49Compact(b, "api_devs.json")
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		l49ParseOld(sRaw, vRaw)
	}
}

func BenchmarkL49BuildDashboard(b *testing.B) {
	defer l49FakeAPI(b)()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if d := buildDashboard(); !d.OK {
			b.Fatal("not OK: ", d.Err)
		}
	}
}

func BenchmarkL49HandleDashboard(b *testing.B) {
	defer l49FakeAPI(b)()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		w := httptest.NewRecorder()
		handleDashboard(w, httptest.NewRequest("GET", "/api/dashboard", nil))
	}
}

func BenchmarkL49ParseWatchdog(b *testing.B) {
	t := string(l49Data(b, "xminerdlog16k"))
	now, _ := time.ParseInLocation("2006-01-02 15:04:05", "2026-11-04 01:31:40", time.Local)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		parseWatchdog(t, now)
	}
}

func BenchmarkL49PBKDF2Blake2b20k(b *testing.B) {
	salt := make([]byte, 16)
	for i := 0; i < b.N; i++ {
		pbkdf2With(blake2b512, []byte("correct horse"), salt, b2Iter, 64)
	}
}

func BenchmarkL49PBKDF2SHA256x60k(b *testing.B) {
	salt := make([]byte, 16)
	for i := 0; i < b.N; i++ {
		pbkdf2([]byte("correct horse"), salt, pbkdfIter, 32)
	}
}

// nextRestart's worst valid cases: a 3-monthly restart just missed (~92 days of minutes), a monthly one.
func BenchmarkL49NextRestart3Months(b *testing.B) {
	anc := 2026*12 + 10 // November
	r := schedRestart{Every: "3months", At: "03:00", Mday: 1, Anchor: &anc}
	from := time.Date(2026, 11, 1, 3, 1, 0, 0, time.Local)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, ok := nextRestart(r, from); !ok {
			b.Fatal("none")
		}
	}
}

func BenchmarkL49NextRestartMonth(b *testing.B) {
	r := schedRestart{Every: "month", At: "03:00", Mday: 1}
	from := time.Date(2026, 11, 1, 3, 1, 0, 0, time.Local)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		nextRestart(r, from)
	}
}

// for instruction counts under qemu (-d in_asm,exec,nochain): L49_ITER rounds of one PRF
func BenchmarkL49Rounds(b *testing.B) {
	n := 1
	if s := os.Getenv("L49_ITER"); s != "" {
		json.Unmarshal([]byte(s), &n)
	}
	salt := make([]byte, 16)
	for i := 0; i < b.N; i++ {
		if os.Getenv("L49_ALGO") == "b2" {
			pbkdf2With(blake2b512, []byte("correct horse"), salt, n, 64)
		} else {
			pbkdf2([]byte("correct horse"), salt, n, 32)
		}
	}
}
