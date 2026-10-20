package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"

	"github.com/giammbo/prosa/internal/checks"
	"github.com/giammbo/prosa/internal/intent"
	"github.com/giammbo/prosa/internal/lock"
	"github.com/giammbo/prosa/internal/provider"
	"github.com/giammbo/prosa/internal/state"
)

// Engine reconciles the intents in Dir against the providers.
type Engine struct {
	Dir, LockPath string
	// Ignore names the files of Dir that are not intents (intent.Load).
	Ignore []string
	// Region is the AWS region of the project (policy, planner input);
	// empty: AWSRegion().
	Region  string
	State   *state.Store
	Planner Planner
	// Providers returns a started, configured provider. An empty version
	// lets it choose one; it then reports it through a Version() string
	// method (ProviderPool.Get does).
	Providers func(ctx context.Context, name, version string) (Provider, error)
	Now       func() time.Time
	// CheckOptions are the settings of the checks (ADR-021). The CLI sets
	// Emulated from the project settings; PROSA_AWS_ENDPOINT is only a
	// fallback for direct Engine users (it also sets Emulated). Now
	// defaults to Engine.Now.
	CheckOptions checks.Options
	// Observer, when set, receives the plan, the approval, each applied
	// operation and each check result (ADR-023).
	Observer Observer
	// InFlight, when set, holds the provider apply in progress (ADR-024).
	InFlight *InFlight
	// Progress, when set, is called by Apply when an operation starts,
	// finishes or fails (see OpProgress), from Apply's goroutine.
	Progress func(OpProgress)
	// NoResume turns off the resume of an interrupted intent from its
	// Pending desired state (ADR-025): the planner is asked again.
	NoResume bool
	// Env, when set, is the environment file (ADR-029): the planner sees it,
	// the engine tags resources with its name, and the hash that decides
	// re-planning covers it. nil: no environment.
	Env *intent.Env
	// EnvFile is the --env argument as the person gave it, for the hints
	// that name a command (empty: Env.Path).
	EnvFile string
	// BaseLockPath is the lock of the base (ADR-030), the root's: with Env
	// set, each intent's base is read from it (planned first, and written by
	// Apply, when missing or stale) and the environment derives from it by a
	// patch. Empty: no base, the environment is planned as a whole (ADR-029).
	BaseLockPath string
	// BaseState is the root's state store, read only: when a base is planned
	// during an environment's run, the planner is shown the text the base
	// last completed with, as `prosa plan` of the base shows it, so that the
	// planner cache gives both the same key. nil: no previous text.
	BaseState *state.Store
	// BaseTextDir keeps, as <intent>.md, the text each intent's base was
	// planned from (ADR-030), written with the base lock entry: by a root
	// apply, and by an apply --env that writes the base lock. A base is
	// planned with it as the previous text, in root and environment runs
	// alike (the planner cache then gives both the same key); without it,
	// with the root state's text. Empty: no base texts.
	BaseTextDir string
}

// Action is what a change does to one resource.
type Action string

const (
	Create  Action = "create"
	Update  Action = "update"
	Replace Action = "replace"
	Delete  Action = "delete"
	NoOp    Action = "no-op"
)

// Change is the planned change of one resource. Before is the refreshed
// state (null when the resource does not exist), After the planned state
// (null for a delete; for a replace, the state planned for the new object).
type Change struct {
	Intent, Key, Type string
	Action            Action
	Before, After     cty.Value
	RequiresReplace   []string
	// Drifted reports that the remote object vanished or that the refreshed
	// state contradicts the chosen configuration (ADR-018): DriftAttrs lists
	// the top-level attributes and blocks, set in the resolved config, whose
	// refreshed value differs from the stored one. InfoDrift lists the other
	// differences, informational only; computed-only attributes never count.
	// Both are sorted.
	Drifted    bool
	DriftAttrs []string
	InfoDrift  []string
	// Gate says whether the change runs on its own, after an approval, or
	// not at all (ADR-022); GateReason says why it needs an approval
	// ("destroy") or why it is refused ("observe", "advise", "not
	// approved", "depends on <intent>/<key>"). A no-op has no gate.
	Gate       Gate
	GateReason string

	// What Apply needs, as computed by Plan.
	provider, providerVersion string // lock name and version of the provider
	// config is the planner's configuration (the lock's chosen), references
	// still as "${...}" strings; nil for a delete.
	config map[string]any
	// dependsOn is the desired resource's depends_on (the lock's); nil for
	// a delete.
	dependsOn []string
	// resolved is config with the references substituted and the ownership
	// tag added, as passed to PlanResource; cty.NilVal for a delete.
	resolved cty.Value
	// prior, priorPrivate and priorIdentity come from the refresh: the
	// state the change was planned from (null on create).
	prior         cty.Value
	priorPrivate  []byte
	priorIdentity cty.Value
	// planned is PlanResource(prior, resolved) for create, update, no-op and
	// replace (the plan that asked for the replacement), and the destroy
	// plan for a delete. A delete whose prior is null (already gone out of
	// band) has no plan: Apply only forgets the resource.
	planned provider.PlannedChange
	// destroyPlan and createPlan are set for a replace only: the destroy of
	// the old object and PlanResource(null, resolved) for the new one.
	destroyPlan, createPlan provider.PlannedChange
}

