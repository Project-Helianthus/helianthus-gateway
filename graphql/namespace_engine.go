package graphql

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"

	"github.com/Project-Helianthus/helianthus-semreg/semreg/runtime/tree"
	semreg "github.com/Project-Helianthus/helianthus-semreg/semreg/v1"
	"github.com/Project-Helianthus/helianthus-semreg/semreg/v1/driver"
	gql "github.com/graphql-go/graphql"
)

// NamespaceFields builds the generic fields for one served installation cut.
// The caller owns the cut and serves the fields to the principal that captured
// it. Pass the schema's semantic_operation field, if present, to share its
// named GraphQL types. Neither root schema nor host composition is changed here.
func NamespaceFields(ctx context.Context, runtime *tree.Tree, cut *tree.TreeCut, root tree.NodeRef, semanticOperation *gql.Field) (query, mutation gql.Fields, err error) {
	if runtime == nil || cut == nil {
		return nil, nil, errors.New("namespace fields require a runtime and its cut")
	}
	if semanticOperation == nil {
		semanticOperation = semanticOperationField(nil)
	}
	types := newNamespaceTypes()
	listName, err := tree.TreeName(tree.SurfaceGraphQL, tree.VerbList)
	if err != nil {
		return nil, nil, err
	}
	query = gql.Fields{listName: namespaceListField(cut, root, types)}
	calls, err := newNamespaceCalls(runtime, cut, types.keyInput, semanticOperation)
	if err != nil {
		return nil, nil, err
	}

	// Namespace plan §§2–3: tree_list walks the registry's nodes, while
	// the native verbs take the names of the published namespace members.
	queue := []tree.NodeRef{root}
	var views []tree.Descriptor
	for len(queue) != 0 {
		node := queue[0]
		queue = queue[1:]
		page, err := cut.List(ctx, node, tree.Page{Size: math.MaxInt})
		if err != nil {
			return nil, nil, err
		}
		for _, d := range page.Entries {
			if d.Kind == tree.EntryNode {
				queue = append(queue, namespaceNodeRef(root, string(d.Ref)))
				continue
			}
			if d.Kind == tree.EntryView {
				views = append(views, d)
				continue
			}
			if d.Domain != tree.KeywordNative {
				continue
			}
			describe, err := tree.DriverNames(d.Namespace, tree.VerbDescribe, string(d.Ref))
			if err != nil {
				return nil, nil, err
			}
			if query[describe.GraphQL] == nil {
				query[describe.GraphQL] = namespaceDescribeField(cut, d.Namespace, types)
			}
			if d.Kind == tree.EntryAttribute {
				read, err := tree.DriverNames(d.Namespace, tree.VerbRead, string(d.Ref))
				if err != nil {
					return nil, nil, err
				}
				if query[read.GraphQL] == nil {
					query[read.GraphQL] = namespaceReadField(cut, d.Namespace, types)
				}
			} else if err := calls.addNative(d); err != nil {
				return nil, nil, err
			}
		}
	}
	if err := namespaceViewFields(query, cut, views); err != nil {
		return nil, nil, err
	}
	return query, calls.fields, nil
}

type namespaceTypes struct {
	keyInput                    *gql.InputObject
	facets                      *gql.Enum
	attribute, descriptor, page *gql.Object
}

func namespaceObject(name string, members map[string]gql.Output) *gql.Object {
	fields := make(gql.Fields, len(members))
	for name, typ := range members {
		fields[name] = &gql.Field{Type: typ}
	}
	return gql.NewObject(gql.ObjectConfig{Name: name, Fields: fields})
}

