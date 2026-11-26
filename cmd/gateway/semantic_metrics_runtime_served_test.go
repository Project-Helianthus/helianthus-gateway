package main

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	ebusgateway "github.com/Project-Helianthus/helianthus-gateway"
	"github.com/Project-Helianthus/helianthus-gateway/internal/semanticmetrics"
	"github.com/Project-Helianthus/helianthus-gateway/internal/semhub"
	semreg "github.com/Project-Helianthus/helianthus-semreg/semreg/v1"
)

const servedZoneField = semreg.DefinitionID("thermal.measurement.air_temperature")

// publishServedZone makes one accepted publication with one current resource
// and one value. The declaration, resource and provenance have distinct IDs.
func publishServedZone(t *testing.T, hub *semhub.Hub, i int) semanticmetrics.Binding {
	t.Helper()
	const driver = "served.synthetic"
	id := fmt.Sprintf("served-zone-%02d", i)
	asset, resource := semreg.AssetID("asset:"+id), semreg.ResourceID("resource:"+id)
	source := semreg.SourceID("source:" + id)
	binding := semreg.NativeBindingID("binding:" + id)
	service := semreg.ServiceInstanceID("service:" + id)
	thermal := m38Domains()[0]
	if _, err := hub.Declare(semhub.AssetSpec{AssetID: asset, SourceID: source, Driver: driver, Validators: []semreg.PackValidator{thermal.validator}, Graph: true}); err != nil {
		t.Fatal(err)
	}
	handle, err := hub.AdmitSource(semhub.Admission{Source: source, Driver: driver})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := hub.Begin(handle, asset)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Abort()
	epoch, generation := handle.Epoch(), handle.GenerationID()
	wall, mono := m38Times(0)
	evidence := m38Evidence()
	text := string(resource)
	dimensions := []semreg.Dimension{{ID: "thermal.dimension.zone", Value: semreg.Value{Kind: semreg.ValueText, Text: &text}}}
	value := semreg.Value{Kind: semreg.ValueQuantity, Quantity: &semreg.Quantity{Number: pairingWatts(int64(280 + i)), Unit: "unit.kelvin"}}
	batch := semreg.PublicationBatch{
		Contract: semreg.ContractKernelV1, BatchID: semreg.BatchID("batch:" + id), AssetID: asset,
		SourceID: source, SourceEpochID: epoch, DriverGeneration: generation, Sequence: tx.NextSequence(), ExpectedSemanticRevision: "0", ObservedAt: wall,
		SourceUpserts:       []semreg.SourceDescriptor{{SourceID: source, SourceEpochID: epoch, ProtocolID: driver, ProfileID: "served.profile", ProfileVersion: "1", RegistryEvidence: evidence, StartedAt: wall, State: semreg.SourceCurrent, Revision: "1"}},
		SourceRetirements:   tx.SourceRetirements(),
		BindingUpserts:      []semreg.NativeBinding{{BindingID: binding, AssetID: asset, SourceID: source, SourceEpochID: epoch, DriverGeneration: generation, NativeResource: evidence, State: semreg.BindingCurrent, Revision: "1"}},
		IdentityLinkUpserts: []semreg.IdentityLink{{AssetID: asset, BindingID: binding, State: semreg.LinkQualified, Basis: []semreg.EvidenceRef{evidence}, Resource: &resource, Revision: "1"}},
		ResourceUpserts: []semreg.Resource{{ResourceID: resource, Kind: "resource.zone", State: semreg.ResourceCurrent, Revision: "1",
			Anchor: semreg.ResourceAnchor{Class: semreg.AnchorFunction, Basis: semreg.AnchorOperatorSlot, Slot: &id, Evidence: []semreg.EvidenceRef{evidence}},
			Origin: semreg.OriginRef{OriginID: semreg.OriginID("origin:resource-" + id), Kind: semreg.OriginOperator, Evidence: []semreg.EvidenceRef{evidence}}}},
		ServiceUpserts:    []semreg.ServiceInstance{{InstanceID: service, AssetID: asset, Definition: thermal.ref("thermal.service.zone"), BindingID: binding, SourceEpochID: epoch, DriverGeneration: generation, Qualification: semreg.QualificationQualified, Availability: semreg.AvailabilityAvailable, Resource: &resource, Revision: "1"}},
		CapabilityUpserts: []semreg.CapabilityInstance{{InstanceID: semreg.CapabilityInstanceID("capability:" + id), AssetID: asset, ServiceInstance: service, Definition: thermal.ref("thermal.capability.read.zone"), BindingID: binding, SourceEpochID: epoch, DriverGeneration: generation, Qualification: semreg.QualificationQualified, Availability: semreg.AvailabilityAvailable, Constraints: []semreg.TypedField{}, ActivationEvidence: []semreg.EvidenceRef{evidence}, Revision: "1"}},
		FactUpserts: []semreg.FactCandidate{{CandidateID: semreg.CandidateID("candidate:" + id),
			Key:             semreg.FactKey{PackID: thermal.metadata.Pack.ID, PackVersion: thermal.metadata.Pack.Version, FactID: servedZoneField, Dimensions: dimensions},
			Value:           &value,
			Quality:         semreg.Quality{Assertion: semreg.AssertionObserved, Qualification: semreg.QualificationQualified, Promotion: semreg.PromotionPromoted, Validity: semreg.ValidityGood, Availability: semreg.AvailabilityAvailable, Freshness: semreg.FreshnessFresh, Reasons: []semreg.DefinitionID{}},
			Times:           semreg.Times{ReceivedAt: wall, ReceiptMonotonic: mono, EvaluatedAt: wall, EvaluateMonotonic: mono},
			FreshnessPolicy: semreg.FreshnessPolicy{PolicyID: "policy:served-synthetic", Version: "1.0.0", FreshForNS: "60000000000", RetainForNS: "300000000000", MaxWallUncertaintyNS: "0"},
			BindingID:       &binding, SourceEpochID: &epoch, DriverGeneration: &generation,
			Origin:   semreg.OriginRef{OriginID: semreg.OriginID("origin:observation-" + id), Kind: semreg.OriginNativeObservation, SourceID: &source, SourceEpochID: &epoch, BindingID: &binding, Evidence: []semreg.EvidenceRef{evidence}},
			Evidence: []semreg.EvidenceRef{evidence}, Revision: "1"}},
		FactWithdrawals: []semreg.CandidateID{}, ServiceWithdrawals: []semreg.ServiceInstanceID{}, CapabilityWithdrawals: []semreg.CapabilityInstanceID{}, GenerationFences: []semreg.GenerationFence{},
	}
	batch.BatchDigest, err = batch.ComputedDigest()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := tx.Apply(batch, mono); err != nil {
		t.Fatalf("apply %s: %v", asset, err)
	}
	if outcome, err := tx.Commit(nil); err != nil || outcome != semhub.Accepted {
		t.Fatalf("commit %s: %s, %v", asset, outcome, err)
	}
	return semanticmetrics.Binding{Domain: "thermal", Slot: i, AssetID: asset, ResourceID: resource, SourceID: source, SourceEpochID: epoch, DriverGeneration: generation, BindingID: binding, ServiceInstance: service, Dimensions: dimensions, Fields: []semreg.DefinitionID{servedZoneField}}
}

