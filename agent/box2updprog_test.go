package main

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The dashboard and Settings show a firmware-image install's progress while the miner is away (the page that started it,
// across the new panel's reload), then the result once it answers again.
func TestUpdProgDashboard(t *testing.T) {
	out := l34Node(t, []string{"updProgSet", "updProgSave", "updProgGet", "updProgClear", "updProgHTML", "updProgShow", "startUpdTick", "stopUpdTick", "updProgBack", "updProgInSettings"}, `
const mem={};globalThis.localStorage={getItem:k=>mem[k]??null,setItem:(k,v)=>{mem[k]=String(v)},removeItem:k=>{delete mem[k]}};
globalThis.setInterval=()=>1;globalThis.clearInterval=()=>{};
globalThis.document=globalThis.document||{};document.querySelectorAll=()=>[];updProgSet('v0.5.1');updProgInSettings();console.log('settings='+$('updbody').innerHTML);console.log('shown='+updProgShow());console.log('ov='+$('ov').innerHTML);
window._updProg=undefined;const p=updProgGet();console.log('kept='+!!(p&&p.down&&p.ver==='v0.5.1'));
globalThis.fetch=async()=>({json:async()=>({version:'0.5.1',last:{result:'installed'}})});globalThis.refresh=()=>{};
await updProgBack();console.log('done='+window._updDone+' left='+JSON.stringify(mem));
updProgSet('v1.5.30','pkg');console.log('lite='+updProgHTML(updProgGet()));updProgClear();`)
	for _, want := range []string{"settings=<!--updprog--><div class=\"note updprog\"", "shown=true", "Updating the firmware to v0.5.1", "Writing the new system", "kept=true", "now running v0.5.1", "left={}", "lite=", "Updating the firmware to v1.5.30", "Installing the new files", "2–3 minutes"} {
		if !strings.Contains(out, want) {
			t.Fatalf("want %q in:\n%s", want, out)
		}
	}
}

// updProgJS: the page's install, progress, dashboard and Settings code, with a localStorage, for the tests below.
var updProgFuncs = []string{"jpost", "fetchTO", "busyHTML", "loadUpdate", "imgInstall", "imgAckAll", "imgPw", "imgQuick", "imgQAll", "imgChainShow", "imgChainHTML",
	"updInstall", "verLess", "updInstallGoing", "updProgSet", "updProgSave", "updProgGet", "updProgClear", "updProgHTML", "updProgShow", "startUpdTick", "stopUpdTick",
	"updProgBack", "updProgInSettings", "updDoneDismiss", "refresh"}

const updProgPrelude = `
let mem={};globalThis.localStorage={getItem:k=>mem[k]??null,setItem:(k,v)=>{mem[k]=String(v)},removeItem:k=>{delete mem[k]}};
globalThis.setInterval=()=>1;globalThis.clearInterval=()=>{};globalThis.setTimeout=()=>1;
globalThis.document=globalThis.document||{};document.querySelectorAll=()=>[];document.getElementById=id=>E[id]||null;
let LOADS=0;globalThis.renderUpdate=()=>{};globalThis.renderImageUpdate=()=>{};globalThis.askPassword=async()=>'pw';
function fresh(){mem={};window._updProg=null;window._updDone='';LOADS=0;calls.toast.length=0;for(const k in E)delete E[k];
 window._upd={version:'0.5.2',staged:{version:'0.5.3-owner'},install_needs_password:true};window._imgCan=true;window._imgChain=null;
 [1,2,3,4].forEach(n=>{$('imgack'+n).checked=true;});$('imgpw').value='pw';
 $('updbody').innerHTML='<div class="imgstep">③ Install <button id="imginst">Install v0.5.3-owner and restart</button></div>';}
// loadUpdate as the page has it, counted (its fetch is the test's)
const realLoadUpdate=loadUpdate;loadUpdate=async function(){LOADS++;return realLoadUpdate();};
`

