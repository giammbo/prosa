package e2e

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giammbo/prosa/internal/checks"
	"github.com/giammbo/prosa/internal/engine"
	"github.com/giammbo/prosa/internal/intent"
	"github.com/giammbo/prosa/internal/lock"
	"github.com/giammbo/prosa/internal/project"
	"github.com/giammbo/prosa/internal/registry"
	"github.com/giammbo/prosa/internal/state"
)

// dbIdentifier is the identifier of the RDS instance in testdata/fixtures.json.
const dbIdentifier = "prosa-e2e"

// TestEngineScriptOnMoto runs the demo script against Moto with the real
// hashicorp/aws provider: create two intents that refer to each other,
// steady state, drift, a rewritten intent, a removed intent, and cleanup.
func TestEngineScriptOnMoto(t *testing.T) {
	RequireE2E(t)
	ctx, cancel := context.WithTimeout(context.Background(), 18*time.Minute)
	defer cancel()
	fixtures, err := engine.LoadFixtures("testdata/fixtures.json")
	if err != nil {
		t.Fatal(err)
	}
	s := newScript(t, fixtures)

	s.copyIntent("intents/network.md", "network.md")
	s.copyIntent("intents/postgres.md", "postgres.md")

	var afterCreate map[string][]byte
	step := func(name string, f func(t *testing.T)) {
		t.Helper()
		if !t.Run(name, f) {
			t.FailNow()
		}
	}

	step("create", func(t *testing.T) {
		p := s.plan(ctx, t)
		if got := len(p.Changes); got != 6 {
			t.Fatalf("plan has %d changes, want 6: %v", got, summary(p))
		}
		pos := map[string]int{}
		for i, c := range p.Changes {
			if c.Action != engine.Create {
				t.Errorf("%s/%s: action %s, want create", c.Intent, c.Key, c.Action)
			}
			pos[c.Intent+"/"+c.Key] = i
		}
		for node, deps := range map[string][]string{
			"network/subnet.a":         {"network/vpc"},
			"network/subnet.b":         {"network/vpc"},
			"network/sg":               {"network/vpc"},
			"postgres/db.subnet_group": {"network/subnet.a", "network/subnet.b"},
			"postgres/db.instance":     {"postgres/db.subnet_group", "network/sg"},
		} {
			for _, d := range deps {
				if pos[d] >= pos[node] {
					t.Errorf("order %v: %s planned before %s, which it refers to", summary(p), node, d)
				}
			}
		}
		s.apply(ctx, t, p)

		lk := s.lock(t)
		if len(lk.Intents) != 2 {
			t.Fatalf("lock has %d intents, want 2", len(lk.Intents))
		}
		prefix := map[string]string{"vpc": "vpc-", "subnet.a": "subnet-", "subnet.b": "subnet-", "sg": "sg-", "db.subnet_group": "prosa-e2e", "db.instance": "db-"}
		n := 0
		for name, li := range lk.Intents {
			if li.IntentHash != s.hashOf(t, name) {
				t.Errorf("lock intent_hash of %s = %q, want the file's hash", name, li.IntentHash)
			}
			for _, r := range li.Resources {
				n++
				if !strings.HasPrefix(r.ID, prefix[r.Key]) {
					t.Errorf("%s/%s: id %q, want prefix %q", name, r.Key, r.ID, prefix[r.Key])
				}
			}
		}
		if n != 6 {
			t.Errorf("lock has %d resources, want 6", n)
		}
		for path, data := range s.files(t) {
			if strings.HasSuffix(path, ".state") && bytes.Contains(data, []byte("subnet-")) {
				t.Errorf("%s holds the string subnet- in clear", path)
			}
		}
		// Right after the create nothing has drifted (ADR-018): computed-only
		// attributes and null-versus-empty differences do not count.
		after := s.plan(ctx, t)
		var out bytes.Buffer
		engine.Render(&out, after, engine.RenderOptions{})
		t.Logf("render of the plan after create:\n%s", out.String())
		if strings.Contains(out.String(), "drifted outside Prosa") {
			t.Errorf("plan after create shows drift:\n%s", out.String())
		}
		for _, c := range after.Changes {
			if c.Drifted {
				t.Errorf("%s/%s drifted after create: %v", c.Intent, c.Key, c.DriftAttrs)
			}
			if len(c.InfoDrift) > 0 {
				t.Logf("%s/%s informational drift: %v", c.Intent, c.Key, c.InfoDrift)
			}
		}
		afterCreate = s.files(t)
	})

	step("steady state", func(t *testing.T) {
		for round := 1; round <= 3; round++ {
			p := s.plan(ctx, t)
			if p.HasChanges() || p.LockDirty || p.Blocked != "" {
				t.Fatalf("round %d: HasChanges=%v LockDirty=%v Blocked=%q: %v", round, p.HasChanges(), p.LockDirty, p.Blocked, summary(p))
			}
			if err := s.engine.Apply(ctx, p, nil); err != nil {
				t.Fatalf("round %d: apply without changes: %v", round, err)
			}
			if got := s.files(t); !sameFiles(got, afterCreate) {
				t.Fatalf("round %d: lock or state store rewritten without changes", round)
			}
		}
	})

	step("drift", func(t *testing.T) {
		cmd := exec.CommandContext(ctx, awsCLI(t), "--endpoint-url", MotoURL(), "--region", motoRegion,
			"rds", "modify-db-instance", "--db-instance-identifier", dbIdentifier,
			"--allocated-storage", "20", "--apply-immediately", "--output", "json")
		cmd.Env = awsCLIEnv()
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("aws rds modify-db-instance: %v\n%s", err, out)
		}

		p := s.plan(ctx, t)
		c := s.onlyChange(t, p, engine.Update, "postgres/db.instance")
		if !c.Drifted {
			t.Error("drifted instance: Drifted = false")
		}
		var out bytes.Buffer
		engine.Render(&out, p, engine.RenderOptions{})
		t.Logf("render of the drift plan:\n%s", out.String())
		if !regexp.MustCompile(`(?m)^      ~ allocated_storage += 20 -> 10$`).MatchString(out.String()) || !strings.Contains(out.String(), "  # (drifted outside Prosa: allocated_storage)\n") {
			t.Errorf("render lacks the drift:\n%s", out.String())
		}
		s.apply(ctx, t, p)
		s.noChanges(ctx, t)
	})

	step("intent change", func(t *testing.T) {
		s.copyIntent("postgres-v2.md", "postgres.md")
		p := s.plan(ctx, t)
		s.onlyChange(t, p, engine.Update, "postgres/db.instance")
		if !p.LockDirty {
			t.Error("rewritten intent: LockDirty = false")
		}
		var out bytes.Buffer
		engine.Render(&out, p, engine.RenderOptions{})
		if !regexp.MustCompile(`(?m)^      ~ backup_retention_period += 7 -> 3$`).MatchString(out.String()) {
			t.Errorf("render lacks the change:\n%s", out.String())
		}
		s.apply(ctx, t, p)
		if got, want := s.lock(t).Intents["postgres"].IntentHash, s.hashOf(t, "postgres"); got != want {
			t.Errorf("lock intent_hash of postgres = %q, want the new %q", got, want)
		}
		s.noChanges(ctx, t)
	})

	step("removal", func(t *testing.T) {
		if err := os.Remove(filepath.Join(s.dir, "postgres.md")); err != nil {
			t.Fatal(err)
		}
		p := s.plan(ctx, t)
		if got, want := summary(p), []string{"delete postgres/db.instance", "delete postgres/db.subnet_group"}; !slices.Equal(got, want) {
			t.Fatalf("changes = %v, want %v", got, want)
		}
		s.apply(ctx, t, p)
		if _, ok := s.lock(t).Intents["postgres"]; ok {
			t.Error("postgres still in the lock")
		}
		if _, err := os.Stat(filepath.Join(s.stateDir, "postgres.state")); !os.IsNotExist(err) {
			t.Errorf("postgres state file still exists (stat err %v)", err)
		}
		s.noChanges(ctx, t)
	})
}

