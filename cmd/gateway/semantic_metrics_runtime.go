package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	ebusgateway "github.com/Project-Helianthus/helianthus-gateway"
	"github.com/Project-Helianthus/helianthus-gateway/internal/semanticmetrics"
	"github.com/Project-Helianthus/helianthus-gateway/internal/semanticview"
	"github.com/Project-Helianthus/helianthus-semreg/semreg/runtime/tree"
	semreg "github.com/Project-Helianthus/helianthus-semreg/semreg/v1"
)

// gatewaySemanticMetricTarget is the scrape target of the metric store. The
// store location is supplied by the caller. This row does not choose a default
// path and does not add a flag.
const gatewaySemanticMetricTarget = "gateway-semantic-metrics"

// gatewaySemanticMetricRegistry is the production binding registry of the
// scrape route. With no store location it declares the availability family
// and does not capture. With a location it creates the store when absent and
// reopens it when present, with the persisted assignments validated again. A
// scrape only reads the registry this returns.
func gatewaySemanticMetricRegistry(storePath ...string) semanticmetrics.Registry {
	if len(storePath) == 0 || storePath[0] == "" {
		return semanticmetrics.Registry{}
	}
	registry, err := composeGatewaySemanticStore(storePath[0], nil, false)
	if err != nil {
		log.Printf("semantic metric store: %s", err.Error())
		return semanticmetrics.Registry{StoreError: err.Error(), StoreDirectory: storePath[0]}
	}
	return registry
}

// gatewaySemanticMetrics renders the configured semantic families of the
// existing scrape route from the HTTP shared hub snapshot provider, the same
// instance GraphQL, MCP and Portal read. A scrape captures at most once and
// never acquires native data, publishes or touches the transport store.
type gatewaySemanticMetrics struct {
	provider *hubSemanticSnapshotProvider
	// native are observations the caller holds beside the namespace tree.
	native  []ebusgateway.NativeObservation
	domains []ebusgateway.SemanticMetricsDomain
	now     func() time.Time
	logf    func(string, ...any)
	// tree holds the retained namespace members of the drivers.
	tree *tree.Tree

	mu       sync.Mutex
	registry semanticmetrics.Registry
	// assets are the distinct assets of the bindings that can still export a
	// value, in bytewise order.
	assets      []semreg.AssetID
	lastFailure string
}

// newGatewaySemanticMetrics takes ownership of registry; callers must not
// mutate it afterwards. A malformed registry fails composition rather than
// silently omitting its configured series from every scrape.
func newGatewaySemanticMetrics(provider *hubSemanticSnapshotProvider, registry semanticmetrics.Registry) (*gatewaySemanticMetrics, error) {
	if _, err := semanticmetrics.Render(semanticview.View{}, registry); err != nil {
		return nil, fmt.Errorf("semantic metric registry: %w", err)
	}
	metrics := &gatewaySemanticMetrics{provider: provider, logf: log.Printf, now: time.Now}
	metrics.adopt(registry)
	return metrics, nil
}

// adopt installs the registry the scrape reads. An unvalidated registry or a
// retired slot withholds the value whatever the capture holds, so neither
// needs a hub read.
func (m *gatewaySemanticMetrics) adopt(registry semanticmetrics.Registry) {
	var assets []semreg.AssetID
	if registry.Validated {
		seen := map[semreg.AssetID]bool{}
		for _, binding := range registry.Bindings {
			if !binding.Retired && !seen[binding.AssetID] {
				seen[binding.AssetID] = true
				assets = append(assets, binding.AssetID)
			}
		}
		sort.Slice(assets, func(i, j int) bool { return assets[i] < assets[j] })
	}
	m.mu.Lock()
	m.registry, m.assets = registry, assets
	m.mu.Unlock()
}

func (m *gatewaySemanticMetrics) configured() (semanticmetrics.Registry, []semreg.AssetID) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.registry, append([]semreg.AssetID(nil), m.assets...)
}

// text renders every configured field from one coherent capture. A failed
// capture or view withholds every value: configured fields report
// availability 0 and no other read path is tried. Text is empty only when the
// renderer itself fails, which omits the semantic section from that scrape.
func (m *gatewaySemanticMetrics) text(ctx context.Context) string {
	slots, cut, native := m.sections(ctx)
	return slots + cut + native
}

// sections splits one scrape into slot text, per-fact text and native text.
// The per-fact text is the served cut plus any domains the caller attached.
// The slot view and the served cut are read from the same capture.
func (m *gatewaySemanticMetrics) sections(ctx context.Context) (string, string, string) {
	registry, assets := m.configured()
	view := semanticview.View{}
	var failure error
	var served []ebusgateway.SemanticMetricsDomain
	if ids := m.provider.captureIDs(assets); len(ids) > 0 {
		metadata, inputs, err := m.provider.Capture(ctx, ids)
		if err != nil {
			failure = fmt.Errorf("capture: %w", err)
		} else {
			served = m.provider.servedDomains(inputs)
			if len(assets) > 0 {
				if view, err = semanticview.Build(metadata, inputsOfAssets(inputs, assets)); err != nil {
					view, failure = semanticview.View{}, fmt.Errorf("view: %w", err)
				}
			}
		}
	}
	text, err := semanticmetrics.Render(view, registry)
	if err != nil {
		text, failure = "", fmt.Errorf("render: %w", err)
	}
	if registry.StoreError != "" {
		reason := strings.ReplaceAll(strings.ReplaceAll(registry.StoreError, "\n", " "), "\r", " ")
		failure = errors.Join(failure, errors.New(reason))
		text = "# semantic metric store: " + reason + "\n" + text
	}
	now := time.Now
	if m.now != nil {
		now = m.now
	}
	at := now()
	domains := append(served, m.domains...)
	cut := ""
	if len(domains) > 0 {
		cut = ebusgateway.RenderSemanticPrometheus(domains, at)
	}
	retained, err := m.namespaceObservations(ctx, at)
	failure = errors.Join(failure, err)
	native := ebusgateway.RenderNativePrometheus(append(append([]ebusgateway.NativeObservation{}, m.native...), retained...))
	m.note(failure)
	return text, cut, native
}

