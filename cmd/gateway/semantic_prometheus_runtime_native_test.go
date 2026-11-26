package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	ebusgateway "github.com/Project-Helianthus/helianthus-gateway"
	semreg "github.com/Project-Helianthus/helianthus-semreg/semreg/v1"
	"github.com/Project-Helianthus/helianthus-semreg/semreg/v1/projection"
)

func composedCanonicalDomain() ebusgateway.SemanticMetricsDomain {
	resource := "resource:inverter.synthetic"
	key := semreg.FactKey{PackID: "helianthus.pack.pv", PackVersion: "1.0.0", FactID: "pv.ac.aggregate_active_power", Dimensions: []semreg.Dimension{{ID: "pv.dimension.inverter", Value: semreg.Value{Kind: semreg.ValueText, Text: &resource}}}}
	value := semreg.Value{Kind: semreg.ValueQuantity, Quantity: &semreg.Quantity{Number: semreg.Decimal{Coefficient: "42"}, Unit: "unit.watt"}}
	candidate := semreg.FactCandidate{CandidateID: "candidate:test", Key: key, Value: &value, Quality: semreg.Quality{Qualification: semreg.QualificationQualified, Promotion: semreg.PromotionPromoted, Validity: semreg.ValidityGood}, Times: semreg.Times{ReceivedAt: semreg.TimePoint{UnixNanoseconds: "100000000000", ClockID: "clock.utc"}}}
	snapshot := semreg.Snapshot{SnapshotID: "snapshot:test", Facts: []semreg.FactEnvelope{{Key: key, Candidates: []semreg.FactCandidate{candidate}}}, Resources: []semreg.Resource{{ResourceID: "resource:inverter.synthetic", Kind: "resource.inverter", Anchor: semreg.ResourceAnchor{Class: semreg.AnchorEquipment, Basis: semreg.AnchorOperatorSlot}, State: semreg.ResourceCurrent, Revision: "1"}}}
	evaluation := semreg.EvaluationView{SnapshotID: snapshot.SnapshotID, Facts: []semreg.EvaluatedFact{{CandidateID: candidate.CandidateID, Freshness: semreg.FreshnessFresh, EffectiveAvailability: semreg.AvailabilityAvailable}}, EvaluationDigest: "digest:synthetic"}
	report := projection.ProjectionReport{SnapshotID: snapshot.SnapshotID, Dispositions: []projection.ProjectionDisposition{{ItemID: "projection.gateway.pv.inverter.ac.power.active", Outcome: projection.ProjectionExact, SourceKeys: []semreg.FactKey{key}}}}
	return ebusgateway.SemanticMetricsDomain{Name: "pv", Snapshot: snapshot, Evaluation: evaluation, Projection: report, Available: true}
}

func composedExpositionFamilies(body string) []string {
	seen := map[string]struct{}{}
	var names []string
	for _, line := range strings.Split(body, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		name := fields[0]
		if name == "#" && len(fields) >= 3 {
			name = fields[2]
		}
		name = strings.SplitN(name, "{", 2)[0]
		if name == "" || name == "#" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	return names
}

func composedInventoryFamilies(t *testing.T) map[string]struct{} {
	t.Helper()
	_, source, _, _ := runtime.Caller(0)
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(source), "..", "..", "docs", "metrics-inventory.json"))
	if err != nil {
		t.Fatal(err)
	}
	var inventory struct {
		Families []struct {
			Name string `json:"name"`
		} `json:"families"`
	}
	if err = json.Unmarshal(raw, &inventory); err != nil {
		t.Fatal(err)
	}
	names := make(map[string]struct{}, len(inventory.Families))
	for _, row := range inventory.Families {
		names[row.Name] = struct{}{}
	}
	return names
}
