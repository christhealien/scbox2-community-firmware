package main

import "testing"

// The Box II plan's 4th field is the second voltage-trim PWM, not a fan: it is always the stock plan's own value.
func TestBox2Field4IsStock(t *testing.T) {
	cur := map[string]any{"powerplans": []any{
		map[string]any{"level": 0.0, "info": "690 MHz 0.000000 V 40 RPM 40 RPM"},
		map[string]any{"level": 2.0, "info": "525 MHz 61 V 40 RPM 59 RPM"},
	}}
	for _, c := range []struct {
		mhz, want int
		ok        bool
	}{
		{690, 40, true}, {525, 59, true}, {600, 40, true}, {500, 59, true}, {700, 0, false},
	} {
		got, ok := box2Field4(cur, powerplan{FreqMHz: c.mhz})
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("%d MHz: got %d,%v want %d,%v", c.mhz, got, ok, c.want, c.ok)
		}
	}
	saved := model
	defer func() { model = saved }()
	model = profSCBox2
	model.Feat.ClockControl = false
	if v, _ := box2PlanVerdict(cur, "690 MHz 42 V 67 RPM 67 RPM"); v != "field4" {
		t.Errorf("a fan duty in the 4th field must be re-sent with the stock value, got %q", v)
	}
	if v, _ := box2PlanVerdict(cur, "690 MHz 42 V 67 RPM 40 RPM"); v != "" {
		t.Errorf("stock 4th field: nothing to do, got %q", v)
	}
	if v, _ := box2PlanVerdict(cur, "600 MHz 42 V 67 RPM 40 RPM"); v != "stock" {
		t.Errorf("600 MHz with ClockControl off must go back to stock, got %q", v)
	}
}

// The OEM's backend rewrites the 690 MHz plan's info as "0.000000 V" (seen on hardware): the code comes from
// the entry's voltage field, so fan writes on that plan aren't refused as "690 MHz / 0 V".
func TestBox2StockPlanZeroVInfo(t *testing.T) {
	saved := model
	defer func() { model = saved }()
	model = profSCBox2
	cur := map[string]any{"select": 0.0, "manual": false, "powerplans": []any{
		map[string]any{"level": 0.0, "info": "690 MHz 0.000000 V 40 RPM 40 RPM", "voltage": 42.00001},
		map[string]any{"level": 2.0, "info": "525 MHz 61 V 40 RPM 59 RPM", "voltage": 61.00001},
	}}
	pp, ok := resolveRunningPlan(cur)
	if !ok || pp.FreqMHz != 690 || pp.VrunMv != 42 || !box2StockMatch(pp) {
		t.Fatalf("running plan %+v ok=%v: want 690 MHz / code 42, a stock tuple", pp, ok)
	}
	if n := len(box2ModesFrom(cur)); n != 2 {
		t.Fatalf("both stock modes must be offered, got %d", n)
	}
}

// "Stock" on the Box II is the factory default (690 MHz / level 42), not whichever stock mode is selected:
// picking Low-power moves the firmware's select to the 525 MHz plan, and the Stock card must not follow it.
func TestBox2StockIsFactoryDefault(t *testing.T) {
	saved := model
	defer func() { model = saved }()
	model = profSCBox2
	cur := map[string]any{"select": 1.0, "manual": false, "powerplans": []any{
		map[string]any{"level": 0.0, "info": "690 MHz 0.000000 V 40 RPM 40 RPM", "voltage": 42.00001},
		map[string]any{"level": 2.0, "info": "525 MHz 61 V 40 RPM 59 RPM", "voltage": 61.00001},
	}}
	i := box2DefaultIndex(cur)
	pp, ok := stockPlanAt(cur, i)
	if i != 0 || !ok || pp.FreqMHz != 690 || pp.VrunMv != 42 {
		t.Fatalf("stock = index %d %+v, want the 690 MHz / 42 plan", i, pp)
	}
	model = profSCLite
	if box2DefaultIndex(cur) != -1 {
		t.Fatal("other models keep the firmware's selected level")
	}
}
