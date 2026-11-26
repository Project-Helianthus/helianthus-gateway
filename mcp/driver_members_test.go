package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/Project-Helianthus/helianthus-semreg/semreg/runtime/tree"
	semreg "github.com/Project-Helianthus/helianthus-semreg/semreg/v1"
	"github.com/Project-Helianthus/helianthus-semreg/semreg/v1/driver"
	"github.com/Project-Helianthus/helianthus-semreg/semreg/v1/graph"
)

type driverFixtureLifecycle struct{ graphFixtureLifecycle }

func (driverFixtureLifecycle) State(tree.InstanceRef) (tree.InstanceState, error) {
	return tree.InstanceState{Name: "running", Generation: 1}, nil
}

func TestDriverMemberReadAndNativeCall(t *testing.T) {
	runtime, _ := newSemanticGraphFixtureWithPorts(t, driverFixtureLifecycle{}, struct{ tree.OperationPort }{})
	inventoryMust(t, runtime.Registry.Reserve(driver.Namespace{Owner: "synthetic", Key: "fixture"}, []driver.InstanceKey{"one", "two"}))
	s := graphFixtureServer(t, runtime)
	calls := 0
	for _, instance := range []driver.InstanceKey{"one", "two"} {
		bind, err := runtime.Registry.Bind("fixture", instance, 1)
		inventoryMust(t, err)
		key := driver.MemberKey{Namespace: "fixture", Instance: instance, Node: driver.NodeID(bind.Root().ID), Local: "sample"}
		inventoryMust(t, bind.ReadMember(bind.Root(), driver.MemberSpec{Local: "sample", Schema: "sample-text", Mode: "read", Scope: "fixture"}, func(context.Context) ([]byte, error) { return []byte(instance), nil }))
		var data struct {
			Descriptor tree.Descriptor
			Value      tree.Value
		}
		inventoryMust(t, json.Unmarshal(callSemanticGraph(t, s, "driver_member_get", mustJSON(map[string]any{"key": key}), ""), &data))
		if data.Descriptor.Namespace != key.Namespace || data.Descriptor.Instance.Instance != key.Instance || data.Descriptor.Schema != "sample-text" || data.Descriptor.Ref != data.Value.Ref || data.Value.Status != tree.StatusLiveOnly || string(data.Value.Encoded) != string(instance) {
			t.Fatalf("member %v: %+v", key, data)
		}
	}
	bind, err := runtime.Registry.Bind("fixture", "one", 1)
	inventoryMust(t, err)
	inventoryMust(t, bind.Method(bind.Root(), driver.MethodSpec{Local: "update", Schema: "sample-text", Scope: "fixture"}, func(_ context.Context, arg []byte) ([]byte, error) {
		calls++
		if string(arg) != "valid" {
			return nil, errors.New("outside driver range")
		}
		return []byte("readback"), nil
	}))
	key := driver.MemberKey{Namespace: "fixture", Instance: "one", Node: driver.NodeID(bind.Root().ID), Local: "update"}
	for _, tc := range []struct{ arg, result, reason string }{{"valid", "readback", ""}, {"invalid", "", "outside driver range"}} {
		var data struct {
			Native []byte
			Error  string
		}
		inventoryMust(t, json.Unmarshal(callSemanticGraph(t, s, "driver_method_call", mustJSON(map[string]any{"key": key, "input": []byte(tc.arg)}), ""), &data))
		if string(data.Native) != tc.result || data.Error != tc.reason {
			t.Fatalf("native result for %q: %+v", tc.arg, data)
		}
	}
	key.Instance = "two"
	callSemanticGraph(t, s, "driver_method_call", mustJSON(map[string]any{"key": key}), "invalid_value")
	if calls != 2 {
		t.Fatalf("got %d calls, want 2", calls)
	}
}

type driverCallFixture struct {
	tree.OperationPort
	t               *testing.T
	definition      semreg.DefinitionRef
	execute         func(driver.OperationRequest) driver.OperationResult
	prepares, calls int
}

func (f *driverCallFixture) Prepare(_ context.Context, cut graph.CutRef, ref semreg.DefinitionRef, input tree.Input) (tree.PortPrepared, error) {
	f.prepares++
	if cut.CutID == "" || ref != f.definition || input.Request == nil || input.Request.Operation != ref || input.Request.Timeout != time.Second {
		f.t.Fatalf("prepare resolved %v at %v with %+v", ref, cut, input)
	}
	return tree.PortPrepared{Request: *input.Request}, nil
}

