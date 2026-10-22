package planner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/giammbo/prosa/internal/engine"
	"github.com/giammbo/prosa/rules"
)

// CachedReviewer wraps a reviewer with an on-disk cache of its answers, one
// file "review-<key>.json" per key in Dir (beside the planner's). The key
// covers everything the reviewer sees (the intent, the environment, the
// resources, what they refer to, the rules by their Hash, the exceptions),
// the model and ReviewPromptVersion. Errors are never stored; cache failures
// never fail a review: they just cost a call. Model is that of Inner.
type CachedReviewer struct {
	Inner      engine.Reviewer
	Dir, Model string

	last lastResult
}

// LastResult returns how the last Review went: a cached result for a hit,
// else the one of Inner (zero when Inner does not report one).
func (c *CachedReviewer) LastResult() Result { return c.last.get() }

// cachedReview is the file format of a cached review.
type cachedReview struct {
	Violations []cachedViolation `json:"violations"`
}

type cachedViolation struct {
	Rule   string `json:"rule"`
	Key    string `json:"key"`
	Reason string `json:"reason"`
}

type reviewKeyInput struct {
	Intent        string
	Env           string
	Resources     []engine.DesiredResource
	Available     []engine.AvailableResource
	Rules         string
	Exceptions    []rules.Exception
	Model         string
	PromptVersion string
}

// reviewKey returns the hex key of a review request. The resources and what
// they refer to are sorted: their order does not change the answer.
func reviewKey(in engine.ReviewInput, model string) (string, error) {
	k := reviewKeyInput{
		Intent:        in.Intent,
		Env:           in.Env,
		Resources:     slices.Clone(in.Resources),
		Available:     slices.Clone(in.Available),
		Rules:         rules.Hash(in.Rules),
		Exceptions:    in.Exceptions,
		Model:         model,
		PromptVersion: ReviewPromptVersion,
	}
	slices.SortFunc(k.Resources, func(a, b engine.DesiredResource) int { return strings.Compare(a.Key, b.Key) })
	slices.SortFunc(k.Available, func(a, b engine.AvailableResource) int {
		if c := strings.Compare(a.Intent, b.Intent); c != 0 {
			return c
		}
		return strings.Compare(a.Key, b.Key)
	})
	return jsonHash(k)
}

// Review answers from the cache when it can, else asks Inner and stores its
// answer, violations or none.
func (c *CachedReviewer) Review(ctx context.Context, in engine.ReviewInput) ([]engine.Violation, error) {
	c.last.set(Result{})
	key, err := reviewKey(in, c.Model)
	if err != nil {
		return c.inner(ctx, in)
	}
	path := filepath.Join(c.Dir, "review-"+key+".json")
	if vs, ok := readCachedReview(path); ok {
		c.last.set(Result{Intent: in.Intent, Job: JobReview, Model: c.Model, Cached: true})
		return vs, nil
	}
	vs, err := c.inner(ctx, in)
	if err != nil {
		return nil, err
	}
	rec := cachedReview{Violations: make([]cachedViolation, len(vs))}
	for i, v := range vs {
		rec.Violations[i] = cachedViolation{Rule: v.Rule, Key: v.Key, Reason: v.Reason}
	}
	if data, err := json.Marshal(rec); err == nil {
		writeFileAtomic(c.Dir, path, data)
	}
	return vs, nil
}

// inner asks Inner and keeps its result, when it reports one.
func (c *CachedReviewer) inner(ctx context.Context, in engine.ReviewInput) ([]engine.Violation, error) {
	vs, err := c.Inner.Review(ctx, in)
	if rr, ok := c.Inner.(ResultReporter); ok {
		c.last.set(rr.LastResult())
	}
	return vs, err
}

// readCachedReview reads a cached review; false when there is none or it
// does not decode (a file without a violations list included).
func readCachedReview(path string) ([]engine.Violation, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var rec struct {
		Violations *[]cachedViolation `json:"violations"`
	}
	if json.Unmarshal(data, &rec) != nil || rec.Violations == nil {
		return nil, false
	}
	vs := make([]engine.Violation, len(*rec.Violations))
	for i, v := range *rec.Violations {
		vs[i] = engine.Violation{Rule: v.Rule, Key: v.Key, Reason: v.Reason}
	}
	return vs, true
}