func newNamespaceTypes() namespaceTypes {
	text := gql.String
	requiredText := gql.NewNonNull(text)
	requiredBool := gql.NewNonNull(gql.Boolean)
	keys := map[string]gql.Output{}
	input := gql.InputObjectConfigFieldMap{}
	for _, name := range []string{"namespace", "instance", "node", "local"} {
		keys[name] = requiredText
		input[name] = &gql.InputObjectFieldConfig{Type: requiredText}
	}
	key := namespaceObject("member_key", keys)
	keyInput := gql.NewInputObject(gql.InputObjectConfig{Name: "member_key_input", Fields: input})
	facets := gql.NewEnum(gql.EnumConfig{Name: "tree_member_facet", Values: gql.EnumValueConfigMap{
		"attributes": &gql.EnumValueConfig{Value: tree.KeywordAttributes}, "methods": &gql.EnumValueConfig{Value: tree.KeywordMethods},
	}})
	records := map[reflect.Type]*gql.Object{}
	state := namespaceRecordType(reflect.TypeOf(tree.InstanceState{}), records)
	clock := namespaceRecordType(reflect.TypeOf(semreg.TimePoint{}), records)
	bounds := namespaceRecordType(reflect.TypeOf(driver.ValueLimits{}), records)
	subject := namespaceRecordType(reflect.TypeOf(driver.NodeRef{}), records)
	descriptor := namespaceObject("tree_descriptor", map[string]gql.Output{
		"ref": requiredText, "key": key, "kind": requiredText, "domain": text, "level": text,
		"namespace": text, "instance": text, "driver": text, "node_kind": text, "contract": text,
		"subject": subject, "local": text, "schema": text, "unit": text, "mode": text,
		"visibility": text, "channel": text, "scope": text, "bounds": bounds, "state": state, "withdrawn": requiredBool,
	})
	value := namespaceObject("tree_value", map[string]gql.Output{
		"ref": requiredText, "cut": requiredText, "status": requiredText, "pinned": requiredBool,
		"encoded": text, "raw": text, "unit": text, "verification": text, "received_at": clock,
		"freshness": text, "age": text, "revision": text, "derived": requiredBool, "name": text, "instance": state,
	})
	node := namespaceObject("tree_node", map[string]gql.Output{"descriptor": gql.NewNonNull(descriptor)})
	attribute := namespaceObject("attribute", map[string]gql.Output{"descriptor": gql.NewNonNull(descriptor), "value": value})
	method := namespaceObject("method", map[string]gql.Output{"descriptor": gql.NewNonNull(descriptor)})
	entry := namespaceObject("tree_entry", map[string]gql.Output{"node": node, "attribute": attribute, "method": method})
	page := namespaceObject("tree_page", map[string]gql.Output{
		"cut": requiredText, "node": gql.NewNonNull(node), "next": text,
		"entries": gql.NewNonNull(gql.NewList(gql.NewNonNull(entry))),
	})
	return namespaceTypes{keyInput: keyInput, facets: facets, attribute: attribute, descriptor: descriptor, page: page}
}

// Keep the JSON names and types of the records nested in the tree envelopes.
// Wide integers are decimal strings, not lossy GraphQL Ints.
func namespaceRecordType(t reflect.Type, records map[reflect.Type]*gql.Object) gql.Output {
	switch t.Kind() {
	case reflect.Pointer:
		return namespaceRecordType(t.Elem(), records)
	case reflect.String, reflect.Uint64:
		return gql.String
	case reflect.Int32:
		return gql.Int
	case reflect.Bool:
		return gql.Boolean
	case reflect.Slice:
		return gql.NewList(gql.NewNonNull(namespaceRecordType(t.Elem(), records)))
	case reflect.Struct:
		if record := records[t]; record != nil {
			return record
		}
		fields := map[string]gql.Output{}
		for name, member := range semanticInputMembers(t) {
			out := namespaceRecordType(member.kind, records)
			if !member.optional {
				out = gql.NewNonNull(out)
			}
			fields[name] = out
		}
		record := namespaceObject("tree_"+semanticTypeName(t.Name()), fields)
		records[t] = record
		return record
	default:
		panic("unsupported tree record field: " + t.String())
	}
}

func namespaceRecords(result map[string]any, records map[string]any) error {
	for name, record := range records {
		encoded, err := json.Marshal(record)
		if err != nil {
			return err
		}
		var value any
		decoder := json.NewDecoder(strings.NewReader(string(encoded)))
		decoder.UseNumber()
		if err := decoder.Decode(&value); err != nil {
			return err
		}
		result[name] = value
	}
	return nil
}

