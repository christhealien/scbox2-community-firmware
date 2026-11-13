package main

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The firmware never names the miner's manufacturer in anything its user reads, on any model or build: the
// panel says "OEM" ("the OEM firmware", "the OEM watchdog", "OEM spec"). Go side: oemName / oemOf / oemPoss
// (brand.go); page side: OEM() / oemOf() / oemPoss() / oemFw() (from /api/model's "oem", "OEM" before the
// model loads).

// oemWord: the manufacturer's name, which these tests look for (the one place a test spells it out).
var oemWord = regexp.MustCompile(`(?i)goldshell`)

// oemJSFuncs: the page's OEM helpers, put into the node harnesses' preludes (l31Node, l34Node) like MODEL().
var oemJSFuncs = []string{"OEM", "oemOf", "OemOf", "oemPoss", "OemPoss", "oemFw", "OemFw"}

func oemJS(t *testing.T) string {
	t.Helper()
	var b strings.Builder
	for _, f := range oemJSFuncs {
		b.WriteString(l31Func(t, f))
		b.WriteString("\n")
	}
	return b.String()
}

// ---- Go: the messages the Box II user reads ----

// oemGoMessages: user-facing texts from the Go side that speak of the manufacturer, as the active model gives
// them (errors, notes, activity/log lines, the log export's file names and headers).
func oemGoMessages(t *testing.T) map[string]string {
	t.Helper()
	m := map[string]string{
		"oemName": oemName(), "oemOf": oemOf(), "OemOf": OemOf(), "oemPoss": oemPoss(), "OemPoss": OemPoss(),
		"oemOwnCurly": oemOwnCurly(), "oemFanCtl": oemFanCtl(),
	}
	if err := friendlyBackendErr(errors.New("EOF")); err != nil {
		m["backendErr"] = err.Error()
	}
	for i, v := range []watchdogView{
		{Fans: []int{2000, 2000, 2000, 2000}, Duty: []int{50, 50, 50, 50}, Status: 4, Flagged: "the watchdog flags fan 2", Kills: 2},
		{Fans: []int{2000, 2000, 0, 2000}, Duty: []int{50, 50, 50, 50}, Status: 4, Flagged: "the watchdog flags fan 2"},
		{Fans: []int{900, 900, 900, 900}, Duty: []int{100, 100, 100, 100}, Status: 15, Flagged: "the watchdog flags every fan"},
		{Fans: []int{900, 900, 900, 900}, Duty: []int{60, 60, 60, 60}, Status: 15, Flagged: "the watchdog flags every fan"},
	} {
		m["watchdog"+strconv.Itoa(i)] = watchdogMessage(v)
	}
	if err := validateCurve(fanCurve{Label: "x", Points: []curvePoint{{40, 30}, {80, 100}}, AbortC: 99, CooldownS: 60}); err != nil {
		m["curveAbort"] = err.Error()
	}
	fc := defaultFanConfig()
	fc.AbortC = 99
	if err := validateFanConfig(fc); err != nil {
		m["fanAbort"] = err.Error()
	}
	tc := defaultTunerConfig()
	tc.AbortC = 99
	if err := validateTunerConfig(tc); err != nil {
		m["tunerAbort"] = err.Error()
	}
	for i, gl := range oemLogs() {
		m["logFile"+strconv.Itoa(i)] = gl[0]
		m["logHeader"+strconv.Itoa(i)] = logHeader(gl[0], true)
	}
	b, _ := buildSettingsBundle(false)
	m["settingsNotes"] = strings.Join(b.Notes, " | ")
	dir := t.TempDir()
	p := filepath.Join(dir, "product.json")
	os.WriteFile(p, []byte(`{"model":"x"}`), 0o644)
	if err := productOverlayOK(p); err != nil {
		m["overlay"] = err.Error()
	}
	if model.Plan == planBox2V {
		m["box2Ignored"] = box2IgnoredMsg("test")
		if n, ok := box2TunerInfo()["note"].(string); ok {
			m["box2TunerNote"] = n
		}
		m["specSource"] = box2SpecSource
		for _, f := range commissionable["scbox2"] {
			m["commission"+f.Key] = f.Label
		}
	}
	return m
}

