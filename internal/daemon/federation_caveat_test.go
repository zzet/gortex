package daemon

import (
	"context"
	"fmt"
	"testing"
)

// zeroEdgeBody renders a zero-edge find_usages tool JSON with an
// optional caveat class, in the shape the daemons serialize.
func zeroEdgeBody(caveatClass string) string {
	caveat := ""
	if caveatClass != "" {
		caveat = fmt.Sprintf(`,"caveat":{"class":%q,"message":"m"}`, caveatClass)
	}
	return fmt.Sprintf(`{"nodes":[{"id":"pkg/hot.go::Hot"}],"edges":[],"total_nodes":1,"total_edges":0,"truncated":false%s}`, caveat)
}

// TestFederator_ZeroRowCaveatConservativePrecedence pins the caveat
// merge when every source answered zero rows: the merged caveat is the
// most conservative of the sources' classes, so a local "likely_unused"
// can never out-rank a peer's "coverage_incomplete" — uncertain removal
// advice must not read as conclusive.
func TestFederator_ZeroRowCaveatConservativePrecedence(t *testing.T) {
	cases := []struct {
		name        string
		localClass  string
		remoteClass string
		want        string
	}{
		{"remote coverage_incomplete beats local likely_unused", "likely_unused", "coverage_incomplete", "coverage_incomplete"},
		{"local coverage_incomplete survives remote likely_unused", "coverage_incomplete", "likely_unused", "coverage_incomplete"},
		{"remote-only caveat adopted", "", "coverage_incomplete", "coverage_incomplete"},
		{"extraction gap beats likely_unused", "likely_unused", "possible_extraction_gap", "possible_extraction_gap"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			remote := fakeRemote(t, fakeRemoteOpts{indexed: true, toolJSON: zeroEdgeBody(tc.remoteClass)})
			out := testFederator().Augment(context.Background(), "find_usages",
				productionBody(`{"id":"pkg/hot.go::Hot","limit":50}`), envelope(zeroEdgeBody(tc.localClass)),
				[]ServerEntry{{Slug: "r2", URL: remote.URL}})
			m := decodeFederated(t, out)
			caveat, _ := m["caveat"].(map[string]any)
			if caveat == nil {
				t.Fatalf("merged zero-row result must keep a caveat, got none")
			}
			if caveat["class"] != tc.want {
				t.Fatalf("want merged caveat class %q, got %v", tc.want, caveat["class"])
			}
		})
	}
}

// TestFederator_RemoteSuppressionMetadataSurvivesMerge pins the
// completeness counters through the merge: a peer that suppressed
// text-matched rows or holds name-only candidates makes the merged
// answer exactly as uncertain as that peer's own answer was.
func TestFederator_RemoteSuppressionMetadataSurvivesMerge(t *testing.T) {
	remoteJSON := `{
		"nodes":[{"id":"pkg/hot.go::Hot"},{"id":"r/a.go::RUse"}],
		"edges":[{"from":"r/a.go::RUse","to":"pkg/hot.go::Hot","kind":"calls","file_path":"r/a.go","line":7,"origin":"lsp_resolved"}],
		"total_nodes":2,"total_edges":1,"truncated":false,
		"text_matched_suppressed":4,"name_only_candidates":7}`
	remote := fakeRemote(t, fakeRemoteOpts{indexed: true, toolJSON: remoteJSON})
	local := envelope(`{
		"nodes":[{"id":"pkg/hot.go::Hot"},{"id":"l/b.go::LUse"}],
		"edges":[{"from":"l/b.go::LUse","to":"pkg/hot.go::Hot","kind":"calls","file_path":"l/b.go","line":5,"origin":"lsp_resolved"}],
		"total_nodes":2,"total_edges":1,"truncated":false}`)

	out := testFederator().Augment(context.Background(), "find_usages",
		productionBody(`{"id":"pkg/hot.go::Hot","limit":50}`), local,
		[]ServerEntry{{Slug: "r2", URL: remote.URL}})
	m := decodeFederated(t, out)
	if got, _ := m["text_matched_suppressed"].(float64); int(got) < 4 {
		t.Errorf("remote text_matched_suppressed must survive the merge as a floor, got %v", m["text_matched_suppressed"])
	}
	if got, _ := m["name_only_candidates"].(float64); int(got) < 7 {
		t.Errorf("remote name_only_candidates must survive the merge as a floor, got %v", m["name_only_candidates"])
	}
}

// TestFederator_RemoteTierFilteredSurvivesMerge pins the tier_filtered
// marker through the merge: a peer whose rows were emptied by the
// caller's min_tier must keep that emptiness legible as "filtered",
// not silently read as "no usages on that peer".
func TestFederator_RemoteTierFilteredSurvivesMerge(t *testing.T) {
	remoteJSON := `{
		"nodes":[{"id":"pkg/hot.go::Hot"}],"edges":[],
		"total_nodes":1,"total_edges":0,"truncated":false,
		"tier_filtered":{"class":"tier_filtered","edges_below_min_tier":3,"max_available_tier":"ast_inferred"}}`
	remote := fakeRemote(t, fakeRemoteOpts{indexed: true, toolJSON: remoteJSON})
	local := envelope(`{"nodes":[{"id":"pkg/hot.go::Hot"}],"edges":[],"total_nodes":1,"total_edges":0,"truncated":false}`)

	out := testFederator().Augment(context.Background(), "find_usages",
		productionBody(`{"id":"pkg/hot.go::Hot","limit":50,"min_tier":"lsp_resolved"}`), local,
		[]ServerEntry{{Slug: "r2", URL: remote.URL}})
	m := decodeFederated(t, out)
	tf, _ := m["tier_filtered"].(map[string]any)
	if tf == nil {
		t.Fatalf("a peer's tier_filtered marker must survive the merge")
	}
	if got, _ := tf["edges_below_min_tier"].(float64); int(got) < 3 {
		t.Errorf("edges_below_min_tier must keep the peer's floor, got %v", tf["edges_below_min_tier"])
	}
}
