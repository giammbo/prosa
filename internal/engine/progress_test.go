package engine

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/zclconf/go-cty/cty"

	"github.com/giammbo/prosa/internal/provider"
)

// recordProgress makes e's engine record each OpProgress as
// "<phase> <action> <intent>/<key> id=<id>[ err]".
func (e *testEnv) recordProgress() *[]string {
	var got []string
	e.engine.Progress = func(ev OpProgress) {
		s := fmt.Sprintf("%s %s %s/%s id=%s", ev.Phase, ev.Action, ev.Intent, ev.Key, ev.ID)
		if ev.Err != nil {
			s += " err=" + ev.Err.Error()
		}
		got = append(got, s)
	}
	return &got
}

func TestProgressCreateAndFailure(t *testing.T) {
	text := "app with a net and a db"
	e := newTestEnv(t, StaticPlanner{hashOf(text): appDesired()})
	e.writeIntent("app", text)
	e.fake.applyErr = func(typeName string, _ cty.Value) error {
		if typeName == "test_db" {
			return errors.New("db quota exceeded")
		}
		return nil
	}
	got := e.recordProgress()
	if err := e.apply(e.plan(), approveAll); err == nil {
		t.Fatal("Apply succeeded, want the db failure")
	}
	want := []string{
		"start create app/net id=",
		"done create app/net id=test_net-1",
		"start create app/db id=",
		"failed create app/db id= err=app/db: db quota exceeded",
	}
	if !equalStrings(*got, want) {
		t.Fatalf("progress =\n%s\nwant\n%s", strings.Join(*got, "\n"), strings.Join(want, "\n"))
	}
}

func TestProgressReplaceUpdateAndDelete(t *testing.T) {
	text := "renamed net, bigger db"
	d := Desired{Resources: []DesiredResource{
		{Key: "net", Type: "test_net", Config: map[string]any{"name": "net2"}},
		{Key: "db", Type: "test_db", Config: map[string]any{"name": "db", "size": 20}},
	}}
	e := newTestEnv(t, StaticPlanner{hashOf(text): d})
	e.fake.replaceOn["test_net"] = []string{"name"}
	e.fake.nextID["test_net"] = 1
	e.writeIntent("app", text)
	e.seed("app", hashOf("old"),
		seedRes{key: "net", typ: "test_net", chosen: map[string]any{"name": "net"},
			state: map[string]any{"id": "test_net-1", "name": "net", "tags": map[string]any{"prosa/intent": "app"}}},
		seedRes{key: "db", typ: "test_db", chosen: map[string]any{"name": "db", "size": 10},
			state: map[string]any{"id": "test_db-1", "name": "db", "size": 10, "tags": map[string]any{"prosa/intent": "app"}}},
	)
	e.seed("old", hashOf("old text"), seedRes{key: "x", typ: "test_net", chosen: map[string]any{"name": "x"},
		state: map[string]any{"id": "test_net-9", "name": "x", "tags": map[string]any{"prosa/intent": "old"}}})
	got := e.recordProgress()
	e.mustApply(e.plan())
	want := []string{
		"start update app/db id=test_db-1",
		"done update app/db id=test_db-1",
		"start delete app/net id=test_net-1",
		"done delete app/net id=",
		"start create app/net id=",
		"done create app/net id=test_net-2",
		"start delete old/x id=test_net-9",
		"done delete old/x id=",
	}
	if !equalStrings(*got, want) {
		t.Fatalf("progress =\n%s\nwant\n%s", strings.Join(*got, "\n"), strings.Join(want, "\n"))
	}
}

func TestProgressSkippedOperationIsSilent(t *testing.T) {
	text := "renamed net"
	d := Desired{Resources: []DesiredResource{{Key: "net", Type: "test_net", Config: map[string]any{"name": "net2"}}}}
	e := newTestEnv(t, StaticPlanner{hashOf(text): d})
	e.writeIntent("app", text)
	e.seed("app", hashOf("net"), seedRes{key: "net", typ: "test_net", chosen: map[string]any{"name": "net"},
		state: map[string]any{"id": "test_net-1", "name": "net", "tags": map[string]any{"prosa/intent": "app"}}})
	p := e.plan()
	e.fake.ignoreChanges["test_net"] = []string{"name"} // the re-plan finds nothing to do
	got := e.recordProgress()
	e.mustApply(p)
	if len(*got) != 0 {
		t.Fatalf("progress = %v, want nothing for a skipped operation", *got)
	}
}

func TestProgressMasksSensitiveID(t *testing.T) {
	text := "a net"
	e := newTestEnv(t, StaticPlanner{hashOf(text): {Resources: []DesiredResource{{Key: "net", Type: "test_net", Config: map[string]any{"name": "net"}}}}})
	b := e.fake.schema.Resources["test_net"].Block
	b.Attributes["id"] = &provider.Attribute{Type: cty.String, Computed: true, Sensitive: true}
	e.writeIntent("app", text)
	got := e.recordProgress()
	e.mustApply(e.plan())
	want := []string{"start create app/net id=", "done create app/net id=(sensitive value)"}
	if !equalStrings(*got, want) {
		t.Fatalf("progress = %v, want %v", *got, want)
	}
}
