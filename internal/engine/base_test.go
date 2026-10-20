package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zclconf/go-cty/cty"

	"github.com/giammbo/prosa/internal/lock"
	"github.com/giammbo/prosa/internal/provider"
	"github.com/giammbo/prosa/internal/state"
)

// basePlanner answers the planning of a base (no environment) from base, by
// intent, and the patch of an environment from patch (an empty patch when
// none). calls records every call as "<intent> <mode> base|env"; inputs
// keeps the last input of each environment call.
type basePlanner struct {
	base   map[string]Desired
	patch  map[string][]lock.PatchOp
	calls  []string
	inputs map[string]PlanInput
	// baseInputs keeps the last input of each base call.
	baseInputs map[string]PlanInput
}

func (b *basePlanner) Plan(_ context.Context, in PlanInput) (Desired, error) {
	where := "base"
	if in.Env != nil {
		where = "env"
	}
	b.calls = append(b.calls, in.Intent.Name+" "+in.Mode+" "+where)
	if in.Env == nil {
		if b.baseInputs == nil {
			b.baseInputs = map[string]PlanInput{}
		}
		b.baseInputs[in.Intent.Name] = in
		d, ok := b.base[in.Intent.Name]
		if !ok {
			return Desired{}, errors.New("no base for " + in.Intent.Name)
		}
		return d, nil
	}
	if b.inputs == nil {
		b.inputs = map[string]PlanInput{}
	}
	b.inputs[in.Intent.Name] = in
	ops := b.patch[in.Intent.Name]
	if ops == nil {
		ops = []lock.PatchOp{}
	}
	return Desired{Patch: ops}, nil
}

// stagingBody has a section for app and web, none for net, and no common
// lines.
const stagingBody = "# Staging\n\n## app\nA bigger db.\n\n## web\nTagged.\n"

// newBaseEnv is a testEnv that derives the environment staging (body) from
// the base (ADR-030): the base lock is the root's, the environment's sits
// next to it. It returns the base lock's path.
func newBaseEnv(t *testing.T, pl Planner, body string) (*testEnv, string) {
	t.Helper()
	e := newTestEnv(t, pl)
	base := e.lockPath
	e.lockPath = filepath.Join(filepath.Dir(base), "staging.lock.yaml")
	e.engine.LockPath, e.engine.BaseLockPath = e.lockPath, base
	e.engine.Env = testEnvFile("staging", body)
	return e, base
}

// threeBases is the base of three intents: app's db is on net's net, web
// has a site of its own.
func threeBases() map[string]Desired {
	return map[string]Desired{
		"app": {Resources: []DesiredResource{{Key: "db", Type: "test_db", Config: map[string]any{
			"name": "db", "net_id": "${net/net.id}", "size": 10, "tags": map[string]any{"team": "core"}}}}},
		"net": {Resources: []DesiredResource{{Key: "net", Type: "test_net", Config: map[string]any{"name": "net"}}}},
		"web": {Resources: []DesiredResource{{Key: "site", Type: "test_net", Config: map[string]any{"name": "site"}}}},
	}
}

func stagingPatches() map[string][]lock.PatchOp {
	return map[string][]lock.PatchOp{
		"app": {{Op: "set", Key: "db", Path: "size", Value: 20}},
		"web": {{Op: "set", Key: "site", Path: "tags.env", Value: "staging"}},
	}
}

// writeIntents writes the intents named in bases, each with the text
// "<name> v1", and returns their hashes.
func (e *testEnv) writeIntents(bases map[string]Desired) map[string]string {
	e.t.Helper()
	hashes := map[string]string{}
	for name := range bases {
		hashes[name] = e.writeIntent(name, name+" v1")
	}
	return hashes
}

