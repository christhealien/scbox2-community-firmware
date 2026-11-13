package main

// Model profile (R14 — the SC Box II port foundation). Every hardware-specific figure the panel uses comes
// from here, picked once at start from the OEM's own identity files and cross-checked against
// /root/product.json. A miner that matches no known profile, or whose product.json disagrees with its
// profile, gets the "unknown" profile: monitoring only, every write feature off.
//
// Lanes after A1 read `model` (the active profile) and `model.Feat` (the feature gates). The window
// constants (freqMin/freqMax/…) are kept in main.go but are now variables seeded from this profile at
// start-up, so the SC Lite profile reproduces the exact pre-port values and SC Lite behaviour is
// unchanged. See setWindowsFromModel.

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
)

type planFormat int

const (
	// SC Lite: "625 MHz 9100 V 50 RPM 50 RPM PV 9400" (freq, Vrun mV, fan1%, fan2%, peak PV mV).
	planSCLitePV planFormat = iota
	// Box II: "690 MHz 42 V 50 RPM 41 RPM" — a float PWM-trim "V", two fan duties, and NO PV. The trim
	// text is carried verbatim (powerplan.VText) and only the two stock tuples are ever written.
	planBox2V
)

// modelFeatures gates every write/control feature. All OFF on Box II (ship monitoring only until the
// hardware is measured); all ON on SC Lite (its long-proven behaviour). A later lane that owns a feature
// handler calls featOff()/featGate() to refuse cleanly when its gate is off.
type modelFeatures struct {
	ClockControl     bool // Controls/presets/schedule may write a clock
	VoltageControl   bool // anything may write a voltage other than the running plan's own
	Tuner            bool // the auto-tuner
	CeilingProbe     bool // the limit probe (overclock caps)
	OverclockUnlock  bool // the unlock-the-window acknowledgment
	ChipTrims        bool // hook library + per-chip trims (manual, auto-trim, strong-chip test)
	FanPairOffset    bool // per-fan-pair offset
	FanFit           bool // "fit to my fans" product.json overlay
	PowerEstimate    bool // watt estimate
	HardwareWatchdog bool // the hardware watchdog control
	FirmwareUpdate   bool // in-panel system image update / rollback

	// SoftIdle (lane B3): the safety guards may stop hashing through the miner API (pgadisable per board)
	// on a model with no stock idle plan. UNPROVEN on the SC Box II — DEFAULTS OFF (conservative): the guards
	// then rely on the OEM's 86 °C hardware cut-off (the guaranteed backstop) and drive fans to 100 %. A
	// hardware test (T1 Gate B) that confirms pgadisable safely stops hashing is what lets this be turned on.
	// Irrelevant on SC Lite, which parks via its own stock idle plan (applyIdle) regardless of this gate.
	SoftIdle bool
}

// box2Tuple is one allowed Box II stock plan, matched by clock + integer PWM-trim "V". The Box II plan
// writer refuses (never clamps) any clock/V that is not one of these.
type box2Tuple struct {
	FreqMHz int
	V       int // integer part of the plan's "V" trim field
}

type modelProfile struct {
	ID             string // "sclite" | "scbox2" | "unknown"
	Name           string // UI text: "SC Lite" / "SC Box II"
	StockModel     string // product.json "model"
	MCB            string // /root/mcbversion
	InitLogTag     string // intminer "C%d: <tag> Init sucessed"
	Boards         int
	ChipsPerBoard  int
	ChipModel      string
	StagePairs     bool // chips 2k-1,2k share a power stage (product.json layout all 2s)
	Fans           int
	FanGroups      [][]int // fans sharing one PWM signal
	FanStockMaxRPM int

	// thermal, all in °C. The cut-off is the OEM fanctrl's own shutdown (product.json tempcutoff) and is
	// never raised. The panel guards are derived: heat-guard = cutoff-4, abort-max = cutoff-2.
	TempCutoffC  float64
	HeatGuardC   float64 // "never lower the fans below here"            (cutoff-4)
	AbortMaxC    float64 // highest abort a user/preset may set          (cutoff-2)
	AbortDefC    float64 // default abort for curves/tuner               (cutoff-4)
	ThrottleDefC float64
	ResumeDefC   float64
	TargetC      float64 // tuner / fan-curve target temperature

	FreqMin, FreqMax int // locked window; FreqMax <= highest stock plan clock on new models
	FreqHardMax      int // unlocked ceiling (== FreqMax when OverclockUnlock is off)
	// OCHardMax: the unlocked ceiling once OverclockUnlock is commissioned on the unit, and the top of the
	// clock-ceiling probe (CeilingProbe). 0 = FreqHardMax. Used only where FreqHardMax == FreqMax (SC Box II).
	OCHardMax int
	ClockStep float64 // MHz grid of the chips (0 = unknown)

	Plan                                  planFormat
	Box2Plans                             []box2Tuple // planBox2V only: the stock tuples the writer will accept
	PVMin, PVMax, VrunMin, VrunMax, PVGap int         // SC Lite only (0 elsewhere)

	// planBox2V only (lane C8, the tuner): the voltage-level "V" codes the tuner may write (closed interval,
	// default = the two stock codes) and which way a higher code moves the boards' voltage. VCodeDir is set
	// only from a MEASURED result (C4); while it is vcodeDirUnknown the tuner tunes the clock only, at the
	// stock Hashrate code (see tuner_box2.go).
	VCodeMin, VCodeMax int
	VCodeDir           vcodeDir

	PowerRefW, PowerRefMHz, PowerRefPV float64
	PowerRefBoards                     int

	Feat modelFeatures
}

