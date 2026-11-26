package ebusgateway

// The semantic Prometheus target deliberately consumes only detached SemReg
// output. It is an observation surface, never a semantic authority.

import (
	"encoding/json"
	"math/big"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/Project-Helianthus/helianthus-gateway/internal/semselect"
	semreg "github.com/Project-Helianthus/helianthus-semreg/semreg/v1"
	"github.com/Project-Helianthus/helianthus-semreg/semreg/v1/graph"
	"github.com/Project-Helianthus/helianthus-semreg/semreg/v1/packs"
	"github.com/Project-Helianthus/helianthus-semreg/semreg/v1/projection"
)

// SemanticMetricsDomain is one coherent detached snapshot/evaluation/
// projection tuple. Name is the publication's domain. Owner is the source of
// the asset's owning driver; it ranks first when a fact key carries candidates
// of several sources, and an empty Owner names none.
type SemanticMetricsDomain struct {
	Name       string
	Snapshot   semreg.Snapshot
	Evaluation semreg.EvaluationView
	Projection projection.ProjectionReport
	Available  bool
	Owner      semreg.SourceID
}

func writeSemanticMetrics(w *prometheusWriter, domains []SemanticMetricsDomain, now time.Time) {
	w.writeHelp("helianthus_semantic_projection_available", "Detached SemReg projection availability for this Prometheus target.")
	w.writeType("helianthus_semantic_projection_available", "gauge")
	w.writeHelp("helianthus_semantic_projection_item", "Current requested SemReg projection outcome.")
	w.writeType("helianthus_semantic_projection_item", "gauge")
	w.writeHelp("helianthus_semantic_fact_state", "Current SemReg fact quality, effective availability, and freshness state.")
	w.writeType("helianthus_semantic_fact_state", "gauge")
	w.writeHelp("helianthus_semantic_fact_value", "Current fresh qualified numeric or boolean SemReg fact value.")
	w.writeType("helianthus_semantic_fact_value", "gauge")
	w.writeHelp("helianthus_semantic_evidence_age", "Age of selected SemReg evidence when wall clocks are comparable.")
	w.writeType("helianthus_semantic_evidence_age", "gauge")
	w.writeHelp("helianthus_semantic_open_conflicts", "Open conflicts on the detached SemReg fact envelope.")
	w.writeType("helianthus_semantic_open_conflicts", "gauge")
	w.writeHelp("helianthus_semantic_projection_loss", "Loss kinds declared by the detached SemReg projection.")
	w.writeType("helianthus_semantic_projection_loss", "gauge")
	w.writeHelp("helianthus_semantic_render_overflow", "Semantic metrics omitted because input was invalid or a duplicate series was withheld.")
	w.writeType("helianthus_semantic_render_overflow", "gauge")

	overflow := 0
	seen := make(map[string]struct{})
	emit := func(name string, value float64, labels map[string]string) bool {
		key := name
		keys := make([]string, 0, len(labels))
		for label := range labels {
			keys = append(keys, label)
		}
		sort.Strings(keys)
		for _, label := range keys {
			key += "\x00" + label + "=" + labels[label]
		}
		if _, ok := seen[key]; ok {
			overflow++
			return false
		}
		seen[key] = struct{}{}
		w.writeGaugeSample(name, value, labels)
		return true
	}
	for _, d := range domains {
		if !d.Available {
			emit("helianthus_semantic_projection_available", 0, labelMap("domain", d.Name))
			continue
		}
		snapshotOK := d.Snapshot.SnapshotID != "" && d.Evaluation.SnapshotID == d.Snapshot.SnapshotID && d.Evaluation.Revisions == d.Snapshot.Revisions && d.Evaluation.EvaluationDigest != ""
		projectionOK := snapshotOK && d.Projection.SnapshotID == d.Snapshot.SnapshotID && d.Projection.Revisions == d.Snapshot.Revisions
		if !snapshotOK || (d.Projection.SnapshotID != "" && !projectionOK) {
			emit("helianthus_semantic_projection_available", 0, labelMap("domain", d.Name))
			overflow++
			continue
		}
		facts := factIndex(d.Snapshot)
		resources := semanticResourceIndex(d.Snapshot)
		evaluated := evaluatedIndex(d.Evaluation)
		covered := map[string]struct{}{}
		if projectionOK {
			emit("helianthus_semantic_projection_available", 1, labelMap("domain", d.Name))
		}
		dispositions := append([]projection.ProjectionDisposition(nil), d.Projection.Dispositions...)
		sort.Slice(dispositions, func(i, j int) bool { return dispositions[i].ItemID < dispositions[j].ItemID })
		if projectionOK {
			for _, disposition := range dispositions {
				outcome := string(disposition.Outcome)
				if !validOutcome(outcome) || !validSemanticItem(d.Name, string(disposition.ItemID)) {
					overflow++
					continue
				}
				emit("helianthus_semantic_projection_item", 1, labelMap("domain", d.Name, "item", string(disposition.ItemID), "outcome", outcome))
				for _, loss := range disposition.Loss {
					if validLoss(string(loss.Kind)) {
						emit("helianthus_semantic_projection_loss", 1, labelMap("domain", d.Name, "item", string(disposition.ItemID), "loss_kind", string(loss.Kind)))
					} else {
						overflow++
					}
				}
				if disposition.Outcome != projection.ProjectionExact && disposition.Outcome != projection.ProjectionTransformed {
					continue
				}
				_, schemaOK := semanticFactSchemaFor(d.Name, string(disposition.ItemID))
				for _, key := range disposition.SourceKeys {
					if schemaOK {
						if canon, ok := semanticFactKey(key); ok {
							covered[canon] = struct{}{}
						}
					}
					if renderSemanticFact(emit, d.Name, string(disposition.ItemID), key, facts, evaluated, resources, d.Owner, now) {
						overflow++
					}
				}
			}
		}
		overflow += renderUncoveredFacts(emit, d.Snapshot, evaluated, resources, d.Owner, covered, !projectionOK, now)
	}
	if overflow != 0 {
		w.writeGaugeSample("helianthus_semantic_render_overflow", float64(overflow), nil)
	}
}

