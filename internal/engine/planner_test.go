package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zclconf/go-cty/cty"

	"github.com/giammbo/prosa/internal/intent"
	"github.com/giammbo/prosa/internal/provider"
)

func TestLoadFixtures(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fx.json")
	write := func(content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(`[{"intent": "app", "hash": "sha256:abc", "resources": [
		{"key": "thing", "type": "test_thing", "config": {"name": "x", "size": 10, "big": 9007199254740993, "ratio": 0.1, "ids": ["${app/other.id}"]}}]}]`)
	sp, err := LoadFixtures(path)
	if err != nil {
		t.Fatal(err)
	}
	d, ok := sp["sha256:abc"]
	if !ok || len(d.Resources) != 1 {
		t.Fatalf("fixtures = %+v", sp)
	}
	r := d.Resources[0]
	if r.Key != "thing" || r.Type != "test_thing" || r.Config["name"] != "x" {
		t.Errorf("resource = %+v", r)
	}
	for k, want := range map[string]string{"size": "10", "big": "9007199254740993", "ratio": "0.1"} {
		if n, ok := r.Config[k].(json.Number); !ok || n.String() != want {
			t.Errorf("config[%q] = %#v, want json.Number %s", k, r.Config[k], want)
		}
	}

	write(`[{"intent": "a", "hash": "sha256:1", "resources": []}, {"intent": "b", "hash": "sha256:1", "resources": []}]`)
	if _, err := LoadFixtures(path); err == nil || !strings.Contains(err.Error(), "sha256:1") {
		t.Errorf("duplicate hash: err = %v", err)
	}
	write(`{"not": "a list"}`)
	if _, err := LoadFixtures(path); err == nil || !strings.Contains(err.Error(), path) {
		t.Errorf("bad JSON: err = %v, want an error naming %s", err, path)
	}
}

// recordingPlanner answers from desired (by intent name) and records the
// input of every call; check, when set, runs inside the call.
type recordingPlanner struct {
	desired map[string]Desired
	inputs  map[string]PlanInput
	order   []string
	check   func(ctx context.Context, in PlanInput)
}

func (r *recordingPlanner) Plan(ctx context.Context, in PlanInput) (Desired, error) {
	if r.inputs == nil {
		r.inputs = map[string]PlanInput{}
	}
	r.inputs[in.Intent.Name] = in
	r.order = append(r.order, in.Intent.Name)
	if r.check != nil {
		r.check(ctx, in)
	}
	d, ok := r.desired[in.Intent.Name]
	if !ok {
		return Desired{}, errors.New("no desired state for " + in.Intent.Name)
	}
	return d, nil
}

func availableKeys(in PlanInput) []string {
	var out []string
	for _, a := range in.Available {
		out = append(out, a.Intent+"/"+a.Key+" "+a.Type)
	}
	return out
}

func TestPlanInputCarriesContext(t *testing.T) {
	t.Setenv("PROSA_AWS_REGION", "us-west-2")
	rp := &recordingPlanner{desired: map[string]Desired{
		"app": appDesired(),
		"web": {Resources: []DesiredResource{
			{Key: "site", Type: "test_net", Config: map[string]any{"name": "${app/net.name}"}},
		}},
	}}
	e := newTestEnv(t, rp)
	e.writeIntent("app", "app v1")
	e.writeIntent("web", "a web site on the app net")
	e.mustApply(e.plan())

	// app changes and gains a cache; api is new, depends on app and refers
	// to the new cache: it must be planned after app and see it.
	rp.desired["app"] = Desired{Resources: append(appDesired().Resources,
		DesiredResource{Key: "cache", Type: "test_net", Config: map[string]any{"name": "cache"}})}
	rp.desired["api"] = Desired{Resources: []DesiredResource{
		{Key: "svc", Type: "test_net", Config: map[string]any{"name": "${app/cache.name}"}},
	}}
	rp.inputs, rp.order = nil, nil
	// api validates against the desired state app got earlier in this Plan.
	var goodErr, badErr error
	rp.check = func(ctx context.Context, in PlanInput) {
		if in.Intent.Name != "api" {
			return
		}
		ref := func(r string) Desired {
			return Desired{Resources: []DesiredResource{{Key: "svc", Type: "test_net", Config: map[string]any{"name": r}}}}
		}
		goodErr = in.Validate(ctx, ref("${app/cache.name}"))
		badErr = in.Validate(ctx, ref("${app/nope.name}"))
	}
	e.writeIntent("app", "app v2")
	e.writeIntent("api", "---\ndepends_on: [app]\n---\nan api on the app cache\n")
	e.plan()
	if goodErr != nil {
		t.Errorf("api Validate(${app/cache.name}) = %v, want nil", goodErr)
	}
	var ve *ValidationError
	if !errors.As(badErr, &ve) || len(ve.Problems) != 1 || ve.Problems[0].Key != "svc" || !strings.Contains(ve.Problems[0].Message, "app/nope") {
		t.Errorf("api Validate(${app/nope.name}) = %v, want one problem on svc naming app/nope", badErr)
	}

	if want := []string{"app", "api"}; !equalStrings(rp.order, want) {
		t.Fatalf("planner order = %v, want %v", rp.order, want)
	}
	app := rp.inputs["app"]
	if app.PreviousBody != "app v1" {
		t.Errorf("app PreviousBody = %q", app.PreviousBody)
	}
	var prev []string
	for _, r := range app.Previous {
		prev = append(prev, r.Key+" "+r.Type)
	}
	if want := []string{"db test_db", "net test_net"}; !equalStrings(prev, want) {
		t.Errorf("app Previous = %v, want %v", prev, want)
	}
	if got, want := availableKeys(app), []string{"web/site test_net"}; !equalStrings(got, want) {
		t.Errorf("app Available = %v, want %v", got, want)
	}
	if want := []string{"net"}; !equalStrings(app.Referenced, want) {
		t.Errorf("app Referenced = %v, want %v", app.Referenced, want)
	}
	if app.Region != "us-west-2" || app.Schemas == nil || app.Validate == nil {
		t.Errorf("app Region = %q, Schemas = %v, Validate nil = %v", app.Region, app.Schemas, app.Validate == nil)
	}

	api := rp.inputs["api"]
	if api.PreviousBody != "" || api.Previous != nil {
		t.Errorf("api (new) PreviousBody = %q, Previous = %v", api.PreviousBody, api.Previous)
	}
	want := []string{"app/cache test_net", "app/db test_db", "app/net test_net", "web/site test_net"}
	if got := availableKeys(api); !equalStrings(got, want) {
		t.Errorf("api Available = %v, want %v", got, want)
	}
	if len(api.Referenced) != 0 {
		t.Errorf("api Referenced = %v", api.Referenced)
	}
}

func TestValidateCallbackReportsProblems(t *testing.T) {
	rp := &recordingPlanner{desired: map[string]Desired{
		"app": appDesired(),
		"web": {Resources: []DesiredResource{
			{Key: "site", Type: "test_net", Config: map[string]any{"name": "${app/net.id}"}},
		}},
	}}
	e := newTestEnv(t, rp)
	e.writeIntent("app", "app v1")
	e.writeIntent("web", "a web site")
	e.mustApply(e.plan())

	var probs [][]ValidationProblem
	var okErr error
	rp.check = func(ctx context.Context, in PlanInput) {
		for _, d := range []Desired{
			// net (referenced by web) removed, db of a type that does not exist.
			{Resources: []DesiredResource{{Key: "db", Type: "test_nope", Config: map[string]any{"name": "db"}}}},
			// net kept with another type.
			{Resources: []DesiredResource{{Key: "net", Type: "test_db", Config: map[string]any{"name": "net"}}}},
		} {
			err := in.Validate(ctx, d)
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("Validate = %v (%T), want a *ValidationError", err, err)
			}
			probs = append(probs, ve.Problems)
		}
		okErr = in.Validate(ctx, rp.desired["app"])
	}
	rp.desired["app"] = Desired{Resources: append(appDesired().Resources,
		DesiredResource{Key: "extra", Type: "test_net", Config: map[string]any{"name": "extra"}})}
	e.writeIntent("app", "app v2")
	p := e.plan()

	if len(probs[0]) != 2 {
		t.Fatalf("problems = %+v, want 2", probs[0])
	}
	if pr := probs[0][0]; pr.Key != "db" || !strings.Contains(pr.Message, `unknown resource type "test_nope"`) {
		t.Errorf("problem 0 = %+v", pr)
	}
	if pr := probs[0][1]; pr.Key != "net" || pr.Type != "test_net" || !strings.Contains(pr.Message, "web/site") {
		t.Errorf("problem 1 = %+v, want the referenced key net naming web/site", pr)
	}
	if len(probs[1]) != 1 || probs[1][0].Key != "net" || !strings.Contains(probs[1][0].Message, "test_net") {
		t.Errorf("type change problems = %+v", probs[1])
	}
	if okErr != nil {
		t.Errorf("Validate(valid) = %v", okErr)
	}
	// Validate changed nothing: the plan is the planner's answer.
	if got, want := summary(p), []string{"create app/extra", "no-op app/net", "no-op app/db", "no-op web/site"}; !equalStrings(got, want) {
		t.Errorf("changes = %v, want %v", got, want)
	}
}

