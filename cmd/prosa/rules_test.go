package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/giammbo/prosa/internal/engine"
	"github.com/giammbo/prosa/internal/events"
	"github.com/giammbo/prosa/internal/planner"
	"github.com/giammbo/prosa/rules"
)

// engineFor parses the arguments of cmd on a fresh project and builds its
// engine the way runReconcile does, with fake planner and reviewer sources.
func engineFor(t *testing.T, cmd string, args []string, setup func(dir string)) (*engine.Engine, string, error) {
	t.Helper()
	dir := projectAt(t, t.TempDir())
	writeFile(t, filepath.Join(dir, "app.md"), "one thing")
	if setup != nil {
		setup(dir)
	}
	for i := range args {
		args[i] = strings.ReplaceAll(args[i], "{dir}", dir)
	}
	var stderr bytes.Buffer
	f, err := parseReconcileFlags(cmd, append(args, dir), &stderr)
	if err != nil {
		t.Fatalf("parse: %v (stderr %q)", err, stderr.String())
	}
	fp := &fakePlanner{}
	e, err := f.buildEngine(nil, fp.source, fakeReviewerSource, nil, &stderr)
	return e, stderr.String(), err
}

type nopReviewer struct{}

func (nopReviewer) Review(context.Context, engine.ReviewInput) ([]engine.Violation, error) {
	return nil, nil
}

func fakeReviewerSource(plannerSettings) engine.Reviewer { return nopReviewer{} }

