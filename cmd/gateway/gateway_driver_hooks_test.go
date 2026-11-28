package main

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Project-Helianthus/helianthus-gateway/internal/runtimecontrol"
	"github.com/Project-Helianthus/helianthus-gateway/portal/contributionv1"
	"github.com/Project-Helianthus/helianthus-semreg/semreg/runtime/life"
	"github.com/Project-Helianthus/helianthus-semreg/semreg/runtime/tree"
	semreg "github.com/Project-Helianthus/helianthus-semreg/semreg/v1"
	"github.com/Project-Helianthus/helianthus-semreg/semreg/v1/driver"
)

// TestGatewayDriverHookRegistrationResolvesHostSurfaces varies every field of
// the member key a setup procedure and a diagnostic read claim to cover.
//
//   - Namespace: the native segment uses the key's namespace. Replacing
//     family.sample with family.alpha on sample.one's node does not call the
//     registered procedure. The diagnostic read dangles, so the addressed
//     read is not the member's own read.
//   - Instance: the node must name the same instance. Replacing sample.one
//     with sample.two on sample.one's node does not call sample.one. Calling
//     sample.two's own node returns sample.two's bytes.
//   - Node: a transport procedure returns that transport's bytes. A device
//     node is refused before prepare, so its procedure is not called. A
//     diagnostic read of the device returns the device's bytes.
//   - Local: pairing and point differ from other. Replacing the local id
//     returns the other member's bytes and does not call the first.
//
// The same surfaces serve family.sample and family.alpha. A disabled driver
// that declared the hooks still resolves. A driver that declared only the
// semantic hook does not receive the others. Replacing the portal binding
// leaves the semantic surface in place and makes portal unimplemented.
func TestGatewayDriverHookRegistrationResolvesHostSurfaces(t *testing.T) {
	ctx := context.Background()
	sample := &hookRegistrationFactory{kind: "family.sample"}
	alpha := &hookRegistrationFactory{kind: "family.alpha"}
	opened, err := openGatewayHost(ctx, filepath.Join(t.TempDir(), "runtime_state.json"), []gatewayHostInstance{
		{Kind: "family.sample", Instance: "sample.one", Enabled: true},
		{Kind: "family.sample", Instance: "sample.two", Enabled: true},
		{Kind: "family.alpha", Instance: "primary", Enabled: true},
	}, sample, alpha)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := opened.Close(context.Background()); err != nil {
			t.Errorf("close host: %v", err)
		}
	})
	if err := opened.Start(ctx); err != nil {
		t.Fatal(err)
	}
	one := sample.waitOpened(t, opened, "sample.one")
	two := sample.waitOpened(t, opened, "sample.two")
	primary := alpha.waitOpened(t, opened, "primary")
	for _, state := range []*hookInstanceState{one, two, primary} {
		if state.began.Load() != 1 || state.aborted.Load() != 1 {
			t.Fatalf("%s publication begin/abort = %d/%d", state.key, state.began.Load(), state.aborted.Load())
		}
	}

	var calls atomic.Int64
	registry := runtimecontrol.NewRegistry(nil)
	if err := registry.Register(
		hookRegistrationDescriptor(&calls, "family.sample", runtimecontrol.DirectionSouthbound, runtimecontrol.DispositionEnabled, "sample-src", reservedRegistrationHooks()),
		hookRegistrationDescriptor(&calls, "family.alpha", runtimecontrol.DirectionBoth, runtimecontrol.DispositionEnabled, "alpha-src", reservedRegistrationHooks()),
		hookRegistrationDescriptor(&calls, "family.beta", runtimecontrol.DirectionNorthbound, runtimecontrol.DispositionEnabled, "beta-src", []runtimecontrol.HookDeclaration{{Name: runtimecontrol.HookSemantic, Version: runtimecontrol.RegistrationHookVersion}}),
		hookRegistrationDescriptor(&calls, "family.disabled", runtimecontrol.DirectionSouthbound, runtimecontrol.DispositionDisabled, "disabled-src", reservedRegistrationHooks()),
	); err != nil {
		t.Fatal(err)
	}
	index := hookPortalIndex()
	portal := &gatewayPortalCatalogContributions{registry: contributionv1.NewRegistry(index)}
	if err := bindGatewayDriverHooks(registry, opened, portal); err != nil {
		t.Fatal(err)
	}

	semanticSurface, err := registry.ResolveHook("family.sample", runtimecontrol.HookSemantic)
	if err != nil {
		t.Fatal(err)
	}
	semantic, ok := semanticSurface.(gatewaySemanticSurface)
	if !ok || semantic.Runtime() != opened.site.Runtime() {
		t.Fatalf("semantic surface = %T, runtime %p", semanticSurface, opened.site.Runtime())
	}
	alphaSemantic, err := registry.ResolveHook("family.alpha", runtimecontrol.HookSemantic)
	if err != nil || alphaSemantic != semanticSurface {
		t.Fatalf("alpha semantic = %T %v; hook resolution switched on the driver", alphaSemantic, err)
	}
	betaSemantic, err := registry.ResolveHook("family.beta", runtimecontrol.HookSemantic)
	if err != nil || betaSemantic != semanticSurface {
		t.Fatalf("beta semantic = %T %v", betaSemantic, err)
	}
	if _, err := registry.ResolveHook("family.beta", runtimecontrol.HookMCP); !errors.Is(err, runtimecontrol.ErrHookUnimplemented) {
		t.Fatalf("undeclared mcp = %v", err)
	}
	disabledSetup, err := registry.ResolveHook("family.disabled", runtimecontrol.HookSetup)
	if err != nil {
		t.Fatalf("disabled setup = %v", err)
	}
	if _, ok := disabledSetup.(gatewaySetupSurface); !ok {
		t.Fatalf("disabled setup = %T", disabledSetup)
	}
	planned := map[string]bool{}
	for _, instance := range semantic.Runtime().Plan().Instances {
		planned[instance.Ref.String()] = true
	}
	for _, ref := range []string{"family.sample/sample.one", "family.sample/sample.two", "family.alpha/primary"} {
		if !planned[ref] {
			t.Fatalf("planned = %v, missing %s", planned, ref)
		}
	}
	if planned["family.beta/primary"] {
		t.Fatal("semantic surface planned a driver the host did not open")
	}

	mcpSurface, err := registry.ResolveHook("family.sample", runtimecontrol.HookMCP)
	if err != nil {
		t.Fatal(err)
	}
	mcp, ok := mcpSurface.(gatewayMCPSurface)
	if !ok || mcp.tree != opened.site.Tree() {
		t.Fatalf("mcp surface = %T", mcpSurface)
	}
	if alphaMCP, err := registry.ResolveHook("family.alpha", runtimecontrol.HookMCP); err != nil || alphaMCP != mcpSurface {
		t.Fatalf("alpha mcp = %T %v", alphaMCP, err)
	}
	roster, err := mcp.Namespaces(ctx)
	if err != nil {
		t.Fatal(err)
	}
	again, err := mcp.Namespaces(ctx)
	if err != nil {
		t.Fatal(err)
	}
	wantRoster := []tree.RosterEntry{
		{Owner: "family.alpha", Key: "family.alpha", Instances: []driver.InstanceKey{"primary"}},
		{Owner: "family.sample", Key: "family.sample", Instances: []driver.InstanceKey{"sample.one", "sample.two"}},
	}
	if !rosterEqual(roster, wantRoster) || !rosterEqual(again, wantRoster) {
		t.Fatalf("roster = %+v then %+v; want %+v", roster, again, wantRoster)
	}

	setupSurface, err := registry.ResolveHook("family.sample", runtimecontrol.HookSetup)
	if err != nil {
		t.Fatal(err)
	}
	setup, ok := setupSurface.(gatewaySetupSurface)
	if !ok || setup.tree != opened.site.Tree() {
		t.Fatalf("setup surface = %T", setupSurface)
	}
	marker := []byte("marker")
	outcome, err := setup.Dispatch(ctx, hookMemberKey("family.sample", "sample.one", one.root, "pairing"), marker)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Operation != nil || outcome.Error != "" || string(outcome.Native) != "sample.one:marker" || one.pairing.Load() != 1 {
		t.Fatalf("instance procedure = %q op=%v err=%q calls=%d", outcome.Native, outcome.Operation, outcome.Error, one.pairing.Load())
	}
	outcome, err = setup.Dispatch(ctx, hookMemberKey("family.sample", "sample.two", two.root, "pairing"), marker)
	if err != nil || string(outcome.Native) != "sample.two:marker" || two.pairing.Load() != 1 {
		t.Fatalf("second instance = %q %v calls=%d", outcome.Native, err, two.pairing.Load())
	}
	outcome, err = setup.Dispatch(ctx, hookMemberKey("family.alpha", "primary", primary.root, "pairing"), marker)
	if err != nil || string(outcome.Native) != "primary:marker" || primary.pairing.Load() != 1 {
		t.Fatalf("other kind = %q %v calls=%d", outcome.Native, err, primary.pairing.Load())
	}
	outcome, err = setup.Dispatch(ctx, hookMemberKey("family.sample", "sample.one", one.root, "other"), marker)
	if err != nil || string(outcome.Native) != "other:sample.one" || one.other.Load() != 1 || one.pairing.Load() != 1 {
		t.Fatalf("other local = %q %v pairing=%d other=%d", outcome.Native, err, one.pairing.Load(), one.other.Load())
	}
	outcome, err = setup.Dispatch(ctx, hookMemberKey("family.sample", "sample.one", one.transport, "pairing"), marker)
	if err != nil || string(outcome.Native) != "transport:sample.one:marker" || one.transportCalls.Load() != 1 {
		t.Fatalf("transport = %q %v calls=%d", outcome.Native, err, one.transportCalls.Load())
	}
	if _, err := setup.Dispatch(ctx, hookMemberKey("family.sample", "sample.one", one.device, "pairing"), marker); !errors.Is(err, errGatewayHookLevel) || one.deviceCalls.Load() != 0 {
		t.Fatalf("device procedure err=%v calls=%d", err, one.deviceCalls.Load())
	}
	install := tree.NodePath{Installation: semantic.Runtime().Plan().Installation}
	if _, err := setup.Dispatch(ctx, hookMemberKey("family.sample", "sample.one", driver.NodeRef{ID: install.NodeID()}, "pairing"), marker); !errors.Is(err, errGatewayHookLevel) || one.pairing.Load() != 1 {
		t.Fatalf("installation procedure err=%v pairing=%d", err, one.pairing.Load())
	}
	mismatched := hookMemberKey("family.sample", "sample.two", one.root, "pairing")
	if _, err := setup.Dispatch(ctx, mismatched, marker); !errors.Is(err, errGatewayHookInstance) || one.pairing.Load() != 1 || two.pairing.Load() != 1 {
		t.Fatalf("mismatched instance err=%v calls=%d/%d", err, one.pairing.Load(), two.pairing.Load())
	}
	wrongNamespace := hookMemberKey("family.alpha", "sample.one", one.root, "pairing")
	if err := dispatchDangles(setup.Dispatch(ctx, wrongNamespace, marker)); err != nil || one.pairing.Load() != 1 || primary.pairing.Load() != 1 {
		t.Fatalf("wrong namespace err=%v sample=%d alpha=%d", err, one.pairing.Load(), primary.pairing.Load())
	}
	if err := dispatchDangles(setup.Dispatch(ctx, hookMemberKey("family.sample", "sample.one", one.root, "missing"), marker)); err != nil || one.pairing.Load() != 1 || one.other.Load() != 1 {
		t.Fatalf("missing local err=%v", err)
	}

	evidenceSurface, err := registry.ResolveHook("family.sample", runtimecontrol.HookEvidence)
	if err != nil {
		t.Fatal(err)
	}
	evidence, ok := evidenceSurface.(gatewayEvidenceSurface)
	if !ok || evidence.tree != opened.site.Tree() {
		t.Fatalf("evidence surface = %T", evidenceSurface)
	}
	// A capture reads every registered member once, and the attribute read
	// reads the addressed member again. A key that does not address the
	// member therefore adds only the capture.
	pointBefore := one.point.Load()
	value, err := evidence.Read(ctx, hookMemberKey("family.sample", "sample.one", one.root, "point"))
	if err != nil || string(value.Encoded) != "sample.one" || value.Unit != "unit.test" || one.point.Load() != pointBefore+2 {
		t.Fatalf("point = %q unit=%s err=%v calls=%d before=%d", value.Encoded, value.Unit, err, one.point.Load(), pointBefore)
	}
	secondBefore := two.point.Load()
	value, err = evidence.Read(ctx, hookMemberKey("family.sample", "sample.two", two.root, "point"))
	if err != nil || string(value.Encoded) != "sample.two" || two.point.Load() != secondBefore+2 {
		t.Fatalf("second point = %q %v calls=%d before=%d", value.Encoded, err, two.point.Load(), secondBefore)
	}
	otherBefore := one.otherPoint.Load()
	value, err = evidence.Read(ctx, hookMemberKey("family.sample", "sample.one", one.root, "other"))
	if err != nil || string(value.Encoded) != "other:sample.one" || value.Unit != "unit.other" || one.otherPoint.Load() != otherBefore+2 {
		t.Fatalf("other point = %q unit=%s err=%v calls=%d before=%d", value.Encoded, value.Unit, err, one.otherPoint.Load(), otherBefore)
	}
	deviceBefore := one.devicePoint.Load()
	value, err = evidence.Read(ctx, hookMemberKey("family.sample", "sample.one", one.device, "point"))
	if err != nil || string(value.Encoded) != "device:sample.one" || one.devicePoint.Load() != deviceBefore+2 {
		t.Fatalf("device point = %q %v calls=%d before=%d", value.Encoded, err, one.devicePoint.Load(), deviceBefore)
	}
	mismatchBefore, otherInstanceBefore := one.point.Load(), two.point.Load()
	if _, err := evidence.Read(ctx, hookMemberKey("family.sample", "sample.two", one.root, "point")); !errors.Is(err, errGatewayHookInstance) || one.point.Load() != mismatchBefore || two.point.Load() != otherInstanceBefore {
		t.Fatalf("mismatched evidence err=%v calls=%d/%d", err, one.point.Load()-mismatchBefore, two.point.Load()-otherInstanceBefore)
	}
	namespaceBefore := one.point.Load()
	if err := readDangles(evidence.Read(ctx, hookMemberKey("family.alpha", "sample.one", one.root, "point"))); err != nil || one.point.Load() != namespaceBefore+1 {
		t.Fatalf("wrong evidence namespace err=%v calls=%d", err, one.point.Load()-namespaceBefore)
	}

	portalSurface, err := registry.ResolveHook("family.sample", runtimecontrol.HookPortal)
	if err != nil {
		t.Fatal(err)
	}
	portalHooks, ok := portalSurface.(gatewayPortalSurface)
	if !ok {
		t.Fatalf("portal surface = %T", portalSurface)
	}
	if err := portalHooks.Publish(contributionv1.DriverGeneration{DriverID: "family.sample", Generation: 1}, []contributionv1.Manifest{hookPortalManifest("family.sample", "portal.sample", "action.sample")}); err != nil {
		t.Fatal(err)
	}
	if err := portalHooks.Publish(contributionv1.DriverGeneration{DriverID: "family.alpha", Generation: 1}, []contributionv1.Manifest{hookPortalManifest("family.alpha", "portal.alpha", "action.alpha")}); err != nil {
		t.Fatal(err)
	}
	snapshot := portalHooks.Snapshot()
	sampleManifest, sampleAction, ok := acceptedAction(snapshot, "family.sample")
	if !ok || sampleManifest != "portal.sample" || sampleAction != "action.sample" {
		t.Fatalf("sample portal = %s %s %v", sampleManifest, sampleAction, ok)
	}
	alphaManifest, alphaAction, ok := acceptedAction(snapshot, "family.alpha")
	if !ok || alphaManifest != "portal.alpha" || alphaAction != "action.alpha" {
		t.Fatalf("alpha portal = %s %s %v", alphaManifest, alphaAction, ok)
	}
	if calls.Load() != 0 {
		t.Fatalf("factory calls = %d", calls.Load())
	}

	if err := bindGatewayDriverHooks(registry, opened, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.ResolveHook("family.sample", runtimecontrol.HookPortal); !errors.Is(err, runtimecontrol.ErrHookUnimplemented) {
		t.Fatalf("portal after replacement = %v", err)
	}
	replaced, err := registry.ResolveHook("family.sample", runtimecontrol.HookSemantic)
	if err != nil {
		t.Fatal(err)
	}
	replacedSemantic, ok := replaced.(gatewaySemanticSurface)
	if !ok || replacedSemantic.Runtime() != opened.site.Runtime() {
		t.Fatal("replacing portal replaced the semantic surface")
	}
}