func factIndex(snapshot semreg.Snapshot) map[string]semreg.FactEnvelope {
	out := make(map[string]semreg.FactEnvelope, len(snapshot.Facts))
	for _, f := range snapshot.Facts {
		for _, c := range f.Candidates {
			out[string(c.CandidateID)] = f
		}
	}
	return out
}

// semanticResourceIndex indexes the resources of a snapshot that carries
// graph records. It is nil for a snapshot without them: the dimension values
// of such a snapshot are its publisher's own text.
func semanticResourceIndex(snapshot semreg.Snapshot) map[semreg.ResourceID]semreg.Resource {
	if len(snapshot.Resources) == 0 {
		return nil
	}
	out := make(map[semreg.ResourceID]semreg.Resource, len(snapshot.Resources))
	for _, resource := range snapshot.Resources {
		out[resource.ResourceID] = resource
	}
	return out
}

// semanticResourceDimensionText is the qualifier role a schema compares. In a
// snapshot with resources the dimension value is the subject's resource
// identifier, and the role comes from that resource's kind and, for a phase,
// its exact anchor slot. A snapshot without resources takes the role from the
// dimension identifier. The published text stays the subject; it is not split.
func semanticResourceDimensionText(dimension semreg.Dimension, resources map[semreg.ResourceID]semreg.Resource) (string, bool) {
	if dimension.Value.Kind != semreg.ValueText || dimension.Value.Text == nil || *dimension.Value.Text == "" {
		return "", false
	}
	text := *dimension.Value.Text
	if resources == nil {
		if dimension.ID == "storage.dimension.pack" {
			return "", false
		}
		switch dimension.ID {
		case "pv.dimension.inverter":
			return "inverter", true
		case "pv.dimension.system":
			return "system", true
		case "pv.dimension.phase":
			switch text {
			case "phase:L1", "phase:L2", "phase:L3":
				return text, true
			default:
				return "", false
			}
		default:
			return text, true
		}
	}
	resource, ok := resources[semreg.ResourceID(text)]
	if !ok || resource.State != semreg.ResourceCurrent {
		return "", false
	}
	switch resource.Kind {
	case "resource.inverter":
		return "inverter", true
	case "resource.pv_system":
		return "system", true
	case "resource.battery_pack":
		return "pack", true
	case "resource.phase":
		if resource.Anchor.Slot == nil {
			return "", false
		}
		switch *resource.Anchor.Slot {
		case "L1", "L2", "L3":
			return "phase:" + *resource.Anchor.Slot, true
		}
	}
	return "", false
}