// writeBaseLock writes the base lock at path: for each intent its hash and
// the choices of its desired state (no id, no state).
func writeBaseLock(t *testing.T, path string, hashes map[string]string, bases map[string]Desired) {
	t.Helper()
	lk, err := lock.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	for name, d := range bases {
		li := &lock.Intent{IntentHash: hashes[name], Checks: d.Checks}
		for _, r := range d.Resources {
			li.Resources = append(li.Resources, lock.Resource{Key: r.Key, Provider: "hashicorp/aws", Type: r.Type, Chosen: r.Config, DependsOn: r.DependsOn})
		}
		lk.Intents[name] = li
	}
	if _, err := lock.Save(path, lk); err != nil {
		t.Fatal(err)
	}
}

// With the base in the lock of the root, --env asks only for the patches of
// the intents the environment names; the one it does not name, without
// required names, is the base, with an empty patch, and no call.
func TestEnvReusesBase(t *testing.T) {
	bp := &basePlanner{patch: stagingPatches()}
	e, base := newBaseEnv(t, bp, stagingBody)
	bases := threeBases()
	hashes := e.writeIntents(bases)
	writeBaseLock(t, base, hashes, bases)
	baseBytes := e.readFile(base)

	p := e.plan()
	if want := []string{"app patch env", "web patch env"}; !equalStrings(bp.calls, want) {
		t.Fatalf("calls = %v, want %v", bp.calls, want)
	}
	in := bp.inputs["app"]
	if in.Mode != "patch" || len(in.Base) != 1 || in.Base[0].Key != "db" || in.OldPatch != nil || in.Problems != nil {
		t.Fatalf("app input: mode %q, base %+v, old patch %v, problems %v", in.Mode, in.Base, in.OldPatch, in.Problems)
	}
	if got := configOf(p.desired["app"], "db")["size"]; !sameJSON(got, 20) {
		t.Fatalf("app db size = %v, want the patch's 20", got)
	}
	e.mustApply(p)
	if !slices.Equal(baseBytes, e.readFile(base)) {
		t.Fatal("an up-to-date base lock was rewritten")
	}

	lk := e.loadLock()
	for name, li := range lk.Intents {
		if li.BaseHash != hashes[name] {
			t.Errorf("%s: base_hash = %q, want %q", name, li.BaseHash, hashes[name])
		}
	}
	if net := lk.Intents["net"]; len(net.Patch) != 0 || !sameJSON(lockRes(net, "net").Chosen, bases["net"].Resources[0].Config) {
		t.Errorf("net: patch %v, chosen %v, want no patch and the base", net.Patch, lockRes(net, "net").Chosen)
	}
	if app := lk.Intents["app"]; !sameJSON(app.Patch, stagingPatches()["app"]) || !sameJSON(lockRes(app, "db").Chosen["size"], 20) {
		t.Errorf("app: patch %v, chosen %v", app.Patch, lockRes(app, "db").Chosen)
	}
	if id := lockRes(lk.Intents["net"], "net").ID; id == "" {
		t.Error("the environment's lock has no id for net")
	}

	// Nothing changed: no call.
	bp.calls = nil
	e.plan()
	if len(bp.calls) != 0 {
		t.Fatalf("unchanged project asked %v", bp.calls)
	}
}

// envOnBase applies staging on the base of threeBases and returns the
// planner, the env, the base lock's path and the hashes.
func envOnBase(t *testing.T, patches map[string][]lock.PatchOp, bases map[string]Desired) (*basePlanner, *testEnv, string, map[string]string) {
	t.Helper()
	bp := &basePlanner{patch: patches}
	e, base := newBaseEnv(t, bp, stagingBody)
	hashes := e.writeIntents(bases)
	writeBaseLock(t, base, hashes, bases)
	e.mustApply(e.plan())
	bp.calls = nil
	return bp, e, base, hashes
}

