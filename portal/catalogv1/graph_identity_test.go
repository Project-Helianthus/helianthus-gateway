package catalogv1

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/Project-Helianthus/helianthus-gateway/portal/contributionv1"
)

func TestLensResourceIdentityAndAdditionalCatalogDomain(t *testing.T) {
	var fixture struct {
		Pack      contributionv1.PackRef `json:"pack"`
		Resources []struct {
			ResourceID string `json:"resource_id"`
			AssetID    string `json:"asset_id"`
		} `json:"resources"`
	}
	read := func(path string, value any) {
		t.Helper()
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, value); err != nil {
			t.Fatal(err)
		}
	}
	read("testdata/additional-domain.json", &fixture)
	pack := fixture.Pack
	manifest, index := actionManifestAndIndexForPack(pack)
	first, second := actionResource(), actionResource()
	first.Domain, second.Domain = pack.ID, pack.ID
	first.ID, first.Source.AssetID = fixture.Resources[0].ResourceID, fixture.Resources[0].AssetID
	second.ID, second.Source.AssetID = fixture.Resources[1].ResourceID, fixture.Resources[1].AssetID
	for _, resource := range []*Resource{&first, &second} {
		resource.State = "CURRENT"
		resource.Source.SnapshotID, resource.Source.Revision = "snapshot:fixture", "revision"
		resource.Source.BindingID, resource.Source.SourceEpoch = "binding", "epoch"
		resource.Source.EvaluationDigest, resource.Source.DriverGeneration = "evaluation", 1
		resource.Source.Snapshot, resource.Source.Evaluation, resource.Source.Projection = json.RawMessage(`{}`), json.RawMessage(`{}`), json.RawMessage(`{}`)
		resource.Source.Selections = json.RawMessage(`[]`)
	}
	membership := map[string][]LensMember{"fixture.lens": {
		{ResourceID: first.ID, AssetID: first.Source.AssetID},
		{ResourceID: second.ID, AssetID: second.Source.AssetID},
	}}
	source := &sourceFake{domains: append(append([]contributionv1.PackRef{}, FivePacks...), pack), members: membership, resources: []Resource{first, second}}
	composer := New(index, source, &invokeFake{})
	if err := composer.Publish(manifest); err != nil {
		t.Fatal(err)
	}
	catalog, err := composer.Catalog()
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Domains) != 6 || catalog.Domains[0] != (Domain{Pack: pack, SourceState: "PRESENT"}) ||
		len(catalog.Resources) != 2 || catalog.Resources[0].ID != first.ID || catalog.Resources[1].ID != second.ID ||
		len(catalog.Actions) != 2 || catalog.Actions[0].ResourceID != first.ID || catalog.Actions[1].ResourceID != second.ID {
		t.Fatalf("incorrect resource identity or source state: %+v", catalog)
	}
	wire, err := CanonicalJSON(catalog)
	if err != nil {
		t.Fatal(err)
	}
	var schema, value map[string]any
	read("../../docs/schemas/portal-catalog-v1.schema.json", &schema)
	if err := json.Unmarshal(wire, &value); err != nil {
		t.Fatal(err)
	}
	if err := validateCatalogSchema(schema, value); err != nil {
		t.Fatalf("composer output failed schema validation: %v", err)
	}
	var negatives []struct {
		Name       string `json:"name"`
		ResourceID string `json:"resource_id"`
		Expect     string `json:"expect"`
	}
	read("testdata/catalog-negative-vectors.json", &negatives)
	for _, negative := range negatives {
		if negative.ResourceID == "" {
			continue
		}
		t.Run(negative.Name, func(t *testing.T) {
			source.resources[0].ID = negative.ResourceID
			defer func() { source.resources[0] = first }()
			if _, err := composer.Catalog(); err == nil || !strings.Contains(err.Error(), negative.Expect) {
				t.Fatalf("unknown ResourceID: %v", err)
			}
		})
	}
	t.Run("wrong asset", func(t *testing.T) {
		source.resources[0].Source.AssetID = second.Source.AssetID
		defer func() { source.resources[0] = first }()
		if _, err := composer.Catalog(); err == nil || !strings.Contains(err.Error(), "invalid detached resource_id") {
			t.Fatalf("resource admitted with a different lens member's asset: %v", err)
		}
	})
	t.Run("wrong lens", func(t *testing.T) {
		source.members = map[string][]LensMember{"fixture.other": membership["fixture.lens"]}
		defer func() { source.members = membership }()
		if _, err := composer.Catalog(); err == nil || !strings.Contains(err.Error(), "invalid detached resource_id") {
			t.Fatalf("resource admitted from another lens: %v", err)
		}
	})
	t.Run("wrong pack version", func(t *testing.T) {
		source.domains[5].Version = "2.4.2"
		got, err := composer.Catalog()
		if err != nil || len(got.Resources) != 0 || got.Domains[0].SourceState != "ABSENT" {
			t.Fatalf("manifest matched a different pack version: %v, %+v", err, got)
		}
	})
}