func servedZones(t *testing.T) (*hubSemanticSnapshotProvider, []semanticmetrics.Binding, *int) {
	t.Helper()
	hub, err := semhub.New(semhub.Options{})
	if err != nil {
		t.Fatal(err)
	}
	bindings := make([]semanticmetrics.Binding, 40)
	for i := len(bindings) - 1; i >= 0; i-- {
		bindings[i] = publishServedZone(t, hub, i)
	}
	provider := newGatewaySemanticSnapshotProvider(&gatewayRuntimeControl{hub: hub})
	reads := 0
	provider.now = func() time.Time {
		reads++
		return time.Unix(0, m38Wall+int64(time.Second))
	}
	return provider, bindings, &reads
}

func TestSemanticMetricScrapeReadsWholeServedCut(t *testing.T) {
	provider, bindings, reads := servedZones(t)
	metrics, err := newGatewaySemanticMetrics(provider, semanticmetrics.Registry{Validated: true, Bindings: []semanticmetrics.Binding{bindings[0], bindings[39]}})
	if err != nil {
		t.Fatal(err)
	}
	store := ebusgateway.NewBusObservabilityStore(ebusgateway.DefaultConfig())
	body := m38Scrape(t, newGatewayMetricsHandler(store, metrics))
	const family = "helianthus_semantic_fact_value{"
	if got := strings.Count(body, "\n"+family); got != len(bindings) || *reads != 1 {
		t.Fatalf("canonical values=%d want %d, capture clock samples=%d want 1", got, len(bindings), *reads)
	}
	for i, binding := range bindings {
		line := fmt.Sprintf(`%sdimension="thermal.dimension.zone",fact="thermal.measurement.air_temperature",pack="helianthus.pack.thermal",resource="%s",resource_kind="resource.zone",unit="unit.kelvin"} %d`, family, binding.ResourceID, 280+i)
		if !strings.Contains(body, "\n"+line+"\n") {
			t.Errorf("missing %s", line)
		}
	}
	for _, i := range []int{0, 39} {
		line := fmt.Sprintf(`helianthus_thermal_air_temperature{slot="%d",unit="kelvin"} %d`, i, 280+i)
		if !strings.Contains(body, "\n"+line+"\n") {
			t.Errorf("missing configured %s", line)
		}
	}
}

