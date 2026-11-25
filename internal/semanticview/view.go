// Package semanticview builds detached, transport-neutral SemReg read values.
package semanticview

import (
	"bytes"
	"encoding/json"
	"sort"

	semreg "github.com/Project-Helianthus/helianthus-semreg/semreg/v1"
	"github.com/Project-Helianthus/helianthus-semreg/semreg/v1/graph"
	"github.com/Project-Helianthus/helianthus-semreg/semreg/v1/packs/evse"
	"github.com/Project-Helianthus/helianthus-semreg/semreg/v1/packs/infrastructure"
	"github.com/Project-Helianthus/helianthus-semreg/semreg/v1/packs/pv"
	"github.com/Project-Helianthus/helianthus-semreg/semreg/v1/packs/storage"
	"github.com/Project-Helianthus/helianthus-semreg/semreg/v1/packs/thermal"
	"github.com/Project-Helianthus/helianthus-semreg/semreg/v1/projection"
)

const reasonPrefix = "semantic.read.reason."

// PendingNormativeSourceReason is the source reason that keeps a fact unknown
// while the normative source of its mapping is unresolved. The view preserves
// it verbatim instead of folding it into the ordinary reason ordering.
const PendingNormativeSourceReason = "unknown_pending_normative_source"

// Input is captured coherently by the caller, including any publication safety
// withholding. Build performs no acquisition or publication and reads no clock.
type Input struct {
	AssetID           semreg.AssetID
	Snapshot          *semreg.Snapshot
	Evaluation        *semreg.EvaluationView
	Selections        []semreg.Selection
	UnavailableReason string
	// Lens is the lens result to read element availability from (TGT-6).
	// Nil is the per-asset capture, which has no lens result.
	Lens *graph.LensResult
}

type View struct {
	Assets []Asset `json:"assets"`
}
type Asset struct {
	AssetID           semreg.AssetID               `json:"asset_id"`
	Snapshot          *semreg.Snapshot             `json:"snapshot,omitempty"`
	Evaluation        *semreg.EvaluationView       `json:"evaluation,omitempty"`
	Projection        *projection.ProjectionReport `json:"projection,omitempty"`
	Selections        []semreg.Selection           `json:"selections"`
	UnavailableReason string                       `json:"unavailable_reason,omitempty"`
	// Lens is a detached copy of the lens result this asset was read with.
	// It is not part of the served snapshot document. Element availability
	// is read from it (TGT-6).
	Lens *graph.LensResult `json:"-"`
}

func failure(id semreg.ErrorID) error { return &semreg.Error{ID: id, Detail: "semantic snapshot read"} }

