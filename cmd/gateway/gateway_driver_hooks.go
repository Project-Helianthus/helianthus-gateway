package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/Project-Helianthus/helianthus-gateway/internal/runtimecontrol"
	"github.com/Project-Helianthus/helianthus-gateway/portal/contributionv1"
	"github.com/Project-Helianthus/helianthus-semreg/semreg/host"
	"github.com/Project-Helianthus/helianthus-semreg/semreg/runtime"
	"github.com/Project-Helianthus/helianthus-semreg/semreg/runtime/tree"
	"github.com/Project-Helianthus/helianthus-semreg/semreg/v1/driver"
)

var (
	errGatewayHookSurface  = errors.New("gateway driver hooks: surface is not open")
	errGatewayHookInstance = errors.New("gateway driver hooks: instance does not match the node")
	errGatewayHookLevel    = errors.New("gateway driver hooks: setup procedure is not on an instance or transport node")
)

// bindGatewayDriverHooks resolves the reserved hook names of a registry to
// the opened host. A nil portal leaves the portal hook unbound. The output
// hook is not bound.
func bindGatewayDriverHooks(registry *runtimecontrol.Registry, opened *gatewayHost, portal *gatewayPortalCatalogContributions) error {
	if registry == nil || opened == nil || opened.site == nil {
		return errGatewayHookSurface
	}
	return registry.BindHookSurfaces(gatewayDriverHookSurfaces(opened, portal))
}

func gatewayDriverHookSurfaces(opened *gatewayHost, portal *gatewayPortalCatalogContributions) map[runtimecontrol.HookName]runtimecontrol.Surface {
	graph := opened.site.Tree()
	surfaces := map[runtimecontrol.HookName]runtimecontrol.Surface{
		runtimecontrol.HookSemantic: gatewaySemanticSurface{site: opened.site},
		runtimecontrol.HookMCP:      gatewayMCPSurface{tree: graph},
		runtimecontrol.HookSetup:    gatewaySetupSurface{tree: graph},
		runtimecontrol.HookEvidence: gatewayEvidenceSurface{tree: graph},
	}
	if portal != nil && portal.registry != nil {
		surfaces[runtimecontrol.HookPortal] = gatewayPortalSurface{contributions: portal}
	}
	return surfaces
}

// gatewaySemanticSurface is the host runtime a registered driver publishes
// through. It is not a second publication hub.
type gatewaySemanticSurface struct {
	site *host.Site
}

func (s gatewaySemanticSurface) Hook() runtimecontrol.HookName { return runtimecontrol.HookSemantic }

// Runtime returns the host runtime the site started.
func (s gatewaySemanticSurface) Runtime() *runtime.Runtime {
	if s.site == nil {
		return nil
	}
	return s.site.Runtime()
}

// gatewayMCPSurface is the host tree whose namespace roster the MCP tools and
// the GraphQL namespace engine already read.
type gatewayMCPSurface struct {
	tree *tree.Tree
}

func (s gatewayMCPSurface) Hook() runtimecontrol.HookName { return runtimecontrol.HookMCP }

// Namespaces returns the namespaces captured from the host tree.
func (s gatewayMCPSurface) Namespaces(ctx context.Context) ([]tree.RosterEntry, error) {
	cut, err := captureHookTree(ctx, s.tree)
	if err != nil {
		return nil, err
	}
	defer func() { _ = cut.Close() }()
	return cut.Roster(), nil
}

// gatewayPortalSurface publishes a driver's manifests and actions through the
// portal contribution registry.
type gatewayPortalSurface struct {
	contributions *gatewayPortalCatalogContributions
}

func (s gatewayPortalSurface) Hook() runtimecontrol.HookName { return runtimecontrol.HookPortal }

// Publish admits one generation of manifests for the named owner.
func (s gatewayPortalSurface) Publish(owner contributionv1.DriverGeneration, manifests []contributionv1.Manifest) error {
	if s.contributions == nil {
		return errGatewayHookSurface
	}
	return s.contributions.PublishGeneration(owner, manifests)
}

