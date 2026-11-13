package main

import (
	"os"
	"testing"
)

// extracted stock root filesystems (not in the repo): R14_BOX2_ROOT / R14_LITE_ROOT, else the test skips
var r14Box2Root = os.Getenv("R14_BOX2_ROOT")
var r14LiteRoot = os.Getenv("R14_LITE_ROOT")

func TestR14LoadModelRealFiles(t *testing.T) {
	if r14Box2Root == "" || r14LiteRoot == "" {
		t.Skip("set R14_BOX2_ROOT and R14_LITE_ROOT to extracted stock root filesystems")
	}
	for _, c := range []struct{ root, want string }{{r14LiteRoot, "sclite"}, {r14Box2Root, "scbox2"}} {
		p, why := loadModelFiles(c.root+"/root/product.json", c.root+"/root/mcbversion")
		t.Logf("%s -> %s (%s) boards=%d chips=%d fans=%d cutoff=%.0f", c.root, p.ID, why, p.Boards, p.ChipsPerBoard, p.Fans, p.TempCutoffC)
		if p.ID != c.want {
			t.Fatalf("%s: got %s want %s (%s)", c.root, p.ID, c.want, why)
		}
	}
	// SC Lite product.json with the Box II's mcbversion: unknown
	b, _ := os.ReadFile(r14LiteRoot + "/root/product.json")
	if p, why := loadModel(b, "MCB_V6_3_5"); p.ID != "unknown" {
		t.Fatalf("mismatch accepted: %s", why)
	}
	// every profile's own guards stay under its cut-off
	for _, p := range []modelProfile{profSCLite, profSCBox2} {
		if !(p.HeatGuardC < p.TempCutoffC && p.AbortMaxC < p.TempCutoffC && p.AbortDefC <= p.AbortMaxC && p.ThrottleDefC < p.TempCutoffC) {
			t.Fatalf("%s: guards not under cut-off", p.ID)
		}
	}
}
