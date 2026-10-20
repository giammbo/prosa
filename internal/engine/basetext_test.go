package engine

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/giammbo/prosa/internal/state"
)

// netAndDB is a base of app: z-db on a-net, so that destroying it needs the
// lock's edges (the reverse of the keys' order).
func netAndDB() map[string]Desired {
	return map[string]Desired{"app": {Resources: []DesiredResource{
		{Key: "a-net", Type: "test_net", Config: map[string]any{"name": "net"}},
		{Key: "z-db", Type: "test_db", Config: map[string]any{"name": "db", "net_id": "${app/a-net.id}"}},
	}}}
}

func baseTextOf(t *testing.T, dir, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name+".md"))
	if os.IsNotExist(err) {
		return "<none>"
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// envOf returns a testEnv for the environment staging of the root project
// of root: its own lock and state store, the root's lock as the base lock and
// the root's store as BaseState.
func envOf(root *testEnv, body string) *testEnv {
	root.t.Helper()
	dirOf := filepath.Dir(root.lockPath)
	store, err := state.Open(filepath.Join(dirOf, ".prosa", "state.d", "staging"), "passphrase")
	if err != nil {
		root.t.Fatal(err)
	}
	eng := *root.engine
	eng.LockPath, eng.State = filepath.Join(dirOf, "staging.lock.yaml"), store
	eng.BaseLockPath, eng.BaseState = root.lockPath, root.store
	eng.Env = testEnvFile("staging", body)
	return &testEnv{t: root.t, dir: root.dir, lockPath: eng.LockPath, store: store, fake: root.fake, engine: &eng}
}

// In a project used only with environments, the env apply that writes the
// base lock keeps the text the base was planned from in the base text
// directory; the next re-plan of the base sees it as the previous text,
// not "a new intent".
func TestBaseTextKeptForEnvRuns(t *testing.T) {
	bp := &basePlanner{base: netAndDB()}
	root := newTestEnv(t, bp)
	texts := filepath.Join(filepath.Dir(root.lockPath), ".prosa", "base")
	root.engine.BaseTextDir = texts
	e := envOf(root, stagingBody)
	e.writeIntent("app", "app v1")
	e.mustApply(e.plan())
	if blk := e.loadBaseIntent(root.lockPath, "app"); blk == nil || blk.IntentHash != hashOf("app v1") {
		t.Fatalf("base lock entry = %+v, want it written: there is no root state", blk)
	}
	if got := baseTextOf(t, texts, "app"); got != "app v1" {
		t.Fatalf("base text = %q, want the text the base was planned from", got)
	}

	e.writeIntent("app", "app v2")
	bp.calls = nil
	e.plan()
	in, ok := bp.baseInputs["app"]
	if !ok || in.PreviousBody != "app v1" || in.Mode != "patch" {
		t.Fatalf("base input: planned %v, previous body %q, mode %q; want the kept text", ok, in.PreviousBody, in.Mode)
	}
}

// A root run sees the kept base text as the previous text (it is what the
// root's lock entry was planned from), and its apply writes it; removing the
// intent removes it.
func TestRootRunUsesBaseText(t *testing.T) {
	bp := &basePlanner{base: netAndDB()}
	e := newTestEnv(t, bp)
	texts := filepath.Join(filepath.Dir(e.lockPath), ".prosa", "base")
	e.engine.BaseTextDir = texts
	e.writeIntent("app", "app v1")
	e.mustApply(e.plan())
	if got := baseTextOf(t, texts, "app"); got != "app v1" {
		t.Fatalf("base text = %q, want the applied text", got)
	}
	if err := os.WriteFile(filepath.Join(texts, "app.md"), []byte("app v1.5"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.writeIntent("app", "app v2")
	e.plan()
	if got := bp.baseInputs["app"].PreviousBody; got != "app v1.5" {
		t.Fatalf("previous body = %q, want the kept base text", got)
	}

	if err := os.Remove(filepath.Join(e.dir, "app.md")); err != nil {
		t.Fatal(err)
	}
	e.mustApply(e.plan())
	if got := baseTextOf(t, texts, "app"); got != "<none>" {
		t.Fatalf("base text of a removed intent = %q", got)
	}
}

// When the root workspace has applied the intent, an env run plans the
// changed base in memory and never rewrites the root's lock: the root keeps
// its lock entry, whose edges order the destroy of what the base drops. The
// environment's lock records the base_hash, and the next env run asks
// nothing.
func TestEnvRunLeavesAppliedRootLock(t *testing.T) {
	bp := &basePlanner{base: netAndDB()}
	root := newTestEnv(t, bp)
	texts := filepath.Join(filepath.Dir(root.lockPath), ".prosa", "base")
	root.engine.BaseTextDir = texts
	root.writeIntent("app", "app v1")
	root.mustApply(root.plan())

	bp.base = map[string]Desired{"app": {Resources: []DesiredResource{{Key: "c-log", Type: "test_net", Config: map[string]any{"name": "log"}}}}}
	v2 := root.writeIntent("app", "app v2")
	rootLock := root.readFile(root.lockPath)
	e := envOf(root, stagingBody)
	bp.calls = nil
	e.mustApply(e.plan())
	if want := []string{"app patch base", "app patch env"}; !equalStrings(bp.calls, want) {
		t.Fatalf("calls = %v, want %v", bp.calls, want)
	}
	if !slices.Equal(rootLock, root.readFile(root.lockPath)) {
		t.Fatal("the env run rewrote the root's lock, which has applied state")
	}
	if got := baseTextOf(t, texts, "app"); got != "app v1" {
		t.Fatalf("base text = %q, want the root's, unchanged", got)
	}
	if app := e.loadLock().Intents["app"]; app.BaseHash != v2 || lockRes(app, "c-log") == nil {
		t.Fatalf("env lock: base_hash %q, resources %+v; want the in-memory base", app.BaseHash, app.Resources)
	}
	bp.calls = nil
	e.plan()
	if len(bp.calls) != 0 {
		t.Fatalf("an up-to-date environment asked %v", bp.calls)
	}

	p := root.plan()
	got := summary(p)
	want := []string{"delete app/z-db", "delete app/a-net", "create app/c-log"}
	if !equalStrings(got, want) {
		t.Fatalf("root plan = %v, want %v", got, want)
	}
	if prev := bp.baseInputs["app"].PreviousBody; prev != "app v1" {
		t.Fatalf("root previous body = %q", prev)
	}
}

// An interrupted root apply leaves its lock entry with an empty intent_hash
// and its Pending: an env run does not overwrite that entry.
func TestEnvRunLeavesPendingRootEntry(t *testing.T) {
	bp := &basePlanner{base: netAndDB()}
	root := newTestEnv(t, bp)
	root.writeIntent("app", "app v1")
	root.interruptAfterNet()
	rootLock := root.readFile(root.lockPath)
	if li := root.loadLock().Intents["app"]; li == nil || li.IntentHash != "" {
		t.Fatalf("root entry = %+v, want an interrupted one", li)
	}
	e := envOf(root, stagingBody)
	e.mustApply(e.plan())
	if !slices.Equal(rootLock, root.readFile(root.lockPath)) {
		t.Fatal("the env run rewrote the root's pending entry")
	}
	root.mustApply(root.plan())
	if li := root.loadLock().Intents["app"]; li.IntentHash != hashOf("app v1") || lockRes(li, "z-db") == nil {
		t.Fatalf("root entry after resume = %+v", li)
	}
}
