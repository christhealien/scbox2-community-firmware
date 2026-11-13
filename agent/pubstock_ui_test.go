package main

import (
	"regexp"
	"strings"
	"testing"
)

// The public release's page (PUB() true: /api/model says "public") shows nothing of the owner's per-unit
// commissioning or its tests, and the SC Box II names the OEM's two stock plans "Stock High" (690 MHz, also the
// factory "Stock") and "Stock Low" (525 MHz) in both builds. The SC Lite page is unchanged. The page's own
// functions run in node (l31Node), as in groom_box2_ui_test.go.

// the Box II profiles: the owner's build (commissioning on, features from it) and the public release's fixed set
const (
	pubModelOwner  = `{ok:true,id:'scbox2',name:'Box',plan:'box2v',commission:'x',features:{SoftIdle:true,PowerEstimate:true,ClockControl:true,VoltageControl:true,ChipTrims:true,FirmwareUpdate:true}}`
	pubModelPublic = `{ok:true,id:'scbox2',name:'Box',plan:'box2v',public:true,commission:'x',features:{SoftIdle:true,PowerEstimate:true,ClockControl:true,VoltageControl:true,ChipTrims:true,FirmwareUpdate:true}}`
)

// pubBad: what a public user must never read
var pubBad = regexp.MustCompile(`(?i)commission|hardware test|test C\d|lane C\d|not tested|unproven`)

func pubText(html string) string { return regexp.MustCompile(`<[^>]*>`).ReplaceAllString(html, " ") }

func pubCheckClean(t *testing.T, what, html string) {
	t.Helper()
	if m := pubBad.FindString(html); m != "" {
		t.Errorf("public %s shows %q:\n%s", what, m, html)
	}
}

var pubPresetFuncs = []string{"renderPresets", "pIsOn", "pTHS", "pFanProfiles", "pSpark", "pFanLine", "estW", "vLine", "calNote",
	"fanUnit", "b2Name", "b2Stock", "chargeTxt", "B2T", "boxVoltsTxt"}

// pubHesc: the page's hesc, and esc1 for names without quotes (l31Func's cut of either runs on into the code after it)
const pubHesc = "function hesc(s){return String(s==null?'':s).replace(/[<>&\"']/g,c=>({'<':'&lt;','>':'&gt;','&':'&amp;','\"':'&quot;',\"'\":'&#39;'}[c]));}\nfunction esc1(s){return hesc(String(s));}\n"

// pubCut: the page's code from "\nfunction name(" up to the next marker (for a function l31Func can't brace-count)
func pubCut(t *testing.T, name, next string) string {
	t.Helper()
	i := strings.Index(indexHTML, "\nfunction "+name+"(")
	if i < 0 {
		t.Fatalf("no function %s", name)
	}
	j := strings.Index(indexHTML[i+1:], next)
	if j < 0 {
		t.Fatalf("function %s: no %q after it", name, next)
	}
	return indexHTML[i+1 : i+1+j]
}

const pubPresetStubs = pubHesc + `
const BOX_V_MEAS={42:7.67,43:7.61,44:7.58,45:7.55,46:7.52,61:7.03};
const PSLOTS=[['high','High'],['middle','Middle'],['low','Low'],['lowest','Lowest power']];
const pSlotOf=n=>{const l=String(n||'').toLowerCase();return l==='lowest power'?'lowest':(['high','middle','low','lowest'].includes(l)?l:null);};
let FAN={status:{max_temp:60,fans_rpm:[3000,3000]},config:{enabled:true,profile:'curve-balanced'},profiles:{}};let PR=null;
globalThis.document={getElementById:id=>E[id]||null,querySelectorAll:()=>[],querySelector:()=>null};
const box2PR=lk=>({ok:true,in_charge:lk,box2:true,has_idle:true,ghs:0,
 presets:[{name:'Hashrate',mhz:690,mv:42,pv:0,fan_profile:'none'},{name:'Low-power',mhz:525,mv:61,pv:0,fan_profile:'none'},
  {name:'Quiet',mhz:600,mv:44,pv:0,fan_profile:'none'}],
 stock:{mhz:690,mv:42,pv:0}});
const cardH3=()=>($('pcards').innerHTML.match(/<h3>[^<]*/g)||[]).map(x=>x.slice(4).trim()).join('|');
const active=()=>($('pcards').innerHTML.match(/<div class="pcard active"><h3>[^<]*/g)||[]).map(x=>x.replace(/.*<h3>/,'').trim()).join('|');
`