func evaluatedIndex(view semreg.EvaluationView) map[semreg.CandidateID]semreg.EvaluatedFact {
	out := make(map[semreg.CandidateID]semreg.EvaluatedFact, len(view.Facts))
	for _, f := range view.Facts {
		out[f.CandidateID] = f
	}
	return out
}

// semanticPresented returns the candidate of the envelope that the target
// presents: the only candidate, or, with candidates of several sources, the
// one the presentation policy selects. presented is false when the policy
// selects none; invalid is true when a candidate has no evaluation.
func semanticPresented(envelope semreg.FactEnvelope, evaluated map[semreg.CandidateID]semreg.EvaluatedFact, owner semreg.SourceID) (candidate semreg.FactCandidate, presented, invalid bool) {
	if len(envelope.Candidates) == 1 {
		return envelope.Candidates[0], true, false
	}
	// The owning source's candidate stands for the key whatever its state, so a
	// stale or unavailable owner is shown as such and never replaced by another
	// source. It applies when the owner has exactly one native observation.
	if owner != "" {
		var owned []semreg.FactCandidate
		for _, c := range envelope.Candidates {
			if c.Origin.Kind == semreg.OriginNativeObservation && c.Origin.SourceID != nil && *c.Origin.SourceID == owner {
				owned = append(owned, c)
			}
		}
		if len(owned) == 1 {
			return owned[0], true, false
		}
	}
	facts := make([]semreg.EvaluatedFact, 0, len(envelope.Candidates))
	for _, c := range envelope.Candidates {
		e, ok := evaluated[c.CandidateID]
		if !ok {
			return semreg.FactCandidate{}, false, true
		}
		facts = append(facts, e)
	}
	selected, ok := semselect.Choose(envelope, facts, owner)
	if !ok {
		return semreg.FactCandidate{}, false, false
	}
	for _, c := range envelope.Candidates {
		if c.CandidateID == selected {
			return c, true, false
		}
	}
	return semreg.FactCandidate{}, false, true
}

// semanticEnvelopeFor finds the envelope of a canonical fact key. Candidate
// identifiers are visited in order, so the result never depends on map order.
func semanticEnvelopeFor(targetKey string, envelopes map[string]semreg.FactEnvelope) (envelope semreg.FactEnvelope, found, invalidCandidate bool) {
	ids := make([]string, 0, len(envelopes))
	for id := range envelopes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		candidate := envelopes[id]
		for _, c := range candidate.Candidates {
			if string(c.CandidateID) != id {
				continue
			}
			candidateKey, ok := semanticFactKey(c.Key)
			if !ok {
				invalidCandidate = true
				continue
			}
			if candidateKey == targetKey && !found {
				envelope, found = candidate, true
			}
		}
	}
	return envelope, found, invalidCandidate
}

