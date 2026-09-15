package resolver

import (
	"fmt"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// resolverBatchCountingStore records only the point/batch operations whose
// cardinality matters to the incremental and inference hot paths. Embedding
// Store keeps the wrapper aligned with future backend capabilities.
type resolverBatchCountingStore struct {
	graph.Store

	getNodeCalls               int
	getNodesByIDsCalls         int
	getNodesByIDsNonEmptyCalls int
	getNodesByIDRequests       [][]string
	returnNilNodeBatches       bool
	getFileNodesCalls          int
	getFileNodesByPathsCalls   int
	getOutEdgesCalls           int
	getOutEdgesByNodeIDsCalls  int
	getInEdgesCalls            int
	getInEdgesByNodeIDsCalls   int
	addEdgeCalls               int
	addBatchCalls              int
	reindexEdgesCalls          int
	allNodesCalls              int
	allEdgesCalls              int
}

func (s *resolverBatchCountingStore) GetNode(id string) *graph.Node {
	s.getNodeCalls++
	return s.Store.GetNode(id)
}

func (s *resolverBatchCountingStore) GetNodesByIDs(ids []string) map[string]*graph.Node {
	s.getNodesByIDsCalls++
	if len(ids) > 0 {
		s.getNodesByIDsNonEmptyCalls++
		s.getNodesByIDRequests = append(s.getNodesByIDRequests, append([]string(nil), ids...))
	}
	if s.returnNilNodeBatches {
		return nil
	}
	return s.Store.GetNodesByIDs(ids)
}

func (s *resolverBatchCountingStore) AllNodes() []*graph.Node {
	s.allNodesCalls++
	return s.Store.AllNodes()
}

func (s *resolverBatchCountingStore) AllEdges() []*graph.Edge {
	s.allEdgesCalls++
	return s.Store.AllEdges()
}

func (s *resolverBatchCountingStore) GetFileNodes(filePath string) []*graph.Node {
	s.getFileNodesCalls++
	return s.Store.GetFileNodes(filePath)
}

func (s *resolverBatchCountingStore) GetFileNodesByPaths(filePaths []string) map[string][]*graph.Node {
	s.getFileNodesByPathsCalls++
	return s.Store.GetFileNodesByPaths(filePaths)
}

func (s *resolverBatchCountingStore) GetOutEdges(nodeID string) []*graph.Edge {
	s.getOutEdgesCalls++
	return s.Store.GetOutEdges(nodeID)
}

func (s *resolverBatchCountingStore) GetOutEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	s.getOutEdgesByNodeIDsCalls++
	return s.Store.GetOutEdgesByNodeIDs(ids)
}

func (s *resolverBatchCountingStore) GetInEdges(nodeID string) []*graph.Edge {
	s.getInEdgesCalls++
	return s.Store.GetInEdges(nodeID)
}

func (s *resolverBatchCountingStore) GetInEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	s.getInEdgesByNodeIDsCalls++
	return s.Store.GetInEdgesByNodeIDs(ids)
}

func (s *resolverBatchCountingStore) AddEdge(edge *graph.Edge) {
	s.addEdgeCalls++
	s.Store.AddEdge(edge)
}

func (s *resolverBatchCountingStore) AddBatch(nodes []*graph.Node, edges []*graph.Edge) {
	s.addBatchCalls++
	s.Store.AddBatch(nodes, edges)
}

func (s *resolverBatchCountingStore) ReindexEdges(batch []graph.EdgeReindex) {
	s.reindexEdgesCalls++
	s.Store.ReindexEdges(batch)
}

func (s *resolverBatchCountingStore) HasLanguage(language string) bool {
	if reader, ok := s.Store.(interface{ HasLanguage(string) bool }); ok {
		return reader.HasLanguage(language)
	}
	return true
}

func (s *resolverBatchCountingStore) nodeIDBatchesContaining(ids ...string) int {
	wanted := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		wanted[id] = struct{}{}
	}
	batches := 0
	for _, request := range s.getNodesByIDRequests {
		for _, id := range request {
			if _, ok := wanted[id]; ok {
				batches++
				break
			}
		}
	}
	return batches
}

