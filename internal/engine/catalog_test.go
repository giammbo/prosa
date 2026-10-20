package engine

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/zclconf/go-cty/cty"

	"github.com/giammbo/prosa/internal/provider"
)

func TestSchemaCatalog(t *testing.T) {
	cat := NewSchemaCatalog(newFakeProvider().schema)

	got := cat.Search("db instance", 10)
	if !slices.Contains(got, "test_db") {
		t.Fatalf("Search(db instance) = %v, want test_db", got)
	}
	if got[0] != "test_db" {
		t.Errorf("Search(db instance) = %v, want test_db first", got)
	}
	// "test" matches every type: ties are lexicographic, the limit applies.
	if got, want := cat.Search("TEST", 3), []string{"test_app", "test_clock", "test_db"}; !slices.Equal(got, want) {
		t.Errorf("Search(TEST, 3) = %v, want %v", got, want)
	}
	if got := cat.Search("secret_value", 10); len(got) == 0 || got[0] != "test_secret" {
		t.Errorf("Search(secret_value) = %v, want test_secret first", got)
	}
	if got := cat.Search("nothing", 10); len(got) != 0 {
		t.Errorf("Search(nothing) = %v", got)
	}

	d, err := cat.Describe("test_db")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"resource test_db (schema version 0)\n", "size  number  optional", "Allocated storage in GiB.", "password  string  optional sensitive"} {
		if !strings.Contains(d, want) {
			t.Errorf("Describe(test_db) lacks %q:\n%s", want, d)
		}
	}
	if d, _ := cat.Describe("test_secret"); !strings.HasPrefix(d, "resource test_secret (schema version 2)\n") || !strings.Contains(d, "write_only") {
		t.Errorf("Describe(test_secret) = %q", d)
	}

	_, err = cat.Describe("test_dbx")
	if err == nil || !strings.Contains(err.Error(), `"test_dbx"`) || !strings.Contains(err.Error(), "test_db") {
		t.Fatalf("Describe(test_dbx) err = %v, want the closest names", err)
	}
}

// deepBlock is a block nested depth levels deep: level i has an attribute
// "a<i>" and a block "n<i+1>" (list, max 1).
func deepBlock(level, depth int) *provider.Block {
	b := &provider.Block{
		Attributes: map[string]*provider.Attribute{fmt.Sprintf("a%d", level): {Type: cty.String, Optional: true}},
		BlockTypes: map[string]*provider.NestedBlock{},
	}
	if level < depth {
		b.BlockTypes[fmt.Sprintf("n%d", level+1)] = &provider.NestedBlock{Nesting: provider.NestingList, MaxItems: 1, Block: deepBlock(level+1, depth)}
	}
	return b
}

