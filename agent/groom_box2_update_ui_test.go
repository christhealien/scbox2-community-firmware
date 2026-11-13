package main

import (
	"regexp"
	"strings"
	"testing"
)

// Settings → Update on the SC Box II (owner's redesign): three numbered steps, step ② runs by itself, the
// genuine-check in plain words, three confirmations before Install. Run in node with the page's own functions.
// The SC Lite's screen (renderUpdate's package path) is untouched.

var groomUpdFuncs = []string{"renderUpdate", "renderImageUpdate", "imgStep", "imgAckAll", "imgAckUI", "imgShowLatest",
	"updWarnHTML", "updSumsHTML", "updShowLatest", "updMigHTML", "updWhen", "hesc", "modelName",
	"imgConfirmHTML", "imgRecoverHTML", "imgDot", "imgMore", "imgDots", "imgChainHTML", "imgChainShow", "imgQAll", "imgQUI", "imgQuick", "imgPwHTML", "imgPw", "updProgSet", "updProgSave", "updProgGet", "updProgHTML", "updProgInSettings", "startUpdTick", "stopUpdTick"}

const groomUpdStubs = `
globalThis.document={getElementById:id=>E[id]||null,querySelectorAll:()=>[],querySelector:()=>null};
let TIMERS=[];globalThis.setTimeout=(f)=>{TIMERS.push(f);};globalThis.toast=()=>{};
function imgVerify(){}
function reset(){for(const k in E)delete E[k];TIMERS=[];window._imgAck=null;window._imgAuto=null;window._imgVerifying=false;window._updProg=null;}
const BASE={ok:true,image:true,version:'0.4.0',repo:'christhealien/scbox2-community-firmware',default_repo:'christhealien/scbox2-community-firmware',
 asset_pattern:'scbox2-firmware-v….ius',partition:18874368,last:{result:'written',to:'0.4.0',from:'0.3.2',at:1700000000}};
const ST=o=>Object.assign({},BASE,{staged:Object.assign({version:'0.5.0',size:12900000,sha256:'ab'.repeat(32),sys_len:10000000,
 sums:'',sums_from:'',sums_tag:'',lookup:'',lookup_why:'',genuine:false,ready:'',same:'different',warning:'',checks:[]},o)});
function show(name,u){reset();renderUpdate(u);const b=$('imginst');
 console.log('=== '+name+' HTML '+$('updbody').innerHTML.replace(/\n/g,' '));
 console.log('=== '+name+' AUTO '+TIMERS.length);
 console.log('=== '+name+' BTN0 '+(/id="imginst"/.test($('updbody').innerHTML)?b.disabled:'none'));
 console.log('=== '+name+' HINT '+$('imginsthint').textContent);
 [1,2,3].forEach(n=>{$('imgack'+n).checked=true;});imgAckUI();
 console.log('=== '+name+' BTN3 '+(/id="imginst"/.test($('updbody').innerHTML)?b.disabled:'none'));
 $('imgack4').checked=true;imgAckUI();
 console.log('=== '+name+' BTN4 '+(/id="imginst"/.test($('updbody').innerHTML)?b.disabled:'none'));
 console.log('=== '+name+' END');}
window._MODEL={ok:true,id:'scbox2',name:'SC Box II',features:{FirmwareUpdate:true}};
show('NONE',BASE);
show('JUST',ST({}));
show('GENUINE',ST({sums:'online',sums_from:'online',sums_tag:'v0.5.0',lookup:'done',genuine:true,ready:'ok'}));
show('DOWNLOADED',ST({sums:'github',sums_from:'github',sums_tag:'v0.5.0',genuine:true,ready:'ok'}));
show('MANUAL',ST({lookup:'unavailable',lookup_why:'the miner couldn’t reach GitHub (timeout)',ready:'ok'}));
show('MISMATCH',ST({sums:'nomatch',sums_from:'online',sums_tag:'v0.5.0',lookup:'done',ready:'ok'}));
show('PASTEBAD',ST({sums:'nomatch',sums_from:'pasted',lookup:'unavailable',ready:'ok'}));
show('NOTREADY',ST({sums:'online',sums_from:'online',sums_tag:'v0.5.0',genuine:true,ready:'fail',checks:[{ok:true,msg:'image ok'},{ok:false,msg:'free memory 20 MB, the install needs 60 MB'}]}));
// the release card: one-click install
const LATEST={tag:'v0.5.0',name:'v0.5.0',newer:true,published:'2026-11-10',url:'https://github.com/christhealien/scbox2-community-firmware/releases/tag/v0.5.0',notes:'- fixes',asset:'scbox2-firmware-v0.5.0.ius',size:12900000};
reset();renderUpdate(BASE,LATEST);
console.log('=== CARD HTML '+$('updlatest').innerHTML.replace(/\n/g,' '));
[1,2,3].forEach(n=>{$('imgq'+n).checked=true;});imgQUI();console.log('=== CARD Q3 '+$('imgquick').disabled);
$('imgq4').checked=true;imgQUI();console.log('=== CARD Q4 '+$('imgquick').disabled);
// the chain, against a mocked backend
let CALLS=[],RESP={};
globalThis.jpost=async(u,b)=>{const k=Object.keys(b)[0];CALLS.push(k+(b.ack?'+ack':''));const r=RESP[k];return {j:typeof r==='function'?r():r};};
const OKV=o=>ST(Object.assign({sums:'github',sums_from:'github',sums_tag:'v0.5.0',genuine:true},o));
async function chain(name,resp,ticks){reset();renderUpdate(BASE,LATEST);CALLS=[];RESP=resp;window._imgChain=null;
 [1,2,3,4].forEach(n=>{$('imgq'+n).checked=n<=ticks;});await imgQuick('v0.5.0');
 console.log('=== '+name+' CALLS '+CALLS.join(','));
 console.log('=== '+name+' CHAIN '+JSON.stringify(window._imgChain));
 console.log('=== '+name+' HTML '+$('updbody').innerHTML.replace(/\n/g,' '));}
(async()=>{
await chain('CH_OK',{fetch:OKV({}),verify:Object.assign(OKV({ready:'ok'}),{verified:true}),install:{ok:true,note:'writing v0.5.0'}},4);
await chain('CH_TICKS',{fetch:OKV({})},3);
await chain('CH_FETCH',{fetch:{ok:false,err:'can’t reach GitHub: timeout'}},4);
await chain('CH_MISMATCH',{fetch:OKV({}),verify:ST({sums:'nomatch',sums_from:'online',sums_tag:'v0.5.0',lookup:'done',ready:'ok'}),install:{ok:true}},4);
await chain('CH_NOTREADY',{fetch:OKV({}),verify:OKV({ready:'fail',checks:[{ok:false,msg:'free memory 20 MB, the install needs 60 MB'}]}),install:{ok:true}},4);
await chain('CH_VERIFYERR',{fetch:OKV({}),verify:{ok:false,err:'an update is already in progress'},install:{ok:true}},4);
await chain('CH_INSTALLREFUSED',{fetch:OKV({}),verify:OKV({ready:'ok'}),install:{ok:false,err:'not installing: the tuner is running: stop it first'}},4);
window._MODEL=null;window._imgChain=null;
reset();renderUpdate({ok:true,version:'1.5.20',repo:'christhealien/sclite-community-firmware',default_repo:'christhealien/sclite-community-firmware',
 staged:{version:'1.5.21',files:12,size:3000000,sha256:'cd'.repeat(32),sums:''}});
console.log('=== LITE HTML '+$('updbody').innerHTML.replace(/\n/g,' '));console.log('=== LITE END');
})();
`

