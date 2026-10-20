package project

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestInitCreatesEnvs(t *testing.T) {
	root := filepath.Join(t.TempDir(), "new-project") // created by Init
	done, err := Init(root)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"created .gitignore", "created envs/"}; !reflect.DeepEqual(done, want) {
		t.Errorf("Init = %v, want %v", done, want)
	}
	if got := readFile(t, filepath.Join(root, ".gitignore")); got != ".prosa/\n" {
		t.Errorf(".gitignore = %q, want .prosa/", got)
	}
	if fi, err := os.Stat(filepath.Join(root, "envs")); err != nil || !fi.IsDir() {
		t.Errorf("envs/: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "prosa.yaml")); !os.IsNotExist(err) {
		t.Errorf("Init wrote prosa.yaml (stat err %v)", err)
	}
	if _, err := Open(root); err != nil {
		t.Errorf("the new project does not open: %v", err)
	}
}

// Running it again leaves both and says so.
func TestInitKeepsExisting(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("bin/\n.prosa/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "envs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "envs", "staging.md"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	done, err := Init(root)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"kept .gitignore", "kept envs/"}; !reflect.DeepEqual(done, want) {
		t.Errorf("Init = %v, want %v", done, want)
	}
	if got := readFile(t, filepath.Join(root, ".gitignore")); got != "bin/\n.prosa/\n" {
		t.Errorf(".gitignore = %q", got)
	}
	if got := readFile(t, filepath.Join(root, "envs", "staging.md")); got != "mine" {
		t.Errorf("envs/staging.md = %q", got)
	}
}

func TestInitAppendsToGitignore(t *testing.T) {
	for _, tc := range []struct{ before, after, done string }{
		{"bin/\n", "bin/\n.prosa/\n", "added .prosa/ to .gitignore"},
		{"bin/", "bin/\n.prosa/\n", "added .prosa/ to .gitignore"}, // no final newline
		{"bin/\n.prosa/\n", "bin/\n.prosa/\n", "kept .gitignore"},
		{".prosa\n", ".prosa\n", "kept .gitignore"},
		{"/.prosa/\n", "/.prosa/\n", "kept .gitignore"},
		{".prosa/*\n", ".prosa/*\n", "kept .gitignore"},
		{".prosa/**\n", ".prosa/**\n", "kept .gitignore"},
	} {
		root := t.TempDir()
		gi := filepath.Join(root, ".gitignore")
		if err := os.WriteFile(gi, []byte(tc.before), 0o644); err != nil {
			t.Fatal(err)
		}
		done, err := Init(root)
		if err != nil {
			t.Fatal(err)
		}
		if want := []string{tc.done, "created envs/"}; !reflect.DeepEqual(done, want) {
			t.Errorf("%q: Init = %v, want %v", tc.before, done, want)
		}
		if got := readFile(t, gi); got != tc.after {
			t.Errorf("%q: .gitignore = %q, want %q", tc.before, got, tc.after)
		}
	}
}
