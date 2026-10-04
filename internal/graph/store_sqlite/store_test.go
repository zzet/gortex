package store_sqlite_test

import (
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graph/storetest"
)

// TestSQLiteStoreConformance runs the shared Store battery against the backend
// the daemon actually serves from.
//
// The factory must stay FILE-backed. A ":memory:" store is a single connection,
// so the reentrancy subtests — which issue a read from inside an iterator's
// yield body — would deadlock rather than fail, and a hung package run is far
// harder to diagnose than a red one.
func TestSQLiteStoreConformance(t *testing.T) {
	storetest.RunConformance(t, func(t *testing.T) graph.Store {
		dir := t.TempDir()
		s, err := store_sqlite.OpenPristineForConformance(t, filepath.Join(dir, "test.sqlite"))
		if err != nil {
			t.Fatalf("OpenPristineForConformance: %v", err)
		}
		t.Cleanup(func() { _ = s.Close() })
		return s
	}, storetest.Semantics{
		Reads: storetest.ReadsDetached,
		// repo_prefix is an ordinary column here, so the empty prefix deletes
		// the unprefixed (standalone-indexing) namespace like any other.
		EvictsEmptyRepoPrefix: true,
		// The adjacency statements carry an ORDER BY that the from/to indexes
		// serve without a sorter; the plan locks hold them to it.
		AdjacencyLineOrdered: true,
	})
}

func TestSQLiteExistingNodeIDsProjectsOnlyRequestedPresence(t *testing.T) {
	s, err := store_sqlite.Open(filepath.Join(t.TempDir(), "existing.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	s.AddBatch([]*graph.Node{
		{ID: "a", Kind: graph.KindFunction, Name: "a", Meta: map[string]any{"large": "payload"}},
		{ID: "b", Kind: graph.KindFunction, Name: "b"},
	}, nil)

	got := graph.LookupExistingNodeIDs(s, []string{"b", "missing", "a", "a", ""})
	if len(got) != 2 {
		t.Fatalf("existing IDs = %v, want exactly a and b", got)
	}
	if _, ok := got["a"]; !ok {
		t.Fatal("existing ID a was omitted")
	}
	if _, ok := got["b"]; !ok {
		t.Fatal("existing ID b was omitted")
	}
}

func TestAddNodeUpdatePreservesIncidentEdges(t *testing.T) {
	s, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	a := &graph.Node{ID: "a", Kind: graph.KindFunction, Name: "a"}
	b := &graph.Node{ID: "b", Kind: graph.KindFunction, Name: "b"}
	s.AddBatch([]*graph.Node{a, b}, []*graph.Edge{{From: "a", To: "b", Kind: graph.EdgeCalls}})
	if got := s.EdgeCount(); got != 1 {
		t.Fatalf("edge count before metadata update = %d, want 1", got)
	}

	b.Meta = map[string]any{"reach_build": uint64(7)}
	s.AddNode(b)
	if got := s.EdgeCount(); got != 1 {
		t.Fatalf("edge count after node upsert = %d, want 1", got)
	}
	if got := len(s.GetInEdges("b")); got != 1 {
		t.Fatalf("incoming edges after node upsert = %d, want 1", got)
	}
	if got := len(s.GetOutEdges("a")); got != 1 {
		t.Fatalf("outgoing edges after node upsert = %d, want 1", got)
	}
}

func TestSQLiteEdgeCandidateLookupIsPredicateShapedAndLossless(t *testing.T) {
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "candidates.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	store.AddBatch([]*graph.Node{
		{ID: "a", Kind: graph.KindFunction, Name: "a"},
		{ID: "b", Kind: graph.KindFunction, Name: "b"},
		{ID: "c", Kind: graph.KindVariable, Name: "c"},
		{ID: "d", Kind: graph.KindFunction, Name: "d"},
		{ID: "z", Kind: graph.KindFunction, Name: "z"},
	}, []*graph.Edge{
		{From: "a", To: "b", Kind: graph.EdgeCalls, FilePath: "a.go", Line: 10, Confidence: 0.5, Meta: map[string]any{"keep": "endpoint"}},
		{From: "a", To: "b", Kind: graph.EdgeReferences, FilePath: "a.go", Line: 13, Confidence: 0.55, Meta: map[string]any{"keep": "endpoint-kind"}},
		{From: "a", To: "c", Kind: graph.EdgeReferences, FilePath: "a.go", Line: 11, Confidence: 0.6, Meta: map[string]any{"keep": "site"}},
		{From: "d", To: "b", Kind: graph.EdgeCalls, FilePath: "d.go", Line: 12, Confidence: 0.7, Meta: map[string]any{"keep": "any-kind"}},
		{From: "a", To: "z", Kind: graph.EdgeCalls, FilePath: "a.go", Line: 99, Confidence: 0.8, Meta: map[string]any{"keep": "unrelated"}},
	})

	candidates := graph.LookupEdgeCandidates(store,
		[]graph.EdgeEndpoint{{From: "a", To: "b"}, {From: "missing", To: "b"}},
		[]graph.EdgeSite{
			{From: "a", Line: 11, Kind: graph.EdgeReferences},
			{From: "d", Line: 12},
		})

	endpoint := candidates.Endpoint("a", "b")
	if endpoint == nil {
		// Explicit return so the dereference below is provably nil-safe
		// rather than safe only by Fatal convention.
		t.Fatal("exact endpoint candidate was not returned")
		return
	}
	if got := endpoint.Meta["keep"]; got != "endpoint" {
		t.Fatalf("endpoint metadata was not preserved: got %v", got)
	}
	if got := candidates.EndpointKind("a", "b", graph.EdgeCalls); got == nil || got.Meta["keep"] != "endpoint" {
		t.Fatalf("kind-specific call candidate was not preserved: %#v", got)
	}
	if got := candidates.EndpointKind("a", "b", graph.EdgeReferences); got == nil || got.Meta["keep"] != "endpoint-kind" {
		t.Fatalf("kind-specific reference candidate was not preserved: %#v", got)
	}
	if candidates.Endpoint("a", "z") != nil {
		t.Fatal("unrequested adjacency leaked into the endpoint result")
	}

	exactSite := candidates.Site("a", 11, graph.EdgeReferences)
	if len(exactSite) != 1 || exactSite[0].To != "c" || exactSite[0].Meta["keep"] != "site" {
		t.Fatalf("unexpected exact-site candidates: %#v", exactSite)
	}
	anySite := candidates.Site("d", 12, "")
	if len(anySite) != 1 || anySite[0].To != "b" || anySite[0].Meta["keep"] != "any-kind" {
		t.Fatalf("unexpected any-kind candidates: %#v", anySite)
	}

	// Reindexing mutates a detached candidate pointer. Endpoint performs a
	// live-field check so the pointer cannot be claimed through its stale key.
	endpoint.To = "c"
	if candidates.Endpoint("a", "b") != nil {
		// The reference edge is still live under this endpoint, so the any-kind
		// lookup may legitimately return it. The calls-specific stale key must
		// nevertheless be empty.
		if candidates.EndpointKind("a", "b", graph.EdgeCalls) != nil {
			t.Fatal("stale kind-specific endpoint bucket returned a reindexed edge")
		}
	}
}
