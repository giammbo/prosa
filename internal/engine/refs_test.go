package engine

import (
	"reflect"
	"strings"
	"testing"

	"github.com/zclconf/go-cty/cty"
)

func testKeysOf(intent string) []string {
	switch intent {
	case "postgres":
		return []string{"db", "db.instance"}
	case "n":
		return []string{"sg"}
	}
	return nil
}

func TestParseRefLongestKeyPrefix(t *testing.T) {
	r, ok, err := ParseRef("${postgres/db.instance.address}", testKeysOf)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if r.Intent != "postgres" || r.Key != "db.instance" || !reflect.DeepEqual(r.Attr, []string{"address"}) {
		t.Fatalf("got %+v", r)
	}
	if r.Node() != "postgres/db.instance" {
		t.Fatalf("node %q", r.Node())
	}
	r, _, err = ParseRef("${postgres/db.endpoint.port}", testKeysOf)
	if err != nil || r.Key != "db" || !reflect.DeepEqual(r.Attr, []string{"endpoint", "port"}) {
		t.Fatalf("got %+v err=%v", r, err)
	}
}

func TestParseRefPlainString(t *testing.T) {
	_, ok, err := ParseRef("hello", testKeysOf)
	if ok || err != nil {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
}

func TestParseRefUnknownKey(t *testing.T) {
	_, _, err := ParseRef("${postgres/nope.id}", testKeysOf)
	if err == nil || !strings.Contains(err.Error(), "postgres/nope") {
		t.Fatalf("err=%v", err)
	}
}

func TestParseRefUnknownIntentAndSyntax(t *testing.T) {
	cases := map[string]string{
		"${ghost/a.b}":      "ghost",
		"${postgres}":       "${postgres}",
		"${postgres/db}":    "${postgres/db}",
		"${postgres/db.}":   "${postgres/db.}",
		"${bad intent/a.b}": "bad intent",
	}
	for s, want := range cases {
		_, _, err := ParseRef(s, testKeysOf)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%q: err=%v", s, err)
		}
	}
}

