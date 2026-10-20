package lock

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func patchBase() ([]Resource, []Check) {
	res := []Resource{
		{Key: "db.instance", Provider: "hashicorp/aws", Type: "aws_db_instance", ID: "i-1", Chosen: map[string]any{
			"engine": "postgres", "allocated_storage": 20, "storage_encrypted": true,
			"tags": map[string]any{"team": "platform"},
		}},
		{Key: "db.security_group", Provider: "hashicorp/aws", Type: "aws_security_group", ID: "sg-1", Chosen: map[string]any{
			"name_prefix": "main-db-",
			"ingress": []any{
				map[string]any{"from_port": 5432, "to_port": 5432},
				map[string]any{"from_port": 80, "to_port": 80},
			},
		}},
		{Key: "db.subnet_group", Provider: "hashicorp/aws", Type: "aws_db_subnet_group", ID: "sn-1",
			Chosen: map[string]any{"name_prefix": "main-db-subnets-"}, DependsOn: []string{"net/vpc"}},
	}
	checks := []Check{{Name: "c1", Type: "resource_attr", Resource: "database/db.instance", Path: "storage_encrypted", Op: "eq", Value: true}}
	return res, checks
}

func byKey(rs []Resource, k string) *Resource {
	for i := range rs {
		if rs[i].Key == k {
			return &rs[i]
		}
	}
	return nil
}

func TestApplyPatchOps(t *testing.T) {
	res, checks := patchBase()
	ops := []PatchOp{
		{Op: "set", Key: "db.instance", Path: "tags.env", Value: "staging"},
		{Op: "set", Key: "db.instance", Path: "allocated_storage", Value: 40},
		{Op: "set", Key: "db.security_group", Path: "ingress.0.from_port", Value: 6432},
		{Op: "unset", Key: "db.instance", Path: "storage_encrypted"},
		{Op: "add", Key: "db.replica", Type: "aws_db_instance", Config: map[string]any{"engine": "postgres"}, DependsOn: []string{"database/db.instance"}},
		{Op: "remove", Key: "db.subnet_group"},
		{Op: "depends_on", Key: "db.security_group", Value: []any{"net/vpc", "net/sg"}},
		{Op: "checks", Value: []any{map[string]any{"name": "n", "type": "tls_days", "host": "x.example", "min_days": 20}}},
	}
	gotR, gotC, err := ApplyPatch(res, checks, ops)
	if err != nil {
		t.Fatal(err)
	}
	in := byKey(gotR, "db.instance")
	if in.Chosen["allocated_storage"] != 40 || in.Chosen["tags"].(map[string]any)["env"] != "staging" ||
		in.Chosen["tags"].(map[string]any)["team"] != "platform" {
		t.Errorf("set: %#v", in.Chosen)
	}
	if _, ok := in.Chosen["storage_encrypted"]; ok {
		t.Errorf("unset: %#v", in.Chosen)
	}
	sg := byKey(gotR, "db.security_group")
	ing := sg.Chosen["ingress"].([]any)
	if ing[0].(map[string]any)["from_port"] != 6432 || ing[1].(map[string]any)["from_port"] != 80 {
		t.Errorf("indexed set: %#v", ing)
	}
	if !reflect.DeepEqual(sg.DependsOn, []string{"net/vpc", "net/sg"}) {
		t.Errorf("depends_on: %v", sg.DependsOn)
	}
	if byKey(gotR, "db.subnet_group") != nil {
		t.Errorf("remove: still there")
	}
	rep := byKey(gotR, "db.replica")
	if rep == nil || rep.Type != "aws_db_instance" || rep.Provider != "hashicorp/aws" || rep.Chosen["engine"] != "postgres" ||
		!reflect.DeepEqual(rep.DependsOn, []string{"database/db.instance"}) {
		t.Errorf("add: %+v", rep)
	}
	if len(gotC) != 1 || gotC[0].Type != "tls_days" || gotC[0].Host != "x.example" || gotC[0].MinDays != 20 {
		t.Errorf("checks: %+v", gotC)
	}
	// the arguments are untouched
	wantR, wantC := patchBase()
	if !reflect.DeepEqual(res, wantR) || !reflect.DeepEqual(checks, wantC) {
		t.Errorf("arguments modified:\n%#v", res)
	}
}

