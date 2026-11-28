package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/Project-Helianthus/helianthus-gateway/internal/modbusadapter"
	"github.com/Project-Helianthus/helianthus-gateway/internal/semhub"
	"github.com/Project-Helianthus/helianthus-gateway/portal/catalogv1"
	"github.com/Project-Helianthus/helianthus-gateway/portal/contributionv1"
	"github.com/Project-Helianthus/helianthus-gateway/portalgraphql"
	"github.com/Project-Helianthus/helianthus-semreg/semreg/runtime/life"
	"github.com/Project-Helianthus/helianthus-semreg/semreg/runtime/tree"
	semreg "github.com/Project-Helianthus/helianthus-semreg/semreg/v1"
	"github.com/Project-Helianthus/helianthus-semreg/semreg/v1/driver"
	"github.com/Project-Helianthus/helianthus-semreg/semreg/v1/graph"
	"github.com/Project-Helianthus/helianthus-semreg/semreg/v1/packs"
)

type gatewayPortalCatalogContributionOwner interface {
	PortalCatalogContributions() *gatewayPortalCatalogContributions
}

// newPortalCatalogV1Handler captures the long-lived Gateway contribution
// registry on every request. Gateway composition may publish only accepted
// descriptors and detached sources; no legacy/public API is consulted as a
// fallback.
func newPortalCatalogV1Handler(source catalogv1.SourceCapture) http.Handler {
	owner, ok := source.(gatewayPortalCatalogContributionOwner)
	if !ok || owner.PortalCatalogContributions() == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "portal contribution registry unavailable", http.StatusServiceUnavailable)
		})
	}
	contributions := owner.PortalCatalogContributions()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		composer, err := contributions.composer(source)
		if err != nil {
			http.Error(w, "portal descriptor registry unavailable", http.StatusServiceUnavailable)
			return
		}
		portalgraphql.Handler{Catalog: composer, TargetCatalog: contributions.index.catalogElements}.ServeHTTP(w, r)
	})
}

// gatewayPortalSemanticIndex is deliberately thin: SemReg owns every semantic
// relation, while Gateway keeps the native-contract membership boundary.
type gatewayPortalSemanticIndex struct {
	metadata *semreg.PackMetadataRegistry
	catalog  *graph.Catalog
	hostMu   sync.RWMutex
	host     *tree.Tree
}

func newGatewayPortalSemanticIndex() (*gatewayPortalSemanticIndex, error) {
	r, err := packs.NewMetadataRegistry()
	if err != nil {
		return nil, err
	}
	catalog, err := graph.Embedded()
	if err != nil {
		return nil, err
	}
	return &gatewayPortalSemanticIndex{metadata: r, catalog: catalog}, nil
}

func (i *gatewayPortalSemanticIndex) bindHostTree(graphTree *tree.Tree) {
	if i == nil {
		return
	}
	i.hostMu.Lock()
	i.host = graphTree
	i.hostMu.Unlock()
}

func (i *gatewayPortalSemanticIndex) hostTree() *tree.Tree {
	if i == nil {
		return nil
	}
	i.hostMu.RLock()
	defer i.hostMu.RUnlock()
	return i.host
}

// catalogElements is the element catalog the Portal target report reads.
// Presentation rows stay in that report and are not graph content.
func (i *gatewayPortalSemanticIndex) catalogElements(context.Context) ([]graph.Element, error) {
	if i == nil || i.catalog == nil {
		return nil, fmt.Errorf("portal target catalog unavailable")
	}
	return i.catalog.Elements(), nil
}

func (i *gatewayPortalSemanticIndex) HasLens(id string) bool {
	if i == nil || i.catalog == nil {
		return false
	}
	for _, lens := range i.catalog.Lenses() {
		if string(lens.ID) == id {
			return true
		}
	}
	return false
}

func semanticPack(p contributionv1.PackRef) semreg.PackRef {
	return semreg.PackRef{ID: semreg.DefinitionID(p.ID), Version: semreg.SemanticVersion(p.Version)}
}
func semanticRef(r contributionv1.DefinitionRef) semreg.DefinitionRef {
	return semreg.DefinitionRef{Pack: semanticPack(r.Pack), ID: semreg.DefinitionID(r.ID), Version: semreg.SemanticVersion(r.Version)}
}
func (i *gatewayPortalSemanticIndex) HasPack(p contributionv1.PackRef) bool {
	if i == nil || i.metadata == nil {
		return false
	}
	for _, got := range i.metadata.Packs() {
		if got == semanticPack(p) {
			return true
		}
	}
	return false
}
func (i *gatewayPortalSemanticIndex) HasDefinition(r contributionv1.DefinitionRef) bool {
	return i != nil && i.metadata != nil && i.metadata.HasDefinition(semanticRef(r))
}
func (i *gatewayPortalSemanticIndex) CanonicalUnit(r contributionv1.DefinitionRef) (contributionv1.DefinitionRef, bool) {
	if i == nil || i.metadata == nil {
		return contributionv1.DefinitionRef{}, false
	}
	u, ok := i.metadata.CanonicalUnit(semanticRef(r))
	return contributionv1.DefinitionRef{Pack: contributionv1.PackRef{ID: string(u.Pack.ID), Version: string(u.Pack.Version)}, ID: string(u.ID), Version: string(u.Version)}, ok
}
func (i *gatewayPortalSemanticIndex) ServiceOwnsCapability(s, c contributionv1.DefinitionRef) bool {
	return i != nil && i.metadata != nil && i.metadata.ServiceOwnsCapability(semanticRef(s), semanticRef(c))
}
func (i *gatewayPortalSemanticIndex) FieldMatches(f, s, c contributionv1.DefinitionRef) bool {
	return i != nil && i.metadata != nil && i.metadata.FieldMatches(semanticRef(f), semanticRef(s), semanticRef(c))
}
func (i *gatewayPortalSemanticIndex) OperationMatches(o, c, s, a, e contributionv1.DefinitionRef) bool {
	return i != nil && i.metadata != nil && i.metadata.OperationMatches(semanticRef(o), semanticRef(c), semanticRef(s), semanticRef(a), semanticRef(e))
}

