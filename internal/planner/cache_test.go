package planner

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/giammbo/prosa/internal/engine"
	"github.com/giammbo/prosa/internal/intent"
	"github.com/giammbo/prosa/internal/lock"
)

type countPlanner struct {
	calls int
	d     engine.Desired
	err   error
}

func (c *countPlanner) Plan(context.Context, engine.PlanInput) (engine.Desired, error) {
	c.calls++
	return c.d, c.err
}

func cacheInput() engine.PlanInput {
	return engine.PlanInput{
		Intent:    intent.Intent{Name: "app", Hash: "sha256:aaa"},
		Region:    "us-east-1",
		Available: []engine.AvailableResource{{Intent: "net", Key: "vpc", Type: "aws_vpc"}},
	}
}

func cacheDesired() engine.Desired {
	return engine.Desired{Resources: []engine.DesiredResource{{
		Key: "web", Type: "aws_instance",
		Config: map[string]any{"name": "x", "big": json.Number("12345678901234567890"), "tags": map[string]any{"a": "b"}},
	}}, Checks: []lock.Check{
		{Name: "Two.", Type: "resource_attr", Resource: "app/web", Path: "count", Op: "eq", Value: json.Number("2")},
		{Name: "Private.", Type: "resource_attr", Resource: "app/web", Path: "public", Op: "eq", Value: false},
		{Name: "Later.", Type: "unverifiable", Reason: "needs history"},
	}}
}

func TestCacheHitSkipsInner(t *testing.T) {
	inner := &countPlanner{d: cacheDesired()}
	c := &Cached{Inner: inner, Dir: filepath.Join(t.TempDir(), "planner"), Model: "m"}
	first, err := c.Plan(context.Background(), cacheInput())
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.Plan(context.Background(), cacheInput())
	if err != nil {
		t.Fatal(err)
	}
	if inner.calls != 1 {
		t.Errorf("inner called %d times, want 1", inner.calls)
	}
	if !reflect.DeepEqual(first, second) {
		t.Errorf("hit differs from miss:\n%#v\n%#v", first, second)
	}
	entries, _ := os.ReadDir(c.Dir)
	if len(entries) != 1 {
		t.Fatalf("cache files = %d, want 1", len(entries))
	}
	fi, _ := os.Stat(filepath.Join(c.Dir, entries[0].Name()))
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("file mode = %v, want 0600", fi.Mode().Perm())
	}
	di, _ := os.Stat(c.Dir)
	if di.Mode().Perm() != 0o700 {
		t.Errorf("dir mode = %v, want 0700", di.Mode().Perm())
	}
}

func TestCacheKeyChangesWithPrevious(t *testing.T) {
	inner := &countPlanner{d: cacheDesired()}
	c := &Cached{Inner: inner, Dir: t.TempDir(), Model: "m"}
	base := cacheInput()
	plan := func(in engine.PlanInput) {
		t.Helper()
		if _, err := c.Plan(context.Background(), in); err != nil {
			t.Fatal(err)
		}
	}
	plan(base)
	withPrev := base
	withPrev.Previous = []lock.Resource{{Key: "web", Type: "aws_instance", Chosen: map[string]any{"ami": "a1"}}}
	plan(withPrev)
	changed := base
	changed.Previous = []lock.Resource{{Key: "web", Type: "aws_instance", Chosen: map[string]any{"ami": "a2"}}}
	plan(changed)
	body := base
	body.PreviousBody = "old text"
	plan(body)
	ref := base
	ref.Referenced = []string{"web"}
	plan(ref)
	renamed := base
	renamed.Intent.Name = "app2"
	plan(renamed)
	deps := base
	deps.Previous = []lock.Resource{{Key: "web", Type: "aws_instance", Chosen: map[string]any{"ami": "a1"}, DependsOn: []string{"app/lb"}}}
	plan(deps)
	if inner.calls != 7 {
		t.Errorf("inner called %d times for 7 distinct inputs", inner.calls)
	}
	plan(withPrev)
	if inner.calls != 7 {
		t.Errorf("repeating an input called inner again (%d)", inner.calls)
	}
	// The lock's id and provider of a previous resource are not seen by the
	// planner: they do not change the key.
	ids := base
	ids.Previous = []lock.Resource{{Key: "web", Type: "aws_instance", Chosen: map[string]any{"ami": "a1"}, ID: "i-123", Provider: "hashicorp/aws"}}
	plan(ids)
	if inner.calls != 7 {
		t.Errorf("id and provider changed the key (%d calls)", inner.calls)
	}
	// Model changes the key too; Schemas, Validate and Canonical do not.
	other := &Cached{Inner: inner, Dir: c.Dir, Model: "other"}
	if _, err := other.Plan(context.Background(), base); err != nil {
		t.Fatal(err)
	}
	if inner.calls != 8 {
		t.Errorf("a different model hit the cache (%d calls)", inner.calls)
	}
	v := base
	v.Validate = func(context.Context, engine.Desired) error { return nil }
	v.Canonical = func(d engine.Desired) engine.Desired { return d }
	plan(v)
	if inner.calls != 8 {
		t.Errorf("Validate or Canonical changed the key (%d calls)", inner.calls)
	}
}