// script is an e2e project in a temporary directory: its root holds the
// intents; the environment points the engine at Moto (ADR-029).
type script struct {
	t                   *testing.T
	root, dir, lockPath string // dir is root
	baseLockPath        string // the base lock in the root, with an environment
	stateDir            string
	aws                 engine.AWSSettings
	engine              *engine.Engine
	env                 *intent.Env // nil without an environment file
}

// newScript builds the engine as the CLI does (project, state store,
// planner, ProviderPool), with Moto's endpoint and region from the environment,
// with the provider version pinned in the lock so that no download happens.
// Cleanup removes every intent and applies.
func newScript(t *testing.T, planner engine.Planner) *script {
	return newEnvScript(t, planner, "")
}

// newEnvScript is newScript for the environment envName: the file
// examples/envs/<envName>.md is copied to envs/ of the project, and the
// engine runs as `prosa --env envs/<envName>.md` does, with the lock next to
// the file and the state store of the environment (project.EnvPaths).
func newEnvScript(t *testing.T, planner engine.Planner, envName string) *script {
	// The test, not the shell, says where to point.
	t.Setenv("PROSA_AWS_ENDPOINT", MotoURL())
	t.Setenv("PROSA_AWS_REGION", motoRegion)
	root := t.TempDir()
	p, err := project.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	aws, err := project.Settings(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	s := &script{t: t, root: root, dir: p.Root, lockPath: p.LockPath(), stateDir: p.StateDir(), aws: aws}
	var env *intent.Env
	if envName != "" {
		data, err := os.ReadFile(filepath.Join(examplesDir, "envs", envName+".md"))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(root, "envs"), 0o755); err != nil {
			t.Fatal(err)
		}
		envFile := filepath.Join(root, "envs", envName+".md")
		if err := os.WriteFile(envFile, data, 0o644); err != nil {
			t.Fatal(err)
		}
		e, err := intent.LoadEnv(envFile)
		if err != nil {
			t.Fatal(err)
		}
		env = &e
		s.env = env
		s.lockPath, s.stateDir, _ = p.EnvPaths(e)
	}
	if s.aws.Endpoint != MotoURL() || s.aws.Region != motoRegion {
		t.Fatalf("project settings = %+v, want Moto", s.aws)
	}
	pinned := &lock.Lock{Version: 1, ProviderVersions: map[string]string{"hashicorp/aws": awsVersion}, Intents: map[string]*lock.Intent{}}
	if _, err := lock.Save(s.lockPath, pinned); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(s.stateDir, "e2e passphrase")
	if err != nil {
		t.Fatal(err)
	}
	reg, err := registry.NewClient()
	if err != nil {
		t.Fatal(err)
	}
	pool := engine.NewProviderPool(reg, s.aws)
	t.Cleanup(pool.Close) // registered first: runs after the cleanup apply
	s.engine = &engine.Engine{
		Dir: s.dir, LockPath: s.lockPath, Ignore: []string{"README.md"}, Region: s.aws.Region,
		State: store, Planner: planner, Providers: pool.Get,
		CheckOptions: checks.Options{Emulated: true},
	}
	if env != nil {
		// As `prosa --env F` does (ADR-030): the environment derives from the
		// base, whose lock and state store are those of the root.
		s.engine.Env, s.engine.EnvFile = env, env.Path
		s.baseLockPath = p.LockPath()
		s.engine.BaseLockPath = s.baseLockPath
		if s.engine.BaseState, err = state.Open(p.StateDir(), "e2e passphrase"); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(s.destroyAll)
	return s
}

// destroyAll removes every intent and applies, leaving Moto clean.
func (s *script) destroyAll() {
	t := s.t
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	paths, _ := filepath.Glob(filepath.Join(s.dir, "*.md"))
	for _, p := range paths {
		if err := os.Remove(p); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	}
	p, err := s.engine.Plan(ctx)
	if err != nil {
		t.Errorf("cleanup plan: %v", err)
		return
	}
	if err := s.engine.Apply(ctx, p, func(*engine.Plan) bool { return true }); err != nil {
		t.Errorf("cleanup apply: %v", err)
		return
	}
	lk, err := lock.Load(s.lockPath)
	if err != nil || len(lk.Intents) != 0 {
		t.Errorf("cleanup: lock intents %v (err %v), want none", lk, err)
	}
}

// copyIntent copies testdata/<from> to <dir>/<to>.
func (s *script) copyIntent(from, to string) {
	s.t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", from))
	if err != nil {
		s.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.dir, to), data, 0o644); err != nil {
		s.t.Fatal(err)
	}
}

