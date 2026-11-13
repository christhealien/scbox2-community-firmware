package main

import (
	"testing"
	"time"
)

func TestParseOverTemp(t *testing.T) {
	boot := time.Date(2026, 11, 4, 10, 0, 0, 0, time.Local).Unix()
	log := " [2026-11-04 09:00:00]  Disable CPB, For Temperature Is Greater Than 92 \n" + // before this boot
		" [2026-11-04 11:00:00]  6 times: Over (93.1), Miner Maybe Shutdown\n" +
		" [2026-11-04 11:00:06]  Disable CPB, For Temperature Is Greater Than 92 \n"
	s := parseOverTemp(log, boot)
	if s == nil || s.Limit != 92 || time.Unix(s.At, 0).Hour() != 11 {
		t.Fatalf("%+v", s)
	}
	if parseOverTemp(" [2026-11-04 09:00:00]  Disable CPB, For Temperature Is Greater Than 92 \n", boot) != nil {
		t.Fatal("a shutdown from before this boot counted")
	}
}
