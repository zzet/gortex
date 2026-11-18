package mcp

import (
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"go.uber.org/zap"
)

type countingAnalysisCacheStore struct {
	*store_sqlite.Store
	store         *store_sqlite.Store
	allNodes      atomic.Int64
	allNodesLight atomic.Int64
	allEdgesLight atomic.Int64
}

func newCountingAnalysisCacheStore(store *store_sqlite.Store) *countingAnalysisCacheStore {
	return &countingAnalysisCacheStore{Store: store, store: store}
}

func (s *countingAnalysisCacheStore) AllNodes() []*graph.Node {
	s.allNodes.Add(1)
	return s.store.AllNodes()
}

func (s *countingAnalysisCacheStore) AllNodesLight() []*graph.Node {
	s.allNodesLight.Add(1)
	return s.store.AllNodesLight()
}

func (s *countingAnalysisCacheStore) AllEdgesLight(kinds ...graph.EdgeKind) []*graph.Edge {
	s.allEdgesLight.Add(1)
	return s.store.AllEdgesLight(kinds...)
}

type mutateBeforeAnalysisCommitStore struct {
	*countingAnalysisCacheStore
	once sync.Once
	node *graph.Node
}

func (s *mutateBeforeAnalysisCommitStore) CommitAnalysisSnapshot(revision uint64, install func()) bool {
	s.once.Do(func() { s.store.AddNode(s.node) })
	return s.store.CommitAnalysisSnapshot(revision, install)
}

func buildAnalysisCacheTestGraph(tb testing.TB, nodeCount int) (*store_sqlite.Store, string) {
	tb.Helper()
	path := tb.TempDir() + "/analysis.sqlite"
	store, err := store_sqlite.Open(path)
	if err != nil {
		tb.Fatal(err)
	}
	nodes := make([]*graph.Node, 0, nodeCount)
	edges := make([]*graph.Edge, 0, nodeCount*2)
	for i := 0; i < nodeCount; i++ {
		id := fmt.Sprintf("repo::pkg%d::HandleRequest%d", i/20, i)
		nodes = append(nodes, &graph.Node{
			ID: id, Kind: graph.KindFunction, Name: fmt.Sprintf("HandleRequest%d", i),
			QualName: fmt.Sprintf("pkg%d.HandleRequest%d", i/20, i),
			FilePath: fmt.Sprintf("pkg%d/file%d.go", i/20, i%20), StartLine: i + 1, EndLine: i + 4,
			Language: "go", RepoPrefix: "repo",
		})
		if i > 0 {
			prev := nodes[i-1].ID
			edges = append(edges, &graph.Edge{From: prev, To: id, Kind: graph.EdgeCalls, FilePath: nodes[i-1].FilePath, Line: i + 2})
		}
		if i > 7 {
			edges = append(edges, &graph.Edge{From: nodes[i-7].ID, To: id, Kind: graph.EdgeReferences, FilePath: nodes[i-7].FilePath, Line: i + 1})
		}
	}
	store.AddBatch(nodes, edges)
	return store, path
}

func populateAnalysisForTest(store graph.Store) (*Server, analysisRunMetrics) {
	server := &Server{graph: store, logger: zap.NewNop()}
	server.analysisMu.Lock()
	metrics := server.populateAnalysisLocked()
	server.analysisMu.Unlock()
	return server, metrics
}

