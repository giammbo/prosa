package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/zclconf/go-cty/cty"

	"github.com/giammbo/prosa/internal/provider"
)

// fakeProvider is an in-memory Provider with two resource types:
//
//	test_net: name, tags, id (computed)
//	test_db:  name, net_id, size (number), password (sensitive), tags, id (computed)
//	test_secret: name, value_wo (write-only), id (computed)
//	test_clock: name, read_at (computed), id (computed)
//	test_list:  name, items (list of string), groups (map of lists), id (computed)
//	test_token: name, value (computed, sensitive), id (computed)
//	test_app:   name, auth block list (client_id, client_secret sensitive), id (computed)
//	test_lb:    name, health block list max 1 (path), rule block list (path),
//	            opts single block with inner block set max 1 (flag), id (computed)
//	test_regional: name, region (optional, computed), id (computed)
//
// PlanResource returns the config with the computed attributes taken from the
// prior state (unknown on create); ApplyResource assigns deterministic ids
// ("<type>-<n>"), sets any other unknown string to "<id>-<attribute>" and
// keeps the objects in remote, which ReadResource reads. ValidateResource
// rejects a size above 100.
type fakeProvider struct {
	mu     sync.Mutex
	schema *provider.Schema
	// remote is the "cloud": id → current state. Tests edit it to simulate
	// out-of-band changes.
	remote map[string]cty.Value
	nextID map[string]int
	// calls records every RPC as "<method> <type>".
	calls []string
	// replaceOn lists, per type, the attributes whose change forces a
	// replacement.
	replaceOn map[string][]string
	// unknownOnUpdate lists, per type, the computed attributes the provider
	// recomputes (plans as unknown) on an in-place update.
	unknownOnUpdate map[string][]string
	// applyErr, when set, is called before each apply; a non-nil error
	// makes the apply fail without touching remote.
	applyErr func(typeName string, planned cty.Value) error
	// partialErr, when set and returning an error, makes the apply fail
	// after the object was created: the state is returned with the error.
	partialErr func(typeName string) error
	// leakWriteOnly makes the plan (and so the apply) keep write-only
	// values, which a correct provider never returns.
	leakWriteOnly bool
	// version is what Version reports.
	version string
	// tickOnRead lists, per type, a computed attribute that changes on
	// every read (like a last-seen timestamp).
	tickOnRead map[string]string
	reads      int
	// ignoreChanges lists, per type, the attributes whose config changes
	// the plan ignores (planned keeps the prior value).
	ignoreChanges map[string][]string
	// unknownOnApply lists, per type, an attribute the apply leaves
	// unknown, which a correct provider never does.
	unknownOnApply map[string]string
	// validateErr, when set, is returned by ValidateResource for a type
	// (a transport failure, not a diagnostic).
	validateErr func(typeName string) error
	// planErr, when set, is returned by PlanResource for a create (a prior
	// state that is null), like a CustomizeDiff that rejects the config.
	planErr func(typeName string, config cty.Value) error
	// updateErr, when set, is returned by PlanResource for a resource that
	// exists (a prior state that is not null) and is not being destroyed.
	updateErr func(typeName string, config cty.Value) error
	// onPlan and onApply, when set, are called with the context of each
	// PlanResource / ApplyResource call, before it runs. A call whose
	// context is cancelled fails with errRPCCanceled, as a gRPC call does,
	// without touching remote.
	onPlan, onApply func(ctx context.Context, typeName string)
	// planIgnoresCancel makes PlanResource succeed with a cancelled context,
	// like a plan that completes as the signal arrives.
	planIgnoresCancel bool
}

// errRPCCanceled is the error of a fake call made with a cancelled context:
// like gRPC's, it does not wrap context.Canceled.
var errRPCCanceled = errors.New("rpc error: code = Canceled desc = context canceled")