func renderSemanticFact(emit func(string, float64, map[string]string) bool, domain, item string, key semreg.FactKey, envelopes map[string]semreg.FactEnvelope, evaluated map[semreg.CandidateID]semreg.EvaluatedFact, resources map[semreg.ResourceID]semreg.Resource, owner semreg.SourceID, now time.Time) bool {
	targetKey, targetOK := semanticFactKey(key)
	schema, schemaOK := semanticFactSchemaFor(domain, item)
	if !targetOK || !schemaOK {
		return true
	}
	envelope, found, invalidCandidate := semanticEnvelopeFor(targetKey, envelopes)
	if !found {
		return invalidCandidate
	}
	candidate, presented, invalid := semanticPresented(envelope, evaluated, owner)
	if invalid {
		return true
	}
	if !presented {
		// Several candidates and none selected: only the conflict state is
		// observable; no candidate's state, age or value stands for the key.
		subject, dimensionsOK := semanticSubject(domain, item, key, resources)
		if !dimensionsOK {
			return true
		}
		emit("helianthus_semantic_open_conflicts", float64(len(envelope.Conflicts)), semanticFactLabels(key, subject))
		return false
	}
	e, ok := evaluated[candidate.CandidateID]
	if !ok {
		return true
	}
	if !validSemanticState(candidate.Quality.Qualification, candidate.Quality.Promotion, candidate.Quality.Validity, e.EffectiveAvailability, e.Freshness) {
		return true
	}
	if !schema.matches(key, candidate.Value, resources) {
		return true
	}
	subject, dimensionsOK := semanticSubject(domain, item, key, resources)
	if !dimensionsOK {
		return true
	}
	labels := semanticFactLabels(key, subject, "qualification", string(candidate.Quality.Qualification), "promotion", string(candidate.Quality.Promotion), "quality", string(candidate.Quality.Validity), "availability", string(e.EffectiveAvailability), "freshness", string(e.Freshness))
	emit("helianthus_semantic_fact_state", 1, labels)
	emit("helianthus_semantic_open_conflicts", float64(len(envelope.Conflicts)), semanticFactLabels(key, subject))
	if age, ok := semanticAge(candidate.Times.ReceivedAt, now); ok {
		emit("helianthus_semantic_evidence_age", age, semanticFactLabels(key, subject, "unit", "seconds"))
	}
	// A conflicted key is never exported as a value, whichever candidate is
	// presented.
	if len(envelope.Conflicts) != 0 || candidate.Quality.Qualification != semreg.QualificationQualified || candidate.Quality.Promotion != semreg.PromotionPromoted || candidate.Quality.Validity != semreg.ValidityGood || e.EffectiveAvailability != semreg.AvailabilityAvailable || e.Freshness != semreg.FreshnessFresh {
		return false
	}
	value, unit, ok := semanticNumeric(key, candidate.Value)
	if ok {
		emit("helianthus_semantic_fact_value", value, semanticFactLabels(key, subject, "unit", unit))
	}
	return false
}

func validSemanticState(q semreg.Qualification, p semreg.Promotion, v semreg.Validity, a semreg.Availability, f semreg.Freshness) bool {
	return (q == semreg.QualificationCandidate || q == semreg.QualificationQualified || q == semreg.QualificationUnsupported || q == semreg.QualificationUnknown || q == semreg.QualificationRejected) && (p == semreg.PromotionPromoted || p == semreg.PromotionUnpromoted) && (v == semreg.ValidityGood || v == semreg.ValiditySuspect || v == semreg.ValidityBad || v == semreg.ValidityUnknown) && (a == semreg.AvailabilityAvailable || a == semreg.AvailabilityDegraded || a == semreg.AvailabilityUnavailable || a == semreg.AvailabilityWithdrawn) && (f == semreg.FreshnessFresh || f == semreg.FreshnessStale || f == semreg.FreshnessExpired || f == semreg.FreshnessUnknown)
}

// semanticFactSubject is the value-key half of one per-fact series: the
// resource, its kind, and the qualifier labels that apply. Dimension and
// connector are set only for a qualifier of that kind. An empty resource is
// omitted from the labels rather than emitted blank.
type semanticFactSubject struct {
	resource, resourceKind, dimension, connector string
}

func semanticFactLabels(key semreg.FactKey, subject semanticFactSubject, extra ...string) map[string]string {
	labels := labelMap("pack", string(key.PackID), "fact", string(key.FactID))
	if subject.resource != "" {
		labels["resource"] = subject.resource
	}
	if subject.resourceKind != "" {
		labels["resource_kind"] = subject.resourceKind
	}
	if subject.dimension != "" {
		labels["dimension"] = subject.dimension
	}
	if subject.connector != "" {
		labels["connector"] = subject.connector
	}
	for index := 0; index+1 < len(extra); index += 2 {
		labels[extra[index]] = extra[index+1]
	}
	return labels
}

