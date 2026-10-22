package planner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/giammbo/prosa/internal/engine"
	"github.com/giammbo/prosa/internal/intent"
	"github.com/giammbo/prosa/internal/lock"
)

// fakeCatalog answers for SQS queues only.
type fakeCatalog struct{}

func (fakeCatalog) Search(query string, limit int) []string {
	if strings.Contains(query, "queue") {
		return []string{"aws_sqs_queue", "aws_sqs_queue_policy", "aws_sqs_queue_redrive_policy"}[:min(limit, 3)]
	}
	return nil
}

func (fakeCatalog) Describe(typeName string) (string, error) {
	if typeName == "aws_sqs_queue" {
		return "resource aws_sqs_queue (schema version 0)\n  name string, optional\n  message_retention_seconds number, optional\n", nil
	}
	return "", fmt.Errorf("unknown resource type %q", typeName)
}

// validator returns a Validate func that rejects any config with an
// attribute in bad, and counts its calls.
func validator(calls *int, bad ...string) func(context.Context, engine.Desired) error {
	return func(_ context.Context, d engine.Desired) error {
		*calls++
		var probs []engine.ValidationProblem
		for _, r := range d.Resources {
			for _, b := range bad {
				if _, ok := r.Config[b]; ok {
					probs = append(probs, engine.ValidationProblem{Intent: "app", Key: r.Key, Type: r.Type,
						Message: fmt.Sprintf("unsupported argument %q", b)})
				}
			}
		}
		if len(probs) > 0 {
			return &engine.ValidationError{Problems: probs}
		}
		return nil
	}
}

func planInput(name, body string, validate func(context.Context, engine.Desired) error) engine.PlanInput {
	return engine.PlanInput{
		Intent:   intent.Intent{Name: name, Hash: "sha256:" + name, Autonomy: "approve", Body: body},
		Region:   "eu-south-1",
		Schemas:  fakeCatalog{},
		Validate: validate,
	}
}

