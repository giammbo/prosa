package rules

import (
	"slices"
	"strings"
	"testing"
)

const net001 = `---
id: AWS-NET-001
title: A database accepts traffic only from its clients
applies_to: [aws_security_group, aws_vpc_security_group_ingress_rule, aws_db_instance]
source:
  - label: AWS Well-Architected, SEC05-BP02
    url: https://docs.aws.amazon.com/wellarchitected/latest/security-pillar/sec_network_protection_layered.html
---
The database's security group allows its port only from the security groups
of the workloads that use it, referenced by their id. Never from a CIDR: not
0.0.0.0/0, not the VPC, not a subnet.
`

func TestParseRule(t *testing.T) {
	r, err := Parse("prosa", []byte(net001))
	if err != nil {
		t.Fatal(err)
	}
	if r.ID != "AWS-NET-001" || r.Title != "A database accepts traffic only from its clients" || r.Origin != "prosa" {
		t.Errorf("rule = %+v", r)
	}
	want := []string{"aws_security_group", "aws_vpc_security_group_ingress_rule", "aws_db_instance"}
	if !slices.Equal(r.AppliesTo, want) {
		t.Errorf("AppliesTo = %v", r.AppliesTo)
	}
	if len(r.Sources) != 1 || !strings.Contains(r.Sources[0].URL, "sec_network_protection_layered") || !strings.Contains(r.Sources[0].Label, "SEC05-BP02") {
		t.Errorf("Sources = %+v", r.Sources)
	}
	if !strings.HasPrefix(r.Body, "The database's security group") || strings.HasSuffix(r.Body, "\n") {
		t.Errorf("Body = %q", r.Body)
	}
}

func TestParseRuleErrors(t *testing.T) {
	cases := map[string]string{
		"no front matter": "Just prose.\n",
		"unterminated":    "---\nid: AWS-NET-001\n",
		"unknown key":     "---\nid: AWS-NET-001\ntitle: t\napplies_to: [a]\nseverity: high\n---\nbody\n",
		"bad id":          "---\nid: aws-net-1\ntitle: t\napplies_to: [a]\n---\nbody\n",
		"no applies_to":   "---\nid: AWS-NET-001\ntitle: t\napplies_to: []\n---\nbody\n",
		"empty body":      "---\nid: AWS-NET-001\ntitle: t\napplies_to: [a]\n---\n\n",
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse("rules/bad.md", []byte(in))
			if err == nil || !strings.Contains(err.Error(), "rules/bad.md") {
				t.Errorf("err = %v, want one naming rules/bad.md", err)
			}
		})
	}
}

func TestBuiltin(t *testing.T) {
	rs, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range rs {
		if r.Origin != "prosa" || len(r.Sources) == 0 {
			t.Errorf("%s: origin %q, %d sources", r.ID, r.Origin, len(r.Sources))
		}
		found = found || r.ID == "AWS-NET-001"
	}
	if !found {
		t.Error("AWS-NET-001 not in Builtin()")
	}
}

func TestHash(t *testing.T) {
	a := Rule{ID: "AWS-NET-001", AppliesTo: []string{"x"}, Body: "one"}
	b := Rule{ID: "AWS-NET-009", AppliesTo: []string{"y"}, Body: "two"}
	h := Hash([]Rule{a, b})
	if h != Hash([]Rule{b, a}) {
		t.Error("hash depends on input order")
	}
	b.Body = "three"
	if h == Hash([]Rule{a, b}) {
		t.Error("hash ignores Body")
	}
	if !strings.HasPrefix(Hash(nil), "sha256:") || Hash(nil) == "" {
		t.Errorf("Hash(nil) = %q", Hash(nil))
	}
}

func TestExceptions(t *testing.T) {
	p := "AWS-NET-001 does not apply here: the VPN 10.8.0.0/16 must reach the database."
	text := "Some intro.\n\n" + p + "\n\nAWS-ENC-009 is not needed; AWS-ENC-009 again.\n\nNothing here.\n"
	ex, unknown := Exceptions(text, []string{"AWS-NET-001"})
	if len(ex) != 1 || ex[0].ID != "AWS-NET-001" || ex[0].Text != p {
		t.Errorf("ex = %+v", ex)
	}
	if !slices.Equal(unknown, []string{"AWS-ENC-009"}) {
		t.Errorf("unknown = %v", unknown)
	}
}

func TestIDWithDigitInArea(t *testing.T) {
	in := strings.Replace(net001, "AWS-NET-001", "AWS-S3-001", 1)
	r, err := Parse("prosa", []byte(in))
	if err != nil || r.ID != "AWS-S3-001" {
		t.Fatalf("Parse = %+v, %v", r, err)
	}
	ex, unknown := Exceptions("AWS-S3-001 does not apply here.", []string{"AWS-S3-001"})
	if len(ex) != 1 || ex[0].ID != "AWS-S3-001" || len(unknown) != 0 {
		t.Errorf("ex = %+v, unknown = %v", ex, unknown)
	}
}
