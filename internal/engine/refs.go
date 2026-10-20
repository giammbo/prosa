package engine

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/convert"
)

// Ref is a reference to an attribute of another intent's resource:
// "${<intent>/<key>.<attr>[.<attr>...]}".
type Ref struct {
	Intent, Key string
	Attr        []string
}

// Node is the dependency-graph node the reference points to.
func (r Ref) Node() string { return r.Intent + "/" + r.Key }

func (r Ref) String() string {
	return "${" + r.Node() + "." + strings.Join(r.Attr, ".") + "}"
}

// TemplatePart is one part of a template (see ParseTemplate): literal Text,
// with every "$${" already turned into "${", or a reference.
type TemplatePart struct {
	Text string
	Ref  *Ref
}

// unescape turns every "$${" of a literal string into "${".
func unescape(s string) string { return strings.ReplaceAll(s, "$${", "${") }

// isWholeRef reports whether s has the shape of a single reference: it
// starts with "${" and its first "}" is its last byte.
func isWholeRef(s string) bool {
	return strings.HasPrefix(s, "${") && strings.IndexByte(s, '}') == len(s)-1
}

// ParseRef parses s as a whole-value reference. The bool is false when s is
// not one: a literal (no unescaped "${"; "$${" is a literal "${", see
// unescape) or a template (see ParseTemplate). Keys may contain dots: the key
// is the longest key known for the intent that is followed by "." and at
// least one attribute segment.
func ParseRef(s string, keysOf func(intent string) []string) (Ref, bool, error) {
	if !isWholeRef(s) {
		return Ref{}, false, nil
	}
	r, err := parseInner(s, keysOf)
	return r, true, err
}

// ParseTemplate parses s as a template (ADR-020): a string holding one or
// more references "${<intent>/<key>.<attribute>}" and other text, "$${"
// being a literal "${". The bool is false when s is not a template: a whole
// reference (ParseRef), or a string without unescaped "${". Adjacent texts
// are one part. An unclosed "${" or an invalid reference is an error naming
// the template.
func ParseTemplate(s string, keysOf func(intent string) []string) ([]TemplatePart, bool, error) {
	if !strings.Contains(s, "${") || isWholeRef(s) {
		return nil, false, nil
	}
	var parts []TemplatePart
	var text strings.Builder
	flush := func() {
		if text.Len() > 0 {
			parts = append(parts, TemplatePart{Text: text.String()})
			text.Reset()
		}
	}
	refs := 0
	for i := 0; i < len(s); {
		switch {
		case strings.HasPrefix(s[i:], "$${"):
			text.WriteString("${")
			i += 3
		case strings.HasPrefix(s[i:], "${"):
			end := strings.IndexByte(s[i:], '}')
			if end < 0 {
				return nil, true, fmt.Errorf(`template %q: unclosed "${" (a reference is ${<intent>/<key>.<attribute>}; write "$${" for a literal "${")`, s)
			}
			r, err := parseInner(s[i:i+end+1], keysOf)
			if err != nil {
				return nil, true, fmt.Errorf("template %q: %w", s, err)
			}
			flush()
			parts = append(parts, TemplatePart{Ref: &r})
			refs++
			i += end + 1
		default:
			text.WriteByte(s[i])
			i++
		}
	}
	if refs == 0 {
		return nil, false, nil
	}
	flush()
	return parts, true, nil
}

// parseInner parses ref, a string "${...}" whose only "}" is the last byte.
func parseInner(ref string, keysOf func(intent string) []string) (Ref, error) {
	inner := ref[2 : len(ref)-1]
	intent, rest, found := strings.Cut(inner, "/")
	if !found || intent == "" || rest == "" {
		return Ref{}, fmt.Errorf("reference %q: expected ${<intent>/<key>.<attribute>}; write $${ for a literal ${", ref)
	}
	for _, c := range intent {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return Ref{}, fmt.Errorf("reference %q: invalid intent name %q; write $${ for a literal ${", ref, intent)
		}
	}
	keys := keysOf(intent)
	if len(keys) == 0 {
		return Ref{}, fmt.Errorf("reference %q: unknown intent %q", ref, intent)
	}
	best := ""
	for _, k := range keys {
		if len(k) > len(best) && strings.HasPrefix(rest, k+".") && len(rest) > len(k)+1 {
			best = k
		}
	}
	if best == "" {
		key, _, _ := strings.Cut(rest, ".")
		if !strings.Contains(rest, ".") || strings.HasSuffix(rest, ".") {
			return Ref{}, fmt.Errorf("reference %q: expected ${<intent>/<key>.<attribute>} with at least one attribute", ref)
		}
		return Ref{}, fmt.Errorf("reference %q: unknown key %s/%s", ref, intent, key)
	}
	attrs := strings.Split(rest[len(best)+1:], ".")
	for _, a := range attrs {
		if a == "" {
			return Ref{}, fmt.Errorf("reference %q: empty attribute segment", ref)
		}
	}
	return Ref{Intent: intent, Key: best, Attr: attrs}, nil
}

// refsIn returns the references of s: none for a literal, one for a whole
// reference, those of a template.
func refsIn(s string, keysOf func(string) []string) ([]Ref, error) {
	if r, ok, err := ParseRef(s, keysOf); err != nil {
		return nil, err
	} else if ok {
		return []Ref{r}, nil
	}
	parts, _, err := ParseTemplate(s, keysOf)
	if err != nil {
		return nil, err
	}
	var refs []Ref
	for _, p := range parts {
		if p.Ref != nil {
			refs = append(refs, *p.Ref)
		}
	}
	return refs, nil
}

