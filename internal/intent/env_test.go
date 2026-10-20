package intent

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestLoadEnv(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	text := "# Staging\n\n- 40 GB di storage.\n"
	p := write("staging.md", text)
	env, err := LoadEnv(p)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(text))
	if env.Name != "staging" || env.Path != p || env.Body != strings.TrimSpace(text) || env.Hash != "sha256:"+hex.EncodeToString(sum[:]) {
		t.Errorf("env = %+v", env)
	}

	t.Chdir(dir)
	if env, err = LoadEnv("staging.md"); err != nil || env.Path != p {
		t.Errorf("relative path: %+v, %v; want Path %s", env, err, p)
	}

	for name, tc := range map[string]struct{ file, content, want string }{
		"frontmatter": {"prod.md", "---\nautonomy: auto\n---\ntext", "environment files have no frontmatter"},
		"upper case":  {"Staging.md", "text", "invalid environment name"},
		"no .md":      {"staging.txt", "text", "invalid environment name"},
		"missing":     {"nope.md", "", "no such file"},
	} {
		path := filepath.Join(dir, tc.file)
		if tc.content != "" {
			path = write(tc.file, tc.content)
		}
		if _, err := LoadEnv(path); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
}

func TestEnvSections(t *testing.T) {
	env, err := LoadEnv("../../examples/envs/staging.md")
	if err != nil {
		t.Fatal(err)
	}
	if c := env.Common(); !strings.Contains(c, "env=staging") || strings.Contains(c, "40 GB") || strings.HasPrefix(c, "#") {
		t.Errorf("Common() = %q", c)
	}
	if s := env.Section("database"); !strings.Contains(s, "40 GB") || !strings.Contains(s, "Postgres 18") || strings.Contains(s, "staging.acme") {
		t.Errorf("Section(database) = %q", s)
	}
	if s := env.Section("cron"); s == "" {
		t.Error("Section(cron) is empty")
	}
	if s := env.Section("queue"); s != "" {
		t.Errorf("Section(queue) = %q, want empty", s)
	}
}

func TestEnvNoSections(t *testing.T) {
	env := Env{Name: "dev", Body: "# Dev\n\nSmall everything.\nTag env=dev."}
	if c := env.Common(); c != "Small everything.\nTag env=dev." {
		t.Errorf("Common() = %q", c)
	}
	if s := env.Section("app"); s != "" {
		t.Errorf("Section(app) = %q, want empty", s)
	}
	if got := EnvFor(env, "app"); got != env.Common() {
		t.Errorf("EnvFor = %q", got)
	}
}

// A "## X" heading names an intent case-insensitively; one that names no
// intent of the project is reported (once, as written).
func TestEnvSectionHeadings(t *testing.T) {
	env := Env{Name: "staging", Body: "# Staging\n\nTag env=staging.\n\n## Database\n- 40 GB.\n\n## app\nTwo tasks."}
	names := []string{"app", "database"}
	if got := env.UnknownSections(names); len(got) != 0 {
		t.Fatalf("UnknownSections = %v, want none", got)
	}
	if s := env.Section("database"); s != "- 40 GB." {
		t.Errorf("Section(database) = %q, want the text under ## Database", s)
	}
	if s := EnvFor(env, "database"); !strings.Contains(s, "- 40 GB.") {
		t.Errorf("EnvFor(database) = %q", s)
	}
	bad := Env{Name: "staging", Body: "## Rete\n- a /16.\n\n## app\nTwo tasks.\n\n## Rete\nagain"}
	if got := bad.UnknownSections(names); !slices.Equal(got, []string{"Rete"}) {
		t.Fatalf("UnknownSections = %v, want [Rete]", got)
	}
}
