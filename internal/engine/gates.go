package engine

import (
	"maps"
	"slices"
)

// Gate says whether a change runs on its own, after an approval, or not at
// all (ADR-022). The empty gate is auto.
type Gate string

const (
	GateAuto          Gate = "auto"
	GateNeedsApproval Gate = "needs_approval"
	GateRefused       Gate = "refused"
)

// rank orders the gates: a rule can only raise one.
func (g Gate) rank() int {
	switch g {
	case GateRefused:
		return 2
	case GateNeedsApproval:
		return 1
	}
	return 0
}

// autonomyGate is the gate the autonomy of an intent gives its changes, and
// why when they are refused.
func autonomyGate(autonomy string) (Gate, string) {
	switch autonomy {
	case "observe", "advise":
		return GateRefused, autonomy
	case "auto":
		return GateAuto, ""
	}
	return GateNeedsApproval, ""
}

// setGates sets the gate of every change that is not a no-op: the one of its
// intent's autonomy, raised by pol. A removed intent has no frontmatter left:
// its changes are gated as approve's.
func (p *Plan) setGates(pol policy) {
	autonomy, allowDestroy := map[string]string{}, map[string]bool{}
	for _, it := range p.intents {
		autonomy[it.Name], allowDestroy[it.Name] = it.Autonomy, it.AllowDestroy
	}
	for i := range p.Changes {
		c := &p.Changes[i]
		if c.Action == NoOp {
			continue
		}
		g, why := autonomyGate(autonomy[c.Intent])
		pg, pwhy := pol.minGate(c, allowDestroy[c.Intent])
		if pg.rank() > g.rank() || (pg == g && why == "") {
			g, why = pg, pwhy
		}
		c.Gate, c.GateReason = g, why
	}
}

// prune refuses every change that depends on a refused one through the
// operation graph (see applyOrder: references, depends_on and the destroy
// order), transitively, with the reason "depends on <intent>/<key>" of the
// first refused one it waits for.
func (p *Plan) prune() error {
	ops, deps, err := p.opGraph(func(*Change) bool { return true })
	if err != nil {
		return err
	}
	ids := slices.Sorted(maps.Keys(ops))
	for again := true; again; {
		again = false
		for _, id := range ids {
			c := ops[id].c
			if c.Gate == GateRefused {
				continue
			}
			var on []string
			for _, d := range deps[id] {
				if dc := ops[d].c; dc.Gate == GateRefused {
					on = append(on, dc.Intent+"/"+dc.Key)
				}
			}
			if len(on) > 0 {
				c.Gate, c.GateReason, again = GateRefused, "depends on "+slices.Min(on), true
			}
		}
	}
	return nil
}

// hasRefused reports whether a change is refused.
func (p *Plan) hasRefused() bool {
	return slices.ContainsFunc(p.Changes, func(c Change) bool { return c.Action != NoOp && c.Gate == GateRefused })
}

// Applicable reports whether at least one change that is not a no-op can be
// applied: it is not refused.
func (p *Plan) Applicable() bool {
	return slices.ContainsFunc(p.Changes, func(c Change) bool { return c.Action != NoOp && c.Gate != GateRefused })
}

// ToApprove returns the changes that need an approval, in plan order.
func (p *Plan) ToApprove() []Change {
	var out []Change
	for _, c := range p.Changes {
		if c.Action != NoOp && c.Gate == GateNeedsApproval {
			out = append(out, c)
		}
	}
	return out
}

// refuseUnapproved refuses, as "not approved", the changes that needed an
// approval, and prunes their dependents.
func (p *Plan) refuseUnapproved() error {
	for i := range p.Changes {
		if c := &p.Changes[i]; c.Action != NoOp && c.Gate == GateNeedsApproval {
			c.Gate, c.GateReason = GateRefused, "not approved"
		}
	}
	return p.prune()
}

// heldIntents returns the intents whose lock entry Apply must not complete:
// those with a refused change, and those with a desired resource that refers
// to, or depends on, a resource with a refused change. They keep the lock's
// intent_hash (or get an empty one when something of theirs was applied), so
// that they are planned again.
func (p *Plan) heldIntents() map[string]bool {
	held, refused := map[string]bool{}, map[string]bool{}
	for _, c := range p.Changes {
		if c.Action != NoOp && c.Gate == GateRefused {
			held[c.Intent], refused[c.Intent+"/"+c.Key] = true, true
		}
	}
	if len(refused) == 0 {
		return held
	}
	for name, rs := range p.desired {
		for _, r := range rs {
			refs, _ := CollectRefs(r.Config, p.keysOf) // validated by Plan
			for _, ref := range refs {
				held[name] = held[name] || refused[ref.Node()]
			}
			for _, d := range r.DependsOn {
				held[name] = held[name] || refused[d]
			}
		}
	}
	return held
}
