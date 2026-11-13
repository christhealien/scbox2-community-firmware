//go:build public

package main

// The public release (github.com/christhealien/scbox2-community-firmware), built with -tags scbox2,public.
//   - No Commissioning: no section in Settings, no banner on the dashboard, /api/commission refuses changes. The
//     features that passed their hardware tests are always on, the rest always off (publicFeatures, commission.go).
//   - No overclock: the clock ceiling isn't decided, so nothing above the stock window (0 = none).
const publicBuild = true

const box2OCHardMax = 0
