package engine

import (
	"bytes"
	"strings"
	"testing"

	"github.com/zclconf/go-cty/cty"
)

// renderPlan renders p without color.
func renderPlan(p *Plan) string {
	var buf bytes.Buffer
	Render(&buf, p, RenderOptions{})
	return buf.String()
}

func checkRender(t *testing.T, got, want string) {
	t.Helper()
	if got != want {
		t.Fatalf("render =\n%s\nwant\n%s\n(got %q)", got, want, got)
	}
}

func strMap(kv ...string) cty.Value {
	m := map[string]cty.Value{}
	for i := 0; i < len(kv); i += 2 {
		m[kv[i]] = cty.StringVal(kv[i+1])
	}
	return cty.MapVal(m)
}

// dbObj is a test_db object; nil values are null.
func dbObj(id cty.Value, name string, size int64, tags cty.Value) cty.Value {
	if tags == cty.NilVal {
		tags = cty.NullVal(cty.Map(cty.String))
	}
	return cty.ObjectVal(map[string]cty.Value{
		"id": id, "name": cty.StringVal(name), "net_id": cty.NullVal(cty.String),
		"size": cty.NumberIntVal(size), "password": cty.NullVal(cty.String), "tags": tags,
	})
}

func TestRenderCreate(t *testing.T) {
	fake := newFakeProvider()
	p := &Plan{schema: fake.schema, Changes: []Change{{
		Intent: "app", Key: "db", Type: "test_db", Action: Create, Gate: GateAuto,
		Before: cty.NullVal(fake.schema.Resources["test_db"].Block.ImpliedType()),
		After:  dbObj(cty.UnknownVal(cty.String), "db", 20, strMap("env", "staging", "prosa/intent", "app")),
	}}}
	checkRender(t, renderPlan(p), `Prosa will perform the following actions:

  # app/db will be created
  + resource "test_db" "db" {
      + id   = (known after apply)
      + name = "db"
      + size = 20
      + tags = {
          + "env"          = "staging"
          + "prosa/intent" = "app"
        }
    }

Plan: 1 to add, 0 to change, 0 to destroy.
`)
}

func TestRenderCreateNestedBlocksAndLists(t *testing.T) {
	fake := newFakeProvider()
	path := func(s string) cty.Value { return cty.ObjectVal(map[string]cty.Value{"path": cty.StringVal(s)}) }
	lb := cty.ObjectVal(map[string]cty.Value{
		"id": cty.UnknownVal(cty.String), "name": cty.StringVal("lb"),
		"health": cty.ListVal([]cty.Value{path("/h")}),
		"rule":   cty.ListVal([]cty.Value{path("/a"), path("/b")}),
		"opts": cty.ObjectVal(map[string]cty.Value{
			"inner": cty.SetVal([]cty.Value{cty.ObjectVal(map[string]cty.Value{"flag": cty.True})}),
		}),
	})
	list := cty.ObjectVal(map[string]cty.Value{
		"id": cty.StringVal("l-1"), "name": cty.StringVal("l"),
		"items":  cty.ListVal([]cty.Value{cty.StringVal("a"), cty.StringVal("b")}),
		"groups": cty.MapVal(map[string]cty.Value{"x": cty.ListVal([]cty.Value{cty.StringVal("1")})}),
	})
	p := &Plan{schema: fake.schema, Changes: []Change{
		{Intent: "web", Key: "lb", Type: "test_lb", Action: Create, After: lb},
		{Intent: "web", Key: "l", Type: "test_list", Action: Create, After: list},
	}}
	checkRender(t, renderPlan(p), `Prosa will perform the following actions:

  # web/lb will be created
  + resource "test_lb" "lb" {
      + id   = (known after apply)
      + name = "lb"

      + health {
          + path = "/h"
        }

      + opts {
          + inner {
              + flag = true
            }
        }

      + rule {
          + path = "/a"
        }
      + rule {
          + path = "/b"
        }
    }

  # web/l will be created
  + resource "test_list" "l" {
      + groups = {
          + "x" = ["1"]
        }
      + id     = "l-1"
      + items  = [
          + "a",
          + "b",
        ]
      + name   = "l"
    }

Plan: 2 to add, 0 to change, 0 to destroy.
`)
}

