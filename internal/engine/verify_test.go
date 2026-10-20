package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/zclconf/go-cty/cty"

	"github.com/giammbo/prosa/internal/checks"
	"github.com/giammbo/prosa/internal/lock"
)

// baseDesired is the intent the checks of app may read through depends_on.
func baseDesired() Desired {
	return Desired{Resources: []DesiredResource{
		{Key: "db", Type: "test_db", Config: map[string]any{"name": "db", "size": 10}},
		{Key: "clock", Type: "test_clock", Config: map[string]any{"name": "clock"}},
	}}
}

func TestValidateChecks(t *testing.T) {
	rp := &recordingPlanner{desired: map[string]Desired{
		"base":  baseDesired(),
		"other": {Resources: []DesiredResource{{Key: "net", Type: "test_net", Config: map[string]any{"name": "other"}}}},
		"app": {Resources: []DesiredResource{
			{Key: "net", Type: "test_net", Config: map[string]any{"name": "net"}},
			{Key: "lb", Type: "test_lb", Config: map[string]any{"name": "lb"}},
			{Key: "list", Type: "test_list", Config: map[string]any{"name": "list"}},
		}},
	}}
	e := newTestEnv(t, rp)
	e.writeIntent("base", "base")
	e.writeIntent("other", "other")
	e.writeIntent("app", "---\ndepends_on: [base]\n---\napp\n")

	attr := func(name, resource, path, op string, value any) lock.Check {
		return lock.Check{Name: name, Type: "resource_attr", Resource: resource, Path: path, Op: op, Value: value}
	}
	bad := []lock.Check{
		attr("", "app/net", "name", "exists", nil),
		attr("dup", "app/net", "name", "exists", nil),
		attr("dup", "app/net", "id", "exists", nil),
		{Name: "odd", Type: "odd"},
		{Name: "foreign", Type: "resource_attr", Resource: "app/net", Path: "name", Op: "exists", URL: "https://x", MinDays: 3},
		{Name: "bare", Type: "resource_attr"},
		attr("form", "net", "name", "exists", nil),
		attr("outside", "other/net", "name", "exists", nil),
		attr("nokey", "base/nope", "name", "exists", nil),
		attr("nopath", "base/db", "nope", "exists", nil),
		attr("set", "app/lb", "opts.inner.0.flag", "exists", nil),
		attr("op", "base/db", "size", "is", 10),
		attr("novalue", "base/db", "size", "eq", nil),
		attr("existsvalue", "base/db", "size", "exists", 10),
		attr("gte", "base/db", "name", "gte", 1),
		attr("fit", "base/db", "size", "eq", "ten"),
		attr("contains", "base/db", "size", "contains", 1),
		attr("matches", "base/db", "name", "matches", "("),
		attr("within", "base/db", "size", "within", "24h"),
		attr("duration", "base/clock", "read_at", "within", "1 day"),
		attr("secret", "base/db", "password", "eq", "x"),
		attr("number", "base/db", "size", "eq", json.Number("1e999")),
		{Name: "nourl", Type: "http_get"},
		{Name: "http", Type: "http_get", URL: "http://staging.acme.example/health"},
		{Name: "status", Type: "http_get", URL: "https://staging.acme.example/health", Status: 99, Within: "soon"},
		{Name: "nohost", Type: "tls_days"},
		{Name: "host", Type: "tls_days", Host: "https://staging.acme.example", MinDays: 20},
		{Name: "noreason", Type: "unverifiable", Reason: " "},
	}
	good := []lock.Check{
		attr("name", "app/net", "name", "eq", "net"),
		attr("health", "app/lb", "health.0.path", "matches", "^/"),
		attr("items", "app/list", "items", "contains", "a"),
		attr("groups", "app/list", "groups.x", "exists", nil),
		attr("size", "base/db", "size", "gte", json.Number("10")),
		attr("fresh", "base/clock", "read_at", "within", "24h"),
		attr("password", "base/db", "password", "exists", nil),
		{Name: "health check", Type: "http_get", URL: "https://staging.acme.example/health", Status: 200, Within: "60s"},
		{Name: "default status", Type: "http_get", URL: "https://staging.acme.example/health"},
		{Name: "cert", Type: "tls_days", Host: "staging.acme.example", MinDays: 20},
		{Name: "later", Type: "unverifiable", Reason: "needs the run history"},
	}
	var badErr, goodErr error
	rp.check = func(ctx context.Context, in PlanInput) {
		if in.Intent.Name != "app" {
			return
		}
		d := rp.desired["app"]
		d.Checks = bad
		badErr = in.Validate(ctx, d)
		d.Checks = good
		goodErr = in.Validate(ctx, d)
	}
	e.plan()

	if goodErr != nil {
		t.Errorf("Validate(good checks) = %v", goodErr)
	}
	var ve *ValidationError
	if !errors.As(badErr, &ve) {
		t.Fatalf("Validate(bad checks) = %v, want a *ValidationError", badErr)
	}
	var got []string
	for _, p := range ve.Problems {
		if p.Intent != "app" {
			t.Errorf("problem on intent %q: %+v", p.Intent, p)
		}
		got = append(got, p.Key+": "+p.Message)
	}
	want := []string{
		"checks[0]: a check needs a name: the sentence of the Checks section it checks",
		`checks[2]: duplicate check name "dup": one check per sentence`,
		`checks[3]: unknown check type "odd": use resource_attr, http_get, tls_days or unverifiable`,
		"checks[4]: resource_attr does not use min_days, url",
		"checks[5]: resource_attr needs resource, path and op",
		`checks[6]: resource "net": expected "<intent>/<key>"`,
		`checks[7]: resource "other/net": a check can read only intent app and those in its depends_on (base)`,
		`checks[8]: resource "base/nope": unknown key nope`,
		`checks[9]: path "nope": base/db (test_db) has no attribute "nope"`,
		`checks[10]: path "opts.inner.0.flag": cannot select "0" in opts.inner, a set: its elements have no index`,
		`checks[11]: unknown op "is": use eq, ne, gte, lte, contains, matches, within or exists`,
		"checks[12]: op eq needs a value",
		"checks[13]: op exists takes no value",
		"checks[14]: op gte needs a number attribute; name is a string",
		`checks[15]: value "ten" does not fit size, a number`,
		"checks[16]: op contains needs a string, list or set attribute; size is a number",
		"checks[17]: value \"(\" is not a regular expression: error parsing regexp: missing closing ): `(`",
		"checks[18]: op within needs a string attribute holding a timestamp; size is a number",
		`checks[19]: value "1 day" is not a duration such as "24h"`,
		`checks[20]: path "password" reads a sensitive value: only op exists can check it (ADR-011)`,
		"checks[21]: value: number 1e999 out of range",
		"checks[22]: http_get needs url",
		`checks[23]: url "http://staging.acme.example/health" must be an https URL with a host`,
		"checks[24]: status 99 is not an HTTP status",
		`checks[24]: within "soon" is not a positive duration such as "60s"`,
		"checks[25]: tls_days needs host",
		"checks[25]: tls_days needs min_days, at least 1",
		`checks[26]: host "https://staging.acme.example" must be a host name, as in staging.acme.example`,
		"checks[27]: unverifiable needs a reason",
	}
	// Problems are sorted by key as text: checks[10] comes before checks[2].
	slices.Sort(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("problems:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

// checkedApp is an intent whose checks read its own resources and those of
// base, through depends_on.
func checkedApp(checks ...lock.Check) Desired {
	return Desired{
		Resources: []DesiredResource{{Key: "net", Type: "test_net", Config: map[string]any{"name": "net"}}},
		Checks:    checks,
	}
}

var (
	netID   = lock.Check{Name: "La rete ha un id.", Type: "resource_attr", Resource: "app/net", Path: "id", Op: "exists"}
	dbSize  = lock.Check{Name: "Il database ha 10 GiB.", Type: "resource_attr", Resource: "base/db", Path: "size", Op: "eq", Value: json.Number("10")}
	later   = lock.Check{Name: "Dopo la prima notte.", Type: "unverifiable", Reason: "needs the run history"}
	appText = "---\ndepends_on: [base]\n---\napp\n"
)

func TestChecksFrozenInLock(t *testing.T) {
	baseText := "base"
	planner := StaticPlanner{
		hashOf(baseText): baseDesired(),
		hashOf(appText):  checkedApp(netID, dbSize, later),
	}
	e := newTestEnv(t, planner)
	e.writeIntent("base", baseText)
	e.writeIntent("app", appText)
	e.mustApply(e.plan())

	li := e.loadLock().Intents["app"]
	want := []lock.Check{netID, {Name: dbSize.Name, Type: "resource_attr", Resource: "base/db", Path: "size", Op: "eq", Value: 10}, later}
	if !reflect.DeepEqual(li.Checks, want) {
		t.Fatalf("lock checks of app = %#v, want %#v", li.Checks, want)
	}
	if got := e.loadLock().Intents["base"].Checks; got != nil {
		t.Errorf("lock checks of base = %#v, want none", got)
	}

	// Unchanged intents: the checks come from the lock, nothing is written.
	lockBefore := e.readFile(e.lockPath)
	delete(planner, hashOf(appText))
	p := e.plan()
	if !reflect.DeepEqual(p.checks["app"], want) {
		t.Errorf("plan checks of app = %#v, want the lock's", p.checks["app"])
	}
	e.mustApply(p)
	if !bytes.Equal(e.readFile(e.lockPath), lockBefore) {
		t.Error("a run without changes rewrote the lock")
	}

	// A rewritten intent gets its new checks, even without infrastructure changes.
	text2 := appText + "\nmore prose\n"
	planner[hashOf(text2)] = checkedApp(netID)
	e.writeIntent("app", text2)
	e.mustApply(e.plan())
	if got := e.loadLock().Intents["app"].Checks; !reflect.DeepEqual(got, []lock.Check{netID}) {
		t.Errorf("lock checks after the rewrite = %#v, want only the net check", got)
	}
}

func TestApplyRunsChecksOfAppliedIntentsOnRefreshedState(t *testing.T) {
	baseText := "base"
	bad := lock.Check{Name: "Il database ha 20 GiB.", Type: "resource_attr", Resource: "base/db", Path: "size", Op: "eq", Value: 20}
	health := lock.Check{Name: "Risponde.", Type: "http_get", URL: "https://staging.acme.example/health"}
	baseCheck := lock.Check{Name: "Base.", Type: "resource_attr", Resource: "base/db", Path: "name", Op: "eq", Value: "db"}
	base := baseDesired()
	base.Checks = []lock.Check{baseCheck}
	planner := StaticPlanner{hashOf(baseText): base}
	e := newTestEnv(t, planner)
	e.writeIntent("base", baseText)
	p := e.plan()
	e.mustApply(p)
	if want := []CheckResult{{Intent: "base", Result: checks.Result{Name: "Base.", Status: checks.Pass}}}; !reflect.DeepEqual(p.CheckResults, want) {
		t.Errorf("first apply: results = %+v, want %+v", p.CheckResults, want)
	}

	// Only app has changes: base's checks do not run.
	t.Setenv("PROSA_AWS_ENDPOINT", "http://localhost:1")
	planner[hashOf(appText)] = checkedApp(netID, dbSize, bad, later, health)
	e.writeIntent("app", appText)
	p = e.plan()
	e.fake.calls = nil
	e.mustApply(p)
	want := []CheckResult{
		{Intent: "app", Result: checks.Result{Name: netID.Name, Status: checks.Pass}},
		{Intent: "app", Result: checks.Result{Name: dbSize.Name, Status: checks.Pass}},
		{Intent: "app", Result: checks.Result{Name: bad.Name, Status: checks.Fail, Detail: "size does not equal 20"}},
		{Intent: "app", Result: checks.Result{Name: later.Name, Status: checks.Skipped, Detail: "needs the run history"}},
		{Intent: "app", Result: checks.Result{Name: health.Name, Status: checks.Skipped, Detail: "emulated endpoint"}},
	}
	if !reflect.DeepEqual(p.CheckResults, want) {
		t.Errorf("results = %+v\nwant %+v", p.CheckResults, want)
	}
	// The id is computed: it is known only in the state read after the apply.
	if got, want := e.fake.calls, []string{"plan test_net", "apply test_net", "read test_net", "read test_db"}; !equalStrings(got, want) {
		t.Errorf("provider calls = %v, want %v", got, want)
	}
	if !HasFailedChecks(p.CheckResults) {
		t.Error("HasFailedChecks = false with a failed check")
	}

	// Nothing applied: no checks.
	p = e.plan()
	e.mustApply(p)
	if len(p.CheckResults) != 0 {
		t.Errorf("steady state: results = %+v, want none", p.CheckResults)
	}

	// A rewritten intent without infrastructure changes runs its checks.
	text2 := appText + "\nmore prose\n"
	planner[hashOf(text2)] = checkedApp(netID)
	e.writeIntent("app", text2)
	p = e.plan()
	if p.HasChanges() || !p.LockDirty {
		t.Fatalf("rewrite: HasChanges=%v LockDirty=%v, want only the lock", p.HasChanges(), p.LockDirty)
	}
	e.mustApply(p)
	if want := []CheckResult{{Intent: "app", Result: checks.Result{Name: netID.Name, Status: checks.Pass}}}; !reflect.DeepEqual(p.CheckResults, want) {
		t.Errorf("rewrite: results = %+v, want %+v", p.CheckResults, want)
	}
}

func TestPlanValidatesChecksWithoutResources(t *testing.T) {
	text := "only prose\n\n## Checks\n- The certificate is valid.\n"
	cert := lock.Check{Name: "The certificate is valid.", Type: "tls_days", Host: "x.example"}
	planner := StaticPlanner{hashOf(text): {Checks: []lock.Check{cert}}}
	e := newTestEnv(t, planner)
	e.writeIntent("app", text)
	_, err := e.engine.Plan(context.Background())
	var ve *ValidationError
	if !errors.As(err, &ve) || len(ve.Problems) != 1 || ve.Problems[0].Key != "checks[0]" || ve.Problems[0].Message != "tls_days needs min_days, at least 1" {
		t.Fatalf("Plan = %v, want the check's problem", err)
	}

	// Valid: frozen in the lock, and run after the apply.
	planner[hashOf(text)] = Desired{Checks: []lock.Check{later}}
	p := e.plan()
	e.mustApply(p)
	if got := e.loadLock().Intents["app"].Checks; !reflect.DeepEqual(got, []lock.Check{later}) {
		t.Errorf("lock checks = %#v", got)
	}
	if want := []CheckResult{{Intent: "app", Result: checks.Result{Name: later.Name, Status: checks.Skipped, Detail: later.Reason}}}; !reflect.DeepEqual(p.CheckResults, want) {
		t.Errorf("results = %+v, want %+v", p.CheckResults, want)
	}
}

func TestEngineCheckReadsStateAndWritesNothing(t *testing.T) {
	t.Setenv("PROSA_AWS_ENDPOINT", "") // a real endpoint: the network check runs
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	baseText := "base"
	health := lock.Check{Name: "Risponde.", Type: "http_get", URL: srv.URL, Within: "1s"}
	planner := StaticPlanner{hashOf(baseText): baseDesired(), hashOf(appText): checkedApp(netID, dbSize, health)}
	e := newTestEnv(t, planner)
	e.engine.CheckOptions = checks.Options{HTTPClient: srv.Client()}
	e.writeIntent("base", baseText)
	e.writeIntent("app", appText)
	e.mustApply(e.plan())

	// Out of band, the database grew.
	for id, v := range e.fake.remote {
		if strings.HasPrefix(id, "test_db-") {
			m := v.AsValueMap()
			m["size"] = cty.NumberIntVal(20)
			e.fake.remote[id] = cty.ObjectVal(m)
		}
	}
	lockBefore, stateBefore := e.readFile(e.lockPath), e.readFile(e.statePath("app"))
	e.engine.Planner = nil // check never plans
	res, changed, err := e.engine.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 0 {
		t.Errorf("changed = %v, want none", changed)
	}
	want := []CheckResult{
		{Intent: "app", Result: checks.Result{Name: netID.Name, Status: checks.Pass}},
		{Intent: "app", Result: checks.Result{Name: dbSize.Name, Status: checks.Fail, Detail: "size does not equal 10"}},
		{Intent: "app", Result: checks.Result{Name: health.Name, Status: checks.Pass}},
	}
	if !reflect.DeepEqual(res, want) {
		t.Errorf("results = %+v\nwant %+v", res, want)
	}
	if !bytes.Equal(e.readFile(e.lockPath), lockBefore) || !bytes.Equal(e.readFile(e.statePath("app")), stateBefore) {
		t.Error("check wrote the lock or the state store")
	}

	// An intent rewritten since the apply is reported; its applied checks run.
	e.writeIntent("app", appText+"\nmore\n")
	if res2, changed, err := e.engine.Check(context.Background()); err != nil || !equalStrings(changed, []string{"app"}) || !reflect.DeepEqual(res2, want) {
		t.Errorf("after a rewrite: changed %v, err %v, results %+v; want app and the same results", changed, err, res2)
	}

	// A state store that does not match the lock is an error; one ahead of
	// the lock points to prosa recover (ADR-024).
	st, _ := e.loadState("base")
	if _, err := e.store.Save("base", st); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.engine.Check(context.Background()); err == nil || !strings.Contains(err.Error(), `state of intent "base" is ahead of the lock`) ||
		!strings.Contains(err.Error(), `run "prosa recover <dir>"`) {
		t.Errorf("check with a state ahead of the lock: %v, want the recover hint", err)
	}
	lk := e.loadLock()
	lk.Intents["base"].State.Serial = 99
	if _, err := lock.Save(e.lockPath, lk); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.engine.Check(context.Background()); err == nil || !strings.Contains(err.Error(), `state digest mismatch for intent "base"`) {
		t.Errorf("check with a stale lock: %v, want a digest mismatch", err)
	}
}

func TestCheckedKeyMustStay(t *testing.T) {
	baseText := "base"
	rp := &recordingPlanner{desired: map[string]Desired{"base": baseDesired(), "app": checkedApp(dbSize)}}
	e := newTestEnv(t, rp)
	e.writeIntent("base", baseText)
	e.writeIntent("app", appText)
	e.mustApply(e.plan())

	var err error
	rp.check = func(ctx context.Context, in PlanInput) {
		if in.Intent.Name != "base" {
			return
		}
		if want := []string{"db"}; !equalStrings(in.Referenced, want) {
			t.Errorf("base Referenced = %v, want %v", in.Referenced, want)
		}
		err = in.Validate(ctx, Desired{Resources: baseDesired().Resources[1:]})
	}
	e.writeIntent("base", "base v2")
	e.plan()
	var ve *ValidationError
	if !errors.As(err, &ve) || len(ve.Problems) != 1 || ve.Problems[0].Key != "db" || !strings.Contains(ve.Problems[0].Message, "app/checks[0]") {
		t.Errorf("Validate without db = %v, want one problem on db naming app/checks[0]", err)
	}
}

func TestPlanReportsStaleChecks(t *testing.T) {
	baseText, base2 := "base", "base v2"
	planner := StaticPlanner{
		hashOf(baseText): baseDesired(),
		hashOf(base2):    {Resources: baseDesired().Resources[1:]},
		hashOf(appText):  checkedApp(dbSize),
	}
	e := newTestEnv(t, planner)
	e.writeIntent("base", baseText)
	e.writeIntent("app", appText)
	e.mustApply(e.plan())

	e.writeIntent("base", base2)
	_, err := e.engine.Plan(context.Background())
	var ve *ValidationError
	if !errors.As(err, &ve) || len(ve.Problems) != 1 || ve.Problems[0].Intent != "app" || ve.Problems[0].Key != "checks[0]" ||
		ve.Problems[0].Message != `resource "base/db": unknown key db` {
		t.Errorf("Plan = %v, want the stale check of app", err)
	}
}
