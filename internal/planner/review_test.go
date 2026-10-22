package planner

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/giammbo/prosa/internal/engine"
	"github.com/giammbo/prosa/rules"
)

const exceptionText = "The database accepts AWS-NET-001 traffic only from the office range 203.0.113.0/24."

func reviewInput() engine.ReviewInput {
	return engine.ReviewInput{
		Intent: "database",
		Resources: []engine.DesiredResource{
			{Key: "db.instance", Type: "aws_db_instance", Config: map[string]any{"engine": "postgres", "publicly_accessible": false}},
			{Key: "db.sg_ingress", Type: "aws_vpc_security_group_ingress_rule",
				Config:    map[string]any{"cidr_ipv4": "0.0.0.0/0", "from_port": json.Number("5432"), "security_group_id": "${database/db.sg.id}"},
				DependsOn: []string{"network/vpc.main"}},
		},
		Available: []engine.AvailableResource{{Intent: "network", Key: "vpc.main", Type: "aws_vpc"}},
		Rules: []rules.Rule{{ID: "AWS-NET-001", Title: "No ingress from the whole internet",
			AppliesTo: []string{"aws_vpc_security_group_ingress_rule"},
			Body:      "An ingress rule never opens a port to 0.0.0.0/0 or ::/0,\nexcept 80 and 443 on a load balancer."}},
		Exceptions: []rules.Exception{{ID: "AWS-NET-001", Text: exceptionText}},
	}
}