func TestRenderUpdateWithDriftAndGate(t *testing.T) {
	fake := newFakeProvider()
	tags := strMap("prosa/intent", "app")
	p := &Plan{schema: fake.schema, Changes: []Change{{
		Intent: "app", Key: "db", Type: "test_db", Action: Update, Gate: GateNeedsApproval,
		Drifted: true, DriftAttrs: []string{"size"},
		Before: dbObj(cty.StringVal("test_db-1"), "db", 30, tags),
		After:  dbObj(cty.StringVal("test_db-1"), "db", 20, tags),
	}}}
	checkRender(t, renderPlan(p), `Prosa will perform the following actions:

  # app/db will be updated in-place
  # (needs approval)
  # (drifted outside Prosa: size)
  ~ resource "test_db" "db" {
        id   = "test_db-1"
      ~ size = 30 -> 20
        # (2 unchanged attributes hidden)
    }

Plan: 0 to add, 1 to change, 0 to destroy.
`)
}

func TestRenderUpdateMapsListsAndNull(t *testing.T) {
	fake := newFakeProvider()
	before := cty.ObjectVal(map[string]cty.Value{
		"id": cty.StringVal("l-1"), "name": cty.StringVal("l"),
		"items":  cty.ListVal([]cty.Value{cty.StringVal("a"), cty.StringVal("b")}),
		"groups": cty.MapVal(map[string]cty.Value{"x": cty.ListVal([]cty.Value{cty.StringVal("1")})}),
	})
	after := cty.ObjectVal(map[string]cty.Value{
		"id": cty.StringVal("l-1"), "name": cty.NullVal(cty.String),
		"items":  cty.ListVal([]cty.Value{cty.StringVal("a"), cty.StringVal("c")}),
		"groups": cty.MapVal(map[string]cty.Value{"x": cty.ListVal([]cty.Value{cty.StringVal("1")}), "y": cty.ListVal([]cty.Value{cty.StringVal("2")})}),
	})
	tagsBefore, tagsAfter := strMap("env", "a", "prosa/intent", "app"), strMap("env", "b", "prosa/intent", "app", "team", "x")
	p := &Plan{schema: fake.schema, Changes: []Change{
		{Intent: "app", Key: "l", Type: "test_list", Action: Update, Before: before, After: after},
		{Intent: "app", Key: "db", Type: "test_db", Action: Update,
			Before: dbObj(cty.StringVal("test_db-1"), "db", 10, tagsBefore), After: dbObj(cty.StringVal("test_db-1"), "db", 10, tagsAfter)},
	}}
	checkRender(t, renderPlan(p), `Prosa will perform the following actions:

  # app/l will be updated in-place
  ~ resource "test_list" "l" {
        id     = "l-1"
      ~ groups = {
          + "y" = ["2"]
            # (1 unchanged element hidden)
        }
      ~ items  = [
          - "b",
          + "c",
            # (1 unchanged element hidden)
        ]
      - name   = "l" -> null
    }

  # app/db will be updated in-place
  ~ resource "test_db" "db" {
        id   = "test_db-1"
      ~ tags = {
          ~ "env"  = "a" -> "b"
          + "team" = "x"
            # (1 unchanged element hidden)
        }
        # (2 unchanged attributes hidden)
    }

Plan: 0 to add, 2 to change, 0 to destroy.
`)
}

func TestRenderReplace(t *testing.T) {
	fake := newFakeProvider()
	net := func(id cty.Value, name string) cty.Value {
		return cty.ObjectVal(map[string]cty.Value{"id": id, "name": cty.StringVal(name), "tags": strMap("prosa/intent", "app")})
	}
	p := &Plan{schema: fake.schema, Changes: []Change{{
		Intent: "app", Key: "net", Type: "test_net", Action: Replace, Gate: GateNeedsApproval, GateReason: "destroy",
		RequiresReplace: []string{"name"},
		Before:          net(cty.StringVal("test_net-1"), "net"), After: net(cty.UnknownVal(cty.String), "net2"),
	}}}
	checkRender(t, renderPlan(p), `Prosa will perform the following actions:

  # app/net must be replaced
  # (needs approval: destroy)
-/+ resource "test_net" "net" {
      ~ id   = "test_net-1" -> (known after apply)
      ~ name = "net" -> "net2" # forces replacement
        # (1 unchanged attribute hidden)
    }

Plan: 1 to add, 0 to change, 1 to destroy.
`)
}

