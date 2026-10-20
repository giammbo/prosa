package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zclconf/go-cty/cty"

	"github.com/giammbo/prosa/internal/engine"
	"github.com/giammbo/prosa/internal/provider"
)

func TestReconcileFlagsDefaults(t *testing.T) {
	root := t.TempDir()
	dir := projectAt(t, filepath.Join(root, "proj"))

	var stderr bytes.Buffer
	f, err := parseReconcileFlags("plan", []string{dir}, &stderr)
	if err != nil {
		t.Fatalf("parse: %v (stderr %q)", err, stderr.String())
	}
	if f.dir != dir {
		t.Errorf("dir = %q, want %q", f.dir, dir)
	}
	for got, want := range map[string]string{
		f.lock:     filepath.Join(dir, "prosa.lock.yaml"),
		f.state:    filepath.Join(dir, ".prosa", "state"),
		f.events:   filepath.Join(dir, ".prosa", "events.jsonl"),
		f.cacheDir: filepath.Join(dir, ".prosa", "planner"),
	} {
		if got != want {
			t.Errorf("default path = %q, want %q", got, want)
		}
	}
	if f.fixtures != "" || f.yes || f.model != "claude-opus-5-5" || !eqStrings(f.ignore, []string{"README.md"}) {
		t.Errorf("fixtures = %q, yes = %v, model %q, ignore %v; want empty, false, the default model, README.md", f.fixtures, f.yes, f.model, f.ignore)
	}

	// A relative dir, and no dir at all: the current directory. The
	// project's paths are absolute.
	t.Chdir(root)
	for _, args := range [][]string{{"proj"}, nil} {
		if args == nil {
			t.Chdir(dir)
		}
		f, err = parseReconcileFlags("plan", args, &stderr)
		if err != nil {
			t.Fatal(err)
		}
		if f.dir != dir || f.lock != filepath.Join(dir, "prosa.lock.yaml") || f.state != filepath.Join(dir, ".prosa", "state") {
			t.Errorf("args %q: dir %q, lock %q, state %q; want the project in %s", args, f.dir, f.lock, f.state, dir)
		}
	}

	// Explicit flags win; --yes exists only for apply.
	f, err = parseReconcileFlags("apply", []string{"--yes", "--lock", "l.yaml", "--state", "s", "--fixtures", "fx.json", dir}, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if f.lock != "l.yaml" || f.state != "s" || f.fixtures != "fx.json" || !f.yes {
		t.Errorf("explicit flags: %+v", f)
	}
	if _, err := parseReconcileFlags("plan", []string{"--yes", dir}, &stderr); err == nil {
		t.Error("plan --yes: want a usage error")
	}
}

// --ignore names the files of the root that are not intents (README.md by
// default); the region comes from PROSA_AWS_REGION, else the AWS profile.
func TestReconcileFlagsIgnoreAndRegion(t *testing.T) {
	dir := projectAt(t, t.TempDir())
	var stderr bytes.Buffer
	f, err := parseReconcileFlags("plan", []string{dir}, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if !eqStrings(f.ignore, []string{"README.md"}) || f.aws.Region != "eu-south-1" || f.aws.Endpoint != "" {
		t.Errorf("defaults: ignore %v, aws %+v", f.ignore, f.aws)
	}
	if f, err = parseReconcileFlags("check", []string{"--ignore", "NOTES.md, TODO.md", dir}, &stderr); err != nil ||
		!eqStrings(f.ignore, []string{"NOTES.md", "TODO.md"}) {
		t.Errorf("--ignore: %v, %v", f.ignore, err)
	}
	t.Setenv("AWS_REGION", "us-east-2")
	if f, _ = parseReconcileFlags("check", []string{dir}, &stderr); f.aws.Region != "us-east-2" {
		t.Errorf("AWS_REGION: region = %q, want us-east-2", f.aws.Region)
	}
	t.Setenv("PROSA_AWS_REGION", "us-east-1")
	t.Setenv("PROSA_AWS_ENDPOINT", "http://localhost:5555")
	if f, _ = parseReconcileFlags("check", []string{dir}, &stderr); f.aws.Region != "us-east-1" || f.aws.Endpoint != "http://localhost:5555" {
		t.Errorf("PROSA_AWS_*: aws %+v", f.aws)
	}
}

// A prosa.yaml left from ADR-027 is never ignored in silence: every command
// exits 2 with the reason, before writing anything.
func TestCommandsRefuseProsaYAML(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "app.md"), "one thing")
	writeFile(t, filepath.Join(dir, "prosa.yaml"), "version: 1\n")
	t.Setenv("PROSA_STATE_PASSPHRASE", "passphrase")
	for _, args := range [][]string{{"plan", dir}, {"apply", "--yes", dir}, {"check", dir}, {"recover", dir}} {
		var stdout, stderr bytes.Buffer
		if code := run(context.Background(), args, strings.NewReader(""), &stdout, &stderr); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
		const want = "prosa: prosa.yaml is no longer used (ADR-029): credentials and region come from the AWS CLI; delete it\n"
		if stderr.String() != want || stdout.Len() != 0 {
			t.Errorf("%v: stderr = %q, stdout %q, want %q", args, stderr.String(), stdout.String(), want)
		}
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 2 {
		t.Errorf("the refused commands wrote files: %v", entries)
	}
}

// Without a region from the environment or the AWS profile, every command
// exits 2 with the reason, before writing anything.
func TestCommandsRequireRegion(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "app.md"), "one thing")
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "absent"))
	t.Setenv("PROSA_STATE_PASSPHRASE", "passphrase")
	for _, args := range [][]string{{"plan", dir}, {"apply", "--yes", dir}, {"check", dir}, {"recover", dir}} {
		var stdout, stderr bytes.Buffer
		if code := run(context.Background(), args, strings.NewReader(""), &stdout, &stderr); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
		const want = "prosa: no AWS region: set AWS_REGION or a region in your AWS profile\n"
		if stderr.String() != want || stdout.Len() != 0 {
			t.Errorf("%v: stderr = %q, stdout %q, want %q", args, stderr.String(), stdout.String(), want)
		}
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("the refused commands wrote files: %v", entries)
	}
}

// A README.md next to the intents is not an intent.
func TestPlanIgnoresReadme(t *testing.T) {
	dir, fx := gatedProject(t, "one thing")
	writeFile(t, filepath.Join(dir, "README.md"), "# My project")
	fake := &fakeProvider{}
	var stdout, stderr bytes.Buffer
	if code := reconcile(context.Background(), "plan", []string{"--fixtures", fx, dir}, strings.NewReader(""), &stdout, &stderr, fake.source, noPlanner); code != 0 {
		t.Fatalf("exit %d (stderr %q)", code, stderr.String())
	}
	if !strings.HasSuffix(stdout.String(), "Plan: 1 to add, 0 to change, 0 to destroy.\n") {
		t.Errorf("stdout = %q", stdout.String())
	}
}

