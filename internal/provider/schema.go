package provider

import (
	"context"
	"fmt"

	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/giammbo/prosa/internal/tfplugin5"
)

// Schema is the provider schema, converted from the protocol 5 messages.
type Schema struct {
	Provider    *Block
	Resources   map[string]*ResourceSchema
	DataSources map[string]*ResourceSchema
	// Identities holds the identity schema of the resource types that
	// declare one; it is empty for providers without resource identity.
	Identities map[string]*IdentitySchema
	// PlanDestroy is the provider's plan_destroy capability: it expects a
	// PlanResourceChange call before a resource is destroyed.
	PlanDestroy bool
}

// IdentitySchema describes the identity data of one resource type: a
// versioned object that identifies a remote object independently of its state.
type IdentitySchema struct {
	Version int64
	Type    cty.Type // an object type, one attribute per identity attribute
}

// ResourceSchema is the schema of one resource or data source type.
type ResourceSchema struct {
	Version int64
	Block   *Block
}

// Block describes a configuration block: attributes and nested blocks.
type Block struct {
	Description string
	Attributes  map[string]*Attribute
	BlockTypes  map[string]*NestedBlock
}

// Attribute describes one attribute. Sensitive and WriteOnly are kept so that
// callers can filter values that must never reach the lockfile.
type Attribute struct {
	Type                                               cty.Type
	Required, Optional, Computed, Sensitive, WriteOnly bool
	Description                                        string
}

// NestingMode is how a nested block repeats inside its parent.
type NestingMode int

const (
	NestingSingle NestingMode = iota + 1
	NestingGroup
	NestingList
	NestingSet
	NestingMap
)

// NestedBlock is a block type nested inside another block.
type NestedBlock struct {
	Nesting            NestingMode
	Block              *Block
	MinItems, MaxItems int64
}

// ImpliedType returns the cty object type that values of this block have.
func (b *Block) ImpliedType() cty.Type {
	attrs := make(map[string]cty.Type, len(b.Attributes)+len(b.BlockTypes))
	for name, a := range b.Attributes {
		attrs[name] = a.Type
	}
	for name, nb := range b.BlockTypes {
		inner := nb.Block.ImpliedType()
		switch nb.Nesting {
		case NestingList:
			attrs[name] = cty.List(inner)
		case NestingSet:
			attrs[name] = cty.Set(inner)
		case NestingMap:
			attrs[name] = cty.Map(inner)
		default: // single, group
			attrs[name] = inner
		}
	}
	return cty.Object(attrs)
}

// Schema returns the provider schema, fetching it once per process.
func (p *Provider) Schema(ctx context.Context) (*Schema, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.schema != nil {
		return p.schema, nil
	}
	resp, err := p.client.GetSchema(ctx, &tfplugin5.GetProviderSchema_Request{})
	if err != nil {
		return nil, fmt.Errorf("get provider schema: %w", err)
	}
	if err := diagsErr("get provider schema", resp.Diagnostics); err != nil {
		return nil, err
	}
	s := &Schema{
		Resources:   make(map[string]*ResourceSchema, len(resp.ResourceSchemas)),
		DataSources: make(map[string]*ResourceSchema, len(resp.DataSourceSchemas)),
		PlanDestroy: resp.GetServerCapabilities().GetPlanDestroy(),
	}
	if resp.Provider != nil {
		ps, err := convertSchema(resp.Provider)
		if err != nil {
			return nil, fmt.Errorf("provider schema: %w", err)
		}
		s.Provider = ps.Block
	}
	for name, rs := range resp.ResourceSchemas {
		c, err := convertSchema(rs)
		if err != nil {
			return nil, fmt.Errorf("resource %q: %w", name, err)
		}
		s.Resources[name] = c
	}
	for name, ds := range resp.DataSourceSchemas {
		c, err := convertSchema(ds)
		if err != nil {
			return nil, fmt.Errorf("data source %q: %w", name, err)
		}
		s.DataSources[name] = c
	}
	if s.Identities, err = p.identitySchemas(ctx); err != nil {
		return nil, err
	}
	p.schema = s
	return s, nil
}