var profSCLite = modelProfile{
	ID: "sclite", Name: "SC Lite", StockModel: stockModelSCLite, MCB: "MCB_V4_3", InitLogTag: "SCLITE",
	Boards: 4, ChipsPerBoard: 46, ChipModel: "ICC590", StagePairs: true,
	Fans: 4, FanGroups: [][]int{{0, 1}, {2, 3}}, FanStockMaxRPM: 2200,
	TempCutoffC: 92, HeatGuardC: 88, AbortMaxC: 90, AbortDefC: 88, ThrottleDefC: 90, ResumeDefC: 80, TargetC: 75,
	FreqMin: 300, FreqMax: 700, FreqHardMax: 900, ClockStep: 12.5, Plan: planSCLitePV,
	PVMin: 9100, PVMax: 9600, VrunMin: 8800, VrunMax: 9300, PVGap: 300,
	PowerRefW: 899, PowerRefMHz: 625, PowerRefPV: 9400, PowerRefBoards: 4,
	Feat: modelFeatures{
		ClockControl: true, VoltageControl: true, Tuner: true, CeilingProbe: true, OverclockUnlock: true,
		ChipTrims: true, FanPairOffset: true, FanFit: true, PowerEstimate: true, HardwareWatchdog: true,
		FirmwareUpdate: true,
	},
}

// SC Box II: figures from its stock 2.2.1 rootfs (product.json, mcbversion, cpbsetting.json, intminer
// strings). Clock window = the stock plans only (525 / 690 MHz); every control feature stays off until
// measured on real hardware. Cut-off 86 (never raised); heat-guard 82 (-4); abort-max 84 (-2); target 77.
var profSCBox2 = modelProfile{
	ID: "scbox2", Name: "SC Box II", StockModel: stockModelSCBox2, MCB: "MCB_V6_3_5", InitLogTag: "SCBOX2",
	Boards: 1, ChipsPerBoard: 36, ChipModel: "ICT580", StagePairs: true,
	Fans: 2, FanGroups: [][]int{{0, 1}}, FanStockMaxRPM: 4500,
	TempCutoffC: 86, HeatGuardC: 82, AbortMaxC: 84, AbortDefC: 82, ThrottleDefC: 82, ResumeDefC: 75, TargetC: 77,
	// ClockStep: the ICT580 PLL grid as the driver logs it ("ICT580 set Clock(req 690, actual 687.5)
	// pll params(55.2.0)"): from 500 MHz up a chip runs 12.5 MHz x N, rounding a request DOWN (510 -> 500,
	// 530 -> 525, 690 -> 687.5). Measured on the owner's unit (lane C8); below 500 the dividers change.
	FreqMin: 525, FreqMax: 690, FreqHardMax: 690, ClockStep: 12.5, Plan: planBox2V,
	// OCHardMax: the overclock ceiling is NOT decided for the SC Box II (box2OCHardMax = 0, ceilingbox2.go), so the
	// clock-ceiling probe (C6) and the overclock unlock (C7) refuse anything above 690 MHz.
	OCHardMax: box2OCHardMax,
	Box2Plans: []box2Tuple{{FreqMHz: 690, V: 42}, {FreqMHz: 525, V: 61}},
	Feat:      modelFeatures{}, // every control feature OFF
	// the voltage LEVELS (the OEM's code, not millivolts; there is no PV) the tuner may write: the stock levels'
	// closed interval, 42 = stock Hashrate = the highest voltage the panel ever sets. Direction MEASURED (C4, at
	// the power lugs under load): a higher level is a LOWER voltage — 42 = 7.67 V, 43 = 7.61, 44 = 7.58,
	// 45 = 7.55, 46 = 7.52, 61 = 7.03 (about 33.5 mV a level).
	VCodeMin: 42, VCodeMax: 61, VCodeDir: vcodeUpIsLess,
}

