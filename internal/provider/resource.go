package provider

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/zclconf/go-cty/cty"

	"github.com/giammbo/prosa/internal/tfplugin5"
)

// TransportError is a failure to reach the provider (a gRPC error), as
// opposed to the provider's own error diagnostics.
type TransportError struct {
	Op  string
	Err error
}

func (e *TransportError) Error() string { return e.Op + ": " + e.Err.Error() }
func (e *TransportError) Unwrap() error { return e.Err }

// terraformVersion is the Terraform version reported to providers in
// Configure; some providers gate features on it.
const terraformVersion = "1.11.0"

// clientCapabilities is what prosa tells providers it can handle: no deferred
// changes; write-only attributes are fine, since prosa never persists config
// values the schema marks write-only.
var clientCapabilities = &tfplugin5.ClientCapabilities{
	DeferralAllowed:            false,
	WriteOnlyAttributesAllowed: true,
}

// PlannedChange is the provider's plan for one resource.
type PlannedChange struct {
	Planned cty.Value
	// RequiresReplace lists the attribute paths whose change forces a
	// replacement, in the form name, tags["team"], setting[0].value.
	RequiresReplace []string
	PlannedPrivate  []byte
	// Identity is the planned resource identity; null when the resource
	// type has no identity schema or the provider returned none.
	Identity cty.Value
	Warnings []Diagnostic
}

// ApplyResult is the outcome of ApplyResourceChange.
type ApplyResult struct {
	// State is the new state; null after a destroy. When ApplyResource also
	// returns an error, State is what the provider reported anyway (a
	// partially created resource must still be recorded); it is cty.NilVal
	// only if no state could be decoded.
	State    cty.Value
	Private  []byte
	Identity cty.Value // null when the type has no identity schema
	Warnings []Diagnostic
}

// ReadResult is the outcome of ReadResource. A null State means the remote
// object no longer exists.
type ReadResult struct {
	State    cty.Value
	Private  []byte
	Identity cty.Value // null when the type has no identity schema
	Warnings []Diagnostic
}

// Configure validates the provider configuration (PrepareProviderConfig) and
// then configures the provider with it.
func (p *Provider) Configure(ctx context.Context, config cty.Value) error {
	s, err := p.Schema(ctx)
	if err != nil {
		return err
	}
	if s.Provider == nil {
		return fmt.Errorf("configure: provider has no configuration schema")
	}
	ty := s.Provider.ImpliedType()
	dv, err := Encode(config, ty)
	if err != nil {
		return fmt.Errorf("configure: %w", err)
	}
	prep, err := p.client.PrepareProviderConfig(ctx, &tfplugin5.PrepareProviderConfig_Request{Config: dv})
	if err != nil {
		return fmt.Errorf("prepare provider config: %w", err)
	}
	if err := diagsErr("prepare provider config", prep.GetDiagnostics()); err != nil {
		return err
	}
	if pc := prep.GetPreparedConfig(); pc != nil && (len(pc.Msgpack) > 0 || len(pc.Json) > 0) {
		prepared, err := Decode(pc, ty)
		if err != nil {
			return fmt.Errorf("prepare provider config: %w", err)
		}
		if dv, err = Encode(prepared, ty); err != nil {
			return fmt.Errorf("configure: %w", err)
		}
	}
	resp, err := p.client.Configure(ctx, &tfplugin5.Configure_Request{
		TerraformVersion:   terraformVersion,
		Config:             dv,
		ClientCapabilities: clientCapabilities,
	})
	if err != nil {
		return fmt.Errorf("configure: %w", err)
	}
	return diagsErr("configure", resp.GetDiagnostics())
}

// ValidateResource asks the provider to validate a resource configuration.
func (p *Provider) ValidateResource(ctx context.Context, typeName string, config cty.Value) error {
	op := "validate " + typeName
	b, err := p.resourceBlock(ctx, typeName)
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	dv, err := Encode(config, b.ImpliedType())
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	resp, err := p.client.ValidateResourceTypeConfig(ctx, &tfplugin5.ValidateResourceTypeConfig_Request{
		TypeName:           typeName,
		Config:             dv,
		ClientCapabilities: clientCapabilities,
	})
	if err != nil {
		return &TransportError{Op: op, Err: err}
	}
	return diagsErr(op, resp.GetDiagnostics())
}

