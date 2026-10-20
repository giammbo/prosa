package planner

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/giammbo/prosa/internal/engine"
)

// scripted answers with the given raw messages, in order.
type scripted struct {
	raw []string
	n   int
}

func (s *scripted) New(context.Context, anthropic.BetaMessageNewParams) (*anthropic.BetaMessage, error) {
	var m anthropic.BetaMessage
	if err := json.Unmarshal([]byte(s.raw[s.n]), &m); err != nil {
		return nil, err
	}
	s.n++
	return &m, nil
}

const submitBadMsg = `{"id":"m1","type":"message","role":"assistant","model":"claude-opus-5-5","stop_reason":"tool_use",
 "content":[{"type":"tool_use","id":"t1","name":"submit_desired_state","input":{"resources":[{"key":"q","type":"aws_sqs_queue","config":{"retention":1}}],"checks":[]}}],
 "usage":{"input_tokens":100,"output_tokens":10,"cache_read_input_tokens":50,"cache_creation_input_tokens":0}}`

const submitOKMsg = `{"id":"m2","type":"message","role":"assistant","model":"claude-opus-5-5","stop_reason":"tool_use",
 "content":[{"type":"tool_use","id":"t2","name":"submit_desired_state","input":{"resources":[{"key":"q","type":"aws_sqs_queue","config":{"name":"orders"}}],"checks":[]}}],
 "usage":{"input_tokens":200,"output_tokens":20,"cache_read_input_tokens":150,"cache_creation_input_tokens":0,
  "iterations":[{"type":"message","model":"claude-opus-5-5","input_tokens":1,"output_tokens":1,"cache_read_input_tokens":0,"cache_creation_input_tokens":0},
                {"type":"fallback_message","model":"claude-sonnet-5-5","input_tokens":200,"output_tokens":20,"cache_read_input_tokens":150,"cache_creation_input_tokens":0}]}}`

func TestLastResultReportsTurnsProblemsModelAndUsage(t *testing.T) {
	calls := 0
	l := &LLM{Messages: &scripted{raw: []string{submitBadMsg, submitOKMsg}}, Model: DefaultModel}
	if _, err := l.Plan(context.Background(), planInput("app", "An SQS queue named orders.", validator(&calls, "retention"))); err != nil {
		t.Fatal(err)
	}
	want := Result{
		Intent: "app", Job: JobPlan, Backend: "anthropic", Turns: 2, Problems: 1,
		Model: DefaultModel, ServedBy: "claude-sonnet-5-5", Fallback: true,
		Usage: Usage{Input: 300, Output: 30, CacheRead: 200},
	}
	if got := l.LastResult(); !reflect.DeepEqual(got, want) {
		t.Errorf("LastResult = %+v, want %+v", got, want)
	}
}

func TestLastResultWithoutFallbackIsServedByResponseModel(t *testing.T) {
	l := &LLM{Messages: &scripted{raw: []string{submitBadMsg}}, Model: DefaultModel, MaxTurns: 1}
	_, _ = l.Plan(context.Background(), planInput("app", "x", validator(new(int), "retention")))
	got := l.LastResult()
	if got.ServedBy != "claude-opus-5-5" || got.Fallback || got.Turns != 1 || got.Problems != 1 {
		t.Errorf("LastResult = %+v", got)
	}
}

func TestCacheHitReportsCachedResult(t *testing.T) {
	inner := &countPlanner{d: cacheDesired()}
	c := &Cached{Inner: inner, Dir: filepath.Join(t.TempDir(), "planner"), Model: "m"}
	for i := 0; i < 2; i++ {
		if _, err := c.Plan(context.Background(), cacheInput()); err != nil {
			t.Fatal(err)
		}
	}
	got := c.LastResult()
	if !got.Cached || got.Intent != "app" || got.Model != "m" || got.Turns != 0 {
		t.Errorf("after a hit: LastResult = %+v, want cached", got)
	}
}

var _ engine.Planner = (*LLM)(nil)