// The Presets page on Box II: "Stock High" / "Stock Low", no separate "Stock" card (the same plan as Stock High),
// and the Stock High card is the one marked "on the miner" when the factory Stock was applied (schedule, resume).
func TestPublicBox2PresetsStockNames(t *testing.T) {
	for _, mdl := range []string{pubModelOwner, pubModelPublic} {
		out := l31Node(t, pubPresetFuncs, pubPresetStubs+`
window._MODEL=`+mdl+`;
PR=box2PR('preset Hashrate');renderPresets();console.log('H3 '+cardH3());console.log('ON1 '+active()+' / '+$('p_preset').textContent);
PR=box2PR('stock settings');renderPresets();console.log('ON2 '+active()+' / '+$('p_preset').textContent);
PR=box2PR('preset Low-power');renderPresets();console.log('ON3 '+active()+' / '+$('p_preset').textContent);
console.log('HTML '+$('pcards').innerHTML);`)
		pub := strings.Contains(mdl, "public:true")
		if !strings.Contains(out, "H3 Stock High|Stock Low|Quiet|☾ Idle\n") && !strings.Contains(out, "H3 High|Middle|Low|Lowest power|Stock High|Stock Low|Quiet|☾ Idle\n") {
			t.Errorf("Box II preset cards (public %v): %s", pub, out)
		}
		if pub && !strings.Contains(out, "H3 Stock High|Stock Low|Quiet|☾ Idle\n") {
			t.Errorf("public Box II: no empty tuner slots (no tuner): %s", out)
		}
		for _, want := range []string{"ON1 Stock High / Stock High\n", "ON2 Stock High / Stock High\n", "ON3 Stock Low / Stock Low\n",
			"The OEM’s factory setting for this miner", `onclick="presetApply('Hashrate')"`, `onclick="presetApply('Low-power')"`} {
			if !strings.Contains(out, want) {
				t.Errorf("Box II presets (public %v) lack %q:\n%s", pub, want, out)
			}
		}
		html := between(out, "HTML ", "\n")
		for _, bad := range []string{"<h3>Stock<", "<h3>Stock <", "Hashrate<", "Low-power<", "presetStock()", "presetEdit('Hashrate')"} {
			if strings.Contains(html, bad) {
				t.Errorf("Box II presets (public %v) show %q:\n%s", pub, bad, html)
			}
		}
		if pub {
			pubCheckClean(t, "Presets", html)
			if !strings.Contains(html, "Set by hand") {
				t.Errorf("public: the hand-set preset should still say so: %s", html)
			}
		} else if !strings.Contains(html, "Set by hand, not tested") {
			t.Errorf("owner build: the hand-set preset keeps its tag: %s", html)
		}
	}
}