// The real SC Box II (v0.5.2 → v0.5.3-owner, an uploaded file): after Install the page showed no progress at all, only
// "Can't reach the miner…" in Settings and on the Dashboard, and no result after it came back. The panel hands off to
// its copy in RAM, whose exec closes every socket: the install's answer was lost, jpost said "couldn't reach the miner",
// and the page took that for a refused install. Reproduced in a browser against a local panel (the answer lost: the
// same screens as the owner's). Now a lost answer counts as an install that most likely started.
func TestUpdProgInstallAnswerLost(t *testing.T) {
	out := l34Node(t, updProgFuncs, updProgPrelude+`
// ① the answer never arrives (the connection drops as the panel hands off)
fresh();globalThis.fetch=async()=>{throw new TypeError('Failed to fetch');};
await imgInstall();
console.log('=== LOSTSET '+$('updbody').innerHTML);
console.log('=== LOSTPROG '+mem['fw-updprog']);
console.log('=== LOSTTOAST '+calls.toast.join(' | ')+' loads='+LOADS);
// the dashboard's poll fails while the miner writes: the progress there too
$('ov').innerHTML='old tiles';await refresh();console.log('=== LOSTDASH '+$('ov').innerHTML);refresh=async()=>{};
// Settings' own reload fails as well: the progress stays, not "Can't reach the miner"
await loadUpdate();console.log('=== LOSTRELOAD '+$('updbody').innerHTML);
// the new panel's page (a reload): the install is still known, then the result once it answers
window._updProg=undefined;console.log('=== RELOADED '+JSON.stringify(updProgGet()));
globalThis.fetch=async()=>({json:async()=>({version:'0.5.3-owner',last:{result:'installed',to:'0.5.3-owner',from:'0.5.2'}})});
await updProgBack();updProgInSettings();console.log('=== BACKSET '+$('updbody').innerHTML);console.log('=== BACKPROG '+mem['fw-updprog']);
updDoneDismiss();console.log('=== DISMISSED '+$('updbody').innerHTML+' done='+window._updDone);

// ② an answer that is cut short (HTTP 200, the body incomplete): the same
fresh();globalThis.fetch=async()=>({status:200,json:async()=>{throw new SyntaxError('Unexpected end of JSON input');}});
await imgInstall();console.log('=== CUT '+(mem['fw-updprog']||'none'));

// ③ a refusal is still a refusal: no progress, the reason, Settings reloaded
fresh();globalThis.fetch=async()=>({status:200,json:async()=>({ok:false,err:'that isn’t the panel password: nothing was changed'})});
await imgInstall();console.log('=== REFUSED '+(mem['fw-updprog']||'none')+' '+calls.toast.join(' | ')+' loads='+LOADS);
fresh();globalThis.fetch=async()=>({status:403,json:async()=>{throw new SyntaxError('Unexpected token');}});
await imgInstall();console.log('=== FORBIDDEN '+(mem['fw-updprog']||'none'));

// ④ the answer lost but the panel goes on answering (the request never got there): after 20 s, "didn't start"
fresh();updProgSet('v0.5.3-owner','image',true);updProgInSettings();
globalThis.fetch=async()=>({json:async()=>({version:'0.5.2'})});
await updProgBack();console.log('=== EARLY '+(mem['fw-updprog']?'kept':'gone'));
window._updProg.at=Date.now()-25000;await updProgBack();
console.log('=== NOSTART '+(mem['fw-updprog']||'none')+' '+calls.toast.join(' | ')+' loads='+LOADS);

// ⑤ the answer arrives: as before (sure), the steps replaced by the progress
fresh();globalThis.fetch=async()=>({status:200,json:async()=>({ok:true,note:'writing v0.5.3-owner'})});
await imgInstall();console.log('=== OK '+mem['fw-updprog']+' '+calls.toast.join(' | '));console.log('=== OKSET '+$('updbody').innerHTML);

// ⑥ the one-click install from the release card, its answer lost
fresh();window._imgChain=null;[1,2,3,4].forEach(n=>{$('imgq'+n).checked=true;});$('imgqpw').value='pw';
let n=0;const ST={ok:true,image:true,staged:{version:'0.5.3',sha256:'ab',genuine:true,ready:'ok'}};
globalThis.fetch=async()=>{n++;if(n<3)return {status:200,json:async()=>ST};throw new TypeError('Failed to fetch');};
await imgQuick('v0.5.3');console.log('=== QUICK '+(mem['fw-updprog']||'none')+' '+JSON.stringify(window._imgChain));

// ⑦ the SC Lite's package install (kind pkg), its answer lost
fresh();window._MODEL=null;window._upd={version:'1.5.29',staged:{version:'1.5.30'}};
globalThis.fetch=async()=>{throw new TypeError('Failed to fetch');};
await updInstall();console.log('=== LITE '+(mem['fw-updprog']||'none'));console.log('=== LITESET '+$('updbody').innerHTML);`)
	get := func(k string) string { return between(out, "=== "+k+" ", "\n") }
	if s := get("LOSTSET"); !strings.HasPrefix(s, `<!--updprog--><div class="note updprog"`) || !strings.Contains(s, "Updating the firmware to v0.5.3-owner") ||
		strings.Contains(s, "imginst") || strings.Contains(s, "reach the miner") {
		t.Errorf("a lost answer: Settings shows the progress in place of the steps:\n%s\n%s", s, out)
	}
	if p := get("LOSTPROG"); !strings.Contains(p, `"unsure":true`) || !strings.Contains(p, `"kind":"image"`) {
		t.Errorf("a lost answer is kept as an install that most likely started: %s", p)
	}
	if tt := get("LOSTTOAST"); !strings.Contains(tt, "most likely installing now") || strings.Contains(tt, "Couldn’t reach") || !strings.HasSuffix(tt, "loads=0") {
		t.Errorf("a lost answer isn't reported as a failure: %s", tt)
	}
	if d := get("LOSTDASH"); !strings.Contains(d, "Updating the firmware to v0.5.3-owner") || strings.Contains(d, "old tiles") {
		t.Errorf("the dashboard shows the progress while the miner is away:\n%s", d)
	}
	if s := get("LOSTRELOAD"); !strings.Contains(s, "updprog") || strings.Contains(s, "reach the miner") {
		t.Errorf("Settings' failed reload keeps the progress:\n%s", s)
	}
	if r := get("RELOADED"); !strings.Contains(r, `"down":true`) {
		t.Errorf("the reloaded page knows the install (seen going down): %s", r)
	}
	if s := get("BACKSET"); !strings.Contains(s, "<!--upddone-->") || !strings.Contains(s, "Firmware update finished: now running v0.5.3-owner") || strings.Contains(s, "updprog") {
		t.Errorf("back: the result in Settings:\n%s", s)
	}
	if p := get("BACKPROG"); p != "undefined" {
		t.Errorf("back: the progress is cleared: %s", p)
	}
	if d := get("DISMISSED"); strings.Contains(d, "upddone") || !strings.HasSuffix(d, "done=") {
		t.Errorf("dismiss removes the result note: %s", d)
	}
	if c := get("CUT"); !strings.Contains(c, `"unsure":true`) {
		t.Errorf("an answer cut short counts as lost: %s", c)
	}
	if r := get("REFUSED"); !strings.HasPrefix(r, "none that isn’t the panel password") || !strings.HasSuffix(r, "loads=1") {
		t.Errorf("a refusal shows no progress: %s", r)
	}
	if f := get("FORBIDDEN"); f != "none" {
		t.Errorf("an error page (HTTP 403) is a refusal: %s", f)
	}
	if e := get("EARLY"); e != "kept" {
		t.Errorf("the panel answering at once doesn't end it yet: %s", e)
	}
	if n := get("NOSTART"); !strings.HasPrefix(n, "none ") || !strings.Contains(n, "The install didn’t start") || !strings.HasSuffix(n, "loads=1") {
		t.Errorf("lost answer, panel never went away: didn't start: %s", n)
	}
	if o := get("OK"); !strings.Contains(o, `"unsure":false`) || !strings.Contains(o, "writing v0.5.3-owner") {
		t.Errorf("an answered install: %s", o)
	}
	if s := get("OKSET"); !strings.HasPrefix(s, "<!--updprog-->") || strings.Contains(s, "imginst") {
		t.Errorf("an answered install: the progress in place of the steps:\n%s", s)
	}
	if q := get("QUICK"); !strings.Contains(q, `"ver":"v0.5.3"`) || !strings.Contains(q, `"unsure":true`) || !strings.Contains(q, `"ok":true`) {
		t.Errorf("one-click install, answer lost: %s", q)
	}
	if l := get("LITE"); !strings.Contains(l, `"kind":"pkg"`) || !strings.Contains(l, `"ver":"v1.5.30"`) || !strings.Contains(l, `"unsure":true`) {
		t.Errorf("SC Lite package install, answer lost: %s", l)
	}
	if s := get("LITESET"); !strings.Contains(s, "Installing the new files") && !strings.Contains(s, "Saving the files it replaces") {
		t.Errorf("SC Lite: its own stages:\n%s", s)
	}
	if strings.Contains(out, "THROWN") {
		t.Errorf("an exception:\n%s", out)
	}
}