// The base changes in an attribute the patch does not touch, the
// environment does not: the saved patch applies again, without a call.
func TestBaseChangeReappliesPatch(t *testing.T) {
	bp, e, base, hashes := envOnBase(t, stagingPatches(), threeBases())
	bases := threeBases()
	bases["app"].Resources[0].Config["tags"] = map[string]any{"team": "platform"}
	hashes["app"] = e.writeIntent("app", "app v2")
	writeBaseLock(t, base, hashes, map[string]Desired{"app": bases["app"]})

	p := e.plan()
	if len(bp.calls) != 0 {
		t.Fatalf("calls = %v, want none", bp.calls)
	}
	want := map[string]any{"name": "db", "net_id": "${net/net.id}", "size": 20, "tags": map[string]any{"team": "platform"}}
	if got := configOf(p.desired["app"], "db"); !sameJSON(got, want) {
		t.Fatalf("app db = %v, want the new base with the patch: %v", got, want)
	}
	e.mustApply(p)
	app := e.loadLock().Intents["app"]
	if app.BaseHash != hashes["app"] || !sameJSON(app.Patch, stagingPatches()["app"]) || !sameJSON(lockRes(app, "db").Chosen, want) {
		t.Fatalf("app lock: base_hash %q, patch %v, db %v", app.BaseHash, app.Patch, lockRes(app, "db").Chosen)
	}
}

// The base drops the resource the saved patch changes (Review Focus 2): the
// engine applies no part of the patch and asks the model for a patch on the
// new base, with the old patch and the problem.
func TestBaseChangePatchConflict(t *testing.T) {
	bases := threeBases()
	bases["app"] = Desired{Resources: []DesiredResource{
		{Key: "db", Type: "test_db", Config: map[string]any{"name": "db", "size": 10}},
		{Key: "log", Type: "test_net", Config: map[string]any{"name": "log"}},
	}}
	old := []lock.PatchOp{
		{Op: "set", Key: "log", Path: "tags.env", Value: "staging"},
		{Op: "set", Key: "db", Path: "size", Value: 20},
	}
	patches := stagingPatches()
	patches["app"] = old
	bp, e, base, hashes := envOnBase(t, patches, bases)

	bases["app"] = Desired{Resources: []DesiredResource{
		{Key: "store", Type: "test_db", Config: map[string]any{"name": "store", "size": 10}},
		{Key: "log", Type: "test_net", Config: map[string]any{"name": "log"}},
	}}
	hashes["app"] = e.writeIntent("app", "app v2")
	writeBaseLock(t, base, hashes, map[string]Desired{"app": bases["app"]})
	fresh := []lock.PatchOp{{Op: "set", Key: "store", Path: "size", Value: 30}}
	bp.patch["app"] = fresh

	p := e.plan()
	if want := []string{"app patch env"}; !equalStrings(bp.calls, want) {
		t.Fatalf("calls = %v, want %v", bp.calls, want)
	}
	in := bp.inputs["app"]
	if !sameJSON(in.OldPatch, old) {
		t.Errorf("OldPatch = %v, want %v", in.OldPatch, old)
	}
	if len(in.Base) != 2 || !slices.ContainsFunc(in.Base, func(r lock.Resource) bool { return r.Key == "store" }) {
		t.Errorf("Base = %+v, want the new base", in.Base)
	}
	if len(in.Problems) != 1 || !strings.Contains(in.Problems[0], `resource "db" not found`) {
		t.Errorf("Problems = %q, want the op that does not apply", in.Problems)
	}
	if got := configOf(p.desired["app"], "log"); !sameJSON(got, map[string]any{"name": "log"}) {
		t.Errorf("log = %v: part of the old patch was applied", got)
	}
	if got := configOf(p.desired["app"], "store")["size"]; !sameJSON(got, 30) {
		t.Errorf("store size = %v, want the new patch's 30", got)
	}
	e.mustApply(p)
	if app := e.loadLock().Intents["app"]; !sameJSON(app.Patch, fresh) || app.BaseHash != hashes["app"] {
		t.Fatalf("app lock: patch %v, base_hash %q", app.Patch, app.BaseHash)
	}
}