// identitySchemas fetches the resource identity schemas. A provider that
// does not implement the RPC has no identities.
func (p *Provider) identitySchemas(ctx context.Context) (map[string]*IdentitySchema, error) {
	out := map[string]*IdentitySchema{}
	resp, err := p.client.GetResourceIdentitySchemas(ctx, &tfplugin5.GetResourceIdentitySchemas_Request{})
	if status.Code(err) == codes.Unimplemented {
		return out, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get resource identity schemas: %w", err)
	}
	if err := diagsErr("get resource identity schemas", resp.GetDiagnostics()); err != nil {
		return nil, err
	}
	for name, is := range resp.GetIdentitySchemas() {
		attrs := make(map[string]cty.Type, len(is.GetIdentityAttributes()))
		for _, a := range is.GetIdentityAttributes() {
			t, err := ctyjson.UnmarshalType(a.GetType())
			if err != nil {
				return nil, fmt.Errorf("identity of %q: attribute %q: invalid type: %w", name, a.GetName(), err)
			}
			attrs[a.GetName()] = t
		}
		out[name] = &IdentitySchema{Version: is.GetVersion(), Type: cty.Object(attrs)}
	}
	return out, nil
}

func convertSchema(in *tfplugin5.Schema) (*ResourceSchema, error) {
	b, err := convertBlock(in.GetBlock())
	if err != nil {
		return nil, err
	}
	return &ResourceSchema{Version: in.GetVersion(), Block: b}, nil
}

func convertBlock(in *tfplugin5.Schema_Block) (*Block, error) {
	b := &Block{
		Description: in.GetDescription(),
		Attributes:  map[string]*Attribute{},
		BlockTypes:  map[string]*NestedBlock{},
	}
	for _, a := range in.GetAttributes() {
		t, err := ctyjson.UnmarshalType(a.Type)
		if err != nil {
			return nil, fmt.Errorf("attribute %q: invalid type: %w", a.Name, err)
		}
		b.Attributes[a.Name] = &Attribute{
			Type:        t,
			Required:    a.Required,
			Optional:    a.Optional,
			Computed:    a.Computed,
			Sensitive:   a.Sensitive,
			WriteOnly:   a.WriteOnly,
			Description: a.Description,
		}
	}
	for _, nb := range in.GetBlockTypes() {
		mode, err := convertNesting(nb.Nesting)
		if err != nil {
			return nil, fmt.Errorf("block %q: %w", nb.TypeName, err)
		}
		inner, err := convertBlock(nb.Block)
		if err != nil {
			return nil, fmt.Errorf("block %q: %w", nb.TypeName, err)
		}
		if (mode == NestingList || mode == NestingSet) && hasDynamic(inner) {
			return nil, fmt.Errorf("unsupported dynamic block %q", nb.TypeName)
		}
		b.BlockTypes[nb.TypeName] = &NestedBlock{
			Nesting:  mode,
			Block:    inner,
			MinItems: nb.MinItems,
			MaxItems: nb.MaxItems,
		}
	}
	return b, nil
}

func convertNesting(m tfplugin5.Schema_NestedBlock_NestingMode) (NestingMode, error) {
	switch m {
	case tfplugin5.Schema_NestedBlock_SINGLE:
		return NestingSingle, nil
	case tfplugin5.Schema_NestedBlock_GROUP:
		return NestingGroup, nil
	case tfplugin5.Schema_NestedBlock_LIST:
		return NestingList, nil
	case tfplugin5.Schema_NestedBlock_SET:
		return NestingSet, nil
	case tfplugin5.Schema_NestedBlock_MAP:
		return NestingMap, nil
	}
	return 0, fmt.Errorf("unsupported nesting mode %s", m)
}

// hasDynamic reports whether the block directly contains a DynamicPseudoType
// attribute. Terraform would render such list/set blocks as tuples.
func hasDynamic(b *Block) bool {
	for _, a := range b.Attributes {
		if a.Type.HasDynamicTypes() {
			return true
		}
	}
	return false
}