func TestPrepareReusesPageSourceHydration(t *testing.T) {
	g := graph.New()
	nodes := []*graph.Node{
		{ID: "repo::caller1", Kind: graph.KindFunction, Name: "caller1", RepoPrefix: "repo", Language: "go", FilePath: "repo/a.go"},
		{ID: "repo::caller2", Kind: graph.KindFunction, Name: "caller2", RepoPrefix: "repo", Language: "go", FilePath: "repo/b_test.go"},
	}
	g.AddBatch(nodes, nil)

	counting := &resolverBatchCountingStore{Store: g}
	r := New(counting)
	r.SetScope(map[string]struct{}{"repo": {}})
	indexes := newResolveAllPassIndexes(r)
	defer indexes.close()
	pages := [][]*graph.Edge{
		{
			{From: "repo::caller1", To: graph.UnresolvedMarker + "Work", Kind: graph.EdgeCalls, FilePath: "repo/a.go"},
			{From: "repo::caller1", To: graph.UnresolvedMarker + "Missing", Kind: graph.EdgeCalls, FilePath: "repo/a.go"},
		},
		{
			{From: "repo::caller2", To: graph.UnresolvedMarker + "Work", Kind: graph.EdgeCalls, FilePath: "repo/b_test.go"},
			{From: "repo::caller2", To: graph.UnresolvedMarker + "Missing", Kind: graph.EdgeCalls, FilePath: "repo/b_test.go"},
		},
	}
	for page, pending := range pages {
		sources := indexes.prepare(pending)
		if sources == nil {
			t.Fatalf("page %d did not publish its source hydration", page)
		}
		if err := r.warmLookupCacheWithSources(t.Context(), pending, sources); err != nil {
			t.Fatal(err)
		}
		if got := r.cachedGetNode(pending[0].From); got == nil || got.ID != pending[0].From {
			t.Fatalf("page %d source cache miss: %+v", page, got)
		}
		r.clearLookupCache()
		indexes.clearPage()
	}
	if got := counting.nodeIDBatchesContaining("repo::caller1", "repo::caller2"); got != len(pages) {
		t.Fatalf("source hydration batches = %d, want one per scoped page (%d)", got, len(pages))
	}
	if counting.getNodeCalls != 0 {
		t.Fatalf("point source lookups = %d, want 0", counting.getNodeCalls)
	}
}

func TestWarmLookupCacheAuthoritativeMissingSourcesAvoidPointNPlusOne(t *testing.T) {
	g := graph.New()
	counting := &resolverBatchCountingStore{Store: g, returnNilNodeBatches: true}
	r := New(counting)

	const unique = 64
	pending := make([]*graph.Edge, 0, unique*2)
	ids := make([]string, 0, unique)
	for i := 0; i < unique; i++ {
		id := fmt.Sprintf("repo::missing::%03d", i)
		ids = append(ids, id)
		for repeat := 0; repeat < 2; repeat++ {
			pending = append(pending, &graph.Edge{
				From: id, To: graph.UnresolvedMarker + "Work", Kind: graph.EdgeCalls,
				FilePath: fmt.Sprintf("repo/missing%d.go", i),
			})
		}
	}

	if err := r.warmLookupCache(pending); err != nil {
		t.Fatal(err)
	}
	if r.nodeByID == nil {
		t.Fatal("nil backend result was not normalized to a completed positive cache")
	}
	if got := len(r.missingNodeByID); got != unique {
		t.Fatalf("authoritative source misses = %d, want %d", got, unique)
	}
	if got := counting.nodeIDBatchesContaining(ids...); got != 1 {
		t.Fatalf("missing-source hydration batches = %d, want 1", got)
	}
	for _, edge := range pending {
		if got := r.cachedGetNode(edge.From); got != nil {
			t.Fatalf("dangling source %q unexpectedly resolved to %+v", edge.From, got)
		}
	}
	if counting.getNodeCalls != 0 {
		t.Fatalf("dangling-source point lookups = %d, want 0", counting.getNodeCalls)
	}
	if counting.allNodesCalls != 0 || counting.allEdgesCalls != 0 {
		t.Fatalf("whole-graph scans = nodes:%d edges:%d, want 0/0", counting.allNodesCalls, counting.allEdgesCalls)
	}

	r.clearLookupCache()
	if r.nodeByID != nil || r.missingNodeByID != nil {
		t.Fatalf("page cleanup retained source caches: nodes=%v missing=%v", r.nodeByID, r.missingNodeByID)
	}
	counting.returnNilNodeBatches = false
	g.AddBatch([]*graph.Node{{
		ID: ids[0], Kind: graph.KindFunction, Name: "late", RepoPrefix: "repo", Language: "go", FilePath: "repo/late.go",
	}}, nil)
	if got := r.cachedGetNode(ids[0]); got == nil || got.ID != ids[0] {
		t.Fatalf("source inserted after page cleanup was hidden: %+v", got)
	}
}

