package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	ebusgateway "github.com/Project-Helianthus/helianthus-gateway"
	"github.com/Project-Helianthus/helianthus-gateway/internal/modbusadapter"
	sunspectest "github.com/Project-Helianthus/helianthus-gateway/internal/modbusadapter/testfixture"
	"github.com/Project-Helianthus/helianthus-gateway/internal/semanticview"
	"github.com/Project-Helianthus/helianthus-gateway/internal/semhub"
	"github.com/Project-Helianthus/helianthus-gateway/mcp"
	"github.com/Project-Helianthus/helianthus-gateway/portal"
	"github.com/Project-Helianthus/helianthus-gateway/portal/catalogv1"
	"github.com/Project-Helianthus/helianthus-gateway/portal/contributionv1"
	modbus "github.com/Project-Helianthus/helianthus-modbus"
	modbusreg "github.com/Project-Helianthus/helianthus-modbusreg"
	"github.com/Project-Helianthus/helianthus-modbusreg/fronius"
	"github.com/Project-Helianthus/helianthus-modbusreg/sunspec"
	semreg "github.com/Project-Helianthus/helianthus-semreg/semreg/v1"
	"github.com/Project-Helianthus/helianthus-semreg/semreg/v1/graph"
)

type portalCatalogBarrierSource struct {
	contributions *gatewayPortalCatalogContributions
	entered       chan struct{}
	release       chan struct{}
	once          sync.Once
}

func (s *portalCatalogBarrierSource) Capture(time.Time) (catalogv1.Capture, error) {
	s.once.Do(func() { close(s.entered) })
	<-s.release
	return catalogv1.Capture{}, nil
}
func (s *portalCatalogBarrierSource) PortalCatalogContributions() *gatewayPortalCatalogContributions {
	return s.contributions
}

func startPortalTestLease(t *testing.T, contributions *gatewayPortalCatalogContributions, manifest contributionv1.Manifest) *gatewayPortalContributionLifecycle {
	t.Helper()
	lease, err := startGatewayPortalContributionLifecycle(contributions, contributionv1.DriverGeneration{DriverID: manifest.Contributor.DriverID, Generation: 1}, []contributionv1.Manifest{manifest})
	if err != nil {
		t.Fatal(err)
	}
	return lease
}

func startPortalTestBaseline(t *testing.T, contributions *gatewayPortalCatalogContributions) []*gatewayPortalContributionLifecycle {
	t.Helper()
	return []*gatewayPortalContributionLifecycle{
		startPortalTestLease(t, contributions, gatewayPortalPVDescriptor()),
		startPortalTestLease(t, contributions, gatewayPortalStorageDescriptor()),
		startPortalTestLease(t, contributions, gatewayPortalEVSEDescriptor()),
	}
}

func TestPortalCatalogDescriptorsUseAcceptedSemRegMetadata(t *testing.T) {
	index, err := newGatewayPortalSemanticIndex()
	if err != nil {
		t.Fatal(err)
	}
	manifests := []contributionv1.Manifest{gatewayPortalPVDescriptor(), gatewayPortalStorageDescriptor(), gatewayPortalEVSEDescriptor()}
	if len(manifests) != 3 {
		t.Fatalf("descriptor count = %d; want PV, Storage and EVSE", len(manifests))
	}
	want := map[string]struct{}{"pv.ac.frequency": {}, "storage.state.soc": {}, "evse.limit.configured_current": {}}
	for _, manifest := range manifests {
		if _, err := contributionv1.CanonicalDigest(manifest, index); err != nil {
			t.Fatalf("descriptor %s does not validate against SemReg metadata: %v", manifest.ManifestID, err)
		}
		for _, field := range manifest.Fields {
			delete(want, field.Ref.ID)
		}
	}
	if len(want) != 0 {
		t.Fatalf("missing production descriptor fields: %v", want)
	}
}

func TestPortalCatalogProductionResourcesBindExactSemanticContext(t *testing.T) {
	for _, descriptor := range []contributionv1.Manifest{gatewayPortalPVDescriptor(), gatewayPortalStorageDescriptor(), gatewayPortalEVSEDescriptor()} {
		t.Run(descriptor.ManifestID, func(t *testing.T) {
			serviceRef := semanticRef(descriptor.Fields[0].ServiceRef)
			capabilityRef := semanticRef(descriptor.Fields[0].CapabilityRef)
			snapshot := semreg.Snapshot{
				AssetID:    "asset",
				SnapshotID: "snapshot",
				Services: []semreg.ServiceInstance{{
					InstanceID: "service-instance", AssetID: "asset", Definition: serviceRef,
					BindingID: "binding", SourceEpochID: "epoch", DriverGeneration: "7",
				}},
				Capabilities: []semreg.CapabilityInstance{{
					InstanceID: "capability-instance", AssetID: "asset", Definition: capabilityRef,
					ServiceInstance: "service-instance", BindingID: "binding", SourceEpochID: "epoch", DriverGeneration: "7",
				}},
				Sources:  []semreg.SourceDescriptor{{SourceID: "source", SourceEpochID: "epoch", State: semreg.SourceCurrent}},
				Bindings: []semreg.NativeBinding{{BindingID: "binding", AssetID: "asset", SourceID: "source", SourceEpochID: "epoch", DriverGeneration: "7", State: semreg.BindingCurrent}},
			}
			resources, _ := appendPortalSnapshot(nil, nil, descriptor, snapshot, semreg.EvaluationView{}, []semreg.Selection{}, map[string]any{})
			if len(resources) != 1 || resources[0].ID != descriptor.Groups[0].ResourceContext || resources[0].Source.AssetID != "asset" || resources[0].ServiceID != descriptor.Fields[0].ServiceRef.ID || resources[0].CapabilityID != descriptor.Fields[0].CapabilityRef.ID || resources[0].Source.DriverGeneration != 7 {
				t.Fatalf("resource context=%+v", resources)
			}
			snapshot.Capabilities = nil
			resources, _ = appendPortalSnapshot(nil, nil, descriptor, snapshot, semreg.EvaluationView{}, []semreg.Selection{}, map[string]any{})
			if len(resources) != 0 {
				t.Fatalf("resource emitted without exact capability context: %+v", resources)
			}
		})
	}
}

// Each production descriptor is published under a runtime driver that differs
// from its contribution driver; with the hub's resolution the one source of
// the asset is still the owner's and the resource keeps the descriptor's
// attribution.
func TestPortalCatalogAttributesProductionRuntimeDriversToTheirDescriptors(t *testing.T) {
	contributions, err := newGatewayPortalCatalogContributions()
	if err != nil {
		t.Fatal(err)
	}
	for _, lease := range startPortalTestBaseline(t, contributions) {
		defer lease.Stop()
	}
	for _, family := range []struct {
		descriptor contributionv1.Manifest
		runtime    string
	}{{gatewayPortalPVDescriptor(), "pv_sunspec"}, {gatewayPortalStorageDescriptor(), growattStorageDriver}, {gatewayPortalEVSEDescriptor(), teslaEVSEDriver}} {
		t.Run(family.descriptor.ManifestID, func(t *testing.T) {
			if family.runtime == family.descriptor.Contributor.DriverID {
				t.Fatalf("runtime driver %q equals the contribution driver", family.runtime)
			}
			service := semanticRef(family.descriptor.Fields[0].ServiceRef)
			capability := semanticRef(family.descriptor.Fields[0].CapabilityRef)
			snapshot := portalContributorSnapshot(service, capability, []string{"owner"}, "owner")
			attribution := portalAttribution{drivers: portalTestDrivers{"source:owner": family.runtime}, accepted: contributions.snapshot().Accepted}.ownedBy("source:owner")
			resources, _, diagnostics := portalSnapshotCapture(attribution, family.descriptor, snapshot, semreg.EvaluationView{}, []semreg.Selection{}, map[string]any{})
			if len(diagnostics) != 0 || len(resources) != 1 {
				t.Fatalf("resources=%d diagnostics=%+v", len(resources), diagnostics)
			}
			if got := resources[0]; got.ContributionDriverID != family.descriptor.Contributor.DriverID || got.ContributionManifestID != family.descriptor.ManifestID || got.Source.BindingID != "binding:owner" {
				t.Fatalf("resource attributed to %s %s on %s", got.ContributionDriverID, got.ContributionManifestID, got.Source.BindingID)
			}
		})
	}
}

