package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Project-Helianthus/helianthus-semreg/semreg/v1/graph"
)

const semanticGraphVectorDir = "testdata"

type semanticGraphVectors struct {
	Catalog struct {
		Module     string `json:"module"`
		Document   string `json:"document"`
		EntryCount int    `json:"entry_count"`
	} `json:"catalog"`
	Reads []struct {
		Tool      string          `json:"tool"`
		Arguments json.RawMessage `json:"arguments"`
		Data      json.RawMessage `json:"data"`
		DataFile  string          `json:"data_file"`
	} `json:"reads"`
}

func loadSemanticGraphVectors(t *testing.T) semanticGraphVectors {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(semanticGraphVectorDir, "semantic_graph_vectors.json"))
	inventoryMust(t, err)
	var vectors semanticGraphVectors
	inventoryMust(t, json.Unmarshal(raw, &vectors))
	return vectors
}

// API-3 and CAT-8: use the public catalog document shipped by the pinned
// module as an input, not the runtime registry or a recorded MCP response.
func graphVectorCatalog(t *testing.T, vectors semanticGraphVectors) ([]byte, *graph.Catalog) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "list", "-m", "-f", "{{.Dir}}", vectors.Catalog.Module)
	cmd.Env = append(os.Environ(), "GOPROXY=off", "GOSUMDB=off")
	dir, err := cmd.Output()
	if err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			t.Fatalf("resolve the public catalog module directory: %v\n%s", err, exit.Stderr)
		}
		t.Fatal(err)
	}
	moduleDir := strings.TrimSpace(string(dir))
	if moduleDir == "" {
		t.Fatal("the public catalog module is not in the module cache")
	}
	raw, err := os.ReadFile(filepath.Join(moduleDir, vectors.Catalog.Document))
	inventoryMust(t, err)
	catalog, err := graph.Decode(raw)
	inventoryMust(t, err)
	if count := len(catalog.Elements()); count != vectors.Catalog.EntryCount {
		t.Fatalf("public catalog has %d entries, vector expects %d", count, vectors.Catalog.EntryCount)
	}
	return raw, catalog
}

// Materialize the catalog record references in lexicographic object-key order
// without using the gateway response encoder. Leave catalog arrays in document order.
func graphVectorRecords(t *testing.T, records any) string {
	t.Helper()
	raw, err := json.Marshal(records)
	inventoryMust(t, err)
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var doc any
	inventoryMust(t, decoder.Decode(&doc))
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	inventoryMust(t, encoder.Encode(doc))
	return strings.TrimSuffix(out.String(), "\n")
}

// CUT-1/4/5, ENT-2, TRV-1/2/4 and API-2/3: compare the actual data bytes, not
// a decoded equivalent. The shared templates bind digests from fixture inputs;
// the lens template is the existing LRS-1 golden, relative to the vector file.
func TestSemanticGraphVectors(t *testing.T) {
	vectors := loadSemanticGraphVectors(t)
	catalogBytes, catalog := graphVectorCatalog(t, vectors)
	runtime, cuts := newSemanticGraphFixture(t)
	server := graphFixtureServer(t, runtime)
	ref := cuts.newest.Ref
	replace := strings.NewReplacer(
		`"$catalog"`, fmt.Sprintf("%q", graphFixtureDigest(string(catalogBytes))),
		`"$catalog_entries"`, graphVectorRecords(t, catalog.Elements()),
		`"$catalog_lenses"`, graphVectorRecords(t, catalog.Lenses()),
		`"$catalog_planes"`, graphVectorRecords(t, catalog.Planes()),
		`"$cut"`, fmt.Sprintf("%q", ref.CutID),
		`"$snapshot_a"`, fmt.Sprintf("%q", ref.Members[0].SnapshotID),
		`"$snapshot_b"`, fmt.Sprintf("%q", ref.Members[1].SnapshotID),
		`"$evidence"`, fmt.Sprintf("%q", graphFixtureEvidence.Digest),
	)
	remaining := map[string]bool{
		SemanticResourcesListTool: true, SemanticTraverseTool: true,
		SemanticLensGetTool: true, SemanticCatalogGetTool: true,
	}
	for _, vector := range vectors.Reads {
		t.Run(vector.Tool, func(t *testing.T) {
			if !remaining[vector.Tool] {
				t.Fatalf("unexpected or duplicate read %q", vector.Tool)
			}
			delete(remaining, vector.Tool)
			template := vector.Data
			if vector.DataFile != "" {
				var err error
				template, err = os.ReadFile(filepath.Join(semanticGraphVectorDir, vector.DataFile))
				inventoryMust(t, err)
			}
			var want bytes.Buffer
			inventoryMust(t, json.Compact(&want, []byte(replace.Replace(string(template)))))
			got := callSemanticGraph(t, server, vector.Tool, string(vector.Arguments), "")
			if !bytes.Equal(got, want.Bytes()) {
				i := 0
				for i < min(len(got), want.Len()) && got[i] == want.Bytes()[i] {
					i++
				}
				t.Fatalf("canonical data differs at byte %d:\n got …%s\nwant …%s", i,
					got[max(0, i-32):min(len(got), i+96)], want.Bytes()[max(0, i-32):min(want.Len(), i+96)])
			}
		})
	}
	if len(remaining) != 0 {
		t.Fatalf("missing read vectors: %v", remaining)
	}
}