func semanticSubject(domain, item string, key semreg.FactKey, resources map[semreg.ResourceID]semreg.Resource) (semanticFactSubject, bool) {
	if len(key.Dimensions) != 1 {
		return semanticFactSubject{}, false
	}
	dimension := key.Dimensions[0]
	if dimension.Value.Kind != semreg.ValueText || dimension.Value.Text == nil || *dimension.Value.Text == "" {
		return semanticFactSubject{}, false
	}
	text := *dimension.Value.Text
	if dimension.ID == "evse.dimension.connector" {
		if item != "evse.limit.allocated_current" {
			return semanticFactSubject{}, false
		}
		resource, kind, ok := semanticCurrentResource(text, resources)
		if ok {
			return semanticFactSubject{resource: resource, resourceKind: kind, dimension: "connector", connector: resource}, true
		}
		if resources != nil {
			return semanticFactSubject{}, false
		}
		return semanticFactSubject{resource: text, resourceKind: "resource.connector", dimension: "connector", connector: text}, true
	}
	if domain == "evse" {
		if item != "evse.limit.configured_current" || dimension.ID != "evse.dimension.evse" {
			return semanticFactSubject{}, false
		}
		resource, kind, ok := semanticCurrentResource(text, resources)
		if ok {
			return semanticFactSubject{resource: resource, resourceKind: kind, dimension: "evse"}, true
		}
		if resources != nil {
			return semanticFactSubject{}, false
		}
		return semanticFactSubject{resource: text, resourceKind: "resource.evse", dimension: "evse"}, true
	}
	value, ok := semanticResourceDimensionText(dimension, resources)
	if !ok {
		return semanticFactSubject{}, false
	}
	label := value
	switch value {
	case "phase:L1":
		label = "phase_l1"
	case "phase:L2":
		label = "phase_l2"
	case "phase:L3":
		label = "phase_l3"
	}
	resource, kind, found := semanticCurrentResource(text, resources)
	if resources != nil && !found {
		return semanticFactSubject{}, false
	}
	if !found {
		kind = semanticUngraphedKind(value)
		resource = text
	}
	return semanticFactSubject{resource: resource, resourceKind: kind, dimension: label}, kind != "" && label != ""
}

func semanticCurrentResource(text string, resources map[semreg.ResourceID]semreg.Resource) (string, string, bool) {
	if resources == nil {
		return "", "", false
	}
	resource, ok := resources[semreg.ResourceID(text)]
	if !ok || resource.State != semreg.ResourceCurrent {
		return "", "", false
	}
	return string(resource.ResourceID), string(resource.Kind), true
}

func semanticUngraphedKind(value string) string {
	switch value {
	case "inverter":
		return "resource.inverter"
	case "system":
		return "resource.pv_system"
	case "phase:L1", "phase:L2", "phase:L3":
		return "resource.phase"
	case "pack":
		return "resource.battery_pack"
	default:
		return ""
	}
}

// renderUncoveredFacts exports eligible values of an accepted publication
// that no schema item owns. A schema rejection stays withheld.
func renderUncoveredFacts(emit func(string, float64, map[string]string) bool, snapshot semreg.Snapshot, evaluated map[semreg.CandidateID]semreg.EvaluatedFact, resources map[semreg.ResourceID]semreg.Resource, owner semreg.SourceID, covered map[string]struct{}, includeSchema bool, now time.Time) int {
	type row struct {
		canon string
		env   semreg.FactEnvelope
	}
	rows := make([]row, 0, len(snapshot.Facts))
	overflow := 0
	for _, env := range snapshot.Facts {
		key := env.Key
		if !includeSchema && semanticFactCovered(string(key.PackID), string(key.FactID)) {
			continue
		}
		canon, ok := semanticFactKey(key)
		if !ok {
			overflow++
			continue
		}
		if _, ok := covered[canon]; ok {
			continue
		}
		rows = append(rows, row{canon: canon, env: env})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].canon < rows[j].canon })
	for _, row := range rows {
		if renderPublishedFact(emit, row.env, evaluated, resources, owner, now) {
			overflow++
		}
	}
	return overflow
}

