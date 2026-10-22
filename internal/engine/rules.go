package engine

import (
	"context"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

	"github.com/giammbo/prosa/internal/intent"
	"github.com/giammbo/prosa/rules"
)

// Violation is a rule a desired state breaks (ADR-034): the rule's ID, the
// key of the resource that breaks it, and why.
type Violation struct {
	Rule, Key, Reason string
}

// ReviewInput is what the reviewer sees of one intent: its name, the
// environment's name ("" without one), the desired resources, the resources
// of the other intents it can refer to, the active rules that apply to the
// types of the resources, and the exceptions the intent writes.
type ReviewInput struct {
	Intent, Env string
	Resources   []DesiredResource
	Available   []AvailableResource
	Rules       []rules.Rule
	Exceptions  []rules.Exception
}

// Reviewer checks a desired state against the rules (ADR-034). A failure
// (transport, refusal, malformed answer) is an error: the plan fails, as it
// does when the planner fails.
type Reviewer interface {
	Review(ctx context.Context, in ReviewInput) ([]Violation, error)
}

// RulesSummary is what the plan says about the rules. Checked is false
// without a reviewer: nothing else is set then. Excepted and Fixed hold
// "<ID> in <intent>", sorted and without duplicates: the exceptions the
// intents write, and the violations the planner fixed in this run.
type RulesSummary struct {
	Checked  bool
	Active   int
	Disabled []rules.DisabledRule
	Excepted []string
	Fixed    []string
}

// ruleHint is appended to each violation the planner is told about, so that
// a planner that cannot fix it ends with both ways out in its last problems.
const ruleHint = " (to keep it: --disable-rule %s, or except it in the intent)"

// summarizeRules fills p.Rules from the rule set of the run: the counts, the
// disabled rules and, when a reviewer checks the plan, the exceptions of
// every intent in Dir (which also warns about the unknown rules they name).
func (p *Plan) summarizeRules() {
	if p.reviewer == nil {
		return
	}
	p.Rules.Checked, p.Rules.Active, p.Rules.Disabled = true, len(p.rules.Active), p.rules.Disabled
	for _, it := range p.intents {
		for _, ex := range p.exceptionsFor(it) {
			p.Rules.Excepted = addSorted(p.Rules.Excepted, ex.ID+" in "+it.Name)
		}
	}
}

// exceptionsFor returns the exceptions of it: the paragraphs of its text,
// then of its section of the environment, that name an active rule. An ID
// that names no rule is a warning of the plan (once); one of a disabled
// rule is ignored.
func (p *Plan) exceptionsFor(it intent.Intent) []rules.Exception {
	text := it.Body
	if p.env != nil {
		if s := p.env.Section(it.Name); s != "" {
			text += "\n\n" + s
		}
	}
	ex, unknown := rules.Exceptions(text, p.rules.IDs())
	for _, id := range unknown {
		if slices.ContainsFunc(p.rules.Disabled, func(d rules.DisabledRule) bool { return d.ID == id }) {
			continue
		}
		if w := fmt.Sprintf("unknown rule %s named in %s", id, it.Name); !slices.Contains(p.Warnings, w) {
			p.Warnings = append(p.Warnings, w)
		}
	}
	return ex
}

// review asks the reviewer about rs, the desired state of it that passed
// validation, with the rules that apply to its types: nil, a
// *ValidationError with one problem per violation, or the reviewer's
// failure. The rule IDs of the violations go to violated; once a review
// finds none, they are the violations fixed while planning. A desired
// state that no rule applies to is not sent. A state that passes is marked
// reviewed (the rules_hash Apply writes, settleRules).
func (p *Plan) review(ctx context.Context, it intent.Intent, rs []DesiredResource, available []AvailableResource, exceptions []rules.Exception, violated map[string]bool) error {
	vs, err := p.reviewState(ctx, it, rs, available, exceptions)
	if err != nil {
		return err
	}
	if len(vs) == 0 {
		for _, id := range slices.Sorted(maps.Keys(violated)) {
			p.Rules.Fixed = addSorted(p.Rules.Fixed, id+" in "+it.Name)
		}
		clear(violated)
		if p.reviewed == nil {
			p.reviewed = map[string]bool{}
		}
		p.reviewed[it.Name] = true
		return nil
	}
	for _, v := range vs {
		violated[v.Rule] = true
	}
	return newValidationError(violationProblems(it.Name, rs, vs))
}

// reviewState asks the reviewer about rs, a desired state of it, with the
// active rules that apply to its types: the violations, nil when none or
// when no rule applies (the reviewer is not asked), or the reviewer's
// failure.
func (p *Plan) reviewState(ctx context.Context, it intent.Intent, rs []DesiredResource, available []AvailableResource, exceptions []rules.Exception) ([]Violation, error) {
	applicable := p.rules.For(typesOf(rs))
	if len(applicable) == 0 {
		return nil, nil
	}
	in := ReviewInput{Intent: it.Name, Resources: rs, Available: available, Rules: applicable, Exceptions: exceptions}
	if p.env != nil {
		in.Env = p.env.Name
	}
	vs, err := p.reviewer.Review(ctx, in)
	if err != nil {
		return nil, fmt.Errorf("review the rules of intent %q: %w", it.Name, err)
	}
	return vs, nil
}

