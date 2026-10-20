package provider

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/zclconf/go-cty/cty"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/giammbo/prosa/internal/tfplugin5"
)

func TestPathString(t *testing.T) {
	attr := func(n string) *tfplugin5.AttributePath_Step {
		return &tfplugin5.AttributePath_Step{Selector: &tfplugin5.AttributePath_Step_AttributeName{AttributeName: n}}
	}
	key := func(k string) *tfplugin5.AttributePath_Step {
		return &tfplugin5.AttributePath_Step{Selector: &tfplugin5.AttributePath_Step_ElementKeyString{ElementKeyString: k}}
	}
	idx := func(i int64) *tfplugin5.AttributePath_Step {
		return &tfplugin5.AttributePath_Step{Selector: &tfplugin5.AttributePath_Step_ElementKeyInt{ElementKeyInt: i}}
	}
	cases := []struct {
		steps []*tfplugin5.AttributePath_Step
		want  string
	}{
		{[]*tfplugin5.AttributePath_Step{attr("name")}, "name"},
		{[]*tfplugin5.AttributePath_Step{attr("tags"), key("team")}, `tags["team"]`},
		{[]*tfplugin5.AttributePath_Step{attr("setting"), idx(0), attr("value")}, "setting[0].value"},
	}
	for _, c := range cases {
		if got := pathString(&tfplugin5.AttributePath{Steps: c.steps}); got != c.want {
			t.Errorf("pathString = %q, want %q", got, c.want)
		}
	}
}

func newFakeProvider(f *fakeClient) *Provider { return &Provider{client: f} }

