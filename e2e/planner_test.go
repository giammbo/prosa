package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/zclconf/go-cty/cty"

	"github.com/giammbo/prosa/internal/engine"
	"github.com/giammbo/prosa/internal/events"
	"github.com/giammbo/prosa/internal/lock"
	"github.com/giammbo/prosa/internal/planner"
	"github.com/giammbo/prosa/rules"
)

const (
	recordEnv      = "PROSA_LLM_RECORD"
	noRecordings   = "no recorded conversations: run with PROSA_LLM_RECORD=1 and Anthropic credentials"
	examplesDir    = "../examples"
	webServiceName = "web"
	webClusterName = "staging-web"
)

// plannerConversations lists, per intent, the conversations the test has
// with the planner, in order: they are the files testdata/llm/<name>.json.
var plannerConversations = map[string][]string{
	"database": {"database-base", "database-staging"},
	"cron":     {"cron-base", "cron-staging"},
	"app":      {"app-base", "app-staging", "app-blocked", "app-two", "app-staging-two"},
}

// The reviewer's conversations (ADR-034): each review starts the next
// conversation of reviewConversations for its intent, or reviewNone, which
// finds no violation, when there is none left. The first review of database
// is that of the planner's first submission, whose subnet rules break
// AWS-NET-001 (see subnetIngress).
const (
	reviewSubnets = "review-database-subnets"
	reviewNone    = "review-none"
)

var reviewConversations = map[string][]string{"database": {reviewSubnets}}

// isReview reports whether the conversation name is one of the reviewer.
func isReview(name string) bool { return strings.HasPrefix(name, "review-") }

// loadConversation loads testdata/llm/<name>.json as the planner's or, for
// a review-* file, as the reviewer's.
func loadConversation(name, path string) (*planner.Replay, error) {
	if isReview(name) {
		return planner.LoadReviewReplay(path)
	}
	return planner.LoadReplay(path)
}

// subnetProblems are the problems the engine returns to the first database
// submission: one per violation the reviewer found, with the hint.
func subnetProblems() []string {
	var out []string
	for _, v := range subnetViolations {
		out = append(out, fmt.Sprintf("database/%s: breaks %s: %s (to keep it: --disable-rule %s, or except it in the intent)", v["key"], v["rule"], v["reason"], v["rule"]))
	}
	return out
}

// builtinRules is the rule set of a run without a rules/ folder: the
// built-in rules, but those disabled as --disable-rule does.
func builtinRules(t *testing.T, disabled ...string) rules.Set {
	t.Helper()
	builtin, err := rules.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	set, err := rules.NewSet(builtin, nil, nil, disabled)
	if err != nil {
		t.Fatal(err)
	}
	return set
}

// wantSubmissions lists, per conversation, the problems the engine returns
// to each submit_desired_state or submit_changes call, in order; nil means
// accepted. A replay
// cannot react to a problem it did not foresee, so any other answer of the
// engine fails the test. Only checked when replaying.
var wantSubmissions = map[string][][]string{
	"database-base": {subnetProblems(), nil},
	"cron-base": {
		{`cron/job.schedule: validate aws_scheduler_schedule: expected mode to be one of ["OFF" "FLEXIBLE"], got off`},
		nil,
	},
	"app-base": {
		{`app/app.service: validate aws_ecs_service: expected launch_type to be one of ["EC2" "FARGATE" "EXTERNAL" "MANAGED_INSTANCES"], got fargate`},
		nil,
	},
	"database-staging": {{badOpProblem}, nil},
	"app-staging":      {nil},
	"cron-staging":     {nil},
	"app-blocked":      {nil},
	"app-two":          {nil},
	"app-staging-two":  {nil},
}