func loadReviewReplay(t *testing.T, name string) *Replay {
	t.Helper()
	r, err := LoadReviewReplay(filepath.Join("testdata", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestReviewerReturnsViolations(t *testing.T) {
	r := loadReviewReplay(t, "review_violation")
	rv := &Reviewer{Backend: Anthropic{Messages: r}, Model: "claude-haiku-5"}
	vs, err := rv.Review(context.Background(), reviewInput())
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != 1 || vs[0].Rule != "AWS-NET-001" || vs[0].Key != "db.sg_ingress" || vs[0].Reason == "" {
		t.Errorf("violations = %+v, want AWS-NET-001 on db.sg_ingress", vs)
	}
	if len(r.Requests) != 1 {
		t.Fatalf("%d requests, want 1", len(r.Requests))
	}
	sys := r.Requests[0].System
	if len(sys) != 1 || sys[0].Text != reviewSystemPrompt {
		t.Errorf("system = %+v, want the reviewer's system prompt", sys)
	}
	if got := r.Requests[0].Tools; len(got) != 1 || got[0].OfTool == nil || got[0].OfTool.Name != toolReview {
		t.Errorf("tools = %+v, want submit_review only", got)
	}
	res := rv.LastResult()
	if res.Intent != "database" || res.Job != JobReview || res.Backend != BackendAnthropic || res.Turns != 1 ||
		res.Model != "claude-haiku-5" || res.ServedBy == "" || res.Usage.Input == 0 || res.Dropped != 0 || res.Cached {
		t.Errorf("LastResult = %+v", res)
	}
}

func TestReviewerDropsUnknown(t *testing.T) {
	r := loadReviewReplay(t, "review_unknown")
	rv := &Reviewer{Backend: Anthropic{Messages: r}, Model: "claude-haiku-5"}
	vs, err := rv.Review(context.Background(), reviewInput())
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != 1 || vs[0].Rule != "AWS-NET-001" || vs[0].Key != "db.sg_ingress" {
		t.Errorf("violations = %+v, want only AWS-NET-001 on db.sg_ingress", vs)
	}
	if got := rv.LastResult().Dropped; got != 2 {
		t.Errorf("Dropped = %d, want 2", got)
	}
}

// reviewScript is a Backend that answers with the given turns, in order, and
// keeps the requests.
type reviewScript struct {
	turns []Turn
	reqs  []TurnRequest
}

func (*reviewScript) Name() string { return "script" }

func (s *reviewScript) Turn(_ context.Context, req TurnRequest) (Turn, error) {
	s.reqs = append(s.reqs, req)
	if len(s.reqs) > len(s.turns) {
		return Turn{}, errors.New("no more turns")
	}
	return s.turns[len(s.reqs)-1], nil
}

func TestReviewerNoSubmit(t *testing.T) {
	talk := Turn{Text: "I think it is fine.", Stop: StopEnd}
	search := Turn{ToolCalls: []ToolCall{{ID: "c", Name: toolSearch, Input: `{"query":"x"}`}}, Stop: StopToolUse}
	bad := Turn{ToolCalls: []ToolCall{{ID: "b", Name: toolReview, Input: `{"violations": "none"}`}}, Stop: StopToolUse}
	b := &reviewScript{turns: []Turn{talk, search, bad, talk, talk}}
	_, err := (&Reviewer{Backend: b, Model: "m"}).Review(context.Background(), reviewInput())
	if err == nil || !strings.Contains(err.Error(), "no review submitted") {
		t.Fatalf("err = %v, want no review submitted", err)
	}
	if len(b.reqs) != 4 {
		t.Errorf("%d requests, want the default 4", len(b.reqs))
	}
	// A turn with no tool call gets a nudge; a wrong tool or a malformed
	// review comes back as an error result.
	last := b.reqs[3].Messages
	if n := len(last); n != 7 || !strings.Contains(last[2].Text, toolReview) || !last[4].Results[0].IsError || !last[6].Results[0].IsError {
		t.Errorf("messages = %+v", last)
	}

	b = &reviewScript{turns: []Turn{talk, talk}}
	if _, err := (&Reviewer{Backend: b, Model: "m", MaxTurns: 2}).Review(context.Background(), reviewInput()); err == nil || len(b.reqs) != 2 {
		t.Errorf("MaxTurns 2: err %v after %d requests", err, len(b.reqs))
	}

	refused := &reviewScript{turns: []Turn{{Stop: StopRefusal, StopDetail: "cyber"}}}
	if _, err := (&Reviewer{Backend: refused, Model: "m"}).Review(context.Background(), reviewInput()); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Errorf("refusal: err = %v", err)
	}
}

func TestReviewerUsesPatchModel(t *testing.T) {
	submit := Turn{ToolCalls: []ToolCall{{ID: "s", Name: toolReview, Input: `{"violations": []}`}}, Stop: StopToolUse, ServedBy: "small"}
	b := &reviewScript{turns: []Turn{submit}}
	vs, err := (&Reviewer{Backend: b, Model: "small"}).Review(context.Background(), reviewInput())
	if err != nil || len(vs) != 0 {
		t.Fatalf("violations %v, err %v; want none", vs, err)
	}
	req := b.reqs[0]
	if req.Model != "small" || req.Effort != "low" || req.System != reviewSystemPrompt || len(req.Tools) != 1 || req.Tools[0].Name != toolReview {
		t.Errorf("request: model %q, effort %q, tools %v", req.Model, req.Effort, req.Tools)
	}
	b = &reviewScript{turns: []Turn{submit}}
	if _, err := (&Reviewer{Backend: b, Model: "small", Effort: "medium"}).Review(context.Background(), reviewInput()); err != nil || b.reqs[0].Effort != "medium" {
		t.Errorf("effort = %q, err %v; want medium", b.reqs[0].Effort, err)
	}
}

func TestReviewMessage(t *testing.T) {
	in := reviewInput()
	msg, err := reviewMessage(in)
	if err != nil {
		t.Fatal(err)
	}
	if first, _, _ := strings.Cut(msg, "\n"); first != `Review the intent "database".` {
		t.Errorf("first line = %q", first)
	}
	for _, want := range []string{
		"<resources>", `"key": "db.sg_ingress"`, `"type": "aws_vpc_security_group_ingress_rule"`, `"cidr_ipv4": "0.0.0.0/0"`, `"network/vpc.main"`,
		"<referable_resources>\n${network/vpc.main.<attribute>} (aws_vpc)\n</referable_resources>",
		`<rule id="AWS-NET-001" title="No ingress from the whole internet">`, "except 80 and 443 on a load balancer.",
		"<exceptions>\nAWS-NET-001: " + exceptionText + "\n</exceptions>",
		toolReview,
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message lacks %q:\n%s", want, msg)
		}
	}
	for _, not := range []string{"<intent", "schema"} {
		if strings.Contains(msg, not) {
			t.Errorf("message contains %q:\n%s", not, msg)
		}
	}

	in.Env, in.Exceptions, in.Available = "staging", nil, nil
	msg, err = reviewMessage(in)
	if err != nil {
		t.Fatal(err)
	}
	if first, _, _ := strings.Cut(msg, "\n"); first != `Review the intent "database" in environment "staging".` {
		t.Errorf("first line = %q", first)
	}
	for _, want := range []string{"<exceptions>none</exceptions>", "<referable_resources>none</referable_resources>"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message lacks %q:\n%s", want, msg)
		}
	}
}

