package provider

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/zclconf/go-cty/cty"

	"github.com/giammbo/prosa/internal/tfplugin5"
)

func testBlock() *Block {
	return &Block{
		Attributes: map[string]*Attribute{
			"name":              {Type: cty.String, Required: true},
			"tags":              {Type: cty.Map(cty.String), Optional: true},
			"id":                {Type: cty.String, Computed: true},
			"allocated_storage": {Type: cty.Number, Optional: true},
			"note":              {Type: cty.String, Optional: true},
		},
		BlockTypes: map[string]*NestedBlock{
			"setting": {
				Nesting: NestingList,
				Block: &Block{Attributes: map[string]*Attribute{
					"name":  {Type: cty.String, Required: true},
					"value": {Type: cty.String, Optional: true},
				}},
			},
		},
	}
}

func TestObjectFromMapNullFill(t *testing.T) {
	b := testBlock()
	v, err := ObjectFromMap(b, map[string]any{"name": "x"})
	if err != nil {
		t.Fatal(err)
	}
	if !v.Type().Equals(b.ImpliedType()) {
		t.Fatalf("type = %#v, want %#v", v.Type(), b.ImpliedType())
	}
	if got := v.GetAttr("name"); !got.RawEquals(cty.StringVal("x")) {
		t.Errorf("name = %#v", got)
	}
	if !v.GetAttr("tags").IsNull() || !v.GetAttr("id").IsNull() {
		t.Errorf("tags/id should be null")
	}
	if s := v.GetAttr("setting"); s.IsNull() || s.LengthInt() != 0 {
		t.Errorf("setting should be an empty list, got %#v", s)
	}
}

func TestObjectFromMapUnknownKey(t *testing.T) {
	_, err := ObjectFromMap(testBlock(), map[string]any{"nmae": "x"})
	if err == nil || !strings.Contains(err.Error(), `"nmae"`) {
		t.Fatalf("err = %v, want mention of \"nmae\"", err)
	}
}

func TestObjectFromMapNumbers(t *testing.T) {
	b := testBlock()
	for _, in := range []any{10, int64(10), 10.0, json.Number("10")} {
		v, err := ObjectFromMap(b, map[string]any{"name": "x", "allocated_storage": in})
		if err != nil {
			t.Fatalf("%T: %v", in, err)
		}
		if got := v.GetAttr("allocated_storage"); !got.RawEquals(cty.NumberIntVal(10)) {
			t.Errorf("%T: got %#v", in, got)
		}
	}
	if _, err := ObjectFromMap(b, map[string]any{"name": "x", "allocated_storage": "10"}); err == nil {
		t.Error("string for number should fail")
	}
}

