package main

import (
	"testing"
	"time"
)

func TestHeatEscalation(t *testing.T) {
	t0 := time.Date(2026, 11, 6, 2, 0, 0, 0, time.UTC)
	var h heatEscState
	// one wild reading: nothing
	if a, i, _ := h.step(127, 88, t0, false); a || i {
		t.Fatal("a single reading acted")
	}
	if a, i, _ := h.step(66, 88, t0.Add(10*time.Second), false); a || i {
		t.Fatal("cool reading acted")
	}
	// two in a row at the abort temperature: fans 100 %, not Idle yet
	h = heatEscState{}
	h.step(88.5, 88, t0, false)
	if a, i, _ := h.step(89, 88, t0.Add(10*time.Second), false); !a || i {
		t.Fatal("abort not engaged on the 2nd reading")
	}
	// fans written at 20 s; still hot at 100 s: not yet; at 111 s: Idle
	h.step(89, 88, t0.Add(20*time.Second), true)
	if _, i, _ := h.step(89, 88, t0.Add(100*time.Second), true); i {
		t.Fatal("idle too early")
	}
	if _, i, why := h.step(89, 88, t0.Add(111*time.Second), true); !i || why == "" {
		t.Fatal("no idle after 90 s at 100 %")
	}
	// cooling resets the episode
	h = heatEscState{}
	h.step(89, 88, t0, true)
	h.step(89, 88, t0.Add(10*time.Second), true)
	h.step(80, 88, t0.Add(20*time.Second), true)
	if _, i, _ := h.step(89, 88, t0.Add(200*time.Second), true); i {
		t.Fatal("episode not reset by a cool reading")
	}
	// 5 °C past it, twice in a row: Idle at once (one such reading isn't enough)
	h = heatEscState{}
	if _, i, _ := h.step(94, 88, t0, false); i {
		t.Fatal("one far reading went idle")
	}
	if _, i, _ := h.step(93.5, 88, t0.Add(10*time.Second), false); !i {
		t.Fatal("two readings 5 °C past didn't go idle")
	}
}

// the fans never confirmed at 100 % (writes failing): it still goes to Idle, 30 s later
func TestHeatEscalationFanWriteFails(t *testing.T) {
	t0 := time.Date(2026, 11, 6, 2, 0, 0, 0, time.UTC)
	var h heatEscState
	h.step(89, 88, t0, false)
	h.step(89, 88, t0.Add(10*time.Second), false)
	if _, i, _ := h.step(89, 88, t0.Add(100*time.Second), false); i {
		t.Fatal("too early")
	}
	if _, i, _ := h.step(89, 88, t0.Add(131*time.Second), false); !i {
		t.Fatal("no idle with the fan write failing")
	}
}
