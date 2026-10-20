package engine

import (
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"

	"github.com/giammbo/prosa/internal/provider"
)

// item is one line of a resource body, in Terraform's shape: a symbol, an
// aligned name and its value, or a whole text (a block header, a list
// element, a comment). A multi-line item (close set) holds children one
// level deeper, then its closing line.
type item struct {
	sym      string // "+", "-", "~", "-/+", or "" (unchanged, comments)
	name     string // aligned with the other names of its level; "" for none
	text     string // the value after "name = ", or the whole line
	children []item
	close    string // "}", "]", "}," ... with any suffix (" -> null")
	gap      bool   // a blank line before
}

func (it item) multi() bool { return it.close != "" }

// withSuffix appends s to the last line of it.
func withSuffix(it item, s string) item {
	if it.multi() {
		it.close += s
	} else {
		it.text += s
	}
	return it
}

// writeItems writes items at depth (0 for the resource line): four spaces
// per level, the symbol right-aligned on three columns, the names of the
// level aligned on their "=".
func writeItems(w io.Writer, st style, items []item, depth int) {
	width := 0
	for _, it := range items {
		width = max(width, utf8.RuneCountInString(it.name))
	}
	indent := strings.Repeat("    ", depth)
	for _, it := range items {
		if it.gap {
			fmt.Fprintln(w)
		}
		var b strings.Builder
		b.WriteString(indent)
		b.WriteString(strings.Repeat(" ", max(0, 3-len(it.sym))))
		b.WriteString(st.symbol(it.sym))
		b.WriteString(" ")
		if it.name != "" {
			b.WriteString(it.name)
			b.WriteString(strings.Repeat(" ", width-utf8.RuneCountInString(it.name)))
			b.WriteString(" = ")
		}
		b.WriteString(it.text)
		fmt.Fprintln(w, b.String())
		if it.multi() {
			writeItems(w, st, it.children, depth+1)
			fmt.Fprintln(w, indent+"    "+it.close)
		}
	}
}

// differ builds the items of a resource body. replace holds the paths whose
// change forces a replacement (name, tags["team"], rule[0].path).
type differ struct {
	st      style
	replace map[string]bool
}

func (d *differ) unknown() string { return d.st.paint(ansiDim, "(known after apply)") }

// hidden is the comment "# (N unchanged <noun>s hidden)".
func (d *differ) hidden(n int, noun string) item {
	if n != 1 {
		noun += "s"
	}
	return item{text: d.st.paint(ansiDim, fmt.Sprintf("# (%d unchanged %s hidden)", n, noun))}
}

// objectItems renders every non-null attribute of v, an object of block b
// (nil when the schema is unknown), then its blocks, all with sym.
func (d *differ) objectItems(sym string, b *provider.Block, v cty.Value) []item {
	var attrs []item
	var groups [][]item
	for _, name := range attrNames(v) {
		av := v.GetAttr(name)
		if nb := blockType(b, name); nb != nil {
			if g := d.wholeBlocks(sym, name, nb, av); len(g) > 0 {
				groups = append(groups, g)
			}
			continue
		}
		if av.IsNull() {
			continue
		}
		attrs = append(attrs, d.valueItem(sym, name, av, sensitiveAttr(b, name)))
	}
	return withBlocks(attrs, groups)
}

// withBlocks appends the groups of block items to items, a blank line
// before each group that follows something.
func withBlocks(items []item, groups [][]item) []item {
	for _, g := range groups {
		if len(items) > 0 {
			g[0].gap = true
		}
		items = append(items, g...)
	}
	return items
}

// valueItem is "<sym> name = <v>", with " -> null" for a removal.
func (d *differ) valueItem(sym, name string, v cty.Value, sensitive bool) item {
	it := d.value(sym, v, sensitive)
	it.sym, it.name = sym, name
	if sym == "-" {
		it = withSuffix(it, " -> null")
	}
	return it
}

