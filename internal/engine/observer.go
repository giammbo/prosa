package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/giammbo/prosa/internal/provider"
)

// Observer receives what the engine does, for the event log (ADR-023). A nil
// Engine.Observer means no events. Observers get names, actions, gates and
// value-free error descriptions: never configuration, state or planned
// values.
type Observer interface {
	// Planned is called when Plan returns a plan (blocked ones included).
	Planned(p *Plan)
	// Approval is called when Apply asked for an approval: changes lists the
	// "<intent>/<key>" it covered.
	Approval(granted bool, changes []string)
	// ApplyOp is called after each operation Apply executed (not for the
	// ones skipped). phase is "destroy" or "create" for a replace, "" else;
	// failure is nil on success.
	ApplyOp(intent, key string, action Action, phase string, failure *ErrorInfo)
	// Check is called with each check result, of Apply or of Check; typ is
	// the type of the check (resource_attr, http_get, ...).
	Check(r CheckResult, typ string)
}

// Error kinds of ErrorInfo: a closed set.
const (
	KindProvider    = "provider"
	KindValidation  = "validation"
	KindPlanChanged = "plan_changed"
	KindInterrupted = "interrupted"
	KindWriteOnly   = "write_only"
	KindUnknown     = "unknown_state"
	KindStateStore  = "state_store"
	KindLock        = "lock"
	KindPlanner     = "planner"
	KindApproval    = "approval"
	KindInternal    = "internal"
)

// ErrorInfo describes an error for the event log without any value: a kind
// from the closed set above, a fixed-shape message, and for provider errors
// the diagnostic attribute paths, cut at their first map key (see attrPath),
// and their number. The full error text goes to the terminal only.
type ErrorInfo struct {
	Kind, Message string
	Attributes    []string
	Count         int
}

// KindError gives an error a kind and a value-free message for the event
// log; its text is Err's.
type KindError struct {
	Kind, Msg string
	Err       error
}

func (e *KindError) Error() string { return e.Err.Error() }
func (e *KindError) Unwrap() error { return e.Err }

// Classify describes err for the event log; nil for a nil error.
func Classify(err error) *ErrorInfo {
	if err == nil {
		return nil
	}
	var ve *ValidationError
	var ke *KindError
	switch {
	case errors.Is(err, context.Canceled):
		return &ErrorInfo{Kind: KindInterrupted, Message: "interrupted"}
	case errors.Is(err, ErrNotApproved):
		return &ErrorInfo{Kind: KindApproval, Message: "plan not approved"}
	case errors.As(err, &ve):
		return &ErrorInfo{Kind: KindValidation, Message: fmt.Sprintf("%d problem(s)", len(ve.Problems)), Count: len(ve.Problems)}
	}
	if diags := collectDiags(err); len(diags) > 0 {
		info := &ErrorInfo{Kind: KindProvider, Message: diags[0].Op + ": provider error", Count: len(diags)}
		for _, d := range diags {
			if attr := attrPath(d.Attr); attr != "" && !slices.Contains(info.Attributes, attr) {
				info.Attributes = append(info.Attributes, attr)
			}
		}
		return info
	}
	if errors.As(err, &ke) {
		return &ErrorInfo{Kind: ke.Kind, Message: ke.Msg}
	}
	return &ErrorInfo{Kind: KindInternal, Message: "error (see command output)"}
}

// attrPath is a diagnostic attribute path cut at its first map key, which
// can be a value: tags["team"] is tags[*], a.b["k"].c is a.b[*].
func attrPath(attr string) string {
	if i := strings.Index(attr, `["`); i >= 0 {
		return attr[:i] + "[*]"
	}
	return attr
}

// collectDiags returns the provider diagnostics in err.
func collectDiags(err error) []*provider.DiagError {
	switch u := err.(type) {
	case *provider.DiagError:
		return []*provider.DiagError{u}
	case interface{ Unwrap() []error }:
		var out []*provider.DiagError
		for _, e := range u.Unwrap() {
			out = append(out, collectDiags(e)...)
		}
		return out
	case interface{ Unwrap() error }:
		return collectDiags(u.Unwrap())
	}
	return nil
}

func kindErr(kind, msg string, err error) error {
	if err == nil {
		return nil
	}
	return &KindError{Kind: kind, Msg: msg, Err: err}
}

// refErr marks err, from resolving the references of a resource, as such.
func refErr(node string, err error) error {
	return kindErr(KindInternal, node+": reference error", err)
}