func groomCase(out, name, what string) string { return between(out, "=== "+name+" "+what+" ", "\n") }

func TestGroomBox2UpdateSteps(t *testing.T) {
	out := l31Node(t, groomUpdFuncs, groomUpdStubs)
	type tc struct {
		name, btn0, btn3, btn4, auto string
		want, bad                    []string
	}
	common := []string{"① Get the update file", "② Check the file", "③ Install", `<details id="imgrbnote"`,
		`<b class="st-fail" style="font-size:13px;margin-right:6px">●</b><b>No automatic roll back: an SD card is the recovery</b></summary>`, "always brings the miner back", "never written"}
	for _, c := range []tc{
		{"NONE", "none", "none", "none", "0", []string{"Check for updates", "Or upload the file", `id="imgfile"`, "Update source", "Starts by itself", "Waiting", "Installed", "v0.4.0", "written"},
			[]string{"Verify only", "SHA-256", "checksum", "imgack"}},
		{"JUST", "true", "true", "true", "1", []string{"Firmware <b>v0.5.0</b>", "Use a different file", "The file is complete", "Made for the SC Box II", "Checking that it’s the official file", "I saved my settings", "Export settings"},
			[]string{"Verify only", "One step by hand"}},
		{"GENUINE", "true", "true", "false", "0", []string{"Genuine: it matches the official release v0.5.0", "The miner is ready for it", `<b class="okpill">Done</b>`, `st-pass" style="font-size:13px;margin-right:6px">●</b>② Check the file`,
			`<span id="imgackd1"><b class="st-run"`, "Why these four?", "the SD card recovery erases them", "2–3 minute write", "Install v0.5.0 and restart", "I have an SD card with the SC Box II firmware ready", "The power is stable",
			"I understand that if the install fails, the miner only comes back with the firmware on an SD card", "How to recover with an SD card",
			"scbox2-sd-v0.5.0.img.xz", "https://github.com/christhealien/scbox2-community-firmware/releases/tag/v0.5.0", "2–3 minute write", "Mining stops for about 4 minutes"},
			[]string{"One step by hand", "Don’t install"}},
		{"DOWNLOADED", "true", "true", "false", "0", []string{"downloaded from GitHub", "Genuine: it matches the official release v0.5.0", "when it downloaded"}, nil},
		{"MANUAL", "true", "true", "true", "0", []string{"One step by hand", "We couldn’t check this file online", "couldn’t reach GitHub", "copy the line for <span class=\"mono\">scbox2-firmware-v0.5.0.ius</span>", `id="imgsums"`, "Example: ", "Where do I find that line?", "Try online again", "Needs you"},
			[]string{"Don’t install"}},
		{"MISMATCH", "none", "none", "none", "0", []string{"This file doesn’t match the official release v0.5.0. Don’t install it.", `<b class="badpill">Stopped</b>`, `<b class="st-fail" style="font-size:13px;margin-right:6px">●</b><b class="badpill">This file`, "didn’t pass the check"},
			[]string{"One step by hand", `id="imginst"`}},
		{"PASTEBAD", "true", "true", "true", "0", []string{"doesn’t match the line you pasted", "One step by hand", "paste the line again"}, nil},
		{"NOTREADY", "none", "none", "none", "0", []string{"The miner isn’t ready for it", "free memory 20 MB", "Not possible until the miner is ready", `<b class="st-run" style="font-size:13px;margin-right:6px">●</b>The miner isn’t ready`}, []string{`id="imginst"`}},
	} {
		h := groomCase(out, c.name, "HTML")
		if h == "" {
			t.Fatalf("%s: no output:\n%s", c.name, out)
		}
		for _, w := range append(append([]string{}, common...), c.want...) {
			if !strings.Contains(h, w) {
				t.Errorf("%s lacks %q:\n%s", c.name, w, h)
			}
		}
		for _, b := range c.bad {
			if strings.Contains(h, b) {
				t.Errorf("%s shows %q:\n%s", c.name, b, h)
			}
		}
		// jargon only inside "Show details" or the manual-paste help
		vis := regexp.MustCompile(`(?s)<details.*?</details>`).ReplaceAllString(h, "")
		if i := strings.Index(vis, "One step by hand"); i >= 0 {
			vis = vis[:i]
		}
		if strings.Contains(vis, "SHA-256") || strings.Contains(strings.ToLower(vis), "checksum") {
			t.Errorf("%s shows jargon up front:\n%s", c.name, vis)
		}
		if g := groomCase(out, c.name, "BTN0"); g != c.btn0 {
			t.Errorf("%s: Install button before ticking = %s, want %s", c.name, g, c.btn0)
		}
		if g := groomCase(out, c.name, "HINT"); c.btn0 == "true" && g != map[bool]string{true: "tick all four to continue", false: "waits for step ② to pass"}[c.btn4 == "false"] {
			t.Errorf("%s: hint next to Install = %q", c.name, g)
		}
		if g := groomCase(out, c.name, "BTN3"); g != c.btn3 {
			t.Errorf("%s: Install button with all three ticked: disabled=%s, want %s", c.name, g, c.btn3)
		}
		if g := groomCase(out, c.name, "BTN4"); g != c.btn4 {
			t.Errorf("%s: Install button with all four ticked: disabled=%s, want %s", c.name, g, c.btn4)
		}
		if g := groomCase(out, c.name, "AUTO"); g != c.auto {
			t.Errorf("%s: automatic check started %s times, want %s", c.name, g, c.auto)
		}
	}
	card := groomCase(out, "CARD", "HTML")
	for _, w := range []string{"Install it in one go", `id="imgquick"`, "Download and install v0.5.0</button>", `disabled>Download and install`,
		"I saved my settings", "Export settings", "I have an SD card with the SC Box II firmware ready", "The power is stable",
		"I understand that if the install fails, the miner only comes back with the firmware on an SD card",
		"<details", "How to recover with an SD card", "scbox2-sd-v0.5.0.img.xz", "balenaEtcher", "SYS LED flashes blue and red",
		"only the <b>blue</b> SYS LED flashes", "<b>remove the card</b>", "<span class=\"mono\">admin</span> / <span class=\"mono\">admin</span>",
		"import the settings file you exported", "prepare the card <b>before</b> you update",
		`href="https://github.com/christhealien/scbox2-community-firmware/blob/main/docs/INSTALL.md"`,
		`href="https://github.com/christhealien/scbox2-community-firmware/releases/tag/v0.5.0"`,
		"No automatic roll back on this miner: if writing fails, the SD card brings it back.", "Download only", "What’s new"} {
		if !strings.Contains(card, w) {
			t.Errorf("release card lacks %q:\n%s", w, card)
		}
	}
	if strings.Contains(card, "<details open") {
		t.Errorf("the recovery steps must be collapsed by default:\n%s", card)
	}
	if groomCase(out, "CARD", "Q3") != "true" || groomCase(out, "CARD", "Q4") != "false" {
		t.Errorf("Download and install: disabled until all four are ticked: Q3=%s Q4=%s", groomCase(out, "CARD", "Q3"), groomCase(out, "CARD", "Q4"))
	}
	for _, c := range []struct{ name, calls, chain, want string }{
		{"CH_OK", "fetch,verify,install+ack", `"stage":3`, "The miner is writing the new system"},
		{"CH_TICKS", "", "null", "① Get the update file"},
		{"CH_FETCH", "fetch", `"stage":0`, "The download didn’t work: can’t reach GitHub: timeout."},
		{"CH_MISMATCH", "fetch,verify", `"stage":1`, "The downloaded file doesn’t match the official release. Don’t install it."},
		{"CH_NOTREADY", "fetch,verify", `"stage":1`, "The miner isn’t ready for it: free memory 20 MB, the install needs 60 MB."},
		{"CH_VERIFYERR", "fetch,verify", `"stage":1`, "The check couldn’t run: an update is already in progress."},
		{"CH_INSTALLREFUSED", "fetch,verify,install+ack", `"stage":2`, "The install didn’t start: not installing: the tuner is running"},
	} {
		if g := groomCase(out, c.name, "CALLS"); g != c.calls {
			t.Errorf("%s: backend calls %q, want %q", c.name, g, c.calls)
		}
		ch, h := groomCase(out, c.name, "CHAIN"), groomCase(out, c.name, "HTML")
		if !strings.Contains(ch, c.chain) || !strings.Contains(h, c.want) {
			t.Errorf("%s: chain %s; page lacks %q:\n%s", c.name, ch, c.want, h)
		}
		if c.name == "CH_OK" && !strings.Contains(h, `id="imginst" onclick="imgInstall()" disabled`) {
			t.Errorf("after the one-click install started, step ③'s Install must stay disabled:\n%s", h)
		}
		failed := c.name != "CH_OK" && c.name != "CH_TICKS"
		if failed != strings.Contains(h, "Nothing was written to the miner.") || failed != strings.Contains(ch, `"err"`) {
			t.Errorf("%s: a failed stage must stop and say nothing was written (and only then):\n%s", c.name, h)
		}
		if failed && (!strings.Contains(h, "① Get the update file") || !strings.Contains(h, "② Check the file")) {
			t.Errorf("%s: the three-step screen must stay in place:\n%s", c.name, h)
		}
	}
	lite := groomCase(out, "LITE", "HTML")
	for _, w := range []string{"Ready to install: v1.5.21", "Check it against the release’s SHA256SUMS (optional, no internet needed)", "sclite-update-….tar.gz",
		"Install v1.5.21 and reboot", "the previous one is put back by itself", "SHA-256"} {
		if !strings.Contains(lite, w) {
			t.Errorf("SC Lite update screen lost %q:\n%s", w, lite)
		}
	}
	for _, b := range []string{"①", "Check the file", "imgack", "One step by hand", "Genuine"} {
		if strings.Contains(lite, b) {
			t.Errorf("SC Lite update screen shows the Box II's %q:\n%s", b, lite)
		}
	}
}
