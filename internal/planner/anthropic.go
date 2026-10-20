package planner

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// Anthropic is the anthropic backend: the beta Messages API through
// Messages, with the prompt cache, adaptive thinking, effort and server-side
// fallbacks (ADR-031). Its conversations are those Replay and Recorder keep.
type Anthropic struct{ Messages Messages }

// Name is "anthropic".
func (Anthropic) Name() string { return BackendAnthropic }

// Turn sends the request with the settings of anthropicParams and the
// conversation, assistant messages as the model returned them.
func (a Anthropic) Turn(ctx context.Context, req TurnRequest) (Turn, error) {
	params := anthropicParams(req.System, req.Tools, req.Model, req.Effort)
	msgs, err := anthropicMessages(req.Messages)
	if err != nil {
		return Turn{}, err
	}
	params.Messages = msgs
	resp, err := a.Messages.New(ctx, params)
	if err != nil {
		return Turn{}, err
	}
	return anthropicTurn(resp), nil
}

// anthropicParams returns the request settings shared by every turn: the
// stable prefix (tools, then system with a cache breakpoint), adaptive
// thinking at the effort given (DefaultEffort when empty), tool_choice auto
// (forced tool use is rejected by the model) and server-side fallbacks on
// refusals. The top-level cache_control caches the growing conversation.
func anthropicParams(system string, tools []ToolDef, model, effort string) anthropic.BetaMessageNewParams {
	if effort == "" {
		effort = DefaultEffort
	}
	return anthropic.BetaMessageNewParams{
		Model:     model,
		MaxTokens: maxTokens,
		System: []anthropic.BetaTextBlockParam{{
			Text:         system,
			CacheControl: anthropic.NewBetaCacheControlEphemeralParam(),
		}},
		Tools:        anthropicTools(tools),
		ToolChoice:   anthropic.BetaToolChoiceUnionParam{OfAuto: &anthropic.BetaToolChoiceAutoParam{}},
		Thinking:     anthropic.BetaThinkingConfigParamUnion{OfAdaptive: &anthropic.BetaThinkingConfigAdaptiveParam{}},
		OutputConfig: anthropic.BetaOutputConfigParam{Effort: anthropic.BetaOutputConfigEffort(effort)},
		Fallbacks:    anthropic.BetaFallbacksParamOfDefault(),
		CacheControl: anthropic.NewBetaCacheControlEphemeralParam(),
		Betas:        []anthropic.AnthropicBeta{anthropic.AnthropicBetaServerSideFallback2026_07_01},
	}
}

// anthropicTools renders the tools as the SDK's: the schema's properties
// and required, its other keywords (but type, always object) as extra fields.
func anthropicTools(tools []ToolDef) []anthropic.BetaToolUnionParam {
	out := make([]anthropic.BetaToolUnionParam, len(tools))
	for i, t := range tools {
		schema := anthropic.BetaToolInputSchemaParam{Properties: t.Schema["properties"]}
		if req, ok := t.Schema["required"].([]string); ok {
			schema.Required = req
		}
		for k, v := range t.Schema {
			if k == "type" || k == "properties" || k == "required" {
				continue
			}
			if schema.ExtraFields == nil {
				schema.ExtraFields = map[string]any{}
			}
			schema.ExtraFields[k] = v
		}
		out[i] = anthropic.BetaToolUnionParam{OfTool: &anthropic.BetaToolParam{
			Name: t.Name, Description: anthropic.String(t.Description), InputSchema: schema,
		}}
	}
	return out
}

// anthropicMessages renders the conversation: an assistant message is the
// one the model returned (native), thinking blocks included, which prompt
// caching and preserved thinking need unchanged.
func anthropicMessages(msgs []Message) ([]anthropic.BetaMessageParam, error) {
	out := make([]anthropic.BetaMessageParam, 0, len(msgs))
	for _, m := range msgs {
		switch {
		case m.Role == "assistant":
			p, ok := m.native.(anthropic.BetaMessageParam)
			if !ok {
				return nil, errors.New("anthropic: an assistant message that this backend did not return")
			}
			out = append(out, p)
		case len(m.Results) > 0:
			blocks := make([]anthropic.BetaContentBlockParamUnion, len(m.Results))
			for i, r := range m.Results {
				blocks[i] = anthropic.NewBetaToolResultBlock(r.ID, r.Content, r.IsError)
			}
			out = append(out, anthropic.NewBetaUserMessage(blocks...))
		default:
			out = append(out, anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(m.Text)))
		}
	}
	return out, nil
}

