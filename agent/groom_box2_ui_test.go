package main

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
)

// Box II grooming: on a planBox2V profile the panel speaks Goldshell's voltage level (in volts, via boxVoltsTxt),
// never the SC Lite's PV / mV / 9.1–9.6 V supply; the SC Lite page renders exactly as before. These run the
// page's own functions in node (l31Node), with the Controls GET reply each model sends.

var groomCtrlFuncs = []string{"renderControl", "ctrlWarn", "applyControl", "cclamp", "cstep", "lockBar", "hesc",
	"calNote", "estW", "boxVoltsTxt", "B2T", "b2VC", "b2Levels", "b2RuleLevel", "b2VoltCtl", "b2VOpts", "b2F4Note",
	"b2Name", "chargeTxt", "b2ModeAt", "b2SelTxt", "imgDot", "imgMore"}

const groomStubs = `
const BOX_V_MEAS={42:7.67,43:7.61,44:7.58,45:7.55,46:7.52,61:7.03};
globalThis.document={getElementById:id=>E[id]||null,querySelectorAll:()=>[],querySelector:()=>null};
globalThis.confirm=m=>{globalThis._conf=m;return true;};
globalThis.toast=()=>{};globalThis.loadControl=()=>{};globalThis.setTimeout=()=>{};
let SENT=null;async function jpost(u,b){SENT=b;return {j:{ok:true,applied:'x'}};}
let CTRL=null;
const box2Ctrl=vc=>({ok:true,manual:true,select:0,current:{freq_mhz:690,vrun_mv:42,pv_mv:0,fan1:50,fan2:41},
 limits:{freq:[525,690],vrun:[0,0],pv:[0,0],fan:[10,100]},hardlimits:{freq:[525,690],vrun:[0,0],pv:[0,0],fan:[10,100]},
 presets:[{index:0,plan:{freq_mhz:690,vrun_mv:42}},{index:1,plan:{freq_mhz:525,vrun_mv:61}}],
 box2:{clock_control:true,voltage_control:vc,v_codes:[42,61]}});
`

func TestGroomBox2ControlsVoltageLevel(t *testing.T) {
	out := l31Node(t, groomCtrlFuncs, groomStubs+`
window._MODEL={ok:true,id:'scbox2',name:'Box',plan:'box2v',features:{ClockControl:true,PowerEstimate:false}};
(async()=>{
CTRL=box2Ctrl(false);renderControl(CTRL);
console.log('BODY '+$('ctrlbody').innerHTML);
console.log('NOTE '+$('ctrlnote').textContent);
$('c_freq').value='525';$('c_vlvl').value='42';ctrlWarn();
console.log('RULE '+$('c_vlvl').value+' '+$('c_vlvl').disabled);
await applyControl();console.log('SENT_OFF '+JSON.stringify(SENT));console.log('CONF '+globalThis._conf.split('\n')[2]);
CTRL=box2Ctrl(true);renderControl(CTRL);console.log('BODYON '+$('ctrlbody').innerHTML);
$('c_freq').value='600';$('c_vlvl').value='44';$('c_vlvl').disabled=false;ctrlWarn();
await applyControl();console.log('SENT_ON '+JSON.stringify(SENT));console.log('CONFON '+globalThis._conf.split('\n')[2]);
})();`)
	body := between(out, "BODY ", "\nNOTE ")
	for _, want := range []string{"<label>Voltage</label>", `<select id="c_vlvl"`, ` disabled>`, `<option value="42" selected>7.67 V</option>`,
		`<option value="61">7.03 V</option>`, `<option value="50">≈ 7.39 V</option>`, "range <b>7.03 V</b>–<b>7.67 V</b>", "Voltage control is off",
		"20 fixed voltage steps between 7.03 V and 7.67 V", "voltage-trim setting, not a fan speed"} {
		if !strings.Contains(body, want) {
			t.Errorf("Box II Controls lack %q:\n%s", want, body)
		}
	}
	// highest voltage first: the first option is level 42 (7.67 V), the last level 61 (7.03 V)
	if i, j := strings.Index(body, `value="42"`), strings.Index(body, `value="61"`); i < 0 || j < i {
		t.Errorf("Box II voltage options not highest-first:\n%s", body)
	}
	for _, bad := range []string{"PV", "mV", "Run voltage", "9.1 V", "9100", "c_follow"} {
		if strings.Contains(body, bad) {
			t.Errorf("Box II Controls still show the SC Lite's %q:\n%s", bad, body)
		}
	}
	groomNoLevel(t, "Box II Controls", body)
	groomNoLevel(t, "Box II Controls (voltage control on)", between(out, "BODYON ", "\nSENT_ON "))
	if note := between(out, "NOTE ", "\nRULE "); !strings.Contains(note, "690 MHz · 7.67 V") || strings.Contains(note, "mV") {
		t.Errorf("Box II Controls header: %q", note)
	} else {
		groomNoLevel(t, "Box II Controls header", note)
	}
	if !strings.Contains(out, "RULE 61 true") {
		t.Errorf("with voltage control off the voltage must follow the clock (525 MHz → 7.03 V) and stay greyed out: %s", out)
	}
	// voltage control off: the same request as before (vrun 0 = the clock's stock level, no PV)
	if !strings.Contains(out, `SENT_OFF {"freq_mhz":525,"vrun_mv":0,"pv_mv":0,"confirm":true,"override":false}`) {
		t.Errorf("Box II Apply (voltage control off) changed: %s", out)
	}
	if !strings.Contains(out, "CONF 525 MHz · 7.03 V (follows the clock) (fans unchanged)") {
		t.Errorf("Box II Apply confirm: %s", out)
	}
	// picking 7.58 V sends level 44
	if !strings.Contains(out, `SENT_ON {"freq_mhz":600,"vrun_mv":44,"pv_mv":0,"confirm":true,"override":false}`) {
		t.Errorf("Box II Apply (voltage control on) must send the level of the voltage picked: %s", out)
	}
	if !strings.Contains(out, "CONFON 600 MHz · 7.58 V (fans unchanged)") {
		t.Errorf("Box II Apply confirm (voltage control on): %s", out)
	}
}

