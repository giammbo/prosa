package engine

import (
	"maps"
	"slices"

	"github.com/giammbo/prosa/internal/provider"
)

// canonicalConfig returns a copy of cfg, a configuration of block b, in the
// one textual form the lock keeps: a lone object given for a list or set
// block of at most one item, at any depth, becomes a one-element list. A
// lone object for a block that allows more items, and names b does not
// have, are left for the validation to report. cfg is not modified.
func canonicalConfig(b *provider.Block, cfg map[string]any) map[string]any {
	out := maps.Clone(cfg)
	for name, nb := range b.BlockTypes {
		v, ok := cfg[name]
		if !ok || v == nil {
			continue
		}
		switch t := v.(type) {
		case map[string]any:
			switch {
			case nb.Nesting == provider.NestingMap:
				m := make(map[string]any, len(t))
				for k, e := range t {
					m[k] = canonicalElem(nb.Block, e)
				}
				out[name] = m
			case (nb.Nesting == provider.NestingList || nb.Nesting == provider.NestingSet) && nb.MaxItems == 1:
				out[name] = []any{canonicalConfig(nb.Block, t)}
			default:
				out[name] = canonicalConfig(nb.Block, t)
			}
		case []any:
			l := make([]any, len(t))
			for i, e := range t {
				l[i] = canonicalElem(nb.Block, e)
			}
			out[name] = l
		}
	}
	return out
}

// canonicalElem is canonicalConfig for an element of a block collection,
// which may not be an object.
func canonicalElem(b *provider.Block, v any) any {
	if m, ok := v.(map[string]any); ok {
		return canonicalConfig(b, m)
	}
	return v
}

// canonical returns d with every resource of a known type in canonical form
// (see canonicalConfig).
func (p *Plan) canonical(d Desired) Desired {
	if len(d.Resources) == 0 {
		return d
	}
	out := d
	out.Resources = make([]DesiredResource, len(d.Resources))
	for i, r := range d.Resources {
		if rs := p.schema.Resources[r.Type]; rs != nil && r.Config != nil {
			r.Config = canonicalConfig(rs.Block, r.Config)
		}
		if len(r.DependsOn) > 0 {
			r.DependsOn = slices.Compact(slices.Sorted(slices.Values(r.DependsOn)))
		}
		out.Resources[i] = r
	}
	return out
}