func TestAppendPortalSnapshotUsesMatchedCurrentBindingAndEpochScalars(t *testing.T) {
	descriptor := gatewayPortalPVDescriptor()
	serviceRef := semanticRef(descriptor.Fields[0].ServiceRef)
	capabilityRef := semanticRef(descriptor.Fields[0].CapabilityRef)
	const binding = semreg.NativeBindingID("binding:current")
	const epoch = semreg.SourceEpochID("epoch:current")
	snapshot := semreg.Snapshot{
		AssetID: "asset", SnapshotID: "snapshot",
		Services:     []semreg.ServiceInstance{{InstanceID: "service:current", AssetID: "asset", Definition: serviceRef, BindingID: binding, SourceEpochID: epoch, DriverGeneration: "7"}},
		Capabilities: []semreg.CapabilityInstance{{InstanceID: "capability:current", AssetID: "asset", Definition: capabilityRef, ServiceInstance: "service:current", BindingID: binding, SourceEpochID: epoch, DriverGeneration: "7"}},
	}
	for i := 0; i < 8; i++ {
		historicalBinding := semreg.NativeBindingID(fmt.Sprintf("binding:historical:%d", i))
		historicalEpoch := semreg.SourceEpochID(fmt.Sprintf("epoch:historical:%d", i))
		snapshot.Sources = append(snapshot.Sources, semreg.SourceDescriptor{SourceID: semreg.SourceID(fmt.Sprintf("source:historical:%d", i)), SourceEpochID: historicalEpoch, State: semreg.SourceRetired})
		snapshot.Bindings = append(snapshot.Bindings, semreg.NativeBinding{BindingID: historicalBinding, AssetID: "asset", SourceEpochID: historicalEpoch, DriverGeneration: "6", State: semreg.BindingRetired})
	}
	snapshot.Sources = append(snapshot.Sources, semreg.SourceDescriptor{SourceID: "source:current", SourceEpochID: epoch, State: semreg.SourceCurrent})
	snapshot.Bindings = append(snapshot.Bindings, semreg.NativeBinding{BindingID: binding, AssetID: "asset", SourceID: "source:current", SourceEpochID: epoch, DriverGeneration: "7", State: semreg.BindingCurrent})
	resources, _ := appendPortalSnapshot(nil, nil, descriptor, snapshot, semreg.EvaluationView{}, []semreg.Selection{}, map[string]any{})
	if len(resources) != 1 {
		t.Fatalf("resources=%+v", resources)
	}
	if got := resources[0].Source; got.BindingID != string(binding) || got.SourceEpoch != string(epoch) {
		t.Fatalf("capture scalars binding=%q epoch=%q", got.BindingID, got.SourceEpoch)
	}
	if !bytes.Contains(resources[0].Source.Snapshot, []byte("binding:historical:7")) || !bytes.Contains(resources[0].Source.Snapshot, []byte("binding:current")) {
		t.Fatalf("snapshot lost nested binding evidence: %s", resources[0].Source.Snapshot)
	}
	withoutResource := func(name string, mutate func(*semreg.Snapshot)) {
		t.Helper()
		candidate := snapshot
		candidate.Sources = append([]semreg.SourceDescriptor(nil), snapshot.Sources...)
		candidate.Bindings = append([]semreg.NativeBinding(nil), snapshot.Bindings...)
		candidate.Services = append([]semreg.ServiceInstance(nil), snapshot.Services...)
		candidate.Capabilities = append([]semreg.CapabilityInstance(nil), snapshot.Capabilities...)
		mutate(&candidate)
		got, _ := appendPortalSnapshot(nil, nil, descriptor, candidate, semreg.EvaluationView{}, []semreg.Selection{}, map[string]any{})
		if len(got) != 0 {
			t.Fatalf("%s emitted resource: %+v", name, got)
		}
	}
	withoutResource("inconsistent capability epoch", func(candidate *semreg.Snapshot) { candidate.Capabilities[0].SourceEpochID = "epoch:inconsistent" })
	withoutResource("inconsistent capability generation", func(candidate *semreg.Snapshot) { candidate.Capabilities[0].DriverGeneration = "8" })
	withoutResource("missing bindings", func(candidate *semreg.Snapshot) { candidate.Bindings = nil })
	withoutResource("retired binding", func(candidate *semreg.Snapshot) {
		candidate.Bindings[len(candidate.Bindings)-1].State = semreg.BindingRetired
	})
	withoutResource("binding generation mismatch", func(candidate *semreg.Snapshot) { candidate.Bindings[len(candidate.Bindings)-1].DriverGeneration = "8" })
	withoutResource("missing sources", func(candidate *semreg.Snapshot) { candidate.Sources = nil })
	withoutResource("retired source", func(candidate *semreg.Snapshot) {
		candidate.Sources[len(candidate.Sources)-1].State = semreg.SourceRetired
	})
	withoutResource("source epoch mismatch", func(candidate *semreg.Snapshot) {
		candidate.Sources[len(candidate.Sources)-1].SourceEpochID = "epoch:inconsistent"
	})
}

// TestPortalCatalogRuntimeHandlerInstallsProductionDescriptors mounts the
// production portal mux. Both catalog composers receive the host invoker and
// revalidator. The claim cases vary every claim field:
//
//   - CatalogRevision: Composer.Invoke compares the served revision and returns ErrStale.
//   - Digest: a non-empty wrong digest fails matchesDigest and Invoke returns ErrStale.
//   - DriverID, ManifestID, ManifestVersion, ActionID, ResourceID, CapabilityID,
//     SnapshotID, Revision, BindingID, SourceEpoch and DriverGeneration: matchesClaim
//     returns ErrUnauthorized before the revalidator runs.
//   - IdempotencyKey empty, Deadline zero and Deadline in the past: Invoke returns
//     ErrStale before it looks the action up.
//
// The accepted call is the one that reaches gatewayHostActionAdmission.Revalidate
// and then Invoke. Replacing the idempotency key changes the native procedure
// input. The member key of the setup procedure is varied the same way:
//
//   - Namespace: Dispatch builds the native segment from the key. A different
//     namespace does not call the registered procedure.
//   - Instance: hookNode requires the node instance. A different instance does
//     not call the procedure.
//   - Node: hookNode refuses a device node before prepare, so the device
//     procedure is not called. The instance procedure is called while that
//     device does not exist yet.
//   - Local: a different local id does not call pairing.
func TestPortalCatalogRuntimeHandlerInstallsProductionDescriptors(t *testing.T) {
	dir := t.TempDir()
	t.Cleanup(resetGatewayComposedSurfaces)
	opened, factory, started := openSurfaceHost(t, dir, true)
	t.Cleanup(func() { _ = opened.Close(context.Background()) })
	t.Cleanup(func() {
		factory.release()
		if started == nil {
			return
		}
		select {
		case err := <-started:
			if err != nil {
				t.Errorf("surface start: %v", err)
			}
		case <-time.After(8 * time.Second):
			t.Errorf("surface start did not return")
		}
	})
	contributions, err := newGatewayPortalCatalogContributions()
	if err != nil {
		t.Fatal(err)
	}
	leases := startPortalTestBaseline(t, contributions)
	defer func() {
		for _, lease := range leases {
			lease.Stop()
		}
	}()
	source := newGatewayPortalCatalogSource(nil, nil, nil, contributions)
	bindGatewayComposedSurfaces(opened, source)
	publishSurfaceManifests(t, contributions)
	mux := http.NewServeMux()
	registerHTTPPortalRoutes(mux, "/portal", portal.Options{SharedSemanticView: func(context.Context) (semanticview.View, error) {
		return semanticview.View{}, nil
	}}, source)
	paths := []string{"/graphql/portal/v1", "/portal/graphql/portal/v1"}
	for _, path := range paths {
		body := portalMuxPost(t, mux, path, `{"operationName":"PortalCatalogV1","variables":{}}`)
		for _, want := range []string{`"portal.pv"`, `"portal.storage"`, `"portal.evse"`, `"pairing"`, `"resource:surface:`} {
			if !bytes.Contains(body, []byte(want)) {
				t.Fatalf("%s omitted %s: %s", path, want, body)
			}
		}
		portalAssertTargetReport(t, mux, path, contributions)
	}
	catalog := portalDecodeCatalog(t, portalMuxPost(t, mux, paths[0], `{"operationName":"PortalCatalogV1","variables":{}}`))
	again := portalDecodeCatalog(t, portalMuxPost(t, mux, paths[0], `{"operationName":"PortalCatalogV1","variables":{}}`))
	if again.CatalogRevision != catalog.CatalogRevision {
		t.Fatalf("catalog revision moved %q -> %q resources %d -> %d", catalog.CatalogRevision, again.CatalogRevision, len(catalog.Resources), len(again.Resources))
	}
	before := factory.pairingSum()
	keys := []string{"marker-gateway", "marker-contribution"}
	for i, path := range paths {
		// Each composer seals its own catalog revision. The claim is the catalog that path just served.
		served := catalog
		if path != paths[0] {
			served = portalDecodeCatalog(t, portalMuxPost(t, mux, path, `{"operationName":"PortalCatalogV1","variables":{}}`))
		}
		action, digest := portalSurfaceAction(t, served)
		claim := portalSurfaceClaim(served, action, digest, keys[i])
		native := portalInvokeNative(t, mux, path, claim)
		if !strings.Contains(native, keys[i]) {
			t.Fatalf("%s native = %s", path, native)
		}
	}
	if factory.pairingSum() != before+2 {
		t.Fatalf("pairing calls = %d, want %d", factory.pairingSum(), before+2)
	}
	rejectAction, rejectDigest := portalSurfaceAction(t, catalog)
	portalRejectClaimFields(t, mux, paths[0], portalSurfaceClaim(catalog, rejectAction, rejectDigest, "marker-reject"), factory)

	state := factory.readyState()
	if state == nil || state.device.ID != "" {
		t.Fatalf("setup ran after the device existed: %+v", state)
	}
	marker := []byte("before-device")
	calls := state.pairing.Load()
	outcome, err := (gatewaySetupSurface{tree: opened.site.Tree()}).Dispatch(context.Background(), hookMemberKey(string(surfaceDriverKind), state.key, state.root, "pairing"), marker)
	if err != nil || !bytes.Contains(outcome.Native, marker) || state.pairing.Load() != calls+1 || state.deviceCalls.Load() != 0 {
		t.Fatalf("instance setup before the device: %v %s calls %d", err, outcome.Native, state.pairing.Load())
	}
	setup := gatewaySetupSurface{tree: opened.site.Tree()}
	if err := dispatchDangles(setup.Dispatch(context.Background(), hookMemberKey("family.other", state.key, state.root, "pairing"), marker)); err != nil || state.pairing.Load() != calls+1 {
		t.Fatalf("wrong namespace called pairing: %v calls %d", err, state.pairing.Load())
	}
	if _, err := setup.Dispatch(context.Background(), hookMemberKey(string(surfaceDriverKind), "surface.other", state.root, "pairing"), marker); err == nil || state.pairing.Load() != calls+1 {
		t.Fatalf("wrong instance = %v calls %d", err, state.pairing.Load())
	}
	if err := dispatchDangles(setup.Dispatch(context.Background(), hookMemberKey(string(surfaceDriverKind), state.key, state.root, "other"), marker)); err != nil || state.pairing.Load() != calls+1 {
		t.Fatalf("wrong local called pairing: %v calls %d", err, state.pairing.Load())
	}
	factory.release()
	factory.waitRunning(t, opened)
	if state.device.ID == "" {
		t.Fatal("device node was not registered")
	}
	if _, err := setup.Dispatch(context.Background(), hookMemberKey(string(surfaceDriverKind), state.key, state.device, "pairing"), marker); !errors.Is(err, errGatewayHookLevel) || state.deviceCalls.Load() != 0 {
		t.Fatalf("device setup = %v device calls %d", err, state.deviceCalls.Load())
	}
	body := portalMuxPost(t, mux, paths[0], `{"operationName":"PortalCatalogV1","variables":{}}`)
	for _, want := range []string{`"resource:surface:surface.one"`, `"resource:surface:surface.two"`} {
		if !bytes.Contains(body, []byte(want)) {
			t.Fatalf("catalog omitted %s after both instances started: %s", want, body)
		}
	}
	entries, err := contributions.index.catalogElements(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	portalAssertPresentationExcluded(t, entries, body)
}

func portalMuxPost(t *testing.T, mux *http.ServeMux, path, payload string) []byte {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(payload))
	res := httptest.NewRecorder()
	mux.ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("%s status=%d body=%s", path, res.Code, res.Body.String())
	}
	return res.Body.Bytes()
}