// groomNoLevel: a Box II text never shows Goldshell's level: no "level" word and no bare 42–61 shown as a
// voltage (option values and other attributes aside).
func groomNoLevel(t *testing.T, what, html string) {
	t.Helper()
	txt := regexp.MustCompile(`<[^>]*>`).ReplaceAllString(html, " ")
	if strings.Contains(strings.ToLower(txt), "level") || strings.Contains(strings.ToLower(txt), "v-code") {
		t.Errorf("%s shows a level:\n%s", what, txt)
	}
	if m := regexp.MustCompile(`(?:^|[^.\d−+-])(4[2-9]|5\d|6[01])(?:\s*(?:V\b|\)|/|–|-)|\s*$)`).FindString(txt); m != "" {
		t.Errorf("%s shows a bare level %q:\n%s", what, m, txt)
	}
}

func TestGroomSCLiteControlsUnchanged(t *testing.T) {
	out := l31Node(t, groomCtrlFuncs, groomStubs+`
(async()=>{
CTRL={ok:true,manual:true,select:0,current:{freq_mhz:625,vrun_mv:9100,pv_mv:9400,fan1:50,fan2:50},
 limits:{freq:[300,700],vrun:[8800,9300],pv:[9100,9600],fan:[10,100]},hardlimits:{freq:[300,900],vrun:[8800,9300],pv:[9100,9600],fan:[10,100]},presets:[]};
renderControl(CTRL);console.log('BODY '+$('ctrlbody').innerHTML);console.log('NOTE '+$('ctrlnote').textContent);
$('c_freq').value='625';$('c_vrun').value='9100';$('c_pv').value='9400';
await applyControl();console.log('SENT '+JSON.stringify(SENT));
})();`)
	for _, want := range []string{"Board voltage (PV)", "Run voltage", "Keep run voltage at PV − 300", "<b>9.1 V and 9.6 V</b>",
		"raise the max clock past the safe window (the voltage range is the supply’s own: PV 9100–9600)",
		"NOTE manual · 625 MHz · PV 9400 mV (run 9100 mV)",
		`SENT {"freq_mhz":625,"vrun_mv":9100,"pv_mv":9400,"confirm":true,"override":false}`} {
		if !strings.Contains(out, want) {
			t.Errorf("SC Lite Controls lost %q:\n%s", want, out)
		}
	}
	for _, bad := range []string{"Voltage level", "c_vlvl", "voltage-trim", "fixed voltage steps"} {
		if strings.Contains(out, bad) {
			t.Errorf("SC Lite Controls show Box II wording %q:\n%s", bad, out)
		}
	}
}

