package graphql

import (
	"fmt"
	"reflect"
	"testing"
)

func TestNamespaceNativeProcedures(t *testing.T) {
	f := newNamespaceFixture(t, nil, false)
	schema, _ := f.schema(t)
	const describe = `query($key:member_key_input!) { sample_describe(key:$key,facet:methods) { kind ref schema scope } }`
	key := namespaceKey("sample", "one", namespaceMainPath, "reset")
	namespaceJSON(t, namespaceExecute(f.ctx, schema, describe, map[string]any{"key": key}), fmt.Sprintf(
		`{"sample_describe":{"kind":"method","ref":%q,"schema":"schema.counter_command","scope":"operator"}}`, namespaceMainPath+"/methods/native/BnNhbXBsZQVyZXNldA"))
	if f.nativeCalls != 0 || f.busRequests != 0 {
		t.Fatal("inspecting a method dispatched it")
	}

	const call = `mutation($key:member_key_input!, $input:String!) { sample_call(key:$key,input:$input) { native error status compound_execution {partial} } }`
	for _, tc := range []struct{ node, local, input, want string }{
		{namespaceMainPath, "reset", "Mg==", `{"native":null,"error":"outside driver range","status":null,"compound_execution":null}`},
		{namespaceMainPath, "reset", "MQ==", `{"native":"dHJhbnNwb3J0","error":null,"status":null,"compound_execution":null}`},
		{namespaceOnePath, "start", "MQ==", `{"native":"aW5zdGFuY2U=","error":null,"status":null,"compound_execution":null}`},
	} {
		vars := map[string]any{"key": namespaceKey("sample", "one", tc.node, tc.local), "input": tc.input}
		namespaceJSON(t, namespaceExecute(f.ctx, schema, call, vars), `{"sample_call":`+tc.want+`}`)
		if tc.input == "Mg==" && (f.busRequests != 0 || len(f.applied) != 0) {
			t.Fatal("rejected value reached the transport")
		}
	}
	for _, tc := range []struct{ field, changed, code string }{
		{"namespace", "aux", "invalid_value"},
		{"instance", "two", "invalid_value"},
		{"node", namespaceSparePath[1:], "dangling_reference"},
		{"local", "start", "dangling_reference"},
	} {
		t.Run(tc.field, func(t *testing.T) {
			wrong := namespaceKey("sample", "one", namespaceMainPath, "reset")
			wrong[tc.field] = tc.changed
			namespaceAssertCode(t, namespaceExecute(f.ctx, schema, call, map[string]any{"key": wrong, "input": "MQ=="}), "sample_call", tc.code)
		})
	}
	namespaceAssertCode(t, namespaceExecute(f.ctx, schema, call, map[string]any{"key": key, "input": "@not-base64"}), "sample_call", "invalid_value")
	if f.nativeCalls != 3 || f.busRequests != 2 || !reflect.DeepEqual(f.applied, []string{"transport", "instance"}) {
		t.Fatalf("native effects: calls=%d, requests=%d, actions=%v", f.nativeCalls, f.busRequests, f.applied)
	}
}
