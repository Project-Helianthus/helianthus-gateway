package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync/atomic"

	ebusgateway "github.com/Project-Helianthus/helianthus-gateway"
	"github.com/Project-Helianthus/helianthus-gateway/graphql"
	"github.com/Project-Helianthus/helianthus-gateway/internal/semanticmetrics"
	"github.com/Project-Helianthus/helianthus-gateway/m2mgraphql"
	"github.com/Project-Helianthus/helianthus-gateway/mcp"
	"github.com/Project-Helianthus/helianthus-gateway/portal/catalogv1"
	"github.com/Project-Helianthus/helianthus-semreg/semreg/runtime/tree"
	semreg "github.com/Project-Helianthus/helianthus-semreg/semreg/v1"
	"github.com/Project-Helianthus/helianthus-semreg/semreg/v1/graph"
)

// httpControlPlaneProviders retains the shared semantic provider and the
// handlers that consume it. The caller owns validation, routes and listeners.
type httpControlPlaneProviders struct {
	sharedSemanticProvider *hubSemanticSnapshotProvider
	queryHandler           http.Handler
	snapshotHandler        http.Handler
	subscriptionHandler    http.Handler
	mcpServer              *mcp.Server
	semanticMetrics        *gatewaySemanticMetrics
}

// newHTTPControlPlaneProviders composes registrations in startup order without
// mounting routes or opening listeners. Optional native providers remain optional.
func newHTTPControlPlaneProviders(
	cfg ebusgateway.Config,
	gateway *ebusgateway.Gateway,
	builder *graphql.Builder,
	hub *graphql.BroadcastHub,
	semanticProvider graphql.SemanticProvider,
	eebusProvider mcp.EEBusV1Provider,
	eebusCommandRouter mcp.EEBusV1CommandRouter,
	modbusProvider mcp.ModbusV1Provider,
	busObservability *ebusgateway.BusObservabilityStore,
	watchSummaryProvider mcp.WatchSummaryProvider,
	ebusSourceProvider func() (byte, bool),
	buildInfo gatewayBuildInfo,
	daemonUpdatesAvailable func() bool,
	ebusDriver *ebusDriverController,
	runtimeControl *gatewayRuntimeControl,
) (*httpControlPlaneProviders, error) {
	sharedSemanticProvider := newGatewaySemanticSnapshotProvider(runtimeControl)
	if err := installGatewaySemanticOperationRoute(builder, sharedSemanticProvider); err != nil {
		return nil, err
	}
	queryHandler, snapshotHandler, subscriptionHandler, mcpServer, err := newHTTPControlPlaneSemanticHandlers(gateway, builder, hub, sharedSemanticProvider)
	if err != nil {
		return nil, err
	}
	// The scrape route reads the same provider instance as GraphQL, MCP and
	// Portal; its transport exposition stays owned by the bus store.
	// The directory is the host state directory when the host is open, and
	// empty otherwise so a server that did not open the host does not create
	// a store.
	stateDir := gatewayComposedStateDir()
	semanticMetrics, err := newGatewaySemanticMetrics(sharedSemanticProvider, gatewaySemanticMetricRegistry(stateDir))
	if err != nil {
		return nil, err
	}
	if graphTree := gatewayComposedTree(); graphTree != nil {
		semanticMetrics.tree = graphTree
	}
	if stateDir != "" {
		if gatewayMetricStoreBeforeHold != nil {
			gatewayMetricStoreBeforeHold(stateDir)
		}
		if holdErr := holdGatewayMetricStore(stateDir); holdErr != nil {
			log.Printf("semantic metric store lock: %s", holdErr.Error())
			// The registry was loaded before this lock. Serving it would
			// publish bindings the process does not hold. A failed hold is
			// the same registry a failed load returns.
			semanticMetrics.adopt(semanticmetrics.Registry{StoreError: holdErr.Error(), StoreDirectory: stateDir})
		}
	}
	if err := mcpServer.SetServerVersion(buildInfo.ReleaseVersion); err != nil {
		return nil, fmt.Errorf("configure MCP build identity: %w", err)
	}
	if err := registerRuntimeControlMCPTools(mcpServer, runtimeControl); err != nil {
		return nil, fmt.Errorf("register runtime control MCP tools: %w", err)
	}
	if eebusProvider != nil {
		if err := mcpServer.RegisterEEBusV1Provider(eebusProvider); err != nil {
			return nil, fmt.Errorf("register eeBUS MCP provider: %w", err)
		}
	}
	if eebusCommandRouter != nil {
		if err := mcpServer.RegisterEEBusV1CommandRouter(eebusCommandRouter); err != nil {
			return nil, fmt.Errorf("register eeBUS MCP command router: %w", err)
		}
	}
	mcp.RegisterModbusV1Tools(mcpServer, modbusProvider)
	if source, ok := eebusProvider.(mcp.LeafPromotionCaptureSource); ok {
		if capture := source.LeafPromotionCapture(); capture != nil {
			if err := mcpServer.RegisterLeafPromotionCapture(capture); err != nil {
				return nil, fmt.Errorf("register leaf promotion capture: %w", err)
			}
		}
	}
	oneShotRuntime, err := newSynchronizedEvidenceOneShotRuntime(
		cfg.EvidenceOneShotEnabled,
		eebusProvider,
		eebusCommandRouter,
		buildInfo,
	)
	if err != nil {
		return nil, err
	}
	if oneShotRuntime != nil {
		if err := mcpServer.RegisterSynchronizedEvidenceCapture(oneShotRuntime); err != nil {
			return nil, fmt.Errorf("register synchronized evidence capture: %w", err)
		}
	}
	mcpServer.SetAdmittedRPCSourceProvider(ebusSourceProvider)
	mcpServer.SetStatusProvider(newMCPRuntimeStatusProviderForControl(semanticProvider, ebusSourceProvider, buildInfo, daemonUpdatesAvailable, ebusAdapterLifecycleStatus(ebusDriver), ebusAdapterConditionFor(ebusDriver)))
	if busObservability != nil {
		mcpServer.SetBusObservabilityProvider(newMCPBusObservabilityProvider(busObservability))
	}
	if watchSummaryProvider != nil {
		mcpServer.SetWatchSummaryProvider(watchSummaryProvider)
	}
	mcpServer.SetSemanticProvider(newMCPSemanticProvider(semanticProvider))

	return &httpControlPlaneProviders{
		sharedSemanticProvider: sharedSemanticProvider,
		queryHandler:           queryHandler,
		snapshotHandler:        snapshotHandler,
		subscriptionHandler:    subscriptionHandler,
		mcpServer:              mcpServer,
		semanticMetrics:        semanticMetrics,
	}, nil
}