func TestAnalysisPersistenceFullHitSurvivesRestart(t *testing.T) {
	store, path := buildAnalysisCacheTestGraph(t, 240)
	first, cold := populateAnalysisForTest(store)
	if cold.cacheHit {
		t.Fatal("cold analysis unexpectedly reported a cache hit")
	}
	wantCommunities := first.communities
	wantProcesses := first.processes
	wantPageRank := first.pageRank
	wantAdjacency := first.adjacency.PersistenceSnapshot()
	wantConcepts := first.autoConcepts.PersistenceSnapshot()
	wantHITS := first.hits
	wantLeiden := first.leidenCache.PersistenceSnapshot()

	header, found, err := store.LoadActiveAnalysisHeader(analysisGenerationFormatVersion)
	if err != nil || !found {
		_, writerOK := any(store).(graph.AnalysisGenerationStore)
		_, queryOK := any(store).(graph.AnalysisQueryStore)
		t.Fatalf("saved generation found=%v err=%v save_err=%v save=%s writer=%v query=%v revision=%d token_revision=%d", found, err, cold.cacheSaveErr, cold.cacheSave, writerOK, queryOK, store.AnalysisMutationRevision(), first.communitiesToken.analysisRevision)
	}
	if header.NodeCount != 240 || header.GenerationID <= 0 {
		t.Fatalf("unexpected generation header: %+v", header)
	}
	t.Logf("analysis generation: cold=%s save=%s id=%d", cold.snapshot+cold.leiden+cold.processes+cold.pageRank+cold.adjacency+cold.autoConcepts+cold.hits, cold.cacheSave, header.GenerationID)

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store_sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	counting := newCountingAnalysisCacheStore(reopened)
	second, warm := populateAnalysisForTest(counting)
	if !warm.cacheHit {
		t.Fatalf("unchanged restart did not hit complete cache: %+v", warm)
	}
	if counting.allNodes.Load() != 0 || counting.allNodesLight.Load() != 0 || counting.allEdgesLight.Load() != 0 {
		t.Fatalf("cache hit scanned graph: full=%d light=%d edges=%d", counting.allNodes.Load(), counting.allNodesLight.Load(), counting.allEdgesLight.Load())
	}
	if second.communities != nil || second.processes != nil || second.pageRank != nil || second.adjacency != nil || second.autoConcepts != nil || second.hits != nil || second.leidenCache != nil {
		t.Fatal("warm startup eagerly materialized normalized analysis")
	}
	if !second.analysisGenerationReady || second.analysisGeneration.GenerationID != header.GenerationID {
		t.Fatalf("warm generation receipt not published: %+v", second.analysisGeneration)
	}
	gotCommunities := second.getCommunities()
	gotProcesses := second.getProcesses()
	gotPageRank := second.getPageRank()
	gotAdjacency := second.getAdjacency()
	gotConcepts := second.getAutoConcepts()
	gotHITS := second.getHITS()
	if !second.ensureLeidenMaterialized() {
		t.Fatal("lazy Leiden state did not materialize")
	}
	checks := map[string]bool{
		"communities": reflect.DeepEqual(gotCommunities, wantCommunities),
		"processes":   reflect.DeepEqual(gotProcesses, wantProcesses),
		"pagerank":    reflect.DeepEqual(gotPageRank, wantPageRank),
		"adjacency":   gotAdjacency != nil && reflect.DeepEqual(gotAdjacency.PersistenceSnapshot(), wantAdjacency),
		"concepts":    gotConcepts != nil && reflect.DeepEqual(gotConcepts.PersistenceSnapshot(), wantConcepts),
		"hits":        reflect.DeepEqual(gotHITS, wantHITS),
		"leiden":      reflect.DeepEqual(second.leidenCache.PersistenceSnapshot(), wantLeiden),
	}
	for component, equal := range checks {
		if !equal {
			t.Errorf("lazy %s differs from cold result", component)
		}
	}
	t.Logf("analysis generation: warm header load=%s", warm.cacheLoad)
}