func TestOEMBox2GoMessages(t *testing.T) {
	useBox2(t)
	msgs := oemGoMessages(t)
	for _, k := range []string{"backendErr", "watchdog0", "curveAbort", "fanAbort", "logFile0", "box2Ignored", "box2TunerNote"} {
		if msgs[k] == "" {
			t.Errorf("Box II message %s not produced", k)
		}
	}
	for k, v := range msgs {
		if oemWord.MatchString(v) {
			t.Errorf("Box II %s names the manufacturer: %q", k, v)
		}
	}
	if !strings.Contains(msgs["backendErr"], "(the OEM's minerd)") || !strings.HasPrefix(msgs["watchdog0"], "The OEM watchdog stops mining") ||
		msgs["logFile2"] != "oem-backend-minerd.txt" || !strings.Contains(msgs["curveAbort"], "(the OEM's own over-temperature control") {
		t.Errorf("Box II wording: %q / %q / %q / %q", msgs["backendErr"], msgs["watchdog0"], msgs["logFile2"], msgs["curveAbort"])
	}
	// /api/model tells the page
	if !strings.Contains(oemModelReply(t), `"oem":"OEM"`) {
		t.Errorf("/api/model on the Box II carries no oem field: %s", oemModelReply(t))
	}
}

func oemModelReply(t *testing.T) string {
	t.Helper()
	rec := httptest.NewRecorder()
	handleModel(rec, httptest.NewRequest("GET", "/api/model", nil))
	return rec.Body.String()
}

// The SC Lite model never names the manufacturer either: the same messages, in its wording.
func TestOEMSCLiteMessages(t *testing.T) {
	old := model
	model = profSCLite
	setWindowsFromModel()
	t.Cleanup(func() { model = old; setWindowsFromModel() })
	msgs := oemGoMessages(t)
	for k, v := range msgs {
		if oemWord.MatchString(v) {
			t.Errorf("SC Lite %s names the manufacturer: %q", k, v)
		}
	}
	for k, want := range map[string]string{
		"backendErr": "the miner's backend (the OEM's minerd) didn't answer: ",
		"watchdog0":  "The OEM watchdog stops mining while a fan looks failed, and flags fan 2. It stopped the mining program 2 times in the last 10 minutes.",
		"curveAbort": "safety temperature must be 70–90 °C (the OEM's own over-temperature control takes over at 92 °C)",
		"fanAbort":   "abort temperature must be 70–90 °C (the OEM's own over-temperature control takes over at 92 °C)",
		"logFile0":   "oem-watchdog-xminerd.txt", "logFile1": "oem-fans-fanctrl.txt", "logFile2": "oem-backend-minerd.txt",
		"logHeader0":    "# SC Lite — oem-watchdog-xminerd.txt\n",
		"settingsNotes": "Pools are stored in the OEM's own miner config, not the panel",
		"oemFanCtl":     "the OEM fan control", "oemOwnCurly": "the OEM’s",
	} {
		if !strings.HasPrefix(msgs[k], want) {
			t.Errorf("SC Lite %s:\n got %q\nwant %q…", k, msgs[k], want)
		}
	}
	if !strings.Contains(oemModelReply(t), `"oem":"OEM"`) {
		t.Errorf("/api/model on the SC Lite carries no oem field: %s", oemModelReply(t))
	}
}