// PlanResource asks the provider to plan the change from prior (null on
// create) to config (null on destroy), carrying the resource's private data
// and identity from the last apply or read. The proposed new state is
// computed with ProposedNew; the planned state is normalized with
// NormalizeBlocks, like the states of reads and applies.
func (p *Provider) PlanResource(ctx context.Context, typeName string, prior, config cty.Value, priorPrivate []byte, priorIdentity cty.Value) (PlannedChange, error) {
	op := "plan " + typeName
	b, is, err := p.resourceSchemas(ctx, typeName)
	if err != nil {
		return PlannedChange{}, fmt.Errorf("%s: %w", op, err)
	}
	ty := b.ImpliedType()
	prior = orNull(prior, ty)
	config = orNull(config, ty)
	priorDV, err := Encode(prior, ty)
	if err != nil {
		return PlannedChange{}, fmt.Errorf("%s: prior state: %w", op, err)
	}
	proposedDV, err := Encode(ProposedNew(b, prior, config), ty)
	if err != nil {
		return PlannedChange{}, fmt.Errorf("%s: proposed state: %w", op, err)
	}
	configDV, err := Encode(config, ty)
	if err != nil {
		return PlannedChange{}, fmt.Errorf("%s: config: %w", op, err)
	}
	priorID, err := encodeIdentity(is, priorIdentity)
	if err != nil {
		return PlannedChange{}, fmt.Errorf("%s: prior identity: %w", op, err)
	}
	resp, err := p.client.PlanResourceChange(ctx, &tfplugin5.PlanResourceChange_Request{
		TypeName:           typeName,
		PriorState:         priorDV,
		ProposedNewState:   proposedDV,
		Config:             configDV,
		PriorPrivate:       priorPrivate,
		ClientCapabilities: clientCapabilities,
		PriorIdentity:      priorID,
	})
	if err != nil {
		return PlannedChange{}, &TransportError{Op: op, Err: err}
	}
	warnings := diagsWarnings(resp.GetDiagnostics())
	if err := diagsErr(op, resp.GetDiagnostics()); err != nil {
		return PlannedChange{Warnings: warnings}, err
	}
	planned, err := Decode(resp.GetPlannedState(), ty)
	if err != nil {
		return PlannedChange{Warnings: warnings}, fmt.Errorf("%s: planned state: %w", op, err)
	}
	planned = NormalizeBlocks(b, planned)
	identity, err := decodeIdentity(is, resp.GetPlannedIdentity())
	if err != nil {
		return PlannedChange{Warnings: warnings}, fmt.Errorf("%s: planned identity: %w", op, err)
	}
	var replace []string
	for _, ap := range resp.GetRequiresReplace() {
		replace = append(replace, pathString(ap))
	}
	return PlannedChange{
		Planned:         planned,
		RequiresReplace: replace,
		PlannedPrivate:  resp.GetPlannedPrivate(),
		Identity:        identity,
		Warnings:        warnings,
	}, nil
}

// ApplyResource applies a planned change. On error the result is still
// filled with what the provider returned: State may be non-null (a resource
// created before the failure), and the caller must keep it.
func (p *Provider) ApplyResource(ctx context.Context, typeName string, prior cty.Value, change PlannedChange, config cty.Value) (ApplyResult, error) {
	op := "apply " + typeName
	b, is, err := p.resourceSchemas(ctx, typeName)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("%s: %w", op, err)
	}
	ty := b.ImpliedType()
	priorDV, err := Encode(orNull(prior, ty), ty)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("%s: prior state: %w", op, err)
	}
	plannedDV, err := Encode(orNull(change.Planned, ty), ty)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("%s: planned state: %w", op, err)
	}
	configDV, err := Encode(orNull(config, ty), ty)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("%s: config: %w", op, err)
	}
	plannedID, err := encodeIdentity(is, change.Identity)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("%s: planned identity: %w", op, err)
	}
	resp, err := p.client.ApplyResourceChange(ctx, &tfplugin5.ApplyResourceChange_Request{
		TypeName:        typeName,
		PriorState:      priorDV,
		PlannedState:    plannedDV,
		Config:          configDV,
		PlannedPrivate:  change.PlannedPrivate,
		PlannedIdentity: plannedID,
	})
	if err != nil {
		return ApplyResult{}, fmt.Errorf("%s: %w", op, err)
	}
	res := ApplyResult{Private: resp.GetPrivate(), Warnings: diagsWarnings(resp.GetDiagnostics())}
	errs := []error{diagsErr(op, resp.GetDiagnostics())}
	if res.State, err = Decode(resp.GetNewState(), ty); err != nil {
		errs = append(errs, fmt.Errorf("%s: new state: %w", op, err))
	}
	res.State = NormalizeBlocks(b, res.State)
	if res.Identity, err = decodeIdentity(is, resp.GetNewIdentity()); err != nil {
		errs = append(errs, fmt.Errorf("%s: new identity: %w", op, err))
	}
	return res, errors.Join(errs...)
}

// DeleteResource destroys the resource. When the provider declares the
// plan_destroy capability (hashicorp/aws does) the destroy is planned first
// with PlanResource and a null config, so the provider computes its own
// destroy plan; the plan is then applied with ApplyResource. A provider
// without the capability gets ApplyResourceChange with a null planned state
// directly, as Terraform does.
func (p *Provider) DeleteResource(ctx context.Context, typeName string, prior cty.Value, private []byte, identity cty.Value) error {
	op := "delete " + typeName
	s, err := p.Schema(ctx)
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	b, _, err := p.resourceSchemas(ctx, typeName)
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	null := cty.NullVal(b.ImpliedType())
	change := PlannedChange{Planned: null, PlannedPrivate: private, Identity: identity}
	if s.PlanDestroy {
		if change, err = p.PlanResource(ctx, typeName, prior, null, private, identity); err != nil {
			return fmt.Errorf("%s: %w", op, err)
		}
		if !change.Planned.IsNull() {
			return fmt.Errorf("%s: provider planned a non-null state for a destroy", op)
		}
	}
	res, err := p.ApplyResource(ctx, typeName, prior, change, null)
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	if !res.State.IsNull() {
		return fmt.Errorf("%s: provider returned a non-null state", op)
	}
	return nil
}

