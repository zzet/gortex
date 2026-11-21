package resolver

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func testPersistedPayloadEdge() *graph.Edge {
	return &graph.Edge{
		From: "repo::caller", To: graph.UnresolvedMarker + "Target",
		Kind: graph.EdgeCalls, FilePath: "pkg/caller.go", Line: 17,
		Confidence: 0.25, ConfidenceLabel: "LOW", Origin: graph.OriginTextMatched,
		Tier: "heuristic", CrossRepo: false,
		Meta: map[string]any{
			"receiver_type": "OldReceiver",
			"nested":        map[string]any{"roles": []string{"reader", "caller"}},
		},
	}
}

func TestResolveJobLivenessRejectsSameKeyPayloadReplacementOnCopyingStore(t *testing.T) {
	mutations := []struct {
		name   string
		mutate func(*graph.Edge)
	}{
		{"confidence", func(edge *graph.Edge) { edge.Confidence = 0.9 }},
		{"confidence_label", func(edge *graph.Edge) { edge.ConfidenceLabel = "HIGH" }},
		{"origin", func(edge *graph.Edge) { edge.Origin = graph.OriginLSPResolved }},
		{"tier", func(edge *graph.Edge) { edge.Tier = "lsp" }},
		{"cross_repo", func(edge *graph.Edge) { edge.CrossRepo = true }},
		{"receiver_type_meta", func(edge *graph.Edge) { edge.Meta["receiver_type"] = "NewReceiver" }},
		{"nested_meta", func(edge *graph.Edge) {
			edge.Meta["nested"].(map[string]any)["roles"].([]string)[0] = "writer"
		}},
	}

	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			store := graph.New()
			original := testPersistedPayloadEdge()
			store.AddEdge(original)
			jobs := [][]reindexJob{{{
				edge: original, preResolution: snapshotPersistedEdge(original),
				oldTo: original.To, newTo: "repo::Target", kind: original.Kind,
			}}}

			if !store.RemoveEdge(original.From, original.To, original.Kind) {
				t.Fatal("remove original edge")
			}
			replacement := testPersistedPayloadEdge()
			mutation.mutate(replacement)
			store.AddEdge(replacement)

			liveness := loadResolveJobLiveness(copyingStore{Store: store}, jobs)
			if liveness.contains(jobs[0][0]) {
				t.Fatalf("same-key replacement with changed %s payload was accepted", mutation.name)
			}
		})
	}

	t.Run("exact_copy_remains_live", func(t *testing.T) {
		store := graph.New()
		original := testPersistedPayloadEdge()
		store.AddEdge(original)
		jobs := [][]reindexJob{{{
			edge: original, preResolution: snapshotPersistedEdge(original),
			oldTo: original.To, newTo: "repo::Target", kind: original.Kind,
		}}}
		if !store.RemoveEdge(original.From, original.To, original.Kind) {
			t.Fatal("remove original edge")
		}
		store.AddEdge(testPersistedPayloadEdge())
		if !loadResolveJobLiveness(copyingStore{Store: store}, jobs).contains(jobs[0][0]) {
			t.Fatal("byte-identical same-key replacement was dropped")
		}
	})

	t.Run("pointer_identity_cannot_mask_nested_meta_mutation", func(t *testing.T) {
		store := graph.New()
		live := testPersistedPayloadEdge()
		store.AddEdge(live)
		jobs := [][]reindexJob{{{
			edge: live, preResolution: snapshotPersistedEdge(live),
			oldTo: live.To, newTo: "repo::Target", kind: live.Kind,
		}}}
		live.Meta["nested"].(map[string]any)["roles"].([]string)[0] = "writer"
		if loadResolveJobLiveness(store, jobs).contains(jobs[0][0]) {
			t.Fatal("pointer identity masked an in-place nested Meta change")
		}
	})
}

