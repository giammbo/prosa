package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInitCommand(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "staging")
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"init", dir}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d (stderr %q)", code, stderr.String())
	}
	if want := "created .gitignore\ncreated envs/\n"; stdout.String() != want || stderr.Len() != 0 {
		t.Errorf("stdout = %q, stderr = %q; want %q", stdout.String(), stderr.String(), want)
	}
	for _, name := range []string{".gitignore", "envs"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}

	// The project it made plans from inside, with no argument.
	writeFile(t, filepath.Join(dir, "app.md"), "one thing")
	t.Chdir(dir)
	t.Setenv("PROSA_STATE_PASSPHRASE", "passphrase")
	stdout.Reset()
	stderr.Reset()
	if code := reconcile(context.Background(), "plan", nil, strings.NewReader(""), &stdout, &stderr, (&fakeProvider{}).source, (&fakePlanner{}).source); code != 0 {
		t.Fatalf("plan in the new project: exit %d (stderr %q)", code, stderr.String())
	}
	if !strings.HasPrefix(stderr.String(), "target: AWS account from current credentials (region eu-south-1)\n") ||
		!strings.Contains(stdout.String(), "  # app/thing will be created\n") {
		t.Errorf("plan: stdout %q, stderr %q", stdout.String(), stderr.String())
	}

	// Again, in the current directory: it leaves both and says so.
	stdout.Reset()
	stderr.Reset()
	if code := run(context.Background(), []string{"init"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Errorf("second init: exit %d, want 0", code)
	}
	if want := "kept .gitignore\nkept envs/\n"; stdout.String() != want || stderr.Len() != 0 {
		t.Errorf("second init: stdout %q, stderr %q; want %q", stdout.String(), stderr.String(), want)
	}
	if _, err := os.Stat(filepath.Join(dir, "prosa.yaml")); !os.IsNotExist(err) {
		t.Errorf("init wrote prosa.yaml (stat err %v)", err)
	}

	if code := run(context.Background(), []string{"init", "a", "b"}, strings.NewReader(""), &stdout, &stderr); code != 2 ||
		!strings.Contains(stderr.String(), "prosa init [<dir>]") {
		t.Errorf("init a b: exit %d, stderr %q; want 2 and the usage", code, stderr.String())
	}
}

func TestInitRefusesLegacyConfig(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "prosa.yaml"), "region: eu-south-1\n")
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"init", dir}, strings.NewReader(""), &stdout, &stderr); code != 2 ||
		!strings.Contains(stderr.String(), "prosa.yaml is no longer used") || stdout.Len() != 0 {
		t.Errorf("init: exit %d, stdout %q, stderr %q; want 2 and the legacy-config error", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(filepath.Join(dir, ".gitignore")); !os.IsNotExist(err) {
		t.Errorf("init touched the folder (stat err %v)", err)
	}
}