func TestApplyPatchErrors(t *testing.T) {
	cases := []struct {
		name   string
		op     PatchOp
		reason string
	}{
		{"missing key", PatchOp{Op: "set", Key: "nope", Path: "a", Value: 1}, "not found"},
		{"index out of range", PatchOp{Op: "set", Key: "db.security_group", Path: "ingress.5.from_port", Value: 1}, "out of range"},
		{"add existing", PatchOp{Op: "add", Key: "db.instance", Type: "aws_x"}, "already exists"},
		{"unknown op", PatchOp{Op: "frobnicate", Key: "db.instance"}, "unknown"},
		{"unset missing path", PatchOp{Op: "unset", Key: "db.instance", Path: "nope"}, "not found"},
		{"remove missing", PatchOp{Op: "remove", Key: "nope"}, "not found"},
		{"set empty path", PatchOp{Op: "set", Key: "db.instance"}, "path"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res, checks := patchBase()
			ok := PatchOp{Op: "set", Key: "db.instance", Path: "engine", Value: "mysql"}
			_, _, err := ApplyPatch(res, checks, []PatchOp{ok, c.op})
			var pe *PatchError
			if !errors.As(err, &pe) {
				t.Fatalf("err = %v, want *PatchError", err)
			}
			if pe.Index != 1 || !strings.Contains(pe.Reason, c.reason) {
				t.Errorf("index %d reason %q, want index 1 containing %q", pe.Index, pe.Reason, c.reason)
			}
			if !strings.HasPrefix(err.Error(), "patch op 1 (") {
				t.Errorf("message = %q", err.Error())
			}
			wantR, wantC := patchBase()
			if !reflect.DeepEqual(res, wantR) || !reflect.DeepEqual(checks, wantC) {
				t.Errorf("arguments modified")
			}
		})
	}
	_, _, err := ApplyPatch(nil, nil, []PatchOp{{Op: "set", Key: "k", Path: "p.q", Value: 1}})
	if want := "patch op 0 (set k.p.q): resource \"k\" not found"; err == nil || err.Error() != want {
		t.Errorf("err = %v, want %q", err, want)
	}
}

func TestMarshalPatchDeterministic(t *testing.T) {
	l := sample(0)
	in := l.Intents["p"]
	in.BaseHash = "sha256:base"
	in.Patch = []PatchOp{
		{Op: "set", Key: "a", Path: "n", Value: json.Number("40")},
		{Op: "set", Key: "a", Path: "ratio", Value: json.Number("0.25")},
		{Op: "add", Key: "c", Type: "aws_x", Config: map[string]any{"size": json.Number("3"), "l": []any{json.Number("1")}}, DependsOn: []string{"p/a"}},
		{Op: "checks", Value: []any{map[string]any{"name": "n", "type": "resource_attr", "value": json.Number("2")}}},
	}
	first, err := l.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(first), `"40"`) || strings.Contains(string(first), `'40'`) {
		t.Errorf("number written as string:\n%s", first)
	}
	dir := t.TempDir()
	path := dir + "/l.yaml"
	if _, err := Save(path, l); err != nil {
		t.Fatal(err)
	}
	l2, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := l2.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatalf("round trip differs:\n%s\n---\n%s", first, second)
	}
	if l2.Intents["p"].BaseHash != "sha256:base" || len(l2.Intents["p"].Patch) != 4 {
		t.Errorf("loaded: %+v", l2.Intents["p"])
	}
	_ = time.Now
}

func TestMarshalPatchBadNumber(t *testing.T) {
	l := sample(0)
	l.Intents["p"].Patch = []PatchOp{{Op: "set", Key: "a", Path: "n", Value: json.Number("1e999")}}
	if _, err := l.Marshal(); err == nil {
		t.Fatal("want error for out-of-range number in patch")
	}
}