// TestPlannerOnMoto plans the example intents with the Claude planner and
// the reviewer of the built-in rules, replaying the conversations in
// testdata/llm (recording them with PROSA_LLM_RECORD=1 and credentials), and
// applies them on Moto: create (the reviewer flags the database's first
// state, the planner fixes it), steady state without the planner nor the
// reviewer, a contradictory intent that blocks the plan, then the replica
// change as a single update.
//
// What Moto cannot do is adapted in the desired states (testdata/llm/states)
// and listed here, with what the intents ask that they leave out:
//   - Autoscaling (up to four replicas at 70% CPU) is not planned: Moto does
//     not implement Application Auto Scaling ListTagsForResource, which the
//     provider calls on every read of a scalable target (always tagged, by
//     the ownership tag at least), and retries for more than 20 minutes.
//   - Moto does not serve CloudWatch's rpcv2-cbor protocol, which the
//     provider uses for PutMetricAlarm: "fails two nights in a row" becomes
//     an EventBridge rule on failed runs that publishes each failure to the
//     SNS topic, instead of an alarm on two consecutive daily periods.
//   - Moto drops the description of a security group rule that references
//     another group, so such a rule would be replaced at every plan: the
//     database's ingress from the app's security group has none. The app
//     port admits the VPC's CIDR (AWS-NET-002 allows a CIDR narrower than
//     the internet).
//   - Moto validates no certificate by DNS: it marks it issued about 60
//     seconds after the request, which aws_acm_certificate_validation waits
//     for. The validation CNAME and the record for staging.acme.example need
//     the acme.example hosted zone, which no intent describes: not planned.
//   - Slack through AWS Chatbot needs a workspace authorized in the
//     console: the alert stops at the SNS topic.
//   - The RDS-managed secret holds the password as JSON, not a URL:
//     DATABASE_URL is that secret until the derived secret of ADR-011
//     exists. It is also not the /staging/db/admin of the intent, since
//     manage_master_user_password chooses its name.
func TestPlannerOnMoto(t *testing.T) {
	RequireE2E(t)
	record := os.Getenv(recordEnv) == "1"
	convs := newConversations(t, record)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()

	llm := &planner.LLM{Messages: convs, Model: planner.DefaultModel, Log: testLog{t}, LogIntent: true}
	rec := newEventRecorder(t, filepath.Join(t.TempDir(), "events", "events.jsonl"), llm)
	s := newEnvScript(t, rec, "staging")
	rec.s = s
	s.engine.Rules = builtinRules(t)
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
	step := func(name string, f func(t *testing.T)) {
		t.Helper()
		if !t.Run(name, f) {
			t.FailNow()
		}
	}

	var created []string // intent/key of every resource of the first apply
	var createRun []engine.CheckResult
	step("create", func(t *testing.T) {
		defer rec.begin("create")(nil)
		p := s.plan(ctx, t)
		var out bytes.Buffer
		engine.Render(&out, p, engine.RenderOptions{})
		t.Logf("render of the first plan:\n%s", out.String())
		if p.IsBlocked() {
			t.Fatalf("first plan is blocked: %v", p.Blockers)
		}
		creates := map[string]int{}
		for _, c := range p.Changes {
			if c.Action != engine.Create {
				t.Errorf("%s/%s: action %s, want create", c.Intent, c.Key, c.Action)
			}
			created = append(created, c.Intent+"/"+c.Key)
			creates[c.Intent]++
			// ADR-022: cron is auto, database and app are approve.
			want := engine.GateNeedsApproval
			if c.Intent == "cron" {
				want = engine.GateAuto
			}
			if c.Gate != want || c.GateReason != "" {
				t.Errorf("%s/%s: gate %s %q, want %s", c.Intent, c.Key, c.Gate, c.GateReason, want)
			}
		}
		for _, name := range []string{"database", "app", "cron"} {
			if creates[name] == 0 {
				t.Errorf("no create for intent %s: %v", name, summary(p))
			}
		}
		convs.check(t, createConversations)
		// depends_on: the service is planned (and applied) after the listener
		// it does not refer to.
		at := map[string]int{}
		for i, c := range p.Changes {
			at[c.Intent+"/"+c.Key] = i
		}
		li, haveL := at["app/app.listener_https"]
		si, haveS := at["app/app.service"]
		if !haveL || !haveS {
			t.Fatalf("plan has no app/app.listener_https (%v) or app/app.service (%v): %v", haveL, haveS, summary(p))
		}
		if li > si {
			t.Errorf("the service comes before the listener it depends on: %v", summary(p))
		}
		// One approval, for the database and app creates only.
		asked := 0
		if err := s.engine.Apply(ctx, p, func(p *engine.Plan) bool {
			asked++
			for _, c := range p.ToApprove() {
				if c.Intent == "cron" {
					t.Errorf("approval asked for %s/%s of the auto intent cron", c.Intent, c.Key)
				}
			}
			if n := len(p.ToApprove()); n != creates["database"]+creates["app"] {
				t.Errorf("approval asked for %d changes, want %d", n, creates["database"]+creates["app"])
			}
			return true
		}); err != nil {
			t.Fatalf("apply: %v", err)
		}
		if asked != 1 {
			t.Errorf("approve called %d times, want once", asked)
		}
		// The checks of the three intents ran on the state read after the
		// apply: the network ones are skipped on Moto (ADR-013).
		wantCheckResults(t, p.CheckResults, []string{
			"app: skipped — `/health` on the web app's domain answers 200 within 60 seconds of the end of the apply.: emulated endpoint",
			"app: skipped — The certificate is valid for more than 20 days.: emulated endpoint",
			"app: pass — The number of healthy replicas at rest is the requested one.",
			"cron: pass — There is an active schedule with the stated time.",
			"cron: skipped — The last run (after the first night) finished successfully.: " + loadStates(t, "staging", "cron").Checks[1].Reason,
			"database: pass — The database accepts connections from the private subnet and rejects them from the internet.",
			"database: pass — There is at least one backup newer than 24 hours (after the first day).",
			"database: pass — Storage is encrypted.",
		})
		createRun = p.CheckResults
		// The distinctive value of the simulated cron state reached the lock
		// and Moto (the event log must not have it, see the events step).
		if data, err := os.ReadFile(s.lockPath); err != nil || !bytes.Contains(data, []byte(sentinel)) {
			t.Errorf("lock has no %s (err %v)", sentinel, err)
		}
		if out, err := awsOut(ctx, t, "ecs", "describe-clusters", "--clusters", "staging-cron", "--include", "TAGS"); err != nil || !strings.Contains(out, sentinel) {
			t.Errorf("tags of the cron cluster on Moto have no %s (err %v):\n%s", sentinel, err, out)
		}
		lk := s.lock(t)
		base, err := lock.Load(s.baseLockPath)
		if err != nil {
			t.Fatal(err)
		}
		var sawService, sawTargetGroup bool
		for _, name := range []string{"database", "app", "cron"} {
			li := lk.Intents[name]
			if li == nil {
				t.Fatalf("lock has no intent %s", name)
			}
			// The environment lock derives from the base lock: base_hash is
			// the intent_hash of the base, which is the hash of the file, and
			// the patch is what staging changes.
			bl := base.Intents[name]
			if bl == nil || bl.IntentHash != s.baseHashOf(t, name) || li.BaseHash != bl.IntentHash || len(li.Patch) == 0 {
				t.Errorf("lock of %s: base_hash %q, %d patch ops; base lock %+v; want the hash of the file and a patch", name, li.BaseHash, len(li.Patch), bl)
			}
			if bl != nil && len(bl.Patch) != 0 {
				t.Errorf("the base lock of %s has a patch", name)
			}
			if li.IntentHash != s.hashOf(t, name) {
				t.Errorf("lock intent_hash of %s = %q, want the file's hash", name, li.IntentHash)
			}
			if got, want := len(li.Checks), len(loadStates(t, "staging", name).Checks); got != want || got == 0 {
				t.Errorf("lock of %s has %d checks, want %d", name, got, want)
			}
			for _, r := range li.Resources {
				if r.ID == "" {
					t.Errorf("%s/%s has no id in the lock", name, r.Key)
				}
				// The template and depends_on are stored as written.
				if r.Key == "app.service" {
					sawService = true
					if tags, _ := r.Chosen["tags"].(map[string]any); tags["Name"] != "${app/app.cluster.name}-svc" ||
						!slices.Equal(r.DependsOn, []string{"app/app.listener_https"}) {
						t.Errorf("lock of %s/%s: tags %#v, depends_on %v; want the template and the listener", name, r.Key, r.Chosen["tags"], r.DependsOn)
					}
				}
				// Submitted as a lone object, stored as a one-element list.
				if r.Key == "app.target_group" {
					sawTargetGroup = true
					if hc, ok := r.Chosen["health_check"].([]any); !ok || len(hc) != 1 {
						t.Errorf("lock chosen health_check of %s/%s = %#v, want a one-element list", name, r.Key, r.Chosen["health_check"])
					}
				}
			}
		}
		if !sawService || !sawTargetGroup {
			t.Errorf("lock has no app/app.service (%v) or app/app.target_group (%v)", sawService, sawTargetGroup)
		}
	})

	step("steady state", func(t *testing.T) {
		defer rec.begin("steady state")(nil)
		before := convs.requestCount()
		p := s.plan(ctx, t)
		var out bytes.Buffer
		engine.Render(&out, p, engine.RenderOptions{})
		if p.HasChanges() || p.LockDirty || p.IsBlocked() || out.String() != "rules: 10 active\n\nNo changes. Your infrastructure matches the intents.\n" {
			t.Fatalf("plan after apply: HasChanges=%v LockDirty=%v Blockers=%v:\n%s", p.HasChanges(), p.LockDirty, p.Blockers, out.String())
		}
		sawService := false
		for _, c := range p.Changes {
			if c.Drifted {
				t.Errorf("%s/%s drifted after apply: %v", c.Intent, c.Key, c.DriftAttrs)
			}
			// The template reached Moto resolved.
			if c.Intent == "app" && c.Key == "app.service" {
				sawService = true
				tags, k := c.Before.GetAttr("tags"), cty.StringVal("Name")
				if tags.IsNull() || !tags.HasIndex(k).True() || tags.Index(k).AsString() != webClusterName+"-svc" {
					t.Errorf("tags of the service on Moto = %#v, want Name %s-svc", tags, webClusterName)
				}
			}
			if len(c.InfoDrift) > 0 {
				t.Logf("%s/%s informational drift: %v", c.Intent, c.Key, c.InfoDrift)
			}
		}
		if !sawService {
			t.Error("the steady-state plan has no app/app.service to check on Moto")
		}
		if got := convs.requestCount(); got != before {
			t.Errorf("the planner was called for unchanged intents: %d requests, want %d", got, before)
		}
	})

	step("contradiction", func(t *testing.T) {
		var runErr error
		end := rec.begin("contradiction")
		defer func() { end(runErr) }()
		s.editIntent(t, "app.md", replicasCheck[0], replicasCheck[1])
		before := s.files(t)
		p := s.plan(ctx, t)
		var out bytes.Buffer
		engine.Render(&out, p, engine.RenderOptions{})
		t.Logf("render of the blocked plan:\n%s", out.String())
		if len(p.Blockers) != 1 || !strings.HasPrefix(p.Blockers[0], "app: ") ||
			!strings.Contains(p.Blockers[0], "There are exactly two healthy replicas") {
			t.Fatalf("blockers = %q, want one for app citing its check", p.Blockers)
		}
		if p.HasChanges() {
			t.Errorf("blocked plan has changes: %v", summary(p))
		}
		if want := "rules: 10 active\n\nBlocked:\n  " + p.Blockers[0] + "\n"; out.String() != want {
			t.Errorf("render = %q, want %q", out.String(), want)
		}
		convs.check(t, append(slices.Clone(createConversations), "app-blocked"))
		err := s.engine.Apply(ctx, p, func(*engine.Plan) bool { return true })
		runErr = err
		if err == nil || !strings.Contains(err.Error(), "plan is blocked") {
			t.Fatalf("apply of a blocked plan: %v, want it refused", err)
		}
		if !sameFiles(s.files(t), before) {
			t.Error("a blocked plan rewrote the lock or the state store")
		}
		s.wantDesiredCount(ctx, t, 1)
	})

	step("replica change", func(t *testing.T) {
		defer rec.begin("replica change")(nil)
		s.editIntent(t, "app.md", replicasSentence[0], replicasSentence[1])
		p := s.plan(ctx, t)
		var out bytes.Buffer
		engine.Render(&out, p, engine.RenderOptions{})
		t.Logf("render of the replica change:\n%s", out.String())
		if p.IsBlocked() {
			t.Fatalf("plan is blocked: %v", p.Blockers)
		}
		var changed []engine.Change
		for _, c := range p.Changes {
			if c.Action != engine.NoOp {
				changed = append(changed, c)
			}
		}
		if len(changed) != 1 || changed[0].Action != engine.Update || changed[0].Intent != "app" || changed[0].Type != "aws_ecs_service" {
			t.Fatalf("changes = %v, want one update of the app ECS service", summary(p))
		}
		c := changed[0]
		render := out.String()
		header := fmt.Sprintf("  # app/%s will be updated in-place\n  # (needs approval)\n  ~ resource \"aws_ecs_service\" %q {\n", c.Key, c.Key)
		blocks := regexp.MustCompile(`(?m)^  # \S+ (will be|must be) `).FindAllString(render, -1)
		changedAttrs := regexp.MustCompile(`(?m)^      [~+-] `).FindAllString(render, -1)
		if !strings.Contains(render, header) || len(blocks) != 1 || len(changedAttrs) != 1 ||
			!regexp.MustCompile(`(?m)^      ~ desired_count += 1 -> 2$`).MatchString(render) ||
			!strings.HasSuffix(render, "Plan: 0 to add, 1 to change, 0 to destroy.\n") {
			t.Errorf("render = %q, want one update of %s changing only desired_count 1 -> 2", render, c.Key)
		}
		convs.check(t, append(slices.Clone(createConversations), "app-blocked", "app-two", "app-staging-two"))
		s.apply(ctx, t, p)
		wantCheckResults(t, p.CheckResults, []string{
			"app: skipped — `/health` on the web app's domain answers 200 within 60 seconds of the end of the apply.: emulated endpoint",
			"app: skipped — The certificate is valid for more than 20 days.: emulated endpoint",
			"app: pass — There are exactly two healthy replicas at rest.",
		})
		before := convs.requestCount()
		s.noChanges(ctx, t)
		if got := convs.requestCount(); got != before {
			t.Errorf("the planner was called for unchanged intents: %d requests, want %d", got, before)
		}
		s.wantDesiredCount(ctx, t, 2)
	})

	// The script's third step: cron.md goes away. Its autonomy is auto, but
	// destroying always asks (ADR-022).
	var cronNodes []string
	step("delete refused", func(t *testing.T) {
		var runErr error
		end := rec.begin("delete refused")
		defer func() { end(runErr) }()
		if err := os.Remove(filepath.Join(s.dir, "cron.md")); err != nil {
			t.Fatal(err)
		}
		p := s.plan(ctx, t)
		for _, c := range p.Changes {
			if c.Action == engine.NoOp {
				continue
			}
			if c.Intent != "cron" || c.Action != engine.Delete || c.Gate != engine.GateNeedsApproval || c.GateReason != "destroy" {
				t.Errorf("%s/%s: %s, gate %s %q; want a cron delete needing approval for destroy", c.Intent, c.Key, c.Action, c.Gate, c.GateReason)
			}
			cronNodes = append(cronNodes, c.Intent+"/"+c.Key)
		}
		if want := len(loadStates(t, "staging", "cron").Resources); len(cronNodes) != want {
			t.Fatalf("plan = %v, want %d cron deletes", summary(p), want)
		}
		before := s.files(t)
		asked := 0
		runErr = s.engine.Apply(ctx, p, func(p *engine.Plan) bool { asked++; return false })
		if !errors.Is(runErr, engine.ErrNotApproved) || asked != 1 {
			t.Fatalf("apply: %v after %d approvals, want ErrNotApproved after one", runErr, asked)
		}
		var out, notApplied bytes.Buffer
		engine.Render(&out, p, engine.RenderOptions{})
		engine.RenderNotApplied(&notApplied, p)
		t.Logf("render after the refusal:\n%s", out.String())
		if got := strings.Count(notApplied.String(), ": not approved\n"); got != len(cronNodes) {
			t.Errorf("RenderNotApplied names %d changes as not approved, want %d:\n%s", got, len(cronNodes), notApplied.String())
		}
		if got := strings.Count(out.String(), ": not approved\n"); got != len(cronNodes) || strings.Count(out.String(), "  # (not applied: not approved)\n") != len(cronNodes) {
			t.Errorf("Render does not say that the %d changes were not approved:\n%s", len(cronNodes), out.String())
		}
		if !sameFiles(s.files(t), before) {
			t.Error("a refused deletion rewrote the lock or the state store")
		}
		// Nothing was deleted: the infrastructure is still there.
		if out, err := awsOut(ctx, t, "scheduler", "get-schedule", "--name", "staging-clearsessions"); err != nil {
			t.Errorf("the schedule is gone from Moto: %v\n%s", err, out)
		}
		if out, err := awsOut(ctx, t, "iam", "get-role", "--role-name", "staging-clearsessions-scheduler"); err != nil {
			t.Errorf("the scheduler role is gone from Moto: %v\n%s", err, out)
		}
	})

	step("delete approved", func(t *testing.T) {
		defer rec.begin("delete approved")(nil)
		cron, _, err := s.engine.State.Load("cron")
		if err != nil {
			t.Fatal(err)
		}
		p := s.plan(ctx, t)
		if got := summary(p); len(got) != len(cronNodes) {
			t.Fatalf("plan after the refusal = %v, want the %d cron deletes again", got, len(cronNodes))
		}
		s.apply(ctx, t, p)
		// Every cron key is gone: from the lock, from the state store and
		// from Moto, each probed by its type.
		s.wantGoneFromProject(t, "cron", cron)
		probeGone(ctx, t, cron)
		// The rest of the project is untouched.
		s.noChanges(ctx, t)
		s.wantDesiredCount(ctx, t, 2)
	})

	step("events", func(t *testing.T) {
		all, raw := rec.read()
		// No attribute value in the log: the simulated state's tag value
		// reached Moto and the lock, never the events.
		if bytes.Contains(raw, []byte(sentinel)) {
			t.Errorf("the event log holds %s", sentinel)
		}
		wantEnd := map[string]string{
			"create": "ok", "steady state": "ok", "contradiction": "error", "replica change": "ok",
			"delete refused": "not_approved", "delete approved": "ok",
		}
		ids := map[string]bool{}
		for _, e := range all {
			ids[e.RunID] = true
		}
		if len(ids) != len(wantEnd) {
			t.Errorf("%d runs in the log, want %d (one per step)", len(ids), len(wantEnd))
		}
		for name, result := range wantEnd {
			evs := rec.ofRun(all, name)
			if len(evs) < 3 || evs[0].Type != events.RunStart || evs[len(evs)-1].Type != events.RunEnd {
				t.Errorf("run %q does not start with run.start and end with run.end: %d events", name, len(evs))
				continue
			}
			if got := evs[len(evs)-1].str("result"); got != result {
				t.Errorf("run %q ends %q, want %q", name, got, result)
			}
			if len(ofType(evs, events.PlanEvent)) == 0 {
				t.Errorf("run %q has no plan event", name)
			}
		}

		// planner.result, as "<intent>:<job>": the base of each intent (plan:
		// the lock has none) and its patch for staging on the first run; on the
		// contradiction only the changes to the base of app (patch: the lock
		// has one), which is blocked, so no patch for staging; on the replica
		// change the changes to the base of app and the patch for staging,
		// which cannot be applied again because it replaces the checks, which
		// the edit changed; none when nothing changed.
		planned := func(name string) []string {
			var got []string
			for _, e := range ofType(rec.ofRun(all, name), events.PlannerResult) {
				got = append(got, e.Intent+":"+e.str("job"))
			}
			slices.Sort(got)
			return got
		}
		for name, want := range map[string][]string{
			"create":         {"app:patch", "app:plan", "cron:patch", "cron:plan", "database:patch", "database:plan"},
			"steady state":   nil,
			"contradiction":  {"app:patch"},
			"replica change": {"app:patch", "app:patch"},
			"delete refused": nil, "delete approved": nil,
		} {
			if got := planned(name); !slices.Equal(got, want) {
				t.Errorf("planner.result of %q for %v, want %v", name, got, want)
			}
		}

		// rules.review, as "<intent>: <violations>": on the first run the base
		// of each intent (database twice: its first state breaks AWS-NET-001
		// on both subnet rules, the fix passes), then the staging patch of
		// each; on the replica change the new base of app and its patch; none
		// where nothing was planned, nor on the blocked app.
		reviews := func(name string) []string {
			var got []string
			for _, e := range ofType(rec.ofRun(all, name), events.RulesReview) {
				vs, _ := e.Data["violations"].([]any)
				got = append(got, fmt.Sprintf("%s: %v", e.Intent, vs))
			}
			return got
		}
		for name, want := range map[string][]string{
			"create": {"database: [AWS-NET-001 db.sg_ingress_private_a AWS-NET-001 db.sg_ingress_private_b]", "database: []",
				"app: []", "cron: []", "database: []", "app: []", "cron: []"},
			"steady state":   nil,
			"contradiction":  nil,
			"replica change": {"app: []", "app: []"},
			"delete refused": nil, "delete approved": nil,
		} {
			if got := reviews(name); !slices.Equal(got, want) {
				t.Errorf("rules.review of %q:\n  %v\nwant\n  %v", name, got, want)
			}
		}

		// apply.op: ok for every resource created.
		var ops []string
		for _, e := range ofType(rec.ofRun(all, "create"), events.ApplyOp) {
			if e.str("action") != "create" || e.str("result") != "ok" {
				t.Errorf("apply.op %v, want an ok create", e.Data)
			}
			ops = append(ops, e.Intent+"/"+e.Key)
		}
		slices.Sort(ops)
		if want := slices.Sorted(slices.Values(created)); !slices.Equal(ops, want) {
			t.Errorf("apply.op nodes of the first apply:\n  %v\nwant\n  %v", ops, want)
		}
		if got := ofType(rec.ofRun(all, "replica change"), events.ApplyOp); len(got) != 1 || got[0].str("action") != "update" || got[0].str("result") != "ok" {
			t.Errorf("apply.op of the replica change = %v, want one ok update", got)
		}
		var deleted []string
		for _, e := range ofType(rec.ofRun(all, "delete approved"), events.ApplyOp) {
			if e.str("action") != "delete" || e.str("result") != "ok" {
				t.Errorf("apply.op %v, want an ok delete", e.Data)
			}
			deleted = append(deleted, e.Intent+"/"+e.Key)
		}
		slices.Sort(deleted)
		if want := slices.Sorted(slices.Values(cronNodes)); !slices.Equal(deleted, want) {
			t.Errorf("apply.op nodes of the deletion:\n  %v\nwant\n  %v", deleted, want)
		}
		if got := ofType(rec.ofRun(all, "delete refused"), events.ApplyOp); len(got) != 0 {
			t.Errorf("apply.op events in the refused run: %v", got)
		}

		// check: one per check result, with its status.
		var checks, wantChecks []string
		for _, e := range ofType(rec.ofRun(all, "create"), events.Check) {
			checks = append(checks, e.Intent+": "+e.str("status")+" — "+e.str("name"))
			typ, status := e.str("type"), e.str("status")
			if want := map[bool]string{true: "skipped", false: "pass"}[typ == "http_get" || typ == "tls_days" || typ == "unverifiable"]; status != want {
				t.Errorf("check %q of type %s is %s, want %s", e.str("name"), typ, status, want)
			}
		}
		for _, r := range createRun {
			wantChecks = append(wantChecks, r.Intent+": "+r.Status+" — "+r.Name)
		}
		if !slices.Equal(checks, wantChecks) || len(checks) != 8 {
			t.Errorf("check events of the first run:\n  %s\nwant the %d results\n  %s", strings.Join(checks, "\n  "), len(wantChecks), strings.Join(wantChecks, "\n  "))
		}

		// approval: one for the creates, none for the auto cron; the
		// refusal and the approval of the deletion.
		approvals := func(name string) []event { return ofType(rec.ofRun(all, name), events.Approval) }
		granted := func(e event) bool { g, _ := e.Data["granted"].(bool); return g }
		if a := approvals("create"); len(a) != 1 || !granted(a[0]) || a[0].str("mode") != "flag" {
			t.Errorf("approvals of the first run = %v, want one granted, mode flag", a)
		} else if n := len(a[0].Data["changes"].([]any)); n != len(created)-len(loadStates(t, "staging", "cron").Resources) {
			t.Errorf("first approval covers %d changes, want the creates of database and app", n)
		}
		if a := approvals("delete refused"); len(a) != 1 || granted(a[0]) || len(a[0].Data["changes"].([]any)) != len(cronNodes) {
			t.Errorf("approvals of the refused run = %v, want one not granted for %d deletes", a, len(cronNodes))
		}
		if a := approvals("delete approved"); len(a) != 1 || !granted(a[0]) || len(a[0].Data["changes"].([]any)) != len(cronNodes) {
			t.Errorf("approvals of the approved run = %v, want one granted for %d deletes", a, len(cronNodes))
		}
		if a := approvals("steady state"); len(a) != 0 {
			t.Errorf("approvals of the steady state: %v", a)
		}
	})
}

