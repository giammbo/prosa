package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"

	"github.com/giammbo/prosa/internal/intent"
	"github.com/giammbo/prosa/internal/lock"
	"github.com/giammbo/prosa/internal/state"
)

// The base is planned once and every environment derives from it by a patch
// (ADR-030).

// requiredNameAttrs are the attributes that hold a resource's name: when its
// schema requires one, the environment's patch sets it (ADR-030).
var requiredNameAttrs = []string{"name", "family", "function_name"}

// baseMode reports whether the environment derives from the base lock.
func (e *Engine) baseMode() bool { return e.Env != nil && e.BaseLockPath != "" }

// loadBase reads the base lock into p.
func (e *Engine) loadBase(p *Plan) error {
	if filepath.Clean(e.BaseLockPath) == filepath.Clean(e.LockPath) {
		return fmt.Errorf("the lock of environment %q cannot be the base lock %s", e.Env.Name, e.BaseLockPath)
	}
	lk, data, err := lock.Read(e.BaseLockPath)
	if err != nil {
		return err
	}
	p.base, p.baseBytes, p.baseWrite = lk, data, map[string]bool{}
	p.baseHashes, p.patches = map[string]string{}, map[string][]lock.PatchOp{}
	return nil
}

// upToDate reports whether li, the lock entry of it, is what this plan
// wants: the same intent_hash and, in an environment derived from the base,
// the same base_hash.
func (p *Plan) upToDate(it intent.Intent, li *lock.Intent) bool {
	return li != nil && li.IntentHash == p.intentHash(it) && (p.base == nil || li.BaseHash == p.baseHashes[it.Name])
}

// envDesiredStates is desiredStates for an environment derived from the base.
// The bases that are missing or stale are planned first (planBases); then,
// in planning order, each intent of Dir whose environment lock is not up to
// date is decided without a call when it can be: the interrupted apply is
// resumed; with the environment's part unchanged the saved patch, when it
// sets every required name of the new base, is applied again to it and
// validated; with no environment's part and no
// required name the state is the base, with an empty patch. Otherwise the
// planner is asked for a patch (Mode "patch"), with the old patch and, when
// it no longer applies, the problems.
func (e *Engine) envDesiredStates(ctx context.Context, p *Plan, lockKeys map[string][]string) (map[string]bool, error) {
	blocked, err := e.planBases(ctx, p)
	if err != nil {
		return nil, err
	}
	view := map[string][]DesiredResource{}
	checkView := map[string][]lock.Check{}
	var toDecide []intent.Intent
	for _, it := range p.intents {
		p.baseHashes[it.Name] = it.Hash
		li := p.lock.Intents[it.Name]
		if li != nil {
			view[it.Name], checkView[it.Name] = fromLock(li), li.Checks
		}
		if !blocked[it.Name] && p.upToDate(it, li) {
			p.desired[it.Name], p.checks[it.Name] = view[it.Name], checkView[it.Name]
			p.patches[it.Name] = li.Patch
		} else {
			toDecide = append(toDecide, it)
		}
	}
	if len(toDecide) > 0 {
		if err := e.startProvider(ctx, p); err != nil {
			return nil, err
		}
		p.LockDirty = true
	}
	for _, it := range planningOrder(p.intents, toDecide) {
		if blocked[it.Name] {
			p.desired[it.Name], p.checks[it.Name] = view[it.Name], checkView[it.Name]
			continue
		}
		d, err := e.deriveEnv(ctx, p, it, p.planInput(it, view, checkView, lockKeys))
		if err != nil {
			return nil, fmt.Errorf("plan intent %q: %w", it.Name, err)
		}
		if len(d.Blockers) > 0 {
			blocked[it.Name] = true
			for _, b := range d.Blockers {
				p.Blockers = append(p.Blockers, it.Name+": "+b)
			}
			d.Resources, d.Checks = view[it.Name], checkView[it.Name]
		}
		view[it.Name], checkView[it.Name] = d.Resources, d.Checks
		p.desired[it.Name], p.checks[it.Name] = d.Resources, d.Checks
	}
	return blocked, p.settleDesired()
}

