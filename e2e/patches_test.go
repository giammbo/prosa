package e2e

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/giammbo/prosa/internal/lock"
)

// The simulated planner works from three readable desired states per intent
// (testdata/llm/states): the base, planned from the intent alone, and the
// result of each environment. The patch of an environment is not written by
// hand: it is the difference between the base and the environment's result,
// computed by diffStates, so that the base plus the patch is exactly the
// state that is applied to Moto (ADR-030).

// diffStates returns the patch that turns the base into env: remove for the
// resources env lacks, add for the new ones, and for the others set and
// unset of the differing attributes (the maps, such as tags, down to the
// differing key; lists and blocks whole), depends_on, and the complete list
// of checks when it differs.
func diffStates(base, env desiredState) []lock.PatchOp {
	ops := []lock.PatchOp{}
	baseByKey := map[string]stateResource{}
	for _, r := range base.Resources {
		baseByKey[r.Key] = r
	}
	inEnv := map[string]bool{}
	for _, r := range env.Resources {
		inEnv[r.Key] = true
	}
	for _, r := range base.Resources {
		if !inEnv[r.Key] {
			ops = append(ops, lock.PatchOp{Op: "remove", Key: r.Key})
		}
	}
	for _, r := range env.Resources {
		b, ok := baseByKey[r.Key]
		if !ok || b.Type != r.Type {
			if ok {
				panic(fmt.Sprintf("diffStates: %s changes type", r.Key))
			}
			ops = append(ops, lock.PatchOp{Op: "add", Key: r.Key, Type: r.Type, Config: r.Config, DependsOn: r.DependsOn})
			continue
		}
		start := len(ops)
		ops = diffMap(ops, r.Key, "", b.Config, r.Config)
		// A required name is always set by the patch, even to the value of
		// the base: that is what tells the engine it can apply the patch
		// again to a new base (ADR-030).
		if attr, ok := requiredNameTypes[r.Type]; ok && !slices.ContainsFunc(ops[start:], func(op lock.PatchOp) bool { return op.Op == "set" && op.Path == attr }) {
			ops = append(ops, lock.PatchOp{Op: "set", Key: r.Key, Path: attr, Value: r.Config[attr]})
		}
		if !slices.Equal(b.DependsOn, r.DependsOn) {
			deps := []any{}
			for _, d := range r.DependsOn {
				deps = append(deps, d)
			}
			ops = append(ops, lock.PatchOp{Op: "depends_on", Key: r.Key, Value: deps})
		}
	}
	if !sameJSON(base.Checks, env.Checks) {
		var checks any
		if err := roundTrip(env.Checks, &checks); err != nil {
			panic(err)
		}
		ops = append(ops, lock.PatchOp{Op: "checks", Value: checks})
	}
	return ops
}

// requiredNameTypes are the resource types of the states whose schema requires
// a name, and the attribute that holds it (engine requiredNameAttrs).
var requiredNameTypes = map[string]string{
	"aws_ecs_cluster":         "name",
	"aws_ecs_service":         "name",
	"aws_ecs_task_definition": "family",
}

// diffMap appends to ops the operations that turn the map old into new at
// the dotted prefix, in the order of the keys.
func diffMap(ops []lock.PatchOp, key, prefix string, old, new map[string]any) []lock.PatchOp {
	keys := map[string]bool{}
	for k := range old {
		keys[k] = true
	}
	for k := range new {
		keys[k] = true
	}
	for _, k := range sortedKeys(keys) {
		path := prefix + k
		ov, inOld := old[k]
		nv, inNew := new[k]
		switch {
		case !inNew:
			ops = append(ops, lock.PatchOp{Op: "unset", Key: key, Path: path})
		case !inOld:
			ops = append(ops, lock.PatchOp{Op: "set", Key: key, Path: path, Value: nv})
		case sameJSON(ov, nv):
		default:
			om, oldIsMap := ov.(map[string]any)
			nm, newIsMap := nv.(map[string]any)
			if oldIsMap && newIsMap {
				ops = diffMap(ops, key, path+".", om, nm)
			} else {
				ops = append(ops, lock.PatchOp{Op: "set", Key: key, Path: path, Value: nv})
			}
		}
	}
	return ops
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func sameJSON(a, b any) bool {
	ja, errA := json.Marshal(a)
	jb, errB := json.Marshal(b)
	return errA == nil && errB == nil && string(ja) == string(jb)
}

// applyStatePatch applies ops to the base as the engine does (lock.ApplyPatch)
// and returns the result as a desired state.
func applyStatePatch(base desiredState, ops []lock.PatchOp) (desiredState, error) {
	var rs []lock.Resource
	for _, r := range base.Resources {
		rs = append(rs, lock.Resource{Key: r.Key, Type: r.Type, Chosen: r.Config, DependsOn: r.DependsOn})
	}
	got, checks, err := lock.ApplyPatch(rs, base.Checks, ops)
	if err != nil {
		return desiredState{}, err
	}
	out := desiredState{Checks: checks}
	for _, r := range got {
		out.Resources = append(out.Resources, stateResource{Key: r.Key, Type: r.Type, Config: r.Chosen, DependsOn: r.DependsOn})
	}
	return out, nil
}

// canonicalState is a desired state in a form that compares: resources by
// key, as JSON.
func canonicalState(ds desiredState) string {
	rs := slices.Clone(ds.Resources)
	slices.SortFunc(rs, func(a, b stateResource) int { return strings.Compare(a.Key, b.Key) })
	for i := range rs {
		if len(rs[i].DependsOn) == 0 {
			rs[i].DependsOn = nil
		}
	}
	data, err := json.Marshal(struct {
		Resources []stateResource
		Checks    []lock.Check
	}{rs, ds.Checks})
	if err != nil {
		panic(err)
	}
	return string(data)
}

// The base holds nothing that belongs to an environment, and each
// environment's patch, applied to the base, gives that environment's state.
func TestEnvironmentStatesDeriveFromTheBase(t *testing.T) {
	for _, name := range []string{"database", "app", "cron"} {
		base := loadStates(t, "base", name)
		if data, _ := json.Marshal(base); strings.Contains(string(data), "staging") || strings.Contains(string(data), "prod") {
			t.Errorf("the base state of %s names an environment: %s", name, data)
		}
		for _, env := range []string{"staging", "prod"} {
			want := loadStates(t, env, name)
			ops := diffStates(base, want)
			got, err := applyStatePatch(base, ops)
			if err != nil {
				t.Errorf("%s/%s: the patch does not apply: %v", env, name, err)
				continue
			}
			if canonicalState(got) != canonicalState(want) {
				t.Errorf("%s/%s: base plus patch is not the state of the environment", env, name)
			}
			if len(ops) == 0 {
				t.Errorf("%s/%s: empty patch", env, name)
			}
		}
	}
}

// A patch of an environment sets every required name of the base's
// resources: the ECS cluster, the task family and the service (ADR-030).
func TestEnvironmentPatchesSetTheRequiredNames(t *testing.T) {
	for _, env := range []string{"staging", "prod"} {
		for _, name := range []string{"database", "app", "cron"} {
			base := loadStates(t, "base", name)
			ops := diffStates(base, loadStates(t, env, name))
			for _, r := range base.Resources {
				attr, ok := requiredNameTypes[r.Type]
				if !ok {
					continue
				}
				if !slices.ContainsFunc(ops, func(op lock.PatchOp) bool { return op.Op == "set" && op.Key == r.Key && op.Path == attr }) {
					t.Errorf("%s/%s: the patch does not set %s.%s", env, name, r.Key, attr)
				}
			}
		}
	}
}