// anthropicTurn reads a response: its text and tool calls, its usage, its
// stop reason, and which model served it: the model of the response, or of
// the `fallback_message` entry of usage.iterations when a server-side
// fallback served it.
func anthropicTurn(resp *anthropic.BetaMessage) Turn {
	t := Turn{
		Usage: Usage{
			Input:         resp.Usage.InputTokens,
			Output:        resp.Usage.OutputTokens,
			CacheRead:     resp.Usage.CacheReadInputTokens,
			CacheCreation: resp.Usage.CacheCreationInputTokens,
		},
		ServedBy: string(resp.Model),
	}
	for _, it := range resp.Usage.Iterations {
		if fb, ok := it.AsAny().(anthropic.BetaFallbackMessageIterationUsage); ok {
			t.Fallback, t.ServedBy = true, string(fb.Model)
		}
	}
	var text []string
	for _, block := range resp.Content {
		switch b := block.AsAny().(type) {
		case anthropic.BetaTextBlock:
			text = append(text, b.Text)
		case anthropic.BetaToolUseBlock:
			t.ToolCalls = append(t.ToolCalls, ToolCall{ID: b.ID, Name: b.Name, Input: b.JSON.Input.Raw()})
		}
	}
	t.Text = strings.Join(text, "\n")
	if len(resp.Content) > 0 {
		t.native = resp.ToParam()
	}
	switch resp.StopReason {
	case anthropic.BetaStopReasonToolUse:
		t.Stop = StopToolUse
	case anthropic.BetaStopReasonRefusal:
		cat := string(resp.StopDetails.Category)
		if cat == "" {
			cat = "none"
		}
		t.Stop, t.StopDetail = StopRefusal, "category "+cat
		if resp.StopDetails.Explanation != "" {
			t.StopDetail += ": " + resp.StopDetails.Explanation
		}
	case anthropic.BetaStopReasonMaxTokens, anthropic.BetaStopReasonModelContextWindowExceeded:
		t.Stop, t.StopDetail = StopMaxTokens, string(resp.StopReason)
	default:
		t.Stop = StopEnd
	}
	return t
}

// anthropicCredentials reports whether the SDK's default chain has something
// to try, as far as getenv shows: an API key or token, a profile or
// federation variable, or a profile written by `ant auth login` in the
// config directory (ANTHROPIC_CONFIG_DIR, else $XDG_CONFIG_HOME/anthropic,
// else $HOME/.config/anthropic).
func anthropicCredentials(getenv func(string) string) bool {
	for _, k := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_PROFILE", "ANTHROPIC_FEDERATION_RULE_ID"} {
		if getenv(k) != "" {
			return true
		}
	}
	dir := getenv("ANTHROPIC_CONFIG_DIR")
	switch {
	case dir != "":
	case getenv("XDG_CONFIG_HOME") != "":
		dir = filepath.Join(getenv("XDG_CONFIG_HOME"), "anthropic")
	case getenv("HOME") != "":
		dir = filepath.Join(getenv("HOME"), ".config", "anthropic")
	default:
		return false
	}
	profiles, _ := filepath.Glob(filepath.Join(dir, "configs", "*.json"))
	return len(profiles) > 0
}

// sdkMessages calls the beta Messages API (needed for server-side
// fallbacks), streaming, with the SDK's default credentials.
type sdkMessages struct{ svc *anthropic.BetaMessageService }

// newSDKMessages returns the SDK implementation of Messages on a client
// built with opts (none in production: default credentials and endpoint).
func newSDKMessages(opts ...option.RequestOption) sdkMessages {
	client := anthropic.NewClient(opts...)
	return sdkMessages{&client.Beta.Messages}
}

// errNoCredentials is the SDK's message when its default credential chain
// found nothing; the SDK's sentinel for it is internal.
const errNoCredentials = "no Anthropic credentials found"

// credentialsError is the missing-credentials failure: a short remediation,
// then the SDK's diagnosis.
type credentialsError struct{ err error }

func (e credentialsError) Error() string {
	return "no Anthropic credentials: set ANTHROPIC_API_KEY or run `ant auth login`: " + e.err.Error()
}

func (e credentialsError) Unwrap() error { return e.err }

// New streams the response and returns the message accumulated from its
// events.
func (s sdkMessages) New(ctx context.Context, params anthropic.BetaMessageNewParams) (*anthropic.BetaMessage, error) {
	stream := s.svc.NewStreaming(ctx, params)
	defer stream.Close()
	var m anthropic.BetaMessage
	for stream.Next() {
		if err := m.Accumulate(stream.Current()); err != nil {
			return nil, fmt.Errorf("stream: %w", err)
		}
	}
	if err := stream.Err(); err != nil {
		if strings.Contains(err.Error(), errNoCredentials) {
			return nil, credentialsError{err}
		}
		return nil, err
	}
	if m.ID == "" {
		return nil, errors.New("stream: the response ended with no message")
	}
	return &m, nil
}