// value renders v, its elements marked with sym: strings quoted, numbers
// and bools plain, lists as [ ... ] (inline with one primitive element),
// maps and objects as { k = v }.
func (d *differ) value(sym string, v cty.Value, sensitive bool) item {
	switch {
	case sensitive && !v.IsNull():
		return item{text: "(sensitive value)"}
	case !v.IsKnown():
		return item{text: d.unknown()}
	case v.IsNull():
		return item{text: "null"}
	}
	switch ty := v.Type(); {
	case ty.IsListType() || ty.IsSetType() || ty.IsTupleType():
		var elems []item
		for it := v.ElementIterator(); it.Next(); {
			_, e := it.Element()
			el := d.value(sym, e, false)
			el.sym = sym
			elems = append(elems, withSuffix(el, ","))
		}
		switch {
		case len(elems) == 0:
			return item{text: "[]"}
		case len(elems) == 1 && !elems[0].multi():
			return item{text: "[" + strings.TrimSuffix(elems[0].text, ",") + "]"}
		}
		return item{text: "[", children: elems, close: "]"}
	case ty.IsMapType() || ty.IsObjectType():
		var children []item
		for _, k := range elemKeys(v) {
			e := elemOrNull(v, k)
			if ty.IsObjectType() && e.IsNull() {
				continue
			}
			el := d.value(sym, e, false)
			el.sym, el.name = sym, keyLabel(v, k)
			children = append(children, el)
		}
		if len(children) == 0 {
			return item{text: "{}"}
		}
		return item{text: "{", children: children, close: "}"}
	}
	return item{text: primitive(v)}
}

// primitive renders a known, non-null primitive value.
func primitive(v cty.Value) string {
	switch v.Type() {
	case cty.String:
		return strconv.Quote(v.AsString())
	case cty.Number:
		return v.AsBigFloat().Text('f', -1)
	case cty.Bool:
		return strconv.FormatBool(v.True())
	}
	js, err := ctyjson.Marshal(v, v.Type())
	if err != nil {
		return "(" + v.Type().FriendlyName() + ")"
	}
	return string(js)
}

// diffObject renders the changes from before to after, objects of block b:
// the id as context when context is set and it does not change, each
// changed attribute, the count of the unchanged ones, the changed blocks,
// the count of the unchanged ones.
func (d *differ) diffObject(b *provider.Block, before, after cty.Value, path string, context bool) []item {
	var ctx, attrs []item
	var groups [][]item
	hiddenAttrs, hiddenBlocks := 0, 0
	for _, name := range unionKeys(before, after) {
		bv, av := elemOrNull(before, name), elemOrNull(after, name)
		p := joinPath(path, name)
		if nb := blockType(b, name); nb != nil {
			g, hidden := d.diffBlocks(name, nb, bv, av, p)
			hiddenBlocks += hidden
			if len(g) > 0 {
				groups = append(groups, g)
			}
			continue
		}
		switch {
		case bv.IsNull() && av.IsNull():
		case same(bv, av) && context && name == "id":
			it := d.value("", bv, sensitiveAttr(b, name))
			it.name = name
			ctx = append(ctx, it)
		case same(bv, av):
			hiddenAttrs++
		default:
			attrs = append(attrs, d.diffValue(name, bv, av, sensitiveAttr(b, name), p)...)
		}
	}
	out := append(ctx, attrs...)
	if hiddenAttrs > 0 {
		out = append(out, d.hidden(hiddenAttrs, "attribute"))
	}
	out = withBlocks(out, groups)
	if hiddenBlocks > 0 {
		out = append(out, d.hidden(hiddenBlocks, "block"))
	}
	return out
}

// diffValue renders the change of the attribute (or element) name from bv
// to av, marked " # forces replacement" when its path forces one.
func (d *differ) diffValue(name string, bv, av cty.Value, sensitive bool, path string) []item {
	out := d.changeItems(name, bv, av, sensitive, path)
	if d.replace[path] {
		last := len(out) - 1
		out[last] = withSuffix(out[last], " "+d.st.paint(ansiRed, "# forces replacement"))
	}
	return out
}

