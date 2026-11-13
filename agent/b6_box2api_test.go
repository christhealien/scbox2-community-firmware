package main

// B6: the panel's dashboard / cgminer-API readers on the SC Box II, against the real captured intminer
// replies in R12/fixtures. Proves: the dashboard shows 1 board and exactly the fans the reply carries
// (fan0/fan1 — not a fixed 0-3), the Box II "voltage" field is treated as a plan LEVEL (never divided,
// smoothed, or fed to the power model via VoltageV), temps keep the -150/range filter, and pools +
// /api/cmd keep working. SC Lite behaviour is checked unchanged in the same file.

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// fixDir locates R12/fixtures; set B6_FIX (or R12_FIX) to override.
func b6FixDir(t *testing.T) string {
	for _, e := range []string{os.Getenv("B6_FIX"), os.Getenv("R12_FIX")} {
		if e != "" {
			return e
		}
	}
	t.Skip("set B6_FIX to the R12 fixtures dir")
	return ""
}

func b6Read(t *testing.T, n string) string {
	b, err := os.ReadFile(b6FixDir(t) + "/" + n)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

// b6Serve stands up a fake :4028 replying with the given per-command strings, points *apiAddr at it, and
// records the commands received. The default reply is a harmless STATUS-only answer (what estats gives).
func b6Serve(t *testing.T, replies map[string]string, got *[]string) func() {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			buf := make([]byte, 512)
			n, _ := c.Read(buf)
			var q map[string]string
			json.Unmarshal(buf[:n], &q)
			if got != nil {
				*got = append(*got, strings.TrimSpace(q["command"]+" "+q["parameter"]))
			}
			r, ok := replies[q["command"]]
			if !ok {
				r = `{"STATUS":[{"STATUS":"S","When":1,"Code":48,"Msg":"ok","Description":"intminer 5.4.2"}],"id":1}`
			}
			c.Write([]byte(r + "\x00"))
			c.Close()
		}
	}()
	old := *apiAddr
	*apiAddr = ln.Addr().String()
	return func() { *apiAddr = old; ln.Close(); <-done }
}

// b6WithModel swaps the active profile for the duration of a test.
func b6WithModel(t *testing.T, p modelProfile) func() {
	old := model
	model = p
	return func() { model = old }
}

func TestB6Box2Dashboard(t *testing.T) {
	defer b6WithModel(t, profSCBox2)()
	var got []string
	defer b6Serve(t, map[string]string{
		"summary": b6Read(t, "b2_summary.json"),
		"devs":    b6Read(t, "b2_devs.json"),
		"pools":   b6Read(t, "b2_pools.json"),
	}, &got)()

	d := buildDashboard0()
	if !d.OK || len(d.Boards) != 1 {
		t.Fatalf("dashboard: ok=%v boards=%d err=%q", d.OK, len(d.Boards), d.Err)
	}
	b := d.Boards[0]
	t.Logf("board: %+v", b)
	if !b.Alive || !b.Enabled || b.ClockMHz != 690 {
		t.Errorf("alive/enabled/clock: %+v", b)
	}

	// Fans come from the keys present: Box II reports fan0/fan1 only. No zero-padding to 4.
	if len(b.Fans) != 2 || b.Fans[0] != 3900 || b.Fans[1] != 3950 {
		t.Errorf("fans = %v, want [3900 3950] (fan0/fan1 only, not fixed 0-3)", b.Fans)
	}

	// "voltage":42 is a PWM plan LEVEL. It must NOT be divided (would be 0.042) and must NOT reach the
	// power model: VoltageV stays 0, the raw level is carried in VoltLevel for display.
	if b.VoltageV != 0 {
		t.Errorf("VoltageV = %v, want 0 (Box II voltage must not be smoothed/divided into the power model)", b.VoltageV)
	}
	if b.VoltLevel != 42 {
		t.Errorf("VoltLevel = %d, want 42 (raw plan level, undivided)", b.VoltLevel)
	}

	// temps: tstemp-0/1 = 61, tstemp-2 = -150 (no reading) must be filtered; 61 is the board temp.
	if mt, _, ok := maxBoardTemp(d); !ok || mt != 61 {
		t.Errorf("maxBoardTemp = %v ok=%v, want 61 (tstemp-2 -150 ignored)", mt, ok)
	}

	pools, err := readPools()
	if err != nil || len(pools) != 2 || !pools[0].Active || pools[0].User != "testwallet.worker1" || pools[0].StratumURL != "127.0.0.1" {
		t.Fatalf("pools %+v %v", pools, err)
	}
	t.Logf("commands sent: %q", got)
}