// HasNativeMember admits the Huawei closed status members and a live member of
// the namespace registry. The registry descriptor does not carry the native
// contract version, so version is not a predicate. A member matches when its
// local id and kind match and either the namespace owner or the published
// schema equals the contract.
func (i *gatewayPortalSemanticIndex) HasNativeMember(contract contributionv1.NativeContractRef, kind, member string) bool {
	if huaweiPortalNativeMember(contract, kind, member) {
		return true
	}
	return i.namespaceHasMember(contract, kind, member)
}

func (i *gatewayPortalSemanticIndex) namespaceHasMember(contract contributionv1.NativeContractRef, kind, member string) bool {
	if i == nil || member == "" {
		return false
	}
	graphTree := i.hostTree()
	if graphTree == nil || graphTree.Registry == nil {
		return false
	}
	var want tree.ChangeKind
	switch kind {
	case "field", "diagnostic":
		want = tree.ChangeMember
	case "action":
		want = tree.ChangeMethod
	default:
		return false
	}
	changes, ok := graphTree.Registry.Since(0)
	if !ok {
		return false
	}
	ctx := context.Background()
	cut, err := graphTree.Capture(ctx, tree.CutOptions{})
	if err != nil {
		return false
	}
	defer func() { _ = cut.Close() }()
	owners := map[driver.NamespaceID]driver.DriverKind{}
	for _, entry := range cut.Roster() {
		owners[entry.Key] = entry.Owner
	}
	for _, change := range changes {
		if change.Kind != want {
			continue
		}
		descriptor, err := cut.Describe(ctx, change.Ref)
		if err != nil || descriptor.Withdrawn || descriptor.Local != member {
			continue
		}
		if owners[descriptor.Namespace] == driver.DriverKind(contract.Owner) || string(descriptor.Schema) == contract.Contract || string(descriptor.Contract) == contract.Contract {
			return true
		}
	}
	return false
}

func gatewayPortalPVDescriptor() contributionv1.Manifest {
	return newGatewayPortalDescriptor("pv.primary", "portal.pv", "inverter", "pv.ac.frequency", "pv.service.inverter", "pv.capability.read.inverter", "unit.hertz", catalogv1.FivePacks[1])
}

func gatewayPortalStorageDescriptor() contributionv1.Manifest {
	return newGatewayPortalDescriptor("storage.primary", "portal.storage", "pack", "storage.state.soc", "storage.service.pack", "storage.capability.read.pack", "unit.ratio", catalogv1.FivePacks[2])
}

func gatewayPortalEVSEDescriptor() contributionv1.Manifest {
	return newGatewayPortalDescriptor("evse.primary", "portal.evse", "evse", "evse.limit.configured_current", "evse.service.evse", "evse.capability.read.evse", "unit.ampere", catalogv1.FivePacks[3])
}

func newGatewayPortalDescriptor(driver, id, group, field, service, capability, unit string, pack contributionv1.PackRef) contributionv1.Manifest {
	ref := func(id string) contributionv1.DefinitionRef {
		return contributionv1.DefinitionRef{Pack: pack, ID: id, Version: pack.Version}
	}
	return contributionv1.Manifest{Contract: contributionv1.Contract, ManifestID: id, ManifestVersion: "1.0.0", Contributor: contributionv1.Contributor{DriverID: driver, NativeContract: contributionv1.NativeContractRef{Owner: "helianthus", Contract: "portal-catalog", Version: "1.0.0"}}, Requires: contributionv1.Requirements{SemanticKernel: contributionv1.SemanticKernel, Packs: []contributionv1.PackRef{pack}}, Groups: []contributionv1.Group{{ID: group, Label: contributionv1.Label{Key: id + ".group", Default: group}, ResourceContext: group}}, Fields: []contributionv1.Field{{ID: field, Group: group, Label: contributionv1.Label{Key: id + ".field", Default: field}, Ref: ref(field), ServiceRef: ref(service), CapabilityRef: ref(capability), UnitRef: ref(unit)}}, Views: []contributionv1.View{{ID: id + ".summary", Group: group, Label: contributionv1.Label{Key: id + ".summary", Default: "Summary"}, Renderer: "summary", Slot: "lens", FieldIDs: []string{field}, DiagnosticIDs: []string{}}}, Actions: []contributionv1.Action{}, Diagnostics: []contributionv1.Diagnostic{}}
}

