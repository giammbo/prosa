package planner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// OpenAI is the openai backend: the Chat Completions API with tool calling,
// as OpenAI and compatible servers (Ollama, vLLM, llama.cpp server, LM
// Studio) offer it, at BaseURL (".../v1"). APIKey, when set, is sent as a
// bearer token. No streaming; the prompt cache is the provider's, and there
// are no fallbacks (ADR-031). HTTP is the client; when nil, one with
// openAITimeout.
type OpenAI struct {
	BaseURL string
	APIKey  string
	HTTP    *http.Client

	mu       sync.Mutex
	noEffort map[string]bool // models whose server rejected reasoning_effort: it is not sent again
}

// Name is "openai".
func (*OpenAI) Name() string { return BackendOpenAI }

// openAITimeout bounds one request of the openai backend when it has no
// client of its own: a local model can take minutes for an answer, a server
// that hangs must not hold the run forever.
const openAITimeout = 10 * time.Minute

// defaultOpenAIClient is the client of a backend without one.
var defaultOpenAIClient = &http.Client{Timeout: openAITimeout}

// client returns HTTP, or defaultOpenAIClient when nil.
func (o *OpenAI) client() *http.Client {
	if o.HTTP != nil {
		return o.HTTP
	}
	return defaultOpenAIClient
}

// errBodyLimit is how much of an error response an error quotes.
const errBodyLimit = 512

type oaRequest struct {
	Model           string      `json:"model"`
	Messages        []oaMessage `json:"messages"`
	Tools           []oaTool    `json:"tools"`
	ToolChoice      string      `json:"tool_choice"`
	ReasoningEffort string      `json:"reasoning_effort,omitempty"`
}

type oaMessage struct {
	Role       string       `json:"role"`
	Content    *string      `json:"content"`
	ToolCalls  []oaToolCall `json:"tool_calls,omitempty"`
	ToolCallID string       `json:"tool_call_id,omitempty"`
}

type oaToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type oaTool struct {
	Type     string     `json:"type"`
	Function oaFunction `json:"function"`
}

type oaFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

type oaResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Content   json.RawMessage `json:"content"`
			Refusal   string          `json:"refusal"`
			ToolCalls []oaToolCall    `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens        int64 `json:"prompt_tokens"`
		CompletionTokens    int64 `json:"completion_tokens"`
		PromptTokensDetails struct {
			CachedTokens int64 `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
}

// Turn posts the request to {BaseURL}/chat/completions, with tool_choice
// auto and the effort as reasoning_effort; a 400 that names
// reasoning_effort (a model without it) is retried once without, and the
// field is not sent again for that model.
func (o *OpenAI) Turn(ctx context.Context, req TurnRequest) (Turn, error) {
	body := oaRequest{Model: req.Model, Messages: oaMessages(req.System, req.Messages), Tools: oaTools(req.Tools), ToolChoice: "auto"}
	o.mu.Lock()
	if !o.noEffort[req.Model] {
		body.ReasoningEffort = req.Effort
	}
	o.mu.Unlock()
	status, data, err := o.post(ctx, body)
	if err == nil && status == http.StatusBadRequest && body.ReasoningEffort != "" && bytes.Contains(data, []byte("reasoning_effort")) {
		o.mu.Lock()
		if o.noEffort == nil {
			o.noEffort = map[string]bool{}
		}
		o.noEffort[req.Model] = true
		o.mu.Unlock()
		body.ReasoningEffort = ""
		status, data, err = o.post(ctx, body)
	}
	if err != nil {
		return Turn{}, err
	}
	if status/100 != 2 {
		return Turn{}, fmt.Errorf("openai: status %d: %s", status, excerpt(data))
	}
	var resp oaResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return Turn{}, fmt.Errorf("openai: the response is not a chat completion: %w", err)
	}
	if len(resp.Choices) == 0 {
		return Turn{}, errors.New("openai: the response has no choices")
	}
	return oaTurn(resp, req.Model), nil
}