func TestGatewayDriverHookRegistrationRefusesAClosedHost(t *testing.T) {
	registry := runtimecontrol.NewRegistry(nil)
	if err := bindGatewayDriverHooks(nil, &gatewayHost{}, nil); err == nil {
		t.Fatal("nil registry bound hooks")
	}
	if err := bindGatewayDriverHooks(registry, nil, nil); err == nil {
		t.Fatal("nil host bound hooks")
	}
	if err := bindGatewayDriverHooks(registry, &gatewayHost{}, nil); err == nil {
		t.Fatal("host without a site bound hooks")
	}
}

func reservedRegistrationHooks() []runtimecontrol.HookDeclaration {
	names := []runtimecontrol.HookName{runtimecontrol.HookSemantic, runtimecontrol.HookMCP, runtimecontrol.HookPortal, runtimecontrol.HookSetup, runtimecontrol.HookEvidence}
	hooks := make([]runtimecontrol.HookDeclaration, len(names))
	for i, name := range names {
		hooks[i] = runtimecontrol.HookDeclaration{Name: name, Version: runtimecontrol.RegistrationHookVersion}
	}
	return hooks
}

func hookRegistrationDescriptor(calls *atomic.Int64, id runtimecontrol.DriverID, direction runtimecontrol.Direction, disposition runtimecontrol.Disposition, source runtimecontrol.SourceID, hooks []runtimecontrol.HookDeclaration) runtimecontrol.Descriptor {
	return runtimecontrol.Descriptor{
		Format:      runtimecontrol.DescriptorFormatV1,
		DriverID:    id,
		Direction:   direction,
		Disposition: disposition,
		NativeContract: runtimecontrol.NativeContractRef{
			Owner: "example.org/native/owner-" + string(id), Contract: "synthetic.contract", Version: "v1.0.0",
		},
		Adapter: runtimecontrol.NewAdapter(func(context.Context, runtimecontrol.Construction) (*hookRegistrationRuntime, error) {
			calls.Add(1)
			return &hookRegistrationRuntime{}, nil
		}),
		Sources: []runtimecontrol.SourceID{source},
		Hooks:   hooks,
	}
}

