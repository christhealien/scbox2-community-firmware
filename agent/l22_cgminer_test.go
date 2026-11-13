package main

// L22: the panel's client for the mining program's API (:4028), fed broken or hostile replies by a fake
// TCP server. A reply the panel can't read must never be taken as a good reading of zeros (uptime 0, no
// shares, no best share): that looks like a miner restart (notification, chart mark, best-share book
// reset, a tuner retest) and, at the next good poll, like a burst of new accepted/rejected shares.

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const l22Summary = `{"STATUS":[{"STATUS":"S","When":1793600000,"Code":11,"Msg":"Summary","Description":"intminer"}],"SUMMARY":[{"Elapsed":36000,"MHS av":4300000.00,"MHS 20s":3800000.00,"Total MH":154800000000.0,"Accepted":5,"Rejected":2,"Stale":1,"Hardware Errors":42,"Best Share":123456,"Device Hardware%":0.21}],"id":1}`

const l22Devs = `{"STATUS":[{"STATUS":"S","Msg":"1 PGA(s)"}],"DEVS":[{"PGA":0,"ID":0,"Enabled":"Y","Status":"Alive","MHS av":1150000.0,"MHS 20s":1100000.0,"Nonced":4821,"Hardware Errors":9,"Device Hardware%":0.02,"voltage":9300.0,"clock":600.0,"overheat":0,"tstemp-0":61.0,"tstemp-1":63.0,"tstemp-2":64.0,"fan0":48,"fan1":48,"fan2":48,"fan3":48},{"PGA":1,"ID":1,"Enabled":"Y","Status":"Alive","MHS av":1150000.0,"MHS 20s":1100000.0,"Nonced":4821,"Hardware Errors":9,"Device Hardware%":0.02,"voltage":9300.0,"clock":600.0,"overheat":0,"tstemp-0":60.0,"tstemp-1":62.0,"tstemp-2":63.0,"fan0":48,"fan1":48,"fan2":48,"fan3":48}],"id":1}`

// l22Fake: a fake miner API on addr; reply(cmd, conn) answers one request (and closes, unless it hangs).
func l22Fake(t *testing.T, addr string, reply func(cmd string, c *net.TCPConn)) func() {
	t.Helper()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				close(done)
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 512)
				n, _ := c.Read(buf)
				var q struct {
					Command string `json:"command"`
				}
				json.Unmarshal(buf[:n], &q)
				reply(q.Command, c.(*net.TCPConn))
			}(c)
		}
	}()
	oldA, oldT := *apiAddr, *apiTimeout
	*apiAddr, *apiTimeout = addr, 1500*time.Millisecond
	return func() { *apiAddr, *apiTimeout = oldA, oldT; ln.Close(); <-done }
}

func l22Reset(c *net.TCPConn) { c.SetLinger(0) } // the deferred Close then sends a RST

// A summary reply the panel can't read (cut off by a connection reset, a number sent as a string, a
// "nan" a C printf makes of 0/0) gave an OK dashboard with uptime 0 and no shares.
func TestL22UnreadableSummaryIsNotAZeroReading(t *testing.T) {
	cases := map[string]func(c *net.TCPConn){
		"cut off by a reset": func(c *net.TCPConn) {
			c.Write([]byte(l22Summary[:180]))
			time.Sleep(50 * time.Millisecond)
			l22Reset(c)
		},
		"cut off by the timeout": func(c *net.TCPConn) {
			c.Write([]byte(l22Summary[:180]))
			time.Sleep(3 * time.Second)
		},
		"number as a string": func(c *net.TCPConn) {
			c.Write([]byte(strings.Replace(l22Summary, `"Elapsed":36000`, `"Elapsed":"36000"`, 1) + "\x00"))
		},
		"nan": func(c *net.TCPConn) {
			c.Write([]byte(strings.Replace(l22Summary, `"Device Hardware%":0.21`, `"Device Hardware%":nan`, 1) + "\x00"))
		},
		"NUL then newline": func(c *net.TCPConn) {
			c.Write([]byte(l22Summary + "\x00\n"))
		},
	}
	for name, bad := range cases {
		t.Run(name, func(t *testing.T) {
			stop := l22Fake(t, "127.0.0.1:47420", func(cmd string, c *net.TCPConn) {
				switch cmd {
				case "summary":
					bad(c)
				case "devs":
					c.Write([]byte(l22Devs + "\x00"))
				}
			})
			defer stop()
			d := buildDashboard0()
			if d.OK && (d.Elapsed != 36000 || d.Accepted != 5 || d.Rejected != 2 || d.BestShare != 123456) {
				t.Fatalf("an unreadable summary reply was taken as a reading: ok=%v elapsed=%d accepted=%d rejected=%d best=%v",
					d.OK, d.Elapsed, d.Accepted, d.Rejected, d.BestShare)
			}
		})
	}
}

// One odd field in one board's entry (a number as a string, or a nan) dropped every board from the
// dashboard while it stayed OK: no temperatures, fans or clocks for the guards and the Live page.
func TestL22OneOddBoardFieldKeepsTheBoards(t *testing.T) {
	for name, devs := range map[string]string{
		"string": strings.Replace(l22Devs, `"Nonced":4821`, `"Nonced":"4821"`, 1),
		"nan":    strings.Replace(l22Devs, `"Device Hardware%":0.02`, `"Device Hardware%":-nan`, 1),
		"}{":     strings.Replace(l22Devs, `},{"PGA":1`, `}{"PGA":1`, 1),
		// past 2^31: Go's int is 32 bits on the miner's ARM (fails there only)
		"uint32 counter": strings.Replace(l22Devs, `"Nonced":4821`, `"Nonced":3000000000`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			stop := l22Fake(t, "127.0.0.1:47421", func(cmd string, c *net.TCPConn) {
				switch cmd {
				case "summary":
					c.Write([]byte(l22Summary + "\x00"))
				case "devs":
					c.Write([]byte(devs + "\x00"))
				}
			})
			defer stop()
			d := buildDashboard0()
			if !d.OK || len(d.Boards) != 2 || d.Boards[1].Temps[2] != 63 || d.Boards[0].ClockMHz != 600 {
				t.Fatalf("boards lost: ok=%v boards=%+v", d.OK, d.Boards)
			}
		})
	}
}