// post sends body and returns the status and the body of the response.
func (o *OpenAI) post(ctx context.Context, body oaRequest) (int, []byte, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return 0, nil, err
	}
	url := strings.TrimRight(o.BaseURL, "/") + "/chat/completions"
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return 0, nil, fmt.Errorf("openai: %w", err)
	}
	hr.Header.Set("Content-Type", "application/json")
	if o.APIKey != "" {
		hr.Header.Set("Authorization", "Bearer "+o.APIKey)
	}
	resp, err := o.client().Do(hr)
	if err != nil {
		return 0, nil, fmt.Errorf("openai: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, fmt.Errorf("openai: reading the response: %w", err)
	}
	return resp.StatusCode, data, nil
}

// excerpt is the start of an error body, on one line.
func excerpt(data []byte) string {
	s := strings.Join(strings.Fields(string(data)), " ")
	if len(s) > errBodyLimit {
		s = s[:errBodyLimit] + "..."
	}
	if s == "" {
		s = "(empty body)"
	}
	return s
}

// oaTools renders the tools as functions.
func oaTools(tools []ToolDef) []oaTool {
	out := make([]oaTool, len(tools))
	for i, t := range tools {
		out[i] = oaTool{Type: "function", Function: oaFunction{Name: t.Name, Description: t.Description, Parameters: t.Schema}}
	}
	return out
}

// oaMessages renders the conversation after a system message. Tool results
// are tool messages, one per call; one that is an error starts with
// "error: ", the API having no flag for it. Arguments go back as the model
// wrote them, valid JSON or not.
func oaMessages(system string, msgs []Message) []oaMessage {
	str := func(s string) *string { return &s }
	out := []oaMessage{{Role: "system", Content: str(system)}}
	for _, m := range msgs {
		switch {
		case m.Role == "assistant":
			am := oaMessage{Role: "assistant"}
			if m.Text != "" {
				am.Content = str(m.Text)
			}
			for _, c := range m.ToolCalls {
				tc := oaToolCall{ID: c.ID, Type: "function"}
				tc.Function.Name, tc.Function.Arguments = c.Name, c.Input
				am.ToolCalls = append(am.ToolCalls, tc)
			}
			out = append(out, am)
		case len(m.Results) > 0:
			for _, r := range m.Results {
				content := r.Content
				if r.IsError {
					content = "error: " + content
				}
				out = append(out, oaMessage{Role: "tool", ToolCallID: r.ID, Content: str(content)})
			}
		default:
			out = append(out, oaMessage{Role: "user", Content: str(m.Text)})
		}
	}
	return out
}

// oaTurn reads the first choice of a response. Cached prompt tokens count as
// cache reads and not as input, as on Anthropic. A tool call without an id
// (some local servers) gets one.
func oaTurn(resp oaResponse, model string) Turn {
	c := resp.Choices[0]
	t := Turn{
		Text: oaContent(c.Message.Content),
		Usage: Usage{
			Input:     resp.Usage.PromptTokens - resp.Usage.PromptTokensDetails.CachedTokens,
			Output:    resp.Usage.CompletionTokens,
			CacheRead: resp.Usage.PromptTokensDetails.CachedTokens,
		},
		ServedBy: resp.Model,
	}
	if t.ServedBy == "" {
		t.ServedBy = model
	}
	for i, tc := range c.Message.ToolCalls {
		id := tc.ID
		if id == "" {
			id = fmt.Sprintf("call_%d", i)
		}
		t.ToolCalls = append(t.ToolCalls, ToolCall{ID: id, Name: tc.Function.Name, Input: tc.Function.Arguments})
	}
	switch {
	case c.Message.Refusal != "":
		t.Stop, t.StopDetail = StopRefusal, c.Message.Refusal
	case c.FinishReason == "content_filter":
		t.Stop, t.StopDetail = StopRefusal, c.FinishReason
	case c.FinishReason == "length":
		t.Stop, t.StopDetail = StopMaxTokens, c.FinishReason
	case len(t.ToolCalls) > 0:
		t.Stop = StopToolUse
	default:
		t.Stop = StopEnd
	}
	return t
}

// oaContent reads a message content: a string, null, or a list of parts
// whose text is joined.
func oaContent(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(p.Text)
	}
	return b.String()
}