func namespaceDescriptorValue(d tree.Descriptor) (map[string]any, error) {
	result := map[string]any{
		"ref": string(d.Ref), "kind": string(d.Kind), "domain": compoundEvidenceValue(d.Domain), "level": compoundEvidenceValue(d.Level),
		"namespace": compoundEvidenceValue(d.Namespace), "instance": compoundEvidenceValue(d.Instance.Instance), "driver": compoundEvidenceValue(d.Instance.Driver),
		"node_kind": compoundEvidenceValue(d.NodeKind), "contract": compoundEvidenceValue(d.Contract), "local": compoundEvidenceValue(d.Local),
		"schema": compoundEvidenceValue(d.Schema), "unit": compoundEvidenceValue(d.Unit), "mode": compoundEvidenceValue(d.Mode),
		"visibility": compoundEvidenceValue(d.Visibility), "channel": compoundEvidenceValue(d.Channel), "scope": compoundEvidenceValue(d.Scope), "withdrawn": d.Withdrawn,
	}
	if d.Domain == tree.KeywordNative {
		key, err := tree.ParseKey(string(d.Ref))
		if err != nil {
			return nil, err
		}
		result["key"] = map[string]any{"namespace": string(d.Namespace), "instance": string(key.Node.Instance), "node": string(key.Node.NodeID()), "local": d.Local}
	}
	return result, namespaceRecords(result, map[string]any{"subject": d.Subject, "bounds": d.Bounds, "state": d.State})
}

func namespaceValue(v tree.Value) (map[string]any, error) {
	result := map[string]any{
		"ref": string(v.Ref), "cut": string(v.Cut), "status": string(v.Status), "pinned": v.Pinned,
		"encoded": namespaceBytes(v.Encoded), "raw": namespaceBytes(v.Raw), "unit": compoundEvidenceValue(v.Unit),
		"verification": compoundEvidenceValue(v.Verification), "freshness": compoundEvidenceValue(v.Freshness),
		"age": semanticOptional(v.Age), "revision": semanticOptional(v.Revision), "derived": v.Derived, "name": compoundEvidenceValue(v.Name),
	}
	return result, namespaceRecords(result, map[string]any{"received_at": v.ReceivedAt, "instance": v.Instance})
}

func namespaceBytes(value []byte) any {
	if value == nil {
		return nil
	}
	return base64.StdEncoding.EncodeToString(value)
}

type namespaceError struct {
	err  error
	code string
}

func (e namespaceError) Error() string              { return e.err.Error() }
func (e namespaceError) Extensions() map[string]any { return map[string]any{"code": e.code} }

func namespaceErrorFrom(err error) error {
	if err == nil {
		return nil
	}
	var semantic *semreg.Error
	if errors.As(err, &semantic) {
		return namespaceError{err: err, code: string(semantic.ID)}
	}
	return namespaceError{err: errors.New("tree request unavailable"), code: "unavailable"}
}

func namespaceInvalid(detail string) error {
	return namespaceErrorFrom(&semreg.Error{ID: semreg.InvalidValue, Detail: detail})
}

// Same MemberKey to SS09 conversion as the MCP native member tools.
func namespaceMemberRef(raw any, namespace driver.NamespaceID, facet tree.Facet) (tree.Ref, error) {
	args, _ := raw.(map[string]any)
	text := func(name string) string { value, _ := args[name].(string); return value }
	member := driver.MemberKey{Namespace: driver.NamespaceID(text("namespace")), Instance: driver.InstanceKey(text("instance")), Node: driver.NodeID(text("node")), Local: driver.MemberID(text("local"))}
	if err := member.Validate(); err != nil {
		return "", namespaceInvalid(err.Error())
	}
	if member.Namespace != namespace {
		return "", namespaceInvalid("member namespace does not match the field")
	}
	node, err := tree.ParseNodePath("/" + string(member.Node))
	if err != nil {
		return "", namespaceErrorFrom(err)
	}
	if node.Instance != member.Instance {
		return "", namespaceInvalid("member instance does not match the node")
	}
	segment, err := tree.NativeSegment(member.Namespace, member.Local)
	if err != nil {
		return "", namespaceErrorFrom(err)
	}
	return tree.Ref(tree.Key{Node: node, Facet: facet, Domain: tree.KeywordNative, Segment: segment}.String()), nil
}