func TestReconcileUsageErrorsExitTwo(t *testing.T) {
	for _, args := range [][]string{
		{"plan", "a", "b"},
		{"apply", "--nope", "a"},
		{"plan", "--yes", "a"},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(context.Background(), args, strings.NewReader(""), &stdout, &stderr); code != 2 {
			t.Errorf("run %v: exit code = %d, want 2 (stderr %q)", args, code, stderr.String())
		}
		if !strings.Contains(stderr.String(), "usage:") {
			t.Errorf("run %v: stderr lacks usage: %q", args, stderr.String())
		}
	}
}

func TestApplyPromptRejectsNonYes(t *testing.T) {
	root := t.TempDir()
	dir := projectAt(t, root)
	text := "one thing"
	writeFile(t, filepath.Join(dir, "app.md"), text)
	fx := filepath.Join(root, "fx.json")
	writeFile(t, fx, `[{"intent": "app", "hash": "`+sha256Of(text)+`", "resources": [
		{"key": "thing", "type": "test_thing", "config": {"name": "x"}}]}]`)
	t.Setenv("PROSA_STATE_PASSPHRASE", "passphrase")

	fake := &fakeProvider{}
	var stdout, stderr bytes.Buffer
	code := reconcile(context.Background(), "apply", []string{"--fixtures", fx, dir}, strings.NewReader("y\n"), &stdout, &stderr, fake.source, noPlanner)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 (stderr %q)", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "  # app/thing will be created\n  # (needs approval)\n  + resource \"test_thing\" \"thing\" {\n") {
		t.Errorf("stdout lacks the plan: %q", out)
	}
	if !strings.HasSuffix(out, "Needs approval:\n  + app/thing (test_thing)\nDo you want to perform these actions? Only 'yes' will be accepted: \nApply cancelled.\n") {
		t.Errorf("stdout lacks the prompt with the changes to approve: %q", out)
	}
	if !strings.Contains(stderr.String(), engine.ErrNotApproved.Error()) || !strings.Contains(stderr.String(), "nothing was changed") {
		t.Errorf("stderr = %q, want a clear not-approved message", stderr.String())
	}
	if fake.applies != 0 {
		t.Errorf("provider applied %d times after a refusal", fake.applies)
	}
	if _, err := os.Stat(filepath.Join(root, "prosa.lock.yaml")); !os.IsNotExist(err) {
		t.Errorf("lock written after a refusal (stat err %v)", err)
	}
	if !fake.closed {
		t.Error("provider source not closed")
	}

	// "yes" applies.
	stdout.Reset()
	stderr.Reset()
	code = reconcile(context.Background(), "apply", []string{"--fixtures", fx, dir}, strings.NewReader("yes\n"), &stdout, &stderr, fake.source, noPlanner)
	if code != 0 || fake.applies != 1 {
		t.Fatalf("yes: exit code = %d, applies = %d (stderr %q)", code, fake.applies, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(root, "prosa.lock.yaml")); err != nil {
		t.Errorf("lock not written after yes: %v", err)
	}
}

