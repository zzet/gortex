package daemon

import (
	"context"
	"fmt"
	"testing"
)

// zeroEdgeBodyUnresolved renders a zero-edge find_usages tool JSON from
// a source that did NOT resolve the queried symbol: no node row, plus
// the extraction-gap caveat that source honestly reports about its own
// graph.
func zeroEdgeBodyUnresolved(caveatClass string) string {
	caveat := ""
	if caveatClass != "" {
		caveat = fmt.Sprintf(`,"caveat":{"class":%q,"message":"m"}`, caveatClass)
	}
	return fmt.Sprintf(`{"nodes":[],"edges":[],"total_nodes":0,"total_edges":0,"truncated":false%s}`, caveat)
}

// TestFederator_ZeroRowCaveatResolutionGateIsSymmetric pins the gate on
// extraction-gap caveats in BOTH source orientations: a source that
// resolved nothing answers a gap caveat about its own graph, not the
// union, and must not displace a classification from a source that
// resolved the node — whether the unresolved source is a remote or the
// local daemon itself. When no resolving source classified the
// emptiness, the gap caveat is the honest answer and survives.
func TestFederator_ZeroRowCaveatResolutionGateIsSymmetric(t *testing.T) {
	cases := []struct {
		name       string
		localJSON  string
		remoteJSON string
		want       string
	}{
		{
			"remote resolution beats local unresolved gap",
			zeroEdgeBodyUnresolved("possible_extraction_gap"),
			zeroEdgeBody("likely_unused"),
			"likely_unused",
		},
		{
			"local resolution beats remote unresolved gap",
			zeroEdgeBody("likely_unused"),
			zeroEdgeBodyUnresolved("possible_extraction_gap"),
			"likely_unused",
		},
		{
			"no source resolved: the gap caveat survives",
			zeroEdgeBodyUnresolved("possible_extraction_gap"),
			zeroEdgeBodyUnresolved(""),
			"possible_extraction_gap",
		},
		{
			// A resolving source that carried NO caveat has classified
			// nothing: it must not erase the unresolved source's honest
			// coverage warning into a bare, confident "0 usages".
			"caveat-less resolving peer does not erase the gap",
			zeroEdgeBodyUnresolved("possible_extraction_gap"),
			zeroEdgeBody(""),
			"possible_extraction_gap",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			remote := fakeRemote(t, fakeRemoteOpts{indexed: true, toolJSON: tc.remoteJSON})
			out := testFederator().Augment(context.Background(), "find_usages",
				productionBody(`{"id":"pkg/hot.go::Hot","limit":50}`), envelope(tc.localJSON),
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

// TestFederator_TierFilteredDoesNotClearAnotherSourcesCaveat pins the
// independence of cross-source uncertainty: within one response,
// tier_filtered and the zero-edge caveat are exclusive — the filter
// explains that source's own emptiness — but one source's filter
// explains nothing about a DIFFERENT source's incomplete coverage, so
// the merge must carry both.
func TestFederator_TierFilteredDoesNotClearAnotherSourcesCaveat(t *testing.T) {
	tierFilteredJSON := `{
		"nodes":[{"id":"pkg/hot.go::Hot"}],"edges":[],
		"total_nodes":1,"total_edges":0,"truncated":false,
		"tier_filtered":{"class":"tier_filtered","edges_below_min_tier":3,"max_available_tier":"ast_inferred"}}`

	cases := []struct {
		name       string
		localJSON  string
		remoteJSON string
		wantClass  string
	}{
		{"remote tier_filtered vs local coverage_incomplete", zeroEdgeBody("coverage_incomplete"), tierFilteredJSON, "coverage_incomplete"},
		{"local tier_filtered vs remote coverage_incomplete", tierFilteredJSON, zeroEdgeBody("coverage_incomplete"), "coverage_incomplete"},
		// The filter marker names edges that exist below the tier — it
		// refutes a likely_unused CONCLUSION — but an unresolved peer's
		// gap caveat is coverage UNCERTAINTY about a different graph,
		// which the filter explains nothing about.
		{"local tier_filtered vs remote unresolved gap", tierFilteredJSON, zeroEdgeBodyUnresolved("possible_extraction_gap"), "possible_extraction_gap"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			remote := fakeRemote(t, fakeRemoteOpts{indexed: true, toolJSON: tc.remoteJSON})
			out := testFederator().Augment(context.Background(), "find_usages",
				productionBody(`{"id":"pkg/hot.go::Hot","limit":50}`), envelope(tc.localJSON),
				[]ServerEntry{{Slug: "r2", URL: remote.URL}})
			m := decodeFederated(t, out)
			if _, ok := m["tier_filtered"].(map[string]any); !ok {
				t.Fatalf("the tier_filtered marker must survive the merge")
			}
			caveat, _ := m["caveat"].(map[string]any)
			if caveat == nil || caveat["class"] != tc.wantClass {
				t.Fatalf("another source's %s must not be cleared by the filter marker, got %v", tc.wantClass, m["caveat"])
			}
		})
	}
}
