// Package events writes the event log (ADR-023): a JSON Lines file with one
// event per line, appended by every command. Events hold names, counts,
// actions, outcomes and error summaries; never attribute values, secrets or
// the model's thinking.
package events

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/giammbo/prosa/internal/engine"
	"github.com/giammbo/prosa/internal/planner"
)

// Event types.
const (
	RunStart      = "run.start"
	PlannerResult = "planner.result"
	RulesReview   = "rules.review"
	PlanEvent     = "plan"
	Approval      = "approval"
	ApplyOp       = "apply.op"
	Check         = "check"
	RunEnd        = "run.end"
)

// Event is one line of the log.
type Event struct {
	Time        time.Time      `json:"time"`
	RunID       string         `json:"run_id"`
	Type        string         `json:"type"`
	Intent, Key string         `json:"-"`
	Data        map[string]any `json:"data,omitempty"`
}

// MarshalJSON puts intent and key next to the other top-level fields.
func (e Event) MarshalJSON() ([]byte, error) {
	type line struct {
		Time   time.Time      `json:"time"`
		RunID  string         `json:"run_id"`
		Type   string         `json:"type"`
		Intent string         `json:"intent,omitempty"`
		Key    string         `json:"key,omitempty"`
		Data   map[string]any `json:"data,omitempty"`
	}
	return json.Marshal(line{e.Time, e.RunID, e.Type, e.Intent, e.Key, e.Data})
}

// Log appends events to a file. It implements engine.Observer. A failed
// write never fails a command: the first error is kept in Err.
type Log struct {
	RunID string // random, one per command invocation
	User  string // $USER, else the OS user
	// ApprovalMode is recorded by approval events: "flag" (--yes) or "prompt".
	ApprovalMode string

	mu  sync.Mutex
	w   io.WriteCloser
	err error
	now func() time.Time
}

var _ engine.Observer = (*Log)(nil)

// Open opens (creating it, and its directory) the event file at path for
// appending, with permissions 0600 (directory 0700), and gives the log a new
// run id.
func Open(path string) (*Log, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return NewWriter(f), nil
}

// NewWriter returns a log that writes to w, with a new run id.
func NewWriter(w io.WriteCloser) *Log {
	return &Log{RunID: newRunID(), User: currentUser(), w: w, now: time.Now}
}

func newRunID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "0000000000000000"
	}
	return hex.EncodeToString(b)
}

func currentUser() string {
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return "unknown"
}

// Emit appends e, with the log's time and run id when e has none.
func (l *Log) Emit(e Event) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e.Time.IsZero() {
		e.Time = l.now().UTC()
	}
	if e.RunID == "" {
		e.RunID = l.RunID
	}
	data, err := json.Marshal(e)
	if err == nil {
		_, err = l.w.Write(append(data, '\n'))
	}
	if err != nil && l.err == nil {
		l.err = err
	}
}

// Err returns the first error of Emit.
func (l *Log) Err() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.err
}

// Close closes the file.
func (l *Log) Close() error { return l.w.Close() }

// Start records the start of a command and where it points (aws): an
// emulated endpoint (target "emulated") or AWS (target "aws"), and the
// region. The account is not recorded: Prosa does not know it (ADR-029).
func (l *Log) Start(command, version, dir string, aws engine.AWSSettings) {
	d := map[string]any{"command": command, "version": version, "dir": dir, "user": l.User,
		"target": "aws", "region": aws.Region}
	if aws.Endpoint != "" {
		d["target"], d["endpoint"] = "emulated", RedactEndpoint(aws.Endpoint)
	}
	l.Emit(Event{Type: RunStart, Data: d})
}

// RedactEndpoint is endpoint without userinfo (user:password@), query string
// and fragment: neither the log nor the target line carry credentials. An endpoint that does not parse as a URL is not
// logged.
func RedactEndpoint(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "(unparseable endpoint)"
	}
	u.User, u.RawQuery, u.Fragment = nil, "", ""
	return u.String()
}

// End records the end of a command: result is "ok", "error", "not_approved"
// or "interrupted"; failure describes the error, if any.
func (l *Log) End(result string, failure *engine.ErrorInfo) {
	d := map[string]any{"result": result}
	addFailure(d, failure)
	l.Emit(Event{Type: RunEnd, Data: d})
}

