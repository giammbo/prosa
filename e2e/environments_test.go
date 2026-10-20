package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giammbo/prosa/internal/lock"
)

// prodRegion is the region of the prod environment in these tests. Moto gives
// every RDS instance the same DbiResourceId (a constant in its source), and
// the AWS provider finds an instance by that id: two instances in one region
// of Moto make the provider read the wrong one (the second create fails with
// "couldn't find resource", the first delete never ends). Two environments of
// one account in two regions run as they would in real life, and keep
// "staging-main-db" and "prod-main-db" on Moto at the same time.
const prodRegion = "eu-west-1"

// envRegion is the region an environment runs in.
func envRegion(env string) string {
	if env == "prod" {
		return prodRegion
	}
	return motoRegion
}

// envProject is a project with two environments, driven through the real
// binary as a user does (ADR-029): the intents of examples/ in the root, the
// environment files in envs/, the planner of the simulated states.
type envProject struct {
	t        *testing.T
	dir      string
	bin      string
	fixtures string
	env      []string // without the region, which belongs to the environment
}

// newEnvProject builds the binary (into a temporary directory), copies the
// example intents and the environment files staging.md and prod.md, pins the
// provider version in both locks and writes the fixtures: the base of each
// intent, keyed by its hash, the patch of each intent in each environment,
// keyed by the effective hash (see effectiveHash), and those of the two edits
// the test makes (see stagingStorageEdit and appTagEdit).
func newEnvProject(t *testing.T) *envProject {
	t.Helper()
	tmp := t.TempDir()
	p := &envProject{t: t, dir: filepath.Join(tmp, "project"), bin: filepath.Join(tmp, "prosa"), fixtures: filepath.Join(tmp, "fixtures.json")}
	build := exec.Command("go", "build", "-tags", "fixtureevents", "-o", p.bin, "../cmd/prosa")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	if err := os.MkdirAll(filepath.Join(p.dir, "envs"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"database", "app", "cron"} {
		p.copyExample(name+".md", name+".md")
	}
	for _, env := range []string{"staging", "prod"} {
		p.copyExample("envs/"+env+".md", "envs/"+env+".md")
		pinned := &lock.Lock{Version: 1, ProviderVersions: map[string]string{"hashicorp/aws": awsVersion}, Intents: map[string]*lock.Intent{}}
		if _, err := lock.Save(filepath.Join(p.dir, "envs", env+".lock.yaml"), pinned); err != nil {
			t.Fatal(err)
		}
	}
	fixtures := slices.Concat(baseFixtures(t), envFixtures(t, "staging"), envFixtures(t, "prod"), editFixtures(t))
	data, err := json.MarshalIndent(fixtures, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.fixtures, data, 0o644); err != nil {
		t.Fatal(err)
	}
	// Moto and its region by the environment, never the shell's AWS; the
	// state store key is the test's own.
	p.env = slices.DeleteFunc(os.Environ(), func(kv string) bool {
		return strings.HasPrefix(kv, "AWS_") || strings.HasPrefix(kv, "PROSA_")
	})
	p.env = append(p.env, "AWS_ACCESS_KEY_ID=test", "AWS_SECRET_ACCESS_KEY=test", "AWS_PAGER=", "AWS_CONFIG_FILE=/dev/null",
		"AWS_SHARED_CREDENTIALS_FILE=/dev/null", "PROSA_AWS_ENDPOINT="+MotoURL(), "PROSA_REQUIRE_ENDPOINT=1", "PROSA_STATE_PASSPHRASE=e2e environments passphrase")
	return p
}

// The two edits of the test: a line of the database section of staging.md,
// which changes the patch of database in staging only, and a sentence of
// app.md outside its Checks section, which changes the base of app only.
var (
	stagingStorageEdit = [2]string{"40 GB of storage and Postgres 18.", "50 GB of storage and Postgres 18."}
	appTagEdit         = [2]string{"Required tag: `team=platform`.", "Every resource carries the tag `team=platform`."}
)

// editFixtures are the fixtures of the planner for the two edits: the patch
// of database in staging with 50 GB, and the base of app with the new
// sentence (the same state: the sentence says what was already planned).
func editFixtures(t *testing.T) []demoFixture {
	t.Helper()
	once := func(text string, sub [2]string) string {
		if n := strings.Count(text, sub[0]); n != 1 {
			t.Fatalf("%q occurs %d times, want once", sub[0], n)
		}
		return strings.Replace(text, sub[0], sub[1], 1)
	}
	db := loadStates(t, "staging", "database")
	db.Resources = mutate(db.Resources, map[string]func(map[string]any){
		"db.instance": func(c map[string]any) { c["allocated_storage"] = json.Number("50") },
	})
	return []demoFixture{
		patchFixture("database", readExample(t, "database"), once(readExample(t, "envs/staging"), stagingStorageEdit), loadStates(t, "base", "database"), db),
		baseFixture("app", once(readExample(t, "app"), appTagEdit), loadStates(t, "base", "app")),
	}
}

// plannerCalls returns the planner.result events of the log (relative to the
// project) that come after the first skip lines, as "<intent>:<job>:<for>",
// sorted, and the number of lines of the log. The fixtures planner (in the
// binary built with the tag fixtureevents) reports each answer as a model
// would, one event per call of the planner: the job is plan for an intent the
// lock does not have and patch for the changes to a state the planner is
// shown (the base, or the base of the environment), and "for" is base (planned
// without an environment) or env (the environment's patch).
func (p *envProject) plannerCalls(rel string, skip int) (calls []string, lines int) {
	p.t.Helper()
	data, err := os.ReadFile(filepath.Join(p.dir, rel))
	if os.IsNotExist(err) {
		return nil, 0
	}
	if err != nil {
		p.t.Fatal(err)
	}
	for i, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
		lines = i + 1
		var e event
		if err := json.Unmarshal(line, &e); err != nil {
			p.t.Fatalf("%s line %d: %v", rel, lines, err)
		}
		if i >= skip && e.Type == "planner.result" {
			calls = append(calls, e.Intent+":"+e.str("job")+":"+e.str("model"))
		}
	}
	slices.Sort(calls)
	return calls, lines
}