// violationProblems returns one problem per violation of rs, the desired
// state of intent name: "breaks <ID>: <reason>" and the hint; the key is ""
// when rs has no such key.
func violationProblems(name string, rs []DesiredResource, vs []Violation) []ValidationProblem {
	types := map[string]string{} // key → type
	for _, r := range rs {
		types[r.Key] = r.Type
	}
	probs := make([]ValidationProblem, 0, len(vs))
	for _, v := range vs {
		pr := ValidationProblem{Intent: name, Key: v.Key, Type: types[v.Key],
			Message: fmt.Sprintf("breaks %s: %s"+ruleHint, v.Rule, v.Reason, v.Rule)}
		if _, ok := types[v.Key]; !ok {
			pr.Key = ""
		}
		probs = append(probs, pr)
	}
	return probs
}

// typesOf returns the types of rs, in order of first appearance.
func typesOf(rs []DesiredResource) []string {
	var out []string
	for _, r := range rs {
		if !slices.Contains(out, r.Type) {
			out = append(out, r.Type)
		}
	}
	return out
}

// mergeRules adds to p what the review of another plan of the run (the
// base's, planBases) found: the violations fixed and the warnings.
func (p *Plan) mergeRules(o *Plan) {
	for _, f := range o.Rules.Fixed {
		p.Rules.Fixed = addSorted(p.Rules.Fixed, f)
	}
	for _, w := range o.Warnings {
		if !slices.Contains(p.Warnings, w) {
			p.Warnings = append(p.Warnings, w)
		}
	}
}

// addSorted inserts s into the sorted list, unless it is there already.
func addSorted(list []string, s string) []string {
	i, found := slices.BinarySearch(list, s)
	if found {
		return list
	}
	return slices.Insert(list, i, s)
}

// rulesCatalog is the schema catalog the planner sees when a reviewer checks
// the plan: the description of a resource type ends with the active rules
// that apply to it. A nested block ("<type>.<block>") gets none: the rules
// come once, with the type.
type rulesCatalog struct {
	SchemaCatalog
	set rules.Set
}

func (c rulesCatalog) Describe(name string) (string, error) {
	text, err := c.SchemaCatalog.Describe(name)
	if err != nil || strings.Contains(name, ".") {
		return text, err
	}
	rs := c.set.For([]string{name})
	if len(rs) == 0 {
		return text, nil
	}
	var b strings.Builder
	b.WriteString(strings.TrimRight(text, "\n"))
	fmt.Fprintf(&b, "\n\nRules for %s (binding; a reviewer checks them):\n", name)
	for _, r := range rs {
		var lines []string
		for _, l := range strings.Split(r.Body, "\n") {
			if l = strings.TrimSpace(l); l != "" {
				lines = append(lines, l)
			}
		}
		fmt.Fprintf(&b, "- %s %s: %s\n", r.ID, r.Title, strings.Join(lines, " "))
	}
	return b.String(), nil
}

// renderRules writes the rules lines that head a plan, then a blank line:
// "rules: not checked (no model)" without a reviewer; otherwise the active
// and disabled rules, one line per exception, and the violations fixed while
// planning.
func renderRules(w io.Writer, r RulesSummary) {
	if !r.Checked {
		fmt.Fprint(w, "rules: not checked (no model)\n\n")
		return
	}
	line := fmt.Sprintf("rules: %d active", r.Active)
	if len(r.Disabled) > 0 {
		var ds []string
		for _, d := range r.Disabled {
			ds = append(ds, d.ID+" by "+d.By)
		}
		line += fmt.Sprintf(", %d disabled (%s)", len(r.Disabled), strings.Join(ds, ", "))
	}
	fmt.Fprintln(w, line)
	for _, e := range r.Excepted {
		id, name, _ := strings.Cut(e, " in ")
		fmt.Fprintf(w, "rules: %s excepted in %s\n", id, name)
	}
	if n := len(r.Fixed); n > 0 {
		noun := "violations"
		if n == 1 {
			noun = "violation"
		}
		fmt.Fprintf(w, "rules: %d %s fixed while planning (%s)\n", n, noun, strings.Join(r.Fixed, ", "))
	}
	fmt.Fprintln(w)
}

// renderWarnings writes "Warning: <text>" for each warning of the plan.
func renderWarnings(w io.Writer, p *Plan) {
	for _, s := range p.Warnings {
		fmt.Fprintf(w, "Warning: %s\n", s)
	}
}