type gatewayPortalCatalogSource struct {
	managed       *managedSidecarBindings
	pv            *modbusadapter.Adapter
	storage       *growattStoragePublication
	evse          *teslaHSCRetainedOwner
	contributions *gatewayPortalCatalogContributions
	// drivers resolves the driver of a binding's source. Without it a
	// resource is kept only while its asset holds one current source.
	drivers gatewayPortalSourceDrivers
	// served reads catalog rows from the host cut, keyed by resource id.
	// An unbound reader leaves the concrete owners unchanged.
	servedMu sync.Mutex
	served   func(context.Context) ([]catalogv1.Resource, error)
	// dropped is the last logged set of dropped-resource diagnostics, so an
	// unchanged set is logged once and not on every capture.
	droppedMu sync.Mutex
	dropped   string
}

func newGatewayPortalCatalogSource(pv *modbusadapter.Adapter, storage *growattStoragePublication, evse *teslaHSCRetainedOwner, contributions *gatewayPortalCatalogContributions, managed ...*managedSidecarBindings) catalogv1.SourceCapture {
	source := &gatewayPortalCatalogSource{pv: pv, storage: storage, evse: evse, contributions: contributions}
	if len(managed) == 1 {
		source.managed = managed[0]
	}
	return source
}
func (s *gatewayPortalCatalogSource) PortalCatalogContributions() *gatewayPortalCatalogContributions {
	if s == nil {
		return nil
	}
	return s.contributions
}

// bindServedCut installs the host cut reader. Nil restores the concrete owners.
func (s *gatewayPortalCatalogSource) bindServedCut(read func(context.Context) ([]catalogv1.Resource, error)) {
	if s == nil {
		return
	}
	s.servedMu.Lock()
	s.served = read
	s.servedMu.Unlock()
}

func (s *gatewayPortalCatalogSource) servedResources(ctx context.Context) ([]catalogv1.Resource, error) {
	if s == nil {
		return nil, nil
	}
	s.servedMu.Lock()
	read := s.served
	s.servedMu.Unlock()
	if read == nil {
		return nil, nil
	}
	return read(ctx)
}

// gatewayServedCutResources reads one page of the newest cut and keeps a row
// for each resource id an admitted descriptor names as its resource context.
func gatewayServedCutResources(ctx context.Context, graphTree *tree.Tree, contributions *gatewayPortalCatalogContributions) ([]catalogv1.Resource, error) {
	if graphTree == nil || contributions == nil {
		return nil, nil
	}
	result, err := graphTree.ListResources(ctx, graph.ListRequest{PageSize: graph.MaxPageSize})
	if err != nil {
		return nil, err
	}
	accepted := contributions.snapshot().Accepted
	seen := map[string]bool{}
	var out []catalogv1.Resource
	for _, entry := range result.Page.Resources {
		id := string(entry.ResourceID)
		if id == "" || seen[id] {
			continue
		}
		descriptor, ok := gatewayDescriptorForResource(accepted, id)
		if !ok {
			continue
		}
		row, ok := gatewayResourceFromCut(descriptor, entry, result.Page.CutID)
		if !ok {
			continue
		}
		seen[id] = true
		out = append(out, row)
	}
	return out, nil
}

func gatewayDescriptorForResource(accepted []contributionv1.AcceptedDescriptor, resourceID string) (contributionv1.AcceptedDescriptor, bool) {
	for _, descriptor := range accepted {
		for _, group := range descriptor.Manifest.Groups {
			if group.ResourceContext == resourceID {
				return descriptor, true
			}
		}
	}
	return contributionv1.AcceptedDescriptor{}, false
}

func gatewayResourceFromCut(descriptor contributionv1.AcceptedDescriptor, entry graph.ResourceEntry, cut semreg.Digest) (catalogv1.Resource, bool) {
	if len(descriptor.Manifest.Requires.Packs) != 1 {
		return catalogv1.Resource{}, false
	}
	var service, capability string
	if len(descriptor.Manifest.Fields) > 0 {
		service = descriptor.Manifest.Fields[0].ServiceRef.ID
		capability = descriptor.Manifest.Fields[0].CapabilityRef.ID
	} else if len(descriptor.Manifest.Actions) > 0 {
		service = descriptor.Manifest.Actions[0].ServiceRef.ID
		capability = descriptor.Manifest.Actions[0].CapabilityRef.ID
	}
	if service == "" || capability == "" {
		return catalogv1.Resource{}, false
	}
	snapshot, err := json.Marshal(entry)
	if err != nil {
		return catalogv1.Resource{}, false
	}
	return catalogv1.Resource{
		ID:                          string(entry.ResourceID),
		Domain:                      descriptor.Manifest.Requires.Packs[0].ID,
		ServiceID:                   service,
		CapabilityID:                capability,
		ContributionDriverID:        descriptor.Key.DriverID,
		ContributionManifestID:      descriptor.Key.ManifestID,
		ContributionManifestVersion: descriptor.Key.ManifestVersion,
		State:                       "CURRENT",
		Source: catalogv1.Source{
			AssetID:    string(entry.AssetID),
			SnapshotID: string(cut),
			Revision:   string(cut),
			Snapshot:   snapshot,
			Evaluation: json.RawMessage(`{}`),
			Selections: json.RawMessage(`[]`),
			Projection: json.RawMessage(`{}`),
		},
	}, true
}

// errGatewaySemanticActionUnavailable is the portal result for an admitted
// action that names a semantic operation. This invoker dispatches native
// setup procedures only.
var errGatewaySemanticActionUnavailable = errors.New("portal semantic action unavailable")