// gatewayComposedSurface is the host tree and state directory the HTTP and
// remote graph surfaces read at request time. M2M is built before the control
// plane starts, so the reads consult this holder instead of a constructor
// argument.
type gatewayComposedSurface struct {
	tree     *tree.Tree
	stateDir string
}

var (
	gatewayComposedSurfaceState atomic.Pointer[gatewayComposedSurface]
	gatewayMetricStoreHold      atomic.Pointer[semanticmetrics.SlotFileStore]
	// gatewayMetricStoreBeforeHold runs after the registry is loaded and
	// before the lock is taken again. A test takes the lock in that interval.
	gatewayMetricStoreBeforeHold func(directory string)
)

func gatewayComposedTree() *tree.Tree {
	surface := gatewayComposedSurfaceState.Load()
	if surface == nil {
		return nil
	}
	return surface.tree
}

func gatewayComposedStateDir() string {
	surface := gatewayComposedSurfaceState.Load()
	if surface == nil {
		return ""
	}
	return surface.stateDir
}

// bindGatewayComposedSurfaces publishes the opened host to the catalog source,
// the semantic index, the action admission and the request-time holder.
func bindGatewayComposedSurfaces(opened *gatewayHost, source catalogv1.SourceCapture) {
	if opened == nil || opened.site == nil || opened.site.Tree() == nil {
		gatewayComposedSurfaceState.Store(nil)
		return
	}
	graphTree := opened.site.Tree()
	if concrete, ok := source.(*gatewayPortalCatalogSource); ok && concrete != nil {
		if contributions := concrete.PortalCatalogContributions(); contributions != nil {
			contributions.bindHostTree(graphTree)
			admission := newGatewayHostActionAdmission(graphTree)
			contributions.bindActionAdmission(admission, admission)
		}
		concrete.bindServedCut(func(ctx context.Context) ([]catalogv1.Resource, error) {
			return gatewayServedCutResources(ctx, graphTree, concrete.PortalCatalogContributions())
		})
	}
	gatewayComposedSurfaceState.Store(&gatewayComposedSurface{tree: graphTree, stateDir: opened.stateDir})
}

