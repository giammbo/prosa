package planner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/giammbo/prosa/internal/engine"
)

// ReviewPromptVersion names the reviewer's system prompt, its tool and its
// user message format: change it with any of them, since it is part of the
// review cache's key and of the review recordings.
const ReviewPromptVersion = "r1"

// The reviewer's job, its default effort and turns (ADR-034): checking a
// state against prose rules is a short, light job.
const (
	JobReview             = "review"
	DefaultReviewEffort   = "low"
	DefaultReviewMaxTurns = 4
	toolReview            = "submit_review"
)

// reviewNudge is the user message sent after a turn with no tool call.
const reviewNudge = "Answer with the submit_review tool: it is the only way to answer. Submit an empty list of violations when no resource breaks a rule."

// reviewSystemPrompt is the reviewer's stable system prompt: no per-request
// data, like the planner's.
const reviewSystemPrompt = `You are the reviewer of Prosa, an engine that reconciles infrastructure described in prose against cloud provider APIs. A planner has translated an intent into a desired state: resources of the Terraform AWS provider (hashicorp/aws), each with a key, a type and a config. Your job is to check that desired state against the rules the request gives, and only against them: no other best practice, preference or guess counts.

- Report a violation only when a resource clearly breaks the text of a rule. When the config does not say enough to tell, it is not a violation.
- A violation names the rule's ID, the key of the resource that breaks it, and a short reason that says what in the config breaks the rule and what would satisfy it.
- An exception is a paragraph of the intent that names a rule's ID: the person's reason for departing from it. It covers what it says and nothing more; a resource that departs from the rule beyond what the exception allows still breaks it.
- A value "${<intent>/<key>.<attribute>}" is a reference: it names an attribute of another resource, of this intent or of one listed in referable_resources, whose value is known only after the apply. Judge it by what it refers to, never as a literal.

Answer only by calling submit_review, once, with every violation you find; with an empty list when no resource breaks a rule.`

// reviewTools returns the reviewer's one tool, always the same bytes.
func reviewTools() []ToolDef {
	str := func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
	violation := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"rule":   str("The ID of the rule broken, as the request gives it, such as \"AWS-NET-001\"."),
			"key":    str("The key of the resource that breaks the rule, as the request gives it."),
			"reason": str("Why the resource breaks the rule, and what would satisfy it, in one or two sentences."),
		},
		"required":             []string{"rule", "key", "reason"},
		"additionalProperties": false,
	}
	return []ToolDef{{
		Name:        toolReview,
		Description: "Submit the review of the desired state: every violation of a rule, one per rule and resource; an empty list when no resource breaks a rule.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"violations": map[string]any{"type": "array", "items": violation,
					"description": "The violations found; empty when no resource breaks a rule."},
			},
			"required":             []string{"violations"},
			"additionalProperties": false,
		},
	}}
}

// ReviewPromptFingerprint is the fingerprint of the reviewer's system prompt
// and tool, as review recordings carry it.
func ReviewPromptFingerprint() string {
	fp, err := fingerprint(anthropicParams(reviewSystemPrompt, reviewTools(), "", DefaultReviewEffort))
	if err != nil {
		panic(err) // fixed data: cannot fail
	}
	return fp
}

// Reviewer checks a desired state against the rules with a model, through
// Backend (engine.Reviewer, ADR-034). Model and Effort are those of the job
// (the CLI gives it the patch model; empty Effort: DefaultReviewEffort);
// MaxTurns bounds the requests per review (DefaultReviewMaxTurns when 0).
type Reviewer struct {
	Backend  Backend
	Model    string
	Effort   string
	MaxTurns int

	last lastResult
}

// LastResult returns how the last Review went (see Result).
func (r *Reviewer) LastResult() Result { return r.last.get() }

