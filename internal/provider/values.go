package provider

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math/big"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/convert"
	ctyjson "github.com/zclconf/go-cty/cty/json"
	"github.com/zclconf/go-cty/cty/msgpack"

	"github.com/giammbo/prosa/internal/tfplugin5"
)

// ObjectFromMap builds a configuration value for block b from a JSON-like map.
// Missing attributes become null, missing blocks become empty according to
// their nesting mode, and unknown keys are an error (with the closest names
// of b as suggestions), as is a value for a computed-only attribute. Every
// problem is reported, joined with errors.Join, each one a complete message
// with its path ("block \"x\": item 0: attribute \"y\": ..."), in sorted
// order of the keys: unknown keys first, then attributes and blocks by name.
//
// A leaf (attribute value, or element of one) may be a cty.Value instead of a
// Go value, to inject references and unknowns: a known value is converted to
// the attribute type with cty's convert rules, as is an unknown of a known
// type (an unknown list cannot fill a string); cty.DynamicVal becomes an
// unknown of the attribute type. Raw Go values keep the stricter rules (a Go
// string where a number is expected is an error).
func ObjectFromMap(b *Block, m map[string]any) (cty.Value, error) {
	var errs []error
	for _, k := range sortedKeys(m) {
		_, isAttr := b.Attributes[k]
		_, isBlock := b.BlockTypes[k]
		if !isAttr && !isBlock {
			errs = append(errs, unknownName(b, k))
		}
	}
	names := append(sortedKeys(b.Attributes), sortedKeys(b.BlockTypes)...)
	sort.Strings(names)
	vals := make(map[string]cty.Value, len(names))
	for _, name := range names {
		a, isAttr := b.Attributes[name]
		if !isAttr {
			v, err := blockFromAny(b.BlockTypes[name], m[name])
			if err != nil {
				errs = append(errs, prefixed(fmt.Sprintf("block %q", name), err)...)
				continue
			}
			vals[name] = v
			continue
		}
		raw, ok := m[name]
		if cv, isCty := raw.(cty.Value); isCty && cv.IsNull() {
			raw = nil
		}
		if !ok || raw == nil {
			vals[name] = cty.NullVal(a.Type)
			continue
		}
		if a.Computed && !a.Optional && !a.Required {
			errs = append(errs, fmt.Errorf("attribute %q is computed and cannot be set", name))
			continue
		}
		v, err := toValue(raw, a.Type)
		if err != nil {
			errs = append(errs, fmt.Errorf("attribute %q: %w", name, err))
			continue
		}
		vals[name] = v
	}
	if len(errs) > 0 {
		return cty.NilVal, errors.Join(errs...)
	}
	return cty.ObjectVal(vals), nil
}

// prefixed returns the errors joined in err, each prefixed with prefix.
func prefixed(prefix string, err error) []error {
	var out []error
	for _, e := range flatten(err) {
		out = append(out, fmt.Errorf("%s: %w", prefix, e))
	}
	return out
}

// flatten returns the errors joined in err (errors.Join), or err alone.
func flatten(err error) []error {
	if j, ok := err.(interface{ Unwrap() []error }); ok {
		var out []error
		for _, e := range j.Unwrap() {
			out = append(out, flatten(e)...)
		}
		return out
	}
	return []error{err}
}

// maxSuggestions is how many names an unknown-name error suggests.
const maxSuggestions = 3

// unknownName is the error for key k, which b does not have, with up to
// maxSuggestions of b's names as suggestions: those within an edit distance
// of a third of k's length (at least 1), or that contain k or are contained
// in it, closest first.
func unknownName(b *Block, k string) error {
	type cand struct {
		name string
		dist int
	}
	var cands []cand
	limit := max(1, len(k)/3)
	for _, n := range append(sortedKeys(b.Attributes), sortedKeys(b.BlockTypes)...) {
		d := editDistance(k, n)
		contains := len(k) >= 3 && len(n) >= 3 && (strings.Contains(n, k) || strings.Contains(k, n))
		if d <= limit || contains {
			cands = append(cands, cand{n, d})
		}
	}
	slices.SortFunc(cands, func(a, b cand) int {
		if a.dist != b.dist {
			return a.dist - b.dist
		}
		return strings.Compare(a.name, b.name)
	})
	if len(cands) > maxSuggestions {
		cands = cands[:maxSuggestions]
	}
	msg := fmt.Sprintf("unknown attribute or block %q", k)
	if len(cands) == 0 {
		return errors.New(msg)
	}
	quoted := make([]string, len(cands))
	for i, c := range cands {
		quoted[i] = strconv.Quote(c.name)
	}
	list := quoted[0]
	if n := len(quoted); n > 1 {
		list = strings.Join(quoted[:n-1], ", ") + " or " + quoted[n-1]
	}
	return fmt.Errorf("%s; did you mean %s?", msg, list)
}

