// Package provider starts a Terraform/OpenTofu provider plugin and talks to it
// over gRPC (protocol 5), without any HCL or Terraform core.
package provider

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"

	"github.com/giammbo/prosa/internal/tfplugin5"
)

const (
	magicCookieKey   = "TF_PLUGIN_MAGIC_COOKIE"
	magicCookieValue = "d602bf8f470bc67ca7faa0386276bbdd4330efaf76d1a219cb4d6991ca9872b2"
	maxRecvMsgSize   = 256 << 20 // the AWS schema is far larger than the 4 MiB default
	startTimeout     = 10 * time.Second
	stopTimeout      = 5 * time.Second
)

// Provider is a running provider plugin process.
type Provider struct {
	client tfplugin5.ProviderClient
	plugin *plugin.Client
	// killProc cancels the context the process was started with, which
	// makes os/exec kill it; nil in tests that use a fake client.
	killProc context.CancelFunc

	mu     sync.Mutex
	schema *Schema
	closed bool
}

// grpcPlugin adapts the provider's gRPC service to go-plugin.
type grpcPlugin struct {
	plugin.NetRPCUnsupportedPlugin
}

func (grpcPlugin) GRPCClient(_ context.Context, _ *plugin.GRPCBroker, c *grpc.ClientConn) (interface{}, error) {
	return tfplugin5.NewProviderClient(c), nil
}

func (grpcPlugin) GRPCServer(*plugin.GRPCBroker, *grpc.Server) error {
	return errors.New("prosa only acts as a plugin client")
}

// Start launches the provider binary at path and connects to it. It fails
// within a few seconds if the binary is not a protocol 5 plugin. Cancelling
// ctx while Start is in flight kills the process and returns promptly; ctx
// does not bound the provider's lifetime after Start returns (use Close).
// env ("KEY=VALUE") is added to the process environment, replacing the
// host's value of the same keys; a bare "KEY" removes the host's value (see
// pluginEnv).
func Start(ctx context.Context, path string, env ...string) (*Provider, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// The process gets its own context: go-plugin holds its lock for the whole
	// handshake, so Kill cannot interrupt a hanging start, but cancelling this
	// context makes os/exec kill the process and the handshake fail at once.
	procCtx, killProc := context.WithCancel(context.Background())
	cmd := exec.CommandContext(procCtx, path)
	cmd.Env = pluginEnv(os.Environ(), env)
	pc := plugin.NewClient(&plugin.ClientConfig{
		HandshakeConfig: plugin.HandshakeConfig{
			ProtocolVersion:  5,
			MagicCookieKey:   magicCookieKey,
			MagicCookieValue: magicCookieValue,
		},
		VersionedPlugins: map[int]plugin.PluginSet{
			5: {"provider": grpcPlugin{}},
		},
		Cmd:              cmd,
		SkipHostEnv:      true, // cmd.Env already has the host's, filtered
		AllowedProtocols: []plugin.Protocol{plugin.ProtocolGRPC},
		AutoMTLS:         true,
		Managed:          true,
		StartTimeout:     startTimeout,
		GRPCDialOptions: []grpc.DialOption{
			grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(maxRecvMsgSize)),
		},
		Logger: newLogger(os.Stderr, path),
	})

	type result struct {
		client tfplugin5.ProviderClient
		err    error
	}
	ch := make(chan result, 1)
	go func() {
		rpc, err := pc.Client()
		if err != nil {
			ch <- result{err: err}
			return
		}
		raw, err := rpc.Dispense("provider")
		if err != nil {
			ch <- result{err: err}
			return
		}
		c, ok := raw.(tfplugin5.ProviderClient)
		if !ok {
			ch <- result{err: fmt.Errorf("unexpected plugin type %T", raw)}
			return
		}
		ch <- result{client: c}
	}()

	select {
	case r := <-ch:
		if r.err != nil {
			pc.Kill()
			killProc()
			return nil, fmt.Errorf("start provider %s: %w", path, r.err)
		}
		return &Provider{client: r.client, plugin: pc, killProc: killProc}, nil
	case <-ctx.Done():
		killProc() // kills the process if it is running, or keeps it from starting
		<-ch       // wait for go-plugin's Start to return, so Kill sees the process
		pc.Kill()
		return nil, fmt.Errorf("start provider %s: %w", path, ctx.Err())
	}
}

// Close asks the provider to stop, then terminates the process. Idempotent.
func (p *Provider) Close() {
	if p == nil {
		return
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	p.mu.Unlock()

	if p.client != nil {
		ctx, cancel := context.WithTimeout(context.Background(), stopTimeout)
		_, _ = p.client.Stop(ctx, &tfplugin5.Stop_Request{})
		cancel()
	}
	if p.plugin != nil {
		p.plugin.Kill()
	}
	if p.killProc != nil {
		p.killProc()
	}
}
