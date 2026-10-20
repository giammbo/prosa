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
	"time"

	"github.com/zclconf/go-cty/cty"

	"github.com/giammbo/prosa/internal/lock"
	"github.com/giammbo/prosa/internal/state"
)

func approveAll(*Plan) bool { return true }

func (e *testEnv) apply(p *Plan, approve func(*Plan) bool) error {
	e.t.Helper()
	return e.engine.Apply(context.Background(), p, approve)
}

func (e *testEnv) mustApply(p *Plan) {
	e.t.Helper()
	if err := e.apply(p, approveAll); err != nil {
		e.t.Fatalf("Apply: %v", err)
	}
}

func (e *testEnv) loadLock() *lock.Lock {
	e.t.Helper()
	lk, err := lock.Load(e.lockPath)
	if err != nil {
		e.t.Fatal(err)
	}
	return lk
}

func (e *testEnv) loadState(intentName string) (*state.IntentState, string) {
	e.t.Helper()
	st, digest, err := e.store.Load(intentName)
	if err != nil {
		e.t.Fatal(err)
	}
	return st, digest
}

func (e *testEnv) statePath(intentName string) string {
	return filepath.Join(filepath.Dir(e.lockPath), ".prosa", "state", intentName+".state")
}

// readFile returns the bytes of path, nil when it does not exist.
func (e *testEnv) readFile(path string) []byte {
	e.t.Helper()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		e.t.Fatal(err)
	}
	return data
}

// applyCalls counts the ApplyResource calls the fake received.
func (e *testEnv) applyCalls() int {
	n := 0
	for _, c := range e.fake.calls {
		if strings.HasPrefix(c, "apply ") {
			n++
		}
	}
	return n
}

// stateAttrs decodes the stored state of a resource as JSON.
func stateAttrs(t *testing.T, rs *state.ResourceState) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rs.State, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func lockRes(li *lock.Intent, key string) *lock.Resource {
	for i := range li.Resources {
		if li.Resources[i].Key == key {
			return &li.Resources[i]
		}
	}
	return nil
}

// checkConsistent asserts that the lock's state reference matches the store.
func (e *testEnv) checkConsistent(intentName string) {
	e.t.Helper()
	st, digest := e.loadState(intentName)
	li := e.loadLock().Intents[intentName]
	if li == nil {
		e.t.Fatalf("intent %q not in the lock", intentName)
	}
	if li.State.Digest != digest || li.State.Serial != st.Serial {
		e.t.Fatalf("lock state ref %+v, store serial %d digest %s", li.State, st.Serial, digest)
	}
}

func TestApplyCreatesAndWritesLockAndState(t *testing.T) {
	text := "app with a net and a db"
	e := newTestEnv(t, StaticPlanner{hashOf(text): appDesired()})
	h := e.writeIntent("app", text)

	called := false
	if err := e.apply(e.plan(), func(*Plan) bool { called = true; return true }); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !called {
		t.Fatal("approve not called")
	}

	lk := e.loadLock()
	if got := lk.ProviderVersions["hashicorp/aws"]; got != "1.0.0" {
		t.Fatalf("provider version = %q", got)
	}
	li := lk.Intents["app"]
	if li == nil || len(li.Resources) != 2 {
		t.Fatalf("lock intent = %+v", li)
	}
	if li.IntentHash != h || !li.ReconciledAt.Equal(time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("intent_hash %q reconciled_at %v", li.IntentHash, li.ReconciledAt)
	}
	net, db := lockRes(li, "net"), lockRes(li, "db")
	if net == nil || db == nil || net.ID != "test_net-1" || db.ID != "test_db-1" {
		t.Fatalf("lock resources = %+v", li.Resources)
	}
	if db.Provider != "hashicorp/aws" || db.Type != "test_db" || db.Chosen["net_id"] != "${app/net.id}" {
		t.Fatalf("db lock entry = %+v", db)
	}
	if _, ok := db.Chosen["tags"]; ok {
		t.Fatalf("ownership tag in chosen: %v", db.Chosen)
	}
	e.checkConsistent("app")

	st, _ := e.loadState("app")
	dbState := stateAttrs(t, st.Resources["db"])
	if dbState["net_id"] != "test_net-1" || dbState["id"] != "test_db-1" {
		t.Fatalf("db state = %v", dbState)
	}
	if tags, _ := dbState["tags"].(map[string]any); tags["prosa/intent"] != "app" {
		t.Fatalf("db tags = %v", dbState["tags"])
	}
	if len(e.fake.remote) != 2 {
		t.Fatalf("remote = %v", e.fake.remote)
	}
}