// The reply ends with a NUL; a program that then leaves the connection open cost every read the full
// API timeout (the pgaset client already stops at the NUL).
func TestL22NulTerminatedReplyWithoutClose(t *testing.T) {
	stop := l22Fake(t, "127.0.0.1:47422", func(cmd string, c *net.TCPConn) {
		c.Write([]byte(l22Summary + "\x00"))
		time.Sleep(3 * time.Second)
	})
	defer stop()
	t0 := time.Now()
	raw, err := cgminerCmd("summary")
	if err != nil || !strings.HasSuffix(string(raw), `"id":1}`) {
		t.Fatalf("%v %q", err, raw)
	}
	if el := time.Since(t0); el > 700*time.Millisecond {
		t.Fatalf("waited %v for a reply that was complete at once", el)
	}
}

// A runaway reply was read into memory without a limit (the panel runs on a 256 MB board).
func TestL22HugeReplyIsCapped(t *testing.T) {
	stop := l22Fake(t, "127.0.0.1:47423", func(cmd string, c *net.TCPConn) {
		chunk := []byte(strings.Repeat(" ", 1<<20))
		c.Write([]byte(`{"STATUS":[{"STATUS":"S"}],"x":"`))
		for i := 0; i < 48; i++ {
			if _, err := c.Write(chunk); err != nil {
				return
			}
		}
		c.Write([]byte(`"}`))
	})
	defer stop()
	raw, err := cgminerCmd("stats")
	if err == nil || len(raw) > 8<<20 {
		t.Fatalf("a 48 MB reply was accepted: %d bytes, err %v", len(raw), err)
	}
	raw, err = cgminerCmdParam("pgaset", "0,clock,600")
	if len(raw) > 8<<20 {
		t.Fatalf("pgaset: a 48 MB reply was read whole: %d bytes, err %v", len(raw), err)
	}
}

// The read-only pass-through answered a cut-off reply with 200 and half a JSON document.
func TestL22PassThroughCutOffReply(t *testing.T) {
	stop := l22Fake(t, "127.0.0.1:47424", func(cmd string, c *net.TCPConn) {
		c.Write([]byte(l22Summary[:100]))
		time.Sleep(50 * time.Millisecond)
		l22Reset(c)
	})
	defer stop()
	raw, err := cgminerCmd("summary")
	if err == nil {
		t.Fatalf("a reply cut off by a connection reset was returned as the answer: %q", raw)
	}
}

// The pool guard reads the miner's pools. One field it can't decode (a nan, a number as a string) made
// the whole reply unreadable and the guard skipped every check: a pool nobody entered kept running.
func TestL22PoolGuardReadsAnOddPoolsReply(t *testing.T) {
	dir := t.TempDir()
	oldB, oldA := *bfgPath, *authFile
	*bfgPath = filepath.Join(dir, "bfgminer", "bfgminer.json")
	*authFile = filepath.Join(dir, "sclite", "auth.json")
	os.MkdirAll(filepath.Dir(*authFile), 0o755)
	oldL, oldP, oldR, oldI, oldIdle := pgListPools, pgLivePools, pgRemove, pgIdle, minerIdleForGuard
	defer func() {
		*bfgPath, *authFile = oldB, oldA
		pgListPools, pgLivePools, pgRemove, pgIdle, minerIdleForGuard = oldL, oldP, oldR, oldI, oldIdle
	}()
	noteMinerChange("test")
	pools := `{"STATUS":[{"STATUS":"S","Msg":"2 Pool(s)"}],"POOLS":[` +
		`{"POOL":0,"URL":"stratum+tcp://mine.example.org:3333","Status":"Alive","Priority":0,"Accepted":10,"Rejected":0,"Stale":0,"User":"owner.w1","Stratum Active":false,"Stratum URL":"","Pool Rejected%":0.0},` +
		`{"POOL":1,"URL":"stratum+tcp://other.example.net:3333","Status":"Alive","Priority":"1","Accepted":7,"Rejected":0,"Stale":0,"User":"someone.else","Stratum Active":true,"Stratum URL":"other.example.net","Pool Rejected%":nan}],"id":1}`
	stop := l22Fake(t, "127.0.0.1:47425", func(cmd string, c *net.TCPConn) { c.Write([]byte(pools + "\x00")) })
	defer stop()
	removed, idled := []int{}, 0
	pgListPools = func() ([]map[string]any, error) {
		return []map[string]any{{"url": "stratum+tcp://mine.example.org:3333", "user": "owner.w1"}}, nil
	}
	pgLivePools = readPools
	pgRemove = func(n int) error { removed = append(removed, n); return nil }
	pgIdle = func() error { idled++; return nil }
	minerIdleForGuard = func() bool { return false }
	pgStrikes, pgActed, pgIncidents = map[string]int{}, map[string]int{}, nil
	for i := 0; i < 3; i++ {
		poolGuardCheck()
	}
	if len(removed) == 0 && idled == 0 {
		_, err := readPools()
		t.Fatalf("a pool nobody entered runs and the guard did nothing (readPools: %v)", err)
	}
	if len(removed) > 0 && removed[0] != 1 {
		t.Fatalf("removed pool %d, not the foreign one (1)", removed[0])
	}
}