func portalDecodeCatalog(t *testing.T, body []byte) catalogv1.Catalog {
	t.Helper()
	var decoded struct {
		Data struct {
			PortalCatalogV1 catalogv1.Catalog `json:"portalCatalogV1"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Errors) != 0 {
		t.Fatalf("catalog errors: %+v", decoded.Errors)
	}
	return decoded.Data.PortalCatalogV1
}

func portalSurfaceAction(t *testing.T, catalog catalogv1.Catalog) (catalogv1.Action, string) {
	t.Helper()
	for _, action := range catalog.Actions {
		if action.NativeMemberID != "pairing" || action.ResourceID == "" {
			continue
		}
		for _, item := range catalog.Contributions {
			if item.DriverID == action.ContributionDriverID && item.ManifestID == action.ContributionManifestID && item.ManifestVersion == action.ContributionManifestVersion && item.Digest != "" {
				return action, item.Digest
			}
		}
	}
	t.Fatalf("pairing action was not discovered: %+v", catalog.Actions)
	return catalogv1.Action{}, ""
}

func portalSurfaceClaim(catalog catalogv1.Catalog, action catalogv1.Action, digest, key string) catalogv1.Claims {
	return catalogv1.Claims{
		CatalogRevision: catalog.CatalogRevision, Digest: digest, DriverID: action.ContributionDriverID,
		ManifestID: action.ContributionManifestID, ManifestVersion: action.ContributionManifestVersion,
		ActionID: action.ID, ResourceID: action.ResourceID, CapabilityID: action.CapabilityID,
		SnapshotID: action.Source.SnapshotID, Revision: action.Source.Revision, BindingID: action.Source.BindingID,
		SourceEpoch: action.Source.SourceEpoch, IdempotencyKey: key, DriverGeneration: action.Source.DriverGeneration,
		Deadline: time.Now().Add(2 * time.Minute),
	}
}

func portalInvokeNative(t *testing.T, mux *http.ServeMux, path string, claim catalogv1.Claims) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"operationName": "PortalActionInvokeV1", "variables": map[string]any{"claim": claim}})
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Data struct {
			PortalActionInvokeV1 map[string]string `json:"portalActionInvokeV1"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(portalMuxPost(t, mux, path, string(raw)), &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Errors) != 0 || decoded.Data.PortalActionInvokeV1["native"] == "" {
		t.Fatalf("invoke errors=%+v result=%+v revision=%q deadline=%s key=%q digest=%q snap=%q", decoded.Errors, decoded.Data.PortalActionInvokeV1, claim.CatalogRevision, claim.Deadline.Format(time.RFC3339Nano), claim.IdempotencyKey, claim.Digest, claim.SnapshotID)
	}
	return decoded.Data.PortalActionInvokeV1["native"]
}

func portalRejectClaimFields(t *testing.T, mux *http.ServeMux, path string, claim catalogv1.Claims, factory *surfaceDriverFactory) {
	t.Helper()
	// CatalogRevision is the composer's revision, not the source snapshot.
	catalog := portalDecodeCatalog(t, portalMuxPost(t, mux, path, `{"operationName":"PortalCatalogV1","variables":{}}`))
	claim.CatalogRevision = catalog.CatalogRevision
	cases := []struct {
		name   string
		mutate func(*catalogv1.Claims)
		want   string
	}{
		{"catalog revision", func(c *catalogv1.Claims) { c.CatalogRevision += "-mutated" }, catalogv1.ErrStale.Error()},
		{"digest", func(c *catalogv1.Claims) { c.Digest += "-mutated" }, catalogv1.ErrStale.Error()},
		{"driver", func(c *catalogv1.Claims) { c.DriverID += "-mutated" }, catalogv1.ErrUnauthorized.Error()},
		{"manifest", func(c *catalogv1.Claims) { c.ManifestID += "-mutated" }, catalogv1.ErrUnauthorized.Error()},
		{"manifest version", func(c *catalogv1.Claims) { c.ManifestVersion = "9.9.9" }, catalogv1.ErrUnauthorized.Error()},
		{"action", func(c *catalogv1.Claims) { c.ActionID += "-mutated" }, catalogv1.ErrUnauthorized.Error()},
		{"resource", func(c *catalogv1.Claims) { c.ResourceID += "-mutated" }, catalogv1.ErrUnauthorized.Error()},
		{"capability", func(c *catalogv1.Claims) { c.CapabilityID += "-mutated" }, catalogv1.ErrUnauthorized.Error()},
		{"snapshot", func(c *catalogv1.Claims) { c.SnapshotID += "-mutated" }, catalogv1.ErrUnauthorized.Error()},
		{"revision", func(c *catalogv1.Claims) { c.Revision += "-mutated" }, catalogv1.ErrUnauthorized.Error()},
		{"binding", func(c *catalogv1.Claims) { c.BindingID += "-mutated" }, catalogv1.ErrUnauthorized.Error()},
		{"source epoch", func(c *catalogv1.Claims) { c.SourceEpoch += "-mutated" }, catalogv1.ErrUnauthorized.Error()},
		{"driver generation", func(c *catalogv1.Claims) { c.DriverGeneration++ }, catalogv1.ErrUnauthorized.Error()},
		{"idempotency key", func(c *catalogv1.Claims) { c.IdempotencyKey = "" }, catalogv1.ErrStale.Error()},
		{"deadline zero", func(c *catalogv1.Claims) { c.Deadline = time.Time{} }, catalogv1.ErrStale.Error()},
		{"deadline past", func(c *catalogv1.Claims) { c.Deadline = time.Now().Add(-time.Minute) }, catalogv1.ErrStale.Error()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mutated := claim
			tc.mutate(&mutated)
			before := factory.pairingSum()
			raw, err := json.Marshal(map[string]any{"operationName": "PortalActionInvokeV1", "variables": map[string]any{"claim": mutated}})
			if err != nil {
				t.Fatal(err)
			}
			body := portalMuxPost(t, mux, path, string(raw))
			var decoded struct {
				Errors []struct {
					Message string `json:"message"`
				} `json:"errors"`
			}
			if err := json.Unmarshal(body, &decoded); err != nil {
				t.Fatal(err)
			}
			if len(decoded.Errors) != 1 || decoded.Errors[0].Message != tc.want || factory.pairingSum() != before {
				t.Fatalf("mutation %s body=%s calls %d", tc.name, body, factory.pairingSum())
			}
		})
	}
}

func portalAssertTargetReport(t *testing.T, mux *http.ServeMux, path string, contributions *gatewayPortalCatalogContributions) {
	t.Helper()
	body := portalMuxPost(t, mux, path, `{"operationName":"PortalTargetReportV1","variables":{}}`)
	var decoded struct {
		Data struct {
			PortalTargetReportV1 catalogv1.TargetReport `json:"portalTargetReportV1"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	report := decoded.Data.PortalTargetReportV1
	if len(report.Descriptors) < 32 || report.TargetID != catalogv1.PortalTarget {
		t.Fatalf("%s target report has %d descriptors", path, len(report.Descriptors))
	}
	for _, descriptor := range report.Descriptors {
		if descriptor.Disposition != graph.DispositionUnsupported || descriptor.Reason != "catalog.reason.target_presentation" {
			t.Fatalf("descriptor %+v is graph content", descriptor)
		}
	}
	entries, err := contributions.index.catalogElements(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	portalAssertPresentationExcluded(t, entries, body)
}

func portalAssertPresentationExcluded(t *testing.T, entries []graph.Element, catalogBody []byte) {
	t.Helper()
	seen := 0
	for _, entry := range entries {
		if entry.ElementKind != graph.KindPresentation || entry.Presentation == nil || entry.Presentation.Target != graph.TargetPortal {
			continue
		}
		seen++
		if entry.Disposition != graph.DispositionUnsupported || string(entry.Reason) != "catalog.reason.target_presentation" || len(entry.Planes) != 0 {
			t.Fatalf("presentation %+v is in graph content", entry.Ref)
		}
		ref, ok := entry.Ref.DefinitionRef()
		if ok && bytes.Contains(catalogBody, []byte(`"`+string(ref.ID)+`"`)) && bytes.Contains(catalogBody, []byte(`"resource_id":"`+string(ref.ID)+`"`)) {
			t.Fatalf("presentation %s is a catalog resource", ref.ID)
		}
	}
	if seen < 32 {
		t.Fatalf("portal presentation descriptors = %d", seen)
	}
}

func TestPortalCatalogSourceLifecycleWithdrawsOwnedGenerations(t *testing.T) {
	contributions, err := newGatewayPortalCatalogContributions()
	if err != nil {
		t.Fatal(err)
	}
	leases := startPortalTestBaseline(t, contributions)
	source := newGatewayPortalCatalogSource(nil, nil, nil, contributions)
	h := newPortalCatalogV1Handler(source)
	request := func() []byte {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/graphql/portal/v1", bytes.NewBufferString(`{"operationName":"PortalCatalogV1","variables":{}}`))
		res := httptest.NewRecorder()
		h.ServeHTTP(res, req)
		if res.Code != http.StatusOK {
			t.Fatalf("handler status=%d body=%s", res.Code, res.Body.String())
		}
		return res.Body.Bytes()
	}
	if body := request(); !bytes.Contains(body, []byte(`"portal.pv"`)) || !bytes.Contains(body, []byte(`"portal.storage"`)) || !bytes.Contains(body, []byte(`"portal.evse"`)) {
		t.Fatalf("source lifecycle did not publish all owned descriptors: %s", body)
	}
	for _, lease := range leases {
		lease.Stop()
	}
	if body := request(); bytes.Contains(body, []byte(`"portal.pv"`)) || bytes.Contains(body, []byte(`"portal.storage"`)) || bytes.Contains(body, []byte(`"portal.evse"`)) {
		t.Fatalf("source lifecycle did not withdraw its exact owned generations: %s", body)
	}
}

func TestPortalCatalogHandlerAdmitsAndWithdrawsDriverGeneration(t *testing.T) {
	contributions := newPortalFixtureContributions(t)
	leases := []*gatewayPortalContributionLifecycle{
		startPortalTestLease(t, contributions, portalFixtureManifest(gatewayPortalPVDescriptor())),
		startPortalTestLease(t, contributions, portalFixtureManifest(gatewayPortalStorageDescriptor())),
		startPortalTestLease(t, contributions, portalFixtureManifest(gatewayPortalEVSEDescriptor())),
	}
	defer func() {
		for _, lease := range leases {
			lease.Stop()
		}
	}()
	source := newGatewayPortalCatalogSource(nil, nil, nil, contributions)
	h := newPortalCatalogV1Handler(source)
	request := func() []byte {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/graphql/portal/v1", bytes.NewBufferString(`{"operationName":"PortalCatalogV1","variables":{}}`))
		res := httptest.NewRecorder()
		h.ServeHTTP(res, req)
		if res.Code != http.StatusOK {
			t.Fatalf("handler status=%d body=%s", res.Code, res.Body.String())
		}
		return res.Body.Bytes()
	}

	extra := portalFixtureManifest(gatewayPortalPVDescriptor())
	extra.ManifestID = "portal.pv.secondary"
	extra.Contributor.DriverID = "pv.secondary"
	extra.Groups[0].Label.Key = "portal.pv.secondary.group"
	extra.Views[0].ID = "portal.pv.secondary.summary"
	extra.Views[0].Label.Key = "portal.pv.secondary.summary"
	owner := contributionv1.DriverGeneration{DriverID: extra.Contributor.DriverID, Generation: 1}
	if err := contributions.PublishGeneration(owner, []contributionv1.Manifest{extra}); err != nil {
		t.Fatalf("publish a valid new driver contribution: %v", err)
	}
	if body := request(); !bytes.Contains(body, []byte(`"portal.pv.secondary"`)) || !bytes.Contains(body, []byte(`"portal.storage"`)) {
		t.Fatalf("published contribution or unrelated row missing: %s", body)
	}
	if !contributions.WithdrawGeneration(owner) {
		t.Fatal("withdraw current contribution generation = false")
	}
	if body := request(); bytes.Contains(body, []byte(`"portal.pv.secondary"`)) || !bytes.Contains(body, []byte(`"portal.storage"`)) {
		t.Fatalf("withdrawal did not atomically remove only the owned row: %s", body)
	}
}

func TestPortalCatalogHandlerSnapshotsCompleteGenerationDuringReplaceAndWithdraw(t *testing.T) {
	contributions := newPortalFixtureContributions(t)
	leases := []*gatewayPortalContributionLifecycle{
		startPortalTestLease(t, contributions, portalFixtureManifest(gatewayPortalPVDescriptor())),
		startPortalTestLease(t, contributions, portalFixtureManifest(gatewayPortalStorageDescriptor())),
		startPortalTestLease(t, contributions, portalFixtureManifest(gatewayPortalEVSEDescriptor())),
	}
	defer func() {
		for _, lease := range leases {
			lease.Stop()
		}
	}()
	source := newGatewayPortalCatalogSource(nil, nil, nil, contributions)
	h := newPortalCatalogV1Handler(source)
	first := portalFixtureManifest(gatewayPortalPVDescriptor())
	first.Contributor.DriverID, first.ManifestID = "fixture.driver", "portal.fixture.pv"
	first.Groups[0].Label.Key, first.Views[0].ID, first.Views[0].Label.Key = "portal.fixture.pv.group", "portal.fixture.pv.summary", "portal.fixture.pv.summary"
	second := portalFixtureManifest(gatewayPortalStorageDescriptor())
	second.Contributor.DriverID, second.ManifestID = "fixture.driver", "portal.fixture.storage"
	second.Groups[0].Label.Key, second.Views[0].ID, second.Views[0].Label.Key = "portal.fixture.storage.group", "portal.fixture.storage.summary", "portal.fixture.storage.summary"
	request := func() []byte {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/graphql/portal/v1", bytes.NewBufferString(`{"operationName":"PortalCatalogV1","variables":{}}`))
		res := httptest.NewRecorder()
		h.ServeHTTP(res, req)
		if res.Code != http.StatusOK {
			t.Fatalf("handler status=%d body=%s", res.Code, res.Body.String())
		}
		return res.Body.Bytes()
	}

	start := make(chan struct{})
	published := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	var writerErr error
	var writerMu sync.Mutex
	go func() {
		defer close(done)
		<-start
		for generation := uint64(1); generation <= 64; generation++ {
			owner := contributionv1.DriverGeneration{DriverID: first.Contributor.DriverID, Generation: generation}
			if err := contributions.PublishGeneration(owner, []contributionv1.Manifest{first, second}); err != nil {
				writerMu.Lock()
				writerErr = err
				writerMu.Unlock()
				return
			}
			if generation == 1 {
				close(published)
				<-release
			}
			contributions.WithdrawGeneration(owner)
		}
	}()
	close(start)
	<-published
	for i := 0; i < 16; i++ {
		body := request()
		if !bytes.Contains(body, []byte(`"portal.fixture.pv"`)) || !bytes.Contains(body, []byte(`"portal.fixture.storage"`)) {
			t.Fatalf("complete published generation was partially visible: %s", body)
		}
	}
	close(release)
	for i := 0; i < 128; i++ {
		body := request()
		if bytes.Contains(body, []byte(`"portal catalog changed during detached capture"`)) {
			continue
		}
		gotFirst := bytes.Contains(body, []byte(`"portal.fixture.pv"`))
		gotSecond := bytes.Contains(body, []byte(`"portal.fixture.storage"`))
		if gotFirst != gotSecond {
			t.Fatalf("mixed complete-generation snapshot: %s", body)
		}
		if !bytes.Contains(body, []byte(`"portal.storage"`)) {
			t.Fatalf("unrelated source-owner contribution disappeared: %s", body)
		}
	}
	<-done
	writerMu.Lock()
	err := writerErr
	writerMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if body := request(); bytes.Contains(body, []byte(`"portal.fixture.pv"`)) || bytes.Contains(body, []byte(`"portal.fixture.storage"`)) {
		t.Fatalf("withdrawn complete generation remains visible: %s", body)
	}
}

func TestPortalCatalogHandlerRejectsRegistryChangeDuringCapture(t *testing.T) {
	contributions := newPortalFixtureContributions(t)
	manifest := portalFixtureManifest(gatewayPortalPVDescriptor())
	owner := contributionv1.DriverGeneration{DriverID: manifest.Contributor.DriverID, Generation: 1}
	if err := contributions.PublishGeneration(owner, []contributionv1.Manifest{manifest}); err != nil {
		t.Fatal(err)
	}
	source := &portalCatalogBarrierSource{contributions: contributions, entered: make(chan struct{}), release: make(chan struct{})}
	h := newPortalCatalogV1Handler(source)
	serve := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/graphql/portal/v1", bytes.NewBufferString(`{"operationName":"PortalCatalogV1","variables":{}}`))
		res := httptest.NewRecorder()
		h.ServeHTTP(res, req)
		return res
	}
	result := make(chan *httptest.ResponseRecorder, 1)
	go func() { result <- serve() }()
	<-source.entered
	if err := contributions.PublishGeneration(contributionv1.DriverGeneration{DriverID: owner.DriverID, Generation: 2}, []contributionv1.Manifest{manifest}); err != nil {
		t.Fatal(err)
	}
	close(source.release)
	res := <-result
	if res.Code != http.StatusOK || !bytes.Contains(res.Body.Bytes(), []byte(`"portal catalog changed during detached capture"`)) {
		t.Fatalf("capture race response=%d body=%s", res.Code, res.Body.String())
	}
	next := serve()
	if next.Code != http.StatusOK || !bytes.Contains(next.Body.Bytes(), []byte(`"portal.pv"`)) || bytes.Contains(next.Body.Bytes(), []byte(`"portal catalog changed during detached capture"`)) {
		t.Fatalf("successor request did not capture the new complete generation: %d %s", next.Code, next.Body.String())
	}
}

// Characterization goldens for issue #1024. They were captured from the
// pre-hub publication paths of the three PublicationKernel owners and must stay
// byte-identical when those kernels move behind the semantic publication hub.
// Regenerate them only on a deliberate, reviewed change with
// HELIANTHUS_UPDATE_CHARACTERIZATION=1.

// characterizationAnchor is the one wall origin shared by every fixture so a
// single consumer instant evaluates all three families coherently.
var characterizationAnchor = time.Unix(1_800_000_000, 0).UTC()

func characterizationGolden(t *testing.T, dir, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", "characterization", dir, name)
	if os.Getenv("HELIANTHUS_UPDATE_CHARACTERIZATION") == "1" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read characterization golden %s: %v", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("characterization golden %s changed:\n got: %s\nwant: %s", path, got, want)
	}
}

func characterizationJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

// characterizationCanonical returns the canonical SemReg bytes of the snapshot
// carried by one public {snapshot, evaluation, selections, projection} value.
func characterizationCanonical(t *testing.T, public []byte) ([]byte, semreg.Snapshot) {
	t.Helper()
	var value struct {
		Snapshot semreg.Snapshot `json:"snapshot"`
	}
	if err := json.Unmarshal(public, &value); err != nil {
		t.Fatal(err)
	}
	canonical, err := semreg.CanonicalJSON(value.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	return canonical, value.Snapshot
}

// characterizationIdentity summarizes every identifier family the hub
// minters must reproduce: asset, source, epoch, generation, binding, service,
// capability, candidate and cursor key.
func characterizationIdentity(t *testing.T, snapshot semreg.Snapshot) []byte {
	t.Helper()
	summary := map[string][]string{"asset": {string(snapshot.AssetID)}}
	add := func(kind string, fields ...string) {
		summary[kind] = append(summary[kind], strings.Join(fields, " "))
	}
	for _, source := range snapshot.Sources {
		add("sources", string(source.SourceID), string(source.SourceEpochID), string(source.State))
	}
	for _, binding := range snapshot.Bindings {
		add("bindings", string(binding.BindingID), string(binding.SourceID), string(binding.SourceEpochID), string(binding.DriverGeneration), string(binding.State))
	}
	for _, link := range snapshot.IdentityLinks {
		add("identity_links", string(link.BindingID), string(link.State))
	}
	for _, service := range snapshot.Services {
		add("services", string(service.InstanceID), string(service.Definition.ID), string(service.BindingID))
	}
	for _, capability := range snapshot.Capabilities {
		add("capabilities", string(capability.InstanceID), string(capability.Definition.ID), string(capability.ServiceInstance), string(capability.BindingID))
	}
	for _, envelope := range snapshot.Facts {
		for _, candidate := range envelope.Candidates {
			add("candidates", string(candidate.CandidateID), string(envelope.Key.FactID), string(candidate.Origin.OriginID))
		}
	}
	for _, cursor := range snapshot.Cursors {
		add("cursors", string(cursor.SourceID), string(cursor.SourceEpochID), string(cursor.DriverGeneration), string(cursor.LastSequence))
	}
	for _, values := range summary {
		sort.Strings(values)
	}
	encoded, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(encoded, '\n')
}

// characterizationMCPCall returns the exact JSON-RPC response body.
func characterizationMCPCall(t *testing.T, handler http.Handler, tool string, arguments string) []byte {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + tool + `","arguments":` + arguments + `}}`
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(body)))
	if response.Code != http.StatusOK || bytes.Contains(response.Body.Bytes(), []byte(`"isError":true`)) {
		t.Fatalf("MCP %s status=%d body=%s", tool, response.Code, response.Body.String())
	}
	return response.Body.Bytes()
}

// characterizationClock is one injected wall and read-clock pair.
type characterizationClock struct {
	mu   sync.Mutex
	wall time.Time
	read uint64
}

func (clock *characterizationClock) set(wall time.Time, read time.Duration) {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	clock.wall, clock.read = wall, uint64(read)
}

func (clock *characterizationClock) now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.wall
}