func TestApplyNoChangesWritesNothing(t *testing.T) {
	text := "app with a net and a db"
	e := newTestEnv(t, StaticPlanner{hashOf(text): appDesired()})
	e.writeIntent("app", text)
	e.mustApply(e.plan())
	lockBefore, stateBefore := e.readFile(e.lockPath), e.readFile(e.statePath("app"))
	applies := e.applyCalls()

	p := e.plan()
	if p.HasChanges() || p.LockDirty {
		t.Fatalf("second plan: %v LockDirty=%v", summary(p), p.LockDirty)
	}
	if err := e.apply(p, func(*Plan) bool { t.Error("approve called without changes"); return true }); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !bytes.Equal(e.readFile(e.lockPath), lockBefore) || !bytes.Equal(e.readFile(e.statePath("app")), stateBefore) {
		t.Fatal("a run without changes rewrote the lock or the state")
	}
	if e.applyCalls() != applies {
		t.Fatalf("ApplyResource called without changes: %v", e.fake.calls)
	}
}

func TestApplyNotApproved(t *testing.T) {
	text := "app with a net and a db"
	e := newTestEnv(t, StaticPlanner{hashOf(text): appDesired()})
	e.writeIntent("app", text)

	err := e.apply(e.plan(), func(*Plan) bool { return false })
	if !errors.Is(err, ErrNotApproved) {
		t.Fatalf("err = %v, want ErrNotApproved", err)
	}
	if e.readFile(e.lockPath) != nil || e.readFile(e.statePath("app")) != nil {
		t.Fatal("files written for a plan that was not approved")
	}
	if e.applyCalls() != 0 {
		t.Fatalf("ApplyResource called: %v", e.fake.calls)
	}
}

func TestApplyPartialFailureKeepsLockAndStateConsistent(t *testing.T) {
	text := "app with a net and a db"
	e := newTestEnv(t, StaticPlanner{hashOf(text): appDesired()})
	h := e.writeIntent("app", text)
	e.fake.applyErr = func(typeName string, _ cty.Value) error {
		if typeName == "test_db" {
			return errors.New("db quota exceeded")
		}
		return nil
	}

	err := e.apply(e.plan(), approveAll)
	if err == nil || !strings.Contains(err.Error(), "db quota exceeded") || !strings.Contains(err.Error(), "app/db") {
		t.Fatalf("err = %v, want the db failure", err)
	}
	li := e.loadLock().Intents["app"]
	if li == nil || len(li.Resources) != 1 || li.Resources[0].Key != "net" || li.Resources[0].ID != "test_net-1" {
		t.Fatalf("lock intent = %+v", li)
	}
	if li.IntentHash != "" {
		t.Fatalf("intent_hash = %q, want the old one (empty)", li.IntentHash)
	}
	st, _ := e.loadState("app")
	if len(st.Resources) != 1 || st.Resources["net"] == nil {
		t.Fatalf("state resources = %v", st.Resources)
	}
	e.checkConsistent("app")

	// The next run resumes: only the db is left to create.
	e.fake.applyErr = nil
	p := e.plan()
	if got, want := summary(p), []string{"no-op app/net", "create app/db"}; !equalStrings(got, want) {
		t.Fatalf("changes = %v, want %v", got, want)
	}
	e.mustApply(p)
	li = e.loadLock().Intents["app"]
	if li.IntentHash != h || len(li.Resources) != 2 {
		t.Fatalf("after resume: %+v", li)
	}
	e.checkConsistent("app")
}

func TestApplyPartialStateIsPersisted(t *testing.T) {
	text := "app with a net and a db"
	e := newTestEnv(t, StaticPlanner{hashOf(text): appDesired()})
	e.writeIntent("app", text)
	e.fake.partialErr = func(typeName string) error {
		if typeName == "test_db" {
			return errors.New("db created but not ready")
		}
		return nil
	}

	err := e.apply(e.plan(), approveAll)
	if err == nil || !strings.Contains(err.Error(), "db created but not ready") {
		t.Fatalf("err = %v", err)
	}
	st, _ := e.loadState("app")
	if st.Resources["db"] == nil || stateAttrs(t, st.Resources["db"])["id"] != "test_db-1" {
		t.Fatalf("partial db state not kept: %v", st.Resources)
	}
	li := e.loadLock().Intents["app"]
	if db := lockRes(li, "db"); db == nil || db.ID != "test_db-1" {
		t.Fatalf("lock resources = %+v", li.Resources)
	}
	if li.IntentHash != "" {
		t.Fatalf("intent_hash = %q after a failure", li.IntentHash)
	}
	e.checkConsistent("app")
}

