package ebusgateway

import (
	"bytes"
	"strconv"
	"strings"
	"testing"
	"time"

	semreg "github.com/Project-Helianthus/helianthus-semreg/semreg/v1"
	"github.com/Project-Helianthus/helianthus-semreg/semreg/v1/projection"
)

func TestSemanticPrometheusRendersOnlyFreshQualifiedNumericFacts(t *testing.T) {
	domain := semanticMetricsFixture("pv", true, semreg.FreshnessFresh, semreg.AvailabilityAvailable, semreg.ValidityGood, 0)
	var out bytes.Buffer
	writeSemanticMetrics(newPrometheusWriter(&out), []SemanticMetricsDomain{domain}, time.Unix(101, 0))
	metrics := out.String()
	if !strings.Contains(metrics, `helianthus_semantic_fact_value{dimension="inverter",fact="pv.ac.aggregate_active_power",pack="helianthus.pack.pv",resource="resource:inverter.synthetic",resource_kind="resource.inverter",unit="unit.watt"} 42`) {
		t.Fatalf("missing accepted value:\n%s", metrics)
	}
	if strings.Contains(metrics, "asset:") {
		t.Fatalf("forbidden asset identifier leaked:\n%s", metrics)
	}
	for _, line := range strings.Split(metrics, "\n") {
		if strings.HasPrefix(line, "helianthus_semantic_fact_") || strings.HasPrefix(line, "helianthus_semantic_evidence_age{") || strings.HasPrefix(line, "helianthus_semantic_open_conflicts{") {
			if strings.Contains(line, "domain=") || strings.Contains(line, "fact_id=") {
				t.Fatalf("per-fact series kept a domain or fact_id label:\n%s", line)
			}
		}
	}
	if !strings.Contains(metrics, `helianthus_semantic_projection_available{domain="pv"} 1`) {
		t.Fatalf("projection family lost its domain label:\n%s", metrics)
	}
	if !strings.Contains(metrics, `helianthus_semantic_evidence_age{dimension="inverter",fact="pv.ac.aggregate_active_power",pack="helianthus.pack.pv",resource="resource:inverter.synthetic",resource_kind="resource.inverter",unit="seconds"} 1`) {
		t.Fatalf("missing evidence age:\n%s", metrics)
	}
}

func TestSemanticPrometheusWithholdsStaleAndConflictedValues(t *testing.T) {
	for _, tc := range []struct {
		name      string
		freshness semreg.Freshness
		conflicts int
	}{{"stale", semreg.FreshnessStale, 0}, {"conflict", semreg.FreshnessFresh, 1}} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			writeSemanticMetrics(newPrometheusWriter(&out), []SemanticMetricsDomain{semanticMetricsFixture("storage", true, tc.freshness, semreg.AvailabilityAvailable, semreg.ValidityGood, tc.conflicts)}, time.Unix(101, 0))
			metrics := out.String()
			if strings.Contains(metrics, "\nhelianthus_semantic_fact_value{") {
				t.Fatalf("unsafe value leakage:\n%s", metrics)
			}
			if !strings.Contains(metrics, "helianthus_semantic_fact_state") || !strings.Contains(metrics, "helianthus_semantic_open_conflicts") {
				t.Fatalf("missing truthful state:\n%s", metrics)
			}
		})
	}
}

func TestSemanticPrometheusRejectsIncoherentTuple(t *testing.T) {
	domain := semanticMetricsFixture("pv", true, semreg.FreshnessFresh, semreg.AvailabilityAvailable, semreg.ValidityGood, 0)
	domain.Projection.SnapshotID = "snapshot:other"
	var out bytes.Buffer
	writeSemanticMetrics(newPrometheusWriter(&out), []SemanticMetricsDomain{domain}, time.Unix(101, 0))
	if strings.Contains(out.String(), "\nhelianthus_semantic_fact_value{") || !strings.Contains(out.String(), `helianthus_semantic_projection_available{domain="pv"} 0`) || !strings.Contains(out.String(), "helianthus_semantic_render_overflow 1") {
		t.Fatalf("incoherent tuple was rendered:\n%s", out.String())
	}
}

func TestSemanticPrometheusRejectsUnknownPVProjectionItems(t *testing.T) {
	domain := semanticMetricsFixture("pv", true, semreg.FreshnessFresh, semreg.AvailabilityAvailable, semreg.ValidityGood, 0)
	for _, suffix := range []string{"attacker-unique-suffix-a", "attacker-unique-suffix-b", "attacker-unique-suffix-c"} {
		domain.Projection.Dispositions = append(domain.Projection.Dispositions, projection.ProjectionDisposition{
			ItemID:     semreg.DefinitionID("projection.gateway.pv.inverter." + suffix),
			Outcome:    projection.ProjectionExact,
			SourceKeys: domain.Projection.Dispositions[0].SourceKeys,
		})
	}
	var out bytes.Buffer
	writeSemanticMetrics(newPrometheusWriter(&out), []SemanticMetricsDomain{domain}, time.Unix(101, 0))
	metrics := out.String()
	if strings.Contains(metrics, "attacker-unique-suffix") {
		t.Fatalf("unbounded PV item label leaked:\n%s", metrics)
	}
	if !strings.Contains(metrics, "helianthus_semantic_render_overflow 3") {
		t.Fatalf("rejected item was not counted:\n%s", metrics)
	}
}

func TestSemanticPrometheusRejectsInvalidStateLabels(t *testing.T) {
	domain := semanticMetricsFixture("pv", true, semreg.FreshnessFresh, semreg.AvailabilityAvailable, semreg.ValidityGood, 0)
	domain.Evaluation.Facts[0].Freshness = semreg.Freshness("asset:private")
	var out bytes.Buffer
	writeSemanticMetrics(newPrometheusWriter(&out), []SemanticMetricsDomain{domain}, time.Unix(101, 0))
	metrics := out.String()
	if strings.Contains(metrics, "asset:private") || strings.Contains(metrics, "\nhelianthus_semantic_fact_state{") {
		t.Fatalf("invalid semantic state leaked:\n%s", metrics)
	}
	if !strings.Contains(metrics, "helianthus_semantic_render_overflow 1") {
		t.Fatalf("invalid state was not counted:\n%s", metrics)
	}
}

func TestSemanticFactKeyRejectsInvalidKeys(t *testing.T) {
	invalid := semreg.FactKey{PackID: "not a definition id"}
	if key, ok := semanticFactKey(invalid); ok || key != "" {
		t.Fatalf("invalid fact key was canonicalized: %q, %t", key, ok)
	}
}

func TestSemanticPrometheusRejectsDimensionlessFacts(t *testing.T) {
	for _, domainName := range []string{"pv", "storage"} {
		t.Run(domainName, func(t *testing.T) {
			domain := semanticMetricsFixture(domainName, true, semreg.FreshnessFresh, semreg.AvailabilityAvailable, semreg.ValidityGood, 0)
			key := domain.Snapshot.Facts[0].Key
			key.Dimensions = nil
			domain.Snapshot.Facts[0].Key = key
			domain.Snapshot.Facts[0].Candidates[0].Key = key
			domain.Projection.Dispositions[0].SourceKeys[0] = key
			var out bytes.Buffer
			writeSemanticMetrics(newPrometheusWriter(&out), []SemanticMetricsDomain{domain}, time.Unix(101, 0))
			metrics := out.String()
			if strings.Contains(metrics, `dimension=""`) || strings.Contains(metrics, "\nhelianthus_semantic_fact_state{") || strings.Contains(metrics, "\nhelianthus_semantic_fact_value{") {
				t.Fatalf("dimensionless fact leaked:\n%s", metrics)
			}
			if !strings.Contains(metrics, "helianthus_semantic_render_overflow 1") {
				t.Fatalf("dimensionless fact was not counted:\n%s", metrics)
			}
		})
	}
}

func TestSemanticPrometheusRejectsCrossSchemaValues(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*SemanticMetricsDomain)
	}{
		{"boolean-power", func(domain *SemanticMetricsDomain) {
			value := true
			domain.Snapshot.Facts[0].Candidates[0].Value = &semreg.Value{Kind: semreg.ValueBoolean, Boolean: &value}
		}},
		{"wrong-unit", func(domain *SemanticMetricsDomain) {
			domain.Snapshot.Facts[0].Candidates[0].Value.Quantity.Unit = "unit.volt"
		}},
		{"future-pack", func(domain *SemanticMetricsDomain) {
			key := domain.Snapshot.Facts[0].Key
			key.PackVersion = "2.0.0"
			domain.Snapshot.Facts[0].Key = key
			domain.Snapshot.Facts[0].Candidates[0].Key = key
			domain.Projection.Dispositions[0].SourceKeys[0] = key
		}},
		{"fact-item-mismatch", func(domain *SemanticMetricsDomain) {
			key := domain.Snapshot.Facts[0].Key
			key.FactID = "pv.ac.frequency"
			domain.Snapshot.Facts[0].Key = key
			domain.Snapshot.Facts[0].Candidates[0].Key = key
			domain.Projection.Dispositions[0].SourceKeys[0] = key
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			domain := semanticMetricsFixture("pv", true, semreg.FreshnessFresh, semreg.AvailabilityAvailable, semreg.ValidityGood, 0)
			tc.mutate(&domain)
			var out bytes.Buffer
			writeSemanticMetrics(newPrometheusWriter(&out), []SemanticMetricsDomain{domain}, time.Unix(101, 0))
			metrics := out.String()
			if strings.Contains(metrics, "\nhelianthus_semantic_fact_state{") || strings.Contains(metrics, "\nhelianthus_semantic_fact_value{") || strings.Contains(metrics, "unit=\"boolean\"") {
				t.Fatalf("cross-schema input leaked:\n%s", metrics)
			}
			if !strings.Contains(metrics, "helianthus_semantic_render_overflow 1") {
				t.Fatalf("cross-schema input was not counted:\n%s", metrics)
			}
		})
	}
}

