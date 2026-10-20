package planner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sync"

	"github.com/anthropics/anthropic-sdk-go"
)

// Messages is the one Messages API call the anthropic backend makes: New
// sends a request and returns the complete final message of the response.
// The SDK implementation (sdkMessages) streams the response and accumulates
// it, as long outputs require; Replay and Recorder deal in final messages
// only, so tests never stream nor touch the network.
type Messages interface {
	New(ctx context.Context, params anthropic.BetaMessageNewParams) (*anthropic.BetaMessage, error)
}

// recording is the file format of a Replay or a Recorder: the responses of
// one conversation and the prompt they were recorded with, so that a
// recording made with another prompt fails loudly instead of replaying
// answers to questions no longer asked.
type recording struct {
	PromptVersion string            `json:"prompt_version"`
	PromptSHA256  string            `json:"prompt_sha256"`
	Turns         []json.RawMessage `json:"turns"`
}

// fingerprint is the sha256, in hex, of the cached prefix of an Anthropic
// request: its tools and system prompt.
func fingerprint(p anthropic.BetaMessageNewParams) (string, error) {
	prefix, err := json.Marshal(struct {
		Tools  []anthropic.BetaToolUnionParam `json:"tools"`
		System []anthropic.BetaTextBlockParam `json:"system"`
	}{p.Tools, p.System})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(prefix)
	return hex.EncodeToString(sum[:]), nil
}

// PromptFingerprint is the fingerprint of the planner's current system
// prompt and tools, as recordings carry it.
func PromptFingerprint() string {
	fp, err := fingerprint(anthropicParams(systemPrompt, tools(), "", DefaultEffort))
	if err != nil {
		panic(err) // fixed data: cannot fail
	}
	return fp
}

// Replay answers with recorded responses, in order, and checks what prompt
// caching and preserved thinking need: every request has the system prompt
// and tools of the recording (of the first request, for a Replay built by
// hand), and its messages start with those of the previous request,
// unchanged. Requests keeps what it was sent.
type Replay struct {
	Turns    []json.RawMessage
	Requests []anthropic.BetaMessageNewParams
	// Fingerprint is the fingerprint of the system prompt and tools the
	// turns were recorded with; empty, the first request's.
	Fingerprint string

	mu       sync.Mutex
	messages [][]byte
}

// LoadReplay reads a recording (as written by a Recorder) and fails when it
// was made with another prompt version, system prompt or tools than the
// planner's current ones: it must then be recorded again.
func LoadReplay(path string) (*Replay, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rec recording
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, fmt.Errorf("%s: %w (a recording is an object with prompt_version, prompt_sha256 and turns)", path, err)
	}
	if rec.PromptVersion != PromptVersion {
		return nil, fmt.Errorf("%s: recorded with prompt version %q, the planner has %q: record it again", path, rec.PromptVersion, PromptVersion)
	}
	if fp := PromptFingerprint(); rec.PromptSHA256 != fp {
		return nil, fmt.Errorf("%s: recorded with another system prompt or other tools (sha256 %s, now %s): record it again", path, rec.PromptSHA256, fp)
	}
	return &Replay{Turns: rec.Turns, Fingerprint: rec.PromptSHA256}, nil
}

// New returns the next recorded response.
func (r *Replay) New(_ context.Context, params anthropic.BetaMessageNewParams) (*anthropic.BetaMessage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := len(r.Requests)
	if n >= len(r.Turns) {
		return nil, fmt.Errorf("replay: request %d, but only %d responses are recorded", n+1, len(r.Turns))
	}
	fp, err := fingerprint(params)
	if err != nil {
		return nil, err
	}
	if r.Fingerprint == "" {
		r.Fingerprint = fp
	} else if fp != r.Fingerprint {
		return nil, fmt.Errorf("replay: request %d has another system prompt or other tools than the recording", n+1)
	}
	msgs := make([][]byte, len(params.Messages))
	for i, m := range params.Messages {
		if msgs[i], err = json.Marshal(m); err != nil {
			return nil, err
		}
	}
	if len(msgs) < len(r.messages) {
		return nil, fmt.Errorf("replay: request %d has %d messages, fewer than the %d of request %d", n+1, len(msgs), len(r.messages), n)
	}
	for i, m := range r.messages {
		if !bytes.Equal(msgs[i], m) {
			return nil, fmt.Errorf("replay: request %d changes message %d of request %d", n+1, i+1, n)
		}
	}
	r.messages = msgs
	r.Requests = append(r.Requests, params)
	var m anthropic.BetaMessage
	if err := json.Unmarshal(r.Turns[n], &m); err != nil {
		return nil, fmt.Errorf("replay: response %d: %w", n+1, err)
	}
	return &m, nil
}

// Recorder wraps the real client and writes every response to Path, after
// each call, in the format LoadReplay reads, with the prompt version and the
// fingerprint of the request's system prompt and tools. It is meant to be used by hand,
// with credentials, to record new test conversations.
type Recorder struct {
	Inner Messages
	Path  string

	mu    sync.Mutex
	turns []json.RawMessage
}

// New calls Inner and records its response.
func (r *Recorder) New(ctx context.Context, params anthropic.BetaMessageNewParams) (*anthropic.BetaMessage, error) {
	m, err := r.Inner.New(ctx, params)
	if err != nil {
		return nil, err
	}
	raw := json.RawMessage(m.RawJSON())
	if len(raw) == 0 {
		if raw, err = json.Marshal(m); err != nil {
			return nil, err
		}
	}
	fp, err := fingerprint(params)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.turns = append(r.turns, raw)
	data, err := json.MarshalIndent(recording{PromptVersion: PromptVersion, PromptSHA256: fp, Turns: r.turns}, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(r.Path, append(data, '\n'), 0o644); err != nil {
		return nil, fmt.Errorf("recorder: %w", err)
	}
	return m, nil
}