func (clock *characterizationClock) readClock() (uint64, error) {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.read, nil
}

func injectTeslaCharacterizationClock(t *testing.T, owner *teslaHSCRetainedOwner, clock *characterizationClock) {
	t.Helper()
	owner.publication.SetClock(clock.now, clock.readClock)
}

// startTeslaCharacterizationOwner ingests one persistent and one provisional
// outcome on the first incarnation with an injected publication clock.
func startTeslaCharacterizationOwner(t *testing.T) (*teslaHSCRetainedOwner, *characterizationClock) {
	t.Helper()
	return startTeslaCharacterizationOwnerOn(t, newTestSemanticHub(t))
}

func startTeslaCharacterizationOwnerOn(t *testing.T, hub *semhub.Hub) (*teslaHSCRetainedOwner, *characterizationClock) {
	t.Helper()
	owner, err := startTeslaHSCRetainedOwner(teslaRetainedConfig(), hub)
	if err != nil {
		t.Fatal(err)
	}
	clock := &characterizationClock{}
	injectTeslaCharacterizationClock(t, owner, clock)
	clock.set(characterizationAnchor.Add(1500*time.Millisecond), 0)
	persistent := teslaPersistentOutcome(t, 1, 16)
	persistent.Exchange.ReceiptWall = characterizationAnchor.Add(time.Second)
	if err := owner.IngestPersistent(context.Background(), persistent); err != nil {
		t.Fatal(err)
	}
	clock.set(characterizationAnchor.Add(4*time.Second), 2500*time.Millisecond)
	provisional := teslaProvisionalOutcome(t, 2, 12, 600, false, 32)
	provisional.Set.ReceiptWall = characterizationAnchor.Add(2 * time.Second)
	provisional.Readback.ReceiptWall = characterizationAnchor.Add(3 * time.Second)
	if err := owner.IngestProvisional(context.Background(), provisional); err != nil {
		t.Fatal(err)
	}
	return owner, clock
}