func TestSemanticPrometheusEVSEExportsBoundedConfiguredAndConnectorFacts(t *testing.T) {
	domain := semanticMetricsFixture("evse", true, semreg.FreshnessFresh, semreg.AvailabilityAvailable, semreg.ValidityGood, 0)
	domain.Snapshot.Resources = []semreg.Resource{
		semanticResourceFixture("resource:evse.synthetic", "resource.evse", ""),
		semanticResourceFixture("resource:connector.a", "resource.connector", ""),
		semanticResourceFixture("resource:connector.b", "resource.connector", ""),
	}
	addEVSEAllocatedFixture(&domain, "resource:connector.a", "candidate:allocated-a")
	addEVSEAllocatedFixture(&domain, "resource:connector.b", "candidate:allocated-b")
	var out bytes.Buffer
	writeSemanticMetrics(newPrometheusWriter(&out), []SemanticMetricsDomain{domain}, time.Unix(101, 0))
	metrics := out.String()
	for _, want := range []string{
		`fact="evse.limit.configured_current"`,
		`resource="resource:evse.synthetic"`,
		`connector="resource:connector.a"`,
		`connector="resource:connector.b"`,
		`fact="evse.limit.allocated_current"`,
	} {
		if !strings.Contains(metrics, want) {
			t.Fatalf("missing EVSE metric %q:\n%s", want, metrics)
		}
	}
	if strings.Contains(metrics, "connector_1") || strings.Contains(metrics, "connector_2") || strings.Contains(metrics, "asset:") {
		t.Fatalf("EVSE ordinal or asset leaked through labels:\n%s", metrics)
	}
}

func TestSemanticPrometheusEVSEWithheldAllocationDoesNotSuppressConfiguredCurrent(t *testing.T) {
	domain := semanticMetricsFixture("evse", true, semreg.FreshnessFresh, semreg.AvailabilityAvailable, semreg.ValidityGood, 0)
	reason := semreg.DefinitionID("withheld_provisional_missing")
	domain.Projection.Dispositions = append(domain.Projection.Dispositions, projection.ProjectionDisposition{ItemID: "evse.limit.allocated_current", Outcome: projection.ProjectionWithheld, Reason: &reason, Loss: []projection.LossDetail{{Kind: projection.LossPolicy}}})
	var out bytes.Buffer
	writeSemanticMetrics(newPrometheusWriter(&out), []SemanticMetricsDomain{domain}, time.Unix(101, 0))
	metrics := out.String()
	if !strings.Contains(metrics, `helianthus_semantic_fact_value{dimension="evse",fact="evse.limit.configured_current",pack="helianthus.pack.evse",resource="resource:evse.synthetic",resource_kind="resource.evse",unit="unit.ampere"} 42`) || !strings.Contains(metrics, `item="evse.limit.allocated_current",loss_kind="policy"`) {
		t.Fatalf("withheld allocation changed configured current:\n%s", metrics)
	}
}

func TestSemanticPrometheusEVSERejectsHostileSchemaAndConnectorOverflow(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*SemanticMetricsDomain)
	}{
		{"future-pack", func(domain *SemanticMetricsDomain) {
			key := domain.Snapshot.Facts[0].Key
			key.PackVersion = "2.0.0"
			domain.Snapshot.Facts[0].Key, domain.Snapshot.Facts[0].Candidates[0].Key, domain.Projection.Dispositions[0].SourceKeys[0] = key, key, key
		}},
		{"wrong-dimension", func(domain *SemanticMetricsDomain) {
			key := domain.Snapshot.Facts[0].Key
			key.Dimensions[0].ID = "evse.dimension.connector"
			domain.Snapshot.Facts[0].Key, domain.Snapshot.Facts[0].Candidates[0].Key, domain.Projection.Dispositions[0].SourceKeys[0] = key, key, key
		}},
		{"wrong-unit", func(domain *SemanticMetricsDomain) {
			domain.Snapshot.Facts[0].Candidates[0].Value.Quantity.Unit = "unit.volt"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			domain := semanticMetricsFixture("evse", true, semreg.FreshnessFresh, semreg.AvailabilityAvailable, semreg.ValidityGood, 0)
			tc.mutate(&domain)
			var out bytes.Buffer
			writeSemanticMetrics(newPrometheusWriter(&out), []SemanticMetricsDomain{domain}, time.Unix(101, 0))
			metrics := out.String()
			if strings.Contains(metrics, "\nhelianthus_semantic_fact_state{") || strings.Contains(metrics, "\nhelianthus_semantic_fact_value{") || !strings.Contains(metrics, "helianthus_semantic_render_overflow 1") {
				t.Fatalf("hostile EVSE input was rendered:\n%s", metrics)
			}
		})
	}
	domain := semanticMetricsFixture("evse", true, semreg.FreshnessFresh, semreg.AvailabilityAvailable, semreg.ValidityGood, 0)
	domain.Snapshot.Resources = []semreg.Resource{semanticResourceFixture("resource:evse.synthetic", "resource.evse", "")}
	addEVSEAllocatedFixture(&domain, "resource:connector.absent", "candidate:allocated-absent")
	var out bytes.Buffer
	writeSemanticMetrics(newPrometheusWriter(&out), []SemanticMetricsDomain{domain}, time.Unix(101, 0))
	metrics := out.String()
	if strings.Contains(metrics, "resource:connector.absent") || !strings.Contains(metrics, "helianthus_semantic_render_overflow") {
		t.Fatalf("absent connector was exported:\n%s", metrics)
	}
}

func TestSemanticPrometheusEVSEConnectorSlotsIgnoreUnrenderableCandidates(t *testing.T) {
	domain := semanticMetricsFixture("evse", true, semreg.FreshnessFresh, semreg.AvailabilityAvailable, semreg.ValidityGood, 0)
	domain.Snapshot.Resources = []semreg.Resource{semanticResourceFixture("resource:connector.promoted", "resource.connector", "")}
	addEVSEAllocatedFixture(&domain, "resource:connector.absent", "candidate:rejected")
	addEVSEAllocatedFixture(&domain, "resource:connector.promoted", "candidate:promoted")
	var out bytes.Buffer
	writeSemanticMetrics(newPrometheusWriter(&out), []SemanticMetricsDomain{domain}, time.Unix(101, 0))
	metrics := out.String()
	if !strings.Contains(metrics, `helianthus_semantic_fact_value{connector="resource:connector.promoted",dimension="connector",fact="evse.limit.allocated_current",pack="helianthus.pack.evse",resource="resource:connector.promoted",resource_kind="resource.connector",unit="unit.ampere"} 16`) {
		t.Fatalf("renderable connector was dropped:\n%s", metrics)
	}
	if strings.Contains(metrics, "resource:connector.absent") {
		t.Fatalf("absent connector was exported:\n%s", metrics)
	}
}

func semanticMetricsFixture(domain string, available bool, freshness semreg.Freshness, availability semreg.Availability, validity semreg.Validity, conflicts int) SemanticMetricsDomain {
	dimensionID, dimensionValue, factID := semreg.DefinitionID("pv.dimension.inverter"), "resource:inverter.synthetic", semreg.DefinitionID("pv.ac.aggregate_active_power")
	unit := semreg.DefinitionID("unit.watt")
	if domain == "storage" {
		dimensionID, dimensionValue, factID, unit = "storage.dimension.pack", "resource:pack.synthetic", "storage.pack.voltage", "unit.volt"
	}
	if domain == "evse" {
		dimensionID, dimensionValue, factID, unit = "evse.dimension.evse", "resource:evse.synthetic", "evse.limit.configured_current", "unit.ampere"
	}
	version := semreg.SemanticVersion("1.0.0")
	if domain == "storage" {
		version = "1.1.0"
	}
	key := semreg.FactKey{PackID: "helianthus.pack." + semreg.DefinitionID(domain), PackVersion: version, FactID: factID, Dimensions: []semreg.Dimension{{ID: dimensionID, Value: semreg.Value{Kind: semreg.ValueText, Text: &dimensionValue}}}}
	value := semreg.Value{Kind: semreg.ValueQuantity, Quantity: &semreg.Quantity{Number: semreg.Decimal{Coefficient: "42"}, Unit: unit}}
	candidate := semreg.FactCandidate{CandidateID: "candidate:test", Key: key, Value: &value, Quality: semreg.Quality{Qualification: semreg.QualificationQualified, Promotion: semreg.PromotionPromoted, Validity: validity}, Times: semreg.Times{ReceivedAt: semreg.TimePoint{UnixNanoseconds: "100000000000", ClockID: "clock.utc"}}}
	envelope := semreg.FactEnvelope{Key: key, Candidates: []semreg.FactCandidate{candidate}}
	for i := 0; i < conflicts; i++ {
		envelope.Conflicts = append(envelope.Conflicts, semreg.Conflict{ConflictID: "conflict:test", Kind: semreg.ConflictValue, Candidates: []semreg.CandidateID{candidate.CandidateID}, State: semreg.ConflictOpen})
	}
	snapshot := semreg.Snapshot{SnapshotID: "snapshot:test", Facts: []semreg.FactEnvelope{envelope}}
	if domain == "pv" {
		// The PV fixture is graphed: its dimension value is the identifier of
		// the fact's subject resource.
		snapshot.Resources = []semreg.Resource{semanticResourceFixture(semreg.ResourceID(dimensionValue), "resource.inverter", "")}
	}
	if domain == "storage" {
		// So is the storage fixture: its pack value names a battery pack.
		snapshot.Resources = []semreg.Resource{semanticResourceFixture(semreg.ResourceID(dimensionValue), "resource.battery_pack", "")}
	}
	evaluation := semreg.EvaluationView{SnapshotID: snapshot.SnapshotID, Facts: []semreg.EvaluatedFact{{CandidateID: candidate.CandidateID, Freshness: freshness, EffectiveAvailability: availability}}, EvaluationDigest: semreg.Digest("sha256:" + strings.Repeat("0", 64))}
	item := semreg.DefinitionID("storage.pack.voltage")
	if domain == "pv" {
		item = "projection.gateway.pv.inverter.ac.power.active"
	}
	if domain == "evse" {
		item = "evse.limit.configured_current"
	}
	report := projection.ProjectionReport{SnapshotID: snapshot.SnapshotID, Dispositions: []projection.ProjectionDisposition{{ItemID: item, Outcome: projection.ProjectionExact, SourceKeys: []semreg.FactKey{key}}}}
	return SemanticMetricsDomain{Name: domain, Snapshot: snapshot, Evaluation: evaluation, Projection: report, Available: available}
}