func TestReviewSystemPrompt(t *testing.T) {
	for _, want := range []string{"submit_review", "An exception", "covers what it says and nothing more", "${", "empty list"} {
		if !strings.Contains(reviewSystemPrompt, want) {
			t.Errorf("reviewer system prompt lacks %q", want)
		}
	}
	if ReviewPromptVersion != "r1" || ReviewPromptFingerprint() == PromptFingerprint() || len(ReviewPromptFingerprint()) != 64 {
		t.Errorf("ReviewPromptVersion %q, fingerprint %q", ReviewPromptVersion, ReviewPromptFingerprint())
	}
	// The planner's recordings do not load as the reviewer's, and back.
	if _, err := LoadReviewReplay(filepath.Join("testdata", "submit_valid.json")); err == nil {
		t.Error("a planner recording loaded as a review recording")
	}
	if _, err := LoadReplay(filepath.Join("testdata", "review_violation.json")); err == nil {
		t.Error("a review recording loaded as a planner recording")
	}
}

// A Recorder of a review writes the reviewer's prompt version.
func TestRecorderWritesReviewVersion(t *testing.T) {
	turns := loadReviewReplay(t, "review_violation").Turns
	path := filepath.Join(t.TempDir(), "rec.json")
	rec := &Recorder{Inner: &fakeMessages{responses: []string{string(turns[0])}}, Path: path}
	if _, err := (&Reviewer{Backend: Anthropic{Messages: rec}, Model: "m"}).Review(context.Background(), reviewInput()); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadReviewReplay(path); err != nil {
		t.Errorf("recorded review: %v", err)
	}
}

// countReviewer answers with vs, or err, and counts its calls.
type countReviewer struct {
	calls int
	vs    []engine.Violation
	err   error
}

func (c *countReviewer) Review(context.Context, engine.ReviewInput) ([]engine.Violation, error) {
	c.calls++
	return c.vs, c.err
}

func (c *countReviewer) LastResult() Result {
	return Result{Intent: "database", Job: JobReview, Backend: "script", Turns: 1, Model: "small"}
}

