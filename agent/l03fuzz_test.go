package main

import (
	"encoding/json"
	"math"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func l03Seed(f *testing.F, extra ...string) {
	for _, s := range append(append([]string{}, l03Hostile[:len(l03Hostile)-0]...), extra...) {
		if len(s) < 4096 {
			f.Add([]byte(s))
		}
	}
}

// l03FT: the l03 environment for a fuzz iteration (testing.T only; the fuzz engine's T)
func l03Post(t *testing.T, path string, body []byte) map[string]any {
	rr := l03Req(l03Mux(), "POST", path, string(body), issueCookie("zq7owner"), map[string]string{"Content-Type": "application/json"})
	if rr.Code >= 500 && rr.Code != 502 && rr.Code != 503 {
		t.Fatalf("%s %q: %d %s", path, body, rr.Code, rr.Body.String())
	}
	var m map[string]any
	json.Unmarshal(rr.Body.Bytes(), &m)
	return m
}

func FuzzL03Pools(f *testing.F) {
	l03Seed(f,
		`{"action":"add","url":"pool.three:3333","user":"w","confirm":true}`,
		`{"action":"add","url":"stratum+tcp://pool.one:3333","user":"wallet1.w","confirm":true}`,
		`{"action":"edit","old_url":"stratum+tcp://pool.two:3333","old_user":"wallet2.w","url":"stratum+ssl://x.y:1","user":"u","confirm":true}`,
		`{"action":"switch","old_url":"STRATUM+TCP://POOL.TWO:3333/","old_user":" wallet2.w ","confirm":true}`,
		`{"action":"delete","old_url":"stratum+tcp://pool.one:3333","old_user":"wallet1.w","confirm":true}`,
		`{"action":"edit","index":1,"url":"x","confirm":true}`, `{"action":"switch","index":1,"confirm":false}`,
		`{"action":"edit","old_url":"stratum+tcp://pool.one:3333","old_user":"wallet1.w","url":"stratum+tcp://a\nb:1","confirm":true}`)
	f.Fuzz(func(t *testing.T, body []byte) {
		l03Env(t, false)
		st := l03Backend(t)
		var in struct {
			Confirm bool   `json:"confirm"`
			Action  string `json:"action"`
		}
		decErr := json.NewDecoder(strings.NewReader(string(body))).Decode(&in)
		before := make([]map[string]any, len(st.pools))
		for i, p := range st.pools {
			before[i] = map[string]any{}
			for k, v := range p {
				before[i][k] = v
			}
		}
		res := l03Post(t, "/api/pools", body)
		puts := st.takePuts()
		if (decErr != nil || !in.Confirm) && len(puts) > 0 {
			t.Fatalf("%q (decode %v, confirm %v) reached the backend: %+v", body, decErr, in.Confirm, puts)
		}
		if res["ok"] != true && len(puts) > 0 {
			t.Fatalf("%q refused (%v) but wrote %d time(s)", body, res["err"], len(puts))
		}
		l03CheckPuts(t, string(body), puts)
		for _, p := range puts {
			if p.path != "/mcb/pools" {
				continue
			}
			var arr []map[string]any
			json.Unmarshal(p.body, &arr)
			switch in.Action {
			case "switch": // same pools, another order
				n := 0
				for _, a := range arr {
					for _, b := range before {
						if reflect.DeepEqual(a, b) {
							n++
							break
						}
					}
				}
				if n != len(before) {
					t.Fatalf("switch changed a pool: %s", p.body)
				}
			case "edit": // exactly one pool changed
				d := 0
				for i := range arr {
					if !reflect.DeepEqual(arr[i], before[i]) {
						d++
					}
				}
				if d > 1 {
					t.Fatalf("edit changed %d pools: %s", d, p.body)
				}
			}
		}
	})
}

func FuzzL03Settings(f *testing.F) {
	l03Seed(f,
		`{"hot_c":82,"hash_low_pct":70,"min_interval_min":30,"ntfy_url":"http://1.2.3.4","webhook_url":"https://x"}`,
		`{"hot_c":49.999,"min_interval_min":1441}`, `{"hot_c":90.0001}`, `{"ntfy_url":"ftp://x"}`, `{"webhook_url":" javascript:alert(1)"}`,
		`{"discord_url":"file:///etc/passwd","discord_on":true}`, `{"min_interval_min":0,"hash_low_pct":99}`)
	f.Fuzz(func(t *testing.T, body []byte) {
		dir := l03Env(t, false)
		notifyMu.Lock()
		old := notifyPath
		notifyPath = filepath.Join(dir, "notify.json")
		notifyCfg = defaultNotifyConfig()
		before := notifyCfg
		notifyMu.Unlock()
		t.Cleanup(func() { notifyMu.Lock(); notifyPath = old; notifyMu.Unlock() })
		res := l03Post(t, "/api/settings", body)
		notifyMu.Lock()
		c := notifyCfg
		notifyMu.Unlock()
		if res["ok"] != true {
			if !reflect.DeepEqual(c, before) {
				t.Fatalf("refused %q but the settings changed", body)
			}
			if e, _ := res["err"].(string); strings.Contains(e, "Go ") || strings.Contains(e, "json:") {
				t.Errorf("raw decoder error shown: %q", e)
			}
			return
		}
		if c.HotC != 0 && (c.HotC < 50 || c.HotC > 90) || math.IsNaN(c.HotC) {
			t.Fatalf("saved hot_c %v", c.HotC)
		}
		if c.HashLowPct < 0 || c.HashLowPct > 99 || c.MinIntervalMin < 1 || c.MinIntervalMin > 1440 {
			t.Fatalf("saved %+v", c)
		}
		for _, u := range []string{c.NtfyURL, c.GotifyURL, c.WebhookURL, c.DiscordURL} {
			if u != "" && !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
				t.Fatalf("saved a server address that isn't http(s): %q", u)
			}
		}
	})
}

