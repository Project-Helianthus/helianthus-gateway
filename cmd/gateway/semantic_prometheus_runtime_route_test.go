package main

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	ebusgateway "github.com/Project-Helianthus/helianthus-gateway"
	"github.com/Project-Helianthus/helianthus-gateway/internal/semhub"
	"github.com/Project-Helianthus/helianthus-semreg/semreg/runtime/tree"
	semreg "github.com/Project-Helianthus/helianthus-semreg/semreg/v1"
)

// TestNativePointRouteComposedScrape scrapes the real route once with the
// three inputs together: a canonical cut published through the hub, a
// persisted metric store reconciled from that cut, and a namespace tree of
// synthetic drivers. The runtime is given no injected domain and no prebuilt
// observation, so every series reaches the exposition through the cut, the
// store or the namespace.
//
// Each check pins one production mutation: an injected domain list or a
// prebuilt observation list on the runtime (asserted empty); a dropped cut
// (no canonical series under the resource key); a dropped store (no slot
// series, and the store object unchanged by the scrape); a dropped namespace
// (no native series); a dropped 1025th value (power count and its sample);
// a dropped instance label (the equal-index pair collapses); an index
// invented for the unindexed points; a stale point exporting a value.
func TestNativePointRouteComposedScrape(t *testing.T) {
	hub, err := semhub.New(semhub.Options{})
	if err != nil {
		t.Fatal(err)
	}
	publishServedZone(t, hub, 0)
	provider := newGatewaySemanticSnapshotProvider(&gatewayRuntimeControl{hub: hub})
	provider.now = func() time.Time { return time.Unix(0, m38Wall+int64(time.Second)) }

	directory := gatewayMetricStoreDir(t)
	registry := gatewaySemanticMetricRegistry(directory)
	if registry.StoreError != "" {
		t.Fatal(registry.StoreError)
	}
	metrics, err := newGatewaySemanticMetrics(provider, registry)
	if err != nil {
		t.Fatal(err)
	}
	metrics.now = func() time.Time { return time.Unix(0, m38Wall+int64(time.Second)).UTC() }
	if err = metrics.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}

	retained := semreg.Uint64("9000000000")
	const received = int64(92 * time.Second)
	published := []namespacePublished{
		{namespace: "family.alpha", instance: "inst.a", kind: "node.outdoor", point: "water_pressure", unit: "unit.bar", channel: "2", encoded: "1.25", freshness: semreg.FreshnessFresh, received: received, age: &retained},
		{namespace: "family.alpha", instance: "inst.b", kind: "node.outdoor", point: "water_pressure", unit: "unit.bar", channel: "2", encoded: "1.25", freshness: semreg.FreshnessFresh, received: received, age: &retained},
		{namespace: "family.beta", instance: "inst.c", kind: "node.battery", point: "pack_temperature", unit: "unit.celsius", encoded: "20", freshness: semreg.FreshnessFresh, received: received, age: &retained},
		{namespace: "family.beta", instance: "inst.c", kind: "node.battery", point: "coolant_flow", unit: "unit.litre_per_minute", encoded: "12", freshness: semreg.FreshnessFresh, received: received, age: &retained},
		{namespace: "family.beta", instance: "inst.c", kind: "node.battery", point: "stored_energy", unit: "unit.kilowatt_hour", encoded: "2.5", freshness: semreg.FreshnessFresh, received: received, age: &retained},
		{namespace: "family.beta", instance: "inst.c", kind: "node.battery", point: "fan_speed", unit: "unit.revolution_per_minute", encoded: "3000", freshness: semreg.FreshnessFresh, received: received, age: &retained},
		{namespace: "family.beta", instance: "inst.c", kind: "node.battery", point: "charge_level", unit: "unit.percent", encoded: "45", freshness: semreg.FreshnessFresh, received: received, age: &retained},
		{namespace: "family.alpha", instance: "inst.a", kind: "node.meter", point: "stale_power", unit: "unit.watt", encoded: "12.5", freshness: semreg.FreshnessStale, received: received, age: &retained},
	}
	for i := 1; i <= 1025; i++ {
		published = append(published, namespacePublished{namespace: "family.alpha", instance: "inst.a", kind: "node.meter", point: "point-" + strconv.Itoa(i), unit: "unit.watt", encoded: strconv.Itoa(i), freshness: semreg.FreshnessFresh, received: received, age: &retained})
	}
	runtimeTree := namespaceFixture(t, published)
	metrics.tree = runtimeTree

	if len(metrics.domains) != 0 || len(metrics.native) != 0 {
		t.Fatalf("runtime carries %d injected domains and %d prebuilt observations; want none", len(metrics.domains), len(metrics.native))
	}
	before := gatewayMetricStoreBytes(t, directory)
	handler := newGatewayMetricsHandler(ebusgateway.NewBusObservabilityStore(ebusgateway.DefaultConfig()), metrics)
	body := m38Scrape(t, handler)
	if gatewayMetricStoreBytes(t, directory) != before {
		t.Fatal("scrape wrote the metric store")
	}
	if len(metrics.domains) != 0 || len(metrics.native) != 0 {
		t.Fatal("scrape filled the injected lists")
	}
	for _, line := range composedRouteExpectation {
		if !strings.Contains(body, "\n"+line+"\n") {
			t.Fatalf("missing %s\n%s", line, body)
		}
	}
	if got := strings.Count(body, "helianthus_native_power{"); got != 1025 {
		t.Fatalf("power series = %d; want the 1025 fresh points", got)
	}
	if got := strings.Count(body, "helianthus_native_point_age{"); got != 1033 {
		t.Fatalf("age series = %d; want 1033 (2 pressure, 5 battery, 1 stale, 1025 power)", got)
	}
	for _, line := range strings.Split(body, "\n") {
		unindexed := strings.Contains(line, `device="node.battery"`) || strings.Contains(line, `device="node.meter"`)
		if unindexed && strings.Contains(line, "index=") {
			t.Fatalf("unindexed point carried an index: %s", line)
		}
		if strings.HasPrefix(line, "helianthus_native_power{") && strings.Contains(line, `point="stale_power"`) {
			t.Fatalf("stale point exported a value: %s", line)
		}
		for _, family := range []string{"helianthus_semantic_fact_value{", "helianthus_semantic_fact_state{", "helianthus_semantic_evidence_age{", "helianthus_semantic_open_conflicts{"} {
			if strings.HasPrefix(line, family) && (strings.Contains(line, "domain=") || strings.Contains(line, "fact_id=")) {
				t.Fatalf("per-fact series kept a domain key: %s", line)
			}
		}
	}

	families := composedInventoryFamilies(t)
	if len(families) != 124 {
		t.Fatalf("inventory families = %d; want 124", len(families))
	}
	for _, name := range composedExpositionFamilies(body) {
		if _, ok := families[name]; !ok {
			t.Fatalf("exposition family %s is not an inventory row", name)
		}
	}

	cut, err := runtimeTree.Capture(context.Background(), tree.CutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cut.Close() }()
	changes, _ := runtimeTree.Registry.Since(0)
	checked := 0
	for _, change := range changes {
		if change.Kind != tree.ChangeMember {
			continue
		}
		descriptor, err := cut.Describe(context.Background(), change.Ref)
		if err != nil {
			t.Fatal(err)
		}
		want, ok := composedRouteOriginals[descriptor.Local]
		if !ok {
			continue
		}
		value, err := cut.Read(context.Background(), change.Ref, tree.ReadOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if got := string(value.Encoded) + " " + string(descriptor.Unit); got != want {
			t.Fatalf("%s namespace value = %q; want %q", descriptor.Local, got, want)
		}
		checked++
	}
	if checked != 7 {
		t.Fatalf("checked %d namespace members; want 7", checked)
	}
}
