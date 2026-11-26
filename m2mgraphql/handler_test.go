package m2mgraphql

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	gatewaygraphql "github.com/Project-Helianthus/helianthus-gateway/graphql"
	"github.com/Project-Helianthus/helianthus-semreg/semreg/runtime/tree"
	semreg "github.com/Project-Helianthus/helianthus-semreg/semreg/v1"
	"github.com/Project-Helianthus/helianthus-semreg/semreg/v1/driver"
	"github.com/Project-Helianthus/helianthus-semreg/semreg/v1/graph"
	graphqlgo "github.com/graphql-go/graphql"
)

type graphCuts struct {
	tree.CutSource
	view tree.CutView
}

func (f *graphCuts) Capture(context.Context, driver.Filter) (tree.CutView, error) {
	return f.view, nil
}

func (f *graphCuts) Lookup(_ context.Context, ref graph.CutRef) (tree.CutView, error) {
	if f.view.Ref.CutID == ref.CutID {
		return f.view, nil
	}
	return tree.CutView{}, &tree.ExpiredError{CutID: ref.CutID, ExpiredAt: time.Unix(1, 0)}
}

type graphLifecycle struct{ tree.LifecycleSource }

func (graphLifecycle) OnFence(func(tree.Fence)) func() { return func() {} }

type graphFixtureRecord map[string]json.RawMessage

func (graphFixtureRecord) Validate() error { return nil }

func graphMust(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func graphDigest(label string) semreg.Digest {
	return semreg.Digest(fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(label))))
}

func graphResource(id semreg.ResourceID, kind semreg.DefinitionID, class semreg.AnchorClass) semreg.Resource {
	evidence := semreg.EvidenceRef{Owner: "owner.synthetic", Kind: "evidence.synthetic", Digest: graphDigest("synthetic evidence"),
		Contract: "synthetic/v1", Access: semreg.EvidenceAccessPublic, Redaction: semreg.RedactionNone}
	slot := "slot-" + string(id)
	return semreg.Resource{
		ResourceID: id, Kind: kind, State: semreg.ResourceCurrent, Revision: "1",
		Anchor: semreg.ResourceAnchor{Class: class, Basis: semreg.AnchorOperatorSlot, Slot: &slot, Evidence: []semreg.EvidenceRef{evidence}},
		Origin: semreg.OriginRef{OriginID: semreg.OriginID("origin:" + id), Kind: semreg.OriginOperator, Evidence: []semreg.EvidenceRef{evidence}},
	}
}

// newGraphFixture builds one synthetic installation served by the local schema.
func newGraphFixture(t *testing.T) (graphqlgo.Schema, context.Context, *graphCuts) {
	t.Helper()
	catalog, err := graph.Embedded()
	graphMust(t, err)
	store := graph.NewCutStore(func() time.Time { return time.Unix(0, 0) })
	site := graphResource("resource:site", "resource.site", semreg.AnchorFunction)
	grid := graphResource("resource:grid", "resource.grid_connection", semreg.AnchorFunction)
	evidence := site.Anchor.Evidence
	relation := semreg.RelationInstance{
		Definition: semreg.DefinitionRef{Pack: semreg.PackRef{ID: "helianthus.pack.infrastructure", Version: "1.0.0"}, ID: "infrastructure.relation.site_grid_connection", Version: "1.0.0"},
		From:       site.ResourceID, To: grid.ResourceID, State: semreg.LinkQualified, Revision: "1",
		BasisClass: semreg.RelationConfiguration, Basis: evidence, Origin: site.Origin,
	}
	snapshot := semreg.Snapshot{
		Contract: semreg.ContractKernelV1, AssetID: "asset:a",
		Revisions:         semreg.RevisionVector{Semantic: "1", Identity: "1", Facts: "1", Services: "1", Capabilities: "1", Structure: "1"},
		EvaluatedAt:       semreg.TimePoint{UnixNanoseconds: "101", ClockID: "clock.utc", UncertaintyNS: "0"},
		EvaluateMonotonic: semreg.MonotonicPoint{ClockEpochID: "clock:synthetic", Nanoseconds: "1"},
		Sources:           []semreg.SourceDescriptor{}, Bindings: []semreg.NativeBinding{}, IdentityLinks: []semreg.IdentityLink{},
		Facts: []semreg.FactEnvelope{}, Services: []semreg.ServiceInstance{}, Capabilities: []semreg.CapabilityInstance{},
		Fences: []semreg.GenerationFence{}, Cursors: []semreg.PublicationCursor{},
		Resources: []semreg.Resource{grid, site}, Relations: []semreg.RelationInstance{relation},
	}
	raw, err := json.Marshal(snapshot)
	graphMust(t, err)
	var record graphFixtureRecord
	graphMust(t, json.Unmarshal(raw, &record))
	delete(record, "snapshot_id")
	canonical, err := semreg.CanonicalJSON(record)
	graphMust(t, err)
	snapshot.SnapshotID = semreg.SnapshotID(graphDigest(string(canonical)))
	graphMust(t, snapshot.Validate())
	members := []semreg.Snapshot{snapshot}
	cut, err := graph.AssembleCut(members, catalog, graph.CutPartial)
	graphMust(t, err)
	graphMust(t, store.Retain(cut, members, catalog))
	cuts := &graphCuts{view: tree.CutView{
		Ref: graph.CutRef{CutID: cut.ID(), Completeness: graph.CutPartial, Members: cut.Members(),
			NotIncluded: []graph.NotIncludedAsset{{AssetID: "asset:omitted", Reason: graph.NotIncludedSourceUnavailable}}},
		Cut: cut, Members: members, Store: store, Catalog: catalog,
		Evaluation: semreg.EvaluationContext{EvaluatedAt: snapshot.EvaluatedAt, EvaluateMonotonic: snapshot.EvaluateMonotonic},
	}}
	runtime, err := tree.New(tree.Config{
		Registry: tree.RegistryConfig{Installation: "synthetic", Bridge: "bridge.synthetic", Incarnation: "synthetic", Lifecycle: graphLifecycle{}},
		Cuts:     cuts, Operations: struct{ tree.OperationPort }{},
	})
	graphMust(t, err)
	t.Cleanup(runtime.Registry.Close)
	schema, err := gatewaygraphql.NewQuerySchema(gatewaygraphql.NewBuilder(nil, nil))
	graphMust(t, err)
	return schema, gatewaygraphql.WithSemanticGraph(context.Background(), runtime), cuts
}

