package engine

import "fmt"

// op is one step of Apply: the create or update ("C") or the destroy ("D")
// of a change. A replace has both, the destroy first.
type op struct {
	kind byte // 'C' or 'D'
	c    *Change
}

func (o op) id() string { return string(o.kind) + ":" + o.c.Intent + "/" + o.c.Key }

// applyOrder returns the operations of p's changes in execution order, with
// Terraform's default (destroy-before-create) rules, where "x refers to d"
// also means that d is in x's depends_on (ADR-020):
//
//	a. C(x) after C(d) for each d that x refers to in the desired graph;
//	b. D(x) after D(y) for each y that referred to x in the lock;
//	c. D(x) after C(y) for each y without a destroy that referred to x in the
//	   lock and no longer does (y moves off x first);
//	d. C(x) after D(x) for a replace.
//
// Ties are broken by operation id. A cycle is an error, before any change.
// Refused changes (ADR-022) have no operations.
func (p *Plan) applyOrder() ([]op, error) {
	ops, deps, err := p.opGraph(func(c *Change) bool { return c.Gate != GateRefused })
	if err != nil || len(ops) == 0 {
		return nil, err
	}
	order, err := TopoSort(deps)
	if err != nil {
		return nil, fmt.Errorf("cannot order the changes: %w", err)
	}
	out := make([]op, len(order))
	for i, id := range order {
		out[i] = ops[id]
	}
	return out, nil
}

// opGraph returns the operations of the changes include accepts, by id, and
// the operations each one runs after (see applyOrder).
func (p *Plan) opGraph(include func(*Change) bool) (map[string]op, map[string][]string, error) {
	ops := map[string]op{}
	hasC, hasD := map[string]bool{}, map[string]bool{}
	for i := range p.Changes {
		c := &p.Changes[i]
		if !include(c) {
			continue
		}
		node := c.Intent + "/" + c.Key
		if c.Action == Delete || c.Action == Replace {
			o := op{'D', c}
			ops[o.id()], hasD[node] = o, true
		}
		if c.Action == Create || c.Action == Update || c.Action == Replace {
			o := op{'C', c}
			ops[o.id()], hasC[node] = o, true
		}
	}
	if len(ops) == 0 {
		return nil, nil, nil
	}

	newRefs := map[string]map[string]bool{}
	for node := range hasC {
		c := ops["C:"+node].c
		refs, err := CollectRefs(c.config, p.keysOf)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", node, err)
		}
		newRefs[node] = map[string]bool{}
		for _, r := range refs {
			newRefs[node][r.Node()] = true
		}
		for _, d := range c.dependsOn {
			newRefs[node][d] = true
		}
	}
	lockKeys := lockKeysOf(p.lock)
	oldRefs := map[string][]string{}
	for name, li := range p.lock.Intents {
		for _, r := range li.Resources {
			refs, err := CollectRefs(r.Chosen, func(n string) []string { return lockKeys[n] })
			if err != nil {
				return nil, nil, fmt.Errorf("lock: %s/%s: %w", name, r.Key, err)
			}
			for _, ref := range refs {
				oldRefs[name+"/"+r.Key] = append(oldRefs[name+"/"+r.Key], ref.Node())
			}
			oldRefs[name+"/"+r.Key] = append(oldRefs[name+"/"+r.Key], r.DependsOn...)
		}
	}

	deps := map[string][]string{}
	for id := range ops {
		deps[id] = nil
	}
	for x := range hasC {
		for d := range newRefs[x] {
			if hasC[d] {
				deps["C:"+x] = append(deps["C:"+x], "C:"+d) // a
			}
		}
		if hasD[x] {
			deps["C:"+x] = append(deps["C:"+x], "D:"+x) // d
		}
	}
	for y, xs := range oldRefs {
		for _, x := range xs {
			switch {
			case !hasD[x]:
			case hasD[y]:
				deps["D:"+x] = append(deps["D:"+x], "D:"+y) // b
			case hasC[y] && !newRefs[y][x]:
				deps["D:"+x] = append(deps["D:"+x], "C:"+y) // c
			}
		}
	}
	return ops, deps, nil
}
