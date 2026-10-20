package lock

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func sample(order int) *Lock {
	chosen := map[string]any{}
	keys := []string{"zeta", "alpha", "mid"}
	if order == 1 {
		keys = []string{"mid", "alpha", "zeta"}
	}
	for _, k := range keys {
		chosen[k] = k
	}
	res := []Resource{
		{Key: "b", Provider: "hashicorp/aws", Type: "aws_x", ID: "2", Chosen: chosen},
		{Key: "a", Provider: "hashicorp/aws", Type: "aws_x", ID: "1", Chosen: map[string]any{"n": 1}},
	}
	if order == 1 {
		res[0], res[1] = res[1], res[0]
	}
	return &Lock{
		Version:          1,
		ProviderVersions: map[string]string{"hashicorp/aws": "6.69.1"},
		Intents: map[string]*Intent{
			"p": {IntentHash: "sha256:x", ReconciledAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
				State: StateRef{Serial: 1, Digest: "sha256:y"}, Resources: res},
		},
	}
}

func TestMarshalIsDeterministic(t *testing.T) {
	a, err := sample(0).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := sample(1).Marshal()
	if string(a) != string(b) {
		t.Fatalf("not deterministic:\n%s\n---\n%s", a, b)
	}
	l := sample(1)
	if _, err := l.Marshal(); err != nil {
		t.Fatal(err)
	}
	if l.Intents["p"].Resources[0].Key != "a" {
		t.Errorf("resources not sorted: %+v", l.Intents["p"].Resources)
	}
}

func TestRoundTrip(t *testing.T) {
	l := sample(0)
	l.Intents["p"].Resources[0].Chosen = map[string]any{
		"i": 3, "b": true, "s": "str", "l": []any{"a", "b"},
		"m": map[string]any{"k": map[string]any{"z": "v"}}, "ref": "${x/y.z}",
	}
	first, err := l.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "l.yaml")
	os.WriteFile(path, first, 0o644)
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
	var got map[string]any
	for _, r := range l2.Intents["p"].Resources {
		if r.Key == "b" {
			got = r.Chosen
		}
	}
	if got["i"] != 3 || got["b"] != true || got["ref"] != "${x/y.z}" ||
		!reflect.DeepEqual(got["l"], []any{"a", "b"}) ||
		!reflect.DeepEqual(got["m"], map[string]any{"k": map[string]any{"z": "v"}}) {
		t.Errorf("chosen types wrong: %#v", got)
	}
}

func TestSaveSkipsIdenticalContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "l.yaml")
	changed, err := Save(path, sample(0))
	if err != nil || !changed {
		t.Fatalf("first save: %v %v", changed, err)
	}
	old := time.Now().Add(-time.Hour)
	os.Chtimes(path, old, old)
	st1, _ := os.Stat(path)
	changed, err = Save(path, sample(1))
	if err != nil || changed {
		t.Fatalf("second save: %v %v", changed, err)
	}
	st2, _ := os.Stat(path)
	if !st1.ModTime().Equal(st2.ModTime()) {
		t.Error("mtime changed")
	}
}

