package main

import (
	"context"
	"errors"
	"fmt"
	"sort"

	ebusgateway "github.com/Project-Helianthus/helianthus-gateway"
	"github.com/Project-Helianthus/helianthus-gateway/internal/semanticmetrics"
	"github.com/Project-Helianthus/helianthus-gateway/internal/semanticview"
	semreg "github.com/Project-Helianthus/helianthus-semreg/semreg/v1"
)

// semanticCutDeclarer is the capability of a captured hub that names the
// assets of the served cut. A capture source without it serves no cut: the
// caller attaches its own domains.
type semanticCutDeclarer interface {
	AssetIDs() []semreg.AssetID
}

// servedAssets is the declared asset set of the served cut and whether the
// capture source serves one.
func (p *hubSemanticSnapshotProvider) servedAssets() ([]semreg.AssetID, bool) {
	if p == nil || p.hub == nil {
		return nil, false
	}
	declarer, ok := p.hub.(semanticCutDeclarer)
	if !ok {
		return nil, false
	}
	return declarer.AssetIDs(), true
}

// captureIDs is the one selection of a scrape: the configured assets and every
// asset of the served cut, in bytewise order, read by one capture.
func (p *hubSemanticSnapshotProvider) captureIDs(configured []semreg.AssetID) []semreg.AssetID {
	served, ok := p.servedAssets()
	if !ok {
		return configured
	}
	seen := make(map[semreg.AssetID]bool, len(configured)+len(served))
	ids := make([]semreg.AssetID, 0, len(configured)+len(served))
	for _, id := range append(append([]semreg.AssetID(nil), configured...), served...) {
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// servedDomains is every accepted publication of the capture as one domain. A
// publication the capture withheld has no snapshot and is not exported. The
// projection stays empty: every eligible value is exported without an item
// list or a domain list.
func (p *hubSemanticSnapshotProvider) servedDomains(inputs []semanticview.Input) []ebusgateway.SemanticMetricsDomain {
	if _, ok := p.servedAssets(); !ok {
		return nil
	}
	var domains []ebusgateway.SemanticMetricsDomain
	for _, input := range inputs {
		if input.Snapshot == nil || input.Evaluation == nil {
			continue
		}
		domains = append(domains, ebusgateway.SemanticMetricsDomain{Name: "cut", Snapshot: *input.Snapshot, Evaluation: *input.Evaluation, Available: true})
	}
	return domains
}

// Reconcile assigns durable slots from the accepted cut and persists them
// before the next scrape reads them: a published resource takes a free slot, a
// withdrawn resource retires its slot, and a cut with no resource retires every
// slot. A cut that cannot be read whole changes nothing, because a publication
// that is only unavailable must not retire its slots. A scrape never calls it.
func (m *gatewaySemanticMetrics) Reconcile(ctx context.Context) error {
	registry, _ := m.configured()
	if registry.StoreDirectory == "" || registry.StoreError != "" {
		return nil
	}
	ids, serves := m.provider.servedAssets()
	if !serves {
		return nil
	}
	var members []semanticmetrics.CutMember
	current := map[semreg.ResourceID]bool{}
	if len(ids) > 0 {
		_, inputs, err := m.provider.Capture(ctx, ids)
		if err != nil {
			return fmt.Errorf("cut capture: %w", err)
		}
		for _, input := range inputs {
			if input.Snapshot == nil {
				return fmt.Errorf("cut is not whole: asset %s is unavailable", input.AssetID)
			}
			members = append(members, cutMembersOf(*input.Snapshot)...)
			for _, resource := range input.Snapshot.Resources {
				if resource.State == semreg.ResourceCurrent {
					current[resource.ResourceID] = true
				}
			}
		}
	}
	// A missing binding can remove the fact along with its provenance. The
	// explicit current resource still holds its existing fact identities.
	for _, binding := range registry.Bindings {
		if !binding.Retired && current[binding.ResourceID] && len(binding.Fields) == 1 {
			members = append(members, semanticmetrics.CutMember{
				Domain: binding.Domain, ResourceID: binding.ResourceID, FactID: binding.Fields[0],
				Dimensions: binding.Dimensions, Current: true, Held: true,
			})
		}
	}
	next, err := composeGatewaySemanticStore(registry.StoreDirectory, members, true)
	if err != nil {
		m.logf("semantic metric store: %s", err.Error())
		m.adopt(semanticmetrics.Registry{StoreError: err.Error(), StoreDirectory: registry.StoreDirectory})
		return err
	}
	if _, err = semanticmetrics.Render(semanticview.View{}, next); err != nil {
		return errors.Join(fmt.Errorf("semantic metric registry"), err)
	}
	m.adopt(next)
	return nil
}

// cutMembersOf lists the facts of one publication that the slot renderer
// declares, keyed by the resource the qualifier names. Without provenance,
// only an explicitly current resource can hold a member, and a held member
// cannot create a slot. A noncurrent resource is not listed, so its slot retires.
func cutMembersOf(snapshot semreg.Snapshot) []semanticmetrics.CutMember {
	current := map[semreg.ResourceID]bool{}
	for _, resource := range snapshot.Resources {
		current[resource.ResourceID] = resource.State == semreg.ResourceCurrent
	}
	var members []semanticmetrics.CutMember
	for _, envelope := range snapshot.Facts {
		domain, ok := semanticmetrics.MetricDomain(envelope.Key.FactID)
		if !ok || len(envelope.Key.Dimensions) != 1 {
			continue
		}
		text := envelope.Key.Dimensions[0].Value.Text
		if envelope.Key.Dimensions[0].Value.Kind != semreg.ValueText || text == nil || *text == "" {
			continue
		}
		resource := semreg.ResourceID(*text)
		if len(snapshot.Resources) > 0 && !current[resource] {
			continue
		}
		member, ok := cutMemberProvenance(snapshot, envelope, domain)
		if !ok && !current[resource] {
			continue
		}
		member.Domain, member.Held = domain, !ok
		member.ResourceID, member.FactID, member.Dimensions, member.Current = resource, envelope.Key.FactID, envelope.Key.Dimensions, true
		members = append(members, member)
	}
	return members
}

func cutMemberProvenance(snapshot semreg.Snapshot, envelope semreg.FactEnvelope, domain string) (semanticmetrics.CutMember, bool) {
	for _, candidate := range envelope.Candidates {
		if candidate.BindingID == nil || candidate.SourceEpochID == nil || candidate.DriverGeneration == nil {
			continue
		}
		member := semanticmetrics.CutMember{Domain: domain, BindingID: *candidate.BindingID, SourceEpochID: *candidate.SourceEpochID, DriverGeneration: *candidate.DriverGeneration}
		found := false
		for _, binding := range snapshot.Bindings {
			if binding.BindingID == member.BindingID && binding.State == semreg.BindingCurrent {
				member.AssetID, member.SourceID, found = binding.AssetID, binding.SourceID, true
				break
			}
		}
		if !found {
			continue
		}
		for _, service := range snapshot.Services {
			if service.BindingID == member.BindingID && service.SourceEpochID == member.SourceEpochID && string(service.Definition.Pack.ID) == "helianthus.pack."+domain {
				member.ServiceInstance = service.InstanceID
				return member, true
			}
		}
	}
	return semanticmetrics.CutMember{}, false
}