func TestAnalysisPersistenceRejectsMutationBeforePublish(t *testing.T) {
	store, _ := buildAnalysisCacheTestGraph(t, 120)
	defer store.Close()
	_, cold := populateAnalysisForTest(store)
	if cold.cacheHit {
		t.Fatal("cold analysis unexpectedly reported a cache hit")
	}

	raceNode := &graph.Node{
		ID: "race::published", Kind: graph.KindFunction, Name: "published",
		QualName: "race.published", FilePath: "race.go", StartLine: 1, EndLine: 2, Language: "go",
	}
	wrapped := &mutateBeforeAnalysisCommitStore{
		countingAnalysisCacheStore: newCountingAnalysisCacheStore(store),
		node:                       raceNode,
	}
	server, metrics := populateAnalysisForTest(wrapped)
	if metrics.cacheHit {
		t.Fatal("cache hit survived a graph mutation before publication")
	}
	if server.pageRank == nil {
		t.Fatal("stable retry did not publish PageRank")
	}
	if _, ok := server.pageRank.Scores[raceNode.ID]; !ok {
		t.Fatal("stable retry published analysis from before the mutation")
	}
	if got, want := server.communitiesToken.analysisRevision, store.AnalysisMutationRevision(); got != want {
		t.Fatalf("published revision=%d, graph revision=%d", got, want)
	}
	if server.getPageRank() == nil {
		t.Fatal("fresh PageRank was rejected")
	}

	store.AddNode(&graph.Node{
		ID: "race::later", Kind: graph.KindFunction, Name: "later",
		QualName: "race.later", FilePath: "race.go", StartLine: 4, EndLine: 5, Language: "go",
	})
	if server.getPageRank() != nil || server.getCommunities() != nil || server.getAdjacency() != nil {
		t.Fatal("analysis readers served a snapshot after a later graph mutation")
	}
}

// buildAnalysisCacheTestGraphAt seeds the same corpus buildAnalysisCacheTestGraph
// builds, but into one payload view generation instead of the base corpus, and
// registers its symbol rows so SearchSymbolBundles has hits at that generation.
// It returns the owning (base) handle and the handle pinned to viewGen.
func buildAnalysisCacheTestGraphAt(tb testing.TB, viewGen int64, nodeCount int) (base, pinned *store_sqlite.Store) {
	tb.Helper()
	base, err := store_sqlite.Open(tb.TempDir() + "/analysis_generation.sqlite")
	if err != nil {
		tb.Fatal(err)
	}
	pinned = base.AtGeneration(viewGen)
	if pinned == nil {
		tb.Fatalf("AtGeneration(%d) returned nil", viewGen)
	}
	nodes := make([]*graph.Node, 0, nodeCount)
	edges := make([]*graph.Edge, 0, nodeCount*2)
	items := make([]graph.SymbolFTSItem, 0, nodeCount)
	for i := 0; i < nodeCount; i++ {
		id := fmt.Sprintf("repo::pkg%d::HandleRequest%d", i/20, i)
		nodes = append(nodes, &graph.Node{
			ID: id, Kind: graph.KindFunction, Name: fmt.Sprintf("HandleRequest%d", i),
			QualName: fmt.Sprintf("pkg%d.HandleRequest%d", i/20, i),
			FilePath: fmt.Sprintf("pkg%d/file%d.go", i/20, i%20), StartLine: i + 1, EndLine: i + 4,
			Language: "go", RepoPrefix: "repo",
		})
		items = append(items, graph.SymbolFTSItem{NodeID: id, Tokens: fmt.Sprintf("handlerequest%d handler", i)})
		if i > 0 {
			prev := nodes[i-1].ID
			edges = append(edges, &graph.Edge{From: prev, To: id, Kind: graph.EdgeCalls, FilePath: nodes[i-1].FilePath, Line: i + 2})
		}
		if i > 7 {
			edges = append(edges, &graph.Edge{From: nodes[i-7].ID, To: id, Kind: graph.EdgeReferences, FilePath: nodes[i-7].FilePath, Line: i + 1})
		}
	}
	pinned.AddBatch(nodes, edges)
	if err := pinned.BatchUpsertSymbolFTS(items); err != nil {
		tb.Fatalf("seed symbol fts at generation %d: %v", viewGen, err)
	}
	return base, pinned
}

