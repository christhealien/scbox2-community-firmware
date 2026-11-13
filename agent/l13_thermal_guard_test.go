package main

import (
	"testing"
	"time"
)

// guards for the L13 fixes (pass on base and with the fix: the fix must not over-hold)

// L13-1c: a board that stops reporting for good is not held forever (a stale reading must not keep the guard on).
func TestL13MissingBoardNotHeldForever(t *testing.T) {
	m := newHLMiner(t)
	c := hlCfg()
	m.setTemps(normal, normal, normal, [3]float64{86, 86, 86})
	m.tick(c, 10*time.Second)
	m.setTemps(normal, normal, normal, [3]float64{-150, -150, -150})
	for i := 0; i < 5; i++ {
		m.tick(c, 10*time.Second)
	}
	fanMu.RLock()
	mt := fanSt.MaxT
	fanMu.RUnlock()
	if mt > 70 {
		t.Fatalf("a board that stopped reporting 50 s ago still counts with its last reading: max %.1f", mt)
	}
}

// L13-2b: woken while still at/over the abort temperature: straight back to Idle (the timers stay elapsed).
func TestL13WokenStillHotReIdlesAtOnce(t *testing.T) {
	t0 := time.Date(2026, 11, 6, 2, 0, 0, 0, time.UTC)
	var h heatEscState
	now := t0
	for i := 0; i < 15; i++ {
		if _, idle, _ := h.step(89, 88, now, i >= 2); idle {
			h.idled()
			break
		}
		now = now.Add(10 * time.Second)
	}
	now = now.Add(5 * time.Minute)
	h.step(88.6, 88, now, false)
	h.step(87.5, 88, now.Add(10*time.Second), false) // a hair under: noise
	if _, idle, _ := h.step(88.4, 88, now.Add(20*time.Second), false); !idle {
		t.Fatal("woken while still at the abort temperature: not straight back to Idle")
	}
}
