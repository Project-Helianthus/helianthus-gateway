package main

import (
	"strings"
	"testing"
	"time"

	ebusgateway "github.com/Project-Helianthus/helianthus-gateway"
	"github.com/Project-Helianthus/helianthus-gateway/internal/semanticmetrics"
	"github.com/Project-Helianthus/helianthus-gateway/internal/semhub"
	semreg "github.com/Project-Helianthus/helianthus-semreg/semreg/v1"
)

const (
	ownerAsset       = semreg.AssetID("asset:owner-shared")
	ownerResource    = semreg.ResourceID("resource:owner-shared")
	ownerSource      = semreg.SourceID("source:owner-shared")
	contributorSrc   = semreg.SourceID("source:contributor-shared")
	ownerDriver      = "served.owner"
	contributorDrive = "served.contributor"
)

// publishOwnerAndContributor declares one asset with an owning source and one
// contributor and publishes the same air-temperature fact from both. The owner
// observes at offset 0 and the contributor at contributorAt. It returns the
// scrape body at scrapeAt nanoseconds after the owner's observation.
func publishOwnerAndContributor(t *testing.T, ownerAvailability semreg.Availability, contributorAt, scrapeAt time.Duration) string {
	t.Helper()
	hub, err := semhub.New(semhub.Options{})
	if err != nil {
		t.Fatal(err)
	}
	thermal := m38Domains()[0]
	if _, err := hub.Declare(semhub.AssetSpec{AssetID: ownerAsset, SourceID: ownerSource, Driver: ownerDriver, Validators: []semreg.PackValidator{thermal.validator}, Graph: true,
		Contributors: []semhub.ContributorSpec{{SourceID: contributorSrc, Driver: contributorDrive, Validators: []semreg.PackValidator{thermal.validator}}}}); err != nil {
		t.Fatal(err)
	}
	resource := ownerResource
	text := string(resource)
	dimensions := []semreg.Dimension{{ID: "thermal.dimension.zone", Value: semreg.Value{Kind: semreg.ValueText, Text: &text}}}
	evidence := m38Evidence()
	publish := func(source semreg.SourceID, driver, id string, at time.Duration, kelvin int64, first bool, availability semreg.Availability) {
		handle, err := hub.AdmitSource(semhub.Admission{Source: source, Driver: driver})
		if err != nil {
			t.Fatal(err)
		}
		tx, err := hub.Begin(handle, ownerAsset)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Abort()
		epoch, generation := handle.Epoch(), handle.GenerationID()
		wall, mono := m38Times(int64(at))
		binding := semreg.NativeBindingID("binding:" + id)
		service := semreg.ServiceInstanceID("service:" + id)
		value := semreg.Value{Kind: semreg.ValueQuantity, Quantity: &semreg.Quantity{Number: pairingWatts(kelvin), Unit: "unit.kelvin"}}
		expected := semreg.Uint64("0")
		if current, exists := tx.Current(); exists {
			expected = current.Revisions.Semantic
		}
		batch := semreg.PublicationBatch{
			Contract: semreg.ContractKernelV1, BatchID: semreg.BatchID("batch:" + id), AssetID: ownerAsset,
			SourceID: source, SourceEpochID: epoch, DriverGeneration: generation, Sequence: tx.NextSequence(), ExpectedSemanticRevision: expected, ObservedAt: wall,
			SourceUpserts:       []semreg.SourceDescriptor{{SourceID: source, SourceEpochID: epoch, ProtocolID: semreg.DefinitionID(driver), ProfileID: "served.profile", ProfileVersion: "1", RegistryEvidence: evidence, StartedAt: wall, State: semreg.SourceCurrent, Revision: "1"}},
			SourceRetirements:   tx.SourceRetirements(),
			BindingUpserts:      []semreg.NativeBinding{{BindingID: binding, AssetID: ownerAsset, SourceID: source, SourceEpochID: epoch, DriverGeneration: generation, NativeResource: evidence, State: semreg.BindingCurrent, Revision: "1"}},
			IdentityLinkUpserts: []semreg.IdentityLink{{AssetID: ownerAsset, BindingID: binding, State: semreg.LinkQualified, Basis: []semreg.EvidenceRef{evidence}, Resource: &resource, Revision: "1"}},
			ResourceUpserts:     []semreg.Resource{},
			ServiceUpserts:      []semreg.ServiceInstance{{InstanceID: service, AssetID: ownerAsset, Definition: thermal.ref("thermal.service.zone"), BindingID: binding, SourceEpochID: epoch, DriverGeneration: generation, Qualification: semreg.QualificationQualified, Availability: semreg.AvailabilityAvailable, Resource: &resource, Revision: "1"}},
			CapabilityUpserts:   []semreg.CapabilityInstance{{InstanceID: semreg.CapabilityInstanceID("capability:" + id), AssetID: ownerAsset, ServiceInstance: service, Definition: thermal.ref("thermal.capability.read.zone"), BindingID: binding, SourceEpochID: epoch, DriverGeneration: generation, Qualification: semreg.QualificationQualified, Availability: semreg.AvailabilityAvailable, Constraints: []semreg.TypedField{}, ActivationEvidence: []semreg.EvidenceRef{evidence}, Revision: "1"}},
			FactUpserts: []semreg.FactCandidate{{CandidateID: semreg.CandidateID("candidate:" + id),
				Key:             semreg.FactKey{PackID: thermal.metadata.Pack.ID, PackVersion: thermal.metadata.Pack.Version, FactID: servedZoneField, Dimensions: dimensions},
				Value:           &value,
				Quality:         semreg.Quality{Assertion: semreg.AssertionObserved, Qualification: semreg.QualificationQualified, Promotion: semreg.PromotionPromoted, Validity: semreg.ValidityGood, Availability: availability, Freshness: semreg.FreshnessFresh, Reasons: []semreg.DefinitionID{}},
				Times:           semreg.Times{ReceivedAt: wall, ReceiptMonotonic: mono, EvaluatedAt: wall, EvaluateMonotonic: mono},
				FreshnessPolicy: semreg.FreshnessPolicy{PolicyID: "policy:served-synthetic", Version: "1.0.0", FreshForNS: "60000000000", RetainForNS: "300000000000", MaxWallUncertaintyNS: "0"},
				BindingID:       &binding, SourceEpochID: &epoch, DriverGeneration: &generation,
				Origin:   semreg.OriginRef{OriginID: semreg.OriginID("origin:observation-" + id), Kind: semreg.OriginNativeObservation, SourceID: &source, SourceEpochID: &epoch, BindingID: &binding, Evidence: []semreg.EvidenceRef{evidence}},
				Evidence: []semreg.EvidenceRef{evidence}, Revision: "1"}},
			FactWithdrawals: []semreg.CandidateID{}, ServiceWithdrawals: []semreg.ServiceInstanceID{}, CapabilityWithdrawals: []semreg.CapabilityInstanceID{}, GenerationFences: []semreg.GenerationFence{},
		}
		if first {
			anchorSlot := "owner-shared"
			batch.ResourceUpserts = []semreg.Resource{{ResourceID: resource, Kind: "resource.zone", State: semreg.ResourceCurrent, Revision: "1",
				Anchor: semreg.ResourceAnchor{Class: semreg.AnchorFunction, Basis: semreg.AnchorOperatorSlot, Slot: &anchorSlot, Evidence: []semreg.EvidenceRef{evidence}},
				Origin: semreg.OriginRef{OriginID: "origin:resource-owner-shared", Kind: semreg.OriginOperator, Evidence: []semreg.EvidenceRef{evidence}}}}
		}
		var err2 error
		batch.BatchDigest, err2 = batch.ComputedDigest()
		if err2 != nil {
			t.Fatal(err2)
		}
		if _, _, err := tx.Apply(batch, mono); err != nil {
			t.Fatalf("apply %s: %v", id, err)
		}
		if outcome, err := tx.Commit(nil); err != nil || outcome != semhub.Accepted {
			t.Fatalf("commit %s: %s, %v", id, outcome, err)
		}
	}
	publish(ownerSource, ownerDriver, "owner", 0, 281, true, ownerAvailability)
	publish(contributorSrc, contributorDrive, "contributor", contributorAt, 281, false, semreg.AvailabilityAvailable)

	provider := newGatewaySemanticSnapshotProvider(&gatewayRuntimeControl{hub: hub})
	provider.now = func() time.Time { return time.Unix(0, m38Wall+int64(scrapeAt)) }
	metrics, err := newGatewaySemanticMetrics(provider, semanticmetrics.Registry{Validated: true})
	if err != nil {
		t.Fatal(err)
	}
	metrics.now = provider.now
	store := ebusgateway.NewBusObservabilityStore(ebusgateway.DefaultConfig())
	return m38Scrape(t, newGatewayMetricsHandler(store, metrics))
}