func namespaceNodeRef(root tree.NodeRef, ref string) tree.NodeRef {
	return tree.NodeRef{Bridge: root.Bridge, ID: semreg.ResourceID(strings.TrimPrefix(ref, "/"))}
}

func namespaceListField(cut *tree.TreeCut, root tree.NodeRef, types namespaceTypes) *gql.Field {
	return &gql.Field{
		Type: types.page,
		Args: gql.FieldConfigArgument{
			"node":       &gql.ArgumentConfig{Type: gql.NewNonNull(gql.String)},
			"page_size":  &gql.ArgumentConfig{Type: gql.NewNonNull(gql.Int)},
			"page_token": &gql.ArgumentConfig{Type: gql.String},
		},
		Resolve: func(p gql.ResolveParams) (any, error) {
			ref := p.Args["node"].(string)
			if _, err := tree.ParseNodePath(ref); err != nil {
				return nil, namespaceErrorFrom(err)
			}
			token, _ := p.Args["page_token"].(string)
			page, err := cut.List(p.Context, namespaceNodeRef(root, ref), tree.Page{Size: p.Args["page_size"].(int), Token: token})
			if err != nil {
				return nil, namespaceErrorFrom(err)
			}
			d, err := cut.Describe(p.Context, tree.Ref(ref))
			if err != nil {
				return nil, namespaceErrorFrom(err)
			}
			descriptor, err := namespaceDescriptorValue(d)
			if err != nil {
				return nil, namespaceErrorFrom(err)
			}
			entries := make([]map[string]any, 0, len(page.Entries))
			for _, d := range page.Entries {
				data, err := namespaceDescriptorValue(d)
				if err != nil {
					return nil, namespaceErrorFrom(err)
				}
				entries = append(entries, map[string]any{string(d.Kind): map[string]any{"descriptor": data}})
			}
			return map[string]any{"cut": string(page.Cut), "node": map[string]any{"descriptor": descriptor}, "entries": entries, "next": compoundEvidenceValue(page.Next)}, nil
		},
	}
}

func namespaceDescribeField(cut *tree.TreeCut, namespace driver.NamespaceID, types namespaceTypes) *gql.Field {
	return &gql.Field{
		Type: types.descriptor,
		Args: gql.FieldConfigArgument{
			"key":   &gql.ArgumentConfig{Type: gql.NewNonNull(types.keyInput)},
			"facet": &gql.ArgumentConfig{Type: gql.NewNonNull(types.facets)},
		},
		Resolve: func(p gql.ResolveParams) (any, error) {
			ref, err := namespaceMemberRef(p.Args["key"], namespace, p.Args["facet"].(tree.Facet))
			if err != nil {
				return nil, err
			}
			descriptor, err := cut.Describe(p.Context, ref)
			if err != nil {
				return nil, namespaceErrorFrom(err)
			}
			value, err := namespaceDescriptorValue(descriptor)
			return value, namespaceErrorFrom(err)
		},
	}
}

func namespaceReadField(cut *tree.TreeCut, namespace driver.NamespaceID, types namespaceTypes) *gql.Field {
	return &gql.Field{
		Type: types.attribute,
		Args: gql.FieldConfigArgument{"key": &gql.ArgumentConfig{Type: gql.NewNonNull(types.keyInput)}},
		Resolve: func(p gql.ResolveParams) (any, error) {
			ref, err := namespaceMemberRef(p.Args["key"], namespace, tree.KeywordAttributes)
			if err != nil {
				return nil, err
			}
			d, err := cut.Describe(p.Context, ref)
			if err != nil {
				return nil, namespaceErrorFrom(err)
			}
			v, err := cut.Read(p.Context, ref, tree.ReadOptions{})
			if err != nil {
				return nil, namespaceErrorFrom(err)
			}
			descriptor, err := namespaceDescriptorValue(d)
			if err != nil {
				return nil, namespaceErrorFrom(err)
			}
			value, err := namespaceValue(v)
			return map[string]any{"descriptor": descriptor, "value": value}, namespaceErrorFrom(err)
		},
	}
}