// Another open page of the panel (a second tab, the phone) picks up an install started elsewhere: "none" isn't kept.
func TestUpdProgOtherPage(t *testing.T) {
	out := l34Node(t, []string{"updProgSet", "updProgSave", "updProgGet", "updProgClear", "stopUpdTick"}, `
const mem={};globalThis.localStorage={getItem:k=>mem[k]??null,setItem:(k,v)=>{mem[k]=String(v)},removeItem:k=>{delete mem[k]}};globalThis.clearInterval=()=>{};
console.log('before='+updProgGet());mem['fw-updprog']=JSON.stringify({ver:'v0.5.3',kind:'image',at:Date.now(),ms:240000,down:false});
console.log('after='+(updProgGet()||{}).ver);`)
	if !strings.Contains(out, "before=null") || !strings.Contains(out, "after=v0.5.3") {
		t.Fatalf("%s", out)
	}
}

// The install's answer leaves whole before the hand-off's exec (800 ms later) closes the panel's sockets: a set length,
// flushed in the handler, the connection closed after it.
func TestUpdInstallAnswerSentBeforeHandoff(t *testing.T) {
	rec := httptest.NewRecorder()
	writeJSONHandoff(rec, map[string]any{"ok": true, "note": "x"})
	if h := rec.Header(); h.Get("Content-Length") != strconv.Itoa(rec.Body.Len()) || h.Get("Connection") != "close" || !rec.Flushed ||
		h.Get("Content-Type") != "application/json" || !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Fatalf("helper: %v flushed=%v %q", rec.Header(), rec.Flushed, rec.Body.String())
	}
	// the image install itself (SC Box II): the hand-off started, then this answer
	box2On(t)
	part := int64(6 << 20)
	fakeSystem(t, part, fakeSquashfs(3<<20, 1), 200000)
	ius := fakeIUS(t, stdItems(fakeSquashfs(2<<20, 9)), true)
	post(t, "/api/update/upload?name="+brand+"-firmware-v0.5.3-owner.ius", string(ius))
	s := sha256.Sum256(ius)
	post(t, "/api/update", `{"sums":"`+hex.EncodeToString(s[:])+`  `+brand+`-firmware-v0.5.3-owner.ius"}`)
	if m := post(t, "/api/update", `{"verify":true}`); m["verified"] != true {
		t.Fatalf("verify: %v", m)
	}
	handed := false
	oh, omc := imgHandoff, imgMarkClean
	imgHandoff = func(string) error { handed = true; return nil }
	t.Cleanup(func() {
		imgHandoff, imgMarkClean = oh, omc
		updMu.Lock()
		updBusy = ""
		updMu.Unlock()
		rebootMu.Lock()
		rebootStart, rebootUntil = time.Time{}, time.Time{}
		rebootMu.Unlock()
	})
	rec = httptest.NewRecorder()
	handleUpdate(rec, httptest.NewRequest("POST", "/api/update", strings.NewReader(`{"install":true,"ack":true,"password":"`+c9TestPw+`"}`)))
	if !handed || !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Fatalf("install: handed=%v %s", handed, rec.Body.String())
	}
	if rec.Header().Get("Connection") != "close" || rec.Header().Get("Content-Length") == "" || !rec.Flushed {
		t.Fatalf("the install's answer must be sent whole before the hand-off's exec: %v flushed=%v", rec.Header(), rec.Flushed)
	}
}
