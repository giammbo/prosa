package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/zclconf/go-cty/cty"

	"github.com/giammbo/prosa/internal/intent"
)

func testEnvFile(name, body string) *intent.Env {
	return &intent.Env{Name: name, Path: "/p/envs/" + name + ".md", Body: body, Hash: hashOf(body)}
}

func TestPlanInputCarriesEnv(t *testing.T) {
	rp := &recordingPlanner{desired: map[string]Desired{"app": appDesired()}}
	e := newTestEnv(t, rp)
	env := testEnvFile("staging", "staging: one small instance")
	e.engine.Env = env
	e.writeIntent("app", "app v1")
	e.mustApply(e.plan())

	in := rp.inputs["app"]
	if in.Env == nil || in.Env.Name != "staging" || in.Env.Body != env.Body {
		t.Fatalf("Env = %+v, want the engine's", in.Env)
	}
	if in.PreviousEnvBody != "" {
		t.Errorf("PreviousEnvBody = %q for a new intent", in.PreviousEnvBody)
	}
	if st, _ := e.loadState("app"); st.EnvBody != env.Body {
		t.Fatalf("state EnvBody = %q, want %q", st.EnvBody, env.Body)
	}

	e.engine.Env = testEnvFile("staging", "staging: two instances")
	e.plan()
	if got := rp.inputs["app"].PreviousEnvBody; got != env.Body {
		t.Errorf("PreviousEnvBody = %q, want %q", got, env.Body)
	}
}

func TestEnvChangeReplans(t *testing.T) {
	rp := &recordingPlanner{desired: map[string]Desired{"app": appDesired(), "web": appDesired()}}
	// web has its own keys, not the app's: keep them apart.
	rp.desired["web"] = Desired{Resources: []DesiredResource{{Key: "site", Type: "test_net", Config: map[string]any{"name": "site"}}}}
	e := newTestEnv(t, rp)
	e.engine.Env = testEnvFile("staging", "env v1")
	e.writeIntent("app", "app")
	e.writeIntent("web", "web")
	e.mustApply(e.plan())
	if len(rp.order) != 2 {
		t.Fatalf("first plan asked %v", rp.order)
	}

	// Same files: nothing is planned again.
	rp.order = nil
	e.plan()
	if len(rp.order) != 0 {
		t.Fatalf("unchanged files asked the planner for %v", rp.order)
	}

	// Only the environment file changes: every intent is planned again.
	e.engine.Env = testEnvFile("staging", "env v2")
	rp.order = nil
	e.plan()
	if len(rp.order) != 2 {
		t.Fatalf("changed env asked the planner for %v, want both intents", rp.order)
	}
}

func TestIntentHashPerSection(t *testing.T) {
	body := func(db, common string) *intent.Env {
		return testEnvFile("staging", "# Staging\n"+common+"\n## database\n"+db+"\n## app\nweb\n")
	}
	app := intent.Intent{Name: "app", Hash: "sha256:a"}
	dbi := intent.Intent{Name: "database", Hash: "sha256:d"}
	base := body("40 GB", "tag env")
	other := body("100 GB", "tag env")
	if IntentHash(app, base) != IntentHash(app, other) {
		t.Error("changing ## database changed the hash of app")
	}
	if IntentHash(dbi, base) == IntentHash(dbi, other) {
		t.Error("changing ## database did not change the hash of database")
	}
	common := body("40 GB", "tag env2")
	if IntentHash(app, base) == IntentHash(app, common) || IntentHash(dbi, base) == IntentHash(dbi, common) {
		t.Error("changing a common line must change every hash")
	}
	if got := IntentHash(app, nil); got != app.Hash {
		t.Errorf("without an environment hash = %q, want %q", got, app.Hash)
	}
}

func TestEnvHashInLock(t *testing.T) {
	rp := &recordingPlanner{desired: map[string]Desired{"app": appDesired()}}
	e := newTestEnv(t, rp)
	env := testEnvFile("staging", "env v1")
	e.engine.Env = env
	h := e.writeIntent("app", "app")
	e.mustApply(e.plan())
	if want := IntentHash(intent.Intent{Name: "app", Hash: h}, env); e.loadLock().Intents["app"].IntentHash != want {
		t.Fatalf("intent_hash = %q, want %q", e.loadLock().Intents["app"].IntentHash, want)
	}
}

