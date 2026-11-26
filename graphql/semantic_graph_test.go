package graphql

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Project-Helianthus/helianthus-semreg/semreg/runtime/tree"
	semreg "github.com/Project-Helianthus/helianthus-semreg/semreg/v1"
	"github.com/Project-Helianthus/helianthus-semreg/semreg/v1/driver"
	"github.com/Project-Helianthus/helianthus-semreg/semreg/v1/graph"
	graphqlgo "github.com/graphql-go/graphql"
)

type semanticGraphCuts struct {
	tree.CutSource
	newest, older tree.CutView
}

func (f *semanticGraphCuts) Capture(context.Context, driver.Filter) (tree.CutView, error) {
	return f.newest, nil
}

func (f *semanticGraphCuts) Lookup(_ context.Context, ref graph.CutRef) (tree.CutView, error) {
	for _, view := range []tree.CutView{f.newest, f.older} {
		if view.Ref.CutID == ref.CutID {
			return view, nil
		}
	}
	return tree.CutView{}, &tree.ExpiredError{CutID: ref.CutID, ExpiredAt: time.Unix(1, 0)}
}

type semanticGraphLifecycle struct{ tree.LifecycleSource }

func (semanticGraphLifecycle) OnFence(func(tree.Fence)) func() { return func() {} }

type semanticGraphFixtureRecord map[string]json.RawMessage

func (semanticGraphFixtureRecord) Validate() error { return nil }

func semanticGraphMust(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func semanticGraphDigest(label string) semreg.Digest {
	return semreg.Digest(fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(label))))
}

var semanticGraphEvidence = semreg.EvidenceRef{
	Owner: "owner.synthetic", Kind: "evidence.synthetic", Digest: semanticGraphDigest("synthetic graph evidence"),
	Contract: "synthetic/v1", Access: semreg.EvidenceAccessPublic, Redaction: semreg.RedactionNone,
}

func semanticGraphResource(id semreg.ResourceID, kind semreg.DefinitionID, class semreg.AnchorClass) semreg.Resource {
	slot := "slot-" + string(id)
	return semreg.Resource{
		ResourceID: id, Kind: kind, State: semreg.ResourceCurrent, Revision: "1",
		Anchor: semreg.ResourceAnchor{Class: class, Basis: semreg.AnchorOperatorSlot, Slot: &slot, Evidence: []semreg.EvidenceRef{semanticGraphEvidence}},
		Origin: semreg.OriginRef{OriginID: semreg.OriginID("origin:" + id), Kind: semreg.OriginOperator, Evidence: []semreg.EvidenceRef{semanticGraphEvidence}},
	}
}

func semanticGraphSnapshot(t *testing.T, asset semreg.AssetID, resources []semreg.Resource, relations []semreg.RelationInstance) semreg.Snapshot {
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
	semanticGraphMust(t, err)
	var record semanticGraphFixtureRecord
	semanticGraphMust(t, json.Unmarshal(raw, &record))
	delete(record, "snapshot_id")
	canonical, err := semreg.CanonicalJSON(record)
	semanticGraphMust(t, err)
	snapshot.SnapshotID = semreg.SnapshotID(semanticGraphDigest(string(canonical)))
	semanticGraphMust(t, snapshot.Validate())
	return snapshot
}

