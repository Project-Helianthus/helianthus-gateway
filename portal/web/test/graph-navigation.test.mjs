import assert from "node:assert/strict";
import test from "node:test";
import { catalog, graphHarness, relation } from "./graph-fixture.mjs";

test("catalog lists installation, lens and plane navigation and renders lens resources in served order", async () => {
  const { context, node } = graphHarness();
  const nav = node(), chosen = [];
  context.renderPortalGraphNavigation(nav, catalog, (...args) => chosen.push(args));
  assert.deepEqual(nav.children.map((el) => el.textContent), ["Installation tree", "fixture.lens", "fixture.plane"]);
  for (const button of nav.children) button.click();
  assert.deepEqual(chosen, [["installation", undefined], ["lens", "fixture.lens"], ["plane", "fixture.plane"]]);
  const calls = [];
  const make = (id) => ({ asset_id: "asset:fixture", resource: { resource_id: id }, depth: 0 });
  const pages = [
    { lens: "fixture.lens", catalog: "catalog", cut: { cut_id: "cut", completeness: "partial", members: [] }, resources: [make("resource:extra")], next_page: "cursor" },
    { catalog: "catalog", cut: { cut_id: "cut", completeness: "partial", members: [] }, resources: [make("resource:site")], relations: [relation], services: [{ resource: "resource:extra", instance_id: "fixture.service" }], capabilities: [{ service_instance: "fixture.service", definition: { id: "fixture.capability" } }] },
  ];
  const result = await context.readPortalGraphView((field, args) => { calls.push([field, args]); return pages.shift(); }, catalog, "lens", "fixture.lens");
  assert.equal(calls[0][0], "semantic_lens");
  assert.equal(calls[0][1].entry.kind, "lens_roots");
  assert.equal(calls[0][1].depth, 7);
  assert.equal(calls[1][1].cut_id, "cut");
  assert.equal(calls[1][1].page_token, "cursor");
  const out = node();
  context.renderPortalGraphView(out, catalog, result, "lens", "fixture.lens", undefined, () => {});
  assert.match(out.text, /Partial.*Resource tree resource:extra resource:site.*Capability tree resource:extra fixture.capability.*current/);
});

test("installation tree nests only qualified coherent reached resources", () => {
  const { context, node } = graphHarness();
  const out = node();
  context.renderPortalGraphView(out, catalog, { resources: [
    { resource_id: "resource:site", depth: 0 }, { resource_id: "resource:extra", depth: 1 }, { resource_id: "resource:candidate", depth: 1 },
  ], relations: [relation, { ...relation, to: "resource:candidate", state: "candidate" }] }, "installation", undefined, undefined, () => {});
  const tree = out.children[0].children[1];
  assert.equal(tree.children.length, 2);
  assert.match(tree.children[0].text, /resource:site resource:extra/);
  assert.equal(tree.children[1].text, "resource:candidate");
  assert.match(out.text, /resource:site \/ resource:candidate.*candidate/);
});
