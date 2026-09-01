package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// TestFederator_BudgetTrimKeepsKeyedListOrigins pins provenance through
// a budget-trimmed keyed-list merge: search_symbols rows live under
// "results", so the origins prune must key off the surviving rows of
// whatever lists the payload carries, never off a "nodes" list the
// shape does not have.
func TestFederator_BudgetTrimKeepsKeyedListOrigins(t *testing.T) {
	rows := func(prefix string, n int) string {
		out := make([]string, 0, n)
		for i := 0; i < n; i++ {
			out = append(out, fmt.Sprintf(`{"id":"%s/f%02d.go::Sym%02d","name":"Sym%02d","padding":%q}`,
				prefix, i, i, i, strings.Repeat("x", 60)))
		}
		return strings.Join(out, ",")
	}
	local := envelope(fmt.Sprintf(`{"results":[%s],"total":20}`, rows("l", 20)))
	remote := fakeRemote(t, fakeRemoteOpts{indexed: true, toolJSON: fmt.Sprintf(`{"results":[%s],"total":20}`, rows("r", 20))})

	out := testFederator().Augment(context.Background(), "search_symbols",
		[]byte(`{"name":"search_symbols","arguments":{"query":"Sym","max_bytes":2200}}`), local,
		[]ServerEntry{{Slug: "r2", URL: remote.URL}})
	m := decodeFederated(t, out)
	if m["_truncated_by_budget"] != true {
		t.Fatalf("fixture must overflow the budget so the trim fires")
	}
	results, _ := m["results"].([]any)
	if len(results) == 0 {
		t.Fatalf("some rows must survive the trim")
	}
	origins, _ := m["origins"].(map[string]any)
	for _, r := range results {
		id, _ := r.(map[string]any)["id"].(string)
		if origins[id] == nil {
			t.Fatalf("surviving row %q lost its origins entry (origins kept: %d of %d rows)", id, len(origins), len(results))
		}
	}
}

// TestFederator_PeerWithoutTheSymbolCannotDisplaceLocalCaveat pins the
// caveat merge against the expected federation topology: most symbols
// live in one repo, so a peer that does not track it resolves nothing
// and answers an extraction-gap caveat about ITS OWN graph. That is
// not evidence about the union — the local daemon resolved the node,
// and its likely_unused classification must survive.
func TestFederator_PeerWithoutTheSymbolCannotDisplaceLocalCaveat(t *testing.T) {
	// The remote resolved nothing: empty nodes, extraction-gap caveat.
	remote := fakeRemote(t, fakeRemoteOpts{indexed: true, toolJSON: `{
		"nodes":[],"edges":[],"total_nodes":0,"total_edges":0,"truncated":false,
		"caveat":{"class":"possible_extraction_gap","message":"no symbol with this id is in the graph"}}`})
	out := testFederator().Augment(context.Background(), "find_usages",
		productionBody(`{"id":"pkg/hot.go::Hot","limit":50}`), envelope(zeroEdgeBody("likely_unused")),
		[]ServerEntry{{Slug: "r2", URL: remote.URL}})
	m := decodeFederated(t, out)
	caveat, _ := m["caveat"].(map[string]any)
	if caveat == nil || caveat["class"] != "likely_unused" {
		t.Fatalf("a peer that resolved nothing must not displace the local classification, got %v", caveat)
	}
}

// TestFederator_TierFilteredZeroRowsCarryNoCaveat pins the merge to the
// handler contract: a tier_filtered emptiness already explains itself,
// and stacking a zero-edge caveat on top asserts two contradictory
// reasons for the same empty page.
func TestFederator_TierFilteredZeroRowsCarryNoCaveat(t *testing.T) {
	local := envelope(`{
		"nodes":[{"id":"pkg/hot.go::Hot"}],"edges":[],"total_nodes":1,"total_edges":0,"truncated":false,
		"tier_filtered":{"class":"tier_filtered","edges_below_min_tier":2,"max_available_tier":"ast_inferred"}}`)
	remote := fakeRemote(t, fakeRemoteOpts{indexed: true, toolJSON: zeroEdgeBody("likely_unused")})
	out := testFederator().Augment(context.Background(), "find_usages",
		productionBody(`{"id":"pkg/hot.go::Hot","limit":50,"min_tier":"lsp_resolved"}`), local,
		[]ServerEntry{{Slug: "r2", URL: remote.URL}})
	m := decodeFederated(t, out)
	if m["tier_filtered"] == nil {
		t.Fatalf("the tier_filtered marker must survive")
	}
	if m["caveat"] != nil {
		t.Fatalf("a tier_filtered empty page must not also carry a zero-edge caveat, got %v", m["caveat"])
	}
}

