package catalogv1

import (
	"bytes"
	"encoding/json"
	"errors"
	"time"

	"github.com/Project-Helianthus/helianthus-gateway/portal/contributionv1"
	semreg "github.com/Project-Helianthus/helianthus-semreg/semreg/v1"
)

// Catalog is a bounded optimistic detached capture. Sources are called once per
// attempt and must themselves be committed-state readers with no I/O.
func (c *Composer) Catalog() (Catalog, error) {
	if c == nil {
		return Catalog{}, errors.New("catalog composer missing")
	}
	for attempt := 0; attempt < 3; attempt++ {
		c.mu.RLock()
		rev := c.revision
		descriptors := make([]Descriptor, 0, len(c.descriptors))
		for _, d := range c.descriptors {
			descriptors = append(descriptors, d)
		}
		quarantined := make([]identity, 0, len(c.quarantined))
		for k := range c.quarantined {
			quarantined = append(quarantined, k)
		}
		source, invoker, revalidator, now := c.source, c.invoker, c.revalidator, c.now
		c.mu.RUnlock()
		fence := ""
		if fenced, ok := source.(FencedSourceCapture); ok {
			fence = fenced.Fence()
		}
		at := now().UTC()
		out := Catalog{Contract: Contract, EvaluationInstant: at, Domains: []Domain{}, Contributions: make([]Identity, 0, len(descriptors)), Descriptors: []ReadDescriptor{}, Resources: []Resource{}, Fields: []Field{}, Actions: []Action{}, Quarantines: make([]Quarantine, 0, len(quarantined))}
		accepted := make(map[identity]Descriptor, len(descriptors))
		for _, descriptor := range descriptors {
			accepted[identity{descriptor.Manifest.Contributor.DriverID, descriptor.Manifest.ManifestID, descriptor.Manifest.ManifestVersion}] = descriptor
		}
		var matches map[string]map[string]bool
		if source != nil {
			captured, err := source.Capture(at)
			if err != nil {
				return Catalog{}, err
			}
			matches, err = composeResources(&out, captured, accepted)
			if err != nil {
				return Catalog{}, err
			}
			if fenced, ok := source.(FencedSourceCapture); ok && fence != fenced.Fence() {
				continue
			}
		}
		admittedPacks := make(map[string]contributionv1.PackRef, len(out.Domains))
		for _, domain := range out.Domains {
			admittedPacks[domain.Pack.ID] = domain.Pack
		}
		for _, d := range descriptors {
			out.Contributions = append(out.Contributions, Identity{DriverID: d.Manifest.Contributor.DriverID, ManifestID: d.Manifest.ManifestID, ManifestVersion: d.Manifest.ManifestVersion, Digest: d.Digest})
			out.Descriptors = append(out.Descriptors, ReadDescriptor{
				Identity:          Identity{DriverID: d.Manifest.Contributor.DriverID, ManifestID: d.Manifest.ManifestID, ManifestVersion: d.Manifest.ManifestVersion, Digest: d.Digest},
				DescriptorVersion: DescriptorVersion, Groups: d.Manifest.Groups, Fields: d.Manifest.Fields, Views: d.Manifest.Views, Diagnostics: d.Manifest.Diagnostics,
			})
			if invoker == nil {
				continue
			}
			for _, resource := range out.Resources {
				if resource.ContributionDriverID != d.Manifest.Contributor.DriverID || resource.ContributionManifestID != d.Manifest.ManifestID || resource.ContributionManifestVersion != d.Manifest.ManifestVersion {
					continue
				}
				for _, a := range d.Manifest.Actions {
					if !matches[resource.ID][a.Group] || !matchesCapability(resource, a.ServiceRef, a.CapabilityRef) || a.CapabilityRef.Pack != admittedPacks[resource.Domain] || a.ServiceRef.Pack != admittedPacks[resource.Domain] {
						continue
					}
					action := Action{ID: a.ID, ResourceID: resource.ID, ServiceID: resource.ServiceID, CapabilityID: resource.CapabilityID, NativeMemberID: a.NativeMemberID, ContributionDriverID: resource.ContributionDriverID, ContributionManifestID: resource.ContributionManifestID, ContributionManifestVersion: resource.ContributionManifestVersion, Source: resource.Source, Discoverable: true, Enabled: revalidator != nil}
					if a.OperationRef != nil {
						action.OperationID = a.OperationRef.ID
					}
					out.Actions = append(out.Actions, action)
				}
			}
		}
		for _, k := range quarantined {
			out.Quarantines = append(out.Quarantines, Quarantine{DriverID: k.driver, ManifestID: k.manifest, ManifestVersion: k.version, Reason: "digest_conflict"})
		}
		sortCatalog(&out)
		structural := struct {
			Revision uint64
			Fence    string
			Catalog  Catalog
		}{rev, fence, catalogRevisionView(out)}
		revisionDigest, err := hash(structural)
		if err != nil {
			return Catalog{}, err
		}
		out.CatalogRevision = revisionDigest
		out.CatalogDigest = ""
		catalogDigest, err := hash(out)
		if err != nil {
			return Catalog{}, err
		}
		out.CatalogDigest = catalogDigest
		c.mu.RLock()
		unchanged := rev == c.revision
		c.mu.RUnlock()
		if unchanged {
			return clone(out), nil
		}
	}
	return Catalog{}, ErrCatalogChanged
}

