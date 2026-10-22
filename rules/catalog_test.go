package rules

import (
	"os"
	"slices"
	"testing"
)

const catalogEnv = "PROSA_RULES_CATALOG"

// TestCatalogUpToDate checks rules/README.md against the built-in rules.
// Regenerate it with PROSA_RULES_CATALOG=1 go test ./rules/ -run TestCatalogUpToDate.
func TestCatalogUpToDate(t *testing.T) {
	rs, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	want := Catalog(rs)
	if os.Getenv(catalogEnv) == "1" {
		if err := os.WriteFile("README.md", []byte(want), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	got, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatalf("%v (run with %s=1 to write it)", err, catalogEnv)
	}
	if string(got) != want {
		t.Errorf("rules/README.md is out of date: run %s=1 go test ./rules/ -run TestCatalogUpToDate", catalogEnv)
	}
}

// awsTypes lists the hashicorp/aws resource types the built-in rules may name.
var awsTypes = []string{
	"aws_cloudwatch_log_group", "aws_db_instance", "aws_ecs_service",
	"aws_ecs_task_definition", "aws_iam_policy", "aws_iam_role",
	"aws_iam_role_policy", "aws_lb_listener", "aws_rds_cluster",
	"aws_rds_cluster_instance", "aws_security_group",
	"aws_security_group_rule", "aws_vpc_security_group_ingress_rule",
}

func TestBuiltinSet(t *testing.T) {
	rs, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"AWS-DATA-001", "AWS-ENC-001", "AWS-ENC-002", "AWS-IAM-001", "AWS-IAM-002",
		"AWS-LOG-001", "AWS-NET-001", "AWS-NET-002", "AWS-NET-003", "AWS-NET-004",
	}
	if got := ids(rs); !slices.Equal(got, want) {
		t.Fatalf("ids = %v, want %v", got, want)
	}
	for _, r := range rs {
		for _, ty := range r.AppliesTo {
			if !slices.Contains(awsTypes, ty) {
				t.Errorf("%s: unknown resource type %s", r.ID, ty)
			}
		}
	}
}