// Build consumes complete public-safe captures. Native safety and lifecycle
// gates must be applied before capture; unsafe assets use publication_withheld.
// Assets share one captured wall instant; each keeps its own aging coordinate.
// A nil provider is an error, never an empty successful publication.
func Build(metadata *semreg.PackMetadataRegistry, inputs []Input) (View, error) {
	if metadata == nil {
		return View{}, failure(semreg.DefinitionOwnerMissing)
	}
	if len(inputs) < 1 || len(inputs) > 32 {
		return View{}, failure(semreg.BoundsExceeded)
	}
	result := View{Assets: make([]Asset, 0, len(inputs))}
	seen := map[semreg.AssetID]bool{}
	var evaluatedAt *semreg.TimePoint
	for _, input := range inputs {
		if input.AssetID.Validate() != nil || len(input.AssetID) > 256 || seen[input.AssetID] {
			return View{}, failure(semreg.InvalidValue)
		}
		seen[input.AssetID] = true
		asset := Asset{AssetID: input.AssetID, Selections: []semreg.Selection{}}
		if input.UnavailableReason != "" {
			switch input.UnavailableReason {
			case reasonPrefix + "asset_unknown", reasonPrefix + "snapshot_unavailable", reasonPrefix + "source_unavailable", reasonPrefix + "publication_withheld":
			default:
				return View{}, failure(semreg.InvalidValue)
			}
			if input.Snapshot != nil || input.Evaluation != nil || len(input.Selections) != 0 {
				return View{}, failure(semreg.InvalidValue)
			}
			asset.UnavailableReason = input.UnavailableReason
			result.Assets = append(result.Assets, asset)
			continue
		}
		if input.Snapshot == nil || input.Evaluation == nil {
			return View{}, failure(semreg.MissingMember)
		}
		snapshot, err := clone(*input.Snapshot)
		if err != nil {
			return View{}, failure(semreg.InvalidValue)
		}
		evaluation, err := clone(*input.Evaluation)
		if err != nil {
			return View{}, failure(semreg.InvalidValue)
		}
		if snapshot.AssetID != input.AssetID {
			return View{}, failure(semreg.RevisionConflict)
		}
		if evaluatedAt != nil && *evaluatedAt != evaluation.Context.EvaluatedAt {
			return View{}, failure(semreg.RevisionConflict)
		}
		evaluatedAt = &evaluation.Context.EvaluatedAt
		if err := evaluationMatches(snapshot, evaluation); err != nil {
			return View{}, failure(semreg.ErrorIdentifier(err))
		}
		var lens *graph.LensResult
		if input.Lens != nil {
			detached, err := cloneLens(*input.Lens)
			if err != nil {
				return View{}, failure(semreg.ErrorIdentifier(err))
			}
			lens = &detached
		}
		report, err := project(metadata, snapshot, evaluation, lens)
		if err != nil {
			return View{}, failure(semreg.ErrorIdentifier(err))
		}
		selections := make([]semreg.Selection, 0, len(input.Selections))
		for _, selection := range input.Selections {
			if err := semreg.ValidateSelection(snapshot, evaluation, selection); err != nil {
				return View{}, failure(semreg.ErrorIdentifier(err))
			}
			detached, err := clone(selection)
			if err != nil {
				return View{}, failure(semreg.InvalidValue)
			}
			selections = append(selections, detached)
		}
		sort.Slice(selections, func(i, j int) bool { return selectionKey(selections[i]) < selectionKey(selections[j]) })
		for i := 1; i < len(selections); i++ {
			if selectionKey(selections[i-1]) == selectionKey(selections[i]) {
				return View{}, failure(semreg.DuplicateKey)
			}
		}
		asset.Snapshot = &snapshot
		asset.Evaluation = &evaluation
		asset.Projection = &report
		asset.Lens = lens
		asset.Selections = selections
		result.Assets = append(result.Assets, asset)
	}
	sort.Slice(result.Assets, func(i, j int) bool { return result.Assets[i].AssetID < result.Assets[j].AssetID })
	raw, err := json.Marshal(result)
	if err != nil {
		return View{}, failure(semreg.InvalidValue)
	}
	if len(raw) > 4*1024*1024 {
		return View{}, failure(semreg.BoundsExceeded)
	}
	return result, nil
}

func clone[T semreg.Record](value T) (T, error) {
	var out T
	raw, err := semreg.CanonicalJSON(value)
	if err != nil {
		return out, err
	}
	return semreg.Decode[T](raw)
}
func selectionKey(s semreg.Selection) string {
	key, _ := semreg.CanonicalJSON(s.Key)
	return string(key) + "\x00" + string(s.PolicyID) + "\x00" + string(s.PolicyVersion)
}

// evaluationMatches requires the supplied view to be the kernel evaluation of
// this snapshot. A mismatched view is not served.
func evaluationMatches(snapshot semreg.Snapshot, supplied semreg.EvaluationView) error {
	if snapshot.SnapshotID != supplied.SnapshotID || snapshot.Revisions != supplied.Revisions {
		return &semreg.Error{ID: semreg.RevisionConflict, Detail: "semantic snapshot read"}
	}
	expected, err := semreg.EvaluateSnapshot(snapshot, supplied.Context)
	if err != nil {
		return err
	}
	facts := map[semreg.CandidateID]semreg.EvaluatedFact{}
	for _, fact := range expected.Facts {
		facts[fact.CandidateID] = fact
	}
	if len(expected.Facts) != len(supplied.Facts) {
		return &semreg.Error{ID: semreg.DanglingReference, Detail: "semantic snapshot read"}
	}
	for _, fact := range supplied.Facts {
		current, ok := facts[fact.CandidateID]
		if !ok {
			return &semreg.Error{ID: semreg.DanglingReference, Detail: "semantic snapshot read"}
		}
		if current.CandidateRevision != fact.CandidateRevision {
			return &semreg.Error{ID: semreg.RevisionConflict, Detail: "semantic snapshot read"}
		}
	}
	a, err := semreg.CanonicalJSON(expected)
	if err != nil {
		return err
	}
	b, err := semreg.CanonicalJSON(supplied)
	if err != nil {
		return err
	}
	if !bytes.Equal(a, b) {
		return &semreg.Error{ID: semreg.InvalidValue, Detail: "semantic snapshot read"}
	}
	return nil
}