// localRead is the local schema's answer to one fixed document.
func localRead(schema graphqlgo.Schema, ctx context.Context, query string, variables map[string]any) (json.RawMessage, error) {
	result := graphqlgo.Do(graphqlgo.Params{Schema: schema, Context: ctx, RequestString: query, VariableValues: variables})
	if len(result.Errors) != 0 {
		return nil, codedError{result.Errors[0].Extensions}
	}
	for _, value := range result.Data.(map[string]any) {
		text, _ := value.(string)
		return json.RawMessage(text), nil
	}
	return nil, fmt.Errorf("no field")
}

type codedError struct{ extensions map[string]any }

func (codedError) Error() string { return "coded" }

func (e codedError) Extensions() map[string]any { return e.extensions }

type graphResponse struct {
	Data   map[string]any `json:"data"`
	Errors []struct {
		Path       []string          `json:"path"`
		Extensions map[string]string `json:"extensions"`
	} `json:"errors"`
}

func graphRequest(operation, query string, variables string) string {
	return `{"operationName":` + strconv.Quote(operation) + `,"query":` + strconv.Quote(query) + `,"variables":` + variables + `}`
}

func serveGraphRequest(t *testing.T, handler http.Handler, body string) graphResponse {
	t.Helper()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, route, strings.NewReader(body)).WithContext(WithRateLimitKey(context.Background(), t.Name())))
	var response graphResponse
	graphMust(t, json.Unmarshal(recorder.Body.Bytes(), &response))
	return response
}

func newGraphHandler(t *testing.T, schema graphqlgo.Schema, ctx context.Context) http.Handler {
	t.Helper()
	read := func(query string) GraphRead {
		return func(_ context.Context, variables map[string]any) (json.RawMessage, error) {
			return localRead(schema, ctx, query, variables)
		}
	}
	handler, err := NewHandler(Config{
		SemanticResources: read(semanticResourcesFixedQuery), SemanticTraversal: read(semanticTraversalFixedQuery),
		SemanticLens: read(semanticLensFixedQuery), SemanticElementCatalog: read(semanticElementCatalogFixedQuery),
	})
	graphMust(t, err)
	return handler
}

const (
	entryVar      = `{"kind":"resource","resource":"resource:site"}`
	definitionVar = `[{"id":"infrastructure.relation.site_grid_connection","version":"1.0.0","pack":{"id":"helianthus.pack.infrastructure","version":"1.0.0"}}]`
)