func TestApplyReplanDetectsChangedAction(t *testing.T) {
	text := "renamed net"
	d := Desired{Resources: []DesiredResource{{Key: "net", Type: "test_net", Config: map[string]any{"name": "net2"}}}}
	e := newTestEnv(t, StaticPlanner{hashOf(text): d})
	e.writeIntent("app", text)
	e.seed("app", hashOf("net"), seedRes{key: "net", typ: "test_net", chosen: map[string]any{"name": "net"},
		state: map[string]any{"id": "test_net-1", "name": "net", "tags": map[string]any{"prosa/intent": "app"}}})
	lockBefore, stateBefore := e.readFile(e.lockPath), e.readFile(e.statePath("app"))

	p := e.plan()
	if got, want := summary(p), []string{"update app/net"}; !equalStrings(got, want) {
		t.Fatalf("changes = %v, want %v", got, want)
	}
	e.fake.replaceOn["test_net"] = []string{"name"} // the provider changed its mind

	err := e.apply(p, approveAll)
	if err == nil || !strings.Contains(err.Error(), "plan changed for app/net: was update, now replace") {
		t.Fatalf("err = %v", err)
	}
	if e.applyCalls() != 0 {
		t.Fatalf("ApplyResource called: %v", e.fake.calls)
	}
	if !bytes.Equal(e.readFile(e.lockPath), lockBefore) || !bytes.Equal(e.readFile(e.statePath("app")), stateBefore) {
		t.Fatal("lock or state written after a changed plan")
	}
}

func TestApplyRemovedIntentCleansUp(t *testing.T) {
	e := newTestEnv(t, failingPlanner)
	owner := map[string]any{"prosa/intent": "old"}
	e.seed("old", hashOf("old text"),
		seedRes{key: "net", typ: "test_net", chosen: map[string]any{"name": "net"},
			state: map[string]any{"id": "test_net-1", "name": "net", "tags": owner}},
		seedRes{key: "db", typ: "test_db", chosen: map[string]any{"name": "db", "net_id": "${old/net.id}"},
			state: map[string]any{"id": "test_db-1", "name": "db", "net_id": "test_net-1", "tags": owner}},
	)

	e.mustApply(e.plan())
	if _, ok := e.loadLock().Intents["old"]; ok {
		t.Fatal("removed intent still in the lock")
	}
	if e.readFile(e.statePath("old")) != nil {
		t.Fatal("state file of the removed intent still exists")
	}
	if len(e.fake.remote) != 0 {
		t.Fatalf("remote = %v", e.fake.remote)
	}
	var applies []string
	for _, c := range e.fake.calls {
		if strings.HasPrefix(c, "apply ") {
			applies = append(applies, c)
		}
	}
	if want := []string{"apply test_db", "apply test_net"}; !equalStrings(applies, want) {
		t.Fatalf("applies = %v, want %v", applies, want)
	}
}

func TestApplyLockDirtyOnlyUpdatesHash(t *testing.T) {
	text := "the same net, said differently"
	d := Desired{Resources: []DesiredResource{{Key: "net", Type: "test_net", Config: map[string]any{"name": "net"}}}}
	e := newTestEnv(t, StaticPlanner{hashOf(text): d})
	h := e.writeIntent("app", text)
	e.seed("app", hashOf("a net"), seedRes{key: "net", typ: "test_net", chosen: map[string]any{"name": "net"},
		state: map[string]any{"id": "test_net-1", "name": "net", "tags": map[string]any{"prosa/intent": "app"}}})
	stateBefore, _ := e.loadState("app")

	p := e.plan()
	if p.HasChanges() || !p.LockDirty {
		t.Fatalf("plan: %v LockDirty=%v", summary(p), p.LockDirty)
	}
	if err := e.apply(p, func(*Plan) bool { t.Error("approve called without infrastructure changes"); return false }); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	li := e.loadLock().Intents["app"]
	if li.IntentHash != h || li.ReconciledAt.IsZero() || len(li.Resources) != 1 || li.Resources[0].ID != "test_net-1" {
		t.Fatalf("lock intent = %+v", li)
	}
	if e.applyCalls() != 0 {
		t.Fatal("resources applied for a lock-only change")
	}
	e.checkSameResources("app", stateBefore, text)
}