func TestResolvePayloadReplacementDoesNotCorruptInMemoryAdjacency(t *testing.T) {
	store := graph.New()
	original := testPersistedPayloadEdge()
	store.AddEdge(original)
	job := reindexJob{
		edge: original, preResolution: snapshotPersistedEdge(original),
		oldTo: original.To, oldKind: original.Kind,
		newTo: "repo::Target", kind: original.Kind,
	}

	if !store.RemoveEdge(original.From, original.To, original.Kind) {
		t.Fatal("remove original edge")
	}
	replacement := testPersistedPayloadEdge()
	replacement.Origin = graph.OriginLSPResolved
	replacement.Meta["receiver_type"] = "NewReceiver"
	store.AddEdge(replacement)

	jobs := [][]reindexJob{{job}}
	liveness := loadResolveJobLiveness(store, jobs)
	if liveness.contains(jobs[0][0]) {
		stale := &jobs[0][0]
		stale.edge.To = stale.newTo
		store.ReindexEdges([]graph.EdgeReindex{{Edge: stale.edge, OldTo: stale.oldTo, OldKind: stale.oldKind}})
	}
	assertReplacementAdjacency(t, store, replacement, "repo::Target")
}

func TestResolvePayloadReplacementSurvivesSQLiteReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "payload-liveness.sqlite")
	store, err := store_sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	original := testPersistedPayloadEdge()
	store.AddEdge(original)
	job := reindexJob{
		edge: original, preResolution: snapshotPersistedEdge(original),
		oldTo: original.To, oldKind: original.Kind,
		newTo: "repo::Target", kind: original.Kind,
	}

	if !store.RemoveEdge(original.From, original.To, original.Kind) {
		t.Fatal("remove original SQLite edge")
	}
	replacement := testPersistedPayloadEdge()
	replacement.Confidence = 0.95
	replacement.ConfidenceLabel = "HIGH"
	replacement.Origin = graph.OriginLSPResolved
	replacement.Tier = "lsp"
	replacement.CrossRepo = true
	replacement.Meta["receiver_type"] = "NewReceiver"
	store.AddEdge(replacement)

	jobs := [][]reindexJob{{job}}
	if loadResolveJobLiveness(store, jobs).contains(jobs[0][0]) {
		stale := &jobs[0][0]
		stale.edge.To = stale.newTo
		store.ReindexEdges([]graph.EdgeReindex{{Edge: stale.edge, OldTo: stale.oldTo, OldKind: stale.oldKind}})
	}
	assertReplacementAdjacency(t, store, replacement, "repo::Target")
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := store_sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	assertReplacementAdjacency(t, reopened, replacement, "repo::Target")
}

func TestResolveGuardSpoolRejectsSameKeyPayloadReplacement(t *testing.T) {
	store := graph.New()
	live := testPersistedPayloadEdge()
	live.To = "repo::Target"
	store.AddEdge(live)
	job := reindexJob{
		edge: live, oldTo: graph.UnresolvedMarker + "Target", newTo: live.To,
		kind: live.Kind, crossRepo: live.CrossRepo,
		confidence: live.Confidence, origin: live.Origin, meta: live.Meta,
	}
	spool, err := newResolveGuardSpool()
	if err != nil {
		t.Fatal(err)
	}
	defer spool.close()
	if err := spool.appendJobs([][]reindexJob{{job}}); err != nil {
		t.Fatal(err)
	}

	if !store.RemoveEdge(live.From, live.To, live.Kind) {
		t.Fatal("remove guarded edge")
	}
	replacement := testPersistedPayloadEdge()
	replacement.To = live.To
	replacement.Origin = graph.OriginLSPResolved
	replacement.Meta["receiver_type"] = "NewReceiver"
	store.AddEdge(replacement)

	records, _, err := spool.nextPage(16)
	if err != nil {
		t.Fatal(err)
	}
	if jobs := guardJobsFromRecords(copyingStore{Store: store}, records); len(jobs) != 0 {
		t.Fatalf("guard replay retained %d stale same-key payload replacement(s)", len(jobs))
	}
}

