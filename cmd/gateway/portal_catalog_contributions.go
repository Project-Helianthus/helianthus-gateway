package main

import (
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/Project-Helianthus/helianthus-gateway/portal/catalogv1"
	"github.com/Project-Helianthus/helianthus-gateway/portal/contributionv1"
	"github.com/Project-Helianthus/helianthus-semreg/semreg/runtime/tree"
)

// gatewayPortalCatalogContributions is the long-lived Gateway-owned admission
// boundary for complete driver descriptor generations. It has no source or
// transport access: drivers publish already-detached manifests here as part of
// their lifecycle, and each catalog request snapshots this registry.
type gatewayPortalCatalogContributions struct {
	index       *gatewayPortalSemanticIndex
	registry    *contributionv1.Registry
	admissionMu sync.RWMutex
	invoker     catalogv1.Invoker
	revalidator catalogv1.Revalidator
}

func newGatewayPortalCatalogContributions() (*gatewayPortalCatalogContributions, error) {
	index, err := newGatewayPortalSemanticIndex()
	if err != nil {
		return nil, err
	}
	return &gatewayPortalCatalogContributions{index: index, registry: contributionv1.NewRegistry(index)}, nil
}

// PublishGeneration is the driver-start/replacement admission path. The
// contribution registry validates, canonicalizes, deep-detaches, generation
// fences, and quarantines before this method returns.
func (c *gatewayPortalCatalogContributions) PublishGeneration(owner contributionv1.DriverGeneration, manifests []contributionv1.Manifest) error {
	if c == nil || c.registry == nil {
		return fmt.Errorf("portal contribution registry unavailable")
	}
	return c.registry.ReplaceGeneration(owner, manifests)
}

// WithdrawGeneration is the matching driver-stop path. A stale stop cannot
// remove a successor because Registry checks the exact driver generation.
func (c *gatewayPortalCatalogContributions) WithdrawGeneration(owner contributionv1.DriverGeneration) bool {
	return c != nil && c.registry != nil && c.registry.WithdrawGeneration(owner)
}

func (c *gatewayPortalCatalogContributions) snapshot() contributionv1.RegistrySnapshot {
	if c == nil || c.registry == nil {
		return contributionv1.RegistrySnapshot{}
	}
	return c.registry.Snapshot()
}

func (c *gatewayPortalCatalogContributions) revision() uint64 { return c.snapshot().Revision }

// bindHostTree supplies the namespace registry the semantic index reads.
func (c *gatewayPortalCatalogContributions) bindHostTree(graph *tree.Tree) {
	if c == nil || c.index == nil {
		return
	}
	c.index.bindHostTree(graph)
}

// bindActionAdmission installs the host invoker and revalidator both catalog
// composers read. A nil pair leaves actions undiscoverable.
func (c *gatewayPortalCatalogContributions) bindActionAdmission(invoker catalogv1.Invoker, revalidator catalogv1.Revalidator) {
	if c == nil {
		return
	}
	c.admissionMu.Lock()
	c.invoker, c.revalidator = invoker, revalidator
	c.admissionMu.Unlock()
}

func (c *gatewayPortalCatalogContributions) actionAdmission() (catalogv1.Invoker, catalogv1.Revalidator) {
	if c == nil {
		return nil, nil
	}
	c.admissionMu.RLock()
	defer c.admissionMu.RUnlock()
	return c.invoker, c.revalidator
}

func (c *gatewayPortalCatalogContributions) accepts(driver, manifest string) bool {
	for _, descriptor := range c.snapshot().Accepted {
		if descriptor.Key.DriverID == driver && descriptor.Key.ManifestID == manifest {
			return true
		}
	}
	return false
}

func (c *gatewayPortalCatalogContributions) composer(source catalogv1.SourceCapture) (*catalogv1.Composer, error) {
	if c == nil || c.index == nil || c.registry == nil {
		return nil, fmt.Errorf("portal contribution registry unavailable")
	}
	snapshot := c.snapshot()
	// The wrapper binds the registry revision across source capture. A provider
	// replacement or withdrawal cannot publish an old descriptor snapshot beside
	// newly captured source rows.
	boundSource := &gatewayPortalCatalogBoundCapture{source: source, contributions: c, revision: snapshot.Revision}
	invoker, revalidator := c.actionAdmission()
	composer := catalogv1.New(c.index, boundSource, invoker)
	composer.SetRevalidator(revalidator)
	if err := composer.InstallDetachedRegistrySnapshot(snapshot.Accepted, snapshot.Quarantined); err != nil {
		return nil, err
	}
	return composer, nil
}

type gatewayPortalCatalogBoundCapture struct {
	source        catalogv1.SourceCapture
	contributions *gatewayPortalCatalogContributions
	revision      uint64
}

func (c *gatewayPortalCatalogBoundCapture) Capture(at time.Time) (catalogv1.Capture, error) {
	if c == nil || c.source == nil || c.contributions == nil || c.contributions.revision() != c.revision {
		return catalogv1.Capture{}, catalogv1.ErrCatalogChanged
	}
	captured, err := c.source.Capture(at)
	if err != nil {
		return catalogv1.Capture{}, err
	}
	if c.contributions.revision() != c.revision {
		return catalogv1.Capture{}, catalogv1.ErrCatalogChanged
	}
	return captured, nil
}

func (c *gatewayPortalCatalogBoundCapture) Fence() string {
	return fmt.Sprintf("contributions:%d", c.revision)
}

// gatewayPortalContributionLifecycle binds an admitted generation to the
// owning runtime's shutdown sequence. It intentionally exposes only the exact
// publish/withdraw operations needed by a driver lifecycle.
type gatewayPortalContributionLifecycle struct {
	registry *gatewayPortalCatalogContributions
	owner    contributionv1.DriverGeneration
}

func startGatewayPortalContributionLifecycle(registry *gatewayPortalCatalogContributions, owner contributionv1.DriverGeneration, manifests []contributionv1.Manifest) (*gatewayPortalContributionLifecycle, error) {
	if err := registry.PublishGeneration(owner, manifests); err != nil {
		if !errors.Is(err, contributionv1.ErrLensReference) {
			return nil, err
		}
		// Missing catalog presentation must not stop the driver. Publish an
		// empty set so the generation still fences out any predecessor's views.
		if err := registry.PublishGeneration(owner, nil); err != nil {
			return nil, err
		}
		log.Printf("portal contribution generation withheld: %v", err)
	}
	return &gatewayPortalContributionLifecycle{registry: registry, owner: owner}, nil
}

func (l *gatewayPortalContributionLifecycle) Stop() {
	if l != nil {
		l.registry.WithdrawGeneration(l.owner)
	}
}