// profUnknown: a miner whose product.json matches neither profile. Monitoring only — every write feature is
// off (empty modelFeatures). The thermal/geometry fields are conservative non-zero defaults so the read-only
// guards don't misfire (a 0 cut-off would make the thermal guard trip on every reading): a generic safe
// envelope, fans and chip count left generous so nothing is flagged as "missing" on an unknown board.
var profUnknown = modelProfile{
	ID: "unknown", Name: "miner",
	Boards: 1, ChipsPerBoard: 255, ChipModel: "unknown",
	Fans: 4, FanGroups: [][]int{{0}, {1}, {2}, {3}}, FanStockMaxRPM: 6000,
	TempCutoffC: 90, HeatGuardC: 86, AbortMaxC: 88, AbortDefC: 86, ThrottleDefC: 86, ResumeDefC: 78, TargetC: 80,
	Plan: planSCLitePV,
	Feat: modelFeatures{},
}

// model is the active profile. Tests keep the SC Lite figures; main() calls loadModelFiles then
// setWindowsFromModel to apply the loaded profile. modelWhy records why the profile was chosen.
var model = profSCLite
var modelWhy = "default (SC Lite); loadModel not called"

// The "model" strings the stock firmware writes in /root/product.json. They carry the OEM's name, so they
// are kept verbatim only to recognise the miner (loadModel); people see stockModelLabel's neutral form.
const (
	stockModelSCLite = "Goldshell-SCLITE"
	stockModelSCBox2 = "Goldshell-SCBoxⅡ"
)

// stockVendorPrefix: the manufacturer prefix the stock model strings start with ("…-").
var stockVendorPrefix = stockModelSCLite[:strings.IndexByte(stockModelSCLite, '-')+1]

// stockModelLabel: a product.json "model" value as people read it (the why of the model choice, the
// system log): a known model by its profile name, any other with the manufacturer prefix taken off.
func stockModelLabel(raw string) string {
	switch raw {
	case stockModelSCLite:
		return profSCLite.Name
	case stockModelSCBox2:
		return profSCBox2.Name
	}
	if n := len(stockVendorPrefix); len(raw) > n && strings.EqualFold(raw[:n], stockVendorPrefix) {
		return raw[n:]
	}
	return raw
}

// oemNameRe: the manufacturer's name as the stock programs write it in their logs, in any case (built from
// the stock model string, so it is spelled out once).
var oemNameRe = regexp.MustCompile(`(?i)` + regexp.QuoteMeta(strings.TrimSuffix(stockVendorPrefix, "-")))

// stockTextShown: text from the stock programs (their logs, streamed or exported) as the panel hands it to
// people: the manufacturer's name reads "OEM" ("OEM-sb2 Miner detected"). Code that parses those logs
// reads them raw.
func stockTextShown(s string) string {
	if !oemNameRe.MatchString(s) {
		return s
	}
	return oemNameRe.ReplaceAllString(s, oemName())
}

type productFile struct {
	Model string `json:"model"`
	Cpbs  []struct {
		Chips struct {
			Model  string `json:"model"`
			Cnt    int    `json:"cnt"`
			Layout []int  `json:"layout"`
		} `json:"chips"`
	} `json:"cpbs"`
	Fans []struct {
		Speed  string `json:"speed"`
		MaxRPM int    `json:"maxrpm"`
	} `json:"fans"`
	TempCutoff float64 `json:"tempcutoff"`
}