func TestDeferredLSPSpoolRejectsSameKeyPayloadReplacement(t *testing.T) {
	store := graph.New()
	live := testPersistedPayloadEdge()
	store.AddEdge(live)
	spool, err := newDeferredLSPSpool()
	if err != nil {
		t.Fatal(err)
	}
	defer spool.close()
	deferred := deferredLSPEdge{edge: live, target: "Target"}
	if err := spool.append([]deferredLSPEdge{deferred}); err != nil {
		t.Fatal(err)
	}

	if !store.RemoveEdge(live.From, live.To, live.Kind) {
		t.Fatal("remove deferred LSP edge")
	}
	replacement := testPersistedPayloadEdge()
	replacement.Origin = graph.OriginLSPResolved
	replacement.Meta["receiver_type"] = "NewReceiver"
	store.AddEdge(replacement)

	records, _, err := spool.iterator(nil).next(16)
	if err != nil {
		t.Fatal(err)
	}
	edges, stale := lspEdgesFromRecords(copyingStore{Store: store}, records, nil)
	if len(edges) != 0 || len(stale) != 1 {
		t.Fatalf("deferred LSP replay edges=%d stale=%d, want 0/1", len(edges), len(stale))
	}
}

type fixedDefinitionLSP struct {
	path string
	line int
}

func (helper fixedDefinitionLSP) SupportsPath(string) bool { return true }
func (helper fixedDefinitionLSP) Definition(string, int, string) (string, int, bool) {
	return helper.path, helper.line, true
}

type refusedLSPCorrection struct {
	*store_sqlite.Store
	refuse bool
}

func (s *refusedLSPCorrection) ReindexEdgesApplied(batch []graph.EdgeReindex) bool {
	for _, move := range batch {
		if s.refuse && move.Edge.Origin == graph.OriginLSPResolved {
			return false
		}
	}
	return s.Store.ReindexEdgesApplied(batch)
}

func TestRefusedLSPCorrectionRetriesResolvedEdge(t *testing.T) {
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	target := &graph.Node{ID: "def.ts::Target", Name: "Target", FilePath: "def.ts", StartLine: 5, Kind: graph.KindFunction}
	edge := &graph.Edge{From: "caller.ts::run", To: "guess.ts::Target", FilePath: "caller.ts", Line: 1, Kind: graph.EdgeCalls}
	store.AddBatch([]*graph.Node{target}, []*graph.Edge{edge})
	writer := &refusedLSPCorrection{Store: store, refuse: true}
	r := New(writer)
	r.SetLSPHelper(fixedDefinitionLSP{path: target.FilePath, line: target.StartLine})
	r.lspDeferredSpool, err = newDeferredLSPSpool()
	require.NoError(t, err)
	t.Cleanup(func() { r.SetGraph(store) })
	require.NoError(t, r.lspDeferredSpool.append([]deferredLSPEdge{{edge: edge, target: "Target"}}))
	stats, err := r.ResolveAllContext(t.Context())
	require.ErrorContains(t, err, "binding write did not apply")
	require.Contains(t, stats.DroppedFiles, edge.FilePath)
	require.Equal(t, "guess.ts::Target", store.GetOutEdges(edge.From)[0].To)
	writer.refuse = false
	_, err = r.ResolveAllContext(t.Context())
	require.NoError(t, err)
	require.Equal(t, target.ID, store.GetOutEdges(edge.From)[0].To)
}