func newFakeProvider() *fakeProvider {
	str := func(opt bool) *provider.Attribute { return &provider.Attribute{Type: cty.String, Optional: opt} }
	tags := &provider.Attribute{Type: cty.Map(cty.String), Optional: true}
	id := &provider.Attribute{Type: cty.String, Computed: true}
	return &fakeProvider{
		schema: &provider.Schema{
			Resources: map[string]*provider.ResourceSchema{
				"test_net": {Block: &provider.Block{
					Attributes: map[string]*provider.Attribute{"name": str(true), "tags": tags, "id": id},
					BlockTypes: map[string]*provider.NestedBlock{},
				}},
				"test_db": {Block: &provider.Block{
					Attributes: map[string]*provider.Attribute{
						"name":     str(true),
						"net_id":   str(true),
						"size":     {Type: cty.Number, Optional: true, Description: "Allocated storage in GiB."},
						"password": {Type: cty.String, Optional: true, Sensitive: true},
						"tags":     tags,
						"id":       id,
					},
					BlockTypes: map[string]*provider.NestedBlock{},
				}},
				"test_clock": {Block: &provider.Block{
					Attributes: map[string]*provider.Attribute{
						"name":    str(true),
						"read_at": {Type: cty.String, Computed: true},
						"id":      id,
					},
					BlockTypes: map[string]*provider.NestedBlock{},
				}},
				"test_list": {Block: &provider.Block{
					Attributes: map[string]*provider.Attribute{
						"name":   str(true),
						"items":  {Type: cty.List(cty.String), Optional: true},
						"groups": {Type: cty.Map(cty.List(cty.String)), Optional: true},
						"id":     id,
					},
					BlockTypes: map[string]*provider.NestedBlock{},
				}},
				"test_token": {Block: &provider.Block{
					Attributes: map[string]*provider.Attribute{
						"name":  str(true),
						"value": {Type: cty.String, Computed: true, Sensitive: true},
						"id":    id,
					},
					BlockTypes: map[string]*provider.NestedBlock{},
				}},
				"test_app": {Block: &provider.Block{
					Attributes: map[string]*provider.Attribute{"name": str(true), "id": id},
					BlockTypes: map[string]*provider.NestedBlock{
						"auth": {Nesting: provider.NestingList, Block: &provider.Block{
							Attributes: map[string]*provider.Attribute{
								"client_id":     str(true),
								"client_secret": {Type: cty.String, Optional: true, Sensitive: true},
							},
							BlockTypes: map[string]*provider.NestedBlock{},
						}},
					},
				}},
				"test_lb": {Block: &provider.Block{
					Attributes: map[string]*provider.Attribute{"name": str(true), "id": id},
					BlockTypes: map[string]*provider.NestedBlock{
						"health": {Nesting: provider.NestingList, MaxItems: 1, Block: &provider.Block{
							Attributes: map[string]*provider.Attribute{"path": str(true)},
							BlockTypes: map[string]*provider.NestedBlock{},
						}},
						"rule": {Nesting: provider.NestingList, Block: &provider.Block{
							Attributes: map[string]*provider.Attribute{"path": str(true)},
							BlockTypes: map[string]*provider.NestedBlock{},
						}},
						"opts": {Nesting: provider.NestingSingle, Block: &provider.Block{
							Attributes: map[string]*provider.Attribute{},
							BlockTypes: map[string]*provider.NestedBlock{
								"inner": {Nesting: provider.NestingSet, MaxItems: 1, Block: &provider.Block{
									Attributes: map[string]*provider.Attribute{"flag": {Type: cty.Bool, Optional: true}},
									BlockTypes: map[string]*provider.NestedBlock{},
								}},
							},
						}},
					},
				}},
				"test_regional": {Block: &provider.Block{
					Attributes: map[string]*provider.Attribute{
						"name":   str(true),
						"region": {Type: cty.String, Optional: true, Computed: true},
						"id":     id,
					},
					BlockTypes: map[string]*provider.NestedBlock{},
				}},
				"test_secret": {Version: 2, Block: &provider.Block{
					Attributes: map[string]*provider.Attribute{
						"name":     str(true),
						"value_wo": {Type: cty.String, Optional: true, WriteOnly: true},
						"id":       id,
					},
					BlockTypes: map[string]*provider.NestedBlock{},
				}},
			},
			Identities:  map[string]*provider.IdentitySchema{},
			PlanDestroy: true,
		},
		remote:    map[string]cty.Value{},
		nextID:    map[string]int{},
		replaceOn: map[string][]string{},

		unknownOnUpdate: map[string][]string{},
		version:         "1.0.0",
		tickOnRead:      map[string]string{},
		ignoreChanges:   map[string][]string{},
		unknownOnApply:  map[string]string{},
	}
}