// resetGatewayComposedSurfaces drops the holder and releases the metric lock.
func resetGatewayComposedSurfaces() {
	gatewayComposedSurfaceState.Store(nil)
	closeGatewayMetricStore()
}

// holdGatewayMetricStore reopens the slot the production registry created and
// keeps its lock until shutdown. The registry composer releases its own lock
// before it returns; this hold is the one that stays.
func holdGatewayMetricStore(directory string) error {
	if directory == "" {
		return nil
	}
	persistence, err := semanticmetrics.NewSlotFileStore(directory, gatewaySemanticMetricTarget)
	if err != nil {
		return err
	}
	if !gatewayMetricStoreHold.CompareAndSwap(nil, persistence) {
		_ = persistence.Close()
	}
	return nil
}

func closeGatewayMetricStore() {
	if store := gatewayMetricStoreHold.Swap(nil); store != nil {
		_ = store.Close()
	}
}

// gatewaySemanticGraphQueryHandler puts the host tree on each local GraphQL
// request. An unbound host leaves the request unchanged.
func gatewaySemanticGraphQueryHandler(next http.Handler) http.Handler {
	if next == nil {
		return nil
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if graphTree := gatewayComposedTree(); graphTree != nil {
			r = r.WithContext(graphql.WithSemanticGraph(r.Context(), graphTree))
		}
		next.ServeHTTP(w, r)
	})
}

func gatewayRemoteSemanticHandler(pinned http.Handler, allowed map[string]struct{}, pv, storage, evse func(context.Context, string) (json.RawMessage, bool)) (http.Handler, error) {
	if pinned == nil {
		return nil, errors.New("M2M GraphQL handler configuration is invalid")
	}
	if allowed == nil {
		allowed = map[string]struct{}{}
	}
	published := allowed
	return m2mgraphql.NewHandler(m2mgraphql.Config{
		AllowedAssets:          allowed,
		SemanticPVCurrent:      pv,
		SemanticStorageCurrent: storage,
		SemanticEVSECurrent:    evse,
		SemanticResources: func(ctx context.Context, variables map[string]any) (json.RawMessage, error) {
			return gatewaySemanticResourcesScoped(ctx, variables, published)
		},
		SemanticTraversal: func(ctx context.Context, variables map[string]any) (json.RawMessage, error) {
			return gatewaySemanticTraversalScoped(ctx, variables, published)
		},
		SemanticLens: func(ctx context.Context, variables map[string]any) (json.RawMessage, error) {
			return gatewaySemanticLensScoped(ctx, variables, published)
		},
		SemanticElementCatalog: gatewaySemanticElementCatalogRead,
	})
}

func gatewaySemanticResourcesScoped(ctx context.Context, variables map[string]any, allowed map[string]struct{}) (json.RawMessage, error) {
	graphTree, err := gatewayGraphTree()
	if err != nil {
		return nil, err
	}
	var request graph.ListRequest
	if err := gatewayGraphDecode(variables, &request); err != nil {
		return nil, gatewayGraphCode(err)
	}
	result, err := graphTree.ListResources(ctx, request)
	record := gatewayGraphResourceList{result.Page, result.NotIncluded}
	if err == nil && allowed != nil {
		index, indexErr := gatewayGraphAssetIndex(ctx, graphTree, result.Page.CutID)
		if indexErr != nil {
			return gatewayGraphFinish(gatewayGraphResourceList{}, indexErr)
		}
		record = gatewayScopeResourceList(record, allowed, index)
	}
	return gatewayGraphFinish(record, err)
}

