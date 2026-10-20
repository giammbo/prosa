package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/giammbo/prosa/internal/checks"
	"github.com/giammbo/prosa/internal/intent"
	"github.com/giammbo/prosa/internal/lock"
)

// planInput builds the planner's input for it. view and checkView hold the
// desired resources and checks known so far of the intents in Dir (see
// desiredStates); the input keeps its own copy, so that Validate sees the
// state of the call. Mode is "plan" for an intent the lock does not have,
// "patch" otherwise, where a patch applies to the lock's state; in an
// environment derived from the base it is always "patch", on the base
// (baseInput). Validate applies a patch first: one that does not apply is a
// validation problem.
func (p *Plan) planInput(it intent.Intent, view map[string][]DesiredResource, checkView map[string][]lock.Check, lockKeys map[string][]string) PlanInput {
	others := maps.Clone(view)
	delete(others, it.Name)
	otherChecks := maps.Clone(checkView)
	delete(otherChecks, it.Name)
	in := PlanInput{
		Intent:       it,
		PreviousBody: p.states[it.Name].Body,
		Env:          p.env,
		Region:       p.region,
		Schemas:      NewSchemaCatalog(p.schema),
	}
	if body, ok := p.prevBodies[it.Name]; ok {
		in.PreviousBody = body
	}
	if p.env != nil {
		in.PreviousEnvBody = p.states[it.Name].EnvBody
	}
	prevTypes := map[string]string{}
	in.Mode = "plan"
	if li := p.lock.Intents[it.Name]; li != nil {
		in.Mode = "patch"
		in.Previous = slices.Clone(li.Resources)
		in.patchBase, in.patchChecks = in.Previous, slices.Clone(li.Checks)
		for _, r := range li.Resources {
			prevTypes[r.Key] = r.Type
		}
	}
	if p.base != nil {
		p.baseInput(&in)
	}
	for _, name := range slices.Sorted(maps.Keys(others)) {
		rs := slices.Clone(others[name])
		slices.SortFunc(rs, func(a, b DesiredResource) int { return strings.Compare(a.Key, b.Key) })
		for _, r := range rs {
			in.Available = append(in.Available, AvailableResource{Intent: name, Key: r.Key, Type: r.Type})
		}
	}
	refBy := referencedBy(it.Name, lockKeys[it.Name], others)
	checkedBy(refBy, it.Name, lockKeys[it.Name], otherChecks)
	in.Referenced = slices.Sorted(maps.Keys(refBy))
	in.Validate = func(ctx context.Context, d Desired) error {
		if err := p.checkPatch(in, d); err != nil {
			return err
		}
		ops := d.Patch
		d, err := in.resolve(d)
		if err != nil {
			return patchProblem(it.Name, err)
		}
		d = p.canonical(d)
		var named []ValidationProblem // required names not covered (ADR-030)
		if len(in.RequiredNames) > 0 {
			if ops == nil {
				ops = diffPatch(in.Base, in.BaseChecks, d)
			}
			named = uncoveredNames(it.Name, in.RequiredNames, ops, d.Resources, it.Body, p.envBody(it.Name))
		}
		err = p.validateCandidate(ctx, it, d.Resources, d.Checks, others, lockKeys, refBy, prevTypes)
		if len(named) == 0 {
			return err
		}
		var ve *ValidationError
		if errors.As(err, &ve) {
			named = append(ve.Problems, named...)
		} else if err != nil {
			return err
		}
		return newValidationError(named)
	}
	in.Canonical = p.canonical
	return in
}

// referencedBy returns, for each key of intent name (keys, as in the lock)
// that a resource of others refers to or has in its depends_on, the
// referring nodes, sorted. Invalid
// references are ignored: the plan's validation reports them.
func referencedBy(name string, keys []string, others map[string][]DesiredResource) map[string][]string {
	othersKeys := keysIn(others)
	keysOf := func(n string) []string {
		if n == name {
			return keys
		}
		return othersKeys(n)
	}
	out := map[string][]string{}
	for other, rs := range others {
		for _, r := range rs {
			node := other + "/" + r.Key
			walkStrings(r.Config, func(s string) {
				refs, _ := refsIn(s, keysOf)
				for _, ref := range refs {
					if ref.Intent == name && !slices.Contains(out[ref.Key], node) {
						out[ref.Key] = append(out[ref.Key], node)
					}
				}
			})
			for _, d := range r.DependsOn {
				n, key, _ := strings.Cut(d, "/")
				if n == name && slices.Contains(keys, key) && !slices.Contains(out[key], node) {
					out[key] = append(out[key], node)
				}
			}
		}
	}
	for k := range out {
		slices.Sort(out[k])
	}
	return out
}

// checkedBy adds to refBy, for each key of intent name (keys, as in the
// lock) that a resource_attr check of otherChecks reads, the checks, as
// "<intent>/checks[<i>]", sorted: a checked resource must stay too.
func checkedBy(refBy map[string][]string, name string, keys []string, otherChecks map[string][]lock.Check) {
	for _, other := range slices.Sorted(maps.Keys(otherChecks)) {
		for i, ck := range otherChecks[other] {
			n, key, _ := strings.Cut(ck.Resource, "/")
			if ck.Type != checks.ResourceAttr || n != name || !slices.Contains(keys, key) {
				continue
			}
			if node := fmt.Sprintf("%s/checks[%d]", other, i); !slices.Contains(refBy[key], node) {
				refBy[key] = append(refBy[key], node)
				slices.Sort(refBy[key])
			}
		}
	}
}

