import assert from "node:assert/strict";
import test from "node:test";
import { graphHarness, relation } from "./graph-fixture.mjs";

for (const [unit, kind, coefficient, exponent10, expected] of [
  ["joule", "energy", "5400000", 0, "1.5 kWh"],
  ["kelvin", "temperature_point", "27815", -2, "5 °C"],
  ["kelvin", "temperature_difference", "27815", -2, "278.15 K"],
  ["ratio", "percentage", "875", -3, "87.5 %"],
  ["pascal", "pressure", "130000", 0, "1.3 bar"],
  ["cubic_metre_per_second", "flow", "2", -4, "12 L/min"],
  ["reciprocal_second", "rotation", "375", -2, "225 rpm"],
]) test(`displays ${unit} ${kind} without losing the exact SI value`, () => {
  const { context } = graphHarness();
  const quantity = Object.freeze({ unit: `unit.${unit}`, number: Object.freeze({ coefficient, exponent10 }) });
  assert.equal(context.displayQuantity(quantity, kind), `${expected} (SI: ${coefficient}e${exponent10} unit.${unit})`);
  assert.deepEqual(quantity.number, { coefficient, exponent10 });
});

test("temperature without a quantity kind and a nonpercentage ratio keep SI", () => {
  const { context } = graphHarness();
  const number = { coefficient: "5", exponent10: -1 };
  assert.equal(context.displayQuantity({ unit: "unit.kelvin", number }), "5e-1 unit.kelvin");
  assert.equal(context.displayQuantity({ unit: "unit.ratio", number }, "custom_ratio"), "5e-1 unit.ratio");
  assert.equal(context.displayQuantity({ unit: "unit.ratio", number: { ...number, exponent10: 0 } }, "percentage"), "5e0 unit.ratio");
});

const resource = { resource_id: relation.from, state: "CURRENT", service_id: "fixture.service", capability_id: "fixture.capability", contribution_driver_id: "driver", contribution_manifest_id: "manifest", contribution_manifest_version: "1.0.0" };
const catalog = { resources: [resource], actions: [{ ...resource, id: "fixture.setup", discoverable: true, enabled: true }] };
const asset = { evaluation: { context: ["fixture.evaluation"] } };
for (const [renderer, expected] of [
  ["summary", []],
  ["field_table", ["Fields · Value · Quality"]],
  ["relationship_graph", [`${relation.from} → ${relation.to} — ${relation.definition.id} (current)`]],
  ["state_strip", ["State: CURRENT"]],
  ["timeline", ['Evaluation: ["fixture.evaluation"]']],
  ["evidence_table", ["Evidence · Provenance · Projection"]],
  ["session_controls", ["Action: fixture.setup"]],
]) test(`renders ${renderer}`, () => {
  const { context } = graphHarness();
  assert.deepEqual(Array.from(context.contributionRendererRows(renderer, resource, catalog, asset, { relations: [relation] })), expected);
});