// editDistance is the optimal string alignment distance of a and b:
// insertions, deletions, substitutions and transpositions of adjacent bytes.
func editDistance(a, b string) int {
	d := make([][]int, len(a)+1)
	for i := range d {
		d[i] = make([]int, len(b)+1)
		d[i][0] = i
	}
	for j := range d[0] {
		d[0][j] = j
	}
	for i := 1; i <= len(a); i++ {
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			d[i][j] = min(d[i-1][j]+1, d[i][j-1]+1, d[i-1][j-1]+cost)
			if i > 1 && j > 1 && a[i-1] == b[j-2] && a[i-2] == b[j-1] {
				d[i][j] = min(d[i][j], d[i-2][j-2]+1)
			}
		}
	}
	return d[len(a)][len(b)]
}

// JSONKind names the JSON kind of a configuration value, for messages read
// by the planner: object, list, string, number, bool or null ("unknown
// value" for a wholly unknown cty value).
func JSONKind(v any) string {
	if v == nil {
		return "null"
	}
	if cv, ok := v.(cty.Value); ok {
		switch ty := cv.Type(); {
		case cv == cty.NilVal || cv.IsNull():
			return "null"
		case ty == cty.DynamicPseudoType:
			return "unknown value"
		case ty == cty.String:
			return "string"
		case ty == cty.Number:
			return "number"
		case ty == cty.Bool:
			return "bool"
		case ty.IsObjectType() || ty.IsMapType():
			return "object"
		case ty.IsListType() || ty.IsSetType() || ty.IsTupleType():
			return "list"
		}
		return "value"
	}
	if _, ok := v.(json.Number); ok {
		return "number"
	}
	switch reflect.ValueOf(v).Kind() {
	case reflect.String:
		return "string"
	case reflect.Bool:
		return "bool"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return "number"
	case reflect.Map:
		return "object"
	case reflect.Slice, reflect.Array:
		return "list"
	}
	return "value"
}

