package main

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/Project-Helianthus/helianthus-gateway/internal/semanticmetrics"
	"github.com/Project-Helianthus/helianthus-gateway/internal/semhub"
	semreg "github.com/Project-Helianthus/helianthus-semreg/semreg/v1"
)

// reconcileCutSource keeps the m38 capture fixture, but declares a served cut
// so the test drives reconciliation and the HTTP scrape of that same cut.
type reconcileCutSource struct{ *m38CaptureSource }

func (s reconcileCutSource) AssetIDs() []semreg.AssetID { return s.hub.AssetIDs() }

// The snapshot identifier hashes the canonical body without the identifier
// itself. EvaluateSnapshot below validates the resulting whole record.
type reconcileSnapshotBody struct {
	semreg.Snapshot
	OmitID *semreg.SnapshotID `json:"snapshot_id,omitempty"`
}

func (reconcileSnapshotBody) Validate() error { return nil }

func TestSemanticMetricReconcileHoldsCurrentZoneWithoutProvenance(t *testing.T) {
	const (
		zone  semreg.ResourceID   = "resource:zone.synthetic"
		field semreg.DefinitionID = "thermal.measurement.air_temperature"
	)
	for _, missing := range []string{"binding", "service"} {
		t.Run(missing, func(t *testing.T) {
			ctx := context.Background()
			f := m38Setup(t)
			thermal := m38Domains()[0]
			dimension := thermal.dimension(t, field)
			zoneService := thermal.service(dimension)
			f.provider.hub = reconcileCutSource{f.source}
			gap := false
			state := semreg.ResourceCurrent
			f.source.mutate = func(capture *semhub.CoherentCapture) {
				for i := range capture.Assets {
					item := &capture.Assets[i]
					if item.AssetID != thermal.asset {
						continue
					}
					snapshot := *item.Snapshot
					text := string(zone)
					evidence := []semreg.EvidenceRef{m38Evidence()}
					snapshot.Resources = []semreg.Resource{{ResourceID: zone, Kind: "resource.zone", State: state, Revision: "1",
						Anchor: semreg.ResourceAnchor{Class: semreg.AnchorFunction, Basis: semreg.AnchorOperatorSlot, Slot: &text, Evidence: evidence},
						Origin: semreg.OriginRef{OriginID: "origin:synthetic-zone", Kind: semreg.OriginOperator, Evidence: evidence}}}
					snapshot.Facts = []semreg.FactEnvelope{}
					for _, envelope := range item.Snapshot.Facts {
						if envelope.Key.FactID != field {
							continue
						}
						envelope.Key.Dimensions = []semreg.Dimension{{ID: dimension, Value: semreg.Value{Kind: semreg.ValueText, Text: &text}}}
						candidate := envelope.Candidates[0]
						candidate.Key = envelope.Key
						envelope.Candidates = []semreg.FactCandidate{candidate}
						snapshot.Facts = append(snapshot.Facts, envelope)
					}
					snapshot.Services = slices.DeleteFunc(slices.Clone(snapshot.Services), func(service semreg.ServiceInstance) bool {
						return service.InstanceID != zoneService
					})
					snapshot.Capabilities = slices.DeleteFunc(slices.Clone(snapshot.Capabilities), func(capability semreg.CapabilityInstance) bool {
						return capability.ServiceInstance != zoneService
					})
					if state == "" {
						snapshot.Resources, snapshot.Facts = nil, []semreg.FactEnvelope{}
					}
					if gap {
						snapshot.Services, snapshot.Capabilities = []semreg.ServiceInstance{}, []semreg.CapabilityInstance{}
						if missing == "binding" {
							// A valid publication cannot keep an observation or link dangling
							// from a missing binding. The independent resource is still present.
							snapshot.Bindings, snapshot.IdentityLinks = []semreg.NativeBinding{}, []semreg.IdentityLink{}
							snapshot.Facts = []semreg.FactEnvelope{}
						}
					}
					digest, err := semreg.DigestRecord(reconcileSnapshotBody{Snapshot: snapshot})
					if err != nil {
						t.Fatal(err)
					}
					snapshot.SnapshotID = semreg.SnapshotID(digest)
					evaluation, err := semreg.EvaluateSnapshot(snapshot, item.Evaluation.Context)
					if err != nil {
						t.Fatalf("evaluate synthetic snapshot: %v", err)
					}
					if gap && missing == "service" {
						members := cutMembersOf(snapshot)
						if len(members) != 1 || !members[0].Held || members[0].ResourceID != zone || members[0].FactID != field {
							t.Fatalf("current fact with no service was not listed as held: %+v", members)
						}
					}
					item.Snapshot, item.Evaluation = &snapshot, &evaluation
				}
			}
			directory := gatewayMetricStoreDir(t)
			metrics, err := newGatewaySemanticMetrics(f.provider, gatewaySemanticMetricRegistry(directory))
			if err != nil {
				t.Fatal(err)
			}
			handler := newGatewayMetricsHandler(f.store, metrics)
			reconcile := func() semanticmetrics.Binding {
				t.Helper()
				if err := metrics.Reconcile(ctx); err != nil {
					t.Fatal(err)
				}
				registry, _ := metrics.configured()
				var found []semanticmetrics.Binding
				for _, binding := range registry.Bindings {
					if binding.ResourceID == zone {
						found = append(found, binding)
					}
				}
				if len(found) != 1 || len(found[0].Fields) != 1 || found[0].Fields[0] != field {
					t.Fatalf("expected exactly one zone assignment for %s, got %+v", field, found)
				}
				return found[0]
			}
			first := reconcile()
			stored := gatewayMetricStoreBytes(t, directory)
			const series = "\nhelianthus_thermal_air_temperature{"
			value := fmt.Sprintf(`%sslot="%d",unit="kelvin"} 294.15`, series, first.Slot) + "\n"
			availability := fmt.Sprintf(`
helianthus_semantic_field_available{domain="thermal",field="%s",slot="%d"} `, field, first.Slot)
			assertScrape := func(exported bool) {
				t.Helper()
				body := m38Scrape(t, handler)
				if exported {
					if !strings.Contains(body, value) || !strings.Contains(body, availability+"1\n") {
						t.Fatalf("slot %d did not export the zone value:\n%s", first.Slot, body)
					}
				} else if strings.Contains(body, series) || !strings.Contains(body, availability+"0\n") {
					t.Fatalf("slot %d did not withhold the zone value:\n%s", first.Slot, body)
				}
				if !strings.Contains(body, "\nhelianthus_infrastructure_ac_voltage_volts{") {
					t.Fatal("the independent infrastructure value was withheld")
				}
			}
			assertScrape(true)

			gap = true
			held := reconcile()
			if held.Retired || held.Slot != first.Slot || stored != gatewayMetricStoreBytes(t, directory) {
				t.Fatalf("missing %s changed the active assignment: before %+v, after %+v", missing, first, held)
			}
			assertScrape(false)

			gap = false
			restored := reconcile()
			if restored.Retired || restored.Slot != first.Slot || stored != gatewayMetricStoreBytes(t, directory) {
				t.Fatalf("restored %s changed the active assignment: before %+v, after %+v", missing, first, restored)
			}
			assertScrape(true)

			state = semreg.ResourceWithdrawn
			if missing == "service" {
				state = "" // The zone can also be absent from the next cut.
			}
			retired := reconcile()
			if !retired.Retired || retired.Slot != first.Slot {
				t.Fatalf("noncurrent zone did not retire its original slot: %+v", retired)
			}
			assertScrape(false)
		})
	}
}