func cloneLens(in graph.LensResult) (graph.LensResult, error) {
	if err := in.Validate(); err != nil {
		return graph.LensResult{}, err
	}
	raw, err := json.Marshal(in)
	if err != nil {
		return graph.LensResult{}, &semreg.Error{ID: semreg.InvalidValue, Detail: "semantic snapshot read"}
	}
	var out graph.LensResult
	if err := json.Unmarshal(raw, &out); err != nil {
		return graph.LensResult{}, &semreg.Error{ID: semreg.InvalidValue, Detail: "semantic snapshot read"}
	}
	if err := out.Validate(); err != nil {
		return graph.LensResult{}, err
	}
	return out, nil
}

// assetUngraphed reports an asset the lens cut lists as ungraphed (ACT-5).
// That asset contributes no element, so its lens facts and capabilities are
// not availability.
func assetUngraphed(lens *graph.LensResult, asset semreg.AssetID) bool {
	if lens == nil {
		return false
	}
	for _, row := range lens.Cut.NotIncluded {
		if row.AssetID == asset && row.Reason == graph.NotIncludedUngraphed {
			return true
		}
	}
	return false
}

// candidateEvaluation reads the candidate's availability. A lens fact carries
// the kernel evaluation (LRS-1); when this lens has a row for the same asset,
// fact key, candidate and revision, that row is the availability. A row the
// lens does not carry, and an ungraphed asset (ACT-5), keep the snapshot
// evaluation. The snapshot projection's fact outcome did not come from the
// retired pack view.
func candidateEvaluation(e semreg.EvaluationView, lens *graph.LensResult, asset semreg.AssetID, c semreg.FactCandidate) (semreg.EvaluatedFact, bool) {
	if lens != nil && !assetUngraphed(lens, asset) {
		for _, fact := range lens.Facts {
			if fact.AssetID != asset || !factKeyEqual(fact.Key, c.Key) {
				continue
			}
			for _, evaluated := range fact.Evaluation {
				if evaluated.CandidateID == c.CandidateID && evaluated.CandidateRevision == c.Revision {
					return evaluated, true
				}
			}
		}
	}
	for _, evaluated := range e.Facts {
		if evaluated.CandidateID == c.CandidateID && evaluated.CandidateRevision == c.Revision {
			return evaluated, true
		}
	}
	return semreg.EvaluatedFact{}, false
}

func factKeyEqual(a, b semreg.FactKey) bool {
	left, errA := semreg.CanonicalJSON(a)
	right, errB := semreg.CanonicalJSON(b)
	return errA == nil && errB == nil && bytes.Equal(left, right)
}

// capabilityInstances is the availability of capability elements.
// A lens result is that availability (TGT-6): instances of this asset, and
// none when the asset is ungraphed (ACT-5). The per-asset capture has no
// lens result; a capability is then judged from its own instance on the
// snapshot, which is the record a lens result carries for an element it
// includes (LRS-1), and only for the five shipped packs the retired helper
// admitted.
func capabilityInstances(snapshot semreg.Snapshot, lens *graph.LensResult) []semreg.CapabilityInstance {
	if lens != nil {
		if assetUngraphed(lens, snapshot.AssetID) {
			return []semreg.CapabilityInstance{}
		}
		out := []semreg.CapabilityInstance{}
		for _, capability := range lens.Capabilities {
			if capability.AssetID == snapshot.AssetID {
				out = append(out, capability)
			}
		}
		return out
	}
	known := shippedPacks()
	out := []semreg.CapabilityInstance{}
	for _, capability := range snapshot.Capabilities {
		if known[capability.Definition.Pack] {
			out = append(out, capability)
		}
	}
	return out
}

func shippedPacks() map[semreg.PackRef]bool {
	packs := map[semreg.PackRef]bool{}
	for _, provider := range []semreg.PackMetadata{evse.Metadata(), infrastructure.Metadata(), pv.Metadata(), storage.Metadata(), thermal.Metadata()} {
		packs[provider.Pack] = true
	}
	return packs
}