// Without a base lock the first --env plans the base, which Apply writes in
// the root's lock as choices only, then the patches.
func TestBasePlannedFirst(t *testing.T) {
	bp := &basePlanner{base: threeBases(), patch: stagingPatches()}
	e, base := newBaseEnv(t, bp, stagingBody)
	hashes := e.writeIntents(threeBases())

	p := e.plan()
	want := []string{"app plan base", "net plan base", "web plan base", "app patch env", "web patch env"}
	if !equalStrings(bp.calls, want) {
		t.Fatalf("calls = %v, want %v", bp.calls, want)
	}
	if got := len(bp.inputs["app"].Base); got != 1 {
		t.Fatalf("app patch input has %d base resources, want the planned one", got)
	}
	if _, err := os.Stat(base); !os.IsNotExist(err) {
		t.Fatalf("Plan wrote the base lock (stat err %v)", err)
	}
	e.mustApply(p)

	blk, err := lock.Load(base)
	if err != nil {
		t.Fatal(err)
	}
	for name, d := range threeBases() {
		li := blk.Intents[name]
		if li == nil || li.IntentHash != hashes[name] || li.State != (lock.StateRef{}) || li.BaseHash != "" || li.Patch != nil {
			t.Fatalf("base %s = %+v, want hash %s and no state", name, li, hashes[name])
		}
		for _, r := range li.Resources {
			if r.ID != "" {
				t.Errorf("base %s/%s has id %q", name, r.Key, r.ID)
			}
			if !sameJSON(r.Chosen, configOf(d.Resources, r.Key)) {
				t.Errorf("base %s/%s chosen = %v", name, r.Key, r.Chosen)
			}
		}
	}
	if net := e.loadLock().Intents["net"]; net.BaseHash != hashes["net"] || len(net.Patch) != 0 {
		t.Fatalf("env net: base_hash %q, patch %v", net.BaseHash, net.Patch)
	}

	bp.calls = nil
	e.plan()
	if len(bp.calls) != 0 {
		t.Fatalf("second plan asked %v", bp.calls)
	}
}

// An intent with a required name (aws_ecs_cluster.name) asks for a patch in
// every environment, even one that does not name it.
func TestRequiredNamesAskPatch(t *testing.T) {
	bases := map[string]Desired{
		"app": {Resources: []DesiredResource{{Key: "app.cluster", Type: "aws_ecs_cluster", Config: map[string]any{"name": "app"}}}},
		"net": {Resources: []DesiredResource{{Key: "net", Type: "test_net", Config: map[string]any{"name": "net"}}}},
	}
	bp := &basePlanner{patch: map[string][]lock.PatchOp{
		"app": {{Op: "set", Key: "app.cluster", Path: "name", Value: "staging-app"}},
	}}
	e, base := newBaseEnv(t, bp, "# Staging\n\n## web\nTagged.\n")
	e.fake.schema.Resources["aws_ecs_cluster"] = &provider.ResourceSchema{Block: &provider.Block{
		Attributes: map[string]*provider.Attribute{
			"name": {Type: cty.String, Required: true},
			"tags": {Type: cty.Map(cty.String), Optional: true},
			"id":   {Type: cty.String, Computed: true},
		},
		BlockTypes: map[string]*provider.NestedBlock{},
	}}
	hashes := e.writeIntents(bases)
	writeBaseLock(t, base, hashes, bases)

	p := e.plan()
	if want := []string{"app patch env"}; !equalStrings(bp.calls, want) {
		t.Fatalf("calls = %v, want %v", bp.calls, want)
	}
	in := bp.inputs["app"]
	if in.Mode != "patch" || !equalStrings(in.RequiredNames, []string{"app.cluster.name"}) {
		t.Fatalf("app input: mode %q, required names %v", in.Mode, in.RequiredNames)
	}
	if got := configOf(p.desired["app"], "app.cluster")["name"]; got != "staging-app" {
		t.Fatalf("cluster name = %v, want the patch's", got)
	}
}