func TestRenderDestroy(t *testing.T) {
	fake := newFakeProvider()
	before := dbObj(cty.StringVal("test_db-1"), "db", 10, strMap("prosa/intent", "old"))
	gone := cty.NullVal(fake.schema.Resources["test_net"].Block.ImpliedType())
	p := &Plan{schema: fake.schema, Changes: []Change{
		{Intent: "old", Key: "db", Type: "test_db", Action: Delete, Gate: GateNeedsApproval, GateReason: "destroy",
			Before: before, After: cty.NullVal(before.Type())},
		{Intent: "old", Key: "net", Type: "test_net", Action: Delete, Gate: GateAuto, Drifted: true, Before: gone, After: gone},
	}}
	checkRender(t, renderPlan(p), `Prosa will perform the following actions:

  # old/db will be destroyed
  # (needs approval: destroy)
  - resource "test_db" "db" {
      - id   = "test_db-1" -> null
      - name = "db" -> null
        # (2 unchanged attributes hidden)
    }

  # old/net will be destroyed
  # (drifted outside Prosa: the object is gone)
  - resource "test_net" "net" {
    }

Plan: 0 to add, 0 to change, 2 to destroy.
`)
}

func TestRenderNestedBlockUpdateMasksSensitive(t *testing.T) {
	fake := newFakeProvider()
	app := func(secret string) cty.Value {
		return cty.ObjectVal(map[string]cty.Value{
			"id": cty.StringVal("test_app-1"), "name": cty.StringVal("app"),
			"auth": cty.ListVal([]cty.Value{cty.ObjectVal(map[string]cty.Value{
				"client_id": cty.StringVal("cid"), "client_secret": cty.StringVal(secret),
			})}),
		})
	}
	p := &Plan{schema: fake.schema, Changes: []Change{
		{Intent: "app", Key: "app", Type: "test_app", Action: Update, Before: app("old-s3cret"), After: app("new-s3cret")},
	}}
	out := renderPlan(p)
	if strings.Contains(out, "s3cret") {
		t.Fatalf("render leaks a nested sensitive value: %q", out)
	}
	checkRender(t, out, `Prosa will perform the following actions:

  # app/app will be updated in-place
  ~ resource "test_app" "app" {
        id = "test_app-1"
        # (1 unchanged attribute hidden)

      ~ auth {
          ~ client_secret = (sensitive value)
            # (1 unchanged attribute hidden)
        }
    }

Plan: 0 to add, 1 to change, 0 to destroy.
`)

	// On create too, at any depth, and write-only like sensitive.
	secret := cty.ObjectVal(map[string]cty.Value{"id": cty.UnknownVal(cty.String), "name": cty.StringVal("s"), "value_wo": cty.StringVal("wo-s3cret")})
	p = &Plan{schema: fake.schema, Changes: []Change{
		{Intent: "app", Key: "app", Type: "test_app", Action: Create, After: app("s3cret")},
		{Intent: "app", Key: "s", Type: "test_secret", Action: Create, After: secret},
	}}
	out = renderPlan(p)
	if strings.Contains(out, "s3cret") {
		t.Fatalf("render leaks a sensitive value: %q", out)
	}
	for _, want := range []string{"          + client_secret = (sensitive value)\n", "      + value_wo = (sensitive value)\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("render lacks %q:\n%s", want, out)
		}
	}
}

func TestRenderNestedBlockHiddenAndAdded(t *testing.T) {
	fake := newFakeProvider()
	path := func(s string) cty.Value { return cty.ObjectVal(map[string]cty.Value{"path": cty.StringVal(s)}) }
	lb := func(name string, rules ...cty.Value) cty.Value {
		return cty.ObjectVal(map[string]cty.Value{
			"id": cty.StringVal("lb-1"), "name": cty.StringVal(name),
			"health": cty.ListVal([]cty.Value{path("/h")}),
			"rule":   cty.ListVal(rules),
			"opts":   cty.NullVal(fake.schema.Resources["test_lb"].Block.BlockTypes["opts"].Block.ImpliedType()),
		})
	}
	p := &Plan{schema: fake.schema, Changes: []Change{
		{Intent: "web", Key: "lb", Type: "test_lb", Action: Update, Before: lb("a", path("/a")), After: lb("b", path("/a"), path("/b"))},
	}}
	checkRender(t, renderPlan(p), `Prosa will perform the following actions:

  # web/lb will be updated in-place
  ~ resource "test_lb" "lb" {
        id   = "lb-1"
      ~ name = "a" -> "b"

      + rule {
          + path = "/b"
        }
        # (2 unchanged blocks hidden)
    }

Plan: 0 to add, 1 to change, 0 to destroy.
`)
}

