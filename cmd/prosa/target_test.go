package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/giammbo/prosa/internal/engine"
)

// afterTarget returns stderr without its first line, which must be the
// target line.
func afterTarget(t *testing.T, stderr string) string {
	t.Helper()
	line, rest, _ := strings.Cut(stderr, "\n")
	if !strings.HasPrefix(line, "target: ") {
		t.Fatalf("stderr = %q, want the target line first", stderr)
	}
	return rest
}

// Every command that can reach a cloud says first where it points.
func TestCommandsPrintTargetFirst(t *testing.T) {
	dir, fx := gatedProject(t, "---\nautonomy: auto\n---\none thing")
	t.Setenv("PROSA_REQUIRE_ENDPOINT", "")
	for _, tc := range []struct {
		endpoint, region, want string
	}{
		{"http://localhost:5555", "eu-west-1", "target: emulated endpoint http://localhost:5555 (region eu-west-1)\n"},
		{"", "", "target: AWS account from current credentials (region eu-south-1)\n"},
	} {
		t.Setenv("PROSA_AWS_ENDPOINT", tc.endpoint)
		t.Setenv("PROSA_AWS_REGION", tc.region)
		for _, cmd := range []string{"plan", "apply", "check", "recover"} {
			fake := &fakeProvider{}
			var stdout, stderr bytes.Buffer
			var code int
			switch cmd {
			case "plan", "apply":
				code = reconcile(context.Background(), cmd, []string{"--fixtures", fx, dir}, strings.NewReader(""), &stdout, &stderr, fake.source, noPlanner)
			case "check":
				code = checkCommand(context.Background(), []string{dir}, &stdout, &stderr, fake.source)
			case "recover":
				code = recoverCommand([]string{dir}, &stdout, &stderr)
			}
			if code != 0 {
				t.Fatalf("%s (endpoint %q): exit %d (stderr %q)", cmd, tc.endpoint, code, stderr.String())
			}
			if !strings.HasPrefix(stderr.String(), tc.want) {
				t.Errorf("%s (endpoint %q): stderr = %q, want it to start with %q", cmd, tc.endpoint, stderr.String(), tc.want)
			}
		}
	}
}

// --require-endpoint, or PROSA_REQUIRE_ENDPOINT=1, refuses to run against
// real AWS: exit 2, nothing planned, applied or written.
func TestRequireEndpointRefusesRealAWS(t *testing.T) {
	dir, fx := gatedProject(t, "---\nautonomy: auto\n---\none thing")
	root := dir
	t.Setenv("PROSA_AWS_ENDPOINT", "")
	for _, env := range []string{"", "1"} {
		t.Setenv("PROSA_REQUIRE_ENDPOINT", env)
		flags := []string{"--require-endpoint"}
		if env == "1" {
			flags = nil
		}
		for _, args := range [][]string{
			append(append([]string{"plan"}, flags...), "--fixtures", fx, dir),
			append(append([]string{"apply", "--yes"}, flags...), "--fixtures", fx, dir),
			append(append([]string{"check"}, flags...), dir),
			append(append([]string{"recover"}, flags...), dir),
		} {
			fake := &fakeProvider{}
			var stdout, stderr bytes.Buffer
			var code int
			switch args[0] {
			case "plan", "apply":
				code = reconcile(context.Background(), args[0], args[1:], strings.NewReader(""), &stdout, &stderr, fake.source, noPlanner)
			case "check":
				code = checkCommand(context.Background(), args[1:], &stdout, &stderr, fake.source)
			case "recover":
				code = recoverCommand(args[1:], &stdout, &stderr)
			}
			if code != 2 {
				t.Errorf("%v (env %q): exit %d, want 2 (stderr %q)", args, env, code, stderr.String())
			}
			if want := "prosa: no emulated endpoint: PROSA_AWS_ENDPOINT is not set, and an endpoint is required (--require-endpoint or PROSA_REQUIRE_ENDPOINT=1)\n"; stderr.String() != want {
				t.Errorf("%v (env %q): stderr = %q, want %q", args, env, stderr.String(), want)
			}
			if fake.applies != 0 || stdout.Len() != 0 {
				t.Errorf("%v (env %q): ran anyway: applies %d, stdout %q", args, env, fake.applies, stdout.String())
			}
		}
	}
	for _, path := range []string{"prosa.lock.yaml", ".prosa"} {
		if matches, _ := filepath.Glob(filepath.Join(root, path)); len(matches) != 0 {
			t.Errorf("a refused command wrote %s", path)
		}
	}

	// With an endpoint set, the flag lets the command run.
	t.Setenv("PROSA_REQUIRE_ENDPOINT", "1")
	t.Setenv("PROSA_AWS_ENDPOINT", "http://localhost:5555")
	fake := &fakeProvider{}
	var stdout, stderr bytes.Buffer
	if code := reconcile(context.Background(), "plan", []string{"--require-endpoint", "--fixtures", fx, dir}, strings.NewReader(""), &stdout, &stderr, fake.source, noPlanner); code != 0 {
		t.Fatalf("plan with an endpoint: exit %d (stderr %q)", code, stderr.String())
	}
	if fake.aws.Endpoint != "http://localhost:5555" {
		t.Errorf("providers configured with endpoint %q, want the one of PROSA_AWS_ENDPOINT", fake.aws.Endpoint)
	}
}

// The target line has two forms: the emulated endpoint, or the AWS account
// of the current credentials (which Prosa does not know), with the region.
func TestTargetLine(t *testing.T) {
	for _, tc := range []struct {
		aws  engine.AWSSettings
		want string
	}{
		{engine.AWSSettings{Region: "eu-west-1"}, "target: AWS account from current credentials (region eu-west-1)"},
		{engine.AWSSettings{Region: "eu-west-1", Endpoint: "http://localhost:5555"}, "target: emulated endpoint http://localhost:5555 (region eu-west-1)"},
	} {
		if got := targetLine(tc.aws); got != tc.want {
			t.Errorf("targetLine(%+v) = %q, want %q", tc.aws, got, tc.want)
		}
	}
}

// The target line never shows userinfo or a query string of the endpoint.
func TestTargetLineRedactsEndpoint(t *testing.T) {
	var stderr bytes.Buffer
	f := reconcileFlags{aws: engine.AWSSettings{Region: "eu-west-1", Endpoint: "http://user:secret@localhost:5555/?token=abc"}}
	if err := announceTarget(f, &stderr); err != nil {
		t.Fatal(err)
	}
	if want := "target: emulated endpoint http://localhost:5555/ (region eu-west-1)\n"; stderr.String() != want {
		t.Errorf("stderr = %q, want %q", stderr.String(), want)
	}
}
