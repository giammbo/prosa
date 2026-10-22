package engine

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// gatesOf lists the changes that are not no-ops as
// "<action> <intent>/<key> <gate>[: <reason>]".
func gatesOf(p *Plan) []string {
	var out []string
	for _, c := range p.Changes {
		if c.Action == NoOp {
			continue
		}
		s := string(c.Action) + " " + c.Intent + "/" + c.Key + " " + string(c.Gate)
		if c.GateReason != "" {
			s += ": " + c.GateReason
		}
		out = append(out, s)
	}
	return out
}

// neverApprove is an approve function that must not be called.
func neverApprove(t *testing.T) func(*Plan) bool {
	return func(p *Plan) bool {
		t.Errorf("approve called: %v", gatesOf(p))
		return false
	}
}

func TestGateFromAutonomy(t *testing.T) {
	netOnly := Desired{Resources: []DesiredResource{{Key: "net", Type: "test_net", Config: map[string]any{"name": "net"}}}}
	for _, tc := range []struct {
		autonomy, gate string
		approveCalls   int
		applies        int
	}{
		{"observe", "create app/net refused: observe", 0, 0},
		{"advise", "create app/net refused: advise", 0, 0},
		{"approve", "create app/net needs_approval", 1, 1},
		{"auto", "create app/net auto", 0, 1},
	} {
		t.Run(tc.autonomy, func(t *testing.T) {
			text := "---\nautonomy: " + tc.autonomy + "\n---\na net"
			e := newTestEnv(t, StaticPlanner{hashOf(text): netOnly})
			h := e.writeIntent("app", text)
			p := e.plan()
			if got := gatesOf(p); !equalStrings(got, []string{tc.gate}) {
				t.Fatalf("gates = %v, want [%s]", got, tc.gate)
			}
			if !p.HasChanges() || p.Applicable() != (tc.applies > 0) {
				t.Fatalf("HasChanges=%v Applicable=%v", p.HasChanges(), p.Applicable())
			}
			calls := 0
			if err := e.apply(p, func(*Plan) bool { calls++; return true }); err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if calls != tc.approveCalls || e.applyCalls() != tc.applies {
				t.Fatalf("approve calls = %d, applies = %d; want %d, %d", calls, e.applyCalls(), tc.approveCalls, tc.applies)
			}
			if tc.applies == 0 {
				if e.readFile(e.lockPath) != nil || e.readFile(e.statePath("app")) != nil {
					t.Fatal("lock or state written for a refused plan")
				}
				return
			}
			if li := e.loadLock().Intents["app"]; li == nil || li.IntentHash != h {
				t.Fatalf("lock intent = %+v, want the new hash", li)
			}
		})
	}
}

// seedNetAndDB records intent app (with hash of "old app") with a net and
// an independent db.
func seedNetAndDB(e *testEnv) {
	owner := map[string]any{"prosa/intent": "app"}
	e.seed("app", hashOf("old app"),
		seedRes{key: "net", typ: "test_net", chosen: map[string]any{"name": "net"},
			state: map[string]any{"id": "test_net-9", "name": "net", "tags": owner}},
		seedRes{key: "db", typ: "test_db", chosen: map[string]any{"name": "db"},
			state: map[string]any{"id": "test_db-9", "name": "db", "tags": owner}},
	)
}

