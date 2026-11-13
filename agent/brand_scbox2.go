//go:build scbox2

package main

// The SC Box II firmware's panel: built with -tags scbox2. See brand.go for the names derived from these.
const (
	brand        = "scbox2"    // file and path names: /usr/config/scbox2, /tmp/scbox2-*, scbox2-agent, ...
	brandProduct = "SC Box II" // shown to people: the SSH sign-in banner, the panel's TLS certificate
	brandOEM     = "OEM"       // the miner's manufacturer as text shown to people (oemName, brand.go)
)

// defaultUpdateRepo: the GitHub repository whose latest release "Check for updates" offers (update.go).
const defaultUpdateRepo = "christhealien/scbox2-community-firmware"