func blockFromAny(nb *NestedBlock, raw any) (cty.Value, error) {
	inner := nb.Block.ImpliedType()
	if cv, ok := raw.(cty.Value); ok && cv != cty.NilVal && !cv.IsNull() {
		want := "an object"
		switch nb.Nesting {
		case NestingList, NestingSet:
			want = "a list of objects"
		case NestingMap:
			want = "an object of objects"
		}
		return cty.NilVal, fmt.Errorf("expected %s, got a reference", want)
	}
	switch nb.Nesting {
	case NestingSingle, NestingGroup:
		if raw == nil {
			if nb.Nesting == NestingSingle {
				return cty.NullVal(inner), nil
			}
			return ObjectFromMap(nb.Block, map[string]any{})
		}
		m, ok := raw.(map[string]any)
		if !ok {
			return cty.NilVal, fmt.Errorf("expected an object, got %s", JSONKind(raw))
		}
		return ObjectFromMap(nb.Block, m)
	case NestingList, NestingSet:
		var elems []cty.Value
		var errs []error
		if raw != nil {
			items, ok := raw.([]any)
			if !ok {
				return cty.NilVal, fmt.Errorf("expected a list of objects, got %s", JSONKind(raw))
			}
			for i, it := range items {
				m, ok := it.(map[string]any)
				if !ok {
					errs = append(errs, fmt.Errorf("item %d: expected an object, got %s", i, JSONKind(it)))
					continue
				}
				v, err := ObjectFromMap(nb.Block, m)
				if err != nil {
					errs = append(errs, prefixed(fmt.Sprintf("item %d", i), err)...)
					continue
				}
				elems = append(elems, v)
			}
		}
		if len(errs) > 0 {
			return cty.NilVal, errors.Join(errs...)
		}
		if len(elems) == 0 {
			if nb.Nesting == NestingList {
				return cty.ListValEmpty(inner), nil
			}
			return cty.SetValEmpty(inner), nil
		}
		if nb.Nesting == NestingList {
			return listVal(elems)
		}
		return setVal(elems)
	case NestingMap:
		elems := map[string]cty.Value{}
		var errs []error
		if raw != nil {
			items, ok := raw.(map[string]any)
			if !ok {
				return cty.NilVal, fmt.Errorf("expected an object of objects, got %s", JSONKind(raw))
			}
			for _, k := range sortedKeys(items) {
				it := items[k]
				m, ok := it.(map[string]any)
				if !ok {
					errs = append(errs, fmt.Errorf("entry %q: expected an object, got %s", k, JSONKind(it)))
					continue
				}
				v, err := ObjectFromMap(nb.Block, m)
				if err != nil {
					errs = append(errs, prefixed(fmt.Sprintf("entry %q", k), err)...)
					continue
				}
				elems[k] = v
			}
		}
		if len(errs) > 0 {
			return cty.NilVal, errors.Join(errs...)
		}
		if len(elems) == 0 {
			return cty.MapValEmpty(inner), nil
		}
		return mapVal(elems)
	}
	return cty.NilVal, fmt.Errorf("unsupported nesting mode %d", nb.Nesting)
}

// toValue converts a JSON-like Go value, or a cty.Value, to a cty value of
// type ty.
func toValue(raw any, ty cty.Type) (cty.Value, error) {
	if raw == nil {
		return cty.NullVal(ty), nil
	}
	if cv, ok := raw.(cty.Value); ok {
		return ctyLeaf(cv, ty)
	}
	switch {
	case ty == cty.DynamicPseudoType:
		return dynamicValue(raw)
	case ty == cty.String:
		s, ok := raw.(string)
		if !ok {
			return cty.NilVal, fmt.Errorf("expected a string, got %s", JSONKind(raw))
		}
		return cty.StringVal(s), nil
	case ty == cty.Bool:
		b, ok := raw.(bool)
		if !ok {
			return cty.NilVal, fmt.Errorf("expected a bool, got %s", JSONKind(raw))
		}
		return cty.BoolVal(b), nil
	case ty == cty.Number:
		return numberValue(raw)
	case ty.IsListType(), ty.IsSetType(), ty.IsTupleType():
		items, err := asSlice(raw)
		if err != nil {
			return cty.NilVal, err
		}
		if ty.IsTupleType() {
			tys := ty.TupleElementTypes()
			if len(tys) != len(items) {
				return cty.NilVal, fmt.Errorf("expected %d elements, got %d", len(tys), len(items))
			}
			vals := make([]cty.Value, len(items))
			for i, it := range items {
				v, err := toValue(it, tys[i])
				if err != nil {
					return cty.NilVal, fmt.Errorf("element %d: %w", i, err)
				}
				vals[i] = v
			}
			return cty.TupleVal(vals), nil
		}
		et := ty.ElementType()
		vals := make([]cty.Value, len(items))
		for i, it := range items {
			v, err := toValue(it, et)
			if err != nil {
				return cty.NilVal, fmt.Errorf("element %d: %w", i, err)
			}
			vals[i] = v
		}
		switch {
		case len(vals) == 0 && ty.IsListType():
			return cty.ListValEmpty(et), nil
		case len(vals) == 0:
			return cty.SetValEmpty(et), nil
		case ty.IsListType():
			return listVal(vals)
		}
		return setVal(vals)
	case ty.IsMapType(), ty.IsObjectType():
		items, err := asStringMap(raw)
		if err != nil {
			return cty.NilVal, err
		}
		if ty.IsObjectType() {
			ats := ty.AttributeTypes()
			vals := make(map[string]cty.Value, len(ats))
			for k := range items {
				if _, ok := ats[k]; !ok {
					return cty.NilVal, fmt.Errorf("unknown attribute %q", k)
				}
			}
			for _, k := range sortedKeys(ats) {
				v, err := toValue(items[k], ats[k])
				if err != nil {
					return cty.NilVal, fmt.Errorf("attribute %q: %w", k, err)
				}
				vals[k] = v
			}
			return cty.ObjectVal(vals), nil
		}
		et := ty.ElementType()
		vals := make(map[string]cty.Value, len(items))
		for _, k := range sortedKeys(items) {
			v, err := toValue(items[k], et)
			if err != nil {
				return cty.NilVal, fmt.Errorf("key %q: %w", k, err)
			}
			vals[k] = v
		}
		if len(vals) == 0 {
			return cty.MapValEmpty(et), nil
		}
		return mapVal(vals)
	}
	return cty.NilVal, fmt.Errorf("unsupported attribute type %s", ty.FriendlyName())
}

