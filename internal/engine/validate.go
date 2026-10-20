package engine

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/zclconf/go-cty/cty"

	"github.com/giammbo/prosa/internal/checks"
	"github.com/giammbo/prosa/internal/lock"
	"github.com/giammbo/prosa/internal/provider"
)

// ValidationProblem is one reason a desired resource is invalid, as found
// before any refresh: a sensitive literal or reference, a configuration that
// does not fit the schema, or a provider validation error. M3 feeds the
// problems back to the planner.
type ValidationProblem struct {
	Intent, Key, Type, Message string
}

// ValidationError lists every problem found in the desired state, sorted by
// intent and key.
type ValidationError struct {
	Problems []ValidationProblem
}

// newValidationError sorts probs by intent and key.
func newValidationError(probs []ValidationProblem) *ValidationError {
	slices.SortStableFunc(probs, func(a, b ValidationProblem) int {
		return cmp.Or(strings.Compare(a.Intent, b.Intent), strings.Compare(a.Key, b.Key))
	})
	return &ValidationError{Problems: probs}
}

func (e *ValidationError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "invalid desired state (%d problem(s)):", len(e.Problems))
	for _, p := range e.Problems {
		fmt.Fprintf(&b, "\n  %s/%s (%s): %s", p.Intent, p.Key, p.Type, p.Message)
	}
	return b.String()
}

// validate checks every desired resource and check before any refresh
// (ADR-011, ADR-021, the policy of ADR-022 and the provider's own
// validation) and returns a
// *ValidationError listing all the problems. A reference is an unknown value
// of the type its path has in the schema of the resource it points to, so a
// path the schema does not have, or a value of the wrong type, is a problem
// before any apply.
//
// pre are the problems found earlier (invalid references, cycles); badRefs
// the nodes whose references are invalid, which skip the substitution and the
// provider's validation but get every other check.
func (p *Plan) validate(ctx context.Context, pre []ValidationProblem, badRefs map[string]bool) error {
	var names []string
	for _, it := range p.intents {
		names = append(names, it.Name)
	}
	graphBad := map[string]bool{} // nodes with a graph problem: no provider call
	for _, pr := range pre {
		graphBad[pr.Intent+"/"+pr.Key] = true
	}
	probs, err := p.checkResources(ctx, p.desired, names, badRefs, graphBad, nil)
	if err != nil {
		return err
	}
	probs = append(probs, p.allCheckProblems()...)
	probs = append(slices.Clone(pre), probs...)
	if len(probs) == 0 {
		return nil
	}
	return newValidationError(probs)
}