// Each case is one fixed document; the remote answer is compared with the
// local schema's answer for the same variables and cut.
func TestGraphReadsMatchLocalSchema(t *testing.T) {
	schema, ctx, cuts := newGraphFixture(t)
	handler := newGraphHandler(t, schema, ctx)
	cut := strconv.Quote(string(cuts.view.Ref.CutID))
	cases := []struct {
		name, operation, query, variables string
		root                              string
	}{
		{"resources", "SemanticResources", semanticResourcesFixedQuery, `{"page_size":2,"kind":"resource.site","plane":"physical"}`, "semantic_resources"},
		{"resources_pinned", "SemanticResources", semanticResourcesFixedQuery, `{"page_size":1,"cut_id":` + cut + `}`, "semantic_resources"},
		{"traversal", "SemanticTraversal", semanticTraversalFixedQuery, `{"entry":` + entryVar + `,"direction":"outgoing","depth":1,"page_size":2,"planes":["energy"],"definitions":` + definitionVar + `}`, "semantic_traversal"},
		{"traversal_lens", "SemanticTraversal", semanticTraversalFixedQuery, `{"entry":{"kind":"lens_roots","lens":"lens.energy_flow"},"lens":"lens.energy_flow","direction":"both","depth":1,"page_size":2,"cut_id":` + cut + `}`, "semantic_traversal"},
		{"lens", "SemanticLens", semanticLensFixedQuery, `{"entry":{"kind":"lens_roots","lens":"lens.energy_flow"},"lens":"lens.energy_flow","direction":"both","depth":1,"page_size":2}`, "semantic_lens"},
		{"catalog", "SemanticElementCatalog", semanticElementCatalogFixedQuery, `{}`, "semantic_element_catalog"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var variables map[string]any
			graphMust(t, json.Unmarshal([]byte(tc.variables), &variables))
			if page, ok := variables["page_size"].(float64); ok {
				variables["page_size"] = int(page)
			}
			if depth, ok := variables["depth"].(float64); ok {
				variables["depth"] = int(depth)
			}
			want, err := localRead(schema, ctx, tc.query, variables)
			graphMust(t, err)
			response := serveGraphRequest(t, handler, graphRequest(tc.operation, tc.query, tc.variables))
			got, _ := response.Data[tc.root].(string)
			if len(response.Errors) != 0 || got == "" || got != string(want) {
				t.Fatalf("remote %v errors=%v\nlocal %s", response.Data, response.Errors, want)
			}
		})
	}
}

func TestGraphReadErrorsMatchLocalSchema(t *testing.T) {
	schema, ctx, _ := newGraphFixture(t)
	handler := newGraphHandler(t, schema, ctx)
	gone := strconv.Quote(string(graphDigest("gone cut")))
	cases := []struct{ name, operation, query, variables, root, code string }{
		{"expired_cut", "SemanticResources", semanticResourcesFixedQuery, `{"page_size":1,"cut_id":` + gone + `}`, "semantic_resources", "revision_conflict"},
		{"bad_token", "SemanticResources", semanticResourcesFixedQuery, `{"page_size":1,"page_token":"malformed"}`, "semantic_resources", "invalid_value"},
		{"page_bound", "SemanticResources", semanticResourcesFixedQuery, fmt.Sprintf(`{"page_size":%d}`, graph.MaxPageSize+1), "semantic_resources", "bounds_exceeded"},
		{"depth_bound", "SemanticTraversal", semanticTraversalFixedQuery, fmt.Sprintf(`{"entry":%s,"direction":"outgoing","depth":%d,"page_size":1,"planes":["energy"]}`, entryVar, graph.MaxTraversalDepth+1), "semantic_traversal", "bounds_exceeded"},
		{"unknown_lens", "SemanticLens", semanticLensFixedQuery, `{"entry":{"kind":"lens_roots","lens":"lens.not_in_catalog"},"lens":"lens.not_in_catalog","direction":"both","depth":1,"page_size":1}`, "semantic_lens", "membership_missing"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var variables map[string]any
			graphMust(t, json.Unmarshal([]byte(tc.variables), &variables))
			if page, ok := variables["page_size"].(float64); ok {
				variables["page_size"] = int(page)
			}
			if depth, ok := variables["depth"].(float64); ok {
				variables["depth"] = int(depth)
			}
			_, err := localRead(schema, ctx, tc.query, variables)
			var local codedError
			if !errors.As(err, &local) {
				t.Fatal("local schema did not fail")
			}
			if local.extensions["code"] != tc.code {
				t.Fatalf("local code %v, want %s", local.extensions["code"], tc.code)
			}
			response := serveGraphRequest(t, handler, graphRequest(tc.operation, tc.query, tc.variables))
			if len(response.Errors) != 1 || response.Errors[0].Extensions["code"] != tc.code || !reflect.DeepEqual(response.Errors[0].Path, []string{tc.root}) {
				t.Fatalf("remote %v errors=%+v, local %v, want %s", response.Data, response.Errors, local, tc.code)
			}
		})
	}
}

