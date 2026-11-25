package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	ebusgateway "github.com/Project-Helianthus/helianthus-gateway"
	semreg "github.com/Project-Helianthus/helianthus-semreg/semreg/v1"
	"github.com/Project-Helianthus/helianthus-semreg/semreg/v1/projection"
)

func TestNativePointComposedScrape(t *testing.T) {
	directory := gatewayMetricStoreDir(t)
	registry := gatewaySemanticMetricRegistry(directory)
	if registry.StoreError != "" {
		t.Fatal(registry.StoreError)
	}
	before := gatewayMetricStoreBytes(t, directory)
	metrics, err := newGatewaySemanticMetrics(nil, registry)
	if err != nil {
		t.Fatal(err)
	}
	metrics.now = func() time.Time { return time.Unix(101, 0).UTC() }
	metrics.domains = []ebusgateway.SemanticMetricsDomain{composedCanonicalDomain()}
	metrics.native = composedNativePoints()
	originalPressure, originalSpeed := *metrics.native[0].NativeValue, *metrics.native[2].NativeValue
	originalPressureUnit, originalSpeedUnit := metrics.native[0].NativeUnit, metrics.native[2].NativeUnit
	body := metrics.text(context.Background())
	if gatewayMetricStoreBytes(t, directory) != before {
		t.Fatal("scrape wrote the metric store")
	}
	if *metrics.native[0].NativeValue != originalPressure || metrics.native[0].NativeUnit != originalPressureUnit || *metrics.native[2].NativeValue != originalSpeed || metrics.native[2].NativeUnit != originalSpeedUnit {
		t.Fatal("native namespace values were replaced")
	}
	for _, line := range composedScrapeExpectation {
		if !strings.Contains(body, line) {
			t.Fatalf("missing %s\n%s", line, body)
		}
	}
	for _, line := range strings.Split(body, "\n") {
		if strings.Contains(line, `point="speed"`) && strings.Contains(line, "index=") {
			t.Fatalf("unindexed point carried an index:\n%s", line)
		}
		if strings.Contains(line, `point="stale_power"`) && strings.Contains(line, "helianthus_native_power{") {
			t.Fatalf("stale point exported a value:\n%s", line)
		}
		if strings.HasPrefix(line, "helianthus_semantic_fact_") || strings.HasPrefix(line, "helianthus_semantic_evidence_age{") || strings.HasPrefix(line, "helianthus_semantic_open_conflicts{") {
			if strings.Contains(line, "domain=") || strings.Contains(line, "fact_id=") {
				t.Fatal(line)
			}
		}
	}
	if strings.Count(body, "helianthus_native_power{") != 1025 {
		t.Fatalf("power series = %d", strings.Count(body, "helianthus_native_power{"))
	}
	if !strings.Contains(body, `helianthus_semantic_projection_available{domain="pv"}`) || !strings.Contains(body, "# HELP helianthus_semantic_field_available ") {
		t.Fatal("summary or projection family missing")
	}
	families := composedInventoryFamilies(t)
	if len(families) != 124 {
		t.Fatalf("inventory families = %d", len(families))
	}
	for _, name := range composedExpositionFamilies(body) {
		if _, ok := families[name]; !ok {
			t.Fatalf("exposition family %s is not an inventory row", name)
		}
	}
	store := ebusgateway.NewBusObservabilityStore(ebusgateway.Config{})
	handler := newGatewayMetricsHandler(store, metrics)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("scrape status %d", response.Code)
	}
	for _, line := range composedScrapeExpectation {
		if !strings.Contains(response.Body.String(), line) {
			t.Fatalf("route missing %s", line)
		}
	}
}

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

func composedNativePoints() []ebusgateway.NativeObservation {
	pressure, speed, stale := 1.25, 3000.0, 12.5
	age := 9.0
	points := []ebusgateway.NativeObservation{
		{Protocol: "family.alpha", Device: "outdoor_unit", Point: "pressure", DriverInstance: "instance.a", Index: "2", Indexed: true, Quantity: "pressure", NativeUnit: "bar", NativeValue: &pressure, AgeSeconds: &age, Fresh: true},
		{Protocol: "family.alpha", Device: "outdoor_unit", Point: "pressure", DriverInstance: "instance.b", Index: "2", Indexed: true, Quantity: "pressure", NativeUnit: "bar", NativeValue: &pressure, AgeSeconds: &age, Fresh: true},
		{Protocol: "family.beta", Device: "indoor_unit", Point: "speed", DriverInstance: "instance.a", Quantity: "rotational_speed", NativeUnit: "revolution_per_minute", NativeValue: &speed, AgeSeconds: &age, Fresh: true},
		{Protocol: "family.beta", Device: "indoor_unit", Point: "speed", DriverInstance: "instance.b", Quantity: "rotational_speed", NativeUnit: "revolution_per_minute", NativeValue: &speed, AgeSeconds: &age, Fresh: true},
		{Protocol: "family.alpha", Device: "meter", Point: "stale_power", DriverInstance: "instance.meter", Quantity: "power", NativeUnit: "watt", NativeValue: &stale, AgeSeconds: &age, Fresh: false},
	}
	sharedAge := 9.0
	for i := 1; i <= 1025; i++ {
		value := float64(i)
		points = append(points, ebusgateway.NativeObservation{
			Protocol: "family.alpha", Device: "meter", Point: "point-" + itoa(i), DriverInstance: "instance.meter",
			Quantity: "power", NativeUnit: "watt", NativeValue: &value, AgeSeconds: &sharedAge, Fresh: true,
		})
	}
	return points
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
