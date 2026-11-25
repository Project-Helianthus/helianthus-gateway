package main

import (
	"errors"
	"testing"

	"github.com/Project-Helianthus/helianthus-gateway/portal/contributionv1"
)

func TestPortalContributionLifecycleWithholdsGenerationWithoutCatalogLens(t *testing.T) {
	for _, lens := range []string{"lens.missing"} {
		t.Run("lens="+lens, func(t *testing.T) {
			registry := newPortalFixtureContributions(t)
			valid := portalFixtureManifest(gatewayPortalPVDescriptor())
			previous := contributionv1.DriverGeneration{DriverID: valid.Contributor.DriverID, Generation: 1}
			successor := contributionv1.DriverGeneration{DriverID: previous.DriverID, Generation: 2}
			other := portalFixtureManifest(gatewayPortalPVDescriptor())
			other.Contributor.DriverID = "fixture.sibling"
			otherOwner := contributionv1.DriverGeneration{DriverID: other.Contributor.DriverID, Generation: successor.Generation}
			if err := registry.PublishGeneration(otherOwner, []contributionv1.Manifest{other}); err != nil {
				t.Fatal(err)
			}
			old, err := startGatewayPortalContributionLifecycle(registry, previous, []contributionv1.Manifest{valid})
			if err != nil {
				t.Fatal(err)
			}

			bad := portalFixtureManifest(gatewayPortalPVDescriptor())
			bad.Views[0].Lens = lens
			alsoValid := valid
			alsoValid.ManifestID += ".extra"
			offered := []contributionv1.Manifest{alsoValid, bad}
			revision := registry.revision()
			if err := registry.PublishGeneration(successor, offered); !errors.Is(err, contributionv1.ErrLensReference) {
				t.Fatalf("direct publication error = %v, want invalid lens", err)
			}
			if !registry.accepts(previous.DriverID, valid.ManifestID) || registry.revision() != revision {
				t.Fatal("rejected publication changed the active generation")
			}

			lease, err := startGatewayPortalContributionLifecycle(registry, successor, offered)
			if err != nil || lease == nil {
				t.Fatalf("driver lifecycle should continue with no admitted presentation: lease=%v, err=%v", lease, err)
			}
			snapshot := registry.snapshot()
			if len(snapshot.Accepted) != 1 || snapshot.Accepted[0].Key.DriverID != otherOwner.DriverID || snapshot.Accepted[0].Generation != otherOwner.Generation || len(snapshot.Quarantined) != 0 {
				t.Fatalf("only the sibling should be admitted, got %+v", snapshot)
			}
			if err := registry.PublishGeneration(previous, []contributionv1.Manifest{valid}); err == nil {
				t.Fatal("withheld successor did not fence the predecessor")
			}
			old.Stop()
			if registry.revision() != snapshot.Revision {
				t.Fatal("stale stop withdrew the withheld successor")
			}
			lease.Stop()
			if registry.revision() == snapshot.Revision || !registry.accepts(otherOwner.DriverID, other.ManifestID) {
				t.Fatal("matching stop did not withdraw only the withheld successor")
			}
			if err := registry.PublishGeneration(successor, nil); err == nil {
				t.Fatal("retired generation was reopened")
			}
		})
	}
}

func TestPortalContributionLifecycleStillPropagatesOtherAdmissionFailures(t *testing.T) {
	registry := newPortalFixtureContributions(t)
	manifest := portalFixtureManifest(gatewayPortalPVDescriptor())
	owner := contributionv1.DriverGeneration{DriverID: manifest.Contributor.DriverID, Generation: 1}
	manifest.Views[0].Renderer = "invalid"
	revision := registry.revision()
	lease, err := startGatewayPortalContributionLifecycle(registry, owner, []contributionv1.Manifest{manifest})
	if err == nil || errors.Is(err, contributionv1.ErrLensReference) || lease != nil || registry.revision() != revision {
		t.Fatalf("expected publication failure without a lease or registry change: lease=%v err=%v", lease, err)
	}
}
