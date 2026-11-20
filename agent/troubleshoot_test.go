package main

// Settings → Troubleshooting: the system-log relay's own panel (the last one in Settings), offered on every SC Box II
// build and hidden (empty) on the SC Lite.

import (
	"html"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// troubleshootingJS: the page's functions the Troubleshooting panel draws with.
var troubleshootingJS = []string{"renderTroubleshooting", "imgMore", "tsHelpHTML", "tsCode", "tsRecvCmd"}

// the receiver commands the help gives, exactly as they are to be copied
const (
	tsWantUnix = `sudo python3 -u -c "import socket;s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM);s.bind(('0.0.0.0',514));[print(s.recv(4096).decode(errors='replace').strip(),flush=True) for _ in iter(int,1)]" | tee scbox2-syslog.txt`
	tsWantWin  = `python -u -c "import socket;s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM);s.bind(('0.0.0.0',514));[print(s.recv(4096).decode(errors='replace').strip(),flush=True) for _ in iter(int,1)]" | tee scbox2-syslog.txt`
)

// the page's own hesc, for the node runs (l34Node's stub doesn't escape)
const tsHesc = `hesc=function(s){return String(s==null?'':s).replace(/[<>&"']/g,c=>({'<':'&lt;','>':'&gt;','&':'&amp;','"':'&quot;',"'":'&#39;'}[c]));};`

// The Troubleshooting panel is the very last panel of the Settings tab, after "Support this project", with its
// header and note, hidden until the panel says the relay is available.
func TestTroubleshootingLastSettingsPanel(t *testing.T) {
	for name, page := range map[string]string{"served": indexPage, "public": pageForVariant(indexPage, true)} {
		i := strings.Index(page, `data-pane="settings"`)
		j := strings.Index(page[i:], "</main>")
		if i < 0 || j < 0 {
			t.Fatalf("%s: no Settings tab", name)
		}
		pane := page[i : i+j]
		panels := regexp.MustCompile(`<div class="panel[^"]*"[^>]*>`).FindAllStringIndex(pane, -1)
		if len(panels) < 2 {
			t.Fatalf("%s: no panels", name)
		}
		last := pane[panels[len(panels)-1][0]:]
		if !strings.HasPrefix(last, `<div class="panel hide" id="tspanel"><div class="hdr"><b>Troubleshooting</b><span class="muted" style="font-size:12px">for diagnosing freezes and unexpected restarts</span></div>`) {
			t.Fatalf("%s: the last Settings panel isn't Troubleshooting: %.200s", name, last)
		}
		if !strings.Contains(pane[panels[len(panels)-2][0]:panels[len(panels)-1][0]], "<b>Support this project</b>") {
			t.Fatalf("%s: Troubleshooting doesn't follow Support this project", name)
		}
		if strings.Count(page, `id="tspanel"`) != 1 || strings.Contains(pane, `id="recsyslog"`) {
			t.Fatalf("%s: one Troubleshooting panel, its field drawn only by the script", name)
		}
	}
}

// The help: the intro, the unencrypted-LAN note, the receiver commands (escaped for the page, copied exactly as
// given), the firewall note, the steps and what to send; Recovery points to it.
func TestTroubleshootingHelp(t *testing.T) {
	out := l34Node(t, append([]string{"renderRecoveryBox2", "fmtWhen"}, troubleshootingJS...), tsHesc+`
window._MODEL={id:'scbox2'};
renderTroubleshooting({ok:true,syslog_remote_avail:true,syslog_remote:''});console.log('=== TS '+$('tsbody').innerHTML);
renderRecoveryBox2({ok:true,keep_log_flash:true,keep_log_every_min:30,syslog_remote_avail:true});console.log('=== REC '+$('recbody').innerHTML);`)
	ts, rec := between(out, "=== TS ", "\n"), between(out, "=== REC ", "\n")
	for _, want := range []string{
		"If the miner freezes or restarts by itself, this sends every system-log line to your computer as it happens, so the last lines before a freeze are kept. Leave it off otherwise.",
		"<b>unencrypted</b> over the local network: point it only at your own computer on the same network. An IPv4 address only",
		"port 514 by default",
		`<details id="tshelp"`, "How to receive it on your computer", "What it sends",
		"sudo ufw allow 514/udp", "ip -4 addr", "ipconfig", "as address:514", "press <b>Save</b>", "Check that lines appear",
		"Reproduce the problem", "last ~100 lines before the freeze", "the private log export",
		`id="recsyslog" placeholder="192.168.1.20:514"`, "Off.",
	} {
		if !strings.Contains(ts, want) {
			t.Errorf("the help has no %q", want)
		}
	}
	for id, want := range map[string]string{"tscmdunix": tsWantUnix, "tscmdwin": tsWantWin} {
		m := regexp.MustCompile(`<pre class="mono" id="` + id + `"[^>]*>([^<]*)</pre><button onclick="tsCopy\('` + id + `'\)"`).FindStringSubmatch(ts)
		if m == nil {
			t.Fatalf("no copyable block %s: %s", id, ts)
		}
		if strings.ContainsAny(m[1], `"'<>`) || html.UnescapeString(m[1]) != want {
			t.Errorf("%s: %q, want %q escaped", id, m[1], want)
		}
	}
	if strings.Contains(rec, "recsyslog") || strings.Contains(rec, "(below)") || !strings.Contains(rec, "Troubleshooting, at the bottom of Settings") {
		t.Errorf("Recovery: no relay field, a pointer to Troubleshooting: %s", rec)
	}
	if !strings.Contains(rec, "Keep the system log across restarts") {
		t.Errorf("Recovery lost its Keep-log setting: %s", rec)
	}
}

// The SC Lite (no relay in its answer): the panel stays hidden and empty, Recovery is drawn as before; the SC Box
// II's answer shows it.
func TestTroubleshootingLiteHidden(t *testing.T) {
	model, modelWhy = profSCLite, "test"
	setWindowsFromModel()
	t.Cleanup(func() { model, modelWhy = profSCLite, "test"; setWindowsFromModel() })
	if _, ok := recoveryView()["syslog_remote_avail"]; ok {
		t.Fatal("the SC Lite's Recovery answer offers the relay")
	}
	out := l34Node(t, append([]string{"renderRecovery", "renderRecoveryBox2", "fmtWhen"}, troubleshootingJS...), tsHesc+`
const cl=new Set(['panel','hide']);$('tspanel').classList={add(c){cl.add(c)},remove(c){cl.delete(c)},toggle(c,on){on?cl.add(c):cl.delete(c)},contains(c){return cl.has(c)}};
renderRecovery({ok:true,keep_log:false});
console.log('=== LITE hide='+cl.has('hide')+' ts=['+$('tsbody').innerHTML+'] rec='+$('recbody').innerHTML);
window._MODEL={id:'scbox2'};
renderRecovery({ok:true,keep_log:false,syslog_remote_avail:true,syslog_remote:'192.168.1.20:514'});
console.log('=== BOX2 hide='+cl.has('hide')+' ts='+$('tsbody').innerHTML);
renderRecovery({ok:true,keep_log:false});
console.log('=== BOX2OFF hide='+cl.has('hide')+' ts=['+$('tsbody').innerHTML+']');`)
	lite := between(out, "=== LITE ", "\n")
	if !strings.HasPrefix(lite, "hide=true ts=[]") || strings.Contains(lite, "recsyslog") || strings.Contains(lite, "Troubleshooting") || !strings.Contains(lite, "Keep the system log across restarts") {
		t.Fatalf("the SC Lite: %s", lite)
	}
	if b := between(out, "=== BOX2 ", "\n"); !strings.HasPrefix(b, "hide=false") || !strings.Contains(b, "On: every line is also sent to 192.168.1.20:514.") {
		t.Fatalf("the SC Box II: %s", b)
	}
	if b := between(out, "=== BOX2OFF ", "\n"); b != "hide=true ts=[]" {
		t.Fatalf("an answer without the relay hides it again: %s", b)
	}
}

// The page's script parses for each variant this build can serve (node --check): the served page, and on the
// owner's build the public variant too. Run under each tag set (none = SC Lite, scbox2, scbox2,public).
func TestServedPageJSParses(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node")
	}
	pages := map[string]string{"served": indexPage}
	if !publicBuild {
		pages["public"] = pageForVariant(indexPage, true)
	}
	re := regexp.MustCompile(`(?s)<script>(.*?)</script>`)
	for name, page := range pages {
		ms := re.FindAllStringSubmatch(page, -1)
		if len(ms) == 0 {
			t.Fatalf("%s: no script", name)
		}
		for k, m := range ms {
			f := filepath.Join(t.TempDir(), name+".js")
			os.WriteFile(f, []byte(m[1]), 0o644)
			if out, err := exec.Command(node, "--check", f).CombinedOutput(); err != nil {
				t.Fatalf("%s script %d doesn't parse: %v\n%s", name, k, err, out)
			}
		}
		if !strings.Contains(page, "function renderTroubleshooting(") || !strings.Contains(page, `id="tspanel"`) {
			t.Fatalf("%s: no Troubleshooting panel", name)
		}
	}
}
