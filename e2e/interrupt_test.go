package e2e

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/zclconf/go-cty/cty"

	"github.com/giammbo/prosa/internal/engine"
	"github.com/giammbo/prosa/internal/provider"
)

// interruptingProvider calls *interrupt, when set, as each ApplyResource
// starts, like a Ctrl-C while the real provider works on it.
type interruptingProvider struct {
	engine.Provider
	interrupt *func()
}

func (p interruptingProvider) ApplyResource(ctx context.Context, typeName string, prior cty.Value, change provider.PlannedChange, config cty.Value) (provider.ApplyResult, error) {
	if *p.interrupt != nil {
		(*p.interrupt)()
	}
	return p.Provider.ApplyResource(ctx, typeName, prior, change, config)
}

// TestInterruptedApplyOnMoto interrupts the apply of the network intent
// while the real provider creates its first resource (ADR-024): that
// resource is created and saved, nothing else starts, lock and state agree,
// and the next run resumes and completes.
func TestInterruptedApplyOnMoto(t *testing.T) {
	RequireE2E(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	fixtures, err := engine.LoadFixtures("testdata/fixtures.json")
	if err != nil {
		t.Fatal(err)
	}
	s := newScript(t, fixtures)
	s.copyIntent("intents/network.md", "network.md")
	var interruptHook func()
	get := s.engine.Providers
	s.engine.Providers = func(ctx context.Context, name, version string) (engine.Provider, error) {
		prov, err := get(ctx, name, version)
		return interruptingProvider{prov, &interruptHook}, err
	}

	p := s.plan(ctx, t)
	if len(p.Changes) != 4 {
		t.Fatalf("plan = %v, want 4 creates", summary(p))
	}
	applyCtx, interrupt := context.WithCancel(ctx)
	defer interrupt()
	interruptHook = interrupt
	err = s.engine.Apply(applyCtx, p, func(*engine.Plan) bool { return true })
	interruptHook = nil
	if !errors.Is(err, engine.ErrInterrupted) || err.Error() != "interrupted: finished 1 of 4 operations; run plan again" {
		t.Fatalf("apply: %v, want interrupted after 1 of 4", err)
	}

	lk := s.lock(t)
	li := lk.Intents["network"]
	if li == nil || li.IntentHash != "" || len(li.Resources) != 1 || li.Resources[0].Key != "vpc" || li.Resources[0].ID == "" {
		t.Fatalf("lock entry = %+v, want the vpc only, with its id", li)
	}
	st, digest, err := s.engine.State.Load("network")
	if err != nil {
		t.Fatal(err)
	}
	if li.State.Serial != st.Serial || li.State.Digest != digest || len(st.Resources) != 1 || st.Resources["vpc"].ID != li.Resources[0].ID {
		t.Fatalf("lock state ref %+v, store serial %d digest %s resources %d", li.State, st.Serial, digest, len(st.Resources))
	}

	// The next run resumes: the vpc is kept, the rest is created.
	p = s.plan(ctx, t)
	creates := 0
	for _, c := range p.Changes {
		switch {
		case c.Key == "vpc" && c.Action != engine.NoOp:
			t.Fatalf("vpc: %s after the interrupt, want no-op", c.Action)
		case c.Action == engine.Create:
			creates++
		}
	}
	if creates != 3 {
		t.Fatalf("plan after the interrupt = %v, want 3 creates", summary(p))
	}
	s.apply(ctx, t, p)
	if li := s.lock(t).Intents["network"]; li.IntentHash != s.hashOf(t, "network") || len(li.Resources) != 4 {
		t.Fatalf("lock entry after resume = %+v", li)
	}
	s.noChanges(ctx, t)
}