// createConversations are the conversations of the first plan, in order: the
// base of each intent, in planning order, then the patch of each.
var createConversations = []string{"database-base", "app-base", "cron-base", "database-staging", "app-staging", "cron-staging"}

// sentinel is a non-sensitive value of the simulated cron state (a tag of
// its cluster) that must reach Moto and the lock but never the event log.
const sentinel = "sentinel-7f3a"

// wantCheckResults compares check results, each as "<intent>: <status> —
// <name>[: <detail>]", with want.
func wantCheckResults(t *testing.T, got []engine.CheckResult, want []string) {
	t.Helper()
	var lines []string
	for _, r := range got {
		line := fmt.Sprintf("%s: %s — %s", r.Intent, r.Status, r.Name)
		if r.Detail != "" {
			line += ": " + r.Detail
		}
		lines = append(lines, line)
	}
	if !slices.Equal(lines, want) {
		t.Errorf("check results:\n  %s\nwant\n  %s", strings.Join(lines, "\n  "), strings.Join(want, "\n  "))
	}
}

// editIntent replaces the only occurrence of old in the intent file.
func (s *script) editIntent(t *testing.T, file, old, repl string) {
	t.Helper()
	path := filepath.Join(s.dir, file)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(data), old); n != 1 {
		t.Fatalf("%s: %q occurs %d times, want once", file, old, n)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(string(data), old, repl, 1)), 0o644); err != nil {
		t.Fatal(err)
	}
}

