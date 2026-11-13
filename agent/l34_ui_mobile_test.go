package main

import (
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// L34 (phones, light theme, keyboard, slow network and API errors): the page's own functions, cut out of
// indexHTML and run in node against a tiny DOM stub. Each case was first seen in a real browser (Playwright
// against the demo mock miner: tests/l34_mobile.js in the L34 folder).

// l34Func cuts "function name(...){...}" out of the page script (braces counted outside strings and // comments).
func l34Func(t *testing.T, name string) string {
	t.Helper()
	i := strings.Index(indexHTML, "\nfunction "+name+"(")
	if i < 0 {
		i = strings.Index(indexHTML, "\nasync function "+name+"(")
	}
	if i < 0 {
		t.Fatalf("no function %s in the page", name)
	}
	s := indexHTML[i+1:]
	depth, q := 0, byte(0)
	for k := 0; k < len(s); k++ {
		c := s[k]
		if q != 0 {
			if c == '\\' {
				k++
			} else if c == q {
				q = 0
			}
			continue
		}
		if c == '/' && k+1 < len(s) && s[k+1] == '/' && k > 0 && strings.IndexByte(" \t\n;{}", s[k-1]) >= 0 {
			for k < len(s) && s[k] != '\n' {
				k++
			}
			continue
		}
		switch c {
		case '\'', '"', '`':
			q = c
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[:k+1]
			}
		}
	}
	t.Fatalf("function %s doesn't end", name)
	return ""
}

func l34Node(t *testing.T, funcs []string, body string) string {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node")
	}
	var b strings.Builder
	b.WriteString(`const window=globalThis;const E={};
function mkEl(id){return {id,innerHTML:'',textContent:'',value:'',checked:false,style:{},classList:{toggle(){},add(){},remove(){},contains(){return false}},
 querySelector(){return null},querySelectorAll(){return []},addEventListener(){},closest(){return {classList:{contains(){return true}}}}};}
const $=id=>E[id]||(E[id]=mkEl(id));
const OFFLINE='Can’t reach the miner just now. Reconnecting…';
const calls={jpost:0,retry:0,toast:[]};
async function jpost(){calls.jpost++;return {s:200,j:{ok:true}};}
function retryLater(fn){calls.retry++;}
function toast(m){calls.toast.push(m);}
function confirm(){return true;}
function boot(){}
function hesc(s){return String(s);}
// B9 model-profile helpers (the real page defines these; stub them here so loaders that read the model
// profile work. With no _MODEL set this is the SC Lite path: featOff2 is always false, limits 90/92.)
function MODEL(){return window._MODEL||null;}
function F(){const m=MODEL();return (m&&m.features)||{};}
function isOtherModel(){const m=MODEL();return !!(m&&m.id&&m.id!=='sclite');}
function featOff2(on){return isOtherModel()&&!on;}
function featNoteHTML(l){return String(l)+' is not available on this model';}
function featBlock(id,on,l){if(!featOff2(on))return false;const e=$(id);if(e)e.innerHTML=featNoteHTML(l);return true;}
function MABORT(){const m=MODEL();return (m&&m.abort_max!=null)?m.abort_max:90;}
function MCUTOFF(){const m=MODEL();return (m&&m.temp_cutoff!=null)?m.temp_cutoff:92;}
` + oemJS(t))
	for _, f := range funcs {
		b.WriteString(l34Func(t, f))
		b.WriteString("\n")
	}
	b.WriteString("(async()=>{" + body + "})().catch(e=>console.log('THROWN '+e));")
	f := filepath.Join(t.TempDir(), "l34.js")
	os.WriteFile(f, []byte(b.String()), 0o644)
	out, err := exec.Command(node, f).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	return string(out)
}

// The Schedule tab's first load failed once (an HTTP 500, or the phone's Wi-Fi blinked): the tab stayed empty
// ("—", "Schedule on" unticked, no changes) even after the miner answered again, and "Save schedule" saved
// that empty page: the owner's 8-change schedule became enabled=false, rules=[] (Playwright: 1a).
func TestL34ScheduleNotSavedBeforeItLoads(t *testing.T) {
	out := l34Node(t, []string{"scSave", "scClear"}, `
Object.assign(globalThis,{SC:null,scRules:[],scBase:null,scCheck:()=>[],scRsBody:()=>({}),loadSchedule:async()=>{},scFmtD:()=>''});
try{await scSave();}catch(e){console.log('scSave threw '+e);}try{await scClear();}catch(e){console.log('scClear threw '+e);}console.log('jpost='+calls.jpost+' toast='+JSON.stringify(calls.toast));`)
	if !strings.Contains(out, "jpost=0") {
		t.Fatalf("Save/Clear on a schedule that never loaded must not post (it wipes the saved one): %s", out)
	}
}