// gatewayHostActionAdmission is the host invoker and revalidator. A native
// action is a setup procedure on an instance or transport node. A semantic
// action has no dispatcher here and is unavailable. The revalidator repeats
// the detached source identity of the action the catalog just built.
type gatewayHostActionAdmission struct {
	tree *tree.Tree
}

func newGatewayHostActionAdmission(graphTree *tree.Tree) gatewayHostActionAdmission {
	return gatewayHostActionAdmission{tree: graphTree}
}

func (a gatewayHostActionAdmission) Revalidate(action catalogv1.Action, claim catalogv1.Claims) error {
	if claim.ResourceID == "" || claim.ResourceID != action.ResourceID || claim.Revision != action.Source.Revision || claim.BindingID != action.Source.BindingID || claim.SourceEpoch != action.Source.SourceEpoch || claim.SnapshotID != action.Source.SnapshotID {
		return catalogv1.ErrStale
	}
	if claim.Deadline.IsZero() || !time.Now().Before(claim.Deadline) {
		return catalogv1.ErrStale
	}
	return nil
}

func (a gatewayHostActionAdmission) Invoke(action catalogv1.Action, claim catalogv1.Claims) (json.RawMessage, error) {
	if action.NativeMemberID != "" {
		if a.tree == nil {
			return nil, fmt.Errorf("portal action %s has no host tree", action.ID)
		}
		key, err := gatewayNativeActionKey(a.tree, action.ContributionDriverID, action.NativeMemberID, action.ResourceID, action.Source.AssetID)
		if err != nil {
			return nil, err
		}
		outcome, err := gatewaySetupSurface(a).Dispatch(context.Background(), key, []byte(claim.IdempotencyKey))
		if err != nil {
			return nil, err
		}
		if outcome.Error != "" {
			return nil, fmt.Errorf("portal action %s: %s", action.ID, outcome.Error)
		}
		return json.Marshal(map[string]string{"native": string(outcome.Native)})
	}
	return nil, errGatewaySemanticActionUnavailable
}

// gatewayNativeActionKey selects the setup method on the instance that owns
// the selected resource. Ownership is the host asset of that instance. An
// instance node wins over a transport node of the same instance. A device
// node is not a setup procedure, and more than one node at the chosen level
// is refused.
func gatewayNativeActionKey(graphTree *tree.Tree, driverID, local, resourceID, sourceAsset string) (driver.MemberKey, error) {
	if graphTree == nil || graphTree.Registry == nil || resourceID == "" {
		return driver.MemberKey{}, fmt.Errorf("native action %s is not registered", local)
	}
	ctx := context.Background()
	asset, err := gatewayResourceAsset(ctx, graphTree, resourceID)
	if err != nil {
		return driver.MemberKey{}, err
	}
	if asset == "" || (sourceAsset != "" && sourceAsset != string(asset)) {
		return driver.MemberKey{}, fmt.Errorf("native action %s is not registered", local)
	}
	changes, ok := graphTree.Registry.Since(0)
	if !ok {
		return driver.MemberKey{}, fmt.Errorf("native action %s is not registered", local)
	}
	cut, err := graphTree.Capture(ctx, tree.CutOptions{})
	if err != nil {
		return driver.MemberKey{}, err
	}
	defer func() { _ = cut.Close() }()
	owners := map[driver.NamespaceID]driver.DriverKind{}
	for _, entry := range cut.Roster() {
		owners[entry.Key] = entry.Owner
	}
	instanceKeys := map[driver.MemberKey]struct{}{}
	transportKeys := map[driver.MemberKey]struct{}{}
	for _, change := range changes {
		if change.Kind != tree.ChangeMethod {
			continue
		}
		descriptor, err := cut.Describe(ctx, change.Ref)
		if err != nil || descriptor.Withdrawn || descriptor.Local != local {
			continue
		}
		if owners[descriptor.Namespace] != driver.DriverKind(driverID) {
			continue
		}
		parsed, err := tree.ParseKey(string(change.Ref))
		if err != nil {
			continue
		}
		level, err := parsed.Node.Level()
		if err != nil || (level != tree.LevelInstance && level != tree.LevelTransport) {
			continue
		}
		if gatewayInstanceAsset(parsed.Node.Driver, parsed.Node.Instance) != asset {
			continue
		}
		key := driver.MemberKey{
			Namespace: descriptor.Namespace,
			Instance:  parsed.Node.Instance,
			Node:      driver.NodeID(parsed.Node.NodeID()),
			Local:     driver.MemberID(local),
		}
		if level == tree.LevelInstance {
			instanceKeys[key] = struct{}{}
			continue
		}
		transportKeys[key] = struct{}{}
	}
	switch {
	case len(instanceKeys) == 1:
		return gatewayOneMemberKey(instanceKeys), nil
	case len(instanceKeys) == 0 && len(transportKeys) == 1:
		return gatewayOneMemberKey(transportKeys), nil
	case len(instanceKeys) == 0 && len(transportKeys) == 0:
		return driver.MemberKey{}, fmt.Errorf("native action %s is not registered", local)
	default:
		return driver.MemberKey{}, fmt.Errorf("native action %s matches more than one node", local)
	}
}

func gatewayOneMemberKey(keys map[driver.MemberKey]struct{}) driver.MemberKey {
	for key := range keys {
		return key
	}
	return driver.MemberKey{}
}

