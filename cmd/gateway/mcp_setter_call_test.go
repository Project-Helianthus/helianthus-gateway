package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Project-Helianthus/helianthus-gateway/mcp"
	"github.com/Project-Helianthus/helianthus-semreg/semreg/runtime/tree"
	"github.com/Project-Helianthus/helianthus-semreg/semreg/v1/driver"
	"github.com/Project-Helianthus/helianthus-semreg/semreg/v1/graph"
)

type setterCallVectors struct {
	Namespace string `json:"namespace"`
	Instance  string `json:"instance"`
	Calls     []struct {
		Name   string `json:"name"`
		Local  string `json:"local"`
		Input  []byte `json:"input"`
		Result []byte `json:"result"`
	} `json:"calls"`
}

type setterCallRegistry struct{ mcp.Registry }

type setterCallCuts struct {
	tree.CutSource
	view tree.CutView
}

func (c setterCallCuts) Capture(context.Context, driver.Filter) (tree.CutView, error) {
	return c.view, nil
}

func (c setterCallCuts) Lookup(context.Context, graph.CutRef) (tree.CutView, error) {
	return c.view, nil
}

type setterCallLifecycle struct{ tree.LifecycleSource }

func (setterCallLifecycle) OnFence(func(tree.Fence)) func() { return func() {} }
func (setterCallLifecycle) State(tree.InstanceRef) (tree.InstanceState, error) {
	return tree.InstanceState{Name: "running", Generation: 1}, nil
}

func TestMCPSetterCall(t *testing.T) {
	raw, err := os.ReadFile("testdata/mcp_setter_call_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors setterCallVectors
	if err := json.Unmarshal(raw, &vectors); err != nil {
		t.Fatal(err)
	}
	if len(vectors.Calls) == 0 {
		t.Fatal("vector file has no calls")
	}

	catalog, err := graph.Embedded()
	if err != nil {
		t.Fatal(err)
	}
	store := graph.NewCutStore(func() time.Time { return time.Unix(0, 0) })
	cut, err := graph.AssembleCut(nil, catalog, graph.CutPartial)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Retain(cut, nil, catalog); err != nil {
		t.Fatal(err)
	}
	view := tree.CutView{
		Ref: graph.CutRef{CutID: cut.ID(), Completeness: graph.CutPartial, Members: cut.Members()},
		Cut: cut, Store: store, Catalog: catalog,
	}
	runtime, err := tree.New(tree.Config{
		Registry: tree.RegistryConfig{Installation: "synthetic", Bridge: "bridge.synthetic", Incarnation: "synthetic", Lifecycle: setterCallLifecycle{}},
		Cuts:     setterCallCuts{view: view}, Operations: struct{ tree.OperationPort }{},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(runtime.Registry.Close)

	ns := driver.NamespaceID(vectors.Namespace)
	instance := driver.InstanceKey(vectors.Instance)
	if err := runtime.Registry.Reserve(driver.Namespace{Owner: "synthetic", Key: ns}, []driver.InstanceKey{instance}); err != nil {
		t.Fatal(err)
	}
	bind, err := runtime.Registry.Bind(ns, instance, 1)
	if err != nil {
		t.Fatal(err)
	}
	received := map[string][]byte{}
	count := map[string]int{}
	for _, v := range vectors.Calls {
		v := v
		err := bind.Method(bind.Root(), driver.MethodSpec{Local: driver.MemberID(v.Local), Schema: "synthetic-bytes", Scope: "synthetic"},
			func(_ context.Context, arg []byte) ([]byte, error) {
				count[v.Local]++
				received[v.Local] = append([]byte(nil), arg...)
				return v.Result, nil
			})
		if err != nil {
			t.Fatal(err)
		}
	}

	server, err := mcp.NewServer(setterCallRegistry{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := mcp.RegisterSemanticGraphTools(server, runtime); err != nil {
		t.Fatal(err)
	}
	handler := server.Handler()

	for _, v := range vectors.Calls {
		v := v
		t.Run(v.Name, func(t *testing.T) {
			key := driver.MemberKey{Namespace: ns, Instance: instance, Node: driver.NodeID(bind.Root().ID), Local: driver.MemberID(v.Local)}
			args, err := json.Marshal(map[string]any{"key": key, "input": v.Input})
			if err != nil {
				t.Fatal(err)
			}
			body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"driver_method_call","arguments":%s}}`, args)
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			handler.ServeHTTP(rec, req)
			var response struct {
				Result struct {
					IsError bool `json:"isError"`
					Content []struct {
						Text string `json:"text"`
					} `json:"content"`
				} `json:"result"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
				t.Fatalf("response: %v: %s", err, rec.Body.String())
			}
			if response.Result.IsError || len(response.Result.Content) != 1 {
				t.Fatalf("call failed: %s", rec.Body.String())
			}
			var envelope struct {
				Data struct {
					Native []byte `json:"native"`
					Error  string `json:"error"`
				} `json:"data"`
			}
			if err := json.Unmarshal([]byte(response.Result.Content[0].Text), &envelope); err != nil {
				t.Fatal(err)
			}
			if count[v.Local] != 1 {
				t.Fatalf("driver received %d calls for %s, want 1", count[v.Local], v.Local)
			}
			if !bytes.Equal(received[v.Local], v.Input) {
				t.Errorf("driver received %q, want %q", received[v.Local], v.Input)
			}
			if !bytes.Equal(envelope.Data.Native, v.Result) || envelope.Data.Error != "" {
				t.Errorf("caller got %q (error %q), want %q", envelope.Data.Native, envelope.Data.Error, v.Result)
			}
		})
	}
}
