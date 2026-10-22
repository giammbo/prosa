package planner

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/giammbo/prosa/internal/engine"
	"github.com/giammbo/prosa/internal/intent"
	"github.com/giammbo/prosa/internal/lock"
)

// seen answers like scripted and keeps the requests.
type seen struct {
	scripted
	reqs []anthropic.BetaMessageNewParams
}

func (s *seen) New(ctx context.Context, p anthropic.BetaMessageNewParams) (*anthropic.BetaMessage, error) {
	s.reqs = append(s.reqs, p)
	return s.scripted.New(ctx, p)
}

// call renders an assistant message with one tool call.
func call(id, name, input string) string {
	return fmt.Sprintf(`{"id":"m_%s","type":"message","role":"assistant","model":"claude-opus-5-5","stop_reason":"tool_use",
 "content":[{"type":"tool_use","id":%q,"name":%q,"input":%s}],
 "usage":{"input_tokens":10,"output_tokens":5}}`, id, id, name, input)
}

func patchInput(validate func(context.Context, engine.Desired) error) engine.PlanInput {
	in := planInput("app", "An SQS queue named orders, kept 60 seconds.", validate)
	in.Mode = "patch"
	in.Base = []lock.Resource{{Key: "queue.orders", Type: "aws_sqs_queue", Chosen: map[string]any{
		"name": "orders", "message_retention_seconds": json.Number("345600")}}}
	return in
}

func TestSubmitChangesApplied(t *testing.T) {
	calls := 0
	m := &seen{scripted: scripted{raw: []string{call("t1", "submit_changes",
		`{"changes":[{"op":"set","key":"queue.orders","path":"message_retention_seconds","value":60},
		 {"op":"add","key":"queue.dlq","type":"aws_sqs_queue","config":{"name":"orders-dlq"}}]}`)}}}
	d, err := (&LLM{Messages: m, Model: DefaultModel}).Plan(context.Background(), patchInput(validator(&calls)))
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Resources) != 2 || d.Resources[0].Key != "queue.orders" || d.Resources[1].Key != "queue.dlq" {
		t.Fatalf("resources = %+v, want the base plus the added one", d.Resources)
	}
	if n, ok := d.Resources[0].Config["message_retention_seconds"].(json.Number); !ok || n.String() != "60" {
		t.Errorf("retention = %#v, want json.Number 60", d.Resources[0].Config["message_retention_seconds"])
	}
	if d.Resources[0].Config["name"] != "orders" {
		t.Errorf("name = %v, want the base's", d.Resources[0].Config["name"])
	}
	if len(d.Patch) != 2 || d.Patch[0].Op != "set" || d.Patch[0].Path != "message_retention_seconds" || d.Patch[1].Op != "add" || d.Patch[1].Config["name"] != "orders-dlq" {
		t.Errorf("patch = %+v, want the two submitted operations", d.Patch)
	}
	if calls != 1 {
		t.Errorf("Validate called %d times, want 1", calls)
	}
}

func TestSubmitChangesEmptyPatchIsValid(t *testing.T) {
	m := &seen{scripted: scripted{raw: []string{call("t1", "submit_changes", `{"changes":[]}`)}}}
	d, err := (&LLM{Messages: m, Model: DefaultModel}).Plan(context.Background(), patchInput(validator(new(int))))
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Resources) != 1 || d.Patch == nil || len(d.Patch) != 0 {
		t.Errorf("desired = %+v, want the base and an empty, non-nil patch", d)
	}
}

func TestSubmitChangesProblems(t *testing.T) {
	m := &seen{scripted: scripted{raw: []string{
		call("t1", "submit_changes", `{"changes":[{"op":"set","key":"db.replica","path":"size","value":1}]}`),
		call("t2", "submit_changes", `{"changes":[{"op":"set","key":"queue.orders","path":"tags.env","value":"x"}]}`),
	}}}
	l := &LLM{Messages: m, Model: DefaultModel}
	d, err := l.Plan(context.Background(), patchInput(validator(new(int))))
	if err != nil {
		t.Fatal(err)
	}
	if got := d.Resources[0].Config["tags"]; got == nil {
		t.Errorf("second patch not applied: %+v", d.Resources[0].Config)
	}
	if len(m.reqs) != 2 {
		t.Fatalf("%d requests, want 2", len(m.reqs))
	}
	data, _ := json.Marshal(m.reqs[1].Messages)
	var msgs []wireMessage
	if err := json.Unmarshal(data, &msgs); err != nil {
		t.Fatal(err)
	}
	b := msgs[len(msgs)-1].Content[0]
	want := (&lock.PatchError{Index: 0, Op: lock.PatchOp{Op: "set", Key: "db.replica", Path: "size"}, Reason: `resource "db.replica" not found`}).Error()
	if b["is_error"] != true || resultText(b) != want {
		t.Errorf("tool result = %v, text %q, want an error with %q", b["is_error"], resultText(b), want)
	}
	if got := l.LastResult().Problems; got != 1 {
		t.Errorf("problems = %d, want 1", got)
	}
}

