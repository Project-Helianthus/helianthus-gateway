package mcp

import (
	"context"

	"github.com/Project-Helianthus/helianthus-semreg/semreg/runtime/tree"
	semreg "github.com/Project-Helianthus/helianthus-semreg/semreg/v1"
	"github.com/Project-Helianthus/helianthus-semreg/semreg/v1/driver"
)

const (
	driverNamespacesListTool = "driver_namespaces_list"
	driverMembersListTool    = "driver_members_list"
	driverMemberGetTool      = "driver_member_get"
	driverMethodCallTool     = "driver_method_call"
)

func driverMemberTools() []Tool {
	text := map[string]any{"type": "string"}
	key := map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{"namespace": text, "instance": text, "node": text, "local": text},
		"required":   []string{"namespace", "instance", "node", "local"},
	}
	return []Tool{
		{Name: driverNamespacesListTool, Description: "List every namespace a driver declares with all its instances, including instances with no members (OPERATOR items 6 and 13).", InputSchema: map[string]any{
			"type": "object", "additionalProperties": false, "properties": map[string]any{},
		}},
		{Name: driverMembersListTool, Description: "List the members of a driver namespace by instance and node, in pages pinned to one cut; send the returned cut and next token to continue (OPERATOR items 6 and 13).", InputSchema: map[string]any{
			"type": "object", "additionalProperties": false,
			"properties": map[string]any{
				"namespace": text, "instance": text, "node": text,
				"kind":       map[string]any{"type": "string", "enum": []string{"node", "attribute", "method", "view"}},
				"page_size":  map[string]any{"type": "integer", "minimum": 1},
				"page_token": text, "cut": text,
			},
			"required": []string{"namespace", "instance", "node", "page_size"},
		}},
		{Name: driverMemberGetTool, Description: "Read a driver's published attribute by its shared member key (OPERATOR items 6 and 13).", InputSchema: map[string]any{
			"type": "object", "additionalProperties": false, "properties": map[string]any{"key": key}, "required": []string{"key"},
		}},
		{Name: driverMethodCallTool, Description: "Call a driver method or a canonical SemReg operation (OPERATOR items 13 and 53).", InputSchema: map[string]any{
			"type": "object", "additionalProperties": false,
			"properties": map[string]any{"key": key, "input": map[string]any{"type": "string", "contentEncoding": "base64"}, "method": text, "request": map[string]any{"type": "object"}},
			"oneOf": []map[string]any{
				{"required": []string{"key"}, "properties": map[string]any{"method": false, "request": false}},
				{"required": []string{"method", "request"}, "properties": map[string]any{"key": false, "input": false}},
			},
		}},
	}
}

func nativeMemberRef(member driver.MemberKey, facet tree.Facet) (tree.Ref, error) {
	if err := member.Validate(); err != nil {
		return "", &semreg.Error{ID: semreg.InvalidValue, Detail: err.Error()}
	}
	node, err := tree.ParseNodePath("/" + string(member.Node))
	if err != nil {
		return "", err
	}
	if node.Instance != member.Instance {
		return "", &semreg.Error{ID: semreg.InvalidValue, Detail: "member instance does not match the node"}
	}
	segment, err := tree.NativeSegment(member.Namespace, member.Local)
	if err != nil {
		return "", err
	}
	return tree.Ref(tree.Key{Node: node, Facet: facet, Domain: tree.KeywordNative, Segment: segment}.String()), nil
}

type driverMemberValue struct {
	Descriptor tree.Descriptor `json:"descriptor"`
	Value      tree.Value      `json:"value"`
}

// These records are assembled from the tree's typed outputs, not client input.
func (driverMemberValue) Validate() error { return nil }

type driverNativeOutcome struct {
	Native []byte `json:"native,omitempty"`
	Error  string `json:"error,omitempty"`
}

func (driverNativeOutcome) Validate() error { return nil }