func TestFullUnscopedPrepareDefersOneSourceHydrationToWarm(t *testing.T) {
	g := graph.New()
	const sourceID = "repo::caller"
	g.AddBatch([]*graph.Node{
		{ID: "repo/caller.go", Kind: graph.KindFile, Name: "caller.go", RepoPrefix: "repo", Language: "go", FilePath: "repo/caller.go"},
		{ID: sourceID, Kind: graph.KindFunction, Name: "caller", RepoPrefix: "repo", Language: "go", FilePath: "repo/caller.go"},
	}, nil)
	counting := &resolverBatchCountingStore{Store: g}
	r := New(counting)
	indexes := newResolveAllPassIndexes(r)
	defer indexes.close()
	pending := []*graph.Edge{{
		From: sourceID, To: graph.UnresolvedMarker + "Work", Kind: graph.EdgeCalls, FilePath: "repo/caller.go",
	}}

	if sources := indexes.prepare(pending); sources != nil {
		t.Fatalf("full unscoped prepare unexpectedly hydrated sources: %+v", sources)
	}
	if got := counting.nodeIDBatchesContaining(sourceID); got != 0 {
		t.Fatalf("full unscoped prepare source batches = %d, want 0", got)
	}
	if !indexes.dirAll || indexes.depAll {
		t.Fatalf("directory-only shape prepared dir/dep = %v/%v, want true/false", indexes.dirAll, indexes.depAll)
	}
	if err := r.warmLookupCacheWithSources(t.Context(), pending, nil); err != nil {
		t.Fatal(err)
	}
	if got := counting.nodeIDBatchesContaining(sourceID); got != 1 {
		t.Fatalf("full unscoped prepare+warm source batches = %d, want 1", got)
	}
	if got := r.cachedGetNode(sourceID); got == nil || got.ID != sourceID {
		t.Fatalf("full unscoped source cache miss: %+v", got)
	}
	if counting.getNodeCalls != 0 {
		t.Fatalf("full unscoped point source lookups = %d, want 0", counting.getNodeCalls)
	}
	r.clearLookupCache()
	indexes.clearPage()
}

func TestSourceNegativesRecomputeAcrossForcedAndGenerationRefresh(t *testing.T) {
	g := graph.New()
	counting := &resolverBatchCountingStore{Store: g}
	r := New(counting)
	r.SetScope(map[string]struct{}{"repo": {}})
	indexes := newResolveAllPassIndexes(r)
	defer indexes.close()
	lateID := "repo::late"
	pending := []*graph.Edge{{
		From: lateID, To: graph.UnresolvedMarker + "Work", Kind: graph.EdgeCalls, FilePath: "repo/late.go",
	}}

	sources := indexes.prepare(pending)
	if err := r.warmLookupCacheWithSources(t.Context(), pending, sources); err != nil {
		t.Fatal(err)
	}
	if _, missing := r.missingNodeByID[lateID]; !missing {
		t.Fatal("initial missing source was not negatively cached")
	}
	if got := r.cachedGetNode(lateID); got != nil {
		t.Fatalf("initial missing source unexpectedly resolved: %+v", got)
	}
	g.AddBatch([]*graph.Node{{
		ID: lateID, Kind: graph.KindFunction, Name: "late", RepoPrefix: "repo", Language: "go", FilePath: "repo/late.go",
	}}, nil)
	if refreshed, err := indexes.refreshAfterInterleave(t.Context(), pending, true); err != nil {
		t.Fatal(err)
	} else if !refreshed {
		t.Fatal("forced refresh did not rebuild the current page")
	}
	if _, missing := r.missingNodeByID[lateID]; missing {
		t.Fatal("forced refresh retained a stale source negative")
	}
	if got := r.cachedGetNode(lateID); got == nil || got.ID != lateID {
		t.Fatalf("forced refresh did not expose inserted source: %+v", got)
	}

	r.clearLookupCache()
	neverID := "repo::never"
	generationPending := []*graph.Edge{{
		From: neverID, To: graph.UnresolvedMarker + "Work", Kind: graph.EdgeCalls, FilePath: "repo/never.go",
	}}
	if refreshed, err := indexes.refreshAfterInterleave(t.Context(), generationPending, false); err != nil {
		t.Fatal(err)
	} else if !refreshed {
		t.Fatal("generation change did not rebuild the current page")
	}
	if _, missing := r.missingNodeByID[neverID]; !missing {
		t.Fatal("generation refresh did not recompute source negatives")
	}
	if got := r.cachedGetNode(neverID); got != nil {
		t.Fatalf("generation-refreshed missing source unexpectedly resolved: %+v", got)
	}
	if counting.getNodeCalls != 0 {
		t.Fatalf("refresh source point lookups = %d, want 0", counting.getNodeCalls)
	}
	r.clearLookupCache()
	indexes.clearPage()
	if r.nodeByID != nil || r.missingNodeByID != nil {
		t.Fatalf("normal page cleanup retained source caches: nodes=%v missing=%v", r.nodeByID, r.missingNodeByID)
	}
}