func (d *differ) changeItems(name string, bv, av cty.Value, sensitive bool, path string) []item {
	switch {
	case bv.IsNull():
		return []item{d.valueItem("+", name, av, sensitive)}
	case av.IsNull():
		return []item{d.valueItem("-", name, bv, sensitive)}
	case sensitive:
		return []item{{sym: "~", name: name, text: "(sensitive value)"}}
	case !av.IsKnown():
		it := withSuffix(d.value("-", bv, false), " -> "+d.unknown())
		it.sym, it.name = "~", name
		return []item{it}
	case !bv.IsKnown():
		it := d.value("+", av, false)
		it.sym, it.name, it.text = "~", name, d.unknown()+" -> "+it.text
		return []item{it}
	}
	if kind := collectionKind(bv); kind != "" && kind == collectionKind(av) {
		switch kind {
		case "map", "object":
			return []item{d.diffMap(name, bv, av, kind == "object", path)}
		case "list":
			return []item{d.diffList(name, bv, av)}
		case "set":
			return []item{d.diffSet(name, bv, av)}
		}
	}
	old, nu := d.value("-", bv, false), d.value("+", av, false)
	if !old.multi() && !nu.multi() {
		return []item{{sym: "~", name: name, text: old.text + " -> " + nu.text}}
	}
	old = withSuffix(old, " -> null")
	old.sym, old.name = "-", name
	nu.sym, nu.name = "+", name
	return []item{old, nu}
}

// diffMap renders the change of a map or an object element by element.
func (d *differ) diffMap(name string, bv, av cty.Value, object bool, path string) item {
	var children []item
	hidden := 0
	for _, k := range unionKeys(bv, av) {
		bk, ak := elemOrNull(bv, k), elemOrNull(av, k)
		switch {
		case bk.IsNull() && ak.IsNull():
		case same(bk, ak):
			hidden++
		default:
			label, p := strconv.Quote(k), path+"["+strconv.Quote(k)+"]"
			if object {
				label, p = k, joinPath(path, k)
			}
			children = append(children, d.diffValue(label, bk, ak, false, p)...)
		}
	}
	if hidden > 0 {
		noun := "element"
		if object {
			noun = "attribute"
		}
		children = append(children, d.hidden(hidden, noun))
	}
	return item{sym: "~", name: name, text: "{", children: children, close: "}"}
}