// Review asks the model for the violations of in's rules. A turn with no
// tool call gets a nudge, another tool or a malformed review an error
// result; the error after MaxTurns says no review was submitted. A
// violation that names a rule or a key not in the input is dropped
// (Result.Dropped).
func (r *Reviewer) Review(ctx context.Context, in engine.ReviewInput) ([]engine.Violation, error) {
	if r.Backend == nil {
		return nil, errors.New("reviewer: no backend")
	}
	effort := r.Effort
	if effort == "" {
		effort = DefaultReviewEffort
	}
	maxTurns := r.MaxTurns
	if maxTurns <= 0 {
		maxTurns = DefaultReviewMaxTurns
	}
	res := Result{Intent: in.Intent, Job: JobReview, Backend: r.Backend.Name(), Model: r.Model}
	defer func() { r.last.set(res) }()
	user, err := reviewMessage(in)
	if err != nil {
		return nil, fmt.Errorf("reviewer: intent %s: %w", in.Intent, err)
	}
	who := fmt.Sprintf("backend %s, model %s", r.Backend.Name(), r.Model)
	req := TurnRequest{System: reviewSystemPrompt, Tools: reviewTools(), Model: r.Model, Effort: effort,
		Messages: []Message{{Role: "user", Text: user}}}
	var last string
	for turn := 1; turn <= maxTurns; turn++ {
		t, err := r.Backend.Turn(ctx, req)
		if err != nil {
			return nil, planErr("reviewer request failed", fmt.Errorf("reviewer: intent %s, turn %d (%s): %w", in.Intent, turn, who, err))
		}
		res.Turns = turn
		res.add(t)
		if err := stopError("reviewer", in.Intent, t); err != nil {
			return nil, planErr("reviewer stopped: "+t.Stop, err)
		}
		if m, ok := t.message(); ok {
			req.Messages = append(req.Messages, m)
		}
		var results []ToolResult
		for _, call := range t.ToolCalls {
			if call.Name != toolReview {
				results = append(results, ToolResult{ID: call.ID, Content: fmt.Sprintf("unknown tool %q: answer with %s", call.Name, toolReview), IsError: true})
				continue
			}
			vs, problem := parseReview(call.Input)
			if problem != "" {
				last = problem
				results = append(results, ToolResult{ID: call.ID, Content: problem, IsError: true})
				continue
			}
			kept, dropped := keepKnown(in, vs)
			res.Dropped = dropped
			return kept, nil
		}
		if len(results) == 0 {
			req.Messages = append(req.Messages, Message{Role: "user", Text: reviewNudge})
			continue
		}
		req.Messages = append(req.Messages, Message{Role: "user", Results: results})
	}
	msg := fmt.Sprintf("no review submitted after %d turns", maxTurns)
	err = fmt.Errorf("reviewer: intent %s: %s (%s)", in.Intent, msg, who)
	if last != "" {
		err = fmt.Errorf("%w; last problem: %s", err, last)
	}
	return nil, planErr(msg, err)
}

// parseReview decodes a submit_review input; the problem is the error
// result for the model when it is malformed.
func parseReview(input string) ([]engine.Violation, string) {
	var sub struct {
		Violations *[]struct {
			Rule   string `json:"rule"`
			Key    string `json:"key"`
			Reason string `json:"reason"`
		} `json:"violations"`
	}
	if err := json.Unmarshal([]byte(input), &sub); err != nil {
		return nil, fmt.Sprintf("the input of %s is not valid: %v; want {\"violations\": [{\"rule\", \"key\", \"reason\"}]}", toolReview, err)
	}
	if sub.Violations == nil {
		return nil, fmt.Sprintf("the input of %s has no violations list; submit an empty list when no resource breaks a rule", toolReview)
	}
	vs := make([]engine.Violation, 0, len(*sub.Violations))
	for i, v := range *sub.Violations {
		if v.Rule == "" || v.Key == "" || v.Reason == "" {
			return nil, fmt.Sprintf("violation %d needs rule, key and reason", i)
		}
		vs = append(vs, engine.Violation{Rule: v.Rule, Key: v.Key, Reason: v.Reason})
	}
	return vs, ""
}

// keepKnown returns the violations that name a rule and a resource of in,
// and how many it dropped.
func keepKnown(in engine.ReviewInput, vs []engine.Violation) ([]engine.Violation, int) {
	ruleIDs, keys := map[string]bool{}, map[string]bool{}
	for _, r := range in.Rules {
		ruleIDs[r.ID] = true
	}
	for _, r := range in.Resources {
		keys[r.Key] = true
	}
	kept := make([]engine.Violation, 0, len(vs))
	for _, v := range vs {
		if ruleIDs[v.Rule] && keys[v.Key] {
			kept = append(kept, v)
		}
	}
	return kept, len(vs) - len(kept)
}

// reviewMessage renders what the reviewer checks: the desired resources,
// what they can refer to, the rules and the exceptions. Never the intent's
// text nor schemas: the rules and the exceptions are what counts.
func reviewMessage(in engine.ReviewInput) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "Review the intent %q", in.Intent)
	if in.Env != "" {
		fmt.Fprintf(&b, " in environment %q", in.Env)
	}
	b.WriteString(".\n\n")
	data, err := json.MarshalIndent(in.Resources, "", "  ")
	if err != nil {
		return "", fmt.Errorf("resources: %w", err)
	}
	fmt.Fprintf(&b, "<resources>\n%s\n</resources>\n\n", data)
	if len(in.Available) == 0 {
		b.WriteString("<referable_resources>none</referable_resources>\n\n")
	} else {
		b.WriteString("<referable_resources>\n")
		for _, a := range in.Available {
			fmt.Fprintf(&b, "${%s/%s.<attribute>} (%s)\n", a.Intent, a.Key, a.Type)
		}
		b.WriteString("</referable_resources>\n\n")
	}
	b.WriteString("<rules>\n")
	for _, r := range in.Rules {
		fmt.Fprintf(&b, "<rule id=%q title=%q>\n%s\n</rule>\n", r.ID, r.Title, strings.TrimSpace(r.Body))
	}
	b.WriteString("</rules>\n\n")
	if len(in.Exceptions) == 0 {
		b.WriteString("<exceptions>none</exceptions>\n\n")
	} else {
		b.WriteString("<exceptions>\n")
		for _, e := range in.Exceptions {
			fmt.Fprintf(&b, "%s: %s\n", e.ID, strings.Join(strings.Fields(e.Text), " "))
		}
		b.WriteString("</exceptions>\n\n")
	}
	b.WriteString("Check every resource against the rules, then call " + toolReview + ".")
	return b.String(), nil
}