func TestCollectIncrementalFileFrontierUsesConstantBatchReads(t *testing.T) {
	g := graph.New()
	const files = 12
	paths := make([]string, 0, files+2)
	var nodes []*graph.Node
	var edges []*graph.Edge
	for i := 0; i < files; i++ {
		path := fmt.Sprintf("pkg/f%d.go", i)
		definitionID := fmt.Sprintf("%s::Def%d", path, i)
		outsideID := fmt.Sprintf("outside/caller%d.go::Caller", i)
		paths = append(paths, path)
		nodes = append(nodes,
			&graph.Node{ID: path, Kind: graph.KindFile, Name: path, FilePath: path},
			&graph.Node{ID: definitionID, Kind: graph.KindFunction, Name: fmt.Sprintf("Def%d", i), FilePath: path},
			&graph.Node{ID: outsideID, Kind: graph.KindFunction, Name: "Caller", FilePath: fmt.Sprintf("outside/caller%d.go", i)},
		)
		edges = append(edges,
			&graph.Edge{From: definitionID, To: graph.UnresolvedMarker + fmt.Sprintf("Missing%d", i), Kind: graph.EdgeCalls, FilePath: path, Line: 2},
			&graph.Edge{From: outsideID, To: graph.UnresolvedMarker + fmt.Sprintf("Def%d", i), Kind: graph.EdgeCalls, FilePath: fmt.Sprintf("outside/caller%d.go", i), Line: 3},
		)
	}
	g.AddBatch(nodes, edges)
	paths = append(paths, paths[0], paths[1]) // duplicates must not add queries

	counting := &resolverBatchCountingStore{Store: g}
	frontier := New(counting).collectIncrementalFileFrontier(paths)
	if got := len(frontier.paths); got != files {
		t.Fatalf("deduped paths = %d, want %d", got, files)
	}
	if got := len(frontier.pending); got != 2*files {
		t.Fatalf("pending frontier = %d, want %d", got, 2*files)
	}
	if counting.getFileNodesByPathsCalls != 1 || counting.getOutEdgesByNodeIDsCalls != 1 || counting.getInEdgesByNodeIDsCalls != 1 {
		t.Fatalf("batch reads = file:%d out:%d in:%d, want 1/1/1",
			counting.getFileNodesByPathsCalls, counting.getOutEdgesByNodeIDsCalls, counting.getInEdgesByNodeIDsCalls)
	}
	if counting.getFileNodesCalls != 0 || counting.getOutEdgesCalls != 0 || counting.getInEdgesCalls != 0 {
		t.Fatalf("point reads leaked: file=%d out=%d in=%d",
			counting.getFileNodesCalls, counting.getOutEdgesCalls, counting.getInEdgesCalls)
	}
}