// The only Go string literals that name the manufacturer are the stock product.json model strings, which the
// panel matches and never shows (stockModelLabel gives their neutral form).
func TestOEMGoLiteralsAllowed(t *testing.T) {
	allowed := map[string]bool{stockModelSCLite: true, stockModelSCBox2: true}
	files, _ := filepath.Glob("*.go")
	fs := token.NewFileSet()
	var bad []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(fs, f, nil, parser.ParseComments)
		if err != nil {
			t.Fatal(err)
		}
		for _, cg := range af.Comments {
			if oemWord.MatchString(cg.Text()) {
				bad = append(bad, fs.Position(cg.Pos()).String()+": a comment")
			}
		}
		ast.Inspect(af, func(n ast.Node) bool {
			bl, ok := n.(*ast.BasicLit)
			if !ok || bl.Kind != token.STRING {
				return true
			}
			s, err := strconv.Unquote(bl.Value)
			if err != nil || !oemWord.MatchString(s) || allowed[s] {
				return true
			}
			if len(s) > 2000 {
				s = s[:2000] + "…"
			}
			bad = append(bad, fs.Position(bl.Pos()).String()+": "+s)
			return true
		})
	}
	sort.Strings(bad)
	for _, b := range bad {
		t.Errorf("names the manufacturer (use oemName/oemOf/oemPoss, or say OEM): %s", b)
	}
	for raw, want := range map[string]string{stockModelSCLite: "SC Lite", stockModelSCBox2: "SC Box II",
		stockVendorPrefix + "KDBOX": "KDBOX", "Other-X": "Other-X"} {
		if got := stockModelLabel(raw); got != want || oemWord.MatchString(got) {
			t.Errorf("stockModelLabel(%q) = %q, want %q", raw, got, want)
		}
	} // a stock program's log line as the panel streams or exports it
	line := "[2026-11-09 16:59:22] " + stockVendorPrefix + "sb2  Miner detected, 1 chains totally!"
	if got := stockTextShown(line); got != "[2026-11-09 16:59:22] OEM-sb2  Miner detected, 1 chains totally!" {
		t.Errorf("stockTextShown: %q", got)
	}
	if got := stockTextShown(strings.ToUpper(line)); oemWord.MatchString(got) {
		t.Errorf("stockTextShown (upper case): %q", got)
	}
}

// ---- the page ----

// oemViewsScript renders the Box II views that named the manufacturer; V_<name> lines carry the text and C_<n>
// lines every confirm() the user would read.
const oemViewsScript = `
const BOX_V_MEAS={42:7.67,43:7.61,44:7.58,45:7.55,46:7.52,61:7.03};
const C=[];globalThis.confirm=m=>{C.push(m);return false;};globalThis.toast=()=>{};globalThis.loadFanCheck=()=>{};
globalThis.document={createElement:()=>mkEl('x'),body:{appendChild(){}},getElementById:id=>E[id]||null,querySelectorAll:()=>[],querySelector:()=>null};
async function jpost(){return {j:{ok:true}};}
const V={};const out=(k,h)=>{V[k]=String(h);};
(async()=>{
out('overtemp',overTempHTML({over_temp:{at:1700000000,limit:86}}));
out('powertile',powerTileBox2({watts:400,amps:3.3,mains_v:120,calibrated:false},{ghs:1900},1900,(a,b,c)=>a+' '+b+' '+c));
out('pwsrc',pwSrcText({source:'spec',spec_tol_pct:5}));
renderPowerBox2({ok:true,calibrated:false,mains_v:230,watts:400,mhz:690,vcode:42,source:'spec',spec_tol_pct:5,
 specs:[{mode:'Hashrate',mhz:690,v:42,watts:400,ths:1.9},{mode:'Low-power',mhz:525,v:61,watts:260,ths:1.45}],cals:[]});out('power',$('powerbody').innerHTML);
renderFanCheck({ok:true,maxrpm:[2200,2200,2200,2200],sweep:[],suggest:[1800,1800,1800,1800],watchdog:{fans:[2000,2000,2000,2000],status:0},lock:{}});
out('fancheck',$('fcbody').innerHTML+' | '+$('fcnote').textContent);
fanLockUI({fan_lock:{at:1700000000,flagged:'the watchdog flags fan 2',fans:[2000,2000,0,2000],idle:true}});out('fanlock',$('fanlockdlg').innerHTML);
renderFanPair({ok:true,offset:0,min:-20,max:20});out('fanpair',$('fpbody').innerHTML);
out('disclaimer',discHTML(''));
renderCeiling({done:true,axes:[{name:'freq',label:'Clock',state:'done',clamped:false,hard_max:800,unit:'MHz',safe_max:690,cap:800}]});out('ceiling',$('ceilbox').innerHTML);
cv={points:[{temp:40,fan:30},{temp:80,fan:100}],abort_c:99,cooldown_s:60};out('curve',cvValidate().join(' '));
out('f4',b2F4Note());
if(window._MODEL)for(const f of [{SoftIdle:true},{SoftIdle:false}]){const m=Object.assign({},window._MODEL,{public:false,features:Object.assign({},window._MODEL.features,f)});
 $('modelbanner').classList.remove=()=>{};renderModelBanner(m);out('banner'+f.SoftIdle,$('modelbanner').innerHTML);}
renderRecoveryBox2({ok:true,watchdog_running:false,keep_log:true});out('recovery',$('recbody').innerHTML);
PR=box2PR('stock settings');renderPresets();out('presets',$('pcards').innerHTML);
if(!window._MODEL){PR={ok:true,in_charge:'',presets:[],stock:{mhz:625,mv:9100,pv:9400}};renderPresets();out('presetsLite',$('pcards').innerHTML);}
window._fc={suggest:[1800,1800],maxrpm:[2200,2200]};
await fanCheckFit();await fanTestRerun();await fanLockSet(true);await softRestart();await ctEnable(true);await ctEnable(false);
await clearPowerCalBox2(null);await clearPowerCalBox2(690,42);
PR={stock:{mhz:690,mv:42,pv:0},in_charge:''};await presetStock();
C.forEach((c,i)=>out('confirm'+i,c));
for(const k in V)console.log('V_'+k+' '+V[k].replace(/\n/g,' '));
})().catch(e=>console.log('THROWN '+e.stack));
`

