package engine

import (
	"fmt"
	"strings"
)

// policy is the set of rules of ADR-022, in two families: violations of a
// desired resource, which are validation problems the planner gets back, and
// the least gate a change needs, which can only raise the gate its intent's
// autonomy gives. Cedar or OPA can implement it later.
type policy interface {
	// violations returns why r breaks the rules, nil when it does not.
	violations(r DesiredResource) []string
	// minGate returns the least gate c needs and why; allowDestroy is the
	// allow_destroy of c's intent.
	minGate(c *Change, allowDestroy bool) (Gate, string)
}

// newPolicy returns the phase-0 policy, in region.
func newPolicy(region string) policy { return builtinPolicy{region: region} }

// region is e.Region, AWSRegion() when empty.
func (e *Engine) region() string {
	if e.Region != "" {
		return e.Region
	}
	return AWSRegion()
}

// builtinPolicy keeps every resource in one region and asks approval for
// deletes and replaces, unless the intent allows destroys.
type builtinPolicy struct{ region string }

// violations reports a top-level region attribute set to another region. A
// reference ("${...}") is not known before the apply and is not checked; an
// empty region is the configured one (hashicorp/aws plans it so).
func (b builtinPolicy) violations(r DesiredResource) []string {
	v, ok := r.Config["region"].(string)
	if !ok || v == "" || v == b.region || strings.Contains(v, "${") {
		return nil
	}
	return []string{fmt.Sprintf("region %q is not allowed: resources must stay in %q (policy)", v, b.region)}
}

func (b builtinPolicy) minGate(c *Change, allowDestroy bool) (Gate, string) {
	if (c.Action == Delete || c.Action == Replace) && !allowDestroy {
		return GateNeedsApproval, "destroy"
	}
	return GateAuto, ""
}
