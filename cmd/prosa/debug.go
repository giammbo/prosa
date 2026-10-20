package main

import (
	"context"
	"fmt"
	"io"
	"runtime"
	"strings"

	"github.com/giammbo/prosa/internal/provider"
	"github.com/giammbo/prosa/internal/registry"
)

// defaultMajor is the provider major used when no @version is given.
const defaultMajor = 6

func runDebug(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "schema" {
		if len(args) > 0 {
			fmt.Fprintf(stderr, "prosa: unknown debug subcommand %q\n", args[0])
		}
		usage(stderr)
		return 2
	}
	if len(args) != 3 {
		usage(stderr)
		return 2
	}
	p, version, err := parseProviderSpec(args[1])
	if err != nil {
		fmt.Fprintf(stderr, "prosa: %v\n", err)
		return 2
	}
	if err := debugSchema(ctx, p, version, args[2], stdout); err != nil {
		fmt.Fprintf(stderr, "prosa: %v\n", err)
		return 1
	}
	return 0
}

// parseProviderSpec parses "namespace/name" or "namespace/name@version".
// The version is empty when omitted.
func parseProviderSpec(spec string) (registry.Provider, string, error) {
	addr, version, _ := strings.Cut(spec, "@")
	ns, name, ok := strings.Cut(addr, "/")
	if !ok || ns == "" || name == "" || strings.Contains(name, "/") {
		return registry.Provider{}, "", fmt.Errorf("invalid provider %q: want namespace/name[@version]", spec)
	}
	return registry.Provider{Namespace: ns, Name: name}, version, nil
}

// debugSchema installs the provider, starts it and prints the schema of one
// resource type to w.
func debugSchema(ctx context.Context, p registry.Provider, version, resourceType string, w io.Writer) error {
	client, err := registry.NewClient()
	if err != nil {
		return err
	}
	goos, goarch := runtime.GOOS, runtime.GOARCH
	if version == "" {
		v, err := client.Latest(ctx, p, defaultMajor, goos, goarch)
		if err != nil {
			return err
		}
		version = v
	}
	inst, err := client.Install(ctx, p, version, goos, goarch)
	if err != nil {
		return err
	}
	prov, err := provider.Start(ctx, inst.Path)
	if err != nil {
		return err
	}
	defer prov.Close()
	schema, err := prov.Schema(ctx)
	if err != nil {
		return err
	}
	rs, ok := schema.Resources[resourceType]
	if !ok {
		return fmt.Errorf("resource type %q not found in %s/%s %s", resourceType, p.Namespace, p.Name, version)
	}
	provider.FormatBlock(w, rs.Block, provider.FormatOptions{KeepComputedDescriptions: true})
	return nil
}