// checkSameResources asserts that the intent's stored resources are those of
// before, that its state now holds the intent's text and that the lock points
// at it: a completed intent whose text changed only records the new text,
// and in its records the lock entries written with them (ADR-024).
func (e *testEnv) checkSameResources(intentName string, before *state.IntentState, body string) {
	e.t.Helper()
	after, _ := e.loadState(intentName)
	if len(after.Resources) != len(before.Resources) {
		e.t.Fatalf("resources = %v, want %v", after.Resources, before.Resources)
	}
	withoutLockEntry := func(rs *state.ResourceState) *state.ResourceState {
		c := *rs
		c.Chosen, c.DependsOn, c.ID = nil, nil, ""
		return &c
	}
	for k, rs := range before.Resources {
		if a := after.Resources[k]; a == nil || !sameResourceState(withoutLockEntry(a), withoutLockEntry(rs)) {
			e.t.Fatalf("resource %s rewritten", k)
		}
	}
	if after.Body != body {
		e.t.Fatalf("Body = %q, want %q", after.Body, body)
	}
	e.checkConsistent(intentName)
	e.checkRecordsMatchLock(intentName)
}

func TestApplyReplaceDestroysThenCreates(t *testing.T) {
	text := "renamed net"
	d := Desired{Resources: []DesiredResource{{Key: "net", Type: "test_net", Config: map[string]any{"name": "net2"}}}}
	e := newTestEnv(t, StaticPlanner{hashOf(text): d})
	e.fake.replaceOn["test_net"] = []string{"name"}
	e.fake.nextID["test_net"] = 1
	h := e.writeIntent("app", text)
	e.seed("app", hashOf("net"), seedRes{key: "net", typ: "test_net", chosen: map[string]any{"name": "net"},
		state: map[string]any{"id": "test_net-1", "name": "net", "tags": map[string]any{"prosa/intent": "app"}}})

	p := e.plan()
	if got, want := summary(p), []string{"replace app/net"}; !equalStrings(got, want) {
		t.Fatalf("changes = %v, want %v", got, want)
	}
	e.mustApply(p)
	if _, ok := e.fake.remote["test_net-1"]; ok || len(e.fake.remote) != 1 || e.fake.remote["test_net-2"] == cty.NilVal {
		t.Fatalf("remote = %v", e.fake.remote)
	}
	li := e.loadLock().Intents["app"]
	if li.IntentHash != h || len(li.Resources) != 1 || li.Resources[0].ID != "test_net-2" || li.Resources[0].Chosen["name"] != "net2" {
		t.Fatalf("lock intent = %+v", li)
	}
	st, _ := e.loadState("app")
	if got := stateAttrs(t, st.Resources["net"]); got["id"] != "test_net-2" || got["name"] != "net2" {
		t.Fatalf("state = %v", got)
	}
	e.checkConsistent("app")
}

func TestApplyWriteOnlyValueNotPersisted(t *testing.T) {
	text := "a secret"
	d := Desired{Resources: []DesiredResource{
		{Key: "tok", Type: "test_token", Config: map[string]any{"name": "tok"}},
		{Key: "s", Type: "test_secret", Config: map[string]any{"name": "s", "value_wo": "${app/tok.value}"}},
	}}
	e := newTestEnv(t, StaticPlanner{hashOf(text): d})
	e.writeIntent("app", text)
	e.fake.leakWriteOnly = true

	err := e.apply(e.plan(), approveAll)
	if err == nil || !strings.Contains(err.Error(), "write-only") || !strings.Contains(err.Error(), "value_wo") {
		t.Fatalf("err = %v, want a write-only error naming value_wo", err)
	}
	st, _ := e.loadState("app")
	rs := st.Resources["s"]
	if rs == nil || rs.SchemaVersion != 2 {
		t.Fatalf("secret state = %+v", rs)
	}
	if strings.Contains(string(rs.State), "test_token-1-value") {
		t.Fatalf("write-only value persisted: %s", rs.State)
	}
	if got := stateAttrs(t, rs); got["value_wo"] != nil || got["id"] != "test_secret-1" {
		t.Fatalf("state = %v", got)
	}
	e.checkConsistent("app")
}

func TestApplyBlockedPlan(t *testing.T) {
	e := newTestEnv(t, failingPlanner)
	err := e.apply(&Plan{Blocked: `state digest mismatch for intent "app"`}, func(*Plan) bool {
		t.Error("approve called for a blocked plan")
		return true
	})
	if err == nil || !strings.Contains(err.Error(), `state digest mismatch for intent "app"`) {
		t.Fatalf("err = %v", err)
	}
}

