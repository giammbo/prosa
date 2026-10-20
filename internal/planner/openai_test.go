package planner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// oaServer is a Chat Completions server: it answers each request with the
// status and body that answer returns, and keeps the requests.
type oaServer struct {
	*httptest.Server
	mu     sync.Mutex
	bodies []map[string]any
	auth   []string
	paths  []string
}

func newOAServer(t *testing.T, answer func(n int, body map[string]any) (int, string)) *oaServer {
	t.Helper()
	s := &oaServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		var body map[string]any
		if err := json.Unmarshal(data, &body); err != nil {
			t.Errorf("request body is not JSON: %s", data)
		}
		s.mu.Lock()
		n := len(s.bodies)
		s.bodies = append(s.bodies, body)
		s.auth = append(s.auth, r.Header.Get("Authorization"))
		s.paths = append(s.paths, r.Method+" "+r.URL.Path)
		s.mu.Unlock()
		status, out := answer(n, body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, out)
	}))
	t.Cleanup(s.Close)
	return s
}

// scriptedOA answers with responses in order, then with an error.
func scriptedOA(responses ...string) func(int, map[string]any) (int, string) {
	return func(n int, _ map[string]any) (int, string) {
		if n >= len(responses) {
			return http.StatusInternalServerError, `{"error":{"message":"no more responses"}}`
		}
		return http.StatusOK, responses[n]
	}
}

// oaCall is a response with one tool call; args is the arguments string as
// the model wrote it (not necessarily valid JSON).
func oaCall(id, name, args string) string {
	a, _ := json.Marshal(args)
	return fmt.Sprintf(`{"id":"chatcmpl-%s","object":"chat.completion","model":"llama-served",
 "choices":[{"index":0,"message":{"role":"assistant","content":null,
  "tool_calls":[{"id":%q,"type":"function","function":{"name":%q,"arguments":%s}}]},"finish_reason":"tool_calls"}],
 "usage":{"prompt_tokens":100,"completion_tokens":5,"total_tokens":105,"prompt_tokens_details":{"cached_tokens":40}}}`, id, id, name, a)
}

// oaText is a response with text and no tool call.
func oaText(text string) string {
	t, _ := json.Marshal(text)
	return fmt.Sprintf(`{"id":"chatcmpl-t","object":"chat.completion","model":"llama-served",
 "choices":[{"index":0,"message":{"role":"assistant","content":%s},"finish_reason":"stop"}],
 "usage":{"prompt_tokens":10,"completion_tokens":3,"total_tokens":13}}`, t)
}

// sentMessages returns the messages of a request body.
func sentMessages(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	raw, _ := body["messages"].([]any)
	out := make([]map[string]any, len(raw))
	for i, m := range raw {
		out[i], _ = m.(map[string]any)
	}
	return out
}

