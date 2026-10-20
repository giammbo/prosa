package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/zclconf/go-cty/cty"

	"github.com/giammbo/prosa/internal/provider"
)

// applyLockName is the state-store lock Apply holds for the whole run, before
// the per-intent locks: it serializes the writers of the shared lock file. An
// intent can never have this name (intent names start with [a-z0-9]).
const applyLockName = "_apply"

// ErrNotApproved is returned by Apply when the changes that needed an
// approval were not approved and nothing else was left to apply.
var ErrNotApproved = errors.New("plan not approved")

// ErrInterrupted is wrapped by the error of an Apply whose context was
// cancelled (ADR-024): the operation in progress finished and was saved,
// no other one started.
var ErrInterrupted = errors.New("interrupted")

// interrupted is the error of an Apply stopped by its context after done of
// total operations.
func interrupted(done, total int) error {
	err := fmt.Errorf("%w: finished %d of %d operations; run plan again", ErrInterrupted, done, total)
	return kindErr(KindInterrupted, err.Error(), err)
}

// Apply executes p; a blocked plan (see Plan.IsBlocked) is refused. approve
// is asked once, only when some change needs an approval (ADR-022): if it
// does not accept, those changes become refused ("not approved"). Refused
// changes, and every change that depends on them (see Plan.prune), are not
// applied, and their intents keep their lock entry (see heldIntents). A plan
// with only auto changes, or with only lock changes (an intent rewritten
// without effects, or removed while empty), runs without asking.
//
// The changes run as an operation graph (see applyOrder). Every change is
// planned again with the values known at that point (the applied state of
// the resources it refers to): a change the provider now finds nothing to do
// for is skipped, any other different action stops Apply. The state store
// and the lock are written after each resource, so a failure leaves them
// consistent with what was applied; a rewritten intent has an empty
// intent_hash from its first write until all its changes succeeded, when it
// gets the new one. An intent whose changes were all skipped, with an
// unchanged hash, writes nothing. When an intent completes after applying
// something, the refreshed state of its no-op and skipped resources is stored
// too (ADR-018). After a successful Apply, the checks of the intents that
// applied something or were rewritten run on the resources read again from
// the provider, and
// their results are in p.CheckResults (ADR-021): a failed check does not
// make Apply fail, the caller decides. Apply holds the project lock
// (applyLockName) and the lock of every intent it writes, and refuses to run
// if the lock file or a state changed since the plan. Apply updates the lock
// and states held by p: a plan is applied at most once. In an environment
// derived from the base, the bases the plan planned are written to the base
// lock first, as choices (see setBase), and Apply refuses to run if that
// file changed since the plan (ADR-030).
//
// Cancelling ctx interrupts Apply (ADR-024): the provider's apply of the
// operation in progress runs on a context that is never cancelled, so it
// finishes and is saved (and its intent completed, if it was the last one);
// no other operation starts, the checks do not run, and Apply returns an
// error wrapping ErrInterrupted. The other provider calls (the re-plan
// before an operation) use ctx: one cut short stops Apply before its
// operation, and ctx is checked again right before each provider apply.
//
// A forced stop (the CLI's second Ctrl-C exits at once) can kill the process
// while the provider applies, leaving a remote object the state does not
// record, or between the state save and the lock save, leaving the state
// ahead of the lock: Plan then says so and Recover rebuilds the lock.
func (e *Engine) Apply(ctx context.Context, p *Plan, approve func(*Plan) bool) error {
	if p.Blocked != "" {
		return errors.New(p.Blocked)
	}
	if len(p.Blockers) > 0 {
		return fmt.Errorf("plan is blocked: %s", strings.Join(p.Blockers, "; "))
	}
	if toApprove := p.ToApprove(); len(toApprove) > 0 {
		granted := approve != nil && approve(p)
		if e.Observer != nil {
			names := make([]string, len(toApprove))
			for i, c := range toApprove {
				names[i] = c.Intent + "/" + c.Key
			}
			e.Observer.Approval(granted, names)
		}
		if !granted {
			if err := p.refuseUnapproved(); err != nil {
				return err
			}
			if !p.Applicable() {
				return ErrNotApproved
			}
		}
	}
	if !p.Applicable() && !p.LockDirty {
		return nil
	}

	order, err := p.applyOrder()
	if err != nil {
		return err
	}
	a := &applier{
		e: e, p: p,
		values: map[string]cty.Value{}, planned: map[string]cty.Value{},
		pending: map[string]int{}, changed: map[string]bool{}, stale: map[string]bool{},
		skipped: map[string]bool{}, executed: map[string]bool{}, held: p.heldIntents(),
	}
	names := a.involved(order)
	unlockApply, err := e.State.Lock(applyLockName)
	if err != nil {
		return kindErr(KindLock, "another apply is running", fmt.Errorf("another apply is running: %w", err))
	}
	defer unlockApply()
	unlock, err := e.lockIntents(names)
	if err != nil {
		return err
	}
	defer unlock()
	disk, err := os.ReadFile(e.LockPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if !bytes.Equal(disk, p.lockBytes) {
		return kindErr(KindLock, "lock file changed since plan", errors.New("lock file changed since plan; run plan again"))
	}
	for _, name := range names {
		_, digest, err := e.State.Load(name)
		if err != nil {
			return err
		}
		if digest != p.digests[name] {
			return kindErr(KindStateStore, "state changed since the plan", fmt.Errorf("state of intent %q changed since the plan: plan again", name))
		}
	}
	if err := a.saveBase(); err != nil {
		return err
	}

	for _, c := range p.Changes {
		node := c.Intent + "/" + c.Key
		switch {
		case c.Gate == GateRefused: // nothing that is applied refers to it
		case c.Action == NoOp:
			a.values[node] = c.prior
		case c.Action == Delete:
		default:
			a.planned[node] = c.After
		}
	}
	if ctx.Err() != nil {
		return interrupted(0, len(order))
	}
	for _, name := range names {
		if a.pending[name] == 0 {
			if err := a.finish(name); err != nil {
				return kindErr(KindStateStore, "state or lock write failed", err)
			}
		}
	}
	for i, o := range order {
		if ctx.Err() != nil {
			return interrupted(i, len(order))
		}
		effects := a.effects
		err := a.run(ctx, o)
		if err != nil && ctx.Err() != nil && a.effects == effects {
			return interrupted(i, len(order)) // cut short before doing anything
		}
		a.observe(o, err)
		if err != nil {
			return err
		}
		if a.pending[o.c.Intent]--; a.pending[o.c.Intent] == 0 {
			if err := a.finish(o.c.Intent); err != nil {
				return kindErr(KindStateStore, "state or lock write failed", err)
			}
		}
	}
	if ctx.Err() != nil {
		return interrupted(len(order), len(order))
	}
	p.CheckResults = a.runChecks(ctx)
	return nil
}

// applier holds the progress of one Apply.
type applier struct {
	e *Engine
	p *Plan
	// values holds the state of each desired resource processed so far
	// (applied, or refreshed for a no-op); planned the state Plan planned
	// for the others. References resolve from values, then from planned.
	values, planned map[string]cty.Value
	// pending counts the operations still to run per intent.
	pending map[string]int
	// changed marks the intents in Dir whose hash differs from the lock's:
	// their lock intent_hash is "" until they complete. stale marks those
	// and the new intents: their lock entry must be rewritten.
	changed, stale map[string]bool
	// skipped marks the changes whose re-plan found nothing to do.
	skipped map[string]bool
	// executed marks the intents for which something was written.
	executed map[string]bool
	// held marks the intents that must not complete (see heldIntents).
	held map[string]bool
	// effects counts the ApplyResource calls and the state removals made:
	// an operation that failed without one did nothing.
	effects int
}

// involved returns, sorted, the intents Apply writes: those with operations
// and those whose lock entry is stale (new, rewritten or removed intents).
func (a *applier) involved(order []op) []string {
	set := map[string]bool{}
	for _, o := range order {
		set[o.c.Intent] = true
		a.pending[o.c.Intent]++
	}
	for _, it := range a.p.intents {
		li := a.p.lock.Intents[it.Name]
		if !a.p.upToDate(it, li) {
			set[it.Name] = true
			a.changed[it.Name], a.stale[it.Name] = li != nil, true
		}
	}
	for _, name := range a.p.removed {
		set[name] = true
	}
	return slices.Sorted(maps.Keys(set))
}

// lockIntents takes the state lock of each intent, in order; the returned
// function releases them all.
func (e *Engine) lockIntents(names []string) (func(), error) {
	var unlocks []func()
	release := func() {
		for i := len(unlocks) - 1; i >= 0; i-- {
			unlocks[i]()
		}
	}
	for _, name := range names {
		u, err := e.State.Lock(name)
		if err != nil {
			release()
			return nil, err
		}
		unlocks = append(unlocks, u)
	}
	return release, nil
}

func (a *applier) lookup(node string) (cty.Value, bool) {
	if v, ok := a.values[node]; ok {
		return v, true
	}
	v, ok := a.planned[node]
	return v, ok
}

// run executes one operation.
func (a *applier) run(ctx context.Context, o op) error {
	c := o.c
	node := c.Intent + "/" + c.Key
	switch {
	case a.skipped[node]:
		return nil
	case o.kind == 'D' && c.Action == Delete && c.prior.IsNull(): // already gone out of band
		a.progress(o, OpStart, c.prior, nil)
		return kindErr(KindStateStore, "state write failed", a.forget(c))
	case o.kind == 'D':
		if c.Action == Replace {
			if _, _, skip, err := a.replan(ctx, c); err != nil || skip {
				return err
			}
		}
		dp, err := a.p.planDestroy(ctx, c.Type, c.prior, c.priorPrivate, c.priorIdentity)
		if err != nil {
			return fmt.Errorf("%s: %w", node, err)
		}
		return a.destroy(ctx, c, dp)
	case c.Action == Replace: // the create half, after the destroy
		b := a.p.schema.Resources[c.Type].Block
		cfg, err := buildConfig(b, c.Intent, a.p.envName(), c.Key, c.config, a.p.keysOf, a.lookup)
		if err != nil {
			return refErr(node, err)
		}
		null := cty.NullVal(b.ImpliedType())
		pc, err := a.p.prov.PlanResource(ctx, c.Type, null, cfg, nil, nullIdentity)
		if err != nil {
			return fmt.Errorf("%s: %w", node, err)
		}
		return a.applyPlanned(ctx, c, null, pc, cfg)
	default: // create or update
		cfg, pc, skip, err := a.replan(ctx, c)
		if err != nil || skip {
			return err
		}
		return a.applyPlanned(ctx, c, c.prior, pc, cfg)
	}
}

// observe reports the operation o that ran with result err, to the
// Observer and to Progress; operations skipped for finding nothing to do are
// not reported.
func (a *applier) observe(o op, err error) {
	c := o.c
	if a.skipped[c.Intent+"/"+c.Key] {
		return
	}
	if err != nil {
		a.progress(o, OpFailed, cty.NilVal, err)
	} else {
		a.progress(o, OpDone, a.values[c.Intent+"/"+c.Key], nil)
	}
	if a.e.Observer == nil {
		return
	}
	phase := ""
	if c.Action == Replace {
		phase = "create"
		if o.kind == 'D' {
			phase = "destroy"
		}
	}
	a.e.Observer.ApplyOp(c.Intent, c.Key, c.Action, phase, Classify(err))
}

// replan plans c again from its prior with the values known now. A re-plan
// that finds nothing to do skips the change; any other different action is
// an error.
func (a *applier) replan(ctx context.Context, c *Change) (cty.Value, provider.PlannedChange, bool, error) {
	node := c.Intent + "/" + c.Key
	cfg, err := buildConfig(a.p.schema.Resources[c.Type].Block, c.Intent, a.p.envName(), c.Key, c.config, a.p.keysOf, a.lookup)
	if err != nil {
		return cty.NilVal, provider.PlannedChange{}, false, refErr(node, err)
	}
	pc, err := a.p.prov.PlanResource(ctx, c.Type, c.prior, cfg, c.priorPrivate, c.priorIdentity)
	if err != nil {
		return cty.NilVal, provider.PlannedChange{}, false, fmt.Errorf("%s: %w", node, err)
	}
	switch now := actionOf(c.prior, pc); now {
	case c.Action:
		return cfg, pc, false, nil
	case NoOp:
		a.skipped[node], a.values[node] = true, c.prior
		return cfg, pc, true, nil
	default:
		return cty.NilVal, provider.PlannedChange{}, false, kindErr(KindPlanChanged,
			fmt.Sprintf("plan changed for %s: was %s, now %s", node, c.Action, now),
			fmt.Errorf("plan changed for %s: was %s, now %s", node, c.Action, now))
	}
}

// applyPlanned applies pc from prior and records the new state.
func (a *applier) applyPlanned(ctx context.Context, c *Change, prior cty.Value, pc provider.PlannedChange, cfg cty.Value) error {
	node := c.Intent + "/" + c.Key
	b := a.p.schema.Resources[c.Type].Block
	if err := ctx.Err(); err != nil { // interrupted during the re-plan: start nothing
		return fmt.Errorf("%s: %w", node, err)
	}
	a.effects++
	defer a.e.InFlight.start(c)()
	a.progress(op{'C', c}, OpStart, prior, nil)
	res, applyErr := a.p.prov.ApplyResource(context.WithoutCancel(ctx), c.Type, prior, pc, cfg)
	if res.State == cty.NilVal || res.State.IsNull() {
		if applyErr != nil { // nothing was created or changed: keep the prior state
			return fmt.Errorf("%s: %w", node, applyErr)
		}
		return fmt.Errorf("%s: provider returned a null state", node)
	}
	v, writeOnly, err := stripWriteOnly(b, res.State)
	if err != nil {
		return fmt.Errorf("%s: %w", node, err)
	}
	if !v.IsWhollyKnown() {
		return errors.Join(wrapNode(node, applyErr), errUnknownState(node))
	}
	if err := a.save(c, v, res.Private, res.Identity); err != nil {
		return errors.Join(wrapNode(node, applyErr), kindErr(KindStateStore, node+": state write failed", err))
	}
	a.values[node] = v
	switch {
	case applyErr != nil:
		return fmt.Errorf("%s: %w", node, applyErr)
	case len(writeOnly) > 0:
		return kindErr(KindWriteOnly, node+": provider returned a write-only value", fmt.Errorf("%s: provider returned a value for write-only attribute(s) %s; not persisted", node, strings.Join(writeOnly, ", ")))
	}
	return nil
}

// destroy applies the destroy plan dp of c's prior object.
func (a *applier) destroy(ctx context.Context, c *Change, dp provider.PlannedChange) error {
	node := c.Intent + "/" + c.Key
	null := cty.NullVal(a.p.schema.Resources[c.Type].Block.ImpliedType())
	if err := ctx.Err(); err != nil { // interrupted during the re-plan: start nothing
		return fmt.Errorf("%s: %w", node, err)
	}
	a.effects++
	defer a.e.InFlight.start(c)()
	a.progress(op{'D', c}, OpStart, c.prior, nil)
	res, err := a.p.prov.ApplyResource(context.WithoutCancel(ctx), c.Type, c.prior, dp, null)
	if res.State == cty.NilVal || res.State.IsNull() {
		if err != nil { // keep the prior state
			return fmt.Errorf("%s: %w", node, err)
		}
		return kindErr(KindStateStore, "state write failed", a.forget(c))
	}
	// The object still exists: record what the provider reports.
	if err == nil {
		err = errors.New("provider returned a non-null state after a destroy")
	}
	v, _, serr := stripWriteOnly(a.p.schema.Resources[c.Type].Block, res.State)
	switch {
	case serr != nil:
	case !v.IsWhollyKnown():
		serr = errUnknownState(node)
	default:
		serr = kindErr(KindStateStore, node+": state write failed", a.save(c, v, res.Private, res.Identity))
	}
	return errors.Join(fmt.Errorf("%s: %w", node, err), serr)
}

// errUnknownState is the error for a provider state that still has unknown
// values after an apply: it cannot be stored, the prior state is kept.
func errUnknownState(node string) error {
	return kindErr(KindUnknown, node+": provider returned unknown values",
		fmt.Errorf("%s: provider returned a state with unknown values; not persisted, the prior state is kept", node))
}

func wrapNode(node string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", node, err)
}
