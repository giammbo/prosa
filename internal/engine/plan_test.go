package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"

	"github.com/giammbo/prosa/internal/intent"
	"github.com/giammbo/prosa/internal/lock"
	"github.com/giammbo/prosa/internal/provider"
	"github.com/giammbo/prosa/internal/state"
)

// plannerFunc adapts a function to Planner.
type plannerFunc func(ctx context.Context, in PlanInput) (Desired, error)

func (f plannerFunc) Plan(ctx context.Context, in PlanInput) (Desired, error) { return f(ctx, in) }

type testEnv struct {
	t             *testing.T
	dir, lockPath string
	store         *state.Store
	fake          *fakeProvider
	providerCalls int
	engine        *Engine
}

func newTestEnv(t *testing.T, planner Planner) *testEnv {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "intents")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(filepath.Join(root, ".prosa", "state"), "passphrase")
	if err != nil {
		t.Fatal(err)
	}
	e := &testEnv{t: t, dir: dir, lockPath: filepath.Join(root, "prosa.lock.yaml"), store: store, fake: newFakeProvider()}
	e.engine = &Engine{
		Dir:      dir,
		LockPath: e.lockPath,
		State:    store,
		Planner:  planner,
		Providers: func(_ context.Context, name, version string) (Provider, error) {
			e.providerCalls++
			if name != "hashicorp/aws" {
				t.Errorf("provider name = %q", name)
			}
			return e.fake, nil
		},
		Now: func() time.Time { return time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC) },
	}
	return e
}

