package main

import (
	"testing"
	"time"
)

func TestScheduleInForceWraps(t *testing.T) {
	rules := []schedRule{{Start: "07:00", Days: []int{0, 1, 2, 3, 4, 5, 6}, Preset: "High"}, {Start: "23:00", Days: []int{0, 1, 2, 3, 4, 5, 6}, Preset: "Low"}}
	o := scheduleOccs(rules)
	// Monday 03:00: still last Sunday's 23:00 change (the week wraps round)
	cur, ok := inForce(o, 0*1440+3*60)
	if !ok || rules[cur.i].Preset != "Low" || cur.m != 6*1440+23*60 {
		t.Fatalf("Mon 03:00 in force = %+v", cur)
	}
	cur, _ = inForce(o, 2*1440+12*60)
	if rules[cur.i].Preset != "High" {
		t.Fatalf("Wed 12:00 = %s, want High", rules[cur.i].Preset)
	}
	nx, in, _ := nextChange(o, 6*1440+23*60+30) // Sun 23:30 → Mon 07:00
	if rules[nx.i].Preset != "High" || in != 7*60+30 {
		t.Fatalf("next = %s in %d min", rules[nx.i].Preset, in)
	}
}

func TestRestartRepeats(t *testing.T) {
	at := func(s string) time.Time { v, _ := time.ParseInLocation("2006-01-02 15:04", s, time.Local); return v }
	r := schedRestart{Every: "6h", At: "04:00"}
	for s, want := range map[string]bool{"2026-11-02 04:00": true, "2026-11-02 10:00": true, "2026-11-02 22:00": true, "2026-11-02 07:00": false, "2026-11-02 04:01": false} {
		if g := restartDue(r, at(s)); g != want {
			t.Errorf("6h %s = %v", s, g)
		}
	}
	if !restartDue(schedRestart{Every: "week", At: "04:00", Day: 2}, at("2026-11-04 04:00")) { // a Wednesday
		t.Error("weekly Wed 04:00 not due")
	}
	a := 2026*12 + 10 // Nov 2026
	q := schedRestart{Every: "3months", At: "03:00", Mday: 1, Anchor: &a}
	if !restartDue(q, at("2026-11-01 03:00")) || restartDue(q, at("2026-12-01 03:00")) || !restartDue(q, at("2027-02-01 03:00")) {
		t.Error("3-monthly anchor wrong")
	}
	if !restartDue(schedRestart{Every: "off", Marks: []int{2*1440 + 15*60}}, at("2026-11-04 15:00")) {
		t.Error("restart mark (Wed 15:00) not due")
	}
	if nr, ok := nextRestart(schedRestart{Every: "day", At: "04:00"}, at("2026-11-02 05:00")); !ok || !nr.Equal(at("2026-11-03 04:00")) {
		t.Errorf("next daily = %v", nr)
	}
}

func TestScheduleMigratesGrid(t *testing.T) {
	var g [7][blocksPerDay]string
	for d := 0; d < 7; d++ {
		for k := 0; k < blocksPerDay; k++ {
			g[d][k] = "High"
			if k < 14 { // 00:00–07:00
				g[d][k] = "Low"
			}
		}
	}
	var rs [7][blocksPerDay]bool
	rs[2][8] = true
	s := schedule{Grid: &g, Restarts: &rs, RestartRepeat: "daily", DailyT: "04:30"}
	migrateSchedule(&s)
	if len(s.Rules) != 2 || s.Rules[0].Start != "00:00" || s.Rules[0].Preset != "Low" || s.Rules[1].Start != "07:00" || len(s.Rules[1].Days) != 7 {
		t.Fatalf("rules = %+v", s.Rules)
	}
	if len(s.Restart.Marks) != 1 || s.Restart.Marks[0] != 2*1440+240 || s.Restart.Every != "day" || s.Restart.At != "04:30" {
		t.Fatalf("restart = %+v", s.Restart)
	}
	if s.Grid != nil {
		t.Fatal("legacy grid kept")
	}
}