func (s *script) plan(ctx context.Context, t *testing.T) *engine.Plan {
	t.Helper()
	start := time.Now()
	p, err := s.engine.Plan(ctx)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if p.Blocked != "" {
		t.Fatalf("plan blocked: %s", p.Blocked)
	}
	t.Logf("plan (%s): %v", time.Since(start).Round(time.Millisecond), summary(p))
	return p
}

func (s *script) apply(ctx context.Context, t *testing.T, p *engine.Plan) {
	t.Helper()
	start := time.Now()
	if err := s.engine.Apply(ctx, p, func(*engine.Plan) bool { return true }); err != nil {
		t.Fatalf("apply: %v", err)
	}
	t.Logf("apply (%s)", time.Since(start).Round(time.Second))
}

// noChanges asserts that a new plan has nothing to do.
func (s *script) noChanges(ctx context.Context, t *testing.T) {
	t.Helper()
	if p := s.plan(ctx, t); p.HasChanges() || p.LockDirty {
		t.Fatalf("plan after apply: HasChanges=%v LockDirty=%v: %v", p.HasChanges(), p.LockDirty, summary(p))
	}
}

// onlyChange asserts that the plan's only change that is not a no-op is
// action on node, and returns it.
func (s *script) onlyChange(t *testing.T, p *engine.Plan, action engine.Action, node string) engine.Change {
	t.Helper()
	got := summary(p)
	if want := []string{string(action) + " " + node}; !slices.Equal(got, want) {
		t.Fatalf("changes = %v, want %v", got, want)
	}
	for _, c := range p.Changes {
		if c.Intent+"/"+c.Key == node {
			return c
		}
	}
	panic("unreachable")
}

