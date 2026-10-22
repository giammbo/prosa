package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/giammbo/prosa/internal/engine"
	"github.com/giammbo/prosa/internal/planner"
	"github.com/giammbo/prosa/rules"
)

// stringList is a repeatable string flag (--disable-rule, --replan).
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }

func (l *stringList) Set(v string) error {
	*l = append(*l, v)
	return nil
}

// loadRules returns the rules in force: the built-in ones and those of the
// project's rules/ folder, without the IDs of rules/disabled.txt and of
// disabled (--disable-rule). Each warning of the set goes to stderr. A
// malformed rule or a duplicate ID is an error.
func loadRules(root string, disabled []string, stderr io.Writer) (rules.Set, error) {
	builtin, err := rules.Builtin()
	if err != nil {
		return rules.Set{}, err
	}
	project, fromFile, err := rules.LoadProject(root)
	if err != nil {
		return rules.Set{}, err
	}
	set, err := rules.NewSet(builtin, project, fromFile, disabled)
	if err != nil {
		return rules.Set{}, err
	}
	for _, w := range set.Warnings {
		fmt.Fprintf(stderr, "prosa: warning: %s\n", w)
	}
	return set, nil
}

// reviewerSource builds the reviewer of one command, from the planner
// settings: the patch model and effort do the job (ADR-034).
type reviewerSource func(s plannerSettings) engine.Reviewer

// newReviewer is the real reviewerSource: the reviewer on the backend the
// environment chooses, created on the first Review so that a run with
// nothing to review needs neither a backend nor credentials.
func newReviewer(s plannerSettings) engine.Reviewer {
	model := s.patchModel
	if model == "" {
		model = s.model
	}
	return &lazyReviewer{model: model, effort: s.patchEffort}
}

// lazyReviewer opens its reviewer on the first Review.
type lazyReviewer struct {
	model, effort string
	once          sync.Once
	r             *planner.Reviewer
	err           error
}

func (l *lazyReviewer) Review(ctx context.Context, in engine.ReviewInput) ([]engine.Violation, error) {
	l.once.Do(func() {
		b, err := planner.NewBackend(os.Getenv)
		if err != nil {
			l.err = err
			return
		}
		l.r = &planner.Reviewer{Backend: b, Model: l.model, Effort: l.effort}
	})
	if l.err != nil {
		return nil, l.err
	}
	return l.r.Review(ctx, in)
}

// LastResult forwards the result of the reviewer, once it exists.
func (l *lazyReviewer) LastResult() planner.Result {
	if l.r == nil {
		return planner.Result{}
	}
	return l.r.LastResult()
}
