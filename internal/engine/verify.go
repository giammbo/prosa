package engine

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/zclconf/go-cty/cty"

	"github.com/giammbo/prosa/internal/checks"
	"github.com/giammbo/prosa/internal/intent"
	"github.com/giammbo/prosa/internal/lock"
	"github.com/giammbo/prosa/internal/state"
)

// CheckResult is the result of one check of an intent (ADR-021).
type CheckResult struct {
	Intent string
	checks.Result
}

// HasFailedChecks reports whether one of rs failed.
func HasFailedChecks(rs []CheckResult) bool {
	return slices.ContainsFunc(rs, func(r CheckResult) bool { return r.Status == checks.Fail })
}

// checkOptions returns e.CheckOptions with Emulated set when
// PROSA_AWS_ENDPOINT is, and Now defaulting to e.Now. The environment
// variable is a fallback for direct Engine users: the CLI sets Emulated
// from the project settings.
func (e *Engine) checkOptions() checks.Options {
	opts := e.CheckOptions
	if os.Getenv("PROSA_AWS_ENDPOINT") != "" {
		opts.Emulated = true
	}
	if opts.Now == nil {
		opts.Now = e.Now
	}
	return opts
}

// stateLookup reads the resources of p's state store from the provider, each
// at most once, starting the provider on the first read. A resource the store
// does not have, or whose remote object is gone, is a null value.
func (e *Engine) stateLookup(ctx context.Context, p *Plan) checks.StateLookup {
	read := map[string]cty.Value{}
	return func(node string) (cty.Value, error) {
		if v, ok := read[node]; ok {
			return v, nil
		}
		name, key, _ := strings.Cut(node, "/")
		st := p.states[name]
		if st == nil || st.Resources[key] == nil {
			return cty.NilVal, nil
		}
		if err := e.startProvider(ctx, p); err != nil {
			return cty.NilVal, err
		}
		rs := st.Resources[key]
		if rsch := p.schema.Resources[rs.Type]; rsch != nil && rs.SchemaVersion != rsch.Version {
			return cty.NilVal, fmt.Errorf("state was written with schema version %d, provider has %d: upgrade not supported yet", rs.SchemaVersion, rsch.Version)
		}
		c, err := p.refresh(ctx, rs)
		if err != nil {
			return cty.NilVal, err
		}
		read[node] = c.state
		return c.state, nil
	}
}

func (e *Engine) observeCheck(r CheckResult, typ string) {
	if e.Observer != nil {
		e.Observer.Check(r, typ)
	}
}

// runChecks runs, by intent, the checks of the intents that applied
// something or were rewritten (new hash, maybe new checks) and are not held,
// on the resources read again from the provider. It runs after every intent
// completed.
func (a *applier) runChecks(ctx context.Context) []CheckResult {
	var out []CheckResult
	lookup := a.e.stateLookup(ctx, a.p)
	opts := a.e.checkOptions()
	for _, it := range a.p.intents {
		if (!a.executed[it.Name] && !a.stale[it.Name]) || a.held[it.Name] {
			continue
		}
		defs := a.p.checks[it.Name]
		for i, r := range checks.Run(ctx, defs, lookup, opts) {
			out = append(out, CheckResult{Intent: it.Name, Result: r})
			a.e.observeCheck(out[len(out)-1], defs[i].Type)
		}
	}
	return out
}

// Check runs every check of the lock, by intent, on the resources of the
// state store read again from the provider (prosa check). It never plans and
// writes nothing; a state store that does not match the lock is an error.
// changed lists, sorted, the intents of the lock whose file in Dir has
// another hash: their checks are the applied ones, not those of the text.
func (e *Engine) Check(ctx context.Context) (results []CheckResult, changed []string, err error) {
	lk, _, err := lock.Read(e.LockPath)
	if err != nil {
		return nil, nil, err
	}
	intents, err := intent.Load(e.Dir, e.Ignore...)
	if err != nil {
		return nil, nil, err
	}
	for _, it := range intents {
		if li := lk.Intents[it.Name]; li != nil && li.IntentHash != e.intentHash(it) {
			changed = append(changed, it.Name)
		}
	}
	p := &Plan{lock: lk, states: map[string]*state.IntentState{}, providerName: awsProvider}
	names := slices.Sorted(maps.Keys(lk.Intents))
	for _, name := range names {
		st, digest, err := e.State.Load(name)
		if err != nil {
			return nil, nil, err
		}
		if li := lk.Intents[name]; digest != li.State.Digest {
			if aheadOfLock(li, st, digest) {
				return nil, nil, errors.New(stateMismatch(name, li, st, digest, e.recoverCommand()))
			}
			return nil, nil, fmt.Errorf("state digest mismatch for intent %q: run prosa plan", name)
		}
		p.states[name] = st
	}
	lookup := e.stateLookup(ctx, p)
	opts := e.checkOptions()
	for _, name := range names {
		defs := lk.Intents[name].Checks
		for i, r := range checks.Run(ctx, defs, lookup, opts) {
			results = append(results, CheckResult{Intent: name, Result: r})
			e.observeCheck(results[len(results)-1], defs[i].Type)
		}
	}
	return results, changed, nil
}

// allCheckProblems returns the problems of the checks of every intent in Dir
// (see checkProblems).
func (p *Plan) allCheckProblems() []ValidationProblem {
	var probs []ValidationProblem
	for _, it := range p.intents {
		probs = append(probs, p.checkProblems(it.Name, it.DependsOn, p.checks[it.Name], p.desired)...)
	}
	return probs
}