func (s *script) lock(t *testing.T) *lock.Lock {
	t.Helper()
	lk, err := lock.Load(s.lockPath)
	if err != nil {
		t.Fatal(err)
	}
	return lk
}

// hashOf is the hash the lock keeps for the intent file in the project: its
// own, or with an environment the effective one (see effectiveHash).
func (s *script) hashOf(t *testing.T, name string) string {
	t.Helper()
	its, err := intent.Load(s.dir, s.engine.Ignore...)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range its {
		if it.Name == name {
			return engine.IntentHash(it, s.env)
		}
	}
	t.Fatalf("no intent %q", name)
	return ""
}

// baseHashOf is the hash of the intent file in the project: the intent_hash of
// its base.
func (s *script) baseHashOf(t *testing.T, name string) string {
	t.Helper()
	its, err := intent.Load(s.dir, s.engine.Ignore...)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range its {
		if it.Name == name {
			return it.Hash
		}
	}
	t.Fatalf("no intent %q", name)
	return ""
}

// files returns the bytes of the lock and of every file in the state store.
func (s *script) files(t *testing.T) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	paths, err := filepath.Glob(filepath.Join(s.stateDir, "*"))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range append(paths, s.lockPath) {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		out[p] = data
	}
	return out
}

func sameFiles(a, b map[string][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for p, data := range a {
		if other, ok := b[p]; !ok || !bytes.Equal(data, other) {
			return false
		}
	}
	return true
}

// awsCLIEnv is the environment of the aws CLI: the process environment
// without any AWS_ variable (profile, real credentials), plus fake
// credentials for Moto.
func awsCLIEnv() []string {
	env := slices.DeleteFunc(os.Environ(), func(kv string) bool { return strings.HasPrefix(kv, "AWS_") })
	return append(env, "AWS_ACCESS_KEY_ID=test", "AWS_SECRET_ACCESS_KEY=test", "AWS_PAGER=")
}

// summary lists the changes that are not no-ops as "<action> <intent>/<key>".
func summary(p *engine.Plan) []string {
	var out []string
	for _, c := range p.Changes {
		if c.Action != engine.NoOp {
			out = append(out, string(c.Action)+" "+c.Intent+"/"+c.Key)
		}
	}
	return out
}