// An apply interrupted in an environment resumes from its saved desired
// state without a call, and the lock still gets the base_hash and the patch.
func TestEnvResumeKeepsPatch(t *testing.T) {
	bases := map[string]Desired{"app": {Resources: []DesiredResource{
		{Key: "db", Type: "test_db", Config: map[string]any{"name": "db", "net_id": "${app/net.id}", "size": 10}},
		{Key: "net", Type: "test_net", Config: map[string]any{"name": "net"}},
	}}}
	patch := []lock.PatchOp{{Op: "set", Key: "db", Path: "size", Value: 20}}
	bp := &basePlanner{patch: map[string][]lock.PatchOp{"app": patch}}
	e, base := newBaseEnv(t, bp, stagingBody)
	hashes := e.writeIntents(bases)
	writeBaseLock(t, base, hashes, bases)
	e.interruptAfterNet()

	e.engine.Planner = failingPlanner
	e.mustApply(e.plan())
	app := e.loadLock().Intents["app"]
	if app.BaseHash != hashes["app"] || !sameJSON(app.Patch, patch) || !sameJSON(lockRes(app, "db").Chosen["size"], 20) {
		t.Fatalf("app lock: base_hash %q, patch %v, db %v", app.BaseHash, app.Patch, lockRes(app, "db").Chosen)
	}
}

// A planner that answers a patch request with a complete desired state: the
// environment's lock records a patch that derives it from the base, so that
// a later change of the base applies it again without a call.
func TestEnvFullStateRecordsPatch(t *testing.T) {
	bases := threeBases()
	full := plannerFunc(func(_ context.Context, in PlanInput) (Desired, error) {
		d := threeBases()[in.Intent.Name]
		d.Resources[0].Config["tags"] = map[string]any{"env": "staging"}
		return d, nil
	})
	e, base := newBaseEnv(t, full, "Every resource has the tag env=staging.")
	hashes := e.writeIntents(bases)
	writeBaseLock(t, base, hashes, bases)
	e.mustApply(e.plan())
	app := e.loadLock().Intents["app"]
	want := []lock.PatchOp{{Op: "set", Key: "db", Path: "tags", Value: map[string]any{"env": "staging"}}}
	if !sameJSON(app.Patch, want) {
		t.Fatalf("app patch = %v, want %v", app.Patch, want)
	}

	e.engine.Planner = failingPlanner
	bases["app"].Resources[0].Config["size"] = 30
	hashes["app"] = e.writeIntent("app", "app v2")
	writeBaseLock(t, base, hashes, map[string]Desired{"app": bases["app"]})
	p := e.plan()
	if got := configOf(p.desired["app"], "db"); !sameJSON(got["size"], 30) || !sameJSON(got["tags"], map[string]any{"env": "staging"}) {
		t.Fatalf("app db = %v, want the new base with the recorded patch", got)
	}
}

