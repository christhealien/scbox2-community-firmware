package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// L31 (Dashboard tab): the page's own functions, taken from indexHTML and run in node against a tiny DOM stub.
// Each case was first seen in a real browser (Playwright against the demo mock miner).

// l31Func cuts "function name(...){...}" out of the page script (braces counted outside strings and // comments).
func l31Func(t *testing.T, name string) string {
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
			for k < len(s) && s[k] != '\n' { // a // comment: its apostrophes aren't quotes
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

func l31Node(t *testing.T, funcs []string, body string) string {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node")
	}
	var b strings.Builder
	b.WriteString(`const window=globalThis;const E={};
function mkEl(id){return {id,innerHTML:'',textContent:'',clientWidth:1200,style:{},classList:{toggle(){},add(){},remove(){}},
 querySelector(){return null},querySelectorAll(){return []},addEventListener(){},insertAdjacentHTML(p,h){this.innerHTML=h+this.innerHTML}};}
const $=id=>E[id]||(E[id]=mkEl(id));
const f1=x=>x==null?'–':(Math.round(x*100)/100);
function gotoPower(){}
// B9 model-profile helpers (stubbed; no _MODEL => SC Lite path, featOff2 always false, limits 90/92).
function MODEL(){return window._MODEL||null;}
function F(){const m=MODEL();return (m&&m.features)||{};}
function isOtherModel(){const m=MODEL();return !!(m&&m.id&&m.id!=='sclite');}
function featOff2(on){return isOtherModel()&&!on;}
function PUB(){const m=MODEL();return !!(m&&m.public);}
function MABORT(){const m=MODEL();return (m&&m.abort_max!=null)?m.abort_max:90;}
function MCUTOFF(){const m=MODEL();return (m&&m.temp_cutoff!=null)?m.temp_cutoff:92;}
` + oemJS(t))
	for _, f := range funcs {
		b.WriteString(l31Func(t, f))
		b.WriteString("\n")
	}
	b.WriteString(body)
	f := filepath.Join(t.TempDir(), "dash.js")
	os.WriteFile(f, []byte(b.String()), 0o644)
	out, err := exec.Command(node, f).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	return string(out)
}

var l31Chart = []string{"renderHR", "hrInspectWire", "hrScale", "hrParts", "fmtHRc", "hrTicks", "renderRejected", "hesc", "fmtWhen", "fmtWhenS"}

// The miner was off (or idle, rebooting, not answering) for 4 hours: the agent records no points then, and the
// chart drew one straight line across the hole at the old rate, as if it had been hashing all along.
func TestL31ChartGapIsAGap(t *testing.T) {
	out := l31Node(t, l31Chart, `
const now=Math.floor(Date.now()/1000),S=[];
for(let t=now-20*3600;t<now-60;t+=60){if(t>now-10*3600&&t<now-6*3600)continue;S.push({t,ghs:4600});}
renderHR({samples:S,marks:[]});
const d=(/class="hrline" d="([^"]*)"/.exec($('hrchart').innerHTML)||[])[1]||'';
console.log('pieces='+(d.match(/M/g)||[]).length);`)
	if !strings.Contains(out, "pieces=2") {
		t.Fatalf("a 4-hour gap in the history must break the hashrate line in two: %s", out)
	}
}

// The caption said "<current rate> on average since its last restart": it printed the dashboard's ghs (the
// boards' rate now) where the Live tile prints the miner's average (ghs_avg).
func TestL31CaptionAverageIsTheAverage(t *testing.T) {
	out := l31Node(t, l31Chart, `
const now=Math.floor(Date.now()/1000),S=[];for(let t=now-3600;t<now;t+=60)S.push({t,ghs:4600});
window._lastDash={ok:true,ghs:3000,ghs_avg:4611,accepted:10,rejected:0,stale:0};
renderHR({samples:S,marks:[]});console.log($('hrcap').innerHTML);`)
	if !strings.Contains(out, "4.611 TH/s on average") || strings.Contains(out, "3.000 TH/s on average") {
		t.Fatalf("the caption's average must be the miner's (4.611 TH/s), not the current rate: %s", out)
	}
}

// Right after the mining software restarted (uptime 25 s, the miner and every board at 0) the Live tile showed
// the last chart point, taken before the restart (4.4 TH/s), for up to a minute.
func TestL31LiveTileAfterRestart(t *testing.T) {
	out := l31Node(t, []string{"liveTiles", "hwSub", "powerTile", "fanLine", "hrParts", "hrScale", "fmtHRc", "hesc"}, `
var pct2=x=>(x>0&&x<0.005)?'<0.01':f1(x);
window._hrNow=4400;window._hrNowT=Math.floor(Date.now()/1000)-120;
const h=liveTiles({ghs:0,ghs_avg:0,elapsed:25,accepted:0,rejected:0,hw_errors:0,hw_pct:0,boards:[{id:0,alive:true,ghs:0,temps:[60,61,62],fans:[48]}],power:{}});
console.log((/<div class="v">([^<]*)/.exec(h)||[])[1]);`)
	if !strings.HasPrefix(strings.TrimSpace(out), "0.00") {
		t.Fatalf("the Live hashrate must not show a reading from before the restart: %q", out)
	}
}

// While the miner gives no reading (Idle, not answering) the dashboard's share counts are just Go's zero values:
// the strip said "Since restart: 0 accepted · 0 rejected".
func TestL31RejectedNoReading(t *testing.T) {
	out := l31Node(t, l31Chart, `
window._hrmarks=[];renderRejected({ok:false,idle:true,err:'The miner is idle',ghs:0,ghs_avg:0,accepted:0,rejected:0,stale:0,hw_errors:0,elapsed:0,boards:null});console.log($('rejbody').innerHTML);`)
	if strings.Contains(out, "0 accepted") {
		t.Fatalf("no reading must not read as zero shares: %s", out)
	}
}

// When the session ends on the server (signed out in another tab, expired), the page's polling gets a 401 and
// shows the sign-in box; signing in runs startApp again, whose start() opened a second log stream next to the
// first (which the server keeps open): every line of the Log tab then came twice.
func TestL31OneLogStream(t *testing.T) {
	out := l31Node(t, []string{"start"}, `
let es=null;const src='mine';function url(){return '/api/engine/stream'}function appendLine(){}
const all=[];globalThis.EventSource=function(u){this.u=u;this.closed=false;this.close=()=>{this.closed=true};all.push(this)};
start();start();console.log('open='+all.filter(e=>!e.closed).length);`)
	if !strings.Contains(out, "open=1") {
		t.Fatalf("start() must leave one log stream open: %s", out)
	}
}
