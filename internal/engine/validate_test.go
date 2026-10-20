package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/zclconf/go-cty/cty"

	"github.com/giammbo/prosa/internal/provider"
)

func TestPlanValidationCollectsAllProblems(t *testing.T) {
	appText, webText := "a big db and a net", "a web net"
	planner := StaticPlanner{
		hashOf(appText): {Resources: []DesiredResource{
			{Key: "net", Type: "test_net", Config: map[string]any{"name": "net", "colour": "red"}},
			{Key: "db", Type: "test_db", Config: map[string]any{"name": "db", "net_id": "${app/net.id}", "size": 500}},
			{Key: "pw", Type: "test_db", Config: map[string]any{"name": "pw", "password": "hunter2"}},
		}},
		hashOf(webText): {Resources: []DesiredResource{
			{Key: "ok", Type: "test_net", Config: map[string]any{"name": "${app/net.name}"}},
		}},
	}
	e := newTestEnv(t, planner)
	e.writeIntent("app", appText)
	e.writeIntent("web", webText)
	// A stored resource: validation must happen before it is refreshed.
	e.seed("old", hashOf("old"), seedRes{key: "n", typ: "test_net", chosen: map[string]any{"name": "n"},
		state: map[string]any{"id": "test_net-9", "name": "n"}})

	_, err := e.engine.Plan(context.Background())
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v (%T), want a *ValidationError", err, err)
	}
	var got []string
	for _, pr := range ve.Problems {
		got = append(got, pr.Intent+"/"+pr.Key+" "+pr.Type)
	}
	if want := []string{"app/db test_db", "app/net test_net", "app/pw test_db"}; !equalStrings(got, want) {
		t.Fatalf("problems = %v, want %v (%v)", got, want, ve.Problems)
	}
	for i, want := range []string{"size must be at most 100", `"colour"`, `attribute "password" of app/pw is sensitive: never write its value; use a native mechanism such as manage_master_user_password or secrets.valueFrom, or reference a secret (ADR-011)`} {
		if !strings.Contains(ve.Problems[i].Message, want) {
			t.Errorf("problem %d = %q, want it to contain %q", i, ve.Problems[i].Message, want)
		}
	}
	lines := strings.Split(err.Error(), "\n")
	if len(lines) != 4 || !strings.Contains(lines[1], "app/db (test_db): ") || !strings.Contains(lines[3], "app/pw (test_db): ") {
		t.Fatalf("Error() = %q, want a header and one line per problem", err.Error())
	}
	for _, c := range e.fake.calls {
		if strings.HasPrefix(c, "read ") || strings.HasPrefix(c, "plan ") {
			t.Fatalf("provider %s before validation failed: %v", c, e.fake.calls)
		}
	}
}

