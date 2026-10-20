package planner

import (
	"context"
	"errors"
	"fmt"
)

// Stop reasons of a Turn.
const (
	StopToolUse   = "tool_use"   // the model called tools
	StopEnd       = "end"        // the model ended its turn
	StopMaxTokens = "max_tokens" // the output was cut off
	StopRefusal   = "refusal"    // the model refused
)

// ToolCall is a call of a tool in a turn. Input is its arguments as the model
// wrote them, raw JSON text that may not be valid: the tools check it.
type ToolCall struct {
	ID, Name, Input string
}

// ToolResult answers the ToolCall with the same ID.
type ToolResult struct {
	ID, Content string
	IsError     bool
}

// ToolDef is a tool as the backends send it: Schema is the JSON Schema of
// its input, an object.
type ToolDef struct {
	Name, Description string
	Schema            map[string]any
}

// Message is a message of the conversation. A user message has Text (the
// request, or the nudge) or Results; an assistant message has Text and
// ToolCalls, and native, the backend's own form of it (Anthropic's keeps the
// thinking blocks, which the next request must carry unchanged).
type Message struct {
	Role      string // "user" or "assistant"
	Text      string
	ToolCalls []ToolCall
	Results   []ToolResult
	native    any
}

// TurnRequest is one request of the tool-use loop: the system prompt, the
// tools, the conversation so far, and the model and effort of the job.
type TurnRequest struct {
	System   string
	Tools    []ToolDef
	Messages []Message
	Model    string
	Effort   string // low, medium or high
}

// Turn is the answer to a TurnRequest. StopDetail says more about a refusal
// or a cut-off, in the provider's words; ServedBy is the model that answered
// (Fallback: a server-side fallback did).
type Turn struct {
	Text       string
	ToolCalls  []ToolCall
	Usage      Usage
	Stop       string
	StopDetail string
	ServedBy   string
	Fallback   bool
	native     any
}

// message returns the turn as the assistant message to keep in the
// conversation; false when the turn has nothing to keep.
func (t Turn) message() (Message, bool) {
	if t.native == nil && t.Text == "" && len(t.ToolCalls) == 0 {
		return Message{}, false
	}
	return Message{Role: "assistant", Text: t.Text, ToolCalls: t.ToolCalls, native: t.native}, true
}

// Backend is a provider of models with tool calling, one turn at a time
// (ADR-031): the tool-use loop of LLM sits above it.
type Backend interface {
	Name() string
	Turn(ctx context.Context, req TurnRequest) (Turn, error)
}

// Backend names.
const (
	BackendAnthropic = "anthropic"
	BackendOpenAI    = "openai"
)

// errNoBackend is the error when no backend is chosen nor has credentials:
// it says the two ways.
var errNoBackend = errors.New("no LLM backend for the planner: set ANTHROPIC_API_KEY (or run `ant auth login`) to use Anthropic, " +
	"or set PROSA_LLM=openai and PROSA_LLM_BASE_URL (plus PROSA_LLM_API_KEY if the server wants one) to use an OpenAI-compatible API")

// NewBackend returns the backend that the environment chooses: PROSA_LLM
// (anthropic or openai); unset, anthropic when it has credentials
// (anthropicCredentials), else an error naming both ways. openai needs
// PROSA_LLM_BASE_URL and takes PROSA_LLM_API_KEY when there is one.
func NewBackend(getenv func(string) string) (Backend, error) {
	switch name := getenv("PROSA_LLM"); name {
	case "":
		if !anthropicCredentials(getenv) {
			return nil, errNoBackend
		}
		return Anthropic{Messages: newSDKMessages()}, nil
	case BackendAnthropic:
		return Anthropic{Messages: newSDKMessages()}, nil
	case BackendOpenAI:
		base := getenv("PROSA_LLM_BASE_URL")
		if base == "" {
			return nil, errors.New("PROSA_LLM=openai needs PROSA_LLM_BASE_URL, the API's base URL (for example https://api.openai.com/v1 or http://localhost:11434/v1)")
		}
		return &OpenAI{BaseURL: base, APIKey: getenv("PROSA_LLM_API_KEY")}, nil
	default:
		return nil, fmt.Errorf("PROSA_LLM=%q: want anthropic or openai", name)
	}
}