// ctyLeaf converts a caller-supplied cty value to ty: a wholly unknown
// dynamic value becomes an unknown of ty, other values (unknowns of a known
// type included) go through cty's convert rules.
func ctyLeaf(v cty.Value, ty cty.Type) (cty.Value, error) {
	if v == cty.NilVal || v.IsNull() {
		return cty.NullVal(ty), nil
	}
	if !v.IsKnown() && v.Type().Equals(cty.DynamicPseudoType) {
		return cty.UnknownVal(ty), nil
	}
	out, err := convert.Convert(v, ty)
	if err != nil && !v.IsKnown() {
		return cty.NilVal, fmt.Errorf("cannot use %s as %s", v.Type().FriendlyName(), ty.FriendlyName())
	}
	if err != nil {
		return cty.NilVal, fmt.Errorf("cannot use %s as %s: %w", v.Type().FriendlyName(), ty.FriendlyName(), err)
	}
	return out, nil
}

// listVal, setVal and mapVal build collections whose elements may have
// different types (an element type with dynamic parts): the types are unified
// as Terraform does, and an error, never a panic, reports elements that
// cannot share one type.
func listVal(vals []cty.Value) (cty.Value, error) {
	vals, err := unifyElems(vals)
	if err != nil {
		return cty.NilVal, err
	}
	return cty.ListVal(vals), nil
}

func setVal(vals []cty.Value) (cty.Value, error) {
	vals, err := unifyElems(vals)
	if err != nil {
		return cty.NilVal, err
	}
	return cty.SetVal(vals), nil
}

func mapVal(m map[string]cty.Value) (cty.Value, error) {
	keys := sortedKeys(m)
	vals := make([]cty.Value, len(keys))
	for i, k := range keys {
		vals[i] = m[k]
	}
	vals, err := unifyElems(vals)
	if err != nil {
		return cty.NilVal, err
	}
	out := make(map[string]cty.Value, len(keys))
	for i, k := range keys {
		out[k] = vals[i]
	}
	return cty.MapVal(out), nil
}

// unifyElems converts vals to a single common type. Wholly unknown dynamic
// elements (cty.DynamicVal) take the type of the others.
func unifyElems(vals []cty.Value) ([]cty.Value, error) {
	var tys []cty.Type
	for _, v := range vals {
		if !v.Type().Equals(cty.DynamicPseudoType) {
			tys = append(tys, v.Type())
		}
	}
	if len(tys) == 0 {
		return vals, nil
	}
	same := len(tys) == len(vals)
	for _, t := range tys[1:] {
		if !t.Equals(tys[0]) {
			same = false
			break
		}
	}
	if same {
		return vals, nil
	}
	ut, _ := convert.Unify(tys)
	if ut == cty.NilType || ut.Equals(cty.DynamicPseudoType) {
		return nil, fmt.Errorf("elements have inconsistent types (%s)", friendlyTypes(tys))
	}
	out := make([]cty.Value, len(vals))
	for i, v := range vals {
		cv, err := convert.Convert(v, ut)
		if err != nil {
			return nil, fmt.Errorf("elements have inconsistent types (%s): element %d: %w", friendlyTypes(tys), i, err)
		}
		out[i] = cv
	}
	return out, nil
}