func TestSubmitChangesValidationProblems(t *testing.T) {
	m := &seen{scripted: scripted{raw: []string{
		call("t1", "submit_changes", `{"changes":[{"op":"set","key":"queue.orders","path":"retention","value":1}]}`),
		call("t2", "submit_changes", `{"changes":[]}`),
	}}}
	if _, err := (&LLM{Messages: m, Model: DefaultModel}).Plan(context.Background(), patchInput(validator(new(int), "retention"))); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(m.reqs[1].Messages)
	if !strings.Contains(string(data), `app/queue.orders: unsupported argument`) {
		t.Errorf("second request lacks the validation problem: %s", data)
	}
}

func TestSubmitChangesBadInput(t *testing.T) {
	for name, input := range map[string]string{
		"no changes":    `{}`,
		"unknown field": `{"changes":[],"resources":[]}`,
		"not a list":    `{"changes":"x"}`,
		"unknown op":    `{"changes":[{"op":"rename","key":"queue.orders"}]}`,
	} {
		m := &seen{scripted: scripted{raw: []string{call("t1", "submit_changes", input), call("t2", "submit_changes", `{"changes":[]}`)}}}
		if _, err := (&LLM{Messages: m, Model: DefaultModel}).Plan(context.Background(), patchInput(validator(new(int)))); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		data, _ := json.Marshal(m.reqs[1].Messages)
		if !strings.Contains(string(data), `"is_error":true`) {
			t.Errorf("%s: no error went back to the model: %s", name, data)
		}
	}
}

func TestSubmitChangesBlockers(t *testing.T) {
	calls := 0
	m := &seen{scripted: scripted{raw: []string{call("t1", "submit_changes", `{"changes":[],"blockers":["Three replicas contradicts the check."]}`)}}}
	d, err := (&LLM{Messages: m, Model: DefaultModel}).Plan(context.Background(), patchInput(validator(&calls)))
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Blockers) != 1 || calls != 0 {
		t.Errorf("blockers %q, Validate calls %d, want one blocker and no validation", d.Blockers, calls)
	}
}

func TestFullStateAcceptedInPatchMode(t *testing.T) {
	m := &seen{scripted: scripted{raw: []string{call("t1", "submit_desired_state",
		`{"resources":[{"key":"queue.orders","type":"aws_sqs_queue","config":{"name":"orders"}}],"checks":[]}`)}}}
	d, err := (&LLM{Messages: m, Model: DefaultModel}).Plan(context.Background(), patchInput(validator(new(int))))
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Resources) != 1 || d.Patch != nil || d.Resources[0].Config["message_retention_seconds"] != nil {
		t.Errorf("desired = %+v, want the complete state as submitted, no patch", d)
	}
}

func TestToolsHaveSubmitChanges(t *testing.T) {
	var names []string
	var tool *anthropic.BetaToolParam
	for _, u := range anthropicTools(tools()) {
		names = append(names, u.OfTool.Name)
		if u.OfTool.Name == "submit_changes" {
			tool = u.OfTool
		}
	}
	if strings.Join(names, ",") != "search_resource_types,get_resource_schema,submit_desired_state,submit_changes" {
		t.Fatalf("tools = %v", names)
	}
	data, _ := json.Marshal(tool.InputSchema)
	for _, want := range []string{`"changes"`, `"blockers"`, `"set"`, `"unset"`, `"add"`, `"remove"`, `"depends_on"`, `"checks"`, `"required":["changes"]`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("submit_changes schema lacks %s: %s", want, data)
		}
	}
	for _, want := range []string{"submit_desired_state", "submit_changes"} {
		if !strings.Contains(nudge, want) {
			t.Errorf("nudge %q lacks %s", nudge, want)
		}
	}
}

