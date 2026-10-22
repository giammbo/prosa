package e2e

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/giammbo/prosa/internal/lock"
	"github.com/giammbo/prosa/internal/planner"
)

// The conversations in testdata/llm are simulated until a run with
// credentials records real ones (PROSA_LLM_RECORD=1, see planner_test.go):
// this file writes them from the readable desired states in
// testdata/llm/states (the base of each intent and the result of each
// environment; the patch of an environment is the difference, see
// patches_test.go), as a sequence of Opus turns (empty thinking block with
// a signature, tool calls with ids, stop reason, usage). The reviewer's
// conversations (ADR-034), the files named review-*, are one turn each: the
// violations of the planner's first database submission, and no violation.
// Rewrite them with
//
//	PROSA_LLM_SIMULATE=1 go test ./e2e -run TestWriteSimulatedConversations
//
// and delete this file once real recordings replace them.

const (
	simulateEnv = "PROSA_LLM_SIMULATE"
	llmDir      = "testdata/llm"
)

// simTurn is one assistant turn: optional text, then tool calls. A turn
// without calls ends with end_turn, which makes the planner send its nudge.
type simTurn struct {
	text  string
	calls []simCall
}

type simCall struct {
	name  string
	input any
}

// desiredState is a states file: the resources and the checks of a
// submission.
type desiredState struct {
	Resources []stateResource `json:"resources"`
	Checks    []lock.Check    `json:"checks"`
}

// stateResource is a resource of a desired state, its fields in the order
// a model writes them.
type stateResource struct {
	Key       string         `json:"key"`
	Type      string         `json:"type"`
	Config    map[string]any `json:"config"`
	DependsOn []string       `json:"depends_on,omitempty"`
}

func searchCall(query string) simCall {
	return simCall{"search_resource_types", map[string]any{"query": query}}
}

func schemaCall(typeName string) simCall {
	return simCall{"get_resource_schema", map[string]any{"type": typeName}}
}

func submitCall(resources []stateResource, checks []lock.Check, blockers ...string) simCall {
	return simCall{"submit_desired_state", struct {
		Resources []stateResource `json:"resources"`
		Checks    []lock.Check    `json:"checks,omitempty"`
		Blockers  []string        `json:"blockers,omitempty"`
	}{resources, checks, blockers}}
}

func schemaCalls(types ...string) []simCall {
	out := make([]simCall, len(types))
	for i, t := range types {
		out[i] = schemaCall(t)
	}
	return out
}

// contradiction is the blocker of app-blocked: it quotes the check changed
// by the test (and the demo script) and the sentence it contradicts.
const contradiction = `The intent asks for "one replica" in its first paragraph, but its check "There are exactly two healthy replicas at rest." requires exactly two: the two sentences contradict each other. Change one of them.`

// webTwoState is the app state with two replicas: only the service's desired
// count changes, and the replica check follows the sentence of the intent,
// which the test (and the demo script) rewrite.
func webTwoState(web desiredState) desiredState {
	resources := mutate(web.Resources, map[string]func(map[string]any){
		"app.service": func(c map[string]any) { c["desired_count"] = json.Number("2") },
	})
	checks := slices.Clone(web.Checks)
	for i, c := range checks {
		if c.Resource == "app/app.service" {
			c.Name = replicasCheckTwo
			c.Value = json.Number("2")
			checks[i] = c
		}
	}
	return desiredState{Resources: resources, Checks: checks}
}

// subnetIngress returns the database state as the planner first writes it,
// breaking AWS-NET-001: Postgres admitted from the CIDR of each private
// subnet instead of from the app's security group (db.sg_ingress_from_app).
func subnetIngress(pg desiredState) []stateResource {
	var out []stateResource
	if err := roundTrip(pg.Resources, &out); err != nil {
		panic(err)
	}
	at := slices.IndexFunc(out, func(r stateResource) bool { return r.Key == "db.sg_ingress_from_app" })
	if at < 0 {
		panic("subnetIngress: no db.sg_ingress_from_app")
	}
	from := func(az string) stateResource {
		return stateResource{Key: "db.sg_ingress_private_" + az, Type: "aws_vpc_security_group_ingress_rule", Config: map[string]any{
			"security_group_id": "${database/db.sg.id}",
			"description":       "Postgres from the private subnet in eu-south-1" + az,
			"cidr_ipv4":         "${database/net.subnet_private_" + az + ".cidr_block}",
			"ip_protocol":       "tcp",
			"from_port":         json.Number("5432"),
			"to_port":           json.Number("5432"),
			"tags":              map[string]any{"team": "platform"},
		}}
	}
	return slices.Replace(out, at, at+1, from("a"), from("b"))
}