// The SC Lite Presets page: its own names, the Stock card, the tested / not tested tags, as before.
func TestPublicSCLitePresetsUnchanged(t *testing.T) {
	out := l31Node(t, pubPresetFuncs, pubPresetStubs+`
PR={ok:true,in_charge:'stock settings',has_idle:true,ghs:0,presets:[{name:'High',mhz:700,mv:9100,pv:9400,fan_profile:'none',tested:'2026-01-01'},
 {name:'Hashrate',mhz:650,mv:9100,pv:9400,fan_profile:'none'}],stock:{mhz:625,mv:9100,pv:9400}};
renderPresets();console.log('H3 '+cardH3());console.log('ON '+active()+' / '+$('p_preset').textContent);console.log('HTML '+$('pcards').innerHTML);`)
	for _, want := range []string{"H3 High|Middle|Low|Lowest power|Hashrate|Stock|☾ Idle\n", "ON Stock / stock settings\n",
		"Set by hand, not tested", "tested 2026-01-01", "presetStock()", `presetEdit('Hashrate')`} {
		if !strings.Contains(out, want) {
			t.Errorf("SC Lite presets lack %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Stock High") || strings.Contains(out, "Stock Low") {
		t.Errorf("SC Lite presets show the Box II names: %s", out)
	}
}

// Controls → Clock & voltage: the public release has no stock-preset row on Box II (Presets does that) and no
// commissioning words; the owner's build keeps the row, its buttons named Stock High / Stock Low.
func TestPublicBox2ControlsNoStockRow(t *testing.T) {
	for _, mdl := range []string{pubModelOwner, pubModelPublic} {
		out := l31Node(t, groomCtrlFuncs, groomStubs+`
window._MODEL=`+mdl+`;
CTRL=box2Ctrl(false);CTRL.manual=false;CTRL.select=1;renderControl(CTRL);
console.log('BODY '+$('ctrlbody').innerHTML);console.log('NOTE '+$('ctrlnote').textContent);
CTRL=box2Ctrl(true);CTRL.in_charge='preset Hashrate';renderControl(CTRL);console.log('LOCK '+$('ctrlbody').innerHTML.split('</div>')[0]);console.log('NOTEL '+$('ctrlnote').textContent);`)
		pub := strings.Contains(mdl, "public:true")
		body := between(out, "BODY ", "\nNOTE ")
		if pub {
			if strings.Contains(body, "Stock presets") || strings.Contains(body, "applyPreset(") {
				t.Errorf("public Controls still show the stock-preset row:\n%s", body)
			}
			pubCheckClean(t, "Controls", out)
		} else {
			for _, want := range []string{"Stock presets — click one", `>Stock High · 690 MHz · 7.67 V</button>`, `>Stock Low · 525 MHz · 7.03 V</button>`, "(Settings → Commissioning)"} {
				if !strings.Contains(body, want) {
					t.Errorf("owner Controls lack %q:\n%s", want, body)
				}
			}
		}
		for _, want := range []string{"7.67 V on Stock High, 7.03 V on Stock Low", "NOTE Stock Low · ", "Locked</b> — preset Stock High is in charge", "NOTEL locked — preset Stock High\n"} {
			if !strings.Contains(out, want) {
				t.Errorf("Box II Controls (public %v) lack %q:\n%s", pub, want, out)
			}
		}
		if txt := pubText(out); strings.Contains(txt, "Hashrate") || strings.Contains(txt, "Low-power") {
			t.Errorf("Box II Controls (public %v) show the OEM's mode names:\n%s", pub, out)
		}
	}
}

// The SC Lite Controls keep their stock-preset row and texts, whatever the build.
func TestPublicSCLiteControlsKeepRow(t *testing.T) {
	out := l31Node(t, groomCtrlFuncs, groomStubs+`
CTRL={ok:true,manual:false,select:0,in_charge:'preset Hashrate',current:{freq_mhz:625,vrun_mv:9100,pv_mv:9400,fan1:50,fan2:50},
 limits:{freq:[300,700],vrun:[8800,9300],pv:[9100,9600],fan:[10,100]},hardlimits:{freq:[300,900],vrun:[8800,9300],pv:[9100,9600],fan:[10,100]},
 presets:[{index:0,raw:'625 MHz',plan:{freq_mhz:625,vrun_mv:9100,pv_mv:9400}}]};
renderControl(CTRL);console.log('BODY '+$('ctrlbody').innerHTML);console.log('NOTE '+$('ctrlnote').textContent);
CTRL.in_charge='';renderControl(CTRL);console.log('NOTE2 '+$('ctrlnote').textContent);`)
	for _, want := range []string{"Stock presets — click one", `>625 MHz · PV 9400</button>`, "Locked</b> — preset Hashrate is in charge", "NOTE locked — preset Hashrate\n", "NOTE2 preset 0 · 625 MHz"} {
		if !strings.Contains(out, want) {
			t.Errorf("SC Lite Controls lack %q:\n%s", want, out)
		}
	}
}

// The sweep: the main Box II views as the public release renders them carry no commissioning or test words.
func TestPublicBox2Sweep(t *testing.T) {
	// the Dashboard's board card voltage line (and its tooltip), cut out of refresh() as groom does
	const head = `'<div class="line"><span class="muted">Voltage</span><span>'+`
	i := strings.Index(indexHTML, head)
	j := strings.Index(indexHTML[i:], `+'</span></div>'`)
	if i < 0 || j < 0 {
		t.Fatal("no board card voltage line in the page")
	}
	expr := indexHTML[i : i+j+len(`+'</span></div>'`)]
	funcs := append([]string{"featNoteHTML", "featBlock", "renderModelBanner", "MCUTOFF", "modelName", "renderPowerBox2", "pwSetting", "pwSrcText",
		"ctOpen", "ctTog", "ctWatchHTML", "ctStrongHTML", "pubNoTuner", "loadCommission"}, groomCtrlFuncs...)
	funcs = append(funcs, pubPresetFuncs...)
	seen := map[string]bool{}
	var uniq []string
	for _, f := range funcs {
		if !seen[f] && f != "hesc" {
			seen[f] = true
			uniq = append(uniq, f)
		}
	}
	out := l31Node(t, uniq, groomStubs+pubHesc+pubCut(t, "renderChipTrim", "\nasync function ctPost(")+`
const PSLOTS=[['high','High'],['middle','Middle'],['low','Low'],['lowest','Lowest power']];
const pSlotOf=n=>{const l=String(n||'').toLowerCase();return l==='lowest power'?'lowest':(['high','middle','low','lowest'].includes(l)?l:null);};
let FAN={status:{},config:{},profiles:{}};let PR=null;
globalThis.fetch=async()=>{throw new Error('no fetch in the public sweep');};globalThis.retryLater=()=>{};globalThis.busyHTML=x=>String(x);
const pr=(b,d)=>`+expr+`;
window._MODEL=`+pubModelPublic+`;
['companel','combody','modelbanner'].forEach(id=>{$(id).classList={hide:true,toggle(){},add(){this.hide=true},remove(){this.hide=false},contains(c){return c==='hide'&&this.hide;}};});
$('companel').innerHTML='';
const V={};
V.board=pr({volt_level:44,alive:true},{});
renderModelBanner(window._MODEL);V.banner=$('modelbanner').innerHTML+' hidden='+$('modelbanner').classList.hide;
loadCommission();V.settings=$('combody').innerHTML+' hidden='+$('companel').classList.hide;
V.tunerNote=featNoteHTML('The tuner');V.ceilNote=featNoteHTML('The clock ceiling probe');
CTRL=box2Ctrl(true);renderControl(CTRL);V.controls=$('ctrlbody').innerHTML+' '+$('ctrlnote').textContent;
PR={ok:true,in_charge:'',box2:true,has_idle:true,presets:[{name:'Hashrate',mhz:690,mv:42,pv:0,fan_profile:'none'},{name:'Low-power',mhz:525,mv:61,pv:0,fan_profile:'none'},{name:'Mine',mhz:600,mv:44,pv:0,fan_profile:'none'}],stock:{mhz:690,mv:42,pv:0}};
renderPresets();V.presets=$('pcards').innerHTML;
renderChipTrim({enabled:true,active:true,limits:{min:-50,max:50,step:12.5},geo:{chips:180,pairs:90,box2:true,fix:'lower the clock'},
 chains:[{chain:0,base:690,avg:100,chips:[{chip:1,trim:0,nonces:100,share:100},{chip:2,trim:-12.5,nonces:80,share:80,low:true}]}],watch:{on:true,status:'watching'},strong:{}});
V.chiptrims=$('ctbody').innerHTML;
renderPowerBox2({ok:true,calibrated:false,mains_v:230,watts:400,mhz:690,vcode:42,source:'spec',spec_tol_pct:5,
 specs:[{mode:'Hashrate',mhz:690,v:42,watts:400,ths:1.9},{mode:'Low-power',mhz:525,v:61,watts:260,ths:1.45}],cals:[]});
V.power=$('powerbody').innerHTML;
['p_intro','pfWarn','sc_hint','btnPresetBuild'].forEach(id=>{$(id).innerHTML=id==='sc_hint'?'The schedule switches between this miner’s presets (the High / Middle / Low / Lowest power the tuner built, your own from Presets, the firmware’s Stock plan, or Idle). Each change':'x';});
pubNoTuner();V.static=$('p_intro').innerHTML+' | '+$('pfWarn').innerHTML+' | '+$('sc_hint').innerHTML;
for(const k in V)console.log('V_'+k+' '+String(V[k]).replace(/\n/g,' '));`)
	views := map[string]string{}
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "V_") {
			if k := strings.Index(l, " "); k > 0 {
				views[l[2:k]] = l[k+1:]
			}
		}
	}
	for _, v := range []string{"board", "banner", "settings", "tunerNote", "ceilNote", "controls", "presets", "chiptrims", "power", "static"} {
		if _, ok := views[v]; !ok {
			t.Fatalf("view %s not rendered:\n%s", v, out)
		}
		pubCheckClean(t, v, views[v])
		if txt := pubText(views[v]); strings.Contains(txt, "Hashrate") || strings.Contains(txt, "Low-power") { // (not the keys in onclick)
			t.Errorf("public %s shows the OEM's mode names: %s", v, views[v])
		}
	}
	if views["banner"] != " hidden=true" || !strings.HasSuffix(views["settings"], "hidden=true") {
		t.Errorf("public: no banner, no Commissioning section: banner %q, settings %q", views["banner"], views["settings"])
	}
	if !strings.Contains(views["tunerNote"], "The tuner isn’t in this release yet.") || strings.Contains(views["tunerNote"], "tested") {
		t.Errorf("public feature note: %s", views["tunerNote"])
	}
	if !strings.Contains(views["power"], "<td>Stock High</td>") || !strings.Contains(views["power"], "<td>Stock Low</td>") {
		t.Errorf("Power table must name the stock modes Stock High / Stock Low: %s", views["power"])
	}
	if strings.Contains(views["chiptrims"], "Trim weak chips") || strings.Contains(views["static"], "tuner") || strings.Contains(views["static"], "the High") {
		t.Errorf("public (no tuner): no pointer to the tuner: %s | %s", views["chiptrims"], views["static"])
	}
	if !strings.Contains(views["static"], "(Stock High, Stock Low, your own from Presets, or Idle)") {
		t.Errorf("public schedule hint: %s", views["static"])
	}
}

// The owner's build keeps its commissioning texts: the feature note talks of testing, and with no "public" the
// banner shows.
func TestPublicOwnerKeepsCommissioning(t *testing.T) {
	out := l31Node(t, []string{"featNoteHTML", "renderModelBanner", "MCUTOFF", "modelName", "hesc", "PUB", "imgDot", "imgMore"}, `
window._MODEL=`+pubModelOwner+`;
console.log('NOTE '+featNoteHTML('The tuner'));renderModelBanner(window._MODEL);console.log('BANNER '+$('modelbanner').innerHTML);`)
	if !strings.Contains(out, "until it has been tested") || !strings.Contains(out, "commissioning") {
		t.Errorf("the owner's build lost its commissioning texts: %s", out)
	}
}