// characterizationPVEndpoint is an inert endpoint: SunSpec observations are
// recorded directly, so no socket identity enters the PV source hash. Its
// open, like the factory and the dial, advances the injected clock.
type characterizationPVEndpoint struct{ advance func() }

func (endpoint characterizationPVEndpoint) OpenConnection(net.Conn) (modbus.TCPConnectionHandle, error) {
	endpoint.advance()
	return modbus.TCPConnectionHandle{}, nil
}
func (characterizationPVEndpoint) EnqueueRead(modbus.TCPReadPlan) (modbus.TCPRequestHandle, error) {
	return modbus.TCPRequestHandle{}, errors.New("characterization endpoint performs no reads")
}
func (characterizationPVEndpoint) Dispatch() (modbus.TCPDispatch, bool) {
	return modbus.TCPDispatch{}, false
}
func (characterizationPVEndpoint) Write(context.Context, modbus.TCPDispatch) (modbus.OwnerTransition, error) {
	return modbus.OwnerTransition{}, errors.New("characterization endpoint performs no writes")
}
func (characterizationPVEndpoint) Read(context.Context, modbus.TCPConnectionHandle) (modbus.TCPReadBatch, error) {
	return modbus.TCPReadBatch{}, errors.New("characterization endpoint performs no reads")
}
func (characterizationPVEndpoint) Cancel(modbus.TCPRequestHandle) error { return nil }
func (characterizationPVEndpoint) Snapshot() modbus.TCPEndpointSnapshot {
	return modbus.TCPEndpointSnapshot{}
}
func (characterizationPVEndpoint) Close() error { return nil }