func gatewaySemanticTraversalScoped(ctx context.Context, variables map[string]any, allowed map[string]struct{}) (json.RawMessage, error) {
	graphTree, err := gatewayGraphTree()
	if err != nil {
		return nil, err
	}
	var request graph.TraversalRequest
	if err := gatewayGraphDecode(variables, &request); err != nil {
		return nil, gatewayGraphCode(err)
	}
	result, err := graphTree.Traverse(ctx, request)
	record := gatewayGraphTraversal{result.Page, result.NotIncluded}
	if err == nil && allowed != nil {
		index, indexErr := gatewayGraphAssetIndex(ctx, graphTree, result.Page.CutID)
		if indexErr != nil {
			return gatewayGraphFinish(gatewayGraphTraversal{}, indexErr)
		}
		record = gatewayScopeTraversal(record, allowed, index)
	}
	return gatewayGraphFinish(record, err)
}

func gatewaySemanticLensScoped(ctx context.Context, variables map[string]any, allowed map[string]struct{}) (json.RawMessage, error) {
	graphTree, err := gatewayGraphTree()
	if err != nil {
		return nil, err
	}
	var request graph.LensRequest
	if err := gatewayGraphDecode(variables, &request); err != nil {
		return nil, gatewayGraphCode(err)
	}
	result, err := graphTree.RetrieveLens(ctx, request)
	if err == nil && allowed != nil {
		index, indexErr := gatewayGraphAssetIndex(ctx, graphTree, result.Cut.CutID)
		if indexErr != nil {
			return gatewayGraphFinish(graph.LensResult{}, indexErr)
		}
		result = gatewayScopeLens(result, allowed, index)
	}
	return gatewayGraphFinish(result, err)
}

// gatewayGraphAssetIndex lists every resource of the cut the caller just
// served, so a resource id on another page can still be classified. The host
// retains a cut under its full reference, so a cut id alone is not a lookup
// key. Each page is the newest cut, and a page whose cut id moved fails closed.
// A repeated page token fails closed too.
func gatewayGraphAssetIndex(ctx context.Context, graphTree *tree.Tree, cut semreg.Digest) (map[semreg.ResourceID]semreg.AssetID, error) {
	index := map[semreg.ResourceID]semreg.AssetID{}
	seen := map[string]struct{}{}
	var token string
	for {
		if _, again := seen[token]; again {
			return nil, errors.New("semantic graph asset index repeated its page")
		}
		seen[token] = struct{}{}
		result, err := graphTree.ListResources(ctx, graph.ListRequest{PageSize: graph.MaxPageSize, PageToken: token})
		if err != nil {
			return nil, err
		}
		if cut != "" && result.Page.CutID != cut {
			return nil, &semreg.Error{ID: semreg.RevisionConflict, Detail: "semantic graph asset index cut moved"}
		}
		for _, entry := range result.Page.Resources {
			index[entry.ResourceID] = entry.AssetID
		}
		if result.Page.NextPage == "" {
			return index, nil
		}
		token = result.Page.NextPage
	}
}

func gatewayAssetAllowed(allowed map[string]struct{}, asset semreg.AssetID) bool {
	_, ok := allowed[string(asset)]
	return ok
}

func gatewayEndpointHidden(allowed map[string]struct{}, index map[semreg.ResourceID]semreg.AssetID, id semreg.ResourceID) bool {
	asset, ok := index[id]
	return id != "" && ok && !gatewayAssetAllowed(allowed, asset)
}

func gatewayRelationHidden(allowed map[string]struct{}, index map[semreg.ResourceID]semreg.AssetID, asset semreg.AssetID, from, to semreg.ResourceID) bool {
	if !gatewayAssetAllowed(allowed, asset) {
		return true
	}
	return gatewayEndpointHidden(allowed, index, from) || gatewayEndpointHidden(allowed, index, to)
}

