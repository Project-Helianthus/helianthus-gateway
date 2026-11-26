package m2mgraphql

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

// The client posts each read's fixed document to a handler and the answer is
// the one the local schema gives for the same request.
func TestClientGraphReadsMatchLocalSchema(t *testing.T) {
	schema, ctx, cuts := newGraphFixture(t)
	handler := newGraphHandler(t, schema, ctx)
	var seen []map[string]any
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var envelope map[string]any
		graphMust(t, json.Unmarshal(body, &envelope))
		seen = append(seen, envelope)
		r.Body = io.NopCloser(bytes.NewReader(body))
		handler.ServeHTTP(w, r.WithContext(WithRateLimitKey(r.Context(), "client-"+envelope["operationName"].(string))))
	}))
	defer server.Close()
	client := &Client{url: server.URL + route, asset: "asset:unused", http: server.Client()}

	decode := func(t *testing.T, response Response, err error) string {
		t.Helper()
		graphMust(t, err)
		var body graphResponse
		graphMust(t, json.Unmarshal(response.Body, &body))
		if len(body.Errors) != 0 {
			t.Fatalf("errors: %+v", body.Errors)
		}
		for _, value := range body.Data {
			return value.(string)
		}
		t.Fatal("no data")
		return ""
	}
	local := func(t *testing.T, query string, variables map[string]any) string {
		t.Helper()
		raw, err := localRead(schema, ctx, query, variables)
		graphMust(t, err)
		return string(raw)
	}
	entry := GraphEntry{Kind: "resource", Resource: "resource:site"}
	definitions := []GraphDefinitionRef{{ID: "infrastructure.relation.site_grid_connection", Version: "1.0.0", Pack: GraphPackRef{ID: "helianthus.pack.infrastructure", Version: "1.0.0"}}}
	pin := string(cuts.view.Ref.CutID)

	t.Run("resources", func(t *testing.T) {
		response, err := client.SemanticResources(context.Background(), SemanticResourcesRequest{GraphPage: GraphPage{PageSize: 2, CutID: pin}, Kind: "resource.site", Plane: "physical"})
		got := decode(t, response, err)
		want := local(t, semanticResourcesFixedQuery, map[string]any{"page_size": 2, "cut_id": pin, "kind": "resource.site", "plane": "physical"})
		if got != want {
			t.Fatalf("got %s want %s", got, want)
		}
	})
	t.Run("traversal", func(t *testing.T) {
		response, err := client.SemanticTraversal(context.Background(), SemanticTraversalRequest{GraphPage: GraphPage{PageSize: 2}, Entry: entry, Direction: "outgoing", Depth: 1, Planes: []string{"energy"}, Definitions: definitions})
		got := decode(t, response, err)
		want := local(t, semanticTraversalFixedQuery, map[string]any{"page_size": 2, "direction": "outgoing", "depth": 1, "planes": []any{"energy"},
			"entry":       map[string]any{"kind": "resource", "resource": "resource:site"},
			"definitions": []any{map[string]any{"id": definitions[0].ID, "version": "1.0.0", "pack": map[string]any{"id": definitions[0].Pack.ID, "version": "1.0.0"}}}})
		if got != want {
			t.Fatalf("got %s want %s", got, want)
		}
	})
	t.Run("lens", func(t *testing.T) {
		lensEntry := GraphEntry{Kind: "lens_roots", Lens: "lens.energy_flow"}
		response, err := client.SemanticLens(context.Background(), SemanticLensRequest{GraphPage: GraphPage{PageSize: 2}, Entry: lensEntry, Direction: "both", Depth: 1, Lens: "lens.energy_flow"})
		got := decode(t, response, err)
		want := local(t, semanticLensFixedQuery, map[string]any{"page_size": 2, "direction": "both", "depth": 1, "lens": "lens.energy_flow",
			"entry": map[string]any{"kind": "lens_roots", "lens": "lens.energy_flow"}})
		if got != want {
			t.Fatalf("got %s want %s", got, want)
		}
	})
	t.Run("catalog", func(t *testing.T) {
		response, err := client.SemanticElementCatalog(context.Background())
		got := decode(t, response, err)
		if want := local(t, semanticElementCatalogFixedQuery, nil); got != want {
			t.Fatalf("got %s want %s", got, want)
		}
	})
	t.Run("sends_fixed_documents", func(t *testing.T) {
		want := []string{semanticResourcesFixedQuery, semanticTraversalFixedQuery, semanticLensFixedQuery, semanticElementCatalogFixedQuery}
		var got []string
		for _, envelope := range seen {
			got = append(got, envelope["query"].(string))
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("documents sent: %v", got)
		}
	})
	t.Run("unavailable_client", func(t *testing.T) {
		var missing *Client
		if _, err := missing.SemanticElementCatalog(context.Background()); err == nil {
			t.Fatal("nil client answered")
		}
	})
}
