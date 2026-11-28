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
		if err := holdGatewayMetricStore(stateDir); err != nil {
			log.Printf("semantic metric store lock: %s", err.Error())
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
	return m2mgraphql.NewHandler(m2mgraphql.Config{
		AllowedAssets:          allowed,
		SemanticPVCurrent:      pv,
		SemanticStorageCurrent: storage,
		SemanticEVSECurrent:    evse,
		SemanticResources:      gatewaySemanticResourcesRead,
		SemanticTraversal:      gatewaySemanticTraversalRead,
		SemanticLens:           gatewaySemanticLensRead,
		SemanticElementCatalog: gatewaySemanticElementCatalogRead,
	})
}