func gatewayKeep[T any](rows []T, required bool, keep func(T) bool) ([]T, bool) {
	if rows == nil {
		return nil, false
	}
	kept := make([]T, 0, len(rows))
	dropped := false
	for _, row := range rows {
		if keep(row) {
			kept = append(kept, row)
			continue
		}
		dropped = true
	}
	if !dropped {
		return rows, false
	}
	if len(kept) == 0 {
		if required {
			return []T{}, true
		}
		return nil, true
	}
	return kept, true
}

func gatewayScopeResourceList(page gatewayGraphResourceList, allowed map[string]struct{}, index map[semreg.ResourceID]semreg.AssetID) gatewayGraphResourceList {
	if allowed == nil {
		return page
	}
	resources, resourcesDropped := gatewayKeep(page.Resources, true, func(row graph.ResourceEntry) bool {
		return gatewayAssetAllowed(allowed, row.AssetID) && !gatewayEndpointHidden(allowed, index, row.ResourceID)
	})
	members, membersDropped := gatewayKeep(page.Members, true, func(row graph.CutMember) bool {
		return gatewayAssetAllowed(allowed, row.AssetID)
	})
	excluded, excludedDropped := gatewayKeep(page.Excluded, false, func(asset semreg.AssetID) bool {
		return gatewayAssetAllowed(allowed, asset)
	})
	omitted, omittedDropped := gatewayKeep(page.NotIncluded, false, func(row graph.NotIncludedAsset) bool {
		return gatewayAssetAllowed(allowed, row.AssetID)
	})
	if !resourcesDropped && !membersDropped && !excludedDropped && !omittedDropped {
		return page
	}
	page.Resources = resources
	page.Members = members
	page.Excluded = excluded
	page.NotIncluded = omitted
	if membersDropped || excludedDropped || omittedDropped {
		page.Completeness = graph.CutPartial
	}
	return page
}

func gatewayScopeEntry(entry graph.EntryPoint, allowed map[string]struct{}, index map[semreg.ResourceID]semreg.AssetID) (graph.EntryPoint, bool) {
	if !gatewayEndpointHidden(allowed, index, entry.Resource) {
		return entry, false
	}
	entry.Resource = ""
	return entry, true
}

func gatewayScopeTraversal(page gatewayGraphTraversal, allowed map[string]struct{}, index map[semreg.ResourceID]semreg.AssetID) gatewayGraphTraversal {
	if allowed == nil {
		return page
	}
	resources, resourcesDropped := gatewayKeep(page.Resources, true, func(row graph.ReachedResource) bool {
		return gatewayAssetAllowed(allowed, row.AssetID) && !gatewayEndpointHidden(allowed, index, row.ResourceID)
	})
	members, membersDropped := gatewayKeep(page.Members, true, func(row graph.CutMember) bool {
		return gatewayAssetAllowed(allowed, row.AssetID)
	})
	excluded, excludedDropped := gatewayKeep(page.Excluded, false, func(asset semreg.AssetID) bool {
		return gatewayAssetAllowed(allowed, asset)
	})
	omitted, omittedDropped := gatewayKeep(page.NotIncluded, false, func(row graph.NotIncludedAsset) bool {
		return gatewayAssetAllowed(allowed, row.AssetID)
	})
	relations, relationsDropped := gatewayKeep(page.Relations, false, func(row graph.CutRelation) bool {
		return !gatewayRelationHidden(allowed, index, row.AssetID, row.From, row.To)
	})
	frontier, frontierDropped := gatewayKeep(page.Frontier, false, func(row graph.FrontierEntry) bool {
		return !gatewayEndpointHidden(allowed, index, row.ResourceID)
	})
	entry, entryDropped := gatewayScopeEntry(page.Entry, allowed, index)
	if !resourcesDropped && !membersDropped && !excludedDropped && !omittedDropped && !relationsDropped && !frontierDropped && !entryDropped {
		return page
	}
	page.Resources = resources
	page.Members = members
	page.Excluded = excluded
	page.NotIncluded = omitted
	page.Relations = relations
	page.Frontier = frontier
	page.Entry = entry
	if membersDropped || excludedDropped || omittedDropped {
		page.Completeness = graph.CutPartial
	}
	return page
}