// planBases plans, as `prosa plan` without an environment would, the base
// of every intent of Dir that the base lock lacks or has with another
// intent_hash, unless the environment's lock entry of the intent is up to
// date (its base is not needed). The base is planned with the base text as
// the previous text (else the root state's), and kept in p.base. Apply
// writes it to the base lock (saveBase) only for an intent the root
// workspace has not applied (no root state): otherwise the root's lock
// entry, with its edges and its pending apply, stays as the root left it,
// and the base lives in this plan only; the planner cache answers the next
// run. It returns the intents whose base is blocked; their blockers are
// added to p.
func (e *Engine) planBases(ctx context.Context, p *Plan) (map[string]bool, error) {
	var stale []intent.Intent
	keep := map[string]bool{}
	for _, it := range p.intents {
		li := p.lock.Intents[it.Name]
		envFresh := li != nil && li.IntentHash == p.intentHash(it) && li.BaseHash == it.Hash
		if bl := p.base.Intents[it.Name]; (bl == nil || bl.IntentHash != it.Hash) && !envFresh {
			stale = append(stale, it)
		} else {
			keep[it.Name] = true
		}
	}
	if len(stale) == 0 {
		return map[string]bool{}, nil
	}
	if e.Planner != nil {
		if err := e.startProvider(ctx, p); err != nil {
			return nil, err
		}
	}
	bp := &Plan{
		dir: p.dir, region: p.region, lock: p.base, lockBytes: p.baseBytes, intents: p.intents,
		desired: map[string][]DesiredResource{}, checks: map[string][]lock.Check{},
		states: map[string]*state.IntentState{}, digests: map[string]string{},
		asked: map[string]bool{}, resumed: map[string]bool{},
		prevBodies: map[string]string{}, keep: keep,
		providerName: p.providerName, providerVersion: p.providerVersion, prov: p.prov, schema: p.schema,
	}
	for _, it := range p.intents {
		bp.states[it.Name] = &state.IntentState{}
	}
	rootApplied := map[string]bool{}
	for _, it := range stale {
		body, ok, err := e.readBaseText(it.Name)
		if err != nil {
			return nil, err
		}
		if e.BaseState != nil {
			st, digest, err := e.BaseState.Load(it.Name)
			if err != nil {
				return nil, fmt.Errorf("base state of %s: %w", it.Name, err)
			}
			rootApplied[it.Name] = digest != ""
			if !ok {
				body = st.Body
			}
		}
		bp.prevBodies[it.Name] = body
	}
	blocked, err := e.planDesired(ctx, bp, lockKeysOf(p.base))
	if err != nil {
		return nil, fmt.Errorf("base: %w", err)
	}
	p.Blockers = append(p.Blockers, bp.Blockers...)
	for name := range bp.asked {
		p.asked[name] = true
	}
	for _, it := range stale {
		if !blocked[it.Name] {
			p.setBase(it, bp.desired[it.Name], bp.checks[it.Name], !rootApplied[it.Name])
		}
	}
	return blocked, nil
}

// setBase records in p.base the base planned for it, as choices: the
// intent_hash, depends_on, resources and checks; write says that Apply
// writes it to the base lock. The id of a resource and the state reference
// belong to the root's applied workspace: an existing entry keeps them (an
// id only for a key of the same type), a new one has none.
func (p *Plan) setBase(it intent.Intent, rs []DesiredResource, cks []lock.Check, write bool) {
	old := p.base.Intents[it.Name]
	li := &lock.Intent{IntentHash: it.Hash, DependsOn: it.DependsOn, Checks: cks, Resources: []lock.Resource{}}
	ids := map[string]lock.Resource{}
	if old != nil {
		li.Recipe, li.State, li.ReconciledAt = old.Recipe, old.State, old.ReconciledAt
		for _, r := range old.Resources {
			ids[r.Key] = r
		}
	}
	for _, r := range rs {
		lr := lock.Resource{Key: r.Key, Provider: p.providerName, Type: r.Type, Chosen: r.Config, DependsOn: r.DependsOn}
		if o, ok := ids[r.Key]; ok && o.Type == r.Type {
			lr.ID = o.ID
		}
		li.Resources = append(li.Resources, lr)
	}
	p.base.Intents[it.Name] = li
	if write {
		p.baseWrite[it.Name] = true
	}
}

