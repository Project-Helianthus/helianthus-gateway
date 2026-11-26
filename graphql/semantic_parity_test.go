package graphql

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/Project-Helianthus/helianthus-semreg/semreg/runtime/tree"
	semreg "github.com/Project-Helianthus/helianthus-semreg/semreg/v1"
	"github.com/Project-Helianthus/helianthus-semreg/semreg/v1/driver"
	"github.com/Project-Helianthus/helianthus-semreg/semreg/v1/graph"
	"github.com/Project-Helianthus/helianthus-semreg/semreg/v1/operation"
)

type namespaceParityPort struct {
	tree.OperationPort
	t                         *testing.T
	request                   driver.OperationRequest
	result                    driver.OperationResult
	prepares, calls, outcomes int
	deferred                  bool
}

func (p *namespaceParityPort) Prepare(ctx context.Context, cut graph.CutRef, ref semreg.DefinitionRef, input tree.Input) (tree.PortPrepared, error) {
	p.t.Helper()
	p.prepares++
	if ctx.Value(namespacePrincipal{}) != "operator" || cut.CutID == "" || ref != p.request.Operation || input.Request == nil || !reflect.DeepEqual(*input.Request, p.request) || input.Encoded != nil {
		p.t.Errorf("unexpected operation dispatch: caller=%v cut=%v ref=%v input=%+v", ctx.Value(namespacePrincipal{}), cut, ref, input.Request)
	}
	return tree.PortPrepared{ID: "prepared", Request: p.request}, nil
}

func (p *namespaceParityPort) Call(_ context.Context, prepared tree.PortPrepared, _ tree.CallOptions) (tree.PortReceipt, error) {
	p.calls++
	if prepared.ID != "prepared" {
		p.t.Errorf("called a different preparation: %v", prepared.ID)
	}
	receipt := tree.PortReceipt{ID: "operation-receipt"}
	if !p.deferred {
		receipt.Result = &p.result
	}
	return receipt, nil
}

func (p *namespaceParityPort) Outcome(_ context.Context, receipt tree.ReceiptID) (driver.OperationResult, error) {
	p.outcomes++
	if receipt != "operation-receipt" {
		p.t.Errorf("read a different outcome: %v", receipt)
	}
	return p.result, nil
}

