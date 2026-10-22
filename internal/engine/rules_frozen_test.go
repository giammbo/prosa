package engine

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/zclconf/go-cty/cty"

	"github.com/giammbo/prosa/internal/lock"
	"github.com/giammbo/prosa/rules"
)

// frozenRules is a rule set for the intents already applied: AWS-NET-001 on
// test_net, AWS-ENC-001 on test_db.
func frozenRules(t *testing.T) rules.Set {
	t.Helper()
	s, err := rules.NewSet([]rules.Rule{
		{ID: "AWS-NET-001", Title: "Nets only from their clients", AppliesTo: []string{"test_net"}, Body: "Only from clients.", Origin: "prosa"},
		{ID: "AWS-ENC-001", Title: "Databases encrypt", AppliesTo: []string{"test_db"}, Body: "Encrypt the storage.", Origin: "prosa"},
	}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// frozenPlanner answers an intent's first planning (Mode "plan") with
// first, and any later call with fix, after validating it as the planner
// does; inputs records the later calls.
type frozenPlanner struct {
	t      *testing.T
	first  map[string]Desired
	fix    map[string]Desired
	inputs []PlanInput
}

func (f *frozenPlanner) Plan(ctx context.Context, in PlanInput) (Desired, error) {
	if in.Mode == "plan" {
		return f.first[in.Intent.Name], nil
	}
	f.inputs = append(f.inputs, in)
	d, ok := f.fix[in.Intent.Name]
	if !ok {
		f.t.Errorf("planner asked again for %s", in.Intent.Name)
		return Desired{}, nil
	}
	if err := in.Validate(ctx, d); err != nil {
		f.t.Errorf("Validate(fix of %s) = %v", in.Intent.Name, err)
	}
	return d, nil
}

// databaseText is the intent of the frozen tests: autonomy auto, so that an
// update runs on its own and a replace needs an approval.
const databaseText = "---\nautonomy: auto\n---\na database behind its security group"

// frozenDatabase applies database (an instance and its ingress) without a
// reviewer, then sets the rules and rv: database is now an intent already
// applied, with no rules_hash.
func frozenDatabase(t *testing.T, rv Reviewer) (*testEnv, *frozenPlanner) {
	t.Helper()
	fp := &frozenPlanner{t: t, fix: map[string]Desired{}, first: map[string]Desired{
		"database": {Resources: []DesiredResource{
			{Key: "db.instance", Type: "test_db", Config: map[string]any{"name": "db", "size": 10}},
			{Key: "db.sg_ingress", Type: "test_net", Config: map[string]any{"name": "sg"}},
		}},
	}}
	e := newTestEnv(t, fp)
	e.writeIntent("database", databaseText)
	e.mustApply(e.plan())
	if li := e.loadLock().Intents["database"]; li.RulesHash != "" {
		t.Fatalf("rules_hash written without a reviewer: %q", li.RulesHash)
	}
	e.engine.Rules, e.engine.Reviewer = frozenRules(t), rv
	return e, fp
}

// bothRules is the hash of the rules that apply to database.
func bothRules(t *testing.T) string {
	return rules.Hash(frozenRules(t).For([]string{"test_db", "test_net"}))
}

func TestFrozenNoViolation(t *testing.T) {
	rv := &fakeReviewer{}
	e, fp := frozenDatabase(t, rv)
	// clock: a frozen intent no active rule applies to.
	fp.first["clock"] = Desired{Resources: []DesiredResource{{Key: "c", Type: "test_clock", Config: map[string]any{"name": "c"}}}}
	e.engine.Reviewer = nil
	e.writeIntent("clock", "a clock")
	e.mustApply(e.plan())
	e.engine.Reviewer = rv

	p := e.plan()
	if len(fp.inputs) != 0 {
		t.Fatalf("planner asked %d times", len(fp.inputs))
	}
	if len(rv.inputs) != 1 || rv.inputs[0].Intent != "database" || len(rv.inputs[0].Resources) != 2 {
		t.Fatalf("reviewer inputs = %+v, want database's locked state only", rv.inputs)
	}
	if p.HasChanges() || !p.LockDirty {
		t.Fatalf("HasChanges = %v, LockDirty = %v; want no changes and a lock to update", p.HasChanges(), p.LockDirty)
	}
	e.mustApply(p)
	lk := e.loadLock()
	if got, want := lk.Intents["database"].RulesHash, bothRules(t); got != want {
		t.Errorf("database rules_hash = %q, want %q", got, want)
	}
	if got, want := lk.Intents["clock"].RulesHash, rules.Hash(nil); got != want {
		t.Errorf("clock rules_hash = %q, want the hash of no rule %q", got, want)
	}
}

func TestFrozenSameHashNotReviewed(t *testing.T) {
	e, _ := frozenDatabase(t, &fakeReviewer{})
	e.mustApply(e.plan())
	lockBefore := e.readFile(e.lockPath)

	rv := &fakeReviewer{script: [][]Violation{{{Rule: "AWS-NET-001", Key: "db.sg_ingress", Reason: "never asked"}}}}
	e.engine.Reviewer = rv
	p := e.plan()
	if len(rv.inputs) != 0 {
		t.Fatalf("reviewer called %d times with a current rules_hash", len(rv.inputs))
	}
	if p.HasChanges() || p.LockDirty {
		t.Fatalf("HasChanges = %v, LockDirty = %v; want nothing", p.HasChanges(), p.LockDirty)
	}
	e.mustApply(p)
	if !slices.Equal(lockBefore, e.readFile(e.lockPath)) {
		t.Error("lock rewritten")
	}

	// A rule switched off changes the hash: the intent is reviewed again.
	e.engine.Rules = func() rules.Set {
		s, err := rules.NewSet(frozenRules(t).Active, nil, nil, []string{"AWS-ENC-001"})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}()
	rv.script = nil
	if p = e.plan(); len(rv.inputs) != 1 || !p.LockDirty {
		t.Fatalf("with a rule disabled: reviewer calls %d, LockDirty %v; want 1, true", len(rv.inputs), p.LockDirty)
	}
}

func TestFrozenFixInPlace(t *testing.T) {
	rv := &fakeReviewer{script: [][]Violation{{{Rule: "AWS-NET-001", Key: "db.sg_ingress", Reason: "ingress from a CIDR"}}}}
	e, fp := frozenDatabase(t, rv)
	fp.fix["database"] = Desired{Patch: []lock.PatchOp{{Op: "set", Key: "db.sg_ingress", Path: "name", Value: "sg-from-app"}}}

	p := e.plan()
	if len(fp.inputs) != 1 {
		t.Fatalf("planner calls = %d, want 1", len(fp.inputs))
	}
	in := fp.inputs[0]
	if in.Mode != "patch" || len(in.Previous) != 2 || len(in.Problems) != 1 ||
		!strings.HasPrefix(in.Problems[0], "database/db.sg_ingress: breaks AWS-NET-001: ingress from a CIDR") {
		t.Fatalf("planner input: mode %q, previous %d, problems %q", in.Mode, len(in.Previous), in.Problems)
	}
	if got, want := gatesOf(p), []string{"update database/db.sg_ingress auto"}; !equalStrings(got, want) {
		t.Fatalf("gates = %v, want %v", got, want)
	}
	for _, c := range p.Changes {
		if c.Rule != "AWS-NET-001" {
			t.Errorf("%s: Rule = %q, want AWS-NET-001", c.Key, c.Rule)
		}
	}
	if len(p.Warnings) != 0 || !p.LockDirty {
		t.Errorf("Warnings = %v, LockDirty = %v", p.Warnings, p.LockDirty)
	}
	if !strings.Contains(renderPlan(p), "# database/db.sg_ingress will be updated in-place (rule AWS-NET-001)") {
		t.Errorf("render:\n%s", renderPlan(p))
	}
	e.mustApply(p)
	li := e.loadLock().Intents["database"]
	if got := lockRes(li, "db.sg_ingress").Chosen["name"]; got != "sg-from-app" {
		t.Errorf("chosen name = %v, want the fix", got)
	}
	if li.RulesHash != bothRules(t) {
		t.Errorf("rules_hash = %q, want %q", li.RulesHash, bothRules(t))
	}

	// Fixed and recorded: nothing to review any more.
	rv.inputs = nil
	if p = e.plan(); len(rv.inputs) != 0 || p.HasChanges() || p.LockDirty {
		t.Fatalf("after the fix: reviewer calls %d, changes %v, LockDirty %v", len(rv.inputs), summary(p), p.LockDirty)
	}
}

// replaceFix makes database's fix rename the instance, which the fake
// provider replaces.
func replaceFix(e *testEnv, fp *frozenPlanner) {
	e.fake.replaceOn["test_db"] = []string{"name"}
	fp.fix["database"] = Desired{Patch: []lock.PatchOp{{Op: "set", Key: "db.instance", Path: "name", Value: "db-encrypted"}}}
}

func encViolation() *fakeReviewer {
	return &fakeReviewer{script: [][]Violation{{{Rule: "AWS-ENC-001", Key: "db.instance", Reason: "storage not encrypted"}}}}
}

func TestFrozenFixReplaceIsWarning(t *testing.T) {
	rv := encViolation()
	e, fp := frozenDatabase(t, rv)
	replaceFix(e, fp)
	lockBefore := e.readFile(e.lockPath)

	p := e.plan()
	if len(fp.inputs) != 1 {
		t.Fatalf("planner calls = %d, want 1", len(fp.inputs))
	}
	if p.HasChanges() || p.LockDirty {
		t.Fatalf("changes = %v, LockDirty = %v; want the fix dropped", summary(p), p.LockDirty)
	}
	for _, c := range p.Changes {
		if c.Rule != "" {
			t.Errorf("%s: Rule = %q on a dropped fix", c.Key, c.Rule)
		}
	}
	want := []string{"database: db.instance breaks AWS-ENC-001; fixing it would replace the resource: run prosa plan --replan database"}
	if !slices.Equal(p.Warnings, want) {
		t.Fatalf("Warnings = %q, want %q", p.Warnings, want)
	}
	if got := configOf(p.desired["database"], "db.instance")["name"]; got != "db" {
		t.Errorf("desired name = %v, want the lock's", got)
	}
	e.mustApply(p)
	if !slices.Equal(lockBefore, e.readFile(e.lockPath)) {
		t.Error("lock rewritten for a dropped fix")
	}
}

func TestFrozenFixDeleteIsWarning(t *testing.T) {
	rv := &fakeReviewer{script: [][]Violation{{
		{Rule: "AWS-NET-001", Key: "db.sg_ingress", Reason: "ingress from a CIDR"},
		{Rule: "AWS-ENC-001", Key: "db.instance", Reason: "storage not encrypted"},
	}}}
	e, fp := frozenDatabase(t, rv)
	e.fake.replaceOn["test_db"] = []string{"name"}
	fp.fix["database"] = Desired{Patch: []lock.PatchOp{
		{Op: "remove", Key: "db.sg_ingress"},
		{Op: "set", Key: "db.instance", Path: "name", Value: "db-encrypted"},
	}}
	p := e.plan()
	want := []string{
		"database: db.instance breaks AWS-ENC-001; fixing it would replace the resource: run prosa plan --replan database",
		"database: db.sg_ingress breaks AWS-NET-001; fixing it would delete the resource: run prosa plan --replan database",
	}
	if !slices.Equal(p.Warnings, want) || p.HasChanges() {
		t.Fatalf("Warnings = %q, changes %v; want %q and none", p.Warnings, summary(p), want)
	}
}

func TestReplanAllowsReplace(t *testing.T) {
	rv := encViolation()
	e, fp := frozenDatabase(t, rv)
	replaceFix(e, fp)
	e.engine.Replan = []string{"database"}

	p := e.plan()
	if got, want := gatesOf(p), []string{"replace database/db.instance needs_approval: destroy"}; !equalStrings(got, want) {
		t.Fatalf("gates = %v, want %v", got, want)
	}
	for _, c := range p.Changes {
		if c.Rule != "AWS-ENC-001" {
			t.Errorf("%s: Rule = %q, want AWS-ENC-001", c.Key, c.Rule)
		}
	}
	if len(p.Warnings) != 0 {
		t.Errorf("Warnings = %q, want none", p.Warnings)
	}
	e.mustApply(p)
	if li := e.loadLock().Intents["database"]; lockRes(li, "db.instance").Chosen["name"] != "db-encrypted" || li.RulesHash != bothRules(t) {
		t.Errorf("lock: instance %v, rules_hash %q", lockRes(li, "db.instance").Chosen, li.RulesHash)
	}
}

func TestReplanUnknownIntent(t *testing.T) {
	e := newTestEnv(t, failingPlanner)
	e.writeIntent("database", "a database")
	e.engine.Rules, e.engine.Reviewer = frozenRules(t), &fakeReviewer{}
	e.engine.Replan = []string{"database", "nope"}
	_, err := e.engine.Plan(context.Background())
	if err == nil || err.Error() != "unknown intent nope in --replan" {
		t.Fatalf("Plan error = %v, want unknown intent nope in --replan", err)
	}
	if e.providerCalls != 0 {
		t.Errorf("provider started %d times before the error", e.providerCalls)
	}
}

// The environment's locked state breaks a rule: the fix is planned as the
// environment's patch, and only the environment's lock changes.
func TestEnvFixGoesInPatch(t *testing.T) {
	bp, e, base, hashes := envOnBase(t, stagingPatches(), threeBases())
	baseBefore := e.readFile(base)
	rv := &fakeReviewer{script: [][]Violation{{{Rule: "AWS-ENC-001", Key: "db", Reason: "storage not encrypted"}}}}
	e.engine.Rules, e.engine.Reviewer = frozenRules(t), rv
	fixed := append(stagingPatches()["app"], lock.PatchOp{Op: "set", Key: "db", Path: "name", Value: "db-encrypted"})
	bp.patch["app"] = fixed

	p := e.plan()
	if want := []string{"app patch env"}; !equalStrings(bp.calls, want) {
		t.Fatalf("calls = %v, want %v", bp.calls, want)
	}
	in := bp.inputs["app"]
	if !sameJSON(in.OldPatch, stagingPatches()["app"]) || len(in.Problems) != 1 ||
		!strings.HasPrefix(in.Problems[0], "app/db: breaks AWS-ENC-001: storage not encrypted") {
		t.Fatalf("app input: old patch %v, problems %q", in.OldPatch, in.Problems)
	}
	if rv.inputs[0].Env != "staging" || rv.inputs[0].Intent != "app" || !sameJSON(configOf(rv.inputs[0].Resources, "db")["size"], 20) {
		t.Fatalf("review input = %+v, want app's effective state in staging", rv.inputs[0])
	}
	if got := gatesOf(p); len(got) != 1 || !strings.HasPrefix(got[0], "update app/db ") {
		t.Fatalf("gates = %v, want the update of app/db", got)
	}
	e.mustApply(p)
	lk := e.loadLock()
	if app := lk.Intents["app"]; !sameJSON(app.Patch, fixed) || app.BaseHash != hashes["app"] || lockRes(app, "db").Chosen["name"] != "db-encrypted" {
		t.Errorf("app: patch %v, base_hash %q, db %v", app.Patch, app.BaseHash, lockRes(app, "db").Chosen)
	}
	if got, want := lk.Intents["net"].RulesHash, rules.Hash(frozenRules(t).For([]string{"test_net"})); got != want {
		t.Errorf("net rules_hash = %q, want %q", got, want)
	}
	if !slices.Equal(baseBefore, e.readFile(base)) {
		t.Error("the root lock changed")
	}
}

// intentReviewer answers the review of each intent from its own script
// (nil once it runs out) and records every input.
type intentReviewer struct {
	script map[string][][]Violation
	inputs []ReviewInput
}

func (r *intentReviewer) Review(_ context.Context, in ReviewInput) ([]Violation, error) {
	r.inputs = append(r.inputs, in)
	s := r.script[in.Intent]
	if len(s) == 0 {
		return nil, nil
	}
	r.script[in.Intent] = s[1:]
	return s[0], nil
}

// validatingPlanner validates, as the planner does, each answer of the
// basePlanner to an environment's call for intent name.
type validatingPlanner struct {
	t    *testing.T
	name string
	*basePlanner
}

func (v validatingPlanner) Plan(ctx context.Context, in PlanInput) (Desired, error) {
	d, err := v.basePlanner.Plan(ctx, in)
	if err == nil && in.Env != nil && in.Intent.Name == v.name {
		if verr := in.Validate(ctx, d); verr != nil {
			v.t.Errorf("Validate(answer for %s) = %v", in.Intent.Name, verr)
		}
	}
	return d, err
}

// An intent the environment does not write about (net: no section, no
// common lines, no required name) is the base, whose lock is up to date but
// was never reviewed: it is reviewed as the environment's state, and a
// violation is fixed in the environment's patch.
func TestEnvBaseStateIsReviewed(t *testing.T) {
	for _, tc := range []struct {
		name  string
		viols [][]Violation
		patch []lock.PatchOp
	}{
		{name: "passes"},
		{name: "violation",
			viols: [][]Violation{{{Rule: "AWS-NET-001", Key: "net", Reason: "open to the world"}}},
			patch: []lock.PatchOp{{Op: "set", Key: "net", Path: "name", Value: "net-private"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bp := &basePlanner{patch: stagingPatches()}
			e, base := newBaseEnv(t, validatingPlanner{t, "net", bp}, stagingBody)
			bases := threeBases()
			hashes := e.writeIntents(bases)
			writeBaseLock(t, base, hashes, bases)
			baseBefore := e.readFile(base)
			rv := &intentReviewer{script: map[string][][]Violation{"net": tc.viols}}
			e.engine.Rules, e.engine.Reviewer = frozenRules(t), rv
			if tc.patch != nil {
				bp.patch["net"] = tc.patch
			}

			p := e.plan()
			var netReviews []ReviewInput
			for _, in := range rv.inputs {
				if in.Intent == "net" {
					netReviews = append(netReviews, in)
				}
			}
			if len(netReviews) == 0 {
				t.Fatalf("net was not reviewed; reviews: %+v", rv.inputs)
			}
			if in := netReviews[0]; in.Env != "staging" || !sameJSON(configOf(in.Resources, "net"), bases["net"].Resources[0].Config) {
				t.Fatalf("net review input = %+v, want the base state in staging", in)
			}
			asked := slices.Contains(bp.calls, "net patch env")
			if tc.patch == nil {
				if asked || len(netReviews) != 1 {
					t.Fatalf("calls %v, net reviews %d; want no call for net and one review", bp.calls, len(netReviews))
				}
			} else {
				in := bp.inputs["net"]
				if !asked || len(in.Problems) != 1 || !strings.HasPrefix(in.Problems[0], "net/net: breaks AWS-NET-001: open to the world") {
					t.Fatalf("calls %v, net problems %q; want a patch asked with the violation", bp.calls, in.Problems)
				}
				if want := []string{"AWS-NET-001 in net"}; !slices.Equal(p.Rules.Fixed, want) {
					t.Errorf("Rules.Fixed = %v, want %v", p.Rules.Fixed, want)
				}
			}
			e.mustApply(p)
			net := e.loadLock().Intents["net"]
			if !sameJSON(net.Patch, tc.patch) && !(tc.patch == nil && len(net.Patch) == 0) {
				t.Errorf("net patch = %v, want %v", net.Patch, tc.patch)
			}
			if tc.patch != nil && lockRes(net, "net").Chosen["name"] != "net-private" {
				t.Errorf("net chosen = %v, want the fix", lockRes(net, "net").Chosen)
			}
			if want := rules.Hash(frozenRules(t).For([]string{"test_net"})); net.RulesHash != want {
				t.Errorf("net rules_hash = %q, want %q", net.RulesHash, want)
			}
			if !slices.Equal(baseBefore, e.readFile(base)) {
				t.Error("the base lock changed")
			}
		})
	}
}

func TestNoReviewerKeepsLock(t *testing.T) {
	e, fp := frozenDatabase(t, nil)
	e.engine.Reviewer = nil
	lockBefore := e.readFile(e.lockPath)
	p := e.plan()
	if p.HasChanges() || p.LockDirty || len(fp.inputs) != 0 {
		t.Fatalf("changes %v, LockDirty %v, planner calls %d; want plain reuse", summary(p), p.LockDirty, len(fp.inputs))
	}
	e.mustApply(p)
	if !slices.Equal(lockBefore, e.readFile(e.lockPath)) {
		t.Fatal("lock rewritten")
	}

	// A rules_hash already there stays on an intent reused as it is, and
	// with drift repaired: its desired state is the one that was checked.
	lk := e.loadLock()
	lk.Intents["database"].RulesHash = "sha256:checked"
	if _, err := lock.Save(e.lockPath, lk); err != nil {
		t.Fatal(err)
	}
	delete(e.fake.remote, lockRes(lk.Intents["database"], "db.sg_ingress").ID)
	p = e.plan()
	if got := summary(p); !slices.Contains(got, "create database/db.sg_ingress") {
		t.Fatalf("changes = %v, want the vanished ingress created again", got)
	}
	e.mustApply(p)
	if got := e.loadLock().Intents["database"].RulesHash; got != "sha256:checked" {
		t.Errorf("rules_hash after a drift repair = %q, want it kept", got)
	}

	// An intent planned again without a review loses it: a later run with
	// a reviewer reviews the new state.
	fp.fix["database"] = Desired{Resources: []DesiredResource{
		{Key: "db.instance", Type: "test_db", Config: map[string]any{"name": "db", "size": 20}},
		{Key: "db.sg_ingress", Type: "test_net", Config: map[string]any{"name": "sg"}},
	}}
	e.writeIntent("database", databaseText+", bigger")
	e.mustApply(e.plan())
	if got := e.loadLock().Intents["database"].RulesHash; got != "" {
		t.Errorf("rules_hash of a state rewritten without a reviewer = %q, want none", got)
	}
}

// A fix the provider has nothing to do for is still written: the new
// chosen configuration and the rules_hash, so it is not reviewed again.
func TestFrozenFixNoOpIsWritten(t *testing.T) {
	rv := &fakeReviewer{script: [][]Violation{{{Rule: "AWS-NET-001", Key: "db.sg_ingress", Reason: "ingress from a CIDR"}}}}
	e, fp := frozenDatabase(t, rv)
	e.fake.ignoreChanges["test_net"] = []string{"name"}
	fp.fix["database"] = Desired{Patch: []lock.PatchOp{{Op: "set", Key: "db.sg_ingress", Path: "name", Value: "sg-from-app"}}}

	p := e.plan()
	if p.HasChanges() || !p.LockDirty {
		t.Fatalf("changes %v, LockDirty %v; want only no-ops and a lock to update", summary(p), p.LockDirty)
	}
	e.mustApply(p)
	li := e.loadLock().Intents["database"]
	if got := lockRes(li, "db.sg_ingress").Chosen["name"]; got != "sg-from-app" {
		t.Errorf("chosen name = %v, want the fix", got)
	}
	if li.RulesHash != bothRules(t) || li.IntentHash == "" {
		t.Errorf("rules_hash %q, intent_hash %q; want %q and the intent's", li.RulesHash, li.IntentHash, bothRules(t))
	}
	rv.inputs = nil
	if p = e.plan(); len(rv.inputs) != 0 || p.LockDirty {
		t.Fatalf("after the fix: reviewer calls %d, LockDirty %v; want none", len(rv.inputs), p.LockDirty)
	}
}

func TestFrozenReviewErrorFailsPlan(t *testing.T) {
	boom := errors.New("boom")
	e, _ := frozenDatabase(t, &fakeReviewer{err: boom})
	if _, err := e.engine.Plan(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("Plan error = %v, want the reviewer's", err)
	}
}

// In an environment, the warning of a dropped fix names the environment
// file, and the environment's patch stays.
func TestEnvFixReplaceWarningNamesEnv(t *testing.T) {
	bp, e, _, _ := envOnBase(t, stagingPatches(), threeBases())
	e.engine.EnvFile = "envs/staging.md"
	e.engine.Rules = frozenRules(t)
	e.engine.Reviewer = &fakeReviewer{script: [][]Violation{{{Rule: "AWS-ENC-001", Key: "db", Reason: "storage not encrypted"}}}}
	e.fake.replaceOn["test_db"] = []string{"name"}
	bp.patch["app"] = append(stagingPatches()["app"], lock.PatchOp{Op: "set", Key: "db", Path: "name", Value: "db-encrypted"})

	p := e.plan()
	want := []string{"app: db breaks AWS-ENC-001; fixing it would replace the resource: run prosa plan --env envs/staging.md --replan app"}
	if !slices.Equal(p.Warnings, want) || p.HasChanges() {
		t.Fatalf("Warnings = %q, changes %v; want %q and none", p.Warnings, summary(p), want)
	}
	if !sameJSON(p.patches["app"], stagingPatches()["app"]) {
		t.Errorf("app patch = %v, want the locked one", p.patches["app"])
	}
}

// A fix cut short is resumed from its Pending, like a rewritten intent.
func TestFrozenFixInterruptedResumes(t *testing.T) {
	rv := &fakeReviewer{script: [][]Violation{{{Rule: "AWS-NET-001", Key: "db.sg_ingress", Reason: "ingress from a CIDR"}}}}
	e, fp := frozenDatabase(t, rv)
	fp.fix["database"] = Desired{Patch: []lock.PatchOp{
		{Op: "set", Key: "db.sg_ingress", Path: "name", Value: "sg-from-app"},
		{Op: "set", Key: "db.instance", Path: "size", Value: 20},
	}}
	failed := false
	e.fake.applyErr = func(typeName string, _ cty.Value) error {
		if typeName == "test_net" && !failed {
			failed = true
			return errors.New("boom")
		}
		return nil
	}
	if err := e.apply(e.plan(), approveAll); err == nil {
		t.Fatal("Apply succeeded, want the ingress's failure")
	}
	if li := e.loadLock().Intents["database"]; li.IntentHash != "" {
		t.Fatalf("intent_hash = %q after a cut fix, want none", li.IntentHash)
	}
	if st, _ := e.loadState("database"); st.Pending == nil {
		t.Fatal("no Pending for the cut fix")
	}

	calls := len(fp.inputs)
	p := e.plan()
	if len(fp.inputs) != calls {
		t.Fatalf("planner asked again; want the fix resumed")
	}
	if got := summary(p); !slices.Contains(got, "update database/db.sg_ingress") {
		t.Fatalf("changes = %v, want the rest of the fix", got)
	}
	e.mustApply(p)
	li := e.loadLock().Intents["database"]
	if lockRes(li, "db.sg_ingress").Chosen["name"] != "sg-from-app" || !sameJSON(lockRes(li, "db.instance").Chosen["size"], 20) || li.RulesHash != bothRules(t) {
		t.Errorf("lock after resume: %+v, rules_hash %q", li.Resources, li.RulesHash)
	}
}