// loadModel picks the profile. product is the STOCK product.json (read before the fan-fit overlay is
// bound, or the saved stock copy); mcb the contents of /root/mcbversion. A model match whose board/chip/
// fan/cut-off facts disagree with the profile, or whose control board differs, falls back to "unknown"
// (monitoring only) rather than trusting figures the panel might write against.
func loadModel(product []byte, mcb string) (modelProfile, string) {
	var pf productFile
	if err := json.Unmarshal(product, &pf); err != nil {
		return profUnknown, "product.json unreadable: " + err.Error()
	}
	mcb = strings.TrimSpace(mcb)
	shown := stockModelLabel(pf.Model) // what the why says: never the raw string with the OEM's name
	for _, p := range []modelProfile{profSCLite, profSCBox2} {
		if pf.Model != p.StockModel {
			continue
		}
		if mcb != p.MCB {
			return profUnknown, fmt.Sprintf("model %q but control board %q (profile made for %q)", shown, mcb, p.MCB)
		}
		if len(pf.Cpbs) != p.Boards || len(pf.Fans) != p.Fans || pf.TempCutoff != p.TempCutoffC {
			return profUnknown, fmt.Sprintf("model %q: product.json has %d boards, %d fans, cutoff %.0f; profile %d/%d/%.0f",
				shown, len(pf.Cpbs), len(pf.Fans), pf.TempCutoff, p.Boards, p.Fans, p.TempCutoffC)
		}
		for _, c := range pf.Cpbs {
			if c.Chips.Cnt != p.ChipsPerBoard || c.Chips.Model != p.ChipModel || len(c.Chips.Layout)*2 != p.ChipsPerBoard {
				return profUnknown, fmt.Sprintf("model %q: board chips %s x%d, profile %s x%d", shown, c.Chips.Model, c.Chips.Cnt, p.ChipModel, p.ChipsPerBoard)
			}
		}
		for _, f := range pf.Fans {
			if f.MaxRPM > p.FanStockMaxRPM {
				return profUnknown, fmt.Sprintf("model %q: fan maxrpm %d over the profile's %d", shown, f.MaxRPM, p.FanStockMaxRPM)
			}
		}
		return p, "matched " + p.ID
	}
	return profUnknown, fmt.Sprintf("unknown model %q", shown)
}

func loadModelFiles(productPath, mcbPath string) (modelProfile, string) {
	b, err := os.ReadFile(productPath)
	if err != nil {
		return profUnknown, err.Error()
	}
	m, _ := os.ReadFile(mcbPath)
	return loadModel(b, string(m))
}

// setWindowsFromModel seeds main.go's window variables from the active profile. Called once from main()
// after loadModelFiles. For the SC Lite profile these are exactly the old compile-time constants, so SC
// Lite behaviour is byte-for-byte unchanged.
func setWindowsFromModel() {
	freqMin, freqMax = model.FreqMin, model.FreqMax
	freqHardMax = model.FreqHardMax
	if model.Feat.OverclockUnlock && model.OCHardMax > freqHardMax { // (after applyCommission: main())
		freqHardMax = model.OCHardMax
	}
	vrunMin, vrunMax = model.VrunMin, model.VrunMax
	pvMin, pvMax = model.PVMin, model.PVMax
	vrunHardMin, vrunHardMax = model.VrunMin, model.VrunMax
	pvHardMin, pvHardMax = model.PVMin, model.PVMax
	capFloor = map[string]int{"freq": freqMax, "vrun": vrunMax, "pv": pvMax}
}

// ---- feature gates ----

// featOff reports whether a named control feature is off on the active model (true => refuse).
func featOff(on bool) bool { return !on }

// featGate writes the standard "not available on this model" JSON refusal and returns false when the
// feature is off; returns true (do nothing) when it is on. Handlers call:
//
//	if !featGate(w, model.Feat.Tuner, "the auto-tuner") { return }
func featGate(w http.ResponseWriter, on bool, feature string) bool {
	if on {
		return true
	}
	writeJSON(w, map[string]any{"ok": false, "disabled": true,
		"err": feature + " is not available on this model (" + model.Name + ")"})
	return false
}

// ---- Box II plan format (planBox2V) ----

// box2PlanRe parses "690 MHz 42.000011 V 50 RPM 41 RPM": clock, a float PWM-trim "V" (accepted whether
// integer like the stock info strings or a float), and the two fan duties. No PV field.
var box2PlanRe = regexp.MustCompile(`(\d+)\s*MHz\s+([0-9.]+)\s*V\s+(\d+)\s*RPM\s+(\d+)\s*RPM`)

// parsePowerplanBox2 parses a Box II plan string. The float "V" trim is accepted and its integer part is
// stored in VrunMv (used only to match the stock tuples); PVMv stays 0 (Box II has no PV). The exact "V"
// text is never reformatted by the panel: the writer (mutatePlanBox2) copies the stock plan's own text.
func parsePowerplanBox2(s string) (powerplan, bool) {
	m := box2PlanRe.FindStringSubmatch(s)
	if m == nil {
		return powerplan{}, false
	}
	bad := false
	atoi := func(x string) int {
		n, err := strconv.Atoi(x)
		bad = bad || err != nil
		return n
	}
	// the level is a whole number: a float text is accepted only when it is one (the stock "42.000011"); a
	// fractional level ("42.5", "41.9" would truncate to another level) or an unreadable one ("4.2.1") is not
	// a plan this panel understands (callers then fall back to the stock plan)
	f, err := strconv.ParseFloat(m[2], 64)
	r := math.Round(f)
	if err != nil || math.Abs(f-r) > 0.001 {
		return powerplan{}, false
	}
	pp := powerplan{
		FreqMHz: atoi(m[1]),
		VrunMv:  int(r),
		Fan1:    atoi(m[3]),
		Fan2:    atoi(m[4]),
	}
	if bad {
		return powerplan{}, false
	}
	return pp, true
}