func TestDeferredLSPSQLiteKindPromotionSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lsp-kind-promotion.sqlite")
	store, err := store_sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	caller := &graph.Node{
		ID: "repo::caller", Kind: graph.KindFunction, Name: "caller",
		FilePath: "pkg/caller.go", StartLine: 5, RepoPrefix: "repo",
	}
	target := &graph.Node{
		ID: "repo::Handler", Kind: graph.KindFunction, Name: "Handler",
		FilePath: "pkg/handler.go", StartLine: 20, RepoPrefix: "repo",
	}
	edge := &graph.Edge{
		From: caller.ID, To: graph.UnresolvedMarker + "Handler",
		Kind: graph.EdgeReads, FilePath: caller.FilePath, Line: 10,
		Confidence: 0.25, Origin: graph.OriginTextMatched,
	}
	store.AddBatch([]*graph.Node{caller, target}, []*graph.Edge{edge})
	unresolved := edge.To

	resolver := New(store)
	resolver.SetLSPHelper(fixedDefinitionLSP{path: target.FilePath, line: target.StartLine})
	result := resolver.resolveDeferredLSP(context.Background(), []deferredLSPEdge{{edge: edge, target: "Handler"}})
	if result.resolved != 1 {
		t.Fatalf("resolved=%d, want 1", result.resolved)
	}
	assertPromotedLSPAdjacency(t, store, caller.ID, target.ID, unresolved)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := store_sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	assertPromotedLSPAdjacency(t, reopened, caller.ID, target.ID, unresolved)
}

func assertReplacementAdjacency(t *testing.T, store graph.Store, replacement *graph.Edge, staleTarget string) {
	t.Helper()
	out := store.GetOutEdges(replacement.From)
	if len(out) != 1 || !snapshotPersistedEdge(replacement).matches(out[0]) {
		t.Fatalf("out adjacency = %#v, want exact replacement %#v", out, replacement)
	}
	in := store.GetInEdges(replacement.To)
	if len(in) != 1 || !snapshotPersistedEdge(replacement).matches(in[0]) {
		t.Fatalf("replacement in adjacency = %#v", in)
	}
	if stale := store.GetInEdges(staleTarget); len(stale) != 0 {
		t.Fatalf("stale target adjacency = %#v", stale)
	}
}

func assertPromotedLSPAdjacency(t *testing.T, store graph.Store, from, to, unresolved string) {
	t.Helper()
	out := store.GetOutEdges(from)
	if len(out) != 1 {
		t.Fatalf("out edges = %#v, want one promoted edge", out)
	}
	if out[0].To != to || out[0].Kind != graph.EdgeReferences || out[0].Origin != graph.OriginLSPResolved {
		t.Fatalf("promoted edge = %#v", out[0])
	}
	if stale := store.GetInEdges(unresolved); len(stale) != 0 {
		t.Fatalf("stale unresolved in adjacency = %#v", stale)
	}
	in := store.GetInEdges(to)
	if len(in) != 1 || in[0].Kind != graph.EdgeReferences {
		t.Fatalf("promoted target in adjacency = %#v", in)
	}
}

func TestRefusedLSPCorrectionStillRunsGuard(t *testing.T) {
	mem := buildGraphFromSources(t, map[string]string{
		"other/lib.ts": "export function lookup(){}",
		"page.ts":      "function run(){const ns = {other:1}; ns.lookup()}",
	})
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	target := &graph.Node{ID: "def.ts::Target", Name: "Target", FilePath: "def.ts", StartLine: 5, Kind: graph.KindFunction}
	edge := &graph.Edge{From: "caller.ts::run", To: "guess.ts::Target", FilePath: "caller.ts", Line: 1, Kind: graph.EdgeCalls}
	store.AddBatch(append(mem.AllNodes(), target), append(mem.AllEdges(), edge))
	writer := &refusedLSPCorrection{Store: store, refuse: true}
	r := New(writer)
	r.SetLSPHelper(fixedDefinitionLSP{path: target.FilePath, line: target.StartLine})
	r.lspDeferredSpool, err = newDeferredLSPSpool()
	require.NoError(t, err)
	t.Cleanup(func() { r.SetGraph(store) })
	require.NoError(t, r.lspDeferredSpool.append([]deferredLSPEdge{{edge: edge, target: "Target"}}))
	_, err = r.ResolveAllContext(t.Context())
	require.ErrorContains(t, err, "binding write did not apply")
	// The guard still reverts a name-only binding into an unimported module.
	require.True(t, graph.IsUnresolvedTarget(callEdgeTo(store, "page.ts::run", 1)), callEdgeTo(store, "page.ts::run", 1))
}
