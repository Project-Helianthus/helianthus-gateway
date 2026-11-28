package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	ebusgateway "github.com/Project-Helianthus/helianthus-gateway"
	"github.com/Project-Helianthus/helianthus-gateway/internal/semanticmetrics"
	"github.com/Project-Helianthus/helianthus-gateway/m2mgraphql"
	"github.com/Project-Helianthus/helianthus-gateway/mcp"
	"github.com/Project-Helianthus/helianthus-gateway/portal/contributionv1"
	"github.com/Project-Helianthus/helianthus-semreg/semreg/runtime/life"
	"github.com/Project-Helianthus/helianthus-semreg/semreg/runtime/tree"
	semreg "github.com/Project-Helianthus/helianthus-semreg/semreg/v1"
	"github.com/Project-Helianthus/helianthus-semreg/semreg/v1/driver"
	"github.com/Project-Helianthus/helianthus-semreg/semreg/v1/graph"
)

const surfaceDriverKind driver.DriverKind = "family.surface"

// The remote listener accepts only these documents. They are the same text the
// handler serves; the constants there are not exported.
const (
	surfaceResourcesQuery = `query SemanticResources($cut_id: String, $page_size: Int!, $page_token: String, $kind: String, $plane: String) {
  semantic_resources(cut_id: $cut_id, page_size: $page_size, page_token: $page_token, kind: $kind, plane: $plane)
}`
	surfaceTraversalQuery = `query SemanticTraversal($entry: semantic_graph_entry_input!, $direction: String!, $depth: Int!, $cut_id: String, $page_size: Int!, $page_token: String, $lens: String, $planes: [String!], $definitions: [semantic_graph_definition_ref_input!]) {
  semantic_traversal(entry: $entry, direction: $direction, depth: $depth, cut_id: $cut_id, page_size: $page_size, page_token: $page_token, lens: $lens, planes: $planes, definitions: $definitions)
}`
	surfaceLensQuery = `query SemanticLens($entry: semantic_graph_entry_input!, $direction: String!, $depth: Int!, $cut_id: String, $page_size: Int!, $page_token: String, $lens: String!) {
  semantic_lens(entry: $entry, direction: $direction, depth: $depth, cut_id: $cut_id, page_size: $page_size, page_token: $page_token, lens: $lens)
}`
	surfaceCatalogQuery = `query SemanticElementCatalog {
  semantic_element_catalog
}`
)

func surfaceTime(at time.Time) semreg.TimePoint {
	return semreg.TimePoint{UnixNanoseconds: semreg.Int64(strconv.FormatInt(at.UnixNano(), 10)), ClockID: "clock.utc", UncertaintyNS: "0"}
}

func surfaceEvidence(label string) semreg.EvidenceRef {
	sum := sha256.Sum256([]byte("surface publication " + label))
	return semreg.EvidenceRef{
		Owner: "owner.test", Kind: "evidence.test", Digest: semreg.Digest("sha256:" + hex.EncodeToString(sum[:])),
		Contract: "test.contract/v1", Access: semreg.EvidenceAccessPublic, Redaction: semreg.RedactionNone,
	}
}

func surfaceResourceKind() (semreg.DefinitionID, semreg.AnchorClass, error) {
	catalog, err := graph.Embedded()
	if err != nil {
		return "", "", err
	}
	for _, kind := range []semreg.DefinitionID{"resource.equipment", "resource.pv_system"} {
		if class, ok := catalog.ResourceKindClass(kind); ok {
			return kind, class, nil
		}
	}
	return "", "", errors.New("synthetic resource kind is not in the element catalog")
}

func surfaceHostInstances() []gatewayHostInstance {
	return []gatewayHostInstance{
		{Kind: string(surfaceDriverKind), Instance: "surface.one", Enabled: true},
		{Kind: string(surfaceDriverKind), Instance: "surface.two", Enabled: true},
	}
}

func surfacePointValue(key driver.InstanceKey) string {
	if key == "surface.two" {
		return "2"
	}
	return "1"
}

type surfaceDriverFactory struct {
	hold    chan struct{}
	mu      sync.Mutex
	states  map[driver.InstanceKey]*surfaceInstanceState
	ready   chan struct{}
	readyOK bool
	err     error
}

func newSurfaceDriverFactory(block bool) *surfaceDriverFactory {
	factory := &surfaceDriverFactory{ready: make(chan struct{})}
	if block {
		factory.hold = make(chan struct{})
	}
	return factory
}