func hashOf(text string) string {
	sum := sha256.Sum256([]byte(text))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// writeIntent writes <name>.md and returns its hash.
func (e *testEnv) writeIntent(name, text string) string {
	e.t.Helper()
	if err := os.WriteFile(filepath.Join(e.dir, name+".md"), []byte(text), 0o644); err != nil {
		e.t.Fatal(err)
	}
	return hashOf(text)
}

type seedRes struct {
	key, typ  string
	chosen    map[string]any
	state     map[string]any // full provider state, id included
	dependsOn []string       // the lock's depends_on
}

// seed records intentName as already applied: state store, lock entry and
// the fake's remote objects.
func (e *testEnv) seed(intentName, hash string, res ...seedRes) {
	e.t.Helper()
	is := &state.IntentState{Resources: map[string]*state.ResourceState{}}
	var lres []lock.Resource
	for _, r := range res {
		ty := e.fake.schema.Resources[r.typ].Block.ImpliedType()
		js, err := json.Marshal(r.state)
		if err != nil {
			e.t.Fatal(err)
		}
		v, err := ctyjson.Unmarshal(js, ty)
		if err != nil {
			e.t.Fatal(err)
		}
		stJSON, err := ctyjson.Marshal(v, ty)
		if err != nil {
			e.t.Fatal(err)
		}
		id := r.state["id"].(string)
		e.fake.remote[id] = v
		is.Resources[r.key] = &state.ResourceState{Provider: "hashicorp/aws", Type: r.typ, SchemaVersion: e.fake.schema.Resources[r.typ].Version, State: stJSON}
		lres = append(lres, lock.Resource{Key: r.key, Provider: "hashicorp/aws", Type: r.typ, ID: id, Chosen: r.chosen, DependsOn: r.dependsOn})
	}
	digest, err := e.store.Save(intentName, is)
	if err != nil {
		e.t.Fatal(err)
	}
	lk, err := lock.Load(e.lockPath)
	if err != nil {
		e.t.Fatal(err)
	}
	lk.ProviderVersions["hashicorp/aws"] = "1.0.0"
	lk.Intents[intentName] = &lock.Intent{
		IntentHash: hash,
		State:      lock.StateRef{Serial: is.Serial, Digest: digest},
		Resources:  lres,
	}
	if _, err := lock.Save(e.lockPath, lk); err != nil {
		e.t.Fatal(err)
	}
}

func (e *testEnv) plan() *Plan {
	e.t.Helper()
	p, err := e.engine.Plan(context.Background())
	if err != nil {
		e.t.Fatalf("Plan: %v", err)
	}
	return p
}

func summary(p *Plan) []string {
	var out []string
	for _, c := range p.Changes {
		out = append(out, string(c.Action)+" "+c.Intent+"/"+c.Key)
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

var failingPlanner = plannerFunc(func(_ context.Context, in PlanInput) (Desired, error) {
	return Desired{}, errors.New("planner must not be called for " + in.Intent.Name)
})

// appDesired is an app intent with a net and a db that refers to it; the db
// comes first to prove that the order comes from the references.
func appDesired() Desired {
	return Desired{Resources: []DesiredResource{
		{Key: "db", Type: "test_db", Config: map[string]any{"name": "db", "net_id": "${app/net.id}", "size": 10}},
		{Key: "net", Type: "test_net", Config: map[string]any{"name": "net"}},
	}}
}

func TestPlanCreatesInDependencyOrder(t *testing.T) {
	text := "app with a net and a db"
	planner := StaticPlanner{hashOf(text): appDesired()}
	e := newTestEnv(t, planner)
	e.writeIntent("app", text)

	p := e.plan()
	if got, want := summary(p), []string{"create app/net", "create app/db"}; !equalStrings(got, want) {
		t.Fatalf("changes = %v, want %v", got, want)
	}
	if !p.HasChanges() || !p.LockDirty || p.Blocked != "" {
		t.Fatalf("HasChanges=%v LockDirty=%v Blocked=%q", p.HasChanges(), p.LockDirty, p.Blocked)
	}
	db := p.Changes[1]
	if db.Type != "test_db" || !db.Before.IsNull() {
		t.Fatalf("db change: type %q, before %#v", db.Type, db.Before)
	}
	if db.After.GetAttr("net_id").IsKnown() {
		t.Fatalf("db.net_id = %#v, want unknown", db.After.GetAttr("net_id"))
	}
	if got := db.After.GetAttr("size"); !got.RawEquals(cty.NumberIntVal(10)) {
		t.Fatalf("db.size = %#v", got)
	}
}

func TestPlanAddsOwnershipTag(t *testing.T) {
	text := "tagged"
	d := Desired{Resources: []DesiredResource{
		{Key: "net", Type: "test_net", Config: map[string]any{
			"name": "net",
			"tags": map[string]any{"prosa/intent": "someone-else", "team": "core"},
		}},
	}}
	e := newTestEnv(t, StaticPlanner{hashOf(text): d})
	e.writeIntent("app", text)

	p := e.plan()
	tags := p.Changes[0].After.GetAttr("tags")
	want := cty.MapVal(map[string]cty.Value{"prosa/intent": cty.StringVal("app"), "team": cty.StringVal("core")})
	if !tags.RawEquals(want) {
		t.Fatalf("tags = %#v, want %#v", tags, want)
	}
	if got := d.Resources[0].Config["tags"].(map[string]any)["prosa/intent"]; got != "someone-else" {
		t.Fatalf("planner config modified: prosa/intent = %v", got)
	}
}

func TestPlanUnchangedIntentSkipsPlanner(t *testing.T) {
	e := newTestEnv(t, failingPlanner)
	h := e.writeIntent("app", "unchanged")
	owner := map[string]any{"prosa/intent": "app"}
	e.seed("app", h,
		seedRes{key: "net", typ: "test_net", chosen: map[string]any{"name": "net"},
			state: map[string]any{"id": "test_net-1", "name": "net", "tags": owner}},
		seedRes{key: "db", typ: "test_db", chosen: map[string]any{"name": "db", "net_id": "${app/net.id}", "size": 10},
			state: map[string]any{"id": "test_db-1", "name": "db", "net_id": "test_net-1", "size": 10, "tags": owner}},
	)

	p := e.plan()
	if got, want := summary(p), []string{"no-op app/net", "no-op app/db"}; !equalStrings(got, want) {
		t.Fatalf("changes = %v, want %v", got, want)
	}
	if p.HasChanges() || p.LockDirty || p.Blocked != "" {
		t.Fatalf("HasChanges=%v LockDirty=%v Blocked=%q", p.HasChanges(), p.LockDirty, p.Blocked)
	}
	for _, c := range p.Changes {
		if c.Drifted {
			t.Errorf("%s drifted", c.Key)
		}
	}
	var buf bytes.Buffer
	Render(&buf, p, RenderOptions{})
	if buf.String() != "No changes. Your infrastructure matches the intents.\n" {
		t.Fatalf("render = %q", buf.String())
	}
}

func TestPlanRejectsSensitiveLiteral(t *testing.T) {
	text := "db with a literal password"
	d := Desired{Resources: []DesiredResource{
		{Key: "db", Type: "test_db", Config: map[string]any{"name": "db", "password": "x"}},
	}}
	e := newTestEnv(t, StaticPlanner{hashOf(text): d})
	e.writeIntent("app", text)

	_, err := e.engine.Plan(context.Background())
	if err == nil || !strings.Contains(err.Error(), "sensitive") || !strings.Contains(err.Error(), "ADR-011") {
		t.Fatalf("err = %v, want a sensitive/ADR-011 error", err)
	}
	if !strings.Contains(err.Error(), "password") || !strings.Contains(err.Error(), "app/db") {
		t.Fatalf("err = %v, want it to name the attribute and the resource", err)
	}
}

func TestPlanRecreatesResourceGoneOutOfBand(t *testing.T) {
	e := newTestEnv(t, failingPlanner)
	h := e.writeIntent("app", "just a net")
	e.seed("app", h, seedRes{key: "net", typ: "test_net", chosen: map[string]any{"name": "net"},
		state: map[string]any{"id": "test_net-1", "name": "net", "tags": map[string]any{"prosa/intent": "app"}}})
	delete(e.fake.remote, "test_net-1")

	p := e.plan()
	if got, want := summary(p), []string{"create app/net"}; !equalStrings(got, want) {
		t.Fatalf("changes = %v, want %v", got, want)
	}
	c := p.Changes[0]
	if !c.Drifted || !c.Before.IsNull() {
		t.Fatalf("Drifted=%v Before=%#v", c.Drifted, c.Before)
	}
	if c.After.GetAttr("id").IsKnown() {
		t.Fatalf("id = %#v, want unknown", c.After.GetAttr("id"))
	}
	var buf bytes.Buffer
	Render(&buf, p, RenderOptions{})
	want := "Prosa will perform the following actions:\n\n" +
		"  # app/net will be created\n" +
		"  # (needs approval)\n" +
		"  # (drifted outside Prosa: the object is gone)\n" +
		"  + resource \"test_net\" \"net\" {\n" +
		"      + id   = (known after apply)\n" +
		"      + name = \"net\"\n" +
		"      + tags = {\n" +
		"          + \"prosa/intent\" = \"app\"\n" +
		"        }\n" +
		"    }\n\n" +
		"Plan: 1 to add, 0 to change, 0 to destroy.\n"
	if buf.String() != want {
		t.Fatalf("render = %q, want %q", buf.String(), want)
	}
}

func TestPlanRemovedIntentDeletesReverseOrder(t *testing.T) {
	e := newTestEnv(t, failingPlanner)
	owner := map[string]any{"prosa/intent": "old"}
	e.seed("old", hashOf("old text"),
		seedRes{key: "net", typ: "test_net", chosen: map[string]any{"name": "net"},
			state: map[string]any{"id": "test_net-1", "name": "net", "tags": owner}},
		seedRes{key: "db", typ: "test_db", chosen: map[string]any{"name": "db", "net_id": "${old/net.id}"},
			state: map[string]any{"id": "test_db-1", "name": "db", "net_id": "test_net-1", "tags": owner}},
	)

	p := e.plan()
	if got, want := summary(p), []string{"delete old/db", "delete old/net"}; !equalStrings(got, want) {
		t.Fatalf("changes = %v, want %v", got, want)
	}
	for _, c := range p.Changes {
		if c.Before.IsNull() || !c.After.IsNull() {
			t.Errorf("%s: before %#v after %#v", c.Key, c.Before, c.After)
		}
	}
	if !p.LockDirty {
		t.Error("LockDirty = false for a removed intent")
	}
	var buf bytes.Buffer
	Render(&buf, p, RenderOptions{})
	want := "Prosa will perform the following actions:\n\n" +
		"  # old/db will be destroyed\n" +
		"  # (needs approval: destroy)\n" +
		"  - resource \"test_db\" \"db\" {\n" +
		"      - id   = \"test_db-1\" -> null\n" +
		"      - name = \"db\" -> null\n" +
		"        # (2 unchanged attributes hidden)\n" +
		"    }\n\n" +
		"  # old/net will be destroyed\n" +
		"  # (needs approval: destroy)\n" +
		"  - resource \"test_net\" \"net\" {\n" +
		"      - id   = \"test_net-1\" -> null\n" +
		"      - name = \"net\" -> null\n" +
		"        # (1 unchanged attribute hidden)\n" +
		"    }\n\n" +
		"Plan: 0 to add, 0 to change, 2 to destroy.\n"
	if buf.String() != want {
		t.Fatalf("render = %q, want %q", buf.String(), want)
	}
}

func TestPlanRemovedIntentStillReferenced(t *testing.T) {
	text := "app on the network"
	d := Desired{Resources: []DesiredResource{
		{Key: "db", Type: "test_db", Config: map[string]any{"name": "db", "net_id": "${network/net.id}"}},
	}}
	e := newTestEnv(t, StaticPlanner{hashOf(text): d})
	e.writeIntent("app", text)
	e.seed("network", hashOf("network text"), seedRes{key: "net", typ: "test_net", chosen: map[string]any{"name": "net"},
		state: map[string]any{"id": "test_net-1", "name": "net"}})

	_, err := e.engine.Plan(context.Background())
	var ve *ValidationError
	if !errors.As(err, &ve) || !strings.Contains(err.Error(), `"app"`) || !strings.Contains(err.Error(), `"network"`) {
		t.Fatalf("err = %v, want a ValidationError naming app and network", err)
	}
	for _, c := range e.fake.calls {
		if c != "schema" {
			t.Fatalf("provider used after the reference check failed: %v", e.fake.calls)
		}
	}
}

func TestPlanDigestMismatchBlocks(t *testing.T) {
	e := newTestEnv(t, failingPlanner)
	h := e.writeIntent("app", "a net")
	e.seed("app", h, seedRes{key: "net", typ: "test_net", chosen: map[string]any{"name": "net"},
		state: map[string]any{"id": "test_net-1", "name": "net", "tags": map[string]any{"prosa/intent": "app"}}})
	// Someone saves the state without updating the lock.
	is, _, err := e.store.Load("app")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.Save("app", is); err != nil {
		t.Fatal(err)
	}

	p := e.plan()
	if !strings.Contains(p.Blocked, `"app"`) {
		t.Fatalf("Blocked = %q, want a digest mismatch naming app", p.Blocked)
	}
	if len(p.Changes) != 0 || len(e.fake.calls) != 0 {
		t.Fatalf("blocked plan has changes %v, calls %v", summary(p), e.fake.calls)
	}
	var buf bytes.Buffer
	Render(&buf, p, RenderOptions{})
	if !strings.Contains(buf.String(), p.Blocked) {
		t.Fatalf("render = %q, want the blocked reason", buf.String())
	}
}

func TestRenderUpdate(t *testing.T) {
	text := "bigger db"
	d := Desired{Resources: []DesiredResource{
		{Key: "db", Type: "test_db", Config: map[string]any{"name": "db", "size": 20}},
	}}
	e := newTestEnv(t, StaticPlanner{hashOf(text): d})
	e.writeIntent("app", text)
	e.seed("app", hashOf("small db"), seedRes{key: "db", typ: "test_db", chosen: map[string]any{"name": "db", "size": 10},
		state: map[string]any{"id": "test_db-1", "name": "db", "size": 10, "tags": map[string]any{"prosa/intent": "app"}}})

	p := e.plan()
	if got, want := summary(p), []string{"update app/db"}; !equalStrings(got, want) {
		t.Fatalf("changes = %v, want %v", got, want)
	}
	if !p.LockDirty {
		t.Error("LockDirty = false for a changed intent")
	}
	var buf bytes.Buffer
	Render(&buf, p, RenderOptions{})
	out := buf.String()
	if !strings.Contains(out, "  # app/db will be updated in-place\n  # (needs approval)\n") || !strings.Contains(out, "      ~ size = 10 -> 20\n") {
		t.Fatalf("render = %q", out)
	}
	if strings.Contains(out, "name ") || strings.Contains(out, "drift") {
		t.Fatalf("render shows unchanged attributes or drift: %q", out)
	}
}

func TestPlanReplaceUsesCreatePlan(t *testing.T) {
	text := "renamed net"
	d := Desired{Resources: []DesiredResource{{Key: "net", Type: "test_net", Config: map[string]any{"name": "net2"}}}}
	e := newTestEnv(t, StaticPlanner{hashOf(text): d})
	e.fake.replaceOn["test_net"] = []string{"name"}
	e.writeIntent("app", text)
	e.seed("app", hashOf("net"), seedRes{key: "net", typ: "test_net", chosen: map[string]any{"name": "net"},
		state: map[string]any{"id": "test_net-1", "name": "net", "tags": map[string]any{"prosa/intent": "app"}}})

	p := e.plan()
	if got, want := summary(p), []string{"replace app/net"}; !equalStrings(got, want) {
		t.Fatalf("changes = %v, want %v", got, want)
	}
	c := p.Changes[0]
	if !equalStrings(c.RequiresReplace, []string{"name"}) || c.After.GetAttr("id").IsKnown() {
		t.Fatalf("RequiresReplace=%v after.id=%#v", c.RequiresReplace, c.After.GetAttr("id"))
	}
	var buf bytes.Buffer
	Render(&buf, p, RenderOptions{})
	want := "Prosa will perform the following actions:\n\n" +
		"  # app/net must be replaced\n" +
		"  # (needs approval: destroy)\n" +
		"-/+ resource \"test_net\" \"net\" {\n" +
		"      ~ id   = \"test_net-1\" -> (known after apply)\n" +
		"      ~ name = \"net\" -> \"net2\" # forces replacement\n" +
		"        # (1 unchanged attribute hidden)\n" +
		"    }\n\n" +
		"Plan: 1 to add, 0 to change, 1 to destroy.\n"
	if buf.String() != want {
		t.Fatalf("render = %q, want %q", buf.String(), want)
	}
}

func TestRenderSensitiveAndNoChanges(t *testing.T) {
	fake := newFakeProvider()
	obj := func(pw string, size int64) cty.Value {
		return cty.ObjectVal(map[string]cty.Value{
			"id": cty.StringVal("test_db-1"), "name": cty.StringVal("db"), "net_id": cty.NullVal(cty.String),
			"size": cty.NumberIntVal(size), "password": cty.StringVal(pw), "tags": cty.NullVal(cty.Map(cty.String)),
		})
	}
	p := &Plan{schema: fake.schema, Changes: []Change{
		{Intent: "app", Key: "db", Type: "test_db", Action: Update, Before: obj("a", 1), After: obj("b", 1)},
		{Intent: "app", Key: "same", Type: "test_db", Action: NoOp, Before: obj("a", 1), After: obj("a", 1)},
	}}
	var buf bytes.Buffer
	Render(&buf, p, RenderOptions{})
	want := "Prosa will perform the following actions:\n\n" +
		"  # app/db will be updated in-place\n" +
		"  ~ resource \"test_db\" \"db\" {\n" +
		"        id       = \"test_db-1\"\n" +
		"      ~ password = (sensitive value)\n" +
		"        # (2 unchanged attributes hidden)\n" +
		"    }\n\n" +
		"Plan: 0 to add, 1 to change, 0 to destroy.\n"
	if buf.String() != want {
		t.Fatalf("render = %q, want %q", buf.String(), want)
	}
	buf.Reset()
	Render(&buf, &Plan{}, RenderOptions{})
	if buf.String() != "No changes. Your infrastructure matches the intents.\n" {
		t.Fatalf("empty plan render = %q", buf.String())
	}
}

func TestStaticPlannerMissingFixture(t *testing.T) {
	_, err := StaticPlanner{}.Plan(context.Background(), PlanInput{})
	if err == nil || !strings.Contains(err.Error(), "no fixture for intent") {
		t.Fatalf("err = %v", err)
	}
	// With an environment the error names the effective hash and the
	// intent's own, the two keys tried.
	it := intent.Intent{Name: "app", Hash: "sha256:aa"}
	env := &intent.Env{Name: "staging", Hash: "sha256:bb"}
	_, err = StaticPlanner{}.Plan(context.Background(), PlanInput{Intent: it, Env: env})
	if err == nil || !strings.Contains(err.Error(), "effective hash "+IntentHash(it, env)) || !strings.Contains(err.Error(), "intent hash sha256:aa") {
		t.Fatalf("err = %v", err)
	}
}

func TestAWSProviderConfigEndpoints(t *testing.T) {
	b := &provider.Block{
		Attributes: map[string]*provider.Attribute{
			"region":                      {Type: cty.String, Optional: true},
			"access_key":                  {Type: cty.String, Optional: true},
			"secret_key":                  {Type: cty.String, Optional: true},
			"skip_credentials_validation": {Type: cty.Bool, Optional: true},
			"skip_requesting_account_id":  {Type: cty.Bool, Optional: true},
			"skip_region_validation":      {Type: cty.Bool, Optional: true},
			"skip_metadata_api_check":     {Type: cty.String, Optional: true},
		},
		BlockTypes: map[string]*provider.NestedBlock{
			"endpoints": {Nesting: provider.NestingSet, Block: &provider.Block{
				Attributes: map[string]*provider.Attribute{"rds": {Type: cty.String, Optional: true}},
				BlockTypes: map[string]*provider.NestedBlock{},
			}},
		},
	}
	const ep = "http://localhost:5555"

	t.Setenv("AWS_ACCESS_KEY_ID", "")
	m, err := AWSProviderConfig(b, AWSSettings{Region: "eu-south-1", Endpoint: ep})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m["endpoints"]; ok {
		t.Fatalf("endpoints block set: the endpoint goes through AWS_ENDPOINT_URL: %v", m)
	}
	if m["region"] != "eu-south-1" || m["access_key"] != "test" || m["secret_key"] != "test" ||
		m["skip_credentials_validation"] != true || m["skip_requesting_account_id"] != true ||
		m["skip_region_validation"] != true || m["skip_metadata_api_check"] != "true" || len(m) != 7 {
		t.Fatalf("config = %v", m)
	}
	if _, err := provider.ObjectFromMap(b, m); err != nil {
		t.Fatalf("config does not fit the schema: %v", err)
	}
	if got := awsProviderEnv(ep, nil); !equalStrings(got, []string{"AWS_ENDPOINT_URL=" + ep}) {
		t.Fatalf("AWSProviderEnv = %v", got)
	}
	if got := awsProviderEnv("", nil); got != nil {
		t.Fatalf("AWSProviderEnv without endpoint = %v, want nil", got)
	}
	host := []string{"PATH=/bin", "AWS_ENDPOINT_URL=http://real", "AWS_ENDPOINT_URL_S3=http://s3", "AWS_ENDPOINT_URL_RDS=http://rds", "AWS_ENDPOINT_URLX=keep", "AWS_REGION=x"}
	if got := awsProviderEnv("", host); !equalStrings(got, []string{"AWS_ENDPOINT_URL", "AWS_ENDPOINT_URL_RDS", "AWS_ENDPOINT_URL_S3"}) {
		t.Fatalf("env without endpoint = %v, want the host endpoint variables removed", got)
	}
	if got := awsProviderEnv(ep, host); !equalStrings(got, []string{"AWS_ENDPOINT_URL_RDS", "AWS_ENDPOINT_URL_S3", "AWS_ENDPOINT_URL=" + ep}) {
		t.Fatalf("env with endpoint = %v", got)
	}

	t.Setenv("AWS_ACCESS_KEY_ID", "AKIAEXAMPLE")
	m, err = AWSProviderConfig(b, AWSSettings{Region: "eu-south-1", Endpoint: ep})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m["access_key"]; ok {
		t.Fatalf("test credentials set although AWS_ACCESS_KEY_ID is: %v", m)
	}

	t.Setenv("AWS_ACCESS_KEY_ID", "")
	m, err = AWSProviderConfig(b, AWSSettings{Region: "eu-west-1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != 1 || m["region"] != "eu-west-1" {
		t.Fatalf("config without endpoint = %v, want only the region", m)
	}
}

// awsConfigBlock has the shape of the hashicorp/aws 6 configuration
// schema for what Prosa sets and must not set (ADR-029).
var awsConfigBlock = &provider.Block{
	Attributes: map[string]*provider.Attribute{
		"region":                      {Type: cty.String, Optional: true},
		"profile":                     {Type: cty.String, Optional: true},
		"allowed_account_ids":         {Type: cty.Set(cty.String), Optional: true},
		"access_key":                  {Type: cty.String, Optional: true},
		"secret_key":                  {Type: cty.String, Optional: true},
		"skip_credentials_validation": {Type: cty.Bool, Optional: true},
		"skip_requesting_account_id":  {Type: cty.Bool, Optional: true},
		"skip_region_validation":      {Type: cty.Bool, Optional: true},
		"skip_metadata_api_check":     {Type: cty.String, Optional: true},
	},
	BlockTypes: map[string]*provider.NestedBlock{
		"assume_role": {Nesting: provider.NestingList, Block: &provider.Block{
			Attributes: map[string]*provider.Attribute{
				"role_arn":     {Type: cty.String, Optional: true},
				"session_name": {Type: cty.String, Optional: true},
				"external_id":  {Type: cty.String, Optional: true},
				"duration":     {Type: cty.String, Optional: true},
			},
		}},
		"default_tags": {Nesting: provider.NestingList, Block: &provider.Block{
			Attributes: map[string]*provider.Attribute{"tags": {Type: cty.Map(cty.String), Optional: true}},
		}},
	},
}

// The provider gets the region and nothing about the identity: the account,
// the role and the profile are the ones of the SDK's credential chain.
func TestAWSProviderConfigNoIdentity(t *testing.T) {
	for _, s := range []AWSSettings{
		{Region: "eu-south-1"},
		{Region: "eu-south-1", Endpoint: "http://localhost:5555"},
	} {
		m, err := AWSProviderConfig(awsConfigBlock, s)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := provider.ObjectFromMap(awsConfigBlock, m); err != nil {
			t.Fatalf("config does not fit the schema: %v", err)
		}
		if m["region"] != "eu-south-1" {
			t.Errorf("%+v: region = %v", s, m["region"])
		}
		for _, k := range []string{"allowed_account_ids", "assume_role", "profile", "default_tags"} {
			if _, ok := m[k]; ok {
				t.Errorf("%+v: %s = %v, want it unset", s, k, m[k])
			}
		}
	}
}

func TestPlanDroppedResourceDeletedAlongsideOthers(t *testing.T) {
	text := "net and cache, no db"
	d := Desired{Resources: []DesiredResource{
		{Key: "net", Type: "test_net", Config: map[string]any{"name": "net"}},
		{Key: "cache", Type: "test_net", Config: map[string]any{"name": "cache"}},
	}}
	e := newTestEnv(t, StaticPlanner{hashOf(text): d})
	e.writeIntent("app", text)
	owner := map[string]any{"prosa/intent": "app"}
	e.seed("app", hashOf("net and db"),
		seedRes{key: "net", typ: "test_net", chosen: map[string]any{"name": "net"},
			state: map[string]any{"id": "test_net-1", "name": "net", "tags": owner}},
		seedRes{key: "db", typ: "test_db", chosen: map[string]any{"name": "db", "net_id": "${app/net.id}"},
			state: map[string]any{"id": "test_db-1", "name": "db", "net_id": "test_net-1", "tags": owner}},
	)

	p := e.plan()
	if got, want := summary(p), []string{"delete app/db", "create app/cache", "no-op app/net"}; !equalStrings(got, want) {
		t.Fatalf("changes = %v, want %v", got, want)
	}
	if del := p.Changes[0]; del.Before.IsNull() || !del.After.IsNull() || del.Drifted {
		t.Fatalf("delete: before %#v after %#v drifted %v", del.Before, del.After, del.Drifted)
	}
}

func TestPlanRefToMissingAttribute(t *testing.T) {
	text := "db refers to a missing attribute"
	d := Desired{Resources: []DesiredResource{
		{Key: "net", Type: "test_net", Config: map[string]any{"name": "net"}},
		{Key: "db", Type: "test_db", Config: map[string]any{"name": "db", "net_id": "${app/net.nope}"}},
	}}
	e := newTestEnv(t, StaticPlanner{hashOf(text): d})
	e.writeIntent("app", text)

	_, err := e.engine.Plan(context.Background())
	if err == nil || !strings.Contains(err.Error(), `"nope"`) || !strings.Contains(err.Error(), "${app/net.nope}") {
		t.Fatalf("err = %v, want an error naming the missing attribute", err)
	}
}

func TestPlanDependentOfUpdateUsesPlannedAfter(t *testing.T) {
	text := "renamed net, same db"
	d := Desired{Resources: []DesiredResource{
		{Key: "net", Type: "test_net", Config: map[string]any{"name": "net2"}},
		{Key: "db", Type: "test_db", Config: map[string]any{"name": "db", "net_id": "${app/net.id}"}},
	}}
	e := newTestEnv(t, StaticPlanner{hashOf(text): d})
	e.fake.unknownOnUpdate["test_net"] = []string{"id"}
	e.writeIntent("app", text)
	owner := map[string]any{"prosa/intent": "app"}
	e.seed("app", hashOf("net and db"),
		seedRes{key: "net", typ: "test_net", chosen: map[string]any{"name": "net"},
			state: map[string]any{"id": "test_net-1", "name": "net", "tags": owner}},
		seedRes{key: "db", typ: "test_db", chosen: map[string]any{"name": "db", "net_id": "${app/net.id}"},
			state: map[string]any{"id": "test_db-1", "name": "db", "net_id": "test_net-1", "tags": owner}},
	)

	p := e.plan()
	// From the refreshed state db.net_id would stay "test_net-1" (a no-op);
	// from the planned After of the net update it is unknown.
	if got, want := summary(p), []string{"update app/net", "update app/db"}; !equalStrings(got, want) {
		t.Fatalf("changes = %v, want %v", got, want)
	}
	if v := p.Changes[1].After.GetAttr("net_id"); v.IsKnown() {
		t.Fatalf("db.net_id = %#v, want unknown", v)
	}
}

func TestRenderMasksNestedSensitive(t *testing.T) {
	auth := &provider.Block{
		Attributes: map[string]*provider.Attribute{
			"client_id":     {Type: cty.String, Optional: true},
			"client_secret": {Type: cty.String, Optional: true, Sensitive: true},
		},
		BlockTypes: map[string]*provider.NestedBlock{},
	}
	schema := &provider.Schema{Resources: map[string]*provider.ResourceSchema{
		"test_app": {Block: &provider.Block{
			Attributes: map[string]*provider.Attribute{"name": {Type: cty.String, Optional: true}},
			BlockTypes: map[string]*provider.NestedBlock{"auth": {Nesting: provider.NestingList, Block: auth}},
		}},
	}}
	obj := func(secret string) cty.Value {
		return cty.ObjectVal(map[string]cty.Value{
			"name": cty.StringVal("app"),
			"auth": cty.ListVal([]cty.Value{cty.ObjectVal(map[string]cty.Value{
				"client_id": cty.StringVal("cid"), "client_secret": cty.StringVal(secret),
			})}),
		})
	}
	p := &Plan{schema: schema, Changes: []Change{
		{Intent: "app", Key: "app", Type: "test_app", Action: Update, Before: obj("old-s3cret"), After: obj("new-s3cret")},
	}}
	var buf bytes.Buffer
	Render(&buf, p, RenderOptions{})
	out := buf.String()
	if strings.Contains(out, "s3cret") {
		t.Fatalf("render leaks a nested sensitive value: %q", out)
	}
	want := "      ~ auth {\n          ~ client_secret = (sensitive value)\n            # (1 unchanged attribute hidden)\n        }\n"
	if !strings.Contains(out, want) {
		t.Fatalf("render = %q, want it to contain %q", out, want)
	}
}

func TestPlanSchemaVersionMismatch(t *testing.T) {
	e := newTestEnv(t, failingPlanner)
	h := e.writeIntent("app", "a secret")
	e.seed("app", h, seedRes{key: "s", typ: "test_secret", chosen: map[string]any{"name": "s"},
		state: map[string]any{"id": "test_secret-1", "name": "s"}})
	st, _ := e.loadState("app")
	st.Resources["s"].SchemaVersion = 1
	digest, err := e.store.Save("app", st)
	if err != nil {
		t.Fatal(err)
	}
	lk := e.loadLock()
	lk.Intents["app"].State = lock.StateRef{Serial: st.Serial, Digest: digest}
	if _, err := lock.Save(e.lockPath, lk); err != nil {
		t.Fatal(err)
	}

	_, err = e.engine.Plan(context.Background())
	if want := "state of app/s was written with schema version 1, provider has 2: upgrade not supported yet"; err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("err = %v, want %q", err, want)
	}
	for _, c := range e.fake.calls {
		if strings.HasPrefix(c, "read ") {
			t.Fatalf("stale state sent to the provider: %v", e.fake.calls)
		}
	}
}

func TestPlanBlockedNeverCallsPlanner(t *testing.T) {
	calls := 0
	planner := plannerFunc(func(context.Context, PlanInput) (Desired, error) {
		calls++
		return Desired{}, nil
	})
	e := newTestEnv(t, planner)
	e.writeIntent("app", "a rewritten net")
	e.seed("app", hashOf("a net"), seedRes{key: "net", typ: "test_net", chosen: map[string]any{"name": "net"},
		state: map[string]any{"id": "test_net-1", "name": "net"}})
	is, _, _ := e.store.Load("app")
	if _, err := e.store.Save("app", is); err != nil { // the digest no longer matches the lock
		t.Fatal(err)
	}

	p := e.plan()
	if p.Blocked == "" || calls != 0 {
		t.Fatalf("Blocked = %q, planner calls = %d; want blocked without calling the planner", p.Blocked, calls)
	}
}