// Same synthetic owner records as the GG02 graph vectors. Retain both cuts so
// that requests can check CUT-1/CUT-2 without racing a wall clock.
func newSemanticGraphFixture(t *testing.T) (graphqlgo.Schema, context.Context, *semanticGraphCuts) {
	t.Helper()
	catalog, err := graph.Embedded()
	semanticGraphMust(t, err)
	store := graph.NewCutStore(func() time.Time { return time.Unix(0, 0) })
	equipmentA := semanticGraphResource("resource:equipment:a", "resource.equipment", semreg.AnchorEquipment)
	equipmentB := semanticGraphResource("resource:equipment:b", "resource.equipment", semreg.AnchorEquipment)
	grid := semanticGraphResource("resource:grid", "resource.grid_connection", semreg.AnchorFunction)
	site := semanticGraphResource("resource:site", "resource.site", semreg.AnchorFunction)
	relation := semreg.RelationInstance{
		Definition: semreg.DefinitionRef{Pack: semreg.PackRef{ID: "helianthus.pack.infrastructure", Version: "1.0.0"}, ID: "infrastructure.relation.site_grid_connection", Version: "1.0.0"},
		From:       site.ResourceID, To: grid.ResourceID, State: semreg.LinkQualified, Revision: "1",
		BasisClass: semreg.RelationConfiguration, Basis: []semreg.EvidenceRef{semanticGraphEvidence}, Origin: site.Origin,
	}
	first := semanticGraphSnapshot(t, "asset:a", []semreg.Resource{equipmentA, grid, site}, []semreg.RelationInstance{relation})
	second := semanticGraphSnapshot(t, "asset:b", []semreg.Resource{equipmentB}, nil)
	stage := func(members []semreg.Snapshot, missing semreg.AssetID) tree.CutView {
		cut, err := graph.AssembleCut(members, catalog, graph.CutPartial)
		semanticGraphMust(t, err)
		semanticGraphMust(t, store.Retain(cut, members, catalog))
		return tree.CutView{
			Ref: graph.CutRef{CutID: cut.ID(), Completeness: graph.CutPartial, Members: cut.Members(),
				NotIncluded: []graph.NotIncludedAsset{{AssetID: missing, Reason: graph.NotIncludedSourceUnavailable}}},
			Cut: cut, Members: members, Store: store, Catalog: catalog,
			Evaluation: semreg.EvaluationContext{EvaluatedAt: first.EvaluatedAt, EvaluateMonotonic: first.EvaluateMonotonic},
		}
	}
	cuts := &semanticGraphCuts{
		older:  stage([]semreg.Snapshot{second}, "asset:a"),
		newest: stage([]semreg.Snapshot{second, first}, "asset:omitted"),
	}
	runtime, err := tree.New(tree.Config{
		Registry: tree.RegistryConfig{Installation: "synthetic", Bridge: "bridge.synthetic", Incarnation: "synthetic", Lifecycle: semanticGraphLifecycle{}},
		Cuts:     cuts, Operations: struct{ tree.OperationPort }{},
	})
	semanticGraphMust(t, err)
	t.Cleanup(runtime.Registry.Close)
	schema, err := NewQuerySchema(NewBuilder(nil, nil))
	semanticGraphMust(t, err)
	return schema, WithSemanticGraph(context.Background(), runtime), cuts
}

func querySemanticGraph(t *testing.T, schema graphqlgo.Schema, ctx context.Context, field, args, wantCode string) string {
	t.Helper()
	selection := field
	if args != "" {
		selection += "(" + args + ")"
	}
	result := graphqlgo.Do(graphqlgo.Params{Schema: schema, Context: ctx, RequestString: "{ " + selection + " }"})
	data, _ := result.Data.(map[string]any)
	if wantCode != "" {
		value, present := data[field]
		if len(result.Errors) != 1 || result.Errors[0].Extensions["code"] != wantCode || !present || value != nil ||
			!reflect.DeepEqual(result.Errors[0].Path, []any{field}) {
			t.Fatalf("%s: data=%v errors=%+v, want %s and a null field", selection, data, result.Errors, wantCode)
		}
		return ""
	}
	if len(result.Errors) != 0 {
		t.Fatalf("%s: %+v", selection, result.Errors)
	}
	got, ok := data[field].(string)
	if !ok || !json.Valid([]byte(got)) {
		t.Fatalf("%s: expected a canonical record string, got %v", selection, data[field])
	}
	return got
}

func decodeSemanticGraph[T any](t *testing.T, raw string) T {
	t.Helper()
	var got T
	semanticGraphMust(t, json.Unmarshal([]byte(raw), &got))
	return got
}