// subnetViolations are the reviewer's answer to the first database
// submission: AWS-NET-001 on each subnet rule.
var subnetViolations = []map[string]string{
	{"rule": "AWS-NET-001", "key": "db.sg_ingress_private_a", "reason": subnetReason("a")},
	{"rule": "AWS-NET-001", "key": "db.sg_ingress_private_b", "reason": subnetReason("b")},
}

func subnetReason(az string) string {
	return "It admits Postgres from the CIDR of the private subnet in eu-south-1" + az +
		"; the database's port must be open only to the security group of its clients: reference net.app_sg by its id instead."
}

// reviewCall is a submit_review call with the violations given.
func reviewCall(violations []map[string]string) simCall {
	if violations == nil {
		violations = []map[string]string{}
	}
	return simCall{"submit_review", map[string]any{"violations": violations}}
}

// badOp is an operation the model gets wrong in the first submission of
// database-staging: a resource the base does not have.
var badOp = lock.PatchOp{Op: "set", Key: "db.replica", Path: "allocated_storage", Value: json.Number("40")}

// badOpProblem is what the engine answers to it, at index 2 of the patch.
const badOpProblem = `patch op 2 (set db.replica.allocated_storage): resource "db.replica" not found`

// changesCall is a submit_changes call: the operations, in this order, and
// the blockers, if any.
func changesCall(ops []lock.PatchOp, blockers ...string) simCall {
	if ops == nil {
		ops = []lock.PatchOp{}
	}
	return simCall{"submit_changes", struct {
		Changes  []lock.PatchOp `json:"changes"`
		Blockers []string       `json:"blockers,omitempty"`
	}{ops, blockers}}
}

// patchTurns is the conversation of an environment's patch on a base: the
// schemas of the resource types it changes, then the changes. With bad, the
// first submission has badOp too, which the engine returns as a problem.
func patchTurns(base, env desiredState, bad bool) []simTurn {
	ops := diffStates(base, env)
	types := map[string]string{}
	for _, r := range append(slices.Clone(base.Resources), env.Resources...) {
		types[r.Key] = r.Type
	}
	var touched []string
	for _, op := range ops {
		typ := types[op.Key]
		if op.Op == "add" {
			typ = op.Type
		}
		if typ != "" && !slices.Contains(touched, typ) {
			touched = append(touched, typ)
		}
	}
	if len(touched) > 4 {
		touched = touched[:4]
	}
	var turns []simTurn
	if len(touched) > 0 {
		turns = append(turns, simTurn{calls: schemaCalls(touched...)})
	}
	if bad {
		at := min(2, len(ops))
		wrong := slices.Insert(slices.Clone(ops), at, badOp)
		turns = append(turns, simTurn{calls: []simCall{changesCall(wrong)}})
	}
	return append(turns, simTurn{calls: []simCall{changesCall(ops)}})
}