func TestBuildReachabilityForPendingUsesConstantBatchReads(t *testing.T) {
	g := graph.New()
	const files = 10
	var nodes []*graph.Node
	var edges []*graph.Edge
	pending := make([]*graph.Edge, 0, files)
	for i := 0; i < files; i++ {
		path := fmt.Sprintf("pkg%d/caller.go", i)
		callerID := path + "::Caller"
		depPath := fmt.Sprintf("dep%d/dep.go", i)
		nodes = append(nodes,
			&graph.Node{ID: path, Kind: graph.KindFile, Name: path, FilePath: path},
			&graph.Node{ID: callerID, Kind: graph.KindFunction, Name: "Caller", FilePath: path},
			&graph.Node{ID: depPath, Kind: graph.KindFile, Name: depPath, FilePath: depPath},
		)
		edges = append(edges, &graph.Edge{From: path, To: depPath, Kind: graph.EdgeImports, FilePath: path, Line: 1})
		pending = append(pending, &graph.Edge{From: callerID, To: graph.UnresolvedMarker + "Work", Kind: graph.EdgeCalls})
	}
	g.AddBatch(nodes, edges)

	counting := &resolverBatchCountingStore{Store: g}
	r := New(counting)
	if !r.buildReachabilityIndexForPending(pending, nil) {
		t.Fatal("bounded reachability was not built")
	}
	defer r.clearReachabilityIndex()
	if counting.getNodeCalls != 0 || counting.getFileNodesCalls != 0 || counting.getOutEdgesCalls != 0 {
		t.Fatalf("point reads leaked: node=%d file=%d out=%d",
			counting.getNodeCalls, counting.getFileNodesCalls, counting.getOutEdgesCalls)
	}
	if counting.getNodesByIDsCalls != 2 || counting.getFileNodesByPathsCalls != 1 || counting.getOutEdgesByNodeIDsCalls != 1 {
		t.Fatalf("batch reads = nodes:%d file:%d out:%d, want 2/1/1",
			counting.getNodesByIDsCalls, counting.getFileNodesByPathsCalls, counting.getOutEdgesByNodeIDsCalls)
	}
	for i := 0; i < files; i++ {
		path := fmt.Sprintf("pkg%d/caller.go", i)
		depDir := fmt.Sprintf("dep%d", i)
		if _, ok := r.reachableDirsByFile[path][depDir]; !ok {
			t.Fatalf("%s missing imported directory %s", path, depDir)
		}
	}
}

func TestInferImplementsBatchesEdgeWrites(t *testing.T) {
	g := graph.New()
	iface := &graph.Node{ID: "p.go::Worker", Kind: graph.KindInterface, Name: "Worker", FilePath: "p.go", RepoPrefix: "repo", Meta: map[string]any{"methods": []string{"Work"}}}
	typ := &graph.Node{ID: "p.go::Thing", Kind: graph.KindType, Name: "Thing", FilePath: "p.go", RepoPrefix: "repo"}
	method := &graph.Node{ID: "p.go::Thing.Work", Kind: graph.KindMethod, Name: "Work", FilePath: "p.go", RepoPrefix: "repo"}
	g.AddBatch([]*graph.Node{iface, typ, method}, []*graph.Edge{{From: method.ID, To: typ.ID, Kind: graph.EdgeMemberOf}})

	counting := &resolverBatchCountingStore{Store: g}
	if added := New(counting).InferImplements(); added != 1 {
		t.Fatalf("added = %d, want 1", added)
	}
	if counting.addBatchCalls != 1 || counting.addEdgeCalls != 0 {
		t.Fatalf("writes = AddBatch:%d AddEdge:%d, want 1/0", counting.addBatchCalls, counting.addEdgeCalls)
	}
}