func matchSemanticGraphBytes(t *testing.T, got, want string) {
	t.Helper()
	if got != want {
		t.Fatalf("canonical graph record\ngot:  %s\nwant: %s", got, want)
	}
}

const semanticGraphMembersJSON = `[{"asset_id":"asset:a","snapshot_id":%q},{"asset_id":"asset:b","snapshot_id":%q}]`
const semanticGraphOmissionsJSON = `[{"asset_id":"asset:omitted","reason":"source_unavailable"}]`

// G-POS-RET-1/2/3 and G-NEG-RET-1/2/6/8: order, home assets, omissions,
// cut pinning and the complete listing argument boundary.
func TestSemanticGraphResources(t *testing.T) {
	schema, ctx, cuts := newSemanticGraphFixture(t)
	list := func(t *testing.T, args, code string) string {
		t.Helper()
		return querySemanticGraph(t, schema, ctx, "semantic_resources", args, code)
	}
	view := cuts.newest
	members := fmt.Sprintf(semanticGraphMembersJSON, view.Ref.Members[0].SnapshotID, view.Ref.Members[1].SnapshotID)
	const filter = `kind:"resource.equipment", plane:"physical"`
	const entry = `{"asset_id":"asset:%s","kind":"resource.equipment","planes":["physical"],"resource_id":"resource:equipment:%s"}`
	const record = `{"catalog":%q,"completeness":"partial","cut_id":%q,"members":%s%s,"not_included":%s,"resources":[%s]}`
	wantEntry := func(id string) string { return fmt.Sprintf(entry, id, id) }
	want := func(cursor, entries string) string {
		return fmt.Sprintf(record, view.Catalog.Digest(), view.Ref.CutID, members, cursor, semanticGraphOmissionsJSON, entries)
	}
	matchSemanticGraphBytes(t, list(t, filter+`,page_size:2`, ""), want("", wantEntry("a")+","+wantEntry("b")))
	const paged = filter + `,page_size:1`
	first := list(t, paged, "")
	page := decodeSemanticGraph[graph.ResourcePage](t, first)
	if page.NextPage == "" {
		t.Fatal("first page lacks continuation")
	}
	matchSemanticGraphBytes(t, first, want(fmt.Sprintf(`,"next_page":%q`, page.NextPage), wantEntry("a")))
	pin := fmt.Sprintf(`,cut_id:%q,page_token:%q`, view.Ref.CutID, page.NextPage)
	cuts.newest, cuts.older = cuts.older, cuts.newest
	matchSemanticGraphBytes(t, list(t, paged+pin, ""), want("", wantEntry("b")))
	for _, tc := range []struct{ name, args, code string }{
		{"kind", strings.Replace(paged, "resource.equipment", "resource.site", 1) + pin, "revision_conflict"},
		{"plane", strings.Replace(paged, "physical", "diagnostic", 1) + pin, "revision_conflict"},
		{"page_size", filter + `,page_size:2` + pin, "revision_conflict"},
		{"cut_id", paged + fmt.Sprintf(`,cut_id:%q,page_token:%q`, cuts.newest.Ref.CutID, page.NextPage), "revision_conflict"},
		{"expired", fmt.Sprintf(`page_size:1,cut_id:%q`, semanticGraphDigest("gone cut")), "revision_conflict"},
		{"page_token", `page_size:1,page_token:"malformed"`, "invalid_value"},
		{"bound", fmt.Sprintf(`page_size:%d`, graph.MaxPageSize+1), "bounds_exceeded"},
	} {
		t.Run(tc.name, func(t *testing.T) { list(t, tc.args, tc.code) })
	}
}

