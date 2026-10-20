package provider

import (
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

	"github.com/zclconf/go-cty/cty"
)

// FormatOptions tunes FormatBlock.
type FormatOptions struct {
	// DescLimit cuts each description to that many characters ("..." marks
	// the cut); <= 0 keeps it whole.
	DescLimit int
	// MaxDepth is how many levels of nested blocks are shown with their
	// content; a deeper block is one line pointing at Path. <= 0 shows all.
	MaxDepth int
	// Path names b in the pointer of an omitted block, as "<Path>.<blocks>".
	Path string
	// KeepComputedDescriptions keeps the descriptions of the attributes that
	// are only computed, which are left out by default: nobody sets them, and
	// they cost a small context window (ADR-031).
	KeepComputedDescriptions bool
}

// FormatBlock prints the attributes of b, sorted by name, one per line as
// "name  type  flags", then its nested blocks sorted by name as
// "name  block nesting[  min N][  max N]", with their children indented by
// two more spaces. A description, when present, ends its line as
// "  # <description>", on one line, except for an attribute that is only
// computed (see KeepComputedDescriptions). A block deeper than opts.MaxDepth
// is printed without its content, with a pointer to ask for it.
func FormatBlock(w io.Writer, b *Block, opts FormatOptions) {
	formatBlock(w, b, opts, "", opts.Path, 1)
}

func formatBlock(w io.Writer, b *Block, opts FormatOptions, indent, path string, depth int) {
	if b == nil {
		return
	}
	for _, name := range slices.Sorted(maps.Keys(b.Attributes)) {
		a := b.Attributes[name]
		line := fmt.Sprintf("%s%s  %s", indent, name, typeName(a.Type))
		if flags := attributeFlags(a); flags != "" {
			line += "  " + flags
		}
		if computedOnly(a) && !opts.KeepComputedDescriptions {
			fmt.Fprintln(w, line)
			continue
		}
		fmt.Fprintln(w, line+describe(a.Description, opts.DescLimit))
	}
	for _, name := range slices.Sorted(maps.Keys(b.BlockTypes)) {
		nb := b.BlockTypes[name]
		line := indent + name + "  " + NestedBlockSummary(nb, "  ")
		sub := path + "." + name
		if opts.MaxDepth > 0 && depth > opts.MaxDepth {
			fmt.Fprintf(w, "%s  (nested content omitted: depth limit; ask for %q)\n", line, sub)
			continue
		}
		if nb.Block != nil {
			line += describe(nb.Block.Description, opts.DescLimit)
		}
		fmt.Fprintln(w, line)
		formatBlock(w, nb.Block, opts, indent+"  ", sub, depth+1)
	}
}

// NestedBlockSummary returns "block <nesting>" followed by "min N" and
// "max N" when set, joined by sep.
func NestedBlockSummary(nb *NestedBlock, sep string) string {
	s := "block " + nestingName(nb.Nesting)
	if nb.MinItems > 0 {
		s += fmt.Sprintf("%smin %d", sep, nb.MinItems)
	}
	if nb.MaxItems > 0 {
		s += fmt.Sprintf("%smax %d", sep, nb.MaxItems)
	}
	return s
}

// describe returns "  # <d>" with d's whitespace collapsed and cut to limit
// characters, or "" for an empty description.
func describe(d string, limit int) string {
	d = strings.Join(strings.Fields(d), " ")
	if d == "" {
		return ""
	}
	if r := []rune(d); limit > 0 && len(r) > limit {
		d = string(r[:limit]) + "..."
	}
	return "  # " + d
}

// computedOnly reports whether a is set by the provider alone.
func computedOnly(a *Attribute) bool { return a.Computed && !a.Optional && !a.Required }

// attributeFlags lists the attribute flags in the fixed order
// required, optional, computed, sensitive, write_only.
func attributeFlags(a *Attribute) string {
	var flags []string
	for _, f := range []struct {
		on   bool
		name string
	}{
		{a.Required, "required"},
		{a.Optional, "optional"},
		{a.Computed, "computed"},
		{a.Sensitive, "sensitive"},
		{a.WriteOnly, "write_only"},
	} {
		if f.on {
			flags = append(flags, f.name)
		}
	}
	return strings.Join(flags, " ")
}

// typeName renders a cty type. Collections are written as map(string) rather
// than cty's "map of string"; other types use cty's FriendlyName.
func typeName(t cty.Type) string {
	switch {
	case t.IsMapType():
		return "map(" + typeName(t.ElementType()) + ")"
	case t.IsListType():
		return "list(" + typeName(t.ElementType()) + ")"
	case t.IsSetType():
		return "set(" + typeName(t.ElementType()) + ")"
	}
	return t.FriendlyName()
}

func nestingName(m NestingMode) string {
	switch m {
	case NestingSingle:
		return "single"
	case NestingGroup:
		return "group"
	case NestingList:
		return "list"
	case NestingSet:
		return "set"
	case NestingMap:
		return "map"
	}
	return fmt.Sprintf("unknown(%d)", int(m))
}
