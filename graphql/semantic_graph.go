package graphql

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/Project-Helianthus/helianthus-semreg/semreg/runtime/tree"
	semreg "github.com/Project-Helianthus/helianthus-semreg/semreg/v1"
	"github.com/Project-Helianthus/helianthus-semreg/semreg/v1/graph"
	graphqlgo "github.com/graphql-go/graphql"
)

type semanticGraphContextKey struct{}

// WithSemanticGraph supplies the served SemReg tree to the local GraphQL reads.
func WithSemanticGraph(ctx context.Context, runtime *tree.Tree) context.Context {
	return context.WithValue(ctx, semanticGraphContextKey{}, runtime)
}

type semanticGraphError struct {
	code    string
	message string
}

func (e semanticGraphError) Error() string { return e.message }

func (e semanticGraphError) Extensions() map[string]any { return map[string]any{"code": e.code} }

// CUT-4: the page and the tree's omission vector travel together.
type semanticGraphResourceList struct {
	graph.ResourcePage
	NotIncluded []graph.NotIncludedAsset `json:"not_included,omitempty"`
}

type semanticGraphTraversal struct {
	graph.TraversalPage
	NotIncluded []graph.NotIncludedAsset `json:"not_included,omitempty"`
}

type semanticGraphCatalog struct {
	Digest  semreg.Digest   `json:"digest"`
	Status  graph.Status    `json:"status"`
	Planes  []graph.Plane   `json:"planes"`
	Lenses  []graph.Lens    `json:"lenses"`
	Entries []graph.Element `json:"entries"`
}

func (c semanticGraphCatalog) Validate() error { return c.Digest.Validate() }

func addSemanticGraphQueries(fields graphqlgo.Fields) {
	text := graphqlgo.String
	requiredText := graphqlgo.NewNonNull(text)
	entry := graphqlgo.NewInputObject(graphqlgo.InputObjectConfig{
		Name: "semantic_graph_entry_input",
		Fields: graphqlgo.InputObjectConfigFieldMap{
			"kind":     {Type: requiredText},
			"resource": {Type: text},
			"lens":     {Type: text},
		},
	})
	pack := graphqlgo.NewInputObject(graphqlgo.InputObjectConfig{
		Name: "semantic_graph_pack_ref_input",
		Fields: graphqlgo.InputObjectConfigFieldMap{
			"id":      {Type: requiredText},
			"version": {Type: requiredText},
		},
	})
	definition := graphqlgo.NewInputObject(graphqlgo.InputObjectConfig{
		Name: "semantic_graph_definition_ref_input",
		Fields: graphqlgo.InputObjectConfigFieldMap{
			"id":      {Type: requiredText},
			"version": {Type: requiredText},
			"pack":    {Type: graphqlgo.NewNonNull(pack)},
		},
	})
	pageArgs := func() graphqlgo.FieldConfigArgument {
		return graphqlgo.FieldConfigArgument{
			"cut_id":     {Type: text},
			"page_size":  {Type: graphqlgo.NewNonNull(graphqlgo.Int)},
			"page_token": {Type: text},
		}
	}
	walkArgs := func() graphqlgo.FieldConfigArgument {
		args := pageArgs()
		args["entry"] = &graphqlgo.ArgumentConfig{Type: graphqlgo.NewNonNull(entry)}
		args["direction"] = &graphqlgo.ArgumentConfig{Type: requiredText}
		args["depth"] = &graphqlgo.ArgumentConfig{Type: graphqlgo.NewNonNull(graphqlgo.Int)}
		return args
	}

	list := pageArgs()
	list["kind"] = &graphqlgo.ArgumentConfig{Type: text}
	list["plane"] = &graphqlgo.ArgumentConfig{Type: text}
	fields["semantic_resources"] = semanticGraphField("List current resources (ENT-2).", list,
		func(ctx context.Context, runtime *tree.Tree, request graph.ListRequest) (semreg.Record, error) {
			result, err := runtime.ListResources(ctx, request)
			return semanticGraphResourceList{result.Page, result.NotIncluded}, err
		})

	traverse := walkArgs()
	traverse["lens"] = &graphqlgo.ArgumentConfig{Type: text}
	traverse["planes"] = &graphqlgo.ArgumentConfig{Type: graphqlgo.NewList(requiredText)}
	traverse["definitions"] = &graphqlgo.ArgumentConfig{Type: graphqlgo.NewList(graphqlgo.NewNonNull(definition))}
	fields["semantic_traversal"] = semanticGraphField("Traverse coherent relations (TRV-1).", traverse,
		func(ctx context.Context, runtime *tree.Tree, request graph.TraversalRequest) (semreg.Record, error) {
			result, err := runtime.Traverse(ctx, request)
			return semanticGraphTraversal{result.Page, result.NotIncluded}, err
		})

	lens := walkArgs()
	lens["lens"] = &graphqlgo.ArgumentConfig{Type: requiredText}
	fields["semantic_lens"] = semanticGraphField("Read a lens result (API-2).", lens,
		func(ctx context.Context, runtime *tree.Tree, request graph.LensRequest) (semreg.Record, error) {
			return runtime.RetrieveLens(ctx, request)
		})

	fields["semantic_element_catalog"] = semanticGraphField("Read the element catalog (API-3).", nil,
		func(ctx context.Context, runtime *tree.Tree, _ struct{}) (semreg.Record, error) {
			result, err := runtime.Catalog(ctx)
			return semanticGraphCatalog(result), err
		})
}

func semanticGraphField[R any](description string, args graphqlgo.FieldConfigArgument, read func(context.Context, *tree.Tree, R) (semreg.Record, error)) *graphqlgo.Field {
	return &graphqlgo.Field{
		// API-1/API-2: preserve the entire canonical JSON record, including
		// omission and order, without turning its keys into GraphQL fields.
		Type:        graphqlgo.String,
		Description: description + " Returns canonical SemReg JSON.",
		Args:        args,
		Resolve: func(p graphqlgo.ResolveParams) (any, error) {
			var runtime *tree.Tree
			if p.Context != nil {
				runtime, _ = p.Context.Value(semanticGraphContextKey{}).(*tree.Tree)
			}
			if runtime == nil {
				return nil, semanticGraphError{"UNAVAILABLE", "semantic graph read unavailable"}
			}
			// The schema limits the arguments to the graph request's wire members.
			var request R
			raw, err := json.Marshal(p.Args)
			if err != nil || json.Unmarshal(raw, &request) != nil {
				return nil, semanticGraphError{string(semreg.InvalidValue), "invalid semantic graph arguments"}
			}
			result, err := read(p.Context, runtime, request)
			if err == nil {
				var canonical []byte
				canonical, err = semreg.CanonicalJSON(result)
				if err == nil {
					return string(canonical), nil
				}
			}
			var graphErr *semreg.Error
			if errors.As(err, &graphErr) {
				return nil, semanticGraphError{string(graphErr.ID), graphErr.Error()}
			}
			return nil, semanticGraphError{"UNAVAILABLE", "semantic graph read unavailable"}
		},
	}
}
