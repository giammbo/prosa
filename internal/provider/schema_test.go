package provider

import (
	"strings"
	"testing"

	"github.com/zclconf/go-cty/cty"

	"github.com/giammbo/prosa/internal/tfplugin5"
)

func TestConvertSchemaFlags(t *testing.T) {
	in := &tfplugin5.Schema{
		Version: 3,
		Block: &tfplugin5.Schema_Block{
			Attributes: []*tfplugin5.Schema_Attribute{
				{Name: "password", Type: []byte(`"string"`), Optional: true, Sensitive: true, WriteOnly: true},
				{Name: "id", Type: []byte(`"string"`), Computed: true},
				{Name: "name", Type: []byte(`"string"`), Required: true},
			},
		},
	}
	rs, err := convertSchema(in)
	if err != nil {
		t.Fatal(err)
	}
	if rs.Version != 3 {
		t.Errorf("version = %d, want 3", rs.Version)
	}
	pw := rs.Block.Attributes["password"]
	if pw == nil || !pw.Sensitive || !pw.WriteOnly || !pw.Optional || pw.Computed || pw.Required {
		t.Errorf("password flags wrong: %+v", pw)
	}
	if !pw.Type.Equals(cty.String) {
		t.Errorf("password type = %s, want string", pw.Type.FriendlyName())
	}
	id := rs.Block.Attributes["id"]
	if id == nil || !id.Computed || id.Sensitive || id.WriteOnly {
		t.Errorf("id flags wrong: %+v", id)
	}
	if n := rs.Block.Attributes["name"]; n == nil || !n.Required {
		t.Errorf("name flags wrong: %+v", n)
	}
}

func TestImpliedTypeNesting(t *testing.T) {
	inner := &tfplugin5.Schema_Block{
		Attributes: []*tfplugin5.Schema_Attribute{{Name: "a", Type: []byte(`"string"`), Optional: true}},
	}
	obj := cty.Object(map[string]cty.Type{"a": cty.String})
	cases := []struct {
		mode tfplugin5.Schema_NestedBlock_NestingMode
		want cty.Type
	}{
		{tfplugin5.Schema_NestedBlock_SINGLE, obj},
		{tfplugin5.Schema_NestedBlock_GROUP, obj},
		{tfplugin5.Schema_NestedBlock_LIST, cty.List(obj)},
		{tfplugin5.Schema_NestedBlock_SET, cty.Set(obj)},
		{tfplugin5.Schema_NestedBlock_MAP, cty.Map(obj)},
	}
	for _, c := range cases {
		t.Run(c.mode.String(), func(t *testing.T) {
			rs, err := convertSchema(&tfplugin5.Schema{Block: &tfplugin5.Schema_Block{
				BlockTypes: []*tfplugin5.Schema_NestedBlock{{TypeName: "b", Nesting: c.mode, Block: inner}},
			}})
			if err != nil {
				t.Fatal(err)
			}
			got := rs.Block.ImpliedType().AttributeType("b")
			if !got.Equals(c.want) {
				t.Errorf("got %s, want %s", got.GoString(), c.want.GoString())
			}
		})
	}
}

func TestConvertSchemaComplexType(t *testing.T) {
	rs, err := convertSchema(&tfplugin5.Schema{Block: &tfplugin5.Schema_Block{
		Attributes: []*tfplugin5.Schema_Attribute{{Name: "tags", Type: []byte(`["map","string"]`), Optional: true}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got := rs.Block.Attributes["tags"].Type; !got.Equals(cty.Map(cty.String)) {
		t.Errorf("got %s", got.GoString())
	}
}

func TestConvertSchemaRejectsDynamicInCollectionBlock(t *testing.T) {
	_, err := convertSchema(&tfplugin5.Schema{Block: &tfplugin5.Schema_Block{
		BlockTypes: []*tfplugin5.Schema_NestedBlock{{
			TypeName: "dyn",
			Nesting:  tfplugin5.Schema_NestedBlock_LIST,
			Block: &tfplugin5.Schema_Block{
				Attributes: []*tfplugin5.Schema_Attribute{{Name: "v", Type: []byte(`"dynamic"`), Optional: true}},
			},
		}},
	}})
	if err == nil || !strings.Contains(err.Error(), `unsupported dynamic block "dyn"`) {
		t.Fatalf("err = %v", err)
	}
}

func TestConvertSchemaBadType(t *testing.T) {
	_, err := convertSchema(&tfplugin5.Schema{Block: &tfplugin5.Schema_Block{
		Attributes: []*tfplugin5.Schema_Attribute{{Name: "x", Type: []byte(`not json`)}},
	}})
	if err == nil || !strings.Contains(err.Error(), `"x"`) {
		t.Fatalf("err = %v", err)
	}
}

func TestConvertSchemaKeepsDescriptions(t *testing.T) {
	in := &tfplugin5.Schema{
		Block: &tfplugin5.Schema_Block{
			Description: "the resource",
			Attributes: []*tfplugin5.Schema_Attribute{
				{Name: "policy", Type: []byte(`"string"`), Optional: true, Description: "A JSON policy document."},
			},
			BlockTypes: []*tfplugin5.Schema_NestedBlock{{
				TypeName: "rule", Nesting: tfplugin5.Schema_NestedBlock_LIST,
				Block: &tfplugin5.Schema_Block{
					Description: "a rule",
					Attributes:  []*tfplugin5.Schema_Attribute{{Name: "x", Type: []byte(`"string"`), Optional: true, Description: "the x"}},
				},
			}},
		},
	}
	rs, err := convertSchema(in)
	if err != nil {
		t.Fatal(err)
	}
	if rs.Block.Description != "the resource" || rs.Block.Attributes["policy"].Description != "A JSON policy document." {
		t.Errorf("top level: %q / %q", rs.Block.Description, rs.Block.Attributes["policy"].Description)
	}
	rule := rs.Block.BlockTypes["rule"].Block
	if rule.Description != "a rule" || rule.Attributes["x"].Description != "the x" {
		t.Errorf("nested: %q / %q", rule.Description, rule.Attributes["x"].Description)
	}
}
