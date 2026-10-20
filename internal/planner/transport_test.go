package planner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/giammbo/prosa/internal/engine"
)

func TestLoadReplayRejectsStaleRecordings(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		t.Helper()
		path := filepath.Join(dir, name+".json")
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	header := func(version, sum string) string {
		return fmt.Sprintf(`{"prompt_version": %q, "prompt_sha256": %q, "turns": []}`, version, sum)
	}
	if _, err := LoadReplay(write("ok", header(PromptVersion, PromptFingerprint()))); err != nil {
		t.Errorf("current recording: %v", err)
	}
	for name, c := range map[string]struct{ content, want string }{
		"old-version": {header("m3-1", PromptFingerprint()), `prompt version "m3-1"`},
		"edited":      {header(PromptVersion, "0123"), "another system prompt or other tools"},
		"no-header":   {`[]`, "old-recording.json"},
	} {
		path := write(name, c.content)
		if name == "no-header" {
			path = write("old-recording", c.content)
		}
		if _, err := LoadReplay(path); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want it to contain %q", name, err, c.want)
		}
	}
	if fp := PromptFingerprint(); len(fp) != 64 {
		t.Errorf("PromptFingerprint() = %q, want a sha256 in hex", fp)
	}
}

// sse renders server-sent events, each a JSON object with its "type".
func sse(events ...string) string {
	var b strings.Builder
	for _, e := range events {
		var head struct{ Type string }
		if err := json.Unmarshal([]byte(e), &head); err != nil {
			panic(err)
		}
		fmt.Fprintf(&b, "event: %s\ndata: %s\n\n", head.Type, e)
	}
	return b.String()
}

// streamServer answers every request with body as an event stream and
// records the request bodies.
func streamServer(t *testing.T, body string) (*httptest.Server, *[]map[string]any) {
	t.Helper()
	var reqs []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		var req map[string]any
		if err := json.Unmarshal(data, &req); err != nil {
			t.Errorf("request body: %v", err)
		}
		reqs = append(reqs, req)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, &reqs
}

func streamEvents(stop string) string {
	return sse(
		`{"type":"message_start","message":{"id":"msg_01","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":120,"output_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"EoSig"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_01","name":"submit_desired_state","input":{}}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"resources\": [{\"key\": \"queue.orders\", "}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"type\": \"aws_sqs_queue\", \"config\": {\"name\": \"orders\"}}]}"}}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"message_delta","delta":{"stop_reason":"`+stop+`","stop_sequence":null},"usage":{"output_tokens":64}}`,
		`{"type":"message_stop"}`,
	)
}

func TestSDKMessagesStreams(t *testing.T) {
	srv, reqs := streamServer(t, streamEvents("tool_use"))
	msgs := newSDKMessages(option.WithBaseURL(srv.URL), option.WithAPIKey("test-key"), option.WithMaxRetries(0))
	calls := 0
	d, err := (&LLM{Messages: msgs, Model: DefaultModel}).Plan(context.Background(), planInput("app", "An SQS queue named orders.", validator(&calls)))
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Resources) != 1 || d.Resources[0].Key != "queue.orders" || d.Resources[0].Config["name"] != "orders" {
		t.Errorf("desired = %+v, want the queue accumulated from the stream", d)
	}
	if len(*reqs) != 1 || (*reqs)[0]["stream"] != true || (*reqs)[0]["max_tokens"] != float64(64000) {
		t.Errorf("requests = %v, want one streamed request with max_tokens 64000", *reqs)
	}

	// The accumulated message keeps its raw JSON, which a Recorder writes.
	m, err := msgs.New(context.Background(), anthropicParams(systemPrompt, tools(), DefaultModel, DefaultEffort))
	if err != nil {
		t.Fatal(err)
	}
	var raw struct {
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type, Signature string
			Input           map[string]any
		} `json:"content"`
	}
	if err := json.Unmarshal([]byte(m.RawJSON()), &raw); err != nil {
		t.Fatal(err)
	}
	if raw.StopReason != "tool_use" || len(raw.Content) != 2 || raw.Content[0].Signature != "EoSig" || raw.Content[1].Input["resources"] == nil {
		t.Errorf("raw message = %s", m.RawJSON())
	}
}

func TestSDKMessagesStreamCutOff(t *testing.T) {
	srv, _ := streamServer(t, streamEvents("max_tokens"))
	msgs := newSDKMessages(option.WithBaseURL(srv.URL), option.WithAPIKey("test-key"), option.WithMaxRetries(0))
	calls := 0
	_, err := (&LLM{Messages: msgs, Model: DefaultModel}).Plan(context.Background(), planInput("app", "x", validator(&calls)))
	if err == nil || !strings.Contains(err.Error(), "response cut off (max_tokens)") {
		t.Errorf("err = %v, want the max_tokens cut-off", err)
	}
	if calls != 0 {
		t.Errorf("a cut-off response was validated (%d calls)", calls)
	}

	empty, _ := streamServer(t, "")
	msgs = newSDKMessages(option.WithBaseURL(empty.URL), option.WithAPIKey("test-key"), option.WithMaxRetries(0))
	if _, err := msgs.New(context.Background(), anthropicParams(systemPrompt, tools(), DefaultModel, DefaultEffort)); err == nil || !strings.Contains(err.Error(), "no message") {
		t.Errorf("empty stream: err = %v, want an error", err)
	}
}

var _ engine.Planner = (*LLM)(nil)