func TestInferOverridesBatchesReadsAndWrites(t *testing.T) {
	g := graph.New()
	parent := &graph.Node{ID: "x.go::Parent", Kind: graph.KindType, Name: "Parent"}
	child := &graph.Node{ID: "x.go::Child", Kind: graph.KindType, Name: "Child"}
	parentMethod := &graph.Node{ID: "x.go::Parent.Run", Kind: graph.KindMethod, Name: "Run", FilePath: "x.go", StartLine: 2}
	childMethod := &graph.Node{ID: "x.go::Child.Run", Kind: graph.KindMethod, Name: "Run", FilePath: "x.go", StartLine: 8}
	g.AddBatch([]*graph.Node{parent, child, parentMethod, childMethod}, []*graph.Edge{
		{From: parentMethod.ID, To: parent.ID, Kind: graph.EdgeMemberOf},
		{From: childMethod.ID, To: child.ID, Kind: graph.EdgeMemberOf},
		{From: child.ID, To: parent.ID, Kind: graph.EdgeExtends, Origin: graph.OriginASTResolved},
	})

	counting := &resolverBatchCountingStore{Store: g}
	r := New(counting)
	if added := r.InferOverrides(); added != 1 {
		t.Fatalf("first added = %d, want 1", added)
	}
	if counting.getOutEdgesCalls != 0 || counting.getOutEdgesByNodeIDsCalls != 1 {
		t.Fatalf("out reads = point:%d batch:%d, want 0/1", counting.getOutEdgesCalls, counting.getOutEdgesByNodeIDsCalls)
	}
	if counting.addEdgeCalls != 0 || counting.addBatchCalls != 1 {
		t.Fatalf("writes = AddEdge:%d AddBatch:%d, want 0/1", counting.addEdgeCalls, counting.addBatchCalls)
	}
	if added := r.InferOverrides(); added != 0 {
		t.Fatalf("idempotent added = %d, want 0", added)
	}
	if counting.getOutEdgesCalls != 0 || counting.getOutEdgesByNodeIDsCalls != 2 {
		t.Fatalf("second out reads = point:%d batch:%d, want 0/2", counting.getOutEdgesCalls, counting.getOutEdgesByNodeIDsCalls)
	}
	if counting.addBatchCalls != 1 {
		t.Fatalf("idempotent pass wrote another batch: %d", counting.addBatchCalls)
	}
}

func TestResolveFilesAndIncomingAvoidsPerFilePointQueries(t *testing.T) {
	g := graph.New()
	const files = 16
	paths := make([]string, 0, files)
	outsideIDs := make([]string, 0, files)
	definitionIDs := make([]string, 0, files)
	var nodes []*graph.Node
	var edges []*graph.Edge
	for i := 0; i < files; i++ {
		path := fmt.Sprintf("pkg/f%d.ts", i)
		definitionID := fmt.Sprintf("%s::Def%d", path, i)
		outsidePath := fmt.Sprintf("outside/caller%d.ts", i)
		outsideID := outsidePath + "::Caller"
		paths = append(paths, path)
		outsideIDs = append(outsideIDs, outsideID)
		definitionIDs = append(definitionIDs, definitionID)
		nodes = append(nodes,
			&graph.Node{ID: path, Kind: graph.KindFile, Name: path, FilePath: path, Language: "typescript"},
			&graph.Node{ID: definitionID, Kind: graph.KindFunction, Name: fmt.Sprintf("Def%d", i), FilePath: path, Language: "typescript"},
			&graph.Node{ID: outsideID, Kind: graph.KindFunction, Name: "Caller", FilePath: outsidePath, Language: "typescript"},
		)
		edges = append(edges,
			&graph.Edge{From: definitionID, To: graph.UnresolvedMarker + fmt.Sprintf("Missing%d", i), Kind: graph.EdgeCalls, FilePath: path, Line: 2},
			&graph.Edge{From: outsideID, To: graph.UnresolvedMarker + fmt.Sprintf("Def%d", i), Kind: graph.EdgeCalls, FilePath: outsidePath, Line: 3},
		)
	}
	g.AddBatch(nodes, edges)

	counting := &resolverBatchCountingStore{Store: g}
	New(counting).ResolveFilesAndIncoming(paths)
	if counting.getNodeCalls != 0 || counting.getFileNodesCalls != 0 || counting.getOutEdgesCalls != 0 || counting.getInEdgesCalls != 0 {
		t.Fatalf("point reads leaked: node=%d file=%d out=%d in=%d",
			counting.getNodeCalls, counting.getFileNodesCalls, counting.getOutEdgesCalls, counting.getInEdgesCalls)
	}
	if counting.getFileNodesByPathsCalls > 3 || counting.getOutEdgesByNodeIDsCalls > 4 || counting.getInEdgesByNodeIDsCalls > 3 {
		t.Fatalf("batch reads grew with %d files: file=%d out=%d in=%d", files,
			counting.getFileNodesByPathsCalls, counting.getOutEdgesByNodeIDsCalls, counting.getInEdgesByNodeIDsCalls)
	}
	if counting.reindexEdgesCalls > 4 {
		t.Fatalf("reindex writes grew with %d files: %d", files, counting.reindexEdgesCalls)
	}
	for i, outsideID := range outsideIDs {
		out := g.GetOutEdges(outsideID)
		if len(out) != 1 || out[0].To != definitionIDs[i] {
			t.Fatalf("incoming edge %s = %#v, want target %s", outsideID, out, definitionIDs[i])
		}
	}
}
