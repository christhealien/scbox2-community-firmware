package main

// The page's side of the security fixes (access.go), run in node with the page's own functions: the clock set
// by hand (L5), the Access switches (M2, M1's HTTPS only, L8), the password asked again for an install and a new
// update source (M1), and the header every change carries (L7).

import (
	"strings"
	"testing"
)

func TestSecUIClockByHand(t *testing.T) {
	out := l31Node(t, []string{"clockHTML", "localDT", "setClockTo", "setClock", "setClockBrowser", "hesc"}, `
let CALLS=[];globalThis.jpost=async(u,b)=>{CALLS.push(u+' '+JSON.stringify(b));return {j:{ok:true,note:'clock set; the time servers correct it at their next sync'}};};
globalThis.toast=m=>console.log('=== TOAST '+m);globalThis.loadTime=()=>{};globalThis.setTimeout=()=>{};
console.log('=== NTP '+clockHTML({manual_clock:true,ntp:{pool:true}}));
console.log('=== NONTP '+clockHTML({manual_clock:true,ntp:{servers:'',dhcp:[]}}));
console.log('=== LITE ['+clockHTML({ntp:{pool:true}})+']');
console.log('=== YEAR '+new Date().getFullYear());
(async()=>{
 $('s_clock').value='2025-06-01T12:00:00';await setClock();
 console.log('=== WANT '+Math.floor(new Date(2025,5,1,12,0,0).getTime()/1000));
 $('s_clock').value='';await setClock();console.log('=== EMPTY '+$('s_clockmsg').textContent);
 const t0=Math.floor(Date.now()/1000);await setClockBrowser();console.log('=== T0 '+t0);
 console.log('=== CALLS '+CALLS.join(' | '));
})();`)
	ntp, nontp := between(out, "=== NTP ", "\n"), between(out, "=== NONTP ", "\n")
	for _, w := range []string{"Set the clock by hand", `type="datetime-local"`, `value="` + between(out, "=== YEAR ", "\n") + `-`,
		">Set clock</button>", "Use this browser’s time</button>", "they correct a hand-set clock at their next sync", "2024 to 2039"} {
		if !strings.Contains(ntp, w) {
			t.Errorf("the clock block lacks %q:\n%s", w, ntp)
		}
	}
	if strings.Contains(nontp, "next sync") || !strings.Contains(nontp, "Set clock") {
		t.Errorf("without time servers, no word about them:\n%s", nontp)
	}
	if !strings.Contains(out, "=== LITE []") {
		t.Errorf("without manual_clock (SC Lite) nothing is shown: %s", out)
	}
	calls := between(out, "=== CALLS ", "\n")
	want := `/api/time {"clock":` + between(out, "=== WANT ", "\n") + `,"manual":true}`
	if !strings.HasPrefix(calls, want) || strings.Count(calls, "/api/time") != 2 || !strings.Contains(calls, `"manual":true} | /api/time {"clock":`) {
		t.Errorf("Set clock / Use this browser's time: %s (want first %s)", calls, want)
	}
	if !strings.Contains(out, "=== EMPTY enter a date and time") || !strings.Contains(out, "next sync") {
		t.Errorf("an empty field must be caught, the note shown: %s", out)
	}
}

