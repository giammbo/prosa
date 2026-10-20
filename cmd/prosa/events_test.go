package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/giammbo/prosa/internal/engine"
	"github.com/giammbo/prosa/internal/events"
	"github.com/giammbo/prosa/internal/planner"
	"github.com/giammbo/prosa/internal/provider"
)

// readEvents returns the events of the log at path, decoded.
func readEvents(t *testing.T, path string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var e map[string]any
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("bad event line %q: %v", line, err)
		}
		out = append(out, e)
	}
	return out
}

func eventTypes(evs []map[string]any) []string {
	var out []string
	for _, e := range evs {
		out = append(out, e["type"].(string))
	}
	return out
}

func eqStrings(a, b []string) bool { return strings.Join(a, ",") == strings.Join(b, ",") }

func eventData(e map[string]any) map[string]any {
	d, _ := e["data"].(map[string]any)
	return d
}

func TestEventsOfPlanApplyAndCheck(t *testing.T) {
	dir := checkedProject(t, `[{"name": "It is called x.", "type": "resource_attr", "resource": "app/thing", "path": "name", "op": "eq", "value": "x"}]`)
	root := dir
	fx := filepath.Join(root, "fx.json")
	evPath := filepath.Join(root, ".prosa", "events.jsonl")
	fake := &fakeProvider{}
	reconcileCmd := func(cmd string, args ...string) int {
		var stdout, stderr bytes.Buffer
		return reconcile(context.Background(), cmd, append(args, "--fixtures", fx, dir), strings.NewReader(""), &stdout, &stderr, fake.source, noPlanner)
	}

	if code := reconcileCmd("plan"); code != 0 {
		t.Fatalf("plan: exit %d", code)
	}
	evs := readEvents(t, evPath)
	if got, want := eventTypes(evs), []string{"run.start", "plan", "run.end"}; !eqStrings(got, want) {
		t.Fatalf("plan events = %v, want %v", got, want)
	}
	start := eventData(evs[0])
	if start["command"] != "plan" || start["version"] != "dev" || start["dir"] != dir || start["user"] == "" {
		t.Errorf("run.start = %v", start)
	}
	changes := eventData(evs[1])["changes"].([]any)
	if len(changes) != 1 {
		t.Fatalf("plan changes = %v", changes)
	}
	ch := changes[0].(map[string]any)
	if ch["intent"] != "app" || ch["key"] != "thing" || ch["type"] != "test_thing" || ch["action"] != "create" || ch["gate"] != "needs_approval" {
		t.Errorf("plan change = %v", ch)
	}
	if eventData(evs[2])["result"] != "ok" {
		t.Errorf("run.end = %v", evs[2])
	}
	runID := evs[0]["run_id"].(string)
	if len(runID) != 16 || evs[1]["run_id"] != runID || evs[2]["run_id"] != runID {
		t.Errorf("run ids = %v %v %v, want one 16-hex id", evs[0]["run_id"], evs[1]["run_id"], evs[2]["run_id"])
	}
	if evs[0]["time"] == "" || evs[0]["time"] == nil {
		t.Error("event without time")
	}

	// Apply: plan, approval, operation, check.
	if code := reconcileCmd("apply", "--yes"); code != 0 {
		t.Fatalf("apply: exit %d", code)
	}
	evs = readEvents(t, evPath)[3:]
	if got, want := eventTypes(evs), []string{"run.start", "plan", "approval", "apply.op", "check", "run.end"}; !eqStrings(got, want) {
		t.Fatalf("apply events = %v, want %v", got, want)
	}
	if evs[0]["run_id"] == runID {
		t.Error("second command reused the run id")
	}
	if a := eventData(evs[2]); a["mode"] != "flag" || a["granted"] != true || a["by"] == "" || len(a["changes"].([]any)) != 1 || a["changes"].([]any)[0] != "app/thing" {
		t.Errorf("approval = %v", a)
	}
	if op := eventData(evs[3]); op["result"] != "ok" || op["node"] != "app/thing" || op["action"] != "create" || evs[3]["intent"] != "app" || evs[3]["key"] != "thing" {
		t.Errorf("apply.op = %v / %v", evs[3], op)
	}
	if c := eventData(evs[4]); c["name"] != "It is called x." || c["type"] != "resource_attr" || c["status"] != "pass" || evs[4]["intent"] != "app" {
		t.Errorf("check = %v", evs[4])
	}

	// A run with nothing to do: only start, plan, end.
	before := len(readEvents(t, evPath))
	if code := reconcileCmd("apply", "--yes"); code != 0 {
		t.Fatalf("no-op apply: exit %d", code)
	}
	evs = readEvents(t, evPath)[before:]
	if got, want := eventTypes(evs), []string{"run.start", "plan", "run.end"}; !eqStrings(got, want) {
		t.Errorf("no-op events = %v, want %v", got, want)
	}

	// prosa check: start, check, end.
	before = len(readEvents(t, evPath))
	var stdout, stderr bytes.Buffer
	if code := checkCommand(context.Background(), []string{dir}, &stdout, &stderr, fake.source); code != 0 {
		t.Fatalf("check: exit %d (%s)", code, stderr.String())
	}
	evs = readEvents(t, evPath)[before:]
	if got, want := eventTypes(evs), []string{"run.start", "check", "run.end"}; !eqStrings(got, want) {
		t.Errorf("check events = %v, want %v", got, want)
	}
	if eventData(evs[0])["command"] != "check" {
		t.Errorf("run.start = %v", evs[0])
	}
}

