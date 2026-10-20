package e2e

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/giammbo/prosa/internal/engine"
	"github.com/giammbo/prosa/internal/intent"
	"github.com/giammbo/prosa/internal/lock"
)

// demo/fixtures.json is the planner of demo/moto/run.sh without credentials:
// the base of each intent (a complete state, keyed by the hash of the intent
// file) and the patch of each intent in the staging environment (keyed by the
// effective hash, see effectiveHash), from the simulated states in
// testdata/llm/states. Rewrite it with
//
//	PROSA_DEMO_FIXTURES=1 go test ./e2e -run TestWriteDemoFixtures
//
// after a change to the examples, the states or the script's edits.

const (
	demoFixturesEnv  = "PROSA_DEMO_FIXTURES"
	demoFixturesPath = "../demo/fixtures.json"
	demoScriptPath   = "../demo/moto/run.sh"
)

// The two text substitutions of the script's second step, the same as
// TestPlannerOnMoto's: the check alone contradicts the first sentence (one
// replica); both make two replicas.
var (
	replicasCheck    = [2]string{"The number of healthy replicas at rest is the requested one.", "There are exactly two healthy replicas at rest."}
	replicasSentence = [2]string{"with one replica", "with two replicas"}
	replicasCheckTwo = replicasCheck[1]
)

// effectiveHash is the hash the engine plans the intent name under when an
// environment file is passed (engine.IntentHash, ADR-029, ADR-030): of the
// intent file, the common lines of the environment and its section for the
// intent. A StaticPlanner looks a patch fixture up under it first.
func effectiveHash(name, intentText, envText string) string {
	return engine.IntentHash(intent.Intent{Name: name, Hash: fileHash(intentText)}, &intent.Env{Body: strings.TrimSpace(envText)})
}

// fileHash is intent.Hash of a file with that text. A base fixture is keyed
// by it.
func fileHash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// demoFixture is one entry of the fixtures file, the fields in the order
// engine.LoadFixtures documents. A base fixture has resources; a patch
// fixture has no resources and a patch.
type demoFixture struct {
	Intent    string          `json:"intent"`
	Hash      string          `json:"hash"`
	Resources []stateResource `json:"resources"`
	Blockers  []string        `json:"blockers"`
	Checks    []lock.Check    `json:"checks"`
	Patch     []lock.PatchOp  `json:"patch"`
}

// readExample reads examples/<rel>.md.
func readExample(t *testing.T, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(examplesDir, rel+".md"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// baseFixture is the fixture of the base planned from the intent text.
func baseFixture(name, text string, st desiredState, blockers ...string) demoFixture {
	return demoFixture{Intent: name, Hash: fileHash(text), Resources: st.Resources, Blockers: blockers, Checks: st.Checks}
}

// patchFixture is the fixture of the patch that turns base into env, for the
// intent text under the environment text.
func patchFixture(name, text, envText string, base, env desiredState) demoFixture {
	return demoFixture{Intent: name, Hash: effectiveHash(name, text, envText), Patch: diffStates(base, env)}
}

// baseFixtures are the fixtures of the base of the three example intents.
func baseFixtures(t *testing.T) []demoFixture {
	t.Helper()
	var out []demoFixture
	for _, name := range []string{"database", "app", "cron"} {
		out = append(out, baseFixture(name, readExample(t, name), loadStates(t, "base", name)))
	}
	return out
}

// envFixtures are the fixtures of the patches of the three example intents in
// the environment env (examples/envs/<env>.md).
func envFixtures(t *testing.T, env string) []demoFixture {
	t.Helper()
	envText := readExample(t, "envs/"+env)
	var out []demoFixture
	for _, name := range []string{"database", "app", "cron"} {
		out = append(out, patchFixture(name, readExample(t, name), envText, loadStates(t, "base", name), loadStates(t, env, name)))
	}
	return out
}

// demoFixtures builds every fixture the script needs: the base of the three
// intents and their staging patch, then for the second step the blocked base
// of app, its two-replica base and the staging patch on it (the saved one
// replaces the checks, which the edit changed).
func demoFixtures(t *testing.T) []demoFixture {
	t.Helper()
	replace := func(text string, sub [2]string) string {
		if n := strings.Count(text, sub[0]); n != 1 {
			t.Fatalf("%q occurs %d times in the example, want once", sub[0], n)
		}
		return strings.Replace(text, sub[0], sub[1], 1)
	}
	web := loadStates(t, "base", "app")
	webTwo := webTwoState(web)
	envText := readExample(t, "envs/staging")
	blockedText := replace(readExample(t, "app"), replicasCheck)
	twoText := replace(blockedText, replicasSentence)
	out := append(baseFixtures(t), envFixtures(t, "staging")...)
	return append(out,
		baseFixture("app", blockedText, web, contradiction),
		baseFixture("app", twoText, webTwo),
		patchFixture("app", twoText, envText, webTwo, webTwoState(loadStates(t, "staging", "app"))),
	)
}

func renderDemoFixtures(t *testing.T) []byte {
	t.Helper()
	data, err := json.MarshalIndent(demoFixtures(t), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(data, '\n')
}

// TestWriteDemoFixtures rewrites demo/fixtures.json. It only runs with
// PROSA_DEMO_FIXTURES=1.
func TestWriteDemoFixtures(t *testing.T) {
	if os.Getenv(demoFixturesEnv) != "1" {
		t.Skipf("set %s=1 to rewrite %s", demoFixturesEnv, demoFixturesPath)
	}
	if err := os.WriteFile(demoFixturesPath, renderDemoFixtures(t), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.LoadFixtures(demoFixturesPath); err != nil {
		t.Fatalf("the written file does not load: %v", err)
	}
}

// demo/fixtures.json is what the generator writes now (offline): a changed
// example or state without a rewrite would make the script fail on a hash.
func TestDemoFixturesUpToDate(t *testing.T) {
	got, err := os.ReadFile(demoFixturesPath)
	if err != nil {
		t.Fatalf("%v: run %s=1 go test ./e2e -run TestWriteDemoFixtures", err, demoFixturesEnv)
	}
	if string(got) != string(renderDemoFixtures(t)) {
		t.Errorf("%s is stale: run %s=1 go test ./e2e -run TestWriteDemoFixtures", demoFixturesPath, demoFixturesEnv)
	}
	sp, err := engine.LoadFixtures(demoFixturesPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(sp) != 9 {
		t.Errorf("%d fixtures, want 9 (three bases and three staging patches, the blocked and two-replica bases of app and its patch)", len(sp))
	}
}

// The script makes the edits the fixtures are keyed by, with the same text.
func TestDemoScriptUsesTheFixtureEdits(t *testing.T) {
	data, err := os.ReadFile(demoScriptPath)
	if err != nil {
		t.Fatalf("the demo script: %v", err)
	}
	for _, sub := range [][2]string{replicasSentence, replicasCheck} {
		line := fmt.Sprintf("edit app.md %q %q\n", sub[0], sub[1])
		if !strings.Contains(string(data), line) {
			t.Errorf("%s lacks the line %s", demoScriptPath, line)
		}
	}
}
