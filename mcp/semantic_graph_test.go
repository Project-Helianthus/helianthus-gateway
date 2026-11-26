package mcp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Project-Helianthus/helianthus-semreg/semreg/runtime/tree"
	semreg "github.com/Project-Helianthus/helianthus-semreg/semreg/v1"
	"github.com/Project-Helianthus/helianthus-semreg/semreg/v1/driver"
	"github.com/Project-Helianthus/helianthus-semreg/semreg/v1/graph"
)

type graphFixtureCuts struct {
	tree.CutSource
	newest tree.CutView
	views  map[semreg.Digest]tree.CutView
}

func (f *graphFixtureCuts) Capture(context.Context, driver.Filter) (tree.CutView, error) {
	return f.newest, nil
}

func (f *graphFixtureCuts) Lookup(_ context.Context, ref graph.CutRef) (tree.CutView, error) {
	if view, ok := f.views[ref.CutID]; ok {
		return view, nil
	}
	return tree.CutView{}, &tree.ExpiredError{CutID: ref.CutID, ExpiredAt: time.Unix(1, 0)}
}

type graphFixtureLifecycle struct{ tree.LifecycleSource }

func (graphFixtureLifecycle) OnFence(func(tree.Fence)) func() { return func() {} }

type graphFixtureJSON map[string]json.RawMessage

func (graphFixtureJSON) Validate() error { return nil }

func graphFixtureDigest(label string) semreg.Digest {
	return semreg.Digest(fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(label))))
}

var graphFixtureEvidence = semreg.EvidenceRef{
	Owner: "owner.synthetic", Kind: "evidence.synthetic", Digest: graphFixtureDigest("synthetic graph evidence"),
	Contract: "synthetic/v1", Access: semreg.EvidenceAccessPublic, Redaction: semreg.RedactionNone,
}

func graphFixtureResource(id semreg.ResourceID, kind semreg.DefinitionID, class semreg.AnchorClass) semreg.Resource {
	slot := "slot-" + string(id)
	return semreg.Resource{
		ResourceID: id, Kind: kind, State: semreg.ResourceCurrent, Revision: "1",
		Anchor: semreg.ResourceAnchor{Class: class, Basis: semreg.AnchorOperatorSlot, Slot: &slot, Evidence: []semreg.EvidenceRef{graphFixtureEvidence}},
		Origin: semreg.OriginRef{OriginID: semreg.OriginID("origin:" + id), Kind: semreg.OriginOperator, Evidence: []semreg.EvidenceRef{graphFixtureEvidence}},
	}
}

func graphFixtureSnapshot(t *testing.T, asset semreg.AssetID, resources []semreg.Resource, relations []semreg.RelationInstance) semreg.Snapshot {
	t.Helper()
	snapshot := semreg.Snapshot{
		Contract: semreg.ContractKernelV1, AssetID: asset,
		Revisions:         semreg.RevisionVector{Semantic: "1", Identity: "1", Facts: "1", Services: "1", Capabilities: "1", Structure: "1"},
		EvaluatedAt:       semreg.TimePoint{UnixNanoseconds: "101", ClockID: "clock.utc", UncertaintyNS: "0"},
		EvaluateMonotonic: semreg.MonotonicPoint{ClockEpochID: "clock:synthetic", Nanoseconds: "1"},
		Sources:           []semreg.SourceDescriptor{}, Bindings: []semreg.NativeBinding{}, IdentityLinks: []semreg.IdentityLink{},
		Facts: []semreg.FactEnvelope{}, Services: []semreg.ServiceInstance{}, Capabilities: []semreg.CapabilityInstance{},
		Fences: []semreg.GenerationFence{}, Cursors: []semreg.PublicationCursor{},
		Resources: resources, Relations: relations,
	}
	raw, err := json.Marshal(snapshot)
	inventoryMust(t, err)
	var record graphFixtureJSON
	inventoryMust(t, json.Unmarshal(raw, &record))
	delete(record, "snapshot_id")
	canonical, err := semreg.CanonicalJSON(record)
	inventoryMust(t, err)
	snapshot.SnapshotID = semreg.SnapshotID(graphFixtureDigest(string(canonical)))
	inventoryMust(t, snapshot.Validate())
	return snapshot
}

