package main

// L49 (perf): the panel's per-request and per-tick work on the miner's ARM CPU.

import (
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

// GET /api/schedule with a 3-monthly restart just past scanned ~132,000 minutes with an fmt.Sscanf each:
// 134 ms on a 2.1 GHz Xeon, ~3.5 s under qemu-arm, per request (the Schedule tab asks every 30 s).
func TestL49NextRestartCheap(t *testing.T) {
	anc := 2026*12 + 10
	r := schedRestart{Every: "3months", At: "03:00", Mday: 1, Anchor: &anc}
	from := time.Date(2026, 11, 1, 3, 1, 0, 0, time.Local)
	var got time.Time
	allocs := testing.AllocsPerRun(3, func() { got, _ = nextRestart(r, from) })
	if want := time.Date(2027, 2, 1, 3, 0, 0, 0, time.Local); !got.Equal(want) {
		t.Fatalf("next restart %v, want %v", got, want)
	}
	if allocs > 50 {
		t.Fatalf("nextRestart made %.0f allocations for one answer (a parse of r.At per minute scanned)", allocs)
	}
}

// the v1.5.22 restartDue, as the reference
func l49OldRestartDue(r schedRestart, t time.Time) bool {
	mow := minuteOfWeek(t)
	for _, m := range r.Marks {
		if m == mow {
			return true
		}
	}
	at := parseHM(r.At)
	if r.Every == "off" || r.Every == "" || at < 0 {
		return false
	}
	hh, mm := at/60, at%60
	h, m := t.Hour(), t.Minute()
	switch r.Every {
	case "hour":
		return m == mm
	case "6h", "12h":
		st := 6
		if r.Every == "12h" {
			st = 12
		}
		return m == mm && ((h-hh)%st+st)%st == 0
	case "day":
		return h == hh && m == mm
	case "week":
		return (int(t.Weekday())+6)%7 == r.Day && h == hh && m == mm
	case "month", "3months":
		if t.Day() != r.Mday || h != hh || m != mm {
			return false
		}
		if r.Every == "3months" && r.Anchor != nil {
			mon := t.Year()*12 + int(t.Month()) - 1
			return ((mon-*r.Anchor)%3+3)%3 == 0
		}
		return true
	}
	return false
}

// the faster restartDue answers exactly as before, minute by minute, across a DST change, for every kind
func TestL49RestartDueSame(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skip(err)
	}
	anc := 2026*12 + 9
	rs := []schedRestart{
		{Every: "hour", At: "00:17"}, {Every: "6h", At: "05:30"}, {Every: "12h", At: "23:00"}, {Every: "day", At: "02:30"},
		{Every: "week", At: "04:00", Day: 6}, {Every: "month", At: "03:00", Mday: 25}, {Every: "3months", At: "02:30", Mday: 26, Anchor: &anc},
		{Every: "off", Marks: []int{2*1440 + 15*60, 6*1440 + 150}}, {Every: "day", At: "01:00", Marks: []int{60}},
		{Every: "day", At: "bad"}, {Every: "nonsense", At: "01:00"}, {Every: "", At: "01:00"},
	}
	for _, r := range rs {
		at := parseHM(r.At)
		for tm := time.Date(2026, 10, 20, 0, 0, 0, 0, loc); tm.Before(time.Date(2026, 11, 5, 0, 0, 0, 0, loc)); tm = tm.Add(time.Minute) {
			if a, b := restartDue(r, tm), l49OldRestartDue(r, tm); a != b || restartDueAt(r, tm, at) != b {
				t.Fatalf("%+v at %v: %v, before %v", r, tm, a, b)
			}
		}
	}
}

