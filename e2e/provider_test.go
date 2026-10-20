package e2e

import (
	"context"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zclconf/go-cty/cty"

	"github.com/giammbo/prosa/internal/engine"
	"github.com/giammbo/prosa/internal/provider"
	"github.com/giammbo/prosa/internal/registry"
)

const (
	awsVersion  = "6.69.1"
	clusterType = "aws_ecs_cluster"
	clusterName = "prosa-m1"
)

func TestAWSProviderLifecycleOnMoto(t *testing.T) {
	RequireE2E(t)
	ctx, cancel := context.WithTimeout(context.Background(), 14*time.Minute)
	defer cancel()

	// 1. Install the real provider (cached after the first run).
	reg, err := registry.NewClient()
	if err != nil {
		t.Fatalf("registry client: %v", err)
	}
	inst, err := reg.Install(ctx, registry.Provider{Namespace: "hashicorp", Name: "aws"}, awsVersion, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if !slices.Contains(inst.Protocols, "5.0") {
		t.Fatalf("protocols %v do not include 5.0", inst.Protocols)
	}
	t.Logf("provider binary: %s (protocols %v)", inst.Path, inst.Protocols)

	// 2. Start and read the schema.
	p, err := provider.Start(ctx, inst.Path, engine.AWSProviderEnv(MotoURL())...)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(p.Close)
	schema, err := p.Schema(ctx)
	if err != nil {
		t.Fatalf("schema: %v", err)
	}
	rs, ok := schema.Resources[clusterType]
	if !ok {
		t.Fatalf("schema has no %s", clusterType)
	}
	if a := rs.Block.Attributes["name"]; a == nil || !a.Required {
		t.Fatalf("%s.name is not required: %+v", clusterType, a)
	}
	is, ok := schema.Identities[clusterType]
	if !ok {
		t.Fatalf("schema has no identity for %s", clusterType)
	}
	if !schema.PlanDestroy {
		t.Fatal("hashicorp/aws does not declare plan_destroy")
	}
	t.Logf("%s identity v%d: %s", clusterType, is.Version, is.Type.GoString())

	// 3. Configure against Moto.
	pcfg, err := AWSConfig(schema.Provider, MotoURL())
	if err != nil {
		t.Fatalf("aws config: %v", err)
	}
	if err := p.Configure(ctx, pcfg); err != nil {
		t.Fatalf("configure: %v", err)
	}

	clusterConfig := func(tags map[string]any) cty.Value {
		t.Helper()
		v, err := provider.ObjectFromMap(rs.Block, map[string]any{"name": clusterName, "tags": tags})
		if err != nil {
			t.Fatalf("cluster config: %v", err)
		}
		return v
	}
	null := cty.NullVal(rs.Block.ImpliedType())

	// 4. Create.
	cfg := clusterConfig(map[string]any{"prosa-intent": "m1"})
	if err := p.ValidateResource(ctx, clusterType, cfg); err != nil {
		t.Fatalf("validate: %v", err)
	}
	change, err := p.PlanResource(ctx, clusterType, null, cfg, nil, cty.NilVal)
	if err != nil {
		t.Fatalf("plan create: %v", err)
	}
	if change.Planned.GetAttr("arn").IsKnown() {
		t.Fatalf("planned arn is known on create: %#v", change.Planned.GetAttr("arn"))
	}
	res, err := p.ApplyResource(ctx, clusterType, null, change, cfg)
	if err != nil {
		if res.State != cty.NilVal && !res.State.IsNull() {
			_ = p.DeleteResource(context.Background(), clusterType, res.State, res.Private, res.Identity)
		}
		t.Fatalf("apply create: %v", err)
	}
	state, private, identity := res.State, res.Private, res.Identity
	deleted := false
	t.Cleanup(func() { // leave Moto clean if the test stops half-way
		if !deleted {
			_ = p.DeleteResource(context.Background(), clusterType, state, private, identity)
		}
	})
	id := stringAttr(t, state, "id")
	if id == "" {
		t.Fatal("created cluster has an empty id")
	}
	if arn := stringAttr(t, state, "arn"); !strings.Contains(arn, "cluster/"+clusterName) {
		t.Fatalf("arn %q does not contain cluster/%s", arn, clusterName)
	}
	if identity.IsNull() || !identity.Type().Equals(is.Type) {
		t.Fatalf("identity after create = %#v, want a non-null %s", identity, is.Type.GoString())
	}
	t.Logf("created %s id=%s identity=%#v", clusterType, id, identity)

	// 5. Read back, passing the identity.
	rr, err := p.ReadResource(ctx, clusterType, state, private, identity)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	state, private = rr.State, rr.Private
	if state.IsNull() {
		t.Fatal("read returned null right after create")
	}
	if rr.Identity.IsNull() || !rr.Identity.RawEquals(identity) {
		t.Fatalf("identity after read = %#v, want %#v", rr.Identity, identity)
	}
	identity = rr.Identity
	if got := stringAttr(t, state, "name"); got != clusterName {
		t.Fatalf("name = %q, want %q", got, clusterName)
	}
	if got, want := state.GetAttr("tags"), cfg.GetAttr("tags"); !got.RawEquals(want) {
		t.Fatalf("tags = %#v, want %#v", got, want)
	}

	// 6. Update tags in place, passing the identity through plan and apply.
	cfg = clusterConfig(map[string]any{"prosa-intent": "m1", "team": "platform"})
	change, err = p.PlanResource(ctx, clusterType, state, cfg, private, identity)
	if err != nil {
		t.Fatalf("plan update: %v", err)
	}
	if len(change.RequiresReplace) != 0 {
		t.Fatalf("tag update requires replace: %v", change.RequiresReplace)
	}
	if !change.Identity.RawEquals(identity) {
		t.Fatalf("planned identity = %#v, want %#v", change.Identity, identity)
	}
	res, err = p.ApplyResource(ctx, clusterType, state, change, cfg)
	if res.State != cty.NilVal && !res.State.IsNull() {
		state, private, identity = res.State, res.Private, res.Identity
	}
	if err != nil {
		t.Fatalf("apply update: %v", err)
	}
	if identity.IsNull() {
		t.Fatal("identity after update is null")
	}
	rr, err = p.ReadResource(ctx, clusterType, state, private, identity)
	if err != nil {
		t.Fatalf("read after update: %v", err)
	}
	state, private, identity = rr.State, rr.Private, rr.Identity
	if state.IsNull() {
		t.Fatal("read returned null after update")
	}
	tags := state.GetAttr("tags")
	if tags.IsNull() || !tags.Type().IsMapType() || !tags.HasIndex(cty.StringVal("team")).True() {
		t.Fatalf("tags after update = %#v", tags)
	}
	if team := tags.Index(cty.StringVal("team")).AsString(); team != "platform" {
		t.Fatalf(`tags["team"] = %q, want "platform"`, team)
	}
	if got := stringAttr(t, state, "id"); got != id {
		t.Fatalf("id changed on update: %q -> %q", id, got)
	}

	// 7. Delete (planned, then applied), then read must report the resource gone.
	if err := p.DeleteResource(ctx, clusterType, state, private, identity); err != nil {
		t.Fatalf("delete: %v", err)
	}
	deleted = true
	gone, err := p.ReadResource(ctx, clusterType, state, private, identity)
	if err != nil {
		t.Fatalf("read after delete: %v", err)
	}
	if !gone.State.IsNull() {
		t.Fatalf("read after delete returned %#v, want null", gone.State)
	}
}

func stringAttr(t *testing.T, v cty.Value, name string) string {
	t.Helper()
	a := v.GetAttr(name)
	if a.IsNull() || !a.IsKnown() {
		t.Fatalf("attribute %q is null or unknown", name)
	}
	return a.AsString()
}