func newSemanticGraphFixture(t *testing.T) (*tree.Tree, *graphFixtureCuts) {
	t.Helper()
	return newSemanticGraphFixtureWithPorts(t, graphFixtureLifecycle{}, struct{ tree.OperationPort }{})
}

func newSemanticGraphFixtureWithPorts(t *testing.T, lifecycle tree.LifecycleSource, operations tree.OperationPort) (*tree.Tree, *graphFixtureCuts) {
	t.Helper()
	catalog, err := graph.Embedded()
	inventoryMust(t, err)
	store := graph.NewCutStore(func() time.Time { return time.Unix(0, 0) })
	equipmentA := graphFixtureResource("resource:equipment:a", "resource.equipment", semreg.AnchorEquipment)
	equipmentB := graphFixtureResource("resource:equipment:b", "resource.equipment", semreg.AnchorEquipment)
	grid := graphFixtureResource("resource:grid", "resource.grid_connection", semreg.AnchorFunction)
	site := graphFixtureResource("resource:site", "resource.site", semreg.AnchorFunction)
	relation := semreg.RelationInstance{
		Definition: semreg.DefinitionRef{Pack: semreg.PackRef{ID: "helianthus.pack.infrastructure", Version: "1.0.0"}, ID: "infrastructure.relation.site_grid_connection", Version: "1.0.0"},
		From:       site.ResourceID, To: grid.ResourceID, State: semreg.LinkQualified, Revision: "1",
		BasisClass: semreg.RelationConfiguration, Basis: []semreg.EvidenceRef{graphFixtureEvidence}, Origin: site.Origin,
	}
	first := graphFixtureSnapshot(t, "asset:a", []semreg.Resource{equipmentA, grid, site}, []semreg.RelationInstance{relation})
	second := graphFixtureSnapshot(t, "asset:b", []semreg.Resource{equipmentB}, nil)
	stage := func(members []semreg.Snapshot, missing semreg.AssetID) tree.CutView {
		cut, err := graph.AssembleCut(members, catalog, graph.CutPartial)
		inventoryMust(t, err)
		inventoryMust(t, store.Retain(cut, members, catalog))
		return tree.CutView{
			Ref: graph.CutRef{CutID: cut.ID(), Completeness: graph.CutPartial, Members: cut.Members(),
				NotIncluded: []graph.NotIncludedAsset{{AssetID: missing, Reason: graph.NotIncludedSourceUnavailable}}},
			Cut: cut, Members: members, Store: store, Catalog: catalog,
			Evaluation: semreg.EvaluationContext{EvaluatedAt: first.EvaluatedAt, EvaluateMonotonic: first.EvaluateMonotonic},
		}
	}
	older := stage([]semreg.Snapshot{second}, "asset:a")
	newest := stage([]semreg.Snapshot{second, first}, "asset:omitted")
	cuts := &graphFixtureCuts{newest: newest, views: map[semreg.Digest]tree.CutView{older.Ref.CutID: older, newest.Ref.CutID: newest}}
	runtime, err := tree.New(tree.Config{
		Registry: tree.RegistryConfig{Installation: "synthetic", Bridge: "bridge.synthetic", Incarnation: "synthetic", Lifecycle: lifecycle},
		Cuts:     cuts, Operations: operations,
	})
	inventoryMust(t, err)
	t.Cleanup(runtime.Registry.Close)
	return runtime, cuts
}

func graphFixtureServer(t *testing.T, runtime *tree.Tree) *Server {
	t.Helper()
	s, err := NewServer(&testRegistry{}, nil)
	inventoryMust(t, err)
	inventoryMust(t, RegisterSemanticGraphTools(s, runtime))
	return s
}