// Plan is the outcome of Engine.Plan. Changes holds the deletes first, in
// reverse dependency order, then every other change (no-ops included) in
// dependency order. LockDirty reports that the lock must be rewritten even
// without infrastructure changes (an intent changed or was removed). A
// non-empty Blocked means the plan must not be applied; Changes is then empty.
// Blockers lists, as "<intent>: <reason>", why the planner could not plan
// intents as written: they have no changes and Apply refuses the plan, while
// Changes holds those of the other intents. Every change that is not a
// no-op has a gate (ADR-022); a refused one, and everything that depends on
// it, is not applied.
type Plan struct {
	Changes   []Change
	LockDirty bool
	Blocked   string
	Blockers  []string
	// CheckResults holds, after a successful Apply, the results of the
	// checks of the intents that applied changes (ADR-021), by intent, then
	// in the order of their checks.
	CheckResults []CheckResult
	// Written reports that Apply wrote the lock or a state.
	Written bool

	// What Apply needs, as loaded by Plan.
	dir       string                        // the intents directory, as given
	region    string                        // the AWS region (Engine.Region)
	env       *intent.Env                   // Engine.Env
	lock      *lock.Lock                    // the lock as loaded
	lockBytes []byte                        // its bytes on disk, nil when there was no file
	intents   []intent.Intent               // intents in Dir, by name
	removed   []string                      // intents only in the lock, sorted
	desired   map[string][]DesiredResource  // desired resources per intent in Dir
	checks    map[string][]lock.Check       // checks per intent in Dir
	states    map[string]*state.IntentState // state per intent (in Dir or removed), as loaded
	digests   map[string]string             // digest of each loaded state ("" when absent)
	asked     map[string]bool               // intents whose desired state the planner gave in this run
	resumed   map[string]bool               // intents whose desired state is the Pending of an interrupted apply
	// prevBodies is, by intent, the previous text the planner is shown when
	// it is not the state's: the base text (Engine.BaseTextDir) of a base.
	prevBodies map[string]string
	// keep lists the intents planDesired takes from the lock as they are,
	// up to date or not: the bases an environment does not need (planBases).
	keep map[string]bool
	// In an environment derived from the base (ADR-030): the base lock as
	// loaded, with the bases this plan planned, and its bytes on disk (nil
	// when there was no file), the intents whose planned base Apply writes
	// to the base lock (baseWrite: those the root workspace has not applied),
	// and per intent of Dir the base_hash and the patch of its environment
	// lock entry. base is nil otherwise.
	base       *lock.Lock
	baseBytes  []byte
	baseWrite  map[string]bool
	baseHashes map[string]string
	patches    map[string][]lock.PatchOp
	// providerName and providerVersion are the provider used (version from
	// the lock, or the one the provider chose); prov and schema are nil when
	// no resource needed a provider.
	providerName, providerVersion string
	prov                          Provider
	schema                        *provider.Schema
}

// IsBlocked reports whether the plan must not be applied: the state store
// does not match the lock (Blocked) or an intent cannot be planned as
// written (Blockers).
func (p *Plan) IsBlocked() bool { return p.Blocked != "" || len(p.Blockers) > 0 }

// HasChanges reports whether at least one change is not a no-op, refused
// or not (see Applicable).
func (p *Plan) HasChanges() bool {
	for _, c := range p.Changes {
		if c.Action != NoOp {
			return true
		}
	}
	return false
}

// keysOf returns the desired keys of an intent in Dir; nil otherwise.
func (p *Plan) keysOf(intentName string) []string { return keysIn(p.desired)(intentName) }

