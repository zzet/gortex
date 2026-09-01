package daemon

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// fedUsageBodies builds a local result with nLocal edges and a remote
// result with nRemote edges (disjoint from-sites, one file per edge),
// in the shape the daemons actually serialize.
func fedUsageBodies(nLocal, nRemote int, localTruncated, remoteTruncated bool) (local []byte, remoteJSON string) {
	rows := func(prefix string, n int) (nodes, edges []string) {
		nodes = append(nodes, `{"id":"pkg/hot.go::Hot"}`)
		for i := 0; i < n; i++ {
			id := fmt.Sprintf("%s/u%02d.go::Use%02d", prefix, i, i)
			nodes = append(nodes, fmt.Sprintf(`{"id":%q}`, id))
			edges = append(edges, fmt.Sprintf(
				`{"from":%q,"to":"pkg/hot.go::Hot","kind":"calls","file_path":"%s/u%02d.go","line":5,"origin":"ast_resolved","confidence_label":"EXTRACTED"}`,
				id, prefix, i))
		}
		return nodes, edges
	}
	ln, le := rows("l", nLocal)
	rn, re := rows("r", nRemote)
	local = envelope(fmt.Sprintf(`{"nodes":[%s],"edges":[%s],"total_nodes":%d,"total_edges":%d,"truncated":%v}`,
		strings.Join(ln, ","), strings.Join(le, ","), len(ln), nLocal, localTruncated))
	remoteJSON = fmt.Sprintf(`{"nodes":[%s],"edges":[%s],"total_nodes":%d,"total_edges":%d,"truncated":%v}`,
		strings.Join(rn, ","), strings.Join(re, ","), len(rn), nRemote, remoteTruncated)
	return local, remoteJSON
}

// productionBody is the request shape the stdio and streamable MCP
// dispatch actually route: tool args nested under "arguments".
func productionBody(args string) []byte {
	return []byte(fmt.Sprintf(`{"name":"find_usages","arguments":%s}`, args))
}

// TestFederator_MergeReappliesUsageLimit_ProductionShape pins the
// global row cap through the request shape production routes: each
// daemon applied limit independently, so the merge must re-cap once,
// reading the limit from the arguments envelope. Covers omitted, 0,
// explicit small, and greater-than-default limits.
func TestFederator_MergeReappliesUsageLimit_ProductionShape(t *testing.T) {
	cases := []struct {
		name      string
		args      string
		nLocal    int
		nRemote   int
		wantEdges int
	}{
		{"explicit small limit", `{"id":"pkg/hot.go::Hot","limit":2}`, 2, 2, 2},
		{"omitted limit defaults to 50", `{"id":"pkg/hot.go::Hot"}`, 30, 30, 50},
		{"limit zero opts out", `{"id":"pkg/hot.go::Hot","limit":0}`, 30, 30, 60},
		{"limit above row count", `{"id":"pkg/hot.go::Hot","limit":120}`, 30, 30, 60},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			local, remoteJSON := fedUsageBodies(tc.nLocal, tc.nRemote, true, true)
			remote := fakeRemote(t, fakeRemoteOpts{indexed: true, toolJSON: remoteJSON})
			out := testFederator().Augment(context.Background(), "find_usages",
				productionBody(tc.args), local, []ServerEntry{{Slug: "r2", URL: remote.URL}})
			m := decodeFederated(t, out)
			edges, _ := m["edges"].([]any)
			if len(edges) != tc.wantEdges {
				t.Fatalf("want %d merged edges, got %d", tc.wantEdges, len(edges))
			}
		})
	}
}

// TestFederator_RemoteOnlyTruncationPropagates pins that a complete
// local result merged with a truncated remote page cannot claim the
// merged result is complete or its totals exact.
func TestFederator_RemoteOnlyTruncationPropagates(t *testing.T) {
	local, remoteJSON := fedUsageBodies(1, 1, false, true)
	remote := fakeRemote(t, fakeRemoteOpts{indexed: true, toolJSON: remoteJSON})
	out := testFederator().Augment(context.Background(), "find_usages",
		productionBody(`{"id":"pkg/hot.go::Hot","limit":50}`), local, []ServerEntry{{Slug: "r2", URL: remote.URL}})
	m := decodeFederated(t, out)
	if m["truncated"] != true {
		t.Errorf("remote-only truncation must propagate to the merged result")
	}
	if m["lower_bound"] != true {
		t.Errorf("the remote's discarded tail makes the merged totals a floor")
	}
}

