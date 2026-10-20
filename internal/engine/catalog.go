package engine

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/giammbo/prosa/internal/provider"
)

// Describe's limits: the length, in characters, at which each description is
// cut; the levels of nested blocks shown with their content; the default size
// of the whole text, in bytes, so that a huge schema (aws_wafv2_web_acl
// renders to about 1 MB) never reaches the planner whole. PROSA_SCHEMA_BYTES
// sets that size, for models with a small context (ADR-031).
const (
	describeLimit  = 200
	describeDepth  = 3
	describeBudget = 30 * 1024
)

// SchemaBudget returns the size, in bytes, at which Describe cuts a schema:
// PROSA_SCHEMA_BYTES from getenv, describeBudget when it is unset. A value
// that is not a positive integer is an error, with describeBudget.
func SchemaBudget(getenv func(string) string) (int, error) {
	v := getenv("PROSA_SCHEMA_BYTES")
	if v == "" {
		return describeBudget, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return describeBudget, fmt.Errorf("PROSA_SCHEMA_BYTES=%q: want a positive number of bytes", v)
	}
	return n, nil
}

// schemaCatalog is a SchemaCatalog over a provider schema.
type schemaCatalog struct {
	s      *provider.Schema
	budget int
}

// NewSchemaCatalog returns a SchemaCatalog over the resource types of s,
// with the budget of SchemaBudget (the default when PROSA_SCHEMA_BYTES is
// not valid: the CLI refuses it before).
func NewSchemaCatalog(s *provider.Schema) SchemaCatalog {
	budget, _ := SchemaBudget(os.Getenv)
	return schemaCatalog{s, budget}
}

// tokens splits s on spaces and underscores, lower case.
func tokens(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return r == '_' || r == ' ' || r == '\t' || r == '\n'
	})
}

// Search scores every resource type by the query tokens it matches: 2 for a
// token of the name equal to it, 1 for one that starts with it or that it
// starts with (plurals, abbreviations), the best token counting. Types that
// match nothing are left out; ties are lexicographic.
func (c schemaCatalog) Search(query string, limit int) []string {
	qs := tokens(query)
	type scored struct {
		name  string
		score int
	}
	var hits []scored
	for name := range c.s.Resources {
		ns := tokens(name)
		score := 0
		for _, q := range qs {
			best := 0
			for _, n := range ns {
				switch {
				case n == q:
					best = 2
				case best < 1 && (strings.HasPrefix(n, q) || strings.HasPrefix(q, n)):
					best = 1
				}
			}
			score += best
		}
		if score > 0 {
			hits = append(hits, scored{name, score})
		}
	}
	slices.SortFunc(hits, func(a, b scored) int {
		return cmp.Or(cmp.Compare(b.score, a.score), strings.Compare(a.name, b.name))
	})
	if limit >= 0 && len(hits) > limit {
		hits = hits[:limit]
	}
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.name
	}
	return out
}

// Describe returns "resource <type> (schema version N)" followed by
// provider.FormatBlock of the type: descriptions cut to describeLimit,
// nested blocks shown describeDepth levels deep, the whole cut at the
// catalog's budget (SchemaBudget). "<type>.<block>[.<block>...]" describes a nested
// block, so that the planner can drill into what the depth limit omitted.
// An unknown type is an error naming up to five close type names; an unknown
// block one naming the blocks there.
func (c schemaCatalog) Describe(name string) (string, error) {
	typeName, path, _ := strings.Cut(name, ".")
	rs := c.s.Resources[typeName]
	if rs == nil {
		msg := fmt.Sprintf("unknown resource type %q", typeName)
		if near := c.Search(typeName, 5); len(near) > 0 {
			msg += "; closest: " + strings.Join(near, ", ")
		}
		return "", errors.New(msg)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "resource %s (schema version %d)", typeName, rs.Version)
	block, at := rs.Block, typeName
	if path != "" {
		var last *provider.NestedBlock
		for _, seg := range strings.Split(path, ".") {
			nb := block.BlockTypes[seg]
			if nb == nil {
				msg := fmt.Sprintf("%s has no nested block %q", at, seg)
				if kids := slices.Sorted(maps.Keys(block.BlockTypes)); len(kids) > 0 {
					msg += "; its blocks: " + strings.Join(kids, ", ")
				} else {
					msg += "; it has no nested blocks"
				}
				return "", errors.New(msg)
			}
			block, at, last = nb.Block, at+"."+seg, nb
		}
		fmt.Fprintf(&b, " block %s (%s)", path, strings.TrimPrefix(provider.NestedBlockSummary(last, ", "), "block "))
	}
	b.WriteString("\n")
	provider.FormatBlock(&b, block, provider.FormatOptions{DescLimit: describeLimit, MaxDepth: describeDepth, Path: name})
	return truncateLines(b.String(), c.budget), nil
}

// truncateLines keeps the whole lines of s that fit in budget bytes and, if
// any is left out, appends "… truncated: N more lines".
func truncateLines(s string, budget int) string {
	if len(s) <= budget {
		return s
	}
	lines := strings.SplitAfter(s, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	n, size := 0, 0
	for n < len(lines) && size+len(lines[n]) <= budget {
		size += len(lines[n])
		n++
	}
	return strings.Join(lines[:n], "") + fmt.Sprintf("… truncated: %d more lines\n", len(lines)-n)
}