// semanticResourceFixture is one current resource; a phase carries its slot.
func semanticResourceFixture(id semreg.ResourceID, kind semreg.DefinitionID, slot string) semreg.Resource {
	resource := semreg.Resource{ResourceID: id, Kind: kind, Anchor: semreg.ResourceAnchor{Class: semreg.AnchorEquipment, Basis: semreg.AnchorOperatorSlot}, State: semreg.ResourceCurrent, Revision: "1"}
	if slot != "" {
		resource.Anchor.Class, resource.Anchor.Basis, resource.Anchor.Slot = semreg.AnchorComponent, semreg.AnchorNativeSlot, &slot
	}
	return resource
}

// semanticPVFixtureAs rekeys the PV fixture's one fact as fact, dimension and
// value under item.
func semanticPVFixtureAs(domain *SemanticMetricsDomain, item, fact, dimension, value string, unit semreg.DefinitionID) {
	key := domain.Snapshot.Facts[0].Key
	key.FactID, key.Dimensions = semreg.DefinitionID(fact), []semreg.Dimension{{ID: semreg.DefinitionID(dimension), Value: semreg.Value{Kind: semreg.ValueText, Text: &value}}}
	domain.Snapshot.Facts[0].Key = key
	domain.Snapshot.Facts[0].Candidates[0].Key = key
	domain.Snapshot.Facts[0].Candidates[0].Value.Quantity.Unit = unit
	domain.Projection.Dispositions[0].ItemID = semreg.DefinitionID(item)
	domain.Projection.Dispositions[0].SourceKeys[0] = key
}

// TestSemanticPrometheusPVLabelsFollowResourceKindAndSlot: in a graphed
// snapshot the dimension label is derived from the subject resource's kind
// and, for a phase, its slot, and the identifier never leaves the renderer. A
// value naming no current resource, or one of another kind, renders no fact,
// and role text is not read in a graphed snapshot. A snapshot without
// resources keeps today's reading of its publisher's role text.
func TestSemanticPrometheusPVLabelsFollowResourceKindAndSlot(t *testing.T) {
	type rendered struct {
		label string
		ok    bool
	}
	for _, tc := range []struct {
		name, item, fact, dimension, value string
		unit                               semreg.DefinitionID
		resources                          []semreg.Resource
		want                               rendered
	}{
		{"inverter", "projection.gateway.pv.inverter.ac.power.active", "pv.ac.aggregate_active_power", "pv.dimension.inverter", "resource:test-inverter", "unit.watt", []semreg.Resource{semanticResourceFixture("resource:test-inverter", "resource.inverter", "")}, rendered{"inverter", true}},
		{"system", "projection.gateway.pv.inverter.ac.energy_lifetime", "pv.energy.generated", "pv.dimension.system", "resource:test-system", "unit.joule", []semreg.Resource{semanticResourceFixture("resource:test-system", "resource.pv_system", "")}, rendered{"system", true}},
		{"phase slot", "projection.gateway.pv.inverter.ac.current.phase_b", "pv.ac.current", "pv.dimension.phase", "resource:test-phase", "unit.ampere", []semreg.Resource{semanticResourceFixture("resource:test-phase", "resource.phase", "L2")}, rendered{"phase_l2", true}},
		{"phase of another slot", "projection.gateway.pv.inverter.ac.current.phase_b", "pv.ac.current", "pv.dimension.phase", "resource:test-phase", "unit.ampere", []semreg.Resource{semanticResourceFixture("resource:test-phase", "resource.phase", "L3")}, rendered{}},
		{"names no resource", "projection.gateway.pv.inverter.ac.power.active", "pv.ac.aggregate_active_power", "pv.dimension.inverter", "resource:test-missing", "unit.watt", []semreg.Resource{semanticResourceFixture("resource:test-inverter", "resource.inverter", "")}, rendered{}},
		{"resource of another kind", "projection.gateway.pv.inverter.ac.power.active", "pv.ac.aggregate_active_power", "pv.dimension.inverter", "resource:test-input", "unit.watt", []semreg.Resource{semanticResourceFixture("resource:test-input", "resource.dc_input", "1")}, rendered{}},
		{"ungraphed literal text", "projection.gateway.pv.inverter.ac.power.active", "pv.ac.aggregate_active_power", "pv.dimension.inverter", "inverter", "unit.watt", nil, rendered{"inverter", true}},
		{"ungraphed role text", "projection.gateway.pv.inverter.ac.power.active", "pv.ac.aggregate_active_power", "pv.dimension.inverter", "inverter:subject.private", "unit.watt", nil, rendered{"inverter", true}},
		{"ungraphed published subject", "projection.gateway.pv.inverter.ac.power.active", "pv.ac.aggregate_active_power", "pv.dimension.inverter", "system:subject.private", "unit.watt", nil, rendered{"inverter", true}},
		{"graphed role text", "projection.gateway.pv.inverter.ac.power.active", "pv.ac.aggregate_active_power", "pv.dimension.inverter", "inverter:asset:private", "unit.watt", []semreg.Resource{semanticResourceFixture("resource:test-inverter", "resource.inverter", "")}, rendered{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			domain := semanticMetricsFixture("pv", true, semreg.FreshnessFresh, semreg.AvailabilityAvailable, semreg.ValidityGood, 0)
			semanticPVFixtureAs(&domain, tc.item, tc.fact, tc.dimension, tc.value, tc.unit)
			domain.Snapshot.Resources = tc.resources
			var out bytes.Buffer
			writeSemanticMetrics(newPrometheusWriter(&out), []SemanticMetricsDomain{domain}, time.Unix(101, 0))
			metrics := out.String()
			if len(tc.resources) > 0 && strings.Contains(metrics, "asset:") {
				t.Fatalf("an asset identifier leaked into a label:\n%s", metrics)
			}
			if !tc.want.ok && strings.Contains(metrics, tc.value) {
				t.Fatalf("an unresolved subject was labeled:\n%s", metrics)
			}
			kind := ""
			resource := ""
			if tc.want.ok {
				resource = `,resource="` + tc.value + `"`
				if len(tc.resources) > 0 {
					kind = string(tc.resources[0].Kind)
				} else {
					kind = semanticUngraphedKind(tc.want.label)
					if tc.want.label == "phase_l1" || tc.want.label == "phase_l2" || tc.want.label == "phase_l3" {
						kind = "resource.phase"
					}
				}
			}
			tail := ""
			if tc.want.ok {
				tail = resource + `,resource_kind="` + kind + `"`
			}
			want := `helianthus_semantic_fact_value{dimension="` + tc.want.label + `",fact="` + tc.fact + `",pack="helianthus.pack.pv"` + tail + `,unit="` + string(tc.unit) + `"} 42`
			if tc.want.ok != strings.Contains(metrics, want) {
				t.Fatalf("rendered=%v, want %v (%s):\n%s", !tc.want.ok, tc.want.ok, want, metrics)
			}
			if !tc.want.ok && (strings.Contains(metrics, "\nhelianthus_semantic_fact_value{") || !strings.Contains(metrics, "helianthus_semantic_render_overflow 1")) {
				t.Fatalf("an unresolved subject was rendered or not counted:\n%s", metrics)
			}
		})
	}
	t.Run("withdrawn resource", func(t *testing.T) {
		domain := semanticMetricsFixture("pv", true, semreg.FreshnessFresh, semreg.AvailabilityAvailable, semreg.ValidityGood, 0)
		domain.Snapshot.Resources[0].State = semreg.ResourceWithdrawn
		var out bytes.Buffer
		writeSemanticMetrics(newPrometheusWriter(&out), []SemanticMetricsDomain{domain}, time.Unix(101, 0))
		if strings.Contains(out.String(), "helianthus_semantic_fact_value{") {
			t.Fatalf("a fact about a withdrawn resource was rendered:\n%s", out.String())
		}
	})
}