func (s *Server) readDriverMember(ctx context.Context, call rawCallToolParams, duplicate bool) (semreg.Record, error) {
	var args struct {
		Key driver.MemberKey `json:"key"`
	}
	if err := decodeTreeArguments(call.Arguments, duplicate, &args); err != nil {
		return nil, err
	}
	ref, err := nativeMemberRef(args.Key, tree.KeywordAttributes)
	if err != nil {
		return nil, err
	}
	cut, err := s.semanticGraph.Capture(ctx, tree.CutOptions{})
	if err != nil {
		return nil, err
	}
	defer func() { _ = cut.Close() }()
	descriptor, err := cut.Describe(ctx, ref)
	if err != nil {
		return nil, err
	}
	value, err := cut.Read(ctx, ref, tree.ReadOptions{})
	return driverMemberValue{descriptor, value}, err
}

func (s *Server) callDriverMethod(ctx context.Context, call rawCallToolParams, duplicate bool) (semreg.Record, error) {
	var args struct {
		Key     *driver.MemberKey        `json:"key"`
		Input   []byte                   `json:"input"`
		Method  tree.MethodRef           `json:"method"`
		Request *driver.OperationRequest `json:"request"`
	}
	if err := decodeTreeArguments(call.Arguments, duplicate, &args); err != nil {
		return nil, err
	}
	var ref tree.MethodRef
	if args.Key != nil && args.Method == "" && args.Request == nil {
		var err error
		ref, err = nativeMemberRef(*args.Key, tree.KeywordMethods)
		if err != nil {
			return nil, err
		}
	} else if args.Key == nil && args.Input == nil && args.Method != "" && args.Request != nil {
		key, err := tree.ParseKey(string(args.Method))
		if err != nil {
			return nil, err
		}
		if key.Facet != tree.KeywordMethods || key.Domain != tree.KeywordSem {
			return nil, &semreg.Error{ID: semreg.InvalidValue, Detail: "method must name a semantic operation"}
		}
		definition, err := tree.ParseDefinitionSegment(key.Segment)
		if err != nil {
			return nil, err
		}
		if definition != args.Request.Operation {
			return nil, &semreg.Error{ID: semreg.InvalidValue, Detail: "request operation does not match the method"}
		}
		ref = args.Method
	} else {
		return nil, &semreg.Error{ID: semreg.InvalidValue, Detail: "supply a native key or a semantic method with its request"}
	}
	runtime := s.semanticGraph
	cut, err := runtime.Capture(ctx, tree.CutOptions{})
	if err != nil {
		return nil, err
	}
	defer func() { _ = cut.Close() }()
	if args.Request != nil {
		if _, err := cut.Describe(ctx, ref); err != nil {
			return nil, err
		}
	}
	prepared, err := runtime.Prepare(ctx, cut.ID(), ref, tree.Input{Encoded: args.Input, Request: args.Request})
	if err != nil {
		return nil, err
	}
	receipt, err := runtime.Call(ctx, prepared, tree.CallOptions{})
	if err != nil {
		return nil, err
	}
	if receipt.Result != nil {
		return receipt.Result, nil
	}
	outcome, err := runtime.Outcome(ctx, receipt.ID)
	if err != nil {
		return nil, err
	}
	if outcome.Operation != nil {
		return outcome.Operation, nil
	}
	return driverNativeOutcome{outcome.Native, outcome.Error}, nil
}

type driverNamespaceEntry struct {
	Owner     driver.DriverKind    `json:"owner"`
	Key       driver.NamespaceID   `json:"namespace"`
	Instances []driver.InstanceKey `json:"instances"`
}

type driverNamespaceList struct {
	Cut        tree.CutID             `json:"cut"`
	Namespaces []driverNamespaceEntry `json:"namespaces"`
}

// These records are assembled from the tree's typed outputs, not client input.
func (driverNamespaceList) Validate() error { return nil }