type hookRegistrationRuntime struct{}

func (hookRegistrationRuntime) Start(context.Context) (uint64, error) { return 1, nil }
func (hookRegistrationRuntime) Replace(context.Context) (uint64, error) {
	return 1, nil
}
func (hookRegistrationRuntime) Stop(context.Context) error { return nil }
func (hookRegistrationRuntime) Generation() uint64         { return 1 }
func (hookRegistrationRuntime) Revision() uint64           { return 1 }
func (hookRegistrationRuntime) SafetyQuarantined() bool    { return false }

type hookRegistrationFactory struct {
	kind     driver.DriverKind
	mu       sync.Mutex
	states   map[driver.InstanceKey]*hookInstanceState
	startErr error
}

func (f *hookRegistrationFactory) Descriptor() driver.Descriptor { return driver.Descriptor{} }

func (f *hookRegistrationFactory) Plan(config driver.Config) ([]driver.InstancePlan, error) {
	if !config.Enabled {
		return nil, nil
	}
	return []driver.InstancePlan{{Driver: f.kind, Instance: driver.InstanceKey(config.Instance)}}, nil
}

func (f *hookRegistrationFactory) Construct(context.Context, driver.InstancePlan) (driver.Instance, error) {
	return &hookRegistrationInstance{factory: f}, nil
}