// TestSemanticPrometheusStorageLabelFollowsResourceKind: the storage pack
// label is taken from the subject resource's kind, never from the pack value,
// which is an identifier. A pack value naming a current battery pack renders
// the label "pack"; one naming no resource, a resource of another kind or a
// withdrawn pack renders no fact, and so does a pack value in a snapshot
// without resources. No identifier reaches a label.
func TestSemanticPrometheusStorageLabelFollowsResourceKind(t *testing.T) {
	const value = `helianthus_semantic_fact_value{dimension="pack",fact="storage.pack.voltage",pack="helianthus.pack.storage",resource="resource:pack.synthetic",resource_kind="resource.battery_pack",unit="unit.volt"} 42`
	pack := semanticResourceFixture("resource:pack.synthetic", "resource.battery_pack", "")
	withdrawn := pack
	withdrawn.State = semreg.ResourceWithdrawn
	for _, tc := range []struct {
		name      string
		resources []semreg.Resource
		rendered  bool
	}{
		{"battery pack", []semreg.Resource{pack}, true},
		{"names no resource", []semreg.Resource{semanticResourceFixture("resource:other", "resource.battery_pack", "")}, false},
		{"resource of another kind", []semreg.Resource{semanticResourceFixture("resource:pack.synthetic", "resource.inverter", "")}, false},
		{"withdrawn pack", []semreg.Resource{withdrawn}, false},
		{"asset-valued text without resources", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			domain := semanticMetricsFixture("storage", true, semreg.FreshnessFresh, semreg.AvailabilityAvailable, semreg.ValidityGood, 0)
			domain.Snapshot.Resources = tc.resources
			var out bytes.Buffer
			writeSemanticMetrics(newPrometheusWriter(&out), []SemanticMetricsDomain{domain}, time.Unix(101, 0))
			metrics := out.String()
			if strings.Contains(metrics, "asset:") || (!tc.rendered && strings.Contains(metrics, "resource:")) {
				t.Fatalf("an identifier leaked into a label:\n%s", metrics)
			}
			if tc.rendered != strings.Contains(metrics, value) {
				t.Fatalf("rendered=%v, want %v:\n%s", !tc.rendered, tc.rendered, metrics)
			}
			if !tc.rendered && (strings.Contains(metrics, "\nhelianthus_semantic_fact_value{") || !strings.Contains(metrics, "helianthus_semantic_render_overflow 1")) {
				t.Fatalf("an unresolved pack was rendered or not counted:\n%s", metrics)
			}
		})
	}
}

func addEVSEAllocatedFixture(domain *SemanticMetricsDomain, connector string, candidateID semreg.CandidateID) {
	value := semreg.Value{Kind: semreg.ValueQuantity, Quantity: &semreg.Quantity{Number: semreg.Decimal{Coefficient: "16"}, Unit: "unit.ampere"}}
	key := semreg.FactKey{PackID: "helianthus.pack.evse", PackVersion: "1.0.0", FactID: "evse.limit.allocated_current", Dimensions: []semreg.Dimension{{ID: "evse.dimension.connector", Value: semreg.Value{Kind: semreg.ValueText, Text: &connector}}}}
	candidate := semreg.FactCandidate{CandidateID: candidateID, Key: key, Value: &value, Quality: semreg.Quality{Qualification: semreg.QualificationQualified, Promotion: semreg.PromotionPromoted, Validity: semreg.ValidityGood}, Times: semreg.Times{ReceivedAt: semreg.TimePoint{UnixNanoseconds: "100000000000", ClockID: "clock.utc"}}}
	domain.Snapshot.Facts = append(domain.Snapshot.Facts, semreg.FactEnvelope{Key: key, Candidates: []semreg.FactCandidate{candidate}})
	domain.Evaluation.Facts = append(domain.Evaluation.Facts, semreg.EvaluatedFact{CandidateID: candidateID, Freshness: semreg.FreshnessFresh, EffectiveAvailability: semreg.AvailabilityAvailable})
	domain.Projection.Dispositions = append(domain.Projection.Dispositions, projection.ProjectionDisposition{ItemID: "evse.limit.allocated_current", Outcome: projection.ProjectionExact, SourceKeys: []semreg.FactKey{key}})
}

// addSemanticSourceCandidate attributes the fixture's candidate to first and
// adds a second candidate of the same key observed by second. It sets the
// envelope's conflict when the coefficients differ.
func addSemanticSourceCandidate(domain *SemanticMetricsDomain, first, second semreg.SourceID, coefficient string, receivedSeconds int, freshness semreg.Freshness) {
	envelope := &domain.Snapshot.Facts[0]
	original := &envelope.Candidates[0]
	original.Origin = semreg.OriginRef{OriginID: "origin:first", Kind: semreg.OriginNativeObservation, SourceID: &first}
	candidate := *original
	candidate.CandidateID = "candidate:test-second"
	value := semreg.Value{Kind: semreg.ValueQuantity, Quantity: &semreg.Quantity{Number: semreg.Decimal{Coefficient: coefficient}, Unit: original.Value.Quantity.Unit}}
	candidate.Value = &value
	candidate.Origin = semreg.OriginRef{OriginID: "origin:second", Kind: semreg.OriginNativeObservation, SourceID: &second}
	candidate.Times.ReceivedAt = semreg.TimePoint{UnixNanoseconds: semreg.Int64(strconv.Itoa(receivedSeconds) + "000000000"), ClockID: "clock.utc"}
	envelope.Candidates = append(envelope.Candidates, candidate)
	if coefficient != original.Value.Quantity.Number.Coefficient {
		envelope.Conflicts = append(envelope.Conflicts, semreg.Conflict{ConflictID: "conflict:test", Kind: semreg.ConflictValue, Candidates: []semreg.CandidateID{original.CandidateID, candidate.CandidateID}, State: semreg.ConflictOpen})
	}
	domain.Evaluation.Facts = append(domain.Evaluation.Facts, semreg.EvaluatedFact{CandidateID: candidate.CandidateID, Freshness: freshness, EffectiveAvailability: semreg.AvailabilityAvailable})
}

// TestSemanticPrometheusTwoCandidatesExportSelected: a key with candidates of
// two sources exports the candidate the presentation policy selects instead of
// dropping the fact; a conflict still withholds the value.
func TestSemanticPrometheusTwoCandidatesExportSelected(t *testing.T) {
	const value = `helianthus_semantic_fact_value{dimension="inverter",fact="pv.ac.aggregate_active_power",pack="helianthus.pack.pv",resource="resource:inverter.synthetic",resource_kind="resource.inverter",unit="unit.watt"} `
	const age = `helianthus_semantic_evidence_age{dimension="inverter",fact="pv.ac.aggregate_active_power",pack="helianthus.pack.pv",resource="resource:inverter.synthetic",resource_kind="resource.inverter",unit="seconds"} `
	const conflicts = `helianthus_semantic_open_conflicts{dimension="inverter",fact="pv.ac.aggregate_active_power",pack="helianthus.pack.pv",resource="resource:inverter.synthetic",resource_kind="resource.inverter"} `
	for _, tc := range []struct {
		name        string
		owner       semreg.SourceID
		coefficient string
		freshness   semreg.Freshness
		value       string // empty: no value is exported
		age         string // empty: no evidence age is exported
		conflicts   string // empty: no conflict gauge is exported
	}{
		// The second candidate is stale, so the first is the single qualified one.
		{name: "single qualified", coefficient: "42", freshness: semreg.FreshnessStale, value: "42", age: "1", conflicts: "0"},
		// Two qualified candidates and no owner: nothing is selected.
		{name: "two qualified without owner", coefficient: "42", freshness: semreg.FreshnessFresh, conflicts: "0"},
		// The owner's candidate is selected, whichever identifier sorts first:
		// the evidence age tells the two candidates apart.
		{name: "owner first", owner: "source:first", coefficient: "42", freshness: semreg.FreshnessFresh, value: "42", age: "1", conflicts: "0"},
		{name: "owner second", owner: "source:second", coefficient: "42", freshness: semreg.FreshnessFresh, value: "42", age: "10", conflicts: "0"},
		// An owner that observed neither candidate selects nothing.
		{name: "owner absent", owner: "source:third", coefficient: "42", freshness: semreg.FreshnessFresh, conflicts: "0"},
		// The owner's candidate is stale: the other one is the single qualified.
		{name: "owner stale", owner: "source:second", coefficient: "42", freshness: semreg.FreshnessStale, value: "42", age: "1", conflicts: "0"},
		// A conflict keeps the owner's state and age but never a value.
		{name: "owner under conflict", owner: "source:second", coefficient: "43", freshness: semreg.FreshnessFresh, age: "10", conflicts: "1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for run := 0; run < 16; run++ {
				domain := semanticMetricsFixture("pv", true, semreg.FreshnessFresh, semreg.AvailabilityAvailable, semreg.ValidityGood, 0)
				addSemanticSourceCandidate(&domain, "source:first", "source:second", tc.coefficient, 91, tc.freshness)
				domain.Owner = tc.owner
				var out bytes.Buffer
				writeSemanticMetrics(newPrometheusWriter(&out), []SemanticMetricsDomain{domain}, time.Unix(101, 0))
				metrics := out.String()
				for _, expect := range []struct{ prefix, want string }{{value, tc.value}, {age, tc.age}, {conflicts, tc.conflicts}} {
					if expect.want == "" {
						if strings.Contains(metrics, expect.prefix) {
							t.Fatalf("unexpected %q:\n%s", expect.prefix, metrics)
						}
						continue
					}
					if !strings.Contains(metrics, expect.prefix+expect.want+"\n") {
						t.Fatalf("missing %q%s:\n%s", expect.prefix, expect.want, metrics)
					}
				}
				if strings.Contains(metrics, "\nhelianthus_semantic_render_overflow ") {
					t.Fatalf("two candidates overflowed:\n%s", metrics)
				}
			}
		})
	}
}

