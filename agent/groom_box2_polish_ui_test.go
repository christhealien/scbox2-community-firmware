package main

import (
	"regexp"
	"strings"
	"testing"
)

// The SC Box II pages' long texts (owner's polish pass): each section keeps its heading, a one-line summary and its
// controls in view; how-it-works text, tips and background fold into a closed drop-down (imgMore, as on the update
// screen), with the tuner's dot where it matters (red: safety, yellow: needs you). The SC Lite page is unchanged.
// The page's own functions run in node (l31Node), as in groom_box2_ui_test.go.

var polishFuncs = []string{"renderControl", "ctrlWarn", "cclamp", "cstep", "lockBar", "hesc", "calNote", "estW", "boxVoltsTxt", "B2T",
	"b2VC", "b2Levels", "b2RuleLevel", "b2VoltCtl", "b2VOpts", "b2F4Note", "b2Name", "chargeTxt", "b2ModeAt", "b2SelTxt",
	"imgDot", "imgMore", "b2Split", "b2More", "b2Fold", "b2Wrap", "b2Folds",
	"renderAutoFan", "fanStatus", "fanUnit", "curveOptions",
	"renderRecovery", "renderRecoveryBox2", "fmtWhen", "unexpText",
	"renderPowerBox2", "pwSetting", "pwSrcText", "modelName", "renderModelBanner", "MCUTOFF",
	"renderDisplay", "fmtShareDisp", "siShareJS", "sshBlock", "ctOpen", "ctTog", "ctWatchHTML", "ctStrongHTML",
	"renderTunerForm", "tunerWizUI", "modeVal", "baseVal", "fanVal", "updateEstimate", "estHours", "tunerCfg", "TGAP",
	"b2StepOpts", "b2StepTxt", "tB2Hint", "tTrimCost", "tB2Note"}