// TestFederator_DistinctCallSitesSurviveMerge pins the merge dedup key:
// two usages of the same (from, to, kind) at different lines are
// distinct rows and must both survive.
func TestFederator_DistinctCallSitesSurviveMerge(t *testing.T) {
	remote := fakeRemote(t, fakeRemoteOpts{indexed: true, toolJSON: `{
		"nodes":[{"id":"l/a.go::LUse1"}],
		"edges":[{"from":"l/a.go::LUse1","to":"pkg/hot.go::Hot","kind":"calls","file_path":"l/a.go","line":9}],
		"total_nodes":1,"total_edges":1,"truncated":false}`})
	local := envelope(`{
		"nodes":[{"id":"l/a.go::LUse1"}],
		"edges":[{"from":"l/a.go::LUse1","to":"pkg/hot.go::Hot","kind":"calls","file_path":"l/a.go","line":5}],
		"total_nodes":1,"total_edges":1,"truncated":false}`)
	out := testFederator().Augment(context.Background(), "find_usages",
		productionBody(`{"id":"pkg/hot.go::Hot","limit":50}`), local, []ServerEntry{{Slug: "r2", URL: remote.URL}})
	m := decodeFederated(t, out)
	edges, _ := m["edges"].([]any)
	if len(edges) != 2 {
		t.Fatalf("distinct call sites (same from/to/kind, different line) must both survive, got %d", len(edges))
	}
}

// TestFederator_MergeRanksByConfidenceLabel pins the sortable rank the
// federation boundary actually carries: Edge.Confidence is excluded
// from JSON, so every peer confidence reads zero after unmarshalling
// and only confidence_label survives. With limit:1, an EXTRACTED row
// must displace an AMBIGUOUS row of the same origin even when the
// file/line tie-breakers favor the weaker row.
func TestFederator_MergeRanksByConfidenceLabel(t *testing.T) {
	remote := fakeRemote(t, fakeRemoteOpts{indexed: true, toolJSON: `{
		"nodes":[{"id":"z/strong.go::Strong"}],
		"edges":[{"from":"z/strong.go::Strong","to":"pkg/hot.go::Hot","kind":"calls","file_path":"z/strong.go","line":9,"origin":"ast_inferred","confidence_label":"EXTRACTED"}],
		"total_nodes":1,"total_edges":1,"truncated":false}`})
	local := envelope(`{
		"nodes":[{"id":"a/weak.go::Weak"}],
		"edges":[{"from":"a/weak.go::Weak","to":"pkg/hot.go::Hot","kind":"calls","file_path":"a/weak.go","line":5,"origin":"ast_inferred","confidence_label":"AMBIGUOUS"}],
		"total_nodes":1,"total_edges":1,"truncated":false}`)
	out := testFederator().Augment(context.Background(), "find_usages",
		productionBody(`{"id":"pkg/hot.go::Hot","limit":1}`), local, []ServerEntry{{Slug: "r2", URL: remote.URL}})
	m := decodeFederated(t, out)
	edges, _ := m["edges"].([]any)
	if len(edges) != 1 {
		t.Fatalf("limit:1 must keep exactly one row, got %d", len(edges))
	}
	row, _ := edges[0].(map[string]any)
	if row["from"] != "z/strong.go::Strong" {
		t.Fatalf("the EXTRACTED row must win the capped slot, got %v", row["from"])
	}
}