func TestBlockersBlockPlan(t *testing.T) {
	appText, webText := "three replicas, verified: exactly two", "a web net"
	planner := StaticPlanner{
		hashOf(appText): {Blockers: []string{"three replicas contradict the check", "second reason"}},
		hashOf(webText): {Resources: []DesiredResource{{Key: "net", Type: "test_net", Config: map[string]any{"name": "web"}}}},
	}
	e := newTestEnv(t, planner)
	// app already has a resource: a blocked intent must not plan its delete.
	e.seed("app", hashOf("old app"), seedRes{key: "old", typ: "test_net", chosen: map[string]any{"name": "old"},
		state: map[string]any{"id": "test_net-9", "name": "old", "tags": map[string]any{"prosa/intent": "app"}}})
	e.writeIntent("app", appText)
	e.writeIntent("web", webText)
	lockBefore := e.readFile(e.lockPath)

	p := e.plan()
	if want := []string{"app: three replicas contradict the check", "app: second reason"}; !equalStrings(p.Blockers, want) {
		t.Fatalf("Blockers = %v, want %v", p.Blockers, want)
	}
	if got, want := summary(p), []string{"create web/net"}; !equalStrings(got, want) {
		t.Fatalf("changes = %v, want %v", got, want)
	}
	var buf bytes.Buffer
	Render(&buf, p, RenderOptions{})
	if out := buf.String(); !strings.HasPrefix(out, "Blocked:\n  app: three replicas contradict the check\n  app: second reason\n") {
		t.Fatalf("render = %q", out)
	}
	err := e.apply(p, func(*Plan) bool { t.Error("approve called for a blocked plan"); return true })
	if err == nil || !strings.Contains(err.Error(), "plan is blocked") {
		t.Fatalf("Apply err = %v, want plan is blocked", err)
	}
	if e.applyCalls() != 0 {
		t.Fatalf("ApplyResource called: %v", e.fake.calls)
	}
	if !bytes.Equal(e.readFile(e.lockPath), lockBefore) {
		t.Fatal("blocked apply rewrote the lock")
	}
}