// diffList renders the change of a list along the longest common
// subsequence of its elements: the removed and added ones, the count of the
// unchanged ones.
func (d *differ) diffList(name string, bv, av cty.Value) item {
	b, a := elems(bv), elems(av)
	n, m := len(b), len(a)
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if same(b[i], a[j]) {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	var children []item
	hidden := 0
	for i, j := 0, 0; i < n || j < m; {
		switch {
		case i < n && j < m && same(b[i], a[j]):
			hidden++
			i, j = i+1, j+1
		case i < n && (j == m || lcs[i+1][j] >= lcs[i][j+1]):
			children = append(children, d.element("-", b[i]))
			i++
		default:
			children = append(children, d.element("+", a[j]))
			j++
		}
	}
	if hidden > 0 {
		children = append(children, d.hidden(hidden, "element"))
	}
	return item{sym: "~", name: name, text: "[", children: children, close: "]"}
}

// diffSet renders the change of a set: the removed and added elements, the
// count of the unchanged ones.
func (d *differ) diffSet(name string, bv, av cty.Value) item {
	b, a := elems(bv), elems(av)
	var children []item
	for _, e := range b {
		if !contains(a, e) {
			children = append(children, d.element("-", e))
		}
	}
	hidden := 0
	for _, e := range a {
		if contains(b, e) {
			hidden++
		} else {
			children = append(children, d.element("+", e))
		}
	}
	if hidden > 0 {
		children = append(children, d.hidden(hidden, "element"))
	}
	return item{sym: "~", name: name, text: "[", children: children, close: "]"}
}

// element is a list or set element marked with sym.
func (d *differ) element(sym string, v cty.Value) item {
	it := d.value(sym, v, false)
	it.sym = sym
	return withSuffix(it, ",")
}

// wholeBlocks renders every block of type name in v, all with sym.
func (d *differ) wholeBlocks(sym, name string, nb *provider.NestedBlock, v cty.Value) []item {
	switch {
	case v.IsNull():
		return nil
	case !v.IsKnown():
		return []item{{sym: sym, text: name + " = " + d.unknown()}}
	}
	var out []item
	switch nb.Nesting {
	case provider.NestingList, provider.NestingSet:
		for _, e := range elems(v) {
			out = append(out, d.blockItem(sym, name, nb.Block, e))
		}
	case provider.NestingMap:
		for _, k := range elemKeys(v) {
			out = append(out, d.blockItem(sym, name+" "+strconv.Quote(k), nb.Block, elemOrNull(v, k)))
		}
	default:
		out = append(out, d.blockItem(sym, name, nb.Block, v))
	}
	return out
}

// blockItem is "<sym> header {" with every attribute and block of v.
func (d *differ) blockItem(sym, header string, b *provider.Block, v cty.Value) item {
	if !isObject(v) {
		return item{sym: sym, text: header + " = " + d.unknown()}
	}
	return item{sym: sym, text: header + " {", children: d.objectItems(sym, b, v), close: "}"}
}

// blockDiff is "~ header {" with the changes from bv to av.
func (d *differ) blockDiff(header string, b *provider.Block, bv, av cty.Value, path string) item {
	if !isObject(bv) || !isObject(av) {
		return item{sym: "~", text: header + " = " + d.unknown()}
	}
	return item{sym: "~", text: header + " {", children: d.diffObject(b, bv, av, path, false), close: "}"}
}

// diffBlocks renders the changes of the blocks of type name, and returns
// how many are unchanged: list blocks by index, set blocks by value, map
// blocks by key.
func (d *differ) diffBlocks(name string, nb *provider.NestedBlock, bv, av cty.Value, path string) ([]item, int) {
	switch {
	case bv.IsNull() && av.IsNull():
		return nil, 0
	case !bv.IsKnown() || !av.IsKnown():
		return []item{{sym: "~", text: name + " = " + d.unknown()}}, 0
	}
	var out []item
	hidden := 0
	switch nb.Nesting {
	case provider.NestingList:
		b, a := elems(bv), elems(av)
		for i := range max(len(b), len(a)) {
			switch {
			case i >= len(b):
				out = append(out, d.blockItem("+", name, nb.Block, a[i]))
			case i >= len(a):
				out = append(out, d.blockItem("-", name, nb.Block, b[i]))
			case same(b[i], a[i]):
				hidden++
			default:
				out = append(out, d.blockDiff(name, nb.Block, b[i], a[i], fmt.Sprintf("%s[%d]", path, i)))
			}
		}
	case provider.NestingSet:
		b, a := elems(bv), elems(av)
		for _, e := range b {
			if !contains(a, e) {
				out = append(out, d.blockItem("-", name, nb.Block, e))
			}
		}
		for _, e := range a {
			if contains(b, e) {
				hidden++
			} else {
				out = append(out, d.blockItem("+", name, nb.Block, e))
			}
		}
	case provider.NestingMap:
		for _, k := range unionKeys(bv, av) {
			bk, ak := elemOrNull(bv, k), elemOrNull(av, k)
			header := name + " " + strconv.Quote(k)
			switch {
			case bk.IsNull():
				out = append(out, d.blockItem("+", header, nb.Block, ak))
			case ak.IsNull():
				out = append(out, d.blockItem("-", header, nb.Block, bk))
			case same(bk, ak):
				hidden++
			default:
				out = append(out, d.blockDiff(header, nb.Block, bk, ak, path+"["+strconv.Quote(k)+"]"))
			}
		}
	default:
		switch {
		case bv.IsNull():
			out = append(out, d.blockItem("+", name, nb.Block, av))
		case av.IsNull():
			out = append(out, d.blockItem("-", name, nb.Block, bv))
		case same(bv, av):
			hidden++
		default:
			out = append(out, d.blockDiff(name, nb.Block, bv, av, path))
		}
	}
	return out, hidden
}

// identifying are the attributes a destroy shows; the others are counted.
var identifying = []string{"arn", "id", "name"}

// destroyItems renders the identifying attributes of before, an object of
// block b, as removed, and counts the other attributes and the blocks.
func (d *differ) destroyItems(b *provider.Block, before cty.Value) []item {
	var out []item
	hiddenAttrs, hiddenBlocks := 0, 0
	for _, name := range attrNames(before) {
		v := before.GetAttr(name)
		switch nb := blockType(b, name); {
		case nb != nil:
			hiddenBlocks += countBlocks(nb, v)
		case v.IsNull():
		case slices.Contains(identifying, name):
			out = append(out, d.valueItem("-", name, v, sensitiveAttr(b, name)))
		default:
			hiddenAttrs++
		}
	}
	if hiddenAttrs > 0 {
		out = append(out, d.hidden(hiddenAttrs, "attribute"))
	}
	if hiddenBlocks > 0 {
		out = append(out, d.hidden(hiddenBlocks, "block"))
	}
	return out
}

// countBlocks is the number of blocks in v, the value of a block type.
func countBlocks(nb *provider.NestedBlock, v cty.Value) int {
	switch {
	case v.IsNull():
		return 0
	case !v.IsKnown():
		return 1
	}
	switch nb.Nesting {
	case provider.NestingList, provider.NestingSet, provider.NestingMap:
		return v.LengthInt()
	}
	return 1
}

// collectionKind is "list" (lists and tuples), "set", "map", "object", or
// "" for a primitive.
func collectionKind(v cty.Value) string {
	switch ty := v.Type(); {
	case ty.IsListType() || ty.IsTupleType():
		return "list"
	case ty.IsSetType():
		return "set"
	case ty.IsMapType():
		return "map"
	case ty.IsObjectType():
		return "object"
	}
	return ""
}

// same reports whether a and b are wholly known and equal.
func same(a, b cty.Value) bool {
	return a.IsWhollyKnown() && b.IsWhollyKnown() && a.Type().Equals(b.Type()) && sameValue(a, b)
}

func contains(vs []cty.Value, v cty.Value) bool {
	return slices.ContainsFunc(vs, func(e cty.Value) bool { return same(e, v) })
}

// elems returns the elements of a known collection; nil for null.
func elems(v cty.Value) []cty.Value {
	if v.IsNull() || !v.IsKnown() {
		return nil
	}
	var out []cty.Value
	for it := v.ElementIterator(); it.Next(); {
		_, e := it.Element()
		out = append(out, e)
	}
	return out
}

// elemKeys returns the attribute names of an object or the keys of a map,
// sorted; nil for null or unknown.
func elemKeys(v cty.Value) []string {
	if v.IsNull() || !v.IsKnown() {
		return nil
	}
	if v.Type().IsObjectType() {
		return attrNames(v)
	}
	if !v.Type().IsMapType() {
		return nil
	}
	var keys []string
	for it := v.ElementIterator(); it.Next(); {
		k, _ := it.Element()
		keys = append(keys, k.AsString())
	}
	return keys
}

// unionKeys returns the keys of a and b, sorted.
func unionKeys(a, b cty.Value) []string {
	set := map[string]bool{}
	for _, k := range elemKeys(a) {
		set[k] = true
	}
	for _, k := range elemKeys(b) {
		set[k] = true
	}
	return slices.Sorted(maps.Keys(set))
}

// elemOrNull returns the attribute or element k of an object or map; a
// dynamic null when it has none.
func elemOrNull(v cty.Value, k string) cty.Value {
	switch {
	case v.IsNull() || !v.IsKnown():
	case v.Type().IsObjectType() && v.Type().HasAttribute(k):
		return v.GetAttr(k)
	case v.Type().IsMapType():
		if key := cty.StringVal(k); v.HasIndex(key).True() {
			return v.Index(key)
		}
	}
	return cty.NullVal(cty.DynamicPseudoType)
}

// keyLabel is how the key k of v is written: bare for an object attribute,
// quoted for a map key.
func keyLabel(v cty.Value, k string) string {
	if v.Type().IsObjectType() {
		return k
	}
	return strconv.Quote(k)
}

// attrNames returns the attribute names of an object, sorted.
func attrNames(v cty.Value) []string {
	return slices.Sorted(maps.Keys(v.Type().AttributeTypes()))
}

func blockType(b *provider.Block, name string) *provider.NestedBlock {
	if b == nil {
		return nil
	}
	return b.BlockTypes[name]
}

// sensitiveAttr reports whether the attribute name of b is masked: when it
// is sensitive or write-only, and for every attribute when b is nil (the
// schema of the resource type is unknown): rendering fails closed.
func sensitiveAttr(b *provider.Block, name string) bool {
	return b == nil || b.Attributes[name] != nil && isSensitive(b.Attributes[name])
}

func joinPath(path, name string) string {
	if path == "" {
		return name
	}
	return path + "." + name
}
