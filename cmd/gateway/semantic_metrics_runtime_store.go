package main

import (
	"errors"
	"io/fs"

	"github.com/Project-Helianthus/helianthus-gateway/internal/semanticmetrics"
)

// composeGatewaySemanticStore creates a missing store and reopens an existing
// one. members, when set, are assigned before the registry is returned. The
// caller closes nothing: the file lock is released before return, and a scrape
// does not open the store again.
func composeGatewaySemanticStore(directory string, members []semanticmetrics.CutMember) (semanticmetrics.Registry, error) {
	persistence, err := semanticmetrics.NewSlotFileStore(directory, gatewaySemanticMetricTarget)
	if err != nil {
		return semanticmetrics.Registry{}, err
	}
	defer func() { _ = persistence.Close() }()
	var store *semanticmetrics.O21SlotStore
	if _, err = persistence.Load(gatewaySemanticMetricTarget); errors.Is(err, fs.ErrNotExist) {
		store, err = semanticmetrics.O21Create(persistence, gatewaySemanticMetricTarget)
	} else if err != nil {
		return semanticmetrics.Registry{}, err
	} else {
		store, err = semanticmetrics.O21Open(persistence, gatewaySemanticMetricTarget)
	}
	if err != nil {
		return semanticmetrics.Registry{}, err
	}
	if len(members) == 0 {
		return store.Export(), nil
	}
	registry, err := store.Sync(gatewaySemanticMetricTarget, members)
	if err != nil {
		return semanticmetrics.Registry{}, err
	}
	if err = store.Revalidate(gatewaySemanticMetricTarget, registry.Bindings); err != nil {
		return semanticmetrics.Registry{}, err
	}
	return store.Export(), nil
}