func (f *driverCallFixture) Call(_ context.Context, prepared tree.PortPrepared, _ tree.CallOptions) (tree.PortReceipt, error) {
	f.calls++
	result := f.execute(prepared.Request)
	return tree.PortReceipt{ID: "call", Result: &result}, nil
}

var driverCallDefinition = semreg.DefinitionRef{Pack: semreg.PackRef{ID: "helianthus.pack.evse", Version: "1.0.0"}, ID: "evse.operation.set_allocated_current", Version: "1.0.0"}

func driverCallMethod(t *testing.T) tree.MethodRef {
	t.Helper()
	segment, err := tree.DefinitionSegment(driverCallDefinition)
	inventoryMust(t, err)
	return tree.MethodRef(tree.Key{Node: tree.NodePath{Installation: "synthetic"}, Facet: tree.KeywordMethods, Domain: tree.KeywordSem, Segment: segment}.String())
}

func TestDriverMethodCallPreservesSemanticResult(t *testing.T) {
	raw, err := os.ReadFile("testdata/semantic_compound_vectors.json")
	inventoryMust(t, err)
	var vectors struct {
		Compound   json.RawMessage `json:"compound"`
		SingleStep json.RawMessage `json:"single_step"`
	}
	inventoryMust(t, json.Unmarshal(raw, &vectors))

	var result driver.OperationResult
	fixture := &driverCallFixture{definition: driverCallDefinition, execute: func(driver.OperationRequest) driver.OperationResult { return result }}
	runtime, _ := newSemanticGraphFixtureWithPorts(t, graphFixtureLifecycle{}, fixture)
	s := graphFixtureServer(t, runtime)
	method := driverCallMethod(t)
	args := func(operation semreg.DefinitionRef) string {
		return mustJSON(map[string]any{"method": method, "request": driver.OperationRequest{Operation: operation, Timeout: time.Second}})
	}
	for _, tc := range []struct {
		name string
		want json.RawMessage
	}{{"compound", vectors.Compound}, {"single_step", vectors.SingleStep}} {
		t.Run(tc.name, func(t *testing.T) {
			fixture.t = t
			result = driver.OperationResult{}
			inventoryMust(t, json.Unmarshal(tc.want, &result))
			var want bytes.Buffer
			inventoryMust(t, json.Compact(&want, tc.want))
			if got := callSemanticGraph(t, s, "driver_method_call", args(driverCallDefinition), ""); !bytes.Equal(got, want.Bytes()) {
				t.Errorf("result:\ngot  %s\nwant %s", got, want.Bytes())
			}
		})
	}
	for _, tc := range []struct {
		name   string
		mutate func(*semreg.DefinitionRef)
	}{
		{"definition_id", func(ref *semreg.DefinitionRef) { ref.ID = "synthetic.other" }},
		{"definition_version", func(ref *semreg.DefinitionRef) { ref.Version = "2.0.0" }},
		{"pack_id", func(ref *semreg.DefinitionRef) { ref.Pack.ID = "helianthus.pack.synthetic" }},
		{"pack_version", func(ref *semreg.DefinitionRef) { ref.Pack.Version = "2.0.0" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture.t = t
			bad := driverCallDefinition
			tc.mutate(&bad)
			assertDriverCallError(t, s, args(bad), "invalid_value", "invalid_value: request operation does not match the method")
		})
	}
	if fixture.prepares != 2 || fixture.calls != 2 {
		t.Fatalf("dispatcher: %d prepares, %d calls; want 2 each", fixture.prepares, fixture.calls)
	}
}

func assertDriverCallError(t *testing.T, s *Server, args, code, message string) {
	t.Helper()
	params := json.RawMessage(mustJSON(map[string]any{"name": "driver_method_call", "arguments": json.RawMessage(args)}))
	res := doRPC(t, s.Handler(), rpcRequest{JSONRPC: "2.0", ID: 1, Method: "tools/call", Params: params})
	assertToolErrorCode(t, res, code)
	envelope := envelopeFromResult(t, res)
	if got := envelope["error"].(map[string]any)["message"]; got != message || envelope["data"] != nil {
		t.Fatalf("error = %q, data = %v; want %q and null", got, envelope["data"], message)
	}
}