// catalogRevisionView excludes response-time decoration from the action claim.
// It retains descriptors, source records, and lifecycle fences;
// native admission still revalidates freshness and expiry immediately before I/O.
func catalogRevisionView(c Catalog) Catalog {
	c.EvaluationInstant = time.Time{}
	c.CatalogRevision = ""
	c.CatalogDigest = ""
	for i := range c.Resources {
		c.Resources[i].Source = sourceRevisionView(c.Resources[i].Source)
	}
	for i := range c.Actions {
		c.Actions[i].Source = sourceRevisionView(c.Actions[i].Source)
	}
	return c
}

func sourceRevisionView(source Source) Source {
	source.EvaluationDigest = ""
	source.Evaluation = normalizeEvaluationClock(source.Evaluation)
	return source
}

// normalizeEvaluationClock removes only the volatile wall/monotonic coordinates
// from the action-stability view.  Facts, quality, provenance, projection
// dispositions and every lifecycle/source identity remain part of the claim.
func normalizeEvaluationClock(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil {
		return raw
	}
	stripEvaluationClock(value)
	normalized, err := json.Marshal(value)
	if err != nil {
		return raw
	}
	return json.RawMessage(normalized)
}

func stripEvaluationClock(value any) {
	root, ok := value.(map[string]any)
	if !ok {
		return
	}
	delete(root, "evaluation_digest")
	if context, ok := root["context"].(map[string]any); ok {
		delete(context, "evaluated_at")
		delete(context, "evaluate_monotonic")
	}
}

type sourceFieldKey struct {
	resourceID string
	id         string
}

func composeResources(out *Catalog, capture Capture, accepted map[identity]Descriptor) (map[string]map[string]bool, error) {
	domains := make(map[string]int, len(capture.Domains))
	for _, pack := range capture.Domains {
		if pack.ID == "" || pack.Version == "" {
			return nil, errors.New("invalid catalog domain")
		}
		if i, exists := domains[pack.ID]; exists {
			if out.Domains[i].Pack != pack {
				return nil, errors.New("ambiguous catalog domain version")
			}
			continue
		}
		domains[pack.ID] = len(out.Domains)
		out.Domains = append(out.Domains, Domain{Pack: pack, SourceState: "ABSENT", Reason: "source_unavailable"})
	}
	members := make(map[string]map[LensMember]bool, len(capture.LensMembers))
	for lens, result := range capture.LensMembers {
		members[lens] = make(map[LensMember]bool, len(result))
		for _, member := range result {
			members[lens][member] = true
		}
	}
	resources := make(map[string]Resource, len(capture.Resources))
	matchedGroups := make(map[string]map[string]bool, len(capture.Resources))
	for _, resource := range capture.Resources {
		owner := identity{resource.ContributionDriverID, resource.ContributionManifestID, resource.ContributionManifestVersion}
		descriptor, admitted := accepted[owner]
		if !admitted {
			continue
		}
		i, knownDomain := domains[resource.Domain]
		if !knownDomain {
			return nil, errors.New("invalid detached resource domain")
		}
		pack := out.Domains[i].Pack
		if semreg.ResourceID(resource.ID).Validate() != nil {
			return nil, errors.New("invalid detached resource_id")
		}
		member := LensMember{ResourceID: resource.ID, AssetID: resource.Source.AssetID}
		groups := make(map[string]bool)
		namedLens := false
		for _, view := range descriptor.Manifest.Views {
			if view.Slot != "lens" || view.Lens == "" {
				continue
			}
			namedLens = true
			if members[view.Lens][member] {
				groups[view.Group] = true
			}
		}
		// A named lens admits only a member of that lens result. A lens slot
		// that does not name a lens selects the group's resource context.
		if !namedLens {
			for _, group := range descriptor.Manifest.Groups {
				if group.ResourceContext == resource.ID {
					groups[group.ID] = true
				}
			}
		}
		if len(groups) == 0 {
			if namedLens {
				return nil, errors.New("invalid detached resource_id: outside the contribution's served lens")
			}
			continue
		}
		if !resourceMatchesManifest(resource, descriptor.Manifest, pack, groups) {
			continue
		}
		if _, exists := resources[resource.ID]; exists {
			return nil, errors.New("ambiguous detached resource_id")
		}
		resources[resource.ID], matchedGroups[resource.ID] = resource, groups
		out.Resources = append(out.Resources, resource)
		out.Domains[i].SourceState, out.Domains[i].Reason = "PRESENT", ""
	}
	seenFields := make(map[sourceFieldKey]bool, len(capture.Fields))
	for _, field := range capture.Fields {
		owner := identity{field.ContributionDriverID, field.ContributionManifestID, field.ContributionManifestVersion}
		descriptor, admitted := accepted[owner]
		if !admitted {
			continue
		}
		resource, exists := resources[field.ResourceID]
		if !exists || owner != (identity{resource.ContributionDriverID, resource.ContributionManifestID, resource.ContributionManifestVersion}) {
			return nil, errors.New("field has no exact detached resource context")
		}
		if !fieldMatchesManifest(field, resource, descriptor.Manifest, out.Domains[domains[resource.Domain]].Pack, matchedGroups[field.ResourceID]) {
			continue
		}
		key := sourceFieldKey{field.ResourceID, field.ID}
		if seenFields[key] {
			return nil, errors.New("ambiguous detached field identity")
		}
		seenFields[key] = true
		out.Fields = append(out.Fields, field)
	}
	return matchedGroups, nil
}

