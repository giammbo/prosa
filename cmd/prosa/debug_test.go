package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/zclconf/go-cty/cty"

	"github.com/giammbo/prosa/internal/provider"
)

func TestFormatBlockSortedWithFlags(t *testing.T) {
	b := &provider.Block{
		Attributes: map[string]*provider.Attribute{
			"tags":     {Type: cty.Map(cty.String), Optional: true},
			"name":     {Type: cty.String, Required: true},
			"id":       {Type: cty.String, Computed: true},
			"password": {Type: cty.String, Optional: true, Sensitive: true, WriteOnly: true},
		},
		BlockTypes: map[string]*provider.NestedBlock{
			"setting": {
				Nesting:  provider.NestingSet,
				MinItems: 1,
				Block: &provider.Block{
					Attributes: map[string]*provider.Attribute{
						"value": {Type: cty.String, Optional: true},
					},
					BlockTypes: map[string]*provider.NestedBlock{},
				},
			},
		},
	}

	var buf bytes.Buffer
	provider.FormatBlock(&buf, b, provider.FormatOptions{})

	want := "" +
		"id  string  computed\n" +
		"name  string  required\n" +
		"password  string  optional sensitive write_only\n" +
		"tags  map(string)  optional\n" +
		"setting  block set  min 1\n" +
		"  value  string  optional\n"
	if got := buf.String(); got != want {
		t.Errorf("formatBlock output mismatch\n--- got ---\n%s--- want ---\n%s", got, want)
	}
}

func TestParseProviderSpec(t *testing.T) {
	p, v, err := parseProviderSpec("hashicorp/aws@6.69.1")
	if err != nil || p.Namespace != "hashicorp" || p.Name != "aws" || v != "6.69.1" {
		t.Errorf("got (%+v, %q, %v)", p, v, err)
	}
	p, v, err = parseProviderSpec("hashicorp/aws")
	if err != nil || p.Name != "aws" || v != "" {
		t.Errorf("no version: got (%+v, %q, %v)", p, v, err)
	}
	for _, bad := range []string{"aws", "/aws", "hashicorp/", "a/b/c", ""} {
		if _, _, err := parseProviderSpec(bad); err == nil {
			t.Errorf("parseProviderSpec(%q): want error", bad)
		}
	}
}

func TestRunUnknownSubcommandExitsWithUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"nope"}, strings.NewReader(""), &stdout, &stderr); code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "usage:") {
		t.Errorf("stderr lacks usage: %q", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout not empty: %q", stdout.String())
	}
}

func TestRunCancelledContextFailsPromptly(t *testing.T) {
	t.Setenv("PROSA_CACHE_DIR", t.TempDir()) // empty cache: Install must hit the (cancelled) network
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var stdout, stderr bytes.Buffer
	start := time.Now()
	code := run(ctx, []string{"debug", "schema", "hashicorp/aws@6.69.1", "aws_ecs_cluster"}, strings.NewReader(""), &stdout, &stderr)
	if code != 1 {
		t.Errorf("exit code = %d, want 1 (stderr %q)", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "context canceled") {
		t.Errorf("stderr = %q, want a context canceled error", stderr.String())
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("run took %s with a cancelled context", d)
	}
}