func TestFinishStoresBody(t *testing.T) {
	text := "---\nautonomy: approve\n---\napp with a net and a db\n"
	e := newTestEnv(t, StaticPlanner{hashOf(text): appDesired()})
	e.writeIntent("app", text)
	e.mustApply(e.plan())
	st, _ := e.loadState("app")
	if st.Body != "app with a net and a db" {
		t.Fatalf("Body = %q", st.Body)
	}
	e.checkConsistent("app")

	// An intent without resources gets its body too.
	empty := "nothing yet"
	e.engine.Planner = StaticPlanner{hashOf(text): appDesired(), hashOf(empty): {}}
	e.writeIntent("idle", empty)
	e.mustApply(e.plan())
	if st, _ := e.loadState("idle"); st.Body != empty {
		t.Fatalf("idle Body = %q", st.Body)
	}
	e.checkConsistent("idle")
}

func TestLoadFixturesBlockers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fx.json")
	if err := os.WriteFile(path, []byte(`[{"intent": "app", "hash": "sha256:abc", "blockers": ["contradiction"]}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	sp, err := LoadFixtures(path)
	if err != nil {
		t.Fatal(err)
	}
	if d := sp["sha256:abc"]; len(d.Blockers) != 1 || d.Blockers[0] != "contradiction" {
		t.Fatalf("fixture = %+v", d)
	}
}

func TestPlanningOrderFollowsDependsOn(t *testing.T) {
	names := func(its []intent.Intent) []string {
		var out []string
		for _, it := range its {
			out = append(out, it.Name)
		}
		return out
	}
	// a depends on b through u, which is not to plan.
	all := []intent.Intent{{Name: "a", DependsOn: []string{"u"}}, {Name: "b"}, {Name: "c"}, {Name: "u", DependsOn: []string{"b"}}}
	if got, want := names(planningOrder(all, []intent.Intent{all[0], all[1], all[2]})), []string{"b", "c", "a"}; !equalStrings(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
	// A cycle: by name.
	its := []intent.Intent{{Name: "a", DependsOn: []string{"b"}}, {Name: "b", DependsOn: []string{"a"}}}
	if got, want := names(planningOrder(its, its)), []string{"a", "b"}; !equalStrings(got, want) {
		t.Errorf("cycle order = %v, want %v", got, want)
	}
}

// Validate plans, from the stored state, every key of the lock whose
// configuration changed: the provider's plan-time errors on an update reach
// the planner as problems. An unchanged key is not planned.
func TestValidatePlansChangedLockKeys(t *testing.T) {
	rp := &recordingPlanner{desired: map[string]Desired{"app": {Resources: []DesiredResource{
		{Key: "net", Type: "test_net", Config: map[string]any{"name": "net"}},
		{Key: "other", Type: "test_net", Config: map[string]any{"name": "other"}},
	}}}}
	e := newTestEnv(t, rp)
	e.writeIntent("app", "app v1")
	e.mustApply(e.plan())
	e.fake.updateErr = func(_ string, config cty.Value) error {
		if config.GetAttr("name").AsString() == "bad" {
			return &provider.DiagError{Op: "plan", Sum: "name bad is reserved", Attr: "name"}
		}
		return nil
	}

	plans := func() int {
		n := 0
		for _, c := range e.fake.calls {
			if c == "plan test_net" {
				n++
			}
		}
		return n
	}
	var bad, renamed, same error
	var badPlans, renamedPlans, samePlans int
	rp.check = func(ctx context.Context, in PlanInput) {
		with := func(netName string) (error, int) {
			before := plans()
			err := in.Validate(ctx, Desired{Resources: []DesiredResource{
				{Key: "net", Type: "test_net", Config: map[string]any{"name": netName}},
				{Key: "other", Type: "test_net", Config: map[string]any{"name": "other"}},
			}})
			return err, plans() - before
		}
		bad, badPlans = with("bad")
		renamed, renamedPlans = with("net2")
		same, samePlans = with("net")
	}
	e.writeIntent("app", "app v2")
	e.plan()

	var ve *ValidationError
	if !errors.As(bad, &ve) || len(ve.Problems) != 1 || ve.Problems[0].Key != "net" || !strings.Contains(ve.Problems[0].Message, "name bad is reserved") {
		t.Fatalf("Validate(bad) = %v, want one problem on app/net from the provider", bad)
	}
	if badPlans != 1 || renamed != nil || renamedPlans != 1 {
		t.Errorf("plans: bad %d, renamed %d (err %v); want one per changed key", badPlans, renamedPlans, renamed)
	}
	if same != nil || samePlans != 0 {
		t.Errorf("unchanged: err %v, %d plans; want none", same, samePlans)
	}
}

// forgettingPlanner is a recordingPlanner that keeps its answers, like the
// planner cache.
type forgettingPlanner struct {
	recordingPlanner
	forgot []string
}

func (f *forgettingPlanner) Forget(name string) { f.forgot = append(f.forgot, name) }

// A provider error while planning a resource the planner just gave tells the
// planner to forget that answer (the cache drops it): the next run plans the
// intent again instead of failing the same way.
func TestPlanProviderErrorForgetsAnswer(t *testing.T) {
	fp := &forgettingPlanner{recordingPlanner: recordingPlanner{desired: map[string]Desired{
		"net": {Resources: []DesiredResource{{Key: "n", Type: "test_net", Config: map[string]any{"name": "n"}}}},
		"app": {Resources: []DesiredResource{{Key: "db", Type: "test_db", Config: map[string]any{"name": "db", "net_id": "${net/n.id}"}}}},
	}}}
	e := newTestEnv(t, fp)
	e.writeIntent("net", "a net")
	e.mustApply(e.plan())
	// The provider rejects the db only with a known net_id: not in
	// validation, where the reference is unknown.
	e.fake.planErr = func(typeName string, config cty.Value) error {
		if typeName == "test_db" && config.GetAttr("net_id").IsKnown() {
			return &provider.DiagError{Op: "plan", Sum: "net is full", Attr: "net_id"}
		}
		return nil
	}
	e.writeIntent("app", "a db on the net")
	if _, err := e.engine.Plan(context.Background()); err == nil || !strings.Contains(err.Error(), "net is full") {
		t.Fatalf("Plan err = %v, want the provider error", err)
	}
	if !equalStrings(fp.forgot, []string{"app"}) {
		t.Fatalf("forgot = %v, want [app] (net came from the lock)", fp.forgot)
	}

	// Any other error is not the answer's fault.
	fp.forgot = nil
	e.fake.planErr = func(typeName string, _ cty.Value) error {
		if typeName == "test_db" {
			return errors.New("connection reset")
		}
		return nil
	}
	if _, err := e.engine.Plan(context.Background()); err == nil {
		t.Fatal("Plan: want the error")
	}
	if len(fp.forgot) != 0 {
		t.Fatalf("forgot = %v after a non-provider error, want none", fp.forgot)
	}
}