// keysIn returns the keysOf function of a set of desired states.
func keysIn(desired map[string][]DesiredResource) func(string) []string {
	return func(intentName string) []string {
		var keys []string
		for _, r := range desired[intentName] {
			keys = append(keys, r.Key)
		}
		return keys
	}
}

// intentNames returns the intents in Dir and the removed ones, sorted.
func (p *Plan) intentNames() []string {
	var names []string
	for _, it := range p.intents {
		names = append(names, it.Name)
	}
	names = append(names, p.removed...)
	sort.Strings(names)
	return names
}

// current is the refreshed state of a resource in the state store.
type current struct {
	typ      string
	stored   cty.Value // the state in the store
	state    cty.Value // the refreshed state, null when the remote object is gone
	private  []byte
	identity cty.Value
}

// gone reports that the remote object vanished out of band.
func (c *current) gone() bool { return c.state.IsNull() }

// nullIdentity is the identity of a resource without one.
var nullIdentity = cty.NullVal(cty.DynamicPseudoType)

// Plan computes the changes that bring the infrastructure to the desired
// state of the intents. It writes nothing. A plan whose state store does not
// match the lock is Blocked before the planner is asked anything; invalid
// desired resources are a *ValidationError, before any refresh. An intent
// the planner reports Blockers for has no changes and blocks the plan; the
// other intents are planned as usual.
func (e *Engine) Plan(ctx context.Context) (*Plan, error) {
	p, err := e.plan(ctx)
	if err == nil && e.Observer != nil {
		e.Observer.Planned(p)
	}
	return p, err
}

