package engine

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/giammbo/prosa/internal/lock"
	"github.com/giammbo/prosa/internal/provider"
)

// Sensitive values (ADR-011): an attribute is sensitive when the schema marks
// it Sensitive or WriteOnly. The planner never writes a sensitive value: it
// refers to one, and a reference never carries a sensitive value into an
// attribute that is not sensitive itself.

func isSensitive(a *provider.Attribute) bool { return a.Sensitive || a.WriteOnly }

// walkAttrs calls fn for every attribute set in cfg, in b or in its nested
// blocks, with its dotted path ("auth.client_secret"), in sorted order. Names
// unknown to b are skipped (ObjectFromMap reports them).
func walkAttrs(b *provider.Block, cfg map[string]any, prefix string, fn func(path string, a *provider.Attribute, v any) error) error {
	for _, name := range slices.Sorted(maps.Keys(cfg)) {
		v := cfg[name]
		if v == nil {
			continue
		}
		if a, ok := b.Attributes[name]; ok {
			if err := fn(prefix+name, a, v); err != nil {
				return err
			}
			continue
		}
		nb, ok := b.BlockTypes[name]
		if !ok {
			continue
		}
		var items []map[string]any
		switch t := v.(type) {
		case map[string]any:
			if nb.Nesting == provider.NestingMap {
				for _, k := range slices.Sorted(maps.Keys(t)) {
					if m, ok := t[k].(map[string]any); ok {
						items = append(items, m)
					}
				}
			} else {
				items = append(items, t)
			}
		case []any:
			for _, it := range t {
				if m, ok := it.(map[string]any); ok {
					items = append(items, m)
				}
			}
		}
		for _, m := range items {
			if err := walkAttrs(nb.Block, m, prefix+name+".", fn); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkSensitive rejects a literal value for a sensitive attribute, in b or
// in its nested blocks. Null and references are allowed.
func checkSensitive(b *provider.Block, cfg map[string]any, where string) error {
	return walkAttrs(b, cfg, "", func(path string, a *provider.Attribute, v any) error {
		if !isSensitive(a) {
			return nil
		}
		if s, isStr := v.(string); isStr {
			if _, isRef, _ := ParseRef(s, func(string) []string { return nil }); isRef {
				return nil
			}
		}
		kind := "sensitive"
		if a.WriteOnly {
			kind = "write-only"
		}
		return fmt.Errorf("attribute %q of %s is %s: never write its value; use a native mechanism such as manage_master_user_password or secrets.valueFrom, or reference a secret (ADR-011)", path, where, kind)
	})
}

// sensitiveRefs returns one message per reference in cfg (the config of
// block b, for resource where) that reads a sensitive value and sits in an
// attribute that is not sensitive. sourceOf returns the block of the
// resource a reference points to, nil when unknown.
func sensitiveRefs(b *provider.Block, cfg map[string]any, where string, keysOf func(string) []string, sourceOf func(Ref) *provider.Block) []string {
	var out []string
	_ = walkAttrs(b, cfg, "", func(path string, a *provider.Attribute, v any) error {
		if isSensitive(a) {
			return nil
		}
		refs, err := CollectRefs(map[string]any{path: v}, keysOf)
		if err != nil {
			return nil // reported when the references are resolved
		}
		for _, r := range refs {
			if src := sourceOf(r); src != nil && sourceSensitive(src, r.Attr) {
				out = append(out, fmt.Sprintf("reference %s carries a sensitive value into non-sensitive attribute %q of %s (ADR-011)", r, path, where))
			}
		}
		return nil
	})
	return out
}

// sourceSensitive reports whether the attribute path attrs of a resource of
// block b reads a sensitive value: the path reaches a sensitive attribute
// (or goes inside its value), or ends on a block that holds one.
func sourceSensitive(b *provider.Block, attrs []string) bool {
	for i := 0; i < len(attrs); i++ {
		if a, ok := b.Attributes[attrs[i]]; ok {
			return isSensitive(a)
		}
		nb, ok := b.BlockTypes[attrs[i]]
		if !ok {
			return false
		}
		b = nb.Block
		switch nb.Nesting {
		case provider.NestingList, provider.NestingSet, provider.NestingMap:
			i++ // the element index or key
		}
	}
	return holdsSensitive(b)
}

// holdsSensitive reports whether b has a sensitive attribute at any depth.
func holdsSensitive(b *provider.Block) bool {
	for _, a := range b.Attributes {
		if isSensitive(a) {
			return true
		}
	}
	for _, nb := range b.BlockTypes {
		if holdsSensitive(nb.Block) {
			return true
		}
	}
	return false
}

// patchSensitive returns the problems of ops, a patch of intent name on base,
// whose values hold a literal for a sensitive or write-only attribute: a set
// at such a path, or an add whose config has one. Each op is checked on its
// own, not only the state the patch resolves to, because the patch is stored
// as it is (lock, planner cache): an op a later one overrides must not carry
// a secret there either (ADR-011). Keys of unknown type are skipped.
func (p *Plan) patchSensitive(name string, base []lock.Resource, ops []lock.PatchOp) []ValidationProblem {
	if p.schema == nil {
		return nil
	}
	types := map[string]string{}
	for _, r := range base {
		types[r.Key] = r.Type
	}
	var probs []ValidationProblem
	for i, op := range ops {
		var cfg map[string]any
		switch op.Op {
		case "add":
			types[op.Key] = op.Type
			cfg = op.Config
		case "set":
			cfg, _ = nestPath(strings.Split(op.Path, "."), op.Value).(map[string]any)
		default:
			continue
		}
		rsch := p.schema.Resources[types[op.Key]]
		if rsch == nil || cfg == nil {
			continue
		}
		if err := checkSensitive(rsch.Block, cfg, name+"/"+op.Key); err != nil {
			probs = append(probs, ValidationProblem{Intent: name, Key: op.Key, Type: types[op.Key],
				Message: fmt.Sprintf("patch op %d (%s): %v", i, op.Op, err)})
		}
	}
	return probs
}

// nestPath returns v placed at the dotted path segs: a map per name, a
// one-element list per index, so that walkAttrs reaches it.
func nestPath(segs []string, v any) any {
	if len(segs) == 0 || segs[0] == "" {
		return v
	}
	inner := nestPath(segs[1:], v)
	if _, err := strconv.Atoi(segs[0]); err == nil {
		return []any{inner}
	}
	return map[string]any{segs[0]: inner}
}