// checkResources returns the problems of the desired resources of the named
// intents, references resolved against desired (every intent in Dir) as
// unknown values; see validate. The error is a transport or context failure.
//
// graphBad holds the nodes with a problem found by desiredOrder (a bad
// depends_on, a cycle): they get every check but the provider's.
//
// plans holds the nodes ("<intent>/<key>") whose resource, when it passes
// every other check, is also planned, read-only, from the prior state given
// (a null one for a new key), so that the provider's plan-time checks (the
// CustomizeDiff of SDKv2) report their errors as problems. It is nil when no
// node needs it.
func (p *Plan) checkResources(ctx context.Context, desired map[string][]DesiredResource, names []string, badRefs, graphBad map[string]bool, plans map[string]validationPrior) ([]ValidationProblem, error) {
	var probs []ValidationProblem
	pol := newPolicy(p.region)
	keysOf := keysIn(desired)
	targetOf := func(r Ref) (string, *provider.Block) {
		for _, d := range desired[r.Intent] {
			if d.Key == r.Key {
				if rs := p.schema.Resources[d.Type]; rs != nil {
					return d.Type, rs.Block
				}
			}
		}
		return "", nil
	}
	sourceOf := func(r Ref) *provider.Block {
		_, b := targetOf(r)
		return b
	}
	for _, name := range names {
		rs := slices.Clone(desired[name])
		slices.SortFunc(rs, func(a, b DesiredResource) int { return strings.Compare(a.Key, b.Key) })
		for _, r := range rs {
			add := func(msg string) {
				probs = append(probs, ValidationProblem{Intent: name, Key: r.Key, Type: r.Type, Message: msg})
			}
			where := name + "/" + r.Key
			before := len(probs)
			for _, m := range pol.violations(r) {
				add(m)
			}
			rsch := p.schema.Resources[r.Type]
			if rsch == nil {
				add(fmt.Sprintf("unknown resource type %q", r.Type))
				continue
			}
			b := rsch.Block
			if bad := configNumberProblems(r.Config); len(bad) > 0 {
				for _, m := range bad {
					add(m)
				}
				continue
			}
			if err := checkSensitive(b, r.Config, where); err != nil {
				add(err.Error())
				continue
			}
			var types map[string]cty.Type
			var refProbs []string
			if !badRefs[where] {
				types, refProbs = refTypes(r.Config, keysOf, targetOf)
				for _, m := range refProbs {
					add(m)
				}
			}
			if len(refProbs) == 0 {
				if msgs := sensitiveRefs(b, r.Config, where, keysOf, sourceOf); len(msgs) > 0 {
					for _, m := range msgs {
						add(m)
					}
					continue
				}
			}
			if badRefs[where] {
				continue
			}
			sub, err := Substitute(r.Config, keysOf, func(ref Ref) (cty.Value, error) {
				if ty, ok := types[ref.String()]; ok {
					return cty.UnknownVal(ty), nil
				}
				return cty.DynamicVal, nil
			})
			if err != nil {
				add(err.Error())
				continue
			}
			if err := encodeJSONDocuments(b, sub); err != nil {
				add(err.Error())
				continue
			}
			if tags, isCty := sub["tags"].(cty.Value); !isCty || tags.IsKnown() {
				if err := addOwnerTag(b, sub, name, p.envName()); err != nil {
					add(err.Error())
					continue
				}
			}
			v, err := provider.ObjectFromMap(b, sub)
			if err != nil {
				for _, e := range splitJoined(err) {
					add(e.Error())
				}
				continue
			}
			if len(refProbs) > 0 {
				continue
			}
			if graphBad[where] {
				continue
			}
			err = p.prov.ValidateResource(ctx, r.Type, v)
			if pr, ok := plans[where]; ok && err == nil && len(probs) == before {
				prior, identity := cty.NullVal(b.ImpliedType()), nullIdentity
				if pr.state != cty.NilVal {
					prior, identity = pr.state, pr.identity
				}
				_, err = p.prov.PlanResource(ctx, r.Type, prior, v, pr.private, identity)
			}
			if err != nil {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				var te *provider.TransportError
				if errors.As(err, &te) {
					return nil, err
				}
				for _, e := range splitJoined(err) {
					add(e.Error())
				}
			}
		}
	}
	return probs, nil
}

// validationPrior is the prior state a resource is planned from in
// validation (see checkResources): the zero value is a null one.
type validationPrior struct {
	state, identity cty.Value
	private         []byte
}

// checkFields lists, by check type, the fields a check of that type may set
// besides its name and type (ADR-021).
var checkFields = map[string][]string{
	checks.ResourceAttr: {"op", "path", "resource", "value"},
	checks.HTTPGet:      {"status", "url", "within"},
	checks.TLSDays:      {"host", "min_days"},
	checks.Unverifiable: {"reason"},
}

// setCheckFields returns, sorted, the fields of ck that are set besides its
// name and type.
func setCheckFields(ck lock.Check) []string {
	var out []string
	for _, f := range []struct {
		name string
		set  bool
	}{
		{"host", ck.Host != ""}, {"min_days", ck.MinDays != 0}, {"op", ck.Op != ""}, {"path", ck.Path != ""},
		{"reason", ck.Reason != ""}, {"resource", ck.Resource != ""}, {"status", ck.Status != 0},
		{"url", ck.URL != ""}, {"value", ck.Value != nil}, {"within", ck.Within != ""},
	} {
		if f.set {
			out = append(out, f.name)
		}
	}
	return out
}

