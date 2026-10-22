package rules

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func synth() []Rule {
	return []Rule{
		{ID: "AWS-NET-009", AppliesTo: []string{"aws_s3_bucket"}, Body: "b", Origin: "prosa"},
		{ID: "AWS-NET-001", AppliesTo: []string{"aws_db_instance", "aws_security_group"}, Body: "a", Origin: "prosa"},
	}
}

func ids(rs []Rule) []string {
	var out []string
	for _, r := range rs {
		out = append(out, r.ID)
	}
	return out
}

func TestLoadProject(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "rules", "team")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	rule := strings.NewReplacer("AWS-NET-001", "ACME-NET-001").Replace(net001)
	files := map[string]string{
		filepath.Join(dir, "ACME-NET-001.md"):        rule,
		filepath.Join(root, "rules", "README.md"):    "# Not a rule\n",
		filepath.Join(root, "rules", "disabled.txt"): "# off\nAWS-NET-001\n\n",
	}
	for p, c := range files {
		if err := os.WriteFile(p, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	rs, disabled, err := LoadProject(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 1 || rs[0].ID != "ACME-NET-001" || rs[0].Origin != "rules/team/ACME-NET-001.md" {
		t.Errorf("rules = %+v", rs)
	}
	if !slices.Equal(disabled, []string{"AWS-NET-001"}) {
		t.Errorf("disabled = %v", disabled)
	}

	rs, disabled, err = LoadProject(t.TempDir())
	if rs != nil || disabled != nil || err != nil {
		t.Errorf("no rules dir: %v %v %v", rs, disabled, err)
	}
}

func TestNewSet(t *testing.T) {
	s, err := NewSet(synth(), nil, []string{"AWS-NET-001"}, []string{"aws-net-002"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(s.IDs(), []string{"AWS-NET-009"}) {
		t.Errorf("IDs = %v", s.IDs())
	}
	if len(s.Disabled) != 1 || s.Disabled[0] != (DisabledRule{ID: "AWS-NET-001", By: "rules/disabled.txt"}) {
		t.Errorf("Disabled = %+v", s.Disabled)
	}
	if !slices.Equal(s.Warnings, []string{"unknown rule AWS-NET-002 in --disable-rule"}) {
		t.Errorf("Warnings = %v", s.Warnings)
	}
}

func TestNewSetDuplicate(t *testing.T) {
	dup := Rule{ID: "AWS-NET-001", AppliesTo: []string{"x"}, Body: "z", Origin: "rules/team/AWS-NET-001.md"}
	_, err := NewSet(synth(), []Rule{dup}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "rules/team/AWS-NET-001.md") {
		t.Errorf("err = %v", err)
	}
}

func TestSetFor(t *testing.T) {
	s, err := NewSet(synth(), nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(s.For([]string{"aws_db_instance"})); !slices.Equal(got, []string{"AWS-NET-001"}) {
		t.Errorf("For db = %v", got)
	}
	if got := s.For([]string{"aws_lambda_function"}); len(got) != 0 {
		t.Errorf("For lambda = %v", got)
	}
	if got := ids(s.For([]string{"aws_s3_bucket", "aws_db_instance"})); !slices.Equal(got, []string{"AWS-NET-001", "AWS-NET-009"}) {
		t.Errorf("For both = %v", got)
	}
}

func TestLoadProjectMalformed(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "rules"), 0o755); err != nil {
		t.Fatal(err)
	}
	bad := "---\nid: ACME-NET-001\ntitle: t\napplies_to: [a]\nseverity: high\n---\nbody\n"
	if err := os.WriteFile(filepath.Join(root, "rules", "bad.md"), []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := LoadProject(root)
	if err == nil || !strings.Contains(err.Error(), "rules/bad.md") {
		t.Errorf("err = %v", err)
	}
}