// gatewayResourceAsset reads every page of the newest cut until it finds the
// resource. An empty asset means the resource is not in the cut.
func gatewayResourceAsset(ctx context.Context, graphTree *tree.Tree, resourceID string) (semreg.AssetID, error) {
	seen := map[string]struct{}{}
	var token string
	for {
		if _, again := seen[token]; again {
			return "", fmt.Errorf("native action resource %s page repeated", resourceID)
		}
		seen[token] = struct{}{}
		result, err := graphTree.ListResources(ctx, graph.ListRequest{PageSize: graph.MaxPageSize, PageToken: token})
		if err != nil {
			return "", err
		}
		for _, entry := range result.Page.Resources {
			if string(entry.ResourceID) == resourceID {
				return entry.AssetID, nil
			}
		}
		if result.Page.NextPage == "" {
			return "", nil
		}
		token = result.Page.NextPage
	}
}

// gatewayInstanceAsset is the asset the host stamps on one driver instance.
func gatewayInstanceAsset(kind driver.DriverKind, instance driver.InstanceKey) semreg.AssetID {
	sum := sha256.Sum256([]byte(life.Ref{Driver: kind, Instance: instance}.ID()))
	return semreg.AssetID("asset:instance:" + hex.EncodeToString(sum[:8]))
}

func gatewayPortalStoragePublication(provider *growattBMSRS485ProductionProvider) *growattStoragePublication {
	if provider == nil {
		return nil
	}
	return provider.storage
}
func (s *gatewayPortalCatalogSource) Capture(at time.Time) (catalogv1.Capture, error) {
	resources, fields, diagnostics, err := s.CaptureDiagnostics(at)
	s.logDropped(diagnostics)
	// These packs are the domains this source serves. The composer does not
	// invent them, and a capture may add another admitted domain beside them.
	domains := append([]contributionv1.PackRef(nil), catalogv1.FivePacks...)
	return catalogv1.Capture{Domains: domains, Resources: resources, Fields: fields}, err
}

// logDropped reports every dropped resource with its reason whenever the set
// changes, so a dropped resource is visible outside the diagnostics method.
func (s *gatewayPortalCatalogSource) logDropped(diagnostics []gatewayPortalCatalogDiagnostic) {
	if s == nil {
		return
	}
	encoded, err := json.Marshal(diagnostics)
	if err != nil {
		return
	}
	s.droppedMu.Lock()
	changed := s.dropped != string(encoded)
	s.dropped = string(encoded)
	s.droppedMu.Unlock()
	if !changed || len(diagnostics) == 0 {
		return
	}
	for _, d := range diagnostics {
		log.Printf("portal catalog: resource %q of asset %q not kept: %s (driver %q, manifest %q)", d.ResourceID, d.AssetID, d.Reason, d.DriverID, d.ManifestID)
	}
}

// CaptureDiagnostics is Capture plus one diagnostic for every resource the
// capture could not keep, so a dropped resource always carries its reason.
func (s *gatewayPortalCatalogSource) CaptureDiagnostics(at time.Time) ([]catalogv1.Resource, []catalogv1.Field, []gatewayPortalCatalogDiagnostic, error) {
	out := portalCapture{resources: []catalogv1.Resource{}, fields: []catalogv1.Field{}, diagnostics: []gatewayPortalCatalogDiagnostic{}}
	if s == nil {
		return out.resources, out.fields, out.diagnostics, nil
	}
	if s.managed != nil {
		_ = s.managed.modbus(context.Background(), func(adapter *modbusadapter.Adapter) error {
			source := &gatewayPortalCatalogSource{pv: adapter, contributions: s.contributions, drivers: s.drivers}
			r, f, d, err := source.CaptureDiagnostics(at)
			out.merge(r, f, d)
			return err
		})
		_ = s.managed.growatt(context.Background(), func(provider *growattBMSRS485ProductionProvider) error {
			source := &gatewayPortalCatalogSource{storage: provider.storage, contributions: s.contributions, drivers: s.drivers}
			r, f, d, err := source.CaptureDiagnostics(at)
			out.merge(r, f, d)
			return err
		})
	}
	attribution := s.attribution()
	if s.pv != nil && s.contributions != nil && s.contributions.accepts("pv.primary", "portal.pv") {
		if v, ok := s.pv.SemanticPVCurrentSingleAt(at); ok {
			// The PV publication does not name its owner's source, so its
			// assets keep the single-source rule.
			out.add(attribution, gatewayPortalPVDescriptor(), v.Snapshot, v.Evaluation, v.Selections, v.Projection)
		}
	}
	if s.storage != nil && s.contributions != nil && s.contributions.accepts("storage.primary", "portal.storage") {
		if v, ok := s.storage.CurrentAt(string(s.storage.assetID), at); ok {
			out.add(attribution.ownedBy(s.storage.sourceID), gatewayPortalStorageDescriptor(), v.Snapshot, v.Evaluation, []semreg.Selection{}, v.Projection)
		}
	}
	if s.evse != nil && s.contributions != nil && s.contributions.accepts("evse.primary", "portal.evse") {
		if v, ok := s.evse.SemanticEVSECurrentAt(at); ok {
			out.add(attribution.ownedBy(semreg.SourceID(s.evse.cfg.SourceID)), gatewayPortalEVSEDescriptor(), v.Snapshot, v.Evaluation, v.Selections, v.Projection)
		}
	}
	served, err := s.servedResources(context.Background())
	if err != nil {
		log.Printf("portal catalog: served cut not read: %s", err.Error())
	} else {
		out.merge(served, nil, nil)
	}
	return out.resources, out.fields, out.diagnostics, nil
}