func TestGateDestroyInAutoNeedsApproval(t *testing.T) {
	text := "---\nautonomy: auto\n---\nonly the net"
	d := Desired{Resources: []DesiredResource{{Key: "net", Type: "test_net", Config: map[string]any{"name": "net"}}}}
	e := newTestEnv(t, StaticPlanner{hashOf(text): d})
	h := e.writeIntent("app", text)
	seedNetAndDB(e)
	lockBefore, stateBefore := e.readFile(e.lockPath), e.readFile(e.statePath("app"))

	p := e.plan()
	if got, want := gatesOf(p), []string{"delete app/db needs_approval: destroy"}; !equalStrings(got, want) {
		t.Fatalf("gates = %v, want %v", got, want)
	}
	if got := p.ToApprove(); len(got) != 1 || got[0].Key != "db" {
		t.Fatalf("ToApprove = %v", got)
	}
	if err := e.apply(p, func(*Plan) bool { return false }); !errors.Is(err, ErrNotApproved) {
		t.Fatalf("refused: err = %v, want ErrNotApproved", err)
	}
	if got, want := gatesOf(p), []string{"delete app/db refused: not approved"}; !equalStrings(got, want) {
		t.Fatalf("gates after the refusal = %v, want %v", got, want)
	}
	if e.applyCalls() != 0 || !bytes.Equal(e.readFile(e.lockPath), lockBefore) || !bytes.Equal(e.readFile(e.statePath("app")), stateBefore) {
		t.Fatalf("a refused destroy changed something: calls %v", e.fake.calls)
	}

	calls := 0
	if err := e.apply(e.plan(), func(*Plan) bool { calls++; return true }); err != nil {
		t.Fatalf("approved: %v", err)
	}
	if _, ok := e.fake.remote["test_db-9"]; ok || calls != 1 {
		t.Fatalf("db still there (%v) or approve calls %d", ok, calls)
	}
	if li := e.loadLock().Intents["app"]; li.IntentHash != h || lockRes(li, "db") != nil {
		t.Fatalf("lock intent = %+v", li)
	}

	// A replace destroys too.
	text2 := "---\nautonomy: auto\n---\na renamed net"
	e.engine.Planner = StaticPlanner{hashOf(text2): {Resources: []DesiredResource{{Key: "net", Type: "test_net", Config: map[string]any{"name": "net2"}}}}}
	e.fake.replaceOn["test_net"] = []string{"name"}
	e.writeIntent("app", text2)
	if got, want := gatesOf(e.plan()), []string{"replace app/net needs_approval: destroy"}; !equalStrings(got, want) {
		t.Fatalf("replace gates = %v, want %v", got, want)
	}
}

func TestGateAllowDestroy(t *testing.T) {
	for _, tc := range []struct{ autonomy, gate string }{
		{"auto", "delete app/db auto"},
		{"approve", "delete app/db needs_approval"},
	} {
		t.Run(tc.autonomy, func(t *testing.T) {
			text := "---\nautonomy: " + tc.autonomy + "\nallow_destroy: true\n---\nonly the net"
			d := Desired{Resources: []DesiredResource{{Key: "net", Type: "test_net", Config: map[string]any{"name": "net"}}}}
			e := newTestEnv(t, StaticPlanner{hashOf(text): d})
			e.writeIntent("app", text)
			seedNetAndDB(e)
			p := e.plan()
			if got := gatesOf(p); !equalStrings(got, []string{tc.gate}) {
				t.Fatalf("gates = %v, want [%s]", got, tc.gate)
			}
			if tc.autonomy != "auto" {
				return
			}
			if err := e.apply(p, neverApprove(t)); err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if _, ok := e.fake.remote["test_db-9"]; ok {
				t.Fatal("db not deleted")
			}
		})
	}
}

func TestGateRemovedIntentNeedsApproval(t *testing.T) {
	e := newTestEnv(t, failingPlanner)
	e.seed("old", hashOf("old text"), seedRes{key: "net", typ: "test_net", chosen: map[string]any{"name": "net"},
		state: map[string]any{"id": "test_net-9", "name": "net", "tags": map[string]any{"prosa/intent": "old"}}})
	if got, want := gatesOf(e.plan()), []string{"delete old/net needs_approval: destroy"}; !equalStrings(got, want) {
		t.Fatalf("gates = %v, want %v", got, want)
	}
}

func TestGatePrunesDependentsOfRefused(t *testing.T) {
	baseText := "---\nautonomy: observe\n---\na net"
	appText := "---\nautonomy: auto\n---\na db on the base net"
	otherText := "---\nautonomy: auto\n---\nanother net"
	e := newTestEnv(t, StaticPlanner{
		hashOf(baseText):  {Resources: []DesiredResource{{Key: "net", Type: "test_net", Config: map[string]any{"name": "net"}}}},
		hashOf(appText):   {Resources: []DesiredResource{{Key: "db", Type: "test_db", Config: map[string]any{"name": "db", "net_id": "${base/net.id}"}}}},
		hashOf(otherText): {Resources: []DesiredResource{{Key: "net", Type: "test_net", Config: map[string]any{"name": "other"}}}},
	})
	e.writeIntent("base", baseText)
	e.writeIntent("app", appText)
	h := e.writeIntent("other", otherText)

	p := e.plan()
	want := []string{"create base/net refused: observe", "create app/db refused: depends on base/net", "create other/net auto"}
	if got := gatesOf(p); !equalStrings(got, want) {
		t.Fatalf("gates = %v, want %v", got, want)
	}
	if err := e.apply(p, neverApprove(t)); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if e.applyCalls() != 1 {
		t.Fatalf("applies = %d, want only other/net: %v", e.applyCalls(), e.fake.calls)
	}
	lk := e.loadLock()
	if lk.Intents["base"] != nil || lk.Intents["app"] != nil {
		t.Fatalf("lock has the refused intents: %+v", lk.Intents)
	}
	if li := lk.Intents["other"]; li == nil || li.IntentHash != h {
		t.Fatalf("lock of other = %+v", li)
	}
	if e.readFile(e.statePath("base")) != nil || e.readFile(e.statePath("app")) != nil {
		t.Fatal("state written for a refused intent")
	}
}