type countingDriverTransport struct{ requests int }

func (bus *countingDriverTransport) send(value int) []byte {
	bus.requests++
	return []byte(strconv.Itoa(value))
}

type syntheticDriverSetting struct {
	writable bool
	min, max int
}

func (s syntheticDriverSetting) write(bus *countingDriverTransport, value int) ([]byte, error) {
	if !s.writable {
		return nil, errors.New("key is not writable")
	}
	if value < s.min || value > s.max {
		return nil, errors.New("outside driver range")
	}
	return bus.send(value), nil
}

func TestNativeDriverMethodCallRejectsBeforeBus(t *testing.T) {
	runtime, _ := newSemanticGraphFixtureWithPorts(t, driverFixtureLifecycle{}, struct{ tree.OperationPort }{})
	inventoryMust(t, runtime.Registry.Reserve(driver.Namespace{Owner: "synthetic", Key: "fixture"}, []driver.InstanceKey{"one"}))
	bind, err := runtime.Registry.Bind("fixture", "one", 1)
	inventoryMust(t, err)
	root := bind.Root()
	child, err := bind.Node(driver.NodeSpec{Kind: "fixture.node", Local: "other", Contract: "fixture"})
	inventoryMust(t, err)
	inventoryMust(t, bind.ReadMember(root, driver.MemberSpec{Local: "read_only", Schema: "fixture-integer", Mode: "read", Scope: "fixture"}, func(context.Context) ([]byte, error) {
		return []byte("5"), nil
	}))
	setting := syntheticDriverSetting{writable: true, min: 3, max: 7}
	bus := &countingDriverTransport{}
	calls := 0
	inventoryMust(t, bind.Method(root, driver.MethodSpec{Local: "update", Schema: "fixture-integer", Scope: "fixture"}, func(_ context.Context, arg []byte) ([]byte, error) {
		calls++
		value, err := strconv.Atoi(string(arg))
		if err != nil {
			return nil, err
		}
		return setting.write(bus, value)
	}))
	s := graphFixtureServer(t, runtime)
	key := driver.MemberKey{Namespace: "fixture", Instance: "one", Node: driver.NodeID(root.ID), Local: "update"}
	args := func(key driver.MemberKey, value string) string {
		return mustJSON(map[string]any{"key": key, "input": []byte(value)})
	}

	for _, tc := range []struct {
		name, input, readback, reason string
		requests                      int
	}{
		{"below_range", "2", "", "outside driver range", 0},
		{"above_range", "8", "", "outside driver range", 0},
		{"accepted", "5", "5", "", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bus.requests, calls = 0, 0
			var got struct {
				Native []byte
				Error  string
			}
			inventoryMust(t, json.Unmarshal(callSemanticGraph(t, s, "driver_method_call", args(key, tc.input), ""), &got))
			if string(got.Native) != tc.readback || got.Error != tc.reason {
				t.Errorf("native result: %+v; want readback %q and reason %q", got, tc.readback, tc.reason)
			}
			if bus.requests != tc.requests || calls != 1 {
				t.Errorf("got %d transport requests from %d native calls; want %d requests from one call", bus.requests, calls, tc.requests)
			}
		})
	}

	readOnly := key
	readOnly.Local = "read_only"
	var published struct{ Value tree.Value }
	inventoryMust(t, json.Unmarshal(callSemanticGraph(t, s, "driver_member_get", mustJSON(map[string]any{"key": readOnly}), ""), &published))
	if string(published.Value.Encoded) != "5" {
		t.Fatalf("read-only member: got %q, want 5", published.Value.Encoded)
	}

	const noMethod = "dangling_reference: tree: method is not in the cut"
	for _, tc := range []struct {
		name          string
		mutate        func(*driver.MemberKey)
		code, message string
	}{
		{"namespace", func(key *driver.MemberKey) { key.Namespace = "other" }, "dangling_reference", noMethod},
		{"instance", func(key *driver.MemberKey) { key.Instance = "two" }, "invalid_value", "invalid_value: member instance does not match the node"},
		{"node", func(key *driver.MemberKey) { key.Node = driver.NodeID(child.ID) }, "dangling_reference", noMethod},
		{"local_read_only", func(key *driver.MemberKey) { key.Local = "read_only" }, "dangling_reference", noMethod},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bus.requests, calls = 0, 0
			bad := key
			tc.mutate(&bad)
			assertDriverCallError(t, s, args(bad, "5"), tc.code, tc.message)
			if bus.requests != 0 || calls != 0 {
				t.Errorf("got %d transport requests from %d native calls; want both zero", bus.requests, calls)
			}
		})
	}
}