// characterizationSunSpecObservation replays the accepted SunSpec chain words
// into one immutable qualification observation.
func characterizationSunSpecObservation(t *testing.T, words []uint16, pollGeneration, deadlineIdentity uint64) fronius.SunSpecQualificationObservation {
	t.Helper()
	registry, err := sunspec.NewStandardSunSpecDecoderRegistry(sunspec.ModelsRevisionV1)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := sunspec.NewChainPlan(sunspec.ChainPlanSpec{
		SchemaRevision: sunspec.ModelsRevisionV1, BaseCandidates: []uint16{40000},
		Limits: sunspec.ChainLimits{MaxTotalWords: 1024, MaxOccurrences: 32}, DecoderKeys: registry.DecoderKeys(),
	})
	if err != nil {
		t.Fatal(err)
	}
	chain, offset := sunspec.NewChain(plan), 0
	var completed sunspec.ChainSnapshot
	for id := uint64(1); ; id++ {
		requests := chain.NextRequests()
		if len(requests) != 1 {
			t.Fatalf("fixture replay requests=%d", len(requests))
		}
		request := requests[0]
		count := int(request.WordCount())
		if offset+count > len(words) {
			t.Fatal("fixture replay exceeded immutable words")
		}
		view, err := modbusreg.NewLogicalViewSnapshot(modbusreg.LogicalViewRecord{
			LogicalViewID: id, WireResponseID: id + 100, PhysicalRequestID: id + 200,
			Endpoint: "fixture", ConnectionID: 4, Transport: modbusreg.TransportTCP, TransportGeneration: 5,
			UnitID: 1, RequestedFunction: modbusreg.FunctionReadHoldingRegisters, ReceivedFunction: modbusreg.FunctionReadHoldingRegisters,
			Table: modbusreg.HoldingRegisters, PhysicalOffset: request.Address(), PhysicalWordCount: request.WordCount(),
			AuthorizationScope: "fixture:characterization", PollGeneration: pollGeneration, DeadlineIdentity: deadlineIdentity,
			LogicalOffset: request.Address(), LogicalWordCount: request.WordCount(), SliceOffset: 0, SliceWordCount: request.WordCount(),
			Words: append([]uint16(nil), words[offset:offset+count]...), WireResponseBytes: []byte{byte(id)},
		})
		if err != nil {
			t.Fatal(err)
		}
		offset += count
		completed, err = chain.AdmitReplay(request, view)
		if err != nil {
			t.Fatal(err)
		}
		if len(completed.RawWords()) != 0 {
			break
		}
	}
	observation, err := fronius.NewSunSpecQualificationObservation(registry, completed)
	if err != nil {
		t.Fatal(err)
	}
	return observation
}

type characterizationFamilies struct {
	pv        *modbusadapter.Adapter
	pvProfile string
	pvSample  string
	pvAsset   string
	growatt   *growattBMSRS485ProductionProvider
	tesla     *teslaHSCRetainedOwner
	clock     *characterizationClock
}

// startCharacterizationFamilies composes the production PV, Growatt and Tesla
// paths with deterministic native fixtures and injected clocks.
func startCharacterizationFamilies(t *testing.T) characterizationFamilies {
	t.Helper()
	return startCharacterizationFamiliesOn(t, nil)
}

