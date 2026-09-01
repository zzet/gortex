package daemon

import (
	"context"
	"testing"
)

// groupedProductionBody is the request shape production routes for a
// group_by:"file" find_usages call.
func groupedProductionBody(extra string) []byte {
	return productionBody(`{"id":"pkg/hot.go::Hot","group_by":"file"` + extra + `}`)
}

const localGroupedJSON = `{
	"grouped_by":"file","file_count":2,"total_uses":3,"truncated":false,
	"groups":[
		{"file":"l/a.go","count":2,"uses":[
			{"line":5,"edge_kind":"calls","symbol_id":"l/a.go::A1","symbol_name":"A1"},
			{"line":9,"edge_kind":"calls","symbol_id":"l/a.go::A2","symbol_name":"A2"}]},
		{"file":"shared/s.go","count":1,"uses":[
			{"line":3,"edge_kind":"references","symbol_id":"shared/s.go::S","symbol_name":"S"}]}]}`

const remoteGroupedJSON = `{
	"grouped_by":"file","file_count":2,"total_uses":3,"truncated":false,
	"groups":[
		{"file":"shared/s.go","count":2,"uses":[
			{"line":3,"edge_kind":"references","symbol_id":"shared/s.go::S","symbol_name":"S"},
			{"line":8,"edge_kind":"calls","symbol_id":"shared/s.go::S2","symbol_name":"S2"}]},
		{"file":"r/b.go","count":1,"uses":[
			{"line":7,"edge_kind":"calls","symbol_id":"r/b.go::B","symbol_name":"B"}]}]}`

// TestFederator_GroupedUsagesSurviveMerge pins group_by:"file" through
// federation: the grouped representation must merge group-wise (rows
// deduplicated, counts and totals recomputed), never round-trip
// through the flat SubGraph shape that discards every grouped field.
func TestFederator_GroupedUsagesSurviveMerge(t *testing.T) {
	remote := fakeRemote(t, fakeRemoteOpts{indexed: true, toolJSON: remoteGroupedJSON})
	out := testFederator().Augment(context.Background(), "find_usages",
		groupedProductionBody(""), envelope(localGroupedJSON),
		[]ServerEntry{{Slug: "r2", URL: remote.URL}})
	m := decodeFederated(t, out)

	if m["grouped_by"] != "file" {
		t.Fatalf("the grouped representation must survive federation, got keys %v", keysOf(m))
	}
	groups, _ := m["groups"].([]any)
	if len(groups) != 3 {
		t.Fatalf("want 3 merged file groups (l/a.go, shared/s.go, r/b.go), got %d", len(groups))
	}
	if got, _ := m["file_count"].(float64); int(got) != 3 {
		t.Errorf("file_count must count the merged groups, got %v", m["file_count"])
	}
	// The shared/s.go line-3 reference exists on both daemons: it must
	// merge to one row, so the merged total is 5, not 6.
	if got, _ := m["total_uses"].(float64); int(got) != 5 {
		t.Errorf("total_uses must count deduplicated merged rows, got %v", m["total_uses"])
	}
	for _, g := range groups {
		gm := g.(map[string]any)
		uses, _ := gm["uses"].([]any)
		if gm["file"] == "shared/s.go" && len(uses) != 2 {
			t.Errorf("shared/s.go must hold 2 deduplicated uses, got %d", len(uses))
		}
		if int(gm["count"].(float64)) != len(uses) {
			t.Errorf("group %v count must match its rows: count=%v rows=%d", gm["file"], gm["count"], len(uses))
		}
	}
}

// TestFederator_GroupedUsagesReapplyLimit pins the caller's limit on
// the merged grouped rows: each daemon capped its own page, so the
// merge must re-cap the row union once, globally, and mark the cut.
func TestFederator_GroupedUsagesReapplyLimit(t *testing.T) {
	remote := fakeRemote(t, fakeRemoteOpts{indexed: true, toolJSON: remoteGroupedJSON})
	out := testFederator().Augment(context.Background(), "find_usages",
		groupedProductionBody(`,"limit":2`), envelope(localGroupedJSON),
		[]ServerEntry{{Slug: "r2", URL: remote.URL}})
	m := decodeFederated(t, out)

	groups, _ := m["groups"].([]any)
	rows := 0
	for _, g := range groups {
		uses, _ := g.(map[string]any)["uses"].([]any)
		rows += len(uses)
	}
	if rows != 2 {
		t.Fatalf("limit:2 must cap the merged grouped rows at 2, got %d", rows)
	}
	if m["truncated"] != true {
		t.Errorf("a capped grouped merge must be marked truncated")
	}
	if got, _ := m["total_edges"].(float64); int(got) < 5 {
		t.Errorf("total_edges must keep the full merged row count as a floor, got %v", m["total_edges"])
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
