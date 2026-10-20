package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/zclconf/go-cty/cty"

	"github.com/giammbo/prosa/internal/provider"
)

// validateInPlan runs in.Validate(d) inside the planner call of an intent "app"
// (its lock already holding appDesired's keys) and returns the error.
func validateInPlan(t *testing.T, e *testEnv, rp *recordingPlanner, d Desired) error {
	t.Helper()
	var got error
	rp.check = func(ctx context.Context, in PlanInput) { got = in.Validate(ctx, d) }
	e.writeIntent("app", "app v2")
	e.plan()
	return got
}

func TestValidatePlansNewKeysOnly(t *testing.T) {
	rp := &recordingPlanner{desired: map[string]Desired{"app": appDesired()}}
	e := newTestEnv(t, rp)
	e.writeIntent("app", "app v1")
	e.mustApply(e.plan())
	e.fake.planErr = func(typeName string, cfg cty.Value) error {
		return fmt.Errorf("plan %s: CustomizeDiff: name %q is reserved", typeName, cfg.GetAttr("name").AsString())
	}

	// "net" and "db" are in the lock: not planned. "extra" is new: the
	// provider's plan-time error comes back as a problem on it.
	var err error
	var before, after int
	rp.check = func(ctx context.Context, in PlanInput) {
		before = len(e.fake.calls)
		err = in.Validate(ctx, Desired{Resources: append(appDesired().Resources,
			DesiredResource{Key: "extra", Type: "test_net", Config: map[string]any{"name": "taken"}})})
		after = len(e.fake.calls)
	}
	e.writeIntent("app", "app v2")
	e.plan()
	var ve *ValidationError
	if !errors.As(err, &ve) || len(ve.Problems) != 1 {
		t.Fatalf("Validate = %v, want one problem", err)
	}
	if pr := ve.Problems[0]; pr.Key != "extra" || pr.Type != "test_net" || !strings.Contains(pr.Message, `CustomizeDiff: name "taken" is reserved`) {
		t.Errorf("problem = %+v", pr)
	}
	if plans := callsWithPrefix(e.fake.calls[before:after], "plan "); len(plans) != 1 {
		t.Errorf("PlanResource calls inside Validate = %v, want one (the new key)", plans)
	}
}