func TestApplyRespectsStateLock(t *testing.T) {
	text := "app with a net and a db"
	e := newTestEnv(t, StaticPlanner{hashOf(text): appDesired()})
	e.writeIntent("app", text)
	p := e.plan()
	unlock, err := e.store.Lock("app")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	if err := e.apply(p, approveAll); err == nil || !strings.Contains(err.Error(), "locked") {
		t.Fatalf("err = %v, want a lock error", err)
	}
	if e.applyCalls() != 0 {
		t.Fatalf("ApplyResource called: %v", e.fake.calls)
	}
}

func TestApplyStateChangedSincePlan(t *testing.T) {
	e := newTestEnv(t, StaticPlanner{hashOf("a bigger net"): {Resources: []DesiredResource{
		{Key: "net", Type: "test_net", Config: map[string]any{"name": "net2"}},
	}}})
	e.writeIntent("app", "a bigger net")
	e.seed("app", hashOf("a net"), seedRes{key: "net", typ: "test_net", chosen: map[string]any{"name": "net"},
		state: map[string]any{"id": "test_net-1", "name": "net", "tags": map[string]any{"prosa/intent": "app"}}})
	p := e.plan()
	is, _ := e.loadState("app")
	if _, err := e.store.Save("app", is); err != nil { // another run wrote the state
		t.Fatal(err)
	}

	err := e.apply(p, approveAll)
	if err == nil || !strings.Contains(err.Error(), `state of intent "app" changed since the plan`) {
		t.Fatalf("err = %v", err)
	}
	if e.applyCalls() != 0 {
		t.Fatalf("ApplyResource called: %v", e.fake.calls)
	}
}

// logApplies records each ApplyResource as "C <type> <name>" (create or
// update) or "D <type>" (destroy).
func (e *testEnv) logApplies() *[]string {
	var log []string
	e.fake.applyErr = func(typeName string, planned cty.Value) error {
		if planned.IsNull() {
			log = append(log, "D "+typeName)
		} else {
			log = append(log, "C "+typeName+" "+planned.GetAttr("name").AsString())
		}
		return nil
	}
	return &log
}

func TestApplyReplaceOrdersDestroysBeforeCreates(t *testing.T) {
	text := "net and db, both renamed"
	d := Desired{Resources: []DesiredResource{
		{Key: "net", Type: "test_net", Config: map[string]any{"name": "net2"}},
		{Key: "db", Type: "test_db", Config: map[string]any{"name": "db2", "net_id": "${app/net.id}"}},
	}}
	e := newTestEnv(t, StaticPlanner{hashOf(text): d})
	e.fake.replaceOn["test_net"] = []string{"name"}
	e.fake.replaceOn["test_db"] = []string{"name"}
	e.fake.nextID["test_net"], e.fake.nextID["test_db"] = 1, 1
	e.writeIntent("app", text)
	owner := map[string]any{"prosa/intent": "app"}
	e.seed("app", hashOf("net and db"),
		seedRes{key: "net", typ: "test_net", chosen: map[string]any{"name": "net"},
			state: map[string]any{"id": "test_net-1", "name": "net", "tags": owner}},
		seedRes{key: "db", typ: "test_db", chosen: map[string]any{"name": "db", "net_id": "${app/net.id}"},
			state: map[string]any{"id": "test_db-1", "name": "db", "net_id": "test_net-1", "tags": owner}},
	)

	p := e.plan()
	if got, want := summary(p), []string{"replace app/net", "replace app/db"}; !equalStrings(got, want) {
		t.Fatalf("changes = %v, want %v", got, want)
	}
	log := e.logApplies()
	e.mustApply(p)
	if want := []string{"D test_db", "D test_net", "C test_net net2", "C test_db db2"}; !equalStrings(*log, want) {
		t.Fatalf("applies = %v, want %v", *log, want)
	}
	st, _ := e.loadState("app")
	if got := stateAttrs(t, st.Resources["db"]); got["net_id"] != "test_net-2" || got["id"] != "test_db-2" {
		t.Fatalf("db state = %v, want net_id test_net-2", got)
	}
	e.checkConsistent("app")
}

