package engine

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"

	"github.com/zclconf/go-cty/cty"

	"github.com/giammbo/prosa/internal/provider"
	"github.com/giammbo/prosa/internal/registry"
)

// awsProvider is the only provider of M2, by its lock name.
const awsProvider = "hashicorp/aws"

// defaultMajor is the provider major installed when the lock pins no version.
const defaultMajor = 6

// DefaultAWSRegion is the region of an Engine without one when
// PROSA_AWS_REGION is not set.
const DefaultAWSRegion = "eu-south-1"

// AWSSettings is where hashicorp/aws operates (ADR-029). Endpoint, when
// set, is an emulated AWS. Credentials, profile, role and account are the
// ones of the AWS SDK's chain, which the provider process inherits: Prosa
// passes none of them.
type AWSSettings struct {
	Region, Endpoint string
}

// Provider is what the engine needs from a provider plugin. The method set is
// that of *provider.Provider.
type Provider interface {
	Schema(ctx context.Context) (*provider.Schema, error)
	ValidateResource(ctx context.Context, typeName string, config cty.Value) error
	PlanResource(ctx context.Context, typeName string, prior, config cty.Value, priorPrivate []byte, priorIdentity cty.Value) (provider.PlannedChange, error)
	ApplyResource(ctx context.Context, typeName string, prior cty.Value, change provider.PlannedChange, config cty.Value) (provider.ApplyResult, error)
	ReadResource(ctx context.Context, typeName string, current cty.Value, private []byte, identity cty.Value) (provider.ReadResult, error)
}

var _ Provider = (*provider.Provider)(nil)

// versioned is implemented by providers that know the version they run; the
// engine uses it when the lock pins no version yet.
type versioned interface {
	Version() string
}

// AWSProviderConfig builds the hashicorp/aws provider configuration of s:
// the region, nothing else about the identity (no allowed_account_ids,
// assume_role, profile or default_tags). With an endpoint, the checks that
// need real AWS are skipped (credentials validation, region validation, the
// EC2 metadata API) and, unless AWS_ACCESS_KEY_ID is set, the credentials
// are "test". The account of the credentials in use is still requested, as
// on real AWS (STS GetCallerIdentity, which an emulator such as Moto
// serves); Prosa never chooses an account. Without it the provider cannot
// tell its own account's resources from another's and reads back, for
// example, a security group rule's referenced group as "<account>/<group>",
// a drift at every plan. The endpoint itself reaches the provider process as
// AWS_ENDPOINT_URL (AWSProviderEnv), which the AWS SDK applies to every
// service. It is endpoint configuration, not a special path for one
// emulator. b is the provider's configuration schema.
func AWSProviderConfig(b *provider.Block, s AWSSettings) (map[string]any, error) {
	m := map[string]any{"region": s.Region}
	if s.Endpoint == "" {
		return m, nil
	}
	m["skip_credentials_validation"] = true
	m["skip_requesting_account_id"] = false
	m["skip_region_validation"] = true
	if os.Getenv("AWS_ACCESS_KEY_ID") == "" {
		m["access_key"] = "test"
		m["secret_key"] = "test"
	}
	meta, ok := b.Attributes["skip_metadata_api_check"]
	if !ok {
		return nil, fmt.Errorf("provider schema has no skip_metadata_api_check")
	}
	switch {
	case meta.Type.Equals(cty.String):
		m["skip_metadata_api_check"] = "true"
	case meta.Type.Equals(cty.Bool):
		m["skip_metadata_api_check"] = true
	default:
		return nil, fmt.Errorf("skip_metadata_api_check has unexpected type %s", meta.Type.FriendlyName())
	}
	return m, nil
}

// AWSProviderEnv is the environment change the hashicorp/aws process needs
// for endpoint, in the form provider.Start takes: with an endpoint,
// AWS_ENDPOINT_URL=endpoint; either way the host's AWS_ENDPOINT_URL_<SERVICE>
// variables are removed, and without an endpoint the host's AWS_ENDPOINT_URL
// too, so a stray host endpoint never redirects the provider.
func AWSProviderEnv(endpoint string) []string {
	return awsProviderEnv(endpoint, os.Environ())
}

