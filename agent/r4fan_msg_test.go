package main

import (
	"strings"
	"testing"
	"time"
)

// While mining, xminerd judges a fan against 60 % of its OWN start-up sweep speed (or the speed it recorded at
// that level); the product.json maxrpm line that "Fit the check" changes is used only by the start-up check, at
// 100 % (xminerd 2.2.0: 0x9a24 calls the check with duty 100 and no table; 0x9c26, the mining loop, with the
// sweep table; the maxrpm/125 line is the no-table branch at 0xa148). All fans flagged while they turn at a
// normal level therefore can't be cured by fitting, yet the message sends the owner there.
func TestR4AllFlaggedAtRunLevelAdvisesFit(t *testing.T) {
	now := time.Now()
	l := " [" + now.Format("2006-01-02 15:04:05") + "]  [Xminerd] fan0:(60, 700)  fan1:(60, 650)  fan2:(60, 690)  fan3:(60, 640)  . fan status: f\n"
	v, ok := parseWatchdog(l, now)
	if !ok {
		t.Fatal("parse")
	}
	if m := watchdogMessage(v); strings.Contains(m, "Fan check can fit") {
		t.Fatalf("BUG: flagged at 60 %% (the running check, 60 %% of the start-up sweep): the message advises fitting maxrpm, which only the 100 %% start-up check uses:\n%s", m)
	}
}