// Snapshot returns the accepted and quarantined manifests.
func (s gatewayPortalSurface) Snapshot() contributionv1.RegistrySnapshot {
	if s.contributions == nil {
		return contributionv1.RegistrySnapshot{}
	}
	return s.contributions.snapshot()
}

// gatewaySetupSurface calls a native procedure on an instance or transport
// node through the host tree. A semantic operation is not a setup procedure.
type gatewaySetupSurface struct {
	tree *tree.Tree
}

func (s gatewaySetupSurface) Hook() runtimecontrol.HookName { return runtimecontrol.HookSetup }

// Dispatch prepares, calls and returns the outcome of one native procedure.
// The node must be the instance or transport named by the key. A device node
// is refused before the procedure is prepared.
func (s gatewaySetupSurface) Dispatch(ctx context.Context, key driver.MemberKey, input []byte) (tree.Outcome, error) {
	path, err := hookNode(s.tree, key, true)
	if err != nil {
		return tree.Outcome{}, err
	}
	segment, err := tree.NativeSegment(key.Namespace, key.Local)
	if err != nil {
		return tree.Outcome{}, err
	}
	method := tree.Key{Node: path, Facet: tree.KeywordMethods, Domain: tree.KeywordNative, Segment: segment}
	cut, err := captureHookTree(ctx, s.tree)
	if err != nil {
		return tree.Outcome{}, err
	}
	defer func() { _ = cut.Close() }()
	prepared, err := s.tree.Prepare(ctx, cut.ID(), tree.MethodRef(method.String()), tree.Input{Encoded: append([]byte(nil), input...)})
	if err != nil {
		return tree.Outcome{}, err
	}
	receipt, err := s.tree.Call(ctx, prepared, tree.CallOptions{})
	if err != nil {
		return tree.Outcome{}, err
	}
	return s.tree.Outcome(ctx, receipt.ID)
}

// gatewayEvidenceSurface reads a diagnostic member from the host tree.
type gatewayEvidenceSurface struct {
	tree *tree.Tree
}

func (s gatewayEvidenceSurface) Hook() runtimecontrol.HookName { return runtimecontrol.HookEvidence }

// Read reads one native attribute. The member may sit on any node level.
func (s gatewayEvidenceSurface) Read(ctx context.Context, key driver.MemberKey) (tree.Value, error) {
	path, err := hookNode(s.tree, key, false)
	if err != nil {
		return tree.Value{}, err
	}
	segment, err := tree.NativeSegment(key.Namespace, key.Local)
	if err != nil {
		return tree.Value{}, err
	}
	attr := tree.Key{Node: path, Facet: tree.KeywordAttributes, Domain: tree.KeywordNative, Segment: segment}
	cut, err := captureHookTree(ctx, s.tree)
	if err != nil {
		return tree.Value{}, err
	}
	defer func() { _ = cut.Close() }()
	return cut.Read(ctx, tree.AttrRef(attr.String()), tree.ReadOptions{})
}

func captureHookTree(ctx context.Context, graph *tree.Tree) (*tree.TreeCut, error) {
	if graph == nil {
		return nil, errGatewayHookSurface
	}
	return graph.Capture(ctx, tree.CutOptions{})
}

func hookNode(graph *tree.Tree, key driver.MemberKey, procedure bool) (tree.NodePath, error) {
	if graph == nil {
		return tree.NodePath{}, errGatewayHookSurface
	}
	if err := key.Validate(); err != nil {
		return tree.NodePath{}, err
	}
	path, err := tree.ParseNodePath("/" + string(key.Node))
	if err != nil {
		return tree.NodePath{}, err
	}
	if procedure {
		level, err := path.Level()
		if err != nil {
			return tree.NodePath{}, err
		}
		if level != tree.LevelInstance && level != tree.LevelTransport {
			return tree.NodePath{}, errGatewayHookLevel
		}
	}
	if path.Instance != key.Instance {
		return tree.NodePath{}, fmt.Errorf("%w: %s", errGatewayHookInstance, key.Instance)
	}
	return path, nil
}