// Apply does not overwrite a base lock that changed since the plan.
func TestBaseLockChangedSincePlan(t *testing.T) {
	bp := &basePlanner{base: threeBases(), patch: stagingPatches()}
	e, base := newBaseEnv(t, bp, stagingBody)
	e.writeIntents(threeBases())
	p := e.plan()
	if err := os.WriteFile(base, []byte("version: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := e.apply(p, approveAll)
	if err == nil || !strings.Contains(err.Error(), "base lock changed since plan") {
		t.Fatalf("err = %v, want the base lock changed", err)
	}
	if got := string(e.readFile(base)); got != "version: 1\n" {
		t.Fatalf("base lock = %q, overwritten", got)
	}
}

// A stale base is asked for a patch on the base lock; when its root entry
// was applied it keeps the root's state and ids:
// they belong to the root's workspace.
func TestBaseReplanKeepsRootState(t *testing.T) {
	bp := &basePlanner{base: threeBases(), patch: stagingPatches()}
	e, base := newBaseEnv(t, bp, stagingBody)
	hashes := e.writeIntents(threeBases())
	writeBaseLock(t, base, hashes, threeBases())
	lk, _ := lock.Load(base)
	lk.Intents["net"].IntentHash = "sha256:old"
	lk.Intents["net"].State = lock.StateRef{Serial: 4, Digest: "sha256:root"}
	lk.Intents["net"].Resources[0].ID = "net-root"
	if _, err := lock.Save(base, lk); err != nil {
		t.Fatal(err)
	}
	e.mustApply(e.plan())
	if want := []string{"net patch base", "app patch env", "web patch env"}; !equalStrings(bp.calls, want) {
		t.Fatalf("calls = %v, want %v", bp.calls, want)
	}
	net := e.loadBaseIntent(base, "net")
	if net.IntentHash != hashes["net"] || net.State.Digest != "sha256:root" || net.Resources[0].ID != "net-root" {
		t.Fatalf("base net = %+v, want the new hash with the root's state and id", net)
	}
}

// The base planned during an --env run is the request `prosa plan` makes for
// it, so the planner cache answers both: it sees the text the root's state
// store holds as the base's previous text, not an empty one.
func TestBaseReplanSeesRootBody(t *testing.T) {
	bp := &basePlanner{base: threeBases(), patch: stagingPatches()}
	e, base := newBaseEnv(t, bp, stagingBody)
	hashes := e.writeIntents(threeBases())
	writeBaseLock(t, base, hashes, threeBases())
	lk, _ := lock.Load(base)
	lk.Intents["net"].IntentHash = "sha256:old"
	if _, err := lock.Save(base, lk); err != nil {
		t.Fatal(err)
	}
	root, err := state.Open(filepath.Join(filepath.Dir(base), "root-state"), "passphrase")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := root.Save("net", &state.IntentState{Body: "net v0", Resources: map[string]*state.ResourceState{}}); err != nil {
		t.Fatal(err)
	}
	e.engine.BaseState = root
	e.plan()
	in, ok := bp.baseInputs["net"]
	if !ok {
		t.Fatalf("net base was not planned: %v", bp.calls)
	}
	if in.PreviousBody != "net v0" || in.Mode != "patch" || in.Env != nil || in.Base != nil || len(in.Previous) != 1 {
		t.Fatalf("base input: body %q, mode %q, env %v, base %v, previous %d", in.PreviousBody, in.Mode, in.Env, in.Base, len(in.Previous))
	}
}

func (e *testEnv) loadBaseIntent(path, name string) *lock.Intent {
	e.t.Helper()
	lk, err := lock.Load(path)
	if err != nil {
		e.t.Fatal(err)
	}
	return lk.Intents[name]
}

// Without an environment, an intent of the lock is asked for a patch on it,
// a new one for a plan; a patch is applied to the lock's state.
func TestPatchAnswerAppliesToPrevious(t *testing.T) {
	var modes []string
	answer := Desired{Resources: []DesiredResource{{Key: "net", Type: "test_net", Config: map[string]any{"name": "net"}}}}
	e := newTestEnv(t, plannerFunc(func(_ context.Context, in PlanInput) (Desired, error) {
		modes = append(modes, in.Mode)
		if in.Base != nil || in.RequiredNames != nil {
			t.Errorf("Base %v, RequiredNames %v without an environment", in.Base, in.RequiredNames)
		}
		return answer, nil
	}))
	e.writeIntent("app", "app v1")
	e.mustApply(e.plan())
	e.writeIntent("app", "app v2")
	answer = Desired{Patch: []lock.PatchOp{{Op: "set", Key: "net", Path: "tags.team", Value: "core"}}}
	p := e.plan()
	if want := []string{"plan", "patch"}; !equalStrings(modes, want) {
		t.Fatalf("modes = %v, want %v", modes, want)
	}
	if got := configOf(p.desired["app"], "net"); !sameJSON(got, map[string]any{"name": "net", "tags": map[string]any{"team": "core"}}) {
		t.Fatalf("net = %v, want the lock's with the patch", got)
	}
}

// An empty patch ("nothing to change") stays a patch through JSON, as the
// planner cache stores it: it resolves to the base, never to an empty state.
func TestEmptyPatchSurvivesJSON(t *testing.T) {
	data, err := json.Marshal(Desired{Patch: []lock.PatchOp{}})
	if err != nil {
		t.Fatal(err)
	}
	var d Desired
	if err := json.Unmarshal(data, &d); err != nil {
		t.Fatal(err)
	}
	base := []lock.Resource{{Key: "net", Type: "test_net", Chosen: map[string]any{"name": "net"}}}
	got, err := PlanInput{patchBase: base}.resolve(d)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Resources) != 1 || got.Resources[0].Key != "net" {
		t.Fatalf("resolved %s to %+v, want the base", data, got.Resources)
	}
}

// The base gains a resource with a required name while the environment's
// part is unchanged: the old patch does not set it, so the model is asked.
func TestBaseChangeNewRequiredName(t *testing.T) {
	bp, e, base, hashes := envOnBase(t, stagingPatches(), threeBases())
	e.fake.schema.Resources["aws_ecs_cluster"] = &provider.ResourceSchema{Block: &provider.Block{
		Attributes: map[string]*provider.Attribute{
			"name": {Type: cty.String, Required: true},
			"id":   {Type: cty.String, Computed: true},
		},
		BlockTypes: map[string]*provider.NestedBlock{},
	}}
	bases := threeBases()
	app := bases["app"]
	app.Resources = append(app.Resources, DesiredResource{Key: "app.cluster", Type: "aws_ecs_cluster", Config: map[string]any{"name": "app-cluster"}})
	bases["app"] = app
	hashes["app"] = e.writeIntent("app", "app v2")
	writeBaseLock(t, base, hashes, map[string]Desired{"app": bases["app"]})
	bp.patch["app"] = append(stagingPatches()["app"], lock.PatchOp{Op: "set", Key: "app.cluster", Path: "name", Value: "staging-app"})

	p := e.plan()
	if want := []string{"app patch env"}; !equalStrings(bp.calls, want) {
		t.Fatalf("calls = %v, want %v", bp.calls, want)
	}
	in := bp.inputs["app"]
	if !equalStrings(in.RequiredNames, []string{"app.cluster.name"}) || !sameJSON(in.OldPatch, stagingPatches()["app"]) {
		t.Fatalf("required names %v, old patch %v", in.RequiredNames, in.OldPatch)
	}
	if len(in.Problems) != 1 || !strings.Contains(in.Problems[0], "app.cluster.name") {
		t.Fatalf("Problems = %q, want the uncovered required name", in.Problems)
	}
	if got := configOf(p.desired["app"], "app.cluster")["name"]; got != "staging-app" {
		t.Fatalf("cluster name = %v, want the new patch's", got)
	}
}

// A patch that replaces all the checks is applied again to a new base only if
// the Checks section of the intent did not change: otherwise it would bring
// back the old checks over the new ones, and the model writes the patch again.
func TestBaseChangeWithChecksPatch(t *testing.T) {
	checkOne := []any{map[string]any{"name": "Uno.", "type": "unverifiable", "reason": "by hand"}}
	cases := []struct {
		name, v2 string
		wantCall bool
	}{
		{"checks unchanged", "app v2\n\n## Checks\n- Uno.\n", false},
		{"checks changed", "app v1\n\n## Checks\n- Due.\n", true},
		{"old heading, checks unchanged", "app v2\n\n## Verifiche\n- Uno.\n", false},
		{"old heading, checks changed", "app v1\n\n## Verifiche\n- Due.\n", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			patches := stagingPatches()
			patches["app"] = []lock.PatchOp{{Op: "set", Key: "db", Path: "size", Value: 20}, {Op: "checks", Value: checkOne}}
			bases := threeBases()
			bp := &basePlanner{patch: patches}
			e, base := newBaseEnv(t, bp, stagingBody)
			hashes := e.writeIntents(bases)
			hashes["app"] = e.writeIntent("app", "app v1\n\n## Checks\n- Uno.\n")
			writeBaseLock(t, base, hashes, bases)
			e.mustApply(e.plan())
			bp.calls = nil

			hashes["app"] = e.writeIntent("app", c.v2)
			writeBaseLock(t, base, hashes, map[string]Desired{"app": bases["app"]})
			e.plan()
			if got := len(bp.calls) > 0; got != c.wantCall {
				t.Fatalf("calls = %v, want a call: %v", bp.calls, c.wantCall)
			}
			if c.wantCall {
				in := bp.inputs["app"]
				if len(in.Problems) != 1 || !strings.Contains(in.Problems[0], "replaces all the checks") || len(in.OldPatch) != 2 {
					t.Errorf("problems %q, old patch %v: want the problem and the old patch", in.Problems, in.OldPatch)
				}
			}
		})
	}
}

