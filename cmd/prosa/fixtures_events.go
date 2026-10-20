//go:build fixtureevents

package main

import (
	"context"

	"github.com/giammbo/prosa/internal/engine"
	"github.com/giammbo/prosa/internal/events"
	"github.com/giammbo/prosa/internal/planner"
)

// With the tag fixtureevents the planner of --fixtures reports each answer as
// a model would (the job, backend "fixtures", no turns or usage). In place of
// the model it names what the answer is for: "base" (the intent planned
// without an environment) or "env" (planned for the environment). Only the
// end-to-end tests build with it: the shipped binary does not report.
func init() {
	fixturesPlanner = func(sp engine.StaticPlanner, log *events.Log) engine.Planner {
		return withPlannerEvents(&reportingFixtures{sp: sp}, log)
	}
}

type reportingFixtures struct {
	sp   engine.StaticPlanner
	last planner.Result
}

func (f *reportingFixtures) Plan(ctx context.Context, in engine.PlanInput) (engine.Desired, error) {
	job := planner.JobPlan
	if in.Mode == planner.JobPatch {
		job = planner.JobPatch
	}
	f.last = planner.Result{Intent: in.Intent.Name, Job: job, Backend: "fixtures", Model: "base"}
	if in.Env != nil {
		f.last.Model = "env"
	}
	return f.sp.Plan(ctx, in)
}

// LastResult implements planner.ResultReporter.
func (f *reportingFixtures) LastResult() planner.Result { return f.last }
