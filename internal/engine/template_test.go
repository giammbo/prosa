package engine

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/zclconf/go-cty/cty"
)

func TestParseTemplate(t *testing.T) {
	parts, ok, err := ParseTemplate("service/${postgres/db.instance.name}/${n/sg.id}", testKeysOf)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	want := []TemplatePart{
		{Text: "service/"},
		{Ref: &Ref{Intent: "postgres", Key: "db.instance", Attr: []string{"name"}}},
		{Text: "/"},
		{Ref: &Ref{Intent: "n", Key: "sg", Attr: []string{"id"}}},
	}
	if !reflect.DeepEqual(parts, want) {
		t.Fatalf("parts = %#v, want %#v", parts, want)
	}

	// An escape is literal text; two adjacent references are a template.
	parts, ok, err = ParseTemplate("$${x}-${n/sg.id}${n/sg.arn}", testKeysOf)
	if err != nil || !ok || len(parts) != 3 || parts[0].Text != "${x}-" || parts[1].Ref == nil || parts[2].Ref == nil {
		t.Fatalf("parts = %#v ok=%v err=%v", parts, ok, err)
	}

	// Not templates: a whole reference, a literal, an escape only.
	for _, s := range []string{"${n/sg.id}", "plain", "$${x}"} {
		if parts, ok, err := ParseTemplate(s, testKeysOf); ok || err != nil || parts != nil {
			t.Errorf("%q: parts=%v ok=%v err=%v, want not a template", s, parts, ok, err)
		}
	}

	// Bad templates are errors naming the template and the reference.
	cases := map[string]string{
		"a ${x":           `unclosed "${"`,
		"x-${ghost/a.b}":  `template "x-${ghost/a.b}": reference "${ghost/a.b}": unknown intent "ghost"`,
		"x-${n/nope.id}":  "unknown key n/nope",
		"x-${n/sg}":       "at least one attribute",
		"${n/sg.id}-${n}": `reference "${n}"`,
	}
	for s, want := range cases {
		_, ok, err := ParseTemplate(s, testKeysOf)
		if !ok || err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: ok=%v err=%v, want an error containing %q", s, ok, err, want)
		}
	}
}

func TestParseRefLeavesTemplates(t *testing.T) {
	for _, s := range []string{"x-${n/sg.id}", "${n/sg.id}-x", "${n/sg.id}${n/sg.arn}"} {
		if _, ok, err := ParseRef(s, testKeysOf); ok || err != nil {
			t.Errorf("%q: ok=%v err=%v, want not a whole reference", s, ok, err)
		}
	}
}

func TestCollectRefsIncludesTemplates(t *testing.T) {
	cfg := map[string]any{
		"a": "svc/${postgres/db.instance.name}/${n/sg.id}",
		"b": []any{map[string]any{"doc": "${n/sg.id}"}},
	}
	refs, err := CollectRefs(cfg, testKeysOf)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range refs {
		got = append(got, r.String())
	}
	if want := []string{"${n/sg.id}", "${postgres/db.instance.name}"}; !equalStrings(got, want) {
		t.Fatalf("refs = %v, want %v", got, want)
	}
	if _, err := CollectRefs(map[string]any{"a": "x-${ghost/a.b}"}, testKeysOf); err == nil || !strings.Contains(err.Error(), "config.a") {
		t.Fatalf("err = %v, want one naming config.a", err)
	}
}