func TestSemanticDriverMethodCallRejectsBeforeBus(t *testing.T) {
	bus := &countingDriverTransport{}
	settings := map[semreg.NativeBindingID]syntheticDriverSetting{
		"binding:writable":  {writable: true, min: 3, max: 7},
		"binding:read-only": {min: 3, max: 7},
	}
	fixture := &driverCallFixture{definition: driverCallDefinition}
	fixture.execute = func(request driver.OperationRequest) driver.OperationResult {
		// The tree delegates to this fixture's owner. Only the owner knows
		// the binding's write access and the synthetic range.
		setting, ok := settings[request.BindingID]
		if !ok || len(request.Arguments) != 1 {
			fixture.t.Fatalf("unexpected target or arguments: %+v", request)
		}
		arg := request.Arguments[0]
		if arg.ID != "evse.limit.allocated_current" || arg.Value.Kind != semreg.ValueQuantity || arg.Value.Quantity == nil ||
			arg.Value.Quantity.Unit != "unit.ampere" || arg.Value.Quantity.Number.Exponent10 != 0 {
			fixture.t.Fatalf("unexpected value: %+v", arg)
		}
		value, err := strconv.Atoi(arg.Value.Quantity.Number.Coefficient)
		inventoryMust(fixture.t, err)
		if _, err := setting.write(bus, value); err != nil {
			return driver.OperationResult{Status: "rejected", Reason: err.Error()}
		}
		return driver.OperationResult{Status: "accepted", Delivery: "sent"}
	}
	runtime, _ := newSemanticGraphFixtureWithPorts(t, graphFixtureLifecycle{}, fixture)
	s := graphFixtureServer(t, runtime)
	method := driverCallMethod(t)

	for _, tc := range []struct {
		name     string
		binding  semreg.NativeBindingID
		value    string
		want     string
		requests int
	}{
		{"below_range", "binding:writable", "2", `{"reason":"outside driver range","status":"rejected"}`, 0},
		{"above_range", "binding:writable", "8", `{"reason":"outside driver range","status":"rejected"}`, 0},
		{"read_only", "binding:read-only", "5", `{"reason":"key is not writable","status":"rejected"}`, 0},
		{"accepted", "binding:writable", "5", `{"delivery":"sent","status":"accepted"}`, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture.t = t
			bus.requests, fixture.prepares, fixture.calls = 0, 0, 0
			request := driver.OperationRequest{
				AssetID: "asset:a", BindingID: tc.binding, CapabilityInstanceID: "capability:synthetic",
				Operation: driverCallDefinition, Timeout: time.Second,
				ExpectedSemanticRevision: "1", ExpectedCapabilityRevision: "1", ExpectedCapabilityInstanceRevision: "1",
				ExpectedDriverGeneration: "1", ExpectedSourceEpochID: "epoch:synthetic",
				Arguments: []semreg.TypedField{{ID: "evse.limit.allocated_current", Value: semreg.Value{
					Kind: semreg.ValueQuantity, Quantity: &semreg.Quantity{Number: semreg.Decimal{Coefficient: tc.value}, Unit: "unit.ampere"},
				}}},
			}
			inventoryMust(t, request.Validate())
			args := mustJSON(map[string]any{"method": method, "request": request})
			if got := callSemanticGraph(t, s, "driver_method_call", args, ""); string(got) != tc.want {
				t.Errorf("got %s, want %s", got, tc.want)
			}
			if bus.requests != tc.requests || fixture.prepares != 1 || fixture.calls != 1 {
				t.Errorf("got %d requests, %d prepares, %d port calls; want %d requests, one prepare and one port call", bus.requests, fixture.prepares, fixture.calls, tc.requests)
			}
		})
	}
}
