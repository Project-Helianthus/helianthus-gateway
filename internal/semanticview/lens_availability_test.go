package semanticview

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	semreg "github.com/Project-Helianthus/helianthus-semreg/semreg/v1"
	"github.com/Project-Helianthus/helianthus-semreg/semreg/v1/graph"
	"github.com/Project-Helianthus/helianthus-semreg/semreg/v1/projection"
)

// TestLensElementAvailability reads availability from a lens result (TGT-6).
// Each case changes one field of the identity the read claims to use.
//
// Capability identity is asset id plus definition id. The test catches:
//   - asset id ignored: another asset's unavailable capability withholds this one
//   - definition id ignored: a different definition withholds this capability
//   - availability ignored: the snapshot's available instance stays exact
//   - ungraphed asset id ignored: another asset's ungraphed row drops this capability
//   - ungraphed reason ignored: source_unavailable drops a capability the lens lists
//
// Fact evaluation identity is asset id, fact key (pack id, pack version, fact
// id, dimension id, dimension value), candidate id and candidate revision.
// The test catches each of those ignored: a mismatched row's unavailable
// evaluation withholds the snapshot's available candidate. Effective
// availability is the value read: available and degraded stay exact,
// unavailable and withdrawn withhold.
func TestLensElementAvailability(t *testing.T) {
	in := m08Input(t)
	capability := in.Snapshot.Capabilities[0]
	omitted := in.Snapshot.Capabilities[1]
	fact := in.Snapshot.Facts[0]
	candidate := fact.Candidates[0]

	without := m08Build(t, in)
	if got := itemOutcome(t, without, projection.ItemCapability, capability.Definition.ID); got != projection.ProjectionExact {
		t.Fatalf("capture without a lens: outcome %s", got)
	}

	same := capability
	withSame := m08Build(t, withLens(t, in, lensWith(t, in, []semreg.CapabilityInstance{same}, nil, nil)))
	if got := itemOutcome(t, withSame, projection.ItemCapability, capability.Definition.ID); got != projection.ProjectionExact {
		t.Fatalf("lens carrying the same capability: outcome %s", got)
	}
	if got := itemOutcome(t, withSame, projection.ItemCapability, omitted.Definition.ID); got != projection.ProjectionUnknown {
		t.Fatalf("capability the lens omits: outcome %s", got)
	}

	unavailable := capability
	unavailable.Availability = semreg.AvailabilityUnavailable
	withheld := m08Build(t, withLens(t, in, lensWith(t, in, []semreg.CapabilityInstance{unavailable}, nil, nil)))
	if got := itemOutcome(t, withheld, projection.ItemCapability, capability.Definition.ID); got != projection.ProjectionWithheld {
		t.Fatalf("lens availability: outcome %s", got)
	}

	otherAsset := unavailable
	otherAsset.AssetID = "asset:other"
	if got := itemOutcome(t, m08Build(t, withLens(t, in, lensWith(t, in, []semreg.CapabilityInstance{otherAsset}, nil, nil))), projection.ItemCapability, capability.Definition.ID); got != projection.ProjectionUnknown {
		t.Fatalf("other asset capability: outcome %s", got)
	}
	otherDefinition := unavailable
	otherDefinition.Definition.ID = "synthetic.capability.other"
	if got := itemOutcome(t, m08Build(t, withLens(t, in, lensWith(t, in, []semreg.CapabilityInstance{otherDefinition}, nil, nil))), projection.ItemCapability, capability.Definition.ID); got != projection.ProjectionUnknown {
		t.Fatalf("other definition capability: outcome %s", got)
	}
	otherPack := unavailable
	otherPack.Definition.Pack.ID = "pack.synthetic.other"
	if got := itemOutcome(t, m08Build(t, withLens(t, in, lensWith(t, in, []semreg.CapabilityInstance{otherPack}, nil, nil))), projection.ItemCapability, capability.Definition.ID); got != projection.ProjectionWithheld {
		t.Fatalf("pack is not capability identity: outcome %s", got)
	}

	ungraphed := graph.NotIncludedAsset{AssetID: in.AssetID, Reason: graph.NotIncludedUngraphed}
	if got := itemOutcome(t, m08Build(t, withLens(t, in, lensWith(t, in, []semreg.CapabilityInstance{unavailable}, nil, []graph.NotIncludedAsset{ungraphed}))), projection.ItemCapability, capability.Definition.ID); got != projection.ProjectionUnknown {
		t.Fatalf("ungraphed asset capability: outcome %s", got)
	}
	otherUngraphed := graph.NotIncludedAsset{AssetID: "asset:other", Reason: graph.NotIncludedUngraphed}
	if got := itemOutcome(t, m08Build(t, withLens(t, in, lensWith(t, in, []semreg.CapabilityInstance{unavailable}, nil, []graph.NotIncludedAsset{otherUngraphed}))), projection.ItemCapability, capability.Definition.ID); got != projection.ProjectionWithheld {
		t.Fatalf("another asset ungraphed: outcome %s", got)
	}
	absent := graph.NotIncludedAsset{AssetID: in.AssetID, Reason: graph.NotIncludedSourceUnavailable}
	if got := itemOutcome(t, m08Build(t, withLens(t, in, lensWith(t, in, []semreg.CapabilityInstance{unavailable}, nil, []graph.NotIncludedAsset{absent}))), projection.ItemCapability, capability.Definition.ID); got != projection.ProjectionWithheld {
		t.Fatalf("source_unavailable is not ungraphed: outcome %s", got)
	}

	evaluation := semreg.EvaluatedFact{CandidateID: candidate.CandidateID, CandidateRevision: candidate.Revision, EffectiveAvailability: semreg.AvailabilityUnavailable}
	lensFact := graph.LensFact{AssetID: in.AssetID, Key: fact.Key, Evaluation: []semreg.EvaluatedFact{evaluation}}
	if got := itemOutcome(t, m08Build(t, withLens(t, in, lensWith(t, in, nil, []graph.LensFact{lensFact}, nil))), projection.ItemFact, fact.Key.FactID); got != projection.ProjectionWithheld {
		t.Fatalf("lens fact availability: outcome %s", got)
	}
	for _, tc := range []struct {
		name string
		edit func(*graph.LensFact)
	}{
		{"asset_id", func(f *graph.LensFact) { f.AssetID = "asset:other" }},
		{"pack_id", func(f *graph.LensFact) { f.Key.PackID = "pack.synthetic.other" }},
		{"pack_version", func(f *graph.LensFact) { f.Key.PackVersion = "9.0.0" }},
		{"fact_id", func(f *graph.LensFact) { f.Key.FactID = "fact.synthetic.other" }},
		{"dimension_id", func(f *graph.LensFact) {
			f.Key = cloneKey(t, f.Key)
			f.Key.Dimensions[0].ID = "dimension.synthetic.other"
		}},
		{"dimension_value", func(f *graph.LensFact) {
			f.Key = cloneKey(t, f.Key)
			text := "another"
			f.Key.Dimensions[0].Value.Text = &text
		}},
		{"candidate_id", func(f *graph.LensFact) { f.Evaluation[0].CandidateID = "candidate:other" }},
		{"candidate_revision", func(f *graph.LensFact) { f.Evaluation[0].CandidateRevision = "9" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := lensFact
			row.Evaluation = append([]semreg.EvaluatedFact{}, lensFact.Evaluation...)
			tc.edit(&row)
			got := itemOutcome(t, m08Build(t, withLens(t, in, lensWith(t, in, nil, []graph.LensFact{row}, nil))), projection.ItemFact, fact.Key.FactID)
			if got != projection.ProjectionExact {
				t.Fatalf("mismatched lens fact withheld the snapshot candidate: %s", got)
			}
		})
	}
	for _, availability := range []semreg.Availability{semreg.AvailabilityAvailable, semreg.AvailabilityDegraded} {
		row := lensFact
		row.Evaluation = []semreg.EvaluatedFact{{CandidateID: candidate.CandidateID, CandidateRevision: candidate.Revision, EffectiveAvailability: availability}}
		if got := itemOutcome(t, m08Build(t, withLens(t, in, lensWith(t, in, nil, []graph.LensFact{row}, nil))), projection.ItemFact, fact.Key.FactID); got != projection.ProjectionExact {
			t.Fatalf("availability %s outcome %s", availability, got)
		}
	}
	for _, availability := range []semreg.Availability{semreg.AvailabilityUnavailable, semreg.AvailabilityWithdrawn} {
		row := lensFact
		row.Evaluation = []semreg.EvaluatedFact{{CandidateID: candidate.CandidateID, CandidateRevision: candidate.Revision, EffectiveAvailability: availability}}
		if got := itemOutcome(t, m08Build(t, withLens(t, in, lensWith(t, in, nil, []graph.LensFact{row}, nil))), projection.ItemFact, fact.Key.FactID); got != projection.ProjectionWithheld {
			t.Fatalf("availability %s outcome %s", availability, got)
		}
	}
	if got := itemOutcome(t, m08Build(t, withLens(t, in, lensWith(t, in, nil, []graph.LensFact{lensFact}, []graph.NotIncludedAsset{ungraphed}))), projection.ItemFact, fact.Key.FactID); got != projection.ProjectionExact {
		t.Fatalf("ungraphed asset still read the lens fact: %s", got)
	}
}