func TestNoEnvHashUnchanged(t *testing.T) {
	rp := &recordingPlanner{desired: map[string]Desired{"app": appDesired()}}
	e := newTestEnv(t, rp)
	h := e.writeIntent("app", "app")
	e.mustApply(e.plan())
	if got := e.loadLock().Intents["app"].IntentHash; got != h {
		t.Fatalf("intent_hash = %q, want the intent's hash %q", got, h)
	}
	if rp.inputs["app"].Env != nil {
		t.Errorf("Env = %+v without an environment", rp.inputs["app"].Env)
	}
	if st, _ := e.loadState("app"); st.EnvBody != "" {
		t.Errorf("EnvBody = %q without an environment", st.EnvBody)
	}
}

func TestOwnerTagEnv(t *testing.T) {
	text := "tagged"
	d := Desired{Resources: []DesiredResource{{Key: "net", Type: "test_net", Config: map[string]any{
		"name": "net", "tags": map[string]any{"prosa/env": "other", "team": "core"},
	}}}}
	e := newTestEnv(t, StaticPlanner{hashOf(text): d})
	e.engine.Env = testEnvFile("staging", "env")
	e.writeIntent("app", text)

	p := e.plan()
	want := cty.MapVal(map[string]cty.Value{"prosa/intent": cty.StringVal("app"), "prosa/env": cty.StringVal("staging"), "team": cty.StringVal("core")})
	if got := p.Changes[0].After.GetAttr("tags"); !got.RawEquals(want) {
		t.Fatalf("tags = %#v, want %#v", got, want)
	}

	// Without an environment there is no prosa/env tag, not even the planner's.
	e.engine.Env = nil
	p = e.plan()
	want = cty.MapVal(map[string]cty.Value{"prosa/intent": cty.StringVal("app"), "prosa/env": cty.StringVal("other"), "team": cty.StringVal("core")})
	if got := p.Changes[0].After.GetAttr("tags"); !got.RawEquals(want) {
		t.Fatalf("tags without env = %#v, want the planner's kept: %#v", got, want)
	}
}

func TestRecoverHintNamesEnv(t *testing.T) {
	text := "app with a net and a db"
	e := newTestEnv(t, StaticPlanner{hashOf(text): appDesired()})
	e.engine.Env = testEnvFile("staging", "env")
	e.engine.EnvFile = "envs/staging.md"
	e.writeIntent("app", text)
	e.failLockWrites("test_db")
	if err := e.apply(e.plan(), approveAll); err == nil {
		t.Fatal("apply succeeded, want a lock write failure")
	}
	e.fake.onApply = nil
	p := e.plan()
	if want := `run "prosa recover --env envs/staging.md"`; !strings.Contains(p.Blocked, want) {
		t.Fatalf("Blocked = %q, want it to contain %s", p.Blocked, want)
	}
	if strings.Contains(p.Blocked, "<dir>") {
		t.Fatalf("Blocked = %q names <dir>", p.Blocked)
	}
}

func TestStaticPlannerKeysByEffectiveHash(t *testing.T) {
	it := intent.Intent{Name: "app", Hash: hashOf("app")}
	staging, prod := testEnvFile("staging", "s"), testEnvFile("prod", "p")
	net := func(name string) Desired {
		return Desired{Resources: []DesiredResource{{Key: "net", Type: "test_net", Config: map[string]any{"name": name}}}}
	}
	sp := StaticPlanner{IntentHash(it, staging): net("s"), IntentHash(it, prod): net("p"), it.Hash: net("own")}
	name := func(env *intent.Env) any {
		d, err := sp.Plan(context.Background(), PlanInput{Intent: it, Env: env})
		if err != nil {
			t.Fatal(err)
		}
		return d.Resources[0].Config["name"]
	}
	if name(staging) != "s" || name(prod) != "p" || name(nil) != "own" {
		t.Fatalf("staging %v prod %v none %v", name(staging), name(prod), name(nil))
	}
	// An intent-hash-only fixture serves both environments.
	sp = StaticPlanner{it.Hash: net("own")}
	if name(staging) != "own" || name(prod) != "own" {
		t.Fatalf("fallback: staging %v prod %v", name(staging), name(prod))
	}
}