func loadReplay(t *testing.T, name string) *Replay {
	t.Helper()
	r, err := LoadReplay(filepath.Join("testdata", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// wireMessage is a request message as sent on the wire.
type wireMessage struct {
	Role    string           `json:"role"`
	Content []map[string]any `json:"content"`
}

// messages returns the messages of request n of r, as JSON.
func messages(t *testing.T, r *Replay, n int) []wireMessage {
	t.Helper()
	data, err := json.Marshal(r.Requests[n].Messages)
	if err != nil {
		t.Fatal(err)
	}
	var out []wireMessage
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// resultText joins the text content of a tool_result block.
func resultText(block map[string]any) string {
	var b strings.Builder
	content, _ := block["content"].([]any)
	for _, c := range content {
		if m, ok := c.(map[string]any); ok {
			s, _ := m["text"].(string)
			b.WriteString(s)
		}
	}
	return b.String()
}

func TestPlanSubmitsValidState(t *testing.T) {
	r := loadReplay(t, "submit_valid")
	var log bytes.Buffer
	calls := 0
	l := &LLM{Messages: r, Model: DefaultModel, Log: &log}
	d, err := l.Plan(context.Background(), planInput("app", "An SQS queue named orders.", validator(&calls)))
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Blockers) != 0 || len(d.Resources) != 1 {
		t.Fatalf("desired = %+v", d)
	}
	res := d.Resources[0]
	if res.Key != "queue.orders" || res.Type != "aws_sqs_queue" || res.Config["name"] != "orders" {
		t.Errorf("resource = %+v", res)
	}
	if n, ok := res.Config["message_retention_seconds"].(json.Number); !ok || n.String() != "345600" {
		t.Errorf("message_retention_seconds = %#v, want json.Number 345600", res.Config["message_retention_seconds"])
	}
	if tags, ok := res.Config["tags"].(map[string]any); !ok || tags["team"] != "platform" {
		t.Errorf("tags = %#v", res.Config["tags"])
	}
	if calls != 1 {
		t.Errorf("Validate called %d times, want 1", calls)
	}
	if len(r.Requests) != 3 {
		t.Fatalf("%d requests, want 3", len(r.Requests))
	}

	// The two searches of turn 1 are answered in one user message.
	msgs := messages(t, r, 1)
	last := msgs[len(msgs)-1]
	if last.Role != "user" || len(last.Content) != 2 {
		t.Fatalf("second request ends with %+v, want one user message with two tool results", last)
	}
	for i, id := range []string{"toolu_01VdJ8nK2pQ4rS6tU8wX0yZa", "toolu_01WeK9oL3qR5sT7uV9xY1zAb"} {
		b := last.Content[i]
		if b["type"] != "tool_result" || b["tool_use_id"] != id || b["is_error"] == true {
			t.Errorf("result %d = %v", i, b)
		}
	}
	if got := resultText(last.Content[0]); !strings.Contains(got, "aws_sqs_queue\n") || !strings.Contains(got, "aws_sqs_queue_policy") {
		t.Errorf("search result = %q", got)
	}
	// The assistant turn, thinking included, is passed back unchanged.
	asst := msgs[len(msgs)-2]
	if asst.Role != "assistant" || asst.Content[0]["type"] != "thinking" || asst.Content[0]["signature"] == "" {
		t.Errorf("assistant turn = %+v", asst)
	}

	msgs = messages(t, r, 2)
	last = msgs[len(msgs)-1]
	if got := resultText(last.Content[0]); !strings.Contains(got, "message_retention_seconds number") {
		t.Errorf("schema result = %q", got)
	}

	wantLog := "turn 1: search_resource_types, search_resource_types\nturn 2: get_resource_schema\nturn 3: submit_desired_state\n"
	if log.String() != wantLog {
		t.Errorf("log = %q, want %q", log.String(), wantLog)
	}
}

func TestPlanRetriesOnValidationProblems(t *testing.T) {
	r := loadReplay(t, "retry_validation")
	calls := 0
	l := &LLM{Messages: r, Model: DefaultModel}
	d, err := l.Plan(context.Background(), planInput("app", "An SQS queue named orders.", validator(&calls, "retention")))
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Resources) != 1 || d.Resources[0].Config["retention"] != nil {
		t.Errorf("desired = %+v", d)
	}
	if calls != 2 || len(r.Requests) != 2 {
		t.Fatalf("Validate calls = %d, requests = %d, want 2 and 2", calls, len(r.Requests))
	}
	msgs := messages(t, r, 1)
	last := msgs[len(msgs)-1]
	if last.Role != "user" || len(last.Content) != 1 {
		t.Fatalf("second request ends with %+v", last)
	}
	b := last.Content[0]
	if b["type"] != "tool_result" || b["tool_use_id"] != "toolu_01ZhN2rO6tU8vW0xY2aB4cDe" || b["is_error"] != true {
		t.Errorf("tool result = %v", b)
	}
	if got, want := resultText(b), `app/queue.orders: unsupported argument "retention"`; got != want {
		t.Errorf("tool result text = %q, want %q", got, want)
	}
}

func TestPlanStopsAfterMaxTurns(t *testing.T) {
	r := loadReplay(t, "always_invalid")
	calls := 0
	l := &LLM{Messages: r, Model: DefaultModel, MaxTurns: 2}
	_, err := l.Plan(context.Background(), planInput("app", "An SQS queue named orders.", validator(&calls, "retention", "retention_days")))
	if err == nil {
		t.Fatal("Plan succeeded, want an error after 2 turns")
	}
	for _, want := range []string{"2 turns", `app/queue.orders: unsupported argument "retention_days"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to contain %q", err, want)
		}
	}
	if strings.Contains(err.Error(), `"retention"`) {
		t.Errorf("err = %v, want only the last problems", err)
	}
	if len(r.Requests) != 2 {
		t.Errorf("%d requests, want 2", len(r.Requests))
	}
}

func TestPlanReturnsBlockers(t *testing.T) {
	r := loadReplay(t, "blockers")
	calls := 0
	l := &LLM{Messages: r, Model: DefaultModel}
	d, err := l.Plan(context.Background(), planInput("app", "Three replicas.\n\n## Checks\n- Exactly two tasks run.", validator(&calls)))
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Blockers) != 1 || !strings.Contains(d.Blockers[0], "three replicas") {
		t.Errorf("blockers = %q", d.Blockers)
	}
	if calls != 0 {
		t.Errorf("Validate called %d times on a blocked state, want 0", calls)
	}
}

func TestPlanRefusal(t *testing.T) {
	r := loadReplay(t, "refusal")
	calls := 0
	l := &LLM{Messages: r, Model: DefaultModel}
	_, err := l.Plan(context.Background(), planInput("app", "An SQS queue named orders.", validator(&calls)))
	if err == nil || !strings.Contains(err.Error(), "refus") || !strings.Contains(err.Error(), "cyber") {
		t.Errorf("err = %v, want a refusal naming the category", err)
	}
}

func TestPlanNudgesWithoutToolCall(t *testing.T) {
	r := loadReplay(t, "nudge")
	calls := 0
	l := &LLM{Messages: r, Model: DefaultModel}
	if _, err := l.Plan(context.Background(), planInput("app", "An SQS queue named orders.", validator(&calls))); err != nil {
		t.Fatal(err)
	}
	msgs := messages(t, r, 1)
	last := msgs[len(msgs)-1]
	if last.Role != "user" || len(last.Content) != 1 || last.Content[0]["type"] != "text" ||
		!strings.Contains(last.Content[0]["text"].(string), "submit_desired_state") {
		t.Errorf("second request ends with %+v, want a user text asking for submit_desired_state", last)
	}
}

func TestSystemPromptIsStable(t *testing.T) {
	ra, rb := loadReplay(t, "blockers"), loadReplay(t, "blockers")
	calls := 0
	a := planInput("zz-orders", "An SQS queue named orders.", validator(&calls))
	b := planInput("zz-maindb", "A Postgres 16 database.", validator(&calls))
	b.PreviousBody = "A Postgres 15 database."
	b.Previous = []lock.Resource{{Key: "db.instance", Type: "aws_db_instance", Chosen: map[string]any{"engine": "postgres"}}}
	b.Referenced = []string{"db.instance"}
	b.Region = "eu-west-1"
	for _, c := range []struct {
		r  *Replay
		in engine.PlanInput
	}{{ra, a}, {rb, b}} {
		if _, err := (&LLM{Messages: c.r, Model: DefaultModel}).Plan(context.Background(), c.in); err != nil {
			t.Fatal(err)
		}
	}

	prefix := func(r *Replay) []byte {
		data, err := json.Marshal(map[string]any{"system": r.Requests[0].System, "tools": r.Requests[0].Tools})
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	if pa, pb := prefix(ra), prefix(rb); !bytes.Equal(pa, pb) {
		t.Errorf("system and tools differ between intents:\n%s\n%s", pa, pb)
	}
	sys := ra.Requests[0].System
	if len(sys) == 0 || sys[len(sys)-1].CacheControl.Type != "ephemeral" {
		t.Errorf("system = %+v, want cache_control on its last block", sys)
	}
	for _, s := range []string{"zz-orders", "zz-maindb", "eu-south-1", "eu-west-1", "An SQS queue named orders.", "Postgres 1"} {
		for _, blk := range sys {
			if strings.Contains(blk.Text, s) {
				t.Errorf("system prompt contains input data %q", s)
			}
		}
	}
	// The variable data is in the user message.
	first := messages(t, rb, 0)[0]
	text, _ := first.Content[0]["text"].(string)
	for _, want := range []string{"zz-maindb", "A Postgres 16 database.", "A Postgres 15 database.", "db.instance", "aws_db_instance", "eu-west-1"} {
		if !strings.Contains(text, want) {
			t.Errorf("user message lacks %q:\n%s", want, text)
		}
	}
}

func TestRequestParams(t *testing.T) {
	r := loadReplay(t, "blockers")
	calls := 0
	if _, err := (&LLM{Messages: r, Model: DefaultModel}).Plan(context.Background(), planInput("app", "x", validator(&calls))); err != nil {
		t.Fatal(err)
	}
	p := r.Requests[0]
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Model        string         `json:"model"`
		MaxTokens    int            `json:"max_tokens"`
		Thinking     map[string]any `json:"thinking"`
		OutputConfig map[string]any `json:"output_config"`
		ToolChoice   map[string]any `json:"tool_choice"`
		Fallbacks    any            `json:"fallbacks"`
		Tools        []struct {
			Name        string         `json:"name"`
			InputSchema map[string]any `json:"input_schema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatal(err)
	}
	if wire.Model != "claude-opus-5-5" || wire.MaxTokens != 64000 {
		t.Errorf("model = %q, max_tokens = %d", wire.Model, wire.MaxTokens)
	}
	if wire.Thinking["type"] != "adaptive" {
		t.Errorf("thinking = %v, want adaptive", wire.Thinking)
	}
	if wire.OutputConfig["effort"] != "high" {
		t.Errorf("output_config = %v, want effort high", wire.OutputConfig)
	}
	if wire.ToolChoice["type"] != "auto" {
		t.Errorf("tool_choice = %v, want auto", wire.ToolChoice)
	}
	if wire.Fallbacks != "default" {
		t.Errorf("fallbacks = %v, want \"default\"", wire.Fallbacks)
	}
	found := false
	for _, b := range p.Betas {
		found = found || b == "server-side-fallback-2026-07-01"
	}
	if !found {
		t.Errorf("betas = %v, want server-side-fallback-2026-07-01", p.Betas)
	}
	var names []string
	for _, tl := range wire.Tools {
		names = append(names, tl.Name)
	}
	if got := strings.Join(names, ","); got != "search_resource_types,get_resource_schema,submit_desired_state,submit_changes" {
		t.Fatalf("tools = %s", got)
	}
	if !strings.Contains(string(data), `"name":"submit_desired_state"`) || strings.Contains(string(data), `"strict"`) {
		t.Errorf("tools = %s, want no strict tool", data)
	}
	props, _ := wire.Tools[2].InputSchema["properties"].(map[string]any)
	items, _ := props["resources"].(map[string]any)["items"].(map[string]any)
	config, _ := items["properties"].(map[string]any)["config"].(map[string]any)
	if config["type"] != "object" {
		t.Errorf("submit_desired_state config schema = %v, want a free-form object", config)
	}
}

func TestReplayChecksPrefix(t *testing.T) {
	r := loadReplay(t, "retry_validation")
	p := anthropicParams(systemPrompt, tools(), DefaultModel, DefaultEffort)
	if _, err := r.New(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	other := p
	other.System = []anthropic.BetaTextBlockParam{{Text: "b"}}
	if _, err := r.New(context.Background(), other); err == nil || !strings.Contains(err.Error(), "another system prompt or other tools") {
		t.Errorf("changed system: err = %v", err)
	}
	if _, err := r.New(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if _, err := r.New(context.Background(), p); err == nil {
		t.Error("request beyond the recorded turns: want an error")
	}

	// Every request keeps the messages of the previous one.
	r = loadReplay(t, "retry_validation")
	user := func(s string) anthropic.BetaMessageParam {
		return anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(s))
	}
	p.Messages = []anthropic.BetaMessageParam{user("one")}
	if _, err := r.New(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	p.Messages = []anthropic.BetaMessageParam{user("edited"), user("two")}
	if _, err := r.New(context.Background(), p); err == nil || !strings.Contains(err.Error(), "changes message 1") {
		t.Errorf("edited history: err = %v", err)
	}
	r = loadReplay(t, "retry_validation")
	p.Messages = []anthropic.BetaMessageParam{user("one"), user("two")}
	if _, err := r.New(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	p.Messages = p.Messages[:1]
	if _, err := r.New(context.Background(), p); err == nil || !strings.Contains(err.Error(), "fewer") {
		t.Errorf("dropped message: err = %v", err)
	}
}

// fakeMessages answers from canned responses.
type fakeMessages struct {
	responses []string
	err       error
}

func (f *fakeMessages) New(_ context.Context, _ anthropic.BetaMessageNewParams) (*anthropic.BetaMessage, error) {
	if f.err != nil {
		return nil, f.err
	}
	var m anthropic.BetaMessage
	if err := json.Unmarshal([]byte(f.responses[0]), &m); err != nil {
		return nil, err
	}
	f.responses = f.responses[1:]
	return &m, nil
}

func TestRecorderWritesResponses(t *testing.T) {
	turns := loadReplay(t, "retry_validation").Turns
	if len(turns) != 2 {
		t.Fatalf("%d turns in retry_validation.json, want 2", len(turns))
	}
	path := filepath.Join(t.TempDir(), "rec.json")
	rec := &Recorder{Inner: &fakeMessages{responses: []string{string(turns[0]), string(turns[1])}}, Path: path}
	calls := 0
	if _, err := (&LLM{Messages: rec, Model: DefaultModel}).Plan(context.Background(), planInput("app", "x", validator(&calls, "retention"))); err != nil {
		t.Fatal(err)
	}
	r, err := LoadReplay(path)
	if err != nil {
		t.Fatal(err)
	}
	var header struct {
		PromptVersion string `json:"prompt_version"`
		PromptSHA256  string `json:"prompt_sha256"`
	}
	if data, err := os.ReadFile(path); err != nil || json.Unmarshal(data, &header) != nil {
		t.Fatalf("recording: %v", err)
	}
	if header.PromptVersion != PromptVersion || header.PromptSHA256 != PromptFingerprint() {
		t.Errorf("recording header = %+v, want %s and %s", header, PromptVersion, PromptFingerprint())
	}
	if len(r.Turns) != 2 {
		t.Fatalf("recorded %d turns, want 2", len(r.Turns))
	}
	for i := range turns {
		var want, got any
		if err := json.Unmarshal(turns[i], &want); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(r.Turns[i], &got); err != nil {
			t.Fatalf("recorded turn %d: %v", i, err)
		}
		if !reflect.DeepEqual(want, got) {
			t.Errorf("turn %d recorded as %s", i, r.Turns[i])
		}
	}

	failing := &Recorder{Inner: &fakeMessages{err: errors.New("boom")}, Path: filepath.Join(t.TempDir(), "none.json")}
	if _, err := failing.New(context.Background(), anthropic.BetaMessageNewParams{}); err == nil || err.Error() != "boom" {
		t.Errorf("err = %v, want the inner error", err)
	}
}

func TestNoCredentials(t *testing.T) {
	for _, k := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_PROFILE",
		"ANTHROPIC_FEDERATION_RULE_ID", "ANTHROPIC_ORGANIZATION_ID", "ANTHROPIC_IDENTITY_TOKEN_FILE", "ANTHROPIC_IDENTITY_TOKEN"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	t.Setenv("ANTHROPIC_CONFIG_DIR", t.TempDir())
	// Never reach the network, even if a credential were found.
	t.Setenv("ANTHROPIC_BASE_URL", "http://127.0.0.1:1")
	l, err := NewLLM(DefaultModel)
	if err != nil {
		t.Fatalf("NewLLM without credentials: %v", err)
	}
	calls := 0
	_, err = l.Plan(context.Background(), planInput("app", "x", validator(&calls)))
	if err == nil || !strings.Contains(err.Error(), "no Anthropic credentials: set ANTHROPIC_API_KEY or run `ant auth login`: no Anthropic credentials found.") {
		t.Errorf("err = %v, want the remediation followed by the SDK's diagnosis", err)
	}
}

func TestPlanSkipsEmptyResponse(t *testing.T) {
	r := loadReplay(t, "empty_content")
	calls := 0
	if _, err := (&LLM{Messages: r, Model: DefaultModel}).Plan(context.Background(), planInput("app", "x", validator(&calls))); err != nil {
		t.Fatal(err)
	}
	msgs := messages(t, r, 1)
	if len(msgs) != 2 || msgs[0].Role != "user" || msgs[1].Role != "user" ||
		!strings.Contains(fmt.Sprint(msgs[1].Content[0]["text"]), "submit_desired_state") {
		t.Errorf("second request messages = %+v, want the first user message and the nudge", msgs)
	}
}

func TestParseSubmission(t *testing.T) {
	d, probs := parseSubmission("app", `{"resources": [{"key": "q", "type": "aws_sqs_queue", "config": {"n": 345600, "tags": {"a": "b"}}}], "blockers": ["  ", ""]}`)
	if len(probs) != 0 || len(d.Blockers) != 0 || len(d.Resources) != 1 {
		t.Fatalf("desired = %+v, problems = %q", d, probs)
	}
	if n, ok := d.Resources[0].Config["n"].(json.Number); !ok || n.String() != "345600" {
		t.Errorf("n = %#v, want json.Number", d.Resources[0].Config["n"])
	}
	if d, probs := parseSubmission("app", `{"resources": [], "blockers": [" contradiction "]}`); len(probs) != 0 || len(d.Blockers) != 1 || d.Blockers[0] != "contradiction" {
		t.Errorf("blockers = %q, problems = %q", d.Blockers, probs)
	}
	if _, probs := parseSubmission("app", `{"resources": []}`); len(probs) != 0 {
		t.Errorf("blockers left out: problems = %q", probs)
	}
	for _, c := range []struct{ input, want string }{
		{`{"resources": [], "extra": 1}`, `unknown field "extra"`},
		{`{"resources": [{"key": "q", "type": "t", "config": {}, "id": "x"}]}`, `unknown field "id"`},
		{`{"blockers": []}`, `"resources" is required`},
		{`not json`, "invalid submit_desired_state input"},
		{`{"resources": [{"key": "", "type": "t", "config": {}}]}`, "app/: resource with an empty key"},
		{`{"resources": [{"key": "q", "type": "", "config": {}}]}`, "app/q: resource has no type"},
		{`{"resources": [{"key": "q", "type": "t", "config": "{}"}]}`, "app/q: config must be a JSON object"},
		{`{"resources": [{"key": "q", "type": "t", "config": null}]}`, "app/q: config must be a JSON object"},
		{`{"resources": [{"key": "q", "type": "t"}]}`, "app/q: config must be a JSON object"},
	} {
		_, probs := parseSubmission("app", c.input)
		if len(probs) != 1 || !strings.Contains(probs[0], c.want) {
			t.Errorf("%s: problems = %q, want one containing %q", c.input, probs, c.want)
		}
	}
}

func TestParseSubmissionMessagesHaveNoGoTypes(t *testing.T) {
	for _, c := range []struct{ input, want string }{
		{`{"resources": [{"key": "q", "type": "t", "config": []}]}`, "app/q: config must be a JSON object, got a list"},
		{`{"resources": [{"key": "q", "type": "t", "config": 5}]}`, "app/q: config must be a JSON object, got a number"},
		{`{"resources": "x"}`, "resources must be a list, got a string"},
		{`{"resources": {}}`, "resources must be a list, got an object"},
		{`{"resources": [{"key": "q", "type": "t", "config": {}, "depends_on": "app/x"}]}`, "resources.0.depends_on must be a list, got a string"},
		{`{"resources": [{"key": 3, "type": "t", "config": {}}]}`, "resources.0.key must be a string, got a number"},
	} {
		_, probs := parseSubmission("app", c.input)
		if len(probs) != 1 || !strings.Contains(probs[0], c.want) {
			t.Errorf("%s: problems = %q, want one containing %q", c.input, probs, c.want)
		}
		for _, p := range probs {
			if strings.Contains(p, "Go ") || strings.Contains(p, "map[") || strings.Contains(p, "[]") {
				t.Errorf("%s: %q names a Go type", c.input, p)
			}
		}
	}
}

func TestParseSubmissionDependsOn(t *testing.T) {
	d, probs := parseSubmission("app", `{"resources": [{"key": "svc", "type": "t", "config": {}, "depends_on": ["app/listener", "net/vpc"]}, {"key": "listener", "type": "t", "config": {}}]}`)
	if len(probs) != 0 || len(d.Resources) != 2 {
		t.Fatalf("desired = %+v, problems = %q", d, probs)
	}
	if got := d.Resources[0].DependsOn; !reflect.DeepEqual(got, []string{"app/listener", "net/vpc"}) {
		t.Errorf("depends_on = %q", got)
	}
	if d.Resources[1].DependsOn != nil {
		t.Errorf("depends_on left out = %q, want nil", d.Resources[1].DependsOn)
	}
	if _, probs := parseSubmission("app", `{"resources": [{"key": "q", "type": "t", "config": {}, "depends_on": "app/x"}]}`); len(probs) != 1 || !strings.Contains(probs[0], "invalid submit_desired_state input") {
		t.Errorf("depends_on as a string: problems = %q", probs)
	}
}

func TestParseSubmissionChecks(t *testing.T) {
	d, probs := parseSubmission("app", `{"resources": [], "checks": [
		{"name": "Two replicas.", "type": "resource_attr", "resource": "app/svc", "path": "desired_count", "op": "eq", "value": 2},
		{"name": "Private.", "type": "resource_attr", "resource": "app/db", "path": "publicly_accessible", "op": "eq", "value": false},
		{"name": "Healthy.", "type": "http_get", "url": "https://x.example/health", "status": 200, "within": "60s"},
		{"name": "Cert.", "type": "tls_days", "host": "x.example", "min_days": 20},
		{"name": "Later.", "type": "unverifiable", "reason": "needs the run history"}]}`)
	if len(probs) != 0 {
		t.Fatalf("problems = %q", probs)
	}
	want := []lock.Check{
		{Name: "Two replicas.", Type: "resource_attr", Resource: "app/svc", Path: "desired_count", Op: "eq", Value: json.Number("2")},
		{Name: "Private.", Type: "resource_attr", Resource: "app/db", Path: "publicly_accessible", Op: "eq", Value: false},
		{Name: "Healthy.", Type: "http_get", URL: "https://x.example/health", Status: 200, Within: "60s"},
		{Name: "Cert.", Type: "tls_days", Host: "x.example", MinDays: 20},
		{Name: "Later.", Type: "unverifiable", Reason: "needs the run history"},
	}
	if !reflect.DeepEqual(d.Checks, want) {
		t.Errorf("checks = %#v, want %#v", d.Checks, want)
	}
	for _, c := range []struct{ input, want string }{
		{`{"resources": [], "checks": [{"name": "x", "type": "unverifiable", "why": "y"}]}`, `unknown field "why"`},
		{`{"resources": [], "checks": "x"}`, "checks must be a list, got a string"},
		{`{"resources": [], "checks": [{"name": "x", "type": "tls_days", "min_days": "20"}]}`, "checks.0.min_days must be a number, got a string"},
	} {
		_, probs := parseSubmission("app", c.input)
		if len(probs) != 1 || !strings.Contains(probs[0], c.want) {
			t.Errorf("%s: problems = %q, want one containing %q", c.input, probs, c.want)
		}
	}
}

func TestSubmitToolHasChecks(t *testing.T) {
	data, err := json.Marshal(anthropicTools(tools()))
	if err != nil {
		t.Fatal(err)
	}
	var defs []struct {
		InputSchema struct {
			Properties struct {
				Checks struct {
					Type  string `json:"type"`
					Items struct {
						Properties map[string]struct {
							Enum []string `json:"enum"`
						} `json:"properties"`
						Required             []string `json:"required"`
						AdditionalProperties bool     `json:"additionalProperties"`
					} `json:"items"`
				} `json:"checks"`
			} `json:"properties"`
			Required []string `json:"required"`
		} `json:"input_schema"`
	}
	if err := json.Unmarshal(data, &defs); err != nil {
		t.Fatal(err)
	}
	sub := defs[2].InputSchema
	item := sub.Properties.Checks.Items
	if sub.Properties.Checks.Type != "array" || slices.Contains(sub.Required, "checks") ||
		!slices.Equal(item.Required, []string{"name", "type"}) || item.AdditionalProperties {
		t.Errorf("checks schema = %+v, want an optional list of objects with name and type", sub.Properties.Checks)
	}
	if got := item.Properties["type"].Enum; !slices.Equal(got, []string{"resource_attr", "http_get", "tls_days", "unverifiable"}) {
		t.Errorf("check types = %v", got)
	}
	if got := item.Properties["op"].Enum; !slices.Equal(got, []string{"eq", "ne", "gte", "lte", "contains", "matches", "within", "exists"}) {
		t.Errorf("ops = %v", got)
	}
	for _, f := range []string{"name", "resource", "path", "value", "url", "status", "within", "host", "min_days", "reason"} {
		if _, ok := item.Properties[f]; !ok {
			t.Errorf("checks schema has no %s", f)
		}
	}
}

func TestSubmitToolHasDependsOn(t *testing.T) {
	data, err := json.Marshal(anthropicTools(tools()))
	if err != nil {
		t.Fatal(err)
	}
	var defs []struct {
		Name        string `json:"name"`
		InputSchema struct {
			Properties struct {
				Resources struct {
					Items struct {
						Properties map[string]struct {
							Type  string            `json:"type"`
							Items map[string]string `json:"items"`
						} `json:"properties"`
						Required []string `json:"required"`
					} `json:"items"`
				} `json:"resources"`
			} `json:"properties"`
		} `json:"input_schema"`
	}
	if err := json.Unmarshal(data, &defs); err != nil {
		t.Fatal(err)
	}
	item := defs[2].InputSchema.Properties.Resources.Items
	dep, ok := item.Properties["depends_on"]
	if defs[2].Name != toolSubmit || !ok || dep.Type != "array" || dep.Items["type"] != "string" || slices.Contains(item.Required, "depends_on") {
		t.Errorf("submit resource schema = %+v, want an optional depends_on list of strings", item)
	}
}

func TestUserMessageShowsPreviousDependsOn(t *testing.T) {
	in := planInput("app", "A service after its listener.", nil)
	in.Previous = []lock.Resource{
		{Key: "svc", Type: "t", Chosen: map[string]any{"name": "svc"}, DependsOn: []string{"app/listener"}},
		{Key: "listener", Type: "t", Chosen: map[string]any{"name": "l"}},
	}
	msg, err := userMessage(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg, `"depends_on": [`+"\n"+`      "app/listener"`) || strings.Count(msg, "depends_on") != 1 {
		t.Errorf("user message =\n%s\nwant the depends_on of svc only", msg)
	}
}

func TestLogTurnWithIntent(t *testing.T) {
	var log bytes.Buffer
	l := &LLM{Log: &log, LogIntent: true}
	l.logTurn("app", 2, []string{"get_resource_schema", "submit_desired_state"})
	l.logTurn("app", 3, nil)
	want := "planner app: turn 2, get_resource_schema, submit_desired_state\nplanner app: turn 3, no tool call\n"
	if log.String() != want {
		t.Errorf("log = %q, want %q", log.String(), want)
	}
}

func TestPlanReturnsCanonicalState(t *testing.T) {
	r := loadReplay(t, "submit_valid")
	var seen []engine.Desired
	in := planInput("app", "An SQS queue named orders.", func(_ context.Context, d engine.Desired) error {
		seen = append(seen, d)
		return nil
	})
	in.Canonical = func(d engine.Desired) engine.Desired {
		out := engine.Desired{Blockers: d.Blockers}
		for _, r := range d.Resources {
			cfg := map[string]any{"canonical": true}
			for k, v := range r.Config {
				cfg[k] = v
			}
			out.Resources = append(out.Resources, engine.DesiredResource{Key: r.Key, Type: r.Type, Config: cfg})
		}
		return out
	}
	d, err := (&LLM{Messages: r, Model: DefaultModel}).Plan(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Resources) != 1 || d.Resources[0].Config["canonical"] != true {
		t.Errorf("desired = %+v, want the canonical form", d)
	}
	if len(seen) != 1 || seen[0].Resources[0].Config["canonical"] != true {
		t.Errorf("Validate saw %+v, want the canonical form", seen)
	}
}

func TestSystemPromptRules(t *testing.T) {
	for _, want := range []string{
		"A single or group block is an object.",
		"A list or set block is a list of objects, even when it allows at most one item (max 1).",
		"A map block is an object of objects, by key.",
		"A sensitive or write-only attribute accepts only a whole-value reference, never a literal value or a template.",
		`A reference can also be part of a longer string, a template: "service/${<intent>/<key>.name}/${<intent>/<key>.name}".`,
		"Every reference in a template must be a string, number or bool attribute",
		`No functions or expressions inside "${}".`,
		`depends_on: ["<intent>/<key>", ...]`,
		`Compile every sentence of the intent's "Checks" section (or "Verifiche", its older name) into exactly one check`,
		`its name is the sentence, exactly as written`,
		`If no type can verify a sentence, submit it as unverifiable with the reason`,
		"# Rules",
		"a reviewer checks the desired state against them",
		"naming its ID in its prose",
	} {
		if !strings.Contains(systemPrompt, want) {
			t.Errorf("system prompt lacks %q", want)
		}
	}
	if strings.Contains(systemPrompt, "never part of a longer string") {
		t.Error("system prompt still forbids templates")
	}
	if PromptVersion != "m8-3" {
		t.Errorf("PromptVersion = %q, want m8-3", PromptVersion)
	}
	if !strings.Contains(systemPrompt, "or the rule violations to fix") {
		t.Error("system prompt does not say that <patch_problems> may hold rule violations")
	}
}

func TestSystemPromptAsksForTimeoutsOnSlowResources(t *testing.T) {
	for _, want := range []string{"many minutes", "timeouts block"} {
		if !strings.Contains(systemPrompt, want) {
			t.Errorf("system prompt lacks %q", want)
		}
	}
}

func TestUserMessageShowsOnlySection(t *testing.T) {
	in := planInput("app", "A web app.", nil)
	in.Env = &intent.Env{Name: "staging", Body: "# Staging\n\nTag env=staging.\n\n## database\n- 40 GB.\n\n## app\n- Exposed on staging.example.\n"}
	in.PreviousEnvBody = intent.EnvFor(*in.Env, "app")
	got, err := userMessage(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Tag env=staging.", "## app\n- Exposed on staging.example."} {
		if !strings.Contains(got, want) {
			t.Errorf("message lacks %q:\n%s", want, got)
		}
	}
	for _, not := range []string{"40 GB", "## database", "# Staging"} {
		if strings.Contains(got, not) {
			t.Errorf("message contains %q:\n%s", not, got)
		}
	}
}

func TestUserMessageWithEnv(t *testing.T) {
	in := planInput("app", "An SQS queue named orders.", nil)
	without, err := userMessage(in)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(without, "environment") {
		t.Errorf("message without an environment mentions one:\n%s", without)
	}

	in.Env = &intent.Env{Name: "staging", Body: "# staging\n\nOne small queue.\n"}
	in.PreviousEnvBody = "Two queues."
	got, err := userMessage(in)
	if err != nil {
		t.Fatal(err)
	}
	const rule = "The environment file overrides the intent where they differ. Use the environment name in the names you set for resources that must be unique in an account."
	for _, want := range []string{
		"<environment name=\"staging\">\nOne small queue.\n</environment>",
		rule,
		"<previous_environment>\nTwo queues.\n</previous_environment>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("message lacks %q:\n%s", want, got)
		}
	}
	if i, j := strings.Index(got, "</intent>"), strings.Index(got, "<environment"); i < 0 || j < i {
		t.Errorf("the environment must come after the intent:\n%s", got)
	}

	in.PreviousEnvBody = ""
	got, _ = userMessage(in)
	if !strings.Contains(got, "<previous_environment>none</previous_environment>") {
		t.Errorf("no previous environment body not shown as none:\n%s", got)
	}
}