var oemViewFuncs = []string{"overTempHTML", "powerTileBox2", "pwSrcText", "pwSetting", "boxVoltsTxt", "renderPowerBox2", "b2Name", "imgMore", "imgDot",
	"renderFanCheck", "fanCheckFit", "fanTestRerun", "fanLockSet", "fanLockUI", "renderFanPair", "discHTML", "renderCeiling", "cvValidate",
	"b2F4Note", "renderModelBanner", "modelName", "renderRecoveryBox2", "fmtWhen", "unexpText", "softRestart", "ctEnable",
	"clearPowerCalBox2", "presetStock", "B2T"}

func oemViews(t *testing.T, mdl string) map[string]string {
	t.Helper()
	funcs := append(append([]string{}, oemViewFuncs...), pubPresetFuncs...)
	seen := map[string]bool{}
	var uniq []string
	for _, f := range funcs {
		if !seen[f] {
			seen[f] = true
			uniq = append(uniq, f)
		}
	}
	pre := "let cv=null;const cvPts=p=>((p&&p.points)||[]).map(x=>({temp:+x.temp,fan:+x.fan})).sort((a,b)=>a.temp-b.temp);\n"
	if mdl != "" {
		pre += "window._MODEL=" + mdl + ";\n"
	} else {
		pre += "window._MODEL=null;\n"
	}
	stubs := strings.Replace(pubPresetStubs, "const BOX_V_MEAS={42:7.67,43:7.61,44:7.58,45:7.55,46:7.52,61:7.03};", "", 1)
	out := l31Node(t, uniq, stubs+pre+oemViewsScript)
	if strings.Contains(out, "THROWN") {
		t.Fatalf("node:\n%s", out)
	}
	views := map[string]string{}
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "V_") {
			if k := strings.Index(l, " "); k > 0 {
				views[l[2:k]] = l[k+1:]
			}
		}
	}
	return views
}