// TestSemanticPrometheusEVSEConnectorSlotTwoCandidates: a connector whose
// allocated current has candidates of two sources receives a slot and exports
// the owner's candidate; without an owner neither candidate is selected, so
// the connector receives no slot and nothing is exported.
func TestSemanticPrometheusEVSEConnectorSlotTwoCandidates(t *testing.T) {
	const value = `helianthus_semantic_fact_value{connector="resource:connector.shared",dimension="connector",fact="evse.limit.allocated_current",pack="helianthus.pack.evse",resource="resource:connector.shared",resource_kind="resource.connector",unit="unit.ampere"} 16`
	for _, tc := range []struct {
		owner  semreg.SourceID
		export bool
	}{{"source:owner", true}, {"", false}} {
		domain := semanticMetricsFixture("evse", true, semreg.FreshnessFresh, semreg.AvailabilityAvailable, semreg.ValidityGood, 0)
		domain.Snapshot.Resources = []semreg.Resource{semanticResourceFixture("resource:connector.shared", "resource.connector", "")}
		addEVSEAllocatedFixture(&domain, "resource:connector.shared", "candidate:allocated")
		envelope := &domain.Snapshot.Facts[len(domain.Snapshot.Facts)-1]
		owner, contributor := semreg.SourceID("source:owner"), semreg.SourceID("source:contributor")
		envelope.Candidates[0].Origin = semreg.OriginRef{OriginID: "origin:owner", Kind: semreg.OriginNativeObservation, SourceID: &owner}
		second := envelope.Candidates[0]
		second.CandidateID = "candidate:allocated-contributor"
		second.Origin = semreg.OriginRef{OriginID: "origin:contributor", Kind: semreg.OriginNativeObservation, SourceID: &contributor}
		envelope.Candidates = append(envelope.Candidates, second)
		domain.Evaluation.Facts = append(domain.Evaluation.Facts, semreg.EvaluatedFact{CandidateID: second.CandidateID, Freshness: semreg.FreshnessFresh, EffectiveAvailability: semreg.AvailabilityAvailable})
		domain.Owner = tc.owner
		var out bytes.Buffer
		writeSemanticMetrics(newPrometheusWriter(&out), []SemanticMetricsDomain{domain}, time.Unix(101, 0))
		metrics := out.String()
		if strings.Contains(metrics, value) != tc.export {
			t.Fatalf("owner %q: connector value exported %v want %v:\n%s", tc.owner, !tc.export, tc.export, metrics)
		}
	}
}

// semanticStorageSeries appends one storage fact. The resource identifier,
// the fact definition and the qualifier text are the value key. A later call
// does not alias an earlier qualifier: each text has its own variable.
func semanticStorageSeries(domain *SemanticMetricsDomain, resource, fact, coefficient string, unit semreg.DefinitionID, candidate semreg.CandidateID) {
	text := resource
	key := semreg.FactKey{PackID: "helianthus.pack.storage", PackVersion: "1.1.0", FactID: semreg.DefinitionID(fact), Dimensions: []semreg.Dimension{{ID: "storage.dimension.pack", Value: semreg.Value{Kind: semreg.ValueText, Text: &text}}}}
	value := semreg.Value{Kind: semreg.ValueQuantity, Quantity: &semreg.Quantity{Number: semreg.Decimal{Coefficient: coefficient}, Unit: unit}}
	candidateValue := semreg.FactCandidate{CandidateID: candidate, Key: key, Value: &value, Quality: semreg.Quality{Qualification: semreg.QualificationQualified, Promotion: semreg.PromotionPromoted, Validity: semreg.ValidityGood}, Times: semreg.Times{ReceivedAt: semreg.TimePoint{UnixNanoseconds: "100000000000", ClockID: "clock.utc"}}}
	domain.Snapshot.Facts = append(domain.Snapshot.Facts, semreg.FactEnvelope{Key: key, Candidates: []semreg.FactCandidate{candidateValue}})
	domain.Evaluation.Facts = append(domain.Evaluation.Facts, semreg.EvaluatedFact{CandidateID: candidate, Freshness: semreg.FreshnessFresh, EffectiveAvailability: semreg.AvailabilityAvailable})
	domain.Projection.Dispositions = append(domain.Projection.Dispositions, projection.ProjectionDisposition{ItemID: semreg.DefinitionID(fact), Outcome: projection.ProjectionExact, SourceKeys: []semreg.FactKey{key}})
}

// TestSemanticPrometheusTwoAssetsOfOnePack varies every field of the value
// key. Swapping the resource identifier moves 11 onto the other series.
// Using the soc fact on the voltage series, or the voltage fact on the ratio
// series, removes the line this test requires. A percent is exported as a
// ratio; an ampere-hour is not rewritten as a coulomb.
func TestSemanticPrometheusTwoAssetsOfOnePack(t *testing.T) {
	domain := semanticMetricsFixture("storage", true, semreg.FreshnessFresh, semreg.AvailabilityAvailable, semreg.ValidityGood, 0)
	domain.Snapshot.Facts = nil
	domain.Evaluation.Facts = nil
	domain.Projection.Dispositions = nil
	domain.Snapshot.Resources = []semreg.Resource{
		semanticResourceFixture("resource:pack.alpha", "resource.battery_pack", ""),
		semanticResourceFixture("resource:pack.beta", "resource.battery_pack", ""),
	}
	semanticStorageSeries(&domain, "resource:pack.alpha", "storage.pack.voltage", "11", "unit.volt", "candidate:alpha-voltage")
	semanticStorageSeries(&domain, "resource:pack.beta", "storage.pack.voltage", "22", "unit.volt", "candidate:beta-voltage")
	semanticStorageSeries(&domain, "resource:pack.alpha", "storage.state.soc", "75", "unit.percent", "candidate:alpha-soc")
	semanticStorageSeries(&domain, "resource:pack.alpha", "storage.capacity.charge", "40", "unit.ampere_hour", "candidate:alpha-capacity")
	var out bytes.Buffer
	writeSemanticMetrics(newPrometheusWriter(&out), []SemanticMetricsDomain{domain}, time.Unix(101, 0))
	metrics := out.String()
	for _, line := range []string{
		`helianthus_semantic_fact_value{dimension="pack",fact="storage.pack.voltage",pack="helianthus.pack.storage",resource="resource:pack.alpha",resource_kind="resource.battery_pack",unit="unit.volt"} 11`,
		`helianthus_semantic_fact_value{dimension="pack",fact="storage.pack.voltage",pack="helianthus.pack.storage",resource="resource:pack.beta",resource_kind="resource.battery_pack",unit="unit.volt"} 22`,
		`helianthus_semantic_fact_value{dimension="pack",fact="storage.state.soc",pack="helianthus.pack.storage",resource="resource:pack.alpha",resource_kind="resource.battery_pack",unit="ratio"} 0.75`,
		`helianthus_semantic_fact_value{dimension="pack",fact="storage.capacity.charge",pack="helianthus.pack.storage",resource="resource:pack.alpha",resource_kind="resource.battery_pack",unit="unit.ampere_hour"} 40`,
		`helianthus_semantic_projection_available{domain="storage"} 1`,
	} {
		if !strings.Contains(metrics, line) {
			t.Fatalf("missing %s\n%s", line, metrics)
		}
	}
	if strings.Contains(metrics, "unit.percent") || strings.Contains(metrics, "coulomb") || strings.Contains(metrics, "fact_id=") || strings.Contains(metrics, "domain=\"storage\",fact=") {
		t.Fatalf("unit or key drifted:\n%s", metrics)
	}
}

