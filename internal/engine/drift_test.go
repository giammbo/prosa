package engine

import (
	"bytes"
	"strings"
	"testing"

	"github.com/zclconf/go-cty/cty"
)

func TestPlanComputedOnlyChangeIsNotDrift(t *testing.T) {
	e := newTestEnv(t, failingPlanner)
	h := e.writeIntent("app", "a clock")
	e.seed("app", h, seedRes{key: "clock", typ: "test_clock", chosen: map[string]any{"name": "clock"},
		state: map[string]any{"id": "test_clock-1", "name": "clock", "read_at": "t0"}})
	e.fake.tickOnRead["test_clock"] = "read_at"

	p := e.plan()
	c := p.Changes[0]
	if c.Action != NoOp || c.Drifted || len(c.DriftAttrs) != 0 || len(c.InfoDrift) != 0 {
		t.Fatalf("change = %s Drifted=%v DriftAttrs=%v InfoDrift=%v, want a no-op without drift", c.Action, c.Drifted, c.DriftAttrs, c.InfoDrift)
	}
}

func TestPlanNullVersusEmptyIsNotDrift(t *testing.T) {
	e := newTestEnv(t, failingPlanner)
	h := e.writeIntent("app", "a list")
	e.seed("app", h, seedRes{key: "l", typ: "test_list", chosen: map[string]any{"name": "l", "groups": map[string]any{"a": []any{}}},
		state: map[string]any{"id": "test_list-1", "name": "l", "items": []any{}, "groups": map[string]any{"a": []any{}}}})
	// The provider now reports null where the store has empty collections.
	e.fake.remote["test_list-1"] = cty.ObjectVal(map[string]cty.Value{
		"id": cty.StringVal("test_list-1"), "name": cty.StringVal("l"),
		"items":  cty.NullVal(cty.List(cty.String)),
		"groups": cty.MapVal(map[string]cty.Value{"a": cty.NullVal(cty.List(cty.String))}),
	})

	c := e.plan().Changes[0] // the fake plans an update: it does not normalize
	if c.Drifted || len(c.DriftAttrs) != 0 || len(c.InfoDrift) != 0 {
		t.Fatalf("Drifted=%v DriftAttrs=%v InfoDrift=%v, want no drift", c.Drifted, c.DriftAttrs, c.InfoDrift)
	}
}

func TestPlanDriftClassifiesConfiguredAttributes(t *testing.T) {
	e := newTestEnv(t, failingPlanner)
	h := e.writeIntent("app", "a db")
	e.seed("app", h, seedRes{key: "db", typ: "test_db", chosen: map[string]any{"name": "db", "size": 10},
		state: map[string]any{"id": "test_db-1", "name": "db", "size": 10, "tags": map[string]any{"prosa/intent": "app"}}})
	// Out of band: the configured size and the unconfigured net_id change.
	vals := e.fake.remote["test_db-1"].AsValueMap()
	vals["size"], vals["net_id"] = cty.NumberIntVal(20), cty.StringVal("net-x")
	e.fake.remote["test_db-1"] = cty.ObjectVal(vals)

	p := e.plan()
	c := p.Changes[0]
	if c.Action != Update || !c.Drifted || !equalStrings(c.DriftAttrs, []string{"size"}) || !equalStrings(c.InfoDrift, []string{"net_id"}) {
		t.Fatalf("change = %s Drifted=%v DriftAttrs=%v InfoDrift=%v", c.Action, c.Drifted, c.DriftAttrs, c.InfoDrift)
	}
	var buf bytes.Buffer
	Render(&buf, p, RenderOptions{})
	if out := buf.String(); !strings.Contains(out, "  # app/db will be updated in-place\n  # (needs approval)\n  # (drifted outside Prosa: size)\n") ||
		!strings.Contains(out, "      ~ size   = 20 -> 10\n") {
		t.Fatalf("render = %q", buf.String())
	}

	// Only an unconfigured attribute changed: informational, not Drifted.
	vals["size"] = cty.NumberIntVal(10)
	e.fake.remote["test_db-1"] = cty.ObjectVal(vals)
	c = e.plan().Changes[0]
	if c.Drifted || len(c.DriftAttrs) != 0 || !equalStrings(c.InfoDrift, []string{"net_id"}) {
		t.Fatalf("level-1 only: Drifted=%v DriftAttrs=%v InfoDrift=%v", c.Drifted, c.DriftAttrs, c.InfoDrift)
	}
}
