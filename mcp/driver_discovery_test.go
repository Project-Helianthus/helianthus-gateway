package mcp

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/Project-Helianthus/helianthus-semreg/semreg/runtime/tree"
	"github.com/Project-Helianthus/helianthus-semreg/semreg/v1/driver"
)

type discoveryWorld struct {
	s     *Server
	roots map[string]driver.NodeID
	calls int
}

type discoveryPage struct {
	Cut     tree.CutID
	Members []struct {
		Key  *driver.MemberKey
		Kind tree.EntryKind
		Mode driver.AccessMode
	}
	Next string
}

func newDiscoveryWorld(t *testing.T) *discoveryWorld {
	t.Helper()
	runtime, _ := newSemanticGraphFixtureWithPorts(t, driverFixtureLifecycle{}, struct{ tree.OperationPort }{})
	w := &discoveryWorld{roots: map[string]driver.NodeID{}}
	inventoryMust(t, runtime.Registry.Reserve(driver.Namespace{Owner: "synthetic", Key: "alpha"}, []driver.InstanceKey{"one", "two"}))
	inventoryMust(t, runtime.Registry.Reserve(driver.Namespace{Owner: "synthetic", Key: "beta"}, []driver.InstanceKey{"one", "two", "idle"}))
	for _, namespace := range []driver.NamespaceID{"alpha", "beta"} {
		for _, instance := range []driver.InstanceKey{"one", "two"} {
			bind, err := runtime.Registry.Bind(namespace, instance, 1)
			inventoryMust(t, err)
			w.roots[string(namespace)+"/"+string(instance)] = driver.NodeID(bind.Root().ID)
			for _, local := range []string{"a", "b", "c"} {
				inventoryMust(t, bind.ReadMember(bind.Root(), driver.MemberSpec{Local: driver.MemberID(local), Schema: "sample-text", Mode: "read", Scope: "fixture"}, func(context.Context) ([]byte, error) {
					return []byte(local), nil
				}))
			}
			inventoryMust(t, bind.Method(bind.Root(), driver.MethodSpec{Local: "m", Schema: "sample-text", Scope: "fixture"}, func(context.Context, []byte) ([]byte, error) {
				w.calls++
				return nil, nil
			}))
		}
	}
	w.s = graphFixtureServer(t, runtime)
	return w
}

func (w *discoveryWorld) list(t *testing.T, args map[string]any, wantCode string) discoveryPage {
	t.Helper()
	var page discoveryPage
	raw := callSemanticGraph(t, w.s, "driver_members_list", mustJSON(args), wantCode)
	if wantCode == "" {
		inventoryMust(t, json.Unmarshal(raw, &page))
	}
	return page
}

