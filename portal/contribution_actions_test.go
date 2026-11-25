package portal

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/Project-Helianthus/helianthus-gateway/internal/semanticview"
	"github.com/Project-Helianthus/helianthus-gateway/portal/catalogv1"
	"github.com/Project-Helianthus/helianthus-gateway/portal/contributionv1"
)

type contributionActionAdapters struct {
	revalidated bool
	reject      bool
	route       string
}

func (a *contributionActionAdapters) Revalidate(catalogv1.Action, catalogv1.Claims) error {
	a.revalidated = true
	if a.reject {
		return errors.New("fixture owner rejected the action")
	}
	return nil
}

func (a *contributionActionAdapters) Invoke(action catalogv1.Action, _ catalogv1.Claims) (json.RawMessage, error) {
	if !a.revalidated || a.reject {
		return nil, errors.New("fixture action was not revalidated")
	}
	if action.OperationID != "" && action.NativeMemberID == "" {
		a.route = action.OperationID
	} else if action.OperationID == "" && action.NativeMemberID != "" {
		a.route = action.NativeMemberID
	}
	return json.Marshal(a.route)
}

func TestContributionActionHandlerUsesSeparateSemanticAndNativeRoutes(t *testing.T) {
	m, index := m22Fixture()
	field := m.Fields[0]
	op, arg, effect := field.Ref, field.Ref, field.Ref
	op.ID, arg.ID, effect.ID = "fixture.operation", "fixture.argument", "fixture.effect"
	for _, ref := range []contributionv1.DefinitionRef{op, arg, effect} {
		index.AddDefinition(ref)
	}
	index.AddOperation(op, field.CapabilityRef, field.ServiceRef, arg, effect)
	index.AddNativeMember(m.Contributor.NativeContract, "action", "fixture.setup")
	semantic := contributionv1.Action{ID: "semantic", Group: field.Group, Label: field.Label, ServiceRef: field.ServiceRef, CapabilityRef: field.CapabilityRef, OperationRef: &op, ArgumentRef: &arg, EffectRef: &effect}
	native := contributionv1.Action{ID: "native", Group: field.Group, Label: field.Label, ServiceRef: field.ServiceRef, CapabilityRef: field.CapabilityRef, NativeMemberID: "fixture.setup", Order: 1}
	m.Actions = []contributionv1.Action{semantic, native}
	registry := contributionv1.NewRegistry(index)
	owner := contributionv1.DriverGeneration{DriverID: m.Contributor.DriverID, Generation: 1}
	if err := RegisterContributions(registry, owner, []contributionv1.Manifest{m}); err != nil {
		t.Fatal(err)
	}
	resource := catalogv1.Resource{ID: "resource:fixture", Domain: field.Ref.Pack.ID, ServiceID: field.ServiceRef.ID, CapabilityID: field.CapabilityRef.ID,
		ContributionDriverID: m.Contributor.DriverID, ContributionManifestID: m.ManifestID, ContributionManifestVersion: m.ManifestVersion,
		State: "CURRENT", Source: catalogv1.Source{AssetID: "asset:fixture", SnapshotID: "fixture.snapshot", Revision: "fixture.revision", BindingID: "fixture.binding", SourceEpoch: "fixture.epoch", DriverGeneration: 1}}
	adapter := &contributionActionAdapters{}
	options := &ContributionOptions{Registry: registry, Index: index, Invoker: adapter, Revalidator: adapter,
		Capture: func(semanticview.View) (catalogv1.Capture, error) {
			return catalogv1.Capture{Domains: m.Requires.Packs, Resources: []catalogv1.Resource{resource}, LensMembers: map[string][]catalogv1.LensMember{
				"fixture.lens": {{ResourceID: resource.ID, AssetID: resource.Source.AssetID}},
			}}, nil
		},
	}
	makeHandler := func() http.Handler {
		return NewHandler(Options{Contributions: options, SharedSemanticView: func(context.Context) (semanticview.View, error) { return semanticview.View{}, nil }})
	}
	handler := makeHandler()
	catalog := m22Catalog(t, handler)
	if len(catalog.Actions) != 2 {
		t.Fatalf("expected two distinct actions: %+v", catalog.Actions)
	}
	invoke := func(claim catalogv1.Claims) map[string]json.RawMessage {
		t.Helper()
		raw, err := json.Marshal(map[string]any{"operationName": "PortalActionInvokeV1", "variables": map[string]any{"claim": claim}})
		if err != nil {
			t.Fatal(err)
		}
		return m22Request(t, handler, "/graphql/portal/v1", string(raw))
	}
	for _, action := range catalog.Actions {
		if !action.Discoverable || !action.Enabled || action.ResourceID != resource.ID {
			t.Fatalf("action not offered on its resource: %+v", action)
		}
		claim := catalogv1.Claims{CatalogRevision: catalog.CatalogRevision, Digest: catalog.Contributions[0].Digest, DriverID: resource.ContributionDriverID, ManifestID: resource.ContributionManifestID, ManifestVersion: resource.ContributionManifestVersion,
			ActionID: action.ID, ResourceID: resource.ID, CapabilityID: resource.CapabilityID, SnapshotID: resource.Source.SnapshotID, Revision: resource.Source.Revision, BindingID: resource.Source.BindingID, SourceEpoch: resource.Source.SourceEpoch, DriverGeneration: resource.Source.DriverGeneration,
			IdempotencyKey: "fixture." + action.ID, Deadline: time.Now().Add(time.Minute)}
		adapter.revalidated, adapter.route = false, ""
		got := invoke(claim)
		want := map[string]string{"semantic": "fixture.operation", "native": "fixture.setup"}[action.ID]
		if got["errors"] != nil || adapter.route != want || !adapter.revalidated {
			t.Fatalf("route %q: adapter=%+v response=%s", want, adapter, got)
		}
		adapter.reject, adapter.route, adapter.revalidated = true, "", false
		if got := invoke(claim); got["errors"] == nil || adapter.route != "" || !adapter.revalidated {
			t.Fatalf("revalidation denial did not stop invocation: %+v", got)
		}
		adapter.reject = false
	}
	options.Revalidator = nil
	handler = makeHandler()
	for _, action := range m22Catalog(t, handler).Actions {
		if action.Enabled {
			t.Fatal("nil revalidator offered an enabled action")
		}
	}
	options.Invoker = nil
	handler = makeHandler()
	if actions := m22Catalog(t, handler).Actions; len(actions) != 0 {
		t.Fatalf("nil invoker exposed actions: %+v", actions)
	}
}