func TestSemanticMetricReconcilePersistsWholeServedCut(t *testing.T) {
	provider, bindings, reads := servedZones(t)
	directory := gatewayMetricStoreDir(t)
	metrics, err := newGatewaySemanticMetrics(provider, gatewaySemanticMetricRegistry(directory))
	if err != nil {
		t.Fatal(err)
	}
	assertReconciled := func(want []semanticmetrics.Binding, calls int) {
		t.Helper()
		if err := metrics.Reconcile(context.Background()); err != nil {
			t.Fatal(err)
		}
		if *reads != calls {
			t.Fatalf("capture clock samples=%d want %d", *reads, calls)
		}
		active, _ := metrics.configured()
		for _, registry := range []semanticmetrics.Registry{active, gatewaySemanticMetricRegistry(directory)} {
			if !registry.Validated || len(registry.Bindings) != len(want) {
				t.Fatalf("validated=%v bindings=%d want %d", registry.Validated, len(registry.Bindings), len(want))
			}
			for i, binding := range registry.Bindings {
				if !reflect.DeepEqual(binding, want[i]) {
					t.Errorf("slot %d: got %+v, want %+v", i, binding, want[i])
				}
			}
		}
	}
	assertReconciled(bindings, 1)
	bindings = append(bindings, publishServedZone(t, provider.hub.(*semhub.Hub), len(bindings)))
	assertReconciled(bindings, 2)
}

type failedServedMetricRead struct {
	*semhub.Hub
	fail          error
	explicitCalls int
}

func (h *failedServedMetricRead) Capture(ctx context.Context, ids []semreg.AssetID, now func() time.Time) (semhub.CoherentCapture, error) {
	h.explicitCalls++
	return h.Hub.Capture(ctx, ids, now)
}

func (h *failedServedMetricRead) CaptureServed(ctx context.Context, now func() time.Time) (semhub.CoherentCapture, error) {
	capture, err := h.Hub.CaptureServed(ctx, now)
	if err == nil && h.fail != nil {
		capture.Assets = capture.Assets[:1]
		return capture, h.fail
	}
	return capture, err
}

func TestSemanticMetricServedReadFailureIsNotASmallerCut(t *testing.T) {
	ctx := context.Background()
	hub, err := semhub.New(semhub.Options{})
	if err != nil {
		t.Fatal(err)
	}
	publishServedZone(t, hub, 0)
	publishServedZone(t, hub, 1)
	source := &failedServedMetricRead{Hub: hub}
	provider := &hubSemanticSnapshotProvider{hub: source, now: func() time.Time { return time.Unix(0, m38Wall+int64(time.Second)) }}
	directory := gatewayMetricStoreDir(t)
	metrics, err := newGatewaySemanticMetrics(provider, gatewaySemanticMetricRegistry(directory))
	if err != nil {
		t.Fatal(err)
	}
	var logged []string
	metrics.logf = func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
	if err := metrics.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	stored := gatewayMetricStoreBytes(t, directory)
	handler := newGatewayMetricsHandler(ebusgateway.NewBusObservabilityStore(ebusgateway.DefaultConfig()), metrics)
	const canonical = "\nhelianthus_semantic_fact_value{"
	const slot = "\nhelianthus_thermal_air_temperature{"
	if body := m38Scrape(t, handler); strings.Count(body, canonical) != 2 || strings.Count(body, slot) != 2 {
		t.Fatal("initial scrape did not serve both canonical and slot values")
	}

	source.fail = errors.New("synthetic served read failed")
	body := m38Scrape(t, handler)
	if strings.Contains(body, canonical) || strings.Contains(body, slot) || !strings.Contains(body, m38AvailabilityName+`{domain="thermal",field="thermal.measurement.air_temperature",slot="0"} 0`+"\n") {
		t.Fatal("scrape did not withhold values and report configured availability as zero")
	}
	if len(logged) != 1 || !strings.Contains(logged[0], "capture: "+source.fail.Error()) {
		t.Fatalf("scrape did not report the capture error: %v", logged)
	}
	if err := metrics.Reconcile(ctx); !errors.Is(err, source.fail) {
		t.Fatalf("reconcile did not propagate the capture error: %v", err)
	}
	if after := gatewayMetricStoreBytes(t, directory); after != stored {
		t.Fatal("failed read changed the slot store")
	}
	if source.explicitCalls != 0 {
		t.Fatalf("made %d explicit captures despite having a served read", source.explicitCalls)
	}
}
