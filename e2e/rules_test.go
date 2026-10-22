package e2e

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giammbo/prosa/internal/engine"
	"github.com/giammbo/prosa/internal/events"
	"github.com/giammbo/prosa/internal/planner"
)

// rulesScript is a project with the three example intents in its root,
// planned (never applied) by the simulated planner and reviewer of
// testdata/llm with the rule set given, its events in a log of its own.
func rulesScript(t *testing.T, convs *conversations, set func(*engine.Engine)) (*script, *eventRecorder) {
	t.Helper()
	llm := &planner.LLM{Messages: convs, Model: planner.DefaultModel, Log: testLog{t}, LogIntent: true}
	rec := newEventRecorder(t, filepath.Join(t.TempDir(), "events.jsonl"), llm)
	s := newScript(t, rec)
	rec.s = s
	set(s.engine)
	s.engine.Reviewer = rec.reviewer(&planner.Reviewer{Backend: planner.Anthropic{Messages: convs}, Model: planner.DefaultModel})
	for _, name := range []string{"database.md", "app.md", "cron.md"} {
		data, err := os.ReadFile(filepath.Join(examplesDir, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(s.dir, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return s, rec
}

// planOnce plans the script's project as `prosa plan` does, as one run of
// the event log, and returns the plan and its render.
func planOnce(ctx context.Context, t *testing.T, s *script, rec *eventRecorder) (*engine.Plan, string) {
	t.Helper()
	end := rec.begin("plan")
	p := s.plan(ctx, t)
	end(nil)
	var out bytes.Buffer
	engine.Render(&out, p, engine.RenderOptions{})
	t.Logf("render of the plan:\n%s", out.String())
	return p, out.String()
}

// changeOf returns the change of node ("<intent>/<key>") in p; false when
// the plan has none.
func changeOf(p *engine.Plan, node string) (engine.Change, bool) {
	i := slices.IndexFunc(p.Changes, func(c engine.Change) bool { return c.Intent+"/"+c.Key == node })
	if i < 0 {
		return engine.Change{}, false
	}
	return p.Changes[i], true
}

// TestRulesFixDuringPlan plans the example intents with the reviewer of the
// built-in rules (ADR-034): the planner's first database state admits
// Postgres from the private subnets' CIDRs, the reviewer flags AWS-NET-001
// on both rules, and the planner answers with an ingress from the app's
// security group. The plan says so in its header, plans that ingress, and
// the event log has the review with its violations.
func TestRulesFixDuringPlan(t *testing.T) {
	RequireE2E(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	convs := newConversations(t, false)
	s, rec := rulesScript(t, convs, func(e *engine.Engine) { e.Rules = builtinRules(t) })
	p, out := planOnce(ctx, t, s, rec)

	if p.IsBlocked() {
		t.Fatalf("plan is blocked: %v", p.Blockers)
	}
	if want := "rules: 10 active\nrules: 1 violation fixed while planning (AWS-NET-001 in database)\n\n"; !strings.HasPrefix(out, want) {
		t.Errorf("render does not start with %q", want)
	}
	convs.check(t, []string{"database-base", "app-base", "cron-base"})
	if got, want := convs.reviewed(), []string{"database:" + reviewSubnets, "database:" + reviewNone, "app:" + reviewNone, "cron:" + reviewNone}; !slices.Equal(got, want) {
		t.Errorf("reviews = %v, want %v", got, want)
	}

	// The fixed ingress: from the app's security group (an id known after
	// the apply), from no CIDR; the subnet rules are not planned.
	c, ok := changeOf(p, "database/db.sg_ingress_from_app")
	if !ok || c.Action != engine.Create {
		t.Fatalf("no create of database/db.sg_ingress_from_app: %v", summary(p))
	}
	if ref := c.After.GetAttr("referenced_security_group_id"); ref.IsKnown() || !c.After.GetAttr("cidr_ipv4").IsNull() {
		t.Errorf("db.sg_ingress_from_app: referenced_security_group_id %#v, cidr_ipv4 %#v; want an unknown group id and no CIDR",
			ref, c.After.GetAttr("cidr_ipv4"))
	}
	for _, az := range []string{"a", "b"} {
		if _, ok := changeOf(p, "database/db.sg_ingress_private_"+az); ok {
			t.Errorf("the plan still has database/db.sg_ingress_private_%s", az)
		}
	}

	// rules.review: the first one, of database, names the violations.
	all, _ := rec.read()
	got := ofType(rec.ofRun(all, "plan"), events.RulesReview)
	if len(got) != 4 {
		t.Fatalf("%d rules.review events, want 4: %v", len(got), got)
	}
	first := got[0]
	var violations, ruleIDs []string
	for _, v := range first.Data["violations"].([]any) {
		violations = append(violations, v.(string))
	}
	for _, r := range first.Data["rules"].([]any) {
		ruleIDs = append(ruleIDs, r.(string))
	}
	if first.Intent != "database" || !slices.Contains(violations, "AWS-NET-001 db.sg_ingress_private_a") ||
		!slices.Contains(violations, "AWS-NET-001 db.sg_ingress_private_b") || !slices.Contains(ruleIDs, "AWS-NET-001") {
		t.Errorf("first rules.review = %s %v, want database with AWS-NET-001 on both subnet rules", first.Intent, first.Data)
	}
	for _, e := range got[1:] {
		if vs := e.Data["violations"].([]any); len(vs) != 0 {
			t.Errorf("rules.review of %s has violations %v, want none", e.Intent, vs)
		}
	}
}

// TestRulesDisableFlagE2E plans the example intents with AWS-NET-001
// disabled as `prosa plan --disable-rule AWS-NET-001` does (the flag's
// parsing is cmd/prosa's): the header says so, the reviewer is never given
// the rule, and the planner's first database state, with its subnet rules,
// is planned as it is.
func TestRulesDisableFlagE2E(t *testing.T) {
	RequireE2E(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	convs := newConversations(t, false)
	convs.reviewQueue = map[string][]string{} // every review finds nothing
	s, rec := rulesScript(t, convs, func(e *engine.Engine) { e.Rules = builtinRules(t, "AWS-NET-001") })
	p, out := planOnce(ctx, t, s, rec)

	if p.IsBlocked() {
		t.Fatalf("plan is blocked: %v", p.Blockers)
	}
	if want := "rules: 9 active, 1 disabled (AWS-NET-001 by --disable-rule)\n\n"; !strings.HasPrefix(out, want) {
		t.Errorf("render does not start with %q", want)
	}
	if strings.Contains(out, "fixed while planning") {
		t.Error("the render reports a fix, with the rule disabled")
	}
	for _, az := range []string{"a", "b"} {
		if c, ok := changeOf(p, "database/db.sg_ingress_private_"+az); !ok || c.Action != engine.Create {
			t.Errorf("no create of database/db.sg_ingress_private_%s: %v", az, summary(p))
		}
	}
	if _, ok := changeOf(p, "database/db.sg_ingress_from_app"); ok {
		t.Error("the plan has database/db.sg_ingress_from_app, the fix of a disabled rule")
	}
	all, _ := rec.read()
	reviews := ofType(rec.ofRun(all, "plan"), events.RulesReview)
	if len(reviews) != 3 {
		t.Errorf("%d rules.review events, want 3 (one per intent)", len(reviews))
	}
	for _, e := range reviews {
		for _, r := range e.Data["rules"].([]any) {
			if r == "AWS-NET-001" {
				t.Errorf("the review of %s was given the disabled AWS-NET-001", e.Intent)
			}
		}
	}
}

// TestRulesNotCheckedWithFixtures plans staging with the demo fixtures, on
// the real binary: no model, so no reviewer, and the plan says that the
// rules were not checked. The fixtures plan the database's ingress from the
// app's security group.
func TestRulesNotCheckedWithFixtures(t *testing.T) {
	RequireE2E(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	p := newEnvProject(t)
	fixtures, err := filepath.Abs(demoFixturesPath)
	if err != nil {
		t.Fatal(err)
	}
	out := p.ok(ctx, "plan", "--env", "envs/staging.md", "--fixtures", fixtures)
	for _, want := range []string{
		"rules: not checked (no model)\n",
		`+ resource "aws_vpc_security_group_ingress_rule" "db.sg_ingress_from_app" {`,
		"+ referenced_security_group_id = (known after apply)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("plan output lacks %q", want)
		}
	}
	if strings.Contains(out, "rules: 10 active") || strings.Contains(out, "db.sg_ingress_private_") {
		t.Error("plan output says the rules were checked, or plans the subnet rules")
	}
}