func TestOpenAIToolLoop(t *testing.T) {
	srv := newOAServer(t, scriptedOA(
		oaCall("c1", "search_resource_types", `{"query":"sqs queue"}`),
		oaCall("c2", "get_resource_schema", `{"type":"aws_sqs_queue"}`),
		oaCall("c3", "submit_changes", `{"changes":[{"op":"set","key":"queue.orders","path":"message_retention_seconds","value":60}]}`),
	))
	l := &LLM{Backend: &OpenAI{BaseURL: srv.URL + "/v1/"}, Model: "llama-test"}
	calls := 0
	d, err := l.Plan(context.Background(), patchInput(validator(&calls)))
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Resources) != 1 || fmt.Sprint(d.Resources[0].Config["message_retention_seconds"]) != "60" {
		t.Fatalf("desired = %+v, want the base with the retention set", d.Resources)
	}
	if len(srv.bodies) != 3 {
		t.Fatalf("%d requests, want 3", len(srv.bodies))
	}
	for i := range srv.bodies {
		if srv.paths[i] != "POST /v1/chat/completions" || srv.auth[i] != "" {
			t.Errorf("request %d: %s, Authorization %q; want POST /v1/chat/completions and no Authorization", i+1, srv.paths[i], srv.auth[i])
		}
	}
	first := srv.bodies[0]
	if first["model"] != "llama-test" || first["tool_choice"] != "auto" || first["reasoning_effort"] != "low" {
		t.Errorf("request: model %v, tool_choice %v, reasoning_effort %v; want llama-test, auto, low (patch job)", first["model"], first["tool_choice"], first["reasoning_effort"])
	}
	tools, _ := first["tools"].([]any)
	var names []string
	for _, tl := range tools {
		m, _ := tl.(map[string]any)
		fn, _ := m["function"].(map[string]any)
		params, _ := fn["parameters"].(map[string]any)
		if m["type"] != "function" || params["type"] != "object" || fn["description"] == "" {
			t.Errorf("tool = %v, want a function with a description and an object schema", m)
		}
		names = append(names, fmt.Sprint(fn["name"]))
	}
	if got := strings.Join(names, ","); got != "search_resource_types,get_resource_schema,submit_desired_state,submit_changes" {
		t.Errorf("tools = %s", got)
	}
	msgs := sentMessages(t, first)
	if len(msgs) != 2 || msgs[0]["role"] != "system" || msgs[0]["content"] != systemPrompt || msgs[1]["role"] != "user" ||
		!strings.Contains(fmt.Sprint(msgs[1]["content"]), "queue.orders") {
		t.Errorf("first request messages = %v, want the system prompt and the user message", msgs)
	}

	// The third request carries the whole conversation: each tool call and
	// its result, by id.
	msgs = sentMessages(t, srv.bodies[2])
	if len(msgs) != 6 {
		t.Fatalf("third request has %d messages, want 6: %v", len(msgs), msgs)
	}
	for i, id := range []string{"c1", "c2"} {
		asst, tool := msgs[2+2*i], msgs[3+2*i]
		tcs, _ := asst["tool_calls"].([]any)
		if asst["role"] != "assistant" || len(tcs) != 1 || tcs[0].(map[string]any)["id"] != id {
			t.Errorf("message %d = %v, want the assistant's call %s", 3+2*i, asst, id)
		}
		if tool["role"] != "tool" || tool["tool_call_id"] != id {
			t.Errorf("message %d = %v, want the result of %s", 4+2*i, tool, id)
		}
	}
	if !strings.Contains(fmt.Sprint(msgs[3]["content"]), "aws_sqs_queue_policy") || !strings.Contains(fmt.Sprint(msgs[5]["content"]), "resource aws_sqs_queue") {
		t.Errorf("tool results = %q, %q", msgs[3]["content"], msgs[5]["content"])
	}

	got := l.LastResult()
	want := Result{Intent: "app", Turns: 3, Model: "llama-test", ServedBy: "llama-served", Job: JobPatch, Backend: "openai",
		Usage: Usage{Input: 180, Output: 15, CacheRead: 120}}
	if got != want {
		t.Errorf("result = %+v, want %+v", got, want)
	}

	// With a key, every request carries it as a bearer token.
	keyed := newOAServer(t, scriptedOA(oaCall("c1", "submit_desired_state", `{"resources":[]}`)))
	l = &LLM{Backend: &OpenAI{BaseURL: keyed.URL, APIKey: "sk-test"}, Model: "m"}
	if _, err := l.Plan(context.Background(), planInput("app", "nothing", validator(new(int)))); err != nil {
		t.Fatal(err)
	}
	if keyed.auth[0] != "Bearer sk-test" || keyed.bodies[0]["reasoning_effort"] != "high" {
		t.Errorf("Authorization %q, reasoning_effort %v; want the bearer key and high (plan job)", keyed.auth[0], keyed.bodies[0]["reasoning_effort"])
	}
}

// A turn without a tool call gets the same nudge as on Anthropic; a model
// that never calls one fails after DefaultMaxTurns, naming backend and model.
func TestOpenAINoToolCall(t *testing.T) {
	srv := newOAServer(t, scriptedOA(
		oaText("I would create a queue."),
		oaCall("c1", "submit_desired_state", `{"resources":[{"key":"q","type":"aws_sqs_queue","config":{"name":"orders"}}]}`),
	))
	calls := 0
	d, err := (&LLM{Backend: &OpenAI{BaseURL: srv.URL}, Model: "m"}).Plan(context.Background(), planInput("app", "x", validator(&calls)))
	if err != nil || len(d.Resources) != 1 {
		t.Fatalf("desired %+v, err %v", d, err)
	}
	msgs := sentMessages(t, srv.bodies[1])
	if len(msgs) != 4 || msgs[2]["role"] != "assistant" || msgs[2]["content"] != "I would create a queue." ||
		msgs[3]["role"] != "user" || msgs[3]["content"] != nudge {
		t.Errorf("second request messages = %v, want the assistant's text and the nudge", msgs)
	}

	silent := newOAServer(t, func(int, map[string]any) (int, string) { return http.StatusOK, oaText("thinking...") })
	_, err = (&LLM{Backend: &OpenAI{BaseURL: silent.URL}, Model: "qwen-local"}).Plan(context.Background(), planInput("app", "x", validator(&calls)))
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("no desired state submitted after %d turns", DefaultMaxTurns)) ||
		!strings.Contains(err.Error(), "backend openai") || !strings.Contains(err.Error(), "model qwen-local") {
		t.Errorf("err = %v, want the turn limit with backend and model", err)
	}
	if len(silent.bodies) != DefaultMaxTurns {
		t.Errorf("%d requests, want %d", len(silent.bodies), DefaultMaxTurns)
	}
}