func (f *surfaceDriverFactory) Descriptor() driver.Descriptor { return driver.Descriptor{} }

func (f *surfaceDriverFactory) Plan(config driver.Config) ([]driver.InstancePlan, error) {
	if !config.Enabled {
		return nil, nil
	}
	return []driver.InstancePlan{{Driver: surfaceDriverKind, Instance: driver.InstanceKey(config.Instance)}}, nil
}

func (f *surfaceDriverFactory) Construct(context.Context, driver.InstancePlan) (driver.Instance, error) {
	return &surfaceInstance{factory: f}, nil
}

func (f *surfaceDriverFactory) fail(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err == nil {
		f.err = err
	}
}

func (f *surfaceDriverFactory) failure() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.err
}

func (f *surfaceDriverFactory) remember(state *surfaceInstanceState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.states == nil {
		f.states = map[driver.InstanceKey]*surfaceInstanceState{}
	}
	f.states[state.key] = state
	if !f.readyOK {
		f.readyOK = true
		close(f.ready)
	}
}

func (f *surfaceDriverFactory) state(key driver.InstanceKey) *surfaceInstanceState {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.states == nil {
		return nil
	}
	return f.states[key]
}

func (f *surfaceDriverFactory) readyState() *surfaceInstanceState {
	f.mu.Lock()
	defer f.mu.Unlock()
	if state := f.states["surface.one"]; state != nil {
		return state
	}
	for _, state := range f.states {
		return state
	}
	return nil
}

func (f *surfaceDriverFactory) pairingSum() int64 {
	f.mu.Lock()
	states := make([]*surfaceInstanceState, 0, len(f.states))
	for _, state := range f.states {
		states = append(states, state)
	}
	f.mu.Unlock()
	var sum int64
	for _, state := range states {
		sum += state.pairing.Load() + state.deviceCalls.Load()
	}
	return sum
}

func (f *surfaceDriverFactory) release() {
	if f.hold == nil {
		return
	}
	select {
	case <-f.hold:
	default:
		close(f.hold)
	}
}