func TestLoadMissingFileIsEmpty(t *testing.T) {
	l, err := Load(filepath.Join(t.TempDir(), "none.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if l.Version != 1 || l.ProviderVersions == nil || l.Intents == nil || len(l.Intents) != 0 {
		t.Errorf("%+v", l)
	}
}

func TestLoadExampleLock(t *testing.T) {
	l, err := Load("../../examples/envs/staging.lock.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Intents) != 1 {
		t.Fatalf("intents = %d", len(l.Intents))
	}
	pg := l.Intents["database"]
	if pg == nil || len(pg.Resources) != 3 {
		t.Fatalf("database: %+v", pg)
	}
	for _, r := range pg.Resources {
		if r.Key == "db.instance" && (r.Chosen["manage_master_user_password"] != true || r.Chosen["engine_version"] != "18.1") {
			t.Errorf("chosen: %#v", r.Chosen)
		}
	}
	if pg.BaseHash != "sha256:3f9c..." {
		t.Errorf("base_hash = %q", pg.BaseHash)
	}
	wantPatch := []PatchOp{
		{Op: "set", Key: "db.instance", Path: "engine_version", Value: "18.1"},
		{Op: "set", Key: "db.instance", Path: "allocated_storage", Value: 40},
		{Op: "set", Key: "db.instance", Path: "tags.env", Value: "staging"},
		{Op: "set", Key: "db.subnet_group", Path: "tags.env", Value: "staging"},
		{Op: "set", Key: "db.security_group", Path: "tags.env", Value: "staging"},
	}
	if !reflect.DeepEqual(pg.Patch, wantPatch) {
		t.Errorf("patch =\n%#v\nwant\n%#v", pg.Patch, wantPatch)
	}
	wantChecks := []Check{
		{Name: "The database accepts connections from the private subnet and rejects them from the internet.", Type: "resource_attr",
			Resource: "database/db.instance", Path: "publicly_accessible", Op: "eq", Value: false},
		{Name: "There is at least one backup newer than 24 hours (after the first day).", Type: "resource_attr",
			Resource: "database/db.instance", Path: "latest_restorable_time", Op: "within", Value: "24h"},
		{Name: "Storage is encrypted.", Type: "resource_attr",
			Resource: "database/db.instance", Path: "storage_encrypted", Op: "eq", Value: true},
	}
	if !reflect.DeepEqual(pg.Checks, wantChecks) {
		t.Errorf("database checks =\n%#v\nwant\n%#v", pg.Checks, wantChecks)
	}
}

func TestLoadExampleBaseLock(t *testing.T) {
	l, err := Load("../../examples/prosa.lock.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if l.Intents["database"] == nil || l.Intents["database"].BaseHash != "" || len(l.Intents["database"].Patch) != 0 {
		t.Errorf("database: %+v", l.Intents["database"])
	}
}

func TestRoundTripDependsOnAndChecks(t *testing.T) {
	l := sample(0)
	in := l.Intents["p"]
	in.Resources[0].DependsOn = []string{"p/a", "q/z"}
	in.Checks = []Check{
		{Name: "n1", Type: "resource_attr", Resource: "p/b", Path: "tags.env", Op: "eq", Value: "staging"},
		{Name: "n2", Type: "resource_attr", Resource: "p/b", Path: "count", Op: "gte", Value: json.Number("2")},
		{Name: "n3", Type: "http_get", URL: "https://x.example/health", Status: 200, Within: "60s"},
		{Name: "n4", Type: "tls_days", Host: "x.example", MinDays: 20},
		{Name: "n5", Type: "unverifiable", Reason: "no way to measure it"},
		{Name: "n6", Type: "resource_attr", Resource: "p/b", Path: "ids", Op: "contains", Value: []any{"a"}},
		{Name: "n7", Type: "resource_attr", Resource: "p/b", Path: "public", Op: "eq", Value: false},
	}
	first, err := l.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	for _, unwanted := range []string{"last_result", "last_run", "url: \"\"", "min_days: 0", "status: 0"} {
		if strings.Contains(string(first), unwanted) {
			t.Errorf("lock has %q:\n%s", unwanted, first)
		}
	}
	if !strings.Contains(string(first), "value: 2\n") {
		t.Errorf("json.Number value not written as a number:\n%s", first)
	}
	path := filepath.Join(t.TempDir(), "l.yaml")
	if err := os.WriteFile(path, first, 0o644); err != nil {
		t.Fatal(err)
	}
	l2, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v\n%s", err, first)
	}
	second, err := l2.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatalf("round trip differs:\n%s\n---\n%s", first, second)
	}
	in2 := l2.Intents["p"]
	var b *Resource
	for i := range in2.Resources {
		if in2.Resources[i].Key == "b" {
			b = &in2.Resources[i]
		}
	}
	if b == nil || !reflect.DeepEqual(b.DependsOn, []string{"p/a", "q/z"}) {
		t.Errorf("depends_on = %+v", b)
	}
	want := slices.Clone(in.Checks)
	want[1].Value = 2
	if !reflect.DeepEqual(in2.Checks, want) {
		t.Errorf("checks =\n%#v\nwant\n%#v", in2.Checks, want)
	}
	if in.Checks[1].Value != json.Number("2") {
		t.Error("Marshal modified its receiver")
	}
}

func TestRoundTripTrickyScalars(t *testing.T) {
	chosen := map[string]any{
		"yes": "yes", "no": "no", "on": "on", "octal": "0123", "exp": "1e3",
		"null": "null", "tilde": "~", "version": "16.4", "window": "02:00-03:00",
		"ref": "${a/b.c}", "big": int64(9007199254740993), "float": 0.1,
		"jint": json.Number("10"), "jbig": json.Number("9007199254740993"), "jfloat": json.Number("0.1"),
	}
	l := sample(0)
	l.Intents["p"].Resources[0].Chosen = chosen
	first, err := l.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "l.yaml")
	if err := os.WriteFile(path, first, 0o644); err != nil {
		t.Fatal(err)
	}
	l2, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v\n%s", err, first)
	}
	second, err := l2.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatalf("round trip differs:\n%s\n---\n%s", first, second)
	}
	var got map[string]any
	for _, r := range l2.Intents["p"].Resources {
		if r.Key == "b" {
			got = r.Chosen
		}
	}
	want := map[string]any{
		"yes": "yes", "no": "no", "on": "on", "octal": "0123", "exp": "1e3",
		"null": "null", "tilde": "~", "version": "16.4", "window": "02:00-03:00",
		"ref": "${a/b.c}", "big": 9007199254740993, "float": 0.1,
		"jint": 10, "jbig": 9007199254740993, "jfloat": 0.1,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("chosen after round trip:\n got %#v\nwant %#v\n%s", got, want, first)
	}
	if _, isNum := chosen["jint"].(json.Number); !isNum {
		t.Fatal("Marshal modified the receiver's chosen")
	}
}