// attribution captures what a capture needs to name a binding's driver: the
// source-to-driver resolution and the admitted descriptors. The composition
// does not supply a resolver yet, so a production capture keeps the
// single-source rule.
func (s *gatewayPortalCatalogSource) attribution() portalAttribution {
	if s == nil || s.drivers == nil || s.contributions == nil {
		return portalAttribution{}
	}
	return portalAttribution{drivers: s.drivers, accepted: s.contributions.snapshot().Accepted}
}

// gatewayPortalSourceDrivers resolves the driver that holds a source. The
// publication hub satisfies it.
type gatewayPortalSourceDrivers interface {
	Source(semreg.SourceID) (semhub.SourceInfo, bool)
}

var _ gatewayPortalSourceDrivers = (*semhub.Hub)(nil)

// gatewayPortalCatalogDiagnostic names a resource the catalog source did not
// keep and why. DriverID and ManifestID name the descriptor that asked for it.
type gatewayPortalCatalogDiagnostic struct {
	Domain     string `json:"domain"`
	AssetID    string `json:"asset_id"`
	ResourceID string `json:"resource_id"`
	DriverID   string `json:"driver_id"`
	ManifestID string `json:"manifest_id"`
	Reason     string `json:"reason"`
}

// Reasons a resource is not kept.
const (
	// The descriptor names no single pack, field or resource context.
	portalDropDescriptorUnusable = "descriptor_unusable"
	// A service instance names a binding the snapshot does not hold.
	portalDropBindingUnresolved = "binding_unresolved"
	// No source holds a complete current service and capability path.
	portalDropCapabilityUnbound = "capability_unbound"
	// One source holds several instances, or several owner or contributor
	// paths qualify and none is the owner's.
	portalDropCapabilityAmbiguous = "capability_ambiguous"
	// The asset holds several sources and the owner's source or the driver of
	// a path's source is not known.
	portalDropDriverUnresolved = "contributor_driver_unresolved"
	// The contributor that carries the capability has no admitted descriptor
	// for the resource.
	portalDropContributorUnadmitted = "contributor_unadmitted"
	// The snapshot's revisions or evaluation cannot be recorded.
	portalDropSourceIncomplete = "source_record_incomplete"
)

type portalAttribution struct {
	drivers  gatewayPortalSourceDrivers
	accepted []contributionv1.AcceptedDescriptor
	// owner is the source the asset is declared with by the publication the
	// capture read the snapshot from. Only that source's path is the owner's
	// and is attributed to the descriptor the publication contributes; a
	// contributor admitted under the same driver is still a contributor.
	owner semreg.SourceID
}

// ownedBy names the owner's source of the publication a snapshot was read
// from.
func (a portalAttribution) ownedBy(source semreg.SourceID) portalAttribution {
	a.owner = source
	return a
}

type portalCapture struct {
	resources   []catalogv1.Resource
	fields      []catalogv1.Field
	diagnostics []gatewayPortalCatalogDiagnostic
}

func (c *portalCapture) merge(resources []catalogv1.Resource, fields []catalogv1.Field, diagnostics []gatewayPortalCatalogDiagnostic) {
	c.resources = append(c.resources, resources...)
	c.fields = append(c.fields, fields...)
	c.diagnostics = append(c.diagnostics, diagnostics...)
}

func (c *portalCapture) add(attribution portalAttribution, descriptor contributionv1.Manifest, snapshot semreg.Snapshot, evaluation any, selections any, projection any) {
	resource, reason := portalSnapshotResource(attribution, descriptor, snapshot, evaluation, selections, projection)
	if reason != "" {
		diagnostic := gatewayPortalCatalogDiagnostic{AssetID: string(snapshot.AssetID), ResourceID: portalResourceContext(descriptor), DriverID: descriptor.Contributor.DriverID, ManifestID: descriptor.ManifestID, Reason: reason}
		if len(descriptor.Requires.Packs) == 1 {
			diagnostic.Domain = descriptor.Requires.Packs[0].ID
		}
		c.diagnostics = append(c.diagnostics, diagnostic)
		return
	}
	c.resources = append(c.resources, resource)
}

// portalSnapshotCapture captures one asset snapshot for one descriptor.
func portalSnapshotCapture(attribution portalAttribution, descriptor contributionv1.Manifest, snapshot semreg.Snapshot, evaluation any, selections any, projection any) ([]catalogv1.Resource, []catalogv1.Field, []gatewayPortalCatalogDiagnostic) {
	out := portalCapture{resources: []catalogv1.Resource{}, fields: []catalogv1.Field{}, diagnostics: []gatewayPortalCatalogDiagnostic{}}
	out.add(attribution, descriptor, snapshot, evaluation, selections, projection)
	return out.resources, out.fields, out.diagnostics
}