func callSemanticGraph(t *testing.T, s *Server, tool, arguments, wantCode string) json.RawMessage {
	t.Helper()
	params := json.RawMessage(fmt.Sprintf(`{"name":%q,"arguments":%s}`, tool, arguments))
	response := doRPC(t, s.Handler(), rpcRequest{JSONRPC: "2.0", ID: 1, Method: "tools/call", Params: params})
	if response.Error != nil {
		t.Fatalf("%s: RPC error: %+v", tool, response.Error)
	}
	result := response.Result.(map[string]any)
	if result["isError"] != (wantCode != "") {
		t.Fatalf("%s %s: unexpected result: %+v", tool, arguments, result)
	}
	var envelope struct {
		Meta struct {
			Contract    map[string]any `json:"contract"`
			Consistency map[string]any `json:"consistency"`
			Timestamp   string         `json:"data_timestamp"`
			Hash        string         `json:"data_hash"`
		} `json:"meta"`
		Data  json.RawMessage `json:"data"`
		Error json.RawMessage `json:"error"`
	}
	content := result["content"].([]any)
	if len(content) != 1 || content[0].(map[string]any)["type"] != "text" {
		t.Fatalf("bad MCP content: %+v", content)
	}
	inventoryMust(t, json.Unmarshal([]byte(content[0].(map[string]any)["text"].(string)), &envelope))
	if envelope.Meta.Contract["name"] == "" || envelope.Meta.Contract["name"] == nil ||
		envelope.Meta.Contract["major"] != float64(1) || envelope.Meta.Contract["minor"] != float64(0) ||
		envelope.Meta.Consistency["mode"] != "LIVE" {
		t.Fatalf("bad envelope metadata: %+v", envelope.Meta)
	}
	_, err := time.Parse(time.RFC3339Nano, envelope.Meta.Timestamp)
	inventoryMust(t, err)
	if want := fmt.Sprintf("%x", sha256.Sum256(envelope.Data)); envelope.Meta.Hash != want {
		t.Fatalf("hash = %q, want %q", envelope.Meta.Hash, want)
	}
	if wantCode == "" {
		if string(envelope.Error) != "null" || len(envelope.Data) == 0 || string(envelope.Data) == "null" {
			t.Fatalf("bad success envelope: %+v", envelope)
		}
	} else {
		var wireError struct{ Code string }
		inventoryMust(t, json.Unmarshal(envelope.Error, &wireError))
		if wireError.Code != wantCode || string(envelope.Data) != "null" {
			t.Fatalf("got error %s, data %s, want %s and null", envelope.Error, envelope.Data, wantCode)
		}
	}
	return envelope.Data
}

// G-POS-RET-3, G-POS-RET-1 and G-NEG-RET-1/2/6: filter, order, continue and refuse.
func TestSemanticGraphResourceList(t *testing.T) {
	runtime, cuts := newSemanticGraphFixture(t)
	s := graphFixtureServer(t, runtime)
	list := func(args string) semanticResourceList {
		t.Helper()
		var listed semanticResourceList
		inventoryMust(t, json.Unmarshal(callSemanticGraph(t, s, SemanticResourcesListTool, args, ""), &listed))
		return listed
	}
	const filter = `"kind":"resource.equipment","plane":"physical","page_size":1`
	first := list(`{` + filter + `}`)
	if first.CutID != cuts.newest.Ref.CutID || first.Completeness != graph.CutPartial ||
		!reflect.DeepEqual(first.NotIncluded, cuts.newest.Ref.NotIncluded) || !reflect.DeepEqual(first.Members, cuts.newest.Ref.Members) ||
		len(first.Resources) != 1 || first.Resources[0].ResourceID != "resource:equipment:a" || first.Resources[0].AssetID != "asset:a" || first.NextPage == "" {
		t.Fatalf("first page: %+v", first)
	}
	pinAndToken := fmt.Sprintf(`"cut_id":%q,"page_token":%q`, first.CutID, first.NextPage)
	// The previously issued cut can still be paged after the newest view changes.
	for id, view := range cuts.views {
		if id != first.CutID {
			cuts.newest = view
		}
	}
	second := list(`{` + filter + `,` + pinAndToken + `}`)
	if second.CutID != first.CutID || !reflect.DeepEqual(second.Members, first.Members) || second.NextPage != "" ||
		len(second.Resources) != 1 || second.Resources[0].ResourceID != "resource:equipment:b" || second.Resources[0].AssetID != "asset:b" {
		t.Fatalf("second page: %+v", second)
	}
	for _, c := range []struct{ name, args, code string }{
		{"kind", `{"kind":"resource.site","plane":"physical","page_size":1,` + pinAndToken + `}`, "revision_conflict"},
		{"plane", `{"kind":"resource.equipment","plane":"diagnostic","page_size":1,` + pinAndToken + `}`, "revision_conflict"},
		{"size", `{"kind":"resource.equipment","plane":"physical","page_size":2,` + pinAndToken + `}`, "revision_conflict"},
		{"cut", fmt.Sprintf(`{%s,"cut_id":%q,"page_token":%q}`, filter, cuts.newest.Ref.CutID, first.NextPage), "revision_conflict"},
		{"token", `{"page_size":1,"page_token":"malformed"}`, "invalid_value"},
		{"expired", fmt.Sprintf(`{"page_size":1,"cut_id":%q}`, graphFixtureDigest("gone cut")), "revision_conflict"},
		{"bound", fmt.Sprintf(`{"page_size":%d}`, graph.MaxPageSize+1), "bounds_exceeded"},
	} {
		t.Run(c.name, func(t *testing.T) { callSemanticGraph(t, s, SemanticResourcesListTool, c.args, c.code) })
	}
}