func TestLoadIsStrict(t *testing.T) {
	const good = "version: 1\nprovider_versions: {}\nintents:\n  app:\n    intent_hash: sha256:x\n    recipe: null\n    reconciled_at: 2026-01-02T03:04:05Z\n    state: {serial: 1, digest: sha256:y}\n    resources:\n      - {key: a, provider: hashicorp/aws, type: t, id: \"1\", chosen: {}}\n"
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if _, err := Load(write("good.yaml", good)); err != nil {
		t.Fatalf("good lock: %v", err)
	}
	for _, tc := range []struct{ name, content, want string }{
		{"unknown-field.yaml", strings.Replace(good, "recipe: null", "recipe: null\n    colour: red", 1), "colour"},
		{"version-2.yaml", strings.Replace(good, "version: 1", "version: 2", 1), "version 2"},
		{"no-version.yaml", strings.Replace(good, "version: 1\n", "", 1), "version 0"},
		{"nil-intent.yaml", good + "  other:\n", `"other"`},
		{"dup-key.yaml", strings.Replace(good, "chosen: {}}\n", "chosen: {}}\n      - {key: a, provider: hashicorp/aws, type: t, id: \"2\", chosen: {}}\n", 1), `"a"`},
	} {
		path := write(tc.name, tc.content)
		_, err := Load(path)
		if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), path) {
			t.Errorf("%s: err = %v, want an error naming %s and %s", tc.name, err, path, tc.want)
		}
	}
	if _, err := Load(write("dup-key-intent.yaml", strings.Replace(good, "chosen: {}}\n", "chosen: {}}\n      - {key: a, provider: hashicorp/aws, type: t, id: \"2\", chosen: {}}\n", 1))); err == nil || !strings.Contains(err.Error(), `"app"`) {
		t.Errorf("duplicate key: err = %v, want it to name the intent", err)
	}
}

func TestReadReturnsBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "l.yaml")
	l, raw, err := Read(path)
	if err != nil || raw != nil || l.Version != 1 || l.Intents == nil {
		t.Fatalf("missing file: %+v %q %v; want an empty lock and nil bytes", l, raw, err)
	}
	if _, err := Save(path, sample(0)); err != nil {
		t.Fatal(err)
	}
	disk, _ := os.ReadFile(path)
	l, raw, err = Read(path)
	if err != nil || string(raw) != string(disk) || len(l.Intents) != 1 {
		t.Fatalf("Read = %+v, %q, %v", l, raw, err)
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Read(path); err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("empty file: err = %v, want an error naming it", err)
	}
}

func TestMarshalRejectsOutOfRangeNumbers(t *testing.T) {
	for _, n := range []string{"123456789012345678901234567890", "-9223372036854775809", "1e400", "-1e400", "1e-400", "0x10", "NaN", "Inf", "007"} {
		l := sample(0)
		l.Intents["p"].Resources[0].Chosen = map[string]any{"deep": []any{map[string]any{"n": json.Number(n)}}}
		_, err := l.Marshal()
		if err == nil || !strings.Contains(err.Error(), "number "+n+" out of range for") || !strings.Contains(err.Error(), "p/") {
			t.Errorf("%s: err = %v, want an out of range error naming the resource", n, err)
		}
	}
	// Edge values that fit stay numbers.
	l := sample(0)
	l.Intents["p"].Resources[0].Chosen = map[string]any{
		"maxu": json.Number("18446744073709551615"), "mini": json.Number("-9223372036854775808"), "f": json.Number("1.5e300"),
	}
	data, err := l.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"maxu: 18446744073709551615", "mini: -9223372036854775808", "f: 1.5e300"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("lock lacks %q:\n%s", want, data)
		}
	}
}

func TestCheckNumber(t *testing.T) {
	for _, ok := range []string{"0", "-0", "10", "0.1", "1.5e300", "0e5", "0.0", "1e-300", "18446744073709551615"} {
		if err := CheckNumber(json.Number(ok)); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "1e-400", "-1e-999", "1e400", "0x1p3", "NaN", "+Inf", "1.", ".5", "1_0"} {
		if err := CheckNumber(json.Number(bad)); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
