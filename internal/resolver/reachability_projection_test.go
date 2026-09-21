package resolver

import (
	"fmt"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

type scriptedImportProjectionStore struct {
	*resolverBatchCountingStore
	projectionCalls int
	projected       map[string][]string
	complete        bool
}

func (s *scriptedImportProjectionStore) ProjectImportAdjacency(filePaths []string) (map[string][]string, bool) {
	s.projectionCalls++
	if !s.complete {
		return nil, false
	}
	out := make(map[string][]string, len(filePaths))
	for _, filePath := range filePaths {
		out[filePath] = append([]string(nil), s.projected[filePath]...)
	}
	return out, true
}

func newReachabilityProjectionFixture(t *testing.T) (*graph.Graph, *scriptedImportProjectionStore, *Resolver, *resolveAllPassIndexes, []*graph.Edge) {
	t.Helper()
	g := graph.New()
	g.AddBatch([]*graph.Node{
		{ID: "repo/caller.go::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "repo/caller.go", RepoPrefix: "repo", Language: "go"},
		{ID: "dep/one.go", Kind: graph.KindFile, Name: "one.go", FilePath: "dep/one.go", RepoPrefix: "dep", Language: "go"},
		{ID: "dep2/two.go", Kind: graph.KindFile, Name: "two.go", FilePath: "dep2/two.go", RepoPrefix: "dep2", Language: "go"},
	}, nil)
	counting := &resolverBatchCountingStore{Store: g}
	projecting := &scriptedImportProjectionStore{
		resolverBatchCountingStore: counting,
		complete:                   true,
		projected: map[string][]string{
			"repo/caller.go": {"dep/one.go"},
		},
	}
	r := New(projecting)
	indexes := newPendingFrontierPassIndexes(r)
	pending := []*graph.Edge{{
		From: "repo/caller.go::Caller", To: graph.UnresolvedMarker + "Work",
		Kind: graph.EdgeCalls, FilePath: "repo/caller.go",
	}}
	return g, projecting, r, indexes, pending
}

func TestReachabilityProjectionReusesStableCallerAcrossPages(t *testing.T) {
	_, store, r, indexes, pending := newReachabilityProjectionFixture(t)
	defer indexes.close()
	indexes.reachabilityFiles["stale/file.go"] = map[string]struct{}{"stale": {}}

	for page := 0; page < 2; page++ {
		sources := indexes.prepare(pending)
		if err := r.warmLookupCacheWithSources(t.Context(), pending, sources); err != nil {
			t.Fatal(err)
		}
		if _, ok := r.reachableDirsByFile["repo/caller.go"]["dep"]; !ok {
			t.Fatalf("page %d missing projected dependency directory", page)
		}
		r.clearLookupCache()
		indexes.clearPage()
	}
	if store.projectionCalls != 1 {
		t.Fatalf("projection calls = %d, want 1 across overlapping pages", store.projectionCalls)
	}
	if store.getFileNodesByPathsCalls != 0 || store.getOutEdgesByNodeIDsCalls != 0 {
		t.Fatalf("legacy adjacency reads = file:%d out:%d, want 0/0",
			store.getFileNodesByPathsCalls, store.getOutEdgesByNodeIDsCalls)
	}
	if got := store.nodeIDBatchesContaining("dep/one.go"); got != 1 {
		t.Fatalf("projected target hydration batches = %d, want 1", got)
	}
	if len(indexes.reachabilityFiles) != 2 {
		t.Fatalf("stable pass cache size = %d, want 2", len(indexes.reachabilityFiles))
	}
	// Pass-scoped retention: a stable caller from an earlier page stays cached
	// (its reachability cannot change while the pass holds the resolve mutex),
	// so its reappearance pages later costs no store read.
	if _, retained := indexes.reachabilityFiles["stale/file.go"]; !retained {
		t.Fatal("pass cache dropped a stable caller from an earlier page")
	}
}

func TestReachabilityProjectionCapClearsPathologicalCache(t *testing.T) {
	_, _, r, indexes, pending := newReachabilityProjectionFixture(t)
	defer indexes.close()
	for i := 0; i <= reachabilityStableFileCap; i++ {
		indexes.reachabilityFiles[fmt.Sprintf("bulk/file%06d.go", i)] = map[string]struct{}{"bulk": {}}
	}
	if ok, _ := r.buildReachabilityIndexForPendingCached(pending, nil, indexes.reachabilityFiles, nil); !ok {
		t.Fatal("reachability build failed")
	}
	defer r.clearReachabilityIndex()
	if len(indexes.reachabilityFiles) > 2 {
		t.Fatalf("cap overflow retained %d entries, want wholesale clear", len(indexes.reachabilityFiles))
	}
}

func TestReachabilityProjectionFallsBackForMalformedProvenance(t *testing.T) {
	g := graph.New()
	g.AddBatch([]*graph.Node{
		{ID: "repo/caller.go::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "repo/caller.go"},
		{ID: "dep/target.go", Kind: graph.KindFile, Name: "target.go", FilePath: "dep/target.go"},
	}, []*graph.Edge{{
		From: "repo/caller.go::Caller", To: "dep/target.go", Kind: graph.EdgeImports, FilePath: "",
	}})
	counting := &resolverBatchCountingStore{Store: g}
	store := &scriptedImportProjectionStore{resolverBatchCountingStore: counting, complete: false}
	r := New(store)
	pending := []*graph.Edge{{
		From: "repo/caller.go::Caller", To: graph.UnresolvedMarker + "Work",
		Kind: graph.EdgeCalls, FilePath: "repo/caller.go",
	}}
	if ok, _ := r.buildReachabilityIndexForPendingCached(pending, nil, make(map[string]map[string]struct{}), nil); !ok {
		t.Fatal("reachability was not built through compatibility fallback")
	}
	defer r.clearReachabilityIndex()
	if _, ok := r.reachableDirsByFile["repo/caller.go"]["dep"]; !ok {
		t.Fatal("fallback lost the legacy imported directory")
	}
	if store.projectionCalls != 1 || store.getFileNodesByPathsCalls != 1 || store.getOutEdgesByNodeIDsCalls != 1 {
		t.Fatalf("projection/fallback reads = projection:%d file:%d out:%d, want 1/1/1",
			store.projectionCalls, store.getFileNodesByPathsCalls, store.getOutEdgesByNodeIDsCalls)
	}
}

func TestReachabilityProjectionRefreshInvalidatesPassCache(t *testing.T) {
	_, store, r, indexes, pending := newReachabilityProjectionFixture(t)

	sources := indexes.prepare(pending)
	if err := r.warmLookupCacheWithSources(t.Context(), pending, sources); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.reachableDirsByFile["repo/caller.go"]["dep"]; !ok {
		t.Fatal("initial projection missing dep")
	}
	store.projected["repo/caller.go"] = []string{"dep2/two.go"}
	if refreshed, err := indexes.refreshAfterInterleave(t.Context(), pending, true); err != nil {
		t.Fatal(err)
	} else if !refreshed {
		t.Fatal("forced refresh did not rebuild page indexes")
	}
	if store.projectionCalls != 2 {
		t.Fatalf("projection calls after refresh = %d, want 2", store.projectionCalls)
	}
	got := r.reachableDirsByFile["repo/caller.go"]
	if _, ok := got["dep2"]; !ok {
		t.Fatal("refreshed projection missing dep2")
	}
	if _, stale := got["dep"]; stale {
		t.Fatal("refreshed projection retained stale dep")
	}
	indexes.close()
	if indexes.reachabilityFiles != nil {
		t.Fatal("close retained the pass-local reachability cache")
	}
	if indexes.importAdjacency != nil {
		t.Fatal("close retained the pass-local adjacency retention")
	}
}

func TestReachabilityAdjacencyRetentionServesUnstableAcrossPages(t *testing.T) {
	_, store, r, indexes, pending := newReachabilityProjectionFixture(t)
	defer indexes.close()
	// An unresolved import keeps the caller out of the stable set — before
	// retention this file re-projected on every page it appeared on.
	store.projected["repo/caller.go"] = []string{graph.UnresolvedMarker + "import::dep"}
	adjacency := make(map[string][]string)
	for page := 0; page < 3; page++ {
		if ok, _ := r.buildReachabilityIndexForPendingCached(pending, nil, indexes.reachabilityFiles, adjacency); !ok {
			t.Fatalf("page %d build failed", page)
		}
		r.clearReachabilityIndex()
	}
	if store.projectionCalls != 1 {
		t.Fatalf("projection calls = %d, want 1: retained adjacency must serve later pages", store.projectionCalls)
	}
	if len(indexes.reachabilityFiles) != 0 {
		t.Fatal("unresolved import must still keep the file out of the stable set")
	}
	if _, ok := adjacency["repo/caller.go"]; !ok {
		t.Fatal("adjacency retention missing the projected caller")
	}
}

func TestReachabilityAdjacencyRetentionStatsCountHits(t *testing.T) {
	_, store, r, indexes, pending := newReachabilityProjectionFixture(t)
	defer indexes.close()
	store.projected["repo/caller.go"] = []string{graph.UnresolvedMarker + "import::dep"}
	adjacency := make(map[string][]string)
	if _, stats := r.buildReachabilityIndexForPendingCached(pending, nil, indexes.reachabilityFiles, adjacency); stats.adjCached != 0 {
		t.Fatalf("first page adjCached = %d, want 0", stats.adjCached)
	}
	r.clearReachabilityIndex()
	_, stats := r.buildReachabilityIndexForPendingCached(pending, nil, indexes.reachabilityFiles, adjacency)
	defer r.clearReachabilityIndex()
	if stats.adjCached != 1 || stats.missing != 1 {
		t.Fatalf("second page adjCached=%d missing=%d, want 1/1", stats.adjCached, stats.missing)
	}
}

func TestReachabilityAdjacencyRetentionSkipsFallback(t *testing.T) {
	// complete=false is a store-canonicality signal — legacy answers are
	// never retained.
	g := graph.New()
	g.AddBatch([]*graph.Node{
		{ID: "repo/caller.go::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "repo/caller.go"},
		{ID: "dep/target.go", Kind: graph.KindFile, Name: "target.go", FilePath: "dep/target.go"},
	}, []*graph.Edge{{
		From: "repo/caller.go::Caller", To: "dep/target.go", Kind: graph.EdgeImports, FilePath: "",
	}})
	counting := &resolverBatchCountingStore{Store: g}
	store := &scriptedImportProjectionStore{resolverBatchCountingStore: counting, complete: false}
	r := New(store)
	pending := []*graph.Edge{{
		From: "repo/caller.go::Caller", To: graph.UnresolvedMarker + "Work",
		Kind: graph.EdgeCalls, FilePath: "repo/caller.go",
	}}
	adjacency := make(map[string][]string)
	if ok, _ := r.buildReachabilityIndexForPendingCached(pending, nil, make(map[string]map[string]struct{}), adjacency); !ok {
		t.Fatal("fallback build failed")
	}
	defer r.clearReachabilityIndex()
	if len(adjacency) != 0 {
		t.Fatalf("fallback results retained: %v", adjacency)
	}
}

func TestReachabilityAdjacencyRetentionCapClearsWholesale(t *testing.T) {
	_, store, r, indexes, pending := newReachabilityProjectionFixture(t)
	defer indexes.close()
	store.projected["repo/caller.go"] = []string{graph.UnresolvedMarker + "import::dep"}
	adjacency := make(map[string][]string, reachabilityStableFileCap+2)
	for i := 0; i <= reachabilityStableFileCap; i++ {
		adjacency[fmt.Sprintf("bulk/file%06d.go", i)] = nil
	}
	if ok, _ := r.buildReachabilityIndexForPendingCached(pending, nil, indexes.reachabilityFiles, adjacency); !ok {
		t.Fatal("build failed")
	}
	defer r.clearReachabilityIndex()
	if len(adjacency) > 1 {
		t.Fatalf("cap overflow retained %d entries, want wholesale clear + this page's entry", len(adjacency))
	}
}

func TestNoteImportEdgeReindexesRecordsDirtyFiles(t *testing.T) {
	r := New(graph.New())
	r.noteImportEdgeReindexes([]graph.EdgeReindex{
		{Edge: &graph.Edge{Kind: graph.EdgeCalls, FilePath: "a.go", To: "x"}, OldTo: "y"},
		{Edge: &graph.Edge{Kind: graph.EdgeImports, FilePath: "b.go", To: "dep/one.go"}, OldTo: "unresolved::import::dep"},
		{Edge: nil},
	})
	if _, ok := r.importDirtyFiles["b.go"]; !ok || len(r.importDirtyFiles) != 1 {
		t.Fatalf("dirty files = %v, want exactly b.go", r.importDirtyFiles)
	}
	if r.importEdgeGen != 0 {
		t.Fatal("known-file invalidation must not bump the wholesale generation")
	}
	// A kind migration away from imports still rewrites an imports row; the
	// pre-mutation provenance names the file.
	r.noteImportEdgeReindexes([]graph.EdgeReindex{
		{Edge: &graph.Edge{Kind: graph.EdgeCalls, To: "x"}, OldTo: "x", OldKind: graph.EdgeImports, OldFilePath: "c.go"},
	})
	if _, ok := r.importDirtyFiles["c.go"]; !ok {
		t.Fatalf("dirty files = %v, want c.go from OldFilePath", r.importDirtyFiles)
	}
	// No file provenance at all → conservative wholesale clear.
	r.noteImportEdgeReindexes([]graph.EdgeReindex{
		{Edge: &graph.Edge{Kind: graph.EdgeImports, To: "dep/one.go"}, OldTo: "unresolved::import::dep"},
	})
	if r.importEdgeGen != 1 {
		t.Fatal("provenance-less imports write must fall back to the wholesale generation")
	}
}

// The pass rewrites still-unresolved import edges for bookkeeping (meta,
// confidence, terminality) on every frontier revisit. A rewrite that changes
// neither target, kind, nor file cannot change stored adjacency — it must
// NOT dirty the file, or the retention evicts exactly the unstable files it
// exists to serve.
func TestNoteImportEdgeReindexesIgnoresIdentityPreservingRewrites(t *testing.T) {
	r := New(graph.New())
	r.noteImportEdgeReindexes([]graph.EdgeReindex{
		{Edge: &graph.Edge{Kind: graph.EdgeImports, FilePath: "a.go", To: "unresolved::import::dep"},
			OldTo: "unresolved::import::dep"},
	})
	if len(r.importDirtyFiles) != 0 || r.importEdgeGen != 0 {
		t.Fatalf("identity-preserving rewrite dirtied files=%v gen=%d, want none",
			r.importDirtyFiles, r.importEdgeGen)
	}
}

func TestNoteImportTargetReindexesRecordsDirtyFiles(t *testing.T) {
	r := New(graph.New())
	r.noteImportTargetReindexes([]graph.UnresolvedEdgeTargetReindex{
		{Old: graph.EdgeIdentity{Kind: graph.EdgeCalls, FilePath: "a.go"}},
		{Old: graph.EdgeIdentity{Kind: graph.EdgeImports, FilePath: "b.go"}},
	})
	if _, ok := r.importDirtyFiles["b.go"]; !ok || len(r.importDirtyFiles) != 1 {
		t.Fatalf("dirty files = %v, want exactly b.go", r.importDirtyFiles)
	}
	if r.importEdgeGen != 0 {
		t.Fatal("known-file invalidation must not bump the wholesale generation")
	}
	r.noteImportTargetReindexes([]graph.UnresolvedEdgeTargetReindex{
		{Old: graph.EdgeIdentity{Kind: graph.EdgeImports}},
	})
	if r.importEdgeGen != 1 {
		t.Fatal("provenance-less imports identity must fall back to the wholesale generation")
	}
}

// Import edges resolve in a steady stream throughout a cold pass, so
// invalidation must be per-file — a write against an UNRELATED file must not
// evict the whole retention.
func TestReachabilityAdjacencyRetentionSurvivesUnrelatedImportWrites(t *testing.T) {
	_, store, r, indexes, pending := newReachabilityProjectionFixture(t)
	defer indexes.close()
	store.projected["repo/caller.go"] = []string{graph.UnresolvedMarker + "import::dep"}

	indexes.prepare(pending)
	indexes.clearPage()
	for page := 0; page < 3; page++ {
		r.noteImportEdgeReindexes([]graph.EdgeReindex{
			{Edge: &graph.Edge{Kind: graph.EdgeImports, FilePath: "other/unrelated.go", To: "dep/one.go"},
				OldTo: graph.UnresolvedMarker + "import::other"},
		})
		indexes.prepare(pending)
		indexes.clearPage()
	}
	if store.projectionCalls != 1 {
		t.Fatalf("projection calls = %d, want 1: unrelated import writes must not evict the retention", store.projectionCalls)
	}
}

// Renegotiated from TestReachabilityProjectionDoesNotCacheUnresolvedImports:
// adjacency for an unresolved-import caller MAY now be retained across pages —
// the pinned invariant is freshness, not absence. An imports-kind edge write
// clears the retention, and the next page re-projects and sees the newly
// resolved target.
//
// The write below carries an Edge.FilePath, so this exercises the per-file
// dirty path; importEdgeGen stays 0 throughout. The generation branch is
// covered by TestAdjacencyRetentionClearedWholesaleByProvenancelessImportWrite.
func TestReachabilityProjectionUnresolvedImportsStayFresh(t *testing.T) {
	_, store, r, indexes, pending := newReachabilityProjectionFixture(t)
	defer indexes.close()
	store.projected["repo/caller.go"] = []string{graph.UnresolvedMarker + "import::dep"}

	for page := 0; page < 2; page++ {
		indexes.prepare(pending)
		if len(indexes.reachabilityFiles) != 0 {
			t.Fatalf("page %d cached an unresolved import in the stable set", page)
		}
		indexes.clearPage()
	}
	if store.projectionCalls != 1 {
		t.Fatalf("projection calls = %d, want 1: unstable adjacency is retained across pages", store.projectionCalls)
	}

	store.projected["repo/caller.go"] = []string{"dep2/two.go"}
	r.noteImportEdgeReindexes([]graph.EdgeReindex{
		{Edge: &graph.Edge{Kind: graph.EdgeImports, FilePath: "repo/caller.go", To: "dep2/two.go"},
			OldTo: graph.UnresolvedMarker + "import::dep"},
	})
	indexes.prepare(pending)
	defer indexes.clearPage()
	if store.projectionCalls != 2 {
		t.Fatalf("projection calls = %d, want 2 after an imports-kind write", store.projectionCalls)
	}
	if _, ok := r.reachableDirsByFile["repo/caller.go"]["dep2"]; !ok {
		t.Fatal("post-write projection missing the newly resolved dep2 directory")
	}
}

// The retention's correctness depends on production write paths calling
// noteImportEdgeReindexes — but every other test in this file notes the write
// itself, from the test body. Stripping all fourteen production call sites
// leaves ./internal/resolver and ./internal/indexer green, so the wiring that
// makes the retention safe is unpinned. This drives the collector every
// attribution pass funnels through instead, so removing the note from
// persistAttributionReindexes fails here.
func TestAdjacencyRetentionInvalidatedThroughProductionAttributionWrite(t *testing.T) {
	_, store, r, indexes, pending := newReachabilityProjectionFixture(t)
	defer indexes.close()
	store.projected["repo/caller.go"] = []string{graph.UnresolvedMarker + "import::dep"}

	indexes.prepare(pending)
	indexes.clearPage()
	if store.projectionCalls != 1 {
		t.Fatalf("projection calls = %d, want 1 before the write", store.projectionCalls)
	}

	// Production path: collector.add -> flush -> persistAttributionReindexes
	// -> noteImportEdgeReindexes. No note* call in this test body.
	store.projected["repo/caller.go"] = []string{"dep2/two.go"}
	collector := newAttributionReindexCollector(r)
	collector.add(
		&graph.Edge{Kind: graph.EdgeImports, FilePath: "repo/caller.go", To: "dep2/two.go"},
		graph.UnresolvedMarker+"import::dep",
	)
	collector.flush()

	indexes.prepare(pending)
	defer indexes.clearPage()
	if store.projectionCalls != 2 {
		t.Fatalf("projection calls = %d, want 2: a production attribution write must evict the retained file", store.projectionCalls)
	}
	if _, ok := r.reachableDirsByFile["repo/caller.go"]["dep2"]; !ok {
		t.Fatal("post-write projection missing the newly resolved dep2 directory")
	}
}

// The generation branch in prepare — the wholesale clear taken when an
// imports write lands without file provenance — had no test reaching it: its
// statements profiled at zero executions. A provenance-less write leaves
// importDirtyFiles empty, so the per-file branch would delete nothing and the
// retention would survive; only the generation branch can evict it. That makes
// re-projection here an exact discriminator between the two branches.
func TestAdjacencyRetentionClearedWholesaleByProvenancelessImportWrite(t *testing.T) {
	_, store, r, indexes, pending := newReachabilityProjectionFixture(t)
	defer indexes.close()
	store.projected["repo/caller.go"] = []string{graph.UnresolvedMarker + "import::dep"}

	indexes.prepare(pending)
	indexes.clearPage()
	if store.projectionCalls != 1 {
		t.Fatalf("projection calls = %d, want 1 before the write", store.projectionCalls)
	}

	store.projected["repo/caller.go"] = []string{"dep2/two.go"}
	collector := newAttributionReindexCollector(r)
	collector.add(
		&graph.Edge{Kind: graph.EdgeImports, To: "dep2/two.go"}, // no FilePath
		graph.UnresolvedMarker+"import::dep",
	)
	collector.flush()
	if len(r.importDirtyFiles) != 0 {
		t.Fatalf("importDirtyFiles = %v, want empty: the test would not distinguish the two branches", r.importDirtyFiles)
	}
	if r.importEdgeGen == 0 {
		t.Fatal("a provenance-less imports write did not bump the generation")
	}

	indexes.prepare(pending)
	if store.projectionCalls != 2 {
		t.Fatalf("projection calls = %d, want 2: a provenance-less write must clear the retention wholesale", store.projectionCalls)
	}
	indexes.clearPage()

	// The generation is re-synced, so an unchanged pass retains again rather
	// than re-projecting on every page from here on.
	indexes.prepare(pending)
	defer indexes.clearPage()
	if store.projectionCalls != 2 {
		t.Fatalf("projection calls = %d, want 2: the retention must re-arm once the generation is re-synced", store.projectionCalls)
	}
}

// The razor pass consumes its @using markers by removing them from the graph —
// an imports-row mutation that travels RemoveEdge, not ReindexEdges. Driving
// the production pass (not the note funcs) pins that the removal invalidates
// the retention; without it the razor file's stale adjacency survives.
func TestAdjacencyRetentionInvalidatedByRazorMarkerRemoval(t *testing.T) {
	g, store, r, indexes, _ := newReachabilityProjectionFixture(t)
	defer indexes.close()
	g.AddBatch([]*graph.Node{
		{ID: "repo/View.razor", Kind: graph.KindFile, Name: "View.razor",
			FilePath: "repo/View.razor", RepoPrefix: "repo", Language: "razor"},
	}, []*graph.Edge{{
		From: "repo/View.razor", To: graph.UnresolvedMarker + "razor_using::Widgets",
		Kind: graph.EdgeImports, FilePath: "repo/View.razor", Line: 1,
	}})
	// The marker target is unresolved, so the file is never stable-cached —
	// the adjacency retention is the only thing standing between a page and
	// a re-projection, which is exactly what the removal must invalidate.
	store.projected["repo/View.razor"] = []string{graph.UnresolvedMarker + "razor_using::Widgets"}
	pending := []*graph.Edge{{
		From: "repo/View.razor::View", To: graph.UnresolvedMarker + "Work",
		Kind: graph.EdgeCalls, FilePath: "repo/View.razor",
	}}

	indexes.prepare(pending)
	indexes.clearPage()
	if store.projectionCalls != 1 {
		t.Fatalf("projection calls = %d, want 1 before the razor pass", store.projectionCalls)
	}

	// Production path: resolveRazorUsings removes the consumed markers.
	// No note* call in this test body.
	r.resolveRazorUsings()

	indexes.prepare(pending)
	defer indexes.clearPage()
	if store.projectionCalls != 2 {
		t.Fatalf("projection calls = %d, want 2: marker removal must invalidate the retained adjacency", store.projectionCalls)
	}
}