func TestApplyDeleteAfterDependentMovesOff(t *testing.T) {
	text := "db on a new net"
	d := Desired{Resources: []DesiredResource{
		{Key: "net_b", Type: "test_net", Config: map[string]any{"name": "net_b"}},
		{Key: "db", Type: "test_db", Config: map[string]any{"name": "db", "net_id": "${app/net_b.id}"}},
	}}
	e := newTestEnv(t, StaticPlanner{hashOf(text): d})
	e.fake.nextID["test_net"] = 1
	e.writeIntent("app", text)
	owner := map[string]any{"prosa/intent": "app"}
	e.seed("app", hashOf("db on net a"),
		seedRes{key: "net_a", typ: "test_net", chosen: map[string]any{"name": "net_a"},
			state: map[string]any{"id": "test_net-1", "name": "net_a", "tags": owner}},
		seedRes{key: "db", typ: "test_db", chosen: map[string]any{"name": "db", "net_id": "${app/net_a.id}"},
			state: map[string]any{"id": "test_db-1", "name": "db", "net_id": "test_net-1", "tags": owner}},
	)

	p := e.plan()
	if got, want := summary(p), []string{"delete app/net_a", "create app/net_b", "update app/db"}; !equalStrings(got, want) {
		t.Fatalf("changes = %v, want %v", got, want)
	}
	log := e.logApplies()
	e.mustApply(p)
	if want := []string{"C test_net net_b", "C test_db db", "D test_net"}; !equalStrings(*log, want) {
		t.Fatalf("applies = %v, want %v", *log, want)
	}
	st, _ := e.loadState("app")
	if got := stateAttrs(t, st.Resources["db"]); got["net_id"] != "test_net-2" {
		t.Fatalf("db state = %v", got)
	}
	if _, ok := st.Resources["net_a"]; ok {
		t.Fatal("net_a still in the state")
	}
	e.checkConsistent("app")
}

func TestApplyReplanDowngradeToNoOpSkips(t *testing.T) {
	text := "renamed net"
	d := Desired{Resources: []DesiredResource{{Key: "net", Type: "test_net", Config: map[string]any{"name": "net2"}}}}
	e := newTestEnv(t, StaticPlanner{hashOf(text): d})
	h := e.writeIntent("app", text)
	e.seed("app", hashOf("net"), seedRes{key: "net", typ: "test_net", chosen: map[string]any{"name": "net"},
		state: map[string]any{"id": "test_net-1", "name": "net", "tags": map[string]any{"prosa/intent": "app"}}})
	stateBefore, _ := e.loadState("app")

	p := e.plan()
	if got, want := summary(p), []string{"update app/net"}; !equalStrings(got, want) {
		t.Fatalf("changes = %v, want %v", got, want)
	}
	e.fake.ignoreChanges["test_net"] = []string{"name"} // the provider now sees nothing to do

	e.mustApply(p)
	if e.applyCalls() != 0 {
		t.Fatalf("ApplyResource called: %v", e.fake.calls)
	}
	e.checkSameResources("app", stateBefore, text)
	if li := e.loadLock().Intents["app"]; li.IntentHash != h {
		t.Fatalf("intent_hash = %q, want the new one", li.IntentHash)
	}
}

func TestApplyDependentUsesUpdatedValue(t *testing.T) {
	text := "renamed net, same db"
	d := Desired{Resources: []DesiredResource{
		{Key: "net", Type: "test_net", Config: map[string]any{"name": "net2"}},
		{Key: "db", Type: "test_db", Config: map[string]any{"name": "db", "net_id": "${app/net.id}"}},
	}}
	e := newTestEnv(t, StaticPlanner{hashOf(text): d})
	e.fake.unknownOnUpdate["test_net"] = []string{"id"}
	e.fake.nextID["test_net"] = 1
	e.writeIntent("app", text)
	owner := map[string]any{"prosa/intent": "app"}
	e.seed("app", hashOf("net and db"),
		seedRes{key: "net", typ: "test_net", chosen: map[string]any{"name": "net"},
			state: map[string]any{"id": "test_net-1", "name": "net", "tags": owner}},
		seedRes{key: "db", typ: "test_db", chosen: map[string]any{"name": "db", "net_id": "${app/net.id}"},
			state: map[string]any{"id": "test_db-1", "name": "db", "net_id": "test_net-1", "tags": owner}},
	)

	p := e.plan()
	if got, want := summary(p), []string{"update app/net", "update app/db"}; !equalStrings(got, want) {
		t.Fatalf("changes = %v, want %v", got, want)
	}
	e.mustApply(p)
	st, _ := e.loadState("app")
	if got := stateAttrs(t, st.Resources["net"])["id"]; got != "test_net-2" {
		t.Fatalf("net id = %v", got)
	}
	if got := stateAttrs(t, st.Resources["db"])["net_id"]; got != "test_net-2" {
		t.Fatalf("db net_id = %v, want the net's new id", got)
	}
}

