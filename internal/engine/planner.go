package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/giammbo/prosa/internal/intent"
	"github.com/giammbo/prosa/internal/lock"
)

// Planner turns an intent into its desired state (ADR-015): the resources it
// wants and their configuration. It decides; it never executes.
type Planner interface {
	Plan(ctx context.Context, in PlanInput) (Desired, error)
}

// Forgetter is implemented by planners that keep their answers (the
// planner cache). Plan calls Forget when the provider rejected, at plan
// time, the desired state the planner gave for an intent in this run, so
// that the next run plans it again instead of failing the same way.
type Forgetter interface {
	Forget(intentName string)
}

// PlanInput is what the planner sees for one intent. It never holds a
// secret value: the lock's chosen configurations have none (ADR-016).
type PlanInput struct {
	Intent intent.Intent
	// Mode is the work asked (ADR-030): "plan", the complete desired state of
	// an intent planned for the first time, or "patch", the changes to the
	// state the planner is shown (Previous, or Base in an environment). In
	// both modes a complete desired state is accepted.
	Mode string
	// PreviousBody is the intent's text when it last completed; "" for a
	// new intent.
	PreviousBody string
	// Env is the environment file the project is reconciled for (ADR-029);
	// nil without --env. PreviousEnvBody is its text when the intent last
	// completed ("" when new, or completed without an environment).
	Env             *intent.Env
	PreviousEnvBody string
	// Previous holds the resources the lock recorded for the intent; nil for
	// a new intent.
	Previous []lock.Resource
	// Available lists the resources of the other intents that the intent can
	// refer to, sorted by intent and key.
	Available []AvailableResource
	// Referenced lists, sorted, the keys of this intent that other intents
	// refer to or have in a depends_on: they must stay, with the same type.
	Referenced []string
	// Base and BaseChecks are the base this intent derives from in an
	// environment (ADR-030): the resources and checks of the base lock, which
	// a patch is applied to. nil without an environment derived from a base.
	Base       []lock.Resource
	BaseChecks []lock.Check
	// OldPatch is the patch the environment's lock recorded for the intent,
	// on its previous base; Problems says why it no longer applies to Base
	// (an operation that fails, a validation problem), nil when it was not
	// tried.
	OldPatch []lock.PatchOp
	Problems []string
	// RequiredNames lists, sorted, the attributes "<key>.<attribute>" of the
	// base whose schema requires a name (name, family, function_name): the
	// environment's patch sets them (ADR-030).
	RequiredNames []string
	// Region is the AWS region the provider is configured for.
	Region string
	// Schemas looks up the provider's resource types.
	Schemas SchemaCatalog
	// Validate checks a candidate desired state with the rules the plan
	// applies, plus the Referenced keys and the types of the Previous keys:
	// nil or a *ValidationError. It changes nothing and can be called any
	// number of times.
	Validate func(ctx context.Context, d Desired) error
	// Canonical returns d in the one textual form the lock keeps: a lone
	// object given for a list or set block of at most one item becomes a
	// one-element list. Validate and the plan apply it themselves; a
	// planner calls it to return, and cache, the form the lock will keep.
	// d is not modified.
	Canonical func(d Desired) Desired

	// patchBase and patchChecks are what a Desired that is only a Patch
	// applies to: Base and BaseChecks in an environment derived from a base,
	// otherwise Previous and the checks of the lock (see resolve).
	patchBase   []lock.Resource
	patchChecks []lock.Check
}

// AvailableResource is a resource of another intent that can be referred to
// as "${<Intent>/<Key>.<attribute>}".
type AvailableResource struct {
	Intent, Key, Type string
}

// SchemaCatalog gives the planner the provider's resource schemas.
type SchemaCatalog interface {
	// Search returns at most limit resource types matching the query, best
	// first.
	Search(query string, limit int) []string
	// Describe returns the schema of a resource type as compact text: its
	// attributes with type, flags (required, optional, computed, sensitive,
	// write_only) and description, then its nested blocks.
	Describe(typeName string) (string, error)
}

// Desired is the desired state of one intent. Non-empty Blockers mean the
// intent cannot be planned as written (it contradicts itself or its checks):
// the plan is blocked with these reasons and Resources is ignored. Checks
// are the checks compiled from the intent's "Checks" section, one per
// sentence (ADR-021).
//
// Patch, with Resources nil, is the answer of a planner in patch mode: the
// operations on the state it was shown (Base, else Previous), which the
// engine applies before validating the result (an empty, non-nil Patch:
// nothing to change). With Resources set, Resources is the desired state and
// Patch only records how it derives from the base. Patch has no omitempty:
// in JSON (the planner cache, the Pending state) nil is null and an empty
// patch is [], so that an empty patch never reads back as an empty desired
// state.
type Desired struct {
	Resources []DesiredResource
	Blockers  []string
	Checks    []lock.Check
	Patch     []lock.PatchOp
}

// DesiredResource is one resource the intent wants. Config is the planner's
// configuration: JSON-like values where a reference to another resource is
// the whole-value string "${<intent>/<key>.<attribute>}", or a part of a
// longer string, a template (ADR-020). DependsOn lists the nodes
// ("<intent>/<key>") the resource comes after without referring to them.
type DesiredResource struct {
	Key       string         `json:"key"`
	Type      string         `json:"type"`
	Config    map[string]any `json:"config"`
	DependsOn []string       `json:"depends_on,omitempty"`
}

// StaticPlanner returns fixed desired states keyed by hash. It stands in for
// the LLM planner until M3. The lookup has two steps: the effective hash of
// the intent (IntentHash, which includes the environment when there is one),
// then the intent's own hash.
type StaticPlanner map[string]Desired

// Plan returns the fixture recorded for the effective hash of the intent
// (with the environment, as the lock keeps it), else for the intent's own.
func (s StaticPlanner) Plan(_ context.Context, in PlanInput) (Desired, error) {
	eff := IntentHash(in.Intent, in.Env)
	d, ok := s[eff]
	if !ok {
		d, ok = s[in.Intent.Hash]
	}
	if !ok {
		return Desired{}, fmt.Errorf("no fixture for intent %q (effective hash %s, intent hash %s)", in.Intent.Name, eff, in.Intent.Hash)
	}
	return d, nil
}

// fixture is one entry of a fixtures file.
type fixture struct {
	Intent    string            `json:"intent"`
	Hash      string            `json:"hash"`
	Resources []DesiredResource `json:"resources"`
	Blockers  []string          `json:"blockers"`
	Checks    []lock.Check      `json:"checks"`
	Patch     []lock.PatchOp    `json:"patch"`
}

// LoadFixtures reads a fixtures file (a JSON list of {intent, hash,
// resources, blockers, checks, patch}) into a StaticPlanner. Without "patch"
// the fixture is a complete state; with it (a list, empty included) and no
// resources, a patch on the state the planner is shown. Numbers stay
// json.Number, so they keep every digit; two fixtures for one hash are an
// error.
func LoadFixtures(path string) (StaticPlanner, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var fxs []fixture
	if err := dec.Decode(&fxs); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	sp := StaticPlanner{}
	for _, fx := range fxs {
		if _, dup := sp[fx.Hash]; dup {
			return nil, fmt.Errorf("%s: two fixtures for hash %s", path, fx.Hash)
		}
		sp[fx.Hash] = Desired{Resources: fx.Resources, Blockers: fx.Blockers, Checks: fx.Checks, Patch: fx.Patch}
	}
	return sp, nil
}