func friendlyTypes(tys []cty.Type) string {
	names := make([]string, len(tys))
	for i, t := range tys {
		names[i] = t.FriendlyName()
	}
	return strings.Join(names, ", ")
}

func numberValue(raw any) (cty.Value, error) {
	switch n := raw.(type) {
	case int:
		return cty.NumberIntVal(int64(n)), nil
	case int8:
		return cty.NumberIntVal(int64(n)), nil
	case int16:
		return cty.NumberIntVal(int64(n)), nil
	case int32:
		return cty.NumberIntVal(int64(n)), nil
	case int64:
		return cty.NumberIntVal(n), nil
	case uint:
		return cty.NumberUIntVal(uint64(n)), nil
	case uint32:
		return cty.NumberUIntVal(uint64(n)), nil
	case uint64:
		return cty.NumberUIntVal(n), nil
	case float32:
		return cty.NumberFloatVal(float64(n)), nil
	case float64:
		return cty.NumberFloatVal(n), nil
	case json.Number:
		f, _, err := big.ParseFloat(n.String(), 10, 512, big.ToNearestEven)
		if err != nil {
			return cty.NilVal, fmt.Errorf("invalid number %q: %w", n.String(), err)
		}
		return cty.NumberVal(f), nil
	}
	return cty.NilVal, fmt.Errorf("expected a number, got %s", JSONKind(raw))
}

func asSlice(raw any) ([]any, error) {
	if s, ok := raw.([]any); ok {
		return s, nil
	}
	rv := reflect.ValueOf(raw)
	if rv.Kind() != reflect.Slice {
		return nil, fmt.Errorf("expected a list, got %s", JSONKind(raw))
	}
	out := make([]any, rv.Len())
	for i := range out {
		out[i] = rv.Index(i).Interface()
	}
	return out, nil
}

func asStringMap(raw any) (map[string]any, error) {
	if m, ok := raw.(map[string]any); ok {
		return m, nil
	}
	rv := reflect.ValueOf(raw)
	if rv.Kind() != reflect.Map || rv.Type().Key().Kind() != reflect.String {
		return nil, fmt.Errorf("expected an object, got %s", JSONKind(raw))
	}
	out := make(map[string]any, rv.Len())
	for _, k := range rv.MapKeys() {
		out[k.String()] = rv.MapIndex(k).Interface()
	}
	return out, nil
}

// dynamicValue converts a value whose type is only known at runtime.
func dynamicValue(raw any) (cty.Value, error) {
	buf, err := json.Marshal(raw)
	if err != nil {
		return cty.NilVal, err
	}
	ty, err := ctyjson.ImpliedType(buf)
	if err != nil {
		return cty.NilVal, err
	}
	return ctyjson.Unmarshal(buf, ty)
}

func sortedKeys[V any](m map[string]V) []string {
	return slices.Sorted(maps.Keys(m))
}

// Encode serializes v, of type ty, as a msgpack DynamicValue.
func Encode(v cty.Value, ty cty.Type) (*tfplugin5.DynamicValue, error) {
	buf, err := msgpack.Marshal(v, ty)
	if err != nil {
		return nil, fmt.Errorf("encode value: %w", err)
	}
	return &tfplugin5.DynamicValue{Msgpack: buf}, nil
}

// Decode reads a DynamicValue of type ty (msgpack, or JSON when msgpack is
// empty). A nil or empty value decodes to null.
func Decode(dv *tfplugin5.DynamicValue, ty cty.Type) (cty.Value, error) {
	switch {
	case dv == nil:
		return cty.NullVal(ty), nil
	case len(dv.Msgpack) > 0:
		v, err := msgpack.Unmarshal(dv.Msgpack, ty)
		if err != nil {
			return cty.NilVal, fmt.Errorf("decode msgpack value: %w", err)
		}
		return v, nil
	case len(dv.Json) > 0:
		v, err := ctyjson.Unmarshal(dv.Json, ty)
		if err != nil {
			return cty.NilVal, fmt.Errorf("decode json value: %w", err)
		}
		return v, nil
	}
	return cty.NullVal(ty), nil
}