// inputsOfAssets keeps the captured inputs of the configured assets, in
// capture order.
func inputsOfAssets(inputs []semanticview.Input, assets []semreg.AssetID) []semanticview.Input {
	wanted := make(map[semreg.AssetID]bool, len(assets))
	for _, id := range assets {
		wanted[id] = true
	}
	out := make([]semanticview.Input, 0, len(assets))
	for _, input := range inputs {
		if wanted[input.AssetID] {
			out = append(out, input)
		}
	}
	return out
}

// note logs a failure once when it begins or changes, so a scrape interval
// cannot flood the log; the availability gauge carries the per-scrape state.
// A scraper abandoning its request is not a gateway failure.
func (m *gatewaySemanticMetrics) note(failure error) {
	if errors.Is(failure, context.Canceled) || errors.Is(failure, context.DeadlineExceeded) {
		return
	}
	message := ""
	if failure != nil {
		message = failure.Error()
	}
	m.mu.Lock()
	changed := message != m.lastFailure
	m.lastFailure = message
	m.mu.Unlock()
	if changed && message != "" {
		m.logf("semantic metrics withheld: %s", message)
	}
}

// newGatewayMetricsHandler serves the transport exposition with the status and
// headers of store.MetricsHandler unchanged, then appends the semantic
// families. The store renders and releases its lock before the semantic
// capture starts, so neither section waits on the other; a semantic failure
// affects only semantic samples.
func newGatewayMetricsHandler(store *ebusgateway.BusObservabilityStore, semantic *gatewaySemanticMetrics) http.Handler {
	if store == nil {
		return nil
	}
	transport := store.MetricsHandler()
	return http.HandlerFunc(func(out http.ResponseWriter, r *http.Request) {
		capture := &metricsResponseCapture{header: make(http.Header)}
		var w http.ResponseWriter = capture
		transport.ServeHTTP(w, r)
		w = out
		body := capture.body.String()
		extra := huaweiWireCounters.text()
		if semantic != nil {
			slots, cut, native := semantic.sections(r.Context())
			body, cut = mergeServedCut(body, cut)
			extra = slots + cut + native + extra
		}
		for key, values := range capture.header {
			if key == "Content-Length" {
				continue
			}
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		code := capture.code
		if code == 0 {
			code = http.StatusOK
		}
		w.WriteHeader(code)
		_, _ = io.WriteString(w, body)
		_, _ = io.WriteString(w, extra)
	})
}

// metricsResponseCapture records one transport exposition so the semantic
// section can follow it without a second declaration of the same family.
type metricsResponseCapture struct {
	header http.Header
	code   int
	body   bytes.Buffer
}

func (c *metricsResponseCapture) Header() http.Header { return c.header }

func (c *metricsResponseCapture) WriteHeader(code int) {
	if c.code == 0 {
		c.code = code
	}
}

func (c *metricsResponseCapture) Write(p []byte) (int, error) {
	if c.code == 0 {
		c.code = http.StatusOK
	}
	return c.body.Write(p)
}

// mergeServedCut keeps one declaration of the per-fact families. Samples the
// transport exposition does not already carry are inserted with that
// declaration. A transport exposition without one returns the cut unchanged.
func mergeServedCut(body, cut string) (string, string) {
	if cut == "" || !strings.Contains(body, "# TYPE helianthus_semantic_fact_value ") {
		return body, cut
	}
	have := map[string]struct{}{}
	for _, line := range strings.Split(body, "\n") {
		have[line] = struct{}{}
	}
	var novel []string
	for _, line := range strings.Split(cut, "\n") {
		if line == "" || strings.HasPrefix(line, "# ") {
			continue
		}
		if _, ok := have[line]; ok {
			continue
		}
		novel = append(novel, line)
	}
	if len(novel) == 0 {
		return body, ""
	}
	return insertSemanticSamples(body, novel), ""
}

func insertSemanticSamples(body string, samples []string) string {
	lines := strings.Split(body, "\n")
	end := 0
	for _, line := range lines {
		if line == "" || strings.HasPrefix(line, "# HELP helianthus_semantic_") || strings.HasPrefix(line, "# TYPE helianthus_semantic_") || strings.HasPrefix(line, "helianthus_semantic_") {
			end++
			continue
		}
		break
	}
	if end == 0 {
		return body
	}
	out := make([]string, 0, len(lines)+len(samples))
	out = append(out, lines[:end]...)
	out = append(out, samples...)
	out = append(out, lines[end:]...)
	return strings.Join(out, "\n")
}