func TestApplyCatchesUpNoOpState(t *testing.T) {
	text := "renamed net and a clock"
	d := Desired{Resources: []DesiredResource{
		{Key: "net", Type: "test_net", Config: map[string]any{"name": "net2"}},
		{Key: "clock", Type: "test_clock", Config: map[string]any{"name": "clock"}},
	}}
	e := newTestEnv(t, StaticPlanner{hashOf(text): d})
	e.fake.tickOnRead["test_clock"] = "read_at"
	e.writeIntent("app", text)
	e.seed("app", hashOf("net and clock"),
		seedRes{key: "net", typ: "test_net", chosen: map[string]any{"name": "net"},
			state: map[string]any{"id": "test_net-1", "name": "net", "tags": map[string]any{"prosa/intent": "app"}}},
		seedRes{key: "clock", typ: "test_clock", chosen: map[string]any{"name": "clock"},
			state: map[string]any{"id": "test_clock-1", "name": "clock", "read_at": "t0"}},
	)

	p := e.plan()
	if got, want := summary(p), []string{"no-op app/clock", "update app/net"}; !equalStrings(got, want) {
		t.Fatalf("changes = %v, want %v", got, want)
	}
	if p.Changes[0].Drifted {
		t.Fatal("clock drifted on a computed-only attribute")
	}
	e.mustApply(p)
	st, _ := e.loadState("app")
	if got := stateAttrs(t, st.Resources["clock"])["read_at"]; got != "t1" {
		t.Fatalf("clock read_at = %v, want the refreshed t1 stored with the intent", got)
	}
	if got := stateAttrs(t, st.Resources["net"])["name"]; got != "net2" {
		t.Fatalf("net name = %v", got)
	}
	e.checkConsistent("app")
}

func TestApplyPartialFailureOnChangedIntentClearsHash(t *testing.T) {
	text := "renamed net and a new db"
	d := Desired{Resources: []DesiredResource{
		{Key: "net", Type: "test_net", Config: map[string]any{"name": "net2"}},
		{Key: "db", Type: "test_db", Config: map[string]any{"name": "db", "net_id": "${app/net.id}"}},
	}}
	e := newTestEnv(t, StaticPlanner{hashOf(text): d})
	e.writeIntent("app", text)
	e.seed("app", hashOf("a net"), seedRes{key: "net", typ: "test_net", chosen: map[string]any{"name": "net"},
		state: map[string]any{"id": "test_net-1", "name": "net", "tags": map[string]any{"prosa/intent": "app"}}})
	e.fake.applyErr = func(typeName string, _ cty.Value) error {
		if typeName == "test_db" {
			return errors.New("db quota exceeded")
		}
		return nil
	}

	if err := e.apply(e.plan(), approveAll); err == nil || !strings.Contains(err.Error(), "db quota exceeded") {
		t.Fatalf("err = %v", err)
	}
	li := e.loadLock().Intents["app"]
	if li.IntentHash != "" {
		t.Fatalf("intent_hash = %q, want empty after a partial apply of a changed intent", li.IntentHash)
	}
	if net := lockRes(li, "net"); net == nil || net.Chosen["name"] != "net2" {
		t.Fatalf("net lock entry = %+v", net)
	}
	e.checkConsistent("app")
}

func TestApplyOrderCycleFailsBeforeChanges(t *testing.T) {
	// y moves from x to z, x (which uses z) is deleted and z is replaced:
	// x must go before z's destroy, y must move off x before x goes, and y
	// needs the new z, which needs z's destroy. No order works.
	text := "y on a new z, no x"
	d := Desired{Resources: []DesiredResource{
		{Key: "z", Type: "test_net", Config: map[string]any{"name": "z2"}},
		{Key: "y", Type: "test_db", Config: map[string]any{"name": "y", "net_id": "${app/z.id}"}},
	}}
	e := newTestEnv(t, StaticPlanner{hashOf(text): d})
	e.fake.replaceOn["test_net"] = []string{"name"}
	e.writeIntent("app", text)
	owner := map[string]any{"prosa/intent": "app"}
	e.seed("app", hashOf("x on z, y on x"),
		seedRes{key: "z", typ: "test_net", chosen: map[string]any{"name": "z"},
			state: map[string]any{"id": "test_net-1", "name": "z", "tags": owner}},
		seedRes{key: "x", typ: "test_db", chosen: map[string]any{"name": "x", "net_id": "${app/z.id}"},
			state: map[string]any{"id": "test_db-1", "name": "x", "net_id": "test_net-1", "tags": owner}},
		seedRes{key: "y", typ: "test_db", chosen: map[string]any{"name": "y", "net_id": "${app/x.id}"},
			state: map[string]any{"id": "test_db-2", "name": "y", "net_id": "test_db-1", "tags": owner}},
	)
	lockBefore, stateBefore := e.readFile(e.lockPath), e.readFile(e.statePath("app"))

	err := e.apply(e.plan(), approveAll)
	if err == nil || !strings.Contains(err.Error(), "cannot order the changes") || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("err = %v, want an ordering cycle", err)
	}
	if e.applyCalls() != 0 {
		t.Fatalf("ApplyResource called: %v", e.fake.calls)
	}
	if !bytes.Equal(e.readFile(e.lockPath), lockBefore) || !bytes.Equal(e.readFile(e.statePath("app")), stateBefore) {
		t.Fatal("lock or state written")
	}
}

