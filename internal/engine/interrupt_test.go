package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/zclconf/go-cty/cty"

	"github.com/giammbo/prosa/internal/lock"
	"github.com/giammbo/prosa/internal/provider"
	"github.com/giammbo/prosa/internal/state"
)

// checkRecordsMatchLock asserts that every state record of an intent holds
// the chosen, depends_on and id of its lock entry (ADR-024).
func (e *testEnv) checkRecordsMatchLock(intentName string) {
	e.t.Helper()
	st, _ := e.loadState(intentName)
	li := e.loadLock().Intents[intentName]
	for key, rs := range st.Resources {
		lr := lockRes(li, key)
		if lr == nil {
			e.t.Fatalf("%s/%s: in the state, not in the lock", intentName, key)
		}
		if rs.ID != lr.ID || !reflect.DeepEqual(rs.DependsOn, lr.DependsOn) || !sameJSON(rs.Chosen, lr.Chosen) {
			e.t.Fatalf("%s/%s: record id %q depends_on %v chosen %v, lock %+v", intentName, key, rs.ID, rs.DependsOn, rs.Chosen, *lr)
		}
	}
}

func sameJSON(a, b any) bool {
	ja, err1 := json.Marshal(a)
	jb, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && bytes.Equal(ja, jb)
}

// The first interrupt lets the operation in progress finish and be saved;
// no other starts, and the next run resumes (ADR-024).
func TestApplyInterruptFinishesOperationInProgress(t *testing.T) {
	text := "app with a net and a db"
	e := newTestEnv(t, StaticPlanner{hashOf(text): appDesired()})
	h := e.writeIntent("app", text)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.fake.onApply = func(_ context.Context, typeName string) {
		if typeName == "test_net" {
			cancel() // Ctrl-C while the net is being created
		}
	}

	p := e.plan()
	err := e.engine.Apply(ctx, p, approveAll)
	if !errors.Is(err, ErrInterrupted) {
		t.Fatalf("err = %v, want ErrInterrupted", err)
	}
	if want := "interrupted: finished 1 of 2 operations; run plan again"; err.Error() != want {
		t.Fatalf("err = %q, want %q", err, want)
	}
	if info := Classify(err); info.Kind != KindInterrupted {
		t.Fatalf("Classify = %+v, want kind %q", info, KindInterrupted)
	}
	if e.applyCalls() != 1 {
		t.Fatalf("calls = %v, want only the net applied", e.fake.calls)
	}
	st, _ := e.loadState("app")
	if len(st.Resources) != 1 || st.Resources["net"] == nil || stateAttrs(t, st.Resources["net"])["id"] != "test_net-1" {
		t.Fatalf("state resources = %v, want the net only", st.Resources)
	}
	li := e.loadLock().Intents["app"]
	if li == nil || li.IntentHash != "" || len(li.Resources) != 1 || li.Resources[0].Key != "net" || li.Resources[0].ID != "test_net-1" {
		t.Fatalf("lock intent = %+v, want the net only and an empty hash", li)
	}
	e.checkConsistent("app")
	e.checkRecordsMatchLock("app")

	// The next run resumes: only the db is left.
	p = e.plan()
	if got, want := summary(p), []string{"no-op app/net", "create app/db"}; !equalStrings(got, want) {
		t.Fatalf("changes = %v, want %v", got, want)
	}
	e.mustApply(p)
	if li := e.loadLock().Intents["app"]; li.IntentHash != h || len(li.Resources) != 2 {
		t.Fatalf("after resume: %+v", li)
	}
	e.checkConsistent("app")
	e.checkRecordsMatchLock("app")
}

