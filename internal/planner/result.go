package planner

import "sync"

// Usage is the token usage of a plan, summed over its turns.
type Usage struct {
	Input, Output, CacheRead, CacheCreation int64
}

// Result describes how the planner answered for one intent, for the event
// log (ADR-023). It holds counts, model names and token usage: never the
// conversation, the thinking or any value.
//
// Job is JobPlan or JobPatch (ADR-030); Backend the backend's name (empty
// for an answer from the cache); Model the model requested for the job;
// ServedBy the one that answered the last turn, as the backend reports it
// (Fallback is true when a server-side fallback served any turn). Cached
// marks an answer read from the cache: no model was called.
type Result struct {
	Intent   string
	Job      string
	Backend  string
	Turns    int
	Problems int
	Model    string
	ServedBy string
	Fallback bool
	Cached   bool
	Usage    Usage
}

// ResultReporter is implemented by the planners that know how their last
// Plan went (LLM, Cached, and wrappers of them). Plans are sequential.
type ResultReporter interface {
	LastResult() Result
}

// lastResult is the storage behind LastResult.
type lastResult struct {
	mu sync.Mutex
	r  Result
}

func (l *lastResult) set(r Result) {
	l.mu.Lock()
	l.r = r
	l.mu.Unlock()
}

func (l *lastResult) get() Result {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.r
}

// add accounts for one turn: its usage, and which model served it.
func (r *Result) add(t Turn) {
	r.Usage.Input += t.Usage.Input
	r.Usage.Output += t.Usage.Output
	r.Usage.CacheRead += t.Usage.CacheRead
	r.Usage.CacheCreation += t.Usage.CacheCreation
	r.ServedBy = t.ServedBy
	r.Fallback = r.Fallback || t.Fallback
}
