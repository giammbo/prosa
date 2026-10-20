package provider

import (
	"context"
	"testing"

	"github.com/zclconf/go-cty/cty"
	"google.golang.org/grpc"

	"github.com/giammbo/prosa/internal/tfplugin5"
)

const fakeType = "test_thing"

// fakeClient is a tfplugin5.ProviderClient for unit tests. The embedded
// interface is nil, so an RPC the test does not expect panics.
type fakeClient struct {
	tfplugin5.ProviderClient

	planDestroy bool
	identities  map[string]*tfplugin5.ResourceIdentitySchema
	identityErr error

	plan  func(*tfplugin5.PlanResourceChange_Request) *tfplugin5.PlanResourceChange_Response
	apply func(*tfplugin5.ApplyResourceChange_Request) *tfplugin5.ApplyResourceChange_Response
	read  func(*tfplugin5.ReadResource_Request) *tfplugin5.ReadResource_Response

	calls []string
}

func (f *fakeClient) GetSchema(context.Context, *tfplugin5.GetProviderSchema_Request, ...grpc.CallOption) (*tfplugin5.GetProviderSchema_Response, error) {
	f.calls = append(f.calls, "GetSchema")
	attr := func(name string, req, opt, comp bool, ty string) *tfplugin5.Schema_Attribute {
		return &tfplugin5.Schema_Attribute{Name: name, Type: []byte(ty), Required: req, Optional: opt, Computed: comp}
	}
	return &tfplugin5.GetProviderSchema_Response{
		ServerCapabilities: &tfplugin5.ServerCapabilities{PlanDestroy: f.planDestroy},
		ResourceSchemas: map[string]*tfplugin5.Schema{
			fakeType: {Block: &tfplugin5.Schema_Block{Attributes: []*tfplugin5.Schema_Attribute{
				attr("name", true, false, false, `"string"`),
				attr("id", false, false, true, `"string"`),
				attr("tags", false, true, false, `["map","string"]`),
			}, BlockTypes: []*tfplugin5.Schema_NestedBlock{{
				TypeName: "rule", Nesting: tfplugin5.Schema_NestedBlock_LIST,
				Block: &tfplugin5.Schema_Block{Attributes: []*tfplugin5.Schema_Attribute{attr("port", false, true, false, `"number"`)}},
			}}}},
		},
	}, nil
}

func (f *fakeClient) GetResourceIdentitySchemas(context.Context, *tfplugin5.GetResourceIdentitySchemas_Request, ...grpc.CallOption) (*tfplugin5.GetResourceIdentitySchemas_Response, error) {
	f.calls = append(f.calls, "GetResourceIdentitySchemas")
	if f.identityErr != nil {
		return nil, f.identityErr
	}
	return &tfplugin5.GetResourceIdentitySchemas_Response{IdentitySchemas: f.identities}, nil
}

func (f *fakeClient) PlanResourceChange(_ context.Context, req *tfplugin5.PlanResourceChange_Request, _ ...grpc.CallOption) (*tfplugin5.PlanResourceChange_Response, error) {
	f.calls = append(f.calls, "PlanResourceChange")
	return f.plan(req), nil
}

func (f *fakeClient) ApplyResourceChange(_ context.Context, req *tfplugin5.ApplyResourceChange_Request, _ ...grpc.CallOption) (*tfplugin5.ApplyResourceChange_Response, error) {
	f.calls = append(f.calls, "ApplyResourceChange")
	return f.apply(req), nil
}

func (f *fakeClient) ReadResource(_ context.Context, req *tfplugin5.ReadResource_Request, _ ...grpc.CallOption) (*tfplugin5.ReadResource_Response, error) {
	f.calls = append(f.calls, "ReadResource")
	return f.read(req), nil
}

// fakeIdentitySchemas declares an identity {id: string} for fakeType.
func fakeIdentitySchemas() map[string]*tfplugin5.ResourceIdentitySchema {
	return map[string]*tfplugin5.ResourceIdentitySchema{
		fakeType: {Version: 2, IdentityAttributes: []*tfplugin5.ResourceIdentitySchema_IdentityAttribute{
			{Name: "id", Type: []byte(`"string"`), RequiredForImport: true},
		}},
	}
}

var fakeIdentityType = cty.Object(map[string]cty.Type{"id": cty.String})

func fakeIdentity(id string) cty.Value {
	return cty.ObjectVal(map[string]cty.Value{"id": cty.StringVal(id)})
}

func fakeState(t *testing.T, p *Provider, name, id string) cty.Value {
	t.Helper()
	b, err := p.resourceBlock(context.Background(), fakeType)
	if err != nil {
		t.Fatal(err)
	}
	return withAttr(t, b, map[string]any{"name": name}, "id", cty.StringVal(id))
}

func fakeConfig(t *testing.T, p *Provider, name string) cty.Value {
	t.Helper()
	b, err := p.resourceBlock(context.Background(), fakeType)
	if err != nil {
		t.Fatal(err)
	}
	v, err := ObjectFromMap(b, map[string]any{"name": name})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func mustEncode(t *testing.T, v cty.Value) *tfplugin5.DynamicValue {
	t.Helper()
	dv, err := Encode(v, v.Type())
	if err != nil {
		t.Fatal(err)
	}
	return dv
}

func mustDecode(t *testing.T, dv *tfplugin5.DynamicValue, ty cty.Type) cty.Value {
	t.Helper()
	v, err := Decode(dv, ty)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func identityData(t *testing.T, v cty.Value) *tfplugin5.ResourceIdentityData {
	t.Helper()
	return &tfplugin5.ResourceIdentityData{IdentityData: mustEncode(t, v)}
}