// An interrupt that cuts short the re-plan of the next operation stops
// Apply before that operation: it is neither applied nor reported failed.
func TestApplyInterruptDuringReplanStopsBeforeTheOperation(t *testing.T) {
	text := "app with a net and a db"
	e := newTestEnv(t, StaticPlanner{hashOf(text): appDesired()})
	e.writeIntent("app", text)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := e.plan()
	e.fake.onPlan = func(_ context.Context, typeName string) {
		if typeName == "test_db" {
			cancel()
		}
	}
	obs := &recordingObserver{}
	e.engine.Observer = obs

	err := e.engine.Apply(ctx, p, approveAll)
	if !errors.Is(err, ErrInterrupted) || err.Error() != "interrupted: finished 1 of 2 operations; run plan again" {
		t.Fatalf("err = %v, want interrupted after 1 of 2", err)
	}
	if e.applyCalls() != 1 {
		t.Fatalf("calls = %v, want only the net applied", e.fake.calls)
	}
	if want := []string{"app/net create ok"}; !equalStrings(obs.ops, want) {
		t.Fatalf("observed ops = %v, want %v", obs.ops, want)
	}
	e.checkConsistent("app")
}

// An interrupt that arrives while the re-plan of the next operation
// completes still starts nothing: the context is checked again right before
// the provider applies.
func TestApplyInterruptAfterReplanStartsNothing(t *testing.T) {
	text := "app with a net and a db"
	e := newTestEnv(t, StaticPlanner{hashOf(text): appDesired()})
	e.writeIntent("app", text)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := e.plan()
	e.fake.planIgnoresCancel = true
	e.fake.onPlan = func(_ context.Context, typeName string) {
		if typeName == "test_db" {
			cancel()
		}
	}
	obs := &recordingObserver{}
	e.engine.Observer = obs

	err := e.engine.Apply(ctx, p, approveAll)
	if !errors.Is(err, ErrInterrupted) || err.Error() != "interrupted: finished 1 of 2 operations; run plan again" {
		t.Fatalf("err = %v, want interrupted after 1 of 2", err)
	}
	if e.applyCalls() != 1 {
		t.Fatalf("calls = %v, want only the net applied", e.fake.calls)
	}
	if want := []string{"app/net create ok"}; !equalStrings(obs.ops, want) {
		t.Fatalf("observed ops = %v, want %v", obs.ops, want)
	}
	e.checkConsistent("app")
}

// A context cancelled before Apply starts writes nothing.
func TestApplyCancelledBeforeStartWritesNothing(t *testing.T) {
	text := "app with a net and a db"
	e := newTestEnv(t, StaticPlanner{hashOf(text): appDesired()})
	e.writeIntent("app", text)
	p := e.plan()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := e.engine.Apply(ctx, p, approveAll)
	if !errors.Is(err, ErrInterrupted) || err.Error() != "interrupted: finished 0 of 2 operations; run plan again" {
		t.Fatalf("err = %v, want interrupted before any operation", err)
	}
	if e.applyCalls() != 0 || e.readFile(e.lockPath) != nil || e.readFile(e.statePath("app")) != nil {
		t.Fatalf("something was written or applied: calls %v", e.fake.calls)
	}
}

// failLockWrites makes every later lock write fail (the lock's directory
// becomes read-only) when the provider applies typeName, as if the process
// died between the state save and the lock save.
func (e *testEnv) failLockWrites(typeName string) {
	root := filepath.Dir(e.lockPath)
	e.fake.onApply = func(_ context.Context, ty string) {
		if ty == typeName {
			if err := os.Chmod(root, 0o500); err != nil {
				e.t.Error(err)
			}
		}
	}
	e.t.Cleanup(func() { os.Chmod(root, 0o755) })
}