// walkStrings calls f on every string in v, at any depth.
func walkStrings(v any, f func(string)) {
	if s, ok := v.(string); ok {
		f(s)
		return
	}
	if v == nil {
		return
	}
	if kids, _, ok, err := children(v, ""); err == nil && ok {
		for _, c := range kids {
			walkStrings(c.value, f)
		}
	}
}

// validationPlans returns the resources of kept, the candidate of intent
// name, that validation also plans (see checkResources): each new key, from
// a null prior, and each key of the lock whose configuration differs from
// the lock's chosen, from the state the store holds for it (the refresh
// comes after the planner), one plan per key. A key whose stored state
// cannot be used (another type, unreadable, none) is left to the plan.
func (p *Plan) validationPlans(name string, kept []DesiredResource) map[string]validationPrior {
	chosen := map[string]lock.Resource{}
	if li := p.lock.Intents[name]; li != nil {
		for _, r := range li.Resources {
			chosen[r.Key] = r
		}
	}
	plans := map[string]validationPrior{}
	for _, r := range kept {
		node := name + "/" + r.Key
		lr, inLock := chosen[r.Key]
		if !inLock {
			plans[node] = validationPrior{}
			continue
		}
		if lr.Type != r.Type || sameConfig(lr.Chosen, r.Config) {
			continue
		}
		rs := p.states[name].Resources[r.Key]
		if rs == nil || rs.Type != r.Type || rs.Provider != p.providerName {
			continue
		}
		if rsch := p.schema.Resources[rs.Type]; rsch == nil || rs.SchemaVersion != rsch.Version {
			continue
		}
		stored, identity, err := p.decodeStored(rs)
		if err != nil {
			continue
		}
		plans[node] = validationPrior{state: stored, identity: identity, private: rs.Private}
	}
	return plans
}

// sameConfig reports whether two configurations are equal as JSON, where
// numbers of any Go type are digits.
func sameConfig(a, b map[string]any) bool {
	ja, errA := json.Marshal(a)
	jb, errB := json.Marshal(b)
	return errA == nil && errB == nil && bytes.Equal(ja, jb)
}

// validateCandidate checks the desired resources rs and the checks cks of
// intent it with the plan's rules, the other intents' desired state being
// others, plus the keys other intents refer to or check (refBy), which must
// stay, and the types the lock recorded (prevTypes), which a key keeps. It
// returns nil, a *ValidationError, or a transport or context error, and
// changes nothing.
func (p *Plan) validateCandidate(ctx context.Context, it intent.Intent, rs []DesiredResource, cks []lock.Check, others map[string][]DesiredResource, lockKeys map[string][]string, refBy map[string][]string, prevTypes map[string]string) error {
	name := it.Name
	var probs []ValidationProblem
	var kept []DesiredResource
	seen := map[string]bool{}
	for _, r := range rs {
		var msg string
		switch {
		case r.Key == "":
			msg = "resource with an empty key"
		case seen[r.Key]:
			msg = fmt.Sprintf("duplicate resource key %q", r.Key)
		case r.Type == "":
			msg = "resource has no type"
		}
		if msg != "" {
			probs = append(probs, ValidationProblem{Intent: name, Key: r.Key, Type: r.Type, Message: msg})
			continue
		}
		seen[r.Key] = true
		kept = append(kept, r)
		if was := prevTypes[r.Key]; was != "" && was != r.Type && p.schema.Resources[r.Type] != nil {
			msg := fmt.Sprintf("key %q was %s, cannot become %s: use a new key", r.Key, was, r.Type)
			if by := refBy[r.Key]; len(by) > 0 {
				msg = fmt.Sprintf("key %q is referred to by %s: keep the type %s, not %s", r.Key, strings.Join(by, ", "), was, r.Type)
			}
			probs = append(probs, ValidationProblem{Intent: name, Key: r.Key, Type: r.Type, Message: msg})
		}
	}
	desired := maps.Clone(others)
	desired[name] = kept
	_, refProbs, badRefs := p.desiredOrder(desired, lockKeys)
	graphBad := map[string]bool{} // nodes with a graph problem: no provider call
	for _, pr := range refProbs {
		if pr.Intent == name {
			probs = append(probs, pr)
			graphBad[name+"/"+pr.Key] = true

		}
	}
	more, err := p.checkResources(ctx, desired, []string{name}, badRefs, graphBad, p.validationPlans(name, kept))
	if err != nil {
		return err
	}
	probs = append(probs, more...)
	probs = append(probs, p.checkProblems(name, it.DependsOn, cks, desired)...)
	for _, key := range slices.Sorted(maps.Keys(refBy)) {
		by, want := strings.Join(refBy[key], ", "), prevTypes[key]
		if !slices.ContainsFunc(kept, func(r DesiredResource) bool { return r.Key == key }) {
			probs = append(probs, ValidationProblem{Intent: name, Key: key, Type: want,
				Message: fmt.Sprintf("referred to by %s: this resource must stay, with type %s", by, want)})
		}
	}
	if len(probs) == 0 {
		return nil
	}
	return newValidationError(probs)
}