func awsProviderEnv(endpoint string, host []string) []string {
	var out []string
	for _, kv := range host {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "AWS_ENDPOINT_URL_") || (k == "AWS_ENDPOINT_URL" && endpoint == "") {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	if endpoint != "" {
		out = append(out, "AWS_ENDPOINT_URL="+endpoint)
	}
	return out
}

// ProviderPool installs, starts and configures providers, one process per
// provider name, and stops them all on Close.
type ProviderPool struct {
	reg *registry.Client
	aws AWSSettings

	mu      sync.Mutex
	started map[string]*pooledProvider
}

// pooledProvider is a started provider and the version it runs.
type pooledProvider struct {
	*provider.Provider
	version string
}

// Version is the provider version the process runs.
func (p *pooledProvider) Version() string { return p.version }

// NewProviderPool returns an empty pool that installs from reg and
// configures hashicorp/aws with aws.
func NewProviderPool(reg *registry.Client, aws AWSSettings) *ProviderPool {
	return &ProviderPool{reg: reg, aws: aws, started: map[string]*pooledProvider{}}
}

// Get returns the provider name ("namespace/type") at version, starting and
// configuring it with the pool's AWSSettings the first time. An empty version
// resolves to the latest stable release of major 6; the returned provider's
// Version method reports the version used. Asking again for a name returns
// the same process; asking for another version of it is an error.
func (p *ProviderPool) Get(ctx context.Context, name, version string) (Provider, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if pp, ok := p.started[name]; ok {
		if version != "" && version != pp.version {
			return nil, fmt.Errorf("provider %s %s is already running, cannot also start %s", name, pp.version, version)
		}
		return pp, nil
	}
	if name != awsProvider {
		return nil, fmt.Errorf("unsupported provider %q (only %s)", name, awsProvider)
	}
	ns, typ, _ := strings.Cut(name, "/")
	rp := registry.Provider{Namespace: ns, Name: typ}
	goos, goarch := runtime.GOOS, runtime.GOARCH
	if version == "" {
		v, err := p.reg.Latest(ctx, rp, defaultMajor, goos, goarch)
		if err != nil {
			return nil, err
		}
		version = v
	}
	inst, err := p.reg.Install(ctx, rp, version, goos, goarch)
	if err != nil {
		return nil, err
	}
	prov, err := provider.Start(ctx, inst.Path, AWSProviderEnv(p.aws.Endpoint)...)
	if err != nil {
		return nil, err
	}
	if err := configureAWS(ctx, prov, p.aws); err != nil {
		prov.Close()
		return nil, fmt.Errorf("configure %s %s: %w", name, version, err)
	}
	pp := &pooledProvider{Provider: prov, version: version}
	p.started[name] = pp
	return pp, nil
}

// configureAWS configures hashicorp/aws with aws.
func configureAWS(ctx context.Context, prov *provider.Provider, aws AWSSettings) error {
	s, err := prov.Schema(ctx)
	if err != nil {
		return err
	}
	if s.Provider == nil {
		return fmt.Errorf("provider has no configuration schema")
	}
	m, err := AWSProviderConfig(s.Provider, aws)
	if err != nil {
		return err
	}
	cfg, err := provider.ObjectFromMap(s.Provider, m)
	if err != nil {
		return err
	}
	return prov.Configure(ctx, cfg)
}

// AWSRegion is the AWS region of an Engine without a Region:
// PROSA_AWS_REGION, DefaultAWSRegion if unset.
func AWSRegion() string {
	if r := os.Getenv("PROSA_AWS_REGION"); r != "" {
		return r
	}
	return DefaultAWSRegion
}

// Close stops every provider the pool started. The pool can be used again.
func (p *ProviderPool) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for name, pp := range p.started {
		pp.Close()
		delete(p.started, name)
	}
}