// The tuner's texts on Box II: no "This SC Lite", no PV / mV columns or figures.
func TestGroomBox2TunerTexts(t *testing.T) {
	out := l31Node(t, []string{"B2T", "boxVoltsTxt", "planTxt", "tcfTxt", "tunerResultsTable", "tunerResultsBox2", "calNote", "estW", "hesc", "scSub"}, groomStubs+`
window._MODEL={ok:true,id:'scbox2',name:'Box',plan:'box2v',features:{}};
console.log('PLAN '+planTxt(690,42,0));
console.log('TCF '+tcfTxt({mhz:690,pv:342,phase:'baseline'}));
console.log('SC '+scSub({mhz:525,mv:61}));
console.log('RES '+tunerResultsTable([{time:'10:00:00',mhz:690,mv:42,pv:0,result:'clean',ths:2.1,max_t:70,min_s:10,err_add:1}],0));`)
	for _, want := range []string{"PLAN 690 MHz · 7.67 V\n", "TCF the baseline (690 MHz)", "SC 525 MHz · 7.03 V\n", "<td>7.67 V</td>"} {
		if !strings.Contains(out, want) {
			t.Errorf("Box II tuner text lacks %q:\n%s", want, out)
		}
	}
	for _, bad := range []string{"PV", "mV", "J/TH", "≈ W", "342", "level"} {
		if strings.Contains(out, bad) {
			t.Errorf("Box II tuner text shows %q:\n%s", bad, out)
		}
	}
}

func between(s, a, b string) string {
	i := strings.Index(s, a)
	if i < 0 {
		return ""
	}
	s = s[i+len(a):]
	if j := strings.Index(s, b); j >= 0 {
		return s[:j]
	}
	return s
}

// The tuner form and its review on Box II: every voltage is picked in volts (the value sent is the level),
// "Highest voltage" feeds min_mv (the level-42 end) and "Lowest voltage" max_mv (the level-61 end), the step is
// a voltage step; nothing shows a level. The SC Lite form keeps its PV fields.
var groomTunerFuncs = []string{"renderTunerForm", "tunerWizUI", "modeVal", "baseVal", "fanVal", "updateEstimate", "estHours",
	"tunerCfg", "TGAP", "B2T", "boxVoltsTxt", "b2VOpts", "b2StepOpts", "b2StepTxt", "tB2Hint", "tTrimCost", "hesc", "modelName",
	"tunerReview", "planParamsHtml", "tB2Note", "imgDot", "imgMore", "b2Split", "b2More"}

const groomTunerData = `
const tdef={mode:'map',baseline_src:'stock',baseline_mhz:690,baseline_mv:42,baseline_min:60,lowest_mhz:525,highest_mhz:690,step:12.5,
 min_mv:42,max_mv:61,mv_step:1,confirm_mhz:690,confirm_mv:42,confirm_hours:4,hold_min:25,settle_min:3,sigma:2.5,abort_c:85,
 fan_hold:false,fan_target_c:70,fan_min_pct:30,preset_min:20,warmest_c:75,dry_run:true};
let TUNERDEF=null;
$('t_reviewbox').scrollIntoView=()=>{};
`