// wantDesiredCount checks the web app service's desired count on Moto with
// the aws CLI.
func (s *script) wantDesiredCount(ctx context.Context, t *testing.T, want int) {
	t.Helper()
	cmd := exec.CommandContext(ctx, awsCLI(t), "--endpoint-url", MotoURL(), "--region", motoRegion,
		"ecs", "describe-services", "--cluster", webClusterName, "--services", webServiceName,
		"--query", "services[0].desiredCount", "--output", "text")
	cmd.Env = awsCLIEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("aws ecs describe-services: %v\n%s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != fmt.Sprint(want) {
		t.Errorf("desired count on Moto = %s, want %d", got, want)
	}
}

// testLog writes the planner's progress lines to the test log.
type testLog struct{ t *testing.T }

func (l testLog) Write(p []byte) (int, error) {
	l.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// conversations is the Messages of the planner and of the reviewer: each
// planning request opens the next conversation of its intent (see
// plannerConversations), each review the next of reviewConversations (see
// reviewNone), replayed from testdata/llm or, when recording, sent to the
// API and written there (a review is sent, not recorded).
type conversations struct {
	record     bool
	open       func(name string) (planner.Messages, error)
	openReview func(name string) (planner.Messages, error)

	mu          sync.Mutex
	queue       map[string][]string
	reviewQueue map[string][]string
	current     planner.Messages // the planner's conversation
	review      planner.Messages // the reviewer's
	started     []string
	reviews     []string // "<intent>:<conversation>" of each review, in order
	replays     map[string]*planner.Replay
	requests    int
}

// newConversations replays the recorded conversations, skipping the test
// when one is missing, or records new ones with the SDK's credentials.
func newConversations(t *testing.T, record bool) *conversations {
	t.Helper()
	c := &conversations{record: record, queue: map[string][]string{}, reviewQueue: map[string][]string{}, replays: map[string]*planner.Replay{}}
	for intent, names := range plannerConversations {
		c.queue[intent] = slices.Clone(names)
	}
	for intent, names := range reviewConversations {
		c.reviewQueue[intent] = slices.Clone(names)
	}
	path := func(name string) string { return filepath.Join(llmDir, name+".json") }
	if record {
		l, err := planner.NewLLM(planner.DefaultModel)
		if err != nil {
			t.Fatal(err)
		}
		c.open = func(name string) (planner.Messages, error) {
			return &planner.Recorder{Inner: l.Messages, Path: path(name)}, nil
		}
		c.openReview = func(string) (planner.Messages, error) { return l.Messages, nil }
		return c
	}
	for _, names := range plannerConversations {
		for _, name := range names {
			if _, err := os.Stat(path(name)); err != nil {
				t.Skip(noRecordings)
			}
		}
	}
	c.open = func(name string) (planner.Messages, error) {
		r, err := planner.LoadReplay(path(name))
		if err != nil {
			return nil, err
		}
		c.replays[name] = r
		return r, nil
	}
	c.openReview = func(name string) (planner.Messages, error) { return planner.LoadReviewReplay(path(name)) }
	return c
}

var (
	planIntentRE   = regexp.MustCompile(`^Plan the intent "([^"]+)"`)
	reviewIntentRE = regexp.MustCompile(`^Review the intent "([^"]+)"`)
)

// New sends a request to the conversation it belongs to, the planner's or,
// when its first message asks for a review, the reviewer's (a review runs
// inside a planning conversation, while the planner waits for its answer): a
// request with a single message starts the next conversation of its intent.
func (c *conversations) New(ctx context.Context, params anthropic.BetaMessageNewParams) (*anthropic.BetaMessage, error) {
	var text string
	if len(params.Messages) > 0 {
		if blocks := params.Messages[0].Content; len(blocks) > 0 && blocks[0].OfText != nil {
			text = blocks[0].OfText.Text
		}
	}
	if m := reviewIntentRE.FindStringSubmatch(text); m != nil {
		return c.newReview(ctx, params, m[1])
	}
	c.mu.Lock()
	if len(params.Messages) == 1 {
		m := planIntentRE.FindStringSubmatch(text)
		if m == nil {
			c.mu.Unlock()
			return nil, fmt.Errorf("conversations: first message does not name an intent: %.80q", text)
		}
		if len(c.queue[m[1]]) == 0 {
			c.mu.Unlock()
			return nil, fmt.Errorf("conversations: no conversation left for intent %q", m[1])
		}
		name := c.queue[m[1]][0]
		c.queue[m[1]] = c.queue[m[1]][1:]
		msgs, err := c.open(name)
		if err != nil {
			c.mu.Unlock()
			return nil, err
		}
		c.current = msgs
		c.started = append(c.started, name)
	}
	cur := c.current
	c.requests++
	c.mu.Unlock()
	if cur == nil {
		return nil, fmt.Errorf("conversations: request before any conversation started")
	}
	return cur.New(ctx, params)
}

// newReview is New for a request of the reviewer about intent.
func (c *conversations) newReview(ctx context.Context, params anthropic.BetaMessageNewParams, intent string) (*anthropic.BetaMessage, error) {
	c.mu.Lock()
	if len(params.Messages) == 1 {
		name := reviewNone
		if q := c.reviewQueue[intent]; len(q) > 0 {
			name, c.reviewQueue[intent] = q[0], q[1:]
		}
		msgs, err := c.openReview(name)
		if err != nil {
			c.mu.Unlock()
			return nil, err
		}
		c.review = msgs
		c.reviews = append(c.reviews, intent+":"+name)
	}
	cur := c.review
	c.requests++
	c.mu.Unlock()
	if cur == nil {
		return nil, fmt.Errorf("conversations: review request before any review started")
	}
	return cur.New(ctx, params)
}

// reviewed returns "<intent>:<conversation>" of each review so far.
func (c *conversations) reviewed() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.reviews)
}

