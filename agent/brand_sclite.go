//go:build !scbox2

package main

// The firmware this panel is built into. The default build is the SC Lite firmware; the SC Box II firmware
// builds with -tags scbox2 (brand_scbox2.go). See brand.go for the names derived from these.
const (
	brand        = "sclite"  // file and path names: /usr/config/sclite, /tmp/sclite-*, sclite-agent, ...
	brandProduct = "SC Lite" // shown to people: the SSH sign-in banner, the panel's TLS certificate
	brandOEM     = "OEM"     // the miner's manufacturer as text shown to people (oemName, brand.go)
)

// defaultUpdateRepo: the GitHub repository whose latest release "Check for updates" offers (update.go).
const defaultUpdateRepo = "christhealien/sclite-community-firmware"