func TestSubstituteTemplate(t *testing.T) {
	vals := map[string]cty.Value{
		"${postgres/db.instance.name}": cty.StringVal("main"),
		"${n/sg.id}":                   cty.NumberIntVal(5),
		"${n/sg.arn}":                  cty.True,
		"${n/sg.ratio}":                cty.NumberFloatVal(0.25),
	}
	resolve := func(r Ref) (cty.Value, error) { return vals[r.String()], nil }
	cfg := map[string]any{
		"two":    "service/${postgres/db.instance.name}/${n/sg.id}",
		"kinds":  "${n/sg.arn}-${n/sg.ratio}",
		"escape": "$${literal}/${n/sg.id}",
	}
	out, err := Substitute(cfg, testKeysOf, resolve)
	if err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{"two": "service/main/5", "kinds": "true-0.25", "escape": "${literal}/5"} {
		v, ok := out[k].(cty.Value)
		if !ok || !v.Type().Equals(cty.String) || !v.IsKnown() || v.AsString() != want {
			t.Errorf("%s = %#v, want %q", k, out[k], want)
		}
	}
	if cfg["two"] != "service/${postgres/db.instance.name}/${n/sg.id}" {
		t.Error("Substitute modified its input")
	}

	// An unknown part makes the whole string unknown.
	vals["${n/sg.id}"] = cty.UnknownVal(cty.Number)
	out, err = Substitute(map[string]any{"a": "x-${n/sg.id}", "b": "y-${n/sg.arn}"}, testKeysOf, func(r Ref) (cty.Value, error) {
		if r.Attr[0] == "arn" {
			return cty.DynamicVal, nil
		}
		return vals[r.String()], nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"a", "b"} {
		if v := out[k].(cty.Value); v.IsKnown() || !v.Type().Equals(cty.String) {
			t.Errorf("%s = %#v, want an unknown string", k, v)
		}
	}

	// A non-primitive value is an error naming the reference, known or not.
	for _, v := range []cty.Value{
		cty.ListVal([]cty.Value{cty.StringVal("a")}),
		cty.UnknownVal(cty.Map(cty.String)),
		cty.NullVal(cty.String),
	} {
		_, err := Substitute(map[string]any{"a": "x-${n/sg.id}"}, testKeysOf, func(Ref) (cty.Value, error) { return v, nil })
		if err == nil || !strings.Contains(err.Error(), "${n/sg.id}") || !strings.Contains(err.Error(), "config.a") {
			t.Errorf("%#v: err = %v, want an error naming the reference and the path", v, err)
		}
	}
}

func TestJSONDocumentWithTemplate(t *testing.T) {
	cfg := map[string]any{
		"policy": map[string]any{"Resource": "arn:aws:s3:::${p/db.name}/$${aws:username}/*"},
	}
	v, err := buildDoc(t, cfg, func(string) (cty.Value, bool) {
		return cty.ObjectVal(map[string]cty.Value{"name": cty.StringVal("bucket")}), true
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := v.GetAttr("policy").AsString(), `{"Resource":"arn:aws:s3:::bucket/${aws:username}/*"}`; got != want {
		t.Fatalf("policy = %s, want %s", got, want)
	}
}

func templateDesired() Desired {
	return Desired{Resources: []DesiredResource{
		{Key: "db", Type: "test_db", Config: map[string]any{"name": "db-${app/net.name}-${app/net.id}", "size": 10}},
		{Key: "net", Type: "test_net", Config: map[string]any{"name": "net"}},
	}}
}

func TestPlanTemplateOrdersAndKeepsChosen(t *testing.T) {
	text := "a db named after its net"
	e := newTestEnv(t, StaticPlanner{hashOf(text): templateDesired()})
	e.writeIntent("app", text)
	p := e.plan()
	if got, want := summary(p), []string{"create app/net", "create app/db"}; !equalStrings(got, want) {
		t.Fatalf("changes = %v, want %v", got, want)
	}
	if name := p.Changes[1].After.GetAttr("name"); name.IsKnown() {
		t.Fatalf("db.name = %#v, want unknown before the net exists", name)
	}
	e.mustApply(p)
	st, _ := e.loadState("app")
	if got := stateAttrs(t, st.Resources["db"])["name"]; got != "db-net-test_net-1" {
		t.Fatalf("db name = %v, want db-net-test_net-1", got)
	}
	if got := lockRes(e.loadLock().Intents["app"], "db").Chosen["name"]; got != "db-${app/net.name}-${app/net.id}" {
		t.Fatalf("chosen name = %v, want the template", got)
	}
}

func TestPlanTemplateReferenceMustBePrimitive(t *testing.T) {
	text := "templates over every kind"
	e := newTestEnv(t, StaticPlanner{hashOf(text): {Resources: []DesiredResource{
		{Key: "list", Type: "test_list", Config: map[string]any{"name": "l", "items": []any{"a"}}},
		{Key: "a", Type: "test_net", Config: map[string]any{"name": "x-${app/list.items}"}},
		{Key: "b", Type: "test_net", Config: map[string]any{"name": "x-${app/list.items.0}-${app/c.id}"}},
		{Key: "c", Type: "test_db", Config: map[string]any{"name": "c", "net_id": "${app/list.groups}/x", "tags": map[string]any{"n": "${app/list.name}-${app/list.id}"}}},
	}}})
	e.writeIntent("app", text)
	_, err := e.engine.Plan(context.Background())
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want a *ValidationError", err)
	}
	var got []string
	for _, pr := range ve.Problems {
		got = append(got, pr.Key+": "+pr.Message)
	}
	want := []string{
		"a: reference ${app/list.items} in a template: it is a list of string; a template takes only a string, number or bool",
		"c: reference ${app/list.groups} in a template: it is a map of list of string; a template takes only a string, number or bool",
	}
	if !equalStrings(got, want) {
		t.Errorf("problems =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestReferencedByIncludesTemplates(t *testing.T) {
	others := map[string][]DesiredResource{
		"web": {{Key: "svc", Type: "test_net", Config: map[string]any{"name": "svc-${app/net.name}", "tags": map[string]any{"k": "${app/db.id}"}}}},
	}
	got := referencedBy("app", []string{"db", "net"}, others)
	if want := map[string][]string{"db": {"web/svc"}, "net": {"web/svc"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("referencedBy = %v, want %v", got, want)
	}
}

func TestSensitiveAttributeRejectsTemplate(t *testing.T) {
	text := "a password from a template"
	e := newTestEnv(t, StaticPlanner{hashOf(text): {Resources: []DesiredResource{
		{Key: "net", Type: "test_net", Config: map[string]any{"name": "net"}},
		{Key: "db", Type: "test_db", Config: map[string]any{"name": "db", "password": "pw-${app/net.id}"}},
	}}})
	e.writeIntent("app", text)
	_, err := e.engine.Plan(context.Background())
	var ve *ValidationError
	if !errors.As(err, &ve) || len(ve.Problems) != 1 || !strings.Contains(ve.Problems[0].Message, `attribute "password" of app/db is sensitive`) {
		t.Fatalf("err = %v, want one sensitive-literal problem", err)
	}
}