func TestSemanticGraphTraversal(t *testing.T) {
	schema, ctx, cuts := newSemanticGraphFixture(t)
	walk := func(t *testing.T, args, code string) semanticGraphTraversal {
		t.Helper()
		raw := querySemanticGraph(t, schema, ctx, "semantic_traversal", args, code)
		if code != "" {
			return semanticGraphTraversal{}
		}
		return decodeSemanticGraph[semanticGraphTraversal](t, raw)
	}
	const request = `entry:{kind:"resource",resource:"resource:site"},planes:["energy"],definitions:[{id:"infrastructure.relation.site_grid_connection",version:"1.0.0",pack:{id:"helianthus.pack.infrastructure",version:"1.0.0"}}],direction:"outgoing",depth:1`
	view := cuts.newest
	full := walk(t, request+`,page_size:2`, "")
	memberPlanes := []graph.PlaneID{"physical", "energy", "diagnostic"}
	want := []graph.ReachedResource{
		{ResourceID: "resource:site", Kind: "resource.site", AssetID: "asset:a", Depth: 0, Planes: memberPlanes},
		{ResourceID: "resource:grid", Kind: "resource.grid_connection", AssetID: "asset:a", Depth: 1, Planes: memberPlanes},
	}
	wantRelation := graph.CutRelation{
		AssetID: "asset:a", From: "resource:site", To: "resource:grid", State: semreg.LinkQualified, Coherence: "coherent",
		Definition: semreg.DefinitionRef{Pack: semreg.PackRef{ID: "helianthus.pack.infrastructure", Version: "1.0.0"}, ID: "infrastructure.relation.site_grid_connection", Version: "1.0.0"},
	}
	if full.CutID != view.Ref.CutID || full.Catalog != view.Catalog.Digest() || full.Completeness != graph.CutPartial ||
		!reflect.DeepEqual(full.Members, view.Ref.Members) || !reflect.DeepEqual(full.NotIncluded, view.Ref.NotIncluded) ||
		!reflect.DeepEqual(full.Resources, want) || !reflect.DeepEqual(full.Relations, []graph.CutRelation{wantRelation}) || full.NextPage != "" {
		t.Fatalf("outgoing traversal: %+v", full)
	}
	for _, tc := range []struct{ name, old, edit, code string }{
		{"entry_kind", `kind:"resource"`, `kind:"invalid"`, "invalid_enum"},
		{"entry_resource", `"resource:site"`, `"resource:missing"`, "dangling_reference"},
		{"direction", `"outgoing"`, `"incoming"`, ""},
		{"depth", `depth:1`, `depth:0`, ""},
		{"definition_id", `"infrastructure.relation.site_grid_connection"`, `"pv.relation.system_inverter"`, "membership_missing"},
		{"definition_version", `version:"1.0.0",pack`, `version:"9.0.0",pack`, "membership_missing"},
		{"definition_pack_id", `"helianthus.pack.infrastructure"`, `"helianthus.pack.pv"`, "membership_missing"},
		{"definition_pack_version", `version:"1.0.0"}}`, `version:"9.0.0"}}`, "membership_missing"},
		{"planes", `["energy"]`, `["provenance"]`, "membership_missing"},
		{"depth_bound", `depth:1`, fmt.Sprintf(`depth:%d`, graph.MaxTraversalDepth+1), "bounds_exceeded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			page := walk(t, strings.Replace(request, tc.old, tc.edit, 1)+`,page_size:2`, tc.code)
			if tc.code == "" && !reflect.DeepEqual(page.Resources, want[:1]) {
				t.Fatalf("changed traversal should reach only the entry: %+v", page)
			}
			if tc.name == "depth" && !reflect.DeepEqual(page.Frontier, []graph.FrontierEntry{{ResourceID: "resource:site", Reason: graph.FrontierDepthLimit, NotFollowed: 1}}) {
				t.Fatalf("depth-limited frontier: %+v", page.Frontier)
			}
		})
	}
	const lensWalk = `entry:{kind:"lens_roots",lens:"lens.energy_flow"},lens:"lens.energy_flow",direction:"both",depth:1,page_size:2`
	if page := walk(t, lensWalk, ""); len(page.Resources) != 2 || page.Entry.Lens != "lens.energy_flow" {
		t.Fatalf("lens-root traversal: %+v", page)
	}
	walk(t, strings.Replace(lensWalk, `lens:"lens.energy_flow"`, `lens:"lens.provenance"`, 1), "invalid_value")
	walk(t, strings.Replace(lensWalk, `},lens:"lens.energy_flow"`, `},lens:"lens.not_in_catalog"`, 1), "invalid_value")
	walk(t, `entry:{kind:"sites"},lens:"lens.not_in_catalog",direction:"both",depth:1,page_size:2`, "membership_missing")

	const paged = request + `,page_size:1`
	first := walk(t, paged, "")
	if first.NextPage == "" || !reflect.DeepEqual(first.Resources, want[:1]) {
		t.Fatalf("first traversal page: %+v", first)
	}
	cuts.newest, cuts.older = cuts.older, cuts.newest
	pin := fmt.Sprintf(`,cut_id:%q,page_token:%q`, first.CutID, first.NextPage)
	second := walk(t, paged+pin, "")
	if second.CutID != first.CutID || !reflect.DeepEqual(second.Resources, want[1:]) || second.NextPage != "" {
		t.Fatalf("second traversal page: %+v", second)
	}
	walk(t, request+`,page_size:2`+pin, "revision_conflict")
	walk(t, paged+fmt.Sprintf(`,cut_id:%q,page_token:%q`, cuts.newest.Ref.CutID, first.NextPage), "revision_conflict")
	walk(t, paged+`,page_token:"malformed"`, "invalid_value")
	if empty := walk(t, `entry:{kind:"sites"},planes:["energy"],direction:"both",depth:1,page_size:1`, ""); empty.EntryStatus != "no_site" || empty.Resources == nil || len(empty.Resources) != 0 {
		t.Fatalf("empty traversal: %+v", empty)
	}
}

