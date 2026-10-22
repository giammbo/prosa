package engine

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/zclconf/go-cty/cty"

	"github.com/giammbo/prosa/rules"
)

// fakeReviewer returns, call after call, the violations of script (nil once
// it runs out) or err, and records every input.
type fakeReviewer struct {
	script [][]Violation
	err    error
	inputs []ReviewInput
}

func (f *fakeReviewer) Review(_ context.Context, in ReviewInput) ([]Violation, error) {
	f.inputs = append(f.inputs, in)
	if f.err != nil {
		return nil, f.err
	}
	if len(f.script) == 0 {
		return nil, nil
	}
	v := f.script[0]
	f.script = f.script[1:]
	return v, nil
}

// testRules is a rule set on the fake provider's types: AWS-NET-001 on
// test_net, AWS-NET-002 on test_db, AWS-ENC-001 on test_lb; disabled are
// switched off by --disable-rule.
func testRules(t *testing.T, disabled ...string) rules.Set {
	t.Helper()
	rule := func(id, title, typ string) rules.Rule {
		return rules.Rule{ID: id, Title: title, AppliesTo: []string{typ}, Body: title + ".\n\nThe second paragraph.", Origin: "prosa"}
	}
	s, err := rules.NewSet([]rules.Rule{
		rule("AWS-NET-001", "Nets only from their clients", "test_net"),
		rule("AWS-NET-002", "Databases are private", "test_db"),
		rule("AWS-ENC-001", "Balancers encrypt", "test_lb"),
	}, nil, nil, disabled)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// staticCatalog describes every type as "resource <name>\n".
type staticCatalog struct{}

func (staticCatalog) Search(string, int) []string { return nil }
func (staticCatalog) Describe(name string) (string, error) {
	return "resource " + name + "\n", nil
}

func TestSchemaCarriesRules(t *testing.T) {
	s, err := rules.NewSet([]rules.Rule{{
		ID: "AWS-NET-001", Title: "Databases only from their clients", AppliesTo: []string{"aws_security_group", "aws_db_instance"},
		Body: "Allow ingress to a database\nonly from the security groups of its clients.\n\nNot from CIDRs.", Origin: "prosa",
	}}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	cat := rulesCatalog{staticCatalog{}, s}
	got, err := cat.Describe("aws_security_group")
	if err != nil {
		t.Fatal(err)
	}
	want := "resource aws_security_group\n\nRules for aws_security_group (binding; a reviewer checks them):\n" +
		"- AWS-NET-001 Databases only from their clients: Allow ingress to a database only from the security groups of its clients. Not from CIDRs.\n"
	if got != want {
		t.Errorf("Describe(aws_security_group) =\n%q\nwant\n%q", got, want)
	}
	for _, name := range []string{"aws_security_group.ingress", "aws_s3_bucket"} {
		got, err := cat.Describe(name)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(got, "Rules for") {
			t.Errorf("Describe(%s) = %q, want no rules", name, got)
		}
	}
}

// netDesired is a desired state of one test_net under key.
func netDesired(key string) Desired {
	return Desired{Resources: []DesiredResource{{Key: key, Type: "test_net", Config: map[string]any{"name": key}}}}
}

func TestReviewViolationIsProblem(t *testing.T) {
	var first, second error
	pl := plannerFunc(func(ctx context.Context, in PlanInput) (Desired, error) {
		first = in.Validate(ctx, netDesired("sg_ingress"))
		second = in.Validate(ctx, netDesired("sg_ingress"))
		return netDesired("sg_ingress"), nil
	})
	e := newTestEnv(t, pl)
	rv := &fakeReviewer{script: [][]Violation{{
		{Rule: "AWS-NET-001", Key: "sg_ingress", Reason: "ingress from a subnet CIDR"},
		{Rule: "AWS-NET-001", Key: "ghost", Reason: "a key the state does not have"},
	}}}
	e.engine.Rules, e.engine.Reviewer = testRules(t), rv
	e.writeIntent("database", "a private net")
	p := e.plan()

	var ve *ValidationError
	if !errors.As(first, &ve) || len(ve.Problems) != 2 {
		t.Fatalf("first Validate = %v, want two problems", first)
	}
	ghost, sg := ve.Problems[0], ve.Problems[1] // sorted by key: "" first
	if sg.Intent != "database" || sg.Key != "sg_ingress" || sg.Type != "test_net" ||
		!strings.HasPrefix(sg.Message, "breaks AWS-NET-001: ingress from a subnet CIDR") {
		t.Errorf("problem = %+v", sg)
	}
	if ghost.Intent != "database" || ghost.Key != "" || !strings.HasPrefix(ghost.Message, "breaks AWS-NET-001: a key the state does not have") {
		t.Errorf("problem on an unknown key = %+v, want an empty key", ghost)
	}
	if second != nil {
		t.Errorf("second Validate = %v, want nil", second)
	}
	if want := []string{"AWS-NET-001 in database"}; !slices.Equal(p.Rules.Fixed, want) {
		t.Errorf("Rules.Fixed = %v, want %v", p.Rules.Fixed, want)
	}
	if !p.Rules.Checked || p.Rules.Active != 3 {
		t.Errorf("Rules = %+v, want checked with 3 active", p.Rules)
	}
}

func TestReviewInputScope(t *testing.T) {
	var schemaText string
	pl := plannerFunc(func(ctx context.Context, in PlanInput) (Desired, error) {
		if in.Intent.Name == "app" {
			return Desired{Resources: []DesiredResource{{Key: "db", Type: "test_db", Config: map[string]any{"name": "db"}}}}, nil
		}
		schemaText, _ = in.Schemas.Describe("test_net")
		if err := in.Validate(ctx, netDesired("sg")); err != nil {
			t.Errorf("Validate = %v", err)
		}
		return netDesired("sg"), nil
	})
	e := newTestEnv(t, pl)
	rv := &fakeReviewer{}
	e.engine.Rules, e.engine.Reviewer = testRules(t), rv
	e.engine.Env = testEnvFile("staging", "Everything small.\n\n## database\nAWS-NET-001 does not apply in staging: the VPN reaches it.\n\n## app\nAWS-NET-002 is not for app here.")
	e.writeIntent("app", "an app with a database")
	e.writeIntent("database", "---\ndepends_on: [app]\n---\na net for the app.\n\nAWS-NET-001 does not apply: the VPN 10.8.0.0/16 must reach it.\n")
	e.plan()

	var in *ReviewInput
	for i := range rv.inputs {
		if rv.inputs[i].Intent == "database" {
			in = &rv.inputs[i]
		}
	}
	if in == nil {
		t.Fatalf("database not reviewed: %+v", rv.inputs)
	}
	if in.Env != "staging" {
		t.Errorf("Env = %q, want staging", in.Env)
	}
	var ids []string
	for _, r := range in.Rules {
		ids = append(ids, r.ID)
	}
	if want := []string{"AWS-NET-001"}; !slices.Equal(ids, want) {
		t.Errorf("Rules = %v, want %v", ids, want)
	}
	if want := []AvailableResource{{Intent: "app", Key: "db", Type: "test_db"}}; !slices.Equal(in.Available, want) {
		t.Errorf("Available = %v, want %v", in.Available, want)
	}
	if len(in.Resources) != 1 || in.Resources[0].Key != "sg" || in.Resources[0].Type != "test_net" {
		t.Errorf("Resources = %+v", in.Resources)
	}
	want := []rules.Exception{
		{ID: "AWS-NET-001", Text: "AWS-NET-001 does not apply: the VPN 10.8.0.0/16 must reach it."},
		{ID: "AWS-NET-001", Text: "AWS-NET-001 does not apply in staging: the VPN reaches it."},
	}
	if !slices.Equal(in.Exceptions, want) {
		t.Errorf("Exceptions = %q, want %q", in.Exceptions, want)
	}
	if !strings.Contains(schemaText, "Rules for test_net (binding; a reviewer checks them):\n- AWS-NET-001 ") {
		t.Errorf("Describe(test_net) = %q, want the rules of test_net", schemaText)
	}
}

func TestReviewErrorFailsPlan(t *testing.T) {
	boom := errors.New("boom")
	var got error
	pl := plannerFunc(func(ctx context.Context, in PlanInput) (Desired, error) {
		got = in.Validate(ctx, netDesired("sg"))
		return netDesired("sg"), nil
	})
	e := newTestEnv(t, pl)
	e.engine.Rules, e.engine.Reviewer = testRules(t), &fakeReviewer{err: boom}
	e.writeIntent("database", "a net")
	e.plan()
	var ve *ValidationError
	if got == nil || errors.As(got, &ve) || !errors.Is(got, boom) {
		t.Fatalf("Validate = %v, want an error wrapping boom, not a ValidationError", got)
	}
}

func TestUnfixableRuleMessage(t *testing.T) {
	var got error
	pl := plannerFunc(func(ctx context.Context, in PlanInput) (Desired, error) {
		got = in.Validate(ctx, netDesired("sg"))
		return netDesired("sg"), nil
	})
	e := newTestEnv(t, pl)
	e.engine.Rules = testRules(t)
	e.engine.Reviewer = &fakeReviewer{script: [][]Violation{{{Rule: "AWS-NET-001", Key: "sg", Reason: "open to the world"}}}}
	e.writeIntent("database", "a net")
	e.plan()
	var ve *ValidationError
	if !errors.As(got, &ve) || len(ve.Problems) != 1 {
		t.Fatalf("Validate = %v, want one problem", got)
	}
	msg := ve.Problems[0].Message
	if want := "breaks AWS-NET-001: open to the world (to keep it: --disable-rule AWS-NET-001, or except it in the intent)"; msg != want {
		t.Errorf("message = %q, want %q", msg, want)
	}
	if !strings.Contains(got.Error(), "--disable-rule AWS-NET-001") || !strings.Contains(got.Error(), "except it in the intent") {
		t.Errorf("error = %q, want both ways out", got)
	}
}

func TestRulesNotChecked(t *testing.T) {
	var in PlanInput
	var verr error
	var schemaText string
	pl := plannerFunc(func(ctx context.Context, i PlanInput) (Desired, error) {
		in = i
		verr = i.Validate(ctx, netDesired("sg"))
		schemaText, _ = i.Schemas.Describe("test_net")
		return netDesired("sg"), nil
	})
	e := newTestEnv(t, pl)
	e.engine.Rules = testRules(t)
	e.writeIntent("database", "a net.\n\nAWS-ENC-009 is not a rule.")
	p := e.plan()
	if verr != nil {
		t.Errorf("Validate = %v", verr)
	}
	if in.RulesHash != "" {
		t.Errorf("RulesHash = %q, want empty without a reviewer", in.RulesHash)
	}
	if strings.Contains(schemaText, "Rules for") {
		t.Errorf("Describe(test_net) = %q, want no rules without a reviewer", schemaText)
	}
	if p.Rules.Checked || len(p.Warnings) > 0 || len(p.Rules.Excepted) > 0 {
		t.Errorf("Rules = %+v, Warnings = %v, want not checked and nothing else", p.Rules, p.Warnings)
	}

	// With a reviewer the input carries the hash of the active rules.
	e.engine.Reviewer = &fakeReviewer{}
	e.writeIntent("database", "a net, again")
	e.plan()
	if want := rules.Hash(e.engine.Rules.Active); in.RulesHash != want {
		t.Errorf("RulesHash = %q, want %q", in.RulesHash, want)
	}
}

func TestExceptionsFor(t *testing.T) {
	e := newTestEnv(t, plannerFunc(func(ctx context.Context, in PlanInput) (Desired, error) {
		return netDesired("sg"), nil
	}))
	e.engine.Rules, e.engine.Reviewer = testRules(t, "AWS-NET-002"), &fakeReviewer{}
	e.writeIntent("database", "A net.\n\nAWS-NET-001 does not apply: the VPN must reach it.\n\n"+
		"AWS-ENC-009 is not needed.\n\nAWS-NET-002 is off anyway.\n")
	p := e.plan()
	if want := []string{"AWS-NET-001 in database"}; !slices.Equal(p.Rules.Excepted, want) {
		t.Errorf("Excepted = %v, want %v", p.Rules.Excepted, want)
	}
	if want := []string{"unknown rule AWS-ENC-009 named in database"}; !slices.Equal(p.Warnings, want) {
		t.Errorf("Warnings = %v, want %v", p.Warnings, want)
	}
	want := []rules.Exception{{ID: "AWS-NET-001", Text: "AWS-NET-001 does not apply: the VPN must reach it."}}
	if got := p.exceptionsFor(p.intents[0]); !slices.Equal(got, want) {
		t.Errorf("exceptionsFor = %q, want %q", got, want)
	}
	if want := []string{"unknown rule AWS-ENC-009 named in database"}; !slices.Equal(p.Warnings, want) {
		t.Errorf("Warnings after a second exceptionsFor = %v, want no duplicate", p.Warnings)
	}
}

// netObj is a test_net object without tags.
func netObj(id, name string) cty.Value {
	return cty.ObjectVal(map[string]cty.Value{"id": cty.StringVal(id), "name": cty.StringVal(name), "tags": cty.NullVal(cty.Map(cty.String))})
}

func TestRenderRules(t *testing.T) {
	fake := newFakeProvider()
	p := &Plan{schema: fake.schema, Changes: []Change{{
		Intent: "database", Key: "sg", Type: "test_net", Action: Update, Gate: GateAuto, Rule: "AWS-NET-001",
		Before: netObj("test_net-1", "old"), After: netObj("test_net-1", "sg"),
	}},
		Rules: RulesSummary{
			Checked: true, Active: 10,
			Disabled: []rules.DisabledRule{{ID: "AWS-NET-002", By: "--disable-rule"}},
			Excepted: []string{"AWS-NET-001 in database"},
			Fixed:    []string{"AWS-NET-001 in database"},
		},
		Warnings: []string{"database: db.instance breaks AWS-ENC-001"},
	}
	checkRender(t, renderPlan(p), `rules: 10 active, 1 disabled (AWS-NET-002 by --disable-rule)
rules: AWS-NET-001 excepted in database
rules: 1 violation fixed while planning (AWS-NET-001 in database)

Prosa will perform the following actions:

  # database/sg will be updated in-place (rule AWS-NET-001)
  ~ resource "test_net" "sg" {
        id   = "test_net-1"
      ~ name = "old" -> "sg"
    }

Plan: 0 to add, 1 to change, 0 to destroy.
Warning: database: db.instance breaks AWS-ENC-001
`)

	p.Rules = RulesSummary{Checked: true, Active: 3, Fixed: []string{"AWS-NET-001 in app", "AWS-NET-002 in database"}}
	p.Warnings = nil
	if got := renderPlan(p); !strings.HasPrefix(got, "rules: 3 active\nrules: 2 violations fixed while planning (AWS-NET-001 in app, AWS-NET-002 in database)\n\nProsa will") {
		t.Errorf("render =\n%s", got)
	}

	p.Rules, p.Changes = RulesSummary{}, nil
	p.Warnings = []string{"something"}
	checkRender(t, renderPlan(p), "rules: not checked (no model)\n\nNo changes. Your infrastructure matches the intents.\nWarning: something\n")
}
