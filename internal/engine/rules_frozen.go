package engine

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/giammbo/prosa/internal/intent"
	"github.com/giammbo/prosa/rules"
)

// Intents already applied and the rules (ADR-034). An intent whose lock
// entry is reused without planning is reviewed when the active rules that
// apply to the types of its resources are not those its state was checked
// with (the lock's rules_hash), or when --replan names it. A state that
// passes gets the new rules_hash. One that breaks a rule is planned again as
// a patch on its locked state, with the violations as problems: the fix
// enters the plan, each change marked with the rules it answers, unless it
// would replace or delete a resource; then it is dropped with a warning and
// the lock stays as it is, until --replan names the intent. A kept fix is
// applied as a rewritten intent (applier.involved). The base's
// sub-plan (planBases) does none of this.

// checkReplan returns an error for a name of replan that is not an intent
// of intents.
func checkReplan(replan []string, intents []intent.Intent) error {
	for _, name := range replan {
		if !slices.ContainsFunc(intents, func(it intent.Intent) bool { return it.Name == name }) {
			return fmt.Errorf("unknown intent %s in --replan", name)
		}
	}
	return nil
}

// applicableHash is the hash of the active rules that apply to the types of
// rs: the rules_hash of a state checked against them.
func (p *Plan) applicableHash(rs []DesiredResource) string {
	return rules.Hash(p.rules.For(typesOf(rs)))
}

// availableIn returns the resources of others, sorted by intent and key, as
// the resources an intent can refer to.
func availableIn(others map[string][]DesiredResource) []AvailableResource {
	var out []AvailableResource
	for _, name := range slices.Sorted(maps.Keys(others)) {
		rs := slices.Clone(others[name])
		slices.SortFunc(rs, func(a, b DesiredResource) int { return strings.Compare(a.Key, b.Key) })
		for _, r := range rs {
			out = append(out, AvailableResource{Intent: name, Key: r.Key, Type: r.Type})
		}
	}
	return out
}

// reviewFrozen reviews the locked state (in view, the desired state known of
// every intent) of each intent of reused, whose lock entry is taken as it
// is, when its rules_hash is not the applicable one or --replan names it;
// a state no active rule applies to passes without a call. It returns the
// intents whose state breaks a rule, to plan again with fixProblems (their
// rule IDs in ruleFix); the others are reused, with the rules_hash Apply
// writes. The error is the reviewer's failure.
func (p *Plan) reviewFrozen(ctx context.Context, reused []intent.Intent, view map[string][]DesiredResource) ([]intent.Intent, error) {
	if p.reviewer == nil || !p.reviewLocked {
		return nil, nil
	}
	var fixes []intent.Intent
	for _, it := range reused {
		li := p.lock.Intents[it.Name]
		p.reused[it.Name], p.rulesHashes[it.Name] = true, li.RulesHash
		rs := view[it.Name]
		h := p.applicableHash(rs)
		if h == li.RulesHash && !p.replan[it.Name] {
			continue
		}
		others := maps.Clone(view)
		delete(others, it.Name)
		vs, err := p.reviewState(ctx, it, rs, availableIn(others), p.exceptionsFor(it))
		if err != nil {
			return nil, err
		}
		if len(vs) == 0 {
			p.rulesHashes[it.Name] = h
			continue
		}
		delete(p.reused, it.Name)
		delete(p.rulesHashes, it.Name)
		var ids []string
		for _, v := range vs {
			ids = addSorted(ids, v.Rule)
		}
		p.ruleFix[it.Name], p.violations[it.Name] = ids, vs
		fixes = append(fixes, it)
	}
	return fixes, nil
}

// fixProblems returns the problems the planner gets for the fix of intent
// name, "<intent>/<key>: breaks <ID>: <reason>" and the hint, one per
// violation of its locked state; nil when name has no fix.
func (p *Plan) fixProblems(name string) []string {
	vs := p.violations[name]
	if vs == nil {
		return nil
	}
	var out []string
	for _, pr := range violationProblems(name, fromLock(p.lock.Intents[name]), vs) {
		out = append(out, fmt.Sprintf("%s/%s: %s", pr.Intent, pr.Key, pr.Message))
	}
	return out
}

