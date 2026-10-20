package engine

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// index returns the position of s in list, -1 when absent.
func index(list []string, s string) int { return slices.Index(list, s) }

func TestPlanDependsOnOrdersCreates(t *testing.T) {
	text := "a service after its listener"
	d := Desired{Resources: []DesiredResource{
		{Key: "a", Type: "test_net", Config: map[string]any{"name": "svc"}, DependsOn: []string{"app/z"}},
		{Key: "z", Type: "test_net", Config: map[string]any{"name": "listener"}},
	}}
	e := newTestEnv(t, StaticPlanner{hashOf(text): d})
	e.writeIntent("app", text)
	log := e.logApplies()
	p := e.plan()
	if got, want := summary(p), []string{"create app/z", "create app/a"}; !equalStrings(got, want) {
		t.Fatalf("changes = %v, want %v", got, want)
	}
	e.mustApply(p)
	if got, want := *log, []string{"C test_net listener", "C test_net svc"}; !equalStrings(got, want) {
		t.Fatalf("applies = %v, want %v", got, want)
	}
	li := e.loadLock().Intents["app"]
	if got := lockRes(li, "a").DependsOn; !reflect.DeepEqual(got, []string{"app/z"}) {
		t.Fatalf("lock depends_on of a = %v", got)
	}
	if got := lockRes(li, "z").DependsOn; got != nil {
		t.Fatalf("lock depends_on of z = %v, want none", got)
	}

	// Unchanged: the lock's depends_on is the desired one, nothing to write.
	e.engine.Planner = failingPlanner
	p = e.plan()
	if p.HasChanges() || p.LockDirty {
		t.Fatalf("second plan: changes %v, LockDirty %v", summary(p), p.LockDirty)
	}
}

func TestApplyDependsOnDestroysInReverse(t *testing.T) {
	// One type per resource: the apply log names destroys by type only.
	text := "two pairs"
	d := Desired{Resources: []DesiredResource{
		{Key: "a", Type: "test_db", Config: map[string]any{"name": "a"}, DependsOn: []string{"app/z"}},
		{Key: "z", Type: "test_net", Config: map[string]any{"name": "z"}},
		{Key: "b", Type: "test_list", Config: map[string]any{"name": "b"}},
		{Key: "y", Type: "test_clock", Config: map[string]any{"name": "y"}, DependsOn: []string{"app/b"}},
	}}
	e := newTestEnv(t, StaticPlanner{hashOf(text): d})
	e.writeIntent("app", text)
	e.mustApply(e.plan())

	e.engine.Planner = StaticPlanner{hashOf("nothing"): {}}
	e.writeIntent("app", "nothing")
	p := e.plan()
	got := summary(p)
	if len(got) != 4 || index(got, "delete app/a") > index(got, "delete app/z") || index(got, "delete app/y") > index(got, "delete app/b") {
		t.Fatalf("changes = %v, want each dependent deleted before what it depends on", got)
	}
	log := e.logApplies()
	e.mustApply(p)
	if len(*log) != 4 || index(*log, "D test_db") > index(*log, "D test_net") || index(*log, "D test_clock") > index(*log, "D test_list") {
		t.Fatalf("applies = %v, want each dependent destroyed before what it depends on", *log)
	}
}

func TestPlanDependsOnProblems(t *testing.T) {
	text := "bad dependencies"
	d := Desired{Resources: []DesiredResource{
		{Key: "a", Type: "test_net", Config: map[string]any{"name": "a"}, DependsOn: []string{"nokey", "app/", "/a"}},
		{Key: "b", Type: "test_net", Config: map[string]any{"name": "b"}, DependsOn: []string{"app/nope", "ghost/x"}},
		{Key: "c", Type: "test_net", Config: map[string]any{"name": "c"}, DependsOn: []string{"app/c"}},
		{Key: "d", Type: "test_net", Config: map[string]any{"name": "d"}, DependsOn: []string{"app/e"}},
		{Key: "e", Type: "test_net", Config: map[string]any{"name": "e"}, DependsOn: []string{"app/d"}},
		{Key: "f", Type: "test_net", Config: map[string]any{"name": "f"}, DependsOn: []string{"old/n"}},
	}}
	e := newTestEnv(t, StaticPlanner{hashOf(text): d})
	e.writeIntent("app", text)
	e.seed("old", hashOf("old"), seedRes{key: "n", typ: "test_net", chosen: map[string]any{"name": "n"},
		state: map[string]any{"id": "test_net-9", "name": "n"}})
	_, err := e.engine.Plan(context.Background())
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want a *ValidationError", err)
	}
	var got []string
	for _, pr := range ve.Problems {
		got = append(got, pr.Key+": "+pr.Message)
	}
	want := []string{
		`a: depends_on "/a": expected "<intent>/<key>"`, // sorted by canonicalization
		`a: depends_on "app/": expected "<intent>/<key>"`,
		`a: depends_on "nokey": expected "<intent>/<key>"`,
		`b: depends_on "app/nope": unknown key app/nope`,
		`b: depends_on "ghost/x": unknown intent "ghost"`,
		`c: depends_on "app/c": a resource cannot depend on itself`,
		`d: dependency cycle: app/d -> app/e -> app/d`,
		`e: dependency cycle: app/d -> app/e -> app/d`,
		`f: intent "app" depends on old/n, but intent "old" was removed`,
	}
	if !equalStrings(got, want) {
		t.Errorf("problems =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestDependsOnKeepsReferencedKeys(t *testing.T) {
	others := map[string][]DesiredResource{
		"web": {{Key: "svc", Type: "test_net", Config: map[string]any{"name": "svc"}, DependsOn: []string{"app/net", "web/x", "app/gone"}}},
	}
	if got, want := referencedBy("app", []string{"db", "net"}, others), map[string][]string{"net": {"web/svc"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("referencedBy = %v, want %v", got, want)
	}

	rp := &recordingPlanner{desired: map[string]Desired{
		"app": appDesired(),
		"web": {Resources: []DesiredResource{
			{Key: "site", Type: "test_net", Config: map[string]any{"name": "site"}, DependsOn: []string{"app/net"}},
		}},
	}}
	e := newTestEnv(t, rp)
	e.writeIntent("app", "app v1")
	e.writeIntent("web", "a web site after the app net")
	e.mustApply(e.plan())

	var referenced []string
	var dropErr error
	rp.check = func(ctx context.Context, in PlanInput) {
		referenced = in.Referenced
		dropErr = in.Validate(ctx, Desired{Resources: []DesiredResource{{Key: "db", Type: "test_db", Config: map[string]any{"name": "db"}}}})
	}
	e.writeIntent("app", "app v2")
	e.plan()
	if !equalStrings(referenced, []string{"net"}) {
		t.Errorf("Referenced = %v, want [net]", referenced)
	}
	var ve *ValidationError
	if !errors.As(dropErr, &ve) || len(ve.Problems) != 1 || ve.Problems[0].Key != "net" || !strings.Contains(ve.Problems[0].Message, "web/site") {
		t.Errorf("Validate without net = %v, want one problem on net naming web/site", dropErr)
	}
}