func TestSecUIAccessSwitches(t *testing.T) {
	out := l31Node(t, []string{"accessBlock", "saveAccess", "imgDot", "hesc"}, `
const A={allow_outside:false,https_only:false,port80:true,port443:true,port80_available:true,port443_available:true};
console.log('=== OFF '+accessBlock(A,'8080'));
console.log('=== ON '+accessBlock(Object.assign({},A,{allow_outside:true}),'8080'));
console.log('=== LITE ['+accessBlock(null,'8080')+']');
let CALLS=[],OK=true,ASK=[];globalThis.jpost=async(u,b)=>{CALLS.push(u+' '+JSON.stringify(b));return {j:OK?{ok:true,note:'saved'}:{ok:false,err:'no'}};};
globalThis.toast=()=>{};globalThis.setTimeout=()=>{};globalThis.reloadSettingsCard=()=>{};
let ANS=false;globalThis.confirm=m=>{ASK.push(m.split('\n')[0]);return ANS;};
let PW=null,PWASK=[];globalThis.askPassword=async m=>{PWASK.push(m);return PW;};
(async()=>{
 $('s_acc_out').checked=true;await saveAccess('s_acc_out',true);console.log('=== DECLINED '+CALLS.length+' '+$('s_acc_out').checked);
 ANS=true;$('s_acc_out').checked=true;await saveAccess('s_acc_out',true);console.log('=== NOPW '+CALLS.length+' '+$('s_acc_out').checked+' '+PWASK.length);
 PW='typed-pw';await saveAccess('s_acc_out',true);console.log('=== PWASKED '+PWASK.join(' | '));
 await saveAccess('s_acc_out',false);
 await saveAccess('s_acc_80',false);await saveAccess('s_acc_443',false);await saveAccess('s_acc_https',true);
 OK=false;$('s_acc_80').checked=true;await saveAccess('s_acc_80',true);console.log('=== REVERT '+$('s_acc_80').checked);
 console.log('=== CALLS '+CALLS.join(' | '));console.log('=== ASKED '+ASK.join(' | '));
})();`)
	off, on := between(out, "=== OFF ", "\n"), between(out, "=== ON ", "\n")
	for _, w := range []string{"Allow access from outside the local network", "Off (recommended)", "HTTPS only", "Port 80", "Port 443",
		"The main port (8080) always stays on", `id="s_acc_out"`, `id="s_acc_https"`, `id="s_acc_80" style="width:auto;margin:2px 0 0" onchange="saveAccess('s_acc_80',this.checked)" checked`} {
		if !strings.Contains(off, w) {
			t.Errorf("the access block lacks %q:\n%s", w, off)
		}
	}
	if strings.Contains(off, "st-fail") || strings.Contains(off, `id="s_acc_out" style="width:auto;margin:2px 0 0" onchange="saveAccess('s_acc_out',this.checked)" checked`) {
		t.Errorf("off by default, no warning:\n%s", off)
	}
	if !strings.Contains(on, `<b class="st-fail"`) || !strings.Contains(on, "Only your password protects the miner then") || !strings.Contains(on, "color:var(--bad)") {
		t.Errorf("switched on: a red-dot warning:\n%s", on)
	}
	if !strings.Contains(out, "=== LITE []") {
		t.Errorf("no access block without the switches (SC Lite): %s", out)
	}
	if !strings.Contains(out, "=== DECLINED 0 false") {
		t.Errorf("declining the confirmation sends nothing and unticks: %s", out)
	}
	if !strings.Contains(out, "=== NOPW 0 false 1") || !strings.Contains(out, "=== PWASKED Allowing access from outside the local network needs your panel password: | Allowing access from outside the local network needs your panel password:\n") {
		t.Errorf("switching outside access on asks for the password (cancelled: nothing sent, unticked): %s", out)
	}
	if c := between(out, "=== CALLS ", "\n"); c != `/api/net {"allow_outside":true,"password":"typed-pw"} | /api/net {"allow_outside":false} | /api/net {"port80":false} | /api/net {"port443":false} | /api/net {"https_only":true} | /api/net {"port80":true}` {
		t.Errorf("calls: %s", c)
	}
	if a := between(out, "=== ASKED ", "\n"); a != "Allow access from outside the local network? | Allow access from outside the local network? | Allow access from outside the local network? | HTTPS only?" {
		t.Errorf("confirmations: %s", a)
	}
	if !strings.Contains(out, "=== REVERT false") {
		t.Errorf("a refused change puts the box back: %s", out)
	}
	if !strings.Contains(indexHTML, "sshBlock(n.ssh||{},n.ip)+accessBlock(n.access,netPorts(n).live)") {
		t.Error("Settings -> Network must show the access block")
	}
}

const secUpdStubs = `
globalThis.document={getElementById:id=>E[id]||null,querySelectorAll:()=>[],querySelector:()=>null};
let TIMERS=[];globalThis.setTimeout=(f)=>{TIMERS.push(f);};let TOASTS=[];globalThis.toast=m=>TOASTS.push(m);
function imgVerify(){}function loadUpdate(){}function refresh(){}function settingsExport(){}
globalThis.confirm=()=>true;let ASKED=[];globalThis.askPassword=async m=>{ASKED.push(m);return 'typed-pw';};
let CALLS=[],RESP={};
globalThis.jpost=async(u,b)=>{CALLS.push(u+' '+JSON.stringify(b));const k=Object.keys(b)[0];const r=RESP[k];return {j:typeof r==='function'?r():(r||{ok:true})};};
function reset(){for(const k in E)delete E[k];TIMERS=[];TOASTS=[];CALLS=[];ASKED=[];window._imgAck=null;window._imgAuto=null;window._imgVerifying=false;window._imgChain=null;}
const BASE={ok:true,image:true,version:'0.4.0',repo:'christhealien/scbox2-community-firmware',default_repo:'christhealien/scbox2-community-firmware',
 asset_pattern:'scbox2-firmware-v….ius',install_needs_password:true,repo_needs_password:true};
const GEN=Object.assign({},BASE,{staged:{version:'0.5.0',size:12900000,sha256:'ab'.repeat(32),sums:'github',sums_from:'github',sums_tag:'v0.5.0',
 lookup:'done',genuine:true,ready:'ok',same:'different',warning:'',checks:[]}});
const LATEST={tag:'v0.5.0',name:'v0.5.0',newer:true,published:'2026-11-10',url:'https://github.com/christhealien/scbox2-community-firmware/releases/tag/v0.5.0',notes:'',asset:'scbox2-firmware-v0.5.0.ius',size:12900000};
window._MODEL={ok:true,id:'scbox2',name:'SC Box II',public:true,features:{FirmwareUpdate:true}};
`