func project(metadata *semreg.PackMetadataRegistry, s semreg.Snapshot, e semreg.EvaluationView, lens *graph.LensResult) (projection.ProjectionReport, error) {
	applicable := map[semreg.PackRef]bool{}
	items := map[projection.RequestedItem]bool{}
	for _, f := range s.Facts {
		applicable[semreg.PackRef{ID: f.Key.PackID, Version: f.Key.PackVersion}] = true
		items[projection.RequestedItem{Kind: projection.ItemFact, ItemID: f.Key.FactID}] = true
	}
	for _, v := range s.Services {
		applicable[v.Definition.Pack] = true
	}
	for _, v := range s.Capabilities {
		applicable[v.Definition.Pack] = true
		items[projection.RequestedItem{Kind: projection.ItemCapability, ItemID: v.Definition.ID}] = true
	}
	manifest := projection.ProjectionManifest{TargetID: "target:semantic-snapshot-read", TargetVersion: "1", KernelVersion: semreg.ContractKernelV1, MappingRevision: "1", PackVersions: []semreg.PackRef{}}
	for _, provider := range []semreg.PackMetadata{evse.Metadata(), infrastructure.Metadata(), pv.Metadata(), storage.Metadata(), thermal.Metadata()} {
		m := provider
		if !applicable[m.Pack] {
			continue
		}
		for _, f := range m.Fields {
			items[projection.RequestedItem{Kind: projection.ItemFact, ItemID: f.Ref.ID}] = true
		}
		for _, c := range m.Capabilities {
			items[projection.RequestedItem{Kind: projection.ItemCapability, ItemID: c.Ref.ID}] = true
		}
	}
	// Preserve numeric version ordering for exact pack references.
	for pack := range applicable {
		manifest.PackVersions = append(manifest.PackVersions, pack)
	}
	sort.Slice(manifest.PackVersions, func(i, j int) bool {
		x, y := manifest.PackVersions[i], manifest.PackVersions[j]
		if x.ID != y.ID {
			return x.ID < y.ID
		}
		return versionLess(x.Version, y.Version)
	})
	requested := make([]projection.RequestedItem, 0, len(items))
	for item := range items {
		requested = append(requested, item)
	}
	sort.Slice(requested, func(i, j int) bool {
		if requested[i].Kind != requested[j].Kind {
			return requested[i].Kind < requested[j].Kind
		}
		return requested[i].ItemID < requested[j].ItemID
	})
	type fieldKey struct {
		pack semreg.PackRef
		id   semreg.DefinitionID
	}
	fields := map[fieldKey]semreg.DefinitionRef{}
	for _, provider := range []semreg.PackMetadata{evse.Metadata(), infrastructure.Metadata(), pv.Metadata(), storage.Metadata(), thermal.Metadata()} {
		for _, field := range provider.Fields {
			fields[fieldKey{field.Ref.Pack, field.Ref.ID}] = field.Ref
		}
	}
	services := map[semreg.ServiceInstanceID]semreg.DefinitionRef{}
	for _, service := range s.Services {
		services[service.InstanceID] = service.Definition
	}
	dispositions := make([]projection.ProjectionDisposition, 0, len(requested))
	for _, item := range requested {
		d := projection.ProjectionDisposition{Kind: item.Kind, ItemID: item.ItemID, SourceKeys: []semreg.FactKey{}, Loss: []projection.LossDetail{}, Outcome: projection.ProjectionUnknown}
		reason := semreg.DefinitionID(reasonPrefix + "value_unknown")
		reasons := []semreg.DefinitionID{}
		candidates := []semreg.FactCandidate{}
		conflictOwned := false
		if item.Kind == projection.ItemFact {
			// Availability and exactness are tracked per complete source key: one
			// available sibling must not hide an unavailable or withdrawn one.
			hasValue, unsupported, found, conflicted := false, true, false, false
			allKeysAvailable, allKeysExact, anyUnknown := true, true, false
			for _, f := range s.Facts {
				if f.Key.FactID != item.ItemID {
					continue
				}
				d.SourceKeys = append(d.SourceKeys, f.Key)
				keyAvailable, keyExact := false, false
				for _, c := range f.Candidates {
					found = true
					hasValue = hasValue || c.Value != nil
					unsupported = unsupported && c.Quality.Qualification == semreg.QualificationUnsupported
					anyUnknown = anyUnknown || (c.Value != nil && c.Quality.Qualification == semreg.QualificationUnknown)
					reasons = append(reasons, c.Quality.Reasons...)
					candidates = append(candidates, c)
					if v, ok := candidateEvaluation(e, lens, s.AssetID, c); ok {
						available := v.EffectiveAvailability != semreg.AvailabilityUnavailable && v.EffectiveAvailability != semreg.AvailabilityWithdrawn
						keyAvailable = keyAvailable || available
						// Exact needs a value that is qualified, promoted, valid (good, or
						// suspect as the kernel allows for promoted state) and available.
						keyExact = keyExact || (available && c.Value != nil && c.Quality.Qualification == semreg.QualificationQualified &&
							c.Quality.Promotion == semreg.PromotionPromoted && (c.Quality.Validity == semreg.ValidityGood || c.Quality.Validity == semreg.ValiditySuspect))
					}
				}
				// Candidates of several sources that disagree carry an open
				// conflict; such a key is never exact, whichever candidate a
				// presentation surface shows.
				keyConflicted := openConflict(f)
				conflicted = conflicted || keyConflicted
				allKeysAvailable = allKeysAvailable && keyAvailable
				allKeysExact = allKeysExact && keyExact && !keyConflicted
			}
			switch {
			case found && !allKeysAvailable:
				// Any unavailable key withholds the whole disposition (SourceKeys names
				// every key); the withheld key's value is never reported as exact.
				d.Outcome = projection.ProjectionWithheld
				reason = reasonPrefix + "fact_unavailable"
			case found && unsupported:
				d.Outcome = projection.ProjectionUnsupported
			case hasValue && conflicted:
				// The sources disagree on the value: it is unknown, not withheld
				// for a lack of qualification.
				d.Outcome = projection.ProjectionUnknown
				conflictOwned = true
			case hasValue && allKeysExact:
				d.Outcome = projection.ProjectionExact
			case hasValue && anyUnknown:
				d.Outcome = projection.ProjectionUnknown
			case hasValue:
				// A valued candidate that is not qualified, promoted and valid stays
				// audit evidence only.
				d.Outcome = projection.ProjectionWithheld
				reason = reasonPrefix + "capability_unqualified"
			}
		} else {
			reason = reasonPrefix + "capability_unknown"
			// Multiple native instances remain intact; a definition is admitted only
			// when every instance is qualified and available for this read.
			found, qualified, unsupported, unknown, available := false, true, true, false, true
			for _, c := range capabilityInstances(s, lens) {
				if c.Definition.ID != item.ItemID {
					continue
				}
				found = true
				qualified = qualified && c.Qualification == semreg.QualificationQualified
				unsupported = unsupported && c.Qualification == semreg.QualificationUnsupported
				unknown = unknown || c.Qualification == semreg.QualificationUnknown
				available = available && c.Availability != semreg.AvailabilityUnavailable && c.Availability != semreg.AvailabilityWithdrawn
			}
			// Supporting keys are joined through the pack's declared service and
			// capability ownership, including the complete dimensioned FactKey.
			for _, f := range s.Facts {
				matched := false
				field, known := fields[fieldKey{semreg.PackRef{ID: f.Key.PackID, Version: f.Key.PackVersion}, f.Key.FactID}]
				for _, c := range s.Capabilities {
					if known && c.Definition.ID == item.ItemID && metadata.FieldMatches(field, services[c.ServiceInstance], c.Definition) {
						matched = true
					}
				}
				if matched {
					d.SourceKeys = append(d.SourceKeys, f.Key)
					for _, candidate := range f.Candidates {
						reasons = append(reasons, candidate.Quality.Reasons...)
						candidates = append(candidates, candidate)
					}
				}
			}
			if found {
				switch {
				case !available:
					d.Outcome = projection.ProjectionWithheld
					reason = reasonPrefix + "capability_unavailable"
				case unsupported:
					d.Outcome = projection.ProjectionUnsupported
				case unknown:
				case !qualified:
					d.Outcome = projection.ProjectionWithheld
					reason = reasonPrefix + "capability_unqualified"
				default:
					d.Outcome = projection.ProjectionExact
				}
			}
		}
		for _, r := range reasons {
			if r == PendingNormativeSourceReason {
				d.Outcome = projection.ProjectionUnknown
				reason = r
				break
			}
		}
		if conflictOwned && reason == reasonPrefix+"value_unknown" {
			// The disagreement owns the reason; native reasons of the
			// candidates must not replace it.
			d.Reason = &reason
		} else if d.Outcome != projection.ProjectionExact {
			if reason != PendingNormativeSourceReason {
				reasons = outcomeReasons(reason, item.Kind, causalOutcomeCandidates(reason, item.Kind, candidates, e, lens, s.AssetID), reasons)
			}
			if len(reasons) > 0 && reason != PendingNormativeSourceReason {
				sort.Slice(reasons, func(i, j int) bool { return reasons[i] < reasons[j] })
				reason = reasons[0]
			}
			d.Reason = &reason
		}
		dispositions = append(dispositions, d)
	}
	return projection.Project(s, manifest, requested, dispositions, nil)
}