func TestRenderGatesSummaryAndSections(t *testing.T) {
	p := &Plan{dir: "intents", Changes: []Change{
		{Intent: "old", Key: "net", Type: "test_net", Action: Delete, Gate: GateNeedsApproval, GateReason: "destroy"},
		{Intent: "base", Key: "net", Type: "test_net", Action: Create, Gate: GateRefused, GateReason: "advise"},
		{Intent: "app", Key: "cfg", Type: "test_net", Action: Create, Gate: GateNeedsApproval},
		{Intent: "cron", Key: "job", Type: "test_net", Action: Create, Gate: GateAuto},
		{Intent: "same", Key: "x", Type: "test_net", Action: NoOp},
	}}
	checkRender(t, renderPlan(p), `Prosa will perform the following actions:

  # old/net will be destroyed
  # (needs approval: destroy)
  - resource "test_net" "net" {
    }

  # base/net will be created
  # (not applied: advise)
  + resource "test_net" "net" {
    }

  # app/cfg will be created
  # (needs approval)
  + resource "test_net" "cfg" {
    }

  # cron/job will be created
  + resource "test_net" "job" {
    }

Plan: 2 to add, 0 to change, 1 to destroy.
Not applied:
  base/net: advise
advise: to apply base, set "autonomy: approve" in base.md and run: prosa plan intents, then prosa apply intents
`)
}

func TestRenderNoChangesAndBlocked(t *testing.T) {
	checkRender(t, renderPlan(&Plan{}), "No changes. Your infrastructure matches the intents.\n")
	checkRender(t, renderPlan(&Plan{Blocked: "state digest mismatch"}), "Blocked: state digest mismatch\n")
	checkRender(t, renderPlan(&Plan{Blockers: []string{"app: a", "app: b"}}), "Blocked:\n  app: a\n  app: b\n")
	p := &Plan{Blockers: []string{"app: a"}, Changes: []Change{{Intent: "web", Key: "net", Type: "test_net", Action: Create}}}
	checkRender(t, renderPlan(p), `Blocked:
  app: a

Prosa will perform the following actions:

  # web/net will be created
  + resource "test_net" "net" {
    }

Plan: 1 to add, 0 to change, 0 to destroy.
`)
}

func TestRenderColor(t *testing.T) {
	fake := newFakeProvider()
	net := func(id cty.Value, name string) cty.Value {
		return cty.ObjectVal(map[string]cty.Value{"id": id, "name": cty.StringVal(name), "tags": cty.NullVal(cty.Map(cty.String))})
	}
	p := &Plan{schema: fake.schema, Changes: []Change{
		{Intent: "app", Key: "a", Type: "test_net", Action: Create, After: net(cty.UnknownVal(cty.String), "a")},
		{Intent: "app", Key: "b", Type: "test_net", Action: Update, Before: net(cty.StringVal("b-1"), "b"), After: net(cty.StringVal("b-1"), "b2")},
		{Intent: "app", Key: "c", Type: "test_net", Action: Replace, RequiresReplace: []string{"name"},
			Before: net(cty.StringVal("c-1"), "c"), After: net(cty.StringVal("c-1"), "c2")},
		{Intent: "app", Key: "d", Type: "test_net", Action: Delete, Before: net(cty.StringVal("d-1"), "d")},
	}}
	var buf bytes.Buffer
	Render(&buf, p, RenderOptions{Color: true})
	const (
		b, d, r, g, y, z = "\x1b[1m", "\x1b[2m", "\x1b[31m", "\x1b[32m", "\x1b[33m", "\x1b[0m"
	)
	want := "Prosa will perform the following actions:\n\n" +
		"  " + b + "# app/a will be created" + z + "\n" +
		"  " + g + "+" + z + " " + b + `resource "test_net" "a" {` + z + "\n" +
		"      " + g + "+" + z + " id   = " + d + "(known after apply)" + z + "\n" +
		"      " + g + "+" + z + ` name = "a"` + "\n" +
		"    }\n\n" +
		"  " + b + "# app/b will be updated in-place" + z + "\n" +
		"  " + y + "~" + z + " " + b + `resource "test_net" "b" {` + z + "\n" +
		`        id   = "b-1"` + "\n" +
		"      " + y + "~" + z + ` name = "b" -> "b2"` + "\n" +
		"    }\n\n" +
		"  " + b + "# app/c must be replaced" + z + "\n" +
		r + "-" + z + "/" + g + "+" + z + " " + b + `resource "test_net" "c" {` + z + "\n" +
		`        id   = "c-1"` + "\n" +
		"      " + y + "~" + z + ` name = "c" -> "c2" ` + r + "# forces replacement" + z + "\n" +
		"    }\n\n" +
		"  " + b + "# app/d will be destroyed" + z + "\n" +
		"  " + r + "-" + z + " " + b + `resource "test_net" "d" {` + z + "\n" +
		"      " + r + "-" + z + ` id   = "d-1" -> null` + "\n" +
		"      " + r + "-" + z + ` name = "d" -> null` + "\n" +
		"    }\n\n" +
		b + "Plan:" + z + " 2 to add, 1 to change, 2 to destroy.\n"
	checkRender(t, buf.String(), want)

	buf.Reset()
	Render(&buf, p, RenderOptions{})
	if strings.Contains(buf.String(), "\x1b[") {
		t.Fatalf("render without color has escape codes: %q", buf.String())
	}
}