func TestRecoverAfterUnwrittenLock(t *testing.T) {
	text := "app with a net and a db"
	e := newTestEnv(t, StaticPlanner{hashOf(text): appDesired()})
	h := e.writeIntent("app", text)
	e.failLockWrites("test_db")

	err := e.apply(e.plan(), approveAll)
	if err == nil || !strings.Contains(err.Error(), "save lock") {
		t.Fatalf("err = %v, want a lock write failure", err)
	}
	if err := os.Chmod(filepath.Dir(e.lockPath), 0o755); err != nil {
		t.Fatal(err)
	}
	e.fake.onApply = nil
	st, _ := e.loadState("app")
	if st.Serial != 2 || st.Resources["db"] == nil {
		t.Fatalf("state serial %d resources %v, want the db saved at serial 2", st.Serial, st.Resources)
	}

	p := e.plan()
	if want := `state of intent "app" is ahead of the lock (serial 2 > 1): run "prosa recover <dir>"`; p.Blocked != want {
		t.Fatalf("Blocked = %q, want %q", p.Blocked, want)
	}

	if _, _, err := e.engine.Check(context.Background()); err == nil || err.Error() != p.Blocked {
		t.Fatalf("Check = %v, want %q", err, p.Blocked)
	}

	got, err := e.engine.Recover()
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if want := []Recovered{{Intent: "app", Resources: 2, Serial: 2}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Recover = %+v, want %+v", got, want)
	}
	li := e.loadLock().Intents["app"]
	if li.IntentHash != "" || len(li.Resources) != 2 || li.Resources[0].Key != "db" || li.Resources[1].Key != "net" {
		t.Fatalf("recovered lock intent = %+v", li)
	}
	db := lockRes(li, "db")
	if db.ID != "test_db-1" || db.Type != "test_db" || db.Provider != "hashicorp/aws" || db.Chosen["net_id"] != "${app/net.id}" {
		t.Fatalf("recovered db = %+v", *db)
	}
	e.checkConsistent("app")
	e.checkRecordsMatchLock("app")

	// The intent is planned again and completes without creating anything.
	p = e.plan()
	if p.Blocked != "" {
		t.Fatalf("still blocked: %s", p.Blocked)
	}
	if got, want := summary(p), []string{"no-op app/net", "no-op app/db"}; !equalStrings(got, want) {
		t.Fatalf("changes = %v, want %v", got, want)
	}
	calls := e.applyCalls()
	e.mustApply(p)
	if e.applyCalls() != calls {
		t.Fatalf("resources applied again: %v", e.fake.calls)
	}
	if li := e.loadLock().Intents["app"]; li.IntentHash != h || len(li.Resources) != 2 {
		t.Fatalf("after the next apply: %+v", li)
	}
	e.checkConsistent("app")
}

// The state of an intent the lock does not have yet (its first save was
// not followed by a lock write) is ahead too: Recover adds its entry.
func TestRecoverIntentMissingFromLock(t *testing.T) {
	e := newTestEnv(t, failingPlanner)
	h := e.writeIntent("base", "a net")
	e.seed("base", h, seedRes{key: "net", typ: "test_net", chosen: map[string]any{"name": "net"},
		state: map[string]any{"id": "test_net-1", "name": "net", "tags": map[string]any{"prosa/intent": "base"}}})
	e.writeIntent("app", "a db")
	rs := &state.ResourceState{Provider: "hashicorp/aws", Type: "test_db", State: json.RawMessage(`{"id":"test_db-1","name":"db"}`),
		ID: "test_db-1", Chosen: map[string]any{"name": "db", "size": json.Number("10")}, DependsOn: []string{"base/net"}}
	if _, err := e.store.Save("app", &state.IntentState{Resources: map[string]*state.ResourceState{"db": rs}}); err != nil {
		t.Fatal(err)
	}
	if p := e.plan(); !strings.HasPrefix(p.Blocked, `state of intent "app" is ahead of the lock (serial 1 > 0)`) {
		t.Fatalf("Blocked = %q", p.Blocked)
	}

	got, err := e.engine.Recover()
	if err != nil {
		t.Fatal(err)
	}
	if want := []Recovered{{Intent: "app", Resources: 1, Serial: 1}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Recover = %+v, want %+v", got, want)
	}
	lk := e.loadLock()
	want := lock.Resource{Key: "db", Provider: "hashicorp/aws", Type: "test_db", ID: "test_db-1",
		Chosen: map[string]any{"name": "db", "size": 10}, DependsOn: []string{"base/net"}}
	if li := lk.Intents["app"]; li == nil || li.IntentHash != "" || len(li.Resources) != 1 || !reflect.DeepEqual(li.Resources[0], want) {
		t.Fatalf("recovered entry = %+v, want %+v", li, want)
	}
	if lk.Intents["base"].IntentHash != h {
		t.Fatalf("base entry changed: %+v", lk.Intents["base"])
	}
	e.checkConsistent("app")
	e.checkConsistent("base")
}