// simulatedConversations returns every conversation, by file name: the base
// of each intent, the patch of each intent in each environment, and the
// edits of app.md that the tests and the demo script make.
func simulatedConversations(t *testing.T) map[string][]simTurn {
	t.Helper()
	pg, web, cron := loadStates(t, "base", "database"), loadStates(t, "base", "app"), loadStates(t, "base", "cron")

	// Both app submissions write the health check, a list block of at
	// most one item, as a lone object, which the engine accepts and stores
	// as a one-element list. The first one also has the ECS launch type in
	// lower case, which the provider's validation rejects.
	webLone := mutate(web.Resources, map[string]func(map[string]any){
		"app.target_group": func(c map[string]any) { unwrapBlock(c, "health_check") },
	})
	webFirst := mutate(webLone, map[string]func(map[string]any){
		"app.service": func(c map[string]any) { c["launch_type"] = "fargate" },
	})
	// First cron submission: a flexible time window mode in lower case, which
	// the provider's validation rejects.
	cronFirst := mutate(cron.Resources, map[string]func(map[string]any){
		"job.schedule": func(c map[string]any) {
			c["flexible_time_window"].([]any)[0].(map[string]any)["mode"] = "off"
		},
	})
	webTwo := webTwoState(web)
	webTwoStaging := webTwoState(loadStates(t, "staging", "app"))

	convs := map[string][]simTurn{
		"database-base": {
			{calls: []simCall{
				searchCall("vpc subnet internet gateway nat gateway eip"),
				searchCall("route table route association"),
				searchCall("security group ingress egress rule"),
				searchCall("db instance subnet group"),
			}},
			{calls: schemaCalls("aws_vpc", "aws_subnet", "aws_internet_gateway", "aws_eip", "aws_nat_gateway",
				"aws_route_table", "aws_route", "aws_route_table_association")},
			{calls: schemaCalls("aws_security_group", "aws_vpc_security_group_ingress_rule",
				"aws_vpc_security_group_egress_rule", "aws_db_subnet_group", "aws_db_instance")},
			{calls: []simCall{submitCall(subnetIngress(pg), pg.Checks)}},
			{text: "The reviewer is right: the database admits its port only from the application's security group now, not from the subnets.",
				calls: []simCall{submitCall(pg.Resources, pg.Checks)}},
		},
		"cron-base": {
			{calls: []simCall{
				searchCall("scheduler schedule"),
				searchCall("ecs cluster task definition"),
				searchCall("iam role policy"),
				searchCall("cloudwatch log group"),
			}},
			{calls: schemaCalls("aws_scheduler_schedule", "aws_ecs_cluster", "aws_ecs_task_definition",
				"aws_iam_role", "aws_iam_role_policy", "aws_cloudwatch_log_group")},
			{calls: []simCall{
				schemaCall("aws_scheduler_schedule.target.ecs_parameters"),
			}},
			{calls: []simCall{submitCall(cronFirst, cron.Checks)}},
			{calls: []simCall{submitCall(cron.Resources, cron.Checks)}},
		},
		"app-base": {
			{calls: []simCall{
				searchCall("ecs cluster service task definition"),
				searchCall("lb listener target group"),
				searchCall("acm certificate validation"),
				searchCall("iam role policy"),
				searchCall("cloudwatch log group"),
			}},
			{calls: schemaCalls("aws_ecs_cluster", "aws_ecs_task_definition", "aws_ecs_service",
				"aws_iam_role", "aws_iam_role_policy", "aws_cloudwatch_log_group")},
			{calls: schemaCalls("aws_security_group", "aws_vpc_security_group_ingress_rule",
				"aws_vpc_security_group_egress_rule", "aws_lb", "aws_lb_target_group", "aws_lb_listener",
				"aws_acm_certificate", "aws_acm_certificate_validation")},
			{calls: []simCall{submitCall(webFirst, web.Checks)}},
			{calls: []simCall{submitCall(webLone, web.Checks)}},
		},
		// The base of app.md with the check changed: the engine already has
		// a base for app, so the model answers with the changes to it.
		"app-blocked": {
			{text: "The first paragraph now asks for one replica, while the check requires exactly two healthy replicas at rest. I cannot satisfy both, so the intent has to be fixed before it can be planned."},
			{calls: []simCall{changesCall(nil, contradiction)}},
		},
		"app-two": {
			{text: "Both the first paragraph and the check now say two replicas: only the service's desired count and the replicas check change.",
				calls: []simCall{changesCall(diffStates(web, webTwo))}},
		},
		// The staging patch on the new base of app: the saved patch replaces
		// all the checks, so it cannot be applied again to a base whose
		// checks changed, and the model writes it again.
		"app-staging-two": {
			{calls: []simCall{changesCall(diffStates(webTwo, webTwoStaging))}},
		},
		// The reviewer: the subnet rules of the first database submission
		// break AWS-NET-001; every other review finds nothing.
		reviewSubnets: {{calls: []simCall{reviewCall(subnetViolations)}}},
		reviewNone:    {{calls: []simCall{reviewCall(nil)}}},
	}
	for _, env := range []string{"staging", "prod"} {
		for _, name := range []string{"database", "app", "cron"} {
			convs[name+"-"+env] = patchTurns(loadStates(t, "base", name), loadStates(t, env, name), name == "database" && env == "staging")
		}
	}
	return convs
}