func TestSemanticNamespaceParityOperationPort(t *testing.T) {
	definition := semreg.DefinitionRef{Pack: semreg.PackRef{ID: "helianthus.pack.evse", Version: "1.0.0"}, ID: "evse.operation.set_allocated_current", Version: "1.0.0"}
	fixture := &namespaceParityPort{t: t, request: driver.OperationRequest{
		AssetID: "asset:synthetic", BindingID: "binding:synthetic", CapabilityInstanceID: "capability:synthetic",
		Operation: definition, Arguments: []semreg.TypedField{}, Preconditions: []operation.Precondition{},
		ExpectedSemanticRevision: "1", ExpectedCapabilityRevision: "2", ExpectedCapabilityInstanceRevision: "3",
		ExpectedDriverGeneration: "4", ExpectedSourceEpochID: "epoch:synthetic", Timeout: time.Second,
	}}
	f := newNamespaceFixture(t, fixture, false)
	// The plan permits a driver to reserve "tree". Its native call shares
	// the name of the tree verb without shadowing the semantic call form.
	namespaceMust(t, f.runtime.Registry.Reserve(driver.Namespace{Owner: "fixture", Key: "tree"}, []driver.InstanceKey{"one"}))
	bound, err := f.runtime.Registry.Bind("tree", "one", 1)
	namespaceMust(t, err)
	treeCalls := 0
	namespaceMust(t, bound.Method(bound.Root(), driver.MethodSpec{Local: "ping", Schema: "schema.ping", Scope: "operator"}, func(context.Context, []byte) ([]byte, error) {
		treeCalls++
		return []byte("ok"), nil
	}))
	schema, _ := f.schema(t)
	key := namespaceKey("tree", "one", namespaceOnePath, "ping")
	const native = `mutation($key:member_key_input!) { tree_call(key:$key) { native error status } }`
	namespaceJSON(t, namespaceExecute(f.ctx, schema, native, map[string]any{"key": key}), `{"tree_call":{"native":"b2s=","error":null,"status":null}}`)
	const mixed = `mutation($key:member_key_input!) { tree_call(key:$key,method:"none") { native } }`
	namespaceAssertCode(t, namespaceExecute(f.ctx, schema, mixed, map[string]any{"key": key}), "tree_call", "invalid_value")
	if treeCalls != 1 {
		t.Fatalf("got %d tree namespace calls, want one", treeCalls)
	}
	segment, err := tree.DefinitionSegment(definition)
	namespaceMust(t, err)
	method := namespaceRootPath + "/methods/sem/" + segment
	makeRequest := func(d semreg.DefinitionRef) map[string]any {
		return map[string]any{
			"asset_id": "asset:synthetic", "binding_id": "binding:synthetic", "capability_instance_id": "capability:synthetic",
			"operation": map[string]any{"id": string(d.ID), "version": string(d.Version), "pack": map[string]any{"id": string(d.Pack.ID), "version": string(d.Pack.Version)}},
			"arguments": []any{}, "preconditions": []any{}, "expected_driver_generation": "4", "expected_source_epoch_id": "epoch:synthetic",
			"expected_semantic_revision": "1", "expected_capability_revision": "2", "expected_capability_instance_revision": "3", "timeout_ms": 1000,
		}
	}
	const call = `mutation Call($method:String!, $request:semantic_operation_input!) {
  tree_call(method:$method,request:$request) { native error status kernel_outcome delivery acknowledgement readback_relation reason
    compound_execution { partial steps { name attempted delivery acknowledgement readback_relation reason } } }
 }`
	for _, tc := range []struct {
		fixture, want string
		deferred      bool
	}{
		{`{"status":"accepted","delivery":"sent"}`, `{"native":null,"error":null,"status":"accepted","kernel_outcome":null,"delivery":"sent","acknowledgement":null,"readback_relation":null,"reason":null,"compound_execution":null}`, false},
		{`{"status":"rejected","reason":"outside driver range"}`, `{"native":null,"error":null,"status":"rejected","kernel_outcome":null,"delivery":null,"acknowledgement":null,"readback_relation":null,"reason":"outside driver range","compound_execution":null}`, false},
		{`{"status":"unknown","kernel_outcome":"indeterminate","compound_execution":{"partial":true,"steps":[
    {"point":"point.first","attempted":true,"delivery":"sent","acknowledgement":"accepted","readback_relation":"confirms"},
    {"point":"point.second","attempted":true,"delivery":"unknown","reason":"delivery ambiguous"},
    {"point":"point.third","attempted":false}]}}`,
			`{"native":null,"error":null,"status":"unknown","kernel_outcome":"indeterminate","delivery":null,"acknowledgement":null,"readback_relation":null,"reason":null,
    "compound_execution":{"partial":true,"steps":[
     {"name":"point.first","attempted":true,"delivery":"sent","acknowledgement":"accepted","readback_relation":"confirms","reason":null},
     {"name":"point.second","attempted":true,"delivery":"unknown","acknowledgement":null,"readback_relation":null,"reason":"delivery ambiguous"},
     {"name":"point.third","attempted":false,"delivery":null,"acknowledgement":null,"readback_relation":null,"reason":null}]}}`, true},
	} {
		fixture.result = driver.OperationResult{}
		namespaceMust(t, json.Unmarshal([]byte(tc.fixture), &fixture.result))
		fixture.deferred = tc.deferred
		namespaceJSON(t, namespaceExecute(f.ctx, schema, call, map[string]any{"method": method, "request": makeRequest(definition)}), `{"tree_call":`+tc.want+`}`)
	}
	for _, tc := range []struct {
		name   string
		change func(*semreg.DefinitionRef)
	}{
		{"id", func(d *semreg.DefinitionRef) { d.ID = "synthetic.other" }},
		{"version", func(d *semreg.DefinitionRef) { d.Version = "2.0.0" }},
		{"pack_id", func(d *semreg.DefinitionRef) { d.Pack.ID = "helianthus.pack.other" }},
		{"pack_version", func(d *semreg.DefinitionRef) { d.Pack.Version = "2.0.0" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := definition
			tc.change(&bad)
			namespaceAssertCode(t, namespaceExecute(f.ctx, schema, call, map[string]any{"method": method, "request": makeRequest(bad)}), "tree_call", "invalid_value")
		})
	}
	for _, tc := range []struct{ method, code string }{
		{namespaceOnePath + "/methods/sem/" + segment, "dangling_reference"},
		{namespaceRootPath + "/attributes/sem/" + segment, "invalid_value"},
		{namespaceMainPath + "/methods/native/BnNhbXBsZQVyZXNldA", "invalid_value"},
	} {
		namespaceAssertCode(t, namespaceExecute(f.ctx, schema, call, map[string]any{"method": tc.method, "request": makeRequest(definition)}), "tree_call", tc.code)
	}
	if fixture.prepares != 3 || fixture.calls != 3 || fixture.outcomes != 1 || f.nativeCalls != 0 || f.busRequests != 0 {
		t.Fatalf("operation route: got %d prepares, %d calls, %d outcome lookups, %d native calls, %d bus requests", fixture.prepares, fixture.calls, fixture.outcomes, f.nativeCalls, f.busRequests)
	}
}
