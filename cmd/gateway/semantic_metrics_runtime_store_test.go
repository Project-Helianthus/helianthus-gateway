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

	"github.com/Project-Helianthus/helianthus-gateway/internal/semanticmetrics"
)

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
	directory := filepath.Join("/Volumes/Helianthus Work/Helianthus Project/work/wave1-20261014/gateway-1745/.tmp", "gg05-"+strings.ReplaceAll(t.Name(), "/", "-")+"-"+itoa(int(time.Now().UnixNano())))
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