// TestWriteSimulatedConversations rewrites testdata/llm/<name>.json from the
// states. It only runs with PROSA_LLM_SIMULATE=1.
func TestWriteSimulatedConversations(t *testing.T) {
	if os.Getenv(simulateEnv) != "1" {
		t.Skipf("set %s=1 to rewrite %s from %s/states", simulateEnv, llmDir, llmDir)
	}
	for name, turns := range simulatedConversations(t) {
		data, err := renderConversation(name, turns)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		path := filepath.Join(llmDir, name+".json")
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := loadConversation(name, path); err != nil {
			t.Fatalf("%s does not load as a replay: %v", path, err)
		}
		t.Logf("wrote %s (%d turns)", path, len(turns))
	}
}

// The files in testdata/llm are what the generator writes now (offline), and
// there is no other: a change to the states, the prompt or the tools without a
// rewrite would make the replays fail on their fingerprint.
func TestSimulatedConversationsUpToDate(t *testing.T) {
	convs := simulatedConversations(t)
	for name, turns := range convs {
		want, err := renderConversation(name, turns)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got, err := os.ReadFile(filepath.Join(llmDir, name+".json"))
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("%s.json is missing or stale (err %v): run %s=1 go test ./e2e -run TestWriteSimulatedConversations", name, err, simulateEnv)
		}
	}
	files, _ := filepath.Glob(filepath.Join(llmDir, "*.json"))
	for _, f := range files {
		if name := strings.TrimSuffix(filepath.Base(f), ".json"); convs[name] == nil {
			t.Errorf("%s is not written by the generator: delete it", f)
		}
	}
}

