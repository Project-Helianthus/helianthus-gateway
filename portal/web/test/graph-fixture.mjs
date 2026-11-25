import { readFileSync } from "node:fs";
import vm from "node:vm";

export function graphHarness() {
  class Node {
    constructor(tag) { this.tag = tag; this.children = []; this.attributes = {}; }
    append(node) { this.children.push(node); }
    setAttribute(key, value) { this.attributes[key] = value; }
    replaceChildren(...nodes) { this.children = nodes; }
    addEventListener(name, callback) { this[name] = callback; }
    get text() { return [this.textContent, ...this.children.map((node) => node.text)].filter(Boolean).join(" "); }
  }
  const context = vm.createContext({ HTMLElement: class {}, customElements: { define() {} }, document: { createElement: (tag) => new Node(tag) } });
  vm.runInContext(readFileSync(new URL("../src/app.js", import.meta.url), "utf8"), context);
  return { context, node: () => new Node("root") };
}

export const definition = { pack: { id: "fixture.additional", version: "2.4.1" }, id: "fixture.relation", version: "1.1.0" };
export const catalog = { digest: "catalog", planes: [{ id: "fixture.plane" }], lenses: [{ id: "fixture.lens", max_depth: 7 }], entries: [{ element_kind: "relation", ref: { owner: { kind: "pack", pack: definition.pack }, id: definition.id, version: definition.version }, planes: ["fixture.plane"] }] };
export const relation = { definition, from: "resource:site", to: "resource:extra", state: "qualified", coherence: "coherent", in_plane: true };