// hostName is a DNS name or an IPv4 address, with an optional port.
var hostName = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?(:[0-9]{1,5})?$`)

// checkProblems returns the problems of the checks cks of intent name, whose
// frontmatter depends_on is deps: a name, unique in the intent; a known type
// with only its own fields, each valid; for resource_attr, a resource of the
// intent or of one in deps among desired, a path its schema has, an operator
// that fits the attribute's type and a value that fits the operator. Key is
// "checks[<i>]".
func (p *Plan) checkProblems(name string, deps []string, cks []lock.Check, desired map[string][]DesiredResource) []ValidationProblem {
	var probs []ValidationProblem
	seen := map[string]bool{}
	for i, ck := range cks {
		add := func(msg string) {
			probs = append(probs, ValidationProblem{Intent: name, Key: fmt.Sprintf("checks[%d]", i), Type: ck.Type, Message: msg})
		}
		switch n := strings.TrimSpace(ck.Name); {
		case n == "":
			add("a check needs a name: the sentence of the Checks section it checks")
		case seen[n]:
			add(fmt.Sprintf("duplicate check name %q: one check per sentence", ck.Name))
		default:
			seen[n] = true
		}
		allowed, known := checkFields[ck.Type]
		if !known {
			add(fmt.Sprintf("unknown check type %q: use resource_attr, http_get, tls_days or unverifiable", ck.Type))
			continue
		}
		var foreign []string
		for _, f := range setCheckFields(ck) {
			if !slices.Contains(allowed, f) {
				foreign = append(foreign, f)
			}
		}
		if len(foreign) > 0 {
			add(fmt.Sprintf("%s does not use %s", ck.Type, strings.Join(foreign, ", ")))
			continue
		}
		var msgs []string
		switch ck.Type {
		case checks.ResourceAttr:
			msgs = p.resourceAttrProblems(name, deps, ck, desired)
		case checks.HTTPGet:
			msgs = httpGetProblems(ck)
		case checks.TLSDays:
			if ck.Host == "" {
				msgs = append(msgs, "tls_days needs host")
			} else if !hostName.MatchString(ck.Host) {
				msgs = append(msgs, fmt.Sprintf("host %q must be a host name, as in staging.acme.example", ck.Host))
			}
			if ck.MinDays < 1 {
				msgs = append(msgs, "tls_days needs min_days, at least 1")
			}
		case checks.Unverifiable:
			if strings.TrimSpace(ck.Reason) == "" {
				msgs = append(msgs, "unverifiable needs a reason")
			}
		}
		for _, m := range msgs {
			add(m)
		}
	}
	return probs
}

// httpGetProblems returns the problems of an http_get check.
func httpGetProblems(ck lock.Check) []string {
	var msgs []string
	if ck.URL == "" {
		msgs = append(msgs, "http_get needs url")
	} else if u, err := url.Parse(ck.URL); err != nil || u.Scheme != "https" || u.Host == "" {
		msgs = append(msgs, fmt.Sprintf("url %q must be an https URL with a host", ck.URL))
	}
	if ck.Status != 0 && (ck.Status < 100 || ck.Status > 599) {
		msgs = append(msgs, fmt.Sprintf("status %d is not an HTTP status", ck.Status))
	}
	if ck.Within != "" {
		if d, err := time.ParseDuration(ck.Within); err != nil || d <= 0 {
			msgs = append(msgs, fmt.Sprintf("within %q is not a positive duration such as \"60s\"", ck.Within))
		}
	}
	return msgs
}

// resourceAttrProblems returns the problems of a resource_attr check of
// intent name (see checkProblems).
func (p *Plan) resourceAttrProblems(name string, deps []string, ck lock.Check, desired map[string][]DesiredResource) []string {
	if ck.Resource == "" || ck.Path == "" || ck.Op == "" {
		return []string{"resource_attr needs resource, path and op"}
	}
	intentName, key, found := strings.Cut(ck.Resource, "/")
	if !found || intentName == "" || key == "" {
		return []string{fmt.Sprintf("resource %q: expected \"<intent>/<key>\"", ck.Resource)}
	}
	if intentName != name && !slices.Contains(deps, intentName) {
		if len(deps) == 0 {
			return []string{fmt.Sprintf("resource %q: a check can read only intent %s, which has no depends_on", ck.Resource, name)}
		}
		return []string{fmt.Sprintf("resource %q: a check can read only intent %s and those in its depends_on (%s)", ck.Resource, name, strings.Join(deps, ", "))}
	}
	typeName := ""
	for _, r := range desired[intentName] {
		if r.Key == key {
			typeName = r.Type
		}
	}
	if typeName == "" {
		return []string{fmt.Sprintf("resource %q: unknown key %s", ck.Resource, key)}
	}
	rs := p.schema.Resources[typeName]
	if rs == nil {
		return nil // an unknown type: reported on the resource
	}
	attrs := strings.Split(ck.Path, ".")
	ty, err := refType(rs.Block, typeName, Ref{Intent: intentName, Key: key, Attr: attrs})
	if err != nil {
		return []string{fmt.Sprintf("path %q: %v", ck.Path, err)}
	}
	if ck.Op != "exists" && sourceSensitive(rs.Block, attrs) {
		return []string{fmt.Sprintf("path %q reads a sensitive value: only op exists can check it (ADR-011)", ck.Path)}
	}
	if !slices.Contains(checks.Ops, ck.Op) {
		return []string{fmt.Sprintf("unknown op %q: use eq, ne, gte, lte, contains, matches, within or exists", ck.Op)}
	}
	if ck.Op == "exists" {
		if ck.Value != nil {
			return []string{"op exists takes no value"}
		}
		return nil
	}
	if ck.Value == nil {
		return []string{fmt.Sprintf("op %s needs a value", ck.Op)}
	}
	if bad := configNumberProblems(map[string]any{"value": ck.Value}); len(bad) > 0 {
		for i := range bad {
			bad[i] = strings.TrimPrefix(bad[i], "config.")
		}
		return bad
	}
	if ty.Equals(cty.DynamicPseudoType) {
		return nil // any value may fit
	}
	shown, _ := json.Marshal(ck.Value)
	fits := func(t cty.Type) bool { _, err := checks.ConvertValue(ck.Value, t); return err == nil }
	notFit := fmt.Sprintf("value %s does not fit %s, a %s", shown, ck.Path, ty.FriendlyName())
	wrongType := func(want string) []string {
		return []string{fmt.Sprintf("op %s needs %s attribute; %s is a %s", ck.Op, want, ck.Path, ty.FriendlyName())}
	}
	s, isString := ck.Value.(string)
	switch ck.Op {
	case "eq", "ne":
		if !fits(ty) {
			return []string{notFit}
		}
	case "gte", "lte":
		if !ty.Equals(cty.Number) {
			return wrongType("a number")
		}
		if !fits(ty) {
			return []string{notFit}
		}
	case "contains":
		switch {
		case ty.Equals(cty.String):
			if !isString {
				return []string{fmt.Sprintf("value %s must be a string: %s is a string", shown, ck.Path)}
			}
		case ty.IsListType() || ty.IsSetType():
			if !fits(ty.ElementType()) {
				return []string{fmt.Sprintf("value %s does not fit an element of %s, a %s", shown, ck.Path, ty.FriendlyName())}
			}
		default:
			return wrongType("a string, list or set")
		}
	case "matches":
		if !ty.Equals(cty.String) {
			return wrongType("a string")
		}
		if !isString {
			return []string{fmt.Sprintf("value %s must be a regular expression, as a string", shown)}
		}
		if _, err := regexp.Compile(s); err != nil {
			return []string{fmt.Sprintf("value %s is not a regular expression: %v", shown, err)}
		}
	case "within":
		if !ty.Equals(cty.String) {
			return []string{fmt.Sprintf("op within needs a string attribute holding a timestamp; %s is a %s", ck.Path, ty.FriendlyName())}
		}
		if d, err := time.ParseDuration(s); !isString || err != nil || d <= 0 {
			return []string{fmt.Sprintf("value %s is not a duration such as \"24h\"", shown)}
		}
	}
	return nil
}

// refTypes returns, by reference (Ref.String), the type of every reference
// in cfg whose target resource has a known type, and one problem per
// reference whose path that type's schema does not have or that is part of
// a template without being a string, number or bool (ADR-020). Invalid references
// are ignored: desiredOrder reports them.
func refTypes(cfg map[string]any, keysOf func(string) []string, targetOf func(Ref) (string, *provider.Block)) (map[string]cty.Type, []string) {
	refs, err := CollectRefs(cfg, keysOf)
	if err != nil {
		return nil, nil
	}
	inTemplate := templateRefs(cfg, keysOf)
	types := map[string]cty.Type{}
	var probs []string
	for _, r := range refs {
		typeName, src := targetOf(r)
		if src == nil {
			continue // an unknown type: reported on the target
		}
		ty, err := refType(src, typeName, r)
		if err != nil {
			probs = append(probs, fmt.Sprintf("reference %s: %v", r, err))
			continue
		}
		if inTemplate[r.String()] && !ty.Equals(cty.DynamicPseudoType) && !ty.IsPrimitiveType() {
			probs = append(probs, fmt.Sprintf("reference %s in a template: it is a %s; a template takes only a string, number or bool", r, ty.FriendlyName()))
			continue
		}
		types[r.String()] = ty
	}
	return types, probs
}

// templateRefs returns the references (Ref.String) that are part of a
// template somewhere in cfg. Invalid templates are ignored.
func templateRefs(cfg map[string]any, keysOf func(string) []string) map[string]bool {
	out := map[string]bool{}
	walkStrings(cfg, func(s string) {
		parts, _, _ := ParseTemplate(s, keysOf)
		for _, p := range parts {
			if p.Ref != nil {
				out[p.Ref.String()] = true
			}
		}
	})
	return out
}

// refType returns the type of the attribute path of r in a resource of type
// typeName and block b, selecting as attrOf does on values: object
// attributes, map keys, list and tuple indexes; a set has no index.
func refType(b *provider.Block, typeName string, r Ref) (cty.Type, error) {
	ty := b.ImpliedType()
	for i, a := range r.Attr {
		path := strings.Join(r.Attr[:i], ".")
		switch {
		case ty.Equals(cty.DynamicPseudoType):
			return ty, nil
		case ty.IsObjectType():
			if !ty.HasAttribute(a) {
				if i == 0 {
					return cty.NilType, fmt.Errorf("%s (%s) has no attribute %q", r.Node(), typeName, a)
				}
				return cty.NilType, fmt.Errorf("%s has no attribute %q", path, a)
			}
			ty = ty.AttributeType(a)
		case ty.IsMapType():
			ty = ty.ElementType()
		case ty.IsListType() || ty.IsTupleType():
			n, err := strconv.Atoi(a)
			if err != nil || n < 0 || (ty.IsTupleType() && n >= len(ty.TupleElementTypes())) {
				hint := path + ".0"
				if et := elemType(ty); et.IsObjectType() && et.HasAttribute(a) {
					hint += "." + a
				}
				return cty.NilType, fmt.Errorf("cannot select %q in %s, a list: use an index, as in %s", a, path, hint)
			}
			if ty.IsTupleType() {
				ty = ty.TupleElementType(n)
			} else {
				ty = ty.ElementType()
			}
		case ty.IsSetType():
			return cty.NilType, fmt.Errorf("cannot select %q in %s, a set: its elements have no index", a, path)
		default:
			return cty.NilType, fmt.Errorf("cannot select %q in %s, a %s", a, path, ty.FriendlyName())
		}
	}
	return ty, nil
}

// elemType is the element type of a list, the first element type of a
// tuple (cty.NilType when empty).
func elemType(ty cty.Type) cty.Type {
	if ty.IsListType() {
		return ty.ElementType()
	}
	if ets := ty.TupleElementTypes(); len(ets) > 0 {
		return ets[0]
	}
	return cty.NilType
}

// configNumberProblems returns one message per json.Number in cfg that the
// lock could not store (see lock.CheckNumber), naming its path.
func configNumberProblems(cfg map[string]any) []string {
	var out []string
	var walk func(v any, path string)
	walk = func(v any, path string) {
		if n, ok := v.(json.Number); ok {
			if err := lock.CheckNumber(n); err != nil {
				out = append(out, fmt.Sprintf("%s: %v", path, err))
			}
			return
		}
		if v == nil {
			return
		}
		if kids, _, ok, err := children(v, path); err == nil && ok {
			for _, c := range kids {
				walk(c.value, c.path)
			}
		}
	}
	walk(cfg, "config")
	return out
}

// splitJoined returns the errors joined in err (errors.Join), or err alone.
func splitJoined(err error) []error {
	if j, ok := err.(interface{ Unwrap() []error }); ok {
		var out []error
		for _, e := range j.Unwrap() {
			out = append(out, splitJoined(e)...)
		}
		return out
	}
	return []error{err}
}