func TestUserMessagePatchMode(t *testing.T) {
	in := patchInput(nil)
	in.PreviousBody = "An SQS queue named orders."
	got, err := userMessage(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"<base>", `"message_retention_seconds": 345600`, "</base>", "submit_changes"} {
		if !strings.Contains(got, want) {
			t.Errorf("message lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "<previous_resources>") || strings.Contains(got, "call submit_desired_state") {
		t.Errorf("an environment patch must show the base, not previous resources:\n%s", got)
	}

	// Without a base the previous state is what a patch changes.
	in = planInput("app", "Two queues.", nil)
	in.Mode = "patch"
	in.PreviousBody = "One queue."
	in.Previous = []lock.Resource{{Key: "queue.orders", Type: "aws_sqs_queue", Chosen: map[string]any{"name": "orders"}}}
	got, _ = userMessage(in)
	for _, want := range []string{"<previous_resources>", "One queue.", "submit_changes"} {
		if !strings.Contains(got, want) {
			t.Errorf("message lacks %q:\n%s", want, got)
		}
	}

	// A new intent is planned in full.
	in = planInput("app", "One queue.", nil)
	in.Mode = "plan"
	got, _ = userMessage(in)
	if !strings.Contains(got, "call submit_desired_state") || strings.Contains(got, "submit_changes") {
		t.Errorf("plan mode must ask for submit_desired_state only:\n%s", got)
	}
}

func TestUserMessagePatchModeShowsOldPatchAndProblems(t *testing.T) {
	in := patchInput(nil)
	in.OldPatch = []lock.PatchOp{{Op: "set", Key: "queue.orders", Path: "message_retention_seconds", Value: 60}}
	in.Problems = []string{`patch op 0 (set queue.gone.size): resource "queue.gone" not found`}
	in.RequiredNames = []string{"queue.orders.name"}
	in.Env = &intent.Env{Name: "staging", Body: "# staging\n\nShort retention.\n"}
	got, err := userMessage(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"<old_patch>", `"path": "message_retention_seconds"`, "<patch_problems>", "queue.gone", "<required_names>\nqueue.orders.name\n</required_names>"} {
		if !strings.Contains(got, want) {
			t.Errorf("message lacks %q:\n%s", want, got)
		}
	}
}

func TestSystemPromptNamesAndPatchRules(t *testing.T) {
	for _, want := range []string{
		"submit_changes",
		"name_prefix",
		"required_names",
		"If the intent states a name, use it",
		"With submit_desired_state, submit the complete set of resources",
	} {
		if !strings.Contains(systemPrompt, want) {
			t.Errorf("system prompt lacks %q", want)
		}
	}
	if strings.Contains(envRule, "ignore the parts about other intents") {
		t.Errorf("envRule still talks about other intents: %q", envRule)
	}
}

func TestCacheKeyPatchMode(t *testing.T) {
	key := func(mut func(*engine.PlanInput)) string {
		in := patchInput(nil)
		in.Env = &intent.Env{Name: "staging", Hash: "sha256:e1", Body: "# staging\n\n## app\nShort.\n"}
		mut(&in)
		k, err := cacheKey(in, "m")
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	base := key(func(*engine.PlanInput) {})
	if again := key(func(*engine.PlanInput) {}); again != base {
		t.Fatal("the key is not stable")
	}
	for name, mut := range map[string]func(*engine.PlanInput){
		"mode":        func(in *engine.PlanInput) { in.Mode = "plan" },
		"base":        func(in *engine.PlanInput) { in.Base[0].Chosen["name"] = "other" },
		"base checks": func(in *engine.PlanInput) { in.BaseChecks = []lock.Check{{Name: "x", Type: "unverifiable"}} },
		"old patch": func(in *engine.PlanInput) {
			in.OldPatch = []lock.PatchOp{{Op: "set", Key: "queue.orders", Path: "name", Value: "x"}}
		},
		"problems":       func(in *engine.PlanInput) { in.Problems = []string{"p"} },
		"required names": func(in *engine.PlanInput) { in.RequiredNames = []string{"queue.orders.name"} },
	} {
		if key(mut) == base {
			t.Errorf("the key ignores %s", name)
		}
	}
	// Planned with or without an empty old patch is the same request.
	if key(func(in *engine.PlanInput) { in.OldPatch = []lock.PatchOp{} }) != base {
		t.Error("nil and empty old patch must share a key")
	}
}

// The cache keeps the patch with the state: an empty patch stays a patch.
func TestCacheStoresPatch(t *testing.T) {
	for name, patch := range map[string][]lock.PatchOp{
		"patch": {{Op: "set", Key: "web", Path: "count", Value: json.Number("3")}},
		"empty": {},
		"none":  nil,
	} {
		d := cacheDesired()
		d.Patch = patch
		inner := &countPlanner{d: d}
		c := &Cached{Inner: inner, Dir: t.TempDir(), Model: "m"}
		first, err := c.Plan(context.Background(), cacheInput())
		if err != nil {
			t.Fatal(err)
		}
		second, err := c.Plan(context.Background(), cacheInput())
		if err != nil {
			t.Fatal(err)
		}
		if inner.calls != 1 || !reflect.DeepEqual(first, second) || (patch == nil) != (second.Patch == nil) || len(second.Patch) != len(patch) {
			t.Errorf("%s: calls %d, miss %+v, hit %+v", name, inner.calls, first.Patch, second.Patch)
		}
	}
}

// A rule fix of an applied intent in root mode (no base) passes problems the
// planner must see and the cache must key on (ADR-034).
func TestRootPatchModeShowsProblems(t *testing.T) {
	in := planInput("app", "Two queues.", nil)
	in.Mode = "patch"
	in.PreviousBody = "Two queues."
	in.Previous = []lock.Resource{{Key: "queue.orders", Type: "aws_sqs_queue", Chosen: map[string]any{"name": "orders"}}}
	in.Problems = []string{"AWS-SQS-001 queue.orders: not encrypted"}
	got, err := userMessage(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "<problems>\nAWS-SQS-001 queue.orders: not encrypted\n</problems>") {
		t.Errorf("message lacks the problems block:\n%s", got)
	}
	if !strings.Contains(systemPrompt, "<problems> lists what is wrong with the previous resources") {
		t.Error("system prompt does not explain <problems>")
	}
	k1, _ := cacheKey(in, "m")
	in.Problems = []string{"AWS-SQS-002 queue.orders: other"}
	if k2, _ := cacheKey(in, "m"); k1 == k2 {
		t.Error("root mode cache key ignores Problems")
	}
}
