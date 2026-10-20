// Package planner implements engine.Planner with an LLM: a tool-use loop in
// which the model reads the provider's schemas and submits the intent's
// desired state, which the engine validates (ADR-015). The loop is the same
// for every provider; a Backend talks to one (ADR-031).
package planner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/giammbo/prosa/internal/engine"
	"github.com/giammbo/prosa/internal/lock"
)

// Request settings (see the M3 constraints; max_tokens is 64000 since the
// anthropic backend streams).
const (
	DefaultModel    = "claude-opus-5-5"
	DefaultMaxTurns = 25
	maxTokens       = 64000
)

// Jobs, and their default efforts (ADR-030): planning thinks hard, a patch
// little.
const (
	JobPlan            = "plan"
	JobPatch           = "patch"
	DefaultEffort      = "high"
	DefaultPatchEffort = "low"
)

// nudge is the user message sent after a turn with no tool call.
const nudge = "Submit your answer with the submit_desired_state tool (the complete desired state) or, when the request gives a base or previous resources, with submit_changes (only what changes): they are the only way to answer. If the intent cannot be planned as written, submit blockers."

// LLM plans an intent through a Backend; with none, Anthropic on Messages.
// Model and Effort plan; PatchModel and PatchEffort answer a PlanInput of
// Mode "patch" (empty PatchModel: Model; empty efforts: DefaultEffort and
// DefaultPatchEffort). MaxTurns bounds the requests per intent
// (DefaultMaxTurns when 0); Log, when set, gets one line per turn with the
// names of the tools called ("planner <intent>: turn N, <tools>" when
// LogIntent is set, "turn N: <tools>" otherwise).
type LLM struct {
	Backend     Backend
	Messages    Messages
	Model       string
	PatchModel  string
	Effort      string
	PatchEffort string
	MaxTurns    int
	Log         io.Writer
	LogIntent   bool

	last lastResult
}

// LastResult returns how the last Plan went (see Result).
func (l *LLM) LastResult() Result { return l.last.get() }

// NewLLM returns a planner on Anthropic with the SDK's default client. The
// SDK resolves credentials (ANTHROPIC_API_KEY or an `ant auth login`
// profile) when a request is made: without any, the first Plan fails with a
// message saying how to set them.
func NewLLM(model string) (*LLM, error) {
	if model == "" {
		return nil, errors.New("planner: no model")
	}
	return &LLM{Messages: newSDKMessages(), Model: model}, nil
}

// backend returns Backend, or Anthropic on Messages.
func (l *LLM) backend() Backend {
	if l.Backend != nil {
		return l.Backend
	}
	return Anthropic{Messages: l.Messages}
}

// jobModel returns the job of a PlanInput's mode and the model that does it.
func jobModel(mode, model, patchModel string) (job, m string) {
	if mode != JobPatch {
		return JobPlan, model
	}
	if patchModel == "" {
		return JobPatch, model
	}
	return JobPatch, patchModel
}

// job returns the job of in, with its model and effort.
func (l *LLM) job(in engine.PlanInput) (job, model, effort string) {
	job, model = jobModel(in.Mode, l.Model, l.PatchModel)
	effort = l.Effort
	if job == JobPatch {
		effort = l.PatchEffort
	}
	switch {
	case effort != "":
	case job == JobPatch:
		effort = DefaultPatchEffort
	default:
		effort = DefaultEffort
	}
	return job, model, effort
}