func TestObjectFromMapCollections(t *testing.T) {
	b := testBlock()
	v, err := ObjectFromMap(b, map[string]any{"name": "x", "tags": map[string]any{"a": "b"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := v.GetAttr("tags"); !got.RawEquals(cty.MapVal(map[string]cty.Value{"a": cty.StringVal("b")})) {
		t.Errorf("tags = %#v", got)
	}
	v, err = ObjectFromMap(b, map[string]any{"name": "x", "tags": map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if got := v.GetAttr("tags"); got.IsNull() || got.LengthInt() != 0 {
		t.Errorf("empty tags should be an empty map, got %#v", got)
	}
}

func TestObjectFromMapNestedBlocks(t *testing.T) {
	v, err := ObjectFromMap(testBlock(), map[string]any{
		"name":    "x",
		"setting": []any{map[string]any{"name": "a", "value": "b"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := v.GetAttr("setting")
	if s.LengthInt() != 1 {
		t.Fatalf("setting = %#v", s)
	}
	e := s.Index(cty.NumberIntVal(0))
	if !e.GetAttr("name").RawEquals(cty.StringVal("a")) || !e.GetAttr("value").RawEquals(cty.StringVal("b")) {
		t.Errorf("element = %#v", e)
	}
}

func TestObjectFromMapMapBlock(t *testing.T) {
	b := &Block{BlockTypes: map[string]*NestedBlock{
		"rule": {Nesting: NestingMap, Block: &Block{Attributes: map[string]*Attribute{
			"v": {Type: cty.String, Optional: true},
		}}},
	}}
	v, err := ObjectFromMap(b, map[string]any{"rule": map[string]any{"k": map[string]any{"v": "1"}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := v.GetAttr("rule").Index(cty.StringVal("k")).GetAttr("v"); !got.RawEquals(cty.StringVal("1")) {
		t.Errorf("got %#v", got)
	}
	v, err = ObjectFromMap(b, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if r := v.GetAttr("rule"); r.IsNull() || r.LengthInt() != 0 {
		t.Errorf("missing map block should be empty map, got %#v", r)
	}
}

func TestEncodeDecodeRoundTripUnknown(t *testing.T) {
	ty := cty.Object(map[string]cty.Type{"a": cty.String, "b": cty.String})
	in := cty.ObjectVal(map[string]cty.Value{"a": cty.StringVal("x"), "b": cty.UnknownVal(cty.String)})
	dv, err := Encode(in, ty)
	if err != nil {
		t.Fatal(err)
	}
	out, err := Decode(dv, ty)
	if err != nil {
		t.Fatal(err)
	}
	if !out.RawEquals(in) {
		t.Errorf("round trip: got %#v, want %#v", out, in)
	}
	if n, err := Decode(nil, ty); err != nil || !n.IsNull() {
		t.Errorf("Decode(nil) = %#v, %v", n, err)
	}
}

func TestDecodeJSONFallback(t *testing.T) {
	ty := cty.Object(map[string]cty.Type{"a": cty.String})
	out, err := Decode(&tfplugin5.DynamicValue{Json: []byte(`{"a":"x"}`)}, ty)
	if err != nil {
		t.Fatal(err)
	}
	if !out.GetAttr("a").RawEquals(cty.StringVal("x")) {
		t.Errorf("got %#v", out)
	}
}

func TestProposedNewCreate(t *testing.T) {
	b := testBlock()
	cfg, _ := ObjectFromMap(b, map[string]any{"name": "x"})
	got := ProposedNew(b, cty.NullVal(b.ImpliedType()), cfg)
	if !got.GetAttr("name").RawEquals(cty.StringVal("x")) {
		t.Errorf("name = %#v", got.GetAttr("name"))
	}
	// As in Terraform (objchange.ProposedNew), a computed attribute absent
	// from the config takes the prior value: null on create. The provider
	// marks it unknown in its plan; an unknown here reaches SDKv2 providers
	// as a placeholder string (aws_sns_topic's policy failed to apply).
	if id := got.GetAttr("id"); !id.IsKnown() || !id.IsNull() {
		t.Errorf("id = %#v, want null", id)
	}
}

func TestProposedNewUpdate(t *testing.T) {
	b := testBlock()
	prior := withAttr(t, b, map[string]any{"name": "x", "tags": map[string]any{"old": "1"}}, "id", cty.StringVal("abc"))
	cfg, _ := ObjectFromMap(b, map[string]any{"name": "x", "tags": map[string]any{"new": "2"}})
	got := ProposedNew(b, prior, cfg)
	if !got.GetAttr("id").RawEquals(cty.StringVal("abc")) {
		t.Errorf("id = %#v", got.GetAttr("id"))
	}
	want := cty.MapVal(map[string]cty.Value{"new": cty.StringVal("2")})
	if !got.GetAttr("tags").RawEquals(want) {
		t.Errorf("tags = %#v", got.GetAttr("tags"))
	}
}

func TestProposedNewOptionalNotComputed(t *testing.T) {
	b := testBlock()
	prior, _ := ObjectFromMap(b, map[string]any{"name": "x", "note": "old"})
	cfg, _ := ObjectFromMap(b, map[string]any{"name": "x"})
	got := ProposedNew(b, prior, cfg)
	if !got.GetAttr("note").IsNull() {
		t.Errorf("note = %#v, want null", got.GetAttr("note"))
	}
}

func TestProposedNewDestroy(t *testing.T) {
	b := testBlock()
	prior, _ := ObjectFromMap(b, map[string]any{"name": "x"})
	got := ProposedNew(b, prior, cty.NullVal(b.ImpliedType()))
	if !got.IsNull() {
		t.Errorf("got %#v, want null", got)
	}
}

func TestProposedNewListBlockRecurses(t *testing.T) {
	b := &Block{BlockTypes: map[string]*NestedBlock{
		"item": {Nesting: NestingList, Block: &Block{Attributes: map[string]*Attribute{
			"n":  {Type: cty.String, Required: true},
			"id": {Type: cty.String, Computed: true},
		}}},
	}}
	itemTy := b.BlockTypes["item"].Block.ImpliedType()
	prior := cty.ObjectVal(map[string]cty.Value{"item": cty.ListVal([]cty.Value{
		cty.ObjectVal(map[string]cty.Value{"n": cty.StringVal("a"), "id": cty.StringVal("i0")}),
	})})
	if !prior.Type().AttributeType("item").ElementType().Equals(itemTy) {
		t.Fatalf("prior item type = %#v", prior.Type())
	}
	cfg, _ := ObjectFromMap(b, map[string]any{"item": []any{
		map[string]any{"n": "a"}, map[string]any{"n": "b"},
	}})
	got := ProposedNew(b, prior, cfg).GetAttr("item")
	if id := got.Index(cty.NumberIntVal(0)).GetAttr("id"); !id.RawEquals(cty.StringVal("i0")) {
		t.Errorf("item[0].id = %#v", id)
	}
	if id := got.Index(cty.NumberIntVal(1)).GetAttr("id"); !id.IsKnown() || !id.IsNull() {
		t.Errorf("item[1].id = %#v, want null", id)
	}
}

func TestProposedNewCreateSingleBlockComputedNull(t *testing.T) {
	b := &Block{BlockTypes: map[string]*NestedBlock{
		"opt": {Nesting: NestingSingle, Block: &Block{Attributes: map[string]*Attribute{
			"name": {Type: cty.String, Optional: true},
			"id":   {Type: cty.String, Computed: true},
		}}},
	}}
	cfg, err := ObjectFromMap(b, map[string]any{"opt": map[string]any{"name": "x"}})
	if err != nil {
		t.Fatal(err)
	}
	got := ProposedNew(b, cty.NullVal(b.ImpliedType()), cfg).GetAttr("opt")
	if !got.GetAttr("name").RawEquals(cty.StringVal("x")) {
		t.Errorf("name = %#v", got.GetAttr("name"))
	}
	if id := got.GetAttr("id"); !id.IsKnown() || !id.IsNull() {
		t.Errorf("id = %#v, want null", id)
	}
}

// withAttr builds a value from m with ObjectFromMap and then sets name to v,
// the way a provider fills a computed attribute in a state.
func withAttr(t *testing.T, b *Block, m map[string]any, name string, v cty.Value) cty.Value {
	t.Helper()
	obj, err := ObjectFromMap(b, m)
	if err != nil {
		t.Fatal(err)
	}
	attrs := obj.AsValueMap()
	attrs[name] = v
	return cty.ObjectVal(attrs)
}

func TestObjectFromMapCtyLeaves(t *testing.T) {
	b := testBlock()
	v, err := ObjectFromMap(b, map[string]any{"name": "x", "allocated_storage": cty.UnknownVal(cty.String)})
	if err != nil {
		t.Fatal(err)
	}
	if got := v.GetAttr("allocated_storage"); !got.RawEquals(cty.UnknownVal(cty.Number)) {
		t.Errorf("unknown string into number = %#v, want unknown number", got)
	}

	// cty inputs follow convert's rules: a numeric string converts.
	v, err = ObjectFromMap(b, map[string]any{"name": cty.StringVal("x"), "allocated_storage": cty.StringVal("10")})
	if err != nil {
		t.Fatal(err)
	}
	if got := v.GetAttr("allocated_storage"); !got.RawEquals(cty.NumberIntVal(10)) {
		t.Errorf("cty string into number = %#v, want 10", got)
	}

	// Elements of collections can be cty values too.
	v, err = ObjectFromMap(b, map[string]any{"name": "x", "tags": map[string]any{"a": cty.UnknownVal(cty.String), "b": "c"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := v.GetAttr("tags").Index(cty.StringVal("a")); got.IsKnown() {
		t.Errorf(`tags["a"] = %#v, want unknown`, got)
	}

	_, err = ObjectFromMap(b, map[string]any{"name": "x", "allocated_storage": cty.BoolVal(true)})
	if err == nil || !strings.Contains(err.Error(), `"allocated_storage"`) {
		t.Errorf("bool into number: err = %v, want an error naming allocated_storage", err)
	}
	_, err = ObjectFromMap(b, map[string]any{"name": "x", "tags": map[string]any{"a": cty.ListValEmpty(cty.String)}})
	if err == nil || !strings.Contains(err.Error(), `"tags"`) || !strings.Contains(err.Error(), `"a"`) {
		t.Errorf("list into map element: err = %v, want an error naming tags and a", err)
	}
}

func TestObjectFromMapRejectsComputedOnly(t *testing.T) {
	_, err := ObjectFromMap(testBlock(), map[string]any{"name": "x", "id": "abc"})
	if err == nil || err.Error() != `attribute "id" is computed and cannot be set` {
		t.Fatalf("err = %v", err)
	}
	b := &Block{Attributes: map[string]*Attribute{"arn": {Type: cty.String, Optional: true, Computed: true}}}
	if _, err := ObjectFromMap(b, map[string]any{"arn": "x"}); err != nil {
		t.Fatalf("optional+computed must be settable: %v", err)
	}
}

func TestObjectFromMapMixedDynamicElements(t *testing.T) {
	b := &Block{Attributes: map[string]*Attribute{
		"list": {Type: cty.List(cty.DynamicPseudoType), Optional: true},
		"set":  {Type: cty.Set(cty.DynamicPseudoType), Optional: true},
		"map":  {Type: cty.Map(cty.DynamicPseudoType), Optional: true},
	}}
	for _, m := range []map[string]any{
		{"list": []any{true, 1.0}},
		{"set": []any{map[string]any{"a": 1.0}, "x"}},
		{"map": map[string]any{"a": true, "b": 1.0}},
	} {
		_, err := ObjectFromMap(b, m)
		if err == nil || !strings.Contains(err.Error(), "inconsistent") {
			t.Errorf("%v: err = %v, want an inconsistent-types error", m, err)
		}
	}
	v, err := ObjectFromMap(b, map[string]any{"list": []any{"a", 1.0}})
	if err != nil {
		t.Fatal(err)
	}
	want := cty.ListVal([]cty.Value{cty.StringVal("a"), cty.StringVal("1")})
	if got := v.GetAttr("list"); !got.RawEquals(want) {
		t.Errorf("unifiable elements: got %#v, want %#v", got, want)
	}
}

func TestNormalizeBlocks(t *testing.T) {
	inner := &Block{Attributes: map[string]*Attribute{"n": {Type: cty.String, Optional: true}}}
	b := &Block{
		Attributes: map[string]*Attribute{"a": {Type: cty.List(cty.String), Optional: true}},
		BlockTypes: map[string]*NestedBlock{
			"list":   {Nesting: NestingList, Block: &Block{BlockTypes: map[string]*NestedBlock{"sub": {Nesting: NestingSet, Block: inner}}}},
			"set":    {Nesting: NestingSet, Block: inner},
			"map":    {Nesting: NestingMap, Block: inner},
			"single": {Nesting: NestingSingle, Block: &Block{BlockTypes: map[string]*NestedBlock{"sub": {Nesting: NestingList, Block: inner}}}},
			"gone":   {Nesting: NestingSingle, Block: inner},
			"later":  {Nesting: NestingList, Block: inner},
		},
	}
	ty := b.ImpliedType()
	listElem := ty.AttributeType("list").ElementType()
	single := ty.AttributeType("single")
	in := cty.ObjectVal(map[string]cty.Value{
		"a":      cty.NullVal(cty.List(cty.String)),
		"list":   cty.ListVal([]cty.Value{cty.ObjectVal(map[string]cty.Value{"sub": cty.NullVal(listElem.AttributeType("sub"))})}),
		"set":    cty.NullVal(ty.AttributeType("set")),
		"map":    cty.NullVal(ty.AttributeType("map")),
		"single": cty.ObjectVal(map[string]cty.Value{"sub": cty.NullVal(single.AttributeType("sub"))}),
		"gone":   cty.NullVal(ty.AttributeType("gone")),
		"later":  cty.UnknownVal(ty.AttributeType("later")),
	})
	got := NormalizeBlocks(b, in)
	want := cty.ObjectVal(map[string]cty.Value{
		"a":      cty.NullVal(cty.List(cty.String)),
		"list":   cty.ListVal([]cty.Value{cty.ObjectVal(map[string]cty.Value{"sub": cty.SetValEmpty(inner.ImpliedType())})}),
		"set":    cty.SetValEmpty(inner.ImpliedType()),
		"map":    cty.MapValEmpty(inner.ImpliedType()),
		"single": cty.ObjectVal(map[string]cty.Value{"sub": cty.ListValEmpty(inner.ImpliedType())}),
		"gone":   cty.NullVal(ty.AttributeType("gone")),
		"later":  cty.UnknownVal(ty.AttributeType("later")),
	})
	if !got.RawEquals(want) {
		t.Errorf("NormalizeBlocks =\n%#v\nwant\n%#v", got, want)
	}
	if null := cty.NullVal(ty); !NormalizeBlocks(b, null).RawEquals(null) {
		t.Error("a null object must stay null")
	}
}

// joined returns the messages of the errors joined in err, in order.
func joined(t *testing.T, err error) []string {
	t.Helper()
	if err == nil {
		t.Fatal("err = nil, want joined errors")
	}
	j, ok := err.(interface{ Unwrap() []error })
	if !ok {
		return []string{err.Error()}
	}
	var out []string
	for _, e := range j.Unwrap() {
		out = append(out, e.Error())
	}
	return out
}

func TestObjectFromMapCollectsAllProblems(t *testing.T) {
	m := map[string]any{"name": "x", "nmae": "y", "id": "abc", "note": json.Number("5"), "allocated_storage": "bad", "tags": "nope", "setting": json.Number("1")}
	want := []string{
		`unknown attribute or block "nmae"; did you mean "name"?`,
		`attribute "allocated_storage": expected a number, got string`,
		`attribute "id" is computed and cannot be set`,
		`attribute "note": expected a string, got number`,
		`block "setting": expected a list of objects, got number`,
		`attribute "tags": expected an object, got string`,
	}
	for i := 0; i < 50; i++ {
		_, err := ObjectFromMap(testBlock(), m)
		if got := joined(t, err); strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Fatalf("run %d: problems =\n%s\nwant\n%s", i, strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
	}

	// Nested blocks: every item, with its path.
	_, err := ObjectFromMap(testBlock(), map[string]any{"name": "x", "setting": []any{
		map[string]any{"nme": "a", "value": true},
		"plain",
		map[string]any{"name": json.Number("1")},
	}})
	want = []string{
		`block "setting": item 0: unknown attribute or block "nme"; did you mean "name"?`,
		`block "setting": item 0: attribute "value": expected a string, got bool`,
		`block "setting": item 1: expected an object, got string`,
		`block "setting": item 2: attribute "name": expected a string, got number`,
	}
	if got := joined(t, err); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("nested problems =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestObjectFromMapSuggestsNames(t *testing.T) {
	b := &Block{Attributes: map[string]*Attribute{
		"message_retention_seconds": {Type: cty.Number, Optional: true},
		"delay_seconds":             {Type: cty.Number, Optional: true},
		"name":                      {Type: cty.String, Optional: true},
		"name_prefix":               {Type: cty.String, Optional: true},
		"fifo_queue":                {Type: cty.Bool, Optional: true},
	}, BlockTypes: map[string]*NestedBlock{
		"health_check": {Nesting: NestingList, Block: &Block{}},
	}}
	for in, want := range map[string]string{
		"retention":   `unknown attribute or block "retention"; did you mean "message_retention_seconds"?`,
		"healthcheck": `unknown attribute or block "healthcheck"; did you mean "health_check"?`,
		"nam":         `unknown attribute or block "nam"; did you mean "name" or "name_prefix"?`,
		"seconds":     `unknown attribute or block "seconds"; did you mean "delay_seconds" or "message_retention_seconds"?`,
		"zzzz":        `unknown attribute or block "zzzz"`,
	} {
		_, err := ObjectFromMap(b, map[string]any{in: "x"})
		if got := joined(t, err); len(got) != 1 || got[0] != want {
			t.Errorf("%s: problems = %q, want %q", in, got, want)
		}
	}
}

func TestObjectFromMapTypedUnknowns(t *testing.T) {
	b := testBlock()
	if _, err := ObjectFromMap(b, map[string]any{"name": cty.DynamicVal, "tags": cty.UnknownVal(cty.Map(cty.String))}); err != nil {
		t.Fatalf("matching unknowns: %v", err)
	}
	_, err := ObjectFromMap(b, map[string]any{"name": cty.UnknownVal(cty.List(cty.String)), "tags": cty.UnknownVal(cty.String)})
	got := joined(t, err)
	if len(got) != 2 || !strings.HasPrefix(got[0], `attribute "name": cannot use list of string as string`) ||
		!strings.HasPrefix(got[1], `attribute "tags": cannot use string as map of string`) {
		t.Errorf("mismatched unknowns: problems = %q", got)
	}
}

func TestJSONKind(t *testing.T) {
	for _, c := range []struct {
		v    any
		want string
	}{
		{nil, "null"}, {"s", "string"}, {true, "bool"}, {json.Number("1"), "number"}, {1.5, "number"}, {3, "number"},
		{map[string]any{}, "object"}, {[]any{}, "list"}, {[]string{"a"}, "list"}, {map[string]string{}, "object"},
		{cty.StringVal("x"), "string"}, {cty.NumberIntVal(1), "number"}, {cty.True, "bool"},
		{cty.ListValEmpty(cty.String), "list"}, {cty.EmptyObjectVal, "object"}, {cty.MapValEmpty(cty.String), "object"},
		{cty.NullVal(cty.String), "null"}, {cty.DynamicVal, "unknown value"},
	} {
		if got := JSONKind(c.v); got != c.want {
			t.Errorf("JSONKind(%#v) = %q, want %q", c.v, got, c.want)
		}
	}
}

func TestObjectFromMapReferenceForABlock(t *testing.T) {
	b := testBlock()
	b.BlockTypes["rule"] = &NestedBlock{Nesting: NestingMap, Block: &Block{Attributes: map[string]*Attribute{"x": {Type: cty.String, Optional: true}}}}
	b.BlockTypes["one"] = &NestedBlock{Nesting: NestingSingle, Block: &Block{Attributes: map[string]*Attribute{"x": {Type: cty.String, Optional: true}}}}
	ref := cty.UnknownVal(cty.List(cty.String))
	for name, want := range map[string]string{
		"setting": `block "setting": expected a list of objects, got a reference`,
		"rule":    `block "rule": expected an object of objects, got a reference`,
		"one":     `block "one": expected an object, got a reference`,
	} {
		_, err := ObjectFromMap(b, map[string]any{"name": "x", name: ref})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", name, err, want)
		}
	}
}