func TestPlanPrintsChangesWithoutWriting(t *testing.T) {
	root := t.TempDir()
	dir := projectAt(t, root)
	text := "one thing"
	writeFile(t, filepath.Join(dir, "app.md"), text)
	fx := filepath.Join(root, "fx.json")
	writeFile(t, fx, `[{"intent": "app", "hash": "`+sha256Of(text)+`", "resources": [
		{"key": "thing", "type": "test_thing", "config": {"name": "x"}}]}]`)
	t.Setenv("PROSA_STATE_PASSPHRASE", "passphrase")

	fake := &fakeProvider{}
	var stdout, stderr bytes.Buffer
	if code := reconcile(context.Background(), "plan", []string{"--fixtures", fx, dir}, strings.NewReader(""), &stdout, &stderr, fake.source, noPlanner); code != 0 {
		t.Fatalf("exit code = %d (stderr %q)", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "  # app/thing will be created\n") || !strings.HasSuffix(stdout.String(), "Plan: 1 to add, 0 to change, 0 to destroy.\n") {
		t.Errorf("stdout = %q", stdout.String())
	}
	if fake.applies != 0 {
		t.Error("plan applied")
	}
	if _, err := os.Stat(filepath.Join(root, "prosa.lock.yaml")); !os.IsNotExist(err) {
		t.Errorf("plan wrote the lock (stat err %v)", err)
	}

	t.Setenv("PROSA_STATE_PASSPHRASE", "")
	stderr.Reset()
	if code := reconcile(context.Background(), "plan", []string{"--fixtures", fx, dir}, strings.NewReader(""), &stdout, &stderr, fake.source, noPlanner); code != 1 {
		t.Errorf("empty passphrase: exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "PROSA_STATE_PASSPHRASE") {
		t.Errorf("empty passphrase: stderr = %q", stderr.String())
	}
}

func TestApplyRefusesBlockedPlan(t *testing.T) {
	root := t.TempDir()
	dir := projectAt(t, root)
	text := "three replicas, verified: exactly two"
	writeFile(t, filepath.Join(dir, "app.md"), text)
	fx := filepath.Join(root, "fx.json")
	writeFile(t, fx, `[{"intent": "app", "hash": "`+sha256Of(text)+`", "blockers": ["three replicas contradict the check"]}]`)
	t.Setenv("PROSA_STATE_PASSPHRASE", "passphrase")

	fake := &fakeProvider{}
	var stdout, stderr bytes.Buffer
	code := reconcile(context.Background(), "apply", []string{"--yes", "--fixtures", fx, dir}, strings.NewReader(""), &stdout, &stderr, fake.source, noPlanner)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 (stderr %q)", code, stderr.String())
	}
	if want := "Blocked:\n  app: three replicas contradict the check\n"; stdout.String() != want {
		t.Errorf("stdout = %q, want %q", stdout.String(), want)
	}
	if !strings.Contains(stderr.String(), "plan blocked: app: three replicas contradict the check") {
		t.Errorf("stderr = %q", stderr.String())
	}
	if fake.applies != 0 {
		t.Errorf("provider applied %d times for a blocked plan", fake.applies)
	}
	if _, err := os.Stat(filepath.Join(root, "prosa.lock.yaml")); !os.IsNotExist(err) {
		t.Errorf("lock written for a blocked plan (stat err %v)", err)
	}
}

// projectAt makes root a project: a folder (ADR-029 needs nothing else).
// It returns root.
func projectAt(t *testing.T, root string) string {
	t.Helper()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fakeProvider is a provider with one type, test_thing (name, id computed),
// that creates objects in memory.
type fakeProvider struct {
	applies int
	closed  bool
	aws     engine.AWSSettings // of the last source call
	// failApply, when set, is returned by ApplyResource (nothing created).
	failApply error
	remote    map[string]cty.Value
}

func (f *fakeProvider) source(aws engine.AWSSettings) (providerFunc, func(), error) {
	f.closed, f.aws = false, aws
	return func(context.Context, string, string) (engine.Provider, error) { return f, nil }, func() { f.closed = true }, nil
}

var thingBlock = &provider.Block{
	Attributes: map[string]*provider.Attribute{
		"name": {Type: cty.String, Required: true},
		"id":   {Type: cty.String, Computed: true},
	},
	BlockTypes: map[string]*provider.NestedBlock{},
}

func (f *fakeProvider) Schema(context.Context) (*provider.Schema, error) {
	return &provider.Schema{
		Resources:  map[string]*provider.ResourceSchema{"test_thing": {Block: thingBlock}},
		Identities: map[string]*provider.IdentitySchema{},
	}, nil
}

func (f *fakeProvider) ValidateResource(context.Context, string, cty.Value) error { return nil }

func (f *fakeProvider) PlanResource(_ context.Context, _ string, prior, config cty.Value, private []byte, _ cty.Value) (provider.PlannedChange, error) {
	if config.IsNull() {
		return provider.PlannedChange{Planned: config, PlannedPrivate: private}, nil
	}
	id := cty.UnknownVal(cty.String)
	if !prior.IsNull() {
		id = prior.GetAttr("id")
	}
	pc := provider.PlannedChange{Planned: cty.ObjectVal(map[string]cty.Value{"name": config.GetAttr("name"), "id": id}), PlannedPrivate: private}
	if !prior.IsNull() && !prior.GetAttr("name").RawEquals(config.GetAttr("name")) {
		pc.RequiresReplace = []string{"name"} // a rename replaces
	}
	return pc, nil
}

func (f *fakeProvider) ApplyResource(_ context.Context, _ string, _ cty.Value, change provider.PlannedChange, _ cty.Value) (provider.ApplyResult, error) {
	if f.failApply != nil {
		return provider.ApplyResult{}, f.failApply
	}
	f.applies++
	if change.Planned.IsNull() { // destroy
		delete(f.remote, "thing-1")
		return provider.ApplyResult{State: cty.NullVal(thingBlock.ImpliedType()), Identity: cty.NullVal(cty.DynamicPseudoType)}, nil
	}
	vals := change.Planned.AsValueMap()
	vals["id"] = cty.StringVal("thing-1")
	st := cty.ObjectVal(vals)
	if f.remote == nil {
		f.remote = map[string]cty.Value{}
	}
	f.remote["thing-1"] = st
	return provider.ApplyResult{State: st, Identity: cty.NullVal(cty.DynamicPseudoType)}, nil
}

func (f *fakeProvider) ReadResource(_ context.Context, _ string, current cty.Value, private []byte, _ cty.Value) (provider.ReadResult, error) {
	st, ok := f.remote[current.GetAttr("id").AsString()]
	if !ok {
		st = cty.NullVal(thingBlock.ImpliedType())
	}
	return provider.ReadResult{State: st, Private: private, Identity: cty.NullVal(cty.DynamicPseudoType)}, nil
}

func sha256Of(text string) string {
	sum := sha256.Sum256([]byte(text))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// noPlanner is a plannerSource for tests that use fixtures: it must not be
// asked for a planner.
func noPlanner(plannerSettings, io.Writer) engine.Planner {
	panic("planner source used with --fixtures")
}

// fakePlanner returns one thing and counts its calls.
type fakePlanner struct {
	calls    int
	model    string
	settings plannerSettings
}

func (f *fakePlanner) source(s plannerSettings, log io.Writer) engine.Planner {
	f.model, f.settings = s.model, s
	return f
}

func (f *fakePlanner) Plan(context.Context, engine.PlanInput) (engine.Desired, error) {
	f.calls++
	return engine.Desired{Resources: []engine.DesiredResource{{Key: "thing", Type: "test_thing", Config: map[string]any{"name": "x"}}}}, nil
}

func TestCLIUsesLLMWithoutFixtures(t *testing.T) {
	root := t.TempDir()
	dir := projectAt(t, root)
	writeFile(t, filepath.Join(dir, "app.md"), "one thing")
	t.Setenv("PROSA_STATE_PASSPHRASE", "passphrase")
	fake := &fakeProvider{}
	planSrc := &fakePlanner{}
	run := func(args ...string) (int, string, string) {
		var stdout, stderr bytes.Buffer
		code := reconcile(context.Background(), "plan", append(args, dir), strings.NewReader(""), &stdout, &stderr, fake.source, planSrc.source)
		return code, stdout.String(), stderr.String()
	}

	code, out, errOut := run()
	if code != 0 || !strings.Contains(out, "  # app/thing will be created\n") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	if planSrc.model != "claude-opus-5-5" || planSrc.calls != 1 {
		t.Errorf("model %q, calls %d; want the default model and 1 call", planSrc.model, planSrc.calls)
	}
	// Same input again: from the cache.
	if code, _, errOut = run(); code != 0 || planSrc.calls != 1 {
		t.Errorf("second run: exit %d, calls %d (stderr %q); want cache hit", code, planSrc.calls, errOut)
	}
	if entries, _ := os.ReadDir(filepath.Join(root, ".prosa", "planner")); len(entries) != 1 {
		t.Errorf("cache files = %d, want 1", len(entries))
	}
	// --no-cache bypasses reading.
	if code, _, _ = run("--no-cache"); code != 0 || planSrc.calls != 2 {
		t.Errorf("--no-cache: exit %d, calls %d; want 2 calls", code, planSrc.calls)
	}
	// --model reaches the planner and keys a separate cache entry.
	if code, _, _ = run("--model", "claude-x"); code != 0 || planSrc.model != "claude-x" || planSrc.calls != 3 {
		t.Errorf("--model: exit %d, model %q, calls %d", code, planSrc.model, planSrc.calls)
	}
	// --fixtures with --model is a usage error.
	if code, _, errOut = run("--fixtures", "fx.json", "--model", "claude-x"); code != 2 || !strings.Contains(errOut, "mutually exclusive") {
		t.Errorf("fixtures+model: exit %d, stderr %q; want 2", code, errOut)
	}
}

func TestNoCacheWritesNothing(t *testing.T) {
	root := t.TempDir()
	dir := projectAt(t, root)
	writeFile(t, filepath.Join(dir, "app.md"), "one thing")
	t.Setenv("PROSA_STATE_PASSPHRASE", "passphrase")
	fp := &fakePlanner{}
	var stdout, stderr bytes.Buffer
	code := reconcile(context.Background(), "plan", []string{"--no-cache", dir}, strings.NewReader(""), &stdout, &stderr, (&fakeProvider{}).source, fp.source)
	if code != 0 {
		t.Fatalf("exit %d (stderr %q)", code, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(root, ".prosa", "planner")); !os.IsNotExist(err) {
		t.Errorf("--no-cache wrote a cache (stat err %v)", err)
	}
}

func TestLazyPlannerOpensOnFirstPlan(t *testing.T) {
	opened := 0
	lp := &lazyPlanner{open: func() (engine.Planner, error) { opened++; return &fakePlanner{}, nil }}
	if opened != 0 {
		t.Fatal("opened before Plan")
	}
	for i := 0; i < 2; i++ {
		if _, err := lp.Plan(context.Background(), engine.PlanInput{}); err != nil {
			t.Fatal(err)
		}
	}
	if opened != 1 {
		t.Errorf("opened %d times, want 1", opened)
	}
}

// checkedProject writes an intent app with a fixture of one thing named x
// and the given checks (JSON), and returns the intents directory.
func checkedProject(t *testing.T, checks string) string {
	t.Helper()
	root := t.TempDir()
	dir := projectAt(t, root)
	text := "one thing\n\n## Checks\n- It is called x.\n"
	writeFile(t, filepath.Join(dir, "app.md"), text)
	writeFile(t, filepath.Join(root, "fx.json"), `[{"intent": "app", "hash": "`+sha256Of(text)+`", "resources": [
		{"key": "thing", "type": "test_thing", "config": {"name": "x"}}], "checks": `+checks+`}]`)
	t.Setenv("PROSA_STATE_PASSPHRASE", "passphrase")
	t.Setenv("PROSA_AWS_ENDPOINT", "")
	return dir
}

func TestApplyRunsChecks(t *testing.T) {
	dir := checkedProject(t, `[
		{"name": "It is called x.", "type": "resource_attr", "resource": "app/thing", "path": "name", "op": "eq", "value": "x"},
		{"name": "It is called y.", "type": "resource_attr", "resource": "app/thing", "path": "name", "op": "eq", "value": "y"},
		{"name": "It runs at night.", "type": "unverifiable", "reason": "needs the run history"}]`)
	fake := &fakeProvider{}
	var stdout, stderr bytes.Buffer
	code := reconcile(context.Background(), "apply", []string{"--yes", "--fixtures", filepath.Join(dir, "fx.json"), dir}, strings.NewReader(""), &stdout, &stderr, fake.source, noPlanner)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 (stderr %q)", code, stderr.String())
	}
	want := "app/thing: Creating...\n" +
		"app/thing: Creation complete after 0s [id=thing-1]\n" +
		"Apply complete! Resources: 1 added, 0 changed, 0 destroyed.\n" +
		"check app: pass — It is called x.\n" +
		"check app: fail — It is called y.: name does not equal \"y\"\n" +
		"check app: skipped — It runs at night.: needs the run history\n"
	if !strings.HasSuffix(stdout.String(), want) {
		t.Errorf("stdout = %q, want it to end with %q", stdout.String(), want)
	}
	if afterTarget(t, stderr.String()) != "prosa: verification failed\n" {
		t.Errorf("stderr = %q", stderr.String())
	}
	if fake.applies != 1 {
		t.Errorf("applies = %d, want 1", fake.applies)
	}
}

func TestCheckCommand(t *testing.T) {
	dir := checkedProject(t, `[{"name": "It is called x.", "type": "resource_attr", "resource": "app/thing", "path": "name", "op": "eq", "value": "x"}]`)
	fake := &fakeProvider{}
	var stdout, stderr bytes.Buffer
	if code := reconcile(context.Background(), "apply", []string{"--yes", "--fixtures", filepath.Join(dir, "fx.json"), dir}, strings.NewReader(""), &stdout, &stderr, fake.source, noPlanner); code != 0 {
		t.Fatalf("apply: exit code = %d (stderr %q)", code, stderr.String())
	}
	lockPath := filepath.Join(dir, "prosa.lock.yaml")
	lockBefore, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	check := func() (int, string, string) {
		var stdout, stderr bytes.Buffer
		code := checkCommand(context.Background(), []string{dir}, &stdout, &stderr, fake.source)
		return code, stdout.String(), afterTarget(t, stderr.String())
	}
	if code, out, errOut := check(); code != 0 || out != "check app: pass — It is called x.\n" || errOut != "" {
		t.Errorf("check: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	// Renamed out of band: the check fails.
	fake.remote["thing-1"] = cty.ObjectVal(map[string]cty.Value{"name": cty.StringVal("z"), "id": cty.StringVal("thing-1")})
	code, out, errOut := check()
	if code != 1 || out != "check app: fail — It is called x.: name does not equal \"x\"\n" || errOut != "prosa: verification failed\n" {
		t.Errorf("check after a rename: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	if after, _ := os.ReadFile(lockPath); !bytes.Equal(after, lockBefore) {
		t.Error("check rewrote the lock")
	}
	// The intent changed since the apply: a warning comes first.
	writeFile(t, filepath.Join(dir, "app.md"), "two things")
	if _, out, _ := check(); !strings.HasPrefix(out, "warning: app changed since the last apply; checks are the applied ones\ncheck app: fail") {
		t.Errorf("check after an edit: stdout %q, want the warning first", out)
	}
	if !fake.closed {
		t.Error("provider source not closed")
	}
}

func TestCheckUsage(t *testing.T) {
	for _, args := range [][]string{{"check", "a", "b"}, {"check", "--fixtures", "fx.json", "a"}, {"check", "--yes", "a"}} {
		var stdout, stderr bytes.Buffer
		if code := run(context.Background(), args, strings.NewReader(""), &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "prosa check [--env FILE] [--ignore FILES] [--lock FILE] [--state DIR] [--events FILE | --no-events] [--require-endpoint] [--no-color] [<dir>]") {
			t.Errorf("run %v: exit code = %d, stderr %q; want 2 and the usage", args, code, stderr.String())
		}
	}
}

// gatedProject writes an intent app with the given text and a fixture of one
// thing, and returns the intents directory and the fixtures file.
func gatedProject(t *testing.T, text string) (dir, fx string) {
	t.Helper()
	root := t.TempDir()
	dir = projectAt(t, root)
	writeFile(t, filepath.Join(dir, "app.md"), text)
	fx = filepath.Join(root, "fx.json")
	writeFile(t, fx, `[{"intent": "app", "hash": "`+sha256Of(text)+`", "resources": [
		{"key": "thing", "type": "test_thing", "config": {"name": "x"}}]}]`)
	t.Setenv("PROSA_STATE_PASSPHRASE", "passphrase")
	return dir, fx
}

func TestApplyGatesByAutonomy(t *testing.T) {
	// auto: asks once like the others (ADR-033); "yes" applies.
	dir, fx := gatedProject(t, "---\nautonomy: auto\n---\none thing")
	fake := &fakeProvider{}
	var stdout, stderr bytes.Buffer
	code := reconcile(context.Background(), "apply", []string{"--fixtures", fx, dir}, strings.NewReader("yes\n"), &stdout, &stderr, fake.source, noPlanner)
	if code != 0 || fake.applies != 1 {
		t.Fatalf("auto: exit %d, applies %d (stderr %q)", code, fake.applies, stderr.String())
	}
	thing := func(comment string) string {
		out := "Prosa will perform the following actions:\n\n  # app/thing will be created\n"
		if comment != "" {
			out += "  # (" + comment + ")\n"
		}
		return out + "  + resource \"test_thing\" \"thing\" {\n      + id   = (known after apply)\n      + name = \"x\"\n    }\n\n"
	}
	if want := thing("") + "Plan: 1 to add, 0 to change, 0 to destroy.\n" + approvePrompt + "\n" +
		"app/thing: Creating...\napp/thing: Creation complete after 0s [id=thing-1]\n" +
		"Apply complete! Resources: 1 added, 0 changed, 0 destroyed.\n"; stdout.String() != want {
		t.Errorf("auto: stdout = %q, want %q", stdout.String(), want)
	}

	// observe: shown, never applied, even with --yes.
	dir, fx = gatedProject(t, "---\nautonomy: observe\n---\none thing")
	fake = &fakeProvider{}
	stdout.Reset()
	stderr.Reset()
	code = reconcile(context.Background(), "apply", []string{"--yes", "--fixtures", fx, dir}, strings.NewReader(""), &stdout, &stderr, fake.source, noPlanner)
	if code != 0 || fake.applies != 0 {
		t.Fatalf("observe: exit %d, applies %d (stderr %q)", code, fake.applies, stderr.String())
	}
	if want := thing("not applied: observe") + "Plan: 0 to add, 0 to change, 0 to destroy.\nNot applied:\n  app/thing: observe\n"; stdout.String() != want {
		t.Errorf("observe: stdout = %q, want %q", stdout.String(), want)
	}
	if _, err := os.Stat(filepath.Join(dir, "prosa.lock.yaml")); !os.IsNotExist(err) {
		t.Errorf("observe: lock written (stat err %v)", err)
	}

	// advise: the suggested command, under the plan.
	dir, fx = gatedProject(t, "---\nautonomy: advise\n---\none thing")
	stdout.Reset()
	if code := reconcile(context.Background(), "plan", []string{"--fixtures", fx, dir}, strings.NewReader(""), &stdout, &stderr, fake.source, noPlanner); code != 0 {
		t.Fatalf("advise: exit %d", code)
	}
	if want := "advise: to apply app, set \"autonomy: approve\" in app.md and run: prosa plan " + dir + ", then prosa apply " + dir + "\n"; !strings.HasSuffix(stdout.String(), want) {
		t.Errorf("advise: stdout = %q, want it to end with %q", stdout.String(), want)
	}
	if strings.Contains(stdout.String(), "--yes") {
		t.Errorf("advise: the hint suggests --yes: %q", stdout.String())
	}
}

func TestApplyRefusalPrunesDependentsAndSummarizes(t *testing.T) {
	root := t.TempDir()
	dir := projectAt(t, root)
	texts := map[string]string{
		"base":  "---\nautonomy: observe\n---\na thing",
		"app":   "---\nautonomy: auto\n---\na thing named after base's",
		"other": "---\nautonomy: auto\n---\nanother thing",
	}
	configs := map[string]string{"base": `{"name": "x"}`, "app": `{"name": "${base/thing.name}-app"}`, "other": `{"name": "o"}`}
	var fxs []string
	for name, text := range texts {
		writeFile(t, filepath.Join(dir, name+".md"), text)
		fxs = append(fxs, `{"intent": "`+name+`", "hash": "`+sha256Of(text)+`", "resources": [{"key": "thing", "type": "test_thing", "config": `+configs[name]+`}]}`)
	}
	fx := filepath.Join(root, "fx.json")
	writeFile(t, fx, "["+strings.Join(fxs, ",")+"]")
	t.Setenv("PROSA_STATE_PASSPHRASE", "passphrase")

	fake := &fakeProvider{}
	var stdout, stderr bytes.Buffer
	code := reconcile(context.Background(), "apply", []string{"--yes", "--fixtures", fx, dir}, strings.NewReader(""), &stdout, &stderr, fake.source, noPlanner)
	if code != 0 || fake.applies != 1 {
		t.Fatalf("exit %d, applies %d (stderr %q)", code, fake.applies, stderr.String())
	}
	want := "other/thing: Creating...\nother/thing: Creation complete after 0s [id=thing-1]\n" +
		"Not applied:\n  base/thing: observe\n  app/thing: depends on base/thing\n" +
		"Apply complete! Resources: 1 added, 0 changed, 0 destroyed.\n"
	if !strings.HasSuffix(stdout.String(), want) {
		t.Errorf("stdout = %q, want it to end with %q", stdout.String(), want)
	}
}

// envProject is a project with the intent app and the environment files
// envs/staging.md and old/staging.md, and the fixtures of app.
func envProject(t *testing.T) (dir, fx string) {
	t.Helper()
	dir, fx = gatedProject(t, "one thing")
	if err := os.MkdirAll(filepath.Join(dir, "envs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "old"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "envs", "staging.md"), "Staging: 40 GB.")
	writeFile(t, filepath.Join(dir, "old", "staging.md"), "Staging, the old one.")
	return dir, fx
}

func TestEnvFlagPaths(t *testing.T) {
	dir, _ := envProject(t)
	t.Chdir(dir)
	var stderr bytes.Buffer
	for _, cmd := range []string{"plan", "apply", "check", "recover"} {
		f, err := parseReconcileFlags(cmd, []string{"--env", "envs/staging.md"}, &stderr)
		if err != nil {
			t.Fatalf("%s: %v (stderr %q)", cmd, err, stderr.String())
		}
		for name, c := range map[string][2]string{
			"env":    {f.env.Path, filepath.Join(dir, "envs", "staging.md")},
			"lock":   {f.lock, filepath.Join(dir, "envs", "staging.lock.yaml")},
			"state":  {f.state, filepath.Join(dir, ".prosa", "state.d", "staging")},
			"events": {f.events, filepath.Join(dir, ".prosa", "events", "staging.jsonl")},
			"cache":  {f.cacheDir, filepath.Join(dir, ".prosa", "planner")},
		} {
			if c[0] != c[1] {
				t.Errorf("%s: %s = %q, want %q", cmd, name, c[0], c[1])
			}
		}
		if f.env.Name != "staging" {
			t.Errorf("%s: env name = %q", cmd, f.env.Name)
		}
	}
	// Without --env nothing changes.
	f, err := parseReconcileFlags("plan", nil, &stderr)
	if err != nil || f.env.Name != "" || f.lock != filepath.Join(dir, "prosa.lock.yaml") || f.state != filepath.Join(dir, ".prosa", "state") {
		t.Errorf("no --env: %+v, %v", f, err)
	}
}

func TestNewEngineCarriesEnv(t *testing.T) {
	dir, _ := envProject(t)
	t.Chdir(dir)
	var stderr bytes.Buffer
	f, err := parseReconcileFlags("plan", []string{"--env", "envs/staging.md"}, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	e := f.newEngine(nil)
	if e.Env == nil || e.Env.Name != "staging" || e.EnvFile != "envs/staging.md" {
		t.Errorf("Env = %+v, EnvFile = %q", e.Env, e.EnvFile)
	}
	f, err = parseReconcileFlags("plan", nil, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if e := f.newEngine(nil); e.Env != nil || e.EnvFile != "" {
		t.Errorf("without --env: Env = %+v, EnvFile = %q", e.Env, e.EnvFile)
	}
}

// With --env the engine derives the environment from the base, whose lock is
// the project's (ADR-030), also when --lock moves the environment's.
func TestEnvEngineReadsBaseLock(t *testing.T) {
	dir, _ := envProject(t)
	t.Chdir(dir)
	var stderr bytes.Buffer
	for _, args := range [][]string{{"--env", "envs/staging.md"}, {"--env", "envs/staging.md", "--lock", "l.yaml"}} {
		f, err := parseReconcileFlags("plan", args, &stderr)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := f.newEngine(nil).BaseLockPath, filepath.Join(dir, "prosa.lock.yaml"); got != want {
			t.Errorf("%v: BaseLockPath = %q, want %q", args, got, want)
		}
	}
	f, err := parseReconcileFlags("plan", nil, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if got := f.newEngine(nil).BaseLockPath; got != "" {
		t.Errorf("without --env: BaseLockPath = %q", got)
	}
	// The base texts are the project's, with --env and without.
	for _, args := range [][]string{nil, {"--env", "envs/staging.md"}} {
		f, err := parseReconcileFlags("plan", args, &stderr)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := f.newEngine(nil).BaseTextDir, filepath.Join(dir, ".prosa", "base"); got != want {
			t.Errorf("%v: BaseTextDir = %q, want %q", args, got, want)
		}
	}
}

func TestFlagsOverrideEnvPaths(t *testing.T) {
	dir, _ := envProject(t)
	t.Chdir(dir)
	var stderr bytes.Buffer
	f, err := parseReconcileFlags("apply", []string{"--env", "envs/staging.md", "--lock", "l.yaml", "--state", "s", "--events", "e.jsonl"}, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if f.lock != "l.yaml" || f.state != "s" || f.events != "e.jsonl" || f.env.Name != "staging" {
		t.Errorf("explicit flags lost: lock %q, state %q, events %q, env %q", f.lock, f.state, f.events, f.env.Name)
	}
}

func TestEnvFlagRefusals(t *testing.T) {
	dir, _ := envProject(t)
	writeFile(t, filepath.Join(dir, "staging.md"), "In the root.")
	writeFile(t, filepath.Join(dir, "envs", "Prod.txt"), "text")
	writeFile(t, filepath.Join(dir, "envs", "fm.md"), "---\nautonomy: auto\n---\ntext")
	t.Chdir(dir)
	for env, want := range map[string]string{
		"staging.md":    "prosa: staging.md is in the project root, where every .md is an intent: move it to envs/\n",
		"envs/fm.md":    "environment files have no frontmatter",
		"envs/nope.md":  "no such file",
		"envs/Prod.txt": "invalid environment name",
	} {
		for _, cmd := range []string{"plan", "apply", "check", "recover"} {
			var stdout, stderr bytes.Buffer
			if code := run(context.Background(), []string{cmd, "--env", env}, strings.NewReader(""), &stdout, &stderr); code != 2 {
				t.Errorf("%s --env %s: exit %d, want 2", cmd, env, code)
			}
			if !strings.Contains(stderr.String(), want) || !strings.HasPrefix(stderr.String(), "prosa: ") {
				t.Errorf("%s --env %s: stderr = %q, want %q", cmd, env, stderr.String(), want)
			}
		}
	}
	if _, err := os.Stat(filepath.Join(dir, ".prosa")); !os.IsNotExist(err) {
		t.Errorf("refused commands wrote .prosa (stat err %v)", err)
	}
}

// The same environment name in two folders shares one state store but has
// two locks: the second sees the state ahead of its lock and stops with the
// message of recover.
func TestSameEnvNameTwoFolders(t *testing.T) {
	dir, fx := envProject(t)
	t.Chdir(dir)
	fake := &fakeProvider{}
	var stdout, stderr bytes.Buffer
	if code := reconcile(context.Background(), "apply", []string{"--yes", "--fixtures", fx, "--env", "envs/staging.md"}, strings.NewReader(""), &stdout, &stderr, fake.source, noPlanner); code != 0 {
		t.Fatalf("apply staging: exit %d (stderr %q)", code, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "envs", "staging.lock.yaml")); err != nil {
		t.Fatalf("lock next to the env file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".prosa", "state.d", "staging")); err != nil {
		t.Fatalf("state of the env: %v", err)
	}

	for _, cmd := range []string{"plan", "apply"} {
		stdout.Reset()
		stderr.Reset()
		args := []string{"--yes", "--fixtures", fx, "--env", "old/staging.md"}
		if cmd == "plan" {
			args = args[1:]
		}
		if code := reconcile(context.Background(), cmd, args, strings.NewReader(""), &stdout, &stderr, fake.source, noPlanner); code != 1 {
			t.Fatalf("%s old/staging: exit %d, want 1 (stderr %q)", cmd, code, stderr.String())
		}
		for _, want := range []string{"is ahead of the lock", `prosa recover`} {
			if !strings.Contains(stderr.String(), want) {
				t.Errorf("%s old/staging: stderr = %q, want %q", cmd, stderr.String(), want)
			}
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "old", "staging.lock.yaml")); !os.IsNotExist(err) {
		t.Errorf("the second environment wrote its lock (stat err %v)", err)
	}

	// The advised command resolves it: recover rebuilds the lock of
	// old/staging from the shared state, and the plan then creates nothing.
	stdout.Reset()
	stderr.Reset()
	if code := run(context.Background(), []string{"recover", "--env", "old/staging.md"}, strings.NewReader(""), &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "recovered app: 1 resource(s)") {
		t.Fatalf("recover: exit %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
	assertNoDuplicateCreate(t, dir, fx, fake, "--env", "old/staging.md")
}

// A deleted lock with the state still there: plan stops with the recover
// message, recover rebuilds the lock, and the next plan creates nothing.
func TestDeletedLockRecovers(t *testing.T) {
	dir, fx := gatedProject(t, "one thing")
	fake := &fakeProvider{}
	var stdout, stderr bytes.Buffer
	if code := reconcile(context.Background(), "apply", []string{"--yes", "--fixtures", fx, dir}, strings.NewReader(""), &stdout, &stderr, fake.source, noPlanner); code != 0 {
		t.Fatalf("apply: exit %d (stderr %q)", code, stderr.String())
	}
	if err := os.Remove(filepath.Join(dir, "prosa.lock.yaml")); err != nil {
		t.Fatal(err)
	}
	stderr.Reset()
	if code := reconcile(context.Background(), "plan", []string{"--fixtures", fx, dir}, strings.NewReader(""), &stdout, &stderr, fake.source, noPlanner); code != 1 || !strings.Contains(stderr.String(), "prosa recover") {
		t.Fatalf("plan without the lock: exit %d, stderr %q; want 1 and the recover message", code, stderr.String())
	}
	stdout.Reset()
	if code := run(context.Background(), []string{"recover", dir}, strings.NewReader(""), &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "recovered app: 1 resource(s)") {
		t.Fatalf("recover: exit %d, stdout %q", code, stdout.String())
	}
	assertNoDuplicateCreate(t, dir, fx, fake)
}

// assertNoDuplicateCreate runs apply --yes (with extra args) and checks that
// the provider made no new create (extra: --env FILE, else dir is the argument).
func assertNoDuplicateCreate(t *testing.T, dir, fx string, fake *fakeProvider, extra ...string) {
	t.Helper()
	before := fake.applies
	args := append([]string{"--yes", "--fixtures", fx}, extra...)
	if len(extra) == 0 { // no --env: the project is dir
		args = append(args, dir)
	}
	var stdout, stderr bytes.Buffer
	if code := reconcile(context.Background(), "apply", args, strings.NewReader(""), &stdout, &stderr, fake.source, noPlanner); code != 0 {
		t.Fatalf("apply after recover: exit %d (stderr %q)", code, stderr.String())
	}
	if fake.applies != before || strings.Contains(stdout.String(), "will be created") {
		t.Errorf("duplicate create after recover: applies %d -> %d, stdout %q", before, fake.applies, stdout.String())
	}
}

// Model and effort of each job come from flags, then PROSA_* variables
// (ADR-030); a bad effort or schema budget is a usage error.
func TestPlannerSettingsFlagsAndEnv(t *testing.T) {
	dir := projectAt(t, t.TempDir())
	parse := func(args ...string) (reconcileFlags, string, error) {
		var stderr bytes.Buffer
		f, err := parseReconcileFlags("plan", append(args, dir), &stderr)
		return f, stderr.String(), err
	}
	f, _, err := parse()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := f.plannerSettings(), (plannerSettings{model: "claude-opus-5-5", effort: "high", patchEffort: "low"}); got != want {
		t.Errorf("defaults = %+v, want %+v", got, want)
	}
	t.Setenv("PROSA_MODEL", "env-model")
	t.Setenv("PROSA_PATCH_MODEL", "env-patch")
	t.Setenv("PROSA_EFFORT", "medium")
	t.Setenv("PROSA_PATCH_EFFORT", "high")
	f, _, err = parse()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := f.plannerSettings(), (plannerSettings{model: "env-model", patchModel: "env-patch", effort: "medium", patchEffort: "high"}); got != want {
		t.Errorf("from the environment = %+v, want %+v", got, want)
	}
	f, _, err = parse("--model", "flag-model", "--patch-model", "flag-patch")
	if err != nil {
		t.Fatal(err)
	}
	if f.model != "flag-model" || f.patchModel != "flag-patch" {
		t.Errorf("flags: model %q, patch model %q; want the flags over the environment", f.model, f.patchModel)
	}
	for _, c := range []struct{ key, value, want string }{
		{"PROSA_EFFORT", "extreme", "PROSA_EFFORT"},
		{"PROSA_PATCH_EFFORT", "LOW", "PROSA_PATCH_EFFORT"},
		{"PROSA_SCHEMA_BYTES", "30k", "PROSA_SCHEMA_BYTES"},
	} {
		t.Run(c.key, func(t *testing.T) {
			t.Setenv(c.key, c.value)
			if _, stderr, err := parse(); !errors.Is(err, errUsage) || !strings.Contains(stderr, c.want) {
				t.Errorf("%s=%s: err %v, stderr %q; want a usage error naming it", c.key, c.value, err, stderr)
			}
		})
	}
	if _, stderr, err := parse("--fixtures", "fx.json", "--patch-model", "x"); !errors.Is(err, errUsage) || !strings.Contains(stderr, "mutually exclusive") {
		t.Errorf("--fixtures with --patch-model: err %v, stderr %q", err, stderr)
	}

	// The settings reach the planner source.
	t.Setenv("PROSA_STATE_PASSPHRASE", "passphrase")
	writeFile(t, filepath.Join(dir, "app.md"), "one thing")
	planSrc := &fakePlanner{}
	var stdout, stderr bytes.Buffer
	if code := reconcile(context.Background(), "plan", []string{"--patch-model", "small", dir}, strings.NewReader(""), &stdout, &stderr, (&fakeProvider{}).source, planSrc.source); code != 0 {
		t.Fatalf("exit %d (%s)", code, stderr.String())
	}
	if planSrc.settings.patchModel != "small" || planSrc.settings.model != "env-model" || planSrc.settings.patchEffort != "high" {
		t.Errorf("planner settings = %+v", planSrc.settings)
	}
}

// A section of the environment file that names no intent is a warning on
// stderr naming the heading and the intents, not an error; a heading in
// another case names its intent.
func TestEnvSectionNamesNoIntent(t *testing.T) {
	dir, _ := envProject(t)
	writeFile(t, filepath.Join(dir, "envs", "bad.md"), "# Bad\n\n## Rete\n- a /16.\n")
	writeFile(t, filepath.Join(dir, "envs", "upper.md"), "# Upper\n\n## APP\n- 40 GB.\n")
	t.Chdir(dir)
	for _, cmd := range []string{"plan", "apply", "check", "recover"} {
		var stderr bytes.Buffer
		if _, err := parseReconcileFlags(cmd, []string{"--env", "envs/bad.md"}, &stderr); err != nil {
			t.Errorf("%s: err %v, want none: a warning, not a usage error (stderr %q)", cmd, err, stderr.String())
		}
		if want := "prosa: warning: "; !strings.Contains(stderr.String(), want) || !strings.Contains(stderr.String(), `section "## Rete" names no intent (intents: app`) || !strings.Contains(stderr.String(), "its lines are ignored") {
			t.Errorf("%s: stderr = %q, want the warning with the heading and the intent names", cmd, stderr.String())
		}
		stderr.Reset()
		if _, err := parseReconcileFlags(cmd, []string{"--env", "envs/upper.md"}, &stderr); err != nil {
			t.Errorf("%s --env upper.md: %v (stderr %q)", cmd, err, stderr.String())
		}
		if stderr.Len() != 0 {
			t.Errorf("%s --env upper.md: stderr = %q, want no warning", cmd, stderr.String())
		}
	}
	// A section of an intent the environment's lock still has, which the
	// run removes (an intent deleted, or all of them for a teardown), is
	// accepted.
	writeFile(t, filepath.Join(dir, "envs", "gone.md"), "# Gone\n\n## cron\n- No emails.\n")
	writeFile(t, filepath.Join(dir, "envs", "gone.lock.yaml"), "version: 1\nintents:\n  cron:\n    intent_hash: sha256:old\n")
	var stderr bytes.Buffer
	if _, err := parseReconcileFlags("apply", []string{"--env", "envs/gone.md"}, &stderr); err != nil {
		t.Errorf("section of a removed intent: %v (stderr %q)", err, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Errorf("section of a removed intent: stderr = %q, want no warning", stderr.String())
	}
	// With no intent anywhere (a teardown done), the environment applies to
	// nothing: its sections are not checked.
	empty := projectAt(t, t.TempDir())
	if err := os.MkdirAll(filepath.Join(empty, "envs"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(empty, "envs", "staging.md"), "# Staging\n\n## database\n- 40 GB.\n")
	stderr.Reset()
	if _, err := parseReconcileFlags("apply", []string{"--env", filepath.Join(empty, "envs", "staging.md"), empty}, &stderr); err != nil {
		t.Errorf("no intents: %v (stderr %q)", err, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Errorf("no intents: stderr = %q, want no warning", stderr.String())
	}
}

// PROSA_LLM=openai needs a model: the default is an Anthropic model the
// server does not know.
func TestOpenAINeedsModel(t *testing.T) {
	dir := projectAt(t, t.TempDir())
	t.Setenv("PROSA_LLM", "openai")
	t.Setenv("PROSA_MODEL", "")
	var stderr bytes.Buffer
	if _, err := parseReconcileFlags("plan", []string{dir}, &stderr); !errors.Is(err, errUsage) || !strings.Contains(stderr.String(), "--model") {
		t.Fatalf("err %v, stderr %q; want a usage error asking for --model", err, stderr.String())
	}
	for _, args := range [][]string{{"--model", "qwen3"}, {"--fixtures", "fx.json"}} {
		stderr.Reset()
		if _, err := parseReconcileFlags("plan", append(args, dir), &stderr); err != nil {
			t.Errorf("%v: %v (stderr %q)", args, err, stderr.String())
		}
	}
	t.Setenv("PROSA_MODEL", "qwen3")
	stderr.Reset()
	if _, err := parseReconcileFlags("apply", []string{dir}, &stderr); err != nil {
		t.Errorf("PROSA_MODEL set: %v (stderr %q)", err, stderr.String())
	}
}

func TestApplyAutoPlanAsksAndDeclineChangesNothing(t *testing.T) {
	dir, fx := gatedProject(t, "---\nautonomy: auto\n---\none thing")
	fake := &fakeProvider{}
	var stdout, stderr bytes.Buffer
	code := reconcile(context.Background(), "apply", []string{"--fixtures", fx, dir}, strings.NewReader("no\n"), &stdout, &stderr, fake.source, noPlanner)
	if code != 1 || fake.applies != 0 {
		t.Fatalf("exit %d, applies %d (stderr %q)", code, fake.applies, stderr.String())
	}
	if !strings.HasSuffix(stdout.String(), approvePrompt+"\nApply cancelled.\n") || strings.Contains(stdout.String(), "Needs approval") {
		t.Errorf("stdout = %q", stdout.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "prosa.lock.yaml")); !os.IsNotExist(err) {
		t.Errorf("lock written after declining (stat err %v)", err)
	}
}

func TestApplyYesAsksNothing(t *testing.T) {
	dir, fx := gatedProject(t, "---\nautonomy: auto\n---\none thing")
	fake := &fakeProvider{}
	var stdout, stderr bytes.Buffer
	code := reconcile(context.Background(), "apply", []string{"--yes", "--fixtures", fx, dir}, strings.NewReader(""), &stdout, &stderr, fake.source, noPlanner)
	if code != 0 || fake.applies != 1 {
		t.Fatalf("exit %d, applies %d (stderr %q)", code, fake.applies, stderr.String())
	}
	if strings.Contains(stdout.String(), "perform these actions") {
		t.Errorf("--yes asked: %q", stdout.String())
	}
}

func TestApplyPromptListsDestroy(t *testing.T) {
	dir, fx := gatedProject(t, "---\nautonomy: auto\n---\none thing")
	fake := &fakeProvider{}
	var stdout, stderr bytes.Buffer
	if code := reconcile(context.Background(), "apply", []string{"--yes", "--fixtures", fx, dir}, strings.NewReader(""), &stdout, &stderr, fake.source, noPlanner); code != 0 {
		t.Fatalf("first apply: exit %d (stderr %q)", code, stderr.String())
	}
	text := "---\nautonomy: auto\n---\nnothing"
	writeFile(t, filepath.Join(dir, "app.md"), text)
	writeFile(t, fx, `[{"intent": "app", "hash": "`+sha256Of(text)+`", "resources": []}]`)
	stdout.Reset()
	stderr.Reset()
	code := reconcile(context.Background(), "apply", []string{"--fixtures", fx, dir}, strings.NewReader("no\n"), &stdout, &stderr, fake.source, noPlanner)
	if code != 1 {
		t.Fatalf("exit %d, want 1 (stdout %q, stderr %q)", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "Needs approval:\n  - app/thing (test_thing)") || !strings.HasSuffix(stdout.String(), approvePrompt+"\nApply cancelled.\n") {
		t.Errorf("stdout = %q", stdout.String())
	}
}