// TestFederator_RemoteCompletenessMetadataPropagates pins that remote
// completeness fields survive the merge: lower_bound, boundaries,
// caller notes, and the usage summary.
func TestFederator_RemoteCompletenessMetadataPropagates(t *testing.T) {
	t.Run("lower_bound and boundaries on a call chain", func(t *testing.T) {
		remote := fakeRemote(t, fakeRemoteOpts{indexed: true, toolJSON: `{
			"nodes":[{"id":"r/a.go::RFn"}],
			"edges":[],
			"total_nodes":1,"total_edges":0,"truncated":false,
			"lower_bound":true,
			"boundaries":[{"seed_id":"r/a.go::RFn","target":"handler","edge_kind":"calls","reason":"dynamic_dispatch","direction":"callees"}]}`})
		local := envelope(`{"nodes":[{"id":"l/a.go::LFn"}],"edges":[],"total_nodes":1,"total_edges":0,"truncated":false}`)
		out := testFederator().Augment(context.Background(), "get_call_chain",
			productionBody(`{"id":"pkg/hot.go::Hot"}`), local, []ServerEntry{{Slug: "r2", URL: remote.URL}})
		m := decodeFederated(t, out)
		if m["lower_bound"] != true {
			t.Errorf("a remote lower_bound result must not merge into a falsely exhaustive one")
		}
		bounds, _ := m["boundaries"].([]any)
		if len(bounds) != 1 {
			t.Errorf("remote epistemic boundaries must survive the merge, got %v", m["boundaries"])
		}
	})
	t.Run("caller notes and usage summary", func(t *testing.T) {
		remote := fakeRemote(t, fakeRemoteOpts{indexed: true, toolJSON: `{
			"nodes":[{"id":"r/a.go::RUse","meta":{"is_test":true}}],
			"edges":[{"from":"r/a.go::RUse","to":"pkg/hot.go::Hot","kind":"calls","file_path":"r/a.go","line":3,"confidence_label":"EXTRACTED"}],
			"total_nodes":1,"total_edges":1,"truncated":false,
			"caller_notes":{"r/a.go::RUse":{"sync_guarded":true}},
			"usage_summary":{"n_refs":1,"n_files":1,"n_test_refs":1}}`})
		local := envelope(`{
			"nodes":[{"id":"l/a.go::LUse"}],
			"edges":[{"from":"l/a.go::LUse","to":"pkg/hot.go::Hot","kind":"calls","file_path":"l/a.go","line":5,"confidence_label":"EXTRACTED"}],
			"total_nodes":1,"total_edges":1,"truncated":false,
			"usage_summary":{"n_refs":1,"n_files":1,"n_test_refs":0}}`)
		out := testFederator().Augment(context.Background(), "find_usages",
			productionBody(`{"id":"pkg/hot.go::Hot","limit":50}`), local, []ServerEntry{{Slug: "r2", URL: remote.URL}})
		m := decodeFederated(t, out)
		notes, _ := m["caller_notes"].(map[string]any)
		if _, ok := notes["r/a.go::RUse"]; !ok {
			t.Errorf("remote caller notes must survive the merge, got %v", m["caller_notes"])
		}
		summary, _ := m["usage_summary"].(map[string]any)
		if summary["n_refs"] != float64(2) || summary["n_test_refs"] != float64(1) {
			t.Errorf("the merged summary must describe the merged rows, got %v", m["usage_summary"])
		}
	})
}

// TestFederator_MergedSummaryKeepsWholeSetFloor pins the summary
// aggregation floor: the local rollup describes the whole local set
// (computed before the local cap), so the merged rollup can never
// report less than any source's own counts.
func TestFederator_MergedSummaryKeepsWholeSetFloor(t *testing.T) {
	remote := fakeRemote(t, fakeRemoteOpts{indexed: true, toolJSON: `{
		"nodes":[{"id":"r/a.go::RUse"}],
		"edges":[{"from":"r/a.go::RUse","to":"pkg/hot.go::Hot","kind":"calls","file_path":"r/a.go","line":3,"confidence_label":"EXTRACTED"}],
		"total_nodes":1,"total_edges":1,"truncated":false,
		"usage_summary":{"n_refs":1,"n_files":1,"n_test_refs":0}}`})
	local := envelope(`{
		"nodes":[{"id":"l/a.go::LUse"}],
		"edges":[{"from":"l/a.go::LUse","to":"pkg/hot.go::Hot","kind":"calls","file_path":"l/a.go","line":5,"confidence_label":"EXTRACTED"}],
		"total_nodes":1,"total_edges":500,"truncated":true,
		"usage_summary":{"n_refs":500,"n_files":120,"n_test_refs":40}}`)
	out := testFederator().Augment(context.Background(), "find_usages",
		productionBody(`{"id":"pkg/hot.go::Hot","limit":50}`), local, []ServerEntry{{Slug: "r2", URL: remote.URL}})
	m := decodeFederated(t, out)
	summary, _ := m["usage_summary"].(map[string]any)
	if summary["n_refs"].(float64) < 500 || summary["n_files"].(float64) < 120 || summary["n_test_refs"].(float64) < 40 {
		t.Fatalf("merged summary must never shrink below a source's whole-set counts, got %v", summary)
	}
}