// openConflict reports whether the envelope carries an open value conflict.
func openConflict(f semreg.FactEnvelope) bool {
	for _, conflict := range f.Conflicts {
		if conflict.State == semreg.ConflictOpen {
			return true
		}
	}
	return false
}

// causalOutcomeCandidates selects whole source keys before native reason
// intersection. Available or exact siblings cannot explain another key's
// withholding; every candidate within a selected key still has to explain it.
func causalOutcomeCandidates(reason semreg.DefinitionID, kind projection.ItemKind, candidates []semreg.FactCandidate, e semreg.EvaluationView, lens *graph.LensResult, asset semreg.AssetID) []semreg.FactCandidate {
	switch reason {
	case reasonPrefix + "fact_unavailable", reasonPrefix + "capability_unavailable", reasonPrefix + "capability_unqualified":
	default:
		return candidates
	}
	keys := map[string][]semreg.FactCandidate{}
	order := []string{}
	for _, c := range candidates {
		key, _ := semreg.CanonicalJSON(c.Key)
		if _, found := keys[string(key)]; !found {
			order = append(order, string(key))
		}
		keys[string(key)] = append(keys[string(key)], c)
	}
	causal := []semreg.FactCandidate{}
	for _, key := range order {
		available, qualified := false, false
		for _, c := range keys[key] {
			v, ok := candidateEvaluation(e, lens, asset, c)
			if !ok {
				continue
			}
			admitted := v.EffectiveAvailability != semreg.AvailabilityUnavailable && v.EffectiveAvailability != semreg.AvailabilityWithdrawn
			available = available || admitted
			q := c.Quality
			exact := q.Qualification == semreg.QualificationQualified
			if kind == projection.ItemFact {
				exact = exact && admitted && c.Value != nil && q.Promotion == semreg.PromotionPromoted &&
					(q.Validity == semreg.ValidityGood || q.Validity == semreg.ValiditySuspect)
			}
			qualified = qualified || exact
		}
		blocked := !available
		if reason == reasonPrefix+"capability_unqualified" {
			blocked = !qualified
		}
		if blocked {
			causal = append(causal, keys[key]...)
		}
	}
	return causal
}

