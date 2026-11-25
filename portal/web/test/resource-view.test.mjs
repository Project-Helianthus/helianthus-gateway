import assert from "node:assert/strict";
import test from "node:test";
import { catalog, definition, graphHarness, relation } from "./graph-fixture.mjs";

const snapshot = { asset_id: "asset:fixture", snapshot_id: "snapshot:fixture" };

test("plane starts from sites and resource traverses both ways with plane-grouped relations", async () => {
  const { context, node } = graphHarness();
  const plane = context.portalGraphSelection(catalog, "plane", "fixture.plane");
  assert.equal(plane.field, "semantic_traversal");
  assert.equal(plane.args.entry.kind, "sites");
  assert.deepEqual(Array.from(plane.args.planes), ["fixture.plane"]);
  assert.throws(() => context.portalGraphSelection(catalog, "plane", "fixture.unknown"), /unknown catalog plane/);
  const relations = [relation, { ...relation, state: "candidate" }, { ...relation, state: "conflict" }];
  const resources = [{ resource_id: "resource:extra" }];
  const result = await context.readPortalGraphView((field, args) => {
    assert.equal(field, "semantic_traversal");
    assert.equal(args.direction, "both");
    assert.equal(args.entry.kind, "resource");
    assert.equal(args.entry.resource, "resource:extra");
    return { catalog: catalog.digest, cut_id: "cut", completeness: "complete", frontier: [{ reason: "depth_limit" }], resources, relations, members: [snapshot] };
  }, catalog, "resource", "resource:extra");
  const identity = { resource_id: "resource:extra", service_id: "fixture.service", capability_id: "fixture.capability",
    contribution_driver_id: "fixture.driver", contribution_manifest_id: "fixture.manifest", contribution_manifest_version: "1.0.0" };
  const portal = { resources: [identity], actions: [
    { ...identity, source: snapshot, discoverable: true, enabled: true, id: "offered" },
    { ...identity, source: { ...snapshot, snapshot_id: "historical" }, discoverable: true, enabled: true, id: "historical" },
    { ...identity, source: snapshot, capability_id: "fixture.other", discoverable: true, enabled: true, id: "other-capability" },
  ] };
  const out = node();
  context.renderPortalGraphView(out, catalog, result, "resource", "resource:extra", portal, () => {});
  assert.match(out.text, /Partial.*Action: offered.*fixture.plane.*→.*current.*\/.*candidate.*\/.*conflicting/);
  assert.doesNotMatch(out.text, /historical|other-capability/);
  context.renderPortalGraphView(out, catalog, result, "plane", "fixture.plane", portal, () => {});
  assert.match(out.text, /fixture.plane.*Relations.*current/);
});

for (const [label, changed] of [
  ["pack ID", { pack: { ...definition.pack, id: "fixture.other" } }],
  ["pack version", { pack: { ...definition.pack, version: "2.4.2" } }],
  ["definition ID", { id: "fixture.other" }],
  ["definition version", { version: "1.1.1" }],
]) test(`resource plane grouping uses ${label}`, () => {
  const { context } = graphHarness();
  assert.equal(context.resourceRelationsByPlane(catalog, [{ ...relation, definition: { ...definition, ...changed } }]).get("fixture.plane").length, 0);
  assert.equal(context.resourceRelationsByPlane(catalog, [relation]).get("fixture.plane").length, 1);
});