// TestSemanticMetricOwnerStateIsShownThroughSharedCut runs the scrape through
// the shared-cut path with a real hub: one asset, an owning source and a
// contributor on one conflict-free key. The owner observed at 0 s and the
// contributor at 100 s; the scrape is at 120 s, so with a fresh period of 60 s
// the owner is stale and the contributor fresh.
// Production mutations caught:
//   - owner stale, contributor fresh: a servedDomains that drops asset.Owner,
//     or a presentation that falls back to the single qualified candidate,
//     presents the contributor (fresh, age 20, a value).
//   - owner fresh, contributor fresh (scrape at 30 s... contributor at 10 s):
//     a presentation that prefers the newest candidate shows age 20 not 30.
func TestSemanticMetricOwnerStateIsShownThroughSharedCut(t *testing.T) {
	const fact = `fact="thermal.measurement.air_temperature"`
	t.Run("owner stale contributor fresh", func(t *testing.T) {
		body := publishOwnerAndContributor(t, semreg.AvailabilityAvailable, 100*time.Second, 120*time.Second)
		state := lineWith(body, "helianthus_semantic_fact_state{", fact)
		if !strings.Contains(state, `freshness="stale"`) {
			t.Fatalf("state is not the owner's stale state: %q\n%s", state, body)
		}
		if age := lineWith(body, "helianthus_semantic_evidence_age{", fact); !strings.HasSuffix(age, "} 120") {
			t.Fatalf("age is not the owner's 120 s: %q\n%s", age, body)
		}
		if value := lineWith(body, "helianthus_semantic_fact_value{", fact); value != "" {
			t.Fatalf("a value was exported for a stale owner: %q", value)
		}
	})
	t.Run("owner unavailable contributor fresh", func(t *testing.T) {
		body := publishOwnerAndContributor(t, semreg.AvailabilityUnavailable, 100*time.Second, 120*time.Second)
		if state := lineWith(body, "helianthus_semantic_fact_state{", fact); !strings.Contains(state, `availability="unavailable"`) {
			t.Fatalf("state is not the owner's unavailable state: %q\n%s", state, body)
		}
		if value := lineWith(body, "helianthus_semantic_fact_value{", fact); value != "" {
			t.Fatalf("a contributor's value replaced an unavailable owner: %q", value)
		}
	})
	t.Run("owner fresh contributor fresh", func(t *testing.T) {
		body := publishOwnerAndContributor(t, semreg.AvailabilityAvailable, 10*time.Second, 30*time.Second)
		if age := lineWith(body, "helianthus_semantic_evidence_age{", fact); !strings.HasSuffix(age, "} 30") {
			t.Fatalf("age is not the owner's 30 s: %q", age)
		}
		if value := lineWith(body, "helianthus_semantic_fact_value{", fact); !strings.HasSuffix(value, "} 281") {
			t.Fatalf("the owner's value is missing: %q", value)
		}
	})
}

func lineWith(body, prefix, contains string) string {
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, prefix) && strings.Contains(line, contains) {
			return line
		}
	}
	return ""
}
