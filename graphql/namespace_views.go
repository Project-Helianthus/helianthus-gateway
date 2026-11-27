package graphql

import (
	"bytes"
	"encoding/json"
	"reflect"
	"regexp"
	"sort"

	"github.com/Project-Helianthus/helianthus-semreg/semreg/runtime/tree"
	semreg "github.com/Project-Helianthus/helianthus-semreg/semreg/v1"
	"github.com/Project-Helianthus/helianthus-semreg/semreg/v1/driver"
	gql "github.com/graphql-go/graphql"
)

var namespaceViewName = regexp.MustCompile(`^[_A-Za-z][_0-9A-Za-z]*$`)

// namespaceViewRoot is the source of a view's root record: its fields read the
// bound members of one cut, and nothing else.
type namespaceViewRoot struct {
	cut    *tree.TreeCut
	record tree.ViewRecord
}

type namespaceViewTypes struct {
	objects map[string]*gql.Object
	all     map[string]tree.ViewRecord
}

// namespaceViewFields adds one query field per declared view root, named
// <namespace>_view_<root>, taking the instance that declared the view. The
// record, field, type, nesting and unit of each GraphQL type are those the
// driver declared; the engine holds no knowledge of any driver.
func namespaceViewFields(query gql.Fields, cut *tree.TreeCut, views []tree.Descriptor) error {
	groups := map[string]map[driver.InstanceKey]*tree.ViewDescription{}
	namespaces := map[string]driver.NamespaceID{}
	for _, d := range views {
		if d.View == nil {
			return namespaceInvalid("view descriptor has no declaration")
		}
		name := string(d.Namespace) + "_view_" + d.View.Root
		if !namespaceViewName.MatchString(name) {
			return namespaceInvalid("view root is not a GraphQL name: " + d.View.Root)
		}
		if groups[name] == nil {
			groups[name] = map[driver.InstanceKey]*tree.ViewDescription{}
		}
		groups[name][d.Instance.Instance] = d.View
		namespaces[name] = d.Namespace
	}
	types := &namespaceViewTypes{objects: map[string]*gql.Object{}, all: map[string]tree.ViewRecord{}}
	names := make([]string, 0, len(groups))
	for name := range groups {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		instances := groups[name]
		var shape *tree.ViewDescription
		for _, view := range instances {
			if shape == nil {
				shape = view
			} else if !reflect.DeepEqual(viewShapeOnly(shape), viewShapeOnly(view)) {
				return namespaceInvalid("instances declare different shapes for " + name)
			}
			for _, record := range view.Records {
				if held, ok := types.all[record.Name]; ok && !reflect.DeepEqual(viewRecordShape(held), viewRecordShape(record)) {
					return namespaceInvalid("record " + record.Name + " is declared with different shapes")
				}
				types.all[record.Name] = record
			}
		}
		root, err := types.object(shape.Root)
		if err != nil {
			return err
		}
		views := instances
		query[name] = &gql.Field{
			Type: root,
			Args: gql.FieldConfigArgument{"instance": &gql.ArgumentConfig{Type: gql.NewNonNull(gql.String)}},
			Resolve: func(p gql.ResolveParams) (any, error) {
				view := views[driver.InstanceKey(p.Args["instance"].(string))]
				if view == nil {
					return nil, namespaceErrorFrom(&semreg.Error{ID: semreg.DanglingReference, Detail: "no such view instance"})
				}
				for _, record := range view.Records {
					if record.Name == view.Root {
						return namespaceViewRoot{cut: cut, record: record}, nil
					}
				}
				return nil, namespaceInvalid("view root is not declared")
			},
		}
	}
	return nil
}

func viewRecordShape(r tree.ViewRecord) tree.ViewRecord {
	out := tree.ViewRecord{Name: r.Name, Fields: make([]tree.ViewField, len(r.Fields))}
	for i, f := range r.Fields {
		out.Fields[i] = tree.ViewField{Name: f.Name, Type: f.Type, Unit: f.Unit}
	}
	return out
}

func viewShapeOnly(v *tree.ViewDescription) tree.ViewDescription {
	out := tree.ViewDescription{Root: v.Root}
	for _, r := range v.Records {
		out.Records = append(out.Records, viewRecordShape(r))
	}
	return out
}