// Arguments that are not valid JSON come back as the same parsing problem
// as on Anthropic, and are sent back as the model wrote them.
func TestOpenAIBadArguments(t *testing.T) {
	const bad = `{"resources": [{"key": "q",`
	srv := newOAServer(t, scriptedOA(
		oaCall("c1", "submit_desired_state", bad),
		oaCall("c2", "submit_desired_state", `{"resources":[{"key":"q","type":"aws_sqs_queue","config":{"name":"orders"}}]}`),
	))
	calls := 0
	l := &LLM{Backend: &OpenAI{BaseURL: srv.URL}, Model: "m"}
	if _, err := l.Plan(context.Background(), planInput("app", "x", validator(&calls))); err != nil {
		t.Fatal(err)
	}
	msgs := sentMessages(t, srv.bodies[1])
	if len(msgs) != 4 {
		t.Fatalf("second request messages = %v", msgs)
	}
	tc := msgs[2]["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if tc["arguments"] != bad {
		t.Errorf("arguments sent back = %q, want %q", tc["arguments"], bad)
	}
	if msgs[3]["role"] != "tool" || msgs[3]["tool_call_id"] != "c1" || !strings.Contains(fmt.Sprint(msgs[3]["content"]), "invalid submit_desired_state input") {
		t.Errorf("tool result = %v, want the parsing problem", msgs[3])
	}
	if r := l.LastResult(); r.Problems != 1 || r.Turns != 2 {
		t.Errorf("result = %+v, want 1 problem in 2 turns", r)
	}

	always := newOAServer(t, func(int, map[string]any) (int, string) {
		return http.StatusOK, oaCall("c", "submit_changes", "not json")
	})
	_, err := (&LLM{Backend: &OpenAI{BaseURL: always.URL}, Model: "m-x"}).Plan(context.Background(), patchInput(validator(&calls)))
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("no valid desired state after %d turns", DefaultMaxTurns)) ||
		!strings.Contains(err.Error(), "invalid submit_changes input") || !strings.Contains(err.Error(), "backend openai, model m-x") {
		t.Errorf("err = %v, want the turn limit with the last problem, backend and model", err)
	}
}

// A server that rejects reasoning_effort is asked again without it, once,
// and never sent it again; another 400 is an error.
func TestOpenAIEffortRetry(t *testing.T) {
	srv := newOAServer(t, func(n int, body map[string]any) (int, string) {
		if _, ok := body["reasoning_effort"]; ok {
			return http.StatusBadRequest, `{"error":{"message":"Unrecognized request argument supplied: reasoning_effort","type":"invalid_request_error"}}`
		}
		if n < 2 {
			return http.StatusOK, oaCall("c1", "search_resource_types", `{"query":"queue"}`)
		}
		return http.StatusOK, oaCall("c2", "submit_desired_state", `{"resources":[]}`)
	})
	if _, err := (&LLM{Backend: &OpenAI{BaseURL: srv.URL}, Model: "m"}).Plan(context.Background(), planInput("app", "x", validator(new(int)))); err != nil {
		t.Fatal(err)
	}
	var sent []bool
	for _, b := range srv.bodies {
		_, ok := b["reasoning_effort"]
		sent = append(sent, ok)
	}
	if fmt.Sprint(sent) != "[true false false]" {
		t.Errorf("reasoning_effort sent = %v, want [true false false]", sent)
	}

	other := newOAServer(t, func(int, map[string]any) (int, string) {
		return http.StatusBadRequest, `{"error":{"message":"model not found"}}`
	})
	_, err := (&LLM{Backend: &OpenAI{BaseURL: other.URL}, Model: "nope"}).Plan(context.Background(), planInput("app", "x", validator(new(int))))
	if err == nil || !strings.Contains(err.Error(), "status 400") || !strings.Contains(err.Error(), "model not found") ||
		!strings.Contains(err.Error(), "backend openai, model nope") || len(other.bodies) != 1 {
		t.Errorf("err = %v after %d requests, want one failed request naming status, body, backend and model", err, len(other.bodies))
	}
}

// Without a client of its own the openai backend uses one with a timeout,
// never http.DefaultClient, which has none: a server that hangs ends the run.
func TestOpenAIClientTimeout(t *testing.T) {
	c := (&OpenAI{}).client()
	if c == http.DefaultClient || c.Timeout != openAITimeout || openAITimeout <= 0 {
		t.Fatalf("client = %p (default %p), timeout %v; want its own with %v", c, http.DefaultClient, c.Timeout, openAITimeout)
	}
	own := &http.Client{}
	if got := (&OpenAI{HTTP: own}).client(); got != own {
		t.Fatal("the given client is not used")
	}
}