func (f *hookRegistrationFactory) remember(state *hookInstanceState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.states == nil {
		f.states = map[driver.InstanceKey]*hookInstanceState{}
	}
	f.states[state.key] = state
}

func (f *hookRegistrationFactory) fail(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.startErr == nil {
		f.startErr = err
	}
}

func (f *hookRegistrationFactory) failure() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.startErr
}

func (f *hookRegistrationFactory) state(key driver.InstanceKey) *hookInstanceState {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.states == nil {
		return nil
	}
	return f.states[key]
}

func (f *hookRegistrationFactory) waitOpened(t *testing.T, opened *gatewayHost, key driver.InstanceKey) *hookInstanceState {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := f.failure(); err != nil {
			t.Fatalf("instance %s/%s: %v", f.kind, key, err)
		}
		if state := f.state(key); state != nil && hookObserved(opened, f.kind, key) == string(life.ObservedRunning) {
			return state
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("instance %s/%s did not start (%s): %v", f.kind, key, hookObserved(opened, f.kind, key), f.failure())
	return nil
}

func hookObserved(opened *gatewayHost, kind driver.DriverKind, key driver.InstanceKey) string {
	status, err := opened.site.Snapshot(life.Ref{Driver: kind, Instance: key})
	if err != nil {
		return err.Error()
	}
	return string(status.ObservedState)
}

type hookRegistrationInstance struct {
	factory *hookRegistrationFactory
}

type hookInstanceState struct {
	key            driver.InstanceKey
	root           driver.NodeRef
	transport      driver.NodeRef
	device         driver.NodeRef
	pairing        atomic.Int64
	other          atomic.Int64
	transportCalls atomic.Int64
	deviceCalls    atomic.Int64
	point          atomic.Int64
	otherPoint     atomic.Int64
	devicePoint    atomic.Int64
	began          atomic.Int64
	aborted        atomic.Int64
}

func (i *hookRegistrationInstance) Start(ctx context.Context, session driver.Session) error {
	if err := i.register(ctx, session); err != nil {
		i.factory.fail(err)
		return err
	}
	return nil
}

func (i *hookRegistrationInstance) register(ctx context.Context, session driver.Session) error {
	rooted, ok := session.Native().(interface{ Root() driver.NodeRef })
	if !ok {
		return errors.New("native registry has no instance root")
	}
	root := rooted.Root()
	path, err := tree.ParseNodePath("/" + string(root.ID))
	if err != nil {
		return err
	}
	state := &hookInstanceState{key: path.Instance, root: root}
	tx, err := session.Publisher().Begin(ctx)
	if err != nil {
		return err
	}
	if err := tx.Abort(ctx); err != nil {
		return err
	}
	state.began.Add(1)
	state.aborted.Add(1)
	native := session.Native()
	if err := native.Method(root, hookMethodSpec("pairing"), func(_ context.Context, input []byte) ([]byte, error) {
		state.pairing.Add(1)
		return []byte(string(state.key) + ":" + string(input)), nil
	}); err != nil {
		return err
	}
	if err := native.Method(root, hookMethodSpec("other"), func(context.Context, []byte) ([]byte, error) {
		state.other.Add(1)
		return []byte("other:" + string(state.key)), nil
	}); err != nil {
		return err
	}
	transport, err := native.Node(hookNodeSpec("transport.one", nil))
	if err != nil {
		return err
	}
	state.transport = transport
	if err := native.Method(transport, hookMethodSpec("pairing"), func(_ context.Context, input []byte) ([]byte, error) {
		state.transportCalls.Add(1)
		return []byte("transport:" + string(state.key) + ":" + string(input)), nil
	}); err != nil {
		return err
	}
	device, err := native.Node(hookNodeSpec("device.one", &transport))
	if err != nil {
		return err
	}
	state.device = device
	if err := native.Method(device, hookMethodSpec("pairing"), func(context.Context, []byte) ([]byte, error) {
		state.deviceCalls.Add(1)
		return []byte("device:" + string(state.key)), nil
	}); err != nil {
		return err
	}
	if err := native.ReadMember(root, hookMemberSpec("point", "unit.test"), func(context.Context) ([]byte, error) {
		state.point.Add(1)
		return []byte(state.key), nil
	}); err != nil {
		return err
	}
	if err := native.ReadMember(root, hookMemberSpec("other", "unit.other"), func(context.Context) ([]byte, error) {
		state.otherPoint.Add(1)
		return []byte("other:" + string(state.key)), nil
	}); err != nil {
		return err
	}
	if err := native.ReadMember(device, hookMemberSpec("point", "unit.test"), func(context.Context) ([]byte, error) {
		state.devicePoint.Add(1)
		return []byte("device:" + string(state.key)), nil
	}); err != nil {
		return err
	}
	i.factory.remember(state)
	return nil
}

func (hookRegistrationInstance) FenceWithdrawal() {}

func (hookRegistrationInstance) Stop(context.Context) driver.CloseReport {
	return driver.CloseReport{Confirmed: true}
}

func hookMethodSpec(local string) driver.MethodSpec {
	return driver.MethodSpec{Local: driver.MemberID(local), Schema: "schema.test", Scope: "scope.test"}
}

func hookMemberSpec(local, units string) driver.MemberSpec {
	return driver.MemberSpec{Local: driver.MemberID(local), Schema: "schema.test", Units: semreg.DefinitionID(units), Mode: "retained", Scope: "scope.test"}
}

func hookNodeSpec(local string, parent *driver.NodeRef) driver.NodeSpec {
	return driver.NodeSpec{Parent: parent, Kind: "node.test", Local: driver.NodeID(local), Contract: "contract.test"}
}

func hookMemberKey(namespace string, instance driver.InstanceKey, node driver.NodeRef, local string) driver.MemberKey {
	return driver.MemberKey{Namespace: driver.NamespaceID(namespace), Instance: instance, Node: driver.NodeID(node.ID), Local: driver.MemberID(local)}
}

func dispatchDangles(outcome tree.Outcome, err error) error {
	if err == nil {
		return errors.New("procedure returned " + string(outcome.Native))
	}
	var semantic *semreg.Error
	if !errors.As(err, &semantic) || semantic.ID != semreg.DanglingReference {
		return err
	}
	return nil
}

func readDangles(value tree.Value, err error) error {
	if err == nil {
		return errors.New("read returned " + string(value.Encoded))
	}
	var semantic *semreg.Error
	if !errors.As(err, &semantic) || semantic.ID != semreg.DanglingReference {
		return err
	}
	return nil
}

func rosterEqual(got, want []tree.RosterEntry) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i].Owner != want[i].Owner || got[i].Key != want[i].Key || len(got[i].Instances) != len(want[i].Instances) {
			return false
		}
		for j := range want[i].Instances {
			if got[i].Instances[j] != want[i].Instances[j] {
				return false
			}
		}
	}
	return true
}

