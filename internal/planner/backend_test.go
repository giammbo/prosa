package planner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewBackendSelection(t *testing.T) {
	home := t.TempDir() // no Anthropic profile in it
	profiled := t.TempDir()
	if err := os.MkdirAll(filepath.Join(profiled, ".config", "anthropic", "configs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profiled, ".config", "anthropic", "configs", "default.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	env := func(kv ...string) func(string) string {
		m := map[string]string{"HOME": home}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return func(k string) string { return m[k] }
	}
	for _, c := range []struct {
		name    string
		getenv  func(string) string
		backend string
		errs    []string // substrings of the error, when one is expected
	}{
		{"api key", env("ANTHROPIC_API_KEY", "sk-ant"), "anthropic", nil},
		{"ant auth profile", env("HOME", profiled), "anthropic", nil},
		{"explicit anthropic", env("PROSA_LLM", "anthropic"), "anthropic", nil},
		{"no credentials", env(), "", []string{"ANTHROPIC_API_KEY", "ant auth login", "PROSA_LLM=openai", "PROSA_LLM_BASE_URL"}},
		{"openai without base url", env("PROSA_LLM", "openai", "ANTHROPIC_API_KEY", "sk-ant"), "", []string{"PROSA_LLM_BASE_URL"}},
		{"openai", env("PROSA_LLM", "openai", "PROSA_LLM_BASE_URL", "http://localhost:11434/v1"), "openai", nil},
		{"unknown", env("PROSA_LLM", "gemini"), "", []string{`"gemini"`, "anthropic", "openai"}},
	} {
		b, err := NewBackend(c.getenv)
		if c.errs != nil {
			if err == nil {
				t.Errorf("%s: backend %s, want an error", c.name, b.Name())
				continue
			}
			for _, want := range c.errs {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("%s: err = %v, want it to mention %s", c.name, err, want)
				}
			}
			continue
		}
		if err != nil || b.Name() != c.backend {
			t.Errorf("%s: backend %v, err %v; want %s", c.name, b, err, c.backend)
		}
	}
	b, err := NewBackend(env("PROSA_LLM", "openai", "PROSA_LLM_BASE_URL", "http://h/v1", "PROSA_LLM_API_KEY", "k"))
	if o, ok := b.(*OpenAI); err != nil || !ok || o.BaseURL != "http://h/v1" || o.APIKey != "k" {
		t.Errorf("openai backend = %#v, err %v; want the base URL and the key", b, err)
	}
}

// fakeBackend submits an empty desired state and keeps the requests.
type fakeBackend struct{ reqs []TurnRequest }

func (*fakeBackend) Name() string { return "fake" }

func (f *fakeBackend) Turn(_ context.Context, req TurnRequest) (Turn, error) {
	f.reqs = append(f.reqs, req)
	return Turn{ToolCalls: []ToolCall{{ID: "c", Name: toolSubmit, Input: `{"resources":[]}`}}, Stop: StopToolUse, ServedBy: req.Model}, nil
}

func TestPatchModelUsedForPatch(t *testing.T) {
	plan := planInput("app", "x", validator(new(int)))
	patch := patchInput(validator(new(int)))
	for _, c := range []struct {
		name                       string
		llmModel, patchModel       string
		eff, patchEff              string
		in                         string
		job, model, effort, served string
	}{
		{"plan, defaults", "big", "small", "", "", "plan", JobPlan, "big", "high", "big"},
		{"patch, defaults", "big", "small", "", "", "patch", JobPatch, "small", "low", "small"},
		{"patch, no patch model", "big", "", "", "", "patch", JobPatch, "big", "low", "big"},
		{"efforts set", "big", "", "medium", "high", "patch", JobPatch, "big", "high", "big"},
		{"efforts set, plan", "big", "", "medium", "high", "plan", JobPlan, "big", "medium", "big"},
	} {
		fb := &fakeBackend{}
		l := &LLM{Backend: fb, Model: c.llmModel, PatchModel: c.patchModel, Effort: c.eff, PatchEffort: c.patchEff}
		in := plan
		if c.in == "patch" {
			in = patch
		}
		if _, err := l.Plan(context.Background(), in); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		req := fb.reqs[0]
		if req.Model != c.model || req.Effort != c.effort || req.System != systemPrompt || len(req.Tools) != 4 {
			t.Errorf("%s: request model %q, effort %q (%d tools); want %q, %q", c.name, req.Model, req.Effort, len(req.Tools), c.model, c.effort)
		}
		if r := l.LastResult(); r.Job != c.job || r.Model != c.model || r.Backend != "fake" || r.ServedBy != c.served {
			t.Errorf("%s: result %+v; want job %s, model %s, backend fake", c.name, r, c.job, c.model)
		}
	}

	// On Anthropic the patch model and effort reach the request.
	m := &seen{scripted: scripted{raw: []string{call("t1", "submit_changes", `{"changes":[]}`)}}}
	l := &LLM{Messages: m, Model: DefaultModel, PatchModel: "claude-haiku-5"}
	if _, err := l.Plan(context.Background(), patch); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(m.reqs[0])
	var wire struct {
		Model        string         `json:"model"`
		OutputConfig map[string]any `json:"output_config"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatal(err)
	}
	if wire.Model != "claude-haiku-5" || wire.OutputConfig["effort"] != "low" {
		t.Errorf("anthropic patch request: model %q, output_config %v; want claude-haiku-5, effort low", wire.Model, wire.OutputConfig)
	}
	if r := l.LastResult(); r.Backend != "anthropic" || r.Job != JobPatch || r.Model != "claude-haiku-5" {
		t.Errorf("anthropic result = %+v", r)
	}
}

// The cache keys a patch by the patch model, and a hit reports the job.
func TestCachedPatchModel(t *testing.T) {
	dir := t.TempDir()
	fb := &fakeBackend{}
	c := &Cached{Inner: &LLM{Backend: fb, Model: "big", PatchModel: "small"}, Dir: dir, Model: "big", PatchModel: "small"}
	patch := patchInput(validator(new(int)))
	for range 2 {
		if _, err := c.Plan(context.Background(), patch); err != nil {
			t.Fatal(err)
		}
	}
	if len(fb.reqs) != 1 {
		t.Fatalf("%d requests, want 1 (the second from the cache)", len(fb.reqs))
	}
	if r := c.LastResult(); !r.Cached || r.Job != JobPatch || r.Model != "small" {
		t.Errorf("hit result = %+v, want cached, job patch, model small", r)
	}
	small, _ := cacheKey(patch, "small")
	big, _ := cacheKey(patch, "big")
	if _, err := os.Stat(filepath.Join(dir, small+".json")); err != nil || small == big {
		t.Errorf("cache file of the patch model: %v", err)
	}
}