func rawPortal(v any) (json.RawMessage, string) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, ""
	}
	return json.RawMessage(b), string(b)
}
func portalDigest(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func appendPortalSnapshot(resources []catalogv1.Resource, fields []catalogv1.Field, descriptor contributionv1.Manifest, snapshot semreg.Snapshot, evaluation any, selections any, projection any) ([]catalogv1.Resource, []catalogv1.Field) {
	r, f, _ := portalSnapshotCapture(portalAttribution{}, descriptor, snapshot, evaluation, selections, projection)
	return append(resources, r...), append(fields, f...)
}

func portalResourceContext(descriptor contributionv1.Manifest) string {
	if len(descriptor.Fields) == 0 {
		return ""
	}
	for _, group := range descriptor.Groups {
		if group.ID == descriptor.Fields[0].Group {
			return group.ResourceContext
		}
	}
	return ""
}

func portalSnapshotResource(attribution portalAttribution, descriptor contributionv1.Manifest, snapshot semreg.Snapshot, evaluation any, selections any, projection any) (catalogv1.Resource, string) {
	resourceID := portalResourceContext(descriptor)
	if len(descriptor.Requires.Packs) != 1 || resourceID == "" {
		return catalogv1.Resource{}, portalDropDescriptorUnusable
	}
	paths, reason := portalSnapshotPaths(snapshot, descriptor.Fields[0].ServiceRef, descriptor.Fields[0].CapabilityRef)
	if reason != "" {
		return catalogv1.Resource{}, reason
	}
	path, contributor, reason := attribution.choose(descriptor, resourceID, snapshot, paths)
	if reason != "" {
		return catalogv1.Resource{}, reason
	}
	snap, _ := rawPortal(snapshot)
	eval, evalText := rawPortal(evaluation)
	sels, _ := rawPortal(selections)
	proj, _ := rawPortal(projection)
	revisions, revisionText := rawPortal(snapshot.Revisions)
	if len(revisions) == 0 || len(evalText) == 0 {
		return catalogv1.Resource{}, portalDropSourceIncomplete
	}
	source := catalogv1.Source{AssetID: string(snapshot.AssetID), SnapshotID: string(snapshot.SnapshotID), Revision: revisionText, EvaluationDigest: portalDigest(evaluation), BindingID: string(path.capability.BindingID), SourceEpoch: string(path.capability.SourceEpochID), DriverGeneration: path.generation, Snapshot: snap, Evaluation: eval, Selections: sels, Projection: proj}
	// A source snapshot is the complete fact/projection envelope.  Do not make
	// a second, lossy field table with null values or reconstructed quality.
	return catalogv1.Resource{ID: resourceID, Domain: descriptor.Requires.Packs[0].ID, ServiceID: string(path.service.Definition.ID), CapabilityID: string(path.capability.Definition.ID), State: "CURRENT", Source: source, ContributionDriverID: contributor.driver, ContributionManifestID: contributor.manifest, ContributionManifestVersion: contributor.version}, ""
}

// portalPath is one source's complete current service and capability path.
type portalPath struct {
	source     semreg.SourceID
	service    semreg.ServiceInstance
	capability semreg.CapabilityInstance
	generation uint64
}

type portalContributor struct{ driver, manifest, version string }

// portalSnapshotPaths groups the service instances of the descriptor's
// definition by the source of their binding and returns each source's path.
// Within one source the rules are those of a single-source asset: exactly one
// service instance, exactly one capability instance on it, and a current
// binding and source epoch that agree with both.
func portalSnapshotPaths(snapshot semreg.Snapshot, serviceRef, capabilityRef contributionv1.DefinitionRef) ([]portalPath, string) {
	wantService, wantCapability := semanticRef(serviceRef), semanticRef(capabilityRef)
	bindingSource := make(map[semreg.NativeBindingID]semreg.SourceID, len(snapshot.Bindings))
	for _, binding := range snapshot.Bindings {
		if binding.AssetID != snapshot.AssetID {
			continue
		}
		if known, ok := bindingSource[binding.BindingID]; ok && known != binding.SourceID {
			return nil, portalDropBindingUnresolved
		}
		bindingSource[binding.BindingID] = binding.SourceID
	}
	services := make(map[semreg.SourceID][]semreg.ServiceInstance)
	for _, candidate := range snapshot.Services {
		if candidate.AssetID != snapshot.AssetID || candidate.Definition != wantService {
			continue
		}
		source, ok := bindingSource[candidate.BindingID]
		if !ok || source == "" {
			return nil, portalDropBindingUnresolved
		}
		services[source] = append(services[source], candidate)
	}
	sources := make([]semreg.SourceID, 0, len(services))
	for source := range services {
		sources = append(sources, source)
	}
	sort.Slice(sources, func(i, j int) bool { return sources[i] < sources[j] })
	paths := make([]portalPath, 0, len(sources))
	// A source that publishes an ambiguous path is invalid on its own; the
	// other sources still decide the resource.
	ambiguous := false
	for _, source := range sources {
		if len(services[source]) != 1 {
			ambiguous = true
			continue
		}
		path, reason := portalSourcePath(snapshot, source, services[source][0], wantCapability)
		if reason == portalDropCapabilityAmbiguous {
			ambiguous = true
			continue
		}
		if reason == "" {
			paths = append(paths, path)
		}
	}
	if len(paths) == 0 {
		if ambiguous {
			return nil, portalDropCapabilityAmbiguous
		}
		return nil, portalDropCapabilityUnbound
	}
	return paths, ""
}

func portalSourcePath(snapshot semreg.Snapshot, source semreg.SourceID, service semreg.ServiceInstance, wantCapability semreg.DefinitionRef) (portalPath, string) {
	var capability semreg.CapabilityInstance
	capabilityMatches := 0
	for _, candidate := range snapshot.Capabilities {
		if candidate.AssetID == snapshot.AssetID && candidate.Definition == wantCapability && candidate.ServiceInstance == service.InstanceID && candidate.BindingID == service.BindingID && candidate.SourceEpochID == service.SourceEpochID && candidate.DriverGeneration == service.DriverGeneration {
			capability, capabilityMatches = candidate, capabilityMatches+1
		}
	}
	if capabilityMatches > 1 {
		return portalPath{}, portalDropCapabilityAmbiguous
	}
	if capabilityMatches != 1 {
		return portalPath{}, portalDropCapabilityUnbound
	}
	generation, err := strconv.ParseUint(string(capability.DriverGeneration), 10, 64)
	if err != nil || generation == 0 {
		return portalPath{}, portalDropCapabilityUnbound
	}
	if service.BindingID == "" || service.SourceEpochID == "" || capability.BindingID == "" || capability.SourceEpochID == "" || service.BindingID != capability.BindingID || service.SourceEpochID != capability.SourceEpochID || service.DriverGeneration != capability.DriverGeneration {
		return portalPath{}, portalDropCapabilityUnbound
	}
	bindingMatches := 0
	for _, candidate := range snapshot.Bindings {
		if candidate.BindingID == capability.BindingID && candidate.AssetID == snapshot.AssetID && candidate.SourceID == source && candidate.SourceEpochID == capability.SourceEpochID && candidate.DriverGeneration == capability.DriverGeneration && candidate.State == semreg.BindingCurrent {
			bindingMatches++
		}
	}
	if bindingMatches != 1 {
		return portalPath{}, portalDropCapabilityUnbound
	}
	sourceMatches := 0
	for _, candidate := range snapshot.Sources {
		if candidate.SourceID == source && candidate.SourceEpochID == capability.SourceEpochID && candidate.State == semreg.SourceCurrent {
			sourceMatches++
		}
	}
	if sourceMatches != 1 {
		return portalPath{}, portalDropCapabilityUnbound
	}
	return portalPath{source: source, service: service, capability: capability, generation: generation}, ""
}

// choose keeps the owner's path, or the path of the one contributor whose
// binding carries the capability, and names the driver of that binding. The
// owner is the source the publication the snapshot was read from declares the
// asset with, compared by source and never by driver; its path is attributed
// to the descriptor that publication contributes, while a contributor's path
// is attributed to its own driver's admitted descriptor.
func (a portalAttribution) choose(descriptor contributionv1.Manifest, resourceID string, snapshot semreg.Snapshot, paths []portalPath) (portalPath, portalContributor, string) {
	owner := portalContributor{driver: descriptor.Contributor.DriverID, manifest: descriptor.ManifestID, version: descriptor.ManifestVersion}
	if a.drivers == nil || a.owner == "" {
		// Without driver resolution or the owner's source only a
		// single-source asset can be attributed: its one source is the
		// descriptor's driver.
		if portalCurrentSources(snapshot) > 1 {
			return portalPath{}, portalContributor{}, portalDropDriverUnresolved
		}
		if len(paths) != 1 {
			return portalPath{}, portalContributor{}, portalDropCapabilityAmbiguous
		}
		return paths[0], owner, ""
	}
	drivers := make([]string, len(paths))
	ownerPath := -1
	for index, path := range paths {
		info, ok := a.drivers.Source(path.source)
		if !ok || info.Driver == "" {
			return portalPath{}, portalContributor{}, portalDropDriverUnresolved
		}
		drivers[index] = info.Driver
		if path.source == a.owner {
			ownerPath = index
		}
	}
	if ownerPath >= 0 {
		return paths[ownerPath], owner, ""
	}
	if len(paths) != 1 {
		return portalPath{}, portalContributor{}, portalDropCapabilityAmbiguous
	}
	contributor, ok := a.descriptorOf(drivers[0], descriptor, resourceID)
	if !ok {
		return portalPath{}, portalContributor{}, portalDropContributorUnadmitted
	}
	return paths[0], contributor, ""
}

// descriptorOf returns the one admitted descriptor of driver that declares the
// same pack, resource context, service and capability as descriptor.
func (a portalAttribution) descriptorOf(driver string, descriptor contributionv1.Manifest, resourceID string) (portalContributor, bool) {
	var found portalContributor
	matches := 0
	for _, accepted := range a.accepted {
		if accepted.Key.DriverID != driver || len(accepted.Manifest.Requires.Packs) != 1 || accepted.Manifest.Requires.Packs[0] != descriptor.Requires.Packs[0] {
			continue
		}
		groups := make(map[string]bool, len(accepted.Manifest.Groups))
		for _, group := range accepted.Manifest.Groups {
			if group.ResourceContext == resourceID {
				groups[group.ID] = true
			}
		}
		for _, field := range accepted.Manifest.Fields {
			if groups[field.Group] && field.ServiceRef == descriptor.Fields[0].ServiceRef && field.CapabilityRef == descriptor.Fields[0].CapabilityRef {
				found, matches = portalContributor{driver: accepted.Key.DriverID, manifest: accepted.Key.ManifestID, version: accepted.Key.ManifestVersion}, matches+1
				break
			}
		}
	}
	return found, matches == 1
}

// portalCurrentSources counts the distinct sources with a current epoch.
func portalCurrentSources(snapshot semreg.Snapshot) int {
	sources := make(map[semreg.SourceID]struct{}, len(snapshot.Sources))
	for _, source := range snapshot.Sources {
		if source.State == semreg.SourceCurrent {
			sources[source.SourceID] = struct{}{}
		}
	}
	return len(sources)
}