func renderPublishedFact(emit func(string, float64, map[string]string) bool, envelope semreg.FactEnvelope, evaluated map[semreg.CandidateID]semreg.EvaluatedFact, resources map[semreg.ResourceID]semreg.Resource, owner semreg.SourceID, now time.Time) bool {
	key := envelope.Key
	if _, ok := semanticFactKey(key); !ok {
		return true
	}
	candidate, presented, invalid := semanticPresented(envelope, evaluated, owner)
	if invalid {
		return true
	}
	subject, dimensionsOK := semanticGenericSubject(key, resources)
	if !dimensionsOK {
		return true
	}
	if !presented {
		emit("helianthus_semantic_open_conflicts", float64(len(envelope.Conflicts)), semanticFactLabels(key, subject))
		return false
	}
	e, ok := evaluated[candidate.CandidateID]
	if !ok || !validSemanticState(candidate.Quality.Qualification, candidate.Quality.Promotion, candidate.Quality.Validity, e.EffectiveAvailability, e.Freshness) {
		return true
	}
	labels := semanticFactLabels(key, subject, "qualification", string(candidate.Quality.Qualification), "promotion", string(candidate.Quality.Promotion), "quality", string(candidate.Quality.Validity), "availability", string(e.EffectiveAvailability), "freshness", string(e.Freshness))
	emit("helianthus_semantic_fact_state", 1, labels)
	emit("helianthus_semantic_open_conflicts", float64(len(envelope.Conflicts)), semanticFactLabels(key, subject))
	if age, ok := semanticAge(candidate.Times.ReceivedAt, now); ok {
		emit("helianthus_semantic_evidence_age", age, semanticFactLabels(key, subject, "unit", "seconds"))
	}
	if len(envelope.Conflicts) != 0 || candidate.Quality.Qualification != semreg.QualificationQualified || candidate.Quality.Promotion != semreg.PromotionPromoted || candidate.Quality.Validity != semreg.ValidityGood || e.EffectiveAvailability != semreg.AvailabilityAvailable || e.Freshness != semreg.FreshnessFresh {
		return false
	}
	value, unit, ok := semanticNumeric(key, candidate.Value)
	if ok {
		emit("helianthus_semantic_fact_value", value, semanticFactLabels(key, subject, "unit", unit))
	}
	return false
}

// semanticGenericSubject preserves the single-qualifier label and uses the
// catalog subject to find the resource when the key has multiple qualifiers.
func semanticGenericSubject(key semreg.FactKey, resources map[semreg.ResourceID]semreg.Resource) (semanticFactSubject, bool) {
	if len(key.Dimensions) == 0 {
		return semanticFactSubject{}, false
	}
	dimension := key.Dimensions[0]
	label := string(dimension.ID)
	if len(key.Dimensions) > 1 {
		catalog, err := graph.Embedded()
		if err != nil {
			return semanticFactSubject{}, false
		}
		subject, ok := catalog.FactSubject(semreg.PackRef{ID: key.PackID, Version: key.PackVersion}, key.FactID)
		if !ok || !subject.Resolved {
			return semanticFactSubject{}, false
		}
		dimension = semreg.Dimension{}
		for _, qualifier := range key.Dimensions {
			if qualifier.ID == subject.Dimension {
				dimension = qualifier
				break
			}
		}
		// FactKey validation has already checked the canonical dimension order.
		// The JSON array preserves that order and keeps typed values unambiguous.
		encoded, err := json.Marshal(key.Dimensions)
		if err != nil {
			return semanticFactSubject{}, false
		}
		label = string(encoded)
	}
	if dimension.Value.Kind != semreg.ValueText || dimension.Value.Text == nil || *dimension.Value.Text == "" {
		return semanticFactSubject{}, false
	}
	text := *dimension.Value.Text
	if resources != nil {
		resource, kind, ok := semanticCurrentResource(text, resources)
		if !ok {
			return semanticFactSubject{}, false
		}
		return semanticFactSubject{resource: resource, resourceKind: kind, dimension: label}, true
	}
	return semanticFactSubject{resource: text, dimension: label}, true
}

func semanticFactCovered(pack, fact string) bool {
	switch fact {
	case "pv.ac.current", "pv.ac.voltage", "pv.ac.aggregate_active_power", "pv.ac.frequency", "pv.energy.generated", "pv.temperature.inverter", "pv.status.operating":
		return pack == "helianthus.pack.pv"
	case "storage.capacity.charge", "storage.capacity.discharge", "storage.pack.current", "storage.pack.voltage", "storage.state.soc", "storage.temperature.pack", "storage.status.operating":
		return pack == "helianthus.pack.storage"
	case "evse.limit.configured_current", "evse.limit.allocated_current":
		return pack == "helianthus.pack.evse"
	default:
		return false
	}
}

func semanticFactKey(key semreg.FactKey) (string, bool) {
	if key.Validate() != nil {
		return "", false
	}
	encoded, err := semreg.CanonicalJSON(key)
	if err != nil {
		return "", false
	}
	return string(encoded), true
}

func validSemanticItem(domain, item string) bool {
	_, ok := semanticFactSchemaFor(domain, item)
	if ok {
		return true
	}
	return domain == "pv" && (item == "projection.gateway.pv.inverter.ac.current.total" || item == "projection.gateway.pv.inverter.events.1" || item == "projection.gateway.pv.inverter.events.2")
}