func TestEventFilePermissionsAndFlags(t *testing.T) {
	dir := checkedProject(t, `[]`)
	root := dir
	fx := filepath.Join(root, "fx.json")
	fake := &fakeProvider{}
	run := func(extra ...string) int {
		var stdout, stderr bytes.Buffer
		return reconcile(context.Background(), "plan", append(extra, "--fixtures", fx, dir), strings.NewReader(""), &stdout, &stderr, fake.source, noPlanner)
	}
	if code := run(); code != 0 {
		t.Fatal("plan failed")
	}
	evDir := filepath.Join(root, ".prosa")
	fi, err := os.Stat(filepath.Join(evDir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("events file mode = %v, want 0600", fi.Mode().Perm())
	}
	// --events overrides the path, --no-events writes nothing.
	custom := filepath.Join(root, "audit", "log.jsonl")
	if code := run("--events", custom); code != 0 {
		t.Fatal("plan --events failed")
	}
	if di, err := os.Stat(filepath.Dir(custom)); err != nil || di.Mode().Perm() != 0o700 {
		t.Errorf("custom events dir: %v, %v; want 0700", di, err)
	}
	if n := len(readEvents(t, custom)); n != 3 {
		t.Errorf("custom log has %d events, want 3", n)
	}
	before := len(readEvents(t, filepath.Join(evDir, "events.jsonl")))
	if code := run("--no-events"); code != 0 {
		t.Fatal("plan --no-events failed")
	}
	if n := len(readEvents(t, filepath.Join(evDir, "events.jsonl"))); n != before {
		t.Errorf("--no-events appended %d events", n-before)
	}
}

func TestEventsRecordRefusalAndNotApproved(t *testing.T) {
	dir := checkedProject(t, `[]`)
	root := dir
	var stdout, stderr bytes.Buffer
	code := reconcile(context.Background(), "apply", []string{"--fixtures", filepath.Join(root, "fx.json"), dir}, strings.NewReader("no\n"), &stdout, &stderr, (&fakeProvider{}).source, noPlanner)
	if code != 1 {
		t.Fatalf("exit %d", code)
	}
	evs := readEvents(t, filepath.Join(root, ".prosa", "events.jsonl"))
	if got, want := eventTypes(evs), []string{"run.start", "plan", "approval", "run.end"}; !eqStrings(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	if eventData(evs[2])["mode"] != "prompt" || eventData(evs[2])["granted"] != false || eventData(evs[3])["result"] != "not_approved" {
		t.Errorf("approval %v, end %v", evs[2], evs[3])
	}
}

func TestEventsNeverHoldValues(t *testing.T) {
	const sentinel = "SENTINEL-8f3a91-VALUE"
	root := t.TempDir()
	dir := projectAt(t, root)
	text := "one thing\n\n## Checks\n- It is called as configured.\n"
	writeFile(t, filepath.Join(dir, "app.md"), text)
	writeFile(t, filepath.Join(root, "fx.json"), `[{"intent": "app", "hash": "`+sha256Of(text)+`", "resources": [
		{"key": "thing", "type": "test_thing", "config": {"name": "`+sentinel+`"}}], "checks": [
		{"name": "It is called as configured.", "type": "resource_attr", "resource": "app/thing", "path": "name", "op": "ne", "value": "other"}]}]`)
	t.Setenv("PROSA_STATE_PASSPHRASE", "passphrase")
	t.Setenv("PROSA_AWS_ENDPOINT", "")
	fake := &fakeProvider{}
	var stdout, stderr bytes.Buffer
	for _, cmd := range []string{"plan", "apply"} {
		args := []string{"--yes", "--fixtures", filepath.Join(root, "fx.json"), dir}
		if cmd == "plan" {
			args = args[1:]
		}
		if code := reconcile(context.Background(), cmd, args, strings.NewReader(""), &stdout, &stderr, fake.source, noPlanner); code != 0 {
			t.Fatalf("%s: exit %d (%s)", cmd, code, stderr.String())
		}
	}
	if code := checkCommand(context.Background(), []string{dir}, &stdout, &stderr, fake.source); code != 0 {
		t.Fatalf("check: exit %d", code)
	}
	if v := fake.remote["thing-1"]; v.IsNull() || v.GetAttr("name").AsString() != sentinel {
		t.Fatalf("test setup: the sentinel is not in the provider state: %#v", v)
	}
	data, err := os.ReadFile(filepath.Join(root, ".prosa", "events.jsonl"))
	if err != nil || len(data) == 0 {
		t.Fatalf("events: %v, %d bytes", err, len(data))
	}
	if strings.Contains(string(data), sentinel) {
		t.Errorf("the event log holds a configured/state value:\n%s", data)
	}
}

// reportingPlanner is a fakePlanner that reports a result like the LLM.
type reportingPlanner struct{ *fakePlanner }

func (r reportingPlanner) LastResult() planner.Result {
	return planner.Result{Intent: "app", Turns: 3, Problems: 1, Model: r.model, ServedBy: "served", Usage: planner.Usage{Input: 7, Output: 5, CacheRead: 2, CacheCreation: 3}}
}

func TestEventsRecordPlannerResultAndCacheHit(t *testing.T) {
	root := t.TempDir()
	dir := projectAt(t, root)
	writeFile(t, filepath.Join(dir, "app.md"), "one thing")
	t.Setenv("PROSA_STATE_PASSPHRASE", "passphrase")
	fp := &fakePlanner{}
	run := func() {
		var stdout, stderr bytes.Buffer
		psrc := func(s plannerSettings, _ io.Writer) engine.Planner {
			fp.model = s.model
			return reportingPlanner{fp}
		}
		if code := reconcile(context.Background(), "plan", []string{dir}, strings.NewReader(""), &stdout, &stderr, (&fakeProvider{}).source, psrc); code != 0 {
			t.Fatalf("exit %d (%s)", code, stderr.String())
		}
	}
	run()
	run()
	evs := readEvents(t, filepath.Join(root, ".prosa", "events.jsonl"))
	if got, want := eventTypes(evs), []string{"run.start", "planner.result", "plan", "run.end", "run.start", "planner.result", "plan", "run.end"}; !eqStrings(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	first, second := eventData(evs[1]), eventData(evs[5])
	if evs[1]["intent"] != "app" || first["turns"] != float64(3) || first["problems"] != float64(1) || first["served_by"] != "served" || first["cached"] != false || first["model"] != "claude-opus-5-5" {
		t.Errorf("first planner.result = %v", evs[1])
	}
	if u := first["usage"].(map[string]any); u["input"] != float64(7) || u["output"] != float64(5) || u["cache_read"] != float64(2) || u["cache_creation"] != float64(3) {
		t.Errorf("usage = %v", u)
	}
	if second["cached"] != true || second["turns"] != float64(0) || fp.calls != 1 {
		t.Errorf("cache hit: planner.result %v, planner calls %d; want cached=true, 1 call", second, fp.calls)
	}
}

// secretProject is a project whose one thing is named after a sentinel.
func secretProject(t *testing.T, sentinel string) (dir, fx string) {
	t.Helper()
	root := t.TempDir()
	dir = projectAt(t, root)
	text := "one thing"
	writeFile(t, filepath.Join(dir, "app.md"), text)
	fx = filepath.Join(root, "fx.json")
	writeFile(t, fx, `[{"intent": "app", "hash": "`+sha256Of(text)+`", "resources": [
		{"key": "thing", "type": "test_thing", "config": {"name": "`+sentinel+`"}}]}]`)
	t.Setenv("PROSA_STATE_PASSPHRASE", "passphrase")
	t.Setenv("PROSA_AWS_ENDPOINT", "")
	return dir, fx
}

func TestEventsOfFailedApplyHoldNoValues(t *testing.T) {
	const sentinel = "SENTINEL-8f3a91-VALUE"
	dir, fx := secretProject(t, sentinel)
	fake := &fakeProvider{failApply: &provider.DiagError{Op: "apply", Sum: "exists " + sentinel, Detail: sentinel, Attr: "name"}}
	var stdout, stderr bytes.Buffer
	code := reconcile(context.Background(), "apply", []string{"--yes", "--fixtures", fx, dir}, strings.NewReader(""), &stdout, &stderr, fake.source, noPlanner)
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), sentinel) {
		t.Fatalf("test setup: the full error should reach stderr: %q", stderr.String())
	}
	path := filepath.Join(dir, ".prosa", "events.jsonl")
	evs := readEvents(t, path)
	if got, want := eventTypes(evs), []string{"run.start", "plan", "approval", "apply.op", "run.end"}; !eqStrings(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	op := eventData(evs[3])
	if op["result"] != "error" || op["error_kind"] != "provider" || op["error"] != "apply: provider error" || op["count"] != float64(1) {
		t.Errorf("apply.op = %v", op)
	}
	if attrs, _ := op["attributes"].([]any); len(attrs) != 1 || attrs[0] != "name" {
		t.Errorf("attributes = %v", op["attributes"])
	}
	if end := eventData(evs[4]); end["result"] != "error" || end["error_kind"] != "provider" {
		t.Errorf("run.end = %v", end)
	}
	if data, _ := os.ReadFile(path); strings.Contains(string(data), sentinel) {
		t.Errorf("the event log holds a value:\n%s", data)
	}
}

// badSubmit submits a resource with an attribute the schema does not have,
// so the validation problem quotes the attribute's name.
type badSubmit struct{ attr string }

func (b badSubmit) New(context.Context, anthropic.BetaMessageNewParams) (*anthropic.BetaMessage, error) {
	var m anthropic.BetaMessage
	raw := `{"id":"m","type":"message","role":"assistant","model":"claude-opus-5-5","stop_reason":"tool_use",
	 "content":[{"type":"tool_use","id":"t","name":"submit_desired_state","input":{"resources":[{"key":"thing","type":"test_thing","config":{"name":"x","` + b.attr + `":1}}],"checks":[]}}],
	 "usage":{"input_tokens":1,"output_tokens":1}}`
	return &m, json.Unmarshal([]byte(raw), &m)
}

func TestEventsOfPlannerFailureHoldNoValues(t *testing.T) {
	const sentinel = "SENTINEL-8f3a91-VALUE"
	root := t.TempDir()
	dir := projectAt(t, root)
	writeFile(t, filepath.Join(dir, "app.md"), "one thing")
	t.Setenv("PROSA_STATE_PASSPHRASE", "passphrase")
	psrc := func(s plannerSettings, _ io.Writer) engine.Planner {
		return &planner.LLM{Messages: badSubmit{sentinel}, Model: s.model, MaxTurns: 2}
	}
	var stdout, stderr bytes.Buffer
	code := reconcile(context.Background(), "plan", []string{"--no-cache", dir}, strings.NewReader(""), &stdout, &stderr, (&fakeProvider{}).source, psrc)
	if code != 1 || !strings.Contains(stderr.String(), sentinel) {
		t.Fatalf("exit %d, stderr %q; want a failure quoting the sentinel on stderr", code, stderr.String())
	}
	path := filepath.Join(root, ".prosa", "events.jsonl")
	evs := readEvents(t, path)
	if got, want := eventTypes(evs), []string{"run.start", "planner.result", "run.end"}; !eqStrings(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	end := eventData(evs[2])
	if end["result"] != "error" || end["error_kind"] != "planner" || end["error"] != "no valid desired state after 2 turns: 1 problem(s)" {
		t.Errorf("run.end = %v", end)
	}
	if data, _ := os.ReadFile(path); strings.Contains(string(data), sentinel) {
		t.Errorf("the event log holds a value:\n%s", data)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }
func (failingWriter) Close() error              { return nil }

func TestEventWriteFailureWarnsButKeepsExitCode(t *testing.T) {
	dir := checkedProject(t, `[]`)
	old := openLog
	openLog = func(string) (*events.Log, error) { return events.NewWriter(failingWriter{}), nil }
	defer func() { openLog = old }()
	var stdout, stderr bytes.Buffer
	code := reconcile(context.Background(), "plan", []string{"--fixtures", filepath.Join(dir, "fx.json"), dir}, strings.NewReader(""), &stdout, &stderr, (&fakeProvider{}).source, noPlanner)
	if code != 0 {
		t.Errorf("exit %d, want 0 (stderr %q)", code, stderr.String())
	}
	if want := "prosa: warning: event log: disk full; some events were not recorded\n"; afterTarget(t, stderr.String()) != want {
		t.Errorf("stderr = %q, want %q", stderr.String(), want)
	}
}

func TestEventsOfReplaceHaveDestroyAndCreatePhases(t *testing.T) {
	dir, fx := secretProject(t, "x")
	fake := &fakeProvider{}
	run := func() int {
		var stdout, stderr bytes.Buffer
		return reconcile(context.Background(), "apply", []string{"--yes", "--fixtures", fx, dir}, strings.NewReader(""), &stdout, &stderr, fake.source, noPlanner)
	}
	if code := run(); code != 0 {
		t.Fatalf("first apply: exit %d", code)
	}
	// The thing is renamed: the provider asks for a replacement.
	text := "another thing"
	writeFile(t, filepath.Join(dir, "app.md"), text)
	writeFile(t, fx, `[{"intent": "app", "hash": "`+sha256Of(text)+`", "resources": [
		{"key": "thing", "type": "test_thing", "config": {"name": "y"}}]}]`)
	path := filepath.Join(dir, ".prosa", "events.jsonl")
	before := len(readEvents(t, path))
	if code := run(); code != 0 {
		t.Fatalf("replace apply: exit %d", code)
	}
	var ops []map[string]any
	for _, e := range readEvents(t, path)[before:] {
		if e["type"] == "apply.op" {
			ops = append(ops, eventData(e))
		}
	}
	if len(ops) != 2 || ops[0]["action"] != "replace" || ops[0]["phase"] != "destroy" || ops[1]["phase"] != "create" || ops[0]["result"] != "ok" || ops[1]["result"] != "ok" {
		t.Errorf("replace ops = %v, want a destroy then a create phase", ops)
	}
}

func TestPlanEventKeepsOnlyBlockerCounts(t *testing.T) {
	const sentinel = "SENTINEL-8f3a91-VALUE"
	var buf bytes.Buffer
	log := events.NewWriter(nopCloser{&buf})
	log.Planned(&engine.Plan{Blockers: []string{"app: needs " + sentinel, "app: also " + sentinel, "db: unclear"}})
	if strings.Contains(buf.String(), sentinel) {
		t.Fatalf("blocker text in the event: %s", buf.String())
	}
	var e map[string]any
	if err := json.Unmarshal(buf.Bytes(), &e); err != nil {
		t.Fatal(err)
	}
	bl := eventData(e)["blockers"].([]any)
	if len(bl) != 2 || bl[0].(map[string]any)["intent"] != "app" || bl[0].(map[string]any)["count"] != float64(2) || bl[1].(map[string]any)["count"] != float64(1) {
		t.Errorf("blockers = %v", bl)
	}
}

type nopCloser struct{ io.Writer }

func (nopCloser) Close() error { return nil }

// forgetRecorder records Forget calls.
type forgetRecorder struct {
	engine.Planner
	forgot []string
}

func (f *forgetRecorder) Forget(name string) { f.forgot = append(f.forgot, name) }

// The event wrapper keeps the planner cache's Forget reachable by the
// engine.
func TestEventPlannerForwardsForget(t *testing.T) {
	inner := &forgetRecorder{}
	pl := withPlannerEvents(inner, events.NewWriter(nopCloser{io.Discard}))
	f, ok := pl.(engine.Forgetter)
	if !ok {
		t.Fatal("the event wrapper hides Forget")
	}
	f.Forget("app")
	if len(inner.forgot) != 1 || inner.forgot[0] != "app" {
		t.Fatalf("forgot = %v, want [app]", inner.forgot)
	}
	eventPlanner{inner: noForget{}}.Forget("app") // a planner without Forget: nothing
}

type noForget struct{ engine.Planner }

// run.start says where the command points: the emulated endpoint, or AWS,
// and the region.
func TestRunStartRecordsTargetAndRegion(t *testing.T) {
	for _, tc := range []struct {
		endpoint, region string
		want             map[string]any
	}{
		{"http://localhost:5555", "eu-west-1", map[string]any{"target": "emulated", "endpoint": "http://localhost:5555", "region": "eu-west-1"}},
		{"", "", map[string]any{"target": "aws", "region": "eu-south-1"}},
	} {
		dir := checkedProject(t, `[]`) // it clears the endpoint: set it after
		t.Setenv("PROSA_AWS_ENDPOINT", tc.endpoint)
		t.Setenv("PROSA_AWS_REGION", tc.region)
		root := dir
		var stdout, stderr bytes.Buffer
		if code := reconcile(context.Background(), "plan", []string{"--fixtures", filepath.Join(root, "fx.json"), dir}, strings.NewReader(""), &stdout, &stderr, (&fakeProvider{}).source, noPlanner); code != 0 {
			t.Fatalf("plan: exit %d: %s", code, stderr.String())
		}
		start := eventData(readEvents(t, filepath.Join(root, ".prosa", "events.jsonl"))[0])
		for k, want := range tc.want {
			if start[k] != want {
				t.Errorf("run.start %s = %v, want %v (%v)", k, start[k], want, start)
			}
		}
		if _, ok := start["endpoint"]; !ok && tc.endpoint != "" {
			t.Errorf("run.start without endpoint: %v", start)
		}
		if _, ok := start["endpoint"]; ok && tc.endpoint == "" {
			t.Errorf("run.start with an endpoint on aws: %v", start)
		}
	}
}