// children returns the elements of any slice/array or string-keyed map as
// (path, value) pairs in deterministic order. isMap tells the container kind.
// ok is false for scalars. A map with non-string keys is an error.
func children(v any, path string) (kids []child, isMap, ok bool, err error) {
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Slice, reflect.Array:
		if rv.Type().Elem().Kind() == reflect.Uint8 {
			return nil, false, false, nil // raw bytes are a scalar
		}
		for i := 0; i < rv.Len(); i++ {
			kids = append(kids, child{path: fmt.Sprintf("%s[%d]", path, i), value: rv.Index(i).Interface()})
		}
		return kids, false, true, nil
	case reflect.Map:
		if rv.Type().Key().Kind() != reflect.String {
			return nil, true, true, fmt.Errorf("%s: map with non-string keys (%s) is not supported", path, rv.Type().Key())
		}
		keys := rv.MapKeys()
		sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
		for _, k := range keys {
			kids = append(kids, child{key: k.String(), path: path + "." + k.String(), value: rv.MapIndex(k).Interface()})
		}
		return kids, true, true, nil
	}
	return nil, false, false, nil
}

type child struct {
	key, path string
	value     any
}

// CollectRefs returns the references found anywhere in cfg, whole values and
// template parts, sorted and without duplicates. Every invalid reference is reported (errors.Join).
func CollectRefs(cfg map[string]any, keysOf func(string) []string) ([]Ref, error) {
	seen := map[string]Ref{}
	var errs []error
	var walk func(v any, path string)
	walk = func(v any, path string) {
		if s, isStr := v.(string); isStr {
			rs, err := refsIn(s, keysOf)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", path, err))
			}
			for _, r := range rs {
				seen[r.String()] = r
			}
			return
		}
		if v == nil {
			return
		}
		kids, _, ok, err := children(v, path)
		if err != nil {
			errs = append(errs, err)
			return
		}
		if !ok {
			return
		}
		for _, c := range kids {
			walk(c.value, c.path)
		}
	}
	walk(cfg, "config")
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	refs := make([]Ref, 0, len(names))
	for _, n := range names {
		refs = append(refs, seen[n])
	}
	return refs, nil
}

// Substitute returns a deep copy of cfg where every reference string is
// replaced by the cty.Value returned by resolve, every template by its
// cty.String (see renderTemplate) and every "$${" of the other strings by a
// literal "${". Any slice becomes []any and
// any string-keyed map becomes map[string]any in the copy. cfg is not modified.
func Substitute(cfg map[string]any, keysOf func(string) []string, resolve func(Ref) (cty.Value, error)) (map[string]any, error) {
	var sub func(v any, path string) (any, error)
	sub = func(v any, path string) (any, error) {
		if s, isStr := v.(string); isStr {
			r, ok, err := ParseRef(s, keysOf)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", path, err)
			}
			if !ok {
				parts, isTemplate, err := ParseTemplate(s, keysOf)
				if err != nil {
					return nil, fmt.Errorf("%s: %w", path, err)
				}
				if !isTemplate {
					return unescape(s), nil
				}
				val, err := renderTemplate(parts, resolve)
				if err != nil {
					return nil, fmt.Errorf("%s: template %q: %w", path, s, err)
				}
				return val, nil
			}
			val, err := resolve(r)
			if err != nil {
				return nil, fmt.Errorf("resolve %s: %w", r, err)
			}
			return val, nil
		}
		if v == nil {
			return nil, nil
		}
		kids, isMap, ok, err := children(v, path)
		if err != nil {
			return nil, err
		}
		if !ok {
			return v, nil
		}
		if isMap {
			out := make(map[string]any, len(kids))
			for _, c := range kids {
				cv, err := sub(c.value, c.path)
				if err != nil {
					return nil, err
				}
				out[c.key] = cv
			}
			return out, nil
		}
		out := make([]any, len(kids))
		for i, c := range kids {
			cv, err := sub(c.value, c.path)
			if err != nil {
				return nil, err
			}
			out[i] = cv
		}
		return out, nil
	}
	out, err := sub(cfg, "config")
	if err != nil {
		return nil, err
	}
	return out.(map[string]any), nil
}

// renderTemplate joins the parts of a template into a cty.String, each
// reference resolved and converted to a string with cty's conversion rules
// (numbers in their exact decimal form, bools as "true"/"false"). A part not
// known yet makes the whole string unknown; a reference to a null or to a
// value that is not a string, number or bool is an error naming it.
func renderTemplate(parts []TemplatePart, resolve func(Ref) (cty.Value, error)) (cty.Value, error) {
	var b strings.Builder
	known := true
	for _, p := range parts {
		if p.Ref == nil {
			b.WriteString(p.Text)
			continue
		}
		v, err := resolve(*p.Ref)
		if err != nil {
			return cty.NilVal, fmt.Errorf("resolve %s: %w", p.Ref, err)
		}
		ty := v.Type()
		if !ty.Equals(cty.DynamicPseudoType) && !ty.IsPrimitiveType() {
			return cty.NilVal, fmt.Errorf("reference %s is a %s; a template takes only a string, number or bool", p.Ref, ty.FriendlyName())
		}
		if !v.IsKnown() {
			known = false
			continue
		}
		if v.IsNull() {
			return cty.NilVal, fmt.Errorf("reference %s is null", p.Ref)
		}
		str, err := convert.Convert(v, cty.String)
		if err != nil {
			return cty.NilVal, fmt.Errorf("reference %s: %w", p.Ref, err)
		}
		b.WriteString(str.AsString())
	}
	if !known {
		return cty.UnknownVal(cty.String), nil
	}
	return cty.StringVal(b.String()), nil
}