// baseInput sets in in, the input of an intent of an environment derived
// from the base, the base it derives from (Base, BaseChecks, what a patch
// applies to), Mode "patch" and the required names of the base.
func (p *Plan) baseInput(in *PlanInput) {
	in.Mode = "patch"
	in.Base, in.BaseChecks = []lock.Resource{}, nil
	if bl := p.base.Intents[in.Intent.Name]; bl != nil {
		in.Base, in.BaseChecks = slices.Clone(bl.Resources), slices.Clone(bl.Checks)
	}
	in.patchBase, in.patchChecks = in.Base, in.BaseChecks
	in.RequiredNames = p.requiredNames(in.Base)
}

// requiredNames returns, sorted, "<key>.<attribute>" for each resource of rs
// whose schema requires one of requiredNameAttrs.
func (p *Plan) requiredNames(rs []lock.Resource) []string {
	var out []string
	for _, r := range rs {
		rsch := p.schema.Resources[r.Type]
		if rsch == nil {
			continue
		}
		for _, a := range requiredNameAttrs {
			if at := rsch.Block.Attributes[a]; at != nil && at.Required {
				out = append(out, r.Key+"."+a)
			}
		}
	}
	slices.Sort(out)
	return out
}

// deriveEnv returns the desired state of it in the environment, whose lock
// entry is not up to date (see envDesiredStates), and records in p the patch
// that derives it from the base.
func (e *Engine) deriveEnv(ctx context.Context, p *Plan, it intent.Intent, in PlanInput) (Desired, error) {
	if !e.NoResume {
		d, resumed, err := p.resume(ctx, it, in)
		if err != nil {
			return Desired{}, err
		}
		if resumed {
			p.resumed[it.Name] = true
			d = p.canonical(d)
			p.patches[it.Name] = d.Patch
			if d.Patch == nil {
				p.patches[it.Name] = diffPatch(in.Base, in.BaseChecks, d)
			}
			return d, nil
		}
	}
	el := p.lock.Intents[it.Name]
	if el != nil {
		in.OldPatch = el.Patch
	}
	switch {
	case el != nil && el.BaseHash != "" && el.IntentHash == IntentHash(intent.Intent{Name: it.Name, Hash: el.BaseHash}, p.env):
		// The environment's part is unchanged, the base is not: the saved
		// patch applies again if it does not replace checks that changed and
		// its result validates, required names covered included.
		problems := staleChecks(el.Patch, in.PreviousBody, it.Body)
		var d Desired
		if problems == nil {
			var err error
			if d, problems, err = p.reapply(ctx, in, el.Patch); err != nil {
				return Desired{}, err
			}
		}
		if problems == nil {
			p.patches[it.Name] = el.Patch
			return d, nil
		}
		in.Problems = problems
	case p.envBody(it.Name) == "" && len(in.RequiredNames) == 0:
		p.patches[it.Name] = nil
		return Desired{Resources: desiredOf(in.Base), Checks: in.BaseChecks}, nil
	}
	if e.Planner == nil {
		return Desired{}, errors.New("needs a patch but no planner is configured")
	}
	d, err := e.Planner.Plan(ctx, in)
	if err != nil {
		return Desired{}, err
	}
	p.asked[it.Name] = true
	if len(d.Blockers) > 0 {
		return d, nil
	}
	// A Patch, alone or with the state it gave (a planner that resolves it),
	// is how the answer derives from the base; a complete state without one
	// is compared with the base.
	patchOnly := d.Patch != nil
	if err := p.checkPatch(in, d); err != nil {
		return Desired{}, err
	}
	if d, err = in.resolve(d); err != nil {
		return Desired{}, err
	}
	d = p.canonical(d)
	if patchOnly {
		p.patches[it.Name] = d.Patch
	} else {
		p.patches[it.Name] = diffPatch(in.Base, in.BaseChecks, d)
	}
	return d, nil
}

// uncoveredNames returns a problem for each required name of the base
// ("<key>.<attribute>") that is not covered (ADR-030): ops does not set it
// and its value in rs, the state the patch resolves to, does not appear
// literally in any of texts (the intent's text, the environment's part),
// which would dictate it. nil when all are covered.
func uncoveredNames(name string, required []string, ops []lock.PatchOp, rs []DesiredResource, texts ...string) []ValidationProblem {
	var out []ValidationProblem
	for _, rn := range required {
		if slices.ContainsFunc(ops, func(op lock.PatchOp) bool { return op.Op == "set" && op.Key+"."+op.Path == rn }) {
			continue
		}
		cut := strings.LastIndex(rn, ".")
		key, attr := rn[:cut], rn[cut+1:]
		if v, _ := configOf(rs, key)[attr].(string); v != "" && slices.ContainsFunc(texts, func(t string) bool { return strings.Contains(t, v) }) {
			continue
		}
		out = append(out, ValidationProblem{Intent: name, Key: key, Message: fmt.Sprintf(
			"required name %s is not set by the patch: set it explicitly, to a name for the environment or to the name the text states", rn)})
	}
	return out
}