// G-POS-RET-4/5: the start, followed definition, direction and depth are visible.
func TestSemanticGraphTraversal(t *testing.T) {
	runtime, cuts := newSemanticGraphFixture(t)
	s := graphFixtureServer(t, runtime)
	const request = `"entry":{"kind":"resource","resource":"resource:site"},"planes":["energy"],"definitions":[{"id":"infrastructure.relation.site_grid_connection","version":"1.0.0","pack":{"id":"helianthus.pack.infrastructure","version":"1.0.0"}}],"direction":"outgoing","depth":1`
	walk := func(t *testing.T, args string) semanticTraversal {
		t.Helper()
		var walked semanticTraversal
		inventoryMust(t, json.Unmarshal(callSemanticGraph(t, s, SemanticTraverseTool, args, ""), &walked))
		return walked
	}
	full := walk(t, `{`+request+`,"page_size":2}`)
	if full.CutID != cuts.newest.Ref.CutID || !reflect.DeepEqual(full.NotIncluded, cuts.newest.Ref.NotIncluded) ||
		len(full.Resources) != 2 || full.Resources[0].ResourceID != "resource:site" || full.Resources[0].Depth != 0 ||
		full.Resources[1].ResourceID != "resource:grid" || full.Resources[1].Depth != 1 || len(full.Relations) != 1 {
		t.Fatalf("traversal: %+v", full)
	}
	for _, c := range []struct{ field, old, new, code string }{
		{"entry_kind", `"kind":"resource"`, `"kind":"invalid"`, "invalid_enum"},
		{"entry_resource", `"resource:site"`, `"resource:grid"`, ""},
		{"direction", `"outgoing"`, `"incoming"`, ""},
		{"depth", `"depth":1`, `"depth":0`, ""},
		{"definition_id", `"infrastructure.relation.site_grid_connection"`, `"pv.relation.system_inverter"`, "membership_missing"},
		{"definition_pack_id", `"helianthus.pack.infrastructure"`, `"helianthus.pack.pv"`, "membership_missing"},
		{"definition_pack_version", `"pack":{"id":"helianthus.pack.infrastructure","version":"1.0.0"}`, `"pack":{"id":"helianthus.pack.infrastructure","version":"9.0.0"}`, "membership_missing"},
		{"definition_version", `"version":"1.0.0","pack"`, `"version":"9.0.0","pack"`, "membership_missing"},
		{"planes", `["energy"]`, `["provenance"]`, "membership_missing"},
	} {
		t.Run(c.field, func(t *testing.T) {
			args := `{` + strings.Replace(request, c.old, c.new, 1) + `,"page_size":2}`
			if c.code != "" {
				callSemanticGraph(t, s, SemanticTraverseTool, args, c.code)
			} else if got := walk(t, args); len(got.Resources) != 1 {
				t.Errorf("change %s -> %s did not narrow traversal: %+v", c.old, c.new, got)
			}
		})
	}
	first := walk(t, `{`+request+`,"page_size":1}`)
	if first.NextPage == "" || len(first.Resources) != 1 || first.Resources[0].ResourceID != "resource:site" {
		t.Fatalf("first traversal page: %+v", first)
	}
	second := walk(t, fmt.Sprintf(`{%s,"page_size":1,"page_token":%q,"cut_id":%q}`, request, first.NextPage, first.CutID))
	if second.CutID != first.CutID || second.NextPage != "" || len(second.Resources) != 1 || second.Resources[0].ResourceID != "resource:grid" {
		t.Fatalf("second traversal page: %+v", second)
	}
	callSemanticGraph(t, s, SemanticTraverseTool, `{"entry":{"kind":"resource","resource":"resource:missing"},"planes":["energy"],"direction":"both","depth":1,"page_size":1}`, "dangling_reference")
	callSemanticGraph(t, s, SemanticTraverseTool, fmt.Sprintf(`{"entry":{"kind":"sites"},"planes":["energy"],"direction":"both","depth":%d,"page_size":1}`, graph.MaxTraversalDepth+1), "bounds_exceeded")
	callSemanticGraph(t, s, SemanticTraverseTool, `{"entry":{"kind":"sites"},"planes":["energy"],"direction":"both","page_size":1}`, "missing_member")
	lensWalk := `{"entry":{"kind":"lens_roots","lens":"lens.energy_flow"},"lens":"lens.energy_flow","direction":"both","depth":1,"page_size":2}`
	if got := walk(t, lensWalk); len(got.Resources) != 2 || got.Entry.Lens != "lens.energy_flow" {
		t.Fatalf("lens traversal: %+v", got)
	}
	callSemanticGraph(t, s, SemanticTraverseTool, strings.Replace(lensWalk, `"lens":"lens.energy_flow"`, `"lens":"lens.provenance"`, 1), "invalid_value")
}