func TestLensDetachedAndRejected(t *testing.T) {
	in := m08Input(t)
	capability := in.Snapshot.Capabilities[0]
	capability.Availability = semreg.AvailabilityUnavailable
	lens := lensWith(t, in, []semreg.CapabilityInstance{capability}, nil, nil)
	in.Lens = &lens
	first := m08Build(t, in)
	second := m08Build(t, in)
	in.Lens.Capabilities[0].Availability = semreg.AvailabilityAvailable
	if first.Assets[0].Lens.Capabilities[0].Availability != semreg.AvailabilityUnavailable || itemOutcome(t, first, projection.ItemCapability, capability.Definition.ID) != projection.ProjectionWithheld {
		t.Fatal("caller lens aliases the read")
	}
	first.Assets[0].Lens.Capabilities[0].Availability = semreg.AvailabilityAvailable
	if second.Assets[0].Lens.Capabilities[0].Availability != semreg.AvailabilityUnavailable {
		t.Fatal("lens results alias")
	}
	in.Lens = &graph.LensResult{}
	if out, err := Build(m08Registry(t), []Input{in}); err == nil || len(out.Assets) != 0 {
		t.Fatal("invalid lens admitted")
	}
}

func itemOutcome(t *testing.T, v View, kind projection.ItemKind, id semreg.DefinitionID) projection.ProjectionOutcome {
	t.Helper()
	for _, d := range v.Assets[0].Projection.Dispositions {
		if d.Kind == kind && d.ItemID == id {
			return d.Outcome
		}
	}
	t.Fatalf("no disposition %s %s", kind, id)
	return ""
}