func TestDisableRuleFlag(t *testing.T) {
	e, _, err := engineFor(t, "plan", []string{"--disable-rule", "AWS-NET-001", "--disable-rule", "aws-net-003"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []rules.DisabledRule{{ID: "AWS-NET-001", By: "--disable-rule"}, {ID: "AWS-NET-003", By: "--disable-rule"}}
	if len(e.Rules.Disabled) != 2 || e.Rules.Disabled[0] != want[0] || e.Rules.Disabled[1] != want[1] {
		t.Errorf("Disabled = %+v, want %+v", e.Rules.Disabled, want)
	}
	for _, r := range e.Rules.Active {
		if r.ID == "AWS-NET-001" || r.ID == "AWS-NET-003" {
			t.Errorf("%s is still active", r.ID)
		}
	}
}

func TestReplanFlag(t *testing.T) {
	e, _, err := engineFor(t, "apply", []string{"--replan", "database", "--replan", "app"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(e.Replan, ","); got != "database,app" {
		t.Errorf("Replan = %q, want database,app", got)
	}
}

func TestRulesWarningsOnStderr(t *testing.T) {
	_, stderr, err := engineFor(t, "plan", nil, func(dir string) {
		if err := mkdirAll(filepath.Join(dir, "rules")); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(dir, "rules", "disabled.txt"), "AWS-XXX-001\n")
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := "prosa: warning: unknown rule AWS-XXX-001 in rules/disabled.txt\n"; !strings.Contains(stderr, want) {
		t.Errorf("stderr = %q, want it to contain %q", stderr, want)
	}
}

func TestBadProjectRuleFailsEarly(t *testing.T) {
	dir := projectAt(t, t.TempDir())
	writeFile(t, filepath.Join(dir, "app.md"), "one thing")
	if err := mkdirAll(filepath.Join(dir, "rules")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "rules", "x.md"), "not a rule\n")
	t.Setenv("PROSA_STATE_PASSPHRASE", "passphrase")
	fp := &fakePlanner{}
	var stdout, stderr bytes.Buffer
	code := reconcile(context.Background(), "plan", []string{dir}, strings.NewReader(""), &stdout, &stderr, (&fakeProvider{}).source, fp.source)
	if code != 1 || !strings.Contains(stderr.String(), "rules/x.md") {
		t.Errorf("exit %d, stderr %q; want 1 naming rules/x.md", code, stderr.String())
	}
	if fp.calls != 0 || fp.model != "" {
		t.Errorf("the planner was created or called (calls %d, model %q)", fp.calls, fp.model)
	}
}

func TestFixturesHaveNoReviewer(t *testing.T) {
	e, _, err := engineFor(t, "plan", []string{"--fixtures", "{dir}/fx.json"}, func(dir string) { writeFile(t, filepath.Join(dir, "fx.json"), "[]") })
	if err != nil {
		t.Fatal(err)
	}
	if e.Reviewer != nil {
		t.Errorf("Reviewer = %T with --fixtures, want nil", e.Reviewer)
	}
	e, _, err = engineFor(t, "plan", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if e.Reviewer == nil {
		t.Error("no reviewer with the LLM planner")
	}
}

// --replan does nothing without a reviewer (--fixtures): the run says so.
func TestReplanWithoutModelWarns(t *testing.T) {
	const warning = "prosa: warning: --replan has no effect without a model (rules are not checked)\n"
	fixtures := func(dir string) { writeFile(t, filepath.Join(dir, "fx.json"), "[]") }
	_, stderr, err := engineFor(t, "plan", []string{"--fixtures", "{dir}/fx.json", "--replan", "app"}, fixtures)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, warning) {
		t.Errorf("stderr = %q, want %q", stderr, warning)
	}
	for _, args := range [][]string{{"--fixtures", "{dir}/fx.json"}, {"--replan", "app"}} {
		_, stderr, err := engineFor(t, "plan", args, fixtures)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(stderr, "--replan has no effect") {
			t.Errorf("%v: stderr = %q, want no warning", args, stderr)
		}
	}
}

func TestCheckIgnoresRules(t *testing.T) {
	dir := projectAt(t, t.TempDir())
	for _, flag := range []string{"--disable-rule", "--replan"} {
		var stderr bytes.Buffer
		if _, err := parseReconcileFlags("check", []string{flag, "x", dir}, &stderr); err == nil || !strings.Contains(stderr.String(), "flag provided but not defined") {
			t.Errorf("check %s: err %v, stderr %q; want an unknown flag", flag, err, stderr.String())
		}
	}
	var out bytes.Buffer
	if code := run(context.Background(), []string{"check", "--disable-rule", "X", dir}, strings.NewReader(""), io.Discard, &out); code != 2 {
		t.Errorf("check --disable-rule: exit %d, want 2", code)
	}
}

func mkdirAll(p string) error { return os.MkdirAll(p, 0o755) }

type reportingReviewer struct{}

func (reportingReviewer) Review(_ context.Context, in engine.ReviewInput) ([]engine.Violation, error) {
	return []engine.Violation{{Rule: "AWS-SQS-001", Key: "queue", Reason: "secret reason"}}, nil
}
func (reportingReviewer) LastResult() planner.Result {
	return planner.Result{Intent: "app", Job: planner.JobReview, Turns: 1}
}

func TestReviewerEventsCarryRuleIDsAndViolationsNoReasons(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ev.jsonl")
	log, err := events.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	rv := withReviewerEvents(reportingReviewer{}, log)
	_, err = rv.Review(context.Background(), engine.ReviewInput{Intent: "app", Rules: []rules.Rule{{ID: "AWS-SQS-001"}, {ID: "AWS-SQS-002"}}})
	log.Close()
	if err != nil {
		t.Fatal(err)
	}
	evs := readEvents(t, path)
	if len(evs) != 1 || evs[0]["type"] != "rules.review" || evs[0]["intent"] != "app" {
		t.Fatalf("events = %v", evs)
	}
	d := eventData(evs[0])
	if r := d["rules"].([]any); len(r) != 2 || r[0] != "AWS-SQS-001" {
		t.Errorf("rules = %v", r)
	}
	if v := d["violations"].([]any); len(v) != 1 || v[0] != "AWS-SQS-001 queue" {
		t.Errorf("violations = %v", v)
	}
	if data, _ := os.ReadFile(path); strings.Contains(string(data), "secret reason") {
		t.Error("the reason reached the log")
	}
}
