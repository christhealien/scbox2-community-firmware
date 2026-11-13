package main

import "testing"

func TestValidMAC(t *testing.T) {
	for _, m := range []string{"28:e2:97:1f:92:58", "02:47:73:28:45:75", "3c:2a:f4:01:02:03"} {
		if !validMAC(m) {
			t.Errorf("should accept %s", m)
		}
	}
	for _, m := range []string{"", "00:00:00:00:00:00", "ff:ff:ff:ff:ff:ff", "01:00:5e:00:00:01", "3c:2a:f4:01:02", "3C:2A:F4:01:02:03", "zz:2a:f4:01:02:03"} {
		if validMAC(m) {
			t.Errorf("should refuse %q", m)
		}
	}
}