// PlannerResult records how the planner answered for an intent: the job
// (plan or patch), the backend and the model among the rest.
func (l *Log) PlannerResult(r planner.Result) {
	l.Emit(Event{Type: PlannerResult, Intent: r.Intent, Data: map[string]any{
		"job": r.Job, "backend": r.Backend,
		"turns": r.Turns, "problems": r.Problems,
		"model": r.Model, "served_by": r.ServedBy, "fallback": r.Fallback, "cached": r.Cached,
		"usage": map[string]any{"input": r.Usage.Input, "output": r.Usage.Output, "cache_read": r.Usage.CacheRead, "cache_creation": r.Usage.CacheCreation},
	}})
}

// RulesReview records a review of an intent against the rules (ADR-034): the
// IDs of the rules it was given, the violations as "<ID> <key>", and how the
// reviewer answered. Never the reasons, which are the model's free text.
func (l *Log) RulesReview(r planner.Result, ruleIDs, violations []string) {
	if ruleIDs == nil {
		ruleIDs = []string{}
	}
	if violations == nil {
		violations = []string{}
	}
	l.Emit(Event{Type: RulesReview, Intent: r.Intent, Data: map[string]any{
		"rules": ruleIDs, "violations": violations, "dropped": r.Dropped,
		"turns": r.Turns, "model": r.Model, "served_by": r.ServedBy, "cached": r.Cached,
		"usage": map[string]any{"input": r.Usage.Input, "output": r.Usage.Output, "cache_read": r.Usage.CacheRead, "cache_creation": r.Usage.CacheCreation},
	}})
}

// Planned implements engine.Observer: the changes with their action and
// gate, the drifted attributes by name, the rules a fix answers, the
// blockers; never a value.
func (l *Log) Planned(p *engine.Plan) {
	changes := []map[string]any{}
	unchanged := 0
	for _, c := range p.Changes {
		if c.Action == engine.NoOp {
			unchanged++
			continue
		}
		ch := map[string]any{
			"intent": c.Intent, "key": c.Key, "type": c.Type, "action": string(c.Action),
			"gate": string(c.Gate), "gate_reason": c.GateReason, "drifted": c.Drifted,
		}
		if len(c.DriftAttrs) > 0 {
			ch["drift_attrs"] = c.DriftAttrs
		}
		if c.Rule != "" {
			ch["rule"] = c.Rule
		}
		changes = append(changes, ch)
	}
	// Blockers are the model's free text, which may quote values: only the
	// count per intent is kept.
	blockers := []map[string]any{}
	counts := map[string]int{}
	var order []string
	for _, b := range p.Blockers {
		name, _, _ := strings.Cut(b, ": ")
		if counts[name] == 0 {
			order = append(order, name)
		}
		counts[name]++
	}
	for _, name := range order {
		blockers = append(blockers, map[string]any{"intent": name, "count": counts[name]})
	}
	l.Emit(Event{Type: PlanEvent, Data: map[string]any{
		"changes": changes, "unchanged": unchanged, "blockers": blockers, "blocked": p.Blocked,
	}})
}

// Approval implements engine.Observer.
func (l *Log) Approval(granted bool, changes []string) {
	l.Emit(Event{Type: Approval, Data: map[string]any{"granted": granted, "mode": l.ApprovalMode, "by": l.User, "changes": changes}})
}

// ApplyOp implements engine.Observer.
func (l *Log) ApplyOp(intent, key string, action engine.Action, phase string, failure *engine.ErrorInfo) {
	d := map[string]any{"node": intent + "/" + key, "action": string(action), "result": "ok"}
	if phase != "" {
		d["phase"] = phase
	}
	if failure != nil {
		d["result"] = "error"
		addFailure(d, failure)
	}
	l.Emit(Event{Type: ApplyOp, Intent: intent, Key: key, Data: d})
}

// Check implements engine.Observer.
func (l *Log) Check(r engine.CheckResult, typ string) {
	d := map[string]any{"name": r.Name, "type": typ, "status": r.Status}
	if r.Detail != "" {
		d["detail"] = r.Detail
	}
	l.Emit(Event{Type: Check, Intent: r.Intent, Data: d})
}

// addFailure puts the value-free description of an error in d.
func addFailure(d map[string]any, f *engine.ErrorInfo) {
	if f == nil {
		return
	}
	d["error_kind"], d["error"] = f.Kind, f.Message
	if len(f.Attributes) > 0 {
		d["attributes"] = f.Attributes
	}
	if f.Count > 0 {
		d["count"] = f.Count
	}
}