func (t *namespaceViewTypes) object(name string) (*gql.Object, error) {
	if held := t.objects[name]; held != nil {
		return held, nil
	}
	record, ok := t.all[name]
	if !ok {
		return nil, namespaceInvalid("view record is not declared: " + name)
	}
	if !namespaceViewName.MatchString(name) {
		return nil, namespaceInvalid("view record is not a GraphQL name: " + name)
	}
	for _, f := range record.Fields {
		if !namespaceViewName.MatchString(f.Name) {
			return nil, namespaceInvalid("view field is not a GraphQL name: " + f.Name)
		}
		if _, err := t.output(f.Type); err != nil {
			return nil, err
		}
	}
	object := gql.NewObject(gql.ObjectConfig{Name: name, Fields: gql.FieldsThunk(func() gql.Fields {
		fields := make(gql.Fields, len(record.Fields))
		for _, f := range record.Fields {
			out, _ := t.output(f.Type)
			field := &gql.Field{Type: out, Resolve: namespaceViewResolve(f)}
			if f.Unit != "" {
				field.Description = "unit: " + f.Unit
			}
			fields[f.Name] = field
		}
		return fields
	})})
	t.objects[name] = object
	return object, nil
}

func (t *namespaceViewTypes) output(typ driver.ViewType) (gql.Output, error) {
	var out gql.Output
	if typ.Record {
		object, err := t.object(typ.Name)
		if err != nil {
			return nil, err
		}
		out = object
	} else {
		switch typ.Name {
		case "String":
			out = gql.String
		case "Int":
			out = gql.Int
		case "Float":
			out = gql.Float
		case "Boolean":
			out = gql.Boolean
		default:
			return nil, namespaceInvalid("view scalar is not served: " + typ.Name)
		}
	}
	if typ.List {
		out = gql.NewList(gql.NewNonNull(out))
	}
	if !typ.Nullable {
		out = gql.NewNonNull(out)
	}
	return out, nil
}

func namespaceViewResolve(f tree.ViewField) gql.FieldResolveFn {
	return func(p gql.ResolveParams) (any, error) {
		switch source := p.Source.(type) {
		case namespaceViewRoot:
			for _, bound := range source.record.Fields {
				if bound.Name != f.Name {
					continue
				}
				value, err := source.cut.Read(p.Context, bound.Member, tree.ReadOptions{})
				if err != nil {
					return nil, namespaceErrorFrom(err)
				}
				return namespaceViewDecode(f, value.Encoded)
			}
			return nil, nil
		case map[string]any:
			return namespaceViewCoerce(f.Name, f.Type, source[f.Name])
		}
		return nil, nil
	}
}

func namespaceViewDecode(f tree.ViewField, encoded []byte) (any, error) {
	var raw any
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	if err := decoder.Decode(&raw); err != nil {
		return nil, namespaceInvalid("view member is not JSON for " + f.Name)
	}
	return namespaceViewCoerce(f.Name, f.Type, raw)
}

// A record-typed value is the member's JSON object; its nested fields are the
// object's members of the same names.
func namespaceViewCoerce(name string, typ driver.ViewType, raw any) (any, error) {
	if raw == nil {
		if typ.Nullable {
			return nil, nil
		}
		return nil, namespaceInvalid("view field is null: " + name)
	}
	if typ.List {
		items, ok := raw.([]any)
		if !ok {
			return nil, namespaceInvalid("view field is not a list: " + name)
		}
		element := typ
		element.List, element.Nullable = false, false
		out := make([]any, len(items))
		for i, item := range items {
			v, err := namespaceViewCoerce(name, element, item)
			if err != nil {
				return nil, err
			}
			out[i] = v
		}
		return out, nil
	}
	bad := namespaceInvalid("view field has the wrong type: " + name)
	switch {
	case typ.Record:
		if object, ok := raw.(map[string]any); ok {
			return object, nil
		}
	case typ.Name == "String":
		if s, ok := raw.(string); ok {
			return s, nil
		}
	case typ.Name == "Boolean":
		if b, ok := raw.(bool); ok {
			return b, nil
		}
	case typ.Name == "Int":
		if n, ok := raw.(json.Number); ok {
			if i, err := n.Int64(); err == nil && int64(int32(i)) == i {
				return int(i), nil
			}
		}
	case typ.Name == "Float":
		if n, ok := raw.(json.Number); ok {
			if f, err := n.Float64(); err == nil {
				return f, nil
			}
		}
	}
	return nil, bad
}