func TestApplyTakesProjectLock(t *testing.T) {
	text := "app with a net and a db"
	e := newTestEnv(t, StaticPlanner{hashOf(text): appDesired()})
	e.writeIntent("app", text)
	p := e.plan()
	unlock, err := e.store.Lock(applyLockName)
	if err != nil {
		t.Fatal(err)
	}
	err = e.apply(p, approveAll)
	unlock()
	if err == nil || !strings.Contains(err.Error(), "another apply") {
		t.Fatalf("err = %v, want the project lock error", err)
	}
	if e.applyCalls() != 0 {
		t.Fatalf("ApplyResource called: %v", e.fake.calls)
	}
	e.mustApply(p) // released: the same plan applies
}

func TestApplyLockFileChangedSincePlan(t *testing.T) {
	text := "app with a net and a db"
	e := newTestEnv(t, StaticPlanner{hashOf(text): appDesired()})
	e.writeIntent("app", text)
	p := e.plan()
	// Another apply (of another intent) wrote the lock meanwhile.
	lk := e.loadLock()
	lk.ProviderVersions["hashicorp/aws"] = "1.0.0"
	if _, err := lock.Save(e.lockPath, lk); err != nil {
		t.Fatal(err)
	}
	lockBefore := e.readFile(e.lockPath)

	err := e.apply(p, approveAll)
	if err == nil || !strings.Contains(err.Error(), "lock file changed since plan; run plan again") {
		t.Fatalf("err = %v", err)
	}
	if e.applyCalls() != 0 || !bytes.Equal(e.readFile(e.lockPath), lockBefore) {
		t.Fatalf("applied or rewrote the lock: %v", e.fake.calls)
	}
}

func TestApplyAllSkippedUnchangedHashWritesNothing(t *testing.T) {
	e := newTestEnv(t, failingPlanner)
	h := e.writeIntent("app", "a net")
	e.seed("app", h, seedRes{key: "net", typ: "test_net", chosen: map[string]any{"name": "net"},
		state: map[string]any{"id": "test_net-1", "name": "net", "tags": map[string]any{"prosa/intent": "app"}}})
	vals := e.fake.remote["test_net-1"].AsValueMap()
	vals["name"] = cty.StringVal("renamed") // drift: the plan corrects it
	e.fake.remote["test_net-1"] = cty.ObjectVal(vals)
	lockBefore, stateBefore := e.readFile(e.lockPath), e.readFile(e.statePath("app"))

	p := e.plan()
	if got, want := summary(p), []string{"update app/net"}; !equalStrings(got, want) || p.LockDirty {
		t.Fatalf("changes = %v LockDirty=%v, want %v", got, p.LockDirty, want)
	}
	e.fake.ignoreChanges["test_net"] = []string{"name"} // the re-plan finds nothing to do
	e.mustApply(p)
	if e.applyCalls() != 0 {
		t.Fatalf("ApplyResource called: %v", e.fake.calls)
	}
	if !bytes.Equal(e.readFile(e.lockPath), lockBefore) || !bytes.Equal(e.readFile(e.statePath("app")), stateBefore) {
		t.Fatal("lock or state rewritten although every change was skipped")
	}
}

func TestApplyUnknownStateNotPersisted(t *testing.T) {
	text := "a token"
	d := Desired{Resources: []DesiredResource{{Key: "tok", Type: "test_token", Config: map[string]any{"name": "tok"}}}}
	e := newTestEnv(t, StaticPlanner{hashOf(text): d})
	e.writeIntent("app", text)
	e.fake.unknownOnApply["test_token"] = "value"

	err := e.apply(e.plan(), approveAll)
	if err == nil || !strings.Contains(err.Error(), "app/tok") || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("err = %v, want an unknown-state error naming app/tok", err)
	}
	if st, _ := e.loadState("app"); len(st.Resources) != 0 {
		t.Fatalf("state with unknown values persisted: %v", st.Resources)
	}
}