// counted runs prosa and returns its output, error and the calls of the
// planner it made (see plannerCalls), from the log of the environment.
func (p *envProject) counted(ctx context.Context, env string, args ...string) (string, []string, error) {
	p.t.Helper()
	rel := ".prosa/events.jsonl"
	if env != "" {
		rel = ".prosa/events/" + env + ".jsonl"
	}
	_, before := p.plannerCalls(rel, 0)
	out, err := p.prosa(ctx, args...)
	calls, _ := p.plannerCalls(rel, before)
	return out, calls, err
}

func (p *envProject) copyExample(from, to string) {
	p.t.Helper()
	data, err := os.ReadFile(filepath.Join(examplesDir, from))
	if err != nil {
		p.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.dir, to), data, 0o644); err != nil {
		p.t.Fatal(err)
	}
}

// prosa runs the binary in the project and returns its output. The region is
// that of the environment named by --env.
func (p *envProject) prosa(ctx context.Context, args ...string) (string, error) {
	p.t.Helper()
	cmd := exec.CommandContext(ctx, p.bin, args...)
	region := motoRegion
	for i, a := range args {
		if a == "--env" && i+1 < len(args) {
			region = envRegion(strings.TrimSuffix(filepath.Base(args[i+1]), ".md"))
		}
	}
	cmd.Dir, cmd.Env = p.dir, append(slices.Clone(p.env), "PROSA_AWS_REGION="+region)
	out, err := cmd.CombinedOutput()
	p.t.Logf("$ prosa %s\n%s", strings.Join(args, " "), out)
	return string(out), err
}

// ok runs prosa and fails the test unless it exits 0.
func (p *envProject) ok(ctx context.Context, args ...string) string {
	p.t.Helper()
	out, err := p.prosa(ctx, args...)
	if err != nil {
		p.t.Fatalf("prosa %s: %v", strings.Join(args, " "), err)
	}
	return out
}

// apply applies envFile with the fixtures, answering yes, and returns the
// calls the planner made.
func (p *envProject) apply(ctx context.Context, envFile string) []string {
	p.t.Helper()
	out, calls, err := p.counted(ctx, strings.TrimSuffix(filepath.Base(envFile), ".md"), "apply", "--env", envFile, "--fixtures", p.fixtures, "--yes")
	if err != nil || !strings.Contains(out, "Apply complete!") {
		p.t.Fatalf("apply --env %s did not complete (%v):\n%s", envFile, err, out)
	}
	return calls
}

