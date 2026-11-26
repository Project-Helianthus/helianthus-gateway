package mcp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Project-Helianthus/helianthus-semreg/semreg/runtime/tree"
	semreg "github.com/Project-Helianthus/helianthus-semreg/semreg/v1"
	"github.com/Project-Helianthus/helianthus-semreg/semreg/v1/graph"
)

const (
	SemanticResourcesListTool = "semantic_v1_resources_list"
	SemanticTraverseTool      = "semantic_v1_traverse"
	SemanticLensGetTool       = "semantic_v1_lens_get"
	SemanticCatalogGetTool    = "semantic_v1_catalog_get"
)

// RegisterSemanticGraphTools attaches the shared runtime tree before serving MCP.
func RegisterSemanticGraphTools(s *Server, runtime *tree.Tree) error {
	if s == nil || runtime == nil {
		return errors.New("semantic graph requires an MCP server and a runtime tree")
	}
	if s.semanticGraph != nil {
		return errors.New("semantic graph already registered")
	}
	s.semanticGraph = runtime
	s.tools = append(s.tools, semanticGraphTools()...)
	return nil
}

func semanticGraphTools() []Tool {
	text := map[string]any{"type": "string"}
	object := func(properties map[string]any, required ...string) map[string]any {
		schema := map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
		if len(required) != 0 {
			schema["required"] = required
		}
		return schema
	}
	page := func() map[string]any {
		return map[string]any{
			"cut_id": text, "page_token": text,
			"page_size": map[string]any{"type": "integer", "minimum": 1, "maximum": graph.MaxPageSize},
		}
	}
	walk := func() map[string]any {
		props := page()
		props["entry"] = object(map[string]any{
			"kind":     map[string]any{"type": "string", "enum": []graph.EntryKind{graph.EntryResource, graph.EntryLensRoots, graph.EntrySites}},
			"resource": text, "lens": text,
		}, "kind")
		props["lens"] = text
		props["direction"] = map[string]any{"type": "string", "enum": []graph.TraversalDirection{graph.DirectionOutgoing, graph.DirectionIncoming, graph.DirectionBoth}}
		props["depth"] = map[string]any{"type": "integer", "minimum": 0, "maximum": graph.MaxTraversalDepth}
		return props
	}
	list := page()
	list["kind"], list["plane"] = text, text
	traverse := walk()
	traverse["planes"] = map[string]any{"type": "array", "items": text}
	traverse["definitions"] = map[string]any{"type": "array", "items": object(map[string]any{
		"id": text, "version": text, "pack": object(map[string]any{"id": text, "version": text}, "id", "version"),
	}, "id", "version", "pack")}
	return []Tool{
		{Name: SemanticResourcesListTool, Description: "List current SemReg resources in the served cut.", InputSchema: object(list, "page_size")},
		{Name: SemanticTraverseTool, Description: "Traverse coherent SemReg relations in the served cut.", InputSchema: object(traverse, "entry", "direction", "depth", "page_size")},
		{Name: SemanticLensGetTool, Description: "Retrieve a SemReg lens result in the served cut.", InputSchema: object(walk(), "entry", "lens", "direction", "depth", "page_size")},
		{Name: SemanticCatalogGetTool, Description: "Read the SemReg element catalog.", InputSchema: object(map[string]any{})},
	}
}

// CUT-4: keep the tree's omission vector beside each graph page.
type semanticResourceList struct {
	graph.ResourcePage
	NotIncluded []graph.NotIncludedAsset `json:"not_included,omitempty"`
}

type semanticTraversal struct {
	graph.TraversalPage
	NotIncluded []graph.NotIncludedAsset `json:"not_included,omitempty"`
}

type semanticElementCatalog struct {
	Digest  semreg.Digest   `json:"digest"`
	Status  graph.Status    `json:"status"`
	Planes  []graph.Plane   `json:"planes"`
	Lenses  []graph.Lens    `json:"lenses"`
	Entries []graph.Element `json:"entries"`
}

func (c semanticElementCatalog) Validate() error { return c.Digest.Validate() }