func TestSecUIInstallAsksPassword(t *testing.T) {
	realJpost := strings.Replace(l31Func(t, "jpost"), "async function jpost(", "async function realJpost(", 1)
	out := l31Node(t, append(append([]string{}, groomUpdFuncs...), "imgInstall", "updRepo", "updPost"), secUpdStubs+realJpost+`
(async()=>{
 reset();renderUpdate(GEN);console.log('=== STEP3 '+$('updbody').innerHTML.replace(/\n/g,' '));
 [1,2,3,4].forEach(n=>{$('imgack'+n).checked=true;});imgAckUI();
 await imgInstall();console.log('=== NOPW '+CALLS.length+' '+TOASTS.join(','));
 $('imgpw').value='owner-pw';await imgInstall();console.log('=== PW '+CALLS.join(' | '));
 // the release card's one-click install
 reset();renderUpdate(BASE,LATEST);console.log('=== CARD '+$('updlatest').innerHTML.replace(/\n/g,' '));
 [1,2,3,4].forEach(n=>{$('imgq'+n).checked=true;});
 RESP={fetch:GEN,verify:Object.assign({},GEN,{verified:true}),install:{ok:true,note:'writing'}};
 await imgQuick('v0.5.0');console.log('=== QNOPW '+CALLS.length+' '+TOASTS.join(',')+' '+JSON.stringify(window._imgChain));
 $('imgqpw').value='owner-pw';await imgQuick('v0.5.0');console.log('=== QPW '+CALLS.join(' | '));
 reset();RESP={install:{ok:false,err:'that isn’t the panel password: nothing was changed'},fetch:GEN,verify:Object.assign({},GEN,{verified:true})};
 renderUpdate(BASE,LATEST);[1,2,3,4].forEach(n=>{$('imgq'+n).checked=true;});$('imgqpw').value='bad';await imgQuick('v0.5.0');
 console.log('=== QBAD '+JSON.stringify(window._imgChain));
 // the update source
 reset();window._upd=BASE;E.updrepo={value:'me/fork'};RESP={repo:BASE};await updRepo();console.log('=== REPO '+CALLS.join(' | ')+' ASK '+ASKED.length);
 reset();window._upd=Object.assign({},BASE,{repo_needs_password:false});E.updrepo={value:'me/fork'};RESP={repo:BASE};await updRepo();console.log('=== REPOLITE '+CALLS.join(' | ')+' ASK '+ASKED.length);
 // without the flag (the SC Lite, an older panel) no password field
 reset();renderUpdate(Object.assign({},GEN,{install_needs_password:false}));console.log('=== NOFLAG '+$('updbody').innerHTML.replace(/\n/g,' '));
 // the page's fetch helper names the page (L7)
 let H=null;globalThis.fetch=async(u,o)=>{H=o.headers;return {status:200,json:async()=>({ok:true})};};
 await realJpost('/api/x',{});console.log('=== HDR '+JSON.stringify(H));
})();`)
	step3 := between(out, "=== STEP3 ", "\n")
	if !strings.Contains(step3, `id="imgpw" type="password" autocomplete="current-password"`) || !strings.Contains(step3, "Your panel password") {
		t.Errorf("step ③ must ask for the password:\n%s", step3)
	}
	if !strings.Contains(out, "=== NOPW 0 Enter your panel password first") {
		t.Errorf("Install without the password sends nothing: %s", out)
	}
	if p := between(out, "=== PW ", "\n"); p != `/api/update {"install":true,"ack":true,"password":"owner-pw"}` {
		t.Errorf("Install sends the password: %s", p)
	}
	card := between(out, "=== CARD ", "\n")
	if i, j := strings.Index(card, `id="imgq4"`), strings.Index(card, `id="imgqpw"`); i < 0 || j < i || !strings.Contains(card, "Download and install v0.5.0") {
		t.Errorf("the one-click card: a password field after the four confirmations:\n%s", card)
	}
	if q := between(out, "=== QNOPW ", "\n"); !strings.HasPrefix(q, "0 Enter your panel password first null") {
		t.Errorf("one-click without the password: nothing starts: %s", q)
	}
	if q := between(out, "=== QPW ", "\n"); q != `/api/update {"fetch":true} | /api/update {"verify":true} | /api/update {"install":true,"ack":true,"password":"owner-pw"}` {
		t.Errorf("one-click with the password: %s", q)
	}
	if q := between(out, "=== QBAD ", "\n"); !strings.Contains(q, `"stage":2`) || !strings.Contains(q, "isn’t the panel password") {
		t.Errorf("a wrong password stops the chain before anything is written: %s", q)
	}
	if r := between(out, "=== REPO ", "\n"); r != `/api/update {"repo":"me/fork","password":"typed-pw"} ASK 1` {
		t.Errorf("a new update source asks for the password: %s", r)
	}
	if r := between(out, "=== REPOLITE ", "\n"); r != `/api/update {"repo":"me/fork"} ASK 0` {
		t.Errorf("without repo_needs_password nothing is asked: %s", r)
	}
	if n := between(out, "=== NOFLAG ", "\n"); strings.Contains(n, "imgpw") {
		t.Errorf("no password field without install_needs_password:\n%s", n)
	}
	if h := between(out, "=== HDR ", "\n"); h != `{"Content-Type":"application/json","X-Requested-With":"panel"}` {
		t.Errorf("jpost's headers: %s", h)
	}
}