// buildPowerplanBox2 renders "%d MHz %d V %d RPM %d RPM" with NO PV. The stock info strings carry an
// integer "V" (e.g. "42"), so this reproduces them exactly. The live writer does not build through here
// when it has the stock plan's own text — it substitutes fans into that text (box2WithFans) so any float
// trim survives verbatim; this function is the format for synthesised/compared plans.
func buildPowerplanBox2(p powerplan) string {
	return fmt.Sprintf("%d MHz %d V %d RPM %d RPM", p.FreqMHz, p.VrunMv, p.Fan1, p.Fan2)
}

// box2WithFans takes a stock plan's own info string and returns it with just the two fan duties replaced,
// preserving the clock and the exact "V" trim text (integer or float) byte-for-byte.
func box2WithFans(stockInfo string, fan1, fan2 int) (string, bool) {
	m := box2PlanRe.FindStringSubmatch(stockInfo)
	if m == nil {
		return "", false
	}
	return fmt.Sprintf("%s MHz %s V %d RPM %d RPM", m[1], m[2], fan1, fan2), true
}

// box2Field4 returns the 4th field of the stock plan for p's clock: the plan with the same clock, or else
// the stock plan with the lowest clock at or above it. The Box II driver writes this field to the second
// voltage-trim PWM (fan3 = 100 − field; stock: 40/41 on the 690 MHz plan, 59 on the 525 MHz plan), so the panel
// never puts a fan duty there. Matched on the clock only: the info text's "V" reads 0 on some stock settings.
func box2Field4(cur map[string]any, p powerplan) (int, bool) {
	val, at, ok := 0, 0, false
	for _, mm := range stockPlans(cur) {
		info, _ := mm["info"].(string)
		q, good := parsePowerplanBox2(info)
		if !good || q.Fan2 <= 0 || q.Fan2 >= 100 {
			continue
		}
		if q.FreqMHz == p.FreqMHz {
			return q.Fan2, true
		}
		if q.FreqMHz > p.FreqMHz && (!ok || q.FreqMHz < at) {
			val, at, ok = q.Fan2, q.FreqMHz, true
		}
	}
	if !ok && p.FreqMHz > model.FreqMax && !publicBuild && model.OCHardMax > model.FreqMax {
		// C6/C7, above the stock window (box2OverclockErr: at the Hashrate plan's V code): that plan's own value
		hf, _ := box2HashrateV()
		if hf != p.FreqMHz {
			return box2Field4(cur, powerplan{FreqMHz: hf})
		}
	}
	return val, ok
}

// box2StockInfoFor returns the info string of the stock plan in cur whose clock+V match p, or "".
func box2StockInfoFor(cur map[string]any, p powerplan) string {
	for _, mm := range stockPlans(cur) {
		info, _ := mm["info"].(string)
		if q, ok := parsePowerplanBox2(info); ok && q.FreqMHz == p.FreqMHz && q.VrunMv == p.VrunMv && q.VrunMv > 0 {
			return info // (an info text reading "0.000000 V" is not copied: the writer builds the plan itself)
		}
	}
	return ""
}

// box2StockMatch reports whether a plan's clock+V is one of the model's stock tuples.
func box2StockMatch(p powerplan) bool {
	for _, t := range model.Box2Plans {
		if p.FreqMHz == t.FreqMHz && p.VrunMv == t.V {
			return true
		}
	}
	return false
}

// box2Mode is one of the two stock modes the panel offers on Box II: a label (Hashrate / Low-power), its
// position in the firmware's own plan list (for select-by-index, manual:false), and the stock clock/trim/fans
// it carries. The panel only ever selects between these; it never builds a custom clock/voltage preset.
type box2Mode struct {
	Name       string
	Index      int // position in the firmware's plan list; -1 when read from the profile (miner unreachable)
	Freq, V    int
	Fan1, Fan2 int
}

