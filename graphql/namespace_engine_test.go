package graphql

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/Project-Helianthus/helianthus-semreg/semreg/runtime/tree"
	semreg "github.com/Project-Helianthus/helianthus-semreg/semreg/v1"
	"github.com/Project-Helianthus/helianthus-semreg/semreg/v1/driver"
	"github.com/Project-Helianthus/helianthus-semreg/semreg/v1/graph"
	gql "github.com/graphql-go/graphql"
)

const (
	namespaceRootPath  = "/installations/CXN5bnRoZXRpYw"
	namespaceOnePath   = namespaceRootPath + "/instances/B2ZpeHR1cmU/A29uZQ"
	namespaceTwoPath   = namespaceRootPath + "/instances/B2ZpeHR1cmU/A3R3bw"
	namespaceAuxPath   = namespaceRootPath + "/instances/BW90aGVy/A29uZQ"
	namespaceMainPath  = namespaceOnePath + "/transports/BG1haW4"
	namespaceSparePath = namespaceOnePath + "/transports/BXNwYXJl"
	namespaceUnitPath  = namespaceMainPath + "/devices/BHVuaXQ"
)

type namespaceCutSource struct {
	tree.CutSource
	view tree.CutView
}

func (s namespaceCutSource) Capture(context.Context, driver.Filter) (tree.CutView, error) {
	return s.view, nil
}

type namespaceLifecycle struct{ tree.LifecycleSource }

func (namespaceLifecycle) State(tree.InstanceRef) (tree.InstanceState, error) {
	return tree.InstanceState{Name: "running", Generation: 1}, nil
}
func (namespaceLifecycle) OnFence(func(tree.Fence)) func() { return func() {} }

type namespacePrincipal struct{}

type namespaceFixtureState struct {
	runtime                                 *tree.Tree
	ctx                                     context.Context
	retainedReads, busRequests, nativeCalls int
	applied                                 []string
}