func TestCachedReviewer(t *testing.T) {
	want := []engine.Violation{{Rule: "AWS-NET-001", Key: "db.sg_ingress", Reason: "open to the internet"}}
	inner := &countReviewer{vs: want}
	dir := filepath.Join(t.TempDir(), "planner")
	c := &CachedReviewer{Inner: inner, Dir: dir, Model: "small"}
	for i := range 2 {
		got, err := c.Review(context.Background(), reviewInput())
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("call %d: violations = %+v, want %+v", i+1, got, want)
		}
		if r := c.LastResult(); r.Cached != (i == 1) || r.Job != JobReview || r.Intent != "database" || r.Model != "small" {
			t.Errorf("call %d: LastResult = %+v", i+1, r)
		}
	}
	if inner.calls != 1 {
		t.Errorf("inner called %d times, want 1", inner.calls)
	}
	files, _ := filepath.Glob(filepath.Join(dir, "review-*.json"))
	if len(files) != 1 {
		t.Fatalf("review cache files = %v, want 1", files)
	}
	data, _ := os.ReadFile(files[0])
	if !strings.Contains(string(data), `"violations":[{"rule":"AWS-NET-001","key":"db.sg_ingress","reason":"open to the internet"}]`) {
		t.Errorf("cache file = %s", data)
	}

	// A rule whose body changes (its Hash) misses; so do another model, the
	// exceptions and the environment.
	changed := reviewInput()
	changed.Rules[0].Body += " Not even for a test."
	other := reviewInput()
	other.Exceptions = nil
	staging := reviewInput()
	staging.Env = "staging"
	for _, in := range []engine.ReviewInput{changed, other, staging} {
		if _, err := c.Review(context.Background(), in); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := (&CachedReviewer{Inner: inner, Dir: dir, Model: "big"}).Review(context.Background(), reviewInput()); err != nil {
		t.Fatal(err)
	}
	if inner.calls != 5 {
		t.Errorf("inner called %d times, want 5 (every change misses)", inner.calls)
	}

	// No violations are stored too; errors never.
	empty := &countReviewer{}
	ec := &CachedReviewer{Inner: empty, Dir: t.TempDir(), Model: "small"}
	for range 2 {
		if vs, err := ec.Review(context.Background(), reviewInput()); err != nil || len(vs) != 0 {
			t.Fatalf("violations %v, err %v", vs, err)
		}
	}
	if empty.calls != 1 {
		t.Errorf("empty review: inner called %d times, want 1", empty.calls)
	}
	failing := &countReviewer{err: errors.New("boom")}
	fc := &CachedReviewer{Inner: failing, Dir: t.TempDir(), Model: "small"}
	for range 2 {
		if _, err := fc.Review(context.Background(), reviewInput()); err == nil || err.Error() != "boom" {
			t.Fatalf("err = %v, want boom", err)
		}
	}
	if failing.calls != 2 {
		t.Errorf("failing review: inner called %d times, want 2", failing.calls)
	}
}

func TestCacheKeyRulesHash(t *testing.T) {
	in := cacheInput()
	none, _ := cacheKey(in, "m")
	in.RulesHash = "sha256:aaa"
	a, _ := cacheKey(in, "m")
	in.RulesHash = "sha256:bbb"
	b, _ := cacheKey(in, "m")
	if none == a || a == b {
		t.Errorf("keys %s, %s, %s: want them all different", none, a, b)
	}
}

// A planner cache hit is validated (which reviews) once; a failure of
// Validate other than a ValidationError fails the plan instead of a paid
// re-plan.
func TestCachedHitStillReviewed(t *testing.T) {
	inner := &countPlanner{d: cacheDesired()}
	c := &Cached{Inner: inner, Dir: t.TempDir(), Model: "m"}
	in := cacheInput()
	validated := 0
	var fail error
	in.Validate = func(context.Context, engine.Desired) error {
		validated++
		return fail
	}
	if _, err := c.Plan(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Plan(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if inner.calls != 1 || validated != 1 {
		t.Fatalf("hit: inner %d, validated %d; want 1, 1", inner.calls, validated)
	}
	fail = errors.New("review the rules of intent \"app\": backend down")
	if _, err := c.Plan(context.Background(), in); !errors.Is(err, fail) {
		t.Errorf("err = %v, want the reviewer's failure", err)
	}
	if inner.calls != 1 || validated != 2 {
		t.Errorf("failed review: inner %d, validated %d; want 1, 2", inner.calls, validated)
	}
}

var (
	_ engine.Reviewer = (*Reviewer)(nil)
	_ engine.Reviewer = (*CachedReviewer)(nil)
	_ ResultReporter  = (*Reviewer)(nil)
	_ ResultReporter  = (*CachedReviewer)(nil)
)