func TestCacheSkipsBlockedAndErrors(t *testing.T) {
	dir := t.TempDir()
	blocked := &countPlanner{d: engine.Desired{Blockers: []string{"contradiction"}}}
	c := &Cached{Inner: blocked, Dir: dir, Model: "m"}
	for i := 0; i < 2; i++ {
		if _, err := c.Plan(context.Background(), cacheInput()); err != nil {
			t.Fatal(err)
		}
	}
	if blocked.calls != 2 {
		t.Errorf("blocked desired was cached (%d calls)", blocked.calls)
	}
	failing := &countPlanner{err: errors.New("boom")}
	c.Inner = failing
	for i := 0; i < 2; i++ {
		if _, err := c.Plan(context.Background(), cacheInput()); err == nil {
			t.Fatal("want the inner error")
		}
	}
	if failing.calls != 2 {
		t.Errorf("error was cached (%d calls)", failing.calls)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("cache has %d files, want none", len(entries))
	}
}

func TestCacheHitIsValidated(t *testing.T) {
	inner := &countPlanner{d: cacheDesired()}
	c := &Cached{Inner: inner, Dir: t.TempDir(), Model: "m"}
	in := cacheInput()
	valid := true
	validated := 0
	in.Validate = func(context.Context, engine.Desired) error {
		validated++
		if valid {
			return nil
		}
		return &engine.ValidationError{}
	}
	if _, err := c.Plan(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Plan(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if inner.calls != 1 || validated != 1 {
		t.Fatalf("valid hit: inner %d, validated %d; want 1, 1", inner.calls, validated)
	}
	valid = false
	if _, err := c.Plan(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if inner.calls != 2 {
		t.Errorf("invalid hit did not re-plan (%d calls)", inner.calls)
	}
}

// Forget drops the answer last given for an intent, a hit or a fresh one,
// so that the next run asks Inner again; other intents keep theirs.
func TestCacheForgetDropsLastAnswer(t *testing.T) {
	inner := &countPlanner{d: cacheDesired()}
	c := &Cached{Inner: inner, Dir: t.TempDir(), Model: "m"}
	other := cacheInput()
	other.Intent = intent.Intent{Name: "web", Hash: "sha256:bbb"}
	for _, in := range []engine.PlanInput{cacheInput(), other} {
		if _, err := c.Plan(context.Background(), in); err != nil {
			t.Fatal(err)
		}
	}
	c.Forget("app") // the fresh answer
	if _, err := c.Plan(context.Background(), cacheInput()); err != nil || inner.calls != 3 {
		t.Fatalf("after forgetting a fresh answer: err %v, inner calls %d, want 3", err, inner.calls)
	}
	if _, err := c.Plan(context.Background(), cacheInput()); err != nil || inner.calls != 3 {
		t.Fatalf("hit: err %v, inner calls %d, want 3", err, inner.calls)
	}
	c.Forget("app") // the hit
	if _, err := c.Plan(context.Background(), cacheInput()); err != nil || inner.calls != 4 {
		t.Fatalf("after forgetting a hit: err %v, inner calls %d, want 4", err, inner.calls)
	}
	if _, err := c.Plan(context.Background(), other); err != nil || inner.calls != 4 {
		t.Fatalf("other intent: err %v, inner calls %d, want its hit", err, inner.calls)
	}
	c.Forget("nope") // nothing to drop
}

func TestCacheKeyIncludesEnv(t *testing.T) {
	inner := &countPlanner{d: cacheDesired()}
	c := &Cached{Inner: inner, Dir: t.TempDir(), Model: "m"}
	plan := func(in engine.PlanInput) {
		t.Helper()
		if _, err := c.Plan(context.Background(), in); err != nil {
			t.Fatal(err)
		}
	}
	env := func(name, hash, prev string) engine.PlanInput {
		in := cacheInput()
		in.Env = &intent.Env{Name: name, Hash: hash, Body: "body " + hash}
		in.PreviousEnvBody = prev
		return in
	}
	plan(cacheInput())
	plan(env("staging", "sha256:e1", ""))
	plan(env("prod", "sha256:e1", ""))
	plan(env("staging", "sha256:e2", ""))
	plan(env("staging", "sha256:e1", "older"))
	if inner.calls != 5 {
		t.Fatalf("inner called %d times, want 5: env name, env hash and previous env body are all in the key", inner.calls)
	}
	plan(env("staging", "sha256:e1", ""))
	if inner.calls != 5 {
		t.Errorf("same env asked inner again (%d calls)", inner.calls)
	}
}
