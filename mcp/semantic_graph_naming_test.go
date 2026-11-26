package mcp

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func forbiddenSemanticGraphName(name string) bool {
	switch strings.ToLower(name) {
	case "plane", "planes", "projection", "projections":
		return true
	default:
		return false
	}
}

// API-4: only the three current family reads remain until the follow-up to
// #1742 moves them. Take the exact storage name from its isolated, storage-only
// production profile in the committed inventory, not the live registrations.
func semanticGraphFamilyRemainder(t *testing.T) map[string]bool {
	t.Helper()
	var storage string
	for _, row := range inventoryRead(t).Rows {
		if row.Surface != "ordinary" || !strings.HasSuffix(row.Profile, "_production_2") || !strings.HasPrefix(row.Name, "semantic_v1_") {
			continue
		}
		if storage != "" {
			t.Fatalf("API-4: storage-only inventory has multiple semantic reads: %q and %q", storage, row.Name)
		}
		storage = row.Name
	}
	if !strings.HasPrefix(storage, "semantic_v1_storage_") || !strings.HasSuffix(storage, "_current_get") {
		t.Fatalf("API-4: storage-only inventory is missing its current read: %q", storage)
	}
	return map[string]bool{
		"semantic_v1_pv_current_get":   true,
		storage:                        true,
		"semantic_v1_evse_current_get": true,
	}
}

func checkSemanticGraphToolName(name string, familyRemainder map[string]bool) error {
	local, semantic := strings.CutPrefix(name, "semantic_")
	if !semantic {
		return nil
	}
	for _, word := range strings.Split(local, "_") {
		if forbiddenSemanticGraphName(word) {
			return fmt.Errorf("API-1: forbidden tool name %q", name)
		}
	}
	if familyRemainder[name] {
		return nil
	}
	switch name {
	case "semantic_v1_resources_list", "semantic_v1_traverse", "semantic_v1_lens_get", "semantic_v1_catalog_get", "semantic_v1_snapshot_get":
		return nil
	default:
		return fmt.Errorf("API-4: unreviewed semantic tool name %q", name)
	}
}

func checkSemanticGraphArgumentNames(tool, path string, schema map[string]any) error {
	properties, _ := schema["properties"].(map[string]any)
	for name, value := range properties {
		argument := path + "." + name
		// ENT-2 and TRV-3 name these selectors. API-1 still applies to
		// every other argument name; identifiers used as values are not names.
		selector := tool == SemanticResourcesListTool && argument == "arguments.plane" ||
			tool == SemanticTraverseTool && argument == "arguments.planes"
		if forbiddenSemanticGraphName(name) && !selector {
			return fmt.Errorf("API-1: %s has forbidden argument %s", tool, argument)
		}
		child, _ := value.(map[string]any)
		if err := checkSemanticGraphArgumentNames(tool, argument, child); err != nil {
			return err
		}
	}
	if items, ok := schema["items"].(map[string]any); ok {
		return checkSemanticGraphArgumentNames(tool, path+"[]", items)
	}
	return nil
}

func semanticGraphCanonicalBody(tool, path string) bool {
	switch tool {
	case SemanticResourcesListTool:
		return path == "data.resources[]"
	case SemanticTraverseTool:
		return path == "data.resources[]" || path == "data.relations[]"
	case SemanticLensGetTool:
		switch path {
		case "data.resources[]", "data.relations[]", "data.absences[]", "data.facts[]", "data.services[]", "data.capabilities[]":
			return true
		}
	case SemanticCatalogGetTool:
		return path == "data.entries[]" || path == "data.lenses[]" || path == "data.planes[]"
	}
	return false
}

