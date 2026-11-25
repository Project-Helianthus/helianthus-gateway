package catalogv1

import (
	"sort"

	"github.com/Project-Helianthus/helianthus-gateway/portal/contributionv1"
	"github.com/Project-Helianthus/helianthus-semreg/semreg/v1/graph"
)

const PortalTarget = "target:portal"

// TargetReport is Portal's presentation accounting (PTL-4). Its descriptors
// are not lens resources, relations, capabilities or facts.
type TargetReport struct {
	TargetID    string               `json:"target_id"`
	Descriptors []TargetPresentation `json:"descriptors"`
}

type TargetPresentation struct {
	Pack        contributionv1.PackRef `json:"pack"`
	ID          string                 `json:"id"`
	Version     string                 `json:"version"`
	Disposition graph.Disposition      `json:"disposition"`
	Reason      string                 `json:"reason"`
}

func ReadTargetReport(entries []graph.Element) TargetReport {
	report := TargetReport{TargetID: PortalTarget, Descriptors: []TargetPresentation{}}
	for _, entry := range entries {
		ref, packOwned := entry.Ref.DefinitionRef()
		if !packOwned || entry.ElementKind != graph.KindPresentation || entry.Presentation == nil || entry.Presentation.Target != graph.TargetPortal ||
			entry.Disposition != graph.DispositionUnsupported || entry.Reason != "catalog.reason.target_presentation" {
			continue
		}
		report.Descriptors = append(report.Descriptors, TargetPresentation{
			Pack: contributionv1.PackRef{ID: string(ref.Pack.ID), Version: string(ref.Pack.Version)},
			ID:   string(ref.ID), Version: string(ref.Version), Disposition: entry.Disposition, Reason: string(entry.Reason),
		})
	}
	sort.Slice(report.Descriptors, func(i, j int) bool {
		a, b := report.Descriptors[i], report.Descriptors[j]
		return less(a.Pack.ID, a.Pack.Version, a.ID, a.Version, b.Pack.ID, b.Pack.Version, b.ID, b.Version)
	})
	return report
}