// startCharacterizationFamiliesOn composes the three families on one
// publication hub, as the gateway does; a nil hub gives each family its own.
func startCharacterizationFamiliesOn(t *testing.T, hub *semhub.Hub) characterizationFamilies {
	t.Helper()
	familyHub := func() *semhub.Hub {
		if hub != nil {
			return hub
		}
		return newTestSemanticHub(t)
	}
	config, err := mapModbusRuntimeConfig(ebusgateway.ModbusTCPConfig{Enabled: true, Endpoint: "tcp://127.0.0.1:1502", DialTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	var pvMu sync.Mutex
	pvNow := characterizationAnchor.Add(-30 * time.Second)
	advance := func() {
		pvMu.Lock()
		pvNow = pvNow.Add(time.Second)
		pvMu.Unlock()
	}
	setPV := func(at time.Time) {
		pvMu.Lock()
		pvNow = at
		pvMu.Unlock()
	}
	config.Clock = func() time.Time {
		pvMu.Lock()
		defer pvMu.Unlock()
		return pvNow
	}
	client, peer := net.Pipe()
	t.Cleanup(func() { _ = peer.Close() })
	pvClass, err := modbusadapter.NewSemanticPVClass(familyHub(), false)
	if err != nil {
		t.Fatal(err)
	}
	config.SemanticPV = pvClass
	adapter, err := modbusadapter.Start(context.Background(), config,
		func(context.Context, string, string) (net.Conn, error) { advance(); return client, nil },
		func(modbus.TCPEndpointConfig) (modbusadapter.Endpoint, error) {
			advance()
			return characterizationPVEndpoint{advance: advance}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = adapter.Close() })
	words := sunspectest.ObservedFroniusFloatControlsWords()
	sunspectest.SetFloat(words, 20, 1_234.5)
	sunspectest.SetFloat(words, 22, 50)
	setPV(characterizationAnchor.Add(-20 * time.Second))
	qualified := characterizationSunSpecObservation(t, words, 701, 801)
	if err := adapter.RecordSunSpecQualificationObservation(qualified); err != nil {
		t.Fatal(err)
	}
	setPV(characterizationAnchor.Add(5 * time.Second))
	refreshed := append([]uint16(nil), words...)
	sunspectest.SetFloat(refreshed, 20, 4_321.5)
	if err := adapter.RecordSunSpecCurrentObservation(characterizationSunSpecObservation(t, refreshed, 702, 802)); err != nil {
		t.Fatal(err)
	}
	setPV(characterizationAnchor.Add(10 * time.Second))
	current, ok := adapter.SemanticPVCurrentSingle()
	if !ok {
		t.Fatal("PV publication unavailable")
	}
	growatt := startGrowattRuntimeWithFakeOn(t, growattProductionConfig(), &growattEndpointFake{words: growattBMSProductionWords(), failAt: -1, mismatch: -1, generation: 4}, familyHub())
	if _, err := growatt.GrowattStorageSemanticCurrent(context.Background()); err != nil {
		t.Fatal(err)
	}
	tesla, clock := startTeslaCharacterizationOwnerOn(t, familyHub())
	return characterizationFamilies{pv: adapter, pvProfile: qualified.Capability().ProfileID(), pvSample: qualified.SampleID(), pvAsset: string(current.Snapshot.AssetID), growatt: growatt, tesla: tesla, clock: clock}
}

func TestCharacterizationConsumersOfAllThreeFamilies(t *testing.T) {
	families := startCharacterizationFamilies(t)
	at := characterizationAnchor.Add(10 * time.Second)
	families.clock.set(at, 9*time.Second)
	const dir = "consumers"

	server, err := mcp.NewServer(emptyMCPRegistry{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	mcp.RegisterModbusV1Tools(server, newGatewayModbusMCPProviderWithRuntimes(families.pv, families.growatt, families.tesla))
	characterizationGolden(t, dir, "mcp_pv.json", characterizationMCPCall(t, server.Handler(), mcp.SemanticV1PVCurrentGetTool, `{"profile_id":"`+families.pvProfile+`","sample_id":"`+families.pvSample+`"}`))
	characterizationGolden(t, dir, "mcp_storage.json", characterizationMCPCall(t, server.Handler(), mcp.SemanticV1GrowattStorageCurrentGetTool, `{}`))
	characterizationGolden(t, dir, "mcp_evse.json", characterizationMCPCall(t, server.Handler(), mcp.SemanticV1EVSECurrentGetTool, `{}`))

	contributions, err := newGatewayPortalCatalogContributions()
	if err != nil {
		t.Fatal(err)
	}
	pvLease, err := startGatewayPortalPVContribution(families.pv, contributions)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pvLease.Stop)
	if err := attachGatewayPortalStorageContribution(families.growatt, contributions); err != nil {
		t.Fatal(err)
	}
	if err := attachGatewayPortalEVSEContribution(families.tesla, contributions); err != nil {
		t.Fatal(err)
	}
	source := newGatewayPortalCatalogSource(families.pv, gatewayPortalStoragePublication(families.growatt), families.tesla, contributions)
	captured, err := source.Capture(at)
	if err != nil {
		t.Fatal(err)
	}
	characterizationGolden(t, dir, "portal_source_capture.json", characterizationJSON(t, map[string]any{"resources": captured.Resources, "fields": captured.Fields}))
	composer, err := contributions.composer(source)
	if err != nil {
		t.Fatal(err)
	}
	composer.SetClock(func() time.Time { return at })
	catalog, err := composer.Catalog()
	if err != nil {
		t.Fatal(err)
	}
	characterizationGolden(t, dir, "portal_catalog.json", characterizationJSON(t, catalog))

	rendered := ebusgateway.RenderSemanticPrometheus(semanticPrometheusDomains(families.pv, families.growatt, families.tesla, true, true, true, string(families.growatt.storage.assetID), at), at)
	var metrics []string
	for _, line := range strings.Split(rendered, "\n") {
		if strings.Contains(line, "helianthus_semantic_") {
			metrics = append(metrics, line)
		}
	}
	characterizationGolden(t, dir, "prometheus_semantic.txt", []byte(strings.Join(metrics, "\n")+"\n"))
}

// portalTestDrivers resolves a source to its driver the way the publication
// hub does.
type portalTestDrivers map[semreg.SourceID]string

func (d portalTestDrivers) Source(id semreg.SourceID) (semhub.SourceInfo, bool) {
	driver, ok := d[id]
	return semhub.SourceInfo{Source: id, Driver: driver}, ok
}

// portalContributorGeneration varies the driver generation per source so a
// test can tell which binding a resource or an action names.
var portalContributorGeneration = map[string]semreg.Uint64{"owner": "3", "contributor": "5", "contributor-b": "9"}

// portalContributorSnapshot builds one asset whose sources each hold a
// current binding; carriers publish the service and capability instance.
func portalContributorSnapshot(service, capability semreg.DefinitionRef, sources []string, carriers ...string) semreg.Snapshot {
	snapshot := semreg.Snapshot{AssetID: "asset:shared", SnapshotID: "snapshot:shared"}
	for _, tag := range sources {
		snapshot.Sources = append(snapshot.Sources, semreg.SourceDescriptor{SourceID: semreg.SourceID("source:" + tag), SourceEpochID: semreg.SourceEpochID("epoch:" + tag), State: semreg.SourceCurrent})
		snapshot.Bindings = append(snapshot.Bindings, semreg.NativeBinding{BindingID: semreg.NativeBindingID("binding:" + tag), AssetID: "asset:shared", SourceID: semreg.SourceID("source:" + tag), SourceEpochID: semreg.SourceEpochID("epoch:" + tag), DriverGeneration: portalContributorGeneration[tag], State: semreg.BindingCurrent})
	}
	for _, tag := range carriers {
		snapshot.Services = append(snapshot.Services, semreg.ServiceInstance{InstanceID: semreg.ServiceInstanceID("service:" + tag), AssetID: "asset:shared", Definition: service, BindingID: semreg.NativeBindingID("binding:" + tag), SourceEpochID: semreg.SourceEpochID("epoch:" + tag), DriverGeneration: portalContributorGeneration[tag]})
		snapshot.Capabilities = append(snapshot.Capabilities, semreg.CapabilityInstance{InstanceID: semreg.CapabilityInstanceID("capability:" + tag), AssetID: "asset:shared", Definition: capability, ServiceInstance: semreg.ServiceInstanceID("service:" + tag), BindingID: semreg.NativeBindingID("binding:" + tag), SourceEpochID: semreg.SourceEpochID("epoch:" + tag), DriverGeneration: portalContributorGeneration[tag]})
	}
	return snapshot
}

// portalAllocationDescriptor declares the EVSE allocation action on the
// connector resource for one driver.
func portalAllocationDescriptor(driver, manifest, version string) contributionv1.Manifest {
	pack := catalogv1.FivePacks[3]
	ref := func(id string) contributionv1.DefinitionRef {
		return contributionv1.DefinitionRef{Pack: pack, ID: id, Version: pack.Version}
	}
	descriptor := newGatewayPortalDescriptor(driver, manifest, "connector", "evse.limit.allocated_current", "evse.service.connector", "evse.capability.set_allocated_current", "unit.ampere", pack)
	descriptor.ManifestVersion = version
	operation, argument, effect := ref("evse.operation.set_allocated_current"), ref("evse.limit.allocated_current"), ref("evse.effect.set_allocated_current")
	descriptor.Actions = []contributionv1.Action{{ID: "allocate", Group: "connector", Label: contributionv1.Label{Key: manifest + ".allocate", Default: "Allocate"}, OperationRef: &operation, CapabilityRef: ref("evse.capability.set_allocated_current"), ServiceRef: ref("evse.service.connector"), ArgumentRef: &argument, EffectRef: &effect}}
	return descriptor
}

// portalStaticCapture replays fixed resources to the catalog composer.
type portalStaticCapture struct{ captured catalogv1.Capture }

func (s portalStaticCapture) Capture(time.Time) (catalogv1.Capture, error) {
	return s.captured, nil
}

type portalTestInvoker struct{}

func (portalTestInvoker) Invoke(catalogv1.Action, catalogv1.Claims) (json.RawMessage, error) {
	return json.RawMessage(`{}`), nil
}

type portalTestRevalidator struct{}

func (portalTestRevalidator) Revalidate(catalogv1.Action, catalogv1.Claims) error { return nil }

func TestPortalCatalogKeepsResourceWithSeveralContributors(t *testing.T) {
	contributions := newPortalFixtureContributions(t)
	// The owner and the contributor publish the same action under different
	// drivers, manifest identifiers and manifest versions.
	owner := portalAllocationDescriptor("evse.primary", "portal.evse.allocation", "1.0.0")
	contributor := portalAllocationDescriptor("evse.contributor", "portal.evse.contributed", "1.1.0")
	for _, descriptor := range []contributionv1.Manifest{owner, contributor} {
		lease := startPortalTestLease(t, contributions, portalFixtureManifest(descriptor))
		defer lease.Stop()
	}
	// The hub names the owner's source by the runtime driver of the EVSE
	// publication, not by the descriptor's contribution driver; the owner is
	// the publication's declared source.
	attribution := portalAttribution{drivers: portalTestDrivers{"source:owner": teslaEVSEDriver, "source:contributor": "evse.contributor", "source:contributor-b": "evse.contributor-b"}, accepted: contributions.snapshot().Accepted}.ownedBy("source:owner")
	service := semanticRef(owner.Fields[0].ServiceRef)
	capability := semanticRef(owner.Fields[0].CapabilityRef)
	twoSources := []string{"owner", "contributor"}

	catalog := func(t *testing.T, resources []catalogv1.Resource) catalogv1.Catalog {
		t.Helper()
		// The legacy row is presentation data. The synthetic served cut
		// names the shared graph resource independently of that row’s group.
		resources[0].ID = "resource:shared"
		captured := portalStaticCapture{captured: catalogv1.Capture{
			Domains: owner.Requires.Packs,
			LensMembers: map[string][]catalogv1.LensMember{portalFixtureLensID: {
				{ResourceID: "resource:shared", AssetID: "asset:shared"},
			}},
			Resources: resources,
		}}
		composer := catalogv1.New(contributions.index, captured, portalTestInvoker{})
		registry := contributions.snapshot()
		if err := composer.InstallDetachedRegistrySnapshot(registry.Accepted, registry.Quarantined); err != nil {
			t.Fatal(err)
		}
		composer.SetRevalidator(portalTestRevalidator{})
		out, err := composer.Catalog()
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	kept := func(t *testing.T, snapshot semreg.Snapshot, attribution portalAttribution, tag, driver string, manifest contributionv1.Manifest) {
		t.Helper()
		resources, _, diagnostics := portalSnapshotCapture(attribution, owner, snapshot, semreg.EvaluationView{}, []semreg.Selection{}, map[string]any{})
		if len(diagnostics) != 0 || len(resources) != 1 {
			t.Fatalf("resources=%d diagnostics=%+v", len(resources), diagnostics)
		}
		got := resources[0]
		generation := map[string]uint64{"owner": 3, "contributor": 5}[tag]
		if got.ID != "connector" || got.Source.AssetID != "asset:shared" || got.Source.BindingID != "binding:"+tag || got.Source.SourceEpoch != "epoch:"+tag || got.Source.DriverGeneration != generation {
			t.Fatalf("resource source=%+v; want the %s binding", got.Source, tag)
		}
		if got.ContributionDriverID != driver || got.ContributionManifestID != manifest.ManifestID || got.ContributionManifestVersion != manifest.ManifestVersion {
			t.Fatalf("resource attributed to %s %s %s; want %s %s %s", got.ContributionDriverID, got.ContributionManifestID, got.ContributionManifestVersion, driver, manifest.ManifestID, manifest.ManifestVersion)
		}
		out := catalog(t, resources)
		if len(out.Resources) != 1 || len(out.Actions) != 1 {
			t.Fatalf("catalog resources=%+v actions=%+v", out.Resources, out.Actions)
		}
		action := out.Actions[0]
		if action.ResourceID != "resource:shared" || action.ContributionDriverID != driver || action.ContributionManifestID != manifest.ManifestID || action.ContributionManifestVersion != manifest.ManifestVersion || action.Source.BindingID != "binding:"+tag || action.Source.SourceEpoch != "epoch:"+tag || action.Source.DriverGeneration != generation || !action.Enabled {
			t.Fatalf("action=%+v; want it attributed to %s on the %s binding", action, driver, tag)
		}
	}
	dropped := func(t *testing.T, snapshot semreg.Snapshot, attribution portalAttribution, reason string) {
		t.Helper()
		resources, _, diagnostics := portalSnapshotCapture(attribution, owner, snapshot, semreg.EvaluationView{}, []semreg.Selection{}, map[string]any{})
		if len(resources) != 0 {
			t.Fatalf("resource kept on binding %s attributed to %s", resources[0].Source.BindingID, resources[0].ContributionDriverID)
		}
		want := []gatewayPortalCatalogDiagnostic{{Domain: "helianthus.pack.evse", AssetID: "asset:shared", ResourceID: "connector", DriverID: "evse.primary", ManifestID: "portal.evse.allocation", Reason: reason}}
		if fmt.Sprint(diagnostics) != fmt.Sprint(want) {
			t.Fatalf("diagnostics=%+v; want %+v", diagnostics, want)
		}
	}

	t.Run("owner and contributor carry the capability: the owner's instance", func(t *testing.T) {
		kept(t, portalContributorSnapshot(service, capability, twoSources, "owner", "contributor"), attribution, "owner", "evse.primary", owner)
	})
	t.Run("only the contributor's binding carries the capability", func(t *testing.T) {
		kept(t, portalContributorSnapshot(service, capability, twoSources, "contributor"), attribution, "contributor", "evse.contributor", contributor)
	})
	t.Run("a contributor without an admitted descriptor", func(t *testing.T) {
		withoutContributor := attribution
		withoutContributor.accepted = nil
		for _, accepted := range attribution.accepted {
			if accepted.Key.DriverID != "evse.contributor" {
				withoutContributor.accepted = append(withoutContributor.accepted, accepted)
			}
		}
		dropped(t, portalContributorSnapshot(service, capability, twoSources, "contributor"), withoutContributor, portalDropContributorUnadmitted)
	})
	t.Run("several sources and no driver resolution", func(t *testing.T) {
		dropped(t, portalContributorSnapshot(service, capability, twoSources, "owner", "contributor"), portalAttribution{}, portalDropDriverUnresolved)
	})
	t.Run("a source the hub does not know", func(t *testing.T) {
		unknown := attribution
		unknown.drivers = portalTestDrivers{"source:owner": teslaEVSEDriver}
		dropped(t, portalContributorSnapshot(service, capability, twoSources, "contributor"), unknown, portalDropDriverUnresolved)
	})
	t.Run("two contributors and no owner carry the capability", func(t *testing.T) {
		dropped(t, portalContributorSnapshot(service, capability, []string{"owner", "contributor", "contributor-b"}, "contributor", "contributor-b"), attribution, portalDropCapabilityAmbiguous)
	})
	t.Run("no source carries the capability", func(t *testing.T) {
		dropped(t, portalContributorSnapshot(service, capability, twoSources), attribution, portalDropCapabilityUnbound)
	})
	t.Run("a service instance whose binding is unknown", func(t *testing.T) {
		snapshot := portalContributorSnapshot(service, capability, twoSources, "owner")
		snapshot.Services = append(snapshot.Services, semreg.ServiceInstance{InstanceID: "service:orphan", AssetID: "asset:shared", Definition: service, BindingID: "binding:orphan", SourceEpochID: "epoch:orphan", DriverGeneration: "1"})
		dropped(t, snapshot, attribution, portalDropBindingUnresolved)
	})
	t.Run("a contributor with two service instances does not suppress the owner", func(t *testing.T) {
		snapshot := portalContributorSnapshot(service, capability, twoSources, "owner", "contributor")
		snapshot.Services = append(snapshot.Services, semreg.ServiceInstance{InstanceID: "service:contributor-2", AssetID: "asset:shared", Definition: service, BindingID: "binding:contributor", SourceEpochID: "epoch:contributor", DriverGeneration: portalContributorGeneration["contributor"]})
		kept(t, snapshot, attribution, "owner", "evse.primary", owner)
	})
	t.Run("a contributor with two capability instances does not suppress the owner", func(t *testing.T) {
		snapshot := portalContributorSnapshot(service, capability, twoSources, "owner", "contributor")
		snapshot.Capabilities = append(snapshot.Capabilities, semreg.CapabilityInstance{InstanceID: "capability:contributor-2", AssetID: "asset:shared", Definition: capability, ServiceInstance: "service:contributor", BindingID: "binding:contributor", SourceEpochID: "epoch:contributor", DriverGeneration: portalContributorGeneration["contributor"]})
		kept(t, snapshot, attribution, "owner", "evse.primary", owner)
	})
	t.Run("an ambiguous owner leaves the contributor's path", func(t *testing.T) {
		snapshot := portalContributorSnapshot(service, capability, twoSources, "owner", "contributor")
		snapshot.Services = append(snapshot.Services, semreg.ServiceInstance{InstanceID: "service:owner-2", AssetID: "asset:shared", Definition: service, BindingID: "binding:owner", SourceEpochID: "epoch:owner", DriverGeneration: portalContributorGeneration["owner"]})
		kept(t, snapshot, attribution, "contributor", "evse.contributor", contributor)
	})
	t.Run("only ambiguous sources drop the resource as ambiguous", func(t *testing.T) {
		snapshot := portalContributorSnapshot(service, capability, twoSources, "contributor")
		snapshot.Services = append(snapshot.Services, semreg.ServiceInstance{InstanceID: "service:contributor-2", AssetID: "asset:shared", Definition: service, BindingID: "binding:contributor", SourceEpochID: "epoch:contributor", DriverGeneration: portalContributorGeneration["contributor"]})
		dropped(t, snapshot, attribution, portalDropCapabilityAmbiguous)
	})
	t.Run("the composed catalog source has no driver resolver", func(t *testing.T) {
		hub, err := semhub.New(semhub.Options{})
		if err != nil {
			t.Fatal(err)
		}
		fresh, err := newGatewayPortalCatalogContributions()
		if err != nil {
			t.Fatal(err)
		}
		source := newGatewayPortalCatalogSource(nil, nil, nil, fresh).(*gatewayPortalCatalogSource)
		if source.attribution().drivers != nil {
			t.Fatal("a composed source must keep the single-source rule")
		}
		source.drivers = hub
		if source.attribution().drivers == nil {
			t.Fatal("the source does not use its driver resolver")
		}
	})
	// A contributor may be admitted under the owner's own runtime driver; the
	// owner is still only the publication's declared source.
	sameDriver := attribution
	sameDriver.drivers = portalTestDrivers{"source:owner": "evse.contributor", "source:contributor": "evse.contributor"}
	t.Run("a contributor under the owner's driver does not take the owner's attribution", func(t *testing.T) {
		kept(t, portalContributorSnapshot(service, capability, twoSources, "contributor"), sameDriver, "contributor", "evse.contributor", contributor)
	})
	t.Run("a contributor under the owner's driver does not make the owner ambiguous", func(t *testing.T) {
		kept(t, portalContributorSnapshot(service, capability, twoSources, "owner", "contributor"), sameDriver, "owner", "evse.primary", owner)
	})
	t.Run("several sources and no owner's source", func(t *testing.T) {
		withoutOwner := attribution.ownedBy("")
		dropped(t, portalContributorSnapshot(service, capability, twoSources, "owner", "contributor"), withoutOwner, portalDropDriverUnresolved)
	})
	t.Run("one source keeps the manifest's driver without resolution", func(t *testing.T) {
		kept(t, portalContributorSnapshot(service, capability, []string{"owner"}, "owner"), portalAttribution{}, "owner", "evse.primary", owner)
	})
}

// startSharedHubPortalSource composes the three families on one publication
// hub, as the gateway does, and returns the hub and their Portal catalog source.
func startSharedHubPortalSource(t *testing.T) (*semhub.Hub, characterizationFamilies, *gatewayPortalCatalogSource, time.Time) {
	t.Helper()
	hub := newTestSemanticHub(t)
	families := startCharacterizationFamiliesOn(t, hub)
	at := characterizationAnchor.Add(10 * time.Second)
	families.clock.set(at, 9*time.Second)
	contributions, err := newGatewayPortalCatalogContributions()
	if err != nil {
		t.Fatal(err)
	}
	pvLease, err := startGatewayPortalPVContribution(families.pv, contributions)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pvLease.Stop)
	if err := attachGatewayPortalStorageContribution(families.growatt, contributions); err != nil {
		t.Fatal(err)
	}
	if err := attachGatewayPortalEVSEContribution(families.tesla, contributions); err != nil {
		t.Fatal(err)
	}
	source := newGatewayPortalCatalogSource(families.pv, gatewayPortalStoragePublication(families.growatt), families.tesla, contributions).(*gatewayPortalCatalogSource)
	return hub, families, source, at
}

// The hub names each family's source by its runtime driver, which differs from
// the contribution driver of the descriptor the family publishes; installing
// the hub as the resolver keeps every single-source resource and its
// attribution.
func TestPortalCatalogKeepsSingleSourceFamiliesWithTheHubResolver(t *testing.T) {
	hub, _, source, at := startSharedHubPortalSource(t)
	before, beforeFields, beforeDiagnostics, err := source.CaptureDiagnostics(at)
	// The storage fixture publishes no capability instance, so only the PV and
	// EVSE resources are kept, as in the characterization capture.
	if err != nil || len(before) != 2 {
		t.Fatalf("without the resolver resources=%d diagnostics=%+v err=%v", len(before), beforeDiagnostics, err)
	}
	source.drivers = hub
	if source.attribution().drivers == nil {
		t.Fatal("the hub is not installed as the driver resolver")
	}
	after, afterFields, afterDiagnostics, err := source.CaptureDiagnostics(at)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := characterizationJSON(t, map[string]any{"resources": after, "fields": afterFields, "diagnostics": afterDiagnostics}), characterizationJSON(t, map[string]any{"resources": before, "fields": beforeFields, "diagnostics": beforeDiagnostics}); !bytes.Equal(got, want) {
		t.Fatalf("the hub resolver changed single-source output:\n%s\nwant\n%s", got, want)
	}
	runtime := map[string]string{"pv.primary": "pv_sunspec", "evse.primary": teslaEVSEDriver}
	for _, resource := range after {
		var snapshot semreg.Snapshot
		if err := json.Unmarshal(resource.Source.Snapshot, &snapshot); err != nil {
			t.Fatal(err)
		}
		var sourceID semreg.SourceID
		for _, binding := range snapshot.Bindings {
			if string(binding.BindingID) == resource.Source.BindingID {
				sourceID = binding.SourceID
			}
		}
		info, ok := hub.Source(sourceID)
		want, known := runtime[resource.ContributionDriverID]
		if !ok || !known || info.Driver != want || info.Driver == resource.ContributionDriverID {
			t.Fatalf("resource %s attributed to %s: hub driver %q known=%t; want runtime driver %q", resource.ID, resource.ContributionDriverID, info.Driver, ok, want)
		}
		delete(runtime, resource.ContributionDriverID)
	}
	if len(runtime) != 0 {
		t.Fatalf("resources missing for %v", runtime)
	}
}

func TestPortalCatalogLogsDroppedResourceOnceWhileItsReasonIsUnchanged(t *testing.T) {
	var buffer bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&buffer)
	defer log.SetOutput(previous)
	source := &gatewayPortalCatalogSource{}
	diagnostics := []gatewayPortalCatalogDiagnostic{{AssetID: "asset:shared", ResourceID: "connector", DriverID: "evse.primary", ManifestID: "portal.evse.allocation", Reason: portalDropCapabilityAmbiguous}}
	source.logDropped(diagnostics)
	source.logDropped(diagnostics)
	if got := buffer.String(); strings.Count(got, "not kept: "+portalDropCapabilityAmbiguous) != 1 || !strings.Contains(got, "asset:shared") {
		t.Fatalf("log=%q; want the reason logged once", got)
	}
	source.logDropped(nil)
	source.logDropped(diagnostics)
	if got := buffer.String(); strings.Count(got, "not kept: "+portalDropCapabilityAmbiguous) != 2 {
		t.Fatalf("log=%q; want the reason logged again after it cleared", got)
	}
}
