// Package e2e holds the end-to-end tests that drive real providers against
// Moto. They run only with PROSA_E2E=1; start Moto with
// `docker compose -f e2e/compose.yaml up -d`.
package e2e

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/zclconf/go-cty/cty"

	"github.com/giammbo/prosa/internal/engine"
	"github.com/giammbo/prosa/internal/provider"
)

const (
	defaultMotoURL = "http://localhost:5555"
	motoRegion     = "eu-south-1"
)

// MotoURL is the Moto endpoint: PROSA_MOTO_URL, or http://localhost:5555.
func MotoURL() string {
	if v := os.Getenv("PROSA_MOTO_URL"); v != "" {
		return strings.TrimRight(v, "/")
	}
	return defaultMotoURL
}

// RequireE2E skips the test unless PROSA_E2E=1. Once the run is opted in, a
// Moto that does not answer fails the test: an opted-in run never passes
// silently.
func RequireE2E(t *testing.T) {
	t.Helper()
	if os.Getenv("PROSA_E2E") != "1" {
		t.Skip("end-to-end test: set PROSA_E2E=1 to run it")
	}
	if err := checkMoto(MotoURL()); err != nil {
		t.Fatalf("PROSA_E2E=1 but %v (start it with `docker compose -f e2e/compose.yaml up -d`)", err)
	}
}

// checkMoto reports whether Moto answers at url.
func checkMoto(url string) error {
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get(url + "/moto-api/")
	if err != nil {
		return fmt.Errorf("moto is not reachable at %s: %w", url, err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("moto at %s answered %s", url, resp.Status)
	}
	return nil
}

// AWSConfig builds the hashicorp/aws provider configuration for endpoint, as
// the engine does (engine.AWSProviderConfig): fake credentials unless
// AWS_ACCESS_KEY_ID is set, every AWS-side check off. The endpoint itself
// goes in the process environment: start the provider with
// engine.AWSProviderEnv(endpoint).
func AWSConfig(b *provider.Block, endpoint string) (cty.Value, error) {
	m, err := engine.AWSProviderConfig(b, engine.AWSSettings{Region: motoRegion, Endpoint: endpoint})
	if err != nil {
		return cty.NilVal, err
	}
	return provider.ObjectFromMap(b, m)
}
