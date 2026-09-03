package mcp

import (
	"fmt"
	"testing"
)

func rtNodes(n int) repoTotal { return repoTotal{nodes: n} }

// TestCappedRepoTotals_VerbatimWhenSmall: within the cap → every repo passes
// through and no _truncated marker is added.
func TestCappedRepoTotals_VerbatimWhenSmall(t *testing.T) {
	in := map[string]repoTotal{"a": rtNodes(10), "b": rtNodes(20)}
	out := cappedRepoTotals(in, 25)
	if len(out) != 2 {
		t.Fatalf("want 2 verbatim entries, got %d", len(out))
	}
	if _, truncated := out["_truncated"]; truncated {
		t.Fatal("must not truncate within cap")
	}
}

// TestCappedRepoTotals_TopNWhenLarge: above the cap (the monorepo case) → only
// the top-N repos by node count survive, plus a _truncated marker carrying the
// real counts. This is the bound that keeps gortex://stats from overflowing an
// agent's context on a many-repo monorepo.
func TestCappedRepoTotals_TopNWhenLarge(t *testing.T) {
	in := map[string]repoTotal{}
	for i := 0; i < 100; i++ {
		in[fmt.Sprintf("repo%02d", i)] = rtNodes(i) // node counts 0..99
	}
	out := cappedRepoTotals(in, 25)
	if len(out) != 26 { // 25 repos + 1 _truncated marker
		t.Fatalf("want 25 top repos + _truncated = 26 keys, got %d", len(out))
	}
	tr, ok := out["_truncated"].(map[string]any)
	if !ok {
		t.Fatal("missing _truncated marker")
	}
	if tr["total_repos"] != 100 || tr["shown"] != 25 {
		t.Fatalf("bad truncation marker: %+v", tr)
	}
	if _, ok := out["repo99"]; !ok {
		t.Error("top repo by nodes (repo99) must be retained")
	}
	if _, ok := out["repo00"]; ok {
		t.Error("smallest repo (repo00) must be dropped")
	}
}
