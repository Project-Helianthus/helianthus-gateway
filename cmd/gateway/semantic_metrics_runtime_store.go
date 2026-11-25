package main

import (
	"errors"
	"io/fs"

	"github.com/Project-Helianthus/helianthus-gateway/internal/semanticmetrics"
)

// composeGatewaySemanticStore creates a missing store and reopens an existing
// one. With assign set, members are assigned and the slots of every identity
// they omit are retired before the registry is returned. The caller closes
// nothing: the file lock is released before return, and a scrape does not open
// the store again.
func composeGatewaySemanticStore(directory string, members []semanticmetrics.CutMember, assign bool) (semanticmetrics.Registry, error) {
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
	if !assign {
		// A reopened store does not carry its validation. The stored
		// assignments are attested as they are, so a restart keeps them.
		if err = store.Revalidate(gatewaySemanticMetricTarget, store.Export().Bindings); err != nil {
			return semanticmetrics.Registry{}, err
		}
		registry := store.Export()
		registry.StoreDirectory = directory
		return registry, nil
	}
	if _, err = store.Sync(gatewaySemanticMetricTarget, members); err != nil {
		return semanticmetrics.Registry{}, err
	}
	if err = store.Revalidate(gatewaySemanticMetricTarget, store.Export().Bindings); err != nil {
		return semanticmetrics.Registry{}, err
	}
	registry := store.Export()
	registry.StoreDirectory = directory
	return registry, nil
}