// TestSemanticPrometheusReplacementAndSupersession: an anchor change on the
// same resource keeps the series bytes. Replacing the equipment resource
// withdraws the old series and exports the new resource.
func TestSemanticPrometheusReplacementAndSupersession(t *testing.T) {
	domain := semanticMetricsFixture("storage", true, semreg.FreshnessFresh, semreg.AvailabilityAvailable, semreg.ValidityGood, 0)
	render := func() string {
		t.Helper()
		var out bytes.Buffer
		writeSemanticMetrics(newPrometheusWriter(&out), []SemanticMetricsDomain{domain}, time.Unix(101, 0))
		return out.String()
	}
	before := render()
	const kept = `helianthus_semantic_fact_value{dimension="pack",fact="storage.pack.voltage",pack="helianthus.pack.storage",resource="resource:pack.synthetic",resource_kind="resource.battery_pack",unit="unit.volt"} 42`
	if !strings.Contains(before, kept) {
		t.Fatal(before)
	}
	slot := "successor"
	domain.Snapshot.Resources[0].Revision = "2"
	domain.Snapshot.Resources[0].Anchor.Slot = &slot
	if render() != before {
		t.Fatal("anchor supersession changed the series")
	}
	domain.Snapshot.Resources[0].State = semreg.ResourceWithdrawn
	domain.Snapshot.Resources = append(domain.Snapshot.Resources, semanticResourceFixture("resource:pack.replacement", "resource.battery_pack", ""))
	text := "resource:pack.replacement"
	domain.Snapshot.Facts[0].Key.Dimensions[0].Value.Text = &text
	domain.Snapshot.Facts[0].Candidates[0].Key = domain.Snapshot.Facts[0].Key
	domain.Projection.Dispositions[0].SourceKeys[0] = domain.Snapshot.Facts[0].Key
	replaced := render()
	if strings.Contains(replaced, "resource:pack.synthetic") || !strings.Contains(replaced, `helianthus_semantic_fact_value{dimension="pack",fact="storage.pack.voltage",pack="helianthus.pack.storage",resource="resource:pack.replacement",resource_kind="resource.battery_pack",unit="unit.volt"} 42`) {
		t.Fatalf("replacement series:\n%s", replaced)
	}
}

// TestSemanticPrometheusRekeyKeepsProjectionSeries: on one fixture the
// projection series keeps its domain label and the per-fact value keeps its
// magnitude under the resource key. The old domain and fact_id labels are
// not emitted.
func TestSemanticPrometheusRekeyKeepsProjectionSeries(t *testing.T) {
	domain := semanticMetricsFixture("pv", true, semreg.FreshnessFresh, semreg.AvailabilityAvailable, semreg.ValidityGood, 0)
	var out bytes.Buffer
	writeSemanticMetrics(newPrometheusWriter(&out), []SemanticMetricsDomain{domain}, time.Unix(101, 0))
	metrics := out.String()
	const projection = `helianthus_semantic_projection_available{domain="pv"} 1`
	const value = `helianthus_semantic_fact_value{dimension="inverter",fact="pv.ac.aggregate_active_power",pack="helianthus.pack.pv",resource="resource:inverter.synthetic",resource_kind="resource.inverter",unit="unit.watt"} 42`
	if !strings.Contains(metrics, projection) || !strings.Contains(metrics, value) {
		t.Fatal(metrics)
	}
	for _, line := range strings.Split(metrics, "\n") {
		if strings.HasPrefix(line, "helianthus_semantic_fact_") || strings.HasPrefix(line, "helianthus_semantic_evidence_age{") || strings.HasPrefix(line, "helianthus_semantic_open_conflicts{") {
			if strings.Contains(line, "domain=") || strings.Contains(line, "fact_id=") {
				t.Fatal(line)
			}
		}
	}
}

// TestSemanticPrometheusUngraphedPublishedSubjectsStayDistinct: two assets of
// one fact keep both values, distinguished by the published subject.
func TestSemanticPrometheusUngraphedPublishedSubjectsStayDistinct(t *testing.T) {
	first := semanticMetricsFixture("pv", true, semreg.FreshnessFresh, semreg.AvailabilityAvailable, semreg.ValidityGood, 0)
	second := semanticMetricsFixture("pv", true, semreg.FreshnessFresh, semreg.AvailabilityAvailable, semreg.ValidityGood, 0)
	first.Snapshot.Resources, second.Snapshot.Resources = nil, nil
	semanticPVFixtureAs(&first, "projection.gateway.pv.inverter.ac.power.active", "pv.ac.aggregate_active_power", "pv.dimension.inverter", "inverter:subject-a", "unit.watt")
	semanticPVFixtureAs(&second, "projection.gateway.pv.inverter.ac.power.active", "pv.ac.aggregate_active_power", "pv.dimension.inverter", "inverter:subject-b", "unit.watt")
	first.Snapshot.Facts[0].Candidates[0].Value.Quantity.Number.Coefficient = "11"
	second.Snapshot.Facts[0].Candidates[0].Value.Quantity.Number.Coefficient = "22"
	var out bytes.Buffer
	writeSemanticMetrics(newPrometheusWriter(&out), []SemanticMetricsDomain{first, second}, time.Unix(101, 0))
	metrics := out.String()
	for _, line := range []string{
		`helianthus_semantic_fact_value{dimension="inverter",fact="pv.ac.aggregate_active_power",pack="helianthus.pack.pv",resource="inverter:subject-a",resource_kind="resource.inverter",unit="unit.watt"} 11`,
		`helianthus_semantic_fact_value{dimension="inverter",fact="pv.ac.aggregate_active_power",pack="helianthus.pack.pv",resource="inverter:subject-b",resource_kind="resource.inverter",unit="unit.watt"} 22`,
		`helianthus_semantic_projection_item{domain="pv",item="projection.gateway.pv.inverter.ac.power.active",outcome="exact"} 1`,
	} {
		if !strings.Contains(metrics, line) {
			t.Fatalf("missing %s\n%s", line, metrics)
		}
	}
	if strings.Count(metrics, "\nhelianthus_semantic_fact_value{") != 2 {
		t.Fatalf("subjects collapsed:\n%s", metrics)
	}
}

// TestSemanticPrometheusExportsEveryAcceptedPublication: a publication outside
// the former domain list is exported, and a series past the former ceiling is
// exported with it.
func TestSemanticPrometheusExportsEveryAcceptedPublication(t *testing.T) {
	domain := semanticMetricsFixture("pv", true, semreg.FreshnessFresh, semreg.AvailabilityAvailable, semreg.ValidityGood, 0)
	text := "meter-a"
	key := domain.Snapshot.Facts[0].Key
	key.PackID = "helianthus.pack.infrastructure"
	key.FactID = "infrastructure.energy.import"
	key.Dimensions = []semreg.Dimension{{ID: "infrastructure.dimension.meter", Value: semreg.Value{Kind: semreg.ValueText, Text: &text}}}
	domain.Name = "infrastructure"
	domain.Snapshot.Resources = nil
	domain.Snapshot.Facts[0].Key = key
	domain.Snapshot.Facts[0].Candidates[0].Key = key
	domain.Snapshot.Facts[0].Candidates[0].Value.Quantity.Unit = "unit.joule"
	domain.Projection = projection.ProjectionReport{}
	domains := []SemanticMetricsDomain{domain}
	for i := 0; i < 193; i++ {
		domains = append(domains, SemanticMetricsDomain{Name: "plant-" + strconv.Itoa(i)})
	}
	var out bytes.Buffer
	writeSemanticMetrics(newPrometheusWriter(&out), domains, time.Unix(101, 0))
	metrics := out.String()
	const value = `helianthus_semantic_fact_value{dimension="infrastructure.dimension.meter",fact="infrastructure.energy.import",pack="helianthus.pack.infrastructure",resource="meter-a",unit="unit.joule"} 42`
	if !strings.Contains(metrics, value) {
		t.Fatalf("infrastructure value withheld:\n%s", metrics)
	}
	if strings.Count(metrics, "helianthus_semantic_projection_available{") != 193 {
		t.Fatalf("projection series = %d\n%s", strings.Count(metrics, "helianthus_semantic_projection_available{"), metrics)
	}
	if strings.Contains(metrics, "\nhelianthus_semantic_render_overflow ") || strings.Contains(metrics, `domain="infrastructure"`) {
		t.Fatalf("a series was withheld or a missing projection was marked available:\n%s", metrics)
	}
	if !strings.Contains(metrics, `helianthus_semantic_projection_available{domain="plant-192"} 0`) {
		t.Fatalf("domain withheld:\n%s", metrics)
	}
}

func semanticPublishedFixture() SemanticMetricsDomain {
	domain := semanticMetricsFixture("cut", true, semreg.FreshnessFresh, semreg.AvailabilityAvailable, semreg.ValidityGood, 0)
	domain.Snapshot.Facts, domain.Evaluation.Facts = nil, nil
	domain.Projection = projection.ProjectionReport{}
	return domain
}

func semanticTextQualifier(id semreg.DefinitionID, text string) semreg.Dimension {
	return semreg.Dimension{ID: id, Value: semreg.Value{Kind: semreg.ValueText, Text: &text}}
}

func addSemanticPublishedQuantity(domain *SemanticMetricsDomain, pack, fact, unit semreg.DefinitionID, number semreg.Decimal, dimensions ...semreg.Dimension) {
	key := semreg.FactKey{PackID: pack, PackVersion: "1.0.0", FactID: fact, Dimensions: dimensions}
	id := semreg.CandidateID("candidate:synthetic-" + strconv.Itoa(len(domain.Snapshot.Facts)))
	candidate := semreg.FactCandidate{
		CandidateID: id, Key: key,
		Value:   &semreg.Value{Kind: semreg.ValueQuantity, Quantity: &semreg.Quantity{Number: number, Unit: unit}},
		Quality: semreg.Quality{Qualification: semreg.QualificationQualified, Promotion: semreg.PromotionPromoted, Validity: semreg.ValidityGood},
		Times:   semreg.Times{ReceivedAt: semreg.TimePoint{UnixNanoseconds: "100000000000", ClockID: "clock.utc"}},
	}
	domain.Snapshot.Facts = append(domain.Snapshot.Facts, semreg.FactEnvelope{Key: key, Candidates: []semreg.FactCandidate{candidate}})
	domain.Evaluation.Facts = append(domain.Evaluation.Facts, semreg.EvaluatedFact{CandidateID: id, Freshness: semreg.FreshnessFresh, EffectiveAvailability: semreg.AvailabilityAvailable})
}

