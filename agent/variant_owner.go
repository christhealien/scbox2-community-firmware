//go:build !public

package main

// The owner's build (the default; the commissioning build): Settings -> Commissioning switches each feature on per
// unit after its hardware test, and the dashboard banner says which are on (commission.go). The overclock ceiling
// is the same as the public release's (box2OCHardMax, ceilingbox2.go: 750 MHz, bounded by this unit's probe result).
const publicBuild = false