// The analysis passes walk s.graph, so the package fingerprints they derive
// describe THAT generation. Installing them through backendStore() — the
// indexer's base handle — stamped the base corpus with another snapshot's
// fingerprints and left the analysed generation permanently uncacheable, since
// the bundle cache validates a generation's entries against its own map only.
//
// Revert-red: swap analysisGenerationStore() back to backendStore() and the
// second query below recomputes, surfacing the freshly added edge.
func TestPopulateAnalysis_InstallsBundleFingerprintsOnTheAnalysedGeneration(t *testing.T) {
	const viewGen = int64(1)
	base, pinned := buildAnalysisCacheTestGraphAt(t, viewGen, 80)
	defer base.Close()

	// The indexer owns the BASE handle; the server reads the routed generation.
	// This is the divergence analysisGenerationStore() exists to correct.
	srv := &Server{graph: pinned, indexer: newBaseHandleIndexer(base), logger: zap.NewNop()}
	if got := srv.backendStore(); got != graph.Store(base) {
		t.Fatalf("fixture precondition: backendStore must hand back the base handle, got %T", got)
	}
	if srv.analysisViewGeneration() != viewGen {
		t.Fatalf("fixture precondition: analysis reads generation %d, want %d",
			srv.analysisViewGeneration(), viewGen)
	}

	srv.analysisMu.Lock()
	metrics := srv.populateAnalysisLocked()
	srv.analysisMu.Unlock()
	if metrics.cacheSaveErr != nil {
		t.Fatalf("analysis generation save: %v", metrics.cacheSaveErr)
	}

	const probe = "repo::pkg0::HandleRequest3"
	first, err := pinned.SearchSymbolBundles("handlerequest3", 4)
	if err != nil {
		t.Fatalf("warm bundles at generation %d: %v", viewGen, err)
	}
	warm, ok := bundlesByNodeID(first)[probe]
	if !ok {
		t.Fatalf("generation %d query returned no bundle for %s", viewGen, probe)
	}
	warmEdges := len(warm.OutEdges)

	// Add an out-edge WITHOUT re-running the analysis: a cached bundle still
	// reports warmEdges, an uncacheable generation recomputes and reports more.
	// This is the hit probe, not a licence to serve stale bundles — under the
	// cache's contract a producer that changes a package's content moves that
	// package's fingerprint, and the probe works precisely because it skips the
	// analysis pass that would have moved it.
	const addedID = "repo::pkg0::AddedAfterAnalysis"
	pinned.AddNode(&graph.Node{
		ID: addedID, Kind: graph.KindFunction, Name: "AddedAfterAnalysis",
		FilePath: "pkg0/file3.go", Language: "go", RepoPrefix: "repo",
	})
	pinned.AddEdge(&graph.Edge{
		From: probe, To: addedID, Kind: graph.EdgeCalls, FilePath: "pkg0/file3.go",
	})

	second, err := pinned.SearchSymbolBundles("handlerequest3", 4)
	if err != nil {
		t.Fatalf("second bundles at generation %d: %v", viewGen, err)
	}
	got, ok := bundlesByNodeID(second)[probe]
	if !ok {
		t.Fatalf("generation %d re-query returned no bundle for %s", viewGen, probe)
	}
	if len(got.OutEdges) != warmEdges {
		t.Fatalf("the analysed generation was never made cacheable — its fingerprints went to another handle: out-edges %d, want the cached %d",
			len(got.OutEdges), warmEdges)
	}
}

func bundlesByNodeID(bundles []graph.SymbolBundle) map[string]graph.SymbolBundle {
	out := make(map[string]graph.SymbolBundle, len(bundles))
	for _, b := range bundles {
		if b.Node != nil {
			out[b.Node.ID] = b
		}
	}
	return out
}

// newBaseHandleIndexer builds the minimal indexer the server needs for
// backendStore() to hand back the base handle rather than s.graph.
func newBaseHandleIndexer(base *store_sqlite.Store) *indexer.Indexer {
	return indexer.New(base, parser.NewRegistry(), config.IndexConfig{}, zap.NewNop())
}