// configOf returns the config of the resource key in rs; nil when absent.
func configOf(rs []DesiredResource, key string) map[string]any {
	for _, r := range rs {
		if r.Key == key {
			return r.Config
		}
	}
	return nil
}

// staleChecks returns a problem when the saved patch replaces all the checks
// (a "checks" operation) and the "Checks" section of the intent changed
// since the patch was made: the patch would bring back the old checks over
// the new ones. nil otherwise.
func staleChecks(ops []lock.PatchOp, previousBody, body string) []string {
	if !slices.ContainsFunc(ops, func(op lock.PatchOp) bool { return op.Op == "checks" }) ||
		checksSection(previousBody) == checksSection(body) {
		return nil
	}
	return []string{"the patch replaces all the checks, and the Checks section of the intent changed: write the patch again, with the checks of the new base"}
}

// checksSection is the text of the "## Checks" section of an intent's body
// ("## Verifiche", its older name, is accepted too), up to the next "## "
// heading; "" when it has none.
func checksSection(body string) string {
	var out []string
	in := false
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "## ") {
			h := strings.TrimSpace(strings.TrimPrefix(line, "## "))
			in = h == "Checks" || h == "Verifiche"
			continue
		}
		if in {
			out = append(out, line)
		}
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// reapply applies ops, the saved patch, to the new base of in and validates
// the result. A patch that does not apply, or a result with validation
// problems, is not used: problems says why (never a partially applied
// patch). The error is a transport or context failure of the validation.
func (p *Plan) reapply(ctx context.Context, in PlanInput, ops []lock.PatchOp) (Desired, []string, error) {
	rs, cks, err := lock.ApplyPatch(in.Base, in.BaseChecks, ops)
	if err != nil {
		return Desired{}, []string{err.Error()}, nil
	}
	d := p.canonical(Desired{Resources: desiredOf(rs), Checks: cks})
	var ve *ValidationError
	switch err := in.Validate(ctx, d); {
	case err == nil:
		return d, nil, nil
	case errors.As(err, &ve):
		problems := []string{}
		for _, pr := range ve.Problems {
			problems = append(problems, fmt.Sprintf("%s/%s: %s", pr.Intent, pr.Key, pr.Message))
		}
		if len(problems) == 0 {
			problems = append(problems, ve.Error())
		}
		return Desired{}, problems, nil
	default:
		return Desired{}, nil, err
	}
}

// resolve returns d with its Patch applied to what the input says a patch
// applies to, when d is only a patch (Patch set, Resources nil): the
// resources and checks that result, with the Patch kept. Any other d is
// returned as it is. A patch that does not apply is a *lock.PatchError.
func (in PlanInput) resolve(d Desired) (Desired, error) {
	if d.Patch == nil || d.Resources != nil || len(d.Blockers) > 0 {
		return d, nil
	}
	base, checks := in.applyBase()
	rs, cks, err := lock.ApplyPatch(base, checks, d.Patch)
	if err != nil {
		return Desired{}, err
	}
	return Desired{Resources: desiredOf(rs), Checks: cks, Patch: d.Patch}, nil
}

// applyBase returns what a patch applies to: the engine's patchBase, else,
// for an input built by hand, Base, else Previous.
func (in PlanInput) applyBase() ([]lock.Resource, []lock.Check) {
	if in.patchBase != nil {
		return in.patchBase, in.patchChecks
	}
	if in.Base != nil {
		return in.Base, in.BaseChecks
	}
	return in.Previous, nil
}

// checkPatch returns a *ValidationError when the Patch of d, the answer for
// the input in, has an op with a sensitive literal (patchSensitive); nil
// otherwise, and for an answer without a Patch.
func (p *Plan) checkPatch(in PlanInput, d Desired) error {
	if d.Patch == nil {
		return nil
	}
	base, _ := in.applyBase()
	if probs := p.patchSensitive(in.Intent.Name, base, d.Patch); len(probs) > 0 {
		return newValidationError(probs)
	}
	return nil
}