var _ Provider = (*fakeProvider)(nil)

func (f *fakeProvider) Version() string { return f.version }

func (f *fakeProvider) record(method, typeName string) {
	f.calls = append(f.calls, method+" "+typeName)
}

func (f *fakeProvider) block(typeName string) (*provider.Block, error) {
	rs, ok := f.schema.Resources[typeName]
	if !ok {
		return nil, fmt.Errorf("unknown resource type %q", typeName)
	}
	return rs.Block, nil
}

func (f *fakeProvider) Schema(context.Context) (*provider.Schema, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "schema")
	return f.schema, nil
}

func (f *fakeProvider) ValidateResource(_ context.Context, typeName string, config cty.Value) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("validate", typeName)
	if f.validateErr != nil {
		if err := f.validateErr(typeName); err != nil {
			return err
		}
	}
	if _, err := f.block(typeName); err != nil {
		return err
	}
	if !config.Type().HasAttribute("size") {
		return nil
	}
	size := config.GetAttr("size")
	if size.IsKnown() && !size.IsNull() && size.GreaterThan(cty.NumberIntVal(100)).True() {
		return fmt.Errorf("validate %s: size must be at most 100", typeName)
	}
	return nil
}

func (f *fakeProvider) PlanResource(ctx context.Context, typeName string, prior, config cty.Value, priorPrivate []byte, _ cty.Value) (provider.PlannedChange, error) {
	if f.onPlan != nil {
		f.onPlan(ctx, typeName)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("plan", typeName)
	if ctx.Err() != nil && !f.planIgnoresCancel {
		return provider.PlannedChange{}, errRPCCanceled
	}
	b, err := f.block(typeName)
	if err != nil {
		return provider.PlannedChange{}, err
	}
	if f.planErr != nil && (prior == cty.NilVal || prior.IsNull()) && config != cty.NilVal && !config.IsNull() {
		if err := f.planErr(typeName, config); err != nil {
			return provider.PlannedChange{}, err
		}
	}
	if f.updateErr != nil && prior != cty.NilVal && !prior.IsNull() && config != cty.NilVal && !config.IsNull() {
		if err := f.updateErr(typeName, config); err != nil {
			return provider.PlannedChange{}, err
		}
	}
	ty := b.ImpliedType()
	// The engine must pass a prior typed with the schema (a null object of
	// the resource's type), never NilVal or a dynamic null.
	if prior == cty.NilVal || !prior.Type().Equals(ty) {
		return provider.PlannedChange{}, fmt.Errorf("plan %s: prior state has type %#v, want the schema type", typeName, prior)
	}
	if config == cty.NilVal || config.IsNull() {
		return provider.PlannedChange{Planned: cty.NullVal(ty), PlannedPrivate: priorPrivate}, nil
	}
	if !prior.IsNull() && len(f.ignoreChanges[typeName]) > 0 {
		vals := config.AsValueMap()
		for _, a := range f.ignoreChanges[typeName] {
			vals[a] = prior.GetAttr(a)
		}
		config = cty.ObjectVal(vals)
	}
	var replace []string
	if !prior.IsNull() {
		for _, a := range f.replaceOn[typeName] {
			if eq := prior.GetAttr(a).Equals(config.GetAttr(a)); !eq.IsKnown() || eq.False() {
				replace = append(replace, a)
			}
		}
	}
	vals := map[string]cty.Value{}
	for name, a := range b.Attributes {
		switch {
		case a.Computed && !a.Optional && (prior.IsNull() || len(replace) > 0):
			vals[name] = cty.UnknownVal(a.Type)
		case a.Computed && !a.Optional:
			vals[name] = prior.GetAttr(name)
		case a.WriteOnly && !f.leakWriteOnly:
			vals[name] = cty.NullVal(a.Type)
		default:
			vals[name] = config.GetAttr(name)
		}
	}
	for name := range b.BlockTypes {
		vals[name] = config.GetAttr(name)
	}
	if !prior.IsNull() && len(replace) == 0 && !sameValue(cty.ObjectVal(vals), prior) {
		for _, a := range f.unknownOnUpdate[typeName] {
			vals[a] = cty.UnknownVal(b.Attributes[a].Type)
		}
	}
	return provider.PlannedChange{Planned: cty.ObjectVal(vals), RequiresReplace: replace, PlannedPrivate: priorPrivate}, nil
}

func (f *fakeProvider) ApplyResource(ctx context.Context, typeName string, prior cty.Value, change provider.PlannedChange, _ cty.Value) (provider.ApplyResult, error) {
	if f.onApply != nil {
		f.onApply(ctx, typeName)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("apply", typeName)
	if ctx.Err() != nil {
		return provider.ApplyResult{}, errRPCCanceled
	}
	b, err := f.block(typeName)
	if err != nil {
		return provider.ApplyResult{}, err
	}
	ty := b.ImpliedType()
	if f.applyErr != nil {
		if err := f.applyErr(typeName, change.Planned); err != nil {
			return provider.ApplyResult{State: cty.NullVal(ty)}, err
		}
	}
	if change.Planned == cty.NilVal || change.Planned.IsNull() {
		if prior != cty.NilVal && !prior.IsNull() {
			delete(f.remote, prior.GetAttr("id").AsString())
		}
		return provider.ApplyResult{State: cty.NullVal(ty), Identity: cty.NullVal(cty.DynamicPseudoType)}, nil
	}
	vals := change.Planned.AsValueMap()
	if !vals["id"].IsKnown() {
		f.nextID[typeName]++
		vals["id"] = cty.StringVal(fmt.Sprintf("%s-%d", typeName, f.nextID[typeName]))
	}
	for name, v := range vals {
		if !v.IsKnown() && v.Type() == cty.String && name != f.unknownOnApply[typeName] {
			vals[name] = cty.StringVal(vals["id"].AsString() + "-" + name)
		}
	}
	st := cty.ObjectVal(vals)
	f.remote[vals["id"].AsString()] = st
	res := provider.ApplyResult{State: st, Private: change.PlannedPrivate, Identity: cty.NullVal(cty.DynamicPseudoType)}
	if f.partialErr != nil {
		if err := f.partialErr(typeName); err != nil {
			return res, err
		}
	}
	return res, nil
}

func (f *fakeProvider) ReadResource(_ context.Context, typeName string, current cty.Value, private []byte, _ cty.Value) (provider.ReadResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("read", typeName)
	b, err := f.block(typeName)
	if err != nil {
		return provider.ReadResult{}, err
	}
	none := cty.NullVal(cty.DynamicPseudoType)
	if current == cty.NilVal || current.IsNull() {
		return provider.ReadResult{State: cty.NullVal(b.ImpliedType()), Identity: none}, nil
	}
	st, ok := f.remote[current.GetAttr("id").AsString()]
	if !ok {
		return provider.ReadResult{State: cty.NullVal(b.ImpliedType()), Identity: none}, nil
	}
	if attr := f.tickOnRead[typeName]; attr != "" {
		f.reads++
		vals := st.AsValueMap()
		vals[attr] = cty.StringVal(fmt.Sprintf("t%d", f.reads))
		st = cty.ObjectVal(vals)
		f.remote[current.GetAttr("id").AsString()] = st
	}
	return provider.ReadResult{State: st, Private: private, Identity: none}, nil
}