// Canonical G-POS-RET-1/2 and LRS-6 for the GG02 synthetic equipment cut.
func TestSemanticGraphLens(t *testing.T) {
	const args = `entry:{kind:"lens_roots"},lens:"lens.equipment_composition",direction:"both",depth:2,page_size:4`
	const evidence = `[{"access":"public","contract":"synthetic/v1","digest":%q,"kind":"evidence.synthetic","owner":"owner.synthetic","redaction":"none"}]`
	const resource = `{"asset_id":"asset:%[1]s","depth":0,"planes":["physical"],"resource":{"anchor":{"basis":"operator_slot","class":"equipment","evidence":%[2]s,"slot":"slot-resource:equipment:%[1]s"},"kind":"resource.equipment","origin":{"evidence":%[2]s,"kind":"operator","origin_id":"origin:resource:equipment:%[1]s"},"resource_id":"resource:equipment:%[1]s","revision":"1","state":"current"}}`
	const record = `{"catalog":%q,"contract":"helianthus.semantic.lens-result/v1","cut":{"completeness":"partial","cut_id":%q,"members":%s,"not_included":%s},"entry":{"kind":"lens_roots","lens":"lens.equipment_composition"},"lens":"lens.equipment_composition","resources":[%s,%s]}`
	var published string
	for i := range 2 {
		schema, ctx, cuts := newSemanticGraphFixture(t)
		view := cuts.newest
		members := fmt.Sprintf(semanticGraphMembersJSON, view.Ref.Members[0].SnapshotID, view.Ref.Members[1].SnapshotID)
		ev := fmt.Sprintf(evidence, semanticGraphEvidence.Digest)
		want := fmt.Sprintf(record, view.Catalog.Digest(), view.Ref.CutID, members, semanticGraphOmissionsJSON, fmt.Sprintf(resource, "a", ev), fmt.Sprintf(resource, "b", ev))
		got := querySemanticGraph(t, schema, ctx, "semantic_lens", args, "")
		matchSemanticGraphBytes(t, got, want)
		if i == 0 {
			published = got
		} else {
			matchSemanticGraphBytes(t, got, published)
		}
	}

	schema, ctx, cuts := newSemanticGraphFixture(t)
	lens := func(t *testing.T, args, code string) graph.LensResult {
		t.Helper()
		raw := querySemanticGraph(t, schema, ctx, "semantic_lens", args, code)
		if code != "" {
			return graph.LensResult{}
		}
		return decodeSemanticGraph[graph.LensResult](t, raw)
	}
	const paged = `entry:{kind:"lens_roots",lens:"lens.equipment_composition"},lens:"lens.equipment_composition",direction:"both",depth:2,page_size:1`
	first := lens(t, paged, "")
	if first.NextPage == nil || len(first.Resources) != 1 || first.Resources[0].AssetID != "asset:a" {
		t.Fatalf("first lens page: %+v", first)
	}
	empty := lens(t, fmt.Sprintf(`entry:{kind:"sites"},lens:"lens.energy_flow",direction:"both",depth:1,page_size:1,cut_id:%q`, cuts.older.Ref.CutID), "")
	if empty.Cut.CutID != cuts.older.Ref.CutID || empty.Resources == nil || len(empty.Resources) != 0 {
		t.Fatalf("pinned empty lens: %+v", empty)
	}
	cuts.newest, cuts.older = cuts.older, cuts.newest
	pin := fmt.Sprintf(`,cut_id:%q,page_token:%q`, first.Cut.CutID, *first.NextPage)
	second := lens(t, paged+pin, "")
	if !reflect.DeepEqual(first.Cut, second.Cut) || second.NextPage != nil || len(second.Resources) != 1 || second.Resources[0].AssetID != "asset:b" {
		t.Fatalf("second lens page: %+v", second)
	}
	for _, tc := range []struct{ name, args, code string }{
		{"entry_kind", strings.Replace(paged, `kind:"lens_roots",lens:"lens.equipment_composition"`, `kind:"sites"`, 1) + pin, "revision_conflict"},
		{"entry_resource", strings.Replace(paged, `kind:"lens_roots",lens:"lens.equipment_composition"`, `kind:"resource",resource:"resource:missing"`, 1) + pin, "dangling_reference"},
		{"entry_lens", strings.Replace(paged, `lens:"lens.equipment_composition"`, `lens:"lens.provenance"`, 1) + pin, "invalid_value"},
		{"lens", strings.Replace(args, "lens.equipment_composition", "lens.not_in_catalog", 1), "membership_missing"},
		{"direction", strings.Replace(paged, `"both"`, `"incoming"`, 1) + pin, "revision_conflict"},
		{"depth", strings.Replace(paged, `depth:2`, `depth:1`, 1) + pin, "revision_conflict"},
		{"page_size", strings.Replace(paged, `page_size:1`, `page_size:2`, 1) + pin, "revision_conflict"},
		{"cut_id", paged + fmt.Sprintf(`,cut_id:%q,page_token:%q`, cuts.newest.Ref.CutID, *first.NextPage), "revision_conflict"},
		{"page_token", args + `,page_token:"malformed"`, "invalid_value"},
		{"bound", strings.Replace(args, `depth:2`, `depth:9`, 1), "bounds_exceeded"},
	} {
		t.Run(tc.name, func(t *testing.T) { lens(t, tc.args, tc.code) })
	}
}

func TestSemanticGraphElementCatalog(t *testing.T) {
	schema, ctx, cuts := newSemanticGraphFixture(t)
	const field = "semantic_element_catalog"
	got := querySemanticGraph(t, schema, ctx, field, "", "")
	catalog := cuts.newest.Catalog
	want := map[string]any{"digest": catalog.Digest(), "entries": catalog.Elements(), "lenses": catalog.Lenses(), "planes": catalog.Planes(), "status": catalog.Status()}
	bytes, err := json.Marshal(want)
	semanticGraphMust(t, err)
	canonical, err := semreg.CanonicalJSON(decodeSemanticGraph[semanticGraphFixtureRecord](t, string(bytes)))
	semanticGraphMust(t, err)
	matchSemanticGraphBytes(t, got, string(canonical))
	querySemanticGraph(t, schema, nil, field, "", "UNAVAILABLE")
}
