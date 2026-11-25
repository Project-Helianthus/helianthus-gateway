package main

import (
	"encoding/json"
	"testing"

	"github.com/Project-Helianthus/helianthus-gateway/portal/contributionv1"
	"github.com/Project-Helianthus/helianthus-semreg/semreg/v1/graph"
)

const portalFixtureLensID = "lens.first"

func newPortalFixtureContributions(t *testing.T) *gatewayPortalCatalogContributions {
	t.Helper()
	index := portalFixtureSemanticIndex(t)
	return &gatewayPortalCatalogContributions{index: index, registry: contributionv1.NewRegistry(index)}
}

func portalFixtureManifest(manifest contributionv1.Manifest) contributionv1.Manifest {
	for i := range manifest.Views {
		if manifest.Views[i].Slot == "lens" {
			manifest.Views[i].Lens = portalFixtureLensID
		}
	}
	return manifest
}

func portalFixtureSemanticIndex(t *testing.T) *gatewayPortalSemanticIndex {
	t.Helper()
	index, err := newGatewayPortalSemanticIndex()
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(index.catalog.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	document["lenses"] = json.RawMessage(`[
		{"id":"lens.first","question":"What is first?","planes":["physical"],"root_kinds":["resource.site"],"max_depth":1},
		{"id":"lens.second","question":"What is second?","planes":["physical"],"root_kinds":["resource.site"],"max_depth":1}
	]`)
	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := graph.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	return &gatewayPortalSemanticIndex{catalog: catalog, metadata: index.metadata}
}

func TestPortalCatalogSemanticIndexHasLens(t *testing.T) {
	index, err := newGatewayPortalSemanticIndex()
	if err != nil {
		t.Fatal(err)
	}
	synthetic := portalFixtureSemanticIndex(t)
	for _, id := range []string{"lens.first", "lens.second"} {
		if !synthetic.HasLens(id) {
			t.Errorf("catalog lens %q not found", id)
		}
	}
	for _, id := range []string{"lens", "lens.second.extra", "lens.SECOND", "physical", "resource.site", ""} {
		if synthetic.HasLens(id) {
			t.Errorf("non-lens %q admitted", id)
		}
	}
	for _, other := range []*gatewayPortalSemanticIndex{index, {}, nil} {
		if other.HasLens("lens.second") {
			t.Error("admitted synthetic lens outside its catalog")
		}
	}
}