// outcomeReasons requires every candidate on the causal keys to explain the
// selected withholding outcome and intersects their native reasons. Evaluation
// expiry and instance qualification alone do not establish native causality.
func outcomeReasons(reason semreg.DefinitionID, kind projection.ItemKind, candidates []semreg.FactCandidate, reasons []semreg.DefinitionID) []semreg.DefinitionID {
	switch reason {
	case reasonPrefix + "fact_unavailable", reasonPrefix + "capability_unavailable", reasonPrefix + "capability_unqualified":
	default:
		return reasons
	}
	if len(candidates) == 0 {
		return nil
	}
	shared := append([]semreg.DefinitionID(nil), candidates[0].Quality.Reasons...)
	for _, candidate := range candidates {
		q := candidate.Quality
		explains := q.Availability == semreg.AvailabilityUnavailable || q.Availability == semreg.AvailabilityWithdrawn
		if reason == reasonPrefix+"capability_unqualified" {
			explains = q.Qualification == semreg.QualificationCandidate || q.Qualification == semreg.QualificationRejected
			if kind == projection.ItemFact {
				explains = explains || q.Promotion != semreg.PromotionPromoted || (q.Validity != semreg.ValidityGood && q.Validity != semreg.ValiditySuspect)
			}
		}
		if !explains {
			return nil
		}
		kept := shared[:0]
		for _, r := range shared {
			for _, native := range q.Reasons {
				if r == native {
					kept = append(kept, r)
					break
				}
			}
		}
		shared = kept
	}
	return shared
}

func versionLess(a, b semreg.SemanticVersion) bool {
	// Semantic versions have validated integer components; compare numerically
	// without narrowing arbitrary-sized components to machine integers.
	var x, y [3]string
	copy(x[:], splitVersion(string(a)))
	copy(y[:], splitVersion(string(b)))
	for i := range x {
		if len(x[i]) != len(y[i]) {
			return len(x[i]) < len(y[i])
		}
		if x[i] != y[i] {
			return x[i] < y[i]
		}
	}
	return false
}
func splitVersion(s string) []string {
	var parts []string
	start := 0
	for i := range s {
		if s[i] == '.' {
			parts = append(parts, s[start:i])
			start = i + 1
		}
	}
	return append(parts, s[start:])
}