func FuzzL03Overclock(f *testing.F) {
	l03Seed(f, `{"acknowledge":true}`, `{"revoke":true}`, `{"caps":{"freq":850}}`, `{"caps":{"freq":1}}`, `{"caps":{"pv":99999,"freq":2000}}`,
		`{"acknowledge":true,"caps":{"freq":300}}`)
	f.Fuzz(func(t *testing.T, body []byte) {
		l03Env(t, false)
		saveOverclock(overclockState{Acknowledged: true})
		l03Post(t, "/api/overclock", body)
		oc := loadOverclock()
		if oc.Acknowledged {
			if hi := clockWindowHi(); hi < freqMax || hi > freqHardMax {
				t.Fatalf("%q: the unlocked clock window ends at %d MHz", body, hi)
			}
		}
		if p := effHard("pv", pvHardMax); p < pvMax || p > 9600 {
			t.Fatalf("%q: PV ceiling %d", body, p)
		}
		if v := effHard("vrun", vrunHardMax); v < vrunMax || v > vrunHardMax {
			t.Fatalf("%q: run-voltage ceiling %d", body, v)
		}
	})
}

// the small switches: whatever the body, the saved state is one of the allowed values
func FuzzL03SmallSwitches(f *testing.F) {
	l03Seed(f, `{"mode":"fixed","mac":"02:00:00:00:00:01"}`, `{"mode":"fixed","mac":"03:00:00:00:00:01"}`, `{"mode":"fixed","mac":"02-00-00-00-00-01\n"}`,
		`{"hash_unit":"ths"}`, `{"share_unit":"miner"}`, `{"mains_v":120}`, `{"mains_v":89.99}`, `{"cal_watts":49}`, `{"on":false}`, `{"keep_log":true}`)
	f.Fuzz(func(t *testing.T, body []byte) {
		dir := l03Env(t, false)
		for _, p := range []string{"/api/mac", "/api/display", "/api/power", "/api/boardauto", "/api/poolguard"} {
			l03Post(t, p, body)
		}
		mode := readTrim(filepath.Join(dir, "macmode"))
		if mode != "" && mode != "keep" && mode != "random" && mode != "fixed" {
			t.Fatalf("macmode %q", mode)
		}
		if mode == "fixed" {
			if m := readTrim(filepath.Join(dir, "mac")); !validMAC(m) {
				t.Fatalf("saved MAC %q", m)
			}
		}
		if !validShareUnit(getShareUnit()) || !validHashUnit(getHashUnit()) {
			t.Fatalf("units %q %q", getShareUnit(), getHashUnit())
		}
		if c := getPowerConfig(); c.MainsV != 0 && (c.MainsV < 90 || c.MainsV > 260) {
			t.Fatalf("mains %v", c.MainsV)
		}
	})
}

var _ = httptest.NewRecorder
