package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"strconv"
	"strings"

	"github.com/zclconf/go-cty/cty"

	"github.com/giammbo/prosa/internal/provider"
)

// ownerTag is the ownership tag the engine adds to every resource whose
// schema has a map(string) "tags" attribute (ADR-016).
const ownerTag = "prosa/intent"

// envTag names the environment, on top of ownerTag, when there is one.
const envTag = "prosa/env"

// buildConfig turns a planner configuration into the provider config value
// of block b: it rejects sensitive literals (ADR-011), substitutes every
// reference with the attribute of the value lookup returns for its node,
// adds the ownership tag and converts the result with the schema.
func buildConfig(b *provider.Block, intentName, envName, key string, cfg map[string]any, keysOf func(string) []string, lookup func(node string) (cty.Value, bool)) (cty.Value, error) {
	where := intentName + "/" + key
	if err := checkSensitive(b, cfg, where); err != nil {
		return cty.NilVal, err
	}
	sub, err := Substitute(cfg, keysOf, func(r Ref) (cty.Value, error) {
		v, ok := lookup(r.Node())
		if !ok {
			return cty.NilVal, fmt.Errorf("%s has no value", r.Node())
		}
		return attrOf(v, r)
	})
	if err != nil {
		return cty.NilVal, fmt.Errorf("%s: %w", where, err)
	}
	if err := addOwnerTag(b, sub, intentName, envName); err != nil {
		return cty.NilVal, fmt.Errorf("%s: %w", where, err)
	}
	if err := encodeJSONDocuments(b, sub); err != nil {
		return cty.NilVal, fmt.Errorf("%s: %w", where, err)
	}
	v, err := provider.ObjectFromMap(b, sub)
	if err != nil {
		return cty.NilVal, fmt.Errorf("%s: %w", where, err)
	}
	return v, nil
}

// addOwnerTag sets the ownership tag in cfg["tags"] when b has a map(string)
// tags attribute, overriding a value from the planner; with an environment
// (envName not empty) it also sets the environment tag. cfg must be a copy.
func addOwnerTag(b *provider.Block, cfg map[string]any, intentName, envName string) error {
	a, ok := b.Attributes["tags"]
	if !ok || !a.Type.Equals(cty.Map(cty.String)) {
		return nil
	}
	tags := map[string]any{}
	switch t := cfg["tags"].(type) {
	case nil:
	case map[string]any:
		maps.Copy(tags, t)
	case cty.Value:
		switch {
		case t.IsNull():
		case !t.IsWhollyKnown():
			return fmt.Errorf("tags are not known before apply: cannot add the %s tag", ownerTag)
		case t.Type().IsMapType() || t.Type().IsObjectType():
			for it := t.ElementIterator(); it.Next(); {
				k, v := it.Element()
				tags[k.AsString()] = v
			}
		default:
			return fmt.Errorf("tags: expected an object, got %s", provider.JSONKind(t))
		}
	default:
		return fmt.Errorf("tags: expected an object, got %s", provider.JSONKind(t))
	}
	tags[ownerTag] = intentName
	if envName != "" {
		tags[envTag] = envName
	}
	cfg["tags"] = tags
	return nil
}

// attrOf selects the attribute path of r in v: object attributes, map keys
// and list or tuple indexes. An unknown value yields an unknown.
func attrOf(v cty.Value, r Ref) (cty.Value, error) {
	for i, a := range r.Attr {
		if !v.IsKnown() {
			return cty.DynamicVal, nil
		}
		if v.IsNull() {
			if i == 0 {
				return cty.NilVal, fmt.Errorf("%s does not exist", r.Node())
			}
			return cty.NilVal, fmt.Errorf("%s: %s is null", r, r.Attr[i-1])
		}
		ty := v.Type()
		switch {
		case ty.IsObjectType():
			if !ty.HasAttribute(a) {
				return cty.NilVal, fmt.Errorf("%s: %s has no attribute %q", r, r.Node(), a)
			}
			v = v.GetAttr(a)
		case ty.IsMapType():
			k := cty.StringVal(a)
			if !v.HasIndex(k).True() {
				return cty.NilVal, fmt.Errorf("%s: no key %q", r, a)
			}
			v = v.Index(k)
		case ty.IsListType() || ty.IsTupleType():
			n, err := strconv.Atoi(a)
			if err != nil || n < 0 || n >= v.LengthInt() {
				return cty.NilVal, fmt.Errorf("%s: invalid index %q", r, a)
			}
			v = v.Index(cty.NumberIntVal(int64(n)))
		default:
			return cty.NilVal, fmt.Errorf("%s: cannot select %q in a %s", r, a, ty.FriendlyName())
		}
	}
	return v, nil
}