// LRS-6 and G-POS-RET-2: absence of roots is successful; partial membership survives.
func TestSemanticGraphLensGolden(t *testing.T) {
	runtime, cuts := newSemanticGraphFixture(t)
	s := graphFixtureServer(t, runtime)
	const args = `{"entry":{"kind":"lens_roots"},"lens":"lens.equipment_composition","direction":"both","depth":2,"page_size":4}`
	golden, err := os.ReadFile("testdata/semantic_lens.golden.json")
	inventoryMust(t, err)
	// Substitute input identities computed from the owner records, not from MCP.
	want := strings.NewReplacer(
		"$catalog", string(cuts.newest.Catalog.Digest()), "$cut", string(cuts.newest.Ref.CutID),
		"$snapshot_a", string(cuts.newest.Ref.Members[0].SnapshotID), "$snapshot_b", string(cuts.newest.Ref.Members[1].SnapshotID),
		"$evidence", string(graphFixtureEvidence.Digest),
	).Replace(string(golden))
	var compact bytes.Buffer
	inventoryMust(t, json.Compact(&compact, []byte(want)))
	for i := 0; i < 2; i++ {
		if got := callSemanticGraph(t, s, SemanticLensGetTool, args, ""); !bytes.Equal(got, compact.Bytes()) {
			t.Fatalf("canonical lens bytes\ngot:  %s\nwant: %s", got, compact.Bytes())
		}
	}
	older := graphFixtureDigest("gone cut")
	for id := range cuts.views {
		if id != cuts.newest.Ref.CutID {
			older = id
		}
	}
	var empty graph.LensResult
	inventoryMust(t, json.Unmarshal(callSemanticGraph(t, s, SemanticLensGetTool, fmt.Sprintf(`{"entry":{"kind":"sites"},"lens":"lens.energy_flow","direction":"both","depth":2,"page_size":4,"cut_id":%q}`, older), ""), &empty))
	if empty.Cut.CutID != older || empty.Resources == nil || len(empty.Resources) != 0 {
		t.Fatalf("pinned empty lens: %+v", empty)
	}
	var first, second graph.LensResult
	paged := strings.Replace(args, `"page_size":4`, `"page_size":1`, 1)
	inventoryMust(t, json.Unmarshal(callSemanticGraph(t, s, SemanticLensGetTool, paged, ""), &first))
	if len(first.Resources) != 1 || first.Resources[0].AssetID != "asset:a" || first.NextPage == nil {
		t.Fatalf("first lens page: %+v", first)
	}
	cont := fmt.Sprintf(`,"cut_id":%q,"page_token":%q}`, first.Cut.CutID, *first.NextPage)
	inventoryMust(t, json.Unmarshal(callSemanticGraph(t, s, SemanticLensGetTool, paged[:len(paged)-1]+cont, ""), &second))
	if len(second.Resources) != 1 || second.Resources[0].AssetID != "asset:b" || second.NextPage != nil || !reflect.DeepEqual(first.Cut, second.Cut) {
		t.Fatalf("second lens page: %+v", second)
	}
	wrongDirection := strings.Replace(paged, `"direction":"both"`, `"direction":"incoming"`, 1)
	callSemanticGraph(t, s, SemanticLensGetTool, wrongDirection[:len(wrongDirection)-1]+cont, "revision_conflict")
	callSemanticGraph(t, s, SemanticLensGetTool, strings.Replace(args, "lens.equipment_composition", "lens.not_in_catalog", 1), "membership_missing")
	callSemanticGraph(t, s, SemanticLensGetTool, strings.Replace(args, `"depth":2`, `"depth":9`, 1), "bounds_exceeded")
	callSemanticGraph(t, s, SemanticLensGetTool, args[:len(args)-1]+`,"page_token":"malformed"}`, "invalid_value")
	callSemanticGraph(t, s, SemanticLensGetTool, args[:len(args)-1]+`,"not_included":[]}`, "invalid_value")
}