func (e *Engine) plan(ctx context.Context) (*Plan, error) {
	intents, err := intent.Load(e.Dir, e.Ignore...)
	if err != nil {
		return nil, err
	}
	lk, lockBytes, err := lock.Read(e.LockPath)
	if err != nil {
		return nil, err
	}
	p := &Plan{
		dir:          e.Dir,
		region:       e.region(),
		env:          e.Env,
		lock:         lk,
		lockBytes:    lockBytes,
		intents:      intents,
		desired:      map[string][]DesiredResource{},
		checks:       map[string][]lock.Check{},
		states:       map[string]*state.IntentState{},
		digests:      map[string]string{},
		asked:        map[string]bool{},
		resumed:      map[string]bool{},
		providerName: awsProvider,
	}
	if e.baseMode() {
		if err := e.loadBase(p); err != nil {
			return nil, err
		}
	}
	if e.Env == nil {
		// The root's lock is the base: its previous texts are the base texts.
		p.prevBodies = map[string]string{}
		for _, it := range intents {
			body, ok, err := e.readBaseText(it.Name)
			if err != nil {
				return nil, err
			}
			if ok {
				p.prevBodies[it.Name] = body
			}
		}
	}
	present := map[string]bool{}
	for _, it := range intents {
		present[it.Name] = true
	}
	for name := range lk.Intents {
		if !present[name] {
			p.removed = append(p.removed, name)
		}
	}
	sort.Strings(p.removed)
	p.LockDirty = len(p.removed) > 0

	// State store, checked against the lock before anything is planned.
	for _, name := range p.intentNames() {
		st, digest, err := e.State.Load(name)
		if err != nil {
			return nil, err
		}
		p.states[name], p.digests[name] = st, digest
		if msg := stateMismatch(name, lk.Intents[name], st, digest, e.recoverCommand()); msg != "" {
			p.Blocked = msg
			return p, nil
		}
	}

	// Desired state.
	lockKeys := lockKeysOf(lk)
	blocked, err := e.desiredStates(ctx, p, lockKeys)
	if err != nil {
		return nil, err
	}

	// Order and references, before any provider call.
	order, refProbs, badRefs := p.desiredOrder(p.desired, lockKeys)
	needProvider := false
	for _, name := range p.intentNames() {
		needProvider = needProvider || len(p.states[name].Resources) > 0 || len(p.desired[name]) > 0
	}
	if !needProvider {
		// No resource anywhere: the checks are still validated (they can
		// only be http_get, tls_days or unverifiable to be valid).
		if probs := p.allCheckProblems(); len(probs) > 0 {
			return nil, newValidationError(probs)
		}
		return p, nil
	}
	if err := e.startProvider(ctx, p); err != nil {
		return nil, err
	}
	if err := p.validate(ctx, refProbs, badRefs); err != nil {
		return nil, err
	}

	// Refresh.
	cur := map[string]*current{}
	for _, name := range p.intentNames() {
		st := p.states[name]
		for _, key := range slices.Sorted(maps.Keys(st.Resources)) {
			node := name + "/" + key
			rs := st.Resources[key]
			if rsch := p.schema.Resources[rs.Type]; rsch != nil && rs.SchemaVersion != rsch.Version {
				return nil, fmt.Errorf("state of %s was written with schema version %d, provider has %d: upgrade not supported yet", node, rs.SchemaVersion, rsch.Version)
			}
			c, err := p.refresh(ctx, rs)
			if err != nil {
				return nil, fmt.Errorf("refresh %s: %w", node, err)
			}
			cur[node] = c
		}
	}

	// Desired resources, in dependency order. A reference takes the planned
	// state of a resource that changes in this run, the refreshed one
	// otherwise.
	type desiredRes struct {
		intent string
		r      DesiredResource
	}
	byNode := map[string]desiredRes{}
	for name, rs := range p.desired {
		for _, r := range rs {
			byNode[name+"/"+r.Key] = desiredRes{name, r}
		}
	}
	values := map[string]cty.Value{}
	lookup := func(node string) (cty.Value, bool) {
		v, ok := values[node]
		return v, ok
	}
	var others []Change
	for _, node := range order {
		d := byNode[node]
		c, err := p.planResource(ctx, d.intent, d.r, cur[node], lookup)
		if err != nil {
			rejected := len(collectDiags(err)) > 0 // the provider rejected the answer
			if f, ok := e.Planner.(Forgetter); ok && p.asked[d.intent] && rejected {
				f.Forget(d.intent)
			}
			if p.resumed[d.intent] && rejected {
				err = fmt.Errorf("%w (resumed from the interrupted apply; run with --no-cache to plan it again)", err)
			}
			return nil, err
		}
		if c.Action == NoOp {
			values[node] = c.prior
		} else {
			values[node] = c.After
		}
		others = append(others, c)
	}

	// Stored resources that are no longer desired.
	deletes, err := p.planDeletes(ctx, cur, lockKeys)
	if err != nil {
		return nil, err
	}
	p.Changes = slices.DeleteFunc(append(deletes, others...), func(c Change) bool { return blocked[c.Intent] })
	p.setGates(newPolicy(p.region))
	if p.hasRefused() {
		if err := p.prune(); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// desiredStates sets the desired resources and checks of every intent in
// Dir. In an environment derived from the base see envDesiredStates;
// otherwise see planDesired.
func (e *Engine) desiredStates(ctx context.Context, p *Plan, lockKeys map[string][]string) (map[string]bool, error) {
	if p.base != nil {
		return e.envDesiredStates(ctx, p, lockKeys)
	}
	return e.planDesired(ctx, p, lockKeys)
}

// planDesired sets the desired resources and checks of every intent in
// Dir: from the lock when the intent is unchanged, otherwise from the
// planner. The planner is asked in depends_on order (by name when depends_on
// has a cycle), each intent seeing the desired state known so far: the new
// one of the intents already planned, the lock's for the others. The provider
// is started before the planner is asked. It returns the blocked intents,
// whose desired resources and checks are the lock's (reference targets only:
// they get no changes). A planner's answer that is only a patch is applied
// to the lock's state of the intent (PlanInput.resolve).
func (e *Engine) planDesired(ctx context.Context, p *Plan, lockKeys map[string][]string) (map[string]bool, error) {
	view := map[string][]DesiredResource{}
	checkView := map[string][]lock.Check{}
	var toPlan []intent.Intent
	for _, it := range p.intents {
		li := p.lock.Intents[it.Name]
		if li != nil {
			view[it.Name], checkView[it.Name] = fromLock(li), li.Checks
		}
		if p.upToDate(it, li) || p.keep[it.Name] {
			p.desired[it.Name], p.checks[it.Name] = view[it.Name], checkView[it.Name]
		} else {
			toPlan = append(toPlan, it)
		}
	}
	blocked := map[string]bool{}
	if len(toPlan) > 0 {
		if e.Planner == nil {
			return nil, fmt.Errorf("intent %q needs planning but no planner is configured", toPlan[0].Name)
		}
		if err := e.startProvider(ctx, p); err != nil {
			return nil, err
		}
		p.LockDirty = true
	}
	for _, it := range planningOrder(p.intents, toPlan) {
		in := p.planInput(it, view, checkView, lockKeys)
		var d Desired
		resumed := false
		var err error
		if !e.NoResume {
			d, resumed, err = p.resume(ctx, it, in)
		}
		if err != nil {
			return nil, fmt.Errorf("plan intent %q: %w", it.Name, err)
		}
		if resumed {
			p.resumed[it.Name] = true
		} else {
			if d, err = e.Planner.Plan(ctx, in); err != nil {
				return nil, fmt.Errorf("plan intent %q: %w", it.Name, err)
			}
			p.asked[it.Name] = true
		}
		if len(d.Blockers) > 0 {
			blocked[it.Name] = true
			for _, b := range d.Blockers {
				p.Blockers = append(p.Blockers, it.Name+": "+b)
			}
			d.Resources, d.Checks = view[it.Name], checkView[it.Name]
		} else {
			if err = p.checkPatch(in, d); err == nil {
				d, err = in.resolve(d)
			}
			if err != nil {
				return nil, fmt.Errorf("plan intent %q: %w", it.Name, err)
			}
			d = p.canonical(d)
		}
		view[it.Name], checkView[it.Name] = d.Resources, d.Checks
		p.desired[it.Name], p.checks[it.Name] = d.Resources, d.Checks
	}
	return blocked, p.settleDesired()
}

// settleDesired sorts the blockers by intent and checks the keys and types
// of every desired state.
func (p *Plan) settleDesired() error {
	slices.SortStableFunc(p.Blockers, func(a, b string) int {
		an, _, _ := strings.Cut(a, ": ")
		bn, _, _ := strings.Cut(b, ": ")
		return strings.Compare(an, bn)
	})
	for _, it := range p.intents {
		if err := validateDesired(it.Name, p.desired[it.Name]); err != nil {
			return err
		}
	}
	return nil
}

// resume returns the desired state an interrupted Apply was executing for
// it (its state's Pending, ADR-024) when the lock says the intent did not
// complete (an empty intent_hash) and its text is the one it was planned
// from: the planner, and its cache, are not asked. Like a cache hit it is
// validated first (in.Validate); with problems, or unreadable, it is not
// used and the intent is planned again. The error is a transport or
// context failure of the validation.
func (p *Plan) resume(ctx context.Context, it intent.Intent, in PlanInput) (Desired, bool, error) {
	li, pd := p.lock.Intents[it.Name], p.states[it.Name].Pending
	if li == nil || li.IntentHash != "" || pd == nil || pd.IntentHash != p.intentHash(it) {
		return Desired{}, false, nil
	}
	dec := json.NewDecoder(bytes.NewReader(pd.Desired))
	dec.UseNumber()
	var d Desired
	if err := dec.Decode(&d); err != nil || len(d.Blockers) > 0 {
		return Desired{}, false, nil
	}
	var ve *ValidationError
	switch err := in.Validate(ctx, d); {
	case err == nil:
		return d, true, nil
	case errors.As(err, &ve):
		return Desired{}, false, nil
	default:
		return Desired{}, false, err
	}
}

// fromLock returns the desired resources the lock recorded for an intent.
func fromLock(li *lock.Intent) []DesiredResource { return desiredOf(li.Resources) }

// planningOrder returns the intents of toPlan in depends_on order over all
// the intents (so that an unchanged intent in between still counts), by name
// when depends_on has a cycle.
func planningOrder(all, toPlan []intent.Intent) []intent.Intent {
	deps := map[string][]string{}
	for _, it := range all {
		deps[it.Name] = append([]string{}, it.DependsOn...)
	}
	order, err := TopoSort(deps)
	if err != nil {
		return toPlan
	}
	byName := map[string]intent.Intent{}
	for _, it := range toPlan {
		byName[it.Name] = it
	}
	var out []intent.Intent
	for _, n := range order {
		if it, ok := byName[n]; ok {
			out = append(out, it)
		}
	}
	return out
}

func validateDesired(intentName string, rs []DesiredResource) error {
	seen := map[string]bool{}
	for _, r := range rs {
		switch {
		case r.Key == "":
			return fmt.Errorf("intent %q: resource with an empty key", intentName)
		case seen[r.Key]:
			return fmt.Errorf("intent %q: duplicate resource key %q", intentName, r.Key)
		case r.Type == "":
			return fmt.Errorf("intent %q: resource %q has no type", intentName, r.Key)
		}
		seen[r.Key] = true
	}
	return nil
}

// lockKeysOf returns the resource keys of every intent in the lock.
func lockKeysOf(lk *lock.Lock) map[string][]string {
	out := map[string][]string{}
	for name, li := range lk.Intents {
		for _, r := range li.Resources {
			out[name] = append(out[name], r.Key)
		}
	}
	return out
}

// desiredOrder sorts the desired resources of all intents (desired, by
// intent in Dir) by their references, template parts included, and their
// depends_on. Invalid references (unknown intent or key, a malformed
// template, an intent that was removed), invalid depends_on entries (see
// dependsOnProblem) and dependency cycles are returned as problems,
// one per fault; badRefs holds the nodes whose references are invalid, which
// take no part in the order.
func (p *Plan) desiredOrder(desired map[string][]DesiredResource, lockKeys map[string][]string) (order []string, probs []ValidationProblem, badRefs map[string]bool) {
	removed := map[string]bool{}
	for _, n := range p.removed {
		removed[n] = true
	}
	refKeys := func(n string) []string {
		if removed[n] {
			return lockKeys[n]
		}
		return keysIn(desired)(n)
	}
	badRefs = map[string]bool{}
	types := map[string]string{}
	deps := map[string][]string{}
	for _, it := range p.intents {
		for _, r := range desired[it.Name] {
			node := it.Name + "/" + r.Key
			types[node] = r.Type
			bad := func(msg string) {
				badRefs[node] = true
				probs = append(probs, ValidationProblem{Intent: it.Name, Key: r.Key, Type: r.Type, Message: msg})
			}
			deps[node] = []string{}
			refs, err := CollectRefs(r.Config, refKeys)
			if err != nil {
				for _, e := range splitJoined(err) {
					bad(e.Error())
				}
				refs = nil // depends_on is still checked
			}
			for _, ref := range refs {
				if removed[ref.Intent] {
					bad(fmt.Sprintf("intent %q refers to %s, but intent %q was removed", it.Name, ref, ref.Intent))
					continue
				}
				deps[node] = append(deps[node], ref.Node())
			}
			for _, d := range r.DependsOn {
				if msg := dependsOnProblem(it.Name, node, d, refKeys, removed); msg != "" {
					probs = append(probs, ValidationProblem{Intent: it.Name, Key: r.Key, Type: r.Type, Message: msg})
					continue
				}
				deps[node] = append(deps[node], d)
			}
		}
	}
	order, err := TopoSort(deps)
	var ce *CycleError
	if errors.As(err, &ce) {
		seen := map[string]bool{}
		for _, node := range ce.Nodes {
			if seen[node] {
				continue
			}
			seen[node] = true
			intentName, key, _ := strings.Cut(node, "/")
			probs = append(probs, ValidationProblem{Intent: intentName, Key: key, Type: types[node], Message: ce.Error()})
		}
	}
	return order, probs, badRefs
}

// dependsOnProblem returns why d, an entry of the depends_on of node (of
// intent name), is invalid, "" when it is a valid "<intent>/<key>" of
// another resource (ADR-020). keysOf gives the keys of an intent, removed
// the intents that were removed.
func dependsOnProblem(name, node, d string, keysOf func(string) []string, removed map[string]bool) string {
	intentName, key, found := strings.Cut(d, "/")
	switch {
	case !found || intentName == "" || key == "":
		return fmt.Sprintf("depends_on %q: expected \"<intent>/<key>\"", d)
	case d == node:
		return fmt.Sprintf("depends_on %q: a resource cannot depend on itself", d)
	case removed[intentName]:
		return fmt.Sprintf("intent %q depends on %s, but intent %q was removed", name, d, intentName)
	}
	keys := keysOf(intentName)
	switch {
	case len(keys) == 0:
		return fmt.Sprintf("depends_on %q: unknown intent %q", d, intentName)
	case !slices.Contains(keys, key):
		return fmt.Sprintf("depends_on %q: unknown key %s", d, d)
	}
	return ""
}

// startProvider gets the provider, records the version used and checks the
// schema, once.
func (e *Engine) startProvider(ctx context.Context, p *Plan) error {
	if p.prov != nil {
		return nil
	}
	if e.Providers == nil {
		return fmt.Errorf("no provider source configured")
	}
	version := p.lock.ProviderVersions[p.providerName]
	if version == "" && p.base != nil {
		version = p.base.ProviderVersions[p.providerName] // a new environment starts from the base's
	}
	prov, err := e.Providers(ctx, p.providerName, version)
	if err != nil {
		return fmt.Errorf("provider %s: %w", p.providerName, err)
	}
	if v, ok := prov.(versioned); ok && version == "" {
		version = v.Version()
	}
	schema, err := prov.Schema(ctx)
	if err != nil {
		return err
	}
	p.prov, p.schema, p.providerVersion = prov, schema, version
	return nil
}

// refresh reads a stored resource from the provider.
func (p *Plan) refresh(ctx context.Context, rs *state.ResourceState) (*current, error) {
	if rs.Provider != p.providerName {
		return nil, fmt.Errorf("unsupported provider %q", rs.Provider)
	}
	stored, identity, err := p.decodeStored(rs)
	if err != nil {
		return nil, err
	}
	ty := stored.Type()
	res, err := p.prov.ReadResource(ctx, rs.Type, stored, rs.Private, identity)
	if err != nil {
		return nil, err
	}
	c := &current{typ: rs.Type, stored: stored, state: res.State, private: res.Private, identity: res.Identity}
	if c.state == cty.NilVal || c.state.IsNull() {
		c.state, c.private, c.identity = cty.NullVal(ty), nil, nullIdentity
	}
	if c.identity == cty.NilVal {
		c.identity = nullIdentity
	}
	if !c.state.IsWhollyKnown() {
		return nil, fmt.Errorf("provider returned a state with unknown values")
	}
	return c, nil
}

// decodeStored returns the state and identity a state-store record holds,
// typed with the provider's schema.
func (p *Plan) decodeStored(rs *state.ResourceState) (stored, identity cty.Value, err error) {
	rsch := p.schema.Resources[rs.Type]
	if rsch == nil {
		return cty.NilVal, cty.NilVal, fmt.Errorf("unknown resource type %q", rs.Type)
	}
	if stored, err = ctyjson.Unmarshal(rs.State, rsch.Block.ImpliedType()); err != nil {
		return cty.NilVal, cty.NilVal, fmt.Errorf("stored state: %w", err)
	}
	identity = nullIdentity
	if is := p.schema.Identities[rs.Type]; is != nil {
		identity = cty.NullVal(is.Type)
		if len(rs.Identity) > 0 {
			if identity, err = ctyjson.Unmarshal(rs.Identity, is.Type); err != nil {
				return cty.NilVal, cty.NilVal, fmt.Errorf("stored identity: %w", err)
			}
		}
	}
	return stored, identity, nil
}

// planResource plans one desired resource from its refreshed state cur (nil
// when the state store does not have it).
func (p *Plan) planResource(ctx context.Context, intentName string, r DesiredResource, cur *current, lookup func(string) (cty.Value, bool)) (Change, error) {
	node := intentName + "/" + r.Key
	b := p.schema.Resources[r.Type].Block
	ty := b.ImpliedType()
	cfg, err := buildConfig(b, intentName, p.envName(), r.Key, r.Config, p.keysOf, lookup)
	if err != nil {
		return Change{}, refErr(node, err)
	}
	c := Change{
		Intent: intentName, Key: r.Key, Type: r.Type,
		provider: p.providerName, providerVersion: p.providerVersion,
		config: r.Config, dependsOn: r.DependsOn, resolved: cfg,
		prior: cty.NullVal(ty), priorIdentity: nullIdentity,
	}
	if cur != nil {
		if cur.typ != r.Type {
			return Change{}, fmt.Errorf("%s: type changed from %s to %s, which is not supported", node, cur.typ, r.Type)
		}
		c.prior, c.priorPrivate, c.priorIdentity = cur.state, cur.private, cur.identity
		c.DriftAttrs, c.InfoDrift = driftOf(b, cur.stored, cur.state, cfg)
		c.Drifted = cur.gone() || len(c.DriftAttrs) > 0
	}
	c.Before = c.prior
	if c.planned, err = p.prov.PlanResource(ctx, r.Type, c.prior, cfg, c.priorPrivate, c.priorIdentity); err != nil {
		return Change{}, fmt.Errorf("%s: %w", node, err)
	}
	c.Action, c.After = actionOf(c.prior, c.planned), c.planned.Planned
	if c.Action == Replace {
		c.RequiresReplace = c.planned.RequiresReplace
		if c.destroyPlan, err = p.planDestroy(ctx, r.Type, c.prior, c.priorPrivate, c.priorIdentity); err != nil {
			return Change{}, fmt.Errorf("%s: %w", node, err)
		}
		if c.createPlan, err = p.prov.PlanResource(ctx, r.Type, cty.NullVal(ty), cfg, nil, nullIdentity); err != nil {
			return Change{}, fmt.Errorf("%s: %w", node, err)
		}
		c.After = c.createPlan.Planned
	}
	return c, nil
}

// actionOf classifies a plan of a resource from its prior state, for Plan and
// for the re-plan in Apply.
func actionOf(prior cty.Value, pc provider.PlannedChange) Action {
	switch {
	case prior.IsNull():
		return Create
	case len(pc.RequiresReplace) > 0:
		return Replace
	case sameValue(pc.Planned, prior):
		return NoOp
	default:
		return Update
	}
}

// planDestroy plans the destroy of prior. Without the plan_destroy
// capability the provider is not asked, as Terraform does.
func (p *Plan) planDestroy(ctx context.Context, typeName string, prior cty.Value, private []byte, identity cty.Value) (provider.PlannedChange, error) {
	null := cty.NullVal(p.schema.Resources[typeName].Block.ImpliedType())
	if !p.schema.PlanDestroy {
		return provider.PlannedChange{Planned: null, PlannedPrivate: private, Identity: identity}, nil
	}
	pc, err := p.prov.PlanResource(ctx, typeName, prior, null, private, identity)
	if err != nil {
		return provider.PlannedChange{}, err
	}
	if pc.Planned != cty.NilVal && !pc.Planned.IsNull() {
		return provider.PlannedChange{}, fmt.Errorf("provider planned a non-null state for a destroy")
	}
	pc.Planned = null
	return pc, nil
}

// planDeletes plans the delete of every stored resource that is no longer
// desired, in reverse dependency order of the lock's chosen configurations
// and depends_on.
func (p *Plan) planDeletes(ctx context.Context, cur map[string]*current, lockKeys map[string][]string) ([]Change, error) {
	deleted := map[string]Change{}
	for _, name := range p.intentNames() {
		want := map[string]bool{}
		for _, r := range p.desired[name] {
			want[r.Key] = true
		}
		for key, rs := range p.states[name].Resources {
			if want[key] {
				continue
			}
			node := name + "/" + key
			c := cur[node]
			_, info := driftOf(p.schema.Resources[rs.Type].Block, c.stored, c.state, cty.NilVal)
			ch := Change{
				Intent: name, Key: key, Type: rs.Type, Action: Delete,
				Before: c.state, After: cty.NullVal(c.state.Type()), Drifted: c.gone(), InfoDrift: info,
				provider: p.providerName, providerVersion: p.providerVersion,
				prior: c.state, priorPrivate: c.private, priorIdentity: c.identity,
			}
			if !c.state.IsNull() {
				pc, err := p.planDestroy(ctx, rs.Type, c.state, c.private, c.identity)
				if err != nil {
					return nil, fmt.Errorf("%s: %w", node, err)
				}
				ch.planned = pc
			}
			deleted[node] = ch
		}
	}
	if len(deleted) == 0 {
		return nil, nil
	}
	keysOf := func(n string) []string { return lockKeys[n] }
	deps := map[string][]string{}
	for name, li := range p.lock.Intents {
		for _, r := range li.Resources {
			node := name + "/" + r.Key
			refs, err := CollectRefs(r.Chosen, keysOf)
			if err != nil {
				return nil, fmt.Errorf("lock: %s: %w", node, err)
			}
			deps[node] = slices.Clone(r.DependsOn)
			for _, ref := range refs {
				deps[node] = append(deps[node], ref.Node())
			}
		}
	}
	for node := range deleted {
		if _, ok := deps[node]; !ok {
			deps[node] = nil
		}
	}
	order, err := TopoSort(deps)
	if err != nil {
		return nil, fmt.Errorf("lock: %w", err)
	}
	var out []Change
	for i := len(order) - 1; i >= 0; i-- {
		if c, ok := deleted[order[i]]; ok {
			out = append(out, c)
		}
	}
	return out, nil
}

// sameValue reports whether a and b are known to be equal.
func sameValue(a, b cty.Value) bool {
	eq := a.Equals(b)
	return eq.IsKnown() && eq.True()
}