// encodeJSONDocuments replaces, in the substituted configuration cfg of
// block b, every map or list given to a string attribute (also inside nested
// blocks) with its JSON encoding: keys sorted, no whitespace, numbers as
// written, HTML not escaped. A document with an unknown value inside becomes
// an unknown string. cfg must be a copy.
func encodeJSONDocuments(b *provider.Block, cfg map[string]any) error {
	for name, a := range b.Attributes {
		v, ok := cfg[name]
		if !ok || !a.Type.Equals(cty.String) {
			continue
		}
		if _, isStr := v.(string); isStr {
			continue
		}
		if _, isCty := v.(cty.Value); isCty || v == nil {
			continue
		}
		if !isContainer(v) {
			continue
		}
		doc, known, err := jsonValue(v, name)
		if err != nil {
			return err
		}
		if !known {
			cfg[name] = cty.UnknownVal(cty.String)
			continue
		}
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(doc); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		cfg[name] = cty.StringVal(strings.TrimSuffix(buf.String(), "\n"))
	}
	for name, nb := range b.BlockTypes {
		v, ok := cfg[name]
		if !ok || v == nil {
			continue
		}
		switch t := v.(type) {
		case map[string]any:
			if nb.Nesting == provider.NestingMap {
				for _, e := range t {
					if m, ok := e.(map[string]any); ok {
						if err := encodeJSONDocuments(nb.Block, m); err != nil {
							return fmt.Errorf("block %q: %w", name, err)
						}
					}
				}
			} else if err := encodeJSONDocuments(nb.Block, t); err != nil {
				return fmt.Errorf("block %q: %w", name, err)
			}
		case []any:
			for i, e := range t {
				if m, ok := e.(map[string]any); ok {
					if err := encodeJSONDocuments(nb.Block, m); err != nil {
						return fmt.Errorf("block %q: item %d: %w", name, i, err)
					}
				}
			}
		}
	}
	return nil
}

// isContainer reports whether v is a slice or a map (not raw bytes).
func isContainer(v any) bool {
	switch reflect.ValueOf(v).Kind() {
	case reflect.Map:
		return true
	case reflect.Slice, reflect.Array:
		return reflect.TypeOf(v).Elem().Kind() != reflect.Uint8
	}
	return false
}

// jsonValue converts a substituted configuration value to plain Go values the
// JSON encoder writes deterministically (maps sort their keys). known is
// false when a cty leaf is not wholly known.
func jsonValue(v any, path string) (out any, known bool, err error) {
	switch t := v.(type) {
	case nil:
		return nil, true, nil
	case cty.Value:
		return ctyToGo(t, path)
	}
	kids, isMap, ok, err := children(v, path)
	if err != nil {
		return nil, false, err
	}
	if !ok {
		return v, true, nil
	}
	if isMap {
		m := make(map[string]any, len(kids))
		for _, c := range kids {
			e, k, err := jsonValue(c.value, c.path)
			if err != nil || !k {
				return nil, false, err
			}
			m[c.key] = e
		}
		return m, true, nil
	}
	l := make([]any, len(kids))
	for i, c := range kids {
		e, k, err := jsonValue(c.value, c.path)
		if err != nil || !k {
			return nil, false, err
		}
		l[i] = e
	}
	return l, true, nil
}

// ctyToGo converts a known cty value to plain Go values (numbers as
// json.Number with their exact digits).
func ctyToGo(v cty.Value, path string) (any, bool, error) {
	if !v.IsKnown() {
		return nil, false, nil
	}
	if v.IsNull() {
		return nil, true, nil
	}
	ty := v.Type()
	switch {
	case ty == cty.String:
		return v.AsString(), true, nil
	case ty == cty.Bool:
		return v.True(), true, nil
	case ty == cty.Number:
		return json.Number(v.AsBigFloat().Text('f', -1)), true, nil
	case ty.IsListType() || ty.IsSetType() || ty.IsTupleType():
		l := make([]any, 0, v.LengthInt())
		for it := v.ElementIterator(); it.Next(); {
			_, e := it.Element()
			g, k, err := ctyToGo(e, path)
			if err != nil || !k {
				return nil, false, err
			}
			l = append(l, g)
		}
		return l, true, nil
	case ty.IsMapType() || ty.IsObjectType():
		m := map[string]any{}
		for it := v.ElementIterator(); it.Next(); {
			k, e := it.Element()
			g, known, err := ctyToGo(e, path)
			if err != nil || !known {
				return nil, false, err
			}
			m[k.AsString()] = g
		}
		return m, true, nil
	}
	return nil, false, fmt.Errorf("%s: cannot encode a %s value as JSON", path, ty.FriendlyName())
}
