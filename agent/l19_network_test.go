package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// l19NoShell: setHostname's `sh` can't be found, so a test never renames this machine or remounts its /.
func l19NoShell(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
}

// F: "localhost" passes the hostname check, but dhcpcd's 30-hostname hook (need_hostname) treats it as no name
// at all and puts the router's name (as name.domain) in its place at the next lease: the owner's choice is lost,
// the tuner's folder (named after the host) moves away, and the name the owner uses no longer exists.
// (the hook itself: tests/dhcpcd_hostname_test.sh)
func TestL19HostnameLocalhostRefused(t *testing.T) {
	l03Env(t, false)
	l19NoShell(t)
	h := l03Mux()
	c := issueCookie("zq7owner")
	rr := l03Req(h, "POST", "/api/net", `{"hostname":"localhost"}`, c, nil)
	var out map[string]any
	json.Unmarshal(rr.Body.Bytes(), &out)
	if out["ok"] == true {
		t.Fatalf("/api/net accepted the hostname localhost: %s", rr.Body.String())
	}
}

// F: Settings shows (and compares the port field with) only the port the panel is on now, never the one saved
// for the next start. An owner who saved 9000 and then puts 8080 back and saves is told "nothing changed",
// and the panel moves to 9000 at the next reboot. The settings must say which port is saved, and the page
// must send the port when it differs from that.
func TestL19PendingPortCanBeReverted(t *testing.T) {
	l03Env(t, false)
	h := l03Mux()
	c := issueCookie("zq7owner")
	old := *listenAddr
	*listenAddr = ":8080"
	t.Cleanup(func() { *listenAddr = old })
	if err := saveNetConfig(netConfig{Port: 47401}); err != nil {
		t.Fatal(err)
	}
	rr := l03Req(h, "GET", "/api/settings", "", c, nil)
	var s struct {
		Network map[string]any `json:"network"`
	}
	json.Unmarshal(rr.Body.Bytes(), &s)
	if v, _ := s.Network["port_saved"].(float64); v != 47401 {
		t.Errorf("settings don't tell the page which port is saved for the next start: %v", s.Network)
	}
	// the page's own saveNetwork, run in node with the settings above: putting 8080 back must post it
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node")
	}
	src, _ := os.ReadFile("main.go")
	fn := func(name string) string {
		m := regexp.MustCompile(`(?s)\n(async )?function ` + name + `\(.*?\n(async )?function `).Find(src)
		if m == nil {
			return ""
		}
		b := string(m)
		return b[:strings.LastIndex(b, "\n")]
	}
	nb, _ := json.Marshal(s.Network)
	js := `let posted=null;const vals={s_host:'',s_port:'8080'};
const $=id=>({get value(){return vals[id]},set textContent(v){},set innerHTML(v){}});
const window={_settings:{network:` + string(nb) + `}};
async function saveMAC(){return null}
function confirm(){return true}
function toast(){}
async function jpost(u,p){posted=p;return {j:{ok:true}}}
` + fn("netPorts") + "\n" + fn("saveNetwork") + `
saveNetwork().then(()=>{console.log(JSON.stringify(posted))});`
	f := filepath.Join(t.TempDir(), "net.js")
	os.WriteFile(f, []byte(js), 0o644)
	out, err := exec.Command(node, f).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), `"port":8080`) {
		t.Errorf("putting the panel's current port back (8080, with 47401 saved) posts %s: the saved 47401 stays and applies at the next reboot", strings.TrimSpace(string(out)))
	}
}