// TestFederator_RemoteOnlyUsagesRefreshSummaryAndCaveat pins the
// locally-unused case: remote rows must bring a summary with them, and
// a local zero-edge caveat cannot survive a merged result that carries
// verified rows it contradicts.
func TestFederator_RemoteOnlyUsagesRefreshSummaryAndCaveat(t *testing.T) {
	remote := fakeRemote(t, fakeRemoteOpts{indexed: true, toolJSON: `{
		"nodes":[{"id":"r/a.go::RUse"},{"id":"r/b.go::RUse2"}],
		"edges":[
			{"from":"r/a.go::RUse","to":"pkg/hot.go::Hot","kind":"calls","file_path":"r/a.go","line":3,"confidence_label":"EXTRACTED","origin":"lsp_resolved"},
			{"from":"r/b.go::RUse2","to":"pkg/hot.go::Hot","kind":"calls","file_path":"r/b.go","line":4,"confidence_label":"EXTRACTED","origin":"lsp_resolved"}
		],
		"total_nodes":2,"total_edges":2,"truncated":false,
		"usage_summary":{"n_refs":2,"n_files":2,"n_test_refs":0}}`})
	local := envelope(`{
		"nodes":[],"edges":[],"total_nodes":0,"total_edges":0,"truncated":false,
		"caveat":{"class":"coverage_incomplete","message":"this symbol appears unused"}}`)
	out := testFederator().Augment(context.Background(), "find_usages",
		productionBody(`{"id":"pkg/hot.go::Hot","limit":50}`), local, []ServerEntry{{Slug: "r2", URL: remote.URL}})
	m := decodeFederated(t, out)
	summary, _ := m["usage_summary"].(map[string]any)
	if summary == nil || summary["n_refs"].(float64) < 2 {
		t.Fatalf("remote rows must carry a summary into the merged result, got %v", m["usage_summary"])
	}
	if _, stale := m["caveat"]; stale {
		t.Fatalf("a zero-edge caveat cannot survive a merge that added verified rows, got %v", m["caveat"])
	}
}

// TestFederator_DynamicBoundariesDedupAndCap pins that dynamic
// boundaries merge like their static siblings: deduplicated, so a peer
// reporting the same dispatch sites cannot duplicate the section.
func TestFederator_DynamicBoundariesDedupAndCap(t *testing.T) {
	remote := fakeRemote(t, fakeRemoteOpts{indexed: true, toolJSON: `{
		"nodes":[],"edges":[],"total_nodes":0,"total_edges":0,"truncated":false,
		"dynamic_boundaries":[
			{"site":"pkg/hot.go:42","form":"computed_member","key":"handlers[name]"},
			{"site":"pkg/hot.go:99","form":"reflection","key":"method"}
		]}`})
	local := envelope(`{
		"nodes":[],"edges":[],"total_nodes":0,"total_edges":0,"truncated":false,
		"dynamic_boundaries":[{"site":"pkg/hot.go:42","form":"computed_member","key":"handlers[name]"}]}`)
	out := testFederator().Augment(context.Background(), "find_usages",
		productionBody(`{"id":"pkg/hot.go::Hot","limit":50}`), local, []ServerEntry{{Slug: "r2", URL: remote.URL}})
	m := decodeFederated(t, out)
	bounds, _ := m["dynamic_boundaries"].([]any)
	if len(bounds) != 2 {
		t.Fatalf("duplicate dynamic boundaries must collapse, want 2 got %d", len(bounds))
	}
}

// TestFederator_CallerNotesPrunedWithCap pins that the post-merge cap
// prunes caller notes along with their nodes, so no note annotates a
// row absent from the response.
func TestFederator_CallerNotesPrunedWithCap(t *testing.T) {
	remote := fakeRemote(t, fakeRemoteOpts{indexed: true, toolJSON: `{
		"nodes":[{"id":"r/b.go::Weak"}],
		"edges":[{"from":"r/b.go::Weak","to":"pkg/hot.go::Hot","kind":"calls","file_path":"r/b.go","line":3,"confidence_label":"AMBIGUOUS"}],
		"total_nodes":1,"total_edges":1,"truncated":false,
		"caller_notes":{"r/b.go::Weak":{"sync_guarded":true}}}`})
	local := envelope(`{
		"nodes":[{"id":"l/a.go::Strong"}],
		"edges":[{"from":"l/a.go::Strong","to":"pkg/hot.go::Hot","kind":"calls","file_path":"l/a.go","line":5,"confidence_label":"EXTRACTED"}],
		"total_nodes":1,"total_edges":1,"truncated":false}`)
	out := testFederator().Augment(context.Background(), "find_usages",
		productionBody(`{"id":"pkg/hot.go::Hot","limit":1}`), local, []ServerEntry{{Slug: "r2", URL: remote.URL}})
	m := decodeFederated(t, out)
	if notes, ok := m["caller_notes"].(map[string]any); ok {
		if _, dangling := notes["r/b.go::Weak"]; dangling {
			t.Fatalf("a note must not survive the prune of its node, got %v", notes)
		}
	}
}