// dropUnsafeFixes takes back to its lock every fix, among the intents not
// named by --replan, with a change (of changes, the plan's) that replaces or
// deletes a resource: its desired state, checks and patch are the lock's,
// its rules_hash stays, the violations fixed while planning it are not
// reported, and the plan warns once per key of the violations, with the
// action of that key's change, else the worst of the intent (delete). It
// reports whether it dropped one: the changes must be planned again.
func (p *Plan) dropUnsafeFixes(changes []Change) bool {
	dropped := false
	for _, name := range slices.Sorted(maps.Keys(p.ruleFix)) {
		if p.replan[name] {
			continue
		}
		var worst Action
		byKey := map[string]Action{}
		for _, c := range changes {
			if c.Intent == name && (c.Action == Replace || c.Action == Delete) {
				byKey[c.Key] = c.Action
				if worst != Delete {
					worst = c.Action
				}
			}
		}
		if worst == "" {
			continue
		}
		li := p.lock.Intents[name]
		p.desired[name], p.checks[name] = fromLock(li), li.Checks
		if p.base != nil {
			p.patches[name] = li.Patch
		}
		p.reused[name], p.rulesHashes[name] = true, li.RulesHash
		p.Rules.Fixed = slices.DeleteFunc(p.Rules.Fixed, func(f string) bool { return strings.HasSuffix(f, " in "+name) })
		ids := map[string][]string{} // key ("" when the state has none) → the rules it breaks
		for _, v := range p.violations[name] {
			key := v.Key
			if !slices.ContainsFunc(p.desired[name], func(r DesiredResource) bool { return r.Key == key }) {
				key = ""
			}
			ids[key] = addSorted(ids[key], v.Rule)
		}
		for _, key := range slices.Sorted(maps.Keys(ids)) {
			act, subject := worst, key
			if a, ok := byKey[key]; ok {
				act = a
			}
			if key == "" {
				subject = "its state"
			}
			w := fmt.Sprintf("%s: %s breaks %s; fixing it would %s the resource: run %s",
				name, subject, strings.Join(ids[key], ", "), act, p.replanCommand(name))
			if !slices.Contains(p.Warnings, w) {
				p.Warnings = append(p.Warnings, w)
			}
		}
		delete(p.ruleFix, name)
		delete(p.violations, name)
		dropped = true
	}
	return dropped
}

// replanCommand is the command that plans name again with its fix:
// "prosa plan --replan <name>", with "--env <file>" in an environment.
func (p *Plan) replanCommand(name string) string {
	if p.envFile == "" {
		return "prosa plan --replan " + name
	}
	return "prosa plan --env " + p.envFile + " --replan " + name
}

// markFixes sets, on every change of an intent whose fix the plan keeps,
// the rules it answers; such an intent rewrites its lock entry.
func (p *Plan) markFixes(changes []Change) {
	for i := range changes {
		if ids := p.ruleFix[changes[i].Intent]; ids != nil {
			changes[i].Rule = strings.Join(ids, ", ")
		}
	}
	if len(p.ruleFix) > 0 {
		p.LockDirty = true
	}
}

// settleRules decides, with a reviewer, the rules_hash Apply writes for each
// intent of Dir (rulesHashes): a reused intent keeps the one reviewFrozen
// set, and makes the lock dirty when it is new; any other intent gets the
// applicable hash of its desired state when the reviewer passed that state
// in this run, none otherwise (it is reviewed next time).
func (p *Plan) settleRules() {
	if p.reviewer == nil || !p.reviewLocked {
		return
	}
	for _, it := range p.intents {
		name := it.Name
		switch {
		case p.reused[name]:
			if p.rulesHashes[name] != p.lock.Intents[name].RulesHash {
				p.LockDirty = true
			}
		case p.reviewed[name]:
			p.rulesHashes[name] = p.applicableHash(p.desired[name])
		default:
			delete(p.rulesHashes, name)
		}
	}
}

// rulesStale reports whether intent name is reused as it is with a
// rules_hash Apply must write: its only change to the lock.
func (p *Plan) rulesStale(name string) bool {
	li := p.lock.Intents[name]
	return p.reviewer != nil && p.reused[name] && li != nil && p.rulesHashes[name] != li.RulesHash
}
