package main

import "strings"

// Names on the miner that carry the firmware's name (brand, from brand_sclite.go or brand_scbox2.go). For
// the default SC Lite build every one of these is exactly the literal it replaced, so that build is unchanged.
const (
	brandAgent       = brand + "-agent"        // the panel binary's name (/usr/bin/<this>)
	brandCfgDir      = "/usr/config/" + brand  // the panel's settings directory
	brandTmp         = "/tmp/" + brand + "-"   // prefix of the panel's and helpers' files in /tmp (RAM)
	brandLibexec     = "/usr/libexec/" + brand // the stock miner's kept copy, hook copy, ntp, sysctl
	brandChipLib     = brand + "_chip.so"      // the per-chip trim library's file name
	brandChipLibPath = "/lib/" + brandChipLib
	brandInitScript  = "/etc/init.d/S48" + brand    // the panel's boot script
	brandMacScript   = "/etc/" + brand + "-mac"     // the boot step that sets eth0's MAC
	brandRelease     = "/etc/" + brand + "-release" // version and build details of the firmware
	brandShim        = brand + " shim"              // marker in the miner wrapper (/usr/bin/intminer)
	brandSettings    = brand + "-settings"          // the format name in exported settings files
)

// brandHeader: the product name in the panel's header, upper case with non-breaking spaces ("SC&nbsp;LITE").
var brandHeader = strings.ReplaceAll(strings.ToUpper(brandProduct), " ", "&nbsp;")

// brandEnv: the name of a testing/override environment variable, SCLITE_<name> on the SC Lite (unchanged) and
// SCBOX2_<name> on the SC Box II.
func brandEnv(name string) string { return strings.ToUpper(brand) + "_" + name }

// legacySettingsFormat: settings files exported by SC Box II test builds before the rename carry the SC Lite
// format name; their contents are the same, so they're accepted.
const legacySettingsFormat = "sclite-settings"

// oemName: the miner's manufacturer in text people read. The SC Box II firmware never names it ("OEM": "the
// OEM firmware", "OEM spec"); the SC Lite keeps "Goldshell", byte-identical to its text before this helper.
// On the Box II model (also in an untagged test build) or on the scbox2 build it is "OEM".
func oemName() string {
	if model.Plan == planBox2V {
		return "OEM"
	}
	return brandOEM
}

// The manufacturer as the owner of something, mid-sentence and at a sentence start. The SC Lite's text says
// "Goldshell's" in every form. On the SC Box II, oemOf is attributive ("the OEM watchdog", "the OEM firmware")
// and oemPoss possessive ("the OEM's own fan control", "the OEM's 86 °C cut-off").
func oemOf() string   { return oemForm("the OEM") }
func OemOf() string   { return oemForm("The OEM") }
func oemPoss() string { return oemForm("the OEM's") }
func OemPoss() string { return oemForm("The OEM's") }

func oemForm(box2 string) string {
	if oemIsLite() {
		return "Goldshell's"
	}
	return strings.Replace(box2, "OEM", oemName(), 1)
}

// oemIsLite: true when the manufacturer is named (the SC Lite's text); a sentence that is reworded rather than
// word-swapped on the SC Box II branches on it.
func oemIsLite() bool { return oemName() == "Goldshell" }

// oemOwnCurly: oemPoss with the typographic apostrophe (’), for text written with it.
func oemOwnCurly() string { return strings.Replace(oemPoss(), "'", "’", 1) }

// oemFanCtl: who drives the fans when the panel leaves them ("Goldshell" on the SC Lite, "the OEM fan control").
func oemFanCtl() string {
	if oemIsLite() {
		return "Goldshell"
	}
	return "the OEM fan control"
}

// oemFile: the name of one of the manufacturer's own logs in a log export ("goldshell-…" on the SC Lite,
// "oem-…" on the SC Box II).
func oemFile(name string) string { return strings.ToLower(oemName()) + "-" + name }