func (c *conversations) requestCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.requests
}

// check asserts that the conversations started so far are want, in order,
// and, when replaying, that each was replayed to its end with the problems
// of wantSubmissions and no failed schema or search call.
func (c *conversations) check(t *testing.T, want []string) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if !slices.Equal(c.started, want) {
		t.Fatalf("conversations = %v, want %v", c.started, want)
	}
	if c.record {
		return
	}
	for _, name := range want {
		r := c.replays[name]
		if len(r.Requests) != len(r.Turns) {
			t.Errorf("%s: %d of %d recorded turns replayed", name, len(r.Requests), len(r.Turns))
			continue
		}
		got, err := submissions(r)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if !equalProblems(got, wantSubmissions[name]) {
			t.Errorf("%s: submissions answered\n%s\nwant\n%s", name, formatProblems(got), formatProblems(wantSubmissions[name]))
		}
	}
}

// submissions returns the problems the engine returned to each
// submit_desired_state call of a replayed conversation (nil when accepted),
// and fails on a search or schema call answered with an error.
func submissions(r *planner.Replay) ([][]string, error) {
	var out [][]string
	for i, raw := range r.Turns {
		var turn struct {
			Content []struct {
				Type, ID, Name string
			} `json:"content"`
		}
		if err := json.Unmarshal(raw, &turn); err != nil {
			return nil, err
		}
		results := map[string]toolResult{}
		if i+1 < len(r.Requests) {
			var err error
			if results, err = lastToolResults(r.Requests[i+1]); err != nil {
				return nil, err
			}
		}
		for _, b := range turn.Content {
			if b.Type != "tool_use" {
				continue
			}
			res, answered := results[b.ID]
			switch {
			case isSubmit(b.Name) && (!answered || !res.isError):
				out = append(out, nil)
			case isSubmit(b.Name):
				out = append(out, strings.Split(res.text, "\n"))
			case !answered:
				return nil, fmt.Errorf("turn %d: %s call %s has no result", i+1, b.Name, b.ID)
			case res.isError:
				return nil, fmt.Errorf("turn %d: %s failed: %s", i+1, b.Name, res.text)
			}
		}
	}
	return out, nil
}