func (f *surfaceDriverFactory) waitReady(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if err := f.failure(); err != nil {
			t.Fatal(err)
		}
		select {
		case <-f.ready:
			return
		default:
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("surface driver did not register: %v", f.failure())
}

func (f *surfaceDriverFactory) waitRunning(t *testing.T, opened *gatewayHost) {
	t.Helper()
	for _, key := range []driver.InstanceKey{"surface.one", "surface.two"} {
		deadline := time.Now().Add(8 * time.Second)
		for {
			if err := f.failure(); err != nil {
				t.Fatal(err)
			}
			if hookObserved(opened, surfaceDriverKind, key) == string(life.ObservedRunning) && f.state(key) != nil && f.state(key).device.ID != "" {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("instance %s did not start (%s): %v", key, hookObserved(opened, surfaceDriverKind, key), f.failure())
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
}

type surfaceInstance struct {
	factory *surfaceDriverFactory
}

type surfaceInstanceState struct {
	key         driver.InstanceKey
	root        driver.NodeRef
	transport   driver.NodeRef
	device      driver.NodeRef
	resourceID  string
	pairing     atomic.Int64
	deviceCalls atomic.Int64
}

func (i *surfaceInstance) Start(ctx context.Context, session driver.Session) error {
	if err := i.register(ctx, session); err != nil {
		i.factory.fail(err)
		return err
	}
	return nil
}

func (i *surfaceInstance) register(ctx context.Context, session driver.Session) error {
	rooted, ok := session.Native().(interface{ Root() driver.NodeRef })
	if !ok {
		return errors.New("native registry has no instance root")
	}
	root := rooted.Root()
	path, err := tree.ParseNodePath("/" + string(root.ID))
	if err != nil {
		return err
	}
	state := &surfaceInstanceState{key: path.Instance, root: root, resourceID: "resource:surface:" + string(path.Instance)}
	if err := surfacePublish(ctx, session, state); err != nil {
		return err
	}
	native := session.Native()
	if err := native.Method(root, hookMethodSpec("pairing"), func(_ context.Context, input []byte) ([]byte, error) {
		state.pairing.Add(1)
		return []byte(string(state.key) + ":" + string(input)), nil
	}); err != nil {
		return err
	}
	transport, err := native.Node(hookNodeSpec("transport.one", nil))
	if err != nil {
		return err
	}
	state.transport = transport
	if err := native.ReadMember(transport, hookMemberSpec("count", "unit.count"), func(context.Context) ([]byte, error) {
		return []byte(surfacePointValue(state.key)), nil
	}); err != nil {
		return err
	}
	i.factory.remember(state)
	if i.factory.hold != nil {
		<-i.factory.hold
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
	return nil
}

func surfacePublish(ctx context.Context, session driver.Session, state *surfaceInstanceState) error {
	kind, class, err := surfaceResourceKind()
	if err != nil {
		return err
	}
	slot := string(state.key)
	resourceID := semreg.ResourceID(state.resourceID)
	anchor := surfaceEvidence("anchor " + slot)
	originEvidence := surfaceEvidence("origin " + slot)
	linkEvidence := surfaceEvidence("link " + slot)
	nativeEvidence := surfaceEvidence("native " + slot)
	binding := semreg.NativeBindingID("binding:surface")
	resource := semreg.Resource{
		ResourceID: resourceID, Kind: kind, State: semreg.ResourceCurrent, Revision: "1",
		Anchor: semreg.ResourceAnchor{Class: class, Basis: semreg.AnchorOperatorSlot, Slot: &slot, Evidence: []semreg.EvidenceRef{anchor}},
		Origin: semreg.OriginRef{Kind: semreg.OriginOperator, OriginID: semreg.OriginID("origin:operator:" + slot), Evidence: []semreg.EvidenceRef{originEvidence}},
	}
	tx, err := session.Publisher().Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Abort(ctx) }()
	if err := tx.Stage(semreg.PublicationBatch{
		SourceUpserts: []semreg.SourceDescriptor{{
			ProtocolID: "protocol.test", ProfileID: "profile.test", ProfileVersion: "1",
			RegistryEvidence: surfaceEvidence("registry " + slot),
			StartedAt:        surfaceTime(time.Now().Add(-time.Second)),
			State:            semreg.SourceCurrent, Revision: "1",
		}},
		BindingUpserts:      []semreg.NativeBinding{{BindingID: binding, NativeResource: nativeEvidence, State: semreg.BindingCurrent, Revision: "1"}},
		IdentityLinkUpserts: []semreg.IdentityLink{{BindingID: binding, Resource: &resourceID, State: semreg.LinkQualified, Basis: []semreg.EvidenceRef{linkEvidence}, Revision: "1"}},
		ResourceUpserts:     []semreg.Resource{resource},
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (surfaceInstance) FenceWithdrawal() {}

func (surfaceInstance) Stop(context.Context) driver.CloseReport {
	return driver.CloseReport{Confirmed: true}
}

func surfacePortalManifest(manifestID, resourceID string) contributionv1.Manifest {
	manifest := gatewayPortalEVSEDescriptor()
	manifest.ManifestID = manifestID
	manifest.Contributor.DriverID = string(surfaceDriverKind)
	manifest.Contributor.NativeContract = contributionv1.NativeContractRef{Owner: string(surfaceDriverKind), Contract: "schema.test", Version: "1.0.0"}
	manifest.Groups[0].ID = "surface"
	manifest.Groups[0].ResourceContext = resourceID
	manifest.Groups[0].Label = contributionv1.Label{Key: manifestID + ".group", Default: "Surface"}
	field := manifest.Fields[0]
	field.Group = "surface"
	manifest.Fields[0] = field
	view := manifest.Views[0]
	view.ID = manifestID + ".summary"
	view.Group = "surface"
	view.Lens = ""
	view.FieldIDs = []string{field.ID}
	view.DiagnosticIDs = []string{}
	view.Label = contributionv1.Label{Key: manifestID + ".summary", Default: "Summary"}
	manifest.Views[0] = view
	manifest.Actions = []contributionv1.Action{{
		ID: "pairing", Group: "surface", Label: contributionv1.Label{Key: manifestID + ".pairing", Default: "Pairing"},
		NativeMemberID: "pairing", ServiceRef: field.ServiceRef, CapabilityRef: field.CapabilityRef, Order: 1,
	}}
	return manifest
}

func publishSurfaceManifests(t *testing.T, contributions *gatewayPortalCatalogContributions) {
	t.Helper()
	manifests := []contributionv1.Manifest{
		surfacePortalManifest("portal.surface.one", "resource:surface:surface.one"),
		surfacePortalManifest("portal.surface.two", "resource:surface:surface.two"),
	}
	if err := contributions.PublishGeneration(contributionv1.DriverGeneration{DriverID: string(surfaceDriverKind), Generation: 1}, manifests); err != nil {
		t.Fatal(err)
	}
}

type surfaceStable struct {
	catalog   []byte
	roster    string
	identity  []byte
	points    string
	resources string
	links     string
}

func surfaceCaptureStable(t *testing.T, opened *gatewayHost, dir string) surfaceStable {
	t.Helper()
	ctx := context.Background()
	graphTree := opened.site.Tree()
	catalog, err := graphTree.Catalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := semreg.CanonicalJSON(gatewayGraphCatalog(catalog))
	if err != nil {
		t.Fatal(err)
	}
	listed, err := graphTree.ListResources(ctx, graph.ListRequest{PageSize: graph.MaxPageSize})
	if err != nil {
		t.Fatal(err)
	}
	if listed.Page.NextPage != "" {
		t.Fatalf("resource page truncated at %d", len(listed.Page.Resources))
	}
	resources := make([]string, 0, len(listed.Page.Resources))
	for _, entry := range listed.Page.Resources {
		resources = append(resources, string(entry.ResourceID)+" "+string(entry.Kind)+" "+string(entry.AssetID))
	}
	sort.Strings(resources)
	cut, err := graphTree.Capture(ctx, tree.CutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	roster := make([]string, 0, len(cut.Roster()))
	for _, entry := range cut.Roster() {
		instances := make([]string, len(entry.Instances))
		for i, instance := range entry.Instances {
			instances[i] = string(instance)
		}
		roster = append(roster, string(entry.Owner)+" "+string(entry.Key)+" "+strings.Join(instances, ","))
	}
	_ = cut.Close()
	sort.Strings(roster)
	hostCut, err := opened.site.Cut(ctx)
	if err != nil {
		t.Fatal(err)
	}
	links := make([]string, 0, len(hostCut.Links()))
	for _, link := range hostCut.Links() {
		links = append(links, string(link.Key.Type.ID)+" "+string(link.Key.From.ID)+" "+string(link.Key.To.ID))
	}
	_ = hostCut.Close()
	sort.Strings(links)
	points := make([]string, 0, 2)
	for _, key := range []driver.InstanceKey{"surface.one", "surface.two"} {
		points = append(points, surfaceReadPoint(t, graphTree, key))
	}
	sort.Strings(points)
	identity, err := os.ReadFile(filepath.Join(dir, "local-identity.json"))
	if err != nil {
		t.Fatal(err)
	}
	return surfaceStable{catalog: canonical, roster: strings.Join(roster, "\n"), identity: identity, points: strings.Join(points, "\n"), resources: strings.Join(resources, "\n"), links: strings.Join(links, "\n")}
}

func surfaceReadPoint(t *testing.T, graphTree *tree.Tree, key driver.InstanceKey) string {
	t.Helper()
	ctx := context.Background()
	cut, err := graphTree.Capture(ctx, tree.CutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cut.Close() }()
	var node driver.NodeID
	for _, change := range mustRegistry(t, graphTree) {
		if change.Kind != tree.ChangeMember {
			continue
		}
		descriptor, err := cut.Describe(ctx, change.Ref)
		if err != nil || descriptor.Withdrawn || descriptor.Local != "count" || descriptor.Instance.Instance != key {
			continue
		}
		parsed, err := tree.ParseKey(string(change.Ref))
		if err != nil {
			continue
		}
		node = driver.NodeID(parsed.Node.NodeID())
		value, err := (gatewayEvidenceSurface{tree: graphTree}).Read(ctx, driver.MemberKey{
			Namespace: descriptor.Namespace, Instance: key, Node: node, Local: "count",
		})
		if err != nil {
			t.Fatal(err)
		}
		return string(key) + "=" + string(value.Encoded)
	}
	t.Fatalf("count point of %s is not registered", key)
	return ""
}

func mustRegistry(t *testing.T, graphTree *tree.Tree) []tree.Change {
	t.Helper()
	changes, ok := graphTree.Registry.Since(0)
	if !ok {
		t.Fatal("namespace registry history unavailable")
	}
	return changes
}

func surfaceSame(t *testing.T, got, want surfaceStable) {
	t.Helper()
	if !bytes.Equal(got.catalog, want.catalog) {
		t.Fatal("element catalog changed across lifecycles")
	}
	if got.roster != want.roster {
		t.Fatalf("roster changed: %s", got.roster)
	}
	if !bytes.Equal(got.identity, want.identity) {
		t.Fatal("installation key changed")
	}
	if got.points != want.points {
		t.Fatalf("point bytes changed: %s", got.points)
	}
	if got.resources != want.resources {
		t.Fatalf("published resources changed:\n%s", got.resources)
	}
	if got.links != want.links {
		t.Fatalf("retained links changed: %s", got.links)
	}
}

func surfaceRequireResources(t *testing.T, record surfaceStable) {
	t.Helper()
	for _, id := range []string{"resource:surface:surface.one", "resource:surface:surface.two"} {
		if !strings.Contains(record.resources, id+" ") {
			t.Fatalf("published resources missing %s: %s", id, record.resources)
		}
	}
	if !strings.Contains(record.points, "surface.one=1") || !strings.Contains(record.points, "surface.two=2") {
		t.Fatalf("point bytes = %s", record.points)
	}
	if !strings.Contains(record.roster, string(surfaceDriverKind)+" "+string(surfaceDriverKind)+" ") {
		t.Fatalf("roster = %s", record.roster)
	}
}

func openSurfaceHost(t *testing.T, dir string, block bool) (*gatewayHost, *surfaceDriverFactory, <-chan error) {
	t.Helper()
	factory := newSurfaceDriverFactory(block)
	opened, err := openGatewayHost(context.Background(), filepath.Join(dir, "runtime_state.json"), surfaceHostInstances(), factory)
	if err != nil {
		t.Fatal(err)
	}
	if !block {
		if err := opened.Start(context.Background()); err != nil {
			_ = opened.Close(context.Background())
			t.Fatal(err)
		}
		factory.waitRunning(t, opened)
		return opened, factory, nil
	}
	errCh := make(chan error, 1)
	go func() { errCh <- opened.Start(context.Background()) }()
	factory.waitReady(t)
	return opened, factory, errCh
}

func TestComposedStoragePublicationParity(t *testing.T) {
	dir := t.TempDir()
	t.Cleanup(resetGatewayComposedSurfaces)
	opened, factory, _ := openSurfaceHost(t, dir, false)
	t.Cleanup(func() { _ = opened.Close(context.Background()) })
	first := surfaceCaptureStable(t, opened, dir)
	surfaceRequireResources(t, first)
	for _, key := range []driver.InstanceKey{"surface.one", "surface.two"} {
		state := factory.state(key)
		if state == nil {
			t.Fatalf("missing %s", key)
		}
		outcome, err := (gatewaySetupSurface{tree: opened.site.Tree()}).Dispatch(context.Background(), hookMemberKey(string(surfaceDriverKind), key, state.root, "pairing"), []byte("stored"))
		if err != nil {
			t.Fatal(err)
		}
		if string(outcome.Native) != string(key)+":stored" {
			t.Fatalf("native read of %s = %s", key, outcome.Native)
		}
	}
	if err := opened.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	reopened, _, _ := openSurfaceHost(t, dir, false)
	t.Cleanup(func() { _ = reopened.Close(context.Background()) })
	second := surfaceCaptureStable(t, reopened, dir)
	surfaceSame(t, second, first)
}

func TestSemanticGraphComposedHostParity(t *testing.T) {
	dir := t.TempDir()
	t.Cleanup(resetGatewayComposedSurfaces)
	opened, factory, _ := openSurfaceHost(t, dir, false)
	defer func() { _ = opened.Close(context.Background()) }()
	contributions, err := newGatewayPortalCatalogContributions()
	if err != nil {
		t.Fatal(err)
	}
	for _, lease := range startPortalTestBaseline(t, contributions) {
		defer lease.Stop()
	}
	source := newGatewayPortalCatalogSource(nil, nil, nil, contributions)
	bindGatewayComposedSurfaces(opened, source)
	publishSurfaceManifests(t, contributions)
	first := surfaceCaptureStable(t, opened, dir)
	surfaceRequireResources(t, first)

	gw, builder, hub := composedSurfaceHTTPParts()
	cfg := ebusgateway.DefaultConfig()
	cfg.HTTPAddr = "127.0.0.1:0"
	cfg.MDNSAdvertise = false
	cfg.PortalPath = ""
	cfg.PortalConfigRoot = dir
	cfg.RuntimeStatePath = filepath.Join(dir, "runtime_state.json")
	cfg.GraphQLPath = "/graphql"
	cfg.MCPPath = "/mcp"
	cfg.MetricsPath = "/metrics"
	server, advertiser, err := startHTTPServer(context.Background(), cfg, gw, builder, hub,
		nil, nil, nil, nil, source, nil, nil, ebusgateway.NewBusObservabilityStore(cfg), nil, http.NotFoundHandler(), nil, nil, nil, gatewayBuildInfo{ReleaseVersion: "0.0.0-fixture"}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if advertiser != nil {
			_ = advertiser.Close()
		}
		_ = shutdownHTTPControlPlane(server)
	}()
	base := "http://" + server.Addr
	surfaceAssertSlotHeld(t, dir)
	surfaceAssertDriverVisible(t, base, contributions)
	surfaceAssertSemanticParity(t, base)
	surfaceAssertNativeMCP(t, base, factory)

	if err := shutdownHTTPControlPlane(server); err != nil {
		t.Fatal(err)
	}
	resetGatewayComposedSurfaces()
	surfaceAssertSlotReleased(t, dir)
	if err := opened.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	reopened, _, _ := openSurfaceHost(t, dir, false)
	defer func() { _ = reopened.Close(context.Background()) }()
	second := surfaceCaptureStable(t, reopened, dir)
	surfaceSame(t, second, first)
}

func surfaceAssertSlotHeld(t *testing.T, dir string) {
	t.Helper()
	sum := sha256.Sum256([]byte(gatewaySemanticMetricTarget))
	name := fmt.Sprintf("slot-%x", sum)
	lock := filepath.Join(dir, name+".lock")
	if _, err := os.Stat(lock); err != nil {
		t.Fatalf("metric slot lock: %v", err)
	}
	if _, err := semanticmetrics.NewSlotFileStore(dir, gatewaySemanticMetricTarget); err == nil {
		t.Fatal("metric slot lock was not held")
	}
}

func surfaceAssertSlotReleased(t *testing.T, dir string) {
	t.Helper()
	store, err := semanticmetrics.NewSlotFileStore(dir, gatewaySemanticMetricTarget)
	if err != nil {
		t.Fatalf("metric slot lock was not released: %v", err)
	}
	_ = store.Close()
}

func surfaceAssertDriverVisible(t *testing.T, base string, contributions *gatewayPortalCatalogContributions) {
	t.Helper()
	catalog := surfacePost(t, base+"/graphql/portal/v1", map[string]any{"operationName": "PortalCatalogV1", "variables": map[string]any{}})
	for _, want := range []string{`"portal.pv"`, `"portal.storage"`, `"portal.evse"`, `"portal.surface.one"`, `"portal.surface.two"`, `"resource:surface:surface.one"`, `"resource:surface:surface.two"`, `"pairing"`} {
		if !bytes.Contains(catalog, []byte(want)) {
			t.Fatalf("portal catalog omitted %s: %s", want, catalog)
		}
	}
	metrics := surfaceGet(t, base+"/metrics")
	for _, want := range []string{"helianthus_native_count", `driver_instance="surface.one"`, `driver_instance="surface.two"`, `point="count"`} {
		if !bytes.Contains(metrics, []byte(want)) {
			t.Fatalf("metrics omitted %s: %s", want, metrics)
		}
	}
	names := surfaceToolNames(t, base+"/mcp")
	for _, want := range []string{mcp.SemanticResourcesListTool, mcp.SemanticTraverseTool, mcp.SemanticLensGetTool, mcp.SemanticCatalogGetTool, "driver_namespaces_list", "driver_members_list", "driver_member_get", "driver_method_call"} {
		if !names[want] {
			t.Fatalf("tools/list missing %s", want)
		}
	}
	roster := surfaceCallTool(t, base+"/mcp", "driver_namespaces_list", map[string]any{})
	if !bytes.Contains(roster, []byte(surfaceDriverKind)) || !bytes.Contains(roster, []byte("surface.one")) || !bytes.Contains(roster, []byte("surface.two")) {
		t.Fatalf("namespace roster = %s", roster)
	}
	_ = contributions
}

func surfaceAssertNativeMCP(t *testing.T, base string, factory *surfaceDriverFactory) {
	t.Helper()
	for _, key := range []driver.InstanceKey{"surface.one", "surface.two"} {
		state := factory.state(key)
		member := map[string]any{"namespace": string(surfaceDriverKind), "instance": string(key), "node": string(state.transport.ID), "local": "count"}
		body := surfaceCallTool(t, base+"/mcp", "driver_member_get", map[string]any{"key": member})
		if got := string(surfaceMemberEncoded(t, body)); got != surfacePointValue(key) {
			t.Fatalf("member %s = %q", key, got)
		}
		before := state.pairing.Load()
		input := base64.StdEncoding.EncodeToString([]byte("mcp-call"))
		called := surfaceCallTool(t, base+"/mcp", "driver_method_call", map[string]any{"key": map[string]any{
			"namespace": string(surfaceDriverKind), "instance": string(key), "node": string(state.root.ID), "local": "pairing",
		}, "input": input})
		if got := string(surfaceOutcomeNative(t, called)); got != string(key)+":mcp-call" || state.pairing.Load() != before+1 {
			t.Fatalf("method %s native = %q calls %d", key, got, state.pairing.Load())
		}
	}
}

func surfaceEnvelopeData(t *testing.T, body []byte) json.RawMessage {
	t.Helper()
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("tool envelope: %v", err)
	}
	return envelope.Data
}

func surfaceMemberEncoded(t *testing.T, body []byte) []byte {
	t.Helper()
	var member struct {
		Value struct {
			Encoded []byte `json:"Encoded"`
		} `json:"value"`
	}
	if err := json.Unmarshal(surfaceEnvelopeData(t, body), &member); err != nil {
		t.Fatalf("member value: %v", err)
	}
	return member.Value.Encoded
}

func surfaceOutcomeNative(t *testing.T, body []byte) []byte {
	t.Helper()
	var outcome struct {
		Native []byte `json:"native"`
	}
	if err := json.Unmarshal(surfaceEnvelopeData(t, body), &outcome); err != nil {
		t.Fatalf("method outcome: %v", err)
	}
	return outcome.Native
}

type surfaceGraphRead struct {
	name      string
	operation string
	query     string
	tool      string
	variables map[string]any
}

func surfaceGraphReads() []surfaceGraphRead {
	return []surfaceGraphRead{
		{name: "resources", operation: "SemanticResources", query: surfaceResourcesQuery, tool: mcp.SemanticResourcesListTool, variables: map[string]any{"page_size": 32}},
		{name: "traversal", operation: "SemanticTraversal", query: surfaceTraversalQuery, tool: mcp.SemanticTraverseTool, variables: map[string]any{"entry": map[string]any{"kind": "sites"}, "direction": "outgoing", "depth": 1, "page_size": 1}},
		{name: "lens", operation: "SemanticLens", query: surfaceLensQuery, tool: mcp.SemanticLensGetTool, variables: map[string]any{"entry": map[string]any{"kind": "lens_roots", "lens": "lens.energy_flow"}, "lens": "lens.energy_flow", "direction": "outgoing", "depth": 1, "page_size": 1}},
		{name: "catalog", operation: "SemanticElementCatalog", query: surfaceCatalogQuery, tool: mcp.SemanticCatalogGetTool, variables: map[string]any{}},
	}
}

func surfaceAssertSemanticParity(t *testing.T, base string) {
	t.Helper()
	remote := surfaceRemoteHandler(t)
	for _, read := range surfaceGraphReads() {
		localBody, localCode := surfaceLocalGraph(t, base+"/graphql", read)
		remoteBody, remoteCode := surfaceRemoteGraph(t, remote, read)
		mcpBody, mcpCode := surfaceMCPGraph(t, base+"/mcp", read)
		if localCode != "" || remoteCode != "" || mcpCode != "" {
			if localCode != remoteCode || localCode != mcpCode {
				t.Fatalf("%s codes local=%s remote=%s mcp=%s", read.name, localCode, remoteCode, mcpCode)
			}
			continue
		}
		if !bytes.Equal(localBody, remoteBody) || !bytes.Equal(localBody, mcpBody) {
			t.Fatalf("%s canonical bytes differ local=%d remote=%d mcp=%d", read.name, len(localBody), len(remoteBody), len(mcpBody))
		}
	}
	rejected := surfaceGraphRead{name: "rejected", operation: "SemanticResources", query: "query Other { semantic_resources(page_size: 1) }", variables: map[string]any{"page_size": 1}}
	if _, code := surfaceRemoteGraph(t, remote, rejected); code != "QUERY_REJECTED" {
		t.Fatalf("remote non-fixed document code = %s", code)
	}
	bad := surfaceGraphRead{name: "token", operation: "SemanticResources", query: surfaceResourcesQuery, tool: mcp.SemanticResourcesListTool, variables: map[string]any{"page_size": 1, "page_token": "malformed"}}
	localBody, localCode := surfaceLocalGraph(t, base+"/graphql", bad)
	remoteBody, remoteCode := surfaceRemoteGraph(t, remote, bad)
	mcpBody, mcpCode := surfaceMCPGraph(t, base+"/mcp", bad)
	if localCode != "invalid_value" || remoteCode != "invalid_value" || mcpCode != "invalid_value" {
		t.Fatalf("malformed token codes local=%s remote=%s mcp=%s bodies %s %s %s", localCode, remoteCode, mcpCode, localBody, remoteBody, mcpBody)
	}
}

func surfaceRemoteHandler(t *testing.T) http.Handler {
	t.Helper()
	handler, err := m2mgraphql.NewHandler(m2mgraphql.Config{
		SemanticResources:      gatewaySemanticResourcesRead,
		SemanticTraversal:      gatewaySemanticTraversalRead,
		SemanticLens:           gatewaySemanticLensRead,
		SemanticElementCatalog: gatewaySemanticElementCatalogRead,
	})
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func surfaceLocalGraph(t *testing.T, url string, read surfaceGraphRead) ([]byte, string) {
	t.Helper()
	body := surfacePost(t, url, map[string]any{"query": read.query, "variables": read.variables, "operationName": read.operation})
	return surfaceGraphPayload(t, body, surfaceGraphRoot(read))
}

func surfaceRemoteGraph(t *testing.T, handler http.Handler, read surfaceGraphRead) ([]byte, string) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"operationName": read.operation, "query": read.query, "variables": read.variables})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/graphql/m2m/v1", bytes.NewReader(raw))
	req = req.WithContext(m2mgraphql.WithRateLimitKey(req.Context(), t.Name()+"-"+read.name+"-"+read.query))
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("remote %s status %d %s", read.name, res.Code, res.Body.String())
	}
	return surfaceGraphPayload(t, res.Body.Bytes(), surfaceGraphRoot(read))
}

func surfaceGraphRoot(read surfaceGraphRead) string {
	switch read.operation {
	case "SemanticTraversal":
		return "semantic_traversal"
	case "SemanticLens":
		return "semantic_lens"
	case "SemanticElementCatalog":
		return "semantic_element_catalog"
	default:
		return "semantic_resources"
	}
}

func surfaceGraphPayload(t *testing.T, body []byte, root string) ([]byte, string) {
	t.Helper()
	var decoded struct {
		Data   map[string]json.RawMessage `json:"data"`
		Errors []struct {
			Extensions map[string]string `json:"extensions"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("graph response: %v %s", err, body)
	}
	if len(decoded.Errors) > 0 {
		return body, decoded.Errors[0].Extensions["code"]
	}
	raw := decoded.Data[root]
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		t.Fatalf("graph %s payload: %v %s", root, err, body)
	}
	return []byte(text), ""
}

func surfaceMCPGraph(t *testing.T, url string, read surfaceGraphRead) ([]byte, string) {
	t.Helper()
	body := surfaceCallTool(t, url, read.tool, read.variables)
	var envelope struct {
		Data  json.RawMessage `json:"data"`
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("mcp envelope: %v %s", err, body)
	}
	if len(envelope.Data) == 0 || string(envelope.Data) == "null" {
		return body, envelope.Error.Code
	}
	return envelope.Data, ""
}

func surfaceToolNames(t *testing.T, url string) map[string]bool {
	t.Helper()
	body := surfacePost(t, url, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list", "params": map[string]any{}})
	var decoded struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tool := range decoded.Result.Tools {
		names[tool.Name] = true
	}
	return names
}

func surfaceCallTool(t *testing.T, url, name string, args map[string]any) []byte {
	t.Helper()
	body := surfacePost(t, url, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
	var decoded struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("tools/call %s: %v %s", name, err, body)
	}
	if decoded.Error != nil {
		t.Fatalf("tools/call %s rpc error: %s", name, decoded.Error.Message)
	}
	if len(decoded.Result.Content) == 0 {
		t.Fatalf("tools/call %s has no content: %s", name, body)
	}
	return []byte(decoded.Result.Content[0].Text)
}

func surfacePost(t *testing.T, url string, payload any) []byte {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.Post(url, "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("POST %s status %d %s", url, res.StatusCode, body)
	}
	return body
}

func surfaceGet(t *testing.T, url string) []byte {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status %d %s", url, res.StatusCode, body)
	}
	return body
}
