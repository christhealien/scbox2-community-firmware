package main

import (
	"strings"
	"testing"
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
