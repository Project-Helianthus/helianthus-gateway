package main

import (
	"strings"
	"testing"
)

func TestSemanticMetricServedCutMergeBySeries(t *testing.T) {
	const declarations = `# TYPE helianthus_semantic_fact_value gauge
# TYPE helianthus_semantic_evidence_age gauge
# TYPE helianthus_semantic_open_conflicts gauge
# TYPE helianthus_semantic_render_overflow gauge
`
	const transport = `helianthus_semantic_fact_value{fact="temperature",resource="synthetic\"\\} west\nfixture"} 42
helianthus_semantic_evidence_age{fact="temperature",resource="synthetic\"\\} west\nfixture"} 7
helianthus_semantic_render_overflow 1
`
	const overlap = `helianthus_semantic_fact_value{fact="temperature",resource="synthetic\"\\} west\nfixture"} 43
helianthus_semantic_evidence_age{resource="synthetic\"\\} west\nfixture",fact="temperature"} 9
helianthus_semantic_render_overflow 2
`
	// Each line changes one part of the transport fact-value identity. The
	// corresponding key mutation would incorrectly drop that line:
	//   - omit the metric name
	//   - omit the name of the fact label
	//   - omit the name of the resource label
	//   - omit the value of the fact label
	//   - omit the value of the resource label (or stop it at the brace,
	//     or conflate an escaped newline with a literal backslash and n)
	const novel = `helianthus_semantic_open_conflicts{fact="temperature",resource="synthetic\"\\} west\nfixture"} 0
helianthus_semantic_fact_value{field="temperature",resource="synthetic\"\\} west\nfixture"} 44
helianthus_semantic_fact_value{fact="temperature",subject="synthetic\"\\} west\nfixture"} 45
helianthus_semantic_fact_value{fact="pressure",resource="synthetic\"\\} west\nfixture"} 46
helianthus_semantic_fact_value{fact="temperature",resource="synthetic\"\\} west\\nfixture"} 47
`
	const suffix = `# TYPE synthetic_transport gauge
synthetic_transport 1
`
	body, cut := mergeServedCut(declarations+transport+suffix, declarations+overlap+novel)
	// Enumerate the full response independently of the production parser:
	// each expected sample denotes a distinct name-plus-label tuple, and
	// each family has exactly one TYPE line. No other lines may survive.
	want := make(map[string]bool)
	for _, line := range strings.Split(strings.TrimSuffix(declarations+transport+novel+suffix, "\n"), "\n") {
		want[line] = true
	}
	for _, line := range strings.Split(strings.TrimSuffix(body+cut, "\n"), "\n") {
		if !want[line] {
			t.Errorf("unexpected or repeated scrape line: %s", line)
		}
		delete(want, line)
	}
	for line := range want {
		t.Errorf("missing scrape line: %s", line)
	}
}