func TestGroomBox2TunerForm(t *testing.T) {
	out := l31Node(t, groomTunerFuncs, groomStubs+groomTunerData+`
window._MODEL={ok:true,id:'scbox2',name:'Box',plan:'box2v',features:{},chips_per_board:36};
const d=Object.assign({},tdef,{_box2:{vcode_min:42,vcode_max:61,note:'n'},_stock:{mhz:690,mv:42},pv_gap:0});
TUNERDEF=d;renderTunerForm(d);
console.log('FORM '+$('tunerbody').innerHTML);
// the owner picks: highest 7.58 V (level 44), lowest ≈ 7.39 V (level 50), a step of ≈ 0.07 V (2 levels), confirm 7.61 V (43)
$('t_low').value='525';$('t_high').value='690';$('t_step').value='12.5';$('t_minmv').value='44';$('t_maxmv').value='50';$('t_mvstep').value='2';
$('t_cmhz').value='650';$('t_cmv').value='43';$('t_chours').value='4';$('t_basemv').value='42';$('t_basemhz').value='690';$('t_base').value='60';
$('t_hold').value='25';$('t_sigma').value='2.5';$('t_abort').value='85';
const c=tunerCfg();console.log('CFG '+JSON.stringify({min_mv:c.min_mv,max_mv:c.max_mv,mv_step:c.mv_step,confirm_mv:c.confirm_mv,baseline_mv:c.baseline_mv}));
tunerReview();console.log('REVIEW '+$('t_reviewbox').innerHTML);
console.log('PARAMS '+planParamsHtml({mode:'map',cfg:Object.assign({},c,{min_mv:42,max_mv:61,mv_step:1}),base:{mhz:690,mv:42}}));
console.log('PCONF '+planParamsHtml({mode:'confirm',cfg:Object.assign({},c,{mode:'confirm',confirm_mhz:650,confirm_mv:43}),base:{mhz:690,mv:42}}));
console.log('END');`)
	form := between(out, "FORM ", "\nCFG ")
	for _, want := range []string{`<label>Highest voltage<select id="t_minmv"`, `<label>Lowest voltage<select id="t_maxmv"`,
		`<label>Voltage step<select id="t_mvstep"`, `<option value="1" selected>≈ 0.03 V per step</option>`,
		`<label>Voltage<select id="t_cmv"`, `Baseline voltage<select id="t_basemv"`, `<option value="42" selected>7.67 V</option>`,
		"20 fixed voltage steps between <b>7.03 V</b> and <b>7.67 V</b>", "the lowest clock starts at <i>Lowest voltage</i>", "(up to <i>Highest voltage</i>)"} {
		if !strings.Contains(form, want) {
			t.Errorf("Box II tuner form lacks %q:\n%s", want, form)
		}
	}
	// the "Lowest voltage" select has level 61 (7.03 V) chosen, the "Highest voltage" one level 42
	if lo := between(form, `id="t_maxmv"`, "</select>"); !strings.Contains(lo, `<option value="61" selected>7.03 V</option>`) {
		t.Errorf("Lowest voltage must default to 7.03 V (level 61): %s", lo)
	}
	for _, bad := range []string{"Lowest PV", "Highest PV", "PV per step", "Levels per step", "V-code", "t_cpv"} {
		if strings.Contains(form, bad) {
			t.Errorf("Box II tuner form shows %q", bad)
		}
	}
	groomNoLevel(t, "Box II tuner form", form)
	if !strings.Contains(out, `CFG {"min_mv":44,"max_mv":50,"mv_step":2,"confirm_mv":43,"baseline_mv":42}`) {
		t.Errorf("the form must send levels (and the step as a number of levels): %s", out)
	}
	rv := between(out, "REVIEW ", "\nPARAMS ")
	if !strings.Contains(rv, "the lowest clean voltage between ≈ 7.39 V and 7.58 V (steps of ≈ 0.07 V)") {
		t.Errorf("Box II review: %s", rv)
	}
	groomNoLevel(t, "Box II tuner review", rv)
	pm := between(out, "PARAMS ", "\nPCONF ")
	if !strings.Contains(pm, "7.03 V – 7.67 V in steps of ≈ 0.03 V") || !strings.Contains(pm, "690 MHz · 7.67 V") {
		t.Errorf("Box II parameter rows: %s", pm)
	}
	groomNoLevel(t, "Box II parameter rows", pm)
	if pc := between(out, "PCONF ", "\nEND"); !strings.Contains(pc, "650 MHz · 7.61 V for 4 h") {
		t.Errorf("Box II confirm row: %s", pc)
	} else {
		groomNoLevel(t, "Box II confirm row", pc)
	}
}

func TestGroomSCLiteTunerFormUnchanged(t *testing.T) {
	out := l31Node(t, groomTunerFuncs, groomStubs+groomTunerData+`
const d=Object.assign({},tdef,{min_mv:8800,max_mv:9300,mv_step:50,confirm_mv:9100,baseline_mv:9100,_stock:{mhz:625,mv:9100,pv:9400},pv_gap:300});
TUNERDEF=d;renderTunerForm(d);console.log('FORM '+$('tunerbody').innerHTML);
$('t_minmv').value='9100';$('t_maxmv').value='9600';$('t_mvstep').value='50';$('t_low').value='575';$('t_high').value='675';$('t_step').value='25';
tunerReview();console.log('REVIEW '+$('t_reviewbox').innerHTML);`)
	for _, want := range []string{`<label>Lowest PV<input type="number" id="t_minmv" value="9100"`, `<label>Highest PV<input type="number" id="t_maxmv" value="9600"`,
		`<label>PV per step (mV)<input type="number" id="t_mvstep" value="50"`, "<b>Voltage is PV</b>", "<label>mV sent along",
		"the lowest clock starts at <i>Lowest PV</i>", "between PV 9100 and PV 9600 (50 mV steps)"} {
		if !strings.Contains(out, want) {
			t.Errorf("SC Lite tuner form lost %q", want)
		}
	}
	for _, bad := range []string{"Voltage step", "Highest voltage", "Lowest voltage", " V per step", "fixed voltage steps"} {
		if strings.Contains(out, bad) {
			t.Errorf("SC Lite tuner form shows Box II wording %q", bad)
		}
	}
}