// Resolve is resolve for a planner: it applies the Patch of d, when d is only
// a patch, to the state the input shows (Base in an environment, else
// Previous) and returns the resulting desired state with the Patch kept; any
// other d is returned as it is. A patch that does not apply is a
// *lock.PatchError.
func (in PlanInput) Resolve(d Desired) (Desired, error) { return in.resolve(d) }

// patchProblem returns the validation error of a patch that does not apply.
func patchProblem(name string, err error) error {
	var pe *lock.PatchError
	if !errors.As(err, &pe) {
		return err
	}
	return newValidationError([]ValidationProblem{{Intent: name, Key: pe.Op.Key, Message: pe.Error()}})
}

// desiredOf returns the desired resources a lock records; never nil.
func desiredOf(rs []lock.Resource) []DesiredResource {
	out := make([]DesiredResource, len(rs))
	for i, r := range rs {
		out[i] = DesiredResource{Key: r.Key, Type: r.Type, Config: r.Chosen, DependsOn: r.DependsOn}
	}
	return out
}

// diffPatch returns a patch that turns the base (resources and checks) into
// d, for a planner that answered with a complete desired state: remove for
// the keys d drops (and those whose type changes), add for the new ones,
// set and unset of the top-level attributes that differ, depends_on, and the
// complete checks when they differ.
func diffPatch(base []lock.Resource, baseChecks []lock.Check, d Desired) []lock.PatchOp {
	byKey := map[string]lock.Resource{}
	for _, r := range base {
		byKey[r.Key] = r
	}
	rs := slices.Clone(d.Resources)
	slices.SortFunc(rs, func(a, b DesiredResource) int { return strings.Compare(a.Key, b.Key) })
	want := map[string]bool{}
	for _, r := range rs {
		want[r.Key] = true
	}
	var ops []lock.PatchOp
	for _, key := range slices.Sorted(maps.Keys(byKey)) {
		if !want[key] {
			ops = append(ops, lock.PatchOp{Op: "remove", Key: key})
		}
	}
	for _, r := range rs {
		b, ok := byKey[r.Key]
		if ok && b.Type != r.Type {
			ops = append(ops, lock.PatchOp{Op: "remove", Key: r.Key})
			ok = false
		}
		if !ok {
			ops = append(ops, lock.PatchOp{Op: "add", Key: r.Key, Type: r.Type, Config: r.Config, DependsOn: r.DependsOn})
			continue
		}
		attrs := map[string]bool{}
		for k := range b.Chosen {
			attrs[k] = true
		}
		for k := range r.Config {
			attrs[k] = true
		}
		for _, k := range slices.Sorted(maps.Keys(attrs)) {
			nv, inNew := r.Config[k]
			ov, inOld := b.Chosen[k]
			switch {
			case !inNew:
				ops = append(ops, lock.PatchOp{Op: "unset", Key: r.Key, Path: k})
			case !inOld || !jsonEqual(ov, nv):
				ops = append(ops, lock.PatchOp{Op: "set", Key: r.Key, Path: k, Value: nv})
			}
		}
		if !slices.Equal(b.DependsOn, r.DependsOn) {
			var deps any
			if r.DependsOn != nil {
				deps = generic(r.DependsOn)
			}
			ops = append(ops, lock.PatchOp{Op: "depends_on", Key: r.Key, Value: deps})
		}
	}
	if !jsonEqual(baseChecks, d.Checks) {
		ops = append(ops, lock.PatchOp{Op: "checks", Value: generic(d.Checks)})
	}
	return ops
}

// jsonEqual reports whether a and b are equal as JSON.
func jsonEqual(a, b any) bool {
	ja, errA := json.Marshal(a)
	jb, errB := json.Marshal(b)
	return errA == nil && errB == nil && bytes.Equal(ja, jb)
}

// generic returns v as plain JSON values (maps, lists, strings, json.Number,
// bools), which the lock writes as YAML; nil when v does not encode.
func generic(v any) any {
	data, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var out any
	if dec.Decode(&out) != nil {
		return nil
	}
	return out
}