func TestSemanticPrometheusRegistryFlowRate(t *testing.T) {
	domain := semanticPublishedFixture()
	domain.Snapshot.Resources = []semreg.Resource{semanticResourceFixture("resource:circuit.synthetic", "resource.thermal_circuit", "")}
	addSemanticPublishedQuantity(&domain, "helianthus.pack.thermal", "thermal.measurement.flow_rate", "unit.cubic_metre_per_second", semreg.Decimal{Coefficient: "25", Exponent10: -4}, semanticTextQualifier("thermal.dimension.circuit", "resource:circuit.synthetic"))
	got := RenderSemanticPrometheus([]SemanticMetricsDomain{domain}, time.Unix(101, 0))
	want := `helianthus_semantic_fact_state{availability="available",dimension="thermal.dimension.circuit",fact="thermal.measurement.flow_rate",freshness="fresh",pack="helianthus.pack.thermal",promotion="promoted",qualification="qualified",quality="good",resource="resource:circuit.synthetic",resource_kind="resource.thermal_circuit"} 1
helianthus_semantic_open_conflicts{dimension="thermal.dimension.circuit",fact="thermal.measurement.flow_rate",pack="helianthus.pack.thermal",resource="resource:circuit.synthetic",resource_kind="resource.thermal_circuit"} 0
helianthus_semantic_evidence_age{dimension="thermal.dimension.circuit",fact="thermal.measurement.flow_rate",pack="helianthus.pack.thermal",resource="resource:circuit.synthetic",resource_kind="resource.thermal_circuit",unit="seconds"} 1
helianthus_semantic_fact_value{dimension="thermal.dimension.circuit",fact="thermal.measurement.flow_rate",pack="helianthus.pack.thermal",resource="resource:circuit.synthetic",resource_kind="resource.thermal_circuit",unit="unit.cubic_metre_per_second"} 0.0025
`
	if got != semanticPrometheusFixtureMetadata+want {
		t.Fatalf("flow rate exposition:\n%s\nwant samples:\n%s", got, want)
	}
}

func TestSemanticPrometheusRegistryPowerUnits(t *testing.T) {
	for _, tc := range []struct {
		name        string
		fact, unit  semreg.DefinitionID
		coefficient string
		pack        semreg.DefinitionID
		version     semreg.SemanticVersion
		want        bool
	}{
		{"apparent", "infrastructure.ac.apparent_power", "unit.volt_ampere", "120", "helianthus.pack.infrastructure", "1.0.0", true},
		{"reactive", "infrastructure.ac.reactive_power", "unit.volt_ampere_reactive", "-35", "helianthus.pack.infrastructure", "1.0.0", true},
		{"wrong unit", "infrastructure.ac.apparent_power", "unit.watt", "120", "helianthus.pack.infrastructure", "1.0.0", false},
		{"percent on non-ratio fact", "infrastructure.ac.apparent_power", "unit.percent", "75", "helianthus.pack.infrastructure", "1.0.0", false},
		{"wrong pack", "infrastructure.ac.apparent_power", "unit.volt_ampere", "120", "helianthus.pack.thermal", "1.0.0", false},
		{"wrong version", "infrastructure.ac.apparent_power", "unit.volt_ampere", "120", "helianthus.pack.infrastructure", "2.0.0", false},
		{"unknown fact", "infrastructure.ac.synthetic", "unit.watt", "120", "helianthus.pack.infrastructure", "1.0.0", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			domain := semanticPublishedFixture()
			domain.Snapshot.Resources = []semreg.Resource{semanticResourceFixture("resource:phase.synthetic", "resource.phase", "L1")}
			addSemanticPublishedQuantity(&domain, tc.pack, tc.fact, tc.unit, semreg.Decimal{Coefficient: tc.coefficient}, semanticTextQualifier("infrastructure.dimension.phase", "resource:phase.synthetic"))
			env := &domain.Snapshot.Facts[0]
			env.Key.PackVersion = tc.version
			env.Candidates[0].Key = env.Key
			got := RenderSemanticPrometheus([]SemanticMetricsDomain{domain}, time.Unix(101, 0))
			for _, family := range []string{"fact_state", "open_conflicts", "evidence_age"} {
				if strings.Count(got, "\nhelianthus_semantic_"+family+"{") != 1 {
					t.Fatalf("missing %s:\n%s", family, got)
				}
			}
			const value = "\nhelianthus_semantic_fact_value{"
			if !tc.want {
				if strings.Contains(got, value) {
					t.Fatalf("noncanonical value emitted:\n%s", got)
				}
				return
			}
			want := value + `dimension="infrastructure.dimension.phase",fact="` + string(tc.fact) + `",pack="helianthus.pack.infrastructure",resource="resource:phase.synthetic",resource_kind="resource.phase",unit="` + string(tc.unit) + `"} ` + tc.coefficient + "\n"
			if strings.Count(got, value) != 1 || !strings.Contains(got, want) {
				t.Fatalf("power exposition:\n%s\nmissing %s", got, want)
			}
		})
	}
}

const semanticPrometheusFixtureMetadata = `# HELP helianthus_semantic_projection_available Detached SemReg projection availability for this Prometheus target.
# TYPE helianthus_semantic_projection_available gauge
# HELP helianthus_semantic_projection_item Current requested SemReg projection outcome.
# TYPE helianthus_semantic_projection_item gauge
# HELP helianthus_semantic_fact_state Current SemReg fact quality, effective availability, and freshness state.
# TYPE helianthus_semantic_fact_state gauge
# HELP helianthus_semantic_fact_value Current fresh qualified numeric or boolean SemReg fact value.
# TYPE helianthus_semantic_fact_value gauge
# HELP helianthus_semantic_evidence_age Age of selected SemReg evidence when wall clocks are comparable.
# TYPE helianthus_semantic_evidence_age gauge
# HELP helianthus_semantic_open_conflicts Open conflicts on the detached SemReg fact envelope.
# TYPE helianthus_semantic_open_conflicts gauge
# HELP helianthus_semantic_projection_loss Loss kinds declared by the detached SemReg projection.
# TYPE helianthus_semantic_projection_loss gauge
# HELP helianthus_semantic_render_overflow Semantic metrics omitted because input was invalid or a duplicate series was withheld.
# TYPE helianthus_semantic_render_overflow gauge
`

