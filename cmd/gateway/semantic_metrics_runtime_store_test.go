package main

import (
	"bytes"
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	ebusgateway "github.com/Project-Helianthus/helianthus-gateway"
	"github.com/Project-Helianthus/helianthus-gateway/internal/semanticmetrics"
	"github.com/Project-Helianthus/helianthus-gateway/internal/semhub"
)

func semhubEmptyProvider() (*hubSemanticSnapshotProvider, error) {
	hub, err := semhub.New(semhub.Options{})
	if err != nil {
		return nil, err
	}
	return newGatewaySemanticSnapshotProvider(&gatewayRuntimeControl{hub: hub}), nil
}

func TestSemanticMetricStoreFreshLocationAndReuse(t *testing.T) {
	directory := gatewayMetricStoreDir(t)
	first := gatewaySemanticMetricRegistry(directory)
	if first.StoreError != "" || len(first.Bindings) != 0 {
		t.Fatalf("fresh store: %+v", first)
	}
	stored := gatewayMetricStoreBytes(t, directory)
	second := gatewaySemanticMetricRegistry(directory)
	if second.StoreError != "" || gatewayMetricStoreBytes(t, directory) != stored {
		t.Fatal("restart did not reuse the store")
	}
}

func TestSemanticMetricStoreReportsUnreadableMalformedAndCrossTarget(t *testing.T) {
	t.Run("unreadable", func(t *testing.T) {
		directory := gatewayMetricStoreDir(t)
		if registry := gatewaySemanticMetricRegistry(directory); registry.StoreError != "" {
			t.Fatal(registry.StoreError)
		}
		file := gatewayMetricStoreData(t, directory)
		if err := os.Chmod(file, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(file, 0o600) })
		assertStoreReason(t, directory)
	})
	t.Run("malformed", func(t *testing.T) {
		directory := gatewayMetricStoreDir(t)
		if registry := gatewaySemanticMetricRegistry(directory); registry.StoreError != "" {
			t.Fatal(registry.StoreError)
		}
		if err := os.WriteFile(gatewayMetricStoreData(t, directory), []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
		assertStoreReason(t, directory)
	})
	t.Run("cross-target", func(t *testing.T) {
		directory := gatewayMetricStoreDir(t)
		if registry := gatewaySemanticMetricRegistry(directory); registry.StoreError != "" {
			t.Fatal(registry.StoreError)
		}
		file := gatewayMetricStoreData(t, directory)
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		rewritten := bytes.Replace(raw, []byte(`"Target":"gateway-semantic-metrics"`), []byte(`"Target":"other-target"`), 1)
		if bytes.Equal(rewritten, raw) {
			t.Fatal("store document has no target")
		}
		if err = os.WriteFile(file, rewritten, 0o600); err != nil {
			t.Fatal(err)
		}
		registry := assertStoreReason(t, directory)
		if !strings.Contains(registry.StoreError, "invalid slot store identity") {
			t.Fatal(registry.StoreError)
		}
	})
}

func assertStoreReason(t *testing.T, directory string) semanticmetrics.Registry {
	t.Helper()
	var logged bytes.Buffer
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	registry := gatewaySemanticMetricRegistry(directory)
	if registry.StoreError == "" {
		t.Fatal("store failure was not reported")
	}
	metrics, err := newGatewaySemanticMetrics(nil, registry)
	if err != nil {
		t.Fatal(err)
	}
	body := metrics.text(context.Background())
	reason := strings.ReplaceAll(strings.ReplaceAll(registry.StoreError, "\n", " "), "\r", " ")
	if !strings.Contains(body, "# semantic metric store: "+reason) || !strings.Contains(logged.String(), "semantic metric store: "+registry.StoreError) {
		t.Fatalf("reason %q\nlog %s\n%s", registry.StoreError, logged.String(), body)
	}
	return registry
}

func gatewayMetricStoreDir(t *testing.T) string {
	t.Helper()
	directory := filepath.Join(t.TempDir(), "store")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	return directory
}

func gatewayMetricStoreData(t *testing.T, directory string) string {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	var data string
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".lock") || entry.IsDir() {
			continue
		}
		if data != "" {
			t.Fatalf("several store objects in %s", directory)
		}
		data = filepath.Join(directory, entry.Name())
	}
	if data == "" {
		t.Fatal("store object was not created")
	}
	return data
}

