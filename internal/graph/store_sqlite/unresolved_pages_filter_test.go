package store_sqlite

import (
	"path/filepath"
	"sort"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// SourceOnly keeps one repository's own unresolved rows (bare targets from
// other repositories included in ScopeFilter are dropped), and Kinds keeps
// only the requested edge kinds.
func TestUnresolvedEdgePagesFilterBySourceRepoAndKind(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "unresolved-filter.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	nodes := []*graph.Node{
		{ID: "a/x.js::run", Kind: graph.KindFunction, Name: "run", FilePath: "a/x.js", RepoPrefix: "a"},
		{ID: "b/y.js::run", Kind: graph.KindFunction, Name: "run", FilePath: "b/y.js", RepoPrefix: "b"},
	}
	edges := []*graph.Edge{
		{From: "a/x.js::run", To: "unresolved::work", Kind: graph.EdgeCalls, FilePath: "a/x.js", Line: 1},
		{From: "a/x.js::run", To: "unresolved::Type", Kind: graph.EdgeReferences, FilePath: "a/x.js", Line: 2},
		{From: "b/y.js::run", To: "unresolved::work", Kind: graph.EdgeCalls, FilePath: "b/y.js", Line: 1},
		{From: "b/y.js::run", To: "a::unresolved::helper", Kind: graph.EdgeCalls, FilePath: "b/y.js", Line: 2},
	}
	store.AddBatch(nodes, edges)

	read := func(mutate func(*graph.UnresolvedEdgeScan)) []string {
		t.Helper()
		scan, err := store.BeginUnresolvedEdgeScan(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		mutate(&scan)
		page, err := store.ReadUnresolvedEdgePage(t.Context(), scan, 0, 64, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, e := range page.Edges {
			got = append(got, e.From+" "+string(e.Kind)+" "+e.To)
		}
		sort.Strings(got)
		return got
	}
	equal := func(name string, got, want []string) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("%s: got %v, want %v", name, got, want)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("%s: got %v, want %v", name, got, want)
			}
		}
	}

	equal("scope filter keeps bare targets of every repo", read(func(s *graph.UnresolvedEdgeScan) {
		s.ScopeAnchors, s.ScopeFilter = []string{"a"}, true
	}), []string{
		"a/x.js::run calls unresolved::work",
		"a/x.js::run references unresolved::Type",
		"b/y.js::run calls a::unresolved::helper",
		"b/y.js::run calls unresolved::work",
	})
	equal("source only", read(func(s *graph.UnresolvedEdgeScan) {
		s.ScopeAnchors, s.SourceOnly = []string{"a"}, true
	}), []string{
		"a/x.js::run calls unresolved::work",
		"a/x.js::run references unresolved::Type",
	})
	equal("source only and kind", read(func(s *graph.UnresolvedEdgeScan) {
		s.ScopeAnchors, s.SourceOnly = []string{"a"}, true
		s.Kinds = []graph.EdgeKind{graph.EdgeCalls}
	}), []string{"a/x.js::run calls unresolved::work"})
	equal("kind only", read(func(s *graph.UnresolvedEdgeScan) {
		s.Kinds = []graph.EdgeKind{graph.EdgeReferences}
	}), []string{"a/x.js::run references unresolved::Type"})
}
