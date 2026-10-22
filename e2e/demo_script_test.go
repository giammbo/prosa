package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestDemoScriptOnMoto runs demo/moto/run.sh, the demo script on the real
// binary, with the simulated planner and every approval answered "yes"
// (DEMO_AUTO=1): the lines that matter of each step are in its output, it
// exits 0, and Moto is clean at the end.
func TestDemoScriptOnMoto(t *testing.T) {
	RequireE2E(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	defer cancel()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs(demoScriptPath)
	if err != nil {
		t.Fatal(err)
	}
	project := filepath.Join(t.TempDir(), "demo")
	cmd := exec.CommandContext(ctx, bash, script)
	// The script's own pause and planner switches must not leak in from the
	// environment of the test run.
	env := slices.DeleteFunc(os.Environ(), func(kv string) bool {
		return strings.HasPrefix(kv, "DEMO_") || strings.HasPrefix(kv, "PROSA_PLANNER=")
	})
	cmd.Env = append(env, "DEMO_AUTO=1", "DEMO_DIR="+project, "PROSA_MOTO_URL="+MotoURL(), "PROSA_PLANNER=fixtures")
	out, err := cmd.CombinedOutput()
	text := string(out)
	t.Logf("output of the script:\n%s", text)
	if err != nil {
		t.Fatalf("run.sh: %v", err)
	}

	// In order: the title of each step, and its key lines.
	wants := []string{
		"=== Step 1: ", "target: emulated endpoint " + MotoURL(), "Prosa will perform the following actions:", "will be created", "Plan: ",
		"=== Step 2: ", "will be created", "Plan: ", "database/db.instance: Creating...", "database/db.instance: Creation complete after ", "Apply complete! Resources: ",
		"$ aws --endpoint-url " + MotoURL(), "rds describe-db-instances", "check database: pass",
		"=== Step 3: ", "Blocked:", "There are exactly two healthy replicas", "~ desired_count = 1 -> 2", "Modifications complete after ",
		"Apply complete! Resources: 0 added, 1 changed, 0 destroyed.",
		"=== Step 4: ", "# (needs approval: destroy)", "Destruction complete after ", "Apply complete! Resources: 0 added, 0 changed, ",
		"=== Step 5: ", "$ prosa apply", "lock identical.", "state store identical.",
		"=== Step 6: ", "# (drifted outside Prosa: allocated_storage)", "Apply complete! Resources: 0 added, 1 changed, 0 destroyed.",
		"=== Step 7: ", "=== Done. Event log: " + project + "/.prosa/events/staging.jsonl",
	}
	rest := text
	for _, w := range wants {
		i := strings.Index(rest, w)
		if i < 0 {
			t.Fatalf("output lacks %q (in order) after the previous line", w)
		}
		rest = rest[i+len(w):]
	}
	if got := strings.Count(text, "No changes."); got != 4 {
		t.Errorf("\"No changes.\" appears %d times, want 4 (three apply reruns, after the drift)", got)
	}
	if _, err := os.Stat(filepath.Join(project, ".prosa", "events", "staging.jsonl")); err != nil {
		t.Errorf("event log: %v", err)
	}

	wantMotoClean(ctx, t)
}

// wantMotoClean asserts that Moto holds nothing of the demo or of the
// environments tests, in the regions they use: the resources of both
// environments are named with their prefix (staging-, prod-).
func wantMotoClean(ctx context.Context, t *testing.T) {
	t.Helper()
	for _, region := range []string{motoRegion, prodRegion} {
		wantMotoCleanIn(ctx, t, region)
	}
}

func wantMotoCleanIn(ctx context.Context, t *testing.T, region string) {
	t.Helper()
	for _, q := range [][]string{
		{"rds", "describe-db-instances", "--query", "DBInstances[].DBInstanceIdentifier"},
		// Moto keeps a deleted cluster listed, INACTIVE.
		{"ecs", "describe-clusters", "--clusters", "staging-web", "staging-cron", "prod-web", "prod-cron", "--query", "clusters[?status=='ACTIVE'].clusterName"},
		{"scheduler", "list-schedules", "--query", "Schedules[].Name"},
		{"elbv2", "describe-load-balancers", "--query", "LoadBalancers[].LoadBalancerName"},
		{"ec2", "describe-vpcs", "--filters", "Name=is-default,Values=false", "--query", "Vpcs[].VpcId"},
		{"ec2", "describe-addresses", "--query", "Addresses[].AllocationId"},
		{"ec2", "describe-nat-gateways", "--query", "NatGateways[?State!='deleted'].NatGatewayId"},
		{"ec2", "describe-security-groups", "--query", "SecurityGroups[?GroupName!='default'].GroupId"},
		{"rds", "describe-db-subnet-groups", "--query", "DBSubnetGroups[].DBSubnetGroupName"},
		{"elbv2", "describe-target-groups", "--query", "TargetGroups[].TargetGroupName"},
		{"events", "list-rules", "--query", "Rules[].Name"},
		{"acm", "list-certificates", "--query", "CertificateSummaryList[].DomainName"},
		{"secretsmanager", "list-secrets", "--query", "SecretList[].Name"},
		{"sns", "list-topics", "--query", "Topics[].TopicArn"},
		{"logs", "describe-log-groups", "--query", "logGroups[].logGroupName"},
		{"iam", "list-roles", "--query", "Roles[?starts_with(RoleName, `staging`) || starts_with(RoleName, `prod`)].RoleName"},
	} {
		got, err := awsOutIn(ctx, t, region, append(q, "--output", "json")...)
		if err != nil {
			t.Errorf("aws %s (%s): %v\n%s", q[0], region, err, got)
		} else if strings.ReplaceAll(strings.Join(strings.Fields(got), ""), "\n", "") != "[]" {
			t.Errorf("Moto is not clean in %s, aws %s %s: %s", region, q[0], q[1], got)
		}
	}
}