// ecsClusterSchema is aws_ecs_cluster with its required name.
func ecsClusterSchema() *provider.ResourceSchema {
	return &provider.ResourceSchema{Block: &provider.Block{
		Attributes: map[string]*provider.Attribute{
			"name": {Type: cty.String, Required: true},
			"tags": {Type: cty.Map(cty.String), Optional: true},
			"id":   {Type: cty.String, Computed: true},
		},
		BlockTypes: map[string]*provider.NestedBlock{},
	}}
}

// The first patch of an intent with a required name is validated for it: a
// patch that does not set it is a problem for the model, unless the name the
// state keeps appears literally in the intent's text or in the environment's
// part (the prose dictates it).
func TestRequiredNameFirstPatchValidated(t *testing.T) {
	cases := []struct {
		name, body, envBody string
		patch               []lock.PatchOp
		wantProblem         bool
	}{
		{"not set", "app v1", "# Staging\n\n## app\nA bigger db.\n", []lock.PatchOp{}, true},
		{"set", "app v1", "# Staging\n\n## app\nA bigger db.\n",
			[]lock.PatchOp{{Op: "set", Key: "app.cluster", Path: "name", Value: "staging-app"}}, false},
		{"stated by the environment", "app v1", "# Staging\n\n## app\nThe cluster stays app-cluster.\n", []lock.PatchOp{}, false},
		{"stated by the intent", "app v1: the cluster is app-cluster.", "# Staging\n\n## app\nA bigger db.\n", []lock.PatchOp{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			bases := map[string]Desired{"app": {Resources: []DesiredResource{
				{Key: "app.cluster", Type: "aws_ecs_cluster", Config: map[string]any{"name": "app-cluster"}}}}}
			var validateErr error
			pl := plannerFunc(func(ctx context.Context, in PlanInput) (Desired, error) {
				validateErr = in.Validate(ctx, Desired{Patch: c.patch})
				return Desired{Patch: c.patch}, nil
			})
			e, base := newBaseEnv(t, pl, c.envBody)
			e.fake.schema.Resources["aws_ecs_cluster"] = ecsClusterSchema()
			hashes := map[string]string{"app": e.writeIntent("app", c.body)}
			writeBaseLock(t, base, hashes, bases)
			e.plan()
			if got := validateErr != nil; got != c.wantProblem {
				t.Fatalf("Validate = %v, want a problem: %v", validateErr, c.wantProblem)
			}
			if c.wantProblem && !strings.Contains(validateErr.Error(), "app.cluster.name") {
				t.Fatalf("Validate = %v, want the required name", validateErr)
			}
		})
	}
}

// The base gains a resource with a required name that the intent's text
// states: the saved patch, which does not set it, applies again without a
// call (the same coverage rule as a new patch).
func TestBaseChangeRequiredNameStated(t *testing.T) {
	bp, e, base, hashes := envOnBase(t, stagingPatches(), threeBases())
	e.fake.schema.Resources["aws_ecs_cluster"] = ecsClusterSchema()
	bases := threeBases()
	app := bases["app"]
	app.Resources = append(app.Resources, DesiredResource{Key: "app.cluster", Type: "aws_ecs_cluster", Config: map[string]any{"name": "app-cluster"}})
	bases["app"] = app
	hashes["app"] = e.writeIntent("app", "app v2, on the cluster app-cluster")
	writeBaseLock(t, base, hashes, map[string]Desired{"app": bases["app"]})

	p := e.plan()
	if len(bp.calls) != 0 {
		t.Fatalf("calls = %v, want none", bp.calls)
	}
	if got := configOf(p.desired["app"], "app.cluster")["name"]; got != "app-cluster" {
		t.Fatalf("cluster name = %v, want the one the text states", got)
	}
}
