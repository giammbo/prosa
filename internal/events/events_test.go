package events

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/giammbo/prosa/internal/engine"
	"github.com/giammbo/prosa/internal/planner"
)

func TestOpenAppendsWithPrivatePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "events.jsonl")
	a, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	a.Emit(Event{Type: RunStart, Data: map[string]any{"command": "plan"}})
	a.Close()
	b, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	b.Emit(Event{Type: ApplyOp, Intent: "app", Key: "q", Time: time.Unix(0, 0).UTC(), RunID: "fixed"})
	b.Close()

	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("file mode = %v, want 0600", fi.Mode().Perm())
	}
	if fi, _ := os.Stat(filepath.Dir(path)); fi.Mode().Perm() != 0o700 {
		t.Errorf("dir mode = %v, want 0700", fi.Mode().Perm())
	}
	f, _ := os.Open(path)
	defer f.Close()
	var lines []map[string]any
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, m)
	}
	if len(lines) != 2 {
		t.Fatalf("%d lines, want 2 (append, not truncate)", len(lines))
	}
	if lines[0]["run_id"] != a.RunID || lines[0]["time"] == "" || lines[0]["type"] != "run.start" {
		t.Errorf("first = %v", lines[0])
	}
	if lines[0]["run_id"] == lines[1]["run_id"] && lines[1]["run_id"] != "fixed" {
		t.Errorf("runs share an id: %v", lines)
	}
	if lines[1]["run_id"] != "fixed" || lines[1]["intent"] != "app" || lines[1]["key"] != "q" || lines[1]["time"] != "1970-01-01T00:00:00Z" {
		t.Errorf("second = %v", lines[1])
	}
	if _, ok := lines[1]["data"]; ok {
		t.Error("empty data should be omitted")
	}
}

// The endpoint in run.start never carries userinfo (credentials in the URL).
func TestStartStripsEndpointUserinfo(t *testing.T) {
	cases := map[string]string{
		"http://user:secret@localhost:5555": "http://localhost:5555",
		"http://localhost:5555":             "http://localhost:5555",
		"http://tok@host:1/p?x=1#f":         "http://host:1/p",
	}
	for in, want := range cases {
		path := filepath.Join(t.TempDir(), "events.jsonl")
		l, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		l.Start("plan", "test", "dir", engine.AWSSettings{Region: "eu-south-1", Endpoint: in})
		l.Close()
		data, _ := os.ReadFile(path)
		if strings.Contains(string(data), "secret") || strings.Contains(string(data), "tok@") {
			t.Errorf("%s: userinfo reached the log: %s", in, data)
		}
		var ev struct {
			Data map[string]any `json:"data"`
		}
		if err := json.Unmarshal(bytes.TrimSpace(data), &ev); err != nil {
			t.Fatal(err)
		}
		if ev.Data["endpoint"] != want {
			t.Errorf("%s: endpoint = %v, want %s", in, ev.Data["endpoint"], want)
		}
	}
}

// run.start says where the command points: only the target, the region and,
// for an emulated AWS, the endpoint. The account is not known to Prosa
// (ADR-029): it comes from the credentials chain.
func TestStartRecordsTarget(t *testing.T) {
	for _, tc := range []struct {
		aws  engine.AWSSettings
		want map[string]any
	}{
		{engine.AWSSettings{Region: "eu-west-1"},
			map[string]any{"target": "aws", "region": "eu-west-1"}},
		{engine.AWSSettings{Region: "eu-south-1", Endpoint: "http://localhost:5555"},
			map[string]any{"target": "emulated", "region": "eu-south-1", "endpoint": "http://localhost:5555"}},
	} {
		path := filepath.Join(t.TempDir(), "events.jsonl")
		l, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		l.Start("plan", "test", "/p", tc.aws)
		l.Close()
		data, _ := os.ReadFile(path)
		var ev struct {
			Data map[string]any `json:"data"`
		}
		if err := json.Unmarshal(bytes.TrimSpace(data), &ev); err != nil {
			t.Fatal(err)
		}
		for _, k := range []string{"command", "version", "dir", "user"} {
			delete(ev.Data, k)
		}
		if !reflect.DeepEqual(ev.Data, tc.want) {
			t.Errorf("run.start data = %v, want %v", ev.Data, tc.want)
		}
	}
}

// planner.result carries the job, the backend and the model (ADR-030, ADR-031).
func TestPlannerResultEventHasJobBackend(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	l.PlannerResult(planner.Result{Intent: "app", Turns: 2, Model: "qwen3", ServedBy: "qwen3", Job: planner.JobPatch, Backend: "openai"})
	l.Close()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var ev struct {
		Type   string         `json:"type"`
		Intent string         `json:"intent"`
		Data   map[string]any `json:"data"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(data), &ev); err != nil {
		t.Fatal(err)
	}
	if ev.Type != "planner.result" || ev.Intent != "app" || ev.Data["job"] != "patch" || ev.Data["backend"] != "openai" || ev.Data["model"] != "qwen3" {
		t.Errorf("event = %+v, want job patch, backend openai, model qwen3", ev)
	}
}

func TestRulesReviewEvent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	l.RulesReview(planner.Result{Intent: "app", Job: "review", Model: "m", ServedBy: "m", Turns: 1, Dropped: 2, Usage: planner.Usage{Input: 3, Output: 4}},
		[]string{"AWS-SQS-001", "AWS-NET-001"}, []string{"AWS-SQS-001 queue.orders"})
	l.Close()
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "reason") {
		t.Errorf("the event carries a reason: %s", data)
	}
	var e struct {
		Type, Intent string
		Data         map[string]any
	}
	if err := json.Unmarshal(bytes.TrimSpace(data), &e); err != nil {
		t.Fatal(err)
	}
	if e.Type != RulesReview || RulesReview != "rules.review" || e.Intent != "app" {
		t.Errorf("event = %+v", e)
	}
	for _, k := range []string{"rules", "violations", "dropped", "turns", "model", "served_by", "cached", "usage"} {
		if _, ok := e.Data[k]; !ok {
			t.Errorf("data lacks %q: %v", k, e.Data)
		}
	}
	if v := e.Data["violations"].([]any); len(v) != 1 || v[0] != "AWS-SQS-001 queue.orders" {
		t.Errorf("violations = %v", v)
	}
}

// The plan event names the rules a change answers (ADR-034), and only then.
func TestPlanEventCarriesRule(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	l.Planned(&engine.Plan{Changes: []engine.Change{
		{Intent: "database", Key: "db.sg_ingress", Type: "aws_vpc_security_group_ingress_rule", Action: engine.Update, Rule: "AWS-NET-001"},
		{Intent: "app", Key: "queue", Type: "aws_sqs_queue", Action: engine.Create},
	}})
	l.Close()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var e struct {
		Type string
		Data struct {
			Changes []map[string]any
		}
	}
	if err := json.Unmarshal(bytes.TrimSpace(data), &e); err != nil {
		t.Fatal(err)
	}
	if len(e.Data.Changes) != 2 {
		t.Fatalf("changes = %v", e.Data.Changes)
	}
	if got := e.Data.Changes[0]["rule"]; got != "AWS-NET-001" {
		t.Errorf("rule of the fix = %v, want AWS-NET-001", got)
	}
	if _, ok := e.Data.Changes[1]["rule"]; ok {
		t.Errorf("a change answering no rule has one: %v", e.Data.Changes[1])
	}
}