// box2StockHigh / box2StockLow: what a person sees for the OEM's two stock modes on the SC Box II. The keys
// stay box2ModeName's ("Hashrate" / "Low-power"): the API, the schedule's saved rules and the charge ("preset:Hashrate")
// keep them; only the texts change (presetLabel; the page's b2Name does the same).
const (
	box2StockHigh = "Stock High"
	box2StockLow  = "Stock Low"
)

// presetLabel: the name a person sees for a preset key (the Presets cards, the schedule's brushes, activity lines,
// notifications, messages). On the SC Box II the stock modes read "Stock High" / "Stock Low", and "Stock" (the
// factory default: the same plan as Hashrate, box2DefaultIndex) reads "Stock High". Anything else, and every
// name on the other models, is unchanged.
func presetLabel(name string) string {
	if model.Plan != planBox2V {
		return name
	}
	switch {
	case strings.EqualFold(name, "Hashrate"), strings.EqualFold(name, "Stock"):
		return box2StockHigh
	case strings.EqualFold(name, "Low-power"):
		return box2StockLow
	}
	return name
}

// stockWord: what to call the stock setting in a sentence ("apply Stock or a preset"): "Stock High" on the SC
// Box II, where the Stock card is folded into it; "Stock" elsewhere.
func stockWord() string {
	if model.Plan == planBox2V {
		return box2StockHigh
	}
	return "Stock"
}

// box2ModeName labels a Box II stock plan by clock: the highest-clock stock tuple is "Hashrate", the rest
// "Low-power". The profile ships exactly two (690 MHz Hashrate, 525 MHz Low-power).
func box2ModeName(freq int) string {
	hi := 0
	for _, t := range model.Box2Plans {
		if t.FreqMHz > hi {
			hi = t.FreqMHz
		}
	}
	if freq >= hi {
		return "Hashrate"
	}
	return "Low-power"
}

// box2ModesFrom returns the sanctioned stock modes from a live /mcb/setting map: only the plans whose
// clock+trim match a profile stock tuple (the 0 MHz / non-stock entries are skipped).
func box2ModesFrom(cur map[string]any) []box2Mode {
	var modes []box2Mode
	for i, mm := range stockPlans(cur) {
		pp, ok := stockPlanOf(mm) // (the code from the entry's voltage field when its info text reads 0 V)
		if !ok || pp.FreqMHz <= 0 || !box2StockMatch(pp) {
			continue
		}
		modes = append(modes, box2Mode{Name: box2ModeName(pp.FreqMHz), Index: i,
			Freq: pp.FreqMHz, V: pp.VrunMv, Fan1: pp.Fan1, Fan2: pp.Fan2})
	}
	return modes
}

// box2Modes returns the live stock modes, falling back to the profile's own stock tuples when the backend
// can't be read (so the schedule/preset UI can still label the two brushes).
func box2Modes() []box2Mode {
	if raw, err := stockGet("/mcb/setting"); err == nil {
		var cur map[string]any
		if json.Unmarshal(raw, &cur) == nil {
			if m := box2ModesFrom(cur); len(m) > 0 {
				return m
			}
		}
	}
	var m []box2Mode
	for _, t := range model.Box2Plans {
		m = append(m, box2Mode{Name: box2ModeName(t.FreqMHz), Index: -1, Freq: t.FreqMHz, V: t.V})
	}
	return m
}

// isBox2ModeName reports whether name is one of the model's stock-mode labels.
func isBox2ModeName(name string) bool {
	for _, m := range box2Modes() {
		if strings.EqualFold(m.Name, name) {
			return true
		}
	}
	return false
}

// presetsList is what the Presets tab and the scheduler treat as "the presets". On the voltage models it is
// the owner's saved presets; on Box II it is the two stock modes (Hashrate / Low-power), which the panel can
// only select between — never a stored custom clock/voltage preset (it carries no voltage and no fan curve).
func presetsList() []preset {
	if model.Plan == planBox2V {
		var out []preset
		for _, m := range box2Modes() {
			out = append(out, preset{Name: m.Name, MHz: m.Freq, MV: m.V, PV: 0, FanProfile: "none"})
		}
		return append(out, box2SavedPresets()...) // lane C3: the owner's clock presets, only with ClockControl on
	}
	return loadPresets()
}

