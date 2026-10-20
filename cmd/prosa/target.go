package main

import (
	"fmt"
	"io"
	"os"

	"github.com/giammbo/prosa/internal/engine"
	"github.com/giammbo/prosa/internal/events"
)

// errNoEndpoint is the refusal of --require-endpoint without an endpoint.
const errNoEndpoint = "prosa: no emulated endpoint: PROSA_AWS_ENDPOINT is not set, and an endpoint is required (--require-endpoint or PROSA_REQUIRE_ENDPOINT=1)"

// announceTarget writes on stderr, before anything else, where the command
// points (targetLine). With --require-endpoint (or PROSA_REQUIRE_ENDPOINT=1)
// and no endpoint (PROSA_AWS_ENDPOINT), it refuses instead,
// with errUsage (exit code 2).
func announceTarget(f reconcileFlags, stderr io.Writer) error {
	if f.aws.Endpoint == "" && (f.requireEndpoint || os.Getenv("PROSA_REQUIRE_ENDPOINT") == "1") {
		fmt.Fprintln(stderr, errNoEndpoint)
		return errUsage
	}
	fmt.Fprintln(stderr, targetLine(f.aws))
	return nil
}

// targetLine says where aws points: the emulated endpoint, or the AWS
// account of the current credentials, and the region.
func targetLine(aws engine.AWSSettings) string {
	if aws.Endpoint != "" {
		return fmt.Sprintf("target: emulated endpoint %s (region %s)", events.RedactEndpoint(aws.Endpoint), aws.Region)
	}
	return "target: AWS account from current credentials (region " + aws.Region + ")"
}
