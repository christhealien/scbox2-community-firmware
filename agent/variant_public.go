//go:build public

package main

// The public release (github.com/christhealien/scbox2-community-firmware), built with -tags scbox2,public.
//   - No Commissioning: no section in Settings, no banner on the dashboard, /api/commission refuses changes. The
//     features that passed their hardware tests are always on, the rest always off (publicFeatures, commission.go).
//   - The clock ceiling probe and the overclock unlock are on, under the same per-unit rule as the owner's build
//     (ceilingbox2.go): the unlock opens only up to this unit's own clean probe result, never above 750 MHz.
const publicBuild = true
