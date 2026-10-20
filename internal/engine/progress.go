package engine

import (
	"github.com/zclconf/go-cty/cty"

	"github.com/giammbo/prosa/internal/provider"
)

// OpPhase is the phase of an Apply operation reported to Engine.Progress.
type OpPhase string

const (
	OpStart  OpPhase = "start"  // the provider is about to apply it
	OpDone   OpPhase = "done"   // applied and saved
	OpFailed OpPhase = "failed" // it failed; Err says why
)

// OpProgress is one phase of an operation of Apply, for the terminal: unlike
// the Observer's events it carries the object's id. Action is Create, Update
// or Delete (a replace is a Delete, then a Create). ID is the provider id,
// when the object has one: of the prior object at OpStart (update, delete),
// of the new one at OpDone (create, update); "(sensitive value)" when the
// schema marks the id sensitive.
type OpProgress struct {
	Intent, Key string
	Action      Action
	Phase       OpPhase
	ID          string
	Err         error
}

// progress reports a phase of o to Engine.Progress, when set. Operations
// skipped by their re-plan are never reported.
func (a *applier) progress(o op, phase OpPhase, obj cty.Value, err error) {
	if a.e.Progress == nil {
		return
	}
	c := o.c
	action := Create
	switch {
	case o.kind == 'D':
		action = Delete
	case c.Action == Update:
		action = Update
	}
	if phase == OpDone && action == Delete {
		obj = cty.NilVal
	}
	id := stateID(obj)
	if id != "" && idSensitive(a.p.schema, c.Type) {
		id = "(sensitive value)"
	}
	a.e.Progress(OpProgress{Intent: c.Intent, Key: c.Key, Action: action, Phase: phase, ID: id, Err: err})
}

// idSensitive reports whether the id of a resource of type typ must be
// masked: the schema marks it sensitive or write-only, or the schema of the
// type is unknown (fail closed).
func idSensitive(s *provider.Schema, typ string) bool {
	if s == nil || s.Resources[typ] == nil || s.Resources[typ].Block == nil {
		return true
	}
	a := s.Resources[typ].Block.Attributes["id"]
	return a != nil && isSensitive(a)
}

// stateID returns the "id" attribute of a state, "" when it has none.
func stateID(v cty.Value) string {
	if !isObject(v) || !v.Type().HasAttribute("id") {
		return ""
	}
	id := v.GetAttr("id")
	if !id.IsKnown() || id.IsNull() || id.Type() != cty.String {
		return ""
	}
	return id.AsString()
}