// validateControlBox2 refuses (never clamps) any clock/V that is not a stock tuple — unless clock control
// (lane C3) / voltage control (C4) are commissioned, when box2CheckTuple's window and V rule apply instead.
// Fan duties may move (0–100%); the fan-safety "up only" rule is enforced by the fan lane, not here.
func validateControlBox2(p powerplan) error {
	if err := box2CheckTuple(p.FreqMHz, p.VrunMv); err != nil {
		return err
	}
	if p.Fan1 < 0 || p.Fan1 > 100 || p.Fan2 < 0 || p.Fan2 > 100 {
		return fmt.Errorf("fan duty must be 0–100%%")
	}
	return nil
}

// mutatePlanBox2 is the planBox2V branch of mutatePlanEx. It reads the live /mcb/setting, applies mut to
// the running plan, refuses anything that is not a stock tuple, and writes the plan back using the stock
// plan's own V trim text. It never invents a voltage/PV and never writes a non-stock clock.
func mutatePlanBox2(mut func(*powerplan), wake bool, gen int64) (string, error) {
	plan, want, err := mutatePlanBox2Write(mut, wake, gen)
	if err != nil || want == nil {
		return plan, err
	}
	// lanes C3/C4: a clock/V change (only possible as a non-stock write with the gates on, or a stock switch
	// with ClockControl on) is confirmed from the miner API and reverted on a mismatch. The fan loop's writers
	// (wake=false: the thermal throttle) can't wait: theirs is confirmed in the background.
	if !wake {
		box2AsyncConfirms.Add(1)
		go func() { defer box2AsyncConfirms.Done(); box2Confirm(*want) }()
		return plan, nil
	}
	if err := box2Confirm(*want); err != nil {
		return "", err
	}
	return plan, nil
}

// mutatePlanBox2Write: mutatePlanBox2's read-modify-write under planMu. want != nil when the write changed
// the clock or V with ClockControl on: the caller confirms it (outside planMu).
func mutatePlanBox2Write(mut func(*powerplan), wake bool, gen int64) (string, *box2Want, error) {
	planMu.Lock()
	defer planMu.Unlock()
	if genMoved(gen) {
		return "", nil, errSafetyStop
	}
	raw, err := stockGet("/mcb/setting")
	if err != nil {
		return "", nil, err
	}
	var cur map[string]any
	if err := json.Unmarshal(raw, &cur); err != nil {
		return "", nil, err
	}
	if !wake && (minerIdle(cur) || box2SoftIdled()) { // (lane C1: a board held off by the soft idle is Idle too)
		return "", nil, errMinerIdle
	}
	if wake {
		// Box II soft-idle (lane B3): a guard may have powered boards down with pgadisable. Re-enable them
		// (pgaenable) before writing a hashing plan. No-op on SC Lite and whenever SoftIdle is off. Refused while a
		// guard parks or holds the miner, and a safety stop that landed while it waited refuses the write.
		if err := box2WakeResume(gen); err != nil {
			return "", nil, err
		}
	}
	pp, ok := resolveRunningPlan(cur)
	if !ok || pp.FreqMHz == 0 {
		if i := firstHashingPlan(cur); i >= 0 {
			pp, ok = stockPlanAt(cur, i)
		}
	}
	if !ok {
		return "", nil, fmt.Errorf("could not read running powerplan")
	}
	before := pp
	mut(&pp)
	if err := box2OverclockErr(pp, before); err != nil { // C6/C7: above the stock window (ceilingbox2.go)
		return "", nil, err
	}
	// a raise (the throttle's restore, the schedule's going back, Controls, a preset) never runs a chip trimmed
	// "+" past the clock window: checked here, under planMu, as the SC Lite writer does (box2Revert, which can't
	// lower the trims under planMu, goes to the low stock plan instead)
	if pp.FreqMHz > before.FreqMHz {
		if err := plusTrimErr(pp.FreqMHz); err != nil {
			return "", nil, fmt.Errorf("refused: %v", err)
		}
	}
	// A clock or V change is refused outright unless it lands on a stock tuple — or, with clock/voltage
	// control commissioned (lanes C3/C4), inside box2CheckTuple's window and V rule.
	if pp.FreqMHz != before.FreqMHz || pp.VrunMv != before.VrunMv {
		if err := validateControlBox2(pp); err != nil {
			return "", nil, err
		}
	}
	if !box2StockMatch(pp) {
		// a running non-stock tuple (fans-only write) or a new one: allowed only under the commissioned gates
		if !model.Feat.ClockControl {
			return "", nil, fmt.Errorf("refused: %d MHz at %s is not a stock plan on this model", pp.FreqMHz, box2LevelText(pp.VrunMv))
		}
		if err := box2CheckTuple(pp.FreqMHz, pp.VrunMv); err != nil {
			return "", nil, err
		}
	}
	// fans at or over the floor and never pulled down while hot (same guards as SC Lite)
	fl := fanFloor()
	if pp.Fan1 < fl {
		pp.Fan1 = fl
	}
	pp.Fan1 = min(heatGuardFan(pp.Fan1), fanMax)
	// The 4th field is not a fan on the Box II: the driver writes it to the second trim PWM (fan3 = 100 − it).
	// It is always the stock plan's own value, whatever the fans do.
	f4, ok := box2Field4(cur, pp)
	if !ok {
		return "", nil, fmt.Errorf("refused: the stock plans don't give this model's voltage-trim setting (the plan's 4th field, kept at the stock value: not a fan speed) for %d MHz", pp.FreqMHz)
	}
	pp.Fan2 = f4
	// Build using the matching stock plan's OWN info text (just the fans swapped), so the clock and the
	// exact "V" trim text are copied verbatim and never re-encoded by the panel.
	plan := buildPowerplanBox2(pp)
	if info := box2StockInfoFor(cur, pp); info != "" {
		if s, ok := box2WithFans(info, pp.Fan1, pp.Fan2); ok {
			plan = s
		}
	}
	if m, _ := cur["manual"].(bool); m && cur["manualPowerplan"] == plan {
		if lv, err := fanLvl("fan1"); err == nil && lv == pp.Fan1 {
			return plan, nil, nil
		}
	}
	cur["manual"] = true
	cur["manualPowerplan"] = plan
	body, _ := json.Marshal(cur)
	if _, err := stockReq("PUT", "/mcb/setting", body); err != nil {
		if strings.HasPrefix(err.Error(), "HTTP 4") {
			return "", nil, putRefusedErr{err}
		}
		return "", nil, err
	}
	if model.Feat.ClockControl && (pp.FreqMHz != before.FreqMHz || pp.VrunMv != before.VrunMv) {
		return plan, &box2Want{MHz: pp.FreqMHz, V: pp.VrunMv, Before: before, Fan1: pp.Fan1, Fan2: pp.Fan2,
			Plan: plan, Seq: box2WriteSeq.Add(1)}, nil
	}
	return plan, nil, nil
}