func acceptedAction(snapshot contributionv1.RegistrySnapshot, driverID string) (string, string, bool) {
	found := false
	manifestID, actionID := "", ""
	for _, item := range snapshot.Accepted {
		if item.Key.DriverID != driverID {
			continue
		}
		if found || len(item.Manifest.Actions) != 1 {
			return "", "", false
		}
		found = true
		manifestID = item.Manifest.ManifestID
		actionID = item.Manifest.Actions[0].ID
	}
	return manifestID, actionID, found
}

func hookPortalIndex() *contributionv1.StaticIndex {
	pack := contributionv1.PackRef{ID: "helianthus.pack.test", Version: "1.0.0"}
	ref := func(id string) contributionv1.DefinitionRef {
		return contributionv1.DefinitionRef{Pack: pack, ID: id, Version: "1.0.0"}
	}
	index := contributionv1.NewStaticIndex()
	index.AddLens("fixture.lens")
	index.AddPack(pack)
	for _, definition := range []contributionv1.DefinitionRef{ref("field"), ref("service"), ref("capability"), ref("unit"), ref("operation"), ref("argument"), ref("effect")} {
		index.AddDefinition(definition)
	}
	index.AddCanonicalUnit(ref("field"), ref("unit"))
	index.AddServiceCapability(ref("service"), ref("capability"))
	index.AddFieldRelation(ref("field"), ref("service"), ref("capability"))
	index.AddOperation(ref("operation"), ref("capability"), ref("service"), ref("argument"), ref("effect"))
	index.AddNativeMember(contributionv1.NativeContractRef{Owner: "helianthus", Contract: "test-native", Version: "1.0.0"}, "field", "native-field")
	return index
}

