package project

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/giammbo/prosa/internal/intent"
)

func TestOpenRejectsProsaYAML(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "prosa.yaml"), []byte("version: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := Open(root)
	if p != nil || !errors.Is(err, ErrLegacyConfig) {
		t.Fatalf("Open = %v, %v; want ErrLegacyConfig", p, err)
	}
	const want = "prosa.yaml is no longer used (ADR-029): credentials and region come from the AWS CLI; delete it"
	if err.Error() != want {
		t.Errorf("err = %q, want %q", err, want)
	}
}

func TestOpenPaths(t *testing.T) {
	root := t.TempDir()
	p, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if p.Root != root {
		t.Errorf("Root = %q, want %q", p.Root, root)
	}
	got := []string{p.LockPath(), p.StateDir(), p.EventsPath(), p.PlannerCacheDir()}
	want := []string{
		filepath.Join(root, "prosa.lock.yaml"),
		filepath.Join(root, ".prosa", "state"),
		filepath.Join(root, ".prosa", "events.jsonl"),
		filepath.Join(root, ".prosa", "planner"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("paths = %v, want %v", got, want)
	}
}

func TestOpenMakesRootAbsolute(t *testing.T) {
	t.Chdir(t.TempDir())
	p, err := Open(".")
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(p.Root) {
		t.Errorf("Root = %q, want absolute", p.Root)
	}
}

// The example project is the contract (CLAUDE.md): it opens without any
// configuration file, and the envs/ folder is not read as intents.
func TestExampleProjectOpens(t *testing.T) {
	p, err := Open("../../examples")
	if err != nil {
		t.Fatal(err)
	}
	intents, err := intent.Load(p.Root, "README.md")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, it := range intents {
		names = append(names, it.Name)
	}
	if want := []string{"app", "cron", "database"}; !reflect.DeepEqual(names, want) {
		t.Errorf("intents = %v, want %v", names, want)
	}
}

// The project of the run on real AWS has the ADR-029 layout: the
// base intents in the root, the staging environment in envs/, no prosa.yaml.
func TestDemoAWSProject(t *testing.T) {
	p, err := Open("../../demo/aws/project")
	if err != nil {
		t.Fatal(err)
	}
	intents, err := intent.Load(p.Root, "README.md")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, it := range intents {
		names = append(names, it.Name)
	}
	if want := []string{"app", "cron", "database"}; !reflect.DeepEqual(names, want) {
		t.Errorf("intents = %v, want %v", names, want)
	}
	env, err := intent.LoadEnv(filepath.Join(p.Root, "envs", "staging.md"))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.CheckEnvLocation(env); err != nil {
		t.Error(err)
	}
	if env.Name != "staging" {
		t.Errorf("env = %q, want staging", env.Name)
	}
}

func TestEnvPaths(t *testing.T) {
	root := t.TempDir()
	p, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	env := intent.Env{Name: "staging", Path: filepath.Join(p.Root, "envs", "staging.md")}
	lockPath, stateDir, eventsPath := p.EnvPaths(env)
	if want := filepath.Join(p.Root, "envs", "staging.lock.yaml"); lockPath != want {
		t.Errorf("lock = %q, want %q", lockPath, want)
	}
	if want := filepath.Join(p.Root, ".prosa", "state.d", "staging"); stateDir != want {
		t.Errorf("state = %q, want %q", stateDir, want)
	}
	if want := filepath.Join(p.Root, ".prosa", "events", "staging.jsonl"); eventsPath != want {
		t.Errorf("events = %q, want %q", eventsPath, want)
	}
	if err := p.CheckEnvLocation(env); err != nil {
		t.Errorf("envs/staging.md: %v", err)
	}
}

// An environment file in the root would also be read as an intent.
func TestEnvInRootRejected(t *testing.T) {
	root := t.TempDir()
	p, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	err = p.CheckEnvLocation(intent.Env{Name: "staging", Path: filepath.Join(p.Root, "staging.md")})
	const want = "staging.md is in the project root, where every .md is an intent: move it to envs/"
	if err == nil || err.Error() != want {
		t.Errorf("err = %v, want %q", err, want)
	}
}
