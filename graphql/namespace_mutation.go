package graphql

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"time"

	"github.com/Project-Helianthus/helianthus-semreg/semreg/runtime/tree"
	"github.com/Project-Helianthus/helianthus-semreg/semreg/v1/driver"
	gql "github.com/graphql-go/graphql"
)

type namespaceCalls struct {
	runtime      *tree.Tree
	cut          *tree.TreeCut
	key          *gql.InputObject
	result       *gql.Object
	fields       gql.Fields
	semanticName string
}

func newNamespaceCalls(runtime *tree.Tree, cut *tree.TreeCut, key *gql.InputObject, semantic *gql.Field) (namespaceCalls, error) {
	name, err := tree.TreeName(tree.SurfaceGraphQL, tree.VerbCall)
	if err != nil {
		return namespaceCalls{}, err
	}
	text := gql.String
	result := namespaceObject("tree_method_result", map[string]gql.Output{
		"native": text, "error": text, "status": text, "kernel_outcome": text, "delivery": text,
		"acknowledgement": text, "readback_relation": text, "reason": text,
		"compound_execution": semantic.Type.(*gql.Object).Fields()["compound_execution"].Type,
	})
	calls := namespaceCalls{runtime: runtime, cut: cut, key: key, result: result, fields: gql.Fields{}, semanticName: name}
	calls.fields[name] = &gql.Field{
		Type: result,
		Args: gql.FieldConfigArgument{
			"method":  &gql.ArgumentConfig{Type: gql.NewNonNull(text)},
			"request": &gql.ArgumentConfig{Type: semantic.Args["input"].Type},
		},
		Resolve: calls.callSemantic,
	}
	return calls, nil
}

func (c namespaceCalls) addNative(d tree.Descriptor) error {
	name, err := tree.DriverNames(d.Namespace, tree.VerbCall, string(d.Ref))
	if err != nil {
		return err
	}
	if name.GraphQL == c.semanticName {
		// Both plan rules give this member the tree_call name. Keep that
		// one field, with the same disjoint call forms as the MCP tool.
		field := c.fields[name.GraphQL]
		if field.Args["key"] == nil {
			field.Args["key"] = &gql.ArgumentConfig{Type: c.key}
			field.Args["input"] = &gql.ArgumentConfig{Type: gql.String}
			field.Args["method"] = &gql.ArgumentConfig{Type: gql.String}
			field.Args["request"] = &gql.ArgumentConfig{Type: field.Args["request"].Type.(*gql.NonNull).OfType.(gql.Input)}
			field.Resolve = func(p gql.ResolveParams) (any, error) {
				switch {
				case p.Args["key"] != nil && p.Args["method"] == nil && p.Args["request"] == nil:
					return c.callNative(p, d.Namespace)
				case p.Args["key"] == nil && p.Args["input"] == nil && p.Args["method"] != nil && p.Args["request"] != nil:
					return c.callSemantic(p)
				default:
					return nil, namespaceInvalid("supply a native key or a semantic method with its request")
				}
			}
		}
	} else if c.fields[name.GraphQL] == nil {
		c.fields[name.GraphQL] = &gql.Field{
			Type: c.result,
			Args: gql.FieldConfigArgument{
				"key":   &gql.ArgumentConfig{Type: gql.NewNonNull(c.key)},
				"input": &gql.ArgumentConfig{Type: gql.String},
			},
			Resolve: func(p gql.ResolveParams) (any, error) { return c.callNative(p, d.Namespace) },
		}
	}
	return nil
}

func (c namespaceCalls) callNative(p gql.ResolveParams, namespace driver.NamespaceID) (any, error) {
	ref, err := namespaceMemberRef(p.Args["key"], namespace, tree.KeywordMethods)
	if err != nil {
		return nil, err
	}
	var input tree.Input
	if encoded, present := p.Args["input"].(string); present {
		input.Encoded, err = base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return nil, namespaceInvalid("native input is not base64")
		}
	}
	return c.call(p.Context, ref, input)
}

func (c namespaceCalls) callSemantic(p gql.ResolveParams) (any, error) {
	ref := tree.MethodRef(p.Args["method"].(string))
	key, err := tree.ParseKey(string(ref))
	if err != nil {
		return nil, namespaceErrorFrom(err)
	}
	if key.Facet != tree.KeywordMethods || key.Domain != tree.KeywordSem {
		return nil, namespaceInvalid("method must name a semantic operation")
	}
	definition, err := tree.ParseDefinitionSegment(key.Segment)
	if err != nil {
		return nil, namespaceErrorFrom(err)
	}
	args := p.Args["request"].(map[string]any)
	// GraphQL's semantic input uses milliseconds; the operation port takes a
	// duration. The port owns request admission and all operation semantics.
	body := make(map[string]any, len(args)-1)
	for name, v := range args {
		if name != "timeout_ms" {
			body[name] = v
		}
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, namespaceInvalid("invalid operation request")
	}
	var request driver.OperationRequest
	if err := json.Unmarshal(encoded, &request); err != nil {
		return nil, namespaceInvalid("invalid operation request")
	}
	request.Timeout = time.Duration(args["timeout_ms"].(int)) * time.Millisecond
	if definition != request.Operation {
		return nil, namespaceInvalid("request operation does not match the method")
	}
	if _, err := c.cut.Describe(p.Context, ref); err != nil {
		return nil, namespaceErrorFrom(err)
	}
	return c.call(p.Context, ref, tree.Input{Request: &request})
}

func (c namespaceCalls) call(ctx context.Context, ref tree.MethodRef, input tree.Input) (any, error) {
	prepared, err := c.runtime.Prepare(ctx, c.cut.ID(), ref, input)
	if err != nil {
		return nil, namespaceErrorFrom(err)
	}
	receipt, err := c.runtime.Call(ctx, prepared, tree.CallOptions{})
	if err != nil {
		return nil, namespaceErrorFrom(err)
	}
	if receipt.Result != nil {
		return namespaceOperationValue(receipt.Result), nil
	}
	outcome, err := c.runtime.Outcome(ctx, receipt.ID)
	if err != nil {
		return nil, namespaceErrorFrom(err)
	}
	if outcome.Operation != nil {
		return namespaceOperationValue(outcome.Operation), nil
	}
	return map[string]any{"native": namespaceBytes(outcome.Native), "error": compoundEvidenceValue(outcome.Error)}, nil
}

func namespaceOperationValue(result *driver.OperationResult) map[string]any {
	return map[string]any{
		"status": string(result.Status), "kernel_outcome": compoundEvidenceValue(result.KernelOutcome),
		"delivery": compoundEvidenceValue(result.Delivery), "acknowledgement": compoundEvidenceValue(result.Acknowledgement),
		"readback_relation": compoundEvidenceValue(result.ReadbackRelation), "reason": compoundEvidenceValue(result.Reason),
		"compound_execution": semanticCompoundExecutionValue(result.CompoundExecution),
	}
}
