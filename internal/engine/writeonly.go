package engine

import (
	"strings"

	"github.com/zclconf/go-cty/cty"

	"github.com/giammbo/prosa/internal/provider"
)

// stripWriteOnly nulls every write-only attribute of b set in v, which a
// provider must never return (WriteOnlyAttributesAllowed), and returns the
// paths it nulled.
func stripWriteOnly(b *provider.Block, v cty.Value) (cty.Value, []string, error) {
	var paths []string
	out, err := cty.Transform(v, func(path cty.Path, v cty.Value) (cty.Value, error) {
		if v.IsNull() || !writeOnlyAt(b, path) {
			return v, nil
		}
		var names []string
		for _, s := range path {
			if g, ok := s.(cty.GetAttrStep); ok {
				names = append(names, g.Name)
			}
		}
		paths = append(paths, strings.Join(names, "."))
		return cty.NullVal(v.Type()), nil
	})
	return out, paths, err
}

// writeOnlyAt reports whether path, from the root of a b object, is a
// write-only attribute.
func writeOnlyAt(b *provider.Block, path cty.Path) bool {
	for i := 0; i < len(path); i++ {
		s, ok := path[i].(cty.GetAttrStep)
		if !ok {
			return false
		}
		if a, ok := b.Attributes[s.Name]; ok {
			return i == len(path)-1 && a.WriteOnly
		}
		nb, ok := b.BlockTypes[s.Name]
		if !ok {
			return false
		}
		b = nb.Block
		switch nb.Nesting {
		case provider.NestingList, provider.NestingSet, provider.NestingMap:
			i++ // the element step
			if i < len(path) {
				if _, ok := path[i].(cty.IndexStep); !ok {
					return false
				}
			}
		}
	}
	return false
}
