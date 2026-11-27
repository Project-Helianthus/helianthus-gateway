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
	// reserved holds the type names the other fields of the engine already use.
	reserved map[string]bool
}

func namespaceViewReserve(query gql.Fields) map[string]bool {
	seen := map[string]bool{}
	var walk func(t gql.Type)
	walk = func(t gql.Type) {
		switch v := t.(type) {
		case *gql.List:
			walk(v.OfType)
		case *gql.NonNull:
			walk(v.OfType)
		case *gql.Object:
			if seen[v.Name()] {
				return
			}
			seen[v.Name()] = true
			for _, f := range v.Fields() {
				walk(f.Type)
				for _, a := range f.Args {
					walk(a.Type)
				}
			}
		case *gql.InputObject:
			if seen[v.Name()] {
				return
			}
			seen[v.Name()] = true
			for _, f := range v.Fields() {
				walk(f.Type)
			}
		case *gql.Scalar:
			seen[v.Name()] = true
		case *gql.Enum:
			seen[v.Name()] = true
		case *gql.Interface:
			seen[v.Name()] = true
		case *gql.Union:
			seen[v.Name()] = true
		}
	}
	for _, f := range query {
		walk(f.Type)
		for _, a := range f.Args {
			walk(a.Type)
		}
	}
	return seen
}

// namespaceViewFields adds one query field per declared view root, named
// <namespace>_view_<root>, taking the instance that declared the view. The
// record, field, type, nesting and unit of each GraphQL type are those the
// driver declared; the engine holds no knowledge of any driver. A declaration
// that cannot be served (a name that is not a GraphQL name, a shape that
// conflicts with another declaration, a scalar the engine does not serve, or a
// field name that two declarations share) is answered by its own field with the
// stable error, and never disturbs the other fields of the engine.
func namespaceViewFields(query gql.Fields, cut *tree.TreeCut, views []tree.Descriptor) error {
	type group struct {
		namespace driver.NamespaceID
		root      string
		instances map[driver.InstanceKey]*tree.ViewDescription
		problem   string
	}
	groups := map[string]*group{}
	for _, d := range views {
		if d.View == nil {
			return namespaceInvalid("view descriptor has no declaration")
		}
		name := string(d.Namespace) + "_view_" + d.View.Root
		g := groups[name]
		if g == nil {
			g = &group{namespace: d.Namespace, root: d.View.Root, instances: map[driver.InstanceKey]*tree.ViewDescription{}}
			groups[name] = g
		} else if g.namespace != d.Namespace || g.root != d.View.Root {
			g.problem = "two declarations share the field name " + name
		}
		g.instances[d.Instance.Instance] = d.View
		if !namespaceViewName.MatchString(name) && g.problem == "" {
			g.problem = "view root is not a GraphQL name: " + d.View.Root
		}
	}
	types := &namespaceViewTypes{objects: map[string]*gql.Object{}, all: map[string]tree.ViewRecord{}, reserved: namespaceViewReserve(query)}
	names := make([]string, 0, len(groups))
	for name := range groups {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if query[name] != nil && groups[name].problem == "" {
			continue // an existing field is never overwritten
		}
		g := groups[name]
		var shape *tree.ViewDescription
		if g.problem == "" {
			g.problem = types.admit(g.instances, &shape, name)
		}
		if g.problem != "" {
			message := g.problem
			query[name] = &gql.Field{
				Type:    gql.String,
				Resolve: func(gql.ResolveParams) (any, error) { return nil, namespaceInvalid(message) },
			}
			continue
		}
		views := g.instances
		root := types.objects[shape.Root]
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

// admit checks one group against the records already held and builds its
// types. It returns the reason the group cannot be served, and leaves the held
// records and types untouched in that case.
func (t *namespaceViewTypes) admit(instances map[driver.InstanceKey]*tree.ViewDescription, shape **tree.ViewDescription, name string) string {
	pending := map[string]tree.ViewRecord{}
	for _, view := range instances {
		if *shape == nil {
			*shape = view
		} else if !reflect.DeepEqual(viewShapeOnly(*shape), viewShapeOnly(view)) {
			return "instances declare different shapes for " + name
		}
		for _, record := range view.Records {
			if held, ok := t.all[record.Name]; ok && !reflect.DeepEqual(viewRecordShape(held), viewRecordShape(record)) {
				return "record " + record.Name + " is declared with different shapes"
			}
			pending[record.Name] = record
		}
	}
	var added []string
	for n, record := range pending {
		if _, ok := t.all[n]; !ok {
			t.all[n] = record
			added = append(added, n)
		}
	}
	held := map[string]bool{}
	for n := range t.objects {
		held[n] = true
	}
	if _, err := t.object((*shape).Root); err != nil {
		for _, n := range added {
			delete(t.all, n)
		}
		for n := range t.objects {
			if !held[n] {
				delete(t.objects, n)
			}
		}
		return err.Error()
	}
	return ""
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
	if t.reserved[name] {
		return nil, namespaceInvalid("view record name is used by the engine: " + name)
	}
	if len(record.Fields) == 0 {
		return nil, namespaceInvalid("view record declares no fields: " + name)
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
	for _, f := range record.Fields {
		if !namespaceViewName.MatchString(f.Name) {
			delete(t.objects, name)
			return nil, namespaceInvalid("view field is not a GraphQL name: " + f.Name)
		}
		if _, err := t.output(f.Type); err != nil {
			delete(t.objects, name)
			return nil, err
		}
	}
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
