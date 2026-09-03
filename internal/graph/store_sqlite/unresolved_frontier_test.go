package store_sqlite

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestSQLiteUnresolvedFrontierIsOneGroupedIndexedQuery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "frontier.sqlite")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}

	nodes := make([]*graph.Node, 12)
	for i := range nodes {
		nodes[i] = &graph.Node{
			ID:         fmt.Sprintf("source-%d", i),
			Kind:       graph.KindFunction,
			Name:       fmt.Sprintf("source%d", i),
			RepoPrefix: "repo",
			Language:   "go",
		}
	}
	edges := []*graph.Edge{
		{From: "source-0", To: "unresolved::Run", Kind: graph.EdgeCalls, FilePath: "a.go", Line: 1},
		{From: "source-1", To: "repo::unresolved::Run", Kind: graph.EdgeCalls, FilePath: "b.go", Line: 2},
		{From: "source-2", To: "unresolved::*.Serve", Kind: graph.EdgeReferences, FilePath: "c.go", Line: 3},
		{From: "source-3", To: "unresolved::import::example/mod", Kind: graph.EdgeImports, FilePath: "d.go", Line: 4},
		{From: "source-4", To: "unresolved::pyrel::pkg/util", Kind: graph.EdgeImports, FilePath: "e.py", Line: 5},
		{From: "source-5", To: "unresolved::grpc::Greeter::Hello", Kind: graph.EdgeCalls, FilePath: "f.go", Line: 6},
		{From: "source-6", To: "unresolved::razor_using::App.Models", Kind: graph.EdgeImports, FilePath: "g.razor", Line: 7},
		{From: "source-7", To: "unresolved::rust::crate::Thing", Kind: graph.EdgeReferences, FilePath: "h.rs", Line: 8},
		{From: "source-8", To: "unresolved::", Kind: graph.EdgeReferences, FilePath: "i.go", Line: 9},
		// fnvalue placeholders have a dedicated resolver and are excluded from
		// the generic frontier by the same predicate as the page reader.
		{From: "source-9", To: "unresolved::fnvalue::handler", Kind: graph.EdgeCalls, FilePath: "j.c", Line: 10},
		{From: "source-10", To: "repo::unresolved::fnvalue::handler", Kind: graph.EdgeCalls, FilePath: "k.c", Line: 11},
		// A resolved edge must never appear in the frontier.
		{From: "source-11", To: "source-0", Kind: graph.EdgeCalls, FilePath: "l.go", Line: 12},
	}
	store.AddBatch(nodes, edges)

	assertFrontier := func(store *Store) {
		t.Helper()
		stats, err := store.CountUnresolvedFrontier()
		if err != nil {
			t.Fatal(err)
		}
		if stats.QueryCount != 1 {
			t.Fatalf("QueryCount = %d, want exactly one grouped query", stats.QueryCount)
		}
		if stats.Pending != 9 {
			t.Fatalf("Pending = %d, want 9", stats.Pending)
		}
		if stats.GroupCount != 8 {
			t.Fatalf("GroupCount = %d, want 8", stats.GroupCount)
		}

		got := make(map[string]int64, len(stats.Buckets))
		for _, bucket := range stats.Buckets {
			got[string(bucket.Kind)+"/"+string(bucket.TargetClass)] = bucket.Count
		}
		want := map[string]int64{
			"calls/bare_symbol":           2,
			"calls/grpc":                  1,
			"imports/import":              1,
			"imports/razor_using":         1,
			"imports/relative_import":     1,
			"references/empty":            1,
			"references/qualified_symbol": 1,
			"references/wildcard_member":  1,
		}
		if len(got) != len(want) {
			t.Fatalf("buckets = %#v, want %#v", got, want)
		}
		for key, wantCount := range want {
			if got[key] != wantCount {
				t.Fatalf("bucket %q = %d, want %d (all=%#v)", key, got[key], wantCount, got)
			}
		}
	}

	assertFrontier(store)

	baseSource, baseGeneration := store.unresolvedScanSource(unresolvedFrontierBaseSource)
	plan := strings.ToLower(sqliteExplainPlan(t, store.db,
		unresolvedFrontierSQL(baseSource, baseGeneration), baseViewGeneration))
	if !strings.Contains(plan, "edges_by_unresolved") {
		t.Fatalf("frontier query did not use unresolved index:\n%s", plan)
	}
	if strings.Contains(plan, "nodes") {
		t.Fatalf("frontier query touched nodes:\n%s", plan)
	}
	edgeAccesses := strings.Count(plan, "search edges") + strings.Count(plan, "scan edges")
	if edgeAccesses != 1 {
		t.Fatalf("frontier query has %d edge-table accesses, want one:\n%s", edgeAccesses, plan)
	}

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	assertFrontier(store)
}

func TestSQLiteUnresolvedFrontierEmptyStoreStillUsesOneQuery(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "empty.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	stats, err := store.CountUnresolvedFrontier()
	if err != nil {
		t.Fatal(err)
	}
	if stats.QueryCount != 1 || stats.Pending != 0 || stats.GroupCount != 0 || len(stats.Buckets) != 0 {
		t.Fatalf("empty stats = %#v, want one query and no frontier", stats)
	}
}

func TestSQLiteUnresolvedFrontierCapsBucketsButKeepsExactTotals(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "bounded.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const extraGroups = 17
	edges := make([]*graph.Edge, 0, unresolvedFrontierBucketLimit+extraGroups)
	for i := 0; i < unresolvedFrontierBucketLimit+extraGroups; i++ {
		edges = append(edges, &graph.Edge{
			From:     "source",
			To:       "unresolved::target",
			Kind:     graph.EdgeKind(fmt.Sprintf("telemetry_kind_%03d", i)),
			FilePath: "x.go",
			Line:     i + 1,
		})
	}
	store.AddBatch([]*graph.Node{{
		ID: "source", Kind: graph.KindFunction, Name: "source", Language: "go",
	}}, edges)

	stats, err := store.CountUnresolvedFrontier()
	if err != nil {
		t.Fatal(err)
	}
	wantTotal := unresolvedFrontierBucketLimit + extraGroups
	if stats.QueryCount != 1 {
		t.Fatalf("QueryCount = %d, want 1", stats.QueryCount)
	}
	if stats.Pending != int64(wantTotal) {
		t.Fatalf("Pending = %d, want %d", stats.Pending, wantTotal)
	}
	if stats.GroupCount != wantTotal {
		t.Fatalf("GroupCount = %d, want %d", stats.GroupCount, wantTotal)
	}
	if len(stats.Buckets) != unresolvedFrontierBucketLimit {
		t.Fatalf("returned buckets = %d, want cap %d", len(stats.Buckets), unresolvedFrontierBucketLimit)
	}
}