// Rule 20: one installation served twice publishes equal graphs.
func TestGraphReadsServedTwiceAreEqual(t *testing.T) {
	schema, ctx, _ := newGraphFixture(t)
	handler := newGraphHandler(t, schema, ctx)
	request := graphRequest("SemanticElementCatalog", semanticElementCatalogFixedQuery, `{}`)
	first := serveGraphRequest(t, handler, request)
	second := serveGraphRequest(t, handler, request)
	if first.Data["semantic_element_catalog"] == nil || !reflect.DeepEqual(first, second) {
		t.Fatalf("published graphs differ: %v vs %v", first, second)
	}
}

func TestGraphReadsKeepOtherOperationsRejected(t *testing.T) {
	provider := func(context.Context, map[string]any) (json.RawMessage, error) { return json.RawMessage(`{}`), nil }
	handler, err := NewHandler(Config{SemanticResources: provider, SemanticTraversal: provider, SemanticLens: provider, SemanticElementCatalog: provider})
	graphMust(t, err)
	const pvVariables = `{"request":{"contract_id":"PUBLIC_GRAPHQL_SEMANTIC_PV_V1","asset_ref":"x"}}`
	for name, body := range map[string]string{
		"unknown_operation":        graphRequest("SemanticSnapshot", `query SemanticSnapshot { semantic_snapshot }`, pvVariables),
		"mutation":                 graphRequest("SemanticOperation", `mutation SemanticOperation { semantic_operation }`, pvVariables),
		"edited_document":          graphRequest("SemanticElementCatalog", `query SemanticElementCatalog { semantic_element_catalog semantic_resources(page_size: 1) }`, `{}`),
		"other_document":           graphRequest("SemanticElementCatalog", semanticResourcesFixedQuery, `{}`),
		"current_provider_missing": graphRequest("SemanticPVCurrent", semanticPVFixedQuery, `{"request":{"contract_id":"PUBLIC_GRAPHQL_SEMANTIC_PV_V1","asset_ref":"x"}}`),
	} {
		t.Run(name, func(t *testing.T) {
			response := serveGraphRequest(t, handler, body)
			if len(response.Errors) != 1 || response.Errors[0].Extensions["code"] != "QUERY_REJECTED" {
				t.Fatalf("%s: %+v", name, response)
			}
		})
	}
	absent, err := NewHandler(Config{SemanticResources: provider})
	graphMust(t, err)
	if response := serveGraphRequest(t, absent, graphRequest("SemanticLens", semanticLensFixedQuery, `{}`)); len(response.Errors) != 1 || response.Errors[0].Extensions["code"] != "QUERY_REJECTED" {
		t.Fatalf("read without a provider: %+v", response)
	}
}

func TestGraphReadsRejectInvalidVariables(t *testing.T) {
	provider := func(context.Context, map[string]any) (json.RawMessage, error) { return json.RawMessage(`{}`), nil }
	handler, err := NewHandler(Config{SemanticResources: provider, SemanticTraversal: provider})
	graphMust(t, err)
	for name, tc := range map[string]struct{ operation, query, variables string }{
		"missing_page_size": {"SemanticResources", semanticResourcesFixedQuery, `{}`},
		"undeclared":        {"SemanticResources", semanticResourcesFixedQuery, `{"page_size":1,"asset_ref":"x"}`},
		"fractional_int":    {"SemanticResources", semanticResourcesFixedQuery, `{"page_size":1.5}`},
		"string_for_int":    {"SemanticResources", semanticResourcesFixedQuery, `{"page_size":"1"}`},
		"entry_extra":       {"SemanticTraversal", semanticTraversalFixedQuery, `{"entry":{"kind":"sites","x":"y"},"direction":"both","depth":1,"page_size":1}`},
	} {
		t.Run(name, func(t *testing.T) {
			response := serveGraphRequest(t, handler, graphRequest(tc.operation, tc.query, tc.variables))
			if len(response.Errors) != 1 || response.Errors[0].Extensions["code"] != "REQUEST_INVALID" {
				t.Fatalf("%s: %+v", name, response)
			}
		})
	}
}
