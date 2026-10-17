package store_sqlite

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func openPrefixDiagnosticsFixture(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "prefix-diagnostics.sqlite"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	store.AddBatch([]*graph.Node{
		{ID: "repo/a.go::Owned", Kind: graph.KindFunction, FilePath: "repo/a.go", RepoPrefix: "repo"},
		{ID: "repo/b.go::OwnedToo", Kind: graph.KindFunction, FilePath: "repo/b.go", RepoPrefix: "repo"},
		{ID: "orphan.go::Unowned", Kind: graph.KindFunction, FilePath: "orphan.go"},
		{ID: "other/c.go::Wrong", Kind: graph.KindFunction, FilePath: "other/c.go", RepoPrefix: "repo"},
		{ID: "external::fmt", Kind: graph.KindFunction, FilePath: "external::fmt"},
		{ID: "repo/contract", Kind: "contract", FilePath: "repo/contract", RepoPrefix: "repo"},
		{ID: "repo/no-file::Synthetic", Kind: graph.KindFunction, RepoPrefix: "repo"},
	}, nil)
	return store
}

func legacyPrefixDiagnostics(t *testing.T, store *Store, sampleLimit int) graph.PrefixDiagnostics {
	t.Helper()
	var d graph.PrefixDiagnostics
	countAndSample := func(predicate string, wantSamples bool) (int, []string) {
		var count int
		if err := store.db.QueryRow(`SELECT COUNT(*) FROM nodes WHERE `+predicate+` AND view_gen = ?`, store.viewGen).Scan(&count); err != nil {
			t.Fatalf("legacy count: %v", err)
		}
		if !wantSamples || count == 0 || sampleLimit <= 0 {
			return count, nil
		}
		rows, err := store.db.Query(`SELECT id FROM nodes WHERE `+predicate+` AND view_gen = ? LIMIT ?`, store.viewGen, sampleLimit)
		if err != nil {
			t.Fatalf("legacy samples: %v", err)
		}
		defer rows.Close()
		return count, collectStringColumn(rows, sampleLimit)
	}
	d.OwnedCodeNodes, _ = countAndSample(ownedCodeNodePredicate, false)
	d.UnownedCodeNodes, d.UnownedSamples = countAndSample(unownedCodeNodePredicate, true)
	d.MisprefixedNodes, d.MisprefixedSamples = countAndSample(misprefixedNodePredicate, true)
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM nodes WHERE view_gen = ?`, store.viewGen).Scan(&d.Scanned); err != nil {
		t.Fatalf("legacy scanned: %v", err)
	}
	return d
}

func TestPrefixDiagnosticsOnePassMatchesLegacyPredicates(t *testing.T) {
	store := openPrefixDiagnosticsFixture(t)
	for _, sampleLimit := range []int{-1, 0, 1, 10} {
		got := store.PrefixDiagnostics(sampleLimit)
		want := legacyPrefixDiagnostics(t, store, sampleLimit)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("sampleLimit=%d diagnostics = %#v, want %#v", sampleLimit, got, want)
		}
	}
	got := store.PrefixDiagnostics(10)
	if got.Scanned != 7 || got.OwnedCodeNodes != 2 || got.UnownedCodeNodes != 1 || got.MisprefixedNodes != 1 {
		t.Fatalf("diagnostic partition = %#v", got)
	}
}

func TestPrefixDiagnosticsOnePassIsolatesViewGeneration(t *testing.T) {
	store := openPrefixDiagnosticsFixture(t)
	generationID, handle, err := store.BeginPayloadGeneration(context.Background(), PayloadGenerationRequest{
		OwnerKind: "dedicated_graph", GraphID: "graph-prefix", LayerID: "layer-prefix",
		CheckoutID: "wt-prefix", GenerationKind: "dirty", TreeOID: "tree-prefix", CreatedAt: 100,
	})
	if err != nil {
		t.Fatalf("BeginPayloadGeneration: %v", err)
	}
	handle.AddBatch([]*graph.Node{
		{ID: "next/a.go::Owned", Kind: graph.KindFunction, FilePath: "next/a.go", RepoPrefix: "next"},
		{ID: "next-orphan.go::Unowned", Kind: graph.KindFunction, FilePath: "next-orphan.go"},
	}, nil)
	if err := store.PublishPayloadGeneration(context.Background(), generationID, 200); err != nil {
		t.Fatalf("PublishPayloadGeneration: %v", err)
	}

	base := store.AtGeneration(0).PrefixDiagnostics(5)
	derived := store.AtGeneration(generationID).PrefixDiagnostics(5)
	if base.Scanned != 7 || base.OwnedCodeNodes != 2 || base.UnownedCodeNodes != 1 || base.MisprefixedNodes != 1 {
		t.Fatalf("base diagnostics = %#v", base)
	}
	if derived.Scanned != 2 || derived.OwnedCodeNodes != 1 || derived.UnownedCodeNodes != 1 || derived.MisprefixedNodes != 0 {
		t.Fatalf("derived diagnostics = %#v", derived)
	}
}

func TestPrefixDiagnosticsOnePassPlanHasSingleNodesAccess(t *testing.T) {
	store := openPrefixDiagnosticsFixture(t)
	rows, err := store.db.Query(`EXPLAIN QUERY PLAN `+prefixDiagnosticsCountQuery, store.viewGen)
	if err != nil {
		t.Fatalf("EXPLAIN QUERY PLAN: %v", err)
	}
	defer rows.Close()
	nodeAccesses := 0
	var details []string
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		details = append(details, detail)
		if strings.Contains(strings.ToLower(detail), "nodes") {
			nodeAccesses++
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("plan rows: %v", err)
	}
	if nodeAccesses != 1 {
		t.Fatalf("nodes accesses = %d, want 1; plan=%v", nodeAccesses, details)
	}
	if strings.Count(prefixDiagnosticsCountQuery, "SUM(CASE WHEN") != 3 || strings.Count(prefixDiagnosticsCountQuery, "FROM nodes") != 1 {
		t.Fatalf("aggregate query shape changed: %s", prefixDiagnosticsCountQuery)
	}
}

func TestPrefixDiagnosticsOnePassClosedStoreStaysQuiet(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "closed.sqlite"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := store.PrefixDiagnostics(5); !reflect.DeepEqual(got, graph.PrefixDiagnostics{}) {
		t.Fatalf("closed-store diagnostics = %#v, want zero value", got)
	}
}
