package mcp

import (
	"context"
	"encoding/json"
	"errors"
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
	result          driver.OperationResult
	prepares, calls int
}

func (f *driverCallFixture) Prepare(_ context.Context, cut graph.CutRef, ref semreg.DefinitionRef, input tree.Input) (tree.PortPrepared, error) {
	f.prepares++
	if cut.CutID == "" || ref != f.definition || input.Request == nil || input.Request.Operation != ref || input.Request.Timeout != time.Second {
		f.t.Fatalf("prepare resolved %v at %v with %+v", ref, cut, input)
	}
	return tree.PortPrepared{Request: *input.Request}, nil
}

func (f *driverCallFixture) Call(context.Context, tree.PortPrepared, tree.CallOptions) (tree.PortReceipt, error) {
	f.calls++
	return tree.PortReceipt{ID: "call", Result: &f.result}, nil
}

func TestDriverMethodCallPreservesSemanticResult(t *testing.T) {
	ref := semreg.DefinitionRef{Pack: semreg.PackRef{ID: "helianthus.pack.evse", Version: "1.0.0"}, ID: "evse.operation.set_allocated_current", Version: "1.0.0"}
	fixture := &driverCallFixture{t: t, definition: ref}
	runtime, _ := newSemanticGraphFixtureWithPorts(t, graphFixtureLifecycle{}, fixture)
	s := graphFixtureServer(t, runtime)
	segment, err := tree.DefinitionSegment(ref)
	inventoryMust(t, err)
	method := tree.Key{Node: tree.NodePath{Installation: "synthetic"}, Facet: tree.KeywordMethods, Domain: tree.KeywordSem, Segment: segment}
	args := func(operation semreg.DefinitionRef) string {
		return mustJSON(map[string]any{"method": method.String(), "request": driver.OperationRequest{Operation: operation, Timeout: time.Second}})
	}
	for _, want := range []string{
		`{"compound_execution":{"partial":true,"steps":[{"acknowledgement":"accepted","attempted":true,"delivery":"sent","point":"point.first","readback_relation":"confirms"},{"attempted":true,"delivery":"unknown","point":"point.second","reason":"delivery ambiguous"},{"attempted":false,"point":"point.third"}]},"kernel_outcome":"indeterminate","status":"unknown"}`,
		`{"delivery":"sent","status":"accepted"}`,
		`{"reason":"outside driver range","status":"rejected"}`,
	} {
		fixture.result = driver.OperationResult{}
		inventoryMust(t, json.Unmarshal([]byte(want), &fixture.result))
		if got := callSemanticGraph(t, s, "driver_method_call", args(ref), ""); string(got) != want {
			t.Errorf("%s result:\ngot  %s\nwant %s", fixture.result.Status, got, want)
		}
	}
	bad := ref
	bad.ID = "synthetic.other"
	callSemanticGraph(t, s, "driver_method_call", args(bad), "invalid_value")
	if fixture.prepares != 3 || fixture.calls != 3 {
		t.Fatalf("dispatcher: %d prepares, %d calls; want 3 each", fixture.prepares, fixture.calls)
	}
}
