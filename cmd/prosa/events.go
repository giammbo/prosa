package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/giammbo/prosa/internal/engine"
	"github.com/giammbo/prosa/internal/events"
	"github.com/giammbo/prosa/internal/planner"
)

// version is the version recorded by run.start and printed by `prosa version`;
// goreleaser sets it with -ldflags "-X main.version=...".
var version = "dev"

// withEvents opens the event log of f (unless --no-events), records the
// start of command, runs fn with it and records the end with the result of
// fn's error. fn gets a nil log when events are off.
func withEvents(f reconcileFlags, command string, stderr io.Writer, fn func(*events.Log) error) error {
	if f.noEvents {
		return fn(nil)
	}
	log, err := openLog(f.events)
	if err != nil {
		return fmt.Errorf("event log: %w (--no-events disables it)", err)
	}
	log.ApprovalMode = "prompt"
	if f.yes {
		log.ApprovalMode = "flag"
	}
	log.Start(command, version, f.dir, f.aws)
	err = fn(log)
	log.End(endResult(err), engine.Classify(err))
	// A log that could not be written never fails the command, but it is said.
	werr := log.Err()
	if cerr := log.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		fmt.Fprintf(stderr, "prosa: warning: event log: %v; some events were not recorded\n", werr)
	}
	return err
}

// openLog opens the event log; a variable so tests can substitute it.
var openLog = events.Open

// endResult maps the error of a command to the result of run.end.
func endResult(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, engine.ErrNotApproved):
		return "not_approved"
	case errors.Is(err, engine.ErrInterrupted), errors.Is(err, context.Canceled):
		return "interrupted"
	}
	return "error"
}

// observer returns log as an engine.Observer, nil (not a typed nil) when
// events are off.
func observer(log *events.Log) engine.Observer {
	if log == nil {
		return nil
	}
	return log
}

// eventPlanner records the planner.result of every Plan of inner, when inner
// reports one.
type eventPlanner struct {
	inner engine.Planner
	log   *events.Log
}

func (p eventPlanner) Plan(ctx context.Context, in engine.PlanInput) (engine.Desired, error) {
	d, err := p.inner.Plan(ctx, in)
	if rr, ok := p.inner.(planner.ResultReporter); ok {
		if r := rr.LastResult(); r.Intent != "" {
			p.log.PlannerResult(r)
		}
	}
	return d, err
}

// Forget forwards to inner, when it keeps its answers (the planner cache).
func (p eventPlanner) Forget(intentName string) {
	if f, ok := p.inner.(engine.Forgetter); ok {
		f.Forget(intentName)
	}
}

// withPlannerEvents wraps pl so that it records its results in log (nil: pl).
func withPlannerEvents(pl engine.Planner, log *events.Log) engine.Planner {
	if log == nil {
		return pl
	}
	return eventPlanner{pl, log}
}

// eventReviewer records the rules.review of every Review of inner, when
// inner reports a result: the rule IDs it was given and the violations it
// found (rule and key), taken from the input and the answer.
type eventReviewer struct {
	inner engine.Reviewer
	log   *events.Log
}

func (r eventReviewer) Review(ctx context.Context, in engine.ReviewInput) ([]engine.Violation, error) {
	vs, err := r.inner.Review(ctx, in)
	if rr, ok := r.inner.(planner.ResultReporter); ok {
		if res := rr.LastResult(); res.Intent != "" {
			ids := make([]string, len(in.Rules))
			for i, rule := range in.Rules {
				ids[i] = rule.ID
			}
			found := make([]string, len(vs))
			for i, v := range vs {
				found[i] = v.Rule + " " + v.Key
			}
			r.log.RulesReview(res, ids, found)
		}
	}
	return vs, err
}

// withReviewerEvents wraps rv so that it records its results in log (nil: rv).
func withReviewerEvents(rv engine.Reviewer, log *events.Log) engine.Reviewer {
	if log == nil {
		return rv
	}
	return eventReviewer{rv, log}
}