func TestSemanticGraphCatalog(t *testing.T) {
	runtime, cuts := newSemanticGraphFixture(t)
	s := graphFixtureServer(t, runtime)
	catalog := cuts.newest.Catalog
	data := callSemanticGraph(t, s, SemanticCatalogGetTool, `{}`, "")
	var got semanticElementCatalog
	inventoryMust(t, json.Unmarshal(data, &got))
	if got.Digest != catalog.Digest() || got.Status != catalog.Status() || !reflect.DeepEqual(got.Planes, catalog.Planes()) ||
		!reflect.DeepEqual(got.Lenses, catalog.Lenses()) || !reflect.DeepEqual(got.Entries, catalog.Elements()) {
		t.Fatal("published catalog differs from the tree's catalog")
	}
	if again := callSemanticGraph(t, s, SemanticCatalogGetTool, `{}`, ""); !bytes.Equal(again, data) {
		t.Fatal("identical catalog calls produced different data")
	}
	callSemanticGraph(t, s, SemanticCatalogGetTool, `{"planes":[]}`, "invalid_value")
	callSemanticGraph(t, s, SemanticCatalogGetTool, `null`, "invalid_value")
}

func TestSemanticGraphDescriptionsCiteContract(t *testing.T) {
	runtime, _ := newSemanticGraphFixture(t)
	server := graphFixtureServer(t, runtime)
	want := map[string]string{
		SemanticResourcesListTool: "ENT-2", SemanticTraverseTool: "TRV-1",
		SemanticLensGetTool: "API-2", SemanticCatalogGetTool: "API-1",
	}
	for _, tool := range inventoryTools(t, server, inventoryProfile{}) {
		if rule, ok := want[tool.Name]; ok {
			if !strings.Contains(tool.Description, rule) {
				t.Errorf("%s description does not cite %s", tool.Name, rule)
			}
			delete(want, tool.Name)
		}
	}
	if len(want) != 0 {
		t.Fatalf("unlisted graph tools: %v", want)
	}
}

func TestSemanticGraphNamingAndToolInventoryDelta(t *testing.T) {
	runtime, _ := newSemanticGraphFixture(t)
	s, err := NewServer(&testRegistry{}, &testInvoker{})
	inventoryMust(t, err)
	inventoryComposed(t, s)
	before := inventoryTools(t, s, inventoryProfile{})
	semanticBefore := 0
	for _, tool := range before {
		if strings.HasPrefix(tool.Name, "semantic_v1_") {
			semanticBefore++
		}
	}
	if semanticBefore != 4 {
		t.Fatalf("fixture registered %d existing semantic tools, want 4", semanticBefore)
	}
	inventoryMust(t, RegisterSemanticGraphTools(s, runtime))
	after := inventoryTools(t, s, inventoryProfile{})
	if len(after) != len(before)+6 || !reflect.DeepEqual(before, after[:len(before)]) {
		t.Fatal("existing tools/list prefix changed or the add count is wrong")
	}
	// API-1 and API-4: only graph reads join the protocol-neutral namespace.
	want := []string{"semantic_v1_resources_list", "semantic_v1_traverse", "semantic_v1_lens_get", "semantic_v1_catalog_get", "driver_member_get", "driver_method_call"}
	for i, tool := range after[len(before):] {
		if tool.Name != want[i] {
			t.Errorf("new tool %d = %q, want %q", i, tool.Name, want[i])
		}
	}
	callSemanticGraph(t, s, SemanticResourcesListTool, `{"page_size":1,"page_size":2}`, "invalid_value")
}