// TestFederator_GroupedMergeCarriesOrigins pins provenance on the
// grouped shape: rows a peer contributed must be attributable, exactly
// as the flat merge attributes nodes.
func TestFederator_GroupedMergeCarriesOrigins(t *testing.T) {
	remote := fakeRemote(t, fakeRemoteOpts{indexed: true, toolJSON: remoteGroupedJSON})
	out := testFederator().Augment(context.Background(), "find_usages",
		groupedProductionBody(""), envelope(localGroupedJSON),
		[]ServerEntry{{Slug: "r2", URL: remote.URL}})
	m := decodeFederated(t, out)
	origins, _ := m["origins"].(map[string]any)
	if origins["r/b.go::B"] != "remote:r2" {
		t.Fatalf("a remote-contributed grouped row must carry its origin, got origins=%v", origins)
	}
	if origins["l/a.go::A1"] != "local" {
		t.Fatalf("local grouped rows must be attributed local, got origins=%v", origins)
	}
}

// TestFederator_GroupedRowsWithoutSymbolIDStayDistinct pins the grouped
// dedup key: rows whose enclosing symbol was pruned (empty symbol_id)
// but that differ in symbol_name or context are distinct references
// and must both survive.
func TestFederator_GroupedRowsWithoutSymbolIDStayDistinct(t *testing.T) {
	local := envelope(`{
		"grouped_by":"file","file_count":1,"total_uses":1,"truncated":false,
		"groups":[{"file":"s/s.go","count":1,"uses":[
			{"line":4,"edge_kind":"calls","symbol_name":"Alpha"}]}]}`)
	remote := fakeRemote(t, fakeRemoteOpts{indexed: true, toolJSON: `{
		"grouped_by":"file","file_count":1,"total_uses":1,"truncated":false,
		"groups":[{"file":"s/s.go","count":1,"uses":[
			{"line":4,"edge_kind":"calls","symbol_name":"Beta"}]}]}`})
	out := testFederator().Augment(context.Background(), "find_usages",
		groupedProductionBody(""), local, []ServerEntry{{Slug: "r2", URL: remote.URL}})
	m := decodeFederated(t, out)
	var rows int
	groups, _ := m["groups"].([]any)
	for _, g := range groups {
		uses, _ := g.(map[string]any)["uses"].([]any)
		rows += len(uses)
	}
	if rows != 2 {
		t.Fatalf("distinct rows (same site, different enclosing symbol name) must both survive, got %d", rows)
	}
}

// TestFederator_UnknownJSONShapeIsNotZeroValueMerged pins the SubGraph
// merge against shapes it does not understand: a JSON body without the
// flat nodes/edges contract (a future grouped variant on another tool)
// must pass through local-only, never round-trip into an empty
// subgraph that replaces a correct local answer.
func TestFederator_UnknownJSONShapeIsNotZeroValueMerged(t *testing.T) {
	grouped := `{"grouped_by":"file","file_count":1,"total_uses":1,"truncated":false,
		"groups":[{"file":"l/a.go","count":1,"uses":[{"line":5,"edge_kind":"calls","symbol_id":"l/a.go::A"}]}]}`
	remote := fakeRemote(t, fakeRemoteOpts{indexed: true, toolJSON: `{
		"nodes":[{"id":"r/b.go::RUse"}],"edges":[],"total_nodes":1,"total_edges":0,"truncated":false}`})
	out := testFederator().Augment(context.Background(), "get_callers",
		[]byte(`{"name":"get_callers","arguments":{"id":"pkg/hot.go::Hot","group_by":"file"}}`), envelope(grouped),
		[]ServerEntry{{Slug: "r2", URL: remote.URL}})
	var env struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(out, &env); err != nil || len(env.Content) == 0 {
		t.Fatalf("result must stay a valid envelope: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(env.Content[0].Text), &m); err != nil {
		t.Fatalf("payload must stay JSON: %v", err)
	}
	if m["grouped_by"] != "file" {
		t.Fatalf("an unmergeable JSON shape must pass through local-only, got keys %v", keysOf(m))
	}
}