// loadStates reads testdata/llm/states/<env>/<name>.json, numbers as
// json.Number: the desired state of the intent <name> under the environment
// <env> (examples/envs/<env>.md), with the intent's base text.
func loadStates(t *testing.T, env, name string) desiredState {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(llmDir, "states", env, name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	dec.DisallowUnknownFields()
	var ds desiredState
	if err := dec.Decode(&ds); err != nil {
		t.Fatalf("states %s/%s: %v", env, name, err)
	}
	return ds
}

// mutate returns a deep copy of rs where the config of each key in changes
// went through its function.
func mutate(rs []stateResource, changes map[string]func(map[string]any)) []stateResource {
	var out []stateResource
	if err := roundTrip(rs, &out); err != nil {
		panic(err)
	}
	done := map[string]bool{}
	for _, r := range out {
		if f := changes[r.Key]; f != nil {
			f(r.Config)
			done[r.Key] = true
		}
	}
	for key := range changes {
		if !done[key] {
			panic(fmt.Sprintf("mutate: no resource %q", key))
		}
	}
	return out
}

// unwrapBlock replaces the one-element list cfg[name] with its element.
func unwrapBlock(cfg map[string]any, name string) {
	list, ok := cfg[name].([]any)
	if !ok || len(list) != 1 {
		panic(fmt.Sprintf("unwrapBlock: %s is not a one-element list", name))
	}
	cfg[name] = list[0]
}

func roundTrip(in any, out any) error {
	data, err := json.Marshal(in)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	return dec.Decode(out)
}

// simMessage is a Messages API response as the API sends it.
type simMessage struct {
	ID           string    `json:"id"`
	Type         string    `json:"type"`
	Role         string    `json:"role"`
	Model        string    `json:"model"`
	Content      []any     `json:"content"`
	StopReason   string    `json:"stop_reason"`
	StopSequence *string   `json:"stop_sequence"`
	StopDetails  *struct{} `json:"stop_details"`
	Usage        simUsage  `json:"usage"`
}

type thinkingBlock struct {
	Type      string `json:"type"`
	Thinking  string `json:"thinking"`
	Signature string `json:"signature"`
}

type textBlock struct {
	Type      string `json:"type"`
	Text      string `json:"text"`
	Citations []any  `json:"citations"`
}

type toolUseBlock struct {
	Type  string `json:"type"`
	ID    string `json:"id"`
	Name  string `json:"name"`
	Input any    `json:"input"`
}

type simUsage struct {
	InputTokens              int    `json:"input_tokens"`
	OutputTokens             int    `json:"output_tokens"`
	CacheCreationInputTokens int    `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int    `json:"cache_read_input_tokens"`
	ServiceTier              string `json:"service_tier"`
}

// Token estimates of the usage block: the cached system prompt and tools,
// the first user message, and the tool results of each kind.
const (
	prefixTokens = 2360
	userTokens   = 900
	searchTokens = 70
	schemaTokens = 1400
	submitTokens = 120
	nudgeTokens  = 40
)

// renderConversation renders turns as the recording LoadReplay reads, with
// the planner's current prompt version and fingerprint; a conversation of
// the reviewer (isReview) as LoadReviewReplay reads it, with the reviewer's.
func renderConversation(name string, turns []simTurn) ([]byte, error) {
	version, fp := planner.PromptVersion, planner.PromptFingerprint()
	if isReview(name) {
		version, fp = planner.ReviewPromptVersion, planner.ReviewPromptFingerprint()
	}
	var msgs []simMessage
	cached := 0
	pending := userTokens
	for i, turn := range turns {
		seed := fmt.Sprintf("%s/%d", name, i)
		content := []any{thinkingBlock{"thinking", "", signature(seed)}}
		if turn.text != "" {
			content = append(content, textBlock{"text", turn.text, nil})
		}
		stop, results := "end_turn", nudgeTokens
		if len(turn.calls) > 0 {
			stop, results = "tool_use", 0
		}
		for j, c := range turn.calls {
			content = append(content, toolUseBlock{"tool_use", "toolu_01" + base58(fmt.Sprintf("%s/%d", seed, j), 22), c.name, c.input})
			switch c.name {
			case "search_resource_types":
				results += searchTokens
			case "get_resource_schema":
				results += schemaTokens
			default:
				results += submitTokens
			}
		}
		body, err := json.Marshal(content)
		if err != nil {
			return nil, err
		}
		out := 180 + len(body)/3
		u := simUsage{InputTokens: pending, OutputTokens: out, CacheReadInputTokens: cached, ServiceTier: "standard"}
		if i == 0 {
			u.CacheCreationInputTokens = prefixTokens
		} else {
			u.CacheReadInputTokens += prefixTokens
		}
		msgs = append(msgs, simMessage{
			ID: "msg_01" + base58(seed, 22), Type: "message", Role: "assistant", Model: planner.DefaultModel,
			Content: content, StopReason: stop, Usage: u,
		})
		cached += pending + out
		pending = results
	}
	data, err := json.MarshalIndent(struct {
		PromptVersion string       `json:"prompt_version"`
		PromptSHA256  string       `json:"prompt_sha256"`
		Turns         []simMessage `json:"turns"`
	}{version, fp, msgs}, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// signature is an opaque thinking signature derived from seed.
func signature(seed string) string {
	var raw []byte
	for i := 0; len(raw) < 180; i++ {
		sum := sha256.Sum256([]byte(fmt.Sprintf("signature/%s/%d", seed, i)))
		raw = append(raw, sum[:]...)
	}
	return "Eo" + base64.StdEncoding.EncodeToString(raw[:180])
}

// base58 returns n base58 characters derived from seed.
func base58(seed string, n int) string {
	const alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"
	sum := sha256.Sum256([]byte("id/" + seed))
	x := new(big.Int).SetBytes(sum[:])
	out := make([]byte, n)
	m := new(big.Int)
	for i := range out {
		x.DivMod(x, big.NewInt(58), m)
		out[i] = alphabet[m.Int64()]
	}
	return string(out)
}