// polishScript renders every changed section; V_<name> lines carry the HTML. Run with window._MODEL set (Box II) or not (SC Lite).
const polishScript = `
const BOX_V_MEAS={42:7.67,43:7.61,44:7.58,45:7.55,46:7.52,61:7.03};
globalThis.confirm=()=>true;globalThis.toast=()=>{};globalThis.loadControl=()=>{};globalThis.setTimeout=()=>0;globalThis.clearTimeout=()=>{};
globalThis.loadMAC=()=>{};globalThis.cvOpen=()=>{};globalThis.loadChipTrim=()=>{};
async function jpost(){return {j:{ok:true}};}
let CTRL=null,FAN=null;const SHARELBL={};
const V={};const out=(k,h)=>{V[k]=String(h)};
// a small DOM for the page's fixed paragraphs: querySelector by the selectors b2Folds uses
const Q={};globalThis.document={getElementById:id=>E[id]||null,querySelectorAll:()=>[],querySelector:s=>Q[s]||null,
 createElement:()=>({style:{},innerHTML:'',kids:[],appendChild(e){this.kids.push(e);e.parentNode=this;}}),activeElement:null};
const fix=(sel,html)=>{const e=mkEl(sel);e.innerHTML=html;e.parentNode={insertBefore(d,x){x.wrap=d;}};Q[sel]=e;return e;};
const box2=!!window._MODEL;
CTRL=box2?{ok:true,manual:true,select:0,current:{freq_mhz:690,vrun_mv:42,pv_mv:0},limits:{freq:[525,690],vrun:[0,0],pv:[0,0],fan:[10,100]},
 hardlimits:{freq:[525,690],vrun:[0,0],pv:[0,0],fan:[10,100]},presets:[{index:0,plan:{freq_mhz:690,vrun_mv:42}},{index:1,plan:{freq_mhz:525,vrun_mv:61}}],
 box2:{clock_control:true,voltage_control:false,v_codes:[42,61]}}
 :{ok:true,manual:true,select:0,current:{freq_mhz:625,vrun_mv:9100,pv_mv:9400},limits:{freq:[400,700],vrun:[8800,9300],pv:[9100,9600],fan:[10,100]},
 hardlimits:{freq:[400,700],vrun:[8800,9300],pv:[9100,9600],fan:[10,100]},presets:[{index:0,plan:{freq_mhz:625,vrun_mv:9100,pv_mv:9400},raw:'625 9100'}]};
renderControl(CTRL);out('controls',$('ctrlbody').innerHTML);
FAN={config:{enabled:true,profile:'curve-balanced',throttle_enable:false,throttle_c:80,throttle_step:25,resume_c:70},status:{max_temp:61,hot_board:0,fans_rpm:[3000,3000],target:55},
 profiles:{'curve-balanced':{label:'Balanced',builtin:true,mode:'curve'}},in_charge:''};
renderAutoFan();out('autofan',$('fanbody').innerHTML);
renderRecovery({ok:true,watchdog_running:false,keep_log:true,kept_before:1700000000});out('recovery',$('recbody').innerHTML);
renderChipTrim({enabled:false,limits:{min:-50,max:50,step:12.5},geo:{chips:36,pairs:18,box2:box2,fix:'lower the clock'},chains:[]});out('chiptrims',$('ctbody').innerHTML);
renderDisplay({share_unit:'both',hash_unit:'ths'});out('display',$('dispbody').innerHTML);
out('ssh',sshBlock({available:true,enabled:true,listening:true,port:22,username:'admin',fingerprint:'SHA256:x'},'10.0.0.5'));
const tdef={mode:'map',baseline_src:'stock',baseline_mhz:690,baseline_mv:42,baseline_min:60,lowest_mhz:525,highest_mhz:690,step:12.5,
 min_mv:42,max_mv:61,mv_step:1,confirm_mhz:690,confirm_mv:42,confirm_hours:4,hold_min:25,settle_min:3,sigma:2.5,abort_c:84,
 fan_hold:false,fan_target_c:70,fan_min_pct:30,preset_min:20,warmest_c:75,dry_run:true};
var TUNERDEF=box2?Object.assign({},tdef,{_box2:{vcode_min:42,vcode_max:61,note:'On this model the voltage is one of 20 fixed steps. Stock: 690 MHz at 7.67 V, 525 MHz at 7.03 V; only these two are known to be safe. Abort at most 84 °C.'},_stock:{mhz:690,mv:42},pv_gap:0})
 :Object.assign({},tdef,{min_mv:8800,max_mv:9300,mv_step:50,confirm_mv:9100,baseline_mv:9100,_stock:{mhz:625,mv:9100,pv:9400},pv_gap:300});
renderTunerForm(TUNERDEF);out('tuner',$('tunerbody').innerHTML);
if(box2){
 renderPowerBox2({ok:true,calibrated:false,mains_v:100,watts:0,specs:[{mode:'Hashrate',mhz:690,v:42,watts:400,ths:1.9}],cals:[]});out('power',$('powerbody').innerHTML);
 $('modelbanner').classList.remove=()=>{};renderModelBanner(window._MODEL);out('banner',$('modelbanner').innerHTML);
 // the page's fixed paragraphs, as applyModel leaves them (data-box2 wording swapped in)
 fix('#ctrlpanel .warnbar','⚠ Changing clock or voltage can permanently damage the miner. The clock box keeps to the range shown under it. Changes apply live to the running board — no reboot.');
 fix('#cvEdit > .thint','<b>Safety temperature:</b> at or above it (two readings in a row) the fans go to 100 %, whatever the curve says. If a board is still that hot 90 s later, the miner goes to Idle to cool down.');
 fix('#tunerpanel > .warnbar','⚠ Experimental. The tuner changes clock/voltage repeatedly and judges stability from live hardware errors. Start in DRY-RUN (reads chips, writes nothing) and supervise the first real run.');
 fix('#fanpanel .sc-intro','This miner runs the curve picked here. Changes take effect straight away. While auto fan is on, the hottest board sets the fan speed.');
 const sc=mkEl('sc_hint');E.sc_hint=sc;sc.innerHTML='The schedule switches between this miner’s presets (Stock High, Stock Low, your own from Presets, or Idle). Each change puts on the preset’s clock, voltage and fan curve once. Times are the miner’s local time (<span id="sc_tz">—</span>).';
 const rk=fix('#rejsec .rj-key','<span>stale: found just as the pool moved to new work</span>');
 b2Folds();b2Folds(); // a second call changes nothing
 for(const s of ['#ctrlpanel .warnbar','#cvEdit > .thint','#tunerpanel > .warnbar','#fanpanel .sc-intro'])out('static'+s.replace(/[^a-zA-Z]/g,''),Q[s].innerHTML);
 out('schint',sc.innerHTML);out('rjkey',rk.wrap?rk.wrap.innerHTML+' WRAPS '+(rk.wrap.kids[0]===rk):'not wrapped');
}
for(const k in V)console.log('V_'+k+' '+V[k].replace(/\n/g,' '));
`

var polishDetails = regexp.MustCompile(`(?s)<details[^>]*>.*?</details>`)