func TestRecoverConsistentProjectHasNothingToDo(t *testing.T) {
	e := newTestEnv(t, failingPlanner)
	h := e.writeIntent("app", "a net")
	e.seed("app", h, seedRes{key: "net", typ: "test_net", chosen: map[string]any{"name": "net"},
		state: map[string]any{"id": "test_net-1", "name": "net", "tags": map[string]any{"prosa/intent": "app"}}})
	before := e.readFile(e.lockPath)

	got, err := e.engine.Recover()
	if err != nil || len(got) != 0 {
		t.Fatalf("Recover = %+v, %v; want nothing to do", got, err)
	}
	if !bytes.Equal(e.readFile(e.lockPath), before) {
		t.Fatal("lock written with nothing to recover")
	}

	// A state behind the lock is not recovered: the generic mismatch stays.
	lk := e.loadLock()
	lk.Intents["app"].State.Serial = 5
	lk.Intents["app"].State.Digest = "sha256:other"
	if _, err := lock.Save(e.lockPath, lk); err != nil {
		t.Fatal(err)
	}
	if p := e.plan(); p.Blocked != `state digest mismatch for intent "app"` {
		t.Fatalf("Blocked = %q, want the generic mismatch", p.Blocked)
	}
	if got, err := e.engine.Recover(); err != nil || len(got) != 0 {
		t.Fatalf("Recover = %+v, %v; want nothing to do", got, err)
	}

	// Recover takes the project lock, like Apply.
	unlock, err := e.store.Lock(applyLockName)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if _, err := e.engine.Recover(); err == nil || !strings.Contains(err.Error(), "another apply is running") {
		t.Fatalf("Recover with the project lock held: %v", err)
	}
}

// recordingObserver records the operations Apply reports, as
// "<intent>/<key> <action> <ok|failure kind>".
type recordingObserver struct{ ops []string }

func (o *recordingObserver) Planned(*Plan)             {}
func (o *recordingObserver) Approval(bool, []string)   {}
func (o *recordingObserver) Check(CheckResult, string) {}
func (o *recordingObserver) ApplyOp(intent, key string, action Action, _ string, failure *ErrorInfo) {
	result := "ok"
	if failure != nil {
		result = failure.Kind
	}
	o.ops = append(o.ops, intent+"/"+key+" "+string(action)+" "+result)
}

// Records written before they kept the lock entry take chosen and
// depends_on from the lock entry of their key.
func TestRecoverTakesOldRecordsEntryFromLock(t *testing.T) {
	e := newTestEnv(t, failingPlanner)
	h := e.writeIntent("app", "a net and a db")
	e.seed("app", h, // seed writes records in the old format
		seedRes{key: "net", typ: "test_net", chosen: map[string]any{"name": "net"},
			state: map[string]any{"id": "test_net-1", "name": "net"}},
		seedRes{key: "db", typ: "test_db", chosen: map[string]any{"name": "db", "size": 10}, dependsOn: []string{"app/net"},
			state: map[string]any{"id": "test_db-1", "name": "db", "size": 10}})
	is, _ := e.loadState("app")
	if _, err := e.store.Save("app", is); err != nil { // a save the lock never saw
		t.Fatal(err)
	}

	if _, err := e.engine.Recover(); err != nil {
		t.Fatal(err)
	}
	li := e.loadLock().Intents["app"]
	want := []lock.Resource{
		{Key: "db", Provider: "hashicorp/aws", Type: "test_db", ID: "test_db-1", Chosen: map[string]any{"name": "db", "size": 10}, DependsOn: []string{"app/net"}},
		{Key: "net", Provider: "hashicorp/aws", Type: "test_net", ID: "test_net-1", Chosen: map[string]any{"name": "net"}},
	}
	if li.IntentHash != "" || !reflect.DeepEqual(li.Resources, want) {
		t.Fatalf("recovered entry = %+v, want resources %+v", li, want)
	}
	e.checkConsistent("app")
}

