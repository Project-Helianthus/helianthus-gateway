package graphql

import (
	"github.com/Project-Helianthus/helianthus-semreg/semreg/v1/driver"
	graphqlgo "github.com/graphql-go/graphql"
)

func semanticCompoundExecutionType() *graphqlgo.Object {
	step := graphqlgo.NewObject(graphqlgo.ObjectConfig{
		Name: "semantic_compound_step",
		Fields: graphqlgo.Fields{
			"name":              &graphqlgo.Field{Type: graphqlgo.NewNonNull(graphqlgo.String)},
			"attempted":         &graphqlgo.Field{Type: graphqlgo.NewNonNull(graphqlgo.Boolean)},
			"delivery":          &graphqlgo.Field{Type: graphqlgo.String},
			"acknowledgement":   &graphqlgo.Field{Type: graphqlgo.String},
			"readback_relation": &graphqlgo.Field{Type: graphqlgo.String},
			"reason":            &graphqlgo.Field{Type: graphqlgo.String},
		},
	})
	return graphqlgo.NewObject(graphqlgo.ObjectConfig{
		Name: "semantic_compound_execution",
		Fields: graphqlgo.Fields{
			"partial": &graphqlgo.Field{Type: graphqlgo.NewNonNull(graphqlgo.Boolean)},
			"steps":   &graphqlgo.Field{Type: graphqlgo.NewNonNull(graphqlgo.NewList(graphqlgo.NewNonNull(step)))},
		},
	})
}

func semanticCompoundExecutionValue(compound *driver.CompoundExecution) any {
	if compound == nil {
		return nil
	}
	steps := make([]map[string]any, len(compound.Steps))
	for i, step := range compound.Steps {
		steps[i] = map[string]any{
			"name":              step.Point,
			"attempted":         step.Attempted,
			"delivery":          compoundEvidenceValue(step.Delivery),
			"acknowledgement":   compoundEvidenceValue(step.Acknowledgement),
			"readback_relation": compoundEvidenceValue(step.ReadbackRelation),
			"reason":            compoundEvidenceValue(step.Reason),
		}
	}
	return map[string]any{"partial": compound.Partial, "steps": steps}
}

func compoundEvidenceValue[T ~string](value T) any {
	if value == "" {
		return nil
	}
	return string(value)
}