// semanticFactSchema is the complete finite v1 contract for a fact-backed
// projection item. It binds pack version, fact, dimension, and value shape so
// independent allowlists cannot accidentally compose a different metric.
type semanticFactSchema struct {
	domain, item, pack, version, fact, dimensionID, dimensionValue, unit string
	symbols                                                              map[string]bool
}

func (s semanticFactSchema) matches(key semreg.FactKey, value *semreg.Value, resources map[semreg.ResourceID]semreg.Resource) bool {
	if string(key.PackID) != s.pack || string(key.PackVersion) != s.version || string(key.FactID) != s.fact || len(key.Dimensions) != 1 || value == nil {
		return false
	}
	dimension := key.Dimensions[0]
	if string(dimension.ID) != s.dimensionID || dimension.Value.Kind != semreg.ValueText || dimension.Value.Text == nil || *dimension.Value.Text == "" {
		return false
	}
	if s.dimensionValue != "" {
		if text, ok := semanticResourceDimensionText(dimension, resources); !ok || text != s.dimensionValue {
			return false
		}
	}
	if s.symbols != nil {
		return value.Kind == semreg.ValueSymbol && value.Symbol != nil && value.Symbol.Known && string(value.Symbol.Namespace) == s.fact && s.symbols[string(value.Symbol.Token)]
	}
	if value.Kind != semreg.ValueQuantity || value.Quantity == nil {
		return false
	}
	unit := string(value.Quantity.Unit)
	if unit == s.unit {
		return true
	}
	// A percent is accepted on a ratio fact. The value export divides it by 100.
	return s.unit == "unit.ratio" && unit == "unit.percent"
}

func semanticFactSchemaFor(domain, item string) (semanticFactSchema, bool) {
	const pvPack, pvVersion = "helianthus.pack.pv", "1.0.0"
	const storagePack, storageVersion = "helianthus.pack.storage", "1.1.0"
	const evsePack, evseVersion = "helianthus.pack.evse", "1.0.0"
	pv := func(item, fact, dimension, value, unit string) semanticFactSchema {
		return semanticFactSchema{domain: "pv", item: item, pack: pvPack, version: pvVersion, fact: fact, dimensionID: dimension, dimensionValue: value, unit: unit}
	}
	storage := func(item, unit string) semanticFactSchema {
		return semanticFactSchema{domain: "storage", item: item, pack: storagePack, version: storageVersion, fact: item, dimensionID: "storage.dimension.pack", dimensionValue: "pack", unit: unit}
	}
	if domain == "pv" {
		switch item {
		case "projection.gateway.pv.inverter.ac.current.phase_a":
			return pv(item, "pv.ac.current", "pv.dimension.phase", "phase:L1", "unit.ampere"), true
		case "projection.gateway.pv.inverter.ac.current.phase_b":
			return pv(item, "pv.ac.current", "pv.dimension.phase", "phase:L2", "unit.ampere"), true
		case "projection.gateway.pv.inverter.ac.current.phase_c":
			return pv(item, "pv.ac.current", "pv.dimension.phase", "phase:L3", "unit.ampere"), true
		case "projection.gateway.pv.inverter.ac.voltage.phase_a":
			return pv(item, "pv.ac.voltage", "pv.dimension.phase", "phase:L1", "unit.volt"), true
		case "projection.gateway.pv.inverter.ac.voltage.phase_b":
			return pv(item, "pv.ac.voltage", "pv.dimension.phase", "phase:L2", "unit.volt"), true
		case "projection.gateway.pv.inverter.ac.voltage.phase_c":
			return pv(item, "pv.ac.voltage", "pv.dimension.phase", "phase:L3", "unit.volt"), true
		case "projection.gateway.pv.inverter.ac.power.active":
			return pv(item, "pv.ac.aggregate_active_power", "pv.dimension.inverter", "inverter", "unit.watt"), true
		case "projection.gateway.pv.inverter.ac.frequency":
			return pv(item, "pv.ac.frequency", "pv.dimension.inverter", "inverter", "unit.hertz"), true
		case "projection.gateway.pv.inverter.ac.energy_lifetime":
			return pv(item, "pv.energy.generated", "pv.dimension.system", "system", "unit.joule"), true
		case "projection.gateway.pv.inverter.temperature.cabinet":
			return pv(item, "pv.temperature.inverter", "pv.dimension.inverter", "inverter", "unit.kelvin"), true
		case "projection.gateway.pv.inverter.operating_state":
			return semanticFactSchema{domain: "pv", item: item, pack: pvPack, version: pvVersion, fact: "pv.status.operating", dimensionID: "pv.dimension.inverter", dimensionValue: "inverter", symbols: map[string]bool{"generating": true}}, true
		}
	}
	if domain == "storage" {
		switch item {
		case "storage.capacity.charge", "storage.capacity.discharge":
			return storage(item, "unit.ampere_hour"), true
		case "storage.pack.current":
			return storage(item, "unit.ampere"), true
		case "storage.pack.voltage":
			return storage(item, "unit.volt"), true
		case "storage.state.soc":
			return storage(item, "unit.ratio"), true
		case "storage.temperature.pack":
			return storage(item, "unit.kelvin"), true
		case "storage.status.operating":
			return semanticFactSchema{domain: "storage", item: item, pack: storagePack, version: storageVersion, fact: item, dimensionID: "storage.dimension.pack", dimensionValue: "pack", symbols: map[string]bool{"active": true, "standby": true}}, true
		}
	}
	if domain == "evse" {
		switch item {
		case "evse.limit.configured_current":
			return semanticFactSchema{domain: "evse", item: item, pack: evsePack, version: evseVersion, fact: item, dimensionID: "evse.dimension.evse", unit: "unit.ampere"}, true
		case "evse.limit.allocated_current":
			return semanticFactSchema{domain: "evse", item: item, pack: evsePack, version: evseVersion, fact: item, dimensionID: "evse.dimension.connector", unit: "unit.ampere"}, true
		}
	}
	return semanticFactSchema{}, false
}

