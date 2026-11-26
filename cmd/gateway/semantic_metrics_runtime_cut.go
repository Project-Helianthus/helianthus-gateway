package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	ebusgateway "github.com/Project-Helianthus/helianthus-gateway"
	"github.com/Project-Helianthus/helianthus-gateway/internal/semanticmetrics"
	"github.com/Project-Helianthus/helianthus-gateway/internal/semanticview"
	"github.com/Project-Helianthus/helianthus-gateway/internal/semhub"
	semreg "github.com/Project-Helianthus/helianthus-semreg/semreg/v1"
)

type semanticServedCapture interface {
	CaptureServed(context.Context, func() time.Time) (semhub.CoherentCapture, error)
}

// captureServed distinguishes an unsupported read from a failed read. Once
// supported, neither caller substitutes an explicit selection for it.
func (p *hubSemanticSnapshotProvider) captureServed(ctx context.Context) (semhub.CoherentCapture, bool, error) {
	if p == nil {
		return semhub.CoherentCapture{}, false, nil
	}
	hub, ok := p.hub.(semanticServedCapture)
	if !ok {
		return semhub.CoherentCapture{}, false, nil
	}
	capture, err := hub.CaptureServed(ctx, p.now)
	return capture, true, err
}

func metricInputs(assets []semhub.CapturedAsset) []semanticview.Input {
	inputs := make([]semanticview.Input, 0, len(assets))
	for _, asset := range assets {
		inputs = append(inputs, semanticview.Input{AssetID: asset.AssetID, Snapshot: asset.Snapshot, Evaluation: asset.Evaluation, UnavailableReason: asset.UnavailableReason})
	}
	return inputs
}

// servedDomains is every accepted publication of the capture as one domain.
// Withheld publications have no snapshot. An empty projection lets the
// renderer export every eligible value without a projection item list.
func servedDomains(assets []semhub.CapturedAsset) []ebusgateway.SemanticMetricsDomain {
	var domains []ebusgateway.SemanticMetricsDomain
	for _, asset := range assets {
		if asset.Snapshot == nil || asset.Evaluation == nil {
			continue
		}
		domains = append(domains, ebusgateway.SemanticMetricsDomain{Name: "cut", Owner: asset.Owner, Snapshot: *asset.Snapshot, Evaluation: *asset.Evaluation, Available: true})
	}
	return domains
}

// Reconcile assigns durable slots from the accepted cut and persists them
// before the next scrape reads them: a published resource takes a free slot, a
// withdrawn resource retires its slot, and a cut with no resource retires every
// slot. A cut that cannot be read whole changes nothing, because a publication
// that is only unavailable must not retire its slots. A scrape never calls it.
func (m *gatewaySemanticMetrics) Reconcile(ctx context.Context) error {
	registry, ids := m.configured()
	if registry.StoreDirectory == "" || registry.StoreError != "" {
		return nil
	}
	capture, serves, err := m.provider.captureServed(ctx)
	var inputs []semanticview.Input
	if serves {
		inputs = metricInputs(capture.Assets)
	} else {
		// A legacy declaration list configures the reconciliation selection.
		// With neither a declaration nor configured slots there is no cut to sync.
		var declared bool
		if m.provider != nil {
			if hub, ok := m.provider.hub.(interface{ AssetIDs() []semreg.AssetID }); ok {
				ids, declared = hub.AssetIDs(), true
			}
		}
		if len(ids) > 0 {
			_, inputs, err = m.provider.Capture(ctx, ids)
		} else if !declared {
			return nil
		}
	}
	if err != nil {
		return fmt.Errorf("cut capture: %w", err)
	}
	var members []semanticmetrics.CutMember
	current := map[semreg.ResourceID]bool{}
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
