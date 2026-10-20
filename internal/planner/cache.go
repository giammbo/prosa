package planner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/giammbo/prosa/internal/engine"
)

// Cached wraps a planner with an on-disk cache of its answers, one JSON file
// per key in Dir. The key covers everything that shapes the answer (the
// intent and its name, what the lock recorded, the neighbours, the model and
// the prompt) and never the schemas, the validator or the canonicalizer. A hit is still validated, so a
// provider that changed since is re-planned; blocked answers and errors are
// never stored, and an answer the provider rejects at plan time is dropped
// (Forget). Cache failures never fail a plan: they just cost a re-plan.
// Model and PatchModel are those of Inner: a patch is keyed by PatchModel
// (Model when empty), like LLM.
type Cached struct {
	Inner      engine.Planner
	Dir, Model string
	PatchModel string

	last lastResult
	mu   sync.Mutex
	// answered holds, by intent, the cache file of the answer last given
	// (a hit or a stored one), for Forget.
	answered map[string]string
}

// Forget removes the cache file of the answer last given for an intent
// (engine.Forgetter): the provider rejected it, so the next run asks Inner
// again. Nothing happens when there is none; removal errors are ignored.
func (c *Cached) Forget(intentName string) {
	c.mu.Lock()
	path, ok := c.answered[intentName]
	delete(c.answered, intentName)
	c.mu.Unlock()
	if ok {
		os.Remove(path)
	}
}

// remember records path as the answer last given for an intent.
func (c *Cached) remember(intentName, path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.answered == nil {
		c.answered = map[string]string{}
	}
	c.answered[intentName] = path
}

// LastResult returns how the last Plan went: a cached result for a hit, else
// the one of Inner (zero when Inner does not report one).
func (c *Cached) LastResult() Result { return c.last.get() }

type cacheKeyInput struct {
	Name          string
	Intent        string
	Mode          string
	PreviousBody  string
	EnvName       string `json:",omitempty"`
	EnvHash       string `json:",omitempty"`
	PreviousEnv   string `json:",omitempty"`
	Previous      []previousResource
	BaseHash      string   `json:",omitempty"`
	OldPatch      string   `json:",omitempty"`
	Problems      []string `json:",omitempty"`
	RequiredNames []string `json:",omitempty"`
	Available     []engine.AvailableResource
	Referenced    []string
	Region        string
	Model         string
	PromptVersion string
}

// jsonHash returns the hex sha256 of the JSON of v (map keys sorted).
func jsonHash(v any) (string, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// cacheKey returns the hex key of a planning request: what the planner sees
// of it (of the previous resources, their key, type, chosen config and
// depends_on: never the lock's id or provider). An environment derived from
// a base (ADR-030) is shown the base, not the previous resources and text,
// so the key has the hash of the base (resources and checks), the old patch,
// its problems and the required names instead.
func cacheKey(in engine.PlanInput, model string) (string, error) {
	k := cacheKeyInput{
		Name:          in.Intent.Name,
		Intent:        in.Intent.Hash,
		Mode:          in.Mode,
		PreviousBody:  in.PreviousBody,
		Available:     append([]engine.AvailableResource{}, in.Available...),
		Referenced:    append([]string{}, in.Referenced...),
		Region:        in.Region,
		Model:         model,
		PromptVersion: PromptVersion,
	}
	if in.Env != nil {
		k.EnvName, k.EnvHash, k.PreviousEnv = in.Env.Name, engine.IntentHash(in.Intent, in.Env), in.PreviousEnvBody
	}
	baseMode := in.Mode == "patch" && in.Base != nil
	if baseMode {
		k.PreviousBody = ""
		base := make([]previousResource, len(in.Base))
		for i, r := range in.Base {
			base[i] = previousResource{Key: r.Key, Type: r.Type, Config: r.Chosen, DependsOn: r.DependsOn}
		}
		var err error
		if k.BaseHash, err = jsonHash([]any{base, in.BaseChecks}); err != nil {
			return "", err
		}
		if len(in.OldPatch) > 0 {
			if k.OldPatch, err = jsonHash(in.OldPatch); err != nil {
				return "", err
			}
		}
		k.Problems, k.RequiredNames = in.Problems, in.RequiredNames
	} else {
		for _, r := range in.Previous {
			k.Previous = append(k.Previous, previousResource{Key: r.Key, Type: r.Type, Config: r.Chosen, DependsOn: r.DependsOn})
		}
	}
	sort.Slice(k.Previous, func(i, j int) bool { return k.Previous[i].Key < k.Previous[j].Key })
	sort.Slice(k.Available, func(i, j int) bool {
		a, b := k.Available[i], k.Available[j]
		if a.Intent != b.Intent {
			return a.Intent < b.Intent
		}
		return a.Key < b.Key
	})
	sort.Strings(k.Referenced)
	return jsonHash(k) // map keys are sorted: canonical
}

// Plan answers from the cache when it can, else asks Inner and stores a
// non-blocked answer.
func (c *Cached) Plan(ctx context.Context, in engine.PlanInput) (engine.Desired, error) {
	c.last.set(Result{})
	job, model := jobModel(in.Mode, c.Model, c.PatchModel)
	key, err := cacheKey(in, model)
	if err != nil {
		return c.inner(ctx, in)
	}
	path := filepath.Join(c.Dir, key+".json")
	if d, ok := readCached(path); ok {
		if in.Validate == nil || in.Validate(ctx, d) == nil {
			c.last.set(Result{Intent: in.Intent.Name, Job: job, Model: model, Cached: true})
			c.remember(in.Intent.Name, path)
			return d, nil
		}
	}
	d, err := c.inner(ctx, in)
	if err != nil {
		return d, err
	}
	if len(d.Blockers) == 0 {
		writeCached(c.Dir, path, d)
		c.remember(in.Intent.Name, path)
	}
	return d, nil
}

// inner asks Inner and keeps its result, when it reports one.
func (c *Cached) inner(ctx context.Context, in engine.PlanInput) (engine.Desired, error) {
	d, err := c.Inner.Plan(ctx, in)
	if rr, ok := c.Inner.(ResultReporter); ok {
		c.last.set(rr.LastResult())
	}
	return d, err
}

func readCached(path string) (engine.Desired, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return engine.Desired{}, false
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var d engine.Desired
	if err := dec.Decode(&d); err != nil || len(d.Blockers) > 0 {
		return engine.Desired{}, false
	}
	return d, true
}

// writeCached writes atomically (temp file, then rename); errors are ignored.
func writeCached(dir, path string, d engine.Desired) {
	data, err := json.Marshal(d)
	if err != nil {
		return
	}
	if os.MkdirAll(dir, 0o700) != nil {
		return
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*") // created 0600
	if err != nil {
		return
	}
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if werr != nil || cerr != nil || os.Rename(tmp.Name(), path) != nil {
		os.Remove(tmp.Name())
	}
}
