//go:build !public

package main

// The owner's build (the default; the commissioning build): Settings -> Commissioning switches each feature on per
// unit after its hardware test, and the dashboard banner says which are on (commission.go).
const publicBuild = false

// box2OCHardMax: the unlocked clock ceiling on the SC Box II (MHz) for the owner's commissioning of the ceiling
// probe (C6) and the overclock (C7): the probe's top and, once OverclockUnlock is commissioned, the unlocked
// window's. 725 MHz is the owner's first, cautious ceiling for C6 (judged by the panel's checks plus the lug volts
// on the C6 sheet). Nothing in the driver or minerd range-checks the clock (the PLL alone goes to 1487.5 MHz on stock
// 690's divider), so the ceiling has to come from this decision, not the software. The public build has none.
const box2OCHardMax = 725