// A stop between the state save and the lock save of a replace's destroy
// half: the store no longer has the resource, the rebuilt lock keeps its key
// without an id (as Apply does), and the next run creates it.
func TestRecoverInterruptedReplace(t *testing.T) {
	text := "renamed net"
	d := Desired{Resources: []DesiredResource{{Key: "net", Type: "test_net", Config: map[string]any{"name": "net2"}}}}
	e := newTestEnv(t, StaticPlanner{hashOf(text): d})
	h := e.writeIntent("app", text)
	e.seed("app", hashOf("net"), seedRes{key: "net", typ: "test_net", chosen: map[string]any{"name": "net"},
		state: map[string]any{"id": "test_net-1", "name": "net", "tags": map[string]any{"prosa/intent": "app"}}})
	e.fake.replaceOn["test_net"] = []string{"name"}
	p := e.plan()
	if got, want := summary(p), []string{"replace app/net"}; !equalStrings(got, want) {
		t.Fatalf("changes = %v, want %v", got, want)
	}
	e.failLockWrites("test_net") // the destroy is saved in the store only
	if err := e.apply(p, approveAll); err == nil || !strings.Contains(err.Error(), "save lock") {
		t.Fatalf("err = %v, want a lock write failure", err)
	}
	if err := os.Chmod(filepath.Dir(e.lockPath), 0o755); err != nil {
		t.Fatal(err)
	}
	e.fake.onApply = nil
	if st, _ := e.loadState("app"); len(st.Resources) != 0 {
		t.Fatalf("state resources = %v, want none after the destroy", st.Resources)
	}

	got, err := e.engine.Recover()
	if err != nil {
		t.Fatal(err)
	}
	if want := []Recovered{{Intent: "app", Resources: 1, Serial: 2}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Recover = %+v, want %+v", got, want)
	}
	li := e.loadLock().Intents["app"]
	want := lock.Resource{Key: "net", Provider: "hashicorp/aws", Type: "test_net", Chosen: map[string]any{"name": "net"}}
	if li.IntentHash != "" || len(li.Resources) != 1 || !reflect.DeepEqual(li.Resources[0], want) {
		t.Fatalf("recovered entry = %+v, want %+v", li, want)
	}
	e.checkConsistent("app")

	p = e.plan()
	if got, want := summary(p), []string{"create app/net"}; !equalStrings(got, want) {
		t.Fatalf("changes after recover = %v, want %v", got, want)
	}
	e.mustApply(p)
	if li := e.loadLock().Intents["app"]; li.IntentHash != h || len(li.Resources) != 1 || li.Resources[0].ID != "test_net-1" || li.Resources[0].Chosen["name"] != "net2" {
		t.Fatalf("after the next apply: %+v", li)
	}
	e.checkConsistent("app")
}

// InFlight names the provider operation in progress, until its state is
// saved; between operations, and after Apply, it is empty.
func TestApplyReportsOperationInFlight(t *testing.T) {
	text := "app with a net and a db"
	e := newTestEnv(t, StaticPlanner{hashOf(text): appDesired()})
	e.writeIntent("app", text)
	e.engine.InFlight = &InFlight{}
	var seen []string
	e.fake.onApply = func(_ context.Context, typeName string) {
		seen = append(seen, e.engine.InFlight.Current())
	}
	e.fake.onPlan = func(context.Context, string) {
		if cur := e.engine.InFlight.Current(); cur != "" {
			t.Errorf("in flight during a plan: %q", cur)
		}
	}
	e.mustApply(e.plan())
	if want := []string{"app/net (create)", "app/db (create)"}; !equalStrings(seen, want) {
		t.Fatalf("in flight = %v, want %v", seen, want)
	}
	if cur := e.engine.InFlight.Current(); cur != "" {
		t.Fatalf("in flight after Apply = %q", cur)
	}

	// A delete, and both halves of a replace, are named too.
	e.fake.replaceOn["test_net"] = []string{"name"}
	e.engine.Planner = StaticPlanner{hashOf("v2"): {Resources: []DesiredResource{{Key: "net", Type: "test_net", Config: map[string]any{"name": "net2"}}}}}
	e.writeIntent("app", "v2")
	seen = nil
	e.mustApply(e.plan())
	if want := []string{"app/db (delete)", "app/net (replace)", "app/net (replace)"}; !equalStrings(seen, want) {
		t.Fatalf("in flight = %v, want %v", seen, want)
	}
	var nilFlight *InFlight
	if nilFlight.Current() != "" {
		t.Fatal("a nil InFlight reports an operation")
	}
}

