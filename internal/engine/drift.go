package engine

import (
	"slices"

	"github.com/zclconf/go-cty/cty"

	"github.com/giammbo/prosa/internal/provider"
)

// driftOf compares the stored and refreshed states of a resource of block b
// (ADR-018). Each top-level attribute or block that differs is classified:
// a computed-only attribute never drifts; one set (non-null) in the resolved
// config contradicts the chosen configuration (level 2, drift); any other is
// informational (level 1, info). Null and empty collections are the same at
// any depth. cfg may be cty.NilVal (a delete): every difference is then
// informational. Both lists are sorted.
func driftOf(b *provider.Block, stored, refreshed, cfg cty.Value) (drift, info []string) {
	if stored.IsNull() || refreshed.IsNull() {
		return nil, nil
	}
	var names []string
	for name, a := range b.Attributes {
		if !(a.Computed && !a.Optional && !a.Required) {
			names = append(names, name)
		}
	}
	for name := range b.BlockTypes {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if sameValue(normalizeEmpty(stored.GetAttr(name)), normalizeEmpty(refreshed.GetAttr(name))) {
			continue
		}
		if cfg != cty.NilVal && !cfg.IsNull() && !normalizeEmpty(cfg.GetAttr(name)).IsNull() {
			drift = append(drift, name)
		} else {
			info = append(info, name)
		}
	}
	return drift, info
}

// normalizeEmpty replaces every known empty list, set or map in v, at any
// depth, with a null of the same type.
func normalizeEmpty(v cty.Value) cty.Value {
	out, err := cty.Transform(v, func(_ cty.Path, v cty.Value) (cty.Value, error) {
		ty := v.Type()
		if v.IsKnown() && !v.IsNull() && (ty.IsListType() || ty.IsSetType() || ty.IsMapType()) && v.LengthInt() == 0 {
			return cty.NullVal(ty), nil
		}
		return v, nil
	})
	if err != nil {
		return v
	}
	return out
}