func TestGateNotApprovedPrunesDestroyOrderAndKeepsHash(t *testing.T) {
	// net drops n (allowed: auto) and adds m; app (approve) drops db, which
	// referred to net/n: n can go only after db.
	netText := "---\nautonomy: auto\nallow_destroy: true\n---\nonly m"
	appText := "---\nautonomy: approve\n---\nno db"
	planned := map[string]int{}
	fixtures := StaticPlanner{
		hashOf(netText): {Resources: []DesiredResource{{Key: "m", Type: "test_net", Config: map[string]any{"name": "m"}}}},
		hashOf(appText): {},
	}
	e := newTestEnv(t, plannerFunc(func(ctx context.Context, in PlanInput) (Desired, error) {
		planned[in.Intent.Name]++
		return fixtures.Plan(ctx, in)
	}))
	e.seed("net", hashOf("old net"), seedRes{key: "n", typ: "test_net", chosen: map[string]any{"name": "n"},
		state: map[string]any{"id": "test_net-9", "name": "n", "tags": map[string]any{"prosa/intent": "net"}}})
	e.seed("app", hashOf("old app"), seedRes{key: "db", typ: "test_db", chosen: map[string]any{"name": "db", "net_id": "${net/n.id}"},
		state: map[string]any{"id": "test_db-9", "name": "db", "net_id": "test_net-9", "tags": map[string]any{"prosa/intent": "app"}}})
	hNet := e.writeIntent("net", netText)
	hApp := e.writeIntent("app", appText)

	p := e.plan()
	want := []string{"delete app/db needs_approval: destroy", "delete net/n auto", "create net/m auto"}
	if got := gatesOf(p); !equalStrings(got, want) {
		t.Fatalf("gates = %v, want %v", got, want)
	}
	var asked []string
	if err := e.apply(p, func(p *Plan) bool {
		for _, c := range p.ToApprove() {
			asked = append(asked, c.Intent+"/"+c.Key)
		}
		return false
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !equalStrings(asked, []string{"app/db"}) {
		t.Fatalf("asked to approve %v, want [app/db]", asked)
	}
	want = []string{"delete app/db refused: not approved", "delete net/n refused: depends on app/db", "create net/m auto"}
	if got := gatesOf(p); !equalStrings(got, want) {
		t.Fatalf("gates after the refusal = %v, want %v", got, want)
	}
	if e.applyCalls() != 1 {
		t.Fatalf("applies = %d, want only net/m: %v", e.applyCalls(), e.fake.calls)
	}
	if _, ok := e.fake.remote["test_net-9"]; !ok {
		t.Fatal("net/n deleted although it depends on a refused change")
	}
	lk := e.loadLock()
	if li := lk.Intents["net"]; li.IntentHash != "" || lockRes(li, "n") == nil || lockRes(li, "m") == nil || lockRes(li, "m").ID == "" {
		t.Fatalf("lock of net = %+v, want n kept, m applied and an empty intent_hash", li)
	}
	if li := lk.Intents["app"]; li.IntentHash != hashOf("old app") || lockRes(li, "db") == nil {
		t.Fatalf("lock of app = %+v, want it untouched", li)
	}
	e.checkConsistent("net")
	var buf bytes.Buffer
	Render(&buf, p, RenderOptions{})
	if !strings.HasSuffix(buf.String(), "Not applied:\n  app/db: not approved\n  net/n: depends on app/db\n") {
		t.Fatalf("render = %q", buf.String())
	}

	// net, applied in part, resumes the desired state it was executing (its
	// state's Pending) without asking the planner; approved, everything goes.
	before := planned["net"]
	p = e.plan()
	if planned["net"] != before {
		t.Fatalf("net planned %d times, want it resumed (%d)", planned["net"], before)
	}
	if got, want := gatesOf(p), []string{"delete app/db needs_approval: destroy", "delete net/n auto"}; !equalStrings(got, want) {
		t.Fatalf("second plan gates = %v, want %v", got, want)
	}
	e.mustApply(p)
	lk = e.loadLock()
	if lk.Intents["net"].IntentHash != hNet || lk.Intents["app"].IntentHash != hApp || len(e.fake.remote) != 1 {
		t.Fatalf("after approval: lock %+v, remote %v", lk.Intents, e.fake.remote)
	}
}

func TestGateNotApprovedPrunesDeleteOfResourceMovedOff(t *testing.T) {
	// app (approve) moves its db off net/x (rule c: x can go only after the
	// db's update); net (auto, allow_destroy) drops x and adds m.
	netText := "---\nautonomy: auto\nallow_destroy: true\n---\nonly m"
	appText := "---\nautonomy: approve\n---\na db on no net"
	e := newTestEnv(t, StaticPlanner{
		hashOf(netText): {Resources: []DesiredResource{{Key: "m", Type: "test_net", Config: map[string]any{"name": "m"}}}},
		hashOf(appText): {Resources: []DesiredResource{{Key: "db", Type: "test_db", Config: map[string]any{"name": "db"}}}},
	})
	e.seed("net", hashOf("old net"), seedRes{key: "x", typ: "test_net", chosen: map[string]any{"name": "x"},
		state: map[string]any{"id": "test_net-9", "name": "x", "tags": map[string]any{"prosa/intent": "net"}}})
	e.seed("app", hashOf("old app"), seedRes{key: "db", typ: "test_db", chosen: map[string]any{"name": "db", "net_id": "${net/x.id}"},
		state: map[string]any{"id": "test_db-9", "name": "db", "net_id": "test_net-9", "tags": map[string]any{"prosa/intent": "app"}}})
	e.writeIntent("net", netText)
	e.writeIntent("app", appText)

	p := e.plan()
	want := []string{"delete net/x auto", "update app/db needs_approval", "create net/m auto"}
	if got := gatesOf(p); !equalStrings(got, want) {
		t.Fatalf("gates = %v, want %v", got, want)
	}
	if err := e.apply(p, func(*Plan) bool { return false }); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	want = []string{"delete net/x refused: depends on app/db", "update app/db refused: not approved", "create net/m auto"}
	if got := gatesOf(p); !equalStrings(got, want) {
		t.Fatalf("gates after the refusal = %v, want %v", got, want)
	}
	if _, ok := e.fake.remote["test_net-9"]; !ok {
		t.Fatal("net/x deleted while app/db still uses it")
	}
	if db := e.fake.remote["test_db-9"]; db.GetAttr("net_id").AsString() != "test_net-9" {
		t.Fatalf("app/db updated after a refusal: %#v", db)
	}
	if e.applyCalls() != 1 {
		t.Fatalf("applies = %d, want only net/m: %v", e.applyCalls(), e.fake.calls)
	}
	if li := e.loadLock().Intents["net"]; li.IntentHash != "" || lockRes(li, "x") == nil {
		t.Fatalf("lock of net = %+v, want x kept and an empty intent_hash", li)
	}
}

func TestRenderGates(t *testing.T) {
	p := &Plan{dir: "intents", Changes: []Change{
		{Intent: "old", Key: "net", Type: "test_net", Action: Delete, Gate: GateNeedsApproval, GateReason: "destroy"},
		{Intent: "base", Key: "net", Type: "test_net", Action: Create, Gate: GateRefused, GateReason: "advise"},
		{Intent: "app", Key: "db", Type: "test_db", Action: Create, Gate: GateRefused, GateReason: "depends on base/net"},
		{Intent: "app", Key: "cfg", Type: "test_net", Action: Create, Gate: GateNeedsApproval},
		{Intent: "cron", Key: "job", Type: "test_net", Action: Create, Gate: GateAuto},
		{Intent: "base", Key: "sg", Type: "test_net", Action: Create, Gate: GateRefused, GateReason: "advise"},
		{Intent: "obs", Key: "x", Type: "test_net", Action: Create, Gate: GateRefused, GateReason: "observe"},
	}}
	var buf bytes.Buffer
	Render(&buf, p, RenderOptions{})
	block := func(header, comment, sym, typ, key string) string {
		out := "  # " + header + "\n"
		if comment != "" {
			out += "  # (" + comment + ")\n"
		}
		return out + "  " + sym + " resource \"" + typ + "\" \"" + key + "\" {\n    }\n\n"
	}
	want := "rules: not checked (no model)\n\n" +
		"Prosa will perform the following actions:\n\n" +
		block("old/net will be destroyed", "needs approval: destroy", "-", "test_net", "net") +
		block("base/net will be created", "not applied: advise", "+", "test_net", "net") +
		block("app/db will be created", "not applied: depends on base/net", "+", "test_db", "db") +
		block("app/cfg will be created", "needs approval", "+", "test_net", "cfg") +
		block("cron/job will be created", "", "+", "test_net", "job") +
		block("base/sg will be created", "not applied: advise", "+", "test_net", "sg") +
		block("obs/x will be created", "not applied: observe", "+", "test_net", "x") +
		"Plan: 2 to add, 0 to change, 1 to destroy.\n" +
		"Not applied:\n" +
		"  base/net: advise\n" +
		"  app/db: depends on base/net\n" +
		"  base/sg: advise\n" +
		"  obs/x: observe\n" +
		"advise: to apply base, set \"autonomy: approve\" in base.md and run: prosa plan intents, then prosa apply intents\n"
	if buf.String() != want {
		t.Fatalf("render =\n%s\nwant\n%s", buf.String(), want)
	}
	buf.Reset()
	RenderApproval(&buf, p, RenderOptions{})
	if want := "Needs approval:\n  - old/net (test_net): destroy\n  + app/cfg (test_net)\n"; buf.String() != want {
		t.Fatalf("approval render = %q, want %q", buf.String(), want)
	}
	if !p.Applicable() {
		t.Fatal("Applicable = false with auto and needs_approval changes")
	}
	refusedOnly := &Plan{Changes: []Change{p.Changes[1], {Intent: "x", Key: "y", Action: NoOp}}}
	if refusedOnly.Applicable() || !refusedOnly.HasChanges() {
		t.Fatalf("refused only: Applicable=%v HasChanges=%v", refusedOnly.Applicable(), refusedOnly.HasChanges())
	}
}

// Engine.Region, the region of the project (ADR-027), wins over
// PROSA_AWS_REGION for the policy and the planner input; Engine.Ignore
// leaves files of the project out of the intents.
func TestEngineRegionAndIgnore(t *testing.T) {
	t.Setenv("PROSA_AWS_REGION", "eu-south-1")
	text := "a bucket in us-east-1"
	rp := &recordingPlanner{desired: map[string]Desired{"app": {Resources: []DesiredResource{
		{Key: "b", Type: "test_regional", Config: map[string]any{"name": "b", "region": "us-east-1"}}}}}}
	e := newTestEnv(t, rp)
	e.engine.Region = "us-east-1"
	e.engine.Ignore = []string{"README.md"}
	e.writeIntent("app", text)
	if err := os.WriteFile(filepath.Join(e.dir, "README.md"), []byte("# Not an intent"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := summary(e.plan()); !equalStrings(got, []string{"create app/b"}) {
		t.Fatalf("changes = %v", got)
	}
	if !equalStrings(rp.order, []string{"app"}) || rp.inputs["app"].Region != "us-east-1" {
		t.Errorf("planner asked for %v, region %q; want app only, in us-east-1", rp.order, rp.inputs["app"].Region)
	}
}

func TestRegionPolicy(t *testing.T) {
	t.Setenv("PROSA_AWS_REGION", "eu-south-1")
	bucket := func(region string) Desired {
		return Desired{Resources: []DesiredResource{{Key: "b", Type: "test_regional", Config: map[string]any{"name": "b", "region": region}}}}
	}
	wantMsg := `region "us-east-1" is not allowed: resources must stay in "eu-south-1" (policy)`

	text := "a bucket in us-east-1"
	e := newTestEnv(t, StaticPlanner{hashOf(text): bucket("us-east-1")})
	e.writeIntent("app", text)
	_, err := e.engine.Plan(context.Background())
	var ve *ValidationError
	if !errors.As(err, &ve) || len(ve.Problems) != 1 || ve.Problems[0].Key != "b" || ve.Problems[0].Message != wantMsg {
		t.Fatalf("err = %v, want one problem %q", err, wantMsg)
	}

	// The configured region and an empty one (which hashicorp/aws plans as
	// the configured region) are fine.
	for _, region := range []string{"eu-south-1", ""} {
		text = "a bucket in " + region + "."
		e = newTestEnv(t, StaticPlanner{hashOf(text): bucket(region)})
		e.writeIntent("app", text)
		if got := summary(e.plan()); !equalStrings(got, []string{"create app/b"}) {
			t.Fatalf("region %q: changes = %v", region, got)
		}
	}

	// The planner gets the problem back from Validate.
	var seen error
	e = newTestEnv(t, plannerFunc(func(ctx context.Context, in PlanInput) (Desired, error) {
		seen = in.Validate(ctx, bucket("us-east-1"))
		return Desired{Resources: []DesiredResource{{Key: "b", Type: "test_regional", Config: map[string]any{"name": "b"}}}}, nil
	}))
	e.writeIntent("app", "a bucket")
	e.plan()
	if !errors.As(seen, &ve) || len(ve.Problems) != 1 || ve.Problems[0].Message != wantMsg {
		t.Fatalf("Validate = %v, want %q", seen, wantMsg)
	}
}