// The preset form on Box II: "Voltage", a choice of volts; saving 7.58 V sends level 44.
func TestGroomBox2PresetForm(t *testing.T) {
	out := l31Node(t, []string{"pfBox2", "B2T", "boxVoltsTxt", "b2VOpts", "presetFormSave", "presetFormCancel"}, groomStubs+`
window._MODEL={ok:true,id:'scbox2',name:'Box',plan:'box2v',features:{}};
const lab={firstChild:{nodeType:3,textContent:'mV'}};$('pfMv').parentNode=lab;
['pfPv','pfPvAuto'].forEach(id=>{$(id).parentNode={classList:{add(c){console.log('HIDE '+id+' '+c);}}};});
pfBox2();console.log('LABEL '+lab.firstChild.textContent);console.log('SEL '+$('pfMv').outerHTML);
window._presets=[];globalThis.loadPresets=()=>{};var pfKey='';
$('pfLabel').value='Quiet';$('pfMhz').value='600';$('pfMv').value='44';$('pfFan').value='none';
(async()=>{await presetFormSave();console.log('SENT '+JSON.stringify(SENT));})();`)
	if !strings.Contains(out, "LABEL Voltage\n") || !strings.Contains(out, "HIDE pfPv hide") || !strings.Contains(out, "HIDE pfPvAuto hide") {
		t.Errorf("Box II preset form label / PV fields: %s", out)
	}
	sel := between(out, "SEL ", "\nSENT ")
	if !strings.Contains(sel, `<select id="pfMv"`) || !strings.Contains(sel, `<option value="44">7.58 V</option>`) || !strings.Contains(sel, `<option value="61">7.03 V</option>`) {
		t.Errorf("Box II preset voltage choice: %s", sel)
	}
	groomNoLevel(t, "Box II preset form", sel)
	if !strings.Contains(out, `SENT {"action":"save","name":"Quiet","mhz":600,"mv":44,"pv":0,`) {
		t.Errorf("Box II preset save must send the level of the voltage picked: %s", out)
	}
}

// The Dashboard's board card voltage line (and its tooltip) on Box II: volts only.
func TestGroomBox2BoardVoltageLine(t *testing.T) {
	const head = `'<div class="line"><span class="muted">Voltage</span><span>'+`
	i := strings.Index(indexHTML, head)
	if i < 0 {
		t.Fatal("no board card voltage line in the page")
	}
	rest := indexHTML[i:]
	j := strings.Index(rest, `+'</span></div>'`)
	if j < 0 {
		t.Fatal("board card voltage line: no end")
	}
	expr := rest[:j+len(`+'</span></div>'`)]
	out := l31Node(t, []string{"boxVoltsTxt", "B2T"}, groomStubs+`
const pr=(b,d)=>`+expr+`;
window._MODEL={ok:true,id:'scbox2',name:'Box',plan:'box2v',features:{}};
console.log('B2 '+pr({volt_level:44,alive:true},{}));console.log('B2E '+pr({volt_level:53,alive:true},{}));
window._MODEL=null;console.log('SCL '+pr({voltage_v:9.31,alive:true},{power:{}}));`)
	b2 := between(out, "B2 ", "\nSCL ")
	if !strings.Contains(b2, ">7.58 V</span>") || !strings.Contains(b2, ">≈ 7.29 V</span>") || !strings.Contains(b2, "7.67 V on Stock High, 7.03 V on Stock Low") {
		t.Errorf("Box II board voltage line: %s", b2)
	}
	if strings.Contains(strings.ToLower(b2), "level") || strings.Contains(b2, "42") || strings.Contains(b2, "61") || strings.Contains(b2, ">44<") {
		t.Errorf("Box II board voltage line (or its tooltip) shows a level: %s", b2)
	}
	if !strings.Contains(out, "SCL <div class=\"line\"><span class=\"muted\">Voltage</span><span>9.31 V</span></div>") {
		t.Errorf("SC Lite board voltage line changed: %s", out)
	}
}