// Every Box II view, in the owner's build and the public release: the manufacturer named nowhere a user reads (text,
// tooltips, confirm dialogs).
func TestPublicOEMBox2Views(t *testing.T) {
	for _, mdl := range []string{pubModelOwner, pubModelPublic} {
		views := oemViews(t, mdl)
		for _, v := range []string{"overtemp", "powertile", "power", "fancheck", "fanlock", "disclaimer", "ceiling", "curve", "bannertrue", "bannerfalse", "recovery", "presets", "confirm0", "confirm8"} {
			if _, ok := views[v]; !ok {
				t.Errorf("view %s not rendered: %v", v, views)
			}
		}
		for k, v := range polishViews(t, mdl) {
			views["polish_"+k] = v
		}
		for k, v := range views {
			if oemWord.MatchString(v) {
				t.Errorf("Box II %s names the manufacturer (model %s): %s", k, mdl, v)
			}
		}
		for k, want := range map[string]string{"overtemp": "The OEM firmware shut the hash boards down", "powertile": ">OEM spec</a>",
			"fancheck": "Re-run the OEM fan test", "curve": "(the OEM firmware takes over at ", "pwsrc": "OEM spec ±5 %"} {
			if !strings.Contains(views[k], want) {
				t.Errorf("Box II %s: want %q in %s", k, want, views[k])
			}
		}
	}
}

// The SC Lite renders of the same functions: the same neutral wording, no name.
func TestPublicOEMSCLiteViews(t *testing.T) {
	views := oemViews(t, "")
	for k, v := range views {
		if oemWord.MatchString(v) {
			t.Errorf("SC Lite %s names the manufacturer: %s", k, v)
		}
	}
	for k, want := range map[string]string{"overtemp": "⚠ <b>The OEM firmware shut the hash boards down", "fancheck": "Re-run the OEM fan test</button>",
		"curve": "(the OEM firmware takes over at 92 °C)", "disclaimer": "supported by the OEM (the miner’s manufacturer) or any miner manufacturer.",
		"fanpair": "the tuner and the OEM’s own fan control", "presetsLite": "<div class=\"meta\">The OEM’s factory setting for this miner, read from its firmware."} {
		if !strings.Contains(views[k], want) {
			t.Errorf("SC Lite %s: want %q in %s", k, want, views[k])
		}
	}
}

// oemStaticPage: the served page's fixed HTML (before the script) as applyModel leaves it on the Box II: the
// [data-box2] wording swapped in, the .lite-only spans hidden.
func oemStaticPage() string {
	page := indexPage
	i, j := strings.Index(page, "<body"), strings.Index(page, "<script>")
	h := page[i:j]
	h = regexp.MustCompile(`<span data-box2="([^"]*)">[^<]*</span>`).ReplaceAllString(h, "$1")
	h = regexp.MustCompile(`(?s)<span class="lite-only">.*?</span>`).ReplaceAllString(h, "")
	return h
}

func TestPublicOEMStaticPage(t *testing.T) {
	h := oemStaticPage()
	if !strings.Contains(h, "The OEM watchdog checks every fan") || !strings.Contains(h, "asks the OEM backend to restart") {
		t.Errorf("the Box II static wording is not in the page")
	}
	for _, l := range strings.Split(h, "\n") {
		if oemWord.MatchString(l) {
			t.Errorf("the Box II page's fixed HTML names the manufacturer: %s", l)
		}
	}
	// the SC Lite page shows the same wording
	lite := regexp.MustCompile(`<span data-box2="[^"]*">([^<]*)</span>`).ReplaceAllString(indexHTML, "$1")
	for _, s := range []string{"<p class=\"muted sc-intro\">The OEM watchdog checks every fan", "<b>Restart mining</b> asks the OEM backend to restart"} {
		if !strings.Contains(lite, s) {
			t.Errorf("SC Lite static text: %q missing", s)
		}
	}
	// the page's default before the model loads
	if want := "return (m&&m.oem)||'OEM';}"; !strings.Contains(indexPage, want) {
		t.Errorf("the served page's OEM() default is not %q", want)
	}
}

// The page's source names the manufacturer nowhere: no text, no comment, on any model.
func TestPublicOEMPageSource(t *testing.T) {
	for _, page := range []string{indexHTML, indexPage} {
		for _, l := range strings.Split(page, "\n") {
			if oemWord.MatchString(l) {
				t.Errorf("the page names the manufacturer: %s", strings.TrimSpace(l))
			}
		}
	}
}