func TestSchemaIdentities(t *testing.T) {
	p := newFakeProvider(&fakeClient{identities: fakeIdentitySchemas()})
	s, err := p.Schema(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	is := s.Identities[fakeType]
	if is == nil || is.Version != 2 || !is.Type.Equals(fakeIdentityType) {
		t.Fatalf("identity schema = %+v", is)
	}
	if s.PlanDestroy {
		t.Error("PlanDestroy should be false: the fake declares no plan_destroy")
	}
}

func TestSchemaToleratesUnimplementedIdentitySchemas(t *testing.T) {
	f := &fakeClient{identityErr: status.Error(codes.Unimplemented, "nope")}
	p := newFakeProvider(f)
	s, err := p.Schema(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Identities) != 0 {
		t.Fatalf("identities = %v, want none", s.Identities)
	}
	f.plan = func(req *tfplugin5.PlanResourceChange_Request) *tfplugin5.PlanResourceChange_Response {
		if req.PriorIdentity != nil {
			t.Errorf("prior identity sent for a type without identity schema: %v", req.PriorIdentity)
		}
		return &tfplugin5.PlanResourceChange_Response{PlannedState: req.ProposedNewState}
	}
	change, err := p.PlanResource(context.Background(), fakeType, cty.NilVal, fakeConfig(t, p, "x"), nil, cty.NilVal)
	if err != nil {
		t.Fatal(err)
	}
	if !change.Identity.IsNull() {
		t.Errorf("identity = %#v, want null", change.Identity)
	}

	other := newFakeProvider(&fakeClient{identityErr: status.Error(codes.Internal, "boom")})
	if _, err := other.Schema(context.Background()); err == nil {
		t.Error("a non-Unimplemented identity schema error must be returned")
	}
}

func TestApplyResourceReturnsPartialStateOnError(t *testing.T) {
	f := &fakeClient{identities: fakeIdentitySchemas()}
	p := newFakeProvider(f)
	partial := fakeState(t, p, "x", "abc")
	f.apply = func(req *tfplugin5.ApplyResourceChange_Request) *tfplugin5.ApplyResourceChange_Response {
		return &tfplugin5.ApplyResourceChange_Response{
			NewState:    mustEncode(t, partial),
			Private:     []byte("p"),
			NewIdentity: identityData(t, fakeIdentity("abc")),
			Diagnostics: []*tfplugin5.Diagnostic{
				{Severity: tfplugin5.Diagnostic_WARNING, Summary: "careful", Detail: "w",
					Attribute: &tfplugin5.AttributePath{Steps: []*tfplugin5.AttributePath_Step{
						{Selector: &tfplugin5.AttributePath_Step_AttributeName{AttributeName: "name"}},
					}}},
				{Severity: tfplugin5.Diagnostic_ERROR, Summary: "waiting for cluster", Detail: "timeout"},
			},
		}
	}
	cfg := fakeConfig(t, p, "x")
	res, err := p.ApplyResource(context.Background(), fakeType, cty.NilVal, PlannedChange{Planned: cfg}, cfg)
	if err == nil || !strings.Contains(err.Error(), "waiting for cluster: timeout") {
		t.Fatalf("err = %v", err)
	}
	if !res.State.RawEquals(partial) {
		t.Errorf("state = %#v, want the partial state %#v", res.State, partial)
	}
	if string(res.Private) != "p" {
		t.Errorf("private = %q", res.Private)
	}
	if !res.Identity.RawEquals(fakeIdentity("abc")) {
		t.Errorf("identity = %#v", res.Identity)
	}
	want := []Diagnostic{{Severity: "warning", Summary: "careful", Detail: "w", Attribute: "name"}}
	if !reflect.DeepEqual(res.Warnings, want) {
		t.Errorf("warnings = %+v, want %+v", res.Warnings, want)
	}
}

func TestPlanApplyReadCarryIdentity(t *testing.T) {
	f := &fakeClient{identities: fakeIdentitySchemas()}
	p := newFakeProvider(f)
	ctx := context.Background()
	prior := fakeState(t, p, "x", "abc")
	cfg := fakeConfig(t, p, "y")
	ty := prior.Type()

	f.plan = func(req *tfplugin5.PlanResourceChange_Request) *tfplugin5.PlanResourceChange_Response {
		if got := mustDecode(t, req.PriorIdentity.GetIdentityData(), fakeIdentityType); !got.RawEquals(fakeIdentity("abc")) {
			t.Errorf("plan: prior identity = %#v", got)
		}
		return &tfplugin5.PlanResourceChange_Response{
			PlannedState:    req.ProposedNewState,
			PlannedIdentity: identityData(t, fakeIdentity("abc")),
			Diagnostics:     []*tfplugin5.Diagnostic{{Severity: tfplugin5.Diagnostic_WARNING, Summary: "deprecated"}},
		}
	}
	change, err := p.PlanResource(ctx, fakeType, prior, cfg, nil, fakeIdentity("abc"))
	if err != nil {
		t.Fatal(err)
	}
	if !change.Identity.RawEquals(fakeIdentity("abc")) {
		t.Errorf("planned identity = %#v", change.Identity)
	}
	if len(change.Warnings) != 1 || change.Warnings[0].Summary != "deprecated" || change.Warnings[0].Severity != "warning" {
		t.Errorf("plan warnings = %+v", change.Warnings)
	}

	f.apply = func(req *tfplugin5.ApplyResourceChange_Request) *tfplugin5.ApplyResourceChange_Response {
		if got := mustDecode(t, req.PlannedIdentity.GetIdentityData(), fakeIdentityType); !got.RawEquals(fakeIdentity("abc")) {
			t.Errorf("apply: planned identity = %#v", got)
		}
		return &tfplugin5.ApplyResourceChange_Response{
			NewState:    mustEncode(t, mustDecode(t, req.PlannedState, ty)),
			NewIdentity: identityData(t, fakeIdentity("abc")),
		}
	}
	res, err := p.ApplyResource(ctx, fakeType, prior, change, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Identity.RawEquals(fakeIdentity("abc")) {
		t.Errorf("applied identity = %#v", res.Identity)
	}

	f.read = func(req *tfplugin5.ReadResource_Request) *tfplugin5.ReadResource_Response {
		if got := mustDecode(t, req.CurrentIdentity.GetIdentityData(), fakeIdentityType); !got.RawEquals(fakeIdentity("abc")) {
			t.Errorf("read: current identity = %#v", got)
		}
		return &tfplugin5.ReadResource_Response{
			NewState:    req.CurrentState,
			Private:     []byte("r"),
			NewIdentity: identityData(t, fakeIdentity("abc")),
		}
	}
	rr, err := p.ReadResource(ctx, fakeType, res.State, res.Private, res.Identity)
	if err != nil {
		t.Fatal(err)
	}
	if !rr.State.RawEquals(res.State) || string(rr.Private) != "r" || !rr.Identity.RawEquals(fakeIdentity("abc")) {
		t.Errorf("read result = %+v", rr)
	}
}

func TestPlanResourceNilConfigIsNull(t *testing.T) {
	f := &fakeClient{}
	p := newFakeProvider(f)
	prior := fakeState(t, p, "x", "abc")
	f.plan = func(req *tfplugin5.PlanResourceChange_Request) *tfplugin5.PlanResourceChange_Response {
		if c := mustDecode(t, req.Config, prior.Type()); !c.IsNull() {
			t.Errorf("config = %#v, want null", c)
		}
		if pn := mustDecode(t, req.ProposedNewState, prior.Type()); !pn.IsNull() {
			t.Errorf("proposed = %#v, want null", pn)
		}
		return &tfplugin5.PlanResourceChange_Response{}
	}
	change, err := p.PlanResource(context.Background(), fakeType, prior, cty.NilVal, nil, cty.NilVal)
	if err != nil {
		t.Fatal(err)
	}
	if !change.Planned.IsNull() {
		t.Errorf("planned = %#v, want null", change.Planned)
	}
}

func TestDeleteResourcePlansDestroy(t *testing.T) {
	for _, planDestroy := range []bool{true, false} {
		t.Run(fmt.Sprintf("planDestroy=%v", planDestroy), func(t *testing.T) {
			f := &fakeClient{planDestroy: planDestroy, identities: fakeIdentitySchemas()}
			p := newFakeProvider(f)
			prior := fakeState(t, p, "x", "abc")
			ty := prior.Type()
			f.plan = func(req *tfplugin5.PlanResourceChange_Request) *tfplugin5.PlanResourceChange_Response {
				if !mustDecode(t, req.Config, ty).IsNull() || !mustDecode(t, req.ProposedNewState, ty).IsNull() {
					t.Error("destroy plan must send a null config and proposed state")
				}
				if !mustDecode(t, req.PriorState, ty).RawEquals(prior) {
					t.Error("destroy plan must send the prior state")
				}
				if req.PriorIdentity == nil {
					t.Error("destroy plan must send the prior identity")
				}
				return &tfplugin5.PlanResourceChange_Response{PlannedState: req.ProposedNewState, PlannedPrivate: []byte("pp")}
			}
			f.apply = func(req *tfplugin5.ApplyResourceChange_Request) *tfplugin5.ApplyResourceChange_Response {
				if !mustDecode(t, req.PlannedState, ty).IsNull() || !mustDecode(t, req.Config, ty).IsNull() {
					t.Error("destroy apply must send a null planned state and config")
				}
				want := "priv"
				if planDestroy {
					want = "pp"
				}
				if string(req.PlannedPrivate) != want {
					t.Errorf("planned private = %q, want %q", req.PlannedPrivate, want)
				}
				return &tfplugin5.ApplyResourceChange_Response{}
			}
			if err := p.DeleteResource(context.Background(), fakeType, prior, []byte("priv"), fakeIdentity("abc")); err != nil {
				t.Fatal(err)
			}
			want := []string{"GetSchema", "GetResourceIdentitySchemas", "PlanResourceChange", "ApplyResourceChange"}
			if !planDestroy {
				want = []string{"GetSchema", "GetResourceIdentitySchemas", "ApplyResourceChange"}
			}
			if !slices.Equal(f.calls, want) {
				t.Errorf("calls = %v, want %v", f.calls, want)
			}
		})
	}
}

func TestDiagsWarnings(t *testing.T) {
	ds := []*tfplugin5.Diagnostic{
		{Severity: tfplugin5.Diagnostic_ERROR, Summary: "bad"},
		{Severity: tfplugin5.Diagnostic_WARNING, Summary: "w1", Detail: "d"},
		{Severity: tfplugin5.Diagnostic_WARNING, Summary: "w2"},
	}
	want := []Diagnostic{{Severity: "warning", Summary: "w1", Detail: "d"}, {Severity: "warning", Summary: "w2"}}
	if got := diagsWarnings(ds); !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
	if got := diagsWarnings(ds[:1]); got != nil {
		t.Errorf("no warnings: got %+v, want nil", got)
	}
}

// A null nested block in a state the provider returns (SDKv2 providers
// return null for an empty block list: aws_ecs_service's
// deployment_configuration on Moto) is stored as an empty collection, as
// Terraform does (objchange.NormalizeObjectFromLegacySDK): passed back as
// the prior state, the null made the provider plan the block as unknown.
func TestPlanReadAndApplyNormalizeNullBlocks(t *testing.T) {
	f := &fakeClient{}
	p := newFakeProvider(f)
	b, err := p.resourceBlock(context.Background(), fakeType)
	if err != nil {
		t.Fatal(err)
	}
	ty := b.ImpliedType()
	returned := cty.ObjectVal(map[string]cty.Value{
		"name": cty.StringVal("x"), "id": cty.StringVal("abc"), "tags": cty.NullVal(cty.Map(cty.String)),
		"rule": cty.NullVal(ty.AttributeType("rule")),
	})
	f.read = func(*tfplugin5.ReadResource_Request) *tfplugin5.ReadResource_Response {
		return &tfplugin5.ReadResource_Response{NewState: mustEncode(t, returned)}
	}
	f.apply = func(*tfplugin5.ApplyResourceChange_Request) *tfplugin5.ApplyResourceChange_Response {
		return &tfplugin5.ApplyResourceChange_Response{NewState: mustEncode(t, returned)}
	}
	f.plan = func(*tfplugin5.PlanResourceChange_Request) *tfplugin5.PlanResourceChange_Response {
		return &tfplugin5.PlanResourceChange_Response{PlannedState: mustEncode(t, returned)}
	}
	want := cty.ListValEmpty(ty.AttributeType("rule").ElementType())
	pc, err := p.PlanResource(context.Background(), fakeType, cty.NilVal, fakeConfig(t, p, "x"), nil, cty.NilVal)
	if err != nil {
		t.Fatal(err)
	}
	if got := pc.Planned.GetAttr("rule"); !got.RawEquals(want) {
		t.Errorf("plan: rule = %#v, want an empty list", got)
	}
	rr, err := p.ReadResource(context.Background(), fakeType, fakeState(t, p, "x", "abc"), nil, cty.NilVal)
	if err != nil {
		t.Fatal(err)
	}
	if got := rr.State.GetAttr("rule"); !got.RawEquals(want) {
		t.Errorf("read: rule = %#v, want an empty list", got)
	}
	cfg := fakeConfig(t, p, "x")
	res, err := p.ApplyResource(context.Background(), fakeType, cty.NilVal, PlannedChange{Planned: cfg}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got := res.State.GetAttr("rule"); !got.RawEquals(want) {
		t.Errorf("apply: rule = %#v, want an empty list", got)
	}
	if got := res.State.GetAttr("tags"); !got.IsNull() {
		t.Errorf("apply: tags = %#v, want null: attributes are left alone", got)
	}
}