func gatewayFindingHidden(finding graph.CutFinding, allowed map[string]struct{}, index map[semreg.ResourceID]semreg.AssetID) bool {
	for _, asset := range finding.Assets {
		if !gatewayAssetAllowed(allowed, asset) {
			return true
		}
	}
	for _, ref := range finding.Resources {
		if !gatewayAssetAllowed(allowed, ref.AssetID) || gatewayEndpointHidden(allowed, index, ref.ResourceID) {
			return true
		}
	}
	if finding.Relation != nil && gatewayRelationHidden(allowed, index, finding.Relation.AssetID, finding.Relation.From, finding.Relation.To) {
		return true
	}
	if finding.Absence != nil && (!gatewayAssetAllowed(allowed, finding.Absence.AssetID) || gatewayEndpointHidden(allowed, index, finding.Absence.Resource)) {
		return true
	}
	return false
}

func gatewayScopeLens(result graph.LensResult, allowed map[string]struct{}, index map[semreg.ResourceID]semreg.AssetID) graph.LensResult {
	if allowed == nil {
		return result
	}
	resources, resourcesDropped := gatewayKeep(result.Resources, true, func(row graph.LensResource) bool {
		return gatewayAssetAllowed(allowed, row.AssetID) && !gatewayEndpointHidden(allowed, index, row.Resource.ResourceID)
	})
	relations, relationsDropped := gatewayKeep(result.Relations, false, func(row graph.LensRelation) bool {
		return !gatewayRelationHidden(allowed, index, row.AssetID, row.From, row.To)
	})
	absences, absencesDropped := gatewayKeep(result.Absences, false, func(row semreg.RelationAbsence) bool {
		return !gatewayEndpointHidden(allowed, index, row.Resource)
	})
	facts, factsDropped := gatewayKeep(result.Facts, false, func(row graph.LensFact) bool {
		return gatewayAssetAllowed(allowed, row.AssetID) && !gatewayEndpointHidden(allowed, index, row.Subject)
	})
	services, servicesDropped := gatewayKeep(result.Services, false, func(row semreg.ServiceInstance) bool {
		if !gatewayAssetAllowed(allowed, row.AssetID) {
			return false
		}
		return row.Resource == nil || !gatewayEndpointHidden(allowed, index, *row.Resource)
	})
	capabilities, capabilitiesDropped := gatewayKeep(result.Capabilities, false, func(row semreg.CapabilityInstance) bool {
		return gatewayAssetAllowed(allowed, row.AssetID)
	})
	findings, findingsDropped := gatewayKeep(result.Findings, false, func(row graph.CutFinding) bool {
		return !gatewayFindingHidden(row, allowed, index)
	})
	frontier, frontierDropped := gatewayKeep(result.Frontier, false, func(row graph.FrontierEntry) bool {
		return !gatewayEndpointHidden(allowed, index, row.ResourceID)
	})
	members, membersDropped := gatewayKeep(result.Cut.Members, true, func(row graph.CutMember) bool {
		return gatewayAssetAllowed(allowed, row.AssetID)
	})
	omitted, omittedDropped := gatewayKeep(result.Cut.NotIncluded, false, func(row graph.NotIncludedAsset) bool {
		return gatewayAssetAllowed(allowed, row.AssetID)
	})
	entry, entryDropped := gatewayScopeEntry(result.Entry, allowed, index)
	dropped := resourcesDropped || relationsDropped || absencesDropped || factsDropped || servicesDropped || capabilitiesDropped || findingsDropped || frontierDropped || membersDropped || omittedDropped || entryDropped
	if !dropped {
		return result
	}
	result.Resources = resources
	result.Relations = relations
	result.Absences = absences
	result.Facts = facts
	result.Services = services
	result.Capabilities = capabilities
	result.Findings = findings
	result.Frontier = frontier
	result.Cut.Members = members
	result.Cut.NotIncluded = omitted
	result.Entry = entry
	if membersDropped || omittedDropped {
		result.Cut.Completeness = graph.CutPartial
	}
	return result
}
