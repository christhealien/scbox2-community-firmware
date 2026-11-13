package main

// The miner has no battery-backed clock: it reads 1970 after a boot until NTP or the browser sets it (a jump of
// ~56 years), and a time server can later step a clock set from a browser back or forward. A wall-clock time
// kept to be compared later (a pause, a cool-down, a heartbeat) must survive those steps: in memory it is
// measured on the monotonic clock; a saved one written on the unset clock is recognised by being before 2024.

import "time"

// clockSetUnix: 2024-01-01 UTC. Any wall-clock time before it was read on the unset (1970) clock.
const clockSetUnix int64 = 1704067200

// unsetStamp: a saved unix time (non-zero) that was taken on the unset 1970 clock.
func unsetStamp(u int64) bool { return u > 0 && u < clockSetUnix }

// monoNanos: nanoseconds on this process's monotonic clock (see monoSec in fancheck.go); never 0.
func monoNanos() int64 { return int64(time.Since(monoStart)) + 1 }