func TestCollectRefsSortedUnique(t *testing.T) {
	cfg := map[string]any{
		"a": "${n/sg.id}",
		"b": []any{"${postgres/db.instance.address}", map[string]any{"x": "${n/sg.id}"}},
		"c": 3, "d": "plain",
	}
	refs, err := CollectRefs(cfg, testKeysOf)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range refs {
		got = append(got, r.Node()+"."+strings.Join(r.Attr, "."))
	}
	want := []string{"n/sg.id", "postgres/db.instance.address"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
	if _, err := CollectRefs(map[string]any{"a": []any{"${ghost/a.b}"}}, testKeysOf); err == nil {
		t.Fatal("expected error")
	}
}

func TestSubstituteNested(t *testing.T) {
	cfg := map[string]any{
		"ids": []any{"${n/sg.id}"},
		"m":   map[string]any{"k": "${n/sg.arn}"},
		"s":   "plain", "i": 4,
	}
	out, err := Substitute(cfg, testKeysOf, func(r Ref) (cty.Value, error) {
		return cty.StringVal(r.Node() + ":" + strings.Join(r.Attr, ".")), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	ids := out["ids"].([]any)
	if v, ok := ids[0].(cty.Value); !ok || v.AsString() != "n/sg:id" {
		t.Fatalf("ids %#v", ids)
	}
	m := out["m"].(map[string]any)
	if v, ok := m["k"].(cty.Value); !ok || v.AsString() != "n/sg:arn" {
		t.Fatalf("m %#v", m)
	}
	if out["s"] != "plain" || out["i"] != 4 {
		t.Fatalf("out %#v", out)
	}
	if cfg["ids"].([]any)[0] != "${n/sg.id}" || cfg["m"].(map[string]any)["k"] != "${n/sg.arn}" {
		t.Fatalf("input mutated: %#v", cfg)
	}
}

func TestSubstituteResolveError(t *testing.T) {
	_, err := Substitute(map[string]any{"a": "${n/sg.id}"}, testKeysOf, func(Ref) (cty.Value, error) {
		return cty.NilVal, errBoom
	})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestTypedContainers(t *testing.T) {
	cfg := map[string]any{
		"maps": []map[string]any{{"a": "${n/sg.id}"}},
		"strs": []string{"${n/sg.arn}", "x"},
		"smap": map[string]string{"k": "${n/sg.id}"},
	}
	refs, err := CollectRefs(cfg, testKeysOf)
	if err != nil || len(refs) != 2 {
		t.Fatalf("refs=%v err=%v", refs, err)
	}
	out, err := Substitute(cfg, testKeysOf, func(r Ref) (cty.Value, error) { return cty.StringVal("v"), nil })
	if err != nil {
		t.Fatal(err)
	}
	m := out["maps"].([]any)[0].(map[string]any)
	if _, ok := m["a"].(cty.Value); !ok {
		t.Fatalf("maps not substituted: %#v", out["maps"])
	}
	s := out["strs"].([]any)
	if _, ok := s[0].(cty.Value); !ok || s[1] != "x" {
		t.Fatalf("strs: %#v", s)
	}
	if _, ok := out["smap"].(map[string]any)["k"].(cty.Value); !ok {
		t.Fatalf("smap: %#v", out["smap"])
	}
	m["a"] = "mutated"
	s[1] = "mutated"
	if cfg["maps"].([]map[string]any)[0]["a"] != "${n/sg.id}" || cfg["strs"].([]string)[1] != "x" {
		t.Fatalf("input aliased: %#v", cfg)
	}
}

func TestNonStringMapKeysError(t *testing.T) {
	cfg := map[string]any{"bad": map[int]any{1: "x"}}
	if _, err := CollectRefs(cfg, testKeysOf); err == nil || !strings.Contains(err.Error(), "config.bad") {
		t.Fatalf("err=%v", err)
	}
	if _, err := Substitute(cfg, testKeysOf, nil); err == nil {
		t.Fatal("expected error")
	}
}

func TestDeterministicFirstError(t *testing.T) {
	cfg := map[string]any{"z": "${ghost/a.b}", "a": "${other/a.b}", "m": "${third/a.b}"}
	for i := 0; i < 30; i++ {
		_, err := CollectRefs(cfg, testKeysOf)
		if err == nil || !strings.Contains(err.Error(), "config.a") {
			t.Fatalf("collect: %v", err)
		}
		_, err = Substitute(cfg, testKeysOf, nil)
		if err == nil || !strings.Contains(err.Error(), "config.a") {
			t.Fatalf("substitute: %v", err)
		}
	}
}

func TestDollarEscapeIsLiteral(t *testing.T) {
	cfg := map[string]any{
		"policy": `{"Resource": "arn:aws:s3:::b/$${aws:username}/*"}`,
		"list":   []any{"$${x}", "a$$${y}"},
		"ref":    "${n/sg.id}",
	}
	refs, err := CollectRefs(cfg, testKeysOf)
	if err != nil || len(refs) != 1 || refs[0].String() != "${n/sg.id}" {
		t.Fatalf("refs = %v, err = %v; want only ${n/sg.id}", refs, err)
	}
	out, err := Substitute(cfg, testKeysOf, func(Ref) (cty.Value, error) { return cty.StringVal("sg-1"), nil })
	if err != nil {
		t.Fatal(err)
	}
	if got := out["policy"]; got != `{"Resource": "arn:aws:s3:::b/${aws:username}/*"}` {
		t.Errorf("policy = %q", got)
	}
	if got := out["list"]; !reflect.DeepEqual(got, []any{"${x}", "a$${y}"}) {
		t.Errorf("list = %#v", got)
	}
	if cfg["policy"] != `{"Resource": "arn:aws:s3:::b/$${aws:username}/*"}` {
		t.Error("Substitute modified its input")
	}
	for _, s := range []string{"$${x} ${n/sg.id}", "${n/sg.id}$${x}"} {
		if parts, ok, err := ParseTemplate(s, testKeysOf); !ok || err != nil || len(parts) != 2 {
			t.Errorf("%q: parts=%v ok=%v err=%v, want a template of two parts", s, parts, ok, err)
		}
	}
	if _, ok, err := ParseTemplate("a ${x", testKeysOf); !ok || err == nil {
		t.Errorf(`"a ${x": ok=%v err=%v, want an error`, ok, err)
	}
}