func polishViews(t *testing.T, model string) map[string]string {
	t.Helper()
	pre := ""
	if model != "" {
		pre = "window._MODEL=" + model + ";"
	}
	out := l31Node(t, polishFuncs, pubHesc+pubCut(t, "renderChipTrim", "\nasync function ctPost(")+pre+polishScript)
	views := map[string]string{}
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "V_") {
			if k := strings.Index(l, " "); k > 0 {
				views[l[2:k]] = l[k+1:]
			}
		}
	}
	if len(views) == 0 {
		t.Fatalf("nothing rendered:\n%s", out)
	}
	return views
}

// polishFold: the summary (visible) part before the drop-down, and the drop-down named label must hold body, closed
func polishFold(t *testing.T, what, h, visible, label, body string) {
	t.Helper()
	vis := polishDetails.ReplaceAllString(h, "")
	if visible != "" && !strings.Contains(vis, visible) {
		t.Errorf("%s: %q must stay in view:\n%s", what, visible, vis)
	}
	if strings.Contains(vis, body) {
		t.Errorf("%s: %q must be folded away:\n%s", what, body, vis)
	}
	for _, d := range polishDetails.FindAllString(h, -1) {
		if strings.Contains(d, label+"</summary>") {
			if !strings.Contains(d, body) {
				t.Errorf("%s: the drop-down %q lacks %q:\n%s", what, label, body, d)
			}
			if regexp.MustCompile(`^<details[^>]* open`).MatchString(d) {
				t.Errorf("%s: the drop-down %q must start closed:\n%s", what, label, d)
			}
			return
		}
	}
	t.Errorf("%s: no drop-down %q:\n%s", what, label, h)
}

const (
	polishRed    = `<b class="st-fail" style="font-size:13px;margin-right:6px">●</b>`
	polishYellow = `<b class="st-run" style="font-size:13px;margin-right:6px">●</b>`
)

