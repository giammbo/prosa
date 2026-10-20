package e2e

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/giammbo/prosa/internal/state"
)

// wantGoneFromProject fails t unless intent name, whose state was st, left
// the lock and the state store, and no lock entry keeps an id of its
// resources.
func (s *script) wantGoneFromProject(t *testing.T, name string, st *state.IntentState) {
	t.Helper()
	lk := s.lock(t)
	if _, ok := lk.Intents[name]; ok {
		t.Errorf("%s is still in the lock", name)
	}
	ids := map[string]string{}
	for key, rs := range st.Resources {
		if id := stateAttrs(t, rs)["id"]; id != "" {
			ids[id] = name + "/" + key
		}
	}
	for other, li := range lk.Intents {
		for _, r := range li.Resources {
			if node, ok := ids[r.ID]; ok {
				t.Errorf("lock entry %s/%s has the id of %s", other, r.Key, node)
			}
		}
	}
	left, digest, err := s.engine.State.Load(name)
	if err != nil || digest != "" || len(left.Resources) != 0 {
		t.Errorf("state of %s left behind: digest %q, %d resources (err %v)", name, digest, len(left.Resources), err)
	}
}

// probeGone checks on Moto, with the aws CLI, that every resource of st is
// gone: a NotFound-style error naming the service's code (any other error
// is not proof), or for what ECS keeps once deleted, the INACTIVE status.
func probeGone(ctx context.Context, t *testing.T, st *state.IntentState) {
	t.Helper()
	for _, key := range slices.Sorted(maps.Keys(st.Resources)) {
		rs := st.Resources[key]
		a := stateAttrs(t, rs)
		what := key + " (" + rs.Type + ")"
		switch rs.Type {
		case "aws_scheduler_schedule":
			group := a["group_name"]
			if group == "" {
				group = "default"
			}
			wantNotFound(ctx, t, what, "ResourceNotFoundException", "scheduler", "get-schedule", "--name", a["name"], "--group-name", group)
		case "aws_iam_role":
			wantNotFound(ctx, t, what, "NoSuchEntity", "iam", "get-role", "--role-name", a["name"])
		case "aws_iam_role_policy":
			wantNotFound(ctx, t, what, "NoSuchEntity", "iam", "get-role-policy", "--role-name", a["role"], "--policy-name", a["name"])
		case "aws_sns_topic", "aws_sns_topic_policy":
			wantNotFound(ctx, t, what, "NotFound", "sns", "get-topic-attributes", "--topic-arn", a["arn"])
		case "aws_cloudwatch_event_rule":
			wantNotFound(ctx, t, what, "ResourceNotFoundException", "events", "describe-rule", "--name", a["name"])
		case "aws_cloudwatch_event_target":
			wantNotFound(ctx, t, what, "ResourceNotFoundException", "events", "list-targets-by-rule", "--rule", a["rule"])
		case "aws_ecs_cluster":
			out, err := awsOut(ctx, t, "ecs", "describe-clusters", "--clusters", a["arn"], "--output", "json")
			var res struct {
				Clusters []struct{ Status string }
				Failures []struct{ Reason string }
			}
			if err != nil || json.Unmarshal([]byte(out), &res) != nil {
				t.Errorf("%s: describe-clusters: %v\n%s", what, err, out)
			} else if !(len(res.Clusters) == 1 && res.Clusters[0].Status == "INACTIVE") && !(len(res.Failures) == 1 && res.Failures[0].Reason == "MISSING") {
				t.Errorf("%s is still on Moto, want INACTIVE or MISSING:\n%s", what, out)
			}
		case "aws_ecs_task_definition":
			out, err := awsOut(ctx, t, "ecs", "describe-task-definition", "--task-definition", a["arn"], "--query", "taskDefinition.status", "--output", "text")
			if err != nil || out != "INACTIVE" {
				t.Errorf("%s: status %q (err %v), want INACTIVE", what, out, err)
			}
		case "aws_cloudwatch_log_group":
			out, err := awsOut(ctx, t, "logs", "describe-log-groups", "--log-group-name-prefix", a["name"], "--query", "logGroups[].logGroupName", "--output", "json")
			var names []string
			if err != nil || json.Unmarshal([]byte(out), &names) != nil || slices.Contains(names, a["name"]) {
				t.Errorf("%s is still on Moto (err %v):\n%s", what, err, out)
			}
		default:
			t.Errorf("%s: no Moto probe for this type; add one", what)
		}
	}
}

// wantNotFound fails t unless the aws CLI call fails with the error code
// code ("An error occurred (<code>) when calling ..."): the resource is
// gone. Success means it is still there; another error proves nothing.
func wantNotFound(ctx context.Context, t *testing.T, what, code string, args ...string) {
	t.Helper()
	out, err := awsOut(ctx, t, args...)
	switch {
	case err == nil:
		t.Errorf("%s is still on Moto:\n%s", what, out)
	case !strings.Contains(out, "("+code+")"):
		t.Errorf("%s: aws %s failed without %s, which does not prove it gone: %v\n%s", what, strings.Join(args[:2], " "), code, err, out)
	}
}

// stateAttrs returns the top-level string attributes of a stored state.
func stateAttrs(t *testing.T, rs *state.ResourceState) map[string]string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rs.State, &m); err != nil {
		t.Fatalf("stored state: %v", err)
	}
	out := map[string]string{}
	for k, v := range m {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}