func TestRenderIsByteStable(t *testing.T) {
	fake := newFakeProvider()
	p := &Plan{schema: fake.schema, Changes: []Change{{
		Intent: "app", Key: "db", Type: "test_db", Action: Create,
		After: dbObj(cty.UnknownVal(cty.String), "db", 20, strMap("b", "2", "a", "1", "c", "3")),
	}}}
	first := renderPlan(p)
	for i := 0; i < 20; i++ {
		if got := renderPlan(p); got != first {
			t.Fatalf("render %d differs:\n%s\nvs\n%s", i, got, first)
		}
	}
}

func TestRenderApproval(t *testing.T) {
	p := &Plan{Changes: []Change{
		{Intent: "old", Key: "net", Type: "test_net", Action: Delete, Gate: GateNeedsApproval, GateReason: "destroy"},
		{Intent: "app", Key: "cfg", Type: "test_net", Action: Create, Gate: GateNeedsApproval},
	}}
	var buf bytes.Buffer
	RenderApproval(&buf, p, RenderOptions{})
	checkRender(t, buf.String(), "Needs approval:\n  - old/net (test_net): destroy\n  + app/cfg (test_net)\n")
	buf.Reset()
	RenderApproval(&buf, p, RenderOptions{Color: true})
	checkRender(t, buf.String(), "Needs approval:\n  \x1b[31m-\x1b[0m old/net (test_net): destroy\n  \x1b[32m+\x1b[0m app/cfg (test_net)\n")
}

func TestRenderFailsClosedWithoutSchema(t *testing.T) {
	fake := newFakeProvider()
	tags := strMap("team", "s3cret-team")
	before, after := dbObj(cty.StringVal("s3cret-id"), "s3cret-a", 10, tags), dbObj(cty.StringVal("s3cret-id"), "s3cret-b", 20, tags)
	changes := []Change{
		{Intent: "app", Key: "new", Type: "test_db", Action: Create, After: after},
		{Intent: "app", Key: "db", Type: "test_db", Action: Update, Before: before, After: after},
		{Intent: "app", Key: "old", Type: "test_db", Action: Delete, Before: before, After: cty.NullVal(before.Type())},
	}
	want := `Prosa will perform the following actions:

  # app/new will be created
  + resource "test_db" "new" {
      + id   = (sensitive value)
      + name = (sensitive value)
      + size = (sensitive value)
      + tags = (sensitive value)
    }

  # app/db will be updated in-place
  ~ resource "test_db" "db" {
        id   = (sensitive value)
      ~ name = (sensitive value)
      ~ size = (sensitive value)
        # (1 unchanged attribute hidden)
    }

  # app/old will be destroyed
  - resource "test_db" "old" {
      - id   = (sensitive value) -> null
      - name = (sensitive value) -> null
        # (2 unchanged attributes hidden)
    }

Plan: 1 to add, 1 to change, 1 to destroy.
`
	// No schema at all, and a schema without the resource type.
	delete(fake.schema.Resources, "test_db")
	for _, p := range []*Plan{{Changes: changes}, {schema: fake.schema, Changes: changes}} {
		out := renderPlan(p)
		if strings.Contains(out, "s3cret") {
			t.Fatalf("render without the type's schema leaks a value:\n%s", out)
		}
		checkRender(t, out, want)
	}
}