func TestDriverDiscoveryNamespacesListsTwoNamespacesOfOneOwnerAndEmptyInstance(t *testing.T) {
	w := newDiscoveryWorld(t)
	var data struct {
		Namespaces []struct {
			Owner     string
			Namespace string
			Instances []string
		}
	}
	inventoryMust(t, json.Unmarshal(callSemanticGraph(t, w.s, "driver_namespaces_list", `{}`, ""), &data))
	got := [][]any{}
	for _, n := range data.Namespaces {
		got = append(got, []any{n.Owner, n.Namespace, n.Instances})
	}
	want := [][]any{{"synthetic", "alpha", []string{"one", "two"}}, {"synthetic", "beta", []string{"idle", "one", "two"}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("namespaces = %v, want %v", got, want)
	}
	callSemanticGraph(t, w.s, "driver_namespaces_list", `{"extra":1}`, "invalid_value")
}

func TestDriverDiscoveryMembersByInstanceAndNode(t *testing.T) {
	w := newDiscoveryWorld(t)
	for _, namespace := range []string{"alpha", "beta"} {
		for _, instance := range []string{"one", "two"} {
			node := w.roots[namespace+"/"+instance]
			page := w.list(t, map[string]any{"namespace": namespace, "instance": instance, "node": node, "page_size": 50}, "")
			if len(page.Members) != 4 || page.Next != "" {
				t.Fatalf("%s/%s: %+v", namespace, instance, page)
			}
			for _, m := range page.Members {
				if m.Key == nil || string(m.Key.Namespace) != namespace || string(m.Key.Instance) != instance || m.Key.Node != node {
					t.Fatalf("%s/%s: key %+v", namespace, instance, m.Key)
				}
			}
			attrs := w.list(t, map[string]any{"namespace": namespace, "instance": instance, "node": node, "kind": "attribute", "page_size": 50}, "")
			if len(attrs.Members) != 3 || attrs.Members[0].Mode != "read" {
				t.Fatalf("attribute listing %+v", attrs)
			}
		}
	}
}

func TestDriverDiscoveryEmptyAndStableErrors(t *testing.T) {
	w := newDiscoveryWorld(t)
	node := w.roots["alpha/one"]
	page := w.list(t, map[string]any{"namespace": "alpha", "instance": "one", "node": node, "kind": "view", "page_size": 5}, "")
	if len(page.Members) != 0 || page.Next != "" {
		t.Fatalf("empty result %+v", page)
	}
	w.list(t, map[string]any{"namespace": "alpha", "instance": "idle", "node": node, "page_size": 5}, "dangling_reference")
	w.list(t, map[string]any{"namespace": "nope", "instance": "one", "node": node, "page_size": 5}, "dangling_reference")
	w.list(t, map[string]any{"namespace": "alpha", "instance": "one", "node": node, "page_size": 0}, "invalid_value")
	w.list(t, map[string]any{"namespace": "alpha", "instance": "one", "node": "missing", "page_size": 5}, "dangling_reference")
	w.list(t, map[string]any{"namespace": "alpha", "instance": "one", "node": node, "page_size": 5, "cut": "gone"}, "dangling_reference")
	callSemanticGraph(t, w.s, "driver_members_list", `{"namespace":"alpha"}`, "invalid_value")
	callSemanticGraph(t, w.s, "driver_members_list", `{"namespace":"alpha","instance":"one","node":"x","page_size":1,"extra":1}`, "invalid_value")
}

func TestDriverDiscoveryPageContinuationConcatenatesAndRefusesOtherCut(t *testing.T) {
	w := newDiscoveryWorld(t)
	node := w.roots["alpha/one"]
	base := map[string]any{"namespace": "alpha", "instance": "one", "node": node}
	full := w.list(t, map[string]any{"namespace": "alpha", "instance": "one", "node": node, "page_size": 50}, "")
	var keys []string
	args := map[string]any{"page_size": 3}
	for k, v := range base {
		args[k] = v
	}
	var firstCut tree.CutID
	for i := 0; ; i++ {
		page := w.list(t, args, "")
		for _, m := range page.Members {
			keys = append(keys, string(m.Key.Local))
		}
		if i == 0 {
			firstCut = page.Cut
			other := w.list(t, map[string]any{"namespace": "alpha", "instance": "one", "node": node, "page_size": 3}, "")
			if other.Cut == firstCut || other.Next == "" {
				t.Fatalf("second capture %+v", other)
			}
			w.list(t, map[string]any{"namespace": "alpha", "instance": "one", "node": node, "page_size": 3, "cut": other.Cut, "page_token": page.Next}, "revision_conflict")
		}
		if page.Next == "" {
			break
		}
		if page.Cut != firstCut {
			t.Fatalf("cut moved: %v != %v", page.Cut, firstCut)
		}
		args["cut"], args["page_token"] = page.Cut, page.Next
	}
	var want []string
	for _, m := range full.Members {
		want = append(want, string(m.Key.Local))
	}
	if !reflect.DeepEqual(keys, want) || len(keys) != 4 {
		t.Fatalf("pages = %v, full = %v", keys, want)
	}
	w.list(t, args, "dangling_reference")
}

func TestDriverDiscoveryListingAndReadingMakeNoBusRequest(t *testing.T) {
	w := newDiscoveryWorld(t)
	var data struct{ Value tree.Value }
	node := w.roots["beta/two"]
	page := w.list(t, map[string]any{"namespace": "beta", "instance": "two", "node": node, "page_size": 50}, "")
	if w.calls != 0 {
		t.Fatalf("listing started a driver call: %d", w.calls)
	}
	key := driver.MemberKey{Namespace: "beta", Instance: "two", Node: node, Local: "a"}
	inventoryMust(t, json.Unmarshal(callSemanticGraph(t, w.s, "driver_member_get", mustJSON(map[string]any{"key": key}), ""), &data))
	if string(data.Value.Encoded) != "a" || w.calls != 0 || len(page.Members) != 4 {
		t.Fatalf("read: %+v calls=%d", data, w.calls)
	}
}
