package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMain removes from the environment the variables that point the
// commands at an emulator or a region, so that the tests do not depend on
// the shell (the e2e script exports them), gives the AWS CLI a config file
// with the region eu-south-1, and stops the clock of the progress lines so
// that their durations are always 0s.
func TestMain(m *testing.M) {
	for _, k := range []string{"PROSA_REQUIRE_ENDPOINT", "PROSA_AWS_ENDPOINT", "PROSA_AWS_REGION", "AWS_REGION", "AWS_DEFAULT_REGION", "AWS_PROFILE"} {
		os.Unsetenv(k)
	}
	dir, err := os.MkdirTemp("", "prosa-aws-config")
	if err != nil {
		panic(err)
	}
	cfg := filepath.Join(dir, "config")
	if err := os.WriteFile(cfg, []byte("[default]\nregion = eu-south-1\n"), 0o644); err != nil {
		panic(err)
	}
	os.Setenv("AWS_CONFIG_FILE", cfg)
	progressClock = newFakeClock()
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func TestVersionCommand(t *testing.T) {
	old := version
	version = "1.2.3"
	defer func() { version = old }()
	for _, arg := range []string{"version", "--version"} {
		var out, errb bytes.Buffer
		if code := run(context.Background(), []string{arg}, strings.NewReader(""), &out, &errb); code != 0 {
			t.Fatalf("%s: code %d, stderr %q", arg, code, errb.String())
		}
		if got := out.String(); got != "prosa 1.2.3\n" {
			t.Errorf("%s: got %q", arg, got)
		}
	}
}