func gatewayMetricStoreBytes(t *testing.T, directory string) string {
	t.Helper()
	raw, err := os.ReadFile(gatewayMetricStoreData(t, directory))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// TestSemanticMetricStoreReconcilesTheServedCut: a supplied store location and
// the accepted cut give persisted, validated assignments before any scrape
// reads them; a restart keeps them validated; and a cut with no resource
// retires every one. Each assertion pins one step the former composition
// skipped: it passed no members, never synced, and never revalidated a
// reopened store.
func TestSemanticMetricStoreReconcilesTheServedCut(t *testing.T) {
	f := m38Setup(t)
	provider := newGatewaySemanticSnapshotProvider(&gatewayRuntimeControl{hub: f.hub})
	provider.now = f.provider.now
	directory := gatewayMetricStoreDir(t)
	metrics, err := newGatewaySemanticMetrics(provider, gatewaySemanticMetricRegistry(directory))
	if err != nil {
		t.Fatal(err)
	}
	metrics.logf = func(string, ...any) {}
	if err = metrics.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	registry, assets := metrics.configured()
	if !registry.Validated || len(registry.Bindings) != len(m38Inventory) || len(assets) != 2 {
		t.Fatalf("reconciled registry: validated=%v bindings=%d assets=%v", registry.Validated, len(registry.Bindings), assets)
	}
	for _, binding := range registry.Bindings {
		if binding.Retired || binding.AssetID == "" || binding.SourceID == "" || binding.BindingID == "" || binding.ServiceInstance == "" {
			t.Fatalf("assignment without its provenance: %+v", binding)
		}
	}
	body := metrics.text(context.Background())
	for _, entry := range m38Inventory {
		if strings.Count(body, "\n"+entry.name+"{") != 1 || !strings.Contains(body, "} "+entry.sample+"\n") {
			t.Fatalf("%s was not exported from the reconciled slots:\n%s", entry.name, body)
		}
	}
	stored := gatewayMetricStoreBytes(t, directory)
	if !strings.Contains(stored, `"Retired":false`) || strings.Contains(stored, `"Bindings":[]`) {
		t.Fatalf("assignments were not persisted:\n%s", stored)
	}

	restarted, err := newGatewaySemanticMetrics(provider, gatewaySemanticMetricRegistry(directory))
	if err != nil {
		t.Fatal(err)
	}
	reopened, reopenedAssets := restarted.configured()
	if !reopened.Validated || len(reopened.Bindings) != len(m38Inventory) || len(reopenedAssets) != 2 {
		t.Fatalf("restart lost the assignments or their validation: validated=%v bindings=%d", reopened.Validated, len(reopened.Bindings))
	}
	if after := restarted.text(context.Background()); !strings.Contains(after, "helianthus_thermal_air_temperature{slot=") {
		t.Fatalf("restart exported no slot value:\n%s", after)
	}
	if gatewayMetricStoreBytes(t, directory) != stored {
		t.Fatal("restart or scrape rewrote the store")
	}

	empty, err := semhubEmptyProvider()
	if err != nil {
		t.Fatal(err)
	}
	restarted.provider = empty
	restarted.logf = func(string, ...any) {}
	if err = restarted.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	retired, none := restarted.configured()
	if len(none) != 0 || len(retired.Bindings) != len(m38Inventory) {
		t.Fatalf("empty cut: assets=%v bindings=%d", none, len(retired.Bindings))
	}
	for _, binding := range retired.Bindings {
		if !binding.Retired {
			t.Fatalf("empty cut left %+v active", binding)
		}
	}
	if emptied := restarted.text(context.Background()); strings.Contains(emptied, "helianthus_thermal_air_temperature{") {
		t.Fatalf("retired slots exported a value:\n%s", emptied)
	}
}

// TestSemanticMetricServedCutJoinsTheTransportFamilies: the served cut reaches
// the scrape through the capture of the shared hub, and a transport exposition
// that already declares the per-fact families keeps one declaration of each,
// with its own samples first and the cut's samples under them.
func TestSemanticMetricServedCutJoinsTheTransportFamilies(t *testing.T) {
	f := m38Setup(t)
	provider := newGatewaySemanticSnapshotProvider(&gatewayRuntimeControl{hub: f.hub})
	provider.now = f.provider.now
	metrics, err := newGatewaySemanticMetrics(provider, gatewaySemanticMetricRegistry())
	if err != nil {
		t.Fatal(err)
	}
	metrics.logf = func(string, ...any) {}
	f.store.SetSemanticMetricsProvider(func(time.Time) []ebusgateway.SemanticMetricsDomain {
		return []ebusgateway.SemanticMetricsDomain{composedCanonicalDomain()}
	})
	body := m38Scrape(t, newGatewayMetricsHandler(f.store, metrics))
	declared := map[string]int{}
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "# TYPE ") {
			declared[strings.Fields(line)[2]]++
		}
	}
	for name, count := range declared {
		if count != 1 {
			t.Fatalf("family %s declared %d times", name, count)
		}
	}
	transport := `helianthus_semantic_fact_value{dimension="inverter",fact="pv.ac.aggregate_active_power",pack="helianthus.pack.pv",resource="resource:inverter.synthetic",resource_kind="resource.inverter",unit="unit.watt"} 42`
	if !strings.Contains(body, transport+"\n") {
		t.Fatalf("transport sample missing:\n%s", body)
	}
	cut := 0
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "helianthus_semantic_fact_value{") && line != transport && strings.Contains(line, `fact="thermal.`) {
			cut++
		}
	}
	if cut == 0 {
		t.Fatalf("no served-cut value reached the scrape:\n%s", body)
	}
	if strings.Index(body, transport) > strings.Index(body, `fact="thermal.`) {
		t.Fatalf("cut samples precede the transport sample:\n%s", body)
	}
}
