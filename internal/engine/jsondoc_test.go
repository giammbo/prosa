package engine

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/zclconf/go-cty/cty"

	"github.com/giammbo/prosa/internal/provider"
)

func docBlock() *provider.Block {
	return &provider.Block{
		Attributes: map[string]*provider.Attribute{
			"name":                  {Type: cty.String, Optional: true},
			"policy":                {Type: cty.String, Optional: true},
			"container_definitions": {Type: cty.String, Optional: true},
			"tags":                  {Type: cty.Map(cty.String), Optional: true},
		},
		BlockTypes: map[string]*provider.NestedBlock{
			"rule": {Nesting: provider.NestingList, Block: &provider.Block{
				Attributes: map[string]*provider.Attribute{"doc": {Type: cty.String, Optional: true}},
				BlockTypes: map[string]*provider.NestedBlock{},
			}},
		},
	}
}

func buildDoc(t *testing.T, cfg map[string]any, lookup func(string) (cty.Value, bool)) (cty.Value, error) {
	t.Helper()
	keys := func(string) []string { return []string{"db"} }
	return buildConfig(docBlock(), "p", "", "r", cfg, keys, lookup)
}

func TestJSONDocumentAttribute(t *testing.T) {
	cfg := map[string]any{
		"policy": map[string]any{
			"Version": "2012-10-17",
			"Statement": []any{map[string]any{
				"Effect": "Allow", "Resource": "arn:aws:s3:::b/<*>&", "Action": []any{"s3:Get*", "s3:List*"},
				"Count": json.Number("12345678901234567890"), "Ratio": json.Number("0.10"), "Flag": true, "None": nil,
			}},
		},
		"rule": []any{map[string]any{"doc": map[string]any{"b": 1, "a": []any{}}}},
	}
	v, err := buildDoc(t, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"Statement":[{"Action":["s3:Get*","s3:List*"],"Count":12345678901234567890,"Effect":"Allow","Flag":true,"None":null,"Ratio":0.10,"Resource":"arn:aws:s3:::b/<*>&"}],"Version":"2012-10-17"}`
	if got := v.GetAttr("policy").AsString(); got != want {
		t.Fatalf("policy =\n%s\nwant\n%s", got, want)
	}
	if got := v.GetAttr("rule").Index(cty.NumberIntVal(0)).GetAttr("doc").AsString(); got != `{"a":[],"b":1}` {
		t.Fatalf("nested doc = %s", got)
	}
}

func TestJSONDocumentWithReference(t *testing.T) {
	cfg := map[string]any{
		"container_definitions": []any{map[string]any{
			"name":    "app",
			"secrets": []any{map[string]any{"valueFrom": "${p/db.master_user_secret.0.secret_arn}", "name": "DB"}},
		}},
	}
	secret := cty.ObjectVal(map[string]cty.Value{
		"master_user_secret": cty.ListVal([]cty.Value{cty.ObjectVal(map[string]cty.Value{"secret_arn": cty.StringVal("arn:aws:secretsmanager:x:1:secret:s\"q")})}),
	})
	v, err := buildDoc(t, cfg, func(string) (cty.Value, bool) { return secret, true })
	if err != nil {
		t.Fatal(err)
	}
	const want = `[{"name":"app","secrets":[{"name":"DB","valueFrom":"arn:aws:secretsmanager:x:1:secret:s\"q"}]}]`
	if got := v.GetAttr("container_definitions").AsString(); got != want {
		t.Fatalf("container_definitions = %s, want %s", got, want)
	}

	// An unknown inside makes the whole string unknown.
	v, err = buildDoc(t, cfg, func(string) (cty.Value, bool) {
		return cty.ObjectVal(map[string]cty.Value{"master_user_secret": cty.UnknownVal(cty.List(cty.Object(map[string]cty.Type{"secret_arn": cty.String})))}), true
	})
	if err != nil {
		t.Fatal(err)
	}
	if cd := v.GetAttr("container_definitions"); cd.IsKnown() || cd.Type() != cty.String {
		t.Fatalf("container_definitions = %#v, want unknown string", cd)
	}

	// A bad index is an error naming the reference.
	_, err = buildDoc(t, cfg, func(string) (cty.Value, bool) {
		return cty.ObjectVal(map[string]cty.Value{"master_user_secret": cty.ListValEmpty(cty.String)}), true
	})
	if err == nil || !strings.Contains(err.Error(), "invalid index") {
		t.Fatalf("err = %v, want an invalid index error", err)
	}
}

func TestJSONDocumentDeterministic(t *testing.T) {
	a := map[string]any{}
	b := map[string]any{}
	for _, k := range []string{"z", "m", "a", "q", "b"} {
		a[k] = map[string]any{"y": 1, "x": []any{"1", 2}}
	}
	for _, k := range []string{"b", "q", "a", "m", "z"} {
		b[k] = map[string]any{"x": []any{"1", 2}, "y": 1}
	}
	va, err := buildDoc(t, map[string]any{"policy": a}, nil)
	if err != nil {
		t.Fatal(err)
	}
	vb, err := buildDoc(t, map[string]any{"policy": b}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ga, gb := va.GetAttr("policy").AsString(), vb.GetAttr("policy").AsString(); ga != gb || strings.Contains(ga, " ") {
		t.Fatalf("documents differ or have spaces:\n%s\n%s", ga, gb)
	}
}

func TestJSONDocumentLeavesOtherAttributesAlone(t *testing.T) {
	v, err := buildDoc(t, map[string]any{"policy": `{"already":"json"}`, "tags": map[string]any{"k": "v"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := v.GetAttr("policy").AsString(); got != `{"already":"json"}` {
		t.Fatalf("string policy = %s", got)
	}
	if got := v.GetAttr("tags").Index(cty.StringVal("k")).AsString(); got != "v" {
		t.Fatalf("tags = %#v", v.GetAttr("tags"))
	}
}
