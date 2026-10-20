package engine

import (
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/zclconf/go-cty/cty"

	"github.com/giammbo/prosa/internal/provider"
)

// RenderOptions are the options of Render and RenderApproval.
type RenderOptions struct {
	// Color turns on ANSI colors (the CLI decides: a terminal, no
	// --no-color, no NO_COLOR).
	Color bool
}

var actionSymbols = map[Action]string{Create: "+", Update: "~", Replace: "-/+", Delete: "-"}

var actionHeaders = map[Action]string{
	Create: "will be created", Update: "will be updated in-place", Replace: "must be replaced", Delete: "will be destroyed",
}

// Render writes the plan for a human, in Terraform's shape: "Blocked:" and
// one reason per line when intents are blocked; "No changes. ..." when
// nothing changes; otherwise one block per change that is not a no-op (its
// header, its gate and drift as comments, the resource with the attributes
// that change, see changeBody), then "Plan: N to add, M to change, K to
// destroy." counting the changes that are not refused (a replace is one add
// and one destroy). The refused changes follow under "Not applied:" with
// their reason, then, for each advise intent, how to apply its changes
// (ADR-022). Sensitive and write-only values are masked at any depth. The
// output is byte-stable for the same plan.
func Render(w io.Writer, p *Plan, opts RenderOptions) {
	st := style{opts.Color}
	if p.Blocked != "" {
		fmt.Fprintf(w, "Blocked: %s\n", p.Blocked)
		return
	}
	if len(p.Blockers) > 0 {
		fmt.Fprintln(w, "Blocked:")
		for _, b := range p.Blockers {
			fmt.Fprintf(w, "  %s\n", b)
		}
		if !p.HasChanges() {
			return
		}
		fmt.Fprintln(w)
	} else if !p.HasChanges() {
		fmt.Fprintln(w, "No changes. Your infrastructure matches the intents.")
		return
	}
	fmt.Fprint(w, "Prosa will perform the following actions:\n\n")
	var add, change, destroy int
	for _, c := range p.Changes {
		if _, ok := actionSymbols[c.Action]; !ok {
			continue
		}
		renderChange(w, st, p.schema, c)
		fmt.Fprintln(w)
		if c.Gate == GateRefused {
			continue
		}
		switch c.Action {
		case Create:
			add++
		case Update:
			change++
		case Delete:
			destroy++
		case Replace:
			add++
			destroy++
		}
	}
	fmt.Fprintf(w, "%s %d to add, %d to change, %d to destroy.\n", st.paint(ansiBold, "Plan:"), add, change, destroy)
	RenderNotApplied(w, p)
	renderAdvice(w, p)
}

// renderChange writes the block of one change: "# <intent>/<key> will be
// ...", the gate and drift comments, then the resource and its body.
func renderChange(w io.Writer, st style, s *provider.Schema, c Change) {
	fmt.Fprintf(w, "  %s\n", st.paint(ansiBold, "# "+c.Intent+"/"+c.Key+" "+actionHeaders[c.Action]))
	for _, comment := range changeComments(c) {
		fmt.Fprintf(w, "  # (%s)\n", comment)
	}
	var block *provider.Block
	if s != nil && s.Resources[c.Type] != nil {
		block = s.Resources[c.Type].Block
	}
	writeItems(w, st, []item{{
		sym:      actionSymbols[c.Action],
		text:     st.paint(ansiBold, fmt.Sprintf("resource %q %q {", c.Type, c.Key)),
		children: changeBody(st, block, c),
		close:    "}",
	}}, 0)
}

// changeComments are the gate and drift of c, as the comments under its
// header.
func changeComments(c Change) []string {
	var out []string
	switch {
	case c.Gate == GateNeedsApproval && c.GateReason != "":
		out = append(out, "needs approval: "+c.GateReason)
	case c.Gate == GateNeedsApproval:
		out = append(out, "needs approval")
	case c.Gate == GateRefused && c.GateReason != "":
		out = append(out, "not applied: "+c.GateReason)
	case c.Gate == GateRefused:
		out = append(out, "not applied")
	}
	switch {
	case c.Drifted && c.Before != cty.NilVal && c.Before.IsNull():
		out = append(out, "drifted outside Prosa: the object is gone")
	case c.Drifted && len(c.DriftAttrs) > 0:
		out = append(out, "drifted outside Prosa: "+strings.Join(c.DriftAttrs, ", "))
	}
	return out
}

// changeBody is what the resource line of c holds (every value masked when
// b, the schema of its type, is nil): every attribute of a
// create; for an update or a replace, the id as context and the attributes
// and blocks that change (those forcing the replacement marked), the others
// counted as hidden; for a destroy, the identifying attributes (id, name,
// arn), the others counted as hidden.
func changeBody(st style, b *provider.Block, c Change) []item {
	d := &differ{st: st, replace: map[string]bool{}}
	for _, path := range c.RequiresReplace {
		d.replace[path] = true
	}
	switch c.Action {
	case Create:
		if isObject(c.After) {
			return d.objectItems("+", b, c.After)
		}
	case Update, Replace:
		if isObject(c.Before) && isObject(c.After) {
			return d.diffObject(b, c.Before, c.After, "", true)
		}
	case Delete:
		if isObject(c.Before) {
			return d.destroyItems(b, c.Before)
		}
	}
	return nil
}

func isObject(v cty.Value) bool {
	return v != cty.NilVal && v.IsKnown() && !v.IsNull() && v.Type().IsObjectType()
}

// RenderNotApplied writes "Not applied:" and "<intent>/<key>: <reason>" for
// each refused change; nothing when no change is refused.
func RenderNotApplied(w io.Writer, p *Plan) {
	first := true
	for _, c := range p.Changes {
		if c.Action == NoOp || c.Gate != GateRefused {
			continue
		}
		if first {
			fmt.Fprintln(w, "Not applied:")
			first = false
		}
		fmt.Fprintf(w, "  %s/%s: %s\n", c.Intent, c.Key, c.GateReason)
	}
}

// renderAdvice writes, for each advise intent with a refused change, the
// command that applies its changes once its autonomy is approve.
func renderAdvice(w io.Writer, p *Plan) {
	var advise []string
	for _, c := range p.Changes {
		if c.Action != NoOp && c.Gate == GateRefused && c.GateReason == "advise" && !slices.Contains(advise, c.Intent) {
			advise = append(advise, c.Intent)
		}
	}
	for _, name := range advise {
		fmt.Fprintf(w, "advise: to apply %s, set \"autonomy: approve\" in %s.md and run: prosa plan %s, then prosa apply %s\n", name, name, p.dir, p.dir)
	}
}

// RenderApproval writes "Needs approval:" and one line per change that needs
// an approval, with the reason when there is one.
func RenderApproval(w io.Writer, p *Plan, opts RenderOptions) {
	st := style{opts.Color}
	fmt.Fprintln(w, "Needs approval:")
	for _, c := range p.ToApprove() {
		line := fmt.Sprintf("  %s %s/%s (%s)", st.symbol(actionSymbols[c.Action]), c.Intent, c.Key, c.Type)
		if c.GateReason != "" {
			line += ": " + c.GateReason
		}
		fmt.Fprintln(w, line)
	}
}
