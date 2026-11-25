package catalogv1

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"

	semreg "github.com/Project-Helianthus/helianthus-semreg/semreg/v1"
	"github.com/Project-Helianthus/helianthus-semreg/semreg/v1/graph"
)

func TestPortalTargetReportAccountsForUnsupportedPresentation(t *testing.T) {
	raw, err := os.ReadFile("testdata/target-presentation.json")
	if err != nil {
		t.Fatal(err)
	}
	var expected []TargetPresentation
	if err := json.Unmarshal(raw, &expected); err != nil {
		t.Fatal(err)
	}
	entries := make([]graph.Element, 0, len(expected)+1)
	for _, row := range expected {
		ref := semreg.DefinitionRef{Pack: semreg.PackRef{ID: semreg.DefinitionID(row.Pack.ID), Version: semreg.SemanticVersion(row.Pack.Version)}, ID: semreg.DefinitionID(row.ID), Version: semreg.SemanticVersion(row.Version)}
		entries = append(entries, graph.Element{Ref: graph.PackElement(ref), ElementKind: graph.KindPresentation,
			Disposition: graph.DispositionUnsupported, Reason: "catalog.reason.target_presentation", Presentation: &graph.PresentationAttributes{Target: graph.TargetPortal}})
	}
	other := entries[0]
	other.Presentation = &graph.PresentationAttributes{Target: graph.TargetProjectionVocabulary}
	entries = append(entries, other)
	report := ReadTargetReport(entries)
	if report.TargetID != PortalTarget || !reflect.DeepEqual(report.Descriptors, expected) {
		t.Fatalf("target report has %d descriptors; expected %d exact pack presentation descriptors", len(report.Descriptors), len(expected))
	}
	catalog, err := graph.Embedded()
	if err != nil {
		t.Fatal(err)
	}
	actual := ReadTargetReport(catalog.Elements())
	for _, entry := range catalog.Elements() {
		if entry.ElementKind == graph.KindPresentation && entry.Presentation != nil && entry.Presentation.Target == graph.TargetPortal &&
			(entry.Disposition != graph.DispositionUnsupported || len(entry.Planes) != 0) {
			t.Fatalf("target presentation is graph content: %v", entry.Ref)
		}
	}
	if len(actual.Descriptors) < 32 {
		t.Fatalf("target report omitted catalog presentation: %d", len(actual.Descriptors))
	}
}