// ...and the failed load must say so and try again by itself (it only told a raw JSON parser error in a toast
// and never loaded until the owner switched tabs).
func TestL34ScheduleLoadRetries(t *testing.T) {
	out := l34Node(t, []string{"loadSchedule"}, `
globalThis.SC=null;globalThis.fetch=async()=>({status:500,json:async()=>{throw new SyntaxError('Unexpected token \'<\', "<html>" is not valid JSON')}});
await loadSchedule();console.log('retry='+calls.retry+' next='+$('sc_next').textContent+' toast='+JSON.stringify(calls.toast));`)
	if !strings.Contains(out, "retry=1") || !strings.Contains(out, "Couldn’t load the schedule") || strings.Contains(out, "Unexpected token") {
		t.Fatalf("a failed schedule load must explain itself and retry: %s", out)
	}
}

// Tuner tab: one failed /api/tuner left "tuner error" there for good (no retry, no reason).
func TestL34TunerLoadRetries(t *testing.T) {
	out := l34Node(t, []string{"loadTuner", "busyHTML"}, `
globalThis.TUNERDEF=null;globalThis.fetch=async()=>{throw new TypeError('Failed to fetch')};
await loadTuner();console.log('retry='+calls.retry+' body='+$('tunerbody').innerHTML);`)
	if !strings.Contains(out, "retry=1") || strings.Contains(out, "tuner error") {
		t.Fatalf("a failed tuner load must say why and retry: %s", out)
	}
}

// Dashboard: a request that never answers (a dead connection) left the last figures up as if live, with no
// notice, for as long as it hung (Playwright: 100 s, 49 requests piling up); and an HTTP 500 error page was
// reported as "Can't reach the miner".
func TestL34DashboardTimeoutAndHTTPError(t *testing.T) {
	out := l34Node(t, []string{"refresh", "fetchTO", "busyHTML", "updProgShow", "updProgGet"}, `
let seen=null;globalThis.fetch=async(u,o)=>{seen=o;return {status:500,json:async()=>{throw new SyntaxError('bad json')}}};
await refresh();console.log('signal='+!!(seen&&seen.signal)+' ov='+$('ov').innerHTML);`)
	if !strings.Contains(out, "signal=true") {
		t.Fatalf("the dashboard's request must time out: %s", out)
	}
	if !strings.Contains(out, "HTTP 500") {
		t.Fatalf("an HTTP 500 page must be reported as an error answer, not as unreachable: %s", out)
	}
}

// Log tab, light theme: the line colours were the dark theme's (green 1.88:1, yellow 1.59:1, orange 2.04:1,
// blue 1.88:1, red 3.03:1 on white).
func TestL34LightLogContrast(t *testing.T) {
	lum := func(h string) float64 {
		var c [3]float64
		for i := 0; i < 3; i++ {
			v, _ := strconv.ParseUint(h[1+2*i:3+2*i], 16, 8)
			x := float64(v) / 255
			if x <= 0.03928 {
				c[i] = x / 12.92
			} else {
				c[i] = math.Pow((x+0.055)/1.055, 2.4)
			}
		}
		return 0.2126*c[0] + 0.7152*c[1] + 0.0722*c[2]
	}
	bg := lum("#ffffff") // --logbg in the light theme
	for _, cls := range []string{"accept", "reject", "invalid", "nonce", "best", "pool", "dim"} {
		col := ""
		for _, re := range []string{`(?m)^ *#log \.ln\.` + cls + `\{color:(#[0-9a-f]{6})`, `#log \.ln\.` + cls + `\{color:(#[0-9a-f]{6})`,
			`:root\[data-theme="light"\] #log \.ln\.` + cls + `\{color:(#[0-9a-f]{6})`} {
			if m := regexp.MustCompile(re).FindAllStringSubmatch(indexHTML, -1); len(m) > 0 {
				col = m[len(m)-1][1] // the light rule, when there is one, comes last
			}
		}
		if col == "" {
			t.Fatalf("no colour for #log .ln.%s", cls)
		}
		f := lum(col)
		if cr := (math.Max(f, bg) + 0.05) / (math.Min(f, bg) + 0.05); cr < 4.5 {
			t.Errorf("light theme: log lines .%s %s on white = %.2f:1 (needs 4.5)", cls, col, cr)
		}
	}
}

// Phones: the schedule week (half the screen on a 390 px phone) had touch-action:none, so a swipe there didn't
// scroll the page and painted the brush down the days instead (Playwright: 6a).
func TestL34ScheduleWeekScrollsOnTouch(t *testing.T) {
	m := regexp.MustCompile(`\.sc-blocks\{[^}]*touch-action:([a-z-]+)`).FindStringSubmatch(indexHTML)
	if m == nil || m[1] != "pan-y" {
		t.Fatalf("the week grid must let a finger scroll the page up and down: %v", m)
	}
	if !strings.Contains(indexHTML, "addEventListener('pointercancel'") {
		t.Fatal("a touch the browser turns into a scroll (pointercancel) must not paint")
	}
}