func TestPlanValidatesWithUnknownReferences(t *testing.T) {
	text := "net and db"
	e := newTestEnv(t, StaticPlanner{hashOf(text): appDesired()})
	e.writeIntent("app", text)
	e.plan()
	n := 0
	for _, c := range e.fake.calls {
		if strings.HasPrefix(c, "validate ") {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("validate calls = %d, want 2: %v", n, e.fake.calls)
	}
}

func TestPlanRejectsSensitiveReference(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config map[string]any
		typ    string
		want   string // "" = accepted
	}{
		{"into plain", map[string]any{"name": "${app/src.password}"}, "test_net", `reference ${app/src.password} carries a sensitive value into non-sensitive attribute "name" of app/dst (ADR-011)`},
		{"into a map", map[string]any{"name": "n", "tags": map[string]any{"k": "${app/src.password}"}}, "test_net", `non-sensitive attribute "tags" of app/dst`},
		{"computed sensitive", map[string]any{"name": "${app/tok.value}"}, "test_net", `reference ${app/tok.value} carries`},
		{"nested block", map[string]any{"name": "${app/auth.auth.0.client_secret}"}, "test_net", `${app/auth.auth.0.client_secret}`},
		{"whole block", map[string]any{"name": "${app/auth.auth}"}, "test_net", `${app/auth.auth}`},
		{"into nested plain", map[string]any{"name": "a", "auth": []any{map[string]any{"client_id": "${app/src.password}"}}}, "test_app", `attribute "auth.client_id" of app/dst`},
		{"into sensitive", map[string]any{"name": "d", "password": "${app/src.password}"}, "test_db", ""},
		{"into write-only", map[string]any{"name": "s", "value_wo": "${app/tok.value}"}, "test_secret", ""},
		{"into nested sensitive", map[string]any{"name": "a", "auth": []any{map[string]any{"client_secret": "${app/tok.value}"}}}, "test_app", ""},
		{"plain source", map[string]any{"name": "${app/auth.auth.0.client_id}"}, "test_net", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text := "refs: " + tc.name
			e := newTestEnv(t, StaticPlanner{hashOf(text): {Resources: []DesiredResource{
				{Key: "src", Type: "test_db", Config: map[string]any{"name": "src", "password": nil}},
				{Key: "tok", Type: "test_token", Config: map[string]any{"name": "tok"}},
				{Key: "auth", Type: "test_app", Config: map[string]any{"name": "auth", "auth": []any{map[string]any{"client_id": "c"}}}},
				{Key: "dst", Type: tc.typ, Config: tc.config},
			}}})
			e.writeIntent("app", text)
			_, err := e.engine.Plan(context.Background())
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("err = %v, want none", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestPlanRejectsWriteOnlyLiteral(t *testing.T) {
	text := "a literal write-only secret"
	e := newTestEnv(t, StaticPlanner{hashOf(text): {Resources: []DesiredResource{
		{Key: "s", Type: "test_secret", Config: map[string]any{"name": "s", "value_wo": "hunter2"}},
	}}})
	e.writeIntent("app", text)
	_, err := e.engine.Plan(context.Background())
	if err == nil || !strings.Contains(err.Error(), `attribute "value_wo" of app/s is write-only: never write its value; use a native mechanism such as manage_master_user_password or secrets.valueFrom, or reference a secret (ADR-011)`) {
		t.Fatalf("err = %v", err)
	}
}

func TestRenderMasksWriteOnly(t *testing.T) {
	fake := newFakeProvider()
	obj := func(v string) cty.Value {
		return cty.ObjectVal(map[string]cty.Value{"id": cty.StringVal("s-1"), "name": cty.StringVal("s"), "value_wo": cty.StringVal(v)})
	}
	p := &Plan{schema: fake.schema, Changes: []Change{
		{Intent: "app", Key: "s", Type: "test_secret", Action: Update, Before: obj("old-s3cret"), After: obj("new-s3cret")},
	}}
	var buf bytes.Buffer
	Render(&buf, p, RenderOptions{})
	if strings.Contains(buf.String(), "s3cret") || !strings.Contains(buf.String(), "      ~ value_wo = (sensitive value)\n") {
		t.Fatalf("render = %q", buf.String())
	}
}

func TestSourceSensitive(t *testing.T) {
	inner := &provider.Block{
		Attributes: map[string]*provider.Attribute{
			"secret": {Type: cty.String, Sensitive: true},
			"plain":  {Type: cty.String},
			"wo":     {Type: cty.String, WriteOnly: true},
			"blob":   {Type: cty.Map(cty.String), Sensitive: true},
		},
		BlockTypes: map[string]*provider.NestedBlock{},
	}
	b := &provider.Block{
		Attributes: map[string]*provider.Attribute{"name": {Type: cty.String}},
		BlockTypes: map[string]*provider.NestedBlock{
			"one":  {Nesting: provider.NestingSingle, Block: inner},
			"many": {Nesting: provider.NestingMap, Block: inner},
			"safe": {Nesting: provider.NestingList, Block: &provider.Block{
				Attributes: map[string]*provider.Attribute{"x": {Type: cty.String}},
				BlockTypes: map[string]*provider.NestedBlock{},
			}},
		},
	}
	for path, want := range map[string]bool{
		"name": false, "one.secret": true, "one.plain": false, "one.wo": true, "one.blob.k": true,
		"many.a.secret": true, "many.a.plain": false, "one": true, "many": true, "safe": false, "safe.0.x": false, "nope": false,
	} {
		if got := sourceSensitive(b, strings.Split(path, ".")); got != want {
			t.Errorf("sourceSensitive(%s) = %v, want %v", path, got, want)
		}
	}
}

func TestPlanValidateTransportErrorIsNotAProblem(t *testing.T) {
	text := "net and db"
	e := newTestEnv(t, StaticPlanner{hashOf(text): appDesired()})
	e.writeIntent("app", text)
	dead := errors.New("rpc error: code = Unavailable desc = connection refused")
	e.fake.validateErr = func(string) error { return &provider.TransportError{Op: "validate test_net", Err: dead} }
	_, err := e.engine.Plan(context.Background())
	var ve *ValidationError
	if err == nil || errors.As(err, &ve) {
		t.Fatalf("err = %v (%T), want a plain error, not a ValidationError", err, err)
	}
	if !errors.Is(err, dead) {
		t.Fatalf("err = %v, want it to wrap the transport error", err)
	}
}

func TestPlanUnknownTypeAndBadRefsAreProblems(t *testing.T) {
	text := "a mess"
	d := Desired{Resources: []DesiredResource{
		{Key: "ghost", Type: "test_ghost", Config: map[string]any{"name": "g"}},
		{Key: "a", Type: "test_net", Config: map[string]any{"name": "${app/nokey.id}"}},
		{Key: "b", Type: "test_net", Config: map[string]any{"name": "x-${app/nokey.id}"}},
		{Key: "c", Type: "test_net", Config: map[string]any{"name": "${nointent/x.id}"}},
		{Key: "bad", Type: "test_db", Config: map[string]any{"name": "d", "size": 500}},
		{Key: "multi", Type: "test_net", Config: map[string]any{"name": "${app/nokey.id}", "tags": map[string]any{"t": "${nointent/x.id}"}}},
	}}
	e := newTestEnv(t, StaticPlanner{hashOf(text): d})
	e.writeIntent("app", text)
	_, err := e.engine.Plan(context.Background())
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v (%T), want a *ValidationError", err, err)
	}
	var got []string
	for _, pr := range ve.Problems {
		got = append(got, pr.Intent+"/"+pr.Key)
	}
	if want := []string{"app/a", "app/b", "app/bad", "app/c", "app/ghost", "app/multi", "app/multi"}; !equalStrings(got, want) {
		t.Fatalf("problems = %v, want %v: %v", got, want, ve.Problems)
	}
	for i, want := range []string{"unknown key", `template "x-${app/nokey.id}": reference "${app/nokey.id}": unknown key app/nokey`, "size must be at most 100", "unknown intent", `unknown resource type "test_ghost"`, "unknown key", "unknown intent"} {
		if !strings.Contains(ve.Problems[i].Message, want) {
			t.Errorf("problem %d = %q, want %q", i, ve.Problems[i].Message, want)
		}
	}
	for _, c := range e.fake.calls {
		if c == "validate test_ghost" || strings.HasPrefix(c, "read ") || strings.HasPrefix(c, "plan ") || strings.HasPrefix(c, "apply ") {
			t.Fatalf("unexpected call %q", c)
		}
	}
}

func TestPlanUnknownTypeIsAProblemWithTheOthers(t *testing.T) {
	text := "a mess"
	d := Desired{Resources: []DesiredResource{
		{Key: "ghost", Type: "test_ghost", Config: map[string]any{"name": "g"}},
		{Key: "bad", Type: "test_db", Config: map[string]any{"name": "d", "size": 500}},
	}}
	e := newTestEnv(t, StaticPlanner{hashOf(text): d})
	e.writeIntent("app", text)
	_, err := e.engine.Plan(context.Background())
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v (%T), want a *ValidationError", err, err)
	}
	if len(ve.Problems) != 2 || ve.Problems[0].Key != "bad" || ve.Problems[1].Key != "ghost" ||
		!strings.Contains(ve.Problems[1].Message, `unknown resource type "test_ghost"`) {
		t.Fatalf("problems = %v", ve.Problems)
	}
	for _, c := range e.fake.calls {
		if strings.HasSuffix(c, "test_ghost") || strings.HasPrefix(c, "read ") || strings.HasPrefix(c, "plan ") {
			t.Fatalf("unexpected call %q", c)
		}
	}
}

func TestPlanRejectsOutOfRangeNumber(t *testing.T) {
	text := "a db"
	d := Desired{Resources: []DesiredResource{
		{Key: "db", Type: "test_db", Config: map[string]any{"name": "d", "size": json.Number("123456789012345678901234567890")}},
	}}
	e := newTestEnv(t, StaticPlanner{hashOf(text): d})
	e.writeIntent("app", text)
	_, err := e.engine.Plan(context.Background())
	var ve *ValidationError
	if !errors.As(err, &ve) || len(ve.Problems) != 1 ||
		!strings.Contains(ve.Problems[0].Message, "config.size") || !strings.Contains(ve.Problems[0].Message, "out of range") {
		t.Fatalf("err = %v, want one out of range problem naming config.size", err)
	}
	for _, c := range e.fake.calls {
		if c != "schema" {
			t.Fatalf("unexpected provider call %q", c)
		}
	}
	if _, statErr := os.Stat(e.lockPath); statErr == nil {
		t.Fatal("lock written")
	}
}

func TestPlanDependencyCycleIsAProblem(t *testing.T) {
	text := "cycle"
	d := Desired{Resources: []DesiredResource{
		{Key: "a", Type: "test_net", Config: map[string]any{"name": "${app/b.name}"}},
		{Key: "b", Type: "test_net", Config: map[string]any{"name": "${app/a.name}"}},
		{Key: "c", Type: "test_net", Config: map[string]any{"name": "ok"}},
	}}
	e := newTestEnv(t, StaticPlanner{hashOf(text): d})
	e.writeIntent("app", text)
	_, err := e.engine.Plan(context.Background())
	var ve *ValidationError
	if !errors.As(err, &ve) || len(ve.Problems) != 2 || ve.Problems[0].Key != "a" || ve.Problems[1].Key != "b" ||
		!strings.Contains(ve.Problems[0].Message, "dependency cycle") {
		t.Fatalf("err = %v, want cycle problems for a and b", err)
	}
}

func TestPlanRejectsSensitiveReferenceInJSONDocument(t *testing.T) {
	text := "token in a policy"
	d := Desired{Resources: []DesiredResource{
		{Key: "tok", Type: "test_token", Config: map[string]any{"name": "t"}},
		{Key: "net", Type: "test_net", Config: map[string]any{"name": "${app/tok.value}"}},
		{Key: "db", Type: "test_db", Config: map[string]any{"name": "d", "net_id": map[string]any{"a": []any{"${app/tok.value}"}}}},
	}}
	e := newTestEnv(t, StaticPlanner{hashOf(text): d})
	e.writeIntent("app", text)
	_, err := e.engine.Plan(context.Background())
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want a ValidationError", err)
	}
	found := false
	for _, pr := range ve.Problems {
		if pr.Key == "db" && strings.Contains(pr.Message, "ADR-011") && strings.Contains(pr.Message, `"net_id"`) {
			found = true
		}
	}
	if !found {
		t.Fatalf("no ADR-011 problem for the document in db.net_id: %v", ve.Problems)
	}
}

func TestValidateRejectsTypeChange(t *testing.T) {
	rp := &recordingPlanner{desired: map[string]Desired{"app": appDesired()}}
	e := newTestEnv(t, rp)
	e.writeIntent("app", "app v1")
	e.mustApply(e.plan())

	var probs []ValidationProblem
	rp.check = func(ctx context.Context, in PlanInput) {
		err := in.Validate(ctx, Desired{Resources: []DesiredResource{
			{Key: "db", Type: "test_net", Config: map[string]any{"name": "db"}},
			{Key: "net", Type: "test_net", Config: map[string]any{"name": "net"}},
		}})
		var ve *ValidationError
		if !errors.As(err, &ve) {
			t.Fatalf("Validate = %v, want a *ValidationError", err)
		}
		probs = ve.Problems
	}
	e.writeIntent("app", "app v2")
	e.plan()
	if len(probs) != 1 || probs[0].Key != "db" || probs[0].Message != `key "db" was test_db, cannot become test_net: use a new key` {
		t.Errorf("problems = %+v, want one on db asking for a new key", probs)
	}
}

func TestPlanChecksReferencesAgainstTargetSchemas(t *testing.T) {
	text := "references of every shape"
	d := Desired{Resources: []DesiredResource{
		{Key: "net", Type: "test_net", Config: map[string]any{"name": "net"}},
		{Key: "list", Type: "test_list", Config: map[string]any{"name": "l", "items": []any{"a"}, "groups": map[string]any{"g": []any{"x", "y"}}}},
		{Key: "auth", Type: "test_app", Config: map[string]any{"name": "a", "auth": []any{map[string]any{"client_id": "c"}}}},
		{Key: "lb", Type: "test_lb", Config: map[string]any{"name": "lb", "opts": map[string]any{"inner": []any{map[string]any{"flag": true}}}}},
		{Key: "a", Type: "test_net", Config: map[string]any{"name": "${app/net.nope}", "colour": "red"}},
		{Key: "b", Type: "test_net", Config: map[string]any{"name": "${app/net.name.0}"}},
		{Key: "c", Type: "test_net", Config: map[string]any{"name": "${app/list.items}"}},
		{Key: "d", Type: "test_net", Config: map[string]any{"name": "${app/list.items.0}", "tags": map[string]any{"k": "${app/list.groups.g.1}"}}},
		{Key: "e", Type: "test_net", Config: map[string]any{"name": "${app/list.items.first}"}},
		{Key: "f", Type: "test_net", Config: map[string]any{"name": "${app/auth.auth.client_id}"}},
		{Key: "g", Type: "test_net", Config: map[string]any{"name": "${app/lb.opts.inner.0.flag}"}},
		{Key: "h", Type: "test_db", Config: map[string]any{"name": "${app/auth.auth.0.client_id}", "size": "${app/net.id}"}},
	}}
	e := newTestEnv(t, StaticPlanner{hashOf(text): d})
	e.writeIntent("app", text)
	_, err := e.engine.Plan(context.Background())
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want a *ValidationError", err)
	}
	var got []string
	for _, pr := range ve.Problems {
		got = append(got, pr.Key+": "+pr.Message)
	}
	want := []string{
		`a: reference ${app/net.nope}: app/net (test_net) has no attribute "nope"`,
		`a: unknown attribute or block "colour"`,
		`b: reference ${app/net.name.0}: cannot select "0" in name, a string`,
		`c: attribute "name": cannot use list of string as string`,
		`e: reference ${app/list.items.first}: cannot select "first" in items, a list: use an index, as in items.0`,
		`f: reference ${app/auth.auth.client_id}: cannot select "client_id" in auth, a list: use an index, as in auth.0.client_id`,
		`g: reference ${app/lb.opts.inner.0.flag}: cannot select "0" in opts.inner, a set: its elements have no index`,
	}
	if !equalStrings(got, want) {
		t.Errorf("problems =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestPlanTagsOfWrongKind(t *testing.T) {
	text := "tags as a list"
	e := newTestEnv(t, StaticPlanner{hashOf(text): {Resources: []DesiredResource{
		{Key: "n", Type: "test_net", Config: map[string]any{"name": "n", "tags": []any{"a"}}},
	}}})
	e.writeIntent("app", text)
	_, err := e.engine.Plan(context.Background())
	var ve *ValidationError
	if !errors.As(err, &ve) || len(ve.Problems) != 1 || ve.Problems[0].Message != "tags: expected an object, got list" {
		t.Fatalf("err = %v, want one problem naming the JSON kinds", err)
	}
}