// ProposedNew computes the planned state a provider receives as
// ProposedNewState, as Terraform does (objchange.ProposedNew): config values
// win, computed attributes absent from the config keep their prior value
// (null on create: the provider's plan marks them unknown), and every other
// absent attribute is null. A null config (destroy) yields null.
func ProposedNew(b *Block, prior, config cty.Value) cty.Value {
	if config.IsNull() || !config.IsKnown() {
		return config
	}
	if !prior.IsKnown() {
		prior = cty.NullVal(config.Type())
	}
	priorNull := prior.IsNull()
	out := make(map[string]cty.Value, len(b.Attributes)+len(b.BlockTypes))
	for name, a := range b.Attributes {
		c := config.GetAttr(name)
		switch {
		case !c.IsNull():
			out[name] = c
		case a.Computed && !priorNull:
			out[name] = prior.GetAttr(name)
		default:
			out[name] = c
		}
	}
	for name, nb := range b.BlockTypes {
		c := config.GetAttr(name)
		p := cty.NullVal(c.Type())
		if !priorNull {
			p = prior.GetAttr(name)
		}
		out[name] = proposedBlock(nb, p, c)
	}
	return cty.ObjectVal(out)
}

func proposedBlock(nb *NestedBlock, prior, config cty.Value) cty.Value {
	switch nb.Nesting {
	case NestingSingle, NestingGroup:
		if config.IsNull() {
			return config
		}
		return ProposedNew(nb.Block, prior, config)
	case NestingList:
		if config.IsNull() || !config.IsKnown() || config.LengthInt() == 0 {
			return config
		}
		var priorElems []cty.Value
		if !prior.IsNull() && prior.IsKnown() {
			priorElems = prior.AsValueSlice()
		}
		elems := config.AsValueSlice()
		for i, c := range elems {
			if i < len(priorElems) {
				elems[i] = ProposedNew(nb.Block, priorElems[i], c)
			} else {
				elems[i] = ProposedNew(nb.Block, cty.NullVal(c.Type()), c)
			}
		}
		return cty.ListVal(elems)
	}
	return config
}

// NormalizeBlocks returns v, an object of block b, with every null list, set
// or map block, at any depth, replaced by an empty collection, as Terraform
// does with the states of SDKv2 providers (objchange.NormalizeObjectFromLegacySDK):
// such providers return null for a block they have no element of, and take
// the null back, as a prior state, for a change. Unknown values, null
// objects and attributes are left alone.
func NormalizeBlocks(b *Block, v cty.Value) cty.Value {
	if v == cty.NilVal || v.IsNull() || !v.IsKnown() || !v.Type().IsObjectType() || len(b.BlockTypes) == 0 {
		return v
	}
	attrs := v.AsValueMap()
	for name, nb := range b.BlockTypes {
		bv, ok := attrs[name]
		if !ok || !bv.IsKnown() {
			continue
		}
		inner := nb.Block.ImpliedType()
		switch nb.Nesting {
		case NestingSingle, NestingGroup:
			attrs[name] = NormalizeBlocks(nb.Block, bv)
		case NestingList, NestingSet:
			if bv.IsNull() {
				if nb.Nesting == NestingList {
					attrs[name] = cty.ListValEmpty(inner)
				} else {
					attrs[name] = cty.SetValEmpty(inner)
				}
				continue
			}
			if bv.LengthInt() == 0 || !bv.Type().IsCollectionType() {
				continue
			}
			elems := bv.AsValueSlice()
			for i, e := range elems {
				elems[i] = NormalizeBlocks(nb.Block, e)
			}
			if nb.Nesting == NestingList {
				attrs[name] = cty.ListVal(elems)
			} else {
				attrs[name] = cty.SetVal(elems)
			}
		case NestingMap:
			if bv.IsNull() {
				if bv.Type().IsMapType() {
					attrs[name] = cty.MapValEmpty(inner)
				}
				continue
			}
			if bv.LengthInt() == 0 || !bv.Type().IsMapType() {
				continue
			}
			elems := bv.AsValueMap()
			for k, e := range elems {
				elems[k] = NormalizeBlocks(nb.Block, e)
			}
			attrs[name] = cty.MapVal(elems)
		}
	}
	return cty.ObjectVal(attrs)
}