// ReadResource refreshes current from the real world, carrying the private
// data and identity from the last apply or read. A null State in the result
// means the resource no longer exists.
func (p *Provider) ReadResource(ctx context.Context, typeName string, current cty.Value, private []byte, identity cty.Value) (ReadResult, error) {
	op := "read " + typeName
	b, is, err := p.resourceSchemas(ctx, typeName)
	if err != nil {
		return ReadResult{}, fmt.Errorf("%s: %w", op, err)
	}
	ty := b.ImpliedType()
	dv, err := Encode(orNull(current, ty), ty)
	if err != nil {
		return ReadResult{}, fmt.Errorf("%s: current state: %w", op, err)
	}
	currentID, err := encodeIdentity(is, identity)
	if err != nil {
		return ReadResult{}, fmt.Errorf("%s: current identity: %w", op, err)
	}
	resp, err := p.client.ReadResource(ctx, &tfplugin5.ReadResource_Request{
		TypeName:           typeName,
		CurrentState:       dv,
		Private:            private,
		ClientCapabilities: clientCapabilities,
		CurrentIdentity:    currentID,
	})
	if err != nil {
		return ReadResult{}, fmt.Errorf("%s: %w", op, err)
	}
	warnings := diagsWarnings(resp.GetDiagnostics())
	if err := diagsErr(op, resp.GetDiagnostics()); err != nil {
		return ReadResult{Warnings: warnings}, err
	}
	newState, err := Decode(resp.GetNewState(), ty)
	if err != nil {
		return ReadResult{Warnings: warnings}, fmt.Errorf("%s: new state: %w", op, err)
	}
	newState = NormalizeBlocks(b, newState)
	newID, err := decodeIdentity(is, resp.GetNewIdentity())
	if err != nil {
		return ReadResult{Warnings: warnings}, fmt.Errorf("%s: new identity: %w", op, err)
	}
	return ReadResult{State: newState, Private: resp.GetPrivate(), Identity: newID, Warnings: warnings}, nil
}

func (p *Provider) resourceBlock(ctx context.Context, typeName string) (*Block, error) {
	b, _, err := p.resourceSchemas(ctx, typeName)
	return b, err
}

// resourceSchemas returns the block and the identity schema (nil when the
// type has none) of a resource type.
func (p *Provider) resourceSchemas(ctx context.Context, typeName string) (*Block, *IdentitySchema, error) {
	s, err := p.Schema(ctx)
	if err != nil {
		return nil, nil, err
	}
	rs, ok := s.Resources[typeName]
	if !ok {
		return nil, nil, fmt.Errorf("unknown resource type %q", typeName)
	}
	return rs.Block, s.Identities[typeName], nil
}

// encodeIdentity encodes an identity value for the wire; nil when the type
// has no identity schema or v is null.
func encodeIdentity(is *IdentitySchema, v cty.Value) (*tfplugin5.ResourceIdentityData, error) {
	if is == nil || v == cty.NilVal || v.IsNull() {
		return nil, nil
	}
	dv, err := Encode(v, is.Type)
	if err != nil {
		return nil, err
	}
	return &tfplugin5.ResourceIdentityData{IdentityData: dv}, nil
}

// decodeIdentity decodes an identity returned by the provider. It is a
// dynamic null when the type has no identity schema, a typed null when the
// provider returned none.
func decodeIdentity(is *IdentitySchema, d *tfplugin5.ResourceIdentityData) (cty.Value, error) {
	if is == nil {
		return cty.NullVal(cty.DynamicPseudoType), nil
	}
	return Decode(d.GetIdentityData(), is.Type)
}

// orNull turns the zero cty.Value into a typed null.
func orNull(v cty.Value, ty cty.Type) cty.Value {
	if v == cty.NilVal {
		return cty.NullVal(ty)
	}
	return v
}

// pathString renders an attribute path as name, tags["team"], setting[0].value.
func pathString(ap *tfplugin5.AttributePath) string {
	var sb strings.Builder
	for _, st := range ap.GetSteps() {
		switch sel := st.GetSelector().(type) {
		case *tfplugin5.AttributePath_Step_AttributeName:
			sb.WriteString("." + sel.AttributeName)
		case *tfplugin5.AttributePath_Step_ElementKeyString:
			sb.WriteString("[" + strconv.Quote(sel.ElementKeyString) + "]")
		case *tfplugin5.AttributePath_Step_ElementKeyInt:
			sb.WriteString("[" + strconv.FormatInt(sel.ElementKeyInt, 10) + "]")
		}
	}
	return strings.TrimPrefix(sb.String(), ".")
}