// idleAction and its resume live in idleaction.go (lane B3): SC Lite parks via the firmware's own idle plan
// (applyIdle, unchanged); Box II parks via the miner API (pgadisable), gated by Feat.SoftIdle and UNPROVEN.

// handleModel exposes the active profile to the UI JS, so the front-end stops hard-coding model facts.
func handleModel(w http.ResponseWriter, r *http.Request) {
	m := map[string]any{
		"ok":               true,
		"id":               model.ID,
		"name":             model.Name,
		"why":              modelWhy,
		"boards":           model.Boards,
		"chips_per_board":  model.ChipsPerBoard,
		"chip_model":       model.ChipModel,
		"fans":             model.Fans,
		"fan_groups":       model.FanGroups,
		"fan_stock_maxrpm": model.FanStockMaxRPM,
		"temp_cutoff":      model.TempCutoffC,
		"heat_guard":       model.HeatGuardC,
		"abort_max":        model.AbortMaxC,
		"abort_def":        model.AbortDefC,
		"throttle_def":     model.ThrottleDefC,
		"resume_def":       model.ResumeDefC,
		"target":           model.TargetC,
		"freq_min":         model.FreqMin, "freq_max": model.FreqMax, "freq_hardmax": model.FreqHardMax,
		"plan":     planFormatName(model.Plan),
		"features": model.Feat,
		// lane B3: when the model parks via the unproven miner-API stop, the UI must show this caveat.
		"soft_idle":          model.Plan == planBox2V,
		"soft_idle_enabled":  model.Feat.SoftIdle,
		"soft_idle_unproven": model.Plan == planBox2V && !model.Feat.SoftIdle,
		"soft_idle_caveat":   idleActionUnprovenMsg,
	}
	if sig := commissionSig(); sig != "" { // only on a model with features to commission: the SC Lite's reply is unchanged
		m["commission"] = sig
	}
	m["oem"] = oemName() // the miner's manufacturer as the page names it (its OEM()): never by name
	if publicBuild {     // the public release: the page shows no Commissioning section and no banner
		m["public"] = true
	}
	if model.Plan == planBox2V && model.Feat.SoftIdle {
		m["soft_idle_status"] = box2IdleStatus() // lane C1: held / phase / note of the soft idle (for the owner's test)
	}
	writeJSON(w, m)
}

func planFormatName(p planFormat) string {
	if p == planBox2V {
		return "box2v"
	}
	return "sclitepv"
}
