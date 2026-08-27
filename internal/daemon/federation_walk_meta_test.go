package daemon

import (
	"context"
	"testing"
)

// TestFederator_RemoteWalkMetadataSurvivesMerge pins the budgeted-walk
// and freshness metadata through the subgraph merge: a peer whose walk
// stopped early (budget_hit / stopped_at_depth) makes the merged
// result exactly as incomplete as that peer's own answer was, and the
// stalest peer's last_synced is the merged answer's freshness floor.
func TestFederator_RemoteWalkMetadataSurvivesMerge(t *testing.T) {
	remoteJSON := `{
		"nodes":[{"id":"pkg/hot.go::Hot"},{"id":"r/a.go::RUse"}],
		"edges":[{"from":"r/a.go::RUse","to":"pkg/hot.go::Hot","kind":"calls","file_path":"r/a.go","line":7}],
		"total_nodes":2,"total_edges":1,"truncated":false,
		"budget_hit":true,"stopped_at_depth":2,"last_synced":"2026-08-20T10:00:00Z"}`
	local := envelope(`{
		"nodes":[{"id":"pkg/hot.go::Hot"},{"id":"l/b.go::LUse"}],
		"edges":[{"from":"l/b.go::LUse","to":"pkg/hot.go::Hot","kind":"calls","file_path":"l/b.go","line":5}],
		"total_nodes":2,"total_edges":1,"truncated":false,
		"stopped_at_depth":4,"last_synced":"2026-08-23T10:00:00Z"}`)

	remote := fakeRemote(t, fakeRemoteOpts{indexed: true, toolJSON: remoteJSON})
	out := testFederator().Augment(context.Background(), "find_usages",
		productionBody(`{"id":"pkg/hot.go::Hot","limit":50}`), local,
		[]ServerEntry{{Slug: "r2", URL: remote.URL}})
	m := decodeFederated(t, out)

	if m["budget_hit"] != true {
		t.Errorf("a peer's budget_hit must survive the merge, got %v", m["budget_hit"])
	}
	if got, _ := m["stopped_at_depth"].(float64); int(got) != 2 {
		t.Errorf("merged stopped_at_depth is the weakest source guarantee (2), got %v", m["stopped_at_depth"])
	}
	if got, _ := m["last_synced"].(string); got != "2026-08-20T10:00:00Z" {
		t.Errorf("merged last_synced is the stalest source (2026-08-20), got %v", m["last_synced"])
	}
}

// TestFederator_BudgetHitWithoutDepthClearsTheDepthClaim pins the
// unknown-depth case: a peer that reports budget_hit without a
// stopped_at_depth gives the union NO known depth guarantee, so the
// merged response must not keep claiming another source's deeper one.
func TestFederator_BudgetHitWithoutDepthClearsTheDepthClaim(t *testing.T) {
	remoteJSON := `{
		"nodes":[{"id":"pkg/hot.go::Hot"}],"edges":[],
		"total_nodes":1,"total_edges":0,"truncated":false,
		"budget_hit":true}`
	local := envelope(`{
		"nodes":[{"id":"pkg/hot.go::Hot"},{"id":"l/b.go::LUse"}],
		"edges":[{"from":"l/b.go::LUse","to":"pkg/hot.go::Hot","kind":"calls","file_path":"l/b.go","line":5}],
		"total_nodes":2,"total_edges":1,"truncated":false,
		"stopped_at_depth":4}`)

	remote := fakeRemote(t, fakeRemoteOpts{indexed: true, toolJSON: remoteJSON})
	out := testFederator().Augment(context.Background(), "find_usages",
		productionBody(`{"id":"pkg/hot.go::Hot","limit":50}`), local,
		[]ServerEntry{{Slug: "r2", URL: remote.URL}})
	m := decodeFederated(t, out)

	if m["budget_hit"] != true {
		t.Errorf("budget_hit must survive, got %v", m["budget_hit"])
	}
	if v, present := m["stopped_at_depth"]; present {
		t.Errorf("a budget-hit source with no depth makes the merged depth unknowable; got claim %v", v)
	}
}
