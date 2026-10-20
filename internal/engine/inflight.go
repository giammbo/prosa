package engine

import "sync/atomic"

// InFlight tells which provider apply is in progress, so that the CLI can
// name it at the first Ctrl-C (ADR-024). The zero value is ready; a nil
// *InFlight records nothing. It is safe for concurrent use.
type InFlight struct {
	cur atomic.Pointer[string]
}

// Current returns "<intent>/<key> (<action>)" of the operation in
// progress, from the start of its provider apply until its state is saved;
// "" when there is none.
func (f *InFlight) Current() string {
	if f == nil {
		return ""
	}
	if s := f.cur.Load(); s != nil {
		return *s
	}
	return ""
}

// start records c as in progress; the returned function clears it.
func (f *InFlight) start(c *Change) func() {
	if f == nil {
		return func() {}
	}
	s := c.Intent + "/" + c.Key + " (" + string(c.Action) + ")"
	f.cur.Store(&s)
	return func() { f.cur.Store(nil) }
}