// pendingDesired is appDesired with a depends_on and a check, as the
// planner answers it.
func pendingDesired() Desired {
	d := appDesired()
	d.Resources[0].DependsOn = []string{"app/net"}
	d.Checks = []lock.Check{{Name: "The net is called net.", Type: "resource_attr", Resource: "app/net", Path: "name", Op: "eq", Value: "net"}}
	return d
}

// interruptAfterNet plans and applies the app intent of text, interrupted
// while the net is created: the db is left.
func (e *testEnv) interruptAfterNet() {
	e.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.fake.onApply = func(_ context.Context, typeName string) {
		if typeName == "test_net" {
			cancel()
		}
	}
	if err := e.engine.Apply(ctx, e.plan(), approveAll); !errors.Is(err, ErrInterrupted) {
		e.t.Fatalf("err = %v, want ErrInterrupted", err)
	}
	e.fake.onApply = nil
}

// An interrupted apply keeps the desired state it was executing in the
// state store; the next plan of the same text resumes it without asking the
// planner, and completing the intent clears it.
func TestResumeUsesPendingDesiredWithoutPlanner(t *testing.T) {
	text := "app with a net and a db"
	calls := 0
	e := newTestEnv(t, plannerFunc(func(context.Context, PlanInput) (Desired, error) {
		calls++
		return pendingDesired(), nil
	}))
	h := e.writeIntent("app", text)
	e.interruptAfterNet()
	if calls != 1 {
		t.Fatalf("planner calls = %d, want 1", calls)
	}
	st, _ := e.loadState("app")
	if st.Pending == nil || st.Pending.IntentHash != h {
		t.Fatalf("pending = %+v, want the desired state of hash %s", st.Pending, h)
	}

	e.engine.Planner = failingPlanner
	p := e.plan()
	if got, want := summary(p), []string{"no-op app/net", "create app/db"}; !equalStrings(got, want) {
		t.Fatalf("changes = %v, want %v", got, want)
	}
	e.mustApply(p)
	li := e.loadLock().Intents["app"]
	want := canonicalFake(pendingDesired())
	if li.IntentHash != h || !sameJSON(li.Checks, want.Checks) {
		t.Fatalf("lock = %+v, want hash %s and the planned checks", li, h)
	}
	db := lockRes(li, "db")
	if db == nil || !equalStrings(db.DependsOn, []string{"app/net"}) || !sameJSON(db.Chosen, want.Resources[0].Config) {
		t.Fatalf("lock db = %+v, want the planned one", db)
	}
	if st, _ := e.loadState("app"); st.Pending != nil {
		t.Fatalf("pending after completion = %+v, want none", st.Pending)
	}
	e.checkConsistent("app")

	// Nothing left: the next plan writes nothing and asks nobody.
	before := e.readFile(e.statePath("app"))
	e.mustApply(e.plan())
	if !bytes.Equal(before, e.readFile(e.statePath("app"))) {
		t.Fatal("a run without changes rewrote the state")
	}
}

// canonicalFake returns d as a plan with the fake provider keeps it.
func canonicalFake(d Desired) Desired {
	return (&Plan{schema: newFakeProvider().schema}).canonical(d)
}