func (s *Server) listDriverNamespaces(ctx context.Context, call rawCallToolParams, duplicate bool) (semreg.Record, error) {
	var args struct{}
	if err := decodeTreeArguments(call.Arguments, duplicate, &args); err != nil {
		return nil, err
	}
	cut, err := s.semanticGraph.Capture(ctx, tree.CutOptions{})
	if err != nil {
		return nil, err
	}
	defer func() { _ = cut.Close() }()
	roster := cut.Roster()
	out := driverNamespaceList{Cut: cut.ID(), Namespaces: make([]driverNamespaceEntry, len(roster))}
	for i, entry := range roster {
		out.Namespaces[i] = driverNamespaceEntry{Owner: entry.Owner, Key: entry.Key, Instances: entry.Instances}
	}
	return out, nil
}

type driverMemberEntry struct {
	Key        *driver.MemberKey `json:"key,omitempty"`
	Kind       tree.EntryKind    `json:"kind"`
	Mode       driver.AccessMode `json:"mode,omitempty"`
	Descriptor tree.Descriptor   `json:"descriptor"`
}

type driverMemberPage struct {
	Cut     tree.CutID          `json:"cut"`
	Members []driverMemberEntry `json:"members"`
	Next    string              `json:"next,omitempty"`
}

// These records are assembled from the tree's typed outputs, not client input.
func (driverMemberPage) Validate() error { return nil }

func (s *Server) listDriverMembers(ctx context.Context, call rawCallToolParams, duplicate bool) (semreg.Record, error) {
	var args struct {
		Namespace driver.NamespaceID `json:"namespace"`
		Instance  driver.InstanceKey `json:"instance"`
		Node      driver.NodeID      `json:"node"`
		Kind      tree.EntryKind     `json:"kind"`
		PageSize  int                `json:"page_size"`
		PageToken string             `json:"page_token"`
		Cut       tree.CutID         `json:"cut"`
	}
	if err := decodeTreeArguments(call.Arguments, duplicate, &args); err != nil {
		return nil, err
	}
	if args.Namespace == "" || args.Instance == "" || args.Node == "" {
		return nil, &semreg.Error{ID: semreg.InvalidValue, Detail: "namespace, instance and node are required"}
	}
	var cut *tree.TreeCut
	var err error
	if args.Cut != "" {
		cut, err = s.semanticGraph.CutByID(args.Cut)
	} else {
		cut, err = s.semanticGraph.Capture(ctx, tree.CutOptions{})
	}
	if err != nil {
		return nil, err
	}
	release := true
	defer func() {
		if release {
			_ = cut.Close()
		}
	}()
	declared := false
	for _, entry := range cut.Roster() {
		if entry.Key != args.Namespace {
			continue
		}
		for _, instance := range entry.Instances {
			declared = declared || instance == args.Instance
		}
	}
	if !declared {
		return nil, &semreg.Error{ID: semreg.DanglingReference, Detail: "namespace instance is not declared"}
	}
	node := tree.NodeRef{Bridge: cut.InstallationRoot().Bridge, ID: semreg.ResourceID(args.Node)}
	listed, err := cut.List(ctx, node, tree.Page{Size: args.PageSize, Token: args.PageToken, Filter: tree.Filter{Instance: &args.Instance, Kind: args.Kind}})
	if err != nil {
		return nil, err
	}
	out := driverMemberPage{Cut: listed.Cut, Members: make([]driverMemberEntry, 0, len(listed.Entries)), Next: listed.Next}
	for _, d := range listed.Entries {
		if d.Namespace != args.Namespace {
			continue
		}
		entry := driverMemberEntry{Kind: d.Kind, Mode: d.Mode, Descriptor: d}
		if d.Kind == tree.EntryAttribute || d.Kind == tree.EntryMethod {
			entry.Key = &driver.MemberKey{Namespace: d.Namespace, Instance: d.Instance.Instance, Node: args.Node, Local: driver.MemberID(d.Local)}
		}
		out.Members = append(out.Members, entry)
	}
	release = listed.Next == ""
	return out, nil
}
