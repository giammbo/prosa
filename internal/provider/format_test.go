package provider

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/zclconf/go-cty/cty"
)

func TestFormatBlockDescriptions(t *testing.T) {
	long := strings.Repeat("x", 250)
	b := &Block{
		Attributes: map[string]*Attribute{
			"name": {Type: cty.String, Required: true, Description: "The name.\n  Must be unique."},
			"size": {Type: cty.Number, Optional: true, Description: long},
			"id":   {Type: cty.String, Computed: true},
		},
		BlockTypes: map[string]*NestedBlock{
			"rule": {Nesting: NestingList, MinItems: 1, MaxItems: 2, Block: &Block{
				Description: "A rule.",
				Attributes:  map[string]*Attribute{"port": {Type: cty.Number, Required: true, Description: "Port."}},
				BlockTypes:  map[string]*NestedBlock{},
			}},
		},
	}
	var buf bytes.Buffer
	FormatBlock(&buf, b, FormatOptions{DescLimit: 200})
	want := "" +
		"id  string  computed\n" +
		"name  string  required  # The name. Must be unique.\n" +
		"size  number  optional  # " + strings.Repeat("x", 200) + "...\n" +
		"rule  block list  min 1  max 2  # A rule.\n" +
		"  port  number  required  # Port.\n"
	if got := buf.String(); got != want {
		t.Errorf("FormatBlock output mismatch\n--- got ---\n%s--- want ---\n%s", got, want)
	}

	buf.Reset()
	FormatBlock(&buf, b, FormatOptions{})
	if !strings.Contains(buf.String(), "# "+long+"\n") {
		t.Errorf("limit 0 truncated the description:\n%s", buf.String())
	}
}

// chain returns a block nested depth levels deep: each level has an
// attribute "a<level>" and a block "n<level+1>" (list, max 1).
func chain(level, depth int) *Block {
	b := &Block{
		Attributes: map[string]*Attribute{fmt.Sprintf("a%d", level): {Type: cty.String, Optional: true}},
		BlockTypes: map[string]*NestedBlock{},
	}
	if level < depth {
		b.BlockTypes[fmt.Sprintf("n%d", level+1)] = &NestedBlock{Nesting: NestingList, MaxItems: 1, Block: chain(level+1, depth)}
	}
	return b
}

func TestFormatBlockDepthLimit(t *testing.T) {
	var buf bytes.Buffer
	FormatBlock(&buf, chain(0, 5), FormatOptions{MaxDepth: 3, Path: "test_deep"})
	want := "" +
		"a0  string  optional\n" +
		"n1  block list  max 1\n" +
		"  a1  string  optional\n" +
		"  n2  block list  max 1\n" +
		"    a2  string  optional\n" +
		"    n3  block list  max 1\n" +
		"      a3  string  optional\n" +
		"      n4  block list  max 1  (nested content omitted: depth limit; ask for \"test_deep.n1.n2.n3.n4\")\n"
	if got := buf.String(); got != want {
		t.Errorf("depth-limited output mismatch\n--- got ---\n%s--- want ---\n%s", got, want)
	}
}

// The description of an attribute that is only computed is left out, unless
// KeepComputedDescriptions: the planner never sets it.
func TestComputedOnlyDescriptionsDropped(t *testing.T) {
	b := &Block{
		Attributes: map[string]*Attribute{
			"arn":  {Type: cty.String, Computed: true, Description: "The ARN."},
			"name": {Type: cty.String, Optional: true, Computed: true, Description: "The name."},
			"size": {Type: cty.Number, Required: true, Description: "The size."},
		},
		BlockTypes: map[string]*NestedBlock{
			"rule": {Nesting: NestingList, Block: &Block{
				Attributes: map[string]*Attribute{"id": {Type: cty.String, Computed: true, Description: "Rule id."}},
				BlockTypes: map[string]*NestedBlock{},
			}},
		},
	}
	var buf bytes.Buffer
	FormatBlock(&buf, b, FormatOptions{})
	want := "" +
		"arn  string  computed\n" +
		"name  string  optional computed  # The name.\n" +
		"size  number  required  # The size.\n" +
		"rule  block list\n" +
		"  id  string  computed\n"
	if got := buf.String(); got != want {
		t.Errorf("FormatBlock output mismatch\n--- got ---\n%s--- want ---\n%s", got, want)
	}
	buf.Reset()
	FormatBlock(&buf, b, FormatOptions{KeepComputedDescriptions: true})
	if !strings.Contains(buf.String(), "arn  string  computed  # The ARN.\n") || !strings.Contains(buf.String(), "id  string  computed  # Rule id.\n") {
		t.Errorf("KeepComputedDescriptions dropped them:\n%s", buf.String())
	}
}
