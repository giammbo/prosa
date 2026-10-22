package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/giammbo/prosa/internal/engine"
	"github.com/giammbo/prosa/internal/events"
	"github.com/giammbo/prosa/internal/planner"
)

// eventRecorder wires the event log into a script the way the CLI does: the
// engine's Observer, and the planner's results through a wrapper of the
// planner (cmd/prosa's eventPlanner, which lives in package main). Each
// begin opens the log at path again, so every step is a run of its own.
type eventRecorder struct {
	t     *testing.T
	path  string
	inner engine.Planner
	s     *script
	log   *events.Log
	runs  map[string]string // step name → run id
}

func newEventRecorder(t *testing.T, path string, inner engine.Planner) *eventRecorder {
	return &eventRecorder{t: t, path: path, inner: inner, runs: map[string]string{}}
}

// Plan implements engine.Planner.
func (r *eventRecorder) Plan(ctx context.Context, in engine.PlanInput) (engine.Desired, error) {
	d, err := r.inner.Plan(ctx, in)
	if rr, ok := r.inner.(planner.ResultReporter); ok && r.log != nil {
		if res := rr.LastResult(); res.Intent != "" {
			r.log.PlannerResult(res)
		}
	}
	return d, err
}

// reviewer wraps inner so that each review is recorded as rules.review in
// the log of the current run, as cmd/prosa's eventReviewer does: the IDs of
// the rules given, the violations found as "<ID> <key>".
func (r *eventRecorder) reviewer(inner engine.Reviewer) engine.Reviewer {
	return reviewRecorder{r, inner}
}

type reviewRecorder struct {
	r     *eventRecorder
	inner engine.Reviewer
}

func (rr reviewRecorder) Review(ctx context.Context, in engine.ReviewInput) ([]engine.Violation, error) {
	vs, err := rr.inner.Review(ctx, in)
	if rep, ok := rr.inner.(planner.ResultReporter); ok && rr.r.log != nil {
		if res := rep.LastResult(); res.Intent != "" {
			ids := make([]string, len(in.Rules))
			for i, rule := range in.Rules {
				ids[i] = rule.ID
			}
			found := make([]string, len(vs))
			for i, v := range vs {
				found[i] = v.Rule + " " + v.Key
			}
			rr.r.log.RulesReview(res, ids, found)
		}
	}
	return vs, err
}

// begin starts a run named command and returns the function that ends it
// with the result of err (as cmd/prosa maps it).
func (r *eventRecorder) begin(command string) (end func(error)) {
	r.t.Helper()
	log, err := events.Open(r.path)
	if err != nil {
		r.t.Fatal(err)
	}
	log.ApprovalMode = "flag"
	log.Start(command, "e2e", r.s.dir, r.s.aws)
	r.log, r.s.engine.Observer, r.runs[command] = log, log, log.RunID
	return func(err error) {
		result := "ok"
		switch {
		case errors.Is(err, engine.ErrNotApproved):
			result = "not_approved"
		case err != nil:
			result = "error"
		}
		log.End(result, engine.Classify(err))
		r.log, r.s.engine.Observer = nil, nil
		if werr := log.Err(); werr != nil {
			r.t.Errorf("event log: %v", werr)
		}
		if cerr := log.Close(); cerr != nil {
			r.t.Errorf("event log: %v", cerr)
		}
	}
}

// event is one line of the log.
type event struct {
	RunID  string         `json:"run_id"`
	Type   string         `json:"type"`
	Intent string         `json:"intent"`
	Key    string         `json:"key"`
	Data   map[string]any `json:"data"`
}

// read returns the events of the file, and its bytes.
func (r *eventRecorder) read() ([]event, []byte) {
	r.t.Helper()
	data, err := os.ReadFile(r.path)
	if err != nil {
		r.t.Fatal(err)
	}
	var out []event
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
		var e event
		if err := json.Unmarshal(line, &e); err != nil {
			r.t.Fatalf("event line %q: %v", line, err)
		}
		out = append(out, e)
	}
	return out, data
}

// ofRun returns the events of a step's run.
func (r *eventRecorder) ofRun(all []event, command string) []event {
	r.t.Helper()
	id := r.runs[command]
	if id == "" {
		r.t.Fatalf("no run %q", command)
	}
	var out []event
	for _, e := range all {
		if e.RunID == id {
			out = append(out, e)
		}
	}
	return out
}

// ofType returns the events of that type.
func ofType(evs []event, typ string) []event {
	return slices.DeleteFunc(slices.Clone(evs), func(e event) bool { return e.Type != typ })
}

// str returns a string field of the event's data.
func (e event) str(field string) string {
	s, _ := e.Data[field].(string)
	return s
}

// awsOut runs the aws CLI against Moto and returns its combined output.
func awsOut(ctx context.Context, t *testing.T, args ...string) (string, error) {
	t.Helper()
	return awsOutIn(ctx, t, motoRegion, args...)
}

// awsOutIn is awsOut in another region of Moto.
func awsOutIn(ctx context.Context, t *testing.T, region string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.CommandContext(ctx, awsCLI(t), append([]string{"--endpoint-url", MotoURL(), "--region", region}, args...)...)
	cmd.Env = awsCLIEnv()
	b, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(b)), err
}

// awsCLI returns the path of the aws CLI. The e2e checks Moto with it: a
// missing one fails the test instead of silently checking less.
func awsCLI(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("aws")
	if err != nil {
		t.Fatalf("aws CLI not found (%v): the e2e checks Moto with it; install it (brew install awscli) or unset PROSA_E2E", err)
	}
	return path
}
