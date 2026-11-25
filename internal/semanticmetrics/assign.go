package semanticmetrics

import (
	"encoding/json"
	"fmt"
	"sort"

	semreg "github.com/Project-Helianthus/helianthus-semreg/semreg/v1"
)

// CutMember is one fact of one current or withdrawn resource of the served cut.
// Provenance fields are recorded on a new assignment and are not its identity.
// Current false retires the slot of that resource, fact and qualifiers.
type CutMember struct {
	ResourceID       semreg.ResourceID
	FactID           semreg.DefinitionID
	Dimensions       []semreg.Dimension
	Domain           string
	AssetID          semreg.AssetID
	SourceID         semreg.SourceID
	SourceEpochID    semreg.SourceEpochID
	BindingID        semreg.NativeBindingID
	DriverGeneration semreg.Uint64
	ServiceInstance  semreg.ServiceInstanceID
	Current          bool
}

// SyncAssignments assigns one slot per current member that does not already
// have one, in resource, fact, qualifier order, and retires a slot whose
// member is no longer current. An existing assignment is kept, including
// across a new source epoch. A retired identity stays reserved. No assignment
// is created for a member that omits its resource, fact or provenance.
func SyncAssignments(existing []Binding, members []CutMember) ([]Binding, error) {
	next := append([]Binding(nil), existing...)
	sorted := append([]CutMember(nil), members...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].ResourceID != sorted[j].ResourceID {
			return sorted[i].ResourceID < sorted[j].ResourceID
		}
		if sorted[i].FactID != sorted[j].FactID {
			return sorted[i].FactID < sorted[j].FactID
		}
		return qualifierText(sorted[i].Dimensions) < qualifierText(sorted[j].Dimensions)
	})
	wanted := map[string]bool{}
	for _, member := range sorted {
		if !member.Current {
			continue
		}
		binding := Binding{
			Domain: member.Domain, ResourceID: member.ResourceID, AssetID: member.AssetID,
			SourceID: member.SourceID, SourceEpochID: member.SourceEpochID, BindingID: member.BindingID,
			DriverGeneration: member.DriverGeneration, ServiceInstance: member.ServiceInstance,
			Dimensions: append([]semreg.Dimension(nil), member.Dimensions...), Fields: []semreg.DefinitionID{member.FactID},
		}
		key, err := identityKey(binding)
		if err != nil {
			return nil, err
		}
		if binding.ResourceID.Validate() != nil || member.FactID.Validate() != nil || len(binding.Dimensions) != 1 || binding.AssetID.Validate() != nil || binding.SourceID.Validate() != nil || binding.SourceEpochID.Validate() != nil || binding.BindingID.Validate() != nil || binding.DriverGeneration.Validate() != nil || binding.ServiceInstance.Validate() != nil {
			return nil, fmt.Errorf("cut member missing resource, fact or provenance")
		}
		wanted[key] = true
		if bindingIndex(next, key) >= 0 {
			continue
		}
		slot := freeSlot(next, member.Domain)
		binding.Slot = slot
		next = append(next, binding)
	}
	for i, binding := range next {
		if binding.Retired {
			continue
		}
		key, err := identityKey(binding)
		if err != nil {
			return nil, err
		}
		if !wanted[key] {
			next[i].Retired = true
		}
	}
	return next, nil
}

func identityKey(binding Binding) (string, error) {
	raw, err := json.Marshal(bindingIdentity(binding))
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func bindingIndex(bindings []Binding, key string) int {
	for i, binding := range bindings {
		got, err := identityKey(binding)
		if err == nil && got == key {
			return i
		}
	}
	return -1
}

func freeSlot(bindings []Binding, domain string) int {
	used := map[int]bool{}
	for _, binding := range bindings {
		if binding.Domain == domain {
			used[binding.Slot] = true
		}
	}
	for slot := 0; ; slot++ {
		if !used[slot] {
			return slot
		}
	}
}

func qualifierText(dimensions []semreg.Dimension) string {
	raw, _ := json.Marshal(dimensions)
	return string(raw)
}

// Sync persists the assignment diff. A scrape does not call it. Retire uses
// the stored binding, so a retired slot stays reserved for that series.
func (s *O21SlotStore) Sync(target string, members []CutMember) (Registry, error) {
	current := s.Export().Bindings
	next, err := SyncAssignments(current, members)
	if err != nil {
		return Registry{}, err
	}
	indexed := map[string]Binding{}
	for _, binding := range next {
		indexed[fmt.Sprintf("%s/%d", binding.Domain, binding.Slot)] = binding
	}
	for _, binding := range current {
		if binding.Retired {
			continue
		}
		got, ok := indexed[fmt.Sprintf("%s/%d", binding.Domain, binding.Slot)]
		if !ok || got.Retired {
			if _, err = s.Retire(target, binding); err != nil {
				return Registry{}, err
			}
		}
	}
	have := map[string]bool{}
	for _, binding := range s.Export().Bindings {
		have[fmt.Sprintf("%s/%d", binding.Domain, binding.Slot)] = true
	}
	for _, binding := range next {
		key := fmt.Sprintf("%s/%d", binding.Domain, binding.Slot)
		if !have[key] {
			if _, err = s.Assign(target, binding); err != nil {
				return Registry{}, err
			}
		}
	}
	return s.Export(), nil
}