func TestPolishBox2LongTextsFold(t *testing.T) {
	for _, m := range []struct{ name, model string }{{"owner", pubModelOwner}, {"public", pubModelPublic}} {
		v := polishViews(t, m.model)
		w := func(k string) string {
			if _, ok := v[k]; !ok {
				t.Fatalf("%s: view %s not rendered: %v", m.name, k, v)
			}
			return v[k]
		}
		name := func(s string) string { return m.name + " " + s }
		// Controls: the voltage-control state with a yellow dot, the steps folded; Apply and the voltage box in view
		c := w("controls")
		polishFold(t, name("controls"), c, polishYellow+"<b>Voltage control is off</b>", "About the voltage steps", "fixed voltage steps between 7.03 V and 7.67 V")
		polishFold(t, name("controls"), c, "leaves the current fan speeds alone.", "The plan’s 4th field", "voltage-trim setting, not a fan speed")
		for _, ctl := range []string{`id="c_freq"`, `<select id="c_vlvl"`, ">Apply</button>"} {
			if !strings.Contains(polishDetails.ReplaceAllString(c, ""), ctl) {
				t.Errorf("%s: control %s must stay in view", name("controls"), ctl)
			}
		}
		polishFold(t, name("auto fan"), w("autofan"), "Changes here take effect straight away.", "Tips", "blake2b-asic-control")
		polishFold(t, name("recovery"), w("recovery"), "it stays dead until the power is cut.", "Why there’s no watchdog", "kernel driver for the watchdog")
		polishFold(t, name("recovery"), w("recovery"), "Saved copy from", "What it keeps", "last 48 KB")
		if !strings.Contains(w("recovery"), `id="reclog"`) || !strings.Contains(w("recovery"), "Keep the system log across restarts") {
			t.Errorf("%s: the switch must stay: %s", name("recovery"), w("recovery"))
		}
		if !strings.Contains(w("chiptrims"), `id="ctHow" data-k="how" ontoggle="ctTog(this)"><summary>`) {
			t.Errorf("%s: “How chip trims work” must start closed: %s", name("chip trims"), w("chiptrims"))
		}
		polishFold(t, name("power"), w("power"), "has no power sensor, and its watts can’t be worked out from the clock and voltage.", "How the figures work", "plug-in wall meter")
		polishFold(t, name("power"), w("power"), "", polishYellow+"How to calibrate", "let it hash at one setting for 10 minutes")
		polishFold(t, name("best share numbers"), w("display"), "How best shares and records are shown", "How the numbers are counted", "units of 2<sup>32</sup>")
		polishFold(t, name("ssh"), w("ssh"), "Signs in with your panel username and password", "Password and wrong tries", "Five wrong passwords")
		// the tuner: the safe-settings sentence in view with a yellow dot; long hints folded, first sentence kept
		tu := w("tuner")
		polishFold(t, name("tuner"), tu, polishYellow+"Stock: 690 MHz at 7.67 V, 525 MHz at 7.03 V; only these two are known to be safe.", "This model’s voltage steps and limits", "Abort at most 84 °C.")
		polishFold(t, name("tuner"), tu, "<b>Minutes per test:</b> how long each setting is watched", "Sensitivity and Abort", "<b>Abort:</b> if any board reaches")
		polishFold(t, name("tuner"), tu, "The baseline runs first", "How the baseline works", "60 min is a good choice")
		for _, ctl := range []string{`id="t_hold"`, `id="t_abort"`, `id="t_dry"`, "Review before starting"} {
			if !strings.Contains(polishDetails.ReplaceAllString(tu, ""), ctl) {
				t.Errorf("%s: control %s must stay in view", name("tuner"), ctl)
			}
		}
		if strings.Contains(tu, `<p class="thint"><details`) {
			t.Errorf("%s: a drop-down must not sit inside a <p> (the browser would close the paragraph)", name("tuner"))
		}
		// the page's fixed paragraphs: safety lines in view with a red dot, details folded; once only
		polishFold(t, name("controls warning"), w("staticctrlpanelwarnbar"), polishRed+"Changing clock or voltage can permanently damage the miner.", "What keeps it in range", "Changes apply live")
		polishFold(t, name("safety temperature"), w("staticcvEditthint"), polishRed+"<b>Safety temperature:</b> at or above it", "If a board stays hot, and the other settings", "goes to Idle")
		polishFold(t, name("tuner warning"), w("statictunerpanelwarnbar"), polishRed+"Experimental. The tuner changes clock/voltage", "Before you start", "Start in DRY-RUN")
		polishFold(t, name("auto fan intro"), w("staticfanpanelscintro"), "This miner runs the curve picked here. Changes take effect straight away.", "How auto fan works", "the hottest board sets")
		polishFold(t, name("schedule hint"), w("schint"), "The schedule switches between this miner’s presets", "How it works", "Each change puts on")
		if strings.Count(w("staticctrlpanelwarnbar"), "<details") != 1 || strings.Contains(w("staticctrlpanelwarnbar"), "⚠") {
			t.Errorf("%s: the controls warning folded once, its ⚠ replaced by the dot: %s", m.name, w("staticctrlpanelwarnbar"))
		}
		if !strings.Contains(w("schint"), `id="sc_tz"`) {
			t.Errorf("%s: the schedule's time zone span must stay: %s", m.name, w("schint"))
		}
		if !strings.Contains(w("rjkey"), "What the marks mean</summary>") || !strings.HasSuffix(w("rjkey"), "WRAPS true") {
			t.Errorf("%s: the rejected-shares key must sit in a drop-down: %s", m.name, w("rjkey"))
		}
		if m.name == "owner" {
			polishFold(t, "owner banner", w("banner"), "<b>Monitoring and pools work normally</b> on this miner", polishYellow+"Still off on this unit, and the heat backstop", "<b>Still off</b> until tested")
		} else if w("banner") != "" {
			t.Errorf("public: no banner: %s", w("banner"))
		}
	}
}

// SC Lite: the same functions render every paragraph whole, with no Box II drop-down or dot.
func TestPolishSCLiteUnchanged(t *testing.T) {
	v := polishViews(t, "")
	for k, h := range v {
		if strings.Contains(h, `<details style="margin-top:6px">`) || strings.Contains(h, "st-run") || strings.Contains(h, "st-fail") {
			t.Errorf("SC Lite %s shows a Box II drop-down or dot:\n%s", k, h)
		}
	}
	for k, want := range map[string]string{
		"autofan":   "Changes here take effect straight away. Turn off your external blake2b-asic-control app’s auto fan",
		"recovery":  "it stays dead until the power is cut. The hardware watchdog that could fix that",
		"display":   "notifications. The miner counts share difficulty in units of 2<sup>32</sup> hashes",
		"ssh":       "(a root shell, plus <code>sftp</code> and <code>scp</code>). Change the password under Account",
		"tuner":     `<p class="thint"><b>Minutes per test:</b> how long each setting is watched (plus 3 minutes to settle). Longer`,
		"chiptrims": `id="ctHow" data-k="how" ontoggle="ctTog(this)" open><summary>`,
	} {
		if !strings.Contains(v[k], want) {
			t.Errorf("SC Lite %s lost %q:\n%s", k, want, v[k])
		}
	}
}