func TestSchemaCatalogBudget(t *testing.T) {
	wide := &provider.Block{Attributes: map[string]*provider.Attribute{}, BlockTypes: map[string]*provider.NestedBlock{}}
	for i := range 2000 {
		wide.Attributes[fmt.Sprintf("attr_%04d", i)] = &provider.Attribute{Type: cty.String, Optional: true, Description: strings.Repeat("d", 150)}
	}
	cat := NewSchemaCatalog(&provider.Schema{Resources: map[string]*provider.ResourceSchema{
		"test_deep": {Version: 1, Block: deepBlock(0, 6)},
		"test_wide": {Block: wide},
	}})

	// Depth: nested blocks are shown three levels deep, then a pointer.
	d, err := cat.Describe("test_deep")
	if err != nil {
		t.Fatal(err)
	}
	if want := "      n4  block list  max 1  (nested content omitted: depth limit; ask for \"test_deep.n1.n2.n3.n4\")\n"; !strings.Contains(d, want) {
		t.Errorf("Describe(test_deep) lacks %q:\n%s", want, d)
	}
	if strings.Contains(d, "a4") {
		t.Errorf("Describe(test_deep) shows content below the depth limit:\n%s", d)
	}

	// Drill down: the sub-block, with the depth limit counted from it.
	d, err = cat.Describe("test_deep.n1.n2.n3.n4")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(d, "resource test_deep (schema version 1) block n1.n2.n3.n4 (list, max 1)\na4  string  optional\nn5  block list  max 1\n") || !strings.Contains(d, "a6") {
		t.Errorf("Describe(test_deep.n1.n2.n3.n4) =\n%s", d)
	}
	for _, bad := range []string{"test_deep.n1.nope", "test_deep.n1.a1"} {
		_, err := cat.Describe(bad)
		if err == nil || !strings.Contains(err.Error(), "test_deep.n1") || !strings.Contains(err.Error(), "n2") {
			t.Errorf("Describe(%s) err = %v, want the child blocks of test_deep.n1", bad, err)
		}
	}
	if _, err := cat.Describe("test_nope.x"); err == nil || !strings.Contains(err.Error(), `unknown resource type "test_nope"`) {
		t.Errorf("Describe(test_nope.x) err = %v", err)
	}

	// Bytes: at most 30 KB, then how many lines were left out.
	d, err = cat.Describe("test_wide")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(d, "\n"), "\n")
	last := lines[len(lines)-1]
	var more int
	if _, err := fmt.Sscanf(last, "… truncated: %d more lines", &more); err != nil {
		t.Fatalf("last line = %q, want the truncation note", last)
	}
	if body := len(d) - len(last) - 1; body > 30*1024 {
		t.Errorf("rendered %d bytes before the note, want at most %d", body, 30*1024)
	}
	if shown := len(lines) - 2; shown+more != 2000 { // minus header and note
		t.Errorf("shown %d + omitted %d attributes, want 2000", shown, more)
	}
}

// PROSA_SCHEMA_BYTES sets the budget of Describe; a value that is not a
// positive number is an error for SchemaBudget and the default for the catalog.
func TestSchemaBudgetEnv(t *testing.T) {
	wide := &provider.Block{Attributes: map[string]*provider.Attribute{}, BlockTypes: map[string]*provider.NestedBlock{}}
	for i := range 200 {
		wide.Attributes[fmt.Sprintf("attr_%04d", i)] = &provider.Attribute{Type: cty.String, Optional: true, Description: strings.Repeat("d", 150)}
	}
	schema := &provider.Schema{Resources: map[string]*provider.ResourceSchema{"test_wide": {Block: wide}}}
	body := func() int {
		d, err := NewSchemaCatalog(schema).Describe("test_wide")
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(strings.TrimSuffix(d, "\n"), "\n")
		if !strings.HasPrefix(lines[len(lines)-1], "… truncated: ") {
			return len(d)
		}
		return len(d) - len(lines[len(lines)-1]) - 1
	}
	t.Setenv("PROSA_SCHEMA_BYTES", "4096")
	if n := body(); n > 4096 || n < 3000 {
		t.Errorf("PROSA_SCHEMA_BYTES=4096: %d bytes before the note", n)
	}
	t.Setenv("PROSA_SCHEMA_BYTES", "")
	if n := body(); n <= 4096 {
		t.Errorf("default budget: %d bytes, want the default 30 KB", n)
	}
	for _, c := range []struct {
		v    string
		want int
		bad  bool
	}{{"", 30720, false}, {"8192", 8192, false}, {"0", 30720, true}, {"-1", 30720, true}, {"30kb", 30720, true}} {
		got, err := SchemaBudget(func(string) string { return c.v })
		if got != c.want || (err != nil) != c.bad {
			t.Errorf("SchemaBudget(%q) = %d, %v; want %d, error %v", c.v, got, err, c.want, c.bad)
		}
		if c.bad && (err == nil || !strings.Contains(err.Error(), "PROSA_SCHEMA_BYTES")) {
			t.Errorf("SchemaBudget(%q) err = %v, want it to name PROSA_SCHEMA_BYTES", c.v, err)
		}
	}
	t.Setenv("PROSA_SCHEMA_BYTES", "nope")
	if n := body(); n <= 4096 {
		t.Errorf("invalid PROSA_SCHEMA_BYTES: %d bytes, want the default budget", n)
	}
}