// An intent edited after the interruption is planned again.
func TestResumeReplansEditedIntent(t *testing.T) {
	calls := 0
	e := newTestEnv(t, plannerFunc(func(context.Context, PlanInput) (Desired, error) {
		calls++
		return pendingDesired(), nil
	}))
	e.writeIntent("app", "app with a net and a db")
	e.interruptAfterNet()
	e.writeIntent("app", "app with a net and a bigger db")
	e.plan()
	if calls != 2 {
		t.Fatalf("planner calls = %d, want 2 (the edited intent is planned again)", calls)
	}
}

// A pending desired state that no longer validates (here the provider now
// rejects the db at plan time) goes back to the planner.
func TestResumeInvalidPendingAsksPlanner(t *testing.T) {
	calls := 0
	e := newTestEnv(t, plannerFunc(func(context.Context, PlanInput) (Desired, error) {
		calls++
		return pendingDesired(), nil
	}))
	e.writeIntent("app", "app with a net and a db")
	e.interruptAfterNet()
	rejected := 0
	e.fake.planErr = func(typeName string, _ cty.Value) error {
		if typeName == "test_db" && rejected == 0 {
			rejected++
			return errors.New("plan test_db: size is not allowed today")
		}
		return nil
	}
	if got, want := summary(e.plan()), []string{"no-op app/net", "create app/db"}; !equalStrings(got, want) {
		t.Fatalf("changes = %v, want %v", got, want)
	}
	if calls != 2 || rejected != 1 {
		t.Fatalf("planner calls = %d, rejections %d; want 2 and 1", calls, rejected)
	}
}

// A plan error on an intent resumed from its Pending says how to plan it
// again; --no-cache (Engine.NoResume) does so without the resume.
func TestResumedPlanErrorHintsNoCache(t *testing.T) {
	calls := 0
	e := newTestEnv(t, plannerFunc(func(context.Context, PlanInput) (Desired, error) {
		calls++
		return pendingDesired(), nil
	}))
	e.writeIntent("app", "app with a net and a db")
	e.interruptAfterNet()
	reject := &provider.DiagError{Op: "plan", Sum: "size is not allowed today", Attr: "size"}
	seen := 0
	e.fake.planErr = func(typeName string, _ cty.Value) error {
		// The first test_db plan is the validation of the pending state,
		// which passes; the one of the plan itself is rejected.
		if typeName == "test_db" {
			if seen++; seen > 1 {
				return reject
			}
		}
		return nil
	}
	before := calls
	_, err := e.engine.Plan(context.Background())
	if err == nil || !errors.Is(err, reject) || !strings.Contains(err.Error(), "(resumed from the interrupted apply; run with --no-cache to plan it again)") {
		t.Fatalf("err = %v, want the rejection with the --no-cache hint", err)
	}
	if calls != before {
		t.Fatalf("planner called %d times while resuming", calls-before)
	}

	// An error that is not the provider's rejecting the answer (a network
	// failure, say) is not fixed by planning again: no hint.
	seen = 0
	e.fake.planErr = func(typeName string, _ cty.Value) error {
		if typeName == "test_db" {
			if seen++; seen > 1 {
				return errors.New("connection reset")
			}
		}
		return nil
	}
	_, err = e.engine.Plan(context.Background())
	if err == nil || !strings.Contains(err.Error(), "connection reset") || strings.Contains(err.Error(), "--no-cache") {
		t.Fatalf("err = %v, want the connection error without the hint", err)
	}

	// With NoResume the planner is asked and the error has no hint.
	e.fake.planErr = func(typeName string, _ cty.Value) error {
		if typeName == "test_db" {
			if seen++; seen > 1 {
				return reject
			}
		}
		return nil
	}
	seen = 1 // every test_db plan is rejected, as in the first part
	e.engine.NoResume = true
	_, err = e.engine.Plan(context.Background())
	if err == nil || strings.Contains(err.Error(), "--no-cache") {
		t.Fatalf("err = %v, want the rejection without the hint", err)
	}
	if calls != before+1 {
		t.Fatalf("planner calls = %d, want %d (no resume)", calls, before+1)
	}
}