func checkSemanticGraphResultNames(tool, path string, value any) error {
	// API-2: these SemReg bodies are checked byte for byte, not renamed by
	// the graph API. In particular PLN-3 membership keeps its planes field.
	if semanticGraphCanonicalBody(tool, path) {
		return nil
	}
	switch node := value.(type) {
	case map[string]any:
		for name, child := range node {
			field := name
			if path != "" {
				field = path + "." + name
			}
			catalogPlanes := tool == SemanticCatalogGetTool && field == "data.planes"
			if forbiddenSemanticGraphName(name) && !catalogPlanes {
				return fmt.Errorf("API-1: %s has forbidden result field %s", tool, field)
			}
			if err := checkSemanticGraphResultNames(tool, field, child); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range node {
			if err := checkSemanticGraphResultNames(tool, path+"[]", child); err != nil {
				return err
			}
		}
	}
	return nil
}

func semanticGraphNamingServer(t *testing.T) *Server {
	t.Helper()
	server, err := NewServer(&testRegistry{}, &testInvoker{})
	inventoryMust(t, err)
	inventoryComposedGraph(t, server)
	return server
}

func semanticGraphNamingResult(t *testing.T, s *Server, tool string, args json.RawMessage, wantError bool) map[string]any {
	t.Helper()
	params := json.RawMessage(fmt.Sprintf(`{"name":%q,"arguments":%s}`, tool, args))
	response := doRPC(t, s.Handler(), rpcRequest{JSONRPC: "2.0", ID: 1, Method: "tools/call", Params: params})
	if response.Error != nil {
		t.Fatalf("%s: RPC error: %+v", tool, response.Error)
	}
	result := response.Result.(map[string]any)
	if result["isError"] != wantError {
		t.Fatalf("%s: unexpected isError: %v", tool, result["isError"])
	}
	content := result["content"].([]any)
	var envelope map[string]any
	inventoryMust(t, json.Unmarshal([]byte(content[0].(map[string]any)["text"].(string)), &envelope))
	return envelope
}

func TestSemanticGraphNaming(t *testing.T) {
	familyRemainder := semanticGraphFamilyRemainder(t)
	t.Run("tools_list", func(t *testing.T) {
		for _, profile := range inventoryProfiles() {
			t.Run(profile.name, func(t *testing.T) {
				server, err := NewServer(&testRegistry{}, &testInvoker{})
				inventoryMust(t, err)
				if profile.setup != nil {
					profile.setup(t, server)
				}
				for _, tool := range inventoryTools(t, server, profile) {
					if !strings.HasPrefix(tool.Name, "semantic_") {
						continue
					}
					if err := checkSemanticGraphToolName(tool.Name, familyRemainder); err != nil {
						t.Error(err)
					}
					if err := checkSemanticGraphArgumentNames(tool.Name, "arguments", tool.InputSchema); err != nil {
						t.Error(err)
					}
				}
			})
		}
	})
	server := semanticGraphNamingServer(t)
	for _, vector := range loadSemanticGraphVectors(t).Reads {
		t.Run(vector.Tool, func(t *testing.T) {
			success := semanticGraphNamingResult(t, server, vector.Tool, vector.Arguments, false)
			inventoryMust(t, checkSemanticGraphResultNames(vector.Tool, "", success))
			// A refusal exercises names inside the structured error, not just error:null.
			refusal := semanticGraphNamingResult(t, server, vector.Tool, json.RawMessage(`null`), true)
			inventoryMust(t, checkSemanticGraphResultNames(vector.Tool, "", refusal))
		})
	}
}

func TestSemanticGraphNamingRejectsInjections(t *testing.T) {
	server := semanticGraphNamingServer(t)
	familyRemainder := semanticGraphFamilyRemainder(t)
	// Register the synthetic names on this test server and inspect tools/list,
	// exactly as for real registrations.
	injected := map[string]string{
		"semantic_v1_plane_list":                   "API-1",
		"semantic_v1_planes_list":                  "API-1",
		"semantic_v1_projection_get":               "API-1",
		"semantic_v1_projections_get":              "API-1",
		"semantic_v1_synthetic_pack_current_get":   "API-4",
		"semantic_v1_synthetic_vendor_current_get": "API-4",
		"semantic_v1_native_profile_current_get":   "API-4",
	}
	for name := range injected {
		server.tools = append(server.tools, Tool{Name: name})
	}
	for _, tool := range inventoryTools(t, server, inventoryProfile{}) {
		if rule, ok := injected[tool.Name]; ok {
			t.Run(tool.Name, func(t *testing.T) {
				err := checkSemanticGraphToolName(tool.Name, familyRemainder)
				if err == nil || !strings.HasPrefix(err.Error(), rule+":") {
					t.Fatalf("%s: got %v, want %s rejection", tool.Name, err, rule)
				}
			})
			delete(injected, tool.Name)
		}
	}
	if len(injected) != 0 {
		t.Fatalf("injections missing from tools/list: %v", injected)
	}

	t.Run("nested_argument", func(t *testing.T) {
		var schema map[string]any
		inventoryMust(t, json.Unmarshal([]byte(`{"properties":{"definitions":{"items":{"properties":{"projection":{}}}}}}`), &schema))
		err := checkSemanticGraphArgumentNames(SemanticTraverseTool, "arguments", schema)
		if err == nil || !strings.Contains(err.Error(), "arguments.definitions[].projection") {
			t.Fatalf("nested argument injection: got %v, want API-1 rejection", err)
		}
	})

	for _, field := range []string{"plane", "planes", "projection", "projections"} {
		t.Run("result_"+field, func(t *testing.T) {
			result := semanticGraphNamingResult(t, server, SemanticResourcesListTool, json.RawMessage(`{"page_size":4}`), false)
			result["data"].(map[string]any)[field] = "physical"
			err := checkSemanticGraphResultNames(SemanticResourcesListTool, "", result)
			if err == nil || !strings.Contains(err.Error(), "data."+field) {
				t.Fatalf("result injection %q: got %v, want API-1 rejection", field, err)
			}
		})
	}
}