func hookPortalManifest(driverID, manifestID, actionID string) contributionv1.Manifest {
	pack := contributionv1.PackRef{ID: "helianthus.pack.test", Version: "1.0.0"}
	ref := func(id string) contributionv1.DefinitionRef {
		return contributionv1.DefinitionRef{Pack: pack, ID: id, Version: "1.0.0"}
	}
	field, service, capability := ref("field"), ref("service"), ref("capability")
	unit, operation, argument, effect := ref("unit"), ref("operation"), ref("argument"), ref("effect")
	contract := contributionv1.NativeContractRef{Owner: "helianthus", Contract: "test-native", Version: "1.0.0"}
	return contributionv1.Manifest{
		Contract: contributionv1.Contract, ManifestID: manifestID, ManifestVersion: "1.0.0",
		Contributor: contributionv1.Contributor{DriverID: driverID, NativeContract: contract},
		Requires:    contributionv1.Requirements{SemanticKernel: contributionv1.SemanticKernel, Packs: []contributionv1.PackRef{pack}},
		Groups:      []contributionv1.Group{{ID: "group", Label: contributionv1.Label{Key: "group", Default: "Group"}, ResourceContext: "resource", Order: 1}},
		Fields:      []contributionv1.Field{{ID: "field", Group: "group", Label: contributionv1.Label{Key: "field", Default: "Field"}, Ref: field, ServiceRef: service, CapabilityRef: capability, UnitRef: unit, Order: 1}},
		Views:       []contributionv1.View{{ID: "view", Group: "group", Label: contributionv1.Label{Key: "view", Default: "View"}, Renderer: "summary", Slot: "lens", Lens: "fixture.lens", FieldIDs: []string{"field"}, DiagnosticIDs: []string{"diagnostic"}, Order: 1}},
		Actions:     []contributionv1.Action{{ID: actionID, Group: "group", Label: contributionv1.Label{Key: "action", Default: "Action"}, OperationRef: &operation, CapabilityRef: capability, ServiceRef: service, ArgumentRef: &argument, EffectRef: &effect, Order: 1}},
		Diagnostics: []contributionv1.Diagnostic{{ID: "diagnostic", Group: "group", Label: contributionv1.Label{Key: "diagnostic", Default: "Diagnostic"}, MemberID: "native-field", Kind: "field", Order: 1}},
	}
}