var semanticPrometheusMetadata = sync.OnceValue(func() *semreg.PackMetadataRegistry {
	metadata, _ := packs.NewMetadataRegistry()
	return metadata // A missing registry admits no quantities.
})

func semanticNumeric(key semreg.FactKey, value *semreg.Value) (float64, string, bool) {
	if value == nil {
		return 0, "", false
	}
	if value.Kind == semreg.ValueBoolean && value.Boolean != nil {
		if *value.Boolean {
			return 1, "boolean", true
		}
		return 0, "boolean", true
	}
	if value.Kind != semreg.ValueQuantity || value.Quantity == nil {
		return 0, "", false
	}
	unit := value.Quantity.Unit
	field := semreg.DefinitionRef{Pack: semreg.PackRef{ID: key.PackID, Version: key.PackVersion}, ID: key.FactID, Version: key.PackVersion}
	canonical, ok := semanticPrometheusMetadata().CanonicalUnit(field)
	percent := unit == "unit.percent" && canonical.ID == "unit.ratio"
	if !ok || (unit != canonical.ID && !percent) {
		return 0, "", false
	}
	r, ok := new(big.Rat).SetString(string(value.Quantity.Number.Coefficient))
	if !ok {
		return 0, "", false
	}
	if value.Quantity.Number.Exponent10 >= 0 {
		r.Mul(r, new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(value.Quantity.Number.Exponent10)), nil)))
	} else {
		r.Quo(r, new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(-value.Quantity.Number.Exponent10)), nil)))
	}
	f, _ := r.Float64()
	if percent {
		return f / 100, "ratio", true
	}
	return f, string(unit), true
}

func semanticAge(point semreg.TimePoint, now time.Time) (float64, bool) {
	if point.ClockID != "clock.utc" && point.ClockID != "wall.utc" {
		return 0, false
	}
	n, err := strconv.ParseInt(string(point.UnixNanoseconds), 10, 64)
	if err != nil {
		return 0, false
	}
	age := now.UTC().Sub(time.Unix(0, n)).Seconds()
	if age < 0 {
		return 0, false
	}
	return age, true
}
func validOutcome(v string) bool {
	switch v {
	case "exact", "transformed", "withheld", "unrepresentable", "unsupported", "unknown":
		return true
	}
	return false
}
func validLoss(v string) bool {
	switch v {
	case "unit", "range", "precision", "time", "symbol", "provenance", "identity", "capability", "operation", "policy":
		return true
	}
	return false
}