func withLens(t *testing.T, in Input, lens graph.LensResult) Input {
	t.Helper()
	in.Lens = &lens
	return in
}

func cloneKey(t *testing.T, key semreg.FactKey) semreg.FactKey {
	t.Helper()
	out, err := clone(key)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func lensWith(t *testing.T, in Input, capabilities []semreg.CapabilityInstance, facts []graph.LensFact, absent []graph.NotIncludedAsset) graph.LensResult {
	t.Helper()
	catalog, err := graph.Embedded()
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("semanticview lens cut"))
	completeness := graph.CutComplete
	if len(absent) != 0 {
		completeness = graph.CutPartial
	}
	if capabilities == nil {
		capabilities = []semreg.CapabilityInstance{}
	}
	return graph.LensResult{
		Contract: graph.LensResultContract, Lens: "lens.equipment_composition", Catalog: catalog.Digest(),
		Cut: graph.CutRef{
			CutID: semreg.Digest("sha256:" + hex.EncodeToString(sum[:])), Completeness: completeness,
			Members:     []graph.CutMember{{AssetID: in.AssetID, SnapshotID: in.Snapshot.SnapshotID}},
			NotIncluded: absent,
		},
		Entry:        graph.EntryPoint{Kind: graph.EntryLensRoots, Lens: "lens.equipment_composition"},
		Resources:    []graph.LensResource{},
		Facts:        facts,
		Capabilities: capabilities,
	}
}
