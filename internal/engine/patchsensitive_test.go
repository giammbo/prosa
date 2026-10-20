package engine

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/giammbo/prosa/internal/lock"
)

// A patch that sets a sensitive attribute to a literal is rejected like the
// complete state would be (ADR-011): the planner's Validate says so, and a
// planner that submits it anyway fails the plan, leaving no lock.
func TestPatchSensitiveRejected(t *testing.T) {
	patch := []lock.PatchOp{{Op: "set", Key: "db", Path: "password", Value: "hunter2"}}
	var validateErr error
	pl := plannerFunc(func(ctx context.Context, in PlanInput) (Desired, error) {
		if in.Mode == "plan" {
			return Desired{Resources: []DesiredResource{{Key: "db", Type: "test_db", Config: map[string]any{"name": "db"}}}}, nil
		}
		validateErr = in.Validate(ctx, Desired{Patch: patch})
		return Desired{Patch: patch}, nil
	})
	e := newTestEnv(t, pl)
	e.writeIntent("app", "app v1")
	e.mustApply(e.plan())
	e.writeIntent("app", "app v2")
	before, _ := os.ReadFile(e.lockPath)

	_, err := e.engine.Plan(context.Background())
	if err == nil || !strings.Contains(err.Error(), "sensitive") || !strings.Contains(err.Error(), "ADR-011") {
		t.Fatalf("plan err = %v, want a sensitive/ADR-011 error", err)
	}
	if validateErr == nil || !strings.Contains(validateErr.Error(), "ADR-011") {
		t.Fatalf("Validate err = %v, want the sensitive-literal problem", validateErr)
	}
	after, _ := os.ReadFile(e.lockPath)
	if string(before) != string(after) || strings.Contains(string(after), "hunter2") {
		t.Errorf("lock changed or holds the literal:\n%s", after)
	}
}

// A literal in a patch op is rejected even when a later op overrides it, so
// that the resolved state is clean (ADR-011): the patch itself is stored in
// the lock and in the planner cache, and must not carry the secret. The same
// for an add whose config holds one, removed afterwards.
func TestPatchOverriddenSensitiveRejected(t *testing.T) {
	cases := map[string][]lock.PatchOp{
		"set then unset": {
			{Op: "set", Key: "db", Path: "password", Value: "hunter2"},
			{Op: "unset", Key: "db", Path: "password"},
		},
		"add then remove": {
			{Op: "add", Key: "db2", Type: "test_db", Config: map[string]any{"name": "db2", "password": "hunter2"}},
			{Op: "remove", Key: "db2"},
		},
	}
	for name, patch := range cases {
		t.Run(name, func(t *testing.T) {
			var validateErr error
			pl := plannerFunc(func(ctx context.Context, in PlanInput) (Desired, error) {
				if in.Mode == "plan" {
					return Desired{Resources: []DesiredResource{{Key: "db", Type: "test_db", Config: map[string]any{"name": "db"}}}}, nil
				}
				validateErr = in.Validate(ctx, Desired{Patch: patch})
				return Desired{Patch: patch}, nil
			})
			e := newTestEnv(t, pl)
			e.writeIntent("app", "app v1")
			e.mustApply(e.plan())
			e.writeIntent("app", "app v2")
			before, _ := os.ReadFile(e.lockPath)

			_, err := e.engine.Plan(context.Background())
			if err == nil || !strings.Contains(err.Error(), "ADR-011") {
				t.Fatalf("plan err = %v, want the sensitive-literal error", err)
			}
			if validateErr == nil || !strings.Contains(validateErr.Error(), "ADR-011") {
				t.Fatalf("Validate err = %v, want the sensitive-literal problem", validateErr)
			}
			after, _ := os.ReadFile(e.lockPath)
			if string(before) != string(after) {
				t.Errorf("lock changed:\n%s", after)
			}
		})
	}
}
