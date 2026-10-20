package main

import (
	"github.com/giammbo/prosa/internal/engine"
	"github.com/giammbo/prosa/internal/events"
)

// fixturesPlanner is the planner of --fixtures. It is replaced, in a build
// with the tag fixtureevents (see fixtures_events.go), by one that records a
// planner.result event for each answer, so that the end-to-end tests can count
// the calls of the planner by mode.
var fixturesPlanner = func(sp engine.StaticPlanner, _ *events.Log) engine.Planner { return sp }
