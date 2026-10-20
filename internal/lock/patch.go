package lock

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// PatchOp is one operation of an environment's patch on the base (ADR-030).
// Op is one of: set {key, path, value}, unset {key, path}, add {key, type,
// config, depends_on}, remove {key}, depends_on {key, value: list of nodes}
// and checks {value: the complete list of checks}. Path is dotted, with the
// index for lists (ingress.0.from_port).
type PatchOp struct {
	Op        string         `yaml:"op" json:"op"`
	Key       string         `yaml:"key,omitempty" json:"key,omitempty"`
	Path      string         `yaml:"path,omitempty" json:"path,omitempty"`
	Value     any            `yaml:"value,omitempty" json:"value,omitempty"`
	Type      string         `yaml:"type,omitempty" json:"type,omitempty"`
	Config    map[string]any `yaml:"config,omitempty" json:"config,omitempty"`
	DependsOn []string       `yaml:"depends_on,omitempty" json:"depends_on,omitempty"`
}

// PatchError says which operation of the patch did not apply and why.
type PatchError struct {
	Index  int
	Op     PatchOp
	Reason string
}

func (e *PatchError) Error() string {
	target := e.Op.Key
	if e.Op.Path != "" {
		target += "." + e.Op.Path
	}
	return fmt.Sprintf("patch op %d (%s %s): %s", e.Index, e.Op.Op, target, e.Reason)
}

// ApplyPatch returns the resources and checks of the base with the patch
// applied, in order; the arguments are not modified. A failing operation is
// reported as a *PatchError and nothing is returned.
func ApplyPatch(resources []Resource, checks []Check, ops []PatchOp) ([]Resource, []Check, error) {
	rs := make([]Resource, len(resources))
	for i, r := range resources {
		r.Chosen, _ = deepCopy(r.Chosen).(map[string]any)
		r.DependsOn = slices.Clone(r.DependsOn)
		rs[i] = r
	}
	cs := make([]Check, len(checks))
	for i, c := range checks {
		c.Value = deepCopy(c.Value)
		cs[i] = c
	}
	for i, op := range ops {
		var reason string
		rs, cs, reason = applyOp(rs, cs, op)
		if reason != "" {
			return nil, nil, &PatchError{Index: i, Op: op, Reason: reason}
		}
	}
	return rs, cs, nil
}

func applyOp(rs []Resource, cs []Check, op PatchOp) ([]Resource, []Check, string) {
	if op.Op == "checks" {
		nc, err := checksFromValue(op.Value)
		if err != "" {
			return rs, cs, err
		}
		return rs, nc, ""
	}
	idx := slices.IndexFunc(rs, func(r Resource) bool { return r.Key == op.Key })
	switch op.Op {
	case "add":
		if op.Key == "" || op.Type == "" {
			return rs, cs, "add needs key and type"
		}
		if idx >= 0 {
			return rs, cs, fmt.Sprintf("resource %q already exists", op.Key)
		}
		cfg, _ := deepCopy(op.Config).(map[string]any)
		if cfg == nil {
			cfg = map[string]any{}
		}
		return append(rs, Resource{Key: op.Key, Provider: providerOf(op.Type), Type: op.Type, Chosen: cfg,
			DependsOn: slices.Clone(op.DependsOn)}), cs, ""
	case "set", "unset", "remove", "depends_on":
	default:
		return rs, cs, fmt.Sprintf("unknown op %q", op.Op)
	}
	if idx < 0 {
		return rs, cs, fmt.Sprintf("resource %q not found", op.Key)
	}
	switch op.Op {
	case "remove":
		return slices.Delete(rs, idx, idx+1), cs, ""
	case "depends_on":
		deps, ok := stringList(op.Value)
		if !ok && op.Value == nil {
			deps, ok = slices.Clone(op.DependsOn), true
		}
		if !ok {
			return rs, cs, "depends_on needs a list of strings"
		}
		rs[idx].DependsOn = deps
		return rs, cs, ""
	}
	if op.Path == "" {
		return rs, cs, op.Op + " needs a path"
	}
	segs := strings.Split(op.Path, ".")
	if rs[idx].Chosen == nil {
		rs[idx].Chosen = map[string]any{}
	}
	var reason string
	if op.Op == "set" {
		reason = setPath(rs[idx].Chosen, segs, deepCopy(op.Value))
	} else {
		reason = unsetPath(rs[idx].Chosen, segs)
	}
	return rs, cs, reason
}

// setPath writes v at segs inside node (a map); missing maps along the way
// are created, a list index must exist.
func setPath(node any, segs []string, v any) string {
	seg, rest := segs[0], segs[1:]
	switch n := node.(type) {
	case map[string]any:
		if len(rest) == 0 {
			n[seg] = v
			return ""
		}
		child, ok := n[seg]
		if !ok || child == nil {
			child = map[string]any{}
			n[seg] = child
		}
		return setPath(child, rest, v)
	case []any:
		i, reason := listIndex(seg, len(n))
		if reason != "" {
			return reason
		}
		if len(rest) == 0 {
			n[i] = v
			return ""
		}
		return setPath(n[i], rest, v)
	}
	return fmt.Sprintf("cannot descend into %q: not a map or list", seg)
}

func unsetPath(node any, segs []string) string {
	seg, rest := segs[0], segs[1:]
	switch n := node.(type) {
	case map[string]any:
		child, ok := n[seg]
		if !ok {
			return fmt.Sprintf("path element %q not found", seg)
		}
		if len(rest) == 0 {
			delete(n, seg)
			return ""
		}
		return unsetPath(child, rest)
	case []any:
		i, reason := listIndex(seg, len(n))
		if reason != "" {
			return reason
		}
		if len(rest) == 0 {
			return "cannot unset a list element: set the whole list"
		}
		return unsetPath(n[i], rest)
	}
	return fmt.Sprintf("cannot descend into %q: not a map or list", seg)
}

func listIndex(seg string, n int) (int, string) {
	i, err := strconv.Atoi(seg)
	if err != nil || i < 0 {
		return 0, fmt.Sprintf("%q is not a list index", seg)
	}
	if i >= n {
		return 0, fmt.Sprintf("index %d out of range (list has %d elements)", i, n)
	}
	return i, ""
}

func providerOf(typ string) string {
	prefix, _, _ := strings.Cut(typ, "_")
	return "hashicorp/" + prefix
}

func stringList(v any) ([]string, bool) {
	switch t := v.(type) {
	case []string:
		return slices.Clone(t), true
	case []any:
		out := make([]string, len(t))
		for i, e := range t {
			s, ok := e.(string)
			if !ok {
				return nil, false
			}
			out[i] = s
		}
		return out, true
	}
	return nil, false
}

// checksFromValue reads the list of checks of a "checks" op, numbers kept as
// json.Number like those of the planner.
func checksFromValue(v any) ([]Check, string) {
	if v == nil {
		return []Check{}, ""
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, "checks: " + err.Error()
	}
	var out []Check
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		return nil, "checks: " + err.Error()
	}
	return out, ""
}

func deepCopy(v any) any {
	switch t := v.(type) {
	case map[string]any:
		if t == nil {
			return t
		}
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[k] = deepCopy(e)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = deepCopy(e)
		}
		return out
	}
	return v
}