func TestSemanticPrometheusCompleteQualifierExposition(t *testing.T) {
	domain := semanticMetricsFixture("pv", true, semreg.FreshnessFresh, semreg.AvailabilityAvailable, semreg.ValidityGood, 0)
	domain.Projection.Dispositions[0].Outcome = projection.ProjectionTransformed
	domain.Projection.Dispositions[0].Loss = []projection.LossDetail{{Kind: projection.LossPrecision}}
	domain.Snapshot.Resources = append(domain.Snapshot.Resources, semanticResourceFixture("resource:meter.synthetic", "resource.meter", ""))
	meter := semanticTextQualifier("infrastructure.dimension.meter", "resource:meter.synthetic")
	addSemanticPublishedQuantity(&domain, "helianthus.pack.infrastructure", "infrastructure.energy.import", "unit.joule", semreg.Decimal{Coefficient: "10"}, meter)
	addSemanticPublishedQuantity(&domain, "helianthus.pack.infrastructure", "infrastructure.energy.import", "unit.joule", semreg.Decimal{Coefficient: "11"}, meter, semanticTextQualifier("synthetic.dimension.tariff", "day"))
	addSemanticPublishedQuantity(&domain, "helianthus.pack.infrastructure", "infrastructure.energy.import", "unit.joule", semreg.Decimal{Coefficient: "12"}, meter, semanticTextQualifier("synthetic.dimension.tariff", "night"))

	// Complete old exposition, declared independently of the writer.
	const projected = semanticPrometheusFixtureMetadata +
		`helianthus_semantic_projection_available{domain="pv"} 1
helianthus_semantic_projection_item{domain="pv",item="projection.gateway.pv.inverter.ac.power.active",outcome="transformed"} 1
helianthus_semantic_projection_loss{domain="pv",item="projection.gateway.pv.inverter.ac.power.active",loss_kind="precision"} 1
helianthus_semantic_fact_state{availability="available",dimension="inverter",fact="pv.ac.aggregate_active_power",freshness="fresh",pack="helianthus.pack.pv",promotion="promoted",qualification="qualified",quality="good",resource="resource:inverter.synthetic",resource_kind="resource.inverter"} 1
helianthus_semantic_open_conflicts{dimension="inverter",fact="pv.ac.aggregate_active_power",pack="helianthus.pack.pv",resource="resource:inverter.synthetic",resource_kind="resource.inverter"} 0
helianthus_semantic_evidence_age{dimension="inverter",fact="pv.ac.aggregate_active_power",pack="helianthus.pack.pv",resource="resource:inverter.synthetic",resource_kind="resource.inverter",unit="seconds"} 1
helianthus_semantic_fact_value{dimension="inverter",fact="pv.ac.aggregate_active_power",pack="helianthus.pack.pv",resource="resource:inverter.synthetic",resource_kind="resource.inverter",unit="unit.watt"} 42
`
	const single = `helianthus_semantic_fact_state{availability="available",dimension="infrastructure.dimension.meter",fact="infrastructure.energy.import",freshness="fresh",pack="helianthus.pack.infrastructure",promotion="promoted",qualification="qualified",quality="good",resource="resource:meter.synthetic",resource_kind="resource.meter"} 1
helianthus_semantic_open_conflicts{dimension="infrastructure.dimension.meter",fact="infrastructure.energy.import",pack="helianthus.pack.infrastructure",resource="resource:meter.synthetic",resource_kind="resource.meter"} 0
helianthus_semantic_evidence_age{dimension="infrastructure.dimension.meter",fact="infrastructure.energy.import",pack="helianthus.pack.infrastructure",resource="resource:meter.synthetic",resource_kind="resource.meter",unit="seconds"} 1
helianthus_semantic_fact_value{dimension="infrastructure.dimension.meter",fact="infrastructure.energy.import",pack="helianthus.pack.infrastructure",resource="resource:meter.synthetic",resource_kind="resource.meter",unit="unit.joule"} 10
`
	const before = projected + single + "helianthus_semantic_render_overflow 2\n"
	const day = `dimension="[{\"id\":\"infrastructure.dimension.meter\",\"value\":{\"kind\":\"text\",\"text\":\"resource:meter.synthetic\"}},{\"id\":\"synthetic.dimension.tariff\",\"value\":{\"kind\":\"text\",\"text\":\"day\"}}]",`
	const night = `dimension="[{\"id\":\"infrastructure.dimension.meter\",\"value\":{\"kind\":\"text\",\"text\":\"resource:meter.synthetic\"}},{\"id\":\"synthetic.dimension.tariff\",\"value\":{\"kind\":\"text\",\"text\":\"night\"}}]",`
	const state = `fact="infrastructure.energy.import",freshness="fresh",pack="helianthus.pack.infrastructure",promotion="promoted",qualification="qualified",quality="good",resource="resource:meter.synthetic",resource_kind="resource.meter"} 1`
	const common = `fact="infrastructure.energy.import",pack="helianthus.pack.infrastructure",resource="resource:meter.synthetic",resource_kind="resource.meter"`

	// Old sample -> new sample. Blank means absent. Everything else is
	// identical, including metadata, domain projections and single qualifiers.
	changes := [][2]string{
		{"helianthus_semantic_render_overflow 2", ""},
		{"", `helianthus_semantic_fact_state{availability="available",` + day + state},
		{"", `helianthus_semantic_open_conflicts{` + day + common + `} 0`},
		{"", `helianthus_semantic_evidence_age{` + day + common + `,unit="seconds"} 1`},
		{"", `helianthus_semantic_fact_value{` + day + common + `,unit="unit.joule"} 11`},
		{"", `helianthus_semantic_fact_state{availability="available",` + night + state},
		{"", `helianthus_semantic_open_conflicts{` + night + common + `} 0`},
		{"", `helianthus_semantic_evidence_age{` + night + common + `,unit="seconds"} 1`},
		{"", `helianthus_semantic_fact_value{` + night + common + `,unit="unit.joule"} 12`},
	}
	want, additions := before, ""
	for _, change := range changes {
		if old := change[0]; old != "" {
			if strings.Count(want, old+"\n") != 1 {
				t.Fatalf("invalid history map entry: %q", old)
			}
			want = strings.Replace(want, old+"\n", "", 1)
		}
		if change[1] != "" {
			additions += change[1] + "\n"
		}
	}
	// In canonical key order the two multikeys precede the single key.
	want = strings.Replace(want, single, additions+single, 1)
	if got := RenderSemanticPrometheus([]SemanticMetricsDomain{domain}, time.Unix(101, 0)); got != want {
		t.Fatalf("complete exposition:\n%s\nwant:\n%s", got, want)
	}
}

func TestSemanticPrometheusQualifierKeyFields(t *testing.T) {
	const (
		measurement = semreg.DefinitionID("infrastructure.dimension.measurement_type")
		meter       = semreg.DefinitionID("infrastructure.dimension.meter")
		primary     = "resource:meter.synthetic"
		other       = "resource:meter.other"
		ratioFact   = semreg.DefinitionID("infrastructure.accuracy.ratio_maximum")
		measured    = semreg.DefinitionID("infrastructure.accuracy.measured")
	)
	// The registry declares the symbol qualifier before the meter in key order.
	// Change each member without relying on candidate IDs to distinguish samples.
	for _, tc := range []struct {
		name          string
		fact          semreg.DefinitionID
		qualifier     semreg.DefinitionID
		value         semreg.Value
		subjectID     semreg.DefinitionID
		resource      string
		qualifierJSON string
		wantValue     string
	}{
		{"base", ratioFact, measurement, semanticSymbolQualifier(measurement, "active_power", true), meter, primary, `{"kind":"symbol","symbol":{"namespace":"infrastructure.dimension.measurement_type","token":"active_power","known":true}}`, `,unit="unit.ratio"} 0.01`},
		{"symbol token", ratioFact, measurement, semanticSymbolQualifier(measurement, "rms_voltage", true), meter, primary, `{"kind":"symbol","symbol":{"namespace":"infrastructure.dimension.measurement_type","token":"rms_voltage","known":true}}`, `,unit="unit.ratio"} 0.01`},
		{"symbol namespace", ratioFact, measurement, semanticSymbolQualifier("synthetic.dimension.type", "active_power", true), meter, primary, `{"kind":"symbol","symbol":{"namespace":"synthetic.dimension.type","token":"active_power","known":true}}`, `,unit="unit.ratio"} 0.01`},
		{"unknown symbol", ratioFact, measurement, semanticSymbolQualifier("native.synthetic", "active_power", false), meter, primary, `{"kind":"symbol","symbol":{"namespace":"native.synthetic","token":"active_power","known":false}}`, `,unit="unit.ratio"} 0.01`},
		{"text kind and decoy resource", ratioFact, measurement, semanticTextQualifier(measurement, other).Value, meter, primary, `{"kind":"text","text":"resource:meter.other"}`, `,unit="unit.ratio"} 0.01`},
		{"qualifier ID", ratioFact, "infrastructure.dimension.measurement_other", semanticSymbolQualifier(measurement, "active_power", true), meter, primary, `{"kind":"symbol","symbol":{"namespace":"infrastructure.dimension.measurement_type","token":"active_power","known":true}}`, `,unit="unit.ratio"} 0.01`},
		{"resource ID", ratioFact, measurement, semanticSymbolQualifier(measurement, "active_power", true), meter, other, `{"kind":"symbol","symbol":{"namespace":"infrastructure.dimension.measurement_type","token":"active_power","known":true}}`, `,unit="unit.ratio"} 0.01`},
		{"fact ID", measured, measurement, semanticSymbolQualifier(measurement, "active_power", true), meter, primary, `{"kind":"symbol","symbol":{"namespace":"infrastructure.dimension.measurement_type","token":"active_power","known":true}}`, `,unit="boolean"} 1`},
		{"missing catalog subject", ratioFact, measurement, semanticSymbolQualifier(measurement, "active_power", true), "infrastructure.dimension.site", primary, "", ""},
		{"missing current resource", ratioFact, measurement, semanticSymbolQualifier(measurement, "active_power", true), meter, "resource:meter.missing", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			domain := semanticPublishedFixture()
			domain.Snapshot.Resources = []semreg.Resource{
				semanticResourceFixture(primary, "resource.meter", ""),
				semanticResourceFixture(other, "resource.meter", ""),
			}
			addSemanticPublishedQuantity(&domain, "helianthus.pack.infrastructure", tc.fact, "unit.ratio", semreg.Decimal{Coefficient: "1", Exponent10: -2},
				semreg.Dimension{ID: tc.qualifier, Value: tc.value}, semanticTextQualifier(tc.subjectID, tc.resource))
			if tc.fact == measured {
				bit := true
				domain.Snapshot.Facts[0].Candidates[0].Value = &semreg.Value{Kind: semreg.ValueBoolean, Boolean: &bit}
			}
			got := RenderSemanticPrometheus([]SemanticMetricsDomain{domain}, time.Unix(101, 0))
			const value = "\nhelianthus_semantic_fact_value{"
			if tc.wantValue == "" {
				if got != semanticPrometheusFixtureMetadata+"helianthus_semantic_render_overflow 1\n" {
					t.Fatalf("missing subject admitted:\n%s", got)
				}
				return
			}
			label := `[{"id":"` + string(tc.qualifier) + `","value":` + tc.qualifierJSON + `},{"id":"infrastructure.dimension.meter","value":{"kind":"text","text":"` + tc.resource + `"}}]`
			want := value + `dimension=` + strconv.Quote(label) + `,fact="` + string(tc.fact) + `",pack="helianthus.pack.infrastructure",resource="` + tc.resource + `",resource_kind="resource.meter"` + tc.wantValue + "\n"
			if strings.Count(got, value) != 1 || !strings.Contains(got, want) {
				t.Fatalf("qualifier exposition:\n%s\nmissing %s", got, want)
			}
		})
	}
}

func semanticSymbolQualifier(namespace semreg.DefinitionID, token string, known bool) semreg.Value {
	return semreg.Value{Kind: semreg.ValueSymbol, Symbol: &semreg.Symbol{Namespace: namespace, Token: token, Known: known}}
}