// buildDashboard parsed the summary reply 3 times and the devs reply 1 + 2 per board (9 for 4 boards), every
// 2 s and for every page poll; once each now.
func TestL49DashboardParsesOnce(t *testing.T) {
	defer l49FakeAPI(t)()
	if d := buildDashboard(); !d.OK || len(d.Boards) != 4 {
		t.Fatalf("dashboard %+v", d)
	}
	allocs := testing.AllocsPerRun(5, func() { buildDashboard() })
	if allocs > 2500 { // v1.5.22: ~5,500 with the real 4-board reply
		t.Fatalf("buildDashboard made %.0f allocations (the API replies decoded again per board)", allocs)
	}
}

// the entry-based readers give what the reply-based ones give, for every entry of the real replies
func TestL49EntryReadersSame(t *testing.T) {
	for _, c := range []struct{ file, sec string }{{"api_summary.json", "SUMMARY"}, {"api_devs.json", "DEVS"}} {
		raw := l49Compact(t, c.file)
		es, ok := apiEntries(raw, c.sec)
		if !ok || len(es) == 0 {
			t.Fatal("no entries in ", c.file)
		}
		for i, e := range es {
			w1, ok1 := workOf(raw, c.sec, i)
			w2, ok2 := workOfEntry(e, c.sec)
			if w1 != w2 || ok1 != ok2 || currentMHS(raw, c.sec, i) != currentMHSOf(e) {
				t.Fatalf("%s[%d]: work %v/%v %v/%v, mhs %v/%v", c.sec, i, w1, w2, ok1, ok2, currentMHS(raw, c.sec, i), currentMHSOf(e))
			}
		}
	}
}

// /api/dashboard encoded the dashboard, decoded it into a map and encoded that again; once now, same JSON.
func TestL49HandleDashboardOneEncode(t *testing.T) {
	defer l49FakeAPI(t)()
	get := func() map[string]any {
		w := httptest.NewRecorder()
		handleDashboard(w, httptest.NewRequest("GET", "/api/dashboard", nil))
		var m map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	got := get()
	// the v1.5.22 way, from the same dashboard
	d := buildDashboard()
	d.Build = pageBuild
	b, _ := json.Marshal(d)
	var want map[string]any
	json.Unmarshal(b, &want)
	want["tempcontrol"] = tempcontrolState()
	want["power"] = powerView(d)
	want["hw_warm"], want["hw_warm_left"], want["hw_pct_steady"] = hwWarmView(d.HWErrors, d.HWPct) // (hwwarm.go)
	wb, _ := json.Marshal(want)
	json.Unmarshal(wb, &want)
	for _, k := range []string{"ghs", "ghs_avg"} { // the live rate moves between two reads
		delete(got, k)
		delete(want, k)
	}
	if bs, ok := got["boards"].([]any); ok {
		for _, b := range bs {
			delete(b.(map[string]any), "ghs")
		}
	}
	if bs, ok := want["boards"].([]any); ok {
		for _, b := range bs {
			delete(b.(map[string]any), "ghs")
		}
	}
	delete(got, "power")
	delete(want, "power") // (watts follow the live rate)
	if !reflect.DeepEqual(got, want) {
		gb, _ := json.Marshal(got)
		wb, _ := json.Marshal(want)
		t.Fatalf("dashboard JSON changed:\n got %s\nwant %s", gb, wb)
	}
	if _, ok := got["tempcontrol"]; !ok {
		t.Fatal("no tempcontrol")
	}
	allocs := testing.AllocsPerRun(5, func() {
		handleDashboard(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/dashboard", nil))
	})
	if allocs > 2500 {
		t.Fatalf("handleDashboard made %.0f allocations", allocs)
	}
}

// the page stops its 8 s / 10 s polls while it's in a background tab and catches up when shown
func TestL49PagePausesHidden(t *testing.T) {
	re := regexp.MustCompile(`window\._iv=setInterval\(\(\)=>\{if\(document\.hidden\)return;`)
	if !re.MatchString(indexHTML) || !strings.Contains(indexHTML, `window._cv=setInterval(()=>{if(!document.hidden)refreshChips();}`) ||
		!strings.Contains(indexHTML, `addEventListener('visibilitychange'`) {
		t.Fatal("the page keeps polling the miner while hidden")
	}
}