func callsWithPrefix(calls []string, prefix string) []string {
	var out []string
	for _, c := range calls {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

func TestValidatePlanCallCountAndOrder(t *testing.T) {
	rp := &recordingPlanner{desired: map[string]Desired{"app": appDesired()}}
	e := newTestEnv(t, rp)
	e.writeIntent("app", "app v1")
	e.mustApply(e.plan())

	var before, after int
	var err error
	rp.check = func(ctx context.Context, in PlanInput) {
		before = len(e.fake.calls)
		err = in.Validate(ctx, Desired{Resources: append(appDesired().Resources,
			DesiredResource{Key: "extra", Type: "test_net", Config: map[string]any{"name": "ok"}})})
		after = len(e.fake.calls)
	}
	e.writeIntent("app", "app v2")
	e.plan()
	if err != nil {
		t.Fatalf("Validate = %v", err)
	}
	inside := e.fake.calls[before:after]
	plans := callsWithPrefix(inside, "plan ")
	if len(plans) != 1 || plans[0] != "plan test_net" {
		t.Errorf("PlanResource calls inside Validate = %v, want exactly one, for the new key", plans)
	}
	// The new key is validated first, then planned.
	vi, pi := slices.Index(inside, "validate test_net"), slices.Index(inside, "plan test_net")
	if vi < 0 || pi < 0 || vi > pi {
		t.Errorf("calls inside Validate = %v, want validate test_net before plan test_net", inside)
	}
}

func TestValidateDoesNotPlanInvalidResources(t *testing.T) {
	rp := &recordingPlanner{desired: map[string]Desired{"app": appDesired()}}
	e := newTestEnv(t, rp)
	e.writeIntent("app", "app v1")
	e.mustApply(e.plan())
	e.fake.planErr = func(string, cty.Value) error { return errors.New("plan: must not be asked") }

	var before, after int
	var err error
	rp.check = func(ctx context.Context, in PlanInput) {
		before = len(e.fake.calls)
		// size 500 fails the provider's validation: no plan for it.
		err = in.Validate(ctx, Desired{Resources: []DesiredResource{
			{Key: "big", Type: "test_db", Config: map[string]any{"name": "big", "size": json.Number("500")}},
		}})
		after = len(e.fake.calls)
	}
	e.writeIntent("app", "app v2")
	e.plan()
	var ve *ValidationError
	if !errors.As(err, &ve) || len(ve.Problems) != 1 || !strings.Contains(ve.Problems[0].Message, "size must be at most 100") {
		t.Fatalf("Validate = %v, want the validation problem only", err)
	}
	if plans := callsWithPrefix(e.fake.calls[before:after], "plan "); len(plans) != 0 {
		t.Errorf("PlanResource called for an invalid resource: %v", plans)
	}
}

func TestValidatePlanTransportErrorIsNotAProblem(t *testing.T) {
	rp := &recordingPlanner{desired: map[string]Desired{"app": appDesired()}}
	e := newTestEnv(t, rp)
	e.writeIntent("app", "app v1")
	e.mustApply(e.plan())
	dead := errors.New("rpc error: code = Unavailable")
	e.fake.planErr = func(typeName string, _ cty.Value) error {
		return &provider.TransportError{Op: "plan " + typeName, Err: dead}
	}
	err := validateInPlan(t, e, rp, Desired{Resources: append(appDesired().Resources,
		DesiredResource{Key: "extra", Type: "test_net", Config: map[string]any{"name": "x"}})})
	var ve *ValidationError
	if err == nil || errors.As(err, &ve) || !errors.Is(err, dead) {
		t.Fatalf("Validate = %v (%T), want a plain error wrapping the transport error", err, err)
	}
}

func TestValidateReferencedKeyTypeChangeMessage(t *testing.T) {
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

	var err error
	rp.check = func(ctx context.Context, in PlanInput) {
		if in.Intent.Name != "app" {
			return
		}
		err = in.Validate(ctx, Desired{Resources: []DesiredResource{
			{Key: "net", Type: "test_db", Config: map[string]any{"name": "net"}},
			{Key: "db", Type: "test_net", Config: map[string]any{"name": "db"}},
		}})
	}
	e.writeIntent("app", "app v2")
	e.plan()
	var ve *ValidationError
	if !errors.As(err, &ve) || len(ve.Problems) != 2 {
		t.Fatalf("Validate = %v, want two problems", err)
	}
	var db, net ValidationProblem
	for _, pr := range ve.Problems {
		if pr.Key == "db" {
			db = pr
		} else {
			net = pr
		}
	}
	if want := `key "net" is referred to by web/site: keep the type test_net, not test_db`; net.Message != want {
		t.Errorf("referenced key: %q, want %q", net.Message, want)
	}
	if want := `key "db" was test_db, cannot become test_net: use a new key`; db.Message != want {
		t.Errorf("other key: %q, want %q", db.Message, want)
	}
}

func TestDesiredOrderChecksDependsOnOfNodesWithBadRefs(t *testing.T) {
	text := "bad ref and bad depends_on together"
	d := Desired{Resources: []DesiredResource{
		{Key: "a", Type: "test_net", Config: map[string]any{"name": "${app/nope.id}"}, DependsOn: []string{"app/ghost"}},
	}}
	e := newTestEnv(t, StaticPlanner{hashOf(text): d})
	e.writeIntent("app", text)
	_, err := e.engine.Plan(context.Background())
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want a *ValidationError", err)
	}
	var msgs []string
	for _, pr := range ve.Problems {
		msgs = append(msgs, pr.Message)
	}
	all := strings.Join(msgs, "\n")
	if !strings.Contains(all, "app/nope") || !strings.Contains(all, `depends_on "app/ghost"`) {
		t.Errorf("problems =\n%s\nwant both the bad reference and the bad depends_on", all)
	}
}

func TestCanonicalSortsAndDedupsDependsOn(t *testing.T) {
	text := "depends_on in any order"
	d := Desired{Resources: []DesiredResource{
		{Key: "a", Type: "test_net", Config: map[string]any{"name": "a"}, DependsOn: []string{"app/z", "app/y", "app/z"}},
		{Key: "y", Type: "test_net", Config: map[string]any{"name": "y"}},
		{Key: "z", Type: "test_net", Config: map[string]any{"name": "z"}},
	}}
	e := newTestEnv(t, StaticPlanner{hashOf(text): d})
	e.writeIntent("app", text)
	e.mustApply(e.plan())
	if got := lockRes(e.loadLock().Intents["app"], "a").DependsOn; !equalStrings(got, []string{"app/y", "app/z"}) {
		t.Fatalf("lock depends_on = %v, want sorted and deduplicated", got)
	}
	// The planner's other order of the same set changes nothing.
	d.Resources[0].DependsOn = []string{"app/y", "app/z"}
	e.engine.Planner = StaticPlanner{hashOf(text): d}
	if p := e.plan(); p.HasChanges() || p.LockDirty {
		t.Errorf("same set in another order: changes %v, LockDirty %v", summary(p), p.LockDirty)
	}
	// The input is not modified.
	in := Desired{Resources: []DesiredResource{{Key: "a", Type: "test_net", DependsOn: []string{"b/x", "a/x"}}}}
	pl := &Plan{schema: e.fake.schema}
	if got := pl.canonical(in).Resources[0].DependsOn; !equalStrings(got, []string{"a/x", "b/x"}) {
		t.Errorf("canonical = %v", got)
	}
	if in.Resources[0].DependsOn[0] != "b/x" {
		t.Error("canonical modified its input")
	}
}

func TestApplyOrderRuleCWithDependsOn(t *testing.T) {
	// y stood on a through depends_on and now stands on w, which is replaced:
	// a goes last, after y moved off it (rule c), and y waits for the new w.
	text := "y moves from a to the replaced w"
	d := Desired{Resources: []DesiredResource{
		{Key: "w", Type: "test_net", Config: map[string]any{"name": "w2"}},
		{Key: "y", Type: "test_db", Config: map[string]any{"name": "y2"}, DependsOn: []string{"app/w"}},
	}}
	e := newTestEnv(t, StaticPlanner{hashOf(text): d})
	e.fake.replaceOn["test_net"] = []string{"name"}
	e.fake.nextID["test_net"], e.fake.nextID["test_db"] = 1, 1
	e.writeIntent("app", text)
	owner := map[string]any{"prosa/intent": "app"}
	e.seed("app", hashOf("before"),
		seedRes{key: "a", typ: "test_clock", chosen: map[string]any{"name": "a"},
			state: map[string]any{"id": "test_clock-1", "name": "a"}},
		seedRes{key: "w", typ: "test_net", chosen: map[string]any{"name": "w"},
			state: map[string]any{"id": "test_net-1", "name": "w", "tags": owner}},
		seedRes{key: "y", typ: "test_db", chosen: map[string]any{"name": "y"}, dependsOn: []string{"app/a"},
			state: map[string]any{"id": "test_db-1", "name": "y", "tags": owner}},
	)
	p := e.plan()
	log := e.logApplies()
	e.mustApply(p)
	want := []string{"D test_net", "C test_net w2", "C test_db y2", "D test_clock"}
	if !equalStrings(*log, want) {
		t.Fatalf("applies = %v, want %v", *log, want)
	}
}

func TestDesiredResourceJSONTags(t *testing.T) {
	out, err := json.Marshal(DesiredResource{Key: "k", Type: "t", Config: map[string]any{"a": "b"}, DependsOn: []string{"x/y"}})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"key":"k","type":"t","config":{"a":"b"},"depends_on":["x/y"]}`; string(out) != want {
		t.Errorf("json = %s, want %s", out, want)
	}
	// Files written before the tags (Key, Type, Config) still load.
	for _, in := range []string{
		`{"Key":"k","Type":"t","Config":{"a":"b"},"depends_on":["x/y"]}`,
		`{"key":"k","type":"t","config":{"a":"b"},"depends_on":["x/y"]}`,
	} {
		var r DesiredResource
		if err := json.Unmarshal([]byte(in), &r); err != nil {
			t.Fatal(err)
		}
		if r.Key != "k" || r.Type != "t" || r.Config["a"] != "b" || len(r.DependsOn) != 1 {
			t.Errorf("%s -> %+v", in, r)
		}
	}
}

func TestUnknownDollarBraceHint(t *testing.T) {
	keysOf := func(string) []string { return []string{"k"} }
	for _, s := range []string{"${aws:username}", "arn:${aws:username}/x"} {
		_, _, err := ParseTemplate(s, keysOf)
		if err == nil {
			// a whole reference is not a template; use CollectRefs
			_, err = CollectRefs(map[string]any{"v": s}, keysOf)
		}
		if err == nil || !strings.Contains(err.Error(), "write $${ for a literal ${") {
			t.Errorf("%s: err = %v, want the $${ hint", s, err)
		}
	}
}

func TestValidateSkipsProviderForNodesWithGraphProblems(t *testing.T) {
	rp := &recordingPlanner{desired: map[string]Desired{"app": appDesired()}}
	e := newTestEnv(t, rp)
	e.writeIntent("app", "app v1")
	e.mustApply(e.plan())

	var before, after int
	var err error
	rp.check = func(ctx context.Context, in PlanInput) {
		before = len(e.fake.calls)
		err = in.Validate(ctx, Desired{Resources: append(appDesired().Resources,
			DesiredResource{Key: "extra", Type: "test_clock", Config: map[string]any{"name": "x", "nope": "y"}, DependsOn: []string{"app/ghost"}})})
		after = len(e.fake.calls)
	}
	e.writeIntent("app", "app v2")
	e.plan()
	var ve *ValidationError
	if !errors.As(err, &ve) || len(ve.Problems) != 2 || !strings.Contains(ve.Problems[0].Message+ve.Problems[1].Message, `depends_on "app/ghost"`) ||
		!strings.Contains(ve.Problems[0].Message+ve.Problems[1].Message, `unknown attribute or block "nope"`) {
		t.Fatalf("Validate = %v, want the depends_on and the unknown attribute problems", err)
	}
	for _, c := range e.fake.calls[before:after] {
		if strings.HasSuffix(c, " test_clock") {
			t.Errorf("provider called for the node with a bad depends_on: %v", e.fake.calls[before:after])
		}
	}
}

func TestPlanSkipsProviderForNodesWithGraphProblems(t *testing.T) {
	text := "bad depends_on and unknown attribute"
	d := Desired{Resources: []DesiredResource{
		{Key: "a", Type: "test_clock", Config: map[string]any{"name": "a", "nope": "y"}, DependsOn: []string{"app/ghost"}},
	}}
	e := newTestEnv(t, StaticPlanner{hashOf(text): d})
	e.writeIntent("app", text)
	_, err := e.engine.Plan(context.Background())
	var ve *ValidationError
	if !errors.As(err, &ve) || len(ve.Problems) != 2 {
		t.Fatalf("err = %v, want two problems", err)
	}
	all := ve.Problems[0].Message + ve.Problems[1].Message
	if !strings.Contains(all, `depends_on "app/ghost"`) || !strings.Contains(all, `unknown attribute or block "nope"`) {
		t.Errorf("problems = %+v", ve.Problems)
	}
	if calls := callsWithPrefix(e.fake.calls, "validate "); len(calls) != 0 {
		t.Errorf("provider validated a node with a graph problem: %v", calls)
	}
}
