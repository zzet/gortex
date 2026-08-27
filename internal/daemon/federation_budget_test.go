package daemon

import (
	"context"
	"strings"
	"testing"
)

// TestFederator_MergedResultHonorsByteBudget pins the caller's max_bytes
// on the FINAL federated representation: each daemon budgets its own
// page independently, so the merge must reapply the caller's cap once,
// globally, over multiple peers.
func TestFederator_MergedResultHonorsByteBudget(t *testing.T) {
	local, remoteJSON := fedUsageBodies(30, 30, false, false)
	_, remote2JSON := fedUsageBodies(0, 25, false, false)
	remote2JSON = strings.ReplaceAll(remote2JSON, `"r/`, `"q/`)
	r1 := fakeRemote(t, fakeRemoteOpts{indexed: true, toolJSON: remoteJSON})
	r2 := fakeRemote(t, fakeRemoteOpts{indexed: true, toolJSON: remote2JSON})

	out := testFederator().Augment(context.Background(), "find_usages",
		productionBody(`{"id":"pkg/hot.go::Hot","limit":0,"max_bytes":600}`), local,
		[]ServerEntry{{Slug: "r1", URL: r1.URL}, {Slug: "r2", URL: r2.URL}})

	tool, _ := unwrapToolJSON(out)
	if len(tool) > 600 {
		t.Fatalf("merged response must honor the caller's max_bytes:600, got %d bytes", len(tool))
	}
	m := decodeFederated(t, out)
	if m["_truncated_by_budget"] != true {
		t.Errorf("a budget-trimmed merge must carry the same truncation meta the local budget path emits")
	}
}

// TestFederator_MergedResultHonorsTokenBudget pins the max_tokens axis
// through the merge: the token cap converts to the same byte ceiling
// the per-daemon budget layer applies (3.5 bytes/token).
func TestFederator_MergedResultHonorsTokenBudget(t *testing.T) {
	local, remoteJSON := fedUsageBodies(30, 30, false, false)
	r1 := fakeRemote(t, fakeRemoteOpts{indexed: true, toolJSON: remoteJSON})

	out := testFederator().Augment(context.Background(), "find_usages",
		productionBody(`{"id":"pkg/hot.go::Hot","limit":0,"max_tokens":200}`), local,
		[]ServerEntry{{Slug: "r1", URL: r1.URL}})

	tool, _ := unwrapToolJSON(out)
	if ceiling := int(200 * 3.5); len(tool) > ceiling {
		t.Fatalf("merged response must honor max_tokens:200 (~%d bytes), got %d bytes", ceiling, len(tool))
	}
}

// TestFederator_SourceBudgetTruncationSurvivesMerge pins that a source
// page the budget layer already trimmed cannot merge into a result that
// claims completeness: the local `_truncated_by_budget` marker does not
// survive the SubGraph round trip, so the merge must read it off the
// raw source bytes.
func TestFederator_SourceBudgetTruncationSurvivesMerge(t *testing.T) {
	local := envelope(`{
		"nodes":[{"id":"pkg/hot.go::Hot"},{"id":"l/a.go::LUse"}],
		"edges":[{"from":"l/a.go::LUse","to":"pkg/hot.go::Hot","kind":"calls","file_path":"l/a.go","line":5}],
		"total_nodes":2,"total_edges":9,"truncated":false,
		"_truncated_by_budget":true,"_max_returned_edges":1,"_original_count_edges":9}`)
	remote := fakeRemote(t, fakeRemoteOpts{indexed: true, toolJSON: `{
		"nodes":[{"id":"r/b.go::RUse"}],
		"edges":[{"from":"r/b.go::RUse","to":"pkg/hot.go::Hot","kind":"calls","file_path":"r/b.go","line":7}],
		"total_nodes":1,"total_edges":1,"truncated":false}`})

	out := testFederator().Augment(context.Background(), "find_usages",
		productionBody(`{"id":"pkg/hot.go::Hot","limit":50}`), local,
		[]ServerEntry{{Slug: "r2", URL: remote.URL}})
	m := decodeFederated(t, out)
	if m["truncated"] != true {
		t.Errorf("a source page trimmed by its own budget makes the merged row set incomplete")
	}
	if m["lower_bound"] != true {
		t.Errorf("a budget-trimmed source's discarded tail makes the merged totals a floor")
	}
}