// read returns the bytes of a file of the project.
func (p *envProject) read(rel string) []byte {
	p.t.Helper()
	data, err := os.ReadFile(filepath.Join(p.dir, rel))
	if err != nil {
		p.t.Fatal(err)
	}
	return data
}

// stateDigest is one digest over every file of the environment's state store.
func (p *envProject) stateDigest(env string) string {
	p.t.Helper()
	dir := filepath.Join(p.dir, ".prosa", "state.d", env)
	paths, err := filepath.Glob(filepath.Join(dir, "*"))
	if err != nil || len(paths) == 0 {
		p.t.Fatalf("state store of %s: %v, %d files", env, err, len(paths))
	}
	slices.Sort(paths)
	h := sha256.New()
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			p.t.Fatal(err)
		}
		fmt.Fprintf(h, "%s %d\n", filepath.Base(path), len(data))
		h.Write(data)
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

// removeIntents deletes every intent of the project root.
func (p *envProject) removeIntents() {
	p.t.Helper()
	paths, _ := filepath.Glob(filepath.Join(p.dir, "*.md"))
	for _, path := range paths {
		if err := os.Remove(path); err != nil {
			p.t.Fatal(err)
		}
	}
}

// TestEnvironmentsOnMoto applies the example intents under two environments
// to the same Moto (ADR-029): distinct resources, locks and state stores; a
// changed environment file moves only its own environment; destroying one
// environment leaves the other's resources alone.
func TestEnvironmentsOnMoto(t *testing.T) {
	RequireE2E(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	defer cancel()
	p := newEnvProject(t)
	// Whatever happens, leave Moto clean: no intents, then apply both.
	t.Cleanup(func() {
		// The test's own context is already cancelled here.
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
		defer cancel()
		p.removeIntents()
		for _, env := range []string{"staging", "prod"} {
			if _, err := p.prosa(ctx, "apply", "--env", "envs/"+env+".md", "--yes"); err != nil {
				t.Errorf("cleanup of %s: %v", env, err)
			}
		}
	})

	step := func(name string, f func(t *testing.T)) {
		t.Helper()
		if !t.Run(name, f) {
			t.FailNow()
		}
	}

	// The base is planned by plan, with no environment: the planner is asked
	// for every intent, in plan mode. plan writes nothing.
	step("plan the base", func(t *testing.T) {
		out, calls, err := p.counted(ctx, "", "plan", "--fixtures", p.fixtures)
		if err != nil || !strings.Contains(out, "will be created") {
			t.Fatalf("plan: %v\n%s", err, out)
		}
		if want := []string{"app:plan:base", "cron:plan:base", "database:plan:base"}; !slices.Equal(calls, want) {
			t.Errorf("planner calls of plan = %v, want %v", calls, want)
		}
		if _, err := os.Stat(filepath.Join(p.dir, "prosa.lock.yaml")); err == nil {
			t.Error("plan wrote the base lock")
		}
	})

	// The first environment to apply plans the base again (nothing kept it)
	// and writes it in the root's lock; the second only asks for patches.
	step("apply both", func(t *testing.T) {
		want := []string{"app:patch:env", "app:plan:base", "cron:patch:env", "cron:plan:base", "database:patch:env", "database:plan:base"}
		if calls := p.apply(ctx, "envs/staging.md"); !slices.Equal(calls, want) {
			t.Errorf("planner calls of apply --env envs/staging.md = %v, want %v", calls, want)
		}
		want = []string{"app:patch:env", "cron:patch:env", "database:patch:env"}
		if calls := p.apply(ctx, "envs/prod.md"); !slices.Equal(calls, want) {
			t.Errorf("planner calls of apply --env envs/prod.md = %v, want only %v: the base is in the root's lock", calls, want)
		}
	})

	step("base lock and patches", func(t *testing.T) {
		base, err := lock.Load(filepath.Join(p.dir, "prosa.lock.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		for _, env := range []string{"staging", "prod"} {
			lk, err := lock.Load(filepath.Join(p.dir, "envs", env+".lock.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"database", "app", "cron"} {
				bl, li := base.Intents[name], lk.Intents[name]
				if bl == nil || li == nil {
					t.Fatalf("%s: no lock entry for %s (base %v)", env, name, bl != nil)
				}
				if bl.IntentHash != fileHash(readExample(t, name)) || len(bl.Patch) != 0 || bl.BaseHash != "" {
					t.Errorf("base lock of %s: intent_hash %q, base_hash %q, %d patch ops", name, bl.IntentHash, bl.BaseHash, len(bl.Patch))
				}
				if li.BaseHash != bl.IntentHash || len(li.Patch) == 0 {
					t.Errorf("%s lock of %s: base_hash %q (want %q), %d patch ops, want a patch", env, name, li.BaseHash, bl.IntentHash, len(li.Patch))
				}
			}
		}
	})

	step("resources and tags", func(t *testing.T) {
		for _, want := range []struct {
			id, storage, multiAZ, env string
		}{
			{"staging-main-db", "40", "False", "staging"},
			{"prod-main-db", "100", "True", "prod"},
		} {
			out, err := awsOutIn(ctx, t, envRegion(want.env), "rds", "describe-db-instances", "--db-instance-identifier", want.id,
				"--query", "DBInstances[0].[AllocatedStorage,MultiAZ,DBInstanceArn]", "--output", "text")
			if err != nil {
				t.Fatalf("describe %s: %v\n%s", want.id, err, out)
			}
			f := strings.Fields(out)
			if len(f) != 3 || f[0] != want.storage || f[1] != want.multiAZ {
				t.Errorf("%s: storage, multi-AZ = %v, want %s, %s", want.id, f, want.storage, want.multiAZ)
				continue
			}
			tag, err := awsOutIn(ctx, t, envRegion(want.env), "rds", "list-tags-for-resource", "--resource-name", f[2],
				"--query", "TagList[?Key=='prosa/env'].Value", "--output", "text")
			if err != nil || tag != want.env {
				t.Errorf("%s: tag prosa/env = %q (err %v), want %q", want.id, tag, err, want.env)
			}
			tag, err = awsOutIn(ctx, t, envRegion(want.env), "ecs", "describe-clusters", "--clusters", want.env+"-web", "--include", "TAGS",
				"--query", "clusters[0].tags[?key=='prosa/env'].value", "--output", "text")
			if err != nil || tag != want.env {
				t.Errorf("%s-web: tag prosa/env = %q (err %v), want %q", want.env, tag, err, want.env)
			}
		}
		stagingLock, prodLock := p.read("envs/staging.lock.yaml"), p.read("envs/prod.lock.yaml")
		if bytes.Equal(stagingLock, prodLock) {
			t.Error("the two locks are identical")
		}
		for env, data := range map[string][]byte{"staging": stagingLock, "prod": prodLock} {
			if !bytes.Contains(data, []byte(env+"-main-db")) {
				t.Errorf("envs/%s.lock.yaml does not name %s-main-db", env, env)
			}
			if bytes.Contains(data, []byte(map[string]string{"staging": "prod", "prod": "staging"}[env]+"-main-db")) {
				t.Errorf("envs/%s.lock.yaml names the other environment's database", env)
			}
		}
	})

	var prodLock []byte
	var prodState string
	step("a changed environment line replans only its intent in its environment", func(t *testing.T) {
		prodLock, prodState = p.read("envs/prod.lock.yaml"), p.stateDigest("prod")
		stagingLock := p.read("envs/staging.lock.yaml")
		staging := filepath.Join(p.dir, "envs", "staging.md")
		data := p.read("envs/staging.md")
		if n := strings.Count(string(data), stagingStorageEdit[0]); n != 1 {
			t.Fatalf("envs/staging.md: %q occurs %d times, want once", stagingStorageEdit[0], n)
		}
		edited := strings.Replace(string(data), stagingStorageEdit[0], stagingStorageEdit[1], 1)
		if err := os.WriteFile(staging, []byte(edited), 0o644); err != nil {
			t.Fatal(err)
		}
		// Prod is not replanned, nor touched.
		out, calls, err := p.counted(ctx, "prod", "plan", "--env", "envs/prod.md", "--fixtures", p.fixtures)
		if err != nil || !strings.Contains(out, "No changes.") || len(calls) != 0 {
			t.Errorf("plan --env envs/prod.md after editing staging.md: err %v, calls %v:\n%s", err, calls, out)
		}
		if !bytes.Equal(p.read("envs/prod.lock.yaml"), prodLock) {
			t.Error("envs/prod.lock.yaml changed")
		}
		if got := p.stateDigest("prod"); got != prodState {
			t.Error("the state store of prod changed")
		}
		// Staging asks for the patch of database alone: the base is in the
		// lock, the line is in its section, app and cron are as they were.
		out, calls, err = p.counted(ctx, "staging", "plan", "--env", "envs/staging.md", "--fixtures", p.fixtures)
		if err != nil || !slices.Equal(calls, []string{"database:patch:env"}) || !strings.Contains(out, "40 -> 50") ||
			!strings.Contains(out, "# database/db.instance will be updated in-place") {
			t.Errorf("plan --env envs/staging.md after editing it: err %v, calls %v, want database:patch:env alone and the storage 40 -> 50:\n%s", err, calls, out)
		}
		if !bytes.Equal(p.read("envs/staging.lock.yaml"), stagingLock) {
			t.Error("plan wrote envs/staging.lock.yaml")
		}
	})

	step("a changed sentence of the base applies the patch again without the model", func(t *testing.T) {
		app := filepath.Join(p.dir, "app.md")
		data := p.read("app.md")
		if n := strings.Count(string(data), appTagEdit[0]); n != 1 {
			t.Fatalf("app.md: %q occurs %d times, want once", appTagEdit[0], n)
		}
		if err := os.WriteFile(app, []byte(strings.Replace(string(data), appTagEdit[0], appTagEdit[1], 1)), 0o644); err != nil {
			t.Fatal(err)
		}
		// The base of app is planned again, as changes to the old base (it has
		// one); its patch in prod sets the names of the new base and replaces
		// checks that did not change, so it is applied again: no call for
		// the environment.
		out, calls, err := p.counted(ctx, "prod", "plan", "--env", "envs/prod.md", "--fixtures", p.fixtures)
		if err != nil || !slices.Equal(calls, []string{"app:patch:base"}) || !strings.Contains(out, "No changes.") ||
			!strings.Contains(out, "The lock will be updated") {
			t.Errorf("plan --env envs/prod.md after editing app.md: err %v, calls %v, want app:patch:base alone (no call for the environment) and the lock to update:\n%s", err, calls, out)
		}
		if !bytes.Equal(p.read("envs/prod.lock.yaml"), prodLock) || p.stateDigest("prod") != prodState {
			t.Error("plan changed the lock or the state store of prod")
		}
	})

	step("destroying one environment leaves the other", func(t *testing.T) {
		p.removeIntents()
		p.apply(ctx, "envs/staging.md")
		if out, err := awsOutIn(ctx, t, motoRegion, "rds", "describe-db-instances", "--db-instance-identifier", "staging-main-db"); err == nil {
			t.Errorf("staging-main-db is still on Moto:\n%s", out)
		}
		out, err := awsOutIn(ctx, t, prodRegion, "rds", "describe-db-instances", "--db-instance-identifier", "prod-main-db",
			"--query", "DBInstances[0].[AllocatedStorage,MultiAZ]", "--output", "text")
		if err != nil || strings.Join(strings.Fields(out), " ") != "100 True" {
			t.Errorf("prod-main-db after destroying staging: %v\n%s", err, out)
		}
		if out, err := awsOutIn(ctx, t, prodRegion, "ecs", "describe-clusters", "--clusters", "prod-web",
			"--query", "clusters[?status=='ACTIVE'].clusterName", "--output", "text"); err != nil || strings.TrimSpace(out) != "prod-web" {
			t.Errorf("active ECS clusters of prod = %q (err %v), want prod-web", out, err)
		}
		if out, err := awsOutIn(ctx, t, motoRegion, "ecs", "describe-clusters", "--clusters", "staging-web",
			"--query", "clusters[?status=='ACTIVE'].clusterName", "--output", "text"); err != nil || strings.TrimSpace(out) != "" {
			t.Errorf("active ECS clusters of staging = %q (err %v), want none", out, err)
		}
		if !bytes.Equal(p.read("envs/prod.lock.yaml"), prodLock) || p.stateDigest("prod") != prodState {
			t.Error("the lock or the state store of prod changed by destroying staging")
		}
	})

	step("clean up", func(t *testing.T) {
		p.apply(ctx, "envs/prod.md")
		wantMotoClean(ctx, t)
	})
}