// The page's boxVoltsTxt / b2StepTxt and the Go helpers give the same text for every level and step.
func TestGroomBox2VoltsJSMatchesGo(t *testing.T) {
	out := l31Node(t, []string{"boxVoltsTxt", "b2StepTxt"}, groomStubs+`
for(let l=42;l<=61;l++)console.log('L'+l+'='+boxVoltsTxt(l));
for(let n=1;n<=20;n++)console.log('S'+n+'='+b2StepTxt(n));`)
	for l := 42; l <= 61; l++ {
		if want := fmt.Sprintf("L%d=%s\n", l, box2LevelText(l)); !strings.Contains(out, want) {
			t.Errorf("level %d: Go says %q, the page:\n%s", l, want, out)
		}
	}
	for n := 1; n <= 20; n++ {
		if want := fmt.Sprintf("S%d=%s\n", n, box2VoltStepText(n)); !strings.Contains(out, want) {
			t.Errorf("step %d: Go says %q, the page:\n%s", n, want, out)
		}
	}
	// the measured levels are exact, the others estimates
	for l, want := range map[int]string{42: "7.67 V", 43: "7.61 V", 44: "7.58 V", 45: "7.55 V", 46: "7.52 V", 61: "7.03 V", 47: "≈ 7.49 V", 60: "≈ 7.06 V"} {
		if got := box2LevelText(l); got != want {
			t.Errorf("box2LevelText(%d) = %q, want %q", l, got, want)
		}
	}
}

// No Box II message the owner reads, as the helpers produce them, shows Goldshell's level.
func TestGroomBox2MessagesSayVolts(t *testing.T) {
	b10TempAuth(t)
	p := profSCBox2
	b10SetModel(t, p)
	var msgs []string
	add := func(s string) { msgs = append(msgs, s) }
	adde := func(err error) {
		if err != nil {
			add(err.Error())
		}
	}
	for l := 42; l <= 61; l++ {
		add(box2LevelText(l))
	}
	add(box2VoltRangeText(42, 61))
	add(box2VoltStepText(3))
	add(box2StockPlansText())
	add(box2VRuleText())
	add(planShow(powerplan{FreqMHz: 690, VrunMv: 42, Fan1: 50, Fan2: 41}))
	add(planShowRaw("690 MHz 42 V 50 RPM 41 RPM"))
	add(box2ReadText(42.0))
	add(box2ReadText(42.5))
	adde(box2CheckTuple(600, 50)) // stock plans only
	p.Feat.ClockControl = true
	b10SetModel(t, p)
	adde(box2CheckTuple(600, 50)) // voltage control off: the rule
	p.Feat.VoltageControl, p.Feat.Tuner = true, true
	b10SetModel(t, p)
	adde(box2CheckTuple(600, 70)) // outside the range
	if s, _ := box2TunerInfo()["note"].(string); s != "" {
		add(s)
	}
	c := defaultTunerConfig()
	add(c.vText(box2LevelOf(50)))
	add(c.planText())
	add((planMV{MHz: 650, MV: box2LevelOf(44)}).text())
	for _, bad := range []tunerConfig{
		func() tunerConfig { x := c; x.MinMV, x.MaxMV = 61, 42; return x }(),
		func() tunerConfig { x := c; x.MVStep = 40; return x }(),
		func() tunerConfig { x := c; x.Mode, x.ConfirmMV = "confirm", 70; return x }(),
		func() tunerConfig { x := c; x.BaselineMV = 50; return x }(),
	} {
		err := validateTunerConfigBox2(bad)
		if err == nil {
			t.Errorf("config %+v accepted", bad)
			continue
		}
		adde(err)
	}
	if len(msgs) < 30 {
		t.Fatalf("only %d messages", len(msgs))
	}
	lv := regexp.MustCompile(`(?:^|[^.\d])(4[2-9]|5\d|6[01])(?:\s*(?:V\b|\)|/)|$)`)
	for _, m := range msgs {
		if strings.Contains(strings.ToLower(m), "level") || strings.Contains(m, "V-code") || strings.Contains(m, "code ") || lv.MatchString(m) {
			t.Errorf("a Box II message shows a level: %q", m)
		}
	}
	// a refusal about the allowed range gives it in volts
	if err := box2CheckTuple(600, 70); err == nil || !strings.Contains(err.Error(), "between 7.03 V and 7.67 V") {
		t.Errorf("range refusal: %v", err)
	}
}