func namespaceMust(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func newNamespaceFixture(t *testing.T, operations tree.OperationPort, reverse bool) *namespaceFixtureState {
	t.Helper()
	catalog, err := graph.Embedded()
	namespaceMust(t, err)
	cut, err := graph.AssembleCut(nil, catalog, graph.CutComplete)
	namespaceMust(t, err)
	store := graph.NewCutStore(nil)
	namespaceMust(t, store.Retain(cut, nil, catalog))
	if operations == nil {
		operations = struct{ tree.OperationPort }{}
	}
	runtime, err := tree.New(tree.Config{
		Registry:   tree.RegistryConfig{Installation: "synthetic", Bridge: "bridge.synthetic", Incarnation: "incarnation.synthetic", Lifecycle: namespaceLifecycle{}},
		Cuts:       namespaceCutSource{view: tree.CutView{Ref: graph.CutRef{CutID: cut.ID(), Completeness: cut.Completeness(), Members: cut.Members()}, Cut: cut, Store: store, Catalog: catalog}},
		Operations: operations,
	})
	namespaceMust(t, err)
	t.Cleanup(runtime.Registry.Close)
	f := &namespaceFixtureState{runtime: runtime, ctx: context.WithValue(context.Background(), namespacePrincipal{}, "operator")}
	type reservation struct {
		namespace driver.Namespace
		instances []driver.InstanceKey
	}
	reservations := []reservation{
		{driver.Namespace{Owner: "fixture", Key: "sample"}, []driver.InstanceKey{"one", "two"}},
		{driver.Namespace{Owner: "other", Key: "aux"}, []driver.InstanceKey{"one"}},
	}
	if reverse {
		reservations[0], reservations[1] = reservations[1], reservations[0]
	}
	for _, r := range reservations {
		namespaceMust(t, runtime.Registry.Reserve(r.namespace, r.instances))
	}
	one, err := runtime.Registry.Bind("sample", "one", 1)
	namespaceMust(t, err)
	two, err := runtime.Registry.Bind("sample", "two", 1)
	namespaceMust(t, err)
	aux, err := runtime.Registry.Bind("aux", "one", 1)
	namespaceMust(t, err)
	main, err := one.Node(driver.NodeSpec{Local: "main", Kind: "node.transport", Contract: "contract.synthetic"})
	namespaceMust(t, err)
	spare, err := one.Node(driver.NodeSpec{Local: "spare", Kind: "node.transport", Contract: "contract.synthetic"})
	namespaceMust(t, err)
	unit, err := one.Node(driver.NodeSpec{Parent: &main, Local: "unit", Kind: "node.device", Contract: "contract.synthetic"})
	namespaceMust(t, err)
	// All read callbacks use the driver's retained observations. The same
	// fake transport counts every request from a method below.
	register := func(reg *tree.InstanceRegistry, node tree.NodeRef, local driver.MemberID, sample string, raw []byte, restricted bool) {
		scope, visibility := driver.ScopeID("diagnostics"), "public"
		if restricted {
			scope, visibility = "operator", "restricted"
		}
		namespaceMust(t, reg.ObserveMember(node, driver.MemberSpec{Local: local, Schema: "schema.counter", Units: "unit.count", Mode: "retained", Scope: scope, Visibility: visibility, Channel: "counter"}, func(ctx context.Context) (tree.Observation, error) {
			f.retainedReads++
			if ctx.Value(namespacePrincipal{}) != "operator" {
				return tree.Observation{}, fmt.Errorf("unexpected principal")
			}
			evidence := raw
			if restricted {
				evidence = nil
			} // Publication omits raw evidence for this principal.
			revision := semreg.Uint64("9007199254740993")
			return tree.Observation{Encoded: []byte(sample), Raw: evidence, Revision: &revision, Freshness: semreg.FreshnessFresh, Verification: "proven"}, nil
		}))
	}
	if reverse {
		register(one, main, "count", "7", []byte{0x07}, false)
		register(one, main, "hidden", "9", []byte{0x09}, true)
	} else {
		register(one, main, "hidden", "9", []byte{0x09}, true)
		register(one, main, "count", "7", []byte{0x07}, false)
	}
	register(one, spare, "count", "8", []byte{0x08}, false)
	register(two, two.Root(), "count", "2", []byte{0x02}, false)
	register(aux, aux.Root(), "count", "3", []byte{0x03}, false)
	register(one, unit, "count", "4", []byte{0x04}, false)
	for _, procedure := range []struct {
		node  tree.NodeRef
		local driver.MemberID
		label string
	}{
		{main, "reset", "transport"}, {one.Root(), "start", "instance"},
	} {
		namespaceMust(t, one.Method(procedure.node, driver.MethodSpec{Local: procedure.local, Schema: "schema.counter_command", Scope: "operator"}, func(ctx context.Context, input []byte) ([]byte, error) {
			f.nativeCalls++
			if ctx.Value(namespacePrincipal{}) != "operator" {
				return nil, fmt.Errorf("wrong principal")
			}
			if string(input) != "1" {
				return nil, fmt.Errorf("outside driver range")
			}
			f.busRequests++
			f.applied = append(f.applied, procedure.label)
			return []byte(procedure.label), nil
		}))
	}
	return f
}

func (f *namespaceFixtureState) schema(t *testing.T) (gql.Schema, tree.CutID) {
	t.Helper()
	cut, err := f.runtime.Capture(f.ctx, tree.CutOptions{})
	namespaceMust(t, err)
	t.Cleanup(func() { namespaceMust(t, cut.Close()) })
	semantic := semanticOperationField(NewBuilder(nil, nil))
	query, mutation, err := NamespaceFields(f.ctx, f.runtime, cut, tree.NodeRef{ID: semreg.ResourceID(namespaceRootPath[1:]), Bridge: "bridge.synthetic"}, semantic)
	namespaceMust(t, err)
	mutation[semanticOperationFieldName] = semantic // Prove the compound type is shared, not recreated under the same name.
	schema, err := gql.NewSchema(gql.SchemaConfig{
		Query:    gql.NewObject(gql.ObjectConfig{Name: "query", Fields: query}),
		Mutation: gql.NewObject(gql.ObjectConfig{Name: "mutation", Fields: mutation}),
	})
	namespaceMust(t, err)
	return schema, cut.ID()
}

func namespaceExecute(ctx context.Context, schema gql.Schema, query string, vars map[string]any) *gql.Result {
	return gql.Do(gql.Params{Schema: schema, Context: ctx, RequestString: query, VariableValues: vars})
}

func namespaceJSON(t *testing.T, result *gql.Result, want string) string {
	t.Helper()
	if len(result.Errors) != 0 {
		t.Fatalf("unexpected GraphQL errors: %+v", result.Errors)
	}
	got, err := json.Marshal(result.Data)
	namespaceMust(t, err)
	if want != "" {
		var expected any
		namespaceMust(t, json.Unmarshal([]byte(want), &expected))
		canonical, err := json.Marshal(expected)
		namespaceMust(t, err)
		if string(got) != string(canonical) {
			t.Fatalf("got  %s\nwant %s", got, canonical)
		}
	}
	return string(got)
}

func namespaceAssertCode(t *testing.T, result *gql.Result, field, code string) {
	t.Helper()
	data, _ := result.Data.(map[string]any)
	if len(result.Errors) != 1 || result.Errors[0].Extensions["code"] != code || data[field] != nil {
		t.Fatalf("%s: got %+v, want null and %s", field, result, code)
	}
}

func namespaceKey(namespace, instance, node, local string) map[string]any {
	return map[string]any{"namespace": namespace, "instance": instance, "node": node[1:], "local": local}
}

func TestNamespaceParityDetachedAttributes(t *testing.T) {
	f := newNamespaceFixture(t, nil, false)
	schema, cut := f.schema(t)
	const read = `query($key:member_key_input!) { sample_read(key:$key) {
   descriptor { ref key { namespace instance node local } schema mode scope unit visibility channel withdrawn }
   value { ref cut encoded raw status pinned unit verification freshness revision derived }
 } }`
	ref := namespaceMainPath + "/attributes/native/BnNhbXBsZQVjb3VudA"
	got := namespaceExecute(f.ctx, schema, read, map[string]any{"key": namespaceKey("sample", "one", namespaceMainPath, "count")})
	namespaceJSON(t, got, fmt.Sprintf(`{"sample_read":{
  "descriptor":{"ref":%[1]q,"key":{"namespace":"sample","instance":"one","node":%[2]q,"local":"count"},"schema":"schema.counter","mode":"retained","scope":"diagnostics","unit":"unit.count","visibility":"public","channel":"counter","withdrawn":false},
  "value":{"ref":%[1]q,"cut":%[3]q,"encoded":"Nw==","raw":"Bw==","status":"current","pinned":true,"unit":"unit.count","verification":"proven","freshness":"fresh","revision":"9007199254740993","derived":false}
 }}`, ref, namespaceMainPath[1:], cut))

	for _, tc := range []struct{ field, ns, instance, node, local, want string }{
		{"sample_read", "sample", "one", namespaceMainPath, "hidden", `{"value":{"encoded":"OQ==","raw":null},"descriptor":{"scope":"operator"}}`},
		{"sample_read", "sample", "one", namespaceSparePath, "count", `{"value":{"encoded":"OA==","raw":"CA=="},"descriptor":{"scope":"diagnostics"}}`},
		{"sample_read", "sample", "two", namespaceTwoPath, "count", `{"value":{"encoded":"Mg==","raw":"Ag=="},"descriptor":{"scope":"diagnostics"}}`},
		{"aux_read", "aux", "one", namespaceAuxPath, "count", `{"value":{"encoded":"Mw==","raw":"Aw=="},"descriptor":{"scope":"diagnostics"}}`},
	} {
		query := `query($key:member_key_input!) { ` + tc.field + `(key:$key){ value {encoded raw} descriptor{scope} } }`
		namespaceJSON(t, namespaceExecute(f.ctx, schema, query, map[string]any{"key": namespaceKey(tc.ns, tc.instance, tc.node, tc.local)}), fmt.Sprintf(`{%q:%s}`, tc.field, tc.want))
	}
	for _, tc := range []struct{ field, changed, code string }{
		{"namespace", "aux", "invalid_value"},
		{"instance", "two", "invalid_value"},
		{"node", namespaceOnePath[1:], "dangling_reference"},
		{"local", "missing", "dangling_reference"},
	} {
		t.Run(tc.field, func(t *testing.T) {
			key := namespaceKey("sample", "one", namespaceMainPath, "count")
			key[tc.field] = tc.changed
			namespaceAssertCode(t, namespaceExecute(f.ctx, schema, read, map[string]any{"key": key}), "sample_read", tc.code)
		})
	}
	if f.retainedReads != 6 || f.busRequests != 0 || f.nativeCalls != 0 {
		t.Fatalf("detached read: got %d retained reads, %d bus requests, %d method calls", f.retainedReads, f.busRequests, f.nativeCalls)
	}
}

func TestNamespaceParityPages(t *testing.T) {
	f := newNamespaceFixture(t, nil, false)
	schema, _ := f.schema(t)
	const page = `query($node:String!, $size:Int!, $token:String) {
  tree_list(node:$node,page_size:$size,page_token:$token) {
   node {descriptor{kind level namespace ref}}
   entries {node{descriptor{ref contract}} attribute{descriptor{local schema}} method{descriptor{local schema}}} next
  }
 }`
	vars := map[string]any{"node": namespaceMainPath, "size": 2}
	first := namespaceExecute(f.ctx, schema, page, vars)
	namespaceJSON(t, first, "")
	data := first.Data.(map[string]any)["tree_list"].(map[string]any)
	token, ok := data["next"].(string)
	if !ok || token == "" {
		t.Fatalf("no continuation: %v", data)
	}
	namespaceJSON(t, first, fmt.Sprintf(`{"tree_list":{"node":{"descriptor":{"kind":"node","level":"transport","namespace":"sample","ref":%q}},"entries":[
  {"node":null,"attribute":{"descriptor":{"local":"count","schema":"schema.counter"}},"method":null},
  {"node":null,"attribute":{"descriptor":{"local":"hidden","schema":"schema.counter"}},"method":null}],"next":%q}}`, namespaceMainPath, token))
	vars["token"] = token
	namespaceJSON(t, namespaceExecute(f.ctx, schema, page, vars), fmt.Sprintf(`{"tree_list":{"node":{"descriptor":{"kind":"node","level":"transport","namespace":"sample","ref":%q}},"entries":[
  {"node":{"descriptor":{"ref":%q,"contract":"contract.synthetic"}},"attribute":null,"method":null},
  {"node":null,"attribute":null,"method":{"descriptor":{"local":"reset","schema":"schema.counter_command"}}}],"next":null}}`, namespaceMainPath, namespaceUnitPath))
	vars["token"] = "not-a-page"
	namespaceAssertCode(t, namespaceExecute(f.ctx, schema, page, vars), "tree_list", "invalid_value")
	vars["size"], vars["token"] = 0, nil
	namespaceAssertCode(t, namespaceExecute(f.ctx, schema, page, vars), "tree_list", "invalid_value")
}

func TestNamespacePublishedGraphAcrossLifecycles(t *testing.T) {
	serve := func(reverse bool) string {
		f := newNamespaceFixture(t, nil, reverse)
		schema, _ := f.schema(t)
		// Each node is a separate tree page. Select the stable published graph,
		// including isolated instances, child nodes and every native member key.
		nodes := []string{namespaceRootPath, namespaceOnePath, namespaceTwoPath, namespaceAuxPath, namespaceMainPath, namespaceSparePath, namespaceUnitPath}
		var query strings.Builder
		query.WriteString("{")
		for i, node := range nodes {
			fmt.Fprintf(&query, `n%d:tree_list(node:%q,page_size:100){node{descriptor{ref namespace kind level}} entries{
    node{descriptor{ref namespace kind level}} attribute{descriptor{key{namespace instance node local} schema unit}} method{descriptor{key{namespace instance node local} schema}}
   }}`, i, node)
		}
		query.WriteString("}")
		return namespaceJSON(t, namespaceExecute(f.ctx, schema, query.String(), nil), "")
	}
	if first, second := serve(false), serve(true); first != second {
		t.Fatalf("published graph changed after restart:\n%s\n%s", first, second)
	}
}