func matchesCapability(resource Resource, service, capability contributionv1.DefinitionRef) bool {
	return service.ID == resource.ServiceID && capability.ID == resource.CapabilityID && service.Pack.ID == resource.Domain && capability.Pack.ID == resource.Domain
}

func resourceMatchesManifest(resource Resource, manifest contributionv1.Manifest, pack contributionv1.PackRef, groups map[string]bool) bool {
	for _, field := range manifest.Fields {
		if groups[field.Group] && field.ServiceRef.Pack == pack && field.CapabilityRef.Pack == pack && matchesCapability(resource, field.ServiceRef, field.CapabilityRef) {
			return true
		}
	}
	for _, action := range manifest.Actions {
		if groups[action.Group] && action.ServiceRef.Pack == pack && action.CapabilityRef.Pack == pack && matchesCapability(resource, action.ServiceRef, action.CapabilityRef) {
			return true
		}
	}
	return false
}

func fieldMatchesManifest(field Field, resource Resource, manifest contributionv1.Manifest, pack contributionv1.PackRef, groups map[string]bool) bool {
	for _, declared := range manifest.Fields {
		if groups[declared.Group] && declared.ID == field.ID && declared.Ref.ID == field.DefinitionID && declared.UnitRef.ID == field.UnitID && declared.Ref.Pack == pack && declared.ServiceRef.Pack == pack && declared.CapabilityRef.Pack == pack && matchesCapability(resource, declared.ServiceRef, declared.CapabilityRef) {
			return true
		}
	}
	return false
}

func (c *Composer) Invoke(claim Claims) (any, error) {
	if c == nil {
		return nil, ErrStale
	}
	// Capture the clock under the lock and call it after unlocking, as
	// Catalog does.
	c.mu.RLock()
	now := c.now
	c.mu.RUnlock()
	if claim.Deadline.IsZero() || !now().Before(claim.Deadline) || claim.IdempotencyKey == "" {
		return nil, ErrStale
	}
	catalog, err := c.Catalog()
	if err != nil {
		return nil, err
	}
	if catalog.CatalogRevision != claim.CatalogRevision {
		return nil, ErrStale
	}
	var action *Action
	for i := range catalog.Actions {
		if matchesClaim(catalog.Actions[i], claim) {
			action = &catalog.Actions[i]
			break
		}
	}
	if action == nil {
		return nil, ErrUnauthorized
	}
	if !matchesDigest(catalog.Contributions, claim) {
		return nil, ErrStale
	}
	c.mu.RLock()
	invoker, revalidator := c.invoker, c.revalidator
	c.mu.RUnlock()
	if invoker == nil {
		return nil, ErrUnauthorized
	}
	if revalidator == nil {
		return nil, ErrUnauthorized
	}
	if err := revalidator.Revalidate(*action, claim); err != nil {
		return nil, err
	}
	return invoker.Invoke(*action, claim)
}

func matchesClaim(a Action, c Claims) bool {
	return a.ID == c.ActionID && a.ResourceID == c.ResourceID && a.CapabilityID == c.CapabilityID && a.ContributionDriverID == c.DriverID && a.ContributionManifestID == c.ManifestID && a.ContributionManifestVersion == c.ManifestVersion && a.Source.SnapshotID == c.SnapshotID && a.Source.Revision == c.Revision && a.Source.BindingID == c.BindingID && a.Source.SourceEpoch == c.SourceEpoch && a.Source.DriverGeneration == c.DriverGeneration && c.Digest != ""
}
func matchesDigest(items []Identity, c Claims) bool {
	for _, i := range items {
		if i.DriverID == c.DriverID && i.ManifestID == c.ManifestID && i.ManifestVersion == c.ManifestVersion && i.Digest == c.Digest {
			return true
		}
	}
	return false
}

// CanonicalJSON is public for transport and fixed wire tests.
func CanonicalJSON(c Catalog) ([]byte, error) { sortCatalog(&c); return canonical(c) }