type semanticLensArguments struct {
	CutID     *semreg.Digest           `json:"cut_id"`
	Lens      semreg.DefinitionID      `json:"lens"`
	Entry     graph.EntryPoint         `json:"entry"`
	Direction graph.TraversalDirection `json:"direction"`
	Depth     *int                     `json:"depth"`
	PageSize  int                      `json:"page_size"`
	PageToken string                   `json:"page_token"`
}

func decodeSemanticGraphArguments(raw json.RawMessage, duplicate bool, dst any) error {
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	if raw = bytes.TrimSpace(raw); duplicate || len(raw) == 0 || raw[0] != '{' {
		return &semreg.Error{ID: semreg.InvalidValue, Detail: "arguments must be an object without duplicate members"}
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return &semreg.Error{ID: semreg.InvalidValue, Detail: "invalid graph arguments"}
	}
	return nil
}

func (s *Server) readSemanticGraph(ctx context.Context, name string, raw json.RawMessage, duplicate bool) (semreg.Record, error) {
	switch name {
	case SemanticResourcesListTool:
		var request graph.ListRequest
		if err := decodeSemanticGraphArguments(raw, duplicate, &request); err != nil {
			return nil, err
		}
		result, err := s.semanticGraph.ListResources(ctx, request)
		return semanticResourceList{result.Page, result.NotIncluded}, err
	case SemanticTraverseTool:
		var request struct {
			graph.TraversalRequest
			Depth *int `json:"depth"`
		}
		if err := decodeSemanticGraphArguments(raw, duplicate, &request); err != nil {
			return nil, err
		}
		if request.Depth == nil {
			return nil, &semreg.Error{ID: semreg.MissingMember, Detail: "depth"}
		}
		request.TraversalRequest.Depth = *request.Depth
		result, err := s.semanticGraph.Traverse(ctx, request.TraversalRequest)
		return semanticTraversal{result.Page, result.NotIncluded}, err
	case SemanticLensGetTool:
		var request semanticLensArguments
		if err := decodeSemanticGraphArguments(raw, duplicate, &request); err != nil {
			return nil, err
		}
		if request.Depth == nil {
			return nil, &semreg.Error{ID: semreg.MissingMember, Detail: "depth"}
		}
		return s.semanticGraph.RetrieveLens(ctx, graph.LensRequest{
			CutID: request.CutID, Lens: request.Lens, Entry: request.Entry, Direction: request.Direction,
			Depth: *request.Depth, PageSize: request.PageSize, PageToken: request.PageToken,
		})
	case SemanticCatalogGetTool:
		if err := decodeSemanticGraphArguments(raw, duplicate, &struct{}{}); err != nil {
			return nil, err
		}
		result, err := s.semanticGraph.Catalog(ctx)
		return semanticElementCatalog(result), err
	default:
		return nil, &semreg.Error{ID: semreg.InvalidValue, Detail: "unknown graph read"}
	}
}

func (s *Server) handleSemanticGraph(ctx context.Context, call rawCallToolParams, duplicate bool) map[string]any {
	data, err := s.readSemanticGraph(ctx, call.Name, call.Arguments, duplicate)
	var canonical []byte
	if err == nil {
		canonical, err = semreg.CanonicalJSON(data)
	}
	if err != nil {
		envelope := newToolEnvelope(nil, err)
		var semanticError *semreg.Error
		if errors.As(err, &semanticError) {
			envelope["error"].(map[string]any)["code"] = string(semanticError.ID)
		}
		return callToolResultText(mustJSON(envelope), true)
	}

	// API-2 and CUT-5: hash and serve the same canonical record bytes.
	envelope := newToolEnvelope(nil, nil)
	envelope["data"] = json.RawMessage(canonical)
	envelope["meta"].(map[string]any)["data_hash"] = fmt.Sprintf("%x", sha256.Sum256(canonical))
	var wire bytes.Buffer
	encoder := json.NewEncoder(&wire)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(envelope); err != nil {
		return callToolResultText(mustJSON(newToolEnvelope(nil, err)), true)
	}
	return callToolResultText(wire.String()[:wire.Len()-1], false)
}