// isSubmit reports whether name is one of the two tools that submit an answer.
func isSubmit(name string) bool { return name == "submit_desired_state" || name == "submit_changes" }

type toolResult struct {
	isError bool
	text    string
}

// lastToolResults returns the tool results of the last message of a
// request, by tool_use id.
func lastToolResults(p anthropic.BetaMessageNewParams) (map[string]toolResult, error) {
	if len(p.Messages) == 0 {
		return nil, nil
	}
	data, err := json.Marshal(p.Messages[len(p.Messages)-1])
	if err != nil {
		return nil, err
	}
	var msg struct {
		Content []struct {
			Type      string `json:"type"`
			ToolUseID string `json:"tool_use_id"`
			IsError   bool   `json:"is_error"`
			Content   []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"content"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		return nil, err
	}
	out := map[string]toolResult{}
	for _, b := range msg.Content {
		if b.Type != "tool_result" {
			continue
		}
		var text strings.Builder
		for _, c := range b.Content {
			text.WriteString(c.Text)
		}
		out[b.ToolUseID] = toolResult{b.IsError, text.String()}
	}
	return out, nil
}

func equalProblems(a, b [][]string) bool {
	return slices.EqualFunc(a, b, func(x, y []string) bool { return slices.Equal(x, y) })
}

func formatProblems(subs [][]string) string {
	var b strings.Builder
	for i, probs := range subs {
		if probs == nil {
			fmt.Fprintf(&b, "  submission %d: accepted\n", i+1)
			continue
		}
		fmt.Fprintf(&b, "  submission %d:\n", i+1)
		for _, p := range probs {
			fmt.Fprintf(&b, "    %s\n", p)
		}
	}
	return b.String()
}