func TestB6Box2DeadBoard(t *testing.T) {
	defer b6WithModel(t, profSCBox2)()
	defer b6Serve(t, map[string]string{
		"summary": b6Read(t, "b2_summary.json"),
		"devs":    b6Read(t, "b2_devs_dead.json"),
		"pools":   b6Read(t, "b2_pools.json"),
	}, nil)()

	d := buildDashboard0()
	if !d.OK || len(d.Boards) != 1 {
		t.Fatalf("dashboard: ok=%v boards=%d", d.OK, len(d.Boards))
	}
	b := d.Boards[0]
	// A dead board still reports fan0/fan1 (as -1): the count stays 2, the -1 readings are kept (a down
	// board), not dropped, and no voltage is fed to the power model.
	if len(b.Fans) != 2 || b.Fans[0] != -1 || b.Fans[1] != -1 {
		t.Errorf("dead board fans = %v, want [-1 -1]", b.Fans)
	}
	if b.VoltageV != 0 {
		t.Errorf("dead board VoltageV = %v, want 0", b.VoltageV)
	}
	if b.Alive || b.Enabled {
		t.Errorf("dead board alive/enabled: %+v", b)
	}
	// no valid temperature on a dead board (0/0/-150): the guards' no-temperature case.
	if _, _, ok := maxBoardTemp(d); ok {
		t.Errorf("dead board: maxBoardTemp should have no reading (0/0/-150 all filtered)")
	}
}

// SC Lite must behave exactly as before: 4 fans (fan0-3) and "voltage" is a real board millivolt reading
// (divided by 1000 and smoothed into VoltageV), with VoltLevel left unset.
func TestB6SCLiteVoltageAndFansUnchanged(t *testing.T) {
	defer b6WithModel(t, profSCLite)()
	liteDevs := `{"STATUS":[{"STATUS":"S","When":1,"Code":9,"Msg":"4 PGA(s)","Description":"sclite"}],` +
		`"DEVS":[{"PGA":0,"ID":0,"Enabled":"Y","Status":"Alive","MHS av":600000.0,"clock":625.0,` +
		`"voltage":9320.0,"fan0":48,"fan1":48,"fan2":48,"fan3":48,"tstemp-0":70.0,"tstemp-1":71.0,` +
		`"tstemp-2":72.0,"overheat":0,"Nonced":4821}],"id":1}`
	liteSummary := `{"STATUS":[{"STATUS":"S","When":1,"Code":11,"Msg":"Summary","Description":"sclite"}],` +
		`"SUMMARY":[{"Elapsed":3600,"MHS av":600000.0,"Accepted":5,"Rejected":2,"Best Share":123456}],"id":1}`
	defer b6Serve(t, map[string]string{"summary": liteSummary, "devs": liteDevs}, nil)()

	d := buildDashboard0()
	if !d.OK || len(d.Boards) != 1 {
		t.Fatalf("dashboard: ok=%v boards=%d", d.OK, len(d.Boards))
	}
	b := d.Boards[0]
	if len(b.Fans) != 4 || b.Fans[0] != 48 || b.Fans[3] != 48 {
		t.Errorf("SC Lite fans = %v, want 4x48", b.Fans)
	}
	if b.VoltLevel != 0 {
		t.Errorf("SC Lite VoltLevel = %d, want 0 (field is Box II only)", b.VoltLevel)
	}
	if b.VoltageV < 9.0 || b.VoltageV > 9.4 {
		t.Errorf("SC Lite VoltageV = %v, want ~9.32 (voltage/1000, smoothed)", b.VoltageV)
	}
}

// boardFanRPMs follows the reply: present keys only, numeric order, gaps and odd counts handled.
func TestB6FanKeysFromPresent(t *testing.T) {
	cases := []struct {
		name string
		dev  map[string]any
		want []int
	}{
		{"box2 two fans", map[string]any{"fan0": 3900.0, "fan1": 3950.0}, []int{3900, 3950}},
		{"sclite four fans", map[string]any{"fan0": 48.0, "fan1": 48.0, "fan2": 48.0, "fan3": 48.0}, []int{48, 48, 48, 48}},
		{"no fans", map[string]any{"clock": 690.0}, []int{}},
		{"out of order + present -1", map[string]any{"fan1": -1.0, "fan0": -1.0}, []int{-1, -1}},
		{"double digit order", map[string]any{"fan0": 1.0, "fan2": 3.0, "fan10": 11.0}, []int{1, 3, 11}},
	}
	for _, c := range cases {
		got := boardFanRPMs(c.dev)
		if len(got) != len(c.want) {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%s: %v, want %v", c.name, got, c.want)
				break
			}
		}
	}
}

// The /api/cmd read-only proxy keeps working on Box II: an allowed command is proxied verbatim, estats is
// allowed (harmlessly invalid on both), and an unlisted command is refused.
func TestB6CmdProxy(t *testing.T) {
	defer b6WithModel(t, profSCBox2)()
	defer b6Serve(t, map[string]string{"devs": b6Read(t, "b2_devs.json")}, nil)()

	for _, cmd := range []string{"devs", "estats"} {
		if !allowedCmds[cmd] {
			t.Errorf("%q should be an allowed /api/cmd command", cmd)
		}
		rr := httptest.NewRecorder()
		handleCmd(rr, httptest.NewRequest("GET", "/api/cmd/"+cmd, nil))
		if rr.Code != 200 {
			t.Errorf("/api/cmd/%s: code %d", cmd, rr.Code)
		}
		if !strings.Contains(rr.Body.String(), `"STATUS"`) {
			t.Errorf("/api/cmd/%s: body %q", cmd, rr.Body.String())
		}
	}
	rr := httptest.NewRecorder()
	handleCmd(rr, httptest.NewRequest("GET", "/api/cmd/pgaset", nil))
	if rr.Code != http.StatusForbidden {
		t.Errorf("/api/cmd/pgaset: code %d, want 403", rr.Code)
	}
}
