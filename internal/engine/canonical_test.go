package engine

import (
	"context"
	"reflect"
	"testing"
)

func TestCanonicalWrapsLoneObjects(t *testing.T) {
	b := newFakeProvider().schema.Resources["test_lb"].Block
	cfg := map[string]any{
		"name":   "x",
		"health": map[string]any{"path": "/h"},
		"rule":   map[string]any{"path": "/r"},
		"opts":   map[string]any{"inner": map[string]any{"flag": true}},
	}
	got := canonicalConfig(b, cfg)
	want := map[string]any{
		"name":   "x",
		"health": []any{map[string]any{"path": "/h"}},
		"rule":   map[string]any{"path": "/r"}, // no max 1: left for validation to reject
		"opts":   map[string]any{"inner": []any{map[string]any{"flag": true}}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("canonicalConfig =\n%#v\nwant\n%#v", got, want)
	}
	if _, isList := cfg["health"].([]any); isList {
		t.Error("canonicalConfig modified its input")
	}
	// Lists and unknown names are kept as they are.
	again := canonicalConfig(b, want)
	if !reflect.DeepEqual(again, want) {
		t.Errorf("canonical form changed: %#v", again)
	}
	odd := map[string]any{"nope": map[string]any{"a": 1}, "health": "x"}
	if got := canonicalConfig(b, odd); !reflect.DeepEqual(got, odd) {
		t.Errorf("odd config changed: %#v", got)
	}
}

func TestPlanCanonicalizesLoneObjects(t *testing.T) {
	lone := Desired{Resources: []DesiredResource{
		{Key: "lb", Type: "test_lb", Config: map[string]any{"name": "lb", "health": map[string]any{"path": "/h"}}},
	}}
	var canonical Desired
	var validateErr error
	rp := &recordingPlanner{desired: map[string]Desired{"app": lone}}
	rp.check = func(ctx context.Context, in PlanInput) {
		if in.Canonical == nil {
			t.Fatal("PlanInput.Canonical is nil")
		}
		canonical = in.Canonical(lone)
		validateErr = in.Validate(ctx, lone)
	}
	e := newTestEnv(t, rp)
	e.writeIntent("app", "a load balancer")
	e.mustApply(e.plan())

	if validateErr != nil {
		t.Errorf("Validate(lone object) = %v, want nil", validateErr)
	}
	wantHealth := []any{map[string]any{"path": "/h"}}
	if got := canonical.Resources[0].Config["health"]; !reflect.DeepEqual(got, wantHealth) {
		t.Errorf("Canonical health = %#v, want %#v", got, wantHealth)
	}
	if _, isList := lone.Resources[0].Config["health"].([]any); isList {
		t.Error("Canonical modified its input")
	}
	chosen := e.loadLock().Intents["app"].Resources[0].Chosen
	if got := chosen["health"]; !reflect.DeepEqual(got, wantHealth) {
		t.Errorf("lock chosen health = %#v, want %#v", got, wantHealth)
	}
}