// Plan runs the tool-use loop until the model submits a valid desired state
// or blockers. Validation problems go back to the model as an error result;
// a turn with no tool call gets the nudge; after MaxTurns requests the error
// lists the last problems, with the backend and the model.
func (l *LLM) Plan(ctx context.Context, in engine.PlanInput) (engine.Desired, error) {
	b := l.backend()
	job, model, effort := l.job(in)
	res := Result{Intent: in.Intent.Name, Job: job, Backend: b.Name(), Model: model}
	defer func() { l.last.set(res) }()
	maxTurns := l.MaxTurns
	if maxTurns <= 0 {
		maxTurns = DefaultMaxTurns
	}
	user, err := userMessage(in)
	if err != nil {
		return engine.Desired{}, fmt.Errorf("planner: intent %s: %w", in.Intent.Name, err)
	}
	who := fmt.Sprintf("backend %s, model %s", b.Name(), model)
	req := TurnRequest{System: systemPrompt, Tools: tools(), Model: model, Effort: effort,
		Messages: []Message{{Role: "user", Text: user}}}
	var last []string
	for turn := 1; turn <= maxTurns; turn++ {
		t, err := b.Turn(ctx, req)
		if err != nil {
			return engine.Desired{}, planErr("planner request failed", fmt.Errorf("planner: intent %s, turn %d (%s): %w", in.Intent.Name, turn, who, err))
		}
		res.Turns = turn
		res.add(t)
		if err := stopError(in.Intent.Name, t); err != nil {
			return engine.Desired{}, planErr("planner stopped: "+t.Stop, err)
		}
		if m, ok := t.message(); ok {
			req.Messages = append(req.Messages, m)
		}
		var results []ToolResult
		var names []string
		for _, call := range t.ToolCalls {
			names = append(names, call.Name)
			var text string
			isError := false
			switch call.Name {
			case toolSearch:
				text, isError = runSearch(in.Schemas, call.Input)
			case toolSchema:
				text, isError = runSchema(in.Schemas, call.Input)
			case toolSubmit, toolChanges:
				d, probs, err := submit(ctx, in, call.Name, call.Input)
				if err != nil {
					return engine.Desired{}, fmt.Errorf("planner: intent %s: %w", in.Intent.Name, err)
				}
				if len(probs) == 0 {
					l.logTurn(in.Intent.Name, turn, names)
					return d, nil
				}
				last = probs
				res.Problems += len(probs)
				text, isError = strings.Join(probs, "\n"), true
			default:
				text, isError = fmt.Sprintf("unknown tool %q", call.Name), true
			}
			results = append(results, ToolResult{ID: call.ID, Content: text, IsError: isError})
		}
		l.logTurn(in.Intent.Name, turn, names)
		if len(results) == 0 {
			req.Messages = append(req.Messages, Message{Role: "user", Text: nudge})
			continue
		}
		req.Messages = append(req.Messages, Message{Role: "user", Results: results})
	}
	if last == nil {
		return engine.Desired{}, planErr(fmt.Sprintf("no desired state submitted after %d turns", maxTurns),
			fmt.Errorf("planner: intent %s: no desired state submitted after %d turns (%s)", in.Intent.Name, maxTurns, who))
	}
	return engine.Desired{}, planErr(fmt.Sprintf("no valid desired state after %d turns: %d problem(s)", maxTurns, len(last)), fmt.Errorf("planner: intent %s: no valid desired state after %d turns (%s); last problems:\n  %s",
		in.Intent.Name, maxTurns, who, strings.Join(last, "\n  ")))
}

// planErr marks err as a planner failure with a value-free message for the
// event log.
func planErr(msg string, err error) error {
	return &engine.KindError{Kind: engine.KindPlanner, Msg: msg, Err: err}
}

// submit decodes a submission of the tool (submit_desired_state or
// submit_changes), resolves a patch on the state the input shows, puts the
// result in canonical form (the one the lock and the cache keep) and
// validates it. Problems is empty when the desired state is accepted: valid,
// or blocked (its resources are then not validated, the plan being blocked
// anyway). A malformed input, or a patch that does not apply, is a problem;
// the error is a failure of Validate other than a *ValidationError
// (transport, context).
func submit(ctx context.Context, in engine.PlanInput, tool, input string) (engine.Desired, []string, error) {
	var d engine.Desired
	var probs []string
	if tool == toolChanges {
		d, probs = parseChanges(input)
	} else {
		d, probs = parseSubmission(in.Intent.Name, input)
	}
	if len(d.Blockers) > 0 {
		if len(probs) > 0 {
			d.Resources = nil
		}
		return d, nil, nil
	}
	if len(probs) > 0 {
		return engine.Desired{}, probs, nil
	}
	var err error
	if d, err = in.Resolve(d); err != nil {
		var pe *lock.PatchError
		if errors.As(err, &pe) {
			return engine.Desired{}, []string{pe.Error()}, nil
		}
		return engine.Desired{}, nil, err
	}
	if in.Canonical != nil {
		d = in.Canonical(d)
	}
	if in.Validate == nil {
		return d, nil, nil
	}
	err = in.Validate(ctx, d)
	var ve *engine.ValidationError
	switch {
	case err == nil:
		return d, nil, nil
	case errors.As(err, &ve):
		return engine.Desired{}, problemLines(ve), nil
	default:
		return engine.Desired{}, nil, err
	}
}

// stopError returns the error for a turn that cannot be continued: a
// refusal (after any fallback), or output cut at max_tokens or at the
// context window.
func stopError(name string, t Turn) error {
	switch t.Stop {
	case StopRefusal:
		detail := t.StopDetail
		if detail == "" {
			detail = "no detail"
		}
		return fmt.Errorf("planner: intent %s: the model refused (%s)", name, detail)
	case StopMaxTokens:
		detail := t.StopDetail
		if detail == "" {
			detail = StopMaxTokens
		}
		return fmt.Errorf("planner: intent %s: response cut off (%s)", name, detail)
	}
	return nil
}

// logTurn writes the turn line to Log: never inputs or thinking.
func (l *LLM) logTurn(intent string, turn int, names []string) {
	if l.Log == nil {
		return
	}
	called := strings.Join(names, ", ")
	if called == "" {
		called = "no tool call"
	}
	if l.LogIntent {
		fmt.Fprintf(l.Log, "planner %s: turn %d, %s\n", intent, turn, called)
		return
	}
	fmt.Fprintf(l.Log, "turn %d: %s\n", turn, called)
}
